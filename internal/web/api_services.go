package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  服务管理接口
//
//  设计要点：
//    - 服务状态一律实时查询，不读数据库缓存（数据库只记录"是什么、怎么管"）
//    - 应用市场的安装先做预检查，把缺依赖/端口冲突一次性说清楚
//    - 纳管（managed=false）与托管（managed=true）在卸载行为上严格区分：
//      纳管服务绝不允许面板卸载
// ============================================================================

// detectDocker 探测 Docker 环境（只返回真正连得上的 socket）。
//
// 统一在这里传 Cfg.UserHome：面板以 root 运行时 os.UserHomeDir() 得到
// /var/root，于是 OrbStack 的 ~/.orbstack/run/docker.sock 永远找不到 ——
// 表现为"没装 Docker"，其实只是找错了家目录。
func (s *Server) detectDocker() (string, string) {
	if sock := s.Cfg.DockerSocket; sock != "" {
		if p, v := services.DetectDocker(sock, s.Cfg.UserHome); p != "" {
			return p, v
		}
	}
	return services.DetectDocker("", s.Cfg.UserHome)
}

// svcManager 构造服务管理器。
func (s *Server) svcManager() *services.Manager {
	sock, _ := s.detectDocker()
	if sock == "" {
		// 没探测到可连的 socket 时，保留用户配置的路径：
		// compose 之类的操作交给 docker 自己去报更有用的错误。
		sock = s.Cfg.DockerSocket
	}
	return services.NewManager(s.serviceRepo, services.Options{
		HelperBin:    s.Cfg.ServicePath("zizpanel-helper"),
		BrewBin:      s.Cfg.BrewBin,
		DockerSocket: sock,
		UserHome:     s.Cfg.UserHome,
		UserName:     s.Cfg.User,
		UID:          s.Cfg.UserUID,
		WorkDir:      s.Cfg.WorkDir,
	})
}

// ---------- 列表与详情 ----------

func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	withHealth := r.URL.Query().Get("health") != "0"
	list, err := mgr.List(r.Context(), withHealth)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取服务列表失败: "+err.Error())
		return
	}

	// Docker 环境信息：前端据此决定是否显示 Docker 相关入口
	sock, ver := s.cachedDocker()

	ok(w, map[string]any{
		"list": list,
		"docker": map[string]any{
			"available": sock != "",
			"socket":    sock,
			"version":   ver,
		},
		"categories": map[string]string{
			"lnmp": "网站环境", "ai": "AI 服务", "tool": "运维工具",
			"custom": "自定义", "other": "其它",
		},
	})
}

// serviceDetail 在服务记录之外，额外带上"目录条目声明的配置文件路径"。
//
// 为什么不在 Service 表里加字段：配置路径是目录（代码里）的静态属性，不是运行时状态；
// 存进库只会多一份会漂的副本。这里按 launchd label / 名字现查目录，返回时摊平
// （嵌入 *Service，JSON 里字段仍是平的，前端不用改读法）。
type serviceDetail struct {
	*services.View
	ConfigPath string `json:"config_path,omitempty"`
}

func (s *Server) handleServiceGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	mgr := s.svcManager()
	v, err := mgr.Get(r.Context(), name)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	// 只透出路径；真正的读写仍走既有的 /api/v1/files/read|write（白名单不变）。
	detail := serviceDetail{View: v}
	if app, ok := services.FindAppByService(v.Service); ok {
		detail.ConfigPath = services.ConfigFilePath(app, s.Cfg.UserHome, s.Cfg.WorkDir)
	}
	ok(w, detail)
}

// ---------- 服务操作 ----------

func (s *Server) handleServiceAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	action := r.PathValue("action")

	mgr := s.svcManager()
	start := time.Now()
	st, err := mgr.Action(r.Context(), name, action)
	cost := time.Since(start).Milliseconds()

	if err != nil {
		s.audit(r, "service_"+action, name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "service_"+action, name,
		fmt.Sprintf("state=%s 耗时=%dms", st.Status, cost), true, "")

	// 若刚动过的是 Qwen 服务，补一次预热。模型驻留是**进程内存**，
	// 重启后两个模型全变冷，下一个网站请求要白等约 25 秒。
	// 后台守温循环也能兜住这件事，但那是分钟级的；用户在这里手动重启，
	// 几秒内就能恢复才对。QwenWarmResident 对已驻留的模型是空操作。
	if svc, err := mgr.Get(r.Context(), name); err == nil && svc.LaunchLabel == services.QwenLabel {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if warmed, _ := mgr.QwenWarmResident(ctx); warmed > 0 {
				s.Log.Info("服务操作后已补载 %d 个 Qwen 模型", warmed)
			}
		}()
	}

	ok(w, map[string]any{"state": st, "cost_ms": cost})
}

// ---------- 日志与日志流 ----------

func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			lines = n
		}
	}
	mgr := s.svcManager()
	content, err := mgr.Logs(r.Context(), name, lines)
	if err != nil {
		// 日志不可用不算致命，返回 200 + 提示，前端展示为说明性文字
		ok(w, map[string]any{"content": "", "error": err.Error(), "lines": 0})
		return
	}
	ok(w, map[string]any{"content": content, "lines": len(strings.Split(content, "\n"))})
}

// handleServiceLogStream 用 SSE 推送服务日志。
//
// 为什么用 SSE：日志是单向服务端推送，SSE 基于普通 HTTP，
// 天然支持 Cookie 鉴权与浏览器自动重连，不需要 WebSocket 的握手与心跳设计。
func (s *Server) handleServiceLogStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	flusher, okf := w.(http.Flusher)
	if !okf {
		fail(w, http.StatusInternalServerError, "当前服务不支持流式响应")
		return
	}
	mgr := s.svcManager()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	ch, err := mgr.LogStream(ctx, name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 经 nginx 反代时必须关缓冲
	w.WriteHeader(http.StatusOK)

	fmt.Fprint(w, "event: open\ndata: {}\n\n")
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case chunk, alive := <-ch:
			if !alive {
				fmt.Fprint(w, "event: close\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			if chunk == "" {
				continue
			}
			// 把内容按行拆成多个 data: 行，符合 SSE 规范（避免单个超长 data 行）
			for _, line := range strings.Split(strings.TrimRight(chunk, "\n"), "\n") {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			fmt.Fprint(w, "\n")
			flusher.Flush()
		}
	}
}

// ---------- 注册 / 编辑 / 移除 ----------

type serviceReq struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	Kind         string `json:"kind"`
	Category     string `json:"category"`
	Icon         string `json:"icon"`
	Description  string `json:"description"`
	Port         int    `json:"port"`
	LaunchLabel  string `json:"launch_label"`
	PlistPath    string `json:"plist_path"`
	WorkDir      string `json:"work_dir"`
	StartCmd     string `json:"start_cmd"`
	Container    string `json:"container"`
	ComposeFile  string `json:"compose_file"`
	Image        string `json:"image"`
	HealthURL    string `json:"health_url"`
	HealthExpect string `json:"health_expect"`
	LogPath      string `json:"log_path"`
	Enabled      *bool  `json:"enabled"`
	Autostart    *bool  `json:"autostart"`
}

// handleServiceCreate 手工注册一个服务。
func (s *Server) handleServiceCreate(w http.ResponseWriter, r *http.Request) {
	var req serviceReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = services.NormalizeName(req.Name)
	if req.Name == "" {
		// 没给名字时用展示名推导
		req.Name = services.NormalizeName(req.DisplayName)
	}
	if req.Name == "" {
		fail(w, http.StatusBadRequest, "请填写服务标识（英文/数字/连字符）")
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.Name
	}
	kind := services.Kind(req.Kind)
	if kind == "" {
		kind = services.KindNative
	}
	if kind == services.KindNative && req.LaunchLabel == "" && strings.TrimSpace(req.StartCmd) == "" {
		fail(w, http.StatusBadRequest, "原生服务需要填写 launchd 标签（LaunchLabel）或启动命令之一")
		return
	}
	if req.Kind == string(services.KindCompose) && req.ComposeFile == "" {
		fail(w, http.StatusBadRequest, "compose 服务需要指定 compose 文件路径")
		return
	}
	if req.Kind == string(services.KindDocker) && req.Container == "" {
		req.Container = req.Name
	}

	// 端口冲突检查：同端口有两个服务时给出明确提示（不阻止，用户可能有特殊安排）
	if req.Port > 0 {
		if info, err := checkPortHelper(r.Context(), req.Port); err == nil && info != "" {
			s.Log.Warn("注册服务 %s 时端口 %d 已被占用：%s", req.Name, req.Port, info)
		}
	}

	svc := &services.Service{
		Name: req.Name, DisplayName: req.DisplayName, Kind: kind,
		Category: orDefault(req.Category, "custom"), Icon: orDefault(req.Icon, "🧩"),
		Description: req.Description, Port: req.Port,
		LaunchLabel: req.LaunchLabel, PlistPath: req.PlistPath, WorkDir: req.WorkDir,
		StartCmd: req.StartCmd, Container: req.Container, ComposeFile: req.ComposeFile,
		Image: req.Image, HealthURL: req.HealthURL, HealthExpect: req.HealthExpect,
		LogPath: req.LogPath, Enabled: true, Managed: true,
	}
	if req.Enabled != nil {
		svc.Enabled = *req.Enabled
	}
	if req.Autostart != nil {
		svc.Autostart = *req.Autostart
	}
	if err := s.serviceRepo.Create(r.Context(), svc); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "service_create", svc.Name, "注册服务 "+svc.DisplayName, true, "")
	ok(w, svc)
}

// handleServiceUpdate 更新服务记录。
func (s *Server) handleServiceUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cur, err := s.serviceRepo.Get(r.Context(), name)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	var req serviceReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 只覆盖传了的字段
	if req.DisplayName != "" {
		cur.DisplayName = req.DisplayName
	}
	if req.Category != "" {
		cur.Category = req.Category
	}
	if req.Icon != "" {
		cur.Icon = req.Icon
	}
	if req.Description != "" {
		cur.Description = req.Description
	}
	if req.Port > 0 {
		cur.Port = req.Port
	}
	if req.LaunchLabel != "" {
		cur.LaunchLabel = req.LaunchLabel
	}
	if req.PlistPath != "" {
		cur.PlistPath = req.PlistPath
	}
	if req.WorkDir != "" {
		cur.WorkDir = req.WorkDir
	}
	if req.StartCmd != "" {
		cur.StartCmd = req.StartCmd
	}
	if req.Container != "" {
		cur.Container = req.Container
	}
	if req.ComposeFile != "" {
		cur.ComposeFile = req.ComposeFile
	}
	if req.Image != "" {
		cur.Image = req.Image
	}
	if req.HealthURL != "" {
		cur.HealthURL = req.HealthURL
	}
	if req.HealthExpect != "" {
		cur.HealthExpect = req.HealthExpect
	}
	if req.LogPath != "" {
		cur.LogPath = req.LogPath
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if req.Autostart != nil {
		cur.Autostart = *req.Autostart
	}
	if err := s.serviceRepo.Update(r.Context(), cur); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "service_update", name, "更新服务配置", true, "")
	ok(w, cur)
}

// handleServiceDelete 从面板移除服务记录（不触碰系统）。
func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	mgr := s.svcManager()
	if err := mgr.Forget(r.Context(), name); err != nil {
		// 记录不存在是 404（客户端用错名字），不是 500（服务端故障）
		if errors.Is(err, services.ErrServiceNotFound) {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "service_forget", name, "从面板移除服务（不影响系统）", true, "")
	ok(w, map[string]any{"msg": "已从面板移除。系统上的服务本身没有被改动。"})
}

// handleServiceUninstall 卸载托管服务。
func (s *Server) handleServiceUninstall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.launchTask(w, r, "uninstall", name, "卸载服务 "+name,
		"service_uninstall", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			if err := s.svcManager().Uninstall(ctx, name); err != nil {
				return nil, err
			}
			return map[string]any{"msg": "服务已卸载"}, nil
		})
}

// ---------- 应用市场 ----------

func (s *Server) handleMarketList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// 「已安装」必须看两个来源，缺一不可：
	//
	//  1. 面板自己的服务记录 —— 面板装过的；
	//  2. Homebrew 里是否真的有这个 formula —— brew 装过的。
	//
	// 只查第 1 个就会出现真机上那种笑话：mini 上 nginx/php/mysql 明明
	// 由 brew 装着、还在系统域跑着，应用市场却给它们显示"安装"按钮。
	// 而用户点下去，重装一遍已经装好的东西。
	//
	// 键也要两套都记：面板记录里是服务名（如 php@8.3），
	// 目录条目里是 ID（如 php83），过去只按 ID 查，
	// 于是连"面板自己装的"都可能匹配不上。
	installed := map[string]bool{}
	var svcRecords []*services.Service
	if list, err := s.serviceRepo.List(ctx); err == nil {
		svcRecords = list
		for _, svc := range list {
			installed[svc.Name] = true // 服务名
			if svc.LaunchLabel != "" {
				installed[svc.LaunchLabel] = true // launchd label
			}
		}
	}
	// 卸载计划要按"这个目录条目对应哪条面板记录"来算，先把记录索引好（只查一次库）
	recByKey := map[string]*services.Service{}
	for _, svc := range svcRecords {
		if svc.Name != "" {
			recByKey[svc.Name] = svc
		}
		if svc.LaunchLabel != "" {
			recByKey[svc.LaunchLabel] = svc
		}
	}
	// 整批查一次（而不是循环里逐个查），见 installedFormulas 的说明
	brewSet := s.installedFormulas(ctx)
	dockerSock, _ := s.cachedDocker()

	type item struct {
		services.App
		// Adopted 表示"这个服务已经在服务管理里登记过了"。
		// 必须与 Installed 分开：一个应用可能**装了但没纳管**
		// （用户自己装的、或换机器后没登记），这时该给的是「纳管」按钮，
		// 而不是"已安装"就完事 —— 用户会发现列表里找不到它。
		Adopted   bool `json:"adopted"`
		Installed bool `json:"installed"`
		// Available 表示当前环境能否安装（缺 Docker 的 compose 应用为 false）
		Available bool   `json:"available"`
		Note      string `json:"note"`
		// Artifacts 表示"磁盘上有安装产物"（面板自研安装器的应用才有意义）。
		// 与 Installed 分开是为了区分「没装」和「装了但服务没注册」——
		// 后者要给的是「重新部署」而不是「安装」。
		Artifacts bool `json:"artifacts"`
		// ServiceInLaunchd 表示这个应用的服务此刻真的能在 launchd 里找到。
		// 「纳管」按钮必须以此为准：plist 不存在时点纳管必然报
		// "找不到 xxx 的 plist，且该服务未在 launchd 中加载"。
		ServiceInLaunchd bool `json:"service_in_launchd"`
		// PortURL 是"直连端口"的入口（http://<本机局域网地址>:<端口>/）。
		// 有界面的应用会给两个入口：子路径（/<slug>/，经面板或 nginx）
		// 与直连端口。子路径探测不通过时（应用必须自己设 base path 才能挂子路径），
		// 界面就把直连作为首选 —— 而不是给一个点开白屏的按钮。
		PortURL string `json:"port_url,omitempty"`
		// ProxyURL 是"经 nginx 的子路径入口"（http://<本机局域网地址>/<slug>/）。
		// 面板自己反代的那些应用用**相对路径**更稳（任何入口都通），
		// 但这个绝对地址有两个用处：显示给用户看，以及给 SelfConf 应用
		// （如 phpMyAdmin，它的 location 只在 nginx 上）当打开入口。
		ProxyURL string `json:"proxy_url,omitempty"`
		// Uninstall 是"这个应用该怎么卸载"的说明（步骤 + 可选删除的产物路径）。
		// 给界面在确认框里如实展示 —— 卸载不可逆，用户必须知道具体会删什么。
		Uninstall services.UninstallPlan `json:"uninstall"`
	}
	lanIP := s.lanIP()
	apps := services.Catalog()
	out := make([]item, 0, len(apps))
	for _, a := range apps {
		// 先看面板记录（ID / 名称 / label 三种写法都认），
		// 再对 brew 类应用查一次 Homebrew。
		// 纳管类条目看的是"这个 launchd 服务是否已在面板记录里"，
		// 而不是它是否被 brew 装过。少了这一条，已经纳管过的服务
		// 在市场里仍然显示「接入纳管」按钮，点下去还报"已经纳管过了"——
		// 状态和入口自相矛盾。
		// brew 类应用的真实 launchd 标签必须**按磁盘上的 plist 推**，不能信目录里
		// 写死的 homebrew.mxcl.*：实测同一台机器上 sh.brew.php@8.3 与
		// homebrew.mxcl.httpd 两套前缀并存。用错标签的后果就是用户看到的
		// "PHP 8.3 显示已安装·未纳管，点纳管报找不到 plist"。
		realLabel := ""
		if a.BrewFormula != "" {
			realLabel = services.BrewLabelFor(s.Cfg.UserHome, a.BrewFormula)
		}
		// 三个候选标签都要认：目录写死的、显式纳管用的、磁盘推出来的
		cand := []string{a.ServiceLabel, a.AdoptLabel, realLabel}

		isInstalled := installed[a.ID] || installed[a.Name]
		for _, l := range cand {
			if !isInstalled && l != "" {
				isInstalled = installed[l]
			}
		}
		// 面板自研安装器部署的系统级服务：/Library/LaunchDaemons 下有 plist 也算
		if !isInstalled {
			for _, l := range cand {
				if l == "" {
					continue
				}
				if _, err := os.Stat(filepath.Join("/Library/LaunchDaemons", l+".plist")); err == nil {
					isInstalled = true
					break
				}
			}
		}
		if !isInstalled && a.BrewFormula != "" {
			isInstalled = brewSet[a.BrewFormula]
		}
		// 面板自研安装器：磁盘上有产物也算"已安装"（否则孤儿态会显示成"未安装"）
		artifacts := false
		if a.PanelInstaller != "" {
			artifacts = services.InstallerArtifactExists(s.Cfg.UserHome, a.PanelInstaller)
			if artifacts {
				isInstalled = true
			}
		}
		// 服务此刻是否真在 launchd 里（决定能不能"纳管"）
		serviceInLaunchd := false
		for _, l := range cand {
			if l != "" && adoptTargetExists(s.Cfg.UserHome, l) {
				serviceInLaunchd = true
				break
			}
		}

		// 纳管的判据：任一候选标签已在面板记录里
		adopted := false
		for _, l := range cand {
			if l != "" && installed[l] {
				adopted = true
				break
			}
		}
		// compose / docker 应用没有 ServiceLabel：它们由面板直接以应用 ID
		// 登记进服务管理。少了这条判断，刚装好的应用会显示成
		// "已安装·未纳管"并给出一个点了必然报错的「纳管」按钮。
		// installed 这张表只由面板的服务记录（名称与 launchd label）构成，
		// 所以命中 ID 就等于"它确实已在服务管理里"。
		if !adopted && (installed[a.ID] || installed[a.Name]) {
			adopted = true
		}
		// 纳管按钮拿的是 item 里的 service_label，所以这里替换成磁盘上真实的那个；
		// 不然"显示已纳管/未纳管"修好了，"点纳管"照样会失败。
		if realLabel != "" {
			a.ServiceLabel = realLabel
		}
		portURL, proxyURL := "", ""
		if a.WebPort() > 0 && lanIP != "" {
			portURL = fmt.Sprintf("http://%s:%d/", lanIP, a.WebPort())
		}
		if a.UI != nil && a.UI.Slug != "" && lanIP != "" {
			proxyURL = fmt.Sprintf("http://%s/%s/", lanIP, a.UI.Slug)
		}
		// 找这条目录对应的面板记录（label / 名称 / ID 三种写法都认）
		var rec *services.Service
		for _, key := range []string{a.ServiceLabel, a.AdoptLabel, a.ID, a.Name} {
			if key != "" && recByKey[key] != nil {
				rec = recByKey[key]
				break
			}
		}
		it := item{App: a, Installed: isInstalled, Adopted: adopted, Available: true,
			Artifacts: artifacts, ServiceInLaunchd: serviceInLaunchd,
			PortURL: portURL, ProxyURL: proxyURL,
			Uninstall: s.svcManager().PlanUninstallFor(ctx, a, rec)}
		if a.Kind == services.KindCompose || a.Kind == services.KindDocker {
			if dockerSock == "" {
				it.Available = false
				it.Note = "需要先安装 Docker 运行时（Colima）"
			}
		}
		if a.AdoptLabel != "" {
			// 纳管类应用：服务不存在时不可安装
			if !adoptTargetExists(s.Cfg.UserHome, a.AdoptLabel) {
				it.Available = false
				it.Note = "未检测到该服务"
			}
		}
		out = append(out, it)
	}

	sock, ver := s.cachedDocker()
	ok(w, map[string]any{
		"list":   out,
		"docker": map[string]any{"available": sock != "", "socket": sock, "version": ver},
	})
}

// handleMarketUninstall 从应用市场卸载一个应用。
//
// 三种语义（见 services.UninstallPlan）：
//
//	· service   —— 托管服务，走通用的 Manager.Uninstall；
//	· installer —— 面板自研安装器装的，走 UninstallApp（卸服务 + 删 plist + 删记录，
//	               并按 remove_data 决定是否删掉虚拟环境/模型/样本）；
//	· forget    —— 纳管的第三方服务：**面板不卸载**，前端改用「取消纳管」
//	               （那个走 DELETE /api/v1/services/{name}）。
//
// 走任务中心：brew uninstall / colima delete / 删几 GB 目录都不是秒级动作，
// 而且用户需要看到"到底删了什么"。
func (s *Server) handleMarketUninstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found {
		fail(w, http.StatusBadRequest, "应用市场中找不到 "+id)
		return
	}
	removeData := r.URL.Query().Get("remove_data") == "1"
	plan := s.svcManager().PlanUninstall(r.Context(), id)
	switch plan.Kind {
	case "service":
		s.launchTask(w, r, "uninstall", app.ID, "卸载 "+app.Name,
			"market_uninstall", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
				if err := s.svcManager().Uninstall(ctx, plan.Service); err != nil {
					return nil, err
				}
				return map[string]any{"msg": "已卸载 " + app.Name}, nil
			})
	case "installer":
		if plan.Blocked != "" {
			fail(w, http.StatusConflict, plan.Blocked)
			return
		}
		s.launchTask(w, r, "uninstall", app.ID, "卸载 "+app.Name,
			"market_uninstall", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
				res := &services.InstallResult{App: app.ID, Steps: []string{}}
				if err := s.svcManager().UninstallApp(ctx, app.ID, removeData, res); err != nil {
					return res, err
				}
				return res, nil
			})
	default:
		msg := plan.Blocked
		if msg == "" {
			msg = "「" + app.Name + "」不能从这里卸载（面板不会卸载你自己安装的软件；" +
				"如只想从列表移除，请用「取消纳管」）"
		}
		fail(w, http.StatusBadRequest, msg)
	}
}

// handleMarketPreflight 安装前检查。
func (s *Server) handleMarketPreflight(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found {
		fail(w, http.StatusNotFound, "应用不存在: "+id)
		return
	}
	mgr := s.svcManager()
	ok(w, mgr.Preflight(r.Context(), app))
}

// handleMarketInstall 安装应用。
//
// 大多数条目走通用的 brew / compose 安装流程；但有一批项目用的是
// 本项目自研的安装器（要建虚拟环境、改 nginx、注册系统级守护进程、
// 下载官方 darwin-arm64 release 产物等），通用流程做不了，所以在这里按 ID 分流。
func (s *Server) handleMarketInstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch id {
	case "iopaint":
		s.handleInstallIOPaint(w, r)
		return
	case "qwen3tts":
		s.handleInstallQwenTTS(w, r)
		return
	case "voicereceiver":
		s.handleInstallVoiceReceiver(w, r)
		return
	case "phpmyadmin":
		s.handleInstallPhpMyAdmin(w, r)
		return
	case "docker-runtime":
		s.handleInstallDockerRuntime(w, r)
		return
	}

	// 官方 release 原生二进制（Lucky / Orbien 服务端 / frps / frpc / Orbien 客户端）：
	// 参数不同但流程同一套，所以共用一个处理器，按目录 ID 分流。
	// 用安装器的注册表判断而不是在这里再抄一份 ID 列表 —— 抄的那份一定会漏。
	if services.IsReleaseBinaryApp(id) {
		s.handleInstallReleaseBinary(w, r)
		return
	}

	app, found := services.FindApp(id)
	if !found {
		fail(w, http.StatusBadRequest, "应用市场中找不到 "+id)
		return
	}
	if app.AdoptLabel != "" {
		fail(w, http.StatusBadRequest, "「"+app.Name+"」是纳管类应用，请用「纳管」而不是安装")
		return
	}
	s.launchTask(w, r, "install", id, "安装 "+app.Name,
		"market_install", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			return s.svcManager().Install(ctx, id)
		})
}

// handleInstallLNMP 一键安装 LNMP 环境。
//
// 这是个"组合动作"：会 brew 安装三个包，并做四件包管理管不到的收尾工作
// （nginx 改 listen 80、建 vhosts/include、初始化 MySQL、注册系统级守护进程）。
// 详见 internal/services/lnmp.go 里的说明。
//
// 异步执行：这一步动辄十几分钟，同步请求期间用户只能看到"请等待"，
// 而且任务挂在 r.Context() 上 —— 一刷新就把 brew 杀了。现在交给任务中心，
// 立刻返回 task_id，进度走 SSE（见 SPEC-任务中心.md）。
func (s *Server) handleInstallLNMP(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "lnmp", "一键安装 LNMP 环境（nginx / PHP 8.3 / MySQL 8.4）",
		"install_lnmp", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "lnmp", Steps: []string{}}
			if err := s.svcManager().InstallLNMP(ctx, res); err != nil {
				return res, err
			}
			return res, nil
		})
}

// handleInstallPhpMyAdmin 单独部署 phpMyAdmin（LNMP 已装好、只想补它时用）。
func (s *Server) handleInstallPhpMyAdmin(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "phpmyadmin", "部署 phpMyAdmin",
		"install_phpmyadmin", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "phpmyadmin", Steps: []string{}}
			if err := s.svcManager().InstallPhpMyAdmin(ctx, res); err != nil {
				return res, err
			}
			return res, nil
		})
}

// handleQwenModels 返回 TTS 模型的下载与驻留状态。
//
// 2026-09-14 起模型清单里只有一个（1.7B-Base-8bit，用于音色克隆）——
// 网站侧只支持自定义音色，预置音色已下线。返回的仍是**列表**，
// 界面按列表渲染，以后清单再变不用改接口。
func (s *Server) handleQwenModels(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	list := mgr.QwenModelsStatus(r.Context())
	ok(w, map[string]any{
		"list":   list,
		"active": mgr.QwenActiveModel(r.Context()),
	})
}

// handleQwenModelSwitch 切换当前驻留的模型（加载目标、卸载其余）。
func (s *Server) handleQwenModelSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		fail(w, http.StatusBadRequest, "缺少模型名")
		return
	}
	mgr := s.svcManager()
	if err := mgr.SetQwenModel(r.Context(), req.Name); err != nil {
		s.audit(r, "qwen_model_switch", req.Name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "qwen_model_switch", req.Name, "已切换模型", true, "")
	ok(w, map[string]any{"active": req.Name, "list": mgr.QwenModelsStatus(r.Context())})
}

// handleQwenModelUnload 释放某个模型占用的内存（下次用到会自动重新加载）。
func (s *Server) handleQwenModelUnload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		fail(w, http.StatusBadRequest, "缺少模型名")
		return
	}
	mgr := s.svcManager()
	if err := mgr.UnloadQwenModel(r.Context(), name); err != nil {
		s.audit(r, "qwen_model_unload", name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "qwen_model_unload", name, "已释放内存", true, "")
	ok(w, map[string]any{"list": mgr.QwenModelsStatus(r.Context())})
}

// handleInstallQwenTTS 按 TtsVoice 插件的契约部署 Qwen3 TTS 服务。
//
// 契约来自 zizdog.cn 的 usr/plugins/TtsVoice/DEPLOY-QWEN.md ——
// 端口、路径、模型名、启动参数都不能自由发挥，否则插件连不上。
// 与手册的唯一差异：注册为系统级 LaunchDaemon 而非用户级 agent，
// 这样不接显示器、无人登录时也能开机自启。
func (s *Server) handleInstallQwenTTS(w http.ResponseWriter, r *http.Request) {
	// auth 默认 true（不加鉴权是危险选项，必须由用户显式选）
	req := struct {
		Auth  *bool  `json:"auth"`
		Token string `json:"token"`
	}{}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	auth := true
	if req.Auth != nil {
		auth = *req.Auth
	}

	opts := services.QwenOptions{Auth: auth, Token: req.Token}
	s.launchTask(w, r, "install", "qwen3tts", "部署 Qwen3 TTS 语音服务",
		"install_qwen3tts", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "qwen3tts", Steps: []string{}}
			if err := s.svcManager().InstallQwenTTS(ctx, res, opts); err != nil {
				return res, err
			}
			return res, nil
		})
}

// handleInstallVoiceReceiver 部署 TtsVoice 音色样本接收端 + 鉴权反代。
//
// 契约来自网站侧交接文档 HANDOFF-TO-MINI.md，receiver.py 逐字节照搬。
// 部署完会把"要告诉网站侧的两项"（本机地址、共享密钥）直接列在结果里。
func (s *Server) handleInstallVoiceReceiver(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Token  string `json:"token"`
		NoAuth bool   `json:"no_auth"`
		// Host 是监听地址：留空沿用 0.0.0.0（网站可能在别的机器上）；
		// 网站与接收端同机时传 127.0.0.1（交接文档推荐，不暴露到局域网）
		Host string `json:"host"`
	}{}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	opts := services.ReceiverOptions{Token: req.Token, NoAuth: req.NoAuth, Host: req.Host}
	s.launchTask(w, r, "install", "voicereceiver", "部署音色样本接收端",
		"install_voicereceiver", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "voicereceiver", Steps: []string{}}
			if err := s.svcManager().InstallVoiceReceiver(ctx, res, opts); err != nil {
				return res, err
			}
			return res, nil
		})
}

// handleInstallIOPaint 部署 IOPaint（图片去水印 / 物体擦除 / 扩图）。
func (s *Server) handleInstallIOPaint(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "iopaint", "安装 IOPaint（图片去水印）",
		"install_iopaint", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "iopaint", Steps: []string{}}
			if err := s.svcManager().InstallIOPaint(ctx, res); err != nil {
				return res, err
			}
			return res, nil
		})
}

// handleInstallReleaseBinary 部署"官方 release 原生二进制"类应用。
//
// 目前服务 Lucky / Orbien 服务端 / frps / frpc / Orbien 客户端（见
// services.IsReleaseBinaryApp 与 releaseBinaryApps）。与其它安装器一样走任务中心：
// 下载 + 解压 + 注册系统级服务不是秒级动作，而且用户需要看到下载来源
// （官方还是加速镜像）、sha256 校验结果与最终的验证结论。
func (s *Server) handleInstallReleaseBinary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found {
		fail(w, http.StatusBadRequest, "应用市场中找不到 "+id)
		return
	}
	s.launchTask(w, r, "install", id, "安装 "+app.Name,
		"market_install", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: id, Name: app.Name, Steps: []string{}}
			if err := s.svcManager().InstallReleaseBinary(ctx, id, res); err != nil {
				return res, err
			}
			return res, nil
		})
}

// handleInstallDockerRuntime 安装 Docker 运行时（Colima）。
func (s *Server) handleInstallDockerRuntime(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "docker-runtime", "安装 Docker 运行时（Colima）",
		"install_docker_runtime", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "docker-runtime", Name: "Docker 运行时（Colima）", Steps: []string{}}
			if err := s.svcManager().InstallColimaRuntime(ctx, res); err != nil {
				return res, err
			}
			return res, nil
		})
}

// handleAdoptScan 扫描本机可纳管但尚未纳管的 launchd 服务。
func (s *Server) handleAdoptScan(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	list, err := mgr.ScanAdoptable(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"list": list})
}

type adoptReq struct {
	Label       string `json:"label"`
	DisplayName string `json:"display_name"`
	Icon        string `json:"icon"`
	Port        int    `json:"port"`
	Category    string `json:"category"`
}

// handleAdopt 纳管一个已有的 launchd 服务。
func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	var req adoptReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 先查重：同名 launchd 服务只能纳管一次。
	//
	// 踩过的坑：同一个服务先通过接口纳管（记录名 = label），
	// 之后用户又从应用市场点了一次"纳管"（记录名 = 应用 ID），
	// 于是服务管理里出现**两条指向同一 label 的记录**，
	// 状态、启停都重复。name 字段有 UNIQUE 约束，但两条记录的
	// name 不同、label 相同，约束拦不住 —— 所以必须在这里判。
	if list, err := s.serviceRepo.List(r.Context()); err == nil {
		for _, e := range list {
			if e.LaunchLabel == req.Label {
				fail(w, http.StatusConflict,
					"该服务已经纳管过了（记录名："+e.DisplayName+"），无需重复纳管")
				return
			}
		}
	}

	mgr := s.svcManager()
	svc, err := mgr.AdoptCandidate(r.Context(), req.Label, req.DisplayName, req.Icon)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Port > 0 || req.Category != "" {
		if req.Port > 0 {
			svc.Port = req.Port
		}
		if req.Category != "" {
			svc.Category = req.Category
		}
		_ = s.serviceRepo.Update(r.Context(), svc)
	}
	s.audit(r, "service_adopt", req.Label, "纳管服务 "+svc.DisplayName, true, "")
	ok(w, svc)
}

// handlePortCandidates 返回若干可用端口（新建服务时参考）。
func (s *Server) handlePortCandidates(w http.ResponseWriter, r *http.Request) {
	from := 9000
	if v := r.URL.Query().Get("from"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			from = n
		}
	}
	mgr := s.svcManager()
	ok(w, map[string]any{"ports": mgr.PortCandidates(r.Context(), from, 8)})
}

// ---------- 小工具 ----------

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// checkPortHelper 通过助手查端口占用（需要 root 才能看到所有进程）。
func checkPortHelper(ctx context.Context, port int) (string, error) {
	res, err := helperCall(ctx, "port-check", strconv.Itoa(port))
	if err != nil {
		return "", err
	}
	data, _ := res["data"].(map[string]any)
	inUse, _ := data["in_use"].(bool)
	if !inUse {
		return "", nil
	}
	holders, _ := data["holders"].([]any)
	var names []string
	for _, h := range holders {
		names = append(names, fmt.Sprint(h))
	}
	return strings.Join(names, ", "), nil
}

// adoptTargetExists 判断纳管目标服务是否存在于本机。
func adoptTargetExists(home, label string) bool {
	if label == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join("/Library/LaunchDaemons", label+".plist")); err == nil {
		return true
	}
	if home != "" {
		if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", label+".plist")); err == nil {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
//  应用市场用的短缓存
//
//  市场列表要给每个条目判断状态，而每个判断都很贵（brew 启动 0.4s+、
//  Docker 套接字探测每次 800ms 超时）。循环里逐个调用会累加到数秒。
//  这里统一成"整批查一次 + 30 秒 TTL"。
//  TTL 取 30 秒的理由：用户刚在面板里装完东西，回到市场应该立刻看到
//  状态变化；再长就会显得"装了没反应"，再短就失去缓存意义。
// ---------------------------------------------------------------------------

// marketCacheTTL 是市场缓存的保鲜期。
//
// 已装 formula 集合只在用户安装/卸载时变化，本身就是低频数据；30 秒的短 TTL
// 会让「隔一会儿再打开市场」几乎必然触发一次 1.5 秒的 `brew list`。这里放宽到
// 5 分钟，并在过期时走后台刷新，请求路径永远不等待 brew。
const marketCacheTTL = 5 * time.Minute

// marketRefreshTimeout 限制单次后台刷新的时长，避免 brew 卡死拖住刷新 goroutine。
const marketRefreshTimeout = 60 * time.Second

// installedFormulas 返回本机已安装的 brew formula 集合。
//
// 三种情况：
//   - 缓存新鲜      → 直接返回；
//   - 缓存过期      → 先返回旧值，同时后台刷新（用户感知不到 brew 的耗时）；
//   - 尚无缓存      → 同步取一次（仅在启动预热未完成时会走到，见 WarmMarket）。
func (s *Server) installedFormulas(ctx context.Context) map[string]bool {
	s.mktMu.Lock()
	brew, at := s.mktBrew, s.mktBrewAt
	s.mktMu.Unlock()

	if brew != nil && time.Since(at) < marketCacheTTL {
		return brew
	}
	if brew != nil {
		s.refreshMarketAsync()
		return brew
	}

	// 冷启动兜底：这里必须同步，否则首屏会把所有应用都显示成"未安装"。
	set := s.fetchInstalledFormulas(ctx)
	s.mktMu.Lock()
	if s.mktBrew == nil {
		s.mktBrew, s.mktBrewAt = set, time.Now()
	} else {
		set = s.mktBrew
	}
	s.mktMu.Unlock()
	return set
}

func (s *Server) fetchInstalledFormulas(ctx context.Context) map[string]bool {
	set := s.svcManager().InstalledFormulas(ctx)
	if set == nil {
		set = map[string]bool{}
	}
	return set
}

// dockerMissTTL 是"没探测到 Docker"这一结果的最长保鲜期。
//
// 必须远小于 marketCacheTTL：探测失败往往是瞬时的（Colima 正在启动、
// 机器刚重启、VM 还没就绪）。若把这次失败按正常 TTL 缓存 5 分钟，面板就会在
// Docker 明明已经可用的情况下持续显示"未安装 Docker"—— 实测正是如此：
// 安装运行时的过程中 VM 正在重启，探测失败被缓存，之后 docker ps 一切正常，
// 面板却仍然报"未装"。负结果只保持几秒，让它自己很快纠正回来。
const dockerMissTTL = 5 * time.Second

// cachedDocker 探测 Docker 并缓存，避免每个条目都探一遍。
func (s *Server) cachedDocker() (string, string) {
	s.mktMu.Lock()
	sock, ver, at := s.mktDocker, s.mktDockerV, s.mktDockerAt
	s.mktMu.Unlock()

	ttl := marketCacheTTL
	if sock == "" {
		ttl = dockerMissTTL // 负结果不长期缓存
	}
	if !at.IsZero() && time.Since(at) < ttl {
		return sock, ver
	}
	if !at.IsZero() {
		s.refreshMarketAsync()
		return sock, ver
	}
	sock, ver = s.detectDocker()
	s.mktMu.Lock()
	if s.mktDockerAt.IsZero() {
		s.mktDocker, s.mktDockerV, s.mktDockerAt = sock, ver, time.Now()
	} else {
		sock, ver = s.mktDocker, s.mktDockerV
	}
	s.mktMu.Unlock()
	return sock, ver
}

// WarmMarket 在启动时预热市场缓存，让用户第一次打开市场就是热的。
func (s *Server) WarmMarket(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, marketRefreshTimeout)
	defer cancel()
	s.refreshMarketCaches(ctx)
}

// refreshMarketAsync 触发一次后台刷新；已有刷新在跑时不重复启动。
func (s *Server) refreshMarketAsync() {
	if !s.mktRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.mktRefreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), marketRefreshTimeout)
		defer cancel()
		s.refreshMarketCaches(ctx)
	}()
}

// refreshMarketCaches 刷新市场依赖的两项慢查询：已装 formula 与 Docker 探测。
//
// 两项都在锁外完成，算完再一次性换入缓存 —— 慢查询绝不持有 mktMu，
// 否则 brew 的 1.5 秒会阻塞所有市场请求。
func (s *Server) refreshMarketCaches(ctx context.Context) {
	set := s.fetchInstalledFormulas(ctx)
	sock, ver := s.detectDocker()

	s.mktMu.Lock()
	s.mktBrew, s.mktBrewAt = set, time.Now()
	// 探测成功才覆盖：一次失败不应把已知可用的 socket 抹掉，
	// 否则容器列表会在 VM 抖动时突然全部"消失"。
	if sock != "" || s.mktDockerAt.IsZero() {
		s.mktDocker, s.mktDockerV, s.mktDockerAt = sock, ver, time.Now()
	} else {
		// 记为一次"失败"，让它按 dockerMissTTL 很快重试。
		s.mktDockerAt = time.Now()
	}
	s.mktMu.Unlock()
}

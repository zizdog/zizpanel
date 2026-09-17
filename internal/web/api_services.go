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
	if s.svcManagerOverride != nil { // 单测注入点，见 server.go 的字段说明
		return s.svcManagerOverride(s)
	}
	// 顺手对一次路径：面板可能在"Homebrew 还不存在"的那一刻就写下了配置
	// （全新机器就是这样），装好 Homebrew 之后前缀若不修正，
	// 站点/数据库/日志会一直在找 /usr/local 下的东西。
	// 这里很便宜（两次 stat），而且只有真的变了才写文件。
	_ = s.Cfg.ReconcilePaths()

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
		// 应用包镜像基址：面板里所有安装过程都从这里取资源（见 services/mirror.go）。
		// svcManager() 每次都按当前 Cfg 新建，所以设置页保存后立刻生效。
		MirrorBase:         s.Cfg.MirrorBase,
		MirrorProbeSeconds: s.Cfg.MirrorProbeSeconds,
		// 仅走 NAS（离线）模式：打开后各安装器禁止回落外网
		// （见 internal/services/mirror.go 的 MirrorOfflineOnly）。
		OfflineOnly: s.Cfg.OfflineOnly,
		// MySQL root 凭据闭环：读面板当前持有的凭据、把新口令写回 config.json。
		//
		// 为什么从这里注入而不是让 services 自己读配置：凭据只有一份来源
		// （config.json，0600），services 不该再造一份。安装流程结束后
		// 「数据库」页面看到的必须就是这里写进去的那一份。
		MySQLCredential: func() services.MySQLCredential {
			return services.MySQLCredential{
				Host:     s.Cfg.MySQLHost,
				Port:     s.Cfg.MySQLPort,
				Socket:   s.Cfg.MySQLSocket,
				User:     s.Cfg.MySQLUser,
				Password: s.Cfg.MySQLPasswordValue(),
			}
		},
		SetMySQLRootPassword: func(pw string) error {
			// 先改内存再落盘；落盘失败时**不回滚**内存值 ——
			// 回滚会让"已经改好的 MySQL"与"面板手里的口令"立刻不一致，
			// 用户连补救的入口都没有了。错误原样上抛，调用方会把它写进
			// 任务步骤与告警（并保留一次性凭据区块）。
			s.Cfg.SetMySQLPassword(pw)
			return s.Cfg.Save()
		},
		MySQLInputTimeout: time.Duration(s.Cfg.MySQLInputTimeoutSeconds) * time.Second,
	})
}

// ---------- 列表与详情 ----------

func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	// 先把**已存在**的服务记录的健康地址与目录对齐（幂等、只在不一致时写库）。
	// 不这么做的话，目录里后补的 HealthPath 永远传不到已装的记录上 ——
	// 表现是"面板纳管了却不监测"（2026-09-16 用户反馈 TTS 像是没被管理）。
	_, _ = mgr.ReconcileHealthURLs(r.Context())
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
		"categories": services.CategoryLabels(),
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

// handleBrewOverview 直接返回 **brew 的真实状态**（只读）。
//
// 设计立场（用户 2026-09-16 的批评）：面板过去自己维护"装了什么/在不在跑"，
// 与 brew 的真实状态漂移，于是出现"日志说装了、市场看不到、服务管理也没有"。
// 这里把 brew 当**唯一真相来源**直接读出来给前端渲染：
//
//	· brew services list --json   → 服务到底起没起、pid、退出码
//	· brew outdated --json=v2     → 哪些能升级（面板此前完全没有这个能力）
//	· brew list --versions        → 本机装了哪些 formula 及版本
//
// 只读、不改任何状态；失败如实把错误放进对应字段，不谎报。
func (s *Server) handleBrewOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mgr := s.svcManager()
	out := map[string]any{}

	captureJSON := func(key string, args ...string) {
		txt, err := mgr.BrewCapture(ctx, 40*time.Second, args...)
		if err != nil {
			out[key] = map[string]any{"error": err.Error()}
			return
		}
		var v any
		if json.Unmarshal([]byte(txt), &v) != nil {
			out[key] = map[string]any{"raw": strings.TrimSpace(txt)}
			return
		}
		out[key] = v
	}
	captureJSON("services", "services", "list", "--json")
	captureJSON("outdated", "outdated", "--json=v2")

	// installed 用文本形式（brew list --versions 最快，且不需要解析大 JSON）
	if txt, err := mgr.BrewCapture(ctx, 40*time.Second, "list", "--versions"); err != nil {
		out["installed_error"] = err.Error()
	} else {
		var list []map[string]string
		for _, line := range strings.Split(strings.TrimSpace(txt), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				list = append(list, map[string]string{"name": f[0], "version": strings.Join(f[1:], " ")})
			}
		}
		out["installed"] = list
	}
	ok(w, out)
}

// handleServiceCredentials 返回面板管理的应用配置里的登录凭据。
//
// 为什么需要：面板给 frpc 这类应用**随机生成** admin UI 的用户名/口令，
// 只在安装任务日志里出现过一次 —— 用户事后想登录 7400（或 Orbien 的 8020）
// 只能自己去翻配置文件（2026-09-16 用户反馈"安装的时候也没让设置啊"）。
// 这里把凭据从**面板自己生成的那个配置文件**里解析出来，服务详情里直接可看。
//
// 只读、且只对"面板管理的应用"开放（按服务记录找到目录条目再定位配置文件）。
func (s *Server) handleServiceCredentials(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	v, err := s.svcManager().Get(r.Context(), name)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	app, okk := services.FindAppByService(v.Service)
	if !okk {
		fail(w, http.StatusNotFound, "这个服务不是面板管理的应用，没有可解析的凭据")
		return
	}
	path := services.ConfigFilePath(app, s.Cfg.UserHome, s.Cfg.WorkDir)
	if path == "" {
		fail(w, http.StatusNotFound, "这个应用没有可读取的配置文件")
		return
	}
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		fail(w, http.StatusNotFound, "读取配置失败："+rerr.Error())
		return
	}
	// 地址必须用**服务所在机器**的，不能用请求里的客户端 IP。
	// 后者是浏览器的地址（真机踩到：显示成用户自己电脑的 192.168.1.179）。
	// 同时给出"本机"与"局域网"两个地址：在本机上用 127.0.0.1 更稳妥
	// （en0 未必是当前在用的网卡）。
	ui, uiLocal := "", ""
	if app.Port > 0 {
		uiLocal = fmt.Sprintf("http://127.0.0.1:%d", app.Port)
		if ip := s.svcManager().PrimaryIP(); ip != "" && !strings.HasPrefix(ip, "<") {
			ui = fmt.Sprintf("http://%s:%d", ip, app.Port)
		}
	}
	ok(w, map[string]any{
		"config_path": path,
		"ui":          ui,
		"ui_local":    uiLocal,
		"credentials": services.ExtractCredentials(string(b)),
	})
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

	ok(w, map[string]any{"state": st, "cost_ms": cost, "warning": st.Warning})
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

// handleServiceDelete 只删面板记录，绝不触碰运行时。
//
// 语义硬要求（2026-09-17 真机缺陷）：用户把本机 Docker/Colima 全删掉后，
// managed=true 的 compose 记录走「卸载」会先 `docker compose down` 而失败，
// 于是那条记录**永远删不掉**。这条通路就是那种情况下的唯一出口：
// 它只删 services 表里的一行，绝不调用 docker / brew / launchctl 去停任何东西。
// 纳管的本机现有服务同样只删记录，真实服务照旧运行。
//
// 为什么**刻意不经 svcManager()**：构造管理器会探测 Docker socket、并
// ReconcilePaths() 顺手写配置。这条通路的语义是"记录与运行时彻底解耦"，
// 所以直接落到 serviceRepo —— 最短、也最容易证明不碰运行时的路径。
func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.serviceRepo.Delete(r.Context(), name); err != nil {
		// 记录不存在是 404（客户端用错名字），不是 500（服务端故障）
		if errors.Is(err, services.ErrServiceNotFound) {
			fail(w, http.StatusNotFound,
				"服务 "+name+" 不在面板记录里（服务不存在或已被移除）")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "service_forget", name, "从面板移除服务（不影响系统）", true, "")
	ok(w, map[string]any{
		"removed": true,
		// runtime_touched 必须**如实**：这条通路只删了 services 表里的一行，
		// 没有执行任何 docker / brew / launchctl 命令，也没有停任何服务。
		"runtime_touched": false,
		"msg":             "已从面板移除。系统上的服务本身没有被改动。",
	})
}

// handleServiceUninstall 卸载托管服务。
func (s *Server) handleServiceUninstall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.launchTask(w, r, "uninstall", name, "卸载服务 "+name,
		"service_uninstall", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{Steps: []string{}}
			if err := s.svcManager().Uninstall(ctx, name); err != nil {
				return res, uninstallFailure(err, name)
			}
			res.Steps = append(res.Steps, "服务已卸载")
			// 卸载后护栏：卸载流程（尤其 brew uninstall / autoremove）可能把
			// 作为共享依赖的 ffmpeg 一起带走，而那是"200 + 空 body"事故的起点。
			// 这是通用服务卸载，不知道用户有意删了哪个 formula，所以传空串：
			// 缺什么就补什么。
			s.svcManager().GuardBaseDependenciesAfterUninstall(ctx, res, "")
			return res, nil
		})
}

// uninstallFailure 把卸载失败整理成用户能据以行动的说明。
//
// 唯一特殊处理：**运行时不可用**（docker/compose 命令缺失）时，明确告诉用户
// "没有停止任何容器、系统一点没动"，并指出还有「取消纳管」（只删记录）这条路 ——
// 否则用户会以为卸载失败＝记录也删不掉（2026-09-17 真机缺陷：
// Docker 全删掉后，managed=true 的 compose 记录只能走卸载，于是永远删不掉）。
//
// 成功路径完全不受影响：这里只在**已经有错误**时改写文案，不改任何行为。
func uninstallFailure(err error, name string) error {
	if !services.IsRuntimeUnavailable(err) {
		return err
	}
	return fmt.Errorf("%w（说明：本机没有可用的容器运行时，本次没有停止任何容器、"+
		"系统上的服务一点没动；如果你只是想清掉面板里的这条记录，请用「取消纳管」——"+
		"DELETE /api/v1/services/%s 只删记录、不需要 Docker）", err, name)
}

// ---------- 应用市场 ----------

// marketHiddenApps 是从应用市场列表里**刻意隐藏**的目录条目（ID → 为什么隐藏）。
//
// 为什么需要它：用户要求「一键 LNMP」的入口放在「网站管理」，市场里不要再出现，
// 避免同一个动作有两处入口、用户不知道该点哪。后端能力
// （POST /api/v1/market/install-lnmp）与目录条目本身都保留，只是不渲染成市场卡片。
//
// 当前 Catalog() 里并没有 ID=lnmp 的条目（LNMP 是组合动作、不是 App，见
// internal/services/catalog.go 里「网站环境」那一段的说明），所以这条排除目前
// 是**防御性**的：将来若有人把 lnmp 当成一个条目加进目录，它会立刻在这里被挡下，
// 不必再改一遍市场渲染逻辑；同时也把"为什么不显示"写在了代码里，而不是靠记忆。
var marketHiddenApps = map[string]string{
	"lnmp": "一键 LNMP 的入口在「网站管理」（sites.js），不在应用市场，避免两处重复",
}

// marketVisibleApps 从目录里挑出要展示在市场里的条目：去掉 marketHiddenApps
// 里刻意隐藏的那些（理由见该变量的说明）。
//
// 单独抽成一个函数是为了**可测试**：当前目录里没有 ID=lnmp 的条目，
// 把这段逻辑写在 handleMarketList 里就只能等"将来真加了条目"才验证得到。
func marketVisibleApps(apps []services.App) []services.App {
	out := make([]services.App, 0, len(apps))
	for _, a := range apps {
		if _, hidden := marketHiddenApps[a.ID]; hidden {
			continue
		}
		out = append(out, a)
	}
	return out
}

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

		// ---- Docker 推荐项目（App.DockerReference）给前端的稳定契约 ----
		//
		// DockerRecommended 是 App.DockerReference 的**别名**：前端 apps.js 的
		// isDockerRec() 已经认这个字段名，而 App 上按用户要求叫 docker_reference
		// （json:"docker_reference"）。两个都发，前端任认一个都不会漏。
		DockerRecommended bool `json:"docker_recommended,omitempty"`
		// ComposeURL 是镜像站上**预配置 compose 文件**的下载/查看地址
		// （<mirror_base>/compose/<id>/docker-compose.yml）；没配镜像基址时为空。
		// compose 的**内容**走 App.ComposeYAML（json:"compose_yaml"，用于"复制"）。
		ComposeURL string `json:"compose_url,omitempty"`
		// ComposeEnvURL 是变量样例 .env.example 的地址（复制成 .env 再改）。
		ComposeEnvURL string `json:"compose_env_url,omitempty"`
		// ComposeReadmeURL 是全部推荐项目的总索引（镜像站 /compose/README.md）。
		ComposeReadmeURL string `json:"compose_readme_url,omitempty"`
	}
	lanIP := s.lanIP()
	apps := marketVisibleApps(services.Catalog())
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
				if _, err := os.Stat(filepath.Join(launchDaemonsDir, l+".plist")); err == nil {
					isInstalled = true
					break
				}
			}
		}
		if !isInstalled && a.BrewFormula != "" {
			isInstalled = brewSet[a.BrewFormula]
		}
		// 面板自研安装器：磁盘上有产物**不等于已安装**。
		//
		// 2026-09-16 用户反馈：卸载时没勾「同时删除数据/产物」（或手动删了服务、
		// 目录还在）之后，卡片被"有产物就算已安装"这条判据永久钉在"已安装"上：
		// 没有「安装」入口、残留数据也清不掉 —— 用户无法重装。
		// 现在产物只作为**残留数据**如实报给界面（artifacts=true 且 installed=false），
		// 重装入口交给「安装」：安装器本身是幂等的，会复用残留产物、不会重复下载。
		// 「已安装」的证据只有三种：面板服务记录、launchd 里的 plist/作业、brew formula。
		artifacts := false
		if a.PanelInstaller != "" {
			artifacts = services.InstallerArtifactExists(s.Cfg.UserHome, a.PanelInstaller)
		}
		// 旧版**原生安装**的残留：应用从注册表里拿掉（如 Lucky 改成 Docker 版）之后，
		// 家目录下那份安装还在。不报出来的话卡片显示"未安装"却没有任何清理入口 ——
		// 2026-09-16 用户实测："lucky 根本没被卸载掉"。
		if !artifacts {
			artifacts = services.LegacyNativeArtifactExists(s.Cfg.UserHome, a.ID, string(a.Kind))
		}
		// compose / docker 类应用的产物是**项目目录**（<WorkDir>/compose/<id>）。
		// 卸载（保留数据）之后目录还在、服务记录不在 —— 与原生类一样必须
		// 如实报 artifacts=true，否则卡片显示"未安装"却没有任何清理入口，
		// 那份数据永远删不掉（2026-09-16 用户反馈的同一类问题）。
		if !artifacts && (a.Kind == services.KindCompose || a.Kind == services.KindDocker) {
			artifacts = services.ComposeArtifactExists(s.Cfg.WorkDir, a.ID)
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
		// 直链端口用 EntryPort()：它可以与健康检查端口不同（MinIO 就是这种 ——
		// 健康检查必须打 9010 的 S3 API，而用户要打开的是 9001 控制台）。
		if p := a.EntryPort(); p > 0 && lanIP != "" {
			portURL = fmt.Sprintf("http://%s:%d/", lanIP, p)
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
		if a.DockerReference {
			// 推荐项目的稳定契约：标记 + compose 内容（App.ComposeYAML → compose_yaml）
			// + 镜像站地址。front 端据此把卡片放进 docker Tab 并隐藏安装按钮。
			base := s.Cfg.MirrorBase
			it.DockerRecommended = true
			it.ComposeURL = services.ComposeYAMLURL(base, a.ID)
			it.ComposeEnvURL = services.ComposeEnvExampleURL(base, a.ID)
			it.ComposeReadmeURL = services.ComposeReferenceIndexURL(base)
		}
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
		"list": out,
		// sections 是市场板块的**顺序与中文名**（services.MarketSections）。
		// 前端按它渲染板块标题，自己不再写一份分类中文名 ——
		// 用户要求「其它」改名「基础环境」，改名只改 catalog.go 一处。
		"sections": services.MarketSections(),
		"docker":   map[string]any{"available": sock != "", "socket": sock, "version": ver},
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
		// 条目可能已经**从目录下架**（2026-09-17 移除 n8n），但用户机器上
		// 还留着它的 compose 项目目录/镜像。只要卸载计划给出 installer
		// （残留清理）就照常放行；没有可清理对象时仍按原来的 400 处理。
		// 这就是"界面上没了、磁盘上还在、用户无处可点"的反面。
		if plan := s.svcManager().PlanUninstall(r.Context(), id); plan.Kind == "installer" {
			app = services.App{ID: id, Name: id}
			found = true
		}
	}
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
				res := &services.InstallResult{Steps: []string{}}
				if err := s.svcManager().Uninstall(ctx, plan.Service); err != nil {
					return res, err
				}
				res.Steps = append(res.Steps, "已卸载 "+app.Name)
				// 卸载后护栏：brew 的清理动作可能把共享依赖（ffmpeg）一起带走，
				// 而那种"静默消失"正是 TTS 全败事故的起点。见 basedep.go。
				// 传 app.BrewFormula：如果是用户主动卸载了提供基础依赖的那个包
				// （例如 ffmpeg 本身），护栏只报告后果、不自动装回去。
				s.svcManager().GuardBaseDependenciesAfterUninstall(ctx, res, app.BrewFormula)
				return res, nil
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
				// 卸载成功后必须**同时**把服务记录删掉：只删文件与 plist 的话，
				// 用户在「服务管理」里还会看到它，观感就是"根本没卸掉"
				// （2026-09-16 用户反馈）。按 label 删，label 从目录条目取。
				if app.ServiceLabel != "" {
					if n, ferr := s.svcManager().ForgetByLabel(ctx, app.ServiceLabel); ferr == nil && n > 0 {
						res.Steps = append(res.Steps,
							fmt.Sprintf("已从「服务管理」移除 %d 条记录（%s）", n, app.ServiceLabel))
					}
				}
				// 卸载后护栏：phpMyAdmin 这类会跑 `brew uninstall`，而 brew 的
				// autoremove 有可能把作为共享依赖的 ffmpeg 一起带走 ——
				// 那种"静默消失"（TTS 返回 200 + 空 body）必须在这里被抓住并补回。
				// 传 app.BrewFormula：用户主动卸载 ffmpeg 条目时不自动装回去。
				s.svcManager().GuardBaseDependenciesAfterUninstall(ctx, res, app.BrewFormula)
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

// dockerReferenceInstallMessage 是安装接口拒绝"推荐 Docker 项目"时返回的人话错误。
//
// 为什么要有单独一条：这些条目过去能一键安装，现在面板不再代装。用户点「安装」
// 必须**立刻**知道三件事：为什么不行、compose 文件在哪、接下来该怎么做。
// 只回一句 "not supported" 等于把问题丢回给用户。
func dockerReferenceInstallMessage(app services.App, mirrorBase string) string {
	msg := "「" + app.Name + "」是面板推荐的 Docker 项目：面板不再代你安装。" +
		"请到「应用 → docker」页复制预配置的 compose 文件，改完自己跑。"
	if url := services.ComposeYAMLURL(mirrorBase, app.ID); url != "" {
		msg += "镜像站上也有一份可直接下载：" + url + "。"
	}
	return msg + "Docker 页有 Compose 面板。"
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
	case "ffmpeg":
		s.handleInstallBaseDependency(w, r)
		return
	case "docker-runtime":
		s.handleInstallDockerRuntime(w, r)
		return
	case "miniflux":
		// Miniflux 只能配 PostgreSQL，而且建库/写配置/迁移/建管理员这一串
		// 通用 brew 流程做不了，所以走自研安装器（见 services/miniflux.go）。
		s.handleInstallMiniflux(w, r)
		return
	case "syncthing":
		// Syncthing 需要把 GUI 从上游默认的 127.0.0.1:8384 改成局域网可访问
		// **并同时设上随机口令**（否则等于无口令暴露远程控制台），
		// 通用 brew 流程做不到，所以走自研安装器（见 services/syncthing.go）。
		s.handleInstallSyncthing(w, r)
		return
	}

	app, found := services.FindApp(id)
	if !found {
		fail(w, http.StatusBadRequest, "应用市场中找不到 "+id)
		return
	}

	// 推荐 Docker 项目：**明确拒绝安装**（4xx + 人话），不再代用户跑 compose。
	//
	// 为什么必须在这里、且在 launchTask 之前挡住：过去这些条目能一键安装，
	// 现在语义变了（用户 2026-09-17："不提供安装，这个功能应该给会用 docker 的人用"）。
	// 静默失败或谎报成功是这个仓库最忌讳的事，所以直接返回 409 并告诉用户
	// compose 文件在哪、Docker 页有 Compose 面板。
	// 注意：这条要放在 release-binary 分流**之前** —— 推荐项目只可能是 compose。
	if app.DockerReference {
		fail(w, http.StatusConflict, dockerReferenceInstallMessage(app, s.Cfg.MirrorBase))
		return
	}

	// 官方 release 原生二进制（frpc / Orbien 客户端）：
	// 参数不同但流程同一套，所以共用一个处理器，按目录 ID 分流。
	//
	// **判定必须以目录条目为准**：注册表里还留着某个 ID，而目录已经把它改成
	// compose（Lucky 就是这样，2026-09-16 用户要求改 Docker 版）时，
	// 只看注册表会绕过 compose 安装器去 GitHub 下 darwin 二进制 ——
	// 真机复现：任务日志里出现 lucky_2.27.2_darwin_arm64.tar.gz 的镜像检查，
	// 而目录条目明明是 compose，用户看到的就是"装不上"。
	if services.IsReleaseBinaryApp(id) && app.Kind != services.KindCompose && app.Kind != services.KindDocker {
		s.handleInstallReleaseBinary(w, r)
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

// handleInstallLNMP 一键 LNMP（nginx + PHP + MySQL 的组合动作）。
//
// 这是个"组合动作"：会 brew 安装三个包，并做四件包管理管不到的收尾工作
// （nginx 改 listen 80、建 vhosts/include、初始化 MySQL、注册系统级守护进程）。
// 详见 internal/services/lnmp.go 里的说明。
//
// 异步执行：这一步动辄十几分钟，同步请求期间用户只能看到"请等待"，
// 而且任务挂在 r.Context() 上 —— 一刷新就把 brew 杀了。现在交给任务中心，
// 立刻返回 task_id，进度走 SSE（见 SPEC-任务中心.md）。
func (s *Server) handleInstallLNMP(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "lnmp", "一键 LNMP（nginx / PHP 8.2 / MySQL 8.4）",
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

// handleInstallBaseDependency 单独安装基础依赖（目前只有 ffmpeg 这一条）。
//
// 为什么不走通用 brew 流程：ffmpeg 是**纯命令行工具**，没有 brew service。
// 通用流程会去 `brew services start ffmpeg`，得到一个"已安装，但启动失败"的
// 假警告，还会在服务管理里留下一条永远没有状态的假记录。
// 这里只做一件事：EnsureBaseDependencies（幂等：已装则跳过并说明）。
func (s *Server) handleInstallBaseDependency(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "ffmpeg", "安装 FFmpeg（音视频工具）",
		"install_basedep", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "ffmpeg", Steps: []string{}}
			if err := s.svcManager().EnsureBaseDependencies(ctx, res); err != nil {
				return res, err
			}
			res.Steps = append(res.Steps,
				"FFmpeg 已就绪：TTS 编码 mp3、音色样本校验与后续音视频功能都会用到它")
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
// 目前服务 frpc 与 Orbien 客户端（见
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
// launchDaemonsDir 是本机系统级 LaunchDaemons 目录。
//
// 抽成变量是为了**测试可沙箱化**：单测必须能把它指向临时目录。
// 2026-09-16 踩到：`TestMarketResidualDataOffersReinstall` 断言"残留态
// installed=false"，而它经 adoptTargetExists 去 stat **真实**的
// /Library/LaunchDaemons —— 用户机器上真的装了 frpc/orbien-client 的 plist 后，
// 这条测试就必然失败。单测不该依赖真机装了什么。
var launchDaemonsDir = "/Library/LaunchDaemons"

func adoptTargetExists(home, label string) bool {
	if label == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(launchDaemonsDir, label+".plist")); err == nil {
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

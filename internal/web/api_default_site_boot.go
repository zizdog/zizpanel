package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  安装后自动建立"默认静态站点"（用户 2026-09-21 明确要求）
//
//  用户原话："面板安装完成后要建立一个默认静态站点！……不依赖 lnmp，很难实现吗？"
//
//  现状（为什么以前没有）：默认站点的**生成器**早就有（buildDefaultVhost，且在
//  没有 PHP 时会退化成一纯静态 vhost），但它的唯一入口是
//  `POST /api/v1/system/default-site/apply` —— 一个**界面上根本没有按钮**的接口。
//  于是新装的机器上：① 没有默认站点；② 用户在面板里找不到任何入口去建它。
//
//  这一步的定位：**装完就用**。面板启动时自动做一次（幂等、结果如实记录），
//  没做成也不谎报 —— 把原因写进状态，界面据此给出下一步（最常见的原因是
//  这台机器还没有 nginx，那就先只装 nginx，不拉整个 LNMP）。
//
//  三条硬约束（这个项目最贵的教训）：
//    · **必须是 root**：写 nginx 配置要过提权助手。调试实例（make run-local）
//      以普通用户跑，且它的 BrewPrefix/WWWRoot 是**真机路径**（AGENTS 坑 162）——
//      所以在非 root 下一律跳过，绝不在调试实例上写真实 nginx 配置。
//    · **一次就够**：成功后落盘标记，之后每次启动只读标记，不重复写配置/reload。
//    · **判据贴着运行体**：先看 nginx **二进制在不在**，不是看有没有目录/配置文件；
//      不在就如实报"没有 nginx"，而不是让它以一段 helper 报错收场。
// ============================================================================

// defaultSiteStateFile 是自动建默认站点的结果落盘文件（放在数据目录里）。
const defaultSiteStateFile = "default-site.json"

// defaultSiteState 是"安装后自动建默认站点"这一步的真实结果。
//
// 字段全部如实填写：没有 nginx 时 Applied=false + Error 说明原因，
// **绝不**因为"文件写过了"就说成功 —— 成功判据是 applyDefaultVhost 里那套
// "写盘 → nginx -t → reload → 请求级复核（首页含标记）"。
type defaultSiteState struct {
	Attempted    bool   `json:"attempted"`
	Applied      bool   `json:"applied"`
	NginxPresent bool   `json:"nginx_present"`
	IndexPath    string `json:"index_path,omitempty"`
	VhostPath    string `json:"vhost_path,omitempty"`
	URL          string `json:"url,omitempty"`
	Error        string `json:"error,omitempty"`
	At           string `json:"at,omitempty"`
}

// defaultSiteEuid 是"当前有效用户 id"的可注入点：单测以普通用户跑，
// 但必须能验证"以 root 启动时会真的建站点"这条路径（否则只能测到跳过分支）。
var defaultSiteEuid = os.Geteuid

// nginxPresent 判"这台机器上到底有没有 nginx" —— 只看**二进制**。
//
// 为什么不用 `<brew>/etc/nginx/nginx.conf` 之类的文件判：那是"有配置文件"，
// 不是"有这个能力"（AGENTS 第三节、DEVELOPMENT 坑 161）。二进制在才算在。
func (s *Server) nginxPresent() (bool, string) {
	bin := strings.TrimSpace(s.Cfg.NginxBin)
	if bin == "" && s.Cfg.BrewPrefix != "" {
		bin = filepath.Join(s.Cfg.BrewPrefix, "bin", "nginx")
	}
	if bin == "" {
		return false, "面板不知道 nginx 装在哪（配置里没有 nginx_bin，也没有 Homebrew 前缀）"
	}
	st, err := os.Stat(bin)
	if err != nil {
		return false, "本机没有 nginx（" + bin + " 不存在）"
	}
	if st.IsDir() {
		return false, bin + " 是个目录，不是 nginx 可执行文件"
	}
	return true, ""
}

// readDefaultSiteState 读落盘状态（读不到就是"还没试过"）。
func (s *Server) readDefaultSiteState() defaultSiteState {
	var st defaultSiteState
	if s.Cfg.DataDir == "" {
		return st
	}
	b, err := os.ReadFile(filepath.Join(s.Cfg.DataDir, defaultSiteStateFile))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	return st
}

// writeDefaultSiteState 落盘状态；写不下去只记日志（不能因为记不住状态就让启动失败）。
func (s *Server) writeDefaultSiteState(st defaultSiteState) {
	if s.Cfg.DataDir == "" {
		return
	}
	if err := os.MkdirAll(s.Cfg.DataDir, 0o755); err != nil {
		s.Log.Warn("记录默认站点状态失败（目录 %s 不可用）: %v", s.Cfg.DataDir, err)
		return
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(s.Cfg.DataDir, defaultSiteStateFile), b, 0o600); err != nil {
		s.Log.Warn("记录默认站点状态失败: %v", err)
	}
}

// defaultSiteStatus 是 GET /api/v1/system/default-site 的响应体。
//
// 它把"上次自动尝试的结果"与"此刻的真实状态"分开说：
// 用户升级面板/手动改过站点之后，落盘状态可能已经过期，所以现场再探一遍。
type defaultSiteStatus struct {
	Applied      bool   `json:"applied"` // 面板认定"默认站点已就绪"
	NginxPresent bool   `json:"nginx_present"`
	NginxBin     string `json:"nginx_bin,omitempty"`
	IndexPath    string `json:"index_path,omitempty"`
	VhostPath    string `json:"vhost_path,omitempty"`
	URL          string `json:"url,omitempty"`
	// NeedsAction 非空表示"需要用户做点什么"（人话，直接显示）。
	NeedsAction string `json:"needs_action,omitempty"`
	// WaitingNginx 为 true 时界面给「只安装 Nginx」按钮（不拉 PHP/MySQL）。
	WaitingNginx bool `json:"waiting_nginx"`
	// Error 是上次尝试的真实错误（空 = 没有错误）。
	Error string `json:"error,omitempty"`
	// VhostMarked 表示磁盘上的 000-default.conf 是不是**面板生成的**那一份。
	VhostMarked bool `json:"vhost_marked"`
	// ForeignVhost 表示 80 端口上已有一份**不是面板生成的**默认站点配置。
	// 这时面板**不会自动覆盖**（那可能是用户自己写的 server 块），只在界面上提示，
	// 由用户点按钮明确同意后才覆盖。
	ForeignVhost bool `json:"foreign_vhost"`
	// At 是上次尝试时间（RFC3339 本地时间）。
	At string `json:"at,omitempty"`
}

// defaultSiteDiskReady 判"磁盘上到底有没有一个面板的默认站点" —— 只看现场证据。
//
// 判据两条，缺一不可：
//  1. `000-default.conf` 存在，且带**面板的统一标记**（services.DefaultVhostMarker）——
//     用户自己写的 server 块不算"面板的默认站点"；
//  2. 占位页 `www/localhost/index.html` 在。
//
// 为什么不用落盘状态文件当判据：状态文件只说明"面板上次试过"，与现场可以分离
// （用户手工删了目录、换了 vhost）。证据必须来自磁盘本身 —— 这也是本项目的
// 老规矩（AGENTS 第三节："有产物" ≠ "可用"，判据要贴着现场）。
func (s *Server) defaultSiteDiskReady() (ready bool, index string, vhostMarked bool) {
	vhostMarked, _ = s.defaultVhostState()
	index = filepath.Join(s.Cfg.WWWRoot, "localhost", "index.html")
	if _, err := os.Stat(index); err != nil {
		index = ""
	}
	return vhostMarked && index != "", index, vhostMarked
}

// defaultVhostState 返回 (是不是面板生成的, 文件是否存在)。
//
// "存在但不是面板生成的"是一个**必须区别对待**的状态：那可能是用户自己写的
// 80 端口 server 块，自动覆盖它等于擅自改用户的东西。
func (s *Server) defaultVhostState() (marked, exists bool) {
	b, err := os.ReadFile(filepath.Join(s.Cfg.VhostDir, "000-default.conf"))
	if err != nil {
		return false, false
	}
	return strings.Contains(string(b), services.DefaultVhostMarker), true
}

// defaultSiteStatusNow 现场探一遍真实状态（不写任何东西）。
func (s *Server) defaultSiteStatusNow() defaultSiteStatus {
	st := s.readDefaultSiteState()
	present, why := s.nginxPresent()
	ready, index, marked := s.defaultSiteDiskReady()
	_, vhostExists := s.defaultVhostState()
	out := defaultSiteStatus{
		Applied:      ready,
		NginxPresent: present,
		NginxBin:     s.Cfg.NginxBin,
		VhostPath:    filepath.Join(s.Cfg.VhostDir, "000-default.conf"),
		URL:          "http://" + s.lanIP() + "/",
		IndexPath:    index,
		VhostMarked:  marked,
		Error:        st.Error,
		At:           st.At,
	}
	if !present {
		// 没有 nginx：默认站点无从谈起（谁去监听 80？）。如实说，并给出"只装 nginx"这条路。
		out.Applied = false
		out.WaitingNginx = true
		out.NeedsAction = "这台机器还没有安装 Nginx（默认站点需要它才能监听 80 端口）。" +
			"可以只装 Nginx、不装 PHP 与 MySQL —— 面板会建一个纯静态的默认站点。"
		if why != "" {
			out.Error = why
		}
		return out
	}
	if !out.Applied {
		out.NeedsAction = "默认站点还没建好。点「创建默认站点」即可（只写面板自己的那份 vhost + 一张占位页，" +
			"不需要 PHP 与 MySQL）。"
	}
	if vhostExists && !marked {
		// 80 端口上有一份**别人写的**默认站点：面板不自动覆盖，也不假装它是自己的。
		out.ForeignVhost = true
		out.NeedsAction = out.VhostPath + " 已经存在，但它不是面板生成的（可能是你自己写的 server 块）。" +
			"面板**不会自动覆盖**它；如果你要的是面板的默认站点，点「覆盖为面板默认站点」" +
			"（原文件会先备份成 .zizpanel.bak）。"
	}
	return out
}

// vhostNotLoadedReason 判断"面板的 vhost 到底有没有被 nginx 加载"，返回人话原因（空 = 没发现问题）。
//
// 为什么需要（2026-09-18 mini 真机）：默认站点复核失败时，面板只报"6 秒内新配置仍未生效"，
// 用户完全不知道该查哪。真机现场是：nginx.conf **没有 include 面板的 vhosts 目录**，
// 于是 :80 上回答请求的是 Homebrew 自带的默认站点（首页 200、但没有面板的占位页标记，
// PHP 探测文件也 404）。这条判断把"vhost 没被加载"这个根因**直接说出来**。
func (s *Server) vhostNotLoadedReason(ctx context.Context) string {
	vhostDir := s.Cfg.VhostDir
	if vhostDir == "" {
		return ""
	}
	// ① 目录 / 文件在不在
	if _, err := os.Stat(filepath.Join(vhostDir, "000-default.conf")); err != nil {
		return vhostDir + "/000-default.conf 不存在（面板没能写入默认站点配置）"
	}
	// ② 用 `nginx -T`（真实加载的全量配置）确认这个目录有没有被 include 进去。
	res, err := s.callHelper(ctx, "nginx-conf-include-status")
	if err == nil {
		if inc, _ := res["data"].(map[string]any); inc != nil {
			if detail, _ := inc["detail"].(string); detail != "" {
				// conf.d 已就绪不代表 vhosts 已就绪；再看下面第三条。
				_ = detail
			}
		}
	}
	out, derr := s.nginxDumpConfForCheck(ctx)
	if derr != nil || out == "" {
		return ""
	}
	if !strings.Contains(out, vhostDir) {
		return "nginx 加载的配置里**没有出现** " + vhostDir + " —— 说明 nginx.conf 的 http 块" +
			"缺少 `include " + filepath.Join(vhostDir, "*.conf") + ";`" +
			"（面板写的站点/默认站点配置全部没生效，:80 上回答请求的是别的 server 块）"
	}
	return ""
}

// nginxDumpConfForCheck 取 `nginx -T` 的全量配置（经提权助手）。
//
// 必须用 -T（dump）而不是 -t（test）：`nginx -t` 的输出只有一行"syntax is ok"，
// **看不到任何 include 的文件**；拿它去判断"面板的 vhost 有没有被加载"必然误判。
func (s *Server) nginxDumpConfForCheck(ctx context.Context) (string, error) {
	res, err := s.callHelper(ctx, "nginx-dump-conf")
	if err != nil {
		return "", err
	}
	if txt, _ := res["msg"].(string); txt != "" {
		return txt, nil
	}
	return "", nil
}

// handleDefaultSiteStatus 返回默认站点状态（只读，不写盘）。
func (s *Server) handleDefaultSiteStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, s.defaultSiteStatusNow())
}

// ensureDefaultSiteOnStart 在面板启动时自动把默认站点建起来（用户要求"装完就有"）。
//
// 幂等 + 诚实：
//   - 已经成功过（落盘标记 + 现场仍在）→ 什么都不做；
//   - 没有 nginx → 如实记一笔"等待 nginx"，**不报错刷屏**（这是新机器的正常状态）；
//   - 非 root → 跳过（调试实例必须不碰真机 nginx 配置，AGENTS 坑 162）。
func (s *Server) ensureDefaultSiteOnStart(ctx context.Context) {
	st := s.readDefaultSiteState()
	present, why := s.nginxPresent()
	if !present {
		if !st.Attempted || st.NginxPresent || st.Error != why {
			s.writeDefaultSiteState(defaultSiteState{
				Attempted: true, Applied: false, NginxPresent: false,
				Error: why, At: time.Now().Format(time.RFC3339),
			})
			s.Log.Info("默认站点：%s（装上 Nginx 后面板会自动建一个纯静态默认站点，"+
				"也可以在「网站管理 → 默认站点」里点「只安装 Nginx」）", why)
		}
		return
	}
	// 磁盘上已经有一个**面板的**默认站点（带标记的 vhost + 占位页）：
	// 什么都不做。老机器（升级上来的）本来就有默认站点，这里绝不重写 + reload ——
	// 用户没要求改的东西，面板不该在每次启动时动它。
	if ready, index, _ := s.defaultSiteDiskReady(); ready {
		if !st.Applied {
			// 补记一次状态：以前是手动建的，现在面板知道了（只记状态，不写配置）。
			s.writeDefaultSiteState(defaultSiteState{
				Attempted: true, Applied: true, NginxPresent: true,
				IndexPath: index, VhostPath: filepath.Join(s.Cfg.VhostDir, "000-default.conf"),
				URL: "http://" + s.lanIP() + "/", At: time.Now().Format(time.RFC3339),
			})
		}
		return
	}
	if marked, exists := s.defaultVhostState(); exists && !marked {
		// 80 端口上有一份不是面板生成的默认站点：**坚决不自动覆盖**用户的配置。
		// 如实记录，让用户自己在界面上决定（那时会先备份再覆盖）。
		st.Attempted, st.NginxPresent, st.Applied = true, true, false
		st.Error = filepath.Join(s.Cfg.VhostDir, "000-default.conf") +
			" 已存在但不是面板生成的（可能是你自己写的），已跳过自动创建；" +
			"在「网站管理 → 默认站点」里可以明确选择覆盖它"
		st.At = time.Now().Format(time.RFC3339)
		s.writeDefaultSiteState(st)
		s.Log.Info("默认站点：%s", st.Error)
		return
	}
	if defaultSiteEuid() != 0 {
		// 调试实例：BrewPrefix/WWWRoot 都是真机路径，绝不能在这里写真实 nginx 配置。
		st.Attempted, st.NginxPresent, st.Applied = true, true, false
		st.Error = "面板不是以 root 运行，已跳过自动创建默认站点（调试实例不写真实 nginx 配置）"
		st.At = time.Now().Format(time.RFC3339)
		s.writeDefaultSiteState(st)
		s.Log.Info("默认站点：%s", st.Error)
		return
	}
	s.Log.Info("默认站点不存在（vhost 或占位页缺失）—— 自动创建一份纯静态默认站点")
	if err := s.createDefaultSite(ctx); err != nil {
		st.Attempted, st.NginxPresent, st.Applied = true, true, false
		st.Error = err.Error()
		st.At = time.Now().Format(time.RFC3339)
		s.writeDefaultSiteState(st)
		s.Log.Warn("默认站点自动创建失败：%v（可在「网站管理 → 默认站点」里重试）", err)
		return
	}
	index := filepath.Join(s.Cfg.WWWRoot, "localhost", "index.html")
	s.writeDefaultSiteState(defaultSiteState{
		Attempted: true, Applied: true, NginxPresent: true,
		IndexPath: index, VhostPath: filepath.Join(s.Cfg.VhostDir, "000-default.conf"),
		URL: "http://" + s.lanIP() + "/", At: time.Now().Format(time.RFC3339),
	})
	s.Log.Info("默认站点已自动创建并复核通过：%s（root %s；纯静态，不需要 PHP/MySQL）",
		"http://"+s.lanIP()+"/", filepath.Dir(index))
}

// createDefaultSite 真正建默认站点：占位页 + 完整 vhost + 复核。
//
// 与界面上的「创建默认站点」按钮**走同一条路**（同两个函数），不新造第二套实现 ——
// 两份实现必然走样，这个项目已经为此踩过坑（见 defaultsite.go 顶部注释）。
func (s *Server) createDefaultSite(ctx context.Context) error {
	present, why := s.nginxPresent()
	if !present {
		return fmt.Errorf("%s；先安装 Nginx（面板里可只装它，不装 PHP/MySQL）", why)
	}
	// 80 端口上已经有一份**不是面板生成的**配置（用户自己写的 server 块）时，
	// 先把原文件备份一份再覆盖 —— 界面上写着"会先备份"，就必须真的备份
	//（"能谎报的功能比没做更糟"，而且这里丢的是用户自己写的配置）。
	if marked, exists := s.defaultVhostState(); exists && !marked {
		vhost := filepath.Join(s.Cfg.VhostDir, "000-default.conf")
		if b, rerr := os.ReadFile(vhost); rerr == nil {
			bak := vhost + ".zizpanel.bak"
			if werr := os.WriteFile(bak, b, 0o644); werr != nil {
				// 备份都写不下去就**不要**覆盖：宁可失败也不能让用户的配置无声消失。
				return fmt.Errorf("覆盖前备份 %s 失败: %w（已放弃覆盖，你的原文件未改动）", bak, werr)
			}
			s.Log.Info("默认站点：%s 不是面板生成的，已备份为 %s 后覆盖", vhost, bak)
		}
	}
	index, _, err := sites.EnsureLocalhostPlaceholder(s.Cfg.WWWRoot)
	if err != nil {
		return err
	}
	dir := filepath.Dir(index)
	if s.Cfg.User != "" && defaultSiteEuid() == 0 {
		if err := chownTreeTo(dir, s.Cfg.User); err != nil {
			s.Log.Warn("调整 %s 归属失败（站点可能仍是 root 属主）：%v", dir, err)
		}
	}
	// applyDefaultVhost 自带 nginx -t + 失败回滚 + reload + 请求级复核（首页含标记）。
	return s.applyDefaultVhost(ctx)
}

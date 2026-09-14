package web

import (
	"encoding/json"
	"fmt"
	"github.com/zizdog/zizpanel/internal/config"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/auth"
	"github.com/zizdog/zizpanel/internal/sysinfo"
)

func goVersion() string { return runtime.Version() }

// handleSystemInfo 返回一次系统快照。
//
// 只读后台采集的缓存，过期时顺带触发一次异步刷新；接口本身不做采集，
// 因此无论并发多少、top 多慢，响应都是毫秒级。
func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	s.Info.RefreshAsync(r.Context())
	ok(w, s.Info.Last())
}

// handleSystemStream 以 SSE 推送实时指标（默认 2 秒一次）。
//
// 为什么用 SSE 而不是 WebSocket：指标是单向服务端推送，SSE 基于普通 HTTP，
// 天然支持 Cookie 鉴权与断线自动重连（浏览器 EventSource 内置），
// 不需要额外的握手协议和心跳设计。
func (s *Server) handleSystemStream(w http.ResponseWriter, r *http.Request) {
	flusher, okf := w.(http.Flusher)
	if !okf {
		fail(w, http.StatusInternalServerError, "当前服务不支持流式响应")
		return
	}
	interval := 2 * time.Second
	if v := r.URL.Query().Get("interval"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 30 {
			interval = time.Duration(n) * time.Second
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 经 nginx 反代时必须关缓冲
	w.WriteHeader(http.StatusOK)

	// 先立即推一次，避免前端等待一个周期
	s.sendSample(w, flusher)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ping := time.NewTicker(25 * time.Second) // 保活，防中间设备断开空闲连接
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			s.sendSample(w, flusher)
		case <-ping.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) sendSample(w http.ResponseWriter, f http.Flusher) {
	smp := s.Info.Last()
	b, _ := json.Marshal(smp)
	_, _ = fmt.Fprintf(w, "event: sample\ndata: %s\n\n", b)
	f.Flush()
}

// handleProcesses 返回占资源最高的进程列表。
//
// CPU 百分比用采样差值计算（见 sysinfo.ProcSampler）。为了让连续刷新都能拿到
// 准确值，这里结果缓存 2 秒：SSE 每 2 秒推一次指标的同时前端会拉进程列表，
// 缓存能让两次采样之间至少间隔 2 秒，差值才稳定。
func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	sortBy := r.URL.Query().Get("sort")
	if sortBy != "mem" {
		sortBy = "cpu"
	}
	limit := 15
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	s.procMu.Lock()
	fresh := time.Since(s.procAt) < 2*time.Second && len(s.procList) > 0
	var list []sysinfo.Proc
	if fresh {
		list = s.procList
	}
	s.procMu.Unlock()

	if !fresh {
		list = s.Procs.Sample(r.Context(), 0) // 取全量，排序后再截断
		if sortBy == "mem" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].RSS > list[j].RSS })
		}
		s.procMu.Lock()
		s.procList, s.procAt = list, time.Now()
		s.procMu.Unlock()
	}

	out := list
	if sortBy == "mem" && len(out) > 0 {
		sorted := make([]sysinfo.Proc, len(out))
		copy(sorted, out)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RSS > sorted[j].RSS })
		out = sorted
	}
	if len(out) > limit {
		out = out[:limit]
	}
	ok(w, map[string]any{"list": out, "sort": sortBy})
}

// audit 写一条审计记录。失败不影响主流程。
// audit 写一条审计日志（请求处理期间用）。
//
// 拆成 captureAudit + auditAs 是因为**后台任务**也要写审计：任务结束时
// HTTP 请求早就返回了，那时候 r.Context() 已取消，直接拿 r 写会静默失败。
func (s *Server) audit(r *http.Request, action, target, detail string, success bool, msg string) {
	s.auditAs(s.captureAudit(r), action, target, detail, success, msg)
}

// ---------- 账号相关 ----------

type changePwdReq struct {
	Old string `json:"old"`
	New string `json:"new"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePwdReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	u := userFrom(r.Context())
	if err := s.Auth.ChangePassword(r.Context(), u.ID, req.Old, req.New); err != nil {
		s.audit(r, "password_change", u.Username, err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "password_change", u.Username, "密码已修改，所有会话已失效", true, "")
	// 改密码会吊销所有会话（含当前），前端需要重新登录
	s.clearSessionCookies(w)
	ok(w, map[string]any{"msg": "密码已修改，请重新登录"})
}

type renameUserReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleRenameUser 修改当前账号的用户名。
//
// 要当前密码：用户名是登录凭据的一半，而且审计里"谁做的"记的就是它 ——
// 不能让一个被劫持的会话把账号改成事后追查对不上人的名字。
// 会话不吊销：sessions 是按 user_id 关联的，改名不影响登录态。
func (s *Server) handleRenameUser(w http.ResponseWriter, r *http.Request) {
	var req renameUserReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	u := userFrom(r.Context())
	if u == nil {
		fail(w, http.StatusUnauthorized, "未登录")
		return
	}
	newName, err := s.Auth.RenameUser(r.Context(), u.ID, req.Username, req.Password)
	if err != nil {
		// 失败也要留痕：改名尝试本身是安全相关事件
		s.audit(r, "account_rename", strings.TrimSpace(req.Username),
			"失败（原名 "+u.Username+"）: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "account_rename", newName, "用户名 "+u.Username+" → "+newName, true, "")
	ok(w, map[string]any{
		"username": newName,
		"msg":      "用户名已改为 " + newName,
	})
}

// handleTOTPSetup 生成新的 TOTP 密钥与二维码链接（此时尚未启用）。
func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		fail(w, http.StatusInternalServerError, "生成密钥失败")
		return
	}
	uri := auth.TOTPProvisioningURI("ZizPanel", u.Username, secret)
	ok(w, map[string]any{
		"secret": secret,
		"uri":    uri,
		"hint":   "用验证器 App 扫描二维码，或手工输入密钥，然后输入 6 位验证码完成绑定。",
	})
}

type totpEnableReq struct {
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	var req totpEnableReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	u := userFrom(r.Context())
	// 密钥由前端回传：服务端不暂存未启用的密钥，避免半途而废的状态残留
	if err := s.Auth.EnableTOTP(r.Context(), u.ID, req.Secret, req.Code); err != nil {
		s.audit(r, "totp_enable", u.Username, "验证码错误", false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "totp_enable", u.Username, "两步验证已开启", true, "")
	ok(w, map[string]any{"msg": "两步验证已开启"})
}

type totpDisableReq struct {
	Password string `json:"password"`
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	var req totpDisableReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	u := userFrom(r.Context())
	if err := s.Auth.DisableTOTP(r.Context(), u.ID, req.Password); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "totp_disable", u.Username, "两步验证已关闭", true, "")
	ok(w, map[string]any{"msg": "两步验证已关闭"})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	list, err := s.Auth.ListSessions(r.Context(), u.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取会话失败")
		return
	}
	ok(w, map[string]any{"list": list, "current_ip": s.clientIP(r)})
}

// ---------- 设置 ----------

// settingsView 是暴露给前端的可调设置。
func (s *Server) settingsView() map[string]any {
	wl := s.Cfg.IPWhitelist
	if wl == nil {
		wl = []string{}
	}
	return map[string]any{
		"access_mode":      s.Cfg.AccessMode,
		"ip_whitelist":     wl,
		"trust_proxy":      s.Cfg.TrustProxy,
		"session_hours":    s.Cfg.SessionHours,
		"login_max_fail":   s.Cfg.LoginMaxFail,
		"login_lock_mins":  s.Cfg.LoginLockMins,
		"require_2fa":      s.Cfg.Require2FA,
		"listen":           s.Cfg.Listen,
		"tls_enable":       s.Cfg.TLSEnable,
		"www_root":         s.Cfg.WWWRoot,
		"panel_public_url": s.Cfg.PanelPublicURL,
		// 安全后缀（安全入口）：界面要显示"当前入口地址"，也允许改
		"panel_suffix": s.Cfg.PanelSuffix,
		"panel_entry":  s.PanelEntryPath(),
		// 升级源也要回传：设置页要能显示当前值并允许清空。
		// 少了它，用户在页面上既看不到、也清不掉在线升级写进去的地址。
		"upgrade_source": s.Cfg.UpgradeSource,
		// 数据库连接（面板管理 MySQL 用）。密码回传是为了让设置页能显示
		// "已填写"状态并允许修改；面板本身是登录后才能访问的后台。
		"mysql_host":     s.Cfg.MySQLHost,
		"mysql_port":     s.Cfg.MySQLPort,
		"mysql_socket":   s.Cfg.MySQLSocket,
		"mysql_user":     s.Cfg.MySQLUser,
		"mysql_password": s.Cfg.MySQLPassword,
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	ok(w, s.settingsView())
}

type settingsReq struct {
	AccessMode     *string   `json:"access_mode"`
	IPWhitelist    *[]string `json:"ip_whitelist"`
	TrustProxy     *bool     `json:"trust_proxy"`
	SessionHours   *int      `json:"session_hours"`
	LoginMaxFail   *int      `json:"login_max_fail"`
	LoginLockMins  *int      `json:"login_lock_mins"`
	Require2FA     *bool     `json:"require_2fa"`
	PanelPublicURL *string   `json:"panel_public_url"`
	// PanelSuffix 是安全后缀。传空串 = 关闭安全入口（会让面板回到根路径，
	// 界面会要求用户二次确认）；非空会被规范化（只留小写字母/数字/-/_）。
	PanelSuffix *string `json:"panel_suffix"`
	// UpgradeSource 是在线升级的默认源地址。传空串表示"清空"。
	//
	// 为什么必须能在这里改：在线升级的"检查更新"会把用过的源**写进配置**，
	// 但之前没有任何界面/接口能把它清掉 —— 用户只能在设置页看着一个清不掉的
	// 地址。UI 测试里"未配置升级源时给出明确提示"那条正是这样失败的：
	// 清空输入框后接口回退到内存里的旧值，于是永远走不到"未配置"分支。
	UpgradeSource *string `json:"upgrade_source"`

	// MySQL 连接（面板管理数据库用）
	MySQLHost     *string `json:"mysql_host"`
	MySQLPort     *int    `json:"mysql_port"`
	MySQLSocket   *string `json:"mysql_socket"`
	MySQLUser     *string `json:"mysql_user"`
	MySQLPassword *string `json:"mysql_password"`

	// Web 终端（默认关闭，需显式开启）
	TerminalEnabled     *bool   `json:"terminal_enabled"`
	TerminalShell       *string `json:"terminal_shell"`
	TerminalIdleMins    *int    `json:"terminal_idle_mins"`
	TerminalMaxSessions *int    `json:"terminal_max_sessions"`
}

// handleSaveSettings 保存可热更新的设置。
// 注意：listen / tls 改动必须重启进程，不在这里处理（避免保存后立刻失联）。
func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.Cfg
	if req.AccessMode != nil {
		switch *req.AccessMode {
		case "any", "local", "whitelist":
			cfg.AccessMode = *req.AccessMode
		default:
			fail(w, http.StatusBadRequest, "访问模式只能是 any / local / whitelist")
			return
		}
	}
	if req.IPWhitelist != nil {
		cleaned := make([]string, 0, len(*req.IPWhitelist))
		for _, c := range *req.IPWhitelist {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if !validCIDROrIP(c) {
				fail(w, http.StatusBadRequest, "非法网段或 IP："+c)
				return
			}
			cleaned = append(cleaned, c)
		}
		cfg.IPWhitelist = cleaned
	}
	if req.TrustProxy != nil {
		cfg.TrustProxy = *req.TrustProxy
	}
	if req.SessionHours != nil && *req.SessionHours > 0 && *req.SessionHours <= 24*30 {
		cfg.SessionHours = *req.SessionHours
	}
	if req.LoginMaxFail != nil && *req.LoginMaxFail > 0 && *req.LoginMaxFail <= 50 {
		cfg.LoginMaxFail = *req.LoginMaxFail
	}
	if req.LoginLockMins != nil && *req.LoginLockMins > 0 && *req.LoginLockMins <= 1440 {
		cfg.LoginLockMins = *req.LoginLockMins
	}
	if req.Require2FA != nil {
		cfg.Require2FA = *req.Require2FA
	}
	if req.PanelPublicURL != nil {
		cfg.PanelPublicURL = strings.TrimSpace(*req.PanelPublicURL)
	}
	if req.PanelSuffix != nil {
		raw := strings.TrimSpace(*req.PanelSuffix)
		next := config.NormalizePanelSuffix(raw)
		if raw != "" && next == "" {
			fail(w, http.StatusBadRequest, "安全后缀只能用字母、数字、- 与 _")
			return
		}
		// 后缀是面板自己的路径，改了之后当前页面立刻失效 —— 保存时就告诉用户新地址，
		// 别让人改完自己找不到面板了。
		cfg.PanelSuffix = next
	}
	if req.UpgradeSource != nil {
		src := strings.TrimSpace(*req.UpgradeSource)
		// 只做最基本的形状校验：空串是合法的（= 清空）。
		// 真正的可用性由"检查更新"去验证 —— 在保存时就发网络请求会让
		// 一个填错的地址把设置页卡住。
		if src != "" && !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
			fail(w, http.StatusBadRequest, "升级源地址必须以 http:// 或 https:// 开头")
			return
		}
		cfg.UpgradeSource = src
	}
	// 终端设置变更后需要重建管理器才生效（它是惰性单例）
	resetTerm := false
	if req.TerminalEnabled != nil {
		cfg.TerminalEnabled = *req.TerminalEnabled
		resetTerm = true
	}
	if req.TerminalShell != nil {
		sh := strings.TrimSpace(*req.TerminalShell)
		if sh != "" {
			if !strings.HasPrefix(sh, "/") {
				fail(w, http.StatusBadRequest, "Shell 必须是绝对路径（例如 /bin/zsh）")
				return
			}
			if _, err := os.Stat(sh); err != nil {
				fail(w, http.StatusBadRequest, "指定的 Shell 不存在: "+sh)
				return
			}
		}
		cfg.TerminalShell = sh
		resetTerm = true
	}
	if req.TerminalIdleMins != nil && *req.TerminalIdleMins >= 0 && *req.TerminalIdleMins <= 1440 {
		cfg.TerminalIdleMins = *req.TerminalIdleMins
		resetTerm = true
	}
	if req.TerminalMaxSessions != nil && *req.TerminalMaxSessions > 0 && *req.TerminalMaxSessions <= 20 {
		cfg.TerminalMaxSessions = *req.TerminalMaxSessions
		resetTerm = true
	}
	if resetTerm {
		s.termOnce = sync.Once{}
		s.termMgr = nil
	}

	// MySQL 连接设置
	if req.MySQLHost != nil {
		cfg.MySQLHost = strings.TrimSpace(*req.MySQLHost)
	}
	if req.MySQLPort != nil && *req.MySQLPort > 0 && *req.MySQLPort < 65536 {
		cfg.MySQLPort = *req.MySQLPort
	}
	if req.MySQLSocket != nil {
		cfg.MySQLSocket = strings.TrimSpace(*req.MySQLSocket)
	}
	if req.MySQLUser != nil {
		cfg.MySQLUser = strings.TrimSpace(*req.MySQLUser)
	}
	if req.MySQLPassword != nil {
		cfg.MySQLPassword = *req.MySQLPassword
	}

	if err := cfg.Save(); err != nil {
		fail(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	s.audit(r, "settings_save", "panel", "更新访问策略/安全设置", true, "")
	ok(w, s.settingsView())
}

func validCIDROrIP(s string) bool {
	if !strings.Contains(s, "/") {
		return net.ParseIP(s) != nil
	}
	_, _, err := net.ParseCIDR(s)
	return err == nil
}

// ---------- 小工具 ----------

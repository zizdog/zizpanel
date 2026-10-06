package web

// api_remote_desktop.go —— 「网页远程桌面」：面板里直接看/操作本机桌面。
//
// 为什么做（用户 2026-10-06）：面板以 LaunchDaemon 跑在无头 Mac 上，macOS 的 GUI 授权
// 弹窗只能在**控制台会话**上点；原来只能靠"另开屏幕共享/VNC 客户端 + 把 5900 暴露出去"。
// 现在面板自己当 VNC 中继：浏览器 ←WebSocket→ 面板 ←TCP→ 127.0.0.1:5900（macOS 屏幕共享），
// 于是**只需要面板自己的入口**（面板会话 + 可开两步验证），不用对外开 5900。
//
// 三个关键决定：
//   1. **不做协议翻译**：noVNC 直接说 RFB，面板只是字节中继（同 websockify 的做法）。
//   2. **不用"VNC 旧版密码"**：那会把机器认证降级成一个可爆破的 8 位密码。noVNC 1.5 原生支持
//      Apple 的 ARD 认证（安全类型 30）与 `RFB 003.889`，所以直接用 **Mac 账号 + 密码**，
//      面板既不存密码、也不改系统的认证设置。
//   3. **上游地址写死 127.0.0.1:5900**：前端只能连本机屏幕共享，连不到别处（没有 SSRF 面）。

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/sharing"
)

// remoteDesktopAddr 是面板中继的目标：默认写死本机屏幕共享（门禁把它改成临时端口）。
// 前端/接口**没有**任何参数能改它 —— 面板只能连本机 5900，没有 SSRF 面。
var remoteDesktopAddr = "127.0.0.1:5900"

const (
	// screensharingLabel 是 macOS 屏幕共享的 launchd 作业（默认 Disabled=1）。
	screensharingLabel = "com.apple.screensharing"
)

// remoteDesktopPort 是目标端口（从 remoteDesktopAddr 推，避免两处写死漂移）。
func remoteDesktopPort() int {
	if _, p, err := net.SplitHostPort(remoteDesktopAddr); err == nil {
		if n, cerr := strconv.Atoi(p); cerr == nil {
			return n
		}
	}
	return 5900
}

// screensharingPlist 是屏幕共享的作业文件（launchctl bootstrap 用它）。
const screensharingPlist = "/System/Library/LaunchDaemons/com.apple.screensharing.plist"

// remoteDesktopExecFn 是"读服务 ACL"用的执行器（与 sharing 包同一套：门禁用 PATH 垫片）。
var remoteDesktopExecFn = func() *sharing.Executor { return &sharing.Executor{Timeout: 20 * time.Second} }

// remoteDesktopLaunchctl 跑一次 launchctl 并拿回合并输出（execCommand 是包级注入点，
// 门禁不需要真动 launchd）。
func remoteDesktopLaunchctl(ctx context.Context, args ...string) (string, error) {
	out, err := execCommand(ctx, "/bin/launchctl", args...).CombinedOutput()
	return string(out), err
}

// remoteDesktopStatus 是远程桌面的状态（前端据此决定显示什么）。
type remoteDesktopStatus struct {
	Enabled      bool `json:"enabled"`
	EnabledKnown bool `json:"enabled_known"`
	// Listening = 5900 真的在听（判据贴运行体：作业"已启用"不等于服务在跑）。
	Listening bool `json:"listening"`
	Port      int  `json:"port"`
	// Access 是 com.apple.access_screensharing 服务 ACL（只读显示：组存在=只允许成员）。
	Access sharing.GroupInfo `json:"access"`
	// AuthTypes 是 RFB 自检读到的安全类型（30 = Apple ARD，2 = VNC 旧版口令）。
	AuthTypes []int `json:"auth_types,omitempty"`
	// AuthSupported 表示"面板能连"（有 30 或 2）。
	AuthSupported bool `json:"auth_supported"`
	// CheckError 是自检失败的原因（读不到就说读不到，不猜）。
	CheckError string `json:"check_error,omitempty"`
	IsRoot     bool   `json:"is_root"`
}

// handleRemoteDesktopStatus GET /api/v1/system/remote-desktop
func (s *Server) handleRemoteDesktopStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, s.remoteDesktopStatus(r.Context()))
}

func (s *Server) remoteDesktopStatus(ctx context.Context) remoteDesktopStatus {
	exec := remoteDesktopExecFn()
	st := remoteDesktopStatus{
		Port:      remoteDesktopPort(),
		IsRoot:    os.Geteuid() == 0,
		AuthTypes: []int{},
		Listening: portListening(ctx, remoteDesktopPort()),
		Access:    exec.AccessGroupNamed(ctx, sharing.AccessScreensharingGroup, config.PanelUser()),
	}
	enabled, known := remoteDesktopJobEnabled(ctx)
	st.Enabled, st.EnabledKnown = enabled, known
	if st.Listening {
		types, err := rfbSecurityTypes(ctx, remoteDesktopAddr)
		if err != nil {
			st.CheckError = err.Error()
		} else {
			st.AuthTypes = types
			for _, t := range types {
				if t == rfbSecurityARD || t == rfbSecurityVNCAuth {
					st.AuthSupported = true
				}
			}
			if !st.AuthSupported {
				st.CheckError = fmt.Sprintf("屏幕共享在听，但它提供的认证方式面板不支持（%v）", types)
			}
		}
	}
	return st
}

// remoteDesktopJobEnabled 读 launchd 里屏幕共享作业的启用状态。
//
// 用 `launchctl print-disabled system`（能读到 `"com.apple.screensharing" => enabled|disabled`）；
// 读不到就 EnabledKnown=false —— 界面显示"未复核"，绝不猜。
func remoteDesktopJobEnabled(ctx context.Context) (bool, bool) {
	out, err := remoteDesktopLaunchctl(ctx, "print-disabled", "system")
	if err != nil {
		return false, false
	}
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, screensharingLabel) {
			continue
		}
		low := strings.ToLower(ln)
		switch {
		case strings.Contains(low, "=> enabled"), strings.Contains(low, "=> false"):
			return true, true
		case strings.Contains(low, "=> disabled"), strings.Contains(low, "=> true"):
			return false, true
		}
	}
	// 没出现在 print-disabled 里：再用 `launchctl print` 探一次作业在不在。
	if _, err := remoteDesktopLaunchctl(ctx, "print", "system/"+screensharingLabel); err == nil {
		return true, true
	}
	// 两个都读不到就**如实说"未复核"**：绝不能拿"5900 有人在听"当"开机自启已开"
	//（2026-10-06 踩到：调试实例上被别人（假 VNC）占了 5900，界面就显示了"开机自启已开"）。
	return false, false
}

// handleRemoteDesktopAction POST /api/v1/system/remote-desktop/{enable|disable}
//
// 和 SMB 那一套同一个纪律：命令的退出码不算数，**以 5900 是否真的在听为准**回读。
func (s *Server) handleRemoteDesktopAction(w http.ResponseWriter, r *http.Request) {
	action := strings.ToLower(strings.TrimSpace(r.PathValue("action")))
	if action != "enable" && action != "disable" {
		fail(w, http.StatusBadRequest, "动作只能是 enable 或 disable")
		return
	}
	if os.Geteuid() != 0 {
		fail(w, http.StatusForbidden, "面板不是以 root 运行，无法开关屏幕共享（调试实例上没有这个能力）")
		return
	}
	ctx := r.Context()
	var cmdErr error
	for _, args := range remoteDesktopCommandPlan(action) {
		if _, err := remoteDesktopLaunchctl(ctx, args...); err != nil && cmdErr == nil {
			cmdErr = err
		}
	}
	// 回读：等端口状态变成我们要的样子（最多 10 秒）。
	want := action == "enable"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if portListening(ctx, remoteDesktopPort()) == want {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
	}
	got := portListening(ctx, remoteDesktopPort())
	s.audit(r, "remote_desktop_"+action, "screen-sharing",
		fmt.Sprintf("%s 屏幕共享（%d 端口在听=%v）", action, remoteDesktopPort(), got), got == want, firstLine(errText(cmdErr)))
	if got != want {
		if cmdErr != nil {
			fail(w, http.StatusBadGateway, fmt.Sprintf("命令报错，且 %d 端口状态没变（%v）：%v",
				remoteDesktopPort(), got, cmdErr))
			return
		}
		fail(w, http.StatusBadGateway, fmt.Sprintf("命令退出码 0，但 %d 端口状态仍是 %v —— 面板不算成功",
			remoteDesktopPort(), got))
		return
	}
	ok(w, s.remoteDesktopStatus(ctx))
}

// remoteDesktopCommandPlan 返回开/关屏幕共享要跑的命令（纯函数：门禁断言 argv，
// 处理器与门禁共用一份，避免"测试断言的和真跑的不是同一串"）。
func remoteDesktopCommandPlan(action string) [][]string {
	if action == "enable" {
		return [][]string{
			{"enable", "system/" + screensharingLabel},
			{"bootstrap", "system", screensharingPlist},
		}
	}
	return [][]string{
		{"bootout", "system/" + screensharingLabel},
		{"disable", "system/" + screensharingLabel},
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------- RFB 自检（只读握手，用来判断"面板到底能不能连"） ----------

// RFB 安全类型（只写用到的两个）。
const (
	rfbSecurityVNCAuth = 2
	rfbSecurityARD     = 30
)

// rfbSecurityTypes 连一次 127.0.0.1:5900，只做版本协商 + 读安全类型列表，然后断开。
//
// 为什么值得做：屏幕共享作业"已启用"、5900"在听"都不等于**面板能连** —— macOS 只有在
// 打开"VNC 旧版密码"时才提供类型 2，平时提供的是 Apple 自己的 ARD（30）。这两个结论
// 只能从运行体上问出来（用户 2026-10-06 的需求：远程桌面要能真的看到画面）。
func rfbSecurityTypes(ctx context.Context, addr string) ([]int, error) {
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("连不上 %s：%v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	ver := make([]byte, 12)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return nil, fmt.Errorf("读版本失败：%v", err)
	}
	if !strings.HasPrefix(string(ver), "RFB ") {
		return nil, fmt.Errorf("%s 不是 RFB 服务（收到 %q）", addr, strings.TrimSpace(string(ver)))
	}
	// 3.8 是标准的"报安全类型数量 + 列表"版本；Apple 的 003.889 也接受它。
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return nil, fmt.Errorf("发版本失败：%v", err)
	}
	n := make([]byte, 1)
	if _, err := io.ReadFull(conn, n); err != nil {
		return nil, fmt.Errorf("读安全类型数量失败：%v", err)
	}
	count := int(n[0])
	if count == 0 {
		// 0 = 服务器拒绝（后面跟原因字符串），如实把原因带出来。
		reason := make([]byte, 4)
		if _, err := io.ReadFull(conn, reason); err == nil {
			l := binary.BigEndian.Uint32(reason)
			if l > 0 && l < 4096 {
				buf := make([]byte, l)
				if _, err := io.ReadFull(conn, buf); err == nil {
					return nil, fmt.Errorf("屏幕共享拒绝了握手：%s", strings.TrimSpace(string(buf)))
				}
			}
		}
		return nil, fmt.Errorf("屏幕共享拒绝了握手（没给原因）")
	}
	types := make([]byte, count)
	if _, err := io.ReadFull(conn, types); err != nil {
		return nil, fmt.Errorf("读安全类型列表失败：%v", err)
	}
	out := make([]int, 0, count)
	for _, t := range types {
		out = append(out, int(t))
	}
	return out, nil
}

// ---------- WebSocket 中继 ----------

// handleRemoteDesktopWS GET /api/v1/system/remote-desktop/ws
//
// 浏览器 ←→ 面板 ←→ 127.0.0.1:5900 的纯字节中继（noVNC 自己说 RFB 协议）。
// 鉴权与会话约束与 Web 终端一致：WebSocket 带不了自定义头，所以只校验会话
// （同源 + SameSite Cookie 覆盖 CSRF 面），并在连接前后各写一条审计。
func (s *Server) handleRemoteDesktopWS(w http.ResponseWriter, r *http.Request) {
	// 先看屏幕共享在不在听：不在听就当场 409 + 人话，别让浏览器只看到 1006。
	if !portListening(r.Context(), remoteDesktopPort()) {
		fail(w, http.StatusConflict,
			fmt.Sprintf("屏幕共享没在运行（%d 端口没人听）。先点上面的「开启屏幕共享」再连。", remoteDesktopPort()))
		return
	}
	tok := s.sessionToken(r)
	u, err := s.Auth.AuthSession(r.Context(), tok)
	if err != nil {
		fail(w, http.StatusUnauthorized, "未登录或会话已过期")
		return
	}
	if verr := validateWSHandshake(r); verr != nil {
		v := wsUpgradeFailure(r, verr)
		fail(w, http.StatusBadRequest, v.Advice+"\n"+v.Detail)
		return
	}

	// 先连上游再升级：连不上就还能回一个正常的 HTTP 错误。
	upstream, derr := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(r.Context(), "tcp", remoteDesktopAddr)
	if derr != nil {
		fail(w, http.StatusBadGateway, fmt.Sprintf("连不上本机屏幕共享（%s）：%v", remoteDesktopAddr, derr))
		return
	}
	defer func() { _ = upstream.Close() }()

	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		s.Log.Warn("远程桌面 WebSocket 升级失败：%v", err)
		return
	}
	defer func() { _ = ws.Close() }()

	s.audit(r, "remote_desktop_attach", "screen-sharing",
		fmt.Sprintf("用户 %s 打开网页远程桌面（来源 %s）", u.Username, s.clientIP(r)), true, "")

	var wg sync.WaitGroup
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	// 上游(TCP) → 浏览器(WebSocket)：原始字节按二进制帧发。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stop()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := upstream.Read(buf)
			if n > 0 {
				if werr := ws.WriteBinary(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// 浏览器(WebSocket) → 上游(TCP)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stop()
		for {
			op, payload, rerr := ws.ReadMessage()
			if rerr != nil {
				return
			}
			switch op {
			case wsOpBinary, wsOpText, wsOpContinuation:
				if len(payload) > 0 {
					if _, werr := upstream.Write(payload); werr != nil {
						return
					}
				}
			}
		}
	}()

	<-done
	_ = upstream.Close()
	_ = ws.Close()
	wg.Wait()
}

package web

// nav_listen.go —— 「导航页独立端口」（坑 222）。
//
// 需求（用户）：导航页要能在 `127.0.0.1:<可配置端口>` 上独立提供服务，
// 好让隧道工具（Orbien `service = "127.0.0.1:xxx"`）把域名 op.zizdog.com 映射进来。
//
// 为什么不能只靠面板端口上的 /nav/：
//   · 面板主端口是 HTTPS 自签 + 安全后缀 —— 隧道按 `protocol="http"` 连不上，
//     按 https 又要处理证书，用户侧多一层无谓的坑；
//   · 独立端口是**纯 HTTP + 只绑回环**：外网只能经隧道进（隧道自己跑在本机）。
//
// 鉴权：这一面**不要求面板登录**（公网首页），且**不套 accessControl** ——
// 隧道带来的请求 X-Forwarded-For 可能是公网 IP，套白名单会把隧道自己挡在门外；
// 真正的边界是"只绑 127.0.0.1"（只有本机进程/隧道能连）。
// 只读：这一面只暴露页面、静态资源、公开数据与图标，写接口一个都不挂。

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// navListenMinPort：1024 以下需要 root，且极易与 nginx(80)/系统服务抢，
	// 直接拒绝并说清原因，比"绑失败了再猜"好。
	navListenMinPort = 1024
	navListenMaxPort = 65535
)

// ValidateNavListenPort 校验导航页独立端口，返回人话错误。
func ValidateNavListenPort(port int) error {
	if port <= 0 {
		return errors.New("导航页独立端口不能为 0（要关闭请用上面的开关，不要填 0）")
	}
	if port > navListenMaxPort {
		return fmt.Errorf("导航页独立端口超出范围（%d~%d，收到 %d）", navListenMinPort, navListenMaxPort, port)
	}
	if port < navListenMinPort {
		return fmt.Errorf("导航页独立端口不能小于 %d（收到 %d）：低端口需要 root 且易与系统服务冲突",
			navListenMinPort, port)
	}
	return nil
}

// navListenState 是**生效状态**（不是配置里的值）：界面回读的就是它。
type navListenState struct {
	Running bool
	Port    int
	URL     string
	Err     string
}

// navListener 管理独立监听的生命周期。
//
// 幂等：同一端口重复 apply 是空操作（不重绑、不中断已建立的连接）；
// 改端口：**先绑新的、成功后才关旧的**，所以"新端口被占用"不会把好的打掉。
type navListener struct {
	s *Server

	mu      sync.Mutex
	srv     *http.Server
	ln      net.Listener
	port    int
	lastErr string
	// starts 记录真实的 bind 次数，供门禁断言"幂等不重启"。
	starts int
}

func (l *navListener) apply(enabled bool, port int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !enabled {
		l.stopLocked()
		l.lastErr = ""
		return nil
	}
	if err := ValidateNavListenPort(port); err != nil {
		l.lastErr = err.Error()
		return err
	}
	if l.ln != nil && l.port == port {
		return nil // 幂等：端口没变就不动它
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		msg := navListenErrText(port, err)
		l.lastErr = msg
		return errors.New(msg)
	}
	// 绑定成功之后才关旧的：换端口失败时旧监听继续服务。
	l.stopLocked()
	srv := &http.Server{
		Handler:           l.s.navStandaloneHandler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	l.srv, l.ln, l.port = srv, ln, port
	l.lastErr = ""
	l.starts++
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// stopLocked 关闭当前监听（调用方必须持锁）。
func (l *navListener) stopLocked() {
	if l.srv != nil {
		_ = l.srv.Close()
	}
	l.srv, l.ln, l.port = nil, nil, 0
}

func (l *navListener) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopLocked()
}

// state 返回给界面回读的生效状态。
func (l *navListener) state() navListenState {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := navListenState{Err: l.lastErr}
	if l.ln != nil {
		st.Running = true
		st.Port = l.port
		st.URL = fmt.Sprintf("http://127.0.0.1:%d/", l.port)
	}
	return st
}

// navListenErrText 把绑定失败翻译成"能照着做的事"，并保留后端原文。
func navListenErrText(port int, err error) string {
	if errors.Is(err, syscall.EADDRINUSE) || strings.Contains(err.Error(), "address already in use") {
		return fmt.Sprintf("导航页独立端口 %d 已被占用（后端原文：%v）。"+
			"请换一个端口，或先停掉占用它的程序：终端执行 lsof -nP -iTCP:%d -sTCP:LISTEN", port, err, port)
	}
	return fmt.Sprintf("导航页独立端口 %d 监听失败（只绑 127.0.0.1，后端原文：%v）", port, err)
}

// ApplyNavListener 应用端口配置并回读生效值（面板设置保存时调用）。
func (s *Server) ApplyNavListener(enabled bool, port int) error {
	if s.navListen == nil {
		return errors.New("导航页独立端口管理器未初始化")
	}
	return s.navListen.apply(enabled, port)
}

// NavListenerState 返回导航页独立监听的生效状态。
func (s *Server) NavListenerState() navListenState {
	if s.navListen == nil {
		return navListenState{}
	}
	return s.navListen.state()
}

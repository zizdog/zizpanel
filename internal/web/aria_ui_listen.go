package web

// aria_ui_listen.go —— AriaNg 的**独立端口**（用户 2026-09-23 要求："现在要给 aria2 前端一个独立的端口"）。
//
// 为什么必须有这个端口（用户原话的两种症状都源于它）：
//   · 面板内的 /aria/ 要面板会话 —— 用域名反代出去时浏览器没有面板 Cookie，直接 401，
//     所以"直接反代 :8443/aria 无法使用"；
//   · 只反代 RPC（6800）也不够：6800 是 JSON-RPC，不是网页；而且 AriaNg 在 https 页面里
//     连 http 的 RPC 会被浏览器按混合内容拦掉（用户看到的"必须 SSL 或者 WebSocket 安全模式"）。
//
// 这一面把"界面 + 同源 /jsonrpc 代理"一起端出来：
//   反代 <域名>:8888 → http://127.0.0.1:<aria2_ui_port> 即可用；界面里的 RPC 走**同源相对路径**，
//   https 反代下自动就是 https，不再报安全模式。
//
// 边界：**绑 0.0.0.0**（用户要求局域网直连）、不套面板会话与 accessControl
// —— 只有本机进程/隧道/反代能连到它。RPC 自身由 aria2 的 `rpc-secret` 保护（AriaNg 会带 token）。

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

// ValidateAria2UIPort 校验 AriaNg 独立端口，返回人话错误。
func ValidateAria2UIPort(port int) error {
	if port <= 0 {
		return errors.New("AriaNg 独立端口不能为 0")
	}
	if port > navListenMaxPort {
		return fmt.Errorf("AriaNg 独立端口超出范围（%d~%d，收到 %d）", navListenMinPort, navListenMaxPort, port)
	}
	if port < navListenMinPort {
		return fmt.Errorf("AriaNg 独立端口不能小于 %d（收到 %d）：低端口需要 root 且易与系统服务冲突",
			navListenMinPort, port)
	}
	return nil
}

// ariaUIState 是独立端口的**生效状态**（不是配置值）。
type ariaUIState struct {
	Running bool
	Port    int
	URL     string
	Err     string
}

// ariaUIListener 管理 AriaNg 独立监听的生命周期（与 navListener 同一套幂等/换端口语义）。
type ariaUIListener struct {
	s *Server

	mu      sync.Mutex
	srv     *http.Server
	ln      net.Listener
	port    int
	lastErr string
	starts  int
}

func (l *ariaUIListener) apply(port int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ValidateAria2UIPort(port); err != nil {
		l.lastErr = err.Error()
		return err
	}
	if l.ln != nil && l.port == port {
		return nil // 幂等
	}
	// 绑 0.0.0.0：用户 2026-09-23 要求局域网直连端口访问（⚠️ 界面无登录保护，
	// 页面里还会带 RPC 密钥 —— 只在可信局域网这样用）。
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		msg := ariaUIPortErrText(port, err)
		l.lastErr = msg
		return errors.New(msg)
	}
	// 绑成功才关旧的：换端口失败时旧监听继续服务。
	l.stopLocked()
	srv := &http.Server{
		Handler:           l.s.ariaStandaloneHandler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	l.srv, l.ln, l.port = srv, ln, port
	l.lastErr = ""
	l.starts++
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (l *ariaUIListener) stopLocked() {
	if l.srv != nil {
		_ = l.srv.Close()
	}
	l.srv, l.ln, l.port = nil, nil, 0
}

func (l *ariaUIListener) state() ariaUIState {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := ariaUIState{Err: l.lastErr}
	if l.ln != nil {
		st.Running = true
		st.Port = l.port
		st.URL = fmt.Sprintf("http://127.0.0.1:%d/", l.port)
	}
	return st
}

// ariaUIPortErrText 把绑定失败翻译成"能照着做的事"，并保留后端原文。
func ariaUIPortErrText(port int, err error) string {
	if errors.Is(err, syscall.EADDRINUSE) || strings.Contains(err.Error(), "address already in use") {
		return fmt.Sprintf("AriaNg 独立端口 %d 已被占用（后端原文：%v）。请在「面板设置」里换一个端口。", port, err)
	}
	return fmt.Sprintf("AriaNg 独立端口 %d 绑定失败：%v", port, err)
}

// ApplyAriaUIListener 让面板在 127.0.0.1:<port> 上提供 AriaNg（界面 + 同源 RPC 代理）。
func (s *Server) ApplyAriaUIListener(port int) error {
	return s.ariaUI.apply(port)
}

// AriaUIListenerState 回读独立端口的生效状态（界面显示"是否真的在听"）。
func (s *Server) AriaUIListenerState() ariaUIState { return s.ariaUI.state() }

// ariaStandaloneHandler 是独立端口上的路由。
//
//   - 界面与静态资源：复用面板内 /aria/ 的**同一份实现**（路径改写到 slug 前缀下）；
//   - /jsonrpc：转发到 127.0.0.1:6800（**不要求面板会话** —— 边界是 RPC 口令，
//     RPC 自身由 aria2 的 rpc-secret 保护）。
func (s *Server) ariaStandaloneHandler() http.Handler {
	slug := "/" + ariaSlugPath()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jsonrpc", s.handleAria2RPCLoopback)
	mux.HandleFunc("POST /"+ariaSlugPath()+"/jsonrpc", s.handleAria2RPCLoopback)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 独立端口的根就是界面；面板内那份按 /<slug>/ 解析，这里补上前缀再交给它。
		if strings.HasPrefix(r.URL.Path, slug) {
			s.handleAria2UI(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		if r.URL.Path == "/" {
			r2.URL.Path = slug + "/"
		} else {
			r2.URL.Path = slug + r.URL.Path
		}
		s.handleAria2UI(w, r2)
	})
	return mux
}

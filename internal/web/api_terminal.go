package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/term"
)

// ============================================================================
//  Web 终端
//
//  这是整个面板权限最大的功能，因此约束也最多：
//    - 默认关闭，需要用户在「面板设置」里显式开启
//    - 会话数有上限，空闲会超时断开
//    - 每个会话的开始/结束写入审计日志
//    - 默认以真实用户身份启动 shell，而不是 root
// ============================================================================

// termManager 构造（或复用）终端管理器。
//
// 用惰性 + 单例：终端开关可以在运行时改变，每次请求重建会丢掉已有会话。
func (s *Server) termManager() *term.Manager {
	s.termOnce.Do(func() {
		m := term.NewManager(term.Options{
			Enabled:     s.Cfg.TerminalEnabled,
			User:        s.Cfg.User,
			Home:        s.Cfg.UserHome,
			Shell:       s.Cfg.TerminalShell,
			IdleTimeout: time.Duration(s.Cfg.TerminalIdleMins) * time.Minute,
			MaxSessions: s.Cfg.TerminalMaxSessions,
			Cols:        120, Rows: 32,
		})
		m.OnAudit = func(event string, sess *term.Session, detail string) {
			s.auditTerminal(event, sess, detail)
		}
		s.termMgr = m
	})
	return s.termMgr
}

// auditTerminal 把终端会话事件写入审计日志。
func (s *Server) auditTerminal(event string, sess *term.Session, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 用系统账号标记，"actor" 由会话信息提供
	_, err := s.Store.DB().ExecContext(ctx,
		`INSERT INTO audit_logs(actor,ip,action,target,detail,ok,message) VALUES(?,?,?,?,?,?,?)`,
		sess.Username, sess.RemoteIP, event, "terminal", detail, 1, "")
	if err != nil {
		s.Log.Warn("写终端审计失败: %v", err)
	}
}

// handleTerminalStatus 返回终端可用性与配置（前端据此决定是否显示入口）。
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	mgr := s.termManager()
	user := s.Cfg.User
	note := ""
	if user == "" {
		user = "root"
	}
	if user == "root" {
		note = "终端以 root 身份运行。如果要降权，请在面板设置里指定运行用户。"
	} else {
		note = fmt.Sprintf("终端以用户 %s 身份运行；需要 root 权限时在终端里使用 sudo。", user)
	}
	ok(w, map[string]any{
		"enabled":           mgr.Enabled(),
		"user":              user,
		"shell":             mgr.Options().Shell,
		"home":              s.Cfg.UserHome,
		"sessions":          mgr.Count(),
		"max_sessions":      mgr.Options().MaxSessions,
		"idle_timeout_mins": s.Cfg.TerminalIdleMins,
		"note":              note,
		"list":              mgr.List(),
	})
}

// handleTerminalKill 强制关闭一个会话（管理员操作）。
func (s *Server) handleTerminalKill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mgr := s.termManager()
	mgr.Close(id)
	s.audit(r, "terminal_kill", id, "强制关闭终端会话", true, "")
	ok(w, map[string]any{"msg": "会话已关闭"})
}

// handleTerminalWS 是终端的主入口：升级为 WebSocket 后双向转发数据。
func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	mgr := s.termManager()
	if !mgr.Enabled() {
		fail(w, http.StatusForbidden,
			"Web 终端未启用。请到「面板设置 → 终端」中开启（这是一个高权限功能，默认关闭）")
		return
	}

	// 鉴权：用与服务端一致的方式解析会话（Cookie 或 Bearer）。
	// 注意 WebSocket 不能用自定义头传 CSRF token，因此这里只校验会话，
	// 不校验 CSRF —— 浏览器同源策略 + SameSite Cookie 已覆盖这类风险。
	tok := s.sessionToken(r)
	u, err := s.Auth.AuthSession(r.Context(), tok)
	if err != nil {
		fail(w, http.StatusUnauthorized, "未登录或会话已过期")
		return
	}

	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		s.Log.Warn("WebSocket 升级失败: %v", err)
		return
	}
	defer func() { _ = ws.Close() }()

	sess, err := mgr.NewSession(s.clientIP(r))
	if err != nil {
		_ = ws.WriteText(term.EncodeMessage(term.Message{Type: term.TypeError, Error: err.Error()}))
		return
	}
	// 会话关闭时也要写审计，因此用 defer 保证只写一次
	closed := false
	defer func() {
		if !closed {
			mgr.Close(sess.ID)
		}
	}()

	s.audit(r, "terminal_attach", sess.ID,
		fmt.Sprintf("用户 %s 打开终端（来源 %s）", u.Username, s.clientIP(r)), true, "")

	// 先告诉前端会话已建立
	_ = ws.WriteText(term.EncodeMessage(term.Message{
		Type: term.TypeOutput,
		Data: fmt.Sprintf("\x1b[36m已连接到 %s（用户 %s）\x1b[0m\r\n", hostname(), sess.Username),
	}))

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// pty -> WebSocket
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				if werr := ws.WriteText(term.EncodeMessage(term.Message{
					Type: term.TypeOutput, Data: string(buf[:n]),
				})); werr != nil {
					return
				}
			}
			if err != nil {
				// shell 退出（EOF）或读错误
				_ = ws.WriteText(term.EncodeMessage(term.Message{
					Type: term.TypeClose, Data: "\r\n\x1b[33m会话已结束\x1b[0m\r\n",
				}))
				return
			}
			if sess.Closed() {
				return
			}
		}
	}()

	// WebSocket -> pty
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			op, payload, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if op == 0x8 { // close
				return
			}
			msg, derr := term.DecodeMessage(payload)
			if derr != nil {
				continue
			}
			switch msg.Type {
			case term.TypeInput:
				if err := sess.Write([]byte(msg.Data)); err != nil {
					return
				}
			case term.TypeResize:
				_ = sess.Resize(msg.Cols, msg.Rows)
			case term.TypePing:
				_ = ws.WriteText(term.EncodeMessage(term.Message{Type: term.TypePong}))
			}
		}
	}()

	// 空闲回收：定期检查
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if sess.IdleSeconds() > int(mgr.Options().IdleTimeout.Seconds()) &&
					mgr.Options().IdleTimeout > 0 {
					_ = ws.WriteText(term.EncodeMessage(term.Message{
						Type: term.TypeClose, Data: "\r\n\x1b[33m空闲超时，会话已断开\x1b[0m\r\n",
					}))
					cancel()
					return
				}
			}
		}
	}()

	wg.Wait()
	mgr.Close(sess.ID)
	closed = true
	s.audit(r, "terminal_detach", sess.ID, "终端会话结束", true, "")
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "本机"
	}
	// 去掉 .local 后缀，终端提示符更清爽
	return strings.TrimSuffix(h, ".local")
}

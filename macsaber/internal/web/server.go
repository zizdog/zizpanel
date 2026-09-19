// Package web 是 mac军刀 的 HTTP 层：路由、鉴权、CSRF、内嵌前端。
//
// 约定（后续工具包不用改这里）：
//   - /api/*   JSON 接口，未登录一律 401；写操作缺 CSRF 头一律 403。
//   - /*       内嵌前端（原生 ESM，无构建步骤），未知路径回落 index.html。
package web

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/macsaber/internal/config"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
)

//go:embed all:assets
var assetsFS embed.FS

// Server 是 HTTP 服务。
type Server struct {
	Store *config.Store
	Guard *fsroot.Guard
	Reg   *tool.Registry
	Run   *tool.Runner
	Tasks *tasks.Manager
	// Version 显示在页脚与 /api/session。
	Version string
	// ReadRoots / WriteRoots 用于前端给路径参数下单选框。
	ReadRoots  []string
	WriteRoots []string
	Logf       func(format string, args ...any)
}

// New 建立服务（Logf 为 nil 时静默）。
func New(st *config.Store, g *fsroot.Guard, reg *tool.Registry, runner *tool.Runner, tm *tasks.Manager, version string) *Server {
	return &Server{
		Store: st, Guard: g, Reg: reg, Run: runner, Tasks: tm, Version: version,
		ReadRoots: g.ReadRoots(), WriteRoots: g.WriteRoots(),
	}
}

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---------- 公开 ----------
	mux.HandleFunc("GET /api/version", s.json(s.handleVersion))
	mux.HandleFunc("GET /api/setup/status", s.json(s.handleSetupStatus))
	mux.HandleFunc("POST /api/setup", s.json(s.handleSetup))
	mux.HandleFunc("POST /api/login", s.json(s.handleLogin))

	// ---------- 需登录 ----------
	mux.HandleFunc("GET /api/session", s.auth(s.handleSession))
	mux.HandleFunc("POST /api/logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /api/tools", s.auth(s.handleTools))
	mux.HandleFunc("POST /api/tools/{id}/run", s.auth(s.handleRun))
	mux.HandleFunc("GET /api/tasks", s.auth(s.handleTasksList))
	mux.HandleFunc("GET /api/tasks/{id}", s.auth(s.handleTaskGet))
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.auth(s.handleTaskCancel))
	// 产物下载：路径重新过一次读根闸门，签名 URL 不能变成任意文件读取。
	mux.HandleFunc("GET /api/files/download", s.auth(s.handleDownload))

	// ---------- 静态前端 ----------
	mux.HandleFunc("GET /", s.handleStatic)
	return securityHeaders(mux)
}

// ============================================================================
//  中间件
// ============================================================================

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

// auth 包裹需要登录的接口，并统一做 CSRF 双提交校验。
func (s *Server) auth(h func(http.ResponseWriter, *http.Request) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := sessionToken(r)
		if !s.Store.SessionValid(tok) {
			clearSessionCookies(w)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "msg": "未登录或会话已过期"})
			return
		}
		if isWriteMethod(r.Method) && !s.Store.CSRFOk(r, tok) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "msg": "CSRF 校验失败，请刷新页面重试"})
			return
		}
		s.json(h)(w, r)
	}
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(config.CookieSession); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

func setSessionCookies(w http.ResponseWriter, session string, csrf string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: config.CookieSession, Value: session, Path: "/",
		Expires: exp, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	// CSRF token 必须能被 JS 读到（双提交），因此不设 HttpOnly。
	http.SetCookie(w, &http.Cookie{
		Name: config.CookieCSRF, Value: csrf, Path: "/",
		Expires: exp, HttpOnly: false, SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookies(w http.ResponseWriter) {
	for _, n := range []string{config.CookieSession, config.CookieCSRF} {
		http.SetCookie(w, &http.Cookie{
			Name: n, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: n == config.CookieSession, SameSite: http.SameSiteLaxMode,
		})
	}
}

// bodyRaw 表示 handler 已经自己写完响应（如文件下载），不要再包一层 JSON。
const bodyRaw = "__raw__"

// json 把 handler 包成"统一 JSON 错误"的形式。
func (s *Server) json(h func(http.ResponseWriter, *http.Request) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code, body, err := h(w, r)
		if err != nil {
			if code == 0 {
				code = http.StatusInternalServerError
			}
			writeJSON(w, code, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		if body == bodyRaw {
			return
		}
		if code == 0 {
			code = http.StatusOK
		}
		writeJSON(w, code, body)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %v", err)
	}
	return nil
}

// ============================================================================
//  鉴权接口
// ============================================================================

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) (int, any, error) {
	return 200, map[string]any{"ok": true, "version": s.Version, "name": "mac军刀"}, nil
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) (int, any, error) {
	return 200, map[string]any{
		"ok": true, "inited": s.Store.Inited(), "name": "mac军刀", "version": s.Version,
	}, nil
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var body struct {
		User    string `json:"user"`
		Pass    string `json:"password"`
		Confirm string `json:"confirm"`
	}
	if err := decodeBody(r, &body); err != nil {
		return 400, nil, err
	}
	if body.Pass != body.Confirm {
		return 400, nil, errors.New("两次输入的口令不一致")
	}
	if err := s.Store.Setup(body.User, body.Pass); err != nil {
		return 400, nil, err
	}
	return s.issueSession(w, body.User)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var body struct {
		User string `json:"user"`
		Pass string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		return 400, nil, err
	}
	if !s.Store.Inited() {
		return 409, nil, errors.New("尚未初始化，请先设置用户名与口令")
	}
	if !s.Store.Verify(body.User, body.Pass) {
		s.logf("登录失败：用户 %q", body.User)
		return 401, nil, errors.New("用户名或口令不正确")
	}
	return s.issueSession(w, body.User)
}

func (s *Server) issueSession(w http.ResponseWriter, user string) (int, any, error) {
	tok := s.Store.NewSession()
	if tok == "" {
		return 500, nil, errors.New("生成会话失败")
	}
	setSessionCookies(w, tok, s.Store.Token(tok), time.Now().Add(12*time.Hour))
	return 200, map[string]any{"ok": true, "user": user}, nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if tok := sessionToken(r); tok != "" {
		s.Store.DropSession(tok)
	}
	clearSessionCookies(w)
	return 200, map[string]any{"ok": true}, nil
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) (int, any, error) {
	return 200, map[string]any{
		"ok": true, "user": s.Store.User, "version": s.Version,
		"read_roots": s.ReadRoots, "write_roots": s.WriteRoots,
		"sensitive_roots": s.Guard.SensitiveRoots(),
		"audit_log":       s.Run.Audit.Path(),
	}, nil
}

// ============================================================================
//  工具与任务
// ============================================================================

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) (int, any, error) {
	return 200, map[string]any{"ok": true, "categories": s.Reg.List()}, nil
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		return 400, nil, err
	}
	user := s.Store.User
	resp, code, err := s.Run.Run(r.Context(), user, id, body)
	if err != nil {
		return code, nil, err
	}
	if resp.Accepted {
		return 202, map[string]any{"ok": true, "accepted": true, "task_id": resp.TaskID}, nil
	}
	return 200, map[string]any{"ok": true, "result": resp.Result}, nil
}

func (s *Server) handleTasksList(w http.ResponseWriter, r *http.Request) (int, any, error) {
	return 200, map[string]any{"ok": true, "list": s.Tasks.List(50)}, nil
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) (int, any, error) {
	t, ok := s.Tasks.Get(r.PathValue("id"))
	if !ok {
		return 404, nil, errors.New("任务不存在")
	}
	return 200, map[string]any{"ok": true, "task": t.Snapshot()}, nil
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if !s.Tasks.Cancel(r.PathValue("id")) {
		return 409, nil, errors.New("任务不存在或已结束")
	}
	return 200, map[string]any{"ok": true}, nil
}

// handleDownload 只允许下载读根内的普通文件（签名 path 不能绕过闸门）。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) (int, any, error) {
	raw := r.URL.Query().Get("path")
	dec, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 400, nil, errors.New("下载参数非法")
	}
	real, err := s.Guard.Resolve(fsroot.Read, string(dec), false)
	if err != nil {
		return 403, nil, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return 404, nil, errors.New("文件不存在")
	}
	if st.IsDir() {
		return 400, nil, errors.New("目录不能下载")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(real)))
	// 头部写出后错误只能记日志：再写 JSON 会变成"200 + 追加字节"的假成功。
	s.logf("下载 %s", real)
	http.ServeFile(w, r, real)
	return 200, bodyRaw, nil
}

// ============================================================================
//  静态资源
// ============================================================================

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		http.Error(w, "资源不可用", http.StatusInternalServerError)
		return
	}
	if _, err := fs.Stat(sub, p); err != nil {
		// 未知路径回落 index.html（前端自己按 hash 路由）。
		p = "index.html"
	}
	if p == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	b, err := fs.ReadFile(sub, p)
	if err != nil {
		http.Error(w, "资源不可用", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeByExt(p))
	_, _ = w.Write(b)
}

func contentTypeByExt(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	}
	return "application/octet-stream"
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

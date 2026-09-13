package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/auth"
)

// ============================================================================
//  会话 Cookie
// ============================================================================

const (
	cookieSession = "zp_session"
	cookieCSRF    = "zp_csrf"
)

func (s *Server) setSessionCookies(w http.ResponseWriter, r *http.Request, token string, exp time.Time) {
	maxAge := int(time.Until(exp).Seconds())
	if maxAge < 60 {
		maxAge = 60
	}
	secure := s.cookieSecure(r)

	// HttpOnly：JS 读不到会话令牌，降低 XSS 窃取风险
	http.SetCookie(w, &http.Cookie{
		Name: cookieSession, Value: token, Path: "/",
		Expires: exp, MaxAge: maxAge, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure,
	})
	// CSRF token 必须能被 JS 读到（双提交模式），因此不设 HttpOnly
	http.SetCookie(w, &http.Cookie{
		Name: cookieCSRF, Value: s.csrfFor(token), Path: "/",
		Expires: exp, MaxAge: maxAge, HttpOnly: false,
		SameSite: http.SameSiteLaxMode, Secure: secure,
	})
}

// cookieSecure 判断本次响应该不该给 Cookie 加 Secure 标志。
//
// 这是必须按"实际访问协议"判断的，不能简单用面板自身的 TLS 开关：
// 面板监听 HTTPS，但通常挂在 nginx 的 http://localhost/_panel 后面。
// 这种情况下如果 Cookie 带 Secure，浏览器会直接丢弃它
// （Secure Cookie 只在 HTTPS 连接上被保存），表现为
// "密码正确但登录后立刻又回到登录页" —— 极难排查。
//
// 因此这里以 X-Forwarded-Proto 为准；没有该头时退回面板自身的 TLS 配置。
func (s *Server) cookieSecure(r *http.Request) bool {
	if r != nil && s.Cfg.TrustProxy {
		if proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); proto != "" {
			return proto == "https"
		}
	}
	if r != nil {
		// 未开启 TrustProxy 时也读一下该头：它是反代的标准约定，
		// 且由我们的安装脚本自己写入，不存在被外部伪造的问题
		// （能连接到面板 8443 的人本来就能直接访问面板）。
		if proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); proto != "" {
			return proto == "https"
		}
	}
	return s.Cfg.TLSEnable
}

func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	for _, n := range []string{cookieSession, cookieCSRF} {
		http.SetCookie(w, &http.Cookie{
			Name: n, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: n == cookieSession, SameSite: http.SameSiteLaxMode,
			Secure: s.Cfg.TLSEnable,
		})
	}
}

// sessionToken 从 Cookie 或 Authorization 头取会话令牌。
// 支持 Bearer 是为了让 CLI/脚本也能调用接口。
func (s *Server) sessionToken(r *http.Request) string {
	if c, err := r.Cookie(cookieSession); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// csrfFor 由会话令牌派生 CSRF token（HMAC），无需额外存储。
func (s *Server) csrfFor(token string) string {
	mac := hmac.New(sha256.New, []byte(s.Cfg.Secret))
	mac.Write([]byte("csrf:" + auth.HashToken(token)))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// ============================================================================
//  认证与授权中间件
// ============================================================================

type ctxKey int

const ctxUserKey ctxKey = 1

func userFrom(ctx context.Context) *auth.User {
	u, _ := ctx.Value(ctxUserKey).(*auth.User)
	return u
}

func withUser(ctx context.Context, u *auth.User) context.Context {
	return context.WithValue(ctx, ctxUserKey, u)
}

// requireAuth 包裹需要登录的接口。
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := s.sessionToken(r)
		u, err := s.Auth.AuthSession(r.Context(), tok)
		if err != nil {
			s.clearSessionCookies(w)
			fail(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		// 对写操作做 CSRF 双提交校验
		if isWriteMethod(r.Method) && !s.csrfOK(r, tok) {
			fail(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面重试")
			return
		}
		h(w, r.WithContext(withUser(r.Context(), u)))
	}
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// csrfOK 校验请求头里的 CSRF token 是否与会话匹配。
//
// 安全要点：必须只认请求头，不能"取不到头就退回读 Cookie"。
// 因为 Cookie 是浏览器自动携带的，如果接受 Cookie 里的值，
// 攻击者只要让受害者的浏览器发起跨站表单提交就能带上它，
// 双提交校验就完全失效了。前端从可读 Cookie 取值再放进头的做法才是正确姿势。
func (s *Server) csrfOK(r *http.Request, token string) bool {
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		return false
	}
	want := s.csrfFor(token)
	return hmac.Equal([]byte(got), []byte(want))
}

// ============================================================================
//  访问控制（远程访问的闸门）
// ============================================================================

// accessControl 是全局中间件：先判断来源 IP 是否允许访问面板。
//
// 这是"面板能被远程访问"与"面板不会被全网扫到"之间的平衡点：
//   - any       任意来源（默认；配合自签 HTTPS + 强密码）
//   - local     仅本机回环
//   - whitelist 仅 CIDR 白名单（可填 Tailscale 网段 100.64.0.0/10）
func (s *Server) accessControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		if !s.ipAllowed(ip) {
			s.Log.Warn("拒绝访问来源 %s (%s)", ip, r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/api/") {
				fail(w, http.StatusForbidden, "当前来源 IP 不在面板白名单内")
			} else {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("<h1>403 拒绝访问</h1><p>当前来源 " + ip +
					" 不在面板白名单内。请在面板「设置 → 访问策略」中调整。</p>"))
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ipAllowed(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	switch s.Cfg.AccessMode {
	case "local":
		return parsed.IsLoopback()
	case "whitelist":
		for _, cidr := range s.Cfg.IPWhitelist {
			if matchCIDR(parsed, cidr) {
				return true
			}
		}
		// 白名单模式下永远允许本机，避免把自己锁在门外
		return parsed.IsLoopback()
	default:
		return true
	}
}

func matchCIDR(ip net.IP, cidr string) bool {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return false
	}
	if !strings.Contains(cidr, "/") {
		// 允许直接写单个 IP
		if other := net.ParseIP(cidr); other != nil {
			return other.Equal(ip)
		}
		return false
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return ipnet.Contains(ip)
}

// clientIP 取客户端真实 IP。
// 只有显式开启 TrustProxy 时才信任 X-Forwarded-For，否则任何人都能伪造它绕过白名单。
func (s *Server) clientIP(r *http.Request) string {
	if s.Cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ============================================================================
//  登录限流（内存滑动窗口）
// ============================================================================

// loginLimiter 限制单个 IP / 账号在时间窗内的失败次数。
// 账号级锁定在 auth 包里（持久化），这里只做内存级快速拦截，
// 防止暴力破解把 CPU 打满（每次 bcrypt 校验约 200ms）。
type loginLimiter struct {
	mu     sync.Mutex
	events map[string][]time.Time
}

const (
	limiterWindow = 5 * time.Minute
	limiterMax    = 20
)

func (l *loginLimiter) key(ip, user string) string { return ip + "|" + user }

func (l *loginLimiter) allow(ip, user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.events == nil {
		l.events = map[string][]time.Time{}
	}
	k := l.key(ip, user)
	cutoff := time.Now().Add(-limiterWindow)
	kept := l.events[k][:0:0]
	for _, t := range l.events[k] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.events[k] = kept
	return len(kept) < limiterMax
}

func (l *loginLimiter) fail(ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.events == nil {
		l.events = map[string][]time.Time{}
	}
	k := l.key(ip, user)
	l.events[k] = append(l.events[k], time.Now())
}

func (l *loginLimiter) success(ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.events, l.key(ip, user))
}

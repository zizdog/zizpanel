package api

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zizdog/zizvideo/internal/domain"
)

// Cookie names. The session cookie is HttpOnly; the CSRF cookie must stay
// readable so the frontend can echo it in the X-CSRF-Token header.
const (
	CookieSession = "zv_session"
	CookieCSRF    = "zv_csrf"
)

func (s *Server) setSessionCookies(w http.ResponseWriter, r *http.Request, token string) {
	maxAge := int(s.Cfg.SessionTTL().Seconds())
	cookie := func(name, value string, httpOnly bool) *http.Cookie {
		return &http.Cookie{
			Name: name, Value: value, Path: "/",
			MaxAge: maxAge, HttpOnly: httpOnly,
			SameSite: http.SameSiteStrictMode,
			Secure:   s.Cfg.SecureCookie || forwardedHTTPS(r),
		}
	}
	http.SetCookie(w, cookie(CookieSession, token, true))
	http.SetCookie(w, cookie(CookieCSRF, s.Auth.CSRFFor(token), false))
}

func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	for _, n := range []string{CookieSession, CookieCSRF} {
		http.SetCookie(w, &http.Cookie{
			Name: n, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: n == CookieSession, SameSite: http.SameSiteStrictMode,
		})
	}
}

func forwardedHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// sessionToken reads the session cookie.
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(CookieSession); err == nil {
		return c.Value
	}
	return ""
}

// requireAuth rejects anonymous requests and enforces CSRF on writes.
func (s *Server) RequireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := sessionToken(r)
		u, err := s.Auth.Authenticate(token)
		if err != nil {
			s.clearSessionCookies(w)
			s.fail(w, r, domain.ErrUnauthorized)
			return
		}
		if isWriteMethod(r.Method) && !s.Auth.CSRFOK(r.Header.Get("X-CSRF-Token"), token) {
			s.fail(w, r, domain.ErrCSRF)
			return
		}
		h(w, r.WithContext(withUser(r.Context(), u)))
	}
}

// requireAdmin additionally requires the admin role, re-read from the database
// on every request by requireAuth.
func (s *Server) RequireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return s.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if u == nil || u.Role != domain.RoleAdmin {
			s.fail(w, r, domain.ErrForbidden)
			return
		}
		h(w, r)
	})
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// ============================================================================
//  Global middleware
// ============================================================================

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush keeps streaming responses working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Middleware is the outermost handler: request id, access log, panic recovery
// and client IP resolution.
func (s *Server) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		ctx := withRequestID(r.Context(), rid)
		ctx = withClientIP(ctx, s.clientIP(r))
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", rid)
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")

		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("请求处理崩溃", "request_id", rid, "path", r.URL.Path)
				if sw.status == 0 {
					writeJSON(sw, http.StatusInternalServerError, envelope{
						Error: &errorBody{Code: "INTERNAL_PANIC", Message: "服务内部错误"}})
				}
			}
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			fields := []any{
				"request_id", rid, "method", r.Method, "path", r.URL.Path,
				"status", status, "duration_ms", time.Since(start).Milliseconds(),
				"client_ip", clientIPFrom(r.Context()), "bytes", sw.bytes,
			}
			if u := UserFrom(r.Context()); u != nil {
				fields = append(fields, "user_id", u.ID)
			}
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				return
			}
			s.Log.Info("http", fields...)
		}()
		next.ServeHTTP(sw, r)
	})
}

// clientIP trusts X-Forwarded-For only when the peer is a configured proxy.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !s.trustedPeer(host) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	// Walk right to left and return the first address that is not a trusted peer.
	for i := len(parts) - 1; i >= 0; i-- {
		cand := strings.TrimSpace(parts[i])
		if cand == "" {
			continue
		}
		if !s.trustedPeer(cand) {
			return cand
		}
	}
	return host
}

func (s *Server) trustedPeer(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, cidr := range s.Cfg.TrustedProxies {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if !strings.Contains(cidr, "/") {
			if other := net.ParseIP(cidr); other != nil && other.Equal(parsed) {
				return true
			}
			continue
		}
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil && ipnet.Contains(parsed) {
			return true
		}
	}
	return false
}

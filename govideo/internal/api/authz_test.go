package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zizdog/govideo/internal/api"
	"github.com/zizdog/govideo/internal/domain"
)

func TestAnonymousRequestsAreRejected(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	c := e.anonClient()
	for _, path := range []string{
		"/api/v1/auth/me", "/api/v1/media", "/api/v1/feed/next",
		"/api/v1/users", "/api/v1/libraries", "/api/v1/me/progress",
		"/api/v1/admin/system/info",
	} {
		res, env, raw := e.doAs(c, http.MethodGet, path)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s 未登录应 401, 得到 %d (%s)", path, res.StatusCode, raw)
		}
		if env.Error == nil || env.Error.Code != "AUTH_UNAUTHORIZED" {
			t.Fatalf("%s 错误码 = %+v", path, env.Error)
		}
	}
}

func TestHealthEndpointsNeedNoSession(t *testing.T) {
	e := newEnv(t)
	c := e.anonClient()
	res, _, _ := e.doAs(c, http.MethodGet, "/healthz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", res.StatusCode)
	}
	res, _, _ = e.doAs(c, http.MethodGet, "/readyz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d", res.StatusCode)
	}
}

func TestNonAdminCannotReachAdminEndpoints(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	res, _, raw := e.write(http.MethodPost, "/api/v1/users", map[string]any{
		"username": "bob", "password": "bobpass12345", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("创建普通用户失败 %d: %s", res.StatusCode, raw)
	}

	bob := e.anonClient()
	if res, raw := e.loginAs(bob, "bob", "bobpass12345"); res.StatusCode != http.StatusOK {
		t.Fatalf("bob 登录失败 %d: %s", res.StatusCode, raw)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodGet, "/api/v1/libraries"},
		{http.MethodGet, "/api/v1/admin/system/info"},
	} {
		res, env, raw := e.doAs(bob, tc.method, tc.path)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s 普通用户应 403, 得到 %d (%s)", tc.path, res.StatusCode, raw)
		}
		if env.Error == nil || env.Error.Code != "FORBIDDEN_ROLE" {
			t.Fatalf("%s 错误码 = %+v", tc.path, env.Error)
		}
	}
	// The same person may read their own media list.
	res, _, raw = e.doAs(bob, http.MethodGet, "/api/v1/media")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("普通用户读媒体列表应 200, 得到 %d (%s)", res.StatusCode, raw)
	}
}

func TestWritesRequireCSRFHeader(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	payload := map[string]any{"name": "nocsrf", "root_path": e.Root}

	// Missing header.
	res, raw := e.callWith(e.Client, "", http.MethodPost, "/api/v1/libraries", payload, false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 头应 403, 得到 %d (%s)", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "FORBIDDEN_CSRF") {
		t.Fatalf("错误码不符: %s", raw)
	}

	// Wrong header value.
	res, raw = e.callWith(e.Client, "deadbeef", http.MethodPost, "/api/v1/libraries", payload, false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("错 CSRF 头应 403, 得到 %d (%s)", res.StatusCode, raw)
	}

	// Cookie value alone must not be accepted: the frontend has to echo the header.
	res, raw = e.call(http.MethodPost, "/api/v1/libraries", payload, true)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("正确 CSRF 头应 201, 得到 %d (%s)", res.StatusCode, raw)
	}

	// Reads never need the header.
	res, _, _ = e.do(http.MethodGet, "/api/v1/libraries", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("读操作不应要求 CSRF, 得到 %d", res.StatusCode)
	}
}

func TestCookieFlagsOnLogin(t *testing.T) {
	e := newEnv(t)
	// The setup response itself must carry both cookies.
	res, _ := e.call(http.MethodPost, "/api/v1/setup", map[string]string{
		"username": "admin", "password": "adminpass123"}, false)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("初始化失败 %d", res.StatusCode)
	}
	var session, csrf *http.Cookie
	for _, c := range res.Cookies() {
		switch c.Name {
		case api.CookieSession:
			session = c
		case api.CookieCSRF:
			csrf = c
		}
	}
	if session == nil || csrf == nil {
		t.Fatalf("缺少会话或 CSRF cookie: %v", res.Cookies())
	}
	if !session.HttpOnly {
		t.Fatal("会话 cookie 必须 HttpOnly")
	}
	if csrf.HttpOnly {
		t.Fatal("CSRF cookie 必须可被 JS 读取")
	}
	if session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("SameSite = %v, 期望 Strict", session.SameSite)
	}
}

// TestDisabledUserLosesAccessImmediately is the revocation gate: the session
// cookie stays valid, but the DB says disabled, so the next call must fail.
func TestDisabledUserLosesAccessImmediately(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	res, env, raw := e.write(http.MethodPost, "/api/v1/users", map[string]any{
		"username": "carol", "password": "carolpass12345", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("创建用户失败 %d: %s", res.StatusCode, raw)
	}
	var created domain.User
	decodeInto(t, env.Data, &created)

	carol := e.anonClient()
	if res, raw := e.loginAs(carol, "carol", "carolpass12345"); res.StatusCode != http.StatusOK {
		t.Fatalf("登录失败 %d: %s", res.StatusCode, raw)
	}
	if res, _, raw := e.doAs(carol, http.MethodGet, "/api/v1/media"); res.StatusCode != http.StatusOK {
		t.Fatalf("禁用前应可访问, 得到 %d (%s)", res.StatusCode, raw)
	}

	if res, _, raw := e.write(http.MethodPatch, "/api/v1/users/"+created.ID,
		map[string]string{"status": "disabled"}); res.StatusCode != http.StatusOK {
		t.Fatalf("禁用失败 %d: %s", res.StatusCode, raw)
	}
	res, _, raw = e.doAs(carol, http.MethodGet, "/api/v1/media")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("禁用后同一会话必须立刻失效, 得到 %d (%s)", res.StatusCode, raw)
	}
}

// TestRoleChangeAppliesWithoutRelogin proves the role is read from the DB.
func TestRoleChangeAppliesWithoutRelogin(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	_, env, _ := e.write(http.MethodPost, "/api/v1/users", map[string]any{
		"username": "dave", "password": "davepass12345", "role": "user"})
	var created domain.User
	decodeInto(t, env.Data, &created)

	dave := e.anonClient()
	if res, raw := e.loginAs(dave, "dave", "davepass12345"); res.StatusCode != http.StatusOK {
		t.Fatalf("登录失败 %d: %s", res.StatusCode, raw)
	}
	if res, _, _ := e.doAs(dave, http.MethodGet, "/api/v1/users"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("提升前应 403, 得到 %d", res.StatusCode)
	}
	if res, _, raw := e.write(http.MethodPatch, "/api/v1/users/"+created.ID,
		map[string]string{"role": "admin"}); res.StatusCode != http.StatusOK {
		t.Fatalf("提升失败 %d: %s", res.StatusCode, raw)
	}
	if res, _, raw := e.doAs(dave, http.MethodGet, "/api/v1/users"); res.StatusCode != http.StatusOK {
		t.Fatalf("提升后同一会话应 200, 得到 %d (%s)", res.StatusCode, raw)
	}
}

func TestLastAdminCannotBeRemoved(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	_, meEnv, _ := e.do(http.MethodGet, "/api/v1/auth/me", nil)
	var u domain.User
	decodeInto(t, meEnv.Data, &u)
	res, env, raw := e.write(http.MethodPatch, "/api/v1/users/"+u.ID,
		map[string]string{"role": "user"})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("最后一个管理员不应被降级, 得到 %d (%s)", res.StatusCode, raw)
	}
	if env.Error == nil || env.Error.Code != "FORBIDDEN_LAST_ADMIN" {
		t.Fatalf("错误码 = %+v", env.Error)
	}
	res, _, raw = e.write(http.MethodPatch, "/api/v1/users/"+u.ID,
		map[string]string{"status": "disabled"})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("不能禁用自己, 得到 %d (%s)", res.StatusCode, raw)
	}
}

func TestLoginBackoffAndLockout(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	c := e.anonClient()
	sawRateLimit := false
	for i := 0; i < 4; i++ {
		res, raw := e.loginAs(c, "admin", "wrong-password")
		if res.StatusCode == http.StatusTooManyRequests {
			sawRateLimit = true
			break
		}
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败登录状态 = %d (%s)", i+1, res.StatusCode, raw)
		}
	}
	if !sawRateLimit {
		t.Fatal("连续失败登录必须触发指数退避/锁定")
	}
	// A locked account must refuse even the correct password.
	res, raw := e.loginAs(c, "admin", "adminpass123")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("锁定期间正确口令也应被拒, 得到 %d (%s)", res.StatusCode, raw)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	if res, _, _ := e.do(http.MethodGet, "/api/v1/auth/me", nil); res.StatusCode != http.StatusOK {
		t.Fatal("登录后 me 应 200")
	}
	if res, _, raw := e.write(http.MethodPost, "/api/v1/auth/logout", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("登出失败 %d: %s", res.StatusCode, raw)
	}
	if res, _, _ := e.do(http.MethodGet, "/api/v1/auth/me", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后应 401, 得到 %d", res.StatusCode)
	}
}

func TestSetupCannotRunTwice(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	res, raw := e.call(http.MethodPost, "/api/v1/setup", map[string]string{
		"username": "other", "password": "otherpass123"}, false)
	var env envelope
	decodeRaw(t, raw, &env)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("重复初始化应 409, 得到 %d (%s)", res.StatusCode, raw)
	}
	if env.Error == nil || env.Error.Code != "VALIDATION_ALREADY_SETUP" {
		t.Fatalf("错误码 = %+v", env.Error)
	}
}

func TestSetupStatusReportsFirstRun(t *testing.T) {
	e := newEnv(t)
	res, env, _ := e.do(http.MethodGet, "/api/v1/setup/status", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态 %d", res.StatusCode)
	}
	var st struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	decodeInto(t, env.Data, &st)
	if !st.NeedsSetup {
		t.Fatal("空库应需要初始化")
	}
	e.setupAdmin()
	_, env, _ = e.do(http.MethodGet, "/api/v1/setup/status", nil)
	decodeInto(t, env.Data, &st)
	if st.NeedsSetup {
		t.Fatal("已初始化后不应再要求初始化")
	}
}

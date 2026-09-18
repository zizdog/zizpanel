package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ============================================================================
//  phpMyAdmin 反代的 Cookie 路径改写（2026-09-18 用户报障）
//
//  症状：本机 phpMyAdmin 登录不上（"Failed to set session cookie. Maybe you are
//  using HTTP instead of HTTPS to access phpMyAdmin."），而且"选好文件一点导入就 500"。
//
//  根因：phpMyAdmin 按自己的路径写 Cookie —— `path=/phpmyadmin/`；而浏览器访问的是
//  面板入口 `/<安全后缀>/phpmyadmin/`。路径对不上 → 浏览器不把 Cookie 带回来 →
//  每个请求都是新会话：登录没有会话、导入 POST 没有会话/令牌 → 500。
//
//  这类 bug 只有"看真实响应头"才抓得到，所以这里用一个假上游把 Set-Cookie 打回来，
//  断言反代真的把 path 补上了面板前缀。
// ============================================================================

func TestPhpMyAdminProxyRewritesCookiePath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name: "__Secure-phpMyAdmin_https", Value: "abc123",
			Path: "/phpmyadmin/", Secure: true, HttpOnly: true,
		})
		http.SetCookie(w, &http.Cookie{Name: "other", Value: "1", Path: "/"})
		w.Header().Set("Location", "/phpmyadmin/index.php?route=/")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, "redirect")
	}))
	defer upstream.Close()

	srv, _ := newTestServer(t)
	srv.Cfg.PanelSuffix = "jab5c63"
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := srv.phpAdminProxyTo(u)

	req := httptest.NewRequest("GET", "https://127.0.0.1:8443/jab5c63/phpmyadmin/", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	cookies := rec.Result().Cookies()
	var pma *http.Cookie
	for _, c := range cookies {
		if c.Name == "__Secure-phpMyAdmin_https" {
			pma = c
		}
	}
	if pma == nil {
		t.Fatalf("响应里没有 phpMyAdmin 的会话 Cookie：%v", rec.Header())
	}
	if pma.Path != "/jab5c63/phpmyadmin/" {
		t.Errorf("Cookie 路径必须改写到面板入口之下（否则浏览器不带回它 → 登录不上/导入 500），实际 %q", pma.Path)
	}
	if !pma.Secure || !pma.HttpOnly {
		t.Errorf("其余属性必须原样保留：%+v", pma)
	}
	// 与面板入口无关的 Cookie 不许被动（改错会让本来正常的 Cookie 失效）。
	for _, c := range cookies {
		if c.Name == "other" && c.Path != "/" {
			t.Errorf(`path=/ 的 Cookie 不该被改写，实际 %q`, c.Path)
		}
	}
	if loc := rec.Header().Get("Location"); loc != "/jab5c63/phpmyadmin/index.php?route=/" {
		t.Errorf("重定向也要带上面板前缀，实际 %q", loc)
	}
}

func TestPhpMyAdminProxyWithoutSuffixKeepsPath(t *testing.T) {
	// 没启用安全后缀时前缀为空：Cookie 路径保持 /phpmyadmin/ 不变。
	srv, _ := newTestServer(t)
	srv.Cfg.PanelSuffix = ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "pma", Value: "1", Path: "/phpmyadmin/"})
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	rec := httptest.NewRecorder()
	srv.phpAdminProxyTo(u).ServeHTTP(rec, httptest.NewRequest("GET", "https://x/phpmyadmin/", nil))
	if got := rec.Result().Cookies()[0].Path; got != "/phpmyadmin/" {
		t.Errorf("没有安全后缀时不该改路径，实际 %q", got)
	}
}

func TestRewriteCookiePathOnlyTouchesMatchingPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a=1; path=/phpmyadmin/; secure", "a=1; path=/jab5c63/phpmyadmin/; secure"},
		{"a=1; Path=/phpmyadmin; HttpOnly", "a=1; Path=/jab5c63/phpmyadmin; HttpOnly"},
		{"a=1; path=/phpmyadmin2/", "a=1; path=/phpmyadmin2/"},
		{"a=1; path=/", "a=1; path=/"},
		{"a=1", "a=1"},
		{"a=1; path=/phpmyadmin/sub/dir", "a=1; path=/jab5c63/phpmyadmin/sub/dir"},
	}
	for _, c := range cases {
		if got := rewriteCookiePath(c.in, "/phpmyadmin", "/jab5c63/phpmyadmin"); got != c.want {
			t.Errorf("rewriteCookiePath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	if !strings.Contains(rewriteCookiePath("a=1; path=/phpmyadmin/", "/phpmyadmin", "/x/phpmyadmin"), "/x/phpmyadmin") {
		t.Error("前缀改写必须真的生效")
	}
}

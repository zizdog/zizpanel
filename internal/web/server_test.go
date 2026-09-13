package web

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/auth"
	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/store"
	"github.com/zizdog/zizpanel/internal/sysinfo"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.LogDir = dir + "/logs"
	cfg.RunDir = dir + "/run"
	cfg.WorkDir = dir + "/work"
	cfg.BinDir = dir + "/bin"
	cfg.TLSCert = dir + "/tls/panel.crt"
	cfg.TLSKey = dir + "/tls/panel.key"
	cfg.TLSEnable = false
	cfg.Secret = strings.Repeat("a", 64)
	cfg.AccessMode = "any"
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	am := auth.New(st, cfg.Secret, 72, 5, 15)
	srv, err := New(cfg, st, am, sysinfo.NewCollector(dir))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts
}

// doJSON 发起 JSON 请求。默认自动带上 cookie 与 CSRF 头（模拟正常前端行为）。
// 传 skipCSRF=true 可刻意不带 CSRF 头，用于验证防护是否生效。
func doJSONOpt(t *testing.T, ts *httptest.Server, method, path string, body any, cookies []*http.Cookie, skipCSRF bool) (*http.Response, map[string]any, []*http.Cookie) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, ts.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
		if c.Name == "zp_csrf" && !skipCSRF {
			req.Header.Set("X-CSRF-Token", c.Value)
		}
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out, res.Cookies()
}

func doJSON(t *testing.T, ts *httptest.Server, method, path string, body any, cookies []*http.Cookie) (*http.Response, map[string]any, []*http.Cookie) {
	t.Helper()
	return doJSONOpt(t, ts, method, path, body, cookies, false)
}

func TestFullSetupLoginAndAuthorizedRequest(t *testing.T) {
	_, ts := newTestServer(t)

	// 1) 未初始化
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/setup/status", nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("setup/status 状态码 %d", res.StatusCode)
	}
	data := out["data"].(map[string]any)
	if data["needs_setup"] != true {
		t.Fatal("初始状态应为 needs_setup=true")
	}

	// 2) 未登录访问受保护接口必须 401
	res, _, _ = doJSON(t, ts, "GET", "/api/v1/session", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录访问应 401，实际 %d", res.StatusCode)
	}

	// 3) 弱密码初始化必须被拒
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/setup", map[string]string{"username": "admin", "password": "123"}, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("弱密码应 400，实际 %d", res.StatusCode)
	}

	// 4) 正常初始化
	res, out, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("初始化失败 %d: %v", res.StatusCode, out)
	}
	if len(cookies) == 0 {
		t.Fatal("初始化后应下发会话 Cookie")
	}

	// 5) 带 Cookie 访问受保护接口
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/session", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("已登录访问应 200，实际 %d: %v", res.StatusCode, out)
	}
	sdata := out["data"].(map[string]any)
	user := sdata["user"].(map[string]any)
	if user["username"] != "admin" {
		t.Fatalf("会话账号错误: %v", user["username"])
	}

	// 6) 初始化后再次调用 setup 必须被拒绝（防止被用来重置密码）
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "hacker", "password": "Whatever123"}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("重复初始化应 403，实际 %d", res.StatusCode)
	}
}

func TestCSRFProtection(t *testing.T) {
	_, ts := newTestServer(t)
	res, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)
	if res.StatusCode != 200 {
		t.Fatal("初始化失败")
	}

	// 不带 CSRF 头写操作必须被拒绝（模拟跨站伪造请求：浏览器会自动带上 Cookie，
	// 但攻击者无法读取 Cookie 内容去构造自定义头）
	res, out, _ := doJSONOpt(t, ts, "POST", "/api/v1/settings",
		map[string]any{"access_mode": "any"}, cookies, true)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 头应 403，实际 %d: %v", res.StatusCode, out)
	}

	// 错误的 CSRF 头同样必须被拒绝
	badCookies := append([]*http.Cookie{}, cookies...)
	for i, c := range badCookies {
		if c.Name == "zp_csrf" {
			badCookies[i] = &http.Cookie{Name: c.Name, Value: strings.Repeat("0", 32)}
		}
	}
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/settings",
		map[string]any{"access_mode": "any"}, badCookies)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("错误 CSRF 头应 403，实际 %d", res.StatusCode)
	}

	// 带上正确的 CSRF 头应通过
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/settings",
		map[string]any{"session_hours": 48}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("带 CSRF 头应成功，实际 %d: %v", res.StatusCode, out)
	}
	if out["data"].(map[string]any)["session_hours"].(float64) != 48 {
		t.Fatal("设置未生效")
	}
}

func TestLoginWrongPasswordAndLogout(t *testing.T) {
	_, ts := newTestServer(t)
	doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/login",
		map[string]string{"username": "admin", "password": "wrong"}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", res.StatusCode)
	}
	if msg, _ := out["msg"].(string); msg == "" || strings.Contains(msg, "bcrypt") {
		t.Fatalf("错误提示不应泄漏实现细节: %v", msg)
	}

	// 正确登录并登出
	res, _, cookies := doJSON(t, ts, "POST", "/api/v1/login",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("登录失败 %d", res.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/logout", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("登出失败 %d", res.StatusCode)
	}
	// 登出后会话必须失效
	res, _, _ = doJSON(t, ts, "GET", "/api/v1/session", nil, cookies)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后应 401，实际 %d", res.StatusCode)
	}
}

func TestAccessControlBlocksForeignIP(t *testing.T) {
	srv, ts := newTestServer(t)
	doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)

	// 切到白名单模式，只允许 Tailscale 网段（不含本机回环之外的任何地址）
	srv.Cfg.AccessMode = "whitelist"
	srv.Cfg.IPWhitelist = []string{"100.64.0.0/10"}

	// httptest 客户端的来源是 127.0.0.1，属于回环，永远放行
	res, _, _ := doJSON(t, ts, "GET", "/api/v1/ping", nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("回环地址应始终放行，实际 %d", res.StatusCode)
	}

	// 用伪造 XFF 也不能绕过（TrustProxy 默认关闭）
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/ping", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.5")
	res2, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res2.Body.Close() }()
	if res2.StatusCode != 200 {
		t.Fatalf("来源是回环，应放行（XFF 不应影响判断），实际 %d", res2.StatusCode)
	}

	// 直接检查判定函数：非白名单地址必须被拒
	if srv.ipAllowed("8.8.8.8") {
		t.Fatal("白名单模式下 8.8.8.8 应被拒绝")
	}
	if !srv.ipAllowed("100.64.0.5") {
		t.Fatal("Tailscale 网段应被放行")
	}
	if !srv.ipAllowed("127.0.0.1") {
		t.Fatal("回环必须始终放行，否则会把自己锁在门外")
	}

	// local 模式
	srv.Cfg.AccessMode = "local"
	if srv.ipAllowed("192.168.1.10") {
		t.Fatal("local 模式下局域网地址应被拒绝")
	}
	if !srv.ipAllowed("::1") {
		t.Fatal("local 模式下 IPv6 回环应放行")
	}
}

func TestMatchCIDR(t *testing.T) {
	cases := []struct {
		ip, cidr string
		want     bool
	}{
		{"192.168.1.5", "192.168.1.0/24", true},
		{"192.168.2.5", "192.168.1.0/24", false},
		{"10.1.2.3", "10.0.0.0/8", true},
		{"100.101.102.103", "100.64.0.0/10", true},
		{"101.0.0.1", "100.64.0.0/10", false},
		{"1.2.3.4", "1.2.3.4", true},
		{"1.2.3.5", "1.2.3.4", false},
		{"1.2.3.4", "", false},
		{"1.2.3.4", "garbage", false},
	}
	for _, c := range cases {
		ip := parseIPForTest(c.ip)
		if got := matchCIDR(ip, c.cidr); got != c.want {
			t.Fatalf("matchCIDR(%s, %s) = %v，期望 %v", c.ip, c.cidr, got, c.want)
		}
	}
}

func TestStaticIndexServed(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Fatalf("首页应返回 200，实际 %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("首页 Content-Type 错误: %s", ct)
	}
	// SPA 回落：未知路径也要返回首页
	res2, _ := ts.Client().Get(ts.URL + "/sites")
	defer func() { _ = res2.Body.Close() }()
	if res2.StatusCode != 200 {
		t.Fatalf("前端路由应回落首页，实际 %d", res2.StatusCode)
	}
	// 内置 JS 必须可访问，否则面板白屏
	res3, _ := ts.Client().Get(ts.URL + "/js/app.js")
	defer func() { _ = res3.Body.Close() }()
	if res3.StatusCode != 200 {
		t.Fatalf("JS 资源应可访问，实际 %d", res3.StatusCode)
	}
	if ct := res3.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("JS Content-Type 错误: %s", ct)
	}
}

func parseIPForTest(s string) net.IP { return net.ParseIP(s) }

// 服务管理路由的行为测试。
//
// 重点验证路径参数与更具体的路由不会互相抢占：
// Go 1.22 的 ServeMux 支持 {name} 通配，但 /services/{name}/{action}
// 与 /services/{name}/logs/stream 这类模式必须都能正确匹配。
func TestServiceRoutesAreRegistered(t *testing.T) {
	_, ts := newTestServer(t)
	res, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)
	if res.StatusCode != 200 {
		t.Fatal("初始化失败")
	}

	cases := []struct {
		method string
		path   string
		want   []int // 可接受的状态码
		why    string
	}{
		{"GET", "/api/v1/services", []int{200}, "服务列表应可访问"},
		{"GET", "/api/v1/services/nope", []int{404}, "不存在的服务应 404（而不是 405/501）"},
		{"POST", "/api/v1/services/nope/start", []int{404, 400, 500}, "动作路由应被识别（服务不存在则报错）"},
		{"GET", "/api/v1/services/nope/logs", []int{200, 404}, "日志路由应被识别"},
		{"GET", "/api/v1/market", []int{200}, "应用市场列表应可访问"},
		{"GET", "/api/v1/market/ollama/preflight", []int{200}, "预检查应可访问"},
		{"GET", "/api/v1/market/nonexistent/preflight", []int{404}, "不存在的应用应 404"},
		{"GET", "/api/v1/adoptable", []int{200}, "纳管扫描应可访问"},
		{"GET", "/api/v1/ports/free", []int{200}, "端口推荐应可访问"},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, c.method, c.path, nil, cookies)
		allowed := false
		for _, w := range c.want {
			if res.StatusCode == w {
				allowed = true
			}
		}
		if !allowed {
			t.Fatalf("%s %s 状态码 %d（期望 %v）: %v — %s",
				c.method, c.path, res.StatusCode, c.want, out, c.why)
		}
	}
}

// 登录 Cookie 的 Secure 标志必须按"实际访问协议"决定。
//
// 这条测试锁死一个真实生产问题：面板监听 HTTPS，但通过
// nginx 的 http://localhost/_panel 访问时，如果 Cookie 带 Secure，
// 浏览器会直接丢弃它，表现为"密码正确但登录后立刻回到登录页"。
func TestSessionCookieSecureFlagFollowsRequestScheme(t *testing.T) {
	_, ts := newTestServer(t)
	doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)

	cases := []struct {
		name       string
		proto      string
		wantSecure bool
	}{
		{"HTTP 反代（X-Forwarded-Proto: http）", "http", false},
		{"HTTPS 反代（X-Forwarded-Proto: https）", "https", true},
		{"HTTPS 直连（X-Forwarded-Proto: https）", "https", true},
	}
	for _, c := range cases {
		req, err := http.NewRequest("POST", ts.URL+"/api/v1/login",
			strings.NewReader(`{"username":"admin","password":"PanelTestPw-9x!"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.proto != "" {
			req.Header.Set("X-Forwarded-Proto", c.proto)
		}
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		var found bool
		for _, ck := range res.Cookies() {
			if ck.Name == "zp_session" {
				found = true
				if ck.Secure != c.wantSecure {
					t.Fatalf("%s：Secure 应为 %v，实际 %v", c.name, c.wantSecure, ck.Secure)
				}
			}
		}
		if !found {
			t.Fatalf("%s：未下发会话 Cookie", c.name)
		}
	}
}

// 计划任务路由必须在位，且字面量路径不能被 {id} 抢占。
//
// Go 1.22 的 ServeMux 会优先匹配更具体的模式，但这条规则值得测试锁死 ——
// 一旦失效，/cron/preview 会被当成 id="preview" 处理并返回 400，
// 而这种错误只在运行时暴露。
func TestCronRoutesAreRegistered(t *testing.T) {
	_, ts := newTestServer(t)
	res, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)
	if res.StatusCode != 200 {
		t.Fatal("初始化失败")
	}

	cases := []struct {
		method string
		path   string
		want   []int
	}{
		{"GET", "/api/v1/cron", []int{200}},
		{"GET", "/api/v1/cron/preview?schedule=0+3+*+*+*", []int{200}},
		{"GET", "/api/v1/cron/999", []int{404}},
		{"GET", "/api/v1/cron/abc", []int{400}},
		{"POST", "/api/v1/cron/sync", []int{200}},
		{"GET", "/api/v1/backups", []int{200}},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, c.method, c.path, nil, cookies)
		allowed := false
		for _, w := range c.want {
			if res.StatusCode == w {
				allowed = true
			}
		}
		if !allowed {
			t.Fatalf("%s %s 状态码 %d（期望 %v）: %v", c.method, c.path, res.StatusCode, c.want, out)
		}
	}

	// preview 必须返回解析结果而不是 ID 解析错误
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/cron/preview?schedule=0+3+*+*+*", nil, cookies)
	if res.StatusCode == 200 {
		data, _ := out["data"].(map[string]any)
		if data["valid"] != true {
			t.Fatalf("preview 应判定表达式合法: %v", data)
		}
		if d, _ := data["describe"].(string); d == "" {
			t.Fatal("preview 应返回中文描述")
		}
	}
}

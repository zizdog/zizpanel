package web

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/auth"
	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/store"
	"github.com/zizdog/zizpanel/internal/sysinfo"
)

// shortTempDir 造一个**足够短**的临时目录。
//
// 为什么不用 t.TempDir()：macOS 上它形如
// /var/folders/jd/<40 个随机字符>/T/TestXxx/001，前缀就有 60+ 字符。
// 而 PHP 多版本的沙箱 brew 前缀下会绑定 Unix domain socket（sun_path 上限 104），
// 真机上 /opt/homebrew/var/run/php-fpm-8.3.sock 只有 41 字符，
// 用 t.TempDir() 会让测试先撞上"路径过长"而不是被测逻辑。
// 仍然是隔离的临时目录，测试结束即删除。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zpt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := shortTempDir(t)
	cfg := config.Default()
	// ⚠️ 用户的真实家目录必须被隔离。config.Default() 会用 os.UserHomeDir()
	// 解析出**真实**家目录，而市场/服务等处理器会往 Cfg.UserHome 下探测甚至写文件：
	// 曾有一次集成测试把用户真实的 ~/Library/LaunchAgents/sh.brew.*.plist
	// 覆盖成了 <plist/>（服务当时还在跑所以没立刻发现，重启后 PHP/MySQL 就起不来了）。
	// 所有测试用的根目录一律指向 t.TempDir()。
	cfg.UserHome = dir + "/home"
	cfg.WWWRoot = cfg.UserHome + "/www"
	cfg.LogRoot = cfg.WWWRoot + "/_logs"
	cfg.DataDir = dir
	cfg.LogDir = dir + "/logs"
	cfg.RunDir = dir + "/run"
	cfg.WorkDir = dir + "/work"
	cfg.BinDir = dir + "/bin"
	// Homebrew 前缀也必须沙箱化。
	//
	// 为什么：这一批新接口（PHP 多版本）会读**并写**
	// /opt/homebrew/etc/php/<版本>/php-fpm.d/www.conf。如果测试里前缀还是真机的，
	// 一次 `go test ./internal/web/` 就会改掉用户真实的 php-fpm 配置 ——
	// 性质等同于 2026-09-14 那次"测试覆盖真实 LaunchAgent plist"的事故。
	// detectBrewPrefix 是变量（见 config 包注释），这里把它钉到临时目录；
	// 目录里放一个 bin/brew 只是为了让 ReconcilePaths 认定"这个前缀是有效的"，
	// 从而**不去**把它改回真机的 /opt/homebrew。
	cfg.BrewPrefix = dir + "/brew"
	cfg.BrewBin = cfg.BrewPrefix + "/bin/brew"
	prevDetectBrew := config.SetBrewPrefixDetectorForTest(func() string { return cfg.BrewPrefix })
	t.Cleanup(func() { config.SetBrewPrefixDetectorForTest(prevDetectBrew) })
	_ = os.MkdirAll(cfg.BrewPrefix+"/bin", 0o755)
	_ = os.WriteFile(cfg.BrewPrefix+"/bin/brew", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	cfg.TLSCert = dir + "/tls/panel.crt"
	cfg.TLSKey = dir + "/tls/panel.key"
	cfg.TLSEnable = false
	cfg.Secret = strings.Repeat("a", 64)
	// 系统级 LaunchDaemons 目录也必须沙箱化：adoptTargetExists 会 stat 它，
	// 而真机上装了 frpc / orbien-client 的 plist 会让"市场残留态"相关测试
	// 假失败（2026-09-16 实际发生）。单测不该依赖真机装了什么。
	prevLaunchDaemonsDir := launchDaemonsDir
	launchDaemonsDir = dir + "/LaunchDaemons"
	if err := os.MkdirAll(launchDaemonsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { launchDaemonsDir = prevLaunchDaemonsDir })
	cfg.AccessMode = "any"
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// 家目录/网站根目录不在 EnsureDirs 的清单里，显式建出来
	for _, d := range []string{cfg.UserHome, cfg.WWWRoot, cfg.LogRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// 反代目标网段判定用的 DNS 解析器必须钉住：否则带域名目标的测试会去查
	// 真实 DNS（单测不许碰真实网络）。这里统一返回一个公网地址 —— 对测试来说
	// 等价于"不需要经面板转发"，因此测试不会意外绑上 47000+ 的回环端口。
	prevLookup := proxyLookupHostFn
	proxyLookupHostFn = func(string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	t.Cleanup(func() { proxyLookupHostFn = prevLookup })
	am := auth.New(st, cfg.Secret, 72, 5, 15)
	srv, err := New(cfg, st, am, sysinfo.NewCollector(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.forwarders.StopAll)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts
}

// TestTestServerSandboxedAwayFromRealHome 是本项目最贵的一次事故的护栏。
//
// 2026-09-14：api_market_test.go 用 srv.Cfg.UserHome 造 plist 文件来复刻"本机实况"，
// 而 newTestServer 当时只隔离了 DataDir 等目录、**没隔离 UserHome**，
// 于是 `make check` 把用户真实的
//
//	~/Library/LaunchAgents/sh.brew.php@8.3.plist
//	~/Library/LaunchAgents/sh.brew.mysql@8.4.plist
//
// 覆盖成了 8 字节的 `<plist/>`。当时服务还在跑所以毫无症状，
// 但只要重启（或面板点一次"重启服务"）PHP/MySQL 就再也起不来了。
//
// 所以这里把"测试服务器的用户可见根目录必须落在临时目录里"钉死：
// 谁再把 Cfg.UserHome 指回真实家目录，这个测试先红。
func TestTestServerSandboxedAwayFromRealHome(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil || realHome == "" {
		t.Skip("拿不到真实家目录，跳过")
	}
	srv, _ := newTestServer(t)

	if srv.Cfg.UserHome == realHome {
		t.Fatalf("测试服务器的 UserHome 指向了真实家目录 %s：测试会写到用户的真实文件上", realHome)
	}
	for name, got := range map[string]string{
		"UserHome": srv.Cfg.UserHome,
		"WWWRoot":  srv.Cfg.WWWRoot,
		"LogRoot":  srv.Cfg.LogRoot,
	} {
		if got == "" {
			t.Errorf("%s 不该为空", name)
			continue
		}
		if got == realHome || strings.HasPrefix(got, realHome+"/") {
			t.Errorf("%s = %s 落在真实家目录里，测试会污染用户环境", name, got)
		}
		if !strings.HasPrefix(got, os.TempDir()) && !strings.Contains(got, "TestTestServerSandboxedAwayFromRealHome") {
			// t.TempDir() 在 macOS 上常是 /var/folders/...（不是 os.TempDir()），
			// 所以这里只做"不在真实家目录"的硬性判断，其余仅提示。
			t.Logf("提示：%s = %s（不在 os.TempDir() 下，确认它确实是测试临时目录）", name, got)
		}
	}
}

// TestTestServerSandboxedAwayFromRealHomebrew 是 Homebrew 前缀版的同一条护栏。
//
// 为什么需要：PHP 多版本功能会读写 <brew>/etc/php/<版本>/php-fpm.d/www.conf
// 与 <brew>/opt/php*。测试服务器若还用真机前缀，`go test` 会直接改用户
// 真实的 php-fpm 配置（== 2026-09-14 覆盖真实 plist 那类事故的翻版）。
//
// 这里断言两件事：
//  1. 测试服务器的 BrewPrefix 不在 /opt/homebrew、/usr/local 里；
//  2. config 包的前缀探测在测试期间返回的也是沙箱路径
//     —— 否则 ReconcilePaths 会把前缀改回真机（它就是这么写的）。
func TestTestServerSandboxedAwayFromRealHomebrew(t *testing.T) {
	srv, _ := newTestServer(t)
	got := srv.Cfg.BrewPrefix
	if got == "" {
		t.Fatal("测试服务器的 BrewPrefix 不该为空")
	}
	for _, real := range []string{"/opt/homebrew", "/usr/local"} {
		if got == real || strings.HasPrefix(got, real+"/") {
			t.Fatalf("BrewPrefix = %s 指向真实 Homebrew：测试会写到用户真实的 php-fpm/nginx 配置上", got)
		}
	}
	// 关键：ReconcilePaths（svcManager 每次都会调）不能把沙箱前缀改回真机。
	// 直接调一次并断言前缀原样不动 —— 这比"读一下探测器"更贴近真实故障路径。
	srv.Cfg.ReconcilePaths()
	if srv.Cfg.BrewPrefix != got {
		t.Fatalf("ReconcilePaths 把 BrewPrefix 从 %s 改成了 %s："+
			"沙箱失效，测试会落到真实 Homebrew 上", got, srv.Cfg.BrewPrefix)
	}
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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

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
			strings.NewReader(`{"username":"admin","password":"zizpanel-test-fixture-pass"}`))
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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
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

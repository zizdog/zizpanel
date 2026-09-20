package web

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  反向代理「写入 → chown → reload → 请求级复核」通道的单测
//
//  背景（真机踩过）：写 vhost 时提权助手会以 **root** 跑 `nginx -t`，而
//  `nginx -t` 会把 access_log/error_log 创建成 root 属主；随后以真实用户运行的
//  nginx master 打不开它们 → reload **[emerg] 失败但退出码仍是 0** →
//  配置根本没加载，面板却报成功。phpMyAdmin 已经因此假"已可用"过一次。
//
//  所以这里锁死三件事：
//    1. chown 必须在 reload **之前**发生（顺序不能反）；
//    2. reload 之后必须有**请求级复核**（靠该规则自己的 access_log 增长，
//       以及响应内容/状态码），不是"文件写进去了"或"退出码是 0"；
//    3. 复核失败必须**返回错误**（调用方据此返回非 2xx），不能只记日志。
//
//  所有测试都跑在 newTestServer 的沙箱里：不碰真实 nginx、不真的 reload。
// ============================================================================

// proxyTestHooks 把复核相关的可注入步骤换成测试替身，并在测试结束恢复。
type proxyTestHooks struct {
	Order []string // write / chown / reload / probe 的调用顺序
}

func stubProxyHooks(t *testing.T) *proxyTestHooks {
	t.Helper()
	h := &proxyTestHooks{}
	prevProbe, prevReload, prevChown := proxyProbeFn, proxyReloadFn, proxyChownLogsFn
	prevWrite, prevWait, prevEvery, prevSettle := proxyWriteVhostFn, proxyVerifyWait, proxyVerifyEvery, proxyLogSettle
	// 失败后的健康探测/恢复动作也必须可注入：否则会真的去 reload 真机 nginx。
	prevHealthProbe, prevHealthReload, prevHealthRestart := nginxHealthProbeFn, nginxHealthReloadFn, nginxHealthRestartFn
	prevRootOwned := nginxRootOwnedFilesFn
	t.Cleanup(func() {
		proxyProbeFn, proxyReloadFn, proxyChownLogsFn = prevProbe, prevReload, prevChown
		proxyWriteVhostFn, proxyVerifyWait, proxyVerifyEvery, proxyLogSettle = prevWrite, prevWait, prevEvery, prevSettle
		nginxHealthProbeFn, nginxHealthReloadFn, nginxHealthRestartFn = prevHealthProbe, prevHealthReload, prevHealthRestart
		nginxRootOwnedFilesFn = prevRootOwned
	})
	// 默认：探测认为 nginx 在应答（分到"配置已写入但未生效"），且不发现 root 属主文件。
	nginxHealthProbeFn = func(context.Context, string, int) (string, error) {
		h.Order = append(h.Order, "health-probe")
		return "200", nil
	}
	nginxHealthReloadFn = func(*Server, context.Context) error {
		h.Order = append(h.Order, "health-reload")
		return nil
	}
	nginxHealthRestartFn = func(*Server, context.Context) error {
		h.Order = append(h.Order, "health-restart")
		return nil
	}
	nginxRootOwnedFilesFn = func(*Server) []string { return nil }

	proxyWriteVhostFn = func(*Server, context.Context, string, string) error {
		h.Order = append(h.Order, "write")
		return nil
	}
	proxyChownLogsFn = func(*Server) { h.Order = append(h.Order, "chown") }
	proxyReloadFn = func(*Server, context.Context) error {
		h.Order = append(h.Order, "reload")
		return nil
	}
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		h.Order = append(h.Order, "probe")
		return "200", "upstream-ok", nil
	}
	// 复核窗口压到毫秒级，否则每个用例要等好几秒。
	proxyVerifyWait = 120 * time.Millisecond
	proxyVerifyEvery = time.Millisecond
	proxyLogSettle = 0
	return h
}

func testProxyRule(id int64, listen int, domains string) *proxies.Rule {
	return &proxies.Rule{
		ID:      id,
		Name:    fmt.Sprintf("规则%d", id),
		Listen:  listen,
		Domains: domains,
		Target:  "http://127.0.0.1:9",
		Enabled: true,
	}
}

// newProxyTestServer 在 newTestServer 的基础上把 **VhostDir** 也指进沙箱。
//
// newTestServer 只隔离了 BrewPrefix/BrewBin，而 config.Default() 解析出来的
// VhostDir 仍然是真机的 /opt/homebrew/etc/nginx/vhosts。任何会写/删/列 vhost
// 的测试都必须先把它改掉，否则一次 `go test` 就会往用户的生产目录里写文件 ——
// 2026-09-14 正是因为这种漏沙箱化把生产的 000-default.conf 改坏、面板 502。
func newProxyTestServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newProxyTestServerTS(t)
	return srv
}

// newProxyTestServerTS 与 newProxyTestServer 相同，但额外返回 httptest 服务器，
// 供需要走真实 HTTP 路由（鉴权 / CSRF / 路径参数）的接口测试使用。
func newProxyTestServerTS(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	srv, ts := newTestServer(t)
	dir := filepath.Join(srv.Cfg.BrewPrefix, "etc", "nginx", "vhosts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, real := range []string{"/opt/homebrew", "/usr/local"} {
		if dir == real || strings.HasPrefix(dir, real+"/") {
			t.Fatalf("无法把测试的 VhostDir 隔离出真实 nginx：%s", dir)
		}
	}
	srv.Cfg.VhostDir = dir
	return srv, ts
}

// appendToFile 模拟"这条规则自己的 nginx 访问日志长出了一行"。
func appendToFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("127.0.0.1 - - [probe] \"GET / HTTP/1.1\" 200\n")
	_ = f.Close()
}

// TestProxyProbeServedDecisions 锁死"响应算不算被 nginx 处理了"的判据。
//
// 关键反例：nginx 的 404/502 错误页**也有内容**，所以绝不能"拿到 2xx 才算通"
// 或"body 非空就算通"；而默认站点的占位页返回 200，正是最容易漏过去的假成功。
func TestProxyProbeServedDecisions(t *testing.T) {
	placeholder := "<p>" + sites.LocalhostIndexMarker + "</p>"
	cases := []struct {
		name   string
		p      proxyProbe
		listen int
		want   bool
	}{
		{"上游正常响应", proxyProbe{code: "200", body: "<html>app</html>"}, 8080, true},
		{"上游 404 也是响应（规则已加载）", proxyProbe{code: "404", body: "not found"}, 8080, true},
		{"nginx 502：规则已加载、只是上游没起来", proxyProbe{code: "502", body: "<title>502 Bad Gateway</title>"}, 8080, true},
		{"连不上（端口上没人监听）", proxyProbe{code: "000"}, 8080, false},
		{"被兜底块 444 断开（空响应）", proxyProbe{code: "000", err: errors.New("Empty reply from server")}, 8080, false},
		{"没有状态码", proxyProbe{code: ""}, 8080, false},
		{"80 端口落到默认站点占位页 = 规则没加载", proxyProbe{code: "200", body: placeholder}, 80, false},
		{"别的端口反代到本机默认站点是合法配置", proxyProbe{code: "200", body: placeholder}, 8080, true},
	}
	for _, c := range cases {
		if got := proxyProbeServed(c.p, c.listen); got != c.want {
			t.Errorf("%s：proxyProbeServed = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestProxyProbeHostPicksAMatchingHost：探测用的 Host 必须能命中该规则，
// 否则请求会被兜底拒绝块接住，把"配置正常"误判成失败。
func TestProxyProbeHostPicksAMatchingHost(t *testing.T) {
	cases := []struct {
		domains string
		want    string
	}{
		{"", "_"}, // Generate 写的 server_name 就是 `_`，必须发同一个 Host
		{"lede.zizdog.com", "lede.zizdog.com"},
		{"a.com, b.com", "a.com"},
		{"*.zizdog.com", "zp-probe.zizdog.com"},
		{"*, a.com", "a.com"},
		{"*", "_"},
		{" A.COM ", "a.com"},
	}
	for _, c := range cases {
		if got := proxyProbeHost(testProxyRule(1, 8080, c.domains)); got != c.want {
			t.Errorf("domains=%q：proxyProbeHost = %q，期望 %q", c.domains, got, c.want)
		}
	}
}

// TestProxyAccessLogPathMatchesGeneratedVhost：复核靠"这条规则自己的 access_log
// 有没有增长"判断，所以路径必须与 internal/proxies 生成的一致 —— 一旦命名走样，
// 复核会永远看不到增长，把所有正常规则都判成失败。
func TestProxyAccessLogPathMatchesGeneratedVhost(t *testing.T) {
	const dir = "/tmp/zp-proxy-logdir"
	rule := testProxyRule(42, 18080, "")
	content, err := rule.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "access_log "+dir+"/proxy-42.access.log;") {
		t.Errorf("vhost 里的 access_log 与复核路径不一致：\n%s", content)
	}
	if got := proxyAccessLogPath(dir, 42); got != filepath.Join(dir, "proxy-42.access.log") {
		t.Errorf("proxyAccessLogPath = %s", got)
	}
}

// TestApplyProxyChownsBeforeReloadAndVerifies：完整顺序必须是
// 写盘 → chown → reload → 探测；chown 在 reload 之后就等于没做。
func TestApplyProxyChownsBeforeReloadAndVerifies(t *testing.T) {
	srv := newProxyTestServer(t)
	h := stubProxyHooks(t)
	rule := testProxyRule(1, 18080, "")
	logPath := proxyAccessLogPath(srv.proxyLogDir(), rule.ID)
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		h.Order = append(h.Order, "probe")
		appendToFile(t, logPath) // 该规则自己的访问日志长出新内容
		return "200", "upstream-ok", nil
	}

	if err := srv.applyProxy(context.Background(), rule); err != nil {
		t.Fatalf("applyProxy 应当成功，实际：%v", err)
	}
	want := []string{"write", "chown", "reload", "probe"}
	if !reflect.DeepEqual(h.Order, want) {
		t.Fatalf("调用顺序 = %v，期望 %v（chown 必须在 reload 之前）", h.Order, want)
	}
}

// TestApplyProxyFailsWhenConfigNotLoaded：reload 退出码 0、但配置没生效时，
// 复核必须失败并给出可排查的原因 —— 这是本族缺陷的核心。
func TestApplyProxyFailsWhenConfigNotLoaded(t *testing.T) {
	srv := newProxyTestServer(t)
	h := stubProxyHooks(t)
	rule := testProxyRule(2, 18081, "")
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		h.Order = append(h.Order, "probe")
		return "000", "", errors.New("Connection refused")
	}

	err := srv.applyProxy(context.Background(), rule)
	if err == nil {
		t.Fatal("配置没生效时必须返回错误，不能只记日志")
	}
	msg := err.Error()
	// 第一行必须是分类结论（≤40 字），细节在后。
	first := strings.SplitN(msg, "\n", 2)[0]
	if !strings.Contains(first, "nginx 无响应") || len([]rune(first)) > 40 {
		t.Errorf("第一行应当是 ≤40 字的分类结论，实际：%q", first)
	}
	for _, want := range []string{"没有生效", "访问日志"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息里应说明 %q，实际：%v", want, msg)
		}
	}
	// nginx 无响应必须真的恢复并复核（健康探测返回 200 → reload 后恢复）。
	if !strings.Contains(strings.Join(h.Order, ","), "health-reload") {
		t.Errorf("nginx 无响应时必须真的尝试恢复，实际调用顺序 = %v", h.Order)
	}
	// 没有发现 root 属主文件时，不得出现无证据的猜测。
	if strings.Contains(msg, "属主是 root") || strings.Contains(msg, "日志属主") {
		t.Errorf("未发现 root 属主文件时不得出现该提示，实际：%s", msg)
	}
	// chown 仍然必须在 reload 之前（即使复核失败）。
	if len(h.Order) < 2 || h.Order[0] != "write" || h.Order[1] != "chown" {
		t.Errorf("调用顺序 = %v，期望以 write → chown 开头", h.Order)
	}
}

// TestReloadProxyAndVerifyRejectsDefaultSitePage：80 端口上，请求落到默认站点
// 的占位页（200、有内容）绝不能被当成"规则生效"。
func TestReloadProxyAndVerifyRejectsDefaultSitePage(t *testing.T) {
	srv := newProxyTestServer(t)
	h := stubProxyHooks(t)
	rule := testProxyRule(3, 80, "")
	logPath := proxyAccessLogPath(srv.proxyLogDir(), rule.ID)
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		h.Order = append(h.Order, "probe")
		appendToFile(t, logPath) // 就算日志长了，内容判据也要拦住
		return "200", "<p>" + sites.LocalhostIndexMarker + "</p>", nil
	}

	err := srv.reloadProxyAndVerify(context.Background(), rule)
	if err == nil {
		t.Fatal("响应是默认站点占位页时必须判失败")
	}
	if !strings.Contains(err.Error(), "占位页") {
		t.Errorf("错误信息应指出落到了默认站点占位页，实际：%v", err)
	}
}

// TestReloadProxyAndVerifyPropagatesReloadError：reload 本身失败要原样上报；
// 失败后按有界探测分类（这里 nginx 仍在应答 → "配置已写入但未生效"），不做恢复动作。
func TestReloadProxyAndVerifyPropagatesReloadError(t *testing.T) {
	srv := newProxyTestServer(t)
	h := stubProxyHooks(t)
	proxyReloadFn = func(*Server, context.Context) error {
		h.Order = append(h.Order, "reload")
		return errors.New("reload boom")
	}
	rule := testProxyRule(4, 18082, "")

	err := srv.reloadProxyAndVerify(context.Background(), rule)
	if err == nil || !strings.Contains(err.Error(), "nginx 重载失败") {
		t.Fatalf("reload 失败时应返回明确错误，实际：%v", err)
	}
	if first := strings.SplitN(err.Error(), "\n", 2)[0]; first != "配置已写入但未生效" {
		t.Errorf("nginx 仍在应答时的结论应当是「配置已写入但未生效」，实际第一行：%q", first)
	}
	// reload 失败就不做请求级复核；失败后只探一次健康，且不触发恢复。
	for _, step := range h.Order {
		if step == "probe" || step == "health-reload" || step == "health-restart" {
			t.Errorf("不该出现 %s，实际调用顺序 = %v", step, h.Order)
		}
	}
}

// TestReloadProxyAndVerifyRetriesUntilServed：`nginx -s reload` 是异步的，
// 新 server 块生效有很短延迟；复核必须重试，不能第一次探测就误判失败。
func TestReloadProxyAndVerifyRetriesUntilServed(t *testing.T) {
	srv := newProxyTestServer(t)
	h := stubProxyHooks(t)
	rule := testProxyRule(5, 18083, "")
	logPath := proxyAccessLogPath(srv.proxyLogDir(), rule.ID)
	proxyVerifyWait = 2 * time.Second // 给足重试窗口
	probes := 0
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		h.Order = append(h.Order, "probe")
		probes++
		if probes < 3 {
			return "000", "", errors.New("Connection refused") // 还没生效
		}
		appendToFile(t, logPath)
		return "200", "upstream-ok", nil
	}

	if err := srv.reloadProxyAndVerify(context.Background(), rule); err != nil {
		t.Fatalf("重试后应当成功，实际：%v", err)
	}
	if probes < 3 {
		t.Fatalf("应当重试到第 3 次，实际只探测了 %d 次", probes)
	}
}

// TestWaitProxyGone：删除/停用后的复核看"这条规则自己的日志是否还在长"。
//
// 用日志而不是"端口有没有响应"，是因为同端口可能还有别的规则在应答。
func TestWaitProxyGone(t *testing.T) {
	t.Run("仍在生效要让删除失败", func(t *testing.T) {
		srv := newProxyTestServer(t)
		h := stubProxyHooks(t)
		rule := testProxyRule(6, 18084, "")
		logPath := proxyAccessLogPath(srv.proxyLogDir(), rule.ID)
		appendToFile(t, logPath) // 规则此前就在被 nginx 使用
		proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
			h.Order = append(h.Order, "probe")
			appendToFile(t, logPath) // 删除后请求仍然写进了它的日志 → 没卸载
			return "200", "still-here", nil
		}

		err := srv.reloadProxyAndVerifyGone(context.Background(), rule)
		if err == nil {
			t.Fatal("规则仍在生效时必须返回错误")
		}
		if !strings.Contains(err.Error(), "仍在生效") {
			t.Errorf("错误信息应说明规则仍在生效，实际：%v", err)
		}
	})

	t.Run("已卸载则通过", func(t *testing.T) {
		srv := newProxyTestServer(t)
		h := stubProxyHooks(t)
		rule := testProxyRule(7, 18085, "")
		logPath := proxyAccessLogPath(srv.proxyLogDir(), rule.ID)
		appendToFile(t, logPath) // 旧日志还在磁盘上，但不再增长
		proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
			h.Order = append(h.Order, "probe")
			return "000", "", errors.New("Connection refused")
		}

		if err := srv.reloadProxyAndVerifyGone(context.Background(), rule); err != nil {
			t.Fatalf("规则已卸载时不该报错，实际：%v", err)
		}
	})
}

// TestVerifyProxyDomainGuard：域名对不上的 Host 不能被转发到后端。
func TestVerifyProxyDomainGuard(t *testing.T) {
	t.Run("兜底块生效则通过", func(t *testing.T) {
		srv := newProxyTestServer(t)
		stubProxyHooks(t)
		rule := testProxyRule(8, 18086, "lede.zizdog.com")
		writeRejectVhost(t, srv, rule.Listen)
		proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
			return "000", "", errors.New("Empty reply from server") // 444
		}
		if err := srv.verifyProxyDomainGuard(context.Background(), rule); err != nil {
			t.Fatalf("兜底块生效时不该报错，实际：%v", err)
		}
	})

	t.Run("被误命中要报错", func(t *testing.T) {
		srv := newProxyTestServer(t)
		stubProxyHooks(t)
		rule := testProxyRule(9, 18087, "lede.zizdog.com")
		writeRejectVhost(t, srv, rule.Listen)
		proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
			return "200", "upstream-ok", nil // 域名对不上却拿到了后端响应
		}
		err := srv.verifyProxyDomainGuard(context.Background(), rule)
		if err == nil {
			t.Fatal("域名对不上却被转发时必须返回错误")
		}
		if !strings.Contains(err.Error(), "域名") {
			t.Errorf("错误信息应说明域名限制没生效，实际：%v", err)
		}
	})

	t.Run("通配规则跳过", func(t *testing.T) {
		srv := newProxyTestServer(t)
		stubProxyHooks(t)
		rule := testProxyRule(10, 18088, "")
		writeRejectVhost(t, srv, rule.Listen)
		proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
			return "200", "upstream-ok", nil
		}
		if err := srv.verifyProxyDomainGuard(context.Background(), rule); err != nil {
			t.Fatalf("通配规则不该被域名复核拦住，实际：%v", err)
		}
	})
}

func writeRejectVhost(t *testing.T, srv *Server, port int) {
	t.Helper()
	path := filepath.Join(srv.Cfg.VhostDir, proxies.RejectVhostName(port)+".conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("server { listen "+fmt.Sprint(port)+" default_server; return 444; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIsDuplicateDefaultServer：80 端口上 000-default 已经占了 default_server，
// 我们的兜底块写不进去 —— 这是**预期**情况，不能当成失败（否则 80 端口上所有
// 带域名的规则都创建不了）。
func TestIsDuplicateDefaultServer(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("写端口 80 的兜底拒绝块失败：配置语法错误，已回滚：\nnginx: [emerg] a duplicate default server for 0.0.0.0:80 in /x/proxy-reject-80.conf:4"), true},
		{errors.New("nginx: [emerg] DUPLICATE DEFAULT SERVER for [::]:80"), true},
		{errors.New("配置语法错误，已回滚：nginx: [emerg] unknown directive"), false},
	}
	for _, c := range cases {
		if got := isDuplicateDefaultServer(c.err); got != c.want {
			t.Errorf("isDuplicateDefaultServer(%v) = %v，期望 %v", c.err, got, c.want)
		}
	}
}

// TestAppProxyApplyReusesDefaultVhostChannel 锁死"不新建第三套 reload 实现"。
//
// 生成应用子路径入口时必须复用 applyDefaultVhost（它内部就是
// 写盘 → 日志树 chown → reload → 请求级复核），而不是自己 writeVhost +
// nginxReload（那条老路会在日志属主不对时静默失败却报成功）。
func TestAppProxyApplyReusesDefaultVhostChannel(t *testing.T) {
	src, err := os.ReadFile("api_appproxy.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	if !strings.Contains(code, "s.applyDefaultVhost(") {
		t.Error("handleAppProxyApply 必须复用 s.applyDefaultVhost（含 chown + 请求级复核）")
	}
	if !strings.Contains(code, "sites.EnsureLocalhostPlaceholder") {
		t.Error("复核判据是默认站点首页，所以必须先确保占位页存在")
	}
	for _, bad := range []string{"s.nginxReload(", "s.writeVhost("} {
		if strings.Contains(code, bad) {
			t.Errorf("api_appproxy.go 不该再自己调用 %s（会绕过复核，回到静默失败的老路）", bad)
		}
	}
}

// TestProxyVhostTestsSandboxedAwayFromProduction 是"测试不许碰生产 nginx 配置"
// 的护栏（AGENTS.md 硬性约定：曾因为漏沙箱化把用户的 000-default.conf 改坏，
// 整个面板 502）。
//
// 这里断言两件事：
//  1. 测试服务器的 VhostDir 不在真实 Homebrew 前缀下；
//  2. 跑一遍"会写 vhost"的路径后，生产的 vhost 目录**连文件名都没变**
//     （曾有过漏沙箱化时，测试直接往生产目录里塞 proxy-reject-*.conf）。
func TestProxyVhostTestsSandboxedAwayFromProduction(t *testing.T) {
	const prodVhost = "/opt/homebrew/etc/nginx/vhosts/000-default.conf"
	prodDir := filepath.Dir(prodVhost)
	before, beforeErr := os.ReadFile(prodVhost)
	beforeEntries := listDirNames(prodDir)

	srv := newProxyTestServer(t)
	for _, real := range []string{"/opt/homebrew", "/usr/local"} {
		if srv.Cfg.VhostDir == real || strings.HasPrefix(srv.Cfg.VhostDir, real+"/") {
			t.Fatalf("测试服务器的 VhostDir 指向真实 nginx：%s", srv.Cfg.VhostDir)
		}
	}

	stubProxyHooks(t)
	rule := testProxyRule(11, 18099, "probe.example.com")
	_ = srv.applyProxy(context.Background(), rule) // 走一遍会写 vhost 的路径
	_ = srv.syncRejectBlocks(context.Background())

	if afterEntries := listDirNames(prodDir); !reflect.DeepEqual(beforeEntries, afterEntries) {
		t.Fatalf("测试往生产 vhost 目录里加了/删了文件：%v → %v", beforeEntries, afterEntries)
	}
	after, afterErr := os.ReadFile(prodVhost)
	if beforeErr != nil || afterErr != nil {
		if os.IsNotExist(beforeErr) && os.IsNotExist(afterErr) {
			t.Skip("本机没有生产 000-default.conf，跳过内容比对")
		}
		t.Fatalf("读取生产 vhost 失败：before=%v after=%v", beforeErr, afterErr)
	}
	if string(before) != string(after) {
		t.Fatal("生产 000-default.conf 被测试改动了！这正是 2026-09-14 那次事故的形态")
	}
}

// listDirNames 列出目录里的文件名（排序后），用于"目录内容一字未变"的断言。
func listDirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

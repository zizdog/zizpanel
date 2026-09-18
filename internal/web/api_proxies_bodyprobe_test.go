package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  反代「大请求体探测」的单测
//
//  背景：端口在听、目标可达、健康检查全绿，**不等于**能收大请求体。
//  2026-09-18 事故：请求体先落盘到 client_body_temp，目录不可写时 nginx 在转给
//  上游之前直接回它自己的 500 页。这里的判据就是"拿到的不是 nginx 自己的 500/413"。
//
//  所有测试都不碰真实服务：发请求那一步（proxyBodyPostFn）与"端口在不在听"
//  （proxyProbeListeningFn）都是可注入点。
// ============================================================================

// nginxOwnErrorPage500 是 nginx 1.31.5 本机实测的出厂 500 页（原样抄下来）。
// 关键指纹是 `<center>nginx/<版本>`；老版本会写成 `<center>nginx</center>`，
// 两种都被 looksLikeNginxErrorPage 认出来。
const nginxOwnErrorPage500 = "<html>\r\n<head><title>500 Internal Server Error</title></head>\r\n<body>\r\n<center><h1>500 Internal Server Error</h1></center>\r\n<hr><center>nginx/1.31.5</center>\r\n</body>\r\n</html>\r\n"

// nginxOwnErrorPage413 是同一台机器上实测的 413 出厂页。
const nginxOwnErrorPage413 = "<html>\r\n<head><title>413 Request Entity Too Large</title></head>\r\n<body>\r\n<center><h1>413 Request Entity Too Large</h1></center>\r\n<hr><center>nginx/1.31.5</center>\r\n</body>\r\n</html>\r\n"

// stubProxyBodyProbe 替换探测的两个外部依赖，并在测试结束恢复。
func stubProxyBodyProbe(t *testing.T,
	listening bool,
	post func(ctx context.Context, scheme, host string, port int, path string, body []byte, timeout time.Duration) (string, string, error),
) *int {
	t.Helper()
	prevListen, prevPost := proxyProbeListeningFn, proxyBodyPostFn
	t.Cleanup(func() { proxyProbeListeningFn, proxyBodyPostFn = prevListen, prevPost })
	calls := 0
	proxyProbeListeningFn = func(context.Context, int) bool { return listening }
	proxyBodyPostFn = func(ctx context.Context, scheme, host string, port int, path string, body []byte, timeout time.Duration) (string, string, error) {
		calls++
		if post == nil {
			return "200", "upstream-ok", nil
		}
		return post(ctx, scheme, host, port, path, body, timeout)
	}
	return &calls
}

// TestClassifyBodyProbe：把"响应算好还是坏"的判据钉死。
//
// 最容易骗过人的是 nginx **自己**的 500 页 —— 它有内容、状态码也是 5xx，
// 但恰恰是"请求体根本没转发出去"，必须判失败；而上游应用自己的 5xx
// 说明请求体已经被转发了，不能误报成 nginx 的问题。
func TestClassifyBodyProbe(t *testing.T) {
	cases := []struct {
		name       string
		code       string
		body       string
		listen     int
		wantStatus string
		wantStep   string
	}{
		{"上游正常", "200", "<html>app</html>", 18080, "ok", ""},
		{"上游自己的 500（没有 nginx 出厂页指纹）", "500", "<h1>Internal Server Error</h1>", 18080, "ok", ""},
		{"nginx 自己的 500 页 → 判失败", "500", nginxOwnErrorPage500, 18080, "bad", "nginx 自身的错误页"},
		{"413 → 判失败", "413", nginxOwnErrorPage413, 18080, "bad", "nginx 请求体上限"},
		{"502 → 判失败", "502", "<hr><center>nginx/1.31.5</center>", 18080, "bad", "转发到上游"},
		{"504 → 判失败", "504", "", 18080, "bad", "转发到上游"},
		{"没有任何 HTTP 响应 → 未能探测", "000", "", 18080, "unknown", "发送请求"},
		{"80 端口落到默认站点占位页 → 未能探测", "200", "<p>" + sites.LocalhostIndexMarker + "</p>", 80, "unknown", "落到默认站点"},
	}
	for _, c := range cases {
		status, step, detail := classifyBodyProbe(c.code, c.body, c.listen)
		if status != c.wantStatus {
			t.Errorf("%s：status = %q，期望 %q（detail=%s）", c.name, status, c.wantStatus, detail)
		}
		if step != c.wantStep {
			t.Errorf("%s：step = %q，期望 %q", c.name, step, c.wantStep)
		}
		if status == "bad" && step == "" {
			t.Errorf("%s：判失败时必须写清是哪一步", c.name)
		}
	}
}

// TestLooksLikeNginxErrorPage：指纹判据既认带版本号的，也认老版本的写法；
// 不含指纹的应用错误页不能被误判。
func TestLooksLikeNginxErrorPage(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{nginxOwnErrorPage500, true},
		{nginxOwnErrorPage413, true},
		{"<hr><center>nginx</center>", true},
		{"<html><body>My App 500 error, nginx is our proxy</body></html>", false},
		{"nginx", false},
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikeNginxErrorPage(c.body); got != c.want {
			t.Errorf("looksLikeNginxErrorPage(%q) = %v，期望 %v", c.body, got, c.want)
		}
	}
}

// TestProbeProxyLargeBodyNginxOwn500IsFailure：探测端到端的核心用例 ——
// nginx 回它自己的 500 页时必须判失败，并写清是哪一步。
func TestProbeProxyLargeBodyNginxOwn500IsFailure(t *testing.T) {
	srv := newProxyTestServer(t)
	seedProxyNginx(t, srv)

	var gotScheme, gotHost, gotPath string
	var gotPort, gotBody int
	calls := stubProxyBodyProbe(t, true, func(_ context.Context, scheme, host string, port int, path string, body []byte, _ time.Duration) (string, string, error) {
		gotScheme, gotHost, gotPort, gotPath, gotBody = scheme, host, port, path, len(body)
		return "500", nginxOwnErrorPage500, nil
	})
	rule := &proxies.Rule{
		ID: 1, Name: "探测规则", Listen: 18080, Domains: "a.test", Path: "/api",
		Target: "http://127.0.0.1:9", Enabled: true,
	}

	res := srv.probeProxyLargeBody(context.Background(), rule)
	if *calls != 1 {
		t.Fatalf("应当只发一次探测请求，实际 %d 次", *calls)
	}
	if res.Status != "bad" || res.OK {
		t.Fatalf("nginx 回自己的 500 页时必须判失败：%+v", res)
	}
	if res.Step != "nginx 自身的错误页" {
		t.Errorf("失败必须写清是哪一步，实际 step=%q（detail=%s）", res.Step, res.Detail)
	}
	if res.BodyBytes != proxyBodyProbeSize || gotBody != proxyBodyProbeSize {
		t.Errorf("请求体应当是 %d 字节，实际 res=%d 发出=%d", proxyBodyProbeSize, res.BodyBytes, gotBody)
	}
	if gotScheme != "http" || gotHost != "a.test" || gotPort != 18080 || gotPath != "/api" {
		t.Errorf("探测地址不对：%s://%s:%d%s", gotScheme, gotHost, gotPort, gotPath)
	}
	if !strings.Contains(res.Detail, "client_body_temp") {
		t.Errorf("失败说明应指出最可能的原因（client_body_temp），实际：%s", res.Detail)
	}
}

// TestProbeProxyLargeBodyOK：正常响应判通过，且请求体确实是 64KB。
func TestProbeProxyLargeBodyOK(t *testing.T) {
	srv := newProxyTestServer(t)
	seedProxyNginx(t, srv)
	stubProxyBodyProbe(t, true, func(_ context.Context, _ string, _ string, _ int, _ string, body []byte, _ time.Duration) (string, string, error) {
		if len(body) != proxyBodyProbeSize {
			t.Errorf("请求体大小 = %d，期望 %d", len(body), proxyBodyProbeSize)
		}
		return "200", "upstream-ok", nil
	})
	res := srv.probeProxyLargeBody(context.Background(), &proxies.Rule{
		ID: 2, Name: "ok", Listen: 18081, Target: "http://127.0.0.1:9", Enabled: true,
	})
	if res.Status != "ok" || !res.OK {
		t.Fatalf("正常响应应判通过：%+v", res)
	}
}

// TestProbeProxyLargeBodyHonestUnknown：探不到时必须是"未能探测"，不能算通过。
func TestProbeProxyLargeBodyHonestUnknown(t *testing.T) {
	srv := newProxyTestServer(t)
	seedProxyNginx(t, srv)

	t.Run("端口没在听：不发请求", func(t *testing.T) {
		calls := stubProxyBodyProbe(t, false, nil)
		res := srv.probeProxyLargeBody(context.Background(), &proxies.Rule{
			ID: 3, Name: "x", Listen: 18082, Target: "http://127.0.0.1:9", Enabled: true,
		})
		if res.Status != "unknown" || res.OK {
			t.Fatalf("端口没在听时必须判「未能探测」：%+v", res)
		}
		if *calls != 0 {
			t.Errorf("端口没在听时不该真的发请求（实际 %d 次）", *calls)
		}
	})

	t.Run("请求失败没有响应：如实报未能探测", func(t *testing.T) {
		stubProxyBodyProbe(t, true, func(context.Context, string, string, int, string, []byte, time.Duration) (string, string, error) {
			return "000", "", context.DeadlineExceeded
		})
		res := srv.probeProxyLargeBody(context.Background(), &proxies.Rule{
			ID: 4, Name: "x", Listen: 18083, Target: "http://127.0.0.1:9", Enabled: true,
		})
		if res.Status != "unknown" || res.OK {
			t.Fatalf("没有拿到响应时必须判「未能探测」：%+v", res)
		}
		if res.Step != "发送请求" {
			t.Errorf("应写清卡在「发送请求」，实际 step=%q", res.Step)
		}
	})

	t.Run("规则停用：不进探测", func(t *testing.T) {
		calls := stubProxyBodyProbe(t, true, nil)
		res := srv.probeProxyLargeBody(context.Background(), &proxies.Rule{
			ID: 5, Name: "x", Listen: 18084, Target: "http://127.0.0.1:9", Enabled: false,
		})
		if res.Status != "unknown" {
			t.Fatalf("停用规则应判未能探测：%+v", res)
		}
		if *calls != 0 {
			t.Errorf("停用规则不该发请求（实际 %d 次）", *calls)
		}
	})
}

// TestProxyBodyProbeNotOnListRenderPath：大请求体探测**不许**出现在列表/首屏
// 渲染路径上（AGENTS.md 第三节坑 165：昂贵探测放列表里会把整页拖慢十几秒）。
//
// 判据是行为级的：跑一遍 proxyView（列表每行都调它），断言探测函数一次都没被调，
// 且视图里不携带任何探测结果字段。
func TestProxyBodyProbeNotOnListRenderPath(t *testing.T) {
	srv := newProxyTestServer(t)
	seedProxyNginx(t, srv)
	calls := stubProxyBodyProbe(t, true, nil)

	prevTarget := proxyProbeTargetFn
	proxyProbeTargetFn = func(context.Context, string) (bool, string) { return true, "可达" }
	t.Cleanup(func() { proxyProbeTargetFn = prevTarget })

	// Enabled=false：proxyView 里就会跳过 portListening（避免测试真的去拨号）。
	rule := &proxies.Rule{
		ID: 6, Name: "列表规则", Listen: 18085, Domains: "a.test",
		Target: "http://127.0.0.1:9", Enabled: false,
	}
	view := srv.proxyView(context.Background(), rule)
	if *calls != 0 {
		t.Fatalf("列表渲染路径上跑了 %d 次大请求体探测（必须是按需触发）", *calls)
	}
	for _, k := range []string{"body_probe", "probe_body", "large_body"} {
		if _, ok := view[k]; ok {
			t.Errorf("列表视图里不该出现探测结果字段 %q", k)
		}
	}
}

// TestHandleProxyBodyProbeEndpoint：接口把规则查出来、跑探测、按统一信封返回。
func TestHandleProxyBodyProbeEndpoint(t *testing.T) {
	srv, _ := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "接口规则", Listen: 18086, Domains: "a.test", Target: "http://127.0.0.1:9", Enabled: true,
	})
	stubProxyBodyProbe(t, true, func(context.Context, string, string, int, string, []byte, time.Duration) (string, string, error) {
		return "500", nginxOwnErrorPage500, nil
	})

	req := httptest.NewRequest("POST", "/api/v1/proxies/"+strconv.FormatInt(rule.ID, 10)+"/probe-body", nil)
	req.SetPathValue("id", strconv.FormatInt(rule.ID, 10))
	rec := httptest.NewRecorder()
	srv.handleProxyBodyProbe(rec, req)

	if rec.Code != 200 {
		t.Fatalf("探测接口应返回 200（结论在 body 里如实标出），实际 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Status string `json:"status"`
			Step   string `json:"step"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应失败：%v（%s）", err, rec.Body.String())
	}
	if !env.OK || env.Data.Status != "bad" {
		t.Fatalf("接口应如实回报 bad，实际：%s", rec.Body.String())
	}
	if env.Data.Step != "nginx 自身的错误页" {
		t.Errorf("接口应带回失败步骤，实际：%s", rec.Body.String())
	}
}

// TestProbeProxyLargeBodyWithRealNginx 用真实 nginx 跑一遍探测（本机端到端证据）：
//
//	① 请求体缓冲关掉时，即使 client_body_temp 不可写也能正常转发 → 探测 ok；
//	② 缓冲打开（事故前的行为）且 client_body_temp 不可写时，nginx 回它自己的
//	   500 页 → 探测必须判 bad 并指出 step=nginx 自身的错误页。
//
// ② 正是 2026-09-18 生产事故的形态，本机可复现。
// 沙箱化：临时 prefix + 随机高端口 + 自己的 client_body_temp，碰不到生产配置。
func TestProbeProxyLargeBodyWithRealNginx(t *testing.T) {
	nginxBin := "/opt/homebrew/bin/nginx"
	if _, err := os.Stat(nginxBin); err != nil {
		t.Skip("未安装 nginx，跳过大请求体探测的真实校验")
	}
	srv := newProxyTestServer(t)
	seedProxyNginx(t, srv) // 只为让 s.nginxInstalled() 为 true

	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	vhostDir := filepath.Join(dir, "vhosts")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "logs")
	bodyDir := filepath.Join(dir, "body")
	for _, d := range []string{confDir, vhostDir, runDir, logDir, bodyDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	proxyPort := freePortForTest(t)
	upPort := freePortForTest(t)
	vhostPath := filepath.Join(vhostDir, "proxy-1.conf")

	main := fmt.Sprintf(`worker_processes 1;
error_log %s/error.log warn;
pid %s/nginx.pid;
events { worker_connections 64; }
http {
    include /opt/homebrew/etc/nginx/mime.types;
    default_type application/octet-stream;
    access_log off;
    client_body_temp_path %s;
    proxy_temp_path %s/proxy;
    fastcgi_temp_path %s/fastcgi;
    uwsgi_temp_path %s/uwsgi;
    scgi_temp_path %s/scgi;
    client_max_body_size 8m;
    server { listen 127.0.0.1:%d; server_name _; location / { return 200 "upstream-ok"; } }
    include %s/*.conf;
}
`, logDir, runDir, bodyDir, runDir, runDir, runDir, runDir, upPort, vhostDir)
	mainPath := filepath.Join(confDir, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	writeVhost := func(requestBuffering string) {
		t.Helper()
		content := fmt.Sprintf(`server {
  listen 127.0.0.1:%d;
  server_name a.test;
  location / {
    proxy_pass http://127.0.0.1:%d;
    proxy_set_header Host $http_host;
    proxy_buffering off;
    proxy_request_buffering %s;
  }
}
`, proxyPort, upPort, requestBuffering)
		if err := os.WriteFile(vhostPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stopNginx := func() {
		_ = exec.Command(nginxBin, "-c", mainPath, "-s", "stop").Run()
	}
	t.Cleanup(func() {
		stopNginx()
		// 恢复权限，保证 t.TempDir 能删干净。
		_ = os.Chmod(bodyDir, 0o755)
	})
	restartNginx := func() {
		t.Helper()
		// nginx 启动时会检查/创建临时目录：先恢复可写，起来之后再按需收紧。
		_ = os.Chmod(bodyDir, 0o755)
		if out, err := exec.Command(nginxBin, "-c", mainPath, "-t").CombinedOutput(); err != nil {
			t.Fatalf("沙箱 nginx 配置不合法：%v\n%s", err, out)
		}
		stopNginx()
		time.Sleep(100 * time.Millisecond)
		if out, err := exec.Command(nginxBin, "-c", mainPath).CombinedOutput(); err != nil {
			t.Fatalf("启动沙箱 nginx 失败：%v\n%s", err, out)
		}
		waitPortForTest(t, proxyPort)
	}

	rule := &proxies.Rule{
		ID: 1, Name: "沙箱规则", Listen: proxyPort, Domains: "a.test",
		Target: fmt.Sprintf("http://127.0.0.1:%d", upPort), Enabled: true,
	}

	// ① 新版生成器关掉请求体缓冲：临时目录不可写也不该坏。
	writeVhost("off")
	restartNginx()
	if err := os.Chmod(bodyDir, 0o500); err != nil {
		t.Fatal(err)
	}
	res := srv.probeProxyLargeBody(context.Background(), rule)
	if res.Status != "ok" {
		t.Fatalf("关掉请求体缓冲后，client_body_temp 不可写也应能转发：%+v", res)
	}

	// ② 事故前的行为（请求体先落盘）+ 同一份不可写目录 → nginx 自己的 500。
	writeVhost("on")
	restartNginx()
	if err := os.Chmod(bodyDir, 0o500); err != nil {
		t.Fatal(err)
	}
	res = srv.probeProxyLargeBody(context.Background(), rule)
	if res.Status != "bad" {
		t.Fatalf("旧行为下应当探测到 nginx 自己的 500：%+v", res)
	}
	if res.Step != "nginx 自身的错误页" {
		t.Errorf("失败应写清是 nginx 自身的错误页，实际 step=%q code=%s", res.Step, res.Code)
	}
	if !looksLikeNginxErrorPage(res.BodyPrefix) {
		t.Errorf("响应体应当就是 nginx 出厂 500 页，实际：%s", res.BodyPrefix)
	}
}

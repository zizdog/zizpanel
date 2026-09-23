package web

import (
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  aria2 的面板托管界面（/aria/）与 RPC 代理门禁
//
//  这一类要锁死三件事：
//    ① 没登录就不能看界面、不能调 RPC（RPC 是控制面：能删文件、能加下载）；
//    ② 界面必须**自动配好** RPC（否则用户打开只看到"未连接"，还得自己填密钥）；
//    ③ RPC 代理只收 application/json 且拒绝跨站来源 —— 这是替代 CSRF 双提交的
//       两道防线（AriaNg 是第三方前端，不会带 X-CSRF-Token）。
// ============================================================================

// writeAria2Conf 在测试沙箱的家目录里造一份配置（返回明文密钥）。
func writeAria2Conf(t *testing.T, srv *Server, secret string) string {
	t.Helper()
	conf := services.Aria2ConfPath(srv.Cfg.UserHome)
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("dir=/tmp\nrpc-secret="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return conf
}

func TestAria2UIRequiresLogin(t *testing.T) {
	_, ts := newTestServer(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/aria/", "/aria/js/aria-ng-a5324ae04a.min.js"} {
		res, err := noRedirect.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("未登录访问 %s 应 401，实际 %d", path, res.StatusCode)
		}
	}
}

func TestAria2UIServesAriaNgAndPresetsRPC(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)
	secret := "s3cret-aria2-key"
	writeAria2Conf(t, srv, secret)

	res, body := getWithCookies(t, ts, "/aria/", cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("界面应 200，实际 %d：%s", res.StatusCode, body)
	}
	if !strings.Contains(body, "aria-ng-a5324ae04a.min.js") {
		t.Errorf("返回的不是 AriaNg 的页面：%s", body[:min(200, len(body))])
	}
	want := base64.RawURLEncoding.EncodeToString([]byte(secret))
	if !strings.Contains(body, want) {
		t.Errorf("页面里没有自动配置 RPC 的脚本（用户会看到「未连接」）——应包含 base64url 密钥 %q", want)
	}
	if !strings.Contains(body, "settings/rpc/set/") {
		t.Error("自动配置必须走 AriaNg 自己的 #!/settings/rpc/set/... 路由（它会保存并跳转）")
	}
	if !strings.Contains(body, "location.hostname") || !strings.Contains(body, "localStorage") {
		t.Error("自动配置要按当前浏览器的主机/端口配，且只在没配过（或密钥变了）时动手")
	}

	// 静态资源：内容与类型都要对（子路径下相对引用，不能回落成面板首页）。
	res2, js := getWithCookies(t, ts, "/aria/js/aria-ng-a5324ae04a.min.js", cookies)
	if res2.StatusCode != http.StatusOK || !strings.Contains(res2.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("静态资源应 200 + javascript，实际 %d %s", res2.StatusCode, res2.Header.Get("Content-Type"))
	}
	if strings.Contains(js, "ZizPanel") && len(js) < 5000 {
		t.Error("拿到的像是面板自己的 index.html（SPA 回落），不是 AriaNg 资源")
	}
}

func TestAria2UIShowsHonestPageWhenNotInstalled(t *testing.T) {
	_, ts := newTestServer(t) // 沙箱家目录里没有 ~/aria/aria2.conf
	cookies := loginTestPanel(t, ts)
	res, body := getWithCookies(t, ts, "/aria/", cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("应返回说明页（200），实际 %d", res.StatusCode)
	}
	if !strings.Contains(body, "还没有安装") {
		t.Errorf("没装时必须如实说明（而不是丢一个连不上的界面），实际：%s", body[:min(200, len(body))])
	}
	if strings.Contains(body, "aria-ng-a5324ae04a.min.js") {
		t.Error("没装时不该把 AriaNg 界面发出去")
	}
}

func TestAria2RPCGuards(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	// ① 未登录 → 401（RPC 是控制面）
	res, err := http.Post(ts.URL+"/jsonrpc", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录调 RPC 应 401，实际 %d", res.StatusCode)
	}

	// ② 已登录但内容类型不对 → 415（跨站表单提交做不到 application/json）
	req, _ := http.NewRequest("POST", ts.URL+"/jsonrpc", strings.NewReader("a=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res2.Body.Close()
	if res2.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("非 JSON 的 RPC 请求应 415，实际 %d", res2.StatusCode)
	}

	// ③ 跨站来源 → 403
	req3, _ := http.NewRequest("POST", ts.URL+"/jsonrpc", strings.NewReader(`{"jsonrpc":"2.0"}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Sec-Fetch-Site", "cross-site")
	for _, c := range cookies {
		req3.AddCookie(c)
	}
	res3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	_ = res3.Body.Close()
	if res3.StatusCode != http.StatusForbidden {
		t.Errorf("跨站来源应 403，实际 %d", res3.StatusCode)
	}
}

func TestAria2RPCProxiesToUpstream(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	// 假上游：断言路径被改写成 /jsonrpc、body 原样转发、并把上游的 JSON 回给浏览器。
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"zizpanel","result":{"version":"1.37.0"}}`))
	}))
	defer upstream.Close()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	old := aria2RPCUpstream
	aria2RPCUpstream = func() *url.URL { return &url.URL{Scheme: "http", Host: u.Host, Path: "/jsonrpc"} }
	t.Cleanup(func() { aria2RPCUpstream = old })

	payload := `{"jsonrpc":"2.0","id":"zizpanel","method":"aria2.getVersion","params":["token:x"]}`
	req, _ := http.NewRequest("POST", ts.URL+"/aria/jsonrpc", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("RPC 代理应 200，实际 %d：%s", res.StatusCode, body)
	}
	if gotPath != "/jsonrpc" {
		t.Errorf("上游路径应被改写成 /jsonrpc（aria2 只认它），实际 %q", gotPath)
	}
	if gotBody != payload {
		t.Errorf("请求体必须原样转发，实际 %q", gotBody)
	}
	if !strings.Contains(string(body), "1.37.0") {
		t.Errorf("上游响应必须回给浏览器，实际 %s", body)
	}
}

// getWithCookies 发一个带会话 Cookie 的 GET，返回状态码与正文。
func getWithCookies(t *testing.T, ts *httptest.Server, path string, cookies []*http.Cookie) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestAria2RPCTimesOutWhenUpstreamHangs 是"aria2 卡死"的真机形态回归（2026-09-23）：
// 进程还在、端口还在听、TCP 连得上，但**一个字节都不回**（写 .aria2 控制文件抛异常后
// 事件循环不再服务 HTTP）。没有超时的话面板的代理会一直等，界面永远停在"连接中…"。
//
// 判据：① 必须在超时附近返回（不是无限等）；② 必须是 502 且文案点明"没有应答"，
// 并把"去点重启"这条出路写出来；③ 不能把"上游拒连"也说成"没有应答"（负向对照）。
func TestAria2RPCTimesOutWhenUpstreamHangs(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	// 半开上游：接受连接、读到请求，然后什么都不回。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c // 故意不读不写：模拟卡死的 aria2
		}
	}()

	oldUp, oldTimeout := aria2RPCUpstream, aria2RPCTimeout
	aria2RPCUpstream = func() *url.URL {
		return &url.URL{Scheme: "http", Host: ln.Addr().String(), Path: "/jsonrpc"}
	}
	aria2RPCTimeout = 400 * time.Millisecond
	t.Cleanup(func() { aria2RPCUpstream, aria2RPCTimeout = oldUp, oldTimeout })

	payload := `{"jsonrpc":"2.0","id":"zizpanel","method":"aria2.getVersion","params":["token:x"]}`
	req, _ := http.NewRequest("POST", ts.URL+"/aria/jsonrpc", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	start := time.Now()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("应当按超时返回（约 0.4s），实际等了 %s —— 界面会一直停在「连接中…」", elapsed)
	}
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("卡死的上游应回 502，实际 %d：%s", res.StatusCode, body)
	}
	msg := string(body)
	if !strings.Contains(msg, "没有应答") {
		t.Errorf("文案要点明「没有应答」（这是卡死而不是配置错的唯一线索），实际：%s", msg)
	}
	if !strings.Contains(msg, "重启") {
		t.Errorf("文案要给出可照做的出路（点「重启」），实际：%s", msg)
	}

	// 负向对照：**没人听**（立刻拒连）不能被说成"卡死"—— 两者的出路不同
	// （一个是"去启动"，一个是"去重启"），混为一谈等于给错药方。
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()
	aria2RPCUpstream = func() *url.URL {
		return &url.URL{Scheme: "http", Host: deadAddr, Path: "/jsonrpc"}
	}
	req2, _ := http.NewRequest("POST", ts.URL+"/aria/jsonrpc", strings.NewReader(payload))
	req2.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	body2, _ := io.ReadAll(res2.Body)
	if res2.StatusCode != http.StatusBadGateway {
		t.Fatalf("拒连也应回 502，实际 %d：%s", res2.StatusCode, body2)
	}
	if strings.Contains(string(body2), "没有应答") {
		t.Errorf("端口没人听时不能说成「没有应答」（那是卡死的说法），实际：%s", body2)
	}
}

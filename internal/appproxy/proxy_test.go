package appproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// TestRewriteBodyRewritesAbsoluteAssetPaths 是本包最核心的行为：
// 前端产物里的绝对路径必须被拉到子路径下，否则页面白屏。
func TestRewriteBodyRewritesAbsoluteAssetPaths(t *testing.T) {
	rw := newRewriter("iopaint", []services.UIRewrite{
		{From: `"/api/v1"`, To: `"/{slug}/api/v1"`},
		{From: "/socket.io", To: "/{slug}/socket.io"},
	})
	html := `<script type="module" src="/assets/index-abc.js"></script>` +
		`<link rel="stylesheet" href="/assets/index-abc.css">` +
		`<link rel="icon" href="/favicon.ico">` +
		`<script>const base="/api/v1";fetch(base+"/run")</script><script>io("/socket.io")</script>`
	got := string(rw.body([]byte(html)))
	for _, want := range []string{
		`src="/iopaint/assets/index-abc.js"`,
		`href="/iopaint/assets/index-abc.css"`,
		`href="/iopaint/favicon.ico"`,
		`"/iopaint/api/v1"`,
		`"/iopaint/socket.io"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("改写后应包含 %q，实际：%s", want, got)
		}
	}
	if strings.Contains(got, `src="/assets/`) {
		t.Error("仍然留着未改写的 /assets/ 绝对路径（页面会白屏）")
	}
}

// TestRewriteBodyIsNotAppliedTwice 防止二次改写：
// 规则是字面量替换，若实现用了"边替换边扫描"，就会得到 /iopaint/iopaint/assets/。
func TestRewriteBodyIsNotAppliedTwice(t *testing.T) {
	rw := newRewriter("it-tools", nil)
	got := string(rw.body([]byte(`<script src="/assets/a.js"></script>`)))
	if strings.Count(got, "/it-tools/assets/") != 1 {
		t.Fatalf("应恰好改写一次，实际：%s", got)
	}
}

// TestLocationHeaderIsRewritten 重定向也要被拉进子路径。
// 真机依据：Uptime Kuma 访问 / 会 302 到 Location: /dashboard，
// 不改写就把用户甩出子路径，看起来就像"应用打不开"。
func TestLocationHeaderIsRewritten(t *testing.T) {
	rw := newRewriter("uptime-kuma", nil)
	cases := map[string]string{
		"/dashboard":             "/uptime-kuma/dashboard",
		"/uptime-kuma/dashboard": "/uptime-kuma/dashboard", // 已带前缀，不动
		"https://example.com/x":  "https://example.com/x",  // 绝对 URL 不碰
		"dashboard":              "dashboard",              // 相对路径交给浏览器
		"":                       "",
		"/":                      "/uptime-kuma/",
	}
	for in, want := range cases {
		if got := rw.location("uptime-kuma", in); got != want {
			t.Errorf("location(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestHandlerStripsPrefixAndRewrites 端到端：起一个假应用，验证
//
//	· 子路径前缀被剥掉（上游收到的是 /foo）
//	· 响应体被改写
//	· Location 被改写
func TestHandlerStripsPrefixAndRewrites(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path == "/redir" {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<script src="/assets/app.js"></script>`)
	}))
	defer up.Close()

	app := services.App{
		ID: "fake", Name: "假应用", Port: portOf(t, up.URL),
		UI: &services.AppUI{Slug: "fake"},
	}
	front := httptest.NewServer(Handler(app))
	defer front.Close()

	// ① 普通请求：前缀被剥掉、正文被改写
	resp, err := http.Get(front.URL + "/fake/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if gotPath != "/index.html" {
		t.Errorf("上游收到的路径应为 /index.html（前缀要被剥掉），实际 %q", gotPath)
	}
	if !strings.Contains(string(body), `src="/fake/assets/app.js"`) {
		t.Errorf("响应体没有改写：%s", body)
	}

	// ② 重定向：Location 被拉到子路径下
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp2, err := client.Get(front.URL + "/fake/redir")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if loc := resp2.Header.Get("Location"); loc != "/fake/elsewhere" {
		t.Errorf("Location 应被改写为 /fake/elsewhere，实际 %q", loc)
	}
}

// TestHandlerRejectsGzipFromUpstream：出站请求必须摘掉 Accept-Encoding，
// 否则上游给压缩正文、改写全部失效（而且不会有任何报错，最难查）。
func TestHandlerRejectsGzipFromUpstream(t *testing.T) {
	var gotAE string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	app := services.App{ID: "fake", Port: portOf(t, up.URL), UI: &services.AppUI{Slug: "fake"}}
	front := httptest.NewServer(Handler(app))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/fake/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotAE != "" {
		t.Errorf("出站请求不应带 Accept-Encoding（否则改写找不到明文），实际 %q", gotAE)
	}
}

// TestBinaryResponseIsNotRewritten 二进制不能改写（图片/字体改了会损坏）。
func TestBinaryResponseIsNotRewritten(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("/assets/\x00\x01\x02"))
	}))
	defer up.Close()
	app := services.App{ID: "fake", Port: portOf(t, up.URL), UI: &services.AppUI{Slug: "fake"}}
	front := httptest.NewServer(Handler(app))
	defer front.Close()

	resp, err := http.Get(front.URL + "/fake/x.png")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "/assets/\x00\x01\x02" {
		t.Errorf("图片内容被改写了：%q", b)
	}
}

// TestSlugsSkipsSelfConfAndPortless：安装器自己写 nginx 的（phpMyAdmin）、
// 以及没有端口的条目不该被生成代理 —— 否则会多出一段永远 502 的 location。
func TestSlugsSkipsSelfConfAndPortless(t *testing.T) {
	for _, a := range Slugs() {
		if a.UI == nil {
			t.Fatalf("%s 没界面却在代理清单里", a.ID)
		}
		if a.UI.SelfConf {
			t.Errorf("%s 的 nginx 由安装器自己写，不该生成代理", a.ID)
		}
		if a.Port <= 0 {
			t.Errorf("%s 没有端口，代理会指向 127.0.0.1:0", a.ID)
		}
	}
}

func portOf(t *testing.T, raw string) int {
	t.Helper()
	i := strings.LastIndex(raw, ":")
	if i < 0 {
		t.Fatalf("无法从 %q 解析端口", raw)
	}
	n := 0
	for _, c := range raw[i+1:] {
		if c < '0' || c > '9' {
			t.Fatalf("端口不是数字：%q", raw)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// TestHomepageRewritesCoverNextjsAssetPaths 锁住用户反馈的「/homepage/ 无样式」。
//
// Homepage 是 Next.js 构建产物，HTML 里是根绝对路径 `/_next/static/…`、`/api/…`；
// 不改写就会打到站点根 404，页面没有样式。这里直接拿目录里的真实规则跑一遍，
// 规则被删/写错时测试立刻红。
func TestHomepageRewritesCoverNextjsAssetPaths(t *testing.T) {
	app, ok := services.FindApp("homepage")
	if !ok || app.UI == nil {
		t.Fatal("目录里没有 homepage 的 UI 声明")
	}
	if app.UI.PreferDirect {
		t.Error("Homepage 加前缀后子路径可用（2026-09-17 mini 实测），不该再标 PreferDirect")
	}
	rw := newRewriter(app.UI.Slug, app.UI.Rewrites)
	html := `<script src="/_next/static/chunks/main-abc.js"></script>` +
		`<link rel="stylesheet" href="/_next/static/css/app.css">` +
		`<link rel="manifest" href="/site.webmanifest">` +
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">` +
		`<link rel="icon" href="/favicon-32x32.png">` +
		`<script>fetch("/api/widgets")</script>`
	got := string(rw.body([]byte(html)))
	for _, want := range []string{
		`src="/homepage/_next/static/chunks/main-abc.js"`,
		`href="/homepage/_next/static/css/app.css"`,
		`href="/homepage/site.webmanifest"`,
		`href="/homepage/apple-touch-icon.png"`,
		`href="/homepage/favicon-32x32.png"`,
		`fetch("/homepage/api/widgets")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Homepage 子路径改写后应包含 %q，实际：%s", want, got)
		}
	}
}

// TestAlistRewritesTargetInlinedBasePath 锁住 Alist 子路径 404 的针对性改写。
//
// 根因（去混淆前端 bundle 得出）：Alist 把 base_path 内联在 HTML 里
// （window.ALIST.base_path），前端拿它拼 API 地址；bundle 里没有 `"/api/`
// 这样的字面量，所以通用改写一定无效，必须**针对性改写 base_path 本身**。
//
// 状态（2026-09-17 真机验证，mini / 0.14.1）：改写后页面内联 base_path 变成
// `/alist/`、`/alist/api/public/settings` 由 404 变 **200**、
// `/alist/static/manifest.json` = 200 —— 子路径可用，所以**不再**标 PreferDirect
// （那颗 ⚠️ 会误导用户以为子路径坏；Note 必须同时说明两个入口都能用）。
func TestAlistRewritesTargetInlinedBasePath(t *testing.T) {
	app, ok := services.FindApp("alist")
	if !ok || app.UI == nil {
		t.Fatal("目录里没有 alist 的 UI 声明")
	}
	if app.UI.PreferDirect {
		t.Error("Alist 的子路径已在真机验证可用（0.14.1：/alist/api/public/settings = 200），" +
			"不该再标 PreferDirect；如有新的反证请附上真机证据再改回来")
	}
	if !strings.Contains(app.UI.Note, "5244") || !strings.Contains(app.UI.Note, "/alist/") {
		t.Errorf("Alist 的 Note 要说清两个入口都可用（5244 直连与 /alist/ 子路径），实际：%s", app.UI.Note)
	}
	rw := newRewriter(app.UI.Slug, app.UI.Rewrites)
	body := `window.ALIST={base_path: '/', settings: {}};` +
		`<script src="/static/js/app.abc.js"></script>`
	got := string(rw.body([]byte(body)))
	if !strings.Contains(got, `base_path: '/alist/'`) {
		t.Errorf("内联的 base_path 没有被改写到子路径：%s", got)
	}
	if !strings.Contains(got, `src="/alist/static/js/app.abc.js"`) {
		t.Errorf("/static/ 没有被改写到子路径：%s", got)
	}
}

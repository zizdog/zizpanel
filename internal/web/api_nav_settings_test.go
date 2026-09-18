package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// api_nav_settings_test.go —— 导航页**外观设置**（标题 / 副标题 / 主题色 / 背景图）HTTP 层单测。
//
// 覆盖用户 2026-09-18 的三条要求，以及这一类最容易出事的地方：
//   · 主题色会被拼进 CSS —— 判据必须是"能安全拼进 CSS 的最小形状"（#rrggbb）；
//   · 背景图地址会被拼进 CSS 的 url() —— 只放行面板托管的图片或 http(s) 直链；
//   · 独立别名页（未登录）必须能拿到外观设置，否则访客看到的是默认样式；
//   · 背景图上限 8 MiB（比图标的 512 KiB 大得多），但不是无限。

func navUploadTo(t *testing.T, ts *httptest.Server, cookies []*http.Cookie,
	path, filename string, content []byte) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", ts.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
		if c.Name == "zp_csrf" {
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
	return res, out
}

func TestNavSettingsRequireAuth(t *testing.T) {
	_, ts := newTestServer(t)
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/nav/settings", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录读取外观设置应 401，得到 %d: %v", res.StatusCode, out)
	}
}

func TestNavSettingsRoundTrip(t *testing.T) {
	_, ts, cookies := setupNavPanel(t)
	// 默认（未设置）：全空 —— 前端据此回落到内置默认值。
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/nav/settings", nil, cookies)
	d0, _ := out["data"].(map[string]any)
	if d0["title"] != "" || d0["accent"] != "" || d0["background"] != "" {
		t.Fatalf("初始外观设置应当是空的，得到 %v", out)
	}
	res, out2, _ := doJSON(t, ts, "POST", "/api/v1/nav/settings", map[string]string{
		"title": "  我的导航  ", "subtitle": "home", "accent": "#3B82F6",
		"background": "/nav/icons/0123456789abcdef.png",
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("保存外观设置应 200，得到 %d: %v", res.StatusCode, out2)
	}
	saved, _ := out2["data"].(map[string]any)
	if saved["title"] != "我的导航" {
		t.Errorf("标题应当折叠首尾空白，得到 %q", saved["title"])
	}
	if saved["accent"] != "#3b82f6" {
		t.Errorf("主题色应当规范化成小写，得到 %q", saved["accent"])
	}
	// 回读（新请求）：必须真的落库，而不是只回显请求体。
	_, out3, _ := doJSON(t, ts, "GET", "/api/v1/nav/settings", nil, cookies)
	d3, _ := out3["data"].(map[string]any)
	if d3["title"] != "我的导航" || d3["background"] != "/nav/icons/0123456789abcdef.png" {
		t.Fatalf("回读与保存不一致：%v", out3)
	}
	// 空串是**有效操作**（恢复默认），不是"没改"。
	_, out4, _ := doJSON(t, ts, "POST", "/api/v1/nav/settings",
		map[string]string{"title": "", "subtitle": "", "accent": "", "background": ""}, cookies)
	d4, _ := out4["data"].(map[string]any)
	if d4["title"] != "" || d4["accent"] != "" || d4["background"] != "" {
		t.Fatalf("清空外观设置应当生效，得到 %v", out4)
	}
}

func TestNavSettingsRejectsBadValues(t *testing.T) {
	_, ts, cookies := setupNavPanel(t)
	cases := []struct {
		name    string
		payload map[string]string
	}{
		{"主题色不是 #rrggbb", map[string]string{"accent": "red"}},
		{"主题色位数不够", map[string]string{"accent": "#fff"}},
		{"主题色里夹 CSS（注入尝试）", map[string]string{"accent": "#fff; background-image:url(http://evil/x)"}},
		{"背景图是相对路径", map[string]string{"background": "../secret.png"}},
		{"背景图指向面板接口", map[string]string{"background": "/api/v1/settings"}},
		{"背景图协议非法", map[string]string{"background": "javascript:alert(1)"}},
		{"背景图是 data URL", map[string]string{"background": "data:image/png;base64,AAAA"}},
		{"标题过长", map[string]string{"title": strings.Repeat("长", navMaxTitleLen+1)}},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/nav/settings", c.payload, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s：应当 400，得到 %d（%v）", c.name, res.StatusCode, out)
		}
	}
	// http(s) 直链是允许的：用户可能把背景图放在别处（自建图床、CDN）。
	if res, out, _ := doJSON(t, ts, "POST", "/api/v1/nav/settings",
		map[string]string{"background": "https://example.com/bg.jpg"}, cookies); res.StatusCode != http.StatusOK {
		t.Errorf("https 背景直链应当允许，得到 %d（%v）", res.StatusCode, out)
	}
}

// TestNavSettingsVisibleOnPublicPage：外观设置必须出现在**公开**的 /nav/data 里。
//
// 独立别名页是匿名可访问的，它只能读 /nav/data —— 设置不带出去的话，
// 访客看到的就是默认标题与默认配色（用户改了却"没生效"）。
func TestNavSettingsVisibleOnPublicPage(t *testing.T) {
	_, ts, cookies := setupNavPanel(t)
	if res, out, _ := doJSON(t, ts, "POST", "/api/v1/nav/settings",
		map[string]string{"title": "我的首页", "accent": "#22c55e"}, cookies); res.StatusCode != http.StatusOK {
		t.Fatalf("保存失败 %d: %v", res.StatusCode, out)
	}
	res, err := ts.Client().Get(ts.URL + "/nav/data") // 刻意不带 Cookie
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	data, _ := body["data"].(map[string]any)
	st, _ := data["settings"].(map[string]any)
	if st == nil || st["title"] != "我的首页" || st["accent"] != "#22c55e" {
		t.Fatalf("公开 /nav/data 必须带上外观设置，得到 %v", body)
	}
}

func TestNavBackgroundUploadLimits(t *testing.T) {
	_, ts, cookies := setupNavPanel(t)
	// 2 MiB 的 PNG：图标接口会拒（上限 512 KiB），背景图接口应当接受。
	big := append(navTestPNG("bg"), bytes.Repeat([]byte("x"), 2<<20)...)
	if res, _ := navUploadTo(t, ts, cookies, "/api/v1/nav/icons", "big.png", big); res.StatusCode != http.StatusBadRequest {
		t.Errorf("2 MiB 走图标接口应当 400（图标上限 512 KiB）")
	}
	res, out := navUploadTo(t, ts, cookies, "/api/v1/nav/background", "big.png", big)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("2 MiB 背景图应当接受，得到 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if url, _ := data["url"].(string); !navLocalIconRe.MatchString(url) {
		t.Fatalf("背景图地址应当是面板托管的内容哈希路径，得到 %q", url)
	}
	// 超过 8 MiB：必须拒（"大"不等于"无限"）。
	huge := append(navTestPNG("huge"), bytes.Repeat([]byte("x"), (8<<20)+4096)...)
	if res, _ := navUploadTo(t, ts, cookies, "/api/v1/nav/background", "huge.png", huge); res.StatusCode != http.StatusBadRequest {
		t.Errorf("超过 8 MiB 的背景图应当 400")
	}
	// SVG：背景图刻意不收（脚本型格式不开放给"整页背景"这个位置）。
	if res, _ := navUploadTo(t, ts, cookies, "/api/v1/nav/background", "x.svg",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)); res.StatusCode != http.StatusBadRequest {
		t.Errorf("背景图不应接受 SVG")
	}
}

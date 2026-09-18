package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// api_nav_icons_test.go —— 导航页「本地图标」HTTP 层单测。
//
// 覆盖用户 2026-09-18 的两条要求：上传本地图标 + 在已上传的里面选。
// 同时把这一类最容易出事的地方钉死：
//   · 上传只认白名单图片（扩展名 + 文件头 + SVG 不含脚本）；
//   · 公开读取路径**不能**被用来读面板数据目录里的别的文件（路径穿越）；
//   · 内容寻址 → 同一张图重复上传是同一个文件（不堆副本）；
//   · 能传也能删（"能装不能卸"是同一类问题）。
//
// 全部走 httptest；`newTestServer` 已经把 DataDir 指向 t.TempDir()，
// 所以图标落盘也只落在临时目录里，不碰真实面板数据。

// navTestPNG 造一份"文件头真的是 PNG"的内容（后面跟任意字节，够魔数校验用）。
func navTestPNG(marker string) []byte {
	return append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte(marker)...)
}

func navIconUploadReq(t *testing.T, ts *httptest.Server, cookies []*http.Cookie,
	filename string, content []byte) (*http.Response, map[string]any) {
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
	req, err := http.NewRequest("POST", ts.URL+"/api/v1/nav/icons", &buf)
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

func TestNavIconUploadListReadDelete(t *testing.T) {
	srv, ts, cookies := setupNavPanel(t)

	png := navTestPNG("zizpanel-nav-icon-test")
	res, out := navIconUploadReq(t, ts, cookies, "我的图标.png", png)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("上传图标应 200，得到 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("上传响应缺少 data: %v", out)
	}
	name, _ := data["name"].(string)
	url, _ := data["url"].(string)
	if !navIconNameRe.MatchString(name) {
		t.Fatalf("图标名应当是内容寻址名（<16 位哈希>.<ext>），得到 %q", name)
	}
	if url != navIconURLBase+name {
		t.Fatalf("图标地址应当是 %s，得到 %q", navIconURLBase+name, url)
	}

	// 真的落盘了（在临时 DataDir 下），且内容一字不差。
	onDisk := filepath.Join(srv.Cfg.DataDir, navIconDirName, name)
	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("图标应当落在 %s：%v", onDisk, err)
	}
	if !bytes.Equal(got, png) {
		t.Fatal("落盘内容与上传内容不一致")
	}

	// 公开读取（**不带任何 Cookie**）：导航页是匿名首页，图标必须匿名可读。
	pub, err := ts.Client().Get(ts.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pub.Body.Close() }()
	if pub.StatusCode != http.StatusOK {
		t.Fatalf("匿名读取图标应 200，得到 %d", pub.StatusCode)
	}
	if ct := pub.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Fatalf("Content-Type 应为 image/png，得到 %q", ct)
	}
	if csp := pub.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("图标响应必须带收紧的 CSP（防 SVG 脚本），得到 %q", csp)
	}

	// 列表里能看到它。
	resList, outList, _ := doJSON(t, ts, "GET", "/api/v1/nav/icons", nil, cookies)
	if resList.StatusCode != http.StatusOK {
		t.Fatalf("列出图标应 200，得到 %d", resList.StatusCode)
	}
	listData, _ := outList["data"].(map[string]any)
	icons, _ := listData["icons"].([]any)
	found := false
	for _, it := range icons {
		m, _ := it.(map[string]any)
		if m["name"] == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("刚上传的图标应当出现在列表里：%v", outList)
	}

	// 同一张图再传一次：内容相同 → 同一个文件，不堆副本。
	res2, out2 := navIconUploadReq(t, ts, cookies, "另一个名字.png", png)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("重复上传应 200，得到 %d: %v", res2.StatusCode, out2)
	}
	data2, _ := out2["data"].(map[string]any)
	if data2["name"] != name {
		t.Fatalf("同一内容重复上传应当复用同一个文件（%s），得到 %v", name, data2["name"])
	}

	// 删除：能传就必须能删，否则攒下的图标再也清不掉。
	resDel, outDel, _ := doJSON(t, ts, "DELETE", "/api/v1/nav/icons/"+name, nil, cookies)
	if resDel.StatusCode != http.StatusOK {
		t.Fatalf("删除图标应 200，得到 %d: %v", resDel.StatusCode, outDel)
	}
	if _, err := os.Stat(onDisk); !os.IsNotExist(err) {
		t.Fatalf("删除后文件应当不在磁盘上：%v", err)
	}
	gone, err := ts.Client().Get(ts.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gone.Body.Close() }()
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后再读应当 404，得到 %d", gone.StatusCode)
	}
}

func TestNavIconUploadRejectsBadInput(t *testing.T) {
	_, ts, cookies := setupNavPanel(t)
	cases := []struct {
		name     string
		filename string
		content  []byte
		why      string
	}{
		{"非图片扩展名", "note.txt", []byte("hello"), "只允许 png/jpg/gif/webp/svg/ico"},
		{"PNG 扩展名但内容不是 PNG", "fake.png", []byte("this is not a png at all"), "文件头必须是 PNG 魔数"},
		{"SVG 里带脚本", "evil.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "SVG 不允许脚本"},
		{"空文件", "empty.png", []byte{}, "0 字节要如实拒绝"},
	}
	for _, c := range cases {
		res, out := navIconUploadReq(t, ts, cookies, c.filename, c.content)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s：应当 400（%s），得到 %d: %v", c.name, c.why, res.StatusCode, out)
		}
	}
}

func TestNavIconUploadRejectsTooLarge(t *testing.T) {
	_, ts, cookies := setupNavPanel(t)
	big := append(navTestPNG("big"), bytes.Repeat([]byte("x"), navIconMaxBytes)...)
	res, out := navIconUploadReq(t, ts, cookies, "big.png", big)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("超过 %d 字节应当 400，得到 %d: %v", navIconMaxBytes, res.StatusCode, out)
	}
}

// TestNavIconPublicReadBlocksTraversal 是这一组里最要紧的一条：
// 公开读取入口绝不能被用来读面板数据目录里的**别的文件**。
func TestNavIconPublicReadBlocksTraversal(t *testing.T) {
	_, ts, _ := setupNavPanel(t)
	for _, p := range []string{
		"/nav/icons/../panel.db",
		"/nav/icons/..%2fpanel.db",
		"/nav/icons/%2e%2e%2fpanel.db",
		"/nav/icons/0123456789abcdef.png/../../panel.db",
		"/nav/icons/config.json",
		"/nav/icons/",
	} {
		res, err := ts.Client().Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("%s 不该能读到内容（得到 200）", p)
		}
	}
}

// TestNavIconWriteRequiresAuth：写操作必须登录（面板的写接口一律 requireAuth）。
func TestNavIconWriteRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t)
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/nav/icons", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录列出图标应 401，得到 %d: %v", res.StatusCode, out)
	}
}

// TestValidateNavIconAcceptsOnlyPanelLocalIcons 锁住判据的**边界**：
// 放行面板自己托管的图标，别的相对路径一个都不放行。
func TestValidateNavIconAcceptsOnlyPanelLocalIcons(t *testing.T) {
	okCases := []string{
		"",
		"🧭",
		"https://example.com/a.png",
		"/nav/icons/0123456789abcdef.png",
		"/nav/icons/0123456789abcdef.svg",
	}
	for _, v := range okCases {
		if _, err := validateNavIcon(v); err != nil {
			t.Errorf("%q 应当被接受，却被拒：%v", v, err)
		}
	}
	badCases := []string{
		"/nav/icons/../panel.db",
		"/nav/icons/0123456789abcdef.exe",
		"/nav/icons/short.png",
		"/api/v1/nav/icons",
		"//evil.com/x.png",
		"javascript:alert(1)",
		"/nav/icons/0123456789ABCDEF.png", // 大写哈希不是我们生成的形状
	}
	for _, v := range badCases {
		if _, err := validateNavIcon(v); err == nil {
			t.Errorf("%q 应当被拒绝（只放行面板自己生成的 /nav/icons/<hash>.<ext>）", v)
		}
	}
}

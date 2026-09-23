package web

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestPWAManifestIsSuffixAware manifest 的 start_url / scope / 图标地址必须带
// 面板入口（安全后缀），否则"加到主屏"后打开的是 404。
func TestPWAManifestIsSuffixAware(t *testing.T) {
	for _, tc := range []struct{ suffix, want string }{
		{"", "/"},
		{"abc123", "/abc123/"},
		{"/abc123/", "/abc123/"},
	} {
		srv, _ := newTestServer(t)
		srv.Cfg.PanelSuffix = tc.suffix
		rec := httptest.NewRecorder()
		srv.handlePWAManifest(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
		if rec.Code != 200 {
			t.Fatalf("后缀 %q：manifest 应当 200，实际 %d", tc.suffix, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "manifest+json") {
			t.Errorf("后缀 %q：Content-Type 应当是 manifest+json，实际 %q", tc.suffix, ct)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("manifest 不是 JSON：%v", err)
		}
		for _, k := range []string{"start_url", "scope"} {
			if got, _ := m[k].(string); got != tc.want {
				t.Errorf("后缀 %q：%s 应当是 %q，实际 %q", tc.suffix, k, tc.want, got)
			}
		}
		icons, _ := m["icons"].([]any)
		if len(icons) < 2 {
			t.Fatalf("图标至少要有 192 与 512 两种：%+v", m["icons"])
		}
		for _, it := range icons {
			ic, _ := it.(map[string]any)
			src, _ := ic["src"].(string)
			if !strings.HasPrefix(src, tc.want) {
				t.Errorf("后缀 %q：图标地址 %q 没有带入口前缀", tc.suffix, src)
			}
			if !strings.Contains(src, "/pwa/icon-") {
				t.Errorf("图标地址形状不对：%q", src)
			}
		}
		if d, _ := m["display"].(string); d != "standalone" {
			t.Errorf("display 应当是 standalone（否则加主屏后还是浏览器壳）：%q", d)
		}
	}
}

// TestPWAIconIsRealPNG 图标必须是能解码的真 PNG，且画了东西（不是纯色/空白）。
func TestPWAIconIsRealPNG(t *testing.T) {
	for _, size := range []int{192, 512} {
		srv, _ := newTestServer(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/pwa/icon-"+strconv.Itoa(size)+".png", nil)
		srv.handlePWAIcon(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%d：图标应当 200，实际 %d", size, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("%d：Content-Type 应当是 image/png，实际 %q", size, ct)
		}
		img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("%d：PNG 解码失败：%v", size, err)
		}
		b := img.Bounds()
		if b.Dx() != size || b.Dy() != size {
			t.Errorf("%d：尺寸不对，实际 %d×%d", size, b.Dx(), b.Dy())
		}
		// 中心落在 Z 的斜线上 → 必须接近白色（证明真的画了图形）
		r, g, bl, _ := img.At(size/2, size/2).RGBA()
		if r>>8 < 200 || g>>8 < 200 || bl>>8 < 200 {
			t.Errorf("%d：中心像素应当是白色笔画，实际 r=%d g=%d b=%d", size, r>>8, g>>8, bl>>8)
		}
		// 左上角在圆角之外 → 必须透明（maskable 才不会切掉图形）
		if _, _, _, a := img.At(1, 1).RGBA(); a != 0 {
			t.Errorf("%d：左上角应当在圆角外（透明），实际 alpha=%d", size, a>>8)
		}
		// 尺寸以外的请求必须 404（不许被当成图片代理）
		bad := httptest.NewRecorder()
		badReq := httptest.NewRequest(http.MethodGet, "/pwa/icon-64.png", nil)
		srv.handlePWAIcon(bad, badReq)
		if bad.Code != http.StatusNotFound {
			t.Errorf("未支持的尺寸应当 404，实际 %d", bad.Code)
		}
	}
}

// TestServiceWorkerIsServedAndSafe sw.js 必须能被浏览器取到（作用域正确），
// 且**绝不缓存 /api/**（拿旧数据糊弄用户比打不开更糟）。
func TestServiceWorkerIsServedAndSafe(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("sw.js 应当 200，实际 %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("sw.js 的 Content-Type 应当是 javascript，实际 %q", ct)
	}
	// 就地升级后必须换缓存：no-cache 才会让浏览器重新校验这个文件。
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("sw.js 必须 no-cache（否则升级后仍是旧 worker）：%q", cc)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(res.Body); err != nil {
		t.Fatal(err)
	}
	src := buf.String()
	for _, want := range []string{"caches.open", "skipWaiting", "clients.claim", "zpanel-shell-"} {
		if !strings.Contains(src, want) {
			t.Errorf("sw.js 里应当有 %q", want)
		}
	}
	// /api/ 必须绕开缓存
	if !strings.Contains(src, "/api/") || !strings.Contains(src, "return; // 接口：只走网络，绝不缓存") {
		t.Error("sw.js 必须显式绕开 /api/（接口数据必须新鲜）")
	}
	// 入口之外的（应用代理/导航页）不插手
	if !strings.Contains(src, "isShellAsset") {
		t.Error("sw.js 必须只处理面板自己的静态外壳")
	}
}

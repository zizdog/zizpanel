package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  下载接口的 Range / 媒体 MIME 门禁
//
//  视频拖动进度条、音频 seek 都依赖 HTTP Range。`http.ServeContent` 现在支持它，
//  但它是**隐式**行为：谁把它换成 io.Copy（或自己写 Content-Length）就会静默退化成
//  "整文件重下、进度条一拖就卡死"。所以这里真的发一次 `Range: bytes=0-99`，
//  必须回 206 + Content-Range。
//
//  另一半是 MIME：下载接口以前一律回 application/octet-stream，
//  `<video>/<audio>` 拿到会直接拒播（坑 178）—— 媒体必须回真实类型。
// ============================================================================

func TestFileDownloadRangeReturns206(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	p := filepath.Join(srv.Cfg.UserHome, "clip.mp4")
	body := strings.Repeat("0123456789", 100) // 1000 字节
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet,
		ts.URL+"/api/v1/files/download?path="+url.QueryEscape(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	req.Header.Set("Range", "bytes=0-99")

	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 请求应回 206，实际 %d（ServeContent 被换掉/绕过了？进度条会拖不动）", res.StatusCode)
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 0-99/1000" {
		t.Errorf("Content-Range = %q，想要 %q", got, "bytes 0-99/1000")
	}
	if got := res.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q，想要 bytes", got)
	}
	// 媒体必须回真实 MIME，否则 <video> 拒播（坑 178）。
	if got := res.Header.Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q，想要 video/mp4 —— octet-stream 会让浏览器拒播", got)
	}
	buf := make([]byte, 200)
	n, _ := res.Body.Read(buf)
	if n != 100 || string(buf[:n]) != body[:100] {
		t.Errorf("返回了 %d 字节（想要 100）：%q", n, string(buf[:n]))
	}
	t.Logf("Range bytes=0-99 → %d，Content-Range=%q, Content-Type=%q",
		res.StatusCode, res.Header.Get("Content-Range"), res.Header.Get("Content-Type"))
}

// TestDownloadContentTypeForMedia 锁死"哪些扩展回什么 MIME"，
// 非媒体仍回 octet-stream（下载行为不变）。
func TestDownloadContentTypeForMedia(t *testing.T) {
	want := map[string]string{
		"a.mp4": "video/mp4", "a.m4v": "video/mp4", "a.mov": "video/quicktime",
		"a.webm": "video/webm", "a.ogv": "video/ogg",
		"a.mp3": "audio/mpeg", "a.m4a": "audio/mp4", "a.aac": "audio/aac",
		"a.wav": "audio/wav", "a.ogg": "audio/ogg", "a.opus": "audio/ogg", "a.flac": "audio/flac",
		"a.MP4": "video/mp4", // 大小写不敏感
		"a.txt": "application/octet-stream", "a.zip": "application/octet-stream",
		"a.mkv": "application/octet-stream", // 放不了的容器也不假装是 video
	}
	for name, exp := range want {
		if got := downloadContentType(name); got != exp {
			t.Errorf("downloadContentType(%q) = %q，想要 %q", name, got, exp)
		}
	}
}

package api_test

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// body builds deterministic, position-dependent content so a byte-range test
// can prove it received the right slice rather than merely the right length.
func body(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('A' + i%26)
	}
	return b
}

func TestStreamFullBody(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	data := body(5000)
	m := e.newMedia(lib.ID, filepath.Join(e.Root, "clip.mp4"), data)

	res, raw := e.call(http.MethodGet, "/api/v1/media/"+m.ID+"/stream", nil, false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d", res.StatusCode)
	}
	if got := res.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if !bytes.Equal(raw, data) {
		t.Fatalf("全量内容不符, 长度 %d", len(raw))
	}
	if got := res.Header.Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q", got)
	}
}

// TestStreamRangeSemantics locks down 206/416/Content-Range and the exact bytes.
func TestStreamRangeSemantics(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	data := body(5000)
	m := e.newMedia(lib.ID, filepath.Join(e.Root, "clip.mp4"), data)
	url := "/api/v1/media/" + m.ID + "/stream"

	t.Run("first 100 bytes", func(t *testing.T) {
		res, raw := e.callRange(url, "bytes=0-99")
		if res.StatusCode != http.StatusPartialContent {
			t.Fatalf("状态 = %d, 期望 206", res.StatusCode)
		}
		if got := res.Header.Get("Content-Range"); got != "bytes 0-99/5000" {
			t.Fatalf("Content-Range = %q", got)
		}
		if len(raw) != 100 {
			t.Fatalf("长度 = %d, 期望 100", len(raw))
		}
		if !bytes.Equal(raw, data[0:100]) {
			t.Fatal("返回的字节片段与原始内容不符")
		}
		if got := res.Header.Get("Content-Length"); got != "100" {
			t.Fatalf("Content-Length = %q", got)
		}
	})

	t.Run("tail truncated at EOF", func(t *testing.T) {
		res, raw := e.callRange(url, "bytes=4990-99999")
		if res.StatusCode != http.StatusPartialContent {
			t.Fatalf("状态 = %d, 期望 206", res.StatusCode)
		}
		if got := res.Header.Get("Content-Range"); got != "bytes 4990-4999/5000" {
			t.Fatalf("尾部应被截断: %q", got)
		}
		if len(raw) != 10 || !bytes.Equal(raw, data[4990:5000]) {
			t.Fatalf("尾部字节不符, 长度 %d", len(raw))
		}
	})

	t.Run("suffix range", func(t *testing.T) {
		res, raw := e.callRange(url, "bytes=-50")
		if res.StatusCode != http.StatusPartialContent {
			t.Fatalf("状态 = %d, 期望 206", res.StatusCode)
		}
		if got := res.Header.Get("Content-Range"); got != "bytes 4950-4999/5000" {
			t.Fatalf("Content-Range = %q", got)
		}
		if len(raw) != 50 {
			t.Fatalf("长度 = %d, 期望 50", len(raw))
		}
	})

	t.Run("start beyond size is 416", func(t *testing.T) {
		res, raw := e.callRange(url, "bytes=5000-5100")
		if res.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("状态 = %d, 期望 416", res.StatusCode)
		}
		if got := res.Header.Get("Content-Range"); got != "bytes */5000" {
			t.Fatalf("416 Content-Range = %q", got)
		}
		// net/http writes a short error line; what matters is that no file bytes leak.
		if bytes.Equal(raw, data) || bytes.Contains(raw, data[:32]) {
			t.Fatal("416 响应里漏出了文件内容")
		}
	})

	t.Run("multi range falls back to 200 full", func(t *testing.T) {
		res, raw := e.callRange(url, "bytes=0-99,200-299")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("状态 = %d, 期望 200 全量回退", res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); strings.Contains(ct, "multipart") {
			t.Fatalf("不应返回 multipart: %q", ct)
		}
		if len(raw) != 5000 || !bytes.Equal(raw, data) {
			t.Fatalf("应返回完整文件, 长度 %d", len(raw))
		}
	})

	t.Run("unknown media is 404", func(t *testing.T) {
		res, _ := e.call(http.MethodGet, "/api/v1/media/med_missing/stream", nil, false)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("状态 = %d, 期望 404", res.StatusCode)
		}
	})
}

func (e *env) callRange(url, rng string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.TS.URL+url, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Range", rng)
	res, err := e.Client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw := readAll(e.t, res.Body)
	return res, raw
}

func TestCoverFallsBackToPlaceholder(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	m := e.newMedia(lib.ID, filepath.Join(e.Root, "clip.mp4"), body(64))
	res, raw := e.call(http.MethodGet, "/api/v1/media/"+m.ID+"/cover", nil, false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("无封面应返回占位, 状态 = %d", res.StatusCode)
	}
	if !strings.Contains(res.Header.Get("Content-Type"), "image/svg") {
		t.Fatalf("占位内容类型 = %q", res.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(raw), "<svg") {
		t.Fatal("占位不是 SVG")
	}
}

func TestMediaListPaginationBoundaries(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	for i := 0; i < 25; i++ {
		e.newMedia(lib.ID, filepath.Join(e.Root, fmt.Sprintf("c%02d.mp4", i)), body(32))
	}
	res, env, raw := e.do(http.MethodGet, "/api/v1/media?per_page=1000", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态 %d: %s", res.StatusCode, raw)
	}
	var meta struct {
		Page    int `json:"page"`
		PerPage int `json:"per_page"`
		Total   int `json:"total"`
	}
	decodeInto(t, env.Meta, &meta)
	if meta.PerPage != 100 {
		t.Fatalf("per_page 应被夹到 100, 实际 %d", meta.PerPage)
	}
	if meta.Total != 25 {
		t.Fatalf("total = %d", meta.Total)
	}
	var list struct {
		List []map[string]any `json:"list"`
	}
	decodeInto(t, env.Data, &list)
	if len(list.List) != 25 {
		t.Fatalf("应返回 25 条, 实际 %d", len(list.List))
	}
	if got := list.List[0]["stream_url"]; got != "/api/v1/media/"+list.List[0]["id"].(string)+"/stream" {
		t.Fatalf("stream_url = %v", got)
	}
}

// TestFeedCursorWalksEveryItemOnce 锁死随机游标：一轮之内每条只出现一次。
func TestFeedCursorWalksEveryItemOnce(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	ids := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		ids = append(ids, e.newMedia(lib.ID, filepath.Join(e.Root, fmt.Sprintf("f%02d.mp4", i)), body(16)).ID)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 6 && len(seen) < 12; page++ {
		path := "/api/v1/feed/next?limit=5"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		res, env, raw := e.do(http.MethodGet, path, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("状态 %d: %s", res.StatusCode, raw)
		}
		var list struct {
			List []struct {
				ID            string `json:"id"`
				Compatibility struct {
					Direct bool `json:"direct"`
				} `json:"compatibility"`
				StreamURL string `json:"stream_url"`
			} `json:"list"`
		}
		decodeInto(t, env.Data, &list)
		var meta struct {
			NextCursor string `json:"next_cursor"`
			HasMore    bool   `json:"has_more"`
		}
		decodeInto(t, env.Meta, &meta)
		if len(list.List) == 0 {
			if meta.HasMore {
				t.Fatal("空页不应 has_more")
			}
			break
		}
		for _, it := range list.List {
			if seen[it.ID] {
				t.Fatalf("游标翻页返回了重复项 %s", it.ID)
			}
			seen[it.ID] = true
			if !it.Compatibility.Direct {
				t.Fatalf("h264 应可直出: %s", it.ID)
			}
		}
		// 默认随机：顺序由 seed+hash 决定，不再按 id 倒序（见 feed_test.go）。
		cursor = meta.NextCursor
	}
	if len(seen) != 12 {
		t.Fatalf("游标翻了 %d 条, 期望 12", len(seen))
	}
}

func TestHEVCIsNotDirectForOldChrome(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	m := e.newMedia(lib.ID, filepath.Join(e.Root, "hevc.mp4"), body(16))
	m.Codecs.Video = "hevc"
	if err := e.DB.UpdateMediaProbe(m); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, e.TS.URL+"/api/v1/media/"+m.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 AppleWebKit/537.36 Chrome/90.0 Safari/537.36")
	res, err := e.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var env envelope
	decodeRaw(t, readAll(t, res.Body), &env)
	var item struct {
		Compatibility struct {
			Direct bool   `json:"direct"`
			Reason string `json:"reason"`
		} `json:"compatibility"`
	}
	decodeInto(t, env.Data, &item)
	if item.Compatibility.Direct {
		t.Fatal("旧 Chrome 不应直出 HEVC")
	}
	if item.Compatibility.Reason == "" {
		t.Fatal("direct=false 必须给出原因文案")
	}
	if n := len([]rune(item.Compatibility.Reason)); n > 40 {
		t.Fatalf("原因文案 %d 字, 超过 40 字上限", n)
	}
}

var _ = strconv.Itoa

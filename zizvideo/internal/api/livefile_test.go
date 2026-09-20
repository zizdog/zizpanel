package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveFileEndsWhenTheFileDisappears locks the "deleted mid-stream" rule:
// the wrapper must surface an error instead of blocking on a dead fd.
func TestLiveFileEndsWhenTheFileDisappears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.bin")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lf := &liveFile{File: f, path: path, lastCheck: time.Now().Add(-2 * time.Second)}

	buf := make([]byte, 4)
	if _, err := lf.Read(buf); err != nil {
		t.Fatalf("首次读取不应失败: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Force the throttled existence check to run on the next read.
	lf.mu.Lock()
	lf.lastCheck = time.Now().Add(-2 * time.Second)
	lf.mu.Unlock()
	if _, err := lf.Read(buf); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("文件消失后应结束流, 得到 %v", err)
	}
}

func TestLiveFileKeepsStreamingWhilePresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.bin")
	if err := os.WriteFile(path, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lf := &liveFile{File: f, path: path, lastCheck: time.Now().Add(-2 * time.Second)}
	buf := make([]byte, 6)
	n, err := lf.Read(buf)
	if err != nil || n != 6 {
		t.Fatalf("读取失败 n=%d err=%v", n, err)
	}
	if string(buf) != "abcdef" {
		t.Fatalf("内容 = %q", buf)
	}
}

func TestIsMultiRange(t *testing.T) {
	cases := map[string]bool{
		"":                 false,
		"bytes=0-99":       false,
		"bytes=0-99,200-2": true,
		"bytes=0-1, 5-6":   true,
		"items=0-1":        false,
	}
	for in, want := range cases {
		if got := isMultiRange(in); got != want {
			t.Fatalf("isMultiRange(%q) = %v, 期望 %v", in, got, want)
		}
	}
}

func TestContentTypeFor(t *testing.T) {
	cases := map[string]string{
		"a.mp4":  "video/mp4",
		"a.MOV":  "video/quicktime",
		"a.mkv":  "video/x-matroska",
		"a.webm": "video/webm",
		"a.xyz":  "",
	}
	for in, want := range cases {
		if got := contentTypeFor(in); got != want {
			t.Fatalf("contentTypeFor(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

package web

// api_video_concurrency_gate_test.go —— 「同一时间只允许一个视频压缩任务」的门禁。
//
// 为什么需要（2026-10-05 用户报障）：mini 上同时跑着两个 video_compress（不同目录，
// 各自的 target 不同 ⇒ 既有的"同一 target 查重"根本拦不住），两个 ffmpeg 各吃 ~2.7 核，
// 速度从 5~8x 掉到 1.4x、负载 15.9。用户以为"同时只有一个视频在转"。
//
// 一条门禁覆盖整类：按 kind 查重（不是按 target）+ 检查与建任务原子（并发两次只过一个）
// + 别的任务类型不误伤。全用假 Runner，不跑真 ffmpeg、不碰真机。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
	"github.com/zizdog/zizpanel/internal/videoopt"
)

// mkVideoDir 造一个"有视频可压"的目录（压缩任务不会因为无事可做而立刻结束）。
func mkVideoDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.mp4"), bytes.Repeat([]byte{2}, 4000), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runnableInfo 让假探测给出"真的会压"的源信息。
//
// 兜底信息（4000 字节 / 800kbps）会被"原码率封顶即跳过"判成不可压，
// 任务几毫秒就结束 —— 那样测的就不是"任务还在跑"这条判据了。
func runnableInfo() videoopt.MediaInfo {
	return videoopt.MediaInfo{
		Width: 1280, Height: 720, DurationSec: 5, FileBytes: 4000,
		VideoKbps: 5000, AudioKbps: 128, HasVideo: true, HasAudio: true,
	}
}

// infoFor 把目录里的视频登记进假探测（键必须与实际路径一致）。
func infoFor(dirs ...string) map[string]videoopt.MediaInfo {
	m := map[string]videoopt.MediaInfo{}
	for _, d := range dirs {
		m[filepath.Join(d, "a.mp4")] = runnableInfo()
	}
	return m
}

// compressBody 是压缩请求体（**并发安全**：不发 t.Fatal，供 goroutine 用）。
func compressBody(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"dir": dir, "preset": "480p"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// postCompress 打一次压缩请求（并发安全）。
func postCompress(t *testing.T, srv *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/video-compress", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleFileVideoCompress(rec, req)
	return rec
}

// waitNoRunningVideo 等压缩任务结束（否则测试收尾时后台还写着 t.TempDir()）。
func waitNoRunningVideo(t *testing.T, srv *Server, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if srv.Tasks.RunningKind("video_compress") == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("压缩任务一直没结束")
}

func TestVideoCompressSingleTaskGate(t *testing.T) {
	t.Run("a 已有 running 的压缩任务 ⇒ 第二次 409 且任务总数不增加", func(t *testing.T) {
		srv, _ := newTestServer(t)
		// 转码慢一点：第 1 个任务必须**还在跑**，第 2 个请求才测得到查重。
		d1 := mkVideoDir(t, srv.Cfg.WWWRoot, "c1")
		d2 := mkVideoDir(t, srv.Cfg.WWWRoot, "c2")
		withFakeVideoRunner(t, &fakeVideoRunner{
			outBytes: 100, transcodeDelay: 1500 * time.Millisecond, infos: infoFor(d1, d2),
		})

		rec := postCompress(t, srv, compressBody(t, d1))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("第 1 个压缩必须 202，实际 %d：%s", rec.Code, rec.Body.String())
		}
		n := len(srv.Tasks.List())

		rec2 := postCompress(t, srv, compressBody(t, d2))
		if rec2.Code != http.StatusConflict {
			t.Fatalf("已有压缩在跑时第二个必须是 409，实际 %d：%s", rec2.Code, rec2.Body.String())
		}
		if got := len(srv.Tasks.List()); got != n {
			t.Fatalf("409 不许建任务：任务总数 %d → %d", n, got)
		}
		waitNoRunningVideo(t, srv, 10*time.Second)
	})

	t.Run("b 前一个已结束 ⇒ 允许新建（202）", func(t *testing.T) {
		srv, _ := newTestServer(t)
		d1 := mkVideoDir(t, srv.Cfg.WWWRoot, "d1")
		d2 := mkVideoDir(t, srv.Cfg.WWWRoot, "d2")
		withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100, infos: infoFor(d1, d2)})

		if rec := postCompress(t, srv, compressBody(t, d1)); rec.Code != http.StatusAccepted {
			t.Fatalf("第 1 个压缩必须 202，实际 %d：%s", rec.Code, rec.Body.String())
		}
		waitNoRunningVideo(t, srv, 10*time.Second)
		if rec := postCompress(t, srv, compressBody(t, d2)); rec.Code != http.StatusAccepted {
			t.Fatalf("前一个结束后必须能再建（202），实际 %d：%s", rec.Code, rec.Body.String())
		}
		waitNoRunningVideo(t, srv, 10*time.Second)
	})

	t.Run("c 别的类型的 running 任务不阻塞压缩", func(t *testing.T) {
		srv, _ := newTestServer(t)
		release := make(chan struct{})
		srv.Tasks.StartWithTask("file_copy", "elsewhere", "复制文件", func(context.Context, *tasks.Task) (any, error) {
			<-release
			return nil, nil
		})
		defer close(release)
		d1 := mkVideoDir(t, srv.Cfg.WWWRoot, "e1")
		withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100, infos: infoFor(d1)})
		if rec := postCompress(t, srv, compressBody(t, d1)); rec.Code != http.StatusAccepted {
			t.Fatalf("别的任务在跑不该拦压缩（要 202），实际 %d：%s", rec.Code, rec.Body.String())
		}
		waitNoRunningVideo(t, srv, 10*time.Second)
	})

	t.Run("d 两个请求同时打 ⇒ 恰好一个 202、一个 409", func(t *testing.T) {
		srv, _ := newTestServer(t)
		dirs := []string{
			mkVideoDir(t, srv.Cfg.WWWRoot, "f1"),
			mkVideoDir(t, srv.Cfg.WWWRoot, "f2"),
		}
		withFakeVideoRunner(t, &fakeVideoRunner{
			outBytes: 100, transcodeDelay: 1500 * time.Millisecond, infos: infoFor(dirs...),
		})
		bodies := [][]byte{compressBody(t, dirs[0]), compressBody(t, dirs[1])}
		codes := make([]int, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start // 尽量让两个请求真的同时到
				codes[i] = postCompress(t, srv, bodies[i]).Code
			}(i)
		}
		close(start)
		wg.Wait()

		ok, conflict := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusAccepted:
				ok++
			case http.StatusConflict:
				conflict++
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("并发两次必须恰好一个 202、一个 409，实际 %v", codes)
		}
		managed := 0
		for _, m := range srv.Tasks.List() {
			if m.Kind == "video_compress" {
				managed++
			}
		}
		if managed != 1 {
			t.Fatalf("只许建出一个压缩任务，实际 %d 个", managed)
		}
		waitNoRunningVideo(t, srv, 10*time.Second)
	})
}

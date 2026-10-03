package web

// api_video_recursive_gate_test.go —— 「包含子目录」的接口接线门禁。
//
// 扫描规则本身的门禁在 internal/videoopt/recurse_gate_test.go（不绕 HTTP、可注入上限）。
// 这里只断言接口层独有的三件事：
//   ① recursive 缺省 false ⇒ 与既有行为一致（只看当前层、rel_path 为空）；
//   ② 带 recursive:true ⇒ 计划里每个条目都有 rel_path，执行后产物按原目录结构落盘；
//   ③ 基准目录里的软链接指向白名单外 ⇒ 403（越界语义），绝不跟着链接走出去。
// 全用假 Runner，不跑真 ffmpeg。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/videoopt"
)

// recAPIFile 造一个（可嵌套的）视频文件并返回它的绝对路径。
func recAPIFile(t *testing.T, dir, rel string, n int) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = 2
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestVideoRecursiveAPIGate 是接口层的递归门禁。
func TestVideoRecursiveAPIGate(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := filepath.Join(srv.Cfg.WWWRoot, "recvideos")

	top := recAPIFile(t, dir, "top.mp4", 4000)
	s1 := recAPIFile(t, dir, "第1季/01.mkv", 4000)
	s2 := recAPIFile(t, dir, "第1季/动漫/02.mp4", 4000)

	fake := &fakeVideoRunner{outBytes: 100}
	info := videoopt.MediaInfo{
		Width: 1280, Height: 720, DurationSec: 3, FileBytes: 4000,
		VideoKbps: 4000, AudioKbps: 0, HasVideo: true,
	}
	fake.infos = map[string]videoopt.MediaInfo{top: info, s1: info, s2: info}
	withFakeVideoRunner(t, fake)

	recPlanBody := func(t *testing.T, raw []byte) (recursive bool, total int, rows []videoopt.Plan) {
		t.Helper()
		var body struct {
			OK   bool `json:"ok"`
			Data struct {
				Recursive bool            `json:"recursive"`
				Total     int             `json:"total"`
				Rows      []videoopt.Plan `json:"rows"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("响应不是 JSON：%v（%s）", err, string(raw))
		}
		return body.Data.Recursive, body.Data.Total, body.Data.Rows
	}

	t.Run("① 缺省 false：只看当前这一层，rel_path 为空", func(t *testing.T) {
		rec := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "preset": "480p"})
		if rec.Code != http.StatusOK {
			t.Fatalf("video-plan 应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		recursive, total, rows := recPlanBody(t, rec.Body.Bytes())
		if recursive {
			t.Error("缺省 recursive 必须是 false")
		}
		if total != 1 {
			t.Fatalf("缺省只该扫到 1 个（当前层），实际 %d", total)
		}
		if len(rows) != 1 || rows[0].RelPath != "" {
			t.Fatalf("缺省时 rel_path 必须为空（与既有响应一致），实际 %+v", rows)
		}
	})

	t.Run("② recursive:true ⇒ 每个条目带 rel_path", func(t *testing.T) {
		rec := postVideoJSON(t, srv.handleFileVideoPlan,
			map[string]any{"dir": dir, "preset": "480p", "recursive": true})
		if rec.Code != http.StatusOK {
			t.Fatalf("video-plan 应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		recursive, total, rows := recPlanBody(t, rec.Body.Bytes())
		if !recursive || total != 3 {
			t.Fatalf("递归该扫到 3 个且回显 recursive=true，实际 recursive=%v total=%d", recursive, total)
		}
		seen := map[string]bool{}
		for _, r := range rows {
			if r.RelPath == "" {
				t.Errorf("递归时每个条目都必须带 rel_path：%+v", r)
			}
			seen[r.RelPath] = true
			if !strings.HasSuffix(r.Path, filepath.FromSlash(r.RelPath)) {
				t.Errorf("Path 与 rel_path 不一致：%s vs %s", r.Path, r.RelPath)
			}
		}
		for _, want := range []string{"top.mp4", "第1季/01.mkv", "第1季/动漫/02.mp4"} {
			if !seen[want] {
				t.Errorf("缺少 rel_path %q（实际 %v）", want, seen)
			}
		}
	})

	t.Run("③ recursive 执行：产物落在 output/<子目录>/", func(t *testing.T) {
		rec := postVideoJSON(t, srv.handleFileVideoCompress,
			map[string]any{"dir": dir, "preset": "480p", "kbps": 2000, "mode": "bitrate", "recursive": true})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("video-compress 必须 202，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var accepted struct {
			Data struct {
				TaskID string `json:"task_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.Data.TaskID == "" {
			t.Fatalf("没有拿到 task_id：%s", rec.Body.String())
		}
		task := srv.Tasks.Get(accepted.Data.TaskID)
		if task == nil {
			t.Fatal("任务不存在")
		}
		select {
		case <-task.Done():
		case <-time.After(15 * time.Second):
			t.Fatal("任务没结束")
		}
		if got := task.Status(); got != "succeeded" {
			t.Fatalf("任务状态 %s，错误=%v", got, task.Meta().Error)
		}
		for _, want := range []string{
			filepath.Join(dir, "output", "top.480p.mp4"),
			filepath.Join(dir, "output", "第1季", "01.480p.mp4"),
			filepath.Join(dir, "output", "第1季", "动漫", "02.480p.mp4"),
		} {
			if _, err := os.Stat(want); err != nil {
				t.Errorf("产物没按原目录结构落盘（%s）：%v", want, err)
			}
		}
	})

	t.Run("④ 基准目录里的软链接指向白名单外 ⇒ 403（绝不跟链接走出去）", func(t *testing.T) {
		outside := t.TempDir()
		recAPIFile(t, outside, "leak.mp4", 4000)
		link := filepath.Join(dir, "外链")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		rec := postVideoJSON(t, srv.handleFileVideoPlan,
			map[string]any{"dir": link, "preset": "480p", "recursive": true})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("越界的目录必须 403，实际 %d：%s", rec.Code, rec.Body.String())
		}
	})
}

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
	"net/http/httptest"
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

// TestVideoStalePartsGate 是「遗留未完成产物（.part.mp4）」的门禁。
//
// 一条覆盖整类：只有 <dir>/output/ 下的 *.part.mp4 会被列出与删除；
// output/ 外的、非 .part.mp4 的、`../` 越界的、不存在的路径一律拒绝（负向对照逐条断言，
// 删掉任一条判据这个门禁必须变红）。全用假 Runner 与临时目录，不跑真 ffmpeg。
func TestVideoStalePartsGate(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := filepath.Join(srv.Cfg.WWWRoot, "partvideos")
	recAPIFile(t, dir, "top.mp4", 4000) // 有一个真实视频，规划才走正常路径

	// 半成品：output/ 根一个 + 子目录一个（子目录的 rel_path 要能区分同名文件）。
	older := recAPIFile(t, dir, "output/old.480p.mp4.part.mp4", 3000)
	deep := recAPIFile(t, dir, "output/第1季/01.480p.mp4.part.mp4", 2000)
	// 负向对照：非半成品、output/ 外的半成品，一个都不许被列出/删除。
	done := recAPIFile(t, dir, "output/done.480p.mp4", 1500)
	stray := recAPIFile(t, dir, "stray.part.mp4", 1200)

	withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100})

	planOf := func(t *testing.T) ([]videoopt.StalePart, string, bool) {
		t.Helper()
		rec := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "preset": "480p"})
		if rec.Code != http.StatusOK {
			t.Fatalf("video-plan 应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data struct {
				OutDir     string               `json:"out_dir"`
				StaleParts []videoopt.StalePart `json:"stale_parts"`
				Truncated  bool                 `json:"stale_parts_truncated"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是 JSON：%v（%s）", err, string(rec.Body.Bytes()))
		}
		return body.Data.StaleParts, body.Data.OutDir, body.Data.Truncated
	}
	clean := func(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return postVideoJSON(t, srv.handleFileVideoCleanParts, body)
	}

	t.Run("① 只列出 output/ 下的 *.part.mp4（含体积/时间/rel_path）", func(t *testing.T) {
		parts, outDir, truncated := planOf(t)
		if truncated {
			t.Error("没有超上限，不该报截断")
		}
		if len(parts) != 2 {
			t.Fatalf("应恰好列出 output/ 里 2 个半成品，实际 %+v", parts)
		}
		byName := map[string]videoopt.StalePart{}
		for _, p := range parts {
			if !strings.HasSuffix(p.Name, videoopt.PartSuffix) {
				t.Errorf("列了非半成品：%+v", p)
			}
			if p.SizeBytes <= 0 || p.Modified.IsZero() {
				t.Errorf("体积/时间没读到真实值：%+v", p)
			}
			if !withinDir(outDir, p.Path) {
				t.Errorf("列出了 output/ 外的：%+v", p)
			}
			byName[p.Name] = p
		}
		if byName[filepath.Base(older)].RelPath != "old.480p.mp4.part.mp4" {
			t.Errorf("output/ 根的 rel_path 不对：%+v", byName[filepath.Base(older)])
		}
		if byName[filepath.Base(deep)].RelPath != "第1季/01.480p.mp4.part.mp4" {
			t.Errorf("子目录的 rel_path 不对：%+v", byName[filepath.Base(deep)])
		}
	})

	t.Run("② 上限写死且如实说明（超上限不静默）", func(t *testing.T) {
		parts, truncated := videoopt.ScanStaleParts(filepath.Join(dir, "output"), 1)
		if !truncated || len(parts) != 1 {
			t.Fatalf("limit=1 而实际有 2 个时应截断到 1 并报 truncated，实际 %d/%v", len(parts), truncated)
		}
	})

	t.Run("③ 越界 / 非半成品 / 不存在一律拒绝", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			path string
			want int
		}{
			{"output/ 外的半成品", stray, http.StatusForbidden},
			{"../ 越界", filepath.Join(dir, "output", "..", "stray.part.mp4"), http.StatusForbidden},
			{"非 .part.mp4", done, http.StatusBadRequest},
			{"不存在", filepath.Join(dir, "output", "ghost.part.mp4"), http.StatusNotFound},
		} {
			rec := clean(t, map[string]any{"dir": dir, "paths": []string{tc.path}})
			if rec.Code != tc.want {
				t.Errorf("%s 应 %d，实际 %d：%s", tc.name, tc.want, rec.Code, rec.Body.String())
			}
		}
		// 负向对照：被拒的路径一个都没被删掉。
		for _, p := range []string{stray, done} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("被拒绝的路径不该被删：%s（%v）", p, err)
			}
		}
	})

	t.Run("④ 指定路径：只删 output/ 下的半成品，结果与审计都如实", func(t *testing.T) {
		rec := clean(t, map[string]any{"dir": dir, "paths": []string{older}})
		if rec.Code != http.StatusOK {
			t.Fatalf("合法清理应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data struct {
				Removed int `json:"removed"`
				Failed  int `json:"failed"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Data.Removed != 1 || body.Data.Failed != 0 {
			t.Fatalf("应删 1 个且 0 失败，实际 %+v", body.Data)
		}
		if _, err := os.Stat(older); !os.IsNotExist(err) {
			t.Errorf("半成品没被删掉：%v", err)
		}
		// 写接口必须留审计（判据：审计表里确有一条成功记录）。
		var n int
		if err := srv.Store.DB().QueryRow(
			`SELECT COUNT(*) FROM audit_logs WHERE action='file_video_clean_parts' AND ok=1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("清理必须写一条成功审计，实际 %d 条", n)
		}
	})

	t.Run("⑤ 不传 paths = 清空 output/ 下全部半成品", func(t *testing.T) {
		rec := clean(t, map[string]any{"dir": dir})
		if rec.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		if parts, _, _ := planOf(t); len(parts) != 0 {
			t.Fatalf("应已清空，实际还剩 %+v", parts)
		}
	})
}

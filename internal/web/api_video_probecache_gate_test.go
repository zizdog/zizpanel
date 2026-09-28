package web

// api_video_probecache_gate_test.go —— 「改配置不再重探」的唯一门禁。
//
// 为什么现有门禁抓不到：TestVideoCompressGate 只断言 planner 判据（码率封顶 / 不放大 /
// 产物更大就删）与"探测在任务里"，TestVideoOptionsGate 只断言"选项 → ffmpeg 命令行"；
// 两条注入的假 Runner 都只被断言**返回了什么**，从来没断言**被调用了几次**。
// 于是"用户每改一处配置，服务端就把目录里每个视频重跑一遍 ffprobe"这个性能缺陷
// 完全没有判据 —— 它只在真机上以"动任何一处都要重新『正在读取目录与视频信息…』"
// 的体验暴露（用户报障）。
//
// 一条门禁覆盖整类：在**同一个 Server** 上连打两次 /video-plan（缓存必须挂在 Server
// 上才活得过一次请求 —— web 层每次请求都新建 Manager 是本项目已知陷阱），用假 Runner
// 数真实探测次数。带两向对照：去掉缓存判据后，同一条断言必须不再成立。
// 全用 t.TempDir() 与假 Runner：不跑 ffmpeg、不读用户真实文件。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/videoopt"
)

// videoPlanData 是门禁只关心的那几个响应字段。
type videoPlanData struct {
	Available bool            `json:"available"`
	Preset    string          `json:"preset"`
	KBps      int             `json:"kbps"`
	Probed    int             `json:"probed"`
	Runnable  int             `json:"runnable"`
	Rows      []videoopt.Plan `json:"rows"`
}

// postVideoPlanData 直接调 handler（绕过鉴权，与既有 API 单测同一做法）。
func postVideoPlanData(t *testing.T, s *Server, body map[string]any) videoPlanData {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/video-plan", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	s.handleFileVideoPlan(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("video-plan 必须 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data videoPlanData `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, rec.Body.String())
	}
	return out.Data
}

// TestVideoProbeCacheGate 是这一条门禁。
func TestVideoProbeCacheGate(t *testing.T) {
	srv, _ := newTestServer(t)
	fake := &fakeVideoRunner{outBytes: 100, infos: map[string]videoopt.MediaInfo{}}
	withFakeVideoRunner(t, fake)

	// mkDir 造一个目录（视频固定 4000 字节；假 Runner 只按脚本回答，不读内容）。
	mkDir := func(s *Server, name string, files ...string) string {
		t.Helper()
		dir := filepath.Join(s.Cfg.WWWRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			p := filepath.Join(dir, f)
			if err := os.WriteFile(p, bytes.Repeat([]byte{2}, 4000), 0o644); err != nil {
				t.Fatal(err)
			}
			fake.infos[p] = videoopt.MediaInfo{
				Width: 1280, Height: 720, DurationSec: 3, FileBytes: 4000,
				VideoKbps: 4000, HasVideo: true, Codec: "h264",
			}
		}
		return dir
	}

	// ① 同一目录同一组文件：第一次探测 N 次；只改配置再请求 ⇒ 0 次，且计划内容正确。
	t.Run("① 首次探测 N 次；只改配置再请求 0 次（负向对照：没有缓存必须重探）", func(t *testing.T) {
		dir := mkDir(srv, "cacheA", "a.mp4", "b.mp4", "c.mkv", "notes.txt")
		first := postVideoPlanData(t, srv, map[string]any{"dir": dir, "preset": "480p"})
		// notes.txt 不是视频：N=3 同时证明"只对视频跑 ffprobe"。
		if first.Probed != 3 || len(first.Rows) != 3 {
			t.Fatalf("首次应探测 3 个视频（非视频不探），实际 probed=%d rows=%d", first.Probed, len(first.Rows))
		}
		before := fake.probeCount.Load()
		second := postVideoPlanData(t, srv, map[string]any{
			"dir": dir, "preset": "720p", "kbps": 1200, "encoder": "hardware", "mode": "bitrate",
		})
		if delta := fake.probeCount.Load() - before; delta != 0 {
			t.Fatalf("只改配置（档位/码率/编码器）不该再探测，实际又探了 %d 次", delta)
		}
		if second.Probed != 0 {
			t.Errorf("响应里的 probed 应如实为 0，实际 %d", second.Probed)
		}
		// 缓存只省探测，**绝不改变判据**：新配置必须真的生效且计划内容正确。
		if second.Preset != "720p" || second.KBps != 1200 {
			t.Errorf("改配置没生效：preset=%s kbps=%d", second.Preset, second.KBps)
		}
		if second.Runnable != 3 {
			t.Errorf("3 个都可压，实际 runnable=%d", second.Runnable)
		}
		for _, r := range second.Rows {
			if r.TargetWidth > 1280 || r.TargetHeight > 720 {
				t.Errorf("计划里放大了：%dx%d", r.TargetWidth, r.TargetHeight)
			}
			if r.VideoKbps != 1200 {
				t.Errorf("%s 的目标码率应为 1200，实际 %d", r.Name, r.VideoKbps)
			}
		}

		// 负向对照：同一条断言，去掉缓存判据（probeCache = nil）必须不再成立。
		srvNo, _ := newTestServer(t)
		srvNo.probeCache = nil
		dirNo := mkDir(srvNo, "cacheA", "a.mp4", "b.mp4", "c.mkv")
		postVideoPlanData(t, srvNo, map[string]any{"dir": dirNo, "preset": "480p"})
		beforeNo := fake.probeCount.Load()
		postVideoPlanData(t, srvNo, map[string]any{"dir": dirNo, "preset": "720p", "kbps": 1200})
		if delta := fake.probeCount.Load() - beforeNo; delta == 0 {
			t.Fatal("负向对照失效：没有缓存却还是 0 次探测，这条门禁抓不到任何东西")
		}
	})

	// ② 改某个文件的 size 或 mtime ⇒ 只重探那个文件（不是全量）。
	t.Run("② 文件 size/mtime 变了 ⇒ 只重探那一个", func(t *testing.T) {
		dir := mkDir(srv, "cacheB", "a.mp4", "b.mp4", "c.mkv")
		postVideoPlanData(t, srv, map[string]any{"dir": dir, "preset": "480p"})

		// 只改 mtime（大小不变）⇒ b.mp4 必须重探，其余命中。
		b := filepath.Join(dir, "b.mp4")
		stB, err := os.Stat(b)
		if err != nil {
			t.Fatal(err)
		}
		future := stB.ModTime().Add(2 * time.Second)
		if err := os.Chtimes(b, future, future); err != nil {
			t.Fatal(err)
		}
		before := fake.probeCount.Load()
		postVideoPlanData(t, srv, map[string]any{"dir": dir, "preset": "360p"})
		if delta := fake.probeCount.Load() - before; delta != 1 {
			t.Fatalf("只改了 1 个文件的 mtime，应只重探 1 个，实际 %d 个", delta)
		}

		// 只改 size（把 mtime 拨回原值）⇒ a.mp4 必须重探：size 也是缓存键的一部分。
		a := filepath.Join(dir, "a.mp4")
		stA, err := os.Stat(a)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(a, bytes.Repeat([]byte{3}, 6000), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(a, stA.ModTime(), stA.ModTime()); err != nil {
			t.Fatal(err)
		}
		restored, err := os.Stat(a)
		if err != nil {
			t.Fatal(err)
		}
		if restored.Size() == stA.Size() || !restored.ModTime().Equal(stA.ModTime()) {
			t.Fatalf("夹具没造对：size=%d mtime=%v", restored.Size(), restored.ModTime())
		}
		before = fake.probeCount.Load()
		postVideoPlanData(t, srv, map[string]any{"dir": dir, "preset": "480p"})
		if delta := fake.probeCount.Load() - before; delta != 1 {
			t.Fatalf("只改了 1 个文件的 size，应只重探 1 个，实际 %d 个", delta)
		}
	})

	// ③ TTL 过期 ⇒ 重新探测（绝不把很久以前的结论当成现在的）。
	t.Run("③ TTL 过期 ⇒ 重新探测", func(t *testing.T) {
		srvTTL, _ := newTestServer(t)
		srvTTL.probeCache = videoopt.NewProbeCache(30*time.Millisecond, 100)
		dir := mkDir(srvTTL, "cacheC", "a.mp4", "b.mp4")
		if got := postVideoPlanData(t, srvTTL, map[string]any{"dir": dir, "preset": "480p"}); got.Probed != 2 {
			t.Fatalf("首次应探测 2 个，实际 %d", got.Probed)
		}
		if got := postVideoPlanData(t, srvTTL, map[string]any{"dir": dir, "preset": "480p"}); got.Probed != 0 {
			t.Fatalf("TTL 内应命中缓存，实际探测 %d", got.Probed)
		}
		time.Sleep(60 * time.Millisecond)
		if got := postVideoPlanData(t, srvTTL, map[string]any{"dir": dir, "preset": "480p"}); got.Probed != 2 {
			t.Fatalf("TTL 过期后必须重探 2 个，实际 %d", got.Probed)
		}
	})

	// ④ 「⟳ 重新扫描」（rescan=true）⇒ 显式让该目录失效并重探。
	t.Run("④ rescan=true ⇒ 重新探测", func(t *testing.T) {
		dir := mkDir(srv, "cacheD", "a.mp4", "b.mp4")
		postVideoPlanData(t, srv, map[string]any{"dir": dir, "preset": "480p"})
		if got := postVideoPlanData(t, srv, map[string]any{"dir": dir, "preset": "480p"}); got.Probed != 0 {
			t.Fatalf("第二次应命中缓存，实际 %d", got.Probed)
		}
		before := fake.probeCount.Load()
		got := postVideoPlanData(t, srv, map[string]any{"dir": dir, "preset": "480p", "rescan": true})
		if delta := fake.probeCount.Load() - before; delta != 2 || got.Probed != 2 {
			t.Fatalf("重新扫描必须重探 2 个（delta=%d probed=%d）", delta, got.Probed)
		}
	})

	// ⑤ 压缩执行也复用同一份缓存（用户点「开始压缩」不再重探一遍）。
	t.Run("⑤ 压缩任务复用缓存 ⇒ 0 次额外探测", func(t *testing.T) {
		dir := mkDir(srv, "cacheE", "a.mp4", "b.mp4")
		postVideoPlanData(t, srv, map[string]any{"dir": dir, "preset": "480p"})
		before := fake.probeCount.Load()
		rec := postVideoJSON(t, srv.handleFileVideoCompress, map[string]any{"dir": dir, "preset": "480p"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("必须 202：%d %s", rec.Code, rec.Body.String())
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
		case <-time.After(10 * time.Second):
			t.Fatal("任务没结束")
		}
		if got := task.Status(); got != "succeeded" {
			t.Fatalf("任务状态 %s，错误=%v", got, task.Meta().Error)
		}
		if delta := fake.probeCount.Load() - before; delta != 0 {
			t.Fatalf("压缩任务不该重新探测（缓存已热），实际 %d 次", delta)
		}
	})

	// ⑥ 顺带收进缓存的第二个昂贵动作：`ffmpeg -version`（每次请求 spawn 一个进程）。
	//    热路径上它反而成了最大开销，所以版本串也进进程级缓存。
	t.Run("⑥ 引擎版本串不再每次 spawn（负向对照：清掉版本缓存必须重读）", func(t *testing.T) {
		srvV, _ := newTestServer(t)
		dir := mkDir(srvV, "cacheF", "a.mp4")
		postVideoPlanData(t, srvV, map[string]any{"dir": dir, "preset": "480p"})
		before := fake.versionCount.Load()
		postVideoPlanData(t, srvV, map[string]any{"dir": dir, "preset": "720p"})
		if delta := fake.versionCount.Load() - before; delta != 0 {
			t.Fatalf("改配置不该再 spawn `ffmpeg -version`，实际 %d 次", delta)
		}
		// 负向对照：清掉版本缓存（engineVer = nil = 每次都问）⇒ 同一断言必须不成立。
		srvNoVer, _ := newTestServer(t)
		srvNoVer.engineVer = nil
		dirNo := mkDir(srvNoVer, "cacheF", "a.mp4")
		postVideoPlanData(t, srvNoVer, map[string]any{"dir": dirNo, "preset": "480p"})
		beforeNo := fake.versionCount.Load()
		postVideoPlanData(t, srvNoVer, map[string]any{"dir": dirNo, "preset": "720p"})
		if delta := fake.versionCount.Load() - beforeNo; delta == 0 {
			t.Fatal("负向对照失效：没有版本缓存却还是 0 次 spawn")
		}
	})
}

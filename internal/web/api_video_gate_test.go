package web

// api_video_gate_test.go —— 「🎬 压缩视频」的唯一门禁。
//
// 为什么现有门禁抓不到：视频压缩是**新功能**，此前仓库里没有任何东西会规划
// "码率/分辨率封顶"，也没有任何地方会在产物变大时删掉产物 —— 图片压缩那条链路
// （internal/imgopt）管的是另一件事（质量/最长边），既不看原视频码率，
// 也不会因为"压完更大"之外的码率越界问题失败（它压的是图，不是码率）。
// 没有这条门禁，"请求 2000kbps、原视频只有 800kbps"就会真的产出比原片更大的文件。
//
// 一条门禁覆盖整类问题：纯 planner 的三条负向对照（码率封顶 / 不放大 /
// 产物更大就删掉）+ 两个 HTTP 接口的根白名单校验 + 接口时序（探测在任务里，
// 202 不许等探测，且两次探测之间文件变了就如实跳过）+ 百分比边界
// （0 字节源不出现 NaN/Infinity）。全用 t.TempDir() 与假 Runner，不跑真 ffmpeg、不碰真实目录。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/videoopt"
)

// fakeVideoRunner 是执行器的测试替身：不跑 ffmpeg，只按脚本写文件。
type fakeVideoRunner struct {
	infos map[string]videoopt.MediaInfo
	// outBytes 是"转码"产物的大小（用来构造"产物比原文件大"的场景）。
	outBytes int64
	probeErr error
	// probeDelay 让探测变慢：门禁用它证明"接口没有在返回前做耗时探测"。
	probeDelay time.Duration
	// probeCount 数**真的探测了几次**（缓存命中不算）：探测缓存门禁的唯一判据。
	// 用原子量：压缩任务在后台 goroutine 里探测，测试在任务结束后读取。
	probeCount atomic.Int64
	// versionCount 数 `ffmpeg -version` 被 spawn 了几次（版本串缓存门禁用）。
	versionCount atomic.Int64
	// reqs 记录每次转码的请求（证明执行期仍走同一份 planner）。
	reqs []videoopt.TranscodeRequest
}

func (f *fakeVideoRunner) Available() error { return nil }

func (f *fakeVideoRunner) Version(context.Context) string {
	f.versionCount.Add(1)
	return "fake-1.0"
}

func (f *fakeVideoRunner) Probe(_ context.Context, path string) (videoopt.MediaInfo, error) {
	f.probeCount.Add(1)
	if f.probeDelay > 0 {
		time.Sleep(f.probeDelay)
	}
	if f.probeErr != nil {
		return videoopt.MediaInfo{}, f.probeErr
	}
	if info, ok := f.infos[path]; ok {
		return info, nil
	}
	// 目录可能被解析过（/tmp → /private/tmp）：按文件名再找一次，
	// 否则夹具会静默失效、退回下面的默认值（曾经因此误判"可压"）。
	base := filepath.Base(path)
	for p, info := range f.infos {
		if filepath.Base(p) == base {
			return info, nil
		}
	}
	return videoopt.MediaInfo{
		Width: 640, Height: 360, DurationSec: 2, FileBytes: 4000,
		VideoKbps: 800, AudioKbps: 128, HasVideo: true, HasAudio: true,
	}, nil
}

func (f *fakeVideoRunner) Transcode(_ context.Context, req videoopt.TranscodeRequest, onProgress func(videoopt.Progress)) error {
	f.reqs = append(f.reqs, req)
	if onProgress != nil {
		onProgress(videoopt.Progress{Percent: 50, Speed: "2x"})
		onProgress(videoopt.Progress{Percent: 100, Speed: "2x"})
	}
	return os.WriteFile(req.Dst, bytes.Repeat([]byte{7}, int(f.outBytes)), 0o644)
}

// withFakeVideoRunner 把假执行器装进 web 层（handler 与任务体都用它）。
func withFakeVideoRunner(t *testing.T, r videoopt.Runner) {
	t.Helper()
	prev := videoRunnerOverride
	videoRunnerOverride = r
	t.Cleanup(func() { videoRunnerOverride = prev })
}

// postVideoJSON 直接调 handler（绕过鉴权，与既有 API 单测同一做法）。
func postVideoJSON(t *testing.T, h http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/video-plan", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// TestVideoCompressGate 是这一条门禁。
func TestVideoCompressGate(t *testing.T) {
	preset720, ok := videoopt.FindPreset("720p")
	if !ok {
		t.Fatal("预设 720p 不存在")
	}
	preset360, ok := videoopt.FindPreset("360p")
	if !ok {
		t.Fatal("预设 360p 不存在")
	}
	preset480, _ := videoopt.FindPreset("480p")

	t.Run("① 选的码率高于原视频时封顶即跳过（不再照压）", func(t *testing.T) {
		info := videoopt.MediaInfo{
			Width: 1280, Height: 720, DurationSec: 10, FileBytes: 5 << 20,
			VideoKbps: 800, AudioKbps: 128, HasVideo: true, HasAudio: true,
		}
		p := videoopt.PlanOne("a.mp4", "/tmp/a.mp4", "/tmp/out", info, videoopt.Options{Preset: preset720, KBps: 2000, Mode: videoopt.ModeBitrate}, false)
		// 负向对照：改前是"照压 + 警告"（SkipReason 为空）；现在封顶即跳过。
		if p.SkipReason == "" || !strings.Contains(p.SkipReason, "码率") {
			t.Fatalf("被原码率封顶的文件必须标跳过（原因含「码率」），实际 %q", p.SkipReason)
		}
		if !p.Capped || !p.PlaceInOutput {
			t.Errorf("必须标 Capped 且要原样放进 output：Capped=%v PlaceInOutput=%v", p.Capped, p.PlaceInOutput)
		}
		// 负向对照的另一半：请求码率低于原片×0.95 时仍应可压，且绝不超原码率。
		q := videoopt.PlanOne("a.mp4", "/tmp/a.mp4", "/tmp/out", info, videoopt.Options{Preset: preset720, KBps: 400, Mode: videoopt.ModeBitrate}, false)
		if q.SkipReason != "" {
			t.Fatalf("请求码率 400 < 800×0.95 时不该跳过，实际 %q", q.SkipReason)
		}
		if float64(q.VideoKbps) > float64(info.VideoKbps)*0.95+0.001 {
			t.Errorf("实际码率 %d kbps 超过了原码率×0.95", q.VideoKbps)
		}
		if q.AudioKbps != 96 {
			t.Errorf("音频码率应封顶到 96 kbps（原 128），实际 %d", q.AudioKbps)
		}
	})

	t.Run("② 原视频是 360p 却选 720p 时绝不放大", func(t *testing.T) {
		cases := []struct {
			name         string
			w, h         int
			preset       videoopt.Preset
			wantW, wantH int
		}{
			{"横屏 360p 选 720p", 640, 360, preset720, 640, 360},
			{"竖屏 360p 选 720p", 360, 640, preset720, 360, 640},
			{"1080p 横屏选 360p", 1920, 1080, preset360, 640, 360},
			{"竖屏 1080x1920 选 720p（按宽度封顶）", 1080, 1920, preset720, 720, 1280},
			{"奇数尺寸也要偶数", 641, 361, preset480, 640, 360},
		}
		for _, c := range cases {
			info := videoopt.MediaInfo{
				Width: c.w, Height: c.h, DurationSec: 5, FileBytes: 1 << 20,
				VideoKbps: 2000, AudioKbps: 0, HasVideo: true,
			}
			p := videoopt.PlanOne("v.mp4", "/tmp/v.mp4", "/tmp/out", info, videoopt.Options{Preset: c.preset, Mode: videoopt.ModeBitrate}, false)
			if p.SkipReason != "" {
				t.Fatalf("%s：不该跳过（%s）", c.name, p.SkipReason)
			}
			if p.TargetWidth > c.w || p.TargetHeight > c.h {
				t.Errorf("%s：目标 %dx%d 超过了原尺寸 %dx%d（放大了）",
					c.name, p.TargetWidth, p.TargetHeight, c.w, c.h)
			}
			if p.TargetWidth%2 != 0 || p.TargetHeight%2 != 0 {
				t.Errorf("%s：目标 %dx%d 不是偶数", c.name, p.TargetWidth, p.TargetHeight)
			}
			if p.TargetWidth != c.wantW || p.TargetHeight != c.wantH {
				t.Errorf("%s：目标 %dx%d，期望 %dx%d", c.name, p.TargetWidth, p.TargetHeight, c.wantW, c.wantH)
			}
			if p.AudioDisabled != !info.HasAudio {
				t.Errorf("%s：无音轨必须 -an（AudioDisabled=%v）", c.name, p.AudioDisabled)
			}
		}
	})

	t.Run("③ 产物比原文件大时必须删掉产物并记为跳过", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "big.mp4")
		if err := os.WriteFile(src, bytes.Repeat([]byte{1}, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		outDir := filepath.Join(dir, "output")
		runner := &fakeVideoRunner{
			outBytes: 5000, // 产物 5000 > 原 1000
			infos: map[string]videoopt.MediaInfo{src: {
				Width: 1280, Height: 720, DurationSec: 3, FileBytes: 1000,
				VideoKbps: 2000, AudioKbps: 128, HasVideo: true, HasAudio: true,
			}},
		}
		row := videoopt.PlanOne("big.mp4", src, outDir, runner.infos[src], videoopt.Options{Preset: preset480, KBps: 800, Mode: videoopt.ModeBitrate}, false)
		if !row.Runnable() {
			t.Fatalf("计划本身应该是可压的：%s", row.SkipReason)
		}
		res, err := videoopt.RunPlan(context.Background(), outDir, []videoopt.Plan{row}, runner, videoopt.Hooks{})
		if err != nil {
			t.Fatalf("压不下不该算任务失败（要如实记为跳过）：%v", err)
		}
		if res.Done != 0 || res.Skipped != 1 {
			t.Fatalf("应记为 1 个跳过、0 个完成，实际 done=%d skipped=%d failed=%d",
				res.Done, res.Skipped, res.Failed)
		}
		if res.Items[0].SkipReason == "" {
			t.Error("跳过必须带原因（面板要如实显示）")
		}
		// 那个 5000 字节的产物必须被删掉；output 里那份是**源文件本身**
		// （原样放进来的硬链接，1000 字节），不是更大的转码产物。
		st, serr := os.Stat(row.OutPath)
		if serr != nil {
			t.Fatalf("跳过的源文件应被原样放进 output：%v", serr)
		}
		if st.Size() != 1000 {
			t.Errorf("output 里必须是原文件（1000 字节），实际 %d", st.Size())
		}
		srcSt, _ := os.Stat(src)
		if !os.SameFile(srcSt, st) {
			t.Error("output 里那份应该是源文件的硬链接")
		}
		if _, err := os.Stat(row.OutPath + ".part.mp4"); !os.IsNotExist(err) {
			t.Errorf("半成品也必须清掉：%s.part.mp4", row.OutPath)
		}
	})

	t.Run("④ 接口：根白名单外的目录必须被拒", func(t *testing.T) {
		srv, _ := newTestServer(t)
		withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100})

		// /etc 不在白名单里（默认根只有 www / 家目录 / 安装根 / brew etc / 卷）。
		for name, h := range map[string]http.HandlerFunc{
			"video-plan":     srv.handleFileVideoPlan,
			"video-compress": srv.handleFileVideoCompress,
		} {
			rec := postVideoJSON(t, h, map[string]any{"dir": "/etc", "preset": "480p"})
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s：白名单外的目录必须是 403，实际 %d（body=%s）", name, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("⑤ 接口：档位/码率非法必须 400，合法则 202+task_id", func(t *testing.T) {
		srv, _ := newTestServer(t)
		fake := &fakeVideoRunner{outBytes: 100}
		withFakeVideoRunner(t, fake)

		dir := filepath.Join(srv.Cfg.WWWRoot, "videos")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(dir, "a.mp4")
		if err := os.WriteFile(src, bytes.Repeat([]byte{2}, 4000), 0o644); err != nil {
			t.Fatal(err)
		}
		fake.infos = map[string]videoopt.MediaInfo{src: {
			Width: 640, Height: 360, DurationSec: 2, FileBytes: 4000,
			VideoKbps: 2000, AudioKbps: 128, HasVideo: true, HasAudio: true,
		}}

		rec := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "preset": "4k"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("不存在的档位必须 400，实际 %d", rec.Code)
		}
		// 1080p 与「原始」现在是**合法档位**（新增）：不许再被当成未知档位。
		for _, id := range []string{"1080p", "source"} {
			if r2 := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "preset": id}); r2.Code != http.StatusOK {
				t.Errorf("档位 %s 必须合法（200），实际 %d", id, r2.Code)
			}
		}
		rec = postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "preset": "480p", "kbps": 5})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("越界码率必须 400，实际 %d", rec.Code)
		}

		// 计划：360p 选 720p 也不能放大（显式走目标码率模式，体积可预估）。
		rec = postVideoJSON(t, srv.handleFileVideoPlan,
			map[string]any{"dir": dir, "preset": "720p", "mode": "bitrate"})
		if rec.Code != http.StatusOK {
			t.Fatalf("video-plan 应该成功：%d %s", rec.Code, rec.Body.String())
		}
		var planBody struct {
			OK   bool `json:"ok"`
			Data struct {
				Available bool            `json:"available"`
				Runnable  int             `json:"runnable"`
				Note      string          `json:"note"`
				EstBytes  int64           `json:"est_bytes"`
				Rows      []videoopt.Plan `json:"rows"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &planBody); err != nil {
			t.Fatalf("计划响应不是 JSON: %v", err)
		}
		if !planBody.Data.Available || planBody.Data.Runnable != 1 || len(planBody.Data.Rows) != 1 {
			t.Fatalf("计划应该给出 1 个可压行：%s", rec.Body.String())
		}
		row := planBody.Data.Rows[0]
		if row.TargetWidth > 640 || row.TargetHeight > 360 {
			t.Errorf("计划里放大了：%dx%d", row.TargetWidth, row.TargetHeight)
		}
		// 提示文案是用户可改的东西 ⇒ 门禁只断言**行为**：目标码率模式给得出体积预估与一句说明。
		if planBody.Data.Note == "" || planBody.Data.EstBytes <= 0 || row.EstBytes <= 0 {
			t.Errorf("目标码率模式必须给体积预估与一句说明，实际 note=%q est=%d row=%d",
				planBody.Data.Note, planBody.Data.EstBytes, row.EstBytes)
		}

		// 执行：202 + task_id，并且任务真的按同一个计划跑完（假 Runner，不碰真 ffmpeg）。
		rec = postVideoJSON(t, srv.handleFileVideoCompress, map[string]any{"dir": dir, "preset": "480p"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("video-compress 必须 202 + task_id，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var accepted struct {
			OK   bool `json:"ok"`
			Data struct {
				TaskID string `json:"task_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.Data.TaskID == "" {
			t.Fatalf("没有拿到 task_id：%s", rec.Body.String())
		}
		task := srv.Tasks.Get(accepted.Data.TaskID)
		if task == nil {
			t.Fatal("任务不存在（202 之后必须能查到）")
		}
		select {
		case <-task.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("任务在 10 秒内没有结束（假 Runner 不该卡住）")
		}
		if got := task.Status(); got != "succeeded" {
			t.Fatalf("任务状态 %s，错误=%v", got, task.Meta().Error)
		}
		if _, err := os.Stat(filepath.Join(dir, videoopt.OutputDirName, "a.480p.mp4")); err != nil {
			t.Errorf("产物没落在 output/ 里：%v", err)
		}
	})

	// ⑥ 是本次新增的那一类（用户实测的空档）：**接口不许在返回前做耗时探测**。
	//
	// 为什么此前两条视频门禁都抓不到：TestVideoCompressGate 只断言 planner 判据
	// （码率封顶/不放大/产物更大就删）与白名单校验，TestVideoOptionsGate 只断言
	// "选项 → ffmpeg 命令行"；两者注入的假 Runner 的 Probe 都是**瞬时返回**的，
	// 于是"请求里同步跑 N 次 ffprobe 才回 202"这个时序缺陷完全没有判据 ——
	// 它只在真机上以"面板消失后干等"的体验暴露。
	t.Run("⑥ 接口必须在探测之前返回 202（探测在任务里）", func(t *testing.T) {
		srv, _ := newTestServer(t)
		dir := filepath.Join(srv.Cfg.WWWRoot, "slowvideos")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		fake := &fakeVideoRunner{
			outBytes:   1000,
			probeDelay: 800 * time.Millisecond, // 故意慢：同步探测会 ≥1.6s
			infos:      map[string]videoopt.MediaInfo{},
		}
		for _, name := range []string{"a.mp4", "b.mp4"} {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, bytes.Repeat([]byte{2}, 4000), 0o644); err != nil {
				t.Fatal(err)
			}
			fake.infos[p] = videoopt.MediaInfo{
				Width: 1280, Height: 720, DurationSec: 3, FileBytes: 4000,
				VideoKbps: 4000, AudioKbps: 128, HasVideo: true, HasAudio: true,
			}
		}
		withFakeVideoRunner(t, fake)

		started := time.Now()
		// kbps=2000 + 目标码率：执行期必须走同一份 planner（源 4000 kbps，选 2000 不封顶）。
		rec := postVideoJSON(t, srv.handleFileVideoCompress,
			map[string]any{"dir": dir, "preset": "480p", "kbps": 2000, "mode": "bitrate"})
		elapsed := time.Since(started)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("video-compress 必须 202，实际 %d：%s", rec.Code, rec.Body.String())
		}
		// 负向对照：把 BuildPlan 挪回请求里（改回同步探测）这里会 ≥1.6s ⇒ 变红。
		if elapsed > 500*time.Millisecond {
			t.Errorf("POST 花了 %v 才返回 202（>500ms）：探测又跑回请求里了", elapsed)
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
			t.Fatal("任务不存在（202 之后必须能查到）")
		}
		select {
		case <-task.Done():
		case <-time.After(20 * time.Second):
			t.Fatal("任务没结束")
		}
		if got := task.Status(); got != "succeeded" {
			t.Fatalf("任务状态 %s，错误=%v", got, task.Meta().Error)
		}
		if len(fake.reqs) != 2 {
			t.Fatalf("应该有 2 次转码，实际 %d", len(fake.reqs))
		}
		want := 2000
		for _, r := range fake.reqs {
			if r.VideoKbps != want {
				t.Errorf("执行期码率 %d，期望 planner 定的 %d", r.VideoKbps, want)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, videoopt.OutputDirName, "a.480p.mp4")); err != nil {
			t.Errorf("按计划执行后应有产物：%v", err)
		}
	})

	// ⑦ 百分比边界：源 0 字节 / 读不到大小时不许出现 NaN% 或 -Infinity%。
	t.Run("⑦ 百分比：0 字节源不出现 NaN/Infinity", func(t *testing.T) {
		cases := []struct {
			before, after int64
			want          string
		}{
			{0, 0, ""},   // 源 0 字节：只说体积，不算百分比
			{0, 100, ""}, // 源读不到
			{100, 0, ""}, // 产物读不到
			{1000, 500, "-50.0%"},
			{1000, 990, "-1.0%"},
			{1000, 1000, ""}, // 没变小：不写 -0.0%
		}
		for _, c := range cases {
			if got := videoopt.FormatSavedPercent(c.before, c.after); got != c.want {
				t.Errorf("FormatSavedPercent(%d, %d) = %q，期望 %q", c.before, c.after, got, c.want)
			}
			if txt := videoopt.SummarySavedText(c.before, c.after); strings.Contains(txt, "NaN") || strings.Contains(txt, "Inf") {
				t.Errorf("汇总文本出现了 NaN/Inf：%q", txt)
			}
		}

		// 端到端：0 字节源跑完任务，进度窗的汇总文本里也不许有 NaN/Inf。
		srv, _ := newTestServer(t)
		dir := filepath.Join(srv.Cfg.WWWRoot, "zerovideos")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		zero := filepath.Join(dir, "zero.mp4")
		if err := os.WriteFile(zero, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		fake := &fakeVideoRunner{outBytes: 0, infos: map[string]videoopt.MediaInfo{zero: {
			Width: 640, Height: 360, DurationSec: 2, FileBytes: 0,
			VideoKbps: 800, AudioKbps: 0, HasVideo: true,
		}}}
		withFakeVideoRunner(t, fake)
		rec := postVideoJSON(t, srv.handleFileVideoCompress, map[string]any{"dir": dir, "preset": "480p"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("必须 202（0 字节源也要开任务如实说）：%d %s", rec.Code, rec.Body.String())
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
		select {
		case <-task.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("任务没结束")
		}
		if p := task.Progress(); p != nil {
			if strings.Contains(p.Message, "NaN") || strings.Contains(p.Message, "Inf") {
				t.Errorf("进度窗文本出现了 NaN/Inf：%q", p.Message)
			}
		}
	})

	// ⑧ 规划搬进任务后必须核对"两次探测之间文件变了吗"：变了/不见了如实跳过，
	//    绝不静默按旧计划压（用户点名）。
	t.Run("⑧ 两次探测之间文件变了/不见了就如实跳过", func(t *testing.T) {
		srv, _ := newTestServer(t)
		dir := filepath.Join(srv.Cfg.WWWRoot, "changed")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		mk := func(name string, n int) string {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, bytes.Repeat([]byte{3}, n), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}
		a, b := mk("a.mp4", 4000), mk("b.mp4", 4000)
		info := videoopt.MediaInfo{
			Width: 1280, Height: 720, DurationSec: 3, FileBytes: 4000,
			VideoKbps: 4000, AudioKbps: 0, HasVideo: true,
		}
		fake := &fakeVideoRunner{outBytes: 100, infos: map[string]videoopt.MediaInfo{a: info, b: info}}
		withFakeVideoRunner(t, fake)

		rec := postVideoJSON(t, srv.handleFileVideoCompress, map[string]any{
			"dir": dir, "preset": "480p",
			"sources": []map[string]any{
				{"name": "a.mp4", "bytes": 9999}, // 计划表说 9999，实际 4000 ⇒ 变了
				{"name": "b.mp4", "bytes": 4000}, // 一致 ⇒ 照压
				{"name": "gone.mp4", "bytes": 5000},
			},
		})
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
		select {
		case <-task.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("任务没结束")
		}
		if len(fake.reqs) != 1 {
			t.Fatalf("只该压没变过的 b.mp4，实际转码 %d 次", len(fake.reqs))
		}
		if filepath.Base(fake.reqs[0].Src) != "b.mp4" {
			t.Errorf("压错了文件：%s", fake.reqs[0].Src)
		}
		res, ok := task.Meta().Result.(*videoopt.RunResult)
		if !ok || res == nil {
			t.Fatalf("任务结果类型不对：%#v", task.Meta().Result)
		}
		reasons := map[string]string{}
		for _, it := range res.Items {
			reasons[it.Name] = it.SkipReason
		}
		if !strings.Contains(reasons["a.mp4"], "已变化") {
			t.Errorf("变过的文件必须如实说明跳过原因，实际 %q", reasons["a.mp4"])
		}
		if !strings.Contains(reasons["gone.mp4"], "已不存在") {
			t.Errorf("不见了的文件必须如实说明跳过原因，实际 %q", reasons["gone.mp4"])
		}
		if res.Done != 1 || res.Skipped != 2 {
			t.Errorf("应 1 完成 2 跳过，实际 done=%d skipped=%d", res.Done, res.Skipped)
		}
	})
}

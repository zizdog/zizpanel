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
// 产物更大就删掉）+ 两个 HTTP 接口的根白名单校验。
// 全用 t.TempDir() 与假 Runner，不跑真 ffmpeg、不碰真实目录。

import (
	"bytes"
	"context"
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

// fakeVideoRunner 是执行器的测试替身：不跑 ffmpeg，只按脚本写文件。
type fakeVideoRunner struct {
	infos map[string]videoopt.MediaInfo
	// outBytes 是"转码"产物的大小（用来构造"产物比原文件大"的场景）。
	outBytes int64
	probeErr error
}

func (f *fakeVideoRunner) Available() error { return nil }

func (f *fakeVideoRunner) Version(context.Context) string { return "fake-1.0" }

func (f *fakeVideoRunner) Probe(_ context.Context, path string) (videoopt.MediaInfo, error) {
	if f.probeErr != nil {
		return videoopt.MediaInfo{}, f.probeErr
	}
	if info, ok := f.infos[path]; ok {
		return info, nil
	}
	return videoopt.MediaInfo{
		Width: 640, Height: 360, DurationSec: 2, FileBytes: 4000,
		VideoKbps: 800, AudioKbps: 128, HasVideo: true, HasAudio: true,
	}, nil
}

func (f *fakeVideoRunner) Transcode(_ context.Context, req videoopt.TranscodeRequest, onProgress func(videoopt.Progress)) error {
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

	t.Run("① 选的码率高于原视频时必须被原码率封顶", func(t *testing.T) {
		info := videoopt.MediaInfo{
			Width: 1280, Height: 720, DurationSec: 10, FileBytes: 5 << 20,
			VideoKbps: 800, AudioKbps: 128, HasVideo: true, HasAudio: true,
		}
		p := videoopt.PlanOne("a.mp4", "/tmp/a.mp4", "/tmp/out", info, preset720, 2000, false)
		if p.SkipReason != "" {
			t.Fatalf("这个视频应该可压，却被跳过：%s", p.SkipReason)
		}
		// 负向对照：改前（直接信用户选的 2000kbps）这里会是 2000 > 760。
		if float64(p.VideoKbps) > float64(info.VideoKbps)*0.95+0.001 {
			t.Errorf("实际码率 %d kbps 超过了原码率×0.95（%d kbps）", p.VideoKbps, int(float64(info.VideoKbps)*0.95))
		}
		if !p.Capped || p.Note == "" {
			t.Errorf("因原码率低而封顶时必须如实说明：Capped=%v Note=%q", p.Capped, p.Note)
		}
		if p.AudioKbps != 96 {
			t.Errorf("音频码率应封顶到 96 kbps（原 128），实际 %d", p.AudioKbps)
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
			p := videoopt.PlanOne("v.mp4", "/tmp/v.mp4", "/tmp/out", info, c.preset, 0, false)
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
				VideoKbps: 800, AudioKbps: 128, HasVideo: true, HasAudio: true,
			}},
		}
		row := videoopt.PlanOne("big.mp4", src, outDir, runner.infos[src], preset480, 800, false)
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
		if _, err := os.Stat(row.OutPath); !os.IsNotExist(err) {
			t.Errorf("产物必须被删掉，实际还在：%s", row.OutPath)
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
			VideoKbps: 800, AudioKbps: 128, HasVideo: true, HasAudio: true,
		}}

		rec := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "preset": "1080p"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("不存在的档位必须 400，实际 %d", rec.Code)
		}
		rec = postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "preset": "480p", "kbps": 5})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("越界码率必须 400，实际 %d", rec.Code)
		}

		// 计划：360p 选 720p 也不能放大，且面板要拿到那句"产物只会更小"。
		rec = postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "preset": "720p"})
		if rec.Code != http.StatusOK {
			t.Fatalf("video-plan 应该成功：%d %s", rec.Code, rec.Body.String())
		}
		var planBody struct {
			OK   bool `json:"ok"`
			Data struct {
				Available bool            `json:"available"`
				Runnable  int             `json:"runnable"`
				Note      string          `json:"note"`
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
		if !strings.Contains(planBody.Data.Note, "产物只会更小") {
			t.Errorf("面板必须有那句「产物只会更小」的说明，实际 %q", planBody.Data.Note)
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
}

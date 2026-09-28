package web

// api_video_options_gate_test.go —— 视频压缩**选项补齐**的唯一门禁。
//
// 为什么现有门禁抓不到：`TestVideoCompressGate` 只断言了 planner 的
// 码率封顶 / 不放大 / "产物更大就删掉"，**从来没有断言"面板选项真的变成了
// ffmpeg 参数"** —— 编码器、质量优先、2-pass 这些新选项就算被 planner 丢掉、
// 或者被错传成别的参数，老门禁也全绿（它注入的假 Runner 根本不看参数）。
//
// 一条门禁覆盖整类：选项 → 命令行 的映射（TranscodeArgs）+ 请求经两个接口
// 真的透传到执行器 + 非法组合 400 + 面板警告/体积对比字段。
// **不跑真 ffmpeg**：argv 是纯函数产物，执行期用记录参数的假 Runner。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/videoopt"
)

// captureVideoRunner 记录每一次 Transcode 的请求（门禁据此断言"选项真的传下去了"）。
type captureVideoRunner struct {
	probe    videoopt.MediaInfo
	reqs     []videoopt.TranscodeRequest
	outBytes int64
}

func (c *captureVideoRunner) Available() error               { return nil }
func (c *captureVideoRunner) Version(context.Context) string { return "fake-1.0" }
func (c *captureVideoRunner) Probe(context.Context, string) (videoopt.MediaInfo, error) {
	return c.probe, nil
}

func (c *captureVideoRunner) Transcode(_ context.Context, req videoopt.TranscodeRequest, onProgress func(videoopt.Progress)) error {
	c.reqs = append(c.reqs, req)
	if onProgress != nil {
		onProgress(videoopt.Progress{Percent: 100, Speed: "9x"})
	}
	return os.WriteFile(req.Dst, bytes.Repeat([]byte{7}, int(c.outBytes)), 0o644)
}

// argsHave 断言 argv 里出现某个 flag（可选断言紧跟的值；value=="" 表示只看 flag）。
func argsHave(args []string, flag, value string) bool {
	for i, a := range args {
		if a != flag {
			continue
		}
		if value == "" {
			return true
		}
		if i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestVideoOptionsGate 是这一条门禁。
func TestVideoOptionsGate(t *testing.T) {
	t.Run("① 编码器/模式必须变成对应的 ffmpeg 参数", func(t *testing.T) {
		base := videoopt.TranscodeRequest{
			Src: "in.mp4", Dst: "out.mp4", Width: 854, Height: 480,
			VideoKbps: 800, AudioKbps: 96, DurationSec: 5,
		}
		cases := []struct {
			name    string
			req     videoopt.TranscodeRequest
			want    [][2]string // 必须出现
			notWant []string    // 必须不出现
		}{
			{
				name:    "默认（空字段）= CPU + 目标码率（实测默认）",
				req:     base,
				want:    [][2]string{{"-c:v", "libx264"}, {"-b:v", "800k"}},
				notWant: []string{"hevc_videotoolbox", "-crf", "-q:v", "-pass"},
			},
			{
				name:    "硬件 + 目标码率 = HEVC，码率参数与 CPU 时逐字相同",
				req:     withOpts(base, videoopt.EncoderHardware, videoopt.ModeBitrate, 0, false),
				want:    [][2]string{{"-c:v", "hevc_videotoolbox"}, {"-b:v", "800k"}, {"-maxrate", "800k"}},
				notWant: []string{"libx264", "-crf", "-q:v"},
			},
			{
				name:    "CPU + 目标码率",
				req:     withOpts(base, videoopt.EncoderCPU, videoopt.ModeBitrate, 0, false),
				want:    [][2]string{{"-c:v", "libx264"}, {"-preset", "veryfast"}, {"-b:v", "800k"}},
				notWant: []string{"hevc_videotoolbox", "-crf", "-q:v"},
			},
			{
				name:    "CPU + 质量优先 = -crf，且不许有 -b:v",
				req:     withOpts(base, videoopt.EncoderCPU, videoopt.ModeQuality, videoopt.DefaultCRF, false),
				want:    [][2]string{{"-c:v", "libx264"}, {"-crf", "26"}},
				notWant: []string{"-b:v", "-q:v", "hevc_videotoolbox"},
			},
			{
				name:    "硬件 + 质量优先 = -q:v（实测越大越好），且不许有 -b:v",
				req:     withOpts(base, videoopt.EncoderHardware, videoopt.ModeQuality, videoopt.DefaultVTQuality, false),
				want:    [][2]string{{"-c:v", "hevc_videotoolbox"}, {"-q:v", "45"}},
				notWant: []string{"-b:v", "-crf", "libx264"},
			},
		}
		for _, c := range cases {
			args := videoopt.TranscodeArgs(c.req, 0)
			for _, w := range c.want {
				if !argsHave(args, w[0], w[1]) {
					t.Errorf("%s：argv 缺少 %s %s\nargv=%v", c.name, w[0], w[1], args)
				}
			}
			for _, n := range c.notWant {
				if argsHave(args, n, "") {
					t.Errorf("%s：argv 不该出现 %s\nargv=%v", c.name, n, args)
				}
			}
		}
	})

	t.Run("② 2-pass 两遍都有 -pass，且各自带 passlogfile", func(t *testing.T) {
		req := withOpts(videoopt.TranscodeRequest{
			Src: "in.mp4", Dst: "out.mp4", Width: 854, Height: 480,
			VideoKbps: 800, AudioKbps: 96, DurationSec: 5,
		}, videoopt.EncoderCPU, videoopt.ModeBitrate, 0, true)
		req.PassLog = "/tmp/x/pass0"

		p1 := videoopt.TranscodeArgs(req, 1)
		p2 := videoopt.TranscodeArgs(req, 2)
		if !argsHave(p1, "-pass", "1") || !argsHave(p1, "-passlogfile", "/tmp/x/pass0") {
			t.Errorf("第一遍缺少 -pass 1 / -passlogfile：%v", p1)
		}
		if !argsHave(p2, "-pass", "2") || !argsHave(p2, "-passlogfile", "/tmp/x/pass0") {
			t.Errorf("第二遍缺少 -pass 2 / -passlogfile：%v", p2)
		}
		// 负向对照：单遍（pass=0）绝不能带 -pass。
		if single := videoopt.TranscodeArgs(withOpts(req, videoopt.EncoderCPU, videoopt.ModeBitrate, 0, false), 0); argsHave(single, "-pass", "") {
			t.Errorf("不开 2-pass 时 argv 出现了 -pass：%v", single)
		}
	})

	t.Run("③ 2-pass 与硬件/质量优先互斥：接口必须 400", func(t *testing.T) {
		srv, _ := newTestServer(t)
		withFakeVideoRunner(t, &captureVideoRunner{outBytes: 10})
		for name, body := range map[string]map[string]any{
			"2-pass + 硬件":   {"dir": "/tmp", "preset": "480p", "encoder": "hardware", "mode": "bitrate", "two_pass": true},
			"2-pass + 质量优先": {"dir": "/tmp", "preset": "480p", "encoder": "cpu", "mode": "quality", "two_pass": true},
			"未知编码器":         {"dir": "/tmp", "preset": "480p", "encoder": "gpu"},
			"未知模式":          {"dir": "/tmp", "preset": "480p", "mode": "fast"},
			"CRF 越界":        {"dir": "/tmp", "preset": "480p", "encoder": "cpu", "mode": "quality", "quality": 99},
		} {
			rec := postVideoJSON(t, srv.handleFileVideoPlan, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s：必须 400，实际 %d（body=%s）", name, rec.Code, rec.Body.String())
			}
		}
		// 负向对照的另一半：合法的 2-pass（CPU + 目标码率）不能被 400。
		rec := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{
			"dir": "/tmp", "preset": "480p", "encoder": "cpu", "mode": "bitrate", "two_pass": true,
		})
		if rec.Code == http.StatusBadRequest {
			t.Errorf("合法的 2-pass 被当成非法：%s", rec.Body.String())
		}
	})

	t.Run("④ 请求经接口透传到执行器，且带上面板警告/体积对比", func(t *testing.T) {
		srv, _ := newTestServer(t)
		// 原片 1200 kbps：用户选 800（480p 下限）时**不该**被判定为 capped；
		// 选 1200 时一定 capped（1200 ≥ 1200×0.95）—— 正是用户实测踩的那个坑。
		runner := &captureVideoRunner{
			probe: videoopt.MediaInfo{
				Width: 1280, Height: 720, DurationSec: 10, FileBytes: 2 << 20,
				VideoKbps: 1200, AudioKbps: 96, HasVideo: true, HasAudio: true, Codec: "h264",
			},
			outBytes: 1000,
		}
		withFakeVideoRunner(t, runner)

		dir := filepath.Join(srv.Cfg.WWWRoot, "opts")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(dir, "a.mp4")
		if err := os.WriteFile(src, bytes.Repeat([]byte{3}, 4000), 0o644); err != nil {
			t.Fatal(err)
		}

		// 用户实测场景：480p + 码率填 1200（≥ 原片 1200×0.95）→ 必须标"封顶即跳过"。
		rec := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{
			"dir": dir, "preset": "480p", "kbps": 1200, "mode": "bitrate",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("video-plan 应该成功：%d %s", rec.Code, rec.Body.String())
		}
		var plan struct {
			Data struct {
				Warning       string `json:"warning"`
				WarningDetail string `json:"warning_detail"`
				CappedSkipped int    `json:"capped_skipped"`
				PlaceCount    int    `json:"place_count"`
				EstPercent    int    `json:"est_percent"`
				TotalSource   int64  `json:"total_source_bytes"`
				Rows          []struct {
					Capped      bool   `json:"capped"`
					SkipReason  string `json:"skip_reason"`
					PlaceInOut  bool   `json:"place_in_output"`
					SourceCodec string `json:"source_codec"`
					SourceBytes int64  `json:"source_bytes"`
					EstBytes    int64  `json:"est_bytes"`
				} `json:"rows"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
			t.Fatalf("计划响应不是 JSON: %v", err)
		}
		if plan.Data.CappedSkipped != 1 || !plan.Data.Rows[0].Capped {
			t.Fatalf("码率 1200 ≥ 原片 1200×0.95，必须标 capped 跳过：%s", rec.Body.String())
		}
		if !strings.Contains(plan.Data.Rows[0].SkipReason, "码率") || !plan.Data.Rows[0].PlaceInOut {
			t.Errorf("封顶的行必须标跳过（原因含「码率」）且要原样放进 output：%+v", plan.Data.Rows[0])
		}
		// 面板必须明确提醒"不会转码、但会原样放进 output"（细节走 warning_detail）。
		if plan.Data.Warning == "" || plan.Data.WarningDetail == "" {
			t.Errorf("必须有「码率已到极限」的提醒与细节，实际 %q / %q", plan.Data.Warning, plan.Data.WarningDetail)
		}
		if plan.Data.PlaceCount != 1 {
			t.Errorf("必须说明有 1 个会被原样放进 output，实际 %d", plan.Data.PlaceCount)
		}
		if !strings.Contains(plan.Data.Rows[0].SourceCodec, "h264") {
			t.Errorf("计划行必须带源编码（面板显示 H.264），实际 %q", plan.Data.Rows[0].SourceCodec)
		}
		// 同一场景选 800（< 原片 1200×0.95，不会被封顶）时：没有提醒，且体积对比字段有效。
		rec = postVideoJSON(t, srv.handleFileVideoPlan,
			map[string]any{"dir": dir, "preset": "480p", "kbps": 800, "mode": "bitrate"})
		var quiet struct {
			Data struct {
				Warning     string `json:"warning"`
				CappedSkip  int    `json:"capped_skipped"`
				TotalSource int64  `json:"total_source_bytes"`
				EstBytes    int64  `json:"est_bytes"`
				EstPercent  int    `json:"est_percent"`
				Rows        []struct {
					Capped   bool  `json:"capped"`
					EstBytes int64 `json:"est_bytes"`
				} `json:"rows"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &quiet)
		if quiet.Data.Warning != "" || quiet.Data.CappedSkip != 0 {
			t.Errorf("码率 800 < 原片 1200×0.95，不该有封顶提醒，实际 %q（capped=%d）",
				quiet.Data.Warning, quiet.Data.CappedSkip)
		}
		if quiet.Data.TotalSource != 2<<20 || quiet.Data.Rows[0].EstBytes <= 0 {
			t.Errorf("体积对比字段缺失：total_source_bytes=%d est_bytes=%d",
				quiet.Data.TotalSource, quiet.Data.Rows[0].EstBytes)
		}

		// 质量优先：计划阶段就要标"体积不可预估"、且不给假体积（前端显示 —）。
		rec = postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{
			"dir": dir, "preset": "480p", "encoder": "cpu", "mode": "quality",
		})
		var unknown struct {
			Data struct {
				EstimateUnknown bool  `json:"estimate_unknown"`
				EstBytes        int64 `json:"est_bytes"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &unknown)
		if !unknown.Data.EstimateUnknown || unknown.Data.EstBytes != 0 {
			t.Errorf("质量优先必须标 estimate_unknown 且不给假体积：%s", rec.Body.String())
		}

		// 再走一次压缩，断言选项真的到了执行器。
		rec = postVideoJSON(t, srv.handleFileVideoCompress, map[string]any{
			"dir": dir, "preset": "480p", "encoder": "cpu", "mode": "quality", "quality": 28,
		})
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
		case <-time.After(10 * time.Second):
			t.Fatal("任务超时")
		}
		if len(runner.reqs) != 1 {
			t.Fatalf("执行器应该收到 1 次转码请求，实际 %d", len(runner.reqs))
		}
		got := runner.reqs[0]
		if got.Encoder != videoopt.EncoderCPU || got.Mode != videoopt.ModeQuality || got.Quality != 28 {
			t.Errorf("选项没透传到执行器：%+v", got)
		}
		args := videoopt.TranscodeArgs(got, 0)
		if !argsHave(args, "-crf", "28") || argsHave(args, "-b:v", "") {
			t.Errorf("执行器拿到的 argv 不是质量优先：%v", args)
		}
		// 质量优先仍要保住硬规则：-maxrate 不超过原片码率×0.95（1200→1140）。
		if !argsHave(args, "-maxrate", "1140k") {
			t.Errorf("质量优先必须用 -maxrate 兜住不超原片码率，argv=%v", args)
		}
	})
}

// withOpts 复制一份请求并套上选项（避免每个 case 重复写字段）。
func withOpts(req videoopt.TranscodeRequest, enc, mode string, quality int, twoPass bool) videoopt.TranscodeRequest {
	req.Encoder, req.Mode, req.Quality, req.TwoPass = enc, mode, quality, twoPass
	return req
}

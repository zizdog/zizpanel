package videoopt

// decode_env_gate_test.go —— 「硬件档：解码策略 / 快路 argv / 三级回退 / 码率标定」的唯一门禁。
//
// 为什么改写（2026-10-05 本机 MacBook Air M4 / macOS 15.6.1 / ffmpeg 9.0.1_1 实测，
// 源 = 用户那段 4K60 10bit 25Mbps HEVC，目标 2592x1080，60 秒/40 秒片段，交错两轮）：
//
//	配置                                        墙钟(40s)  speed   实际码率
//	软解+软缩放+hevc_videotoolbox q:v45 上限24M   25~28s   1.5x    14845 kbps
//	同上 + 缩放前降到 30fps（软解仍瓶颈）          26~27s   1.5x    14808 kbps
//	硬解 + 软缩放 + 硬编（30fps/prio_speed）       8.1~8.4s 4.8x    15085 kbps
//	全 GPU：硬解+scale_vt+硬编（30fps/prio_speed） 5.0~5.3s 8.0x    15085 kbps
//	全 GPU + -b:v 4500k -maxrate 4500k            5.0~5.2s 8.0x     2968 kbps
//	libx264 veryfast -b:v 3500k（软解）            34s     1.15x     3073 kbps
//	libx265 medium -b:v 3500k（软解，20s 片段）      —      0.6x     3483 kbps
//
// 三条结论（都写进了代码注释）：
//  1. 硬解的价值不在"零拷贝"，而在"软件解码扛不住"：4K60 8bit 源软解 9.6~16.5x、硬解
//     只有 2.4x（硬解有固定吞吐上限），10bit 高码率源软解 2.4x、硬解 6.4x ⇒ 判据见
//     needFastDecode（4K 级 + 10bit↑ + ≥6Mbps 才硬解）。
//  2. ffmpeg 9.0.1 的 -hwaccel_output_format 取值是 **videotoolbox_vld**；写
//     `videotoolbox` 会被忽略（只是警告），随后 scale_vt 报 -22/-78 —— 旧注释里
//     "scale_vt 不可用"其实是这个拼写错的。
//  3. 降到 30fps 必须放在**缩放之前**：放在最后 5.98x，放最前 8.53x（少一半缩放+编码）。
//
// 门禁覆盖：快路 argv 逐字形态 / 快路判据 / 三级回退（快路→软解软缩放→CPU）/
// 1080p 档码率落在用户要的 3000~4000 kbps 区间。
// 变异测试（必须变红）：删掉快路回退、把 fps 挪到 scale_vt 之后、hwdownload 格式猜错。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// TestFastPipelineGate ①-④：argv 的逐字形态与快路判据。
func TestFastPipelineGate(t *testing.T) {
	hwFast := TranscodeRequest{
		Src: "in.mkv", Dst: "out.mp4", Width: 2592, Height: 1080,
		VideoKbps: 4500, AudioKbps: 96, DurationSec: 5,
		Encoder: EncoderHardware, Mode: ModeBitrate, Quality: DefaultVTQuality,
		Fast: true, SourceFPS: 60, SrcBitDepth: 10,
	}

	t.Run("① 快路 argv 逐字固定（10bit 源：硬解 + scale_vt + p010le 下载 + 降到 30fps）", func(t *testing.T) {
		got := strings.Join(TranscodeArgs(hwFast, 0), " ")
		want := "-hide_banner -nostdin -y -hwaccel videotoolbox -hwaccel_output_format videotoolbox_vld " +
			"-i in.mkv -c:v hevc_videotoolbox -tag:v hvc1 -prio_speed 1 -pix_fmt yuv420p " +
			"-b:v 4500k -maxrate 4500k -bufsize 9000k " +
			"-vf fps=30,scale_vt=w=2592:h=1080,hwdownload,format=p010le,format=yuv420p " +
			"-map 0:v:0 -map 0:a -c:a aac -b:a 96k -movflags +faststart -f mp4 -progress pipe:1 -nostats " +
			"-loglevel error out.mp4"
		if got != want {
			t.Fatalf("快路 argv 变了\n got=%s\nwant=%s", got, want)
		}
		// -hwaccel 是输入选项：必须在 -i 之前（放后面会被当成输出选项）。
		args := TranscodeArgs(hwFast, 0)
		if idxOf(args, "-hwaccel") > idxOf(args, "-i") {
			t.Fatalf("-hwaccel 必须在 -i 之前：%v", args)
		}
	})

	t.Run("② 8bit 源：下载成 nv12；源 ≤30fps 不补帧", func(t *testing.T) {
		req := hwFast
		req.SrcBitDepth, req.SourceFPS = 8, 24
		args := TranscodeArgs(req, 0)
		if !argsHave(args, "-vf", "scale_vt=w=2592:h=1080,hwdownload,format=nv12") {
			t.Fatalf("8bit 源必须下载成 nv12（p010le 会 -22）：%v", args)
		}
		for _, a := range args {
			if strings.Contains(a, "fps=") {
				t.Fatalf("源 24fps 不该补帧到 30（会白白多编码 25%% 帧）：%v", args)
			}
		}
		// 负向对照：位深认不出（0）时按 8bit 走（拿不准就用能被软解兜住的那个）。
		req.SrcBitDepth = 0
		if !argsHave(TranscodeArgs(req, 0), "-vf", "scale_vt=w=2592:h=1080,hwdownload,format=nv12") {
			t.Fatalf("位深未知时不该猜 p010le：%v", TranscodeArgs(req, 0))
		}
	})

	t.Run("③ 非快路的硬件档：软解 + 软缩放（不加 -hwaccel），>30fps 仍降帧", func(t *testing.T) {
		req := hwFast
		req.Fast = false
		args := TranscodeArgs(req, 0)
		for _, a := range args {
			if a == "-hwaccel" || a == "-hwaccel_output_format" || strings.Contains(a, "scale_vt") {
				t.Fatalf("非快路不许出现硬解/GPU 缩放：%v", args)
			}
		}
		if !argsHave(args, "-vf", "fps=30,scale=2592:1080") {
			t.Fatalf("非快路的硬件档也要降帧：%v", args)
		}
		if !argsHave(args, "-prio_speed", "1") {
			t.Fatalf("硬件编码器要带 -prio_speed 1（实测吞吐 5.5x→9.6x）：%v", args)
		}
	})

	t.Run("④ CPU 档一个字不变；硬件档的码率/质量参数与 CPU 逐字相同", func(t *testing.T) {
		base := TranscodeRequest{
			Src: "in.mp4", Dst: "out.mp4", Width: 854, Height: 480,
			VideoKbps: 800, AudioKbps: 96, DurationSec: 5,
			Encoder: EncoderCPU, Mode: ModeBitrate,
		}
		got := strings.Join(TranscodeArgs(base, 0), " ")
		want := "-hide_banner -nostdin -y -i in.mp4 -c:v libx264 -preset veryfast -profile:v main " +
			"-pix_fmt yuv420p -b:v 800k -maxrate 800k -bufsize 1600k -vf scale=854:480 " +
			"-map 0:v:0 -map 0:a -c:a aac -b:a 96k -movflags +faststart -f mp4 -progress pipe:1 -nostats " +
			"-loglevel error out.mp4"
		if got != want {
			t.Fatalf("CPU 档 argv 变了\n got=%s\nwant=%s", got, want)
		}
		// 源 60fps 的 CPU 档不许偷偷降帧（用户没要 CPU 档变速；只有硬件档降）。
		base.SourceFPS = 60
		if strings.Contains(strings.Join(TranscodeArgs(base, 0), " "), "fps=30") {
			t.Fatalf("CPU 档不该降帧：%v", TranscodeArgs(base, 0))
		}
		// 逐字相同：硬件档（快路/非快路）的 -b:v/-maxrate/-bufsize 必须等于 CPU 档。
		hwReq := TranscodeRequest{
			Src: "in.mp4", Dst: "out.mp4", Width: 854, Height: 480,
			VideoKbps: 800, AudioKbps: 96, DurationSec: 5,
			Encoder: EncoderHardware, Mode: ModeBitrate, SourceFPS: 60, SrcBitDepth: 10, Fast: true,
		}
		for _, fast := range []bool{true, false} {
			hwReq.Fast = fast
			a := TranscodeArgs(hwReq, 0)
			for _, w := range [][2]string{{"-b:v", "800k"}, {"-maxrate", "800k"}, {"-bufsize", "1600k"}} {
				if !argsHave(a, w[0], w[1]) {
					t.Fatalf("硬件档（Fast=%v）码率参数与 CPU 不一致：缺 %s %s\n%v", fast, w[0], w[1], a)
				}
			}
		}
	})

	t.Run("⑤ 快路判据：只对 4K 级 + 10bit↑ + ≥6Mbps 的源开硬解", func(t *testing.T) {
		cases := []struct {
			name string
			info MediaInfo
			want bool
		}{
			{"用户片源 4K60 10bit 25Mbps", MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 25283}, true},
			{"4K 10bit 8Mbps", MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 8000}, true},
			{"4K 10bit 3Mbps（软解更快）", MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 3000}, false},
			{"4K 8bit 25Mbps（硬解慢 6 倍）", MediaInfo{Width: 3840, Height: 1608, BitDepth: 8, VideoKbps: 25000}, false},
			{"4K 位深未知", MediaInfo{Width: 3840, Height: 1608, BitDepth: 0, VideoKbps: 25000}, false},
			{"1080p 10bit 8Mbps（像素太少，软解够快）", MediaInfo{Width: 1920, Height: 1080, BitDepth: 10, VideoKbps: 8000}, false},
		}
		for _, c := range cases {
			if got := needFastDecode(c.info); got != c.want {
				t.Errorf("%s：needFastDecode=%v，应为 %v", c.name, got, c.want)
			}
		}
	})

	t.Run("⑧ 探测→规划→argv 一条链：帧率/位深真的传下来了", func(t *testing.T) {
		// 真实 ffprobe 输出的形状（用户那段 4K60 10bit 源）。
		raw := []byte(`{"streams":[
			{"codec_type":"video","codec_name":"hevc","width":3840,"height":1608,
			 "r_frame_rate":"60/1","avg_frame_rate":"60/1","pix_fmt":"yuv420p10le"},
			{"codec_type":"audio","codec_name":"eac3","bit_rate":"256000"}],
			"format":{"duration":"6228.448000","bit_rate":"25283554","size":"19684662933"}}`)
		info, err := ParseProbeJSON(raw, 19684662933)
		if err != nil {
			t.Fatal(err)
		}
		if info.FPS != 60 || info.BitDepth != 10 || info.Width != 3840 || info.Height != 1608 {
			t.Fatalf("探测结果不对：fps=%v depth=%d %dx%d", info.FPS, info.BitDepth, info.Width, info.Height)
		}
		preset, _ := FindPreset("1080p")
		hw := PlanOne("a.mkv", "/tmp/a.mkv", "/tmp/out", info,
			Options{Preset: preset, Encoder: EncoderHardware, Mode: ModeBitrate}, false)
		if !hw.FastPipeline || !hw.CapFPS30 || hw.SourceBitDepth != 10 || hw.SourceFPS != 60 {
			t.Fatalf("10bit 4K60 高码率源必须走快路且降帧：%+v", hw)
		}
		if hw.TargetWidth != 2592 || hw.TargetHeight != 1080 {
			t.Fatalf("1080p 档对齐后应是 2592x1080：%dx%d", hw.TargetWidth, hw.TargetHeight)
		}
		args := TranscodeArgs(TranscodeRequest{
			Src: hw.Path, Dst: "o.mp4", Width: hw.TargetWidth, Height: hw.TargetHeight,
			VideoKbps: hw.VideoKbps, Mode: hw.Mode, Encoder: hw.Encoder, Quality: hw.Quality,
			Fast: hw.FastPipeline, SourceFPS: hw.SourceFPS, SrcBitDepth: hw.SourceBitDepth,
		}, 0)
		for _, w := range [][2]string{
			{"-hwaccel_output_format", "videotoolbox_vld"},
			{"-prio_speed", "1"},
			{"-vf", "fps=30,scale_vt=w=2592:h=1080,hwdownload,format=p010le,format=yuv420p"},
		} {
			if !argsHave(args, w[0], w[1]) {
				t.Fatalf("规划出来的 argv 缺 %s %s：%v", w[0], w[1], args)
			}
		}
		// CPU 档同一份源：不快路、也不降帧（CPU 档保持源帧率）。
		cpu := PlanOne("a.mkv", "/tmp/a.mkv", "/tmp/out", info,
			Options{Preset: preset, Encoder: EncoderCPU, Mode: ModeBitrate}, false)
		if cpu.FastPipeline || cpu.CapFPS30 {
			t.Fatalf("CPU 档不该走快路/降帧：%+v", cpu)
		}
		// 8bit 24fps 源：不快路、不补帧；帧率读不到（0/0）时也不动帧率。
		info8 := info
		info8.BitDepth, info8.FPS = 8, 24
		if needFastDecode(info8) || capFPS30(info8) {
			t.Fatalf("8bit 24fps 源不该快路/降帧")
		}
		unknown, err := ParseProbeJSON([]byte(`{"streams":[{"codec_type":"video","codec_name":"h264",
			"width":3840,"height":1608,"r_frame_rate":"0/0","avg_frame_rate":"0/0","pix_fmt":"yuv420p"}],
			"format":{"duration":"10","bit_rate":"25283554"}}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		if unknown.FPS != 0 || unknown.BitDepth != 8 || capFPS30(unknown) {
			t.Fatalf("帧率读不到时不该乱降帧：fps=%v depth=%d", unknown.FPS, unknown.BitDepth)
		}
	})
}

// failRunner 是"按条件失败"的假执行器：用来断言回退链真的换了参数重跑。
type failRunner struct {
	gateRunner
	failFast bool // Fast=true 的请求直接失败（模拟 scale_vt 不可用）
	failHW   bool // 硬件编码的请求直接失败（模拟 hevc_videotoolbox 不可用）
}

func (f *failRunner) Transcode(ctx context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	f.reqs = append(f.reqs, req)
	if f.failFast && req.Fast {
		return fmt.Errorf("模拟：No such filter: 'scale_vt'")
	}
	if f.failHW && req.Encoder == EncoderHardware {
		return fmt.Errorf("模拟：hevc_videotoolbox 初始化失败")
	}
	if err := os.MkdirAll(filepath.Dir(req.Dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(req.Dst, bytes.Repeat([]byte{7}, int(f.outBytes)), 0o644)
}

// TestFastPipelineFallbackGate ⑥：三级回退（快路 → 软解软缩放 → CPU）逐级生效且写明原因。
func TestFastPipelineFallbackGate(t *testing.T) {
	rows := func(dir string) []Plan {
		name := "a.mkv"
		return []Plan{{
			Name: name, Path: filepath.Join(dir, name), OutPath: filepath.Join(dir, "output", name+".1080p.mp4"),
			SourceBytes: 4096, DurationSec: 5, TargetWidth: 2592, TargetHeight: 1080,
			VideoKbps: 4500, AudioKbps: 96, Encoder: EncoderHardware, Mode: ModeBitrate,
			SourceWidth: 3840, SourceHeight: 1608, SourceFPS: 60, SourceBitDepth: 10,
			FastPipeline: true, CapFPS30: true, Quality: DefaultVTQuality,
		}}
	}
	setup := func(t *testing.T) (string, map[string]MediaInfo) {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "a.mkv")
		if err := os.WriteFile(p, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, map[string]MediaInfo{p: {}}
	}

	t.Run("快路失败 ⇒ 软解软缩放重试（编码器不变、Fast 清掉、日志写明）", func(t *testing.T) {
		dir, infos := setup(t)
		runner := &failRunner{gateRunner: gateRunner{infos: infos, outBytes: 500}, failFast: true}
		var logs []string
		res, err := RunPlan(context.Background(), filepath.Join(dir, "output"), rows(dir), runner,
			Hooks{Log: func(level, msg string) { logs = append(logs, level+"|"+msg) }})
		if err != nil || res.Done != 1 {
			t.Fatalf("回退后应成功：err=%v done=%d", err, res.Done)
		}
		if len(runner.reqs) != 2 {
			t.Fatalf("应有 2 次尝试（快路 + 软解软缩放），实际 %d：%+v", len(runner.reqs), runner.reqs)
		}
		if !runner.reqs[0].Fast || runner.reqs[1].Fast {
			t.Fatalf("第二次必须清掉 Fast：%+v", runner.reqs)
		}
		if runner.reqs[1].Encoder != EncoderHardware {
			t.Fatalf("只是解码/缩放回退，编码器不该变：%+v", runner.reqs[1])
		}
		// 第二次 argv 不许再出现硬解/GPU 缩放（否则等于没回退）。
		a2 := TranscodeArgs(runner.reqs[1], 0)
		for _, a := range a2 {
			if a == "-hwaccel" || a == "-hwaccel_output_format" || strings.Contains(a, "scale_vt") {
				t.Fatalf("回退后的 argv 还带着硬解/GPU 缩放：%v", a2)
			}
		}
		if res.Items[0].EncoderFallback {
			t.Fatalf("这是解码回退、不是编码回退，不该标 EncoderFallback")
		}
		if !hasLine(logs, "全 GPU 加速不可用，已回退软件解码/缩放重试") {
			t.Fatalf("必须如实写明回退原因，实际日志：%v", logs)
		}
	})

	t.Run("快路+硬件都失败 ⇒ CPU 编码（清掉 Fast、结果标已回退、两条日志都在）", func(t *testing.T) {
		dir, infos := setup(t)
		runner := &failRunner{gateRunner: gateRunner{infos: infos, outBytes: 500}, failFast: true, failHW: true}
		var logs []string
		res, err := RunPlan(context.Background(), filepath.Join(dir, "output"), rows(dir), runner,
			Hooks{Log: func(level, msg string) { logs = append(logs, level+"|"+msg) }})
		if err != nil || res.Done != 1 {
			t.Fatalf("CPU 兜底后应成功：err=%v done=%d", err, res.Done)
		}
		if len(runner.reqs) != 3 {
			t.Fatalf("应有 3 次尝试（快路 + 软解软缩放 + CPU），实际 %d：%+v", len(runner.reqs), runner.reqs)
		}
		cpu := runner.reqs[2]
		if cpu.Encoder != EncoderCPU || cpu.Fast {
			t.Fatalf("第三次必须是 CPU 且不带快路：%+v", cpu)
		}
		ca := TranscodeArgs(cpu, 0)
		if !argsHave(ca, "-c:v", "libx264") {
			t.Fatalf("CPU 兜底的 argv 不对：%v", ca)
		}
		for _, a := range ca {
			if a == "-hwaccel" || a == "-hwaccel_output_format" {
				t.Fatalf("CPU 兜底不许带硬解：%v", ca)
			}
		}
		if !res.Items[0].EncoderFallback {
			t.Fatalf("CPU 兜底必须在结果里标「已回退」")
		}
		if !hasLine(logs, "全 GPU 加速不可用，已回退软件解码/缩放重试") || !hasLine(logs, "硬件编码失败，已回退软件编码重试") {
			t.Fatalf("两条回退都要如实写明，实际日志：%v", logs)
		}
	})

	t.Run("负向对照：一切正常时只跑一次，日志里不许出现任何回退", func(t *testing.T) {
		dir, infos := setup(t)
		runner := &failRunner{gateRunner: gateRunner{infos: infos, outBytes: 500}}
		var logs []string
		res, err := RunPlan(context.Background(), filepath.Join(dir, "output"), rows(dir), runner,
			Hooks{Log: func(level, msg string) { logs = append(logs, level+"|"+msg) }})
		if err != nil || res.Done != 1 || len(runner.reqs) != 1 {
			t.Fatalf("正常路径应只跑一次：err=%v done=%d reqs=%d", err, res.Done, len(runner.reqs))
		}
		if !runner.reqs[0].Fast {
			t.Fatalf("计划标了 FastPipeline，请求就该带 Fast：%+v", runner.reqs[0])
		}
		if hasLine(logs, "回退") {
			t.Fatalf("没失败就不许写回退日志：%v", logs)
		}
		// 任务日志要说清楚走的是哪条管线（全 GPU / 软解硬编 / 降到 30fps）。
		if !hasLine(logs, "全 GPU（硬解+GPU 缩放）") || !hasLine(logs, "降到 30fps") {
			t.Fatalf("日志必须写明管线与降帧，实际：%v", logs)
		}
	})
}

// TestVideoBitrateCalibrationGate ⑦：1080p 档的码率必须落在用户要的 3000~4000 kbps。
//
// 标定依据（本机实测，4K60 10bit → 2592x1080，60 秒片段，hevc_videotoolbox 快路）：
//
//	-b:v/-maxrate 3500k → 实测 2439 kbps    4000k → 2752 kbps
//	4500k → 3052 kbps ✅落在区间              5000k → 3280 kbps ✅
//
// 硬件编码器的平均码率只有请求值的 ~0.68 倍（-maxrate 会压低平均），所以 1080p 档
// 默认 3000（面板下限）与已有的 4500 选项合起来正好覆盖用户要的 3~4 Mbps。
func TestVideoBitrateCalibrationGate(t *testing.T) {
	p, ok := FindPreset("1080p")
	if !ok {
		t.Fatal("没有 1080p 档")
	}
	if p.DefaultKbps < 3000 || p.DefaultKbps > 4000 {
		t.Errorf("1080p 档默认码率 %d kbps 不在用户要的 3000~4000 区间", p.DefaultKbps)
	}
	got4500 := false
	for _, c := range BitrateChoices(p) {
		if c.KBps >= 3000 && c.KBps <= 4000 {
			got4500 = true
		}
	}
	if !got4500 {
		t.Errorf("1080p 档必须给一个落在 3000~4000 kbps 的选项（硬件档实测要 4500k 请求才够）")
	}
	// 标定值真的进 argv：1080p + 目标码率 + 硬件档。
	req := TranscodeRequest{
		Src: "in.mkv", Dst: "out.mp4", Width: 2592, Height: 1080,
		VideoKbps: p.DefaultKbps, AudioKbps: 96, DurationSec: 5,
		Encoder: EncoderHardware, Mode: ModeBitrate, SourceFPS: 60, SrcBitDepth: 10, Fast: true,
	}
	args := TranscodeArgs(req, 0)
	def := fmt.Sprintf("%dk", p.DefaultKbps)
	if !argsHave(args, "-b:v", def) || !argsHave(args, "-maxrate", def) {
		t.Fatalf("1080p 档的标定码率（%s）没进 argv：%v", def, args)
	}
	if !argsHave(args, "-vf", "fps=30,scale_vt=w=2592:h=1080,hwdownload,format=p010le,format=yuv420p") {
		t.Fatalf("1080p 档的快路缩放链不对：%v", args)
	}
}

// TestEnvNotesGate 断言环境提示（别的 ffmpeg / swap / 负载）只在真异常时出现。
func TestEnvNotesGate(t *testing.T) {
	t.Run("① 纯函数：三个信号各自触发；用户报障那组数字必须三条全中", func(t *testing.T) {
		cases := []struct {
			name                     string
			peers, swapUsed, swapTot int
			load                     float64
			cores                    int
			want                     int
		}{
			{"全正常", 0, 0, 0, 2.0, 10, 0},
			{"负载未超核数（等于也不算）", 0, 0, 0, 10.0, 10, 0},
			{"swap 占了一半但不到 1G 不提示", 0, 512, 4096, 2.0, 10, 0},
			{"有别的 ffmpeg", 2, 0, 0, 2.0, 10, 1},
			{"swap 过半且超 1G", 0, 2944, 4096, 2.0, 10, 1},
			{"负载超核数", 0, 0, 0, 15.75, 10, 1},
			{"三条同时", 1, 2944, 4096, 15.75, 10, 3},
		}
		for _, c := range cases {
			got := envNotesFrom(c.peers, c.swapUsed, c.swapTot, c.load, c.cores)
			if len(got) != c.want {
				t.Errorf("%s：应有 %d 条，实际 %d 条（%v）", c.name, c.want, len(got), got)
			}
		}
		joined := strings.Join(envNotesFrom(1, 2944, 4096, 15.75, 10), " | ")
		for _, want := range []string{"ffmpeg", "swap", "负载"} {
			if !strings.Contains(joined, want) {
				t.Errorf("用户报障场景（CPU 56.6%%、负载 15.75、swap 2.94G）必须提示 %q，实际：%s", want, joined)
			}
		}
	})

	t.Run("② 任务里：开始时说一次、每 20 个文件复查、同一条不重复、nil 不检测", func(t *testing.T) {
		dir := t.TempDir()
		outDir := filepath.Join(dir, "output")
		rows := make([]Plan, 25)
		for i := range rows {
			name := fmt.Sprintf("f%02d.mp4", i)
			rows[i] = Plan{
				Name: name, Path: filepath.Join(dir, name),
				OutPath:   filepath.Join(outDir, name+".480p.mp4"),
				VideoKbps: 800, AudioKbps: 96, TargetWidth: 854, TargetHeight: 480,
				SourceBytes: 4096, Encoder: EncoderCPU, Mode: ModeBitrate,
			}
		}
		runner := &gateRunner{outBytes: 100}

		var envLines []string
		calls := 0
		hooks := Hooks{
			Log: func(level, msg string) {
				if strings.HasPrefix(msg, "⚠ ") {
					envLines = append(envLines, level+"|"+msg)
				}
			},
			EnvNotes: func() []string {
				calls++
				if calls == 1 {
					// 任务开始时：两个信号（第二条要在复查时被去重）。
					return []string{"检测到 1 个其它 ffmpeg 进程，可能在抢 CPU", "系统负载 15.8 超过 10 核"}
				}
				// 中途（第 21 个文件前）复查：多出一个新信号。
				return []string{"系统负载 15.8 超过 10 核", "检测到 2 个其它 ffmpeg 进程，可能在抢 CPU"}
			},
		}
		res, err := RunPlan(context.Background(), outDir, rows, runner, hooks)
		if err != nil || res.Done != len(rows) {
			t.Fatalf("任务没跑完：err=%v done=%d/%d", err, res.Done, len(rows))
		}
		if calls != 2 {
			t.Fatalf("25 个文件应复查 2 次（第 1 个之前 + 第 21 个之前），实际 %d 次", calls)
		}
		if len(envLines) != 3 {
			t.Fatalf("应有 3 条环境提示（开始 2 条 + 中途新增 1 条），实际 %d 条：%v", len(envLines), envLines)
		}
		joined := strings.Join(envLines, "\n")
		if n := strings.Count(joined, "系统负载"); n != 1 {
			t.Errorf("同一条（系统负载）只许说一次，实际 %d 次", n)
		}
		if n := strings.Count(joined, "其它 ffmpeg"); n != 2 {
			t.Errorf("ffmpeg 数从 1 变 2 ⇒ 要说两次（开始一次、中途一次），实际 %d 次", n)
		}
		for _, l := range envLines {
			if !strings.HasPrefix(l, tasks.LevelWarn+"|⚠ ") {
				t.Errorf("环境提示必须是 warn + ⚠ 前缀，实际 %q", l)
			}
		}

		// 负向对照：nil = 不检测（单测默认），一条都不许有。
		envLines = nil
		if _, err := RunPlan(context.Background(), outDir, rows, runner, Hooks{Log: hooks.Log}); err != nil {
			t.Fatal(err)
		}
		if len(envLines) != 0 {
			t.Errorf("EnvNotes=nil 时不该有任何环境提示，实际 %v", envLines)
		}
	})
}

// withOpts 复制一份请求并套上选项（与 web 门禁同一口径，避免每个 case 重复写字段）。
func withOpts(req TranscodeRequest, enc, mode string, quality int, twoPass bool) TranscodeRequest {
	req.Encoder, req.Mode, req.Quality, req.TwoPass = enc, mode, quality, twoPass
	return req
}

// argsHave 断言 argv 里出现某个 flag（value=="" 只看 flag）。
func argsHave(args []string, flag, value string) bool {
	for i, a := range args {
		if a != flag {
			continue
		}
		if value == "" || (i+1 < len(args) && args[i+1] == value) {
			return true
		}
	}
	return false
}

// idxOf 返回 flag 在 argv 里的下标（找不到返回 len）。
func idxOf(args []string, flag string) int {
	for i, a := range args {
		if a == flag {
			return i
		}
	}
	return len(args)
}

// hasLine 断言日志里有包含这段文字的行。
func hasLine(logs []string, want string) bool {
	for _, l := range logs {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

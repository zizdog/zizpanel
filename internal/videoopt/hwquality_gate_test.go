package videoopt

// hwquality_gate_test.go —— 「硬件质量档 = 有上限质量模式」的唯一门禁。
//
// 为什么要有它（2026-10-06 用户报障）：硬件质量档三档都压出 8GB+ 的电影。根因有两条，
// 任何一条单独都能让体积失控：
//  1. 命令行里只要有 -maxrate，hevc_videotoolbox 的 -q:v 就被**完全忽略**
//     （本机实测同参数 q:v 20/70 输出逐字节相同）⇒ "三档"其实是一档；
//  2. 旧实现质量档的上限 = 原片码率×0.95 ⇒ 4K 源压 1080p 也按 4K 码率出片（~16 Mbps）。
//
// 门禁四条（不跑真 ffmpeg，全是纯函数）：
//  ① 硬件质量档 argv 必须同时含 -q:v 与明确上限 -maxrate/-bufsize，且不许有 -b:v/-crf；
//  ② 标定表里的档位→上限映射 + 分辨率缩放（480p/720p 不许用 1080p 的上限）；
//  ③ 预计体积用的系数必须与标定表**同源**（PlanOne 的数字 == 表×系数，别处不许再写死）；
//  ④ CPU 档 argv 逐字未变（真 CRF 语义不许动）。
//
// 变异（必须变红，见本轮报告）：删掉 -maxrate、把 hwQualityRateRatio 改成 0.95。

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func presetID(t *testing.T, id string) Preset {
	t.Helper()
	p, ok := FindPreset(id)
	if !ok {
		t.Fatalf("没有 %s 档", id)
	}
	return p
}

// ① 硬件质量档 argv：-q:v + 明确上限，且固定形态（逐字）。
func TestHWQualityArgvHasCapGate(t *testing.T) {
	p1080 := presetID(t, "1080p")
	// 用**真实规划**（4K 源 → 1080p，正是用户报障那台机器的形态）拿上限：
	// 去掉 planBitrates 里的标定上限 ⇒ 这里会拿到原片码率×0.95（23775k）⇒ 立刻红。
	info := MediaInfo{
		Width: 3840, Height: 1608, DurationSec: 7200, FileBytes: 20 << 30,
		VideoKbps: 25027, AudioKbps: 256, HasVideo: true, HasAudio: true, FPS: 60, BitDepth: 10, Codec: "hevc",
	}
	for _, tier := range hwQualityCalib {
		capKbps := HWQualityCapKbps(tier.Quality, p1080, 1920, 1080)
		p := PlanOne("a.mkv", "/t/a.mkv", "/t/out", info,
			Options{Preset: p1080, Encoder: EncoderHardware, Mode: ModeQuality, Quality: tier.Quality}, false)
		if p.MaxRateKbps != capKbps || p.VideoKbps != capKbps {
			t.Fatalf("档位 %d 的规划上限不是标定值 %d：video=%d maxrate=%d",
				tier.Quality, capKbps, p.VideoKbps, p.MaxRateKbps)
		}
		args := TranscodeArgs(TranscodeRequest{
			Src: "in.mkv", Dst: "out.mp4", Width: p.TargetWidth, Height: p.TargetHeight,
			VideoKbps: p.VideoKbps, AudioKbps: p.AudioKbps, DurationSec: p.DurationSec,
			Encoder: p.Encoder, Mode: p.Mode, Quality: p.Quality,
			Fast: p.FastPipeline, SourceFPS: p.SourceFPS, SrcBitDepth: p.SourceBitDepth,
		}, 0)
		if !argsHave(args, "-q:v", fmt.Sprint(tier.Quality)) {
			t.Errorf("档位 %d：argv 缺少 -q:v %d：%v", tier.Quality, tier.Quality, args)
		}
		if !argsHave(args, "-maxrate", fmt.Sprintf("%dk", capKbps)) {
			t.Errorf("档位 %d：argv 必须含上限 -maxrate %dk：%v", tier.Quality, capKbps, args)
		}
		if !argsHave(args, "-bufsize", fmt.Sprintf("%dk", capKbps*2)) {
			t.Errorf("档位 %d：argv 必须含 -bufsize %dk：%v", tier.Quality, capKbps*2, args)
		}
		if argsHave(args, "-b:v", "") || argsHave(args, "-crf", "") {
			t.Errorf("档位 %d：有上限质量模式不许出现 -b:v/-crf：%v", tier.Quality, args)
		}
	}

	// 逐字形态（1080p 均衡档）：任何"顺手删掉上限"的改动都会在这里与上面同时变红。
	got := TranscodeArgs(TranscodeRequest{
		Src: "in.mkv", Dst: "out.mp4", Width: 1920, Height: 1080,
		VideoKbps: HWQualityCapKbps(VTQualityBalanced, p1080, 1920, 1080),
		AudioKbps: 96, DurationSec: 5, Encoder: EncoderHardware, Mode: ModeQuality, Quality: VTQualityBalanced,
	}, 0)
	want := strings.Join([]string{
		"-hide_banner", "-nostdin", "-y", "-i", "in.mkv",
		"-c:v", "hevc_videotoolbox", "-tag:v", "hvc1", "-prio_speed", "1", "-pix_fmt", "yuv420p",
		"-q:v", "45", "-maxrate", "3000k", "-bufsize", "6000k",
		"-vf", "scale=1920:1080",
		"-map", "0:v:0", "-map", "0:a",
		"-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", "-f", "mp4",
		"-progress", "pipe:1", "-nostats", "-loglevel", "error", "out.mp4",
	}, " ")
	if strings.Join(got, " ") != want {
		t.Errorf("硬件质量档 argv 形态变了：\n got=%s\nwant=%s", strings.Join(got, " "), want)
	}

	// 负向对照自检：真的去掉上限（VideoKbps=0）时，"必须含 -maxrate"这条断言会立刻抓不住。
	noCap := TranscodeRequest{
		Src: "in.mkv", Dst: "out.mp4", Width: 1920, Height: 1080,
		AudioKbps: 96, DurationSec: 5, Encoder: EncoderHardware, Mode: ModeQuality, Quality: VTQualityBalanced,
	}
	if argsHave(TranscodeArgs(noCap, 0), "-maxrate", "") {
		t.Fatal("负向对照失效：没有码率时 argv 仍出现了 -maxrate")
	}
}

// ② 标定表：档位 → 1080p 上限；其它分辨率按档位锚点缩放，不出现"480p 用 1080p 的上限"。
func TestHWQualityCapTableGate(t *testing.T) {
	p1080, p720, p480 := presetID(t, "1080p"), presetID(t, "720p"), presetID(t, "480p")
	want := map[int]int{VTQualitySmall: 2500, VTQualityBalanced: 3000, VTQualityHigh: 3600}
	for q, w := range want {
		if got := HWQualityCapKbps(q, p1080, 1920, 1080); got != w {
			t.Errorf("1080p 档位 %d 的上限应为 %d kbps，实际 %d", q, w, got)
		}
	}
	// 表里不许有"没人管"的档位。
	for _, tier := range hwQualityCalib {
		w, ok := want[tier.Quality]
		if !ok {
			t.Fatalf("标定表新增了档位 %d，门禁没跟上（档位→上限映射必须一起定）", tier.Quality)
		}
		if tier.Cap1080p != w {
			t.Errorf("标定表档位 %d 的上限 %d 与门禁预期 %d 不一致", tier.Quality, tier.Cap1080p, w)
		}
	}
	// 分辨率缩放：上限 × 档位锚点/3000（480p 锚点 800、720p 锚点 1500）。
	for _, tier := range hwQualityCalib {
		if got := HWQualityCapKbps(tier.Quality, p480, 854, 480); got != int(math.Round(float64(tier.Cap1080p)*800/3000)) {
			t.Errorf("档位 %d 在 480p 的上限应为 %d，实际 %d", tier.Quality, int(math.Round(float64(tier.Cap1080p)*800/3000)), got)
		}
		if got := HWQualityCapKbps(tier.Quality, p720, 1280, 720); got != int(math.Round(float64(tier.Cap1080p)*1500/3000)) {
			t.Errorf("档位 %d 在 720p 的上限应为 %d，实际 %d", tier.Quality, int(math.Round(float64(tier.Cap1080p)*1500/3000)), got)
		}
	}
	if got := HWQualityCapKbps(VTQualityBalanced, p480, 854, 480); got != 800 {
		t.Errorf("480p 均衡档上限应为 800 kbps，实际 %d", got)
	}
	if got := HWQualityCapKbps(VTQualityBalanced, p720, 1280, 720); got != 1500 {
		t.Errorf("720p 均衡档上限应为 1500 kbps，实际 %d", got)
	}
	if HWQualityCapKbps(VTQualityBalanced, p480, 854, 480) >= HWQualityCapKbps(VTQualityBalanced, p1080, 1920, 1080) {
		t.Error("480p 的上限不许 ≥ 1080p 的上限")
	}
	// 面板只给三档；其它 q:v 值按最近档处理（不猜新刻度）。
	if got := HWQualityCapKbps(100, p1080, 1920, 1080); got != 3600 {
		t.Errorf("q:v 100 应按最近档（50）取上限 3600，实际 %d", got)
	}
}

// ③ 预计体积：系数与标定表同源；1080p 两小时落在按新上限推导的 1.5~2.4GB。
func TestHWQualityEstimateSharesCalibrationGate(t *testing.T) {
	p1080 := presetID(t, "1080p")
	// 用户那段源的真实量级：4K60 10bit、视频 ~25027 kbps、两小时。
	info := MediaInfo{
		Width: 3840, Height: 1608, DurationSec: 7200, FileBytes: 20 << 30,
		VideoKbps: 25027, AudioKbps: 256, HasVideo: true, HasAudio: true, FPS: 60, BitDepth: 10, Codec: "hevc",
	}
	for _, tier := range hwQualityCalib {
		p := PlanOne("a.mkv", "/t/a.mkv", "/t/out", info,
			Options{Preset: p1080, Encoder: EncoderHardware, Mode: ModeQuality, Quality: tier.Quality}, false)
		capKbps := HWQualityCapKbps(tier.Quality, p1080, p.TargetWidth, p.TargetHeight)
		// 源码率×0.95 = 23775 > 3600 ⇒ 上限不被原片封顶，正是用户那部片子。
		if p.MaxRateKbps != capKbps {
			t.Fatalf("档位 %d 的上限应为 %d，实际 %d（MaxRateKbps 没走标定表）", tier.Quality, capKbps, p.MaxRateKbps)
		}
		wantKbps := int(math.Round(float64(capKbps) * HWQualityRateRatio()))
		if p.EstKbps != wantKbps {
			t.Errorf("档位 %d 预计实际码率应为 %d（上限×同源系数），实际 %d", tier.Quality, wantKbps, p.EstKbps)
		}
		if p.EstBytes != estimateBytes(wantKbps, p.AudioKbps, info.DurationSec) {
			t.Errorf("档位 %d 的 est_bytes 没按同源系数算：%d", tier.Quality, p.EstBytes)
		}
		if p.EstimateUnknown {
			t.Errorf("档位 %d 是有上限质量模式，必须给预计体积", tier.Quality)
		}
		// 系数真的 <1（没乘系数时预计值会等于上限）。
		if wantKbps >= p.MaxRateKbps {
			t.Errorf("档位 %d：预计码率 %d 没有低于上限 %d，系数没生效", tier.Quality, wantKbps, p.MaxRateKbps)
		}
		// 两小时预计体积区间：按本表与实测系数推导（2500×0.67≈1.6GB、3600×0.67≈2.3GB）。
		gb := float64(p.EstBytes) / 1e9
		if gb < 1.5 || gb > 2.4 {
			t.Errorf("档位 %d：1080p 两小时预计 %.2f GB，不在按标定推导的 1.5~2.4GB 区间", tier.Quality, gb)
		}
	}
	// 读不到上限 ⇒ 返回 0（调用方按"未知"处理，绝不猜体积）。
	if HWQualityEstimateKbps(0) != 0 {
		t.Error("没有上限时必须返回 0（未知），不许猜")
	}
	// 系数必须落在本机实测区间（2026-10-06 实测 0.666，四个上限全一致）：
	// 改大（例如为了"好看"写成 0.95）会让预计体积谎报，必须红。
	if r := HWQualityRateRatio(); r < 0.60 || r > 0.72 {
		t.Errorf("标定系数 %.3f 超出实测区间 0.60~0.72（实测 0.666）", r)
	}
	// 时间读不到 ⇒ 预计字节为 0（面板显示"未知"），不编数字。
	if got := estimateBytes(HWQualityEstimateKbps(3000), 96, 0); got != 0 {
		t.Errorf("读不到时长时预计体积必须为 0，实际 %d", got)
	}
}

// ④ CPU 档 argv 逐字未变（真 CRF 的语义与命令行不许动）。
func TestCPUQualityArgvUnchangedGate(t *testing.T) {
	crf := TranscodeArgs(TranscodeRequest{
		Src: "in.mp4", Dst: "out.mp4", Width: 854, Height: 480,
		VideoKbps: 800, AudioKbps: 96, DurationSec: 5, Encoder: EncoderCPU, Mode: ModeQuality, Quality: DefaultCRF,
	}, 0)
	wantCRF := strings.Join([]string{
		"-hide_banner", "-nostdin", "-y", "-i", "in.mp4",
		"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "main", "-pix_fmt", "yuv420p",
		"-crf", "26", "-maxrate", "800k", "-bufsize", "1600k", "-vf", "scale=854:480",
		"-map", "0:v:0", "-map", "0:a",
		"-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", "-f", "mp4",
		"-progress", "pipe:1", "-nostats", "-loglevel", "error", "out.mp4",
	}, " ")
	if strings.Join(crf, " ") != wantCRF {
		t.Errorf("CPU 真 CRF 的 argv 变了：\n got=%s\nwant=%s", strings.Join(crf, " "), wantCRF)
	}
	// 目标码率模式同样逐字冻结（回归面）。
	br := TranscodeArgs(TranscodeRequest{
		Src: "in.mp4", Dst: "out.mp4", Width: 854, Height: 480,
		VideoKbps: 800, AudioKbps: 96, DurationSec: 5, Encoder: EncoderCPU, Mode: ModeBitrate,
	}, 0)
	wantBR := strings.Replace(wantCRF, "-crf 26 ", "-b:v 800k ", 1)
	if strings.Join(br, " ") != wantBR {
		t.Errorf("CPU 目标码率的 argv 变了：\n got=%s\nwant=%s", strings.Join(br, " "), wantBR)
	}
}

// ⑤ 同级更小（用户点名的对比口径）：质量档要跟**对应的**固定码率档配对比较，
// 而不是跟该级别最小的固定档比 —— 用户原话"同级别的选择要比固定码率时文件更小"。
// 配对规则：同一级别里，第 i 个质量档 ↔ 第 i 个固定码率档
// （1080p：质量 35↔3000k、45↔4500k、50↔6000k；360/480/720p 同理按顺序配）。
// 三样都必须严格更小：上限（实际码率天花板）、预计实际码率、预测体积。
// 不跑真 ffmpeg：全部按标定表 + 源时长算。
// 变异（必须变红）：把某个质量档上限抬到 ≥ 它配对的固定码率档。
func TestHWQualitySmallerThanBitrateGate(t *testing.T) {
	const audioKbps, durSec = 96.0, 7200.0
	for _, id := range []string{"360p", "480p", "720p", "1080p"} {
		p := presetID(t, id)
		choices := BitrateChoices(p)
		if len(choices) != len(hwQualityCalib) {
			t.Fatalf("%s 级别的固定码率档有 %d 个，质量档有 %d 个，配对数不上",
				id, len(choices), len(hwQualityCalib))
		}
		for i, tier := range hwQualityCalib {
			pairKbps := choices[i].KBps
			if pairKbps <= 0 {
				t.Fatalf("%s 第 %d 个固定码率档不是绝对码率（%d），配对口径不成立", id, i, pairKbps)
			}
			capKbps := HWQualityCapKbps(tier.Quality, p, 1920, 1080)
			estKbps := HWQualityEstimateKbps(capKbps)
			if capKbps >= pairKbps {
				t.Errorf("%s 质量档 %d vs 配对固定档 %d kbps：上限 %d kbps 没有更小",
					id, tier.Quality, pairKbps, capKbps)
			}
			if estKbps >= pairKbps {
				t.Errorf("%s 质量档 %d vs 配对固定档 %d kbps：预计实际 %d kbps 没有更小",
					id, tier.Quality, pairKbps, estKbps)
			}
			want := estimateBytes(estKbps, int(audioKbps), durSec)
			pair := estimateBytes(pairKbps, int(audioKbps), durSec)
			if want >= pair {
				t.Errorf("%s 质量档 %d vs 配对固定档 %d kbps：预计体积 %d 字节 ≥ %d 字节",
					id, tier.Quality, pairKbps, want, pair)
			}
		}
	}
}

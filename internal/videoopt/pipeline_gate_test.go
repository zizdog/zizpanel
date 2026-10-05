package videoopt

// pipeline_gate_test.go —— 「按源类型给提示」的判据门禁（纯函数那一半）。
//
// 为什么需要：面板要在点「开始」之前告诉用户"这条源会走哪条管线、为什么"。
// 判据只能有一份（PipelineDecision），前端只按它下发的字段渲染；这条门禁钉住
// 判据本身 + 与 needFastDecode 的等价性，接口/前端另有两半（internal/web）。
//
// 变异测试（必须变红）：把 low_bitrate 与 4k_10bit_highbitrate 判反、
// 把"位深读不到"当成 fast。

import "testing"

func TestPipelineDecisionGate(t *testing.T) {
	cases := []struct {
		name       string
		info       MediaInfo
		wantLine   string
		wantReason string
	}{
		{
			"用户片源 4K60 10bit 25Mbps",
			MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 25283, FPS: 60},
			PipelineFast, PipelineReason4K10BitHighBitrate,
		},
		{
			"4K 10bit 8Mbps", // 判据的下界（≥6Mbps 即快路）
			MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 8000},
			PipelineFast, PipelineReason4K10BitHighBitrate,
		},
		{
			"4K 10bit 3Mbps：软解更快",
			MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 3000},
			PipelineSoftware, PipelineReasonLowBitrate,
		},
		{
			"4K 8bit 25Mbps：硬解慢 6 倍",
			MediaInfo{Width: 3840, Height: 1608, BitDepth: 8, VideoKbps: 25000},
			PipelineSoftware, PipelineReasonBitDepth8,
		},
		{
			"1080p 10bit 8Mbps：像素太少，软解够快",
			MediaInfo{Width: 1920, Height: 1080, BitDepth: 10, VideoKbps: 8000},
			PipelineSoftware, PipelineReasonNot4K,
		},
		{
			"4K 10bit 但码率读不到 ⇒ 不猜",
			MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 0},
			PipelineSoftware, PipelineReasonUnknown,
		},
		{
			"4K 高码率但位深读不到 ⇒ 不猜",
			MediaInfo{Width: 3840, Height: 1608, BitDepth: 0, VideoKbps: 25000},
			PipelineSoftware, PipelineReasonUnknown,
		},
		{
			"连分辨率都读不到 ⇒ 不猜",
			MediaInfo{Width: 0, Height: 0, BitDepth: 0, VideoKbps: 0},
			PipelineSoftware, PipelineReasonUnknown,
		},
		{
			"1080p 位深读不到：结论已定，照实给 not_4k（不报 unknown）",
			MediaInfo{Width: 1920, Height: 1080, BitDepth: 0, VideoKbps: 0},
			PipelineSoftware, PipelineReasonNot4K,
		},
	}
	for _, c := range cases {
		gotLine, gotReason := PipelineDecision(c.info)
		if gotLine != c.wantLine || gotReason != c.wantReason {
			t.Errorf("%s：PipelineDecision=%s/%s，应为 %s/%s",
				c.name, gotLine, gotReason, c.wantLine, c.wantReason)
		}
		// 等价性：pipeline == fast ⟺ needFastDecode（两处判据绝不能走样）。
		if got := needFastDecode(c.info); got != (gotLine == PipelineFast) {
			t.Errorf("%s：needFastDecode=%v 与 PipelineDecision=%s 不一致", c.name, got, gotLine)
		}
	}

	t.Run("PlanOne 把判据与事实真的写进行里", func(t *testing.T) {
		preset, _ := FindPreset("1080p")
		info := MediaInfo{
			Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 25283, FPS: 60,
			DurationSec: 10, FileBytes: 1 << 20, HasVideo: true, HasAudio: true,
		}
		p := PlanOne("a.mkv", "/tmp/a.mkv", "/tmp/out", info,
			Options{Preset: preset, Encoder: EncoderHardware, Mode: ModeBitrate}, false)
		if p.Pipeline != PipelineFast || p.PipelineReason != PipelineReason4K10BitHighBitrate {
			t.Fatalf("快路源必须写 fast/4k_10bit_highbitrate：%s/%s", p.Pipeline, p.PipelineReason)
		}
		if p.SrcBitrateKbps != 25283 || p.SrcBitDepth != 10 || p.SrcFPS != 60 ||
			p.SourceBitDepth != 10 || p.SourceFPS != 60 || !p.CapFPS30 {
			t.Fatalf("结构化事实没写全：%+v", p)
		}
		// CPU 档同一份源：源判据不变（面板提示按源给，与当前编码器选择无关）。
		cpu := PlanOne("a.mkv", "/tmp/a.mkv", "/tmp/out", info,
			Options{Preset: preset, Encoder: EncoderCPU, Mode: ModeBitrate}, false)
		if cpu.Pipeline != PipelineFast || cpu.PipelineReason != PipelineReason4K10BitHighBitrate {
			t.Fatalf("CPU 档下源判据不该变：%s/%s", cpu.Pipeline, cpu.PipelineReason)
		}
	})
}

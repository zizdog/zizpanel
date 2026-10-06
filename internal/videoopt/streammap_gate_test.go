package videoopt

// streammap_gate_test.go —— 「转码不许丢流」的门禁（坑 239）。
//
// 事故（2026-10-06 用户报障）：多音轨电影转码后**多余音轨全没了**，用户只能重新下载。
// 根因：TranscodeArgs 里根本没有 `-map` —— ffmpeg 的默认流选择只挑"最好的一条"视频/
// 音频/字幕，多音轨（国语/粤语/英语）会被**静默**砍到只剩第一条。
//
// 这个门禁锁两件事：
//  ① argv 必须显式映射：第一条视频 + **全部音轨** + 每条文本字幕；音频关掉时才允许 -an；
//  ② 探测结果要如实数出音轨条数与文本/图形字幕（执行器靠它回读核对）。
// 负向对照：把 TranscodeArgs 里的 `-map 0:a` 去掉，第一组断言立刻红。

import (
	"strings"
	"testing"
)

// TestTranscodeArgsKeepsEveryStream 是这条事故的直接门禁。
func TestTranscodeArgsKeepsEveryStream(t *testing.T) {
	base := TranscodeRequest{
		Src: "/tmp/in.mkv", Dst: "/tmp/out.mp4", Width: 1920, Height: 1080,
		VideoKbps: 3000, AudioKbps: 192, DurationSec: 600, Encoder: EncoderCPU, Mode: ModeBitrate,
		AudioStreams: 3, TextSubtitleIndexes: []int{0, 2},
	}
	args := TranscodeArgs(base, 0)
	joined := strings.Join(args, " ")

	// ① 视频只留第一条（封面/多角度不该混进产物）。
	if !strings.Contains(joined, "-map 0:v:0") {
		t.Errorf("缺少 `-map 0:v:0`（不写映射时 ffmpeg 会自作主张挑流）：%s", joined)
	}
	// ② **全部音轨**：这是本次事故的核心。
	if !strings.Contains(joined, "-map 0:a") {
		t.Fatalf("缺少 `-map 0:a`：多音轨影片会被 ffmpeg 静默砍到只剩第一条（坑 239 复发）")
	}
	if !strings.Contains(joined, "-c:a aac") || !strings.Contains(joined, "-b:a 192k") {
		t.Errorf("音轨编码参数丢了：%s", joined)
	}
	// ③ 文本字幕逐条映射（0:s:0 与 0:s:2）并转 mov_text；图形字幕不在这里。
	for _, want := range []string{"-map 0:s:0", "-map 0:s:2", "-c:s mov_text"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少 %q：文本字幕会被丢掉或容器不认：%s", want, joined)
		}
	}
	if strings.Contains(joined, "-map 0:s:1") {
		t.Errorf("把没在 TextSubtitleIndexes 里的字幕也映射了：%s", joined)
	}
	// ④ 音轨开着时绝不许出现 -an（-an 会把音轨整个关掉）。
	for i, a := range args {
		if a == "-an" {
			t.Fatalf("第 %d 个参数是 -an：产物会没有音轨：%s", i, joined)
		}
	}

	// ⑤ 用户明确选"不要音轨"时：只有 -an，不许再映射音轨。
	mute := base
	mute.AudioKbps = 0
	muteArgs := TranscodeArgs(mute, 0)
	muteJoined := strings.Join(muteArgs, " ")
	if !strings.Contains(muteJoined, "-an") {
		t.Errorf("选了不要音轨时必须有 -an：%s", muteJoined)
	}
	if strings.Contains(muteJoined, "-map 0:a") {
		t.Errorf("选了不要音轨却还在映射音轨：%s", muteJoined)
	}

	// ⑥ 2-pass 第一遍只做分析：不要音轨、不写成品（映射只属于出片那一遍）。
	pass1 := strings.Join(TranscodeArgs(base, 1), " ")
	if !strings.Contains(pass1, "-an") || strings.Contains(pass1, "-map 0:a") {
		t.Errorf("2-pass 第一遍不该带流映射：%s", pass1)
	}
}

// TestParseProbeCountsStreams 保证执行器拿到的"源片有几条音轨/几条文本字幕"是真的。
func TestParseProbeCountsStreams(t *testing.T) {
	raw := []byte(`{"streams":[
		{"index":0,"codec_type":"video","codec_name":"hevc","width":1920,"height":1080},
		{"index":1,"codec_type":"audio","codec_name":"aac","bit_rate":"192000"},
		{"index":2,"codec_type":"audio","codec_name":"ac3","bit_rate":"448000"},
		{"index":3,"codec_type":"audio","codec_name":"eac3","bit_rate":"256000"},
		{"index":4,"codec_type":"subtitle","codec_name":"subrip"},
		{"index":5,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle"},
		{"index":6,"codec_type":"subtitle","codec_name":"ass"}],
		"format":{"duration":"600.0","bit_rate":"8000000","size":"600000000"}}`)
	info, err := ParseProbeJSON(raw, 600000000)
	if err != nil {
		t.Fatal(err)
	}
	if info.AudioStreams != 3 {
		t.Errorf("音轨条数应为 3，实际 %d（回读核对靠它，数错了等于没核对）", info.AudioStreams)
	}
	if info.BitmapSubtitles != 1 {
		t.Errorf("图形字幕（PGS）应计 1 条，实际 %d", info.BitmapSubtitles)
	}
	// 文本字幕在"字幕流内"的序号是 0 与 2（PGS 占了 1）。
	if got := len(info.TextSubtitleIndexes); got != 2 {
		t.Fatalf("文本字幕应 2 条，实际 %d（%v）", got, info.TextSubtitleIndexes)
	}
	if info.TextSubtitleIndexes[0] != 0 || info.TextSubtitleIndexes[1] != 2 {
		t.Errorf("文本字幕序号应为 [0 2]（-map 0:s:N 用的就是它），实际 %v", info.TextSubtitleIndexes)
	}

	// 规划层要把这些数字带给执行器，否则回读核对无从谈起。
	preset, _ := FindPreset("1080p")
	plan := PlanOne("a.mkv", "/tmp/a.mkv", "/tmp/out", info,
		Options{Preset: preset, Encoder: EncoderCPU, Mode: ModeBitrate}, false)
	if plan.AudioStreams != 3 || plan.TextSubtitles != 2 || plan.BitmapSubtitles != 1 {
		t.Errorf("计划没把流信息带下去：audio=%d text=%d bitmap=%d",
			plan.AudioStreams, plan.TextSubtitles, plan.BitmapSubtitles)
	}
	if len(plan.TextSubtitleIndexes) != 2 {
		t.Errorf("计划丢了文本字幕序号：%v", plan.TextSubtitleIndexes)
	}
}

package services

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  「macOS 语音合成（say）」的单测
//
//  纪律（AGENTS 第三节）：**不许碰真实服务、真实家目录**。
//  所以这里
//    · 音色解析喂的是**假的** `say -v '?'` 输出（真的那份会随系统语言变）；
//    · 合成链路用注入的 SpeechCommandRunner，绝不真的跑 say / afconvert；
//    · 安装/卸载用注入的 plist 路径与 launchd 动作，绝不写 /Library/LaunchDaemons。
//
//  重点锁住的是**静默失败**这一类（本应用最容易谎报成功的地方）：
//    · say 对未知音色返回退出码 0 却不产出文件 → 必须报错；
//    · say 对空文本返回 0 并产出只有头的文件 → 必须由我们拦住；
//    · 没有 ffmpeg 时 mp3 → 必须如实说"不可用"，而不是给一个坏文件。
// ============================================================================

// sayVoicesFixture 是一份**假**的 `say -v '?'` 输出（形状照实测那份）：
// 名字里有空格、有中文全角括号、有 3 位语言代码、有一行没有示例、还有一行
// 是"表头"（解析不出来，必须被跳过而不是猜）。
const sayVoicesFixture = `Albert              en_US    # Hello! My name is Albert.
Bad News            en_US    # Hello! My name is Bad News.
Eddy (中文（中国大陆）)     zh_CN    # 你好！我叫Eddy。
Meijia              zh_TW    # 你好，我叫美佳。
Sinji               zh_HK    # 你好！我叫善怡。
Tingting            zh_CN    # 你好！我叫婷婷。
Amélie              fr_CA    # Bonjour! Je m’appelle Amélie.
Damayanti           id_ID    # Halo! Nama saya Damayanti.
Nora                ar_001   # مرحبا
NoExampleVoice      en_GB
Name                Lang     Example
Tingting            zh_CN    # 重复行：必须被去重
`

func TestParseSayVoices(t *testing.T) {
	voices := parseSayVoices(sayVoicesFixture)
	if len(voices) != 10 {
		t.Fatalf("应解析出 10 个音色（跳过表头、去重重复项），实际 %d：%+v", len(voices), voices)
	}
	byName := map[string]SpeechVoice{}
	for _, v := range voices {
		byName[v.Name] = v
	}
	// 带空格的名字
	if v, ok := byName["Bad News"]; !ok || v.Lang != "en_US" {
		t.Errorf("带空格的名字解析错了：%+v", v)
	}
	// 带中文全角括号的名字（最容易切错的一种）
	if v, ok := byName["Eddy (中文（中国大陆）)"]; !ok || v.Lang != "zh_CN" || !v.Chinese {
		t.Errorf("带括号的中文名字解析错了：%+v", v)
	} else if v.Example != "你好！我叫Eddy。" {
		t.Errorf("示例文本解析错了：%q", v.Example)
	}
	// 3 位语言代码（ar_001）
	if v, ok := byName["Nora"]; !ok || v.Lang != "ar_001" || v.Chinese {
		t.Errorf("ar_001 解析错了：%+v", v)
	}
	// 没有示例的行也要收（没有 # 就只有名字+语言）
	if v, ok := byName["NoExampleVoice"]; !ok || v.Example != "" {
		t.Errorf("没有示例的行解析错了：%+v", v)
	}
	// 去重
	n := 0
	for _, v := range voices {
		if v.Name == "Tingting" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("重复音色应被去重，实际出现 %d 次", n)
	}
	// 中文计数
	zh := 0
	for _, v := range voices {
		if v.Chinese {
			zh++
		}
	}
	if zh != 4 {
		t.Errorf("中文音色应为 4 个（Eddy/Meijia/Sinji/Tingting），实际 %d", zh)
	}
}

func TestParseSayVoicesEmptyInput(t *testing.T) {
	if got := parseSayVoices(""); len(got) != 0 {
		t.Errorf("空输入应得到空列表，实际 %+v", got)
	}
	if got := parseSayVoices("\n  \n"); len(got) != 0 {
		t.Errorf("纯空白应得到空列表，实际 %+v", got)
	}
}

func TestLookupSpeechVoice(t *testing.T) {
	voices := parseSayVoices(sayVoicesFixture)
	if _, ok := LookupSpeechVoice(voices, "tingting"); !ok {
		t.Error("查音色应大小写不敏感")
	}
	if _, ok := LookupSpeechVoice(voices, "  Tingting  "); !ok {
		t.Error("查音色应忽略两端空白")
	}
	if _, ok := LookupSpeechVoice(voices, "TingTing"); !ok {
		t.Error("查音色应大小写不敏感（混合大小写）")
	}
	if _, ok := LookupSpeechVoice(voices, "NoSuchVoice"); ok {
		t.Error("不存在的音色不能被查出来（那会让 say 静默失败）")
	}
	if _, ok := LookupSpeechVoice(voices, ""); ok {
		t.Error("空音色名不能被当成有效音色")
	}
}

func TestPickDefaultVoice(t *testing.T) {
	voices := parseSayVoices(sayVoicesFixture)
	v, ok := pickDefaultVoice(voices, "你好，世界")
	if !ok || v.Name != "Tingting" {
		t.Errorf("中文文本应默认挑 Tingting，实际 %+v ok=%v", v, ok)
	}
	if _, ok := pickDefaultVoice(voices, "hello world"); ok {
		t.Error("英文文本不该被自动换成中文音色（应交给 say 自己的默认）")
	}
	// 没有中文音色时不该硬塞一个
	only := parseSayVoices("Albert en_US # hi\n")
	if _, ok := pickDefaultVoice(only, "你好"); ok {
		t.Error("没有中文音色时不该挑出一个非中文音色")
	}
}

func TestParseSpeechFormat(t *testing.T) {
	cases := map[string]SpeechFormat{
		"": SpeechFormatAIFF, "aiff": SpeechFormatAIFF, "AIF": SpeechFormatAIFF,
		"wav": SpeechFormatWAV, "WAVE": SpeechFormatWAV,
		"m4a": SpeechFormatM4A, "mp4": SpeechFormatM4A, "aac": SpeechFormatM4A,
		"mp3": SpeechFormatMP3, "mpeg": SpeechFormatMP3,
	}
	for in, want := range cases {
		got, err := ParseSpeechFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseSpeechFormat(%q) = %v, %v；想要 %v", in, got, err, want)
		}
	}
	if _, err := ParseSpeechFormat("flac"); !errors.Is(err, ErrSpeechUnsupportedFormat) {
		t.Errorf("不支持的格式必须返回 ErrSpeechUnsupportedFormat，实际 %v", err)
	}
	// 内容类型要能被浏览器正确识别（UI 用它决定 <audio> 能不能播）
	if SpeechFormatM4A.ContentType() != "audio/mp4" || SpeechFormatMP3.ContentType() != "audio/mpeg" {
		t.Error("格式的 Content-Type 不对")
	}
}

func TestRateFromSpeedClamps(t *testing.T) {
	if got := RateFromSpeed(0); got != MacSpeechDefaultRate {
		t.Errorf("speed=0 应用默认语速 %d，实际 %d", MacSpeechDefaultRate, got)
	}
	if got := RateFromSpeed(1); got != MacSpeechDefaultRate {
		t.Errorf("speed=1 应是默认语速，实际 %d", got)
	}
	// 超范围必须**夹取**（say 对 -r 9999 实测也返回 0，直接透传就是静默乱来），
	// 由调用方把实际值回报给用户。
	if got := RateFromSpeed(100); got != MacSpeechMaxRate {
		t.Errorf("超大 speed 应夹到 %d，实际 %d", MacSpeechMaxRate, got)
	}
	if got := RateFromSpeed(0.01); got != MacSpeechMinRate {
		t.Errorf("超小 speed 应夹到 %d，实际 %d", MacSpeechMinRate, got)
	}
	if got := SpeedFromRate(RateFromSpeed(1.5)); got < 1.4 || got > 1.6 {
		t.Errorf("语速往返换算应大致相等，实际 %v", got)
	}
}

func TestSplitSpeechText(t *testing.T) {
	if got := SplitSpeechText("   ", 100); got != nil {
		t.Errorf("空白文本应返回 nil，实际 %#v", got)
	}
	got := SplitSpeechText("第一句。第二句！第三句？", 100)
	if len(got) != 1 {
		t.Fatalf("短文本不该被切分，实际 %#v", got)
	}
	in := strings.Repeat("这是一句测试文本。", 20) // 180 字
	got = SplitSpeechText(in, 60)
	if len(got) < 3 {
		t.Fatalf("180 字按 60 字切应至少 3 段，实际 %d 段：%#v", len(got), got)
	}
	for i, seg := range got {
		if n := len([]rune(seg)); n > 60 {
			t.Errorf("第 %d 段 %d 字，超过上限 60", i+1, n)
		}
	}
	// **不许丢字符**：切分只是分段，不是截断。
	if joined := strings.Join(got, ""); strings.ReplaceAll(joined, " ", "") != in {
		t.Errorf("切分后内容变了：\n得到 %q\n想要 %q", joined, in)
	}
	// 单句超长：在字符边界硬切，且不丢字符。
	long := strings.Repeat("啊", 250)
	got = SplitSpeechText(long, 100)
	if len(got) != 3 {
		t.Fatalf("250 字无标点文本按 100 硬切应有 3 段，实际 %d：%#v", len(got), got)
	}
	if strings.Join(got, "") != long {
		t.Error("硬切后字符对不上（丢了或多了字符）")
	}
}

// testWAV 造一个合法的 PCM WAV；withFiller=true 时在 fmt 与 data 之间插一个
// `fLLR` 块 —— afconvert 产出的 WAV 就长这样（实测 data 从 4096 开始，
// 不是 44），专门用来证明拼接走的是 chunk 链而不是"硬跳 44 字节"。
func testWAV(samples []int16, withFiller bool) []byte {
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}
	fillerLen := 0
	if withFiller {
		fillerLen = 8 + 12 // 一个假块（8 字节头 + 12 字节内容）
	}
	total := 12 + (8 + 16) + fillerLen + (8 + len(data))
	out := make([]byte, 0, total)
	le32 := func(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
	le16 := func(v uint16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, v); return b }
	out = append(out, "RIFF"...)
	out = append(out, le32(uint32(total-8))...)
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	out = append(out, le32(16)...)
	out = append(out, le16(1)...) // PCM
	out = append(out, le16(1)...) // 单声道
	out = append(out, le32(MacSpeechSampleRate)...)
	out = append(out, le32(MacSpeechSampleRate*2)...)
	out = append(out, le16(2)...)  // block align
	out = append(out, le16(16)...) // bits
	if withFiller {
		out = append(out, "fLLR"...)
		out = append(out, le32(12)...)
		out = append(out, make([]byte, 12)...)
	}
	out = append(out, "data"...)
	out = append(out, le32(uint32(len(data)))...)
	out = append(out, data...)
	return out
}

func TestConcatWAVSkipsFillerChunks(t *testing.T) {
	a := testWAV([]int16{1, 2, 3}, true) // 带 fLLR 填充块
	b := testWAV([]int16{4, 5}, true)
	merged, err := ConcatWAV([][]byte{a, b})
	if err != nil {
		t.Fatalf("拼接失败: %v", err)
	}
	info, err := parseWAVPCM(merged)
	if err != nil {
		t.Fatalf("拼接结果不是合法 WAV: %v", err)
	}
	if info.DataLen != 10 {
		t.Fatalf("拼接后音频数据应为 5 个 16bit 样本 = 10 字节，实际 %d（说明把填充块也当音频拼进去了）", info.DataLen)
	}
	var got []int16
	for i := 0; i < 5; i++ {
		got = append(got, int16(binary.LittleEndian.Uint16(merged[info.DataOffset+i*2:])))
	}
	want := []int16{1, 2, 3, 4, 5}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("拼接顺序不对：%v，想要 %v", got, want)
		}
	}
	// 头部必须是标准的 44 字节（不是把第一段的 4096 字节头原样搬过来）
	if info.DataOffset != 44 {
		t.Errorf("拼接结果的数据偏移应是 44，实际 %d", info.DataOffset)
	}
	if int(binary.LittleEndian.Uint32(merged[4:8])) != len(merged)-8 {
		t.Error("RIFF 长度字段不对（播放器会截断或报损坏）")
	}
}

func TestConcatWAVRejectsBadInput(t *testing.T) {
	if _, err := ConcatWAV(nil); err == nil {
		t.Error("空列表必须报错，不能返回空音频")
	}
	if _, err := ConcatWAV([][]byte{[]byte("not a wav")}); err == nil {
		t.Error("非 WAV 输入必须报错")
	}
	mono := testWAV([]int16{1, 2}, false)
	// 造一个 44.1kHz 的"另一段"：格式不一致必须报错（拼起来只会是噪声）
	other := testWAV([]int16{3, 4}, false)
	binary.LittleEndian.PutUint32(other[24:28], 44100)
	if _, err := ConcatWAV([][]byte{mono, other}); err == nil {
		t.Error("格式不一致的两段必须报错")
	}
}

// fakeSpeechEngine 造一个"引擎可用"的假环境：
//   - say / afconvert / ffmpeg 都是临时目录里真实存在且可执行的文件
//     （Available() 判的是文件在不在、有没有执行位，不是"能不能跑"）；
//   - Run 由调用方注入，绝不真的执行；
//   - 记录所有被调用的命令，供断言"到底走了哪条链路"。
func fakeSpeechEngine(t *testing.T, run SpeechCommandRunner) (*SpeechEngine, *[][]string) {
	t.Helper()
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	calls := &[][]string{}
	eng := &SpeechEngine{
		SayBin:       mk("say"),
		AfconvertBin: mk("afconvert"),
		FfmpegBin:    mk("ffmpeg"),
		TempRoot:     dir,
		MaxChars:     1000,
		ChunkChars:   50,
		Run: func(ctx context.Context, timeout time.Duration, bin string, args ...string) (string, error) {
			*calls = append(*calls, append([]string{bin}, args...))
			return run(ctx, timeout, bin, args...)
		},
	}
	return eng, calls
}

// realFakeRunner 模拟 say + afconvert + ffmpeg 的**正常**行为：
// say 写一个非空 aiff，afconvert 写一个合法 WAV，ffmpeg 写一个非空 mp3。
func realFakeRunner(t *testing.T) SpeechCommandRunner {
	t.Helper()
	return func(_ context.Context, _ time.Duration, bin string, args ...string) (string, error) {
		base := filepath.Base(bin)
		switch {
		case base == "say":
			if len(args) >= 2 && args[0] == "-v" && args[1] == "?" {
				return sayVoicesFixture, nil // 音色探测
			}
			return "", writeAt(args, "-o", []byte("FAKE-AIFF-BYTES"))
		case base == "afconvert":
			// 最后一个参数是输出
			out := args[len(args)-1]
			return "", os.WriteFile(out, testWAV([]int16{10, 20, 30}, true), 0o644)
		case base == "ffmpeg":
			out := args[len(args)-1]
			return "", os.WriteFile(out, []byte("FAKE-MP3-BYTES"), 0o644)
		}
		return "", errors.New("意外的命令: " + bin)
	}
}

func writeAt(args []string, flag string, body []byte) error {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return os.WriteFile(args[i+1], body, 0o644)
		}
	}
	return errors.New("没找到 " + flag + " 参数")
}

func TestSynthesizeHappyPath(t *testing.T) {
	eng, calls := fakeSpeechEngine(t, realFakeRunner(t))
	res, err := eng.Synthesize(context.Background(), SynthOptions{
		Text: "你好，这是一段测试文本。", Voice: "Tingting", Format: SpeechFormatWAV,
	})
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	defer res.Cleanup()
	if res.Voice != "Tingting" || res.Lang != "zh_CN" {
		t.Errorf("实际音色不对：%+v", res)
	}
	if res.Format != SpeechFormatWAV || res.ContentType != "audio/wav" {
		t.Errorf("格式不对：%+v", res)
	}
	if res.Rate != MacSpeechDefaultRate {
		t.Errorf("默认语速应是 %d，实际 %d", MacSpeechDefaultRate, res.Rate)
	}
	st, err := os.Stat(res.Path)
	if err != nil || st.Size() == 0 {
		t.Fatalf("产物不存在或为空：%v", err)
	}
	// 清理必须真的删掉临时目录（否则每次合成都会在 /tmp 里留东西）
	dir := res.WorkDir
	res.Cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Cleanup 没有删掉临时目录 %s", dir)
	}
	// 链路必须包含 say 与 afconvert（格式转换真的做了，而不是把 say 的产物
	// 直接当 wav 交给用户）
	var sawSay, sawAfconvert bool
	for _, c := range *calls {
		if filepath.Base(c[0]) == "say" {
			sawSay = true
		}
		if filepath.Base(c[0]) == "afconvert" {
			sawAfconvert = true
		}
	}
	if !sawSay || !sawAfconvert {
		t.Errorf("合成链路不完整（say=%v afconvert=%v）：%v", sawSay, sawAfconvert, *calls)
	}
}

// TestSynthesizeFailsWhenSaySilentlySucceeds 锁住本应用最容易谎报成功的一条：
// `say -v 未知音色`（以及别的静默失败）返回退出码 0 **但不产出文件** ——
// 实测确认过。引擎必须报错，绝不能返回一个 0 字节的"成功"。
func TestSynthesizeFailsWhenSaySilentlySucceeds(t *testing.T) {
	eng, _ := fakeSpeechEngine(t, func(_ context.Context, _ time.Duration, bin string, _ ...string) (string, error) {
		// 模拟 say 的静默成功：什么都不写，返回成功。
		return "", nil
	})
	_, err := eng.Synthesize(context.Background(), SynthOptions{Text: "你好", Voice: "Tingting"})
	if err == nil {
		t.Fatal("say 没有产出文件时必须报错（否则就是把 0 字节当成功交给用户）")
	}
	if !errors.Is(err, ErrSpeechEngineUnavailable) {
		t.Errorf("这类静默失败应可被识别为引擎不可用，实际 %v", err)
	}
}

func TestSynthesizeRequestValidation(t *testing.T) {
	ctx := context.Background()
	eng, _ := fakeSpeechEngine(t, realFakeRunner(t))

	if _, err := eng.Synthesize(ctx, SynthOptions{Text: "   "}); !errors.Is(err, ErrSpeechEmptyText) {
		t.Errorf("空文本应被拒绝（say 对空文本会返回 0 并产出只有头的文件），实际 %v", err)
	}
	if _, err := eng.Synthesize(ctx, SynthOptions{Text: strings.Repeat("啊", 1001)}); !errors.Is(err, ErrSpeechTextTooLong) {
		t.Errorf("超长文本应被拒绝，实际 %v", err)
	}
	if _, err := eng.Synthesize(ctx, SynthOptions{Text: "你好", Voice: "NoSuchVoice"}); !errors.Is(err, ErrSpeechUnknownVoice) {
		t.Errorf("未知音色应被拒绝，实际 %v", err)
	}
	if _, err := eng.Synthesize(ctx, SynthOptions{Text: "你好", Format: "flac"}); !errors.Is(err, ErrSpeechUnsupportedFormat) {
		t.Errorf("未知格式应被拒绝，实际 %v", err)
	}
	// 没有 ffmpeg 时 mp3 必须**当场**如实说不可用，而不是产出一个坏文件
	noFF := &SpeechEngine{SayBin: eng.SayBin, AfconvertBin: eng.AfconvertBin, Run: eng.Run, TempRoot: eng.TempRoot}
	if _, err := noFF.Synthesize(ctx, SynthOptions{Text: "你好", Format: SpeechFormatMP3}); !errors.Is(err, ErrSpeechUnsupportedFormat) {
		t.Errorf("没有 ffmpeg 时的 mp3 应报 ErrSpeechUnsupportedFormat，实际 %v", err)
	}
	// say 不存在时（例如非 macOS）必须如实失败
	missing := &SpeechEngine{SayBin: filepath.Join(t.TempDir(), "nope"), Run: eng.Run}
	if _, err := missing.Synthesize(ctx, SynthOptions{Text: "你好"}); !errors.Is(err, ErrSpeechEngineUnavailable) {
		t.Errorf("say 不存在时应报引擎不可用，实际 %v", err)
	}
}

func TestSynthesizeLongTextReportsRealProgress(t *testing.T) {
	eng, _ := fakeSpeechEngine(t, realFakeRunner(t))
	var progress [][2]int
	res, err := eng.Synthesize(context.Background(), SynthOptions{
		Text:  strings.Repeat("这是一句用于分段的中文文本。", 10), // 150 字 → 按 50 字切
		Voice: "Tingting",
		Progress: func(done, total int, note string) {
			progress = append(progress, [2]int{done, total})
			if note == "" {
				t.Error("进度回调必须带可读说明（界面要显示它）")
			}
		},
	})
	if err != nil {
		t.Fatalf("长文本合成失败: %v", err)
	}
	defer res.Cleanup()
	if res.Segments < 3 {
		t.Fatalf("150 字按 50 字应切成至少 3 段，实际 %d", res.Segments)
	}
	if len(progress) != res.Segments {
		t.Errorf("进度回调次数应等于段数（%d），实际 %d", res.Segments, len(progress))
	}
	for i, p := range progress {
		if p[0] != i+1 || p[1] != res.Segments {
			t.Errorf("第 %d 次进度应是 %d/%d，实际 %d/%d", i+1, i+1, res.Segments, p[0], p[1])
		}
	}
}

func TestEngineHealth(t *testing.T) {
	eng, _ := fakeSpeechEngine(t, func(_ context.Context, _ time.Duration, bin string, args ...string) (string, error) {
		if filepath.Base(bin) == "say" {
			return sayVoicesFixture, nil
		}
		return "", nil
	})
	h := eng.Health(context.Background())
	if !h.OK {
		t.Fatalf("引擎可用时 health 必须 ok:true，实际 %+v", h)
	}
	if h.Voices != 10 || h.ChineseVoices != 4 {
		t.Errorf("音色计数不对：总数 %d / 中文 %d（想要 10 / 4）", h.Voices, h.ChineseVoices)
	}
	if !h.SayPresent || !h.AfconvertReady {
		t.Errorf("say/afconvert 的可用性没被如实标出：%+v", h)
	}
	if len(h.Formats) != 4 {
		t.Errorf("格式可用性应逐个列出（4 个），实际 %d", len(h.Formats))
	}

	// say 不存在：必须 ok:false 且给出理由（绝不绿灯）
	missing := &SpeechEngine{SayBin: filepath.Join(t.TempDir(), "nope"), Run: eng.Run}
	mh := missing.Health(context.Background())
	if mh.OK || mh.Reason == "" {
		t.Errorf("say 不存在时 health 必须 ok:false 且写清理由，实际 %+v", mh)
	}
	// say 存在但报不出音色：同样必须 ok:false
	silent := &SpeechEngine{SayBin: eng.SayBin, Run: func(context.Context, time.Duration, string, ...string) (string, error) {
		return "", nil
	}}
	sh := silent.Health(context.Background())
	if sh.OK || sh.Reason == "" {
		t.Errorf("say 报不出音色时 health 必须 ok:false 且写清理由，实际 %+v", sh)
	}
}

// ---------------------------------------------------------------------------
//  安装 / 卸载
// ---------------------------------------------------------------------------

func TestMacSpeechPrerequisitesFailsHonestlyWithoutSay(t *testing.T) {
	m, _ := sandboxManager(t)
	missing := &SpeechEngine{SayBin: filepath.Join(t.TempDir(), "nope")}
	res := &InstallResult{Steps: []string{}}
	if _, err := m.MacSpeechPrerequisites(context.Background(), res, missing); err == nil {
		t.Fatal("没有 say 时必须如实失败（装一个合成不出声音的服务才是谎报成功）")
	}
	if len(res.Steps) != 0 {
		t.Errorf("前置检查失败时不该声称做成了任何事：%v", res.Steps)
	}
}

func TestMacSpeechPrerequisitesReportsVoices(t *testing.T) {
	m, _ := sandboxManager(t)
	eng, _ := fakeSpeechEngine(t, func(_ context.Context, _ time.Duration, bin string, _ ...string) (string, error) {
		return sayVoicesFixture, nil
	})
	res := &InstallResult{Steps: []string{}}
	voices, err := m.MacSpeechPrerequisites(context.Background(), res, eng)
	if err != nil {
		t.Fatalf("引擎可用时前置检查不该失败: %v", err)
	}
	if len(voices) != 10 {
		t.Errorf("应返回 10 个音色，实际 %d", len(voices))
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "10 个音色") || !strings.Contains(joined, "中文 4 个") {
		t.Errorf("安装步骤里应如实报告音色数：%v", res.Steps)
	}
}

func TestMacSpeechPlistContent(t *testing.T) {
	plist := macSpeechPlist("/opt/zizpanel/bin/zizpanel", MacSpeechPort, "zizdog",
		"/tmp/out.log", "/tmp/err.log")
	for _, want := range []string{
		"<string>" + MacSpeechLabel + "</string>",
		"<string>speech-serve</string>",
		"<string>127.0.0.1:8891</string>",
		"<string>zizdog</string>",
		"<string>/tmp/out.log</string>",
		"<key>UserName</key>", // 必须以真实用户运行（音色与临时文件都按该身份）
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist 缺少 %q：\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "0.0.0.0") {
		t.Error("plist 不该把服务暴露到 0.0.0.0（只绑回环）")
	}
}

// TestInstallMacSpeechSandboxed 在沙箱里走一遍安装：
//   - plist 写到临时目录（绝不碰 /Library/LaunchDaemons）；
//   - launchd 动作被注入替换（绝不碰真实 launchd）；
//   - 引擎是假的（绝不真的跑 say）。
func TestInstallMacSpeechSandboxed(t *testing.T) {
	m, _ := sandboxManager(t)
	m.launchdDirsOverride = []string{filepath.Join(t.TempDir(), "LaunchDaemons")}
	plistDir := t.TempDir()
	plistPath := filepath.Join(plistDir, MacSpeechLabel+".plist")

	origPlist, origLaunch, origHealthy, origExe :=
		macSpeechPlistPath, macSpeechLaunch, macSpeechHealthy, macSpeechExecutable
	defer func() {
		macSpeechPlistPath, macSpeechLaunch, macSpeechHealthy, macSpeechExecutable =
			origPlist, origLaunch, origHealthy, origExe
	}()
	macSpeechPlistPath = func() string { return plistPath }
	launched := ""
	macSpeechLaunch = func(_ *Manager, _ context.Context, label, plist string) error {
		launched = label + "|" + plist
		return nil
	}
	macSpeechHealthy = func(context.Context, string, time.Duration) bool { return true }
	macSpeechExecutable = func() (string, error) { return "/opt/zizpanel/bin/zizpanel", nil }

	eng, _ := fakeSpeechEngine(t, func(_ context.Context, _ time.Duration, bin string, _ ...string) (string, error) {
		return sayVoicesFixture, nil
	})
	origEngine := macSpeechEngineOverride
	macSpeechEngineOverride = func() *SpeechEngine { return eng }
	defer func() { macSpeechEngineOverride = origEngine }()

	app, ok := FindApp(MacSpeechAppID)
	if !ok {
		t.Fatalf("目录里应有 %s 条目", MacSpeechAppID)
	}
	res := &InstallResult{Steps: []string{}}
	if err := m.InstallMacSpeech(context.Background(), app, res); err != nil {
		t.Fatalf("沙箱安装应成功: %v", err)
	}
	if launched != MacSpeechLabel+"|"+plistPath {
		t.Errorf("没有装载预期的服务：%q", launched)
	}
	b, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("plist 没写出来: %v", err)
	}
	if !strings.Contains(string(b), "speech-serve") {
		t.Errorf("plist 内容不对：\n%s", b)
	}
	joined := strings.Join(res.Steps, "\n")
	for _, want := range []string{"/speech/", "/v1/audio/speech", "开机自启"} {
		if !strings.Contains(joined, want) {
			t.Errorf("安装步骤里应包含 %q：\n%s", want, joined)
		}
	}
}

func TestUninstallMacSpeechStopsServiceAndKeepsSystemSay(t *testing.T) {
	m, _ := sandboxManager(t)
	plistDir := t.TempDir()
	plistPath := filepath.Join(plistDir, MacSpeechLabel+".plist")
	if err := os.WriteFile(plistPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	origPlist, origStop := macSpeechPlistPath, macSpeechStop
	defer func() { macSpeechPlistPath, macSpeechStop = origPlist, origStop }()
	macSpeechPlistPath = func() string { return plistPath }
	stopped := 0
	macSpeechStop = func(_ *Manager, _ context.Context, label, plist string) error {
		stopped++
		if label != MacSpeechLabel || plist != plistPath {
			t.Errorf("停止服务时参数不对：%s / %s", label, plist)
		}
		return nil
	}

	app, _ := FindApp(MacSpeechAppID)
	res := &InstallResult{Steps: []string{}}
	if err := m.UninstallMacSpeech(context.Background(), app, res); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if stopped != 1 {
		t.Errorf("应正好停止一次服务，实际 %d", stopped)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "/usr/bin/say") {
		t.Errorf("卸载说明里必须写清「系统自带的 say 不会被删」：\n%s", joined)
	}
}

func TestUninstallMacSpeechIsIdempotent(t *testing.T) {
	m, _ := sandboxManager(t)
	origPlist, origStop := macSpeechPlistPath, macSpeechStop
	defer func() { macSpeechPlistPath, macSpeechStop = origPlist, origStop }()
	macSpeechPlistPath = func() string { return filepath.Join(t.TempDir(), "missing.plist") }
	origRepo := m.repo
	m.repo = nil // 没有仓库记录、也没有 plist → 本该"没装"
	defer func() { m.repo = origRepo }()
	called := false
	macSpeechStop = func(*Manager, context.Context, string, string) error { called = true; return nil }

	app, _ := FindApp(MacSpeechAppID)
	res := &InstallResult{Steps: []string{}}
	if err := m.UninstallMacSpeech(context.Background(), app, res); err != nil {
		t.Fatalf("幂等卸载不该报错: %v", err)
	}
	if called {
		t.Error("什么都没装时不该去动 launchd")
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "本来就没有注册") {
		t.Errorf("应如实说明「本来就没注册」：%v", res.Steps)
	}
}

// ---------------------------------------------------------------------------
//  目录 / 下载点声明
// ---------------------------------------------------------------------------

// TestMacSpeechCatalogEntry 锁住这个条目的关键声明：有界面、有端口、有健康检查、
// 有卸载路径（PanelInstaller 必须在卸载实现里登记 —— 见 HasInstallerUninstall）。
func TestMacSpeechCatalogEntry(t *testing.T) {
	app, ok := FindApp(MacSpeechAppID)
	if !ok {
		t.Fatalf("目录里应有 %s 条目", MacSpeechAppID)
	}
	if app.UI == nil || app.UI.Slug != MacSpeechSlug {
		t.Errorf("必须声明界面别名 UI.Slug=%s（否则不会有 /speech/ 入口）：%+v", MacSpeechSlug, app.UI)
	}
	if app.Port != MacSpeechPort || app.HealthPath != "/healthz" {
		t.Errorf("端口/健康检查路径不对：port=%d path=%q", app.Port, app.HealthPath)
	}
	if app.PanelInstaller != "macspeech" {
		t.Errorf("PanelInstaller 应是 macspeech，实际 %q", app.PanelInstaller)
	}
	if !HasInstallerUninstall(app.PanelInstaller) {
		t.Error("有 PanelInstaller 却查不到卸载实现（装上就卸不掉）")
	}
	if app.BrewFormula != "" {
		t.Errorf("这个条目不该有 BrewFormula（引擎是系统自带的 say）：%q", app.BrewFormula)
	}
	// 端口不能和别的条目撞（这里再独立断言一次，便于定位）
	for _, other := range Catalog() {
		if other.ID != app.ID && other.Port == app.Port && other.Port != 0 {
			t.Errorf("端口 %d 与 %s 冲突", app.Port, other.ID)
		}
	}
}

// TestMarketZeroDownloadGateIsNotVacuous 证明"零下载点必须有理由"这条门禁
// **不是空断言**：把理由抽掉必须报错，写够理由必须放行。
func TestMarketZeroDownloadGateIsNotVacuous(t *testing.T) {
	app, _ := FindApp(MacSpeechAppID)
	base := MarketApp{
		ID: app.ID, Kind: app.Kind, PanelInstaller: app.PanelInstaller,
		ServiceLabel: app.ServiceLabel,
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: MacSpeechLabel,
			LabelSource: "目录 ServiceLabel（测试用）",
		},
	}
	// ① 没理由 → 必须报错
	problems := MarketDeclarationProblems(base, app)
	if len(problems) == 0 {
		t.Fatal("零下载点且没有 NoDownloadReason 时必须被门禁拦下（否则漏填下载点会被静默放过）")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "NoDownloadReason") {
		t.Errorf("报错信息要点名 NoDownloadReason：%v", problems)
	}
	// ② 理由太短 → 仍然报错（与镜像站理由同一条规矩）
	base.NoDownloadReason = "不需要"
	if len(MarketDeclarationProblems(base, app)) == 0 {
		t.Error("理由太短也该被拦下（不许写「不需要」三个字就过）")
	}
	// ③ 写够理由 → 放行
	base.NoDownloadReason = strings.Repeat("实测安装不需要任何网络下载：引擎是 macOS 自带的。", 2)
	if problems := MarketDeclarationProblems(base, app); len(problems) != 0 {
		t.Errorf("写清理由后不该再报错：%v", problems)
	}
	// ④ 有下载点却还写 NoDownloadReason → 自相矛盾，必须报错
	base.Downloads = []MarketDownloadPoint{brewBottlePoint("vips", time.Minute, "假的下载点（只为验证规则）")}
	if problems := MarketDeclarationProblems(base, app); len(problems) == 0 {
		t.Error("有下载点又写 NoDownloadReason 属于自相矛盾，必须报错")
	}
}

// TestMacSpeechMarketDeclarationExists 锁住"目录里的条目必须有声明"这条反漂移。
func TestMacSpeechMarketDeclarationExists(t *testing.T) {
	decl, ok := MarketAppFor(MacSpeechAppID)
	if !ok {
		t.Fatalf("market_downloads.go 里应有 %s 的声明", MacSpeechAppID)
	}
	if len(decl.Downloads) != 0 {
		t.Errorf("这个条目不该有下载点（引擎是系统自带的 say）：%+v", decl.Downloads)
	}
	if len([]rune(decl.NoDownloadReason)) < marketInvariantMinReason {
		t.Errorf("零下载点的理由太短（%d 字）", len([]rune(decl.NoDownloadReason)))
	}
}

package tools

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
)

// ---------- 共用脚手架（PATH shim 等） ----------

// shimDir 造一个只含指定假命令的目录，用作临时 PATH。
func shimDir(t *testing.T, cmds map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range cmds {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// tinyWAV 用纯 Go 造一个合法 WAV，测试不依赖任何外部下载。
func tinyWAV(sampleRate, channels int, seconds float64) []byte {
	const bits = 16
	frames := int(float64(sampleRate) * seconds)
	dataSize := frames * channels * bits / 8
	var buf bytes.Buffer
	write := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	write(uint32(36 + dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	write(uint32(16))
	write(uint16(1))
	write(uint16(channels))
	write(uint32(sampleRate))
	write(uint32(sampleRate * channels * bits / 8))
	write(uint16(channels * bits / 8))
	write(uint16(bits))
	buf.WriteString("data")
	write(uint32(dataSize))
	buf.Write(make([]byte, dataSize))
	return buf.Bytes()
}

func resetVoiceCache() {
	ttsVoiceCache.mu.Lock()
	ttsVoiceCache.loaded = false
	ttsVoiceCache.voices = nil
	ttsVoiceCache.mu.Unlock()
}

// ---------- 解析器（不碰真实命令） ----------

func TestParseAfInfo(t *testing.T) {
	const fixture = `File:           t.aiff
File type ID:   AIFC
Num Tracks:     1
----
Data format:     1 ch,  22050 Hz, lpcm (0x0000000E) 16-bit big-endian signed integer
                no channel layout.
estimated duration: 2.125125 sec
audio bytes: 93718
audio packets: 46859
bit rate: 352800 bits per second
source bit depth: I16
----
`
	d := parseAfInfo(fixture)
	if d["container"] != "AIFC" {
		t.Errorf("container = %v", d["container"])
	}
	if d["tracks"] != 1 {
		t.Errorf("tracks = %v", d["tracks"])
	}
	if d["channels"] != 1 {
		t.Errorf("channels = %v", d["channels"])
	}
	if d["sample_rate_hz"] != float64(22050) {
		t.Errorf("sample_rate_hz = %v", d["sample_rate_hz"])
	}
	if d["data_format"] != "lpcm" {
		t.Errorf("data_format = %v", d["data_format"])
	}
	if d["duration_seconds"] != 2.125125 {
		t.Errorf("duration = %v", d["duration_seconds"])
	}
	if d["bit_rate_bps"] != float64(352800) {
		t.Errorf("bit_rate = %v", d["bit_rate_bps"])
	}
	if d["source_bit_depth"] != "I16" {
		t.Errorf("bit depth = %v", d["source_bit_depth"])
	}
}

func TestParseMdlsHandlesNullAndArrays(t *testing.T) {
	const fixture = `kMDItemAudioBitRate      = 33405
kMDItemCodecs            = (
    HEVC,
    Timecode
)
kMDItemDurationSeconds   = (null)
kMDItemFSName            = "t.aiff"
kMDItemKind              = "Apple MPEG-4音频"
`
	md := parseMdls(fixture)
	if md["kMDItemAudioBitRate"] != "33405" {
		t.Errorf("bitrate = %q", md["kMDItemAudioBitRate"])
	}
	if md["kMDItemCodecs"] != "HEVC, Timecode" {
		t.Errorf("codecs = %q", md["kMDItemCodecs"])
	}
	if md["kMDItemKind"] != "Apple MPEG-4音频" {
		t.Errorf("kind = %q", md["kMDItemKind"])
	}
	if _, ok := md["kMDItemDurationSeconds"]; ok {
		t.Error("(null) 不该被当成取值")
	}
}

func TestParseSayVoices(t *testing.T) {
	const fixture = `Albert              en_US    # Hello! My name is Albert.
Bad News            en_US    # Hello! My name is Bad News.
Eddy (德语（德国）)       de_DE    # Hallo! Ich heiße Eddy.
Meijia              zh_TW    # 你好，我叫美佳。
`
	list := parseSayVoices(fixture)
	want := map[string]string{
		"Albert": "en_US", "Bad News": "en_US",
		"Eddy (德语（德国）)": "de_DE", "Meijia": "zh_TW",
	}
	if len(list) != len(want) {
		t.Fatalf("解析出 %d 个音色，期望 %d：%+v", len(list), len(want), list)
	}
	for _, v := range list {
		if want[v.name] != v.locale {
			t.Errorf("音色 %q locale = %q，期望 %q", v.name, v.locale, want[v.name])
		}
	}
	for _, v := range list {
		if v.name == "Meijia" && !strings.Contains(v.label, "zh_TW") {
			t.Errorf("label 应带区域码：%q", v.label)
		}
	}
}

// ---------- media.probe ----------

func TestMediaProbeRealWAV(t *testing.T) {
	if _, ok := execx.LookPath("afinfo"); !ok {
		t.Skip("本机没有 afinfo，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "tone.wav")
	if err := os.WriteFile(src, tinyWAV(22050, 1, 1.0), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := b.run("media.probe", map[string]any{"input": src})
	d := res.Data.(map[string]any)
	if d["container"] != "WAVE" {
		t.Errorf("container = %v，期望 WAVE", d["container"])
	}
	if d["sample_rate_hz"] != float64(22050) {
		t.Errorf("sample_rate_hz = %v，期望 22050", d["sample_rate_hz"])
	}
	if d["channels"] != 1 {
		t.Errorf("channels = %v，期望 1", d["channels"])
	}
	if dur, ok := d["duration_seconds"].(float64); !ok || dur < 0.9 || dur > 1.1 {
		t.Errorf("duration = %v，期望约 1 秒", d["duration_seconds"])
	}
}

func TestMediaProbeNonMediaReportsReason(t *testing.T) {
	b := newBench(t)
	src := filepath.Join(b.home, "not-media.txt")
	if err := os.WriteFile(src, []byte("这不是媒体文件\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := b.runErr("media.probe", map[string]any{"input": src})
	if err == nil {
		t.Fatal("非媒体文件应如实报错，不能谎报成功")
	}
	if !strings.Contains(err.Error(), "afinfo") && !strings.Contains(err.Error(), "mdls") {
		t.Errorf("错误信息应点出真实原因，实际：%v", err)
	}
}

// ---------- media.audio_convert ----------

func TestMediaAudioConvertReal(t *testing.T) {
	if _, ok := execx.LookPath("afconvert"); !ok {
		t.Skip("本机没有 afconvert，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "tone.wav")
	if err := os.WriteFile(src, tinyWAV(22050, 1, 0.5), 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("media.audio_convert", map[string]any{
		"input": src, "format": "caf", "sample_rate": "44100",
		"output": filepath.Join(b.write, "tone.caf"),
	})
	data := b.resultData(t, snap)
	out := data["output"].(string)
	st, err := os.Stat(out)
	if err != nil || st.Size() == 0 {
		t.Fatalf("产物不存在或是空文件：%v", err)
	}
	// 独立复核采样率真的变了，而不是只信工具自报。
	info := execx.New().Run(t.Context(), 15*time.Second, "afinfo", out)
	if !strings.Contains(info.Stdout, "44100 Hz") {
		t.Fatalf("afinfo 未报告 44100 Hz：%s", info.Stdout)
	}
}

func TestMediaAudioConvertShimFailure(t *testing.T) {
	dir := shimDir(t, map[string]string{"afconvert": `#!/bin/sh
if [ "$1" = "-hf" ]; then
  echo "'WAVE'" >&2; echo "'AIFF'" >&2; echo "'m4af'" >&2; echo "'caff'" >&2; exit 2
fi
echo "假 afconvert：转码失败" >&2
exit 1
`})
	t.Setenv("PATH", dir)
	b := newBench(t)
	src := filepath.Join(b.home, "tone.wav")
	if err := os.WriteFile(src, tinyWAV(22050, 1, 0.2), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := metaOf(t, b.reg, "media.audio_convert"); !m.Available {
		t.Fatalf("有 shim 时元数据应可用：%s", m.UnavailableReason)
	}
	_, snap := b.run("media.audio_convert", map[string]any{"input": src, "format": "m4a"})
	if snap.Status != tasks.Failed {
		t.Fatalf("应失败，实际 %s（%v）", snap.Status, snap.Result)
	}
	if !strings.Contains(snap.Error, "假 afconvert") {
		t.Errorf("失败原因应带真实 stderr，实际：%s", snap.Error)
	}
}

func TestMediaAudioConvertUnavailableWithoutCommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	b := newBench(t)
	m := metaOf(t, b.reg, "media.audio_convert")
	if m.Available {
		t.Fatal("PATH 没有 afconvert 时不该报告可用")
	}
	if !strings.Contains(m.UnavailableReason, "afconvert") {
		t.Errorf("不可用原因应点出缺哪个命令，实际：%q", m.UnavailableReason)
	}
}

func TestMediaAudioConvertRefusesToOverwriteSource(t *testing.T) {
	if _, ok := execx.LookPath("afconvert"); !ok {
		t.Skip("本机没有 afconvert，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.write, "keep.wav")
	original := tinyWAV(22050, 1, 0.2)
	if err := os.WriteFile(src, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("media.audio_convert", map[string]any{
		"input": src, "format": "m4a", "output": src,
	})
	if snap.Status != tasks.Failed {
		t.Fatalf("输出等于输入时应失败，实际 %s", snap.Status)
	}
	after, err := os.ReadFile(src)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("源文件被改动了")
	}
}

// ---------- media.tts ----------

func TestTTSProbeFailureIsReported(t *testing.T) {
	dir := shimDir(t, map[string]string{"say": "#!/bin/sh\necho \"假 say：列音色失败\" >&2\nexit 1\n"})
	t.Setenv("PATH", dir)
	b := newBench(t)
	code, err := b.runErr("media.tts", map[string]any{"text": "你好"})
	if err == nil || code != 503 {
		t.Fatalf("探测失败应 503，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "假 say") {
		t.Errorf("应带真实 stderr，实际：%v", err)
	}
}

func TestTTSRunFailureAfterProbe(t *testing.T) {
	dir := shimDir(t, map[string]string{"say": `#!/bin/sh
if [ "$1" = "-v" ] && [ "$2" = "?" ]; then
  printf 'Fake Voice   zh_CN    # 你好\n'
  exit 0
fi
echo "假 say：合成失败" >&2
exit 1
`})
	t.Setenv("PATH", dir)
	b := newBench(t)
	_, snap := b.run("media.tts", map[string]any{"text": "你好，军刀"})
	if snap.Status != tasks.Failed {
		t.Fatalf("应失败，实际 %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "假 say：合成失败") {
		t.Errorf("失败原因应带真实 stderr，实际：%s", snap.Error)
	}
}

func TestTTSRealSynthesisAndVoiceCache(t *testing.T) {
	if _, ok := execx.LookPath("say"); !ok {
		t.Skip("本机没有 say，跳过")
	}
	resetVoiceCache()
	b := newBench(t)
	if got := len(voiceOptions()); got != 1 {
		t.Fatalf("探测前只该有 auto 选项，实际 %d", got)
	}
	_, snap := b.run("media.tts", map[string]any{
		"text": "你好，这是军刀语音测试。", "format": "aiff",
		"output": filepath.Join(b.write, "tts.aiff"),
	})
	data := b.resultData(t, snap)
	out := data["output"].(string)
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatalf("没有产出音频：%v", err)
	}
	// 运行时列出并缓存真实音色。
	if n := len(voiceOptions()); n < 5 {
		t.Fatalf("say -v '?' 后应缓存真实音色，实际 %d 个", n)
	}
	voice, note := pickAutoVoice()
	if voice == "" {
		t.Fatalf("本机应有中文音色：%s", note)
	}
	// 缓存后可以用具体音色再合成一次。
	_, snap2 := b.run("media.tts", map[string]any{
		"text": "第二次，指定音色。", "voice": voice, "format": "aiff",
		"output": filepath.Join(b.write, "tts-voice.aiff"),
	})
	d2 := b.resultData(t, snap2)
	if d2["voice"] != voice {
		t.Errorf("实际音色 %v，期望 %s", d2["voice"], voice)
	}
}

func TestTTSRealM4A(t *testing.T) {
	if _, ok := execx.LookPath("say"); !ok {
		t.Skip("本机没有 say，跳过")
	}
	if _, ok := execx.LookPath("afconvert"); !ok {
		t.Skip("本机没有 afconvert，跳过")
	}
	b := newBench(t)
	_, snap := b.run("media.tts", map[string]any{
		"text": "导出 m4a。", "format": "m4a",
		"output": filepath.Join(b.write, "tts.m4a"),
	})
	data := b.resultData(t, snap)
	out := data["output"].(string)
	if !strings.HasSuffix(out, ".m4a") {
		t.Fatalf("输出后缀应为 .m4a：%s", out)
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatalf("m4a 产物不存在：%v", err)
	}
}

// ---------- media.extract_audio（如实不可用） ----------

func TestExtractAudioIsHonestlyUnavailable(t *testing.T) {
	b := newBench(t)
	m := metaOf(t, b.reg, "media.extract_audio")
	if m.Available {
		t.Fatal("系统 CLI 做不到，不该报告可用")
	}
	if !strings.Contains(m.UnavailableReason, "ffmpeg") {
		t.Errorf("原因应点出需要 ffmpeg，实际：%q", m.UnavailableReason)
	}
	code, err := b.runErr("media.extract_audio", map[string]any{})
	if err == nil || code != 503 {
		t.Fatalf("不可用工具应 503，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("拒绝原因应可读，实际：%v", err)
	}
}

// ---------- 元数据纪律 ----------

func TestWave2MetaDiscipline(t *testing.T) {
	want := map[string]struct {
		category string
		async    bool
		bin      string
	}{
		"media.probe":         {"media", false, "afinfo"},
		"media.audio_convert": {"media", true, "afconvert"},
		"media.tts":           {"media", true, "say"},
		"media.extract_audio": {"media", true, ""},
		"text.doc_convert":    {"text", true, "textutil"},
		"text.encoding_fix":   {"text", true, "iconv"},
		"text.plist_json":     {"text", false, "plutil"},
		"text.hash_file":      {"text", false, "shasum"},
		"text.compare":        {"text", false, "diff"},
	}
	reg := tool.NewRegistry()
	RegisterAll(reg)
	for id, w := range want {
		m := metaOf(t, reg, id)
		if m.Category != w.category {
			t.Errorf("%s: category=%q，期望 %q", id, m.Category, w.category)
		}
		if m.Async != w.async {
			t.Errorf("%s: async=%v，期望 %v", id, m.Async, w.async)
		}
		if n := len([]rune(m.Summary)); n > 40 {
			t.Errorf("%s: summary %d 字，超过 40：%q", id, n, m.Summary)
		}
		if m.Danger {
			t.Errorf("%s: 本轮工具都不该是危险工具", id)
		}
		if w.bin != "" {
			_, found := execx.LookPath(w.bin)
			if found != m.Available {
				t.Errorf("%s: available=%v，但 %s 存在=%v", id, m.Available, w.bin, found)
			}
		}
		if !m.Available && len([]rune(m.UnavailableReason)) < 6 {
			t.Errorf("%s: 不可用原因太短/不可读：%q", id, m.UnavailableReason)
		}
	}
	// 新工具 id 必须与骨架的 text.hash 区分开。
	if _, ok := reg.Get("text.hash_file"); !ok {
		t.Fatal("text.hash_file 未注册")
	}
	if _, ok := reg.Get("text.hash"); !ok {
		t.Fatal("骨架 text.hash 应保持存在")
	}
}

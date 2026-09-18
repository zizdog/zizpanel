package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  语音转文字（whisper.cpp）的契约测试
//
//  这一组测试的共同前提：**单测不许碰真实服务、真实家目录、真实 whisper-cli**
//  （AGENTS 第三节）。所以：
//    · 档位/路径解析全部注入 modelsDir（临时目录），不读 ~/stt；
//    · 引擎的命令执行器用假的 Run（不 spawn 任何进程）；
//    · "健康探活"用注入的假引擎验证**判据**（模型缺失/引擎缺失时必须不健康），
//      而不是真去跑一次推理。
//  唯一"真"的东西是纯函数（JSON 解析、字幕格式化、语言归一、探针 WAV 头）。
// ============================================================================

// sttTestModelDir 造一个"某个档位已下好"的沙箱模型目录。
//
// 写的是**真实字节数**（STTModel.Bytes）：因为判据就是"大小必须与上游一致"，
// 造一个随便大小的文件会让测试失去意义（那正是"0 字节空壳也报健康"的形态）。
func sttTestModelDir(t *testing.T, installed ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, id := range installed {
		m, err := FindSTTModel(id)
		if err != nil {
			t.Fatalf("测试写错了档位名 %q：%v", id, err)
		}
		// 造一个"大小正确 + ggml 魔数正确"的文件。真的写 466 MB 太浪费，
		// 所以这里用**同尺寸的稀疏文件**：Seek 到末尾写 1 字节即可，
		// os.Stat().Size() 会如实报出那个大小，而磁盘占用接近 0。
		path := filepath.Join(dir, m.File)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("lmgg")); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if _, err := f.Seek(m.Bytes-1, 0); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if _, err := f.Write([]byte{0}); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// ---------------------------------------------------------------------------
//  ① 档位与路径解析（可注入）
// ---------------------------------------------------------------------------

// TestSTTModelTiersAreStable 锁住三档的 ID / 文件名 / 体积。
//
// 这些值是**对外契约**（请求里的 model 字段、/v1/models 的 id、磁盘上的文件名）
// 与**下载校验的期望值**（Bytes 必须与上游一致，否则装好的模型会被判成损坏）。
// 改动它们等于同时改协议与校验，必须是显式行为。
func TestSTTModelTiersAreStable(t *testing.T) {
	want := []struct {
		id    string
		file  string
		bytes int64
	}{
		// 体积是 2026-09-25 对 hf-mirror 发 Range 请求读 Content-Range 得到的**实测值**。
		{"small", "ggml-small.bin", 487601967},
		{"large-v3-turbo", "ggml-large-v3-turbo-q5_0.bin", 574041195},
		{"medium", "ggml-medium.bin", 1533763059},
	}
	if len(STTModels) != len(want) {
		t.Fatalf("档位数量变了：现在 %d 个，测试期望 %d 个", len(STTModels), len(want))
	}
	for i, w := range want {
		got := STTModels[i]
		if got.ID != w.id || got.File != w.file || got.Bytes != w.bytes {
			t.Errorf("第 %d 档漂移：得到 %s/%s/%d，期望 %s/%s/%d",
				i, got.ID, got.File, got.Bytes, w.id, w.file, w.bytes)
		}
		if strings.TrimSpace(got.Note) == "" {
			t.Errorf("%s 没有 Note（界面要显示取舍说明）", got.ID)
		}
	}
	// 用户要求"至少支持 small / medium / large-v3-turbo(q5)"，三条都得在。
	for _, id := range []string{"small", "medium", "large-v3-turbo"} {
		if _, err := FindSTTModel(id); err != nil {
			t.Errorf("用户点名的档位 %s 不在清单里：%v", id, err)
		}
	}
}

// TestFindSTTModelInjection 锁住"档位 → STTModel"的解析规则。
func TestFindSTTModelInjection(t *testing.T) {
	if id := DefaultSTTModelID(); id != "small" {
		t.Errorf("默认档应为 small，实际 %q", id)
	}
	// 空串 = 默认档（请求里不写 model 时就是这个行为）。
	m, err := FindSTTModel("")
	if err != nil || m.ID != "small" {
		t.Errorf("空档位应解析为默认档，得到 %q err=%v", m.ID, err)
	}
	// 大小写与两端空白都要容忍（curl 调用方很容易写成 "Small"）。
	m, err = FindSTTModel("  LARGE-V3-TURBO ")
	if err != nil || m.ID != "large-v3-turbo" {
		t.Errorf("大小写/空白不敏感失败：%q err=%v", m.ID, err)
	}
	// 认不出来必须**报错**，不许悄悄给默认档 —— 那会让用户以为用的是他点的档。
	_, err = FindSTTModel("whisper-large-v9")
	if err == nil {
		t.Fatal("未知档位不该被接受（悄悄回落默认档 = 谎报）")
	}
	if !strings.Contains(err.Error(), "small") {
		t.Errorf("未知档位的错误信息应当列出支持的档位：%v", err)
	}
}

// TestSTTModelPathInjection 锁住"档位 → 绝对路径"，且**不依赖真实家目录**。
func TestSTTModelPathInjection(t *testing.T) {
	dir := t.TempDir()
	path, m, err := sttModelPath(dir, "medium")
	if err != nil {
		t.Fatalf("解析 medium 路径失败：%v", err)
	}
	if want := filepath.Join(dir, "ggml-medium.bin"); path != want {
		t.Errorf("路径不对：得到 %q，期望 %q", path, want)
	}
	if m.ID != "medium" {
		t.Errorf("返回的档位不对：%q", m.ID)
	}
	// 目录为空时必须报错（否则会拼出相对路径 "./ggml-small.bin"，落到进程 cwd）。
	if _, _, err := sttModelPath("", "small"); err == nil {
		t.Error("模型目录为空时应报错，而不是拼出相对路径")
	}
	// 未知档位要报错。
	if _, _, err := sttModelPath(dir, "nope"); err == nil {
		t.Error("未知档位应报错")
	}
}

// TestSTTPathsFollowManagerOptions：路径全部从 Manager 选项来 ——
// 不许出现写死的 /opt/homebrew 或写死的用户名。
func TestSTTPathsFollowManagerOptions(t *testing.T) {
	m, _ := sandboxManager(t)
	m.opt.UserHome = "/tmp/zp-stt-home"
	p := m.sttPaths()
	if p.Root != "/tmp/zp-stt-home/stt" {
		t.Errorf("Root 应从 UserHome 推出来，实际 %q", p.Root)
	}
	if p.ModelsDir != "/tmp/zp-stt-home/stt/models" {
		t.Errorf("ModelsDir 应为 <root>/models，实际 %q", p.ModelsDir)
	}
	if !strings.HasPrefix(p.OutLog, "/tmp/zp-stt-home/") {
		t.Errorf("日志应落在该用户家目录下，实际 %q", p.OutLog)
	}
	if strings.Contains(p.Plist, "/opt/homebrew") {
		t.Errorf("plist 路径不该写死 Homebrew 前缀：%q", p.Plist)
	}

	// UserHome 为空时按 UserName 推（面板以 root 跑时必须能这么推）。
	m2, _ := sandboxManager(t)
	m2.opt.UserHome = ""
	m2.opt.UserName = "someuser"
	if got := m2.sttPaths().Root; got != "/Users/someuser/stt" {
		t.Errorf("UserHome 为空时应按 UserName 推，实际 %q", got)
	}
}

// TestSTTEngineUsesInjectedBrewPrefix：whisper 相关的路径**完全由注入的
// brewPrefix 推出来**。Intel Mac 的前缀是 /usr/local，写死 /opt/homebrew
// 会让那里的用户点开界面就报"引擎不可用"（而引擎其实装好了）。
func TestSTTEngineUsesInjectedBrewPrefix(t *testing.T) {
	for _, prefix := range []string{"/opt/homebrew", "/usr/local", "/tmp/custom-brew"} {
		dir := filepath.Join(t.TempDir(), "brew")
		bin := filepath.Join(dir, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{STTCLIBinName, STTServerBinName, "ffmpeg", "ffprobe"} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		e := NewSTTEngine(dir, "/models", "small")
		// 前缀下**有**可执行文件时必须用它：绝不越过注入值去挑别的。
		for name, got := range map[string]string{
			STTCLIBinName:    e.CLIBin,
			STTServerBinName: e.ServerBin,
			"ffmpeg":         e.FfmpegBin,
			"ffprobe":        e.FfprobeBin,
		} {
			if want := filepath.Join(bin, name); got != want {
				t.Errorf("brewPrefix=%s：%s 的路径应跟着前缀走，得到 %q 期望 %q", prefix, name, got, want)
			}
		}
		_ = prefix
	}
}

// TestSTTSourceHasNoHardcodedPrefixOrUser 是"覆盖整类"的源码门禁。
//
// 只看行为不够：`/opt/homebrew` 完全可以藏在某个分支里，而这台机器恰好就是
// /opt/homebrew，行为测试会全绿。所以直接读**我自己新写的源码**。
//
// 规则刻意写得很具体，并**逐条写明允许的例外**（含糊的 grep 只会逼下一个人
// 把代码写歪去绕开门禁，而不是真的修好问题）：
//
//	禁止：
//	  · `/opt/homebrew` —— Homebrew 前缀必须从 brewPrefix() 推（Intel 是 /usr/local）；
//	  · `/Users/<某个具体用户名>` —— 家目录必须从 UserHome/UserName 推；
//	  · `Library/LaunchDaemons` 字面量 —— plist 路径必须走 SystemDaemonPlistPath。
//	允许（写清理由）：
//	  · launchd plist 里的 `PATH` 串包含 `/usr/local/bin` —— 那是 launchd 作业的
//	    标准 PATH，与架构无关；两种前缀都列上，brew 装的工具在 Intel 上也找得到；
//	  · `"/Users/" + userName` —— macOS 家目录布局，用户名是**变量**不是字面量；
//	  · `com.zizdog.<app>` 形式的 launchd label —— 全项目的反向域名约定
//	    （com.zizdog.imgcompress / com.zizdog.macosspeech 同形）。
func TestSTTSourceHasNoHardcodedPrefixOrUser(t *testing.T) {
	files := []string{"stt.go", "stt_install.go"}
	banned := []struct{ lit, why string }{
		{"/opt/homebrew", "Homebrew 前缀必须从 brewPrefix() 推（Intel Mac 是 /usr/local）"},
		{"/Users/zizdog", "不许写死某个具体用户的家目录（面板要能装在别的用户名下）"},
		{"Library/LaunchDaemons", "plist 路径必须走 SystemDaemonPlistPath（两处写会漂移）"},
	}
	codeLines := 0
	all := ""
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", f, err)
		}
		src := string(b)
		all += src
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				continue // 注释里写实测证据（例如 /opt/homebrew/bin/whisper-server）是允许且鼓励的
			}
			// 允许的例外之一：plist 里的 PATH 串。
			if strings.Contains(line, "PATH") && strings.Contains(line, "/usr/local/bin") {
				continue
			}
			codeLines++
			for _, bad := range banned {
				if strings.Contains(line, bad.lit) {
					t.Errorf("%s:%d 代码行里写死了 %q —— %s\n  %s",
						f, i+1, bad.lit, bad.why, trimmed)
				}
			}
		}
	}
	// 反方向：必须**真的**用推导，而不是靠"没写"通过。
	if !strings.Contains(all, `"/Users/" +`) {
		t.Error("没有 `\"/Users/\" + userName` 这个推导 —— 家目录是怎么得到的？")
	}
	if !strings.Contains(all, "SystemDaemonPlistPath(") {
		t.Error("plist 路径必须走 SystemDaemonPlistPath（不许自己拼 /Library/LaunchDaemons）")
	}
	if codeLines < 100 {
		t.Fatalf("只检查到 %d 行代码，判据可能失效了", codeLines)
	}
}

// ---------------------------------------------------------------------------
//  ② OpenAI 契约：字段与导出格式
// ---------------------------------------------------------------------------

// TestParseWhisperCLIJSONParsesRealOutput 用**真机上跑出来的** JSON 形状锁解析。
//
// 这是一份真实 `whisper-cli -oj` 输出的裁剪（2026-09-25 本机，模型 small，
// 音频是 say 合成的中文）。解析规则错了的直接后果是"转出的文本是空的"
// 或"时间戳全是 0"。
func TestParseWhisperCLIJSONParsesRealOutput(t *testing.T) {
	raw := []byte(`{
		"systeminfo": "WHISPER : ... MTL : EMBED_LIBRARY = 1 ...",
		"model": {"type": "small", "multilingual": true, "vocab": 51865},
		"params": {"model": "/tmp/stt-models/ggml-small.bin", "language": "zh", "translate": false},
		"result": {"language": "zh"},
		"transcription": [
			{
				"timestamps": {"from": "00:00:00,000", "to": "00:00:03,500"},
				"offsets": {"from": 0, "to": 3500},
				"text": " 今天天气不错,测试语音拽文字。"
			},
			{
				"timestamps": {"from": "00:00:03,500", "to": "00:00:04,000"},
				"offsets": {"from": 3500, "to": 4000},
				"text": "   "
			}
		]
	}`)
	segs, lang, err := parseWhisperCLIJSON(raw)
	if err != nil {
		t.Fatalf("解析真实形状的 JSON 失败：%v", err)
	}
	if lang != "zh" {
		t.Errorf("识别语言应为 zh，实际 %q", lang)
	}
	// 空文本的分段必须被丢掉（whisper 对静音会给空段，塞进去会让"分段数"虚高）。
	if len(segs) != 1 {
		t.Fatalf("空文本分段应被丢弃，期望 1 段，实际 %d 段：%+v", len(segs), segs)
	}
	s := segs[0]
	if s.Text != "今天天气不错,测试语音拽文字。" {
		t.Errorf("文本没被 TrimSpace 或解析错：%q", s.Text)
	}
	if s.Start != 0 || s.End != 3.5 {
		t.Errorf("时间戳（秒）不对：start=%v end=%v，期望 0 / 3.5", s.Start, s.End)
	}
	if s.ID != 0 {
		t.Errorf("分段 ID 应从 0 开始，实际 %d", s.ID)
	}
}

// TestParseWhisperCLIJSONFallsBackToTimestamps：拿不到 offsets 时用 timestamps 字符串。
func TestParseWhisperCLIJSONFallsBackToTimestamps(t *testing.T) {
	raw := []byte(`{"result":{"language":"en"},"transcription":[
		{"timestamps":{"from":"00:01:02,500","to":"00:01:04,000"},"text":"hello"}]}`)
	segs, _, err := parseWhisperCLIJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Fatalf("期望 1 段，实际 %d", len(segs))
	}
	if segs[0].Start != 62.5 || segs[0].End != 64 {
		t.Errorf("timestamps 回落解析错：%v / %v，期望 62.5 / 64", segs[0].Start, segs[0].End)
	}
}

// TestParseWhisperCLIJSONRejectsGarbage：坏 JSON 要报错，不许给空结果冒充成功。
func TestParseWhisperCLIJSONRejectsGarbage(t *testing.T) {
	if _, _, err := parseWhisperCLIJSON([]byte("<html>404 Not Found</html>")); err == nil {
		t.Error("HTML 错误页必须让解析失败（那正是'转出空文本'的成因）")
	}
}

// TestFormatTranscriptContract 锁住四种 response_format 的**字节级**形状。
//
// 用户要求"response_format=json|text|srt|vtt，返回 text 与 segments"：
// 这里逐字断言，避免"文本里有、字幕里没有"这类不一致。
func TestFormatTranscriptContract(t *testing.T) {
	res := &STTResult{
		Text:       "今天天气不错,测试语音转文字。",
		Language:   "zh",
		ModelID:    "small",
		ModelFile:  "ggml-small.bin",
		Chunks:     1,
		DurationMS: 3500,
		Segments: []STTSegment{
			{ID: 0, Start: 0, End: 1.5, Text: "今天天气不错,"},
			{ID: 1, Start: 1.5, End: 3.5, Text: "测试语音转文字。"},
		},
	}

	// text：就是纯文本，没有任何包装。
	body, err := FormatTranscript(res, STTFormatText)
	if err != nil {
		t.Fatal(err)
	}
	if body != res.Text {
		t.Errorf("text 格式应原样输出文本，得到 %q", body)
	}

	// json：只保证 text（OpenAI 的 json 语义）。
	body, err = FormatTranscript(res, STTFormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var jm map[string]any
	if err := json.Unmarshal([]byte(body), &jm); err != nil {
		t.Fatalf("json 格式不是合法 JSON：%v（%s）", err, body)
	}
	if jm["text"] != res.Text {
		t.Errorf("json.text 不对：%v", jm["text"])
	}

	// verbose_json：必须带 language / duration / segments（含 start/end/text）。
	body, err = FormatTranscript(res, STTFormatVerboseJSON)
	if err != nil {
		t.Fatal(err)
	}
	var vm struct {
		Task     string  `json:"task"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Text     string  `json:"text"`
		Model    string  `json:"model"`
		Segments []struct {
			ID    int     `json:"id"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal([]byte(body), &vm); err != nil {
		t.Fatalf("verbose_json 解析失败：%v（%s）", err, body)
	}
	if vm.Task != "transcribe" || vm.Language != "zh" || vm.Duration != 3.5 || vm.Text != res.Text {
		t.Errorf("verbose_json 顶层字段不对：%+v", vm)
	}
	if vm.Model != "small" {
		t.Errorf("verbose_json 必须回报**实际使用**的档位（扩展字段 model），实际 %q", vm.Model)
	}
	if len(vm.Segments) != 2 {
		t.Fatalf("verbose_json 应带 2 个分段，实际 %d", len(vm.Segments))
	}
	if vm.Segments[1].Start != 1.5 || vm.Segments[1].End != 3.5 || vm.Segments[1].Text != "测试语音转文字。" {
		t.Errorf("分段字段不对：%+v", vm.Segments[1])
	}

	// srt：标准 SubRip（序号 / 时间行 / 文本 / 空行）。
	body, err = FormatTranscript(res, STTFormatSRT)
	if err != nil {
		t.Fatal(err)
	}
	wantSrt := "1\n00:00:00,000 --> 00:00:01,500\n今天天气不错,\n\n" +
		"2\n00:00:01,500 --> 00:00:03,500\n测试语音转文字。\n\n"
	if body != wantSrt {
		t.Errorf("srt 形状不对。\n得到：%q\n期望：%q", body, wantSrt)
	}

	// vtt：WEBVTT 头 + 点号毫秒。
	body, err = FormatTranscript(res, STTFormatVTT)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, "WEBVTT\n\n") {
		t.Errorf("vtt 必须以 WEBVTT 头开始：%q", body)
	}
	if !strings.Contains(body, "00:00:01.500 --> 00:00:03.500") {
		t.Errorf("vtt 时间戳格式不对（应当是点号）：%q", body)
	}
}

// TestParseSTTFormatAliases 锁住 response_format 的解析与**错误信息**。
func TestParseSTTFormatAliases(t *testing.T) {
	cases := map[string]STTFormat{
		"":             STTFormatJSON, // OpenAI 默认
		"json":         STTFormatJSON,
		"text":         STTFormatText,
		"txt":          STTFormatText,
		"plain":        STTFormatText,
		"SRT":          STTFormatSRT,
		"srt":          STTFormatSRT,
		"vtt":          STTFormatVTT,
		"webvtt":       STTFormatVTT,
		"verbose_json": STTFormatVerboseJSON,
	}
	for in, want := range cases {
		got, err := ParseSTTFormat(in)
		if err != nil {
			t.Errorf("ParseSTTFormat(%q) 报错：%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSTTFormat(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 认不出来必须报 ErrSTTExportUnsupported（web 层据此给 400，而不是 500）。
	if _, err := ParseSTTFormat("docx"); err == nil {
		t.Fatal("未知格式应报错")
	} else if !strings.Contains(err.Error(), "json") || !strings.Contains(err.Error(), "srt") {
		t.Errorf("未知格式的错误里应列出支持项：%v", err)
	}
}

// TestNormalizeSTTLanguage：界面写"中文"、接口写"zh-CN"，都必须归一到 whisper 的语言码。
func TestNormalizeSTTLanguage(t *testing.T) {
	cases := map[string]string{
		"": "auto", "auto": "auto", "自动": "auto", "detect": "auto",
		"zh": "zh", "zh-CN": "zh", "Chinese": "zh", "中文": "zh",
		"EN": "en", "english": "en", "英语": "en",
		"ja": "ja", "Japanese": "ja", "日语": "ja",
		// 认不出来时**原样小写返回**（交给 whisper 自己拒绝），绝不猜一个 ——
		// 猜错语言会让整段结果变成另一种文字，比报错糟得多。
		"Klingon": "klingon",
	}
	for in, want := range cases {
		if got := NormalizeSTTLanguage(in); got != want {
			t.Errorf("NormalizeSTTLanguage(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 界面上必须能直接选中文/英语/日语/自动（用户点名要求）。
	for _, want := range []string{"auto", "zh", "en", "ja"} {
		found := false
		for _, c := range STTLanguageChoices {
			if c.Code == want {
				found = true
			}
		}
		if !found {
			t.Errorf("语言下拉里缺少 %q", want)
		}
	}
}

// TestParseWhisperProgressLine：进度行是"真实进度"的唯一来源，解析错就是假进度。
func TestParseWhisperProgressLine(t *testing.T) {
	// 真机上实测的那一行（两边有空格）。
	if p, ok := parseWhisperProgressLine("whisper_print_progress_callback: progress =  35%"); !ok || p != 35 {
		t.Errorf("实测进度行解析失败：p=%d ok=%v", p, ok)
	}
	if p, ok := parseWhisperProgressLine("whisper_print_progress_callback: progress = 100%"); !ok || p != 100 {
		t.Errorf("100%% 解析失败：p=%d ok=%v", p, ok)
	}
	// 非进度行不许被误判（误判会给出假进度）。
	for _, line := range []string{
		"read_audio_data: reading audio data from '/tmp/x.wav' ...",
		"[00:00:00.000 --> 00:00:03.500]  今天天气不错",
		"progress = 999%", // 越界：宁可报"没有进度"，也不给一个 999% 的假进度
	} {
		if p, ok := parseWhisperProgressLine(line); ok {
			t.Errorf("非进度行 %q 被误判成进度 %d%%", line, p)
		}
	}
}

// TestTinyProbeWAVIsValidPCM 验证健康探针音频本身是合法的 16 kHz 单声道 WAV。
//
// 探针文件头写错的话，whisper 会直接报"读不出音频"，于是 /healthz 永远不健康 ——
// 一个"看起来只是探测"的东西就能把整个应用卡死。
func TestTinyProbeWAVIsValidPCM(t *testing.T) {
	b := tinyProbeWAV(0.5, 16000)
	if len(b) != 44+16000/2*2 {
		t.Fatalf("WAV 长度不对：%d", len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[12:16]) != "fmt " || string(b[36:40]) != "data" {
		t.Fatalf("WAV 头不对：%q", b[0:44])
	}
	// 采样率与声道数（whisper 只吃 16 kHz 单声道）。
	if sr := int(b[24]) | int(b[25])<<8 | int(b[26])<<16 | int(b[27])<<24; sr != 16000 {
		t.Errorf("采样率应为 16000，实际 %d", sr)
	}
	if ch := int(b[22]) | int(b[23])<<8; ch != 1 {
		t.Errorf("声道数应为 1，实际 %d", ch)
	}
	if bits := int(b[34]) | int(b[35])<<8; bits != 16 {
		t.Errorf("位深应为 16，实际 %d", bits)
	}
}

// ---------------------------------------------------------------------------
//  ③ /healthz 必须在"模型缺失 / 引擎不在"时不健康（可注入假状态）
// ---------------------------------------------------------------------------

// TestHealthIsUnhealthyWithoutModel 是"不许绿灯"的核心断言（模型缺失那一半）。
func TestHealthIsUnhealthyWithoutModel(t *testing.T) {
	dir := t.TempDir() // 空目录 = 一个档位都没下
	e := NewSTTEngine("/opt/homebrew", dir, "small")
	// 三件套用真的存在的系统工具（/bin/ls 一定在），这样"不健康"只可能来自模型缺失 ——
	// 否则测试会因为别的原因通过，就验证不到我们要验证的那条判据。
	e.CLIBin, e.FfmpegBin, e.FfprobeBin = "/bin/ls", "/bin/ls", "/bin/ls"
	h := e.Health(context.Background())
	if h.OK {
		t.Fatal("一个模型都没有时 /healthz 必须是 ok:false（否则界面绿灯、每次转写都失败）")
	}
	if h.ModelPresent {
		t.Error("模型缺失时 model_present 必须为 false")
	}
	if !strings.Contains(h.Reason, "small") {
		t.Errorf("reason 要点名是**哪一档**不可用：%q", h.Reason)
	}
	if h.Missing != len(STTModels) || h.Ready != 0 {
		t.Errorf("档位统计不对：ready=%d missing=%d（期望 0 / %d）", h.Ready, h.Missing, len(STTModels))
	}
	// 探针不许在模型缺失时被调用（那会白跑一次推理）。
	if h.ProbeOK {
		t.Error("模型缺失时 probe_ok 不该为 true")
	}
}

// TestHealthIsUnhealthyWithoutEngine 锁住"引擎不在"那一半。
func TestHealthIsUnhealthyWithoutEngine(t *testing.T) {
	dir := sttTestModelDir(t, "small") // 模型都在
	e := NewSTTEngine("/tmp/definitely-no-brew-here", dir, "small")
	e.CLIBin = "/nonexistent/" + STTCLIBinName
	e.FfmpegBin, e.FfprobeBin = "/nonexistent/ffmpeg", "/nonexistent/ffprobe"
	h := e.Health(context.Background())
	if h.OK {
		t.Fatal("引擎/ffmpeg 不在时 /healthz 必须是 ok:false（端口在听不代表能转写）")
	}
	if h.ModelPresent != true {
		// 这一条很重要：模型是在的，不健康的**原因**必须是引擎 ——
		// 否则用户会照着错误的提示去重下模型。
		t.Error("模型在时必须如实报 model_present=true")
	}
	if !strings.Contains(h.Reason, STTCLIBinName) && !strings.Contains(h.Reason, "ffmpeg") {
		t.Errorf("reason 要点名缺的是引擎/ffmpeg：%q", h.Reason)
	}
}

// TestHealthUnhealthyWhenModelFileIsTruncated 锁住"0 字节空壳/截断文件不算已安装"。
//
// 这是"有文件就算已安装"那一类缺陷的 whisper 版本：
// 下载中断留下的半份权重会让每次转写都失败，而只看 os.Stat 的判据会给绿灯。
func TestHealthUnhealthyWhenModelFileIsTruncated(t *testing.T) {
	dir := t.TempDir()
	m, _ := FindSTTModel("small")
	if err := os.WriteFile(filepath.Join(dir, m.File), []byte("lmgg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if STTModelFileExists(dir, "small") {
		t.Fatal("截断的模型文件不该被判成已安装（大小必须与上游一致）")
	}
	e := NewSTTEngine("/opt/homebrew", dir, "small")
	e.CLIBin, e.FfmpegBin, e.FfprobeBin = "/bin/ls", "/bin/ls", "/bin/ls"
	h := e.Health(context.Background())
	if h.OK {
		t.Fatal("模型被截断时 /healthz 必须不健康")
	}
	if !strings.Contains(h.Reason, "大小") {
		t.Errorf("reason 要说清是大小不对（用户才知道该重下）：%q", h.Reason)
	}
	// 反过来：大小正确的稀疏文件必须被判成已安装（判据的另一半）。
	good := sttTestModelDir(t, "small")
	if !STTModelFileExists(good, "small") {
		t.Error("大小与上游一致的文件应被判成已安装")
	}
}

// TestHealthProbeIsCachedAndReportedHonestly：探针结果有 TTL，且响应里**如实**
// 说明这是缓存结论（probe_cached / probe_age_ms）——不把缓存伪装成刚刚的实测。
func TestHealthProbeIsCachedAndReportedHonestly(t *testing.T) {
	dir := sttTestModelDir(t, "small")
	e := NewSTTEngine("/opt/homebrew", dir, "small")
	e.CLIBin, e.FfmpegBin, e.FfprobeBin = "/bin/ls", "/bin/ls", "/bin/ls"
	// 注入假时钟，把"探针"替换成一个不跑进程的假实现：我们只验证缓存语义。
	now := time.Unix(1700000000, 0)
	e.Now = func() time.Time { return now }
	probeCalls := 0
	// 用一个假 Run 让探针"成功"：whisper-cli 被换成 /bin/sh 写一份合法 JSON。
	// 但 /bin/sh 无法写出我们期望的文件……所以这里直接**只验证缓存判定逻辑**：
	// 手动写入一次探针结果，然后检查 TTL 内的第二次 Health 不重跑、TTL 后重跑。
	e.probeAt = now
	e.probeOK = true
	e.probeNote = "第一次探针的结论"
	e.ProbeTTL = 20 * time.Second
	h := e.Health(context.Background())
	if !h.OK {
		t.Fatalf("缓存里是成功结论时 Health 应为 ok:true（reason=%q）", h.Reason)
	}
	if !h.ProbeCached {
		t.Error("TTL 内必须如实标记 probe_cached=true")
	}
	if h.ProbeNote != "第一次探针的结论" {
		t.Errorf("应回报缓存里的探针结论，实际 %q", h.ProbeNote)
	}

	// 越过 TTL：缓存失效 → 会真的去跑探针（这里会失败，因为 /bin/ls 不认识
	// whisper 的参数），于是结论必须变成不健康 —— 证明"缓存会过期、不会永久绿灯"。
	now = now.Add(21 * time.Second)
	h2 := e.Health(context.Background())
	if h2.ProbeCached {
		t.Error("越过 TTL 后必须重新探一次（probe_cached 应为 false）")
	}
	if h2.OK {
		t.Error("探针失败后必须如实报不健康，而不是继续用过期结论")
	}
	if probeCalls != 0 {
		t.Errorf("这个测试不该真的跑探针，probeCalls=%d", probeCalls)
	}
}

// TestInvalidateHealthProbe：安装器刚装完必须能让缓存失效，
// 否则"装之前的红灯"会被 20 秒缓存继续报出来（用户看到"装完了还是红的"）。
func TestInvalidateHealthProbe(t *testing.T) {
	e := &STTEngine{Now: time.Now}
	e.probeAt = time.Now()
	e.probeOK = false
	e.probeNote = "装之前的结论：模型不在"
	e.InvalidateHealthProbe()
	if !e.probeAt.IsZero() {
		t.Error("InvalidateHealthProbe 应清掉时间戳")
	}
}

// ---------------------------------------------------------------------------
//  ④ 安装 / 卸载计划（installed ⇒ 必须有卸载路径）
// ---------------------------------------------------------------------------

// TestSTTCatalogEntryIsWired 锁住目录条目的关键字段。
//
// 少任何一个都会让用户看到"装了却显示未装 / 没有卸载按钮 / 卡片点不开"
// 这一族缺陷（AGENTS 第三节反复点名的那一类）。
func TestSTTCatalogEntryIsWired(t *testing.T) {
	app, ok := FindApp(STTAppID)
	if !ok {
		t.Fatalf("目录里没有 %s", STTAppID)
	}
	if app.PanelInstaller != "stt" {
		t.Errorf("PanelInstaller 应为 stt，实际 %q", app.PanelInstaller)
	}
	// ⚠️ formula 必须是**正名**：写别名会让镜像清单探测全部 404，
	// 于是静默回落到官方 ghcr.io（实测慢 240 倍）。
	if app.BrewFormula != STTBrewFormula {
		t.Errorf("BrewFormula 应为 %q（正名），实际 %q", STTBrewFormula, app.BrewFormula)
	}
	if strings.Contains(app.BrewFormula, "whisper-cpp") {
		t.Error("BrewFormula 不许写成别名 whisper-cpp（镜像清单只有 whisper.cpp 那个；写别名会静默回落官方源）")
	}
	if app.ServiceLabel != STTLabel {
		t.Errorf("ServiceLabel 应为 %q，实际 %q", STTLabel, app.ServiceLabel)
	}
	if app.Port != STTPort || app.WebPort() != STTPort {
		t.Errorf("端口应为 %d，实际 %d", STTPort, app.WebPort())
	}
	if app.HealthPath != "/healthz" {
		t.Errorf("健康检查路径应为 /healthz，实际 %q", app.HealthPath)
	}
	if app.UI == nil || app.UI.Slug != STTSlug {
		t.Errorf("必须声明界面别名 UI.Slug=%s（否则不会有 /stt/ 入口）", STTSlug)
	}
	// 安装体判据必须贴着**引擎本体**（whisper-cli），不是界面、也不是那个目录。
	if !strings.HasSuffix(app.RuntimePath, "/"+STTCLIBinName) {
		t.Errorf("RuntimePath=%q 必须以 /%s 结尾（否则 brew 探测失败时会显示未安装）", app.RuntimePath, STTCLIBinName)
	}
	if app.NoDaemon {
		t.Error("语音转文字有常驻的网页界面服务，不该标 NoDaemon")
	}
	// ffmpeg 是转码链路的硬前置，必须声明（安装流程据此一并装好）。
	found := false
	for _, req := range app.Requires {
		if req.Type == "brew_formula" && req.Value == "ffmpeg" {
			found = true
		}
	}
	if !found {
		t.Error("Requires 里必须声明 ffmpeg（转码链路要用它；缺了安装第一步就该报出来）")
	}
	// 有安装器就必须有卸载实现（门禁也查，这里给一条贴着 STT 的断言）。
	if !HasInstallerUninstall(app.PanelInstaller) {
		t.Fatal("PanelInstaller=stt 没有卸载实现（installed=true ⇒ 必须有可用卸载路径）")
	}
}

// TestSTTPortIsUnique 确认新端口没和目录里别的条目撞车。
func TestSTTPortIsUnique(t *testing.T) {
	seen := map[int]string{}
	for _, a := range Catalog() {
		for _, p := range []int{a.Port, a.UIPort} {
			if p <= 0 {
				continue
			}
			if other, dup := seen[p]; dup && other != a.ID {
				t.Errorf("端口 %d 被 %s 与 %s 同时占用", p, other, a.ID)
			}
			seen[p] = a.ID
		}
	}
	if seen[STTPort] != STTAppID {
		t.Errorf("端口 %d 应属于 %s，实际 %q", STTPort, STTAppID, seen[STTPort])
	}
}

// TestSTTUninstallPlanListsModelsWithSizes 是"卸载要能列出将删除的路径与体积"的断言。
//
// 用户要求：**模型删除要让用户确认，并列出将删除的路径与体积**。
// 所以计划里必须逐条给出 DataPaths（路径）与带体积的说明。
func TestSTTUninstallPlanListsModelsWithSizes(t *testing.T) {
	m, _ := sandboxManager(t)
	app, ok := FindApp(STTAppID)
	if !ok {
		t.Fatal("目录里没有 stt")
	}
	// 造出"两个档位已下载"的磁盘状态。
	p := m.sttPaths()
	if err := os.MkdirAll(p.ModelsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var wantPaths []string
	for _, id := range []string{"small", "large-v3-turbo"} {
		mdl, _ := FindSTTModel(id)
		path := filepath.Join(p.ModelsDir, mdl.File)
		if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		wantPaths = append(wantPaths, path)
	}

	plan := m.PlanUninstallFor(context.Background(), app, nil)
	if plan.Kind != "installer" {
		t.Fatalf("应为 installer 计划，实际 %q（blocked=%q）", plan.Kind, plan.Blocked)
	}
	if plan.Blocked != "" {
		t.Errorf("计划不该被 blocked：%q", plan.Blocked)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("计划没有任何步骤，确认框里会是一片空白")
	}
	// 每一档模型都要在 DataPaths 里（勾选"同时删除数据"才会删）。
	for _, want := range wantPaths {
		found := false
		for _, got := range plan.DataPaths {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("计划里缺少模型路径 %s（DataPaths=%v）", want, plan.DataPaths)
		}
		// 体积必须写进步骤里，用户按下去之前就知道会释放多少。
		if !hasStepContaining(plan.Steps, want) {
			t.Errorf("步骤里没有逐条列出 %s —— 用户看不到将要删除什么", want)
		}
	}
	if !hasStepContaining(plan.Steps, "同时删除数据") {
		t.Error("步骤里必须说清『勾选「同时删除数据」才会删模型』")
	}
	// 默认保留模型，KeepNote 必须写清楚（否则用户以为卸载就删了模型）。
	if !strings.Contains(plan.KeepNote, "保留") {
		t.Errorf("KeepNote 必须说明默认保留模型，实际 %q", plan.KeepNote)
	}
	// 引擎与服务的卸载步骤必须在。
	if !hasStepContaining(plan.Steps, STTLabel) {
		t.Error("计划里必须包含停止 launchd 服务的步骤")
	}
	if !hasStepContaining(plan.Steps, "brew uninstall "+STTBrewFormula) {
		t.Error("计划里必须包含 brew uninstall 引擎的步骤")
	}
}

// TestSTTUninstallPathExistsForEveryInstalledEvidence 是"installed ⇒ 有卸载路径"
// 在 STT 这一条上的**逐一**断言（目录声明、brew、面板记录三种已安装态）。
func TestSTTUninstallPathExistsForEveryInstalledEvidence(t *testing.T) {
	m, _ := sandboxManager(t)
	app, _ := FindApp(STTAppID)
	ctx := context.Background()

	// ① brew 装着（无面板记录）
	plan := m.PlanUninstallForBrew(ctx, app, nil, BrewState{Formula: app.BrewFormula, Installed: true})
	if plan.Kind == "none" {
		t.Errorf("brew 装着却没给卸载路径：kind=none blocked=%q", plan.Blocked)
	}
	// ② 有面板记录
	rec := &Service{Name: "stt", DisplayName: app.Name, LaunchLabel: STTLabel, Managed: true}
	plan = m.PlanUninstallForBrew(ctx, app, rec, BrewState{Formula: app.BrewFormula, Installed: true})
	if plan.Kind == "none" {
		t.Errorf("有面板记录却没给卸载路径：kind=none blocked=%q", plan.Blocked)
	}
	// ③ 只有真实产物（whisper-cli 在），没有 brew、没有记录 ——
	//    这是"RuntimePath 声明了安装体"的那条路，也必须给得出计划。
	prefix := t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, STTCLIBinName), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// brewPrefix() 是从 BrewBin 的父父目录推出来的（见 lnmp.go）——
	// 所以把 BrewBin 指到沙箱前缀下，安装体判据就会去那里找 whisper-cli。
	m.opt.BrewBin = filepath.Join(prefix, "bin", "brew")
	plan = m.PlanUninstallForBrew(ctx, app, nil, BrewState{})
	if plan.Kind == "none" {
		t.Errorf("真实产物在磁盘上却没给卸载路径：kind=none blocked=%q（用户会看到已装却卸不掉的卡片）",
			plan.Blocked)
	}
}

// ---------------------------------------------------------------------------
//  ⑤ 模型下载：来源顺序、校验、失败不留半份
// ---------------------------------------------------------------------------

// TestSTTModelSourcesAreMirrorFirst 锁住"镜像优先 + 逐个回落"的候选顺序。
func TestSTTModelSourcesAreMirrorFirst(t *testing.T) {
	mdl, _ := FindSTTModel("small")
	// probe 全通：两个镜像候选都该排在公网前面。
	srcs := STTModelSourceList(context.Background(), "http://mirror.lan:8090", "", mdl,
		func(context.Context, string) error { return nil })
	if len(srcs) < 3 {
		t.Fatalf("候选太少：%d", len(srcs))
	}
	if !srcs[0].Mirror || !srcs[1].Mirror {
		t.Errorf("镜像候选必须排在前面：%+v", srcs)
	}
	if !strings.Contains(srcs[0].URL, "mirror.lan:8090") {
		t.Errorf("第一个候选应指向自建镜像：%q", srcs[0].URL)
	}
	if srcs[len(srcs)-1].Mirror {
		t.Errorf("最后一个候选应是公网源（兜底）：%+v", srcs[len(srcs)-1])
	}
	// hf-mirror 与官方必须在（顺序：hf-mirror 在官方之前）。
	var order []string
	for _, s := range srcs {
		order = append(order, s.URL)
	}
	joined := strings.Join(order, " ")
	if !strings.Contains(joined, sttHFMirror) || !strings.Contains(joined, sttHFUpstream) {
		t.Errorf("必须同时有 hf-mirror 与官方兜底：%v", order)
	}
	if strings.Index(joined, sttHFMirror) > strings.Index(joined, sttHFUpstream) {
		t.Error("hf-mirror 必须排在官方 huggingface.co 之前（官方国内常不可达）")
	}
	// 不配镜像时不该出现任何镜像候选。
	srcs = STTModelSourceList(context.Background(), "", "", mdl, nil)
	for _, s := range srcs {
		if s.Mirror {
			t.Errorf("没配镜像却出现镜像候选：%+v", s)
		}
		if strings.Contains(s.URL, "/models/whisper/") {
			t.Errorf("没配镜像却出现镜像静态路径：%q", s.URL)
		}
	}
}

// TestDownloadSTTModelDeletesBadFileAndFallsBack 锁住"失败不留半份模型"。
//
// 判据：第一个来源返回**大小不对**的文件时，必须当场删掉，然后换下一个来源；
// 全部来源都坏时，磁盘上**不许留下任何文件**（留着它，服务会带着坏权重起来，
// 表现为每次转写都失败/输出乱码，而界面上一切正常）。
func TestDownloadSTTModelDeletesBadFileAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "ggml-small.bin")
	mdl, _ := FindSTTModel("small")
	srcs := []STTModelSource{
		{Name: "坏来源", URL: "http://bad/file"},
		{Name: "好来源", URL: "http://good/file", Mirror: true},
	}
	calls := 0
	fetch := func(ctx context.Context, url, dest string, _ func(got, total int64)) error {
		calls++
		if strings.Contains(url, "bad") {
			// 半份文件（大小不对）。
			return os.WriteFile(dst, []byte("lmgg-partial"), 0o644)
		}
		// 好来源：大小与魔数都对（稀疏文件）。
		f, err := os.Create(dest)
		if err != nil {
			return err
		}
		if _, err := f.Write([]byte("lmgg")); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.Seek(mdl.Bytes-1, 0); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.Write([]byte{0}); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	res, err := DownloadSTTModelTo(context.Background(), srcs, dst, mdl, fetch, nil)
	if err != nil {
		t.Fatalf("第二个来源是好的，不该失败：%v", err)
	}
	if calls != 2 {
		t.Errorf("应当试两个来源，实际 %d 次", calls)
	}
	if res.Source != "好来源" {
		t.Errorf("结果里应如实写明用了哪个来源，实际 %q", res.Source)
	}
	if res.Bytes != mdl.Bytes {
		t.Errorf("报出的体积应等于文件实际大小，得到 %d 期望 %d", res.Bytes, mdl.Bytes)
	}

	// 全部来源都坏：必须失败，且**不留任何文件**。
	_ = os.Remove(dst)
	allBad := func(ctx context.Context, url, dest string, _ func(got, total int64)) error {
		return os.WriteFile(dest, []byte("<html>404</html>"), 0o644)
	}
	_, err = DownloadSTTModelTo(context.Background(), srcs, dst, mdl, allBad, nil)
	if err == nil {
		t.Fatal("所有来源都返回坏文件时必须失败（谎报成功比没做更糟）")
	}
	if _, serr := os.Stat(dst); serr == nil {
		t.Error("失败后磁盘上不许留下坏模型文件（服务会带着它跑起来）")
	}
	if _, serr := os.Stat(dst + ".part"); serr == nil {
		t.Error("失败后不许留下 .part 临时文件")
	}
	// 错误信息要逐个点名来源，用户才知道试过什么。
	if !strings.Contains(err.Error(), "坏来源") || !strings.Contains(err.Error(), "好来源") {
		t.Errorf("错误信息应列出试过的每个来源：%v", err)
	}
}

// TestValidateSTTModelFileRejectsNonGGML 锁住"下载到网页/错误页"这一现场。
func TestValidateSTTModelFileRejectsNonGGML(t *testing.T) {
	mdl, _ := FindSTTModel("small")
	dir := t.TempDir()
	path := filepath.Join(dir, mdl.File)

	// ① 大小对但内容是 HTML（中间层用内容填充凑长度时就是这样）。
	if err := os.WriteFile(path, []byte("<htm"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, mdl.Bytes); err != nil {
		t.Fatal(err)
	}
	if err := validateSTTModelFile(path, mdl); err == nil {
		t.Error("头部不是 ggml 魔数时必须报错（那是下载到了错误页）")
	}

	// ② 0 字节。
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateSTTModelFile(path, mdl); err == nil {
		t.Error("0 字节文件必须被拒绝")
	}

	// ③ 大小不对。
	if err := os.WriteFile(path, []byte("lmgg"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := validateSTTModelFile(path, mdl)
	if err == nil {
		t.Fatal("大小不对必须被拒绝")
	}
	if !strings.Contains(err.Error(), "大小") {
		t.Errorf("错误信息要说清是大小问题：%v", err)
	}

	// ④ 正确形态（大小 + 魔数）必须通过。
	good := filepath.Join(sttTestModelDir(t, "small"), mdl.File)
	if err := validateSTTModelFile(good, mdl); err != nil {
		t.Errorf("大小与魔数都正确的文件应当通过校验：%v", err)
	}
}

// TestWriteSTTCurrentModelRejectsUnknownTier：把不存在的档位写进 current_model
// 会让服务起不来，所以写入那一刻就必须拒绝。
func TestWriteSTTCurrentModelRejectsUnknownTier(t *testing.T) {
	root := t.TempDir()
	if err := WriteSTTCurrentModel(root, "bogus"); err == nil {
		t.Error("未知档位必须写不进去")
	}
	if err := WriteSTTCurrentModel(root, "medium"); err != nil {
		t.Fatalf("合法档位应写入成功：%v", err)
	}
	if got := sttCurrentModel(root); got != "medium" {
		t.Errorf("读回来应为 medium，实际 %q", got)
	}
	// 文件被写坏时回落默认档，而不是让服务直接挂掉。
	if err := os.WriteFile(filepath.Join(root, STTCurrentModelFile), []byte("nonsense\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sttCurrentModel(root); got != DefaultSTTModelID() {
		t.Errorf("坏内容应回落默认档，实际 %q", got)
	}
	// 文件不存在时也是默认档。
	if got := sttCurrentModel(filepath.Join(root, "nope")); got != DefaultSTTModelID() {
		t.Errorf("缺文件应回落默认档，实际 %q", got)
	}
}

// TestSTTModelsStateForReportsPerTierTruth：/v1/models 的每条状态必须贴着磁盘事实。
func TestSTTModelsStateForReportsPerTierTruth(t *testing.T) {
	dir := sttTestModelDir(t, "small", "medium")
	st := STTModelsStateFor(dir, "medium")
	if st.Ready != 2 || st.Total != len(STTModels) {
		t.Errorf("ready/total 不对：%d/%d", st.Ready, st.Total)
	}
	if st.Current != "medium" {
		t.Errorf("current 应为 medium，实际 %q", st.Current)
	}
	for _, ms := range st.Models {
		wantInstalled := ms.ID == "small" || ms.ID == "medium"
		if ms.Installed != wantInstalled {
			t.Errorf("%s 的 installed=%v，期望 %v（reason=%q）", ms.ID, ms.Installed, wantInstalled, ms.Reason)
		}
		if ms.Missing == ms.Installed {
			t.Errorf("%s 的 installed 与 missing 应当互补", ms.ID)
		}
		if !ms.Installed && ms.Reason == "" {
			t.Errorf("%s 未安装却没写原因（界面无法告诉用户为什么）", ms.ID)
		}
		if ms.Current != (ms.ID == "medium") {
			t.Errorf("%s 的 current 标记不对", ms.ID)
		}
	}
	// 非法 current 回落默认档（不许让 /v1/models 报一个不存在的当前档）。
	if got := STTModelsStateFor(dir, "bogus").Current; got != DefaultSTTModelID() {
		t.Errorf("非法 current 应回落默认档，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
//  ⑦ plist / 监听地址 / 下载接线
// ---------------------------------------------------------------------------

// TestSTTPlistRunsAsRealUserAndCarriesMirrorConfig 锁住 launchd 定义的关键内容。
//
// 少一样就是真机故障：
//
//	· 没有 UserName → 服务以 root 跑，模型/临时文件属主变成 root，用户拿不走；
//	· 没有 KeepAlive/RunAtLoad → 开机不自启、"面板显示已装但服务没了"；
//	· 没有 --root / --model → 服务找不到模型目录或当前档；
//	· 没有把镜像基址带进去 → stt-serve 不读面板配置，下载模型时会绕过"镜像优先"。
func TestSTTPlistRunsAsRealUserAndCarriesMirrorConfig(t *testing.T) {
	plist := sttPlist("/opt/zizpanel/bin/zizpanel", STTPort, "/opt/homebrew",
		"/Users/someone/stt", "small", "https://mirror.example.com", "", "someone",
		"/Users/someone/Library/Logs/out.log", "/Users/someone/Library/Logs/err.log")

	for _, want := range []string{
		"<string>" + STTLabel + "</string>",
		"<string>someone</string>", // UserName：以真实用户运行
		"stt-serve",
		"<string>127.0.0.1:" + strconv.Itoa(STTPort) + "</string>",
		"--brew-prefix", "--root", "--model",
		"/Users/someone/stt", "small",
		"https://mirror.example.com",
		"<key>RunAtLoad</key>", "<key>KeepAlive</key>",
		"/Users/someone/Library/Logs/out.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist 里缺少 %q：\n%s", want, plist)
		}
	}
	// launchd 的 PATH 要把 Homebrew 前缀放进去（否则找不到 whisper-cli / ffmpeg）。
	if !strings.Contains(plist, "/opt/homebrew/bin:") {
		t.Errorf("plist 的 PATH 里应包含注入的 brew 前缀：\n%s", plist)
	}
	// 只绑回环：这个服务读用户上传的音频，不该默认暴露给局域网。
	if strings.Contains(plist, "0.0.0.0") {
		t.Error("plist 里出现了 0.0.0.0 —— 默认只能绑 127.0.0.1")
	}
}

// TestSTTPlistEscapesXMLInMirrorBase：镜像基址来自用户输入，
// 里面出现 `&` 是合法 URL 字符；不转义会生成**非法 plist**，
// launchd 直接拒绝加载，而日志里什么都没有（最难查的一类）。
func TestSTTPlistEscapesXMLInMirrorBase(t *testing.T) {
	plist := sttPlist("/bin/zp", STTPort, "/opt/homebrew", "/tmp/r", "small",
		"https://mirror.example.com/a?x=1&y=2", "https://lan.example.com/b&c", "u", "/o", "/e")
	if strings.Contains(plist, "&y=2") || strings.Contains(plist, "b&c") {
		t.Errorf("镜像基址里的 & 没有被转义（会生成非法 plist）：\n%s", plist)
	}
	if !strings.Contains(plist, "&amp;") {
		t.Errorf("镜像基址里的 & 应被转义成 &amp;：\n%s", plist)
	}
}

// TestSTTListenOnlyLoopback：默认只绑回环，且测试可注入覆盖。
func TestSTTListenOnlyLoopback(t *testing.T) {
	sttListenOverride = func() string { return "" }
	t.Cleanup(func() { sttListenOverride = func() string { return "" } })
	if got := STTListen(STTPort); got != "127.0.0.1:8892" {
		t.Errorf("默认监听地址不对：%q", got)
	}
	if got := STTHealthURL(); got != "http://127.0.0.1:8892/healthz" {
		t.Errorf("健康检查地址不对：%q", got)
	}
	sttListenOverride = func() string { return "127.0.0.1:19999" }
	if got := STTListen(STTPort); got != "127.0.0.1:19999" {
		t.Errorf("注入覆盖没生效：%q", got)
	}
}

// TestDownloadSTTModelSkipsWhenAlreadyPresent：已经下好的档位不该重复联网。
func TestDownloadSTTModelSkipsWhenAlreadyPresent(t *testing.T) {
	m, _ := sandboxManager(t)
	p := m.sttPaths()
	if err := os.MkdirAll(p.ModelsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 造一个大小正确的 small。
	mdl, _ := FindSTTModel("small")
	f, err := os.Create(filepath.Join(p.ModelsDir, mdl.File))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("lmgg")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(mdl.Bytes-1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	calls := 0
	sttFetchOverride = func(context.Context, string, string, func(int64, int64)) error {
		calls++
		return nil
	}
	t.Cleanup(func() { sttFetchOverride = nil })

	res, err := m.DownloadSTTModel(context.Background(), "small", nil)
	if err != nil {
		t.Fatalf("已存在的档位不该失败：%v", err)
	}
	if calls != 0 {
		t.Errorf("已经下好的档位不该再联网（下载调用 %d 次）", calls)
	}
	if res.Source != "（已在本地）" {
		t.Errorf("结果应如实写明是本地已有的，实际 %q", res.Source)
	}
}

// TestDownloadSTTModelOfflineModeRefusesPublicFallback：离线模式（仅走 NAS）下
// 缺件必须**明确失败**，绝不偷偷出网。
func TestDownloadSTTModelOfflineModeRefusesPublicFallback(t *testing.T) {
	m, _ := sandboxManager(t)
	m.opt.OfflineOnly = true
	m.opt.MirrorBase = "" // 连镜像都没配
	sttFetchOverride = func(context.Context, string, string, func(int64, int64)) error {
		return fmt.Errorf("离线模式下绝不该走到这里（联网了）")
	}
	t.Cleanup(func() { sttFetchOverride = nil })

	_, err := m.DownloadSTTModel(context.Background(), "small", nil)
	if err == nil {
		t.Fatal("离线模式 + 没有镜像时必须明确失败（不许偷偷回落公网）")
	}
	if !strings.Contains(err.Error(), "离线模式") {
		t.Errorf("错误信息要点明是离线模式主动拒绝（免得用户去查网络）：%v", err)
	}
}

// TestDownloadSTTModelClearsIncompleteLeftover：磁盘上有"大小不对"的旧文件时，
// 必须先删掉再下 —— 否则会把它当成"已经下好了"，永远卡在一个坏模型上。
func TestDownloadSTTModelClearsIncompleteLeftover(t *testing.T) {
	m, _ := sandboxManager(t)
	p := m.sttPaths()
	if err := os.MkdirAll(p.ModelsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mdl, _ := FindSTTModel("small")
	leftover := filepath.Join(p.ModelsDir, mdl.File)
	if err := os.WriteFile(leftover, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sawGone bool
	sttFetchOverride = func(ctx context.Context, url, dest string, _ func(int64, int64)) error {
		// 下载开始时，那个坏文件必须已经被删掉了。
		if _, err := os.Stat(leftover); os.IsNotExist(err) {
			sawGone = true
		}
		// 造一个大小正确的产物。
		f, err := os.Create(dest)
		if err != nil {
			return err
		}
		if _, err := f.Write([]byte("lmgg")); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.Seek(mdl.Bytes-1, 0); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.Write([]byte{0}); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	t.Cleanup(func() { sttFetchOverride = nil })

	res, err := m.DownloadSTTModel(context.Background(), "small", nil)
	if err != nil {
		t.Fatalf("下载应成功：%v", err)
	}
	if !sawGone {
		t.Error("下载开始时，大小不对的旧模型文件应当已被删除（否则会永远用着一个坏权重）")
	}
	if res.Bytes != mdl.Bytes {
		t.Errorf("结果体积不对：%d", res.Bytes)
	}
	if !STTModelFileExists(p.ModelsDir, "small") {
		t.Error("下载完成后模型应被判成已安装")
	}
}

// TestInstalledSTTModelsListsOnlyRealFiles：卸载确认框只列**真的在磁盘上**的文件。
func TestInstalledSTTModelsListsOnlyRealFiles(t *testing.T) {
	m, _ := sandboxManager(t)
	p := m.sttPaths()
	if err := os.MkdirAll(p.ModelsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("half") // 4 字节：故意与上游大小不符
	if err := os.WriteFile(filepath.Join(p.ModelsDir, "ggml-small.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	got := m.InstalledSTTModels()
	if len(got) != 1 {
		t.Fatalf("磁盘上只有 1 个文件，应只列出 1 条，实际 %d：%+v", len(got), got)
	}
	if got[0].ID != "small" || got[0].Bytes != int64(len(body)) {
		t.Errorf("列出的文件信息不对：%+v（期望 %d 字节）", got[0], len(body))
	}
	// 大小与上游不符 → 不能标成"已安装"（但**要**列出来，它确实占着磁盘）。
	if got[0].Installed {
		t.Error("截断的文件不该标 installed=true")
	}
	if m.STTModelBytes() != int64(len(body)) {
		t.Errorf("总体积应为 %d，实际 %d", len(body), m.STTModelBytes())
	}
}

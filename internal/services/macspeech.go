package services

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ============================================================================
//  macOS 语音合成（/usr/bin/say）—— 引擎
//
//  为什么是系统自带的 `say`，而不是网上那些 "macos speech server"（评估见
//  docs/应用市场-speech评估.md）：
//    · 零安装、零下载、零运行时：/usr/bin/say 是 macOS 的一部分
//      （Mach-O universal，含 arm64e），不需要 Node / Python / JVM / Docker，
//      也不违反"能原生就原生"（它比 Homebrew formula 还原生）；
//    · 完全离线：Speech Synthesis Manager 在本机合成，模型不下载、不联网；
//    · 中文可用：`say -v '?'` 里就有 zh_CN / zh_TW / zh_HK 音色；
//    · 与面板的 Qwen3 TTS **不重叠**：Qwen 是"音色克隆/高质量模型"路线
//      （要 2.9GB 权重 + Python + ffmpeg），这里是"装了就能用"的合成。
//
//  两个必须按"运行体"判定的坑（否则就会谎报成功）：
//    1. **`say` 对未知音色会静默成功**：实测 `say -v NoSuchVoice -o x.aiff` 返回
//       退出码 0，**但不产出任何文件**。所以音色必须先在 `say -v '?'` 的清单里
//       校验（lookupVoice），合成后还要复核产物存在且非空。
//    2. **`say` 自己不能编码 mp3**：`say -o x.mp3` 同样返回 0 却只写一个
//       16 字节的非音频桩。mp3 必须交给 ffmpeg（应用市场里已有 FFmpeg 条目）；
//       afconvert（系统自带）**不支持编码 mp3** —— 实测报
//       `Error: ExtAudioFileSetProperty ('cfmt') failed ('fmt?')`。
//       没有 ffmpeg 时如实报错，绝不返回一个坏文件。
//
//  格式链路（每一步都实测过）：
//      say -o seg.aiff                    → AIFC/lpcm 22050Hz 单声道（say 的默认产物）
//      afconvert -f WAVE -d LEI16@22050   → RIFF/WAVE PCM 16bit（中间格式，便于拼接）
//      afconvert -f AIFC -d BEI16@22050   → aiff（目标格式）
//      afconvert -f m4af -d aac -b 64000  → m4a（AAC）
//      ffmpeg -codec:a libmp3lame         → mp3（**系统不自带，缺了如实报错**）
// ============================================================================

const (
	// MacSpeechAppID 是应用市场里的条目 ID。
	MacSpeechAppID = "macspeech"
	// MacSpeechLabel 是面板托管的「语音合成网页界面」launchd 标签。
	MacSpeechLabel = "com.zizdog.macosspeech"
	// MacSpeechSlug 是面板别名（目录条目的 UI.Slug）：/<slug>/。
	MacSpeechSlug = "speech"
	// MacSpeechPort 是网页界面默认监听端口（只绑 127.0.0.1）。
	//
	// 8891 与目录里其它端口不冲突（8880 Qwen3 TTS / 8890 图片压缩 / 8899 音色接收端），
	// TestCatalogPortsAreUnique 会锁住唯一性。
	MacSpeechPort = 8891

	// MacSpeechSayBin / MacSpeechAfconvertBin 是系统自带工具（macOS 的一部分）。
	MacSpeechSayBin       = "/usr/bin/say"
	MacSpeechAfconvertBin = "/usr/bin/afconvert"

	// MacSpeechDefaultRate 是 say 的默认语速（词/分钟）。`say -r` 的单位就是这个，
	// 与 OpenAI 的 speed（倍率）不是一个东西 —— 见 RateFromSpeed。
	MacSpeechDefaultRate = 175
	// MacSpeechMinRate / MacSpeechMaxRate 是面板允许的语速范围：
	// 超出这个范围 say 的表现不可预期（实测 -r 9999 也返回 0），所以由面板夹取，
	// 并把**实际使用**的语速如实回给用户（响应头 X-Zizpanel-Rate）。
	MacSpeechMinRate = 80
	MacSpeechMaxRate = 400

	// MacSpeechMaxChars 是单次请求允许的最大字符数。超过它一律 400 如实拒绝，
	// 不做静默截断（截断会让用户以为整段都合成了）。
	MacSpeechMaxChars = 20000
	// MacSpeechChunkChars 是长文本切分的目标长度：分段合成才能给出**真实进度**，
	// 而不是让用户对着一个转圈的界面猜。
	MacSpeechChunkChars = 300
	// MacSpeechSampleRate 是 say 的合成采样率（实测 22050Hz 单声道）。
	MacSpeechSampleRate = 22050
)

// ---------------------------------------------------------------------------
//  错误：全部可被 errors.Is 判定，web 层据此如实映射成 400 / 503（不是 500）
// ---------------------------------------------------------------------------

var (
	// ErrSpeechEmptyText 文本为空。`say` 对空文本会返回 0 并写一个只有头的文件，
	// 所以必须由我们拦住。
	ErrSpeechEmptyText = errors.New("要合成的文本为空")
	// ErrSpeechTextTooLong 文本超过上限。
	ErrSpeechTextTooLong = errors.New("文本超过单次上限")
	// ErrSpeechUnknownVoice 音色不在 `say -v '?'` 的清单里。
	ErrSpeechUnknownVoice = errors.New("未知音色")
	// ErrSpeechUnsupportedFormat 目标格式在这台机器上不可用（例如没有 ffmpeg 时的 mp3）。
	ErrSpeechUnsupportedFormat = errors.New("目标格式不可用")
	// ErrSpeechEngineUnavailable say（或它依赖的系统能力）不可用。
	ErrSpeechEngineUnavailable = errors.New("macOS 语音合成引擎不可用")
	// ErrSpeechInvalidRequest 是"请求参数本身不合法"（例如 speed 超范围）。
	//
	// 为什么要单独一个：HTTP 层必须把**请求方的问题**映射成 4xx。
	// 少了它，speed=9 这种会被归到"其它错误"→ 500，用户看到的是"服务器错误"，
	// 而他明明只需要改一个数字（真机上就是从这条测试里发现并修掉的）。
	ErrSpeechInvalidRequest = errors.New("请求参数不合法")
)

// ---------------------------------------------------------------------------
//  音色
// ---------------------------------------------------------------------------

// SpeechVoice 是 `say -v '?'` 里的一个音色。
//
// 字段名就是对外契约（GET /v1/voices 的返回体），改名等于改协议。
type SpeechVoice struct {
	Name string `json:"name"`
	// Lang 是 BCP-47 风格的语音代码（zh_CN / en_US / ar_001…）
	Lang string `json:"lang"`
	// Example 是 Apple 给的那句示例（`# 你好！我叫婷婷。`）
	Example string `json:"example,omitempty"`
	// Chinese 标记"这个音色会说中文"。判据是语言代码前缀（zh / yue / cmn），
	// 不是猜名字 —— 界面上中文音色要排在前面并打标。
	Chinese bool `json:"chinese"`
}

// IsChineseSpeechLang 判断语音代码是不是中文（zh_CN / zh_TW / zh_HK / yue…）。
func IsChineseSpeechLang(lang string) bool {
	l := strings.ToLower(strings.TrimSpace(lang))
	for _, p := range []string{"zh", "yue", "cmn", "wuu", "nan"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// sayVoiceLineRe 匹配 `say -v '?'` 一行里"名字 + 语言代码"这两段。
//
// 行的形状（实测）：`Name<padding>lang<padding># 示例文本`
//
//	名字可以带空格与括号，也可以是中文：`Eddy (中文（中国大陆）)       zh_CN`
//	语言代码形如 zh_CN / en_US / ar_001。
//
// 非贪婪的 (.*?) 保证只有**行尾**那个语言代码会被匹配到（$ 锚定），
// 于是名字里的数字/下划线不会被误当成语言代码。
var sayVoiceLineRe = regexp.MustCompile(`^(.*?)\s+([a-zA-Z]{2,3}_[A-Za-z0-9]{2,4})$`)

// parseSayVoices 解析 `say -v '?'` 的输出。
//
// 纯函数（不跑进程、不碰文件系统），所以单测可以喂假输出锁住解析规则 ——
// 这条规则错了，"下拉框里全是乱码/找不到音色"就是必然。
func parseSayVoices(out string) []SpeechVoice {
	res := make([]SpeechVoice, 0, 64)
	seen := map[string]bool{}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		body, example := line, ""
		if i := strings.Index(line, "#"); i >= 0 {
			body, example = line[:i], strings.TrimSpace(line[i+1:])
		}
		m := sayVoiceLineRe.FindStringSubmatch(strings.TrimSpace(body))
		if m == nil {
			continue // 表头之类的行：解析不出来就跳过，不猜
		}
		name, lang := strings.TrimSpace(m[1]), m[2]
		if name == "" || lang == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue // 同一个音色出现两次（不同语言包）时保留第一个
		}
		seen[key] = true
		res = append(res, SpeechVoice{
			Name:    name,
			Lang:    lang,
			Example: example,
			Chinese: IsChineseSpeechLang(lang),
		})
	}
	return res
}

// LookupSpeechVoice 按名字查音色（大小写不敏感、允许两端空白）。
//
// **必须在调用 say 之前查**：`say -v 不存在的音色` 返回退出码 0 却不产出文件，
// 不查就等着把"成功"报给用户、然后给一个 0 字节的音频。
//
// 导出给 web 层做请求校验：校验必须在**开任务之前**发生（否则用户对着一个
// 注定失败的任务干等）。
func LookupSpeechVoice(voices []SpeechVoice, name string) (SpeechVoice, bool) {
	want := strings.TrimSpace(name)
	if want == "" {
		return SpeechVoice{}, false
	}
	for _, v := range voices {
		if strings.EqualFold(v.Name, want) {
			return v, true
		}
	}
	return SpeechVoice{}, false
}

// preferredChineseVoices 是"文本是中文、用户没指定音色"时的挑选顺序。
//
// 只用**每个 macOS 都有的**经典音色：Tingting(zh_CN) / Sinji(zh_HK) / Meijia(zh_TW)。
// 不带中文语音代码时返回 false，交给 say 用它自己的默认音色（不改用户的系统设置）。
var preferredChineseVoices = []string{"Tingting", "Sinji", "Meijia"}

// PreferredChineseVoiceNames 返回上面那份挑选顺序（GET /v1/voices 会带上它，
// 好让调用方知道"不指定 voice 时中文文本会落到哪个音色"）。
func PreferredChineseVoiceNames() []string {
	out := make([]string, len(preferredChineseVoices))
	copy(out, preferredChineseVoices)
	return out
}

// pickDefaultVoice 为一段文本挑默认音色。
func pickDefaultVoice(voices []SpeechVoice, text string) (SpeechVoice, bool) {
	if !hasCJK(text) {
		return SpeechVoice{}, false
	}
	for _, want := range preferredChineseVoices {
		if v, ok := LookupSpeechVoice(voices, want); ok {
			return v, true
		}
	}
	for _, v := range voices { // 退而求其次：任何中文音色
		if v.Chinese {
			return v, true
		}
	}
	return SpeechVoice{}, false
}

// hasCJK 判断文本里有没有中日韩统一表意文字（决定"要不要自动挑中文音色"）。
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
//  格式
// ---------------------------------------------------------------------------

// SpeechFormat 是支持的输出格式。取值是对外契约（请求体的 format 字段）。
type SpeechFormat string

const (
	SpeechFormatAIFF SpeechFormat = "aiff"
	SpeechFormatWAV  SpeechFormat = "wav"
	SpeechFormatM4A  SpeechFormat = "m4a"
	SpeechFormatMP3  SpeechFormat = "mp3"
)

// SpeechFormats 是界面下拉框的顺序，也是"这台机器支持哪些格式"的枚举来源。
var SpeechFormats = []SpeechFormat{SpeechFormatAIFF, SpeechFormatWAV, SpeechFormatM4A, SpeechFormatMP3}

// ParseSpeechFormat 解析请求里的格式名（含常见别名）。
//
// 空串 = 默认 aiff（say 的原生产物，链路最短、最不容易出错）。
func ParseSpeechFormat(s string) (SpeechFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return SpeechFormatAIFF, nil
	case "aiff", "aif", "aifc":
		return SpeechFormatAIFF, nil
	case "wav", "wave":
		return SpeechFormatWAV, nil
	case "m4a", "mp4", "aac", "m4af":
		return SpeechFormatM4A, nil
	case "mp3", "mpeg", "mpga":
		return SpeechFormatMP3, nil
	}
	return "", fmt.Errorf("%w：%q（支持 %s）", ErrSpeechUnsupportedFormat, s, SpeechFormatList())
}

// SpeechFormatList 返回 "aiff / wav / m4a / mp3" 这样的可读清单（错误信息里用）。
func SpeechFormatList() string {
	parts := make([]string, 0, len(SpeechFormats))
	for _, f := range SpeechFormats {
		parts = append(parts, string(f))
	}
	return strings.Join(parts, " / ")
}

// IsKnownSpeechFormat 判断格式是不是面板支持的四种之一。
//
// 引擎入口也做一次（不只是 web 层的 ParseSpeechFormat）：直接调 Synthesize 的
// 调用方传进一个没见过的格式时，必须在**开始合成之前**失败，而不是合成完再在
// 转换那一步兜底 —— 后者会白跑一遍 say，还可能给出一个后缀骗人的文件。
func IsKnownSpeechFormat(f SpeechFormat) bool {
	for _, x := range SpeechFormats {
		if x == f {
			return true
		}
	}
	return false
}

// ContentType 返回该格式的 MIME。
func (f SpeechFormat) ContentType() string {
	switch f {
	case SpeechFormatAIFF:
		return "audio/aiff"
	case SpeechFormatWAV:
		return "audio/wav"
	case SpeechFormatM4A:
		return "audio/mp4"
	case SpeechFormatMP3:
		return "audio/mpeg"
	}
	return "application/octet-stream"
}

// Ext 返回文件后缀（不含点）。
func (f SpeechFormat) Ext() string { return string(f) }

// NeedsFfmpeg 表示这个格式必须靠 ffmpeg 编码（系统自带的 afconvert 做不到）。
func (f SpeechFormat) NeedsFfmpeg() bool { return f == SpeechFormatMP3 }

// ---------------------------------------------------------------------------
//  引擎
// ---------------------------------------------------------------------------

// SpeechCommandRunner 跑一条命令并返回合并输出。
//
// 做成注入点（而不是直接 exec）是为了单测：合成与探测都**不允许**在单测里
// 真的去跑 say / afconvert，更不允许碰用户的音频设备（AGENTS 第三节）。
type SpeechCommandRunner func(ctx context.Context, timeout time.Duration, bin string, args ...string) (string, error)

// SpeechEngine 是"用 macOS 自带 say 合成语音"的引擎。
type SpeechEngine struct {
	// SayBin / AfconvertBin 是系统工具路径（留空用默认值）。
	SayBin       string
	AfconvertBin string
	// FfmpegBin 为空表示这台机器上没有 ffmpeg（此时 mp3 不可用，如实降级）。
	FfmpegBin string
	// Run 是命令执行器（测试注入）。
	Run SpeechCommandRunner
	// TempRoot 是临时目录父目录（空 = os.TempDir()）。
	TempRoot string
	// MaxChars / ChunkChars 覆盖默认上限（<=0 用默认值）。
	MaxChars   int
	ChunkChars int
}

// NewSpeechEngine 造一个用真实系统工具的引擎。
func NewSpeechEngine() *SpeechEngine {
	return &SpeechEngine{
		SayBin:       MacSpeechSayBin,
		AfconvertBin: MacSpeechAfconvertBin,
		FfmpegBin:    findFfmpegBin(),
		Run:          runSpeechCommand,
		TempRoot:     os.TempDir(),
		MaxChars:     MacSpeechMaxChars,
		ChunkChars:   MacSpeechChunkChars,
	}
}

// findFfmpegBin 找 ffmpeg（面板的基础环境条目装在 Homebrew 前缀下）。
//
// 找不到**不是错误**：aiff/wav/m4a 都不需要它，只有 mp3 需要。
func findFfmpegBin() string {
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// runSpeechCommand 是生产用的执行器：带超时、失败时把输出尾部带进错误。
func runSpeechCommand(ctx context.Context, timeout time.Duration, bin string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, args...).CombinedOutput()
	s := string(out)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return s, fmt.Errorf("命令超时（%s）：%s %s", timeout, bin, strings.Join(args, " "))
		}
		return s, fmt.Errorf("%s 执行失败: %v（%s）", filepath.Base(bin), err, tailText(strings.TrimSpace(s), 300))
	}
	return s, nil
}

// Available 报告 say 能不能用（判据贴着运行体：文件在、可执行）。
func (e *SpeechEngine) Available() (bool, string) {
	if strings.TrimSpace(e.SayBin) == "" {
		return false, "没有配置 say 可执行文件路径"
	}
	st, err := os.Stat(e.SayBin)
	if err != nil {
		return false, fmt.Sprintf("%s 不存在：%v（这不是 macOS，或系统被人为改动过）", e.SayBin, err)
	}
	if st.IsDir() {
		return false, fmt.Sprintf("%s 是目录，不是可执行文件", e.SayBin)
	}
	if st.Mode()&0o111 == 0 {
		return false, fmt.Sprintf("%s 没有可执行权限（mode %s）", e.SayBin, st.Mode())
	}
	return true, ""
}

// FfmpegPath 返回 ffmpeg 路径（空 = 没有）。
func (e *SpeechEngine) FfmpegPath() string { return e.FfmpegBin }

// Voices 真的跑一次 `say -v '?'` 并把结果解析出来。
//
// 这就是"能力探活"：拿不到音色清单 = 这台机器现在合成不了语音，
// 而不是"服务进程活着所以健康"。
func (e *SpeechEngine) Voices(ctx context.Context) ([]SpeechVoice, error) {
	if ok, reason := e.Available(); !ok {
		return nil, fmt.Errorf("%w：%s", ErrSpeechEngineUnavailable, reason)
	}
	run := e.Run
	if run == nil {
		run = runSpeechCommand
	}
	out, err := run(ctx, 20*time.Second, e.SayBin, "-v", "?")
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrSpeechEngineUnavailable, err)
	}
	voices := parseSayVoices(out)
	if len(voices) == 0 {
		return nil, fmt.Errorf("%w：`say -v '?'` 没有返回任何音色", ErrSpeechEngineUnavailable)
	}
	return voices, nil
}

// FormatAvailable 报告某个格式现在能不能产出。
func (e *SpeechEngine) FormatAvailable(f SpeechFormat) (bool, string) {
	if !IsKnownSpeechFormat(f) {
		return false, fmt.Sprintf("不支持的格式 %q（支持 %s）", f, SpeechFormatList())
	}
	if f.NeedsFfmpeg() && strings.TrimSpace(e.FfmpegBin) == "" {
		return false, "mp3 需要 ffmpeg 编码（macOS 自带的 say 与 afconvert 都**不能**编码 mp3）；" +
			"可以到「应用市场 → FFmpeg」装上它，或改用 aiff / wav / m4a"
	}
	if !f.NeedsFfmpeg() {
		bin := e.AfconvertBin
		if bin == "" {
			bin = MacSpeechAfconvertBin
		}
		if st, err := os.Stat(bin); err != nil || st.IsDir() {
			return false, fmt.Sprintf("缺少系统工具 %s（aiff 之外的目标格式需要它做转换）", bin)
		}
	}
	return true, ""
}

// FormatsAvailability 返回每个格式的可用性（界面据此禁用下拉项，而不是让用户点了才报错）。
func (e *SpeechEngine) FormatsAvailability() []SpeechFormatState {
	out := make([]SpeechFormatState, 0, len(SpeechFormats))
	for _, f := range SpeechFormats {
		ok, reason := e.FormatAvailable(f)
		out = append(out, SpeechFormatState{Format: f, Available: ok, Reason: reason})
	}
	return out
}

// SpeechFormatState 是单个格式的可用性（对外契约）。
type SpeechFormatState struct {
	Format    SpeechFormat `json:"format"`
	Available bool         `json:"available"`
	Reason    string       `json:"reason,omitempty"`
	Needs     string       `json:"needs,omitempty"`
}

// RateFromSpeed 把 OpenAI 的 speed（倍率，1.0 = 正常）换算成 `say -r` 的词/分钟。
//
// 单位不同，所以必须换算而不是直接乘：speed 是"相对默认语速的倍数"，
// 而 say 的 -r 是绝对词/分钟。换算后夹取到 [MinRate, MaxRate]，
// 并让调用方把**实际使用**的值如实回给用户（超出范围时面板不假装照做了）。
func RateFromSpeed(speed float64) int {
	if speed <= 0 {
		return MacSpeechDefaultRate
	}
	rate := int(speed*float64(MacSpeechDefaultRate) + 0.5)
	if rate < MacSpeechMinRate {
		rate = MacSpeechMinRate
	}
	if rate > MacSpeechMaxRate {
		rate = MacSpeechMaxRate
	}
	return rate
}

// SpeedFromRate 是 RateFromSpeed 的逆运算（把实际语速回报成倍率）。
func SpeedFromRate(rate int) float64 {
	if rate <= 0 {
		return 1
	}
	return float64(rate) / float64(MacSpeechDefaultRate)
}

// ---------------------------------------------------------------------------
//  文本切分（纯函数，可单测）
// ---------------------------------------------------------------------------

// speechSentenceRe 是切句点：中日韩句末标点 + 英文句末标点 + 换行 + 分号。
//
// 分段的目的是"给得出真实进度"：长文本按句切段、逐段合成，
// 界面上的进度就是真的（已完成 N/M 段），而不是一个假的转圈。
var speechSentenceRe = regexp.MustCompile(`[^。！？!?；;\n]+[。！？!?；;\n]*`)

// SplitSpeechText 把文本切成每段不超过 maxChars 个字符的片段。
//
// 规则：优先按句子边界切；单句本身超长时在**字符**边界硬切（不丢字符、不截断）。
// maxChars<=0 时整段返回。
func SplitSpeechText(text string, maxChars int) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if maxChars <= 0 {
		return []string{trimmed}
	}
	var out []string
	var buf strings.Builder
	bufRunes := 0
	flush := func() {
		s := strings.TrimSpace(buf.String())
		if s != "" {
			out = append(out, s)
		}
		buf.Reset()
		bufRunes = 0
	}
	for _, sentence := range speechSentenceRe.FindAllString(trimmed, -1) {
		if strings.TrimSpace(sentence) == "" {
			continue
		}
		rs := []rune(sentence)
		// 单句超长：先冲掉手上的缓冲，再按字符硬切成若干段。
		if len(rs) > maxChars {
			flush()
			for i := 0; i < len(rs); i += maxChars {
				end := i + maxChars
				if end > len(rs) {
					end = len(rs)
				}
				out = append(out, strings.TrimSpace(string(rs[i:end])))
			}
			continue
		}
		if bufRunes+len(rs) > maxChars {
			flush()
		}
		buf.WriteString(sentence)
		bufRunes += len(rs)
	}
	flush()
	// 全部被 TrimSpace 吃掉（例如纯空白/纯标点）时，退化成整段 ——
	// 宁可合成一段"没有可读内容"的音频，也不要静默返回空列表让调用方当成成功。
	if len(out) == 0 {
		return []string{trimmed}
	}
	return out
}

// ---------------------------------------------------------------------------
//  WAV 拼接（纯函数，可单测）
// ---------------------------------------------------------------------------

// wavPCMInfo 是标准 WAV 里我们需要的那几个字段。
type wavPCMInfo struct {
	Channels   int
	SampleRate int
	Bits       int
	DataOffset int
	DataLen    int
}

// parseWAVPCM 解析一个 PCM WAV，返回格式信息与 data 块位置。
//
// 为什么必须真的走 chunk 链，而不是"跳过 44 字节"：afconvert 产出的 WAV 里
// 在 fmt 与 data 之间还有一个 `FLLR` 填充块（实测：data 从 4096 开始，不是 44），
// 按 44 硬跳会把填充块当音频拼进去 —— 表现为"合成出来的音频开头有一小段噪声"。
func parseWAVPCM(b []byte) (wavPCMInfo, error) {
	var info wavPCMInfo
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return info, errors.New("不是 RIFF/WAVE 文件")
	}
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if body+16 > len(b) {
				return info, errors.New("WAV 的 fmt 块不完整")
			}
			if binary.LittleEndian.Uint16(b[body:body+2]) != 1 {
				return info, errors.New("WAV 不是未压缩 PCM（面板只拼接自己转出来的 PCM）")
			}
			info.Channels = int(binary.LittleEndian.Uint16(b[body+2 : body+4]))
			info.SampleRate = int(binary.LittleEndian.Uint32(b[body+4 : body+8]))
			info.Bits = int(binary.LittleEndian.Uint16(b[body+14 : body+16]))
		case "data":
			if body+size > len(b) {
				// 有些写入器把 size 写成 0 或占位值：按实际剩余长度处理
				size = len(b) - body
			}
			info.DataOffset, info.DataLen = body, size
		}
		off = body + size
		if size%2 == 1 {
			off++
		}
	}
	if info.DataOffset == 0 || info.DataLen <= 0 {
		return info, errors.New("WAV 里没有 data 块")
	}
	if info.Channels <= 0 || info.SampleRate <= 0 || info.Bits <= 0 {
		return info, errors.New("WAV 缺少 fmt 信息")
	}
	return info, nil
}

// ConcatWAV 把若干段 PCM WAV 拼成一个 WAV（同采样率/声道/位深）。
//
// 长文本分段合成的最后一公里：每段各自是完整 WAV，拼的时候只取 data 块、
// 重新写一份标准 44 字节头。全部段必须是同样的格式，否则如实报错
// （拼不同格式只会得到一段莫名其妙的噪声）。
func ConcatWAV(parts [][]byte) ([]byte, error) {
	if len(parts) == 0 {
		return nil, errors.New("没有可拼接的音频段")
	}
	first, err := parseWAVPCM(parts[0])
	if err != nil {
		return nil, fmt.Errorf("第 1 段音频无法解析：%w", err)
	}
	total := 0
	datas := make([][]byte, 0, len(parts))
	for i, p := range parts {
		info, perr := parseWAVPCM(p)
		if perr != nil {
			return nil, fmt.Errorf("第 %d 段音频无法解析：%w", i+1, perr)
		}
		if info.Channels != first.Channels || info.SampleRate != first.SampleRate || info.Bits != first.Bits {
			return nil, fmt.Errorf("第 %d 段音频的格式与前一段不一致（%d ch / %d Hz / %d bit vs %d ch / %d Hz / %d bit）",
				i+1, info.Channels, info.SampleRate, info.Bits,
				first.Channels, first.SampleRate, first.Bits)
		}
		datas = append(datas, p[info.DataOffset:info.DataOffset+info.DataLen])
		total += info.DataLen
	}

	blockAlign := first.Channels * first.Bits / 8
	out := make([]byte, 44+total)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+total))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:24], uint16(first.Channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(first.SampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(first.SampleRate*blockAlign))
	binary.LittleEndian.PutUint16(out[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(out[34:36], uint16(first.Bits))
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(total))
	at := 44
	for _, d := range datas {
		copy(out[at:], d)
		at += len(d)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
//  合成
// ---------------------------------------------------------------------------

// SynthOptions 是一次合成请求。
type SynthOptions struct {
	Text string
	// Voice 为空时按文本自动挑（中文文本优先 Tingting 等中文音色）。
	Voice string
	// Speed 是 OpenAI 风格的倍率（0 = 1.0）。
	Speed float64
	// Format 为空 = aiff。
	Format SpeechFormat
	// WorkDir 为空时自动建临时目录（结果里的 Cleanup 会删掉它）。
	WorkDir string
	// Progress 在每段合成完成后被调用（done=已完成段数，total=总段数）。
	// 有它才有"真实进度"——不是假转圈。
	Progress func(done, total int, note string)
}

// SynthResult 是一次合成的产物。
type SynthResult struct {
	// Path 是产物文件（在 WorkDir 里）。
	Path string
	// WorkDir 是本任务的私有目录，Cleanup 会整个删掉。
	WorkDir string
	// OwnWorkDir 表示目录是引擎自己建的（调用方不必管）。
	OwnWorkDir  bool
	ContentType string
	Format      SpeechFormat
	// Voice / Lang 是**实际使用**的音色（空 = 用了 say 自己的默认音色）。
	Voice string
	Lang  string
	// Rate 是**实际使用**的语速（词/分钟），Speed 是它换算回的倍率 ——
	// 夹取过就与请求值不同，必须如实回报给用户。
	Rate  int
	Speed float64
	Chars int
	// Segments 是切成了几段（长文本进度就是按它算的）。
	Segments int
	Bytes    int64
}

// Cleanup 删掉这次合成的临时目录（幂等）。
func (r *SynthResult) Cleanup() {
	if r == nil || r.WorkDir == "" {
		return
	}
	_ = os.RemoveAll(r.WorkDir)
	r.WorkDir = ""
}

// SynthTimeout 是单段合成的超时。实测 say 合成 39 秒音频只要 0.7 秒，
// 5 分钟只针对"文本极长 + 机器极慢"的兜底，不是常见的等待时间。
const SynthTimeout = 5 * time.Minute

// Synthesize 把文本合成为指定格式的音频文件。
//
// 长文本按句子切段、逐段合成，并用 ConcatWAV 拼接（进度是真的）。
// 任何一步失败都返回错误 —— 尤其是"say 返回 0 但没产出文件"这种
// 静默失败，必须在这里被抓住（否则就是把 0 字节当成功交给用户）。
func (e *SpeechEngine) Synthesize(ctx context.Context, opt SynthOptions) (*SynthResult, error) {
	text := strings.TrimSpace(opt.Text)
	if text == "" {
		return nil, ErrSpeechEmptyText
	}
	chars := len([]rune(text))
	maxChars := e.MaxChars
	if maxChars <= 0 {
		maxChars = MacSpeechMaxChars
	}
	if chars > maxChars {
		return nil, fmt.Errorf("%w（当前 %d 字，上限 %d 字）：请拆成多次请求",
			ErrSpeechTextTooLong, chars, maxChars)
	}
	if ok, reason := e.Available(); !ok {
		return nil, fmt.Errorf("%w：%s", ErrSpeechEngineUnavailable, reason)
	}
	format := opt.Format
	if format == "" {
		format = SpeechFormatAIFF
	}
	if ok, reason := e.FormatAvailable(format); !ok {
		return nil, fmt.Errorf("%w：%s", ErrSpeechUnsupportedFormat, reason)
	}
	voices, err := e.Voices(ctx)
	if err != nil {
		return nil, err
	}
	var chosen SpeechVoice
	if strings.TrimSpace(opt.Voice) != "" {
		v, ok := LookupSpeechVoice(voices, opt.Voice)
		if !ok {
			return nil, fmt.Errorf("%w：%q（这台机器上有 %d 个音色，可 GET /v1/voices 查看）",
				ErrSpeechUnknownVoice, strings.TrimSpace(opt.Voice), len(voices))
		}
		chosen = v
	} else if v, ok := pickDefaultVoice(voices, text); ok {
		chosen = v
	}
	rate := RateFromSpeed(opt.Speed)

	workDir := strings.TrimSpace(opt.WorkDir)
	own := false
	if workDir == "" {
		root := e.TempRoot
		if root == "" {
			root = os.TempDir()
		}
		d, derr := os.MkdirTemp(root, "zp-speech-")
		if derr != nil {
			return nil, fmt.Errorf("创建临时目录失败: %w", derr)
		}
		workDir, own = d, true
	}
	fail := func(err error) (*SynthResult, error) {
		if own {
			_ = os.RemoveAll(workDir)
		}
		return nil, err
	}

	chunkChars := e.ChunkChars
	if chunkChars <= 0 {
		chunkChars = MacSpeechChunkChars
	}
	segments := SplitSpeechText(text, chunkChars)
	if len(segments) == 0 {
		return fail(ErrSpeechEmptyText)
	}

	parts := make([][]byte, 0, len(segments))
	for i, seg := range segments {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		wav, serr := e.synthSegmentToWAV(ctx, workDir, i+1, seg, chosen, rate)
		if serr != nil {
			return fail(fmt.Errorf("第 %d/%d 段合成失败：%w", i+1, len(segments), serr))
		}
		parts = append(parts, wav)
		if opt.Progress != nil {
			opt.Progress(i+1, len(segments), fmt.Sprintf("已合成 %d/%d 段", i+1, len(segments)))
		}
	}

	merged, cerr := ConcatWAV(parts)
	if cerr != nil {
		return fail(fmt.Errorf("拼接音频失败：%w", cerr))
	}
	mergedPath := filepath.Join(workDir, "merged.wav")
	if werr := os.WriteFile(mergedPath, merged, 0o644); werr != nil {
		return fail(fmt.Errorf("写入中间文件失败：%w", werr))
	}

	outPath := filepath.Join(workDir, "speech."+format.Ext())
	if cerr := e.convert(ctx, mergedPath, outPath, format); cerr != nil {
		return fail(cerr)
	}
	st, serr := os.Stat(outPath)
	if serr != nil || st.Size() == 0 {
		// 到这一步还拿不到非空文件 = 引擎静默失败（say/afconvert 都可能这样）：
		// 如实报错，绝不把一个空文件当成功返回。
		return fail(fmt.Errorf("转换后没有得到音频文件（%s）：这是引擎静默失败，请反馈", outPath))
	}
	return &SynthResult{
		Path:        outPath,
		WorkDir:     workDir,
		OwnWorkDir:  own,
		ContentType: format.ContentType(),
		Format:      format,
		Voice:       chosen.Name,
		Lang:        chosen.Lang,
		Rate:        rate,
		Speed:       SpeedFromRate(rate),
		Chars:       chars,
		Segments:    len(segments),
		Bytes:       st.Size(),
	}, nil
}

// synthSegmentToWAV 合成一段文本并转成 PCM WAV（拼接用的中间格式）。
//
// 返回的是**文件内容**（不是路径）：拼接前所有段都在内存里，段数有上限
// （20000 字 / 300 字 ≈ 67 段，每段几百 KB，没有内存风险）。
func (e *SpeechEngine) synthSegmentToWAV(ctx context.Context, workDir string, idx int, text string,
	voice SpeechVoice, rate int) ([]byte, error) {

	run := e.Run
	if run == nil {
		run = runSpeechCommand
	}
	txtPath := filepath.Join(workDir, fmt.Sprintf("seg-%03d.txt", idx))
	if err := os.WriteFile(txtPath, []byte(text), 0o600); err != nil {
		return nil, fmt.Errorf("写入待合成文本失败: %w", err)
	}
	aiffPath := filepath.Join(workDir, fmt.Sprintf("seg-%03d.aiff", idx))
	// 每次都先删掉旧文件：`say` 对未知音色会**返回 0 且不写文件**，
	// 如果这一段的旧文件还在，就会把上一段的内容当成这一段的结果拼进去。
	_ = os.Remove(aiffPath)

	args := []string{"-o", aiffPath}
	if voice.Name != "" {
		args = append(args, "-v", voice.Name)
	}
	if rate > 0 {
		args = append(args, "-r", strconv.Itoa(rate))
	}
	// 用 -f 传文本（而不是把文本当参数）：命令行长度有上限，
	// 而且 exec 传参可以完全避开 shell 引号问题。
	args = append(args, "-f", txtPath)
	if _, err := run(ctx, SynthTimeout, e.SayBin, args...); err != nil {
		return nil, err
	}
	st, err := os.Stat(aiffPath)
	if err != nil || st.Size() == 0 {
		// 这就是"say 静默成功"的现场：退出码 0、没有文件（未知音色时实测如此）。
		return nil, fmt.Errorf("%w：say 返回成功但没有产出音频文件（音色 %q 可能不可用）",
			ErrSpeechEngineUnavailable, voice.Name)
	}

	wavPath := filepath.Join(workDir, fmt.Sprintf("seg-%03d.wav", idx))
	afc := e.AfconvertBin
	if afc == "" {
		afc = MacSpeechAfconvertBin
	}
	if _, err := run(ctx, SynthTimeout, afc,
		"-f", "WAVE", "-d", fmt.Sprintf("LEI16@%d", MacSpeechSampleRate), aiffPath, wavPath); err != nil {
		return nil, err
	}
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		return nil, fmt.Errorf("读取转换后的音频失败: %w", err)
	}
	if len(wav) == 0 {
		return nil, errors.New("afconvert 产出了 0 字节的音频文件")
	}
	return wav, nil
}

// convert 把拼接好的 PCM WAV 转成目标格式。
//
// wav 直接复用中间文件；aiff/m4a 用系统自带的 afconvert；mp3 只能靠 ffmpeg
// （系统不自带 MP3 编码器 —— 实测 afconvert 报 'cfmt' failed）。
func (e *SpeechEngine) convert(ctx context.Context, wavPath, outPath string, format SpeechFormat) error {
	run := e.Run
	if run == nil {
		run = runSpeechCommand
	}
	switch format {
	case SpeechFormatWAV:
		in, err := os.ReadFile(wavPath)
		if err != nil {
			return fmt.Errorf("读取中间音频失败: %w", err)
		}
		if err := os.WriteFile(outPath, in, 0o644); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", format, err)
		}
		return nil
	case SpeechFormatAIFF, SpeechFormatM4A:
		afc := e.AfconvertBin
		if afc == "" {
			afc = MacSpeechAfconvertBin
		}
		args := []string{"-f", "AIFC", "-d", fmt.Sprintf("BEI16@%d", MacSpeechSampleRate)}
		if format == SpeechFormatM4A {
			args = []string{"-f", "m4af", "-d", "aac", "-b", "64000"}
		}
		_, err := run(ctx, SynthTimeout, afc, append(args, wavPath, outPath)...)
		return err
	case SpeechFormatMP3:
		if strings.TrimSpace(e.FfmpegBin) == "" {
			return fmt.Errorf("%w：mp3 需要 ffmpeg（系统自带的 say 与 afconvert 都不能编码 mp3）；"+
				"可从「应用市场 → FFmpeg」安装，或改用 aiff / wav / m4a", ErrSpeechUnsupportedFormat)
		}
		_, err := run(ctx, SynthTimeout, e.FfmpegBin,
			"-hide_banner", "-loglevel", "error", "-y", "-i", wavPath,
			"-codec:a", "libmp3lame", "-qscale:a", "4", outPath)
		return err
	}
	return fmt.Errorf("%w：%q", ErrSpeechUnsupportedFormat, format)
}

// ---------------------------------------------------------------------------
//  健康快照
// ---------------------------------------------------------------------------

// SpeechHealth 是 /healthz 的返回体（也是"能力探活"的证据）。
type SpeechHealth struct {
	OK bool `json:"ok"`
	// SayBin / SayPresent：引擎本体。
	SayBin     string `json:"say_bin"`
	SayPresent bool   `json:"say_present"`
	// Voices / ChineseVoices：**真实跑** `say -v '?'` 数出来的，不是猜的。
	Voices        int `json:"voices"`
	ChineseVoices int `json:"chinese_voices"`
	// Afconvert / Ffmpeg：转换链路。
	Afconvert      string `json:"afconvert"`
	AfconvertReady bool   `json:"afconvert_ready"`
	Ffmpeg         string `json:"ffmpeg,omitempty"`
	// Formats：每个格式现在能不能产出（界面据此禁用下拉项）。
	Formats []SpeechFormatState `json:"formats"`
	// Service / MarketAppID 是自我标识。
	Service     string `json:"service"`
	MarketAppID string `json:"market_app_id"`
	// Reason 在 ok=false 时说明**为什么不可用**（绝不绿灯）。
	Reason string `json:"reason,omitempty"`
	// EffectiveRateDefault 是 speed 缺省时的实际语速（词/分钟）。
	EffectiveRateDefault int `json:"effective_rate_default"`
	MaxChars             int `json:"max_chars"`
	ChunkChars           int `json:"chunk_chars"`
	SampleRate           int `json:"sample_rate"`
}

// Health 真的探一次活并把结论组织成对外契约。
//
// 判据贴着能力（AGENTS 第三节）：say 在不在、`say -v '?'` 答不答、有多少音色。
// "进程活着"不算健康 —— 那样会给出一个"每次合成都失败"的绿灯。
func (e *SpeechEngine) Health(ctx context.Context) SpeechHealth {
	maxChars := e.MaxChars
	if maxChars <= 0 {
		maxChars = MacSpeechMaxChars
	}
	chunkChars := e.ChunkChars
	if chunkChars <= 0 {
		chunkChars = MacSpeechChunkChars
	}
	h := SpeechHealth{
		SayBin:               e.SayBin,
		Afconvert:            e.AfconvertBin,
		Ffmpeg:               e.FfmpegBin,
		Formats:              e.FormatsAvailability(),
		Service:              MacSpeechAppID,
		MarketAppID:          MacSpeechAppID,
		EffectiveRateDefault: MacSpeechDefaultRate,
		MaxChars:             maxChars,
		ChunkChars:           chunkChars,
		SampleRate:           MacSpeechSampleRate,
	}
	if ok, reason := e.Available(); !ok {
		h.Reason = reason
		return h
	}
	h.SayPresent = true
	if st, err := os.Stat(h.Afconvert); err == nil && !st.IsDir() {
		h.AfconvertReady = true
	}
	voices, err := e.Voices(ctx)
	if err != nil {
		h.Reason = err.Error()
		return h
	}
	h.Voices = len(voices)
	for _, v := range voices {
		if v.Chinese {
			h.ChineseVoices++
		}
	}
	h.OK = h.Voices > 0
	if !h.OK {
		h.Reason = "`say -v '?'` 没有返回任何音色"
	}
	return h
}

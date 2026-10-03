// Package videoopt 是「文件管理 → 🎬 压缩视频」的规划与执行实现。
//
// 为什么单独成包：分辨率封顶、码率封顶、预计体积这三件事必须是**纯函数**，
// 门禁才能在不跑真 ffmpeg 的情况下断言"绝不放大、绝不越压越大"。
// 规划只有这一份实现（PlanOne/BuildPlan），/video-plan 与 /video-compress 都走它，
// 所以"面板上看到的计划"与"真正执行的东西"不可能漂移。
package videoopt

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Preset 是一档目标分辨率。
//
// CapHeight 是"按档位封顶"的边长：**横屏按高度封顶、竖屏按宽度封顶**
// （竖屏视频 1080x1920 选 720p 时封的是宽度 720，而不是把高度压到 720）。
type Preset struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	CapHeight   int    `json:"cap_height"`
	DefaultKbps int    `json:"default_kbps"`
	// Hint 是选中这一档时给用户看的一句话（前端不重复写文案）。
	Hint string `json:"hint,omitempty"`
}

// 各档的"能用下限"建议码率（kbps）：网络视频口径（能发给别人看就行），
// 来自 2026-09 真机实测的经验值，不是标准里的数字。
const (
	Kbps360p  = 400
	Kbps480p  = 800
	Kbps720p  = 1500
	Kbps1080p = 3000
)

// PresetSource 是"原始"档：**不缩放**，只可能降码率/换编码（CapHeight=0 即不封顶）。
const PresetSource = "source"

var presets = []Preset{
	{ID: "360p", Label: "360p", CapHeight: 360, DefaultKbps: Kbps360p,
		Hint: "封顶 360p；只降不升，小画面绝不放大"},
	{ID: "480p", Label: "480p", CapHeight: 480, DefaultKbps: Kbps480p,
		Hint: "封顶 480p；只降不升，小画面绝不放大"},
	{ID: "720p", Label: "720p", CapHeight: 720, DefaultKbps: Kbps720p,
		Hint: "封顶 720p；只降不升，小画面绝不放大"},
	{ID: "1080p", Label: "1080p", CapHeight: 1080, DefaultKbps: Kbps1080p,
		Hint: "封顶 1080p；只降不升，720p 源仍是 720p"},
	{ID: PresetSource, Label: "原始", CapHeight: 0, DefaultKbps: 0,
		Hint: "不缩放：只可能降码率或换编码；码率按源分辨率建议"},
}

// sourceTiers 是"原始档按源分辨率给建议码率"用的档位（升序）。
var sourceTiers = []struct{ Side, KBps int }{
	{360, Kbps360p}, {480, Kbps480p}, {720, Kbps720p}, {1080, Kbps1080p},
}

// SuggestedKBps 是「原始」档的建议码率：**min(该源分辨率的档位建议, 源码率×0.7)**。
//
// 用户拍板的口径（A 方案）：档位建议按源分辨率取（封顶边与 targetSize 同口径：
// 横屏取高、竖屏取宽；落在两档之间取更接近的那一档，正好居中取上限档；低于 360 取 400、
// 高于 1080 取 3000），再用源码率×0.7 压一档 —— 这样常见源（如 720p/1500k）
// 建议 1050 而不是 1500，**不会被"封顶即跳过"整片跳过**，真的能压小约 30%。
//
// 0.7 < 0.95（bitrateSafety）⇒ 建议值天然不会触发封顶；
// 用户**手动**把码率拉到 ≥ 原片×0.95 时，封顶即跳过的规则照旧不变（判据在 planBitrates）。
func SuggestedKBps(w, h, srcKbps int) int {
	sug := tierKBps(w, h)
	if srcKbps > 0 {
		if byRate := int(math.Floor(float64(srcKbps) * sourceRateFactor)); byRate > 0 && byRate < sug {
			return byRate
		}
	}
	return sug
}

// tierKBps 只按源分辨率给档位建议（不含"源码率×0.7"那一档）。
func tierKBps(w, h int) int {
	side := h
	if w > 0 && w < h {
		side = w
	}
	if side <= sourceTiers[0].Side {
		return sourceTiers[0].KBps
	}
	for i := 1; i < len(sourceTiers); i++ {
		lo, hi := sourceTiers[i-1], sourceTiers[i]
		if side > hi.Side {
			continue
		}
		if side-lo.Side >= hi.Side-side {
			return hi.KBps
		}
		return lo.KBps
	}
	return sourceTiers[len(sourceTiers)-1].KBps
}

// Presets 返回五档预设（副本，调用方改不动包内状态）。
func Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	return out
}

// DefaultPresetID 是默认档位：480p。
//
// 为什么不是「原始」：用户拍板 —— 「原始 + 目标码率」在常见源上会因"封顶即跳过"
// 整片跳过（实测 `压缩 0 个 · 原样放入 output 2 个`），默认必须真的在压。
const DefaultPresetID = "480p"

// IsSourcePreset 判断这一档是不是"原始"（不缩放）。
func IsSourcePreset(p Preset) bool {
	return strings.EqualFold(strings.TrimSpace(p.ID), PresetSource)
}

// FindPreset 按 ID 找档位。
func FindPreset(id string) (Preset, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// BitrateChoice 是面板码率下拉的一项（label 由后端给，前端不重复写数字）。
type BitrateChoice struct {
	KBps  int    `json:"kbps"`
	Label string `json:"label"`
	// Hint 是这一项的细节（前端放进 option 的 title，不占主句）。
	Hint string `json:"hint,omitempty"`
}

// BitrateChoices 返回某一档的码率选项：档位下限 / 1.5× / 2×。
//
// 默认用**网络视频能用下限**（360p=400、480p=800、720p=1500、1080p=3000 kbps）：
// 用户要的往往是"能发给别人看就行"，更大的码率只是浪费流量。
//
// 原始档不缩放 ⇒ 默认项是"按源分辨率建议"（0 = 规划时逐文件取档位下限），
// 也可以手动指定一个绝对码率；两种都照旧受原码率×0.95 封顶。
func BitrateChoices(p Preset) []BitrateChoice {
	if IsSourcePreset(p) {
		return []BitrateChoice{
			{KBps: 0, Label: "按源分辨率建议（默认）",
				Hint: "源 1080p→3000 / 720p→1500 / 480p→800 / 360p→400 kbps；再受原码率×0.95 封顶"},
			{KBps: Kbps360p, Label: fmt.Sprintf("%d kbps · 更小", Kbps360p)},
			{KBps: Kbps720p, Label: fmt.Sprintf("%d kbps · 清晰些", Kbps720p)},
			{KBps: Kbps1080p, Label: fmt.Sprintf("%d kbps · 更清晰", Kbps1080p)},
		}
	}
	base := p.DefaultKbps
	if base <= 0 {
		base = Kbps480p
	}
	return []BitrateChoice{
		{KBps: base, Label: fmt.Sprintf("%d kbps · %s 能用下限", base, p.ID)},
		{KBps: base * 3 / 2, Label: fmt.Sprintf("%d kbps · 清晰些", base*3/2)},
		{KBps: base * 2, Label: fmt.Sprintf("%d kbps · 更清晰", base*2)},
	}
}

// ResolveKBps 把请求里的 kbps 归一化成实际要用的值（0 = 档位下限；原始档 = 按源分辨率建议）。
func ResolveKBps(p Preset, kbps int) int {
	if kbps > 0 {
		return kbps
	}
	if IsSourcePreset(p) {
		return 0 // 0 = 规划时按该文件的源分辨率取档位下限（见 SuggestedKBps）
	}
	if p.DefaultKbps > 0 {
		return p.DefaultKbps
	}
	return Kbps480p
}

const (
	// MinKBps / MaxKBps 是用户可选码率的合法区间（超出即 400，不静默钳制）。
	MinKBps = 100
	MaxKBps = 20000
	// audioMaxKbps 是音频码率上限；原音轨更低时取原值。
	audioMaxKbps = 96
	// bitrateSafety 是"绝不越压越大"的安全系数：实际视频码率还要再乘它。
	bitrateSafety = 0.95
	// sourceRateFactor 是「原始」档建议码率的"按原片压一档"系数（用户拍板 0.7）：
	// 建议 = min(源分辨率的档位建议, 源码率×0.7) ⇒ 默认能真的压小约 30%。
	sourceRateFactor = 0.7
	// minSide 是能转码的最小边长（再小的视频没有压缩意义，也过不了 yuv420p 的偶数要求）。
	minSide = 16
)

// ============================================================================
//  选项（编码器 / 模式 / 质量档 / 2-pass）
//
//  默认值只在这里定义一次：面板默认、"没带字段的老请求"兜底、门禁断言都取它。
// ============================================================================

const (
	// EncoderHardware / EncoderCPU 是两个编码器取值（请求与响应同一套字符串）。
	EncoderHardware = "hardware"
	EncoderCPU      = "cpu"
	// ModeBitrate 按目标码率压（体积可预估）；ModeQuality 质量优先（体积不可预估）。
	ModeBitrate = "bitrate"
	ModeQuality = "quality"

	// DefaultEncoder / DefaultMode 是面板默认值（改这里就同时改了后端兜底与面板初值）。
	//
	// 实测来源（2026-09-25，M4 + ffmpeg 9.0.1，源 H.264 1280x720 1474kbps 701s 136.9MB，
	// 目标 852x480）：CPU 25s / SSIM 0.9670 与硬件 22s / SSIM 0.9523 耗时接近
	//（瓶颈在解码+缩放），但**同码率 CPU 画质明显更好** ⇒ 默认 CPU。
	DefaultEncoder = EncoderCPU
	// 默认模式是目标码率（用户拍板：体积可预估）。CRF 仍是可选项，
	// 选它时面板照旧标"体积不可预估、可能不小于原文件"。
	DefaultMode = ModeBitrate

	// CPU 质量优先 = libx264 -crf N：**越小画质越好、文件越大**。
	// 实测：CRF 26 → 81.9MB / 957kbps / SSIM 0.9737（省 37%）；
	// CRF 24 → 100.8MB / 1177kbps（只省 23%）⇒ 默认 26。
	CRFHigh     = 24
	CRFBalanced = 26
	CRFSmall    = 28
	// DefaultCRF 是 CPU 质量优先的默认 CRF。
	DefaultCRF = CRFBalanced

	// 硬件质量优先 = hevc_videotoolbox -q:v N：**越大画质越好、文件越大**
	//（方向与 CRF 相反 —— 不是同一把尺子，面板必须如实说明）。
	// 2026-09-28 真机标定（同源 720p/1474kbps）：q:v 35≈SSIM 0.963、45≈0.972、50≈0.980；
	// x264 crf26≈0.9725 ⇒ **q:v 45 才与 crf26 同观感**，故默认 45。
	VTQualitySmall    = 35
	VTQualityBalanced = 45
	VTQualityHigh     = 50
	// DefaultVTQuality 是硬件质量优先的默认质量档（q:v 45 = 与 CPU 默认 crf26 同观感）。
	DefaultVTQuality = VTQualityBalanced
)

// Options 是一次规划的全部可选项（纯值，无副作用）。
type Options struct {
	Preset Preset
	// KBps 是用户选的码率（0 = 用档位下限）；仅 ModeBitrate 有意义。
	KBps    int
	Encoder string
	Mode    string
	// Quality 是质量档：CPU=CRF、硬件=-q:v（0 = 用该编码器的默认档）。
	Quality int
	// TwoPass 只有 CPU + 目标码率能用（硬件不支持 2-pass，CRF 本来就是单遍）。
	TwoPass bool
}

// Choice 是编码器/模式下拉的一项（label/hint 由后端给，前端不重复写文案）。
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// QualityChoice 是质量档下拉的一项（Value 是 CRF 或 -q:v 的原始数值）。
type QualityChoice struct {
	Value int    `json:"value"`
	Label string `json:"label"`
}

// ResolveEncoder 把请求里的编码器归一化（空 = 默认；未知值由 ValidateOptions 拦成 400）。
func ResolveEncoder(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case EncoderCPU:
		return EncoderCPU
	case EncoderHardware:
		return EncoderHardware
	default:
		return DefaultEncoder
	}
}

// ResolveMode 把模式归一化（空 = 默认）。
func ResolveMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ModeQuality:
		return ModeQuality
	case ModeBitrate:
		return ModeBitrate
	default:
		return DefaultMode
	}
}

// ResolveQuality 归一化质量档（0 = 该编码器的默认档）。
func ResolveQuality(encoder string, q int) int {
	if q > 0 {
		return q
	}
	if ResolveEncoder(encoder) == EncoderCPU {
		return DefaultCRF
	}
	return DefaultVTQuality
}

// NormalizeOptions 填好所有默认值（老请求不带新字段时行为 = 现在的默认）。
func NormalizeOptions(o Options) Options {
	if _, ok := FindPreset(o.Preset.ID); !ok {
		// 空档位 = 默认档（原始）；未知档位由接口层拦成 400，不会走到这里。
		if p, ok := FindPreset(DefaultPresetID); ok {
			o.Preset = p
		}
	}
	o.Encoder = ResolveEncoder(o.Encoder)
	o.Mode = ResolveMode(o.Mode)
	o.Quality = ResolveQuality(o.Encoder, o.Quality)
	// 2-pass 只能 CPU + 目标码率：非法组合在这里收敛成"不开 2-pass"，
	// 接口层另有一道硬判据（ValidateOptions → 400），面板也会锁死。
	if o.TwoPass && !(o.Encoder == EncoderCPU && o.Mode == ModeBitrate) {
		o.TwoPass = false
	}
	return o
}

// ValidateOptions 校验请求里的选项（空值合法 = 用默认）；非法即 400。
func ValidateOptions(o Options) error {
	enc := strings.ToLower(strings.TrimSpace(o.Encoder))
	if enc != "" && enc != EncoderHardware && enc != EncoderCPU {
		return fmt.Errorf("编码器只能是 hardware 或 cpu")
	}
	mode := strings.ToLower(strings.TrimSpace(o.Mode))
	if mode != "" && mode != ModeBitrate && mode != ModeQuality {
		return fmt.Errorf("模式只能是 bitrate 或 quality")
	}
	if o.Quality != 0 {
		limit := 51 // CRF 的合法上限
		if ResolveEncoder(o.Encoder) == EncoderHardware {
			limit = 100 // -q:v 的合法上限
		}
		if o.Quality < 1 || o.Quality > limit {
			return fmt.Errorf("质量档必须是 1~%d 的整数", limit)
		}
	}
	if o.TwoPass && !(ResolveEncoder(o.Encoder) == EncoderCPU && ResolveMode(o.Mode) == ModeBitrate) {
		return fmt.Errorf("2-pass 只能配合 CPU + 目标码率（硬件与质量优先都不支持）")
	}
	return nil
}

// EncoderChoices 是面板的编码器单选（默认项由 DefaultEncoder 决定）。
//
// 硬件档**只换编码器（HEVC）**：码率/质量设置一个都不动（用户点名"码率不变！"）。
func EncoderChoices() []Choice {
	return []Choice{
		{Value: EncoderCPU, Label: "CPU（x264）",
			Hint: "默认：同码率画质更好、码率控制更准"},
		{Value: EncoderHardware, Label: "硬件加速（HEVC）",
			Hint: "改用 hevc_videotoolbox（更快）；码率设置不变，只有质量档刻度不同"},
	}
}

// ModeChoices 是面板的模式单选（默认项由 DefaultMode 决定 = 质量优先）。
func ModeChoices() []Choice {
	return []Choice{
		{Value: ModeQuality, Label: "CRF（质量优先）",
			Hint: "按画质档压；体积不可预估、可能不小于原文件，届时会跳过并说明"},
		{Value: ModeBitrate, Label: "目标码率（体积可预估）",
			Hint: "按你选的码率压；体积 ≈ 码率×时长，基本能算出来"},
	}
}

// QualityChoices 返回某个编码器的质量档（**方向相反**：CRF 越小越好、-q:v 越大越好）。
func QualityChoices(encoder string) []QualityChoice {
	if ResolveEncoder(encoder) == EncoderCPU {
		return []QualityChoice{
			{CRFHigh, fmt.Sprintf("CRF %d · 高画质（更小数字=更清楚）", CRFHigh)},
			{CRFBalanced, fmt.Sprintf("CRF %d · 均衡（默认）", CRFBalanced)},
			{CRFSmall, fmt.Sprintf("CRF %d · 更小", CRFSmall)},
		}
	}
	return []QualityChoice{
		{VTQualitySmall, fmt.Sprintf("质量档 %d · 更小", VTQualitySmall)},
		{VTQualityBalanced, fmt.Sprintf("质量档 %d · 均衡（默认，≈CRF %d 观感）", VTQualityBalanced, CRFBalanced)},
		{VTQualityHigh, fmt.Sprintf("质量档 %d · 高画质", VTQualityHigh)},
	}
}

// MediaInfo 是原视频的真实信息（由 ffprobe 探得；门禁直接构造它）。
type MediaInfo struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	// DurationSec 是时长（秒），<=0 表示读不到 —— 读不到就不压（无法保证更小）。
	DurationSec float64 `json:"duration_sec"`
	FileBytes   int64   `json:"file_bytes"`
	// VideoKbps 是原视频流码率；VideoEstimated 为 true 表示它是
	// "文件大小×8/时长 − 音频码率"估出来的（ffprobe 没给视频流码率）。
	VideoKbps      int  `json:"video_kbps"`
	VideoEstimated bool `json:"video_estimated"`
	AudioKbps      int  `json:"audio_kbps"`
	HasAudio       bool `json:"has_audio"`
	HasVideo       bool `json:"has_video"`
	// Codec 是视频流编码名（ffprobe 的 codec_name，如 h264/hevc/vp9）；读不到为空。
	Codec string `json:"codec,omitempty"`
}

// Plan 是一个视频文件的压缩计划（同时是 /video-plan 的一行）。
type Plan struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// RelPath 是相对**基准目录**的路径（如 `第1季/01.mkv`）；递归扫描时才有，非递归为空。
	RelPath string `json:"rel_path,omitempty"`
	OutName string `json:"out_name"`
	OutPath string `json:"out_path"`
	// SkipReason 非空表示这个文件不压，且原因要如实展示给用户。
	SkipReason string `json:"skip_reason,omitempty"`

	SourceWidth     int     `json:"source_width"`
	SourceHeight    int     `json:"source_height"`
	SourceVideoKbps int     `json:"source_video_kbps"`
	SourceEstimated bool    `json:"source_estimated"`
	SourceBytes     int64   `json:"source_bytes"`
	DurationSec     float64 `json:"duration_sec"`
	// SourceCodec 是源视频编码名（ffprobe 的 codec_name，如 h264）。
	SourceCodec string `json:"source_codec,omitempty"`
	// SourceCodecLabel 是给用户看的编码名（如 H.264）。
	SourceCodecLabel string `json:"source_codec_label,omitempty"`

	TargetWidth  int `json:"target_width"`
	TargetHeight int `json:"target_height"`
	VideoKbps    int `json:"video_kbps"`
	// SuggestedKbps 是"原始档给出的建议码率"（0 = 不适用/用户手填了码率）：
	// min(该源分辨率的档位建议, 源码率×0.7)。面板据此显示"每个文件用的是哪个建议值"。
	SuggestedKbps int `json:"suggested_kbps,omitempty"`
	// SuggestedFrom 是建议值的依据："rate70"（按原片码率 70%，用户拍板口径）或
	// "res"（按源分辨率的档位下限）。面板把依据写进 title，不占主句。
	SuggestedFrom string `json:"suggested_from,omitempty"`
	// AudioKbps 为 0 且 AudioDisabled 为 true 表示产物不带音轨（-an）。
	AudioKbps     int   `json:"audio_kbps"`
	AudioDisabled bool  `json:"audio_disabled"`
	EstBytes      int64 `json:"est_bytes"`
	// Capped 表示"因原视频码率低而封顶"，这类文件**封顶即跳过**（不再转码）。
	Capped bool   `json:"capped"`
	Note   string `json:"note,omitempty"`

	// PlaceInOutput 表示这一行不转码，但要把源文件**原样放进 output/**：
	// 硬链接优先（同卷、零额外空间），失败退复制。跳过的文件也保证 output/ 是完整一套。
	// "产物已存在"与"文件已不存在"为 false（前者 output 里已经有东西，后者没源可放）。
	PlaceInOutput bool `json:"place_in_output,omitempty"`
	// PlaceName/PlacePath 是原样放入时的目标：**保留源扩展名/容器**
	// （源 a.mkv ⇒ output/a.<档位>.mkv），绝不把 mkv 内容改名成 .mp4（那是谎报格式）。
	PlaceName string `json:"place_name,omitempty"`
	PlacePath string `json:"place_path,omitempty"`

	// 下面这些是"面板选项 → 执行"的同一份记录（执行器不再重算一遍）。
	Encoder string `json:"encoder"`
	// EncoderCodec 是真正传给 ffmpeg 的 -c:v（libx264 / hevc_videotoolbox）：
	// 面板与任务日志直接显示它，用户能看到"硬件档到底用了什么编码器"。
	EncoderCodec string `json:"encoder_codec,omitempty"`
	Mode         string `json:"mode"`
	Quality      int    `json:"quality,omitempty"`
	TwoPass      bool   `json:"two_pass,omitempty"`
	// MaxRateKbps 是给编码器的 -maxrate 上限：目标码率模式下等于目标码率，
	// 质量优先模式下等于 floor(原码率×0.95)（保证不超原片码率）。
	MaxRateKbps int `json:"maxrate_kbps,omitempty"`
	// EstPercent 是预计省下的百分比；EstimateUnknown=true（质量优先）时无意义。
	EstPercent      int  `json:"est_percent,omitempty"`
	EstimateUnknown bool `json:"estimate_unknown,omitempty"`
}

// Runnable 表示这一行真的会被压缩。
func (p Plan) Runnable() bool { return p.SkipReason == "" && p.VideoKbps > 0 }

// DisplayName 是面板与任务日志显示用的名字：递归时带相对目录（`第1季/01.mkv`），
// 否则就是文件名 —— 同名文件在不同子目录里靠它区分。
func (p Plan) DisplayName() string {
	if strings.TrimSpace(p.RelPath) != "" {
		return p.RelPath
	}
	return p.Name
}

// PlanRequest 是一次规划请求（dir 必须已经过白名单校验）。
type PlanRequest struct {
	Dir    string
	OutDir string
	Options
	// Recursive 为 true 时连子目录里的视频一起处理，产物按原目录结构放进 output/。
	Recursive bool
	// Limits 覆盖递归扫描的深度/数量上限（零值 = DefaultScanLimits）。
	Limits ScanLimits
	// Expect 是面板计划表的指纹（文件名 → 源字节数），可空。
	//
	// 非空时规划会**核对两次探测之间文件有没有变**：大小不一致、或计划表里
	// 有而这次扫不到，都如实跳过并说明 —— 绝不静默按旧计划压（见 BuildPlanProgress）。
	Expect map[string]int64
	// Cache 是进程级探测缓存（由 Server 持有并注入，见 ProbeCache）。
	// nil = 不缓存（每次现探）；命中即 0 次 ffprobe，配置变更只做纯函数重算。
	Cache *ProbeCache
	// OnProbe 在真的跑了一次 ffprobe 之后被调用（缓存命中不调用）：
	// 首次扫描的实时进度据此知道"这次到底有没有在探测"。
	OnProbe func()
	// Names 是"只处理这些文件"（当前目录下的**文件名**，不含路径）；空 = 处理全部视频。
	//
	// 用户点名的语义：文件管理器里选了视频就只处理选中的，没选就处理整个目录。
	// 不是视频/不存在的名字**逐条如实标记**（绝不静默忽略）；全不合法则整体报错。
	Names []string
}

// NameReject 是 names 里被拒的一条（带原因，面板要如实展示）。
type NameReject struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ErrNoRequestedVideo 表示 names 里没有任何一个是本目录下的视频（接口层据此 400）。
var ErrNoRequestedVideo = fmt.Errorf("选中的文件都不是本目录下的视频")

// RequestedNamesError 带上逐条原因，便于接口层把"为什么一个都处理不了"说清楚。
type RequestedNamesError struct{ Rejects []NameReject }

func (e *RequestedNamesError) Error() string {
	parts := make([]string, 0, len(e.Rejects))
	for _, r := range e.Rejects {
		parts = append(parts, r.Name+"（"+r.Reason+"）")
	}
	return ErrNoRequestedVideo.Error() + "：" + strings.Join(parts, "、")
}

func (e *RequestedNamesError) Is(target error) bool { return target == ErrNoRequestedVideo }

// NormalizeNames 去掉空项与重复项（顺序保留），并做**路径安全**校验：
// 只允许当前目录下的文件名，带路径分隔符或 `..` 一律拒绝（防越界）。
func NormalizeNames(names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, raw := range names {
		n := strings.TrimSpace(raw)
		if n == "" {
			continue
		}
		if n == "." || n == ".." || strings.ContainsAny(n, `:/\`) || strings.Contains(n, "..") {
			return nil, fmt.Errorf("文件名不合法（只能是本目录下的文件名）：%s", raw)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// CheckRequestedNames 逐个核对 names：可用的（本目录下的视频文件）与逐条拒绝原因。
//
// 只 Stat（不跑 ffprobe）：够回答"这个名字现在还是不是一个视频文件"。
func CheckRequestedNames(dir string, names []string) (usable []string, rejects []NameReject) {
	for _, n := range names {
		if !IsVideoName(n) {
			rejects = append(rejects, NameReject{Name: n, Reason: "不是视频文件"})
			continue
		}
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil || st.IsDir() {
			rejects = append(rejects, NameReject{Name: n, Reason: "文件不存在"})
			continue
		}
		usable = append(usable, n)
	}
	return usable, rejects
}

// PlanResult 是一次规划的结果。
type PlanResult struct {
	Dir      string `json:"dir"`
	OutDir   string `json:"out_dir"`
	Preset   Preset `json:"preset"`
	KBps     int    `json:"kbps"`
	Encoder  string `json:"encoder"`
	Mode     string `json:"mode"`
	Quality  int    `json:"quality"`
	TwoPass  bool   `json:"two_pass"`
	Rows     []Plan `json:"rows"`
	Runnable int    `json:"runnable"`
	Skipped  int    `json:"skipped"`
	// EstBytes 是所有可压行的预计产物合计（质量优先时无意义，见 EstimateUnknown）。
	EstBytes int64 `json:"est_bytes"`
	// TotalSourceBytes 是可压行的原文件体积合计（面板表尾 "原 → 预计" 用）。
	TotalSourceBytes int64 `json:"total_source_bytes"`
	// EstSavedBytes / EstPercent 是预计省下的体积与百分比。
	EstSavedBytes int64 `json:"est_saved_bytes"`
	EstPercent    int   `json:"est_percent"`
	// EstimateUnknown 表示"至少有一行是质量优先，体积不可预估"。
	EstimateUnknown bool `json:"estimate_unknown"`
	// CappedSkipped 是"因原视频码率已到极限而跳过"的行数，
	// 面板据此提醒"不会转码，但会原样放进 output"（判据只在 planBitrates 里）。
	CappedSkipped int `json:"capped_skipped"`
	// PlaceCount 是"不转码但要原样放进 output/"的行数（含码率封顶与其它跳过原因）。
	PlaceCount int `json:"place_count"`
	// Probed 是这一次真正跑了 ffprobe 的次数（缓存命中不算）——
	// 面板与门禁据此如实判断"配置变更有没有又去重探一遍"。
	Probed int `json:"probed"`
	// Names 是这一次"只处理这些文件"（空 = 全部视频）；Rejects 是其中被如实拒掉的条目。
	Names   []string     `json:"names,omitempty"`
	Rejects []NameReject `json:"rejects,omitempty"`
	// Recursive 回显这次是否扫了子目录；ScanSkipped/ScanNotes 是递归扫描的如实说明
	// （跳过了哪些子目录、哪里到了深度/数量上限）。
	Recursive   bool       `json:"recursive,omitempty"`
	ScanSkipped []ScanSkip `json:"scan_skipped,omitempty"`
	ScanNotes   []string   `json:"scan_notes,omitempty"`
}

// OutputDirName 是产物子目录名（写进 <当前目录>/output/）。
const OutputDirName = "output"

// OutputName 是产物文件名：<原名>.<档位>.mp4。
//
// 刻意只带档位、不带码率：码率是"上限"而不是承诺值（会被原码率封顶），
// 写进文件名会撒谎；而只带档位让"同一档位重复点"稳定命中已存在的产物（幂等）。
func OutputName(srcName, presetID string) string {
	base := filepath.Base(srcName)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if strings.TrimSpace(base) == "" {
		base = "video"
	}
	return base + "." + presetID + ".mp4"
}

// PlaceName 是"原样放进 output/"时的文件名：**保留源扩展名/容器**。
//
// 源 a.mkv ⇒ a.<档位>.mkv —— 绝不能把 mkv 内容改名成 .mp4（那是谎报格式）。
func PlaceName(srcName, presetID string) string {
	base := filepath.Base(srcName)
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if strings.TrimSpace(base) == "" {
		base = "video"
	}
	return base + "." + presetID + ext
}

// targetSize 按档位封顶算出目标宽高：绝不放大，宽高都是偶数且都 ≤ 原尺寸。
//
// 横屏（含正方形）按高度封顶、竖屏按宽度封顶；封顶后按比例缩放，
// 向下取偶数（yuv420p 要求偶数）。cap<=0（原始档）表示**不缩放**，只取偶数。
// 返回 0,0 表示这个尺寸不该转码。
func targetSize(srcW, srcH, cap int) (int, int) {
	if srcW < minSide || srcH < minSide {
		return 0, 0
	}
	if cap <= 0 {
		// 原始档：不缩放（只把奇数边向下取到偶数，yuv420p 的硬要求）。
		return evenDown(srcW), evenDown(srcH)
	}
	scale := 1.0
	if srcW >= srcH { // 横屏：按高度封顶
		if srcH > cap {
			scale = float64(cap) / float64(srcH)
		}
	} else { // 竖屏：按宽度封顶
		if srcW > cap {
			scale = float64(cap) / float64(srcW)
		}
	}
	w := evenDown(int(math.Floor(float64(srcW) * scale)))
	h := evenDown(int(math.Floor(float64(srcH) * scale)))
	// 兜底：无论如何不许超过原尺寸（原尺寸是奇数时向下取偶数已经更小）。
	if w > srcW {
		w = evenDown(srcW)
	}
	if h > srcH {
		h = evenDown(srcH)
	}
	if w < 2 || h < 2 {
		return 0, 0
	}
	return w, h
}

// evenDown 向下取到偶数（最小 2）。
func evenDown(n int) int {
	if n%2 != 0 {
		n--
	}
	if n < 2 {
		n = 2
	}
	return n
}

// planBitrates 算出实际使用的视频/音频码率，并把"为什么被压小"讲清楚。
//
// 硬要求（用户点名）：转码码率绝不超过原视频。
//
//	目标码率模式：实际视频码率 = min(用户选的, 原视频视频码率 × 0.95)
//	质量优先模式：不设目标码率，只把 -maxrate 封在原码率×0.95 以内
//	音频码率     = min(96k, 原音频码率)；无音轨则 -an
//
// 原始档没带码率（kbps=0）时：按**这个文件**的源分辨率取档位下限（见 SuggestedKBps），
// 再照旧受原码率×0.95 封顶 —— 绝不"跟随原片"（那等于封顶即跳过、默认什么都不压）。
//
// capped 只表示"实际码率 ≥ 原码率×0.95"（这种文件**封顶即跳过**，
// 不再转码，改为原样放进 output/；判据见 PlanOne）。
// suggested 是"源分辨率建议值"（仅原始档给），面板据此显示每个文件用的建议值。
func planBitrates(info MediaInfo, opts Options) (vKbps, aKbps, maxRate, suggested int, suggestedByRate, capped bool, note, skip string) {
	if info.VideoKbps <= 0 {
		return 0, 0, 0, 0, false, false, "", "读不到原视频码率，无法保证压完更小"
	}
	capKbps := int(math.Floor(float64(info.VideoKbps) * bitrateSafety))
	if capKbps < 1 {
		return 0, 0, 0, 0, false, false, "", "原视频码率太低，没有可压空间"
	}
	maxRate = capKbps
	if opts.Mode == ModeQuality {
		// 质量优先：不承诺体积，但仍用 -maxrate 兜住"绝不超原片码率"。
		vKbps = capKbps
	} else {
		vKbps = ResolveKBps(opts.Preset, opts.KBps)
		if vKbps <= 0 {
			suggested = SuggestedKBps(info.Width, info.Height, info.VideoKbps)
			vKbps = suggested
			suggestedByRate = suggested < tierKBps(info.Width, info.Height)
		}
		if vKbps > capKbps {
			vKbps = capKbps
			capped = true
			if suggested > 0 {
				note = fmt.Sprintf("建议 %d kbps，已被原码率封顶到 %d kbps", suggested, capKbps)
			} else {
				note = fmt.Sprintf("原码率低（%d kbps），已封顶到 %d kbps", info.VideoKbps, capKbps)
			}
		} else if suggested > 0 {
			// 建议值本身要能看出依据（用户点名）：是按源分辨率，还是按原片 70%。
			if suggestedByRate {
				note = fmt.Sprintf("原始档按原片码率 70%% 建议 %d kbps（源 %d）", suggested, info.VideoKbps)
			} else {
				note = fmt.Sprintf("原始档按源分辨率建议 %d kbps", suggested)
			}
		}
		maxRate = vKbps
	}
	if !info.HasAudio {
		return vKbps, 0, maxRate, suggested, suggestedByRate, capped, note, ""
	}
	aKbps = audioMaxKbps
	if info.AudioKbps > 0 && info.AudioKbps < aKbps {
		aKbps = info.AudioKbps
	}
	return vKbps, aKbps, maxRate, suggested, suggestedByRate, capped, note, ""
}

// PlanOne 是**唯一的单文件规划实现**（纯函数：不碰文件系统）。
//
// outExists 由调用方（BuildPlan）传进来，因此门禁可以直接断言各种负向情形。
func PlanOne(name, srcPath, outDir string, info MediaInfo, opts Options, outExists bool) Plan {
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(srcPath)
	}
	opts = NormalizeOptions(opts)
	preset := opts.Preset
	p := Plan{
		Name:             name,
		Path:             srcPath,
		OutName:          OutputName(name, preset.ID),
		PlaceName:        PlaceName(name, preset.ID),
		SourceWidth:      info.Width,
		SourceHeight:     info.Height,
		SourceVideoKbps:  info.VideoKbps,
		SourceEstimated:  info.VideoEstimated,
		SourceBytes:      info.FileBytes,
		DurationSec:      info.DurationSec,
		SourceCodec:      info.Codec,
		SourceCodecLabel: CodecLabel(info.Codec),
		Encoder:          opts.Encoder,
		EncoderCodec:     EncoderCodec(opts.Encoder),
		Mode:             opts.Mode,
		Quality:          opts.Quality,
		TwoPass:          opts.TwoPass,
	}
	if outDir != "" {
		p.OutPath = filepath.Join(outDir, p.OutName)
		p.PlacePath = filepath.Join(outDir, p.PlaceName)
	}

	switch {
	case !info.HasVideo || info.Width <= 0 || info.Height <= 0:
		p.PlaceInOutput = true
		p.SkipReason = "没有视频流（不是视频或文件损坏）"
		return p
	case info.Width < minSide || info.Height < minSide:
		p.PlaceInOutput = true
		p.SkipReason = fmt.Sprintf("分辨率太小（%dx%d）", info.Width, info.Height)
		return p
	case info.DurationSec <= 0:
		p.PlaceInOutput = true
		p.SkipReason = "读不到时长，无法保证压完更小"
		return p
	}

	w, h := targetSize(info.Width, info.Height, preset.CapHeight)
	if w == 0 || h == 0 {
		p.PlaceInOutput = true
		p.SkipReason = "分辨率不可用，无法转码"
		return p
	}
	p.TargetWidth, p.TargetHeight = w, h

	vKbps, aKbps, maxRate, suggested, suggestedByRate, capped, note, skip := planBitrates(info, opts)
	if skip != "" {
		p.PlaceInOutput = true
		p.SkipReason = skip
		return p
	}
	// 建议值（仅原始档）要逐文件记在计划里：面板才看得出"这个文件用了哪个建议值、依据是什么"。
	p.SuggestedKbps = suggested
	if suggested > 0 {
		if suggestedByRate {
			p.SuggestedFrom = "rate70"
		} else {
			p.SuggestedFrom = "res"
		}
	}
	// 封顶即跳过（用户拍板）：实际码率被迫等于原片×0.95 时再压只会更糊、体积几乎不变
	// —— 不转码，改为原样放进 output/（规则不变）。
	if capped {
		p.Capped = true
		p.VideoKbps, p.AudioKbps, p.MaxRateKbps, p.Note = vKbps, aKbps, maxRate, note
		p.AudioDisabled = !info.HasAudio
		p.PlaceInOutput = true
		p.SkipReason = fmt.Sprintf("原视频码率已经很低（%d kbps @ %dx%d），再压只会更糊、体积几乎不变",
			info.VideoKbps, info.Width, info.Height)
		return p
	}
	p.VideoKbps, p.AudioKbps, p.Capped, p.Note = vKbps, aKbps, capped, note
	// 原始档要有一句清楚的语义：不缩放，只可能降码率 / 换编码。
	if IsSourcePreset(preset) && p.Note == "" {
		p.Note = "原始档不缩放（码率按你选的走）"
	}
	p.MaxRateKbps = maxRate
	p.AudioDisabled = !info.HasAudio
	if opts.Mode == ModeQuality {
		// 质量优先：体积由画面复杂程度决定，**不给假数字**。
		p.EstimateUnknown = true
	} else {
		p.EstBytes = estimateBytes(vKbps, aKbps, info.DurationSec)
		p.EstPercent = savePercent(info.FileBytes, p.EstBytes)
	}

	if outExists {
		p.SkipReason = "产物已存在，跳过"
	}
	return p
}

// estimateBytes 估算产物字节数（码率按 kbps=1000bit/s 计）。
func estimateBytes(vKbps, aKbps int, durationSec float64) int64 {
	if durationSec <= 0 {
		return 0
	}
	return int64(math.Round(float64(vKbps+aKbps) * 1000 / 8 * durationSec))
}

// savePercent 算出"预计省下的百分比"（四舍五入；读不到体积返回 0）。
func savePercent(before, after int64) int {
	if before <= 0 || after <= 0 || after >= before {
		return 0
	}
	return int(math.Round(float64(before-after) / float64(before) * 100))
}

// SavedPercent 返回实际省下的百分比（before 是分母）。
//
// before/after 任一 <=0（源 0 字节 / 读不到大小）时 ok=false：调用方据此
// **只写体积、不写百分比**，绝不出现 -Infinity% / NaN%（用户点名的边界）。
func SavedPercent(before, after int64) (float64, bool) {
	if before <= 0 || after <= 0 {
		return 0, false
	}
	return float64(before-after) / float64(before) * 100, true
}

// FormatSavedPercent 是日志与汇总用的百分比文本（如 "-44.6%"）；
// 算不出（源 0 字节 / 读不到大小 / 没变小）时返回空串。
func FormatSavedPercent(before, after int64) string {
	pct, ok := SavedPercent(before, after)
	if !ok || pct <= 0 {
		return ""
	}
	return fmt.Sprintf("-%.1f%%", pct)
}

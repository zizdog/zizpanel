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
}

var presets = []Preset{
	{ID: "360p", Label: "360p", CapHeight: 360, DefaultKbps: 400},
	{ID: "480p", Label: "480p", CapHeight: 480, DefaultKbps: 800},
	{ID: "720p", Label: "720p", CapHeight: 720, DefaultKbps: 1500},
}

// Presets 返回三档预设（副本，调用方改不动包内状态）。
func Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	return out
}

// DefaultPresetID 是默认档位（480p：网络视频最常用的"能看且省流量"档）。
const DefaultPresetID = "480p"

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
}

// BitrateChoices 返回某一档的码率选项：档位下限 / 1.5× / 2×。
//
// 默认用**网络视频能用下限**（360p=400、480p=800、720p=1500 kbps）：
// 用户要的往往是"能发给别人看就行"，更大的码率只是浪费流量。
func BitrateChoices(p Preset) []BitrateChoice {
	base := p.DefaultKbps
	if base <= 0 {
		base = 800
	}
	return []BitrateChoice{
		{KBps: base, Label: fmt.Sprintf("%d kbps · %s 能用下限", base, p.ID)},
		{KBps: base * 3 / 2, Label: fmt.Sprintf("%d kbps · 清晰些", base*3/2)},
		{KBps: base * 2, Label: fmt.Sprintf("%d kbps · 更清晰", base*2)},
	}
}

// ResolveKBps 把请求里的 kbps 归一化成实际要用的值（0 = 用档位下限）。
func ResolveKBps(p Preset, kbps int) int {
	if kbps > 0 {
		return kbps
	}
	if p.DefaultKbps > 0 {
		return p.DefaultKbps
	}
	return 800
}

const (
	// MinKBps / MaxKBps 是用户可选码率的合法区间（超出即 400，不静默钳制）。
	MinKBps = 100
	MaxKBps = 20000
	// audioMaxKbps 是音频码率上限；原音轨更低时取原值。
	audioMaxKbps = 96
	// bitrateSafety 是"绝不越压越大"的安全系数：实际视频码率还要再乘它。
	bitrateSafety = 0.95
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
	//（瓶颈在解码+缩放），但**同码率 CPU 画质明显更好** ⇒ 默认用 CPU。
	DefaultEncoder = EncoderCPU
	DefaultMode    = ModeBitrate

	// CPU 质量优先 = libx264 -crf N：**越小画质越好、文件越大**。
	// 实测：CRF 26 → 81.9MB / 957kbps / SSIM 0.9737（省 37%）；
	// CRF 24 → 100.8MB / 1177kbps（只省 23%）⇒ 默认 26。
	CRFHigh     = 24
	CRFBalanced = 26
	CRFSmall    = 28
	// DefaultCRF 是 CPU 质量优先的默认 CRF。
	DefaultCRF = CRFBalanced

	// VideoToolbox 质量优先 = h264_videotoolbox -q:v N：**越大画质越好、文件越大**
	//（方向与 CRF 相反）。实测同源：q:v 40 → 105.4MB / 1231kbps / SSIM 0.9699；
	// q:v 70 → **414.8MB / 4842kbps（是源的 3 倍，会被"压不小"兜底删掉）**。
	VTQualitySmall    = 40
	VTQualityBalanced = 55
	VTQualityHigh     = 70
	// DefaultVTQuality 是硬件质量优先的默认质量档（实测 q:v 40 体积最接近源、最省）。
	DefaultVTQuality = VTQualitySmall
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
// 硬件留给"1080p/4K 源或想省电"；480p 实测两者耗时接近（22s vs 25s）而画质 CPU 更好。
func EncoderChoices() []Choice {
	return []Choice{
		{Value: EncoderHardware, Label: "硬件加速（VideoToolbox）",
			Hint: "快一些（高清源更明显）；同码率画质略逊于 CPU（实测 SSIM 0.95 vs 0.97）"},
		{Value: EncoderCPU, Label: "CPU（x264）",
			Hint: "同码率画质更好、码率控制更准（实测 480p 不比硬件慢）"},
	}
}

// ModeChoices 是面板的模式单选。
func ModeChoices() []Choice {
	return []Choice{
		{Value: ModeBitrate, Label: "目标码率（体积可预估）",
			Hint: "按你选的码率压；体积 ≈ 码率×时长，基本能算出来"},
		{Value: ModeQuality, Label: "质量优先（体积不可预估，通常明显更小）",
			Hint: "按画质档压；体积看画面复杂程度，算不出来"},
	}
}

// QualityChoices 返回某个编码器的质量档（**方向相反**：CRF 越小越好、-q:v 越大越好）。
func QualityChoices(encoder string) []QualityChoice {
	if ResolveEncoder(encoder) == EncoderCPU {
		return []QualityChoice{
			{CRFHigh, fmt.Sprintf("CRF %d · 高画质", CRFHigh)},
			{CRFBalanced, fmt.Sprintf("CRF %d · 均衡（默认）", CRFBalanced)},
			{CRFSmall, fmt.Sprintf("CRF %d · 更小", CRFSmall)},
		}
	}
	return []QualityChoice{
		{VTQualitySmall, fmt.Sprintf("质量档 %d · 更小（默认）", VTQualitySmall)},
		{VTQualityBalanced, fmt.Sprintf("质量档 %d · 均衡", VTQualityBalanced)},
		{VTQualityHigh, fmt.Sprintf("质量档 %d · 高画质（越大越好，实测标定）", VTQualityHigh)},
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
	Name    string `json:"name"`
	Path    string `json:"path"`
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
	// AudioKbps 为 0 且 AudioDisabled 为 true 表示产物不带音轨（-an）。
	AudioKbps     int   `json:"audio_kbps"`
	AudioDisabled bool  `json:"audio_disabled"`
	EstBytes      int64 `json:"est_bytes"`
	// Capped 表示"因原视频码率低而封顶"（面板必须讲清楚，见 Note）。
	Capped bool   `json:"capped"`
	Note   string `json:"note,omitempty"`

	// 下面这些是"面板选项 → 执行"的同一份记录（执行器不再重算一遍）。
	Encoder string `json:"encoder"`
	Mode    string `json:"mode"`
	Quality int    `json:"quality,omitempty"`
	TwoPass bool   `json:"two_pass,omitempty"`
	// MaxRateKbps 是给编码器的 -maxrate 上限：目标码率模式下等于目标码率，
	// 质量优先模式下等于 floor(原码率×0.95)（保证不超原片码率）。
	MaxRateKbps int `json:"maxrate_kbps,omitempty"`
	// EstPercent 是预计省下的百分比；EstimateUnknown=true（质量优先）时无意义。
	EstPercent      int  `json:"est_percent,omitempty"`
	EstimateUnknown bool `json:"estimate_unknown,omitempty"`
}

// Runnable 表示这一行真的会被压缩。
func (p Plan) Runnable() bool { return p.SkipReason == "" && p.VideoKbps > 0 }

// PlanRequest 是一次规划请求（dir 必须已经过白名单校验）。
type PlanRequest struct {
	Dir    string
	OutDir string
	Options
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
	// CappedRunnable / AllCapped 是"用户选的码率 ≥ 原码率×0.95"的行数，
	// 面板据此给醒目警告（判据只在 planBitrates 里，不在前端重复实现）。
	CappedRunnable int  `json:"capped_runnable"`
	AllCapped      bool `json:"all_capped"`
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

// targetSize 按档位封顶算出目标宽高：绝不放大，宽高都是偶数且都 ≤ 原尺寸。
//
// 横屏（含正方形）按高度封顶、竖屏按宽度封顶；封顶后按比例缩放，
// 向下取偶数（yuv420p 要求偶数）。返回 0,0 表示这个尺寸不该转码。
func targetSize(srcW, srcH, cap int) (int, int) {
	if srcW < minSide || srcH < minSide || cap <= 0 {
		return 0, 0
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
// capped 只表示"用户选的码率 ≥ 原码率×0.95"（面板据此警告"这样压基本不会变小"）。
func planBitrates(info MediaInfo, opts Options) (vKbps, aKbps, maxRate int, capped bool, note, skip string) {
	if info.VideoKbps <= 0 {
		return 0, 0, 0, false, "", "读不到原视频码率，无法保证压完更小"
	}
	capKbps := int(math.Floor(float64(info.VideoKbps) * bitrateSafety))
	if capKbps < 1 {
		return 0, 0, 0, false, "", "原视频码率太低，没有可压空间"
	}
	maxRate = capKbps
	if opts.Mode == ModeQuality {
		// 质量优先：不承诺体积，但仍用 -maxrate 兜住"绝不超原片码率"。
		vKbps = capKbps
	} else {
		vKbps = ResolveKBps(opts.Preset, opts.KBps)
		if vKbps <= 0 || vKbps > capKbps {
			vKbps = capKbps
			capped = true
			note = fmt.Sprintf("原码率低（%d kbps），已封顶到 %d kbps", info.VideoKbps, capKbps)
		}
		maxRate = vKbps
	}
	if !info.HasAudio {
		return vKbps, 0, maxRate, capped, note, ""
	}
	aKbps = audioMaxKbps
	if info.AudioKbps > 0 && info.AudioKbps < aKbps {
		aKbps = info.AudioKbps
	}
	return vKbps, aKbps, maxRate, capped, note, ""
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
		SourceWidth:      info.Width,
		SourceHeight:     info.Height,
		SourceVideoKbps:  info.VideoKbps,
		SourceEstimated:  info.VideoEstimated,
		SourceBytes:      info.FileBytes,
		DurationSec:      info.DurationSec,
		SourceCodec:      info.Codec,
		SourceCodecLabel: CodecLabel(info.Codec),
		Encoder:          opts.Encoder,
		Mode:             opts.Mode,
		Quality:          opts.Quality,
		TwoPass:          opts.TwoPass,
	}
	if outDir != "" {
		p.OutPath = filepath.Join(outDir, p.OutName)
	}

	switch {
	case !info.HasVideo || info.Width <= 0 || info.Height <= 0:
		p.SkipReason = "没有视频流（不是视频或文件损坏）"
		return p
	case info.Width < minSide || info.Height < minSide:
		p.SkipReason = fmt.Sprintf("分辨率太小（%dx%d）", info.Width, info.Height)
		return p
	case info.DurationSec <= 0:
		p.SkipReason = "读不到时长，无法保证压完更小"
		return p
	}

	w, h := targetSize(info.Width, info.Height, preset.CapHeight)
	if w == 0 || h == 0 {
		p.SkipReason = "分辨率不可用，无法转码"
		return p
	}
	p.TargetWidth, p.TargetHeight = w, h

	vKbps, aKbps, maxRate, capped, note, skip := planBitrates(info, opts)
	if skip != "" {
		p.SkipReason = skip
		return p
	}
	p.VideoKbps, p.AudioKbps, p.Capped, p.Note = vKbps, aKbps, capped, note
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

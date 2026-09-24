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
}

// Runnable 表示这一行真的会被压缩。
func (p Plan) Runnable() bool { return p.SkipReason == "" && p.VideoKbps > 0 }

// PlanRequest 是一次规划请求（dir 必须已经过白名单校验）。
type PlanRequest struct {
	Dir    string
	OutDir string
	Preset Preset
	// KBps 是用户选的码率（0 = 用档位下限）。
	KBps int
}

// PlanResult 是一次规划的结果。
type PlanResult struct {
	Dir      string `json:"dir"`
	OutDir   string `json:"out_dir"`
	Preset   Preset `json:"preset"`
	KBps     int    `json:"kbps"`
	Rows     []Plan `json:"rows"`
	Runnable int    `json:"runnable"`
	Skipped  int    `json:"skipped"`
	// EstBytes 是所有可压行的预计产物合计。
	EstBytes int64 `json:"est_bytes"`
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
//	实际视频码率 = min(用户选的, 原视频视频码率 × 0.95)
//	音频码率     = min(96k, 原音频码率)；无音轨则 -an
func planBitrates(info MediaInfo, wantKbps int) (vKbps, aKbps int, capped bool, note, skip string) {
	if info.VideoKbps <= 0 {
		return 0, 0, false, "", "读不到原视频码率，无法保证压完更小"
	}
	capKbps := int(math.Floor(float64(info.VideoKbps) * bitrateSafety))
	if capKbps < 1 {
		return 0, 0, false, "", "原视频码率太低，没有可压空间"
	}
	vKbps = wantKbps
	if vKbps <= 0 || vKbps > capKbps {
		vKbps = capKbps
		capped = true
		note = fmt.Sprintf("原码率低（%d kbps），已封顶到 %d kbps", info.VideoKbps, capKbps)
	}
	if !info.HasAudio {
		return vKbps, 0, capped, note, ""
	}
	aKbps = audioMaxKbps
	if info.AudioKbps > 0 && info.AudioKbps < aKbps {
		aKbps = info.AudioKbps
	}
	return vKbps, aKbps, capped, note, ""
}

// PlanOne 是**唯一的单文件规划实现**（纯函数：不碰文件系统）。
//
// outExists 由调用方（BuildPlan）传进来，因此门禁可以直接断言各种负向情形。
func PlanOne(name, srcPath, outDir string, info MediaInfo, preset Preset, wantKbps int, outExists bool) Plan {
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(srcPath)
	}
	// 0 = 没指定：用档位下限（PlanOne 自己是完整的，不依赖调用方先归一化）。
	wantKbps = ResolveKBps(preset, wantKbps)
	p := Plan{
		Name:            name,
		Path:            srcPath,
		OutName:         OutputName(name, preset.ID),
		SourceWidth:     info.Width,
		SourceHeight:    info.Height,
		SourceVideoKbps: info.VideoKbps,
		SourceEstimated: info.VideoEstimated,
		SourceBytes:     info.FileBytes,
		DurationSec:     info.DurationSec,
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

	vKbps, aKbps, capped, note, skip := planBitrates(info, wantKbps)
	if skip != "" {
		p.SkipReason = skip
		return p
	}
	p.VideoKbps, p.AudioKbps, p.Capped, p.Note = vKbps, aKbps, capped, note
	p.AudioDisabled = !info.HasAudio
	p.EstBytes = estimateBytes(vKbps, aKbps, info.DurationSec)

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

package videoopt

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// videoExts 是"当成视频处理"的扩展名白名单。
//
// 判据只用于**挑选候选**（非视频一律不碰）；真实内容仍由 ffprobe 判定 ——
// 扩展名骗人时以内容为准（读不出视频流就如实跳过）。
var videoExts = map[string]bool{
	".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".avi": true,
	".webm": true, ".flv": true, ".wmv": true, ".mpg": true, ".mpeg": true,
	".ts": true, ".m2ts": true, ".mts": true, ".3gp": true, ".rmvb": true,
	".rm": true, ".vob": true, ".ogv": true, ".asf": true, ".f4v": true,
}

// IsVideoName 判断文件名是否可能是视频（只看扩展名）。
func IsVideoName(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
}

// VideoExts 返回候选视频扩展名（升序副本）。
//
// 面板前端要按**同一口径**判断"选中的是不是视频"（只处理选中的视频），
// 走这个接口取清单，绝不两边各写一份（门禁会比对前端那份有没有走样）。
func VideoExts() []string {
	out := make([]string, 0, len(videoExts))
	for ext := range videoExts {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// probeJSON 是 ffprobe -print_format json 的响应形状（只取用得到的字段）。
type probeJSON struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		BitRate   string `json:"bit_rate"`
		Duration  string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
		Size     string `json:"size"`
	} `json:"format"`
}

// ParseProbeJSON 把 ffprobe 的输出转成 MediaInfo（纯函数，门禁可以直接喂 JSON）。
//
// 视频流码率拿不到时的估算口径（用户点名的兜底）：
//
//	总码率（format.bit_rate，或 文件大小×8/时长）− 音频码率
//
// 并在 VideoEstimated 上如实标记"这是估算"，面板会把它显示出来。
func ParseProbeJSON(raw []byte, fileBytes int64) (MediaInfo, error) {
	var p probeJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		return MediaInfo{}, fmt.Errorf("ffprobe 输出不是合法 JSON: %w", err)
	}
	info := MediaInfo{FileBytes: fileBytes}
	if info.FileBytes <= 0 {
		if n, err := strconv.ParseInt(strings.TrimSpace(p.Format.Size), 10, 64); err == nil {
			info.FileBytes = n
		}
	}

	totalKbps := kbpsFromBits(p.Format.BitRate)
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if !info.HasVideo {
				info.HasVideo = true
				info.Width, info.Height = s.Width, s.Height
				info.VideoKbps = kbpsFromBits(s.BitRate)
				info.Codec = s.CodecName
			}
		case "audio":
			info.HasAudio = true
			info.AudioKbps += kbpsFromBits(s.BitRate)
		}
	}

	// 时长：容器优先，其次视频流。
	info.DurationSec = parseSeconds(p.Format.Duration)
	if info.DurationSec <= 0 {
		for _, s := range p.Streams {
			if s.CodecType == "video" {
				if d := parseSeconds(s.Duration); d > 0 {
					info.DurationSec = d
					break
				}
			}
		}
	}

	// 视频流码率缺失 ⇒ 用"总码率 − 音频码率"估算（并如实标记）。
	if info.VideoKbps <= 0 {
		if totalKbps <= 0 && info.FileBytes > 0 && info.DurationSec > 0 {
			totalKbps = int(info.FileBytes * 8 / 1000 / int64(info.DurationSec))
		}
		if totalKbps > 0 {
			v := totalKbps - info.AudioKbps
			if v > 0 {
				info.VideoKbps = v
				info.VideoEstimated = true
			}
		}
	}
	return info, nil
}

// kbpsFromBits 把 ffprobe 的 bit_rate（bit/s 字符串）转成 kbps；"N/A"/空值算 0。
func kbpsFromBits(s string) int {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return int(n / 1000)
}

// codecLabels 是 ffprobe 的 codec_name → 面板显示名（没有就原样大写）。
var codecLabels = map[string]string{
	"h264": "H.264", "hevc": "H.265/HEVC", "vp9": "VP9", "av1": "AV1",
	"vp8": "VP8", "mpeg4": "MPEG-4", "mpeg2video": "MPEG-2", "prores": "ProRes",
	"theora": "Theora", "wmv3": "WMV3", "vc1": "VC-1",
}

// CodecLabel 把 codec_name 变成给用户看的一行文字（读不到返回空串）。
func CodecLabel(codec string) string {
	c := strings.ToLower(strings.TrimSpace(codec))
	if c == "" {
		return ""
	}
	if l, ok := codecLabels[c]; ok {
		return l
	}
	return strings.ToUpper(c)
}

// parseSeconds 解析 ffprobe 的时长字符串（"N/A" 或空值算 0）。
func parseSeconds(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return f
}

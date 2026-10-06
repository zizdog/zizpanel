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
		// RFrameRate/AvgFrameRate/PixFmt 决定"要不要硬解"与"降到 30fps"（见 needFastDecode）。
		RFrameRate   string `json:"r_frame_rate"`
		AvgFrameRate string `json:"avg_frame_rate"`
		PixFmt       string `json:"pix_fmt"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
		Size     string `json:"size"`
	} `json:"format"`
}

// isBitmapSubtitleCodec：图形（位图）字幕在 mp4 里放不下，必须与文本字幕区别对待。
//
// 只列**确定是位图**的编码；认不出来的一律当文本字幕（让 ffmpeg 转 mov_text）。
// 宁可转码当场报错，也绝不把字幕悄悄丢掉（用户 2026-10-06 的报障就是这么来的）。
func isBitmapSubtitleCodec(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "xsub", "dvb_teletext":
		return true
	}
	return false
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
	audioStreams, subStreams := 0, 0
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if !info.HasVideo {
				info.HasVideo = true
				info.Width, info.Height = s.Width, s.Height
				info.VideoKbps = kbpsFromBits(s.BitRate)
				info.Codec = s.CodecName
				info.FPS = parseRate(s.RFrameRate)
				if info.FPS <= 0 {
					info.FPS = parseRate(s.AvgFrameRate)
				}
				info.BitDepth = bitDepthFromPixFmt(s.PixFmt)
			}
		case "audio":
			audioStreams++
			info.HasAudio = true
			info.AudioKbps += kbpsFromBits(s.BitRate)
		case "subtitle":
			// 字幕按"字幕流内序号"编号（-map 0:s:N 用的就是它），逐条分类。
			if isBitmapSubtitleCodec(s.CodecName) {
				info.BitmapSubtitles++
			} else {
				info.TextSubtitleIndexes = append(info.TextSubtitleIndexes, subStreams)
			}
			subStreams++
		}
	}
	info.AudioStreams = audioStreams

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

// parseRate 解析 ffprobe 的帧率字符串（"60/1" → 60；"0/0"/"N/A" → 0 = 读不到）。
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0
	}
	n, err1 := strconv.ParseFloat(strings.TrimSpace(num), 64)
	d, err2 := strconv.ParseFloat(strings.TrimSpace(den), 64)
	if err1 != nil || err2 != nil || d == 0 || n <= 0 {
		return 0
	}
	return n / d
}

// bitDepthFromPixFmt 只认能确定的位深：8 / 10；认不出返回 0（=不猜，走不依赖位深的老路）。
//
// 为什么要它：全 GPU 快路的 hwdownload 必须指名软件格式（10bit 的 VT 帧是 p010le、
// 8bit 是 nv12，猜错直接 -22）。nv12/nv16 里带"12/16"但不是位深，所以不能按子串扫。
func bitDepthFromPixFmt(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	tenBit := []string{
		"yuv420p10le", "yuv420p10be", "yuv422p10le", "yuv422p10be",
		"yuv444p10le", "yuv444p10be", "p010le", "p010be", "p210le", "p210be",
		"p410le", "p410be", "x2rgb10le", "x2bgr10le", "gray10le", "gray10be",
	}
	for _, p := range tenBit {
		if s == p {
			return 10
		}
	}
	eightBit := []string{
		"yuv420p", "yuvj420p", "yuv422p", "yuvj422p", "yuv444p", "yuvj444p",
		"nv12", "nv21", "nv16", "nv24", "nv42", "yuyv422", "uyvy422", "gray",
	}
	for _, p := range eightBit {
		if s == p {
			return 8
		}
	}
	return 0
}

// parseSeconds 解析 ffprobe 的时长字符串（"N/A" 或空值算 0）。
func parseSeconds(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return f
}

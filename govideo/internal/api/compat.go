package api

import (
	"strconv"
	"strings"

	"github.com/zizdog/govideo/internal/domain"
)

type uaInfo struct {
	family string
	major  int
}

// parseUA classifies a User-Agent into a coarse family. The server owns this
// decision so the frontend never guesses from codec strings.
func parseUA(ua string) uaInfo {
	lower := strings.ToLower(ua)
	pick := func(token string) int {
		idx := strings.Index(lower, token)
		if idx < 0 {
			return -1
		}
		rest := lower[idx+len(token):]
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0
		}
		n, err := strconv.Atoi(rest[:end])
		if err != nil {
			return 0
		}
		return n
	}
	switch {
	case strings.Contains(lower, "edg/") || strings.Contains(lower, "edge/"):
		return uaInfo{"edge", pick("edg/")}
	case strings.Contains(lower, "opr/") || strings.Contains(lower, "opera"):
		return uaInfo{"opera", pick("opr/")}
	case strings.Contains(lower, "fxios"):
		return uaInfo{"firefox", pick("fxios/")}
	case strings.Contains(lower, "crios"):
		return uaInfo{"chrome", pick("crios/")}
	case strings.Contains(lower, "firefox/"):
		return uaInfo{"firefox", pick("firefox/")}
	case strings.Contains(lower, "headlesschrome/"):
		return uaInfo{"chrome", pick("headlesschrome/")}
	case strings.Contains(lower, "chromium/"):
		return uaInfo{"chrome", pick("chromium/")}
	case strings.Contains(lower, "chrome/"):
		return uaInfo{"chrome", pick("chrome/")}
	case strings.Contains(lower, "safari/"):
		return uaInfo{"safari", pick("version/")}
	}
	return uaInfo{"other", 0}
}

type compatibility struct {
	Direct bool   `json:"direct"`
	Reason string `json:"reason"`
}

// evaluateCompat decides direct playback from the probed codec plus a coarse UA
// family table. Phase1 never transcodes, so a false here is a truthful warning.
func evaluateCompat(codecs domain.Codecs, ua uaInfo) compatibility {
	video := strings.ToLower(codecs.Video)
	switch video {
	case "h264", "avc1", "mpeg4", "vp8":
		return compatibility{Direct: true}
	case "hevc", "h265":
		switch ua.family {
		case "safari":
			return compatibility{Direct: true}
		case "chrome", "edge", "opera":
			if ua.major >= 107 {
				return compatibility{Direct: true}
			}
		}
		return compatibility{Direct: false, Reason: "该浏览器放不了 HEVC，请用 Safari（转码属 Phase2）"}
	case "av1":
		switch ua.family {
		case "chrome", "edge", "opera":
			if ua.major >= 70 {
				return compatibility{Direct: true}
			}
		case "firefox":
			if ua.major >= 67 {
				return compatibility{Direct: true}
			}
		case "safari":
			if ua.major >= 17 {
				return compatibility{Direct: true}
			}
		}
		return compatibility{Direct: false, Reason: "该浏览器放不了 AV1，请换新版浏览器（转码属 Phase2）"}
	case "vp9":
		if ua.family == "safari" && ua.major < 14 {
			return compatibility{Direct: false, Reason: "该浏览器放不了 VP9，请换新版浏览器（转码属 Phase2）"}
		}
		return compatibility{Direct: true}
	case "":
		return compatibility{Direct: false, Reason: "尚未探测到编码信息，请等扫描完成"}
	default:
		return compatibility{Direct: false, Reason: "这个编码暂无直出支持，转码属 Phase2"}
	}
}

// audioProblem reports an audio codec no browser in the target set accepts.
func audioProblem(codecs domain.Codecs) string {
	switch strings.ToLower(codecs.Audio) {
	case "", "aac", "mp3", "opus", "vorbis", "flac", "ac3", "eac3", "alac", "pcm_s16le":
		return ""
	default:
		return "音轨编码可能无法播放"
	}
}

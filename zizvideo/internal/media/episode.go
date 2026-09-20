package media

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 补丁 R1：从文件名推导季/集号。识别不到一律返回空，绝不用年份/分辨率/编码数字冒充集号。

// EpisodeNumbers is one filename's derived numbers. HasSeason distinguishes
// "S01E02"（显示 S1E2）from "第03集"（显示 第 3 集，库里 season 留空）。
type EpisodeNumbers struct {
	Season    int
	HasSeason bool
	Episode   int
}

var (
	// 1) SxxExx / Sxx.EPxx / Sxx-Exx
	reSeasonEpisode = regexp.MustCompile(`(?i)s(\d{1,2})[\s._-]*e(?:p)?(\d{1,4})`)
	// 2) EPxx / Exx（须是独立词，避免 1080p / x265 之类）
	reEpisode = regexp.MustCompile(`(?i)(?:^|[^0-9a-z])e(?:p)?(\d{1,4})(?:[^0-9]|$)`)
	// 3) 第xx集 / 第xx話 / 第xx话（阿拉伯或中文数字，允许字间空格）
	reChineseEpisode = regexp.MustCompile(`第\s*([0-9零一二三四五六七八九十百两\s]{1,12}?)\s*[集话話]`)
	// 4) 尾部独立数字
	reDigitRun = regexp.MustCompile(`\d+`)
	// 编码前缀：x.264 / h.265 / hvc1 之类的数字不是集号
	reCodecPrefix = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:x|h|avc|hevc|hvc|av1|aac|dts|ac3|eac3)[\s._-]*$`)
	reDiscPrefix  = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])cd[\s._-]*$`)
)

// 分辨率裸数字（带 p/i 后缀时本就不会被当成独立数字）。
var bareResolutions = map[int]bool{480: true, 576: true, 720: true, 1080: true, 1440: true, 2160: true}

// ParseEpisode derives the episode number from a file name. ok=false means the
// name carries no trustworthy episode marker — the caller must show 未识别.
func ParseEpisode(name string) (EpisodeNumbers, bool) {
	stem := strings.TrimSuffix(filepath.Base(name), filepath.Ext(filepath.Base(name)))
	if stem == "" {
		return EpisodeNumbers{}, false
	}

	if m := reSeasonEpisode.FindStringSubmatch(stem); m != nil {
		season, err1 := strconv.Atoi(m[1])
		episode, err2 := strconv.Atoi(m[2])
		if err1 == nil && err2 == nil {
			return EpisodeNumbers{Season: season, HasSeason: true, Episode: episode}, true
		}
	}
	if m := reEpisode.FindStringSubmatch(stem); m != nil {
		if episode, err := strconv.Atoi(m[1]); err == nil {
			return EpisodeNumbers{Episode: episode}, true
		}
	}
	if m := reChineseEpisode.FindStringSubmatch(stem); m != nil {
		if episode, ok := parseNumberToken(m[1]); ok {
			return EpisodeNumbers{Episode: episode}, true
		}
	}
	if episode, ok := trailingNumber(stem); ok {
		return EpisodeNumbers{Episode: episode}, true
	}
	return EpisodeNumbers{}, false
}

// trailingNumber finds the last standalone digit run and rejects the numbers R1
// names as non-episodes: resolution, year, codec and disc markers.
func trailingNumber(stem string) (int, bool) {
	runs := reDigitRun.FindAllStringIndex(stem, -1)
	for i := len(runs) - 1; i >= 0; i-- {
		start, end := runs[i][0], runs[i][1]
		if start > 0 && isASCIILetter(stem[start-1]) {
			continue // x264 / h265 / 10bit 的一部分
		}
		if end < len(stem) && isASCIILetter(stem[end]) {
			continue // 1080p / 720p 的一部分
		}
		value, err := strconv.Atoi(stem[start:end])
		if err != nil {
			continue
		}
		if bareResolutions[value] || isYear(value) {
			continue // 分辨率或年份，不是集号
		}
		if reCodecPrefix.MatchString(stem[:start]) || reDiscPrefix.MatchString(stem[:start]) {
			continue // x.264 / CD1
		}
		return value, true
	}
	return 0, false
}

// isYear treats every 19xx/20xx as a year. R1 only requires the exclusion when a
// resolution/codec marker follows, but a lone 4-digit 19xx/20xx is far more
// likely a year than an episode, and guessing is forbidden.
func isYear(v int) bool {
	return (v >= 1900 && v <= 2099)
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// parseNumberToken reads an Arabic or Chinese numeral used in 第xx集.
func parseNumberToken(raw string) (int, bool) {
	token := strings.TrimSpace(strings.ReplaceAll(raw, " ", ""))
	if token == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(token); err == nil {
		return n, n >= 0
	}
	digits := map[rune]int{'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
		'五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	if idx := strings.IndexRune(token, '百'); idx >= 0 {
		head := token[:idx]
		hundreds := 1
		if head != "" {
			d, ok := digits[[]rune(head)[0]]
			if !ok {
				return 0, false
			}
			hundreds = d
		}
		rest := token[idx+len("百"):]
		if rest == "" {
			return hundreds * 100, true
		}
		tail, ok := parseNumberToken(rest)
		if !ok {
			return 0, false
		}
		return hundreds*100 + tail, true
	}
	if idx := strings.IndexRune(token, '十'); idx >= 0 {
		head := token[:idx]
		tens := 1
		if head != "" {
			d, ok := digits[[]rune(head)[0]]
			if !ok {
				return 0, false
			}
			tens = d
		}
		rest := token[idx+len("十"):]
		ones := 0
		if rest != "" {
			d, ok := digits[[]rune(rest)[0]]
			if !ok {
				return 0, false
			}
			ones = d
		}
		return tens*10 + ones, true
	}
	if len([]rune(token)) == 1 {
		d, ok := digits[[]rune(token)[0]]
		return d, ok
	}
	n := 0
	for _, r := range token {
		d, ok := digits[r]
		if !ok {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

// EpisodeLabel renders 未识别 / 第 3 集 / S1E3 for the wire.
func EpisodeLabel(season, episode *int) string {
	if episode == nil {
		return "未识别"
	}
	if season != nil {
		return fmt.Sprintf("S%dE%d", *season, *episode)
	}
	return fmt.Sprintf("第 %d 集", *episode)
}

// EpisodePointers adapts EpisodeNumbers to the nullable DB columns.
func EpisodePointers(n EpisodeNumbers) (*int, *int) {
	episode := n.Episode
	if !n.HasSeason {
		return nil, &episode
	}
	season := n.Season
	return &season, &episode
}

// OrderKey is one series member as the playback order sees it.
type OrderKey struct {
	Season   *int
	Episode  *int
	Manual   bool
	Position int
	Name     string
}

// SortOrderKeys returns the playback order as indices into keys: recognized
// episodes by season→episode then name-natural; 未识别永远排在末尾（R1 不许塞进
// 中间冒充有序）。任一行被手动排序过时，同一层内按手动 position 排 —— 手动是
// 权威，自动识别不覆盖它。
func SortOrderKeys(keys []OrderKey) []int {
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	manualAny := false
	for _, k := range keys {
		if k.Manual {
			manualAny = true
			break
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := keys[order[i]], keys[order[j]]
		ta, tb := episodeTier(a), episodeTier(b)
		if ta != tb {
			return ta < tb
		}
		if manualAny {
			if a.Position != b.Position {
				return a.Position < b.Position
			}
			return NaturalLess(a.Name, b.Name)
		}
		if ta == 0 {
			sa, sb := seasonOf(a), seasonOf(b)
			if sa != sb {
				return sa < sb
			}
			if *a.Episode != *b.Episode {
				return *a.Episode < *b.Episode
			}
		}
		if a.Name != b.Name {
			return NaturalLess(a.Name, b.Name)
		}
		return a.Position < b.Position
	})
	return order
}

func episodeTier(k OrderKey) int {
	if k.Episode == nil {
		return 1
	}
	return 0
}

func seasonOf(k OrderKey) int {
	if k.Season == nil {
		return 1
	}
	return *k.Season
}

// NaturalLess is the natural-order comparison used for 文件名排序（02 < 10）.
func NaturalLess(a, b string) bool {
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		da, db := isASCIIDigit(a[ai]), isASCIIDigit(b[bi])
		if da && db {
			za, zb := ai, bi
			for za < len(a) && a[za] == '0' {
				za++
			}
			for zb < len(b) && b[zb] == '0' {
				zb++
			}
			ea, eb := za, zb
			for ea < len(a) && isASCIIDigit(a[ea]) {
				ea++
			}
			for eb < len(b) && isASCIIDigit(b[eb]) {
				eb++
			}
			na, nb := a[za:ea], b[zb:eb]
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			if za-ai != zb-bi {
				return za-ai < zb-bi
			}
			ai, bi = ea, eb
			continue
		}
		la, lb := lowerASCII(a[ai]), lowerASCII(b[bi])
		if la != lb {
			return la < lb
		}
		ai++
		bi++
	}
	return len(a)-ai < len(b)-bi
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

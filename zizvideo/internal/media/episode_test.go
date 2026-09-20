package media_test

import (
	"testing"

	"github.com/zizdog/zizvideo/internal/media"
)

func intPtr(v int) *int { return &v }

// parseCase is one real-world filename. R1: 解析不出来就返回空，绝不拿年份/分辨率冒充集号。
type parseCase struct {
	name    string
	ok      bool
	season  *int // nil = 文件名没写季
	episode *int
}

var parseCases = []parseCase{
	// 1) SxxExx / Sxx.EPxx
	{"剧名.S01E02.mp4", true, intPtr(1), intPtr(2)},
	{"剧名.S01E02.1080p.x265.mkv", true, intPtr(1), intPtr(2)},
	{"Show.S2.EP10.720p.mp4", true, intPtr(2), intPtr(10)},
	{"Show.S01.E02.mkv", true, intPtr(1), intPtr(2)},
	{"Show.S1E2.mp4", true, intPtr(1), intPtr(2)},
	{"剧名.S01E02.第03集.mp4", true, intPtr(1), intPtr(2)}, // 优先级 1 压过 3

	// 2) EPxx / Exx
	{"Show.EP4.mp4", true, nil, intPtr(4)},
	{"Show.E04.mkv", true, nil, intPtr(4)},
	{"Show.EP12.1080p.x264.mkv", true, nil, intPtr(12)},
	{"Show.ep07.mp4", true, nil, intPtr(7)},

	// 3) 第xx集 / 第xx话，含中文数字
	{"剧名.第03集.mp4", true, nil, intPtr(3)},
	{"剧名 第03集.mp4", true, nil, intPtr(3)},
	{"剧名 第 十 集.mp4", true, nil, intPtr(10)},
	{"剧名.第十一集.mp4", true, nil, intPtr(11)},
	{"剧名 第二十三话.mp4", true, nil, intPtr(23)},
	{"Show.第7话.mp4", true, nil, intPtr(7)},
	{"第 十 集.mp4", true, nil, intPtr(10)},

	// 4) 尾部独立数字
	{"剧名 - 05.mp4", true, nil, intPtr(5)},
	{"剧名 03.mp4", true, nil, intPtr(3)},
	{"剧名.10.mp4", true, nil, intPtr(10)},
	{"剧名.02.mp4", true, nil, intPtr(2)},
	{"01.mp4", true, nil, intPtr(1)},
	{"剧名 - 04 - 1080p.mkv", true, nil, intPtr(4)},

	// 反例：绝不能猜
	{"Movie.2023.1080p.x265.mp4", false, nil, nil},
	{"Movie.2019.mp4", false, nil, nil},
	{"无编号.mp4", false, nil, nil},
	{"Show.1080p.mp4", false, nil, nil},
	{"Show.2160p.HDR.mp4", false, nil, nil},
	{"Show.HD.720.mp4", false, nil, nil},
	{"Show.10bit.mp4", false, nil, nil},
	{"Show.x.264.mkv", false, nil, nil},
	{"Show.CD1.mp4", false, nil, nil},
	{"Show.720p.x264.aac.mp4", false, nil, nil},
}

// TestParseEpisodeTable 是补丁 R1 的解析门禁（≥20 例，含点名的正反例）。
func TestParseEpisodeTable(t *testing.T) {
	if len(parseCases) < 20 {
		t.Fatalf("解析表只有 %d 例，R1 要求 ≥20", len(parseCases))
	}
	for _, tc := range parseCases {
		got, ok := media.ParseEpisode(tc.name)
		if ok != tc.ok {
			t.Errorf("%s: ok=%v, 期望 %v（got=%+v）", tc.name, ok, tc.ok, got)
			continue
		}
		if !ok {
			continue
		}
		if (tc.season == nil) != !got.HasSeason {
			t.Errorf("%s: hasSeason=%v, 期望 %v", tc.name, got.HasSeason, tc.season != nil)
		}
		if tc.season != nil && got.Season != *tc.season {
			t.Errorf("%s: season=%d, 期望 %d", tc.name, got.Season, *tc.season)
		}
		if tc.episode != nil && got.Episode != *tc.episode {
			t.Errorf("%s: episode=%d, 期望 %d", tc.name, got.Episode, *tc.episode)
		}
	}
}

// TestEpisodeLabel 覆盖 GET 详情的三种标签文案。
func TestEpisodeLabel(t *testing.T) {
	if got := media.EpisodeLabel(nil, nil); got != "未识别" {
		t.Errorf("未识别标签 = %q", got)
	}
	if got := media.EpisodeLabel(nil, intPtr(3)); got != "第 3 集" {
		t.Errorf("中文集号标签 = %q, 期望 第 3 集", got)
	}
	if got := media.EpisodeLabel(intPtr(1), intPtr(3)); got != "S1E3" {
		t.Errorf("季集标签 = %q, 期望 S1E3", got)
	}
}

func TestNaturalLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"02.mp4", "10.mp4", true},
		{"10.mp4", "02.mp4", false},
		{"A2.mp4", "A10.mp4", true},
		{"A10.mp4", "A2.mp4", false},
		{"a2.mp4", "B1.mp4", true},
		{"第2集.mp4", "第10集.mp4", true},
	}
	for _, tc := range cases {
		if got := media.NaturalLess(tc.a, tc.b); got != tc.want {
			t.Errorf("NaturalLess(%q,%q)=%v, 期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestSortOrderKeys 是排序门禁：季→集→文件名自然序，未识别永远在末尾。
func TestSortOrderKeys(t *testing.T) {
	keys := []media.OrderKey{
		{Name: "B.mp4", Position: 4}, // 未识别
		{Name: "剧名.S01E10.mp4", Position: 2, Episode: intPtr(10), Season: intPtr(1)},
		{Name: "A10.mp4", Position: 5}, // 未识别
		{Name: "剧名.S01E02.mp4", Position: 1, Episode: intPtr(2), Season: intPtr(1)},
		{Name: "A2.mp4", Position: 6}, // 未识别
		{Name: "剧名.S02E01.mp4", Position: 3, Episode: intPtr(1), Season: intPtr(2)},
	}
	order := media.SortOrderKeys(keys)
	got := []string{}
	for _, idx := range order {
		got = append(got, keys[idx].Name)
	}
	want := []string{"剧名.S01E02.mp4", "剧名.S01E10.mp4", "剧名.S02E01.mp4", "A2.mp4", "A10.mp4", "B.mp4"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("播放顺序 = %v, 期望 %v", got, want)
		}
	}
}

// TestSortOrderKeysManualWins 是"手动排序是权威"的门禁：
// 手动排过之后，同层按 position 走，自动识别不覆盖它。
func TestSortOrderKeysManualWins(t *testing.T) {
	keys := []media.OrderKey{
		{Name: "剧名.S01E02.mp4", Position: 2, Episode: intPtr(2), Season: intPtr(1), Manual: true},
		{Name: "剧名.S01E10.mp4", Position: 1, Episode: intPtr(10), Season: intPtr(1), Manual: true},
		{Name: "无编号.mp4", Position: 3}, // 未识别，仍必须在末尾
	}
	order := media.SortOrderKeys(keys)
	got := []string{}
	for _, idx := range order {
		got = append(got, keys[idx].Name)
	}
	want := []string{"剧名.S01E10.mp4", "剧名.S01E02.mp4", "无编号.mp4"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("手动顺序 = %v, 期望 %v", got, want)
		}
	}
}

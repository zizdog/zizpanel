package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 剧场门禁：顺序连播、增删排序、权限、删除不碰媒体文件、不吃随机 bag。

type seriesBody struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	EpisodeCount int    `json:"episode_count"`
	CoverURL     string `json:"cover_url"`
}

type seriesDetail struct {
	Series seriesBody `json:"series"`
	List   []struct {
		Position int `json:"position"`
		Episode  int `json:"episode"`
		Media    struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"media"`
	} `json:"list"`
}

func (e *env) createSeries(title string) seriesBody {
	e.t.Helper()
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/series", map[string]any{"title": title})
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("新建剧场失败 %d: %s", res.StatusCode, raw)
	}
	var body seriesBody
	decodeInto(e.t, env.Data, &body)
	if body.ID == "" {
		e.t.Fatalf("新建剧场未返回 id: %s", raw)
	}
	return body
}

func (e *env) seriesDetail(id string) seriesDetail {
	e.t.Helper()
	res, env, raw := e.do(http.MethodGet, "/api/v1/series/"+id, nil)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("读取剧场失败 %d: %s", res.StatusCode, raw)
	}
	var out seriesDetail
	decodeInto(e.t, env.Data, &out)
	return out
}

func (e *env) addSeriesMedia(id string, mediaIDs ...string) (int, int) {
	e.t.Helper()
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/series/"+id+"/media",
		map[string]any{"media_ids": mediaIDs})
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("加入剧集失败 %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Added        int `json:"added"`
		Requested    int `json:"requested"`
		EpisodeCount int `json:"episode_count"`
	}
	decodeInto(e.t, env.Data, &out)
	return out.Added, out.EpisodeCount
}

func (e *env) episodes(id string) []string {
	e.t.Helper()
	detail := e.seriesDetail(id)
	out := []string{}
	for _, item := range detail.List {
		out = append(out, item.Media.ID)
	}
	return out
}

// TestSeriesEpisodesFollowPositionOrder 是"剧场顺序连播"的后端门禁：
// 顺序必须由 position 决定，和媒体 id / 创建时间无关。
func TestSeriesEpisodesFollowPositionOrder(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "a.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "b.mp4"), []byte("b"))
	c := e.newMedia(lib.ID, filepath.Join(e.Root, "c.mp4"), []byte("c"))

	series := e.createSeries("测试剧场")
	if _, count := e.addSeriesMedia(series.ID, a.ID, b.ID, c.ID); count != 3 {
		t.Fatalf("加入后集数 = %d, 期望 3", count)
	}
	got := e.episodes(series.ID)
	want := []string{a.ID, b.ID, c.ID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("默认顺序 = %v, 期望 %v", got, want)
		}
	}

	res, _, raw := e.write(http.MethodPut, "/api/v1/admin/series/"+series.ID+"/order",
		map[string]any{"media_ids": []string{c.ID, a.ID, b.ID}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("排序失败 %d: %s", res.StatusCode, raw)
	}
	detail := e.seriesDetail(series.ID)
	if len(detail.List) != 3 {
		t.Fatalf("集数 = %d, 期望 3", len(detail.List))
	}
	for i, item := range detail.List {
		if item.Episode != i+1 || item.Position != i+1 {
			t.Fatalf("第 %d 项 episode=%d position=%d, 期望都是 %d", i, item.Episode, item.Position, i+1)
		}
	}
	got = e.episodes(series.ID)
	want = []string{c.ID, a.ID, b.ID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("排序后顺序 = %v, 期望 %v", got, want)
		}
	}

	// 连播的下一集判据就是 position+1：这里断言它拿到的正是排好序的下一项。
	if got[1] != a.ID {
		t.Fatalf("第 2 集 = %s, 期望 %s", got[1], a.ID)
	}
}

func TestSeriesAddSkipsDuplicatesAndReportsHonestly(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "a.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "b.mp4"), []byte("b"))
	c := e.newMedia(lib.ID, filepath.Join(e.Root, "c.mp4"), []byte("c"))

	series := e.createSeries("去重剧场")
	if added, count := e.addSeriesMedia(series.ID, a.ID, b.ID); added != 2 || count != 2 {
		t.Fatalf("首次加入 added=%d count=%d, 期望 2/2", added, count)
	}
	// 第二次有 1 个重复：added 必须如实报 1，不能报 requested=2。
	if added, count := e.addSeriesMedia(series.ID, b.ID, c.ID); added != 1 || count != 3 {
		t.Fatalf("重复加入 added=%d count=%d, 期望 1/3", added, count)
	}
}

func TestSeriesRemoveCompactsPositions(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "a.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "b.mp4"), []byte("b"))
	c := e.newMedia(lib.ID, filepath.Join(e.Root, "c.mp4"), []byte("c"))

	series := e.createSeries("删集剧场")
	e.addSeriesMedia(series.ID, a.ID, b.ID, c.ID)
	res, _, raw := e.write(http.MethodDelete, "/api/v1/admin/series/"+series.ID+"/media/"+b.ID, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("移除剧集失败 %d: %s", res.StatusCode, raw)
	}
	detail := e.seriesDetail(series.ID)
	if len(detail.List) != 2 {
		t.Fatalf("移除后集数 = %d, 期望 2", len(detail.List))
	}
	if detail.List[0].Episode != 1 || detail.List[1].Episode != 2 {
		t.Fatalf("移除后集号 = %d,%d, 期望 1,2", detail.List[0].Episode, detail.List[1].Episode)
	}
	got := e.episodes(series.ID)
	if got[0] != a.ID || got[1] != c.ID {
		t.Fatalf("移除后顺序 = %v, 期望 [a c]", got)
	}
}

func TestSeriesReorderRejectsStaleMembership(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "a.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "b.mp4"), []byte("b"))
	series := e.createSeries("过期排序剧场")
	e.addSeriesMedia(series.ID, a.ID, b.ID)

	res, env, _ := e.write(http.MethodPut, "/api/v1/admin/series/"+series.ID+"/order",
		map[string]any{"media_ids": []string{a.ID}})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("成员不一致时状态 = %d, 期望 409", res.StatusCode)
	}
	if env.Error == nil || env.Error.Code != "SERIES_ORDER_MISMATCH" {
		t.Fatalf("错误码 = %+v, 期望 SERIES_ORDER_MISMATCH", env.Error)
	}
	// 顺序必须原样保留，不能被半截写入破坏。
	if got := e.episodes(series.ID); len(got) != 2 || got[0] != a.ID || got[1] != b.ID {
		t.Fatalf("失败排序后顺序 = %v, 期望 [a b]", got)
	}
}

func TestSeriesRejectsUnknownMediaAndSeries(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	series := e.createSeries("校验剧场")
	res, _, _ := e.write(http.MethodPost, "/api/v1/admin/series/"+series.ID+"/media",
		map[string]any{"media_ids": []string{"med_nope"}})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("加入不存在的媒体状态 = %d, 期望 404", res.StatusCode)
	}
	res, _, _ = e.write(http.MethodPost, "/api/v1/admin/series/ser_nope/media",
		map[string]any{"media_ids": []string{"med_nope"}})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("向不存在的剧场加入媒体状态 = %d, 期望 404", res.StatusCode)
	}
	res, _, _ = e.write(http.MethodPost, "/api/v1/admin/series", map[string]any{"title": "   "})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("空标题状态 = %d, 期望 400", res.StatusCode)
	}
}

func TestSeriesPermissionSplit(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	anon := e.anonClient()
	res, _, _ := e.doAs(anon, http.MethodGet, "/api/v1/series")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录读剧场状态 = %d, 期望 401", res.StatusCode)
	}

	res, _, raw := e.write(http.MethodPost, "/api/v1/users", map[string]string{
		"username": "bob", "password": "bobpass12345", "display_name": "Bob", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("创建普通用户失败 %d: %s", res.StatusCode, raw)
	}
	bob := e.anonClient()
	if res, raw := e.loginAs(bob, "bob", "bobpass12345"); res.StatusCode != http.StatusOK {
		t.Fatalf("普通用户登录失败 %d: %s", res.StatusCode, raw)
	}
	res, _, _ = e.doAs(bob, http.MethodGet, "/api/v1/series")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("普通用户读剧场状态 = %d, 期望 200", res.StatusCode)
	}
	res, _, _ = e.writeAs(bob, http.MethodPost, "/api/v1/admin/series", map[string]any{"title": "x"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户建剧场状态 = %d, 期望 403", res.StatusCode)
	}
}

func TestDeleteSeriesKeepsMediaRowsAndFiles(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	paths := []string{filepath.Join(e.Root, "a.mp4"), filepath.Join(e.Root, "b.mp4")}
	ids := []string{}
	for i, p := range paths {
		m := e.newMedia(lib.ID, p, []byte{byte('a' + i)})
		ids = append(ids, m.ID)
	}
	before, err := e.DB.CountMedia("")
	if err != nil {
		t.Fatal(err)
	}
	series := e.createSeries("待删剧场")
	e.addSeriesMedia(series.ID, ids...)

	res, _, raw := e.write(http.MethodDelete, "/api/v1/admin/series/"+series.ID, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("删除剧场失败 %d: %s", res.StatusCode, raw)
	}
	if res, env, _ := e.do(http.MethodGet, "/api/v1/series", nil); res.StatusCode == http.StatusOK {
		var list struct {
			List []seriesBody `json:"list"`
		}
		decodeInto(t, env.Data, &list)
		for _, item := range list.List {
			if item.ID == series.ID {
				t.Fatal("已删除的剧场仍在列表里")
			}
		}
	}
	after, err := e.DB.CountMedia("")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("删除剧场改变了媒体记录数: %d -> %d", before, after)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("删除剧场动了磁盘文件 %s: %v", p, err)
		}
	}
}

// TestSeriesSourceDoesNotTouchFeedRandom 是"剧场不吃随机"的静态门禁：
// 剧场相关代码不得出现 feed 随机游标/种子/用户播放设置的任何标识符。
func TestSeriesSourceDoesNotTouchFeedRandom(t *testing.T) {
	forbidden := []string{"feed_state", "user_prefs", "feed_seed", "FeedNext", "feedNext", "feed/next", "seed"}
	files := []string{
		"handlers_series.go",
		filepath.Join("..", "storage", "series.go"),
		filepath.Join("..", "web", "assets", "js", "series.js"),
		filepath.Join("..", "web", "assets", "js", "series-admin.js"),
	}
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		body := string(raw)
		for _, token := range forbidden {
			if strings.Contains(body, token) {
				t.Fatalf("%s 引用了随机播放标识符 %q —— 剧场必须按序连播，不吃随机 bag", name, token)
			}
		}
	}
}

package api_test

import (
	"net/http"
	"path/filepath"
	"testing"
)

// 补丁 R1 门禁：文件名识别、按集号排序（未识别在末尾）、detect 的 confirm 前后对比、
// 手动排序不被自动识别覆盖。

type detectBody struct {
	Applied      bool `json:"applied"`
	Changed      int  `json:"changed"`
	Updated      int  `json:"updated"`
	ManualSkippd int  `json:"manual_skipped"`
	Total        int  `json:"total"`
	Changes      []struct {
		MediaID   string `json:"media_id"`
		Filename  string `json:"filename"`
		OldLabel  string `json:"old_label"`
		NewLabel  string `json:"new_label"`
		NewSeason *int   `json:"new_season"`
		NewEpisd  *int   `json:"new_episode"`
	} `json:"changes"`
}

func (e *env) detect(id string, confirm bool) detectBody {
	e.t.Helper()
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/series/"+id+"/detect",
		map[string]any{"confirm": confirm})
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("detect 失败 %d: %s", res.StatusCode, raw)
	}
	var out detectBody
	decodeInto(e.t, env.Data, &out)
	return out
}

func (e *env) addSeriesMediaDetailed(id string, mediaIDs ...string) (int, int) {
	e.t.Helper()
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/series/"+id+"/media",
		map[string]any{"media_ids": mediaIDs})
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("加入剧集失败 %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Added    int `json:"added"`
		Detected int `json:"detected"`
	}
	decodeInto(e.t, env.Data, &out)
	return out.Added, out.Detected
}

func (e *env) labels(id string) []string {
	e.t.Helper()
	detail := e.seriesDetail(id)
	out := []string{}
	for _, item := range detail.List {
		out = append(out, item.EpisodeLabel)
	}
	return out
}

func (e *env) names(id string) []string {
	e.t.Helper()
	detail := e.seriesDetail(id)
	out := []string{}
	for _, item := range detail.List {
		out = append(out, filepath.Base(item.Media.Path))
	}
	return out
}

// TestSeriesAddRecognizesEpisodesAndSorts 覆盖"加入即识别 + 按集号排序 + 未识别在末尾"。
func TestSeriesAddRecognizesEpisodesAndSorts(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	names := []string{
		"Movie.2023.1080p.x265.mp4", // 未识别
		"剧名.20.mp4",
		"剧名 第03集.mp4",
		"剧名.S01E10.mp4",
		"剧名.S01E02.mp4",
	}
	ids := []string{}
	for _, name := range names {
		m := e.newMedia(lib.ID, filepath.Join(e.Root, name), []byte(name))
		ids = append(ids, m.ID)
	}
	series := e.createSeries("识别剧场")
	added, detected := e.addSeriesMediaDetailed(series.ID, ids...)
	if added != 5 || detected != 4 {
		t.Fatalf("added=%d detected=%d, 期望 5/4（2023 那个不许被识别）", added, detected)
	}
	wantNames := []string{"剧名.S01E02.mp4", "剧名 第03集.mp4", "剧名.S01E10.mp4", "剧名.20.mp4", "Movie.2023.1080p.x265.mp4"}
	gotNames := e.names(series.ID)
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("排序 = %v, 期望 %v", gotNames, wantNames)
		}
	}
	wantLabels := []string{"S1E2", "第 3 集", "S1E10", "第 20 集", "未识别"}
	gotLabels := e.labels(series.ID)
	for i := range wantLabels {
		if gotLabels[i] != wantLabels[i] {
			t.Fatalf("episode_label = %v, 期望 %v", gotLabels, wantLabels)
		}
	}
	// 未识别必须在末尾，不许塞进中间冒充有序。
	if gotLabels[len(gotLabels)-1] != "未识别" {
		t.Fatalf("未识别没有排到末尾: %v", gotLabels)
	}
}

// TestSeriesDetectConfirmBeforeAfter 是"detect 需 confirm 才落库"的门禁。
func TestSeriesDetectConfirmBeforeAfter(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.10.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.S01E02.mp4"), []byte("b"))
	series := e.createSeries("确认剧场")
	e.addSeriesMedia(series.ID, a.ID, b.ID)
	// 模拟 R1 之前的旧数据：清空识别结果。
	if _, err := e.DB.Exec(
		`UPDATE series_media SET season = NULL, episode = NULL, episode_source = NULL WHERE series_id = ?`,
		series.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.labels(series.ID); got[0] != "未识别" || got[1] != "未识别" {
		t.Fatalf("清理后标签 = %v, 期望都是未识别", got)
	}

	preview := e.detect(series.ID, false)
	if preview.Applied || preview.Changed != 2 || preview.Updated != 0 {
		t.Fatalf("未确认的 detect = %+v, 期望 applied=false changed=2 updated=0", preview)
	}
	if got := e.labels(series.ID); got[0] != "未识别" {
		t.Fatalf("未确认就已落库: %v", got)
	}

	applied := e.detect(series.ID, true)
	if !applied.Applied || applied.Updated != 2 {
		t.Fatalf("确认的 detect = %+v, 期望 applied=true updated=2", applied)
	}
	got := e.labels(series.ID)
	if got[0] != "S1E2" || got[1] != "第 10 集" {
		t.Fatalf("确认后标签 = %v, 期望 [S1E2 第 10 集]", got)
	}
	detail := e.seriesDetail(series.ID)
	for _, item := range detail.List {
		if item.EpisodeSource != "filename" {
			t.Fatalf("确认后 episode_source = %q, 期望 filename", item.EpisodeSource)
		}
	}
	// 再跑一次：没有变化，不能谎报 updated。
	if again := e.detect(series.ID, true); again.Changed != 0 || again.Updated != 0 {
		t.Fatalf("重复 detect = %+v, 期望无变化", again)
	}
}

// TestSeriesDetectDoesNotOverwriteManual 是"自动识别不覆盖 manual"的门禁。
func TestSeriesDetectDoesNotOverwriteManual(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.S01E02.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.S01E10.mp4"), []byte("b"))
	series := e.createSeries("手动剧场")
	e.addSeriesMedia(series.ID, a.ID, b.ID)

	// 手动排序：2 和 10 对调，两行都应变成 manual。
	res, _, raw := e.write(http.MethodPut, "/api/v1/admin/series/"+series.ID+"/order",
		map[string]any{"media_ids": []string{b.ID, a.ID}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("手动排序失败 %d: %s", res.StatusCode, raw)
	}
	for _, item := range e.seriesDetail(series.ID).List {
		if item.EpisodeSource != "manual" {
			t.Fatalf("PUT order 后 episode_source = %q, 期望 manual", item.EpisodeSource)
		}
	}
	// 把数字清空但保留 manual：自动识别不许把它们填回来。
	if _, err := e.DB.Exec(
		`UPDATE series_media SET season = NULL, episode = NULL WHERE series_id = ?`, series.ID); err != nil {
		t.Fatal(err)
	}
	out := e.detect(series.ID, true)
	if out.Changed != 0 || out.Updated != 0 || out.ManualSkippd != 2 {
		t.Fatalf("detect = %+v, 期望全被 manual 跳过", out)
	}
	after := e.seriesDetail(series.ID)
	if after.List[0].Episode != nil || after.List[1].Episode != nil {
		t.Fatalf("detect 覆盖了 manual 行的数据: %+v", after.List)
	}
	// 顺序仍是手动排的（b 在前）。
	if got := e.episodes(series.ID); got[0] != b.ID || got[1] != a.ID {
		t.Fatalf("手动顺序被改动: %v", got)
	}
}

// TestSeriesDetectPermissionSplit：普通用户不得调用 detect。
func TestSeriesDetectPermissionSplit(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	series := e.createSeries("权限剧场")
	res, _, raw := e.write(http.MethodPost, "/api/v1/users", map[string]string{
		"username": "carol", "password": "carolpass123", "display_name": "Carol", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("创建普通用户失败 %d: %s", res.StatusCode, raw)
	}
	carol := e.anonClient()
	if res, raw := e.loginAs(carol, "carol", "carolpass123"); res.StatusCode != http.StatusOK {
		t.Fatalf("普通用户登录失败 %d: %s", res.StatusCode, raw)
	}
	res, _, _ = e.writeAs(carol, http.MethodPost, "/api/v1/admin/series/"+series.ID+"/detect",
		map[string]any{"confirm": true})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户 detect 状态 = %d, 期望 403", res.StatusCode)
	}
}

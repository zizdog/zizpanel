package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 按目录建剧场（A/B）门禁：预览计数、导入顺序、幂等、越界拒绝、批量映射。

type dirPreviewBody struct {
	Path         string `json:"path"`
	TotalFiles   int    `json:"total_files"`
	Recognized   int    `json:"recognized"`
	Unidentified int    `json:"unidentified"`
	Entries      []struct {
		Path             string `json:"path"`
		EpisodeLabel     string `json:"episode_label"`
		InSeries         bool   `json:"in_series"`
		OtherSeriesTitle string `json:"other_series_title"`
	} `json:"entries"`
}

type dirImportBody struct {
	Registered   int `json:"registered"`
	Added        int `json:"added"`
	Skipped      int `json:"skipped"`
	Unidentified int `json:"unidentified"`
	EpisodeCount int `json:"episode_count"`
}

type dirsPreviewBody struct {
	Subdirs   int `json:"subdirs"`
	FileCount int `json:"file_count"`
	Entries   []struct {
		Name         string `json:"name"`
		FileCount    int    `json:"file_count"`
		Recognized   int    `json:"recognized"`
		Unidentified int    `json:"unidentified"`
		SeriesID     string `json:"series_id"`
		WillReuse    bool   `json:"will_reuse"`
	} `json:"entries"`
}

func putFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sandboxDir 造 剧集/庆余年/{S01E01,S01E02,SP01,乱名} 并返回库与目录。
func sandboxDir(t *testing.T, e *env) (libRoot, seriesDir string) {
	t.Helper()
	libRoot = filepath.Join(e.Root, "剧集")
	seriesDir = filepath.Join(libRoot, "庆余年")
	for _, name := range []string{"S01E01.mp4", "S01E02.mp4", "SP01.mkv", "乱名.mp4"} {
		putFile(t, filepath.Join(seriesDir, name))
	}
	return libRoot, seriesDir
}

// 门禁 A：预览 识别 2 / 未识别 2；导入后顺序正确；再导入一次不新增。
func TestSeriesDirImportPreviewOrderAndIdempotent(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot, seriesDir := sandboxDir(t, e)
	e.newLibrary("剧集库", libRoot)
	series := e.createSeries("庆余年")

	res, env, raw := e.write(http.MethodPost,
		"/api/v1/admin/series/"+series.ID+"/import-dir/preview",
		map[string]any{"path": seriesDir})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("预览失败 %d: %s", res.StatusCode, raw)
	}
	var preview dirPreviewBody
	decodeInto(t, env.Data, &preview)
	if preview.TotalFiles != 4 || preview.Recognized != 2 || preview.Unidentified != 2 {
		t.Fatalf("预览计数不对: %+v", preview)
	}
	labels := map[string]string{}
	for _, entry := range preview.Entries {
		labels[filepath.Base(entry.Path)] = entry.EpisodeLabel
		if entry.InSeries {
			t.Fatalf("尚未导入就报了 in_series: %+v", entry)
		}
	}
	if labels["S01E01.mp4"] != "S1E1" || labels["S01E02.mp4"] != "S1E2" {
		t.Fatalf("识别标签不对: %v", labels)
	}
	if labels["SP01.mkv"] != "未识别" || labels["乱名.mp4"] != "未识别" {
		t.Fatalf("SP01/乱名必须未识别: %v", labels)
	}

	res, env, raw = e.write(http.MethodPost, "/api/v1/admin/series/"+series.ID+"/import-dir",
		map[string]any{"path": seriesDir})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("导入失败 %d: %s", res.StatusCode, raw)
	}
	var first dirImportBody
	decodeInto(t, env.Data, &first)
	if first.Added != 4 || first.Registered != 4 || first.Skipped != 0 || first.Unidentified != 2 {
		t.Fatalf("首次导入结果不对: %+v", first)
	}

	detail := e.seriesDetail(series.ID)
	got := []string{}
	for _, item := range detail.List {
		got = append(got, item.EpisodeLabel+":"+filepath.Base(item.Media.Path))
	}
	want := []string{"S1E1:S01E01.mp4", "S1E2:S01E02.mp4", "未识别:SP01.mkv", "未识别:乱名.mp4"}
	if len(got) != len(want) {
		t.Fatalf("集数 = %d (%v)，期望 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("剧集顺序错误: got %v, want %v", got, want)
		}
	}

	// 幂等：同一目录再导入一次不新增集、不重复登记媒体。
	res, env, raw = e.write(http.MethodPost, "/api/v1/admin/series/"+series.ID+"/import-dir",
		map[string]any{"path": seriesDir})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("二次导入失败 %d: %s", res.StatusCode, raw)
	}
	var second dirImportBody
	decodeInto(t, env.Data, &second)
	if second.Added != 0 || second.Skipped != 4 || second.Registered != 0 || second.EpisodeCount != 4 {
		t.Fatalf("重复导入必须幂等: %+v", second)
	}
}

// 负向对照：允许根之外的目录必须被拒（即使路径绝对、规范）。
func TestSeriesDirImportRejectsOutsideAllowRoot(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "剧集")
	e.newLibrary("剧集库", libRoot)
	series := e.createSeries("庆余年")

	outside := filepath.Join(e.Base, "outside")
	putFile(t, filepath.Join(outside, "S01E01.mp4"))
	res, _, raw := e.write(http.MethodPost, "/api/v1/admin/series/"+series.ID+"/import-dir/preview",
		map[string]any{"path": outside})
	if res.StatusCode == http.StatusOK {
		t.Fatalf("越界目录必须被拒: %s", raw)
	}
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusBadRequest {
		t.Fatalf("越界目录状态码 = %d，期望 400/403: %s", res.StatusCode, raw)
	}

	// 相对路径同样拒绝。
	res, _, raw = e.write(http.MethodPost, "/api/v1/admin/series/"+series.ID+"/import-dir/preview",
		map[string]any{"path": "剧集/庆余年"})
	if res.StatusCode == http.StatusOK {
		t.Fatalf("相对路径必须被拒: %s", raw)
	}
}

// 负向对照：允许根内但不在任何媒体库内的目录要如实拒绝（没有库可挂媒体记录）。
func TestSeriesDirImportRequiresLibrary(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	dir := filepath.Join(e.Root, "无库目录")
	putFile(t, filepath.Join(dir, "S01E01.mp4"))
	series := e.createSeries("庆余年")
	res, _, raw := e.write(http.MethodPost, "/api/v1/admin/series/"+series.ID+"/import-dir/preview",
		map[string]any{"path": dir})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("无库目录状态 = %d，期望 409: %s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "媒体库") {
		t.Fatalf("拒绝理由要说清「没有媒体库」: %s", raw)
	}
}

// 门禁 B：一级子目录 = 一个剧场；任务中心跑完集数正确；再跑一次不重复建。
func TestSeriesDirsBatchJobAndRerun(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "剧集")
	e.newLibrary("剧集库", libRoot)
	for _, name := range []string{"S01E01.mp4", "S01E02.mp4", "S01E03.mkv"} {
		putFile(t, filepath.Join(libRoot, "庆余年", name))
	}
	for _, name := range []string{"EP01.mp4", "EP02.mp4"} {
		putFile(t, filepath.Join(libRoot, "狂飙", name))
	}

	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/series/import-dirs/preview",
		map[string]any{"path": libRoot})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("批量预览失败 %d: %s", res.StatusCode, raw)
	}
	var preview dirsPreviewBody
	decodeInto(t, env.Data, &preview)
	if preview.Subdirs != 2 || preview.FileCount != 5 {
		t.Fatalf("批量预览计数不对: %+v", preview)
	}
	byName := map[string]int{}
	for _, entry := range preview.Entries {
		byName[entry.Name] = entry.Recognized
	}
	if byName["庆余年"] != 3 || byName["狂飙"] != 2 {
		t.Fatalf("每个子目录识别集数不对: %v", byName)
	}

	start := e.startDirsImport(t, libRoot)
	task := e.waitImportJob(t, start)
	if task.Status != "success" || task.Processed != 2 || task.Failed != 0 {
		t.Fatalf("批量任务终态不对: %+v", task)
	}
	if task.Summary.SeriesCreated != 2 || task.Summary.EpisodesAdded != 5 {
		t.Fatalf("批量任务汇总不对: %+v", task.Summary)
	}

	qy := e.seriesByTitle(t, "庆余年")
	kb := e.seriesByTitle(t, "狂飙")
	if qy.EpisodeCount != 3 || kb.EpisodeCount != 2 {
		t.Fatalf("剧场集数不对: 庆余年=%d 狂飙=%d", qy.EpisodeCount, kb.EpisodeCount)
	}

	// 再跑一次：复用已有剧场补集，不重复建、不重复加集。
	start = e.startDirsImport(t, libRoot)
	task = e.waitImportJob(t, start)
	if task.Summary.SeriesCreated != 0 || task.Summary.SeriesReused != 2 || task.Summary.EpisodesAdded != 0 {
		t.Fatalf("重跑必须幂等: %+v", task.Summary)
	}
}

type importJobBody struct {
	Status    string `json:"status"`
	Processed int    `json:"processed"`
	Failed    int    `json:"failed"`
	Updated   int    `json:"updated"`
	Summary   struct {
		SeriesCreated int `json:"series_created"`
		SeriesReused  int `json:"series_reused"`
		EpisodesAdded int `json:"episodes_added"`
		FailedTotal   int `json:"failed_total"`
	} `json:"summary"`
}

func (e *env) startDirsImport(t *testing.T, path string) string {
	t.Helper()
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/series/import-dirs",
		map[string]any{"path": path})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("批量建剧场未返回 202: %d %s", res.StatusCode, raw)
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	decodeInto(t, env.Data, &out)
	if out.TaskID == "" {
		t.Fatalf("批量建剧场没有 task_id: %s", raw)
	}
	return out.TaskID
}

func (e *env) waitImportJob(t *testing.T, id string) importJobBody {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, env, raw := e.do(http.MethodGet, "/api/v1/admin/tasks/"+id, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("查询导入任务失败 %d: %s", res.StatusCode, raw)
		}
		var task importJobBody
		decodeInto(t, env.Data, &task)
		if task.Status == "success" || task.Status == "failed" || task.Status == "interrupted" {
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("导入任务 %s 未在超时内结束", id)
	return importJobBody{}
}

func (e *env) seriesByTitle(t *testing.T, title string) seriesBody {
	t.Helper()
	_, env, raw := e.do(http.MethodGet, "/api/v1/series", nil)
	var body struct {
		List []seriesBody `json:"list"`
	}
	decodeInto(t, env.Data, &body)
	for _, item := range body.List {
		if item.Title == title {
			return item
		}
	}
	t.Fatalf("找不到剧场 %q: %s", title, raw)
	return seriesBody{}
}

package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zizdog/zizvideo/internal/domain"
)

// D.2 识别类门禁：只填未识别 / 不覆盖 manual / 批量幂等 / 预览不落库 /
// confirm 才落库 / 后台失败如实 / 任务进度可查；外加扫描成功后自动补齐。

type detectAllBody struct {
	TaskID             string `json:"task_id"`
	Total              int    `json:"total"`
	Note               string `json:"note"`
	SeriesTotal        int    `json:"series_total"`
	SeriesWithChanges  int    `json:"series_with_changes"`
	ChangesTotal       int    `json:"changes_total"`
	ManualSkippedTotal int    `json:"manual_skipped_total"`
	UnidentifiedTotal  int    `json:"unidentified_total"`
	FailedTotal        int    `json:"failed_total"`
	PerSeries          []struct {
		SeriesID      string `json:"series_id"`
		Title         string `json:"title"`
		Changed       int    `json:"changed"`
		Updated       int    `json:"updated"`
		ManualSkipped int    `json:"manual_skipped"`
		Unidentified  int    `json:"unidentified"`
		Error         string `json:"error"`
		Changes       []struct {
			MediaID string `json:"media_id"`
		} `json:"changes"`
	} `json:"per_series"`
}

type jobTaskBody struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Trigger       string `json:"trigger"`
	Status        string `json:"status"`
	Total         int    `json:"total"`
	Processed     int    `json:"processed"`
	Updated       int    `json:"updated"`
	Failed        int    `json:"failed"`
	ManualSkipped int    `json:"manual_skipped"`
	Unidentified  int    `json:"unidentified"`
	Degraded      bool   `json:"degraded"`
	DegradeReason string `json:"degrade_reason"`
	Error         string `json:"error"`
	Summary       struct {
		SeriesTotal  int `json:"series_total"`
		ChangesTotal int `json:"changes_total"`
		UpdatedTotal int `json:"updated_total"`
		FailedTotal  int `json:"failed_total"`
		PerSeries    []struct {
			SeriesID string `json:"series_id"`
		} `json:"per_series"`
	} `json:"summary"`
}

func (e *env) detectAll(body any) (*http.Response, detectAllBody, []byte) {
	e.t.Helper()
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/series/detect-all", body)
	var out detectAllBody
	decodeInto(e.t, env.Data, &out)
	return res, out, raw
}

func (e *env) jobTask(id string) jobTaskBody {
	e.t.Helper()
	res, env, raw := e.do(http.MethodGet, "/api/v1/admin/tasks/"+id, nil)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("查询识别任务失败 %d: %s", res.StatusCode, raw)
	}
	var out jobTaskBody
	decodeInto(e.t, env.Data, &out)
	return out
}

// waitJob polls until the job reaches a terminal status (进度可查门禁）。
func (e *env) waitJob(id string) jobTaskBody {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		task := e.jobTask(id)
		switch task.Status {
		case "success", "failed", "interrupted":
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("识别任务 %s 未在超时内结束", id)
	return jobTaskBody{}
}

func clearSeriesEpisodes(t *testing.T, e *env, seriesID string) {
	t.Helper()
	if _, err := e.DB.Exec(`UPDATE series_media SET season = NULL, episode = NULL,
		episode_source = NULL WHERE series_id = ?`, seriesID); err != nil {
		t.Fatal(err)
	}
}

func seriesIdentifiedCount(t *testing.T, e *env, seriesID string) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow(`SELECT COUNT(1) FROM series_media WHERE series_id = ?
		AND (season IS NOT NULL OR episode IS NOT NULL)`,
		seriesID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDetectAllPreviewDoesNotWriteAndConfirmIsIdempotent 覆盖"预览不落库 /
// confirm 才落库 / 只填未识别 / 批量幂等 / 进度与汇总可查"。
func TestDetectAllPreviewDoesNotWriteAndConfirmIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.S01E02.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名 第03集.mp4"), []byte("b"))
	series := e.createSeries("批量剧场")
	e.addSeriesMedia(series.ID, a.ID, b.ID)
	clearSeriesEpisodes(t, e, series.ID)

	_, preview, _ := e.detectAll(map[string]any{"confirm": false})
	if preview.SeriesTotal != 1 || preview.SeriesWithChanges != 1 || preview.ChangesTotal != 2 ||
		preview.FailedTotal != 0 || len(preview.PerSeries) != 1 || len(preview.PerSeries[0].Changes) != 2 {
		t.Fatalf("预览 = %+v, 期望 1 剧场 2 集变化", preview)
	}
	if n := seriesIdentifiedCount(t, e, series.ID); n != 0 {
		t.Fatalf("预览落库了 %d 行", n)
	}

	res, job, raw := e.detectAll(map[string]any{"confirm": true})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("confirm 状态 = %d, 期望 202: %s", res.StatusCode, raw)
	}
	if job.TaskID == "" || job.Total != 1 {
		t.Fatalf("confirm 返回 = %+v, 期望 202 + task_id", job)
	}
	task := e.waitJob(job.TaskID)
	if task.Status != "success" || task.Updated != 2 || task.Failed != 0 || task.Processed != 1 {
		t.Fatalf("任务终态 = %+v, 期望 success updated=2", task)
	}
	if task.Summary.UpdatedTotal != 2 || task.Summary.ChangesTotal != 2 || task.Summary.SeriesTotal != 1 {
		t.Fatalf("任务汇总 = %+v, 期望 updated=2 changes=2", task.Summary)
	}
	got := e.labels(series.ID)
	if got[0] != "S1E2" || got[1] != "第 3 集" {
		t.Fatalf("确认后标签 = %v, 期望 [S1E2 第 3 集]", got)
	}

	// 幂等：同参数再跑，第二次 changed=0 && updated=0。
	_, second, _ := e.detectAll(map[string]any{"confirm": false})
	if second.ChangesTotal != 0 {
		t.Fatalf("第二次预览 = %+v, 期望无变化", second)
	}
	_, job2, _ := e.detectAll(map[string]any{"confirm": true})
	task2 := e.waitJob(job2.TaskID)
	if task2.Status != "success" || task2.Updated != 0 || task2.Summary.ChangesTotal != 0 {
		t.Fatalf("第二次任务 = %+v, 期望 updated=0", task2)
	}
}

// TestDetectAllNeverOverwritesManual 覆盖"绝不覆盖 manual"，批量路径同样成立。
func TestDetectAllNeverOverwritesManual(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.S01E02.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.S01E10.mp4"), []byte("b"))
	series := e.createSeries("手动剧场")
	e.addSeriesMedia(series.ID, a.ID, b.ID)

	res, _, raw := e.write(http.MethodPut, "/api/v1/admin/series/"+series.ID+"/order",
		map[string]any{"media_ids": []string{b.ID, a.ID}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("手动排序失败 %d: %s", res.StatusCode, raw)
	}
	// 保留 manual、清掉数字：批量识别不许把它们填回来。
	if _, err := e.DB.Exec(`UPDATE series_media SET season = NULL, episode = NULL WHERE series_id = ?`,
		series.ID); err != nil {
		t.Fatal(err)
	}

	_, preview, _ := e.detectAll(map[string]any{"confirm": false})
	if preview.ChangesTotal != 0 || preview.ManualSkippedTotal != 2 {
		t.Fatalf("预览 = %+v, 期望 0 变化 / 跳过 2 manual", preview)
	}
	_, job, _ := e.detectAll(map[string]any{"confirm": true})
	task := e.waitJob(job.TaskID)
	if task.Status != "success" || task.Updated != 0 || task.ManualSkipped != 2 {
		t.Fatalf("任务 = %+v, 期望 updated=0 manual_skipped=2", task)
	}
	if n := seriesIdentifiedCount(t, e, series.ID); n != 0 {
		t.Fatalf("manual 行被写入: %d", n)
	}
	if list := e.episodes(series.ID); list[0] != b.ID || list[1] != a.ID {
		t.Fatalf("手动顺序被改动: %v", list)
	}
}

// TestDetectAllReportsFailureAndDegradeHonestly 覆盖"后台失败如实"：
// 单剧场失败 ⇒ 任务 failed + error 非空；有未识别 ⇒ degraded + 理由非空。
func TestDetectAllReportsFailureAndDegradeHonestly(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.S01E02.mp4"), []byte("a"))
	series := e.createSeries("混合剧场")
	e.addSeriesMedia(series.ID, a.ID)
	clearSeriesEpisodes(t, e, series.ID)

	res, job, raw := e.detectAll(map[string]any{
		"series_ids": []string{series.ID, "ser_missing"}, "confirm": true})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("状态 = %d, 期望 202: %s", res.StatusCode, raw)
	}
	task := e.waitJob(job.TaskID)
	if task.Status != "failed" || task.Error == "" || task.Failed != 1 {
		t.Fatalf("失败任务 = %+v, 期望 failed + error 非空 + failed=1", task)
	}
	if task.Updated != 1 || task.Processed != 2 {
		t.Fatalf("失败任务计数 = %+v, 期望合法剧场仍被更新", task)
	}

	// 全是无法识别的命名 ⇒ 允许降级，但必须给出理由，且不许猜。
	series2 := e.createSeries("未识别剧场")
	u := e.newMedia(lib.ID, filepath.Join(e.Root, "Movie.2023.1080p.x265.mp4"), []byte("u"))
	e.addSeriesMedia(series2.ID, u.ID)
	_, job2, _ := e.detectAll(map[string]any{"series_ids": []string{series2.ID}, "confirm": true})
	task2 := e.waitJob(job2.TaskID)
	if task2.Status != "success" || !task2.Degraded || task2.DegradeReason == "" || task2.Unidentified != 1 {
		t.Fatalf("降级任务 = %+v, 期望 success + degraded + 理由", task2)
	}
	if n := seriesIdentifiedCount(t, e, series2.ID); n != 0 {
		t.Fatalf("不可识别的命名被猜成了 %d 行", n)
	}
}

// TestDetectAllFiltersByLibrary：library_id 只圈定该库下的剧场。
func TestDetectAllFiltersByLibrary(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib1 := e.newLibrary("库一", e.Root)
	lib2 := e.newLibrary("库二", filepath.Join(e.Root, "sub"))
	m1 := e.newMedia(lib1.ID, filepath.Join(e.Root, "甲.S01E01.mp4"), []byte("1"))
	m2 := e.newMedia(lib2.ID, filepath.Join(e.Root, "sub", "乙.S01E01.mp4"), []byte("2"))
	s1 := e.createSeries("甲剧场")
	s2 := e.createSeries("乙剧场")
	e.addSeriesMedia(s1.ID, m1.ID)
	e.addSeriesMedia(s2.ID, m2.ID)
	clearSeriesEpisodes(t, e, s1.ID)
	clearSeriesEpisodes(t, e, s2.ID)

	_, preview, _ := e.detectAll(map[string]any{"library_id": lib1.ID, "confirm": false})
	if preview.SeriesTotal != 1 || len(preview.PerSeries) != 1 || preview.PerSeries[0].SeriesID != s1.ID {
		t.Fatalf("按库预览 = %+v, 期望只有库一的剧场", preview)
	}
	_, job, _ := e.detectAll(map[string]any{"library_id": lib2.ID, "confirm": true})
	task := e.waitJob(job.TaskID)
	if task.Total != 1 || task.Updated != 1 {
		t.Fatalf("按库任务 = %+v, 期望只处理库二", task)
	}
	if n := seriesIdentifiedCount(t, e, s1.ID); n != 0 {
		t.Fatalf("子集识别影响了其它剧场: %d", n)
	}
}

// TestDetectAllPermissionSplit：普通用户不得调用批量识别与任务查询。
func TestDetectAllPermissionSplit(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	e.createSeries("权限剧场")
	res, _, raw := e.write(http.MethodPost, "/api/v1/users", map[string]string{
		"username": "carol", "password": "carolpass123", "display_name": "Carol", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("创建普通用户失败 %d: %s", res.StatusCode, raw)
	}
	carol := e.anonClient()
	if res, raw := e.loginAs(carol, "carol", "carolpass123"); res.StatusCode != http.StatusOK {
		t.Fatalf("普通用户登录失败 %d: %s", res.StatusCode, raw)
	}
	if res, _, _ := e.writeAs(carol, http.MethodPost, "/api/v1/admin/series/detect-all",
		map[string]any{"confirm": false}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户 detect-all 状态 = %d, 期望 403", res.StatusCode)
	}
	if res, _, _ := e.doAs(carol, http.MethodGet, "/api/v1/admin/tasks/job_x"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户任务查询状态 = %d, 期望 403", res.StatusCode)
	}
}

// waitScanDetectJob finds the job_tasks row the scan-finished hook created.
func waitScanDetectJob(t *testing.T, e *env, trigger string) *domain.JobTask {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var id, status string
		err := e.DB.QueryRow(`SELECT id, status FROM job_tasks WHERE kind = ?
			AND trigger = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
			domain.JobKindEpisodeDetect, trigger).Scan(&id, &status)
		if err == nil && (status == "success" || status == "failed" || status == "interrupted") {
			task, gerr := e.DB.GetJobTask(id)
			if gerr != nil {
				t.Fatal(gerr)
			}
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("未等到 trigger=%s 的识别任务", trigger)
	return nil
}

// TestScanFinishedAutoDetectsNewEpisodes 覆盖"扫描成功结束后自动补齐"：
// 新增文件 → 扫描 success → 后台识别任务被投递并补齐未识别成员。
func TestScanFinishedAutoDetectsNewEpisodes(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("剧集库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "剧名.S01E02.mp4"), []byte("a"))
	series := e.createSeries("扫描剧场")
	e.addSeriesMedia(series.ID, a.ID)
	clearSeriesEpisodes(t, e, series.ID)

	// 新增媒体是"含新增媒体的剧场"的触发条件（A.4）。
	if err := os.WriteFile(filepath.Join(e.Root, "新剧.S02E05.mp4"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, env, raw := e.write(http.MethodPost, "/api/v1/libraries/"+lib.ID+"/scan",
		map[string]any{"kind": "incremental"})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("起扫描失败 %d: %s", res.StatusCode, raw)
	}
	var scan struct {
		TaskID string `json:"task_id"`
	}
	decodeInto(t, env.Data, &scan)
	waitTask(t, e, scan.TaskID)

	task := waitScanDetectJob(t, e, domain.JobTriggerScanFinished)
	if task.Status != "success" || task.Updated != 1 {
		t.Fatalf("扫描后识别任务 = %+v, 期望 success updated=1", task)
	}
	if got := e.labels(series.ID); got[0] != "S1E2" {
		t.Fatalf("扫描后成员未被补齐: %v", got)
	}
}

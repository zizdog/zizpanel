package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zizdog/zizvideo/internal/detect"
	"github.com/zizdog/zizvideo/internal/domain"
)

// 一键识别（批量）+ 扫描成功后的后台补齐。识别内核只在 internal/detect，
// 预览不落库、confirm 才建 job_tasks（A.3/A.4）。

type detectAllReq struct {
	SeriesIDs []string `json:"series_ids"`
	LibraryID string   `json:"library_id"`
	Confirm   bool     `json:"confirm"`
}

// detectSeriesJSON is one 剧场 in a preview or job summary.
type detectSeriesJSON struct {
	SeriesID      string              `json:"series_id"`
	Title         string              `json:"title"`
	Changed       int                 `json:"changed"`
	Updated       int                 `json:"updated"`
	ManualSkipped int                 `json:"manual_skipped"`
	Unidentified  int                 `json:"unidentified"`
	Error         string              `json:"error,omitempty"`
	Changes       []episodeChangeJSON `json:"changes"`
}

type detectSummaryJSON struct {
	SeriesTotal        int                `json:"series_total"`
	SeriesWithChanges  int                `json:"series_with_changes"`
	ChangesTotal       int                `json:"changes_total"`
	UpdatedTotal       int                `json:"updated_total"`
	ManualSkippedTotal int                `json:"manual_skipped_total"`
	UnidentifiedTotal  int                `json:"unidentified_total"`
	FailedTotal        int                `json:"failed_total"`
	PerSeries          []detectSeriesJSON `json:"per_series"`
}

// HandleDetectAll is the batch entry. confirm=false returns the change list
// without writing; confirm=true starts a job_tasks job (202 + task_id).
func (s *Server) HandleDetectAll(w http.ResponseWriter, r *http.Request) {
	var req detectAllReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ids, err := s.detectTargets(req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !req.Confirm {
		respond(w, http.StatusOK, s.detectPreview(r.Context(), ids), nil)
		return
	}
	if len(ids) == 0 {
		// 没有可识别的剧场：不建空任务，避免"成功但什么都没做"的假象。
		respond(w, http.StatusOK, map[string]any{
			"task_id": "", "total": 0, "note": "没有需要识别的剧场"}, nil)
		return
	}
	task, err := s.launchDetectJob(ids, domain.JobTriggerManualBatch)
	if err != nil {
		s.audit(r, "series.detect_all", "count:"+fmt.Sprint(len(ids)), false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "series.detect_all", "job:"+task.ID, true, "count:"+fmt.Sprint(task.Total))
	respond(w, http.StatusAccepted, map[string]any{"task_id": task.ID, "total": task.Total}, nil)
}

// HandleGetJobTask returns job progress; the terminal row carries the summary.
func (s *Server) HandleGetJobTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetJobTask(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, jobTaskJSON(t), nil)
}

func jobTaskJSON(t *domain.JobTask) map[string]any {
	out := map[string]any{
		"id": t.ID, "kind": t.Kind, "trigger": t.Trigger, "status": t.Status,
		"total": t.Total, "processed": t.Processed, "updated": t.Updated,
		"manual_skipped": t.ManualSkipped, "unidentified": t.Unidentified, "failed": t.Failed,
		"degraded": t.Degraded, "degrade_reason": t.DegradeReason, "error": t.Error,
		"started_at": t.StartedAt, "finished_at": t.FinishedAt, "updated_at": t.UpdatedAt,
	}
	if t.Summary != "" {
		var summary any
		if err := json.Unmarshal([]byte(t.Summary), &summary); err == nil {
			out["summary"] = summary
		}
	}
	return out
}

// detectTargets resolves series_ids > library_id > all live 剧场.
func (s *Server) detectTargets(req detectAllReq) ([]string, error) {
	if ids := cleanIDs(req.SeriesIDs); len(ids) > 0 {
		return ids, nil
	}
	if libraryID := strings.TrimSpace(req.LibraryID); libraryID != "" {
		if _, err := s.DB.GetLibrary(libraryID); err != nil {
			return nil, err
		}
		return s.DB.SeriesIDsInLibrary(libraryID)
	}
	rows, err := s.DB.ListSeries(scopeAll())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// detectPreview reads every target without writing anything.
func (s *Server) detectPreview(ctx context.Context, ids []string) detectSummaryJSON {
	out := detectSummaryJSON{SeriesTotal: len(ids), PerSeries: []detectSeriesJSON{}}
	for _, id := range ids {
		row := detectSeriesJSON{SeriesID: id, Changes: []episodeChangeJSON{}}
		res, err := detect.Series(ctx, s.DB, scopeAll(), id)
		if err != nil {
			row.Error = humanErr(err)
			out.FailedTotal++
			out.PerSeries = append(out.PerSeries, row)
			continue
		}
		row.Title = res.Title
		row.Changed = len(res.Changes)
		row.ManualSkipped = res.ManualSkipped
		row.Unidentified = res.Unidentified
		for _, c := range res.Changes {
			row.Changes = append(row.Changes, changeJSON(c))
		}
		out.ChangesTotal += len(res.Changes)
		out.ManualSkippedTotal += res.ManualSkipped
		out.UnidentifiedTotal += res.Unidentified
		if len(res.Changes) > 0 {
			out.SeriesWithChanges++
		}
		out.PerSeries = append(out.PerSeries, row)
	}
	return out
}

// launchDetectJob persists the job row and runs it in the background.
func (s *Server) launchDetectJob(ids []string, trigger string) (*domain.JobTask, error) {
	t := &domain.JobTask{ID: domain.NewID("job"), Kind: domain.JobKindEpisodeDetect,
		Trigger: trigger, Total: len(ids)}
	if err := s.DB.CreateJobTask(t); err != nil {
		return nil, err
	}
	go s.runDetectJob(t.ID, ids)
	return t, nil
}

// runDetectJob settles every 剧场 independently: a failing series is counted and
// reported, the job end state never claims success while failed>0 (A.5).
func (s *Server) runDetectJob(jobID string, ids []string) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Error("识别任务崩溃", "job_id", jobID)
			_ = s.DB.FinishJobTask(jobID, domain.TaskFailed, "识别任务异常退出", "{}", 0, false, "")
		}
	}()
	ctx := context.Background()
	total := len(ids)
	processed, updated, failed, manualSkipped, unidentified := 0, 0, 0, 0, 0
	changesTotal, seriesWithChanges := 0, 0
	perSeries := []detectSeriesJSON{}
	problems := []string{}
	for _, id := range ids {
		row := detectSeriesJSON{SeriesID: id, Changes: []episodeChangeJSON{}}
		res, err := detect.Series(ctx, s.DB, scopeAll(), id)
		if err == nil {
			row.Title, row.Changed = res.Title, len(res.Changes)
			row.ManualSkipped, row.Unidentified = res.ManualSkipped, res.Unidentified
			for _, c := range res.Changes {
				row.Changes = append(row.Changes, changeJSON(c))
			}
			if len(res.Assigns) > 0 {
				applied, aerr := detect.Apply(ctx, s.DB, id, res.Assigns)
				if aerr != nil {
					err = aerr
				} else {
					row.Updated = applied
					updated += applied
				}
			}
		}
		if err != nil {
			failed++
			row.Error = humanErr(err)
			if len(problems) < 5 {
				problems = append(problems, id+": "+row.Error)
			}
		} else {
			manualSkipped += res.ManualSkipped
			unidentified += res.Unidentified
			changesTotal += len(res.Changes)
			if len(res.Changes) > 0 {
				seriesWithChanges++
			}
		}
		processed++
		perSeries = append(perSeries, row)
		if perr := s.DB.UpdateJobTaskProgress(jobID, processed, updated, failed, manualSkipped); perr != nil {
			s.Log.Error("更新识别任务进度失败", "job_id", jobID, "error", perr.Error())
		}
	}
	status, errMsg := domain.TaskSuccess, ""
	if failed > 0 {
		status = domain.TaskFailed
		errMsg = fmt.Sprintf("%d/%d 个剧场识别失败：%s", failed, total, strings.Join(problems, "；"))
	}
	degraded := unidentified > 0
	degradeReason := ""
	if degraded {
		degradeReason = fmt.Sprintf("%d 集文件名不匹配任何模式，已保留未识别", unidentified)
	}
	summary, _ := json.Marshal(detectSummaryJSON{
		SeriesTotal: total, SeriesWithChanges: seriesWithChanges, ChangesTotal: changesTotal,
		UpdatedTotal: updated, ManualSkippedTotal: manualSkipped, UnidentifiedTotal: unidentified,
		FailedTotal: failed, PerSeries: perSeries,
	})
	if err := s.DB.FinishJobTask(jobID, status, errMsg, string(summary), unidentified, degraded, degradeReason); err != nil {
		s.Log.Error("写入识别任务终态失败", "job_id", jobID, "error", err.Error())
	}
	s.Log.Info("识别任务结束", "job_id", jobID, "status", status, "total", total,
		"updated", updated, "failed", failed, "manual_skipped", manualSkipped)
}

// OnScanFinished is the only automatic-detection trigger: after a successful
// scan, 对含新增媒体的剧场补齐（不做服务启动全量、不做定时，A.4）。
func (s *Server) OnScanFinished(libraryID, scanTaskID, status string) {
	if status != domain.TaskSuccess {
		return
	}
	scan, err := s.DB.GetScanTask(scanTaskID)
	if err != nil {
		s.Log.Warn("读取扫描任务失败，跳过自动识别", "scan_task_id", scanTaskID, "error", err.Error())
		return
	}
	ids, err := s.DB.SeriesIDsWithNewMediaSince(libraryID, scan.StartedAt)
	if err != nil {
		s.Log.Error("查询待识别剧场失败", "library_id", libraryID, "error", err.Error())
		return
	}
	if len(ids) == 0 {
		return
	}
	task, err := s.launchDetectJob(ids, domain.JobTriggerScanFinished)
	if err != nil {
		s.Log.Error("创建扫描后识别任务失败", "library_id", libraryID, "error", err.Error())
		return
	}
	s.Log.Info("扫描后自动识别已排队", "library_id", libraryID, "job_id", task.ID, "series", task.Total)
}

// humanErr returns the coded human message, falling back to the raw error.
func humanErr(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return err.Error()
}

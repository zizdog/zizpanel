package storage

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
)

// job_tasks 是跨库后台任务（0008）。不做 scan_tasks 的复用：后者 library_id
// NOT NULL + 外键指向单库，装不下跨库识别任务。
const jobTaskCols = `id, kind, trigger, status, total, processed, updated, failed,
	manual_skipped, unidentified, degraded, degrade_reason, error, summary,
	COALESCE(started_at,''), COALESCE(finished_at,''), created_at, updated_at`

func scanJobTask(s interface{ Scan(...any) error }) (*domain.JobTask, error) {
	var t domain.JobTask
	var degraded int
	if err := s.Scan(&t.ID, &t.Kind, &t.Trigger, &t.Status, &t.Total, &t.Processed, &t.Updated,
		&t.Failed, &t.ManualSkipped, &t.Unidentified, &degraded, &t.DegradeReason, &t.Error,
		&t.Summary, &t.StartedAt, &t.FinishedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Degraded = degraded == 1
	return &t, nil
}

// CreateJobTask persists a new pending job.
func (db *DB) CreateJobTask(t *domain.JobTask) error {
	now := domain.NowString()
	t.Status = domain.TaskPending
	t.StartedAt, t.CreatedAt, t.UpdatedAt = now, now, now
	_, err := db.Exec(`INSERT INTO job_tasks
		(id, kind, trigger, status, total, processed, updated, failed, manual_skipped,
		 unidentified, degraded, degrade_reason, error, summary, started_at, created_at, updated_at)
		VALUES (?,?,?,?,?,0,0,0,0,0,0,'','','',?,?,?)`,
		t.ID, t.Kind, t.Trigger, domain.TaskPending, t.Total, now, now, now)
	return err
}

// GetJobTask loads one job by id.
func (db *DB) GetJobTask(id string) (*domain.JobTask, error) {
	row := db.QueryRow(`SELECT `+jobTaskCols+` FROM job_tasks WHERE id = ?`, id)
	t, err := scanJobTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	return t, err
}

// UpdateJobTaskProgress writes the counters collected so far and flips to running.
func (db *DB) UpdateJobTaskProgress(id string, processed, updated, failed, manualSkipped int) error {
	_, err := db.Exec(`UPDATE job_tasks SET status = ?, processed = ?, updated = ?, failed = ?,
		manual_skipped = ?, updated_at = ? WHERE id = ?`,
		domain.TaskRunning, processed, updated, failed, manualSkipped, domain.NowString(), id)
	return err
}

// FinishJobTask closes a job with its terminal status. 如实语义（A.5）在写库前校验：
// failed 必须带 error、degraded 必须带 reason、success 不得带 error；空理由一律拒绝。
func (db *DB) FinishJobTask(id, status, errMsg, summary string, unidentified int,
	degraded bool, degradeReason string) error {
	errMsg, degradeReason = strings.TrimSpace(errMsg), strings.TrimSpace(degradeReason)
	if status == domain.TaskFailed && errMsg == "" {
		return domain.ErrJobErrorRequired
	}
	if degraded && degradeReason == "" {
		return domain.ErrJobDegradeReason
	}
	if status == domain.TaskSuccess && errMsg != "" {
		return domain.ErrJobErrorOnSuccess
	}
	now := domain.NowString()
	res, err := db.Exec(`UPDATE job_tasks SET status = ?, error = ?, summary = ?, unidentified = ?,
		degraded = ?, degrade_reason = ?, finished_at = ?, updated_at = ?
		WHERE id = ?`,
		status, truncate(errMsg, 500), summary, unidentified, boolToInt(degraded),
		truncate(degradeReason, 200), now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrTaskNotFound
	}
	return nil
}

// AbandonStaleJobTasks marks jobs left running by a previous process as
// interrupted, so a restart never reports phantom progress.
func (db *DB) AbandonStaleJobTasks() (int64, error) {
	now := domain.NowString()
	res, err := db.Exec(`UPDATE job_tasks SET status = ?, error = ?, finished_at = ?, updated_at = ?
		WHERE status IN (?,?)`,
		domain.TaskInterrupted, "服务重启，任务已中断", now, now,
		domain.TaskPending, domain.TaskRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

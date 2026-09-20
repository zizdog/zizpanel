package storage

import (
	"database/sql"
	"errors"

	"github.com/zizdog/zizvideo/internal/domain"
)

const taskCols = `id, library_id, kind, status, total, scanned, updated, failed, missing,
	suspected, error, COALESCE(started_at,''), COALESCE(finished_at,''), updated_at`

func scanTask(s interface{ Scan(...any) error }) (*domain.ScanTask, error) {
	var t domain.ScanTask
	if err := s.Scan(&t.ID, &t.LibraryID, &t.Kind, &t.Status, &t.Total, &t.Scanned, &t.Updated,
		&t.Failed, &t.Missing, &t.Suspected, &t.Error, &t.StartedAt, &t.FinishedAt,
		&t.UpdatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateScanTask persists a new pending scan task.
func (db *DB) CreateScanTask(t *domain.ScanTask) error {
	now := domain.NowString()
	t.Status = domain.TaskPending
	t.StartedAt, t.UpdatedAt = now, now
	_, err := db.Exec(`INSERT INTO scan_tasks
		(id, library_id, kind, status, total, scanned, updated, failed, missing, suspected,
		 error, started_at, created_at, updated_at)
		VALUES (?,?,?,?,0,0,0,0,0,0,'',?,?,?)`,
		t.ID, t.LibraryID, t.Kind, domain.TaskPending, now, now, now)
	return err
}

// GetScanTask loads one task.
func (db *DB) GetScanTask(id string) (*domain.ScanTask, error) {
	row := db.QueryRow(`SELECT `+taskCols+` FROM scan_tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	return t, err
}

// UpdateScanTaskProgress writes the counters collected so far.
func (db *DB) UpdateScanTaskProgress(id string, total, scanned, updated, failed int) error {
	_, err := db.Exec(`UPDATE scan_tasks SET status = ?, total = ?, scanned = ?, updated = ?,
		failed = ?, updated_at = ? WHERE id = ?`,
		domain.TaskRunning, total, scanned, updated, failed, domain.NowString(), id)
	return err
}

// FinishScanTask closes a task with its terminal status.
func (db *DB) FinishScanTask(id, status, errMsg string, missing, suspected int) error {
	now := domain.NowString()
	_, err := db.Exec(`UPDATE scan_tasks SET status = ?, error = ?, missing = ?, suspected = ?,
		finished_at = ?, updated_at = ? WHERE id = ?`,
		status, truncate(errMsg, 500), missing, suspected, now, now, id)
	return err
}

// LatestScanTask returns the most recent task of a library, or nil.
func (db *DB) LatestScanTask(libraryID string) (*domain.ScanTask, error) {
	row := db.QueryRow(`SELECT `+taskCols+` FROM scan_tasks
		WHERE library_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, libraryID)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// ActiveScanTask returns a pending/running task of a library, or nil.
func (db *DB) ActiveScanTask(libraryID string) (*domain.ScanTask, error) {
	row := db.QueryRow(`SELECT `+taskCols+` FROM scan_tasks
		WHERE library_id = ? AND status IN (?,?) ORDER BY created_at DESC LIMIT 1`,
		libraryID, domain.TaskPending, domain.TaskRunning)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// AbandonStaleTasks marks tasks left running by a previous process as
// interrupted, so a restart never reports phantom progress.
func (db *DB) AbandonStaleTasks() (int64, error) {
	now := domain.NowString()
	res, err := db.Exec(`UPDATE scan_tasks SET status = ?, error = ?, finished_at = ?, updated_at = ?
		WHERE status IN (?,?)`,
		domain.TaskInterrupted, "服务重启，任务已中断", now, now,
		domain.TaskPending, domain.TaskRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

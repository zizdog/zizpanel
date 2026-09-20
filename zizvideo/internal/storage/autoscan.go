package storage

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
)

// DefaultAutoScan 是自动扫描的内置默认值：默认开、5 分钟、事件加速开、去抖 8 秒。
func DefaultAutoScan() *domain.AutoScanSettings {
	return &domain.AutoScanSettings{
		Enabled: true, IntervalMinutes: 5, EventsEnabled: true, DebounceSeconds: 8,
	}
}

// GetAutoScanSettings reads the single settings row; a missing row (should not
// happen after 0009) falls back to the documented defaults instead of guessing.
func (db *DB) GetAutoScanSettings() (*domain.AutoScanSettings, error) {
	s := DefaultAutoScan()
	var enabled, events int
	err := db.QueryRow(`SELECT enabled, interval_minutes, events_enabled, debounce_seconds, updated_at
		FROM autoscan_settings WHERE id = 1`).
		Scan(&enabled, &s.IntervalMinutes, &events, &s.DebounceSeconds, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.Enabled, s.EventsEnabled = enabled != 0, events != 0
	return s, nil
}

// SaveAutoScanSettings upserts the single row after validating bounds. Callers
// (API/测试) rely on this being the only write path for these numbers.
func (db *DB) SaveAutoScanSettings(s *domain.AutoScanSettings) error {
	n, err := NormalizeAutoScan(s)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO autoscan_settings
		(id, enabled, interval_minutes, events_enabled, debounce_seconds, updated_at)
		VALUES (1,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			enabled = excluded.enabled, interval_minutes = excluded.interval_minutes,
			events_enabled = excluded.events_enabled, debounce_seconds = excluded.debounce_seconds,
			updated_at = excluded.updated_at`,
		boolToInt(n.Enabled), n.IntervalMinutes, boolToInt(n.EventsEnabled), n.DebounceSeconds,
		domain.NowString())
	return err
}

// NormalizeAutoScan copies s, applies defaults for zero values and rejects
// out-of-range interval/debounce. Zero interval means "not provided".
func NormalizeAutoScan(s *domain.AutoScanSettings) (*domain.AutoScanSettings, error) {
	out := *s
	if out.IntervalMinutes == 0 {
		out.IntervalMinutes = 5
	}
	if out.DebounceSeconds == 0 {
		out.DebounceSeconds = 8
	}
	if out.IntervalMinutes < domain.AutoScanIntervalMin || out.IntervalMinutes > domain.AutoScanIntervalMax {
		return nil, domain.ErrAutoScanInterval
	}
	if out.DebounceSeconds < domain.AutoScanDebounceMin || out.DebounceSeconds > domain.AutoScanDebounceMax {
		return nil, domain.ErrAutoScanDebounce
	}
	return &out, nil
}

const autoScanRunCols = `trigger, status, started_at, finished_at, libraries, started,
	skipped, updated, new_media, failed, missing, events_ok, events_note, note`

func scanAutoScanRun(s interface{ Scan(...any) error }) (*domain.AutoScanRun, error) {
	var r domain.AutoScanRun
	var eventsOK int
	if err := s.Scan(&r.Trigger, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Libraries,
		&r.Started, &r.Skipped, &r.Updated, &r.NewMedia, &r.Failed, &r.Missing,
		&eventsOK, &r.EventsNote, &r.Note); err != nil {
		return nil, err
	}
	r.EventsOK = eventsOK != 0
	return &r, nil
}

// RecordAutoScanRun appends one round. 如实语义在写库前校验：skipped/failed 必须带
// note、events_ok=0 必须带 events_note、partial 必须真有失败，否则拒绝写入。
func (db *DB) RecordAutoScanRun(r *domain.AutoScanRun) error {
	r.Note, r.EventsNote = strings.TrimSpace(r.Note), strings.TrimSpace(r.EventsNote)
	switch {
	case (r.Status == domain.AutoScanSkipped || r.Status == domain.AutoScanFailed) && r.Note == "":
		return domain.ErrAutoScanNoteRequired
	case r.Status == domain.AutoScanPartial && (r.Failed == 0 && r.Skipped == 0):
		return domain.ErrAutoScanNoteRequired
	case r.Status == domain.AutoScanPartial && r.Note == "":
		return domain.ErrAutoScanNoteRequired
	case !r.EventsOK && r.EventsNote == "":
		return domain.ErrAutoScanEventsRequired
	}
	if r.StartedAt == "" {
		r.StartedAt = domain.NowString()
	}
	if r.FinishedAt == "" {
		r.FinishedAt = domain.NowString()
	}
	_, err := db.Exec(`INSERT INTO autoscan_runs
		(trigger, status, started_at, finished_at, libraries, started, skipped, updated,
		 new_media, failed, missing, events_ok, events_note, note)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Trigger, r.Status, r.StartedAt, r.FinishedAt, r.Libraries, r.Started, r.Skipped,
		r.Updated, r.NewMedia, r.Failed, r.Missing, boolToInt(r.EventsOK),
		truncate(r.EventsNote, 200), truncate(r.Note, 300))
	return err
}

// LatestAutoScanRun returns the newest round, or nil when none happened yet.
func (db *DB) LatestAutoScanRun() (*domain.AutoScanRun, error) {
	row := db.QueryRow(`SELECT ` + autoScanRunCols + ` FROM autoscan_runs
		ORDER BY started_at DESC, id DESC LIMIT 1`)
	r, err := scanAutoScanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// PruneAutoScanRuns keeps only the newest `keep` rows.
func (db *DB) PruneAutoScanRuns(keep int) error {
	if keep < 1 {
		keep = 1
	}
	_, err := db.Exec(`DELETE FROM autoscan_runs WHERE id NOT IN (
		SELECT id FROM autoscan_runs ORDER BY id DESC LIMIT ?)`, keep)
	return err
}

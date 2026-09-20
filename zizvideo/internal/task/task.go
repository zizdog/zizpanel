// Package task owns the in-process scan worker pool and its DB-persisted state.
package task

import (
	"context"
	"log/slog"
	"sync"

	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
	"github.com/zizdog/zizvideo/internal/storage"
)

// Manager starts scans and tracks which libraries are busy.
type Manager struct {
	cfg     *config.Config
	db      *storage.DB
	Roots   *config.Roots
	scanner *media.Scanner
	log     *slog.Logger

	baseCtx context.Context
	cancel  context.CancelFunc

	mu     sync.Mutex
	active map[string]bool
	// afterScan 在扫描协程写出终态后被调用（只用于扫描成功后的自动识别）。
	afterScan func(libraryID, scanTaskID, status string)
	wg        sync.WaitGroup
}

// NewManager builds a Manager; call Stop to cancel in-flight scans.
func NewManager(cfg *config.Config, db *storage.DB, roots *config.Roots,
	sc *media.Scanner, log *slog.Logger) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		cfg: cfg, db: db, Roots: roots, scanner: sc, log: log,
		baseCtx: ctx, cancel: cancel, active: map[string]bool{},
	}
}

// Recover marks tasks left over by a previous process as interrupted.
func (m *Manager) Recover() {
	n, err := m.db.AbandonStaleTasks()
	if err != nil {
		m.log.Error("恢复遗留任务失败", "error", err.Error())
	} else if n > 0 {
		m.log.Warn("遗留任务已标记为中断", "count", n)
	}
	// job_tasks 的跨库任务同样要收尾，否则重启后永远显示"进行中"。
	j, jerr := m.db.AbandonStaleJobTasks()
	if jerr != nil {
		m.log.Error("恢复遗留识别任务失败", "error", jerr.Error())
	} else if j > 0 {
		m.log.Warn("遗留识别任务已标记为中断", "count", j)
	}
}

// SetAfterScan registers the post-scan hook; it is invoked with the terminal
// status after the scan task row has been written.
func (m *Manager) SetAfterScan(fn func(libraryID, scanTaskID, status string)) {
	m.mu.Lock()
	m.afterScan = fn
	m.mu.Unlock()
}

// notifyAfterScan re-reads the terminal status so the hook never guesses.
func (m *Manager) notifyAfterScan(libraryID, scanTaskID string) {
	m.mu.Lock()
	fn := m.afterScan
	m.mu.Unlock()
	if fn == nil {
		return
	}
	t, err := m.db.GetScanTask(scanTaskID)
	if err != nil {
		m.log.Warn("读取扫描终态失败，跳过后置识别", "task_id", scanTaskID, "error", err.Error())
		return
	}
	fn(libraryID, scanTaskID, t.Status)
}

// Stop cancels running scans and waits briefly for them to unwind.
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
}

// StartScan creates a task row and runs it in the background. It never blocks
// on the scan itself, so the HTTP handler answers immediately.
func (m *Manager) StartScan(libraryID, kind string) (*domain.ScanTask, error) {
	lib, err := m.db.GetLibrary(libraryID)
	if err != nil {
		return nil, err
	}
	if !lib.Enabled {
		return nil, &domain.Error{Code: "SCAN_LIBRARY_DISABLED",
			Message: "媒体库已禁用，请先启用", Status: 409}
	}
	// Server-side gate: never queue a scan for a root outside the allow list.
	if err := media.ValidateAllowedLibrary(m.Roots.List(), lib.RootPath); err != nil {
		return nil, err
	}
	if kind != "full" {
		kind = "incremental"
	}
	m.mu.Lock()
	if m.active[libraryID] {
		m.mu.Unlock()
		return nil, domain.ErrScanRunning
	}
	if t, err := m.db.ActiveScanTask(libraryID); err == nil && t != nil {
		m.mu.Unlock()
		return nil, domain.ErrScanRunning
	}
	m.active[libraryID] = true
	m.mu.Unlock()

	t := &domain.ScanTask{ID: domain.NewID("scn"), LibraryID: libraryID, Kind: kind}
	if err := m.db.CreateScanTask(t); err != nil {
		m.mu.Lock()
		delete(m.active, libraryID)
		m.mu.Unlock()
		return nil, err
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("扫描协程崩溃", "task_id", t.ID, "library_id", libraryID)
				_ = m.db.FinishScanTask(t.ID, domain.TaskFailed, "扫描协程异常退出", 0, 0)
			}
			m.mu.Lock()
			delete(m.active, libraryID)
			m.mu.Unlock()
		}()
		m.scanner.Run(m.baseCtx, t, lib)
		m.notifyAfterScan(libraryID, t.ID)
	}()
	return t, nil
}

// Get returns a task by id.
func (m *Manager) Get(id string) (*domain.ScanTask, error) { return m.db.GetScanTask(id) }

// Busy reports whether a library currently has a running scan.
func (m *Manager) Busy(libraryID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[libraryID]
}

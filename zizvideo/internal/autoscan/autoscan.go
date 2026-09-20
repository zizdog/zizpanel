// Package autoscan runs periodic incremental scans (and an optional macOS
// filesystem-event accelerator) by reusing task.Manager, so results and the
// post-scan detection hook stay in the existing task centre.
package autoscan

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
)

// Store is the persistence surface the scheduler needs.
type Store interface {
	GetAutoScanSettings() (*domain.AutoScanSettings, error)
	RecordAutoScanRun(*domain.AutoScanRun) error
	LatestAutoScanRun() (*domain.AutoScanRun, error)
	PruneAutoScanRuns(int) error
	ListEnabledLibraries() ([]domain.Library, error)
	ActiveScanTask(libraryID string) (*domain.ScanTask, error)
	CountMedia(libraryID string) (int, error)
}

// Scans is the scan-start surface, satisfied by *task.Manager.
type Scans interface {
	StartScan(libraryID, kind string) (*domain.ScanTask, error)
	Get(id string) (*domain.ScanTask, error)
	Busy(libraryID string) bool
}

// Status is what the admin endpoint reads back.
type Status struct {
	Settings   *domain.AutoScanSettings `json:"settings"`
	EventsOK   bool                     `json:"events_ok"`
	EventsNote string                   `json:"events_note"`
	LastRun    *domain.AutoScanRun      `json:"last_run"`
	NextRunAt  string                   `json:"next_run_at"`
	Running    bool                     `json:"running"`
	Available  bool                     `json:"available"`
}

type watcherStatus struct {
	ok   bool
	note string
}

// Scheduler owns the timer, the event watcher and the one-run-at-a-time gate.
type Scheduler struct {
	store Store
	scans Scans
	log   *slog.Logger

	newWatcher       func() (Watcher, error)
	debounceOverride time.Duration
	awaitTimeout     time.Duration

	mu           sync.Mutex
	events       watcherStatus
	nextAt       time.Time
	running      bool
	started      bool
	watchedRoots []string
	watcher      Watcher
	cancelW      context.CancelFunc
	cancel       context.CancelFunc
	ctx          context.Context
	triggerCh    chan struct{}
	reloadCh     chan struct{}

	runMu sync.Mutex
	wg    sync.WaitGroup
}

// New builds a scheduler; nothing runs until Start is called.
func New(store Store, scans Scans, log *slog.Logger) *Scheduler {
	return &Scheduler{
		store: store, scans: scans, log: log,
		newWatcher:   defaultWatcher,
		events:       watcherStatus{note: "事件监听未启动"},
		triggerCh:    make(chan struct{}, 1),
		reloadCh:     make(chan struct{}, 1),
		awaitTimeout: 30 * time.Minute,
	}
}

// Start launches the loop, which first runs one incremental scan (startup) so
// files changed while the service was down (or a disk plugged in) are covered.
func (s *Scheduler) Start(parent context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel, s.started, s.ctx = cancel, true, ctx
	s.mu.Unlock()
	s.wg.Add(1)
	go s.loop(ctx)
}

// Stop cancels the loop and the watcher; safe to call twice.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	cancel := s.cancel
	w, wc := s.watcher, s.cancelW
	s.watcher, s.cancelW, s.watchedRoots, s.ctx = nil, nil, nil, nil
	s.mu.Unlock()
	if wc != nil {
		wc()
	}
	if w != nil {
		_ = w.Close()
	}
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// Reload asks the loop to re-read settings and rebuild the watcher; used after
// settings or library changes so the timer/watches match the new reality.
func (s *Scheduler) Reload() {
	select {
	case s.reloadCh <- struct{}{}:
	default:
	}
}

// Status returns settings, watcher availability and the latest run.
func (s *Scheduler) Status() Status {
	st, err := s.store.GetAutoScanSettings()
	if err != nil {
		s.log.Warn("读取自动扫描设置失败", "error", err.Error())
		st = &domain.AutoScanSettings{}
	}
	last, err := s.store.LatestAutoScanRun()
	if err != nil {
		s.log.Warn("读取自动扫描记录失败", "error", err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{Settings: st, EventsOK: s.events.ok, EventsNote: s.events.note,
		LastRun: last, Running: s.running, Available: true}
	if !s.nextAt.IsZero() {
		out.NextRunAt = s.nextAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Scheduler) loop(ctx context.Context) {
	defer s.wg.Done()
	first := true
	for {
		settings, err := s.store.GetAutoScanSettings()
		if err != nil {
			s.log.Error("读取自动扫描设置失败", "error", err.Error())
			if !s.waitOrReload(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if !settings.Enabled {
			// 关闭时不产生任何定时器、不启动监听、不扫描。
			s.stopWatcher("事件监听已关闭（按设置）")
			s.setNext(time.Time{})
			if !s.waitOrReload(ctx, 0) {
				return
			}
			continue
		}
		// 启动即扫一轮：覆盖停机期间的文件变动与刚插上的外置盘。
		s.ensureWatcher(ctx, settings)
		if first {
			first = false
			s.runGuarded(ctx, domain.AutoScanTriggerStartup)
		}
		interval := time.Duration(settings.IntervalMinutes) * time.Minute
		s.setNext(time.Now().Add(interval))
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.runGuarded(ctx, domain.AutoScanTriggerTimer)
		case <-s.triggerCh:
			timer.Stop()
			s.runGuarded(ctx, domain.AutoScanTriggerEvents)
		case <-s.reloadCh:
			timer.Stop()
			continue
		}
	}
}

// waitOrReload blocks for d (0 = forever) or until a reload/ctx event.
func (s *Scheduler) waitOrReload(ctx context.Context, d time.Duration) bool {
	var timer *time.Timer
	var ch <-chan time.Time
	if d > 0 {
		timer = time.NewTimer(d)
		defer timer.Stop()
		ch = timer.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.reloadCh:
		return true
	case <-ch:
		return true
	}
}

// runGuarded performs one loop-driven round. RunOnce owns the runMu gate, so a
// round already in flight becomes an honestly recorded skip.
func (s *Scheduler) runGuarded(ctx context.Context, trigger string) *domain.AutoScanRun {
	run, _ := s.RunOnce(ctx, trigger)
	return run
}

func (s *Scheduler) recordSkipped(trigger string) *domain.AutoScanRun {
	run := &domain.AutoScanRun{Trigger: trigger, Status: domain.AutoScanSkipped,
		Note: "跳过：上一次自动扫描仍在进行"}
	s.fillEvents(run)
	s.record(run)
	return run
}

type startedTask struct {
	libID  string
	taskID string
	before int
}

// roundState carries one round from queueing to its recorded outcome.
type roundState struct {
	run           *domain.AutoScanRun
	started       []startedTask
	reasons       []string
	startFailures int
	final         bool
}

// beginRound decides what to scan and queues incremental tasks. The caller must
// hold runMu; the settings/disabled gates are also re-checked here.
func (s *Scheduler) beginRound(ctx context.Context, trigger string) (*roundState, error) {
	settings, err := s.store.GetAutoScanSettings()
	if err != nil {
		return nil, err
	}
	if !settings.Enabled {
		return nil, domain.ErrAutoScanDisabled
	}
	st := &roundState{run: &domain.AutoScanRun{Trigger: trigger, StartedAt: domain.NowString()}}
	s.fillEvents(st.run)
	s.setRunning(true)

	libs, err := s.store.ListEnabledLibraries()
	if err != nil {
		st.run.Status, st.run.Note = domain.AutoScanFailed, "读取媒体库失败: "+err.Error()
		st.final = true
		return st, err
	}
	st.run.Libraries = len(libs)
	if len(libs) == 0 {
		st.reasons = append(st.reasons, "没有启用的媒体库，跳过")
		return st, nil
	}
	for _, lib := range libs {
		if !RootReadable(lib.RootPath) {
			st.run.Skipped++
			st.reasons = append(st.reasons, "媒体根不可读，跳过："+lib.RootPath)
			continue
		}
		if s.busy(lib.ID) {
			st.run.Skipped++
			st.reasons = append(st.reasons, "跳过：上一轮仍在进行（"+lib.Name+"）")
			continue
		}
		before, _ := s.store.CountMedia(lib.ID)
		task, serr := s.scans.StartScan(lib.ID, "incremental")
		if serr != nil {
			if errors.Is(serr, domain.ErrScanRunning) {
				st.run.Skipped++
				st.reasons = append(st.reasons, "跳过：上一轮仍在进行（"+lib.Name+"）")
				continue
			}
			st.startFailures++
			st.reasons = append(st.reasons, "启动扫描失败（"+lib.Name+"）："+humanErr(serr))
			continue
		}
		st.run.Started++
		st.started = append(st.started, startedTask{libID: lib.ID, taskID: task.ID, before: before})
	}
	return st, nil
}

// finishRound records the counters and the terminal status, then releases the
// running flag. It is the single place a round becomes visible to the admin.
func (s *Scheduler) finishRound(st *roundState, interrupted bool, updated, failed, missing int) *domain.AutoScanRun {
	run := st.run
	if len(st.started) > 0 {
		run.Updated, run.Failed, run.Missing = updated, st.startFailures+failed, missing
		for _, task := range st.started {
			after, cerr := s.store.CountMedia(task.libID)
			if cerr == nil && after > task.before {
				run.NewMedia += after - task.before
			}
		}
	} else {
		run.Failed = st.startFailures
	}
	run.FinishedAt = domain.NowString()
	if !st.final {
		run.Note = joinReasons(st.reasons)
		s.classify(run, interrupted)
	}
	s.setRunning(false)
	s.record(run)
	return run
}

// RunOnce performs one round and waits for its tasks; used by the timer/event
// loop and by tests.
func (s *Scheduler) RunOnce(ctx context.Context, trigger string) (*domain.AutoScanRun, error) {
	if !s.runMu.TryLock() {
		return s.recordSkipped(trigger), nil
	}
	defer s.runMu.Unlock()
	st, err := s.beginRound(ctx, trigger)
	if err != nil {
		s.setRunning(false)
		return nil, err
	}
	var interrupted bool
	var updated, failed, missing int
	if len(st.started) > 0 {
		interrupted, updated, failed, missing = s.waitTasks(ctx, st.started)
	}
	return s.finishRound(st, interrupted, updated, failed, missing), nil
}

// Trigger queues one round and answers immediately with the scan task ids (the
// API returns 202); the round is recorded when its tasks are terminal. It uses
// the scheduler's own context so an HTTP client disconnect cannot cancel scans.
func (s *Scheduler) Trigger(trigger string) (*domain.AutoScanRun, []string, error) {
	if !s.runMu.TryLock() {
		return s.recordSkipped(trigger), nil, nil
	}
	ctx := s.baseContext()
	st, err := s.beginRound(ctx, trigger)
	if err != nil {
		s.setRunning(false)
		s.runMu.Unlock()
		return nil, nil, err
	}
	ids := make([]string, 0, len(st.started))
	for _, task := range st.started {
		ids = append(ids, task.taskID)
	}
	// 没启动任何任务（不可读/被跳过/读取失败）就没有可等的，直接收尾返回；否则
	// 后台协程会改写 st.run，不能把同一个结构体当响应发出去（竞争 + 文案错位）。
	if len(st.started) == 0 {
		run := s.finishRound(st, false, 0, 0, 0)
		s.runMu.Unlock()
		return run, ids, nil
	}
	preview := *st.run
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.runMu.Unlock()
		interrupted, updated, failed, missing := s.waitTasks(ctx, st.started)
		s.finishRound(st, interrupted, updated, failed, missing)
	}()
	return &preview, ids, nil
}

func (s *Scheduler) baseContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// waitTasks polls the started scan tasks until every one is terminal; the
// returned counters are the tasks' final values.
func (s *Scheduler) waitTasks(ctx context.Context, started []startedTask) (interrupted bool, updated, failed, missing int) {
	deadline := time.Now().Add(s.timeout())
	for {
		done := 0
		updated, failed, missing = 0, 0, 0
		for _, st := range started {
			t, err := s.scans.Get(st.taskID)
			if err != nil {
				s.log.Warn("读取扫描任务失败", "task_id", st.taskID, "error", err.Error())
				done++
				continue
			}
			updated += t.Updated
			failed += t.Failed
			missing += t.Missing
			switch t.Status {
			case domain.TaskSuccess, domain.TaskFailed, domain.TaskInterrupted:
				done++
			}
		}
		if done == len(started) {
			return false, updated, failed, missing
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return true, updated, failed, missing
		}
		select {
		case <-ctx.Done():
			return true, updated, failed, missing
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Scheduler) busy(libraryID string) bool {
	if s.scans.Busy(libraryID) {
		return true
	}
	if t, err := s.store.ActiveScanTask(libraryID); err == nil && t != nil {
		return true
	}
	return false
}

// fillEvents copies the current watcher state so every recorded round says
// whether the event accelerator was actually available.
func (s *Scheduler) fillEvents(run *domain.AutoScanRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run.EventsOK, run.EventsNote = s.events.ok, s.events.note
}

func (s *Scheduler) record(run *domain.AutoScanRun) *domain.AutoScanRun {
	if run.StartedAt == "" {
		run.StartedAt = domain.NowString()
	}
	if run.Status == domain.AutoScanSkipped || run.Status == domain.AutoScanFailed {
		if strings.TrimSpace(run.Note) == "" {
			run.Note = "跳过（未给原因）"
		}
	}
	if err := s.store.RecordAutoScanRun(run); err != nil {
		s.log.Error("写入自动扫描记录失败", "trigger", run.Trigger, "error", err.Error())
	}
	if err := s.store.PruneAutoScanRuns(50); err != nil {
		s.log.Warn("清理自动扫描记录失败", "error", err.Error())
	}
	s.log.Info("自动扫描结束", "trigger", run.Trigger, "status", run.Status,
		"libraries", run.Libraries, "started", run.Started, "skipped", run.Skipped,
		"new_media", run.NewMedia, "failed", run.Failed, "note", run.Note,
		"events_ok", run.EventsOK, "events_note", run.EventsNote)
	return run
}

func (s *Scheduler) classify(run *domain.AutoScanRun, interrupted bool) {
	switch {
	case interrupted:
		run.Status = domain.AutoScanFailed
		if run.Note == "" {
			run.Note = "服务停止，本轮扫描未收尾"
		}
	case run.Started == 0 && run.Failed > 0:
		run.Status = domain.AutoScanFailed
	case run.Started == 0 && (run.Skipped > 0 || run.Libraries == 0):
		run.Status = domain.AutoScanSkipped
	case run.Failed > 0 || run.Skipped > 0:
		run.Status = domain.AutoScanPartial
	default:
		run.Status = domain.AutoScanSuccess
	}
}

func (s *Scheduler) setRunning(v bool) {
	s.mu.Lock()
	s.running = v
	s.mu.Unlock()
}

func (s *Scheduler) setNext(at time.Time) {
	s.mu.Lock()
	s.nextAt = at
	s.mu.Unlock()
}

func (s *Scheduler) timeout() time.Duration {
	if s.awaitTimeout > 0 {
		return s.awaitTimeout
	}
	return 30 * time.Minute
}

func (s *Scheduler) debounce() time.Duration {
	if s.debounceOverride > 0 {
		return s.debounceOverride
	}
	st, err := s.store.GetAutoScanSettings()
	if err != nil || st.DebounceSeconds <= 0 {
		return 8 * time.Second
	}
	return time.Duration(st.DebounceSeconds) * time.Second
}

// RootReadable reports whether a library root exists, is a directory and can be
// listed. Unreadable roots must be skipped honestly, never reported as scanned.
func RootReadable(root string) bool {
	f, err := os.Open(root)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.IsDir() {
		return false
	}
	_, err = f.Readdirnames(1)
	return err == nil || errors.Is(err, io.EOF)
}

func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	out := strings.Join(reasons, "；")
	if len(out) > 300 {
		out = out[:300]
	}
	return out
}

func humanErr(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return err.Error()
}

// IgnoreRulesFor keeps the watcher and the scanner agreeing on which directory
// names to skip; media.DefaultIgnoreRules is the shared floor.
func IgnoreRulesFor(lib *domain.Library) []string {
	return append(append([]string{}, media.DefaultIgnoreRules...), lib.IgnoreRules...)
}

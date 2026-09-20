package autoscan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/storage"
)

// ============================================================================
//  测试替身：扫描启动器 / 事件监听器
// ============================================================================

type fakeScans struct {
	mu      sync.Mutex
	started []string
	busy    map[string]bool
	running map[string]bool
	fail    map[string]error
	tasks   map[string]*domain.ScanTask
	finish  bool
	n       int
}

func newFakeScans(finish bool) *fakeScans {
	return &fakeScans{busy: map[string]bool{}, running: map[string]bool{},
		fail: map[string]error{}, tasks: map[string]*domain.ScanTask{}, finish: finish}
}

func (f *fakeScans) StartScan(libraryID, kind string) (*domain.ScanTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[libraryID] {
		return nil, domain.ErrScanRunning
	}
	if err, ok := f.fail[libraryID]; ok {
		return nil, err
	}
	f.n++
	id := fmt.Sprintf("scn_%d", f.n)
	t := &domain.ScanTask{ID: id, LibraryID: libraryID, Kind: kind, Status: domain.TaskRunning}
	if f.finish {
		t.Status, t.Updated = domain.TaskSuccess, 2
	}
	f.tasks[id] = t
	f.started = append(f.started, libraryID)
	f.busy[libraryID] = true
	return t, nil
}

func (f *fakeScans) Get(id string) (*domain.ScanTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, domain.ErrTaskNotFound
	}
	cp := *t
	return &cp, nil
}

func (f *fakeScans) Busy(libraryID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy[libraryID]
}

func (f *fakeScans) startedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

type fakeWatcher struct {
	events chan fsnotify.Event
	errs   chan error
	addErr error
	once   sync.Once
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{events: make(chan fsnotify.Event, 64), errs: make(chan error, 4)}
}

func (f *fakeWatcher) Add(string) error    { return f.addErr }
func (f *fakeWatcher) Remove(string) error { return nil }
func (f *fakeWatcher) Events() <-chan fsnotify.Event {
	return f.events
}
func (f *fakeWatcher) Errors() <-chan error { return f.errs }
func (f *fakeWatcher) Close() error         { f.once.Do(func() {}); return nil }

// ============================================================================
//  夹具
// ============================================================================

func newTestEnv(t *testing.T, finish bool) (*Scheduler, *storage.DB, *fakeScans, string) {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	db, err := storage.Open(filepath.Join(dir, "zv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	root := filepath.Join(dir, "media")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	scans := newFakeScans(finish)
	s := New(db, scans, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.awaitTimeout = 5 * time.Second
	return s, db, scans, root
}

func addLib(t *testing.T, db *storage.DB, name, root string) *domain.Library {
	t.Helper()
	lib := &domain.Library{ID: domain.NewID("lib"), Name: name, RootPath: root,
		Recursive: true, Enabled: true, IgnoreRules: []string{}}
	if err := db.CreateLibrary(lib); err != nil {
		t.Fatal(err)
	}
	return lib
}

func saveSettings(t *testing.T, db *storage.DB, s *domain.AutoScanSettings) {
	t.Helper()
	if err := db.SaveAutoScanSettings(s); err != nil {
		t.Fatal(err)
	}
}

func enabledSettings() *domain.AutoScanSettings {
	return &domain.AutoScanSettings{Enabled: true, IntervalMinutes: 5, EventsEnabled: true, DebounceSeconds: 8}
}

// ============================================================================
//  门禁 1：设置校验（越界拒绝，不许静默夹取）
// ============================================================================

func TestAutoScanSettingsBounds(t *testing.T) {
	_, db, _, _ := newTestEnv(t, true)
	if err := db.SaveAutoScanSettings(&domain.AutoScanSettings{Enabled: true, IntervalMinutes: 5,
		EventsEnabled: true, DebounceSeconds: 8}); err != nil {
		t.Fatal(err)
	}
	rejects := []struct {
		name string
		s    *domain.AutoScanSettings
		want error
	}{
		{"间隔 1441 拒绝", &domain.AutoScanSettings{IntervalMinutes: 1441, DebounceSeconds: 8}, domain.ErrAutoScanInterval},
		{"间隔 -1 拒绝", &domain.AutoScanSettings{IntervalMinutes: -1, DebounceSeconds: 8}, domain.ErrAutoScanInterval},
		{"去抖 4 拒绝", &domain.AutoScanSettings{IntervalMinutes: 5, DebounceSeconds: 4}, domain.ErrAutoScanDebounce},
		{"去抖 11 拒绝", &domain.AutoScanSettings{IntervalMinutes: 5, DebounceSeconds: 11}, domain.ErrAutoScanDebounce},
	}
	for _, tc := range rejects {
		if err := db.SaveAutoScanSettings(tc.s); !errors.Is(err, tc.want) {
			t.Fatalf("%s: 期望 %v，得到 %v", tc.name, tc.want, err)
		}
		got, err := db.GetAutoScanSettings()
		if err != nil {
			t.Fatal(err)
		}
		if got.IntervalMinutes != 5 || got.DebounceSeconds != 8 {
			t.Fatalf("%s 越界写入污染了设置: %+v", tc.name, got)
		}
	}
	// 边界值接受。
	for _, ok := range []*domain.AutoScanSettings{
		{IntervalMinutes: 1, DebounceSeconds: 5},
		{IntervalMinutes: 1440, DebounceSeconds: 10},
	} {
		if err := db.SaveAutoScanSettings(ok); err != nil {
			t.Fatalf("边界值 %+v 被拒: %v", ok, err)
		}
	}
}

// ============================================================================
//  门禁 2：开关关闭 → 不产生定时器、不扫描
// ============================================================================

func TestDisabledRunsNothing(t *testing.T) {
	s, db, scans, root := newTestEnv(t, true)
	addLib(t, db, "A", root)
	off := enabledSettings()
	off.Enabled = false
	saveSettings(t, db, off)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	time.Sleep(400 * time.Millisecond)
	s.Stop()
	if n := scans.startedCount(); n != 0 {
		t.Fatalf("开关关闭仍启动了 %d 次扫描", n)
	}
	if _, err := s.RunOnce(context.Background(), domain.AutoScanTriggerManual); !errors.Is(err, domain.ErrAutoScanDisabled) {
		t.Fatalf("关闭时 RunOnce 期望 ErrAutoScanDisabled，得到 %v", err)
	}
	if _, err := db.LatestAutoScanRun(); err != nil {
		t.Fatal(err)
	}
}

// ============================================================================
//  门禁 3：不重叠（Busy 与 ErrScanRunning 两条路径都要如实记跳过）
// ============================================================================

func TestNoOverlapSkipsBusyLibrary(t *testing.T) {
	s, db, scans, root := newTestEnv(t, true)
	lib := addLib(t, db, "A", root)
	saveSettings(t, db, enabledSettings())
	scans.busy[lib.ID] = true // 注入：上一轮还在跑

	run, err := s.RunOnce(context.Background(), domain.AutoScanTriggerTimer)
	if err != nil {
		t.Fatal(err)
	}
	if run.Started != 0 || run.Skipped != 1 {
		t.Fatalf("期望 started=0 skipped=1，得到 %+v", run)
	}
	if run.Status != domain.AutoScanSkipped {
		t.Fatalf("期望 status=skipped（不是 success），得到 %q", run.Status)
	}
	if !strings.Contains(run.Note, "上一轮仍在进行") {
		t.Fatalf("跳过原因不真实: %q", run.Note)
	}
	latest, err := db.LatestAutoScanRun()
	if err != nil || latest == nil {
		t.Fatalf("跳过没有落库: %v %v", latest, err)
	}
	if latest.Status != domain.AutoScanSkipped || !strings.Contains(latest.Note, "上一轮仍在进行") {
		t.Fatalf("落库的跳过记录不真实: %+v", latest)
	}
}

func TestNoOverlapSkipsOnScanRunningError(t *testing.T) {
	s, db, scans, root := newTestEnv(t, true)
	lib := addLib(t, db, "A", root)
	saveSettings(t, db, enabledSettings())
	scans.running[lib.ID] = true // StartScan 会返回 ErrScanRunning

	run, err := s.RunOnce(context.Background(), domain.AutoScanTriggerTimer)
	if err != nil {
		t.Fatal(err)
	}
	if run.Started != 0 || run.Skipped != 1 || !strings.Contains(run.Note, "上一轮仍在进行") {
		t.Fatalf("ErrScanRunning 未被如实记为跳过: %+v", run)
	}
}

// ============================================================================
//  门禁 4：启动即扫一次（成功路径）
// ============================================================================

func TestStartupRunsOnce(t *testing.T) {
	s, db, scans, root := newTestEnv(t, true)
	addLib(t, db, "A", root)
	saveSettings(t, db, enabledSettings())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if run, err := db.LatestAutoScanRun(); err == nil && run != nil &&
			run.Trigger == domain.AutoScanTriggerStartup {
			if run.Status != domain.AutoScanSuccess || run.Started != 1 {
				t.Fatalf("启动轮结果不对: %+v", run)
			}
			s.Stop()
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	s.Stop()
	t.Fatalf("启动后未自动跑一轮（scans=%d）", scans.startedCount())
}

// ============================================================================
//  门禁 5：媒体根不可读 → 如实跳过，负向对照不许报成功
// ============================================================================

func TestUnreadableRootSkippedNotSuccess(t *testing.T) {
	s, db, scans, root := newTestEnv(t, true)
	addLib(t, db, "A", root) // 可读，作为对照：它必须被扫
	bad := filepath.Join(filepath.Dir(root), "not-mounted")
	addLib(t, db, "B", bad)
	saveSettings(t, db, enabledSettings())

	run, err := s.RunOnce(context.Background(), domain.AutoScanTriggerStartup)
	if err != nil {
		t.Fatal(err)
	}
	if run.Started != 1 || run.Skipped != 1 {
		t.Fatalf("期望 1 个库扫、1 个库跳过，得到 %+v", run)
	}
	if !strings.Contains(run.Note, "媒体根不可读") {
		t.Fatalf("不可读原因不真实: %q", run.Note)
	}
	if run.Status == domain.AutoScanSuccess {
		t.Fatalf("混合结果不得报成功: %+v", run)
	}

	// 负向对照：只剩不可读的库时，状态必须是 skipped，绝不是 success。
	libs, _ := db.ListEnabledLibraries()
	for i := range libs {
		if libs[i].RootPath == root {
			off := false
			if _, err := db.UpdateLibrary(libs[i].ID, storage.LibraryPatch{Enabled: &off}); err != nil {
				t.Fatal(err)
			}
		}
	}
	run2, err := s.RunOnce(context.Background(), domain.AutoScanTriggerTimer)
	if err != nil {
		t.Fatal(err)
	}
	if run2.Status != domain.AutoScanSkipped || run2.Started != 0 {
		t.Fatalf("全不可读时必须是 skipped/0，得到 %+v", run2)
	}
	if scans.startedCount() != 1 {
		t.Fatalf("不可读的库不得启动扫描，实际 %d 次", scans.startedCount())
	}
}

// ============================================================================
//  门禁 6：事件去抖 —— 连续多次变动只触发一次
// ============================================================================

func TestEventBurstTriggersOnce(t *testing.T) {
	s, db, _, root := newTestEnv(t, true)
	addLib(t, db, "A", root)
	saveSettings(t, db, enabledSettings())
	s.debounceOverride = 80 * time.Millisecond
	fw := newFakeWatcher()
	s.newWatcher = func() (Watcher, error) { return fw, nil }

	s.startWatcher(context.Background(), []watchRoot{{path: root}})
	defer s.stopWatcher("")

	for i := 0; i < 6; i++ {
		fw.events <- fsnotify.Event{Name: filepath.Join(root, fmt.Sprintf("v%d.mp4", i)), Op: fsnotify.Create}
	}
	time.Sleep(300 * time.Millisecond)
	if got := drainTriggers(s); got != 1 {
		t.Fatalf("连续 6 次变动触发了 %d 次，期望 1 次", got)
	}
	// 再变动一次，应再触发一次（去抖不是只触发一次就死掉）。
	fw.events <- fsnotify.Event{Name: filepath.Join(root, "later.mp4"), Op: fsnotify.Create}
	time.Sleep(250 * time.Millisecond)
	if got := drainTriggers(s); got != 1 {
		t.Fatalf("第二次变动触发 %d 次，期望 1 次", got)
	}
}

func drainTriggers(s *Scheduler) int {
	n := 0
	for {
		select {
		case <-s.triggerCh:
			n++
		default:
			return n
		}
	}
}

// ============================================================================
//  门禁 7：监听不可用/注册失败 → 降级为只定时，状态如实
// ============================================================================

func TestWatcherUnavailableDegradesHonestly(t *testing.T) {
	s, db, _, root := newTestEnv(t, true)
	addLib(t, db, "A", root)
	saveSettings(t, db, enabledSettings())
	s.newWatcher = func() (Watcher, error) { return nil, errors.New("kqueue: too many open files") }

	settings, err := db.GetAutoScanSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.ensureWatcher(context.Background(), settings)
	st := s.Status()
	if st.EventsOK {
		t.Fatalf("注册失败却报监听可用: %+v", st)
	}
	if !strings.Contains(st.EventsNote, "事件监听不可用") {
		t.Fatalf("监听失败原因不真实: %q", st.EventsNote)
	}

	// 定时路径不受影响：RunOnce 仍然真的扫，并且记录里如实写监听不可用。
	run, err := s.RunOnce(context.Background(), domain.AutoScanTriggerTimer)
	if err != nil {
		t.Fatal(err)
	}
	if run.Started != 1 || run.Status != domain.AutoScanSuccess {
		t.Fatalf("监听失败后定时扫描应照常工作: %+v", run)
	}
	latest, err := db.LatestAutoScanRun()
	if err != nil || latest == nil {
		t.Fatalf("记录缺失: %v %v", latest, err)
	}
	if latest.EventsOK || latest.EventsNote == "" {
		t.Fatalf("记录必须如实写监听不可用: %+v", latest)
	}
}

func TestWatcherAddFailureDegrades(t *testing.T) {
	s, db, _, root := newTestEnv(t, true)
	addLib(t, db, "A", root)
	saveSettings(t, db, enabledSettings())
	fw := newFakeWatcher()
	fw.addErr = errors.New("kqueue: no space")
	s.newWatcher = func() (Watcher, error) { return fw, nil }

	settings, _ := db.GetAutoScanSettings()
	s.ensureWatcher(context.Background(), settings)
	st := s.Status()
	if st.EventsOK || !strings.Contains(st.EventsNote, "事件监听不可用") {
		t.Fatalf("注册失败未如实降级: %+v", st)
	}
}

func TestEventsDisabledIsHonest(t *testing.T) {
	s, db, _, root := newTestEnv(t, true)
	addLib(t, db, "A", root)
	cfg := enabledSettings()
	cfg.EventsEnabled = false
	saveSettings(t, db, cfg)
	s.ensureWatcher(context.Background(), cfg)
	st := s.Status()
	if st.EventsOK {
		t.Fatalf("关闭事件监听却报可用: %+v", st)
	}
	if !strings.Contains(st.EventsNote, "已关闭") {
		t.Fatalf("关闭原因不真实: %q", st.EventsNote)
	}
}

// ============================================================================
//  门禁 8：落库如实语义（空原因/假成功一律拒绝）
// ============================================================================

func TestRecordHonestSemantics(t *testing.T) {
	_, db, _, _ := newTestEnv(t, true)
	cases := []struct {
		name string
		run  *domain.AutoScanRun
	}{
		{"skipped 无原因", &domain.AutoScanRun{Status: domain.AutoScanSkipped, EventsOK: true}},
		{"failed 无原因", &domain.AutoScanRun{Status: domain.AutoScanFailed, EventsOK: true}},
		{"partial 无失败也无跳过", &domain.AutoScanRun{Status: domain.AutoScanPartial, Note: "x", EventsOK: true}},
		{"events_ok=false 无原因", &domain.AutoScanRun{Status: domain.AutoScanSuccess, EventsOK: false}},
	}
	for _, tc := range cases {
		if err := db.RecordAutoScanRun(tc.run); err == nil {
			t.Fatalf("%s: 应拒绝写入", tc.name)
		}
	}
	ok := &domain.AutoScanRun{Status: domain.AutoScanSuccess, EventsOK: true, Trigger: domain.AutoScanTriggerStartup}
	if err := db.RecordAutoScanRun(ok); err != nil {
		t.Fatalf("合法记录被拒: %v", err)
	}
}

package autoscan

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
)

// Watcher is the subset of fsnotify.Watcher the scheduler uses; it is an
// interface so tests can inject registration failures (degradation gate).
type Watcher interface {
	Add(dir string) error
	Remove(dir string) error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

// defaultWatcher uses fsnotify's kqueue backend on macOS: pure Go, no cgo, so
// CGO_ENABLED=0 release builds keep working. kqueue watches each directory, so a
// huge tree can exhaust the open-file limit — registration failure degrades to
// timer-only with the real reason recorded, never a silent fake success.
func defaultWatcher() (Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return fsWatcher{w}, nil
}

// fsWatcher adapts fsnotify's channel fields to the small interface used here.
type fsWatcher struct{ *fsnotify.Watcher }

func (f fsWatcher) Events() <-chan fsnotify.Event { return f.Watcher.Events }
func (f fsWatcher) Errors() <-chan error          { return f.Watcher.Errors }

type watchRoot struct {
	path  string
	rules []string
}

// ensureWatcher (re)builds the event watcher to match the current libraries.
func (s *Scheduler) ensureWatcher(ctx context.Context, settings *domain.AutoScanSettings) {
	if !settings.EventsEnabled {
		s.stopWatcher("事件监听已关闭（按设置）")
		return
	}
	libs, err := s.store.ListEnabledLibraries()
	if err != nil {
		s.stopWatcher("事件监听不可用（读取媒体库失败：" + err.Error() + "）")
		return
	}
	roots := make([]watchRoot, 0, len(libs))
	for i := range libs {
		roots = append(roots, watchRoot{path: libs[i].RootPath, rules: IgnoreRulesFor(&libs[i])})
	}
	if s.watcherMatches(roots) {
		return
	}
	s.stopWatcher("")
	s.startWatcher(ctx, roots)
}

func (s *Scheduler) watcherMatches(roots []watchRoot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watcher == nil || len(s.watchedRoots) != len(roots) {
		return false
	}
	for i := range roots {
		if s.watchedRoots[i] != roots[i].path {
			return false
		}
	}
	return true
}

func (s *Scheduler) startWatcher(ctx context.Context, roots []watchRoot) {
	w, err := s.newWatcher()
	if err != nil {
		s.stopWatcher("事件监听不可用（" + err.Error() + "）")
		return
	}
	watched := make([]string, 0, len(roots))
	for _, r := range roots {
		dirs, derr := collectDirs(r.path, r.rules)
		if derr != nil {
			_ = w.Close()
			s.stopWatcher("事件监听不可用（" + derr.Error() + "）")
			return
		}
		for _, dir := range dirs {
			if aerr := w.Add(dir); aerr != nil {
				_ = w.Close()
				s.stopWatcher("事件监听不可用（" + aerr.Error() + "）")
				return
			}
			watched = append(watched, dir)
		}
	}
	wctx, wcancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.watcher, s.cancelW = w, wcancel
	s.watchedRoots = make([]string, len(roots))
	for i := range roots {
		s.watchedRoots[i] = roots[i].path
	}
	s.events = watcherStatus{ok: true}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pumpEvents(wctx, w)
	}()
	s.log.Info("文件事件监听已启用", "roots", len(roots), "dirs", len(watched))
}

// stopWatcher tears the watcher down; a non-empty note is recorded as the
// honest reason (e.g. 监听不可用 / 已关闭).
func (s *Scheduler) stopWatcher(note string) {
	s.mu.Lock()
	w, wc := s.watcher, s.cancelW
	s.watcher, s.cancelW, s.watchedRoots = nil, nil, nil
	if note != "" {
		s.events = watcherStatus{ok: false, note: note}
	}
	s.mu.Unlock()
	if wc != nil {
		wc()
	}
	if w != nil {
		_ = w.Close()
	}
}

func (s *Scheduler) setEvents(ok bool, note string) {
	s.mu.Lock()
	s.events = watcherStatus{ok: ok, note: note}
	s.mu.Unlock()
}

// pumpEvents debounces a burst of changes into exactly one trigger.
func (s *Scheduler) pumpEvents(ctx context.Context, w Watcher) {
	debounce := s.debounce()
	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = w.Add(ev.Name)
				}
			}
			resetTimer(timer, debounce)
		case <-timer.C:
			s.emitEventTrigger()
		case err, ok := <-w.Errors():
			if !ok {
				return
			}
			// 监听出错不停止定时扫描，但状态必须如实写出来。
			s.setEvents(false, "事件监听异常（"+err.Error()+"），已退回定时扫描")
		}
	}
}

func (s *Scheduler) emitEventTrigger() {
	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// collectDirs lists every directory under root that the scanner would walk.
func collectDirs(root string, rules []string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && media.IgnoredName(d.Name(), rules) {
			return filepath.SkipDir
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

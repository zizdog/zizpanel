package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentTasksAndFailureRecorded：并发跑任务，失败要如实记 error，结果可查。
func TestConcurrentTasksAndFailureRecorded(t *testing.T) {
	m := NewManager()
	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var tk *Task
			if i%2 == 0 {
				tk = m.Start("demo.ok", fmt.Sprintf("成功任务%d", i), func(ctx context.Context, log Logger) (any, error) {
					log("开始")
					log(fmt.Sprintf("第 %d 号", i))
					return map[string]any{"index": i}, nil
				})
			} else {
				tk = m.Start("demo.fail", fmt.Sprintf("失败任务%d", i), func(ctx context.Context, log Logger) (any, error) {
					log("开始")
					return nil, errors.New("真实失败原因")
				})
			}
			ids[i] = tk.ID()
		}(i)
	}
	wg.Wait()

	for i, id := range ids {
		tk, ok := m.Get(id)
		if !ok {
			t.Fatalf("任务 %s 查不到", id)
		}
		select {
		case <-tk.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("任务 %s 未在 5s 内结束", id)
		}
		snap := tk.Snapshot()
		if i%2 == 0 {
			if snap.Status != Succeeded {
				t.Errorf("任务 %d 应成功，实际 %s（%s）", i, snap.Status, snap.Error)
			}
			if snap.Result == nil {
				t.Errorf("任务 %d 成功后应能查到结果", i)
			}
		} else {
			if snap.Status != Failed {
				t.Errorf("任务 %d 应失败，实际 %s", i, snap.Status)
			}
			if !strings.Contains(snap.Error, "真实失败原因") {
				t.Errorf("任务 %d 的 error 应如实记录，实际 %q", i, snap.Error)
			}
			if snap.Result != nil {
				t.Errorf("失败任务不该有成功结果")
			}
		}
		if snap.LineCount == 0 {
			t.Errorf("任务 %d 应有进度日志", i)
		}
	}
	if got := len(m.List(100)); got != n {
		t.Errorf("列表应有 %d 条，实际 %d", n, got)
	}
}

// TestCancelTask：取消要真的让任务结束，并标记 canceled。
func TestCancelTask(t *testing.T) {
	m := NewManager()
	started := make(chan struct{})
	tk := m.Start("demo.slow", "慢任务", func(ctx context.Context, log Logger) (any, error) {
		log("开始")
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return "不该走到这里", nil
		}
	})
	<-started
	if !m.Cancel(tk.ID()) {
		t.Fatal("运行中的任务应可取消")
	}
	select {
	case <-tk.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("取消后任务应很快结束")
	}
	if snap := tk.Snapshot(); snap.Status != Canceled {
		t.Fatalf("状态应为 canceled，实际 %s", snap.Status)
	}
	if m.Cancel(tk.ID()) {
		t.Fatal("已结束的任务不该能再取消")
	}
}

// TestPanicRecordedAsFailure：执行体 panic 不能把服务带走，要记成失败。
func TestPanicRecordedAsFailure(t *testing.T) {
	m := NewManager()
	tk := m.Start("demo.panic", "panic 任务", func(ctx context.Context, log Logger) (any, error) {
		panic("boom")
	})
	<-tk.Done()
	snap := tk.Snapshot()
	if snap.Status != Failed {
		t.Fatalf("panic 应记为失败，实际 %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "panic") {
		t.Fatalf("失败原因应含 panic，实际 %q", snap.Error)
	}
}

// TestLogRingBufferBounded：日志有上限，不会无限增长。
func TestLogRingBufferBounded(t *testing.T) {
	m := NewManager()
	done := make(chan struct{})
	tk := m.Start("demo.log", "日志任务", func(ctx context.Context, log Logger) (any, error) {
		defer close(done)
		for i := 0; i < maxLines+50; i++ {
			log(fmt.Sprintf("行 %d", i))
		}
		return nil, nil
	})
	<-done
	<-tk.Done()
	snap := tk.Snapshot()
	if len(snap.Logs) > maxLines {
		t.Fatalf("日志应被限制在 %d 行，实际 %d", maxLines, len(snap.Logs))
	}
	if snap.LineCount <= int64(maxLines) {
		t.Fatalf("LineCount 应记录总行数，实际 %d", snap.LineCount)
	}
	if !strings.Contains(snap.Last, "行") {
		t.Fatalf("Last 应是最新一行，实际 %q", snap.Last)
	}
}

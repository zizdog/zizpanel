package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件的测试全部用注入的 now/tick，**不等待真实时间**。
// 每日循环最容易出的错是"同一天重复触发"与"到点前就触发"，
// 这两条都能用假时钟钉死。

func fixedClock(t *testing.T, times ...time.Time) func() time.Time {
	t.Helper()
	i := 0
	return func() time.Time {
		if i >= len(times) {
			return times[len(times)-1]
		}
		v := times[i]
		i++
		return v
	}
}

func at(y int, mo time.Month, d, h, m int) time.Time {
	return time.Date(y, mo, d, h, m, 0, 0, time.Local)
}

func TestDailyRunnerRunsOncePerDayAtScheduledTime(t *testing.T) {
	runs := 0
	r := NewDailyRunner(3, 17, []DailyTask{{
		Name: "t",
		Run:  func(context.Context) error { runs++; return nil },
	}}, nil)

	r.now = fixedClock(t, at(2026, 9, 16, 3, 0))
	r.tickOnce(context.Background())
	if runs != 0 {
		t.Fatalf("到点之前不该执行，实际执行 %d 次", runs)
	}

	r.now = fixedClock(t, at(2026, 9, 16, 3, 17))
	r.tickOnce(context.Background())
	if runs != 1 {
		t.Fatalf("到点后应执行一次，实际 %d 次", runs)
	}

	// 同一天再 tick 多次也不能重复执行。
	r.now = fixedClock(t, at(2026, 9, 16, 3, 18), at(2026, 9, 16, 23, 59))
	r.tickOnce(context.Background())
	r.tickOnce(context.Background())
	if runs != 1 {
		t.Fatalf("同一天重复触发了，实际 %d 次", runs)
	}

	// 第二天（含"错过时刻"的补跑语义）会再执行一次。
	r.now = fixedClock(t, at(2026, 9, 17, 4, 30))
	r.tickOnce(context.Background())
	if runs != 2 {
		t.Fatalf("第二天应再执行一次，实际 %d 次", runs)
	}
}

func TestDailyRunnerReportsTaskFailure(t *testing.T) {
	var logged []string
	r := NewDailyRunner(3, 17, []DailyTask{{
		Name: "bad",
		Run:  func(context.Context) error { return errors.New("ca 拒绝了请求") },
	}}, func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})

	out := r.RunTasks(context.Background())
	if len(out) != 1 || out[0].Name != "bad" {
		t.Fatalf("结果形状不对: %+v", out)
	}
	if out[0].Err == nil {
		t.Fatal("任务返回错误时 DailyOutcome.Err 必须非空（失败不许被吞掉）")
	}
	// 失败必须留痕：调用方即使不处理返回值，日志里也要有。
	if joined := strings.Join(logged, "\n"); !strings.Contains(joined, "ca 拒绝了请求") {
		t.Fatalf("失败没有被记录到日志: %q", joined)
	}
}

func TestDailyRunnerNextRunIsStrictlyFuture(t *testing.T) {
	r := NewDailyRunner(3, 17, nil, nil)

	got := r.NextRun(at(2026, 9, 16, 1, 0))
	want := at(2026, 9, 16, 3, 17)
	if !got.Equal(want) {
		t.Fatalf("凌晨应返回当天 3:17，得到 %s", got)
	}

	// 已经过点 → 明天
	got = r.NextRun(at(2026, 9, 16, 3, 17))
	want = at(2026, 9, 17, 3, 17)
	if !got.Equal(want) {
		t.Fatalf("正好在触发时刻应返回次日，得到 %s", got)
	}

	got = r.NextRun(at(2026, 9, 16, 23, 0))
	if !got.Equal(want) {
		t.Fatalf("夜里应返回次日 3:17，得到 %s", got)
	}
}

func TestNewDailyRunnerClampsBadTime(t *testing.T) {
	r := NewDailyRunner(99, -5, nil, nil)
	if r.hour != 3 || r.minute != 0 {
		t.Fatalf("非法时刻应回落到 3:00，得到 %d:%d", r.hour, r.minute)
	}
}

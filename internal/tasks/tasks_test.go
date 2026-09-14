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

// waitStatus 等任务到达终态（带超时，避免测试挂死）。
func waitStatus(t *testing.T, task *Task, want Status) {
	t.Helper()
	select {
	case <-task.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("等任务结束超时（当前状态 %s）", task.Status())
	}
	if got := task.Status(); got != want {
		t.Fatalf("任务状态应为 %s，实际 %s（error=%q）", want, got, task.Meta().Error)
	}
}

func TestTaskSuccessRecordsResultAndLines(t *testing.T) {
	m := NewManager()
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		log(LevelStep, "第一步")
		log(LevelOut, "正在下载")
		return map[string]any{"token": "abc"}, nil
	})
	waitStatus(t, task, StatusSucceeded)

	meta := task.Meta()
	if meta.Title != "安装 Demo" || meta.Kind != "install" || meta.Target != "demo" {
		t.Errorf("元信息不对: %+v", meta)
	}
	if meta.FinishedAt.IsZero() {
		t.Error("结束后 finished_at 应该有值")
	}
	if meta.ElapsedMS < 0 {
		t.Error("elapsed_ms 不该为负")
	}
	if meta.Result == nil {
		t.Error("成功任务应保留业务返回值（前端要用它展示 token/address）")
	}

	lines, next, hasMore, oldest := task.Snapshot(0, 100)
	if hasMore {
		t.Error("只有几行时不该有 has_more")
	}
	if oldest != 1 {
		t.Errorf("最老的行 seq 应为 1，实际 %d", oldest)
	}
	text := joinText(lines)
	for _, want := range []string{"第一步", "正在下载", "任务完成"} {
		if !strings.Contains(text, want) {
			t.Errorf("日志里缺少 %q：\n%s", want, text)
		}
	}
	if next != int64(len(lines)) {
		t.Errorf("next_after 应等于最后一行 seq（%d），实际 %d", len(lines), next)
	}
}

func TestTaskFailureKeepsErrorAndSteps(t *testing.T) {
	m := NewManager()
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		log(LevelStep, "已走到第二步")
		return nil, errors.New("brew install 失败: 404")
	})
	waitStatus(t, task, StatusFailed)

	meta := task.Meta()
	if !strings.Contains(meta.Error, "brew install 失败") {
		t.Errorf("失败原因应保留，实际 %q", meta.Error)
	}
	// 关键：失败时**已经走完的步骤**必须还在，否则用户不知道卡在哪一步
	if !strings.Contains(joinText(mustLines(task)), "已走到第二步") {
		t.Error("失败任务的日志里应保留已完成的步骤")
	}
}

// TestTaskCancelReportsCanceledNotFailed 锁住一个体验细节：
// 中断时子进程返回的通常是 "signal: killed"，那不是"安装失败"。
func TestTaskCancelReportsCanceledNotFailed(t *testing.T) {
	m := NewManager()
	started := make(chan struct{})
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, fmt.Errorf("signal: killed")
	})
	<-started
	if _, err := m.Cancel(task.ID()); err != nil {
		t.Fatalf("中断失败: %v", err)
	}
	waitStatus(t, task, StatusCanceled)
	if !strings.Contains(joinText(mustLines(task)), "已被中断") {
		t.Error("日志里应说明任务被中断")
	}
}

// TestTaskCancelRejectsFinished 已结束的任务不能"假装中断成功"。
func TestTaskCancelRejectsFinished(t *testing.T) {
	m := NewManager()
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		return nil, nil
	})
	waitStatus(t, task, StatusSucceeded)
	if _, err := m.Cancel(task.ID()); err == nil {
		t.Error("已结束的任务应当拒绝中断（否则前端会显示成「已中断」）")
	}
	if _, err := m.Cancel("t-不存在"); err == nil {
		t.Error("不存在的任务应当返回错误")
	}
}

// TestTaskRecoversPanic 是安全网：任务体跑在普通 goroutine 里，
// 一旦 panic 没兜住，整个面板进程都会退出。
func TestTaskRecoversPanic(t *testing.T) {
	m := NewManager()
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		panic("模拟安装代码里的空指针")
	})
	waitStatus(t, task, StatusFailed)
	if !strings.Contains(task.Meta().Error, "panic") {
		t.Errorf("panic 应被记录成失败原因，实际 %q", task.Meta().Error)
	}
}

// TestTaskRingBufferKeepsTail 锁住有界缓冲：任务日志不能无限增长。
func TestTaskRingBufferKeepsTail(t *testing.T) {
	m := NewManager()
	total := maxLinesPerTask + 500
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		for i := 0; i < total; i++ {
			log(LevelOut, fmt.Sprintf("行 %d", i))
		}
		return nil, nil
	})
	waitStatus(t, task, StatusSucceeded)

	meta := task.Meta()
	// 1（任务已创建）+ total + 1（任务完成）
	wantCount := int64(total + 2)
	if meta.LineCount != wantCount {
		t.Errorf("line_count 应统计所有写过的行（%d），实际 %d", wantCount, meta.LineCount)
	}
	lines, _, _, oldest := task.Snapshot(0, maxLinesPerTask+10)
	if len(lines) != maxLinesPerTask {
		t.Errorf("缓冲里应只保留 %d 行，实际 %d", maxLinesPerTask, len(lines))
	}
	// 最老的行 seq 必须如实反映"前面已经滚出缓冲"，而不是假装日志完整
	if want := wantCount - maxLinesPerTask + 1; oldest != want {
		t.Errorf("oldest_seq 应为 %d，实际 %d", want, oldest)
	}
	if !strings.Contains(lines[len(lines)-1].Text, "任务完成") {
		t.Error("尾部应保留最新的行")
	}
}

func TestTaskSnapshotPagination(t *testing.T) {
	m := NewManager()
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		for i := 0; i < 10; i++ {
			log(LevelOut, fmt.Sprintf("行 %d", i))
		}
		return nil, nil
	})
	waitStatus(t, task, StatusSucceeded)

	first, next, hasMore, _ := task.Snapshot(0, 3)
	if len(first) != 3 || !hasMore {
		t.Fatalf("第一页应为 3 行且还有更多，实际 %d 行 hasMore=%v", len(first), hasMore)
	}
	second, _, _, _ := task.Snapshot(next, 3)
	if len(second) != 3 || second[0].Seq != first[2].Seq+1 {
		t.Fatalf("第二页应从 %d 开始连续，实际首条 seq=%d", first[2].Seq+1, second[0].Seq)
	}
}

// TestTaskSubscribeReceivesLiveAndAfterSubscribe 验证 SSE 依赖的两个语义：
// 订阅后能收到新行；"先订阅再取快照"不会丢行（重复靠 seq 去重）。
func TestTaskSubscribeReceivesLiveAndAfterSubscribe(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		log(LevelStep, "订阅前的行")
		<-release
		log(LevelOut, "订阅后的行")
		return nil, nil
	})
	// 等第一行写完
	// 注意：Start 会先写一行"任务已创建"（seq=1），所以第一行业务日志是 seq=2
	waitFor(t, func() bool { return task.Meta().LineCount >= 2 }, "订阅前的行")

	subID, ch := task.Subscribe()
	defer task.Unsubscribe(subID)
	snapshot, _, _, _ := task.Snapshot(1, 100)
	close(release)

	var got []string
	deadline := time.After(3 * time.Second)
	for len(got) < 1 {
		select {
		case l, open := <-ch:
			if !open {
				t.Fatal("订阅通道被意外关闭")
			}
			if l.Seq > 1 { // 与快照去重（处理器用 cursor 做同一件事）
				got = append(got, l.Text)
			}
		case <-deadline:
			t.Fatal("没收到订阅后的行")
		}
	}
	if !strings.Contains(got[0], "订阅后的行") {
		t.Errorf("收到的应是订阅后的行，实际 %q", got[0])
	}
	// 快照按 after=1 取，应只含 seq=2 那一行（订阅前的业务日志）。
	// SSE 处理器正是用这个 cursor 把"订阅早于快照"造成的重复行滤掉。
	if len(snapshot) != 1 || snapshot[0].Seq != 2 {
		t.Errorf("after=1 的快照应只有 seq=2 一行，实际 %+v", snapshot)
	}
}

// TestTaskSlowSubscriberGetsClosedNotBlocked：
// 消费者跟不上时必须"关掉它的流"（前端会带 Last-Event-ID 重连补齐），
// 绝不能阻塞任务本身。
func TestTaskSlowSubscriberGetsClosedNotBlocked(t *testing.T) {
	m := NewManager()
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		for i := 0; i < 4000; i++ {
			log(LevelOut, fmt.Sprintf("行 %d", i))
		}
		return nil, nil
	})
	// 故意订阅但**不读**，让通道被塞满
	subID, ch := task.Subscribe()
	defer task.Unsubscribe(subID)

	done := make(chan Status, 1)
	go func() {
		<-task.Done()
		done <- task.Status()
	}()
	select {
	case st := <-done:
		if st != StatusSucceeded {
			t.Errorf("任务不该因为订阅者慢而失败，实际 %s", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("任务被慢订阅者卡住了（必须是非阻塞发送）")
	}

	// 通道最终应被服务端关闭
	closed := false
	for i := 0; i < 5000; i++ {
		if _, open := <-ch; !open {
			closed = true
			break
		}
	}
	if !closed {
		t.Error("跟不上订阅者的通道应被关闭，而不是一直堆着")
	}
}

// TestTaskConcurrentLogging 是给 -race 用的：stdout/stderr 是两个 goroutine。
func TestTaskConcurrentLogging(t *testing.T) {
	m := NewManager()
	task := m.Start("install", "demo", "安装 Demo", func(ctx context.Context, log LogFunc) (any, error) {
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					log(LevelOut, fmt.Sprintf("w%d-%d", n, i))
				}
			}(w)
		}
		wg.Wait()
		return nil, nil
	})
	waitStatus(t, task, StatusSucceeded)
	if got := task.Meta().LineCount; got != 802 { // 800 + 创建行 + 收尾行
		t.Errorf("并发写日志不应丢行，实际 %d", got)
	}
}

func TestManagerListRunningAndDuplicateGuard(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})
	running := m.Start("install", "php83", "安装 PHP", func(ctx context.Context, log LogFunc) (any, error) {
		<-release
		return nil, nil
	})
	finished := m.Start("install", "nginx", "安装 Nginx", func(ctx context.Context, log LogFunc) (any, error) {
		return nil, nil
	})
	waitStatus(t, finished, StatusSucceeded)

	if got := m.RunningFor("php83"); got == nil || got.ID() != running.ID() {
		t.Error("RunningFor 应找到进行中的任务（用于禁止重复安装同一个应用）")
	}
	if m.RunningFor("nginx") != nil {
		t.Error("已结束的任务不该被当成进行中")
	}
	if got := m.RunningCount(); got != 1 {
		t.Errorf("运行中任务数应为 1，实际 %d", got)
	}
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("列表应包含两个任务（已结束的也要留着让用户回看），实际 %d", len(list))
	}
	if list[0].ID != finished.ID() || list[1].ID != running.ID() {
		t.Error("列表应新的在前（前两个 ID 与启动顺序相反）")
	}
	close(release)
	waitStatus(t, running, StatusSucceeded)
}

func TestManagerEvictsOldTasks(t *testing.T) {
	m := NewManager()
	for i := 0; i < maxTasks+5; i++ {
		task := m.Start("install", fmt.Sprintf("app%d", i), "安装", func(ctx context.Context, log LogFunc) (any, error) {
			return nil, nil
		})
		waitStatus(t, task, StatusSucceeded)
	}
	if got := len(m.List()); got != maxTasks {
		t.Errorf("应只保留最近 %d 个任务，实际 %d", maxTasks, got)
	}
}

func TestManagerRunningCountIncludesUnfinished(t *testing.T) {
	m := NewManager()
	if m.RunningCount() != 0 {
		t.Error("空管理器不该有运行中任务")
	}
	if m.RunningFor("x") != nil || m.Get("x") != nil {
		t.Error("空管理器不该返回任务")
	}
}

// ---------- 小工具 ----------

func joinText(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func mustLines(task *Task) []Line {
	lines, _, _, _ := task.Snapshot(0, maxLinesPerTask+10)
	return lines
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

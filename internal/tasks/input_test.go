package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  任务内"限时询问用户输入"
//
//  背景：装 MySQL 时必须在安装过程中拿到 root 口令，否则就会出现
//  "面板以为空口令、MySQL 实际要口令"的不一致（2026-09-16 真机事故）。
//  这一组测试锁住四件事：等待/超时自动继续/用户输入优先/晚到的输入被拒绝，
//  以及最要命的一条：**口令不能出现在任务的任何快照里**。
// ============================================================================

// inputReq 是测试用的一份输入请求。
func inputReq() InputRequest {
	return InputRequest{
		Key:    "mysql_root_password",
		Label:  "MySQL root 口令",
		Hint:   "留空＝自动生成",
		Secret: true,
	}
}

// TestWaitInputAcceptsSubmission 用户提交了就用用户的值（优先于任何默认值）。
func TestWaitInputAcceptsSubmission(t *testing.T) {
	m := NewManager()
	got := make(chan string, 1)
	okCh := make(chan bool, 1)
	task := m.StartWithTask("install", "mysql", "安装 MySQL",
		func(ctx context.Context, task *Task) (any, error) {
			v, ok := task.WaitInput(ctx, inputReq(), 10*time.Second)
			got <- v
			okCh <- ok
			return nil, nil
		})

	// 等到任务真的在等输入：Meta 里必须出现结构化的 input_required
	// （前端就是靠它渲染输入框与倒计时，而不是去解析日志文案）。
	waitFor(t, func() bool { return task.PendingInput() != nil }, "任务进入等待输入")
	req := task.PendingInput()
	if req.Key != "mysql_root_password" || !req.Secret {
		t.Fatalf("input_required 字段不对: %+v", req)
	}
	if req.TimeoutSeconds != 10 {
		t.Errorf("timeout_seconds 应为 10，实际 %d", req.TimeoutSeconds)
	}
	if req.Deadline.IsZero() || time.Until(req.Deadline) <= 0 {
		t.Errorf("deadline 应是未来的时刻，实际 %v", req.Deadline)
	}

	if err := task.SubmitInput("mysql_root_password", "UserChosen-Pw-123"); err != nil {
		t.Fatalf("提交输入失败: %v", err)
	}
	select {
	case v := <-got:
		if v != "UserChosen-Pw-123" {
			t.Fatalf("WaitInput 应返回用户输入的值，实际 %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("提交后 WaitInput 应立即返回")
	}
	if ok := <-okCh; !ok {
		t.Fatal("用户提交时 provided 应为 true")
	}
	waitStatus(t, task, StatusSucceeded)

	// 落定后：不再有 pending，结局是 submitted
	if task.PendingInput() != nil {
		t.Error("输入落定后 PendingInput 应为 nil（否则前端会一直显示输入框）")
	}
	if r := task.InputResult(); r != "submitted" {
		t.Errorf("input_result 应为 submitted，实际 %q", r)
	}
}

// TestWaitInputTimesOutAndTaskContinues 是本需求的核心：
// 超时**不是失败**，调用方拿到 ok=false 后自己生成默认值继续。
func TestWaitInputTimesOutAndTaskContinues(t *testing.T) {
	m := NewManager()
	start := time.Now()
	task := m.StartWithTask("install", "mysql", "安装 MySQL",
		func(ctx context.Context, task *Task) (any, error) {
			_, ok := task.WaitInput(ctx, inputReq(), 200*time.Millisecond)
			if ok {
				t.Error("没有提交时 provided 应为 false")
			}
			return "已自动生成口令并继续", nil
		})
	waitStatus(t, task, StatusSucceeded)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时后必须继续往下走，实际等了 %v", elapsed)
	}
	if r := task.InputResult(); r != "timeout" {
		t.Errorf("input_result 应为 timeout，实际 %q", r)
	}
	if task.Meta().Result != "已自动生成口令并继续" {
		t.Error("超时后任务应正常完成（而不是被当成失败）")
	}
	if task.PendingInput() != nil {
		t.Error("超时后 PendingInput 应为 nil")
	}
}

// TestSubmitInputRejectsLateAndWrongKey：晚到的/答错题的输入必须**明确报错**。
// 静默丢弃会让用户以为已经提交，而任务早已用默认值继续了。
func TestSubmitInputRejectsLateAndWrongKey(t *testing.T) {
	m := NewManager()
	task := m.StartWithTask("install", "mysql", "安装 MySQL",
		func(ctx context.Context, task *Task) (any, error) {
			// 等一个很短的超时，之后任务仍在跑
			task.WaitInput(ctx, inputReq(), 100*time.Millisecond)
			time.Sleep(150 * time.Millisecond)
			return nil, nil
		})
	waitFor(t, func() bool { return task.PendingInput() != nil }, "进入等待输入")

	// 答错题（key 不匹配）
	if err := task.SubmitInput("other_key", "x"); err == nil {
		t.Fatal("key 不匹配时必须报错")
	} else if !strings.Contains(err.Error(), "mysql_root_password") {
		t.Errorf("错误信息应说明在等哪个 key，实际 %v", err)
	}
	// 正确的 key 仍然可以提交
	if err := task.SubmitInput("mysql_root_password", "pw-12345678"); err != nil {
		t.Fatalf("正确的 key 应被接受: %v", err)
	}
	// 重复提交
	if err := task.SubmitInput("mysql_root_password", "pw-12345678"); err == nil {
		t.Fatal("重复提交必须报错")
	}

	waitStatus(t, task, StatusSucceeded)
	// 任务结束后的提交必须被拒绝（前端倒计时晚一点没关系，但要如实报错）
	if err := task.SubmitInput("mysql_root_password", "late"); err == nil {
		t.Fatal("任务结束后提交必须报错")
	}
}

// TestWaitInputAfterTimeoutRejects 超时之后再投递：任务可能还在跑，
// 但已经不在等这个 key 了 —— 必须报错，而不是假装收下。
func TestWaitInputAfterTimeoutRejects(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})
	task := m.StartWithTask("install", "mysql", "安装 MySQL",
		func(ctx context.Context, task *Task) (any, error) {
			_, ok := task.WaitInput(ctx, inputReq(), 80*time.Millisecond)
			if ok {
				t.Error("超时后不该拿到 provided=true")
			}
			<-release
			return nil, nil
		})
	waitFor(t, func() bool { return task.InputResult() == "timeout" }, "等待超时")
	if task.Status() != StatusRunning {
		t.Fatal("任务应当还在跑（超时只是这一问结束）")
	}
	err := task.SubmitInput("mysql_root_password", "too-late")
	if err == nil {
		t.Fatal("超时之后投递必须报错")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误信息应说明已经超时，实际 %v", err)
	}
	close(release)
	waitStatus(t, task, StatusSucceeded)
}

// TestTaskCancelEndsInputWait：任务被中断时不能让等待的 goroutine 干等满超时。
func TestTaskCancelEndsInputWait(t *testing.T) {
	m := NewManager()
	task := m.StartWithTask("install", "mysql", "安装 MySQL",
		func(ctx context.Context, task *Task) (any, error) {
			_, ok := task.WaitInput(ctx, inputReq(), 30*time.Second)
			if ok {
				t.Error("被中断时不该拿到 provided=true")
			}
			return nil, ctx.Err()
		})
	waitFor(t, func() bool { return task.PendingInput() != nil }, "进入等待输入")
	if _, err := m.Cancel(task.ID()); err != nil {
		t.Fatalf("中断失败: %v", err)
	}
	waitStatus(t, task, StatusCanceled)
	if r := task.InputResult(); r != "canceled" {
		t.Errorf("input_result 应为 canceled，实际 %q", r)
	}
}

// TestInputValueNeverAppearsInSnapshots 是安全断言：
// 用户提交的口令**绝不能**出现在任务的元信息里 ——
// GET /api/v1/tasks/{id} 与 SSE 会反复下发 Meta，
// 值一旦进去就等于把口令写进了每一份快照、每一个日志抓包。
func TestInputValueNeverAppearsInSnapshots(t *testing.T) {
	const secret = "SuperSecret-RootPw-9876"
	m := NewManager()
	task := m.StartWithTask("install", "mysql", "安装 MySQL",
		func(ctx context.Context, task *Task) (any, error) {
			task.WaitInput(ctx, inputReq(), 5*time.Second)
			return map[string]any{"saved": true}, nil
		})
	waitFor(t, func() bool { return task.PendingInput() != nil }, "进入等待输入")
	if err := task.SubmitInput("mysql_root_password", secret); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, task, StatusSucceeded)

	// 等待期间 + 结束之后的 Meta 都不能带这个值
	meta := task.Meta()
	if meta.InputRequired != nil {
		t.Error("落定后不该还挂着 input_required")
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("任务元信息里出现了输入的值（会随 GET/SSE 反复下发）：%s", b)
	}
	// 日志里也不许有
	if strings.Contains(joinText(mustLines(task)), secret) {
		t.Error("任务日志里出现了输入的值")
	}
}

// TestStartWithTaskPassesTask 保证 web 层能拿到任务对象本身来建输入通道。
func TestStartWithTaskPassesTask(t *testing.T) {
	m := NewManager()
	var gotID string
	task := m.StartWithTask("install", "demo", "安装 Demo", func(ctx context.Context, tk *Task) (any, error) {
		gotID = tk.ID()
		return nil, nil
	})
	waitStatus(t, task, StatusSucceeded)
	if gotID != task.ID() {
		t.Fatalf("执行体拿到的任务不是启动的那个：%q vs %q", gotID, task.ID())
	}
}

// TestSameTaskCannotWaitTwoInputsAtOnce 一次只允许等一个 key：
// 并发等两个会让"这个值是回答哪个问题"变得不确定。
func TestSameTaskCannotWaitTwoInputsAtOnce(t *testing.T) {
	m := NewManager()
	task := m.StartWithTask("install", "demo", "安装", func(ctx context.Context, tk *Task) (any, error) {
		go tk.WaitInput(ctx, InputRequest{Key: "a", Label: "A"}, 2*time.Second)
		time.Sleep(50 * time.Millisecond)
		tk.WaitInput(ctx, InputRequest{Key: "b", Label: "B"}, 2*time.Second)
		return nil, nil
	})
	waitFor(t, func() bool { return task.PendingInput() != nil }, "进入等待输入")
	if got := task.PendingInput().Key; got != "a" {
		t.Errorf("先发起的等待应占住输入通道，实际等待 %q", got)
	}
	waitStatus(t, task, StatusSucceeded)
}

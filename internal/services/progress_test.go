package services

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  安装进度管道（progress.go）的回归
//
//  这一段以前是 CombinedOutput()：输出攒到命令结束才出现。
//  下面锁住"真的在流式"以及几个容易踩的边界（\r 进度条、超长行、ANSI）。
// ============================================================================

func TestProgressContextRoundTrip(t *testing.T) {
	if ProgressFrom(context.Background()) != nil {
		t.Error("没有挂接收器时不该返回非 nil（业务代码据此判断要不要发进度）")
	}
	var mu sync.Mutex
	var got []string
	ctx := WithProgress(context.Background(), func(level, text string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, level+":"+text)
	})
	emit(ctx, tasks.LevelStep, "第一步")
	emit(ctx, tasks.LevelOut, "输出")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "step:第一步" {
		t.Errorf("接收器没收到预期内容: %v", got)
	}
	// nil 接收器必须安全：WithProgress(ctx, nil) 不能把 ctx 弄坏
	if got := ProgressFrom(WithProgress(context.Background(), nil)); got != nil {
		t.Error("传 nil 接收器时应保持没有接收器")
	}
	if got := ProgressFrom(nil); got != nil { //nolint:staticcheck // 故意测 nil
		t.Error("ctx 为 nil 时不该 panic，也不该返回接收器")
	}
}

// TestInstallResultStepEmitsLiveAndKeepsSteps 锁住双写：
// 步骤既要进 Steps（旧接口/审计/单测都依赖），也要实时外发。
func TestInstallResultStepEmitsLiveAndKeepsSteps(t *testing.T) {
	res := &InstallResult{}
	var live []string
	ctx := WithProgress(context.Background(), func(level, text string) {
		if level != tasks.LevelStep {
			t.Errorf("步骤应当以 step 级别外发，实际 %s", level)
		}
		live = append(live, text)
	})
	res.step(ctx, "第一条")
	res.step(ctx, "第二条", "第三条") // 变参：有些地方一次 append 多条

	if len(res.Steps) != 3 {
		t.Fatalf("Steps 应有 3 条，实际 %v", res.Steps)
	}
	if len(live) != 3 || live[2] != "第三条" {
		t.Errorf("实时外发应逐条发生，实际 %v", live)
	}
}

// TestScanLines 覆盖切行规则。
func TestScanLines(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"普通换行", "a\nb\n", []string{"a", "b"}},
		{"CRLF", "a\r\nb\r\n", []string{"a", "b"}},
		{
			// pip / docker 的进度条用 \r 原地刷新。只按 \n 切的话整条进度
			// 会攒到命令结束才出现 —— 那就等于没做流式。
			"回车刷新进度条",
			"下载中 10%\r下载中 55%\r下载中 100%\n完成\n",
			[]string{"下载中 10%", "下载中 55%", "下载中 100%", "完成"},
		},
		{
			"去掉 ANSI 颜色",
			"\x1b[1m\x1b[34m==> Downloading\x1b[0m\n",
			[]string{"==> Downloading"},
		},
		{"空行丢弃", "\n\n   \n有内容\n", []string{"有内容"}},
		{"没有结尾换行也要发出来", "最后一行", []string{"最后一行"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			scanLines(strings.NewReader(c.input), func(s string) { got = append(got, s) })
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("期望 %q，实际 %q", c.want, got)
			}
		})
	}
}

// TestScanLinesLongLine：bufio.Scanner 默认 64KB 就报错，
// 而 docker/git 的输出里确实会有超长单行（压缩 JSON、长 URL）。
func TestScanLinesLongLine(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	var got []string
	scanLines(strings.NewReader(long+"\n短行\n"), func(s string) { got = append(got, s) })
	if len(got) < 2 {
		t.Fatalf("超长行应被拆成多段发出来，实际只有 %d 段", len(got))
	}
	joined := strings.Join(got, "")
	if !strings.Contains(joined, long) {
		t.Error("拆段不应丢字符（拼接后应与原文一致）")
	}
	if got[len(got)-1] != "短行" {
		t.Errorf("超长行之后的行应正常发出，实际最后一段 %q", got[len(got)-1])
	}
}

// TestStreamCmdStreamsBeforeExit 是本文件最关键的断言：
// **命令还没结束时就已经能收到输出**。如果退回 CombinedOutput，这里必红。
func TestStreamCmdStreamsBeforeExit(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	ctx := WithProgress(context.Background(), func(level, text string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, level+":"+text)
	})

	got := make(chan struct{})
	go func() {
		defer close(got)
		// 先打一行，睡 700ms，再打一行：第一行必须在睡眠期间就到达
		cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c",
			`echo 第一行; echo 错误行 >&2; sleep 0.7; echo 最后一行`)
		_, _ = streamCmd(ctx, cmd)
	}()

	select {
	case <-got:
		t.Fatal("命令还没结束就返回了？")
	case <-time.After(400 * time.Millisecond):
	}
	mu.Lock()
	snapshot := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(snapshot, "第一行") {
		t.Fatalf("等了 400ms 还没收到第一行输出 —— 这不是流式：\n%s", snapshot)
	}
	// 注意要按**级别**精确判断：命令标签本身现在也含 "最后一行" 这几个字
	// （标签从 cmd.Args 派生，脚本原文就在里面），用 Contains 会假阳性。
	for _, l := range lines {
		if l == "out:最后一行" {
			t.Error("命令还没结束，不该已经收到最后一行输出")
		}
	}

	<-got
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(lines, "\n")
	for _, want := range []string{"cmd:", "第一行", "err:错误行", "最后一行"} {
		if !strings.Contains(all, want) {
			t.Errorf("日志里缺少 %q：\n%s", want, all)
		}
	}
}

// TestStreamCmdReturnsOutputAndError：调用方原有的"失败时把输出摘要塞进 error"逻辑
// 依赖返回的累计文本，不能被流式改造弄丢。
func TestStreamCmdReturnsOutputAndError(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "echo 有用的线索; exit 3")
	out, err := streamCmd(context.Background(), cmd)
	if err == nil {
		t.Fatal("退出码非 0 应返回 error")
	}
	if !strings.Contains(out, "有用的线索") {
		t.Errorf("失败时也要返回累计输出（错误信息里要用），实际 %q", out)
	}
}

// TestStreamCmdCancelKillsProcess：中断必须真的杀掉子进程，
// 否则"中断"只是前端上的一个按钮。
func TestStreamCmdCancelKillsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30")
		_, err := streamCmd(ctx, cmd)
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("被中断的命令应返回错误")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel 之后命令没有被杀掉（streamCmd 阻塞了）")
	}
}

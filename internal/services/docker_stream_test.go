package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================================
//  compose up 的**流式**输出
//
//  用户原话："拉取、部署操作都要显示详细的过程，日志"。`docker compose up -d`
//  首次要拉镜像，层进度就是用户唯一能看到的真实进展；这一段锁住"逐行实时下发"，
//  一旦退回 CombinedOutput（攒到进程结束才一次性返回）这条测试必红。
// ============================================================================

// TestComposeUpStreamsLinesBeforeExit 用假的 docker-compose 脚本验证流式下发。
//
// 为什么用假脚本而不是真实 compose：单测不许碰真实 Docker。把 PATH 换成一个
// 只有假脚本的目录，composeBin() 就会找到它 —— 而脚本的输出照样走
// composeDriver.run → streamCmd → emit 这条**真实链路**，所以"是否流式"
// 是被真正测到的：脚本先打一行、睡 700ms、再打一行，第一行必须在睡眠期间
// 就到达进度接收器。
func TestComposeUpStreamsLinesBeforeExit(t *testing.T) {
	dir := t.TempDir()

	script := filepath.Join(dir, "docker-compose")
	body := "#!/bin/sh\n" +
		"echo \"$@\" >&2\n" +
		"echo 'Pulling web [==>   ] 30%'\n" +
		"sleep 3\n" +
		"echo 'Container web Started'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写假 compose 脚本失败: %v", err)
	}
	// PATH 里**不能**有开发机上的 docker / docker-compose（在 /opt/homebrew/bin
	// 或 /usr/local/bin），否则会真的去跑 compose：既慢又碰真实环境。
	//
	// 但也不能只留这一个目录：假脚本里的 `sleep` 是 /bin/sleep，
	// 只给一个目录会让它 "command not found" 而瞬间退出（测试会误报"不是流式"）。
	// /bin 与 /usr/bin 里没有 docker，所以带上它们是安全的。
	t.Setenv("PATH", dir+":/bin:/usr/bin")

	yml := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(yml, []byte("services:\n  web:\n    image: nginx:alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lines []string
	ctx := WithProgress(context.Background(), func(level, text string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, level+":"+text)
	})

	// DockerSocket 留空：不要往子进程注入 DOCKER_HOST（假脚本也不需要）。
	d := &composeDriver{opt: Options{}, svc: &Service{Name: "demo", Kind: KindCompose, ComposeFile: yml}}

	done := make(chan error, 1)
	go func() {
		_, err := d.run(ctx, 30*time.Second, "up", "-d")
		done <- err
	}()

	// 等第一行输出出现（轮询而不是睡固定的 400ms：整包测试并发跑时机器很忙，
	// 固定时长会把"shell 还没启动"误判成"不是流式"）。
	deadline := time.Now().Add(2500 * time.Millisecond)
	firstSeen := false
	for time.Now().Before(deadline) {
		mu.Lock()
		snapshot := strings.Join(lines, "\n")
		mu.Unlock()
		if strings.Contains(snapshot, "Pulling web [==>   ] 30%") {
			firstSeen = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("compose 已经结束（err=%v）却始终没收到输出行 —— 这不是流式：\n%s", err, snapshot)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !firstSeen {
		mu.Lock()
		snapshot := strings.Join(lines, "\n")
		mu.Unlock()
		t.Fatalf("等了 2.5 秒还没收到第一行 compose 输出 —— 这不是流式：\n%s", snapshot)
	}

	// 关键断言：第一行到达时脚本还在 sleep（尚未结束）。
	// 如果输出是攒到进程结束才一次性返回的，这里必然已经能收到 done。
	select {
	case err := <-done:
		t.Fatalf("第一行到达时脚本已经结束了（err=%v）—— 输出不是流式下发的", err)
	default:
	}

	mu.Lock()
	snapshot := strings.Join(lines, "\n")
	mu.Unlock()

	// 命令标签必须从 cmd.Args 派生（AGENTS.md）：要能看到真正执行的
	// `<脚本> -f <yml> up -d`，而不是一句手写的"正在部署"。
	if !strings.Contains(snapshot, "cmd:") || !strings.Contains(snapshot, "-f "+yml) {
		t.Errorf("命令标签应从 cmd.Args 派生（含 -f <文件> up -d），实际：\n%s", snapshot)
	}
	if !strings.Contains(snapshot, "cmd:") || !strings.Contains(snapshot, "up -d") {
		t.Errorf("命令标签里应包含实际的 up -d，实际：\n%s", snapshot)
	}

	if err := <-done; err != nil {
		t.Fatalf("假 compose 应成功退出: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(lines, "\n")
	for _, want := range []string{"Pulling web", "Container web Started"} {
		if !strings.Contains(all, want) {
			t.Errorf("任务日志里缺 %q：\n%s", want, all)
		}
	}
}

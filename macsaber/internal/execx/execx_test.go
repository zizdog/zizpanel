package execx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTimeoutKillsWholeProcessGroup：超时必须真的杀掉进程（含它 fork 的子进程）。
// 判据贴着"运行体"：脚本里的 sleep 结束后写的标记文件**不能出现**。
func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "done.marker")
	script := filepath.Join(dir, "slow.sh")
	body := "#!/bin/sh\n" +
		"trap 'exit 3' TERM INT\n" +
		"sleep 4\n" +
		"echo done > " + marker + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	e := New()
	start := time.Now()
	res := e.Run(context.Background(), 300*time.Millisecond, "/bin/sh", script)
	elapsed := time.Since(start)

	if !res.TimedOut {
		t.Fatalf("应标记为超时，实际 TimedOut=%v exit=%d stderr=%q", res.TimedOut, res.ExitCode, res.Stderr)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("超时后应立即返回，实际耗时 %s", elapsed)
	}
	// 给被杀的子进程一点时间落地，再断言它没写出标记。
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("子进程在超时后仍活着并写出了标记文件：进程组没被杀干净")
	}
}

// TestOutputTruncated：输出超上限要截断，并标明被截断。
func TestOutputTruncated(t *testing.T) {
	e := &Execer{MaxOutputBytes: 64}
	res := e.Run(context.Background(), 5*time.Second, "/bin/sh", "-c", "head -c 4096 /dev/zero | tr '\\0' 'a'")
	if !res.TruncOut {
		t.Fatal("应标记 stdout 被截断")
	}
	if len(res.Stdout) != 64 {
		t.Fatalf("截断后长度应为 64，实际 %d", len(res.Stdout))
	}
	if res.TimedOut {
		t.Fatal("不该超时")
	}
}

// TestArgsNeverInterpretedByShell：`; rm -rf` 作为**普通参数**必须原样传递，不被执行。
func TestArgsNeverInterpretedByShell(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	printer := filepath.Join(dir, "args.sh")
	if err := os.WriteFile(printer, []byte("#!/bin/sh\nfor a in \"$@\"; do echo \"ARG:$a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	e := New()
	evil := "; rm -rf " + dir
	res := e.Run(context.Background(), 5*time.Second, "/bin/sh", printer, evil)
	if res.ExitCode != 0 {
		t.Fatalf("脚本应成功，实际 exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "ARG:"+evil) {
		t.Fatalf("危险串应作为**单个普通参数**原样传入，实际输出 %q", res.Stdout)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("受害者文件被删了：参数被 shell 解释了（红线）: %v", err)
	}
	if _, err := os.Stat(printer); err != nil {
		t.Fatal("脚本自己也被删了")
	}
}

// TestArgvRecordedAsExecuted：审计用的 argv 必须等于真实执行的命令。
func TestArgvRecordedAsExecuted(t *testing.T) {
	e := New()
	res := e.Run(context.Background(), 5*time.Second, "/bin/echo", "a", "b c")
	if got := strings.Join(res.Argv, "|"); got != "/bin/echo|a|b c" {
		t.Fatalf("argv 记录不符: %s", got)
	}
	if strings.TrimSpace(res.Stdout) != "a b c" {
		t.Fatalf("输出不符: %q", res.Stdout)
	}
}

// TestLookPathAndProbeCache：便宜探测给结论，昂贵探测结果被缓存。
func TestLookPathAndProbeCache(t *testing.T) {
	if _, ok := LookPath("definitely-not-a-real-binary-xyz"); ok {
		t.Fatal("不存在的命令不该报可用")
	}
	if _, ok := LookPath("sh"); !ok {
		t.Fatal("sh 应该存在")
	}
	c := NewProbeCache()
	if _, _, hit := c.Get("k"); hit {
		t.Fatal("空缓存不该命中")
	}
	c.Put("k", false, "缺东西", 0)
	ok, reason, hit := c.Get("k")
	if !hit || ok || reason != "缺东西" {
		t.Fatalf("缓存结论不符: hit=%v ok=%v reason=%q", hit, ok, reason)
	}
	e := New()
	ok2, reason2 := e.Framework(context.Background(), c, "probe-key", "/bin/sh", "-c", "exit 7")
	if ok2 {
		t.Fatal("退出码非 0 的框架探测应判不可用")
	}
	if !strings.Contains(reason2, "原生框架不可用") {
		t.Fatalf("应给出真实原因，实际 %q", reason2)
	}
	if _, _, hit := c.Get("probe-key"); !hit {
		t.Fatal("昂贵探测结果应进缓存")
	}
}

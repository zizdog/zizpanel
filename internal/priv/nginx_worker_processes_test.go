package priv

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ============================================================================
//  worker_processes 不能是 1（坑 210）
//
//  2026-09-20 生产事故：站点 root 指向没有 TCC 授权的外置盘，唯一 worker 卡死
//  → 整机所有站点（含博客）一起超时，而进程还在、nginx -t 还通过。
//  门禁要求：生成/修复后的配置不是 1；修复路径遇到 1 改成 auto；幂等。
// ============================================================================

// seedNginxConf 在临时 brew 前缀下放一份 nginx.conf 与一个"假 nginx"（-t 结果可指定）。
func seedNginxConf(t *testing.T, body string, testExit int) string {
	t.Helper()
	prefix := t.TempDir()
	t.Setenv("ZIZPANEL_BREW_PREFIX", prefix)
	binDir := filepath.Join(prefix, "bin")
	etcDir := filepath.Join(prefix, "etc", "nginx")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexit " + strconv.Itoa(testExit) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "nginx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(etcDir, "nginx.conf")
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return conf
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNormalizeWorkerProcesses(t *testing.T) {
	cases := []struct {
		name, in, want string
		changed        bool
	}{
		{"唯一 worker 改成 auto", "worker_processes 1;\nevents {}\n", "worker_processes auto;\nevents {}\n", true},
		{"空白与缩进不影响", "  worker_processes   1 ;\n", "  worker_processes   auto;\n", true},
		{"auto 原样", "worker_processes auto;\n", "worker_processes auto;\n", false},
		{"多 worker 原样", "worker_processes 4;\n", "worker_processes 4;\n", false},
		{"注释不算", "# worker_processes 1;\nworker_processes auto;\n", "# worker_processes 1;\nworker_processes auto;\n", false},
		{"别的指令不动", "worker_connections 1;\n", "worker_connections 1;\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := NormalizeWorkerProcesses(c.in)
			if got != c.want || changed != c.changed {
				t.Fatalf("NormalizeWorkerProcesses(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, changed, c.want, c.changed)
			}
		})
	}
}

func TestNginxConfUsesMultipleWorkers(t *testing.T) {
	conf := seedNginxConf(t, "worker_processes 1;\nevents { worker_connections 64; }\nhttp {}\n", 0)

	changed, err := EnsureNginxWorkerProcesses()
	if err != nil {
		t.Fatalf("修复应当成功：%v", err)
	}
	if !changed {
		t.Fatal("配置里是 1，修复必须报告已改动（否则不会 reload，等于没修）")
	}
	got := readFile(t, conf)
	if strings.Contains(got, "worker_processes 1") {
		t.Errorf("修复后仍是 1：\n%s", got)
	}
	if !strings.Contains(got, "worker_processes auto") {
		t.Errorf("修复后应当是 auto：\n%s", got)
	}
	// 幂等：第二次一个字节都不该改
	again, err := EnsureNginxWorkerProcesses()
	if err != nil {
		t.Fatalf("第二次调用不该报错：%v", err)
	}
	if again {
		t.Error("已经是 auto 时必须幂等（changed=false）")
	}
	if readFile(t, conf) != got {
		t.Error("幂等调用不该改动文件内容")
	}
}

func TestNginxWorkerFixRollsBackWhenTestFails(t *testing.T) {
	const orig = "worker_processes 1;\nevents {}\n"
	conf := seedNginxConf(t, orig, 1) // 假 nginx -t 失败

	if _, err := EnsureNginxWorkerProcesses(); err == nil {
		t.Fatal("nginx -t 不通过时必须报错")
	}
	if got := readFile(t, conf); got != orig {
		t.Errorf("校验失败必须回滚成原文，实际：\n%s", got)
	}
}

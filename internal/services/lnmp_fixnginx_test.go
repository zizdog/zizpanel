package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFixNginxBaseConfigUsesMultipleWorkers：一键 LNMP 的"生成/修复 nginx.conf"路径
// 也必须把 worker_processes=1 纠正为 auto（坑 210）。
func TestFixNginxBaseConfigUsesMultipleWorkers(t *testing.T) {
	prefix := t.TempDir()
	etc := filepath.Join(prefix, "etc", "nginx")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(etc, "nginx.conf")
	body := "worker_processes 1;\nevents { worker_connections 64; }\nhttp { listen       8080; }\n"
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(nil, Options{BrewBin: filepath.Join(prefix, "bin", "brew")})
	changed, err := m.fixNginxBaseConfig(context.Background(), &InstallResult{})
	if err != nil {
		t.Fatalf("修复基础配置失败：%v", err)
	}
	if !changed {
		t.Fatal("配置里是 1，修复必须报告已改动")
	}
	got, rerr := os.ReadFile(conf)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(got), "worker_processes 1") {
		t.Errorf("修复后仍是 1：\n%s", got)
	}
	if !strings.Contains(string(got), "worker_processes auto") {
		t.Errorf("修复后应当是 auto：\n%s", got)
	}
}

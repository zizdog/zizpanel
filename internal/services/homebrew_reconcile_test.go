package services

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReconcileBrewBin 锁住"装完 Homebrew 却执行不了"这个真机事故的修法。
//
// 事故：全新机器上配置存的是 Intel 路径 /usr/local/bin/brew，而 Apple Silicon 上
// Homebrew 装到 /opt/homebrew/bin/brew —— 装成功之后没人重新探测，验证那一步就报
// "Homebrew 装完了但执行不了"（2026-09-17 生产机）。
func TestReconcileBrewBin(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "opt-homebrew", "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 钉住候选列表：绝不依赖真机是否装了 Homebrew（单测不许碰真实环境）
	prev := brewBinCandidates
	brewBinCandidates = func() []string { return []string{fake} }
	t.Cleanup(func() { brewBinCandidates = prev })

	stale := filepath.Join(dir, "usr-local", "bin", "brew")
	m := &Manager{opt: Options{BrewBin: stale}}
	if !m.reconcileBrewBin() {
		t.Fatal("配置路径不存在时应发现并修正到真实存在的 brew")
	}
	if m.opt.BrewBin != fake {
		t.Fatalf("BrewBin = %q，期望 %q", m.opt.BrewBin, fake)
	}

	// 已经指向真实存在的 brew → 不动（尊重用户自定义前缀）
	m2 := &Manager{opt: Options{BrewBin: fake}}
	if m2.reconcileBrewBin() {
		t.Fatal("已经正确时不该改动")
	}

	// 候选里一个都不存在 → 保持原值（不瞎改，让后续报错保持可读）
	brewBinCandidates = func() []string { return []string{filepath.Join(dir, "nope")} }
	m3 := &Manager{opt: Options{BrewBin: stale}}
	if m3.reconcileBrewBin() {
		t.Fatal("没有可用候选时不该报告修正")
	}
	if m3.opt.BrewBin != stale {
		t.Fatalf("没有候选时应保持原值，得到 %q", m3.opt.BrewBin)
	}
}

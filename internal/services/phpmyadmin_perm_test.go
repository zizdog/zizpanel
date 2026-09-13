package services

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// newPMAFixture 造一个临时 brew 前缀，返回 Manager 与配置文件路径。
// brewPrefix() 取的是 BrewBin 的上两级目录，所以这样就能把配置文件挪到 t.TempDir() 下，
// 不必真的往 /opt/homebrew 里写东西。
func newPMAFixture(t *testing.T, mode os.FileMode) (*Manager, string) {
	t.Helper()
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	brew := filepath.Join(prefix, "bin", "brew")
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(prefix, "etc", "phpmyadmin.config.inc.php")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("<?php $cfg['blowfish_secret']='x';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(conf, mode); err != nil {
		t.Fatal(err)
	}

	cur, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, Options{BrewBin: brew, UserName: cur.Username})
	return m, conf
}

// TestRepairPhpMyAdminConfigPerm_NotExist 未装 phpMyAdmin 时应安静跳过，不报错。
func TestRepairPhpMyAdminConfigPerm_NotExist(t *testing.T) {
	prefix := t.TempDir()
	m := NewManager(nil, Options{BrewBin: filepath.Join(prefix, "bin", "brew"), UserName: "nobody"})
	fixed, err := m.RepairPhpMyAdminConfigPerm()
	if err != nil {
		t.Fatalf("文件不存在时不应报错，得到: %v", err)
	}
	if fixed {
		t.Error("文件不存在时不应报告已修复")
	}
}

// TestRepairPhpMyAdminConfigPerm_AlreadyOK 归属与权限都正确时应无动作（幂等）。
func TestRepairPhpMyAdminConfigPerm_AlreadyOK(t *testing.T) {
	m, conf := newPMAFixture(t, 0o600)
	fixed, err := m.RepairPhpMyAdminConfigPerm()
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if fixed {
		t.Error("已正确的文件不应被再次修改")
	}
	fi, _ := os.Stat(conf)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("权限被意外改动: %v", fi.Mode().Perm())
	}
}

// TestRepairPhpMyAdminConfigPerm_FixesUnreadable 是本修复的核心回归：
// 配置文件属主读位缺失时，php-fpm 会渲染 "configuration file is not readable."，
// 自愈必须把它改回 0600。
func TestRepairPhpMyAdminConfigPerm_FixesUnreadable(t *testing.T) {
	m, conf := newPMAFixture(t, 0o000)
	fixed, err := m.RepairPhpMyAdminConfigPerm()
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !fixed {
		t.Fatal("权限不可读时应报告已修复")
	}
	fi, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("修复后权限应为 0600，实际 %v", fi.Mode().Perm())
	}
	if fi.Mode().Perm()&0o400 == 0 {
		t.Error("属主读位仍缺失，php-fpm 依然读不到")
	}
}

// TestRepairPhpMyAdminConfigPerm_Idempotent 修好之后再跑一次不应重复动作。
func TestRepairPhpMyAdminConfigPerm_Idempotent(t *testing.T) {
	m, _ := newPMAFixture(t, 0o000)
	if fixed, err := m.RepairPhpMyAdminConfigPerm(); err != nil || !fixed {
		t.Fatalf("首次修复应生效: fixed=%v err=%v", fixed, err)
	}
	if fixed, err := m.RepairPhpMyAdminConfigPerm(); err != nil || fixed {
		t.Fatalf("第二次不应再修改: fixed=%v err=%v", fixed, err)
	}
}

// TestRepairPhpMyAdminConfigPerm_NoUserName 未配置真实用户时不应尝试 chown，
// 否则会把文件改成 root 所有，反而制造出这个 bug。
func TestRepairPhpMyAdminConfigPerm_NoUserName(t *testing.T) {
	m, _ := newPMAFixture(t, 0o000)
	// 明确清掉 UserName：模拟面板拿不到真实用户的场景。
	m.opt.UserName = ""
	fixed, err := m.RepairPhpMyAdminConfigPerm()
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if fixed {
		t.Error("未配置 UserName 时不应修改文件")
	}
}

package mysql

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  数据库引擎解析的门禁（MySQL 8.4 / MariaDB）
//
//  全部在临时 brew 前缀里造"装了什么"的现场，不碰 /opt/homebrew、不碰真实 socket
//  （socket 用 net.Listen("unix") 在临时目录里造一个真的 Unix socket 文件）。
// ============================================================================

// engineFixture 在临时前缀里造出某个 formula 的 keg 与数据目录。
func engineFixture(t *testing.T, prefix, formula string) string {
	t.Helper()
	binDir := filepath.Join(prefix, "opt", formula, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "mysql"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "var", "mysql"), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir
}

// unixSocketAt 造一个真实的 Unix socket 文件（临时目录内，不碰 /tmp/mysql.sock）。
func unixSocketAt(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func TestResolveEngineMySQLOnly(t *testing.T) {
	prefix := t.TempDir()
	binDir := engineFixture(t, prefix, "mysql@8.4")
	sock := filepath.Join(t.TempDir(), "mysql.sock")
	unixSocketAt(t, sock)

	eng, err := ResolveEngine(prefix, sock)
	if err != nil {
		t.Fatalf("只装了 MySQL 时应解析成功：%v", err)
	}
	if eng.Engine != EngineMySQL || eng.Formula != "mysql@8.4" {
		t.Errorf("引擎应为 mysql@8.4，实际 %s/%s", eng.Engine, eng.Formula)
	}
	if eng.BinDir != binDir {
		t.Errorf("BinDir 应为 %s，实际 %s", binDir, eng.BinDir)
	}
	if eng.DataDir != filepath.Join(prefix, "var", "mysql") || !eng.DataDirFound {
		t.Errorf("DataDir 解析不对：%+v", eng)
	}
	if eng.Socket != sock || !eng.SocketFound {
		t.Errorf("Socket 应解析到真实存在的 %s，实际 %q（found=%v）", sock, eng.Socket, eng.SocketFound)
	}
	if eng.ServiceLabel != "sh.brew.mysql@8.4" {
		t.Errorf("Homebrew 7 的 canonical label 应为 sh.brew.mysql@8.4，实际 %q", eng.ServiceLabel)
	}
	if !eng.Verified {
		t.Error("客户端真的在，Verified 应为 true")
	}
}

func TestResolveEngineMariaDBOnly(t *testing.T) {
	prefix := t.TempDir()
	binDir := engineFixture(t, prefix, "mariadb")
	sock := filepath.Join(t.TempDir(), "mysql.sock")
	unixSocketAt(t, sock)

	eng, err := ResolveEngine(prefix, sock)
	if err != nil {
		t.Fatalf("只装了 MariaDB 时应解析成功：%v", err)
	}
	if eng.Engine != EngineMariaDB || eng.Formula != "mariadb" {
		t.Errorf("引擎应为 mariadb，实际 %s/%s", eng.Engine, eng.Formula)
	}
	if eng.BinDir != binDir {
		t.Errorf("BinDir 应为 %s，实际 %s", binDir, eng.BinDir)
	}
	if eng.ServiceLabel != "sh.brew.mariadb" {
		t.Errorf("MariaDB 的 canonical label 应为 sh.brew.mariadb，实际 %q", eng.ServiceLabel)
	}
}

// 两个都装着时**不许挑一个默认值**：磁盘上看不出该连哪个（两者不能同时跑）。
func TestResolveEngineBothInstalledIsRefused(t *testing.T) {
	prefix := t.TempDir()
	engineFixture(t, prefix, "mysql@8.4")
	engineFixture(t, prefix, "mariadb")

	if _, err := ResolveEngine(prefix, ""); err == nil {
		t.Fatal("两个引擎都装着时应当如实拒绝，而不是挑一个")
	} else if !strings.Contains(err.Error(), "mariadb") || !strings.Contains(err.Error(), "mysql@8.4") {
		t.Errorf("拒绝原因要点名两个引擎，实际：%v", err)
	}
	// 调用方带上线索（哪个在 3306 上跑）时仍能解析出指定的那一个。
	eng, ok := ResolveEngineFor(prefix, "", "mariadb")
	if !ok || eng.Formula != "mariadb" {
		t.Errorf("ResolveEngineFor(mariadb) 应能解析，实际 ok=%v %+v", ok, eng)
	}
}

// TestResolveEnginePreferring 锁住"两个都装着时按 3306 上的进程判开"。
//
// 备份与站点运行时都靠它：判不出来就必须报错（宁可拒绝，也不许用错客户端）。
func TestResolveEnginePreferring(t *testing.T) {
	prefix := t.TempDir()
	engineFixture(t, prefix, "mysql@8.4")
	engineFixture(t, prefix, "mariadb")

	cases := []struct {
		name    string
		holders []string
		want    string // 期望的 formula；空 = 应当报错
	}{
		{"端上是 mariadbd", []string{"mariadbd (pid 35985)"}, "mariadb"},
		{"端上是 mysqld", []string{"mysqld (pid 950)"}, "mysql@8.4"},
		{"端上是别的进程", []string{"nginx (pid 12)"}, ""},
		{"端上没人", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eng, err := ResolveEnginePreferring(prefix, "", c.holders)
			if c.want == "" {
				if err == nil {
					t.Fatalf("判不开时必须报错，实际解析出 %q", eng.Formula)
				}
				return
			}
			if err != nil {
				t.Fatalf("应当判到 %s，实际报错：%v", c.want, err)
			}
			if eng.Formula != c.want {
				t.Errorf("应判到 %s，实际 %s", c.want, eng.Formula)
			}
			if proc := strings.Fields(c.holders[0])[0]; !strings.Contains(eng.Note, proc) {
				t.Errorf("Note 要写清凭哪条证据判的（进程名 %q），实际 %q", proc, eng.Note)
			}
		})
	}
}

// 什么都没装 = 未复核（不是"猜一个默认引擎"）。
func TestResolveEngineNotFoundIsUnverified(t *testing.T) {
	prefix := t.TempDir()
	eng, err := ResolveEngine(prefix, "")
	if err != nil {
		t.Fatalf("没装任何引擎不是错误：%v", err)
	}
	if eng.Verified || eng.ClientFound || eng.BinDir != "" {
		t.Errorf("没装任何引擎时必须如实报未复核，实际 %+v", eng)
	}
	if strings.TrimSpace(eng.Note) == "" {
		t.Error("未复核时必须写清原因（不许静默）")
	}
	if eng.DataDir == "" {
		t.Error("数据目录的预期路径仍要给出（用于提示两个引擎共用它）")
	}
}

// MariaDB 只提供 mariadb / mariadb-dump 一套名字时也要能连（上游正在收敛命名）。
func TestClientBinaryAcceptsMariaDBNames(t *testing.T) {
	dir := t.TempDir()
	if p := clientBinary(dir, "mysql"); p != "" {
		t.Fatalf("空目录不该解析出客户端，实际 %s", p)
	}
	if err := os.WriteFile(filepath.Join(dir, "mariadb"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mariadb-dump"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := clientBinary(dir, "mysql"), filepath.Join(dir, "mariadb"); got != want {
		t.Errorf("只有 mariadb 时应回落到它，实际 %q", got)
	}
	if got, want := clientBinary(dir, "mysqldump"), filepath.Join(dir, "mariadb-dump"); got != want {
		t.Errorf("mysqldump 应回落到 mariadb-dump，实际 %q", got)
	}
	// 两套都在时优先 MySQL 的名字（保持老机器行为逐字不变）。
	if err := os.WriteFile(filepath.Join(dir, "mysql"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := clientBinary(dir, "mysql"), filepath.Join(dir, "mysql"); got != want {
		t.Errorf("两套都在时应优先 mysql，实际 %q", got)
	}
}

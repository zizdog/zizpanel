package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/config"
)

// ============================================================================
//  备份的数据库客户端门禁（MySQL 8.4 / MariaDB）
//
//  过去 clientFor 写死 <brew>/opt/mysql@8.4/bin：MariaDB 机器上那条路径不存在，
//  于是要么用了错的客户端、要么静默退到 <brew>/bin（可能是另一个引擎）。
//  这里用**临时 brew 前缀 + 假客户端脚本**跑真实代码路径：
//    · 只有 MariaDB  → 必须用 opt/mariadb/bin 下的客户端；
//    · 两个都装着、端上是 mariadbd → 必须判到 MariaDB；
//    · 两个都装着、端上说不清是谁 → **返回错误**（不许挑默认值继续）。
//
//  端口探测注入（mysqlPortHolders）：默认实现会真的起 lsof，单测不许碰真实服务。
// ============================================================================

// writeFakeBin 在 dir 下放一个可执行假脚本。
func writeFakeBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeMySQLClient 是一个"能回答 ListDatabases"的假客户端：把自己与参数记进
// $FAKE_DB_LOG，并回一行业务库。
const fakeMySQLClient = `#!/bin/sh
printf 'client=%s args=%s\n' "$0" "$*" >> "$FAKE_DB_LOG"
printf 'blog\tutf8mb4\tutf8mb4_general_ci\t0\t0\n'
`

const fakeDump = `#!/bin/sh
printf 'dump=%s args=%s\n' "$0" "$*" >> "$FAKE_DB_LOG"
printf -- '-- fake dump for test\n'
`

// engineFixture 造出"装着某个 keg"的现场，返回它的 bin 目录。
func engineFixture(t *testing.T, prefix, formula string) string {
	t.Helper()
	binDir := filepath.Join(prefix, "opt", formula, "bin")
	writeFakeBin(t, binDir, "mysql", fakeMySQLClient)
	return binDir
}

// cfgForEngine 造一个指向临时前缀的面板配置（不碰真实面板配置）。
func cfgForEngine(t *testing.T, prefix string) *config.Config {
	t.Helper()
	return &config.Config{
		BrewPrefix: prefix,
		DataDir:    filepath.Join(prefix, "data"),
		WorkDir:    filepath.Join(prefix, "work"),
		UserHome:   filepath.Join(prefix, "home"),
		User:       "tester",
		// 指向一个不存在的 socket：客户端于是走 TCP，测试不依赖真机 /tmp/mysql.sock。
		MySQLSocket: filepath.Join(prefix, "no-such.sock"),
	}
}

// onlyMySQLGens 断言"选了数据库范围就必须产出至少一个生成项"，返回生成项。
func onlyMySQLGens(t *testing.T, cfg *config.Config) []GeneratedFile {
	t.Helper()
	gens := mysqlGenerators(cfg, []string{TargetMySQL})
	if len(gens) == 0 {
		t.Fatal("选了「数据库」范围却一个生成项都没有 —— 数据库会被静默漏备")
	}
	return gens
}

// TestBackupDumpUsesMariaDBClientWhenOnlyMariaDB 只有 MariaDB 时必须用它的客户端。
func TestBackupDumpUsesMariaDBClientWhenOnlyMariaDB(t *testing.T) {
	prefix := t.TempDir()
	binDir := engineFixture(t, prefix, "mariadb")
	// 只放 MariaDB 收敛后的名字（没有 mysqldump），逼出 mariadb-dump 回落。
	writeFakeBin(t, binDir, "mariadb-dump", fakeDump)

	logPath := filepath.Join(prefix, "client.log")
	t.Setenv("FAKE_DB_LOG", logPath)

	gens := onlyMySQLGens(t, cfgForEngine(t, prefix))
	dest := filepath.Join(t.TempDir(), "blog.sql")
	if err := gens[0].Write(dest); err != nil {
		t.Fatalf("导出应当成功（假客户端会回一行库）：%v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("导出文件不存在：%v", err)
	}

	log := readFileOrFatal(t, logPath)
	if !strings.Contains(log, "client="+filepath.Join(binDir, "mysql")) {
		t.Errorf("查询用的客户端必须来自解析结果 %s，实际日志：\n%s", binDir, log)
	}
	if !strings.Contains(log, "dump="+filepath.Join(binDir, "mariadb-dump")) {
		t.Errorf("MariaDB 下必须用 mariadb-dump，实际日志：\n%s", log)
	}
	if strings.Contains(log, "mysql@8.4") {
		t.Errorf("只有 MariaDB 时不该出现 mysql@8.4 的路径，实际日志：\n%s", log)
	}
}

// TestBackupDumpUsesEffectiveEngineWhenBothInstalled
// 两个引擎都装着时，必须按端口上的进程判生效引擎（这里是 MariaDB）。
func TestBackupDumpUsesEffectiveEngineWhenBothInstalled(t *testing.T) {
	prefix := t.TempDir()
	mysqlBin := engineFixture(t, prefix, "mysql@8.4")
	writeFakeBin(t, mysqlBin, "mysqldump", fakeDump)
	mariadbBin := engineFixture(t, prefix, "mariadb")
	writeFakeBin(t, mariadbBin, "mariadb-dump", fakeDump)

	logPath := filepath.Join(prefix, "client.log")
	t.Setenv("FAKE_DB_LOG", logPath)

	restore := setPortHolders(func(int) []string { return []string{"mariadbd (pid 35985)"} })
	defer restore()

	gens := onlyMySQLGens(t, cfgForEngine(t, prefix))
	dest := filepath.Join(t.TempDir(), "blog.sql")
	if err := gens[0].Write(dest); err != nil {
		t.Fatalf("导出应当成功：%v", err)
	}
	log := readFileOrFatal(t, logPath)
	if !strings.Contains(log, "client="+filepath.Join(mariadbBin, "mysql")) {
		t.Errorf("生效引擎是 MariaDB 时必须用 %s 的客户端，实际日志：\n%s", mariadbBin, log)
	}
	if !strings.Contains(log, filepath.Join(mariadbBin, "mariadb-dump")) {
		t.Errorf("必须用 MariaDB 的 dump，实际日志：\n%s", log)
	}
	if strings.Contains(log, mysqlBin) {
		t.Errorf("不该用 MySQL 的客户端（两个都装着时挑默认值＝可能导出一份坏归档），实际日志：\n%s", log)
	}
}

// TestBackupRefusesWhenEngineCannotBeDecided
// 两个都装着、端口上又是别的进程（判不开）→ 必须如实报错，不许挑默认值继续。
func TestBackupRefusesWhenEngineCannotBeDecided(t *testing.T) {
	prefix := t.TempDir()
	engineFixture(t, prefix, "mysql@8.4")
	engineFixture(t, prefix, "mariadb")

	logPath := filepath.Join(prefix, "client.log")
	t.Setenv("FAKE_DB_LOG", logPath)

	restore := setPortHolders(func(int) []string { return []string{"nginx (pid 12)"} })
	defer restore()

	gens := onlyMySQLGens(t, cfgForEngine(t, prefix))
	if len(gens) != 1 || !strings.Contains(gens[0].ArchivePath, "failed") {
		t.Fatalf("判不开时必须产出一个「必失败」的生成项，实际 %+v", gens)
	}
	err := gens[0].Write(filepath.Join(t.TempDir(), "x"))
	if err == nil {
		t.Fatal("判不开时必须报错，绝不能挑一个默认引擎继续备份")
	}
	if !strings.Contains(err.Error(), "无法确定当前生效的数据库引擎") {
		t.Errorf("错误要指向引擎解析，实际：%v", err)
	}
	if _, statErr := os.Stat(logPath); statErr == nil {
		t.Errorf("判不开时不该执行任何客户端命令，实际日志：\n%s", readFileOrFatal(t, logPath))
	}
}

// TestBackupRefusesWhenNoClientAtAll 一个引擎的 keg 都没有 → 同样如实失败。
func TestBackupRefusesWhenNoClientAtAll(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	restore := setPortHolders(func(int) []string { return nil })
	defer restore()

	gens := onlyMySQLGens(t, cfgForEngine(t, prefix))
	if err := gens[0].Write(filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("没有客户端时必须报错")
	} else if !strings.Contains(err.Error(), "无法定位数据库客户端") {
		t.Errorf("错误要说清是客户端找不到，实际：%v", err)
	}
}

// setPortHolders 替换端口监听者探测，返回值供测试恢复。
func setPortHolders(fn func(int) []string) func() {
	prev := mysqlPortHolders
	mysqlPortHolders = fn
	return func() { mysqlPortHolders = prev }
}

func readFileOrFatal(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	return string(b)
}

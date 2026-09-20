package mysql

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  账号列表的 MariaDB 兼容门禁（2026-09-20 本机真机实测）
//
//  MariaDB 的 mysql.user **没有** account_locked 列：
//    ERROR 1054 (42S22): Unknown column 'account_locked' in 'SELECT'
//  旧实现只有 MySQL 那条 SQL，于是 MariaDB 下整个「账号与权限」列表直接报错。
//  现在按服务端**自己的版本串**（不靠安装路径猜）选 SQL，并把锁定状态如实标成
//  "读不到"（LockStateKnown=false）——绝不把 false 显示成"未锁定"。
//
//  用假客户端脚本跑真实代码路径：脚本按 SQL 内容回放（含那条真实的 1054）。
// ============================================================================

const fakeMySQLScript = `#!/bin/sh
printf 'ARGS: %s\n' "$*" >> "$FAKE_LOG"
case "$*" in
  *VERSION*)
    printf '%s\n' "$FAKE_VERSION"
    exit 0 ;;
esac
case "$*" in
  *mysql.user*)
    case "$*" in
      *account_locked*)
        if [ "${FAKE_REJECT_ACCOUNT_LOCKED:-1}" = "1" ]; then
          echo "ERROR 1054 (42S22) at line 1: Unknown column 'account_locked' in 'SELECT'" >&2
          exit 1
        fi ;;
    esac
    # 锁定位按 SQL 走：MySQL 那条会算出真实值（这里假造 1），MariaDB 那条只有 '0'。
    lock=0
    case "$*" in
      *account_locked*) lock=1 ;;
    esac
    printf 'root\tlocalhost\t%s\t1\tmysql_native_password\n' "$lock"
    printf 'app\t%%\t0\t1\tmysql_native_password\n'
    exit 0 ;;
esac
exit 0
`

func fakeClient(t *testing.T, version, rejectAccountLocked string) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(filepath.Join(dir, "mysql"), []byte(fakeMySQLScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LOG", logPath)
	t.Setenv("FAKE_VERSION", version)
	t.Setenv("FAKE_REJECT_ACCOUNT_LOCKED", rejectAccountLocked)
	return NewClient(Options{
		BinDir: dir, Host: "127.0.0.1", Port: 3306, User: "root",
		Timeout: 10 * time.Second,
	}), logPath
}

// TestListUsersOnMariaDBUsesCompatibleSQL：MariaDB 下必须用没有 account_locked 的
// 那条 SQL，成功列出账号，并把锁定状态标成"读不到"。
func TestListUsersOnMariaDBUsesCompatibleSQL(t *testing.T) {
	c, logPath := fakeClient(t, "13.0.2-MariaDB", "1")
	ctx := context.Background()

	// 先查一次版本（概览就是这么做的）：ListUsers 必须复用这次结果，不再重复查。
	if _, err := c.Version(ctx); err != nil {
		t.Fatalf("读版本失败：%v", err)
	}
	users, err := c.ListUsers(ctx)
	if err != nil {
		t.Fatalf("MariaDB 下必须能列出账号（换它有的列），实际报错：%v", err)
	}
	if len(users) != 2 {
		t.Fatalf("应列出 2 个账号，实际 %d：%+v", len(users), users)
	}
	for _, u := range users {
		if u.LockStateKnown {
			t.Errorf("MariaDB 读不到 account_locked，LockStateKnown 必须为 false：%+v", u)
		}
		if u.IsLocked {
			t.Errorf("MariaDB 下不能凭缺失的列断言'已锁定'：%+v", u)
		}
		if !u.HasPassword {
			t.Errorf("authentication_string 非空的账号 HasPassword 应为 true：%+v", u)
		}
	}

	log := readAll(t, logPath)
	userLines := ""
	for _, ln := range strings.Split(log, "\n") {
		if strings.Contains(ln, "mysql.user") {
			userLines += ln + "\n"
		}
	}
	if userLines == "" {
		t.Fatalf("假客户端没收到账号查询，日志：\n%s", log)
	}
	if strings.Contains(userLines, "account_locked") {
		t.Errorf("MariaDB 下不该执行带 account_locked 的 SQL（真机会 1054）：\n%s", userLines)
	}
	if n := strings.Count(log, "VERSION"); n != 1 {
		t.Errorf("版本只该查一次（Client 内缓存），实际查了 %d 次：\n%s", n, log)
	}
}

// TestListUsersOnMySQLKeepsLockedColumn：MySQL 行为逐字不变（仍读 account_locked）。
func TestListUsersOnMySQLKeepsLockedColumn(t *testing.T) {
	c, logPath := fakeClient(t, "8.4.11", "0")
	users, err := c.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("MySQL 下应当成功：%v", err)
	}
	if len(users) != 2 {
		t.Fatalf("应列出 2 个账号，实际 %d", len(users))
	}
	if !users[0].IsLocked || !users[0].LockStateKnown {
		t.Errorf("MySQL 下 locked='Y' 应报 IsLocked=true 且锁定状态已知：%+v", users[0])
	}
	if users[1].IsLocked || !users[1].LockStateKnown {
		t.Errorf("MySQL 下 locked='N' 应报未锁定且已知：%+v", users[1])
	}
	log := readAll(t, logPath)
	if !strings.Contains(log, "account_locked") {
		t.Errorf("MySQL 下必须继续用带 account_locked 的 SQL：\n%s", log)
	}
}

func readAll(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	return string(b)
}

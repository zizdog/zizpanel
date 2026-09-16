package mysql

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  改口令：一次调用只许连一次
//
//  这一组测试是 2026-09-16 mini 事故的回归锁：
//  旧实现是「ALTER USER … 然后再 FLUSH PRIVILEGES」。FLUSH 会**重新连接**，
//  带的是面板手里的旧口令；当被改的正是面板自己用的账号（root）时，
//  这次连接必然 1045 ——
//  于是 ALTER 已经生效、调用方却收到"认证失败"，既没把新口令写回配置，
//  也没把新口令告诉用户。从此面板永久连不上 MySQL。
//
//  用假 mysql 客户端（把参数追加到一个日志文件里）来断言"只调用了一次、
//  且没有 FLUSH"，这样单测不需要真实 MySQL（项目铁律）。
// ============================================================================

// writeFakeMySQL 在 dir 下造一个假的 mysql 客户端。
//
// 它把每次调用的 argv 记成一行 `ARGS: …`、stdin（如果有）记成一行 `SQL: …`，
// 便于断言"到底连了几次、执行了什么、口令有没有混进 argv"。
func writeFakeMySQL(t *testing.T, dir string) {
	t.Helper()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"printf 'ARGS: %s\\n' \"$*\" >> " + shellQuote(logPath) + "\n" +
		"if [ ! -t 0 ]; then cat >> " + shellQuote(logPath) + "; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "mysql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// shellQuote 给路径加单引号（测试里的路径由 t.TempDir 生成，不含单引号）。
func shellQuote(s string) string { return "'" + s + "'" }

// fakeCalls 读出假客户端记录下来的所有行。
func fakeCalls(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// fakeInvocations 返回"调用了几次"（ARGS 行的条数）与全部记录。
func fakeInvocations(t *testing.T, dir string) (int, []string) {
	t.Helper()
	all := fakeCalls(t, dir)
	n := 0
	for _, ln := range all {
		if strings.HasPrefix(ln, "ARGS: ") {
			n++
		}
	}
	return n, all
}

func TestSetUserPasswordRunsExactlyOneStatement(t *testing.T) {
	dir := t.TempDir()
	writeFakeMySQL(t, dir)
	c := NewClient(Options{
		BinDir: dir, Host: "127.0.0.1", Port: 3306,
		Socket: "/nonexistent/mysql.sock", // 走 TCP 分支，避免依赖真实 socket
		User:   "root", Password: "old-password-1",
	})
	if err := c.SetUserPassword(context.Background(), "root", "localhost", "NewPassword-2"); err != nil {
		t.Fatalf("改口令失败: %v", err)
	}
	n, calls := fakeInvocations(t, dir)
	if n != 1 {
		t.Fatalf("改口令必须只连接一次（多连一次就是旧 bug），实际 %d 次：%v", n, calls)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "ALTER USER") {
		t.Errorf("应执行 ALTER USER，实际：%s", joined)
	}
	// 明确锁死"改完不许再 FLUSH"：FLUSH 用旧口令重连，必然 1045。
	if strings.Contains(strings.ToUpper(joined), "FLUSH") {
		t.Fatalf("改口令之后不许再执行 FLUSH PRIVILEGES（会用旧口令重连→永久锁死）：%s", joined)
	}
	// 口令必须走 stdin / 环境变量，**不能进 argv**：同机 ps 能看到命令行。
	for _, ln := range calls {
		if strings.HasPrefix(ln, "ARGS: ") && strings.Contains(ln, "NewPassword-2") {
			t.Errorf("新口令不能出现在命令行参数里（ps 可见）：%s", ln)
		}
		if strings.Contains(ln, "old-password-1") {
			t.Errorf("连接口令不能出现在任何记录里（应走 MYSQL_PWD 环境变量）：%s", ln)
		}
	}
}

// TestSetUserPasswordValidationStillApplies 顺手确认校验没被改坏。
func TestSetUserPasswordValidationStillApplies(t *testing.T) {
	dir := t.TempDir()
	writeFakeMySQL(t, dir)
	c := NewClient(Options{BinDir: dir, User: "root", Host: "127.0.0.1"})
	if err := c.SetUserPassword(context.Background(), "root", "localhost", "short"); err == nil {
		t.Error("短口令必须被拒绝")
	}
	if err := c.SetUserPassword(context.Background(), "bad user", "localhost", "longenough1"); err == nil {
		t.Error("非法用户名必须被拒绝")
	}
	if calls := fakeCalls(t, dir); len(calls) != 0 {
		t.Errorf("校验失败时不该真的去连 MySQL，实际调用了：%v", calls)
	}
}

// TestRunReportsAuthFailureAsErrAuth 让上层能区分"口令问题"与"服务没起来"。
func TestRunReportsAuthFailureAsErrAuth(t *testing.T) {
	dir := t.TempDir()
	// 假客户端：把 Access denied 写到 stderr 并以 1 退出
	script := "#!/bin/sh\necho \"ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: NO)\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "mysql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewClient(Options{BinDir: dir, Host: "127.0.0.1", Port: 3306, Socket: "/nonexistent/x.sock", User: "root"})
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("1045 应能被 errors.Is(err, ErrAuth) 识别，实际 %v", err)
	}
}

// TestListUsersReturnsAuthPlugin 锁住"无密码"的判定依据：
// 只 authentication_string 为空并不等于"谁都能连"（auth_socket 就是无口令但安全），
// 所以必须把 plugin 一起带上，界面才能给出正确文案。
func TestListUsersReturnsAuthPlugin(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    *mysql.user*)
      printf 'root\tlocalhost\t0\t1\tcaching_sha2_password\n'
      printf 'app\tlocalhost\t0\t0\tauth_socket\n'
      exit 0
      ;;
  esac
done
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "mysql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewClient(Options{BinDir: dir, Host: "127.0.0.1", Port: 3306, Socket: "/nonexistent/x.sock", User: "root"})
	users, err := c.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("应解析出 2 个账号，实际 %d：%+v", len(users), users)
	}
	if users[0].User != "root" || !users[0].HasPassword || users[0].AuthPlugin != "caching_sha2_password" {
		t.Errorf("root 解析不对: %+v", users[0])
	}
	if users[1].User != "app" || users[1].HasPassword || users[1].AuthPlugin != "auth_socket" {
		t.Errorf("auth_socket 账号解析不对（应无口令但带 plugin）: %+v", users[1])
	}
}

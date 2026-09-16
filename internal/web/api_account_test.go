package web

import (
	"net/http"
	"strings"
	"testing"
)

// ============================================================================
//  改用户名
//
//  要点：改名后**旧名不能登录、新名能登录**（这是"改成功了"的唯一硬证据，
//  光看接口返回 200 不算），会话不受影响，失败尝试也要留痕。
// ============================================================================

func TestRenameUserHappyPathAndLogin(t *testing.T) {
	srv, ts := newTestServer(t)
	res, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatal("初始化失败")
	}

	// 改名前：旧名能登录
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/login",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("改名前旧名应能登录，实际 %d", res.StatusCode)
	}

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/account/username",
		map[string]string{"username": "zizdog", "password": "zizpanel-test-fixture-pass"}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("改名应 200，实际 %d: %v", res.StatusCode, out)
	}
	if got := asString(out["data"].(map[string]any)["username"]); got != "zizdog" {
		t.Fatalf("应返回新用户名，实际 %q", got)
	}

	// ★ 硬证据：旧名登录必须失败、新名必须成功
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/login",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode == 200 {
		t.Error("改名后旧用户名不该还能登录")
	}
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/login",
		map[string]string{"username": "zizdog", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Errorf("改名后新用户名应能登录，实际 %d", res.StatusCode)
	}

	// 会话不受影响：改名用的那个 cookie 仍然有效，且看到的是新名字
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/session", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("改名不该让当前会话失效，实际 %d", res.StatusCode)
	}
	user := out["data"].(map[string]any)["user"].(map[string]any)
	if user["username"] != "zizdog" {
		t.Errorf("会话里的用户名应已是新名，实际 %v", user["username"])
	}

	// 留痕
	var n int
	if err := srv.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action='account_rename' AND ok=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("成功的改名应留 1 条审计，实际 %d", n)
	}
}

func TestRenameUserRejections(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// 造第二个账号，用来测"重名"。直接插库：面板本身没有多用户管理界面。
	if _, err := srv.Store.DB().Exec(
		`INSERT INTO users(username,password_hash) SELECT 'taken', password_hash FROM users WHERE username='admin'`); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, user, pwd, wantMsg string }{
		{"缺密码", "newname", "", "当前密码"},
		{"密码错", "newname", "wrong-password", "用户名或密码错误"},
		{"重名", "taken", "zizpanel-test-fixture-pass", "已被占用"},
		{"太短", "ab", "zizpanel-test-fixture-pass", "用户名不合法"},
		{"非法字符", "bad name!", "zizpanel-test-fixture-pass", "用户名不合法"},
		{"与当前相同", "admin", "zizpanel-test-fixture-pass", "相同"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, out, _ := doJSON(t, ts, "POST", "/api/v1/account/username",
				map[string]string{"username": c.user, "password": c.pwd}, cookies)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d: %v", res.StatusCode, out)
			}
			if msg := asString(out["msg"]); !strings.Contains(msg, c.wantMsg) {
				t.Errorf("错误信息应包含 %q，实际: %s", c.wantMsg, msg)
			}
		})
	}

	// 失败的尝试也要留痕（安全相关事件）
	var n int
	if err := srv.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action='account_rename' AND ok=0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(cases) {
		t.Errorf("失败尝试应留 %d 条审计，实际 %d", len(cases), n)
	}

	// 用户名没被改动过
	var cur string
	_ = srv.Store.DB().QueryRow(`SELECT username FROM users WHERE id=1`).Scan(&cur)
	if cur != "admin" {
		t.Errorf("被拒的改名不该改动数据库，实际 %q", cur)
	}
}

func TestRenameUserRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t)
	res, _, _ := doJSON(t, ts, "POST", "/api/v1/account/username",
		map[string]string{"username": "x", "password": "y"}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录应 401，实际 %d", res.StatusCode)
	}
}

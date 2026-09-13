package store

import (
	"context"
	"testing"
)

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	// 迁移必须建出所有关键表
	for _, table := range []string{
		"users", "sessions", "sites", "services",
		"api_keys", "audit_logs", "settings", "cron_jobs", "certificates",
	} {
		var n int
		err := st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if err != nil {
			t.Fatalf("查询表 %s 失败: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("表 %s 未被创建", table)
		}
	}

	// 再次打开同一个库不应报错（迁移必须幂等）
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("二次打开失败（迁移不幂等）: %v", err)
	}
	_ = st2.Close()
}

func TestSettingsRoundTrip(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// 不存在的键返回空串且不报错
	v, err := st.GetSetting(ctx, "not-exist")
	if err != nil || v != "" {
		t.Fatalf("读取不存在的设置应返回空串，got=%q err=%v", v, err)
	}
	if err := st.SetSetting(ctx, "panel_name", "我的面板"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSetting(ctx, "panel_name")
	if err != nil || got != "我的面板" {
		t.Fatalf("设置往返失败: %q %v", got, err)
	}
	// 覆盖写
	if err := st.SetSetting(ctx, "panel_name", "ZizPanel"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetSetting(ctx, "panel_name")
	if got != "ZizPanel" {
		t.Fatalf("设置覆盖失败: %q", got)
	}
}

func TestForeignKeysAndCascade(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	res, err := st.DB().ExecContext(ctx,
		`INSERT INTO users(username,password_hash) VALUES('u','h')`)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES('t',?,'2099-01-01 00:00:00')`, uid); err != nil {
		t.Fatal(err)
	}
	// 外键约束必须生效：不存在的 user_id 应被拒绝
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES('bad',99999,'2099-01-01 00:00:00')`); err == nil {
		t.Fatal("外键约束未生效：允许插入不存在的 user_id")
	}
	// 级联删除
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM users WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("删除用户后会话应级联删除，剩余 %d 条", n)
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	res, _ := st.DB().ExecContext(ctx, `INSERT INTO users(username,password_hash) VALUES('u','h')`)
	uid, _ := res.LastInsertId()
	_, _ = st.DB().ExecContext(ctx,
		`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES('old',?, '2000-01-01 00:00:00')`, uid)
	_, _ = st.DB().ExecContext(ctx,
		`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES('new',?, '2099-01-01 00:00:00')`, uid)

	n, err := st.PurgeExpiredSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清理 1 条过期会话，实际 %d", n)
	}
	var left int
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&left)
	if left != 1 {
		t.Fatalf("未过期会话不应被删除，剩余 %d", left)
	}
}

func TestSiteUniqueConstraint(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	ins := `INSERT INTO sites(domain,root) VALUES(?,?)`
	if _, err := st.DB().ExecContext(ctx, ins, "a.test", "/tmp/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, ins, "a.test", "/tmp/b"); err == nil {
		t.Fatal("域名必须唯一")
	}
}

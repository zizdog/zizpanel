package store

import (
	"context"
	"database/sql"
	"path/filepath"
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

// TestProxiesSSLColumnsMigrateBackwardCompatible 锁住反向代理 HTTPS 的向后兼容迁移。
//
// 场景就是真实升级：用户库里已经有一张**没有 ssl_* 列**的 proxies 表和一两条
// 正在用的规则。升级后面板必须能打开、老规则必须还能读出来、且默认关 SSL
// （ssl_enabled=0 → vhost 输出与加这个功能之前逐字一致）。
func TestProxiesSSLColumnsMigrateBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "panel.db")

	// 1) 用"加 HTTPS 之前"的表结构造一个老库，并塞一条老规则。
	old, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(context.Background(), `
		CREATE TABLE proxies (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT    NOT NULL,
			listen        INTEGER NOT NULL,
			domains       TEXT    NOT NULL DEFAULT '',
			path          TEXT    NOT NULL DEFAULT '',
			target        TEXT    NOT NULL,
			preserve_host INTEGER NOT NULL DEFAULT 0,
			websocket     INTEGER NOT NULL DEFAULT 1,
			enabled       INTEGER NOT NULL DEFAULT 1,
			remark        TEXT    NOT NULL DEFAULT '',
			created_at    TEXT    NOT NULL DEFAULT '',
			updated_at    TEXT    NOT NULL DEFAULT ''
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(context.Background(),
		`INSERT INTO proxies(name,listen,domains,path,target,enabled,remark,created_at,updated_at)
		 VALUES('老规则',8090,'lede.zizdog.com','','http://192.168.1.8:8090',1,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// 2) 用当前版本的 Open 打开：迁移必须补列且不动老数据。
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("老库升级失败（迁移不向后兼容）: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	want := map[string]bool{
		"ssl_enabled": false, "ssl_cert": false, "ssl_key": false,
		"ssl_provider": false, "ssl_expires": false,
	}
	rows, err := st.DB().QueryContext(ctx, "PRAGMA table_info(proxies)")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	_ = rows.Close()
	for col, found := range want {
		if !found {
			t.Errorf("迁移后 proxies 表缺少列 %s", col)
		}
	}

	var name, domains, cert, provider string
	var sslEnabled int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT name,domains,ssl_enabled,ssl_cert,ssl_provider FROM proxies WHERE listen=8090`).
		Scan(&name, &domains, &sslEnabled, &cert, &provider); err != nil {
		t.Fatalf("老规则读取失败: %v", err)
	}
	if name != "老规则" || domains != "lede.zizdog.com" {
		t.Fatalf("迁移改动了老数据: name=%q domains=%q", name, domains)
	}
	if sslEnabled != 0 || cert != "" || provider != "" {
		t.Fatalf("老规则的 SSL 必须默认为关闭且为空: enabled=%d cert=%q provider=%q", sslEnabled, cert, provider)
	}

	// 3) 再打开一次（迁移必须幂等：ALTER TABLE 不能重复执行）。
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("二次打开失败（迁移不幂等）: %v", err)
	}
	_ = st2.Close()
}

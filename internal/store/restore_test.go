package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// seedStore 往库里写一批覆盖各表的测试数据。
func seedStore(t *testing.T, s *Store, tag string) {
	t.Helper()
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("写入测试数据失败 (%s): %v", tag, err)
		}
	}
	mustExec(`INSERT INTO users(username,password_hash,is_admin) VALUES(?,?,1)`, "admin-"+tag, "hash-"+tag)
	var uid int64
	_ = s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username=?`, "admin-"+tag).Scan(&uid)
	mustExec(`INSERT INTO sessions(token_hash,user_id,ip,expires_at) VALUES(?,?,?,datetime('now','localtime','+1 day'))`,
		"tok-"+tag, uid, "127.0.0.1")
	mustExec(`INSERT INTO sites(domain,root,php_version,ssl_enabled,ssl_provider) VALUES(?,?,?,?,?)`,
		tag+".example.com", "/tmp/"+tag, "8.4", 1, "self")
	mustExec(`INSERT INTO proxies(name,listen,target,tls_name,standard_headers,redirect_http,lan_forward,forward_port)
	         VALUES(?,?,?,?,?,?,?,?)`, "proxy-"+tag, 8080, "http://127.0.0.1:9000", "sni-"+tag, 1, 1, "on", 47123)
	mustExec(`INSERT INTO services(name,display_name,kind,stopped_by_user) VALUES(?,?,?,?)`,
		"svc-"+tag, "服务 "+tag, "native", 1)
	mustExec(`INSERT INTO api_keys(name,key_hash,scope) VALUES(?,?,?)`, "key-"+tag, "hash-"+tag, "read")
	mustExec(`INSERT INTO audit_logs(actor,action,target) VALUES(?,?,?)`, "admin-"+tag, "test_"+tag, "x")
	mustExec(`INSERT INTO settings(k,v) VALUES(?,?)`, "k-"+tag, "v-"+tag)
	mustExec(`INSERT INTO cron_jobs(name,kind,schedule,command,backup_targets,backup_dir,keep_days)
	         VALUES(?,?,?,?,?,?,?)`, "job-"+tag, "backup", "0 3 * * *", "", "nginx,panel", "/tmp/b-"+tag, 14)
	mustExec(`INSERT INTO db_credentials(user,host,password,source) VALUES(?,?,?,?)`,
		"u-"+tag, "localhost", "pw-"+tag, "panel")
}

// dumpTable 把一张表的所有行导成"与顺序无关"的字符串集合，用于逐字比对。
func dumpTable(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	return dumpQuery(t, db, `SELECT * FROM "`+table+`"`)
}

// dumpQuery 是 dumpTable 的底层实现（settings 这类表要比对指定列）。
func dumpQuery(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("查询失败 (%s): %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				parts[i] = "<NULL>"
			case []byte:
				parts[i] = string(x)
			case int64:
				parts[i] = strconv.FormatInt(x, 10)
			case float64:
				parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
			case time.Time:
				parts[i] = x.Format(time.RFC3339Nano)
			default:
				parts[i] = fmt.Sprint(x)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// tableDump 返回用于比对的表内容；settings 的 schema_version 会在恢复后
// 被 migrate() 重放一次（updated_at 变化，值不变），所以只比对键值。
func tableDump(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	if table == "settings" {
		return dumpQuery(t, db, `SELECT k,v FROM settings`)
	}
	return dumpTable(t, db, table)
}

func dumpSeq(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT name,seq FROM sqlite_sequence`)
	if err != nil {
		return map[string]int64{}
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var n string
		var s int64
		if err := rows.Scan(&n, &s); err != nil {
			t.Fatal(err)
		}
		out[n] = s
	}
	return out
}

func openStoreAt(t *testing.T, dir string) *Store {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestReplaceFromBackupRoundTrip 是恢复功能的端到端一致性断言：
// 逐表数据一致、sqlite_sequence 一致、外键自检为空、旧会话被清空。
func TestReplaceFromBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openStoreAt(t, t.TempDir())
	seedStore(t, src, "src")

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := src.SnapshotTo(ctx, snap); err != nil {
		t.Fatalf("VACUUM INTO 快照失败: %v", err)
	}
	if st, err := os.Stat(snap); err != nil || st.Size() == 0 {
		t.Fatalf("快照文件不可用: %v", err)
	}
	wantSeq := dumpSeq(t, src.db)

	// 目标库故意写入**不同**的数据，确保恢复是真的替换而不是"碰巧一样"。
	dst := openStoreAt(t, t.TempDir())
	seedStore(t, dst, "dst")

	rep, err := dst.ReplaceFromBackup(ctx, snap)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if rep.SessionsCleared == 0 {
		t.Fatal("恢复后必须清空 sessions（否则旧 cookie 会被重新激活），实际清空 0 条")
	}

	for _, table := range KnownTables() {
		if table == "sessions" {
			continue // 按设计清空，单独断言
		}
		got := tableDump(t, dst.db, table)
		want := tableDump(t, src.db, table)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("表 %s 恢复后与备份不一致：\n--- 恢复后 ---\n%s\n--- 备份 ---\n%s",
				table, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	// sessions 必须为空
	if n := len(dumpTable(t, dst.db, "sessions")); n != 0 {
		t.Fatalf("sessions 应被清空，实际还有 %d 行", n)
	}
	// 外键自检
	if err := dst.CheckForeignKeys(ctx); err != nil {
		t.Fatalf("恢复后外键自检不通过: %v", err)
	}
	// sqlite_sequence 一致（否则新建记录会撞主键）
	gotSeq := dumpSeq(t, dst.db)
	for k, v := range wantSeq {
		if gotSeq[k] != v {
			t.Fatalf("sqlite_sequence[%s] = %d，备份里是 %d", k, gotSeq[k], v)
		}
	}
	// 恢复后还能正常写入，且自增主键必须继续往前走（不能复用旧 id）
	if _, err := dst.db.ExecContext(ctx,
		`INSERT INTO sites(domain,root) VALUES('after-restore.example.com','/tmp/after')`); err != nil {
		t.Fatalf("恢复后写入失败: %v", err)
	}
	var id int64
	if err := dst.db.QueryRowContext(ctx,
		`SELECT id FROM sites WHERE domain='after-restore.example.com'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var oldMax int64
	_ = dst.db.QueryRowContext(ctx, `SELECT MAX(id) FROM sites WHERE domain<>'after-restore.example.com'`).Scan(&oldMax)
	if id <= oldMax {
		t.Fatalf("恢复后新建记录的 id=%d 没有大于备份里的最大 id=%d（sqlite_sequence 没恢复）", id, oldMax)
	}
}

// TestReplaceFromBackupTwiceOnSameStore：连续恢复两次必须都能成功。
//
// 回归测试（2026-09-18 本机真机实测到的缺陷）：恢复成功后要在 migrate() 之前
// 把连接还给连接池，而那时函数级的 defer 还没执行 —— ATTACH 的 `backup`
// 会残留在池里那条（唯一）连接上，第二次恢复报
// "database backup is already in use"。SQLite 的连接级 ATTACH 必须在
// 归还连接**之前**显式 DETACH。
func TestReplaceFromBackupTwiceOnSameStore(t *testing.T) {
	ctx := context.Background()
	src := openStoreAt(t, t.TempDir())
	seedStore(t, src, "src")
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := src.SnapshotTo(ctx, snap); err != nil {
		t.Fatal(err)
	}
	dst := openStoreAt(t, t.TempDir())
	seedStore(t, dst, "dst")

	for i := 1; i <= 3; i++ {
		rep, err := dst.ReplaceFromBackup(ctx, snap)
		if err != nil {
			t.Fatalf("第 %d 次恢复失败（ATTACH 是否残留在连接上？）: %v", i, err)
		}
		if len(rep.Tables) == 0 {
			t.Fatalf("第 %d 次恢复没有复制任何表", i)
		}
		if err := dst.CheckForeignKeys(ctx); err != nil {
			t.Fatalf("第 %d 次恢复后外键自检失败: %v", i, err)
		}
	}
}

// TestReplaceFromBackupRollsBackOnFailure：注入失败后目标库必须**一字未变**。
func TestReplaceFromBackupRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	src := openStoreAt(t, t.TempDir())
	seedStore(t, src, "src")
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := src.SnapshotTo(ctx, snap); err != nil {
		t.Fatal(err)
	}

	dst := openStoreAt(t, t.TempDir())
	seedStore(t, dst, "dst")
	before := map[string][]string{}
	for _, tbl := range KnownTables() {
		before[tbl] = tableDump(t, dst.db, tbl)
	}
	beforeSeq := dumpSeq(t, dst.db)

	restore := SetReplaceFromBackupHookForTest(func(stage string) error {
		if stage == "before-commit" {
			return fmt.Errorf("测试注入：提交前失败")
		}
		return nil
	})
	defer restore()

	if _, err := dst.ReplaceFromBackup(ctx, snap); err == nil {
		t.Fatal("注入失败后不应返回成功")
	}
	for _, tbl := range KnownTables() {
		got := tableDump(t, dst.db, tbl)
		if strings.Join(got, "\n") != strings.Join(before[tbl], "\n") {
			t.Fatalf("事务失败后表 %s 被改动了：\n%s\n--- 之前 ---\n%s",
				tbl, strings.Join(got, "\n"), strings.Join(before[tbl], "\n"))
		}
	}
	gotSeq := dumpSeq(t, dst.db)
	for k, v := range beforeSeq {
		if gotSeq[k] != v {
			t.Fatalf("事务失败后 sqlite_sequence[%s] 被改成 %d（原 %d）", k, gotSeq[k], v)
		}
	}
}

// TestReplaceFromBackupRejectsUnknownTable：备份比程序新 → 拒绝，且不做任何改动。
func TestReplaceFromBackupRejectsUnknownTable(t *testing.T) {
	ctx := context.Background()
	src := openStoreAt(t, t.TempDir())
	seedStore(t, src, "src")
	if _, err := src.db.ExecContext(ctx, `CREATE TABLE zzz_future_feature(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := src.SnapshotTo(ctx, snap); err != nil {
		t.Fatal(err)
	}
	dst := openStoreAt(t, t.TempDir())
	seedStore(t, dst, "dst")
	before := dumpTable(t, dst.db, "sites")

	_, err := dst.ReplaceFromBackup(ctx, snap)
	if err == nil || !strings.Contains(err.Error(), "zzz_future_feature") {
		t.Fatalf("备份里有未知表时必须拒绝并说明是哪张表，实际: %v", err)
	}
	if got := dumpTable(t, dst.db, "sites"); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatal("拒绝恢复时不应改动目标库")
	}
}

// TestReplaceFromBackupOlderBackupFillsDefaults：旧备份缺新列时允许恢复，
// 缺的列由 ADD COLUMN 的默认值补上，恢复后能正常读写。
func TestReplaceFromBackupOlderBackupFillsDefaults(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old-panel.db")
	db, err := sql.Open("sqlite", "file:"+oldPath)
	if err != nil {
		t.Fatal(err)
	}
	// 老版本的 sites 表：没有 ssl_provider / proxy_pass / extra_conf 等新列。
	// domain/root 是 NOT NULL 且无默认，必须提供；其余由目标库默认值补。
	stmts := []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE,
		   password_hash TEXT NOT NULL, totp_secret TEXT NOT NULL DEFAULT '', totp_enabled INTEGER NOT NULL DEFAULT 0,
		   is_admin INTEGER NOT NULL DEFAULT 1, last_login_at TEXT NOT NULL DEFAULT '', last_login_ip TEXT NOT NULL DEFAULT '',
		   fail_count INTEGER NOT NULL DEFAULT 0, locked_until TEXT NOT NULL DEFAULT '',
		   created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')), updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')))`,
		`CREATE TABLE sites (id INTEGER PRIMARY KEY AUTOINCREMENT, domain TEXT NOT NULL UNIQUE, root TEXT NOT NULL,
		   php_version TEXT NOT NULL DEFAULT '', rewrite TEXT NOT NULL DEFAULT 'none', ssl_enabled INTEGER NOT NULL DEFAULT 0,
		   created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')), updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')))`,
		`INSERT INTO users(username,password_hash) VALUES('oldadmin','oldhash')`,
		`INSERT INTO sites(domain,root,ssl_enabled) VALUES('old.example.com','/tmp/old',1)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("构造老库失败 (%s): %v", q, err)
		}
	}
	_ = db.Close()

	dst := openStoreAt(t, t.TempDir())
	seedStore(t, dst, "dst")

	rep, err := dst.ReplaceFromBackup(ctx, oldPath)
	if err != nil {
		t.Fatalf("旧备份应允许恢复（由默认值/迁移补齐），实际失败: %v", err)
	}
	if len(rep.SkippedTables) == 0 {
		t.Fatal("旧备份缺表时应在报告里如实列出未处理的表")
	}
	var domain, provider string
	var ssl int
	if err := dst.db.QueryRowContext(ctx,
		`SELECT domain, ssl_provider, ssl_enabled FROM sites WHERE domain='old.example.com'`).
		Scan(&domain, &provider, &ssl); err != nil {
		t.Fatalf("恢复后读不出旧站点: %v", err)
	}
	if provider != "" || ssl != 1 {
		t.Fatalf("缺失的列应由默认值补齐（ssl_provider 空、ssl_enabled=1），实际 provider=%q ssl=%d", provider, ssl)
	}
	// 恢复后还能正常读写（迁移补齐的列可用）
	if _, err := dst.db.ExecContext(ctx,
		`UPDATE sites SET ssl_provider='le' WHERE domain='old.example.com'`); err != nil {
		t.Fatalf("恢复后更新新列失败: %v", err)
	}
	// 站点根目录可自定义 / 目录索引这两个新列也必须在旧备份恢复后存在且为默认值
	// （base_root 空 = 默认 <WWWRoot>/<域名>；autoindex 0 = 关，老站点 vhost 逐字不变）。
	var baseRoot string
	var autoIndex int
	if err := dst.db.QueryRowContext(ctx,
		`SELECT base_root, autoindex FROM sites WHERE domain='old.example.com'`).
		Scan(&baseRoot, &autoIndex); err != nil {
		t.Fatalf("旧备份恢复后读不出 base_root/autoindex: %v", err)
	}
	if baseRoot != "" || autoIndex != 0 {
		t.Fatalf("旧备份缺的站点新列应由默认值补齐（base_root='' autoindex=0），实际 base_root=%q autoindex=%d",
			baseRoot, autoIndex)
	}
	if err := dst.CheckForeignKeys(ctx); err != nil {
		t.Fatalf("外键自检失败: %v", err)
	}
}

// TestOpenMigratesOldDatabaseColumns：手工造一个缺列的旧库，store.Open 必须补列。
func TestOpenMigratesOldDatabaseColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE proxies (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,
	   listen INTEGER NOT NULL, target TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO proxies(name,listen,target) VALUES('old',8081,'http://127.0.0.1:1')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s := openStoreAt(t, dir)
	ctx := context.Background()
	for _, col := range []string{"ssl_enabled", "tls_name", "standard_headers", "redirect_http", "lan_forward", "forward_port"} {
		var v any
		row := s.db.QueryRowContext(ctx, `SELECT `+col+` FROM proxies WHERE name='old'`)
		if err := row.Scan(&v); err != nil {
			t.Fatalf("迁移没有补上列 %s: %v", col, err)
		}
	}
}

// TestEnsureBackupTablesAddsCronColumns：cron_jobs 的备份选项列必须存在
// （否则用户选的备份范围重启后静默回落默认值）。
func TestEnsureBackupTablesAddsCronColumns(t *testing.T) {
	s := openStoreAt(t, t.TempDir())
	ctx := context.Background()
	for _, col := range []string{"backup_targets", "backup_dir", "keep_days"} {
		rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(cron_jobs)`)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var (
				cid, notNull, pk int
				name, ctype      string
				dflt             any
			)
			if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			if name == col {
				found = true
			}
		}
		_ = rows.Close()
		if !found {
			t.Fatalf("cron_jobs 缺少列 %s（备份任务的选项会静默丢失）", col)
		}
	}
}

package storage

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/migrations"
)

func newLib(id, name, root string) *domain.Library {
	return &domain.Library{ID: id, Name: name, RootPath: root, Recursive: true,
		Enabled: true, IgnoreRules: []string{}}
}

// TestMigrateCreatesSchemaAndIsIdempotent covers both migration gates: an empty
// database gets every table, and repeated startups are a no-op.
func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db", "zizvideo.db")
	// 版本号从迁移文件数推导：并行加迁移时不会因为写死数字而误报。
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	want := len(migs)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	v, err := db.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != migs[len(migs)-1].version {
		t.Fatalf("schema 版本 = %d, 期望 %d", v, migs[len(migs)-1].version)
	}
	for _, table := range []string{
		"schema_migrations", "users", "sessions", "media_libraries", "media",
		"scan_tasks", "watch_progress", "favorites", "reactions", "audit_log",
		"series", "series_media", "user_libraries", "job_tasks",
	} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("表 %s 未创建: %v", table, err)
		}
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, 期望 wal", mode)
	}
	db.Close()

	// Second start on the same file must not fail or duplicate anything.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("重复打开失败: %v", err)
	}
	defer db2.Close()
	v2, err := db2.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v2 != v {
		t.Fatalf("重复启动后 schema 版本 = %d, 期望 %d", v2, v)
	}
	var applied int
	if err := db2.QueryRow(`SELECT COUNT(1) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != want {
		t.Fatalf("迁移记录数 = %d, 期望 %d", applied, want)
	}
}

// TestMigrationVersionsAreUnique 锁死"同版本号会被静默跳过"这类问题。
func TestMigrationVersionsAreUnique(t *testing.T) {
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("迁移加载失败: %v", err)
	}
	seen := map[int]string{}
	for _, m := range migs {
		if prev, ok := seen[m.version]; ok {
			t.Fatalf("迁移版本号 %d 重复: %s 与 %s", m.version, prev, m.name)
		}
		seen[m.version] = m.name
	}
}

func TestUniqueConstraintsRespectSoftDelete(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "zizvideo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	lib := newLib("lib_1", "Movies", "/tmp/movies")
	if err := db.CreateLibrary(lib); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateLibrary(newLib("lib_2", "Movies", "/tmp/other")); err == nil {
		t.Fatal("同名媒体库必须被唯一索引拒绝")
	}
}

func TestAbandonStaleTasksOnRestart(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "zizvideo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateLibrary(newLib("lib_1", "l", "/tmp/l")); err != nil {
		t.Fatal(err)
	}
	task := &domain.ScanTask{ID: "scn_1", LibraryID: "lib_1", Kind: "incremental"}
	if err := db.CreateScanTask(task); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateScanTaskProgress("scn_1", 10, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := db.AbandonStaleTasks(); err != nil || n != 1 {
		t.Fatalf("遗留任务回收 = %d, err=%v", n, err)
	}
	got, err := db.GetScanTask("scn_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" {
		t.Fatalf("重启后状态 = %q, 期望 interrupted", got.Status)
	}
}

// TestMigration0006FailClosed：真实 0005 结构 + 历史普通用户 → 升级到 0006 后
// 0 授权（有意），管理员照旧全见。锁死"升级后家人被挡在门外"的预期行为（D.3）。
func TestMigration0006FailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zizvideo.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	old := []string{"0001_init.sql", "0002_feed.sql", "0003_series.sql",
		"0004_episode.sql", "0005_seek.sql"}
	now := domain.NowString()
	for i, name := range old {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(string(body)); err != nil {
			t.Fatalf("执行 %s: %v", name, err)
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`,
			i+1, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO users
		(id,username,display_name,password_hash,role,status,created_at,updated_at)
		VALUES ('usr_old','old','Old','x','user','active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO media_libraries
		(id,name,root_path,recursive,enabled,ignore_rules,mount_id,created_at,updated_at)
		VALUES ('lib_old','旧库','/tmp/old',1,1,'[]','',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO media
		(id,library_id,path,normalized_path,title,size,mtime_ns,container,video_codec,audio_codec,
		 width,height,duration_ms,bitrate,fps,status,error_class,error_message,probe_attempts,
		 created_at,updated_at)
		VALUES ('med_old','lib_old','/tmp/old/a.mp4','/tmp/old/a.mp4','a',1,1,'mp4','h264','aac',
		 1,1,1,1,1,'ready','','',0,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("从 0005 升级失败: %v", err)
	}
	defer db.Close()
	// 0006（库授权）+ 0007（默认可见库/source）+ 0008（跨库任务）+ 0009（自动扫描）都跑完，版本是 9。
	if v, _ := db.SchemaVersion(); v != 9 {
		t.Fatalf("schema 版本 = %d, 期望 9", v)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='user_libraries'`).
		Scan(&name); err != nil {
		t.Fatalf("user_libraries 未创建: %v", err)
	}
	ids, err := db.UserLibraryIDs("usr_old")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("迁移不得回填存量用户: %v", ids)
	}
	if briefs, err := db.ListLibrariesIn(domain.LibraryScope{}); err != nil || len(briefs) != 0 {
		t.Fatalf("空 scope 必须返回空: %v err=%v", briefs, err)
	}
	all, err := db.ListLibrariesIn(domain.LibraryScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("管理端应看到 1 个库, 得到 %d", len(all))
	}
}

// TestUserLibrariesCascadeAndSoftDelete：软删库判据立即失效但授权行保留；
// 硬删用户 CASCADE 清行；软删后重建的同名库不被旧授权命中（D.3）。
func TestUserLibrariesCascadeAndSoftDelete(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "zizvideo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateUser(&domain.User{ID: "usr_1", Username: "u1", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.StatusActive}); err != nil {
		t.Fatal(err)
	}
	l1, l2 := newLib("lib_1", "L1", "/tmp/l1"), newLib("lib_2", "L2", "/tmp/l2")
	for _, l := range []*domain.Library{l1, l2} {
		if err := db.CreateLibrary(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.GrantUserLibrary("usr_1", l1.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantUserLibrary("usr_1", l1.ID); err != nil { // 复合主键 ⇒ 幂等
		t.Fatal(err)
	}
	if err := db.GrantUserLibrary("usr_1", l2.ID); err != nil {
		t.Fatal(err)
	}
	if ids, _ := db.UserLibraryIDs("usr_1"); len(ids) != 2 {
		t.Fatalf("授权数 = %d, 期望 2", len(ids))
	}

	if err := db.DeleteLibrary(l2.ID); err != nil { // 软删
		t.Fatal(err)
	}
	ids, err := db.UserLibraryIDs("usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if ids[l2.ID] || len(ids) != 1 {
		t.Fatalf("软删库不得再进判据: %v", ids)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(1) FROM user_libraries WHERE user_id='usr_1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("软删库的授权行应保留: %d", rows)
	}

	l3 := newLib("lib_3", "L2", "/tmp/l2b") // 同名重建 = 新 id
	if err := db.CreateLibrary(l3); err != nil {
		t.Fatal(err)
	}
	ids, _ = db.UserLibraryIDs("usr_1")
	if ids[l3.ID] {
		t.Fatal("幽灵授权：旧库授权不得对新库生效")
	}
	if briefs, _ := db.ListLibrariesIn(domain.LibraryScope{IDs: map[string]bool{l2.ID: true}}); len(briefs) != 0 {
		t.Fatalf("软删库的授权范围应返回空: %v", briefs)
	}

	if _, err := db.Exec(`DELETE FROM users WHERE id='usr_1'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(1) FROM user_libraries WHERE user_id='usr_1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("硬删用户应 CASCADE 清授权行: %d", rows)
	}
}

// TestMigration0007DefaultsAndSource：0007 加列、source CHECK、自助注册同事务继承。
func TestMigration0007DefaultsAndSource(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "zizvideo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var cols int
	if err := db.QueryRow(`SELECT COUNT(1) FROM pragma_table_info('media_libraries')
		WHERE name = 'default_for_new_users'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("default_for_new_users 列缺失: n=%d err=%v", cols, err)
	}
	if err := db.QueryRow(`SELECT COUNT(1) FROM pragma_table_info('user_libraries')
		WHERE name = 'source'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("source 列缺失: n=%d err=%v", cols, err)
	}
	if _, err := db.Exec(`INSERT INTO user_libraries (user_id, library_id, source, created_at)
		VALUES ('u','l','bogus','now')`); err == nil {
		t.Fatal("source CHECK 未拦住非法值")
	}

	l1, l2 := newLib("lib_1", "L1", "/tmp/l1"), newLib("lib_2", "L2", "/tmp/l2")
	for _, l := range []*domain.Library{l1, l2} {
		if err := db.CreateLibrary(l); err != nil {
			t.Fatal(err)
		}
	}
	if ids, err := db.DefaultLibraryIDs(); err != nil || len(ids) != 0 {
		t.Fatalf("未设置默认库时必须为空（fail-closed）: %v err=%v", ids, err)
	}
	l1.DefaultForNewUsers = true
	if _, err := db.UpdateLibrary(l1.ID, LibraryPatch{DefaultForNewUsers: &l1.DefaultForNewUsers}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := db.DefaultLibraryIDs(); len(ids) != 1 || ids[0] != l1.ID {
		t.Fatalf("默认库集合 = %v, 期望 [%s]", ids, l1.ID)
	}

	self := &domain.User{ID: "usr_self", Username: "self", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.StatusActive}
	if err := db.CreateUserWithDefaults(self); err != nil {
		t.Fatal(err)
	}
	if src := grantSourceRow(t, db, self.ID, l1.ID); src != "default" {
		t.Fatalf("自助注册继承 source = %q, 期望 default", src)
	}
	manual := &domain.User{ID: "usr_manual", Username: "manual", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.StatusActive}
	if err := db.CreateUser(manual); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(1) FROM user_libraries WHERE user_id = ?`, manual.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("管理员建号不应继承: %d 行", rows)
	}
}

func grantSourceRow(t *testing.T, db *DB, userID, libraryID string) string {
	t.Helper()
	var src string
	if err := db.QueryRow(`SELECT source FROM user_libraries WHERE user_id = ? AND library_id = ?`,
		userID, libraryID).Scan(&src); err != nil {
		t.Fatal(err)
	}
	return src
}

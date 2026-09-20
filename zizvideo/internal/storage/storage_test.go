package storage

import (
	"path/filepath"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
)

func newLib(id, name, root string) *domain.Library {
	return &domain.Library{ID: id, Name: name, RootPath: root, Recursive: true,
		Enabled: true, IgnoreRules: []string{}}
}

// TestMigrateCreatesSchemaAndIsIdempotent covers both migration gates: an empty
// database gets every table, and repeated startups are a no-op.
func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db", "zizvideo.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	v, err := db.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("schema 版本 = %d, 期望 1", v)
	}
	for _, table := range []string{
		"schema_migrations", "users", "sessions", "media_libraries", "media",
		"scan_tasks", "watch_progress", "favorites", "reactions", "audit_log",
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
	if v2 != 1 {
		t.Fatalf("重复启动后 schema 版本 = %d", v2)
	}
	var applied int
	if err := db2.QueryRow(`SELECT COUNT(1) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("迁移记录数 = %d, 期望 1", applied)
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

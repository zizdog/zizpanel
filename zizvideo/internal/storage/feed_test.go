package storage

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
)

func openFeedDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "zizvideo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedFeedMedia registers n ready rows in one library.
func seedFeedMedia(t *testing.T, db *DB, libID string, n int) map[string]bool {
	t.Helper()
	want := map[string]bool{}
	for i := 0; i < n; i++ {
		m := &domain.Media{
			ID: domain.NewID("med"), LibraryID: libID,
			Path:  fmt.Sprintf("/tmp/feed/%s/%d.mp4", libID, i),
			Title: "clip", Size: 10, Status: domain.MediaReady, DurationMS: 1000,
		}
		if err := db.InsertMedia(m); err != nil {
			t.Fatal(err)
		}
		want[m.ID] = true
	}
	return want
}

// collectFeed walks one cycle and returns the ids in visit order.
func collectFeed(t *testing.T, db *DB, scope, seed string, limit int) []string {
	t.Helper()
	out := []string{}
	var hash int64
	id := ""
	for guard := 0; guard < 200; guard++ {
		rows, err := db.FeedPage(scope, seed, hash, id, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			return out
		}
		for _, m := range rows {
			out = append(out, m.ID)
			hash, id = FeedOrderKey(m.ID, seed), m.ID
		}
	}
	t.Fatalf("游标走了 200 页还没有终点")
	return out
}

// 门禁：同一 (id, seed) 必须得到同一个键；seed 变了键就该变。
func TestFeedOrderKeyDeterministicAndSeedDependent(t *testing.T) {
	id := domain.NewID("med")
	if FeedOrderKey(id, "seed-a") != FeedOrderKey(id, "seed-a") {
		t.Fatal("同一个 (id, seed) 必须得到同一个排序键")
	}
	if FeedOrderKey(id, "seed-a") == FeedOrderKey(id, "seed-b") {
		t.Fatal("seed 不同时排序键不应相同")
	}
	if FeedOrderKey(id, "seed-a") < 0 {
		t.Fatal("排序键必须是非负的 63 位整数")
	}
}

// 门禁：同一 seed 的一轮里每条只出现一次，走完就停。
func TestFeedPageVisitsEachItemOncePerCycle(t *testing.T) {
	db := openFeedDB(t)
	lib := newLib("lib_1", "A", "/tmp/a")
	if err := db.CreateLibrary(lib); err != nil {
		t.Fatal(err)
	}
	want := seedFeedMedia(t, db, lib.ID, 7)

	got := collectFeed(t, db, FeedScopeAll, "seed-1", 3)
	if len(got) != len(want) {
		t.Fatalf("一轮覆盖 %d 条, 期望 %d", len(got), len(want))
	}
	seen := map[string]int{}
	for _, id := range got {
		seen[id]++
		if !want[id] {
			t.Fatalf("返回了无关媒体 %s", id)
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("同一 seed 内 %s 出现 %d 次", id, n)
		}
	}
	// 换 seed 重开一轮，仍然覆盖全部（只是顺序不同）
	if again := collectFeed(t, db, FeedScopeAll, "seed-2", 3); len(again) != len(want) {
		t.Fatalf("新 seed 一轮覆盖 %d 条, 期望 %d", len(again), len(want))
	}
}

// 门禁：library_id 范围只返回该库，全部库范围返回全部。
func TestFeedPageScopeFiltersLibrary(t *testing.T) {
	db := openFeedDB(t)
	libA := newLib("lib_a", "A", "/tmp/a")
	libB := newLib("lib_b", "B", "/tmp/b")
	for _, lib := range []*domain.Library{libA, libB} {
		if err := db.CreateLibrary(lib); err != nil {
			t.Fatal(err)
		}
	}
	seedFeedMedia(t, db, libA.ID, 3)
	seedFeedMedia(t, db, libB.ID, 2)

	if got := collectFeed(t, db, FeedScopeAll, "s", 4); len(got) != 5 {
		t.Fatalf("全部库 = %d 条, 期望 5", len(got))
	}
	scoped := collectFeed(t, db, libA.ID, "s", 4)
	if len(scoped) != 3 {
		t.Fatalf("库 A = %d 条, 期望 3", len(scoped))
	}
	for _, id := range scoped {
		m, err := db.GetMedia(id)
		if err != nil {
			t.Fatal(err)
		}
		if m.LibraryID != libA.ID {
			t.Fatalf("库 A 范围出现了 %s 的媒体", m.LibraryID)
		}
	}
}

// 门禁：seed / 游标 / 已播计数 / 播放设置按用户持久化，scope 各一轮。
func TestFeedStateAndPrefsPersist(t *testing.T) {
	db := openFeedDB(t)
	if err := db.CreateUser(&domain.User{ID: "usr_1", Username: "u1", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.StatusActive}); err != nil {
		t.Fatal(err)
	}
	st, err := db.GetFeedState("usr_1", FeedScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if st.Seed == "" {
		t.Fatal("首次取状态应生成 seed")
	}
	st.Seed, st.CursorHash, st.CursorID, st.Played = "seed-x", 42, "med_9", 3
	if err := db.SaveFeedState(st); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetFeedState("usr_1", FeedScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seed != "seed-x" || got.CursorHash != 42 || got.CursorID != "med_9" || got.Played != 3 {
		t.Fatalf("状态没有持久化: %+v", got)
	}
	other, err := db.GetFeedState("usr_1", "lib_a")
	if err != nil {
		t.Fatal(err)
	}
	if other.Seed == "seed-x" || other.Played != 0 {
		t.Fatalf("不同 scope 必须是独立的一轮: %+v", other)
	}

	prefs, err := db.GetUserPrefs("usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if prefs.LoopPlay || !prefs.AutoplayNext {
		t.Fatalf("默认应是 循环关 / 连播开: %+v", prefs)
	}
	if err := db.SaveUserPrefs("usr_1", &UserPrefs{LoopPlay: true, AutoplayNext: false}); err != nil {
		t.Fatal(err)
	}
	prefs, err = db.GetUserPrefs("usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if !prefs.LoopPlay || prefs.AutoplayNext {
		t.Fatalf("设置没有持久化: %+v", prefs)
	}
}

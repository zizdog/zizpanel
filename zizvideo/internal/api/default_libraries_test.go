package api_test

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/domain"
)

// P3 门禁（ITERATION-2 D.5 八条）：默认可见库、注册继承、来源不参与判据、补发、软删。

type defaultsFixture struct {
	e     *env
	libs  map[string]*domain.Library
	media map[string]*domain.Media
}

func newDefaultsEnv(t *testing.T) *defaultsFixture {
	t.Helper()
	e := newEnv(t, func(c *config.Config) { c.AllowRegister = true })
	e.setupAdmin()
	f := &defaultsFixture{e: e, libs: map[string]*domain.Library{}, media: map[string]*domain.Media{}}
	for _, key := range []string{"L1", "L2", "L3"} {
		root := filepath.Join(e.Root, key)
		lib := e.newLibrary("库"+key, root)
		f.libs[key] = lib
		f.media[key] = e.newMedia(lib.ID, filepath.Join(root, "a.mp4"), []byte(key))
	}
	return f
}

func setDefault(t *testing.T, e *env, lib *domain.Library, on bool) {
	t.Helper()
	res, _, raw := e.write(http.MethodPatch, "/api/v1/libraries/"+lib.ID,
		map[string]any{"default_for_new_users": on})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("设置默认库失败 %d: %s", res.StatusCode, raw)
	}
}

// registerClient 走唯一公开自助注册入口，然后登录（返回已带会话的客户端）。
func registerClient(t *testing.T, e *env, username, password string) *http.Client {
	t.Helper()
	c := e.anonClient()
	res, raw := e.callWith(c, "", http.MethodPost, "/api/v1/auth/register",
		map[string]string{"username": username, "password": password}, false)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("自助注册 %s 失败 %d: %s", username, res.StatusCode, raw)
	}
	if res, raw := e.loginAs(c, username, password); res.StatusCode != http.StatusOK {
		t.Fatalf("登录 %s 失败 %d: %s", username, res.StatusCode, raw)
	}
	return c
}

func mustUserByUsername(t *testing.T, e *env, name string) *domain.User {
	t.Helper()
	u, err := e.DB.GetUserByUsername(name)
	if err != nil {
		t.Fatalf("找不到用户 %s: %v", name, err)
	}
	return u
}

func myLibraryIDs(t *testing.T, e *env, c *http.Client) []string {
	t.Helper()
	res, env, raw := e.doAs(c, http.MethodGet, "/api/v1/me/libraries")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/me/libraries = %d: %s", res.StatusCode, raw)
	}
	briefs := []domain.LibraryBrief{}
	decodeInto(t, env.Data, &briefs)
	ids := make([]string, 0, len(briefs))
	for _, b := range briefs {
		ids = append(ids, b.ID)
	}
	return ids
}

func idSet(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func listMediaLibraries(t *testing.T, e *env, c *http.Client) map[string]bool {
	t.Helper()
	res, env, raw := e.doAs(c, http.MethodGet, "/api/v1/media?per_page=100")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/media = %d: %s", res.StatusCode, raw)
	}
	var payload struct {
		List []struct {
			ID        string `json:"id"`
			LibraryID string `json:"library_id"`
		} `json:"list"`
	}
	decodeInto(t, env.Data, &payload)
	out := map[string]bool{}
	for _, m := range payload.List {
		out[m.LibraryID] = true
	}
	return out
}

func feedLibraries(t *testing.T, e *env, c *http.Client) []string {
	t.Helper()
	res, env, raw := e.doAs(c, http.MethodGet, "/api/v1/feed/next?limit=50")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/feed/next = %d: %s", res.StatusCode, raw)
	}
	var payload struct {
		List []struct {
			LibraryID string `json:"library_id"`
		} `json:"list"`
	}
	decodeInto(t, env.Data, &payload)
	out := []string{}
	for _, item := range payload.List {
		out = append(out, item.LibraryID)
	}
	return out
}

func grantSource(t *testing.T, e *env, userID, libraryID string) string {
	t.Helper()
	var src string
	if err := e.DB.QueryRow(`SELECT source FROM user_libraries WHERE user_id = ? AND library_id = ?`,
		userID, libraryID).Scan(&src); err != nil {
		t.Fatalf("读 source 失败: %v", err)
	}
	return src
}

// 门禁：新增管理端接口普通用户一律 403（角色错误，不是对象存在性）。
func TestNonAdminCannotCallUserLibraryAdminRoutes(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	res, _, raw := e.write(http.MethodPost, "/api/v1/users",
		map[string]any{"username": "bob", "password": "bobpass12345", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("建 bob 失败 %d: %s", res.StatusCode, raw)
	}
	bob := e.anonClient()
	if res, raw := e.loginAs(bob, "bob", "bobpass12345"); res.StatusCode != http.StatusOK {
		t.Fatalf("bob 登录失败 %d: %s", res.StatusCode, raw)
	}
	user := mustUserByUsername(t, e, "bob")
	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/admin/users/" + user.ID + "/libraries", nil},
		{http.MethodPut, "/api/v1/admin/users/" + user.ID + "/libraries", map[string]any{"library_ids": []string{}}},
		{http.MethodGet, "/api/v1/admin/libraries/defaults", nil},
		{http.MethodPost, "/api/v1/admin/libraries/defaults/backfill", map[string]bool{"confirm": true}},
	}
	for _, tc := range cases {
		res, env, raw := e.writeAs(bob, tc.method, tc.path, tc.body)
		if res.StatusCode != http.StatusForbidden || env.Error == nil || env.Error.Code != "FORBIDDEN_ROLE" {
			t.Fatalf("%s %s = %d %+v: %s", tc.method, tc.path, res.StatusCode, env.Error, raw)
		}
	}
}

// 门禁 1：新用户只看到默认库（含多人多库不越权/不重复）。
func TestNewUserSeesOnlyDefaultLibraries(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	setDefault(t, e, f.libs["L1"], true)
	setDefault(t, e, f.libs["L2"], true)

	fresh := registerClient(t, e, "fresh", "freshpass123")
	want := map[string]bool{f.libs["L1"].ID: true, f.libs["L2"].ID: true}
	got := idSet(myLibraryIDs(t, e, fresh))
	if len(got) != 2 || !got[f.libs["L1"].ID] || !got[f.libs["L2"].ID] {
		t.Fatalf("新用户 /me/libraries = %v, 期望 L1+L2 各一条", got)
	}
	if media := listMediaLibraries(t, e, fresh); !sameSet(media, want) {
		t.Fatalf("新用户 /media 库集合 = %v, 期望 %v", media, want)
	}
	for _, libID := range feedLibraries(t, e, fresh) {
		if libID == f.libs["L3"].ID {
			t.Fatal("新用户 feed 返回了非默认库 L3")
		}
	}
	res, _, _ := e.doAs(fresh, http.MethodGet, "/api/v1/media/"+f.media["L3"].ID)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("新用户读 L3 媒体 = %d, 期望 404", res.StatusCode)
	}

	// 同一个库被多人共享 + 管理员给另一人加库：不串权、不重复。
	other := registerClient(t, e, "fresh2", "freshpass456")
	otherUser := mustUserByUsername(t, e, "fresh2")
	res, _, raw := e.write(http.MethodPut, "/api/v1/admin/users/"+otherUser.ID+"/libraries",
		map[string]any{"library_ids": []string{f.libs["L1"].ID, f.libs["L2"].ID, f.libs["L3"].ID}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("管理员授权失败 %d: %s", res.StatusCode, raw)
	}
	otherIDs := myLibraryIDs(t, e, other)
	if len(otherIDs) != 3 || len(idSet(otherIDs)) != 3 {
		t.Fatalf("fresh2 /me/libraries = %v, 期望 L1+L2+L3 去重后 3 条", otherIDs)
	}
	if again := myLibraryIDs(t, e, fresh); len(again) != 2 {
		t.Fatalf("fresh 受他人授权影响 = %v, 期望仍为 L1+L2", again)
	}
}

// 门禁 2：未设默认 ⇒ 新用户 0 库（200 空 list，不是 500、更不是全库）。
func TestNoDefaultUserSeesNothing(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	u := registerClient(t, e, "nobody", "nobodypass123")
	if got := myLibraryIDs(t, e, u); len(got) != 0 {
		t.Fatalf("未设默认时 /me/libraries = %v, 期望空", got)
	}
	if media := listMediaLibraries(t, e, u); len(media) != 0 {
		t.Fatalf("未设默认时 /media 库集合 = %v, 期望空", media)
	}
	res, env, raw := e.doAs(u, http.MethodGet, "/api/v1/feed/next?limit=50")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("未设默认时 /feed/next = %d（期望 200）: %s", res.StatusCode, raw)
	}
	var payload struct {
		List []any `json:"list"`
	}
	decodeInto(t, env.Data, &payload)
	if len(payload.List) != 0 {
		t.Fatalf("未设默认时 /feed/next list = %d 条, 期望 0", len(payload.List))
	}
	res, _, _ = e.doAs(u, http.MethodGet, "/api/v1/media/"+f.media["L1"].ID)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("未设默认时读任意媒体 = %d, 期望 404", res.StatusCode)
	}
}

// 门禁 3：改默认不追溯已注册用户；此后注册的才是新集合。
func TestChangingDefaultsDoesNotRetroact(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	setDefault(t, e, f.libs["L1"], true)
	u := registerClient(t, e, "early", "earlypass123")
	setDefault(t, e, f.libs["L1"], false)
	setDefault(t, e, f.libs["L2"], true)

	if got := idSet(myLibraryIDs(t, e, u)); !got[f.libs["L1"].ID] || got[f.libs["L2"].ID] {
		t.Fatalf("改默认后老用户 = %v, 期望仍只有 L1", got)
	}
	v := registerClient(t, e, "late", "latepass123")
	if got := idSet(myLibraryIDs(t, e, v)); !got[f.libs["L2"].ID] || got[f.libs["L1"].ID] {
		t.Fatalf("改默认后新用户 = %v, 期望只有 L2", got)
	}
}

// 门禁 4：管理员手动建号不继承默认库。
func TestAdminCreatedUserDoesNotInherit(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	setDefault(t, e, f.libs["L1"], true)
	res, env, raw := e.write(http.MethodPost, "/api/v1/users",
		map[string]any{"username": "manual", "password": "manualpass123", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("管理员建号失败 %d: %s", res.StatusCode, raw)
	}
	var created domain.User
	decodeInto(t, env.Data, &created)
	c := e.anonClient()
	if res, raw := e.loginAs(c, "manual", "manualpass123"); res.StatusCode != http.StatusOK {
		t.Fatalf("manual 登录失败 %d: %s", res.StatusCode, raw)
	}
	if got := myLibraryIDs(t, e, c); len(got) != 0 {
		t.Fatalf("管理员建号 /me/libraries = %v, 期望空（不继承）", got)
	}
	var rows int
	if err := e.DB.QueryRow(`SELECT COUNT(1) FROM user_libraries WHERE user_id = ?`, created.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("管理员建号写了 %d 条授权行, 期望 0", rows)
	}
}

// 门禁 5：source 只做标注，改 source 不改变可见集合。
func TestSourceDoesNotAffectVisibility(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	setDefault(t, e, f.libs["L1"], true)
	u := registerClient(t, e, "sourced", "sourcedpass123")
	user := mustUserByUsername(t, e, "sourced")
	if src := grantSource(t, e, user.ID, f.libs["L1"].ID); src != "default" {
		t.Fatalf("注册继承 source = %q, 期望 default", src)
	}
	for _, flip := range []string{"admin", "default", "admin"} {
		if _, err := e.DB.Exec(`UPDATE user_libraries SET source = ? WHERE user_id = ? AND library_id = ?`,
			flip, user.ID, f.libs["L1"].ID); err != nil {
			t.Fatal(err)
		}
		got := myLibraryIDs(t, e, u)
		if len(got) != 1 || got[0] != f.libs["L1"].ID {
			t.Fatalf("source=%s 时可见集合 = %v, 期望恒为 L1", flip, got)
		}
	}
	// 管理员保存后 source 归一为 admin，可见集合不变。
	res, _, raw := e.write(http.MethodPut, "/api/v1/admin/users/"+user.ID+"/libraries",
		map[string]any{"library_ids": []string{f.libs["L1"].ID}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("管理员保存失败 %d: %s", res.StatusCode, raw)
	}
	if src := grantSource(t, e, user.ID, f.libs["L1"].ID); src != "admin" {
		t.Fatalf("管理员保存后 source = %q, 期望 admin", src)
	}
	if got := myLibraryIDs(t, e, u); len(got) != 1 {
		t.Fatalf("管理员保存后可见集合 = %v, 期望仍为 L1", got)
	}
}

// 门禁 6：补发只影响 0 授权用户、计数与 DB 一致、可重复且未设默认 409。
func TestBackfillDefaults(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	// 未设默认 ⇒ 409，绝不"补发 0 个还报成功"。
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/libraries/defaults/backfill",
		map[string]bool{"confirm": true})
	if res.StatusCode != http.StatusConflict || env.Error == nil || env.Error.Code != "DEFAULT_LIBRARIES_UNSET" {
		t.Fatalf("未设默认补发 = %d code=%v: %s", res.StatusCode, env.Error, raw)
	}

	setDefault(t, e, f.libs["L1"], true)
	// 一个 0 授权用户（管理员建号）+ 一个有授权用户。
	res, _, raw = e.write(http.MethodPost, "/api/v1/users",
		map[string]any{"username": "pending", "password": "pendingpass123", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("建 pending 失败 %d: %s", res.StatusCode, raw)
	}
	registerClient(t, e, "granted", "grantedpass123") // 注册会继承 L1
	grantedUser := mustUserByUsername(t, e, "granted")
	res, _, raw = e.write(http.MethodPut, "/api/v1/admin/users/"+grantedUser.ID+"/libraries",
		map[string]any{"library_ids": []string{f.libs["L3"].ID}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("给 granted 授权失败 %d: %s", res.StatusCode, raw)
	}

	res, env, raw = e.do(http.MethodGet, "/api/v1/admin/libraries/defaults", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("默认库预览 = %d: %s", res.StatusCode, raw)
	}
	var preview struct {
		DefaultCount          int `json:"default_count"`
		UsersWithoutLibraries int `json:"users_without_libraries"`
	}
	decodeInto(t, env.Data, &preview)
	if preview.DefaultCount != 1 || preview.UsersWithoutLibraries != 1 {
		t.Fatalf("预览 = %+v, 期望 default_count=1 / pending=1", preview)
	}

	res, env, raw = e.write(http.MethodPost, "/api/v1/admin/libraries/defaults/backfill",
		map[string]bool{"confirm": true})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("补发 = %d: %s", res.StatusCode, raw)
	}
	var first domain.DefaultBackfillResult
	decodeInto(t, env.Data, &first)
	if first.UsersGranted != 1 || first.UsersSkipped != 1 || first.RowsWritten != 1 {
		t.Fatalf("首次补发 = %+v, 期望 granted=1 skipped=1 rows=1", first)
	}
	pending := mustUserByUsername(t, e, "pending")
	if src := grantSource(t, e, pending.ID, f.libs["L1"].ID); src != "default" {
		t.Fatalf("补发行 source = %q, 期望 default", src)
	}
	// 已有授权用户不变。
	var grantedRows int
	if err := e.DB.QueryRow(`SELECT COUNT(1) FROM user_libraries WHERE user_id = ?`, grantedUser.ID).Scan(&grantedRows); err != nil {
		t.Fatal(err)
	}
	if grantedRows != 1 {
		t.Fatalf("已有授权用户被改动: %d 行, 期望 1", grantedRows)
	}

	res, env, raw = e.write(http.MethodPost, "/api/v1/admin/libraries/defaults/backfill",
		map[string]bool{"confirm": true})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("二次补发 = %d: %s", res.StatusCode, raw)
	}
	var second domain.DefaultBackfillResult
	decodeInto(t, env.Data, &second)
	if second.UsersGranted != 0 || second.RowsWritten != 0 {
		t.Fatalf("二次补发 = %+v, 期望 granted=0 rows=0（幂等）", second)
	}
}

// 门禁 7：注册写路径失败必须整体回滚（不许"有用户、无授权"）。
func TestRegisterRollsBackWhenGrantWriteFails(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	setDefault(t, e, f.libs["L1"], true)
	if _, err := e.DB.Exec(`CREATE TRIGGER fail_ul BEFORE INSERT ON user_libraries
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	before, err := e.DB.CountUsers()
	if err != nil {
		t.Fatal(err)
	}
	c := e.anonClient()
	res, raw := e.callWith(c, "", http.MethodPost, "/api/v1/auth/register",
		map[string]string{"username": "rollback", "password": "rollbackpass123"}, false)
	if res.StatusCode < 400 {
		t.Fatalf("注入授权写入失败后注册仍返回 %d: %s", res.StatusCode, raw)
	}
	if _, err := e.DB.GetUserByUsername("rollback"); err == nil {
		t.Fatal("授权写入失败但用户已建：事务没回滚")
	}
	after, err := e.DB.CountUsers()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("回滚后用户数 = %d, 期望 %d", after, before)
	}
	if _, err := e.DB.Exec(`DROP TRIGGER fail_ul`); err != nil {
		t.Fatal(err)
	}
	ok := registerClient(t, e, "recovered", "recoveredpass123")
	if got := myLibraryIDs(t, e, ok); len(got) != 1 || got[0] != f.libs["L1"].ID {
		t.Fatalf("恢复后注册可见集合 = %v, 期望 L1", got)
	}
}

// 门禁 8：默认库软删后已继承行保留但立即不可见；此后注册拿不到。
func TestSoftDeletedDefaultLibrary(t *testing.T) {
	f := newDefaultsEnv(t)
	e := f.e
	setDefault(t, e, f.libs["L1"], true)
	setDefault(t, e, f.libs["L2"], true)
	u := registerClient(t, e, "softy", "softypass123")
	user := mustUserByUsername(t, e, "softy")
	if got := myLibraryIDs(t, e, u); len(got) != 2 {
		t.Fatalf("软删前可见 = %v, 期望 2 个", got)
	}
	res, _, raw := e.write(http.MethodDelete, "/api/v1/libraries/"+f.libs["L1"].ID, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("软删库失败 %d: %s", res.StatusCode, raw)
	}
	if got := myLibraryIDs(t, e, u); len(got) != 1 || got[0] != f.libs["L2"].ID {
		t.Fatalf("软删后可见 = %v, 期望只剩 L2", got)
	}
	var rows int
	if err := e.DB.QueryRow(`SELECT COUNT(1) FROM user_libraries WHERE user_id = ?`, user.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("软删后已继承行 = %d, 期望保留 2 行", rows)
	}
	v := registerClient(t, e, "softy2", "softypass456")
	if got := myLibraryIDs(t, e, v); len(got) != 1 || got[0] != f.libs["L2"].ID {
		t.Fatalf("软删后新注册 = %v, 期望只有 L2", got)
	}
}

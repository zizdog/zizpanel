package api_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
)

// 权限矩阵门禁（D.1）：4 用户 × 3 库 × 16 条用户路径。
// 越权必须 404 且与"不存在"同码同文案；列表静默过滤；admin 恒全见。

type authzFixture struct {
	e       *env
	libs    map[string]*domain.Library
	media   map[string]*domain.Media
	series  map[string]string
	clients map[string]*http.Client
	ids     map[string]string
}

var authzLibKeys = []string{"L1", "L2", "L3"}

func newAuthzFixture(t *testing.T) *authzFixture {
	t.Helper()
	e := newEnv(t)
	e.setupAdmin()
	f := &authzFixture{
		e: e, libs: map[string]*domain.Library{}, media: map[string]*domain.Media{},
		series: map[string]string{}, clients: map[string]*http.Client{}, ids: map[string]string{},
	}
	f.clients["admin"] = e.Client
	_, meEnv, _ := e.do(http.MethodGet, "/api/v1/auth/me", nil)
	var me domain.User
	decodeInto(t, meEnv.Data, &me)
	f.ids["admin"] = me.ID

	for _, key := range authzLibKeys {
		root := filepath.Join(e.Root, key)
		lib := e.newLibrary("库"+key, root)
		f.libs[key] = lib
		m := e.newMedia(lib.ID, filepath.Join(root, "a.mp4"), []byte(key))
		f.media[key] = m
		sb := e.createSeries("剧场" + key)
		e.addSeriesMedia(sb.ID, m.ID)
		f.series[key] = sb.ID
	}
	grants := map[string][]string{"alice": {"L1"}, "bob": {"L1", "L2"}, "carol": nil}
	for _, name := range []string{"alice", "bob", "carol"} {
		pass := name + "pass12345"
		res, env, raw := e.write(http.MethodPost, "/api/v1/users",
			map[string]any{"username": name, "password": pass, "role": "user"})
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("创建 %s 失败 %d: %s", name, res.StatusCode, raw)
		}
		var u domain.User
		decodeInto(t, env.Data, &u)
		f.ids[name] = u.ID
		for _, libKey := range grants[name] {
			if err := e.DB.GrantUserLibrary(u.ID, f.libs[libKey].ID); err != nil {
				t.Fatal(err)
			}
		}
		c := e.anonClient()
		if res, raw := e.loginAs(c, name, pass); res.StatusCode != http.StatusOK {
			t.Fatalf("%s 登录失败 %d: %s", name, res.StatusCode, raw)
		}
		f.clients[name] = c
	}
	// 三个库都种上进度/收藏/点赞，才能验证 join 聚合也被 scope 过滤（S2）。
	for _, name := range []string{"admin", "alice", "bob", "carol"} {
		for _, key := range authzLibKeys {
			mid := f.media[key].ID
			if err := e.DB.UpsertProgress(&domain.Progress{UserID: f.ids[name], MediaID: mid,
				PositionMS: 1000, DurationMS: 5000}); err != nil {
				t.Fatal(err)
			}
			if err := e.DB.AddFavorite(f.ids[name], mid); err != nil {
				t.Fatal(err)
			}
			if err := e.DB.SetReaction(f.ids[name], mid, "like"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}

func (f *authzFixture) allowed(user string) map[string]bool {
	out := map[string]bool{}
	switch user {
	case "admin":
		for _, key := range authzLibKeys {
			out[key] = true
		}
	case "alice":
		out["L1"] = true
	case "bob":
		out["L1"], out["L2"] = true, true
	}
	return out
}

func sameSet(got, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for id := range want {
		if !got[id] {
			return false
		}
	}
	return true
}

func TestLibraryAuthzMatrix(t *testing.T) {
	f := newAuthzFixture(t)
	for _, user := range []string{"admin", "alice", "bob", "carol"} {
		allowed := f.allowed(user)
		c := f.clients[user]
		f.assertLists(t, user, c, allowed)

		for _, key := range authzLibKeys {
			mid := f.media[key].ID
			isAllowed := allowed[key]
			want := http.StatusNotFound
			if isAllowed {
				want = http.StatusOK
			}
			for _, suffix := range []string{"", "/stream", "/cover"} {
				res, env, raw := f.e.doAs(c, http.MethodGet, "/api/v1/media/"+mid+suffix)
				if res.StatusCode != want {
					t.Errorf("%s GET /media/{id}%s lib=%s = %d, 期望 %d (%s)",
						user, suffix, key, res.StatusCode, want, raw)
				}
				if !isAllowed && (env.Error == nil || env.Error.Code != "VALIDATION_NOT_FOUND") {
					t.Errorf("%s /media/{id}%s 越权必须与不存在同码: %+v", user, suffix, env.Error)
				}
			}
			res, env, raw := f.e.doAs(c, http.MethodGet, "/api/v1/feed/next?library_id="+f.libs[key].ID)
			if res.StatusCode != want {
				t.Errorf("%s feed/next lib=%s = %d, 期望 %d (%s)", user, key, res.StatusCode, want, raw)
			}
			if !isAllowed && (env.Error == nil || env.Error.Code != "VALIDATION_NOT_FOUND") {
				t.Errorf("%s feed 越权必须同码: %+v", user, env.Error)
			}
			res, env, raw = f.e.doAs(c, http.MethodGet, "/api/v1/series/"+f.series[key])
			if res.StatusCode != want {
				t.Errorf("%s series/{id} lib=%s = %d, 期望 %d (%s)", user, key, res.StatusCode, want, raw)
			}
			if !isAllowed && (env.Error == nil || env.Error.Code != "VALIDATION_NOT_FOUND") {
				t.Errorf("%s series 越权必须同码: %+v", user, env.Error)
			}

			writes := []struct {
				name, method, path string
				body               any
			}{
				{"progress", http.MethodPatch, "/api/v1/me/progress/" + mid,
					map[string]any{"position_ms": 7, "duration_ms": 9}},
				{"favorite+", http.MethodPost, "/api/v1/me/favorites/" + mid, map[string]any{}},
				{"favorite-", http.MethodDelete, "/api/v1/me/favorites/" + mid, nil},
				{"reaction+", http.MethodPost, "/api/v1/media/" + mid + "/reactions",
					map[string]any{"kind": "like"}},
				{"reaction~", http.MethodPatch, "/api/v1/media/" + mid + "/reactions",
					map[string]any{"kind": "like"}},
				{"reaction-", http.MethodDelete, "/api/v1/media/" + mid + "/reactions", nil},
			}
			for _, w := range writes {
				res, _, raw := f.e.writeAs(c, w.method, w.path, w.body)
				if res.StatusCode != want {
					t.Errorf("%s %s lib=%s = %d, 期望 %d (%s)", user, w.name, key, res.StatusCode, want, raw)
				}
			}
			if !isAllowed {
				f.assertNoSideEffect(t, user, key, mid)
			}
		}
	}
}

// 越权写不能落库，否则删收藏/进度就成了存在性探测。
func (f *authzFixture) assertNoSideEffect(t *testing.T, user, key, mid string) {
	t.Helper()
	p, err := f.e.DB.GetProgress(f.ids[user], mid)
	if err != nil || p == nil || p.PositionMS != 1000 {
		t.Errorf("%s 越权 PATCH 进度落库了 lib=%s: %+v err=%v", user, key, p, err)
	}
	favs, err := f.e.DB.Favorites(f.ids[user])
	if err != nil || !favs[mid] {
		t.Errorf("%s 越权删除收藏落库了 lib=%s", user, key)
	}
	reacts, err := f.e.DB.Reactions(f.ids[user])
	if err != nil || reacts[mid] != "like" {
		t.Errorf("%s 越权删除点赞落库了 lib=%s", user, key)
	}
}

// assertLists 覆盖 B.4 的 6 条列表路径：200 + 只出现范围内容（静默过滤）。
func (f *authzFixture) assertLists(t *testing.T, user string, c *http.Client, allowed map[string]bool) {
	t.Helper()
	wantMedia := map[string]bool{}
	wantSeries := map[string]bool{}
	for key, ok := range allowed {
		if ok {
			wantMedia[f.media[key].ID] = true
			wantSeries[f.series[key]] = true
		}
	}

	type idList struct {
		List []struct {
			ID      string `json:"id"`
			MediaID string `json:"media_id"`
			Media   struct {
				ID string `json:"id"`
			} `json:"media"`
		} `json:"list"`
	}
	get := func(path string) idList {
		t.Helper()
		res, env, raw := f.e.doAs(c, http.MethodGet, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s GET %s = %d (%s)", user, path, res.StatusCode, raw)
		}
		var out idList
		decodeInto(t, env.Data, &out)
		return out
	}
	ids := func(list idList, pick func(int) string) map[string]bool {
		got := map[string]bool{}
		for i := range list.List {
			got[pick(i)] = true
		}
		return got
	}

	mList := get("/api/v1/media?per_page=100")
	got := map[string]bool{}
	for _, it := range mList.List {
		got[it.ID] = true
	}
	if !sameSet(got, wantMedia) {
		t.Errorf("%s /media 过滤错误: %v, 期望 %v", user, got, wantMedia)
	}
	if pList := get("/api/v1/me/progress"); !sameSet(ids(pList, func(i int) string { return pList.List[i].Media.ID }), wantMedia) {
		t.Errorf("%s /me/progress 带出了无权媒体", user)
	}
	if fList := get("/api/v1/me/favorites"); !sameSet(ids(fList, func(i int) string { return fList.List[i].ID }), wantMedia) {
		t.Errorf("%s /me/favorites 带出了无权媒体", user)
	}
	if lList := get("/api/v1/me/likes"); !sameSet(ids(lList, func(i int) string { return lList.List[i].ID }), wantMedia) {
		t.Errorf("%s /me/likes 带出了无权媒体", user)
	}
	if sList := get("/api/v1/series"); !sameSet(ids(sList, func(i int) string { return sList.List[i].ID }), wantSeries) {
		t.Errorf("%s /series 过滤错误", user)
	}
	if feed := get("/api/v1/feed/next?limit=50"); !sameSet(ids(feed, func(i int) string { return feed.List[i].ID }), wantMedia) {
		t.Errorf("%s /feed/next 带出了无权媒体", user)
	}
}

// admin 越权不存在：全见；普通用户拿到 404 时响应里不能出现真实路径。
func TestAdminSeesEverythingAndPathStaysAdminOnly(t *testing.T) {
	f := newAuthzFixture(t)
	admin := f.clients["admin"]
	for _, key := range authzLibKeys {
		res, env, raw := f.e.doAs(admin, http.MethodGet, "/api/v1/media/"+f.media[key].ID)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("admin 读 lib=%s = %d (%s)", key, res.StatusCode, raw)
		}
		var item struct {
			Path string `json:"path"`
		}
		decodeInto(t, env.Data, &item)
		if item.Path != f.media[key].Path {
			t.Fatalf("admin 应拿到真实路径 lib=%s: %q", key, item.Path)
		}
	}
	res, _, raw := f.e.doAs(f.clients["alice"], http.MethodGet, "/api/v1/media?per_page=100")
	if res.StatusCode != http.StatusOK || strings.Contains(string(raw), f.media["L1"].Path) {
		t.Fatalf("普通用户响应不应含真实路径: %s", raw)
	}
}

// /me/libraries 用唯一判据，且结构里根本没有 root_path（E.2 #4）。
func TestMyLibrariesRespectsScope(t *testing.T) {
	f := newAuthzFixture(t)
	cases := map[string][]string{"alice": {"L1"}, "bob": {"L1", "L2"}, "carol": {}}
	for user, keys := range cases {
		res, env, raw := f.e.doAs(f.clients[user], http.MethodGet, "/api/v1/me/libraries")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s /me/libraries = %d (%s)", user, res.StatusCode, raw)
		}
		var list []domain.LibraryBrief
		decodeInto(t, env.Data, &list)
		got := map[string]bool{}
		for _, b := range list {
			got[b.ID] = true
		}
		want := map[string]bool{}
		for _, key := range keys {
			want[f.libs[key].ID] = true
		}
		if !sameSet(got, want) {
			t.Errorf("%s /me/libraries = %v, 期望 %v", user, got, want)
		}
		if strings.Contains(string(raw), "root_path") {
			t.Errorf("%s /me/libraries 泄露了 root_path: %s", user, raw)
		}
	}
	res, env, raw := f.e.doAs(f.clients["admin"], http.MethodGet, "/api/v1/me/libraries")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("admin /me/libraries = %d (%s)", res.StatusCode, raw)
	}
	var list []domain.LibraryBrief
	decodeInto(t, env.Data, &list)
	if len(list) != len(authzLibKeys) {
		t.Errorf("admin 应看到全部 %d 个库, 得到 %d", len(authzLibKeys), len(list))
	}
}

func TestForbiddenListsAreEmpty200NotError(t *testing.T) {
	f := newAuthzFixture(t)
	carol := f.clients["carol"]
	for _, path := range []string{"/api/v1/media", "/api/v1/series", "/api/v1/me/progress",
		"/api/v1/me/favorites", "/api/v1/me/likes", "/api/v1/feed/next"} {
		res, env, raw := f.e.doAs(carol, http.MethodGet, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("无授权用户 %s = %d, 期望 200 空列表 (%s)", path, res.StatusCode, raw)
		}
		var body struct {
			List []json.RawMessage `json:"list"`
		}
		decodeInto(t, env.Data, &body)
		if len(body.List) != 0 {
			t.Fatalf("无授权用户 %s 不该有内容: %s", path, raw)
		}
	}
}

// TestUpgradeFrom0005BlocksRegularUsers：HTTP 侧的升级 fail-closed。
// 用"删掉 0006 产物再重跑 Migrate"模拟旧库升级（真实 0005 结构见 storage 包同名门禁）。
func TestUpgradeFrom0005BlocksRegularUsers(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	res, env, raw := e.write(http.MethodPost, "/api/v1/users",
		map[string]any{"username": "olduser", "password": "oldpass12345", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("建历史用户失败 %d: %s", res.StatusCode, raw)
	}
	var old domain.User
	decodeInto(t, env.Data, &old)

	lib := e.newLibrary("历史库", e.Root)
	media := e.newMedia(lib.ID, filepath.Join(e.Root, "old.mp4"), []byte("old"))

	if _, err := e.DB.Exec(`DROP TABLE user_libraries`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`DELETE FROM schema_migrations WHERE version = 6`); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Migrate(); err != nil {
		t.Fatalf("升级 0006 失败: %v", err)
	}

	user := e.anonClient()
	if res, raw := e.loginAs(user, "olduser", "oldpass12345"); res.StatusCode != http.StatusOK {
		t.Fatalf("历史用户登录失败 %d: %s", res.StatusCode, raw)
	}
	check := func(path string, want int) envelope {
		t.Helper()
		res, env, raw := e.doAs(user, http.MethodGet, path)
		if res.StatusCode != want {
			t.Fatalf("历史用户 GET %s = %d, 期望 %d (%s)", path, res.StatusCode, want, raw)
		}
		return env
	}
	var list struct {
		List []json.RawMessage `json:"list"`
	}
	decodeInto(t, check("/api/v1/media", http.StatusOK).Data, &list)
	if len(list.List) != 0 {
		t.Fatalf("升级后历史用户必须看不到媒体: %v", list.List)
	}
	list.List = nil
	// /me/libraries 返回数组本身（不是 {list}）
	var libs []json.RawMessage
	decodeInto(t, check("/api/v1/me/libraries", http.StatusOK).Data, &libs)
	if len(libs) != 0 {
		t.Fatalf("升级后历史用户必须 0 个库: %v", libs)
	}
	check("/api/v1/libraries", http.StatusForbidden)
	res, _, raw = e.doAs(user, http.MethodGet, "/api/v1/media/"+media.ID)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("升级后历史用户读单条 = %d, 期望 404 (%s)", res.StatusCode, raw)
	}

	// admin 照旧全见
	if res, _, raw := e.do(http.MethodGet, "/api/v1/libraries", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("admin /libraries = %d (%s)", res.StatusCode, raw)
	}
	_, adminEnv, _ := e.do(http.MethodGet, "/api/v1/media", nil)
	var adminMedia struct {
		List []struct {
			ID string `json:"id"`
		} `json:"list"`
	}
	decodeInto(t, adminEnv.Data, &adminMedia)
	found := false
	for _, it := range adminMedia.List {
		if it.ID == media.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("admin 升级后应照旧看到全部媒体")
	}
	if old.ID == "" {
		t.Fatal("历史用户 id 为空")
	}
}

// 空剧场：admin 必须仍能看到（否则新建后无法加集管理），普通用户看不到。
func TestEmptySeriesVisibleOnlyToAdmin(t *testing.T) {
	f := newAuthzFixture(t)
	empty := f.e.createSeries("空剧场")

	seriesIDs := func(c *http.Client) map[string]bool {
		t.Helper()
		var res *http.Response
		var env envelope
		var raw []byte
		if c == f.e.Client {
			res, env, raw = f.e.do(http.MethodGet, "/api/v1/series", nil)
		} else {
			res, env, raw = f.e.doAs(c, http.MethodGet, "/api/v1/series")
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET /series = %d (%s)", res.StatusCode, raw)
		}
		var body struct {
			List []struct {
				ID string `json:"id"`
			} `json:"list"`
		}
		decodeInto(t, env.Data, &body)
		out := map[string]bool{}
		for _, it := range body.List {
			out[it.ID] = true
		}
		return out
	}

	if !seriesIDs(f.e.Client)[empty.ID] {
		t.Fatal("admin 列表必须包含还没加集的空剧场")
	}
	bob := seriesIDs(f.clients["bob"])
	if bob[empty.ID] || bob[f.series["L3"]] {
		t.Fatalf("普通用户列表出现了不可见剧场: %v", bob)
	}
	// 空剧场详情：admin 必须 200（管理抽屉依赖它加集），普通用户仍是 404。
	if res, _, raw := f.e.do(http.MethodGet, "/api/v1/series/"+empty.ID, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("admin 空剧场详情 = %d (%s)", res.StatusCode, raw)
	}
	if res, _, _ := f.e.doAs(f.clients["bob"], http.MethodGet, "/api/v1/series/"+empty.ID); res.StatusCode != http.StatusNotFound {
		t.Fatalf("普通用户空剧场详情 = %d, 期望 404", res.StatusCode)
	}
}

// 跨库剧场：剧集按 scope 过滤，封面属别的库时 cover_url 置空（B.4#7 / S4）。
func TestSeriesCrossLibraryFiltering(t *testing.T) {
	f := newAuthzFixture(t)
	s := f.e.createSeries("跨库剧场")
	f.e.addSeriesMedia(s.ID, f.media["L2"].ID, f.media["L1"].ID)
	res, _, raw := f.e.write(http.MethodPatch, "/api/v1/admin/series/"+s.ID,
		map[string]any{"cover_media_id": f.media["L2"].ID})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("设置封面失败 %d: %s", res.StatusCode, raw)
	}

	type detail struct {
		Series struct {
			CoverURL     string `json:"cover_url"`
			EpisodeCount int    `json:"episode_count"`
		} `json:"series"`
		List []struct {
			Media struct {
				ID string `json:"id"`
			} `json:"media"`
		} `json:"list"`
	}
	read := func(name string) detail {
		t.Helper()
		res, env, raw := f.e.doAs(f.clients[name], http.MethodGet, "/api/v1/series/"+s.ID)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s 读跨库剧场 = %d (%s)", name, res.StatusCode, raw)
		}
		var out detail
		decodeInto(t, env.Data, &out)
		return out
	}

	alice := read("alice")
	if len(alice.List) != 1 || alice.List[0].Media.ID != f.media["L1"].ID {
		t.Fatalf("alice 只应看到 L1 一集: %+v", alice.List)
	}
	if alice.Series.CoverURL != "" || alice.Series.EpisodeCount != 1 {
		t.Fatalf("alice 封面应置空、集数应为 1: %+v", alice.Series)
	}
	bob := read("bob")
	if len(bob.List) != 2 || bob.Series.CoverURL == "" {
		t.Fatalf("bob 应看到两集且封面可用: %+v cover=%q", bob.List, bob.Series.CoverURL)
	}
	res, _, raw = f.e.doAs(f.clients["carol"], http.MethodGet, "/api/v1/series/"+s.ID)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("无授权用户读跨库剧场 = %d, 期望 404 (%s)", res.StatusCode, raw)
	}
}

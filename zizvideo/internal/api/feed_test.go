package api_test

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type feedItemBody struct {
	ID          string `json:"id"`
	LibraryID   string `json:"library_id"`
	LibraryName string `json:"library_name"`
}

type feedBody struct {
	List []feedItemBody `json:"list"`
}

type feedSettingsBody struct {
	LoopPlay      bool `json:"loop_play"`
	LoopEffective bool `json:"loop_effective"`
	AutoplayNext  bool `json:"autoplay_next"`
	SeekSeconds   int  `json:"seek_seconds"`
}

type feedMetaBody struct {
	NextCursor string           `json:"next_cursor"`
	HasMore    bool             `json:"has_more"`
	Seed       string           `json:"seed"`
	Played     int              `json:"played"`
	Rotated    bool             `json:"rotated"`
	Scope      string           `json:"scope"`
	Settings   feedSettingsBody `json:"settings"`
}

// feedPage fetches one feed page for the default client.
func feedPage(t *testing.T, e *env, scope, cursor string, limit int) (feedBody, feedMetaBody) {
	t.Helper()
	q := url.Values{}
	q.Set("limit", fmt.Sprint(limit))
	if scope != "" {
		q.Set("library_id", scope)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	res, env, raw := e.do(http.MethodGet, "/api/v1/feed/next?"+q.Encode(), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("feed 状态 %d: %s", res.StatusCode, raw)
	}
	var body feedBody
	decodeInto(t, env.Data, &body)
	var meta feedMetaBody
	decodeInto(t, env.Meta, &meta)
	return body, meta
}

// 门禁：默认随机 —— 同一 seed 一轮内不重复，播完自动换新 seed 进入下一轮。
func TestFeedRandomCycleNoRepeatThenRotatesSeed(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("A", filepath.Join(e.Root, "a"))
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		m := e.newMedia(lib.ID, filepath.Join(e.Root, "a", fmt.Sprintf("c%d.mp4", i)), []byte("clip"))
		want[m.ID] = true
	}

	seen := map[string]int{}
	cursor, seed := "", ""
	for page := 0; page < 3; page++ {
		body, meta := feedPage(t, e, "", cursor, 2)
		if meta.Rotated {
			t.Fatalf("第 %d 页就换轮了：还没播完", page+1)
		}
		if seed == "" {
			seed = meta.Seed
			if meta.Settings.AutoplayNext != true || meta.Settings.LoopPlay != false {
				t.Fatalf("默认设置应是 连播开 / 循环关: %+v", meta.Settings)
			}
		}
		if meta.Seed != seed {
			t.Fatalf("同一轮内 seed 变了: %s -> %s", seed, meta.Seed)
		}
		if !meta.HasMore {
			t.Fatal("还有内容时 has_more 必须为 true")
		}
		for _, item := range body.List {
			seen[item.ID]++
		}
		cursor = meta.NextCursor
	}
	if len(seen) != len(want) {
		t.Fatalf("一轮覆盖 %d 条, 期望 %d", len(seen), len(want))
	}
	for id, n := range seen {
		if !want[id] {
			t.Fatalf("返回了无关媒体 %s", id)
		}
		if n != 1 {
			t.Fatalf("播完前重复返回 %s（%d 次）", id, n)
		}
	}

	// 游标已到本轮末尾：下一次必须换新 seed 并给出新一轮第一批。
	body, meta := feedPage(t, e, "", cursor, 2)
	if !meta.Rotated {
		t.Fatal("一轮播完必须换新 seed")
	}
	if meta.Seed == seed {
		t.Fatalf("换轮后 seed 未变化: %s", seed)
	}
	if len(body.List) == 0 {
		t.Fatal("换轮后应返回新一轮的第一批")
	}
	if meta.Played != len(body.List) {
		t.Fatalf("played = %d, 期望 %d", meta.Played, len(body.List))
	}
	for _, item := range body.List {
		if !want[item.ID] {
			t.Fatalf("新一轮返回了无关媒体 %s", item.ID)
		}
	}
}

// 门禁：播放页显示"来自 <库名>"，library_id 范围只给该库，未知库不静默放行。
func TestFeedScopeIsolatesLibraries(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libA := e.newLibrary("A库", filepath.Join(e.Root, "a"))
	libB := e.newLibrary("B库", filepath.Join(e.Root, "b"))
	ma := e.newMedia(libA.ID, filepath.Join(e.Root, "a", "1.mp4"), []byte("a"))
	mb := e.newMedia(libB.ID, filepath.Join(e.Root, "b", "1.mp4"), []byte("b"))

	all, _ := feedPage(t, e, "", "", 10)
	if len(all.List) != 2 {
		t.Fatalf("全部库 = %d 条, 期望 2", len(all.List))
	}
	names := map[string]string{}
	for _, item := range all.List {
		names[item.ID] = item.LibraryName
	}
	if names[ma.ID] != "A库" || names[mb.ID] != "B库" {
		t.Fatalf("库名没有带到播放页: %v", names)
	}

	scoped, _ := feedPage(t, e, libA.ID, "", 10)
	if len(scoped.List) != 1 || scoped.List[0].ID != ma.ID {
		t.Fatalf("锁定库 A 后返回 %+v", scoped.List)
	}
	if scoped.List[0].LibraryID != libA.ID || scoped.List[0].LibraryName != "A库" {
		t.Fatalf("范围切换后的库信息不对: %+v", scoped.List[0])
	}

	res, _, raw := e.do(http.MethodGet, "/api/v1/feed/next?library_id=lib_nope", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("未知媒体库状态 = %d, 期望 404: %s", res.StatusCode, raw)
	}
}

// 门禁：循环/连播设置按用户持久化，且连播开时循环不生效。
func TestFeedSettingsPersistPerUser(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()

	res, env, raw := e.write(http.MethodPatch, "/api/v1/feed/settings",
		map[string]any{"loop_play": true, "autoplay_next": false})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PATCH 设置状态 = %d: %s", res.StatusCode, raw)
	}
	var got feedSettingsBody
	decodeInto(t, env.Data, &got)
	if !got.LoopPlay || got.AutoplayNext || !got.LoopEffective {
		t.Fatalf("连播关时循环应生效: %+v", got)
	}

	_, env, _ = e.write(http.MethodPatch, "/api/v1/feed/settings", map[string]any{"autoplay_next": true})
	decodeInto(t, env.Data, &got)
	if !got.LoopPlay || !got.AutoplayNext || got.LoopEffective {
		t.Fatalf("连播开时循环必须无效（但设置值保留）: %+v", got)
	}

	_, env, _ = e.do(http.MethodGet, "/api/v1/feed/settings", nil)
	var read feedSettingsBody
	decodeInto(t, env.Data, &read)
	if read != got {
		t.Fatalf("设置没有回读一致: %+v vs %+v", read, got)
	}

	// 第二个用户必须是自己的默认值，不受前一个用户影响。
	res, _, raw = e.write(http.MethodPost, "/api/v1/users", map[string]any{
		"username": "bob", "password": "bobpass123", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("建用户状态 = %d: %s", res.StatusCode, raw)
	}
	client := e.anonClient()
	if res, raw := e.loginAs(client, "bob", "bobpass123"); res.StatusCode != http.StatusOK {
		t.Fatalf("登录失败 %d: %s", res.StatusCode, raw)
	}
	_, envB, rawB := e.doAs(client, http.MethodGet, "/api/v1/feed/settings")
	var bob feedSettingsBody
	decodeInto(t, envB.Data, &bob)
	if bob.LoopPlay || !bob.AutoplayNext {
		t.Fatalf("新用户应是默认设置（循环关/连播开）: %+v (%s)", bob, rawB)
	}
}

// 门禁：左右键跳转秒数默认 10；PATCH 后回读一致；越界（0/999）保留原值。
func TestFeedSeekSecondsDefaultPersistAndOutOfRange(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()

	get := func() feedSettingsBody {
		t.Helper()
		res, env, raw := e.do(http.MethodGet, "/api/v1/feed/settings", nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET 设置状态 = %d: %s", res.StatusCode, raw)
		}
		var got feedSettingsBody
		decodeInto(t, env.Data, &got)
		return got
	}
	patch := func(value int) feedSettingsBody {
		t.Helper()
		res, env, raw := e.write(http.MethodPatch, "/api/v1/feed/settings",
			map[string]any{"seek_seconds": value})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("PATCH seek_seconds=%d 状态 = %d: %s", value, res.StatusCode, raw)
		}
		var got feedSettingsBody
		decodeInto(t, env.Data, &got)
		return got
	}

	if got := get(); got.SeekSeconds != 10 {
		t.Fatalf("新用户默认跳转秒数 = %d, 期望 10", got.SeekSeconds)
	}
	if got := patch(15); got.SeekSeconds != 15 {
		t.Fatalf("PATCH 15 后 = %d, 期望 15", got.SeekSeconds)
	}
	if got := get(); got.SeekSeconds != 15 {
		t.Fatalf("PATCH 15 回读 = %d, 期望 15", got.SeekSeconds)
	}
	// 越界语义 = 保留原值：不 clamp 到 1/120，也不回落默认 10。
	if got := patch(0); got.SeekSeconds != 15 {
		t.Fatalf("PATCH 0 后 = %d, 期望保留原值 15", got.SeekSeconds)
	}
	if got := patch(999); got.SeekSeconds != 15 {
		t.Fatalf("PATCH 999 后 = %d, 期望保留原值 15", got.SeekSeconds)
	}
	if got := get(); got.SeekSeconds != 15 {
		t.Fatalf("越界 PATCH 回读 = %d, 期望仍为 15", got.SeekSeconds)
	}
	// 边界值本身必须被接受。
	if got := patch(1); got.SeekSeconds != 1 {
		t.Fatalf("PATCH 1 后 = %d, 期望 1", got.SeekSeconds)
	}
	if got := patch(120); got.SeekSeconds != 120 {
		t.Fatalf("PATCH 120 后 = %d, 期望 120", got.SeekSeconds)
	}
}

// 门禁：左右键跳转不许只写一半 —— 两个播放器都要接 ArrowLeft/ArrowRight，
// 且前后端共用 seek_seconds 字段名。
func TestSeekKeysWiredInBothPlayersAndBackend(t *testing.T) {
	for _, name := range []string{
		filepath.Join("..", "web", "assets", "js", "feed.js"),
		filepath.Join("..", "web", "assets", "js", "series.js"),
	} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		body := string(raw)
		for _, token := range []string{"ArrowLeft", "ArrowRight", "seek_seconds"} {
			if !strings.Contains(body, token) {
				t.Fatalf("%s 缺少 %q —— 左右键跳转只写了一半", name, token)
			}
		}
	}
	raw, err := os.ReadFile("handlers_feed.go")
	if err != nil {
		t.Fatalf("读取 handlers_feed.go 失败: %v", err)
	}
	if !strings.Contains(string(raw), `"seek_seconds"`) {
		t.Fatal("handlers_feed.go 的响应体没有 seek_seconds 字段")
	}
}

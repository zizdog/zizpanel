package api_test

import (
	"net/http"
	"path/filepath"
	"testing"
)

// 收藏页三 Tab 的清除记录门禁：条数必须如实回传，清完确实为空。

func (e *env) seedUserRecords(t *testing.T) {
	t.Helper()
	lib := e.newLibrary("记录库", e.Root)
	a := e.newMedia(lib.ID, filepath.Join(e.Root, "a.mp4"), []byte("a"))
	b := e.newMedia(lib.ID, filepath.Join(e.Root, "b.mp4"), []byte("b"))
	for _, id := range []string{a.ID, b.ID} {
		if res, _, raw := e.write(http.MethodPost, "/api/v1/me/favorites/"+id, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("加收藏失败 %d: %s", res.StatusCode, raw)
		}
		if res, _, raw := e.write(http.MethodPatch, "/api/v1/me/progress/"+id,
			map[string]any{"position_ms": 1000, "duration_ms": 5000, "completed": false}); res.StatusCode != http.StatusOK {
			t.Fatalf("写进度失败 %d: %s", res.StatusCode, raw)
		}
	}
	if res, _, raw := e.write(http.MethodPost, "/api/v1/media/"+a.ID+"/reactions",
		map[string]string{"kind": "like"}); res.StatusCode != http.StatusOK {
		t.Fatalf("点赞失败 %d: %s", res.StatusCode, raw)
	}
	if res, _, raw := e.write(http.MethodPost, "/api/v1/media/"+b.ID+"/reactions",
		map[string]string{"kind": "dislike"}); res.StatusCode != http.StatusOK {
		t.Fatalf("点踩失败 %d: %s", res.StatusCode, raw)
	}
}

func (e *env) listCount(path string) int {
	e.t.Helper()
	res, env, raw := e.do(http.MethodGet, path, nil)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("GET %s 失败 %d: %s", path, res.StatusCode, raw)
	}
	var body struct {
		List []struct {
			ID string `json:"id"`
		} `json:"list"`
	}
	decodeInto(e.t, env.Data, &body)
	return len(body.List)
}

func (e *env) clearCount(path string) int64 {
	e.t.Helper()
	res, env, raw := e.write(http.MethodDelete, path, nil)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("DELETE %s 失败 %d: %s", path, res.StatusCode, raw)
	}
	var body struct {
		Cleared int64 `json:"cleared"`
	}
	decodeInto(e.t, env.Data, &body)
	return body.Cleared
}

func TestClearFavoritesReportsRealCount(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	e.seedUserRecords(t)

	if n := e.listCount("/api/v1/me/favorites"); n != 2 {
		t.Fatalf("收藏列表 = %d, 期望 2", n)
	}
	if n := e.clearCount("/api/v1/me/favorites"); n != 2 {
		t.Fatalf("清除收藏条数 = %d, 期望 2", n)
	}
	if n := e.listCount("/api/v1/me/favorites"); n != 0 {
		t.Fatalf("清除后收藏 = %d, 期望 0", n)
	}
	// 再清一次必须报 0，不能谎报成功。
	if n := e.clearCount("/api/v1/me/favorites"); n != 0 {
		t.Fatalf("重复清除条数 = %d, 期望 0", n)
	}
}

func TestClearLikesOnlyRemovesLikes(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	e.seedUserRecords(t)

	if n := e.listCount("/api/v1/me/likes"); n != 1 {
		t.Fatalf("点赞列表 = %d, 期望 1（点踩不算点赞）", n)
	}
	if n := e.clearCount("/api/v1/me/likes"); n != 1 {
		t.Fatalf("清除点赞条数 = %d, 期望 1", n)
	}
	if n := e.listCount("/api/v1/me/likes"); n != 0 {
		t.Fatalf("清除后点赞 = %d, 期望 0", n)
	}
	// 点踩不能被"清除点赞"顺手删掉。
	var kind string
	if err := e.DB.QueryRow(`SELECT kind FROM reactions LIMIT 1`).Scan(&kind); err != nil {
		t.Fatalf("点踩记录被误删: %v", err)
	}
	if kind != "dislike" {
		t.Fatalf("残留反应 = %q, 期望 dislike", kind)
	}
}

func TestClearProgressReportsRealCount(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	e.seedUserRecords(t)

	if n := e.listCount("/api/v1/me/progress"); n != 2 {
		t.Fatalf("历史列表 = %d, 期望 2", n)
	}
	if n := e.clearCount("/api/v1/me/progress"); n != 2 {
		t.Fatalf("清除历史条数 = %d, 期望 2", n)
	}
	if n := e.listCount("/api/v1/me/progress"); n != 0 {
		t.Fatalf("清除后历史 = %d, 期望 0", n)
	}
}

func TestClearRecordsRequiresAuth(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	anon := e.anonClient()
	for _, path := range []string{"/api/v1/me/favorites", "/api/v1/me/likes", "/api/v1/me/progress"} {
		res, _ := e.callWith(anon, "", http.MethodDelete, path, nil, false)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("未登录 DELETE %s 状态 = %d, 期望 401", path, res.StatusCode)
		}
	}
	// 未登录也不允许偷看别人的记录列表。
	for _, path := range []string{"/api/v1/me/favorites", "/api/v1/me/likes", "/api/v1/me/progress"} {
		res, _, _ := e.doAs(anon, http.MethodGet, path)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("未登录 GET %s 状态 = %d, 期望 401", path, res.StatusCode)
		}
	}
}

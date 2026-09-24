package web

// api_favorites_gate_test.go —— 「⭐ 目录收藏」的唯一门禁。
//
// 为什么现有门禁抓不到：收藏是**新存储点**（settings KV 里的一份路径列表），
// 此前没有任何门禁断言"存进 KV 的路径必须过文件白名单"。少这一条，
// 收藏夹就会变成绕过根目录限制的任意路径存储点 —— 而且是**服务端持久化**的，
// 手机上看得到、重启也不消失，比一次越界请求更危险。
//
// 一条门禁覆盖整类问题：往返（add/list/remove）+ 白名单外必被拒 +
// 目录不存在时如实标记（且允许移除）。全用 t.TempDir()，不碰用户真实目录。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// favCall 直接调 handler（绕过鉴权，与既有 API 单测同一做法）。
func favCall(t *testing.T, h http.HandlerFunc, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/api/v1/files/favorites", rdr)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// TestFileFavoritesGate 是这一条门禁。
func TestFileFavoritesGate(t *testing.T) {
	srv, _ := newTestServer(t)

	inside := filepath.Join(srv.Cfg.WWWRoot, "fav-me")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	// 接口存的是 Resolve 之后的**真实路径**（macOS 上 /tmp 是 /private/tmp 的软链接），
	// 断言必须按同一个口径比，否则比的是两串不同的字符串。
	inside = resolveForCompare(inside)
	// 白名单外的目录（t.TempDir() 在 /var/folders/… 下，不属于任何文件根）。
	outside := t.TempDir()

	t.Run("① 往返：收藏 → 列出 → 取消收藏", func(t *testing.T) {
		rec := favCall(t, srv.handleFileFavoritesSet, http.MethodPost,
			map[string]any{"action": "add", "path": inside})
		if rec.Code != http.StatusOK {
			t.Fatalf("收藏失败：%d %s", rec.Code, rec.Body.String())
		}
		if !favHas(rec.Body.Bytes(), inside, true) {
			t.Fatalf("收藏后列表里没有它：%s", rec.Body.String())
		}
		// 重复收藏不该产生第二条（幂等）。
		rec = favCall(t, srv.handleFileFavoritesSet, http.MethodPost,
			map[string]any{"action": "add", "path": inside})
		if n := favCount(rec.Body.Bytes()); n != 1 {
			t.Fatalf("重复收藏产生了 %d 条（应为 1）：%s", n, rec.Body.String())
		}
		// GET 也要看得到（服务端存储：换设备/刷新页面后仍在）。
		rec = favCall(t, srv.handleFileFavoritesList, http.MethodGet, nil)
		if !favHas(rec.Body.Bytes(), inside, true) {
			t.Fatalf("GET 列表里没有它：%s", rec.Body.String())
		}
		// 取消收藏。
		rec = favCall(t, srv.handleFileFavoritesSet, http.MethodPost,
			map[string]any{"action": "remove", "path": inside})
		if rec.Code != http.StatusOK {
			t.Fatalf("取消收藏失败：%d %s", rec.Code, rec.Body.String())
		}
		if favCount(rec.Body.Bytes()) != 0 {
			t.Fatalf("取消收藏后还留着：%s", rec.Body.String())
		}
	})

	t.Run("② 白名单外的路径必须被拒（add 与 remove 都不许存进去）", func(t *testing.T) {
		rec := favCall(t, srv.handleFileFavoritesSet, http.MethodPost,
			map[string]any{"action": "add", "path": outside})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("白名单外的目录必须 403，实际 %d：%s", rec.Code, rec.Body.String())
		}
		rec = favCall(t, srv.handleFileFavoritesSet, http.MethodPost,
			map[string]any{"action": "remove", "path": outside})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("remove 也必须过白名单（不许拿收藏夹探测任意路径），实际 %d", rec.Code)
		}
		// 不存在的目录不能收藏（否则列表里立刻多一条假条目）。
		rec = favCall(t, srv.handleFileFavoritesSet, http.MethodPost,
			map[string]any{"action": "add", "path": filepath.Join(srv.Cfg.WWWRoot, "nope-not-here")})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("不存在的目录不该能收藏，实际 %d", rec.Code)
		}
	})

	t.Run("③ 目录不存在时如实标记，且仍能移除", func(t *testing.T) {
		gone := filepath.Join(srv.Cfg.WWWRoot, "will-vanish")
		if err := os.MkdirAll(gone, 0o755); err != nil {
			t.Fatal(err)
		}
		gone = resolveForCompare(gone)
		rec := favCall(t, srv.handleFileFavoritesSet, http.MethodPost,
			map[string]any{"action": "add", "path": gone})
		if rec.Code != http.StatusOK {
			t.Fatalf("收藏失败：%d %s", rec.Code, rec.Body.String())
		}
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
		// 关键断言：条目**不许静默消失**，必须还在列表里并标"目录不存在"。
		rec = favCall(t, srv.handleFileFavoritesList, http.MethodGet, nil)
		body := rec.Body.Bytes()
		if !favPresent(body, gone) {
			t.Fatalf("目录没了以后条目静默消失了：%s", string(body))
		}
		if !favHas(body, gone, false) {
			t.Fatalf("必须如实标 exists=false：%s", string(body))
		}
		if !favReasonContains(body, gone, "目录不存在") {
			t.Fatalf("必须如实标原因「目录不存在」：%s", string(body))
		}
		// 且允许移除（否则这条死条目永远清不掉）。
		rec = favCall(t, srv.handleFileFavoritesSet, http.MethodPost,
			map[string]any{"action": "remove", "path": gone})
		if rec.Code != http.StatusOK {
			t.Fatalf("已不存在的目录必须允许移除，实际 %d：%s", rec.Code, rec.Body.String())
		}
		if favPresent(rec.Body.Bytes(), gone) {
			t.Fatalf("移除后还在：%s", rec.Body.String())
		}
	})
}

func favCount(raw []byte) int {
	var body struct {
		Data struct {
			Items []favoriteItem `json:"items"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &body)
	return len(body.Data.Items)
}

func favPresent(raw []byte, path string) bool {
	var body struct {
		Data struct {
			Items []favoriteItem `json:"items"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &body)
	for _, it := range body.Data.Items {
		if it.Path == path {
			return true
		}
	}
	return false
}

func favHas(raw []byte, path string, exists bool) bool {
	var body struct {
		Data struct {
			Items []favoriteItem `json:"items"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &body)
	for _, it := range body.Data.Items {
		if it.Path == path && it.Exists == exists {
			return true
		}
	}
	return false
}

func favReasonContains(raw []byte, path, want string) bool {
	var body struct {
		Data struct {
			Items []favoriteItem `json:"items"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &body)
	for _, it := range body.Data.Items {
		if it.Path == path && it.Reason != "" {
			return bytes.Contains([]byte(it.Reason), []byte(want))
		}
	}
	return false
}

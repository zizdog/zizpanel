package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/files"
)

// ============================================================================
//  文件管理 → ⭐ 目录收藏
//
//  存服务端（settings KV，键 file_favorites）：手机上打开面板也能看到同一份。
//  路径必须过**与文件操作同一套**白名单校验（files.Manager.Resolve），
//  所以收藏不能成为"绕过根目录限制拿到任意路径"的入口。
//
//  诚实口径：目录被删/被移走/不再允许访问时，条目**仍然列出来**并如实标原因
//  （"目录不存在"），允许用户移除；绝不静默消失、也绝不假装能跳过去。
// ============================================================================

// fileFavoritesKey 是 settings 表里的键（值 = []fileFavorite 的 JSON）。
const fileFavoritesKey = "file_favorites"

// fileFavoritesMax 是收藏数量上限（settings 是一行 KV，别让它无限长大）。
const fileFavoritesMax = 100

// fileFavMu 串行化"读-改-写"：settings KV 没有事务性的读改写，
// 两个并发请求同时加收藏会丢一个。
var fileFavMu sync.Mutex

// fileFavorite 是落库的一条收藏。
type fileFavorite struct {
	Path    string `json:"path"`
	AddedAt string `json:"added_at,omitempty"`
}

// favoriteItem 是返回给前端的一条（带**真实**存在性结论）。
type favoriteItem struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Reason string `json:"reason,omitempty"`
}

// favoritesPayload 是 GET/POST /files/favorites 的响应体。
type favoritesPayload struct {
	Items   []favoriteItem `json:"items"`
	Missing int            `json:"missing"`
	Max     int            `json:"max"`
}

type favoriteReq struct {
	// Action 是 "add" 或 "remove"（一个接口两种动作，语义写在这里）。
	Action string `json:"action"`
	Path   string `json:"path"`
}

// loadFavorites 读收藏列表。
func (s *Server) loadFavorites(r *http.Request) ([]fileFavorite, error) {
	raw, err := s.Store.GetSetting(r.Context(), fileFavoritesKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return []fileFavorite{}, nil
	}
	var list []fileFavorite
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("收藏数据损坏（%s）: %w", fileFavoritesKey, err)
	}
	return list, nil
}

// saveFavorites 写收藏列表。
func (s *Server) saveFavorites(r *http.Request, list []fileFavorite) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.Store.SetSetting(r.Context(), fileFavoritesKey, string(b))
}

// favoriteStatus 判断一条收藏此刻的真实状态（不存在的原因要如实、可操作）。
func (s *Server) favoriteStatus(path string) (bool, string) {
	mgr := s.fileManager()
	if _, err := mgr.Resolve(path, false); err != nil {
		switch {
		case errors.Is(err, files.ErrForbidden):
			return false, "不在允许访问的范围内"
		case errors.Is(err, fs.ErrPermission):
			return false, "读不到（权限被拒）"
		default:
			return false, "目录不存在"
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, "目录不存在"
	}
	if !st.IsDir() {
		return false, "已不是目录"
	}
	return true, ""
}

// favoritesView 把落库列表转成带状态的响应。
func (s *Server) favoritesView(r *http.Request, list []fileFavorite) favoritesPayload {
	out := favoritesPayload{Items: make([]favoriteItem, 0, len(list)), Max: fileFavoritesMax}
	for _, f := range list {
		it := favoriteItem{Path: f.Path, Name: filepath.Base(f.Path)}
		it.Exists, it.Reason = s.favoriteStatus(f.Path)
		if !it.Exists {
			out.Missing++
		}
		out.Items = append(out.Items, it)
	}
	return out
}

// handleFileFavoritesList 列出收藏（目录不存在也照列，带原因）。
func (s *Server) handleFileFavoritesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.loadFavorites(r)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, s.favoritesView(r, list))
}

// handleFileFavoritesSet 添加或取消收藏（{action:"add"|"remove", path}）。
//
// add：路径必须真实存在、是目录、且在白名单内（Resolve 校验根目录与软链接）。
// remove：允许目录已经不存在 —— 否则"删掉的目录"永远清不掉（那是静默残留，不是安全）。
// 两种动作都要过白名单：收藏夹不能变成任意路径的存储点。
func (s *Server) handleFileFavoritesSet(w http.ResponseWriter, r *http.Request) {
	var req favoriteReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action != "add" && action != "remove" {
		fail(w, http.StatusBadRequest, "action 只能是 add 或 remove")
		return
	}
	mgr := s.fileManager()

	// remove 允许目标已不存在（allowMissing=true），但白名单照样生效。
	real, err := mgr.Resolve(req.Path, action == "remove")
	if err != nil {
		failFileErr(w, err, req.Path)
		return
	}
	if action == "add" {
		st, serr := os.Stat(real)
		if serr != nil || !st.IsDir() {
			fail(w, http.StatusBadRequest, "只能收藏目录")
			return
		}
	}

	fileFavMu.Lock()
	list, lerr := s.loadFavorites(r)
	if lerr != nil {
		fileFavMu.Unlock()
		fail(w, http.StatusInternalServerError, lerr.Error())
		return
	}
	if action == "add" {
		found := false
		for _, f := range list {
			if f.Path == real {
				found = true
				break
			}
		}
		if !found && len(list) >= fileFavoritesMax {
			fileFavMu.Unlock()
			fail(w, http.StatusBadRequest, fmt.Sprintf("收藏最多 %d 个，请先移除一些", fileFavoritesMax))
			return
		}
		if !found {
			list = append(list, fileFavorite{Path: real, AddedAt: time.Now().Format(time.RFC3339)})
			if serr := s.saveFavorites(r, list); serr != nil {
				fileFavMu.Unlock()
				fail(w, http.StatusInternalServerError, "保存收藏失败: "+serr.Error())
				return
			}
		}
	} else {
		kept := make([]fileFavorite, 0, len(list))
		for _, f := range list {
			if f.Path == real || f.Path == filepath.Clean(req.Path) {
				continue
			}
			kept = append(kept, f)
		}
		list = kept
		if serr := s.saveFavorites(r, list); serr != nil {
			fileFavMu.Unlock()
			fail(w, http.StatusInternalServerError, "保存收藏失败: "+serr.Error())
			return
		}
	}
	fileFavMu.Unlock()

	detail := "收藏目录"
	if action == "remove" {
		detail = "取消收藏"
	}
	s.audit(r, "file_favorite", real, detail, true, "")
	ok(w, s.favoritesView(r, list))
}

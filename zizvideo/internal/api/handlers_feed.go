package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/storage"
)

// feedItem is mediaItem plus the owning library name shown on the play page.
type feedItem struct {
	mediaItem
	LibraryName string `json:"library_name"`
}

// buildFeedItems decorates feed rows with their library name (item 13).
func (s *Server) buildFeedItems(rows []domain.Media, r *http.Request) []feedItem {
	base := s.buildItems(rows, r, false)
	names := map[string]string{}
	if libs, err := s.DB.ListLibraries(); err == nil {
		for _, l := range libs {
			names[l.ID] = l.Name
		}
	}
	out := make([]feedItem, 0, len(base))
	for _, item := range base {
		out = append(out, feedItem{mediaItem: item, LibraryName: names[item.LibraryID]})
	}
	return out
}

// HandleFeedNext serves the next batch of the caller's seeded random cycle.
//
// 一轮内不重复：seed 固定时顺序确定、游标只向前；游标走完说明本轮播完，
// 换新 seed 重开一轮。scope（library_id）各自独立一轮。
func (s *Server) HandleFeedNext(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	scope := strings.TrimSpace(r.URL.Query().Get("library_id"))
	if scope != "" {
		if _, err := s.DB.GetLibrary(scope); err != nil {
			s.fail(w, r, domain.ErrNotFound)
			return
		}
	}
	limit := queryInt(r, "limit", 10, 1, 50)
	st, err := s.DB.GetFeedState(u.ID, scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// 客户端游标只在同一轮（seed 相同）内可信；其它情况以服务端游标为准。
	if hash, id, seed, ok := parseFeedCursor(r.URL.Query().Get("cursor")); ok && seed == st.Seed {
		st.CursorHash, st.CursorID = hash, id
	}
	rows, err := s.DB.FeedPage(scope, st.Seed, st.CursorHash, st.CursorID, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rotated := false
	if len(rows) == 0 && st.Played > 0 {
		st.Seed, st.CursorHash, st.CursorID, st.Played = domain.NewID("s"), 0, "", 0
		rows, err = s.DB.FeedPage(scope, st.Seed, 0, "", limit)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		rotated = true
	}
	next := ""
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		st.CursorHash, st.CursorID = storage.FeedOrderKey(last.ID, st.Seed), last.ID
		st.Played += len(rows)
		next = formatFeedCursor(st.CursorHash, last.ID, st.Seed)
	}
	if err := s.DB.SaveFeedState(st); err != nil {
		s.fail(w, r, err)
		return
	}
	prefs, err := s.DB.GetUserPrefs(u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	total, err := s.DB.CountPlayable(scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"list": s.buildFeedItems(rows, r)}, map[string]any{
		"next_cursor": next,
		"has_more":    total > 0,
		"seed":        st.Seed,
		"played":      st.Played,
		"rotated":     rotated,
		"scope":       scope,
		"settings":    feedSettingsBody(prefs),
	})
}

// HandleGetFeedSettings returns the caller's player settings.
func (s *Server) HandleGetFeedSettings(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	prefs, err := s.DB.GetUserPrefs(u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, feedSettingsBody(prefs), nil)
}

type feedSettingsReq struct {
	LoopPlay     *bool `json:"loop_play"`
	AutoplayNext *bool `json:"autoplay_next"`
}

// HandlePatchFeedSettings persists the player settings for one user.
func (s *Server) HandlePatchFeedSettings(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	var req feedSettingsReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	prefs, err := s.DB.GetUserPrefs(u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.LoopPlay != nil {
		prefs.LoopPlay = *req.LoopPlay
	}
	if req.AutoplayNext != nil {
		prefs.AutoplayNext = *req.AutoplayNext
	}
	if err := s.DB.SaveUserPrefs(u.ID, prefs); err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, feedSettingsBody(prefs), nil)
}

// feedSettingsBody reports stored settings plus the effective loop flag:
// 连播（自动播放下一个）开着时循环播放不生效。
func feedSettingsBody(p *storage.UserPrefs) map[string]any {
	return map[string]any{
		"loop_play":      p.LoopPlay,
		"loop_effective": p.LoopPlay && !p.AutoplayNext,
		"autoplay_next":  p.AutoplayNext,
	}
}

// formatFeedCursor packs the (hash, id, seed) position into one opaque string.
func formatFeedCursor(hash int64, id, seed string) string {
	return strconv.FormatInt(hash, 10) + "." + id + "." + seed
}

// parseFeedCursor reads that string back; parse failures mean "no cursor".
func parseFeedCursor(raw string) (int64, string, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(raw), ".", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return 0, "", "", false
	}
	hash, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || hash < 0 {
		return 0, "", "", false
	}
	return hash, parts[1], parts[2], true
}

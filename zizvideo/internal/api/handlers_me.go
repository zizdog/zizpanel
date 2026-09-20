package api

import (
	"net/http"

	"github.com/zizdog/zizvideo/internal/domain"
)

type progressReq struct {
	PositionMS int64 `json:"position_ms"`
	DurationMS int64 `json:"duration_ms"`
	Completed  bool  `json:"completed"`
}

// HandlePatchProgress stores the caller's playback position.
func (s *Server) HandlePatchProgress(w http.ResponseWriter, r *http.Request) {
	mediaID := r.PathValue("mediaId")
	var req progressReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.PositionMS < 0 {
		s.fail(w, r, domain.New("VALIDATION_POSITION", "进度不能为负", 400))
		return
	}
	u := UserFrom(r.Context())
	if _, err := s.canAccessMedia(r.Context(), mediaID); err != nil {
		s.fail(w, r, err)
		return
	}
	p := &domain.Progress{
		UserID: u.ID, MediaID: mediaID, PositionMS: req.PositionMS,
		DurationMS: req.DurationMS, Completed: req.Completed,
	}
	if err := s.DB.UpsertProgress(p); err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{
		"position_ms": req.PositionMS, "completed": req.Completed}, nil)
}

// HandleListProgress returns the caller's most recent positions.
func (s *Server) HandleListProgress(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	limit := queryInt(r, "limit", 50, 1, 200)
	progs, medias, err := s.DB.ListProgress(ScopeFrom(r.Context()), u.ID, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := s.buildItems(medias, r, false)
	list := make([]map[string]any, 0, len(items))
	for i, item := range items {
		list = append(list, map[string]any{
			"media_id": item.ID, "position_ms": progs[i].PositionMS,
			"duration_ms": progs[i].DurationMS, "completed": progs[i].Completed,
			"updated_at": progs[i].UpdatedAt, "media": item,
		})
	}
	respond(w, http.StatusOK, map[string]any{"list": list}, nil)
}

// HandleAddFavorite marks a media item as favorite.
func (s *Server) HandleAddFavorite(w http.ResponseWriter, r *http.Request) {
	mediaID := r.PathValue("mediaId")
	if _, err := s.canAccessMedia(r.Context(), mediaID); err != nil {
		s.fail(w, r, err)
		return
	}
	u := UserFrom(r.Context())
	if err := s.DB.AddFavorite(u.ID, mediaID); err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"favorite": true}, nil)
}

// HandleRemoveFavorite clears the favorite mark.
func (s *Server) HandleRemoveFavorite(w http.ResponseWriter, r *http.Request) {
	mediaID := r.PathValue("mediaId")
	if _, err := s.canAccessMedia(r.Context(), mediaID); err != nil {
		s.fail(w, r, err)
		return
	}
	u := UserFrom(r.Context())
	if err := s.DB.RemoveFavorite(u.ID, mediaID); err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"favorite": false}, nil)
}

type reactionReq struct {
	Kind string `json:"kind"`
}

func validKind(kind string) bool { return kind == "like" || kind == "dislike" }

// HandleSetReaction stores like/dislike (POST and PATCH share this handler).
func (s *Server) HandleSetReaction(w http.ResponseWriter, r *http.Request) {
	mediaID := r.PathValue("id")
	var req reactionReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !validKind(req.Kind) {
		s.fail(w, r, domain.New("VALIDATION_REACTION", "只能点赞或点踩", 400))
		return
	}
	if _, err := s.canAccessMedia(r.Context(), mediaID); err != nil {
		s.fail(w, r, err)
		return
	}
	u := UserFrom(r.Context())
	if err := s.DB.SetReaction(u.ID, mediaID, req.Kind); err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"kind": req.Kind}, nil)
}

// HandleDeleteReaction removes the reaction.
func (s *Server) HandleDeleteReaction(w http.ResponseWriter, r *http.Request) {
	mediaID := r.PathValue("id")
	if _, err := s.canAccessMedia(r.Context(), mediaID); err != nil {
		s.fail(w, r, err)
		return
	}
	u := UserFrom(r.Context())
	if err := s.DB.ClearReaction(u.ID, mediaID); err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"kind": nil}, nil)
}

// ============================================================================
//  收藏页三个 Tab：点赞 / 收藏 / 历史（每个都能清除记录，条数如实回传）
// ============================================================================

// HandleListFavorites returns the caller's favorites, newest first.
func (s *Server) HandleListFavorites(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	limit := queryInt(r, "limit", 100, 1, 200)
	rows, err := s.DB.ListFavorites(ScopeFrom(r.Context()), u.ID, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"list": s.buildItems(rows, r, false)}, nil)
}

// HandleListLikes returns the caller's liked media, newest first.
func (s *Server) HandleListLikes(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	limit := queryInt(r, "limit", 100, 1, 200)
	rows, err := s.DB.ListLikes(ScopeFrom(r.Context()), u.ID, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"list": s.buildItems(rows, r, false)}, nil)
}

// HandleClearFavorites deletes every favorite row and reports the real count.
func (s *Server) HandleClearFavorites(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	n, err := s.DB.ClearFavorites(u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "me.favorites.clear", "user:"+u.ID, true, "")
	respond(w, http.StatusOK, map[string]any{"cleared": n}, nil)
}

// HandleClearLikes deletes every like row and reports the real count.
func (s *Server) HandleClearLikes(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	n, err := s.DB.ClearLikes(u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "me.likes.clear", "user:"+u.ID, true, "")
	respond(w, http.StatusOK, map[string]any{"cleared": n}, nil)
}

// HandleClearProgress deletes the caller's whole watch history.
func (s *Server) HandleClearProgress(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	n, err := s.DB.ClearProgress(u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "me.progress.clear", "user:"+u.ID, true, "")
	respond(w, http.StatusOK, map[string]any{"cleared": n}, nil)
}

// HandleMyLibraries returns the caller's visible libraries as [{id,name}]; the
// scope comes from the single判据, and root_path is structurally absent (E.2 #4).
func (s *Server) HandleMyLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.DB.ListLibrariesIn(ScopeFrom(r.Context()))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, libs, nil)
}

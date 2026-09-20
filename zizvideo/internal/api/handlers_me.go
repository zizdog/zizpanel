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
	if _, err := s.DB.GetMedia(mediaID); err != nil {
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
	progs, medias, err := s.DB.ListProgress(u.ID, limit)
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
	if _, err := s.DB.GetMedia(mediaID); err != nil {
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
	if _, err := s.DB.GetMedia(mediaID); err != nil {
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
	if _, err := s.DB.GetMedia(mediaID); err != nil {
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
	if _, err := s.DB.GetMedia(mediaID); err != nil {
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

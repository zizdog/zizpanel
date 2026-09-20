package api

import (
	"net/http"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/storage"
)

// 剧场（短剧）：剧集引用已有 media，不复制文件；顺序由 position 决定，
// 播放端按 position 顺序连播，与 feed 的随机 bag 完全无关（不吃随机）。

const (
	seriesTitleMax = 80
	seriesDescMax  = 500
	seriesMediaMax = 200
)

type seriesCreateReq struct {
	Title        string `json:"title"`
	Description  string `json:"description"`
	CoverMediaID string `json:"cover_media_id"`
	LibraryID    string `json:"library_id"`
	SortOrder    int    `json:"sort_order"`
}

type seriesPatchReq struct {
	Title        *string `json:"title"`
	Description  *string `json:"description"`
	CoverMediaID *string `json:"cover_media_id"`
	LibraryID    *string `json:"library_id"`
	SortOrder    *int    `json:"sort_order"`
}

type seriesMediaReq struct {
	MediaIDs []string `json:"media_ids"`
}

// seriesJSON is the wire shape shared by list and detail.
func seriesJSON(s domain.Series) map[string]any {
	coverURL := ""
	if s.CoverMediaID != "" {
		coverURL = "/api/v1/media/" + s.CoverMediaID + "/cover"
	}
	return map[string]any{
		"id": s.ID, "title": s.Title, "description": s.Description,
		"cover_media_id": s.CoverMediaID, "cover_url": coverURL,
		"library_id": s.LibraryID, "sort_order": s.SortOrder,
		"episode_count": s.EpisodeCount, "created_at": s.CreatedAt, "updated_at": s.UpdatedAt,
	}
}

// HandleListSeries returns every 剧场.
func (s *Server) HandleListSeries(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListSeries()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, item := range rows {
		list = append(list, seriesJSON(item))
	}
	respond(w, http.StatusOK, map[string]any{"list": list}, nil)
}

// HandleGetSeries returns one 剧场 with its episodes in playback order.
func (s *Server) HandleGetSeries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	series, err := s.DB.GetSeries(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	eps, medias, err := s.DB.ListSeriesEpisodes(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := UserFrom(r.Context())
	items := s.buildItems(medias, r, u != nil && u.Role == domain.RoleAdmin)
	list := make([]map[string]any, 0, len(items))
	for i, item := range items {
		list = append(list, map[string]any{
			"position": eps[i].Position, "episode": eps[i].Episode, "media": item,
		})
	}
	respond(w, http.StatusOK, map[string]any{"series": seriesJSON(*series), "list": list}, nil)
}

func (s *Server) seriesTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", domain.ErrSeriesTitle
	}
	if len([]rune(title)) > seriesTitleMax {
		return "", domain.New("SERIES_TITLE_TOO_LONG", "剧场标题不能超过 80 字", 400)
	}
	return title, nil
}

// HandleCreateSeries creates a 剧场 (admin only).
func (s *Server) HandleCreateSeries(w http.ResponseWriter, r *http.Request) {
	var req seriesCreateReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	title, err := s.seriesTitle(req.Title)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len([]rune(req.Description)) > seriesDescMax {
		s.fail(w, r, domain.New("SERIES_DESC_TOO_LONG", "简介不能超过 500 字", 400))
		return
	}
	if req.CoverMediaID != "" {
		if _, err := s.DB.GetMedia(req.CoverMediaID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if req.LibraryID != "" {
		if _, err := s.DB.GetLibrary(req.LibraryID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	series := &domain.Series{
		ID: domain.NewID("ser"), Title: title, Description: strings.TrimSpace(req.Description),
		CoverMediaID: req.CoverMediaID, LibraryID: req.LibraryID, SortOrder: req.SortOrder,
	}
	if err := s.DB.CreateSeries(series); err != nil {
		s.audit(r, "series.create", "series:"+series.ID, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	fresh, err := s.DB.GetSeries(series.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "series.create", "series:"+series.ID, true, "")
	respond(w, http.StatusCreated, seriesJSON(*fresh), nil)
}

// HandlePatchSeries applies a partial 剧场 update (admin only).
func (s *Server) HandlePatchSeries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req seriesPatchReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	patch := storage.SeriesPatch{}
	if req.Title != nil {
		title, err := s.seriesTitle(*req.Title)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		patch.Title = &title
	}
	if req.Description != nil {
		if len([]rune(*req.Description)) > seriesDescMax {
			s.fail(w, r, domain.New("SERIES_DESC_TOO_LONG", "简介不能超过 500 字", 400))
			return
		}
		desc := strings.TrimSpace(*req.Description)
		patch.Description = &desc
	}
	if req.CoverMediaID != nil {
		if *req.CoverMediaID != "" {
			if _, err := s.DB.GetMedia(*req.CoverMediaID); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		patch.CoverMediaID = req.CoverMediaID
	}
	if req.LibraryID != nil {
		if *req.LibraryID != "" {
			if _, err := s.DB.GetLibrary(*req.LibraryID); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		patch.LibraryID = req.LibraryID
	}
	if req.SortOrder != nil {
		patch.SortOrder = req.SortOrder
	}
	fresh, err := s.DB.UpdateSeries(id, patch)
	if err != nil {
		s.audit(r, "series.update", "series:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "series.update", "series:"+id, true, "")
	respond(w, http.StatusOK, seriesJSON(*fresh), nil)
}

// HandleDeleteSeries soft-deletes a 剧场; media rows and files stay untouched.
func (s *Server) HandleDeleteSeries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.DB.DeleteSeries(id); err != nil {
		s.audit(r, "series.delete", "series:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "series.delete", "series:"+id, true, "")
	respond(w, http.StatusOK, map[string]any{"deleted": true, "media_files_untouched": true}, nil)
}

// HandleAddSeriesMedia appends existing media to a 剧场 (admin only).
func (s *Server) HandleAddSeriesMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.DB.GetSeries(id); err != nil {
		s.fail(w, r, err)
		return
	}
	var req seriesMediaReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(req.MediaIDs) == 0 || len(req.MediaIDs) > seriesMediaMax {
		s.fail(w, r, domain.New("VALIDATION_SERIES_MEDIA", "每次需要 1-200 个媒体", 400))
		return
	}
	for _, mediaID := range req.MediaIDs {
		if _, err := s.DB.GetMedia(mediaID); err != nil {
			s.fail(w, r, domain.New("VALIDATION_NOT_FOUND", "媒体不存在: "+mediaID, 404))
			return
		}
	}
	added, err := s.DB.AddSeriesMedia(id, req.MediaIDs)
	if err != nil {
		s.audit(r, "series.media.add", "series:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	fresh, err := s.DB.GetSeries(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "series.media.add", "series:"+id, true, "")
	respond(w, http.StatusOK, map[string]any{
		"added": added, "requested": len(req.MediaIDs), "episode_count": fresh.EpisodeCount}, nil)
}

// HandleRemoveSeriesMedia drops one episode and compacts the order.
func (s *Server) HandleRemoveSeriesMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mediaID := r.PathValue("mediaId")
	if err := s.DB.RemoveSeriesMedia(id, mediaID); err != nil {
		s.audit(r, "series.media.remove", "series:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	fresh, err := s.DB.GetSeries(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "series.media.remove", "series:"+id, true, "")
	respond(w, http.StatusOK, map[string]any{"removed": true, "episode_count": fresh.EpisodeCount}, nil)
}

// HandleReorderSeries rewrites the episode order (admin only).
func (s *Server) HandleReorderSeries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.DB.GetSeries(id); err != nil {
		s.fail(w, r, err)
		return
	}
	var req seriesMediaReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.DB.ReorderSeries(id, req.MediaIDs); err != nil {
		s.audit(r, "series.reorder", "series:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	fresh, err := s.DB.GetSeries(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "series.reorder", "series:"+id, true, "")
	respond(w, http.StatusOK, map[string]any{
		"episode_count": fresh.EpisodeCount, "media_ids": req.MediaIDs}, nil)
}

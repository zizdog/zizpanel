package api

import (
	"net/http"
	"os"
	"strings"

	"github.com/zizdog/zizvideo/internal/detect"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
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

// seriesJSON is the wire shape shared by list and detail. cover_url 只在封面
// media 也落在 scope 内时才给（S4：封面可能属于别的库）。
func seriesJSON(s domain.Series, scope LibraryScope) map[string]any {
	coverURL := ""
	if s.CoverMediaID != "" && scope.Allows(s.CoverLibraryID) {
		coverURL = "/api/v1/media/" + s.CoverMediaID + "/cover"
	}
	// 剧场可能显式挂在别的库：范围外就不外发这个 id，避免反推存在性。
	libraryID := s.LibraryID
	if libraryID != "" && !scope.Allows(libraryID) {
		libraryID = ""
	}
	return map[string]any{
		"id": s.ID, "title": s.Title, "description": s.Description,
		"cover_media_id": s.CoverMediaID, "cover_url": coverURL,
		"library_id": libraryID, "sort_order": s.SortOrder,
		"episode_count": s.EpisodeCount, "created_at": s.CreatedAt, "updated_at": s.UpdatedAt,
	}
}

// HandleListSeries returns the caller's visible 剧场（列表静默过滤）。
func (s *Server) HandleListSeries(w http.ResponseWriter, r *http.Request) {
	scope := ScopeFrom(r.Context())
	rows, err := s.DB.ListSeries(scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, item := range rows {
		list = append(list, seriesJSON(item, scope))
	}
	respond(w, http.StatusOK, map[string]any{"list": list}, nil)
}

// HandleGetSeries returns one 剧场 with its episodes in playback order
// (补丁 R1：season→episode→文件名自然序，未识别排末尾).
func (s *Server) HandleGetSeries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	scope := ScopeFrom(r.Context())
	series, err := s.DB.GetSeries(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	eps, medias, err := s.DB.ListSeriesEpisodes(scope, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// 可见集为 0 ⇒ 普通用户视为不可见（与不存在同码）；admin 必须仍能打开管理
	// 抽屉（否则新建后还没加集的剧场无法管理，与 ListSeries 的管理端偏离一致）。
	if len(eps) == 0 && !scope.All {
		s.fail(w, r, domain.ErrNotFound)
		return
	}
	series.EpisodeCount = len(eps)
	eps, medias = sortSeriesEpisodes(eps, medias)
	u := UserFrom(r.Context())
	items := s.buildItems(medias, r, u != nil && u.Role == domain.RoleAdmin)
	list := make([]map[string]any, 0, len(items))
	for i, item := range items {
		list = append(list, seriesEpisodeJSON(eps[i], item))
	}
	respond(w, http.StatusOK, map[string]any{
		"series": seriesJSON(*series, scope), "list": list, "guide": s.seriesGuide()}, nil)
}

// guideStep is one concrete next action for an empty 剧场.
type guideStep struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// seriesGuide answers "新建剧场后下一步做什么" without making the user guess:
// where the files live, how filenames are recognised, and the two entry points.
// Naming mirrors internal/media.ParseEpisode（唯一内核，不许另编一套）.
type seriesGuide struct {
	Title       string      `json:"title"`
	Where       string      `json:"where"`
	Naming      string      `json:"naming"`
	NamingNote  string      `json:"naming_note"`
	Roots       []string    `json:"roots"`
	UsableRoots []string    `json:"usable_roots"`
	Steps       []guideStep `json:"steps"`
	AdminURL    string      `json:"admin_url"`
	RootsURL    string      `json:"roots_url"`
}

func (s *Server) seriesGuide() seriesGuide {
	all := s.Roots.List()
	usable := make([]string, 0, len(all))
	for _, root := range all {
		if info, err := os.Stat(root); err == nil && info.IsDir() && readableDir(root) {
			usable = append(usable, root)
		}
	}
	return seriesGuide{
		Title:       "这个剧场还没有剧集",
		Where:       "文件放在某个允许根目录下的子文件夹里",
		Naming:      "文件名带 S01E01、EP03、第3集，或结尾独立数字",
		NamingNote:  "只看文件名，识别不到留「未识别」，绝不猜",
		Roots:       all,
		UsableRoots: usable,
		Steps: []guideStep{
			{Key: "scan", Text: "先在后台「媒体库」建库指向该子文件夹并扫描"},
			{Key: "detect", Text: "剧场「管理」→ 自动识别剧集"},
			{Key: "add_existing", Text: "剧场「管理」→ 搜索并勾选 → 加入所选"},
		},
		AdminURL: "#/admin",
		RootsURL: "#/admin/roots",
	}
}

// seriesEpisodeJSON is one episode entry: numbers + the honest 人读标签.
func seriesEpisodeJSON(e domain.SeriesEpisode, item mediaItem) map[string]any {
	return map[string]any{
		"position": e.Position, "season": e.Season, "episode": e.Episode,
		"episode_source": e.EpisodeSource,
		"episode_label":  media.EpisodeLabel(e.Season, e.Episode),
		"media":          item,
	}
}

// sortSeriesEpisodes applies the playback order to the two parallel slices.
func sortSeriesEpisodes(eps []domain.SeriesEpisode, medias []domain.Media) ([]domain.SeriesEpisode, []domain.Media) {
	if len(eps) != len(medias) || len(eps) == 0 {
		return eps, medias
	}
	keys := make([]media.OrderKey, len(eps))
	for i := range eps {
		keys[i] = media.OrderKey{
			Season: eps[i].Season, Episode: eps[i].Episode,
			Manual:   eps[i].EpisodeSource == domain.EpisodeSourceManual,
			Position: eps[i].Position, Name: detect.MediaName(medias[i]),
		}
	}
	order := media.SortOrderKeys(keys)
	sortedEps := make([]domain.SeriesEpisode, len(eps))
	sortedMedias := make([]domain.Media, len(medias))
	for i, idx := range order {
		sortedEps[i] = eps[idx]
		sortedMedias[i] = medias[idx]
	}
	return sortedEps, sortedMedias
}

// seriesTitle validates and trims a 剧场 title.
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
		if _, err := s.DB.GetMediaIn(scopeAll(), req.CoverMediaID); err != nil {
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
	respond(w, http.StatusCreated, seriesJSON(*fresh, scopeAll()), nil)
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
			if _, err := s.DB.GetMediaIn(scopeAll(), *req.CoverMediaID); err != nil {
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
	respond(w, http.StatusOK, seriesJSON(*fresh, scopeAll()), nil)
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
// 补丁 R1：加入时就从文件名推导 season/episode（识别不到就留空）。
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
	inputs := make([]storage.SeriesMediaInput, 0, len(req.MediaIDs))
	for _, mediaID := range req.MediaIDs {
		m, err := s.DB.GetMediaIn(scopeAll(), mediaID)
		if err != nil {
			s.fail(w, r, domain.New("VALIDATION_NOT_FOUND", "媒体不存在: "+mediaID, 404))
			return
		}
		inputs = append(inputs, seriesMediaFromMedia(*m))
	}
	addedIDs, err := s.DB.AddSeriesMedia(id, inputs)
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
	detected := 0
	for _, input := range inputs {
		if input.Episode == nil {
			continue
		}
		for _, mediaID := range addedIDs {
			if mediaID == input.MediaID {
				detected++
				break
			}
		}
	}
	s.audit(r, "series.media.add", "series:"+id, true, "")
	respond(w, http.StatusOK, map[string]any{
		"added": len(addedIDs), "requested": len(req.MediaIDs),
		"detected": detected, "episode_count": fresh.EpisodeCount}, nil)
}

// seriesMediaFromMedia derives the nullable numbers for one media row.
func seriesMediaFromMedia(m domain.Media) storage.SeriesMediaInput {
	input := storage.SeriesMediaInput{MediaID: m.ID}
	season, episode, ok := detect.EpisodesFor(m)
	if !ok {
		return input
	}
	input.Season, input.Episode = season, episode
	input.Source = domain.EpisodeSourceFilename
	return input
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

// seriesDetectReq asks for the change list; nothing is written until confirm=true.
type seriesDetectReq struct {
	Confirm bool `json:"confirm"`
}

type episodeChangeJSON struct {
	MediaID    string `json:"media_id"`
	Title      string `json:"title"`
	Filename   string `json:"filename"`
	OldSeason  *int   `json:"old_season"`
	OldEpisode *int   `json:"old_episode"`
	OldLabel   string `json:"old_label"`
	NewSeason  *int   `json:"new_season"`
	NewEpisode *int   `json:"new_episode"`
	NewLabel   string `json:"new_label"`
}

// HandleDetectSeries re-derives season/episode from filenames (补丁 R1).
// Without confirm it only returns the change list; manual rows are never touched.
// 识别逻辑只在 internal/detect，与批量/后台共用同一份（A.2）。
func (s *Server) HandleDetectSeries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.DB.GetSeries(id); err != nil {
		s.fail(w, r, err)
		return
	}
	var req seriesDetectReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := detect.Series(r.Context(), s.DB, scopeAll(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	changes := make([]episodeChangeJSON, 0, len(res.Changes))
	for _, c := range res.Changes {
		changes = append(changes, changeJSON(c))
	}
	applied := 0
	if req.Confirm && len(res.Assigns) > 0 {
		applied, err = detect.Apply(r.Context(), s.DB, id, res.Assigns)
		if err != nil {
			s.audit(r, "series.detect", "series:"+id, false, errCode(err))
			s.fail(w, r, err)
			return
		}
		s.audit(r, "series.detect", "series:"+id, true, "")
	}
	respond(w, http.StatusOK, map[string]any{
		"applied": req.Confirm, "changed": len(changes), "updated": applied,
		"manual_skipped": res.ManualSkipped, "total": res.Total, "changes": changes}, nil)
}

// changeJSON is the one wire shape for a detect change.
func changeJSON(c detect.Change) episodeChangeJSON {
	return episodeChangeJSON{
		MediaID: c.MediaID, Title: c.Title, Filename: c.Filename,
		OldSeason: c.OldSeason, OldEpisode: c.OldEpisode, OldLabel: c.OldLabel,
		NewSeason: c.NewSeason, NewEpisode: c.NewEpisode, NewLabel: c.NewLabel,
	}
}

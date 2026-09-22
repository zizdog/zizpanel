package storage

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
)

// coverExpr 解析有效封面（显式 cover_media_id 或第一集），coverLibExpr 再取它的库
// 以便按 scope 判封面归属（S4：封面可能属于别的库）。
const coverExpr = `COALESCE(NULLIF(s.cover_media_id,''), (
		SELECT sm.media_id FROM series_media sm JOIN media m2 ON m2.id = sm.media_id
		WHERE sm.series_id = s.id AND m2.deleted_at IS NULL
		ORDER BY sm.position ASC LIMIT 1
	), '')`

const coverLibExpr = `COALESCE((SELECT mc.library_id FROM media mc WHERE mc.id = ` +
	coverExpr + ` AND mc.deleted_at IS NULL), '')`

// seriesCols 里的封面回落到第一集；剧场只引用 media，永远不碰磁盘文件。
const seriesCols = `s.id, s.title, s.description, ` + coverExpr + `, ` + coverLibExpr + `,
	COALESCE(s.library_id,''), COALESCE(s.dir_path,''), s.sort_order, s.created_at, s.updated_at,
	(SELECT COUNT(1) FROM series_media sm JOIN media m3 ON m3.id = sm.media_id
		WHERE sm.series_id = s.id AND m3.deleted_at IS NULL)`

func scanSeries(s interface{ Scan(...any) error }) (*domain.Series, error) {
	var out domain.Series
	if err := s.Scan(&out.ID, &out.Title, &out.Description, &out.CoverMediaID,
		&out.CoverLibraryID, &out.LibraryID, &out.DirPath, &out.SortOrder,
		&out.CreatedAt, &out.UpdatedAt, &out.EpisodeCount); err != nil {
		return nil, err
	}
	return &out, nil
}

// SeriesRef is one (series, title) membership of a media row.
type SeriesRef struct {
	ID    string
	Title string
}

// SeriesRefsByMedia maps media_id -> the live 剧场 that contain it（分块查询）。
// 导入预览用它回答"已在剧场?"与"已是别的剧场的集?"。
func (db *DB) SeriesRefsByMedia(mediaIDs []string) (map[string][]SeriesRef, error) {
	out := map[string][]SeriesRef{}
	for start := 0; start < len(mediaIDs); start += batchSize {
		end := start + batchSize
		if end > len(mediaIDs) {
			end = len(mediaIDs)
		}
		chunk := mediaIDs[start:end]
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := db.Query(`SELECT sm.media_id, s.id, s.title FROM series_media sm
			JOIN series s ON s.id = sm.series_id AND s.deleted_at IS NULL
			WHERE sm.media_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var mediaID string
			var ref SeriesRef
			if err := rows.Scan(&mediaID, &ref.ID, &ref.Title); err != nil {
				rows.Close()
				return nil, err
			}
			out[mediaID] = append(out[mediaID], ref)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SeriesByTitle returns the oldest live 剧场 with an exact title, or nil.
func (db *DB) SeriesByTitle(title string) (*domain.Series, error) {
	row := db.QueryRow(`SELECT `+seriesCols+` FROM series s
		WHERE s.deleted_at IS NULL AND s.title = ? ORDER BY s.created_at ASC, s.id ASC LIMIT 1`, title)
	out, err := scanSeries(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return out, err
}

// CreateSeries inserts a 剧场 row.
func (db *DB) CreateSeries(s *domain.Series) error {
	now := domain.NowString()
	s.CreatedAt, s.UpdatedAt = now, now
	_, err := db.Exec(`INSERT INTO series
		(id, title, description, cover_media_id, library_id, dir_path, sort_order, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Title, s.Description, nullStr(s.CoverMediaID), nullStr(s.LibraryID),
		s.DirPath, s.SortOrder, now, now)
	return err
}

// SetSeriesDir persists the upload landing directory once and for all.
func (db *DB) SetSeriesDir(id, dir string) error {
	_, err := db.Exec(`UPDATE series SET dir_path = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		dir, domain.NowString(), id)
	return err
}

// GetSeries loads one live 剧场.
func (db *DB) GetSeries(id string) (*domain.Series, error) {
	row := db.QueryRow(`SELECT `+seriesCols+` FROM series s WHERE s.id = ? AND s.deleted_at IS NULL`, id)
	out, err := scanSeries(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return out, err
}

// ListSeries returns visible 剧场 ordered by sort_order then creation time.
// 无可见集且无可见封面 ⇒ 从列表消失；episode_count 只数可见集（B.3 / B.4#6）。
func (db *DB) ListSeries(scope domain.LibraryScope) ([]domain.Series, error) {
	rows, err := db.Query(`SELECT ` + seriesCols + ` FROM series s
		WHERE s.deleted_at IS NULL ORDER BY s.sort_order ASC, s.created_at ASC, s.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []domain.Series{}
	for rows.Next() {
		s, err := scanSeries(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 管理端全见：不过滤空剧场，否则新建后还没加集的剧场会从列表消失、无法管理。
	// （偏离 ITERATION-2 B.3 的字面写法，安全意图不变：scope.All 不存在越权泄露。）
	if scope.All {
		return all, nil
	}
	counts, err := db.seriesVisibleCounts(scope)
	if err != nil {
		return nil, err
	}
	out := []domain.Series{}
	for _, s := range all {
		visible := counts[s.ID]
		coverVisible := s.CoverMediaID != "" && scope.Allows(s.CoverLibraryID)
		if visible == 0 && !coverVisible {
			continue
		}
		s.EpisodeCount = visible
		out = append(out, s)
	}
	return out, nil
}

// seriesVisibleCounts counts the in-scope live members of every 剧场.
func (db *DB) seriesVisibleCounts(scope domain.LibraryScope) (map[string]int, error) {
	w, args := scopeWhere(scope, "m.library_id")
	rows, err := db.Query(`SELECT sm.series_id, COUNT(1) FROM series_media sm
		JOIN media m ON m.id = sm.media_id
		WHERE m.deleted_at IS NULL`+w+` GROUP BY sm.series_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// SeriesPatch is a partial 剧场 update.
type SeriesPatch struct {
	Title        *string
	Description  *string
	CoverMediaID *string
	LibraryID    *string
	SortOrder    *int
}

// UpdateSeries applies a partial update and returns the fresh row.
func (db *DB) UpdateSeries(id string, p SeriesPatch) (*domain.Series, error) {
	sets := []string{}
	args := []any{}
	if p.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *p.Title)
	}
	if p.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *p.Description)
	}
	if p.CoverMediaID != nil {
		sets = append(sets, "cover_media_id = ?")
		args = append(args, nullStr(*p.CoverMediaID))
	}
	if p.LibraryID != nil {
		sets = append(sets, "library_id = ?")
		args = append(args, nullStr(*p.LibraryID))
	}
	if p.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *p.SortOrder)
	}
	if len(sets) == 0 {
		return db.GetSeries(id)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, domain.NowString(), id)
	res, err := db.Exec(`UPDATE series SET `+joinComma(sets)+
		` WHERE id = ? AND deleted_at IS NULL`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, domain.ErrNotFound
	}
	return db.GetSeries(id)
}

// DeleteSeries soft-deletes the 剧场 and drops its episode links. Media rows and
// files are never touched.
func (db *DB) DeleteSeries(id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM series_media WHERE series_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.Exec(`UPDATE series SET deleted_at = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`, domain.NowString(), domain.NowString(), id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return domain.ErrNotFound
	}
	return tx.Commit()
}

// ListSeriesEpisodes returns the in-scope episode links plus their media rows, in
// stored position order. The playback/display order (season→episode→文件名自然序)
// is applied by the API layer, which may import the filename parser.
func (db *DB) ListSeriesEpisodes(scope domain.LibraryScope, seriesID string) ([]domain.SeriesEpisode, []domain.Media, error) {
	w, sargs := scopeWhere(scope, "m.library_id")
	args := append([]any{seriesID}, sargs...)
	rows, err := db.Query(`SELECT sm.series_id, sm.media_id, sm.position, sm.created_at,
		sm.season, sm.episode, COALESCE(sm.episode_source,''), `+mediaColsQ+`
		FROM series_media sm JOIN media m ON m.id = sm.media_id
		WHERE sm.series_id = ? AND m.deleted_at IS NULL`+w+`
		ORDER BY sm.position ASC`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	eps := []domain.SeriesEpisode{}
	medias := []domain.Media{}
	for rows.Next() {
		var e domain.SeriesEpisode
		var m domain.Media
		var season, episode sql.NullInt64
		if err := rows.Scan(&e.SeriesID, &e.MediaID, &e.Position, &e.AddedAt,
			&season, &episode, &e.EpisodeSource,
			&m.ID, &m.LibraryID, &m.Path, &m.Title, &m.Size, &m.MtimeNS, &m.Container,
			&m.Codecs.Video, &m.Codecs.Audio, &m.Width, &m.Height, &m.DurationMS, &m.Bitrate,
			&m.FPS, &m.Status, &m.ErrorClass, &m.ErrorMessage, &m.MissingSince,
			&m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, nil, err
		}
		e.Season = nullInt(season)
		e.Episode = nullInt(episode)
		eps = append(eps, e)
		medias = append(medias, m)
	}
	return eps, medias, rows.Err()
}

func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func intArg(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// SeriesMediaInput is one media to append together with its derived numbers.
type SeriesMediaInput struct {
	MediaID string
	Season  *int
	Episode *int
	Source  string
}

// AddSeriesMedia appends media to the end of a 剧场, skipping duplicates.
// Returns the media ids that were really added so callers can report honestly.
func (db *DB) AddSeriesMedia(seriesID string, items []SeriesMediaInput) ([]string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	existing := map[string]bool{}
	position := 0
	rows, err := tx.Query(`SELECT media_id, position FROM series_media WHERE series_id = ?`, seriesID)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	for rows.Next() {
		var id string
		var pos int
		if err := rows.Scan(&id, &pos); err != nil {
			rows.Close()
			_ = tx.Rollback()
			return nil, err
		}
		existing[id] = true
		if pos > position {
			position = pos
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	now := domain.NowString()
	added := []string{}
	for _, item := range items {
		if item.MediaID == "" || existing[item.MediaID] {
			continue
		}
		position++
		if _, err := tx.Exec(`INSERT INTO series_media
			(series_id, media_id, position, created_at, season, episode, episode_source)
			VALUES (?,?,?,?,?,?,?)`,
			seriesID, item.MediaID, position, now,
			intArg(item.Season), intArg(item.Episode), nullStr(item.Source)); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		existing[item.MediaID] = true
		added = append(added, item.MediaID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return added, nil
}

// EpisodeAssignment is one derived (season, episode) write for a member.
type EpisodeAssignment struct {
	MediaID string
	Season  *int
	Episode *int
	Source  string
}

// ApplySeriesEpisodes writes derived numbers, never touching manual rows (R1:
// 自动识别不得覆盖 manual). Returns how many rows were really updated.
func (db *DB) ApplySeriesEpisodes(seriesID string, assigns []EpisodeAssignment) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, a := range assigns {
		res, err := tx.Exec(`UPDATE series_media SET season = ?, episode = ?, episode_source = ?
			WHERE series_id = ? AND media_id = ?
			  AND COALESCE(episode_source,'') <> ?`,
			intArg(a.Season), intArg(a.Episode), a.Source, seriesID, a.MediaID,
			domain.EpisodeSourceManual)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

// RemoveSeriesMedia drops one episode and keeps positions contiguous.
func (db *DB) RemoveSeriesMedia(seriesID, mediaID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM series_media WHERE series_id = ? AND media_id = ?`, seriesID, mediaID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return domain.ErrNotFound
	}
	if err := renumberSeries(tx, seriesID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ReorderSeries rewrites the episode order and marks every listed row as manual
// so a later 自动识别 never overwrites the admin's choice (补丁 R1). The id list
// must match the current membership exactly, otherwise a stale UI would
// silently drop episodes.
func (db *DB) ReorderSeries(seriesID string, mediaIDs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	current := map[string]bool{}
	rows, err := tx.Query(`SELECT media_id FROM series_media WHERE series_id = ?`, seriesID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			_ = tx.Rollback()
			return err
		}
		current[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if len(mediaIDs) != len(current) {
		_ = tx.Rollback()
		return domain.ErrSeriesOrder
	}
	for _, id := range mediaIDs {
		if !current[id] {
			_ = tx.Rollback()
			return domain.ErrSeriesOrder
		}
	}
	// 唯一索引下直接改 position 会互相撞车，先整体挪到负数再写目标值。
	if _, err := tx.Exec(`UPDATE series_media SET position = -position WHERE series_id = ?`, seriesID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for i, id := range mediaIDs {
		if _, err := tx.Exec(`UPDATE series_media SET position = ?, episode_source = ?
			WHERE series_id = ? AND media_id = ?`,
			i+1, domain.EpisodeSourceManual, seriesID, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// renumberSeries compacts positions to 1..N after a removal.
func renumberSeries(tx *sql.Tx, seriesID string) error {
	if _, err := tx.Exec(`UPDATE series_media SET position = -position WHERE series_id = ?`, seriesID); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT media_id FROM series_media WHERE series_id = ? ORDER BY position DESC`, seriesID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE series_media SET position = ? WHERE series_id = ? AND media_id = ?`,
			i+1, seriesID, id); err != nil {
			return err
		}
	}
	return nil
}

// SeriesCount returns how many live 剧场 exist (used by tests and system info).
func (db *DB) SeriesCount() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(1) FROM series WHERE deleted_at IS NULL`).Scan(&n)
	return n, err
}

// SeriesIDsInLibrary returns the live 剧场 having at least one live member media
// in the library. 内部/管理端用，不是用户读路径（不带 scope）。
func (db *DB) SeriesIDsInLibrary(libraryID string) ([]string, error) {
	return db.seriesIDsInLibrary(libraryID, false, "")
}

// SeriesIDsWithNewMediaSince returns 剧场 with a live member in the library AND
// at least one media row in that library created at/after since —— "含新增媒体的
// 剧场"。扫描成功后的自动识别只针对它们（A.4），避免每次重扫全量跑。
func (db *DB) SeriesIDsWithNewMediaSince(libraryID, since string) ([]string, error) {
	return db.seriesIDsInLibrary(libraryID, true, since)
}

func (db *DB) seriesIDsInLibrary(libraryID string, requireNew bool, since string) ([]string, error) {
	query := `SELECT DISTINCT sm.series_id FROM series_media sm
		JOIN media m ON m.id = sm.media_id AND m.deleted_at IS NULL
		JOIN series s ON s.id = sm.series_id AND s.deleted_at IS NULL
		WHERE m.library_id = ?`
	args := []any{libraryID}
	if requireNew {
		query += ` AND EXISTS (SELECT 1 FROM media nm WHERE nm.library_id = ?
			AND nm.deleted_at IS NULL AND nm.created_at >= ?)`
		args = append(args, libraryID, since)
	}
	query += ` ORDER BY sm.series_id ASC`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

package storage

import (
	"database/sql"
	"errors"

	"github.com/zizdog/zizvideo/internal/domain"
)

// seriesCols 里的封面回落到第一集；剧场只引用 media，永远不碰磁盘文件。
const seriesCols = `s.id, s.title, s.description,
	COALESCE(NULLIF(s.cover_media_id,''), (
		SELECT sm.media_id FROM series_media sm JOIN media m2 ON m2.id = sm.media_id
		WHERE sm.series_id = s.id AND m2.deleted_at IS NULL
		ORDER BY sm.position ASC LIMIT 1
	), ''),
	COALESCE(s.library_id,''), s.sort_order, s.created_at, s.updated_at,
	(SELECT COUNT(1) FROM series_media sm JOIN media m3 ON m3.id = sm.media_id
		WHERE sm.series_id = s.id AND m3.deleted_at IS NULL)`

func scanSeries(s interface{ Scan(...any) error }) (*domain.Series, error) {
	var out domain.Series
	if err := s.Scan(&out.ID, &out.Title, &out.Description, &out.CoverMediaID,
		&out.LibraryID, &out.SortOrder, &out.CreatedAt, &out.UpdatedAt, &out.EpisodeCount); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSeries inserts a 剧场 row.
func (db *DB) CreateSeries(s *domain.Series) error {
	now := domain.NowString()
	s.CreatedAt, s.UpdatedAt = now, now
	_, err := db.Exec(`INSERT INTO series
		(id, title, description, cover_media_id, library_id, sort_order, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		s.ID, s.Title, s.Description, nullStr(s.CoverMediaID), nullStr(s.LibraryID),
		s.SortOrder, now, now)
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

// ListSeries returns all live 剧场 ordered by sort_order then creation time.
func (db *DB) ListSeries() ([]domain.Series, error) {
	rows, err := db.Query(`SELECT ` + seriesCols + ` FROM series s
		WHERE s.deleted_at IS NULL ORDER BY s.sort_order ASC, s.created_at ASC, s.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Series{}
	for rows.Next() {
		s, err := scanSeries(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
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

// ListSeriesEpisodes returns the ordered episode links plus their media rows.
func (db *DB) ListSeriesEpisodes(seriesID string) ([]domain.SeriesEpisode, []domain.Media, error) {
	rows, err := db.Query(`SELECT sm.series_id, sm.media_id, sm.position, sm.created_at, `+mediaColsQ+`
		FROM series_media sm JOIN media m ON m.id = sm.media_id
		WHERE sm.series_id = ? AND m.deleted_at IS NULL
		ORDER BY sm.position ASC`, seriesID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	eps := []domain.SeriesEpisode{}
	medias := []domain.Media{}
	for rows.Next() {
		var e domain.SeriesEpisode
		var m domain.Media
		if err := rows.Scan(&e.SeriesID, &e.MediaID, &e.Position, &e.AddedAt,
			&m.ID, &m.LibraryID, &m.Path, &m.Title, &m.Size, &m.MtimeNS, &m.Container,
			&m.Codecs.Video, &m.Codecs.Audio, &m.Width, &m.Height, &m.DurationMS, &m.Bitrate,
			&m.FPS, &m.Status, &m.ErrorClass, &m.ErrorMessage, &m.MissingSince,
			&m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, nil, err
		}
		e.Episode = e.Position
		eps = append(eps, e)
		medias = append(medias, m)
	}
	return eps, medias, rows.Err()
}

// AddSeriesMedia appends media to the end of a 剧场, skipping duplicates.
// Returns how many rows were really added so callers can report it honestly.
func (db *DB) AddSeriesMedia(seriesID string, mediaIDs []string) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	existing := map[string]bool{}
	position := 0
	rows, err := tx.Query(`SELECT media_id, position FROM series_media WHERE series_id = ?`, seriesID)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	for rows.Next() {
		var id string
		var pos int
		if err := rows.Scan(&id, &pos); err != nil {
			rows.Close()
			_ = tx.Rollback()
			return 0, err
		}
		existing[id] = true
		if pos > position {
			position = pos
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	now := domain.NowString()
	added := 0
	for _, id := range mediaIDs {
		if id == "" || existing[id] {
			continue
		}
		position++
		if _, err := tx.Exec(`INSERT INTO series_media (series_id, media_id, position, created_at)
			VALUES (?,?,?,?)`, seriesID, id, position, now); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		existing[id] = true
		added++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
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

// ReorderSeries rewrites the episode order; the id list must match the current
// membership exactly, otherwise a stale UI would silently drop episodes.
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
		if _, err := tx.Exec(`UPDATE series_media SET position = ? WHERE series_id = ? AND media_id = ?`,
			i+1, seriesID, id); err != nil {
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

package storage

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/zizdog/zizvideo/internal/domain"
)

const mediaCols = `id, library_id, path, title, size, mtime_ns, container,
	video_codec, audio_codec, width, height, duration_ms, bitrate, fps,
	status, error_class, error_message, COALESCE(missing_since,''), created_at, updated_at`

// mediaColsQ is mediaCols qualified for JOINs (created_at 会被别的表撞名).
const mediaColsQ = `m.id, m.library_id, m.path, m.title, m.size, m.mtime_ns, m.container,
	m.video_codec, m.audio_codec, m.width, m.height, m.duration_ms, m.bitrate, m.fps,
	m.status, m.error_class, m.error_message, COALESCE(m.missing_since,''), m.created_at, m.updated_at`

func scanMedia(s interface{ Scan(...any) error }) (*domain.Media, error) {
	var m domain.Media
	if err := s.Scan(&m.ID, &m.LibraryID, &m.Path, &m.Title, &m.Size, &m.MtimeNS, &m.Container,
		&m.Codecs.Video, &m.Codecs.Audio, &m.Width, &m.Height, &m.DurationMS, &m.Bitrate, &m.FPS,
		&m.Status, &m.ErrorClass, &m.ErrorMessage, &m.MissingSince, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	return &m, nil
}

// MediaByPath loads the live row for a library+normalized path, or nil.
func (db *DB) MediaByPath(libraryID, normalizedPath string) (*domain.Media, error) {
	row := db.QueryRow(`SELECT `+mediaCols+` FROM media
		WHERE library_id = ? AND normalized_path = ? AND deleted_at IS NULL`,
		libraryID, normalizedPath)
	m, err := scanMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// GetMedia loads one live media row.
func (db *DB) GetMedia(id string) (*domain.Media, error) {
	row := db.QueryRow(`SELECT `+mediaCols+` FROM media WHERE id = ? AND deleted_at IS NULL`, id)
	m, err := scanMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return m, err
}

// InsertMedia creates a new media row.
func (db *DB) InsertMedia(m *domain.Media) error {
	now := domain.NowString()
	m.CreatedAt, m.UpdatedAt = now, now
	_, err := db.Exec(`INSERT INTO media (id, library_id, path, normalized_path, title, size,
		mtime_ns, container, video_codec, audio_codec, width, height, duration_ms, bitrate, fps,
		status, error_class, error_message, probe_attempts, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.LibraryID, m.Path, m.Path, m.Title, m.Size, m.MtimeNS, m.Container,
		m.Codecs.Video, m.Codecs.Audio, m.Width, m.Height, m.DurationMS, m.Bitrate, m.FPS,
		m.Status, m.ErrorClass, m.ErrorMessage, 0, now, now)
	return err
}

// UpdateMediaProbe writes probed metadata and clears any stale failure state.
func (db *DB) UpdateMediaProbe(m *domain.Media) error {
	_, err := db.Exec(`UPDATE media SET path = ?, title = ?, size = ?, mtime_ns = ?,
		container = ?, video_codec = ?, audio_codec = ?, width = ?, height = ?, duration_ms = ?,
		bitrate = ?, fps = ?, status = ?, error_class = '', error_message = '',
		probe_attempts = 0, missing_since = NULL, updated_at = ?
		WHERE id = ?`,
		m.Path, m.Title, m.Size, m.MtimeNS, m.Container, m.Codecs.Video, m.Codecs.Audio,
		m.Width, m.Height, m.DurationMS, m.Bitrate, m.FPS, m.Status, domain.NowString(), m.ID)
	return err
}

// MarkProbeFailed records a classified probe failure.
func (db *DB) MarkProbeFailed(id, class, message string, attempts int) error {
	_, err := db.Exec(`UPDATE media SET status = ?, error_class = ?, error_message = ?,
		probe_attempts = ?, missing_since = NULL, updated_at = ? WHERE id = ?`,
		domain.MediaProbeFail, class, truncate(message, 300), attempts, domain.NowString(), id)
	return err
}

// MarkMissing flags a row as absent since the given time without deleting it.
func (db *DB) MarkMissing(id, since string) error {
	_, err := db.Exec(`UPDATE media SET missing_since = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`, since, domain.NowString(), id)
	return err
}

// ClearMissing removes a stale missing marker.
func (db *DB) ClearMissing(id string) error {
	_, err := db.Exec(`UPDATE media SET missing_since = NULL, updated_at = ?
		WHERE id = ? AND missing_since IS NOT NULL`, domain.NowString(), id)
	return err
}

// ApplyDeletions soft-deletes rows in one transaction.
func (db *DB) ApplyDeletions(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := domain.NowString()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE media SET deleted_at = ?, updated_at = ? WHERE id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(now, now, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// MediaFilter drives the paged list endpoint.
type MediaFilter struct {
	LibraryID string
	Query     string
	Status    string
	Page      int
	PerPage   int
	Sort      string
	Desc      bool
}

// ListMedia returns a page of media plus the unpaged total.
func (db *DB) ListMedia(f MediaFilter) ([]domain.Media, int, error) {
	where := `deleted_at IS NULL`
	args := []any{}
	if f.LibraryID != "" {
		where += ` AND library_id = ?`
		args = append(args, f.LibraryID)
	}
	if f.Query != "" {
		where += ` AND (title LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\')`
		like := "%" + escapeLike(f.Query) + "%"
		args = append(args, like, like)
	}
	if f.Status != "" {
		where += ` AND status = ?`
		args = append(args, f.Status)
	}
	var total int
	if err := db.QueryRow(`SELECT COUNT(1) FROM media WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	col := "id"
	switch f.Sort {
	case "title":
		col = "title"
	case "created_at":
		col = "created_at"
	case "size":
		col = "size"
	case "duration_ms":
		col = "duration_ms"
	case "id":
		col = "id"
	}
	dir := "DESC"
	if !f.Desc {
		dir = "ASC"
	}
	per := f.PerPage
	if per <= 0 {
		per = 20
	}
	off := (f.Page - 1) * per
	if off < 0 {
		off = 0
	}
	q := `SELECT ` + mediaCols + ` FROM media WHERE ` + where +
		` ORDER BY ` + col + ` ` + dir + `, id DESC LIMIT ? OFFSET ?`
	rows, err := db.Query(q, append(args, per, off)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.Media{}
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *m)
	}
	return out, total, rows.Err()
}

// FeedNext 已被 storage/feed.go 的 seed+hash 随机游标取代（默认随机播放）。

// MediaState is the cheap per-file fingerprint used to skip unchanged files.
type MediaState struct {
	ID      string
	Size    int64
	MtimeNS int64
	Status  string
}

// MediaStatesByLibrary maps normalized path -> fingerprint for one library.
func (db *DB) MediaStatesByLibrary(libraryID string) (map[string]MediaState, error) {
	rows, err := db.Query(`SELECT normalized_path, id, size, mtime_ns, status FROM media
		WHERE library_id = ? AND deleted_at IS NULL`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MediaState{}
	for rows.Next() {
		var p string
		var st MediaState
		if err := rows.Scan(&p, &st.ID, &st.Size, &st.MtimeNS, &st.Status); err != nil {
			return nil, err
		}
		out[p] = st
	}
	return out, rows.Err()
}

// MediaIDsByLibrary lists live media ids and normalized paths for reconciliation.
func (db *DB) MediaIDsByLibrary(libraryID string) (map[string]string, map[string]string, error) {
	rows, err := db.Query(`SELECT id, normalized_path, COALESCE(missing_since,'') FROM media
		WHERE library_id = ? AND deleted_at IS NULL`, libraryID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byPath := map[string]string{}
	missing := map[string]string{}
	for rows.Next() {
		var id, p, since string
		if err := rows.Scan(&id, &p, &since); err != nil {
			return nil, nil, err
		}
		byPath[p] = id
		missing[id] = since
	}
	return byPath, missing, rows.Err()
}

// CountMedia returns live media count overall or per library.
func (db *DB) CountMedia(libraryID string) (int, error) {
	q := `SELECT COUNT(1) FROM media WHERE deleted_at IS NULL`
	args := []any{}
	if libraryID != "" {
		q += ` AND library_id = ?`
		args = append(args, libraryID)
	}
	var n int
	err := db.QueryRow(q, args...).Scan(&n)
	return n, err
}

// CountFailedMedia returns probe_failed rows overall or per library.
func (db *DB) CountFailedMedia(libraryID string) (int, error) {
	q := `SELECT COUNT(1) FROM media WHERE deleted_at IS NULL AND status = ?`
	args := []any{domain.MediaProbeFail}
	if libraryID != "" {
		q += ` AND library_id = ?`
		args = append(args, libraryID)
	}
	var n int
	err := db.QueryRow(q, args...).Scan(&n)
	return n, err
}

func escapeLike(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '%' || r == '_' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var _ = fmt.Sprintf

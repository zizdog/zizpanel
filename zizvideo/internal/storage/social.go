package storage

import (
	"database/sql"
	"errors"

	"github.com/zizdog/govideo/internal/domain"
)

// UpsertProgress stores a playback position (frequent, small write).
func (db *DB) UpsertProgress(p *domain.Progress) error {
	_, err := db.Exec(`INSERT INTO watch_progress
		(user_id, media_id, position_ms, duration_ms, completed, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(user_id, media_id) DO UPDATE SET
			position_ms = excluded.position_ms,
			duration_ms = excluded.duration_ms,
			completed = excluded.completed,
			updated_at = excluded.updated_at`,
		p.UserID, p.MediaID, p.PositionMS, p.DurationMS, boolToInt(p.Completed), domain.NowString())
	return err
}

// GetProgress returns one user's progress for one media item, or nil.
func (db *DB) GetProgress(userID, mediaID string) (*domain.Progress, error) {
	p := &domain.Progress{UserID: userID, MediaID: mediaID}
	var completed int
	err := db.QueryRow(`SELECT position_ms, duration_ms, completed, updated_at
		FROM watch_progress WHERE user_id = ? AND media_id = ?`, userID, mediaID).
		Scan(&p.PositionMS, &p.DurationMS, &completed, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Completed = completed != 0
	return p, nil
}

// ProgressMap returns every progress row of a user keyed by media id.
func (db *DB) ProgressMap(userID string) (map[string]domain.Progress, error) {
	rows, err := db.Query(`SELECT media_id, position_ms, duration_ms, completed, updated_at
		FROM watch_progress WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]domain.Progress{}
	for rows.Next() {
		var p domain.Progress
		var completed int
		if err := rows.Scan(&p.MediaID, &p.PositionMS, &p.DurationMS, &completed, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.UserID = userID
		p.Completed = completed != 0
		out[p.MediaID] = p
	}
	return out, rows.Err()
}

// ListProgress returns recent progress rows with their media attached.
func (db *DB) ListProgress(userID string, limit int) ([]domain.Progress, []domain.Media, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.Query(`SELECT p.media_id, p.position_ms, p.duration_ms, p.completed, p.updated_at,
			m.id, m.library_id, m.path, m.title, m.size, m.mtime_ns, m.container,
			m.video_codec, m.audio_codec, m.width, m.height, m.duration_ms, m.bitrate, m.fps,
			m.status, m.error_class, m.error_message, COALESCE(m.missing_since,''),
			m.created_at, m.updated_at
		FROM watch_progress p JOIN media m ON m.id = p.media_id
		WHERE p.user_id = ? AND m.deleted_at IS NULL
		ORDER BY p.updated_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	progs := []domain.Progress{}
	medias := []domain.Media{}
	for rows.Next() {
		var p domain.Progress
		var m domain.Media
		var completed int
		if err := rows.Scan(&p.MediaID, &p.PositionMS, &p.DurationMS, &completed, &p.UpdatedAt,
			&m.ID, &m.LibraryID, &m.Path, &m.Title, &m.Size, &m.MtimeNS, &m.Container,
			&m.Codecs.Video, &m.Codecs.Audio, &m.Width, &m.Height, &m.DurationMS, &m.Bitrate,
			&m.FPS, &m.Status, &m.ErrorClass, &m.ErrorMessage, &m.MissingSince,
			&m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, nil, err
		}
		p.UserID = userID
		p.Completed = completed != 0
		progs = append(progs, p)
		medias = append(medias, m)
	}
	return progs, medias, rows.Err()
}

// AddFavorite idempotently marks a media item as favorite.
func (db *DB) AddFavorite(userID, mediaID string) error {
	_, err := db.Exec(`INSERT INTO favorites (user_id, media_id, created_at)
		VALUES (?,?,?) ON CONFLICT(user_id, media_id) DO NOTHING`,
		userID, mediaID, domain.NowString())
	return err
}

// RemoveFavorite clears the favorite mark.
func (db *DB) RemoveFavorite(userID, mediaID string) error {
	_, err := db.Exec(`DELETE FROM favorites WHERE user_id = ? AND media_id = ?`, userID, mediaID)
	return err
}

// Favorites returns the set of favorite media ids for a user.
func (db *DB) Favorites(userID string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT media_id FROM favorites WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetReaction stores like/dislike for a media item.
func (db *DB) SetReaction(userID, mediaID, kind string) error {
	_, err := db.Exec(`INSERT INTO reactions (user_id, media_id, kind, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(user_id, media_id) DO UPDATE SET kind = excluded.kind,
			updated_at = excluded.updated_at`,
		userID, mediaID, kind, domain.NowString())
	return err
}

// ClearReaction removes the reaction row.
func (db *DB) ClearReaction(userID, mediaID string) error {
	_, err := db.Exec(`DELETE FROM reactions WHERE user_id = ? AND media_id = ?`, userID, mediaID)
	return err
}

// Reactions returns mediaID -> kind for a user.
func (db *DB) Reactions(userID string) (map[string]string, error) {
	rows, err := db.Query(`SELECT media_id, kind FROM reactions WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

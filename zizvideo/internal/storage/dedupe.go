package storage

import (
	"errors"

	"github.com/zizdog/zizvideo/internal/domain"
)

// DuplicateMember is one media row inside a suspected-duplicate group.
type DuplicateMember struct {
	ID           string `json:"id"`
	LibraryID    string `json:"library_id"`
	Path         string `json:"path"`
	Title        string `json:"title"`
	DurationMS   int64  `json:"duration_ms"`
	Size         int64  `json:"size_bytes"`
	Status       string `json:"status"`
	MissingSince string `json:"missing_since,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// DuplicateGroup shares one (size_bytes, duration_ms) key. It is a suspicion
// only: identical size and duration does not prove identical content.
type DuplicateGroup struct {
	Size       int64             `json:"size_bytes"`
	DurationMS int64             `json:"duration_ms"`
	Members    []DuplicateMember `json:"members"`
}

// DuplicateGroups groups live, ready media by (size_bytes, duration_ms) and
// returns only groups with more than one member. limit caps the group count.
func (db *DB) DuplicateGroups(limit int) ([]DuplicateGroup, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(`SELECT size, duration_ms, COUNT(1) AS n FROM media
		WHERE deleted_at IS NULL AND status = ? AND duration_ms > 0 AND size > 0
		GROUP BY size, duration_ms HAVING n > 1
		ORDER BY n DESC, size DESC LIMIT ?`, domain.MediaReady, limit)
	if err != nil {
		return nil, err
	}
	keys := []DuplicateGroup{}
	for rows.Next() {
		var g DuplicateGroup
		var n int
		if err := rows.Scan(&g.Size, &g.DurationMS, &n); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]DuplicateGroup, 0, len(keys))
	for _, g := range keys {
		members, err := db.duplicateMembers(g.Size, g.DurationMS)
		if err != nil {
			return nil, err
		}
		g.Members = members
		out = append(out, g)
	}
	return out, nil
}

func (db *DB) duplicateMembers(size, durationMS int64) ([]DuplicateMember, error) {
	rows, err := db.Query(`SELECT id, library_id, path, title, duration_ms, size, status,
		COALESCE(missing_since,''), created_at FROM media
		WHERE deleted_at IS NULL AND status = ? AND size = ? AND duration_ms = ?
		ORDER BY created_at ASC, id ASC`, domain.MediaReady, size, durationMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DuplicateMember{}
	for rows.Next() {
		var m DuplicateMember
		if err := rows.Scan(&m.ID, &m.LibraryID, &m.Path, &m.Title, &m.DurationMS, &m.Size,
			&m.Status, &m.MissingSince, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MediaByIDs loads live rows for the given ids and reports the ids that are
// gone, so a delete request never silently skips an unknown object.
func (db *DB) MediaByIDs(ids []string) ([]domain.Media, []string, error) {
	found := []domain.Media{}
	missing := []string{}
	for _, id := range ids {
		m, err := db.getMedia(id)
		if errors.Is(err, domain.ErrNotFound) {
			missing = append(missing, id)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		found = append(found, *m)
	}
	return found, missing, nil
}

// SoftDeleteMedia soft-deletes live rows and returns how many were marked. It
// only writes the database; files on disk are never touched here.
func (db *DB) SoftDeleteMedia(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	live := 0
	for _, id := range ids {
		var n int
		if err := db.QueryRow(`SELECT COUNT(1) FROM media WHERE id = ? AND deleted_at IS NULL`,
			id).Scan(&n); err != nil {
			return 0, err
		}
		live += n
	}
	if err := db.ApplyDeletions(ids); err != nil {
		return 0, err
	}
	return live, nil
}

package storage

import (
	"database/sql"
	"errors"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
)

// FeedScopeAll is the scope key for "every library".
const FeedScopeAll = ""

// FeedState is one user's position inside one random cycle of one feed scope.
type FeedState struct {
	UserID     string
	Scope      string
	Seed       string
	CursorHash int64
	CursorID   string
	Played     int
}

// FeedOrderKey is the deterministic pseudo-random sort key hash(id||seed).
// 63-bit so it round-trips through SQLite INTEGER without sign surprises.
func FeedOrderKey(id, seed string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(seed))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// GetFeedState loads the state row for one user+scope, creating it with a fresh
// seed on first use.
func (db *DB) GetFeedState(userID, scope string) (*FeedState, error) {
	st := &FeedState{UserID: userID, Scope: scope}
	err := db.QueryRow(`SELECT seed, cursor_hash, cursor_id, played FROM feed_state
		WHERE user_id = ? AND scope = ?`, userID, scope).
		Scan(&st.Seed, &st.CursorHash, &st.CursorID, &st.Played)
	if errors.Is(err, sql.ErrNoRows) {
		st.Seed = domain.NewID("s")
		if err := db.SaveFeedState(st); err != nil {
			return nil, err
		}
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	return st, nil
}

// SaveFeedState upserts one cycle cursor.
func (db *DB) SaveFeedState(st *FeedState) error {
	_, err := db.Exec(`INSERT INTO feed_state
		(user_id, scope, seed, cursor_hash, cursor_id, played, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(user_id, scope) DO UPDATE SET
			seed = excluded.seed, cursor_hash = excluded.cursor_hash,
			cursor_id = excluded.cursor_id, played = excluded.played,
			updated_at = excluded.updated_at`,
		st.UserID, st.Scope, st.Seed, st.CursorHash, st.CursorID, st.Played, domain.NowString())
	return err
}

// DefaultSeekSeconds 是左右键跳转的默认秒数；存量 <=0 一律按默认处理。
const DefaultSeekSeconds = 10

// UserPrefs is the per-user player settings block.
type UserPrefs struct {
	LoopPlay     bool
	AutoplayNext bool
	SeekSeconds  int
}

// GetUserPrefs returns the player settings; autoplay defaults on, loop off.
func (db *DB) GetUserPrefs(userID string) (*UserPrefs, error) {
	p := &UserPrefs{AutoplayNext: true, SeekSeconds: DefaultSeekSeconds}
	var loop, auto int
	err := db.QueryRow(`SELECT loop_play, autoplay_next, seek_seconds FROM user_prefs WHERE user_id = ?`, userID).
		Scan(&loop, &auto, &p.SeekSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	p.LoopPlay, p.AutoplayNext = loop != 0, auto != 0
	if p.SeekSeconds <= 0 {
		p.SeekSeconds = DefaultSeekSeconds
	}
	return p, nil
}

// SaveUserPrefs upserts the player settings block.
func (db *DB) SaveUserPrefs(userID string, p *UserPrefs) error {
	seek := p.SeekSeconds
	if seek <= 0 {
		seek = DefaultSeekSeconds
	}
	_, err := db.Exec(`INSERT INTO user_prefs (user_id, loop_play, autoplay_next, seek_seconds, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET
			loop_play = excluded.loop_play, autoplay_next = excluded.autoplay_next,
			seek_seconds = excluded.seek_seconds, updated_at = excluded.updated_at`,
		userID, boolToInt(p.LoopPlay), boolToInt(p.AutoplayNext), seek, domain.NowString())
	return err
}

// CountPlayable counts the rows a feed scope can ever serve.
func (db *DB) CountPlayable(scope domain.LibraryScope) (int, error) {
	q := `SELECT COUNT(1) FROM media WHERE deleted_at IS NULL AND status = ?`
	args := []any{domain.MediaReady}
	if w, sargs := scopeWhere(scope, "library_id"); w != "" {
		q += w
		args = append(args, sargs...)
	}
	var n int
	err := db.QueryRow(q, args...).Scan(&n)
	return n, err
}

// FeedPage returns the next `limit` playable rows of one seeded random cycle,
// strictly after the (afterHash, afterID) cursor.
//
// Ordering is stable for a given (scope, seed) and visits every playable row
// exactly once per cycle, so a client that follows next_cursor never sees a
// repeat before the cycle is exhausted。排序在 Go 里做：媒体量级是个人库，
// 每次翻页只读 id 列，换来的是与 SQLite 无关的确定顺序。
func (db *DB) FeedPage(scope domain.LibraryScope, seed string, afterHash int64, afterID string, limit int) ([]domain.Media, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	ids, err := db.feedScopeIDs(scope)
	if err != nil {
		return nil, err
	}
	type keyed struct {
		id  string
		key int64
	}
	list := make([]keyed, 0, len(ids))
	for _, id := range ids {
		list = append(list, keyed{id: id, key: FeedOrderKey(id, seed)})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].key != list[j].key {
			return list[i].key < list[j].key
		}
		return list[i].id < list[j].id
	})
	start := sort.Search(len(list), func(i int) bool {
		if list[i].key != afterHash {
			return list[i].key > afterHash
		}
		return list[i].id > afterID
	})
	end := start + limit
	if end > len(list) {
		end = len(list)
	}
	picked := make([]string, 0, end-start)
	for _, k := range list[start:end] {
		picked = append(picked, k.id)
	}
	if len(picked) == 0 {
		return []domain.Media{}, nil
	}
	return db.mediaByIDs(picked)
}

// feedScopeIDs lists the playable ids of one scope, unordered.
func (db *DB) feedScopeIDs(scope domain.LibraryScope) ([]string, error) {
	q := `SELECT id FROM media WHERE deleted_at IS NULL AND status = ?`
	args := []any{domain.MediaReady}
	if w, sargs := scopeWhere(scope, "library_id"); w != "" {
		q += w
		args = append(args, sargs...)
	}
	rows, err := db.Query(q, args...)
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

// mediaByIDs loads rows and returns them in the requested order.
func (db *DB) mediaByIDs(ids []string) ([]domain.Media, error) {
	if len(ids) == 0 {
		return []domain.Media{}, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `SELECT ` + mediaCols + ` FROM media WHERE id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]domain.Media{}
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		byID[m.ID] = *m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]domain.Media, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

package storage

import (
	"sort"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
)

// scopeWhere 生成 " AND <col> IN (?,...)"；All ⇒ 无子句，空集合 ⇒ AND 1=0
// （fail-closed，绝不回落成全部）。args 顺序与占位符在文本中的顺序一致（ITERATION-2 B.3）。
func scopeWhere(scope domain.LibraryScope, col string) (string, []any) {
	if scope.All {
		return "", nil
	}
	if len(scope.IDs) == 0 {
		return " AND 1=0", nil
	}
	ids := make([]string, 0, len(scope.IDs))
	for id := range scope.IDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return " AND " + col + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")", args
}

// UserLibraryIDs 是唯一的授权判据 SQL：只认授权行，软删库 JOIN 掉（B.1 幽灵授权）。
func (db *DB) UserLibraryIDs(userID string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT ul.library_id FROM user_libraries ul
		JOIN media_libraries l ON l.id = ul.library_id AND l.deleted_at IS NULL
		WHERE ul.user_id = ?`, userID)
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

// GrantUserLibrary 写入一条直授（重复授权幂等）。P3 的管理端保存会复用它。
func (db *DB) GrantUserLibrary(userID, libraryID string) error {
	_, err := db.Exec(`INSERT INTO user_libraries (user_id, library_id, created_at)
		VALUES (?,?,?) ON CONFLICT(user_id, library_id) DO NOTHING`,
		userID, libraryID, domain.NowString())
	return err
}

// ListLibrariesIn 返回范围内的库（id/name，无 root_path），范围外一条都不返回。
func (db *DB) ListLibrariesIn(scope domain.LibraryScope) ([]domain.LibraryBrief, error) {
	w, args := scopeWhere(scope, "id")
	rows, err := db.Query(`SELECT id, name FROM media_libraries WHERE deleted_at IS NULL`+
		w+` ORDER BY created_at ASC, id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.LibraryBrief{}
	for rows.Next() {
		var b domain.LibraryBrief
		if err := rows.Scan(&b.ID, &b.Name); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

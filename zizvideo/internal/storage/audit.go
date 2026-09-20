package storage

import "github.com/zizdog/govideo/internal/domain"

// AuditEntry is one write-operation record.
type AuditEntry struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Action    string `json:"action"`
	Object    string `json:"object"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id"`
	CreatedAt string `json:"created_at"`
}

// AddAudit appends an audit row. Write failures are the caller's to log.
func (db *DB) AddAudit(e *AuditEntry) error {
	if e.ID == "" {
		e.ID = domain.NewID("aud")
	}
	e.CreatedAt = domain.NowString()
	_, err := db.Exec(`INSERT INTO audit_log
		(id, user_id, username, action, object, ok, detail, request_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		e.ID, e.UserID, e.Username, e.Action, e.Object, boolToInt(e.OK),
		truncate(e.Detail, 300), e.RequestID, e.CreatedAt)
	return err
}

// ListAudit returns the newest audit rows.
func (db *DB) ListAudit(limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.Query(`SELECT id, user_id, username, action, object, ok, detail,
		request_id, created_at FROM audit_log ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var ok int
		if err := rows.Scan(&e.ID, &e.UserID, &e.Username, &e.Action, &e.Object, &ok,
			&e.Detail, &e.RequestID, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.OK = ok != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

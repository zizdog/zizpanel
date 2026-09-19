package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zizdog/govideo/internal/domain"
)

const userCols = `id, username, display_name, password_hash, role, status,
	COALESCE(last_login_at,''), created_at`

func scanUser(s interface{ Scan(...any) error }) (*domain.User, error) {
	var u domain.User
	err := s.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.Status,
		&u.LastLoginAt, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CountUsers returns how many live accounts exist (used to decide first-run setup).
func (db *DB) CountUsers() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(1) FROM users WHERE deleted_at IS NULL`).Scan(&n)
	return n, err
}

// CreateUser inserts a new account.
func (db *DB) CreateUser(u *domain.User) error {
	now := domain.NowString()
	u.CreatedAt = now
	_, err := db.Exec(`INSERT INTO users
		(id, username, display_name, password_hash, role, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, u.Role, u.Status, now, now)
	if err != nil {
		return fmt.Errorf("创建用户失败: %w", err)
	}
	return nil
}

// GetUserByUsername finds a live account by its unique name.
func (db *DB) GetUserByUsername(username string) (*domain.User, error) {
	row := db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ? AND deleted_at IS NULL`, username)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return u, err
}

// GetUserByID finds a live account by id.
func (db *DB) GetUserByID(id string) (*domain.User, error) {
	row := db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ? AND deleted_at IS NULL`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return u, err
}

// ListUsers returns all live accounts, newest first.
func (db *DB) ListUsers() ([]domain.User, error) {
	rows, err := db.Query(`SELECT ` + userCols + ` FROM users WHERE deleted_at IS NULL ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// UserPatch is a partial user update; nil fields are left untouched.
type UserPatch struct {
	DisplayName  *string
	Role         *string
	Status       *string
	PasswordHash *string
}

// UpdateUser applies a partial update and returns the fresh row.
func (db *DB) UpdateUser(id string, p UserPatch) (*domain.User, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) { sets = append(sets, col+" = ?"); args = append(args, v) }
	if p.DisplayName != nil {
		add("display_name", *p.DisplayName)
	}
	if p.Role != nil {
		add("role", *p.Role)
	}
	if p.Status != nil {
		add("status", *p.Status)
	}
	if p.PasswordHash != nil {
		add("password_hash", *p.PasswordHash)
		// A password reset must not leave the account locked out.
		add("failed_attempts", 0)
		add("locked_until", nil)
	}
	if len(sets) == 0 {
		return db.GetUserByID(id)
	}
	add("updated_at", domain.NowString())
	args = append(args, id)
	res, err := db.Exec(`UPDATE users SET `+joinComma(sets)+` WHERE id = ? AND deleted_at IS NULL`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, domain.ErrNotFound
	}
	return db.GetUserByID(id)
}

// LoginState reports the brute-force counters for an account.
type LoginState struct {
	FailedAttempts int
	LockedUntil    string
}

// GetLoginState reads the lockout counters.
func (db *DB) GetLoginState(username string) (LoginState, error) {
	var st LoginState
	var until sql.NullString
	err := db.QueryRow(`SELECT failed_attempts, locked_until FROM users
		WHERE username = ? AND deleted_at IS NULL`, username).Scan(&st.FailedAttempts, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return st, domain.ErrNotFound
	}
	st.LockedUntil = until.String
	return st, err
}

// RegisterLoginFailure increments the counter and locks the account once the
// threshold is reached; the lock duration doubles per extra failure.
func (db *DB) RegisterLoginFailure(id string, threshold int, window time.Duration) error {
	var attempts int
	if err := db.QueryRow(`SELECT failed_attempts FROM users WHERE id = ?`, id).Scan(&attempts); err != nil {
		return err
	}
	attempts++
	var locked any
	if attempts >= threshold {
		mult := 1 << min(attempts-threshold, 4)
		locked = domain.FormatTime(time.Now().Add(window * time.Duration(mult)))
	}
	_, err := db.Exec(`UPDATE users SET failed_attempts = ?, locked_until = ?, updated_at = ?
		WHERE id = ?`, attempts, locked, domain.NowString(), id)
	return err
}

// ClearLoginFailures resets counters after a successful login.
func (db *DB) ClearLoginFailures(id string) error {
	_, err := db.Exec(`UPDATE users SET failed_attempts = 0, locked_until = NULL, last_login_at = ?
		WHERE id = ?`, domain.NowString(), id)
	return err
}

// CreateSession stores the hash of a new session token.
func (db *DB) CreateSession(sessionID, userID, tokenHash string, expires time.Time) error {
	_, err := db.Exec(`INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at)
		VALUES (?,?,?,?,?)`, sessionID, userID, tokenHash,
		domain.NowString(), domain.FormatTime(expires))
	return err
}

// SessionUser resolves a session hash to its live user; disabled or deleted
// accounts resolve to an error so revocation is immediate.
func (db *DB) SessionUser(tokenHash string) (*domain.User, string, error) {
	var userID, expires string
	err := db.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", domain.ErrUnauthorized
	}
	if err != nil {
		return nil, "", err
	}
	if domain.ParseTime(expires).Before(time.Now()) {
		_, _ = db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
		return nil, "", domain.ErrUnauthorized
	}
	u, err := db.GetUserByID(userID)
	if err != nil {
		return nil, "", domain.ErrUnauthorized
	}
	if u.Status != domain.StatusActive {
		return nil, "", domain.ErrUnauthorized
	}
	return u, userID, nil
}

// DeleteSession revokes one session token.
func (db *DB) DeleteSession(tokenHash string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessions revokes every session of a user (disable / password reset).
func (db *DB) DeleteUserSessions(userID string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeExpiredSessions cleans stale rows; called once at startup.
func (db *DB) PurgeExpiredSessions() error {
	_, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, domain.NowString())
	return err
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

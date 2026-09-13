// Package auth 负责账号、密码、会话与两步验证。
//
// 安全约定：
//   - 密码用 bcrypt（cost 12）存储，永不落明文、永不写日志。
//   - 会话令牌只在生成时返回一次，数据库里只存 SHA-256 哈希。
//   - 连续登录失败按账号锁定，锁定时间写入数据库（重启不失效）。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/zizdog/zizpanel/internal/store"
)

// BcryptCost 是密码哈希强度。12 在现代 Mac 上约 200ms，够安全也不影响体验。
const BcryptCost = 12

var (
	ErrInvalidCredentials = errors.New("用户名或密码错误")
	ErrAccountLocked      = errors.New("账号已被锁定，请稍后再试")
	ErrTOTPRequired       = errors.New("需要两步验证码")
	ErrTOTPInvalid        = errors.New("两步验证码不正确")
	ErrUserExists         = errors.New("用户已存在")
	ErrUserNotFound       = errors.New("用户不存在")
	ErrSessionInvalid     = errors.New("会话无效或已过期")
	ErrWeakPassword       = errors.New("密码太短，至少 8 位")
)

// User 是一个面板账号。
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	TOTPSecret   string
	TOTPEnabled  bool
	IsAdmin      bool
	LastLoginAt  string
	LastLoginIP  string
	FailCount    int
	LockedUntil  string
	CreatedAt    string
}

// Manager 提供账号与会话操作。
type Manager struct {
	st        *store.Store
	secret    string
	sessHours int
	maxFail   int
	lockMins  int
}

// New 创建 Manager。
func New(st *store.Store, secret string, sessionHours, maxFail, lockMins int) *Manager {
	if sessionHours <= 0 {
		sessionHours = 72
	}
	if maxFail <= 0 {
		maxFail = 5
	}
	if lockMins <= 0 {
		lockMins = 15
	}
	return &Manager{st: st, secret: secret, sessHours: sessionHours, maxFail: maxFail, lockMins: lockMins}
}

// ---------- 账号 ----------

// HasAnyUser 判断是否已经初始化过管理员（用于决定是否展示安装向导）。
func (m *Manager) HasAnyUser(ctx context.Context) (bool, error) {
	var n int
	err := m.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n > 0, err
}

// CreateUser 创建账号。
func (m *Manager) CreateUser(ctx context.Context, username, password string, isAdmin bool) (*User, error) {
	username = strings.TrimSpace(username)
	if len(username) < 2 {
		return nil, errors.New("用户名至少 2 个字符")
	}
	if len(password) < 8 {
		return nil, ErrWeakPassword
	}
	var exists int
	if err := m.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE username=?`, username).Scan(&exists); err != nil {
		return nil, err
	}
	if exists > 0 {
		return nil, ErrUserExists
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return nil, fmt.Errorf("密码加密失败: %w", err)
	}
	admin := 0
	if isAdmin {
		admin = 1
	}
	res, err := m.st.DB().ExecContext(ctx,
		`INSERT INTO users(username,password_hash,is_admin) VALUES(?,?,?)`,
		username, string(hash), admin)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return m.UserByID(ctx, id)
}

// HashPassword 生成 bcrypt 哈希，供 CLI 与测试使用。
func HashPassword(pwd string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pwd), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UserByID 按 ID 取账号。
func (m *Manager) UserByID(ctx context.Context, id int64) (*User, error) {
	row := m.st.DB().QueryRowContext(ctx,
		`SELECT id,username,password_hash,totp_secret,totp_enabled,is_admin,
		        last_login_at,last_login_ip,fail_count,locked_until,created_at
		 FROM users WHERE id=?`, id)
	return scanUser(row)
}

// UserByName 按用户名取账号。
func (m *Manager) UserByName(ctx context.Context, name string) (*User, error) {
	row := m.st.DB().QueryRowContext(ctx,
		`SELECT id,username,password_hash,totp_secret,totp_enabled,is_admin,
		        last_login_at,last_login_ip,fail_count,locked_until,created_at
		 FROM users WHERE username=?`, name)
	return scanUser(row)
}

// CountUsers 返回账号数量。
func (m *Manager) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := m.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// ListUsers 返回所有账号（不含哈希）。
func (m *Manager) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := m.st.DB().QueryContext(ctx,
		`SELECT id,username,password_hash,totp_secret,totp_enabled,is_admin,
		        last_login_at,last_login_ip,fail_count,locked_until,created_at
		 FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		u.PasswordHash = "" // 绝不下发哈希
		out = append(out, u)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanUser(s scanner) (*User, error) {
	var u User
	var totpEnabled, isAdmin int
	err := s.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &totpEnabled,
		&isAdmin, &u.LastLoginAt, &u.LastLoginIP, &u.FailCount, &u.LockedUntil, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	u.TOTPEnabled = totpEnabled == 1
	u.IsAdmin = isAdmin == 1
	return &u, nil
}

// ChangePassword 修改密码，并吊销该账号全部会话。
func (m *Manager) ChangePassword(ctx context.Context, userID int64, oldPwd, newPwd string) error {
	u, err := m.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if oldPwd != "" {
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(oldPwd)) != nil {
			return ErrInvalidCredentials
		}
	}
	if len(newPwd) < 8 {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPwd), BcryptCost)
	if err != nil {
		return err
	}
	if _, err := m.st.DB().ExecContext(ctx,
		`UPDATE users SET password_hash=?, updated_at=datetime('now','localtime') WHERE id=?`,
		string(hash), userID); err != nil {
		return err
	}
	_, _ = m.RevokeAll(ctx, userID)
	return nil
}

// SetPassword 直接重设密码（管理员操作，不校验旧密码）。
func (m *Manager) SetPassword(ctx context.Context, userID int64, newPwd string) error {
	if len(newPwd) < 8 {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPwd), BcryptCost)
	if err != nil {
		return err
	}
	_, err = m.st.DB().ExecContext(ctx,
		`UPDATE users SET password_hash=?, updated_at=datetime('now','localtime') WHERE id=?`,
		string(hash), userID)
	_, _ = m.RevokeAll(ctx, userID)
	return err
}

// DeleteUser 删除账号；不允许删掉最后一个账号。
func (m *Manager) DeleteUser(ctx context.Context, userID int64) error {
	n, err := m.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n <= 1 {
		return errors.New("至少要保留一个账号")
	}
	_, err = m.st.DB().ExecContext(ctx, `DELETE FROM users WHERE id=?`, userID)
	return err
}

// ---------- 登录 ----------

// LoginResult 描述一次登录尝试的结果。
type LoginResult struct {
	User      *User
	Token     string // 仅在成功且不需要 2FA 时返回
	NeedTOTP  bool
	ExpiresAt time.Time
}

// Login 校验密码。若账号开启了 2FA 且未提供 code，返回 NeedTOTP=true。
func (m *Manager) Login(ctx context.Context, username, password, code, ip, ua string) (*LoginResult, error) {
	u, err := m.UserByName(ctx, username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// 用户不存在也走一次 bcrypt，避免通过响应时间枚举用户名
			_ = bcrypt.CompareHashAndPassword(
				[]byte("$2y$12$0000000000000000000000000000000000000000000000000000"),
				[]byte(password))
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if locked(u.LockedUntil) {
		return nil, ErrAccountLocked
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		m.bumpFail(ctx, u)
		return nil, ErrInvalidCredentials
	}
	if u.TOTPEnabled {
		if code == "" {
			// 密码已通过，但还差一步。此时绝不能建会话。
			return &LoginResult{User: u, NeedTOTP: true}, nil
		}
		return m.finishLogin(ctx, u, code, ip, ua, true)
	}
	return m.finishLogin(ctx, u, "", ip, ua, false)
}

// CompleteTOTP 用于两步登录的第二步：校验验证码后建会话。
func (m *Manager) CompleteTOTP(ctx context.Context, userID int64, code, ip, ua string) (*LoginResult, error) {
	u, err := m.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !u.TOTPEnabled {
		return nil, ErrTOTPInvalid
	}
	if locked(u.LockedUntil) {
		return nil, ErrAccountLocked
	}
	return m.finishLogin(ctx, u, code, ip, ua, true)
}

// finishLogin 是登录流程的最后一步：校验验证码（如需要）、清失败计数、建会话。
func (m *Manager) finishLogin(ctx context.Context, u *User, code, ip, ua string, checkTOTP bool) (*LoginResult, error) {
	if checkTOTP {
		if !VerifyTOTP(u.TOTPSecret, code) {
			m.bumpFail(ctx, u)
			return nil, ErrTOTPInvalid
		}
	}
	_, _ = m.st.DB().ExecContext(ctx,
		`UPDATE users SET fail_count=0, locked_until='', last_login_at=datetime('now','localtime'),
		 last_login_ip=? WHERE id=?`, ip, u.ID)

	tok, exp, err := m.NewSession(ctx, u.ID, ip, ua)
	if err != nil {
		return nil, err
	}
	return &LoginResult{User: u, Token: tok, ExpiresAt: exp}, nil
}

func (m *Manager) bumpFail(ctx context.Context, u *User) {
	n := u.FailCount + 1
	lockedUntil := ""
	if n >= m.maxFail {
		lockedUntil = time.Now().Add(time.Duration(m.lockMins) * time.Minute).
			Format("2006-01-02 15:04:05")
		n = 0 // 锁定期满后重新计数
	}
	_, _ = m.st.DB().ExecContext(ctx,
		`UPDATE users SET fail_count=?, locked_until=? WHERE id=?`, n, lockedUntil, u.ID)
}

func locked(until string) bool {
	if until == "" {
		return false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", until, time.Local)
	if err != nil {
		return false
	}
	return time.Now().Before(t)
}

// Unlock 手动解锁账号。
func (m *Manager) Unlock(ctx context.Context, userID int64) error {
	_, err := m.st.DB().ExecContext(ctx,
		`UPDATE users SET fail_count=0, locked_until='' WHERE id=?`, userID)
	return err
}

// ---------- 会话 ----------

// NewSession 生成会话令牌。返回的 token 只在这一次出现，数据库存哈希。
func (m *Manager) NewSession(ctx context.Context, userID int64, ip, ua string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := hex.EncodeToString(raw)
	exp := time.Now().Add(time.Duration(m.sessHours) * time.Hour)
	if len(ua) > 300 {
		ua = ua[:300]
	}
	_, err := m.st.DB().ExecContext(ctx,
		`INSERT INTO sessions(token_hash,user_id,ip,user_agent,expires_at)
		 VALUES(?,?,?,?,?)`,
		HashToken(token), userID, ip, ua, exp.Format("2006-01-02 15:04:05"))
	if err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// HashToken 对令牌做 SHA-256。令牌本身是高熵随机串，不需要再加盐。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AuthSession 校验令牌并返回账号。同时刷新 last_seen。
func (m *Manager) AuthSession(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	var userID int64
	var expires string
	err := m.st.DB().QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM sessions WHERE token_hash=?`,
		HashToken(token)).Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	exp, err := time.ParseInLocation("2006-01-02 15:04:05", expires, time.Local)
	if err != nil || time.Now().After(exp) {
		_, _ = m.st.DB().ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, HashToken(token))
		return nil, ErrSessionInvalid
	}
	_, _ = m.st.DB().ExecContext(ctx,
		`UPDATE sessions SET last_seen=datetime('now','localtime') WHERE token_hash=?`, HashToken(token))
	return m.UserByID(ctx, userID)
}

// Revoke 登出：删除指定会话。
func (m *Manager) Revoke(ctx context.Context, token string) error {
	_, err := m.st.DB().ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, HashToken(token))
	return err
}

// RevokeAll 吊销某账号的全部会话（改密码时调用）。
func (m *Manager) RevokeAll(ctx context.Context, userID int64) (int64, error) {
	res, err := m.st.DB().ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListSessions 返回某账号的在线会话（不含令牌）。
func (m *Manager) ListSessions(ctx context.Context, userID int64) ([]map[string]string, error) {
	rows, err := m.st.DB().QueryContext(ctx,
		`SELECT ip,user_agent,created_at,last_seen,expires_at FROM sessions
		 WHERE user_id=? ORDER BY last_seen DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var ip, ua, ca, ls, ea string
		if err := rows.Scan(&ip, &ua, &ca, &ls, &ea); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{
			"ip": ip, "user_agent": ua, "created_at": ca,
			"last_seen": ls, "expires_at": ea,
		})
	}
	return out, rows.Err()
}

// ---------- TOTP 两步验证 ----------

// NewTOTPSecret 生成 base32 密钥（RFC 4648，无填充），兼容 Google Authenticator。
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// EnableTOTP 在验证码校验通过后开启两步验证。
func (m *Manager) EnableTOTP(ctx context.Context, userID int64, secret, code string) error {
	if !VerifyTOTP(secret, code) {
		return ErrTOTPInvalid
	}
	_, err := m.st.DB().ExecContext(ctx,
		`UPDATE users SET totp_secret=?, totp_enabled=1, updated_at=datetime('now','localtime')
		 WHERE id=?`, secret, userID)
	return err
}

// DisableTOTP 关闭两步验证，需要当前密码确认。
func (m *Manager) DisableTOTP(ctx context.Context, userID int64, password string) error {
	u, err := m.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return ErrInvalidCredentials
	}
	_, err = m.st.DB().ExecContext(ctx,
		`UPDATE users SET totp_secret='', totp_enabled=0 WHERE id=?`, userID)
	return err
}

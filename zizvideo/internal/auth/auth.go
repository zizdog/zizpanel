// Package auth owns password hashing, sessions, CSRF and login throttling.
package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/storage"
)

// Iterations is the PBKDF2 work factor. Stored in the hash so it can grow.
const Iterations = 210000

const hashPrefix = "pbkdf2-sha256"

// HashPassword derives a salted PBKDF2-SHA256 hash; plaintext is never stored.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashPrefix, Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword compares a candidate password against a stored hash.
func VerifyPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != hashPrefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// HashToken is the at-rest representation of a session token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// LoadSecret reads (or creates) the HMAC secret used to derive CSRF tokens.
func LoadSecret(dataDir string) ([]byte, error) {
	if env := os.Getenv("ZV_AUTH_SECRET"); env != "" {
		return []byte(env), nil
	}
	path := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 16 {
		return b, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return nil, err
	}
	return buf, nil
}

// Manager ties authentication to the store and configuration.
type Manager struct {
	DB        *storage.DB
	Secret    []byte
	TTL       time.Duration
	Threshold int
	Window    time.Duration
	Limiter   *Limiter
}

// NewManager builds a Manager with an in-memory login limiter.
func NewManager(db *storage.DB, secret []byte, ttl time.Duration, threshold int, window time.Duration) *Manager {
	return &Manager{
		DB: db, Secret: secret, TTL: ttl, Threshold: threshold, Window: window,
		Limiter: NewLimiter(threshold, window),
	}
}

// NewToken returns a fresh opaque session token.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Login verifies credentials and opens a session. It returns the user and token.
func (m *Manager) Login(ip, username, password string) (*domain.User, string, error) {
	key := ip + "|" + strings.ToLower(username)
	if wait, ok := m.Limiter.Blocked(key); !ok {
		return nil, "", &domain.Error{Code: "RATE_LIMITED",
			Message: fmt.Sprintf("尝试过于频繁，请 %d 秒后再试", int(wait.Seconds())+1), Status: 429}
	}
	// Exponential slowdown: slows brute force without locking honest typos out.
	if d := m.Limiter.Delay(key); d > 0 {
		time.Sleep(d)
	}

	u, err := m.DB.GetUserByUsername(username)
	if err != nil {
		// Burn comparable work so a missing account is not faster to probe.
		_, _ = pbkdf2.Key(sha256.New, password, []byte("zizvideo-dummy-salt"), Iterations, 32)
		m.Limiter.Fail(key)
		return nil, "", domain.ErrUnauthorized
	}
	if u.Status != domain.StatusActive {
		m.Limiter.Fail(key)
		return nil, "", domain.ErrUnauthorized
	}
	if st, err := m.DB.GetLoginState(username); err == nil && st.LockedUntil != "" {
		if until := domain.ParseTime(st.LockedUntil); until.After(time.Now()) {
			m.Limiter.Fail(key)
			return nil, "", &domain.Error{Code: "AUTH_LOCKED",
				Message: "账号已临时锁定，请稍后再试", Status: 429}
		}
	}
	if !VerifyPassword(u.PasswordHash, password) {
		_ = m.DB.RegisterLoginFailure(u.ID, m.Threshold, m.Window)
		m.Limiter.Fail(key)
		if st, err := m.DB.GetLoginState(username); err == nil && st.LockedUntil != "" {
			return nil, "", &domain.Error{Code: "AUTH_LOCKED",
				Message: "失败次数过多，账号已临时锁定", Status: 429}
		}
		return nil, "", domain.ErrUnauthorized
	}

	token, err := NewToken()
	if err != nil {
		return nil, "", err
	}
	expires := time.Now().Add(m.TTL)
	if err := m.DB.CreateSession(domain.NewID("ses"), u.ID, HashToken(token), expires); err != nil {
		return nil, "", err
	}
	if err := m.DB.ClearLoginFailures(u.ID); err != nil {
		return nil, "", err
	}
	m.Limiter.Success(key)
	return u, token, nil
}

// Logout revokes a session token.
func (m *Manager) Logout(token string) {
	_ = m.DB.DeleteSession(HashToken(token))
}

// Authenticate resolves a token to a live, enabled user. Role is re-read from
// the database on every request so disabling an account is immediate.
func (m *Manager) Authenticate(token string) (*domain.User, error) {
	if token == "" {
		return nil, domain.ErrUnauthorized
	}
	u, _, err := m.DB.SessionUser(HashToken(token))
	if err != nil {
		return nil, domain.ErrUnauthorized
	}
	return u, nil
}

// IssueSession opens a session for an already-authenticated user (first-run setup).
func (m *Manager) IssueSession(userID string) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	if err := m.DB.CreateSession(domain.NewID("ses"), userID, HashToken(token),
		time.Now().Add(m.TTL)); err != nil {
		return "", err
	}
	return token, nil
}

// CSRFFor derives the double-submit token bound to a session token.
func (m *Manager) CSRFFor(token string) string {
	mac := hmac.New(sha256.New, m.Secret)
	mac.Write([]byte("csrf:" + HashToken(token)))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// CSRFOK compares the X-CSRF-Token header against the derived value.
//
// Only the header counts: accepting the cookie value here would defeat the
// double-submit scheme entirely, since browsers attach cookies automatically.
func (m *Manager) CSRFOK(header, token string) bool {
	if header == "" || token == "" {
		return false
	}
	return hmac.Equal([]byte(header), []byte(m.CSRFFor(token)))
}

// ============================================================================
//  Login throttling
// ============================================================================

// Limiter is an in-memory exponential backoff keyed by IP+username.
type Limiter struct {
	mu        sync.Mutex
	entries   map[string]*limitEntry
	threshold int
	window    time.Duration
}

type limitEntry struct {
	fails    int
	lastFail time.Time
}

// NewLimiter builds a Limiter; threshold <= 1 falls back to 5.
func NewLimiter(threshold int, window time.Duration) *Limiter {
	if threshold <= 1 {
		threshold = 5
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	return &Limiter{entries: map[string]*limitEntry{}, threshold: threshold, window: window}
}

// backoff returns the exponential slowdown applied to the next attempt.
func backoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := 200 * time.Millisecond * time.Duration(1<<uint(min(n-1, 4)))
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// Blocked reports whether the key is hard-locked and how long it stays locked.
// Below the threshold it stays open and only Delay applies.
func (l *Limiter) Blocked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return 0, true
	}
	if time.Since(e.lastFail) > l.window {
		delete(l.entries, key)
		return 0, true
	}
	if e.fails >= l.threshold {
		return l.window - time.Since(e.lastFail), false
	}
	return 0, true
}

// Delay is the exponential slowdown for the next attempt.
func (l *Limiter) Delay(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || time.Since(e.lastFail) > l.window {
		return 0
	}
	return backoff(e.fails)
}

// Fail records one failed attempt.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || time.Since(e.lastFail) > l.window {
		e = &limitEntry{}
		l.entries[key] = e
	}
	e.fails++
	e.lastFail = time.Now()
	if len(l.entries) > 4096 {
		for k, v := range l.entries {
			if time.Since(v.lastFail) > l.window {
				delete(l.entries, k)
			}
		}
	}
}

// Success clears the key's failure history.
func (l *Limiter) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// Failures exposes the current counter (tests and diagnostics only).
func (l *Limiter) Failures(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok {
		return e.fails
	}
	return 0
}

var _ = errors.Is

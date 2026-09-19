// Package config 管 mac军刀 的数据目录、口令与登录会话。
//
// 结论：口令用标准库 pbkdf2(sha256, 60 万次) + 每用户随机 salt（坑 C1）；
// 会话只在内存里，进程重启即失效；写操作靠双提交 CSRF（照面板做法）。
package config

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 会话 Cookie 名与面板一致（双提交模式：会话 HttpOnly，CSRF 可读）。
const (
	CookieSession = "ms_session"
	CookieCSRF    = "ms_csrf"
)

const (
	pbkdf2Iter = 600000
	saltLen    = 16
	keyLen     = 32
	sessionTTL = 12 * time.Hour
)

// Store 是 <data>/config.json 的内容。
type Store struct {
	mu       sync.Mutex
	path     string
	dataDir  string
	Version  int       `json:"version"`
	User     string    `json:"user"`
	Hash     string    `json:"hash"`
	Salt     string    `json:"salt"`
	Secret   string    `json:"secret"`
	Created  time.Time `json:"created_at"`
	sessions map[string]time.Time
}

// DefaultDataDir 返回 ~/Library/Application Support/macsaber。
// 读 HOME 环境变量而不是 os.UserHomeDir：测试要能把它指到临时目录（坑 C2）。
func DefaultDataDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, "Library", "Application Support", "macsaber")
}

// DefaultWriteRoot 返回 ~/MacSaberFiles。
func DefaultWriteRoot() string {
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, "MacSaberFiles")
}

// Open 读取（或原地新建）配置；数据目录不存在就创建。
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, "config.json"), dataDir: dataDir, sessions: map[string]time.Time{}}
	raw, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, s); err != nil {
			return nil, fmt.Errorf("配置文件损坏 %s: %w", s.path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		s.Version = 1
		if s.Secret, err = randHex(32); err != nil {
			return nil, err
		}
		s.Created = time.Now()
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	if s.Secret == "" {
		if s.Secret, err = randHex(32); err != nil {
			return nil, err
		}
		_ = s.saveLocked()
	}
	return s, nil
}

// DataDir 返回数据目录。
func (s *Store) DataDir() string { return s.dataDir }

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// Inited 表示已经设置过用户名与口令。
func (s *Store) Inited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.User != "" && s.Hash != "" && s.Salt != ""
}

// Setup 是首次初始化：设用户名 + 口令。已初始化则拒绝（不覆盖既有账号）。
func (s *Store) Setup(user, pass string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.User != "" && s.Hash != "" {
		return errors.New("已初始化，不能再执行初始化")
	}
	user = strings.TrimSpace(user)
	if len(user) < 3 {
		return errors.New("用户名至少 3 个字符")
	}
	if len(pass) < 8 {
		return errors.New("口令至少 8 个字符")
	}
	salt, err := randBytes(saltLen)
	if err != nil {
		return err
	}
	key, err := pbkdf2.Key(sha256.New, pass, salt, pbkdf2Iter, keyLen)
	if err != nil {
		return err
	}
	s.User = user
	s.Salt = hex.EncodeToString(salt)
	s.Hash = hex.EncodeToString(key)
	return s.saveLocked()
}

// Verify 校验用户名口令。恒定时间比较，且用户不存在时也付一次哈希代价。
func (s *Store) Verify(user, pass string) bool {
	s.mu.Lock()
	saltHex, hashHex, want := s.Salt, s.Hash, s.User
	s.mu.Unlock()
	if saltHex == "" || hashHex == "" {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pass, salt, pbkdf2Iter, keyLen)
	if err != nil {
		return false
	}
	ok := subtle.ConstantTimeCompare([]byte(hex.EncodeToString(got)), []byte(hashHex)) == 1
	// 用户名也恒定时间比对，避免"存在性"泄露。
	return ok && subtle.ConstantTimeCompare([]byte(user), []byte(want)) == 1
}

// NewSession 建立会话并返回令牌。
func (s *Store) NewSession() string {
	tok, err := randHex(32)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.sessions[tok] = time.Now().Add(sessionTTL)
	return tok
}

// SessionValid 校验会话令牌。
func (s *Store) SessionValid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, tok)
		return false
	}
	return true
}

// DropSession 退出登录。
func (s *Store) DropSession(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, tok)
}

func (s *Store) gcLocked() {
	now := time.Now()
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
		}
	}
}

// Token 由会话令牌派生 CSRF token（HMAC），无需额外存储 —— 与面板同款做法。
// 只认请求头，绝不回退读 Cookie：Cookie 是浏览器自动带的，回退等于没有防护。
func (s *Store) Token(session string) string {
	mac := hmac.New(sha256.New, []byte(s.Secret))
	mac.Write([]byte("csrf:" + session))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// CSRFOk 校验双提交头。
func (s *Store) CSRFOk(r *http.Request, session string) bool {
	got := r.Header.Get("X-CSRF-Token")
	if got == "" || session == "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(s.Token(session)))
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func randHex(n int) (string, error) {
	b, err := randBytes(n)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 两步验证的中间凭据。
//
// 问题：用户提交「用户名 + 密码」后如果账号开了 2FA，此时还不能建会话，
// 但第二步需要知道"这是谁"。
//
// 方案：服务端签一个短期 challenge 令牌（HMAC 签名，5 分钟有效），
// 客户端拿它 + 验证码换正式会话。对比"把用户名密码在客户端暂存再提交一次"
// 的做法，密码不会在浏览器内存里多留一份，令牌也无法被篡改。
//
// 另外维护已用 nonce 集合，防止同一个 challenge 被重放。

const challengeTTL = 5 * time.Minute

type challengeStore struct {
	mu     sync.Mutex
	used   map[string]time.Time
	lastGC time.Time
}

var challenges = &challengeStore{used: map[string]time.Time{}}

// IssueTOTPChallenge 生成两步验证中间令牌。
func (m *Manager) IssueTOTPChallenge(userID int64) string {
	exp := time.Now().Add(challengeTTL).Unix()
	nonce := randomString(8)
	payload := fmt.Sprintf("%d:%d:%s", userID, exp, nonce)
	sig := m.sign(payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload + ":" + sig))
}

// ParseTOTPChallenge 校验中间令牌，成功则返回 userID。
// 同一个令牌只能使用一次。
func (m *Manager) ParseTOTPChallenge(token string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, ErrSessionInvalid
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 4 {
		return 0, ErrSessionInvalid
	}
	payload := strings.Join(parts[:3], ":")
	if !hmac.Equal([]byte(parts[3]), []byte(m.sign(payload))) {
		return 0, ErrSessionInvalid
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, ErrSessionInvalid
	}
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, ErrSessionInvalid
	}

	nonce := parts[2]
	challenges.mu.Lock()
	defer challenges.mu.Unlock()
	challenges.gcLocked()
	if _, dup := challenges.used[nonce]; dup {
		return 0, ErrSessionInvalid
	}
	challenges.used[nonce] = time.Now()
	return uid, nil
}

func (m *Manager) sign(payload string) string {
	h := hmac.New(sha256.New, []byte(m.secret))
	h.Write([]byte("totp-challenge:" + payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s *challengeStore) gcLocked() {
	if time.Since(s.lastGC) < time.Minute {
		return
	}
	cutoff := time.Now().Add(-challengeTTL)
	for k, t := range s.used {
		if t.Before(cutoff) {
			delete(s.used, k)
		}
	}
	s.lastGC = time.Now()
}

// randomString 生成 n 字节的随机十六进制串。
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

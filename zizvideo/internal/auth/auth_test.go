package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("哈希里出现了明文口令")
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") {
		t.Fatalf("哈希格式 = %q", hash)
	}
	if !VerifyPassword(hash, pw) {
		t.Fatal("正确口令校验失败")
	}
	if VerifyPassword(hash, pw+"x") {
		t.Fatal("错误口令通过了校验")
	}
	if VerifyPassword("garbage", pw) {
		t.Fatal("畸形哈希不应通过")
	}
}

func TestHashPasswordUsesFreshSalt(t *testing.T) {
	a, err := HashPassword("same-password")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same-password")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("相同口令必须产生不同哈希（盐未随机）")
	}
}

func TestCSRFIsBoundToTheSessionToken(t *testing.T) {
	m := &Manager{Secret: []byte("s3cret")}
	tokA, tokB := "token-a", "token-b"
	if m.CSRFFor(tokA) == m.CSRFFor(tokB) {
		t.Fatal("不同会话必须得到不同 CSRF token")
	}
	if m.CSRFFor(tokA) != m.CSRFFor(tokA) {
		t.Fatal("同一会话的 CSRF token 必须稳定")
	}
	if !m.CSRFOK(m.CSRFFor(tokA), tokA) {
		t.Fatal("正确 token 应通过")
	}
	if m.CSRFOK("", tokA) {
		t.Fatal("缺失头必须失败")
	}
	if m.CSRFOK(m.CSRFFor(tokB), tokA) {
		t.Fatal("跨会话 token 必须失败")
	}
	if m.CSRFOK(m.CSRFFor(tokA), "") {
		t.Fatal("无会话必须失败")
	}
}

func TestLimiterBacksOffExponentiallyAndThenBlocks(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	key := "127.0.0.1|admin"
	if _, ok := l.Blocked(key); !ok {
		t.Fatal("首次应放行")
	}
	l.Fail(key)
	if _, ok := l.Blocked(key); !ok {
		t.Fatal("低于阈值不应硬锁定")
	}
	first := l.Delay(key)
	if first <= 0 {
		t.Fatalf("第一次失败后应有退避, 得到 %v", first)
	}
	l.Fail(key)
	if second := l.Delay(key); second <= first {
		t.Fatalf("退避必须指数增长: %v -> %v", first, second)
	}
	l.Fail(key)
	if _, ok := l.Blocked(key); ok {
		t.Fatal("达到阈值后必须硬锁定")
	}
	l.Success(key)
	if _, ok := l.Blocked(key); !ok {
		t.Fatal("成功登录后应清零")
	}
	if d := l.Delay(key); d != 0 {
		t.Fatalf("清零后不应再有延迟: %v", d)
	}
}

func TestLoadSecretIsStableAndPrivate(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("密钥必须跨重启稳定")
	}
	if len(a) < 16 {
		t.Fatalf("密钥长度 %d 过短", len(a))
	}
}

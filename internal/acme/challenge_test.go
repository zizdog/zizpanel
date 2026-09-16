package acme

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/challenge"
)

// ---------------- http-01 webroot ----------------

func TestWebrootProviderWritesAndCleansUp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wwwroot") // 故意不存在：Present 必须自己 MkdirAll
	rec := &logRecorder{}
	p := &webrootProvider{root: root, emit: rec.emitf}

	const token = "tok-123_ABC"
	if err := p.Present("example.com", token, "key-auth-value"); err != nil {
		t.Fatalf("Present 失败: %v", err)
	}

	path := filepath.Join(root, ".well-known", "acme-challenge", token)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("挑战文件未落盘: %v", err)
	}
	if string(data) != "key-auth-value" {
		t.Fatalf("挑战文件内容错误: %q", data)
	}
	assertMode(t, path, 0o644)

	// 日志里不能出现 token（它会拼进文件名）。
	if strings.Contains(rec.all(), token) {
		t.Fatalf("日志泄露了挑战 token:\n%s", rec.all())
	}
	if !strings.Contains(rec.all(), "http-01") {
		t.Fatalf("日志应说明正在进行 http-01:\n%s", rec.all())
	}

	if err := p.CleanUp("example.com", token, "key-auth-value"); err != nil {
		t.Fatalf("CleanUp 失败: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CleanUp 未删除挑战文件: %v", err)
	}
	// 重复清理不应报错（CA 可能重复调用）。
	if err := p.CleanUp("example.com", token, "key-auth-value"); err != nil {
		t.Fatalf("重复 CleanUp 应幂等: %v", err)
	}
}

func TestWebrootProviderRejectsUnsafeToken(t *testing.T) {
	root := t.TempDir()
	p := &webrootProvider{root: root, emit: func(string, ...any) {}}

	for _, token := range []string{"", "../escape", "a/b", "a\\b", "a b", "tok\n"} {
		if err := p.Present("example.com", token, "x"); err == nil {
			t.Fatalf("非法 token %q 未被拒绝", token)
		}
	}
	// 空 root 必须报错，而不是把文件写到进程当前目录。
	empty := &webrootProvider{root: " ", emit: func(string, ...any) {}}
	if err := empty.Present("example.com", "tok", "x"); err == nil {
		t.Fatal("空网站根目录未被拒绝")
	}
}

func TestIsTokenSafe(t *testing.T) {
	for _, ok := range []string{"a", "A0-_", "xY9-_z"} {
		if !isTokenSafe(ok) {
			t.Fatalf("%q 应被判为合法 token", ok)
		}
	}
	for _, bad := range []string{"", "a.b", "a/b", "a+b", "a=b", "a b", "%", "中文"} {
		if isTokenSafe(bad) {
			t.Fatalf("%q 应被判为非法 token", bad)
		}
	}
}

// ---------------- dns-01 provider 包装 ----------------

// fakeDNSProvider 记录 lego 会怎么调用它。
type fakeDNSProvider struct {
	presented []string
	cleaned   []string
	err       error
}

func (f *fakeDNSProvider) Present(domain, token, keyAuth string) error {
	f.presented = append(f.presented, domain)
	return f.err
}

func (f *fakeDNSProvider) CleanUp(domain, token, keyAuth string) error {
	f.cleaned = append(f.cleaned, domain)
	return f.err
}

func TestLoggingDNSProviderForwardsAndRedacts(t *testing.T) {
	rec := &logRecorder{}
	inner := &fakeDNSProvider{}
	p := &loggingDNSProvider{
		inner:    inner,
		emit:     rec.emitf,
		timeout:  3 * time.Minute,
		interval: 5 * time.Second,
	}

	const token = "dns-challenge-token-value"
	if err := p.Present("example.com", token, "key-auth"); err != nil {
		t.Fatalf("Present 失败: %v", err)
	}
	if len(inner.presented) != 1 || inner.presented[0] != "example.com" {
		t.Fatalf("未透传给 lego provider: %v", inner.presented)
	}
	if strings.Contains(rec.all(), token) || strings.Contains(rec.all(), "key-auth") {
		t.Fatalf("dns-01 日志泄露了 token/keyAuth:\n%s", rec.all())
	}
	if !strings.Contains(rec.all(), "example.com") {
		t.Fatalf("dns-01 日志应包含域名:\n%s", rec.all())
	}

	timeout, interval := p.Timeout()
	if timeout != 3*time.Minute || interval != 5*time.Second {
		t.Fatalf("Timeout 未透传: %v / %v", timeout, interval)
	}

	if err := p.CleanUp("example.com", token, "key-auth"); err != nil {
		t.Fatalf("CleanUp 失败: %v", err)
	}
	if len(inner.cleaned) != 1 {
		t.Fatalf("CleanUp 未透传: %v", inner.cleaned)
	}

	// provider 报错时要带上下文，方便排查是哪一步失败。
	inner.err = errors.New("boom")
	if err := p.Present("example.com", token, "key-auth"); err == nil || !strings.Contains(err.Error(), "dns-01") {
		t.Fatalf("provider 错误未包装出上下文: %v", err)
	}
}

// ---------------- DNS 凭据注入 ----------------

func TestApplyDNSEnvInjectsAndRestores(t *testing.T) {
	const (
		keyExisting = "ZIZPANEL_TEST_ACME_KEY"
		keyNew      = "ZIZPANEL_TEST_ACME_NEW"
	)
	// 手工设置并还原，避免 t.Setenv 与直接 os.Setenv 混用造成的困惑。
	t.Setenv(keyExisting, "old-value")
	os.Unsetenv(keyNew)
	defer os.Unsetenv(keyNew)

	spec := &DNSProviderSpec{
		Name: "cloudflare",
		Env: map[string]string{
			keyExisting: "new-value",
			keyNew:      "fresh-value",
		},
	}
	restore, err := applyDNSEnv(spec)
	if err != nil {
		t.Fatalf("applyDNSEnv 失败: %v", err)
	}
	if got := os.Getenv(keyExisting); got != "new-value" {
		t.Fatalf("已有环境变量未注入: %q", got)
	}
	if got := os.Getenv(keyNew); got != "fresh-value" {
		t.Fatalf("新环境变量未注入: %q", got)
	}

	restore()
	if got := os.Getenv(keyExisting); got != "old-value" {
		t.Fatalf("已有环境变量未还原: %q", got)
	}
	if _, ok := os.LookupEnv(keyNew); ok {
		t.Fatal("新环境变量未清除")
	}
}

func TestApplyDNSEnvRejectsBadKey(t *testing.T) {
	const badKey = "ZIZPANEL_TEST_A=B"
	spec := &DNSProviderSpec{Name: "cloudflare", Env: map[string]string{badKey: "v", "": "v2"}}
	if _, err := applyDNSEnv(spec); err == nil {
		t.Fatal("非法环境变量名未被拒绝")
	}
	if _, ok := os.LookupEnv(badKey); ok {
		t.Fatal("非法环境变量名被写进了进程环境")
	}
}

func TestRedact(t *testing.T) {
	const secret = "my-super-secret-token-123456"
	rememberSecret(secret)

	got := redact("before " + secret + " after")
	if strings.Contains(got, secret) {
		t.Fatalf("脱敏失败: %q", got)
	}
	if got != "before *** after" {
		t.Fatalf("脱敏结果不符合预期: %q", got)
	}

	// 太短的值不登记，避免把正常日志打糊。
	rememberSecret("abc")
	if redact("abc") != "abc" {
		t.Fatal("过短的值不应被登记为敏感串")
	}
}

// TestDNSProviderRegistryConstructsRealProviders 走真实的 lego provider 注册表，
// 但只构造 provider、不发起任何网络请求。它锁死三件事：
//  1. 注册表入口（dns.NewDNSChallengeProviderByName）的名字与用法正确；
//  2. DNSProviderSpec.Env 通过进程环境真的被 provider 读到了（缺凭据会构造失败）；
//  3. 凭据用完会从进程环境里清掉，不污染面板进程。
//
// 真实 DNS API 调用与 CA 验证不在单测覆盖范围内（离线要求）。
func TestDNSProviderRegistryConstructsRealProviders(t *testing.T) {
	m, _, _ := newTestManager(t)

	cases := []struct {
		name string
		env  map[string]string
	}{
		{"cloudflare", map[string]string{"CF_DNS_API_TOKEN": "test-token-1234567890"}},
		{"alidns", map[string]string{
			"ALICLOUD_ACCESS_KEY": "test-access-key-1234567890",
			"ALICLOUD_SECRET_KEY": "test-secret-key-1234567890",
		}},
		{"tencentcloud", map[string]string{
			"TENCENTCLOUD_SECRET_ID":  "test-secret-id-1234567890",
			"TENCENTCLOUD_SECRET_KEY": "test-secret-key-1234567890",
		}},
	}

	for _, tc := range cases {
		prv, restore, err := m.dnsProvider(&DNSProviderSpec{Name: tc.name, Env: tc.env})
		if err != nil {
			t.Fatalf("%s: 构造 provider 失败: %v", tc.name, err)
		}
		if prv == nil {
			t.Fatalf("%s: provider 为 nil", tc.name)
		}
		// 包装后仍要满足 lego 的 ProviderTimeout，否则传播等待会退回默认值。
		if _, ok := prv.(challenge.ProviderTimeout); !ok {
			t.Fatalf("%s: 包装后的 provider 未实现 ProviderTimeout", tc.name)
		}
		timeout, interval := prv.(challenge.ProviderTimeout).Timeout()
		if timeout <= 0 || interval <= 0 {
			t.Fatalf("%s: 传播等待参数非法: %v / %v", tc.name, timeout, interval)
		}
		restore()
		for k := range tc.env {
			if _, ok := os.LookupEnv(k); ok {
				t.Fatalf("%s: 凭据 %s 未被清理出进程环境", tc.name, k)
			}
		}
	}

	// 未知 provider 必须报错，并提示应填 lego 的 code。
	if _, restore, err := m.dnsProvider(&DNSProviderSpec{Name: "no-such-provider-xyz"}); err == nil {
		restore()
		t.Fatal("未知 DNS provider 未被拒绝")
	} else if !strings.Contains(err.Error(), "cloudflare") {
		t.Fatalf("错误信息应举例说明 provider code，实际: %v", err)
	}
}

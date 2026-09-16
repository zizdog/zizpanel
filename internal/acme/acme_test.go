package acme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/lego"
)

// ---------------- 域名校验 ----------------

func TestPrepareRejectsBadDomains(t *testing.T) {
	m, _, _ := newTestManager(t)

	bad := [][]string{
		{},                     // 空域名列表
		{""},                   // 空域名
		{"   "},                // 只有空格
		{"localhost"},          // 单标签，不是完整域名
		{"a..b.com"},           // 空标签
		{"-bad.com"},           // 标签以连字符开头
		{"bad-.com"},           // 标签以连字符结尾
		{"bad_domain.com"},     // 下划线
		{"中文.com"},             // IDN 明确不支持
		{"evil.com/../x"},      // 路径字符
		{"evil.com; rm -rf /"}, // 空格与元字符
		{"evil.com\nx"},        // 换行
		{strings.Repeat("a", 60) + ".com", strings.Repeat("b", 64) + ".com"}, // 标签超 63
	}
	for _, domains := range bad {
		if _, err := m.prepare(IssueRequest{Domains: domains, Challenge: ChallengeHTTP01}); err == nil {
			t.Fatalf("非法域名未被拒绝: %q", domains)
		}
	}
}

func TestPrepareAcceptsGoodDomainsAndNormalizes(t *testing.T) {
	m, _, _ := newTestManager(t)

	plan, err := m.prepare(IssueRequest{
		Domains:   []string{"Example.COM.", "www.example.com", "www.example.com"},
		Email:     "a@b.com",
		Challenge: ChallengeHTTP01,
	})
	if err != nil {
		t.Fatalf("合法请求被拒绝: %v", err)
	}
	if plan.primary != "example.com" {
		t.Fatalf("主域名应为 example.com，实际 %q", plan.primary)
	}
	// 应小写、去尾点、去重，且保持顺序。
	want := []string{"example.com", "www.example.com"}
	if strings.Join(plan.domains, ",") != strings.Join(want, ",") {
		t.Fatalf("域名归一化错误: %v", plan.domains)
	}
}

func TestWildcardRequiresDNS01(t *testing.T) {
	m, _, _ := newTestManager(t)

	// http-01 传 wildcard 必须明确报错，且提示改 dns-01。
	_, err := m.prepare(IssueRequest{Domains: []string{"*.example.com"}, Challenge: ChallengeHTTP01})
	if err == nil {
		t.Fatal("http-01 + wildcard 未被拒绝")
	}
	if !strings.Contains(err.Error(), "dns-01") {
		t.Fatalf("错误信息应提示 dns-01，实际: %v", err)
	}

	// dns-01 没给 DNS provider 也要报错。
	if _, err := m.prepare(IssueRequest{Domains: []string{"*.example.com"}, Challenge: ChallengeDNS01}); err == nil {
		t.Fatal("dns-01 缺少 DNS 配置未被拒绝")
	}

	// dns-01 + provider 才放行。
	plan, err := m.prepare(IssueRequest{
		Domains:   []string{"*.example.com", "example.com"},
		Challenge: ChallengeDNS01,
		DNS:       &DNSProviderSpec{Name: "cloudflare", Env: map[string]string{"CF_DNS_API_TOKEN": "x"}},
	})
	if err != nil {
		t.Fatalf("合法 wildcard 请求被拒绝: %v", err)
	}
	if plan.primary != "*.example.com" {
		t.Fatalf("主域名应为 *.example.com，实际 %q", plan.primary)
	}

	// 非通配符域名里混入 "*." 的非法写法要拒绝。
	for _, d := range []string{"*.", "*.*.example.com", "www.*.example.com"} {
		if _, err := m.prepare(IssueRequest{
			Domains:   []string{d},
			Challenge: ChallengeDNS01,
			DNS:       &DNSProviderSpec{Name: "cloudflare"},
		}); err == nil {
			t.Fatalf("非法通配符 %q 未被拒绝", d)
		}
	}
}

func TestPrepareChallengeAndCAErrors(t *testing.T) {
	m, _, _ := newTestManager(t)

	if _, err := m.prepare(IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeType("tls-alpn-01"),
	}); err == nil {
		t.Fatal("未知验证方式未被拒绝")
	}

	if _, err := m.prepare(IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeHTTP01,
		CA:        "letsencrypt_test",
	}); err == nil {
		t.Fatal("未知 CA 未被拒绝")
	}

	// 空 challenge 按 http-01 处理（契约未规定，取最常用值）。
	plan, err := m.prepare(IssueRequest{Domains: []string{"example.com"}})
	if err != nil {
		t.Fatalf("空 challenge 应默认为 http-01: %v", err)
	}
	if plan.challenge != ChallengeHTTP01 {
		t.Fatalf("空 challenge 默认值错误: %q", plan.challenge)
	}
}

func TestPrepareRequiresHTTPWebroot(t *testing.T) {
	root := t.TempDir()
	m := New(root, "", func(string) {})
	if _, err := m.prepare(IssueRequest{Domains: []string{"example.com"}}); err == nil {
		t.Fatal("http-01 缺少网站根目录未被拒绝")
	}
	// dns-01 不依赖网站根目录，应放行。
	if _, err := m.prepare(IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeDNS01,
		DNS:       &DNSProviderSpec{Name: "cloudflare"},
	}); err != nil {
		t.Fatalf("dns-01 不应要求网站根目录: %v", err)
	}
}

// ---------------- CA 选择 ----------------

func TestCADirURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", lego.LEDirectoryProduction},
		{"letsencrypt", lego.LEDirectoryProduction},
		{"LETSENCRYPT-STAGING", lego.LEDirectoryStaging},
		{"letsencrypt-staging", lego.LEDirectoryStaging},
		{"zerossl", zeroSSLDirectoryURL},
		{"ZeroSSL", zeroSSLDirectoryURL},
		{"unknown-ca", ""},
		// 非标准写法必须报未知，不能猜。
		{"Let's Encrypt", ""},
		{"letsencrypt_test", ""},
	}
	for _, tc := range cases {
		if got := CADirURL(tc.in); got != tc.want {
			t.Fatalf("CADirURL(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestStagingFirstDoesNotOverrideExplicitCA 锁死一条重要安全语义：
// StagingFirst 只能影响「用户没选 CA」的情况，绝不能把显式的生产请求降级成 staging。
// 静默降级 = 用户以为拿到正式证书、实际拿到浏览器不信任的测试证书，属于谎报。
func TestStagingFirstDoesNotOverrideExplicitCA(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.StagingFirst = true

	// 显式 letsencrypt → 必须生产。
	plan, err := m.prepare(IssueRequest{Domains: []string{"example.com"}, Challenge: ChallengeHTTP01, CA: "letsencrypt"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.caName != CALetsEncrypt || plan.caURL != lego.LEDirectoryProduction {
		t.Fatalf("显式 letsencrypt 被降级了: name=%q url=%q", plan.caName, plan.caURL)
	}

	// 空 CA → 才允许 staging 优先。
	plan, err = m.prepare(IssueRequest{Domains: []string{"example.com"}, Challenge: ChallengeHTTP01})
	if err != nil {
		t.Fatal(err)
	}
	if plan.caName != CALetsEncryptStaging || plan.caURL != lego.LEDirectoryStaging {
		t.Fatalf("空 CA 未按 StagingFirst 走 staging: name=%q url=%q", plan.caName, plan.caURL)
	}

	// 显式 zerossl 不受影响。
	plan, err = m.prepare(IssueRequest{Domains: []string{"example.com"}, Challenge: ChallengeHTTP01, CA: "zerossl"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.caName != CAZeroSSL || plan.caURL != zeroSSLDirectoryURL {
		t.Fatalf("显式 zerossl 被改写: name=%q url=%q", plan.caName, plan.caURL)
	}
}

// ---------------- NeedsRenewal ----------------

func TestNeedsRenewalBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	days := func(d int) time.Time { return now.Add(time.Duration(d) * 24 * time.Hour) }

	cases := []struct {
		name     string
		notAfter time.Time
		days     int
		want     bool
	}{
		{"剩余 31 天不续期", days(31), 30, false},
		{"剩余 30 天整续期（阈值闭区间）", days(30), 30, true},
		{"剩余 29 天续期", days(29), 30, true},
		{"已过期续期", days(-1), 30, true},
		{"距到期 8 天、阈值 7 天不续", days(8), 7, false},
		{"距到期 7 天、阈值 7 天续", days(7), 7, true},
		{"阈值 0 用默认 30 天(31 天不续)", days(31), 0, false},
		{"阈值 0 用默认 30 天(30 天续)", days(30), 0, true},
		{"阈值负数用默认 30 天", days(29), -5, true},
	}
	for _, tc := range cases {
		c := &Cert{Primary: "example.com", NotAfter: tc.notAfter}
		if got := NeedsRenewal(c, now, tc.days); got != tc.want {
			t.Fatalf("%s: NeedsRenewal=%v，期望 %v", tc.name, got, tc.want)
		}
	}

	// 没有可信到期时间时保守续期。
	if !NeedsRenewal(&Cert{Primary: "x.com"}, now, 30) {
		t.Fatal("NotAfter 为零值时应判定需要续期")
	}
	if !NeedsRenewal(nil, now, 30) {
		t.Fatal("nil 证书应判定需要续期")
	}
}

// ---------------- 路径安全 ----------------

func TestPrimaryRejectsPathTraversal(t *testing.T) {
	m, _, _ := newTestManager(t)
	for _, bad := range []string{"../evil", "a/b", `a\b`, "..", ".", ""} {
		if err := m.Delete(bad); err == nil {
			t.Fatalf("Delete(%q) 应被拒绝", bad)
		}
		if _, err := m.Load(bad); err == nil {
			t.Fatalf("Load(%q) 应被拒绝", bad)
		}
	}
}

func TestMetaPathsAreAbsoluteUnderDataDir(t *testing.T) {
	m, _, root := newTestManager(t)
	m.obtain = (&fakeIssuer{validDays: 90}).obtain

	c, err := m.Issue(t.Context(), IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeHTTP01,
	})
	if err != nil {
		t.Fatal(err)
	}
	certsRoot := filepath.Join(root, "certs")
	for _, p := range []string{c.CertPath, c.KeyPath} {
		if !filepath.IsAbs(p) {
			t.Fatalf("路径应是绝对路径: %s", p)
		}
		if !strings.HasPrefix(p, certsRoot+string(os.PathSeparator)) {
			t.Fatalf("路径应在证书根目录下: %s", p)
		}
	}
}

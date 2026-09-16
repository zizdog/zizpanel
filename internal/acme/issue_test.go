package acme

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-acme/lego/v4/lego"
	legolog "github.com/go-acme/lego/v4/log"
)

// ---------------- 落盘 / 权限 / meta ----------------

func TestIssueSavesCorrectLayoutAndModes(t *testing.T) {
	m, rec, root := newTestManager(t)
	fake := &fakeIssuer{validDays: 90}
	m.obtain = fake.obtain

	c, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"Example.com.", "www.example.com"},
		Email:     "me@example.com",
		Challenge: ChallengeHTTP01,
	})
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}

	if c.Primary != "example.com" {
		t.Fatalf("Primary=%q", c.Primary)
	}
	if c.Challenge != ChallengeHTTP01 || c.CA != CALetsEncrypt {
		t.Fatalf("Challenge/CA 记录错误: %q / %q", c.Challenge, c.CA)
	}
	if c.Issuer == "" {
		t.Fatal("Issuer 不应为空")
	}
	if c.NotBefore.IsZero() || c.NotAfter.IsZero() {
		t.Fatal("有效期不应为零值")
	}
	if c.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt 不应为零值")
	}

	dir := filepath.Join(root, "certs", "example.com")

	// 目录 0700、私钥 0600、证书与 meta 0644 —— 私钥权限是硬要求。
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "privkey.pem"), 0o600)
	assertMode(t, filepath.Join(dir, "fullchain.pem"), 0o644)
	assertMode(t, filepath.Join(dir, "meta.json"), 0o644)
	// renewal.json 含 DNS 凭据（可能），必须 0600。
	assertMode(t, filepath.Join(dir, "renewal.json"), 0o600)

	// 私钥文件内容必须与 CA 返回一致，且是 PEM。
	keyBytes, err := os.ReadFile(filepath.Join(dir, "privkey.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(keyBytes), "PRIVATE KEY") {
		t.Fatal("privkey.pem 不是 PEM 私钥")
	}

	// meta.json 必须能独立读出来。
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Cert
	if err := json.Unmarshal(metaBytes, &got); err != nil {
		t.Fatalf("meta.json 不是合法 JSON: %v", err)
	}
	if got.Primary != "example.com" || len(got.Domains) != 2 {
		t.Fatalf("meta.json 内容错误: %+v", got)
	}

	// 不应留下临时文件。
	assertNoTempFiles(t, dir)

	// 日志应报告保存路径（便于用户定位证书）。
	if !strings.Contains(rec.all(), dir) {
		t.Fatalf("日志未报告保存路径:\n%s", rec.all())
	}
}

func TestIssueOverwritesSamePrimary(t *testing.T) {
	m, _, root := newTestManager(t)

	first := &fakeIssuer{validDays: 10}
	m.obtain = first.obtain
	c1, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeHTTP01,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 同一主域名、不同域名集合：必须正常覆盖而不是报错。
	second := &fakeIssuer{validDays: 90}
	m.obtain = second.obtain
	c2, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"example.com", "www.example.com", "api.example.com"},
		Challenge: ChallengeHTTP01,
	})
	if err != nil {
		t.Fatalf("重复 Issue 覆盖失败: %v", err)
	}

	if !c2.NotAfter.After(c1.NotAfter) {
		t.Fatalf("覆盖后到期时间未更新: %v -> %v", c1.NotAfter, c2.NotAfter)
	}
	if len(c2.Domains) != 3 {
		t.Fatalf("覆盖后域名未更新: %v", c2.Domains)
	}

	// 磁盘上只有一份，且是新的。
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("覆盖后应只有 1 张证书，实际 %d", len(list))
	}
	if len(list[0].Domains) != 3 {
		t.Fatalf("磁盘上的域名未更新: %v", list[0].Domains)
	}
	assertNoTempFiles(t, filepath.Join(root, "certs", "example.com"))
}

// TestIssueKeepsOldCertOnFailure：签发失败时旧证书必须原样保留，
// 否则线上 TLS 会因为一次失败的重签而中断。
func TestIssueKeepsOldCertOnFailure(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.obtain = (&fakeIssuer{validDays: 30}).obtain

	c1, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeHTTP01,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(c1.CertPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile(c1.KeyPath)
	if err != nil {
		t.Fatal(err)
	}

	m.obtain = func(context.Context, issuePlan) (*obtainedCert, error) {
		return nil, errors.New("模拟 CA 失败")
	}
	if _, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeHTTP01,
	}); err == nil {
		t.Fatal("失败的签发不应返回成功")
	}

	after, err := os.ReadFile(c1.CertPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("签发失败后旧证书被改动了")
	}
	keyAfter, err := os.ReadFile(c1.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(keyBefore) != string(keyAfter) {
		t.Fatal("签发失败后旧私钥被改动了")
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].NotAfter.Equal(c1.NotAfter) {
		t.Fatalf("失败后元信息被破坏: %+v", list)
	}
}

func TestListLoadDelete(t *testing.T) {
	m, _, root := newTestManager(t)
	m.obtain = (&fakeIssuer{validDays: 90}).obtain

	for _, d := range []string{"b.example.com", "a.example.com"} {
		if _, err := m.Issue(context.Background(), IssueRequest{Domains: []string{d}, Challenge: ChallengeHTTP01}); err != nil {
			t.Fatal(err)
		}
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List 应返回 2 张，实际 %d", len(list))
	}
	if !sort.SliceIsSorted(list, func(i, j int) bool { return list[i].Primary < list[j].Primary }) {
		t.Fatalf("List 未排序: %v", []string{list[0].Primary, list[1].Primary})
	}

	c, err := m.Load("a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if c.Primary != "a.example.com" {
		t.Fatalf("Load 返回错误对象: %+v", c)
	}

	if _, err := m.Load("nope.example.com"); err == nil {
		t.Fatal("Load 不存在的证书应报错")
	}

	if err := m.Delete("a.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "certs", "a.example.com")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete 未删除目录: %v", err)
	}
	if err := m.Delete("a.example.com"); err == nil {
		t.Fatal("重复 Delete 应报错")
	}
	list, err = m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Primary != "b.example.com" {
		t.Fatalf("Delete 后 List 错误: %+v", list)
	}
}

func TestListEmptyWhenNothingIssued(t *testing.T) {
	m, _, _ := newTestManager(t)
	list, err := m.List()
	if err != nil {
		t.Fatalf("空证书目录不应报错: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("应为空列表，实际 %d", len(list))
	}
}

// ---------------- 续期 ----------------

func TestRenewUsesStoredRequest(t *testing.T) {
	m, _, _ := newTestManager(t)

	first := &fakeIssuer{validDays: 10}
	m.obtain = first.obtain
	c1, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"*.example.com", "example.com"},
		Email:     "me@example.com",
		Challenge: ChallengeDNS01,
		DNS: &DNSProviderSpec{
			Name: "cloudflare",
			Env:  map[string]string{"CF_DNS_API_TOKEN": "cf-token-value-abcdefgh"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c1.Challenge != ChallengeDNS01 {
		t.Fatalf("challenge 记录错误: %q", c1.Challenge)
	}

	second := &fakeIssuer{validDays: 90}
	m.obtain = second.obtain
	c2, err := m.Renew(context.Background(), "*.example.com")
	if err != nil {
		t.Fatalf("Renew 失败: %v", err)
	}
	if !c2.NotAfter.After(c1.NotAfter) {
		t.Fatalf("续期未更新到期时间: %v -> %v", c1.NotAfter, c2.NotAfter)
	}

	plan := second.lastPlanCopy()
	if plan.challenge != ChallengeDNS01 {
		t.Fatalf("续期丢失验证方式: %q", plan.challenge)
	}
	if plan.dns == nil || plan.dns.Name != "cloudflare" {
		t.Fatalf("续期丢失 DNS provider: %+v", plan.dns)
	}
	if plan.dns.Env["CF_DNS_API_TOKEN"] != "cf-token-value-abcdefgh" {
		t.Fatalf("续期丢失 DNS 凭据: %+v", plan.dns.Env)
	}
	if strings.Join(plan.domains, ",") != "*.example.com,example.com" {
		t.Fatalf("续期域名错误: %v", plan.domains)
	}

	if _, err := m.Renew(context.Background(), "not-issued.example.com"); err == nil {
		t.Fatal("续期不存在的证书应报错")
	}
}

func TestRenewWithoutRenewalRecordFailsClearly(t *testing.T) {
	m, _, root := newTestManager(t)
	m.obtain = (&fakeIssuer{validDays: 30}).obtain
	if _, err := m.Issue(context.Background(), IssueRequest{Domains: []string{"example.com"}, Challenge: ChallengeHTTP01}); err != nil {
		t.Fatal(err)
	}
	// 模拟手工拷贝证书目录时漏了 renewal.json。
	if err := os.Remove(filepath.Join(root, "certs", "example.com", "renewal.json")); err != nil {
		t.Fatal(err)
	}
	_, err := m.Renew(context.Background(), "example.com")
	if err == nil {
		t.Fatal("缺少 renewal.json 应报错")
	}
	if !strings.Contains(err.Error(), "重新申请") {
		t.Fatalf("错误信息应引导用户重新申请，实际: %v", err)
	}
}

// ---------------- CA / 生效值 ----------------

func TestIssueRecordsEffectiveCA(t *testing.T) {
	m, _, _ := newTestManager(t)
	fake := &fakeIssuer{validDays: 90}
	m.obtain = fake.obtain

	c, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeHTTP01,
		CA:        "letsencrypt-staging",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.CA != CALetsEncryptStaging {
		t.Fatalf("Cert.CA 未如实记录 staging: %q", c.CA)
	}
	if plan := fake.lastPlanCopy(); plan.caURL != lego.LEDirectoryStaging {
		t.Fatalf("未走 staging directory: %q", plan.caURL)
	}

	// StagingFirst=true 也不能把显式 letsencrypt 降级。
	m2, _, _ := newTestManager(t)
	m2.StagingFirst = true
	fake2 := &fakeIssuer{validDays: 90}
	m2.obtain = fake2.obtain
	c2, err := m2.Issue(context.Background(), IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeHTTP01,
		CA:        "letsencrypt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c2.CA != CALetsEncrypt {
		t.Fatalf("StagingFirst 静默降级了显式生产请求: %q", c2.CA)
	}
	if plan := fake2.lastPlanCopy(); plan.caURL != lego.LEDirectoryProduction {
		t.Fatalf("显式 letsencrypt 未走生产 directory: %q", plan.caURL)
	}
}

// ---------------- 私钥 / token 不进日志 ----------------

func TestSecretsNeverReachLogsOrErrors(t *testing.T) {
	m, rec, _ := newTestManager(t)

	const token = "cf-super-secret-token-0123456789"
	success := &fakeIssuer{validDays: 90}
	m.obtain = success.obtain

	c, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"example.com"},
		Challenge: ChallengeHTTP01,
	})
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := success.keyPEM()
	if len(keyPEM) == 0 {
		t.Fatal("测试假实现未记录私钥")
	}
	// 私钥内容与 PEM 头都不能出现在日志里。
	if strings.Contains(rec.all(), string(keyPEM)) {
		t.Fatal("私钥内容出现在日志里")
	}
	if strings.Contains(rec.all(), "PRIVATE KEY") {
		t.Fatal("日志里出现私钥 PEM 头")
	}
	if strings.Contains(rec.all(), string(mustRead(t, c.KeyPath))) {
		t.Fatal("磁盘私钥内容出现在日志里")
	}

	// 模拟「provider 把 DNS 凭据写进日志和错误」的最坏情况：
	// 注入凭据 → 发一条含 token 的 lego 日志 → 返回含 token 的错误。
	failing := &fakeIssuer{before: func(plan issuePlan) error {
		restore, err := applyDNSEnv(plan.dns)
		if err != nil {
			return err
		}
		defer restore()
		legolog.Infof("provider debug: using token %s", plan.dns.Env["CF_DNS_API_TOKEN"])
		return errors.New("provider rejected token " + plan.dns.Env["CF_DNS_API_TOKEN"])
	}}
	m.obtain = failing.obtain

	_, err = m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"*.example.com"},
		Challenge: ChallengeDNS01,
		DNS:       &DNSProviderSpec{Name: "cloudflare", Env: map[string]string{"CF_DNS_API_TOKEN": token}},
	})
	if err == nil {
		t.Fatal("预期签发失败")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("错误信息泄露了 DNS token: %v", err)
	}
	if strings.Contains(rec.all(), token) {
		t.Fatalf("日志泄露了 DNS token:\n%s", rec.all())
	}
	// 同时确认脱敏留了痕迹，而不是把整条日志吞掉（否则用户看不到失败原因）。
	if !strings.Contains(rec.all(), "***") {
		t.Fatalf("日志应保留脱敏痕迹:\n%s", rec.all())
	}
}

func TestIssueHonorsContextCancellation(t *testing.T) {
	m, _, root := newTestManager(t)
	m.obtain = (&fakeIssuer{validDays: 90}).obtain

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Issue(ctx, IssueRequest{Domains: []string{"example.com"}, Challenge: ChallengeHTTP01}); err == nil {
		t.Fatal("已取消的 context 不应继续签发")
	}
	if _, err := os.Stat(filepath.Join(root, "certs", "example.com")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("取消后不应留下证书目录: %v", err)
	}
}

// ---------------- 小工具 ----------------

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s 权限 = %o，期望 %o", path, got, want)
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

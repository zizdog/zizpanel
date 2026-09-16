package acme

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件锁死用户明确点名的两条硬要求：
//   1. 失败条目会被保存，并出现在（接入层的）列表里（status=failed）；
//   2. 条目文件里不含任何凭据值。

// TestFailedIssueSavesAttemptWithoutCredentials：签发失败必须留下可重试条目，
// 且条目文件里只有 DNS 服务商**名字**，没有 token / SecretKey。
func TestFailedIssueSavesAttemptWithoutCredentials(t *testing.T) {
	m, _, root := newTestManager(t)
	const token = "cf-super-secret-token-0123456789"

	// 走真实路径登记凭据（applyDNSEnv → rememberSecret），再模拟 provider 把 token
	// 写进错误文本：条目里必须只剩 ***。
	failing := &fakeIssuer{before: func(plan issuePlan) error {
		restore, err := applyDNSEnv(plan.dns)
		if err != nil {
			return err
		}
		defer restore()
		return errors.New("provider rejected token " + plan.dns.Env["CLOUDFLARE_DNS_API_TOKEN"])
	}}
	m.obtain = failing.obtain

	_, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"*.example.com", "example.com"},
		Email:     "me@example.com",
		Challenge: ChallengeDNS01,
		CA:        CALetsEncryptStaging,
		DNS:       &DNSProviderSpec{Name: "cloudflare", Env: map[string]string{"CLOUDFLARE_DNS_API_TOKEN": token}},
	})
	if err == nil {
		t.Fatal("预期签发失败")
	}

	a, err := m.LoadAttempt("*.example.com")
	if err != nil {
		t.Fatalf("签发失败后应留下可重试条目: %v", err)
	}
	if a.Primary != "*.example.com" {
		t.Errorf("Primary=%q", a.Primary)
	}
	if strings.Join(a.Domains, ",") != "*.example.com,example.com" {
		t.Errorf("域名列表未保留: %v", a.Domains)
	}
	if a.Email != "me@example.com" {
		t.Errorf("邮箱未保留: %q", a.Email)
	}
	if a.Challenge != ChallengeDNS01 {
		t.Errorf("校验方式未保留: %q", a.Challenge)
	}
	if a.CA != CALetsEncryptStaging {
		t.Errorf("CA 未保留: %q", a.CA)
	}
	if a.DNSProvider != "cloudflare" {
		t.Errorf("DNS 服务商名字未保留: %q", a.DNSProvider)
	}
	if a.Failures != 1 {
		t.Errorf("失败次数应为 1，实际 %d", a.Failures)
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Error("创建/更新时间不应为零值")
	}
	if !strings.Contains(a.LastError, "***") {
		t.Errorf("错误文本应保留脱敏痕迹: %q", a.LastError)
	}
	if strings.Contains(a.LastError, token) {
		t.Fatalf("条目里的错误文本泄露了凭据: %q", a.LastError)
	}
	if strings.Contains(a.LastError, "CLOUDFLARE_DNS_API_TOKEN") {
		t.Errorf("错误文本里不应出现凭据字段名: %q", a.LastError)
	}

	// 直接读原始文件：这是「绝不落盘任何凭据」的硬证据。
	path := m.attemptPath("*.example.com")
	raw := mustRead(t, path)
	if bytes.Contains(raw, []byte(token)) {
		t.Fatalf("条目文件泄露了凭据值:\n%s", raw)
	}
	if bytes.Contains(raw, []byte("CLOUDFLARE_DNS_API_TOKEN")) {
		t.Fatalf("条目文件里不应出现凭据字段名（只允许服务商名字）:\n%s", raw)
	}
	// 条目在 0700 的目录里且自身 0600（安全冗余：它不含凭据，但也没必要放开）。
	assertMode(t, path, 0o600)
	if filepath.Dir(path) != filepath.Join(root, accountsDirName, attemptsDirName) {
		t.Fatalf("条目落盘位置不符合约定: %s", path)
	}

	// 失败不写进证书列表（issues 与 attempts 分开存储，List 只回已签发的）。
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("失败不应写进证书列表: %v", list)
	}
	attempts, err := m.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Primary != "*.example.com" {
		t.Fatalf("Attempts 应列出这条失败记录: %v", attempts)
	}
}

// TestAttemptJSONFieldNames 锁死条目文件的字段名与字段集合：
// 前端按这些名字预填表单，而**任何凭据容器都不允许出现**。
func TestAttemptJSONFieldNames(t *testing.T) {
	a := Attempt{
		Primary:     "example.com",
		Domains:     []string{"example.com"},
		Email:       "me@example.com",
		Challenge:   ChallengeDNS01,
		CA:          CALetsEncrypt,
		DNSProvider: "cloudflare",
		LastError:   "失败: ***",
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"primary", "domains", "email", "challenge", "ca", "dns_provider",
		"created_at", "updated_at", "failures", "last_error",
	}
	if len(got) != len(want) {
		t.Fatalf("Attempt 的 JSON 字段数量变化（多/少了字段）: %v", got)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("Attempt 缺少 JSON 字段 %q（前端按它取值）", k)
		}
	}
	for _, forbidden := range []string{"env", "dns", "token", "secret", "key_pem", "credentials"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("Attempt 不允许有凭据相关字段 %q", forbidden)
		}
	}
}

// TestRepeatedFailuresUpdateSingleAttempt：同一主域名反复失败只更新同一条，
// CreatedAt 保留首次、Failures 递增，不会堆积多份记录。
func TestRepeatedFailuresUpdateSingleAttempt(t *testing.T) {
	m, _, root := newTestManager(t)
	m.obtain = func(context.Context, issuePlan) (*obtainedCert, error) {
		return nil, errors.New("模拟 CA 失败")
	}

	req := IssueRequest{Domains: []string{"example.com"}, Email: "me@example.com", Challenge: ChallengeHTTP01}
	if _, err := m.Issue(context.Background(), req); err == nil {
		t.Fatal("预期第 1 次签发失败")
	}
	first, err := m.LoadAttempt("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Issue(context.Background(), req); err == nil {
		t.Fatal("预期第 2 次签发失败")
	}

	entries, err := os.ReadDir(filepath.Join(root, accountsDirName, attemptsDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("反复失败应只留 1 个条目文件，实际 %d: %v", len(entries), entries)
	}
	again, err := m.LoadAttempt("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if again.Failures != 2 {
		t.Fatalf("失败次数应累加到 2，实际 %d", again.Failures)
	}
	// CreatedAt 保留首次失败时间（同一条记录被更新，而不是新建一条）。
	if !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt 应保留首次失败时间: %v -> %v", first.CreatedAt, again.CreatedAt)
	}
	if again.UpdatedAt.Before(again.CreatedAt) {
		t.Fatalf("UpdatedAt 不应早于 CreatedAt: %v / %v", again.UpdatedAt, again.CreatedAt)
	}
}

// TestSuccessfulIssueClearsAttempt：成功签发后失败条目必须消失（让位给正式证书）。
func TestSuccessfulIssueClearsAttempt(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.obtain = func(context.Context, issuePlan) (*obtainedCert, error) {
		return nil, errors.New("模拟 CA 失败")
	}
	req := IssueRequest{Domains: []string{"example.com"}, Challenge: ChallengeHTTP01}
	if _, err := m.Issue(context.Background(), req); err == nil {
		t.Fatal("预期首次签发失败")
	}
	if _, err := m.LoadAttempt("example.com"); err != nil {
		t.Fatalf("失败后应存在条目: %v", err)
	}

	m.obtain = (&fakeIssuer{validDays: 90}).obtain
	if _, err := m.Issue(context.Background(), req); err != nil {
		t.Fatalf("第二次签发应成功: %v", err)
	}
	if _, err := m.LoadAttempt("example.com"); err == nil {
		t.Fatal("成功后失败条目应被清除（让位给正式证书）")
	}
	attempts, err := m.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("成功后不应再有失败条目: %v", attempts)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Primary != "example.com" {
		t.Fatalf("成功后应只剩正式证书: %v", list)
	}
}

// TestPrepareFailureLeavesRetryableAttempt：连校验都没过（例如 CA 名字写错）也要留条目，
// 让用户不必重新填表。CA 保留用户原始写法，重试时 StagingFirst 等开关仍然生效。
func TestPrepareFailureLeavesRetryableAttempt(t *testing.T) {
	m, _, _ := newTestManager(t)

	_, err := m.Issue(context.Background(), IssueRequest{
		Domains:   []string{"example.com"},
		Email:     "me@example.com",
		Challenge: ChallengeHTTP01,
		CA:        "not-a-ca",
	})
	if err == nil {
		t.Fatal("未知 CA 应报错")
	}
	a, lerr := m.LoadAttempt("example.com")
	if lerr != nil {
		t.Fatalf("校验失败后也应留下可重试条目: %v", lerr)
	}
	if a.CA != "not-a-ca" {
		t.Errorf("应保留用户原始 CA 写法，实际 %q", a.CA)
	}
	if a.Challenge != ChallengeHTTP01 {
		t.Errorf("Challenge=%q", a.Challenge)
	}
}

// TestDeleteAttemptRemovesRecord：条目要能被显式删除（列表里的「删除」用它）。
func TestDeleteAttemptRemovesRecord(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.obtain = func(context.Context, issuePlan) (*obtainedCert, error) {
		return nil, errors.New("模拟 CA 失败")
	}
	if _, err := m.Issue(context.Background(), IssueRequest{
		Domains: []string{"example.com"}, Challenge: ChallengeHTTP01,
	}); err == nil {
		t.Fatal("预期签发失败")
	}
	if err := m.DeleteAttempt("example.com"); err != nil {
		t.Fatalf("删除条目失败: %v", err)
	}
	if _, err := m.LoadAttempt("example.com"); err == nil {
		t.Fatal("删除后不应还能读到条目")
	}
	if err := m.DeleteAttempt("example.com"); err == nil {
		t.Fatal("重复删除应报错")
	}
}

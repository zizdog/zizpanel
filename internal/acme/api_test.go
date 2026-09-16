package acme

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestPublicAPISignatures 用「赋值给函数变量」的方式在编译期锁死对外的函数签名。
// 接入层与前端按这份契约编写；任何人改动签名都会让这里编译失败，
// 而不是等到运行时才发现不匹配。
func TestPublicAPISignatures(t *testing.T) {
	var _ func(dataDir, http01WebRoot string, logf func(string)) *Manager = New
	var _ func(*Manager, context.Context, IssueRequest) (*Cert, error) = (*Manager).Issue
	var _ func(*Manager, context.Context, string) (*Cert, error) = (*Manager).Renew
	var _ func(*Manager) ([]*Cert, error) = (*Manager).List
	var _ func(*Manager, string) (*Cert, error) = (*Manager).Load
	var _ func(*Manager, string) error = (*Manager).Delete
	var _ func(*Cert, time.Time, int) bool = NeedsRenewal
	var _ func(string) string = CADirURL
}

// TestEnumStringsAreStable 锁死前后端约定的枚举字符串，一字不能差。
func TestEnumStringsAreStable(t *testing.T) {
	if string(ChallengeHTTP01) != "http-01" {
		t.Fatalf("ChallengeHTTP01 = %q", ChallengeHTTP01)
	}
	if string(ChallengeDNS01) != "dns-01" {
		t.Fatalf("ChallengeDNS01 = %q", ChallengeDNS01)
	}
	if CALetsEncrypt != "letsencrypt" || CALetsEncryptStaging != "letsencrypt-staging" || CAZeroSSL != "zerossl" {
		t.Fatalf("CA 枚举字符串被改动: %q / %q / %q", CALetsEncrypt, CALetsEncryptStaging, CAZeroSSL)
	}
}

// TestCertJSONFieldNames 锁死 meta.json / 接口返回的字段名。
// 前端按这些名字取值，改名会静默把界面变成空白。
func TestCertJSONFieldNames(t *testing.T) {
	c := Cert{
		Primary:   "example.com",
		Domains:   []string{"example.com"},
		CertPath:  "/data/certs/example.com/fullchain.pem",
		KeyPath:   "/data/certs/example.com/privkey.pem",
		Issuer:    "R11",
		NotBefore: time.Unix(0, 0).UTC(),
		NotAfter:  time.Unix(1, 0).UTC(),
		Challenge: ChallengeHTTP01,
		CA:        CALetsEncrypt,
		UpdatedAt: time.Unix(2, 0).UTC(),
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"primary", "domains", "cert_path", "key_path", "issuer",
		"not_before", "not_after", "challenge", "ca", "updated_at",
	}
	if len(got) != len(want) {
		t.Fatalf("Cert 的 JSON 字段数量变化（少了或多了一个字段）: %v", got)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("Cert 缺少 JSON 字段 %q（前端按它取值）", k)
		}
	}
}

// TestShouldRenewUsesManagerThreshold 确认 Manager.RenewalDays 真的被用上
// （而不是一个没人读的配置项）。
func TestShouldRenewUsesManagerThreshold(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.RenewalDays = 7
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	near := &Cert{Primary: "example.com", NotAfter: now.Add(6 * 24 * time.Hour)}
	if !m.ShouldRenew(near, now) {
		t.Fatal("剩余 6 天、阈值 7 天应判定续期")
	}
	far := &Cert{Primary: "example.com", NotAfter: now.Add(8 * 24 * time.Hour)}
	if m.ShouldRenew(far, now) {
		t.Fatal("剩余 8 天、阈值 7 天不应判定续期")
	}
}

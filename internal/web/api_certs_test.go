package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/acme"
	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tasks"
	"github.com/zizdog/zizpanel/internal/tlsx"
)

// ============================================================================
//  ACME 证书接口的单测
//
//  两条硬约束（与项目纪律一致）：
//   1. **绝不联网、绝不碰真实 CA**：证书引擎是注入的假实现（fakeACME）。
//   2. **绝不调用提权助手、绝不真发 HTTP 请求**：applySite 的三个步骤
//      （写 vhost / reload / 探针）全部替换成可控的假函数。
// ============================================================================

// ---------- 假 ACME 引擎 ----------

type fakeACME struct {
	mu      sync.Mutex
	certs   map[string]*acme.Cert
	order   []string
	issued  []acme.IssueRequest
	renewed []string
	deleted []string

	// 失败申请条目（见 internal/acme/attempt.go）。
	attempts       map[string]*acme.Attempt
	attemptOrder   []string
	deletedAttempt []string

	issueErr error
	renewErr error
}

func newFakeACME() *fakeACME {
	return &fakeACME{certs: map[string]*acme.Cert{}, attempts: map[string]*acme.Attempt{}}
}

func (f *fakeACME) putAttempt(a *acme.Attempt) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.attempts[a.Primary]; !ok {
		f.attemptOrder = append(f.attemptOrder, a.Primary)
	}
	f.attempts[a.Primary] = a
}

func (f *fakeACME) Attempts() ([]*acme.Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*acme.Attempt, 0, len(f.attemptOrder))
	for _, p := range f.attemptOrder {
		out = append(out, f.attempts[p])
	}
	return out, nil
}

func (f *fakeACME) LoadAttempt(primary string) (*acme.Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.attempts[primary]
	if !ok {
		return nil, fmt.Errorf("申请记录 %s 不存在", primary)
	}
	return a, nil
}

func (f *fakeACME) DeleteAttempt(primary string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.attempts, primary)
	f.deletedAttempt = append(f.deletedAttempt, primary)
	return nil
}

func (f *fakeACME) put(c *acme.Cert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.certs[c.Primary]; !ok {
		f.order = append(f.order, c.Primary)
	}
	f.certs[c.Primary] = c
}

func (f *fakeACME) List() ([]*acme.Cert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*acme.Cert, 0, len(f.order))
	for _, p := range f.order {
		out = append(out, f.certs[p])
	}
	return out, nil
}

func (f *fakeACME) Load(primary string) (*acme.Cert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.certs[primary]
	if !ok {
		return nil, fmt.Errorf("证书 %s 不存在", primary)
	}
	return c, nil
}

func (f *fakeACME) Issue(_ context.Context, req acme.IssueRequest) (*acme.Cert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued = append(f.issued, req)
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	c := &acme.Cert{
		Primary:   req.Domains[0],
		Domains:   append([]string(nil), req.Domains...),
		CertPath:  filepath.Join("/tmp", req.Domains[0], "fullchain.pem"),
		KeyPath:   filepath.Join("/tmp", req.Domains[0], "privkey.pem"),
		Issuer:    "Fake CA",
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(90 * 24 * time.Hour),
		Challenge: req.Challenge,
		CA:        orDefault(req.CA, acme.CALetsEncrypt),
		UpdatedAt: time.Now(),
	}
	if _, ok := f.certs[c.Primary]; !ok {
		f.order = append(f.order, c.Primary)
	}
	f.certs[c.Primary] = c
	// 真实引擎成功后会清掉同主域名的失败条目（让位给正式证书），假实现保持一致。
	delete(f.attempts, c.Primary)
	return c, nil
}

func (f *fakeACME) Renew(_ context.Context, primary string) (*acme.Cert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewed = append(f.renewed, primary)
	if f.renewErr != nil {
		return nil, f.renewErr
	}
	c, ok := f.certs[primary]
	if !ok {
		return nil, fmt.Errorf("证书 %s 不存在", primary)
	}
	c.NotAfter = time.Now().Add(90 * 24 * time.Hour)
	c.UpdatedAt = time.Now()
	return c, nil
}

func (f *fakeACME) Delete(primary string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, primary)
	delete(f.certs, primary)
	return nil
}

func (f *fakeACME) snapshot() (issued []acme.IssueRequest, renewed, deleted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]acme.IssueRequest(nil), f.issued...),
		append([]string(nil), f.renewed...),
		append([]string(nil), f.deleted...)
}

// stubSiteApply 把 applySite 的"写 vhost / reload / 复核探针"三步换成假实现。
//
// probeCode 是探针模拟返回的状态码；返回的 *int 记录写 vhost 的次数。
//
// 同时把复核窗口压到毫秒级：生产窗口是 6 秒，单测**不许真睡**那么久
// （探针恒为 404 的用例会一直轮询到窗口耗尽）。
func stubSiteApply(t *testing.T, probeCode string) (*int, *probeCall) {
	t.Helper()
	prevWrite, prevReload, prevProbe := siteWriteVhostFn, siteReloadFn, siteProbeFn
	prevRead, prevDelete := siteReadVhostFn, siteDeleteVhostFn
	prevWait, prevEvery := siteVerifyWait, siteVerifyEvery
	writes := 0
	deletes := 0
	call := &probeCall{}
	siteWriteVhostFn = func(_ *Server, _ context.Context, _, _ string) error {
		writes++
		return nil
	}
	siteReloadFn = func(_ *Server, _ context.Context) error { return nil }
	siteProbeFn = func(_ context.Context, scheme, domain string, port int, path string, _ time.Duration) (string, string, error) {
		call.scheme, call.domain, call.port, call.path = scheme, domain, port, path
		return probeCode, "", nil
	}
	// 回滚要读旧内容/删新建文件：默认让"旧文件不存在"，测试需要时再自行覆盖。
	siteReadVhostFn = func(_ *Server, _ string) ([]byte, error) { return nil, os.ErrNotExist }
	siteDeleteVhostFn = func(_ *Server, _ context.Context, _ string) error {
		deletes++
		return nil
	}
	call.deletes = &deletes
	siteVerifyWait = 120 * time.Millisecond
	siteVerifyEvery = time.Millisecond
	t.Cleanup(func() {
		siteWriteVhostFn, siteReloadFn, siteProbeFn = prevWrite, prevReload, prevProbe
		siteReadVhostFn, siteDeleteVhostFn = prevRead, prevDelete
		siteVerifyWait, siteVerifyEvery = prevWait, prevEvery
	})
	return &writes, call
}

type probeCall struct {
	scheme, domain, path string
	port                 int
	// deletes 记录回滚时删除 vhost 的次数（本次新建的文件才会走删除）。
	deletes *int
}

// seedSite 在沙箱数据库里建一个站点（只落库，不碰 nginx）。
func seedSite(t *testing.T, srv *Server, domain string) *sites.Site {
	t.Helper()
	root := filepath.Join(srv.Cfg.WWWRoot, domain)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	site := &sites.Site{Domain: domain, Root: root, Enabled: true, Rewrite: "none"}
	if err := srv.siteMgr().Create(context.Background(), site); err != nil {
		t.Fatalf("建站点失败: %v", err)
	}
	return site
}

// waitTaskDone 等任务中心的某个任务结束（假引擎是瞬时的，这里只防竞态）。
func waitTaskDone(t *testing.T, srv *Server, id string) *tasks.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tk := srv.Tasks.Get(id); tk != nil && tk.Status() != tasks.StatusRunning {
			return tk
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务 %s 没有在 5 秒内结束", id)
	return nil
}

func taskIDFrom(t *testing.T, body map[string]any) string {
	t.Helper()
	data := apiData(t, body)
	id, _ := data["task_id"].(string)
	if id == "" {
		t.Fatalf("响应里没有 task_id（前端要靠它接任务中心）: %v", body)
	}
	return id
}

// ---------- 列表接口：不泄露私钥 ----------

func TestCertsListExposesMetadataButNeverPrivateKey(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake

	// 私钥内容真实落在文件里，接口无论如何都不能把它带出去。
	keyPath := filepath.Join(srv.Cfg.DataDir, "certs", "api.test", "privkey.pem")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "SUPERSECRET-PRIVATE-KEY-MATERIAL"
	if err := os.WriteFile(keyPath, []byte("-----BEGIN PRIVATE KEY-----\n"+secret+"\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.put(&acme.Cert{
		Primary:   "api.test",
		Domains:   []string{"api.test", "www.api.test"},
		Issuer:    "Fake CA",
		CertPath:  filepath.Join(srv.Cfg.DataDir, "certs", "api.test", "fullchain.pem"),
		KeyPath:   keyPath,
		NotBefore: time.Now().Add(-24 * time.Hour),
		NotAfter:  time.Now().Add(10 * 24 * time.Hour), // 10 天后到期 → 需要续期
		Challenge: acme.ChallengeDNS01,
		CA:        acme.CALetsEncrypt,
		UpdatedAt: time.Now(),
	})

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "GET", "/api/v1/certs", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/certs 返回 %d: %v", res.StatusCode, body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("证书列表泄露了私钥内容")
	}
	data := apiData(t, body)
	list, _ := data["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("应列出 1 张证书，实际 %v", body)
	}
	item, _ := list[0].(map[string]any)
	for _, k := range []string{
		"primary", "domains", "issuer", "not_after", "days_left", "challenge",
		"ca", "needs_renewal", "cert_path", "key_path",
	} {
		if _, has := item[k]; !has {
			t.Errorf("证书视图缺少字段 %q: %v", k, item)
		}
	}
	if item["key_path"] != keyPath {
		t.Errorf("key_path 应为路径本身，得到 %v", item["key_path"])
	}
	if item["needs_renewal"] != true {
		t.Errorf("10 天后到期应标记 needs_renewal=true: %v", item)
	}
	days, _ := item["days_left"].(float64)
	if days < 9 || days > 10 {
		t.Errorf("days_left 应在 9~10 之间，得到 %v", item["days_left"])
	}
	if item["challenge"] != "dns-01" {
		t.Errorf("challenge 字段不对: %v", item["challenge"])
	}
}

// ---------- 申请：走任务中心 + 归一化域名 ----------

func TestCertIssueHTTP01GoesThroughTaskCenter(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs", map[string]any{
		"domains":   []string{"API.Test", "api.test", "www.api.test"},
		"email":     "a@b.c",
		"challenge": "http-01",
		"ca":        "letsencrypt-staging",
	}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("申请证书应返回 202（任务中心），实际 %d: %v", res.StatusCode, body)
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, body))
	if tk.Status() != tasks.StatusSucceeded {
		t.Fatalf("任务应成功，实际 %s: %s", tk.Status(), tk.Meta().Title)
	}

	issued, _, _ := fake.snapshot()
	if len(issued) != 1 {
		t.Fatalf("引擎应收到 1 次 Issue，实际 %d", len(issued))
	}
	got := issued[0].Domains
	if len(got) != 2 || got[0] != "api.test" || got[1] != "www.api.test" {
		t.Fatalf("域名应去重并转小写，实际 %v", got)
	}
	if issued[0].Challenge != acme.ChallengeHTTP01 {
		t.Fatalf("challenge 传递错误: %v", issued[0].Challenge)
	}
	if issued[0].CA != "letsencrypt-staging" {
		t.Fatalf("ca 应原样传给引擎: %v", issued[0].CA)
	}
}

func TestCertIssueWildcardRequiresDNS01(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.acmeOverride = newFakeACME()
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs", map[string]any{
		"domains":   []string{"*.api.test"},
		"challenge": "http-01",
	}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("通配符 + http-01 应被拒，实际 %d: %v", res.StatusCode, body)
	}
	if !strings.Contains(fmt.Sprint(body["msg"]), "DNS-01") {
		t.Fatalf("错误信息要指明只能用 DNS-01: %v", body)
	}
}

func TestCertIssueDNS01StoresCredsServerSideAndNeverEchoes(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake
	cookies := loginTestPanel(t, ts)

	const secret = "cf-super-secret-token-value"
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs", map[string]any{
		"domains":   []string{"*.api.test"},
		"email":     "a@b.c",
		"challenge": "dns-01",
		"ca":        "letsencrypt",
		"dns": map[string]any{
			"name": "cloudflare",
			"env":  map[string]string{"CLOUDFLARE_DNS_API_TOKEN": secret},
		},
	}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("dns-01 申请应 202，实际 %d: %v", res.StatusCode, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), secret) {
		t.Fatal("申请响应回显了 DNS 凭据")
	}
	waitTaskDone(t, srv, taskIDFrom(t, body))

	issued, _, _ := fake.snapshot()
	if len(issued) != 1 || issued[0].DNS == nil || issued[0].DNS.Name != "cloudflare" {
		t.Fatalf("引擎应拿到 DNS 凭据规格: %+v", issued)
	}
	if issued[0].DNS.Env["CLOUDFLARE_DNS_API_TOKEN"] != secret {
		t.Fatal("引擎没有拿到凭据值")
	}

	// 凭据必须真的存在服务端（0600），几天后的自动续期要靠它。
	path := srv.acmeCredsPath()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("凭据文件不存在: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("凭据文件权限应为 0600，实际 %v", fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), secret) {
		t.Fatal("凭据没有真正落盘")
	}

	// 清单接口：只说"已配置"，绝不回显值。
	res, body, _ = doJSON(t, ts, "GET", "/api/v1/certs/dns-providers", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dns-providers 返回 %d: %v", res.StatusCode, body)
	}
	raw, _ = json.Marshal(body)
	if strings.Contains(string(raw), secret) {
		t.Fatal("dns-providers 回显了用户填过的凭据值")
	}
	data := apiData(t, body)
	list, _ := data["list"].([]any)
	if len(list) == 0 {
		t.Fatalf("应返回服务商清单: %v", body)
	}
	var cf map[string]any
	for _, it := range list {
		m, _ := it.(map[string]any)
		if m["name"] == "cloudflare" {
			cf = m
		}
	}
	if cf == nil {
		t.Fatalf("清单里应有 cloudflare: %v", body)
	}
	if cf["configured"] != true {
		t.Errorf("填过凭据后 configured 应为 true: %v", cf)
	}
	fields, _ := cf["fields"].([]any)
	if len(fields) == 0 {
		t.Fatalf("服务商条目必须带 fields（前端据此渲染输入框）: %v", cf)
	}
	f0, _ := fields[0].(map[string]any)
	if f0["key"] == "" || f0["key"] == nil {
		t.Fatalf("fields 里必须有 key: %v", fields)
	}
}

func TestCertIssueRejectsUnknownDNSProviderAndUnknownKey(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.acmeOverride = newFakeACME()
	cookies := loginTestPanel(t, ts)

	base := map[string]any{"domains": []string{"api.test"}, "challenge": "dns-01"}

	bad := map[string]any{"name": "not-a-provider", "env": map[string]string{"X": "y"}}
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs", mergeMap(base, map[string]any{"dns": bad}), cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知服务商应 400，实际 %d: %v", res.StatusCode, body)
	}

	wrongKey := map[string]any{"name": "cloudflare", "env": map[string]string{"CF_Token": "x"}}
	res, body, _ = doJSON(t, ts, "POST", "/api/v1/certs", mergeMap(base, map[string]any{"dns": wrongKey}), cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("服务商不认识的键应 400，实际 %d: %v", res.StatusCode, body)
	}
	if !strings.Contains(fmt.Sprint(body["msg"]), "CF_Token") {
		t.Fatalf("错误里要指名道姓说哪个键不认识: %v", body)
	}
}

func TestCertIssueMissingDNSForDNS01IsRejected(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.acmeOverride = newFakeACME()
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs", map[string]any{
		"domains": []string{"api.test"}, "challenge": "dns-01",
	}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("dns-01 缺少 dns 字段应 400，实际 %d: %v", res.StatusCode, body)
	}
}

func mergeMap(base, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ---------- 续期 / 删除 ----------

func TestCertRenewLaunchesTask(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	fake.put(&acme.Cert{Primary: "api.test", Domains: []string{"api.test"}, NotAfter: time.Now().Add(5 * 24 * time.Hour)})
	srv.acmeOverride = fake
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs/api.test/renew", nil, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("续期应 202，实际 %d: %v", res.StatusCode, body)
	}
	waitTaskDone(t, srv, taskIDFrom(t, body))
	_, renewed, _ := fake.snapshot()
	if len(renewed) != 1 || renewed[0] != "api.test" {
		t.Fatalf("应调用一次 Renew(api.test)，实际 %v", renewed)
	}
}

func TestCertRenewUnknownCertIs404(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.acmeOverride = newFakeACME()
	cookies := loginTestPanel(t, ts)

	res, _, _ := doJSON(t, ts, "POST", "/api/v1/certs/nope.test/renew", nil, cookies)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在的证书应 404，实际 %d", res.StatusCode)
	}
}

// ---------- 失败申请条目：保存 / 列表可区分 / 一键重试 / 可删除 ----------

// seedAttempt 造一条「申请失败、可重试」的条目（dns-01 + cloudflare）。
func seedAttempt(f *fakeACME, primary string) *acme.Attempt {
	a := &acme.Attempt{
		Primary:     primary,
		Domains:     []string{primary, "www." + primary},
		Email:       "ops@example.com",
		Challenge:   acme.ChallengeDNS01,
		CA:          acme.CALetsEncrypt,
		DNSProvider: "cloudflare",
		CreatedAt:   time.Now().Add(-2 * time.Hour),
		UpdatedAt:   time.Now().Add(-time.Hour),
		Failures:    2,
		LastError:   "向 CA 申请证书失败: provider rejected token ***",
	}
	f.putAttempt(a)
	return a
}

// TestCertsListDistinguishesIssuedAndFailed：列表要能区分「已签发」与「失败/待重试」，
// 失败条目带上次错误与时间；同一主域名同时有正式证书时只显示证书行（不重复、不误导）。
func TestCertsListDistinguishesIssuedAndFailed(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake

	fake.put(&acme.Cert{
		Primary: "ok.test", Domains: []string{"ok.test"}, Issuer: "Fake CA",
		CertPath:  filepath.Join(srv.Cfg.DataDir, "certs", "ok.test", "fullchain.pem"),
		KeyPath:   filepath.Join(srv.Cfg.DataDir, "certs", "ok.test", "privkey.pem"),
		NotAfter:  time.Now().Add(60 * 24 * time.Hour),
		Challenge: acme.ChallengeHTTP01, CA: acme.CALetsEncrypt, UpdatedAt: time.Now(),
	})
	seedAttempt(fake, "bad.test")
	// 同主域名既有正式证书又有失败记录：列表只出"已签发"这一条，上次失败作为附加信息。
	seedAttempt(fake, "ok.test")

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "GET", "/api/v1/certs", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/certs 返回 %d: %v", res.StatusCode, body)
	}
	list, _ := apiData(t, body)["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("应列出 2 条（1 已签发 + 1 失败），实际 %v", body)
	}
	by := map[string]map[string]any{}
	for _, it := range list {
		m, _ := it.(map[string]any)
		by[fmt.Sprint(m["primary"])] = m
	}

	issued, ok := by["ok.test"]
	if !ok {
		t.Fatalf("列表里缺少 ok.test: %v", body)
	}
	if issued["status"] != "issued" {
		t.Errorf("已签发证书 status 应为 issued: %v", issued)
	}
	if _, has := issued["days_left"]; !has {
		t.Errorf("已签发证书应带 days_left: %v", issued)
	}
	if s, _ := issued["last_error"].(string); s == "" {
		t.Errorf("同主域名有失败记录时，证书行应带上次错误: %v", issued)
	}

	failed, ok := by["bad.test"]
	if !ok {
		t.Fatalf("失败条目没有出现在列表里: %v", body)
	}
	if failed["status"] != "failed" {
		t.Errorf("失败条目 status 应为 failed: %v", failed)
	}
	if s, _ := failed["last_error"].(string); !strings.Contains(s, "***") {
		t.Errorf("失败条目应带脱敏后的错误文本: %v", failed)
	}
	if failed["last_error_at"] == "" || failed["updated_at"] == "" {
		t.Errorf("失败条目应带最后失败时间: %v", failed)
	}
	if failed["dns_provider"] != "cloudflare" {
		t.Errorf("失败条目应保存 DNS 服务商名字（用于预填）: %v", failed)
	}
	if failed["email"] != "ops@example.com" {
		t.Errorf("失败条目应保存邮箱: %v", failed)
	}
	if failed["challenge"] != "dns-01" || failed["ca"] != acme.CALetsEncrypt {
		t.Errorf("失败条目应保存校验方式与 CA: %v", failed)
	}
	if _, has := failed["days_left"]; has {
		t.Errorf("失败条目不应有 days_left（根本没有到期时间）: %v", failed)
	}
	if _, has := failed["dns"]; has {
		t.Errorf("失败条目不该带 dns 凭据容器: %v", failed)
	}
	for _, k := range []string{"env", "token", "secret", "api_key", "access_key"} {
		if _, has := failed[k]; has {
			t.Errorf("失败条目出现了疑似凭据字段 %q: %v", k, failed)
		}
	}
}

// TestCertRetryReissuesFromSavedAttemptWithoutUserInput：重试必须用保存的条目 +
// 服务端已存凭据重签，请求体为空（用户不重填任何东西），成功后失败条目让位给正式证书。
func TestCertRetryReissuesFromSavedAttemptWithoutUserInput(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake

	const token = "cf-retry-secret-token-0987654321"
	if err := srv.storeDNSEnv("cloudflare", map[string]string{"CLOUDFLARE_DNS_API_TOKEN": token}); err != nil {
		t.Fatalf("预置 DNS 凭据失败: %v", err)
	}
	seedAttempt(fake, "api.test")

	cookies := loginTestPanel(t, ts)
	// 请求体为 nil：用户不需要重填；DNS 凭据也绝不经过浏览器。
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs/api.test/retry", nil, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("重试应返回 202（任务中心），实际 %d: %v", res.StatusCode, body)
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, body))
	if tk.Status() != tasks.StatusSucceeded {
		t.Fatalf("重试任务应成功，实际 %s", tk.Status())
	}
	issued, _, _ := fake.snapshot()
	if len(issued) != 1 {
		t.Fatalf("引擎应收到 1 次 Issue，实际 %d", len(issued))
	}
	got := issued[0]
	if strings.Join(got.Domains, ",") != "api.test,www.api.test" {
		t.Fatalf("重试应沿用保存的域名，实际 %v", got.Domains)
	}
	if got.Challenge != acme.ChallengeDNS01 || got.CA != acme.CALetsEncrypt {
		t.Fatalf("重试应沿用保存的校验方式/CA，实际 %v / %v", got.Challenge, got.CA)
	}
	if got.DNS == nil || got.DNS.Name != "cloudflare" {
		t.Fatalf("重试应带上保存的 DNS 服务商: %+v", got.DNS)
	}
	if got.DNS.Env["CLOUDFLARE_DNS_API_TOKEN"] != token {
		t.Fatal("重试没有用服务端已保存的凭据")
	}

	// 成功后：失败条目让位给正式证书。
	res, body, _ = doJSON(t, ts, "GET", "/api/v1/certs", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("重试后列表返回 %d: %v", res.StatusCode, body)
	}
	list, _ := apiData(t, body)["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("重试成功后应只剩 1 条（正式证书），实际 %v", body)
	}
	item, _ := list[0].(map[string]any)
	if item["status"] != "issued" {
		t.Fatalf("重试成功后该条目应为 issued: %v", item)
	}
}

// TestCertRetryGuards：没有失败记录 → 404；已有正式证书 → 409（不能拿失败条目覆盖它）。
func TestCertRetryGuards(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake
	cookies := loginTestPanel(t, ts)

	res, _, _ := doJSON(t, ts, "POST", "/api/v1/certs/nope.test/retry", nil, cookies)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("没有失败记录时应 404，实际 %d", res.StatusCode)
	}

	fake.put(&acme.Cert{Primary: "done.test", Domains: []string{"done.test"}, NotAfter: time.Now().Add(time.Hour)})
	seedAttempt(fake, "done.test")
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs/done.test/retry", nil, cookies)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("已签发证书的重试应 409，实际 %d: %v", res.StatusCode, body)
	}
}

// TestCertDeleteRemovesFailedAttempt：失败条目必须能删掉，否则会永远留在列表里。
func TestCertDeleteRemovesFailedAttempt(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake
	seedAttempt(fake, "bad.test")
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "DELETE", "/api/v1/certs/bad.test", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("删除失败记录应 200，实际 %d: %v", res.StatusCode, body)
	}
	if _, err := fake.LoadAttempt("bad.test"); err == nil {
		t.Fatal("失败记录没有被删除")
	}
	fake.mu.Lock()
	got := append([]string(nil), fake.deletedAttempt...)
	fake.mu.Unlock()
	if len(got) != 1 || got[0] != "bad.test" {
		t.Fatalf("应调用一次 DeleteAttempt(bad.test)，实际 %v", got)
	}
}

func TestCertDeleteRefusesWhenSiteUsesIt(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	certPath := filepath.Join(srv.Cfg.DataDir, "certs", "api.test", "fullchain.pem")
	fake.put(&acme.Cert{
		Primary: "api.test", Domains: []string{"api.test"},
		CertPath: certPath, KeyPath: filepath.Join(srv.Cfg.DataDir, "certs", "api.test", "privkey.pem"),
		NotAfter: time.Now().Add(60 * 24 * time.Hour),
	})
	srv.acmeOverride = fake

	site := seedSite(t, srv, "api.test")
	site.SSLEnabled = true
	site.SSLCert = certPath
	site.SSLKey = "x"
	site.SSLProvider = "acme"
	if err := srv.siteMgr().Update(context.Background(), site); err != nil {
		t.Fatal(err)
	}

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "DELETE", "/api/v1/certs/api.test", nil, cookies)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("被站点引用时必须拒绝删除（409），实际 %d: %v", res.StatusCode, body)
	}
	if !strings.Contains(fmt.Sprint(body["msg"]), "api.test") {
		t.Fatalf("拒绝理由里要列出正在使用它的站点: %v", body)
	}
	if _, _, deleted := fake.snapshot(); len(deleted) != 0 {
		t.Fatalf("拒绝时不该真的删除，实际 %v", deleted)
	}
}

func TestCertDeleteSucceedsWhenUnused(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	fake.put(&acme.Cert{Primary: "api.test", Domains: []string{"api.test"}, NotAfter: time.Now().Add(60 * 24 * time.Hour)})
	srv.acmeOverride = fake
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "DELETE", "/api/v1/certs/api.test", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("无人引用时应能删除，实际 %d: %v", res.StatusCode, body)
	}
	if _, _, deleted := fake.snapshot(); len(deleted) != 1 {
		t.Fatalf("应调用一次 Delete，实际 %v", deleted)
	}
}

// ---------- 站点侧：provider=acme ----------

func TestSiteSSLACMESharesEngineCertPaths(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake

	// 引擎的证书放在 <DataDir>/certs/<primary>/ 下，站点必须引用**同一份路径**。
	certDir := filepath.Join(srv.Cfg.DataDir, "certs", "api.test")
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")
	if err := tlsx.GenerateSelfSigned(certPath, keyPath, []string{"api.test"}, 90); err != nil {
		t.Fatal(err)
	}
	fake.put(&acme.Cert{
		Primary: "api.test", Domains: []string{"api.test", "www.api.test"},
		CertPath: certPath, KeyPath: keyPath, Issuer: "Fake CA",
		NotBefore: time.Now(), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		Challenge: acme.ChallengeDNS01, CA: acme.CALetsEncrypt,
	})

	seedSite(t, srv, "api.test")
	writes, _ := stubSiteApply(t, "403")
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/sites/api.test/ssl",
		map[string]any{"provider": "acme", "cert_primary": "api.test"}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("绑定 ACME 证书应成功，实际 %d: %v", res.StatusCode, body)
	}
	data := apiData(t, body)
	if data["cert"] != certPath || data["key"] != keyPath {
		t.Fatalf("站点必须直接引用引擎的证书路径（cert=%v key=%v）", data["cert"], data["key"])
	}
	if data["provider_label"] == nil || data["days_left"] == nil {
		t.Fatalf("响应应带 provider_label / days_left: %v", data)
	}

	got, err := srv.siteMgr().Get(context.Background(), "api.test")
	if err != nil {
		t.Fatal(err)
	}
	if !got.SSLEnabled || got.SSLProvider != "acme" || got.SSLCert != certPath || got.SSLKey != keyPath {
		t.Fatalf("站点记录没有写回正确字段: %+v", got)
	}
	if got.SSLExpires == "" {
		t.Fatal("SSLExpires 应从证书 NotAfter 写入")
	}
	if *writes == 0 {
		t.Fatal("绑定证书后必须重建 vhost（applySite）")
	}
}

func TestSiteSSLAcmeMatchesByDomainAlias(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake

	certDir := filepath.Join(srv.Cfg.DataDir, "certs", "api.test")
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")
	if err := tlsx.GenerateSelfSigned(certPath, keyPath, []string{"*.api.test"}, 90); err != nil {
		t.Fatal(err)
	}
	// 通配证书：站点域名是它的 SAN（前端也支持这种预选）。
	fake.put(&acme.Cert{
		Primary: "*.api.test", Domains: []string{"*.api.test"},
		CertPath: certPath, KeyPath: keyPath,
		NotAfter: time.Now().Add(90 * 24 * time.Hour), Challenge: acme.ChallengeDNS01,
	})

	seedSite(t, srv, "app.api.test")
	stubSiteApply(t, "403")
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/sites/app.api.test/ssl",
		map[string]any{"provider": "acme"}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("通配证书应能匹配站点域名，实际 %d: %v", res.StatusCode, body)
	}
}

func TestSiteSSLAcmeWithoutCertGivesGuidance(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.acmeOverride = newFakeACME() // 空证书库
	seedSite(t, srv, "api.test")
	stubSiteApply(t, "403")
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/sites/api.test/ssl",
		map[string]any{"provider": "acme"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("没有可用证书时应 400 并指路，实际 %d: %v", res.StatusCode, body)
	}
	msg := fmt.Sprint(body["msg"])
	if !strings.Contains(msg, "证书") || !strings.Contains(msg, "申请") {
		t.Fatalf("错误信息要告诉用户去证书页申请: %q", msg)
	}
}

// ---------- 站点详情：来源 / 到期 / 剩余天数 ----------

func TestSiteGetExposesSSLMeta(t *testing.T) {
	srv, ts := newTestServer(t)
	certDir := filepath.Join(srv.Cfg.DataDir, "certs", "api.test")
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")
	if err := tlsx.GenerateSelfSigned(certPath, keyPath, []string{"api.test"}, 20); err != nil {
		t.Fatal(err)
	}
	site := seedSite(t, srv, "api.test")
	site.SSLEnabled = true
	site.SSLCert = certPath
	site.SSLKey = keyPath
	site.SSLProvider = "acme"
	if err := srv.siteMgr().Update(context.Background(), site); err != nil {
		t.Fatal(err)
	}

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "GET", "/api/v1/sites/api.test", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("站点详情应 200，实际 %d: %v", res.StatusCode, body)
	}
	data := apiData(t, body)
	ssl, _ := data["ssl"].(map[string]any)
	if ssl == nil {
		t.Fatalf("站点详情必须带 ssl 字段: %v", body)
	}
	if ssl["provider"] != "acme" {
		t.Errorf("ssl.provider 应为 acme: %v", ssl)
	}
	if ssl["provider_label"] == nil || ssl["provider_label"] == "" {
		t.Errorf("ssl.provider_label 应给出中文来源: %v", ssl)
	}
	if ssl["not_after"] == nil || ssl["not_after"] == "" {
		t.Errorf("ssl.not_after 应给出到期时间: %v", ssl)
	}
	days, _ := ssl["days_left"].(float64)
	if days < 18 || days > 20 {
		t.Errorf("ssl.days_left 应在 18~20 之间，实际 %v", ssl["days_left"])
	}
	if ssl["renew_hint"] == nil || !strings.Contains(fmt.Sprint(ssl["renew_hint"]), "到期") {
		t.Errorf("20 天后到期应给出续期提示: %v", ssl)
	}
}

// ---------- applySite：reload 成功 ≠ 配置生效 ----------
//
// 这是另一个代理点名要求的缺陷修复：
// `nginx -s reload` 在配置加载失败时**退出码仍然是 0**，
// 所以必须按真实结果复核，否则面板会报"创建成功"而站点其实 404。

func TestApplySiteFailsWhenProbeSaysVhostNotLoaded(t *testing.T) {
	srv, _ := newTestServer(t)
	writes, _ := stubSiteApply(t, "404") // reload 说成功，但探针落到默认站点

	site := &sites.Site{
		Domain:  "probe.test",
		Root:    filepath.Join(srv.Cfg.WWWRoot, "probe.test"),
		Enabled: true,
		Rewrite: "none",
	}
	if err := os.MkdirAll(site.Root, 0o755); err != nil {
		t.Fatal(err)
	}

	err := srv.applySite(context.Background(), site)
	if err == nil {
		t.Fatal("reload 成功但站点实际 404 时必须判失败（否则就是谎报成功）")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("错误信息要带上真实状态码便于排查: %v", err)
	}
	if !strings.Contains(err.Error(), "error_log") {
		t.Fatalf("错误信息要指向 nginx error_log: %v", err)
	}
	if *writes == 0 {
		t.Fatal("vhost 仍应写好（失败发生在复核阶段）")
	}
}

func TestApplySitePassesWhenProbeSeesVhostRule(t *testing.T) {
	srv, _ := newTestServer(t)
	_, call := stubSiteApply(t, "403")

	site := &sites.Site{
		Domain:  "probe.test",
		Root:    filepath.Join(srv.Cfg.WWWRoot, "probe.test"),
		Enabled: true,
		Rewrite: "none",
	}
	if err := os.MkdirAll(site.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := srv.applySite(context.Background(), site); err != nil {
		t.Fatalf("403 代表站点 vhost 已生效，不该失败: %v", err)
	}
	if call.scheme != "http" || call.port != 80 {
		t.Fatalf("未开 SSL 时探针应走 http:80，实际 %s:%d", call.scheme, call.port)
	}
	if call.path == "" || !strings.HasPrefix(call.path, "/.") {
		t.Fatalf("探针路径应是隐藏文件路径（由 vhost 的 deny 规则判定）: %q", call.path)
	}
}

func TestApplySiteProbesHTTPSWhenSSLEnabled(t *testing.T) {
	srv, _ := newTestServer(t)
	_, call := stubSiteApply(t, "403")

	site := &sites.Site{
		Domain:     "probe.test",
		Root:       filepath.Join(srv.Cfg.WWWRoot, "probe.test"),
		Enabled:    true,
		Rewrite:    "none",
		SSLEnabled: true,
		SSLCert:    "/tmp/x/fullchain.pem",
		SSLKey:     "/tmp/x/privkey.pem",
	}
	if err := os.MkdirAll(site.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := srv.applySite(context.Background(), site); err != nil {
		t.Fatalf("SSL 站点复核应通过: %v", err)
	}
	if call.scheme != "https" || call.port != 443 {
		t.Fatalf("开了 SSL 的站点探针应走 https:443，实际 %s:%d", call.scheme, call.port)
	}
}

// ---------- 自动续期 ----------

func TestRenewDueCertsRenewsOnlyExpiringOnes(t *testing.T) {
	srv, _ := newTestServer(t)
	fake := newFakeACME()
	fake.put(&acme.Cert{Primary: "soon.test", Domains: []string{"soon.test"}, NotAfter: time.Now().Add(10 * 24 * time.Hour)})
	fake.put(&acme.Cert{Primary: "later.test", Domains: []string{"later.test"}, NotAfter: time.Now().Add(80 * 24 * time.Hour)})
	srv.acmeOverride = fake
	stubSiteApply(t, "403")

	if err := srv.renewDueCerts(context.Background()); err != nil {
		t.Fatalf("自动续期不该失败: %v", err)
	}
	_, renewed, _ := fake.snapshot()
	if len(renewed) != 1 || renewed[0] != "soon.test" {
		t.Fatalf("只应续期 30 天内到期的证书，实际 %v", renewed)
	}

	// 结果必须写审计（自动续期没有 HTTP 请求，审计是"失败可见"的兜底）。
	var n int
	if err := srv.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action='cert_renew' AND target='soon.test' AND ok=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("续期成功必须在审计里留下记录，实际 %d 条", n)
	}
}

func TestRenewDueCertsFailureIsVisible(t *testing.T) {
	srv, _ := newTestServer(t)
	fake := newFakeACME()
	fake.put(&acme.Cert{Primary: "soon.test", Domains: []string{"soon.test"}, NotAfter: time.Now().Add(10 * 24 * time.Hour)})
	fake.renewErr = fmt.Errorf("CA 拒绝了请求")
	srv.acmeOverride = fake
	stubSiteApply(t, "403")

	err := srv.renewDueCerts(context.Background())
	if err == nil {
		t.Fatal("续期失败必须返回 error（不能静默）")
	}
	if !strings.Contains(err.Error(), "CA 拒绝了请求") {
		t.Fatalf("错误要带上真正的原因: %v", err)
	}
	var n int
	if err := srv.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action='cert_renew' AND target='soon.test' AND ok=0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("续期失败必须留下失败审计，实际 %d 条", n)
	}
	// 失败信息也要进任务中心（用户在界面上看得到）。
	found := false
	for _, m := range srv.Tasks.List() {
		if m.Kind == "cert-renew" && m.Target == "cert:soon.test" {
			found = true
		}
	}
	if !found {
		t.Fatal("自动续期应出现在任务中心里")
	}
}

func TestRenewDueCertsReloadsReferencingSites(t *testing.T) {
	srv, _ := newTestServer(t)
	fake := newFakeACME()
	certDir := filepath.Join(srv.Cfg.DataDir, "certs", "api.test")
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")
	if err := tlsx.GenerateSelfSigned(certPath, keyPath, []string{"api.test"}, 5); err != nil {
		t.Fatal(err)
	}
	fake.put(&acme.Cert{
		Primary: "api.test", Domains: []string{"api.test"},
		CertPath: certPath, KeyPath: keyPath, NotAfter: time.Now().Add(5 * 24 * time.Hour),
	})
	srv.acmeOverride = fake

	// 站点引用了这份证书 → 续期后必须重建它的 vhost。
	site := seedSite(t, srv, "api.test")
	site.SSLEnabled = true
	site.SSLCert = certPath
	site.SSLKey = keyPath
	site.SSLProvider = "acme"
	if err := srv.siteMgr().Update(context.Background(), site); err != nil {
		t.Fatal(err)
	}
	writes, _ := stubSiteApply(t, "403")

	if err := srv.renewDueCerts(context.Background()); err != nil {
		t.Fatalf("自动续期不该失败: %v", err)
	}
	_, renewed, _ := fake.snapshot()
	if len(renewed) != 1 {
		t.Fatalf("应续期 api.test，实际 %v", renewed)
	}
	if *writes == 0 {
		t.Fatal("引用了该证书的站点必须在续期后重建 vhost（否则 nginx 还用旧证书）")
	}
}

// ---------- ZeroSSL 的 EAB（External Account Binding） ----------

func TestCertIssueZeroSSLStoresEABAndNeverEchoes(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake
	cookies := loginTestPanel(t, ts)

	const kid = "eab-kid-abc123"
	const hmac = "eab-hmac-SUPER-SECRET-VALUE"

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs", map[string]any{
		"domains":   []string{"api.test"},
		"email":     "a@b.c",
		"challenge": "http-01",
		"ca":        "zerossl",
		"eab_kid":   kid,
		"eab_hmac":  hmac,
	}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("ZeroSSL 申请应 202，实际 %d: %v", res.StatusCode, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), hmac) || strings.Contains(string(raw), kid) {
		t.Fatal("申请响应回显了 EAB 凭据")
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, body))

	// 任务步骤里也不能出现 EAB（任务日志会被用户看到并可能被复制出来）。
	lines, _, _, _ := tk.Snapshot(0, 4000)
	var sb strings.Builder
	for _, ln := range lines {
		sb.WriteString(ln.Text)
		sb.WriteString("\n")
	}
	if strings.Contains(sb.String(), hmac) || strings.Contains(sb.String(), kid) {
		t.Fatalf("任务日志泄露了 EAB 凭据:\n%s", sb.String())
	}

	issued, _, _ := fake.snapshot()
	if len(issued) != 1 || issued[0].CA != "zerossl" {
		t.Fatalf("CA 应原样传给引擎: %+v", issued)
	}

	// 凭据必须落到服务端 0600 文件里（续期还要用）。
	path := srv.acmeCredsPath()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("凭据文件不存在: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("凭据文件权限应为 0600，实际 %v", fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), hmac) || !strings.Contains(string(b), kid) {
		t.Fatal("EAB 凭据没有真正落盘")
	}

	// 列表接口：只回 has_eab，不回值。
	res, body, _ = doJSON(t, ts, "GET", "/api/v1/certs", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/certs 返回 %d: %v", res.StatusCode, body)
	}
	raw, _ = json.Marshal(body)
	if strings.Contains(string(raw), hmac) || strings.Contains(string(raw), kid) {
		t.Fatal("证书列表回显了 EAB 凭据")
	}
	data := apiData(t, body)
	if data["has_eab"] != true {
		t.Fatalf("已保存 EAB 时应返回 has_eab=true: %v", data)
	}
	eabObj, _ := data["eab"].(map[string]any)
	if eabObj == nil || eabObj["has_eab"] != true {
		t.Fatalf("eab 对象形状不对: %v", data["eab"])
	}
	if _, leaked := eabObj["kid"]; leaked {
		t.Fatalf("eab 对象不得包含凭据字段: %v", eabObj)
	}
}

func TestCertIssueEABIgnoredForOtherCAs(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	srv.acmeOverride = fake
	cookies := loginTestPanel(t, ts)

	const hmac = "should-not-be-stored-anywhere"
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs", map[string]any{
		"domains":   []string{"api.test"},
		"challenge": "http-01",
		"ca":        "letsencrypt",
		"eab_kid":   "some-kid",
		"eab_hmac":  hmac,
	}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("非 ZeroSSL 带 EAB 字段应被忽略而不是报错，实际 %d: %v", res.StatusCode, body)
	}
	waitTaskDone(t, srv, taskIDFrom(t, body))

	eab, err := srv.loadEAB(acme.CAZeroSSL)
	if err != nil {
		t.Fatal(err)
	}
	if eab.Kid != "" || eab.HMAC != "" {
		t.Fatalf("非 ZeroSSL 的申请不该保存 EAB: %+v", eab)
	}
	if b, err := os.ReadFile(srv.acmeCredsPath()); err == nil && strings.Contains(string(b), hmac) {
		t.Fatal("非 ZeroSSL 的 EAB 值被落盘了")
	}
}

func TestCertIssueZeroSSLPartialEABIsRejected(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.acmeOverride = newFakeACME()
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs", map[string]any{
		"domains":   []string{"api.test"},
		"challenge": "http-01",
		"ca":        "zerossl",
		"eab_kid":   "only-kid",
	}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("只填一半 EAB 应 400，实际 %d: %v", res.StatusCode, body)
	}
}

// ZeroSSL 缺 EAB 时的错误由**引擎**给出（面板不预判、也不回落到别的 CA）。
// 这里只验证：手动续期能提前给出可读指引，而不是一个含糊的失败。
func TestCertRenewZeroSSLWithoutEABIsActionable(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	fake.put(&acme.Cert{
		Primary: "api.test", Domains: []string{"api.test"},
		CA: acme.CAZeroSSL, NotAfter: time.Now().Add(5 * 24 * time.Hour),
	})
	srv.acmeOverride = nil // 走真实的"是否有 EAB"判断
	srv.acmeMgr = fake

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/certs/api.test/renew", nil, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("ZeroSSL 缺 EAB 续期应 400，实际 %d: %v", res.StatusCode, body)
	}
	msg := fmt.Sprint(body["msg"])
	if !strings.Contains(msg, "EAB") {
		t.Fatalf("错误信息要说明需要 EAB: %q", msg)
	}
	if _, renewed, _ := fake.snapshot(); len(renewed) != 0 {
		t.Fatal("缺 EAB 时不该尝试续期")
	}
}

func TestCertManagerForRenewInjectsStoredEABForZeroSSL(t *testing.T) {
	srv, _ := newTestServer(t)
	cert := &acme.Cert{Primary: "api.test", Domains: []string{"api.test"}, CA: acme.CAZeroSSL}

	// 没存 EAB：必须在开始续期之前就给出可读的指引。
	if _, err := srv.certManagerForRenew(cert); err == nil {
		t.Fatal("ZeroSSL 证书没有 EAB 时应返回明确错误")
	} else if !strings.Contains(err.Error(), "EAB") {
		t.Fatalf("错误信息要说明缺 EAB: %v", err)
	}

	// 存了 EAB：返回的引擎必须真的带上它（否则 ZeroSSL 注册一定失败）。
	if err := srv.storeEAB(acme.CAZeroSSL, "kid-9", "hmac-9"); err != nil {
		t.Fatal(err)
	}
	mgr, err := srv.certManagerForRenew(cert)
	if err != nil {
		t.Fatalf("有 EAB 时应能续期: %v", err)
	}
	real, ok := mgr.(*acme.Manager)
	if !ok {
		t.Fatalf("应返回真实引擎，实际 %T", mgr)
	}
	if real.EABKid != "kid-9" || real.EABHmacKey != "hmac-9" {
		t.Fatalf("续期引擎没有注入 EAB: kid=%q hmac=%q", real.EABKid, real.EABHmacKey)
	}

	// 非 ZeroSSL：用共享引擎，不该因为"没配 EAB"而报错。
	other, err := srv.certManagerForRenew(&acme.Cert{Primary: "x.test", CA: acme.CALetsEncrypt})
	if err != nil {
		t.Fatalf("Let's Encrypt 证书续期不该要求 EAB: %v", err)
	}
	if other != srv.certManager() {
		t.Fatal("非 ZeroSSL 应复用共享引擎实例")
	}
}

// EAB 必须真的被设到引擎上（否则 ZeroSSL 注册一定失败）。
func TestCertManagerWithCredsSetsEABOnEngine(t *testing.T) {
	srv, _ := newTestServer(t)

	mgr := srv.certManagerWithCreds("kid-1", "hmac-1")
	real, ok := mgr.(*acme.Manager)
	if !ok {
		t.Fatalf("没有注入假实现时应返回真实引擎，实际 %T", mgr)
	}
	if real.EABKid != "kid-1" || real.EABHmacKey != "hmac-1" {
		t.Fatalf("EAB 没有设到引擎上: kid=%q hmac=%q", real.EABKid, real.EABHmacKey)
	}
	if real.RenewalDays != certRenewThresholdDays {
		t.Fatalf("续期阈值应与面板一致，实际 %d", real.RenewalDays)
	}
	// 不传凭据时走共享实例（不重复构造）。
	if srv.certManagerWithCreds("", "") != srv.certManager() {
		t.Fatal("没有 EAB 时应复用共享引擎实例")
	}
}

// ---------- HTTP-01 目录准备 ----------

func TestPrepareHTTP01WebRootLinksSiteRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	site := seedSite(t, srv, "api.test")

	notes := srv.prepareHTTP01WebRoot([]string{"api.test"})
	challengeDir := filepath.Join(srv.acmeHTTP01WebRoot(), ".well-known", "acme-challenge")
	if fi, err := os.Stat(challengeDir); err != nil || !fi.IsDir() {
		t.Fatalf("HTTP-01 挑战目录没有建出来: %v", err)
	}
	link := filepath.Join(site.Root, ".well-known")
	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("站点根下应建立 .well-known 软链: %v（notes=%v）", err, notes)
	}
	if filepath.Clean(dest) != filepath.Clean(filepath.Join(srv.acmeHTTP01WebRoot(), ".well-known")) {
		t.Fatalf("软链指向不对: %s", dest)
	}

	// 用户自己放的真实目录不能被动：只给提示，不删不改。
	other := seedSite(t, srv, "manual.test")
	real := filepath.Join(other.Root, ".well-known")
	if err := os.MkdirAll(filepath.Join(real, "acme-challenge"), 0o755); err != nil {
		t.Fatal(err)
	}
	notes = srv.prepareHTTP01WebRoot([]string{"manual.test"})
	if _, err := os.Lstat(real); err != nil {
		t.Fatalf("真实目录不该被删除: %v", err)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "真实目录") {
		t.Fatalf("遇到真实目录要给出提示: %v", notes)
	}
}

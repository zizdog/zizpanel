package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/acme"
	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/tlsx"
)

// ============================================================================
//  反向代理 HTTPS 的接口层测试
//
//  覆盖四件事：
//    1. manual / acme 两种来源真的把证书绑上，并生成 `listen ... ssl` 的 vhost；
//    2. acme 引用的是**面板证书库路径**（不复制副本）；
//    3. 校验失败（缺私钥 / 未知来源）返回 400；
//    4. 请求级复核失败（TLS 握手取不到证书）返回非 2xx，且数据库不被改成"已开启"。
//
//  全部跑在 newTestServer 沙箱里：不碰真实 nginx、不碰用户家目录。
// ============================================================================

// seedProxyNginx 在沙箱 brew 前缀里造一个假的 nginx 可执行文件。
//
// 没有它时 nginxInstalled() 为 false，所有 SSL 接口会先被
// "请先安装 nginx" 挡住，测不到真正的证书逻辑。
//
// ⚠️ 必须显式改写 Cfg.NginxBin：newTestServer 只重算了 BrewPrefix，
// 而 config.Default() 已经把 NginxBin 指向**真机** /opt/homebrew/bin/nginx。
// 直接往那个路径写会覆盖用户真实的 nginx 二进制（第一次写这段测试时就撞上了
// "permission denied" 才没造成后果）。
func seedProxyNginx(t *testing.T, srv *Server) {
	t.Helper()
	bin := filepath.Join(srv.Cfg.BrewPrefix, "bin", "nginx")
	if real := "/opt/homebrew"; strings.HasPrefix(bin, real) {
		t.Fatalf("拒绝把假 nginx 写到真实安装路径：%s", bin)
	}
	srv.Cfg.NginxBin = bin
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedProxyRule 在沙箱数据库里插入一条规则（只落库，不碰 nginx）。
func seedProxyRule(t *testing.T, srv *Server, rule *proxies.Rule) *proxies.Rule {
	t.Helper()
	if rule.Name == "" {
		rule.Name = "测试规则"
	}
	if rule.Target == "" {
		rule.Target = "http://127.0.0.1:9"
	}
	created, err := srv.proxyRepo().Create(context.Background(), rule)
	if err != nil {
		t.Fatalf("建反代规则失败: %v", err)
	}
	return created
}

// stubProxyTLS 替换"取回对端证书"这一步。
func stubProxyTLS(t *testing.T, info tlsPeerInfo, err error) {
	t.Helper()
	prev := proxyTLSPeerFn
	proxyTLSPeerFn = func(context.Context, int, string, time.Duration) (tlsPeerInfo, error) {
		return info, err
	}
	t.Cleanup(func() { proxyTLSPeerFn = prev })
}

// captureProxyVhost 让"写 vhost"只记录内容，并让探测"长出一条访问日志"
// 以便通过既有的 access_log 增长复核。返回记录用的指针。
func captureProxyVhost(t *testing.T, srv *Server, rule *proxies.Rule) (*string, *string) {
	t.Helper()
	var name, content string
	proxyWriteVhostFn = func(_ *Server, _ context.Context, n, c string) error {
		name, content = n, c
		return nil
	}
	logPath := proxyAccessLogPath(srv.proxyLogDir(), rule.ID)
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		appendToFile(t, logPath)
		return "200", "upstream-ok", nil
	}
	return &name, &content
}

// makeSeedCert 生成一份真实的自签证书，返回 (certPEM, keyPEM, NotAfter)。
//
// 用真实 PEM 而不是假字符串：manual 分支会立刻解析证书，
// 假内容只会走到"证书无法解析"的错误分支，测不到成功路径。
func makeSeedCert(t *testing.T, dir string, hosts []string) ([]byte, []byte, time.Time) {
	t.Helper()
	certFile := filepath.Join(dir, "fullchain.pem")
	keyFile := filepath.Join(dir, "privkey.pem")
	if err := tlsx.GenerateSelfSigned(certFile, keyFile, hosts, 90); err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	notAfter, err := tlsx.CertExpiry(certFile)
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM, notAfter
}

// TestProxySSLManualBindsAndGeneratesSSLListen：手工来源的完整成功路径。
func TestProxySSLManualBindsAndGeneratesSSLListen(t *testing.T) {
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	stubProxyHooks(t)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "手工证书规则", Listen: 18443, Target: "http://127.0.0.1:9", Enabled: true,
	})
	name, content := captureProxyVhost(t, srv, rule)
	certPEM, keyPEM, notAfter := makeSeedCert(t, filepath.Join(srv.Cfg.DataDir, "seed"), []string{"a.test"})
	stubProxyTLS(t, tlsPeerInfo{NotAfter: notAfter, DNSNames: []string{"a.test"}}, nil)

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "POST", fmt.Sprintf("/api/v1/proxies/%d/ssl", rule.ID),
		map[string]any{"provider": "manual", "cert": string(certPEM), "key": string(keyPEM)}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("手工绑定证书应成功，实际 %d: %v", res.StatusCode, body)
	}
	data := apiData(t, body)
	wantCert := filepath.Join(srv.Cfg.DataDir, "proxy-certs", rule.VhostName(), "fullchain.pem")
	wantKey := filepath.Join(srv.Cfg.DataDir, "proxy-certs", rule.VhostName(), "privkey.pem")
	if data["cert"] != wantCert || data["key"] != wantKey {
		t.Fatalf("self/mkcert/manual 应落到 <DataDir>/proxy-certs/<规则>/，实际 cert=%v key=%v", data["cert"], data["key"])
	}
	if _, err := os.Stat(wantCert); err != nil {
		t.Fatalf("证书文件没有落盘: %v", err)
	}
	// 私钥仍然是 0600（不是全局可读）
	if fi, err := os.Stat(wantKey); err != nil {
		t.Fatalf("私钥没有落盘: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("私钥权限应为 0600，实际 %v", fi.Mode().Perm())
	}

	if *name != rule.VhostName() {
		t.Fatalf("写了错误的 vhost 文件: %q", *name)
	}
	for _, want := range []string{
		"listen      18443 ssl;",
		"ssl_certificate     " + wantCert + ";",
		"ssl_certificate_key " + wantKey + ";",
		"proxy_pass http://127.0.0.1:9;",
	} {
		if !strings.Contains(*content, want) {
			t.Errorf("生成的 vhost 缺少 %q：\n%s", want, *content)
		}
	}

	got, err := srv.proxyRepo().Get(context.Background(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SSLEnabled || got.SSLProvider != "manual" || got.SSLCert != wantCert || got.SSLKey != wantKey {
		t.Fatalf("规则记录没写回正确的 SSL 字段: %+v", got)
	}
	if got.SSLExpires == "" {
		t.Fatal("SSLExpires 应从证书 NotAfter 写入")
	}
	if data["days_left"] == nil || data["provider_label"] == nil {
		t.Fatalf("响应应带 days_left / provider_label: %v", data)
	}
}

// TestProxySSLACMEReferencesEnginePaths：acme 必须直接引用面板证书库路径。
func TestProxySSLACMEReferencesEnginePaths(t *testing.T) {
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	stubProxyHooks(t)
	fake := newFakeACME()
	srv.acmeOverride = fake

	certDir := filepath.Join(srv.Cfg.DataDir, "certs", "api.test")
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")
	if err := tlsx.GenerateSelfSigned(certPath, keyPath, []string{"api.test"}, 90); err != nil {
		t.Fatal(err)
	}
	notAfter, _ := tlsx.CertExpiry(certPath)
	fake.put(&acme.Cert{
		Primary: "api.test", Domains: []string{"api.test", "www.api.test"},
		CertPath: certPath, KeyPath: keyPath, Issuer: "Fake CA",
		NotBefore: time.Now(), NotAfter: notAfter,
		Challenge: acme.ChallengeDNS01, CA: acme.CALetsEncrypt,
	})

	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "acme 规则", Listen: 18447, Target: "http://127.0.0.1:9", Enabled: true,
	})
	_, content := captureProxyVhost(t, srv, rule)
	stubProxyTLS(t, tlsPeerInfo{NotAfter: notAfter, DNSNames: []string{"api.test"}}, nil)

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "POST", fmt.Sprintf("/api/v1/proxies/%d/ssl", rule.ID),
		map[string]any{"provider": "acme", "cert_primary": "api.test"}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("绑定 ACME 证书应成功，实际 %d: %v", res.StatusCode, body)
	}
	data := apiData(t, body)
	if data["cert"] != certPath || data["key"] != keyPath {
		t.Fatalf("acme 必须引用证书库原路径（cert=%v key=%v）", data["cert"], data["key"])
	}
	if !strings.Contains(*content, "ssl_certificate     "+certPath+";") {
		t.Errorf("vhost 里应写着证书库路径：\n%s", *content)
	}
	// 续期是同路径覆盖，所以绝不能复制到 proxy-certs 下。
	if strings.Contains(*content, "proxy-certs") {
		t.Errorf("acme 来源不该出现 proxy-certs 副本：\n%s", *content)
	}
}

// TestProxySSLValidationRejects：可预期的输入错误必须 400 并说清缺什么。
func TestProxySSLValidationRejects(t *testing.T) {
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	stubProxyHooks(t)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "校验规则", Listen: 18448, Target: "http://127.0.0.1:9", Enabled: true,
	})
	cookies := loginTestPanel(t, ts)
	path := fmt.Sprintf("/api/v1/proxies/%d/ssl", rule.ID)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"未知来源", map[string]any{"provider": "bogus"}, "不支持的证书来源"},
		{"手工缺私钥", map[string]any{"provider": "manual", "cert": "x"}, "证书与私钥"},
		{"手工全空", map[string]any{"provider": "manual"}, "证书与私钥"},
		{"acme 库里没有证书", map[string]any{"provider": "acme", "cert_primary": "none.test"}, "还没有任何 ACME 证书"},
	}
	for _, c := range cases {
		res, body, _ := doJSON(t, ts, "POST", path, c.body, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s：应返回 400，实际 %d: %v", c.name, res.StatusCode, body)
			continue
		}
		msg := fmt.Sprint(body["msg"])
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s：错误信息应包含 %q，实际 %q", c.name, c.want, msg)
		}
	}
	// 全部被拒后数据库里这条规则必须仍然没有 SSL
	got, _ := srv.proxyRepo().Get(context.Background(), rule.ID)
	if got.SSLEnabled {
		t.Fatal("校验失败时不该把规则改成已开启 SSL")
	}
}

// TestProxySSLVerifyFailureIsReportedAndNotSaved：TLS 复核失败必须非 2xx 且不落库。
func TestProxySSLVerifyFailureIsReportedAndNotSaved(t *testing.T) {
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	stubProxyHooks(t)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "复核失败规则", Listen: 18449, Target: "http://127.0.0.1:9", Enabled: true,
	})
	captureProxyVhost(t, srv, rule) // 写盘与访问日志都"成功"，只有 TLS 握手失败
	certPEM, keyPEM, _ := makeSeedCert(t, filepath.Join(srv.Cfg.DataDir, "seed2"), []string{"b.test"})
	stubProxyTLS(t, tlsPeerInfo{}, errors.New("dial tcp 127.0.0.1:18449: connection refused"))

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "POST", fmt.Sprintf("/api/v1/proxies/%d/ssl", rule.ID),
		map[string]any{"provider": "manual", "cert": string(certPEM), "key": string(keyPEM)}, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("TLS 复核失败时必须返回非 2xx，实际 200: %v", body)
	}
	msg := fmt.Sprint(body["msg"])
	if !strings.Contains(msg, "HTTPS 复核失败") && !strings.Contains(msg, "TLS 握手") {
		t.Fatalf("错误信息应说明 TLS 复核失败，实际：%s", msg)
	}
	got, _ := srv.proxyRepo().Get(context.Background(), rule.ID)
	if got.SSLEnabled {
		t.Fatal("复核失败时不该把规则落库成已开启 SSL（那正是谎报成功）")
	}
}

// TestVerifyProxyTLSServedDetectsWrongCert：端口上端出来的不是配置里那份证书 → 必须失败。
func TestVerifyProxyTLSServedDetectsWrongCert(t *testing.T) {
	srv := newProxyTestServer(t)
	seedDir := filepath.Join(srv.Cfg.DataDir, "seed3")
	_, _, notAfter := makeSeedCert(t, seedDir, []string{"c.test"})
	rule := &proxies.Rule{
		ID: 1, Name: "错证书规则", Listen: 18451, Target: "http://127.0.0.1:9", Enabled: true,
		SSLEnabled: true,
		SSLCert:    filepath.Join(seedDir, "fullchain.pem"),
		SSLKey:     filepath.Join(seedDir, "privkey.pem"),
	}

	// 对端端出的是"另一张到期时间不同"的证书 → 说明这份 ssl_certificate 没生效。
	stubProxyTLS(t, tlsPeerInfo{NotAfter: notAfter.Add(24 * time.Hour)}, nil)
	err := srv.verifyProxyTLSServed(context.Background(), rule)
	if err == nil {
		t.Fatal("对端证书与配置不一致时必须失败")
	}
	if !strings.Contains(err.Error(), "没有生效") {
		t.Errorf("错误信息应指出证书没有生效，实际：%v", err)
	}

	// 一致时通过。
	stubProxyTLS(t, tlsPeerInfo{NotAfter: notAfter}, nil)
	if err := srv.verifyProxyTLSServed(context.Background(), rule); err != nil {
		t.Fatalf("证书一致时不该报错：%v", err)
	}

	// 配置里的证书文件不存在 → 明确报"读不到"，不要含糊。
	broken := *rule
	broken.SSLCert = filepath.Join(seedDir, "missing.pem")
	if err := srv.verifyProxyTLSServed(context.Background(), &broken); err == nil ||
		!strings.Contains(err.Error(), "读不到") {
		t.Errorf("证书文件缺失时应报读不到，实际：%v", err)
	}
}

// TestCheckProxySSLPortMix：同一端口不能 HTTP/HTTPS 混用；80 端口不能开 HTTPS。
func TestCheckProxySSLPortMix(t *testing.T) {
	srv := newProxyTestServer(t)
	seedProxyRule(t, srv, &proxies.Rule{
		Name: "已有 HTTP 规则", Listen: 18450, Target: "http://127.0.0.1:9", Enabled: true,
	})
	ctx := context.Background()

	sslRule := &proxies.Rule{
		Name: "新 HTTPS 规则", Listen: 18450, Target: "http://127.0.0.1:9", Enabled: true,
		SSLEnabled: true, SSLCert: "/c/f.pem", SSLKey: "/c/k.pem",
	}
	err := srv.checkProxySSLPortMix(ctx, sslRule)
	if err == nil {
		t.Fatal("同端口 HTTP/HTTPS 混用必须被拒绝")
	}
	for _, want := range []string{"不能同时跑", "已有 HTTP 规则"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("拒绝理由应包含 %q，实际：%v", want, err)
		}
	}

	// 端口 80 上的 HTTPS 会与默认站点的 default_server 打架 → 直接拒绝
	port80 := &proxies.Rule{
		Name: "80 上的 HTTPS", Listen: 80, Target: "http://127.0.0.1:9", Enabled: true,
		SSLEnabled: true, SSLCert: "/c/f.pem", SSLKey: "/c/k.pem",
	}
	if err := srv.checkProxySSLPortMix(ctx, port80); err == nil || !strings.Contains(err.Error(), "80") {
		t.Errorf("80 端口开 HTTPS 必须被拒绝并提到 80，实际：%v", err)
	}

	// 停用的规则不写配置，不该拦
	disabled := &proxies.Rule{
		Name: "停用的 HTTPS", Listen: 18450, Target: "http://127.0.0.1:9", Enabled: false,
		SSLEnabled: true, SSLCert: "/c/f.pem", SSLKey: "/c/k.pem",
	}
	if err := srv.checkProxySSLPortMix(ctx, disabled); err != nil {
		t.Errorf("停用的规则不该被混用检查拦住：%v", err)
	}
}

// TestProxyRejectSpecsCarryCertForSSLPorts：SSL 端口的兜底块必须带证书。
func TestProxyRejectSpecsCarryCertForSSLPorts(t *testing.T) {
	rules := []*proxies.Rule{
		{ID: 1, Listen: 443, Domains: "a.test", Enabled: true, SSLEnabled: true,
			SSLCert: "/c/f.pem", SSLKey: "/c/k.pem"},
		{ID: 2, Listen: 8080, Domains: "b.test", Enabled: true},
		{ID: 3, Listen: 9443, Domains: "", Enabled: true, SSLEnabled: true,
			SSLCert: "/c/x.pem", SSLKey: "/c/xk.pem"},
		{ID: 4, Listen: 443, Domains: "c.test", Enabled: true},
		{ID: 5, Listen: 7443, Domains: "d.test", Enabled: false, SSLEnabled: true,
			SSLCert: "/c/d.pem", SSLKey: "/c/dk.pem"},
	}
	specs := proxyRejectSpecs(rules)
	if sp := specs[443]; !sp.SSL || sp.Cert != "/c/f.pem" || sp.Key != "/c/k.pem" {
		t.Errorf("443 端口（有 SSL 规则）的兜底块必须带证书，实际 %+v", sp)
	}
	if sp := specs[8080]; sp.SSL || sp.Cert != "" {
		t.Errorf("纯 HTTP 端口的兜底块不该带证书，实际 %+v", sp)
	}
	if _, ok := specs[9443]; ok {
		t.Error("通配规则（无域名）不需要兜底块")
	}
	if _, ok := specs[7443]; ok {
		t.Error("停用规则不该参与兜底块推导")
	}
}

// TestProbeProxyScheme：HTTPS 规则必须用 https 探测，否则会被误判成"没生效"。
func TestProbeProxyScheme(t *testing.T) {
	if got := probeProxyScheme(&proxies.Rule{SSLEnabled: false}); got != "http" {
		t.Errorf("未启用 SSL 应探测 http，实际 %q", got)
	}
	if got := probeProxyScheme(&proxies.Rule{SSLEnabled: true}); got != "https" {
		t.Errorf("启用 SSL 应探测 https，实际 %q", got)
	}
}

// TestCertDeleteRefusesWhenProxyUsesIt：证书被反代规则引用时不允许删除。
//
// 否则 vhost 里的 ssl_certificate 会指向不存在的文件，nginx 下次 reload
// 直接 [emerg] —— 影响的是整台机器上所有站点，不只是这条规则。
func TestCertDeleteRefusesWhenProxyUsesIt(t *testing.T) {
	srv, ts := newTestServer(t)
	fake := newFakeACME()
	certPath := filepath.Join(srv.Cfg.DataDir, "certs", "api.test", "fullchain.pem")
	fake.put(&acme.Cert{
		Primary: "api.test", Domains: []string{"api.test"},
		CertPath: certPath, KeyPath: filepath.Join(srv.Cfg.DataDir, "certs", "api.test", "privkey.pem"),
		NotAfter: time.Now().Add(60 * 24 * time.Hour),
	})
	srv.acmeOverride = fake
	seedProxyRule(t, srv, &proxies.Rule{
		Name: "引用证书的反代", Listen: 18452, Domains: "api.test",
		Target: "http://127.0.0.1:9", Enabled: true, SSLEnabled: true,
		SSLCert: certPath, SSLKey: filepath.Join(srv.Cfg.DataDir, "certs", "api.test", "privkey.pem"),
		SSLProvider: "acme",
	})

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "DELETE", "/api/v1/certs/api.test", nil, cookies)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("被反代规则引用时必须拒绝删除（409），实际 %d: %v", res.StatusCode, body)
	}
	if msg := fmt.Sprint(body["msg"]); !strings.Contains(msg, "反向代理") {
		t.Fatalf("拒绝理由里要列出引用它的反代规则: %v", msg)
	}
	if _, _, deleted := fake.snapshot(); len(deleted) != 0 {
		t.Fatalf("拒绝时不该真的删除，实际 %v", deleted)
	}
}

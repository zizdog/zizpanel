package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/proxies"
)

// ============================================================================
//  反向代理「需要用户名密码」（HTTP Basic Auth）的测试
//
//  证据分三层：
//    1. apr1 哈希与 openssl 逐字节一致（格式错 = nginx 一律 401）；
//    2. 生成的 nginx 配置真的有 auth_basic / auth_basic_user_file；
//    3. 接口层：保存 → 回读生效值 → 明文/哈希都不回显。
//  真正的"无凭据 401 / 正确凭据 200 / 错误凭据 401"由
//  api_proxies_auth_realnginx_test.go 起一个沙箱 nginx 证明。
// ============================================================================

// TestApr1CryptMatchesOpenSSL 用 openssl passwd -apr1 的**真实输出**钉住实现。
//
// 期望值来自：
//
//	openssl passwd -apr1 -salt zzSalt12 s3cret
//	openssl passwd -apr1 s3cret   # 盐 V3NPg8DE
func TestApr1CryptMatchesOpenSSL(t *testing.T) {
	cases := []struct{ password, salt, want string }{
		{"s3cret", "zzSalt12", "$apr1$zzSalt12$L/7TrEnY.dtLVSSwsrlPt."},
		{"s3cret", "V3NPg8DE", "$apr1$V3NPg8DE$DUStAgfure7XJBqr.kZrt1"},
		{"", "abcdefgh", "$apr1$abcdefgh$L.PT565ESX4Tp2bqNs7Ie."},
	}
	for _, c := range cases {
		if got := apr1Crypt(c.password, c.salt); got != c.want {
			t.Errorf("apr1Crypt(%q, %q) = %q，期望 %q", c.password, c.salt, got, c.want)
		}
	}
}

// TestHashProxyBasicAuthPasswordIsSaltedAndNotPlaintext：随机盐 + 绝不出现明文。
func TestHashProxyBasicAuthPasswordIsSaltedAndNotPlaintext(t *testing.T) {
	a, err := hashProxyBasicAuthPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hashProxyBasicAuthPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("两次哈希相同 —— 盐没有随机化（同一个口令会得到同一份哈希）")
	}
	for _, h := range []string{a, b} {
		if !strings.HasPrefix(h, "$apr1$") {
			t.Errorf("哈希格式不对（nginx 只认 apr1 等格式）：%q", h)
		}
		if strings.Contains(h, "s3cret") {
			t.Errorf("哈希里出现了明文口令：%q", h)
		}
	}
}

// TestProxyGenerateBasicAuth：开启鉴权时生成 auth_basic 两行；关闭时一行都没有。
func TestProxyGenerateBasicAuth(t *testing.T) {
	base := &proxies.Rule{
		Name: "鉴权规则", Listen: 18450, Domains: "auth.test",
		Target: "http://127.0.0.1:9", Enabled: true,
	}
	hash, err := hashProxyBasicAuthPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	on := *base
	on.AuthEnabled = true
	on.AuthUser = "tester"
	on.AuthHash = hash
	on.AuthFile = "/tmp/zp-test.htpasswd"
	out, err := on.Generate("")
	if err != nil {
		t.Fatalf("开启鉴权时生成配置失败：%v", err)
	}
	for _, want := range []string{
		`auth_basic "` + proxies.ProxyAuthRealm + `";`,
		"auth_basic_user_file /tmp/zp-test.htpasswd;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("生成的配置缺少 %q：\n%s", want, out)
		}
	}

	off, err := base.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "auth_basic") {
		t.Errorf("未开启鉴权时不该出现 auth_basic：\n%s", off)
	}
}

// TestProxyGenerateBasicAuthFailsClosed：鉴权配置不完整时必须报错，
// 绝不生成一份"没有鉴权"的配置（那等于把"需要密码"变成公开访问）。
func TestProxyGenerateBasicAuthFailsClosed(t *testing.T) {
	hash, _ := hashProxyBasicAuthPassword("s3cret")
	cases := []struct {
		name string
		rule proxies.Rule
	}{
		{"缺用户名", proxies.Rule{Name: "r", Listen: 18451, Target: "http://127.0.0.1:9",
			AuthEnabled: true, AuthHash: hash, AuthFile: "/tmp/a.htpasswd"}},
		{"缺哈希", proxies.Rule{Name: "r", Listen: 18451, Target: "http://127.0.0.1:9",
			AuthEnabled: true, AuthUser: "t", AuthFile: "/tmp/a.htpasswd"}},
		{"缺文件", proxies.Rule{Name: "r", Listen: 18451, Target: "http://127.0.0.1:9",
			AuthEnabled: true, AuthUser: "t", AuthHash: hash}},
	}
	for _, c := range cases {
		if _, err := c.rule.Generate(""); err == nil {
			t.Errorf("%s：应当报错而不是生成无鉴权配置", c.name)
		}
	}
}

// proxyAuthRealWrite 让"写 vhost"真的落盘，并让请求级复核看到访问日志增长。
//
// create 路径不知道规则 ID，所以 probe 时对所有 proxy-*.access.log 追加一行；
// waitProxyServed 只看该规则自己那份，逻辑上等价。
func proxyAuthRealWrite(t *testing.T, srv *Server) {
	t.Helper()
	prevProbe := proxyProbeFn
	t.Cleanup(func() { proxyProbeFn = prevProbe })
	proxyWriteVhostFn = func(s *Server, _ context.Context, name, content string) error {
		if err := os.MkdirAll(s.Cfg.VhostDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(s.Cfg.VhostDir, name+".conf"), []byte(content), 0o644); err != nil {
			return err
		}
		// 复核靠"该规则自己的访问日志增长"：这里先把日志文件建出来
		//（create 路径不知道 ID，从 vhost 名 proxy-<id> 里取）。
		if id := strings.TrimPrefix(name, "proxy-"); id != name && id != "" {
			_ = os.MkdirAll(s.proxyLogDir(), 0o755)
			f, ferr := os.OpenFile(filepath.Join(s.proxyLogDir(), "proxy-"+id+".access.log"),
				os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if ferr == nil {
				_ = f.Close()
			}
		}
		return nil
	}
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		matches, _ := filepath.Glob(filepath.Join(srv.proxyLogDir(), "proxy-*.access.log"))
		for _, m := range matches {
			appendToFile(t, m)
		}
		return "200", "upstream-ok", nil
	}
}

// TestProxyAuthUpdateRoundTripAndReadback：走真实接口开启鉴权，并断言"回读生效值"。
func TestProxyAuthUpdateRoundTripAndReadback(t *testing.T) {
	proxyStatusResetCache()
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	stubProxyHooks(t)
	proxyAuthRealWrite(t, srv)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "鉴权规则", Listen: 18452, Target: "http://127.0.0.1:9", Enabled: true,
	})
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/proxies/"+strconv.FormatInt(rule.ID, 10), map[string]any{
		"name": "鉴权规则", "listen": 18452, "target": "http://127.0.0.1:9", "enabled": true,
		"auth_enabled": true, "auth_user": "tester", "auth_password": "s3cret",
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("开启鉴权应 200，实际 %d：%v", res.StatusCode, body)
	}
	// 明文口令与哈希都绝不能出现在响应里。
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "s3cret") {
		t.Fatalf("响应泄漏了明文口令：%s", raw)
	}
	if strings.Contains(string(raw), "$apr1$") {
		t.Fatalf("响应泄漏了口令哈希：%s", raw)
	}
	data := apiData(t, body)
	auth, _ := data["auth"].(map[string]any)
	if auth == nil {
		t.Fatalf("响应缺少鉴权回读对象：%v", data)
	}
	if auth["enabled"] != true || auth["user"] != "tester" {
		t.Errorf("回读的鉴权开关/用户名不对：%v", auth)
	}
	if auth["verified"] != true {
		t.Errorf("配置已在磁盘上生效，回读应当 verified=true，实际：%v（note=%v）", auth, auth["note"])
	}

	// htpasswd 文件真的在、0600、内容为 user:hash。
	path := srv.proxyAuthFile(rule.ID)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("密码文件没有落盘：%v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("密码文件权限应为 0600，实际 %v", fi.Mode().Perm())
	}
	content, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(content), "tester:$apr1$") {
		t.Errorf("密码文件内容不对：%q", string(content))
	}
	if strings.Contains(string(content), "s3cret") {
		t.Errorf("密码文件里出现了明文：%q", string(content))
	}
	// 生成的 vhost 真有这两行。
	vhost, _ := os.ReadFile(filepath.Join(srv.Cfg.VhostDir, rule.VhostName()+".conf"))
	for _, want := range []string{"auth_basic ", "auth_basic_user_file " + path + ";"} {
		if !strings.Contains(string(vhost), want) {
			t.Errorf("vhost 缺少 %q：\n%s", want, vhost)
		}
	}

	// 再保存一次：用户名不变、密码留空 → 仍开启，且哈希保持不变（沿用原密码）。
	before := mustReadSetting(t, srv, proxyAuthKey(rule.ID))
	res, body, _ = doJSON(t, ts, "POST", "/api/v1/proxies/"+strconv.FormatInt(rule.ID, 10), map[string]any{
		"name": "鉴权规则", "listen": 18452, "target": "http://127.0.0.1:9", "enabled": true,
		"auth_enabled": true, "auth_user": "tester", "auth_password": "",
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("留空密码再保存应 200，实际 %d：%v", res.StatusCode, body)
	}
	if after := mustReadSetting(t, srv, proxyAuthKey(rule.ID)); after != before {
		t.Errorf("留空密码不该改哈希：before=%s after=%s", before, after)
	}

	// 关掉鉴权：文件与设置都清掉，vhost 里也不再出现 auth_basic。
	res, body, _ = doJSON(t, ts, "POST", "/api/v1/proxies/"+strconv.FormatInt(rule.ID, 10), map[string]any{
		"name": "鉴权规则", "listen": 18452, "target": "http://127.0.0.1:9", "enabled": true,
		"auth_enabled": false, "auth_user": "", "auth_password": "",
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("关闭鉴权应 200，实际 %d：%v", res.StatusCode, body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("关闭鉴权后密码文件应被删除，实际 err=%v", err)
	}
	if got := mustReadSetting(t, srv, proxyAuthKey(rule.ID)); strings.Contains(got, "$apr1$") {
		t.Errorf("关闭鉴权后不该残留哈希：%s", got)
	}
	vhost, _ = os.ReadFile(filepath.Join(srv.Cfg.VhostDir, rule.VhostName()+".conf"))
	if strings.Contains(string(vhost), "auth_basic") {
		t.Errorf("关闭鉴权后 vhost 不该再有 auth_basic：\n%s", vhost)
	}
}

// TestProxyAuthRequiresPasswordOnCreate：开启鉴权却没给密码 → 400，且规则不落库。
func TestProxyAuthRequiresPasswordOnCreate(t *testing.T) {
	proxyStatusResetCache()
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	stubProxyHooks(t)
	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/proxies", map[string]any{
		"name": "没密码", "listen": 18453, "target": "http://127.0.0.1:9", "enabled": true,
		"auth_enabled": true, "auth_user": "tester",
	}, cookies)
	if res.StatusCode != 400 {
		t.Fatalf("开启鉴权但缺密码应 400，实际 %d：%v", res.StatusCode, body)
	}
	if _, err := srv.proxyRepo().List(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ := srv.proxyRepo().List(context.Background())
	if len(list) != 0 {
		t.Errorf("校验失败时不该留下规则，实际 %d 条", len(list))
	}
}

// TestProxyAuthReadbackUnverifiedWithoutVhost：vhost 读不到时**绝不**报 verified，
// 而要如实说"未复核"（诚实原则）。
func TestProxyAuthReadbackUnverifiedWithoutVhost(t *testing.T) {
	proxyStatusResetCache()
	srv := newProxyTestServer(t)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "只落库", Listen: 18454, Target: "http://127.0.0.1:9", Enabled: true,
	})
	hash, _ := hashProxyBasicAuthPassword("s3cret")
	if err := srv.saveProxyAuth(context.Background(), rule.ID, proxyAuthConfig{
		Enabled: true, User: "tester", Hash: hash,
	}); err != nil {
		t.Fatal(err)
	}
	auth := srv.proxyAuthView(context.Background(), rule)
	if auth["enabled"] != true {
		t.Fatalf("设置里已开启，回读应报 enabled=true：%v", auth)
	}
	if auth["verified"] != false {
		t.Errorf("vhost 不存在时不得报 verified=true：%v", auth)
	}
	if note, _ := auth["note"].(string); !strings.Contains(note, "未复核") {
		t.Errorf("未复核时必须明确写出来，实际 note=%q", note)
	}
}

// helper ------------------------------------------------------------------

func mustReadSetting(t *testing.T, srv *Server, key string) string {
	t.Helper()
	v, err := srv.Store.GetSetting(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

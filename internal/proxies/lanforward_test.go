package proxies

import (
	"context"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/store"
)

// ============================================================================
//  「局域网出口」三态语义与渲染差异
//
//  背景（真机事实）：macOS 15 的「本地网络」隐私门会拦 Homebrew 的 nginx 访问
//  局域网，无头服务器没人点弹窗 → 反代全部 502；而面板自己（Go、linker-signed）
//  从来没被拦过。所以 auto 要把"目标是局域网"的规则改成经面板回环转发。
//
//  这一组测试**不碰真实网络**：DNS 全部用假解析器。
// ============================================================================

// fakeLookup 造一个只认固定表项的 DNS 解析器。
func fakeLookup(m map[string][]string) func(string) ([]string, error) {
	return func(host string) ([]string, error) {
		if ips, ok := m[strings.ToLower(host)]; ok {
			return ips, nil
		}
		return nil, &lookupNotFound{host: host}
	}
}

type lookupNotFound struct{ host string }

func (e *lookupNotFound) Error() string { return "no such host: " + e.host }

// TestClassifyTargetScope 锁住"什么算局域网"的判据。
func TestClassifyTargetScope(t *testing.T) {
	lookup := fakeLookup(map[string][]string{
		"nas.local":      {"192.168.1.8"},
		"public.example": {"93.184.216.34"},
		"mixed.example":  {"93.184.216.34", "10.0.0.5"},
		"loop.example":   {"127.0.0.1"},
		"weird.example":  {"not-an-ip"},
	})
	cases := []struct {
		host string
		want TargetScope
	}{
		// 回环：nginx 连它不经过隐私门 → 不需要转发
		{"127.0.0.1", ScopeLoopback},
		{"127.1.2.3", ScopeLoopback},
		{"::1", ScopeLoopback},
		// RFC1918
		{"10.0.0.5", ScopePrivate},
		{"172.16.3.4", ScopePrivate},
		{"172.31.255.254", ScopePrivate},
		{"192.168.1.8", ScopePrivate},
		// 链路本地与唯一本地 IPv6
		{"169.254.1.1", ScopePrivate},
		{"fe80::1", ScopePrivate},
		{"fd00::1", ScopePrivate},
		{"fc00::1", ScopePrivate},
		{"0.0.0.0", ScopePrivate},
		// 公网
		{"8.8.8.8", ScopePublic},
		{"172.32.0.1", ScopePublic}, // 172.16/12 之外，是公网
		{"2606:4700::1111", ScopePublic},
		// 域名：按解析结果判
		{"nas.local", ScopePrivate},
		{"public.example", ScopePublic},
		{"mixed.example", ScopePrivate}, // 只要有一个私有就按私有
		{"loop.example", ScopeLoopback},
		{"weird.example", ScopePrivate}, // 解析出怪东西 → 保守按私有
		{"unresolvable.example", ScopePrivate},
		{"", ScopePrivate},
	}
	for _, c := range cases {
		if got := ClassifyTarget(c.host, lookup); got != c.want {
			t.Errorf("ClassifyTarget(%q) = %q，期望 %q", c.host, got, c.want)
		}
	}
	// 大小写与 IPv6 方括号也要能判对（url.Hostname() 会去掉方括号，但这里直接收字面量）
	if got := ClassifyTarget("[::1]", lookup); got != ScopeLoopback {
		t.Errorf("带方括号的 ::1 应判回环，实际 %q", got)
	}
}

// TestRuleNeedsForward 锁住三态语义（auto/on/off）。
func TestRuleNeedsForward(t *testing.T) {
	lookup := fakeLookup(map[string][]string{
		"nas.local":      {"192.168.1.8"},
		"public.example": {"93.184.216.34"},
	})
	cases := []struct {
		name   string
		rule   Rule
		want   bool
		reason string
	}{
		{"auto+私有 IP", Rule{Target: "http://192.168.1.8:8081", LANForward: "auto"}, true, "局域网 → 经面板转发"},
		{"auto+链路本地", Rule{Target: "http://169.254.10.10:80", LANForward: "auto"}, true, "链路本地 → 转发"},
		{"auto+回环", Rule{Target: "http://127.0.0.1:8080", LANForward: "auto"}, false, "回环不受隐私门限制"},
		{"auto+公网 IP", Rule{Target: "http://93.184.216.34:80", LANForward: "auto"}, false, "公网 → 直连"},
		{"auto+私有域名", Rule{Target: "http://nas.local:8090", LANForward: "auto"}, true, "解析到私有 → 转发"},
		{"auto+公网域名", Rule{Target: "http://public.example:80", LANForward: "auto"}, false, "解析到公网 → 直连"},
		{"auto+解析不了", Rule{Target: "http://nope.example:80", LANForward: "auto"}, true, "判不出来按私有（安全侧）"},
		{"空字符串按 auto", Rule{Target: "http://192.168.1.8:8081"}, true, "默认 auto"},
		{"on+公网", Rule{Target: "http://93.184.216.34:80", LANForward: "on"}, true, "用户强制转发"},
		{"on+回环", Rule{Target: "http://127.0.0.1:8080", LANForward: "on"}, true, "强制转发连回环也转"},
		{"off+私有", Rule{Target: "http://192.168.1.8:8081", LANForward: "off"}, false, "强制直连保留旧行为"},
	}
	for _, c := range cases {
		r := c.rule
		if got := r.NeedsForward(lookup); got != c.want {
			t.Errorf("%s：NeedsForward = %v，期望 %v（%s）", c.name, got, c.want, c.reason)
		}
	}
}

// TestGenerateForwardsPrivateTarget：auto + 私有目标 + 已分配端口 → proxy_pass 只连回环，
// 同时保留真实目标的注释与正确的 Host 头。
func TestGenerateForwardsPrivateTarget(t *testing.T) {
	r := &Rule{
		ID: 3, Name: "镜像站", Listen: 8081, Target: "http://192.168.1.8:8081",
		Enabled: true, Websocket: true, LANForward: "auto", ForwardPort: 47003,
	}
	out, err := r.Generate("/tmp/logs")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"proxy_pass http://127.0.0.1:47003;",
		"# target: http://192.168.1.8:8081（经面板转发）",
		// Host 头仍是真实目标：后端按 Host 分站，写回环地址会让它 404
		"proxy_set_header Host 192.168.1.8:8081;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("经面板转发的配置缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "proxy_pass http://192.168.1.8:8081;") {
		t.Errorf("经面板转发时不该再直连局域网目标：\n%s", out)
	}
}

// TestGenerateForwardsHTTPSUpstreamKeepsScheme：https 上游经转发时必须写成
// https://127.0.0.1:<port>，否则 nginx 会对转发器发明文 HTTP，而转发器把明文
// 原样送到 TLS 上游 → 握手失败。proxy_ssl_* 仍要照旧输出（SNI 穿过转发器）。
func TestGenerateForwardsHTTPSUpstreamKeepsScheme(t *testing.T) {
	r := &Rule{
		ID: 4, Name: "panel2", Listen: 8889, Domains: "p2.example.com",
		Target: "https://192.168.1.4:8443", Enabled: true,
		LANForward: "auto", ForwardPort: 47004,
	}
	out, err := r.Generate("/tmp/logs")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"proxy_pass https://127.0.0.1:47004;",
		"proxy_ssl_server_name on;",
		"proxy_ssl_name p2.example.com;", // 目标是 IP → SNI 取规则域名
		"proxy_ssl_verify off;",
		"# target: https://192.168.1.4:8443（经面板转发）",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("https 上游经转发的配置缺少 %q：\n%s", want, out)
		}
	}
}

// TestGenerateDirectForPublicAndOffStaysLegacy：直连渲染必须与升级前逐字一致。
func TestGenerateDirectForPublicAndOffStaysLegacy(t *testing.T) {
	// auto + 公网：没有分配端口，输出与老版一致
	pub := &Rule{ID: 5, Name: "web", Listen: 8082, Target: "http://example.com:8080", Enabled: true, LANForward: "auto"}
	out, err := pub.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"proxy_pass http://example.com:8080;", "# target: http://example.com:8080\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("公网直连输出缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "127.0.0.1") || strings.Contains(out, "经面板转发") {
		t.Errorf("公网目标不该走转发：\n%s", out)
	}

	// off + 私有：用户明确要 nginx 直连（即使数据库里残留了端口也不能转发）
	off := &Rule{ID: 6, Name: "legacy", Listen: 8083, Target: "http://192.168.1.8:8090",
		Enabled: true, LANForward: "off", ForwardPort: 47006}
	out2, err := off.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "proxy_pass http://192.168.1.8:8090;") {
		t.Errorf("off 模式必须直连：\n%s", out2)
	}
	if strings.Contains(out2, "127.0.0.1") || strings.Contains(out2, "经面板转发") {
		t.Errorf("off 模式不该出现任何转发痕迹：\n%s", out2)
	}
}

// TestGenerateOnWithoutPortFailsHonestly：选了「强制经面板转发」但端口没分配时
// 必须报错，不能静默退回直连（那是"以为在转发、其实直连"的假成功）。
func TestGenerateOnWithoutPortFailsHonestly(t *testing.T) {
	r := &Rule{ID: 7, Name: "forced", Listen: 8084, Target: "http://192.168.1.8:8081",
		Enabled: true, LANForward: "on", ForwardPort: 0}
	_, err := r.Generate("")
	if err == nil {
		t.Fatal("on 模式没有回环端口时必须报错")
	}
	if !strings.Contains(err.Error(), "经面板转发") {
		t.Errorf("报错应说明是强制转发没起来，实际：%v", err)
	}
}

// TestValidateRejectsBadLANForward：三态写错时明确拒绝，不静默按 auto。
func TestValidateRejectsBadLANForward(t *testing.T) {
	bad := &Rule{Name: "n", Listen: 8080, Target: "http://192.168.1.8:80", LANForward: "yes"}
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "局域网出口") {
		t.Fatalf("非法三态应被拒绝，实际：%v", err)
	}
	ok := &Rule{Name: "n", Listen: 8080, Target: "http://192.168.1.8:80", LANForward: ""}
	if err := ok.Validate(); err != nil {
		t.Fatalf("空字符串应按 auto 通过，实际：%v", err)
	}
	if ok.LANForward != LANForwardAuto {
		t.Errorf("Validate 应把空值归一成 auto，实际 %q", ok.LANForward)
	}
}

// TestRuleRepositoryRoundTripsLANForwardFields：新列必须真的写进/读回数据库。
func TestRuleRepositoryRoundTripsLANForwardFields(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repo := NewRepository(st)
	ctx := context.Background()

	created, err := repo.Create(ctx, &Rule{
		Name: "局域网规则", Listen: 18091, Target: "http://192.168.1.8:8081", Enabled: true,
		LANForward: "on", ForwardPort: 47001,
	})
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if created.LANForward != "on" || created.ForwardPort != 47001 {
		t.Fatalf("新建后局域网出口字段没有落库/读回：%+v", created)
	}

	// SetForwardPort 只改端口，不动其它字段
	if err := repo.SetForwardPort(ctx, created.ID, 47002); err != nil {
		t.Fatalf("SetForwardPort 失败：%v", err)
	}
	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ForwardPort != 47002 || got.LANForward != "on" || got.Target != "http://192.168.1.8:8081" {
		t.Fatalf("SetForwardPort 改动了不该改的字段：%+v", got)
	}

	// 默认 auto：不显式给 lan_forward 时落库为 auto
	def, err := repo.Create(ctx, &Rule{Name: "默认", Listen: 18092, Target: "http://127.0.0.1:9", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if def.LANForward != LANForwardAuto || def.ForwardPort != 0 {
		t.Fatalf("缺省应是 auto + 端口 0，实际 lan_forward=%q port=%d", def.LANForward, def.ForwardPort)
	}
}

package web

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/proxies"
)

// ============================================================================
//  「局域网出口」在面板层的接线：状态展示、诊断文案、转发器生命周期。
//
//  单测全部沙箱化：假上游 / 假探针 / 假解析器，不碰真实局域网、不碰真实 nginx。
// ============================================================================

// useTestForwarder 把面板的转发器换成端口区间受限、解析器可钉的测试实例。
//
// 不复用 New 里那个默认 47000–47999 的管理器：开发机上真的跑着面板，
// 测试不该去抢生产可能用到的端口区间。
func useTestForwarder(t *testing.T, srv *Server) *proxies.Manager {
	t.Helper()
	m := proxies.NewManager(proxies.ManagerOptions{
		PortMin:    48700,
		PortMax:    48749,
		LookupHost: func(h string) ([]string, error) { return proxyLookupHostFn(h) },
		Logf:       func(string, ...any) {},
	})
	srv.forwarders = m
	t.Cleanup(m.StopAll)
	return m
}

// stubProbeTarget 把"面板直连目标"钉成假结果，避免单测真的去连局域网。
func stubProbeTarget(t *testing.T, ok bool, detail string) {
	t.Helper()
	prev := proxyProbeTargetFn
	proxyProbeTargetFn = func(context.Context, string) (bool, string) { return ok, detail }
	t.Cleanup(func() { proxyProbeTargetFn = prev })
}

// TestProxyViewReportsLANForwardState：规则视图必须给出可读的三态、端口、
// 连接数与"直连局域网有风险"的警示位。
func TestProxyViewReportsLANForwardState(t *testing.T) {
	srv := newProxyTestServer(t)
	m := useTestForwarder(t, srv)
	stubProbeTarget(t, true, "可达 192.168.1.8:8081（1ms）")

	rule := &proxies.Rule{ID: 101, Name: "局域网", Listen: 18080,
		Target: "http://192.168.1.8:8081", Enabled: true, LANForward: "off"}

	v := srv.proxyView(context.Background(), rule)
	if v["lan_forward"] != "off" || v["lan_forward_label"] != "nginx 直连" {
		t.Fatalf("三态展示不对：%v / %v", v["lan_forward"], v["lan_forward_label"])
	}
	if v["target_scope"] != "private" {
		t.Fatalf("target_scope 应为 private，实际 %v", v["target_scope"])
	}
	if v["forward_active"] != false {
		t.Fatalf("off 模式不该在转发：%v", v["forward_active"])
	}
	if v["lan_direct_warning"] != true {
		t.Fatalf("直连局域网目标必须给出显眼提示：%+v", v)
	}

	// 改成强制转发：状态要能看出端口在听、提示消失
	rule.LANForward = "on"
	if _, err := m.Ensure(rule); err != nil {
		t.Fatal(err)
	}
	v2 := srv.proxyView(context.Background(), rule)
	if v2["forward_active"] != true {
		t.Fatalf("转发器起来后 forward_active 应为 true：%+v", v2)
	}
	if v2["forward_port"] != rule.ForwardPort || rule.ForwardPort <= 0 {
		t.Fatalf("应展示回环端口：%v vs %d", v2["forward_port"], rule.ForwardPort)
	}
	if v2["lan_direct_warning"] != false {
		t.Fatalf("已在转发就不该再提示直连风险：%+v", v2)
	}
}

// TestDescribeProxyProbeGivesLANAdvice：502/504 + 直连 + 局域网目标时，
// 诊断文案必须写清"是什么 + 怎么修"。
func TestDescribeProxyProbeGivesLANAdvice(t *testing.T) {
	directLAN := &proxies.Rule{Listen: 18080, Target: "http://192.168.1.8:8081",
		Enabled: true, LANForward: "off"}
	msg := describeProxyProbe(proxyProbe{code: "502", body: "<title>502</title>"}, directLAN)
	for _, want := range []string{"HTTP 502", "本地网络", "经面板转发", "系统设置", "隐私与安全性"} {
		if !strings.Contains(msg, want) {
			t.Errorf("局域网直连失败文案缺少 %q：\n%s", want, msg)
		}
	}

	// 已经在用面板转发时不该再劝用户"改成经面板转发"
	forwarding := &proxies.Rule{Listen: 18080, Target: "http://192.168.1.8:8081",
		Enabled: true, LANForward: "auto", ForwardPort: 48700}
	if got := describeProxyProbe(proxyProbe{code: "502"}, forwarding); strings.Contains(got, "本地网络") {
		t.Errorf("已在转发的规则不该出现转发建议：\n%s", got)
	}

	// 公网目标也不是这个毛病
	public := &proxies.Rule{Listen: 18080, Target: "http://93.184.216.34:80",
		Enabled: true, LANForward: "auto"}
	if got := describeProxyProbe(proxyProbe{code: "502"}, public); strings.Contains(got, "本地网络") {
		t.Errorf("公网目标不该出现局域网授权建议：\n%s", got)
	}

	// 没有 502/504 时不加
	if got := describeProxyProbe(proxyProbe{code: "200"}, directLAN); strings.Contains(got, "本地网络") {
		t.Errorf("非 502/504 不该出现转发建议：\n%s", got)
	}
}

// TestDirectLANBlockedAdvice：只有在"面板自己能连上、只有 nginx 连不上"时才
// 把 502 判成 macOS 授权问题（否则会把"上游挂了"误诊成授权问题）。
func TestDirectLANBlockedAdvice(t *testing.T) {
	srv := newProxyTestServer(t)
	useTestForwarder(t, srv)
	rule := &proxies.Rule{Name: "局域网", Listen: 18080, Target: "http://192.168.1.8:8081",
		Enabled: true, LANForward: "off"}
	probe := proxyProbe{code: "502", body: "502"}

	stubProbeTarget(t, true, "可达")
	advice := srv.directLANBlockedAdvice(context.Background(), rule, probe)
	for _, want := range []string{"本地网络", "经面板转发", "只有 nginx 连不上"} {
		if !strings.Contains(advice, want) {
			t.Errorf("诊断缺少 %q：\n%s", want, advice)
		}
	}

	// 面板也连不上 → 是上游没起来，不该归咎于授权门
	stubProbeTarget(t, false, "连不上")
	if got := srv.directLANBlockedAdvice(context.Background(), rule, probe); got != "" {
		t.Errorf("面板也连不上时不该报授权问题：\n%s", got)
	}

	// 已经在用面板转发 → 不适用
	stubProbeTarget(t, true, "可达")
	rule2 := *rule
	rule2.LANForward = "auto" // 端口已分配 = 真的在转发
	rule2.ForwardPort = 48701
	if got := srv.directLANBlockedAdvice(context.Background(), &rule2, probe); got != "" {
		t.Errorf("已转发的规则不该报授权问题：\n%s", got)
	}
}

// TestSyncForwarderLifecycle：web 层的增删改都通过 syncForwarder 起停转发器，
// 且"改成 off / 停用"是真的把监听器关掉。
func TestSyncForwarderLifecycle(t *testing.T) {
	srv := newProxyTestServer(t)
	m := useTestForwarder(t, srv)

	rule := &proxies.Rule{ID: 102, Name: "lan", Listen: 18080,
		Target: "http://192.168.1.8:8081", Enabled: true, LANForward: "auto"}
	if err := srv.syncForwarder(rule); err != nil {
		t.Fatalf("syncForwarder 失败：%v", err)
	}
	if rule.ForwardPort <= 0 {
		t.Fatal("auto + 局域网目标应当分配回环端口")
	}
	if _, ok := m.Status(rule.ID); !ok {
		t.Fatal("转发器应当在监听")
	}
	out, err := rule.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "proxy_pass http://127.0.0.1:"+strconv.Itoa(rule.ForwardPort)+";") {
		t.Fatalf("生成的配置应当只连回环：\n%s", out)
	}

	// 改成 nginx 直连 → 监听器必须关掉、端口清 0、渲染退回直连
	rule.LANForward = "off"
	if err := srv.syncForwarder(rule); err != nil {
		t.Fatal(err)
	}
	if rule.ForwardPort != 0 {
		t.Fatalf("off 之后端口应清 0，实际 %d", rule.ForwardPort)
	}
	if _, ok := m.Status(rule.ID); ok {
		t.Fatal("off 之后不该还有监听器")
	}
	out2, err := rule.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "proxy_pass http://192.168.1.8:8081;") {
		t.Fatalf("off 之后应直连：\n%s", out2)
	}

	// 停用规则 → 同样关掉（端口可留待启用时复用，这里只要求不再监听）
	rule.LANForward = "auto"
	if err := srv.syncForwarder(rule); err != nil {
		t.Fatal(err)
	}
	rule.Enabled = false
	if err := srv.syncForwarder(rule); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Status(rule.ID); ok {
		t.Fatal("停用后不该还有监听器")
	}
}

// TestReconcileForwardersPersistsPortAndRewritesVhost：面板启动时按数据库里的
// 规则对齐转发器，把端口落库，并在端口变化时重写 nginx 配置。
func TestReconcileForwardersPersistsPortAndRewritesVhost(t *testing.T) {
	srv := newProxyTestServer(t)
	m := useTestForwarder(t, srv)
	h := stubProxyHooks(t)

	repo := srv.proxyRepo()
	ctx := context.Background()
	rule, err := repo.Create(ctx, &proxies.Rule{
		Name: "局域网", Listen: 18080, Domains: "lan.example.com",
		Target: "http://192.168.1.8:8081", Enabled: true, LANForward: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule.ForwardPort != 0 {
		t.Fatalf("新建时还没分配端口，实际 %d", rule.ForwardPort)
	}
	// 复核靠该规则自己的访问日志增长，这里模拟 nginx 真的受理了请求。
	logPath := proxyAccessLogPath(srv.proxyLogDir(), rule.ID)
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		h.Order = append(h.Order, "probe")
		appendToFile(t, logPath)
		return "200", "upstream-ok", nil
	}

	srv.reconcileForwarders(ctx)

	got, err := repo.Get(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ForwardPort <= 0 {
		t.Fatalf("启动对齐后端口必须落库：%+v", got)
	}
	if _, ok := m.Status(got.ID); !ok {
		t.Fatal("启动对齐后转发器应当在监听")
	}
	if len(h.Order) == 0 || h.Order[0] != "write" {
		t.Fatalf("端口变化后必须重写 vhost，调用顺序 = %v", h.Order)
	}

	// 幂等：再次对齐不应改端口
	srv.reconcileForwarders(ctx)
	again, err := repo.Get(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ForwardPort != got.ForwardPort {
		t.Fatalf("重复对齐不应改端口：%d → %d", got.ForwardPort, again.ForwardPort)
	}
}

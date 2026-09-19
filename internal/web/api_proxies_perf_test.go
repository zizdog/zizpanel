package web

import (
	"context"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/proxies"
)

// ============================================================================
//  反向代理列表的**性能门禁**（2026-09-19 用户报"反向代理页明显变慢"）
//
//  为什么是行为级而不是纯计时：列表路径上"有没有跑真实探测"才是根因。
//  这里把目标探测换成"睡 200ms 并计数"的替身 —— 只要列表路径碰了它，
//  计数就 >0、耗时就随规则数线性增长。修好后列表一次都不该碰它。
// ============================================================================

// TestProxyListNeverProbesTargets：列表接口不跑任何真实探测。
func TestProxyListNeverProbesTargets(t *testing.T) {
	proxyStatusResetCache()
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	for i := 1; i <= 4; i++ {
		seedProxyRule(t, srv, &proxies.Rule{
			Name: "慢规则", Listen: 18500 + i, Target: "http://127.0.0.1:9", Enabled: true,
		})
	}
	prev := proxyProbeTargetFn
	calls := 0
	proxyProbeTargetFn = func(ctx context.Context, target string) (bool, string) {
		calls++
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
		return false, "不可达"
	}
	t.Cleanup(func() { proxyProbeTargetFn = prev })
	cookies := loginTestPanel(t, ts)

	start := time.Now()
	res, body, _ := doJSON(t, ts, "GET", "/api/v1/proxies", nil, cookies)
	elapsed := time.Since(start)
	if res.StatusCode != 200 {
		t.Fatalf("列表应 200，实际 %d：%v", res.StatusCode, body)
	}
	if calls != 0 {
		t.Fatalf("列表路径跑了 %d 次真实目标探测（必须是 0：探测只能异步/按需）", calls)
	}
	// 4 条 × 200ms 串行 = 800ms；这里给足余量但必须远低于它。
	if elapsed > 250*time.Millisecond {
		t.Errorf("列表耗时 %v，仍随规则数增长（期望 <250ms，且不随条数变化）", elapsed)
	}
	list, _ := apiData(t, body)["list"].([]any)
	if len(list) != 4 {
		t.Fatalf("应返回 4 条规则，实际 %d", len(list))
	}
	first := list[0].(map[string]any)
	if first["status_probed"] != false {
		t.Errorf("未探测过的规则应报 status_probed=false，实际 %v", first["status_probed"])
	}
}

// TestProxyBatchProbeIsConcurrentAndCached：批量探测并发跑、结果进缓存、
// 列表随即能读到（带检测时间）。
func TestProxyBatchProbeIsConcurrentAndCached(t *testing.T) {
	proxyStatusResetCache()
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	for i := 1; i <= 4; i++ {
		seedProxyRule(t, srv, &proxies.Rule{
			Name: "目标", Listen: 18510 + i, Target: "http://127.0.0.1:9", Enabled: true,
		})
	}
	prev := proxyProbeTargetFn
	proxyProbeTargetFn = func(ctx context.Context, target string) (bool, string) {
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
		return false, "连不上 127.0.0.1:9"
	}
	t.Cleanup(func() { proxyProbeTargetFn = prev })
	cookies := loginTestPanel(t, ts)

	// 并发上限 8，4 条应在一轮超时内（~200ms）跑完，而不是 4×200ms。
	start := time.Now()
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/proxies/test", map[string]any{"all": true}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("批量探测应 200，实际 %d：%v", res.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Errorf("4 条并发探测耗时 %v（>600ms，像是串行）", elapsed)
	}
	data := apiData(t, body)
	items, _ := data["list"].([]any)
	if len(items) != 4 {
		t.Fatalf("应返回 4 条探测结果，实际 %d：%v", len(items), data)
	}
	for _, it := range items {
		m := it.(map[string]any)
		if m["target_ok"] != false {
			t.Errorf("探测结果应如实报 target_ok=false：%v", m)
		}
		if m["probed_at"] == "" || m["probed_at"] == nil {
			t.Errorf("每条结果都要带检测时间：%v", m)
		}
	}
	if data["probed_at"] == nil {
		t.Errorf("响应要带整体检测时间：%v", data)
	}

	// 探测后列表立刻能读到缓存（且带时间戳）。
	res, body, _ = doJSON(t, ts, "GET", "/api/v1/proxies", nil, cookies)
	_ = res
	list, _ := apiData(t, body)["list"].([]any)
	m := list[0].(map[string]any)
	if m["status_probed"] != true {
		t.Errorf("探测后列表应报 status_probed=true：%v", m)
	}
	if m["status_probe_at"] == nil || m["status_probe_at"] == "" {
		t.Errorf("列表应带回检测时间：%v", m)
	}
}

// TestProxyStatusCacheStaleMarked：缓存超过 TTL 后列表要标 stale=true，
// 绝不能把过期结果当成实时状态（诚实原则）。
func TestProxyStatusCacheStaleMarked(t *testing.T) {
	proxyStatusResetCache()
	srv := newProxyTestServer(t)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "过期", Listen: 18520, Target: "http://127.0.0.1:9", Enabled: true,
	})
	// 直接写一条"很久以前"的缓存。
	proxyStatusStore(rule, true, true, "可达", time.Now().Add(-2*proxyStatusTTL))
	v := srv.proxyView(context.Background(), rule)
	if v["status_probed"] != true {
		t.Fatalf("有缓存就应报 status_probed=true：%v", v)
	}
	if v["status_stale"] != true {
		t.Errorf("超过 TTL 的缓存必须标 stale=true：%v", v)
	}
}

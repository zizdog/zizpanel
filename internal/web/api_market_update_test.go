package web

// api_market_update_test.go —— 市场「检查更新」接口的门禁。
//
// 两件事必须锁住：
//  ① 列表/首屏路径**不做探测**（昂贵探测只在按需接口里做，AGENTS 第三节 7）；
//  ② 按需接口真的做探测、有 TTL 缓存、安装完能让缓存失效。
//
// 探测默认会打真实镜像索引 + 以真实用户起 `--version` 子进程，所以单测一律用
// marketUpdateCheckOverride 注入假结论（绝不联网、绝不起进程）。

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// TestMarketListSupportsUpdateCheckIsStaticNoProbe 证明列表路径只给静态字段、不做探测。
//
// 行为层：把探测入口换成"被调用就记账"的假实现，拉一次市场列表后计数必须是 0。
// 代码层：handleMarketList 所在的 api_services.go 里不许出现任何探测调用 ——
// 万一有人绕过 probeAppUpdate 直接调 services 的探测，行为层抓不到，代码层抓得到。
func TestMarketListSupportsUpdateCheckIsStaticNoProbe(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	var probes atomic.Int64
	srv.marketUpdateCheckOverride = func(context.Context, string) services.ZizvideoUpdateCheck {
		probes.Add(1)
		return services.ZizvideoUpdateCheck{App: "zizvideo", Unknown: true}
	}

	res, _, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("市场列表应 200，实际 %d", res.StatusCode)
	}
	if n := probes.Load(); n != 0 {
		t.Errorf("列表路径不许探测更新（探测入口被调用 %d 次）", n)
	}

	ziz := marketItem(t, ts, cookies, "zizvideo")
	if ziz["supports_update_check"] != true {
		t.Errorf("zizvideo 是动态版本条目，列表必须给 supports_update_check=true，实际 %v",
			ziz["supports_update_check"])
	}
	if _, has := ziz["update_available"]; has {
		t.Error("列表不该带探测结论（update_available 是按需接口的字段）")
	}
	for _, id := range []string{"nginx", "php82", "frpc"} {
		it := marketItem(t, ts, cookies, id)
		if it["supports_update_check"] != false {
			t.Errorf("%s 不该支持更新检查，实际 %v", id, it["supports_update_check"])
		}
	}

	// 代码层：探测调用只许出现在 api_market_update.go，不许混进列表所在文件。
	// 判据带 "(" —— 只抓真的调用，不抓注释里提到的接口路径。
	src := readGoSource(t, "api_services.go")
	for _, bad := range []string{"CheckZizvideoUpdate(", "probeAppUpdate(", "appUpdateCheck("} {
		if strings.Contains(src, bad) {
			t.Errorf("api_services.go 里出现了探测调用 %q —— 市场列表/首屏路径不许含更新探测", bad)
		}
	}
}

// TestMarketUpdateCheckEndpointAndCache 覆盖按需接口的 JSON 形状与 TTL 缓存。
func TestMarketUpdateCheckEndpointAndCache(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	var probes atomic.Int64
	srv.marketUpdateCheckOverride = func(context.Context, string) services.ZizvideoUpdateCheck {
		probes.Add(1)
		return services.ZizvideoUpdateCheck{
			App: "zizvideo", Installed: "0.1.1-mvp", Latest: "0.2.0",
			UpdateAvailable: true, CheckedAt: time.Now().Format(time.RFC3339),
		}
	}

	get := func() map[string]any {
		t.Helper()
		res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/zizvideo/update-check", nil, cookies)
		if res.StatusCode != 200 {
			t.Fatalf("update-check 应 200，实际 %d", res.StatusCode)
		}
		data, _ := out["data"].(map[string]any)
		if data == nil {
			t.Fatalf("update-check 响应缺少 data：%v", out)
		}
		return data
	}

	d1 := get()
	if d1["installed"] != "0.1.1-mvp" || d1["latest"] != "0.2.0" || d1["update_available"] != true || d1["unknown"] != false {
		t.Errorf("update-check 字段不符：%v", d1)
	}
	if asString(d1["checked_at"]) == "" {
		t.Error("必须带 checked_at（前端按它做 sessionStorage TTL）")
	}
	_ = get()
	if n := probes.Load(); n != 1 {
		t.Errorf("TTL 内应命中缓存只探测一次，实际 %d 次", n)
	}

	// 安装/更新完成后要能立刻让缓存失效（api_tasks.go 的收尾钩子就是这么做的）。
	srv.forgetUpdateCheck("zizvideo")
	_ = get()
	if n := probes.Load(); n != 2 {
		t.Errorf("失效后应重新探测，实际 %d 次", n)
	}

	// 用户手动点「检查更新」（fresh=1）要绕过进程内缓存，否则一次瞬时故障的
	// unknown 会顶到 TTL 结束、点按钮没反应。
	res, out2, _ := doJSON(t, ts, "GET", "/api/v1/market/zizvideo/update-check?fresh=1", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("fresh=1 应 200，实际 %d", res.StatusCode)
	}
	if _, okData := out2["data"].(map[string]any); !okData {
		t.Fatalf("fresh=1 响应缺少 data：%v", out2)
	}
	if n := probes.Load(); n != 3 {
		t.Errorf("fresh=1 应绕过缓存重新探测，实际 %d 次", n)
	}
}

// TestMarketUpdateCheckRejectsUnsupportedApp 非动态版本条目直接 4xx，不假装能检查。
func TestMarketUpdateCheckRejectsUnsupportedApp(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	srv.marketUpdateCheckOverride = func(context.Context, string) services.ZizvideoUpdateCheck {
		t.Error("不支持的条目不该走到探测")
		return services.ZizvideoUpdateCheck{}
	}

	res, _, _ := doJSON(t, ts, "GET", "/api/v1/market/nginx/update-check", nil, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("nginx 不支持更新检查，应 400，实际 %d", res.StatusCode)
	}
}

package web

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  站点「打开」必须用公网入口（坑 214，2026-09-20 生产事故）
//
//  站点自己的监听端口（默认 80）不等于"公网怎么进来"：域名被一条反代规则
//  前置（规则监听别的端口、可能带 SSL）时，点「打开」落到的却是 80 上的默认站点
//  或路由器。门禁：后端从 proxies 算 public_entry（有/无/多条/停用四态），
//  前端「打开」优先用它，且不许在打开处硬编码 80。
// ============================================================================

func seedPublicEntrySite(t *testing.T, srv *Server, domain string) *sites.Site {
	t.Helper()
	st := &sites.Site{
		Domain: domain, Rewrite: "none", ListenPort: 8090, Enabled: true,
		Root: filepath.Join(srv.Cfg.WWWRoot, domain),
	}
	if err := srv.siteMgr().Create(context.Background(), st); err != nil {
		t.Fatalf("建站点 %s 失败: %v", domain, err)
	}
	return st
}

func listSiteItem(t *testing.T, out map[string]any, domain string) map[string]any {
	t.Helper()
	data := apiData(t, out)
	raw, ok := data["list"].([]any)
	if !ok {
		t.Fatalf("站点列表响应里没有 list 数组: %v", data)
	}
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if fmtDomain(m) == domain {
			return m
		}
	}
	t.Fatalf("列表里找不到站点 %s", domain)
	return nil
}

func fmtDomain(m map[string]any) string {
	s, _ := m["domain"].(string)
	return s
}

// TestSitePublicEntryWithRule：有规则命中 → 用规则的端口与来源规则名。
func TestSitePublicEntryWithRule(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	seedPublicEntrySite(t, srv, "entry-rule.test")
	seedProxyRule(t, srv, &proxies.Rule{
		Name: "镜像入口", Listen: 8899, Domains: "entry-rule.test", Enabled: true,
		Target: "http://127.0.0.1:9001",
	})
	cookies := loginTestPanel(t, ts)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/sites", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("列表应 200: %d %v", res.StatusCode, out)
	}
	pe, _ := listSiteItem(t, out, "entry-rule.test")["public_entry"].(map[string]any)
	if pe == nil {
		t.Fatalf("有规则命中时必须给 public_entry: %v", listSiteItem(t, out, "entry-rule.test"))
	}
	if pe["public_url"] != "http://entry-rule.test:8899/" {
		t.Errorf("public_url = %v，期望 http://entry-rule.test:8899/", pe["public_url"])
	}
	if pe["rule_name"] != "镜像入口" || pe["available"] != true {
		t.Errorf("来源规则与可用状态不对: %v", pe)
	}

	// 详情接口同样要带（用户从详情页点「打开」也一样）
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/sites/entry-rule.test", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("详情应 200: %d %v", res.StatusCode, out)
	}
	dpe, _ := apiData(t, out)["public_entry"].(map[string]any)
	if dpe == nil || dpe["public_url"] != "http://entry-rule.test:8899/" {
		t.Errorf("详情接口的 public_entry 不对: %v", dpe)
	}
}

// TestSitePublicEntryNoRule：没有规则命中 → 不给 public_entry（前端保持旧行为）。
func TestSitePublicEntryNoRule(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	seedPublicEntrySite(t, srv, "entry-none.test")
	// 一条规则，但服务的是别的域名：不算命中
	seedProxyRule(t, srv, &proxies.Rule{
		Name: "别的站", Listen: 8899, Domains: "other.test", Enabled: true,
		Target: "http://127.0.0.1:9001",
	})
	cookies := loginTestPanel(t, ts)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites", nil, cookies)
	it := listSiteItem(t, out, "entry-none.test")
	if _, ok := it["public_entry"]; ok && it["public_entry"] != nil {
		t.Errorf("无规则命中时不该给 public_entry: %v", it["public_entry"])
	}
}

// TestSitePublicEntryMultipleRules：多条命中 → 选一条并可说明其它规则名。
func TestSitePublicEntryMultipleRules(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	seedPublicEntrySite(t, srv, "entry-multi.test")
	seedProxyRule(t, srv, &proxies.Rule{
		Name: "规则B", Listen: 9002, Domains: "entry-multi.test", Enabled: true,
		Target: "http://127.0.0.1:9001",
	})
	seedProxyRule(t, srv, &proxies.Rule{
		Name: "规则A", Listen: 9001, Domains: "entry-multi.test", Enabled: true,
		Target: "http://127.0.0.1:9001",
	})
	cookies := loginTestPanel(t, ts)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites", nil, cookies)
	pe, _ := listSiteItem(t, out, "entry-multi.test")["public_entry"].(map[string]any)
	if pe == nil {
		t.Fatal("多规则命中时必须给 public_entry")
	}
	if pe["rule_name"] != "规则A" || pe["public_url"] != "http://entry-multi.test:9001/" {
		t.Errorf("多条命中应选端口小的那条并说明规则名: %v", pe)
	}
	others, _ := pe["other_rules"].([]any)
	if len(others) != 1 || others[0] != "规则B" {
		t.Errorf("其它命中规则要如实列出: %v", pe["other_rules"])
	}
}

// TestSitePublicEntryDisabledRule：命中的规则停用 → 地址给出来但标不可用（前端退回旧行为）。
func TestSitePublicEntryDisabledRule(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	seedPublicEntrySite(t, srv, "entry-off.test")
	seedProxyRule(t, srv, &proxies.Rule{
		Name: "停用的入口", Listen: 8899, Domains: "entry-off.test", Enabled: false,
		Target: "http://127.0.0.1:9001",
	})
	cookies := loginTestPanel(t, ts)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites", nil, cookies)
	pe, _ := listSiteItem(t, out, "entry-off.test")["public_entry"].(map[string]any)
	if pe == nil {
		t.Fatal("命中但停用的规则也要能说明，不能装作没有")
	}
	if pe["available"] != false {
		t.Errorf("停用的规则不能标可用: %v", pe)
	}
	if note, _ := pe["note"].(string); !strings.Contains(note, "停用") {
		t.Errorf("停用要在 note 里说清: %v", pe)
	}
}

// TestSiteOpenPrefersPublicEntryFrontend：前端「打开」不许再硬编码 80。
func TestSiteOpenPrefersPublicEntryFrontend(t *testing.T) {
	js := readAssetJS(t, "sites.js")
	openBody := jsFuncBody(t, js, "siteOpenTarget")
	if !strings.Contains(openBody, "public_entry") || !strings.Contains(openBody, "pe.available") {
		t.Error("siteOpenTarget 必须先看后端的 public_entry.available（否则规则前置的站点又落回 80）")
	}
	// 端口必须经 dial 拼进地址：写死 'http://' + host + '/' 就是回到 80 的写法。
	if !strings.Contains(openBody, "scheme + '://' + host + dial + '/'") {
		t.Error("siteOpenTarget 的兜底地址必须拼上 dial（监听端口），不能在打开处硬编码 80")
	}
	// 「打开」链接只许走这一个函数
	linkBody := jsFuncBody(t, js, "domainLink")
	if !strings.Contains(linkBody, "siteOpenTarget(s)") {
		t.Error("domainLink（「打开」）必须用 siteOpenTarget 算地址")
	}
	if strings.Contains(linkBody, "80") {
		t.Error("domainLink 里出现硬编码 80：端口只能由 siteOpenTarget 决定")
	}
}

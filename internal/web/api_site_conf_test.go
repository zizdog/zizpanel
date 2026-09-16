package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/proxies"
)

// ============================================================================
//  「配置」页直接编辑 vhost 的回归测试（用户 2026-09-17 要求）
//
//  三个必须成立的语义：
//    1. 保存的是**磁盘上那一份**（nginx 真正加载的），内容逐字写进去；
//    2. 写完必须 nginx -t（helper 内）→ reload → 复核新配置里的 listen 端口在应答；
//    3. **任何一步失败都回滚**成修改前的内容，并且保留用户写的内容在界面上
//       （接口只回错误，站点不能变成打不开）。
// ============================================================================

func TestSiteListenPortsParsesCommonForms(t *testing.T) {
	conf := `server {
    listen 80;
    server_name a.test;
}
server {
    listen 127.0.0.1:8889;
    listen [::]:8889;
}
server {
    listen 443 ssl;
    listen 8443 ssl http2;
}
server {
    listen unix:/tmp/not-a-port.sock;
}`
	got := siteListenPorts(conf)
	want := []siteListenPort{{80, false}, {8889, false}, {443, true}, {8443, true}}
	if len(got) != len(want) {
		t.Fatalf("解析出的端口 = %+v，期望 %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 = %+v，期望 %+v", i, got[i], want[i])
		}
	}
}

// ensureSiteForConfTest 走真实接口建一个站点（复核通道已被换成毫秒级假实现）。
func ensureSiteForConfTest(t *testing.T, domain string) (*Server, *httptest.Server, []*http.Cookie) {
	t.Helper()
	srv, ts := newTestServer(t)
	stubSiteApplyChannel(t, "403")
	cookies := loginTestPanel(t, ts)
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{"domain": domain, "rewrite": "none"}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("建站点失败：%v", out)
	}
	return srv, ts, cookies
}

// TestSiteConfSaveWritesExactlyWhatUserTyped 保存成功：内容逐字落盘 + 复核端口。
func TestSiteConfSaveWritesExactlyWhatUserTyped(t *testing.T) {
	_, ts, cookies := ensureSiteForConfTest(t, "confsave.test")
	st := stubSiteApplyChannel(t, "403") // 复核通道：403 = vhost 真的在服务
	const body = "# 用户手写的配置\nserver {\n    listen 8890;\n    server_name confsave.test;\n}\n"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites/confsave.test/conf", map[string]any{"content": body}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("保存应成功，实际 %d：%v", res.StatusCode, out)
	}
	if st.lastWrite() != body {
		t.Errorf("写入内容必须与用户输入逐字一致，实际：\n%q", st.lastWrite())
	}
	if st.probes == 0 {
		t.Error("保存后必须复核新配置里的监听端口真的在应答")
	}
}

// TestSiteConfSaveReloadFailureRollsBack：reload 失败 → 回滚成修改前的内容。
func TestSiteConfSaveReloadFailureRollsBack(t *testing.T) {
	_, ts, cookies := ensureSiteForConfTest(t, "confreload.test")
	st := stubSiteApplyChannel(t, "403")
	const old = "# 修改前的配置\nserver { listen 80; }\n"
	siteReadVhostFn = func(_ *Server, _ string) ([]byte, error) { return []byte(old), nil }
	siteReloadFn = func(_ *Server, _ context.Context) error { return fmt.Errorf("nginx -s reload boom") }

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites/confreload.test/conf",
		map[string]any{"content": "server { listen 8891; }\n"}, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("reload 失败不得返回 2xx：%v", out)
	}
	if st.writeCount() < 2 || st.lastWrite() != old {
		t.Fatalf("reload 失败必须回滚成修改前的内容，写入序列：%q", st.writes)
	}
}

// TestSiteConfSaveProbeFailureRollsBack：reload 返回 0 但新端口没人应答 → 也要回滚。
//
// 这正是 nginx 读配置失败（日志/证书打不开）时的形态：命令成功、站点其实没生效。
func TestSiteConfSaveProbeFailureRollsBack(t *testing.T) {
	_, ts, cookies := ensureSiteForConfTest(t, "confprobe.test")
	st := stubSiteApplyChannel(t, "403")
	const old = "# 旧配置\nserver { listen 80; }\n"
	siteReadVhostFn = func(_ *Server, _ string) ([]byte, error) { return []byte(old), nil }
	siteProbeFn = func(_ context.Context, _, _ string, _ int, _ string, _ time.Duration) (string, string, error) {
		return "", "", fmt.Errorf("connection refused")
	}

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites/confprobe.test/conf",
		map[string]any{"content": "server { listen 8892; }\n"}, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("端口无人应答不得返回 2xx：%v", out)
	}
	if st.lastWrite() != old {
		t.Fatalf("复核失败必须回滚，最后一次写入应为旧配置，实际 %q", st.lastWrite())
	}
}

// TestSiteConfSaveRejectsEmptyContentAndUnknownSite 锁住两个 4xx 边界。
func TestSiteConfSaveRejectsEmptyContentAndUnknownSite(t *testing.T) {
	_, ts, cookies := ensureSiteForConfTest(t, "confempty.test")
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites/confempty.test/conf", map[string]any{"content": "   \n"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("空内容必须 400，实际 %d：%v", res.StatusCode, out)
	}
	if !strings.Contains(fmt.Sprint(out), "不能为空") {
		t.Errorf("错误信息要写清原因，实际 %v", out)
	}
	res2, _, _ := doJSON(t, ts, "POST", "/api/v1/sites/nosuch.test/conf", map[string]any{"content": "server {}\n"}, cookies)
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在的站点必须 404，实际 %d", res2.StatusCode)
	}
}

// ============================================================================
//  vhost「同端口 + 同 server_name」冲突拦截（真机事故 2026-09-17）
//
//  用户把 wp.zizdog.com 的 vhost 存成 `listen 8889 ssl` + 同名，撞上反代规则
//  proxy-9（也是 8889 + 同名）；proxy-9.conf 字典序在前，于是**站点那份被
//  nginx 静默忽略**，8889 的流量落到 blog.zizdog.com。这一组测试锁住"保存时
//  就要拦下来并说清后果"，而不是让用户自己去看一行 warning。
// ============================================================================

func TestVhostIdentitiesPerServerBlock(t *testing.T) {
	conf := `server {
    listen 80;
    server_name a.test b.test;
}
server {
    listen 443 ssl;
    server_name a.test;
}
server {
    listen 8889 ssl default_server;
    server_name _;
}`
	got := vhostIdentities(conf, "x.conf")
	want := map[string]bool{
		"80|a.test": true, "80|b.test": true, "443|a.test": true,
	}
	if len(got) != len(want) {
		t.Fatalf("身份数 = %d（%+v），期望 %d —— 不应把 80 的域名配到 443，也要跳过 server_name _", len(got), got, len(want))
	}
	for _, id := range got {
		key := fmt.Sprintf("%d|%s", id.Port, id.Name)
		if !want[key] {
			t.Errorf("多出/错误的身份 %s", key)
		}
	}
}

func TestSiteConfSaveRejectsServerNameCollision(t *testing.T) {
	srv, ts, cookies := ensureSiteForConfTest(t, "collide.test")
	stubSiteApplyChannel(t, "403")
	// 模拟"已有的反代规则文件"：同端口 + 同 server_name。
	if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	proxyConf := "server {\n    listen 8899 ssl;\n    server_name collide.test;\n}\n"
	if err := os.WriteFile(filepath.Join(srv.Cfg.VhostDir, "proxy-9.conf"), []byte(proxyConf), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "server {\n    listen 8899 ssl;\n    server_name collide.test;\n}\n"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites/collide.test/conf", map[string]any{"content": body}, cookies)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("同端口同 server_name 必须 409，实际 %d：%v", res.StatusCode, out)
	}
	msg := fmt.Sprint(out)
	for _, want := range []string{"proxy-9.conf", "8899", "collide.test", "静默失效"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息里应写清 %q，实际：%v", want, out)
		}
	}
}

func TestSiteConfSaveAllowsDifferentPortOrName(t *testing.T) {
	srv, ts, cookies := ensureSiteForConfTest(t, "ok.test")
	stubSiteApplyChannel(t, "403")
	if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	proxyConf := "server {\n    listen 8899 ssl;\n    server_name other.test;\n}\n"
	if err := os.WriteFile(filepath.Join(srv.Cfg.VhostDir, "proxy-9.conf"), []byte(proxyConf), 0o644); err != nil {
		t.Fatal(err)
	}
	// 同端口不同域名 → 允许（nginx 正常按 SNI/Host 分流）
	body := "server {\n    listen 8899 ssl;\n    server_name ok.test;\n}\n"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites/ok.test/conf", map[string]any{"content": body}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("同端口不同域名应允许保存，实际 %d：%v", res.StatusCode, out)
	}
}

// TestProxySSLPortMixMessageNamesTheRightSide 锁住错误信息的**方向**。
//
// 真机报障（2026-09-17，用户）：他在 8889 上新建一条**没开 HTTPS** 的规则，
// 已有规则（wp.zizdog.com）是 HTTPS，面板却弹「端口 8889 上已有HTTP规则
// 「wp.zizdog.com」，而这条是HTTPS」—— 正好说反，把人往错误方向带。
func TestProxySSLPortMixMessageNamesTheRightSide(t *testing.T) {
	srv, _ := newTestServer(t)
	repo := srv.proxyRepo()
	ctx := context.Background()
	// 已有一条 HTTPS 规则占着 8889。
	httpsRule := &proxies.Rule{Name: "wp.zizdog.com", Listen: 8889, Domains: "wp.zizdog.com",
		Target: "https://127.0.0.1:443", Enabled: true, Websocket: true, SSLEnabled: true,
		SSLProvider: "manual", SSLCert: "/tmp/zp-test.crt", SSLKey: "/tmp/zp-test.key"}
	if _, err := repo.Create(ctx, httpsRule); err != nil {
		t.Fatal(err)
	}
	// 新规则：同端口但没开 HTTPS。
	plain := &proxies.Rule{Name: "te", Listen: 8889, Domains: "te.zizdog.com",
		Target: "https://127.0.0.1:443", Enabled: true, Websocket: true, SSLEnabled: false}
	err := srv.checkProxySSLPortMix(ctx, plain)
	if err == nil {
		t.Fatal("同端口 HTTP/HTTPS 混用必须被拦下")
	}
	msg := err.Error()
	if !strings.Contains(msg, "已有HTTPS规则「wp.zizdog.com」，而这条是HTTP") {
		t.Errorf("信息必须写对方向（已有的是 HTTPS、这条是 HTTP），实际：%s", msg)
	}
	if strings.Contains(msg, "已有HTTP规则") {
		t.Errorf("方向写反了（这正是用户遇到的 bug），实际：%s", msg)
	}
	if !strings.Contains(msg, "让这条也启用 HTTPS") {
		t.Errorf("应把最省事的出路（给这条也开 HTTPS）写在最前面，实际：%s", msg)
	}
}

// TestProxySSLPortMixAllowsSameModeOnSamePort 锁住"同端口同模式"必须放行。
//
// 这条是用户 2026-09-17 卡住的那一步：8889 上已有三条 HTTPS 规则，他新建一条
// **也要 HTTPS** 的规则却保存不了 —— 真正的 bug 在前端（创建请求里没带证书，
// 后端看来它是一条 HTTP 规则）。这里锁住后端本身对"同为 HTTPS"是放行的，
// 免得以后有人把保护改成"同端口只能有一条"。
func TestProxySSLPortMixAllowsSameModeOnSamePort(t *testing.T) {
	srv, _ := newTestServer(t)
	repo := srv.proxyRepo()
	ctx := context.Background()
	if _, err := repo.Create(ctx, &proxies.Rule{
		Name: "wp", Listen: 8889, Domains: "wp.zizdog.com", Target: "https://127.0.0.1:443",
		Enabled: true, Websocket: true, SSLEnabled: true, SSLProvider: "acme",
		SSLCert: "/tmp/a.crt", SSLKey: "/tmp/a.key",
	}); err != nil {
		t.Fatal(err)
	}
	// 另一条 HTTPS 规则，同端口、不同域名 —— 这是 nginx 的标准用法，必须允许。
	other := &proxies.Rule{
		Name: "te", Listen: 8889, Domains: "te.zizdog.com", Target: "https://127.0.0.1:443",
		Enabled: true, Websocket: true, SSLEnabled: true, SSLProvider: "acme",
		SSLCert: "/tmp/a.crt", SSLKey: "/tmp/a.key",
	}
	if err := srv.checkProxySSLPortMix(ctx, other); err != nil {
		t.Fatalf("同端口同模式（都 HTTPS）必须放行，实际: %v", err)
	}
	// 停用的规则也不参与混用判定（这就是 self/mkcert 的出路：先停用建好、绑证书、再启用）。
	disabled := &proxies.Rule{
		Name: "tmp", Listen: 8889, Domains: "tmp.zizdog.com", Target: "http://127.0.0.1:80",
		Enabled: false, Websocket: true,
	}
	if err := srv.checkProxySSLPortMix(ctx, disabled); err != nil {
		t.Fatalf("停用的规则不写配置，不该被混用判定拦下，实际: %v", err)
	}
}

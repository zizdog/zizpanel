package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

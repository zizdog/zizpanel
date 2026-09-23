package web

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestAriaStandalonePortServesUIAndRPC 锁住用户 2026-09-23 的要求：
// AriaNg 要有**独立端口**——界面与 RPC 同源，反代出去就能用（不再受面板会话与
// 混合内容限制）。这里直接打独立端口的 handler：
//
//	① GET /		→ 200 且是 AriaNg（含内置脚本）
//	② POST /jsonrpc	→ 转发到 127.0.0.1:6800（**不要求面板会话**）
func TestAriaStandalonePortServesUIAndRPC(t *testing.T) {
	srv, _ := newTestServer(t)
	writeAria2Conf(t, srv, "s3cret-standalone")

	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"z","result":{"version":"1.37.0"}}`))
	}))
	defer upstream.Close()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	old := aria2RPCUpstream
	aria2RPCUpstream = func() *url.URL { return &url.URL{Scheme: "http", Host: u.Host, Path: "/jsonrpc"} }
	t.Cleanup(func() { aria2RPCUpstream = old })

	ts := httptest.NewServer(srv.ariaStandaloneHandler())
	defer ts.Close()

	// ① 界面（独立端口根路径，不需要任何 Cookie）
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("独立端口根路径应 200，实际 %d：%s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "aria-ng-a5324ae04a.min.js") {
		t.Errorf("根路径应返回 AriaNg 界面，实际：%s", string(body)[:min(200, len(body))])
	}

	// ② RPC 代理：不带面板会话也必须通（边界是"只绑回环"，不是会话）
	payload := `{"jsonrpc":"2.0","id":"z","method":"aria2.getVersion","params":["token:x"]}`
	res2, err := http.Post(ts.URL+"/jsonrpc", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(res2.Body)
	_ = res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("独立端口的 RPC 代理应 200（不要求面板会话），实际 %d：%s", res2.StatusCode, body2)
	}
	if gotPath != "/jsonrpc" || gotBody != payload {
		t.Errorf("上游应收到原样请求：path=%q body=%q", gotPath, gotBody)
	}
	if !strings.Contains(string(body2), "1.37.0") {
		t.Errorf("上游响应必须回给浏览器，实际 %s", body2)
	}
}

// TestAriaUIListenerReportsPortConflictHonestly 端口被占用时如实报错，且不静默换端口。
func TestAriaUIListenerReportsPortConflictHonestly(t *testing.T) {
	srv, _ := newTestServer(t)
	// 先占一个端口，再让监听器去绑同一个端口。
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	if err := srv.ApplyAriaUIListener(port); err == nil {
		t.Fatal("端口被占用时必须返回错误（不许静默换端口）")
	}
	st := srv.AriaUIListenerState()
	if st.Running {
		t.Error("绑定失败时不应报告 Running")
	}
	if !strings.Contains(st.Err, "已被占用") {
		t.Errorf("错误里要说清是端口被占用，实际：%q", st.Err)
	}
}

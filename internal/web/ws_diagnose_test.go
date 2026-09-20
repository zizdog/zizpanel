package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================================
//  WebSocket 升级失败结论门禁（坑 219）
//
//  判据：反代痕迹 + 逐跳头缺失 ⇒ "去开 WebSocket 透传"；直连缺头 ⇒ 换浏览器/直连；
//  正常握手一个字都不受影响（仍然 101）。
// ============================================================================

// TestWSUpgradeFailureTellsUserWhatToDo 锁住分类器。
func TestWSUpgradeFailureTellsUserWhatToDo(t *testing.T) {
	cases := []struct {
		name     string
		trail    bool
		err      error
		advice   string
		viaProxy bool
	}{
		{"反代痕迹+缺 Connection", true, errWSNoConnectionUpgrade, wsAdviceProxy, true},
		{"反代痕迹+Upgrade 头被改写", true, errWSUpgradeNotWebsocket, wsAdviceProxy, true},
		{"无痕迹+缺 Connection", false, errWSNoConnectionUpgrade, wsAdviceDirect, false},
		{"无痕迹+Upgrade 头被改写", false, errWSUpgradeNotWebsocket, wsAdviceDirect, false},
		{"反代痕迹+版本不支持", true, fmt.Errorf("%w: 12（需要 13）", errWSBadVersion), wsAdviceOther, true},
		{"无痕迹+缺 Key", false, errWSMissingKey, wsAdviceOther, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/terminal/ws", nil)
			if c.trail {
				r.Header.Set("X-Forwarded-For", "10.0.0.9")
			}
			v := wsUpgradeFailure(r, c.err)
			if v.Advice != c.advice {
				t.Errorf("结论首行 = %q，期望 %q", v.Advice, c.advice)
			}
			if v.ViaProxy != c.viaProxy {
				t.Errorf("via_proxy = %v，期望 %v", v.ViaProxy, c.viaProxy)
			}
			if v.Detail == "" {
				t.Error("结论必须带细节（折叠项要显示它）")
			}
			if n := len([]rune(v.Advice)); n > 40 {
				t.Errorf("结论首行必须 ≤40 字，实际 %d 字：%q", n, v.Advice)
			}
		})
	}

	// 两类结论都必须能照着做：反代类要点出 WebSocket 透传与那两个头，
	// 直连类要点出"换浏览器/直连"。
	proxyReq := httptest.NewRequest(http.MethodGet, "/api/v1/terminal/ws", nil)
	proxyReq.Header.Set("Forwarded", "for=10.0.0.9")
	pv := wsUpgradeFailure(proxyReq, errWSNoConnectionUpgrade)
	for _, need := range []string{"WebSocket 透传", "proxy_set_header Upgrade", "proxy_set_header Connection", "map $http_upgrade"} {
		if !strings.Contains(pv.Detail, need) {
			t.Errorf("反代类细节里缺少可行动信息 %q：%s", need, pv.Detail)
		}
	}
	dv := wsUpgradeFailure(httptest.NewRequest(http.MethodGet, "/api/v1/terminal/ws", nil), errWSNoConnectionUpgrade)
	if !strings.Contains(dv.Detail, "浏览器") || !strings.Contains(dv.Detail, "直连") {
		t.Errorf("直连类细节必须给换浏览器/直连出路：%s", dv.Detail)
	}
}

// TestHasProxyTrail 逐个头都要认（反代实现五花八门）；空值不算。
func TestHasProxyTrail(t *testing.T) {
	for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded", "X-Real-IP"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if hasProxyTrail(r) {
			t.Errorf("%s 未设置却判成有反代痕迹", k)
		}
		r.Header.Set(k, "10.0.0.9")
		if !hasProxyTrail(r) {
			t.Errorf("%s 有值却没认出来", k)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "   ")
	if hasProxyTrail(r) {
		t.Error("空白头不算反代痕迹")
	}
}

// TestWSUpgradeStillWorksWithProperHandshake 正常升级不受影响（回归负向对照）。
func TestWSUpgradeStillWorksWithProperHandshake(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		ws, err := upgradeWebSocket(w, r)
		if err != nil {
			t.Errorf("正常握手被判失败：%v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = ws.Close()
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("连接测试服务器失败：%v", err)
	}
	defer func() { _ = conn.Close() }()
	req := "GET /api/v1/terminal/ws HTTP/1.1\r\n" +
		"Host: " + strings.TrimPrefix(srv.URL, "http://") + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("发握手失败：%v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("读握手响应失败：%v", err)
	}
	if !strings.Contains(line, "101") {
		t.Fatalf("正常握手应回 101 Switching Protocols，实际：%q（handler 被调用 %d 次）", line, hits)
	}
}

// TestTerminalWSRejectTellsUserWhatToDo 端到端：握手被拒时接口/诊断接口都带可行动结论。
func TestTerminalWSRejectTellsUserWhatToDo(t *testing.T) {
	for _, c := range []struct {
		name   string
		trail  string
		advice string
	}{
		{"经反代", "10.0.0.9", wsAdviceProxy},
		{"直连", "", wsAdviceDirect},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, ts := newTestServer(t)
			// 终端默认关闭；这里只走到握手校验，不会真的起 shell。
			srv.Cfg.TerminalEnabled = true
			cookies := loginTestPanel(t, ts)

			get := func(path string) (int, string) {
				t.Helper()
				req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, ck := range cookies {
					req.AddCookie(ck)
				}
				if c.trail != "" {
					req.Header.Set("X-Forwarded-For", c.trail)
				}
				res, err := ts.Client().Do(req)
				if err != nil {
					t.Fatalf("GET %s 失败：%v", path, err)
				}
				defer func() { _ = res.Body.Close() }()
				var body struct {
					Msg string `json:"msg"`
				}
				_ = json.NewDecoder(res.Body).Decode(&body)
				return res.StatusCode, body.Msg
			}

			code, msg := get("/api/v1/terminal/ws")
			if code != http.StatusBadRequest {
				t.Fatalf("缺 Connection: Upgrade 的 WS 请求应回 400，实际 %d", code)
			}
			if !strings.HasPrefix(msg, c.advice) {
				t.Errorf("错误体首行应为 %q，实际：%q", c.advice, msg)
			}

			// 前端就是靠这个接口把结论取回去显示的（结构见下一个用例）。
			code, _ = get("/api/v1/terminal/ws-diagnose")
			if code != http.StatusOK {
				t.Fatalf("诊断接口应 200，实际 %d", code)
			}
		})
	}
}

// TestTerminalWSDiagnoseAdvice 诊断接口返回结构（前端直接读 advice/detail）。
func TestTerminalWSDiagnoseAdvice(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.Cfg.TerminalEnabled = true
	cookies := loginTestPanel(t, ts)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/terminal/ws-diagnose", nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			ViaProxy bool   `json:"via_proxy"`
			Advice   string `json:"advice"`
			Detail   string `json:"detail"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !body.OK || !body.Data.ViaProxy {
		t.Fatalf("带 X-Forwarded-Proto 的诊断应判为经反代：%+v", body)
	}
	if body.Data.Advice != wsAdviceProxy || body.Data.Detail == "" {
		t.Errorf("诊断结论不对：%+v", body.Data)
	}
}

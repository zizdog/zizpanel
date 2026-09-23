package web

// api_peers_test.go —— 多机管理（C5）的门禁。
//
// 纪律：这里**绝不发真实网络请求** —— 联网那一段用 httptest 的本地服务器验证，
// 其余路径用注入的假拉取函数。重点是安全边界：没开凭证不许读、凭证错了不许读、
// 非回环的 http 一律拒绝、https 必须固定证书指纹。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/config"
)

// dataInto 把 doJSON 返回的 data 段解成具体类型（测试里读起来比 map 断言清楚）。
func dataInto(t *testing.T, out map[string]any, dst any) {
	t.Helper()
	raw, err := json.Marshal(out["data"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("data 解不出来（%s）：%v", string(raw), err)
	}
}

// TestAgentSummaryNeedsToken 子机侧：默认关闭、凭证错了拒绝、凭证对了才给摘要。
func TestAgentSummaryNeedsToken(t *testing.T) {
	srv, _ := newTestServer(t)

	call := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/summary", nil)
		if token != "" {
			req.Header.Set("X-ZizPanel-Agent", token)
		}
		rec := httptest.NewRecorder()
		srv.handleAgentSummary(rec, req)
		return rec
	}

	// ① 默认关闭：任何请求都 403
	if rec := call("whatever"); rec.Code != http.StatusForbidden {
		t.Fatalf("没开启时必须 403，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(strings.ToLower(call("whatever").Body.String()), "zizpanel") {
		t.Error("403 响应里不该出现面板字样")
	}

	// ② 开了之后：没凭证/错凭证都拒绝
	srv.Cfg.AgentToken = "correct-token-abcdef"
	if rec := call(""); rec.Code != http.StatusForbidden {
		t.Errorf("没带凭证必须 403，实际 %d", rec.Code)
	}
	if rec := call("wrong-token"); rec.Code != http.StatusForbidden {
		t.Errorf("凭证不对必须 403，实际 %d", rec.Code)
	}

	// ③ 凭证对：拿到摘要，且**只有聚合数字**
	rec := call("correct-token-abcdef")
	if rec.Code != http.StatusOK {
		t.Fatalf("凭证对应当 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK   bool         `json:"ok"`
		Data agentSummary `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Data.Panel != "zizpanel" || out.Data.At == "" {
		t.Errorf("摘要形状不对：%+v", out.Data)
	}
	body := strings.ToLower(rec.Body.String())
	for _, leak := range []string{"password", "privkey", "/users/", "api_token"} {
		if strings.Contains(body, leak) {
			t.Errorf("摘要里不该出现 %q：%s", leak, rec.Body.String())
		}
	}
}

// TestValidatePeerURLRejectsUnsafe 地址与指纹的搭配：明文只放行回环，https 必须给指纹。
func TestValidatePeerURLRejectsUnsafe(t *testing.T) {
	fp := strings.Repeat("ab", 32) // 64 位十六进制
	cases := []struct {
		name, url, fp string
		bad           bool
		want          string
	}{
		{"https 没指纹", "https://m4.lan:8443/ab/", "", true, "指纹"},
		{"https 指纹长度不对", "https://m4.lan:8443/ab/", "abcd", true, "64 位"},
		{"https 带指纹", "https://m4.lan:8443/ab/", fp, false, ""},
		{"局域网 http", "http://192.0.2.9:8443/ab/", "", true, "http"},
		{"公网 http", "http://example.com/ab/", "", true, "http"},
		{"回环 http", "http://127.0.0.1:18443/dev/", "", false, ""},
		{"localhost http", "http://localhost:18443/dev/", "", false, ""},
		{"协议不对", "ftp://m4.lan/", fp, true, "http"},
		{"带查询串", "https://m4.lan/ab/?x=1", fp, true, "参数"},
		{"缺 host", "https:///ab/", fp, true, "完整"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePeerURL(c.url, c.fp)
			if c.bad {
				if err == nil {
					t.Fatalf("应当拒绝：%s", c.url)
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("错误里应当提到 %q，实际 %q", c.want, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("不该拒绝：%v", err)
			}
		})
	}
	if got := normalizeFingerprint("AB:CD:EF"); got != "abcdef" {
		t.Errorf("指纹规范化不对：%q", got)
	}
}

// TestFetchPeerSummaryPinsCertificate https 子机必须**固定指纹**（自签证书下唯一的信任锚）。
func TestFetchPeerSummaryPinsCertificate(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dev/api/v1/agent/summary" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-ZizPanel-Agent") != "tok" {
			fail(w, http.StatusForbidden, "凭证不对")
			return
		}
		ok(w, agentSummary{Panel: "zizpanel", Version: "9.9.9", At: "now"})
	})
	tlsSrv := httptest.NewTLSServer(handler)
	defer tlsSrv.Close()
	plainSrv := httptest.NewServer(handler)
	defer plainSrv.Close()

	sum := sha256.Sum256(tlsSrv.Certificate().Raw)
	rightFp := hex.EncodeToString(sum[:])

	good := config.Peer{URL: tlsSrv.URL + "/dev/", Token: "tok", Fingerprint: rightFp}
	raw, err := fetchPeerSummary(context.Background(), good)
	if err != nil {
		t.Fatalf("指纹正确时应当取到摘要：%v", err)
	}
	if !strings.Contains(string(raw), "9.9.9") {
		t.Errorf("摘要内容不对：%s", raw)
	}

	bad := good
	bad.Fingerprint = strings.Repeat("00", 32)
	if _, err := fetchPeerSummary(context.Background(), bad); err == nil {
		t.Fatal("指纹不匹配必须拒绝连接")
	} else if !strings.Contains(err.Error(), "指纹") {
		t.Errorf("错误要说清是指纹问题：%v", err)
	}

	// 非回环的 http 一律拒绝（凭证不能明文过网）
	if _, err := fetchPeerSummary(context.Background(), config.Peer{
		URL: "http://192.0.2.9:18443/dev/", Token: "tok",
	}); err == nil {
		t.Error("非回环 http 必须拒绝")
	}

	// 回环 http 放行
	if raw, err := fetchPeerSummary(context.Background(), config.Peer{
		URL: plainSrv.URL + "/dev/", Token: "tok",
	}); err != nil {
		t.Errorf("回环 http 应当放行：%v", err)
	} else if !strings.Contains(string(raw), "9.9.9") {
		t.Errorf("摘要内容不对：%s", raw)
	}

	// 子机返回 403：必须如实报错（不许当成"空摘要"）
	if _, err := fetchPeerSummary(context.Background(), config.Peer{
		URL: plainSrv.URL + "/dev/", Token: "wrong",
	}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("子机 403 时应当如实报错：%v", err)
	}
}

// peerListBody 是列/增/刷三个接口的返回形状。
type peerListBody struct {
	Peers []peerSummaryView `json:"peers"`
	Self  struct {
		Enabled bool   `json:"enabled"`
		Entry   string `json:"entry"`
		URLHint string `json:"url_hint"`
		Token   string `json:"token"`
	} `json:"self"`
}

// TestPeerCRUDAndHonestFailure 主面板侧：加/列/删 + 拉取失败如实记录。
func TestPeerCRUDAndHonestFailure(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	mode := "ok"
	srv.peerFetchFn = func(_ context.Context, _ config.Peer) (json.RawMessage, error) {
		if mode == "fail" {
			return nil, context.DeadlineExceeded
		}
		return json.RawMessage(`{"panel":"zizpanel","version":"1.2.3"}`), nil
	}

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/peers",
		map[string]any{"name": "m4-mini", "url": "http://127.0.0.1:18443/dev/", "token": "tok-1234"}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("添加子机应当成功，实际 %d：%v", res.StatusCode, out)
	}
	var list peerListBody
	dataInto(t, out, &list)
	if len(list.Peers) != 1 {
		t.Fatalf("应当有一台子机：%+v", list.Peers)
	}
	p := list.Peers[0]
	if p.Name != "m4-mini" || p.LastAt == "" || len(p.Summary) == 0 {
		t.Errorf("加完应当立刻拉到摘要：%+v", p)
	}
	// 凭证只留尾巴（界面不需要原样看到别人的 token）
	if strings.Contains(string(p.Summary), "tok-1234") || strings.Contains(p.MaskedToken, "tok-1234") {
		t.Errorf("不该把子机凭证原样返回：%+v", p)
	}
	if !strings.HasSuffix(p.MaskedToken, "1234") {
		t.Errorf("掩码应当留尾巴几位：%q", p.MaskedToken)
	}

	// 拉取失败：LastError 如实写
	mode = "fail"
	_, out, _ = doJSON(t, ts, "POST", "/api/v1/peers/refresh", map[string]any{}, cookies)
	dataInto(t, out, &list)
	if list.Peers[0].LastError == "" {
		t.Error("拉取失败必须记进 LastError（界面据此显示「没连上」）")
	}

	// 本机接入信息：没开凭证时不给 token
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/peers", nil, cookies)
	dataInto(t, out, &list)
	if list.Self.Enabled || list.Self.Token != "" {
		t.Errorf("默认不该开启「被主面板管理」：%+v", list.Self)
	}
	if list.Self.URLHint == "" || !strings.HasSuffix(list.Self.URLHint, "/") {
		t.Errorf("接入地址提示要带面板入口：%q", list.Self.URLHint)
	}

	// 删掉
	id := p.ID
	res, _, _ = doJSON(t, ts, "DELETE", "/api/v1/peers/"+itoa64(id), nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删除应当成功，实际 %d", res.StatusCode)
	}
	_, out, _ = doJSON(t, ts, "POST", "/api/v1/peers/refresh", map[string]any{}, cookies)
	dataInto(t, out, &list)
	if len(list.Peers) != 0 {
		t.Errorf("删掉之后不该还有子机：%+v", list.Peers)
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// TestAgentTokenLifecycle 本机开关：开启给凭证、重新生成会换、关掉即吊销。
func TestAgentTokenLifecycle(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	var out struct {
		Enabled bool   `json:"enabled"`
		Token   string `json:"token"`
	}
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/agent/token", map[string]any{"enabled": true}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("开启应当成功，实际 %d：%v", res.StatusCode, body)
	}
	dataInto(t, body, &out)
	if !out.Enabled || len(out.Token) < 32 {
		t.Fatalf("开启后应当有够长的凭证：%+v", out)
	}
	first := out.Token

	_, body, _ = doJSON(t, ts, "POST", "/api/v1/agent/token", map[string]any{"enabled": true}, cookies)
	dataInto(t, body, &out)
	if out.Token == first {
		t.Error("再次开启应当换一个新凭证（旧主面板立即失效）")
	}

	_, body, _ = doJSON(t, ts, "POST", "/api/v1/agent/token", map[string]any{"enabled": false}, cookies)
	dataInto(t, body, &out)
	if out.Enabled || srv.Cfg.AgentToken != "" {
		t.Error("关闭后凭证必须清空")
	}
}

// TestPeerAddRejectsBadInput 校验不过必须 400 + 人话，绝不"先存下再说"。
func TestPeerAddRejectsBadInput(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)
	srv.peerFetchFn = func(context.Context, config.Peer) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}
	for _, body := range []map[string]any{
		{"name": "", "url": "http://127.0.0.1:1/", "token": "t"},
		{"name": "a", "url": "http://127.0.0.1:1/", "token": ""},
		{"name": "a", "url": "http://198.51.100.5:1/", "token": "t"},
		{"name": "a", "url": "https://m4.lan/", "token": "t"},
	} {
		if res, _, _ := doJSON(t, ts, "POST", "/api/v1/peers", body, cookies); res.StatusCode != http.StatusBadRequest {
			t.Errorf("应当 400，实际 %d（%v）", res.StatusCode, body)
		}
	}
	if len(srv.Cfg.Peers) != 0 {
		t.Errorf("校验不过时不该写进配置：%+v", srv.Cfg.Peers)
	}
}

// TestPeerAdviceTellsUserWhatToDo 失败建议必须**贴合原因**，拿不准就不给建议（别把人带偏）。
func TestPeerAdviceTellsUserWhatToDo(t *testing.T) {
	cases := []struct {
		err  string
		want string // 建议里必须出现的关键词（空 = 必须不给建议）
	}{
		{"子机返回 HTTP 403（{\"msg\":\"凭证不对\"）", "重新生成"},
		{"子机证书指纹不匹配（期望 abcd…，实际 1234…）：换过证书就要在面板里更新指纹", "指纹"},
		{"只有 127.0.0.1 / localhost 允许用 http（凭证明文过网会被局域网里任何人抄走）", "https"},
		{"连不上子机：dial tcp 192.0.2.9:8443: connect: connection refused", "浏览器"},
		{"子机返回 HTTP 500（internal error）", "日志"},
		{"子机响应不是面板的摘要格式（地址可能指到了别的服务）", "面板入口"},
		{"some totally unknown failure", ""},
	}
	for _, c := range cases {
		got := peerAdvice(errors.New(c.err))
		if c.want == "" {
			if got != "" {
				t.Errorf("拿不准的错误不该给建议，实际 %q（错误：%s）", got, c.err)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("建议里应当提到 %q，实际 %q（错误：%s）", c.want, got, c.err)
		}
	}
	if peerAdvice(nil) != "" {
		t.Error("没有错误就没有建议")
	}
}

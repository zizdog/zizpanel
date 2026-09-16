package web

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================================
//  操作审计接口的测试
//
//  重点覆盖三件容易出错的事：
//    1. **游标分页不能漂移**（这是当初不用 OFFSET 的原因，要断言"翻页不重不漏"）
//    2. **LIKE 通配符必须被转义**（不转义时用户搜 `%` 会匹配一切，看起来像搜索失灵）
//    3. **导出要受限、要留痕**（整段带走审计日志本身是敏感操作）
// ============================================================================

// seedAudit 清空并写入可预期的审计数据，返回 cookie。
//
// 注意必须先清空：setup/login 自己也会写审计记录，留着会让条数断言变成
// "跟环境有关"的脆弱测试。
func seedAudit(t *testing.T, srv *Server, ts *httptest.Server) []*http.Cookie {
	t.Helper()
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if _, err := srv.Store.DB().Exec(`DELETE FROM audit_logs`); err != nil {
		t.Fatal(err)
	}

	rows := []struct {
		ts, actor, ip, action, target, detail string
		ok                                    int
		msg                                   string
	}{
		{"2026-09-01 10:00:00", "admin", "10.0.0.1", "site_create", "a.com", "PHP 8.3", 1, ""},
		{"2026-09-02 11:00:00", "admin", "10.0.0.1", "site_create", "b.com", "PHP 8.3", 1, ""},
		{"2026-09-03 12:00:00", "admin", "10.0.0.2", "site_delete", "a.com", "", 1, ""},
		{"2026-09-04 13:00:00", "admin", "10.0.0.2", "login", "", "", 0, "密码错误"},
		{"2026-09-05 14:00:00", "ops", "10.0.0.3", "login", "", "", 1, ""},
		{"2026-09-06 15:00:00", "ops", "10.0.0.3", "upgrade_apply", "v0.3.5", "来源 remote", 0, "看门狗回滚"},
		{"2026-09-07 16:00:00", "admin", "10.0.0.1", "file_delete", "/tmp/x", "含百分号 100% 的记录", 1, ""},
		// 诱饵：动作名里没有下划线。用来证明 `_` 被当成字面量而不是通配符 ——
		// 若转义失效，搜 `_create` 会把它（siteXcreate）一起匹配上。
		{"2026-09-08 09:00:00", "admin", "10.0.0.1", "siteXcreate", "c.com", "诱饵记录", 1, ""},
	}
	for i, r := range rows {
		if _, err := srv.Store.DB().Exec(
			`INSERT INTO audit_logs(ts,actor,ip,action,target,detail,ok,message) VALUES(?,?,?,?,?,?,?,?)`,
			r.ts, r.actor, r.ip, r.action, r.target, r.detail, r.ok, r.msg); err != nil {
			t.Fatalf("插入第 %d 行失败: %v", i, err)
		}
	}
	return cookies
}

// rawGet 发一个带 cookie 的原始 GET（导出接口返回的不是 JSON，需要拿到原始 body 与响应头）。
func rawGet(t *testing.T, ts *httptest.Server, path string, cookies []*http.Cookie) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func TestAuditListFilterAndCursor(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := seedAudit(t, srv, ts)

	// ---- 默认列表：倒序、带总数 ----
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/audit?limit=3", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("审计列表应 200，实际 %d: %v", res.StatusCode, out)
	}
	data := out["data"].(map[string]any)
	list := data["list"].([]any)
	if len(list) != 3 {
		t.Fatalf("limit=3 应返回 3 条，实际 %d", len(list))
	}
	if data["total"] != float64(8) {
		t.Errorf("总数应为 8，实际 %v", data["total"])
	}
	if data["failed"] != float64(2) {
		t.Errorf("失败数应为 2，实际 %v", data["failed"])
	}
	first := list[0].(map[string]any)
	if first["action"] != "siteXcreate" || first["ts"] != "2026-09-08 09:00:00" {
		t.Errorf("最新一条（id 最大）应排在最前，实际 %v / %v", first["action"], first["ts"])
	}
	if data["has_more"] != true {
		t.Error("还有更多时应 has_more=true")
	}

	// ---- 游标分页：不重不漏 ----
	seen := map[string]bool{}
	cursor := int64(data["next_before_id"].(float64))
	pages := 0
	for cursor > 0 && pages < 10 {
		pages++
		_, out, _ := doJSON(t, ts, "GET",
			fmt.Sprintf("/api/v1/audit?limit=2&before_id=%d", cursor), nil, cookies)
		d := out["data"].(map[string]any)
		page := d["list"].([]any)
		for _, it := range page {
			e := it.(map[string]any)
			key := fmt.Sprintf("%v|%v", e["ts"], e["action"])
			if seen[key] {
				t.Errorf("翻页出现重复记录: %s", key)
			}
			seen[key] = true
		}
		if d["has_more"] != true {
			break
		}
		cursor = int64(d["next_before_id"].(float64))
	}
	if len(seen) != 5 {
		// 共 8 条，首页 limit=3 拿掉 3 条，剩下的 5 条应该被翻到（且不重复）
		t.Errorf("首页 3 条之后应能翻到剩余 5 条（且不重复），实际 %d 条", len(seen))
	}

	// ---- 按动作精确筛选 ----
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/audit?action=site_create", nil, cookies)
	d := out["data"].(map[string]any)
	if d["total"] != float64(2) {
		t.Errorf("action=site_create 应 2 条，实际 %v", d["total"])
	}
	for _, it := range d["list"].([]any) {
		if it.(map[string]any)["action"] != "site_create" {
			t.Errorf("筛选后混入了别的动作: %v", it)
		}
	}

	// ---- 只看失败 ----
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/audit?ok=0", nil, cookies)
	d = out["data"].(map[string]any)
	if d["total"] != float64(2) {
		t.Errorf("ok=0 应 2 条，实际 %v", d["total"])
	}

	// ---- 关键词（跨多个字段）----
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/audit?q=%E7%9C%8B%E9%97%A8%E7%8B%97", nil, cookies) // 看门狗
	d = out["data"].(map[string]any)
	if d["total"] != float64(1) {
		t.Errorf("关键词“看门狗”应命中 1 条（在 message 里），实际 %v", d["total"])
	}

	// ---- 时间范围（含纯日期补齐到当天末尾）----
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/audit?from=2026-09-02&to=2026-09-04", nil, cookies)
	d = out["data"].(map[string]any)
	if d["total"] != float64(3) {
		t.Errorf("9/2~9/4 应 3 条（含 9/4 全天），实际 %v", d["total"])
	}

	// ---- 操作者 ----
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/audit?actor=ops", nil, cookies)
	d = out["data"].(map[string]any)
	if d["total"] != float64(2) {
		t.Errorf("actor=ops 应 2 条，实际 %v", d["total"])
	}
}

// TestAuditLikeWildcardEscaped 用户输入 `%` 不能变成"匹配一切"。
//
// 不转义时 `q=%` 会被 SQL 当成通配符，于是"搜一个百分号"返回全部记录 ——
// 用户会以为搜索坏了。这是搜索类接口最典型的一个坑。
func TestAuditLikeWildcardEscaped(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := seedAudit(t, srv, ts)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/audit?q=%25", nil, cookies) // q=%
	d := out["data"].(map[string]any)
	if d["total"] != float64(1) {
		t.Errorf("搜 %% 应只命中那条含百分号的记录（1 条），实际 %v（说明通配符没转义）", d["total"])
	}

	// 下划线同理：搜 `_create` 只应命中 site_create（2 条），
	// 不能把诱饵 siteXcreate 也算进去（那是"下划线当任意单字符"的行为）。
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/audit?q=_create", nil, cookies)
	d = out["data"].(map[string]any)
	if d["total"] != float64(2) {
		t.Errorf("搜 _create 应只命中 site_create 的 2 条，实际 %v（若把 siteXcreate 也算上说明通配符没转义）", d["total"])
	}
}

func TestAuditRejectsBadParams(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	cases := []struct{ path, wantMsg string }{
		{"/api/v1/audit?ok=maybe", "ok"},
		{"/api/v1/audit?from=昨天", "时间格式"},
		{"/api/v1/audit/export?format=xml", "format"},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "GET", c.path, nil, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400，实际 %d", c.path, res.StatusCode)
			continue
		}
		if msg := asString(out["msg"]); !strings.Contains(msg, c.wantMsg) {
			t.Errorf("%s 的错误信息应包含 %q，实际: %s", c.path, c.wantMsg, msg)
		}
	}

	// limit 超上限要被夹住，而不是原样放大
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/audit?limit=99999", nil, cookies)
	if got := out["data"].(map[string]any)["limit"]; got != float64(auditMaxLimit) {
		t.Errorf("limit 应被夹到 %d，实际 %v", auditMaxLimit, got)
	}
}

func TestAuditFacets(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := seedAudit(t, srv, ts)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/audit/facets", nil, cookies)
	if out["ok"] != true {
		t.Fatalf("facets 应成功: %v", out)
	}
	d := out["data"].(map[string]any)
	actions := d["actions"].([]any)
	if len(actions) != 6 {
		t.Errorf("应有 6 种动作，实际 %d: %v", len(actions), actions)
	}
	// 计数要正确；并列时顺序不做断言（SQL 只保证按 count 降序，同数之间是任意的）
	byValue := map[string]int{}
	for _, a := range actions {
		m := a.(map[string]any)
		byValue[m["value"].(string)] = int(m["count"].(float64))
	}
	if byValue["site_create"] != 2 || byValue["login"] != 2 || byValue["file_delete"] != 1 {
		t.Errorf("动作计数不对: %v", byValue)
	}
	if top := actions[0].(map[string]any); top["count"] != float64(2) {
		t.Errorf("出现次数最多的应排在最前（count=2），实际 %v", top)
	}
	actors := d["actors"].([]any)
	if len(actors) != 2 {
		t.Errorf("应有两个操作者（admin/ops），实际 %d", len(actors))
	}
}

func TestAuditExportCSVAndJSON(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := seedAudit(t, srv, ts)

	// ---- CSV ----
	res, body := rawGet(t, ts, "/api/v1/audit/export?format=csv&action=site_create", cookies)
	if res.StatusCode != 200 {
		t.Fatalf("CSV 导出应 200，实际 %d", res.StatusCode)
	}
	if !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Error("导出应带 attachment 下载头")
	}
	if !strings.HasPrefix(body, "\ufeff") {
		t.Error("CSV 应带 UTF-8 BOM（否则 Excel 打开中文乱码）")
	}
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("CSV 解析失败: %v", err)
	}
	if len(recs) != 3 { // 表头 + 2 条
		t.Fatalf("CSV 应有 表头+2 行，实际 %d 行", len(recs))
	}
	if recs[0][0] != "时间" || recs[0][3] != "动作" {
		t.Errorf("表头不对: %v", recs[0])
	}

	// ---- JSON ----
	res, body = rawGet(t, ts, "/api/v1/audit/export?format=json&ok=0", cookies)
	if res.StatusCode != 200 {
		t.Fatalf("JSON 导出应 200，实际 %d", res.StatusCode)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("JSON 导出不是合法 JSON: %v", err)
	}
	if doc["count"] != float64(2) {
		t.Errorf("JSON 导出 count 应为 2，实际 %v", doc["count"])
	}

	// ---- 导出本身要留痕 ----
	var n int
	if err := srv.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action = 'audit_export'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("两次导出应留下 2 条 audit_export 记录，实际 %d（导出审计日志本身是敏感操作，必须留痕）", n)
	}
}

func TestAuditRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t)
	for _, p := range []string{"/api/v1/audit", "/api/v1/audit/facets", "/api/v1/audit/export"} {
		res, _, _ := doJSON(t, ts, "GET", p, nil, nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 未登录应 401，实际 %d", p, res.StatusCode)
		}
	}
}

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// api_nav_test.go —— 导航页 HTTP 层的单测。
//
// 覆盖：CRUD、URL/图标校验（javascript: 被拒）、排序、导入导出往返一致、
// 未登录的只读/写入行为、独立别名页在安全后缀之外可匿名访问。
// 全部走 httptest，不碰真实服务、真实 nginx、真实家目录。

const navTestPass = "zizpanel-test-fixture-pass"

func setupNavPanel(t *testing.T) (*Server, *httptest.Server, []*http.Cookie) {
	t.Helper()
	srv, ts := newTestServer(t)
	res, out, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": navTestPass}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("初始化失败 %d: %v", res.StatusCode, out)
	}
	if len(cookies) == 0 {
		t.Fatal("初始化后应下发会话 Cookie")
	}
	return srv, ts, cookies
}

func navCreateGroup(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, name string) map[string]any {
	t.Helper()
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/nav/groups", map[string]any{"name": name}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("创建分组 %q 失败 %d: %v", name, res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("创建分组响应缺少 data: %v", out)
	}
	return data
}

func navCreateItem(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, payload map[string]any) map[string]any {
	t.Helper()
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/nav/items", payload, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("创建站点失败 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("创建站点响应缺少 data: %v", out)
	}
	return data
}

// TestNavRequiresAuth：全部 /api/v1/nav/* 未登录必须 401（写操作也不例外）。
func TestNavRequiresAuth(t *testing.T) {
	_, ts, _ := setupNavPanel(t)
	cases := []struct{ method, path string }{
		{"GET", "/api/v1/nav/tree"},
		{"GET", "/api/v1/nav/groups"},
		{"POST", "/api/v1/nav/groups"},
		{"POST", "/api/v1/nav/groups/reorder"},
		{"PUT", "/api/v1/nav/groups/1"},
		{"DELETE", "/api/v1/nav/groups/1"},
		{"GET", "/api/v1/nav/items"},
		{"POST", "/api/v1/nav/items"},
		{"POST", "/api/v1/nav/items/reorder"},
		{"PUT", "/api/v1/nav/items/1"},
		{"DELETE", "/api/v1/nav/items/1"},
		{"GET", "/api/v1/nav/export"},
		{"POST", "/api/v1/nav/import"},
	}
	for _, c := range cases {
		res, _, _ := doJSON(t, ts, c.method, c.path, map[string]any{"name": "x"}, nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未登录应 401，实际 %d", c.method, c.path, res.StatusCode)
		}
	}
}

// TestNavCRUDAndAudit：建组 → 建站点 → 改 → 排序 → 删；并确认写操作进了审计。
func TestNavCRUDAndAudit(t *testing.T) {
	srv, ts, cookies := setupNavPanel(t)

	tools := navCreateGroup(t, ts, cookies, "常用工具")
	media := navCreateGroup(t, ts, cookies, "影音")
	tid := int64(tools["id"].(float64))
	mid := int64(media["id"].(float64))

	item := navCreateItem(t, ts, cookies, map[string]any{
		"group_id": tid, "name": "ZizPanel", "url": "https://panel.example.com",
		"icon": "🛠️", "description": "面板", "open_new_tab": true,
	})
	iid := int64(item["id"].(float64))
	if item["url"] != "https://panel.example.com" || item["open_new_tab"] != true {
		t.Fatalf("创建站点返回不对: %v", item)
	}

	// 整棵树：两个组、一个站点
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/nav/tree", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("tree 失败 %d", res.StatusCode)
	}
	tree := out["data"].(map[string]any)
	if len(tree["groups"].([]any)) != 2 || len(tree["items"].([]any)) != 1 {
		t.Fatalf("tree 数量不对: %v", tree)
	}

	// 改：改名 + 换组 + 关闭新标签
	res, out, _ = doJSON(t, ts, "PUT", "/api/v1/nav/items/"+navItoa(iid), map[string]any{
		"group_id": mid, "name": "面板", "url": "http://panel.example.com",
		"icon": "", "description": "", "open_new_tab": false,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("更新站点失败 %d: %v", res.StatusCode, out)
	}
	updated := out["data"].(map[string]any)
	if updated["name"] != "面板" || updated["group_id"].(float64) != float64(mid) || updated["open_new_tab"] != false {
		t.Fatalf("更新结果不对: %v", updated)
	}

	// 排序：影音（第 2 个）挪到最前
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/nav/groups/reorder",
		map[string]any{"ids": []int64{mid, tid}}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("分组排序失败 %d: %v", res.StatusCode, out)
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/nav/groups", nil, cookies)
	groups := out["data"].(map[string]any)["list"].([]any)
	first := groups[0].(map[string]any)
	if first["name"] != "影音" {
		t.Fatalf("排序未生效，第一个是 %v", first["name"])
	}

	// 删除
	res, _, _ = doJSON(t, ts, "DELETE", "/api/v1/nav/items/"+navItoa(iid), nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("删除站点失败 %d", res.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "DELETE", "/api/v1/nav/groups/"+navItoa(mid), nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("删除分组失败 %d", res.StatusCode)
	}
	// 再删同一个 → 404（不是 500，也不是静默成功）
	res, _, _ = doJSON(t, ts, "DELETE", "/api/v1/nav/groups/"+navItoa(mid), nil, cookies)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除分组应 404，实际 %d", res.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "PUT", "/api/v1/nav/groups/"+navItoa(mid), map[string]any{"name": "x"}, cookies)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("更新不存在的分组应 404，实际 %d", res.StatusCode)
	}

	// 审计：每个写操作都要有记录
	for _, action := range []string{
		"nav_group_create", "nav_item_create", "nav_item_update",
		"nav_group_reorder", "nav_item_delete", "nav_group_delete",
	} {
		var n int
		if err := srv.Store.DB().QueryRow(
			`SELECT COUNT(*) FROM audit_logs WHERE action=?`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("审计里缺少 %s 的记录", action)
		}
	}
}

// TestNavURLValidationRejectsJavascript 是安全底线：
// 站点地址与图标地址都只允许 http/https，javascript: 等一律 400。
func TestNavURLValidationRejectsJavascript(t *testing.T) {
	_, ts, cookies := setupNavPanel(t)
	g := navCreateGroup(t, ts, cookies, "测试")
	gid := int64(g["id"].(float64))

	bad := []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"vbscript:msgbox(1)",
		"//example.com/no-scheme",
		"",
	}
	for _, u := range bad {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/nav/items",
			map[string]any{"group_id": gid, "name": "坏站点", "url": u}, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("地址 %q 应被拒（400），实际 %d: %v", u, res.StatusCode, out)
		}
	}
	// 图标也不能借道
	for _, icon := range []string{"javascript:alert(1)", "data:image/svg+xml;base64,AAA"} {
		res, _, _ := doJSON(t, ts, "POST", "/api/v1/nav/items",
			map[string]any{"group_id": gid, "name": "坏图标", "url": "https://ok.example.com", "icon": icon}, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("图标 %q 应被拒（400），实际 %d", icon, res.StatusCode)
		}
	}
	// 合法地址要能通过（http 与 https 都可，emoji 图标可）
	for _, u := range []string{"http://a.example.com", "https://a.example.com/x?y=1#z"} {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/nav/items",
			map[string]any{"group_id": gid, "name": "好站点", "url": u, "icon": "🧭"}, cookies)
		if res.StatusCode != http.StatusOK {
			t.Errorf("地址 %q 应通过，实际 %d: %v", u, res.StatusCode, out)
		}
	}

	// 名称/描述长度上限
	longName := strings.Repeat("名", 65)
	res, _, _ := doJSON(t, ts, "POST", "/api/v1/nav/groups", map[string]any{"name": longName}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("超长分组名应 400，实际 %d", res.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/nav/items",
		map[string]any{"group_id": gid, "name": "x", "url": "https://ok.example.com",
			"description": strings.Repeat("描", 201)}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("超长描述应 400，实际 %d", res.StatusCode)
	}
	// 引用不存在的分组
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/nav/items",
		map[string]any{"group_id": 99999, "name": "x", "url": "https://ok.example.com"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("引用不存在的分组应 400，实际 %d", res.StatusCode)
	}
}

// TestNavExportImportRoundTrip：导出的 JSON 原样导入后，再次导出必须逐字段一致。
func TestNavExportImportRoundTrip(t *testing.T) {
	_, ts, cookies := setupNavPanel(t)
	g1 := navCreateGroup(t, ts, cookies, "工作")
	g2 := navCreateGroup(t, ts, cookies, "生活")
	gid1 := int64(g1["id"].(float64))
	gid2 := int64(g2["id"].(float64))
	navCreateItem(t, ts, cookies, map[string]any{
		"group_id": gid1, "name": "Git", "url": "https://git.example.com",
		"icon": "🌿", "description": "代码", "open_new_tab": true,
	})
	navCreateItem(t, ts, cookies, map[string]any{
		"group_id": gid1, "name": "CI", "url": "https://ci.example.com", "open_new_tab": false,
	})
	navCreateItem(t, ts, cookies, map[string]any{
		"group_id": gid2, "name": "相册", "url": "https://photo.example.com",
		"icon": "https://photo.example.com/favicon.ico",
	})

	before := navExport(t, ts, cookies)

	// 原样导入（导出文件本身就是导入接受的格式）
	doc := map[string]any{"groups": before.Groups, "items": before.Items}
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/nav/import", doc, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("导入失败 %d: %v", res.StatusCode, out)
	}
	after := navExport(t, ts, cookies)
	if !navDocEqual(before, after) {
		t.Fatalf("导出→导入→导出 不一致：\nbefore=%+v\nafter=%+v", before, after)
	}

	// 导入必须**整份替换**：先换成只有 1 个组 0 个站点，再确认旧数据全没了。
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/nav/import", map[string]any{
		"groups": []map[string]any{{"id": 100, "name": "唯一分组", "sort": 0}},
		"items":  []any{},
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("整份替换导入失败 %d", res.StatusCode)
	}
	replaced := navExport(t, ts, cookies)
	if len(replaced.Groups) != 1 || replaced.Groups[0].Name != "唯一分组" || len(replaced.Items) != 0 {
		t.Fatalf("导入没有整份替换：%+v", replaced)
	}

	// 非法导入：坏地址必须被拒，且**原数据不变**（校验在替换之前）
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/nav/import", map[string]any{
		"groups": []map[string]any{{"id": 1, "name": "新"}},
		"items":  []map[string]any{{"group_id": 1, "name": "坏", "url": "javascript:alert(1)"}},
	}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法导入应 400，实际 %d", res.StatusCode)
	}
	still := navExport(t, ts, cookies)
	if len(still.Groups) != 1 || still.Groups[0].Name != "唯一分组" {
		t.Fatalf("非法导入不应改动原数据：%+v", still)
	}

	// 缺字段的导入（既没有 groups 也没有 items）应 400，不能把数据清空
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/nav/import", map[string]any{"foo": 1}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺少 groups/items 应 400，实际 %d", res.StatusCode)
	}
	// 站点引用不存在的分组
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/nav/import", map[string]any{
		"groups": []map[string]any{{"id": 1, "name": "新"}},
		"items":  []map[string]any{{"group_id": 2, "name": "孤儿", "url": "https://ok.example.com"}},
	}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("孤儿站点应 400，实际 %d", res.StatusCode)
	}
}

// TestNavStandalonePageIsPublicAndReadOnly：
// 独立别名页与会话无关 —— 未登录能看（只读），且拿不到面板安全后缀。
func TestNavStandalonePageIsPublicAndReadOnly(t *testing.T) {
	srv, ts, cookies := setupNavPanel(t)
	g := navCreateGroup(t, ts, cookies, "公开组")
	gid := int64(g["id"].(float64))
	navCreateItem(t, ts, cookies, map[string]any{
		"group_id": gid, "name": "公开站点", "url": "https://public.example.com", "icon": "🌍",
	})

	// ① 页面本身：不带任何 cookie 也能打开
	for _, path := range []string{"/nav/", "/nav/index.html", "/nav/nav.js"} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("未登录访问 %s 应 200，实际 %d", path, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Errorf("%s 返回空内容", path)
		}
	}

	// ② 公开只读数据接口：不带 cookie 返回同一份数据
	req, _ := http.NewRequest("GET", ts.URL+"/nav/data", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var pub map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&pub)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || pub["ok"] != true {
		t.Fatalf("未登录 /nav/data 应 200 且 ok=true，实际 %d %v", resp.StatusCode, pub)
	}
	pubData := pub["data"].(map[string]any)
	if len(pubData["groups"].([]any)) != 1 || len(pubData["items"].([]any)) != 1 {
		t.Fatalf("公开数据内容不对: %v", pubData)
	}

	// ③ whoami：未登录不能泄露面板后缀
	req, _ = http.NewRequest("GET", ts.URL+"/nav/whoami", nil)
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var who map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&who)
	_ = resp.Body.Close()
	whoData := who["data"].(map[string]any)
	if whoData["authenticated"] != false || whoData["panel_entry"] != "" {
		t.Fatalf("未登录 whoami 不应回显面板入口: %v", whoData)
	}

	// ④ 登录后 whoami 才给出入口
	res, out, _ := doJSON(t, ts, "GET", "/nav/whoami", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("已登录 whoami 应 200，实际 %d", res.StatusCode)
	}
	whoData = out["data"].(map[string]any)
	if whoData["authenticated"] != true {
		t.Fatalf("已登录 whoami 应 authenticated=true: %v", whoData)
	}

	// ⑤ /nav 补斜杠跳转；未知子路径 404（不要回落到面板 index.html）
	noRedir := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noRedir.Get(ts.URL + "/nav")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/nav/" {
		t.Fatalf("/nav 应 301 到 /nav/，实际 %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = noRedir.Get(ts.URL + "/nav/nope")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/nav/nope 应 404，实际 %d", resp.StatusCode)
	}

	// ⑥ 别名页在**安全后缀之外**：设置后缀后 /nav/ 仍可访问，
	//    而面板接口在没有后缀时应 404（证明两者确实在不同层）。
	srv.Cfg.PanelSuffix = "sec12345"
	resp, err = noRedir.Get(ts.URL + "/nav/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("有安全后缀时 /nav/ 仍应 200，实际 %d", resp.StatusCode)
	}
	resp, err = noRedir.Get(ts.URL + "/api/v1/nav/tree")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("有安全后缀时无后缀的接口路径应 404，实际 %d", resp.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "GET", "/sec12345/api/v1/nav/tree", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("带后缀访问接口应 200，实际 %d", res.StatusCode)
	}
}

// TestNavAliasInAppProxyEntries：导航页必须作为一条 nginx 别名入口，
// 这样 `http://<主机>/nav/` 才能当浏览器首页。
// （生成出来的整段 location 会在用户点「打开」/整理默认站点时写进 000-default.conf。）
func TestNavAliasInAppProxyEntries(t *testing.T) {
	entries := appProxyEntries()
	found := false
	for _, e := range entries {
		if e.Slug == "nav" {
			found = true
			if e.Port != 0 {
				t.Errorf("导航页由面板自身提供，不应带应用端口，got=%d", e.Port)
			}
		}
	}
	if !found {
		t.Fatal("appProxyEntries() 里缺少 nav 条目 —— /nav/ 别名不会进 nginx 配置")
	}
	block := appProxyBlock(entries, "127.0.0.1:8443")
	for _, need := range []string{
		"location = /nav {", "return 301 /nav/;", "location ^~ /nav/ {",
		"proxy_pass https://127.0.0.1:8443;", "导航页（面板自带）",
	} {
		if !strings.Contains(block, need) {
			t.Errorf("生成的 nginx 块里缺少 %q\n%s", need, block)
		}
	}
	// 端口为 0 的条目不能打印一个假的 127.0.0.1:0
	if strings.Contains(block, "127.0.0.1:0") {
		t.Error("导航页条目的注释里不应出现 127.0.0.1:0")
	}
	// 把生成的块打进 -v 日志：这是「/nav/ 别名到底会给 nginx 写什么」的原始证据
	// （真机验证说明里直接引用这段文本）。
	t.Logf("生成的 nginx 别名块：\n%s", block)
}

// ---------- 小工具 ----------

func navItoa(v int64) string { return strconv.FormatInt(v, 10) }

func navExport(t *testing.T, ts *httptest.Server, cookies []*http.Cookie) navExportDoc {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/nav/export", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("导出失败 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("导出 Content-Type 不对: %q", ct)
	}
	var doc navExportDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("导出内容不是合法 JSON: %v", err)
	}
	if doc.App != "zizpanel" || doc.Kind != "nav" {
		t.Fatalf("导出文档头不对: %+v", doc)
	}
	return doc
}

func navDocEqual(a, b navExportDoc) bool {
	// exported_at 每次都不同，不参与比较。
	a.ExportedAt, b.ExportedAt = "", ""
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

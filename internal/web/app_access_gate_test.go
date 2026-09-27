package web

// app_access_gate_test.go —— 应用「访问地址（自定义）」的唯一门禁。
//
// 为什么现有门禁抓不到：此前**没有任何**"应用访问地址"的存储或校验 ——
// 「打开」的地址是前端按 ui.slug / port_url / location.hostname+端口 现拼的，
// 服务端一个字节都不存。所以既没有可断言的对象，也没有"用户填的 URL 会被
// 浏览器当地址打开"这条风险的门禁（拼出来的端口地址从来不是用户输入）。
// 这里一条覆盖三类：纯函数校验（合法通过 / 危险 scheme 一律拒）+
// settings KV 往返（设/读/覆盖/清除/互不影响，目录 id 键与服务名键两套）+
// 两种键都认不出的 id 404（垃圾键不许占格）。
//
// 负向对照：删掉 ValidateAppAccessURL 里的 scheme 白名单 ⇒ 用例②必须红
// （`ftp://` 与 `data://host/…` 这两条只有白名单能挡；javascript: 另有
// "必须有主机名"挡着，所以单靠它还证明不了白名单在生效）。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// accessCall 直接调 handler（绕过鉴权，与既有 API 单测同一做法）。
func accessCall(t *testing.T, h http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/market/access-url", rdr)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// accessTable 直接读 KV（不经接口），用来验证"落库/不落库"。
func accessTable(t *testing.T, srv *Server) map[string]string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	m, err := srv.loadAppAccessURLs(req)
	if err != nil {
		t.Fatalf("读访问地址表失败：%v", err)
	}
	return m
}

// TestAppAccessURLGate 是这一条门禁。目录 ID 用 nginx / phpmyadmin（内置目录里长期存在）。
func TestAppAccessURLGate(t *testing.T) {
	srv, _ := newTestServer(t)

	t.Run("① 合法地址通过（去首尾空白）", func(t *testing.T) {
		cases := []struct{ in, want string }{
			{"https://demo.example.com/app/", "https://demo.example.com/app/"},
			{"http://192.0.2.10:8890/", "http://192.0.2.10:8890/"},
			{"  https://demo.example.com/app/  ", "https://demo.example.com/app/"},
			{"HTTPS://Demo.Example.com/X", "HTTPS://Demo.Example.com/X"},
		}
		for _, c := range cases {
			got, err := ValidateAppAccessURL(c.in)
			if err != nil {
				t.Errorf("%q 应通过，却报错：%v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("%q 归一化 = %q，期望 %q", c.in, got, c.want)
			}
		}
	})

	t.Run("② 危险 / 非法地址一律拒绝", func(t *testing.T) {
		reject := []string{
			"",                                     // 空（清除走的是"空串"分支，不经这个函数）
			"   ",                                  // 只有空白
			"javascript:alert(1)",                  // 浏览器会执行
			"data:text/html,<b>x",                  // 内联文档
			"file:///etc/passwd",                   // 本地文件
			"vbscript:msgbox(1)",                   // 旧 IE 脚本
			"ftp://example.com/x",                  // 白名单：只有白名单能挡（带主机名）
			"data://example.com/x",                 // 白名单：同上
			"example.com/app",                      // 没有 scheme
			"https://",                             // 没有主机名
			"https://demo.example.com/a b",         // 空格
			"https://demo.example.com/a\nb",        // 换行
			"https://" + strings.Repeat("a", 2048), // 超长
		}
		for _, in := range reject {
			if got, err := ValidateAppAccessURL(in); err == nil {
				t.Errorf("%q 必须被拒，却通过了（返回 %q）", in, got)
			}
		}
	})

	t.Run("③ KV 往返：设 → 读 → 覆盖 → 另一个 app 互不影响 → 清除", func(t *testing.T) {
		if rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "nginx", "url": "https://nginx.example.com/"}); rec.Code != 200 {
			t.Fatalf("保存应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		if got := accessTable(t, srv)["nginx"]; got != "https://nginx.example.com/" {
			t.Fatalf("落库值 = %q，期望 https://nginx.example.com/", got)
		}

		// 覆盖同一 app：只留最新一条。
		if rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "nginx", "url": "http://198.51.100.7:8080/"}); rec.Code != 200 {
			t.Fatalf("覆盖应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		if got := accessTable(t, srv)["nginx"]; got != "http://198.51.100.7:8080/" {
			t.Fatalf("覆盖后 = %q，期望 http://198.51.100.7:8080/", got)
		}

		// 另一个 app：互不影响。
		if rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "phpmyadmin", "url": "https://pma.example.com/"}); rec.Code != 200 {
			t.Fatalf("第二个 app 保存应 200，实际 %d", rec.Code)
		}
		tbl := accessTable(t, srv)
		if tbl["nginx"] != "http://198.51.100.7:8080/" || tbl["phpmyadmin"] != "https://pma.example.com/" {
			t.Fatalf("两个 app 的地址互相影响了：%v", tbl)
		}

		// 清除（空串）：只清这一个。
		if rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "nginx", "url": ""}); rec.Code != 200 {
			t.Fatalf("清除应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		tbl = accessTable(t, srv)
		if _, ok := tbl["nginx"]; ok {
			t.Fatalf("清除后 nginx 还在：%v", tbl)
		}
		if tbl["phpmyadmin"] != "https://pma.example.com/" {
			t.Fatalf("清除 nginx 不该动 phpmyadmin：%v", tbl)
		}

		// 非法地址必须 400 且**不落库**（先设一个合法值，再试非法值，回读必须还是合法值）。
		if rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "phpmyadmin", "url": "javascript:alert(1)"}); rec.Code != 400 {
			t.Fatalf("javascript: 必须 400，实际 %d：%s", rec.Code, rec.Body.String())
		}
		if got := accessTable(t, srv)["phpmyadmin"]; got != "https://pma.example.com/" {
			t.Fatalf("非法地址把已有值改坏了：%q", got)
		}
	})

	t.Run("④ 两种键都认不出的 id ⇒ 404；缺 id ⇒ 400", func(t *testing.T) {
		rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "no-such-app-xyz", "url": "https://x.example.com/"})
		if rec.Code != 404 {
			t.Fatalf("目录外的 id 必须 404，实际 %d：%s", rec.Code, rec.Body.String())
		}
		if _, ok := accessTable(t, srv)["no-such-app-xyz"]; ok {
			t.Fatalf("404 的请求不该写进 KV")
		}
		rec = accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "", "url": "https://x.example.com/"})
		if rec.Code != 400 {
			t.Fatalf("缺 id 必须 400，实际 %d", rec.Code)
		}
	})

	t.Run("④b 不在目录里的**服务名**也能配访问地址（设→读→清）+ 不存在的服务名 404", func(t *testing.T) {
		// 用户自建服务（目录里没有）：键回退成 svc:<服务名>，入口不能少。
		const svcName = "my-custom-svc"
		if err := srv.serviceRepo.Create(t.Context(), &services.Service{
			Name: svcName, DisplayName: "我的自建服务", Kind: services.KindNative,
		}); err != nil {
			t.Fatalf("建测试服务记录失败：%v", err)
		}

		rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": svcName, "url": "https://custom.example.com/"})
		if rec.Code != 200 {
			t.Fatalf("服务名键保存应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		if got := accessTable(t, srv)["svc:"+svcName]; got != "https://custom.example.com/" {
			t.Fatalf("服务名键落库 = %q，期望 svc:%s", got, svcName)
		}
		// 目录 id 键与服务名键互不影响（同一张表里两套键）。
		if got := accessTable(t, srv)["phpmyadmin"]; got != "https://pma.example.com/" {
			t.Fatalf("服务名键把目录键挤坏了：phpmyadmin = %q", got)
		}
		// 清除。
		if rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": svcName, "url": ""}); rec.Code != 200 {
			t.Fatalf("服务名键清除应 200，实际 %d", rec.Code)
		}
		if _, ok := accessTable(t, srv)["svc:"+svcName]; ok {
			t.Fatalf("清除后服务名键还在：%v", accessTable(t, srv))
		}

		// 服务名必须**真的存在**：垃圾键不许写进去（负向对照：去掉存在性检查 ⇒ 这条红）。
		rec = accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "no-such-service-xyz", "url": "https://x.example.com/"})
		if rec.Code != 404 {
			t.Fatalf("不存在的服务名必须 404，实际 %d：%s", rec.Code, rec.Body.String())
		}
		if _, ok := accessTable(t, srv)["svc:no-such-service-xyz"]; ok {
			t.Fatalf("404 的服务名不该写进 KV")
		}
		if _, ok := accessTable(t, srv)["no-such-service-xyz"]; ok {
			t.Fatalf("404 的服务名不该写进 KV（无前缀形态）")
		}
	})

	t.Run("⑤ 市场列表每条带上 access_url（前端不必再拉一次）", func(t *testing.T) {
		if rec := accessCall(t, srv.handleMarketAccessURLSet,
			map[string]any{"id": "nginx", "url": "https://nginx.example.com/"}); rec.Code != 200 {
			t.Fatalf("保存应 200，实际 %d", rec.Code)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/market", nil)
		rec := httptest.NewRecorder()
		srv.handleMarketList(rec, req)
		if rec.Code != 200 {
			t.Fatalf("市场列表应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data struct {
				List []struct {
					ID        string `json:"id"`
					AccessURL string `json:"access_url"`
				} `json:"list"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析市场列表失败：%v", err)
		}
		found := false
		for _, it := range body.Data.List {
			if it.ID == "nginx" {
				found = true
				if it.AccessURL != "https://nginx.example.com/" {
					t.Fatalf("nginx 的 access_url = %q，期望 https://nginx.example.com/", it.AccessURL)
				}
			}
			if it.ID == "ffmpeg" && it.AccessURL != "" {
				t.Fatalf("没配过的条目不该带 access_url，%s = %q", it.ID, it.AccessURL)
			}
		}
		if !found {
			t.Fatalf("市场列表里没有 nginx —— 目录/过滤变了，门禁需要更新")
		}
	})
}

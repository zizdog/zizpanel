package web

// compose-env 接口的门禁（2026-10-04 报障：镜像站的 `/.env.example` 一律 403）。
//
// 面板改成自己出变量样例内容，这一组把关键判据钉死：
//  ① 内容与 services.ComposeEnvExample 逐字节一致 —— 防"面板又抄了第二份样例"；
//  ② 未知 id / 非推荐项目 ⇒ 404 + 人话；
//  ③ 全部推荐项目的样例都不含疑似真实密钥（与发布脚本同一条判据）。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

func TestServiceComposeEnvEndpoint(t *testing.T) {
	_, ts := newTestServer(t)

	// 与其它 services 接口一致：未登录一律 401。
	res, _, _ := doJSON(t, ts, "GET", "/api/v1/services/immich/compose-env", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录访问应 401，实际 %d", res.StatusCode)
	}

	if res, out, _ := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil); res.StatusCode != 200 {
		t.Fatalf("初始化失败 %d: %v", res.StatusCode, out)
	}
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/login",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	app, found := services.FindApp("immich")
	if !found || !app.DockerReference {
		t.Fatal("目录里没有 immich 这个推荐 Docker 项目 —— 门禁里的已知 id 要跟着目录改")
	}

	// ① 内容逐字节等于 services.ComposeEnvExample(App)（单一数据源）。
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/services/immich/compose-env", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("已知 id 应 200，实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应缺少 data：%v", out)
	}
	content, _ := data["content"].(string)
	if want := services.ComposeEnvExample(app); content != want {
		t.Errorf("接口内容与 services.ComposeEnvExample 不一致（长度 %d vs %d）—— "+
			"变量样例只能有一个来源", len(content), len(want))
	}
	if data["id"] != "immich" || data["name"] != app.Name {
		t.Errorf("id/name 不对：got id=%v name=%v", data["id"], data["name"])
	}
	if !strings.Contains(content, "=") {
		t.Error("样例里一个 `变量=` 都没有，用户复制过去没用")
	}

	// ② 未知 id ⇒ 404（不是 500/200）。
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/services/definitely-no-such-app/compose-env", nil, cookies)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 id 应 404，实际 %d: %v", res.StatusCode, out)
	}
	if msg, _ := out["msg"].(string); strings.TrimSpace(msg) == "" {
		t.Error("404 必须带人话原因（前端要显示给用户）")
	}

	// ②b 已知但**不是**推荐 Docker 项目 ⇒ 404（别给一份不属于它的样例）。
	plainID := ""
	for _, a := range services.Catalog() {
		if !a.DockerReference {
			plainID = a.ID
			break
		}
	}
	if plainID == "" {
		t.Fatal("目录里一个非推荐项目都没有 —— 这条门禁失去意义")
	}
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/services/"+plainID+"/compose-env", nil, cookies)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("非推荐项目 %s 应 404，实际 %d: %v", plainID, res.StatusCode, out)
	}

	// ③ 全部推荐项目：内容非空、与单一数据源一致、不含疑似真实密钥。
	seen := 0
	for _, a := range services.Catalog() {
		if !a.DockerReference {
			continue
		}
		seen++
		res, out, _ = doJSON(t, ts, "GET", "/api/v1/services/"+a.ID+"/compose-env", nil, cookies)
		if res.StatusCode != 200 {
			t.Errorf("%s 应 200，实际 %d: %v", a.ID, res.StatusCode, out)
			continue
		}
		data, _ := out["data"].(map[string]any)
		got, _ := data["content"].(string)
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s 的样例是空的", a.ID)
			continue
		}
		if want := services.ComposeEnvExample(a); got != want {
			t.Errorf("%s 的接口内容与 ComposeEnvExample 不一致", a.ID)
		}
		if suspect := services.ComposeEnvSuspectSecret(got); suspect != "" {
			t.Errorf("%s 的接口内容含疑似真实密钥：%q —— 这个接口会把内容发给用户", a.ID, suspect)
		}
	}
	if seen == 0 {
		t.Fatal("一个推荐 Docker 项目都没有 —— ③ 等于没跑")
	}
}

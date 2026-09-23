package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

const localPluginFixture = `{
  "schema":"zizpanel.app/v1","id":"myapp","name":"我的应用","icon":"🧩",
  "source":{"kind":"brew","formula":"myapp","checksum":"sha256"},
  "run":{"mode":"brew-service"},
  "expose":{"port":12345,"bind":"0.0.0.0","ui":"app"},
  "verify":{"any_of":[{"kind":"http","path":"/healthz","expect":"ok"}]},
  "health":{"kind":"http","path":"/healthz","expect":"ok"},
  "uninstall":{"always":["/Library/LaunchDaemons/homebrew.mxcl.myapp.plist"],
               "optional_data":["~/myapp"],"formula":"myapp"}
}`

// TestPluginEndpointsListAndToggle 锁住"填表 → 启用 → 市场出现"这条链的接口侧。
func TestPluginEndpointsListAndToggle(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "myapp.json"), []byte(localPluginFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"schema":"zizpanel.app/v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	services.SetLocalPluginDir(dir)
	t.Cleanup(func() { services.SetLocalPluginDir("") })

	// ① 列表：合法 + 坏文件都要出现，坏的要带原因
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/plugins", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("列插件应 200，实际 %d：%v", res.StatusCode, out)
	}
	data := apiData(t, out)
	items, _ := data["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("应列出 2 份（含坏文件），实际 %d：%v", len(items), data["items"])
	}
	var brokenErr string
	for _, raw := range items {
		it, _ := raw.(map[string]any)
		if asString(it["id"]) == "" && asString(it["error"]) != "" {
			brokenErr = asString(it["error"])
		}
	}
	if brokenErr == "" {
		t.Error("坏文件必须在列表里带 error（不许静默忽略）")
	}

	// ② 未启用：市场里没有
	if m := marketItemOrNil(t, ts, cookies, "myapp"); m != nil {
		t.Error("未启用的本地插件不该出现在市场里")
	}

	// ③ 启用 → 市场出现
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/plugins/myapp/toggle", map[string]any{"enabled": true}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("启用应 200，实际 %d：%v", res.StatusCode, out)
	}
	if m := marketItemOrNil(t, ts, cookies, "myapp"); m == nil {
		t.Error("启用后市场里应当出现这张卡")
	}

	// ④ 停用 → 又消失
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/plugins/myapp/toggle", map[string]any{"enabled": false}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("停用应 200，实际 %d：%v", res.StatusCode, out)
	}
	if m := marketItemOrNil(t, ts, cookies, "myapp"); m != nil {
		t.Error("停用后不该还在市场里")
	}

	// ⑤ 不存在的 id / 缺字段：如实 4xx
	if res, _, _ := doJSON(t, ts, "POST", "/api/v1/plugins/nope/toggle", map[string]any{"enabled": true}, cookies); res.StatusCode != 404 {
		t.Errorf("不存在的插件应 404，实际 %d", res.StatusCode)
	}
	if res, _, _ := doJSON(t, ts, "POST", "/api/v1/plugins/myapp/toggle", map[string]any{}, cookies); res.StatusCode != 400 {
		t.Errorf("缺 enabled 字段应 400，实际 %d", res.StatusCode)
	}
	_ = srv
}

// marketItemOrNil 在市场响应里找某个 id（找不到返回 nil，不 fail —— 这里要断言"不该出现"）。
func marketItemOrNil(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, id string) map[string]any {
	t.Helper()
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("市场应 200，实际 %d", res.StatusCode)
	}
	list, _ := apiData(t, out)["list"].([]any)
	for _, raw := range list {
		it, _ := raw.(map[string]any)
		if asString(it["id"]) == id {
			return it
		}
	}
	return nil
}

// TestPluginToggleRefusesUninstallable 不能装的来源：启用必须被拒，并把原因说清楚。
func TestPluginToggleRefusesUninstallable(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	dir := t.TempDir()
	rel := `{
	  "schema":"zizpanel.app/v1","id":"myrel","name":"发布轨","icon":"📦",
	  "source":{"kind":"release","repo":"a/b","asset":"c.tar.gz","binary":"c","checksum":"sha256"},
	  "run":{"mode":"app-daemon","bin":"~/myrel/c"},
	  "expose":{"port":12346,"bind":"0.0.0.0","ui":"app"},
	  "verify":{"any_of":[{"kind":"port","port":12346}]},
	  "health":{"kind":"port","port":12346},
	  "uninstall":{"always":["/Library/LaunchDaemons/com.zizdog.myrel.plist"]}
	}`
	if err := os.WriteFile(filepath.Join(dir, "myrel.json"), []byte(rel), 0o644); err != nil {
		t.Fatal(err)
	}
	services.SetLocalPluginDir(dir)
	t.Cleanup(func() { services.SetLocalPluginDir("") })

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/plugins/myrel/toggle", map[string]any{"enabled": true}, cookies)
	if res.StatusCode != 400 {
		t.Fatalf("不可安装的插件启用应 400，实际 %d：%v", res.StatusCode, out)
	}
	msg := asString(out["msg"])
	if !strings.Contains(msg, "JSON 装载") {
		t.Errorf("拒绝理由要能照做，实际 %q", msg)
	}
}

// TestPluginEndpointsRequireAuth 插件接口是控制面：未登录一律 401。
func TestPluginEndpointsRequireAuth(t *testing.T) {
	_, ts := newTestServer(t)
	if res, _, _ := doJSON(t, ts, "GET", "/api/v1/plugins", nil, nil); res.StatusCode != 401 {
		t.Errorf("未登录列插件应 401，实际 %d", res.StatusCode)
	}
	if res, _, _ := doJSON(t, ts, "POST", "/api/v1/plugins/x/toggle", map[string]any{"enabled": true}, nil); res.StatusCode != 401 {
		t.Errorf("未登录改插件状态应 401，实际 %d", res.StatusCode)
	}
}

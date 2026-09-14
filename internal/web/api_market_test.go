package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  应用市场的状态判定（第 5、6 项的真实回归）
//
//  这两条用户反馈都表现在市场页上：
//    5. PHP/nginx 显示"已安装·未纳管"，点纳管报找不到 homebrew.mxcl.* 的 plist
//    6. IOPaint 既不在服务管理里，也不显示纳管按钮
//  下面用**临时 HOME + 真实 plist 文件**复刻这两种状态，断言接口给出的结论。
// ============================================================================

// marketItem 从市场响应里取某个应用。
func marketItem(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, id string) map[string]any {
	t.Helper()
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("市场应 200，实际 %d", res.StatusCode)
	}
	list, _ := out["data"].(map[string]any)["list"].([]any)
	for _, it := range list {
		m, _ := it.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("市场里没有 %s", id)
	return nil
}

func TestMarketUsesRealBrewLabel(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)

	// 复刻本机实况：Homebrew 把 plist 写成 sh.brew.*（不是 homebrew.mxcl.*）
	home := srv.Cfg.UserHome
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"sh.brew.php@8.3", "sh.brew.mysql@8.4"} {
		if err := os.WriteFile(filepath.Join(agents, n+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	php := marketItem(t, ts, cookies, "php83")
	if got := asString(php["service_label"]); got != "sh.brew.php@8.3" {
		t.Errorf("php83 的纳管标签应是磁盘上真实的 sh.brew.php@8.3，实际 %q"+
			"（用 homebrew.mxcl.* 会让纳管必然失败）", got)
	}
	if php["service_in_launchd"] != true {
		t.Errorf("plist 存在时应报告 service_in_launchd=true，实际 %v", php["service_in_launchd"])
	}
	if php["adopted"] != false {
		t.Errorf("还没登记时不该说已纳管，实际 %v", php["adopted"])
	}

	// 把它登记进面板记录（模拟纳管之后），市场必须说"已纳管"
	repo := services.NewRepository(srv.Store)
	if err := repo.Create(t.Context(), &services.Service{
		Name: "sh-brew-php8-3", DisplayName: "PHP 8.3", Kind: services.KindNative,
		LaunchLabel: "sh.brew.php@8.3", Category: "lnmp",
	}); err != nil {
		t.Fatalf("登记服务失败: %v", err)
	}
	php = marketItem(t, ts, cookies, "php83")
	if php["adopted"] != true {
		t.Error("已登记的 php83 在市场里应显示已纳管（否则会给一个点了报「已经纳管过了」的按钮）")
	}
}

// TestMarketDetectsOrphanInstall 第 6 项：安装产物还在、服务没注册。
//
// 这种"孤儿态"以前既不在服务管理里，也不给纳管按钮（因为 installed=false），
// 用户什么都点不了。现在要能识别出来，并给出可执行的出口。
func TestMarketDetectsOrphanInstall(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)

	// 造出 IOPaint 的安装产物，但不注册任何服务、也不放 plist
	exe := filepath.Join(srv.Cfg.UserHome, "iopaint", ".venv", "bin", "iopaint")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	it := marketItem(t, ts, cookies, "iopaint")
	if it["installed"] != true {
		t.Error("有安装产物就该算已安装（否则市场显示「安装」，用户会重装一遍已有的东西）")
	}
	if it["artifacts"] != true {
		t.Error("应报告 artifacts=true（前端据此区分「没装」与「装了但服务没注册」）")
	}
	if it["service_in_launchd"] != false {
		t.Errorf("没有 plist 时 service_in_launchd 应为 false，实际 %v", it["service_in_launchd"])
	}
	if it["adopted"] != false {
		t.Error("没登记就不该说已纳管")
	}
}

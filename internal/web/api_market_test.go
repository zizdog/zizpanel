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
//
// ⚠️ 2026-09-16 修正：这里**曾经**断言 installed=true（理由："有安装产物就该算
// 已安装，否则市场显示「安装」，用户会重装一遍已有的东西"）。那条规则正是
// "卸载后卡片停在已安装、连安装入口都没有"的根因 —— 用户的原话是
// "卸载完成后连安装的入口都没有，用户怎么重装"。
// 现在的契约：installed 只认服务记录 / launchd 里的 plist / brew formula，
// 产物单独用 artifacts 报出来；前端据此显示「残留数据」+「安装」。
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
	if it["installed"] != false {
		t.Error("只有产物、服务与记录都不在时**不能**算已安装 —— 否则卡片停在「已安装」，" +
			"用户没有「安装」入口也就无法重装（2026-09-16 用户反馈）")
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

// TestMarketResidualDataOffersReinstall 是 2026-09-16 用户反馈的回归。
//
// 场景：在应用市场里卸载了一个面板装的应用，但**没有**勾"同时删除数据/产物"
// （Lucky 这类会把安装目录留在 ~/lucky），或者用户手动删了服务、目录却还在。
// 旧代码据此把卡片永久钉在"已安装·服务未注册"上，用户的原话是
// "卸载完成后连安装的入口都没有，用户怎么重装"。
//
// 契约（前端 primaryButton / uninstallButtons 依赖这三条）：
//  1. installed=false —— 回到可安装态，卡片给「安装」；
//  2. artifacts=true  —— 如实说磁盘上还有残留，并说明"安装会复用它们"；
//  3. uninstall.kind=installer 且列出 data_paths —— 前端据此给「删除残留数据」。
func TestMarketResidualDataOffersReinstall(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)

	// 复刻"卸载（保留数据）之后的磁盘状态，服务与记录都不在、只剩产物"。
	//
	// 用 orbien-client 复刻**原生 release 二进制**那条路径（注册表里的 panel installer
	// + 家目录下的安装产物）；compose 残留那条路径由
	// TestMarketComposeResidualOffersReinstall 覆盖。
	exe := filepath.Join(srv.Cfg.UserHome, "orbien-client", "orbien")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	it := marketItem(t, ts, cookies, "orbien-client")
	if it["installed"] != false {
		t.Error("卸载（保留数据）之后市场必须回到可安装态 —— 否则卡片停在「已安装」，" +
			"用户没有任何重装入口")
	}
	if it["artifacts"] != true {
		t.Error("残留产物要如实报出来（前端据此显示「残留数据」并说明安装会复用它们）")
	}
	if it["service_in_launchd"] != false {
		t.Errorf("plist 不在时 service_in_launchd 应为 false，实际 %v", it["service_in_launchd"])
	}

	plan, _ := it["uninstall"].(map[string]any)
	if got := asString(plan["kind"]); got != "installer" {
		t.Errorf("残留态仍要给出 installer 卸载计划（前端据此提供「删除残留数据」），实际 %q", got)
	}
	if paths, _ := plan["data_paths"].([]any); len(paths) == 0 {
		t.Error("卸载计划要列出残留数据路径，用户才知道会删什么")
	}
}

// TestMarketComposeResidualOffersReinstall 覆盖 compose 应用的残留态：
// 卸载（保留数据）后磁盘上只剩 <WorkDir>/compose/<app>，服务记录与容器都不在。
//
// 契约与原生类完全一致（这也是用户反馈的那件事）：
//  1. installed=false —— 回到可安装态，卡片给「安装」；
//  2. artifacts=true  —— 如实说磁盘上还有残留；
//  3. uninstall.kind=installer 且列出 data_paths —— 前端据此给「删除残留数据」。
func TestMarketComposeResidualOffersReinstall(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "PanelTestPw-9x!"}, nil)

	dir := filepath.Join(srv.Cfg.WorkDir, "compose", "uptime-kuma")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services:\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	it := marketItem(t, ts, cookies, "uptime-kuma")
	if it["installed"] != false {
		t.Error("compose 应用卸载后必须回到可安装态")
	}
	if it["artifacts"] != true {
		t.Error("compose 目录还在时要如实报 artifacts=true（前端据此显示「残留数据」）")
	}
	plan, _ := it["uninstall"].(map[string]any)
	if got := asString(plan["kind"]); got != "installer" {
		t.Errorf("compose 残留态也要给 installer 计划（前端据此给「删除残留数据」），实际 %q", got)
	}
	if paths, _ := plan["data_paths"].([]any); len(paths) == 0 {
		t.Error("要列出残留的 compose 目录路径，用户才知道会删什么")
	}
}

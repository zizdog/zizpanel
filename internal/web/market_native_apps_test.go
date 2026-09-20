package web

// 2026-09-20 新增三个原生条目（Memos / Navidrome / Transmission）在市场里的接线门禁。
//
// 用户可见行为的门禁：市场里必须真的有这三张卡片、端口/健康路径与目录一致，
// 而且「已安装」的判据必须贴运行体 —— 只有文件（没有服务记录、没有 plist）时
// 必须是 installed=false 并给得出「安装」入口，而不是"有文件就算装了"（坑 161/170/174）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// TestMarketShowsThreeNativeEntries 锁住三个新条目在市场里的可见字段。
func TestMarketShowsThreeNativeEntries(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)
	seedFakeBrew(t, srv, nil, true) // 干净机器：brew 查成了、什么都没装

	cases := []struct {
		id, name  string
		port      int
		healthURL string
	}{
		{"memos", "Memos（笔记）", 5230, "/healthz"},
		{"navidrome", "Navidrome（音乐）", 4533, "/ping"},
		{"transmission", "Transmission（下载）", 9091, "/transmission/web/"},
	}
	for _, c := range cases {
		it := marketItem(t, ts, cookies, c.id)
		if got := asString(it["name"]); got != c.name {
			t.Errorf("%s 的 name 应为 %q，实际 %q", c.id, c.name, got)
		}
		if got, _ := it["port"].(float64); int(got) != c.port {
			t.Errorf("%s 的 port 应为 %d，实际 %v", c.id, c.port, it["port"])
		}
		if got := asString(it["health_path"]); got != c.healthURL {
			t.Errorf("%s 的 health_path 应为 %q，实际 %q", c.id, c.healthURL, got)
		}
		// 干净机器上必须是"未安装"，而且有卸载计划（能装必能卸：installed=false 时
		// 计划至少不能是 none —— 否则装上以后也卸不掉）。
		if it["installed"] != false {
			t.Errorf("%s：干净机器（brew 空、无记录、无产物）不该判成已安装", c.id)
		}
		plan, _ := it["uninstall"].(map[string]any)
		if asString(plan["kind"]) == "none" {
			t.Errorf("%s：没有卸载路径（kind=none）—— 违反『能装必须能卸』", c.id)
		}
	}
}

// TestMarketTarballFilesAloneAreNotInstalled 是"只有文件 ⇒ 不算已安装"的负向对照。
//
// 做法：把 memos / navidrome 的安装目录与**安装器注册表认可的产物**（
// InstallerArtifactExists 查的 <家目录>/<RootDir>/<Binary>，两个条目都是 ID 同名）
// 真的造出来（模拟"下载完、解压完，但服务没起来 / 被手工删了 plist"），
// 断言市场仍然显示未安装、并且把这份产物如实报成"残留数据"（artifacts=true）。
func TestMarketTarballFilesAloneAreNotInstalled(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)
	seedFakeBrew(t, srv, nil, true)

	for _, id := range []string{"memos", "navidrome"} {
		app, ok := services.FindApp(id)
		if !ok {
			t.Fatalf("目录里没有 %s", id)
		}
		root := filepath.Join(srv.Cfg.UserHome, app.ID)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, app.ID), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if app.ConfigPath != "" {
			p := services.ConfigFilePath(app, srv.Cfg.UserHome, srv.Cfg.WorkDir)
			if p == "" {
				t.Fatalf("%s：ConfigPath=%q 解析不出绝对路径", id, app.ConfigPath)
			}
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte("{}\n"), 0o600)
		}

		it := marketItem(t, ts, cookies, id)
		if it["installed"] != false {
			t.Errorf("%s：只有文件、没有服务记录时必须是 installed=false —— "+
				"『有文件就算装了』会让用户既没有安装入口、也清不掉残留", id)
		}
		if it["artifacts"] != true {
			t.Errorf("%s：产物在磁盘上时应如实报成残留数据（artifacts=true），实际 %v", id, it["artifacts"])
		}
		// launchd 里没有它（沙箱家目录里没有 plist），所以纳管也谈不上。
		if it["adopted"] != false {
			t.Errorf("%s：没有任何服务记录时不该显示已纳管", id)
		}
	}

	// 反向对照：transmission 的判据也不能是"永远 false" —— brew 真的装着它时必须显示已安装。
	srv3, ts3 := newTestServer(t)
	cookies3 := loginTestPanel(t, ts3)
	seedFakeBrew(t, srv3, []string{"transmission-cli"}, true)
	if got := marketItem(t, ts3, cookies3, "transmission")["installed"]; got != true {
		t.Errorf("brew 里装着 transmission-cli 时必须算已安装（否则是永远 false 的空断言），实际 %v", got)
	}

	// 反向对照（同一条判据的另一面）：真的登记了服务记录时，必须显示已安装 ——
	// 否则这条门禁只是"永远返回 false"的空断言。
	srv2, ts2 := newTestServer(t)
	cookies2 := loginTestPanel(t, ts2)
	seedFakeBrew(t, srv2, nil, true)
	label := "com.zizdog.memos"
	agents := filepath.Join(srv2.Cfg.UserHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, label+".plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv2.svcManager().RegisterInstalledService(t.Context(), label,
		"Memos（笔记）", "📝", "tool", 5230); err != nil {
		t.Fatal(err)
	}
	if got := marketItem(t, ts2, cookies2, "memos")["installed"]; got != true {
		t.Errorf("launchd plist 与服务记录都在时 memos 必须算已安装（否则是永远 false 的空断言），实际 %v", got)
	}
}

// TestMarketThreeEntriesHaveRunnableUninstallPlan 走真实 HTTP 契约：
// 三个新条目的卸载计划都必须可读、kind=installer、有步骤。
//
// 这条同时覆盖「能装必能卸」在 **web 层** 的接线（服务层的
// TestCatalogUninstallActionMatrix 只管注册表是否登记）。
func TestMarketThreeEntriesHaveRunnableUninstallPlan(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)
	seedFakeBrew(t, srv, nil, true)

	for _, id := range []string{"memos", "navidrome", "transmission"} {
		res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/"+id+"/uninstall-plan", nil, cookies)
		if res.StatusCode != 200 {
			t.Fatalf("%s 的卸载计划接口应 200，实际 %d: %v", id, res.StatusCode, out)
		}
		data, _ := out["data"].(map[string]any)
		plan, _ := data["uninstall"].(map[string]any)
		if plan == nil {
			plan = data
		}
		kind := asString(plan["kind"])
		if kind != "installer" {
			t.Errorf("%s：卸载计划 kind 应为 installer，实际 %q（%v）", id, kind, plan)
		}
		if steps, _ := plan["steps"].([]any); len(steps) == 0 {
			t.Errorf("%s：卸载计划没有步骤（确认框里会是一片空白）", id)
		}
		if asString(plan["blocked"]) != "" {
			t.Errorf("%s：干净机器上不该被拦下，实际 %q", id, asString(plan["blocked"]))
		}
	}

	// transmission 的计划必须点名"下载的文件不会被删"（用户按下确认前要看得见）。
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/transmission/uninstall-plan", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("uninstall-plan %d", res.StatusCode)
	}
	data, _ := out["data"].(map[string]any)
	plan, _ := data["uninstall"].(map[string]any)
	if plan == nil {
		plan = data
	}
	joined := asString(plan["keep_note"])
	for _, s := range asAnyStrings(plan["steps"]) {
		joined += "\n" + s
	}
	if !strings.Contains(joined, "下载") {
		t.Errorf("transmission 的卸载计划必须说明下载的文件不会被删：\n%s", joined)
	}
}

// asAnyStrings 把 JSON 数组里能当字符串读的都读出来（仅测试用）。
func asAnyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s := asString(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}

package web

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  「📝 编辑配置文件」不许变成灰色按钮（2026-09-18 用户报障）
//
//  用户原话：
//    "php和nginx的『编辑配置文件』都是灰色的！用户没法更改文件大小限制的问题！
//     并且，这些常用更改应该同时做成功能！而不应该是让用户只能编辑配置原文件。"
//
//  旧行为：只有**服务记录**里带 config_path 时才给可点的按钮，否则渲染一颗
//  `disabled: true` 的灰按钮 + 一句"先去注册服务"。而 nginx 完全可以
//  "装着、在跑、面板里没有记录"（用户自己装的 / 换机后记录丢了）——
//  于是最常用的那颗按钮在最需要它的机器上是灰的。
//
//  两条不变量（这一组测试锁死）：
//    ① 配置路径是目录的静态属性：目录里声明了 ConfigPath 的条目，**任何情况下**
//       都能算出绝对路径（与有没有服务记录无关）；
//    ② 前端不许再给这颗按钮加 `disabled` —— 给不出路径时要**说清原因 + 给出下一步**，
//       而不是把入口灰掉（灰按钮 = 点了没反应）。
// ============================================================================

// TestConfigPathResolvesForEveryApp 遍历全目录：声明了 ConfigPath 的条目必须能算出绝对路径。
func TestConfigPathResolvesForEveryApp(t *testing.T) {
	srv, _ := newTestServer(t)
	n := 0
	for _, app := range services.Catalog() {
		if app.ConfigPath == "" {
			continue
		}
		n++
		got := services.ConfigFilePath(app, srv.Cfg.UserHome, srv.Cfg.WorkDir)
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s：目录声明了 ConfigPath=%q，却算不出绝对路径 —— "+
				"前端会因此把「编辑配置文件」渲染成灰按钮", app.ID, app.ConfigPath)
			continue
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s：解析出来的路径不是绝对路径（%q）—— 编辑器只认绝对路径", app.ID, got)
		}
		if strings.Contains(got, "{") || strings.Contains(got, "}") {
			t.Errorf("%s：占位符没被替换（%q）—— 这种路径点开必然报文件不存在", app.ID, got)
		}
	}
	if n == 0 {
		t.Fatal("目录里一个声明了 ConfigPath 的条目都没有？这条门禁就白跑了")
	}
	t.Logf("已校验 %d 个声明了配置文件的条目", n)
}

// TestMarketListCarriesAbsoluteConfigPath：市场列表必须带**解析好的**绝对路径。
//
// 为什么必须在列表里（而不是只在服务详情里）：没有服务记录的 nginx / PHP
// 也要能点开编辑器 —— 用户报障的场景正是"面板里没有这条记录"。
func TestMarketListCarriesAbsoluteConfigPath(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// nginx 正是用户报障的那个条目（面板里通常没有它的服务记录）。
	it := marketItem(t, ts, cookies, "nginx")
	got := asString(it["config_path_abs"])
	want := filepath.Join(srv.Cfg.BrewPrefix, "etc", "nginx", "nginx.conf")
	if got == "" {
		t.Fatalf("市场条目必须带 config_path_abs（否则「编辑配置文件」只能变灰）：%v", it["config_path"])
	}
	// 沙箱里 services.ConfigFilePath 用环境推导的 brew 前缀；这里只要求"是个绝对路径、
	// 且指向 nginx.conf"，不要求等于沙箱前缀（生产环境两者一致）。
	if !filepath.IsAbs(got) || filepath.Base(got) != filepath.Base(want) {
		t.Errorf("config_path_abs 应指向 nginx.conf 的绝对路径，实际 %q", got)
	}
}

// TestServiceListCarriesAbsoluteConfigPath：服务列表也要带（管理面板用它）。
func TestServiceListCarriesAbsoluteConfigPath(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	// 造一条 nginx 服务记录（真机上可能没有；这里验证"有记录时也带路径"）。
	seedServiceRecord(t, srv, "nginx", "Nginx", "homebrew.mxcl.nginx", "lnmp")

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/services", nil, cookies)
	list, _ := out["data"].(map[string]any)["list"].([]any)
	found := false
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if asString(m["name"]) != "nginx" {
			continue
		}
		found = true
		if asString(m["config_path"]) == "" {
			t.Errorf("服务列表里的 nginx 必须带 config_path（管理面板的编辑按钮靠它）：%v", m)
		}
	}
	if !found {
		t.Fatalf("服务列表里没有刚建的 nginx 记录：%v", out)
	}
}

// TestFrontendNeverDisablesConfigEditButton：前端不许再把「编辑配置文件」灰掉。
//
// 这是**用户可见文案/状态**的门禁：灰按钮点下去什么都不发生，原因还只在
// 悬浮提示里 —— 用户看到的就是"点了没反应"（本项目为此踩过多次）。
func TestFrontendNeverDisablesConfigEditButton(t *testing.T) {
	js := readAssetJS(t, "servicePanel.js")
	// 找「编辑配置文件」按钮附近的代码块，断言它不再带 disabled。
	idx := strings.Index(js, "📝 编辑配置文件")
	if idx < 0 {
		t.Fatal("servicePanel.js 里找不到「📝 编辑配置文件」按钮 —— 门禁需要同步更新")
	}
	// 取按钮前后各 1200 字符作为上下文（覆盖三颗按钮的分支）。
	from := idx - 1200
	if from < 0 {
		from = 0
	}
	to := idx + 1200
	if to > len(js) {
		to = len(js)
	}
	block := js[from:to]
	if strings.Contains(block, "disabled: true") {
		t.Errorf("「📝 编辑配置文件」附近又出现了 `disabled: true` —— 灰按钮 = 用户点了没反应；" +
			"给不出路径时必须说清原因并给出下一步（见 api_services.go 的 config_path_abs）")
	}
	if !strings.Contains(js, "config_path_abs") {
		t.Errorf("前端没有用 config_path_abs —— 没有服务记录的 nginx / PHP 会再次拿不到路径")
	}
}

// seedServiceRecord 直接在仓库里造一条服务记录（供列表断言用）。
func seedServiceRecord(t *testing.T, srv *Server, name, display, label, category string) {
	t.Helper()
	repo := services.NewRepository(srv.Store)
	if err := repo.Create(t.Context(), &services.Service{
		Name: name, DisplayName: display, Kind: services.KindNative,
		LaunchLabel: label, Category: category, Managed: true,
	}); err != nil {
		t.Fatalf("建服务记录 %s 失败: %v", name, err)
	}
}

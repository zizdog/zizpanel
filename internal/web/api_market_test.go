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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// 复刻本机实况：Homebrew 把 plist 写成 sh.brew.*（不是 homebrew.mxcl.*）
	home := srv.Cfg.UserHome
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"sh.brew.php@8.2", "sh.brew.mysql@8.4"} {
		if err := os.WriteFile(filepath.Join(agents, n+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	php := marketItem(t, ts, cookies, "php82")
	if got := asString(php["service_label"]); got != "sh.brew.php@8.2" {
		t.Errorf("php82 的纳管标签应是磁盘上真实的 sh.brew.php@8.2，实际 %q"+
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
		Name: "sh-brew-php8-2", DisplayName: "PHP 8.2", Kind: services.KindNative,
		LaunchLabel: "sh.brew.php@8.2", Category: "lnmp",
	}); err != nil {
		t.Fatalf("登记服务失败: %v", err)
	}
	php = marketItem(t, ts, cookies, "php82")
	if php["adopted"] != true {
		t.Error("已登记的 php82 在市场里应显示已纳管（否则会给一个点了报「已经纳管过了」的按钮）")
	}
}

// TestMarketCardMatchesPanelRecordForRealMachineApps 锁住"卡片已安装 ↔ 任务说已安装"的一致性。
//
// 真机（2026-09-16）就是这里对不上：卡片按面板记录显示「已安装」，
// 用户再点一次安装，任务却以 failed 结束（端口被自己占用 / 服务名已存在）。
// 现在安装任务会走幂等分支返回「已安装（跳过）」；这条测试确保卡片的
// installed 判定仍然是"有面板记录就算已安装"，两端不会再互相矛盾。
//
// 用真机上那 8 个应用的**原始记录名**复刻现场（nginx/mysql 的记录名是标签
// 归一化出来的，PHP 的记录名就是目录 ID）。
func TestMarketCardMatchesPanelRecordForRealMachineApps(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	repo := services.NewRepository(srv.Store)
	records := []struct {
		name  string
		label string
		kind  services.Kind
	}{
		{"homebrew-mxcl-nginx", "homebrew.mxcl.nginx", services.KindNative},
		{"sh-brew-mysql8-4", "sh.brew.mysql@8.4", services.KindNative},
		{"ollama", "homebrew.mxcl.ollama", services.KindNative},
		{"uptime-kuma", "", services.KindCompose},
		{"php82", "homebrew.mxcl.php@8.2", services.KindNative},
		{"php84", "homebrew.mxcl.php@8.4", services.KindNative},
	}
	for _, r := range records {
		if err := repo.Create(t.Context(), &services.Service{
			Name: r.name, DisplayName: r.name, Kind: r.kind, LaunchLabel: r.label, Managed: true,
		}); err != nil {
			t.Fatalf("准备记录 %s 失败: %v", r.name, err)
		}
	}

	for _, id := range []string{"nginx", "mysql84", "ollama", "uptime-kuma", "php82", "php84"} {
		it := marketItem(t, ts, cookies, id)
		if it["installed"] != true {
			t.Errorf("%s 有面板记录，市场卡片必须显示已安装"+
				"（否则会出现「卡片说没装、任务说已装」的矛盾）：%v", id, it["installed"])
		}
		if it["adopted"] != true {
			t.Errorf("%s 已登记，市场必须显示已纳管，实际 %v", id, it["adopted"])
		}
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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

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
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

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

// ============================================================================
//  市场板块：最后一个板块是「基础环境」（单一来源）
// ============================================================================

// TestMarketExposesSectionsForBaseEnvironment 锁住两个接口契约：
//  1. GET /api/v1/market 必须返回 sections —— 它是市场板块顺序与中文名的
//     唯一数据源（前端 apps.js 不再自带分类中文名）；最后一个板块是兜底板块
//     「基础环境」（原「其它」），且位置上确实在最后；
//  2. 一键 LNMP 不作为市场条目出现（它的入口在「网站管理」，避免两处重复）。
func TestMarketExposesSectionsForBaseEnvironment(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("市场应 200，实际 %d", res.StatusCode)
	}
	data, _ := out["data"].(map[string]any)

	secs, _ := data["sections"].([]any)
	if len(secs) == 0 {
		t.Fatal("市场接口必须返回 sections（前端不再自带分类中文名，缺了它只能退化成「全部应用」）")
	}
	last, _ := secs[len(secs)-1].(map[string]any)
	if got := asString(last["label"]); got != "基础环境" {
		t.Errorf("最后一个板块应为「基础环境」，实际 %q", got)
	}
	if last["fallback"] != true {
		t.Errorf("最后一个板块应是兜底板块（fallback=true），实际 %v", last["fallback"])
	}
	for _, it := range secs {
		m, _ := it.(map[string]any)
		if asString(m["label"]) == "其它" {
			t.Error("市场板块不该再出现旧名「其它」（用户要求改名为「基础环境」）")
		}
	}

	list, _ := data["list"].([]any)
	for _, it := range list {
		m, _ := it.(map[string]any)
		if asString(m["id"]) == "lnmp" {
			t.Error("一键 LNMP 不该作为市场条目出现（入口在「网站管理」），见 marketHiddenApps")
		}
	}
}

// TestMarketVisibleAppsHidesHiddenIDs 覆盖"刻意排除"那段逻辑本身。
//
// 当前目录里没有 ID=lnmp 的条目，所以 handleMarketList 那条排除是**防御性**的；
// 直接测 marketVisibleApps 才能在今天就证明它真的会把 lnmp 挡下，
// 而不是等将来有人把 lnmp 加进目录才发现漏了。
func TestMarketVisibleAppsHidesHiddenIDs(t *testing.T) {
	got := marketVisibleApps([]services.App{{ID: "lnmp"}, {ID: "nginx"}, {ID: "php82"}})
	if len(got) != 2 {
		t.Fatalf("lnmp 必须从市场列表里被排除（入口在网站管理），实际 %d 个: %+v", len(got), got)
	}
	for _, a := range got {
		if a.ID == "lnmp" {
			t.Fatalf("lnmp 不该出现在市场可见条目里: %+v", got)
		}
	}
	if reason := marketHiddenApps["lnmp"]; reason == "" {
		t.Error("marketHiddenApps 里必须写清 lnmp 被隐藏的原因（代码就是注释）")
	}
}

// TestMarketDockerReferenceContract 锁住"推荐 Docker 项目"给前端的稳定契约。
//
// 用户 2026-09-17 的决策：Docker 类条目不再是"可安装应用"，而是"面板建议的项目"。
// 前端据此把卡片放进 docker Tab 并隐藏安装按钮，所以接口必须稳定给出：
//   - docker_reference（App 上的字段，json 名）与 docker_recommended（前端已认的别名）；
//   - compose_yaml —— 预配置 compose 的**内容**（用于"复制"）；
//   - compose_url / compose_env_url / compose_readme_url —— 镜像站上的下载/查看地址。
func TestMarketDockerReferenceContract(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	for _, id := range []string{"it-tools", "filebrowser", "activepieces", "immich"} {
		it := marketItem(t, ts, cookies, id)
		if it["docker_reference"] != true {
			t.Errorf("%s 是推荐 Docker 项目，接口必须给 docker_reference=true，实际 %v", id, it["docker_reference"])
		}
		if it["docker_recommended"] != true {
			t.Errorf("%s 缺少 docker_recommended 别名（前端 apps.js 的 isDockerRec 认这个名字），实际 %v",
				id, it["docker_recommended"])
		}
		yaml := asString(it["compose_yaml"])
		if !strings.Contains(yaml, "image:") || !strings.Contains(yaml, "services:") {
			t.Errorf("%s 的 compose_yaml 不是一份可用的 compose（前端的「复制」按钮要用它）：%q", id, yaml)
		}
		if got := asString(it["compose_url"]); !strings.HasSuffix(got, "/compose/"+id+"/docker-compose.yml") {
			t.Errorf("%s 的 compose_url 应指向镜像站 /compose/%s/docker-compose.yml，实际 %q", id, id, got)
		}
		if got := asString(it["compose_env_url"]); !strings.HasSuffix(got, "/compose/"+id+"/.env.example") {
			t.Errorf("%s 的 compose_env_url 应指向镜像站 /compose/%s/.env.example，实际 %q", id, id, got)
		}
		if got := asString(it["compose_readme_url"]); !strings.HasSuffix(got, "/compose/README.md") {
			t.Errorf("%s 缺少总索引地址 compose_readme_url，实际 %q", id, got)
		}
	}
}

// TestMarketDockerReferenceInstallRefused 锁住"安装接口明确拒绝"。
//
// 必须是 4xx + 人话（说清是推荐项目、compose 在哪、Docker 页有 Compose 面板），
// 而不是静默失败、更不是谎报成功。
func TestMarketDockerReferenceInstallRefused(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	for _, id := range []string{"it-tools", "uptime-kuma"} {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/market/"+id+"/install", nil, cookies)
		if res.StatusCode < 400 || res.StatusCode >= 500 {
			t.Fatalf("%s 的安装请求必须返回 4xx（而不是 2xx 假成功或 5xx），实际 %d（%v）",
				id, res.StatusCode, out["msg"])
		}
		msg := asString(out["msg"])
		if !strings.Contains(msg, "推荐") || !strings.Contains(msg, "compose") {
			t.Errorf("%s 的拒绝错误要说清「这是推荐项目、请取用 compose 文件」，实际：%q", id, msg)
		}
	}
}

// TestMarketDeletedEntriesAreGone 锁住这一轮下架：minio / portainer 不再出现在市场。
//
// 注意语义：删除的是**条目**（不再推荐/不在市场），不是卸载 —— mini 上两个容器
// 与数据都还在，所以这里只断言接口不再列出它们。
func TestMarketDeletedEntriesAreGone(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("市场应 200，实际 %d", res.StatusCode)
	}
	list, _ := out["data"].(map[string]any)["list"].([]any)
	for _, it := range list {
		m, _ := it.(map[string]any)
		switch asString(m["id"]) {
		case "minio", "portainer":
			t.Errorf("%s 已按用户要求删除，市场里不该再出现", m["id"])
		}
	}
	// 下架不等于卸载：卸载计划仍要能说出它们的镜像名（services 层有
	// legacyComposeImages 的锁，这里只确认市场声明也删干净了）。
	for _, id := range []string{"minio", "portainer"} {
		if _, ok := services.MarketAppFor(id); ok {
			t.Errorf("%s 的市场下载点声明也要删干净（反漂移门禁是双向的）", id)
		}
	}
}

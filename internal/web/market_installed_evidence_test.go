package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  「装了却显示未装 / 不在已安装里 / 还显示安装按钮」这一类的全目录门禁
//
//  2026-09-23 用户连报两条同一类缺陷：
//    · 图片压缩（libvips）安装成功后"没变化、没有出现在已安装里、还显示安装按钮"；
//    · phpMyAdmin 在应用市场里显示未安装（"它是有状态的目录，只要判断这个目录在，
//      就是安装！"）。
//
//  两条都发生在**没有常驻服务**的应用上（App.NoDaemon）：面板不会给它们建服务
//  记录，于是它们的 installed 只剩 brew 探测这一条路。这里用**真实 HTTP 契约**
//  把这一类钉死（不是只测被点名的那两个）：
//
//   ① 遍历 Catalog()：凡是"不登记服务"的条目，只要它声明的**真实产物**在磁盘上
//      （纯 CLI = 可执行文件，phpMyAdmin = web 根 + index.php），
//      在 brew **完全探测失败**的情况下也必须 installed=true；
//   ② installed=true ⇒ uninstall.kind != "none"（AGENTS 第三节的硬规矩：
//      能装必须能卸，说已安装就必须有卸载入口）；
//   ③ 安装/卸载任务结束必须让市场缓存失效 —— 否则装完刷新拿到的还是旧结论；
//   ④ brew 探测失败**不得**把上一次的真实结论抹成"什么都没装"。
// ============================================================================

// seedFakeBrew 用一段假 brew 替换沙箱里的 brew 可执行文件。
//
// ok=false 表示模拟"brew 探测失败"（不可用/超时/非零退出）——这时脚本必须
// 真的以非零退出，面板才有机会暴露"未复核"与"真实产物"这两条路。
func seedFakeBrew(t *testing.T, srv *Server, formulas []string, ok bool) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if !ok {
		b.WriteString("exit 1\n")
	} else {
		b.WriteString("if [ \"$1\" = \"list\" ]; then\n")
		for _, f := range formulas {
			b.WriteString("  echo \"" + f + " 1.0.0\"\n")
		}
		b.WriteString("  exit 0\nfi\nexit 0\n")
	}
	if err := os.WriteFile(filepath.Join(srv.Cfg.BrewPrefix, "bin", "brew"), []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedDeclaredRuntimeBodies 在沙箱里把**每一条**目录条目声明的安装体造出来
// （路径与形态全部来自 RuntimePath/RuntimeEntry，不写死任何应用 ID），
// 返回造出来的条数 —— 门禁必须确认这个数不是 0。
func seedDeclaredRuntimeBodies(t *testing.T, srv *Server) int {
	t.Helper()
	n := 0
	for _, app := range services.Catalog() {
		if strings.TrimSpace(app.RuntimePath) == "" {
			continue
		}
		path := services.ResolveRuntimePath(app.RuntimePath, srv.Cfg.BrewPrefix, srv.Cfg.UserHome)
		if path == "" {
			t.Fatalf("%s：RuntimePath=%q 在测试沙箱里展开不出来", app.ID, app.RuntimePath)
		}
		if entry := strings.TrimSpace(app.RuntimeEntry); entry != "" {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, entry), []byte("<?php\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("#!/bin/sh\necho 1.0.0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		n++
	}
	return n
}

// serviceLessMarketApps 返回"面板装了也不会留下服务记录"的目录条目
// （NoDaemon：纯 CLI 引擎与 phpMyAdmin 这类网页入口）。
func serviceLessMarketApps() []services.App {
	out := []services.App{}
	for _, app := range services.Catalog() {
		if _, hidden := marketHiddenApps[app.ID]; hidden {
			continue
		}
		if app.SiteApp != nil || app.Kind == services.KindCompose || app.Kind == services.KindDocker || app.Kind == services.KindColima {
			continue
		}
		if app.NoDaemon {
			out = append(out, app)
		}
	}
	return out
}

// TestMarketServiceLessAppsInstalledFromRealBodyWhenBrewFails 门禁①②。
//
// 最严苛的现场：brew **完全探测失败**（退出码非 0，面板拿不到任何已装清单），
// 磁盘上只有目录里声明的真实产物。这时那些没有服务记录的条目**必须**仍然
// 显示已安装、并且给得出卸载路径 —— 否则用户看到的就是本次报障的那张卡片。
func TestMarketServiceLessAppsInstalledFromRealBodyWhenBrewFails(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	seedFakeBrew(t, srv, nil, false) // brew 探测彻底失败
	if n := seedDeclaredRuntimeBodies(t, srv); n < 3 {
		t.Fatalf("只造出 %d 个安装体，门禁覆盖不足（声明是不是被删了？）", n)
	}

	apps := serviceLessMarketApps()
	if len(apps) < 3 {
		t.Fatalf("目录里只有 %d 个 NoDaemon 条目，明显偏少 —— 判据可能把整类跳过了", len(apps))
	}
	for _, app := range apps {
		it := marketItem(t, ts, cookies, app.ID)
		if it["installed"] != true {
			t.Errorf("%s：真实产物在磁盘上、brew 探测又失败，installed 仍为 false —— "+
				"这正是用户看到的『装了却显示安装』（NO service record, brew probe failed）", app.ID)
		}
		if got := asString(it["runtime_body_path"]); got == "" {
			t.Errorf("%s：installed=true 时应如实给出 runtime_body_path（真实产物路径）", app.ID)
		}
		plan, _ := it["uninstall"].(map[string]any)
		if asString(plan["kind"]) == "none" {
			t.Errorf("%s：installed=true 却给不出卸载路径（kind=none）—— "+
				"违反『能装必须能卸』（AGENTS 第三节）", app.ID)
		}
	}
	// 如实降级：这次没能复核 Homebrew，前端据此显示「未能复核已装软件」。
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("market %d", res.StatusCode)
	}
	if got := out["data"].(map[string]any)["brew_probe_ok"]; got != false {
		t.Errorf("brew 探测失败时 brew_probe_ok 必须是 false（不许把『没查成』说成『没装』），实际 %v", got)
	}
}

// TestMarketPhpMyAdminDirectoryAloneMeansInstalled 是用户第二条报障的回归。
//
// 现场：phpMyAdmin 的 web 根目录在磁盘上（用户手工装的那份不在 Homebrew 里），
// 面板过去据此显示"未安装"。用户原话："它是有状态的目录，只要判断这个目录在，
// 就是安装！" —— 这里同时锁住两件事：目录在 ⇒ 已安装 + 有卸载路径；
// 而**没有**产物的条目不能因此被顺带谎报成已安装。
func TestMarketPhpMyAdminDirectoryAloneMeansInstalled(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeBrew(t, srv, nil, false) // 用户手工装的：brew 里根本没有 phpmyadmin

	root := filepath.Join(srv.Cfg.BrewPrefix, "share", "phpmyadmin")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.php"), []byte("<?php\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	it := marketItem(t, ts, cookies, "phpmyadmin")
	if it["installed"] != true {
		t.Error("phpMyAdmin 的 web 根目录（含 index.php）在磁盘上时必须算已安装")
	}
	if got := asString(it["runtime_body_path"]); got != root {
		t.Errorf("runtime_body_path 应是真实的 web 根 %q，实际 %q", root, got)
	}
	plan, _ := it["uninstall"].(map[string]any)
	if asString(plan["kind"]) != "installer" {
		t.Errorf("phpMyAdmin 必须给得出 installer 卸载路径，实际 %q", asString(plan["kind"]))
	}
	if steps, _ := plan["steps"].([]any); len(steps) == 0 {
		t.Error("卸载计划必须有步骤（确认框里不能一片空白）")
	} else {
		joined := ""
		for _, s := range steps {
			joined += asString(s) + "\n"
		}
		if !strings.Contains(joined, root) {
			t.Errorf("卸载步骤里必须逐字写出会移除的 web 根 %s（用户按确认前要看得见），实际：\n%s", root, joined)
		}
	}

	// 反向：没有任何产物的 imgcompress 不能跟着一起被谎报成已安装。
	if other := marketItem(t, ts, cookies, "imgcompress"); other["installed"] != false {
		t.Error("沙箱里没有 vips 的任何产物，imgcompress 不该被判成已安装")
	}
}

// TestMarketCacheInvalidatedByInstallTask 门禁③。
//
// 现场（本次报障的根因之一）：市场缓存 5 分钟 TTL，而安装任务结束时**没有任何
// 地方让它失效** —— 用户装完，前端 onDone 里那次刷新拿到的还是旧集合，
// 卡片停在「安装」，「已安装」Tab 里也没有它。
func TestMarketCacheInvalidatedByInstallTask(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// 第一次请求：brew 什么都没报（缓存暖成"这台机器什么都没装"）
	if got := marketItem(t, ts, cookies, "imgcompress")["installed"]; got != false {
		t.Fatalf("前置条件不成立：imgcompress 一开始应当是未安装，实际 %v", got)
	}
	// "装好了"：brew 报告 vips。
	//
	// 刻意**不**在磁盘上放 vips 可执行文件：那属于"真实产物"这条独立判据，
	// 会绕过缓存直接得出已安装，就测不到"缓存必须被主动失效"这件事。
	seedFakeBrew(t, srv, []string{"vips"}, true)
	// 缓存还在 → 仍然显示未安装（这就是报障现场；不是我们想要的终态，而是
	// 说明"必须主动失效"）
	if got := marketItem(t, ts, cookies, "imgcompress")["installed"]; got != false {
		t.Fatalf("缓存未失效时不应变化（前置条件变了？），实际 %v", got)
	}
	// 安装任务结束时调用的就是它
	srv.InvalidateMarketCache()
	if got := marketItem(t, ts, cookies, "imgcompress")["installed"]; got != true {
		t.Error("安装任务结束（InvalidateMarketCache）后市场必须立刻反映新状态，" +
			"否则用户装完刷新页面看到的还是「安装」")
	}

	// 哪些任务会触发失效：会改 Homebrew 包集合的那几类；其余不触发
	//（否则每次与安装无关的任务之后，打开市场都要多等一次同步 brew 探测）。
	for _, kind := range []string{"install", "uninstall", "site-install"} {
		if !marketAffectingTask(kind) {
			t.Errorf("任务 kind=%q 可能改变『装了哪些 Homebrew 包』，结束时应让市场缓存失效", kind)
		}
	}
	for _, kind := range []string{"file_compress", "db_import", "cert-renew", "docker-container-create", "deploy", "settings"} {
		if marketAffectingTask(kind) {
			t.Errorf("任务 kind=%q 与『装了哪些 brew 包』无关，不该让市场缓存失效"+
				"（会让下次开市场多等一次 brew）", kind)
		}
	}

	// 光有 InvalidateMarketCache 不够：**任务收尾真的调它**才算接线完成。
	// 少了这一步，所有安装/卸载都会退回"5 分钟后才更新"的旧行为。
	tasks := readGoSource(t, "api_tasks.go")
	if !strings.Contains(tasks, "if marketAffectingTask(kind) {") ||
		!strings.Contains(tasks, "s.InvalidateMarketCache()") {
		t.Error("api_tasks.go 的任务收尾必须按 kind 调用 Server.InvalidateMarketCache() —— " +
			"否则安装/卸载完成后市场仍是旧结论（本次报障的根因之一）")
	}
}

// TestMarketBrewProbeFailureKeepsLastKnownTruth 门禁④。
//
// 一次瞬时 brew 故障**不得**把上一次的真实结论抹成"什么都没装"——
// 那会让所有只靠 brew 证据的条目一起显示「安装」，用户以为软件被卸了。
// 这时应当：保留旧结论 + 如实标 brew_probe_ok=false（前端显示「未能复核」）。
func TestMarketBrewProbeFailureKeepsLastKnownTruth(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// 真实产物只有 ffmpeg 那条（为了确认"旧结论保留"针对的是 brew 证据，
	// 而不是被运行时产物这条独立判据顺带救回来的）。
	seedFakeBrew(t, srv, []string{"nginx"}, true)
	if got := marketItem(t, ts, cookies, "nginx")["installed"]; got != true {
		t.Fatalf("前置条件不成立：brew 报告 nginx 已装时应显示已安装，实际 %v", got)
	}

	// brew 开始失败 + 把缓存时间推老，逼出一次刷新
	seedFakeBrew(t, srv, nil, false)
	srv.mktMu.Lock()
	srv.mktBrewAt = time.Now().Add(-time.Hour)
	srv.mktMu.Unlock()
	if got := marketItem(t, ts, cookies, "nginx")["installed"]; got != true {
		t.Fatalf("刷新进行中仍应返回上一次的真实结论，实际 %v", got)
	}
	// 等后台刷新落地
	deadline := time.Now().Add(20 * time.Second)
	for srv.mktRefreshing.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	it := marketItem(t, ts, cookies, "nginx")
	if it["installed"] != true {
		t.Error("这次 brew 探测失败，不得把上一次『nginx 已装』的真实结论抹成未安装 —— " +
			"那会把机器上所有只靠 brew 证据的软件一起谎报成未装")
	}
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("market %d", res.StatusCode)
	}
	if got := out["data"].(map[string]any)["brew_probe_ok"]; got != false {
		t.Errorf("探测失败必须如实报 brew_probe_ok=false（前端据此显示「未能复核」），实际 %v", got)
	}
}

// TestMarketBrewProbeRecoversAfterTransientFailure 失败不是永久状态：
// brew 恢复后市场必须很快回到真实结论（失败只按 marketProbeMissTTL 缓存）。
func TestMarketBrewProbeRecoversAfterTransientFailure(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	seedFakeBrew(t, srv, nil, false)
	if got := marketItem(t, ts, cookies, "nginx")["installed"]; got != false {
		t.Fatalf("前置条件：brew 失败时 nginx 应为未安装，实际 %v", got)
	}
	// brew 恢复
	seedFakeBrew(t, srv, []string{"nginx"}, true)
	srv.mktMu.Lock()
	srv.mktBrewAt = time.Now().Add(-time.Hour) // 失败结果的 TTL 很短，这里只是不等它
	srv.mktMu.Unlock()
	// 这次请求会触发后台刷新（并先返回旧结论），等它落地再看。
	_ = marketItem(t, ts, cookies, "nginx")
	deadline := time.Now().Add(20 * time.Second)
	for srv.mktRefreshing.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := marketItem(t, ts, cookies, "nginx")["installed"]; got != true {
		t.Errorf("brew 恢复后应很快回到真实结论（已安装），实际 %v —— "+
			"失败结论粘住会让用户以为软件永远没了", got)
	}
}

// TestMarketItemNeverClaimsInstalledWithoutEvidence 反向门禁：
// 没有任何证据的条目**不得**被判成已安装（否则用户看不到安装入口）。
// 遍历全目录，用一台"干净的空机器"（brew 空、没有产物、没有记录）断言。
func TestMarketItemNeverClaimsInstalledWithoutEvidence(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeBrew(t, srv, nil, true) // brew 查成了、什么都没装

	apps := marketVisibleApps(services.Catalog())
	checked := 0
	for _, app := range apps {
		if app.SiteApp != nil || app.DockerReference || app.Kind == services.KindCompose ||
			app.Kind == services.KindDocker || app.Kind == services.KindColima {
			continue
		}
		// 容器运行时走自己的真实探测（dockerRuntimeStatus），而且它的探测会看
		// 真实 PATH —— 开发机上装着 colima 时它**应该**是已安装，不属于本条门禁。
		checked++
		it := marketItem(t, ts, cookies, app.ID)
		if it["installed"] != false || it["adopted"] != false {
			t.Errorf("%s：空机器（无记录、无产物、brew 空）上不该判成已安装/已纳管，实际 installed=%v adopted=%v",
				app.ID, it["installed"], it["adopted"])
		}
	}
	if checked == 0 {
		t.Fatal("一个条目都没检查到 —— 门禁等于没跑")
	}
}

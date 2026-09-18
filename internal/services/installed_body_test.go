package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  「安装体」判据的全目录门禁
//
//  2026-09-23 用户连报两条同一类缺陷：
//    · 「图片压缩（libvips）安装成功后没变化、不在已安装里、还显示安装按钮」；
//    · 「phpMyAdmin 在应用市场里是未安装状态，很明显是判断错误。它是有状态的
//       目录，只要判断这个目录在，就是安装！」
//
//  两条的根因是同一个：**没有常驻服务的应用（NoDaemon）在面板里没有服务记录，
//  它们的「已安装」只能靠 `brew list --versions` 的一句话**。那句话一旦缺席
//  （探测失败、缓存没失效、用户手工装），判据就整个落空 —— 界面把明明装着的
//  东西显示成「安装」。
//
//  按 AGENTS 第三节：修一个 bug 之前先问"这是哪一类问题"，一次修完 + 补一条
//  覆盖整类的门禁。所以这里**遍历 Catalog() 的每一条**，把两条不变量钉死：
//
//   ① 没有常驻服务 / 没有服务标签的条目，必须声明**不依赖服务登记**的已安装证据
//      （BrewFormula 或 RuntimePath），并且 NoDaemon 的条目必须有 RuntimePath
//      —— 它就是"真实产物在不在"的那条判据；
//   ② 只要那条证据成立（产物真的在），PlanUninstallForBrew(**没有服务记录**)
//      就必须给得出 kind != "none" 的卸载路径 —— installed=true ⇒ 有可用的
//      卸载路径（AGENTS 第三节的硬规矩）。
//
//  这不是"只修被点名的那两个"：真加一个新应用却忘了声明证据 / 忘了卸载路径时，
//  测试阶段就会红，不会等用户再报一次。
// ============================================================================

// materializeRuntimeBody 在临时 brew 前缀/家目录里把条目声明的安装体造出来，
// 并返回它此刻的探测结论。
//
// 故意**不是**"写死 vips/phpmyadmin 的路径"：路径与形态（可执行文件 / 目录+入口）
// 全部来自目录条目的 RuntimePath/RuntimeEntry，所以新条目一加进来这条门禁就自动
// 覆盖它，不需要回来补 case。
func materializeRuntimeBody(t *testing.T, app App, brewPrefix, userHome string) RuntimeBody {
	t.Helper()
	if strings.TrimSpace(app.RuntimePath) == "" {
		return RuntimeBody{}
	}
	path := ResolveRuntimePath(app.RuntimePath, brewPrefix, userHome)
	if path == "" {
		t.Fatalf("%s：RuntimePath=%q 在给定沙箱里展开不出来（{brew}/~ 前缀写错了？）",
			app.ID, app.RuntimePath)
	}
	if entry := strings.TrimSpace(app.RuntimeEntry); entry != "" {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		ef := filepath.Join(path, entry)
		if err := os.MkdirAll(filepath.Dir(ef), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ef, []byte("<?php\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return DetectRuntimeBody(app, brewPrefix, userHome)
}

// isServiceRegisteredOnInstall 报告"面板/brew 装这个应用时会不会留下服务登记"
// （面板记录或 launchd 作业）。会留下的条目，它们的「已安装」有服务记录那条路；
// 不留下的（NoDaemon / 纯 CLI / 纯 brew 包）就必须有别的真实证据。
func isServiceRegisteredOnInstall(app App) bool {
	return app.ServiceLabel != "" || app.AdoptLabel != ""
}

// TestCatalogServiceLessAppsNeedRealBodyEvidence 不变量①。
func TestCatalogServiceLessAppsNeedRealBodyEvidence(t *testing.T) {
	apps := Catalog()
	if len(apps) == 0 {
		t.Fatal("目录为空，这条门禁就失去意义了")
	}
	checked := 0
	for _, app := range apps {
		// 一键建站 / compose / Docker / 容器运行时各有自己的"装没装"语义，
		// 不走应用市场那条 install 判据（站点看站点、compose 看容器与记录）。
		if app.SiteApp != nil || app.Kind == KindCompose || app.Kind == KindDocker || app.Kind == KindColima {
			continue
		}
		// NoDaemon 是这一类缺陷的典型形态：装完什么都没有（没有服务、没有端口）。
		if app.NoDaemon {
			checked++
			if strings.TrimSpace(app.RuntimePath) == "" {
				t.Errorf("%s 标了 NoDaemon（没有常驻服务）却没有声明 RuntimePath —— "+
					"它的「已安装」就只能靠 brew 的一句话，brew 探测失败/缓存没刷新/"+
					"用户手工装时会把明明装着的东西显示成「安装」（2026-09-23 用户报障）", app.ID)
			}
			if isServiceRegisteredOnInstall(app) {
				t.Errorf("%s 既是 NoDaemon 又声明了服务标签（%q/%q）：两者会互相矛盾，"+
					"市场会给出一个点了必然报错的启停/纳管入口",
					app.ID, app.ServiceLabel, app.AdoptLabel)
			}
		}
		// 不登记服务的非 NoDaemon 条目（例如纯 brew 包）：同样必须有不依赖记录的证据。
		if !isServiceRegisteredOnInstall(app) && app.PanelInstaller != "" {
			checked++
			if app.BrewFormula == "" && strings.TrimSpace(app.RuntimePath) == "" {
				t.Errorf("%s 由面板安装但不登记服务，却没有 BrewFormula 也没有 RuntimePath"+
					" —— 没有任何「真实证据」能证明它装上了", app.ID)
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个条目都没检查到 —— 判据写错了，门禁等于没跑")
	}
}

// TestCatalogInstalledEvidenceImpliesUninstallPath 不变量②。
//
// 对每一条：用"目录声明的真实证据"造出已安装态（**没有任何服务记录**），
// 卸载计划必须给得出来。这正是 AGENTS 第三节"installed=true ⇒ 必须有可用的
// 卸载路径"在"没有服务记录"这一态下的落地。
func TestCatalogInstalledEvidenceImpliesUninstallPath(t *testing.T) {
	m := matrixManager(t)
	brewPrefix := filepath.Join(t.TempDir(), "brew")
	userHome := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(brewPrefix, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userHome, 0o755); err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, app := range Catalog() {
		if app.SiteApp != nil || app.Kind == KindCompose || app.Kind == KindDocker || app.Kind == KindColima {
			continue
		}
		// ① 造出"真实产物在"（RuntimePath 声明的那个），并确认探测结论成立。
		body := materializeRuntimeBody(t, app, brewPrefix, userHome)
		if strings.TrimSpace(app.RuntimePath) != "" && !body.Exists() {
			t.Errorf("%s：RuntimePath=%q 的产物已按声明造出来，DetectRuntimeBody 却说不在"+
				"（判据实现与声明不一致）", app.ID, app.RuntimePath)
		}
		// ② 「已安装」的证据：brew 包里装着，或磁盘上真实产物在。
		//    注意这里**没有任何服务记录** —— 这正是 NoDaemon 应用的正常状态。
		brew := BrewState{}
		if app.BrewFormula != "" {
			brew = BrewState{Formula: app.BrewFormula, Installed: true}
		}
		if !brew.Installed && !body.Exists() {
			continue // 这条没有"不依赖服务记录"的已安装证据，由不变量①负责报错
		}
		checked++
		plan := m.PlanUninstallForBrewFast(app, nil, brew)
		if plan.Kind == "none" {
			t.Errorf("%s：没有服务记录、但真实产物/brew 包证明它装着，卸载计划却是 kind=none"+
				"（blocked=%q）—— 用户会看到一个既显示已安装、又一颗收尾按钮都没有的卡片",
				app.ID, plan.Blocked)
		}
		if plan.Kind == "brew" && plan.Formula == "" {
			t.Errorf("%s：brew 计划里没有 formula，执行时无法知道该卸哪个包", app.ID)
		}
	}
	if checked < 5 {
		t.Fatalf("只检查到 %d 条，明显偏少 —— 判据可能把整类条目都跳过了", checked)
	}
}

// TestDetectRuntimeBodyRejectsDanglingAndIncomplete 锁住"不许谎报已安装"的那一半：
// 声明了安装体、但产物不完整时，判据必须**不成立**。
//
// 三条都是真机形态：
//   - brew uninstall 之后 `<prefix>/bin/vips` 是**悬空软链**（Cellar 已删）→ 不算安装；
//   - phpMyAdmin 的 web 根目录还在、入口文件没了 → 不算安装（空壳不是安装）；
//   - RuntimePath 指向一个**目录**却没写 RuntimeEntry → 不算安装（目录不是运行体）。
func TestDetectRuntimeBodyRejectsDanglingAndIncomplete(t *testing.T) {
	root := t.TempDir()
	brew := filepath.Join(root, "brew")
	home := filepath.Join(root, "home")

	// 悬空软链
	if err := os.MkdirAll(filepath.Join(brew, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(brew, "Cellar", "vips", "8.18.6", "bin", "vips"),
		filepath.Join(brew, "bin", "vips")); err != nil {
		t.Fatal(err)
	}
	vips := App{ID: "imgcompress", RuntimePath: "{brew}/bin/vips"}
	if b := DetectRuntimeBody(vips, brew, home); b.Exists() {
		t.Errorf("悬空软链不是运行体，不该判成已安装：%+v", b)
	}
	if b := DetectRuntimeBody(vips, brew, home); !b.Declared {
		t.Error("声明过 RuntimePath 时 Declared 必须是 true（用于区分『没声明』与『声明了但不在』）")
	}

	// 认不出的可执行位：文件在、但没有 x 位 → 不是可执行运行体
	if err := os.WriteFile(filepath.Join(brew, "bin", "vips2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b := DetectRuntimeBody(App{RuntimePath: "{brew}/bin/vips2"}, brew, home); b.Exists() {
		t.Errorf("没有可执行位的普通文件不是运行体：%+v", b)
	}

	// phpMyAdmin：目录在、入口文件不在
	pma := App{ID: "phpmyadmin", RuntimePath: "{brew}/share/phpmyadmin", RuntimeEntry: "index.php"}
	if err := os.MkdirAll(filepath.Join(brew, "share", "phpmyadmin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if b := DetectRuntimeBody(pma, brew, home); b.Exists() {
		t.Errorf("web 根里没有入口文件时不算装好：%+v", b)
	}
	if err := os.WriteFile(filepath.Join(brew, "share", "phpmyadmin", "index.php"), []byte("<?php\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b := DetectRuntimeBody(pma, brew, home); !b.Exists() || !b.IsDir {
		t.Errorf("目录 + 入口文件都在时应判成已安装（目录型）：%+v", b)
	}

	// 目录型却没写 RuntimeEntry → 一律不成立
	if b := DetectRuntimeBody(App{RuntimePath: "{brew}/share/phpmyadmin"}, brew, home); b.Exists() {
		t.Errorf("RuntimePath 指向目录但没写 RuntimeEntry 时不得判成已安装：%+v", b)
	}

	// 没声明 RuntimePath 的条目：这条证据不适用，Declared=false
	if b := DetectRuntimeBody(App{ID: "iopaint"}, brew, home); b.Declared || b.Exists() {
		t.Errorf("没声明 RuntimePath 时不该给任何产物证据：%+v", b)
	}
}

// TestNoDaemonCatalogEntriesKeepRuntimePathWired 是这一类的"点名"回归：
// 用户 2026-09-23 点名的条目必须在门禁里留下名字，避免将来被静默删掉声明。
//
// 2026-09-23 晚些时候用户又要求给图片压缩"加一个 webui 通过端口和别名调用"，
// 于是 imgcompress 从"纯 CLI"变成"CLI 引擎 + 面板托管的网页界面服务"：
// 它**不再**标 NoDaemon，但 RuntimePath（vips 可执行文件）这条安装体判据必须
// 继续保留 —— 下面分开断言这两类，免得把"新增了服务"误当成"判据可以删了"。
func TestNoDaemonCatalogEntriesKeepRuntimePathWired(t *testing.T) {
	// 纯 CLI / 网页入口（没有常驻进程）：必须标 NoDaemon。
	for _, id := range []string{"ffmpeg", "phpmyadmin"} {
		app, ok := FindApp(id)
		if !ok {
			t.Fatalf("目录里没有 %s（条目被删了？）", id)
		}
		if strings.TrimSpace(app.RuntimePath) == "" {
			t.Errorf("%s 必须有 RuntimePath（用户点名的那一类：装了却显示未装）", id)
		}
		if !app.NoDaemon {
			t.Errorf("%s 是纯 CLI/网页入口，应当标 NoDaemon（否则市场会去找不存在的服务）", id)
		}
	}
	// 图片压缩：有面板托管的网页界面服务（不标 NoDaemon），但安装体判据仍是 vips。
	img, ok := FindApp("imgcompress")
	if !ok {
		t.Fatal("目录里没有 imgcompress")
	}
	if strings.TrimSpace(img.RuntimePath) == "" {
		t.Error("imgcompress 必须有 RuntimePath（否则 brew 探测失败时又会显示未安装）")
	}
	if img.NoDaemon {
		t.Error("imgcompress 现在有常驻的网页界面服务，不该再标 NoDaemon")
	}
	if img.ServiceLabel != ImgCompressLabel {
		t.Errorf("imgcompress 的 ServiceLabel 应为 %s，实际 %q", ImgCompressLabel, img.ServiceLabel)
	}
	// 运行体必须就是面板安装时复核的那个命令名（vips）：
	// 声明漂移（写了别的二进制）会让"装了也判成没装"，正是本类缺陷的形态。
	if !strings.HasSuffix(img.RuntimePath, "/"+ImgCompressBinName) {
		t.Errorf("imgcompress 的 RuntimePath=%q 必须以 /%s 结尾（与 ImgCompressBinName 同源）",
			img.RuntimePath, ImgCompressBinName)
	}
}

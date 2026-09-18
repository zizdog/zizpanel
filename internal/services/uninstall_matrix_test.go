package services

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  动作矩阵门禁（全目录遍历）
//
//  为什么要有这一组测试（2026-09-21 用户第三次报"装上了卸不掉"）：
//  过去三次都是"用户点名一个应用 → 修那一个"，下一次换个应用又是同一类问题
//  （卸载入口缺失修了三轮、有文件就算已安装修了 Colima 还有别的条目）。
//  这里改成**遍历 Catalog() 的每一条**，把"状态 → 必须给得出的计划"钉成不变量：
//
//    · installed=true（brew 装着 / 有面板记录） ⇒ uninstall.kind != "none"
//      —— 说已安装却没有任何可卸载对象，本身就是自相矛盾；
//    · 有 PanelInstaller ⇒ 必须有卸载实现（HasInstallerUninstall），
//      且 installerPlan 给得出 kind=installer + 非空步骤；
//    · installed=false 且没有产物 ⇒ 允许 kind="none"（这时界面不给收尾按钮）。
//
//  "有安装器却没有卸载实现"这类新缺陷会在**测试阶段**失败，而不是等用户点下去。
//
//  同一个类里还有一条（2026-09-21 现场）：Homebrew 卸载后 `<prefix>/opt/php`
//  会留下**悬空软链接**，只 Readlink 就会把已经不在的版本列成"已安装"。
//  所以"已安装"的证据必须贴着运行体（keg 真在 / 包真的在 brew list 里），
//  这里的 BrewStateFor 用例专门锁住"悬空/别的版本不算已安装"。
// ============================================================================

// matrixManager 造一个"完全离线"的 Manager：不碰真实 brew、不读真实 launchd。
func matrixManager(t *testing.T) *Manager {
	t.Helper()
	m, _ := sandboxManager(t)
	m.opt.BrewBin = filepath.Join(t.TempDir(), "nonexistent-brew")
	m.launchdDirsOverride = []string{filepath.Join(t.TempDir(), "LaunchDaemons")}
	m.brewInstalledProbe = func(context.Context) (map[string]string, bool) {
		return map[string]string{}, true
	}
	m.brewUsesProbe = func(context.Context, string) ([]string, bool) { return nil, true }
	t.Cleanup(resetBrewUsesCache)
	return m
}

// TestCatalogUninstallActionMatrix 遍历整个应用目录，逐条断言动作矩阵。
func TestCatalogUninstallActionMatrix(t *testing.T) {
	m := matrixManager(t)
	ctx := context.Background()
	apps := Catalog()
	if len(apps) == 0 {
		t.Fatal("目录为空，这个门禁就失去意义了")
	}
	for _, app := range apps {
		// ---- 状态①：brew 装了、但面板里没有任何记录 ----
		if app.BrewFormula != "" {
			brew := BrewState{Formula: app.BrewFormula, Installed: true}
			plan := m.PlanUninstallForBrew(ctx, app, nil, brew)
			if plan.Kind == "none" {
				t.Errorf("%s：brew 装着却没给任何卸载路径（kind=none，blocked=%q）"+
					"—— 这就是用户看到的『甚至没有卸载按钮』", app.ID, plan.Blocked)
			}
			if plan.Kind == "brew" {
				if plan.Formula == "" {
					t.Errorf("%s：brew 计划里没有 formula，执行时无法知道该卸哪个包", app.ID)
				}
				if !hasStep(plan.Steps, "brew uninstall "+plan.Formula) {
					t.Errorf("%s：brew 计划里没有 `brew uninstall %s` 这一步：%v",
						app.ID, plan.Formula, plan.Steps)
				}
			}
		}

		// ---- 状态②：有面板记录（合成一条 managed=true 的服务记录） ----
		rec := &Service{
			Name: "syn-" + app.ID, DisplayName: app.Name, Kind: app.Kind,
			LaunchLabel: app.ServiceLabel, Managed: true,
		}
		brewInstalled := app.BrewFormula != ""
		brew := BrewState{Formula: app.BrewFormula, Installed: brewInstalled}
		plan := m.PlanUninstallForBrew(ctx, app, rec, brew)
		if plan.Kind == "none" {
			t.Errorf("%s：有面板记录却给不出卸载计划（kind=none，blocked=%q）", app.ID, plan.Blocked)
		}
		if plan.Kind == "brew" && !hasStep(plan.Steps, "brew uninstall "+plan.Formula) {
			t.Errorf("%s：托管记录走 brew 计划却没有 brew uninstall 这一步：%v", app.ID, plan.Steps)
		}
		// brew 原生托管服务：计划必须是 brew（只删服务定义的话，brew 包还在 ——
		// 用户点了"卸载"却什么都没删掉，这正是 php82 的形态）。
		if brewInstalled && app.PanelInstaller == "" &&
			app.Kind != KindCompose && app.Kind != KindDocker && plan.Kind != "brew" {
			t.Errorf("%s：brew 原生托管服务应给 brew 计划（真卸包），实际 %q：%v",
				app.ID, plan.Kind, plan.Steps)
		}

		// ---- 状态③：没装、没记录、没产物 → 允许 none（界面不给收尾按钮） ----
		empty := m.PlanUninstallForBrew(ctx, app, nil, BrewState{})
		if empty.Kind != "none" && empty.Kind != "installer" {
			// 只有"确实有残留"才会是 installer；这里沙箱家目录是空的，
			// 所以除了 none 不该有别的东西。
			t.Errorf("%s：没装没记录却给了 %q 计划（沙箱里没有任何残留）：%v",
				app.ID, empty.Kind, empty.Steps)
		}

		// ---- 状态④：PanelInstaller ⇒ 必须有卸载实现 + 可读计划 ----
		if app.PanelInstaller == "" {
			continue
		}
		if !HasInstallerUninstall(app.PanelInstaller) {
			t.Errorf("%s：PanelInstaller=%q 在 UninstallApp 里**没有卸载实现**"+
				"（装上就卸不掉）", app.ID, app.PanelInstaller)
		}
		ip := m.installerPlan(ctx, app)
		if ip.Kind != "installer" {
			t.Errorf("%s：安装器应用应给 installer 计划，实际 %q", app.ID, ip.Kind)
		}
		if ip.Blocked != "" {
			t.Errorf("%s：installer 计划被 blocked（%q）—— 目录里的应用必须给得出卸载步骤",
				app.ID, ip.Blocked)
		}
		if len(ip.Steps) == 0 {
			t.Errorf("%s：installer 计划没有任何步骤，确认框里将是一片空白", app.ID)
		}
	}
}

// TestBrewStateForRequiresRealEvidence 锁住"已安装"的证据必须是真实的。
//
// 同一个类（2026-09-21）：`<prefix>/opt/php` 是悬空软链接（指向已删掉的
// Cellar/php/8.4.7）时，只解析软链接就会把 8.4 列成"已安装"，
// 用户照着去 `brew uninstall php@8.4` 只会得到 "No such keg"。
// 所以：
//   - 表里没有这个 formula（悬空/已卸载）→ 不算已安装；
//   - 只装着**别的版本**（php 8.2）→ 不能证明 8.4 装着；
//   - 真装着 8.4（`php` keg 就是 8.4.7）→ 算已安装，且用真实写法 `php`。
func TestBrewStateForRequiresRealEvidence(t *testing.T) {
	if st := BrewStateFor("php@8.4", map[string]string{}); st.Installed {
		t.Error("brew 里没有这个包（悬空软链接/已卸载），却被判成已安装")
	}
	if st := BrewStateFor("php@8.4", map[string]string{"php": "8.2.33"}); st.Installed {
		t.Error("机器上只有 php 8.2，不能证明 php@8.4 装着")
	}
	st := BrewStateFor("php@8.4", map[string]string{"php": "8.4.7"})
	if !st.Installed || st.Formula != "php" {
		t.Errorf("php 8.4.7 应当被视为 php@8.4 的真实运行体，实际 %+v", st)
	}
	if st := BrewStateFor("php@8.2", map[string]string{"php@8.2": "8.2.33"}); !st.Installed || st.Formula != "php@8.2" {
		t.Errorf("精确匹配优先，实际 %+v", st)
	}
}

// TestPlanUninstallPhp84UsesRealFormula 是现场（php84）的端到端断言：
// 目录里写 php@8.4，机器上装的是 php 8.4.7（brew list 的名字是 php），
// 计划必须是 `brew uninstall php` —— 而不是没有按钮、也不是去卸 php@8.4
// （那只会得到 "No such keg"）。
func TestPlanUninstallPhp84UsesRealFormula(t *testing.T) {
	m := matrixManager(t)
	m.brewInstalledProbe = func(context.Context) (map[string]string, bool) {
		return map[string]string{"php": "8.4.7", "php@8.2": "8.2.33"}, true
	}
	plan := m.PlanUninstall(context.Background(), "php84")
	if plan.Kind != "brew" {
		t.Fatalf("php84 应给 brew 计划，实际 %q（blocked=%q）", plan.Kind, plan.Blocked)
	}
	if plan.Formula != "php" {
		t.Errorf("应卸真实安装的 formula `php`，实际 %q", plan.Formula)
	}
	if !hasStep(plan.Steps, "brew uninstall php") {
		t.Errorf("计划步骤里必须有 `brew uninstall php`：%v", plan.Steps)
	}
	// nginx 现场：brew 装着、面板没有记录、也没有残留目录 → 必须有卸载路径。
	m.brewInstalledProbe = func(context.Context) (map[string]string, bool) {
		return map[string]string{"nginx": "1.31.5"}, true
	}
	nplan := m.PlanUninstall(context.Background(), "nginx")
	if nplan.Kind != "brew" || nplan.Formula != "nginx" {
		t.Fatalf("nginx（brew 装着、无记录）应给 brew 计划，实际 %+v", nplan)
	}
}

// TestBrewPlanTellsTruthAboutDependents 依赖关系必须如实：
//   - 查到了 → 点名；
//   - 查了没有 → 明说已检查；
//   - 没查成 → 明说"未检查"，绝不假装没有依赖。
func TestBrewPlanTellsTruthAboutDependents(t *testing.T) {
	m := matrixManager(t)
	ctx := context.Background()
	app, _ := FindApp("nginx")
	if app.ID == "" {
		t.Fatal("目录里应有 nginx")
	}

	m.brewUsesProbe = func(context.Context, string) ([]string, bool) { return []string{"php@8.2"}, true }
	p := m.brewUninstallPlan(ctx, app, BrewState{Formula: "nginx", Installed: true}, true)
	if !hasStepContaining(p.Steps, "php@8.2") {
		t.Errorf("查到依赖时必须点名，实际 %v", p.Steps)
	}
	if !p.DependentsChecked {
		t.Error("查到了就应标记 checked")
	}

	m.brewUsesProbe = func(context.Context, string) ([]string, bool) { return nil, true }
	p = m.brewUninstallPlan(ctx, app, BrewState{Formula: "nginx", Installed: true}, true)
	if !hasStepContaining(p.Steps, "已检查") {
		t.Errorf("查了没有依赖时应明说已检查，实际 %v", p.Steps)
	}

	m.brewUsesProbe = func(context.Context, string) ([]string, bool) { return nil, false }
	p = m.brewUninstallPlan(ctx, app, BrewState{Formula: "nginx", Installed: true}, true)
	if !hasStepContaining(p.Steps, "未检查") {
		t.Errorf("没查成时必须说『未检查』（不许假装没有依赖），实际 %v", p.Steps)
	}
	if p.DependentsChecked {
		t.Error("没查成时 DependentsChecked 必须是 false")
	}
}

func hasStep(steps []string, want string) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
}

func hasStepContaining(steps []string, sub string) bool {
	for _, s := range steps {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

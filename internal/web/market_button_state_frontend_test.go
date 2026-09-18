package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ============================================================================
//  前端「已安装 / 安装 / 卸载」按钮状态与后端字段的一致性门禁
//
//  这一类缺陷（"装了却显示安装按钮 / 不在已安装里 / 装了却卸不掉"）有一半是
//  **前端判据漂移**造成的：后端老老实实给了 installed 与 uninstall.kind，
//  而某个页面自己用别的东西（有没有产物、plist 在不在、记录在不在）另算了一遍。
//  读了不报错，只会悄悄多/少一颗按钮，所以必须静态钉住。
//
//  这些断言读的是**真正被浏览器加载的** assets/js（不是测试里另抄一份规则），
//  与 market_plan_frontend_test.go 同一套做法。
// ============================================================================

// TestFrontendInstalledStateComesFromBackendFields 锁住"已安装"的唯一判据。
func TestFrontendInstalledStateComesFromBackendFields(t *testing.T) {
	apps := readAssetJS(t, "apps.js")
	// 市场卡片 / 安装按钮共用的那一份判据
	mustContain(t, "apps.js", apps, "function isInstalled(a) { return !!(a && (a.installed || a.adopted)); }")
	// 装完/卸完必须两边都重拉（市场条目与服务记录），否则「已安装」停在旧状态
	mustContain(t, "apps.js", apps, "async function refreshSilently()")
	mustContain(t, "apps.js", apps, "await fetchAll();")

	sp := readAssetJS(t, "servicePanel.js")
	// 「已安装」Tab 的合并判据：只有 installed / adopted 能把它收进列表。
	// 少了这一条，装了但没有服务记录的条目（纯 CLI / phpMyAdmin）永远不出现在
	// 「已安装」里 —— 正是用户报障的现象之一。
	mustContain(t, "servicePanel.js", sp, "if (!a || (!a.installed && !a.adopted)) continue;")
	// 管理面板里的动作组：没装就没有任何动作（更不会有卸载）
	mustContain(t, "servicePanel.js", sp, "const installed = !!(m && (m.installed || m.adopted)) || !!s;")
	mustContain(t, "servicePanel.js", sp, "if (!installed) return [];")

	// 安装态判定里的"未装"必须来自后端字段，绝不用产物/plist 另算一遍：
	// 那正是 2026-09-16"有产物就算已安装"与 2026-09-23"装了却显示未装"的同一类错误。
	assertInstalledNotDerivedFromArtifacts(t)
}

// TestFrontendInstallButtonOnlyWhenNotInstalled 锁住「装了就不该再出现安装按钮」。
func TestFrontendInstallButtonOnlyWhenNotInstalled(t *testing.T) {
	apps := readAssetJS(t, "apps.js")
	if n := strings.Count(apps, "actions.push(primaryButton(a))"); n != 1 {
		t.Errorf("apps.js 里 actions.push(primaryButton(a))（那颗「安装 / 添加到面板」）出现了 %d 次，"+
			"应该只有 1 次且在『未安装』分支里 —— 多出来的入口会让已安装的应用也显示安装按钮", n)
	}
	// 唯一那次必须在 else（未安装）分支：卡片的三种形态是
	// site_app / installed / 否则（安装）。
	notInstalled := regexp.MustCompile(`(?s)else\s*\{\s*actions\.push\(primaryButton\(a\)\)`)
	if !notInstalled.MatchString(apps) {
		t.Error("apps.js 的 appCard 必须只在『未安装』分支里给「安装」按钮" +
			"（installed 分支走 marketQuickActions 的「⚙️ 管理」，里面才是卸载）")
	}
	// 已安装分支必须存在，并且给的是市场动作组（打开 / 启停 / 管理）
	if !strings.Contains(apps, "} else if (installed) {") {
		t.Error("apps.js 的 appCard 缺少『已安装』分支")
	}
	if !strings.Contains(apps, "actions.push(...marketQuickActions(a, {") {
		t.Error("apps.js 的『已安装』分支必须走 marketQuickActions" +
			"（卡片上不再直接摆卸载，但必须给得出「⚙️ 管理」这条入口）")
	}
}

// TestFrontendUninstallButtonFollowsUninstallPlanKind 锁住「installed=true ⇒ 有卸载入口」
// 在前端这一侧的实现：收尾按钮只由**后端的 uninstall.kind**决定。
func TestFrontendUninstallButtonFollowsUninstallPlanKind(t *testing.T) {
	sp := readAssetJS(t, "servicePanel.js")
	// 有服务记录时：按计划 kind 决定给哪颗收尾按钮
	mustContain(t, "servicePanel.js", sp, "const pk = (mi && mi.uninstall && mi.uninstall.kind) || '';")
	mustContain(t, "servicePanel.js", sp, "if (pk === 'brew' || pk === 'installer' || pk === 'service') {")
	// 没有服务记录时（纯 CLI 应用 / phpMyAdmin 的常态）：判据仍然是 uninstall.kind
	mustContain(t, "servicePanel.js", sp, "mi.uninstall?.kind === 'installer' || mi.uninstall?.kind === 'service'")
	mustContain(t, "servicePanel.js", sp, "'brew') {")
	// kind 为空/未知时不得摆卸载按钮：那种情况下面板根本不知道该怎么卸
	mustContain(t, "servicePanel.js", sp, "} else if (s.managed) {")

	// 反向：任何把 `kind === 'none'` 与卸载按钮写到一起的实现都是错的
	//（计划说不出来怎么卸，界面就不该给卸载入口）。
	re := regexp.MustCompile(`(?m)^.*kind\s*===?\s*'none'.*$`)
	for _, line := range re.FindAllString(sp, -1) {
		if strings.Contains(line, "marketUninstallButton") || strings.Contains(line, "uninstallButton(") {
			t.Errorf("servicePanel.js：kind='none'（没有可卸载对象）时不得给卸载按钮：%s", strings.TrimSpace(line))
		}
	}
	// marketUninstallButton 的调用点必须都被 kind 判据包住（全文件只应有这两处 + 函数定义）
	if n := strings.Count(sp, "out.push(marketUninstallButton("); n != 2 {
		t.Errorf("marketUninstallButton 的调用点应恰好 2 处（有记录 / 无记录各一处），实际 %d 处 —— "+
			"多出来的入口会绕开 uninstall.kind 判据", n)
	}
}

// TestFrontendReportsUnverifiedBrewProbe 锁住"未复核"这件事在界面上说得出来。
//
// 后端在 brew 探测失败时给 brew_probe_ok=false；前端必须如实标出来，
// 而不是让用户以为自己的软件真的全没了（铁律 11）。
func TestFrontendReportsUnverifiedBrewProbe(t *testing.T) {
	apps := readAssetJS(t, "apps.js")
	mustContain(t, "apps.js", apps, "cache.brew_probe_ok === false")
	mustContain(t, "apps.js", apps, "未能复核已装软件")
}

// assertInstalledNotDerivedFromArtifacts 遍历**全部**前端 JS：任何给 `installed`
// 赋值的代码都不得用"有产物 / plist 在 / service_in_launchd"来算。
//
// 为什么遍历全目录：这类判据漂移的形态就是"下次在别的页面再算一遍"。
func assertInstalledNotDerivedFromArtifacts(t *testing.T) {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*(?:const|let|var)\s+installed\s*=\s*(.+?);?\s*$`)
	checked := 0
	for _, name := range listAssetJS(t) {
		if name == "api.js" {
			continue // 接口定义文件，不渲染按钮
		}
		js := readAssetJS(t, name)
		for _, m := range re.FindAllStringSubmatch(js, -1) {
			rhs := m[1]
			checked++
			for _, bad := range []string{"artifacts", "service_in_launchd"} {
				if strings.Contains(rhs, bad) {
					t.Errorf("%s：installed 由 %q 推导（%s）—— "+
						"「已安装」必须来自后端 installed/adopted，产物与 plist 只配叫残留/未注册", name, bad, rhs)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个前端 installed 赋值都没检查到 —— 正则或文件名猜错了，门禁等于没跑")
	}
}

func mustContain(t *testing.T, file, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s 里找不到这条契约：%q —— 前端与后端的按钮状态判据必须同源", file, needle)
	}
}

// listAssetJS 返回 assets/js 下的全部 .js 文件名（遍历全目录，理由见
// assertInstalledNotDerivedFromArtifacts 的注释）。
func listAssetJS(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("assets", "js"))
	if err != nil {
		t.Fatalf("读不到前端目录: %v", err)
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		out = append(out, e.Name())
	}
	if len(out) == 0 {
		t.Fatal("assets/js 里一个 .js 都没有 —— 目录猜错了，门禁等于没跑")
	}
	return out
}

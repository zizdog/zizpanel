package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  卸载的两个真机报障（2026-09-21 用户原文）
//
//  A. 卸载 python@3.13 失败：
//       Error: Refusing to uninstall /opt/homebrew/Cellar/python@3.13/3.13.3_1
//       because it is required by llvm and rust, which are currently installed.
//       You can override this and force removal with:
//         brew uninstall --ignore-dependencies python@3.13
//     面板把这段英文原文整段贴回给用户，用户既看不懂"谁依赖它"，
//     也看不到还能选"强制卸载"。
//
//  B. 卸载 PHP 8.4 成功，但 brew 打了一段吓人的 warning（列出整个
//     /opt/homebrew/etc/php，含 **8.2** 与 phpmyadmin 的配置），
//     而且顺手 `Autoremoving 2 unneeded formulae: net-snmp rtmpdump`。
//
//  这一组测试把四件事锁死：
//   1) 计划阶段就查 `brew uses --installed`，命中 → Blocked + 逐条 Dependents
//      （kind=brew） + ForceAllowed；
//   2) 只有显式 force 才出现 `--ignore-dependencies`，默认绝不出现；
//   3) 卸载命令一律带 HOMEBREW_NO_AUTOREMOVE=1（禁止顺手删别人的包）；
//   4) PHP 8.4 的卸载计划里**不得**出现 etc/php/8.2 或 phpmyadmin 的配置路径。
// ============================================================================

// uninstallForceManager 造一个完全离线的 Manager，并预置 PHP 的 etc 目录。
func uninstallForceManager(t *testing.T) (*Manager, string) {
	t.Helper()
	m := matrixManager(t)
	prefix := t.TempDir()
	m.opt.BrewBin = filepath.Join(prefix, "bin", "brew")
	t.Cleanup(resetBrewUsesCache)
	return m, prefix
}

// TestPlanBrewUninstallBlockedByInstalledDependents 锁住用户真机 A 的**计划阶段**行为。
func TestPlanBrewUninstallBlockedByInstalledDependents(t *testing.T) {
	m, _ := uninstallForceManager(t)
	ctx := context.Background()
	// 真机 `brew uses --installed python@3.13` 的输出形态（llvm、rust 各一行）。
	m.brewUsesProbe = func(context.Context, string) ([]string, bool) {
		return []string{"llvm", "rust"}, true
	}
	m.brewInstalledProbe = func(context.Context) map[string]string {
		return map[string]string{"python@3.13": "3.13.3_1"}
	}

	plan := m.PlanUninstall(ctx, "python313")
	// 面板上架的 Python 走 installer 计划（真正执行的是 brew uninstall python@3.13）；
	// 关键是它必须把 brew 依赖带出来 —— 这正是用户真机看到的缺口。
	if plan.Kind != "installer" {
		t.Fatalf("python313 的卸载计划应是 installer，实际 %q（blocked=%q）", plan.Kind, plan.Blocked)
	}
	if !strings.Contains(strings.Join(plan.Steps, "\n"), "brew uninstall python@3.13") {
		t.Errorf("计划里必须写清会执行 brew uninstall python@3.13：%v", plan.Steps)
	}
	if plan.Blocked == "" {
		t.Error("llvm/rust 依赖它时计划必须 Blocked（不能让用户点了才吃 brew 的英文原文）")
	}
	if !strings.Contains(plan.Blocked, "llvm") || !strings.Contains(plan.Blocked, "rust") {
		t.Errorf("Blocked 文案必须点名依赖方，实际：%s", plan.Blocked)
	}
	// 依赖方要结构化透出：kind=brew + Name=包名 + Action=可选动作。
	for _, want := range []string{"llvm", "rust"} {
		var got *Dependent
		for i := range plan.Dependents {
			if plan.Dependents[i].Kind == "brew" && plan.Dependents[i].Name == want {
				got = &plan.Dependents[i]
			}
		}
		if got == nil {
			t.Fatalf("依赖 %s 没有进 Dependents（界面就无法逐条列出）：%+v", want, plan.Dependents)
		}
		if !strings.Contains(got.Action, "强制卸载") {
			t.Errorf("%s 的 Action 必须给出「先卸载它 / 强制卸载」两个动作，实际 %q", want, got.Action)
		}
	}
	if !plan.ForceAllowed {
		t.Error("被 brew 依赖拦下时必须允许**用户选择**强制卸载（ForceAllowed=true）")
	}
	if plan.ForceNote == "" || !strings.Contains(plan.ForceNote, "llvm") {
		t.Errorf("ForceNote 要逐字写清会破坏哪些包，实际 %q", plan.ForceNote)
	}
}

// TestBrewUninstallCmdShape：命令形状——默认绝不加 --ignore-dependencies；
// force 时才加；两条路都必须带 HOMEBREW_NO_AUTOREMOVE=1。
func TestBrewUninstallCmdShape(t *testing.T) {
	m, prefix := uninstallForceManager(t)
	ctx := context.Background()

	type call struct {
		args []string
		env  []string
	}
	var calls []call
	m.brewSourceRunOverride = func(_ context.Context, _ time.Duration, src brewInstallSource, args ...string) (string, error) {
		calls = append(calls, call{args: append([]string(nil), args...), env: append([]string(nil), src.Env...)})
		return "", nil
	}

	if err := m.brewUninstall(ctx, "python@3.13", false, nil); err != nil {
		t.Fatalf("默认卸载不该失败：%v", err)
	}
	if err := m.brewUninstall(ctx, "python@3.13", true, nil); err != nil {
		t.Fatalf("强制卸载不该失败：%v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("应有 2 次 brew 调用，实际 %d", len(calls))
	}

	// ① 默认：brew uninstall python@3.13 —— 绝不能出现 --ignore-dependencies。
	if joined := strings.Join(calls[0].args, " "); joined != "uninstall python@3.13" {
		t.Errorf("默认卸载的命令形状必须是 `brew uninstall python@3.13`，实际 %q", joined)
	}
	if strings.Contains(strings.Join(calls[0].args, " "), "ignore-dependencies") {
		t.Error("默认卸载**绝不能**带 --ignore-dependencies（会让依赖它的包坏掉）")
	}
	// ② 强制：只有用户显式选择时才出现。
	if joined := strings.Join(calls[1].args, " "); joined != "uninstall --ignore-dependencies python@3.13" {
		t.Errorf("强制卸载的命令形状必须是 `brew uninstall --ignore-dependencies python@3.13`，实际 %q", joined)
	}
	// ③ 两条路都必须带 HOMEBREW_NO_AUTOREMOVE=1：brew 的 autoremove 会顺手删掉
	// 用户没点名的包（真机：卸 PHP 时带走了 net-snmp / rtmpdump）。
	for i, c := range calls {
		if brewEnvValue(c.env, "HOMEBREW_NO_AUTOREMOVE") != "1" {
			t.Errorf("第 %d 条卸载命令的环境变量里没有 HOMEBREW_NO_AUTOREMOVE=1：%v", i+1, c.env)
		}
	}
	// ④ root 路径（LaunchDaemon）也要带上：生产上面板以 root 跑，走的是
	//    `/usr/bin/env -u … KEY=VAL <brew> …` 这条 argv。
	args := brewEnvArgs(brewInstallSource{Env: m.brewEnv(ctx, "python@3.13")}, filepath.Join(prefix, "bin", "brew"),
		[]string{"uninstall", "python@3.13"})
	if !strings.Contains(strings.Join(args, " "), "HOMEBREW_NO_AUTOREMOVE=1") {
		t.Errorf("root（sudo/env）路径的 argv 里没有 HOMEBREW_NO_AUTOREMOVE=1：%v", args)
	}
}

// TestForceRejectedWhenNotAllowed：计划没查出依赖时，force 请求必须被拒
// （绝不因为前端多传一个参数就加破坏性开关）。
func TestForceRejectedWhenNotAllowed(t *testing.T) {
	m, _ := uninstallForceManager(t)
	m.brewSourceRunOverride = func(context.Context, time.Duration, brewInstallSource, ...string) (string, error) {
		t.Error("计划不允许强制卸载时，绝不该真的去跑 brew")
		return "", nil
	}
	app := App{ID: "python313", Name: "Python 3.13", BrewFormula: "python@3.13", PanelInstaller: "python"}
	plan := UninstallPlan{Kind: "brew", Formula: "python@3.13", Steps: []string{"brew uninstall python@3.13"}}
	if err := m.UninstallBrewApp(context.Background(), app, plan, false, true, nil); err == nil {
		t.Fatal("ForceAllowed=false 时传 force=true 必须被拒绝")
	}
}

// TestBrewUninstallErrTranslation：把 brew 的英文拒绝文案翻译成人话
// （用户真机 A 的原文必须能解析出 llvm / rust）。
func TestBrewUninstallErrTranslation(t *testing.T) {
	realOutput := `Error: Refusing to uninstall /opt/homebrew/Cellar/python@3.13/3.13.3_1
because it is required by llvm and rust, which are currently installed.
You can override this and force removal with:
  brew uninstall --ignore-dependencies python@3.13`
	deps := parseBrewRequiredBy(realOutput)
	if len(deps) != 2 || deps[0] != "llvm" || deps[1] != "rust" {
		t.Fatalf("真机原文应解析出 [llvm rust]，实际 %v", deps)
	}
	msg := brewUninstallErrText("python@3.13", false, realOutput, errors.New("exit status 1"))
	for _, want := range []string{"python@3.13", "llvm", "rust", "强制卸载", "ignore-dependencies"} {
		if !strings.Contains(msg, want) {
			t.Errorf("翻译后的失败文案缺少 %q：\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "原始输出") {
		t.Errorf("原始 brew 输出必须保留在细节里（排查要用），实际：\n%s", msg)
	}
	// 已经强制了还被拒：文案要变（不能又说"可以选择强制卸载"）。
	forced := brewUninstallErrText("python@3.13", true, realOutput, errors.New("exit status 1"))
	if strings.Contains(forced, "或选择「强制卸载」") {
		t.Errorf("已经强制过了，不该再建议「选择强制卸载」：\n%s", forced)
	}
	// 解析不出依赖时退回通用文案 + 原文，绝不编造包名。
	other := brewUninstallErrText("nginx", false, "Error: Refusing to uninstall nginx", errors.New("x"))
	if !strings.Contains(other, "brew uses --installed nginx") {
		t.Errorf("识别不出依赖方时要给出排查命令，实际：\n%s", other)
	}
}

// TestPhp84PlanNeverTouchesOtherVersions：用户真机 B —— 卸载 PHP 8.4 的计划里
// 不得出现 8.2（另一个版本）或 phpmyadmin 的配置；只允许**本版本自己**的目录；
// 父目录 etc/php 只有空了才允许删。
func TestPhp84PlanNeverTouchesOtherVersions(t *testing.T) {
	m, prefix := uninstallForceManager(t)
	ctx := context.Background()
	etcPhp := filepath.Join(prefix, "etc", "php")
	// 真机形态：8.2 与 8.4 并存，另有 phpmyadmin 的配置在同级目录。
	for _, d := range []string{filepath.Join(etcPhp, "8.2", "conf.d"), filepath.Join(etcPhp, "8.4", "php-fpm.d")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pma := filepath.Join(prefix, "etc", "phpmyadmin.config.inc.php")
	if err := os.WriteFile(pma, []byte("<?php\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.brewUsesProbe = func(context.Context, string) ([]string, bool) { return nil, true }
	m.brewInstalledProbe = func(context.Context) map[string]string {
		// 目录写 php@8.4，机器上装的是 php 8.4.7（真机形态见 ResolveBrewFormula）。
		return map[string]string{"php": "8.4.7", "php@8.2": "8.2.33"}
	}
	plan := m.PlanUninstall(ctx, "php84")
	if plan.Kind != "brew" || plan.Formula != "php" {
		t.Fatalf("php84 应给 `brew uninstall php` 的计划，实际 kind=%q formula=%q", plan.Kind, plan.Formula)
	}
	// 别名 `php` 无版本号 → 版本必须从 brew 报出的真实版本推出来（8.4.7 → 8.4）。
	if plan.PhpVersion != "8.4" {
		t.Fatalf("别名 php 的版本应从 brew 版本 8.4.7 推出 8.4，实际 %q", plan.PhpVersion)
	}
	all := strings.Join(append(append([]string{}, plan.Steps...), plan.DataPaths...), "\n")
	if strings.Contains(all, "etc/php/8.2") {
		t.Errorf("卸载 8.4 的计划里出现了**另一个版本**的路径 etc/php/8.2：\n%s", all)
	}
	if strings.Contains(all, "phpmyadmin") {
		t.Errorf("卸载 PHP 的计划里出现了 phpmyadmin 的配置路径（那是另一个应用的东西）：\n%s", all)
	}
	if !strings.Contains(all, filepath.Join("etc", "php", "8.4")) {
		t.Errorf("计划必须列出**本版本自己**的配置目录 etc/php/8.4：\n%s", all)
	}
	// brew 的 warning 里那些 8.2 路径必须在文案里被明确撇清（那正是用户的疑问）。
	if !strings.Contains(plan.KeepNote, "8.2") {
		t.Errorf("文案要如实说明「brew 的 warning 里那些 8.2 路径不属于本次卸载」：\n%s", plan.KeepNote)
	}
	if !strings.Contains(plan.KeepNote, "phpmyadmin") {
		t.Errorf("文案要如实说明 phpmyadmin 的配置不属于本次卸载：\n%s", plan.KeepNote)
	}

	// 执行：勾选删除配置 → 只删 8.4，8.2 与 phpmyadmin 配置必须原样还在；
	// 父目录 etc/php 里有别的东西 → 保留。
	if err := m.cleanupPHPConfigDirs(ctx, plan.PhpVersion, &InstallResult{}); err != nil {
		t.Fatalf("清理本版本配置失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(etcPhp, "8.4")); !os.IsNotExist(err) {
		t.Error("8.4 的配置目录应当被删掉")
	}
	if _, err := os.Stat(filepath.Join(etcPhp, "8.2", "conf.d")); err != nil {
		t.Errorf("8.2（另一个版本）的配置目录必须原样保留：%v", err)
	}
	if _, err := os.Stat(pma); err != nil {
		t.Errorf("phpmyadmin 的配置必须原样保留：%v", err)
	}
	if _, err := os.Stat(etcPhp); err != nil {
		t.Errorf("父目录 etc/php 里还有 8.2 与 phpmyadmin 配置，不该被删：%v", err)
	}
}

// TestPhpConfigParentRemovedOnlyWhenEmpty：父目录 etc/php 只有在空了才删。
func TestPhpConfigParentRemovedOnlyWhenEmpty(t *testing.T) {
	m, prefix := uninstallForceManager(t)
	etcPhp := filepath.Join(prefix, "etc", "php")
	if err := os.MkdirAll(filepath.Join(etcPhp, "8.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.cleanupPHPConfigDirs(context.Background(), "8.4", &InstallResult{}); err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if _, err := os.Stat(etcPhp); !os.IsNotExist(err) {
		t.Error("etc/php 空掉之后应当被删（否则每次卸载都留一个空目录）")
	}
}

// TestPhpPlanForVersionedFormula：目录 formula 带版本时（php@8.4），
// 也要能算出本版本目录；无版本别名（php）在没有真实 brew 版本时**不猜** ——
// 宁可不列，也绝不把别的版本写进要删的名单。
func TestPhpPlanForVersionedFormula(t *testing.T) {
	m, prefix := uninstallForceManager(t)
	etcPhp := filepath.Join(prefix, "etc", "php")
	if err := os.MkdirAll(filepath.Join(etcPhp, "8.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := m.phpVersionFor("php@8.4", ""); got != "8.4" {
		t.Errorf("php@8.4 的版本应是 8.4，实际 %q", got)
	}
	if got := m.phpConfigPathsFor("8.4"); len(got) != 1 || !strings.HasSuffix(got[0], filepath.Join("etc", "php", "8.4")) {
		t.Errorf("PHP 8.4 应只给出 etc/php/8.4，实际 %v", got)
	}
	if got := m.phpVersionFor("php", ""); got != "" {
		t.Errorf("别名 php 没有真实 brew 版本时不许猜版本，应返回空，实际 %q", got)
	}
	if got := m.phpConfigPathsFor(""); len(got) != 0 {
		t.Errorf("版本为空时不许列出任何配置目录（更不许列别的版本），实际 %v", got)
	}
	// brew 多版本串（`8.4.7 8.3.9`）取第一个；带重建后缀（8.4.7_1）也要认。
	if got := m.phpVersionFor("php", "8.4.7_1"); got != "8.4" {
		t.Errorf("8.4.7_1 → 8.4，实际 %q", got)
	}
	if got := m.phpVersionFor("php", "8.4.7 8.3.9"); got != "8.4" {
		t.Errorf("多版本串取第一个 → 8.4，实际 %q", got)
	}
	if got := phpVersionOfFormula("php@8.2"); got != "8.2" {
		t.Errorf("php@8.2 → 8.2，实际 %q", got)
	}
}

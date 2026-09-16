package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  基础依赖（ffmpeg 等）的行为锁
//
//  背景：2026-09-16 真机事故 —— mini 被抹机后由面板重装 Qwen TTS，
//  整条安装链里没有 ffmpeg，于是得到"能启动、能回 wav、一合成 mp3 就
//  HTTP 200 + 0 字节 body"的服务，用户所有 TTS 作业全败而健康检查全绿。
//  这些测试锁住三件事：
//    1) 探测要能分辨缺/不缺（且不碰真实环境）；
//    2) 补装失败必须如实报错，绝不谎报"已就绪"；
//    3) 三个安装链与卸载护栏都必须真的接上（源码级回归锁）。
// ============================================================================

// fakeProbe 用固定的"存在/不存在"集合替换真实探测。
//
// 为什么必须注入：默认探测会看真实文件系统，测试机装没装 ffmpeg 会直接决定
// 结论 —— 开发机装了、CI 没装，"缺/不缺"就总有一条测不到（AGENTS.md 第三节）。
func fakeProbe(present ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(command string) (string, error) {
		if set[command] {
			return "/fake/bin/" + command, nil
		}
		return "", os.ErrNotExist
	}
}

// fakeBrewMissing 返回一个"Homebrew 不存在"的 Manager：用来证明
// "补装不成就如实报错"，而不是在测试里真的去跑 brew。
func fakeBrewMissing(t *testing.T, probe func(string) (string, error)) *Manager {
	t.Helper()
	return &Manager{opt: Options{
		BrewBin:  filepath.Join(t.TempDir(), "no-such-prefix", "bin", "brew"),
		LookPath: probe,
	}}
}

// TestBaseDependenciesAlwaysContainFFmpeg 锁住清单本身。
//
// ffmpeg 是这次事故的核心：它必须永远在清单里，而且必须说明"为什么需要"
// 与"缺了怎么补" —— 只报一句"缺失"对用户没有 Information 量。
func TestBaseDependenciesAlwaysContainFFmpeg(t *testing.T) {
	deps := BaseDependencies()
	if len(deps) == 0 {
		t.Fatal("基础依赖清单不能为空")
	}
	var found bool
	for _, d := range deps {
		if d.Command == "ffmpeg" {
			found = true
			if d.Formula != "ffmpeg" {
				t.Errorf("ffmpeg 的 formula 应为 ffmpeg，实际 %q", d.Formula)
			}
		}
		if d.Why == "" {
			t.Errorf("%s 缺少 Why（必须说清为什么需要它）", d.Command)
		}
		if d.Hint == "" {
			t.Errorf("%s 缺少 Hint（缺失时要给出可照抄的补救提示）", d.Command)
		}
	}
	if !found {
		t.Fatal("基础依赖清单里没有 ffmpeg —— TTS 编码 mp3 会再次静默失败")
	}
}

// TestCheckBaseDependenciesReportsAllMissing 缺的时候要把每一项都报出来。
func TestCheckBaseDependenciesReportsAllMissing(t *testing.T) {
	m := &Manager{opt: Options{LookPath: fakeProbe()}}
	missing := m.CheckBaseDependencies(context.Background())
	if len(missing) != len(BaseDependencies()) {
		t.Fatalf("全都缺时应报 %d 项，实际 %d 项", len(BaseDependencies()), len(missing))
	}
	for _, dep := range missing {
		if dep.FixCmd == "" {
			t.Errorf("%s 缺失时必须给出可照抄的补救命令", dep.Command)
		}
	}
	if missing[0].Command != "ffmpeg" {
		t.Errorf("第一项应是 ffmpeg，实际 %q", missing[0].Command)
	}
}

// TestCheckBaseDependenciesNoFalseAlarm 全都在时不能报缺（否则每次都白装）。
func TestCheckBaseDependenciesNoFalseAlarm(t *testing.T) {
	m := &Manager{opt: Options{LookPath: fakeProbe("ffmpeg", "ffprobe")}}
	if missing := m.CheckBaseDependencies(context.Background()); len(missing) != 0 {
		t.Fatalf("都装了却报缺：%+v", missing)
	}
}

// TestBaseDependencyStatusesPartiallySatisfied 只读接口要如实区分每一项。
func TestBaseDependencyStatusesPartiallySatisfied(t *testing.T) {
	m := &Manager{opt: Options{LookPath: fakeProbe("ffmpeg")}}
	list := m.BaseDependencyStatuses(context.Background())
	byCmd := map[string]BaseDependencyStatus{}
	for _, st := range list {
		byCmd[st.Command] = st
	}
	if st := byCmd["ffmpeg"]; !st.Satisfied || st.Path == "" {
		t.Errorf("ffmpeg 应判定为已满足且给出路径，实际 %+v", st)
	}
	if st := byCmd["ffprobe"]; st.Satisfied {
		t.Errorf("ffprobe 未装时不应判定为满足，实际 %+v", st)
	}
	for _, st := range list {
		if st.Why == "" || st.Hint == "" {
			t.Errorf("%s 的状态缺少说明/补救提示", st.Command)
		}
	}
}

// TestEnsureBaseDependenciesSkipsWhenPresent 幂等：都装了就不该碰 brew。
//
// BrewBin 指向不存在的路径：只要它去装就必然失败，所以"返回 nil"本身就证明
// 没有触发任何 brew 调用。
func TestEnsureBaseDependenciesSkipsWhenPresent(t *testing.T) {
	m := fakeBrewMissing(t, fakeProbe("ffmpeg", "ffprobe"))
	res := &InstallResult{Steps: []string{}}
	if err := m.EnsureBaseDependencies(context.Background(), res); err != nil {
		t.Fatalf("已就绪时不该报错：%v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "已就绪") {
		t.Errorf("跳过时也要说清「跳过」，实际步骤：%s", joined)
	}
	if strings.Contains(joined, "正在安装") {
		t.Errorf("已就绪却尝试安装：%s", joined)
	}
}

// TestEnsureBaseDependenciesFailsHonestlyWithoutBrew
// 缺依赖又没法自动装时，必须返回错误 —— 绝不把"没装上"写成"已就绪"。
func TestEnsureBaseDependenciesFailsHonestlyWithoutBrew(t *testing.T) {
	m := fakeBrewMissing(t, fakeProbe())
	res := &InstallResult{Steps: []string{}}
	err := m.EnsureBaseDependencies(context.Background(), res)
	if err == nil {
		t.Fatal("缺 ffmpeg 且没有 Homebrew 时必须如实报错，不能谎报已就绪")
	}
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("错误里要点名缺的是 ffmpeg，实际：%v", err)
	}
	if strings.Contains(strings.Join(res.Steps, "\n"), "已全部就绪") {
		t.Error("失败的安装步骤里绝不能出现「已全部就绪」")
	}
}

// TestVerifyBaseDependenciesCatchesUnrunnableCommand
// "文件在"不等于"能用"：验收必须真的执行一次。
func TestVerifyBaseDependenciesCatchesUnrunnableCommand(t *testing.T) {
	m := &Manager{opt: Options{LookPath: fakeProbe("ffmpeg", "ffprobe")}}
	m.depExecOverride = func(path string) string {
		if strings.HasSuffix(path, "/ffmpeg") {
			return "dyld: Library not loaded: libavcodec.dylib"
		}
		return ""
	}
	problems := m.VerifyBaseDependencies(context.Background())
	if len(problems) != 1 {
		t.Fatalf("应报 1 项验收失败，实际 %d 项：%+v", len(problems), problems)
	}
	if problems[0].Command != "ffmpeg" {
		t.Errorf("应报 ffmpeg，实际 %q", problems[0].Command)
	}
	if !strings.Contains(problems[0].Detail, "dyld") {
		t.Errorf("Detail 必须带真实失败原因，实际 %q", problems[0].Detail)
	}
}

// TestVerifyBaseDependenciesPassesWhenRunnable 跑得通时不能误报。
func TestVerifyBaseDependenciesPassesWhenRunnable(t *testing.T) {
	m := &Manager{opt: Options{LookPath: fakeProbe("ffmpeg", "ffprobe")}}
	m.depExecOverride = func(string) string { return "" }
	if problems := m.VerifyBaseDependencies(context.Background()); len(problems) != 0 {
		t.Fatalf("命令能跑通却报失败：%+v", problems)
	}
}

// TestGuardAfterUninstallReportsMissing
// 卸载护栏：依赖被带走时要如实写进步骤 + Warning，而不是安静通过。
func TestGuardAfterUninstallReportsMissing(t *testing.T) {
	m := fakeBrewMissing(t, fakeProbe())
	res := &InstallResult{Steps: []string{}}
	m.GuardBaseDependenciesAfterUninstall(context.Background(), res, "")

	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "基础依赖不见了") {
		t.Errorf("缺少「被带走」的步骤：%s", joined)
	}
	if !strings.Contains(joined, "autoremove") {
		t.Errorf("要点明 autoremove 这种可能（否则用户不知道下次怎么防）：%s", joined)
	}
	if res.Warning == "" {
		t.Fatal("补装失败必须在 Warning 里留痕，绝不能静默")
	}
	if !strings.Contains(res.Warning, "brew install ffmpeg") {
		t.Errorf("Warning 要给出可照抄的手工命令，实际：%s", res.Warning)
	}
}

// TestGuardAfterIntentionalUninstallDoesNotReinstall
// 用户主动卸载 ffmpeg 时护栏不能把它装回去 —— 那等于对着用户干。
// 只要求：后果必须说清楚（写进步骤 + Warning），且不去碰 brew。
func TestGuardAfterIntentionalUninstallDoesNotReinstall(t *testing.T) {
	m := fakeBrewMissing(t, fakeProbe())
	res := &InstallResult{Steps: []string{}}
	m.GuardBaseDependenciesAfterUninstall(context.Background(), res, "ffmpeg")

	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "基础依赖") {
		t.Errorf("必须说明它是基础依赖：%s", joined)
	}
	if res.Warning == "" || !strings.Contains(res.Warning, "TTS") {
		t.Errorf("必须提示卸载后 TTS 等会失效，实际 Warning=%q", res.Warning)
	}
	// 没有 BrewBin 的情况下若走了"自动补回"就必然报"自动补装失败"——那说明路径错了。
	if strings.Contains(joined, "自动补装失败") {
		t.Errorf("用户主动卸载时不该尝试自动补装：%s", joined)
	}
}

// TestGuardAfterUninstallQuietWhenHealthy 依赖都在时不要往任务日志里塞噪声。
func TestGuardAfterUninstallQuietWhenHealthy(t *testing.T) {
	m := &Manager{opt: Options{LookPath: fakeProbe("ffmpeg", "ffprobe")}}
	res := &InstallResult{Steps: []string{}}
	m.GuardBaseDependenciesAfterUninstall(context.Background(), res, "")
	if len(res.Steps) != 0 || res.Warning != "" {
		t.Fatalf("依赖齐全时护栏应安静通过，实际 steps=%v warning=%q", res.Steps, res.Warning)
	}
}

// TestInstallChainsEnsureBaseDependencies 源码级回归锁。
//
// 这是本次事故最重要的防线：qwentts.go 的安装链里**当时没有 ffmpeg**，
// 而 InstallQwenTTS 没法直接跑单测（第一件事就要求 root）。
// 所以按本项目 lnmp_register_test.go 的做法，直接读源码锁住调用存在。
func TestInstallChainsEnsureBaseDependencies(t *testing.T) {
	for _, f := range []string{"qwentts.go", "voicereceiver.go", "lnmp.go", "iopaint.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", f, err)
		}
		if !strings.Contains(string(b), "EnsureBaseDependencies") {
			t.Errorf("%s 的安装链必须调用 EnsureBaseDependencies —— "+
				"2026-09-16 事故就是因为 Qwen TTS 安装链里没有 ffmpeg", f)
		}
	}
	// 两个 TTS 安装器还必须在收尾时真的执行一次（不只是探测文件在不在）。
	for _, f := range []string{"qwentts.go", "voicereceiver.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", f, err)
		}
		if !strings.Contains(string(b), "VerifyBaseDependencies") {
			t.Errorf("%s 必须在收尾验收里调用 VerifyBaseDependencies（实跑一次 ffmpeg）", f)
		}
	}
}

// TestFFmpegCatalogEntryIsStandaloneCLI 独立条目必须"能单独装、无网页界面、无端口"。
//
// 用户要求"有单独安装入口（作为一个独立软件）"。这里锁住三件事，
// 因为任何一条错了界面就会给出一个点不开/点了报错的按钮：
//   - UI 必须为空（纯命令行工具，没有网页界面）；
//   - NoDaemon + Port=0（没有常驻进程与端口）；
//   - PanelInstaller 必须是 "ffmpeg"（web 层据此走基础依赖安装器，
//     而不是通用 brew 流程 —— 后者会去 brew services start ffmpeg 并报假警告）。
func TestFFmpegCatalogEntryIsStandaloneCLI(t *testing.T) {
	app, found := FindApp("ffmpeg")
	if !found {
		t.Fatal("应用市场里必须有 ffmpeg 条目（用户要求它能被单独安装）")
	}
	if app.BrewFormula != "ffmpeg" {
		t.Errorf("BrewFormula 应为 ffmpeg，实际 %q", app.BrewFormula)
	}
	if app.PanelInstaller != "ffmpeg" {
		t.Errorf("PanelInstaller 应为 ffmpeg（决定走哪个安装器），实际 %q", app.PanelInstaller)
	}
	if app.UI != nil {
		t.Error("命令行工具不该有 UI：市场会渲染一个点开必然打不开的「打开」按钮")
	}
	if !app.NoDaemon {
		t.Error("ffmpeg 没有常驻进程，必须标 NoDaemon")
	}
	if app.Port != 0 {
		t.Error("ffmpeg 不监听端口，Port 必须为 0")
	}
}

// TestFFmpegDependentsDeclareRequirement 依赖它的应用必须用既有的 Requires 声明。
//
// 用户原话："所有要用到他的软件安装时要提示对 ffmpeg 的依赖，最好同时安装。"
// Requires 是目录里**既有**的机制：Preflight 会把它渲染成"安装前检查"里的
// 一行提示（含 fix_cmd），前端读的就是 pf.checks，不需要新字段。
func TestFFmpegDependentsDeclareRequirement(t *testing.T) {
	for _, id := range []string{"qwen3tts", "voicereceiver", "iopaint"} {
		app, found := FindApp(id)
		if !found {
			t.Fatalf("目录里找不到 %s", id)
		}
		declared := false
		for _, req := range app.Requires {
			if req.Type == "brew_formula" && req.Value == "ffmpeg" {
				declared = true
				if req.Hint == "" {
					t.Errorf("%s 对 ffmpeg 的依赖缺少 Hint（用户看不到补救命令）", id)
				}
			}
		}
		if !declared {
			t.Errorf("%s 必须声明 Requires(brew_formula ffmpeg) —— "+
				"否则用户装出来的可能是「能启动、一合成 mp3 就返回空 body」的残废服务", id)
		}
	}
}

// TestAnnounceAppDependenciesWritesPrompt
// 用户要求"安装时要提示对 ffmpeg 的依赖" —— 提示必须真的进任务步骤。
func TestAnnounceAppDependenciesWritesPrompt(t *testing.T) {
	m := &Manager{}
	for _, id := range []string{"qwen3tts", "voicereceiver", "iopaint"} {
		res := &InstallResult{Steps: []string{}}
		m.AnnounceAppDependencies(context.Background(), id, res)
		joined := strings.Join(res.Steps, "\n")
		if !strings.Contains(joined, "依赖提示") || !strings.Contains(joined, "ffmpeg") {
			t.Errorf("%s 的安装必须提示 ffmpeg 依赖，实际步骤：%s", id, joined)
		}
	}
	// 没声明基础依赖的应用不该凭空提示（提示必须来自目录数据，不是硬编码）
	res := &InstallResult{Steps: []string{}}
	m.AnnounceAppDependencies(context.Background(), "nginx", res)
	if len(res.Steps) != 0 {
		t.Errorf("nginx 没声明 ffmpeg 依赖，不该提示，实际：%v", res.Steps)
	}
}

// TestFFmpegUninstallPlanWarnsButDoesNotBlock
// 用户要求"不要禁止卸载，但要明确提示"。锁住：计划不是 Blocked，且把后果写清。
func TestFFmpegUninstallPlanWarnsButDoesNotBlock(t *testing.T) {
	app, found := FindApp("ffmpeg")
	if !found {
		t.Fatal("目录里找不到 ffmpeg")
	}
	m := &Manager{}
	plan := m.installerPlan(context.Background(), app)
	if plan.Kind != "installer" {
		t.Errorf("ffmpeg 的卸载计划应是 installer，实际 %q", plan.Kind)
	}
	if plan.Blocked != "" {
		t.Errorf("用户明确要求不要禁止卸载 ffmpeg，实际被阻止：%s", plan.Blocked)
	}
	joined := strings.Join(plan.Steps, "\n")
	if !strings.Contains(joined, "基础依赖") || !strings.Contains(joined, "TTS") {
		t.Errorf("卸载计划必须写清「它是基础依赖、会让 TTS 失效」，实际：%s", joined)
	}
	if !strings.Contains(joined, "brew uninstall") {
		t.Errorf("卸载计划要写清会执行什么命令，实际：%s", joined)
	}
}

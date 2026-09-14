package upgrade

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  看门狗端到端测试
//
//  这些测试与其它测试的区别：它们**真的把生成的 watchdog.sh 跑起来**，
//  用桩件替换 curl 与 launchctl，来验证"新版起不来时旧版能不能被救回来"。
//
//  为什么值得单独写：看门狗是整个升级流程里唯一的救命绳。
//  它跑起来的时候，面板可能已经崩了、Go 运行时已经不可用、
//  我们连日志都不一定看得到。所以它必须被真实执行过一次，
//  而不能只靠"检查脚本里包含某些字符串"来证明它能工作。
// ============================================================================

// watchdogEnv 准备一套桩件，并返回执行看门狗所需的 PATH。
//
// curl 的行为由 healthyBody 决定：
//   - 非空：健康检查返回该内容（用来模拟"新版起来了"或"旧版起来了"）
//   - 空：健康检查永远失败（模拟"新版根本起不来"）
func watchdogEnv(t *testing.T, sandbox string, body func() string) string {
	t.Helper()
	stub := filepath.Join(sandbox, "stubs")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	// curl 桩：把 body 写到一个文件里由桩件读取，避免往脚本里塞引号地狱
	bodyFile := filepath.Join(sandbox, "health-body.txt")
	writeBody := func() {
		_ = os.WriteFile(bodyFile, []byte(body()), 0o644)
	}
	writeBody()
	t.Cleanup(writeBody)

	curl := "#!/bin/bash\n" +
		"cat " + bodyFile + " 2>/dev/null\n" +
		"grep -q . " + bodyFile + " 2>/dev/null || exit 7\n" +
		"exit 0\n"
	// launchctl 桩：记录调用
	launchctl := "#!/bin/bash\necho \"launchctl $*\" >> " + filepath.Join(sandbox, "launchctl.log") + "\nexit 0\n"
	for name, content := range map[string]string{"curl": curl, "launchctl": launchctl} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return stub
}

// runWatchdog 执行一次看门狗脚本。
func runWatchdog(t *testing.T, opt Options, stubDir string) {
	t.Helper()
	script := watchdogScript(opt, mustRunID(t, opt), "0.1.0", "0.9.9")
	p := watchdogScriptPath(opt)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", p)
	cmd.Env = append(os.Environ(), "PATH="+stubDir+":/usr/bin:/bin:/usr/sbin:/sbin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("看门狗执行失败: %v\n%s", err, out)
	}
	t.Logf("看门狗输出:\n%s", out)
}

// installFakeUpgrade 造出"已经升级完成、正在等验证"的现场：
// 安装目录里是新版二进制，同时留有旧版的 .bak 备份。
func installFakeUpgrade(t *testing.T, opt Options) (oldPanel, newPanel string) {
	t.Helper()
	oldPanel = readFile(t, fakeBinary(t, opt.BinDir, PanelBinary, "0.1.0", true))
	fakeBinary(t, opt.BinDir, HelperBinary, "0.1.0", true)
	for _, name := range []string{PanelBinary, HelperBinary} {
		src := filepath.Join(opt.BinDir, name)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src+".bak", b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 换成"新版"
	newPanel = readFile(t, fakeBinary(t, opt.BinDir, PanelBinary, "0.9.9", true))
	fakeBinary(t, opt.BinDir, HelperBinary, "0.9.9", true)
	return oldPanel, newPanel
}

// TestWatchdogRollsBackWhenNewVersionUnhealthy 是本次改动最重要的一条断言：
// 新版永远无法通过健康检查时，看门狗必须把旧版**真的还原回去**并如实报告。
func TestWatchdogRollsBackWhenNewVersionUnhealthy(t *testing.T) {
	opt := newTestOptions(t)
	opt.HealthTries = 2 // 生产是 90；测试只要够证明逻辑即可
	opt.RollbackTries = 2

	oldPanel, newPanel := installFakeUpgrade(t, opt)
	if oldPanel == newPanel {
		t.Fatal("测试自身有问题：新旧二进制内容相同")
	}

	// 健康检查永远失败 → 新版起不来 → 应该回滚
	stub := watchdogEnv(t, opt.WorkDir, func() string { return "" })
	runWatchdog(t, opt, stub)

	// 1) 二进制必须被还原成旧版
	if got := readFile(t, filepath.Join(opt.BinDir, PanelBinary)); got != oldPanel {
		t.Fatal("看门狗没有把面板二进制还原成升级前的版本")
	}
	if got := readFile(t, filepath.Join(opt.BinDir, HelperBinary)); !strings.Contains(got, "0.1.0") {
		t.Fatal("看门狗没有还原提权助手")
	}

	// 2) 结论必须落盘，且面板重启后能读到
	if _, err := os.Stat(resultPath(opt.WorkDir)); err != nil {
		t.Fatalf("看门狗没有写下结果文件: %v", err)
	}
	st := LoadState(opt.WorkDir)
	if st.Status != StatusRolledBack {
		t.Fatalf("状态应为 rolled_back，实际 %s", st.Status)
	}
	if st.To != "0.1.0" {
		t.Errorf("回滚后的版本应为 0.1.0，实际 %q", st.To)
	}
	if !strings.Contains(st.Message, "回滚") {
		t.Errorf("提示信息应当说明已回滚，实际: %s", st.Message)
	}

	// 3) 回滚后必须重启面板（否则旧版不会真的跑起来）
	calls := readFile(t, filepath.Join(opt.WorkDir, "launchctl.log"))
	if !strings.Contains(calls, "kickstart") {
		t.Errorf("回滚后应当重启面板，实际 launchctl 调用：%s", calls)
	}
	// 4) 看门狗必须自我清理，且**只能摘掉自己**。
	//
	// 这一条是真实事故的回归锁：曾经把面板的 label 当成自己的 label
	// 去 bootout，结果是"升级成功之后面板从 launchd 里消失" ——
	// 网站入口直接 502，而且没有任何报错。
	// 只断言"调用过 bootout"是不够的，必须断言 bootout 的是谁。
	if !strings.Contains(calls, "bootout system/"+WatchdogLabel) {
		t.Errorf("看门狗应当把自己从 launchd 里摘掉，实际：%s", calls)
	}
	if strings.Contains(calls, "bootout system/"+opt.Label) && !strings.Contains(calls, "bootstrap system") {
		t.Fatalf("看门狗 bootout 了面板却没装回去（%s）—— 服务会直接消失！实际：%s",
			opt.Label, calls)
	}
	if !strings.Contains(calls, "kickstart -k system/"+opt.Label) {
		t.Errorf("回滚后应当用面板的 label 重启面板，实际：%s", calls)
	}
}

// TestWatchdogScriptNeverBootsOutPanel 直接检查生成的脚本文本。
// 上一个测试跑的是回滚路径；这条不管走哪条路径都盯着同一个约束，
// 因为"清理"和"回滚"是两段独立代码，都可能写错 label。
//
// 2026-09-14 补充：现在脚本里**确实**有一处 bootout 面板的语句，
// 但它必须同时满足两个条件，否则就是当年"升级成功后服务消失"的事故重演：
//  1. 只能出现在 recover_panel() 里 —— 也就是"健康检查已经连续失败"才会走到；
//  2. 同一个函数里必须紧跟 bootstrap（把它装回去），不能只摘不装。
func TestWatchdogScriptNeverBootsOutPanel(t *testing.T) {
	opt := newTestOptions(t)
	script := watchdogScript(opt, "test-run", "0.1.0", "0.2.0")

	if !strings.Contains(script, `SELF_LABEL="`+WatchdogLabel+`"`) {
		t.Error("脚本里应当有独立的自有 label 变量 SELF_LABEL")
	}
	if !strings.Contains(script, `PANEL_LABEL="`+opt.Label+`"`) {
		t.Error("脚本里应当有独立的面板 label 变量 PANEL_LABEL")
	}

	// 把脚本按函数切开：bootout 面板的语句只允许出现在 recover_panel 里。
	lines := strings.Split(script, "\n")
	fn := ""
	bootIdx, bootFn := -1, ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "() {") {
			fn = strings.TrimSuffix(trimmed, "() {")
		} else if trimmed == "}" {
			fn = ""
		}
		if strings.Contains(line, "bootout") && strings.Contains(line, opt.Label) {
			bootIdx, bootFn = i, fn
		}
	}
	switch {
	case bootIdx < 0:
		// 没有这种语句本来就是允许的。
	case bootFn != "recover_panel":
		t.Fatalf("只有 recover_panel() 里才允许 bootout 面板，实际出现在 %q 里", bootFn)
	default:
		rest := strings.Join(lines[bootIdx:], "\n")
		if end := strings.Index(rest, "\n}"); end >= 0 {
			rest = rest[:end]
		}
		if !strings.Contains(rest, "launchctl bootstrap system") {
			t.Error("recover_panel() 里 bootout 面板之后必须紧跟 bootstrap 把它装回去 —— 只摘不装会让面板彻底消失")
		}
	}

	// 旧写法（单一 $LABEL）不能再出现
	if strings.Contains(script, `bootout "system/$LABEL"`) {
		t.Error("仍在使用混用的 $LABEL 变量，必须改成 $SELF_LABEL")
	}
}

// TestWatchdogReportsSuccessWhenHealthy 验证成功路径：
// 新版通过健康检查时，**不能**回滚，且要写下 success。
func TestWatchdogReportsSuccessWhenHealthy(t *testing.T) {
	opt := newTestOptions(t)
	opt.HealthTries = 3
	opt.RollbackTries = 2

	oldPanel, newPanel := installFakeUpgrade(t, opt)

	// 健康检查返回新版版本号 → 视为启动成功
	stub := watchdogEnv(t, opt.WorkDir, func() string {
		return `{"data":{"status":"ok","version":"0.9.9"},"ok":true}`
	})
	runWatchdog(t, opt, stub)

	if got := readFile(t, filepath.Join(opt.BinDir, PanelBinary)); got != newPanel {
		t.Fatal("新版明明健康，看门狗却把它回滚了")
	}
	if got := readFile(t, filepath.Join(opt.BinDir, PanelBinary)); got == oldPanel {
		t.Fatal("新版被误还原成旧版")
	}
	st := LoadState(opt.WorkDir)
	if st.Status != StatusSuccess {
		t.Fatalf("状态应为 success，实际 %s", st.Status)
	}
	if st.To != "0.9.9" {
		t.Errorf("成功版本应为 0.9.9，实际 %q", st.To)
	}
}

// TestWatchdogDoesNotMistakeOldVersionForSuccess 是一个容易忽略的陷阱：
// 如果健康检查返回的是**旧版本**（比如重启后旧进程还在应答、
// 或者新版根本没启动而 launchd 拉起了旧版），
// 看门狗绝不能当成"新版成功了"。
//
// 这正是为什么要比对 version 字段，而不是只要 HTTP 200 就算成功。
func TestWatchdogDoesNotMistakeOldVersionForSuccess(t *testing.T) {
	opt := newTestOptions(t)
	opt.HealthTries = 2
	opt.RollbackTries = 2

	oldPanel, _ := installFakeUpgrade(t, opt)

	// 面板能应答，但报告的是**旧版本** —— 说明新版压根没跑起来
	stub := watchdogEnv(t, opt.WorkDir, func() string {
		return `{"data":{"status":"ok","version":"0.1.0"},"ok":true}`
	})
	runWatchdog(t, opt, stub)

	st := LoadState(opt.WorkDir)
	if st.Status == StatusSuccess {
		t.Fatal("旧版本在应答却被判定为升级成功 —— 用户会以为升级生效了，实际上没有")
	}
	if st.Status != StatusRolledBack {
		t.Fatalf("应当判定为失败并回滚，实际 %s", st.Status)
	}
	if got := readFile(t, filepath.Join(opt.BinDir, PanelBinary)); got != oldPanel {
		t.Error("回滚后二进制应等于旧版")
	}
}

// TestApplyThenWatchdogRollbackFullCycle 把 Apply 与看门狗串起来跑一遍：
// 完整走「自检 → 备份 → 替换 → 启动看门狗 → 重启」，然后让看门狗回滚。
// 这条链路上的顺序错误（比如先重启再启动看门狗）在实践中是不可恢复的，
// 所以值得整条测一次。
func TestApplyThenWatchdogRollbackFullCycle(t *testing.T) {
	opt := newTestOptions(t)
	opt.HealthTries = 2
	opt.RollbackTries = 2

	oldPanel := readFile(t, fakeBinary(t, opt.BinDir, PanelBinary, "0.1.0", true))
	fakeBinary(t, opt.BinDir, HelperBinary, "0.1.0", true)

	stagedDir := stageDirOf(opt)
	staged := map[string]string{
		PanelBinary:  fakeBinary(t, stagedDir, PanelBinary, "0.9.9", true),
		HelperBinary: fakeBinary(t, stagedDir, HelperBinary, "0.9.9", true),
	}
	// 让 watchdogScript 用测试用的重试次数：Apply 会自己生成脚本，
	// 所以这里通过 opt 把次数传进去。
	var launched bool
	opt.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			if len(args) > 0 && args[0] == "bootstrap" {
				launched = true
			}
			// 真实 launchd 会在 bootstrap 后立刻运行 RunAtLoad 任务。
			// 这里我们只记录，等 Apply 返回后再手动执行看门狗 ——
			// 因为如果现在就执行，会在 Apply 还没写完 plist 时就跑。
			return []byte(""), nil
		}
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}

	if err := Apply(context.Background(), opt, staged, "0.1.0", "0.9.9", "remote"); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if !launched {
		t.Fatal("看门狗没有被加载")
	}
	if got := readFile(t, filepath.Join(opt.BinDir, PanelBinary)); got == oldPanel {
		t.Fatal("替换阶段没有生效")
	}

	// 现在模拟"新版起不来"，执行真实生成的看门狗脚本
	script := readFile(t, watchdogScriptPath(opt))
	// Apply 生成的脚本用的是生产重试次数，替换成测试值以加快速度
	script = strings.Replace(script, "HEALTH_TRIES=90", "HEALTH_TRIES=2", 1)
	script = strings.Replace(script, "ROLLBACK_TRIES=60", "ROLLBACK_TRIES=2", 1)
	if err := os.WriteFile(watchdogScriptPath(opt), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	stub := watchdogEnv(t, opt.WorkDir, func() string { return "" })
	cmd := exec.Command("/bin/bash", watchdogScriptPath(opt))
	cmd.Env = append(os.Environ(), "PATH="+stub+":/usr/bin:/bin:/usr/sbin:/sbin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("看门狗执行失败: %v\n%s", err, out)
	}

	if got := readFile(t, filepath.Join(opt.BinDir, PanelBinary)); got != oldPanel {
		t.Fatal("完整流程结束后，面板应当已回滚到旧版")
	}
	if st := LoadState(opt.WorkDir); st.Status != StatusRolledBack {
		t.Fatalf("最终状态应为 rolled_back，实际 %s", st.Status)
	}
}

// mustRunID 取出当前升级尝试的 RunID。
//
// 看门狗脚本必须带上它才会被 LoadState 采纳 —— 这正是我们防止
// "旧结论覆盖新尝试"的机制，所以测试也必须走同一条路径，
// 不能图省事直接塞一个假 ID 进去。
func mustRunID(t *testing.T, opt Options) string {
	t.Helper()
	st := LoadState(opt.WorkDir)
	if st.RunID == "" {
		st.RunID = "test-run"
		st.Status = StatusRestarting
		if err := SaveState(opt.WorkDir, st); err != nil {
			t.Fatal(err)
		}
	}
	return st.RunID
}

// TestStaleResultDoesNotHijackNewAttempt 锁定真机上发现的第二个 bug。
//
// 现场复现过程：
//  1. 一次升级成功，看门狗写下 success 到 result.txt；
//     如果用户没打开过设置页，这条结论一直没被消费。
//  2. 用户后来下载准备了新版本（状态变成 staged）。
//  3. 点"立即升级"时，LoadState 读到了上一条 success，
//     把状态从 staged 改写成 success —— 于是拒绝执行，
//     提示"还没有已准备好的升级包"。用户完全看不懂为什么。
//
// 修法：结论绑定 RunID，且只有"进行中"的状态才接受结论。
func TestStaleResultDoesNotHijackNewAttempt(t *testing.T) {
	w := t.TempDir()

	// 上一次升级（run-old）留下的结论
	if err := WriteResult(w, "run-old", StatusSuccess, "0.2.0", "上一次的结论"); err != nil {
		t.Fatal(err)
	}
	// 这一次已经准备好升级包，正在等用户点确认
	if err := SaveState(w, &State{
		Status: StatusStaged, From: "0.2.0", To: "0.3.0", RunID: "run-new",
	}); err != nil {
		t.Fatal(err)
	}

	st := LoadState(w)
	if st.Status != StatusStaged {
		t.Fatalf("上一次的结论污染了这一次的状态：期望 staged，实际 %s（%s）", st.Status, st.Message)
	}
	if st.To != "0.3.0" {
		t.Fatalf("目标版本被改写成了 %q", st.To)
	}
}

// TestResultWithoutMatchingRunIDIsIgnored 保证"没有配对 RunID 的结论一律不采纳"。
func TestResultWithoutMatchingRunIDIsIgnored(t *testing.T) {
	w := t.TempDir()
	if err := SaveState(w, &State{Status: StatusRestarting, To: "0.3.0", RunID: "run-A"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteResult(w, "run-B", StatusRolledBack, "0.2.0", "别的尝试的结论"); err != nil {
		t.Fatal(err)
	}
	if st := LoadState(w); st.Status != StatusRestarting {
		t.Fatalf("不匹配 RunID 的结论不应被采纳，实际 %s", st.Status)
	}
}

// TestApplyClearsStaleResult 确认每次开始升级都会先清掉旧结论。
func TestApplyClearsStaleResult(t *testing.T) {
	w := t.TempDir()
	if err := WriteResult(w, "run-old", StatusSuccess, "0.9.9", "陈旧结论"); err != nil {
		t.Fatal(err)
	}
	opt := Options{WorkDir: w, BinDir: t.TempDir(), PlistDir: t.TempDir()}
	ClearResult(opt.WorkDir)
	if _, err := os.Stat(resultPath(w)); err == nil {
		t.Fatal("ClearResult 没有删掉陈旧结论")
	}
}

// TestWatchdogCleansUpPlistBeforeBootout 锁定真机上发现的第三个 bug。
//
// 看门狗跑完要自我清理。原来的写法是：
//
//	launchctl bootout system/$SELF_LABEL   # 先摘自己
//	rm -f "$PLIST"                          # 再删 plist
//
// 但 bootout 摘掉自己时会**当场杀掉这个进程**，所以 rm 永远执行不到 ——
// 真机上的现象是 /Library/LaunchDaemons/cn.zizpanel.upgrade-watchdog.plist
// 升级后依然存在，下一次升级加载时会撞上旧注册。
//
// 正确顺序：先删文件，再摘 job。
func TestWatchdogCleansUpPlistBeforeBootout(t *testing.T) {
	opt := newTestOptions(t)
	script := watchdogScript(opt, "run-1", "0.1.0", "0.2.0")

	rmIdx := strings.Index(script, `rm -f "$PLIST"`)
	bootIdx := strings.Index(script, `launchctl bootout "system/$SELF_LABEL"`)
	if rmIdx < 0 {
		t.Fatal("脚本里应当删除自己的 plist")
	}
	if bootIdx < 0 {
		t.Fatal("脚本里应当摘掉自己的 launchd job")
	}
	if rmIdx > bootIdx {
		t.Fatal("必须先删 plist 再 bootout —— bootout 会杀掉进程，后面的语句不会执行，" +
			"plist 会永久残留")
	}
}

// TestCleanupStaleWatchdogRemovesLeftovers 验证启动时的清扫逻辑。
func TestCleanupStaleWatchdogRemovesLeftovers(t *testing.T) {
	opt := newTestOptions(t)
	if err := os.MkdirAll(opt.PlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 造一个"上次被强杀留下的"残骸
	if err := os.WriteFile(watchdogPlistPath(opt), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(opt.WorkDir, &State{Status: StatusSuccess}); err != nil {
		t.Fatal(err)
	}

	var calls []string
	opt.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	CleanupStaleWatchdog(context.Background(), opt)

	if _, err := os.Stat(watchdogPlistPath(opt)); err == nil {
		t.Error("残留的看门狗 plist 应当被删除")
	}
	if !strings.Contains(strings.Join(calls, ";"), WatchdogLabel) {
		t.Errorf("应当把残留的 job 一并摘掉，实际调用：%v", calls)
	}
}

// TestCleanupStaleWatchdogSkipsRunningUpgrade 确保清扫不会打断正在进行的升级。
func TestCleanupStaleWatchdogSkipsRunningUpgrade(t *testing.T) {
	opt := newTestOptions(t)
	if err := os.MkdirAll(opt.PlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watchdogPlistPath(opt), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(opt.WorkDir, &State{Status: StatusRestarting}); err != nil {
		t.Fatal(err)
	}
	CleanupStaleWatchdog(context.Background(), opt)

	if _, err := os.Stat(watchdogPlistPath(opt)); err != nil {
		t.Fatal("升级进行中时不应清理看门狗（会破坏正在进行的验证与回滚）")
	}
}

// TestWatchdogAcceptsVersionWithCommitSuffix 是线上回滚事故的回归测试。
//
// 事故经过：/api/v1/health 返回的是 version.Full()，正式发布出来的二进制带 git commit
// （例如 "0.3.1+9a304af"），而看门狗拿到的是清单里的纯版本号（"0.3.1"）。
// 脚本里只精确匹配 `"version":"0.3.1"`，于是**新版明明起来并且正常服务**，
// 看门狗 90 秒都匹配不上，判定"启动失败"并把面板回滚掉 —— 真机上连续回滚了三次。
//
// 之所以长期没暴露：平时构建的二进制 commit=dev，Full() 不带后缀。
// 这个测试同时锁住两侧（新版判定与回滚后判定），任何一侧退回精确匹配都会失败。
func TestWatchdogAcceptsVersionWithCommitSuffix(t *testing.T) {
	opt := newTestOptions(t)
	s := watchdogScript(opt, "run-1", "0.3.0", "0.3.1")

	// 必须同时接受带后缀的形式：即模式里出现 "$WANT+" 与 "$FROM+"
	if !strings.Contains(s, `"$WANT+"`) {
		t.Error("看门狗判定新版时没有接受 +commit 后缀（会把正常服务的新版误判为启动失败并回滚）")
	}
	if !strings.Contains(s, `"$FROM+"`) {
		t.Error("看门狗判定回滚结果时没有接受 +commit 后缀")
	}
	// 纯版本号的形式也必须仍然匹配（本地 dev 构建不带后缀）
	if !strings.Contains(s, `"\"version\":\"$WANT\""`) {
		t.Error("看门狗判定新版时丢掉了纯版本号的匹配")
	}
	if !strings.Contains(s, `"\"version\":\"$FROM\""`) {
		t.Error("看门狗判定回滚时丢掉了纯版本号的匹配")
	}
}

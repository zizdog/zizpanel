package priv

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================================
//  launchd 域解析与"操作后验证"的回归测试
//
//  真机缺陷（Mac mini，面板 0.12.9，提交前已复现）：
//    `sh.brew.syncthing` 由面板以 `sudo -n -u zizdog brew ...` 加载，
//    作业落在 **user/501** 域，而旧代码只按 plist 路径断定 gui/501：
//      · POST .../stop  → HTTP 200 ok:true（cost≈8s），进程仍以同一 PID 监听 8384；
//      · POST .../start → HTTP 500 Bootstrap failed: 125: Domain does not support
//        specified action。
//    根因是旧 LaunchUnload：bootout 打偏域报错后，它用 LaunchStatus 复核，
//    而旧 LaunchStatus 把**任何**查询错误都当成"未加载" → 什么都没做也返回 nil。
//
//  这批测试全部跑在进程内的假 launchctl 上：不碰真实 launchd、不碰真实服务。
// ============================================================================

const testLabel = "sh.brew.syncthing"

// fakeLaunchctl 是一个进程内假 launchctl。
//
// 设计目标：把"候选域探测 → 操作 → 终态验证"整条逻辑跑通，而不是只替换
// LaunchStatus 一个函数。真机事故恰恰出在"操作命令的退出码被当成结论"，
// 只测单层假接口是抓不到的。
type fakeLaunchctl struct {
	mu sync.Mutex
	// loaded 记录每个域里是否有该作业。
	loaded map[string]bool
	// stopped 表示该域里作业已加载但**进程没在跑**（print 成功但没有 pid 行）。
	stopped map[string]bool
	// 各子命令按域返回的错误输出（"" 表示成功）。
	printErr     map[string]string
	bootoutErr   map[string]string
	bootstrapErr map[string]string
	kickstartErr map[string]string
	// afterFailedBootout 在 bootout 报错后调用，用于模拟
	// "命令报错、但作业其实已经不在了"的竞态。
	afterFailedBootout func(domain string)
	// bootoutGracePrints > 0 时模拟"bootout 是异步的"：命令返回后，
	// 该域还要再被 print 到这么多次才真的消失（真机上是 state = SIGTERMed）。
	bootoutGracePrints int
	grace              map[string]int
	// calls 按顺序记录调用，如 "bootout user/501/sh.brew.syncthing"。
	calls []string
}

func newFakeLaunchctl(loaded ...string) *fakeLaunchctl {
	f := &fakeLaunchctl{
		loaded:       map[string]bool{},
		stopped:      map[string]bool{},
		printErr:     map[string]string{},
		bootoutErr:   map[string]string{},
		bootstrapErr: map[string]string{},
		kickstartErr: map[string]string{},
		grace:        map[string]int{},
	}
	for _, d := range loaded {
		f.loaded[d] = true
	}
	return f
}

// domainOf 从 "user/501/label" 里取出 "user/501"（label 本身不含 /）。
func (f *fakeLaunchctl) domainOf(target string) string {
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[:i]
	}
	return target
}

func (f *fakeLaunchctl) log(sub, target string) {
	f.calls = append(f.calls, strings.TrimSpace(sub+" "+target))
}

func (f *fakeLaunchctl) run(_ string, args ...string) cmdResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) == 0 {
		return cmdResult{err: errors.New("fake launchctl: 缺少子命令")}
	}
	sub := args[0]

	switch sub {
	case "print", "bootout":
		if len(args) < 2 {
			return cmdResult{err: fmt.Errorf("fake launchctl: %s 缺少目标", sub)}
		}
		target := args[1]
		d := f.domainOf(target)
		f.log(sub, target)
		switch sub {
		case "print":
			if g := f.grace[d]; g > 0 {
				g--
				f.grace[d] = g
				if g == 0 {
					f.loaded[d] = false
				}
			}
			if msg := f.printErr[d]; msg != "" {
				return cmdResult{stderr: msg, err: errors.New("exit status 1")}
			}
			if f.loaded[d] {
				if f.stopped[d] {
					// 已加载但没在跑：真机上是 `state = spawn scheduled` 且没有 pid 行。
					return cmdResult{stdout: "state = spawn scheduled\n\tlast exit code = 0\n"}
				}
				return cmdResult{stdout: "state = running\n\tpid = 4242\n\tlast exit code = 0\n"}
			}
			// 与真机一致的 113 输出（见 launchOutputSaysMissing 的注释）。
			return cmdResult{
				stderr: fmt.Sprintf("Bad request.\nCould not find service %q in domain for %s", target, d),
				err:    errors.New("exit status 113"),
			}
		default: // bootout
			if msg := f.bootoutErr[d]; msg != "" {
				if f.afterFailedBootout != nil {
					f.afterFailedBootout(d)
				}
				return cmdResult{stderr: msg, err: errors.New("exit status 1")}
			}
			if f.bootoutGracePrints > 0 {
				f.grace[d] = f.bootoutGracePrints
				return cmdResult{}
			}
			f.loaded[d] = false
			return cmdResult{}
		}
	case "bootstrap":
		// bootstrap <domain> <plist>
		if len(args) < 3 {
			return cmdResult{err: errors.New("fake launchctl: bootstrap 缺少参数")}
		}
		d, plist := args[1], args[2]
		f.log(sub, d)
		if msg := f.bootstrapErr[d]; msg != "" {
			return cmdResult{stderr: msg, err: errors.New("exit status 1")}
		}
		if plist == "" {
			return cmdResult{err: errors.New("fake launchctl: plist 为空")}
		}
		f.loaded[d] = true
		return cmdResult{}
	case "kickstart":
		// kickstart -k <domain>/<label>
		if len(args) < 3 {
			return cmdResult{err: errors.New("fake launchctl: kickstart 缺少目标")}
		}
		target := args[2]
		d := f.domainOf(target)
		f.log(sub, target)
		if msg := f.kickstartErr[d]; msg != "" {
			return cmdResult{stderr: msg, err: errors.New("exit status 1")}
		}
		f.loaded[d] = true
		return cmdResult{}
	case "list":
		f.log(sub, "")
		// pid 兜底走不通；需要 pid 的用例都让 print 直接给出 pid。
		return cmdResult{err: errors.New("fake launchctl: list 未实现")}
	default:
		f.log(sub, "")
		return cmdResult{err: fmt.Errorf("fake launchctl: 未知子命令 %q", sub)}
	}
}

func (f *fakeLaunchctl) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeLaunchctl) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// count 数出以 prefix 开头的调用次数（门禁用：连发几次 kickstart）。
func (f *fakeLaunchctl) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// withFakeLaunch 用假 launchctl 与固定的候选域替换真实实现，测试结束自动恢复。
func withFakeLaunch(t *testing.T, f *fakeLaunchctl, domains ...string) {
	t.Helper()
	if len(domains) == 0 {
		domains = []string{"user/501", "gui/501"}
	}
	prevRun, prevDomains := runFn, launchDomainsFn
	prevWait, prevPoll := launchUnloadWait, launchUnloadPoll
	runFn = f.run
	launchDomainsFn = func(string) []string { return domains }
	// 默认不等待（真机默认 3 秒）；异步用例自己调大。
	launchUnloadWait, launchUnloadPoll = 0, time.Millisecond
	t.Cleanup(func() {
		runFn = prevRun
		launchDomainsFn = prevDomains
		launchUnloadWait, launchUnloadPoll = prevWait, prevPoll
	})
}

// withFakePlist 让 findPlist 只认给定的 plist（不碰真实 /Users 与 /Library）。
func withFakePlist(t *testing.T, plistPath string) {
	t.Helper()
	prevExists, prevHomes, prevUID := fileExistsFn, userHomesFn, uidOfHomeFn
	fileExistsFn = func(p string) bool { return p == plistPath }
	userHomesFn = func() []string { return []string{"/Users/tester"} }
	uidOfHomeFn = func(string) string { return "501" }
	t.Cleanup(func() {
		fileExistsFn, userHomesFn, uidOfHomeFn = prevExists, prevHomes, prevUID
	})
}

// ---------- 状态查询 ----------

func TestLaunchStatusProbesUserAndGUIDomains(t *testing.T) {
	cases := []struct {
		name       string
		loaded     []string
		printErr   map[string]string
		wantLoaded bool
		wantDomain string
		wantErr    bool
	}{
		{
			name:       "加载在 user/501，gui 说找不到",
			loaded:     []string{"user/501"},
			wantLoaded: true,
			wantDomain: "user/501",
		},
		{
			name:       "user 域报 125，gui 域能查到",
			loaded:     []string{"gui/501"},
			printErr:   map[string]string{"user/501": "Could not print domain: 125: Domain does not support specified action"},
			wantLoaded: true,
			wantDomain: "gui/501",
		},
		{
			name:       "所有候选域都明确说没有",
			wantLoaded: false,
			wantDomain: "user/501", // 未加载时报第一个候选域
		},
		{
			// 真机 2026-09-17（mini，headless：没有图形登录会话）：两个候选域都回
			// `Could not print domain: 125: Domain does not support specified action`
			// —— 那是"域不存在"，等于**这个域里没有该作业**，必须判为未加载。
			// 曾经把它当"查不了"→ stop 假失败（服务其实停了）、start 起不来。
			name: "两个域都不存在（headless 125）→ 判为未加载，不报错",
			printErr: map[string]string{
				"user/501": "Could not print domain: 125: Domain does not support specified action",
				"gui/501":  "Could not print domain: 125: Domain does not support specified action",
			},
			wantLoaded: false,
			wantDomain: "user/501",
		},
		{
			name:     "一个域说没有、另一个域查不了 → 不能断定未加载",
			printErr: map[string]string{"gui/501": "Could not print domain: 1: Operation not permitted"},
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLaunchctl(tc.loaded...)
			for d, msg := range tc.printErr {
				f.printErr[d] = msg
			}
			withFakeLaunch(t, f)

			st, err := LaunchStatus(testLabel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当报错（查不了不等于没加载），实际 st=%+v", st)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if st.Loaded != tc.wantLoaded {
				t.Fatalf("Loaded = %v，期望 %v（st=%+v）", st.Loaded, tc.wantLoaded, st)
			}
			if st.Domain != tc.wantDomain {
				t.Fatalf("Domain = %q，期望 %q", st.Domain, tc.wantDomain)
			}
			if tc.wantLoaded && !st.Running {
				t.Fatalf("print 已给出 pid，Running 应为 true：%+v", st)
			}
		})
	}
}

// ---------- 卸载 ----------

func TestLaunchUnloadVerifiesEndState(t *testing.T) {
	t.Run("加载在 user/501：bootout 该域并复核已消失", func(t *testing.T) {
		f := newFakeLaunchctl("user/501")
		withFakeLaunch(t, f)

		if err := LaunchUnload(testLabel); err != nil {
			t.Fatalf("应当成功: %v", err)
		}
		if !f.called("bootout user/501/" + testLabel) {
			t.Fatalf("应当 bootout user/501，实际调用：%v", f.callList())
		}
		if f.loaded["user/501"] {
			t.Fatal("假 launchd 里该作业仍在 user/501")
		}
	})

	t.Run("bootout 失败且作业仍在 → 必须报错（谎报成功的回归）", func(t *testing.T) {
		f := newFakeLaunchctl("user/501")
		f.bootoutErr["user/501"] = "Bootout failed: 125: Domain does not support specified action"
		withFakeLaunch(t, f)

		err := LaunchUnload(testLabel)
		if err == nil {
			t.Fatalf("bootout 失败且作业仍在，绝不能返回 nil（调用：%v）", f.callList())
		}
		if !strings.Contains(err.Error(), "仍加载在 user/501") {
			t.Fatalf("错误信息应指出作业仍在哪个域，实际: %v", err)
		}
	})

	t.Run("bootout 报错但复核确认已消失 → 成功（竞态不误报）", func(t *testing.T) {
		f := newFakeLaunchctl("user/501")
		f.bootoutErr["user/501"] = "Bootout failed: 3: No such process"
		f.afterFailedBootout = func(d string) { f.loaded[d] = false }
		withFakeLaunch(t, f)

		if err := LaunchUnload(testLabel); err != nil {
			t.Fatalf("复核已消失就该成功: %v", err)
		}
	})

	t.Run("bootout 异步：作业过一会儿才消失 → 等待后成功，不误报", func(t *testing.T) {
		f := newFakeLaunchctl("user/501")
		f.bootoutGracePrints = 2 // bootout 之后还要再被 print 到 2 次才消失
		withFakeLaunch(t, f)
		launchUnloadWait, launchUnloadPoll = 500*time.Millisecond, time.Millisecond

		if err := LaunchUnload(testLabel); err != nil {
			t.Fatalf("真实 launchd 的 bootout 是异步的，等待后应当成功: %v", err)
		}
	})

	t.Run("确认未加载 → 幂等成功且不 bootout", func(t *testing.T) {
		f := newFakeLaunchctl()
		withFakeLaunch(t, f)

		if err := LaunchUnload(testLabel); err != nil {
			t.Fatalf("幂等空操作应当成功: %v", err)
		}
		if f.called("bootout") {
			t.Fatalf("未加载时不该调用 bootout：%v", f.callList())
		}
	})
}

// ---------- 加载 ----------

func TestLaunchLoadPicksWorkingDomain(t *testing.T) {
	plist := "/Users/tester/Library/LaunchAgents/" + testLabel + ".plist"

	t.Run("已加载在 user/501 → kickstart，不 bootstrap", func(t *testing.T) {
		f := newFakeLaunchctl("user/501")
		withFakeLaunch(t, f)
		withFakePlist(t, plist)

		if err := LaunchLoad(testLabel); err != nil {
			t.Fatalf("应当成功: %v", err)
		}
		if !f.called("kickstart user/501/" + testLabel) {
			t.Fatalf("应当 kickstart user/501，实际：%v", f.callList())
		}
		if f.called("bootstrap") {
			t.Fatalf("已加载时不该再 bootstrap：%v", f.callList())
		}
	})

	t.Run("user 域 125、gui 域可用 → 回退成功", func(t *testing.T) {
		f := newFakeLaunchctl()
		f.bootstrapErr["user/501"] = "Bootstrap failed: 125: Domain does not support specified action"
		withFakeLaunch(t, f)
		withFakePlist(t, plist)

		if err := LaunchLoad(testLabel); err != nil {
			t.Fatalf("gui 域可用时应当成功: %v（调用：%v）", err, f.callList())
		}
		if !f.called("bootstrap user/501") || !f.called("bootstrap gui/501") {
			t.Fatalf("应当先试 user/501 再回退 gui/501：%v", f.callList())
		}
		if !f.loaded["gui/501"] {
			t.Fatalf("终态应在 gui/501 加载：%v", f.loaded)
		}
	})

	t.Run("gui 域 125、user 域可用 → 成功（user 优先）", func(t *testing.T) {
		f := newFakeLaunchctl()
		f.bootstrapErr["gui/501"] = "Bootstrap failed: 125: Domain does not support specified action"
		withFakeLaunch(t, f)
		withFakePlist(t, plist)

		if err := LaunchLoad(testLabel); err != nil {
			t.Fatalf("user 域可用时应当成功: %v（调用：%v）", err, f.callList())
		}
		if !f.called("bootstrap user/501") {
			t.Fatalf("应当优先 bootstrap user/501：%v", f.callList())
		}
	})

	t.Run("两个域都失败 → 报错并带上真实输出", func(t *testing.T) {
		f := newFakeLaunchctl()
		f.bootstrapErr["user/501"] = "Bootstrap failed: 125: Domain does not support specified action"
		f.bootstrapErr["gui/501"] = "Bootstrap failed: 125: Domain does not support specified action"
		withFakeLaunch(t, f)
		withFakePlist(t, plist)

		err := LaunchLoad(testLabel)
		if err == nil {
			t.Fatalf("两个域都失败必须报错（调用：%v）", f.callList())
		}
		if !strings.Contains(err.Error(), "125") {
			t.Fatalf("错误信息必须带上真实的 launchctl 输出，实际: %v", err)
		}
		if !strings.Contains(err.Error(), "gui/501") {
			t.Fatalf("错误信息应指出试过哪些域，实际: %v", err)
		}
	})
}

// ---------- 域解析 ----------

func TestLaunchDomainCandidates(t *testing.T) {
	label := "sh.brew.syncthing"

	cases := []struct {
		name   string
		plists map[string]bool
		want   []string
	}{
		{
			name:   "系统 LaunchDaemon → 只认 system",
			plists: map[string]bool{"/Library/LaunchDaemons/" + label + ".plist": true},
			want:   []string{"system"},
		},
		{
			name:   "用户 LaunchAgent → user 在前、gui 兜底",
			plists: map[string]bool{"/Users/tester/Library/LaunchAgents/" + label + ".plist": true},
			want:   []string{"user/501", "gui/501"},
		},
		{
			name:   "找不到 plist 也要探测所有候选域",
			plists: map[string]bool{},
			want:   []string{"system", "user/501", "gui/501"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prevExists, prevHomes, prevUID := fileExistsFn, userHomesFn, uidOfHomeFn
			fileExistsFn = func(p string) bool { return tc.plists[p] }
			userHomesFn = func() []string { return []string{"/Users/tester"} }
			uidOfHomeFn = func(string) string { return "501" }
			t.Cleanup(func() {
				fileExistsFn, userHomesFn, uidOfHomeFn = prevExists, prevHomes, prevUID
			})

			got := launchDomainCandidates(label)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("候选域 = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// ---------- 重启（kickstart） ----------

// 门禁（坑 225）：对已加载的作业，重启只能发**一次** kickstart。
// 连发两次时第二次会撞 launchd 的 10s 节流窗口 —— 真机实测把面板重启拖成 10.0s，
// 而命令行同级操作 <0.1s。
func TestLaunchKickstartKicksExactlyOnceWhenLoaded(t *testing.T) {
	f := newFakeLaunchctl("user/501")
	withFakeLaunch(t, f)

	if err := LaunchKickstart(testLabel); err != nil {
		t.Fatalf("重启应当成功: %v", err)
	}
	if n := f.count("kickstart"); n != 1 {
		t.Fatalf("已加载作业只应 kickstart 一次，实际 %d 次：%v", n, f.callList())
	}
}

// 门禁（坑 225）：未加载时 bootstrap 已按 RunAtLoad 拉起它，不得再补一次 kickstart。
func TestLaunchKickstartDoesNotDoubleKickAfterBootstrap(t *testing.T) {
	f := newFakeLaunchctl()
	withFakeLaunch(t, f)
	withFakePlist(t, "/Library/LaunchDaemons/"+testLabel+".plist")

	if err := LaunchKickstart(testLabel); err != nil {
		t.Fatalf("未加载时应 bootstrap 后成功: %v", err)
	}
	if !f.called("bootstrap user/501") {
		t.Fatalf("应当 bootstrap user/501：%v", f.callList())
	}
	if n := f.count("kickstart"); n != 0 {
		t.Fatalf("bootstrap 已拉起作业，不该再 kickstart（真机会撞 10s 节流），实际 %d 次：%v",
			n, f.callList())
	}
}

// ---------- 幂等启动（LaunchEnsureRunning） ----------

// 门禁（坑 225）：对**正在运行**的服务执行「启动」必须是空操作 ——
// 旧实现走 LaunchLoad，它会 kickstart -k 把服务白杀一次（真机实测 pid 变了），
// 而且刚起过的还会撞 launchd 的 10s 节流窗口（"启动也很慢"）。
func TestLaunchEnsureRunningIsNoOpWhenAlreadyRunning(t *testing.T) {
	f := newFakeLaunchctl("user/501")
	withFakeLaunch(t, f)

	if err := LaunchEnsureRunning(testLabel); err != nil {
		t.Fatalf("启动已在跑的服务应当成功: %v", err)
	}
	if n := f.count("kickstart"); n != 0 {
		t.Fatalf("已在跑就不该 kickstart（会白杀服务/撞节流），实际 %d 次：%v", n, f.callList())
	}
	if f.called("bootstrap") {
		t.Fatalf("已在跑就不该 bootstrap：%v", f.callList())
	}
}

// 门禁（坑 225）：已加载但没在跑 → 只 kickstart 一次；未加载 → 只 bootstrap。
func TestLaunchEnsureRunningKicksOrBootstrapsExactlyOnce(t *testing.T) {
	t.Run("已加载但没在跑 → 一次 kickstart", func(t *testing.T) {
		f := newFakeLaunchctl("user/501")
		f.stopped["user/501"] = true
		withFakeLaunch(t, f)

		if err := LaunchEnsureRunning(testLabel); err != nil {
			t.Fatalf("应当成功: %v", err)
		}
		if n := f.count("kickstart"); n != 1 {
			t.Fatalf("应 kickstart 恰好一次，实际 %d 次：%v", n, f.callList())
		}
		if f.called("bootstrap") {
			t.Fatalf("已加载就不该 bootstrap：%v", f.callList())
		}
	})

	t.Run("未加载 → 一次 bootstrap、零 kickstart", func(t *testing.T) {
		f := newFakeLaunchctl()
		withFakeLaunch(t, f)
		withFakePlist(t, "/Library/LaunchDaemons/"+testLabel+".plist")

		if err := LaunchEnsureRunning(testLabel); err != nil {
			t.Fatalf("应当成功: %v", err)
		}
		if !f.called("bootstrap user/501") {
			t.Fatalf("应当 bootstrap user/501：%v", f.callList())
		}
		if n := f.count("kickstart"); n != 0 {
			t.Fatalf("bootstrap 之后不该再 kickstart，实际 %d 次：%v", n, f.callList())
		}
	})
}

// TestLaunchOutputSaysMissing 用**真机采集到的原文**锁死"没有这个作业"的判据。
func TestLaunchOutputSaysMissing(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"Bad request.\nCould not find service \"x\" in domain for user gui: 501", true},
		{"Bad request.\nCould not find service \"x\" in domain for uid: 501", true},
		{"Bad request.\nCould not find service \"x\" in domain for system", true},
		{"Bad request.\nCould not find domain for user gui: 999", true},
		{"Could not print domain: 1: Operation not permitted", false},
		{"Bootstrap failed: 125: Domain does not support specified action", false},
		{"Bootout failed: 125: Domain does not support specified action", false},
		{"Bootstrap failed: 5: Input/output error", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := launchOutputSaysMissing(tc.out); got != tc.want {
			t.Errorf("launchOutputSaysMissing(%q) = %v，期望 %v", tc.out, got, tc.want)
		}
	}
}

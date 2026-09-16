package sysconfig

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================================
//  「允许免授权访问内网段」单测
//
//  这块功能的每一条命令都会写 macOS 偏好域，所以这里的原则是：
//    - 纯函数（CIDR 解析、defaults 输出解析、argv 拼装）用真机输出做输入；
//    - 执行路径全部换成 fakeDefaults，**绝不真的跑 defaults**，
//      也**绝不读写真实偏好域**，更不需要重启。
// ============================================================================

// fakeFileInfo 是最小的 os.FileInfo，用来给 defaultsSupported / 重启判断
// 提供"这个路径存在吗、什么时候改的"，而不去碰真实文件系统。
type fakeFileInfo struct {
	name string
	dir  bool
	mod  time.Time
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return f.mod }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

// fakeDefaults 在内存里模拟 `defaults`（含 sudo 包装）的读/写/删。
type fakeDefaults struct {
	mu         sync.Mutex
	store      map[string][]string
	calls      [][]string
	failWrites map[string]error
	failReads  map[string]error
	dropWrites bool // 写"成功"但不落库：用于验证"复核失败要报错"
}

func newFakeDefaults() *fakeDefaults {
	return &fakeDefaults{
		store:      map[string][]string{},
		failWrites: map[string]error{},
		failReads:  map[string]error{},
	}
}

// fdKey 把"以谁的身份 + 哪个域 + 哪个键"作为存储键。
// 系统域与用户域用的是**同一个裸域名**（Apple 官方命令），只有执行身份不同，
// 所以身份必须进 key，否则两个域的写入会互相覆盖、测试就测不出"两边都写了"。
func fdKey(user, domain, key string) string { return user + "\x00" + domain + "\x00" + key }

func (f *fakeDefaults) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))

	i := 0
	effUser := "root" // 系统域：面板自身以 root 直接执行 defaults
	if name == sudoBin {
		// ["-n","-u",user,defaultsBin,action,domain,key,...]
		if len(args) < 6 || args[3] != defaultsBin {
			return "", errors.New("fake: sudo argv 形状不对")
		}
		if args[2] == "" {
			return "", errors.New("fake: sudo 用户为空")
		}
		effUser = args[2]
		i = 4
	}
	action, domain, key := args[i], args[i+1], args[i+2]
	k := fdKey(effUser, domain, key)
	switch action {
	case "read":
		if err := f.failReads[k]; err != nil {
			return "", err
		}
		vals, ok := f.store[k]
		if !ok {
			return "The domain/default pair of (" + domain + ", " + key + ") does not exist\n",
				errors.New("exit status 1")
		}
		var sb strings.Builder
		sb.WriteString("(\n")
		for _, v := range vals {
			sb.WriteString("    \"" + v + "\",\n")
		}
		sb.WriteString(")\n")
		return sb.String(), nil
	case "write":
		if err := f.failWrites[k]; err != nil {
			return "", err
		}
		if len(args) < i+4 || args[i+3] != "-array" {
			return "", errors.New("fake: write 缺少 -array")
		}
		if !f.dropWrites {
			f.store[k] = append([]string{}, args[i+4:]...)
		}
		return "", nil
	case "delete":
		if err := f.failWrites[k]; err != nil {
			return "", err
		}
		if _, ok := f.store[k]; !ok {
			return "The domain/default pair of (" + domain + ", " + key + ") does not exist\n",
				errors.New("exit status 1")
		}
		delete(f.store, k)
		return "", nil
	}
	return "", errors.New("fake: 未知动作 " + action)
}

func (f *fakeDefaults) has(user, domain, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.store[fdKey(user, domain, key)]
	return ok
}

// setupLANTest 把本包所有会碰真实系统的注入点换掉，测试结束自动还原。
func setupLANTest(t *testing.T, root bool, user string) *fakeDefaults {
	t.Helper()
	fake := newFakeDefaults()

	prevRun := runCommand
	prevRoot := isRootFn
	prevUser := panelUserFn
	prevHome := userHomeFn
	prevStat := statFn
	prevBoot := bootTimeFn
	prevIf := ifconfigFn
	prevIfList := ifconfigListFn

	runCommand = fake.run
	isRootFn = func() bool { return root }
	panelUserFn = func() string { return user }
	userHomeFn = func(name string) string { return "/Users/" + name }
	// defaults 命令"存在"；两个 plist 都"不存在"（没有待生效改动）。
	statFn = func(p string) (os.FileInfo, error) {
		if p == defaultsBin {
			return fakeFileInfo{name: "defaults"}, nil
		}
		return nil, os.ErrNotExist
	}
	bootTimeFn = func(context.Context) time.Time { return time.Unix(1_000_000, 0) }
	ifconfigFn = func(context.Context, string) string { return "" }
	ifconfigListFn = func(context.Context) string { return "" }

	t.Cleanup(func() {
		runCommand = prevRun
		isRootFn = prevRoot
		panelUserFn = prevUser
		userHomeFn = prevHome
		statFn = prevStat
		bootTimeFn = prevBoot
		ifconfigFn = prevIf
		ifconfigListFn = prevIfList
	})
	return fake
}

// ---------- 纯函数 ----------

func TestParseLANCIDRsNormalizesAndDedupes(t *testing.T) {
	got, err := ParseLANCIDRs("192.168.1.5/24, 10.0.0.0/8;192.168.1.9/24")
	if err != nil {
		t.Fatalf("合法输入不该报错：%v", err)
	}
	want := []string{"192.168.1.0/24", "10.0.0.0/8"} // 归一 + 去重，保留首次出现顺序
	if len(got) != len(want) {
		t.Fatalf("应得到 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个应是 %q，实际 %q（全部 %v）", i, want[i], got[i], got)
		}
	}
}

func TestParseLANCIDRsRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"192.168.1.0/33", "not-a-cidr", "192.168.1.1"} {
		if _, err := ParseLANCIDRs(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestParseLANCIDRsEmptyIsEmptyNotError(t *testing.T) {
	got, err := ParseLANCIDRs("  ,  \n")
	if err != nil {
		t.Fatalf("空输入不该报错：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空输入应得到空列表，实际 %v", got)
	}
}

func TestParseIPv4CIDRFromRealIfconfigOutput(t *testing.T) {
	// 真机 en0 输出（2026-09-17 本机实测），netmask 是十六进制。
	real := "en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500\n" +
		"\toptions=6460<TSO4,TSO6,CHANNEL_IO,PARTIAL_CSUM,ZEROINVERT_CSUM>\n" +
		"\tether 7e:3b:8f:e1:c5:81\n" +
		"\tinet6 fe80::473:dff3:9c79:480b%en0 prefixlen 64 secured scopeid 0xb \n" +
		"\tinet 192.168.1.179 netmask 0xffffff00 broadcast 192.168.1.255\n" +
		"\tnd6 options=201<PERFORMNUD,DAD>\n"
	got, ok := ParseIPv4CIDR(real)
	if !ok || got != "192.168.1.0/24" {
		t.Fatalf("应解析出 192.168.1.0/24，实际 %q ok=%v", got, ok)
	}
}

func TestParseIPv4CIDRDottedNetmask(t *testing.T) {
	out := "\tinet 10.1.2.34 netmask 255.255.255.0 broadcast 10.1.2.255\n"
	got, ok := ParseIPv4CIDR(out)
	if !ok || got != "10.1.2.0/24" {
		t.Fatalf("应解析出 10.1.2.0/24，实际 %q ok=%v", got, ok)
	}
}

func TestParseIPv4CIDRSkipsInet6Only(t *testing.T) {
	out := "en1: flags=8863\n\tinet6 fe80::1%en1 prefixlen 64\n"
	if got, ok := ParseIPv4CIDR(out); ok {
		t.Fatalf("只有 inet6 时不该解析出 IPv4 CIDR，实际 %q", got)
	}
}

func TestInterfaceCandidatesPreferPhysical(t *testing.T) {
	got := interfaceCandidates("lo0 gif0 stf0 anpi0 utun0 awdl0 llw0 bridge0 ap1 en1 en0 vmenet0")
	want := []string{"en0", "en1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("应得到 %v，实际 %v", want, got)
	}
}

func TestInterfaceCandidatesFallbackIsNotHardcodedSubnet(t *testing.T) {
	got := interfaceCandidates("")
	if len(got) == 0 {
		t.Fatal("空输出也要给出候选接口，否则永远探测不到网段")
	}
	for _, n := range got {
		if strings.Contains(n, "/") {
			t.Fatalf("候选列表里只能是接口名，不该是网段：%v", got)
		}
	}
}

func TestParseDefaultsArray(t *testing.T) {
	got, err := parseDefaultsArray("(\n    \"192.168.1.0/24\",\n    \"10.0.0.0/8\"\n)\n")
	if err != nil {
		t.Fatalf("合法数组不该报错：%v", err)
	}
	if len(got) != 2 || got[0] != "192.168.1.0/24" || got[1] != "10.0.0.0/8" {
		t.Fatalf("解析结果不对：%v", got)
	}
	empty, err := parseDefaultsArray("(\n)\n")
	if err != nil || len(empty) != 0 {
		t.Fatalf("空数组应解析为空列表，实际 %v err=%v", empty, err)
	}
	// 非数组（例如有人把键写成了字符串）必须报错，不能猜。
	if _, err := parseDefaultsArray("192.168.1.0/24\n"); err == nil {
		t.Fatal("非数组形态应报错")
	}
}

func TestParseBootTime(t *testing.T) {
	got := parseBootTime("{ sec = 1789397632, usec = 424305 } Mon Sep 14 22:53:52 2026\n")
	if got.Unix() != 1789397632 {
		t.Fatalf("应解析出 1789397632，实际 %v", got.Unix())
	}
	if !parseBootTime("garbage").IsZero() {
		t.Fatal("解析不出来时应返回零值（由调用方按需要重启处理）")
	}
}

// ---------- argv 拼装 ----------

// TestLANCommandSystemDomainMatchesAppleOfficialCommand 锁住"系统域"的 argv
// 与 Apple TN3179 官方示例一字不差：
//
//	sudo defaults write com.apple.network.local-network <key> -array <cidr>
//
// 面板本身是 root，所以等价于直接跑裸域名（不需要再套一层 sudo）。
func TestLANCommandSystemDomainMatchesAppleOfficialCommand(t *testing.T) {
	tgt := lanTarget{Label: "系统域", Domain: lanPrefDomain}
	name, args := lanCommand(tgt, "write", lanWiFiKey, []string{"192.168.1.0/24", "10.0.0.0/8"})
	if name != defaultsBin {
		t.Fatalf("系统域应由 %s 直接执行（面板已是 root），实际 %s", defaultsBin, name)
	}
	want := []string{"write", lanPrefDomain, lanWiFiKey, "-array", "192.168.1.0/24", "10.0.0.0/8"}
	assertArgv(t, args, want)

	_, delArgs := lanCommand(tgt, "delete", lanEthernetKey, nil)
	assertArgv(t, delArgs, []string{"delete", lanPrefDomain, lanEthernetKey})

	_, readArgs := lanCommand(tgt, "read", lanEthernetKey, nil)
	assertArgv(t, readArgs, []string{"read", lanPrefDomain, lanEthernetKey})
}

func TestLANCommandUserDomainUsesRealUserViaSudo(t *testing.T) {
	tgt := lanTarget{Label: "用户域", Domain: lanPrefDomain, SudoUser: "zizdog"}
	name, args := lanCommand(tgt, "write", lanEthernetKey, []string{"192.168.1.0/24"})
	if name != sudoBin {
		t.Fatalf("用户域应以 %s 降权执行，实际 %s", sudoBin, name)
	}
	// 必须是 -n -u <真实用户>，且 defaults/动作/域/键/CIDR 都是**独立参数**
	// （不经过 shell，用户输入永远只是 argv 元素）。
	want := []string{"-n", "-u", "zizdog", defaultsBin, "write", lanPrefDomain, lanEthernetKey, "-array", "192.168.1.0/24"}
	assertArgv(t, args, want)
}

func TestLANTargetsUserDomainCarriesSudoUserOnlyWhenRoot(t *testing.T) {
	setupLANTest(t, true, "zizdog")
	targets := lanTargets()
	if len(targets) != 2 {
		t.Fatalf("应有两个目标域，实际 %d", len(targets))
	}
	if targets[0].SudoUser != "" {
		t.Errorf("系统域不该走 sudo，实际 %q", targets[0].SudoUser)
	}
	if targets[0].Domain != lanPrefDomain {
		t.Errorf("系统域应使用 Apple 官方裸域名，实际 %q", targets[0].Domain)
	}
	if targets[1].SudoUser != "zizdog" {
		t.Errorf("root 运行时用户域必须以 zizdog 降权，实际 %q", targets[1].SudoUser)
	}
	if len(targets[1].PlistPaths) != 1 ||
		!strings.Contains(targets[1].PlistPaths[0], "/Users/zizdog/Library/Preferences/") {
		t.Errorf("用户域 plist 路径不对：%v", targets[1].PlistPaths)
	}
	// 系统域的 mtime 候选要同时覆盖 /Library 与 root 家目录两个可能落点。
	if len(targets[0].PlistPaths) < 2 {
		t.Errorf("系统域应有两个 plist 候选（/Library 与 /var/root），实际 %v", targets[0].PlistPaths)
	}
}

// ---------- 探针 ----------

func TestDetectLANPreauthUnset(t *testing.T) {
	setupLANTest(t, true, "zizdog")
	st := DetectLANPreauth(context.Background())
	if !st.Supported || !st.Readable {
		t.Fatalf("应是 supported+readable，实际 %+v", st)
	}
	if st.Enabled || st.SystemSet || st.UserSet {
		t.Fatalf("没写过时不该显示已启用：%+v", st)
	}
	if st.Warning == "" {
		t.Fatal("必须把「对所有程序生效」的警告带回给前端")
	}
}

func TestDetectLANPreauthReportsReadFailureInsteadOfGuessing(t *testing.T) {
	fake := setupLANTest(t, true, "zizdog")
	fake.failReads[fdKey("root", lanPrefDomain, lanEthernetKey)] = errors.New("Operation not permitted")
	st := DetectLANPreauth(context.Background())
	if st.Readable {
		t.Fatal("读不到时 Readable 必须是 false（不能猜一个状态）")
	}
	if !strings.Contains(st.ReadError, "Operation not permitted") {
		t.Fatalf("ReadError 应带上真实原因，实际 %q", st.ReadError)
	}
	if st.Enabled {
		t.Fatal("读不到时绝不能声称已启用")
	}
}

func TestDetectLANPreauthRebootPendingFromPlistMtime(t *testing.T) {
	setupLANTest(t, true, "zizdog")
	boot := time.Unix(1_000_000, 0)
	bootTimeFn = func(context.Context) time.Time { return boot }
	statFn = func(p string) (os.FileInfo, error) {
		switch p {
		case defaultsBin:
			return fakeFileInfo{name: "defaults"}, nil
		case "/Users/zizdog/Library/Preferences/" + lanPrefDomain + ".plist":
			return fakeFileInfo{name: "user.plist", mod: boot.Add(time.Hour)}, nil
		}
		return nil, os.ErrNotExist
	}
	st := DetectLANPreauth(context.Background())
	if !st.RebootRequired {
		t.Fatal("偏好文件在开机之后被改过 → 必须提示需要重启")
	}
	if st.RebootNote == "" {
		t.Fatal("需要重启时要说明依据")
	}

	// 开机之前写的 → 说明本次运行已经带着这个设置，不需要再重启。
	statFn = func(p string) (os.FileInfo, error) {
		switch p {
		case defaultsBin:
			return fakeFileInfo{name: "defaults"}, nil
		case "/Users/zizdog/Library/Preferences/" + lanPrefDomain + ".plist":
			return fakeFileInfo{name: "user.plist", mod: boot.Add(-time.Hour)}, nil
		}
		return nil, os.ErrNotExist
	}
	st = DetectLANPreauth(context.Background())
	if st.RebootRequired {
		t.Fatal("开机前就写好的设置不该提示需要重启")
	}
}

func TestDetectPrimaryCIDRFromInjectedIfconfig(t *testing.T) {
	setupLANTest(t, true, "zizdog")
	ifconfigListFn = func(context.Context) string { return "lo0 en0 en1" }
	ifconfigFn = func(_ context.Context, iface string) string {
		if iface == "en1" {
			return "\tinet 10.9.8.7 netmask 0xffffff00 broadcast 10.9.8.255\n"
		}
		return "en0: flags=8863\n" // 没插网线：没有 inet
	}
	if got := DetectPrimaryCIDR(context.Background()); got != "10.9.8.0/24" {
		t.Fatalf("应从 en1 推导出 10.9.8.0/24，实际 %q", got)
	}
}

// ---------- 应用 / 撤销 ----------

func TestApplyLANPreauthWritesBothKeysBothDomainsAndVerifies(t *testing.T) {
	fake := setupLANTest(t, true, "zizdog")
	var logs []string
	log := func(level, text string) { logs = append(logs, level+":"+text) }

	if err := ApplyLANPreauth(context.Background(), "192.168.1.0/24", log); err != nil {
		t.Fatalf("应用应成功：%v", err)
	}
	// 两个域 × 两个键都要写。
	for _, tgt := range []struct{ user, domain string }{
		{"root", lanPrefDomain},   // 系统域：面板以 root 执行
		{"zizdog", lanPrefDomain}, // 用户域：sudo -u zizdog
	} {
		for _, key := range []string{lanEthernetKey, lanWiFiKey} {
			if !fake.has(tgt.user, tgt.domain, key) {
				t.Errorf("缺少写入：user=%s domain=%s key=%s", tgt.user, tgt.domain, key)
			}
		}
	}
	// 用户域的写入必须以真实用户身份执行。
	if !hasSudoUserCall(fake.calls, "zizdog") {
		t.Fatalf("用户域写入没有以 zizdog 身份执行：%v", fake.calls)
	}
	// 复核：重新探测应显示已启用。
	st := DetectLANPreauth(context.Background())
	if !st.Enabled || !st.SystemSet || !st.UserSet {
		t.Fatalf("复核应显示两个域都已写入，实际 %+v", st)
	}
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), "重启后生效") {
		t.Fatalf("应用日志必须说明重启后生效，实际 %v", logs)
	}
}

func TestApplyLANPreauthFailsOnWriteError(t *testing.T) {
	fake := setupLANTest(t, true, "zizdog")
	fake.failWrites[fdKey("root", lanPrefDomain, lanEthernetKey)] = errors.New("write permission denied")
	err := ApplyLANPreauth(context.Background(), "192.168.1.0/24", nil)
	if err == nil {
		t.Fatal("写入失败必须返回真实错误，绝不能报成功")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("错误里应带上真实原因，实际 %v", err)
	}
}

func TestApplyLANPreauthFailsWhenVerificationMismatch(t *testing.T) {
	fake := setupLANTest(t, true, "zizdog")
	fake.dropWrites = true // 命令"成功"但值没落库
	err := ApplyLANPreauth(context.Background(), "192.168.1.0/24", nil)
	if err == nil {
		t.Fatal("复核读不回来时必须报错")
	}
	if !strings.Contains(err.Error(), "复核失败") {
		t.Fatalf("错误应说明是复核失败，实际 %v", err)
	}
}

func TestApplyLANPreauthRejectsInvalidAndEmpty(t *testing.T) {
	setupLANTest(t, true, "zizdog")
	if err := ApplyLANPreauth(context.Background(), "192.168.1.0/33", nil); err == nil {
		t.Fatal("非法 CIDR 应被拒绝")
	}
	if err := ApplyLANPreauth(context.Background(), "  ", nil); err == nil {
		t.Fatal("空网段应被拒绝")
	}
}

func TestApplyLANPreauthRequiresRoot(t *testing.T) {
	setupLANTest(t, false, "zizdog")
	if err := ApplyLANPreauth(context.Background(), "192.168.1.0/24", nil); err == nil {
		t.Fatal("非 root 必须拒绝（否则会谎报成功）")
	}
}

func TestRollbackLANPreauthDeletesBothDomainsAndVerifies(t *testing.T) {
	fake := setupLANTest(t, true, "zizdog")
	// 先写好两个域 × 两个键。
	for _, tgt := range []struct{ user, domain string }{
		{"root", lanPrefDomain}, {"zizdog", lanPrefDomain},
	} {
		for _, key := range []string{lanEthernetKey, lanWiFiKey} {
			fake.store[fdKey(tgt.user, tgt.domain, key)] = []string{"192.168.1.0/24"}
		}
	}
	var logs []string
	if err := RollbackLANPreauth(context.Background(), func(level, text string) {
		logs = append(logs, level+":"+text)
	}); err != nil {
		t.Fatalf("撤销应成功：%v", err)
	}
	for _, tgt := range []struct{ user, domain string }{
		{"root", lanPrefDomain}, {"zizdog", lanPrefDomain},
	} {
		for _, key := range []string{lanEthernetKey, lanWiFiKey} {
			if fake.has(tgt.user, tgt.domain, key) {
				t.Errorf("没有删掉：user=%s domain=%s key=%s", tgt.user, tgt.domain, key)
			}
		}
	}
	st := DetectLANPreauth(context.Background())
	if st.Enabled || st.SystemSet || st.UserSet {
		t.Fatalf("撤销后不该还有设置：%+v", st)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "重启后") {
		t.Fatalf("撤销日志必须说明重启后恢复，实际 %v", logs)
	}
}

func TestRollbackLANPreauthToleratesMissingKeys(t *testing.T) {
	setupLANTest(t, true, "zizdog")
	if err := RollbackLANPreauth(context.Background(), nil); err != nil {
		t.Fatalf("本来就没设置时撤销应视为幂等成功：%v", err)
	}
}

func TestRollbackLANPreauthFailsOnDeleteError(t *testing.T) {
	fake := setupLANTest(t, true, "zizdog")
	fake.store[fdKey("root", lanPrefDomain, lanWiFiKey)] = []string{"10.0.0.0/8"}
	fake.failWrites[fdKey("root", lanPrefDomain, lanWiFiKey)] = errors.New("Operation not permitted")
	if err := RollbackLANPreauth(context.Background(), nil); err == nil {
		t.Fatal("删除失败必须返回真实错误")
	}
}

// ---------- 断言小工具 ----------

func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv 长度应为 %d，实际 %d\n得到 %q\n期望 %q", len(want), len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] 应为 %q，实际 %q（全部 %q）", i, want[i], got[i], got)
		}
	}
}

// hasSudoUserCall 判断有没有一次 `sudo -n -u <user>` 调用。
func hasSudoUserCall(calls [][]string, user string) bool {
	for _, c := range calls {
		if len(c) >= 3 && c[0] == sudoBin && c[1] == "-n" && c[2] == "-u" {
			for _, a := range c[3:] {
				if a == user {
					return true
				}
			}
		}
	}
	return false
}

package sysconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParsePmsetCurrent 用真机 `pmset -g` 的真实输出锁住解析规则。
//
// 这段输出有几个坑：值后面可能带括号注释（"sleep prevented by ..."）、
// 还有一大堆我们不关心的键（hibernatefile 等）、以及 "Currently in use:" 之前
// 可能还有别的段落（`pmset -g` 在插电/电池下的输出不同）。
func TestParsePmsetCurrent(t *testing.T) {
	real := `System-wide power settings:
Currently in use:
 standby              1
 Sleep On Power Button 1
 hibernatefile        /var/vm/sleepimage
 powernap             0
 networkoversleep     0
 disksleep            0
 sleep                0 (sleep prevented by powerd)
 hibernatemode        3
 displaysleep         10
 womp                 1
`
	got := parsePmsetCurrent(real)
	want := map[string]string{
		"sleep": "0", "disksleep": "0", "displaysleep": "10",
		"womp": "1", "powernap": "0",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s 应解析为 %q，实际 %q", k, v, got[k])
		}
	}
	// 不关心的键不该被收进来
	if _, ok := got["hibernatefile"]; ok {
		t.Error("不该收 hibernatefile 这类无关键")
	}
	// 没设置的键（如 autorestart）不该凭空出现
	if _, ok := got["autorestart"]; ok {
		t.Error("输出里没有 autorestart 就不该有值")
	}
}

// TestParsePmsetCurrentIgnoresBeforeCurrently 只看 "Currently in use:" 段后面。
func TestParsePmsetCurrentIgnoresBeforeCurrently(t *testing.T) {
	out := `Battery Power:
 sleep                5
Currently in use:
 sleep                0
`
	got := parsePmsetCurrent(out)
	if got["sleep"] != "0" {
		t.Errorf("应取 Currently in use 段的值 0，实际 %q", got["sleep"])
	}
}

// TestHostsBlockRoundTrip 锁住 hosts 阻断的幂等性：
// 加两次只能有一段，删掉后必须干净。
//
// 这条在真机上出过事故：我曾用 `sudo -S tee` 追加，stdin 被密码管道占掉，
// 结果把密码写进了 /etc/hosts。所以这里必须验证"加-加-删-删"四个动作后
// 文件内容逐字节回到原样。
func TestHostsBlockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	original := "127.0.0.1\tlocalhost\n::1\tlocalhost\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	old := hostsPath
	hostsPath = path
	t.Cleanup(func() { hostsPath = old })

	ctx := context.Background()
	var logs []string
	log := func(level, text string) { logs = append(logs, level+":"+text) }

	// 加一次
	if err := applyHostsBlock(ctx, &runner{log: log}); err != nil {
		t.Fatalf("第一次加阻断失败：%v", err)
	}
	if !hostsHasBlock() {
		t.Fatal("加完之后应能检测到阻断段")
	}
	body, _ := os.ReadFile(path)
	if n := strings.Count(string(body), hostsBegin); n != 1 {
		t.Fatalf("阻断段应恰好 1 段，实际 %d", n)
	}
	for _, h := range updateHosts {
		if !strings.Contains(string(body), "0.0.0.0 "+h) {
			t.Errorf("hosts 里缺少 %s 的阻断", h)
		}
	}

	// 再加一次：必须幂等（不能再写一段）
	if err := applyHostsBlock(ctx, &runner{log: log}); err != nil {
		t.Fatalf("第二次加阻断失败：%v", err)
	}
	body, _ = os.ReadFile(path)
	if n := strings.Count(string(body), hostsBegin); n != 1 {
		t.Fatalf("重复加应保持 1 段（幂等），实际 %d", n)
	}

	// 删一次
	if err := removeHostsBlock(ctx, &runner{log: log}); err != nil {
		t.Fatalf("移除阻断失败：%v", err)
	}
	body, _ = os.ReadFile(path)
	if strings.Contains(string(body), "0.0.0.0") {
		t.Errorf("移除后不该还有阻断条目：\n%s", body)
	}
	if strings.TrimSpace(string(body)) != strings.TrimSpace(original) {
		t.Errorf("移除后应回到原内容，\n得到：%q\n期望：%q", body, original)
	}

	// 再删一次：幂等
	if err := removeHostsBlock(ctx, &runner{log: log}); err != nil {
		t.Fatalf("重复移除应幂等，实际报错：%v", err)
	}
}

// TestHostsBlockIncompleteIsNotTouched：阻断段被人手改坏时，
// 宁可不改也不能把 /etc/hosts 写乱（那是登录/网络的关键文件）。
func TestHostsBlockIncompleteIsNotTouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	broken := "127.0.0.1 localhost\n" + hostsBegin + "\n0.0.0.0 swscan.apple.com\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	old := hostsPath
	hostsPath = path
	t.Cleanup(func() { hostsPath = old })

	if err := removeHostsBlock(context.Background(), &runner{}); err == nil {
		t.Fatal("找不到结束标记时应报错，而不是乱改文件")
	}
	body, _ := os.ReadFile(path)
	if string(body) != broken {
		t.Error("出错时不该改动文件内容")
	}
}

// TestProbeUpdatesJudgement 锁住"凭什么说已经阻断"的判断。
func TestProbeUpdatesJudgement(t *testing.T) {
	// 判断逻辑：安装类偏好全 0 且 hosts 有阻断段 → blocked。
	// 这里直接测判断用的谓词语义（Probe 会真的去读系统，不适合单测）。
	prefs := map[string]string{
		"AutomaticDownload":                "0",
		"AutomaticallyInstallMacOSUpdates": "0",
		"CriticalUpdateInstall":            "0",
		"ConfigDataInstall":                "0",
	}
	installOff := prefs["AutomaticDownload"] == "0" &&
		prefs["AutomaticallyInstallMacOSUpdates"] == "0" &&
		prefs["CriticalUpdateInstall"] == "0" &&
		prefs["ConfigDataInstall"] == "0"
	if !installOff {
		t.Fatal("四个键都是 0 时应判定为\"安装类已关\"")
	}
	prefs["CriticalUpdateInstall"] = "1"
	installOff = prefs["AutomaticDownload"] == "0" &&
		prefs["AutomaticallyInstallMacOSUpdates"] == "0" &&
		prefs["CriticalUpdateInstall"] == "0" &&
		prefs["ConfigDataInstall"] == "0"
	if installOff {
		t.Error("只要有一个安装类键为开，就不能说已阻断")
	}
}

// TestParsePmsetCaps 用**真机抓下来的原始字节**锁住能力探测。
//
// 这段输出来自 Mac mini M4（Mac16,10，2026-09-14 用 `pmset -g cap | od -c` 抓的）。
// 第一版实现拿整段做子串匹配（`strings.Contains(" "+out+" ", " "+key+" ")`），
// 因为输出是"每行前一个空格"的多行文本而不是空格分隔的一行，
// 除首尾两个键外**全都匹配不上** —— 页面于是把每一项都标成"本机不支持"。
// 这个 bug 单测没抓到（当时的桩件是单行输出），是升级到 mini 上真机验证才暴露的。
func TestParsePmsetCaps(t *testing.T) {
	real := "Capabilities for AC Power:\n displaysleep\n disksleep\n sleep\n womp\n" +
		" autorestart\n standby\n powernap\n ttyskeepawake\n tcpkeepalive\n lowpowermode\n "
	caps := parsePmsetCaps(real)

	for _, d := range powerDesired {
		if !caps[d.Key] {
			t.Errorf("真机输出里明明有 %q，却探测成不支持（会导致界面上无法修改这一项）", d.Key)
		}
	}
	// 表头不能被当成一个键
	if caps["Capabilities"] || caps["for"] || caps["AC"] || caps["Power:"] {
		t.Errorf("表头被当成了能力项：%v", caps)
	}
	// 不在输出里的键必须为 false
	if caps["hibernatemode"] {
		t.Error("输出里没有 hibernatemode，不该被当成支持")
	}
	// 空输出（命令失败）时不能声称支持任何键
	if len(parsePmsetCaps("")) != 0 {
		t.Error("空输出时应返回空集合")
	}
}

// TestLsofListenArgs 锁死 lsof 的参数形状。
//
// 曾经把 `-nP -iTCP:22 -sTCP:LISTEN` 拼成一个字符串作为单个 argv 传给 lsof，
// lsof 直接报错退出 → 端口探测永远返回 false。真机上的表现是：
// 用户正连着 SSH，"系统设置"页却说"未监听"。
func TestLsofListenArgs(t *testing.T) {
	args := lsofListenArgs(22)
	want := []string{"-nP", "-iTCP:22", "-sTCP:LISTEN"}
	if len(args) != len(want) {
		t.Fatalf("参数个数应为 %d，实际 %d：%q", len(want), len(args), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("第 %d 个参数应为 %q，实际 %q", i, want[i], args[i])
		}
		if strings.ContainsAny(args[i], " \t") {
			t.Errorf("参数 %q 里含空格 —— 它会被当成一个整体传给 lsof，导致命令失败", args[i])
		}
	}
}

// TestPowerDesiredMatchesServerModeScript 防止有人把建议值改得和
// tools/server-mode.sh 不一致（那个脚本是安装时用的，两边必须同口径）。
func TestPowerDesiredMatchesServerModeScript(t *testing.T) {
	script, err := os.ReadFile("../../tools/server-mode.sh")
	if err != nil {
		t.Skipf("读不到 server-mode.sh，跳过：%v", err)
	}
	text := string(script)
	for _, d := range powerDesired {
		needle := "pmset -a " + d.Key + " " + d.Value
		if !strings.Contains(text, needle) {
			t.Errorf("server-mode.sh 里没有 %q —— 两边的建议值必须一致（否则安装时和面板里会互相覆盖）", needle)
		}
	}
}

// TestProbeDoesNotPanicOnThisMachine 是条冒烟测试：探测会读 pmset/mdutil 等，
// 在真实机器上跑一遍确保不崩、且结构完整（不校验具体值，那取决于机器）。
func TestProbeDoesNotPanicOnThisMachine(t *testing.T) {
	st := Probe(context.Background())
	if len(st.Power) != len(powerDesired) {
		t.Errorf("电源项应有 %d 项，实际 %d", len(powerDesired), len(st.Power))
	}
	if st.SSHPort != 22 {
		t.Errorf("SSH 端口应是 22，实际 %d", st.SSHPort)
	}
	for _, it := range st.Power {
		if it.Label == "" || it.Key == "" {
			t.Errorf("电源项缺字段：%+v", it)
		}
		if it.Desired == "" {
			t.Errorf("%s 缺少建议值", it.Key)
		}
	}
	t.Logf("本机：model=%s 便携=%v 更新已阻断=%v(hosts=%v) Spotlight=%q SSH在听=%v 崩溃弹窗=%q 自动登录=%q",
		st.Model, st.Portable, st.Updates.Blocked, st.Updates.HostsBlocked, st.Spotlight, st.SSHListening, st.CrashDialog, st.AutoLogin)
}

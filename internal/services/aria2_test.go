package services

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  aria2 安装器门禁
//
//  这一类缺陷（用户的下载器今天报的）值得单独锁住：
//    · 配置写错一个字段 = 服务起来了但下不了东西（dir 不在、lpd 弹权限窗）；
//    · 密钥被重装换掉 = 用户浏览器里配好的 AriaNg 一直"未授权"；
//    · 卸载顺手删了 ~/Downloads = 删用户的文件（最严重）。
//  单测全用假 brew / 假 launchctl / 假 RPC 探针（绝不允许碰真实 launchd 与家目录）。
// ============================================================================

// aria2Harness 造一个沙箱化的 Manager + 假 brew + 注入的生命周期动作。
func aria2Harness(t *testing.T, installed bool) (*Manager, *InstallResult, Aria2Paths, *string, *string) {
	t.Helper()
	m, _ := sandboxIdempotentManager(t)
	home := t.TempDir()
	m.opt.UserHome = home
	// 安装器要求有真实用户（守护进程要以它身份跑，下载文件才属于用户）。
	// 用当前用户：chown 到自己不需要特权，临时目录上的归属操作不会失败。
	cur, err := user.Current()
	if err != nil {
		t.Skipf("拿不到当前用户：%v", err)
	}
	m.opt.UserName = cur.Username
	m.opt.BrewBin = fakeAria2Brew(t, installed)

	// plist 落在 <临时家目录>/Library/LaunchAgents 下：这样 RegisterInstalledService
	// 的纳管路径（AdoptCandidate 会按家目录找 plist）能真的走通 —— 生产环境是
	// /Library/LaunchDaemons，同一套逻辑。
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(plistDir, Aria2Label+".plist")
	oldPlist := aria2PlistPath
	aria2PlistPath = func() string { return plist }
	launched := ""
	stopped := ""
	oldLaunch, oldStop := aria2Launch, aria2Stop
	aria2Launch = func(_ *Manager, _ context.Context, label, p string) error {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("启动时 plist 还不存在：%w", err)
		}
		launched = label + "|" + p
		return nil
	}
	aria2Stop = func(_ *Manager, _ context.Context, label, p string) error {
		stopped = label + "|" + p
		return nil
	}
	oldTimeout := aria2ReadyTimeout
	aria2ReadyTimeout = 2 * time.Second
	t.Cleanup(func() {
		aria2PlistPath, aria2Launch, aria2Stop = oldPlist, oldLaunch, oldStop
		aria2ReadyTimeout = oldTimeout
	})
	// 写探针：默认成功（不碰真实文件系统之外的东西）。
	oldProbe := transmissionWriteProbe
	transmissionWriteProbe = func(_ *Manager, _ context.Context, p string) error {
		return os.WriteFile(p, nil, 0o600)
	}
	t.Cleanup(func() { transmissionWriteProbe = oldProbe })

	res := &InstallResult{App: Aria2AppID, Name: "aria2（下载器）", Steps: []string{}}
	return m, res, m.aria2Paths(), &launched, &stopped
}

// fakeAria2Brew 是只认 aria2 的假 brew（list --versions / uninstall 都记账）。
func fakeAria2Brew(t *testing.T, installed bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "brew")
	line := "exit 1"
	if installed {
		line = "printf '%s\\n' 'aria2 1.37.0'; exit 0"
	}
	script := "#!/bin/sh\nif [ \"$1\" = \"list\" ] && [ \"$2\" = \"--versions\" ]; then\n  " + line + "\nfi\nexit 0\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// aria2c 必须真的存在（安装器会 stat 它）。
	bin := filepath.Join(filepath.Dir(p), "aria2c")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAria2ConfMatchesUserRequirements 锁住两条：默认下载目录在应用安装根下（不占受保护的
// ~/Downloads，2026-10-06 用户要求）、不弹本地网络授权。
func TestAria2ConfMatchesUserRequirements(t *testing.T) {
	home := "/Users/someone"
	p := Aria2Paths{Home: home, Root: filepath.Join(home, "aria"),
		Conf:        Aria2ConfPath(home),
		Session:     filepath.Join(home, "aria", Aria2SessionName),
		DownloadDir: filepath.Join(home, "aria", "downloads"),
		OutLog:      filepath.Join(home, "Library", "Logs", "zizpanel-aria2.out.log"),
		ErrLog:      filepath.Join(home, "Library", "Logs", "zizpanel-aria2.err.log"),
	}
	conf := aria2Conf(p, "SECRET123")
	for _, want := range []string{
		"dir=" + filepath.Join(home, "aria", "downloads"), // 默认目录：应用安装根下，不受隐私保护
		"rpc-listen-all=true",                             // 用户 2026-09-23：局域网直连（靠 rpc-secret 保护）
		fmt.Sprintf("rpc-listen-port=%d", Aria2RPCPort),
		"rpc-secret=SECRET123",
		"enable-rpc=true",
		"rpc-listen-all=true", // 用户 2026-09-23：局域网直连（RPC 有 rpc-secret）
		"bt-enable-lpd=false", // 关 lpd：macOS 的"查找本地网络设备"弹窗就是它引起的
		"enable-dht=true",
		"async-dns=false", // 用系统解析器：c-ares 在 macOS 上会与系统解析器分叉（真机 DNS 报错）     // DHT 是公网单播，不弹窗，磁力链要靠它
		"save-session=" + p.Session,
		"input-file=" + p.Session,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("配置里缺少 %q\n实际：\n%s", want, conf)
		}
	}
	if !strings.Contains(conf, "rpc-secret=SECRET123") {
		t.Error("RPC 绑了局域网就必须有 rpc-secret，否则同网段任何人都能操纵下载器")
	}
	if !strings.Contains(conf, "async-dns=false") {
		t.Error("必须用系统解析器：aria2 默认的 c-ares 在 macOS 上会和系统解析器分叉，" +
			"真机表现是某些域名报 'DNS server returned answer with no data' 而 curl 却正常")
	}
}

// TestInstallAria2WritesConfPlistAndVerifiesRPC 主路径。
func TestInstallAria2WritesConfPlistAndVerifiesRPC(t *testing.T) {
	m, res, p, launched, _ := aria2Harness(t, true)
	var gotSecret string
	oldRPC := aria2RPCProbe
	aria2RPCProbe = func(_ context.Context, port int, secret string, _ time.Duration) (string, error) {
		if port != Aria2RPCPort {
			return "", fmt.Errorf("探测打到了端口 %d", port)
		}
		gotSecret = secret
		return "1.37.0", nil
	}
	t.Cleanup(func() { aria2RPCProbe = oldRPC })

	app, _ := FindApp(Aria2AppID)
	if err := m.InstallAria2(t.Context(), app, res); err != nil {
		t.Fatalf("安装应成功，实际：%v", err)
	}
	raw, err := os.ReadFile(p.Conf)
	if err != nil {
		t.Fatalf("配置文件没写出来：%v", err)
	}
	conf := string(raw)
	if !strings.Contains(conf, "rpc-secret=") || gotSecret == "" {
		t.Fatalf("密钥没写进配置或没参与复核：conf=%q secret=%q", conf, gotSecret)
	}
	if !strings.Contains(conf, "rpc-secret="+gotSecret) {
		t.Errorf("复核用的密钥与配置里的不一致：secret=%q\n%s", gotSecret, conf)
	}
	if st, err := os.Stat(p.Conf); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("配置文件权限应为 0600（里面有 RPC 密钥），实际 %v %v", st.Mode().Perm(), err)
	}
	if _, err := os.Stat(p.Session); err != nil {
		t.Errorf("会话文件必须先建出来（aria2 的 input-file 指它）：%v", err)
	}
	if !strings.Contains(*launched, Aria2Label) {
		t.Errorf("没有装载系统级服务：%q", *launched)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), p.DownloadDir) {
		t.Errorf("安装步骤里必须写明下载目录，实际：\n%s", strings.Join(res.Steps, "\n"))
	}
	found := false
	for _, c := range res.Credentials {
		if c.Key == "aria2_rpc_secret" && c.Value == gotSecret {
			found = true
		}
	}
	if !found {
		t.Errorf("RPC 密钥必须进凭据区（用户要在别的客户端里用它）：%+v", res.Credentials)
	}
	// 登记进服务管理（否则「已安装」看不到它）。
	if rec := m.installedRecordFor(t.Context(), app); rec == nil {
		t.Error("安装后必须有服务记录（否则市场显示未安装）")
	}
}

// TestInstallAria2ReusesExistingSecret 重装/升级不许换密钥（换了 AriaNg 就"未授权"）。
func TestInstallAria2ReusesExistingSecret(t *testing.T) {
	m, res, p, _, _ := aria2Harness(t, true)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Conf, []byte("dir=/tmp\nrpc-secret=OLD-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotSecret string
	oldRPC := aria2RPCProbe
	aria2RPCProbe = func(_ context.Context, _ int, secret string, _ time.Duration) (string, error) {
		gotSecret = secret
		return "1.37.0", nil
	}
	t.Cleanup(func() { aria2RPCProbe = oldRPC })

	app, _ := FindApp(Aria2AppID)
	if err := m.InstallAria2(t.Context(), app, res); err != nil {
		t.Fatalf("安装应成功，实际：%v", err)
	}
	if gotSecret != "OLD-SECRET" {
		t.Errorf("重装应复用原有密钥，实际用了 %q", gotSecret)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "复用原有 RPC 密钥") {
		t.Errorf("复用了密钥就要如实说明，实际步骤：\n%s", strings.Join(res.Steps, "\n"))
	}
}

// TestInstallAria2FailsWhenDownloadDirUnwritable 下载目录写不进去必须当场失败（坑 226 的形态）。
func TestInstallAria2FailsWhenDownloadDirUnwritable(t *testing.T) {
	m, res, p, _, _ := aria2Harness(t, true)
	oldProbe := transmissionWriteProbe
	transmissionWriteProbe = func(_ *Manager, _ context.Context, _ string) error {
		return fmt.Errorf("permission denied")
	}
	t.Cleanup(func() { transmissionWriteProbe = oldProbe })
	oldRPC := aria2RPCProbe
	aria2RPCProbe = func(context.Context, int, string, time.Duration) (string, error) {
		return "1.37.0", nil
	}
	t.Cleanup(func() { aria2RPCProbe = oldRPC })

	app, _ := FindApp(Aria2AppID)
	err := m.InstallAria2(t.Context(), app, res)
	if err == nil {
		t.Fatal("下载目录不可写时安装必须失败（不许报成功）")
	}
	if !strings.Contains(err.Error(), "不可写") || !strings.Contains(err.Error(), p.DownloadDir) {
		t.Errorf("失败原因必须点名目录不可写，实际：%v", err)
	}
}

// TestInstallAria2FailsWhenRPCNeverAnswers 端口在听不算装好：RPC 不回版本号必须失败。
func TestInstallAria2FailsWhenRPCNeverAnswers(t *testing.T) {
	m, res, p, _, _ := aria2Harness(t, true)
	oldRPC := aria2RPCProbe
	aria2RPCProbe = func(context.Context, int, string, time.Duration) (string, error) {
		return "", fmt.Errorf("connection refused")
	}
	oldInterval := aria2ReadyInterval
	aria2ReadyInterval = time.Millisecond
	t.Cleanup(func() { aria2RPCProbe, aria2ReadyInterval = oldRPC, oldInterval })
	// 日志文件先造出来，让失败信息能附上日志尾部（而不是"没有日志"）。
	if err := os.MkdirAll(filepath.Dir(p.ErrLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ErrLog, []byte("aria2c: error\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, _ := FindApp(Aria2AppID)
	err := m.InstallAria2(t.Context(), app, res)
	if err == nil {
		t.Fatal("RPC 不可用时安装必须失败（端口在听不代表能用）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "connection refused") && !strings.Contains(msg, "RPC") {
		t.Errorf("失败原因必须点明 RPC，实际：%v", err)
	}
}

// TestUninstallAria2NeverTouchesDownloads 卸载**绝不**删用户的下载文件。
func TestUninstallAria2NeverTouchesDownloads(t *testing.T) {
	m, res, p, _, stopped := aria2Harness(t, true)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Conf, []byte("rpc-secret=X\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.DownloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(p.DownloadDir, "用户的电影.mp4")
	if err := os.WriteFile(keep, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, _ := FindApp(Aria2AppID)
	if err := m.UninstallAria2(t.Context(), app, true, res); err != nil {
		t.Fatalf("卸载应成功，实际：%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("卸载动了下载目录里的文件（这是用户的数据）：%v", err)
	}
	if !strings.Contains(*stopped, Aria2Label) {
		t.Errorf("没有停服务/撤 plist：%q", *stopped)
	}
	if _, err := os.Stat(p.Conf); !os.IsNotExist(err) {
		t.Errorf("勾了「同时删除数据」时配置应被删掉，实际 err=%v", err)
	}

	// 计划里也必须写清楚：数据路径只含配置与会话，**不含**下载目录。
	plan := m.installerPlan(t.Context(), app)
	if plan.Kind != "installer" {
		t.Fatalf("aria2 的卸载计划应是 installer，实际 %q", plan.Kind)
	}
	for _, dp := range plan.DataPaths {
		if strings.HasPrefix(dp, p.DownloadDir) {
			t.Errorf("卸载计划把下载目录列进了可删数据：%s", dp)
		}
	}
	joined := strings.Join(plan.Steps, "\n") + plan.KeepNote
	if !strings.Contains(joined, "不碰下载目录") || !strings.Contains(joined, p.DownloadDir) {
		t.Errorf("卸载计划必须写明不碰下载目录，实际：\n%s", joined)
	}
}

// TestUpgradeBrewAppRestartsRunningService 锁住"升级了但跑的还是旧二进制"这一类
// （坑 216 ③ 同形）：brew upgrade 只换磁盘上的文件，长驻进程要用新版本必须重启。
//
// 三条都验：正在跑的 → 重启并如实说；用户自己停掉的 → **不复活**；重启失败 → 如实报错。
func TestUpgradeBrewAppRestartsRunningService(t *testing.T) {
	app, _ := FindApp(Aria2AppID)
	label := Aria2Label

	oldPID, oldRestart := brewUpgradeServicePID, brewUpgradeServiceRestart
	t.Cleanup(func() { brewUpgradeServicePID, brewUpgradeServiceRestart = oldPID, oldRestart })

	run := func(t *testing.T, pid int, restartErr error) (*InstallResult, bool, string) {
		t.Helper()
		m, _ := sandboxIdempotentManager(t)
		m.opt.BrewBin = fakeAria2Brew(t, true)
		called := false
		brewUpgradeServicePID = func(l string) (int, error) {
			if l != label {
				return 0, nil
			}
			return pid, nil
		}
		brewUpgradeServiceRestart = func(l string) error {
			if l != label {
				return nil
			}
			called = true
			return restartErr
		}
		res := &InstallResult{App: app.ID, Name: app.Name, Steps: []string{}}
		if err := m.UpgradeBrewApp(t.Context(), app, res); err != nil {
			t.Fatalf("升级应成功，实际：%v", err)
		}
		return res, called, strings.Join(res.Steps, "\n")
	}

	res, called, steps := run(t, 4321, nil)
	if !called {
		t.Error("服务正在跑时必须重启它，否则用户仍在使用旧版本")
	}
	if !strings.Contains(steps, "已重启") {
		t.Errorf("重启了就要如实说，实际步骤：\n%s", steps)
	}
	_ = res

	if _, called, _ := run(t, 0, nil); called {
		t.Error("用户自己停掉的服务不许被升级复活（我们在替他做主）")
	}

	_, called, steps = run(t, 4321, fmt.Errorf("kickstart 失败"))
	if !called {
		t.Fatal("前置条件：应当尝试重启")
	}
	if !strings.Contains(steps, "重启") || !strings.Contains(steps, "失败") {
		t.Errorf("重启失败必须如实报告并给出出路，实际步骤：\n%s", steps)
	}
}

// TestAria2ReadyRemedyNamesMacPrivacyTrap 锁住 2026-09-23 真机那次排查最久的一条：
// aria2 起来但 RPC 不回，绝大多数时候**不是**"端口被占/配置坏"，而是 macOS 隐私保护
// 挡住了后台服务访问 ~/Downloads（挂在 open() 上，界面永远"连接中…"）。
// 旧文案只写前者，我们照着它查了一上午 —— 所以这里把两种文案都钉死。
func TestAria2ReadyRemedyNamesMacPrivacyTrap(t *testing.T) {
	home := "/Users/zizdog"
	got := aria2ReadyRemedy(home+"/Downloads", home)
	if !strings.Contains(got, "完全磁盘访问权限") {
		t.Errorf("下载目录被隐私保护时必须给出授权出路，实际：%s", got)
	}
	if !strings.Contains(got, "/opt/homebrew/bin/aria2c") {
		t.Errorf("要写明给哪个二进制授权（否则用户不知道该加谁），实际：%s", got)
	}
	if !strings.Contains(got, home+"/Downloads") {
		t.Errorf("要写清当前下载目录，实际：%s", got)
	}
	// 子目录同样算（~/Downloads/aria2 也在保护范围内）
	if g := aria2ReadyRemedy(home+"/Downloads/sub", home); !strings.Contains(g, "完全磁盘访问权限") {
		t.Errorf("保护目录的子目录也应给出授权出路，实际：%s", g)
	}
	// 非保护目录：给原来的出路（不能把"卡死"当成万能解释）
	plain := aria2ReadyRemedy(home+"/aria/downloads", home)
	if strings.Contains(plain, "完全磁盘访问权限") {
		t.Errorf("不受保护的目录不该提隐私授权，实际：%s", plain)
	}
	if !strings.Contains(plain, "重启服务") {
		t.Errorf("兜底文案要给出下一步，实际：%s", plain)
	}
	// 负向对照：别的用户的 /data/Downloads、空值都不许误报
	for _, bad := range []string{"/data/Downloads", "", ".", "/Users/zizdog/Down"} {
		if strings.Contains(aria2ReadyRemedy(bad, home), "完全磁盘访问权限") {
			t.Errorf("%q 不该被判成 macOS 保护目录", bad)
		}
	}
}

// TestAria2SuperviseArgsPointAtPanelBinary 锁住 2026-09-23 的结构性决定：
// aria2 由**面板自己的二进制**托管（`<面板> aria2-supervise …`），而不是裸跑
// /opt/homebrew/bin/aria2c。
//
// 为什么必须这样（不是风格问题）：panel 二进制 fork 后 setuid 到真实用户，下载文件
// 归属用户；且与面板同一代码要求 ⇒ 用户把 dir= 指到受保护目录/外接盘时继承面板的
// 「完全磁盘访问权限」。裸跑 aria2c 的 adhoc 身份每次 brew 升级都会变，给它的授权随即
// 失效（真机表现：端口在听、界面永远"连接中…"）。
func TestAria2SuperviseArgsPointAtPanelBinary(t *testing.T) {
	home := t.TempDir()
	// BrewBin 必须给：aria2c 的路径由它推导，拿不到就该在安装时拒绝写 plist。
	m := NewManager(nil, Options{UserName: "zizdog", UserHome: home,
		WorkDir: filepath.Join(home, "work"), BrewBin: filepath.Join(home, "bin", "brew")})
	old := aria2Executable
	aria2Executable = func() (string, error) { return "/opt/zizpanel/bin/zizpanel", nil }
	t.Cleanup(func() { aria2Executable = old })

	p := m.aria2Paths()
	args := Aria2SuperviseArgs("/opt/zizpanel/bin/zizpanel", "zizdog", p)

	if args[0] != "/opt/zizpanel/bin/zizpanel" {
		t.Fatalf("ProgramArguments[0] 必须是面板自身的二进制，实际 %q", args[0])
	}
	if args[1] != "aria2-supervise" {
		t.Fatalf("ProgramArguments[1] 必须是 aria2-supervise，实际 %q", args[1])
	}
	for _, kv := range []struct{ flag, want string }{
		{"--user", "zizdog"},
		{"--bin", p.Bin},
		{"--conf", p.Conf},
		{"--root", p.Root},
		{"--home", p.Home},
	} {
		i := indexOfToken(args, kv.flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != kv.want {
			t.Errorf("%s 后面应是 %q，实际 %v", kv.flag, kv.want, args)
		}
	}
	// 每个路径参数都必须是绝对路径：supervisor 会拒绝相对路径，写在 plist 里等
	// launchd 起来才报错就太晚了。
	for _, a := range args {
		if strings.HasPrefix(a, "--") || a == "aria2-supervise" || a == "/opt/zizpanel/bin/zizpanel" || a == "zizdog" {
			continue
		}
		if !filepath.IsAbs(a) {
			t.Errorf("参数 %q 不是绝对路径（launchd 的工作目录不是用户家目录）", a)
		}
	}
	// 默认下载目录在应用安装根下（2026-10-06 用户要求：不再用受保护的 ~/Downloads）。
	if want := filepath.Join(home, "aria", "downloads"); p.DownloadDir != want {
		t.Errorf("下载目录默认值应为 %s，实际 %q", want, p.DownloadDir)
	}
}

// TestAria2PlistRunsPanelSupervisorNotBareAria2c 锁住 plist 形态：
//
//	· ProgramArguments[0] 是面板二进制、[1] 是 aria2-supervise；
//	· **不写 UserName**（作业以 root 跑，由 supervisor fork 后降权到真实用户）；
//	· 仍然带着 aria2c 的绝对路径（supervisor 拿它去 exec）；
//	· label 与面板自己的 label 不同 —— aria2 是**独立守护进程**，面板重启/升级
//	  不会顺带杀掉它（下载不中断）。
func TestAria2PlistRunsPanelSupervisorNotBareAria2c(t *testing.T) {
	plist := aria2Plist("/opt/zizpanel/bin/zizpanel", "zizdog", "/Users/zizdog/aria",
		"/opt/homebrew/bin/aria2c", "/Users/zizdog/aria/aria2.conf",
		"/tmp/o.log", "/tmp/e.log")
	for _, want := range []string{
		"<string>/opt/zizpanel/bin/zizpanel</string>",
		"<string>aria2-supervise</string>",
		"<string>--user</string>",
		"<string>zizdog</string>",
		"<string>/opt/homebrew/bin/aria2c</string>",
		"<string>/Users/zizdog/aria/aria2.conf</string>",
		"<string>/Users/zizdog/aria</string>",
		"<string>/Users/zizdog</string>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist 里缺少 %q：\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "UserName") {
		t.Errorf("这个作业必须由 root 跑（supervisor 要 fork 后 setuid 降权），不能写 UserName：\n%s", plist)
	}
	if strings.Contains(plist, "--conf-path=") {
		t.Errorf("不该在 plist 里直接给 aria2c 传 --conf-path（那是 supervisor 的活）：\n%s", plist)
	}
	// label 必须与面板自己的守护进程（cn.zizpanel.panel，见 install.sh 的 PANEL_LABEL）
	// 不同：它们是**兄弟**，面板重启/升级不会顺带杀掉 aria2，下载不中断。
	if Aria2Label == "cn.zizpanel.panel" {
		t.Errorf("aria2 必须是与面板不同的独立守护进程（label %q 撞了）", Aria2Label)
	}
}

// TestInstallAria2WritesSupervisePlist 端到端锁住"写出来的是托管 plist"：
// 走一遍 InstallAria2（brew/launchd/RPC 全注入假实现），把真写出来的 plist 读回来断言。
// 只测 aria2Plist 不够 —— 参数是从安装器那条路上拼出来的。
func TestInstallAria2WritesSupervisePlist(t *testing.T) {
	m, _, p, _, _ := aria2Harness(t, true)
	old := aria2Executable
	aria2Executable = func() (string, error) { return "/opt/zizpanel/bin/zizpanel", nil }
	t.Cleanup(func() { aria2Executable = old })
	oldProbe := aria2RPCProbe
	aria2RPCProbe = func(context.Context, int, string, time.Duration) (string, error) { return "1.37.0", nil }
	t.Cleanup(func() { aria2RPCProbe = oldProbe })

	app, ok := FindApp(Aria2AppID)
	if !ok {
		t.Fatal("目录里没有 aria2")
	}
	if err := m.InstallAria2(context.Background(), app, &InstallResult{App: Aria2AppID, Steps: []string{}}); err != nil {
		t.Fatalf("安装应当成功：%v", err)
	}
	raw, err := os.ReadFile(p.Plist)
	if err != nil {
		t.Fatalf("plist 没写出来：%v", err)
	}
	plist := string(raw)
	for _, want := range []string{
		"<string>/opt/zizpanel/bin/zizpanel</string>",
		"<string>aria2-supervise</string>",
		"<string>--bin</string>",
		"<string>" + p.Bin + "</string>",
		"<string>--conf</string>",
		"<string>" + p.Conf + "</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("装完的 plist 里缺少 %q：\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "UserName") {
		t.Errorf("supervisor 作业必须由 root 跑（fork 后降权），不该有 UserName：\n%s", plist)
	}
}

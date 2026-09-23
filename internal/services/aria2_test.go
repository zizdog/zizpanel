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

// TestAria2ConfMatchesUserRequirements 锁住用户点名的两条：下载目录 ~/Downloads、不弹本地网络授权。
func TestAria2ConfMatchesUserRequirements(t *testing.T) {
	home := "/Users/someone"
	p := Aria2Paths{Home: home, Root: filepath.Join(home, "aria"),
		Conf:        Aria2ConfPath(home),
		Session:     filepath.Join(home, "aria", Aria2SessionName),
		DownloadDir: filepath.Join(home, "Downloads"),
		OutLog:      filepath.Join(home, "Library", "Logs", "zizpanel-aria2.out.log"),
		ErrLog:      filepath.Join(home, "Library", "Logs", "zizpanel-aria2.err.log"),
	}
	conf := aria2Conf(p, "SECRET123")
	for _, want := range []string{
		"dir=" + filepath.Join(home, "Downloads"), // 用户要的默认下载目录
		"rpc-listen-all=true",                     // 用户 2026-09-23：局域网直连（靠 rpc-secret 保护）
		fmt.Sprintf("rpc-listen-port=%d", Aria2RPCPort),
		"rpc-secret=SECRET123",
		"enable-rpc=true",
		"rpc-listen-all=true", // 用户 2026-09-23：局域网直连（RPC 有 rpc-secret）
		"bt-enable-lpd=false", // 关 lpd：macOS 的"查找本地网络设备"弹窗就是它引起的
		"enable-dht=true",     // DHT 是公网单播，不弹窗，磁力链要靠它
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

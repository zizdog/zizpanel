package services

// macsaber_test.go —— mac军刀（MacSaber）的门禁测试。
//
// 纪律（AGENTS 第三节）：
//   - 单测**绝不**联网（下载由 macSaberFetch 注入伪造，HTTP 探针默认返回"连接被拒"）；
//   - 单测**绝不**碰真实 launchd（macSaberLaunch/Stop/LegacyStop 注入记录）；
//   - 单测**绝不**写 /opt/macsaber、/Library/LaunchDaemons 或真实家目录
//     （MacSaberInstallRoot 与 SystemLaunchDaemonsDir 都指向 t.TempDir()，
//     Manager 的 UserHome 是沙箱临时目录）。
//
// 判据都是**行为**而不是字符串：installed 必须在"二进制缺失 / version 跑不起来 /
// 端口不健康 / 版本对不上"时为 false；安装必须真的清理旧用户级 agent；卸载必须
// 真的删掉该删的、保留该保留的，并复核终态。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// macSaberTestEnv 造一个隔离的安装环境：临时安装根、临时家目录、假 launchd。
type macSaberTestEnv struct {
	m          *Manager
	root       string
	plist      string
	legacy     string
	panel      string
	launch     []string
	stop       []string
	legacyStop []string
	// events 是按时间顺序记下的动作（用于锁"先迁移、后 bootstrap"的顺序）。
	events []string
}

// newMacSaberTestEnv 建立沙箱并替换全部注入点（t.Cleanup 恢复）。
func newMacSaberTestEnv(t *testing.T) *macSaberTestEnv {
	t.Helper()
	m, _ := sandboxManager(t)
	root := t.TempDir()
	env := &macSaberTestEnv{m: m, root: root}

	prevRoot := MacSaberInstallRoot
	prevLaunchdDir := SystemLaunchDaemonsDir
	prevLaunch, prevStop, prevLegacy := macSaberLaunch, macSaberStop, macSaberLegacyStop
	prevVerify, prevExtract := macSaberVerifyArm64, macSaberExtract
	prevVer, prevGet := macSaberVersionFn, macSaberHTTPGet
	prevExec := macSaberExecutable
	prevRemovedWait := macSaberRemovedWait
	t.Cleanup(func() {
		MacSaberInstallRoot = prevRoot
		SystemLaunchDaemonsDir = prevLaunchdDir
		macSaberLaunch, macSaberStop, macSaberLegacyStop = prevLaunch, prevStop, prevLegacy
		macSaberVerifyArm64, macSaberExtract = prevVerify, prevExtract
		macSaberVersionFn, macSaberHTTPGet = prevVer, prevGet
		macSaberExecutable = prevExec
		macSaberRemovedWait = prevRemovedWait
	})

	MacSaberInstallRoot = root
	SystemLaunchDaemonsDir = t.TempDir()
	// 卸载复核的等待缩到毫秒级（真实实现是 5 秒）。
	macSaberRemovedWait = 400 * time.Millisecond
	env.panel = filepath.Join(t.TempDir(), "zizpanel")
	if err := os.WriteFile(env.panel, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	macSaberExecutable = func() (string, error) { return env.panel, nil }

	macSaberLaunch = func(m *Manager, _ context.Context, label, plist string) error {
		env.launch = append(env.launch, label+"|"+plist)
		env.events = append(env.events, "launch:"+label)
		// 模拟 launchd：AdoptCandidate 只认 /Library/LaunchDaemons 与用户级目录，
		// 单测不能写真实系统目录，所以把系统 plist 复制一份到用户级位置让它找得到
		// （与 imgcompress 的沙箱做法一致）。
		b, err := os.ReadFile(plist)
		if err != nil {
			return err
		}
		dst := filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	}
	macSaberStop = func(m *Manager, _ context.Context, label, plist string) error {
		env.stop = append(env.stop, label+"|"+plist)
		env.events = append(env.events, "stop:"+label)
		return removeAllPlists([]string{plist, filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")})
	}
	macSaberLegacyStop = func(_ *Manager, _ context.Context, plist string) error {
		env.legacyStop = append(env.legacyStop, plist)
		env.events = append(env.events, "legacy:"+MacSaberLegacyLabel)
		if plist != "" {
			if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	// 单测里的"二进制"是一个 shell 脚本，不可能真是 Mach-O arm64；
	// 架构复核另有专门的用例（见 TestMacSaberArm64CheckIsNotVacuous）。
	macSaberVerifyArm64 = func(*Manager, context.Context, string) error { return nil }
	// 假 runtime：不做 version 复核（那是另一个注入点）。
	macSaberVersionFn = func(_ *Manager, _ context.Context, bin string) (string, error) {
		return "mac军刀 macsaber " + MacSaberVersion, nil
	}
	// 默认 HTTP 探针 = "没人在听"：卸载复核与就绪等待都不许真的联网。
	macSaberHTTPGet = func(context.Context, string) (string, int, error) {
		return "", 0, errString("Connection refused")
	}
	p := m.MacSaberPathsFor()
	env.plist = p.Plist
	env.legacy = p.LegacyPlist
	return env
}

// stubHTTPGet 把 HTTP 探测替换成固定响应。
func (e *macSaberTestEnv) stubHTTPGet(body string, code int, err error) {
	macSaberHTTPGet = func(context.Context, string) (string, int, error) { return body, code, err }
}

// removeAllPlists 删除一组 plist（不存在算成功）。
func removeAllPlists(paths []string) error {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// writeFakeBin 在安装根写一个可执行的假二进制。
func (e *macSaberTestEnv) writeFakeBin(t *testing.T, script string) string {
	t.Helper()
	bin := MacSaberBin()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// writeLegacyAgent 造出"机器上还留着旧用户级 agent"的夹具。
func (e *macSaberTestEnv) writeLegacyAgent(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.legacy, []byte("<plist>旧用户级 agent</plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
//  installed 判据：必须是运行体证据，不许是"文件存在"
// ---------------------------------------------------------------------------

// TestMacSaberInstalledIsFalseWithoutBinary 二进制缺失时必须判未安装。
func TestMacSaberInstalledIsFalseWithoutBinary(t *testing.T) {
	env := newMacSaberTestEnv(t)
	// 连 plist 都造出来（模拟"僵尸 plist"）：判据不许因此说"已安装"。
	if err := os.MkdirAll(filepath.Dir(env.plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.plist, []byte("僵尸"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 端口服务"看起来"健康（哪怕真有服务在该端口上）：二进制不在就不是它装的。
	env.stubHTTPGet(`{"ok":true,"version":"`+MacSaberVersion+`"}`, 200, nil)

	ok, detail := env.m.MacSaberServeCheck(context.Background())
	if ok {
		t.Fatalf("二进制缺失时 installed 必须为 false，实际 true（%s）", detail)
	}
	if !strings.Contains(detail, MacSaberBin()) {
		t.Errorf("结论要点名缺的是哪个文件，实际：%s", detail)
	}
}

// TestMacSaberInstalledIsFalseWhenVersionCommandFails 二进制在但跑不起来时判未安装。
func TestMacSaberInstalledIsFalseWhenVersionCommandFails(t *testing.T) {
	env := newMacSaberTestEnv(t)
	env.writeFakeBin(t, "#!/bin/sh\nexit 3\n")
	env.stubHTTPGet(`{"ok":true,"version":"`+MacSaberVersion+`"}`, 200, nil)
	prev := macSaberVersionFn
	macSaberVersionFn = func(*Manager, context.Context, string) (string, error) {
		return "", errString("exec format error")
	}
	t.Cleanup(func() { macSaberVersionFn = prev })

	ok, detail := env.m.MacSaberServeCheck(context.Background())
	if ok {
		t.Fatalf("`macsaber version` 失败时 installed 必须为 false，实际 true（%s）", detail)
	}
	if !strings.Contains(detail, "exec format error") {
		t.Errorf("结论要带真实原因，实际：%s", detail)
	}
}

// TestMacSaberInstalledFollowsHTTPHealth 端口不健康 / 版本对不上时必须判未安装。
//
// 这是"端口在听就算装好"的反例集合：进程没起来、别人占着端口、装的是别的版本。
func TestMacSaberInstalledFollowsHTTPHealth(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
		err  error
		want bool
	}{
		{"服务没起来", "", 0, errString("Connection refused"), false},
		{"HTTP 500", "boom", 500, nil, false},
		{"不是 JSON", "<html>nginx 502</html>", 200, nil, false},
		{"版本对不上", `{"ok":true,"version":"9.9.9"}`, 200, nil, false},
		{"版本对得上", `{"ok":true,"version":"` + MacSaberVersion + `"}`, 200, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMacSaberTestEnv(t)
			env.writeFakeBin(t, "#!/bin/sh\necho mac军刀 macsaber "+MacSaberVersion+"\n")
			env.stubHTTPGet(tc.body, tc.code, tc.err)
			ok, detail := env.m.MacSaberServeCheck(context.Background())
			if ok != tc.want {
				t.Fatalf("installed=%v，期望 %v（结论：%s）", ok, tc.want, detail)
			}
		})
	}
}

// TestMacSaberHealthProbeUsesRealEndpoint 探针必须打真实的 /api/version
// （macsaber/internal/web/server.go 里那个**不需要登录**的公开端点）。
func TestMacSaberHealthProbeUsesRealEndpoint(t *testing.T) {
	env := newMacSaberTestEnv(t)
	env.writeFakeBin(t, "#!/bin/sh\necho x\n")
	var got string
	macSaberHTTPGet = func(_ context.Context, url string) (string, int, error) {
		got = url
		return `{"ok":true,"version":"` + MacSaberVersion + `"}`, 200, nil
	}
	if _, _ = env.m.MacSaberServeCheck(context.Background()); !strings.HasSuffix(got, "/api/version") {
		t.Fatalf("健康探针必须打 /api/version，实际 %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("127.0.0.1:%d", MacSaberPort)) {
		t.Errorf("探针只许打回环地址，实际 %q", got)
	}
}

// TestMacSaberServeArgsUseRealUserPaths 子进程参数必须来自真实用户的路径。
//
// 这是本设计的核心之一：读根就是真实用户家目录（权限来自面板的 TCC 授权，
// 不再靠"避开受保护目录"），写根是 ~/MacSaberFiles，数据目录在该用户下。
func TestMacSaberServeArgsUseRealUserPaths(t *testing.T) {
	env := newMacSaberTestEnv(t)
	p := env.m.MacSaberPathsFor()
	listen := fmt.Sprintf("127.0.0.1:%d", MacSaberPort)
	args := MacSaberServeArgs(p.DataDir, listen, p.ReadRoot, p.WriteRoot)
	joined := strings.Join(args, " ")

	if p.ReadRoot != env.m.opt.UserHome {
		t.Errorf("读根必须是真实用户家目录 %s，实际 %s", env.m.opt.UserHome, p.ReadRoot)
	}
	if p.WriteRoot != filepath.Join(env.m.opt.UserHome, "MacSaberFiles") {
		t.Errorf("写根必须是 ~/MacSaberFiles，实际 %s", p.WriteRoot)
	}
	for _, want := range []string{"serve", "--data", p.DataDir, "--listen", listen, "--read-root", p.ReadRoot, "--write-root", p.WriteRoot} {
		if !strings.Contains(joined, want) {
			t.Errorf("serve 参数缺少 %q：%s", want, joined)
		}
	}
	if strings.Contains(joined, "0.0.0.0") {
		t.Errorf("serve 参数里出现了 0.0.0.0（会把本机文件工具暴露到局域网）：%s", joined)
	}
	if strings.Contains(joined, "/var/root") {
		t.Errorf("参数里出现了 /var/root（root 的家目录）：%s", joined)
	}
	// 参数顺序必须与 macsaber/main.go 的 serve 子命令一致（--data/--listen/--read-root/--write-root）。
	wantOrder := []string{"serve", "--data", "--listen", "--read-root", "--write-root"}
	last := -1
	for _, flag := range wantOrder {
		idx := indexOfToken(args, flag)
		if idx < 0 {
			t.Fatalf("serve 参数缺少 %q：%v", flag, args)
		}
		if idx <= last {
			t.Errorf("参数顺序不对：%q 出现在 %q 之前：%v", flag, wantOrder, args)
		}
		last = idx
	}
}

// indexOfToken 返回 token 在 args 里的下标（找不到 -1）。
func indexOfToken(args []string, token string) int {
	for i, a := range args {
		if a == token {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
//  plist 生成物：系统级 LaunchDaemon
// ---------------------------------------------------------------------------

// TestMacSaberPlistIsSystemDaemonAndArgsAreExact 逐项锁住系统 plist：
// label / 可执行文件是面板自己 / 参数顺序 / 日志，且**不含 UserName**。
func TestMacSaberPlistIsSystemDaemonAndArgsAreExact(t *testing.T) {
	env := newMacSaberTestEnv(t)
	p := env.m.MacSaberPathsFor()
	args := MacSaberSuperviseArgs(env.panel, env.m.opt.UserName, p)
	content := macSaberPlistContent(MacSaberLabel, args, p.SuperviseOutLog, p.SuperviseErrLog)

	// label 与"可执行文件 = 面板自己的二进制 + 子命令"，参数顺序固定。
	wantOrder := []string{
		"<string>" + MacSaberLabel + "</string>",
		"<string>" + env.panel + "</string>",
		"<string>macsaber-supervise</string>",
		"<string>--user</string>",
		"<string>" + env.m.opt.UserName + "</string>",
		"<string>--macsaber</string>",
		"<string>" + p.Bin + "</string>",
		"<string>--data</string>",
		"<string>" + p.DataDir + "</string>",
		"<string>--listen</string>",
		"<string>" + fmt.Sprintf("127.0.0.1:%d", MacSaberPort) + "</string>",
		"<string>--read-root</string>",
		"<string>" + env.m.opt.UserHome + "</string>",
		"<string>--write-root</string>",
		"<string>" + p.WriteRoot + "</string>",
		"<string>--log-dir</string>",
		"<string>" + p.LogDir + "</string>",
	}
	last := -1
	for _, want := range wantOrder {
		idx := strings.Index(content, want)
		if idx < 0 {
			t.Errorf("plist 缺少 %q：\n%s", want, content)
			continue
		}
		if idx <= last {
			t.Errorf("plist 里 %q 的位置不对（顺序与 MacSaberSuperviseArgs 不一致）：\n%s", want, content)
		}
		last = idx
	}
	if strings.Contains(content, "<key>UserName</key>") {
		// supervisor 必须以 root 运行才能 fork 后 setuid 降权；写 UserName
		// 会让 launchd 直接以普通用户启动它，降权那一步必然失败。
		t.Errorf("系统守护进程不该写 UserName（supervisor 需要 root 才能降权）：\n%s", content)
	}
	for _, want := range []string{
		"<key>RunAtLoad</key>\n    <true/>",
		"<key>KeepAlive</key>\n    <true/>",
		"<key>StandardOutPath</key>\n    <string>" + p.SuperviseOutLog + "</string>",
		"<key>StandardErrorPath</key>\n    <string>" + p.SuperviseErrLog + "</string>",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("plist 缺少 %q：\n%s", want, content)
		}
	}
	if strings.Contains(content, MacSaberLegacyLabel) {
		t.Errorf("新系统 plist 里不该出现旧 label %s：\n%s", MacSaberLegacyLabel, content)
	}
	// 位置：系统 plist 在 /Library/LaunchDaemons（测试里被替换为临时目录）；
	// 旧用户级 agent 仍在 ~/Library/LaunchAgents。
	if filepath.Dir(p.Plist) != SystemLaunchDaemonsDir {
		t.Errorf("系统 plist 必须在 %s 下，实际 %s", SystemLaunchDaemonsDir, p.Plist)
	}
	if !strings.Contains(p.LegacyPlist, filepath.Join("Library", "LaunchAgents")) {
		t.Errorf("旧 plist 路径必须在 ~/Library/LaunchAgents 下，实际 %s", p.LegacyPlist)
	}
	if !strings.Contains(p.OutLog, filepath.Join("Library", "Logs")) {
		t.Errorf("子进程日志必须在 ~/Library/Logs 下，实际 %s", p.OutLog)
	}
	// XML 必须能被 plutil 解析（结构错了 launchd 直接拒绝，且不给行号）。
	if _, err := os.Stat("/usr/bin/plutil"); err == nil {
		tmp := filepath.Join(t.TempDir(), MacSaberLabel+".plist")
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := runOutputErr("/usr/bin/plutil", "-lint", tmp); err != nil {
			t.Errorf("plutil 拒绝这份 plist：%v（%s）", err, out)
		}
	}
}

// runOutputErr 跑一个命令并把输出与错误一起带回（测试用，只用于 plutil 校验）。
func runOutputErr(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// TestMacSaberLabelAndPlistPathAreSystemDomain 锁住 label 与 plist 的**系统域**归属。
func TestMacSaberLabelAndPlistPathAreSystemDomain(t *testing.T) {
	prev := SystemLaunchDaemonsDir
	SystemLaunchDaemonsDir = "/Library/LaunchDaemons"
	t.Cleanup(func() { SystemLaunchDaemonsDir = prev })

	if got := SystemDaemonPlistPath(MacSaberLabel); got != "/Library/LaunchDaemons/cn.zizpanel.macsaber.plist" {
		t.Errorf("系统 plist 路径不对：%s", got)
	}
	if MacSaberLabel != "cn.zizpanel.macsaber" {
		t.Errorf("launchd label 应是 cn.zizpanel.macsaber（面板前缀），实际 %s", MacSaberLabel)
	}
	if MacSaberLegacyLabel != "cn.macsaber.web" {
		t.Errorf("旧 label 常量必须是 cn.macsaber.web（迁移要按它停旧作业），实际 %s", MacSaberLegacyLabel)
	}
	if MacSaberLabel == MacSaberLegacyLabel {
		t.Error("新旧 label 不能相同（否则迁移会把新守护进程也停掉）")
	}
}

// TestMacSaberSuperviseArgsArePanelBinaryAndRealUserPaths 锁住"系统守护进程执行的
// 是面板自己的二进制"这条关键判据：它保证 supervisor 与面板同一代码要求 ⇒ 共用 TCC 授权。
func TestMacSaberSuperviseArgsArePanelBinaryAndRealUserPaths(t *testing.T) {
	env := newMacSaberTestEnv(t)
	p := env.m.MacSaberPathsFor()
	args := MacSaberSuperviseArgs(env.panel, env.m.opt.UserName, p)
	if args[0] != env.panel {
		t.Fatalf("ProgramArguments[0] 必须是面板自身的二进制（%s），实际 %q", env.panel, args[0])
	}
	if args[1] != "macsaber-supervise" {
		t.Fatalf("ProgramArguments[1] 必须是 macsaber-supervise，实际 %q", args[1])
	}
	// 必须点名某个真实用户（supervisor 据此 setuid），且不能是 root。
	if indexOfToken(args, "--user") < 0 {
		t.Fatal("必须显式传 --user（supervisor 据此降权）")
	}
	for _, a := range args {
		if a == "root" || a == "0" {
			t.Errorf("参数里出现了 root（supervisor 只做降权，不许以 root 跑 macsaber）：%v", args)
		}
	}
}

// ---------------------------------------------------------------------------
//  安装：迁移旧 agent + 幂等 + 如实失败
// ---------------------------------------------------------------------------

// macSaberTestPayload 是一份"归档字节"（内容随意，全部下载/解包都被注入替换）。
var macSaberTestPayload = []byte("macsaber-test-archive")

// stageFakeInstall 把下载与解包都换成本地伪造：往 staging 目录写一个假二进制。
func stageFakeInstall(t *testing.T, env *macSaberTestEnv) {
	t.Helper()
	prevFetch, prevSHA := macSaberFetch, MacSaberArchiveSHA256
	t.Cleanup(func() { macSaberFetch, MacSaberArchiveSHA256 = prevFetch, prevSHA })

	sum := sha256.Sum256(macSaberTestPayload)
	MacSaberArchiveSHA256 = hex.EncodeToString(sum[:])
	macSaberFetch = func(_ context.Context, url, dest string, _ func(got, total int64)) error {
		if !strings.Contains(url, "apps/macsaber/"+MacSaberVersion+"/") {
			t.Errorf("下载地址不是镜像站的 apps/macsaber/<ver>/：%s", url)
		}
		return os.WriteFile(dest, macSaberTestPayload, 0o644)
	}
	macSaberExtract = func(_ *Manager, _ context.Context, archive, dest string) error {
		return os.WriteFile(filepath.Join(dest, "macsaber"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
}

// newMacSaberInstallManager 造一个带镜像基址的沙箱 Manager（下载地址才拼得出来）。
func newMacSaberInstallManager(t *testing.T) *macSaberTestEnv {
	t.Helper()
	env := newMacSaberTestEnv(t)
	env.m.opt.MirrorBase = "https://mirror.example.test"
	return env
}

// TestInstallMacSaberMigratesLegacyAgent 夹具里放一个旧用户级 agent：
// 安装后必须旧 plist 被删、旧域作业被 bootout、只留新的系统守护进程。
func TestInstallMacSaberMigratesLegacyAgent(t *testing.T) {
	env := newMacSaberInstallManager(t)
	stageFakeInstall(t, env)
	env.stubHTTPGet(`{"ok":true,"version":"`+MacSaberVersion+`"}`, 200, nil)
	env.writeLegacyAgent(t)

	app, _ := FindApp(MacSaberAppID)
	res := &InstallResult{Steps: []string{}}
	if err := env.m.InstallMacSaber(context.Background(), app, res); err != nil {
		t.Fatalf("沙箱安装应成功：%v\n步骤：%v", err, res.Steps)
	}
	// 旧 plist 必须没了，且旧 label 被 bootout 过一次（不能两个实例抢 8895）。
	if _, err := os.Stat(env.legacy); !os.IsNotExist(err) {
		t.Errorf("安装后旧用户级 plist %s 必须被删除，实际 err=%v", env.legacy, err)
	}
	if len(env.legacyStop) != 1 || env.legacyStop[0] != env.legacy {
		t.Errorf("必须恰好清理一次旧用户级 agent，实际 %v", env.legacyStop)
	}
	// 只允许 bootstrap 新 label（旧 label 一次都不许被 bootstrap）。
	if len(env.launch) != 1 || !strings.HasPrefix(env.launch[0], MacSaberLabel+"|") {
		t.Errorf("必须恰好 bootstrap 一次 %s，实际 %v", MacSaberLabel, env.launch)
	}
	for _, l := range env.launch {
		if strings.Contains(l, MacSaberLegacyLabel) {
			t.Errorf("不许 bootstrap 旧 label %s：%v", MacSaberLegacyLabel, env.launch)
		}
	}
	// 顺序：必须**先**清理旧 agent，**再** bootstrap 新守护进程 ——
	// 反过来的话旧实例还占着 8895，新的必然起不来（安装会"看起来失败"）。
	legacyAt, launchAt := indexOfEvent(env.events, "legacy:"+MacSaberLegacyLabel),
		indexOfEvent(env.events, "launch:"+MacSaberLabel)
	if legacyAt < 0 || launchAt < 0 {
		t.Fatalf("事件记录不完整（legacy=%d launch=%d）：%v", legacyAt, launchAt, env.events)
	}
	if legacyAt > launchAt {
		t.Errorf("必须先清理旧用户级 agent 再 bootstrap 新守护进程，实际顺序：%v", env.events)
	}
	// 终态只允许一个系统 daemon。
	plists, err := filepath.Glob(filepath.Join(SystemLaunchDaemonsDir, "*.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plists) != 1 || filepath.Base(plists[0]) != MacSaberLabel+".plist" {
		t.Errorf("迁移后只允许一份系统 plist（%s），实际 %v", MacSaberLabel, plists)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), MacSaberLegacyLabel) {
		t.Errorf("安装步骤要如实说明迁移了旧 agent：%v", res.Steps)
	}
}

// indexOfEvent 返回事件在序列里的下标（找不到 -1）。
func indexOfEvent(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}

// TestInstallMacSaberSandboxed 走一遍安装：二进制落盘、系统 plist 写出、服务登记、验收。
func TestInstallMacSaberSandboxed(t *testing.T) {
	env := newMacSaberInstallManager(t)
	stageFakeInstall(t, env)
	env.stubHTTPGet(`{"ok":true,"version":"`+MacSaberVersion+`"}`, 200, nil)

	app, ok := FindApp(MacSaberAppID)
	if !ok {
		t.Fatalf("目录里应有 %s 条目", MacSaberAppID)
	}
	res := &InstallResult{Steps: []string{}}
	if err := env.m.InstallMacSaber(context.Background(), app, res); err != nil {
		t.Fatalf("沙箱安装应成功：%v\n步骤：%v", err, res.Steps)
	}
	if _, err := os.Stat(MacSaberBin()); err != nil {
		t.Fatalf("安装后 %s 必须存在：%v", MacSaberBin(), err)
	}
	if _, err := os.Stat(env.plist); err != nil {
		t.Fatalf("安装后系统 plist 必须写出（%s）：%v", env.plist, err)
	}
	if st, err := os.Stat(env.plist); err == nil && st.Mode().Perm() != 0o644 {
		t.Errorf("系统 plist 权限必须是 0644，实际 %v", st.Mode().Perm())
	}
	if content, err := os.ReadFile(env.plist); err == nil && strings.Contains(string(content), "<key>UserName</key>") {
		t.Errorf("系统 plist 不许写 UserName（supervisor 需要 root 才能降权）：\n%s", content)
	}
	if len(env.launch) != 1 || !strings.HasPrefix(env.launch[0], MacSaberLabel+"|") {
		t.Errorf("必须恰好 bootstrap 一次 %s，实际 %v", MacSaberLabel, env.launch)
	}
	// plist 内容必须指向面板二进制 + supervisor 子命令。
	if content, err := os.ReadFile(env.plist); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(content), env.panel+"</string>") ||
		!strings.Contains(string(content), "macsaber-supervise") {
		t.Errorf("系统 plist 必须用面板自身执行 macsaber-supervise：\n%s", content)
	}
	// 服务记录必须登记（否则「服务管理」里找不到它）。
	list, err := env.m.repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range list {
		if s.LaunchLabel == MacSaberLabel {
			found = true
		}
	}
	if !found {
		t.Errorf("安装后必须有 %s 的服务记录：%+v", MacSaberLabel, list)
	}
	// 可写根必须建好（服务以真实用户身份跑，写不进去就是装完不能用）。
	if _, err := os.Stat(filepath.Join(env.m.opt.UserHome, "MacSaberFiles")); err != nil {
		t.Errorf("可写根 %s 必须被创建：%v", filepath.Join(env.m.opt.UserHome, "MacSaberFiles"), err)
	}
	joined := strings.Join(res.Steps, "\n")
	for _, want := range []string{"SHA-256 校验通过", "/macsaber/", fmt.Sprintf("127.0.0.1:%d", MacSaberPort), "共用文件权限"} {
		if !strings.Contains(joined, want) {
			t.Errorf("安装步骤里应包含 %q：\n%s", want, joined)
		}
	}
}

// TestInstallMacSaberIsIdempotent 重复安装必须幂等（不重复 bootstrap、二进制仍是那一份）。
func TestInstallMacSaberIsIdempotent(t *testing.T) {
	env := newMacSaberInstallManager(t)
	stageFakeInstall(t, env)
	env.stubHTTPGet(`{"ok":true,"version":"`+MacSaberVersion+`"}`, 200, nil)
	app, _ := FindApp(MacSaberAppID)

	for i := 0; i < 2; i++ {
		res := &InstallResult{Steps: []string{}}
		if err := env.m.InstallMacSaber(context.Background(), app, res); err != nil {
			t.Fatalf("第 %d 次安装失败：%v", i+1, err)
		}
	}
	if len(env.launch) != 2 {
		t.Errorf("两次安装应各 bootstrap 一次（先停后装），实际 %v", env.launch)
	}
	list, _ := env.m.repo.List(context.Background())
	n := 0
	for _, s := range list {
		if s.LaunchLabel == MacSaberLabel {
			n++
		}
	}
	if n != 1 {
		t.Errorf("重复安装只允许一条服务记录，实际 %d：%+v", n, list)
	}
	// 只允许一个系统 plist（旧的用户级 plist 也不许冒出来）。
	plists, err := filepath.Glob(filepath.Join(SystemLaunchDaemonsDir, "*.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plists) != 1 || filepath.Base(plists[0]) != MacSaberLabel+".plist" {
		t.Errorf("重复安装后只允许一份系统 plist，实际 %v", plists)
	}
	if _, err := os.Stat(MacSaberBin()); err != nil {
		t.Errorf("重复安装后二进制必须还在：%v", err)
	}
}

// TestInstallMacSaberFailsWhenHealthNeverComesUp 验收失败必须**如实报错**，
// 绝不能把"plist 写出来了"当成"装好了"（僵尸 plist 谎报已装是本仓库踩过的坑）。
func TestInstallMacSaberFailsWhenHealthNeverComesUp(t *testing.T) {
	env := newMacSaberInstallManager(t)
	stageFakeInstall(t, env)
	env.stubHTTPGet("", 0, errString("Connection refused"))
	// 把等待时间缩到毫秒级（真实实现是 60 秒）。
	app, _ := FindApp(MacSaberAppID)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res := &InstallResult{Steps: []string{}}
	err := env.m.InstallMacSaber(ctx, app, res)
	if err == nil {
		t.Fatal("服务没起来时安装必须返回 error（不许谎报成功）")
	}
	if !strings.Contains(err.Error(), "未就绪") && !strings.Contains(err.Error(), "context") {
		t.Errorf("错误信息要交代未就绪这件事，实际：%v", err)
	}
	if len(env.launch) == 0 {
		t.Error("这一步应该已经 bootstrap 过（如实告诉用户问题出在起服务之后）")
	}
}

// TestInstallMacSaberFailsHonestlyOnDownloadError 下载失败必须如实报错，且不留半成品。
func TestInstallMacSaberFailsHonestlyOnDownloadError(t *testing.T) {
	env := newMacSaberInstallManager(t)
	prevFetch := macSaberFetch
	t.Cleanup(func() { macSaberFetch = prevFetch })
	macSaberFetch = func(context.Context, string, string, func(int64, int64)) error {
		return errString("dial tcp: lookup mirror.example.test: no such host")
	}
	app, _ := FindApp(MacSaberAppID)
	res := &InstallResult{Steps: []string{}}
	err := env.m.InstallMacSaber(context.Background(), app, res)
	if err == nil {
		t.Fatal("下载失败必须返回 error")
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Errorf("错误要带真实原因：%v", err)
	}
	if _, serr := os.Stat(MacSaberBin()); serr == nil {
		t.Error("下载失败时不许在安装根留下二进制")
	}
	if len(env.launch) != 0 {
		t.Error("下载失败时不该去碰 launchd")
	}
}

// TestInstallMacSaberRefusesWithoutMirror 镜像基址为空时必须明确失败（没有上游可回落）。
func TestInstallMacSaberRefusesWithoutMirror(t *testing.T) {
	env := newMacSaberTestEnv(t) // 注意：不设 MirrorBase
	app, _ := FindApp(MacSaberAppID)
	err := env.m.InstallMacSaber(context.Background(), app, &InstallResult{Steps: []string{}})
	if err == nil {
		t.Fatal("没有镜像基址时必须失败（这个应用没有公开回落源）")
	}
	if !strings.Contains(err.Error(), "镜像") {
		t.Errorf("错误要说清是镜像基址的问题：%v", err)
	}
}

// TestInstallMacSaberRejectsWrongArchiveSHA 校验不过必须中止，且不落盘。
func TestInstallMacSaberRejectsWrongArchiveSHA(t *testing.T) {
	env := newMacSaberInstallManager(t)
	stageFakeInstall(t, env)
	MacSaberArchiveSHA256 = strings.Repeat("0", 64)
	env.stubHTTPGet(`{"ok":true,"version":"`+MacSaberVersion+`"}`, 200, nil)
	app, _ := FindApp(MacSaberAppID)
	err := env.m.InstallMacSaber(context.Background(), app, &InstallResult{Steps: []string{}})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("sha256 不符必须中止并点名，实际：%v", err)
	}
	if _, serr := os.Stat(MacSaberBin()); serr == nil {
		t.Error("校验失败时不许在安装根留下二进制")
	}
}

// TestMacSaberArm64CheckIsNotVacuous 证明架构复核不是空断言：
// 一份非 Mach-O arm64 的文件必须被 file(1) 判掉。
func TestMacSaberArm64CheckIsNotVacuous(t *testing.T) {
	env := newMacSaberTestEnv(t)
	bin := env.writeFakeBin(t, "#!/bin/sh\necho hi\n")
	err := verifyMacSaberArm64(env.m, context.Background(), bin)
	if err == nil {
		t.Fatal("文本脚本不是 Mach-O arm64，复核必须失败（否则这道门禁形同虚设）")
	}
	if !strings.Contains(err.Error(), "arm64") {
		t.Errorf("错误要点名 arm64：%v", err)
	}
}

// ---------------------------------------------------------------------------
//  卸载：删什么、留什么，并复核终态
// ---------------------------------------------------------------------------

// TestUninstallMacSaberRemovesPayloadKeepsData 默认卸载 = 删运行体、留数据，
// 且新旧两份 plist 都要没有、8895 不再返回本版。
func TestUninstallMacSaberRemovesPayloadKeepsData(t *testing.T) {
	env := newMacSaberTestEnv(t)
	env.writeFakeBin(t, "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Dir(env.plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.writeLegacyAgent(t) // 旧版残留也要一起清掉
	p := env.m.MacSaberPathsFor()
	for _, dir := range []string{p.DataDir, p.WriteRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app, _ := FindApp(MacSaberAppID)
	res := &InstallResult{Steps: []string{}}
	if err := env.m.UninstallMacSaber(context.Background(), app, false, res); err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	if _, err := os.Stat(env.root); !os.IsNotExist(err) {
		t.Errorf("默认卸载必须删掉安装根 %s，实际 err=%v", env.root, err)
	}
	if _, err := os.Stat(env.plist); !os.IsNotExist(err) {
		t.Errorf("默认卸载必须删掉系统 plist，实际 err=%v", err)
	}
	if _, err := os.Stat(env.legacy); !os.IsNotExist(err) {
		t.Errorf("默认卸载必须清掉旧的用户级 plist，实际 err=%v", err)
	}
	if len(env.stop) != 1 || !strings.HasPrefix(env.stop[0], MacSaberLabel+"|") {
		t.Errorf("必须恰好停止一次 %s，实际 %v", MacSaberLabel, env.stop)
	}
	if len(env.legacyStop) != 1 || env.legacyStop[0] != env.legacy {
		t.Errorf("必须清理一次旧的用户级 agent，实际 %v", env.legacyStop)
	}
	for _, dir := range []string{p.DataDir, p.WriteRoot} {
		if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
			t.Errorf("默认必须保留 %s（用户数据），实际 err=%v", dir, err)
		}
	}
	joined := strings.Join(res.Steps, "\n")
	for _, want := range []string{p.DataDir, "卸载复核通过"} {
		if !strings.Contains(joined, want) {
			t.Errorf("卸载步骤里应包含 %q：\n%s", want, joined)
		}
	}
}

// TestUninstallMacSaberRemoveDataDeletesEverything 显式删数据时才删数据目录与可写根。
func TestUninstallMacSaberRemoveDataDeletesEverything(t *testing.T) {
	env := newMacSaberTestEnv(t)
	env.writeFakeBin(t, "#!/bin/sh\nexit 0\n")
	p := env.m.MacSaberPathsFor()
	for _, dir := range []string{p.DataDir, p.WriteRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	app, _ := FindApp(MacSaberAppID)
	res := &InstallResult{Steps: []string{}}
	if err := env.m.UninstallMacSaber(context.Background(), app, true, res); err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	for _, dir := range []string{p.DataDir, p.WriteRoot, env.root} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("勾选删除数据后 %s 必须不存在，实际 err=%v", dir, err)
		}
	}
}

// TestUninstallMacSaberFailsWhenServiceStillServing 卸载后 8895 还在返回本版时必须报错，
// 不许"删了文件就说卸干净了"（判据贴着运行体）。
func TestUninstallMacSaberFailsWhenServiceStillServing(t *testing.T) {
	env := newMacSaberTestEnv(t)
	env.writeFakeBin(t, "#!/bin/sh\nexit 0\n")
	env.stubHTTPGet(`{"ok":true,"version":"`+MacSaberVersion+`"}`, 200, nil)
	app, _ := FindApp(MacSaberAppID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := env.m.UninstallMacSaber(ctx, app, false, &InstallResult{Steps: []string{}})
	if err == nil {
		t.Fatal("8895 仍在返回 mac军刀 时必须报错（不许谎报卸载成功）")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", MacSaberPort)) {
		t.Errorf("错误要点名是哪个端口还在服务：%v", err)
	}
}

// TestUninstallMacSaberIsIdempotent 什么都没装时卸载不该报错、也不该动 launchd。
func TestUninstallMacSaberIsIdempotent(t *testing.T) {
	env := newMacSaberTestEnv(t)
	app, _ := FindApp(MacSaberAppID)
	res := &InstallResult{Steps: []string{}}
	if err := env.m.UninstallMacSaber(context.Background(), app, false, res); err != nil {
		t.Fatalf("幂等卸载不该报错：%v", err)
	}
	if len(env.stop) != 0 {
		t.Errorf("本来就没注册时不该去停止系统服务，实际 %v", env.stop)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "本来就没有注册") {
		t.Errorf("应如实说明「本来就没注册」：%v", res.Steps)
	}
}

// TestMacSaberUninstallPlanNamesWhatIsKept 卸载计划必须写清"删什么、留什么"。
func TestMacSaberUninstallPlanNamesWhatIsKept(t *testing.T) {
	env := newMacSaberTestEnv(t)
	app, _ := FindApp(MacSaberAppID)
	plan := env.m.installerPlan(context.Background(), app)
	if plan.Kind != "installer" {
		t.Fatalf("mac军刀 应给 installer 计划，实际 %q（blocked=%q）", plan.Kind, plan.Blocked)
	}
	if plan.Blocked != "" {
		t.Fatalf("计划不该被 blocked：%q", plan.Blocked)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("计划没有任何步骤，确认框会是一片空白")
	}
	if len(plan.DataPaths) == 0 {
		t.Error("计划必须列出「可选删除」的数据路径")
	}
	p := env.m.MacSaberPathsFor()
	for _, want := range []string{p.DataDir, p.WriteRoot} {
		if !strings.Contains(strings.Join(plan.DataPaths, "\n"), want) {
			t.Errorf("DataPaths 缺少 %s：%v", want, plan.DataPaths)
		}
		if !strings.Contains(plan.KeepNote, want) {
			t.Errorf("KeepNote 要点名保留了什么（%s），实际 %q", want, plan.KeepNote)
		}
	}
	// 计划里必须同时点名系统 plist 与旧用户级 agent（迁移/兼容清理是真实动作）。
	joined := strings.Join(plan.Steps, "\n")
	for _, want := range []string{p.Plist, MacSaberLegacyLabel, p.Root} {
		if !strings.Contains(joined, want) {
			t.Errorf("卸载计划缺少 %q：\n%s", want, joined)
		}
	}
}

// ---------------------------------------------------------------------------
//  目录 / 下载点声明
// ---------------------------------------------------------------------------

// TestMacSaberCatalogEntry 锁住条目的关键声明。
func TestMacSaberCatalogEntry(t *testing.T) {
	app, ok := FindApp(MacSaberAppID)
	if !ok {
		t.Fatalf("目录里应有 %s 条目", MacSaberAppID)
	}
	if app.Name != "mac军刀" {
		t.Errorf("展示名应是 mac军刀，实际 %q", app.Name)
	}
	if app.Category != CategoryTool {
		t.Errorf("分类必须是运维工具（%s），实际 %q", CategoryTool, app.Category)
	}
	if app.UI == nil || app.UI.Slug != MacSaberSlug {
		t.Errorf("必须声明 UI.Slug=%s（否则卡片上没有「打开」入口）：%+v", MacSaberSlug, app.UI)
	}
	if app.Port != MacSaberPort || app.HealthPath != macSaberHealthPath {
		t.Errorf("端口/健康路径不对：port=%d path=%q", app.Port, app.HealthPath)
	}
	if app.ServiceLabel != MacSaberLabel {
		t.Errorf("ServiceLabel 必须是 %s，实际 %q", MacSaberLabel, app.ServiceLabel)
	}
	if app.PanelInstaller != MacSaberAppID {
		t.Errorf("PanelInstaller 应是 %s，实际 %q", MacSaberAppID, app.PanelInstaller)
	}
	if app.SystemDaemon {
		// 那个字段驱动的是"把 brew services 的 agent 搬到系统域"；这个条目
		// 自己写系统 plist（同 release 二进制那条轨），所以刻意不标。
		t.Error("mac军刀 不该标 SystemDaemon：它自己写系统 plist，不经过 brew services 搬迁")
	}
	if app.BrewFormula != "" {
		t.Errorf("这个条目不该有 BrewFormula（没有 brew 包）：%q", app.BrewFormula)
	}
	if !HasInstallerUninstall(app.PanelInstaller) {
		t.Error("有 PanelInstaller 却查不到卸载实现（装上就卸不掉）")
	}
	// 文案：必须如实说明"与面板共用文件权限"，且不许再写"首次弹一次隐私授权"。
	if !strings.Contains(app.PostInstallHint, "文件权限与面板共用") {
		t.Errorf("PostInstallHint 要如实说明共用文件权限：%q", app.PostInstallHint)
	}
	for _, banned := range []string{"隐私授权", "弹一次"} {
		if strings.Contains(app.PostInstallHint, banned) {
			t.Errorf("PostInstallHint 还在写旧口径 %q：%q", banned, app.PostInstallHint)
		}
	}
}

// TestMacSaberMarketDeclaration 锁住下载点声明与"唯一来源"的事实。
func TestMacSaberMarketDeclaration(t *testing.T) {
	m, ok := MarketAppFor(MacSaberAppID)
	if !ok {
		t.Fatalf("声明里应有 %s", MacSaberAppID)
	}
	if len(m.Downloads) != 1 {
		t.Fatalf("mac军刀 只有一个下载点（镜像站），实际 %d", len(m.Downloads))
	}
	if m.Runtime.Label != MacSaberLabel {
		t.Errorf("市场声明的运行 label 必须是 %s（与目录一致），实际 %q", MacSaberLabel, m.Runtime.Label)
	}
	d := m.Downloads[0]
	if d.Purpose != MarketFetchReleaseBinary {
		t.Errorf("用途应是 release_binary，实际 %q", d.Purpose)
	}
	if d.NAS.State != NASMirrored {
		t.Fatalf("镜像站状态必须是 mirrored（这是唯一来源），实际 %q", d.NAS.State)
	}
	wantPath := "apps/macsaber/" + MacSaberVersion + "/" + MacSaberArtifactName(MacSaberVersion)
	if d.NAS.Path != wantPath {
		t.Errorf("镜像站路径应是 %s，实际 %s", wantPath, d.NAS.Path)
	}
	if !d.Required {
		t.Error("唯一来源的下载点必须 Required=true")
	}
	if d.Timeout <= 0 {
		t.Error("下载点必须有真实超时")
	}
	// Tag/Asset 必须与注册表一致（市场门禁比对的就是这两个），但不许填 Repo：
	// 填了会被拿去拼一个不存在的 github.com 地址。
	if d.Upstream.Tag != MacSaberVersion || d.Upstream.Asset != MacSaberArtifactName(MacSaberVersion) {
		t.Errorf("Tag/Asset 必须与注册表一致（%s/%s），实际 %q/%q",
			MacSaberVersion, MacSaberArtifactName(MacSaberVersion), d.Upstream.Tag, d.Upstream.Asset)
	}
	if d.Upstream.Repo != "" {
		t.Errorf("自研产物不该填 GitHub repo（没有官方地址）：%q", d.Upstream.Repo)
	}
	if got := ReleaseBinaryAssets(); !containsAsset(got, MacSaberAppID) {
		t.Error("注册表 ReleaseBinaryAssets() 里必须有 macsaber（镜像同步与门禁都靠它）")
	}
	if !ReleaseBinaryIsMirrorOnly(MacSaberAppID) {
		t.Error("macsaber 必须在注册表里标成 MirrorOnly（没有公网回落源）")
	}
	if d.Checksum.SHA256 != MacSaberArchiveSHA256 {
		t.Errorf("声明的 sha256 必须与打包实测值一致（%s），实际 %q", MacSaberArchiveSHA256, d.Checksum.SHA256)
	}
	if strings.TrimSpace(d.Checksum.Source) == "" {
		t.Error("声明了 sha256 就必须写 Source")
	}
	if strings.TrimSpace(d.ARM64) == "" {
		t.Error("必须有 arm64 证据")
	}
	// 反漂移：声明与目录逐字段一致（这条也是全目录门禁的一部分，这里单独报更清楚）。
	app, _ := FindApp(MacSaberAppID)
	if problems := MarketDeclarationProblems(m, app); len(problems) != 0 {
		t.Errorf("声明与目录不一致：%v", problems)
	}
}

// TestMacSaberArtifactName 锁住产物命名（发布脚本、镜像布局、安装器三方共用）。
func TestMacSaberArtifactName(t *testing.T) {
	if got := MacSaberArtifactName(MacSaberVersion); got != "macsaber_0.1.0_darwin_arm64.tar.gz" {
		t.Errorf("产物名不对：%s", got)
	}
	if !strings.HasSuffix(MacSaberArtifactName("9.9.9"), "macsaber_9.9.9_darwin_arm64.tar.gz") {
		t.Error("产物名必须跟版本走")
	}
}

// containsAsset 判断注册表里有没有某个 ID。
func containsAsset(list []ReleaseBinaryAsset, id string) bool {
	for _, a := range list {
		if a.ID == id {
			return true
		}
	}
	return false
}

// errString 是测试用的最小 error（不引额外依赖）。
type errString string

func (e errString) Error() string { return string(e) }

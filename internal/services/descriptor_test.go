package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  描述符 v2 + steps DSL 的单测
//
//  三条硬约束（AGENTS.md 第三节）：
//    1. **绝不联网** —— 所有下载命令由 fakeRunner 在本地 t.TempDir() 里伪造；
//    2. **绝不碰真实 launchd** —— bootstrap/bootout 只被记录，不执行 launchctl；
//    3. **绝不动真实家目录/生产配置** —— 安装根目录一律是 t.TempDir()。
//
//  这套测试要证明的三件事：
//    · DSL 执行器的编排是对的（下载 → 校验 → 解压 → 落盘 → 注册 → 验收）；
//    · 三个 tarball 应用的"描述符 → 步骤序列"与老实现逐条对齐；
//    · **新增一个应用只需要写一份描述符**（下面那个假应用就是证明）。
// ============================================================================

// fakeRunner 是测试用的 Runner：文件操作真的做（在 t.TempDir 里），
// 命令执行被记录并交给调用方的钩子，launchd 一律只记录。
//
// 用真实文件系统而不是全内存模拟：这样"解压后文件真的在不在""chmod 之后
// 权限对不对"这类断言测的是与生产同一条代码路径。
type fakeRunner struct {
	cmds       []string
	bootouts   []string
	bootstraps []string
	// runHook 按命令名决定"伪造的输出与是否成功"。
	runHook func(name string, args []string) (string, error)
	// launchRunning 是 readyLaunchRunning 的替身。
	launchRunning func(label string) (bool, string)
	// waitPort 是 readyWaitPort 的替身。
	waitPort func(port int) bool
}

func (f *fakeRunner) Stat(path string) (bool, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, fi.IsDir(), nil
}
func (f *fakeRunner) MkdirAll(p string, m os.FileMode) error            { return os.MkdirAll(p, m) }
func (f *fakeRunner) WriteFile(p string, b []byte, m os.FileMode) error { return os.WriteFile(p, b, m) }
func (f *fakeRunner) ReadFile(p string) ([]byte, error)                 { return os.ReadFile(p) }
func (f *fakeRunner) Size(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return fi.Size()
}
func (f *fakeRunner) Chmod(p string, m os.FileMode) error { return os.Chmod(p, m) }
func (f *fakeRunner) Rename(a, b string) error            { return os.Rename(a, b) }
func (f *fakeRunner) Remove(p string) error {
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
func (f *fakeRunner) RemoveAll(p string) error        { return os.RemoveAll(p) }
func (f *fakeRunner) SHA256(p string) (string, error) { return fileSHA256(p) }
func (f *fakeRunner) ChownTree(string, string) error  { return nil }
func (f *fakeRunner) CopyTree(a, b string) error      { return copyTreeNative(a, b) }
func (f *fakeRunner) FileType(_ context.Context, p string) (string, error) {
	return "Mach-O 64-bit executable arm64", nil
}
func (f *fakeRunner) run(name string, args []string) (string, error) {
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	if f.runHook != nil {
		return f.runHook(name, args)
	}
	return "", nil
}
func (f *fakeRunner) RunAsUser(_ context.Context, _ time.Duration, n string, a ...string) (string, error) {
	return f.run(n, a)
}
func (f *fakeRunner) RunAsUserEnv(_ context.Context, _ time.Duration, _ []string, n string, a ...string) (string, error) {
	return f.run(n, a)
}
func (f *fakeRunner) RunRoot(_ context.Context, _ time.Duration, n string, a ...string) (string, error) {
	return f.run(n, a)
}
func (f *fakeRunner) Bootout(_ context.Context, label string) error {
	f.bootouts = append(f.bootouts, label)
	return nil
}
func (f *fakeRunner) Bootstrap(_ context.Context, label, plist string) error {
	f.bootstraps = append(f.bootstraps, label+"|"+plist)
	return nil
}
func (f *fakeRunner) LaunchRunning(label string) (bool, string) {
	if f.launchRunning != nil {
		return f.launchRunning(label)
	}
	return true, "launchd 已拉起该作业（pid 4242）"
}
func (f *fakeRunner) WaitPort(_ context.Context, port int, _ time.Duration) bool {
	if f.waitPort != nil {
		return f.waitPort(port)
	}
	return true
}
func (f *fakeRunner) HTTPGet(context.Context, string, time.Duration) (int, error) {
	return 200, nil
}
func (f *fakeRunner) PrimaryIP(context.Context) string { return "192.168.1.4" }

// testExec 造一个执行现场：安装根目录在 t.TempDir() 下，绝不碰真实家目录。
func testExec(t *testing.T, d AppDescriptor, runner Runner) (*ExecConfig, string) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, d.Paths.RootDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ec := &ExecConfig{
		Ctx:    context.Background(),
		Spec:   d,
		Result: &InstallResult{App: d.ID, Name: d.Name, Steps: []string{}},
		Runner: runner,
		pathVars: map[string]string{
			"{root}": root,
			"{home}": home,
			"{user}": "tester",
		},
	}
	return ec, root
}

// ---------------------------------------------------------------------------
//  ① 描述符自检与注册表一致性
// ---------------------------------------------------------------------------

// TestTarballDescriptorsMatchRegistry 锁住"参数表与描述符是同一份事实"。
//
// 加新应用时最容易漏的就是这里：releaseBinaryApps 里有、描述符没注册
// （市场点了安装分流到 InstallReleaseBinary 却找不到描述符），或者反过来。
func TestTarballDescriptorsMatchRegistry(t *testing.T) {
	for _, id := range tarballDescriptors {
		spec, ok := releaseBinaryApps[id]
		if !ok {
			t.Errorf("tarball 列表里的 %s 不在 releaseBinaryApps 里", id)
			continue
		}
		d, ok := FindDescriptor(id)
		if !ok {
			t.Errorf("%s 没有注册描述符（市场点了安装会报「没有安装器」）", id)
			continue
		}
		if err := d.Validate(); err != nil {
			t.Errorf("%s 的描述符自检失败：%v", id, err)
		}
		// 同一个事实写两遍的地方必须逐字一致（它们是"打开"入口、健康检查、
		// 「编辑配置文件」定位、已安装判定的依据）。
		if d.Service.Label != spec.Label {
			t.Errorf("%s 的 launchd label 不一致：描述符 %q / 参数表 %q",
				id, d.Service.Label, spec.Label)
		}
		if d.Port != spec.Port {
			t.Errorf("%s 的端口不一致：描述符 %d / 参数表 %d", id, d.Port, spec.Port)
		}
		if d.Paths.ConfigFile != spec.ConfigFile {
			t.Errorf("%s 的配置文件不一致：描述符 %q / 参数表 %q",
				id, d.Paths.ConfigFile, spec.ConfigFile)
		}
		if d.PanelInstaller != id {
			t.Errorf("%s 的 PanelInstaller 应为 %q，实际 %q", id, id, d.PanelInstaller)
		}
		// 产物必须来自参数表（单一事实来源），且带 arm64 证据字段。
		if len(d.Artifacts) == 0 {
			t.Fatalf("%s 没有产物", id)
		}
		if d.Artifacts[0].Name != spec.Asset || d.Artifacts[0].Version != spec.Tag {
			t.Errorf("%s 的产物与参数表不一致：%s@%s vs %s@%s",
				id, d.Artifacts[0].Name, d.Artifacts[0].Version, spec.Asset, spec.Tag)
		}
		if d.Artifacts[0].Arm64 == nil {
			t.Errorf("%s 的产物缺少 arm64 证据字段（铁律 8 要求留下取数来源）", id)
		}
		// url 约定：proxy_url 缺失必须显式声明原因（Validate 已经强制，
		// 这里再断言一次，避免有人把 Validate 的检查删掉）。
		if d.Urls.ProxyURL == "" && strings.TrimSpace(d.Urls.ProxyURLReason) == "" {
			t.Errorf("%s 没有 proxy_url 也没有 proxy_url_reason", id)
		}
		if !IsReleaseBinaryApp(id) {
			t.Errorf("IsReleaseBinaryApp(%s) 应为 true（否则 web 层不会分流到安装器）", id)
		}
	}
}

// TestTarballRailOnlyContainsMigratedApps 锁住"tarball 轨上有哪些应用"。
//
// 目标 rail 是 tarball 的应用必须都真的有描述符；反过来，描述符里 rail=tarball
// 的也必须都在参数表里（否则 web 分流与执行器会对不上）。
//
// 2026-09-17 新增 alist（它从未进过 homebrew-core，只能走官方 release 产物）。
// 2026-09-19 新增 filebrowser（homebrew formula 没有 service 定义，改走官方 darwin-arm64 产物）。
func TestTarballRailOnlyContainsMigratedApps(t *testing.T) {
	got := descriptorIDsForRail(RailTarball)
	want := []string{"alist", "ddns-go", "filebrowser", "frpc", "orbien-client"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tarball 轨的应用 = %v，期望 %v", got, want)
	}
	for _, id := range got {
		if !IsReleaseBinaryApp(id) {
			t.Errorf("%s 在 tarball 轨上，但 IsReleaseBinaryApp 为 false", id)
		}
	}
}

// ---------------------------------------------------------------------------
//  ② 三个应用的"描述符 → 步骤序列"断言
// ---------------------------------------------------------------------------

// stepKinds 把描述符的步骤序列压成类型名列表（断言顺序用）。
func stepKinds(d AppDescriptor) []string {
	out := make([]string, 0, len(d.Steps))
	for _, s := range d.Steps {
		out = append(out, s.Kind())
	}
	return out
}

// TestTarballStepSequences 逐个应用断言步骤序列。
//
// 顺序是有理由的，不是随便排的（详见 tarballInstallSteps 的注释）：
//   - 校验在**解压之前**：宁可下载完立刻失败，也不要把校验不通过的 tarball
//     解压出来、chmod、再交给 launchd 去执行；
//   - 登记在**验收之前**：验收如实失败时，登记留在后面会留下"任务失败、
//     服务管理里又找不到它"的半成品。
func TestTarballStepSequences(t *testing.T) {
	cases := []struct {
		id string
		// want 是步骤类型序列。
		want []string
	}{
		// frpc 有上游 checksums，所以多一步下载清单。
		{"frpc", []string{
			"ensure_dir", "download", "download", "verify_sha256", "extract",
			"verify_arm64", "chmod", "ensure_config", "chown", "write_plist",
			"launchd_bootstrap", "register_service", "message", "assert_ready",
		}},
		// orbien-client 上游没有 checksums：没有清单下载步，verify_sha256 靠
		// SkipIfNoChecksum 放行（由 verify_arm64 兜底）。
		{"orbien-client", []string{
			"ensure_dir", "download", "verify_sha256", "extract",
			"verify_arm64", "chmod", "ensure_config", "chown", "write_plist",
			"launchd_bootstrap", "register_service", "message", "assert_ready",
		}},
		{"ddns-go", []string{
			"ensure_dir", "download", "download", "verify_sha256", "extract",
			"verify_arm64", "chmod", "ensure_config", "chown", "write_plist",
			"launchd_bootstrap", "register_service", "message", "assert_ready",
		}},
	}
	for _, tc := range cases {
		d, ok := FindDescriptor(tc.id)
		if !ok {
			t.Fatalf("没有 %s 的描述符", tc.id)
		}
		got := stepKinds(d)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s 的步骤序列不对：\n got=%v\nwant=%v", tc.id, got, tc.want)
		}
	}
}

// TestTarballExtractStepsMatchLegacyArgs 锁住解压参数与老实现逐字一致。
//
// 这条区分是**真机踩出来的**：ddns-go 的产物没有顶层目录，沿用"成员名一定带
// 顶层目录（用 asset 名去掉 .tar.gz 猜）"的老写法，tar 会去找一个不存在的
// ddns-go_6.17.7_darwin_arm64/ddns-go，解压直接失败。
func TestTarballExtractStepsMatchLegacyArgs(t *testing.T) {
	specs := releaseBinaryApps
	// frpc：剥 1 层、只挑 frpc，成员名必须带顶层目录。
	spec := specs["frpc"]
	d, _ := FindDescriptor("frpc")
	ext := firstExtract(t, d)
	if ext.Member != "frpc" || ext.StripComponents != 1 {
		t.Errorf("frpc 的挑成员参数不对：member=%q strip=%d", ext.Member, ext.StripComponents)
	}
	args := strings.Join(tarExtractArgs("/tmp/x/"+spec.Asset, "/root", ext.Member, ext.StripComponents), " ")
	if !strings.Contains(args, "--strip-components=1") {
		t.Errorf("frpc 必须剥一层顶层目录：%s", args)
	}
	if !strings.HasSuffix(args, "frp_0.71.0_darwin_arm64/frpc") {
		t.Errorf("frpc 的成员名必须带顶层目录：%s", args)
	}
	// 与老方法算出来的必须完全一致（换轨不许改行为）。
	legacy := strings.Join(spec.extractArgs("/tmp/x/"+spec.Asset, "/root"), " ")
	if legacy != args {
		t.Errorf("新老解压参数不一致：\n legacy=%s\n   new=%s", legacy, args)
	}

	// ddns-go：无顶层目录、挑 ddns-go。
	spec = specs["ddns-go"]
	d, _ = FindDescriptor("ddns-go")
	ext = firstExtract(t, d)
	args = strings.Join(tarExtractArgs("/tmp/x/"+spec.Asset, "/root", ext.Member, ext.StripComponents), " ")
	if strings.Contains(args, "--strip-components") {
		t.Errorf("ddns-go 的产物没有顶层目录，不该带 --strip-components：%s", args)
	}
	if !strings.HasSuffix(args, " ddns-go") {
		t.Errorf("ddns-go 的成员名应是平级的 ddns-go：%s", args)
	}
	if legacy := strings.Join(spec.extractArgs("/tmp/x/"+spec.Asset, "/root"), " "); legacy != args {
		t.Errorf("ddns-go 新老解压参数不一致：\n legacy=%s\n   new=%s", legacy, args)
	}

	// orbien-client：整体解压（与老实现一致，老代码也不挑成员）。
	spec = specs["orbien-client"]
	d, _ = FindDescriptor("orbien-client")
	ext = firstExtract(t, d)
	if ext.Member != "" || ext.StripComponents != 0 {
		t.Errorf("orbien-client 应整体解压，实际 member=%q strip=%d", ext.Member, ext.StripComponents)
	}
	if legacy := strings.Join(spec.extractArgs("a.tar.gz", "/r"), " "); legacy != "-xzf a.tar.gz -C /r" {
		t.Errorf("orbien-client 老参数被改了：%s", legacy)
	}
}

func firstExtract(t *testing.T, d AppDescriptor) ExtractAction {
	t.Helper()
	for _, s := range d.Steps {
		if a, ok := s.(ExtractAction); ok {
			return a
		}
	}
	t.Fatalf("%s 的描述符里没有 extract 步骤", d.ID)
	return ExtractAction{}
}

// TestTarballDescriptorPolicyMatchesLegacy 锁住配置/健康/卸载三类策略字段。
func TestTarballDescriptorPolicyMatchesLegacy(t *testing.T) {
	// frpc：端口 7400、健康路径 "/"、面板 marker 判据（不是"存在即保留"）。
	frpc, _ := FindDescriptor("frpc")
	if frpc.Health.Port != 7400 || frpc.Health.Probe != ProbePort {
		t.Errorf("frpc 的健康判定应为端口 7400，实际 %+v", frpc.Health)
	}
	if frpc.Health.Degrade {
		t.Error("frpc 的验收必须失败即 error，不许降级")
	}
	cfg := firstEnsureConfig(t, frpc)
	if cfg.PreserveExistingConfig {
		t.Error("frpc 的配置判据是面板 marker（用户可能改过），不该整份保留")
	}
	if !strings.Contains(cfg.Template, panelConfigMarker) {
		t.Error("frpc 的配置模板必须带面板 marker")
	}

	// ddns-go：9876、"存在即保留"（网页界面保存会抹掉 marker）。
	ddns, _ := FindDescriptor("ddns-go")
	if ddns.Health.Port != 9876 {
		t.Errorf("ddns-go 的健康端口应为 9876，实际 %d", ddns.Health.Port)
	}
	cfg = firstEnsureConfig(t, ddns)
	if !cfg.PreserveExistingConfig {
		t.Error("ddns-go 必须标 PreserveExistingConfig，否则重装会冲掉用户的 DNS 密钥")
	}
	if !strings.Contains(ddns.Uninstall.KeepNote, "API Token") {
		t.Errorf("ddns-go 的保留说明必须点名密钥在文件里：%q", ddns.Uninstall.KeepNote)
	}

	// orbien-client：不监听端口 → 判据必须是 launchd，而不是等 0 号端口。
	orb, _ := FindDescriptor("orbien-client")
	if orb.Health.Port != 0 {
		t.Errorf("orbien-client 不监听端口，实际 %d", orb.Health.Port)
	}
	if orb.Urls.ProxyURL != "" {
		t.Error("orbien-client 没有网页界面，不该声称有代理入口")
	}
	if !strings.Contains(orb.Urls.ProxyURLReason, "不监听任何端口") {
		t.Errorf("orbien-client 必须显式说明为什么没有代理入口：%q", orb.Urls.ProxyURLReason)
	}
	if !strings.Contains(orb.Uninstall.KeepNote, "token") {
		t.Errorf("orbien-client 的保留说明必须点名配置里可能有 token：%q", orb.Uninstall.KeepNote)
	}
}

func firstEnsureConfig(t *testing.T, d AppDescriptor) EnsureConfigAction {
	t.Helper()
	for _, s := range d.Steps {
		if a, ok := s.(EnsureConfigAction); ok {
			return a
		}
	}
	t.Fatalf("%s 的描述符里没有 ensure_config 步骤", d.ID)
	return EnsureConfigAction{}
}

// ---------------------------------------------------------------------------
//  ③ 绑定地址决定广告地址
// ---------------------------------------------------------------------------

// TestURLAdvertisingFollowsBindAddress 是 urls 约定的核心规则：
// 只绑 127.0.0.1 的服务**不得**广告成 LAN 地址（点了打不开），
// 绑 0.0.0.0 的必须广告 LAN 地址。
func TestURLAdvertisingFollowsBindAddress(t *testing.T) {
	cases := []struct {
		name string
		u    Urls
		host string
		port int
		want string
	}{
		{"只绑回环", Urls{BindAddress: "127.0.0.1", AdminPath: "/"}, "192.168.1.4", 7400,
			"http://127.0.0.1:7400/"},
		{"绑通配", Urls{BindAddress: "0.0.0.0", AdminPath: "/"}, "192.168.1.4", 9876,
			"http://192.168.1.4:9876/"},
		{"绑具体网卡地址", Urls{BindAddress: "192.168.1.4", AdminPath: "/x"}, "10.0.0.1", 80,
			"http://192.168.1.4:80/x"},
		{"看不懂的绑定写法一律不广告", Urls{BindAddress: "localhost", AdminPath: "/"}, "192.168.1.4", 80, ""},
		{"没有端口就没有地址", Urls{BindAddress: "0.0.0.0", AdminPath: "/"}, "192.168.1.4", 0, ""},
		{"没有声明绑定地址就不猜", Urls{AdminPath: "/"}, "192.168.1.4", 80, ""},
	}
	for _, tc := range cases {
		if got := tc.u.AdvertisedURL(tc.host, tc.port); got != tc.want {
			t.Errorf("%s：AdvertisedURL = %q，期望 %q", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
//  ④ DSL 执行器：下载 / 校验 / 解压 / 落盘
// ---------------------------------------------------------------------------

// mirrorTestDescriptor 造一个"产物带公网候选"的描述符。
//
// 刻意**不**把镜像地址写进 URL 列表：生产里镜像基址是执行期设置，
// 镜像候选由 MirrorPreflight 钩子在执行期注入（0.12.4 的 P0 正是
// 描述符里没有镜像、预检又只回了一个 bool，地址永远进不了候选表）。
func mirrorTestDescriptor() AppDescriptor {
	return AppDescriptor{
		ID: "fake", Name: "假应用", Rail: RailTarball, Kind: KindNative, Port: 7400,
		Paths: DescriptorPaths{RootDir: "fake", Binary: "faked", ConfigFile: "fake.toml"},
		Artifacts: []Artifact{{
			Name: "fake_1.0.0_darwin_arm64.tar.gz", Version: "v1.0.0",
			URLs: []string{
				"https://github.com/x/y/releases/download/v1.0.0/fake_1.0.0_darwin_arm64.tar.gz",
				"https://ghfast.top/https://github.com/x/y/releases/download/v1.0.0/fake_1.0.0_darwin_arm64.tar.gz",
			},
			Kind: ArtifactTarGz,
		}},
	}
}

// TestDownloadPrefersMirrorAndFallsBack 锁住"镜像优先、失败回落、并把
// 这次走的是不是镜像记下来（决定用哪套 sha256）"。
//
// 走 DownloadAction.Exec（而不是直接调 ec.download）：镜像地址只能由
// MirrorPreflight 钩子在执行期注入，直接调 download 会绕过这段接线 ——
// 那正是这个 P0 当年逃过测试的原因。
func TestDownloadPrefersMirrorAndFallsBack(t *testing.T) {
	d := mirrorTestDescriptor()
	attempts := 0
	f := &fakeRunner{
		runHook: func(name string, args []string) (string, error) {
			if name != "/usr/bin/curl" {
				return "", nil
			}
			attempts++
			if attempts == 1 {
				return "connection timed out", fmt.Errorf("exit status 28")
			}
			// 第二次（官方地址）成功：把产物写到 -o 指定的位置。
			dest := args[len(args)-2]
			if err := os.WriteFile(dest, []byte("payload"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		},
	}
	ec, _ := testExec(t, d, f)
	ec.MirrorBase = "http://mirror.local"
	// 钩子返回的地址就是描述符里没有的那一个：它必须出现在真实命令行里。
	mirrorURL := "http://mirror.local/apps/fake/v1.0.0/" + d.Artifacts[0].Name
	ec.MirrorPreflight = func(Artifact) string { return mirrorURL }

	if err := (DownloadAction{Artifact: d.Artifacts[0], MirrorPreflight: true}).Exec(ec); err != nil {
		t.Fatalf("镜像失败后应回落到官方地址：%v", err)
	}
	if attempts != 2 {
		t.Fatalf("应尝试两次（镜像 + 官方），实际 %d", attempts)
	}
	if ec.mirrorUsed(d.Artifacts[0].Name) {
		t.Error("这次是从官方地址下到的，mirrorUsed 不该为 true（它决定用镜像清单校验）")
	}
	if !strings.Contains(f.cmds[0], mirrorURL) {
		t.Errorf("镜像地址必须排第一且真的出现在 curl 命令行里：%s", f.cmds[0])
	}
	if strings.Contains(f.cmds[0], "github.com") {
		t.Errorf("首选命令不该是 GitHub —— 预检说镜像可用就必须打镜像：%s", f.cmds[0])
	}
	if !strings.Contains(f.cmds[1], "github.com") || !strings.Contains(f.cmds[1], "--max-time 150") {
		t.Errorf("备用地址应是官方地址且给 150 秒：%s", f.cmds[1])
	}
	steps := strings.Join(ec.Result.Steps, "\n")
	if !strings.Contains(steps, "下载失败，换下一个地址") {
		t.Errorf("回落必须如实写进任务日志：\n%s", steps)
	}
	if !strings.Contains(steps, "下载完成") {
		t.Errorf("成功必须写进任务日志：\n%s", steps)
	}
}

// TestDownloadStepUsesPreflightMirrorURLInRealCommand 是 P0 的**回归锁**：
//
// 真机（mini，2026-09-17）日志写着"镜像可用，将从镜像站下载"，实际执行的却
// 是 GitHub 命令。原因：MirrorPreflight 钩子的返回值被丢掉，镜像地址从未进入
// 候选列表。这条测试断言**真实 curl 命令行里的 URL**（不是日志文案），
// 并覆盖"镜像→官方→加速镜像"的完整回落阶梯与按来源给的截止时间。
func TestDownloadStepUsesPreflightMirrorURLInRealCommand(t *testing.T) {
	d := mirrorTestDescriptor()
	main := d.Artifacts[0]
	mirrorURL := "https://mirror.zizdog.com:8888/apps/fake/v1.0.0/" + main.Name

	seen := []string{}
	f := &fakeRunner{
		runHook: func(name string, args []string) (string, error) {
			if name != "/usr/bin/curl" {
				return "", nil
			}
			// -o 前一个参数是 URL；-o 后一个是落盘路径。
			u := args[len(args)-1]
			seen = append(seen, u)
			if u == mirrorURL {
				return "mirror down", fmt.Errorf("exit status 22")
			}
			if strings.HasPrefix(u, "https://github.com/") {
				return "github slow", fmt.Errorf("exit status 28")
			}
			// 最后是加速镜像：成功。
			dest := args[len(args)-2]
			return "", os.WriteFile(dest, []byte("payload"), 0o644)
		},
	}
	ec, _ := testExec(t, d, f)
	ec.MirrorBase = "https://mirror.zizdog.com:8888"
	ec.MirrorPreflight = func(Artifact) string { return mirrorURL }

	if err := (DownloadAction{Artifact: main, MirrorPreflight: true}).Exec(ec); err != nil {
		t.Fatalf("应逐级回落到加速镜像后成功：%v", err)
	}
	// ① 真实命令行必须按 镜像 → 官方 → 加速镜像 顺序尝试。
	if len(seen) != 3 {
		t.Fatalf("应尝试 3 个地址（镜像/官方/加速镜像），实际 %d：%v", len(seen), seen)
	}
	if seen[0] != mirrorURL {
		t.Errorf("第一条命令必须是镜像地址（预检说可用），实际 %s", seen[0])
	}
	if !strings.Contains(seen[1], "github.com/x/y") {
		t.Errorf("镜像失败后应回落官方地址，实际 %s", seen[1])
	}
	if !strings.Contains(seen[2], "ghfast.top") {
		t.Errorf("官方失败后应回落加速镜像，实际 %s", seen[2])
	}
	// ② 截止时间按来源：镜像/官方 150 秒，加速镜像 300 秒。
	if !strings.Contains(f.cmds[0], "--max-time 150") {
		t.Errorf("镜像站应给 150 秒：%s", f.cmds[0])
	}
	if !strings.Contains(f.cmds[1], "--max-time 150") {
		t.Errorf("官方地址应给 150 秒：%s", f.cmds[1])
	}
	if !strings.Contains(f.cmds[2], "--max-time 300") {
		t.Errorf("加速镜像应给 300 秒：%s", f.cmds[2])
	}
	// ③ 日志标签必须与实际地址一致：说"镜像站"的那一行必须写的是镜像主机，
	//    不许出现"镜像站 github.com"这类自相矛盾的日志。
	joinedSteps := strings.Join(ec.Result.Steps, "\n")
	if !strings.Contains(joinedSteps, "镜像站 mirror.zizdog.com:8888") {
		t.Errorf("任务日志里应有一条写明「镜像站 <镜像主机>」：\n%s", joinedSteps)
	}
	if strings.Contains(joinedSteps, "镜像站 github.com") {
		t.Errorf("日志把 GitHub 标成了镜像站（说镜像、走 GitHub 的翻版）：\n%s", joinedSteps)
	}
	// ④ 实际是从加速镜像下到的：mirrorUsed 必须为 false（不能用镜像清单校验）。
	if ec.mirrorUsed(main.Name) {
		t.Error("最终是从加速镜像下到的，mirrorUsed 不该为 true")
	}
}

// TestDownloadStepMarksMirrorUsedOnlyOnMirrorSuccess：镜像真的下成功时，
// mirrorUsed 必须为 true（后续 verify_sha256 据此改用镜像清单）。
func TestDownloadStepMarksMirrorUsedOnlyOnMirrorSuccess(t *testing.T) {
	d := mirrorTestDescriptor()
	main := d.Artifacts[0]
	mirrorURL := "https://mirror.example.com/apps/fake/" + main.Name
	f := &fakeRunner{
		runHook: func(name string, args []string) (string, error) {
			if name != "/usr/bin/curl" {
				return "", nil
			}
			dest := args[len(args)-2]
			return "", os.WriteFile(dest, []byte("payload"), 0o644)
		},
	}
	ec, _ := testExec(t, d, f)
	ec.MirrorBase = "https://mirror.example.com"
	ec.MirrorPreflight = func(Artifact) string { return mirrorURL }

	if err := (DownloadAction{Artifact: main, MirrorPreflight: true}).Exec(ec); err != nil {
		t.Fatalf("镜像可用时下载应成功：%v", err)
	}
	if !ec.mirrorUsed(main.Name) {
		t.Error("真的从镜像下到了，mirrorUsed 必须为 true（否则会去取上游清单）")
	}
}

// TestChecksumDownloadSkippedOnlyWhenMirrorUsed：上游校验清单只存在于 GitHub，
// 走镜像时 sha256 以镜像清单为准 —— 这一步必须跳过，否则"镜像优先"里又塞回
// 一次公网访问，GitHub 不可达时还会让整个安装失败。回落公网时必须照常下载。
func TestChecksumDownloadSkippedOnlyWhenMirrorUsed(t *testing.T) {
	d := mirrorTestDescriptor()
	main := d.Artifacts[0]
	checksum := Artifact{
		Name: "checksums.txt",
		URLs: []string{"https://github.com/x/y/releases/download/v1.0.0/checksums.txt"},
		Kind: ArtifactBinary,
	}
	mirrorURL := "https://mirror.example.com/apps/fake/" + main.Name

	cases := []struct {
		name        string
		mirrorURL   string
		curlMirror  bool
		wantCurls   int
		wantSkipped bool
	}{
		{"镜像可用时跳过上游清单", mirrorURL, true, 1, true},
		{"镜像不可用时照常下清单", "", false, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			curlCount := 0
			mirrorHits := 0
			f := &fakeRunner{runHook: func(name string, args []string) (string, error) {
				if name != "/usr/bin/curl" {
					return "", nil
				}
				curlCount++
				if args[len(args)-1] == mirrorURL {
					mirrorHits++
				}
				dest := args[len(args)-2]
				return "", os.WriteFile(dest, []byte("payload"), 0o644)
			}}
			ec, _ := testExec(t, d, f)
			ec.Spec.Steps = []InstallStep{
				DownloadAction{Artifact: main, MirrorPreflight: true},
				DownloadAction{Artifact: checksum, SkipWhenMirrorUsed: true},
			}
			if tc.mirrorURL != "" {
				ec.MirrorBase = "https://mirror.example.com"
			}
			// 预检钩子始终接线：返回空串 = 预检过、镜像不可用，回落公网。
			ec.MirrorPreflight = func(Artifact) string { return tc.mirrorURL }
			if err := ExecuteInstall(ec); err != nil {
				t.Fatalf("安装步骤应成功：%v", err)
			}
			if curlCount != tc.wantCurls {
				t.Errorf("curl 次数应为 %d，实际 %d（命令：%v）", tc.wantCurls, curlCount, f.cmds)
			}
			joined := strings.Join(ec.Result.Steps, "\n")
			skipped := strings.Contains(joined, "跳过 checksums.txt 的下载")
			if skipped != tc.wantSkipped {
				t.Errorf("是否跳过上游清单 = %v，期望 %v：\n%s", skipped, tc.wantSkipped, joined)
			}
			if tc.curlMirror && mirrorHits != 1 {
				t.Errorf("主产物应只从镜像下一次，实际镜像命中 %d 次", mirrorHits)
			}
		})
	}
}

// TestTarballOrchestratorInjectsMirrorURL 是 P0 的结构性锁（与行为测试互补）：
// 执行器的装配处必须（a）把镜像基址传进去（否则 isMirrorURL 永远 false、
// mirrorUsed 永远不成立）、（b）让预检钩子返回**地址**而不是 bool。
//
// 只记 bool 的写法（`ec.mirrorUsed = used`）必须彻底消失 —— 它正是
// "日志说走镜像、curl 实际打 GitHub"的根因，而且没有任何编译错误会提醒。
func TestTarballOrchestratorInjectsMirrorURL(t *testing.T) {
	src, err := os.ReadFile("tarball_descriptor.go")
	if err != nil {
		t.Fatalf("读不到 tarball_descriptor.go：%v", err)
	}
	text := string(src)
	for _, want := range []string{
		"MirrorBase: m.mirrorBase()",
		"return m.preflightMirrorAsset(ctx, spec, result)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("tarball 执行器装配处缺少 %q（镜像地址进不了候选列表）", want)
		}
	}
	if strings.Contains(text, "ec.mirrorUsed = ") {
		t.Error("预检钩子不许再只记 bool：镜像地址必须被插进候选列表")
	}
}

// TestDownloadUsesDiskArtifactWhenAllSourcesFail 锁住那条退路：
// 所有自动地址都失败时，磁盘上已有的产物照样能用 —— 否则错误信息里
// 那句"手动下载放到 <root>"就是一句空话。
func TestDownloadUsesDiskArtifactWhenAllSourcesFail(t *testing.T) {
	d := mirrorTestDescriptor()
	f := &fakeRunner{
		runHook: func(name string, args []string) (string, error) {
			if name == "/usr/bin/curl" {
				return "could not resolve host", fmt.Errorf("exit status 6")
			}
			return "", nil
		},
	}
	ec, root := testExec(t, d, f)
	// 用户按提示手动放好的产物。
	dest := filepath.Join(root, d.Artifacts[0].Name)
	if err := os.WriteFile(dest, []byte("manual"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ec.download(DownloadAction{Artifact: d.Artifacts[0]}, dest); err != nil {
		t.Fatalf("磁盘上已有产物时不该失败：%v", err)
	}
	if !strings.Contains(strings.Join(ec.Result.Steps, "\n"), "改用磁盘上已有的") {
		t.Errorf("用磁盘产物必须如实写进日志：%v", ec.Result.Steps)
	}
	// 磁盘上也没有时必须明确失败，并告诉他放哪里。
	dest2 := filepath.Join(root, "missing.tar.gz")
	err := ec.download(DownloadAction{Artifact: d.Artifacts[0]}, dest2)
	if err == nil {
		t.Fatal("没有产物时必须报错")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("错误信息必须告诉他手动放到哪：%v", err)
	}
}

// TestVerifySHA256RejectsMismatch 锁住内容校验的判定（坏文件真的会被拦住）。
func TestVerifySHA256RejectsMismatch(t *testing.T) {
	d := mirrorTestDescriptor()
	f := &fakeRunner{}
	ec, root := testExec(t, d, f)
	path := filepath.Join(root, d.Artifacts[0].Name)
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := sha256.Sum256([]byte("payload"))
	err := ec.verifySHA256(d.Artifacts[0], hex.EncodeToString(good[:]), "官方清单")
	if err == nil {
		t.Fatal("sha256 不一致时必须中止安装（静默跳过等于把校验变成看运气）")
	}
	if !strings.Contains(err.Error(), "SHA-256 校验不通过") {
		t.Errorf("错误信息应说清是校验失败：%v", err)
	}
	if err := ec.verifySHA256(d.Artifacts[0],
		hex.EncodeToString(sha256Sum([]byte("tampered"))), "官方清单"); err != nil {
		t.Fatalf("哈希一致时应通过：%v", err)
	}
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// TestExtractAssertsMemberExists 锁住"解压成功但什么都没解出来"这类最隐蔽的
// 失败：tar 退出码是 0，目录里却什么都没有，直到 launchd 报 "no such file"。
func TestExtractAssertsMemberExists(t *testing.T) {
	d := mirrorTestDescriptor()
	// 让 tar 假装成功，但**不**创建文件。
	f := &fakeRunner{runHook: func(name string, args []string) (string, error) {
		return "", nil
	}}
	ec, _ := testExec(t, d, f)
	err := ec.extract(ExtractAction{
		Artifact:   d.Artifacts[0],
		Dest:       "{root}",
		ExpectFile: "{root}/faked",
	})
	if err == nil {
		t.Fatal("解压后缺少可执行文件时必须报错，不能静默通过")
	}
	if !strings.Contains(err.Error(), "归档成员路径与描述符不一致") {
		t.Errorf("错误信息应指向成员路径不对：%v", err)
	}
}

// ---------------------------------------------------------------------------
//  ⑤ 端到端：新增一个假应用 = 只写一份描述符
// ---------------------------------------------------------------------------

// TestNewAppInstallsFromDescriptorAlone 是"模块化目标"的直接证明：
// 一个新的应用**不需要新写任何安装函数**，只要有一份描述符
// （rail=tarball + artifacts + steps），通用执行器就能把它从下载装到
// "登记进服务管理 + 验收通过"。
//
// 这个测试里的"下载"由 fakeRunner 在 t.TempDir() 里伪造（绝不联网），
// "注册 launchd"只被记录（绝不碰真实 launchctl）。
func TestNewAppInstallsFromDescriptorAlone(t *testing.T) {
	const id = "fake-app-modularity"
	archive := "fakeapp_9.9.9_darwin_arm64.tar.gz"
	sha := sha256.Sum256([]byte("archive-bytes"))

	// —— 唯一的接线点：一份描述符 ——
	d := AppDescriptor{
		ID: id, Name: "假应用（模块化证明）", Icon: "🧪", Category: "tool",
		Rail: RailTarball, Kind: KindNative, Port: 7788,
		Paths: DescriptorPaths{RootDir: "fakeapp", Binary: "fakeapp", ConfigFile: "fakeapp.toml"},
		Artifacts: []Artifact{{
			Name: archive, Version: "v9.9.9",
			URLs: []string{"https://example.invalid/" + archive},
			Kind: ArtifactTarGz, StripComponents: 1, ExtractMember: "fakeapp",
			Arm64: &Arm64Evidence{Evidence: "上游 release 资产", Source: "文件名 + file(1) 复核"},
		}},
		Service: ServiceSpec{
			Manager: "launchd-system", Label: "com.example.fakeapp",
			// plist 落在临时目录里：证明执行器的 plist 落盘走的是 Runner
			// （单测里是假的），绝不会真的碰 /Library/LaunchDaemons。
			PlistPath: filepath.Join(os.TempDir(), "zizpanel-descriptor-test", "com.example.fakeapp.plist"),
			RunAs:     "user", RequiresSudo: true,
		},
		Health: HealthSpec{Probe: ProbePort, Port: 7788, Timeout: time.Second},
		Urls: Urls{
			BindAddress: "0.0.0.0", AdminPath: "/",
			ProxyURLReason: "假应用没有声明子路径入口",
		},
		Uninstall: UninstallSpec{DataPaths: []string{"{root}"}, KeepNote: "默认保留安装目录"},
	}
	d.Steps = []InstallStep{
		EnsureDirAction{Path: "{root}", Mode: 0o755, Owner: "user"},
		DownloadAction{Artifact: d.Artifacts[0]},
		VerifySHA256Action{Artifact: d.Artifacts[0], Expect: hex.EncodeToString(sha[:]), Source: "测试清单"},
		ExtractAction{
			Artifact: d.Artifacts[0], Dest: "{root}",
			Member: "fakeapp", StripComponents: 1,
			ExpectFile: "{root}/fakeapp",
		},
		VerifyArm64Action{Path: "{root}/fakeapp"},
		ChmodAction{Path: "{root}/fakeapp", Mode: 0o755},
		EnsureConfigAction{Path: "{root}/fakeapp.toml",
			Template: "# " + panelConfigMarker + "\ntoken = \"{token}\"\n", Mode: 0o600},
		ChownAction{Path: "{root}", Owner: "user"},
		WritePlistAction{Args: []string{"{root}/fakeapp", "-c", "{root}/fakeapp.toml"}},
		LaunchdBootstrapAction{},
		RegisterServiceAction{},
		AssertReadyAction{},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("新应用的描述符自检失败：%v", err)
	}
	// 注册进桌面注册表（等价于加新应用时在 tarballDescriptors 里加一行），
	// 测试结束恢复，避免影响其它测试。
	if _, dup := descriptorsByID[id]; dup {
		t.Fatalf("%s 已存在，测试的临时 ID 撞车了", id)
	}
	registerDescriptor(d)
	defer func() {
		delete(descriptorsByID, id)
		for i, x := range descriptorOrder {
			if x == id {
				descriptorOrder = append(descriptorOrder[:i], descriptorOrder[i+1:]...)
				break
			}
		}
	}()

	// —— 执行 ——
	f := &fakeRunner{
		runHook: func(name string, args []string) (string, error) {
			switch name {
			case "/usr/bin/curl":
				// 伪造下载：把产物写到 -o 指定的路径。
				dest := args[len(args)-2]
				if err := os.WriteFile(dest, []byte("archive-bytes"), 0o644); err != nil {
					return "", err
				}
				return "", nil
			case "/usr/bin/tar":
				// 伪造解压：tar 说成功，并真的把二进制放到目标目录。
				// args: -xzf <asset> -C <dest> --strip-components=1 <member>
				var dest string
				for i, a := range args {
					if a == "-C" && i+1 < len(args) {
						dest = args[i+1]
					}
				}
				if dest == "" {
					return "", fmt.Errorf("tar 参数里没有 -C")
				}
				if err := os.MkdirAll(dest, 0o755); err != nil {
					return "", err
				}
				return "", os.WriteFile(filepath.Join(dest, "fakeapp"), []byte("#!/bin/sh\n"), 0o644)
			}
			return "", nil
		},
	}
	ec, root := testExec(t, d, f)
	ec.RegisterService = func(label, name, icon, category string, port int) error {
		if label != d.Service.Label || port != 7788 {
			return fmt.Errorf("登记参数不对：label=%q port=%d", label, port)
		}
		return nil
	}
	// 就绪判定走 package 注入点，绝不真的开端口/真的等 1 秒。
	stubReadyWaitPort(t, func(_ context.Context, port int, _ time.Duration) bool {
		return port == 7788
	})

	if err := ExecuteInstall(ec); err != nil {
		t.Fatalf("只靠描述符安装失败：%v", err)
	}

	// —— 断言：产物、权限、配置、plist、launchd、登记、就绪 ——
	binPath := filepath.Join(root, "fakeapp")
	fi, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("二进制没有落盘：%v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("二进制权限应为 0755，实际 %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(root, "fakeapp.toml")); err != nil {
		t.Errorf("配置文件没有写出来：%v", err)
	}
	if !hasBootstrap(f.bootstraps, d.Service.Label) {
		t.Errorf("没有注册 launchd 服务：%v", f.bootstraps)
	}
	if _, err := os.Stat("/Library/LaunchDaemons/com.example.fakeapp.plist"); err == nil {
		t.Fatalf("测试不该真的写出 /Library/LaunchDaemons/com.example.fakeapp.plist" +
			"（单测绝不允许碰 launchd 目录）")
	}
	b, err := os.ReadFile(d.Service.PlistPath)
	if err != nil {
		t.Errorf("plist 没有写出来：%v", err)
	} else {
		for _, want := range []string{
			"<string>" + d.Service.Label + "</string>",
			"<string>" + binPath + "</string>",
			"<string>tester</string>", // 以真实用户身份运行（不是 root）
			DefaultPATH,               // LaunchDaemon 里没有 Homebrew，必须注入 PATH
		} {
			if !strings.Contains(string(b), want) {
				t.Errorf("plist 缺少 %q：\n%s", want, b)
			}
		}
		if strings.Contains(string(b), "{root}") {
			t.Errorf("plist 里还留着未替换的 {root} 占位符：\n%s", b)
		}
	}
	if !readyStepsContain(ec.Result, "已就绪") {
		t.Errorf("验收没有写成功日志：%v", ec.Result.Steps)
	}
	// 凭据只允许出现在 InstallResult.Steps 的凭据区块里，不许混进步骤叙述。
	if !strings.Contains(strings.Join(ec.Result.Steps, "\n"), "凭据（面板随机生成，请自行保存）") {
		t.Errorf("配置里生成了 token，结果里应给出凭据区块：%v", ec.Result.Steps)
	}
	// 命令标签必须是从真实 args 派生的（这里断言 curl 真的被调用过，
	// 而不是"描述符说下载了就当作下载了"）。
	foundCurl := false
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "/usr/bin/curl ") {
			foundCurl = true
		}
	}
	if !foundCurl {
		t.Errorf("下载步骤没有真的执行 curl：%v", f.cmds)
	}
}

func hasBootstrap(list []string, label string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, label+"|") {
			return true
		}
	}
	return false
}

// TestFakeNewAppNeedsNoWebChange 是上一条的负向证明：**没有**为这个假应用
// 在任何地方注册 ID —— 它不在 releaseBinaryApps 里，也不在目录里，
// 但描述符执行器照样装得动它。这正是"以后加新应用不用一个一个针对性上架"。
func TestFakeNewAppNeedsNoWebChange(t *testing.T) {
	if _, ok := releaseBinaryApps["fake-app-modularity"]; ok {
		t.Fatal("假应用不该出现在 releaseBinaryApps 里（那说明它走的是另一条路）")
	}
	if _, ok := FindApp("fake-app-modularity"); ok {
		t.Fatal("假应用不该出现在应用目录里")
	}
	// 但"描述符 + 执行器"这条路是通的：GenericInstall 只需要一份描述符。
	if _, ok := FindDescriptor("fake-app-modularity"); ok {
		t.Fatal("上一个测试应当已经清理掉临时描述符")
	}
}

// ---------------------------------------------------------------------------
//  ⑥ 换轨不许留下两套实现
// ---------------------------------------------------------------------------

// TestReleaseBinaryIsThinDelegateOverDescriptor 是一条结构性断言：
// 老入口（InstallReleaseBinary / UninstallReleaseBinary / releaseBinaryPlan）
// 必须**只是壳**，安装/卸载流程只有描述符执行器那一份。
//
// 为什么要用读源码的方式钉死：这类"两套实现各说各话"的退化不会有编译错误，
// 表现是"改了描述符但真机行为没变"（或反过来），排查成本极高。
// 历史上就有过"注释说一套、代码做一套"的教训。
func TestReleaseBinaryIsThinDelegateOverDescriptor(t *testing.T) {
	src, err := os.ReadFile("binary_release.go")
	if err != nil {
		t.Fatalf("读不到 binary_release.go：%v", err)
	}
	text := string(src)
	// 老实现里那些"自己拼流程"的函数必须已经删掉（它们已被 steps DSL 取代）。
	for _, gone := range []string{
		"func (m *Manager) downloadReleaseBinary(",
		"func (m *Manager) verifyArm64Binary(",
		"func (m *Manager) verifyReleaseChecksum(",
		"func (m *Manager) orderDownloadURLs(",
	} {
		if strings.Contains(text, gone) {
			t.Errorf("binary_release.go 里还留着 %s —— 它与描述符执行器是两套实现，必须删掉", gone)
		}
	}
	// 老入口必须把活交给描述符/执行器。
	for _, want := range []string{
		"m.OrchestrateTarballInstall(ctx, d, result)",
		"m.UninstallByDescriptor(ctx, d, removeData, result)",
		"m.uninstallPlanForDescriptor(d)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("老入口应当委托给 %q（否则流程有了第二份实现）", want)
		}
	}
	// 描述符注册表必须由 tarball 轨列表驱动（加新应用只有一个接线点）。
	reg, err := os.ReadFile("tarball_descriptor.go")
	if err != nil {
		t.Fatalf("读不到 tarball_descriptor.go：%v", err)
	}
	if !strings.Contains(string(reg), "for _, id := range tarballDescriptors {") {
		t.Error("描述符注册必须遍历 tarballDescriptors（单一接线点）")
	}
	// 参数表必须由描述符回填展示字段，避免同一个事实写两遍。
	if !strings.Contains(string(reg), "releaseBinaryApps[id] = spec") {
		t.Error("releaseBinaryApps 的展示字段应由描述符回填（单一事实来源）")
	}
}

// TestPlistArgumentsAreNotPathJoined 锁住"plist 的参数不做路径拼接"。
//
// 真机事故（2026-09-17 Mac mini，Alist）：renderPlist 用 ec.path() 渲染每个参数，
// 于是 `server` 被拼成 `<root>/server`、`--data` 被拼成 `<root>/--data`；launchd
// 拉起后进程立刻退出，日志是
// `Error: unknown command "/Users/zizdog/alist/server" for "alist"`，端口从未监听。
//
// 这个坑此前没暴露，是因为 tarball 轨"写 plist"这一步从未在真机上跑过：
// frpc / ddns-go 的 plist 是旧安装器写的，而幂等闸门让它们没有被重写。
func TestPlistArgumentsAreNotPathJoined(t *testing.T) {
	spec, ok := releaseBinaryApps["alist"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 alist")
	}
	d, ok := FindDescriptor("alist")
	if !ok {
		t.Fatal("没有 alist 的描述符")
	}
	ec := &ExecConfig{
		Spec: d,
		pathVars: map[string]string{
			"{root}": "/Users/x/alist",
			"{home}": "/Users/x",
			"{user}": "x",
		},
	}
	body, err := ec.renderPlist(WritePlistAction{Args: plistArgs(spec)})
	if err != nil {
		t.Fatalf("渲染 plist 失败: %v", err)
	}
	for _, want := range []string{
		"<string>/Users/x/alist/alist</string>",
		"<string>server</string>",
		"<string>--data</string>",
		"<string>/Users/x/alist/data</string>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist 里应有 %q，实际：\n%s", want, body)
		}
	}
	for _, bad := range []string{"/Users/x/alist/server", "/Users/x/alist/--data"} {
		if strings.Contains(body, bad) {
			t.Errorf("plist 里不应出现被拼成路径的参数 %q：\n%s", bad, body)
		}
	}
}

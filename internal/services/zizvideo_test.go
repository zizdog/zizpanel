package services

// zizvideo_test.go —— zizvideo（面板托管模块）的门禁测试。
//
// 纪律（AGENTS 第三节）：单测绝不联网、绝不碰真实 launchd、绝不写 /opt 与真实家目录。
// 判据都是**行为**：installed 必须在"二进制缺失 / version 跑不起来 / 端口不健康"时为
// false；安装必须真的落盘 + 复核版本；卸载必须删该删的、保留该保留的（**媒体根永不进
// DataPaths**）并复核终态。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zizvideoTestEnv 造一个隔离的安装环境：临时安装根、临时家目录、假 launchd。
type zizvideoTestEnv struct {
	m      *Manager
	root   string
	plist  string
	panel  string
	source string
	launch []string
	stop   []string
}

func newZizvideoTestEnv(t *testing.T) *zizvideoTestEnv {
	t.Helper()
	m, _ := sandboxManager(t)
	env := &zizvideoTestEnv{m: m, root: t.TempDir()}

	prevRoot := ZizvideoInstallRoot
	prevLaunchdDir := SystemLaunchDaemonsDir
	prevLaunch, prevStop := zizvideoLaunch, zizvideoStop
	prevVer, prevGet := zizvideoVersionFn, zizvideoHTTPGet
	prevExec, prevSource, prevArm64 := zizvideoPanelExecutable, zizvideoSourceBin, zizvideoArm64Fn
	prevRemovedWait := zizvideoRemovedWait
	t.Cleanup(func() {
		ZizvideoInstallRoot = prevRoot
		SystemLaunchDaemonsDir = prevLaunchdDir
		zizvideoLaunch, zizvideoStop = prevLaunch, prevStop
		zizvideoVersionFn, zizvideoHTTPGet = prevVer, prevGet
		zizvideoPanelExecutable, zizvideoSourceBin, zizvideoArm64Fn = prevExec, prevSource, prevArm64
		zizvideoRemovedWait = prevRemovedWait
	})

	ZizvideoInstallRoot = env.root
	SystemLaunchDaemonsDir = t.TempDir()
	zizvideoRemovedWait = 400 * time.Millisecond

	dir := t.TempDir()
	env.panel = filepath.Join(dir, "zizpanel")
	if err := os.WriteFile(env.panel, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env.source = filepath.Join(dir, "zizvideo")
	if err := os.WriteFile(env.source, []byte("#!/bin/sh\necho 'zizvideo "+ZizvideoVersion+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	zizvideoPanelExecutable = func() (string, error) { return env.panel, nil }
	zizvideoSourceBin = func(string) string { return env.source }

	zizvideoLaunch = func(m *Manager, _ context.Context, label, plist string) error {
		env.launch = append(env.launch, label+"|"+plist)
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
	zizvideoStop = func(m *Manager, _ context.Context, label, plist string) error {
		env.stop = append(env.stop, label+"|"+plist)
		return removeAllPlists([]string{plist, filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")})
	}
	// 单测里的"二进制"是 shell 脚本，不可能真是 Mach-O arm64。
	zizvideoArm64Fn = func(*Manager, context.Context, string) error { return nil }
	zizvideoVersionFn = func(_ *Manager, _ context.Context, _ string) (string, error) {
		return "zizvideo " + ZizvideoVersion, nil
	}
	// 默认 HTTP 探针 = "没人在听"：就绪等待与卸载复核都不许真的联网。
	zizvideoHTTPGet = func(context.Context, string) (string, int, error) {
		return "", 0, errString("Connection refused")
	}
	p := m.ZizvideoPathsFor()
	env.plist = p.Plist
	return env
}

// stubHealth 把健康探测替换成固定响应。
func (e *zizvideoTestEnv) stubHealth(body string, code int, err error) {
	zizvideoHTTPGet = func(context.Context, string) (string, int, error) { return body, code, err }
}

// writeInstalledBin 在安装根写一个可执行的假 zizvideo。
func (e *zizvideoTestEnv) writeInstalledBin(t *testing.T) string {
	t.Helper()
	bin := ZizvideoBin()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// ---------------------------------------------------------------------------
//  installed 判据：必须是运行体证据
// ---------------------------------------------------------------------------

func TestZizvideoInstalledIsFalseWithoutBinary(t *testing.T) {
	env := newZizvideoTestEnv(t)
	// 僵尸 plist：判据不许因此说"已安装"（坑 161）。
	if err := os.MkdirAll(filepath.Dir(env.plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := env.m.ZizvideoBinaryInstalled(context.Background()); ok {
		t.Error("二进制不存在时必须判未安装（plist 不算证据）")
	}
}

func TestZizvideoInstalledFollowsHealth(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.writeInstalledBin(t)

	env.stubHealth("ok", 200, nil)
	if ok, detail := env.m.ZizvideoReady(context.Background()); !ok {
		t.Errorf("二进制 + version + /healthz ok 时应判就绪，实际：%s", detail)
	}
	env.stubHealth("down", 503, nil)
	if ok, _ := env.m.ZizvideoReady(context.Background()); ok {
		t.Error("/healthz 不是 200 时不许判就绪")
	}
	env.stubHealth("", 0, errString("Connection refused"))
	if ok, _ := env.m.ZizvideoReady(context.Background()); ok {
		t.Error("端口没人听时不许判就绪（端口被别的进程占着 ≠ 我们装好了）")
	}
}

// ---------------------------------------------------------------------------
//  托管形态：plist 参数与身份
// ---------------------------------------------------------------------------

func TestZizvideoSuperviseArgsArePanelBinaryAndRealUserPaths(t *testing.T) {
	env := newZizvideoTestEnv(t)
	p := env.m.ZizvideoPathsFor()
	args := ZizvideoSuperviseArgs(env.panel, env.m.opt.UserName, p)

	if args[0] != env.panel {
		t.Fatalf("ProgramArguments[0] 必须是面板自身的二进制（%s），实际 %q", env.panel, args[0])
	}
	if args[1] != "zizvideo-supervise" {
		t.Fatalf("ProgramArguments[1] 必须是 zizvideo-supervise，实际 %q", args[1])
	}
	if indexOfToken(args, "--user") < 0 {
		t.Fatal("必须显式传 --user（supervisor 据此降权）")
	}
	for _, kv := range []struct{ flag, want string }{
		{"--zizvideo", p.Bin},
		{"--config", p.ConfigPath},
		{"--home", p.Home},
		{"--log-dir", p.LogDir},
		{"--listen", "127.0.0.1:7766"},
	} {
		i := indexOfToken(args, kv.flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != kv.want {
			t.Errorf("%s 后面应是 %q，实际 %v", kv.flag, kv.want, args)
		}
	}
}

func TestZizvideoPlistIsSystemDaemonAndArgsAreExact(t *testing.T) {
	env := newZizvideoTestEnv(t)
	p := env.m.ZizvideoPathsFor()
	args := ZizvideoSuperviseArgs(env.panel, env.m.opt.UserName, p)
	content := zizvideoPlistContent(ZizvideoLabel, args, p.SuperviseOutLog, p.SuperviseErrLog)

	wantOrder := []string{
		"<string>" + ZizvideoLabel + "</string>",
		"<string>" + env.panel + "</string>",
		"<string>zizvideo-supervise</string>",
		"<string>--user</string>",
		"<string>" + env.m.opt.UserName + "</string>",
		"<string>--zizvideo</string>",
		"<string>" + p.Bin + "</string>",
		"<string>--config</string>",
		"<string>" + p.ConfigPath + "</string>",
		"<string>--home</string>",
		"<string>" + p.Home + "</string>",
		"<string>--listen</string>",
		"<string>127.0.0.1:7766</string>",
	}
	last := -1
	for _, want := range wantOrder {
		idx := strings.Index(content, want)
		if idx < 0 {
			t.Errorf("plist 缺少 %q：\n%s", want, content)
			continue
		}
		if idx <= last {
			t.Errorf("plist 里 %q 的位置不对（顺序与 ZizvideoSuperviseArgs 不一致）", want)
		}
		last = idx
	}
	if strings.Contains(content, "<key>UserName</key>") {
		t.Errorf("系统守护进程不该写 UserName（supervisor 需要 root 才能降权）：\n%s", content)
	}
	for _, want := range []string{
		"<key>RunAtLoad</key>\n    <true/>",
		"<key>KeepAlive</key>\n    <true/>",
		"<string>" + p.SuperviseErrLog + "</string>",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("plist 缺少 %q", want)
		}
	}
	if filepath.Dir(p.Plist) != SystemLaunchDaemonsDir {
		t.Errorf("系统 plist 必须在 %s 下，实际 %s", SystemLaunchDaemonsDir, p.Plist)
	}
	if _, err := os.Stat("/usr/bin/plutil"); err == nil {
		tmp := filepath.Join(t.TempDir(), ZizvideoLabel+".plist")
		if werr := os.WriteFile(tmp, []byte(content), 0o644); werr != nil {
			t.Fatal(werr)
		}
		if out, perr := runOutputErr("/usr/bin/plutil", "-lint", tmp); perr != nil {
			t.Errorf("plutil 拒绝这份 plist：%v（%s）", perr, out)
		}
	}
}

func TestZizvideoPathsFollowRealUserAndKeepMediaOut(t *testing.T) {
	env := newZizvideoTestEnv(t)
	p := env.m.ZizvideoPathsFor()
	if p.DataDir != filepath.Join(env.m.opt.UserHome, "Library", "Application Support", "zizvideo") {
		t.Errorf("数据目录必须在真实用户家目录下，实际 %s", p.DataDir)
	}
	if p.ConfigPath != filepath.Join(p.DataDir, "config.json") {
		t.Errorf("config.json 必须在数据目录下，实际 %s", p.ConfigPath)
	}
	if p.Bin != filepath.Join(env.root, "bin", "zizvideo") {
		t.Errorf("二进制必须在安装根下，实际 %s", p.Bin)
	}
	if ZizvideoServeArgs(p.ConfigPath)[0] != "--config" {
		t.Errorf("serve 参数必须以 --config 开头：%v", ZizvideoServeArgs(p.ConfigPath))
	}
}

// ---------------------------------------------------------------------------
//  安装
// ---------------------------------------------------------------------------

func TestInstallZizvideoSandboxed(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	p := env.m.ZizvideoPathsFor()

	res := &InstallResult{}
	if err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID, Name: "zizvideo"}, res); err != nil {
		t.Fatalf("安装应成功：%v", err)
	}
	if !fileExecutable(p.Bin) {
		t.Error("安装后二进制必须可执行")
	}
	if !fileExists(p.Plist) {
		t.Error("安装后必须写系统 plist")
	}
	if len(env.launch) != 1 {
		t.Fatalf("必须 bootstrap 一次，实际 %v", env.launch)
	}
	if !fileExists(p.ConfigPath) {
		t.Error("config.json 不存在时必须补一份，否则服务起不来")
	}
	if data, err := os.ReadFile(p.ConfigPath); err != nil || !strings.Contains(string(data), "{") {
		t.Errorf("补出的 config.json 应是合法 JSON 对象：%q err=%v", data, err)
	}
}

func TestInstallZizvideoIsIdempotent(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	ctx := context.Background()
	app := App{ID: ZizvideoAppID, Name: "zizvideo"}
	if err := env.m.InstallZizvideo(ctx, app, &InstallResult{}); err != nil {
		t.Fatalf("第一次安装失败：%v", err)
	}
	if err := env.m.InstallZizvideo(ctx, app, &InstallResult{}); err != nil {
		t.Fatalf("重复安装必须幂等成功：%v", err)
	}
	if !fileExecutable(ZizvideoBin()) {
		t.Error("重复安装后二进制仍应在")
	}
}

// TestInstallZizvideoKeepsExistingConfig：绝不覆盖用户已有配置（媒体根就在里面）。
func TestInstallZizvideoKeepsExistingConfig(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	p := env.m.ZizvideoPathsFor()
	media := filepath.Join(env.m.opt.UserHome, "Movies")
	original := `{"media_allow_roots":["` + media + `"],"listen":"127.0.0.1:7766"}`
	if err := os.MkdirAll(p.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID}, &InstallResult{}); err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	got, err := os.ReadFile(p.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("安装不许覆盖已有 config.json：\n原 %s\n现 %s", original, got)
	}
}

func TestInstallZizvideoFailsWhenHealthNeverComesUp(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("", 0, errString("Connection refused"))

	err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID}, &InstallResult{})
	if err == nil {
		t.Fatal("服务没就绪时必须如实报错，不许写个 plist 就算装好")
	}
	if !strings.Contains(err.Error(), "healthz") {
		t.Errorf("错误要点名健康端点，实际：%v", err)
	}
}

func TestInstallZizvideoRefusesWithoutSourceBinary(t *testing.T) {
	env := newZizvideoTestEnv(t)
	if err := os.Remove(env.source); err != nil {
		t.Fatal(err)
	}
	err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID}, &InstallResult{})
	if err == nil {
		t.Fatal("既没有本地二进制、又没有镜像时必须如实失败")
	}
	if !strings.Contains(err.Error(), "不再随面板包分发") || !strings.Contains(err.Error(), "镜像基址") {
		t.Errorf("错误要说清「不再随包、需要镜像」，实际：%v", err)
	}
}

func TestInstallZizvideoRejectsVersionMismatch(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	zizvideoVersionFn = func(_ *Manager, _ context.Context, _ string) (string, error) {
		return "zizvideo 9.9.9", nil
	}
	err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID}, &InstallResult{})
	if err == nil || !strings.Contains(err.Error(), ZizvideoVersion) {
		t.Fatalf("版本对不上必须中止安装并点名期望版本，实际：%v", err)
	}
	if fileExecutable(ZizvideoBin()) {
		t.Error("校验不过时绝不许把二进制写到安装根")
	}
}

// ---------------------------------------------------------------------------
//  卸载
// ---------------------------------------------------------------------------

func TestUninstallZizvideoRemovesPayloadKeepsData(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	ctx := context.Background()
	app := App{ID: ZizvideoAppID, Name: "zizvideo"}
	if err := env.m.InstallZizvideo(ctx, app, &InstallResult{}); err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	p := env.m.ZizvideoPathsFor()
	// 用户媒体根（与数据目录分离）：卸载绝不许动它。
	media := filepath.Join(env.m.opt.UserHome, "Movies")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	mediaFile := filepath.Join(media, "clip.mp4")
	if err := os.WriteFile(mediaFile, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 服务真的停了：健康探测改为没人听。
	env.stubHealth("", 0, errString("Connection refused"))

	if err := env.m.UninstallZizvideo(ctx, app, false, &InstallResult{}); err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	if fileExists(env.root) {
		t.Error("安装根必须删除干净")
	}
	if fileExists(p.Plist) {
		t.Error("系统 plist 必须删除干净")
	}
	if !fileExists(p.DataDir) {
		t.Error("默认必须保留数据目录（DB/封面/config.json）")
	}
	if !fileExists(mediaFile) {
		t.Error("媒体根是用户数据，任何情况下都不许动")
	}
}

func TestUninstallZizvideoRemoveDataDeletesDataNotMedia(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	ctx := context.Background()
	app := App{ID: ZizvideoAppID, Name: "zizvideo"}
	if err := env.m.InstallZizvideo(ctx, app, &InstallResult{}); err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	p := env.m.ZizvideoPathsFor()
	mediaFile := filepath.Join(env.m.opt.UserHome, "Movies", "clip.mp4")
	if err := os.MkdirAll(filepath.Dir(mediaFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaFile, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.stubHealth("", 0, errString("Connection refused"))

	if err := env.m.UninstallZizvideo(ctx, app, true, &InstallResult{}); err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	if fileExists(p.DataDir) {
		t.Error("勾选删除数据后数据目录必须消失")
	}
	if !fileExists(mediaFile) {
		t.Error("即使勾选删除数据，媒体文件也不许动")
	}
}

func TestUninstallZizvideoFailsWhenServiceStillServing(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	ctx := context.Background()
	app := App{ID: ZizvideoAppID, Name: "zizvideo"}
	if err := env.m.InstallZizvideo(ctx, app, &InstallResult{}); err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	// 健康探测始终 ok：服务其实没被停掉，卸载必须报错而不是谎报成功。
	err := env.m.UninstallZizvideo(ctx, app, false, &InstallResult{})
	if err == nil {
		t.Fatal("服务仍在应答时必须如实报错")
	}
}

// TestZizvideoUninstallPlanNeverListsMediaRoot：卸载计划里的 DataPaths 只许有数据目录。
func TestZizvideoUninstallPlanNeverListsMediaRoot(t *testing.T) {
	env := newZizvideoTestEnv(t)
	p := env.m.ZizvideoPathsFor()
	plan := env.m.zizvideoInstallPlan()
	if len(plan.DataPaths) != 1 || plan.DataPaths[0] != p.DataDir {
		t.Fatalf("DataPaths 只能有数据目录 %s，实际 %v", p.DataDir, plan.DataPaths)
	}
	for _, d := range plan.DataPaths {
		if strings.Contains(d, "Movies") || strings.Contains(d, "volumes") || strings.Contains(d, "Volumes") {
			t.Errorf("媒体根绝不许进 DataPaths：%v", plan.DataPaths)
		}
	}
}

// TestZizvideoUninstallPlanNamesDaemonPlistAndRoot：计划必须点名守护进程/plist/安装根。
func TestZizvideoUninstallPlanNamesDaemonPlistAndRoot(t *testing.T) {
	env := newZizvideoTestEnv(t)
	p := env.m.ZizvideoPathsFor()
	plan := env.m.zizvideoInstallPlan()
	if plan.Kind != "installer" {
		t.Fatalf("zizvideo 的卸载计划 kind 应为 installer，实际 %q", plan.Kind)
	}
	joined := strings.Join(plan.Steps, " | ")
	for _, want := range []string{ZizvideoLabel, p.Plist, p.Root} {
		if !strings.Contains(joined, want) {
			t.Errorf("卸载计划必须点名 %q，实际：%s", want, joined)
		}
	}
}

// TestZizvideoMarketEntryIsWired：上架市场的判据 —— 目录条目 + 市场声明 + 卸载实现三条齐。
func TestZizvideoMarketEntryIsWired(t *testing.T) {
	app, ok := FindApp(ZizvideoAppID)
	if !ok {
		t.Fatal("应用目录里没有 zizvideo 条目（市场里看不到它，也就装不了）")
	}
	if app.Kind != KindNative || app.PanelInstaller != ZizvideoAppID {
		t.Errorf("应是 KindNative + PanelInstaller=%s，实际 kind=%s installer=%q",
			ZizvideoAppID, app.Kind, app.PanelInstaller)
	}
	if app.ServiceLabel != ZizvideoLabel || app.Port != ZizvideoPort || app.HealthPath != zizvideoHealthPath {
		t.Errorf("服务契约不对：label=%q port=%d health=%q",
			app.ServiceLabel, app.Port, app.HealthPath)
	}
	if app.SystemDaemon {
		t.Error("zizvideo 自己写系统 plist，不该标 SystemDaemon（那个字段驱动 brew services 搬迁）")
	}
	if app.BrewFormula != "" {
		t.Errorf("这个条目不该有 BrewFormula（没有 brew 包）：%q", app.BrewFormula)
	}
	if !HasInstallerUninstall(app.PanelInstaller) {
		t.Error("有 PanelInstaller 却查不到卸载实现（装上就卸不掉）")
	}
	// 市场声明必须存在，且与目录逐字段一致 + 自身不变量成立（反漂移）。
	var decl *MarketApp
	for _, m := range MarketApps() {
		if m.ID == ZizvideoAppID {
			mm := m
			decl = &mm
			break
		}
	}
	if decl == nil {
		t.Fatal("market_downloads.go 里没有 zizvideo 的下载点声明")
	}
	if problems := MarketDeclarationProblems(*decl, app); len(problems) != 0 {
		t.Errorf("市场声明与目录/不变量不一致：%v", problems)
	}
	if len(decl.Downloads) != 1 {
		t.Fatalf("zizvideo 不随面板包分发：必须声明恰好一个镜像下载点，实际 %d 个", len(decl.Downloads))
	}
	d := decl.Downloads[0]
	if d.Purpose != MarketFetchReleaseBinary || !d.Required {
		t.Errorf("下载点应是必需（Required）的原生产物：purpose=%s required=%v", d.Purpose, d.Required)
	}
	wantAsset := ZizvideoArtifactName(ZizvideoVersion)
	if d.Upstream.Asset != wantAsset || !strings.Contains(d.Upstream.URL, "/apps/zizvideo/") {
		t.Errorf("下载点应指向镜像站 %s，实际 asset=%q url=%q", wantAsset, d.Upstream.Asset, d.Upstream.URL)
	}
	if d.Upstream.Repo != "" {
		t.Error("自研产物不是 GitHub release 产物，不该写 Repo")
	}
	if strings.TrimSpace(decl.NoDownloadReason) != "" {
		t.Errorf("有下载点就不该再写 NoDownloadReason：%q", decl.NoDownloadReason)
	}
}

// 不随包分发：本地没有携带位时，从镜像站按需下载、核 sha256 通过才安装。
func TestInstallZizvideoDownloadsFromMirrorWhenNotBundled(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	if err := os.Remove(env.source); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 没有 manifest.json ⇒ 用面板内置值兜底
	}))
	defer srv.Close()
	env.m.opt.MirrorBase = srv.URL

	payload := []byte("#!/bin/sh\necho ok\n")
	stub := filepath.Join(t.TempDir(), "zizvideo-dl")
	if err := os.WriteFile(stub, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	wantURL := srv.URL + "/apps/zizvideo/" + ZizvideoVersion + "/" + ZizvideoArtifactName(ZizvideoVersion)
	zizvideoFetch = func(_ *Manager, _ context.Context, url, dst string, _ *InstallResult) error {
		if url != wantURL {
			t.Errorf("下载地址不对：%q（期望 %q）", url, wantURL)
		}
		return os.WriteFile(dst, payload, 0o755)
	}
	zizvideoExpectedSHA256 = func() string { return sha256OfFile(stub) }
	t.Cleanup(func() {
		zizvideoFetch = defaultZizvideoFetch
		zizvideoExpectedSHA256 = defaultZizvideoExpectedSHA256
	})

	if err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID, Name: "zizvideo"}, &InstallResult{}); err != nil {
		t.Fatalf("应从镜像站取到二进制并装好，实际：%v", err)
	}
	if _, err := os.Stat(ZizvideoBin()); err != nil {
		t.Errorf("安装后应落盘 %s：%v", ZizvideoBin(), err)
	}
}

// 镜像上的包 sha256 与期望不符 ⇒ 拒绝安装，且不落安装位（不许把坏包装上去）。
func TestInstallZizvideoRejectsMirrorSHA256Mismatch(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	if err := os.Remove(env.source); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	env.m.opt.MirrorBase = srv.URL
	zizvideoFetch = func(_ *Manager, _ context.Context, _ string, dst string, _ *InstallResult) error {
		return os.WriteFile(dst, []byte("#!/bin/sh\necho tampered\n"), 0o755)
	}
	zizvideoExpectedSHA256 = func() string { return strings.Repeat("a", 64) }
	t.Cleanup(func() {
		zizvideoFetch = defaultZizvideoFetch
		zizvideoExpectedSHA256 = defaultZizvideoExpectedSHA256
	})

	err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID}, &InstallResult{})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("sha256 对不上必须拒绝安装，实际：%v", err)
	}
	if fileExecutable(ZizvideoBin()) {
		t.Error("坏包不该落到安装位")
	}
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

// indexOfToken 返回 token 在 args 里的下标（找不到 -1）。
func indexOfToken(args []string, token string) int {
	for i, a := range args {
		if a == token {
			return i
		}
	}
	return -1
}

// runOutputErr 跑一个命令并把输出与错误一起带回（测试用，只用于 plutil 校验）。
func runOutputErr(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// errString 是测试用的最小 error（不引额外依赖）。
type errString string

func (e errString) Error() string { return string(e) }

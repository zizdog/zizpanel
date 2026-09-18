package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  图片压缩（libvips）—— 网页界面 / 端口 / 别名的门禁
//
//  背景（2026-09-23 用户要求）："图片压缩（libvips）安装完没有反应！这个问题
//  解决一下。给它加一个 webui 通过端口和别名调用！"
//
//  这里锁住四件事，缺一条用户看到的就是"装了没用"：
//    ① 目录条目必须声明 UI.Slug（别名）与端口（appproxy 只挑有界面、有端口的）；
//    ② 安装器必须注册并启动一个 launchd 服务（plist 要真的执行子命令）；
//    ③ 卸载计划必须同时给出"停服务"与"brew uninstall"（installed ⇒ 有卸载路径）；
//    ④ 安装流程跑完必须真的登记进「服务管理」并等健康检查通过。
//
//  全部用注入点隔离：不碰真实 launchd、不碰真实 brew、不碰真实家目录。
// ============================================================================

// TestImgCompressCatalogDeclaresWebUIAndAlias 锁住目录条目上的"端口 + 别名"。
func TestImgCompressCatalogDeclaresWebUIAndAlias(t *testing.T) {
	app, ok := FindApp("imgcompress")
	if !ok {
		t.Fatal("应用市场里应有 imgcompress")
	}
	if app.UI == nil || strings.TrimSpace(app.UI.Slug) == "" {
		t.Fatal("必须声明 UI.Slug —— 没有它面板不会生成 /imgcompress/ 别名入口")
	}
	if app.UI.Slug != "imgcompress" {
		t.Errorf("别名应为 imgcompress（用户可见的地址就是 /imgcompress/），实际 %q", app.UI.Slug)
	}
	// appproxy.Slugs() 的挑选条件：UI 非空、非 SelfConf、端口 > 0。
	// 这里逐条断言，等价于"别名这条路真的会被生成"（跨包直接调会形成 import 环）。
	if app.UI.SelfConf {
		t.Error("图片压缩的别名由面板反代（不是安装器自己写 nginx），不该标 SelfConf")
	}
	if app.Port != ImgCompressPort || app.WebPort() != ImgCompressPort {
		t.Errorf("端口应为 %d，实际 Port=%d WebPort=%d", ImgCompressPort, app.Port, app.WebPort())
	}
	if app.HealthPath != "/healthz" {
		t.Errorf("健康检查路径应为 /healthz（服务必须能被判活/判红），实际 %q", app.HealthPath)
	}
	if app.ServiceLabel != ImgCompressLabel {
		t.Errorf("ServiceLabel 应为 %s，实际 %q", ImgCompressLabel, app.ServiceLabel)
	}
	if app.NoDaemon {
		t.Error("现在它有一个常驻的网页界面服务，不该再标 NoDaemon=true" +
			"（标了会被当成「没有守护进程」，服务记录与健康检查都会缺一块）")
	}
	if !strings.HasSuffix(app.RuntimePath, "/"+ImgCompressBinName) {
		t.Errorf("安装体判据仍应是 vips 可执行文件（与 ImgCompressBinName 同源），实际 %q", app.RuntimePath)
	}
	// 端口唯一性由 TestCatalogPortsAreUnique 全目录遍历锁住，这里只确认它是正数。
	if app.Port <= 0 {
		t.Errorf("端口必须为正，实际 %d", app.Port)
	}
}

// TestImgCompressPlistRunsPanelSubcommand 锁住 plist 的内容：
// 它必须执行**面板自己的二进制**的 imgcompress-serve 子命令，并带上监听地址与 brew 前缀。
func TestImgCompressPlistRunsPanelSubcommand(t *testing.T) {
	plist := imgCompressPlist("/opt/zizpanel/bin/zizpanel", ImgCompressPort,
		"/opt/homebrew", "zizdog", "/tmp/out.log", "/tmp/err.log")
	for _, want := range []string{
		"<string>" + ImgCompressLabel + "</string>",
		"<string>/opt/zizpanel/bin/zizpanel</string>",
		"<string>imgcompress-serve</string>",
		"<string>--listen</string>",
		"<string>127.0.0.1:8890</string>",
		"<string>--brew-prefix</string>",
		"<string>/opt/homebrew</string>",
		"<string>zizdog</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/tmp/out.log</string>",
		"<string>/tmp/err.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist 里缺少 %q\n---\n%s", want, plist)
		}
	}
	// 只绑回环：界面里能做的事（spawn vips、读上传的图）不该在用户没要求时暴露到局域网。
	if strings.Contains(plist, "0.0.0.0") {
		t.Error("网页界面服务不该绑 0.0.0.0（那会把它暴露到局域网）")
	}
}

// TestImgCompressUninstallPlanStopsServiceAndRemovesEngine 锁住卸载计划：
// 两个组成部分（launchd 服务 + brew 包）都必须出现在步骤里，且不许 blocked。
func TestImgCompressUninstallPlanStopsServiceAndRemovesEngine(t *testing.T) {
	m := matrixManager(t)
	app, _ := FindApp("imgcompress")
	plan := m.installerPlan(context.Background(), app)
	if plan.Kind != "installer" {
		t.Fatalf("应给 installer 计划，实际 %q", plan.Kind)
	}
	if plan.Blocked != "" {
		t.Fatalf("目录里的应用必须给得出卸载步骤，实际 blocked=%q", plan.Blocked)
	}
	joined := strings.Join(plan.Steps, "\n")
	if !strings.Contains(joined, ImgCompressLabel) {
		t.Errorf("计划里必须提到停掉 launchd 服务 %s（否则脚本升级后会留一个 KeepAlive 复活的僵尸）\n%s",
			ImgCompressLabel, joined)
	}
	if !strings.Contains(joined, "brew uninstall "+ImgCompressFormula) {
		t.Errorf("计划里必须有 `brew uninstall %s`（否则引擎留在机器上，卡片永远「已安装」）\n%s",
			ImgCompressFormula, joined)
	}
	if !HasInstallerUninstall("imgcompress") {
		t.Error("PanelInstaller=imgcompress 必须有卸载实现（有安装入口就必须有卸载入口）")
	}
}

// fakeImgCompressBrew 写一个假 brew：`list` 报已装、其它子命令记一笔并成功。
func fakeImgCompressBrew(t *testing.T, root string) (brewBin, marker string) {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(root, "brew-calls.log")
	brewBin = filepath.Join(binDir, "brew")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\n" +
		"case \"$1\" in\n  list) echo \"vips 8.18.6\"; exit 0 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(brewBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// 引擎二进制：`--version` 报版本，其它调用产出一个小文件。
	vips := filepath.Join(binDir, "vips")
	vipsScript := `#!/bin/sh
if [ "$1" = "--version" ]; then echo vips-8.18.6; exit 0; fi
out=""
for a in "$@"; do
  case "$a" in *.*) out="$a" ;; esac
done
out="${out%%[*}"
[ -n "$out" ] && printf 'x' > "$out"
exit 0
`
	if err := os.WriteFile(vips, []byte(vipsScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return brewBin, marker
}

// withImgCompressSeams 在测试期间替换安装/卸载用到的注入点，结束后恢复。
func withImgCompressSeams(t *testing.T, launch func(m *Manager, ctx context.Context, label, plist string) error,
	stop func(m *Manager, ctx context.Context, label, plist string) error) (plistPath *string, healthy *bool) {

	t.Helper()
	origPath, origLaunch, origStop := imgCompressPlistPath, imgCompressLaunch, imgCompressStop
	origHealthy, origExe := imgCompressHealthy, imgCompressExecutable
	path := filepath.Join(t.TempDir(), ImgCompressLabel+".plist")
	okHealthy := false
	imgCompressPlistPath = func() string { return path }
	imgCompressExecutable = func() (string, error) { return "/opt/zizpanel/bin/zizpanel", nil }
	imgCompressHealthy = func(context.Context, string, time.Duration) bool { return okHealthy }
	if launch != nil {
		imgCompressLaunch = launch
	}
	if stop != nil {
		imgCompressStop = stop
	}
	t.Cleanup(func() {
		imgCompressPlistPath, imgCompressLaunch, imgCompressStop = origPath, origLaunch, origStop
		imgCompressHealthy, imgCompressExecutable = origHealthy, origExe
	})
	return &path, &okHealthy
}

// TestImgCompressInstallRegistersService 走一遍**安装的收尾段**：
// 写 plist → launchctl（注入）→ 登记服务 → 等健康（注入）。
// 断言的正是用户要的三件事：服务真的注册了、plist 内容对、健康检查被复核过。
func TestImgCompressInstallRegistersService(t *testing.T) {
	m, repo := sandboxIdempotentManager(t)
	brewRoot := t.TempDir()
	brewBin, _ := fakeImgCompressBrew(t, brewRoot)
	m.opt.BrewBin = brewBin

	var launchedLabel, launchedPlist string
	launch := func(m *Manager, _ context.Context, label, plist string) error {
		launchedLabel, launchedPlist = label, plist
		// 模拟 launchd：把 plist 落到用户级目录，好让 AdoptCandidate 找得到它。
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
	plistPath, healthy := withImgCompressSeams(t, launch, nil)
	*healthy = true

	app, _ := FindApp("imgcompress")
	res := &InstallResult{App: app.ID, Steps: []string{}}
	if err := m.InstallImageCompressor(context.Background(), app, res); err != nil {
		t.Fatalf("安装（引擎已就绪的收尾段）不该失败：%v\n步骤：%v", err, res.Steps)
	}
	if launchedLabel != ImgCompressLabel {
		t.Fatalf("装载的 label 应为 %s，实际 %q", ImgCompressLabel, launchedLabel)
	}
	if launchedPlist != *plistPath {
		t.Errorf("装载的 plist 应为 %s，实际 %q", *plistPath, launchedPlist)
	}
	b, err := os.ReadFile(*plistPath)
	if err != nil {
		t.Fatalf("plist 没有落盘：%v", err)
	}
	if !strings.Contains(string(b), "imgcompress-serve") {
		t.Errorf("plist 里必须执行 imgcompress-serve 子命令：\n%s", b)
	}
	// 服务登记：installed 判定与「服务管理」都靠这条记录。
	list, err := repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var rec *Service
	for i := range list {
		if list[i].LaunchLabel == ImgCompressLabel {
			rec = list[i]
		}
	}
	if rec == nil {
		t.Fatal("装完必须自动登记进「服务管理」（否则状态/启停/日志都没有）")
	}
	if !strings.Contains(rec.HealthURL, "/healthz") {
		t.Errorf("服务记录的健康检查地址应指向 /healthz，实际 %q", rec.HealthURL)
	}
	if rec.Port != ImgCompressPort {
		t.Errorf("服务记录端口应为 %d，实际 %d", ImgCompressPort, rec.Port)
	}
}

// TestImgCompressUninstallStopsService 锁住卸载真的停服务并跑 brew uninstall。
func TestImgCompressUninstallStopsService(t *testing.T) {
	m, repo := sandboxIdempotentManager(t)
	brewRoot := t.TempDir()
	brewBin, brewLog := fakeImgCompressBrew(t, brewRoot)
	m.opt.BrewBin = brewBin

	// 先造一个"已安装"的现场：plist + 服务记录。
	stopped := false
	stop := func(m *Manager, ctx context.Context, label, plist string) error {
		stopped = true
		_ = os.Remove(plist)
		list, _ := m.repo.List(ctx)
		for _, s := range list {
			if s.LaunchLabel == label {
				_ = m.repo.Delete(ctx, s.Name)
			}
		}
		return nil
	}
	plistPath, _ := withImgCompressSeams(t, nil, stop)
	if err := os.WriteFile(*plistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(context.Background(), &Service{
		Name: NormalizeName(ImgCompressLabel), DisplayName: "图片压缩（libvips）",
		Kind: KindNative, LaunchLabel: ImgCompressLabel, PlistPath: *plistPath,
		HealthURL: ImgCompressHealthURL(), Port: ImgCompressPort, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	app, _ := FindApp("imgcompress")
	res := &InstallResult{App: app.ID, Steps: []string{}}
	if err := m.UninstallImageCompressor(context.Background(), app, res); err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	if !stopped {
		t.Error("卸载必须先停止并删除 launchd 服务（否则 KeepAlive 会让它复活）")
	}
	if fileExists(*plistPath) {
		t.Error("卸载后 plist 必须被删掉")
	}
	list, _ := repo.List(context.Background())
	for _, s := range list {
		if s.LaunchLabel == ImgCompressLabel {
			t.Errorf("卸载后不该还留着服务记录：%+v", s)
		}
	}
	logBytes, err := os.ReadFile(brewLog)
	if err != nil {
		t.Fatalf("假 brew 没有被调用（卸载没有真的卸包）：%v", err)
	}
	if !strings.Contains(string(logBytes), "uninstall vips") {
		t.Errorf("必须执行 `brew uninstall vips`，实际调用记录：\n%s", logBytes)
	}
}

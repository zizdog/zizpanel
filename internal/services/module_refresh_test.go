package services

// module_refresh_test.go —— "升级后刷新面板托管模块二进制"的门禁（坑 216）。
//
// 五条门禁对应真机报障：① 已安装且 sha 不同 ⇒ 替换 + 重启 + 成功记录；
// ② sha 相同 ⇒ 零动作（不重启）；③ 未安装 ⇒ 跳过且零副作用；
// ④ 自检失败 ⇒ 回滚旧二进制 + 失败记录；⑤ 模块数据目录零改动。
// 全部在 t.TempDir() 里跑，launchd 与自检都是注入的假实现（AGENTS 第三节）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type moduleRefreshEnv struct {
	panelDir string // 携带位所在目录（面板 BinDir）
	zRoot    string // zizvideo 安装根（临时）
	msRoot   string // macsaber 安装根（临时）
	daemons  string // 系统 plist 目录（临时）
}

func newModuleRefreshEnv(t *testing.T) *moduleRefreshEnv {
	t.Helper()
	prevZ, prevMS := ZizvideoInstallRoot, MacSaberInstallRoot
	prevDaemons := SystemLaunchDaemonsDir
	prevSelf := moduleSelfCheckRun
	prevZLaunch, prevMSLaunch := zizvideoLaunch, macSaberLaunch
	t.Cleanup(func() {
		ZizvideoInstallRoot, MacSaberInstallRoot = prevZ, prevMS
		SystemLaunchDaemonsDir = prevDaemons
		moduleSelfCheckRun = prevSelf
		zizvideoLaunch, macSaberLaunch = prevZLaunch, prevMSLaunch
	})

	env := &moduleRefreshEnv{
		panelDir: t.TempDir(),
		zRoot:    t.TempDir(),
		msRoot:   t.TempDir(),
		daemons:  t.TempDir(),
	}
	ZizvideoInstallRoot = env.zRoot
	MacSaberInstallRoot = env.msRoot
	SystemLaunchDaemonsDir = env.daemons
	return env
}

// moduleRefreshTestFiles 造出"已安装"的运行体：安装位二进制 + launchd plist。
func writeModuleFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func moduleRefreshResultFor(t *testing.T, results []ModuleRefreshResult, name string) ModuleRefreshResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("结果里没有模块 %s：%+v", name, results)
	return ModuleRefreshResult{}
}

// 门禁 ①：已安装且 sha 不同 ⇒ 备份 + 原子替换 + 自检 + 重启，并如实记录。
func TestModuleRefreshReplacesInstalledBinaryAndRestarts(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	plist := SystemDaemonPlistPath(ZizvideoLabel)
	bundled := filepath.Join(env.panelDir, ZizvideoAppID)

	writeModuleFile(t, installed, "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, plist, "<plist/>", 0o644)
	writeModuleFile(t, bundled, "#!/bin/bash\necho 'zizvideo new'\n", 0o755)
	oldSHA, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}
	newSHA, err := fileSHA256(bundled)
	if err != nil {
		t.Fatal(err)
	}

	var launched []string
	zizvideoLaunch = func(_ *Manager, _ context.Context, label, plist string) error {
		launched = append(launched, label+"|"+plist)
		return nil
	}
	var checked []string
	moduleSelfCheckRun = func(_ *Manager, _ context.Context, bin string, args ...string) (string, error) {
		checked = append(checked, bin+" "+strings.Join(args, " "))
		return "zizvideo 0.2.0", nil
	}

	results := RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleRefreshed {
		t.Fatalf("sha 不同时应刷新，实际 %s（%s）", r.Status, r.Reason)
	}
	if r.FromSHA != oldSHA || r.ToSHA != newSHA {
		t.Errorf("结果里的 sha 不对：from=%s to=%s", r.FromSHA, r.ToSHA)
	}
	if got := readFileOrFail(t, installed); !strings.Contains(got, "zizvideo new") {
		t.Errorf("安装位没有被替换成携带位二进制：%q", got)
	}
	if got := readFileOrFail(t, installed+".bak"); !strings.Contains(got, "zizvideo old") {
		t.Errorf("旧二进制没有备份：%q", got)
	}
	if len(checked) != 1 || !strings.HasPrefix(checked[0], installed+" --version") {
		t.Errorf("应在安装位跑一次 --version 自检，实际 %v", checked)
	}
	if len(launched) != 1 || !strings.HasPrefix(launched[0], ZizvideoLabel) {
		t.Errorf("通过自检后应重启守护进程 %s，实际 %v", ZizvideoLabel, launched)
	}
}

// 门禁 ②：sha256 相同 ⇒ 零动作（尤其**不许重启**，否则每次升级都抖动一次服务）。
func TestModuleRefreshIsNoOpWhenBundledMatchesInstalled(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	plist := SystemDaemonPlistPath(ZizvideoLabel)
	bundled := filepath.Join(env.panelDir, ZizvideoAppID)
	body := "#!/bin/bash\necho 'zizvideo same'\n"

	writeModuleFile(t, installed, body, 0o755)
	writeModuleFile(t, plist, "<plist/>", 0o644)
	writeModuleFile(t, bundled, body, 0o755)

	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}
	moduleSelfCheckRun = func(*Manager, context.Context, string, ...string) (string, error) {
		t.Error("sha 相同时不该跑自检")
		return "", nil
	}

	results := RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleUpToDate {
		t.Fatalf("sha 相同时状态应为 up-to-date，实际 %s（%s）", r.Status, r.Reason)
	}
	if launched {
		t.Error("sha 相同时不该重启守护进程")
	}
	if _, err := os.Stat(installed + ".bak"); err == nil {
		t.Error("sha 相同时不该产生备份")
	}
}

// 门禁 ③：未安装 ⇒ 跳过且不产生任何副作用（不许顺手安装）。
func TestModuleRefreshSkipsWhenNotInstalled(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	bundled := filepath.Join(env.panelDir, ZizvideoAppID)
	writeModuleFile(t, bundled, "#!/bin/bash\necho 'zizvideo new'\n", 0o755)

	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}
	moduleSelfCheckRun = func(*Manager, context.Context, string, ...string) (string, error) {
		t.Error("未安装时不该跑自检")
		return "", nil
	}

	results := RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleSkipped {
		t.Fatalf("未安装时应跳过，实际 %s（%s）", r.Status, r.Reason)
	}
	if launched {
		t.Error("未安装时不该重启（也不该安装）")
	}
	if _, err := os.Stat(installed); err == nil {
		t.Error("未安装时不该把携带位二进制装到安装位")
	}
	if _, err := os.Stat(installed + ".bak"); err == nil {
		t.Error("未安装时不该产生备份")
	}
}

// 只有二进制、没有 launchd 作业 ⇒ 也算未安装（判据贴运行体），照样跳过。
func TestModuleRefreshSkipsWhenDaemonMissing(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	writeModuleFile(t, installed, "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, filepath.Join(env.panelDir, ZizvideoAppID), "#!/bin/bash\necho new\n", 0o755)

	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}
	results := RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleSkipped {
		t.Fatalf("没有守护进程时应跳过，实际 %s（%s）", r.Status, r.Reason)
	}
	if launched {
		t.Error("没有守护进程时不该重启")
	}
	if got := readFileOrFail(t, installed); !strings.Contains(got, "old") {
		t.Errorf("跳过后安装位不该被改动：%q", got)
	}
}

// 门禁 ④：自检失败 ⇒ 回滚到旧二进制 + 失败记录（不许把模块留在起不来的状态）。
func TestModuleRefreshRollsBackWhenSelfCheckFails(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	plist := SystemDaemonPlistPath(ZizvideoLabel)
	writeModuleFile(t, installed, "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, plist, "<plist/>", 0o644)
	writeModuleFile(t, filepath.Join(env.panelDir, ZizvideoAppID), "#!/bin/bash\nexit 1\n", 0o755)

	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}
	moduleSelfCheckRun = func(_ *Manager, _ context.Context, bin string, _ ...string) (string, error) {
		return "", context.DeadlineExceeded
	}

	results := RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleRolledBack {
		t.Fatalf("自检失败应回滚，实际 %s（%s）", r.Status, r.Reason)
	}
	if got := readFileOrFail(t, installed); !strings.Contains(got, "zizvideo old") {
		t.Errorf("自检失败后必须回滚旧二进制，实际 %q", got)
	}
	if launched {
		t.Error("自检失败不该重启守护进程")
	}
}

// 门禁 ⑤：模块数据目录（DB/config/封面/媒体）零改动。用带 UserHome 的 Manager
// 让 DataDir 是真实算出来的路径，前后指纹必须逐字节一致。
func TestModuleRefreshNeverTouchesModuleDataDir(t *testing.T) {
	env := newModuleRefreshEnv(t)
	home := t.TempDir()
	m := NewManager(nil, Options{UserHome: home, UserName: "tester"})
	p := m.ZizvideoPathsFor()
	if p.DataDir == "" || !strings.HasPrefix(p.DataDir, home) {
		t.Fatalf("测试前置不成立：DataDir=%q", p.DataDir)
	}
	writeModuleFile(t, filepath.Join(p.DataDir, "zizvideo.db"), "DB-BYTES", 0o600)
	writeModuleFile(t, filepath.Join(p.DataDir, "config.json"), "{\"media_allow_roots\":[\"/media\"]}", 0o600)
	writeModuleFile(t, filepath.Join(p.DataDir, "covers", "a.jpg"), "COVER", 0o600)
	writeModuleFile(t, filepath.Join(p.DataDir, "media", "v.mp4"), "MEDIA", 0o600)

	writeModuleFile(t, p.Bin, "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, p.Plist, "<plist/>", 0o644)
	writeModuleFile(t, filepath.Join(env.panelDir, ZizvideoAppID), "#!/bin/bash\necho new\n", 0o755)
	zizvideoLaunch = func(*Manager, context.Context, string, string) error { return nil }
	moduleSelfCheckRun = func(*Manager, context.Context, string, ...string) (string, error) {
		return "zizvideo 0.2.0", nil
	}

	before := fingerprintTree(t, p.DataDir)
	results := m.RefreshInstalledModules(context.Background(), env.panelDir)
	after := fingerprintTree(t, p.DataDir)
	if r := moduleRefreshResultFor(t, results, ZizvideoAppID); r.Status != ModuleRefreshed {
		t.Fatalf("前置不成立：模块应被刷新，实际 %s（%s）", r.Status, r.Reason)
	}
	if before != after {
		t.Errorf("模块数据目录被改动了：\nbefore=%s\nafter=%s", before, after)
	}
}

// macsaber（已冻结但已安装）走同一套刷新逻辑，不写第二份。
func TestModuleRefreshAppliesToMacSaberViaSharedLogic(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := MacSaberBin()
	plist := SystemDaemonPlistPath(MacSaberLabel)
	writeModuleFile(t, installed, "#!/bin/bash\necho 'macsaber old'\n", 0o755)
	writeModuleFile(t, plist, "<plist/>", 0o644)
	writeModuleFile(t, filepath.Join(env.panelDir, MacSaberAppID), "#!/bin/bash\necho new\n", 0o755)

	var checked []string
	moduleSelfCheckRun = func(_ *Manager, _ context.Context, bin string, args ...string) (string, error) {
		checked = append(checked, bin+" "+strings.Join(args, " "))
		return "macsaber v0.2.0", nil
	}
	var launched []string
	macSaberLaunch = func(_ *Manager, _ context.Context, label, plist string) error {
		launched = append(launched, label)
		return nil
	}

	results := RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, MacSaberAppID)
	if r.Status != ModuleRefreshed {
		t.Fatalf("macsaber 应被刷新，实际 %s（%s）", r.Status, r.Reason)
	}
	if len(checked) != 1 || !strings.HasPrefix(checked[0], installed+" version") {
		t.Errorf("macsaber 自检命令应是 `version`，实际 %v", checked)
	}
	if len(launched) != 1 || launched[0] != MacSaberLabel {
		t.Errorf("应重启 %s，实际 %v", MacSaberLabel, launched)
	}
	// zizvideo 未安装（本环境没造它），应作为 skipped 一并如实回报。
	if z := moduleRefreshResultFor(t, results, ZizvideoAppID); z.Status != ModuleSkipped {
		t.Errorf("zizvideo 未安装应 skipped，实际 %s", z.Status)
	}
}

// 发布包没携带某个模块 ⇒ 跳过并说明，不报"刷新失败"也不去别处找。
func TestModuleRefreshSkipsWhenBundledMissing(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	writeModuleFile(t, installed, "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, SystemDaemonPlistPath(ZizvideoLabel), "<plist/>", 0o644)

	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}
	results := RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleSkipped {
		t.Fatalf("携带位缺失应跳过，实际 %s（%s）", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "未携带") {
		t.Errorf("跳过原因应说明发布包未携带，实际 %q", r.Reason)
	}
	if launched {
		t.Error("携带位缺失时不该重启")
	}
	if got := readFileOrFail(t, installed); !strings.Contains(got, "old") {
		t.Errorf("跳过后安装位不该被改动：%q", got)
	}
}

// fingerprintTree 给一棵目录树拍"路径+大小+sha256"指纹（文件内容级）。
func fingerprintTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if info.IsDir() {
			b.WriteString("D " + rel + "\n")
			return nil
		}
		sum, herr := fileSHA256(path)
		if herr != nil {
			return herr
		}
		b.WriteString("F " + rel + " " + sum + "\n")
		return nil
	})
	if err != nil {
		t.Fatalf("指纹 %s 失败: %v", root, err)
	}
	return b.String()
}

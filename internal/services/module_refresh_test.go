package services

// module_refresh_test.go —— "升级后刷新面板托管模块二进制"的门禁（坑 216）。
//
// 五条门禁对应真机报障：① 已安装且 sha 不同 ⇒ 替换 + 重启 + 成功记录；
// ② sha 相同 ⇒ 零动作（不重启）；③ 未安装 ⇒ 跳过且零副作用；
// ④ 自检失败 ⇒ 回滚旧二进制 + 失败记录；⑤ 模块数据目录零改动。
// 全部在 t.TempDir() 里跑，launchd 与自检都是注入的假实现（AGENTS 第三节）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type moduleRefreshEnv struct {
	panelDir string // 携带位所在目录（面板 BinDir）
	zRoot    string // zizvideo 安装根（临时）
	daemons  string // 系统 plist 目录（临时）
}

func newModuleRefreshEnv(t *testing.T) *moduleRefreshEnv {
	t.Helper()
	prevZ := ZizvideoInstallRoot
	prevDaemons := SystemLaunchDaemonsDir
	prevSelf := moduleSelfCheckRun
	prevZLaunch := zizvideoLaunch
	t.Cleanup(func() {
		ZizvideoInstallRoot = prevZ
		SystemLaunchDaemonsDir = prevDaemons
		moduleSelfCheckRun = prevSelf
		zizvideoLaunch = prevZLaunch
	})

	env := &moduleRefreshEnv{
		panelDir: t.TempDir(),
		zRoot:    t.TempDir(),
		daemons:  t.TempDir(),
	}
	ZizvideoInstallRoot = env.zRoot
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

// zizvideoIndexServer 造一个只提供应用级索引的假镜像（latest 固定，sha 是占位值）。
// 刷新路径只按"索引说的版本"决定是否替换，不会用到这个 sha。
func zizvideoIndexServer(t *testing.T, latest string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps/zizvideo/manifest.json" {
			http.NotFound(w, r)
			return
		}
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{
			"app": "zizvideo", "latest": latest,
			"assets": []map[string]any{{
				"name": "zizvideo_" + latest + "_darwin_arm64", "version": latest,
				"arch": "arm64", "sha256": strings.Repeat("0", 64), "size": 3,
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
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
		if strings.Contains(readFileOrFail(t, bin), "zizvideo new") {
			return "zizvideo " + ZizvideoVersion, nil
		}
		return "zizvideo 0.0.0-old", nil // 已装的是旧版本 ⇒ 该刷新
	}

	results := RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleRefreshed {
		t.Fatalf("版本不同时应刷新，实际 %s（%s）", r.Status, r.Reason)
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
	if len(checked) < 2 || !strings.HasPrefix(checked[len(checked)-1], installed+" --version") {
		t.Errorf("应在安装位跑 --version 自检（刷新前后各一次），实际 %v", checked)
	}
	if len(launched) != 1 || !strings.HasPrefix(launched[0], ZizvideoLabel) {
		t.Errorf("通过自检后应重启守护进程 %s，实际 %v", ZizvideoLabel, launched)
	}
}

// 门禁 ②：已装的就是这一版期望的版本 ⇒ 零动作（尤其**不许重启**，否则每次升级都抖动一次服务）。
func TestModuleRefreshIsNoOpWhenInstalledVersionMatches(t *testing.T) {
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
		return "zizvideo " + ZizvideoVersion, nil
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
	moduleSelfCheckRun = func(_ *Manager, _ context.Context, bin string, _ ...string) (string, error) {
		if strings.Contains(readFileOrFail(t, bin), "new") {
			return "zizvideo " + ZizvideoVersion, nil
		}
		return "zizvideo 0.0.0-old", nil
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

// 不随包分发的模块：索引可达、但按需取件失败要**如实失败**，且一个字节都不许改动安装位。
func TestModuleRefreshFailsHonestlyWhenFetchFails(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	writeModuleFile(t, installed, "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, SystemDaemonPlistPath(ZizvideoLabel), "<plist/>", 0o644)

	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}
	moduleSelfCheckRun = func(*Manager, context.Context, string, ...string) (string, error) {
		return "zizvideo 0.0.0-old", nil // 已装的是旧版本 ⇒ 触发取件
	}
	zizvideoModuleFetch = func(*Manager, context.Context, *InstallResult) (string, error) {
		return "", fmt.Errorf("镜像站访问不了")
	}
	t.Cleanup(func() { zizvideoModuleFetch = defaultZizvideoModuleFetch })

	// 索引说 0.2.0-index：解析成功 ⇒ 进入取件路径（取件失败必须如实报告）。
	m := NewManager(nil, Options{MirrorBase: zizvideoIndexServer(t, "0.2.0-index")})
	results := m.RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleFailed {
		t.Fatalf("取件失败应如实失败，实际 %s（%s）", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "取件失败") || !strings.Contains(r.Reason, "镜像站访问不了") {
		t.Errorf("失败原因要带上真正的原因，实际 %q", r.Reason)
	}
	if launched {
		t.Error("取件失败时不该重启守护进程")
	}
	if got := readFileOrFail(t, installed); !strings.Contains(got, "old") {
		t.Errorf("取件失败后安装位不该被改动：%q", got)
	}
}

// 索引不可用 + 没有携带位 ⇒ 如实失败，一个字节都不许动已装二进制（绝不谎报"已是最新"）。
func TestModuleRefreshFailsWhenIndexUnavailableAndNoBundle(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	writeModuleFile(t, installed, "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, SystemDaemonPlistPath(ZizvideoLabel), "<plist/>", 0o644)
	before, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}

	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}
	moduleSelfCheckRun = func(*Manager, context.Context, string, ...string) (string, error) {
		return "zizvideo 0.9.9", nil
	}
	// 没有配镜像基址 ⇒ 索引解析必然失败；携带位也不存在。
	m := NewManager(nil, Options{})
	results := m.RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleFailed {
		t.Fatalf("索引不可用且无携带位时必须如实失败，实际 %s（%s）", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "镜像索引不可用") || !strings.Contains(r.Reason, "未被改动") {
		t.Errorf("失败原因要说清是索引不可用且安装位未动，实际 %q", r.Reason)
	}
	if launched {
		t.Error("失败时不该重启守护进程")
	}
	after, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("安装位二进制被改动了：before=%s after=%s", before, after)
	}
	if _, err := os.Stat(installed + ".bak"); err == nil {
		t.Error("失败时不该产生备份")
	}
}

// 不随包分发的模块：已装版本不是这一版期望的 ⇒ 从镜像取件、替换、自检、重启。
func TestModuleRefreshFetchesWhenNotBundled(t *testing.T) {
	env := newModuleRefreshEnv(t)
	installed := ZizvideoBin()
	writeModuleFile(t, installed, "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, SystemDaemonPlistPath(ZizvideoLabel), "<plist/>", 0o644)

	fetched := filepath.Join(t.TempDir(), "zizvideo")
	writeModuleFile(t, fetched, "#!/bin/bash\necho 'zizvideo new'\n", 0o755)

	var launched []string
	zizvideoLaunch = func(_ *Manager, _ context.Context, label, plist string) error {
		launched = append(launched, label)
		return nil
	}
	moduleSelfCheckRun = func(_ *Manager, _ context.Context, bin string, _ ...string) (string, error) {
		if strings.Contains(readFileOrFail(t, bin), "zizvideo new") {
			return "zizvideo " + ZizvideoVersion, nil
		}
		return "zizvideo 0.0.0-old", nil
	}
	zizvideoModuleFetch = func(*Manager, context.Context, *InstallResult) (string, error) {
		return fetched, nil
	}
	t.Cleanup(func() { zizvideoModuleFetch = defaultZizvideoModuleFetch })

	// 索引版本 = 面板内置版本：自检桩对新二进制返回这个版本 ⇒ 应判刷新成功。
	m := NewManager(nil, Options{MirrorBase: zizvideoIndexServer(t, ZizvideoVersion)})
	results := m.RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleRefreshed {
		t.Fatalf("应取件并刷新，实际 %s（%s）", r.Status, r.Reason)
	}
	if got := readFileOrFail(t, installed); !strings.Contains(got, "zizvideo new") {
		t.Errorf("安装位没有被替换成取到的二进制：%q", got)
	}
	if len(launched) != 1 || launched[0] != ZizvideoLabel {
		t.Errorf("应重启 %s，实际 %v", ZizvideoLabel, launched)
	}
}

// 已装的就是索引/内置期望的版本 ⇒ 不取件、零动作。
func TestModuleRefreshSkipsFetchWhenVersionMatches(t *testing.T) {
	env := newModuleRefreshEnv(t)
	writeModuleFile(t, ZizvideoBin(), "#!/bin/bash\necho 'zizvideo old'\n", 0o755)
	writeModuleFile(t, SystemDaemonPlistPath(ZizvideoLabel), "<plist/>", 0o644)
	moduleSelfCheckRun = func(*Manager, context.Context, string, ...string) (string, error) {
		return "zizvideo " + ZizvideoVersion, nil
	}
	fetched := false
	zizvideoModuleFetch = func(*Manager, context.Context, *InstallResult) (string, error) {
		fetched = true
		return "", fmt.Errorf("不该被调用")
	}
	t.Cleanup(func() { zizvideoModuleFetch = defaultZizvideoModuleFetch })

	m := NewManager(nil, Options{MirrorBase: zizvideoIndexServer(t, ZizvideoVersion)})
	results := m.RefreshInstalledModules(context.Background(), env.panelDir)
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleUpToDate {
		t.Fatalf("版本相同应零动作，实际 %s（%s）", r.Status, r.Reason)
	}
	if fetched {
		t.Error("版本相同却仍去取件了")
	}
}

// 不随包分发 + 没装（安装位或守护进程缺）⇒ 绝不顺手安装、也不去取件。
func TestModuleRefreshSkipsWhenNotBundledAndNotInstalled(t *testing.T) {
	env := newModuleRefreshEnv(t)
	if r := moduleRefreshResultFor(t, RefreshInstalledModules(context.Background(), env.panelDir), ZizvideoAppID); r.Status != ModuleSkipped {
		t.Fatalf("未安装应跳过，实际 %s（%s）", r.Status, r.Reason)
	}
	writeModuleFile(t, ZizvideoBin(), "#!/bin/bash\necho old\n", 0o755)
	if r := moduleRefreshResultFor(t, RefreshInstalledModules(context.Background(), env.panelDir), ZizvideoAppID); r.Status != ModuleSkipped {
		t.Fatalf("只有二进制没有守护进程也应跳过，实际 %s（%s）", r.Status, r.Reason)
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

package services

// systemdaemon_test.go —— 「必须常驻的服务装成系统级 LaunchDaemon」的单元测试。
//
// 测试纪律（AGENTS.md 第三节）：**绝不碰真实的 /Library/LaunchDaemons**、
// 不真的调 launchctl / brew。能纯函数测的就纯函数测；要执行的就换掉注入点。
// 真机行为（重启后真的起来）在 mini 上验，见 DEVELOPMENT.md 坑 130。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 假 launchctl：单测绝不允许调真实的 /bin/launchctl（那会动到开发机上的服务）。
// 与 internal/web/api_services_launchd_test.go 同一套注入方式（ZIZPANEL_LAUNCHCTL）。
const fakeLaunchctlLoaded = `#!/bin/sh
case "$1" in
  print) echo "state = running"; echo "pid = 4242"; echo "last exit code = 0"; exit 0 ;;
  *) exit 0 ;;
esac
`

const fakeLaunchctlMissing = `#!/bin/sh
echo "Could not find service $2 in domain" >&2
exit 113
`

func withFakeLaunchctl(t *testing.T, script string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "launchctl")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old, had := os.LookupEnv("ZIZPANEL_LAUNCHCTL")
	if err := os.Setenv("ZIZPANEL_LAUNCHCTL", p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("ZIZPANEL_LAUNCHCTL", old)
			return
		}
		_ = os.Unsetenv("ZIZPANEL_LAUNCHCTL")
	})
}

// withTempLaunchDaemons 把系统级 plist 目录指向临时目录，并还原注入点。
func withTempLaunchDaemons(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := SystemLaunchDaemonsDir
	SystemLaunchDaemonsDir = dir
	t.Cleanup(func() { SystemLaunchDaemonsDir = old })
	return dir
}

// stubSystemDaemonEnsure 让单测里的"系统化"这一步变成可控的成功/失败。
//
// 为什么必须注入而不是让它真跑：面板在单测里不是 root（真实环境是 root
// LaunchDaemon），真跑必然失败并产生一条告警，把"顺利路径不该有告警"的
// 断言弄红 —— 而那条告警在真机上本来就不会出现。
func stubSystemDaemonEnsure(t *testing.T, err error) {
	t.Helper()
	old := systemDaemonEnsureFn
	t.Cleanup(func() { systemDaemonEnsureFn = old })
	systemDaemonEnsureFn = func(m *Manager, ctx context.Context, app App, res *InstallResult) (string, string, error) {
		if err != nil {
			return "", "", err
		}
		label := m.appBrewLabel(ctx, app.BrewFormula)
		plist := SystemDaemonPlistPath(label)
		return label, plist, nil
	}
}

// TestSystemDaemonFlagMatchesJudgment 锁住"哪些应用必须常驻"的判断。
//
// 这条断言是给未来的自己看的：加一个新应用时如果忘了判断"面板的承诺是否
// 依赖它开机就在"，这里会立刻提示他去做决定（而不是默默装成用户级服务，
// 然后在无头机器上重启后失效）。
func TestSystemDaemonFlagMatchesJudgment(t *testing.T) {
	want := map[string]bool{
		// 必须常驻：数据库 / Web 服务 / 同步守护 / 推理后端 / 站点赖以运行的 PHP-FPM
		"postgresql17": true,
		"miniflux":     true,
		"syncthing":    true,
		"ollama":       true,
		"php81":        true,
		"php82":        true,
		"php83":        true,
		"php84":        true,
	}
	got := map[string]bool{}
	for _, app := range Catalog() {
		if app.SystemDaemon {
			got[app.ID] = true
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("「%s」必须开机就在，应标记 SystemDaemon", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("「%s」标记了 SystemDaemon，但它不在判断清单里 —— 要么删掉标记，"+
				"要么把它写进 systemdaemon.go 顶部的判断标准里", id)
		}
	}

	// 明确不该被系统化的：没有常驻进程 / 不是服务 / 已经是 root 系统级。
	for _, id := range []string{"ffmpeg", "phpmyadmin", "nginx", "mysql84", "typecho", "wordpress"} {
		app, ok := FindApp(id)
		if !ok {
			t.Fatalf("目录里找不到 %s", id)
		}
		if app.SystemDaemon {
			t.Errorf("「%s」不该标记 SystemDaemon（见 systemdaemon.go 的判断标准）", id)
		}
	}
}

// TestSystemDaemonNeededRequiresFormula 锁住前置条件。
func TestSystemDaemonNeededRequiresFormula(t *testing.T) {
	cases := []struct {
		name string
		app  App
		want bool
	}{
		{"正常常驻应用", App{Name: "A", SystemDaemon: true, BrewFormula: "a"}, true},
		{"没有 formula（走 release 二进制自己的系统 plist）", App{Name: "B", SystemDaemon: true}, false},
		{"没有常驻进程", App{Name: "C", SystemDaemon: true, NoDaemon: true, BrewFormula: "c"}, false},
		{"没标记", App{Name: "D", BrewFormula: "d"}, false},
	}
	for _, c := range cases {
		if got := systemDaemonNeeded(c.app); got != c.want {
			t.Errorf("%s: systemDaemonNeeded=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestEnsureSystemDaemonIsIdempotentWhenAlreadySystem 锁住幂等：
// 系统级 plist 已经在时不再走脚本（脚本会 bootout + bootstrap，等于白重启一次）。
func TestEnsureSystemDaemonIsIdempotentWhenAlreadySystem(t *testing.T) {
	dir := withTempLaunchDaemons(t)
	m, _ := sandboxIdempotentManager(t)
	app, ok := FindApp("ollama")
	if !ok {
		t.Fatal("目录里应有 ollama")
	}
	label := m.appBrewLabel(context.Background(), app.BrewFormula)
	plist := SystemDaemonPlistPath(label)
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 假 launchctl 报"已加载"，端口检查用 Port=0 跳过（真机那一步在 mini 上验）。
	withFakeLaunchctl(t, fakeLaunchctlLoaded)
	app.Port = 0
	res := &InstallResult{App: app.ID, Name: app.Name}
	gotLabel, gotPlist, err := m.ensureSystemDaemon(context.Background(), app, res)
	if err != nil {
		t.Fatalf("已是系统级时应直接通过，实际报错: %v", err)
	}
	if gotLabel != label || gotPlist != plist {
		t.Errorf("应返回真实 label/plist，实际 %q / %q（期望 %q / %q）", gotLabel, gotPlist, label, plist)
	}
	if filepath.Dir(gotPlist) != dir {
		t.Errorf("系统 plist 应落在 %s，实际 %s", dir, gotPlist)
	}
}

// TestEnsureSystemDaemonRefusesWithoutRoot 锁住"面板不是 root 时必须如实失败"。
//
// 真机上不 root 就写不了 /Library/LaunchDaemons；这时**不能**悄悄当成功 ——
// 用户会以为重启后会自动起来，实际不会。
func TestEnsureSystemDaemonRefusesWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 跑测试时这条分支不成立")
	}
	withTempLaunchDaemons(t)
	withFakeLaunchctl(t, fakeLaunchctlMissing)
	m, _ := sandboxIdempotentManager(t)
	app, ok := FindApp("postgresql17")
	if !ok {
		t.Fatal("目录里应有 postgresql17")
	}
	res := &InstallResult{App: app.ID, Name: app.Name}
	_, _, err := m.ensureSystemDaemon(context.Background(), app, res)
	if err == nil {
		t.Fatal("非 root 时必须报错，不能谎报系统化成功")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("错误信息要说清是权限问题，实际 %q", err.Error())
	}
}

// TestSystemDaemonUserPlistPath 锁住用户级 plist 的定位（迁移时靠它判断"要不要搬"）。
func TestSystemDaemonUserPlistPath(t *testing.T) {
	m, _ := sandboxIdempotentManager(t)
	got := m.systemDaemonUserPlist("sh.brew.ollama")
	want := filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", "sh.brew.ollama.plist")
	if got != want {
		t.Errorf("用户级 plist 路径=%q，期望 %q", got, want)
	}
}

// TestMigrateSystemDaemonsOnlyTouchesUserLevelPlists 锁住迁移的范围：
// 只搬"用户级 plist 还在、系统级还没有"的；没装的应用不能碰。
func TestMigrateSystemDaemonsOnlyTouchesUserLevelPlists(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下会真的执行系统化，这条单测只验证非 root 的跳过行为")
	}
	withTempLaunchDaemons(t)
	m, _ := sandboxIdempotentManager(t)

	// 只给 ollama 造一个用户级 plist，其余应用"没装"。
	label := m.appBrewLabel(context.Background(), "ollama")
	userPlist := m.systemDaemonUserPlist(label)
	if err := os.MkdirAll(filepath.Dir(userPlist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPlist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrated, errs := m.MigrateSystemDaemons(context.Background())
	if migrated != 0 {
		t.Errorf("非 root 时不该有成功项，实际 %d", migrated)
	}
	// 非 root：只应报"跳过迁移"这一条，而不是为每个没装的应用都报一句。
	if len(errs) != 1 || !strings.Contains(errs[0], "root") {
		t.Errorf("非 root 时应只报一条跳过原因，实际 %v", errs)
	}
}

// TestStopLaunchdServiceRemovesBothDomainsPlist 锁住卸载要两个位置都清。
//
// 系统化之前装的（或迁移失败的）服务会留着 ~/Library/LaunchAgents 的 plist，
// 只删 /Library/LaunchDaemons 那份的话，它会在重启后又回来。
func TestStopLaunchdServiceRemovesBothDomainsPlist(t *testing.T) {
	withTempLaunchDaemons(t)
	withFakeLaunchctl(t, fakeLaunchctlMissing)
	m, _ := sandboxIdempotentManager(t)
	label := "sh.brew.ollama"
	sysPlist := SystemDaemonPlistPath(label)
	userPlist := m.systemDaemonUserPlist(label)
	for _, p := range []string{sysPlist, userPlist} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.stopLaunchdService(context.Background(), label, userPlist); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	for _, p := range []string{sysPlist, userPlist} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s 应该被删掉（两个域都要清）", p)
		}
	}
}

// TestSystemDaemonExistingLabelFindsBothNameFamilies 锁住"两套命名都要认"。
//
// 真机事故（本机 2026-09-17，坑 133）：php@8.3 的系统域里跑的是
// `homebrew.mxcl.php@8.3`（脚本按 opt 目录里那份 plist 的 Label 写出来的），
// 而 `brew services info` 报的是 `sh.brew.php@8.3`。只认后者的结果是
// "脚本明明搬成功了，复核却说没生成"，同时把另一份用户级 agent 留在机器上
// —— 重启后两份实例抢同一个 socket。
func TestSystemDaemonExistingLabelFindsBothNameFamilies(t *testing.T) {
	dir := withTempLaunchDaemons(t)
	withFakeLaunchctl(t, fakeLaunchctlLoaded)
	m, _ := sandboxIdempotentManager(t)

	if got := m.systemDaemonExistingLabel(context.Background(), "php@8.3"); got != "" {
		t.Fatalf("还没装时不该找到系统级 label，实际 %q", got)
	}
	// 只放"另一套命名"的那份：必须仍然找得到。
	other := filepath.Join(dir, "homebrew.mxcl.php@8.3.plist")
	if err := os.WriteFile(other, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.systemDaemonExistingLabel(context.Background(), "php@8.3"); got != "homebrew.mxcl.php@8.3" {
		t.Errorf("应从磁盘上认出 homebrew.mxcl.php@8.3，实际 %q", got)
	}
	// 完全陌生的前缀也要能靠通配兜住（历史遗留前缀不会让迁移卡住）。
	weird := filepath.Join(dir, "cn.zizdog.php@8.3.plist")
	if err := os.WriteFile(weird, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.systemDaemonExistingLabel(context.Background(), "php@8.3"); got == "" {
		t.Error("通配兜底应能认出任意前缀的 <formula>.plist")
	}
}

// TestCleanupUserAgentsRemovesBothNameFamilies 锁住用户级 agent 的清理范围。
func TestCleanupUserAgentsRemovesBothNameFamilies(t *testing.T) {
	withTempLaunchDaemons(t)
	m, _ := sandboxIdempotentManager(t)
	for _, label := range []string{"homebrew.mxcl.php@8.3", "sh.brew.php@8.3"} {
		p := m.systemDaemonUserPlist(label)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.cleanupUserAgents(context.Background(), "php@8.3"); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	for _, label := range []string{"homebrew.mxcl.php@8.3", "sh.brew.php@8.3"} {
		if p := m.systemDaemonUserPlist(label); fileExists(p) {
			t.Errorf("用户级 plist %s 应被清掉（留着会在重启后起第二份实例）", p)
		}
	}
	// 别的服务不能被误删。
	other := m.systemDaemonUserPlist("sh.brew.ollama")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.cleanupUserAgents(context.Background(), "php@8.3"); err != nil {
		t.Fatal(err)
	}
	if !fileExists(other) {
		t.Error("清理 php@8.3 时不该动 ollama 的 agent")
	}
}

// TestSyncServiceRecordsAlignsStaleLabel 锁住"记录要对齐到真实 label"。
//
// 真机事故（mini 2026-09-17，坑 133）：迁移把服务装成了
// `homebrew.mxcl.php@8.1`，而面板记录里还是旧安装留下的 `sh.brew.php@8.1`
// （plist 指向已被删掉的 ~/Library/LaunchAgents 文件）—— 服务明明在跑，
// 界面上却是"找不到 plist 文件（服务可能已被移除）"。以磁盘为准回写记录。
func TestSyncServiceRecordsAlignsStaleLabel(t *testing.T) {
	dir := withTempLaunchDaemons(t)
	m, repo := sandboxIdempotentManager(t)
	ctx := context.Background()

	stale := &Service{
		Name: "php81", DisplayName: "PHP 8.1", Kind: KindNative,
		LaunchLabel: "sh.brew.php@8.1",
		PlistPath:   filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", "sh.brew.php@8.1.plist"),
		Port:        0, Enabled: true, Managed: true,
	}
	if err := repo.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}
	// 无关的记录不能被改动。
	other := &Service{
		Name: "ollama", DisplayName: "Ollama", Kind: KindNative,
		LaunchLabel: "sh.brew.ollama", PlistPath: "/tmp/sh.brew.ollama.plist",
		Enabled: true, Managed: true,
	}
	if err := repo.Create(ctx, other); err != nil {
		t.Fatal(err)
	}

	real := filepath.Join(dir, "homebrew.mxcl.php@8.1.plist")
	if err := os.WriteFile(real, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := m.syncServiceRecords(ctx, "php@8.1", "homebrew.mxcl.php@8.1", real); n != 1 {
		t.Fatalf("应只修 1 条记录，实际 %d", n)
	}
	got, err := repo.Get(ctx, "php81")
	if err != nil {
		t.Fatal(err)
	}
	if got.LaunchLabel != "homebrew.mxcl.php@8.1" || got.PlistPath != real {
		t.Errorf("记录未对齐：label=%q plist=%q", got.LaunchLabel, got.PlistPath)
	}
	otherGot, err := repo.Get(ctx, "ollama")
	if err != nil {
		t.Fatal(err)
	}
	if otherGot.LaunchLabel != "sh.brew.ollama" || otherGot.PlistPath != "/tmp/sh.brew.ollama.plist" {
		t.Errorf("不该改别的服务：label=%q plist=%q", otherGot.LaunchLabel, otherGot.PlistPath)
	}
}

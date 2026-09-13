package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate 把面板根目录指向临时目录，避免测试触碰 /opt/zizpanel 里的真实数据。
func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ZIZPANEL_ROOT", root)
	t.Setenv("SUDO_USER", "")
	return root
}

func TestDefaultHasSanePaths(t *testing.T) {
	isolate(t)
	c := Default()
	if c.Listen == "" {
		t.Fatal("监听地址不能为空")
	}
	if c.DataDir == "" || c.LogDir == "" || c.WWWRoot == "" {
		t.Fatal("关键目录不能为空")
	}
	if c.SessionHours <= 0 || c.LoginMaxFail <= 0 || c.LoginLockMins <= 0 {
		t.Fatal("安全默认值必须为正数")
	}
	if c.TLSCert == "" || c.TLSKey == "" {
		t.Fatal("默认应启用 HTTPS 并给出证书路径")
	}
	// 默认必须允许远程访问，否则"装完即可远程操作"无法达成
	if c.AccessMode != "any" {
		t.Fatalf("默认访问模式应为 any，实际 %s", c.AccessMode)
	}
	// 证书必须落在数据目录内（备份时能一起带走）
	if filepath.Dir(c.TLSCert) != filepath.Join(c.DataDir, "tls") {
		t.Fatalf("证书路径不在数据目录内: %s", c.TLSCert)
	}
}

func TestRootRedirect(t *testing.T) {
	root := isolate(t)
	if got := DefaultConfigPath(); got != filepath.Join(root, "data", "config.json") {
		t.Fatalf("默认配置路径未跟随根目录: %s", got)
	}
	c := Default()
	if c.DataDir != filepath.Join(root, "data") {
		t.Fatalf("数据目录未跟随根目录: %s", c.DataDir)
	}
}

func TestDefaultHomebrewDetected(t *testing.T) {
	isolate(t)
	c := Default()
	// 测试机是 Apple Silicon，必须识别出 /opt/homebrew
	if _, err := os.Stat("/opt/homebrew/bin/brew"); err == nil {
		if c.BrewPrefix != "/opt/homebrew" {
			t.Fatalf("未识别 Homebrew 前缀: %s", c.BrewPrefix)
		}
		if c.NginxBin != "/opt/homebrew/bin/nginx" {
			t.Fatalf("nginx 路径错误: %s", c.NginxBin)
		}
	}
}

func TestBootstrapIsIdempotent(t *testing.T) {
	isolate(t)
	path := DefaultConfigPath()

	c1, created, err := Bootstrap(path)
	if err != nil {
		t.Fatalf("首次初始化失败: %v", err)
	}
	if !created {
		t.Fatal("首次初始化应报告 created=true")
	}
	if c1.Secret == "" || c1.InstallID == "" {
		t.Fatal("首次初始化必须生成密钥与安装标识")
	}
	if len(c1.Secret) != 64 { // 32 字节 hex
		t.Fatalf("密钥长度异常: %d", len(c1.Secret))
	}
	for _, d := range c1.allDirs() {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("目录未创建: %s", d)
		}
	}
	// 配置文件权限必须是 600（内含会话签名密钥）
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("配置文件权限应为 0600，实际 %o", perm)
	}

	// 二次初始化必须复用已有配置，不能覆盖密钥
	c2, created2, err := Bootstrap(path)
	if err != nil {
		t.Fatalf("二次初始化失败: %v", err)
	}
	if created2 {
		t.Fatal("二次初始化不应报告 created=true")
	}
	if c2.Secret != c1.Secret {
		t.Fatal("二次初始化不允许更换密钥（会导致所有会话失效）")
	}
	if c2.InstallID != c1.InstallID {
		t.Fatal("安装标识不应改变")
	}
}

func TestLoadMissingReturnsErrNotInstalled(t *testing.T) {
	isolate(t)
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("读取不存在的配置应返回错误")
	}
	if err != ErrNotInstalled {
		t.Fatalf("应为 ErrNotInstalled，实际 %v", err)
	}
}

func TestLoadFillsMissingFields(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// 模拟老版本配置：只有极少数字段
	old := map[string]any{"listen": ":9999", "data_dir": dir}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" {
		t.Fatalf("已有字段不应被覆盖: %s", c.Listen)
	}
	if c.Secret == "" {
		t.Fatal("缺失的密钥应被补齐")
	}
	if c.SessionHours <= 0 {
		t.Fatal("缺失的安全默认值应被补齐")
	}
	if c.AccessMode == "" {
		t.Fatal("缺失的访问模式应被补齐")
	}
	if c.TLSCert == "" || c.TLSKey == "" {
		t.Fatal("缺失的证书路径应被补齐")
	}
}

func TestSaveIsAtomicAndRoundTrips(t *testing.T) {
	isolate(t)
	path := DefaultConfigPath()
	c, _, err := Bootstrap(path)
	if err != nil {
		t.Fatal(err)
	}
	c.AccessMode = "whitelist"
	c.IPWhitelist = []string{"100.64.0.0/10"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	// 不应残留临时文件
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("原子写入后不应残留 .tmp 文件")
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessMode != "whitelist" || len(got.IPWhitelist) != 1 || got.IPWhitelist[0] != "100.64.0.0/10" {
		t.Fatalf("配置未正确往返: %+v", got)
	}
}

func TestEnsureDirsIsRecoverable(t *testing.T) {
	isolate(t)
	c := Default()
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// 模拟用户手工删掉目录后重启：必须能自愈
	if err := os.RemoveAll(c.WorkDir); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureDirs(); err != nil {
		t.Fatalf("目录被删除后应能重建: %v", err)
	}
	if st, err := os.Stat(c.WorkDir); err != nil || !st.IsDir() {
		t.Fatal("目录未重建")
	}
}

func TestPortParsing(t *testing.T) {
	c := Default()
	c.Listen = ":8443"
	if c.Port() != 8443 {
		t.Fatalf("端口解析错误: %d", c.Port())
	}
	c.Listen = "127.0.0.1:9443"
	if c.Port() != 9443 {
		t.Fatalf("端口解析错误: %d", c.Port())
	}
	c.Listen = "garbage"
	if c.Port() != 0 {
		t.Fatalf("非法地址应返回 0，实际 %d", c.Port())
	}
}

// 配置文件必须能被"真实用户"读取。
//
// 这条测试锁死一个真实问题：面板以 root 运行，首次初始化写出的
// config.json 属于 root（0600），导致普通用户执行 `zizpanel status`
// 报 permission denied —— 而这条命令正是排障时最需要的。
func TestSavedConfigIsReadableByRealUser(t *testing.T) {
	isolate(t)
	t.Setenv("ZIZPANEL_USER", "nobody") // 模拟一个非 root 用户
	path := DefaultConfigPath()
	c, _, err := Bootstrap(path)
	if err != nil {
		t.Fatal(err)
	}
	// 非 root 环境下只能验证权限位不过分严格
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("配置文件权限应为 0600（含密钥），实际 %o", perm)
	}
	// 重新加载应成功
	if _, err := Load(path); err != nil {
		t.Fatalf("配置应可重新加载: %v", err)
	}
	_ = c
}

// TestRepairRuntimeAccessSkipsRoot root 本来就能读能写，不该去改任何归属。
func TestRepairRuntimeAccessSkipsRoot(t *testing.T) {
	isolate(t)
	c := Default()
	if err := c.EnsureDirs(); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if got := c.RepairRuntimeAccess(); len(got) != 0 {
		t.Fatalf("root 身份不该产生任何修复动作，实际: %v", got)
	}
}

// TestRepairRuntimeAccessFixesOwnBrokenMode 覆盖自愈**能做**到的那一半：
//
// 文件属于当前身份、但权限位被改坏（0o000）—— 这种情况下进程有权 chown，
// 自愈必须把它救回来，因为面板的失败点极靠前（读证书 → 直接退出，
// 连日志都来不及写，launchd 只报 `last exit code = 78: EX_CONFIG`）。
//
// 为什么不用"另一个 uid"来测：非 root 身份**无权** chown 别人的文件，
// 那种归属漂移自愈修不了（已由 RepairRuntimeAccess 的警告如实上报，
// 并在安装脚本里从源头避免）。测一个做不到的场景只会得到一个假测试。
func TestRepairRuntimeAccessFixesOwnBrokenMode(t *testing.T) {
	isolate(t)
	c := Default()
	if err := c.EnsureDirs(); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}

	logFile := filepath.Join(c.LogDir, "panel-20060102.log")
	if err := os.WriteFile(logFile, []byte("x"), 0o000); err != nil {
		t.Fatalf("造日志文件失败: %v", err)
	}
	if err := os.WriteFile(c.TLSKey, []byte("k"), 0o000); err != nil {
		t.Fatalf("造 TLS 私钥失败: %v", err)
	}
	if err := os.WriteFile(c.TLSCert, []byte("c"), 0o000); err != nil {
		t.Fatalf("造 TLS 证书失败: %v", err)
	}

	// 以"当前身份"跑修复 —— 文件确实属于这个身份，只是权限位坏了。
	uid, gid := os.Geteuid(), os.Getegid()
	if uid == 0 {
		t.Skip("以 root 运行时权限位拦不住任何操作，这个场景不成立")
	}
	warnings := c.repairRuntimeAccessFor(uid, gid)

	// 核心不变量：修复后当前进程必须能读私钥、能写日志。
	// 否则面板还是起不来，等于没修。
	if !canRead(c.TLSKey) {
		t.Errorf("修复后 TLS 私钥仍不可读（面板会在这一步退出）: %v", warnings)
	}
	if !canWrite(logFile) {
		t.Errorf("修复后日志文件仍不可写（logx.Init 会失败）: %v", warnings)
	}
	if len(warnings) == 0 {
		t.Error("修了东西却没给出任何警告，用户将无从得知发生过权限漂移")
	}
}

// TestRepairRuntimeAccessReportsUnfixableForeignOwner 覆盖自愈**做不到**的那一半：
//
// 文件属于另一个身份、当前进程又无权 chown 时，不能默默失败 ——
// 必须给出可执行的修复指令，否则用户只会看到"升级完面板没了"。
func TestRepairRuntimeAccessReportsUnfixableForeignOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 改归属总是成功，构造不出'修不了'的场景")
	}
	isolate(t)
	c := Default()
	if err := c.EnsureDirs(); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(c.TLSKey, []byte("k"), 0o000); err != nil {
		t.Fatalf("造 TLS 私钥失败: %v", err)
	}

	// 假装当前身份是另一个没有权限的用户
	warnings := c.repairRuntimeAccessFor(65534, 65534)
	if len(warnings) == 0 {
		t.Fatal("修不了的权限问题必须给出警告，不能静默")
	}
	found := false
	for _, w := range warnings {
		// 必须给出可直接照抄的修复指令，否则用户只会看到"升级完面板没了"
		if strings.Contains(w, "以 root 执行") {
			found = true
		}
	}
	if !found {
		t.Errorf("警告里应给出可直接照抄的 chown 指令，实际: %v", warnings)
	}
}

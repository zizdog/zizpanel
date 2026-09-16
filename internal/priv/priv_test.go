package priv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 安全相关的校验必须有测试锁死。这些函数是"面板被攻破也不能提权"的边界，
// 一旦回归就是严重漏洞。

func TestValidateDomainRejectsInjection(t *testing.T) {
	bad := []string{
		"",
		"../etc/passwd",
		"evil.com/../../etc/passwd",
		"evil.com; rm -rf /",
		"evil.com && whoami",
		"evil.com\nmalicious",
		"`id`",
		"$(id)",
		"a",
		"-leading-dash.com",
		"trailing-.com",
		"..",
		".",
		"a..b",
		strings.Repeat("a", 260) + ".com",
		"中文域名.com",
		"evil.com\tx",
	}
	for _, d := range bad {
		if err := ValidateDomain(d); err == nil {
			t.Fatalf("非法域名未被拒绝: %q", d)
		}
	}
}

func TestValidateDomainAcceptsRealDomains(t *testing.T) {
	good := []string{
		"example.com",
		"www.example.com",
		"a.b.c.d.example.com",
		"demo.test",
		"my-site.local",
		"xn--fiqs8s.com",
		"sub.domain.co.uk",
	}
	for _, d := range good {
		if err := ValidateDomain(d); err != nil {
			t.Fatalf("合法域名被拒绝: %q (%v)", d, err)
		}
	}
}

func TestSafeJoinBlocksTraversal(t *testing.T) {
	dir := t.TempDir()
	// macOS 上 /var 是 /private/var 的软链接，safeJoin 返回的是解析后的真实路径，
	// 因此比较基准也要解析，否则会误判（这正是真实踩到过的坑）。
	wantDir := resolvePath(dir)

	bad := []string{
		"../evil.conf",
		"../../etc/passwd",
		"a/../../evil.conf",
		"sub/evil.conf",
		"/etc/passwd",
		"..",
		"",
		"a b.conf",
		"a;b.conf",
		"a$(id).conf",
	}
	for _, n := range bad {
		if _, err := safeJoin(dir, n); err == nil {
			t.Fatalf("越界名称未被拒绝: %q", n)
		}
	}

	good := []string{"example.com", "example.com.conf", "a-b_c.1.conf", "000-default.conf"}
	for _, n := range good {
		p, err := safeJoin(dir, n)
		if err != nil {
			t.Fatalf("合法名称被拒绝: %q (%v)", n, err)
		}
		if filepath.Dir(p) != wantDir {
			t.Fatalf("结果不在目标目录内: %s (期望目录 %s)", p, wantDir)
		}
	}
}

// 即使目录内存在指向外部的软链接，也不能借它写出去。
func TestSafeJoinResistsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	vhostDir := filepath.Join(base, "vhosts")
	if err := os.MkdirAll(vhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// vhostDir 本身是个软链接，指向外部目录
	link := filepath.Join(base, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("当前环境不支持软链接")
	}
	// safeJoin 会用 EvalSymlinks 解析真实目录，因此写入位置应落在 outside 内，
	// 而不会逃到别处 —— 关键是结果必须是 link 目录的"直接子项"。
	p, err := safeJoin(link, "ok.conf")
	if err != nil {
		t.Fatalf("解析软链接目录失败: %v", err)
	}
	if filepath.Dir(p) != resolvePath(outside) {
		t.Fatalf("未按真实目录定位: %s (期望 %s)", p, resolvePath(outside))
	}
	// 穿越尝试仍然必须失败
	if _, err := safeJoin(link, "../escape.conf"); err == nil {
		t.Fatal("穿越尝试未被拒绝")
	}
}

func TestVhostFileName(t *testing.T) {
	cases := map[string]string{
		"example.com":      "example.com.conf",
		"example.com.conf": "example.com.conf",
		"000-default":      "000-default.conf",
	}
	for in, want := range cases {
		if got := vhostFileName(in); got != want {
			t.Fatalf("vhostFileName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestToggleHostsContentOnlyTouchesTaggedLines(t *testing.T) {
	original := `##
# Host Database
##
127.0.0.1	localhost
255.255.255.255	broadcasthost
::1             localhost
127.0.0.1	demo.test	# zizpanel-local
# 127.0.0.1	old.test	# zizpanel-local
127.0.0.1	user-own.test
`
	// 关闭：只注释掉启用中的本面板条目
	off, changed := toggleHostsContent(original, false)
	if !changed {
		t.Fatal("关闭时应检测到变更")
	}
	if !strings.Contains(off, "# 127.0.0.1\tdemo.test\t# zizpanel-local") {
		t.Fatalf("启用中的条目未被注释:\n%s", off)
	}
	// 用户自己的解析绝不能被动
	if !strings.Contains(off, "\n127.0.0.1\tuser-own.test") {
		t.Fatal("用户自己的 hosts 条目被误改")
	}
	if !strings.Contains(off, "127.0.0.1\tlocalhost") {
		t.Fatal("系统自带条目被误改")
	}
	// 已注释的条目保持注释（幂等）
	if !strings.Contains(off, "# 127.0.0.1\told.test\t# zizpanel-local") {
		t.Fatal("已停用条目状态被改变")
	}

	// 再次关闭：不应再产生变更（幂等）
	if _, changed2 := toggleHostsContent(off, false); changed2 {
		t.Fatal("重复关闭不应产生变更")
	}

	// 开启：两条都恢复
	on, changed3 := toggleHostsContent(off, true)
	if !changed3 {
		t.Fatal("开启时应检测到变更")
	}
	if strings.Contains(on, "# 127.0.0.1\tdemo.test") || strings.Contains(on, "# 127.0.0.1\told.test") {
		t.Fatalf("条目未被启用:\n%s", on)
	}
	if !strings.Contains(on, "\n127.0.0.1\tuser-own.test") {
		t.Fatal("开启时误改了用户条目")
	}
}

func TestParsePortValidation(t *testing.T) {
	bad := []string{"", "0", "65536", "abc", "-1", "80; rm -rf /", "8 0", "999999"}
	for _, p := range bad {
		if _, err := CheckPort(p); err == nil {
			t.Fatalf("非法端口未被拒绝: %q", p)
		}
	}
	// 合法端口即使未被占用也应正常返回
	info, err := CheckPort("59999")
	if err != nil {
		t.Fatalf("合法端口检测失败: %v", err)
	}
	if info.Port != 59999 {
		t.Fatalf("端口号解析错误: %d", info.Port)
	}
}

func TestLaunchLabelValidation(t *testing.T) {
	// 非法 label 必须被拒绝，且不能真的去调用 launchctl
	bad := []string{"a;b", "a/b", "$(id)", "a b", ""}
	for _, l := range bad {
		if _, err := LaunchStatus(l); err == nil {
			t.Fatalf("非法 label 未被拒绝: %q", l)
		}
		if err := LaunchLoad(l); err == nil {
			t.Fatalf("非法 label 未被拒绝(load): %q", l)
		}
		if err := LaunchUnload(l); err == nil {
			t.Fatalf("非法 label 未被拒绝(unload): %q", l)
		}
	}
}

func TestNginxStatusOnThisMachine(t *testing.T) {
	requireRoot(t)
	st, err := NginxStatus()
	if err != nil {
		t.Fatalf("查询 nginx 状态失败: %v", err)
	}
	if st != "running" && st != "stopped" {
		t.Fatalf("状态值异常: %q", st)
	}
	t.Logf("本机 nginx 状态: %s", st)
}

// requireRoot 跳过需要 root 的用例。
//
// 说明：priv 包在生产中始终以 root 运行（由 sudoers 授权），
// 非 root 下 nginx 无法读取 pid 文件与日志目录，属于预期内的权限差异，
// 不是代码缺陷 —— 所以这里跳过而不是失败。
// 以 root 运行 `go test ./internal/priv/` 时会真正执行这些校验。
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("需要 root 权限（生产环境中 helper 始终以 root 运行）")
	}
}

func TestNginxTestOnThisMachine(t *testing.T) {
	requireRoot(t)
	out, err := NginxTest()
	if err != nil {
		t.Fatalf("nginx 配置检查失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "successful") {
		t.Fatalf("nginx -t 输出异常: %s", out)
	}
	t.Logf("nginx -t: %s", strings.Split(out, "\n")[0])
}

func TestListVhostsFindsExisting(t *testing.T) {
	list, err := ListVhosts()
	if err != nil {
		t.Fatalf("列出 vhost 失败: %v", err)
	}
	// 本机存在 000-default.conf 与 zizdog.cn.conf
	if len(list) == 0 {
		t.Skip("本机没有 vhost 文件")
	}
	hasConf := false
	for _, f := range list {
		if !strings.HasSuffix(f, ".conf") {
			t.Fatalf("列出了非 conf 文件: %s", f)
		}
		hasConf = true
	}
	if !hasConf {
		t.Fatal("未列出任何 conf 文件")
	}
	t.Logf("vhost 文件: %v", list)
}

func TestReadVhostRejectsTraversal(t *testing.T) {
	bad := []string{"../../../etc/passwd", "..", "/etc/hosts", "a/../../b.conf"}
	for _, n := range bad {
		if _, err := ReadVhost(n); err == nil {
			t.Fatalf("越界读取未被拒绝: %q", n)
		}
	}
}

func TestFirewallStateReadable(t *testing.T) {
	st, err := FirewallState()
	if err != nil {
		t.Skipf("无法读取防火墙状态（可能无权限）: %v", err)
	}
	if !strings.Contains(st, "Firewall") {
		t.Fatalf("防火墙状态输出异常: %s", st)
	}
	t.Logf("防火墙: %s", st)
}

func TestRootRespectsEnv(t *testing.T) {
	t.Setenv("ZIZPANEL_ROOT", "/tmp/custom-root")
	if Root() != "/tmp/custom-root" {
		t.Fatalf("ZIZPANEL_ROOT 未生效: %s", Root())
	}
	t.Setenv("ZIZPANEL_ROOT", "")
	if Root() != "/opt/zizpanel" {
		t.Fatalf("默认根目录错误: %s", Root())
	}
}

func TestHomebrewPrefixDetected(t *testing.T) {
	p := HomebrewPrefix()
	if p != "/opt/homebrew" && p != "/usr/local" {
		t.Fatalf("Homebrew 前缀异常: %s", p)
	}
	if !strings.HasSuffix(NginxConf(), "etc/nginx/nginx.conf") {
		t.Fatalf("nginx 配置路径异常: %s", NginxConf())
	}
}

// TestEnsureNginxRuntimeDirsCreatesLogDirs 锁住"nginx 日志目录必须存在"。
//
// 真机事故（2026-09-16）：全新装出来的 nginx 没有 <brew>/var/log/nginx，
// 于是 `nginx -t` 报 "could not open error log file"，**任何** vhost 都写不进去；
// 反代规则保存时被如实拦下（"配置语法错误，已回滚"），而用户完全联想不到
// 是缺一个日志目录。所以补目录必须是面板的职责，并在这里钉住。
func TestEnsureNginxRuntimeDirsCreatesLogDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZIZPANEL_BREW_PREFIX", root)
	t.Setenv("ZIZPANEL_USER", "")

	// 造一份**真实形状**的 nginx.conf（brew 默认配置里的路径指令都在）
	confDir := filepath.Join(root, "etc", "nginx")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := `pid        ` + filepath.Join(root, "var", "run", "nginx.pid") + `;
error_log  ` + filepath.Join(root, "var", "log", "nginx", "error.log") + `;
http {
    access_log  ` + filepath.Join(root, "var", "log", "nginx", "access.log") + `;
    client_body_temp_path ` + filepath.Join(root, "var", "run", "nginx", "client_body_temp") + `;
    proxy_temp_path       ` + filepath.Join(root, "var", "run", "nginx", "proxy_temp") + `;
}
`
	if err := os.WriteFile(filepath.Join(confDir, "nginx.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	msg, err := ensureNginxRuntimeDirs()
	if err != nil {
		t.Fatalf("补目录失败：%v", err)
	}
	if msg == "" {
		t.Error("第一次调用应当报告有改动（调用方据此 reload）")
	}
	for _, d := range []string{
		filepath.Join(root, "var", "log", "nginx"),
		filepath.Join(root, "var", "log", "nginx", "proxy"),
		filepath.Join(root, "var", "run"), // nginx.pid 要写这里
		filepath.Join(root, "var", "run", "nginx", "client_body_temp"),
		filepath.Join(root, "var", "run", "nginx", "proxy_temp"),
	} {
		if st, serr := os.Stat(d); serr != nil || !st.IsDir() {
			t.Errorf("目录应存在：%s（err=%v）", d, serr)
		}
	}
	// 幂等：第二次不该再报告改动
	msg2, err := ensureNginxRuntimeDirs()
	if err != nil {
		t.Fatal(err)
	}
	if msg2 != "" {
		t.Errorf("第二次调用不该报告改动，实际 %q", msg2)
	}
}

// TestEnsureVhostsIncludeIsIdempotent 锁住"vhosts/*.conf 必须被加载"。
//
// 真机事故（2026-09-16）：`brew reinstall nginx` 把 nginx.conf 还原成 brew 默认版，
// 面板的两条 include（conf.d 与 vhosts）一起丢了。于是站点/反代配置**写进去了、
// 文件也在**，nginx 却根本不加载 —— 用户看到的是"配置明明有，访问却不通"。
// 之前只有 LNMP 安装流程补这条，修复/重装 nginx 之后就丢了，所以改为启动自愈。
func TestEnsureVhostsIncludeIsIdempotent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZIZPANEL_BREW_PREFIX", root)
	confDir := filepath.Join(root, "etc", "nginx")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 模拟 brew 默认版：**没有** vhosts include
	conf := "events {}\\nhttp {\\n    server {\\n        listen 80;\\n    }\\n}\\n"
	confPath := filepath.Join(confDir, "nginx.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	if ok, _, _ := VhostsIncluded(); ok {
		t.Fatal("前提错误：这份配置本来不该包含 vhosts")
	}
	if err := EnsureVhostsInclude(); err != nil {
		t.Fatalf("补 include 失败：%v", err)
	}
	ok, msg, err := VhostsIncluded()
	if err != nil || !ok {
		t.Fatalf("补完后应当包含 vhosts，实际 ok=%v msg=%q err=%v", ok, msg, err)
	}
	// 备份要留下（改用户配置前必须能回退）
	if _, err := os.Stat(confPath + ".zizpanel.bak"); err != nil {
		t.Errorf("改 nginx.conf 前应当备份：%v", err)
	}
	// 幂等：再跑一次不该重复插入
	first, _ := os.ReadFile(confPath)
	if err := EnsureVhostsInclude(); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(confPath)
	if string(first) != string(second) {
		t.Error("重复调用不该再改文件（幂等）")
	}
	// vhosts 目录要建出来
	if st, serr := os.Stat(filepath.Join(confDir, "vhosts")); serr != nil || !st.IsDir() {
		t.Errorf("vhosts 目录应被创建：%v", serr)
	}
}

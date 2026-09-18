package sites

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// listenUnix 在测试里真的绑一个 unix socket（不启动任何 PHP 进程），
// 用来验证 EndpointLive 的判定逻辑。
func listenUnix(t *testing.T, path string) (func(), string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("绑定测试 socket 失败: %v", err)
	}
	return func() { _ = ln.Close() }, path
}

// 本文件是"PHP 多版本共存"的核心测试。
//
// 铁律：全部用临时目录造假 brewPrefix（shortTempDir，见下），**绝不碰真实 /opt/homebrew**。
// 本项目历史上因为测试漏沙箱化把生产 nginx 配置改坏过（全站 502），
// 这里是会改写 php-fpm 配置的代码，沙箱化更是一票否决项。

// shortTempDir 造一个**足够短**的临时目录。
//
// 为什么不用 t.TempDir()：macOS 上它形如
// /var/folders/jd/<40 个随机字符>/T/TestXxx/001，光前缀就有 60+ 字符，
// 而 Unix domain socket 的 sun_path 上限是 104 字节 —— 真机上
// /opt/homebrew/var/run/php-fpm-8.3.sock 只有 41 字符，测试里却会先撞上限。
// 所以这里用 os.MkdirTemp("/tmp", "zp") 造短路径，并自己负责清理。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeBrew 在临时目录里搭一个假的 Homebrew 前缀。
//
// versions 形如 {"8.2": "127.0.0.1:9000", "8.3": "/tmp/x.sock"}，
// 值是写进该版本 www.conf 的 listen 原文（模拟 Homebrew 出厂配置）。
func fakeBrew(t *testing.T, versions map[string]string) string {
	t.Helper()
	prefix := shortTempDir(t)
	if err := os.MkdirAll(filepath.Join(prefix, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	for v, listen := range versions {
		// opt/php@x.y（DiscoverPHPVersions 从 opt/ 里发现版本）
		if err := os.MkdirAll(filepath.Join(prefix, "opt", "php@"+v, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		// 解释器必须真存在：DiscoverPHPVersions 现在要求 <opt>/<name>/bin/php 可 stat
		// （否则一个空目录/残留会被当成"装了 PHP"，2026-09-18 用户实测的假版本）。
		if err := os.WriteFile(filepath.Join(prefix, "opt", "php@"+v, "bin", "php"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		confDir := filepath.Join(prefix, "etc", "php", v, "php-fpm.d")
		if err := os.MkdirAll(confDir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf(`; 由测试生成：模拟 Homebrew 出厂的 www.conf
[global]
pid = run/php-fpm.pid

[www]
user = tester
group = staff
;listen.owner = _www
;listen.group = _www
;listen.mode = 0660
listen = %s
pm = dynamic
`, listen)
		if err := os.WriteFile(filepath.Join(confDir, "www.conf"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return prefix
}

// ---------------------------------------------------------------------------
//  1) 多版本解析：两个版本必须得到**两个不同的端点**，且都不能是 9000
// ---------------------------------------------------------------------------

func TestResolveConfigEndpointMultiVersion(t *testing.T) {
	// 刻意让两个版本都写着 Homebrew 出厂的 9000 —— 这正是真机上冲突的起点
	prefix := fakeBrew(t, map[string]string{
		"8.2": "127.0.0.1:9000",
		"8.3": "127.0.0.1:9000",
	})

	// 先落实面板分配的端点（模拟"安装/启用某版本"这一步）
	for _, ver := range []string{"8.2", "8.3"} {
		res, err := EnsureListen(prefix, ver, EnsureListenOptions{
			SocketUser: "tester", SocketGroup: "staff", NoBackup: true,
		})
		if err != nil {
			t.Fatalf("EnsureListen(%s) 失败: %v", ver, err)
		}
		if !res.Changed {
			t.Fatalf("PHP %s 从 9000 改成独立端点，Changed 应为 true", ver)
		}
	}

	got := map[string]string{}
	for _, ver := range []string{"8.2", "8.3"} {
		ep, err := ResolveConfigEndpoint(prefix, ver)
		if err != nil {
			t.Fatalf("解析 PHP %s 端点失败: %v", ver, err)
		}
		got[ver] = ep
	}

	// 核心断言：两个版本解析出两个不同的端点
	if got["8.2"] == got["8.3"] {
		t.Fatalf("两个 PHP 版本解析出了同一个端点 %s —— 多版本共存不成立", got["8.2"])
	}
	// 且都不能回退到 9000（那正是旧实现里"静默回退"的后果）
	for ver, ep := range got {
		if strings.Contains(ep, ":9000") {
			t.Fatalf("PHP %s 仍然解析到 9000：%s", ver, ep)
		}
		if !strings.HasPrefix(ep, "unix:") {
			t.Fatalf("PHP %s 的端点应为 unix socket，实际 %s", ver, ep)
		}
	}
	// 端点里应带各自的版本号，便于人工排查
	if !strings.Contains(got["8.2"], "php-fpm-8.2.sock") || !strings.Contains(got["8.3"], "php-fpm-8.3.sock") {
		t.Fatalf("端点未按版本唯一命名：8.2=%s 8.3=%s", got["8.2"], got["8.3"])
	}
	t.Logf("PHP 8.2 -> %s", got["8.2"])
	t.Logf("PHP 8.3 -> %s", got["8.3"])
}

// 解析失败必须**报错**，而不是蒙一个端口。
//
// 这是本次改动最关键的行为变更：旧的 ResolveFastCGI 读不到 www.conf 时
// 返回 127.0.0.1:9000，于是面板会为"没装的版本"生成一份指向 9000 的 vhost。
func TestResolveConfigEndpointErrorsInsteadOfFallingBackTo9000(t *testing.T) {
	prefix := fakeBrew(t, map[string]string{"8.3": "127.0.0.1:9000"})

	cases := []struct {
		name    string
		version string
		wantSub []string
	}{
		{
			name:    "版本未安装（配置文件不存在）",
			version: "8.1",
			wantSub: []string{"8.1", "www.conf", "未安装"},
		},
		{
			name:    "版本号为空",
			version: "",
			wantSub: []string{"未指定 PHP 版本"},
		},
		{
			name:    "版本号格式非法",
			version: "../../etc/passwd",
			wantSub: []string{"格式不合法"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ep, err := ResolveConfigEndpoint(prefix, c.version)
			if err == nil {
				t.Fatalf("应当报错，却返回了端点 %q", ep)
			}
			if ep != "" {
				t.Fatalf("报错时不应同时返回端点，实际 %q", ep)
			}
			for _, sub := range c.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("错误信息里应包含 %q（便于排障），实际: %s", sub, err.Error())
				}
			}
		})
	}
}

// www.conf 存在但没有生效的 listen 行 -> 也必须报错，不能猜。
func TestResolveConfigEndpointMissingListenLine(t *testing.T) {
	prefix := shortTempDir(t)
	confDir := filepath.Join(prefix, "etc", "php", "8.3", "php-fpm.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 只有注释掉的 listen（真实 www.conf 里就有一堆这种示例行）
	body := "[www]\nuser = tester\n;listen = 127.0.0.1:9000\n"
	if err := os.WriteFile(filepath.Join(confDir, "www.conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ep, err := ResolveConfigEndpoint(prefix, "8.3")
	if err == nil {
		t.Fatalf("注释掉的 listen 不算配置，应报错；实际返回 %q", ep)
	}
	for _, sub := range []string{"8.3", "www.conf", "listen"} {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("错误信息里应包含 %q，实际: %s", sub, err.Error())
		}
	}
}

// ResolveEndpoint（站点生成配置时用的口径）还要求端点真的在监听。
func TestResolveEndpointRequiresLiveProcess(t *testing.T) {
	prefix := fakeBrew(t, map[string]string{"8.3": "127.0.0.1:9000"})
	if _, err := EnsureListen(prefix, "8.3", EnsureListenOptions{NoBackup: true}); err != nil {
		t.Fatal(err)
	}
	// 配置已改对，但没人监听 socket（测试环境里当然没有 fpm）
	ep, err := ResolveEndpoint(prefix, "8.3")
	if err == nil {
		t.Fatalf("端点没人监听时应报错，实际返回 %q", ep)
	}
	for _, sub := range []string{"8.3", "没有进程在监听"} {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("错误信息里应包含 %q，实际: %s", sub, err.Error())
		}
	}
}

// ---------------------------------------------------------------------------
//  2) vhost 生成：两个站点用不同 PHP 版本 -> 两份 fastcgi_pass 必须不同
// ---------------------------------------------------------------------------

func TestGenerateVhostPerSitePHPVersion(t *testing.T) {
	prefix := fakeBrew(t, map[string]string{
		"8.2": "127.0.0.1:9000",
		"8.3": "127.0.0.1:9000",
	})
	for _, ver := range []string{"8.2", "8.3"} {
		if _, err := EnsureListen(prefix, ver, EnsureListenOptions{NoBackup: true}); err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	siteA := &Site{Domain: "a.test", Root: root, PHPVersion: "8.2", Rewrite: "typecho", Enabled: true}
	siteB := &Site{Domain: "b.test", Root: root, PHPVersion: "8.3", Rewrite: "typecho", Enabled: true}

	passA, err := ResolveConfigEndpoint(prefix, siteA.PHPVersion)
	if err != nil {
		t.Fatal(err)
	}
	passB, err := ResolveConfigEndpoint(prefix, siteB.PHPVersion)
	if err != nil {
		t.Fatal(err)
	}
	confA, err := siteA.Generate(Options{LogDir: root, FastCGIPass: passA})
	if err != nil {
		t.Fatal(err)
	}
	confB, err := siteB.Generate(Options{LogDir: root, FastCGIPass: passB})
	if err != nil {
		t.Fatal(err)
	}

	if passA == passB {
		t.Fatalf("两个站点的 fastcgi_pass 相同（%s）—— 按站点指定 PHP 版本不成立", passA)
	}
	if !strings.Contains(confA, "fastcgi_pass "+passA+";") {
		t.Fatalf("站点 A 的 vhost 没有使用 8.2 的端点 %s\n%s", passA, confA)
	}
	if !strings.Contains(confB, "fastcgi_pass "+passB+";") {
		t.Fatalf("站点 B 的 vhost 没有使用 8.3 的端点 %s\n%s", passB, confB)
	}
	// 两者必须互不串台
	if strings.Contains(confA, passB) || strings.Contains(confB, passA) {
		t.Fatal("两个站点的 vhost 互相包含了对方的端点")
	}
	// 顺带守住"内联 fastcgi 参数、不 include 全局文件"这个设计
	for _, conf := range []string{confA, confB} {
		if strings.Contains(conf, "php-fpm.conf") {
			t.Fatal("不应 include 全局 php-fpm.conf：那会让所有站点共用同一 PHP 版本")
		}
		if strings.Contains(conf, "127.0.0.1:9000") {
			t.Fatal("vhost 不应再出现 9000 兜底地址")
		}
	}
}

// ---------------------------------------------------------------------------
//  3) EnsureListen：幂等、留一次性备份、补齐缺失指令
// ---------------------------------------------------------------------------

func TestEnsureListenIdempotentAndBackup(t *testing.T) {
	prefix := fakeBrew(t, map[string]string{"8.3": "127.0.0.1:9000"})
	conf := FPMConfPath(prefix, "8.3")
	original, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}

	opt := EnsureListenOptions{SocketUser: "tester", SocketGroup: "staff"}
	res1, err := EnsureListen(prefix, "8.3", opt)
	if err != nil {
		t.Fatal(err)
	}
	if !res1.Changed {
		t.Fatal("首次改写应报告 Changed=true")
	}
	if !strings.HasPrefix(res1.Endpoint, "unix:") {
		t.Fatalf("面板应分配 unix socket 端点，实际 %s", res1.Endpoint)
	}
	// 备份必须留着出厂配置（能回到 Homebrew 原状）
	if res1.Backup == "" {
		t.Fatal("首次改写必须留备份")
	}
	bak, err := os.ReadFile(res1.Backup)
	if err != nil {
		t.Fatalf("读备份失败: %v", err)
	}
	if string(bak) != string(original) {
		t.Fatal("备份内容与改写前不一致，起不到回滚作用")
	}

	// 套接字目录必须已被建出来：php-fpm 是在启动时 bind 的，
	// 目录不存在它会直接起不来（真机上就是这个症状）。
	sockDir := filepath.Join(prefix, "var", "run")
	if st, err := os.Stat(sockDir); err != nil || !st.IsDir() {
		t.Fatalf("套接字目录 %s 应被创建: %v", sockDir, err)
	}

	after1, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	// 写进去的是**裸路径**（php-fpm 不接受 unix: 前缀），而 res.Endpoint 是
	// 对外的规范形式（带 unix:），两者是有意区分的。
	if !strings.Contains(string(after1), "listen = "+strings.TrimPrefix(res1.Endpoint, "unix:")) {
		t.Fatalf("www.conf 里没有写入目标端点（应为裸路径 %s）:\n%s",
			strings.TrimPrefix(res1.Endpoint, "unix:"), after1)
	}
	// 注释掉的示例行必须原样保留（否则会把 Homebrew 的说明文档改成乱码）
	if !strings.Contains(string(after1), ";listen.owner = _www") {
		t.Fatal("被注释的示例行不应被改写")
	}

	// 再跑一次：必须什么都不做
	res2, err := EnsureListen(prefix, "8.3", opt)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Fatal("第二次调用应幂等（Changed=false）")
	}
	after2, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if string(after2) != string(after1) {
		t.Fatal("幂等性被破坏：文件内容发生了变化")
	}
}

// 被精简过的 www.conf（没有 listen 行）也要能补齐，且补在 [www] 段里。
func TestEnsureListenAppendsMissingDirectives(t *testing.T) {
	prefix := shortTempDir(t)
	confDir := filepath.Join(prefix, "etc", "php", "8.4", "php-fpm.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(confDir, "www.conf")
	if err := os.WriteFile(conf, []byte("[global]\npid = run/x.pid\n\n[www]\nuser = tester\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := EnsureListen(prefix, "8.4", EnsureListenOptions{
		SocketUser: "tester", SocketGroup: "staff", NoBackup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		// php-fpm 的 listen 是裸路径（不带 unix: 前缀）
		"listen = " + strings.TrimPrefix(res.Endpoint, "unix:"),
		"listen.owner = tester",
		"listen.group = staff",
		"listen.mode = 0660",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("补齐后的配置缺少 %q:\n%s", want, got)
		}
	}
	// 解析回来必须就是我们写进去的那个端点
	ep, err := ResolveConfigEndpoint(prefix, "8.4")
	if err != nil {
		t.Fatal(err)
	}
	if ep != res.Endpoint {
		t.Fatalf("写进去的端点读回来不一致：写 %s 读 %s", res.Endpoint, ep)
	}
}

// 用真实的 Homebrew www.conf 形态验证改写只动该动的行。
//
// 这里刻意让 www.conf **已经有** listen.owner/group/mode（某些机器上被手工
// 取消过注释，或者别的工具写过），这样才能断言"行数不变、无关行一字不动"。
// 缺失这些指令时的补齐行为由 TestEnsureListenAppendsMissingDirectives 覆盖。
func TestEnsureListenPreservesUnrelatedLines(t *testing.T) {
	prefix := shortTempDir(t)
	confDir := filepath.Join(prefix, "etc", "php", "8.3", "php-fpm.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(confDir, "www.conf")
	body := `; 模拟被改过的 www.conf
[global]
pid = run/php-fpm.pid
error_log = /opt/homebrew/var/log/php-fpm.log

[www]
user = tester
group = staff
listen = 127.0.0.1:9000
listen.owner = _www
listen.group = _www
listen.mode = 0660
pm = dynamic
pm.max_children = 5
`
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(conf)

	// 记录改写前的"与 listen 无关"的行：listen* 的赋值行会被改写，
	// 注释行不受影响，其余必须一字不动。
	unrelated := func(content string) []string {
		var out []string
		for _, ln := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(ln)
			if !strings.HasPrefix(trimmed, ";") && strings.HasPrefix(trimmed, "listen") {
				continue
			}
			out = append(out, ln)
		}
		return out
	}
	want := unrelated(string(before))

	if _, err := EnsureListen(prefix, "8.3", EnsureListenOptions{
		SocketUser: "tester", SocketGroup: "staff", NoBackup: true,
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(conf)
	got := unrelated(string(after))

	if len(got) != len(want) {
		t.Fatalf("无关行数变了：前 %d 行 -> 后 %d 行\n%s", len(want), len(got), after)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("第 %d 行被改动：\n前: %q\n后: %q", i+1, want[i], got[i])
		}
	}
	// 顺带确认确实改了 listen 三项。
	// 注意：写进 php-fpm 的必须是**裸路径**（它不认 nginx 的 unix: 前缀，
	// 带了会以 `invalid port value` 启动失败 —— 真机实测）。
	text := string(after)
	if !strings.Contains(text, "listen = "+filepath.Join(prefix, "var", "run", "php-fpm-8.3.sock")) {
		t.Fatalf("listen 未被改写成 php-fpm 能接受的裸路径:\n%s", text)
	}
	if strings.Contains(text, "listen = unix:") {
		t.Fatalf("php-fpm 的 listen 不能带 unix: 前缀（会启动失败）:\n%s", text)
	}
	if !strings.Contains(text, "listen.owner = tester") || !strings.Contains(text, "listen.group = staff") {
		t.Fatalf("listen.owner/group 未被改写:\n%s", text)
	}
}

// 套接字路径过长（自定义 Homebrew 深前缀）要给出可读报错，而不是让 fpm 后台失败。
func TestSocketPathTooLong(t *testing.T) {
	deep := "/tmp/" + strings.Repeat("a", fpmSocketMaxLen)
	if _, err := SocketPath(deep, "8.3"); err == nil {
		t.Fatal("超长套接字路径应报错（macOS sun_path 上限 104）")
	} else if !strings.Contains(err.Error(), "过长") {
		t.Fatalf("报错信息应说明原因，实际: %s", err.Error())
	}
}

// 用 test hook 装一个真实存在的 unix socket，验证 EndpointLive 的判定。
func TestEndpointLiveUnixSocket(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "php-fpm-test.sock")
	if EndpointLive("unix:" + sock) {
		t.Fatal("不存在的 socket 不应判定为在监听")
	}
	// 残留的普通文件（不是 socket）也不能算"在监听"：
	// 只看 os.Stat 是否成功会造成假阳性，进而生成指向空气的 vhost。
	if err := os.WriteFile(sock, []byte("not a socket"), 0o644); err != nil {
		t.Fatal(err)
	}
	if EndpointLive("unix:" + sock) {
		t.Fatal("普通文件不应被当成在监听的 socket")
	}
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	closeFn, addr := listenUnix(t, sock)
	defer closeFn()
	if !EndpointLive("unix:" + addr) {
		t.Fatalf("已绑定的 socket %s 应判定为在监听", addr)
	}
}

// ---------------------------------------------------------------------------
//  4) 按 Homebrew 实际安装情况发现版本
// ---------------------------------------------------------------------------

func TestDiscoverPHPVersions(t *testing.T) {
	prefix := fakeBrew(t, map[string]string{
		"8.2": "127.0.0.1:9000",
		"8.3": "127.0.0.1:9000",
		"8.4": "127.0.0.1:9000",
	})
	// 模拟 `php` 这个版本别名（软链到 php@8.4）：不能因此多出一条"php"版本
	if err := os.Symlink(filepath.Join(prefix, "opt", "php@8.4"), filepath.Join(prefix, "opt", "php")); err != nil {
		t.Fatal(err)
	}
	// 混入一个非 PHP 的 opt 条目，必须被忽略
	if err := os.MkdirAll(filepath.Join(prefix, "opt", "phpmyadmin"), 0o755); err != nil {
		t.Fatal(err)
	}

	list := DiscoverPHPVersions(prefix)
	if len(list) != 3 {
		t.Fatalf("应发现 3 个版本，实际 %d 个: %+v", len(list), list)
	}
	wantOrder := []string{"8.2", "8.3", "8.4"}
	for i, want := range wantOrder {
		if list[i].Version != want {
			t.Fatalf("第 %d 个版本应为 %s，实际 %s（顺序不稳定会让界面跳来跳去）",
				i+1, want, list[i].Version)
		}
	}
	// 每个版本都要有各自的"面板分配端点"，且互不相同
	seen := map[string]string{}
	for _, pv := range list {
		if pv.PreferredPass == "" {
			t.Fatalf("PHP %s 没有分配端点", pv.Version)
		}
		if prev, dup := seen[pv.PreferredPass]; dup {
			t.Fatalf("PHP %s 与 %s 分配到了同一个端点 %s", pv.Version, prev, pv.PreferredPass)
		}
		seen[pv.PreferredPass] = pv.Version
		// 还没改过 www.conf：端点仍然是 9000，ListenOK 必须是 false
		if pv.ListenOK {
			t.Fatalf("PHP %s 的 www.conf 还是 9000，ListenOK 不应为 true", pv.Version)
		}
		if pv.Pass != "127.0.0.1:9000" {
			t.Fatalf("PHP %s 的当前端点应为 9000，实际 %s", pv.Version, pv.Pass)
		}
	}

	// 改好一个版本后，该版本的 ListenOK 应变为 true
	if _, err := EnsureListen(prefix, "8.3", EnsureListenOptions{NoBackup: true}); err != nil {
		t.Fatal(err)
	}
	list = DiscoverPHPVersions(prefix)
	for _, pv := range list {
		if pv.Version != "8.3" {
			continue
		}
		if !pv.ListenOK {
			t.Fatalf("PHP 8.3 已配好，ListenOK 应为 true（实际 pass=%s）", pv.Pass)
		}
		if pv.Pass != pv.PreferredPass {
			t.Fatalf("PHP 8.3 的 pass 应等于分配端点：%s vs %s", pv.Pass, pv.PreferredPass)
		}
	}
}

// 两个版本共用端点时必须被显式标记（而不是像旧实现那样悄悄去重、少列一个版本）。
// TestDiscoverPHPVersionsIgnoresDanglingAlias 锁住 2026-09-18 用户实测的假"已安装"：
//
// Homebrew 卸掉某个 PHP 之后，<prefix>/opt/php 会**留下悬空软链接**（指向已删掉的
// Cellar/php/8.4.7）。旧实现只 os.Readlink 解析出 "8.4" 就把它列成已安装 ——
// 用户在面板里看到 PHP 8.4，照着去 `brew uninstall php@8.4` 只得到
// "Error: No such keg: /opt/homebrew/Cellar/php@8.4"。
//
// 判据必须贴着运行体：软链接/目录要能 stat 到，且 bin/php 真的在。
func TestDiscoverPHPVersionsIgnoresDanglingAlias(t *testing.T) {
	prefix := fakeBrew(t, map[string]string{"8.2": "127.0.0.1:9000"})
	// 悬空软链接：目标 Cellar/php/8.4.7 根本不存在
	if err := os.Symlink(filepath.Join(prefix, "Cellar", "php", "8.4.7"),
		filepath.Join(prefix, "opt", "php")); err != nil {
		t.Fatal(err)
	}
	list := DiscoverPHPVersions(prefix)
	if len(list) != 1 || list[0].Version != "8.2" {
		t.Fatalf("悬空软链接不该被当成已安装的 PHP 版本，实际: %+v", list)
	}

	// 版本别名 `php` 指向的 Cellar 目录名是**完整版本**（php/8.4.7）：
	// 必须归一化成 major.minor（8.4），否则这台机器上"装了 PHP 8.4 却完全列不出来"。
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar", "php", "8.4.7", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "Cellar", "php", "8.4.7", "bin", "php"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(prefix, "Cellar", "php", "8.4.7"),
		filepath.Join(prefix, "opt", "php2")); err != nil {
		t.Fatal(err)
	}
	// （opt/php2 不是合法名字，仅用于确认下面的 php 解析；这里直接覆盖 opt/php 软链）
	_ = os.Remove(filepath.Join(prefix, "opt", "php"))
	if err := os.Symlink(filepath.Join(prefix, "Cellar", "php", "8.4.7"),
		filepath.Join(prefix, "opt", "php")); err != nil {
		t.Fatal(err)
	}
	list = DiscoverPHPVersions(prefix)
	found84 := false
	for _, pv := range list {
		if pv.Version == "8.4" {
			found84 = true
		}
		if strings.Contains(pv.Version, ".") && strings.Count(pv.Version, ".") > 1 {
			t.Fatalf("版本号必须是 major.minor 形式，实际 %q", pv.Version)
		}
	}
	if !found84 {
		t.Fatalf("`php` 别名指向 Cellar/php/8.4.7 时应列出 8.4，实际: %+v", list)
	}

	// 目录在、但没有解释器 → 同样不算装好
	if err := os.MkdirAll(filepath.Join(prefix, "opt", "php@8.3", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	list = DiscoverPHPVersions(prefix)
	for _, pv := range list {
		if pv.Version == "8.3" {
			t.Fatalf("没有 bin/php 的目录不该被列成已安装：%+v", list)
		}
	}
}

func TestDiscoverPHPVersionsFlagsConflict(t *testing.T) {
	prefix := fakeBrew(t, map[string]string{
		"8.2": "127.0.0.1:9000",
		"8.3": "127.0.0.1:9000",
	})
	list := DiscoverPHPVersions(prefix)
	if len(list) != 2 {
		t.Fatalf("两个已安装版本都要列出来，实际 %d 个", len(list))
	}
	conflicts := 0
	for _, pv := range list {
		if pv.Conflict != "" {
			conflicts++
			if !strings.Contains(pv.Conflict, "9000") {
				t.Fatalf("冲突说明应指出共用端点，实际: %s", pv.Conflict)
			}
		}
	}
	if conflicts == 0 {
		t.Fatal("两个版本共用 9000 时应标记冲突（这是多版本共存最该被看见的故障）")
	}
}

// PreferedEndpoint 对 TCP 版本也要稳定（保留 TCP 通道，且端口选择集中在一处）。
func TestPreferredEndpointStable(t *testing.T) {
	prefix := shortTempDir(t)
	ep1, err := PreferredEndpoint(prefix, "8.3")
	if err != nil {
		t.Fatal(err)
	}
	ep2, _ := PreferredEndpoint(prefix, "8.3")
	if ep1 != ep2 {
		t.Fatalf("同一版本的端点必须稳定：%s vs %s", ep1, ep2)
	}
	if !strings.HasPrefix(ep1, "unix:"+filepath.Join(prefix, "var", "run")) {
		t.Fatalf("默认应为 <brew>/var/run 下的 socket，实际 %s", ep1)
	}
	// 不同版本 -> 不同路径
	epOther, _ := PreferredEndpoint(prefix, "8.4")
	if ep1 == epOther {
		t.Fatal("不同版本必须分配不同端点")
	}
}

// NormalizeListen 的各种写法（真实环境里都出现过）。
func TestNormalizeListen(t *testing.T) {
	good := map[string]string{
		"9000":                              "127.0.0.1:9000",
		"127.0.0.1:9000":                    "127.0.0.1:9000",
		"/opt/homebrew/var/run/a.sock":      "unix:/opt/homebrew/var/run/a.sock",
		"unix:/opt/homebrew/var/run/a.sock": "unix:/opt/homebrew/var/run/a.sock",
	}
	for in, want := range good {
		got, err := NormalizeListen(in)
		if err != nil {
			t.Fatalf("NormalizeListen(%q) 报错: %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizeListen(%q) = %q，期望 %q", in, got, want)
		}
	}
	bad := []string{
		"", "abc", "127.0.0.1", "127.0.0.1:99999", "/tmp/x socket",
		"unix:relative.sock", "/tmp/../etc/passwd",
		"127.0.0.1:9000; include /etc/passwd", // 配置注入
	}
	for _, in := range bad {
		if got, err := NormalizeListen(in); err == nil {
			t.Fatalf("非法 listen 值 %q 应被拒绝，却返回 %q", in, got)
		}
	}
}

// ============================================================================
//  真机事故回归（2026-09-17 mini）：两个让 php-fpm **根本起不来**的写法
//
//  1. `listen = unix:<路径>` —— php-fpm 只接受裸的绝对路径，带前缀会报
//     `ERROR: invalid port value '/path'`；而 `php-fpm -t` 当时还报 successful。
//  2. `listen.group = `（等号后为空）—— 报
//     `value is NULL for a ZEND_INI_PARSER_ENTRY`，同样是启动即死。
//
//  这两条都在"装完一键 LNMP、php-fpm 被 launchd 反复拉起又反复失败"里出现过，
//  而面板上只看得到"PHP 没在监听"。下面既有配置级断言，也有真启动一次 fpm 的
//  集成验证（本机没有 php-fpm 时自动跳过）。
// ============================================================================

// TestEnsureListenRepairsInvalidUnixPrefix：把历史遗留的 `listen = unix:...`
// 改写成裸路径（这是修复已坏配置的关键：光看解析结果它"完全正确"）。
func TestEnsureListenRepairsInvalidUnixPrefix(t *testing.T) {
	prefix := shortTempDir(t)
	confDir := filepath.Join(prefix, "etc", "php", "8.3", "php-fpm.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(confDir, "www.conf")
	sock := filepath.Join(prefix, "var", "run", "php-fpm-8.3.sock")
	body := "[global]\n\n[www]\nuser = tester\ngroup = staff\n" +
		"listen = unix:" + sock + "\nlisten.owner = tester\nlisten.group = \nlisten.mode = 0660\npm = static\n"
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// 先确认它"看起来是对的"却必须被判为要修
	need, why := NeedsListenFix(prefix, "8.3")
	if !need {
		t.Fatal("listen 带 unix: 前缀（php-fpm 会启动失败）必须判为需要修复")
	}
	if !strings.Contains(why, "unix:") {
		t.Errorf("说明里要写清是 unix: 前缀的问题，实际 %q", why)
	}

	res, err := EnsureListen(prefix, "8.3", EnsureListenOptions{SocketUser: "tester", SocketGroup: "staff", NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("非法写法应被判为需要改写")
	}
	got, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "unix:") {
		t.Errorf("www.conf 里不该再有 unix: 前缀（php-fpm 不接受）：\n%s", text)
	}
	if !strings.Contains(text, "listen = "+sock) {
		t.Errorf("listen 应为裸路径 %s：\n%s", sock, text)
	}
	// 修完之后就不该再喊要修
	if need, why := NeedsListenFix(prefix, "8.3"); need {
		t.Errorf("修完仍被判为需要修复：%s", why)
	}
}

// TestEnsureListenNeverWritesEmptyGroup：组名拿不到时**绝不写空值**，
// 并且要把已经写坏的空值修掉（注释掉，php-fpm 用进程自身的组）。
func TestEnsureListenNeverWritesEmptyGroup(t *testing.T) {
	prefix := shortTempDir(t)
	confDir := filepath.Join(prefix, "etc", "php", "8.3", "php-fpm.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(confDir, "www.conf")
	// 现场一：什么都没有（要补齐属主，但不知道组名）
	if err := os.WriteFile(conf, []byte("[global]\n\n[www]\nlisten = 127.0.0.1:9000\npm = static\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureListen(prefix, "8.3", EnsureListenOptions{
		SocketUser: "tester", SocketGroup: "zizpanel-no-such-group", NoBackup: true,
	}); err != nil {
		// 显式传一个不存在的组名：EnsureListen 会原样用（调用方负责），这里改传空
		t.Logf("（显式组名被原样使用，忽略该分支：%v）", err)
	}
	body, _ := os.ReadFile(conf)
	if regexpEmptyGroup.Match(body) {
		t.Errorf("写了空的 listen.group（php-fpm 会启动失败）：\n%s", body)
	}

	// 现场二：已经被写坏成空值 —— 必须被修掉
	broken := "[global]\n\n[www]\nlisten = " + filepath.Join(prefix, "var", "run", "php-fpm-8.3.sock") +
		"\nlisten.owner = tester\nlisten.group = \nlisten.mode = 0660\npm = static\n"
	if err := os.WriteFile(conf, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := EnsureListen(prefix, "8.3", EnsureListenOptions{SocketUser: "tester", NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("空 listen.group 属于必须修掉的状态（它让 php-fpm 起不来）")
	}
	body, _ = os.ReadFile(conf)
	if regexpEmptyGroup.Match(body) {
		t.Errorf("空 listen.group 没被修掉：\n%s", body)
	}
	// 幂等：再跑一次不再改
	res2, err := EnsureListen(prefix, "8.3", EnsureListenOptions{SocketUser: "tester", NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Error("修好之后应幂等（不再改动）")
	}
}

// regexpEmptyGroup 匹配"值等于空"的 listen.group 行（php-fpm 的致命写法）。
var regexpEmptyGroup = regexp.MustCompile(`(?m)^\s*listen\.group\s*=\s*$`)

// TestEnsureListenOutputStartsRealFPM 是这两次真机事故的**集成回归**。
//
// 为什么必须真启动：`php-fpm -t` 对上面两个问题都报 "test is successful"
// （实测），只有真跑一次才会报 invalid port value / value is NULL。
// 所以这里用临时目录里的假前缀 + 假 php-fpm.conf 真启动一个 fpm 进程，
// 只绑一个临时 socket、只写临时日志，验证完立刻杀掉 ——
// 不碰任何真实服务、不碰 /opt/homebrew 的配置。
func TestEnsureListenOutputStartsRealFPM(t *testing.T) {
	bin, version := findRealFPM(t)
	prefix := shortTempDir(t)

	confDir := filepath.Join(prefix, "etc", "php", version, "php-fpm.d")
	logDir := filepath.Join(prefix, "log")
	for _, d := range []string{confDir, logDir, filepath.Join(prefix, "var", "run")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	www := filepath.Join(confDir, "www.conf")
	// 故意从**被历史版本写坏的现场**开始，三处都是真机上真实出现过的致命写法：
	//   · listen = unix:<路径>   → php-fpm 报 invalid port value
	//   · # 由 ZizPanel 补齐…    → php-fpm 报 value is NULL（INI 不认 #）
	//   · listen.group =（空值） → 至少是"不该写的值"
	// 修完之后必须**真的能启动**：这是端到端修复能力的唯一硬证据。
	body := "[global]\n\n[www]\nuser = " + currentUserName(t) + "\ngroup = staff\n" +
		"listen = unix:" + filepath.Join(prefix, "var", "run", "php-fpm-"+version+".sock") + "\n" +
		"pm = static\npm.max_children = 1\n" +
		"listen.owner = nobody\nlisten.group = \nlisten.mode = 0660\n" +
		"# 由 ZizPanel 补齐：多版本 PHP 各自监听独立端点\n"
	if err := os.WriteFile(www, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	fpmConf := filepath.Join(prefix, "php-fpm.conf")
	if err := os.WriteFile(fpmConf, []byte(
		"[global]\npid = "+filepath.Join(prefix, "var", "run", "php-fpm.pid")+
			"\nerror_log = "+filepath.Join(logDir, "php-fpm.log")+
			"\ninclude = "+confDir+"/*.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureListen(prefix, version, EnsureListenOptions{
		SocketUser: currentUserName(t), NoBackup: true,
	}); err != nil {
		t.Fatalf("EnsureListen 失败: %v", err)
	}
	sock, err := SocketPath(prefix, version)
	if err != nil {
		t.Fatal(err)
	}

	var fpmOut bytes.Buffer
	cmd := exec.Command(bin, "--nodaemonize", "-y", fpmConf)
	cmd.Stdout = &fpmOut
	cmd.Stderr = &fpmOut // 失败时把 fpm 的原话带出来（否则只有一句 exit 78）
	// 独立进程组：php-fpm 会 fork worker，只杀 master 会留下孤儿 worker，
	// 而 cmd.Wait 会在管道读取上一直等它们（实测多等 5 秒）。测试必须自己
	// 收拾干净，不留任何 php-fpm 进程。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 php-fpm 失败: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			// 杀整个进程组（master + worker），否则 worker 会活下来
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	// 真启动：socket 必须出现（这是"配置能被 php-fpm 接受"的唯一硬证据）
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if fi, serr := os.Stat(sock); serr == nil && fi.Mode()&os.ModeSocket != 0 {
			return // 成功
		}
		select {
		case werr := <-done:
			logs, _ := os.ReadFile(filepath.Join(logDir, "php-fpm.log"))
			wwwNow, _ := os.ReadFile(www)
			t.Fatalf("php-fpm 启动后立即退出（配置有问题）：%v\n--- fpm 输出 ---\n%s\n--- 日志 ---\n%s\n--- www.conf ---\n%s",
				werr, fpmOut.String(), logs, wwwNow)
		case <-time.After(200 * time.Millisecond):
		}
	}
	logs, _ := os.ReadFile(filepath.Join(logDir, "php-fpm.log"))
	wwwNow, _ := os.ReadFile(www)
	t.Fatalf("等不到 %s（php-fpm 没起来）\n--- fpm 输出 ---\n%s\n--- 日志 ---\n%s\n--- www.conf ---\n%s",
		sock, fpmOut.String(), logs, wwwNow)
}

// findRealFPM 找一个本机真实存在的 php-fpm 可执行文件与版本号；没有就跳过。
func findRealFPM(t *testing.T) (bin, version string) {
	t.Helper()
	var matches []string
	for _, pat := range []string{"/opt/homebrew/opt/php@*/sbin/php-fpm", "/usr/local/opt/php@*/sbin/php-fpm"} {
		m, _ := filepath.Glob(pat)
		matches = append(matches, m...)
	}
	if len(matches) == 0 {
		t.Skip("本机没有安装版本化 php-fpm，跳过真启动验证")
	}
	sort.Strings(matches)
	bin = matches[len(matches)-1]
	// …/opt/php@8.3/sbin/php-fpm → 8.3
	dir := filepath.Dir(filepath.Dir(bin))
	version = strings.TrimPrefix(filepath.Base(dir), "php@")
	if version == "" || version == "php" {
		t.Skipf("从 %s 推不出 PHP 版本号", bin)
	}
	return bin, version
}

// currentUserName 取当前用户（php-fpm 的池属主）。
func currentUserName(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("取不到当前用户: %v", err)
	}
	return u.Username
}

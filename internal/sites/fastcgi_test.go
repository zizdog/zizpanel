package sites

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreferredFastCGIPass：默认站点/phpMyAdmin 用的端点必须是"当前可用"的
// **专属端点**，绝不能写死 9000。
//
// 真机背景（2026-09-17 mini）：默认站点的 PHP location 曾 include 一份写死
// `fastcgi_pass 127.0.0.1:9000;` 的片段，而多版本设计下每个 php@x.y 听自己的
// Unix socket —— 用户点一次「整理默认站点」就会把它改回 9000 而 502。
func TestPreferredFastCGIPass(t *testing.T) {
	// 没装任何 PHP → 空，且调用方必须据此**不写** fastcgi_pass
	if pass, ver := PreferredFastCGIPass(filepath.Join(t.TempDir(), "nope")); pass != "" || ver != "" {
		t.Errorf("没有 PHP 时应返回空，实际 (%q,%q)", pass, ver)
	}

	// 8.2 配了一个**不存在**的 socket（没在监听），8.3 配了 socket 且真的在监听。
	// 刻意不用 9000 当"没监听"的例子：开发机上 php-fpm 可能真的跑在 9000 上，
	// 那样测试就依赖了真实环境（本项目最贵的一类教训）。
	prefix := shortTempDir(t)
	deadSock := filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	liveSock := filepath.Join(prefix, "var", "run", "php-fpm-8.3.sock")
	if err := writeFakePHPConfForTest(t, prefix, "8.2", deadSock); err != nil {
		t.Fatal(err)
	}
	if err := writeFakePHPConfForTest(t, prefix, "8.3", liveSock); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(liveSock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", liveSock)
	if err != nil {
		t.Fatalf("建测试 socket 失败: %v", err)
	}

	pass, ver := PreferredFastCGIPass(prefix)
	if ver != "8.3" {
		t.Errorf("应挑真的在监听的 8.3，实际版本 %q（pass=%q）", ver, pass)
	}
	if pass != "unix:"+liveSock {
		t.Errorf("端点应为 unix:%s，实际 %q", liveSock, pass)
	}

	// 关掉监听：退回"配置里写着的端点"（按版本升序 → 8.2 的那个 socket）
	_ = ln.Close()
	pass2, ver2 := PreferredFastCGIPass(prefix)
	if ver2 != "8.2" || pass2 != "unix:"+deadSock {
		t.Errorf("都没有监听时应退回配置里的端点（8.2），实际 (%q,%q)", pass2, ver2)
	}
}

// TestFastCGIParamsBlockNoFastcgiPass：参数块**不含 fastcgi_pass**，
// 且与站点 vhost 用的是同一份参数清单（不存在两份会走样的文本）。
func TestFastCGIParamsBlockNoFastcgiPass(t *testing.T) {
	block := FastCGIParamsBlock()
	if strings.Contains(block, "fastcgi_pass") {
		t.Errorf("参数块不该含 fastcgi_pass（端点由调用方按机器解析后写）：\n%s", block)
	}
	if strings.Contains(block, "127.0.0.1:9000") {
		t.Errorf("参数块不该出现写死的 9000：\n%s", block)
	}
	for _, want := range []string{
		"fastcgi_index  index.php;",
		"fastcgi_split_path_info",
		"fastcgi_param  SCRIPT_FILENAME    $request_filename;",
		"fastcgi_param  HTTP_AUTHORIZATION $http_authorization;",
		"fastcgi_read_timeout  300;",
		"fastcgi_buffering     off;",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("参数块缺少 %q：\n%s", want, block)
		}
	}
	// 与站点 vhost 的片段同源：vhost 片段里的每条参数都应出现在块里
	// （缩进/空格数不同是刻意的，比较时按空白归一化）
	norm := func(in string) string { return strings.Join(strings.Fields(in), " ") }
	blockNorm := norm(block)
	for _, line := range strings.Split(fastcgiParams(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "fastcgi_param") {
			continue
		}
		if !strings.Contains(blockNorm, norm(line)) {
			t.Errorf("参数块与站点 vhost 不同源，缺少：%q", line)
		}
	}
}

// writeFakePHPConfForTest 造一个版本的 www.conf（listen 由参数决定）。
func writeFakePHPConfForTest(t *testing.T, prefix, version, listen string) error {
	t.Helper()
	for _, d := range []string{
		filepath.Join(prefix, "opt", "php@"+version, "bin"),
		filepath.Join(prefix, "etc", "php", version, "php-fpm.d"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// 解释器文件也要真存在：DiscoverPHPVersions 现在要求 <opt>/<name>/bin/php
	// 可 stat（否则悬空软链接/残留目录会被当成"装了 PHP"，2026-09-18 用户实测）。
	if err := os.WriteFile(filepath.Join(prefix, "opt", "php@"+version, "bin", "php"),
		[]byte("#!/bin/sh\n"), 0o755); err != nil {
		return err
	}
	body := "[global]\npid = run/php-fpm.pid\n\n[www]\nuser = tester\ngroup = staff\n" +
		"listen = " + listen + "\npm = dynamic\n"
	conf := filepath.Join(prefix, "etc", "php", version, "php-fpm.d", "www.conf")
	return os.WriteFile(conf, []byte(body), 0o644)
}

// TestEveryGeneratedBlockDefinesSchemeVars：**凡引用 $zp_scheme/$zp_https，就必须定义它们。**
//
// 2026-09-18 mini 真机事故（用户报"nginx 无法启动""重装 nginx 也无效"）：
// 默认站点/phpMyAdmin 的参数块引用了 $zp_scheme（REQUEST_SCHEME / HTTPS 两条），
// 却没有那几条 `set` —— 站点 vhost 里恰好有（Generate 走 fastcgiParams()），
// 于是"本机能跑、mini 起不来"：mini 上没有任何站点 vhost 定义过它，
// nginx 直接 `[emerg] unknown "zp_scheme" variable` **起不来**，
// 而配置是面板写的，重装 nginx 当然没用。
//
// 这条门禁遍历**所有**生成器输出，逐块检查"引用即定义"，把这一整类钉死。
func TestEveryGeneratedBlockDefinesSchemeVars(t *testing.T) {
	blocks := map[string]string{
		"FastCGIParamsBlock": FastCGIParamsBlock(),
		"FastCGIParams":      FastCGIParams(),
		"PHPIniFragment":     PHPIniFragment(DefaultLimits()),
	}
	// 站点 vhost 也过一遍（它内部可能有多处引用）
	site := &Site{Domain: "x.test", Root: "/tmp/x", Rewrite: "none", Enabled: true}
	if conf, err := site.Generate(Options{LogDir: "/tmp/logs"}); err == nil {
		blocks["site.Generate"] = conf
	} else {
		t.Fatalf("生成站点配置失败: %v", err)
	}
	for name, body := range blocks {
		usesScheme := strings.Contains(body, "$zp_scheme")
		usesHTTPS := strings.Contains(body, "$zp_https")
		if !usesScheme && !usesHTTPS {
			continue
		}
		if usesScheme && !strings.Contains(body, "set $zp_scheme") {
			t.Errorf("%s 引用了 $zp_scheme 却没有定义它 —— nginx 会报 unknown zp_scheme variable 并**拒绝启动**（mini 真机事故）", name)
		}
		if usesHTTPS && !strings.Contains(body, "set $zp_https") {
			t.Errorf("%s 引用了 $zp_https 却没有定义它 —— 同上", name)
		}
	}
}

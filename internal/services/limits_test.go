package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  上传与执行限制：phpMyAdmin 入口与默认站点生成物必须带上限
//
//  用户报障：phpMyAdmin 导入几十 MB 的 SQL 报 413。默认站点若是**用户自己的**
//  （面板只插一段 phpMyAdmin location、不整份重写），server 级就没有面板的
//  client_max_body_size —— 所以那个 location 里必须自己再写一遍。
// ============================================================================

func TestPMAEntryBlockCarriesClientMaxBodySize(t *testing.T) {
	// 默认：512m
	block := PMAEntryBlock(PMAEntryOptions{Share: "/tmp/pma", FastCGIPass: "unix:/tmp/x.sock", PHPVersion: "8.2"})
	if !strings.Contains(block, "client_max_body_size 512m;") {
		t.Fatalf("phpMyAdmin 入口默认必须带 client_max_body_size 512m：\n%s", block)
	}
	// 自定义值必须被采纳
	block = PMAEntryBlock(PMAEntryOptions{Share: "/tmp/pma", FastCGIPass: "unix:/tmp/x.sock",
		PHPVersion: "8.2", ClientMaxBodySize: "2g"})
	if !strings.Contains(block, "client_max_body_size 2g;") {
		t.Fatalf("自定义上限必须写进 phpMyAdmin 入口：\n%s", block)
	}
	// 非法值退化为默认（绝不许把注入串写进 nginx 配置）
	block = PMAEntryBlock(PMAEntryOptions{Share: "/tmp/pma", FastCGIPass: "unix:/tmp/x.sock",
		PHPVersion: "8.2", ClientMaxBodySize: "1m; } server { listen 9999; "})
	if strings.Contains(block, "listen 9999") {
		t.Fatalf("非法上限被写进了 nginx 配置（注入面）：\n%s", block)
	}
	if !strings.Contains(block, "client_max_body_size 512m;") {
		t.Fatalf("非法上限应退化为默认 512m：\n%s", block)
	}
	// 安全指令与上限必须同时存在（三份生成器合并后的不变量）
	for _, want := range []string{"allow 127.0.0.1;", "allow ::1;", "deny all;", "alias /tmp/pma;"} {
		if !strings.Contains(block, want) {
			t.Errorf("入口缺少 %q：\n%s", want, block)
		}
	}
}

// TestPMADefaultVhostCarriesLimits：一键 LNMP 收尾写的最小默认站点也要带上限。
func TestPMADefaultVhostCarriesLimits(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{opt: Options{
		BrewBin:  filepath.Join(dir, "brew", "bin", "brew"),
		UserHome: filepath.Join(dir, "home"),
		UserName: "tester",
		UploadLimits: sites.Limits{ClientMaxBodySize: "1g", UploadMaxFilesize: "1G",
			PostMaxSize: "1G", MemoryLimit: "512M", MaxExecutionTime: 600},
	}}
	content := m.pmaDefaultVhostContent(
		filepath.Join(dir, "home", "www"),
		filepath.Join(dir, "home", "www", "localhost"),
		"unix:/tmp/php-fpm-8.2.sock", "8.2")
	if !strings.Contains(content, "client_max_body_size 1g;") {
		t.Fatalf("默认站点 server 级必须带 client_max_body_size 1g：\n%s", content)
	}
	// phpMyAdmin 入口里也要有一份 1g
	if n := strings.Count(content, "client_max_body_size 1g;"); n != 2 {
		t.Fatalf("默认站点应有 2 处上限（server 级 + phpMyAdmin location），实际 %d：\n%s", n, content)
	}
	// 没设限制时退回默认 512m（调用的 Manager 可能来自旧配置）
	m2 := &Manager{opt: Options{BrewBin: filepath.Join(dir, "brew", "bin", "brew"),
		UserHome: filepath.Join(dir, "home"), UserName: "tester"}}
	if got := m2.pmaDefaultVhostContent(dir, filepath.Join(dir, "localhost"), "unix:/tmp/x.sock", "8.2"); !strings.Contains(got, "client_max_body_size 512m;") {
		t.Fatalf("未配置限制时默认站点应写 512m：\n%s", got)
	}
}

// TestPMALimitsAreNormalizedInInstallPath：安装路径（pmaDefaultVhostContent）与
// pmaConfig 用的是同一份 limits（Options.UploadLimits）。
func TestPMALimitsAreNormalizedInInstallPath(t *testing.T) {
	conf := pmaConfig("0123456789abcdef0123456789abcdef", "/tmp/pma", false, 600)
	if !strings.Contains(conf, "$cfg['ExecTimeLimit']                  = 600;") {
		t.Fatalf("phpMyAdmin 配置必须带与面板一致的 ExecTimeLimit：\n%s", conf)
	}
	// 非法/空值退回默认 300
	conf = pmaConfig("0123456789abcdef0123456789abcdef", "/tmp/pma", false, 0)
	if !strings.Contains(conf, "$cfg['ExecTimeLimit']                  = 300;") {
		t.Fatalf("ExecTimeLimit 为空时应退回默认 300：\n%s", conf)
	}
}

// TestSyncPhpMyAdminExecTimeLimit：改面板限制后，phpMyAdmin 那份配置必须跟着改，
// 否则"面板说改好了、导入还是超时"。幂等、缺行时追加、没装时无操作。
func TestSyncPhpMyAdminExecTimeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "phpmyadmin.config.inc.php")

	// 没装 phpMyAdmin：无操作、不报错、不创建文件
	if changed, err := SyncPhpMyAdminExecTimeLimit(path, 600, ""); err != nil || changed {
		t.Fatalf("配置不存在时应 (false,nil)，实际 changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("配置不存在时不该创建文件")
	}

	// 已有该行：改值 → true；同值 → false
	base := pmaConfig("0123456789abcdef0123456789abcdef", "/tmp/pma", false, 300)
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := SyncPhpMyAdminExecTimeLimit(path, 600, "")
	if err != nil || !changed {
		t.Fatalf("应改值为 600，实际 changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "= 600;") {
		t.Fatalf("文件里没有 600：\n%s", b)
	}
	// 不能碰别的行
	if strings.Count(string(b), "blowfish_secret") != 1 ||
		!strings.Contains(string(b), "$cfg['Servers'][$i]['auth_type']       = 'cookie';") {
		t.Fatalf("同步影响了别的配置行：\n%s", b)
	}
	if changed, err := SyncPhpMyAdminExecTimeLimit(path, 600, ""); err != nil || changed {
		t.Fatalf("同值同步必须幂等，实际 changed=%v err=%v", changed, err)
	}

	// 旧配置没有这一行：追加（而不是报错）
	legacy := "<?php\n$cfg['blowfish_secret'] = 'abc';\n$cfg['ServerDefault'] = 1;\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err = SyncPhpMyAdminExecTimeLimit(path, 900, "")
	if err != nil || !changed {
		t.Fatalf("缺行时应追加，实际 changed=%v err=%v", changed, err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "$cfg['ExecTimeLimit']") || !strings.Contains(string(b), "= 900;") {
		t.Fatalf("追加失败：\n%s", b)
	}
	if !strings.Contains(string(b), "abc") {
		t.Fatalf("追加时弄丢了原有内容：\n%s", b)
	}
	// 复核：同值再同步幂等
	if changed, err := SyncPhpMyAdminExecTimeLimit(path, 900, ""); err != nil || changed {
		t.Fatalf("追加后同值同步应幂等，实际 changed=%v err=%v", changed, err)
	}
}

// TestPhpMyAdminConfPathMatchesPmaPaths：导出的路径推导必须与 pmaPaths 一致
// （web 层用它同步 ExecTimeLimit，两处不一致会改到不存在的文件）。
func TestPhpMyAdminConfPathMatchesPmaPaths(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{opt: Options{BrewBin: filepath.Join(dir, "brew", "bin", "brew")}}
	if got, want := PhpMyAdminConfPath(filepath.Join(dir, "brew")), m.pmaPaths().ConfReal; got != want {
		t.Fatalf("PhpMyAdminConfPath=%s，pmaPaths().ConfReal=%s", got, want)
	}
}

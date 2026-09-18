package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  默认站点的 PHP 端点必须是"该机器当前版本"的专属端点
//
//  真机背景（2026-09-17 mini）：`buildDefaultVhost` 过去往 PHP location 里
//  `include <brew>/etc/nginx/includes/php-fpm.conf`，而那份片段写死
//  `fastcgi_pass 127.0.0.1:9000;` —— 在"每个 php@x.y 只听自己专属 Unix socket"
//  的机器上，用户点一次「整理默认站点」就会把默认站点改回 9000 而 **502**，
//  等于把一键 LNMP 刚修好的东西弄坏。
//
//  现在端点解析与参数文本都来自 internal/sites（与 services 侧共用同一份）。
// ============================================================================

// TestBuildDefaultVhostUsesSocketEndpoint：生成的默认站点必须指向专属 socket，
// 且**不含**写死的 9000，也不再 include 那份片段。
func TestBuildDefaultVhostUsesSocketEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	seedFakePHP(t, srv.Cfg.BrewPrefix, "8.2")
	// 真实面板里这一步由「修复端点」或一键 LNMP 完成：把 8.2 改成专属 socket
	if _, err := sites.EnsureListen(srv.Cfg.BrewPrefix, "8.2", sites.EnsureListenOptions{
		SocketUser: "tester", NoBackup: true,
	}); err != nil {
		t.Fatalf("把假 PHP 改成 socket 端点失败: %v", err)
	}

	content := srv.buildDefaultVhost()
	sock := filepath.Join(srv.Cfg.BrewPrefix, "var", "run", "php-fpm-8.2.sock")
	if !strings.Contains(content, "fastcgi_pass unix:"+sock+";") {
		t.Errorf("默认站点的 PHP 必须指向专属 socket unix:%s：\n%s", sock, content)
	}
	if strings.Contains(content, "127.0.0.1:9000") {
		t.Errorf("默认站点不许出现写死的 9000（多版本下没人监听）：\n%s", content)
	}
	if strings.Contains(content, "includes/php-fpm.conf") {
		t.Errorf("不该再 include 那份写死 9000 的片段（这正是要修的 bug）：\n%s", content)
	}
	// 用户 2026-09-18 明确要求："默认站点是纯静态的，只需要一个 index.html" ——
	// 所以 index.html 必须排在前面（面板也不再往默认站点里放 index.php）。
	if !strings.Contains(content, "index  index.html index.php;") {
		t.Errorf("index.html 应排在 index.php 前面（默认站点是纯静态的）：\n%s", content)
	}
	// 默认站点 + phpMyAdmin 各一处
	if n := strings.Count(content, "fastcgi_pass"); n != 2 {
		t.Errorf("应有且仅有两处 fastcgi_pass，实际 %d：\n%s", n, content)
	}
	if strings.Count(content, "{") != strings.Count(content, "}") {
		t.Errorf("花括号不配对，nginx 会拒绝加载：\n%s", content)
	}
	// 面板「整理默认站点」会复核首页含这段标记，不能弄丢
	if _, err := os.Stat(filepath.Join(srv.Cfg.WWWRoot, "localhost", "index.html")); err == nil {
		if !strings.Contains(content, filepath.Join(srv.Cfg.WWWRoot, "localhost")) {
			t.Errorf("默认站点的 root 应指向 %s：\n%s", filepath.Join(srv.Cfg.WWWRoot, "localhost"), content)
		}
	}
}

// TestBuildDefaultVhostWithoutPHP：本机没有可用 PHP 时**不能**写出非法配置。
//
// `fastcgi_pass ;` 是非法指令，会让整个 nginx 拒绝加载（连静态站点一起挂掉）——
// 这种情况必须退化成"纯静态 + 一行说明"，而不是留个残破的 location。
func TestBuildDefaultVhostWithoutPHP(t *testing.T) {
	srv, _ := newTestServer(t) // 不 seed：本机没有 php@*
	content := srv.buildDefaultVhost()
	if strings.Contains(content, "fastcgi_pass") {
		t.Errorf("没有 PHP 端点时不该写 fastcgi_pass：\n%s", content)
	}
	if !strings.Contains(content, "没有可用 PHP 端点") {
		t.Errorf("应当写明「本机没有可用 PHP 端点」，让用户知道为什么 PHP 不解析：\n%s", content)
	}
	if strings.Count(content, "{") != strings.Count(content, "}") {
		t.Errorf("花括号不配对：\n%s", content)
	}
}

// TestFastCGIParamsSingleSource：端点解析与参数文本**只有一份**（读源码锁调用点）。
//
// 两份参数必然走样（本项目刚因为"一处写死 9000、一处用 socket"而踩过），
// 所以这里直接断言两个调用点都走 internal/sites 的导出。
func TestFastCGIParamsSingleSource(t *testing.T) {
	webSrc, err := os.ReadFile("api_default_site.go")
	if err != nil {
		t.Fatal(err)
	}
	web := string(webSrc)
	for _, want := range []string{"sites.PreferredFastCGIPass", "sites.FastCGIParamsBlock"} {
		if !strings.Contains(web, want) {
			t.Errorf("buildDefaultVhost 必须用 %s（端点/参数收敛在 sites 包）", want)
		}
	}
	for _, bad := range []string{`"includes", "php-fpm.conf"`, "fpmIncludeContent"} {
		if strings.Contains(web, bad) {
			t.Errorf("默认站点不该再依赖 %s（那份片段写死 9000）", bad)
		}
	}

	svcSrc, err := os.ReadFile(filepath.Join("..", "services", "phpmyadmin.go"))
	if err != nil {
		t.Fatal(err)
	}
	svc := string(svcSrc)
	for _, want := range []string{"sites.PreferredFastCGIPass", "sites.FastCGIParamsBlock"} {
		if !strings.Contains(svc, want) {
			t.Errorf("services 侧也必须用 %s（与默认站点共用同一份实现）", want)
		}
	}
}

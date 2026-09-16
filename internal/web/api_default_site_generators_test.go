package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  完整默认站点（「整理默认站点」那份模板）与 phpMyAdmin 入口
//
//  D14 要的是"三份入口生成器合并成一份"。services 侧的两份由
//  internal/services/phpmyadmin_entry_test.go 锁；这里锁第三份
//  （internal/web 的 buildDefaultVhost）也必须原样嵌入 services.PMAEntryBlock，
//  且带统一的默认站点标记（D44）。
//
//  沙箱约定：newTestServer 的 brew 前缀是临时目录，buildDefaultVhost 只生成字符串；
//  测试仍然显式断言生产的 000-default.conf 一字未变（AGENTS.md 硬性约定）。
// ============================================================================

// TestBuildDefaultVhostEmbedsSharedPMAEntry：完整模板必须用唯一的入口生成器。
func TestBuildDefaultVhostEmbedsSharedPMAEntry(t *testing.T) {
	const prod = "/opt/homebrew/etc/nginx/vhosts/000-default.conf"
	before, beforeErr := os.ReadFile(prod)

	srv, _ := newTestServer(t)
	seedFakePHP(t, srv.Cfg.BrewPrefix, "8.2")
	if _, err := sites.EnsureListen(srv.Cfg.BrewPrefix, "8.2", sites.EnsureListenOptions{
		SocketUser: "tester", NoBackup: true,
	}); err != nil {
		t.Fatalf("把假 PHP 改成 socket 端点失败: %v", err)
	}

	content := srv.buildDefaultVhost()
	pass, version := sites.PreferredFastCGIPass(srv.Cfg.BrewPrefix)
	if pass == "" {
		t.Fatal("沙箱里应当能解析出 PHP 端点")
	}
	want := services.PMAEntryBlock(services.PMAEntryOptions{
		Share:       filepath.Join(srv.Cfg.BrewPrefix, "share", "phpmyadmin"),
		FastCGIPass: pass,
		PHPVersion:  version,
	})
	if !strings.Contains(content, want) {
		t.Errorf("完整默认站点没有原样嵌入 services.PMAEntryBlock（三份生成器必须合并成一份）：\n"+
			"--- content ---\n%s\n--- want ---\n%s", content, want)
	}
	// 安全指令必须在（D14），标记必须统一（D44）。
	for _, d := range []string{
		"allow 127.0.0.1;",
		"allow ::1;",
		"deny all;",
		"return 301 /phpmyadmin/;",
		services.DefaultVhostMarker,
		services.DefaultVhostKindFull,
	} {
		if !strings.Contains(content, d) {
			t.Errorf("完整默认站点缺少 %q：\n%s", d, content)
		}
	}
	if !services.IsZizPanelDefaultVhost(content) {
		t.Error("完整默认站点必须能被 services 认成「面板创建的」（D44）")
	}
	if strings.Count(content, "{") != strings.Count(content, "}") {
		t.Errorf("花括号不配对：\n%s", content)
	}

	// 沙箱护栏：生产 vhost 一字未变。
	after, afterErr := os.ReadFile(prod)
	if beforeErr != nil || afterErr != nil {
		if os.IsNotExist(beforeErr) && os.IsNotExist(afterErr) {
			t.Skip("本机没有生产 000-default.conf，跳过内容比对")
		}
		t.Fatalf("读取生产 vhost 失败：before=%v after=%v", beforeErr, afterErr)
	}
	if string(before) != string(after) {
		t.Fatal("生产 000-default.conf 被测试改动了！这正是 2026-09-14 那次事故的形态")
	}
}

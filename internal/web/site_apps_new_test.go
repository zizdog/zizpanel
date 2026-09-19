package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  一键建站：目录声明 + 前置条件检查（Flarum / emlog / 可道云）
// ============================================================================

// TestSiteAppCatalogDeclarationsAreComplete 遍历目录里的**每一个**一键建站条目，
// 锁住"声明完整、伪静态预设存在"这两条不变量（新增站点应用自动被覆盖）。
func TestSiteAppCatalogDeclarationsAreComplete(t *testing.T) {
	n := 0
	for _, app := range services.Catalog() {
		if app.SiteApp == nil {
			continue
		}
		n++
		spec := app.SiteApp
		if strings.TrimSpace(spec.Rewrite) == "" {
			t.Errorf("%s: SiteApp.Rewrite 为空（必须指向一个伪静态预设）", app.ID)
		} else if _, ok := sites.RewritePresetByName(spec.Rewrite); !ok {
			t.Errorf("%s: 声明的伪静态预设 %q 在 sites.RewritePresets 里不存在", app.ID, spec.Rewrite)
		}
		if !strings.HasPrefix(spec.FinishPath, "/") {
			t.Errorf("%s: FinishPath 必须以 / 开头，实际 %q", app.ID, spec.FinishPath)
		}
		if spec.Archive != "zip" && spec.Archive != "tar.gz" {
			t.Errorf("%s: Archive 只支持 zip / tar.gz，实际 %q", app.ID, spec.Archive)
		}
		if spec.DownloadURL == "" {
			t.Errorf("%s: 没有 DownloadURL", app.ID)
		}
	}
	if n < 5 {
		t.Fatalf("只检查到 %d 个一键建站条目，明显偏少", n)
	}
}

// TestNewSiteAppsHavePreflightRequirements 锁住新增三个站点应用必须在目录里声明
// PHP 最低版本与必需扩展（缺了会被静默装出一个打不开的站点）。
func TestNewSiteAppsHavePreflightRequirements(t *testing.T) {
	for _, id := range []string{"flarum", "emlog", "kodbox"} {
		app, ok := services.FindApp(id)
		if !ok || app.SiteApp == nil {
			t.Fatalf("%s 应是一键建站条目", id)
		}
		if app.SiteApp.MinPHP == "" {
			t.Errorf("%s: 必须声明 MinPHP", id)
		}
		if len(app.SiteApp.PHPExts) == 0 {
			t.Errorf("%s: 必须声明 PHPExts", id)
		}
		if !app.SiteApp.NeedsDB {
			t.Errorf("%s: 需要 MySQL，NeedsDB 应为 true", id)
		}
	}
	// 顶层目录剥离：flarum/emlog 无顶层目录；kodbox 有 kodbox-<ver>/。
	if a, _ := services.FindApp("flarum"); a.SiteApp.StripTopDir {
		t.Error("flarum 包内无单一顶层目录，不该剥")
	}
	if a, _ := services.FindApp("emlog"); a.SiteApp.StripTopDir {
		t.Error("emlog 包内无单一顶层目录，不该剥")
	}
	if a, _ := services.FindApp("kodbox"); !a.SiteApp.StripTopDir {
		t.Error("kodbox 归档内有一层 kodbox-<ver>/，必须剥")
	}
}

// writeFakePHP 在沙箱 brew 前缀下放一个假 php，只实现 `-r` 与 `-m`。
func writeFakePHP(t *testing.T, brewPrefix, version, fullVersion string, mods []string) {
	t.Helper()
	dir := filepath.Join(brewPrefix, "opt", "php@"+version, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("case \"$1\" in\n")
	b.WriteString("  -r) echo " + fullVersion + " ;;\n")
	b.WriteString("  -m) printf '[PHP Modules]\\n" + strings.Join(mods, "\\n") + "\\n\\n[Zend Modules]\\n' ;;\n")
	b.WriteString("esac\n")
	if err := os.WriteFile(filepath.Join(dir, "php"), []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestSitePreflightChecksVersionAndExtensions 用**假 php 二进制**验证前置检查，
// 不依赖本机真的装了哪个 PHP 版本。
func TestSitePreflightChecksVersionAndExtensions(t *testing.T) {
	srv, _ := newTestServer(t)
	brew := t.TempDir()
	srv.Cfg.BrewPrefix = brew
	srv.Cfg.PHPSvc = "php@8.2"
	srv.Cfg.WWWRoot = filepath.Join(t.TempDir(), "www")
	writeFakePHP(t, brew, "8.2", "8.2.33", []string{"curl", "gd", "mbstring"})

	okSpec := &services.SiteAppSpec{MinPHP: "8.2", PHPExts: []string{"curl", "gd"}}
	if err := checkSiteAppRequirements(srv, "测试站", okSpec, "8.2"); err != nil {
		t.Fatalf("满足条件时不该拒绝：%v", err)
	}
	// PHP 版本太低 → 拒绝，且要说清缺什么。
	lowSpec := &services.SiteAppSpec{MinPHP: "8.3"}
	err := checkSiteAppRequirements(srv, "测试站", lowSpec, "8.2")
	if err == nil || !strings.Contains(err.Error(), "8.3") {
		t.Fatalf("低版本 PHP 必须被拒且说明要求：%v", err)
	}
	// 缺扩展 → 拒绝，且点名缺哪个。
	missingSpec := &services.SiteAppSpec{PHPExts: []string{"pdo_mysql", "zip"}}
	err = checkSiteAppRequirements(srv, "测试站", missingSpec, "8.2")
	if err == nil || !strings.Contains(err.Error(), "pdo_mysql") || !strings.Contains(err.Error(), "zip") {
		t.Fatalf("缺扩展必须被拒且列出缺失项：%v", err)
	}
	// 该版本 PHP 没装 → 拒绝。
	if err := checkSiteAppRequirements(srv, "测试站", okSpec, "9.9"); err == nil {
		t.Fatal("找不到该版本 php 时必须拒绝")
	}
}

// TestSiteInstallRejectsBeforeCreatingDir 锁住"前置检查失败发生在建目录之前"。
func TestSiteInstallRejectsBeforeCreatingDir(t *testing.T) {
	srv, _ := newTestServer(t)
	prev := sitePHPPreflightFn
	sitePHPPreflightFn = func(*Server, string, *services.SiteAppSpec, string) error {
		return errors.New("当前 PHP 8.2 缺少必需扩展：pdo_mysql")
	}
	t.Cleanup(func() { sitePHPPreflightFn = prev })

	app, ok := services.FindApp("flarum")
	if !ok || app.SiteApp == nil {
		t.Fatal("目录里应有 flarum 一键建站条目")
	}
	domain := "flarum-preflight.test"
	_, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{PHP: "8.2"})
	if err == nil || !strings.Contains(err.Error(), "pdo_mysql") {
		t.Fatalf("前置检查失败必须原样报出缺什么：%v", err)
	}
	dir, derr := sites.SiteDir(srv.Cfg.WWWRoot, domain)
	if derr != nil {
		t.Fatal(derr)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("被前置检查拒绝时不该创建站点目录 %s", dir)
	}
}

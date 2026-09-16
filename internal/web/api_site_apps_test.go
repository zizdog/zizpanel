package web

import (
	"archive/zip"
	"bytes"
	"context"
	crand "crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  一键建站（D34 源码镜像 + D23 回滚）的 web 层契约
//
//  单测纪律（AGENTS.md 第三节）：
//    · 不碰真实 MySQL（用 NeedsDB=false 的目录条目副本绕开建库）；
//    · 不碰真实 nginx（siteWriteVhostFn / siteReloadFn / siteProbeFn 注入假实现）；
//    · 不联网（sitePackageFetchFn / siteHomeProbeFn 注入；未登记应用那条回落
//      路径打到 httptest 本地服务器，仍是沙箱内）；
//    · 站点目录全部落在 newTestServer 的 WWWRoot（t.TempDir）下。
// ============================================================================

// makeSiteZip 造一个**大于 1KB**的合法 zip（installSiteApp 会拒绝 <1024 字节的产物）。
func makeSiteZip(t *testing.T, topDir string, pad int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := ""
	if topDir != "" {
		prefix = topDir + "/"
	}
	f, err := zw.Create(prefix + "index.php")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("<?php // zizpanel test site\n"))
	g, err := zw.Create(prefix + "padding.txt")
	if err != nil {
		t.Fatal(err)
	}
	// 必须是**不可压缩**的随机字节：重复字符会被 deflate 压到几百字节，
	// 产物就过不了 installSiteApp 的 "<1024 字节 = 不完整" 检查。
	padBytes := make([]byte, pad)
	if _, err := crand.Read(padBytes); err != nil {
		t.Fatal(err)
	}
	_, _ = g.Write(padBytes)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 1024 {
		t.Fatalf("测试用 zip 太小（%d 字节），会被 installSiteApp 当成不完整", buf.Len())
	}
	return buf.Bytes()
}

// stubSitePackageFetch 把源码包下载换成"写一个本地 zip"，并返回固定的来源标签。
func stubSitePackageFetch(t *testing.T, label string, payload []byte) {
	t.Helper()
	prev := sitePackageFetchFn
	sitePackageFetchFn = func(_ *Server, _ context.Context, appID, dest string,
		_ func(string)) (services.SiteSource, string, error) {
		if err := os.WriteFile(dest, payload, 0o644); err != nil {
			return services.SiteSource{}, "", err
		}
		if appID == "typecho" {
			return services.SiteSource{
				App: "typecho", Name: "Typecho", Version: "1.3.0", File: "typecho.zip",
			}, label, nil
		}
		return services.SiteSource{App: appID, Name: appID, Version: "test", File: "x.zip"}, label, nil
	}
	t.Cleanup(func() { sitePackageFetchFn = prev })
}

// stubSiteApplySteps 注入 applySite 的三步与首页探测，避免碰真实 nginx/网络。
// writeErr 非 nil 时模拟"vhost 写不进去"，用来触发 D23 回滚分支。
func stubSiteApplySteps(t *testing.T, writeErr error) {
	t.Helper()
	prevWrite, prevReload, prevProbe := siteWriteVhostFn, siteReloadFn, siteProbeFn
	prevHome := siteHomeProbeFn
	siteWriteVhostFn = func(_ *Server, _ context.Context, _, _ string) error { return writeErr }
	siteReloadFn = func(_ *Server, _ context.Context) error { return nil }
	siteProbeFn = func(_ context.Context, _, _ string, _ int, _ string, _ time.Duration) (string, string, error) {
		return "403", "", nil
	}
	siteHomeProbeFn = func(_ context.Context, _ string) (int, string) { return 200, "200" }
	t.Cleanup(func() {
		siteWriteVhostFn, siteReloadFn, siteProbeFn = prevWrite, prevReload, prevProbe
		siteHomeProbeFn = prevHome
	})
}

// typechoAppNoDB 拿目录里的 Typecho 条目，但把 NeedsDB 关掉 ——
// 单测不许连真实 MySQL，建库那一段不是本测试的对象。
func typechoAppNoDB(t *testing.T) services.App {
	t.Helper()
	app, ok := services.FindApp("typecho")
	if !ok || app.SiteApp == nil {
		t.Fatal("目录里应有 typecho 的一键建站条目")
	}
	spec := *app.SiteApp
	spec.NeedsDB = false
	app.SiteApp = &spec
	return app
}

// typechoAppWithDB 同上，但保留 NeedsDB（配合 fakeMySQLClient 使用）。
func typechoAppWithDB(t *testing.T) services.App {
	t.Helper()
	app := typechoAppNoDB(t)
	spec := *app.SiteApp
	spec.NeedsDB = true
	app.SiteApp = &spec
	return app
}

// fakeMySQLClient 在沙箱 BrewPrefix/bin 下放一个"永远成功"的 mysql 假客户端。
//
// 为什么可行：internal/mysql 的 exec 是 shell out 到 <BinDir>/mysql，
// 换掉这个可执行文件就等于换掉了"真实 MySQL"，全程不碰用户的数据库。
// 这样 NeedsDB 这条路径（建库 → 建用户 → 记住口令）才能在沙箱里整条跑通。
func fakeMySQLClient(t *testing.T, srv *Server) {
	t.Helper()
	binDir := filepath.Join(srv.Cfg.BrewPrefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "mysql"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestSiteInstallRemembersDBPassword 是"一键建站口令可回显"那条需求的护栏：
// CREATE USER 成功后必须把口令交给 RememberDBPassword，且能被 RecallDBPassword 读回。
func TestSiteInstallRemembersDBPassword(t *testing.T) {
	srv, _ := newTestServer(t)
	fakeMySQLClient(t, srv)
	stubSiteApplySteps(t, nil)
	stubSitePackageFetch(t, "NAS 镜像", makeSiteZip(t, "typecho", 2048))

	app := typechoAppWithDB(t)
	domain := "dbcred.test"
	res, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{})
	if err != nil {
		t.Fatalf("建站应成功（假 mysql 永远返回 0）：%v", err)
	}
	if res.DBUser == "" || res.DBPass == "" {
		t.Fatalf("结果里应有库账号与口令，实际 user=%q pass=%q", res.DBUser, res.DBPass)
	}
	pw, known := srv.RecallDBPassword(context.Background(), res.DBUser, "localhost")
	if !known {
		t.Fatalf("口令应已被面板记住（用户 %s）", res.DBUser)
	}
	if pw != res.DBPass {
		t.Errorf("回显的口令与建站时的不一致：got %q want %q", pw, res.DBPass)
	}
	if strings.Contains(strings.Join(res.Steps, "\n"), "未能保存口令") {
		t.Errorf("记住口令不应失败，实际步骤：%q", strings.Join(res.Steps, "\n"))
	}
}

// TestSiteInstallRollsBackRecordWhenApplyFails 是缺陷 D23 的回归护栏：
// 站点记录先落库、applySite 失败后必须回滚，不能留下"记录在、站点打不开"。
func TestSiteInstallRollsBackRecordWhenApplyFails(t *testing.T) {
	srv, _ := newTestServer(t)
	stubSiteApplySteps(t, os.ErrPermission)
	stubSitePackageFetch(t, "NAS 镜像", makeSiteZip(t, "typecho", 2048))

	app := typechoAppNoDB(t)
	domain := "rollback.test"
	res, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{})
	if err == nil {
		t.Fatal("applySite 失败时建站必须失败")
	}
	if !strings.Contains(err.Error(), "生成 nginx 配置失败") {
		t.Errorf("错误应说明是生成 nginx 配置失败，实际：%v", err)
	}
	if _, gerr := srv.siteMgr().Get(context.Background(), domain); gerr == nil {
		t.Errorf("applySite 失败后站点记录 %s 必须被回滚删除", domain)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "已回滚站点记录") {
		t.Errorf("任务日志要如实写出已回滚，实际步骤：%q", strings.Join(res.Steps, "\n"))
	}
}

// TestSiteInstallReportsPinnedVersionAndSource：成功路径要如实上报
// "装的是哪个固定版本、实际走的哪个源"，并且站点记录确实存在。
func TestSiteInstallReportsPinnedVersionAndSource(t *testing.T) {
	srv, _ := newTestServer(t)
	stubSiteApplySteps(t, nil)
	stubSitePackageFetch(t, "NAS 镜像", makeSiteZip(t, "typecho", 2048))

	app := typechoAppNoDB(t)
	domain := "pinned.test"
	res, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{})
	if err != nil {
		t.Fatalf("建站应成功：%v", err)
	}
	if res.Version != "1.3.0" {
		t.Errorf("结果里应带固定版本 1.3.0，实际 %q", res.Version)
	}
	if res.Source != "NAS 镜像" {
		t.Errorf("结果里应带真实来源，实际 %q", res.Source)
	}
	steps := strings.Join(res.Steps, "\n")
	if !strings.Contains(steps, "源码包已就绪") || !strings.Contains(steps, "NAS 镜像") {
		t.Errorf("任务日志要写明版本与来源，实际：%q", steps)
	}
	if _, gerr := srv.siteMgr().Get(context.Background(), domain); gerr != nil {
		t.Errorf("成功建站后站点记录应存在，实际：%v", gerr)
	}
	// 站点目录里应真的有源码（复核真实状态，不看退出码）。
	if _, serr := os.Stat(filepath.Join(res.Dir, "index.php")); serr != nil {
		t.Errorf("站点目录里应解压出 index.php，实际：%v", serr)
	}
}

// TestSiteInstallUnregisteredAppKeepsCatalogFallback：未登记固定版本的站点应用
// 仍走目录条目里的地址（老行为），证明改造没有把这条路堵死。
func TestSiteInstallUnregisteredAppKeepsCatalogFallback(t *testing.T) {
	payload := makeSiteZip(t, "unregsite", 2048)
	zipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	}))
	defer zipSrv.Close()

	srv, _ := newTestServer(t)
	stubSiteApplySteps(t, nil)

	app := services.App{
		ID: "unregsite", Name: "Unregistered Site",
		SiteApp: &services.SiteAppSpec{
			DownloadURL: zipSrv.URL + "/unregsite.zip", Archive: "zip",
			StripTopDir: true, Rewrite: "none", FinishPath: "/install.php",
			NeedsDB: false,
		},
	}
	domain := "catalog.test"
	res, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{})
	if err != nil {
		t.Fatalf("未登记应用应能用目录条目地址装好：%v", err)
	}
	if res.Source != "" {
		t.Errorf("未登记应用没有镜像来源，Source 应为空，实际 %q", res.Source)
	}
	if _, serr := os.Stat(filepath.Join(res.Dir, "index.php")); serr != nil {
		t.Errorf("站点目录里应解压出 index.php，实际：%v", serr)
	}
}

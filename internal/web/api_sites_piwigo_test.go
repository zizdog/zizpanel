package web

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/mysql"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  Piwigo 一键建站的契约（声明 / 库名 / 门禁 / 回滚 / 收尾判据 / 卸载）
//
//  单测纪律照 api_site_apps_test.go：不联网、不碰真实 MySQL、不碰真实 nginx。
// ============================================================================

// TestPiwigoDeclarationIsComplete 锁住声明完整：固定地址、伪静态预设存在、PHP 8.2 门槛。
func TestPiwigoDeclarationIsComplete(t *testing.T) {
	app, ok := siteAppByID(piwigoAppID)
	if !ok || app.SiteApp == nil {
		t.Fatal("Piwigo 应已登记为一键建站应用")
	}
	spec := app.SiteApp
	if spec.Archive != "zip" {
		t.Errorf("Piwigo 官方发行包是 zip，Archive 应为 zip，实际 %q", spec.Archive)
	}
	if !spec.StripTopDir {
		t.Error("官方 zip 里有一层顶层目录，StripTopDir 必须为 true")
	}
	if _, ok := sites.RewritePresetByName(spec.Rewrite); !ok {
		t.Errorf("伪静态预设 %q 不存在", spec.Rewrite)
	}
	if !strings.HasPrefix(spec.FinishPath, "/") {
		t.Errorf("FinishPath 必须以 / 开头，实际 %q", spec.FinishPath)
	}
	if !spec.NeedsDB {
		t.Error("Piwigo 需要 MySQL，NeedsDB 应为 true")
	}
	if spec.MinPHP != "8.2" {
		t.Errorf("Piwigo 官方要求 PHP 8.2+，MinPHP 应为 8.2，实际 %q", spec.MinPHP)
	}
	if len(spec.PHPExts) == 0 {
		t.Error("必须声明 PHPExts（缺扩展会装出一个打不开的站点）")
	}
	if !strings.Contains(spec.DownloadURL, "piwigo.org") {
		t.Errorf("发行包应来自官方域名 piwigo.org，实际 %q", spec.DownloadURL)
	}
	if strings.Contains(spec.DownloadURL, "latest") {
		t.Errorf("下载地址不能追 latest（无法登记 sha256）：%q", spec.DownloadURL)
	}
}

// TestPiwigoDeclarationMatchesCatalog：目录里出现同 ID 条目后，两份声明必须逐字段一致。
//
// 目录条目由市场侧合入；这条门禁保证"合进来之后不会与这里悄悄漂移"。
func TestPiwigoDeclarationMatchesCatalog(t *testing.T) {
	cat, ok := services.FindApp(piwigoAppID)
	if !ok {
		t.Skip("目录里还没有 piwigo 条目（等市场侧合入）；web 层登记已可用")
	}
	if cat.SiteApp == nil {
		t.Fatal("目录里的 piwigo 条目必须是 SiteApp")
	}
	mine := piwigoExtraApp().SiteApp
	for _, c := range []struct{ name, want, got string }{
		{"DownloadURL", mine.DownloadURL, cat.SiteApp.DownloadURL},
		{"Archive", mine.Archive, cat.SiteApp.Archive},
		{"Rewrite", mine.Rewrite, cat.SiteApp.Rewrite},
		{"FinishPath", mine.FinishPath, cat.SiteApp.FinishPath},
		{"MinPHP", mine.MinPHP, cat.SiteApp.MinPHP},
	} {
		if c.want != c.got {
			t.Errorf("piwigo 声明漂移：SiteApp.%s 目录=%q web 层=%q", c.name, c.got, c.want)
		}
	}
	if cat.SiteApp.NeedsDB != mine.NeedsDB || cat.SiteApp.StripTopDir != mine.StripTopDir {
		t.Errorf("piwigo 声明漂移：NeedsDB/StripTopDir 目录=%v/%v web 层=%v/%v",
			cat.SiteApp.NeedsDB, cat.SiteApp.StripTopDir, mine.NeedsDB, mine.StripTopDir)
	}
}

// TestPiwigoDBIdentifiersUniqueAndSafe 锁住库名/用户名：唯一、合法、可转义面为零。
func TestPiwigoDBIdentifiersUniqueAndSafe(t *testing.T) {
	domains := []string{
		"piwigo-test.test", "piwigo.test", "gallery.example.com",
		"a-very-long-subdomain-name-for-piwigo.example.com",
		"a-very-long-subdomain-name-for-piwigo.example.org",
		"UPPER.Case.Test", "x.y",
	}
	seenDB, seenUser := map[string]string{}, map[string]string{}
	for _, d := range domains {
		db, user := piwigoDBIdentifiers(d)
		if err := mysql.ValidateDBName(db); err != nil {
			t.Errorf("%s 生成的库名 %q 不合法：%v", d, db, err)
		}
		if err := mysql.ValidateUserName(user); err != nil {
			t.Errorf("%s 生成的用户名 %q 不合法：%v", d, user, err)
		}
		if !strings.HasPrefix(db, piwigoDBPrefix) || !strings.HasPrefix(user, piwigoUserPrefix) {
			t.Errorf("%s 的名字缺少前缀：库=%q 用户=%q", d, db, user)
		}
		if strings.ContainsAny(db+user, "'\"`;\\ \n\t") {
			t.Errorf("%s 的名字里有可疑字符：库=%q 用户=%q", d, db, user)
		}
		if prev, dup := seenDB[db]; dup {
			t.Errorf("%s 与 %s 派生出同一个库名 %q（必须带域名后缀避免冲突）", d, prev, db)
		}
		if prev, dup := seenUser[user]; dup {
			t.Errorf("%s 与 %s 派生出同一个用户名 %q", d, prev, user)
		}
		seenDB[db], seenUser[user] = d, d
	}
	// 大小写不同的同一域名应得到同一个名字（域名不区分大小写），不能因此产生两套库。
	dbUpper, userUpper := piwigoDBIdentifiers("A.Test")
	dbLower, _ := piwigoDBIdentifiers("a.test")
	if dbUpper != dbLower {
		t.Errorf("大小写不同的同一域名应派生同一个库名：%q vs %q", dbUpper, dbLower)
	}
	if userUpper == "" {
		t.Error("用户名不该为空")
	}
}

// TestPiwigoRejectsPHPBelow82 是"PHP < 8.2 如实拒绝"的负向对照（用假 php 二进制，不依赖真机）。
func TestPiwigoRejectsPHPBelow82(t *testing.T) {
	srv, _ := newTestServer(t)
	brew := t.TempDir()
	srv.Cfg.BrewPrefix = brew
	srv.Cfg.PHPSvc = "php@8.1"
	srv.Cfg.WWWRoot = filepath.Join(t.TempDir(), "www")
	writeFakePHP(t, brew, "8.1", "8.1.31", []string{"mysqli", "gd", "mbstring", "session", "json", "xml", "curl", "openssl", "zip"})

	app := piwigoExtraApp()
	if err := checkSiteAppRequirements(srv, app.Name, app.SiteApp, "8.1"); err == nil {
		t.Fatal("PHP 8.1 必须被拒（Piwigo 要求 8.2+）")
	} else if !strings.Contains(err.Error(), "8.2") {
		t.Errorf("拒绝理由必须写清要求 8.2+，实际：%v", err)
	}

	// 8.2 + 全部必需扩展 → 放行（负向对照的反面：不是"一律拒绝"）。
	writeFakePHP(t, brew, "8.2", "8.2.33", []string{"mysqli", "gd", "mbstring", "session", "json", "xml", "curl", "openssl", "zip"})
	srv.Cfg.PHPSvc = "php@8.2"
	if err := checkSiteAppRequirements(srv, app.Name, app.SiteApp, "8.2"); err != nil {
		t.Fatalf("PHP 8.2 + 必需扩展应放行：%v", err)
	}
}

// ---------- 安装路径的打桩 ----------

// stubPiwigoPreflight 关掉真实 php 前置检查（Piwigo 声明了 MinPHP/PHPExts，测试机不可能都有）。
func stubPiwigoPreflight(t *testing.T) {
	t.Helper()
	prev := sitePHPPreflightFn
	sitePHPPreflightFn = func(*Server, string, *services.SiteAppSpec, string) error { return nil }
	t.Cleanup(func() { sitePHPPreflightFn = prev })
}

// stubPiwigoProbes 注入收尾验证的两个探针：不真发 HTTP、不连真库。
func stubPiwigoProbes(t *testing.T, homeCode, finishCode int, dbErr error) {
	t.Helper()
	prevFollow, prevDB := siteFollowProbeFn, siteDBConnectCheckFn
	siteFollowProbeFn = func(_ context.Context, _, _ string, _ int, path string, _ time.Duration) (int, string) {
		if strings.HasPrefix(path, "/install.php") {
			return finishCode, strconv.Itoa(finishCode)
		}
		return homeCode, strconv.Itoa(homeCode)
	}
	siteDBConnectCheckFn = func(context.Context, *Server, string, string, string) error { return dbErr }
	t.Cleanup(func() { siteFollowProbeFn, siteDBConnectCheckFn = prevFollow, prevDB })
}

// fakeMySQLFailingOnPattern 造一个"匹配到某条 SQL 就失败"的假 mysql（不碰真实数据库）。
func fakeMySQLFailingOnPattern(t *testing.T, srv *Server, pattern string) {
	t.Helper()
	binDir := filepath.Join(srv.Cfg.BrewPrefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n  *" + pattern + "*) echo 'ERROR 1044 (42000): Access denied' >&2; exit 1 ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "mysql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// piwigoSiteDir 返回这个域名在测试 WWWRoot 下的站点目录。
func piwigoSiteDir(t *testing.T, srv *Server, domain string) string {
	t.Helper()
	dir, err := sites.SiteDir(srv.Cfg.WWWRoot, domain)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeFakeFPM 在沙箱 brew 前缀下造一个"活着"的 php-fpm 端点（真 unix socket + www.conf）。
//
// applySite 会解析 PHP 端点并要求它真在监听（"不生成指向空气的 vhost"）；测试里不许碰真机
// php-fpm，所以这里用 t.TempDir 下的一次性 socket 顶上。
func writeFakeFPM(t *testing.T, srv *Server, version string) {
	t.Helper()
	confDir := filepath.Join(srv.Cfg.BrewPrefix, "etc", "php", version, "php-fpm.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(srv.Cfg.BrewPrefix, "run", "php-fpm-"+version+".sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("造测试用 php-fpm socket 失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := os.WriteFile(filepath.Join(confDir, "www.conf"), []byte("listen = "+sock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPiwigoInstallSucceedsAndReportsWizard 是正向对照：三项验证都过时必须成功，
// 并留下"打开安装向导"的地址与预写的数据库配置。
func TestPiwigoInstallSucceedsAndReportsWizard(t *testing.T) {
	srv, _ := newTestServer(t)
	fakeMySQLClient(t, srv)
	stubSiteApplySteps(t, nil)
	stubSitePackageFetch(t, "官方源", makeSiteZip(t, "piwigo", 2048))
	stubPiwigoPreflight(t)
	stubPiwigoProbes(t, 200, 200, nil)
	writeFakeFPM(t, srv, "8.2")
	// 系统 DNS 探不到（真机上 .test 域名没进 hosts 就是这样）：只提示解析，不要再报"探测不到响应"。
	siteHomeProbeFn = func(context.Context, string) (int, string) { return 0, "000" }

	app := piwigoExtraApp()
	domain := "piwigo-ok.test"
	res, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{PHP: "8.2"})
	if err != nil {
		t.Fatalf("三项验证都过时必须成功：%v（步骤：%s）", err, strings.Join(res.Steps, "\n"))
	}
	if !strings.Contains(res.FinishURL, domain) || !strings.HasSuffix(res.FinishURL, "/install.php") {
		t.Errorf("结果里必须给出安装向导地址，实际 %q", res.FinishURL)
	}
	if res.DBName == "" || res.DBUser == "" || res.DBPass == "" {
		t.Fatalf("结果里必须有库名/账号/口令：%+v", res)
	}
	if res.PackageSize <= 0 || len(res.PackageSHA256) != 64 {
		t.Errorf("结果里必须带上实算回读的发行包指纹：size=%d sha=%q", res.PackageSize, res.PackageSHA256)
	}
	if !strings.Contains(res.Message, "/install.php") {
		t.Errorf("收尾说明必须给出向导地址，实际 %q", res.Message)
	}
	steps := strings.Join(res.Steps, "\n")
	if !strings.Contains(steps, "系统 DNS 解析不到") || strings.Contains(steps, "首页探测不到响应") {
		t.Errorf("已用 --resolve 验通后，DNS 探不到只应提示解析，不能同时说'探测不到响应'：%s", steps)
	}
	for _, want := range []string{"安装向导可达", "数据库连接验证通过", "回读校验通过"} {
		if !strings.Contains(steps, want) {
			t.Errorf("任务日志缺少 %q，实际：%s", want, steps)
		}
	}
	// 预写的配置必须是 Piwigo 自己的格式，且**不能**带 PHPWG_INSTALLED（否则向导直接 die）。
	cfg := filepath.Join(res.Dir, "local", "config", "database.inc.php")
	b, rerr := os.ReadFile(cfg)
	if rerr != nil {
		t.Fatalf("应预写 local/config/database.inc.php：%v", rerr)
	}
	s := string(b)
	if strings.Contains(s, "define('PHPWG_INSTALLED'") {
		t.Error("配置里不能定义 PHPWG_INSTALLED（install.php 见到它就 die('already installed')）")
	}
	if !strings.Contains(s, res.DBName) || !strings.Contains(s, res.DBUser) {
		t.Error("配置里应写入本次建的库名与账号")
	}
	if !strings.Contains(res.Message, "向导") {
		t.Errorf("收尾说明要讲清还差安装向导，实际 %q", res.Message)
	}
}

// TestPiwigoInstallFailsWhenSiteDoesNotComeUp 是状态判据的负向对照：
// 站点起不来（HTTP 无响应）就不算成功 —— 必须失败并回滚站点记录。
func TestPiwigoInstallFailsWhenSiteDoesNotComeUp(t *testing.T) {
	srv, _ := newTestServer(t)
	fakeMySQLClient(t, srv)
	stubSiteApplySteps(t, nil)
	stubSitePackageFetch(t, "官方源", makeSiteZip(t, "piwigo", 2048))
	stubPiwigoPreflight(t)
	stubPiwigoProbes(t, 0, 0, nil) // 首页与向导都无响应
	writeFakeFPM(t, srv, "8.2")

	app := piwigoExtraApp()
	domain := "piwigo-down.test"
	res, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{PHP: "8.2"})
	if err == nil {
		t.Fatal("站点起不来时一键建站必须失败（不许谎报成功）")
	}
	if !strings.Contains(err.Error(), "建站完成但用不了") || !strings.Contains(err.Error(), "无响应") {
		t.Errorf("错误必须如实说清是站点起不来，实际：%v", err)
	}
	if _, gerr := srv.siteMgr().Get(context.Background(), domain); gerr == nil {
		t.Error("失败后站点记录必须被回滚（否则用户以为建好了）")
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "已回滚站点记录") {
		t.Errorf("任务日志要如实写出回滚，实际：%q", strings.Join(res.Steps, "\n"))
	}
}

// TestPiwigoInstallRollsBackOnDownloadFailure 注入下载失败：不留半截站点。
func TestPiwigoInstallRollsBackOnDownloadFailure(t *testing.T) {
	srv, _ := newTestServer(t)
	fakeMySQLClient(t, srv)
	stubSiteApplySteps(t, nil)
	stubPiwigoPreflight(t)
	stubPiwigoProbes(t, 200, 200, nil)
	prev := sitePackageFetchFn
	sitePackageFetchFn = func(*Server, context.Context, string, string, func(string)) (services.SiteSource, string, error) {
		return services.SiteSource{}, "", os.ErrDeadlineExceeded
	}
	t.Cleanup(func() { sitePackageFetchFn = prev })

	app := piwigoExtraApp()
	domain := "piwigo-dlfail.test"
	if _, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{PHP: "8.2"}); err == nil {
		t.Fatal("下载失败必须让建站失败")
	}
	if _, gerr := srv.siteMgr().Get(context.Background(), domain); gerr == nil {
		t.Error("下载失败后不该留下站点记录")
	}
	if _, serr := os.Stat(piwigoSiteDir(t, srv, domain)); !os.IsNotExist(serr) {
		t.Errorf("下载失败后本次创建的空目录应被清掉，实际 stat err=%v", serr)
	}
}

// TestPiwigoInstallRollsBackOnExtractFailure 注入解压失败：清干净并报错。
func TestPiwigoInstallRollsBackOnExtractFailure(t *testing.T) {
	srv, _ := newTestServer(t)
	fakeMySQLClient(t, srv)
	stubSiteApplySteps(t, nil)
	stubPiwigoPreflight(t)
	stubPiwigoProbes(t, 200, 200, nil)
	// 大于 1KB 的假归档：过得了"完整性"检查，但 unzip 一定失败。
	prev := sitePackageFetchFn
	sitePackageFetchFn = func(_ *Server, _ context.Context, _, dest string, _ func(string)) (services.SiteSource, string, error) {
		if err := os.WriteFile(dest, []byte(strings.Repeat("not a zip at all. ", 200)), 0o644); err != nil {
			return services.SiteSource{}, "", err
		}
		return services.SiteSource{App: piwigoAppID, Name: "Piwigo", Version: "16.4.0"}, "官方源", nil
	}
	t.Cleanup(func() { sitePackageFetchFn = prev })

	app := piwigoExtraApp()
	domain := "piwigo-unzipfail.test"
	_, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{PHP: "8.2"})
	if err == nil || !strings.Contains(err.Error(), "解压失败") {
		t.Fatalf("解压失败必须如实报错，实际：%v", err)
	}
	if _, gerr := srv.siteMgr().Get(context.Background(), domain); gerr == nil {
		t.Error("解压失败后不该留下站点记录")
	}
	if _, serr := os.Stat(piwigoSiteDir(t, srv, domain)); !os.IsNotExist(serr) {
		t.Errorf("解压失败后本次创建的空目录应被清掉，实际 stat err=%v", serr)
	}
}

// TestPiwigoInstallRollsBackOnDBFailure 注入建库失败：不留站点记录，也不留半截目录。
func TestPiwigoInstallRollsBackOnDBFailure(t *testing.T) {
	srv, _ := newTestServer(t)
	fakeMySQLFailingOnPattern(t, srv, "CREATE DATABASE")
	stubSiteApplySteps(t, nil)
	stubSitePackageFetch(t, "官方源", makeSiteZip(t, "piwigo", 2048))
	stubPiwigoPreflight(t)
	stubPiwigoProbes(t, 200, 200, nil)

	app := piwigoExtraApp()
	domain := "piwigo-dbfail.test"
	_, err := srv.installSiteApp(context.Background(), app, domain, siteInstallReq{PHP: "8.2"})
	if err == nil || !strings.Contains(err.Error(), "创建数据库失败") {
		t.Fatalf("建库失败必须如实报错，实际：%v", err)
	}
	if _, gerr := srv.siteMgr().Get(context.Background(), domain); gerr == nil {
		t.Error("建库失败后不该留下站点记录（半截站点）")
	}
}

// TestPiwigoUninstallRefusesForeignDB 是"永不动用户其它库"的护栏：
// 不是一键建站的站点 → 拒绝删库；库里有非 piwigo_ 表 → 拒绝删库。
func TestPiwigoUninstallRefusesForeignDB(t *testing.T) {
	// ① Remark 不是面板写的 → 直接拒绝。
	if _, _, ok := piwigoSiteInstallDB(&sites.Site{Domain: "blog.test", Remark: "手建的站点"}); ok {
		t.Error("不是一键建站的站点必须拒绝删库")
	}
	// ② 一键建站的站点，但库里混着别人的表 → 拒绝（夹具用真实派生名）。
	srv, _ := newTestServer(t)
	domain := "blog.test"
	dbName, _ := piwigoDBIdentifiers(domain)
	logPath := fakeMySQLForUninstall(t, srv, []string{dbName}, []string{"piwigo_images", "user_data"})

	msg := srv.dropPiwigoSiteDB(context.Background(),
		&sites.Site{Domain: domain, Remark: piwigoInstallRemark}, func(string, ...any) {})
	if !strings.Contains(msg, "未删除") {
		t.Errorf("库里有非 Piwigo 表时必须拒绝删除，实际：%q", msg)
	}
	calls, _ := os.ReadFile(logPath)
	if strings.Contains(string(calls), "DROP DATABASE") {
		t.Errorf("拒绝时不该执行 DROP DATABASE，实际调用：\n%s", string(calls))
	}
	// ③ 由域名推导不出来的库（这里模拟"库里没有派生名那个库"）→ 如实说"不存在"。
	fakeMySQLForUninstall(t, srv, []string{"someone_else_db"}, nil)
	msg = srv.dropPiwigoSiteDB(context.Background(),
		&sites.Site{Domain: domain, Remark: piwigoInstallRemark}, func(string, ...any) {})
	if !strings.Contains(msg, "不存在") {
		t.Errorf("库不存在时应如实说明，实际：%q", msg)
	}
}

// TestPiwigoUninstallDropsItsOwnDB：只有 piwigo_ 表的库按显式勾选删除，并删掉专用账号。
func TestPiwigoUninstallDropsItsOwnDB(t *testing.T) {
	srv, _ := newTestServer(t)
	domain := "gallery.test"
	dbName, dbUser := piwigoDBIdentifiers(domain)
	logPath := fakeMySQLForUninstall(t, srv, []string{dbName}, []string{"piwigo_images", "piwigo_config"})

	var steps []string
	msg := srv.dropPiwigoSiteDB(context.Background(),
		&sites.Site{Domain: domain, Remark: piwigoInstallRemark},
		func(f string, a ...any) { steps = append(steps, f) })
	if !strings.Contains(msg, "已删除数据库 "+dbName) {
		t.Errorf("应删除本站在建库，实际：%q", msg)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), "DROP DATABASE") {
		t.Errorf("应真的执行 DROP DATABASE，实际调用：\n%s", string(calls))
	}
	if !strings.Contains(string(calls), "DROP USER") {
		t.Errorf("应同时删掉专用账号 %s，实际调用：\n%s", dbUser, string(calls))
	}
	if len(steps) == 0 {
		t.Error("删除动作要写进任务日志（用户可见）")
	}
}

// fakeMySQLForUninstall 造一个"能列出库与表"的假 mysql：不碰真实数据库。
func fakeMySQLForUninstall(t *testing.T, srv *Server, dbs, tables []string) string {
	t.Helper()
	binDir := filepath.Join(srv.Cfg.BrewPrefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(srv.Cfg.BrewPrefix, "mysql-uninstall.log")
	var dbRows, tblRows strings.Builder
	for _, d := range dbs {
		dbRows.WriteString(d + "\\tutf8mb4\\tutf8mb4_general_ci\\t0\\t0\\n")
	}
	for _, tb := range tables {
		tblRows.WriteString(tb + "\\tInnoDB\\t0\\t0\\tutf8mb4_general_ci\\t\\t2026-01-01 00:00:00\\n")
	}
	script := "#!/bin/sh\n" +
		"{ echo \"ARGV: $@\"; cat; } >> " + logPath + "\n" +
		"case \"$*\" in\n" +
		"  *information_schema.SCHEMATA*) printf '" + dbRows.String() + "' ;;\n" +
		"  *information_schema.TABLES*) printf '" + tblRows.String() + "' ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "mysql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return logPath
}

// TestPiwigoFrontendEntryWired 是前端入口接线门禁：入口、接口、向导链接与 8.2 文案缺一不可。
func TestPiwigoFrontendEntryWired(t *testing.T) {
	js := readAssetJS(t, "sites.js")
	for _, needle := range []string{
		"Piwigo 一键建站",
		"api.marketInstallSite('piwigo'",
		"Piwigo 需要 PHP 8.2+",
		"/install.php",
		"remove_db",
		"api.del(apiURL(",
		"s.install_db",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("sites.js 缺少 Piwigo 入口接线：%q", needle)
		}
	}
	// 安装向导链接必须由站点域名拼出来，不能写死某个域名。
	if !regexp.MustCompile(`function wizardURL\(`).MatchString(js) {
		t.Error("sites.js 必须有 wizardURL（用站点域名拼安装向导地址）")
	}
	// 前端只让 PHP 8.2+ 可选，后端也拒 —— 两边的门槛必须同值。
	if !strings.Contains(js, "phpAtLeast82") {
		t.Error("sites.js 必须在前端就按 8.2+ 过滤 PHP 版本（后端仍会再拒一次）")
	}
}

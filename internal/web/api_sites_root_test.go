package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/store"
)

// ============================================================================
//  站点根目录可自定义 + 目录索引（用户 2026-09-19：通用站点能力）
//
//  这一组门禁锁死四件事：
//    1. 白名单矩阵：系统路径逐个被拒且原因具体；家目录/外接卷/WWWRoot 放行；
//    2. 改根目录后**回读** vhost 里真的变了；autoindex 开关真的体现在生成物里；
//    3. 面板创建的目录归属 = 真实用户（stat 回读）；
//    4. 旧库（没有新列）读出来是默认值、不报错；不存在/不可读目录如实报错。
//
//  全部跑在 newTestServer 的沙箱里：不碰真实 nginx、不碰真实家目录。
// ============================================================================

// stubSiteChannelDisk 把"写 vhost → reload → 复核"换成沙箱磁盘实现：
// 写真的落到 Cfg.VhostDir、读真的从磁盘读 —— 这样"改完回读生效值"走的是
// 与生产完全相同的路径（stubSiteApplyChannel 的纯内存假实现读不到 root）。
func stubSiteChannelDisk(t *testing.T, srv *Server) {
	t.Helper()
	prevWrite, prevReload, prevProbe := siteWriteVhostFn, siteReloadFn, siteProbeFn
	prevRead, prevDelete := siteReadVhostFn, siteDeleteVhostFn
	prevWait, prevEvery := siteVerifyWait, siteVerifyEvery

	siteWriteVhostFn = func(s *Server, _ context.Context, name, content string) error {
		if err := os.MkdirAll(s.Cfg.VhostDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(s.Cfg.VhostDir, name+".conf"), []byte(content), 0o644)
	}
	siteReloadFn = func(*Server, context.Context) error { return nil }
	siteProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		return "403", "", nil
	}
	siteReadVhostFn = func(_ *Server, path string) ([]byte, error) { return os.ReadFile(path) }
	siteDeleteVhostFn = func(s *Server, _ context.Context, name string) error {
		return os.Remove(filepath.Join(s.Cfg.VhostDir, name+".conf"))
	}
	siteVerifyWait, siteVerifyEvery = 60*time.Millisecond, time.Millisecond

	t.Cleanup(func() {
		siteWriteVhostFn, siteReloadFn, siteProbeFn = prevWrite, prevReload, prevProbe
		siteReadVhostFn, siteDeleteVhostFn = prevRead, prevDelete
		siteVerifyWait, siteVerifyEvery = prevWait, prevEvery
	})
}

func readVhost(t *testing.T, srv *Server, domain string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(srv.Cfg.VhostDir, domain+".conf"))
	if err != nil {
		t.Fatalf("读取 %s 的 vhost 失败: %v", domain, err)
	}
	return string(b)
}

// TestSiteCreateCustomRootAutoIndexAndReadback：创建时指定外接盘目录（带空格）
// + 目录索引，接口必须回读 vhost 确认 root 真的变了。
func TestSiteCreateCustomRootAutoIndexAndReadback(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	cookies := loginTestPanel(t, ts)

	// 盘名带空格：顺带覆盖 vhost 里的引号写法
	ext := filepath.Join(t.TempDir(), "mirror site")
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "mirror.test", "rewrite": "none", "root": ext, "autoindex": true,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("创建应成功，实际 %d: %v", res.StatusCode, out)
	}
	data := apiData(t, out)
	if fmt.Sprint(data["root"]) != ext {
		t.Errorf("接口返回 root = %v，期望 %s", data["root"], ext)
	}
	if data["root_verified"] != true {
		t.Errorf("必须回读确认根目录已生效，实际 root_verified=%v err=%v", data["root_verified"], data["root_verify_error"])
	}
	if fmt.Sprint(data["root_applied"]) != ext {
		t.Errorf("回读到的 root = %v，期望 %s", data["root_applied"], ext)
	}

	conf := readVhost(t, srv, "mirror.test")
	if !strings.Contains(conf, `root        "`+ext+`";`) {
		t.Errorf("vhost 里应写入带引号的根目录 %q：\n%s", ext, conf)
	}
	if !strings.Contains(conf, "autoindex   on;") {
		t.Errorf("vhost 里应有 autoindex on：\n%s", conf)
	}

	// 目录真的建了，且归属 = 真实用户
	uid, _, err := sites.DirOwnerUID(ext)
	if err != nil {
		t.Fatalf("站点目录应已创建: %v", err)
	}
	wantUID, _, uerr := lookupIDs(srv.Cfg.User)
	if uerr == nil && uid != wantUID {
		t.Errorf("目录归属 uid=%d，期望真实用户 %s（uid %d）—— nginx 与用户都会读写不了", uid, srv.Cfg.User, wantUID)
	}

	// 库里的新字段
	got, err := srv.siteMgr().Get(context.Background(), "mirror.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseRoot != ext || !got.AutoIndex || got.Root != ext {
		t.Fatalf("store 读回应为 base_root=%s autoindex=true root=%s，实际 %+v", ext, ext, got)
	}
}

// TestSiteUpdateChangesRootAndVhost：编辑时改根目录 / 清空回默认 / 目录索引开关。
func TestSiteUpdateChangesRootAndVhost(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	cookies := loginTestPanel(t, ts)
	ctx := context.Background()

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "edit-root.test", "rewrite": "none",
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("建默认站点失败: %v", out)
	}
	defRoot := filepath.Join(srv.Cfg.WWWRoot, "edit-root.test")
	if !strings.Contains(readVhost(t, srv, "edit-root.test"), "root        "+defRoot+";") {
		t.Fatalf("默认站点根目录应为 %s", defRoot)
	}

	// ① 改到外接盘目录 → vhost 必须真的变，并回读确认
	ext := filepath.Join(t.TempDir(), "downloads")
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/edit-root.test",
		map[string]any{"root": ext}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("改根目录应成功，实际 %d: %v", res.StatusCode, out)
	}
	data := apiData(t, out)
	if fmt.Sprint(data["root_applied"]) != ext {
		t.Errorf("改完回读的 root = %v，期望 %s", data["root_applied"], ext)
	}
	if data["root_verified"] != true {
		t.Errorf("根目录改动必须复核，实际 %v", data)
	}
	if conf := readVhost(t, srv, "edit-root.test"); !strings.Contains(conf, "root        "+ext+";") {
		t.Errorf("vhost 的 root 没改成功：\n%s", conf)
	}
	got, _ := srv.siteMgr().Get(ctx, "edit-root.test")
	if got.BaseRoot != ext || got.Root != ext {
		t.Fatalf("store 未记录自定义根目录: %+v", got)
	}

	// ② 目录索引：开 → on，关 → 不出现
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/edit-root.test",
		map[string]any{"autoindex": true}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("开目录索引失败: %v", out)
	}
	if conf := readVhost(t, srv, "edit-root.test"); !strings.Contains(conf, "autoindex   on;") {
		t.Errorf("开启后 vhost 应有 autoindex on：\n%s", conf)
	}
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/edit-root.test",
		map[string]any{"autoindex": false}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("关目录索引失败: %v", out)
	}
	if conf := readVhost(t, srv, "edit-root.test"); strings.Contains(conf, "autoindex") {
		t.Errorf("关闭后 vhost 不该出现 autoindex：\n%s", conf)
	}

	// ③ 清空根目录 = 回默认目录
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/edit-root.test",
		map[string]any{"root": ""}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("清空根目录应回到默认，实际 %d: %v", res.StatusCode, out)
	}
	got, _ = srv.siteMgr().Get(ctx, "edit-root.test")
	if got.BaseRoot != "" || got.Root != defRoot {
		t.Fatalf("清空后应回到默认根目录 %s，实际 %+v", defRoot, got)
	}
	if conf := readVhost(t, srv, "edit-root.test"); !strings.Contains(conf, "root        "+defRoot+";") {
		t.Errorf("清空后 vhost 没回到默认根目录：\n%s", conf)
	}
}

// TestSiteRootWhitelistRejectedAtAPI：白名单在接口层就挡住，且原因具体。
func TestSiteRootWhitelistRejectedAtAPI(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	cookies := loginTestPanel(t, ts)

	cases := []struct{ domain, root, want string }{
		{"evil1.test", "/etc", "系统配置目录"},
		{"evil2.test", "/System/Volumes/Data", "macOS 系统目录"},
		{"evil3.test", "/Library", "系统资源库"},
		{"evil4.test", "/usr/local/site", "系统程序目录"},
		{"evil5.test", "/var/db/mirror", "系统数据库目录"},
		{"evil6.test", "/opt/homebrew/www", "Homebrew"},
		{"evil7.test", "/", "整个系统盘"},
		{"evil8.test", "relative/path", "绝对路径"},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites",
			map[string]any{"domain": c.domain, "rewrite": "none", "root": c.root}, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 的 root=%s 必须 400，实际 %d: %v", c.domain, c.root, res.StatusCode, out)
			continue
		}
		msg := fmt.Sprint(out["msg"])
		if !strings.Contains(msg, c.want) {
			t.Errorf("root=%s 的拒绝原因应含 %q（不能只说『不合法』），实际: %s", c.root, c.want, msg)
		}
		if _, err := srv.siteMgr().Get(context.Background(), c.domain); err == nil {
			t.Errorf("被拒的站点 %s 不该落库", c.domain)
		}
	}
}

// TestSiteRootMissingOrUnreadableReportsHonestly：不存在/不可读目录必须当场报错，
// 不许"创建成功"然后站点 403。
func TestSiteRootMissingOrUnreadableReportsHonestly(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	cookies := loginTestPanel(t, ts)

	missing := filepath.Join(t.TempDir(), "not-there")
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "missing.test", "rewrite": "none", "root": missing, "create_dir": false,
	}, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("根目录不存在却报成功: %v", out)
	}
	if msg := fmt.Sprint(out["msg"]); !strings.Contains(msg, "不存在") {
		t.Errorf("错误信息应说明目录不存在，实际: %s", msg)
	}
	if _, err := srv.siteMgr().Get(context.Background(), "missing.test"); err == nil {
		t.Error("失败后不该留下站点记录")
	}

	// 存在但不可读（权限 000）
	noRead := filepath.Join(t.TempDir(), "no-read")
	if err := os.Mkdir(noRead, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(noRead, 0o755) })
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "noread.test", "rewrite": "none", "root": noRead, "create_dir": false,
	}, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("根目录不可读却报成功: %v", out)
	}
	if msg := fmt.Sprint(out["msg"]); !strings.Contains(msg, "不可读") {
		t.Errorf("错误信息应说明不可读（nginx 会 403），实际: %s", msg)
	}

	// 已存在但不可读 + 默认 create_dir=true（不会覆盖已有目录）→ 同样必须报错
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "noread2.test", "rewrite": "none", "root": noRead,
	}, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("已存在但不可读的目录却报成功（站点会 403）: %v", out)
	}
	if msg := fmt.Sprint(out["msg"]); !strings.Contains(msg, "不可读") {
		t.Errorf("复用已有不可读目录时错误应说明不可读，实际: %s", msg)
	}
	if _, err := srv.siteMgr().Get(context.Background(), "noread2.test"); err == nil {
		t.Error("失败后不该留下站点记录")
	}
}

// TestSiteRootCreatedOwnedByRealUser：面板创建的目录归属必须是真实用户（stat 回读）。
func TestSiteRootCreatedOwnedByRealUser(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	cookies := loginTestPanel(t, ts)

	// 多级目录：只应把新建的那几层交还用户，不动已存在的祖先
	root := filepath.Join(t.TempDir(), "mirror", "downloads")
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "owned.test", "rewrite": "none", "root": root,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("创建应成功: %v", out)
	}
	wantUID, _, err := lookupIDs(srv.Cfg.User)
	if err != nil {
		t.Skipf("解析不出面板用户 %q，跳过归属校验: %v", srv.Cfg.User, err)
	}
	for _, p := range []string{root, filepath.Dir(root), filepath.Dir(filepath.Dir(root))} {
		uid, _, oerr := sites.DirOwnerUID(p)
		if oerr != nil {
			t.Fatalf("回读 %s 归属失败: %v", p, oerr)
		}
		if uid != wantUID {
			t.Errorf("%s 的归属 uid=%d，期望真实用户 uid=%d（面板以 root 创建后必须 chown 回去）",
				p, uid, wantUID)
		}
	}
}

func TestFirstMissingAncestor(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "a")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := firstMissingAncestor(existing); got != "" {
		t.Errorf("已存在目录应返回空串，实际 %q", got)
	}
	want := filepath.Join(dir, "a", "b")
	if got := firstMissingAncestor(filepath.Join(dir, "a", "b", "c")); got != want {
		t.Errorf("最上层缺失段 = %q，期望 %q", got, want)
	}
}

// TestSiteCreateCustomListenPortAndReadback：创建时指定监听端口，vhost 必须真的
// listen 该端口并回读确认（镜像站独占端口的用法）。
func TestSiteCreateCustomListenPortAndReadback(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	cookies := loginTestPanel(t, ts)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "mirror8095.test", "rewrite": "none", "listen_port": 8095, "autoindex": true,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("创建应成功，实际 %d: %v", res.StatusCode, out)
	}
	data := apiData(t, out)
	if fmt.Sprint(data["listen_port"]) != "8095" {
		t.Errorf("接口返回 listen_port = %v，期望 8095", data["listen_port"])
	}
	if data["listen_port_verified"] != true {
		t.Errorf("必须回读确认端口已生效，实际 %v (%v)", data["listen_port_verified"], data["listen_port_verify_error"])
	}
	if fmt.Sprint(data["listen_port_applied"]) != "8095" {
		t.Errorf("回读到 listen = %v，期望 8095", data["listen_port_applied"])
	}
	if conf := readVhost(t, srv, "mirror8095.test"); !strings.Contains(conf, "listen      8095;") {
		t.Errorf("vhost 里必须是 listen 8095：\n%s", conf)
	}
	got, err := srv.siteMgr().Get(context.Background(), "mirror8095.test")
	if err != nil || got.ListenPort != 8095 {
		t.Fatalf("store 未记录端口: %+v err=%v", got, err)
	}

	// 编辑改端口 → vhost 必须真的变
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/mirror8095.test",
		map[string]any{"listen_port": 8096}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("改端口应成功: %v", out)
	}
	data = apiData(t, out)
	if fmt.Sprint(data["listen_port_applied"]) != "8096" || data["listen_port_verified"] != true {
		t.Errorf("改端口后回读不对：%v", data)
	}
	if conf := readVhost(t, srv, "mirror8095.test"); !strings.Contains(conf, "listen      8096;") {
		t.Errorf("vhost 的 listen 没改成 8096：\n%s", conf)
	}
}

// TestSiteListenPortValidationMatrix：非法值 / 面板保留端口 / 站点冲突逐个被拒、原因具体。
func TestSiteListenPortValidationMatrix(t *testing.T) {
	srv, ts := newTestServer(t)
	stubSiteChannelDisk(t, srv)
	cookies := loginTestPanel(t, ts)

	panelPort := listenPortFromAddr(srv.Cfg.Listen)
	if panelPort == 0 {
		t.Fatalf("测试配置里应能解析出面板端口: %q", srv.Cfg.Listen)
	}
	cases := []struct {
		name string
		port int
		want string
	}{
		{"zero", 0, "1-65535"},
		{"negative", -5, "1-65535"},
		{"too-big", 65536, "1-65535"},
		{"panel", panelPort, "面板自身"},
		{"https", 443, "HTTPS 端口"},
		{"iopaint", 8080, "IOPaint"},
		// 3306 现在由数据库引擎声明（目录里 MariaDB 排在 MySQL 前面，默认引擎也是它）。
		{"mysql", 3306, "MariaDB"},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
			"domain": "port-" + c.name + ".test", "rewrite": "none", "listen_port": c.port,
		}, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s（端口 %d）必须 400，实际 %d: %v", c.name, c.port, res.StatusCode, out)
			continue
		}
		if msg := fmt.Sprint(out["msg"]); !strings.Contains(msg, c.want) {
			t.Errorf("端口 %d 的拒绝原因应含 %q，实际: %s", c.port, c.want, msg)
		}
		if _, err := srv.siteMgr().Get(context.Background(), "port-"+c.name+".test"); err == nil {
			t.Errorf("被拒的站点 port-%s.test 不该落库", c.name)
		}
	}

	// 同一非 80 端口允许多个站点共存（nginx 支持同端口多 server_name，坑 215）。
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "taken.test", "rewrite": "none", "listen_port": 8098,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("建占用端口的站点失败: %v", out)
	}
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "clash.test", "rewrite": "none", "listen_port": 8098,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("同端口不同域名必须允许，实际 %d: %v", res.StatusCode, out)
	}
	// 真冲突：新站点的别名撞上同端口已有站点的域名/别名
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "alias-clash.test", "aliases": "taken.test", "rewrite": "none", "listen_port": 8098,
	}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("同端口域名重复必须被拒，实际 %d: %v", res.StatusCode, out)
	}
	if msg := fmt.Sprint(out["msg"]); !strings.Contains(msg, "taken.test") || !strings.Contains(msg, "域名重复") {
		t.Errorf("冲突原因应指出重复的域名与占用它的站点，实际: %s", msg)
	}
	// 正常值放行
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "free.test", "rewrite": "none", "listen_port": 8099,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("未被占用的端口应放行，实际 %d: %v", res.StatusCode, out)
	}
	// 80 是共享端口：多个站点都能用（老站点全在 80）
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "shared80-a.test", "rewrite": "none", "listen_port": 80,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("80 必须允许多站点共享，实际 %d: %v", res.StatusCode, out)
	}
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "shared80-b.test", "rewrite": "none", "listen_port": 80,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("80 上第二个站点也必须放行，实际 %d: %v", res.StatusCode, out)
	}
}

// TestOldSiteListenPortDefaultsTo80：老库站点读出来是 80，生成的 vhost 逐字不变。
func TestOldSiteListenPortDefaultsTo80(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sites (
	    id INTEGER PRIMARY KEY AUTOINCREMENT, domain TEXT NOT NULL UNIQUE,
	    aliases TEXT NOT NULL DEFAULT '', root TEXT NOT NULL,
	    php_version TEXT NOT NULL DEFAULT '', rewrite TEXT NOT NULL DEFAULT 'none',
	    ssl_enabled INTEGER NOT NULL DEFAULT 0, ssl_cert TEXT NOT NULL DEFAULT '',
	    ssl_key TEXT NOT NULL DEFAULT '', ssl_provider TEXT NOT NULL DEFAULT '',
	    ssl_expires TEXT NOT NULL DEFAULT '', proxy_pass TEXT NOT NULL DEFAULT '',
	    extra_conf TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
	    remark TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
	    updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sites(domain,root) VALUES('legacy.test','/tmp/legacy.test')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("老库迁移失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	got, err := sites.NewManager(st, sites.Options{}).Get(context.Background(), "legacy.test")
	if err != nil {
		t.Fatalf("老站点读不出来: %v", err)
	}
	if got.EffectiveListenPort() != 80 || got.ListenPort != 80 {
		t.Fatalf("老站点端口应为 80，实际 %+v", got)
	}
	conf, err := got.Generate(sites.Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "listen      80;") || strings.Contains(conf, "listen      0;") {
		t.Fatalf("老站点 vhost 必须逐字监听 80：\n%s", conf)
	}
}

// TestSiteCheckProbesCustomPort：诊断必须探站点**自己的**端口。
// 曾经硬编码探 80：自定义端口站点点「诊断」，拿到的是默认站点的回包 —— 典型谎报。
func TestSiteCheckProbesCustomPort(t *testing.T) {
	srv, _ := newTestServer(t)
	prev := siteCheckProbeFn
	type probeRec struct {
		scheme, host, path string
		port               int
	}
	var probes []probeRec
	siteCheckProbeFn = func(_ context.Context, scheme, domain string, port int, path string, _ time.Duration) (string, string, error) {
		probes = append(probes, probeRec{scheme, domain, path, port})
		return "403", "", nil
	}
	t.Cleanup(func() { siteCheckProbeFn = prev })

	root := filepath.Join(srv.Cfg.WWWRoot, "diag8090.test")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	site := &sites.Site{Domain: "diag8090.test", Root: root, Rewrite: "none",
		ListenPort: 8090, Enabled: true}
	res := checkSite(context.Background(), site, srv.siteLogDir())
	if len(probes) == 0 {
		t.Fatal("诊断必须真的发起探测")
	}
	for _, p := range probes {
		if p.port != 8090 {
			t.Errorf("探测端口 = %d，期望站点自己的 8090（探 80 会拿到默认站点的结论）", p.port)
		}
		if p.scheme != "http" {
			t.Errorf("未开 SSL 的站点应用 http 探测，实际 %q", p.scheme)
		}
	}
	if res.URL != "http://diag8090.test:8090/" {
		t.Errorf("诊断 URL 必须带端口，实际 %q", res.URL)
	}

	// 开了 SSL 的站点：按真实 TLS 状态探 https 443，不按端口猜。
	probes = nil
	sslSite := &sites.Site{Domain: "diagssl.test", Root: root, Rewrite: "none",
		ListenPort: 8090, SSLEnabled: true, Enabled: true}
	sslRes := checkSite(context.Background(), sslSite, srv.siteLogDir())
	if len(probes) == 0 || probes[0].port != 443 || probes[0].scheme != "https" {
		t.Fatalf("开 SSL 的站点必须探 https 443，实际 %+v", probes)
	}
	if sslRes.URL != "https://diagssl.test/" {
		t.Errorf("SSL 站点 URL 应无端口，实际 %q", sslRes.URL)
	}
}

func TestSiteCheckURLKeepsDefaultPortsClean(t *testing.T) {
	if got := siteCheckURL("http", "a.test", 80); got != "http://a.test/" {
		t.Errorf("80 不该拼端口，实际 %q", got)
	}
	if got := siteCheckURL("https", "a.test", 443); got != "https://a.test/" {
		t.Errorf("443 不该拼端口，实际 %q", got)
	}
	if got := siteCheckURL("http", "a.test", 8090); got != "http://a.test:8090/" {
		t.Errorf("自定义端口必须拼上，实际 %q", got)
	}
}

// TestOldSiteWithoutNewColumnsReadsDefaults：老库（sites 表没有 base_root/autoindex/listen_port）
// 经 migrate() 后必须能读出默认值、不报错，且原有 root 不丢。
func TestOldSiteWithoutNewColumnsReadsDefaults(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	// 加字段之前的 sites 表结构（逐字来自旧版 schema）
	oldDDL := `CREATE TABLE sites (
	    id INTEGER PRIMARY KEY AUTOINCREMENT,
	    domain TEXT NOT NULL UNIQUE,
	    aliases TEXT NOT NULL DEFAULT '',
	    root TEXT NOT NULL,
	    php_version TEXT NOT NULL DEFAULT '',
	    rewrite TEXT NOT NULL DEFAULT 'none',
	    ssl_enabled INTEGER NOT NULL DEFAULT 0,
	    ssl_cert TEXT NOT NULL DEFAULT '',
	    ssl_key TEXT NOT NULL DEFAULT '',
	    ssl_provider TEXT NOT NULL DEFAULT '',
	    ssl_expires TEXT NOT NULL DEFAULT '',
	    proxy_pass TEXT NOT NULL DEFAULT '',
	    extra_conf TEXT NOT NULL DEFAULT '',
	    enabled INTEGER NOT NULL DEFAULT 1,
	    remark TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
	    updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
	)`
	if _, err := db.Exec(oldDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sites(domain,root,rewrite) VALUES('old.test','/tmp/old.test','none')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("老库迁移失败（旧站点不能因缺字段报错）: %v", err)
	}
	defer func() { _ = st.Close() }()
	mgr := sites.NewManager(st, sites.Options{})
	got, err := mgr.Get(context.Background(), "old.test")
	if err != nil {
		t.Fatalf("老站点读不出来: %v", err)
	}
	if got.BaseRoot != "" || got.AutoIndex || got.ListenPort != 80 {
		t.Fatalf("老站点新字段应为默认值（base_root 空、autoindex 关、端口 80），实际 %+v", got)
	}
	if got.Root != "/tmp/old.test" {
		t.Fatalf("老站点的 root 不该被迁移改写，实际 %q", got.Root)
	}
	list, err := mgr.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("列出老站点失败: list=%d err=%v", len(list), err)
	}
}

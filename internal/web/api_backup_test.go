package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/backup"
	"github.com/zizdog/zizpanel/internal/offsite"
)

// TestRestoreReportsArchiveFilesItDoesNotApply：归档里的 sites/数据库导出
// 恢复时不会自动覆盖，必须**如实列进"跳过"清单**（否则用户以为恢复完了网站却没变）。
func TestRestoreReportsArchiveFilesItDoesNotApply(t *testing.T) {
	srv, ts := newTestServer(t)
	_ = loginPanel(t, ts)
	ctx := context.Background()

	// 造一个 www 站点文件，让 sites 目标真的产出 sites/www.tar.gz
	www := filepath.Join(srv.Cfg.UserHome, "www", "demo")
	if err := os.MkdirAll(www, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := makeArchive(t, srv, []string{backup.TargetSites, backup.TargetPanel})
	if _, ok := res.Manifest.FileByPath("sites/www.tar.gz"); !ok {
		t.Fatalf("sites 目标没有产出 sites/www.tar.gz：%v", res.Manifest.Files)
	}

	rep, err := srv.runRestore(ctx, func(string, string) {}, res.Path, false)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	found := false
	for _, s := range rep.Skipped {
		if strings.Contains(s, "sites/www.tar.gz") {
			found = true
		}
	}
	if !found {
		t.Fatalf("归档里的 sites/www.tar.gz 必须如实列进跳过清单，实际 skipped=%v unapplied=%v",
			rep.Skipped, rep.Unapplied)
	}
}

// TestUnderAnyBoundary：非特权恢复的"只写面板自己目录"闸门必须在目录边界上截断，
// 否则 /data 会错误地放行 /database。
func TestUnderAnyBoundary(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/data/config.json", true},
		{"/data", true},
		{"/database/config.json", false},
		{"/data-other/x", false},
		{"/work/compose/a/docker-compose.yml", true},
		{"/opt/homebrew/etc/nginx/nginx.conf", false},
	}
	for _, c := range cases {
		if got := underAny(c.path, "/data", "/work"); got != c.want {
			t.Fatalf("underAny(%q) = %v，期望 %v", c.path, got, c.want)
		}
	}
}

// makeArchive 直接调用备份实现生成一份归档（不走任务中心，测试更确定）。
func makeArchive(t *testing.T, srv *Server, targets []string) *backup.Result {
	t.Helper()
	outDir := filepath.Join(srv.Cfg.WorkDir, "backup")
	req := backup.NewRequest(srv.Cfg, srv.Store, targets, outDir, 0)
	res, err := backup.Create(context.Background(), srv.Store, req)
	if err != nil {
		t.Fatalf("生成测试备份失败: %v", err)
	}
	return res
}

// TestRestoreRoundTripRestoresDeletedSiteAndCert 是本功能的核心真机场景的沙箱版：
// 备份 → 删掉一个站点记录与一个证书文件 → 恢复 → 两者都回来，旧会话失效。
func TestRestoreRoundTripRestoresDeletedSiteAndCert(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)
	ctx := context.Background()

	const domain = "restore.example.com"
	if _, err := srv.Store.DB().Exec(
		`INSERT INTO sites(domain,root,php_version,ssl_enabled,ssl_provider) VALUES(?,?,?,?,?)`,
		domain, "/tmp/restore", "8.4", 1, "self"); err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(srv.Cfg.DataDir, "site-certs", domain)
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(certDir, "fullchain.pem")
	if err := os.WriteFile(certFile, []byte("CERT-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := makeArchive(t, srv, []string{backup.TargetPanel})
	if _, err := backup.Verify(res.Path); err != nil {
		t.Fatalf("自产归档校验失败: %v", err)
	}

	// 破坏现场：删站点记录 + 删证书目录
	if _, err := srv.Store.DB().Exec(`DELETE FROM sites WHERE domain=?`, domain); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(certDir); err != nil {
		t.Fatal(err)
	}

	logLines := []string{}
	logf := func(level, text string) { logLines = append(logLines, level+": "+text) }
	rep, err := srv.runRestore(ctx, logf, res.Path, false)
	if err != nil {
		t.Fatalf("恢复失败: %v\n日志:\n%s", err, strings.Join(logLines, "\n"))
	}
	if !rep.SnapshotUsable {
		t.Fatal("恢复前必须生成可用的回滚快照")
	}
	if _, err := os.Stat(rep.Snapshot); err != nil {
		t.Fatalf("回滚快照不存在: %v", err)
	}
	if _, err := backup.Verify(rep.Snapshot); err != nil {
		t.Fatalf("回滚快照本身必须是一个能校验通过的归档: %v", err)
	}

	// ① 站点记录回来了
	var root string
	if err := srv.Store.DB().QueryRow(
		`SELECT root FROM sites WHERE domain=?`, domain).Scan(&root); err != nil {
		t.Fatalf("恢复后站点记录没回来: %v", err)
	}
	if root != "/tmp/restore" {
		t.Fatalf("站点根目录不对: %s", root)
	}
	// ② 证书文件回来了，内容一致
	b, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("恢复后证书文件没回来: %v", err)
	}
	if string(b) != "CERT-CONTENT" {
		t.Fatalf("证书内容不对: %q", string(b))
	}
	// ③ 旧会话必须失效：拿旧 cookie 访问受保护接口 → 401
	res2, _ := rawGet(t, ts, "/api/v1/session", cookies)
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("恢复后旧会话 cookie 仍可用（期望 401，实际 %d）—— 等于把认证状态回退到备份那一刻",
			res2.StatusCode)
	}
	// ④ 外键自检与回读
	if err := srv.Store.CheckForeignKeys(ctx); err != nil {
		t.Fatalf("外键自检失败: %v", err)
	}
	if rep.ForeignKeyCheck == "" || rep.Sites == 0 {
		t.Fatalf("恢复报告缺少如实回读: %+v", rep)
	}
	// ⑤ 非特权测试进程不应触碰 nginx，但必须如实说明
	if os.Geteuid() != 0 {
		found := false
		for _, u := range rep.Skipped {
			if strings.Contains(u, "nginx") {
				found = true
			}
		}
		if !found {
			t.Fatalf("非特权实例必须如实列出未重建 nginx：skipped=%v unapplied=%v", rep.Skipped, rep.Unapplied)
		}
	}
}

// TestRestoreRejectsTamperedArchive：坏包必须整包拒绝，且数据库一字未改。
func TestRestoreRejectsTamperedArchive(t *testing.T) {
	srv, ts := newTestServer(t)
	_ = loginPanel(t, ts)
	ctx := context.Background()

	if _, err := srv.Store.DB().Exec(
		`INSERT INTO sites(domain,root) VALUES('keep.example.com','/tmp/keep')`); err != nil {
		t.Fatal(err)
	}
	res := makeArchive(t, srv, []string{backup.TargetPanel})

	// 解包 → 改一个字节 → 用原清单重打包（sha256 必然不符）
	stage := t.TempDir()
	if _, err := backup.Extract(res.Path, stage); err != nil {
		t.Fatal(err)
	}
	// 用数据库快照做篡改目标：它一定在归档里（config.json 在测试沙箱里可能不存在）。
	cfgPath := filepath.Join(stage, "db", "panel.db")
	orig, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(orig, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(srv.Cfg.WorkDir, "backup", "tampered.tar.gz")
	if err := tarGzForTest(stage, bad); err != nil {
		t.Fatal(err)
	}

	_, err = srv.runRestore(ctx, func(string, string) {}, bad, false)
	if err == nil {
		t.Fatal("坏 sha256 的归档必须被拒绝")
	}
	var n int
	if err := srv.Store.DB().QueryRow(`SELECT COUNT(*) FROM sites WHERE domain='keep.example.com'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("拒绝恢复时不应改动数据库")
	}
}

// TestBackupHTTPEndpoints 覆盖接口接线：列表 / 立即备份 / 上传 / 详情 / 下载 / 删除。
func TestBackupHTTPEndpoints(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/backups", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("列表接口失败 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil || data["targets"] == nil {
		t.Fatalf("列表必须返回可选备份范围（界面勾选用）: %v", out)
	}

	// 立即备份 → 202 + task_id（异步长任务，接线只验证到任务创建）
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/backups",
		map[string]any{"targets": []string{"panel"}, "keep_days": 0}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("立即备份应返回 202，实际 %d: %v", res.StatusCode, out)
	}
	tdata, _ := out["data"].(map[string]any)
	if tdata == nil || tdata["task_id"] == nil || tdata["task_id"] == "" {
		t.Fatalf("立即备份必须返回 task_id: %v", out)
	}

	// 直接造一份归档，验证 详情/下载/删除 与上传（好包/坏包）
	arch := makeArchive(t, srv, []string{backup.TargetPanel}).Path

	res, out, _ = doJSON(t, ts, "GET", "/api/v1/backups/"+filepath.Base(arch), nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("详情接口失败 %d: %v", res.StatusCode, out)
	}
	if d, _ := out["data"].(map[string]any); d == nil || d["compatible"] != true {
		t.Fatalf("详情必须给出兼容性结论: %v", out)
	}

	// 下载
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/backups/"+filepath.Base(arch)+"/download", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	dres, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = dres.Body.Close()
	if dres.StatusCode != 200 {
		t.Fatalf("下载接口失败 %d", dres.StatusCode)
	}

	// 上传一个合法归档
	upload, err := doUpload(t, ts, cookies, arch)
	if err != nil {
		t.Fatalf("上传合法归档失败: %v", err)
	}
	if upload.StatusCode != 200 {
		t.Fatalf("上传合法归档应 200，实际 %d: %v", upload.StatusCode, upload.Body)
	}
	// 上传一个坏包 → 400，且文件不能留在备份目录里
	junk := filepath.Join(t.TempDir(), "junk.tar.gz")
	if err := os.WriteFile(junk, []byte("definitely not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	badUpload, err := doUpload(t, ts, cookies, junk)
	if err != nil {
		t.Fatal(err)
	}
	if badUpload.StatusCode != http.StatusBadRequest {
		t.Fatalf("坏包上传必须被拒绝（400），实际 %d", badUpload.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(srv.backupDir(), "junk.tar.gz")); err == nil {
		t.Fatal("校验失败的上传文件不应留在备份目录里")
	}

	// 删除
	res, _, _ = doJSON(t, ts, "DELETE", "/api/v1/backups/"+filepath.Base(arch), nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删除接口失败 %d", res.StatusCode)
	}
	if _, err := os.Stat(arch); err == nil {
		t.Fatal("删除后文件仍存在")
	}

	// 恢复一个不存在的归档 → 404
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/backups/restore",
		map[string]any{"name": "nope.tar.gz"}, cookies)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("恢复不存在的归档应 404，实际 %d", res.StatusCode)
	}
}

type uploadResult struct {
	StatusCode int
	Body       string
}

func doUpload(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, path string) (uploadResult, error) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return uploadResult{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return uploadResult{}, err
	}
	if _, err := fw.Write(b); err != nil {
		return uploadResult{}, err
	}
	if err := w.Close(); err != nil {
		return uploadResult{}, err
	}
	req, err := http.NewRequest("POST", ts.URL+"/api/v1/backups/upload", &buf)
	if err != nil {
		return uploadResult{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
		if c.Name == "zp_csrf" {
			req.Header.Set("X-CSRF-Token", c.Value)
		}
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		return uploadResult{}, err
	}
	defer func() { _ = res.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(res.Body)
	return uploadResult{StatusCode: res.StatusCode, Body: body.String()}, nil
}

// tarGzForTest 用标准库重新打一个 tar.gz（模拟"被改过的归档"）。
func tarGzForTest(dir, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	werr := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		_, err = io.Copy(tw, in)
		return err
	})
	if werr != nil {
		return werr
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

// TestRestoreMergesOffsiteSettingsWithoutTouchingIdentity 是 2026-10-06 用户要求的门禁：
// 备份/恢复必须**默认**把"异地备份（SMTP/FTP）设置"带回来，同时**不许**动本机身份
// （面板后缀 / 监听端口 / 升级源）——那是"整份 config.json 默认不恢复"要挡的东西。
//
// 负向对照：把 runRestore 里的 mergeOffsiteFromArchive 调用去掉，第一条断言立刻红。
func TestRestoreMergesOffsiteSettingsWithoutTouchingIdentity(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()

	// 备份那一刻的现场：配好异地备份（含口令与收件人），本机身份是 A 套。
	srv.Cfg.Offsite = &offsite.Settings{
		Enabled: true, Protocol: "smtp", Host: "smtp.backup-host.example", Port: 465,
		Encryption: "ssl", Username: "backup-user", Password: "backup-pass",
		From: "panel@example.com", To: "me@example.com", MaxFileMB: 20,
	}
	identitySuffix := srv.Cfg.PanelSuffix
	if err := srv.Cfg.Save(); err != nil {
		t.Fatal(err)
	}

	res := makeArchive(t, srv, []string{backup.TargetPanel})

	// 恢复前把本机改成另一套：异地备份清空、身份字段换掉（模拟"换了台机器"）。
	srv.Cfg.Offsite = nil
	srv.Cfg.Listen = ":9999"
	if err := srv.Cfg.Save(); err != nil {
		t.Fatal(err)
	}

	logLines := []string{}
	logf := func(level, text string) { logLines = append(logLines, level+": "+text) }
	rep, err := srv.runRestore(ctx, logf, res.Path, false)
	if err != nil {
		t.Fatalf("恢复失败: %v\n日志:\n%s", err, strings.Join(logLines, "\n"))
	}

	// ① 异地备份设置回来了（含口令与收件人）—— 这是用户点名的"应该包括"。
	if srv.Cfg.Offsite == nil {
		t.Fatalf("恢复后异地备份（SMTP）设置没回来（用户报障的原形）\n日志:\n%s", strings.Join(logLines, "\n"))
	}
	if srv.Cfg.Offsite.Host != "smtp.backup-host.example" || srv.Cfg.Offsite.To != "me@example.com" {
		t.Errorf("异地备份设置内容不对：%+v", *srv.Cfg.Offsite)
	}
	if srv.Cfg.Offsite.Password != "backup-pass" {
		t.Errorf("异地备份口令没跟着恢复（恢复后还要重填）：%q", srv.Cfg.Offsite.Password)
	}
	// 报告里要如实说"这是局部合并恢复的"，别让用户以为整份 config 都换了。
	merged := strings.Join(rep.Merged, "；")
	if !strings.Contains(merged, "异地备份") {
		t.Errorf("恢复报告没写清异地备份是局部合并恢复的：%v", rep.Merged)
	}
	// ② 本机身份字段一个字都不许动。
	if srv.Cfg.Listen != ":9999" {
		t.Errorf("本机监听端口被备份覆盖了（默认策略要求保持本机身份）：%q", srv.Cfg.Listen)
	}
	if srv.Cfg.PanelSuffix != identitySuffix {
		t.Errorf("本机面板后缀被覆盖了：%q（应为 %q）", srv.Cfg.PanelSuffix, identitySuffix)
	}
	if rep.ConfigRestored {
		t.Error("没勾选恢复 config.json 时不许标记 ConfigRestored")
	}
}

// TestRestoreBringsBackNavPage：导航页（导航分组/条目/设置）是**面板数据的一部分**，
// 必须跟着备份与恢复一起回来（用户 2026-10-06："备份导航页非常重要"）。
//
// 它们在 panel.db 里（nav_groups / nav_items / nav_settings），而数据库是**单事务整表复制**
// 恢复的 —— 这个门禁就是证明这条链真的覆盖到它们，而不是"应该覆盖"。
func TestRestoreBringsBackNavPage(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()

	if _, err := srv.Store.DB().Exec(`INSERT INTO nav_groups(id,name,sort) VALUES(1,'我的服务',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store.DB().Exec(
		`INSERT INTO nav_items(group_id,name,url,icon,description,sort) VALUES(1,'面板','https://127.0.0.1:8443/','🧭','本机面板',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store.DB().Exec(
		`INSERT INTO nav_settings(key,value) VALUES('title','我的导航'),('nav_listen_port','8896')`); err != nil {
		t.Fatal(err)
	}

	res := makeArchive(t, srv, []string{backup.TargetPanel})

	// 破坏现场：导航页数据全没了（相当于换机/误删）
	if _, err := srv.Store.DB().Exec(`DELETE FROM nav_items; DELETE FROM nav_groups; DELETE FROM nav_settings;`); err != nil {
		t.Fatal(err)
	}

	logLines := []string{}
	logf := func(level, text string) { logLines = append(logLines, level+": "+text) }
	if _, err := srv.runRestore(ctx, logf, res.Path, false); err != nil {
		t.Fatalf("恢复失败: %v\n日志:\n%s", err, strings.Join(logLines, "\n"))
	}

	var name, url string
	if err := srv.Store.DB().QueryRow(`SELECT name,url FROM nav_items WHERE group_id=1`).Scan(&name, &url); err != nil {
		t.Fatalf("恢复后导航条目没回来（导航页是面板数据的一部分）：%v", err)
	}
	if name != "面板" || url != "https://127.0.0.1:8443/" {
		t.Errorf("导航条目内容不对：%q / %q", name, url)
	}
	var gname string
	if err := srv.Store.DB().QueryRow(`SELECT name FROM nav_groups WHERE id=1`).Scan(&gname); err != nil {
		t.Fatalf("恢复后导航分组没回来：%v", err)
	}
	if gname != "我的服务" {
		t.Errorf("导航分组内容不对：%q", gname)
	}
	var title string
	if err := srv.Store.DB().QueryRow(`SELECT value FROM nav_settings WHERE key='title'`).Scan(&title); err != nil {
		t.Fatalf("恢复后导航设置没回来：%v", err)
	}
	if title != "我的导航" {
		t.Errorf("导航设置内容不对：%q", title)
	}
}

// TestRestoreBringsBackNavIconFiles：导航页上传的**图标文件**（<DataDir>/nav-icons/）
// 必须进备份、并且能被恢复 —— 它们不在数据库快照里，只备份库的话恢复后图标全裂。
//
// 用户 2026-10-06："备份导航页非常重要"。这条是当天查出的真缺口：图标目录既不在
// 备份覆盖名单里、也不在排除名单里（门禁当时没抓到，因为它用常量拼路径）。
func TestRestoreBringsBackNavIconFiles(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()

	iconDir := filepath.Join(srv.Cfg.DataDir, "nav-icons")
	if err := os.MkdirAll(iconDir, 0o700); err != nil {
		t.Fatal(err)
	}
	iconPath := filepath.Join(iconDir, "abcdef0123456789.png")
	const iconBytes = "PNG-ICON-CONTENT"
	if err := os.WriteFile(iconPath, []byte(iconBytes), 0o600); err != nil {
		t.Fatal(err)
	}

	res := makeArchive(t, srv, []string{backup.TargetPanel})

	// 破坏现场：图标文件被删（换机恢复的常见形态）
	if err := os.RemoveAll(iconDir); err != nil {
		t.Fatal(err)
	}

	logLines := []string{}
	logf := func(level, text string) { logLines = append(logLines, level+": "+text) }
	if _, err := srv.runRestore(ctx, logf, res.Path, false); err != nil {
		t.Fatalf("恢复失败: %v\n日志:\n%s", err, strings.Join(logLines, "\n"))
	}

	b, err := os.ReadFile(iconPath)
	if err != nil {
		t.Fatalf("恢复后导航图标文件没回来（导航页图标会全裂）：%v", err)
	}
	if string(b) != iconBytes {
		t.Errorf("图标内容不对：%q", string(b))
	}
}

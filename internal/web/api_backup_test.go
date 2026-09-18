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

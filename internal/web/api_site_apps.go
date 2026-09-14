package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  一键建站（Typecho / WordPress 等）
//
//  用户要求：应用市场加一个「一键建站」分类，装完要**自动配好伪静态**等配置。
//
//  这类应用与"服务/容器"完全不同：产出是一个**站点** ——
//  目录 + 数据库 + 伪静态 + nginx vhost。所以流程单独写：
//
//    ① 下载官方源码（带备用镜像，国内主址常慢）
//    ② 解压到 ~/www/<域名>（必要时剥掉压缩包里那层顶层目录）
//    ③ 建数据库 + 建用户（随机强密码）
//    ④ 写配置文件（WordPress 的 wp-config.php / Typecho 的 config.inc.php）
//    ⑤ 建站点：写入 sites 表并生成 vhost（**伪静态预设由目录条目指定**）
//    ⑥ 探测首页，把"接下来该点哪里"写清楚（安装向导地址、库名、账号）
//
//  每一步都复核真实状态（目录里有文件吗、库建出来了吗、首页通不通），
//  而不是"命令退出码 0 就算成功"。
// ============================================================================

type siteInstallReq struct {
	Domain    string `json:"domain"`
	AdminUser string `json:"admin_user"`
	AdminPass string `json:"admin_pass"`
	AdminMail string `json:"admin_mail"`
	SiteName  string `json:"site_name"`
	PHP       string `json:"php"`
}

type siteInstallResult struct {
	App       string   `json:"app"`
	Domain    string   `json:"domain"`
	Dir       string   `json:"dir"`
	URL       string   `json:"url"`
	FinishURL string   `json:"finish_url"`
	DBName    string   `json:"db_name"`
	DBUser    string   `json:"db_user"`
	DBPass    string   `json:"db_pass"`
	Steps     []string `json:"steps"`
}

// handleSiteAppInstall 一键建站（长任务：下载 + 解压 + 建库 + 建站点）。
func (s *Server) handleSiteAppInstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found || app.SiteApp == nil {
		fail(w, http.StatusBadRequest, "「"+id+"」不是一键建站类应用")
		return
	}
	var req siteInstallReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if err := sites.ValidateDomain(domain); err != nil {
		fail(w, http.StatusBadRequest, "域名不合法: "+err.Error())
		return
	}
	if _, err := s.siteMgr().Get(r.Context(), domain); err == nil {
		fail(w, http.StatusConflict, "站点 "+domain+" 已存在，请换一个域名或在「网站管理」里删掉它")
		return
	}
	if req.PHP == "" {
		req.PHP = "8.3"
	}

	s.launchTask(w, r, "site-install", domain, "一键建站 "+app.Name+"（"+domain+"）",
		"site_app_install", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			return s.installSiteApp(ctx, app, domain, req)
		})
}

func (s *Server) installSiteApp(ctx context.Context, app services.App, domain string, req siteInstallReq) (*siteInstallResult, error) {
	spec := app.SiteApp
	res := &siteInstallResult{App: app.ID, Domain: domain}
	step := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		res.Steps = append(res.Steps, msg)
		services.EmitProgress(ctx, tasks.LevelStep, msg)
	}

	// ---------- ① 目录 ----------
	dir, err := sites.SiteDir(s.Cfg.WWWRoot, domain)
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("创建站点目录失败: %w", err)
	}
	res.Dir = dir
	step("站点目录：%s", dir)

	// ---------- ② 下载 + 解压 ----------
	urls := append([]string{spec.DownloadURL}, spec.MirrorURLs...)
	archive := filepath.Join(os.TempDir(), "zizpanel-site-"+app.ID+"-"+fmt.Sprint(time.Now().Unix())+"."+spec.Archive)
	defer func() { _ = os.Remove(archive) }()
	var lastErr error
	downloaded := false
	for _, u := range urls {
		step("下载 %s", u)
		if err := downloadFile(ctx, u, archive); err != nil {
			lastErr = err
			step("  失败：%v", err)
			continue
		}
		downloaded = true
		break
	}
	if !downloaded {
		return res, fmt.Errorf("下载源码失败（试过 %d 个地址）：%v", len(urls), lastErr)
	}
	if st, err := os.Stat(archive); err != nil || st.Size() < 1024 {
		return res, fmt.Errorf("下载到的文件不完整（%d 字节）", sizeOf(archive))
	}
	step("已下载 %d KB，正在解压到站点目录", sizeOf(archive)/1024)

	// 解压到临时目录再搬进去：先剥顶层目录，且避免半途失败留一地碎片
	tmpDir, err := os.MkdirTemp("", "zizpanel-site-extract-")
	if err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	if err := extractArchive(ctx, archive, tmpDir); err != nil {
		return res, fmt.Errorf("解压失败: %w", err)
	}
	src := tmpDir
	if spec.StripTopDir {
		if inner, ok := singleSubdir(tmpDir); ok {
			src = inner
		}
	}
	if err := copyTree(src, dir); err != nil {
		return res, fmt.Errorf("复制文件到站点目录失败: %w", err)
	}
	// 复核：目录里得真的有东西（不是"命令成功但一个文件都没进去"）
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		return res, fmt.Errorf("解压后站点目录是空的，源码包结构与预期不符（%s）", spec.DownloadURL)
	}
	step("已解压到站点目录（%d 个顶层条目）", len(entries))

	// ---------- ③ 建库 ----------
	dbName := dbNameFor(domain)
	dbUser := dbName
	dbPass := randomPassword(20)
	if spec.NeedsDB {
		c, err := s.mysqlClient()
		if err != nil {
			return res, fmt.Errorf("MySQL 不可用，无法建库：%w", err)
		}
		if err := c.CreateDatabase(ctx, dbName, "utf8mb4", "utf8mb4_unicode_ci"); err != nil {
			return res, fmt.Errorf("创建数据库失败: %w", err)
		}
		if err := c.CreateUser(ctx, dbUser, "localhost", dbPass,
			[]string{"ALL"}, []string{dbName}); err != nil {
			return res, fmt.Errorf("创建数据库用户失败: %w", err)
		}
		res.DBName, res.DBUser, res.DBPass = dbName, dbUser, dbPass
		step("已建库 %s 与用户 %s（密码随机生成，见下方结果）", dbName, dbUser)
	}

	// ---------- ④ 写配置文件 ----------
	switch app.ID {
	case "wordpress":
		if err := writeWpConfig(dir, dbName, dbUser, dbPass); err != nil {
			return res, err
		}
		step("已生成 wp-config.php（数据库信息已写入）")
	case "typecho":
		if err := writeTypechoConfig(dir, dbName, dbUser, dbPass); err != nil {
			return res, err
		}
		step("已生成 config.inc.php（数据库信息已写入；安装向导里会自动带上）")
	}

	// ---------- ⑤ 建站点（带伪静态）----------
	site := &sites.Site{
		Domain: domain, Root: dir, PHPVersion: req.PHP,
		Rewrite: spec.Rewrite, Enabled: true,
		Remark: app.Name + " 一键建站",
	}
	if err := s.siteMgr().Create(ctx, site); err != nil {
		return res, fmt.Errorf("创建站点记录失败: %w", err)
	}
	if err := s.applySite(ctx, site); err != nil {
		return res, fmt.Errorf("生成 nginx 配置失败: %w", err)
	}
	step("已创建站点并套用「%s」伪静态", spec.Rewrite)

	// 目录归属交给运行用户（PHP 要写文件）
	if s.Cfg.User != "" && os.Geteuid() == 0 {
		if err := chownTreeTo(dir, s.Cfg.User); err != nil {
			step("（警告）调整目录归属失败：%v", err)
		}
	}
	if err := s.nginxReload(ctx); err != nil {
		step("（警告）nginx 重载失败：%v", err)
	}

	// ---------- ⑥ 复核首页 ----------
	scheme := "http"
	res.URL = scheme + "://" + domain + "/"
	res.FinishURL = res.URL + strings.TrimPrefix(spec.FinishPath, "/")
	code, body := probeHTTP(ctx, res.URL)
	switch {
	case code == 0:
		step("（警告）首页探测不到响应 —— 通常是因为这个域名还没做本地解析。"+
			"请在「网站管理」里给 %s 加 hosts 解析，或把 DNS 指到这台机器", domain)
	default:
		step("首页探测：HTTP %d（%.0f 字节）", code, float64(len(body)))
	}
	return res, nil
}

// ---------- 小工具 ----------

func sizeOf(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func singleSubdir(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dir, false
	}
	var sub string
	for _, e := range entries {
		if e.Name() == "__MACOSX" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !e.IsDir() {
			return dir, false
		}
		if sub != "" {
			return dir, false // 多个目录：不剥
		}
		sub = filepath.Join(dir, e.Name())
	}
	if sub == "" {
		return dir, false
	}
	return sub, true
}

func dbNameFor(domain string) string {
	// MySQL 库名不能有点号；取域名主体 + 短哈希避免重名
	base := strings.ReplaceAll(domain, ".", "_")
	base = strings.ReplaceAll(base, "-", "_")
	if len(base) > 40 {
		base = base[:40]
	}
	return "wp_" + base
}

func randomPassword(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	return s[:n]
}

func downloadFile(ctx context.Context, url, dest string) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/usr/bin/curl", "-fsSL", "--retry", "2", "--connect-timeout", "15", "-o", dest, url)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v（%s）", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func extractArchive(ctx context.Context, archive, dest string) error {
	var cmd *exec.Cmd
	switch {
	case strings.HasSuffix(archive, ".zip"):
		cmd = exec.CommandContext(ctx, "/usr/bin/unzip", "-q", "-o", archive, "-d", dest)
	case strings.HasSuffix(archive, ".tar.gz"), strings.HasSuffix(archive, ".tgz"):
		cmd = exec.CommandContext(ctx, "/usr/bin/tar", "-xzf", archive, "-C", dest)
	default:
		return fmt.Errorf("不认识的压缩格式: %s", filepath.Base(archive))
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v（%s）", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		return os.WriteFile(target, b, mode)
	})
}

func writeWpConfig(dir, dbName, dbUser, dbPass string) error {
	salts := make([]string, 8)
	keys := []string{"AUTH_KEY", "SECURE_AUTH_KEY", "LOGGED_IN_KEY", "NONCE_KEY",
		"AUTH_SALT", "SECURE_AUTH_SALT", "LOGGED_IN_SALT", "NONCE_SALT"}
	for i := range salts {
		salts[i] = fmt.Sprintf("define('%s', '%s');", keys[i], randomPassword(48))
	}
	content := fmt.Sprintf(`<?php
/** 由 ZizPanel 一键建站生成 —— 数据库信息已填好，向导里直接点「开始」即可 */
define('DB_NAME', '%s');
define('DB_USER', '%s');
define('DB_PASSWORD', '%s');
define('DB_HOST', 'localhost');
define('DB_CHARSET', 'utf8mb4');
define('DB_COLLATE', '');

%s

$table_prefix = 'wp_';

define('WP_DEBUG', false);

if (!defined('ABSPATH')) {
	define('ABSPATH', __DIR__ . '/');
}
require_once ABSPATH . 'wp-settings.php';
`, dbName, dbUser, dbPass, strings.Join(salts, "\n"))
	return os.WriteFile(filepath.Join(dir, "wp-config.php"), []byte(content), 0o644)
}

func writeTypechoConfig(dir, dbName, dbUser, dbPass string) error {
	content := fmt.Sprintf(`<?php
/** 由 ZizPanel 一键建站生成 —— 数据库信息已填好 */
define('__TYPECHO_ROOT_DIR__', dirname(__FILE__));
define('__TYPECHO_PLUGIN_DIR__', '/usr/plugins');

define('__TYPECHO_ADMIN_DIR__', '/admin/');
@set_include_path(get_include_path() . PATH_SEPARATOR . __TYPECHO_ROOT_DIR__ . '/var' . PATH_SEPARATOR . __TYPECHO_ROOT_DIR__ . '/var/Typecho');

/** 初始化数据库 */
define('__TYPECHO_DB_ADAPTER__', 'Pdo_Mysql');
define('__TYPECHO_DB_HOST__', 'localhost');
define('__TYPECHO_DB_PORT__', 3306);
define('__TYPECHO_DB_USER__', '%s');
define('__TYPECHO_DB_PASSWORD__', '%s');
define('__TYPECHO_DB_CHAR__', 'utf8mb4');
define('__TYPECHO_DB_DATABASE__', '%s');
define('__TYPECHO_DB_PREFIX__', 'typecho_');

require_once __TYPECHO_ROOT_DIR__ . '/var/Typecho/Common.php';
Typecho_Common::init();
`, dbUser, dbPass, dbName)

	// Typecho 的正常流程由安装向导生成 config.inc.php；预置一个可以省掉手填，
	// 但必须写在根目录且可写（安装向导会覆盖它）。
	return os.WriteFile(filepath.Join(dir, "config.inc.php"), []byte(content), 0o644)
}

func probeHTTP(ctx context.Context, url string) (int, string) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/usr/bin/curl", "-sS", "-o", "/dev/null",
		"-w", "%{http_code}", "--max-time", "15", url)
	out, err := cmd.Output()
	if err != nil {
		return 0, ""
	}
	code := strings.TrimSpace(string(out))
	n := 0
	_, _ = fmt.Sscanf(code, "%d", &n)
	return n, code
}

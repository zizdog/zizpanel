package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  站点管理接口
//
//  职责划分：
//    - 面板侧（本文件）：校验输入、生成 nginx 配置、维护数据库、编排操作顺序
//    - 助手侧（zizpanel-helper）：白名单路径内原子落盘、语法校验、reload
//
//  所有会改动 nginx 的操作都遵循同一个安全顺序：
//    写配置（helper 内部校验并允许回滚）→ reload → 失败则还原数据库状态
// ============================================================================

// siteLogDir 返回站点日志目录。约定与现有 LNMP 环境一致（~/www/_logs）。
func (s *Server) siteLogDir() string {
	return filepath.Join(s.Cfg.WWWRoot, "_logs")
}

// siteMgr 构造站点管理器。
func (s *Server) siteMgr() *sites.Manager {
	return sites.NewManager(s.Store, sites.Options{LogDir: s.siteLogDir()})
}

// helperCall 调用提权助手并返回解析后的结果（包级函数，便于其它模块复用）。
func helperCall(ctx context.Context, args ...string) (map[string]any, error) {
	bin := "/opt/zizpanel/bin/zizpanel-helper"
	if v := os.Getenv("ZIZPANEL_WORKDIR"); v != "" {
		bin = filepath.Join(filepath.Dir(v), "bin", "zizpanel-helper")
	}
	return helperCallBin(ctx, bin, args...)
}

// helperCallBin 用指定路径的助手执行。
func helperCallBin(ctx context.Context, bin string, args ...string) (map[string]any, error) {
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("提权助手不存在: %s", bin)
	}
	cmdArgs := args
	if os.Geteuid() != 0 {
		cmdArgs = append([]string{"-n", bin}, args...)
		bin = "/usr/bin/sudo"
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := execCommand(ctx, bin, cmdArgs...)
	// stderr 必须单独收集并带进错误信息。
	// 提权失败最常见的原因是 sudoers 规则不对/未生效，而原因只写在 stderr 上
	// （例如 "sudo: a password is required"）。只用 Output() 会把它丢掉，
	// 用户看到的是一句毫无信息量的 "exit status 1 ()"，根本没法排查。
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	var parsed map[string]any
	if len(out) > 0 {
		_ = jsonUnmarshal(out, &parsed)
	}
	if err != nil {
		if parsed != nil {
			if msg, _ := parsed["error"].(string); msg != "" {
				return parsed, errors.New(msg)
			}
		}
		detail := strings.TrimSpace(stderrBuf.String())
		if detail == "" {
			detail = strings.TrimSpace(string(out))
		}
		if detail == "" {
			detail = "助手没有输出任何信息"
		}
		return parsed, fmt.Errorf("调用提权助手失败: %v（%s）", err, detail)
	}
	if parsed == nil {
		// 退出码 0 但输出不是 JSON：不能当成成功，
		// 否则调用方会以为操作生效了（例如"站点已创建"）。
		return nil, fmt.Errorf("提权助手返回了无法解析的结果: %s", strings.TrimSpace(string(out)))
	}
	if okv, _ := parsed["ok"].(bool); !okv {
		msg, _ := parsed["error"].(string)
		if msg == "" {
			msg, _ = parsed["msg"].(string)
		}
		return parsed, errors.New(msg)
	}
	return parsed, nil
}

// callHelper 调用提权助手并解析 JSON 结果。
//
// 助手必须由 root 调用（sudoers 白名单）或当前进程本身就是 root。
// 具体执行逻辑统一在 helperCallBin 里，这里只负责解析路径：
// 两份重复实现曾经分叉过（一处修了错误信息、另一处没修），不要再复制。
func (s *Server) callHelper(ctx context.Context, args ...string) (map[string]any, error) {
	return helperCallBin(ctx, s.Cfg.ServicePath("zizpanel-helper"), args...)
}

// writeVhost 把配置写入 nginx vhost 目录（经助手，含语法校验与回滚）。
func (s *Server) writeVhost(ctx context.Context, domain, content string) error {
	bin := s.Cfg.ServicePath("zizpanel-helper")
	args := []string{"vhost-write", domain}
	if os.Geteuid() != 0 {
		args = append([]string{"-n", bin}, args...)
		bin = "/usr/bin/sudo"
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := execCommand(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	var parsed map[string]any
	if len(out) > 0 {
		_ = jsonUnmarshal(out, &parsed)
	}
	if err != nil {
		if parsed != nil {
			if msg, _ := parsed["error"].(string); msg != "" {
				return errors.New(msg)
			}
		}
		return fmt.Errorf("写入配置失败: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// nginxReload 重载 nginx。
func (s *Server) nginxReload(ctx context.Context) error {
	_, err := s.callHelper(ctx, "nginx-reload")
	return err
}

// applySite 生成并应用一个站点的配置。
//
// 顺序很关键：先生成内容 → 让 helper 写入并校验 → reload。
// helper 在写入前会做 nginx -t，不通过会回滚文件，
// 因此这里不需要额外处理"配置已写坏"的情况。
func (s *Server) applySite(ctx context.Context, site *sites.Site) error {
	pass := ""
	if site.PHPVersion != "" {
		pass = sites.ResolveFastCGI(s.Cfg.BrewPrefix, site.PHPVersion)
	}
	content, err := site.Generate(sites.Options{
		LogDir:      s.siteLogDir(),
		FastCGIPass: pass,
	})
	if err != nil {
		return err
	}
	if err := s.writeVhost(ctx, site.Domain, content); err != nil {
		return err
	}
	if err := s.nginxReload(ctx); err != nil {
		return fmt.Errorf("配置已写入但 nginx 重载失败: %w", err)
	}
	return nil
}

// ensureSiteRoot 创建站点根目录。
//
// 面板以 root 运行，但站点目录必须归属真实用户，否则用户无法用编辑器/上传文件。
func (s *Server) ensureSiteRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("创建站点目录失败: %w", err)
	}
	// 归属真实用户（root 运行时 $HOME 不同，必须用配置里的用户名）
	if s.Cfg.User != "" && s.Cfg.User != "root" {
		if uid, gid, err := lookupIDs(s.Cfg.User); err == nil {
			_ = os.Chown(root, uid, gid)
		}
	}
	return nil
}

// ---------- HTTP 接口 ----------

// handleSiteList 列出所有站点（含实时状态）。
func (s *Server) handleSiteList(w http.ResponseWriter, r *http.Request) {
	mgr := s.siteMgr()
	list, err := mgr.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取站点失败: "+err.Error())
		return
	}
	// 探测每个站点的 nginx 配置是否已存在（数据库与文件是否一致）
	type item struct {
		*sites.Site
		ConfExists bool `json:"conf_exists"`
		Running    bool `json:"running"`
	}
	out := make([]item, 0, len(list))
	vhostDir := s.Cfg.VhostDir
	for _, st := range list {
		confPath := filepath.Join(vhostDir, st.Domain+".conf")
		_, err := os.Stat(confPath)
		out = append(out, item{Site: st, ConfExists: err == nil, Running: err == nil && st.Enabled})
	}
	ok(w, map[string]any{
		"list":         out,
		"www_root":     s.Cfg.WWWRoot,
		"log_dir":      s.siteLogDir(),
		"vhost_dir":    vhostDir,
		"presets":      sites.RewritePresets,
		"php_versions": s.detectPHPVersions(r.Context()),
	})
}

// detectPHPVersions 探测本机已安装的 PHP 版本。
//
// 关键处理：按"实际运行的 FPM 地址"去重，而不是按 formula 名逐个列出。
//
// 踩过的坑：Homebrew 的 `php` 是版本别名（当前指向 8.4），
// 而用户可能同时装了 php@8.3 并让 8.3 占着 9000 端口。
// 按 formula 名列出时，界面会出现两个都声称监听 9000 的版本；
// 用户选了"8.4"但实际由 9000 上的 8.3 处理，
// 表现为"我明明选了 8.4，跑的却是 8.3" —— 极难排查。
//
// 因此这里：
//  1. 以 fastcgi 地址为主键去重
//  2. 额外探测该地址上真实运行的版本（X-Powered-By），让不一致直接可见
func (s *Server) detectPHPVersions(ctx context.Context) []sites.PHPVersion {
	optDir := filepath.Join(s.Cfg.BrewPrefix, "opt")
	entries, err := os.ReadDir(optDir)
	if err != nil {
		return []sites.PHPVersion{}
	}

	byPass := map[string]sites.PHPVersion{}
	var order []string

	for _, e := range entries {
		name := e.Name()
		if name != "php" && !strings.HasPrefix(name, "php@") {
			continue
		}
		version := "8.3"
		if v, ok := strings.CutPrefix(name, "php@"); ok {
			version = v
		} else if name == "php" {
			// php 是版本别名：解析软链接拿到真实版本号
			if link, err := os.Readlink(filepath.Join(optDir, name)); err == nil {
				base := filepath.Base(link)
				if v, ok := strings.CutPrefix(base, "php@"); ok {
					version = v
				} else if base != "php" {
					version = base
				}
			}
		}
		if version == "" {
			continue
		}

		pass := sites.ResolveFastCGI(s.Cfg.BrewPrefix, version)
		pv := sites.PHPVersion{
			Version:   version,
			Service:   name,
			Binary:    filepath.Join(optDir, name, "bin", "php"),
			FPMConf:   filepath.Join(s.Cfg.BrewPrefix, "etc", "php", version, "php-fpm.d", "www.conf"),
			Pass:      pass,
			IsDefault: name == s.Cfg.PHPSvc,
		}
		if port := sites.ParsePort(pass); port > 0 {
			pv.Running = isPortListening(ctx, port)
		} else if strings.HasPrefix(pass, "unix:") {
			_, err := os.Stat(strings.TrimPrefix(pass, "unix:"))
			pv.Running = err == nil
		}

		// 同一 fastcgi 地址只保留一个条目：
		// 运行中的优先；都运行时优先带 @ 版本号的那个（信息更明确）
		if prev, exists := byPass[pass]; exists {
			takeNew := (pv.Running && !prev.Running) ||
				(pv.Running == prev.Running && !strings.Contains(prev.Service, "@") && strings.Contains(pv.Service, "@"))
			if takeNew {
				byPass[pass] = pv
			}
			continue
		}
		byPass[pass] = pv
		order = append(order, pass)
	}

	out := make([]sites.PHPVersion, 0, len(order))
	for _, pass := range order {
		out = append(out, byPass[pass])
	}
	s.probePHPVersions(ctx, out)
	return out
}

// probePHPVersions 通过一次真实请求探测当前 FPM 实际运行的版本。
//
// 让"配置里写的版本"与"实际处理的版本"不一致时一眼可见。
func (s *Server) probePHPVersions(ctx context.Context, list []sites.PHPVersion) {
	if len(list) == 0 {
		return
	}
	probeDir := filepath.Join(s.Cfg.WWWRoot, "_default")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return
	}
	probeFile := filepath.Join(probeDir, "__zp_ver.php")
	if err := os.WriteFile(probeFile, []byte("<?php echo PHP_VERSION;"), 0o644); err != nil {
		return
	}
	defer func() { _ = os.Remove(probeFile) }()

	// 用 -D - 把响应头打到 stdout，从中读 X-Powered-By
	// 用 Host: localhost 命中默认 server（它能处理 PHP）
	rctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := execCommand(rctx, "/usr/bin/curl", "-sS", "-D", "-", "-o", "/dev/null",
		"--max-time", "5", "-H", "Host: localhost",
		"http://127.0.0.1/__zp_ver.php").Output()
	if err != nil {
		return
	}
	actual := parsePoweredBy(string(out))
	if actual == "" {
		return
	}
	for i := range list {
		if sameMajorMinor(list[i].Version, actual) {
			list[i].ActualVersion = actual
		}
	}
}

// parsePoweredBy 从响应头里提取 X-Powered-By 的版本号。
func parsePoweredBy(headers string) string {
	for _, ln := range strings.Split(headers, "\n") {
		ln = strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		lower := strings.ToLower(ln)
		v, ok := strings.CutPrefix(lower, "x-powered-by:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if rest, ok := strings.CutPrefix(v, "php/"); ok {
			return strings.TrimSpace(rest)
		}
		return v
	}
	return ""
}

// majorMinor 从 "8.4.7" 提取 "8.4"。
func majorMinor(v string) string {
	v = strings.TrimSpace(v)
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

func sameMajorMinor(a, b string) bool {
	return majorMinor(a) == majorMinor(b)
}

type siteCreateReq struct {
	Domain     string `json:"domain"`
	Aliases    string `json:"aliases"`
	PHPVersion string `json:"php_version"`
	Rewrite    string `json:"rewrite"`
	ProxyPass  string `json:"proxy_pass"`
	Remark     string `json:"remark"`
	// CreateDir 为 false 时不创建目录（站点根目录可能已存在）
	CreateDir *bool `json:"create_dir"`
}

// handleSiteCreate 新建站点。
func (s *Server) handleSiteCreate(w http.ResponseWriter, r *http.Request) {
	var req siteCreateReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	if err := sites.ValidateDomain(req.Domain); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	mgr := s.siteMgr()
	if _, err := mgr.Get(r.Context(), req.Domain); err == nil {
		fail(w, http.StatusConflict, "站点 "+req.Domain+" 已存在")
		return
	}

	root, err := sites.SiteDir(s.Cfg.WWWRoot, req.Domain)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// 伪静态为 Laravel/ThinkPHP 时，运行目录要落到 public 子目录
	runRoot := root
	if p, okk := sites.RewritePresetByName(req.Rewrite); okk && p.PublicDir != "" {
		runRoot = filepath.Join(root, p.PublicDir)
	}

	site := &sites.Site{
		Domain:     req.Domain,
		Aliases:    strings.TrimSpace(req.Aliases),
		Root:       runRoot,
		PHPVersion: req.PHPVersion,
		Rewrite:    req.Rewrite,
		ProxyPass:  strings.TrimSpace(req.ProxyPass),
		Remark:     strings.TrimSpace(req.Remark),
		Enabled:    true,
	}

	createDir := true
	if req.CreateDir != nil {
		createDir = *req.CreateDir
	}
	if createDir {
		if err := s.ensureSiteRoot(runRoot); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Laravel/ThinkPHP 的 public 目录需要存在，否则 nginx 会 404
		if runRoot != root {
			if err := s.ensureSiteRoot(root); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if err := s.seedIndexPHP(runRoot, site); err != nil {
			s.Log.Warn("写入默认首页失败: %v", err)
		}
	}

	// 先落数据库（拿到 ID），再写 nginx 配置；配置失败则回滚数据库
	if err := mgr.Create(r.Context(), site); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.applySite(r.Context(), site); err != nil {
		// 回滚：删掉刚建的记录，避免出现"数据库有、nginx 没有"的不一致状态
		_ = mgr.Delete(r.Context(), site.Domain)
		s.audit(r, "site_create", site.Domain, "创建失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "创建站点失败: "+err.Error())
		return
	}

	s.audit(r, "site_create", site.Domain,
		fmt.Sprintf("根目录=%s PHP=%s 伪静态=%s", runRoot, req.PHPVersion, req.Rewrite), true, "")
	ok(w, map[string]any{"site": site, "root": runRoot})
}

// seedIndexPHP 为新站点写入一个初始首页，方便建完立刻能看到效果。
func (s *Server) seedIndexPHP(root string, site *sites.Site) error {
	indexPath := filepath.Join(root, "index.php")
	if !strings.HasSuffix(site.Root, "public") && site.PHPVersion == "" {
		// 纯静态站点写 index.html
		indexPath = filepath.Join(root, "index.html")
	}
	if _, err := os.Stat(indexPath); err == nil {
		return nil // 已有首页，不动它
	}
	var content string
	if strings.HasSuffix(indexPath, ".php") {
		content = fmt.Sprintf(`<?php
// 站点 %s 的默认首页，由 ZizPanel 创建。
// 部署你的程序时可以直接删除本文件，或覆盖整个目录。
$ip = $_SERVER['SERVER_ADDR'] ?? 'localhost';
echo "<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">";
echo "<title>%s</title><style>body{font-family:-apple-system,'PingFang SC',sans-serif;";
echo "display:grid;place-items:center;height:100vh;margin:0;background:#0f1116;color:#e6e9f0}";
echo ".c{text-align:center}h1{margin:0 0 10px;font-size:22px}";
echo "p{color:#9aa3b5;margin:4px 0;font-size:14px}code{background:#1b1f2a;padding:2px 6px;border-radius:4px}</style>";
echo "</head><body><div class=\"c\"><h1>%s</h1>";
echo "<p>站点已创建成功，PHP 运行正常</p>";
echo "<p>PHP 版本：<code>" . PHP_VERSION . "</code></p>";
echo "<p>站点目录：<code>%s</code></p>";
echo "<p>现在可以把程序文件放到该目录，或直接删除本文件。</p>";
echo "</div></body></html>";
`, site.Domain, site.Domain, site.Domain, root)
	} else {
		content = fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>%s</title>
<style>
  body { font-family: -apple-system, "PingFang SC", sans-serif; display: grid;
         place-items: center; height: 100vh; margin: 0; background: #0f1116; color: #e6e9f0; }
  .c { text-align: center; }
  h1 { margin: 0 0 10px; font-size: 22px; }
  p { color: #9aa3b5; margin: 4px 0; font-size: 14px; }
  code { background: #1b1f2a; padding: 2px 6px; border-radius: 4px; }
</style>
</head>
<body>
  <div class="c">
    <h1>%s</h1>
    <p>站点已创建成功</p>
    <p>站点目录：<code>%s</code></p>
    <p>现在可以把网站文件放到该目录，或直接删除本文件。</p>
  </div>
</body>
</html>
`, site.Domain, site.Domain, root)
	}
	if err := os.WriteFile(indexPath, []byte(content), 0o644); err != nil {
		return err
	}
	if s.Cfg.User != "" && s.Cfg.User != "root" {
		if uid, gid, err := lookupIDs(s.Cfg.User); err == nil {
			_ = os.Chown(indexPath, uid, gid)
		}
	}
	return nil
}

// handleSiteGet 返回单个站点的完整信息（含配置内容）。
func (s *Server) handleSiteGet(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	confPath := filepath.Join(s.Cfg.VhostDir, domain+".conf")
	conf, _ := os.ReadFile(confPath)
	pass := ""
	if site.PHPVersion != "" {
		pass = sites.ResolveFastCGI(s.Cfg.BrewPrefix, site.PHPVersion)
	}
	generated, genErr := site.Generate(sites.Options{
		LogDir: s.siteLogDir(), FastCGIPass: pass,
	})
	ok(w, map[string]any{
		"site":         site,
		"conf":         string(conf),
		"conf_path":    confPath,
		"generated":    generated,
		"generate_err": errString(genErr),
		"fastcgi_pass": pass,
		"log_dir":      s.siteLogDir(),
		"access_log":   filepath.Join(s.siteLogDir(), domain+".access.log"),
		"error_log":    filepath.Join(s.siteLogDir(), domain+".error.log"),
		"presets":      sites.RewritePresets,
		"php_versions": s.detectPHPVersions(r.Context()),
	})
}

type siteUpdateReq struct {
	Aliases    *string `json:"aliases"`
	PHPVersion *string `json:"php_version"`
	Rewrite    *string `json:"rewrite"`
	ProxyPass  *string `json:"proxy_pass"`
	ExtraConf  *string `json:"extra_conf"`
	Remark     *string `json:"remark"`
	Enabled    *bool   `json:"enabled"`
}

// handleSiteUpdate 更新站点并重新生成配置。
func (s *Server) handleSiteUpdate(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	var req siteUpdateReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	before := *site // 用于失败回滚
	if req.Aliases != nil {
		site.Aliases = strings.TrimSpace(*req.Aliases)
	}
	if req.PHPVersion != nil {
		site.PHPVersion = strings.TrimSpace(*req.PHPVersion)
	}
	if req.Rewrite != nil {
		site.Rewrite = strings.TrimSpace(*req.Rewrite)
	}
	if req.ProxyPass != nil {
		site.ProxyPass = strings.TrimSpace(*req.ProxyPass)
	}
	if req.ExtraConf != nil {
		site.ExtraConf = *req.ExtraConf
	}
	if req.Remark != nil {
		site.Remark = strings.TrimSpace(*req.Remark)
	}
	if req.Enabled != nil {
		site.Enabled = *req.Enabled
	}

	// 伪静态切到 Laravel/ThinkPHP 时，运行目录要跟着切到 public；
	// 切回来则要退回站点根目录。这里按"域名目录 + 模板要求"重新推导，
	// 避免用户改模板后目录还停在 public 导致 404。
	baseDir, err := sites.SiteDir(s.Cfg.WWWRoot, site.Domain)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if p, okk := sites.RewritePresetByName(site.Rewrite); okk && p.PublicDir != "" {
		site.Root = filepath.Join(baseDir, p.PublicDir)
	} else if strings.HasPrefix(site.Root, baseDir) {
		site.Root = baseDir
	}

	if err := mgr.Update(r.Context(), site); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if site.Enabled {
		if err := s.applySite(r.Context(), site); err != nil {
			// 回滚数据库，保证"数据库状态 = nginx 实际状态"
			_ = mgr.Update(r.Context(), &before)
			_ = s.applySite(r.Context(), &before)
			s.audit(r, "site_update", domain, "更新失败: "+err.Error(), false, "")
			fail(w, http.StatusInternalServerError, "更新失败: "+err.Error())
			return
		}
	} else {
		// 停用：从 nginx 移除配置并重载
		if _, err := s.callHelper(r.Context(), "vhost-delete", domain); err != nil {
			s.Log.Warn("删除 vhost 失败: %v", err)
		}
		if err := s.nginxReload(r.Context()); err != nil {
			s.Log.Warn("重载 nginx 失败: %v", err)
		}
	}

	s.audit(r, "site_update", domain, "更新站点配置", true, "")
	ok(w, map[string]any{"site": site})
}

// handleSiteDelete 删除站点。
func (s *Server) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	removeFiles := r.URL.Query().Get("remove_files") == "1"

	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}

	if _, err := s.callHelper(r.Context(), "vhost-delete", domain); err != nil {
		s.Log.Warn("删除 vhost 失败: %v", err)
	}
	if err := mgr.Delete(r.Context(), domain); err != nil {
		fail(w, http.StatusInternalServerError, "删除站点记录失败: "+err.Error())
		return
	}
	if err := s.nginxReload(r.Context()); err != nil {
		s.Log.Warn("重载 nginx 失败: %v", err)
	}

	var filesMsg string
	if removeFiles {
		// 只允许删除站点根目录（严格限定在 www 根下，且必须是该域名的目录）
		base := filepath.Join(s.Cfg.WWWRoot, domain)
		cleanBase := filepath.Clean(base)
		if filepath.Dir(cleanBase) != filepath.Clean(s.Cfg.WWWRoot) {
			filesMsg = "目录路径校验失败，未删除文件"
		} else if err := os.RemoveAll(cleanBase); err != nil {
			filesMsg = "删除文件失败: " + err.Error()
		} else {
			filesMsg = "已删除站点目录"
		}
	}

	// 审计里记录被删站点的关键信息，便于事后追溯"删掉了什么"
	detail := fmt.Sprintf("删除站点（根目录=%s，SSL=%v，PHP=%s）%s",
		site.Root, site.SSLEnabled, site.PHPVersion, filesMsg)
	s.audit(r, "site_delete", domain, detail, true, "")
	ok(w, map[string]any{"msg": "站点已删除", "files": filesMsg})
}

// ---------- SSL ----------

type siteSSLReq struct {
	Provider string   `json:"provider"` // self / mkcert / manual
	Cert     string   `json:"cert"`
	Key      string   `json:"key"`
	ExtraSAN []string `json:"extra_san"`
	Enable   bool     `json:"enable"`
}

// handleSiteSSL 为站点签发/配置证书。
//
// 目前支持两种自动签发：
//   - self   ：openssl 自签（无需任何外部依赖，浏览器会提示不受信任）
//   - mkcert ：使用 mkcert 的本地 CA（在已信任 mkcert CA 的机器上无提示）
//
// Let's Encrypt 需要公网域名与 80 端口可达，计划在后续阶段接入 lego。
func (s *Server) handleSiteSSL(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	var req siteSSLReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	certDir := filepath.Join(s.Cfg.DataDir, "site-certs", domain)
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, "创建证书目录失败: "+err.Error())
		return
	}
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")

	switch req.Provider {
	case "self":
		if _, err := s.callHelper(r.Context(), "site-cert-self",
			"--domain", domain, "--cert", certPath, "--key", keyPath); err != nil {
			fail(w, http.StatusInternalServerError, "签发自签证书失败: "+err.Error())
			return
		}
	case "mkcert":
		if _, err := s.callHelper(r.Context(), "mkcert-issue",
			"--hosts", strings.Join(append([]string{domain}, req.ExtraSAN...), ","),
			"--cert", certPath, "--key", keyPath); err != nil {
			fail(w, http.StatusInternalServerError,
				"mkcert 签发失败: "+err.Error()+"（可先执行 brew install mkcert nss && mkcert -install）")
			return
		}
	case "manual":
		if req.Cert == "" || req.Key == "" {
			fail(w, http.StatusBadRequest, "手工模式需要提供证书与私钥内容")
			return
		}
		if err := os.WriteFile(certPath, []byte(req.Cert), 0o644); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(keyPath, []byte(req.Key), 0o600); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		fail(w, http.StatusBadRequest, "不支持的证书来源: "+req.Provider)
		return
	}

	// 读取证书到期时间，便于前端提示续期
	expires := ""
	if res, err := s.callHelper(r.Context(), "site-cert-info", certPath); err == nil {
		if data, okk := res["data"].(map[string]any); okk {
			expires, _ = data["expires"].(string)
		}
	}

	site.SSLEnabled = true
	site.SSLCert = certPath
	site.SSLKey = keyPath
	site.SSLProvider = req.Provider
	site.SSLExpires = expires

	if err := mgr.Update(r.Context(), site); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.applySite(r.Context(), site); err != nil {
		fail(w, http.StatusInternalServerError, "证书已签发但应用配置失败: "+err.Error())
		return
	}

	s.audit(r, "site_ssl", domain, "签发证书 provider="+req.Provider+" 到期="+expires, true, "")
	ok(w, map[string]any{"site": site, "cert": certPath, "key": keyPath, "expires": expires})
}

// handleSiteSSLDisable 关闭 SSL。
func (s *Server) handleSiteSSLDisable(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	site.SSLEnabled = false
	site.SSLCert = ""
	site.SSLKey = ""
	site.SSLProvider = ""
	site.SSLExpires = ""
	if err := mgr.Update(r.Context(), site); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.applySite(r.Context(), site); err != nil {
		fail(w, http.StatusInternalServerError, "已关闭 SSL 但应用配置失败: "+err.Error())
		return
	}
	s.audit(r, "site_ssl_disable", domain, "关闭 SSL", true, "")
	ok(w, map[string]any{"site": site})
}

// ---------- 校验与诊断 ----------

// handleSiteCheck 用真实 HTTP 请求检查站点是否可访问，并检测 PHP 是否被当静态文件吐出。
func (s *Server) handleSiteCheck(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result := checkSite(ctx, site, s.siteLogDir())
	ok(w, result)
}

// handleNginxTest 校验整个 nginx 配置。
func (s *Server) handleNginxTest(w http.ResponseWriter, r *http.Request) {
	res, err := s.callHelper(r.Context(), "nginx-test")
	if err != nil {
		ok(w, map[string]any{"ok": false, "output": err.Error()})
		return
	}
	msg, _ := res["msg"].(string)
	ok(w, map[string]any{"ok": true, "output": msg})
}

// handleNginxStatus 返回 nginx 运行状态与站点数量统计。
func (s *Server) handleNginxStatus(w http.ResponseWriter, r *http.Request) {
	status := "stopped"
	if res, err := s.callHelper(r.Context(), "nginx-status"); err == nil {
		if m, okk := res["msg"].(string); okk {
			status = m
		}
	}
	mgr := s.siteMgr()
	list, _ := mgr.List(r.Context())
	enabled := 0
	for _, st := range list {
		if st.Enabled {
			enabled++
		}
	}
	ok(w, map[string]any{"status": status, "total": len(list), "enabled": enabled})
}

// handleSiteReload 重新生成所有站点配置并重载 nginx。
//
// 用途：配置文件被手工改坏、或升级面板后需要按最新模板重建。
func (s *Server) handleSiteReload(w http.ResponseWriter, r *http.Request) {
	mgr := s.siteMgr()
	list, err := mgr.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var rebuilt, failed []string
	for _, site := range list {
		if !site.Enabled {
			// 停用的站点应确保配置不存在
			_, _ = s.callHelper(r.Context(), "vhost-delete", site.Domain)
			continue
		}
		if err := s.applySite(r.Context(), site); err != nil {
			failed = append(failed, site.Domain+": "+err.Error())
			continue
		}
		rebuilt = append(rebuilt, site.Domain)
	}
	if err := s.nginxReload(r.Context()); err != nil {
		failed = append(failed, "reload: "+err.Error())
	}
	s.audit(r, "site_reload", "all",
		fmt.Sprintf("重建 %d 个站点，失败 %d 个", len(rebuilt), len(failed)), len(failed) == 0, "")
	ok(w, map[string]any{"rebuilt": rebuilt, "failed": failed})
}

// handleNginxEnvRepair 修复 nginx 环境（WebSocket map 与 conf.d include）。
func (s *Server) handleNginxEnvRepair(w http.ResponseWriter, r *http.Request) {
	res, err := s.callHelper(r.Context(), "nginx-ensure-env")
	if err != nil {
		fail(w, http.StatusInternalServerError, "修复失败: "+err.Error())
		return
	}
	msg, _ := res["msg"].(string)
	// 修复后重载，让变更生效
	reloadErr := s.nginxReload(r.Context())
	s.audit(r, "nginx_env_repair", "nginx", msg, reloadErr == nil, "")
	ok(w, map[string]any{"msg": msg, "reload_ok": reloadErr == nil})
}

// ---------- 站点日志 ----------

// handleSiteLog 返回站点访问/错误日志内容。
func (s *Server) handleSiteLog(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	kind := r.URL.Query().Get("kind")
	if kind != "error" {
		kind = "access"
	}
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			lines = n
		}
	}
	path := filepath.Join(s.siteLogDir(), fmt.Sprintf("%s.%s.log", domain, kind))
	if filepath.Base(domain) != domain {
		fail(w, http.StatusBadRequest, "非法域名")
		return
	}
	content, total, err := tailFile(path, lines)
	if err != nil {
		if os.IsNotExist(err) {
			ok(w, map[string]any{
				"content": "", "path": path, "lines": 0,
				"msg": "日志文件尚不存在（站点可能还没有被访问过）",
			})
			return
		}
		fail(w, http.StatusInternalServerError, "读取日志失败: "+err.Error())
		return
	}
	ok(w, map[string]any{"content": content, "path": path, "lines": total})
}

// ---------- 小工具 ----------

// tailFile 读取文件末尾若干行，返回内容与总行数。
func tailFile(path string, lines int) (string, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	all := strings.Split(string(b), "\n")
	// 末尾通常是空行，去掉以免显示出一行空白
	if len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	total := len(all)
	if total > lines {
		all = all[total-lines:]
	}
	return strings.Join(all, "\n"), total, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isPortListening 判断 TCP 端口是否在监听。
func isPortListening(ctx context.Context, port int) bool {
	if port <= 0 {
		return false
	}
	// 直接尝试连接，比调用 lsof 更快、也不需要权限
	ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	d := netDialer()
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Shutdown 在面板退出时清理资源。
//
// 必须关掉终端会话：它们持有真实的 shell 子进程，
// 面板退出后这些 shell 如果继续存在，就成了没人管的孤儿进程，
// 而且它们的审计上下文（哪个用户开的）也丢失了。
func (s *Server) Shutdown() {
	if s.termMgr != nil {
		s.termMgr.CloseAll()
	}
}

// Startup 在面板启动时做一次环境准备与自愈。
//
// 做三件事：
//  1. 把 upgrade map 的内容源注入 priv 包（保持单一数据源，避免两处硬编码）
//  2. 确保站点日志目录存在（nginx 打不开日志目录会直接启动失败）
//  3. 确保 nginx 有 WebSocket 升级 map 与 conf.d include
//
// 这些在安装脚本里也会做一遍，但启动时再检查一次能覆盖
// "用户换了 nginx 配置""重装了 nginx""恢复了旧备份"等情况。
func (s *Server) Startup(ctx context.Context) {
	priv.SetUpgradeMapContent(sites.UpgradeMapConf())

	logDir := s.siteLogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		s.Log.Warn("创建站点日志目录失败 %s: %v", logDir, err)
	}

	s.ensureNginxEnvOnStart(ctx)

	// 空闲终端会话回收：
	// WebSocket 断开时会关闭会话，但网络异常（客户端崩溃、断网）
	// 可能导致连接一直挂着。这里做兜底清理。
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if s.termMgr != nil {
				if n := s.termMgr.ReapIdle(); n > 0 {
					s.Log.Info("回收空闲终端会话 %d 个", n)
				}
			}
		}
	}()
}

// ensureNginxEnvOnStart 在面板启动时确保 nginx 具备所需环境。
//
// 这一步是"自愈"：即使安装脚本没跑、或用户换了 nginx 配置，
// 面板启动后也会把 WebSocket map 与 conf.d include 补齐，
// 否则反向代理站点会在 nginx -t 阶段直接失败。
func (s *Server) ensureNginxEnvOnStart(ctx context.Context) {
	if os.Geteuid() != 0 {
		// 非 root 时尝试通过助手（sudoers 已授权）
		if _, err := s.callHelper(ctx, "nginx-ensure-env"); err != nil {
			s.Log.Warn("nginx 环境自愈失败（反向代理可能不可用）: %v", err)
		}
		return
	}
	msg, err := priv.EnsureNginxEnv()
	if err != nil {
		s.Log.Warn("nginx 环境自愈失败（反向代理可能不可用）: %v", err)
		return
	}
	// 只有真的改动了 nginx 配置才重载。
	// 每次启动无条件 reload 会在开机时给所有站点造成一次无谓的请求抖动。
	if priv.NginxEnvChanged(msg) {
		if err := s.nginxReload(ctx); err != nil {
			s.Log.Warn("nginx 环境已更新但重载失败: %v", err)
		} else {
			s.Log.Info("nginx 环境已更新并重载: %s", msg)
		}
		return
	}
	s.Log.Info("nginx 环境检查: %s", msg)
}

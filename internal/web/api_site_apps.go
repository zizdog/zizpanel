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
	App    string `json:"app"`
	Domain string `json:"domain"`
	Dir    string `json:"dir"`
	URL    string `json:"url"`
	// Version / Source 如实告诉用户"装的是哪个固定版本、实际走的哪个源"
	// （镜像站 / 官方源）。留空表示这个应用还没登记固定版本。
	Version   string `json:"version,omitempty"`
	Source    string `json:"source,omitempty"`
	FinishURL string `json:"finish_url"`
	DBName    string `json:"db_name"`
	DBUser    string `json:"db_user"`
	DBPass    string `json:"db_pass"`
	// PackageSize / PackageSHA256 是**实算回读**的发行包指纹（Piwigo 要求记录并回读）。
	PackageSize   int64    `json:"package_size,omitempty"`
	PackageSHA256 string   `json:"package_sha256,omitempty"`
	Steps         []string `json:"steps"`
	// Message 是收尾说明（一句话告诉用户"还差哪一步"）。前端优先展示它，
	// 因为 Steps 里那句警告很容易被淹没（真机 2026-09-17：用户反代成功后看到 500 就懵了）。
	Message string `json:"message,omitempty"`
}

// sitePackageFetchFn 下载并校验固定版本源码包。
//
// 默认实现走 services.Manager 的"镜像站优先 → 回落官方 → SHA256 校验"
// （见 services/site_sources.go）；单测注入假实现，绝不联网（AGENTS.md 第三节）。
var sitePackageFetchFn = func(s *Server, ctx context.Context, appID, dest string,
	logf func(string)) (services.SiteSource, string, error) {
	return s.svcManager().DownloadSitePackage(ctx, appID, dest, logf)
}

// siteHomeProbeFn 探测站点首页（单测注入假实现，不真发请求）。
var siteHomeProbeFn = func(ctx context.Context, url string) (int, string) {
	return probeHTTP(ctx, url)
}

// sitePHPPreflightFn 是"一键建站前置条件检查"的注入点。
//
// 默认真实执行：解析该版本真实的 php 二进制、核对最低版本、必需的 PHP 扩展、
// 以及站点根目录可写。缺任何一项都在**建目录/下载之前**拒绝，绝不装出一个
// 打不开的站点。单测注入假实现（不跑真实 php、不碰真实 brew 前缀）。
var sitePHPPreflightFn = func(s *Server, appName string, spec *services.SiteAppSpec, phpVersion string) error {
	return checkSiteAppRequirements(s, appName, spec, phpVersion)
}

// checkSiteAppRequirements 实现一键建站的前置条件检查（MinPHP / PHPExts / 目录可写）。
//
// 判据贴着"运行体"：真的去跑本机该版本的 php 二进制，而不是猜扩展装没装。
// spec 没声明任何要求时直接通过（既有 typecho / wordpress 不受影响）。
func checkSiteAppRequirements(s *Server, appName string, spec *services.SiteAppSpec, phpVersion string) error {
	if spec == nil || (spec.MinPHP == "" && len(spec.PHPExts) == 0) {
		return nil
	}
	bin := s.phpBinaryFor(phpVersion)
	if bin == "" {
		return fmt.Errorf("「%s」需要 PHP %s，但本机找不到它的可执行文件"+
			"（已找 %s/opt/php@%s/bin/php）——请先在「应用市场」安装 PHP %s，或在一键建站时选一个已安装的版本",
			appName, phpVersion, s.Cfg.BrewPrefix, phpVersion, phpVersion)
	}
	actual, err := phpVersionOf(bin)
	if err != nil {
		return fmt.Errorf("无法执行 PHP %s（%s）：%w", phpVersion, bin, err)
	}
	if spec.MinPHP != "" && !phpVersionAtLeast(actual, spec.MinPHP) {
		return fmt.Errorf("「%s」要求 PHP >= %s，但本机 PHP %s 实际是 %s —— 请选更高的 PHP 版本或先安装新版本",
			appName, spec.MinPHP, phpVersion, actual)
	}
	if len(spec.PHPExts) > 0 {
		have, err := phpModules(bin)
		if err != nil {
			return fmt.Errorf("读取 PHP %s 的扩展列表失败（%s）：%w", actual, bin, err)
		}
		var missing []string
		for _, e := range spec.PHPExts {
			if !have[strings.ToLower(e)] {
				missing = append(missing, e)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("「%s」需要 PHP %s，但当前 PHP %s 缺少必需扩展：%s —— "+
				"缺扩展时站点会打不开，所以这里直接拒绝安装；请补齐这些扩展后重试",
				appName, phpVersion, actual, strings.Join(missing, "、"))
		}
	}
	if root := strings.TrimSpace(s.Cfg.WWWRoot); root != "" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("站点根目录 %s 不可用：%w", root, err)
		}
		f, err := os.CreateTemp(root, ".zp-writecheck-*")
		if err != nil {
			return fmt.Errorf("站点根目录 %s 不可写（%v）—— 一键建站需要在该目录下创建站点目录", root, err)
		}
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}
	return nil
}

// phpBinaryFor 返回某个 PHP 版本对应的 php 可执行文件路径（空 = 找不到）。
//
// 只认 Homebrew 每个版本独立的 keg：{brew}/opt/php@<ver>/bin/php；只有配置里的
// 默认 PHP 服务就是该版本时，才允许退回 {brew}/bin/php（那个别名随时可能指向别的版本）。
func (s *Server) phpBinaryFor(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ""
	}
	cands := []string{filepath.Join(s.Cfg.BrewPrefix, "opt", "php@"+version, "bin", "php")}
	if s.Cfg.PHPSvc == "php@"+version || s.Cfg.PHPSvc == "php" {
		cands = append(cands, filepath.Join(s.Cfg.BrewPrefix, "bin", "php"))
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// phpVersionOf 跑 `php -r 'echo PHP_VERSION;'` 取真实版本。
func phpVersionOf(bin string) (string, error) {
	cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "-r", "echo PHP_VERSION;").Output()
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("php 没有输出版本号")
	}
	return v, nil
}

// phpModules 跑 `php -m` 取扩展名集合（小写）。
func phpModules(bin string) (map[string]bool, error) {
	cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "-m").Output()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		have[strings.ToLower(line)] = true
	}
	return have, nil
}

// phpVersionAtLeast 判断 actual（如 "8.2.33"）是否 >= min（如 "8.2"）。
func phpVersionAtLeast(actual, min string) bool {
	pa, pb := parsePHPVersion(actual), parsePHPVersion(min)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return true
}

// parsePHPVersion 把 "8.2.33" 拆成 [8,2,33]（缺位补 0）。
func parsePHPVersion(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(strings.TrimSpace(v), ".", 3) {
		if i >= 3 {
			break
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

// handleSiteAppInstall 一键建站（长任务：下载 + 解压 + 建库 + 建站点）。
func (s *Server) handleSiteAppInstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := siteAppByID(id)
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
		// 默认 PHP 版本与「一键 LNMP」装的版本保持一致（用户 2026-09-17：默认 8.2）。
		// 不写死成别的：站点向导里能选的版本来自本机实际安装列表，
		// 这里只是"用户没填"时的兜底，兜错会让新建的站点 502。
		req.PHP = "8.2"
	}

	// 前置条件检查放在**开任务之前**：缺 PHP 版本/扩展时直接 400 + 说清缺什么，
	// 而不是让用户等一个必然失败的任务（installSiteApp 里还会再挡一次，纵深防御）。
	if err := sitePHPPreflightFn(s, app.Name, app.SiteApp, req.PHP); err != nil {
		s.audit(r, "site_app_install", domain, "前置检查未通过: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, "安装前检查未通过："+err.Error())
		return
	}

	s.launchTask(w, r, "site-install", domain, "一键建站 "+app.Name+"（"+domain+"）",
		"site_app_install", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res, err := s.installSiteApp(ctx, app, domain, req)
			// 源码包要从镜像站/GitHub 下：网络类失败附统一提示（见 services/netfail.go）。
			return res, services.AppendNetworkHint(err)
		})
}

// siteInstallArtifacts 记录"本次一键建站**真正创建出来**的东西"。
//
// 失败清理只碰这里记着的东西，且必须满足"本次创建 **且** 为空/无数据"：
//   - 已有目录、已有数据库、已有账号一律不动（绝不删用户已有数据）；
//   - 数据库只有在**本次创建**且**表数为 0** 时才允许删；
//   - 目录只有在本次创建且**为空**时才允许删（解压出来的源码会保留）；
//   - 站点记录由这里删除；本次写入的 vhost 由 applySite 自己回滚。
//
// 只有这样才能保证"失败了还能干净地再点一次"：不会被
// database exists / 站点已存在 这类上一次的残留永久挡住。
type siteInstallArtifacts struct {
	domain      string
	dirCreated  bool
	dbCreated   bool
	dbName      string
	userCreated bool
	dbUser      string
	siteCreated bool
}

func (s *Server) installSiteApp(ctx context.Context, app services.App, domain string, req siteInstallReq) (res *siteInstallResult, err error) {
	spec := app.SiteApp
	res = &siteInstallResult{App: app.ID, Domain: domain}
	// 前置条件（PHP 版本 / 扩展 / 目录可写）不满足时，**在建目录之前**如实失败。
	// 放在这里而不只是在 HTTP 层，是纵深防御：任务中心、未来调用方都可能直接调它。
	// 注意：这里**不替调用方补默认 PHP 版本** —— 站点为空 PHP 时是"纯静态站点"，
	// 那是既有语义（HTTP 层已经在开任务前把默认值填好了）。
	if err := sitePHPPreflightFn(s, app.Name, spec, req.PHP); err != nil {
		return res, fmt.Errorf("安装前检查未通过：%w", err)
	}
	step := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		res.Steps = append(res.Steps, msg)
		services.EmitProgress(ctx, tasks.LevelStep, msg)
	}
	// 应用专属行为（库名规则 / 预写配置 / 指纹回读 / 收尾验证）：默认应用一样都没有。
	extra, hasExtra := siteAppExtraFor(app.ID)
	art := &siteInstallArtifacts{domain: domain}
	defer func() {
		if err == nil {
			return
		}
		// 清理用 WithoutCancel：任务超时/用户关页面时 ctx 会被取消，
		// 但残留（空库、空目录、站点记录）必须清掉，否则重试被自己挡住。
		s.rollbackSiteInstall(context.WithoutCancel(ctx), art, step)
	}()

	// ---------- ① 目录 ----------
	dir, err := sites.SiteDir(s.Cfg.WWWRoot, domain)
	if err != nil {
		return res, err
	}
	// 先看这个目录是不是本来就存在 —— 只清理**本次创建**的目录。
	if _, serr := os.Stat(dir); os.IsNotExist(serr) {
		art.dirCreated = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("创建站点目录失败: %w", err)
	}
	res.Dir = dir
	step("站点目录：%s", dir)

	// ---------- ② 下载 + 解压 ----------
	//
	// 固定版本 + 镜像站优先 + 强制 SHA256（见 services/site_sources.go）。
	// 原实现直接 curl 目录条目里的地址、不校验哈希；审计还发现 Typecho 那个
	// 写死的 jsdelivr 备用地址早已 404 —— 一旦 GitHub 不通就完全装不上。
	logf := func(msg string) { step("%s", msg) }
	pinned, hasPinned := services.SiteSourceFor(app.ID)
	archiveName := "zizpanel-site-" + app.ID + "-" + fmt.Sprint(time.Now().Unix())
	if hasPinned {
		// 临时文件名是给 extractArchive **按后缀判格式**用的：
		// filepath.Ext("FreshRSS-1.30.0.tar.gz") 只给出 ".gz"，解压器会以
		// "不认识的压缩格式" 失败（FreshRSS 就是这样）。所以复合后缀要保住，
		// 实在认不出来就退回目录声明的归档类型。
		lower := strings.ToLower(pinned.File)
		switch {
		case strings.HasSuffix(lower, ".tar.gz"):
			archiveName += ".tar.gz"
		case strings.HasSuffix(lower, ".tgz"):
			archiveName += ".tgz"
		case strings.HasSuffix(lower, ".zip"):
			archiveName += ".zip"
		default:
			archiveName += "." + spec.Archive
		}
	} else {
		archiveName += "." + spec.Archive
	}
	archive := filepath.Join(os.TempDir(), archiveName)
	defer func() { _ = os.Remove(archive) }()

	if hasPinned {
		// 已登记固定版本：只走实测过的地址（镜像站 → 官方 → 加速），
		// 每个地址下完都核对真实 sha256，绝不把 404/坏文件当成"可用备源"。
		fetched, label, err := sitePackageFetchFn(s, ctx, app.ID, archive, logf)
		if err != nil {
			return res, err
		}
		res.Version, res.Source = fetched.Version, label
		step("源码包已就绪：%s %s ← %s", fetched.Name, fetched.Version, label)
		if hasExtra && extra.recordFingerprint {
			// 记录并回读真实大小/sha256：下载器写完的字节必须自己再证明一次（坑 224）。
			size, sum, ferr := recordArchiveFingerprint(archive, fetched, step)
			if ferr != nil {
				return res, ferr
			}
			res.PackageSize, res.PackageSHA256 = size, sum
		}
	} else {
		// 未登记固定版本的站点应用：保持原行为（目录条目里的官方 + 备用地址）。
		urls := append([]string{spec.DownloadURL}, spec.MirrorURLs...)
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
	// 库名/用户名默认由域名派生；有专属规则的应用（Piwigo）用带站点后缀的专用名。
	dbName, dbUser := dbNameFor(domain), dbNameFor(domain)
	if hasExtra && extra.dbIdentifiers != nil {
		dbName, dbUser = extra.dbIdentifiers(domain)
	}
	dbPass := randomPassword(20)
	if spec.NeedsDB {
		c, err := s.mysqlClient()
		if err != nil {
			return res, fmt.Errorf("MySQL 不可用，无法建库：%w", err)
		}
		if err := c.CreateDatabase(ctx, dbName, "utf8mb4", "utf8mb4_unicode_ci"); err != nil {
			return res, fmt.Errorf("创建数据库失败: %w", err)
		}
		// 库里的一切都是本次创建的：失败清理时只要确认它还是空的就删掉。
		art.dbCreated, art.dbName = true, dbName
		if err := c.CreateUser(ctx, dbUser, "localhost", dbPass,
			[]string{"ALL"}, []string{dbName}); err != nil {
			return res, fmt.Errorf("创建数据库用户失败: %w", err)
		}
		art.userCreated, art.dbUser = true, dbUser
		res.DBName, res.DBUser, res.DBPass = dbName, dbUser, dbPass
		step("已建库 %s 与用户 %s（密码随机生成，见下方结果）", dbName, dbUser)

		// 口令在 MySQL 接受 CREATE USER 的那一刻就真实生效了。后面的写站点配置、
		// 建站点记录、applySite 任何一步失败都会提前 return —— 如果只在最后才记，
		// 这个已存在的账号口令就永远找不回来了。所以先记，后续失败还能在密码列里找回。
		// source=site-install 让前端能标明这条口令的来源。
		if err := s.RememberDBPassword(context.WithoutCancel(ctx), dbUser, "localhost", dbPass, DBCredSourceSiteInstall); err != nil {
			// 账号已在 MySQL 里真实存在 → 不要因此中断建站；但要如实留痕。
			// **警告文本里绝不能出现口令**（install.go 的既有约定：口令只允许出现在 result.Credentials）。
			step("（警告）数据库账号已创建，但面板未能保存口令以便日后在「账号与权限」里回显：%v", err)
		}
	}

	// ---------- ④ 写配置文件 ----------
	//
	// configReady：面板是否**替用户预填**了数据库配置。只有 WordPress / Typecho
	// 走这条路；Flarum / emlog / 可道云由各自的安装向导写自己的配置文件，
	// 所以下面那句"数据库信息已预填"必须按事实说（谎报成功比没做更糟）。
	configReady := false
	switch app.ID {
	case "wordpress":
		if err := writeWpConfig(dir, dbName, dbUser, dbPass); err != nil {
			return res, err
		}
		step("已生成 wp-config.php（数据库信息已写入）")
		configReady = true
	case "typecho":
		if err := writeTypechoConfig(dir, dbName, dbUser, dbPass); err != nil {
			return res, err
		}
		step("已生成 config.inc.php（数据库信息已写入；安装向导里会自动带上）")
		configReady = true
	}
	if hasExtra && extra.writeConfig != nil {
		// 专属配置文件（Piwigo：local/config/database.inc.php，Piwigo 自己的格式）。
		if err := extra.writeConfig(dir, dbName, dbUser, dbPass); err != nil {
			return res, err
		}
		step("已生成 %s 的数据库配置文件", app.Name)
		configReady = true
	}

	// ---------- ⑤ 建站点（带伪静态）----------
	//
	// 站点记录里的 Root 是**运行目录**（nginx docroot），不是源码目录：
	// 伪静态预设声明了 PublicDir 时必须落进那个子目录。少这一步的后果不只是 404 ——
	// FreshRSS 的订阅数据在同级的 data/ 里，docroot 停在源码根就等于**把用户数据
	// 放进 web 根**（2026-09-17 mini 真机实测：/data/tos.example.html 可被 HTTP 读出，
	// 而任务中心还报 succeeded，是典型的"谎报成功"）。
	// handleSiteCreate 与"改伪静态"两条路一直是套 PublicDir 的，只有一键建站这条漏了。
	runRoot := dir
	if p, okk := sites.RewritePresetByName(spec.Rewrite); okk && p.PublicDir != "" {
		runRoot = filepath.Join(dir, p.PublicDir)
	}
	site := &sites.Site{
		Domain: domain, Root: runRoot, PHPVersion: req.PHP,
		Rewrite: spec.Rewrite, Enabled: true,
		Remark: app.Name + " 一键建站",
	}
	if err := s.siteMgr().Create(ctx, site); err != nil {
		return res, fmt.Errorf("创建站点记录失败: %w", err)
	}
	art.siteCreated = true
	if err := s.applySite(ctx, site); err != nil {
		// D23：站点记录已经落库，但 vhost 没生成（或没生效）。这里只返回错误，
		// 真正的回滚（站点记录 / 空数据库 / 空目录）由上面的 defer 统一做 ——
		// 与"下载失败""解压失败"等更早的失败走同一条清理路径，不会再漏。
		return res, fmt.Errorf("生成 nginx 配置失败: %w", err)
	}
	step("已创建站点并套用「%s」伪静态", spec.Rewrite)

	// 目录归属交给运行用户（PHP 要写文件）
	if s.Cfg.User != "" && os.Geteuid() == 0 {
		if err := chownTreeTo(dir, s.Cfg.User); err != nil {
			step("（警告）调整目录归属失败：%v", err)
		}
	}
	// 不再重复 reload：applySite 内部已经是"写 vhost → reload → 请求级复核"，
	// 这里再发一次 reload 既没有新配置可加载，又会让首页探测撞上异步重载的中间态。

	// ---------- ⑥ 复核首页 ----------
	scheme := "http"
	res.URL = scheme + "://" + domain + "/"
	res.FinishURL = res.URL + strings.TrimPrefix(spec.FinishPath, "/")

	// 应用专属收尾验证（Piwigo：站点 200 / 安装向导非 5xx / 库能连）。
	// 任一不过就如实失败（"建站完成但用不了"），由上面的 defer 回滚站点记录与新建空库。
	verifiedByApp := false
	if hasExtra && extra.verify != nil {
		if verr := extra.verify(ctx, s, res, site, spec, step); verr != nil {
			return res, verr
		}
		verifiedByApp = true
	}

	code, body := siteHomeProbeFn(ctx, res.URL)
	switch {
	case code == 0 && verifiedByApp:
		// 上面已用 --resolve 探通（200）；这里是**系统 DNS** 探不到 —— 说清是解析问题，
		// 别让同一份日志里同时出现"验证通过"与"探测不到响应"两句自相矛盾的话。
		step("（提示）系统 DNS 解析不到 %s（本地 hosts/DNS 未配置）：浏览器访问前请先加解析", domain)
	case code == 0:
		step("（警告）首页探测不到响应 —— 通常是因为这个域名还没做本地解析。"+
			"请在「网站管理」里给 %s 加 hosts 解析，或把 DNS 指到这台机器", domain)
	default:
		step("首页探测：HTTP %d（%.0f 字节）", code, float64(len(body)))
	}
	// ⚠️ 必须把"这一步还没做完"说清楚（真机 2026-09-17 用户报障：te.zizdog.com 反代成功、
	// 站点却 500「Database Query Error」）。根因是**面板预置了 config.inc.php**（省掉手填数据库），
	// 而 Typecho/WordPress 看到该文件就认为"已经装好了" —— 于是首页直接去查还不存在的表，
	// 表现成 500，而不是自动跳到安装向导。用户不知道要去 /install.php，就会以为站点坏了。
	if configReady && hasExtra && extra.finishText != nil {
		// Piwigo 的向导**不读**预写的配置文件：不能沿用"数据库信息已预填"那句话。
		st, msg := extra.finishText(app.Name, res.FinishURL)
		step("%s", st)
		res.Message = msg
	} else if configReady {
		step("⚠️ 还差最后一步：打开 %s 走完安装向导（数据库信息已预填）。"+
			"**在向导完成之前，访问站点首页会显示 500/数据库错误**，这是应用以为已安装导致的，不是配置坏了。",
			res.FinishURL)
		res.Message = fmt.Sprintf("「%s」文件与数据库已就绪 —— 请打开 %s 完成安装向导", app.Name, res.FinishURL)
	} else {
		// Flarum / emlog / 可道云：面板**没有**预写配置文件，向导会自己写。
		// 所以要说清"去向导里把上面的库名/账号/口令填进去"，不能谎报"已预填"。
		step("⚠️ 还差最后一步：打开 %s 走完安装向导 —— 向导里数据库地址填 localhost，"+
			"库名/用户名/密码照上面的安装结果填（面板没有替你预写配置文件）。", res.FinishURL)
		res.Message = fmt.Sprintf("「%s」文件与空数据库已就绪 —— 请打开 %s，"+
			"在向导里填安装结果中的数据库信息完成安装", app.Name, res.FinishURL)
	}
	return res, nil
}

// rollbackSiteInstall 清理本次一键建站留下的残留。
//
// 铁律：**只清理本次创建、且确认为空的产物**。
//   - 站点记录：本次建的 → 删；
//   - 数据库：本次建的 **且表数 = 0** → 删；有表 → 保留并明确提示；
//   - 数据库账号：本次建的，且库已经删掉 → 删（否则留着，避免库还在用、账号没了）；
//   - 目录：本次建的 **且为空** → 删；有源码 → 保留并提示（重试会复用）。
//
// 本次写入的 vhost 由 applySite 自己回滚（见 rollbackVhostWrite），
// 这里不再重复处理，避免两处各删一次。
func (s *Server) rollbackSiteInstall(ctx context.Context, art *siteInstallArtifacts, step func(string, ...any)) {
	if art == nil {
		return
	}
	if art.siteCreated {
		if derr := s.siteMgr().Delete(ctx, art.domain); derr != nil {
			step("（警告）回滚站点记录失败：%v，请到「网站管理」手动删除 %s 后再重试", derr, art.domain)
		} else {
			art.siteCreated = false
			step("生成 nginx 配置失败，已回滚站点记录 %s（可换域名或排障后重试）", art.domain)
		}
	}
	if art.dbCreated {
		s.rollbackSiteInstallDB(ctx, art, step)
	}
	if art.dirCreated {
		s.rollbackSiteInstallDir(art, step)
	}
}

// rollbackSiteInstallDB 删除"本次创建的空数据库"（有表一律保留）。
//
// 这正是真机 2026-09-16 那次的根因：一键建站首轮失败留下空库，
// 用户重试被 `ERROR 1007 ... database exists` **永久挡住**。
func (s *Server) rollbackSiteInstallDB(ctx context.Context, art *siteInstallArtifacts, step func(string, ...any)) {
	if art.dbName == "" {
		return
	}
	c, err := s.mysqlClient()
	if err != nil {
		step("（警告）本次创建了数据库 %s，但清理时连不上 MySQL（%v）："+
			"请到「数据库」页确认并手动删除，否则重试会被 \"database exists\" 挡住", art.dbName, err)
		return
	}
	tables, terr := c.ListTables(ctx, art.dbName)
	if terr != nil {
		// 查不到就不敢删 —— 宁可留一个空库让用户手工处理，也不能误删有数据的库。
		step("（警告）无法确认数据库 %s 是否为空（%v）：为安全起见**未删除**，"+
			"请在「数据库」页确认后处理", art.dbName, terr)
		return
	}
	if len(tables) > 0 {
		step("（注意）数据库 %s 里已有 %d 张表，可能已有数据：**保留未删**。"+
			"确认无用后请到「数据库」页删除，再重试建站", art.dbName, len(tables))
		return
	}
	if derr := c.DropDatabase(ctx, art.dbName); derr != nil {
		step("（警告）删除本次创建的空数据库 %s 失败：%v；"+
			"重试建站可能被 \"database exists\" 挡住，请到「数据库」页手动删除", art.dbName, derr)
		return
	}
	step("已删除本次创建的空数据库 %s（重试不会被 \"database exists\" 挡住）", art.dbName)
	if !art.userCreated || art.dbUser == "" {
		return
	}
	if uerr := c.DropUser(ctx, art.dbUser, "localhost"); uerr != nil {
		step("（警告）删除本次创建的数据库账号 %s 失败：%v；请到「数据库 → 账号与权限」手动删除",
			art.dbUser, uerr)
		return
	}
	// 账号都没了，面板里那条"可回显口令"的记录也必须一起清掉，
	// 否则用户会看到一个已经不存在的账号的口令。
	if ferr := s.ForgetDBPassword(ctx, art.dbUser, "localhost"); ferr != nil {
		step("（警告）数据库账号 %s 已删除，但面板没能清掉它的口令记录：%v", art.dbUser, ferr)
	}
	step("已删除本次创建的数据库账号 %s", art.dbUser)
}

// rollbackSiteInstallDir 只删除"本次创建且为空"的站点目录。
//
// 解压出来的源码不是空目录 → 保留并提示。重试时 copyTree 会覆盖同名文件，
// 不会因为目录已存在而失败。
func (s *Server) rollbackSiteInstallDir(art *siteInstallArtifacts, step func(string, ...any)) {
	dir, err := sites.SiteDir(s.Cfg.WWWRoot, art.domain)
	if err != nil {
		return
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		return // 已经不在了/读不到：没有可清理的
	}
	if len(entries) > 0 {
		step("站点目录 %s 里已有 %d 个条目（源码/文件），**保留未删**；重试建站会直接复用该目录",
			dir, len(entries))
		return
	}
	if rerr := os.Remove(dir); rerr != nil {
		step("（警告）删除本次创建的空目录 %s 失败：%v", dir, rerr)
		return
	}
	step("已删除本次创建的空目录 %s", dir)
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

// downloadFile 用 curl 下一个文件（未登记固定版本的站点应用的回落路径）。
//
// 超时是**显式**加的：审计发现原实现只有外层 5 分钟的 ctx、curl 自己没有
// --max-time，卡住时用户要干等 5 分钟而且看不到任何进度。
//
//	--connect-timeout 15   连不上就快点换下一个源
//	--max-time 540         单次尝试最多 9 分钟（外层 ctx 10 分钟兜底）
//	--speed-limit/-time    低于 1KB/s 持续 60 秒即判定停滞，不等满 --max-time
func downloadFile(ctx context.Context, url, dest string) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/usr/bin/curl", "-fsSL", "--retry", "2",
		"--connect-timeout", "15", "--max-time", "540",
		"--speed-limit", "1024", "--speed-time", "60", "-o", dest, url)
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
	// ⚠️ Typecho **1.2 起换了配置格式**：数据库不再由那些 __TYPECHO_DB_* 常量 +
	// Common::init() 配置（1.1 的做法），必须显式 `new \Typecho\Db(...)` +
	// addServer(...) + `\Typecho\Db::set(...)`。
	//
	// 真机事故（2026-09-17 mini，te.zizdog.com 一键建站）：面板原来只写 1.1 那套，
	// 于是 1.2 上 `Common::init()` 不会注册数据库 → 站点首页 500
	// （`Missing Database Object`，install.php:23）、**安装向导永远走不完**
	// （数据库 0 张表）。权威写法来自 Typecho 自己的 install.php
	// `install_config_file()`（它就是这么生成 config.inc.php 的）。
	//
	// 两套都写上、按类是否存在分支：既支持最新版，也不砸掉还在用 1.1 的老站。
	content := fmt.Sprintf(`<?php
/** 由 ZizPanel 一键建站生成 —— 数据库信息已填好 */
define('__TYPECHO_ROOT_DIR__', dirname(__FILE__));
define('__TYPECHO_PLUGIN_DIR__', '/usr/plugins');
define('__TYPECHO_THEME_DIR__', '/usr/themes');
define('__TYPECHO_ADMIN_DIR__', '/admin/');

// ---- Typecho 1.1 的常量写法（新版不再读，留着不冲突）----
define('__TYPECHO_DB_ADAPTER__', 'Pdo_Mysql');
define('__TYPECHO_DB_HOST__', 'localhost');
define('__TYPECHO_DB_PORT__', 3306);
define('__TYPECHO_DB_USER__', '%s');
define('__TYPECHO_DB_PASSWORD__', '%s');
define('__TYPECHO_DB_CHAR__', 'utf8mb4');
define('__TYPECHO_DB_DATABASE__', '%s');
define('__TYPECHO_DB_PREFIX__', 'typecho_');

@set_include_path(get_include_path() . PATH_SEPARATOR . __TYPECHO_ROOT_DIR__ . '/var' . PATH_SEPARATOR . __TYPECHO_ROOT_DIR__ . '/var/Typecho');

require_once __TYPECHO_ROOT_DIR__ . '/var/Typecho/Common.php';

if (class_exists('\Typecho\Db')) {
	// ---- Typecho 1.2+：必须显式注册数据库连接 ----
	\Typecho\Common::init();
	$db = new \Typecho\Db('Pdo_Mysql', 'typecho_');
	$db->addServer(array(
		'host' => 'localhost',
		'port' => 3306,
		'user' => '%s',
		'password' => '%s',
		'charset' => 'utf8mb4',
		'database' => '%s',
		'engine' => 'InnoDB',
	), \Typecho\Db::READ | \Typecho\Db::WRITE);
	\Typecho\Db::set($db);
} elseif (class_exists('Typecho_Common')) {
	// ---- Typecho 1.1：常量 + 下划线类 ----
	Typecho_Common::init();
}
`, dbUser, dbPass, dbName, dbUser, dbPass, dbName)

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

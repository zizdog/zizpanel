package web

// api_sites_piwigo.go —— Piwigo 相册的「一键建站」登记与专属收尾。
//
// 目录条目（services.Catalog）由市场侧合入；这里先做 web 层登记，等目录里有同 ID 条目时
// 门禁会锁死两份声明一致（TestPiwigoDeclarationMatchesCatalog）。坑 223/224。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/mysql"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/sites"
)

const (
	piwigoAppID = "piwigo"
	// piwigoInstallRemark 是面板建站时写下的标记：删除接口据此判断"库是本面板给这个站点建的"。
	piwigoInstallRemark = "Piwigo 一键建站"
	piwigoDBPrefix      = "piwigo_"
	piwigoUserPrefix    = "pw_"
	// piwigoTablePrefix 是 Piwigo 自己的表前缀；删库前用它区分"这是相册的库"。
	piwigoTablePrefix = "piwigo_"
)

// piwigoExtraApp 是 Piwigo 的站点应用声明：PHP 8.2+（官方要求，见 cn.piwigo.org 系统要求）、MySQL。
func piwigoExtraApp() services.App {
	return services.App{
		ID: piwigoAppID, Name: "Piwigo", Icon: "🖼️",
		Summary:     "开源相册，一键建好站点与数据库",
		Description: "Piwigo 相册（PHP + MySQL）：面板建站、建库并解压官方发行包；装完打开安装向导收尾。",
		Category:    services.CategorySite, Kind: services.KindNative,
		SiteApp: &services.SiteAppSpec{
			// 固定版本、不追 latest：滚动地址没法登记 sha256（见 services/site_sources.go）。
			DownloadURL: "https://piwigo.org/download/dlcounter.php?code=16.4.0",
			Archive:     "zip", StripTopDir: true,
			Rewrite: "piwigo", FinishPath: "/install.php", NeedsDB: true,
			// 官方系统要求写的是 PHP 8.2+（7.4 能跑但已停止维护）——低于 8.2 如实拒绝。
			MinPHP: "8.2",
			// 依据上游代码而非文档猜测：install.php 硬要求 mysqli；官方图形库要 gd（或 imagick，
			// 本机 PHP 无 imagick）；其余是核心运行时用到的扩展。
			PHPExts: []string{"mysqli", "gd", "mbstring", "session", "json", "xml", "curl", "openssl", "zip"},
			Notes: []string{
				"安装向导在 /install.php：数据库地址填 localhost，库名/用户名/密码照安装结果填",
				"面板预写了 local/config/database.inc.php；但向导只认表单输入，仍要手填一次",
				"管理员账号由你在向导里设置（面板不预设）",
			},
		},
		DocsURL: "https://piwigo.org",
	}
}

// siteAppExtra 是 web 层为某个站点应用追加的专属行为（默认应用没有这些）。
type siteAppExtra struct {
	app services.App
	// dbIdentifiers 生成专库/专用账号名（默认沿用通用的 dbNameFor）。
	dbIdentifiers func(domain string) (dbName, dbUser string)
	// writeConfig 预写应用自己的数据库配置。
	writeConfig func(dir, dbName, dbUser, dbPass string) error
	// recordFingerprint 要求"记录并回读发行包的真实大小与 sha256"。
	recordFingerprint bool
	// verify 是收尾的真实性验证（HTTP 200 / PHP 非 5xx / 库可连）。
	verify func(ctx context.Context, s *Server, res *siteInstallResult, site *sites.Site,
		spec *services.SiteAppSpec, step func(string, ...any)) error
	// finishText 覆盖"配置文件已预写"时的收尾说明（Piwigo 的向导不读该文件，绝不能谎报已预填）。
	finishText func(appName, finishURL string) (step, message string)
}

var siteAppExtras = map[string]siteAppExtra{
	piwigoAppID: {
		app:               piwigoExtraApp(),
		dbIdentifiers:     piwigoDBIdentifiers,
		writeConfig:       writePiwigoConfig,
		recordFingerprint: true,
		verify:            verifyPiwigoSite,
		finishText:        piwigoFinishText,
	},
}

// piwigoFinishText 如实说明"配置文件写了、但向导不吃它"：上游只认表单输入（坑 223）。
func piwigoFinishText(appName, finishURL string) (string, string) {
	step := "⚠️ 还差最后一步：打开 " + finishURL + " 走完安装向导 —— 数据库地址填 localhost，" +
		"库名/用户名/密码照安装结果填（面板已预写 local/config/database.inc.php，" +
		"但 Piwigo 向导只认表单输入，不会自动带上）。管理员账号由你在向导里设置。"
	msg := "「" + appName + "」文件与空数据库已就绪 —— 请打开 " + finishURL + " 走完安装向导（填库信息、设管理员）"
	return step, msg
}

// siteAppExtraFor 查 web 层登记（没有就是普通站点应用）。
func siteAppExtraFor(id string) (siteAppExtra, bool) {
	e, ok := siteAppExtras[strings.TrimSpace(id)]
	return e, ok
}

// siteAppByID 先查目录，再查 web 层登记：目录里有同 ID 条目时以目录为准。
func siteAppByID(id string) (services.App, bool) {
	if a, ok := services.FindApp(id); ok {
		return a, true
	}
	if e, ok := siteAppExtraFor(id); ok {
		return e.app, true
	}
	return services.App{}, false
}

// piwigoDBIdentifiers 从域名派生"专库 + 专用账号"名：前缀 + 域名主体 + 域名短哈希。
//
// 短哈希保证"长域名截断后同名"也撞不上；两段都过 mysql 的标识符白名单。
func piwigoDBIdentifiers(domain string) (dbName, dbUser string) {
	base := piwigoIdentBase(domain)
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(domain))))
	suffix := hex.EncodeToString(sum[:])[:6]
	return piwigoDBPrefix + base + "_" + suffix, piwigoUserPrefix + base + "_" + suffix
}

// piwigoIdentBase 把域名压成 [a-z0-9_] 主体（最长 20 字节），保证名字不超 MySQL 上限。
func piwigoIdentBase(domain string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(domain)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		s = "site"
	}
	if len(s) > 20 {
		s = s[:20]
	}
	return strings.Trim(s, "_")
}

// writePiwigoConfig 预写 Piwigo 自己的数据库配置（local/config/database.inc.php）。
//
// ⚠️ 绝不能带 PHPWG_INSTALLED：install.php 读到它就 die("already installed")，向导再也进不去。
func writePiwigoConfig(dir, dbName, dbUser, dbPass string) error {
	cfgDir := filepath.Join(dir, "local", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return fmt.Errorf("创建 local/config 失败: %w", err)
	}
	content := fmt.Sprintf(`<?php
// 由 ZizPanel 一键建站生成。不含 PHPWG_INSTALLED：安装向导仍要跑完（表与管理员由它建）。
$conf['dblayer'] = 'mysqli';
$conf['db_base'] = '%s';
$conf['db_user'] = '%s';
$conf['db_password'] = '%s';
$conf['db_host'] = 'localhost';

$prefixeTable = '%s';

?>`+"\n", phpQuote(dbName), phpQuote(dbUser), phpQuote(dbPass), piwigoTablePrefix)
	return os.WriteFile(filepath.Join(cfgDir, "database.inc.php"), []byte(content), 0o640)
}

// phpQuote 转义单引号字符串里的反斜杠与单引号（生成物里的值本来就只含 [a-z0-9_]）。
func phpQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

// recordArchiveFingerprint 记录并**回读**发行包的真实大小/sha256，再与登记值核对。
//
// 回读不是形式：下载器写完文件后可能被截断/替换，只有重算一次才能证明读到的字节一致。
func recordArchiveFingerprint(archive string, pinned services.SiteSource, step func(string, ...any)) (int64, string, error) {
	size := sizeOf(archive)
	sum, err := piwigoFileSHA256(archive)
	if err != nil {
		return 0, "", fmt.Errorf("计算发行包 sha256 失败: %w", err)
	}
	step("发行包：%s，%d 字节，sha256=%s", filepath.Base(archive), size, sum)
	if pinned.Size > 0 && size != pinned.Size {
		return 0, "", fmt.Errorf("发行包大小不符：登记 %d 字节，实际 %d 字节（%s）", pinned.Size, size, filepath.Base(archive))
	}
	if pinned.SHA256 != "" && !strings.EqualFold(sum, pinned.SHA256) {
		return 0, "", fmt.Errorf("发行包 sha256 不符：登记 %s，实际 %s", pinned.SHA256, sum)
	}
	size2 := sizeOf(archive)
	sum2, err := piwigoFileSHA256(archive)
	if err != nil {
		return 0, "", fmt.Errorf("回读发行包失败: %w", err)
	}
	if size2 != size || !strings.EqualFold(sum2, sum) {
		return 0, "", fmt.Errorf("回读发行包不一致：记录 %d 字节/%s，回读 %d 字节/%s", size, sum, size2, sum2)
	}
	step("回读校验通过：大小与 sha256 与记录一致")
	return size, sum, nil
}

// piwigoFileSHA256 实算磁盘文件的 sha256（流式，不把 19 MB 读进内存）。
func piwigoFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------- 收尾验证 ----------

// siteFollowProbeFn / siteDBConnectCheckFn 是收尾验证的两个注入点（单测不真发请求、不连真库）。
var (
	siteFollowProbeFn    = curlSiteFollow
	siteDBConnectCheckFn = func(ctx context.Context, s *Server, dbName, dbUser, dbPass string) error {
		return s.testSiteDBConnect(ctx, dbName, dbUser, dbPass)
	}
)

// curlSiteFollow 取"跟着跳转之后"的最终状态码（--resolve 钉 127.0.0.1，不依赖真实 DNS）。
func curlSiteFollow(ctx context.Context, scheme, domain string, port int, path string, timeout time.Duration) (int, string) {
	url := fmt.Sprintf("%s://%s:%d%s", scheme, domain, port, path)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{
		"-sS", "-k", "-L", "--max-time", strconv.Itoa(int(timeout.Seconds())),
		"--resolve", fmt.Sprintf("%s:%d:127.0.0.1", domain, port),
		"-o", "/dev/null", "-w", "%{http_code}", url,
	}
	out, err := execCommand(ctx, "/usr/bin/curl", args...).Output()
	raw := strings.TrimSpace(string(out))
	if err != nil && raw == "" {
		return 0, raw
	}
	n, _ := strconv.Atoi(raw)
	if n == 0 {
		return 0, raw
	}
	return n, raw
}

// testSiteDBConnect 用**新建的那个账号**连库执行 SELECT 1（面板自己的连接测试）。
//
// 不用面板管理身份代连：那样证明不了应用拿到的口令能用（诚实判据贴运行体）。
func (s *Server) testSiteDBConnect(ctx context.Context, dbName, dbUser, dbPass string) error {
	eng, err := s.databaseEngine()
	if err != nil {
		return err
	}
	binDir := eng.BinDir
	if binDir == "" {
		binDir = fallbackMySQLBinDir(s.Cfg.BrewPrefix)
	}
	host, port, socket, _ := s.mysqlConn()
	if strings.TrimSpace(eng.Socket) != "" {
		socket = eng.Socket
	}
	c := mysql.NewClient(mysql.Options{
		BinDir: binDir, Host: host, Port: port, Socket: socket,
		User: dbUser, Password: dbPass, Timeout: 30 * time.Second,
		UserName: s.Cfg.User, UserHome: s.Cfg.UserHome,
	})
	if _, err := c.Execute(ctx, dbName, "SELECT 1"); err != nil {
		return fmt.Errorf("用新建账号连库失败: %w", err)
	}
	return nil
}

// verifyPiwigoSite 是一键建站的收尾判据：站点首页 200（跟跳转）、PHP 能跑、库能连。
// 任一不过就报"建站完成但用不了"（调用方回滚站点记录与新建空库，源码保留）。
func verifyPiwigoSite(ctx context.Context, s *Server, res *siteInstallResult, site *sites.Site,
	spec *services.SiteAppSpec, step func(string, ...any)) error {
	scheme, port := "http", site.EffectiveListenPort()
	if site.SSLEnabled {
		scheme, port = "https", 443
	}
	var fails []string

	if code, raw := siteFollowProbeFn(ctx, scheme, site.Domain, port, "/", siteProbeTimeout); code == 200 {
		step("站点首页验证通过：HTTP 200（已跟随跳转）")
	} else if code == 0 {
		fails = append(fails, "站点首页无响应（"+orUnknown(raw)+"）")
	} else {
		fails = append(fails, fmt.Sprintf("站点首页 HTTP %d（期待 200）", code))
	}

	finishPath := "/install.php"
	if spec != nil && strings.TrimSpace(spec.FinishPath) != "" {
		finishPath = spec.FinishPath
	}
	if !strings.HasPrefix(finishPath, "/") {
		finishPath = "/" + finishPath
	}
	// 4xx 也算失败：文件在、PHP 却在跑的话不会给 4xx（404 意味着 docroot/文件不对）。
	if code, raw := siteFollowProbeFn(ctx, scheme, site.Domain, port, finishPath, siteProbeTimeout); code >= 200 && code < 400 {
		step("安装向导可达：HTTP %d（%s）", code, finishPath)
	} else if code == 0 {
		fails = append(fails, "安装向导无响应（"+orUnknown(raw)+"）")
	} else {
		fails = append(fails, fmt.Sprintf("安装向导 HTTP %d（%s）——PHP 没跑起来", code, finishPath))
	}

	if res.DBName == "" || res.DBUser == "" {
		fails = append(fails, "安装结果里没有数据库信息，无法验证连接")
	} else if err := siteDBConnectCheckFn(ctx, s, res.DBName, res.DBUser, res.DBPass); err != nil {
		fails = append(fails, "数据库连接失败（"+err.Error()+"）")
	} else {
		step("数据库连接验证通过：%s（账号 %s）", res.DBName, res.DBUser)
	}

	if len(fails) > 0 {
		return fmt.Errorf("建站完成但用不了：%s —— 已回滚站点记录与新建的空库，源码保留在 %s",
			strings.Join(fails, "；"), res.Dir)
	}
	return nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "curl 无输出"
	}
	return s
}

// ---------- 卸载（显式勾选才删库） ----------

// piwigoSiteInstallDB 判断"这个站点是面板一键建站建的"，并返回由域名重新推导的库/账号名。
//
// 只认面板写下的 Remark 标记 + 域名推导：用户手填的库名绝不参与删除决策。
func piwigoSiteInstallDB(site *sites.Site) (dbName, dbUser string, ok bool) {
	if site == nil || !strings.Contains(site.Remark, piwigoInstallRemark) {
		return "", "", false
	}
	dbName, dbUser = piwigoDBIdentifiers(site.Domain)
	return dbName, dbUser, true
}

// dropPiwigoSiteDB 删除本站点的库与账号（显式勾选才调用）。
//
// 三条护栏，缺一不删并如实说明：① 名字必须由域名推导；② 库里的表必须都是 piwigo_ 前缀；
// ③ 只删这个库与这个账号。**永不动其它站点/手工建的库**。
func (s *Server) dropPiwigoSiteDB(ctx context.Context, site *sites.Site, step func(string, ...any)) string {
	dbName, dbUser, ok := piwigoSiteInstallDB(site)
	if !ok {
		return "这个站点不是面板一键建站建的，未删除任何数据库"
	}
	c, err := s.mysqlClient()
	if err != nil {
		return fmt.Sprintf("连不上 MySQL，数据库 %s 未删除：%v", dbName, err)
	}
	dbs, err := c.ListDatabases(ctx)
	if err != nil {
		return fmt.Sprintf("无法确认数据库 %s 是否存在，未删除：%v", dbName, err)
	}
	found := false
	for _, d := range dbs {
		if d.Name == dbName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("数据库 %s 不存在（可能已删除）", dbName)
	}
	tables, err := c.ListTables(ctx, dbName)
	if err != nil {
		return fmt.Sprintf("无法确认数据库 %s 是否只有 Piwigo 的表，为安全未删除：%v", dbName, err)
	}
	for _, t := range tables {
		if !strings.HasPrefix(strings.ToLower(t.Name), piwigoTablePrefix) {
			return fmt.Sprintf("数据库 %s 里有非 Piwigo 表（%s），为安全未删除", dbName, t.Name)
		}
	}
	if err := c.DropDatabase(ctx, dbName); err != nil {
		return fmt.Sprintf("删除数据库 %s 失败：%v", dbName, err)
	}
	step("已删除数据库 %s", dbName)
	if err := c.DropUser(ctx, dbUser, "localhost"); err != nil {
		return fmt.Sprintf("已删除数据库 %s，但账号 %s 未删除：%v", dbName, dbUser, err)
	}
	if err := s.ForgetDBPassword(ctx, dbUser, "localhost"); err != nil {
		s.logWarn("清理账号 %s 的口令记录失败: %v", dbUser, err)
	}
	step("已删除数据库账号 %s", dbUser)
	return fmt.Sprintf("已删除数据库 %s 与账号 %s", dbName, dbUser)
}

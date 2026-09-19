package backup

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zizdog/zizpanel/internal/services"
)

// 备份目标（勾选项）。sites/mysql/nginx/panel 是既有备份任务就有的四项；
// apps 系列是本轮新增的可选含密项（默认不勾）。
const (
	TargetSites = "sites"
	TargetMySQL = "mysql"
	TargetNginx = "nginx"
	TargetPanel = "panel"
	// TargetCompose 是 <WorkDir>/compose 下的 Docker Compose 项目（含 .env 凭据）。
	TargetCompose = "apps:compose"
	// 其余 apps:<id> 由 services.ReleaseBinaryAppConfigRelPaths() 派生。
	appTargetPrefix = "apps:"
)

// Apply 描述一个归档条目在**恢复**时怎么处理。
type Apply string

const (
	// ApplyReplace 直接落盘（配置、证书、ACME 状态、PHP 片段、应用配置）。
	ApplyReplace Apply = "replace"
	// ApplyRegenerate 不落盘，由恢复流程按数据库/环境**重建**。
	// 为什么 nginx 配置走这条：文件接口没有语法校验，整份覆盖 nginx.conf
	// 写坏就是面板/网站全挂；vhost 只有 helper `vhost-write` 带 nginx -t + 回滚。
	// 备份里仍然带上它们，用作回滚凭据与排障。
	ApplyRegenerate Apply = "regenerate"
	// ApplySkip 归档里有，但本次恢复不应用（sites/mysql 内容），如实列进"未恢复项"。
	ApplySkip Apply = "skip"
)

// Item 是备份清单里的一条来源。
type Item struct {
	// ArchivePath 是归档内的逻辑路径（恢复时按**当前**配置映射回真实路径）。
	ArchivePath string
	// SourcePath 是真实路径（文件或目录）。
	SourcePath string
	// Targets 是包含它的目标；任一被勾选就进归档。
	Targets []string
	Apply   Apply
	Secrets bool
}

// GeneratedFile 是需要现场生成的归档条目（mysqldump / sites 打包）。
type GeneratedFile struct {
	ArchivePath string
	Targets     []string
	Secrets     bool
	Write       func(dest string) error
}

// PlanOptions 是生成备份清单所需的**全部**路径来源。
//
// 关键：没有任何默认值写死系统路径。调用方从面板配置取
// （config.Config 的 DataDir/WorkDir/BrewPrefix/UserHome）。
type PlanOptions struct {
	DataDir    string
	WorkDir    string
	BrewPrefix string
	UserHome   string
}

// Plan 返回当前配置下"应当进备份"的完整清单。
//
// 这个函数是备份覆盖面的**唯一真相**：门禁测试会扫描代码里对
// <DataDir>/<WorkDir> 的字面引用，新增数据文件却没进这里（或明确的排除清单）
// 就会变红（见 plan_gate_test.go）。
func Plan(o PlanOptions) []Item {
	var items []Item
	add := func(archive, src string, apply Apply, secrets bool, targets ...string) {
		if src == "" {
			return
		}
		items = append(items, Item{
			ArchivePath: archive, SourcePath: src, Apply: apply, Secrets: secrets, Targets: targets,
		})
	}

	// ---------- 面板自身状态（panel） ----------
	// 注意 config.json 也在这里：默认**不恢复**（保持本机身份），
	// 但必须进归档，否则「要保证可恢复」这条做不到。
	add("data/config.json", filepath.Join(o.DataDir, "config.json"), ApplyReplace, true, TargetPanel)
	add("data/tls", filepath.Join(o.DataDir, "tls"), ApplyReplace, true, TargetPanel)
	// 证书的真相在磁盘 meta.json（certificates 是死表），而 sites/proxies 表里
	// 的 ssl_cert/ssl_key 只是路径指针 —— 只备份库 = 指向空气。
	add("data/certs", filepath.Join(o.DataDir, "certs"), ApplyReplace, true, TargetPanel)
	add("data/site-certs", filepath.Join(o.DataDir, "site-certs"), ApplyReplace, true, TargetPanel)
	add("data/proxy-certs", filepath.Join(o.DataDir, "proxy-certs"), ApplyReplace, true, TargetPanel)
	// 反向代理的访问鉴权凭据（htpasswd 文件，2026-09-25 新增"可加权鉴"时引入）。
	// 必须进备份：vhost 里的 `auth_basic_user_file` 指向它，只备份库/vhost 而丢了
	// 这个文件，恢复后要么鉴权悄悄失效、要么 nginx 因文件缺失而报错。
	add("data/proxy-auth", filepath.Join(o.DataDir, "proxy-auth"), ApplyReplace, true, TargetPanel)
	add("data/acme", filepath.Join(o.DataDir, "acme"), ApplyReplace, true, TargetPanel)
	add("data/default-site.json", filepath.Join(o.DataDir, "default-site.json"), ApplyReplace, false, TargetPanel)

	// ---------- nginx / PHP 环境（nginx） ----------
	if o.BrewPrefix != "" {
		nginxEtc := filepath.Join(o.BrewPrefix, "etc", "nginx")
		add("nginx/nginx.conf", filepath.Join(nginxEtc, "nginx.conf"), ApplyRegenerate, false, TargetNginx)
		add("nginx/nginx.conf.zizpanel.bak", filepath.Join(nginxEtc, "nginx.conf")+".zizpanel.bak",
			ApplyRegenerate, false, TargetNginx)
		add("nginx/conf.d", filepath.Join(nginxEtc, "conf.d"), ApplyRegenerate, false, TargetNginx)
		add("nginx/includes", filepath.Join(nginxEtc, "includes"), ApplyRegenerate, false, TargetNginx)
		add("nginx/vhosts", filepath.Join(nginxEtc, "vhosts"), ApplyRegenerate, false, TargetNginx)
		// phpMyAdmin 配置含 blowfish secret，恢复后必须还是同一份
		// （否则已登录的 phpMyAdmin 会话全部失效，cookie 解密失败）。
		add("phpmyadmin/config.inc.php", services.PhpMyAdminConfPath(o.BrewPrefix),
			ApplyReplace, true, TargetNginx)
		// 各 PHP 版本的 upload/exec 上限片段。
		for _, f := range phpLimitFiles(o.BrewPrefix) {
			ver := filepath.Base(filepath.Dir(filepath.Dir(f)))
			add(filepath.ToSlash(filepath.Join("php", ver, "99-zizpanel-limits.ini")), f,
				ApplyReplace, false, TargetNginx)
		}
	}

	// ---------- 可选：第三方客户端的含密配置（默认不勾） ----------
	for id, rel := range services.ReleaseBinaryAppConfigRelPaths() {
		if o.UserHome == "" {
			break
		}
		add(filepath.ToSlash(filepath.Join("apps", id, filepath.Base(rel))),
			filepath.Join(o.UserHome, filepath.FromSlash(rel)),
			ApplyReplace, true, appTargetPrefix+id)
	}
	if o.WorkDir != "" {
		add("apps/compose", filepath.Join(o.WorkDir, "compose"), ApplyReplace, true, TargetCompose)
	}
	return items
}

// phpLimitFiles 找出所有 PHP 版本的 99-zizpanel-limits.ini。
func phpLimitFiles(brewPrefix string) []string {
	matches, err := filepath.Glob(filepath.Join(brewPrefix, "etc", "php", "*", "conf.d", "99-zizpanel-limits.ini"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

// AppTargets 返回全部可勾选的 apps:* 目标（供界面与校验使用）。
func AppTargets() []string {
	paths := services.ReleaseBinaryAppConfigRelPaths()
	out := []string{TargetCompose}
	for id := range paths {
		out = append(out, appTargetPrefix+id)
	}
	sort.Strings(out)
	return out
}

// AllTargets 返回全部合法的备份目标（apps:* 从注册表派生，不另抄一份）。
func AllTargets() []string {
	return append([]string{TargetSites, TargetMySQL, TargetNginx, TargetPanel}, AppTargets()...)
}

// IsKnownTarget 判断目标名是否合法（含由注册表派生的 apps:<id>）。
func IsKnownTarget(t string) bool {
	switch t {
	case TargetSites, TargetMySQL, TargetNginx, TargetPanel:
		return true
	}
	if !strings.HasPrefix(t, appTargetPrefix) {
		return false
	}
	id := strings.TrimPrefix(t, appTargetPrefix)
	if id == "compose" {
		return true
	}
	_, ok := services.ReleaseBinaryAppConfigRelPaths()[id]
	return ok
}

// NormalizeTargets 去重并保持稳定顺序（sites→mysql→nginx→panel→apps）。
func NormalizeTargets(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range []string{TargetSites, TargetMySQL, TargetNginx, TargetPanel} {
		for _, x := range in {
			if x == t && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	var apps []string
	for _, t := range in {
		if strings.HasPrefix(t, appTargetPrefix) && !seen[t] {
			seen[t] = true
			apps = append(apps, t)
		}
	}
	sort.Strings(apps)
	return append(out, apps...)
}

// Selected 判断某个 item 是否被 targets 命中。
func Selected(itemTargets, targets []string) bool {
	for _, it := range itemTargets {
		for _, t := range targets {
			if it == t {
				return true
			}
		}
	}
	return false
}

// ---------- 覆盖面门禁用的名单 ----------
//
// 这些名单是"代码里新增数据文件"与"备份清单"之间的桥：
// plan_gate_test.go 扫描源码里的 <X>Dir, "<name>" 字面量，
// 每个名字必须出现在 Cover* 或 Exclude* 里，否则测试变红。

// DataCoveredNames 是 <DataDir> 下进了备份（或由数据库快照等价覆盖）的一级名字。
func DataCoveredNames() []string {
	return []string{"config.json", "tls", "certs", "site-certs", "proxy-certs", "proxy-auth", "acme",
		"default-site.json", "panel.db"}
}

// DataExcludedNames 是 <DataDir> 下**明确不进**备份的一级名字及原因。
func DataExcludedNames() map[string]string {
	return map[string]string{
		"panel.db-wal": "数据库快照由 VACUUM INTO 生成，绝不用 WAL 文件凑",
		"panel.db-shm": "同上",
		"logs":         "日志可重建、体积大",
	}
}

// WorkCoveredNames 是 <WorkDir> 下进了备份的一级名字。
func WorkCoveredNames() []string { return []string{"compose"} }

// WorkExcludedNames 是 <WorkDir> 下明确不进备份的一级名字及原因。
func WorkExcludedNames() map[string]string {
	return map[string]string{
		"backup":    "备份输出目录自身（否则越备越大）",
		"db-backup": "数据库导出暂存目录，可重建",
		"services":  "服务代码可重新下载",
		"upgrade":   "升级暂存包/看门狗，一次性产物",
	}
}

// Exists 判断来源路径是否存在（不存在不算错误：可选项目本就可能没有）。
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

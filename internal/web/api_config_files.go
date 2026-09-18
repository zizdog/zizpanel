package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  「网站管理 → ⚙️ 配置文件」文件清单
//
//  用户 2026-09-20 的要求（与 413 同批）：
//  「但是面板也要有手动改 php 和 nginx 的入口啊！这是基本操作。类似宝塔。」
//
//  面板的原则是"能在面板里做完"，但底层配置文件（nginx.conf / php.ini /
//  my.cnf）必须留一个**可见、可编辑、可回滚**的入口 —— 宝塔就是这么做的。
//
//  这份清单由后端给路径（而不是前端拼字符串）：
//   - brew 前缀在 Apple Silicon / Intel / 自定义安装下不同，写死 /opt/homebrew
//     会让 Intel 机器点开就报"文件不存在"；
//   - 站点 vhost 的目录、PHP 各版本的 php.ini/www.conf、面板自己的限制片段
//     路径规则都收敛在 Go 侧（sites 包），前端不该有第二份。
//
//  只列路径与是否存在，**不读内容**：编辑走既有的文件接口
//  （GET/POST /api/v1/files/read|write，含白名单与越界校验），
//  UI 复用 services.js 的 configFileModal —— 不新造第二套编辑器。
// ============================================================================

// ConfigFileItem 是一个可在面板里编辑的配置文件。
type ConfigFileItem struct {
	// Group 是分组标题（nginx / PHP 8.2 / MySQL），前端按它分组渲染。
	Group string `json:"group"`
	// Label 是这一项的用途说明。
	Label string `json:"label"`
	Path  string `json:"path"`
	// Exists 是真实探测结果（false 时前端要说明"文件还不存在"，而不是让编辑器报错）。
	Exists bool `json:"exists"`
	// Service 是"改完要重启哪个服务"的面板服务记录名；空 = 面板里没有可靠的服务名，
	// 前端只提示手工重载/重启，不假装能一键重启。
	Service string `json:"service,omitempty"`
	// Hint 是这一项的生效方式说明（nginx 要 reload、PHP 要重启 php-fpm）。
	Hint string `json:"hint,omitempty"`
}

// panelConfigFiles 返回"网站环境"这一层所有值得手工编辑的配置文件清单。
//
// 清单是**现实驱动**的：PHP 每个已安装版本、每个已登记站点的 vhost 都逐个列出；
// 文件不存在也照样列（带 exists=false）—— 用户需要知道"该有但还没有"，
// 而不是从列表里凭空消失。
func (s *Server) panelConfigFiles(ctx context.Context) []ConfigFileItem {
	const nginxHint = "改完必须让 nginx 重新加载才生效：可点本弹窗里的「🔄 重载 nginx」，" +
		"或到「⋯ 更多 → 校验 nginx」确认语法，再保存任意站点让面板自动重载。"
	out := []ConfigFileItem{}
	add := func(group, label, path, service, hint string) {
		if path == "" {
			return
		}
		_, err := os.Stat(path)
		out = append(out, ConfigFileItem{
			Group: group, Label: label, Path: path, Exists: err == nil,
			Service: service, Hint: hint,
		})
	}

	// ---- nginx ----
	add("nginx", "nginx 主配置", s.Cfg.NginxConf, "nginx", nginxHint)
	// 每个站点的 vhost（含默认站点）。
	siteList, err := s.siteMgr().List(ctx)
	if err == nil {
		for _, st := range siteList {
			if st == nil {
				continue
			}
			add("nginx", "站点配置："+st.Domain,
				filepath.Join(s.Cfg.VhostDir, st.Domain+".conf"), "nginx", nginxHint)
		}
	}
	add("nginx", "默认站点配置（000-default，含 phpMyAdmin 入口）",
		filepath.Join(s.Cfg.VhostDir, "000-default.conf"), "nginx", nginxHint)

	// vhost 目录里**实际存在但没在上面列出**的 .conf 也列出来。
	//
	// 为什么必须扫目录：面板数据库与磁盘会不一致（站点被手工删过、或 vhost 是
	// 上一版面板/用户自己写的）。只列"数据库里的站点"会让用户在这些文件出问题时
	// 找不到入口 —— 而"手动改 nginx"恰恰是为这种时候准备的。
	if entries, derr := os.ReadDir(s.Cfg.VhostDir); derr == nil {
		listed := map[string]bool{}
		for _, it := range out {
			listed[it.Path] = true
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			p := filepath.Join(s.Cfg.VhostDir, e.Name())
			if listed[p] {
				continue
			}
			add("nginx", "站点配置："+e.Name()+"（面板里没有对应站点记录）", p, "nginx", nginxHint)
		}
	}

	// ---- PHP（逐个已安装版本）----
	for _, v := range sites.DiscoverPHPVersions(s.Cfg.BrewPrefix) {
		group := "PHP " + v.Version
		hint := "改完必须重启 PHP " + v.Version + " 的 php-fpm 才生效" +
			"（本弹窗里的「🔄 重启服务」，或到「网站管理 → 🐘 PHP 环境」）。"
		add(group, "php.ini", filepath.Join(s.Cfg.BrewPrefix, "etc", "php", v.Version, "php.ini"), v.Service, hint)
		add(group, "php-fpm.conf", filepath.Join(s.Cfg.BrewPrefix, "etc", "php", v.Version, "php-fpm.conf"), v.Service, hint)
		add(group, "php-fpm 池配置 (www.conf)", v.FPMConf, v.Service, hint)
		add(group, "面板写的上传/执行限制片段（删掉它并重启即恢复出厂值）",
			sites.PHPConfDPath(s.Cfg.BrewPrefix, v.Version), v.Service,
			"这个文件由「面板设置 → 上传与执行限制」生成，改它不如去那里改（下次应用会覆盖）。")
	}

	// ---- MySQL ----
	add("MySQL", "MySQL 主配置", filepath.Join(s.Cfg.BrewPrefix, "etc", "my.cnf"), s.Cfg.MySQLSvc,
		"改完必须重启 MySQL 才生效（到「服务」页重启）。")
	return out
}

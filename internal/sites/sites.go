// Package sites 实现网站（站点）管理的核心逻辑：数据模型、nginx 配置生成、
// 伪静态模板、SSL 与反向代理。
//
// 设计要点：
//
//  1. nginx 配置由本包生成，helper 只负责"白名单目录内原子落盘 + 语法校验 + 自动回滚"。
//     这样 nginx 语法相关的逻辑全部留在可直接单元测试的 Go 代码里，
//     而 root 侧的能力依然是一个薄执行器 —— 提权面最小。
//
//  2. 站点信息以 SQLite 为权威来源，nginx conf 是它的派生物。
//     任何时候都可以用数据库状态重新生成全部配置（用于修复被手工改坏的配置）。
//
//  3. 生成配置后必须做语法校验，不通过就回滚。nginx 配置写坏等于全站 502，
//     这是面板最不能犯的错误之一。
package sites

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/store"
)

// Site 是一个站点。字段与 sites 表一一对应。
type Site struct {
	ID          int64  `json:"id"`
	Domain      string `json:"domain"`
	Aliases     string `json:"aliases"` // 逗号分隔
	Root        string `json:"root"`
	PHPVersion  string `json:"php_version"` // 空=纯静态
	Rewrite     string `json:"rewrite"`
	SSLEnabled  bool   `json:"ssl_enabled"`
	SSLCert     string `json:"ssl_cert"`
	SSLKey      string `json:"ssl_key"`
	SSLProvider string `json:"ssl_provider"` // self / mkcert / le / manual
	SSLExpires  string `json:"ssl_expires"`
	ProxyPass   string `json:"proxy_pass"`
	ExtraConf   string `json:"extra_conf"`
	Enabled     bool   `json:"enabled"`
	Remark      string `json:"remark"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`

	// 运行时状态（不入库，由 Detect 填充）
	Running  bool   `json:"running"`
	HTTPCode string `json:"http_code"`
	PHPRaw   bool   `json:"php_raw"` // PHP 被当静态文件吐出（配置错误）
	Logs     string `json:"-"`
}

// AliasList 返回去空格后的附加域名列表。
func (s *Site) AliasList() []string {
	var out []string
	for _, a := range strings.Split(s.Aliases, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// ServerNames 返回 server_name 指令的完整值。
func (s *Site) ServerNames() string {
	names := append([]string{s.Domain}, s.AliasList()...)
	return strings.Join(names, " ")
}

// ---------- 校验 ----------

var (
	reDomain = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$`)
	// 站点目录名：只允许域名安全字符，禁止路径穿越
	reDirSafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// 伪静态模板名
	reRewriteName = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// ErrInvalid 表示用户输入不合法。
var ErrInvalid = errors.New("参数不合法")

// ValidateDomain 校验域名。
func ValidateDomain(d string) error {
	d = strings.TrimSpace(d)
	if d == "" {
		return fmt.Errorf("%w: 域名不能为空", ErrInvalid)
	}
	if len(d) > 253 {
		return fmt.Errorf("%w: 域名过长", ErrInvalid)
	}
	if !reDomain.MatchString(d) {
		return fmt.Errorf("%w: 域名格式不合法（示例：demo.test）", ErrInvalid)
	}
	return nil
}

// ValidateProxyPass 校验反向代理目标地址。
//
// 只接受 http/https 的 host[:port] 形式，避免把任意字符串塞进 nginx 配置
// （例如 `; }` 之类能闭合指令块的注入）。
func ValidateProxyPass(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
		return fmt.Errorf("%w: 反代地址必须以 http:// 或 https:// 开头", ErrInvalid)
	}
	body := strings.TrimPrefix(strings.TrimPrefix(p, "http://"), "https://")
	if body == "" {
		return fmt.Errorf("%w: 反代地址缺少主机名", ErrInvalid)
	}
	// 只允许主机名/IP + 可选端口 + 可选路径
	if !regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]+(/[A-Za-z0-9._~%\-/]*)?$`).MatchString(body) {
		return fmt.Errorf("%w: 反代地址含非法字符", ErrInvalid)
	}
	return nil
}

// ---------- 伪静态模板 ----------

// RewritePreset 是一个伪静态规则模板。
type RewritePreset struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// PublicDir 非空表示站点根目录下需要这个子目录作为入口（如 Laravel 的 public）
	PublicDir string `json:"public_dir"`
}

// RewritePresets 是内置伪静态模板列表。
//
// 这些都是从实际项目验证过的写法，而不是网上抄来的：
// 关键是 try_files 的最后一个参数必须把请求交给入口文件，
// 而 PHP 的 location 块必须能匹配到最终文件。
var RewritePresets = []RewritePreset{
	{Name: "none", Label: "无（纯静态）", Description: "不做任何重写，仅按文件路径查找。适合 HTML 静态站。"},
	{Name: "generic", Label: "通用（推荐）", Description: "文件存在则直接返回，否则交给 index.php。适配绝大多数 PHP 程序。"},
	{Name: "typecho", Label: "Typecho", Description: "官方推荐的 try_files 写法，兼容性最好。"},
	{Name: "wordpress", Label: "WordPress", Description: "WordPress 标准伪静态，固定链接可自定义。"},
	{Name: "laravel", Label: "Laravel / Lumen", Description: "入口在 public/ 子目录，需要把运行目录指到 public。", PublicDir: "public"},
	{Name: "thinkphp", Label: "ThinkPHP", Description: "入口在 public/ 子目录，兼容 pathinfo 路由。", PublicDir: "public"},
	{Name: "discuz", Label: "Discuz!", Description: "兼容 Discuz 的 rewrite 规则。"},
	{Name: "empirecms", Label: "帝国 CMS", Description: "兼容帝国 CMS 的 rewrite 规则。"},
}

// RewritePresetByName 查找模板。
func RewritePresetByName(name string) (RewritePreset, bool) {
	if name == "" {
		name = "none"
	}
	for _, p := range RewritePresets {
		if p.Name == name {
			return p, true
		}
	}
	return RewritePreset{}, false
}

// rewriteLocation 返回 `location / { ... }` 的内容。
//
// 注意：这里刻意不使用 `if (!-e $request_filename)` 那类写法。
// nginx 的 if 在 location 上下文里有"if is evil"的著名陷阱
// （if 块内的 try_files/rewrite 行为不符合直觉），
// 用 try_files 表达同样的意图既正确又高效。
func rewriteLocation(name string) string {
	switch name {
	case "none":
		return "\tlocation / {\n\t\ttry_files $uri $uri/ =404;\n\t}"
	case "typecho":
		return "\tlocation / {\n\t\ttry_files $uri $uri/ /index.php$is_args$args;\n\t}"
	case "wordpress":
		return "\tlocation / {\n\t\ttry_files $uri $uri/ /index.php?$args;\n\t}"
	case "laravel":
		return "\tlocation / {\n\t\ttry_files $uri $uri/ /index.php?$query_string;\n\t}"
	case "thinkphp":
		// ThinkPHP 的路由兼容：不存在的文件交给 index.php，并保留 pathinfo
		return "\tlocation / {\n" +
			"\t\tif (!-e $request_filename) {\n" +
			"\t\t\trewrite ^(.*)$ /index.php?s=$1 last;\n" +
			"\t\t}\n" +
			"\t}"
	case "discuz":
		return "\tlocation / {\n" +
			"\t\trewrite ^([^\\.]*)/topic-(.+)\\.html$ $1/portal.php?mod=topic&topic=$2 last;\n" +
			"\t\trewrite ^([^\\.]*)/article-([0-9]+)-([0-9]+)\\.html$ $1/portal.php?mod=view&aid=$2&page=$3 last;\n" +
			"\t\trewrite ^([^\\.]*)/forum-(\\w+)-([0-9]+)\\.html$ $1/forum.php?mod=forumdisplay&fid=$2&page=$3 last;\n" +
			"\t\trewrite ^([^\\.]*)/thread-([0-9]+)-([0-9]+)-([0-9]+)\\.html$ $1/forum.php?mod=viewthread&tid=$2&extra=page%3D$4&page=$3 last;\n" +
			"\t\trewrite ^([^\\.]*)/space-(username|uid)-(.+)\\.html$ $1/home.php?mod=space&$2=$3 last;\n" +
			"\t\tif (!-e $request_filename) {\n" +
			"\t\t\trewrite ^([^\\.]*)/(fid|tid)-([0-9]+)\\.html$ $1/index.php?action=$2&value=$3 last;\n" +
			"\t\t}\n" +
			"\t}"
	case "empirecms":
		return "\tlocation / {\n" +
			"\t\tif (!-e $request_filename) {\n" +
			"\t\t\trewrite ^/listinfo-(.+?)-(.+?)-(.+?)-(.+?)-(.+?)-(.+?)-(.+?)-(.+?)-(.+?)-(.+?)\\.html$ /e/action/ListInfo/index.php?classid=$1&id=$2&page=$3&line=$4&tempid=$5&bsid=$6&showmod=$7&class=$8&id=$9&$10 last;\n" +
			"\t\t\trewrite ^/showinfo-(.+?)-(.+?)-(.+?)-(.+?)\\.html$ /e/action/ShowInfo.php?classid=$1&id=$2&page=$3&tempid=$4 last;\n" +
			"\t\t}\n" +
			"\t}"
	default:
		return "\tlocation / {\n\t\ttry_files $uri $uri/ /index.php$is_args$args;\n\t}"
	}
}

// ---------- nginx 配置生成 ----------

// Options 控制配置生成的行为。
type Options struct {
	LogDir string // access/error 日志目录
	// FastCGIPass 是 PHP 版本的 fastcgi 目标，如 unix:/opt/homebrew/var/run/php-fpm-8.3.sock
	// 或 127.0.0.1:9000。由调用方按站点所选的 PHP 版本解析后传入
	// （见 ResolveEndpoint：每个版本一个唯一端点，解析不到会报错）。
	FastCGIPass string
}

// Generate 生成一个站点的 nginx server 配置。
//
// 生成的内容必须满足：nginx -t 能通过、能够实际提供服务（有测试验证）。
func (s *Site) Generate(opt Options) (string, error) {
	if err := ValidateDomain(s.Domain); err != nil {
		return "", err
	}
	for _, a := range s.AliasList() {
		if err := ValidateDomain(a); err != nil {
			return "", fmt.Errorf("附加域名 %s: %w", a, err)
		}
	}
	if err := ValidateProxyPass(s.ProxyPass); err != nil {
		return "", err
	}
	if !filepath.IsAbs(s.Root) {
		return "", fmt.Errorf("%w: 站点根目录必须是绝对路径", ErrInvalid)
	}
	preset, ok := RewritePresetByName(s.Rewrite)
	if !ok {
		return "", fmt.Errorf("%w: 未知的伪静态模板 %q", ErrInvalid, s.Rewrite)
	}
	logDir := opt.LogDir
	if logDir == "" {
		logDir = "/tmp"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# 站点: %s\n", s.Domain)
	fmt.Fprintf(&b, "# 由 ZizPanel 生成于 %s —— 手工修改会在面板下次保存时被覆盖\n", time.Now().Format("2006-01-02 15:04:05"))
	if s.Remark != "" {
		fmt.Fprintf(&b, "# 备注: %s\n", strings.ReplaceAll(s.Remark, "\n", " "))
	}

	// ---------------- HTTP server ----------------
	b.WriteString("server {\n")
	b.WriteString("\tlisten      80;\n")
	if s.SSLEnabled {
		// 开了 SSL 又把 HTTP 直接放行会产生重复内容，不利于 SEO；
		// 这里统一 301 到 HTTPS。证书校验路径除外，否则 Let's Encrypt 无法续期。
		b.WriteString("\tserver_name " + s.ServerNames() + ";\n")
		b.WriteString("\n\t# 证书续期与健康检查放行\n")
		b.WriteString("\tlocation ^~ /.well-known/acme-challenge/ {\n")
		b.WriteString("\t\troot " + s.Root + ";\n")
		b.WriteString("\t}\n")
		b.WriteString("\n\tlocation / {\n\t\treturn 301 https://$host$request_uri;\n\t}\n")
		b.WriteString("}\n\n")
		b.WriteString("server {\n")
		b.WriteString("\tlisten      443 ssl;\n")
		b.WriteString("\thttp2       on;\n")
	}

	b.WriteString("\tserver_name " + s.ServerNames() + ";\n")
	fmt.Fprintf(&b, "\troot        %s;\n", s.Root)
	b.WriteString("\tindex       index.php index.html index.htm;\n\n")
	fmt.Fprintf(&b, "\taccess_log  %s/%s.access.log;\n", logDir, s.Domain)
	fmt.Fprintf(&b, "\terror_log   %s/%s.error.log warn;\n\n", logDir, s.Domain)
	b.WriteString("\tcharset utf-8;\n")
	b.WriteString("\tclient_max_body_size 64m;\n")

	if s.SSLEnabled {
		if s.SSLCert == "" || s.SSLKey == "" {
			return "", fmt.Errorf("%w: 已开启 SSL 但缺少证书路径", ErrInvalid)
		}
		b.WriteString("\n\tssl_certificate     " + s.SSLCert + ";\n")
		b.WriteString("\tssl_certificate_key " + s.SSLKey + ";\n")
		b.WriteString("\tssl_protocols       TLSv1.2 TLSv1.3;\n")
		b.WriteString("\tssl_ciphers         ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305;\n")
		b.WriteString("\tssl_prefer_server_ciphers off;\n")
		b.WriteString("\tssl_session_cache   shared:SSL:10m;\n")
		b.WriteString("\tssl_session_timeout 1d;\n")
		b.WriteString("\tssl_stapling        off;\n")
	}

	// 安全响应头。刻意不设 X-Frame-Options: DENY —— 很多后台需要 iframe，
	// 用 SAMEORIGIN 更实用。
	b.WriteString("\n\t# ---- 安全响应头 ----\n")
	b.WriteString("\tadd_header X-Content-Type-Options nosniff always;\n")
	b.WriteString("\tadd_header X-Frame-Options SAMEORIGIN always;\n")
	b.WriteString("\tadd_header Referrer-Policy strict-origin-when-cross-origin always;\n")

	b.WriteString("\n\t# ---- 安全: 隐藏敏感文件 ----\n")
	b.WriteString("\tlocation ~ /\\. { deny all; access_log off; log_not_found off; }\n")
	b.WriteString("\tlocation ~* \\.(ini|log|conf|sql|bak|swp)$ { deny all; }\n")

	b.WriteString("\n\t# ---- 静态资源缓存 ----\n")
	b.WriteString("\tlocation ~* \\.(?:css|js|jpg|jpeg|gif|png|ico|svg|webp|avif|woff2?|ttf|eot|mp4|webm)$ {\n")
	b.WriteString("\t\texpires 7d;\n\t\taccess_log off;\n\t}\n")

	// ---------------- 反向代理 或 常规处理 ----------------
	if s.ProxyPass != "" {
		b.WriteString("\n\t# ---- 反向代理到 " + s.ProxyPass + " ----\n")
		b.WriteString("\tlocation / {\n")
		b.WriteString("\t\tproxy_pass " + s.ProxyPass + ";\n")
		b.WriteString("\t\tproxy_http_version 1.1;\n")
		// WebSocket 升级头：没有这几行，反代后的应用无法使用 WS
		b.WriteString("\t\tproxy_set_header Upgrade $http_upgrade;\n")
		b.WriteString("\t\tproxy_set_header Connection $connection_upgrade;\n")
		b.WriteString("\t\tproxy_set_header Host $host;\n")
		b.WriteString("\t\tproxy_set_header X-Real-IP $remote_addr;\n")
		b.WriteString("\t\tproxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
		b.WriteString("\t\tproxy_set_header X-Forwarded-Proto $scheme;\n")
		b.WriteString("\t\tproxy_cache_bypass $http_upgrade;\n")
		b.WriteString("\t\tproxy_read_timeout 300s;\n")
		b.WriteString("\t\tproxy_send_timeout 300s;\n")
		b.WriteString("\t\tclient_max_body_size 512m;\n")
		b.WriteString("\t}\n")
	} else {
		b.WriteString("\n\t# ---- 伪静态规则（模板：" + preset.Name + "）----\n")
		b.WriteString(rewriteLocation(preset.Name))
		b.WriteString("\n")

		if s.PHPVersion != "" {
			if opt.FastCGIPass == "" {
				return "", fmt.Errorf("%w: 指定了 PHP 版本 %s 但未解析出 FastCGI 地址", ErrInvalid, s.PHPVersion)
			}
			b.WriteString("\n\t# ---- PHP " + s.PHPVersion + " 转发 ----\n")
			b.WriteString("\tlocation ~ \\.php$ {\n")
			b.WriteString("\t\ttry_files $uri =404;\n")
			b.WriteString("\t\tfastcgi_pass " + opt.FastCGIPass + ";\n")
			b.WriteString("\t\tfastcgi_index index.php;\n")
			b.WriteString("\t\tfastcgi_split_path_info ^(.+?\\.php)(/.*)$;\n")
			b.WriteString("\t\tfastcgi_param SCRIPT_FILENAME $request_filename;\n")
			b.WriteString("\t\tfastcgi_param PATH_INFO $fastcgi_path_info;\n")
			// 直接内联 fastcgi 参数而不是 include 全局文件：
			// include 的方式会让所有站点共用同一份配置，无法按站点指定 PHP 版本。
			b.WriteString(fastcgiParams())
			b.WriteString("\t\tfastcgi_read_timeout 300s;\n")
			b.WriteString("\t\tfastcgi_buffering off;\n")
			b.WriteString("\t}\n")
		} else {
			// 纯静态站点：显式拒绝执行 PHP，避免上传的 .php 被意外执行
			b.WriteString("\n\t# ---- 纯静态站点：拒绝执行 PHP ----\n")
			b.WriteString("\tlocation ~ \\.php$ { return 403; }\n")
		}
	}

	if s.ExtraConf != "" {
		b.WriteString("\n\t# ---- 自定义配置 ----\n")
		b.WriteString(strings.TrimRight(s.ExtraConf, "\n"))
		b.WriteString("\n")
	}

	b.WriteString("}\n")
	return b.String(), nil
}

// UpgradeMapName 是 WebSocket 升级所需的 map 变量片段文件名。
//
// 为什么需要它：反向代理要支持 WebSocket，必须根据 $http_upgrade 决定
// Connection 头的值。nginx 的 KeepAlive 指令不接受变量，唯一正确的做法是
// 用 map 定义一个变量（社区标准写法 $connection_upgrade）。
//
// 但 map 只能定义在 http 上下文，不能写在 server 块里。
// 因此面板会把这个片段写到 <brew>/etc/nginx/conf.d/upgrade-map.conf，
// 并由安装脚本确保 nginx.conf 的 http 块 include 了 conf.d/*.conf。
//
// 踩过的坑：早期版本直接在 vhost 里用 $connection_upgrade 却没定义 map，
// nginx -t 会报 "unknown variable"，反代站点全部无法启用。
const UpgradeMapName = "upgrade-map.conf"

// UpgradeMapConf 返回 WebSocket 升级所需的 map 定义（写到 conf.d）。
func UpgradeMapConf() string {
	return `# 由 ZizPanel 生成：反向代理的 WebSocket 升级支持
# nginx 的 proxy_set_header 里不能用 if 判断，只能用 map 预定义变量。
# 该文件位于 http 上下文的 include 路径下，因此可以定义 map。
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}
`
}

// FastCGIParams 对外暴露 FastCGI 参数片段（供测试与文档使用）。
func FastCGIParams() string { return fastcgiParams() }

// fastcgiParamPairs 是 FastCGI 参数的唯一来源（name + value 列表）。
//
// **只有这一份**：站点的 vhost（Generate）、默认站点与 phpMyAdmin 入口
// 都从这里派生，避免"一处改了另一处没改"（真机上就出现过 9000 与专属
// socket 两个版本并存的问题）。
func fastcgiParamPairs() []string {
	return []string{
		"QUERY_STRING $query_string",
		"REQUEST_METHOD $request_method",
		"CONTENT_TYPE $content_type",
		"CONTENT_LENGTH $content_length",
		"SCRIPT_NAME $fastcgi_script_name",
		"REQUEST_URI $request_uri",
		"DOCUMENT_URI $document_uri",
		"DOCUMENT_ROOT $document_root",
		"SERVER_PROTOCOL $server_protocol",
		"REQUEST_SCHEME $scheme",
		"SERVER_SOFTWARE nginx/$nginx_version",
		"REMOTE_ADDR $remote_addr",
		"REMOTE_PORT $remote_port",
		"SERVER_ADDR $server_addr",
		"SERVER_PORT $server_port",
		"SERVER_NAME $server_name",
		"REDIRECT_STATUS 200",
		"HTTP_AUTHORIZATION $http_authorization",
		// HTTPS 变量在非 SSL 连接下为空，用 if_not_empty 避免警告。
		// 放在清单末尾而不是循环外特判：这样它就是**同一份参数清单**的一部分，
		// 默认站点/phpMyAdmin 用的 FastCGIParamsBlock 也会带上它。
		"HTTPS $https if_not_empty",
	}
}

// fastcgiParams 返回 PHP 转发所需的 FastCGI 参数（**已带站点 vhost 的缩进**）。
// 与 /opt/homebrew/etc/nginx/conf.d/php-fpm.conf 的内容保持一致，
// 但内联进 vhost 以便每个站点独立选择 PHP 版本。
func fastcgiParams() string {
	var b strings.Builder
	for _, p := range fastcgiParamPairs() {
		b.WriteString("\t\tfastcgi_param " + p + ";\n")
	}
	return b.String()
}

// ---------- PHP 版本与 FastCGI 地址 ----------

// PHPVersion 描述一个已安装的 PHP 版本。
type PHPVersion struct {
	Version   string `json:"version"`  // 8.3
	Service   string `json:"service"`  // php@8.3
	Binary    string `json:"binary"`   // /opt/homebrew/opt/php@8.3/bin/php
	FPMConf   string `json:"fpm_conf"` // php-fpm 配置文件
	Pass      string `json:"pass"`     // fastcgi 目标，如 unix:/opt/homebrew/var/run/php-fpm-8.3.sock
	Running   bool   `json:"running"`
	IsDefault bool   `json:"is_default"`
	// PreferredPass 是面板为该版本**分配**的端点（Unix socket）。
	// 与 Pass 不一致，就说明 www.conf 还没被面板改过（典型是出厂的 9000）。
	PreferredPass string `json:"preferred_pass"`
	// ListenOK 表示 www.conf 里的 listen 已经等于面板分配的端点。
	// false 且有 ListenErr 时，界面应引导用户点"修复监听端点"。
	ListenOK bool `json:"listen_ok"`
	// ListenErr 是解析 www.conf 失败的原因（版本、文件、原因都在里面）。
	// 非空时 Pass 为空 —— 宁可为空，也不给出一个猜出来的地址。
	ListenErr string `json:"listen_err,omitempty"`
	// Conflict 非空表示该版本与另一个版本共用同一个端点
	// （多版本共存的直接故障：两个版本抢一个地址）。
	Conflict string `json:"conflict,omitempty"`
	// ActualVersion 是通过实际请求探测到的版本。
	// 与 Version 不一致时说明"配置与实际运行的不是同一个版本"。
	ActualVersion string `json:"actual_version,omitempty"`
}

// DiscoverPHPVersions 从 Homebrew 的实际安装情况推导已安装的 PHP 版本。
//
// 为什么不能写死列表：用户可能只装了 php@8.3，也可能装了 8.2/8.3/8.4，
// 甚至全新版本（8.5）。写死列表的后果是"装了却选不到"，
// 所以以 <brew>/opt/php* 里真实存在的东西为准。
//
// 返回的条目**按版本号升序**，保证界面顺序稳定。
// 注意：这里不做"端点是否在监听"的判断（那是调用方的运行时探测）；
// 它只回答"这台机器上装了哪些版本、各自配在哪个端点上"。
func DiscoverPHPVersions(brewPrefix string) []PHPVersion {
	optDir := filepath.Join(brewPrefix, "opt")
	entries, err := os.ReadDir(optDir)
	if err != nil {
		return []PHPVersion{}
	}
	seen := map[string]bool{}
	var out []PHPVersion
	for _, e := range entries {
		name := e.Name()
		if name != "php" && !strings.HasPrefix(name, "php@") {
			continue
		}
		version := ""
		if v, ok := strings.CutPrefix(name, "php@"); ok {
			version = v
		} else {
			// `php` 是版本别名（当前指 8.4）：解析软链接拿到真实版本号，
			// 否则会出现一个叫 "php" 的条目，用户根本不知道那是哪个版本。
			if link, err := os.Readlink(filepath.Join(optDir, name)); err == nil {
				base := filepath.Base(link)
				if v, ok := strings.CutPrefix(base, "php@"); ok {
					version = v
				} else if base != "php" {
					version = base
				}
			}
		}
		if !rePHPVersion.MatchString(version) {
			continue // 拿不到版本号就不列出来（列出来也没法配）
		}
		// php 与 php@8.4 会解析到同一个版本号，按版本号去重（保留 php@x.y 那一条）
		if seen[version] {
			continue
		}
		seen[version] = true

		pv := PHPVersion{
			Version:   version,
			Service:   name,
			Binary:    filepath.Join(optDir, name, "bin", "php"),
			FPMConf:   FPMConfPath(brewPrefix, version),
			IsDefault: name == "php",
		}
		if pref, err := PreferredEndpoint(brewPrefix, version); err == nil {
			pv.PreferredPass = pref
		}
		if ep, err := ResolveConfigEndpoint(brewPrefix, version); err != nil {
			pv.ListenErr = err.Error()
		} else {
			pv.Pass = ep
			pv.ListenOK = EndpointIsPreferred(brewPrefix, version, ep)
		}
		out = append(out, pv)
	}
	sortPHPVersions(out)

	// 标记"两个版本共用同一个端点"——这是多版本共存里最该被看见的故障。
	//
	// 注意要**两边都标**：只标后一个会让用户以为前一个是正常的。
	byEndpoint := map[string][]int{}
	for i := range out {
		if out[i].Pass == "" {
			continue
		}
		byEndpoint[out[i].Pass] = append(byEndpoint[out[i].Pass], i)
	}
	for ep, idxs := range byEndpoint {
		if len(idxs) < 2 {
			continue
		}
		names := make([]string, 0, len(idxs))
		for _, i := range idxs {
			names = append(names, out[i].Version)
		}
		for _, i := range idxs {
			out[i].Conflict = fmt.Sprintf("PHP %s 共用端点 %s（多个版本抢一个地址，只有一个能起来）",
				strings.Join(names, "、"), ep)
		}
	}
	return out
}

// sortPHPVersions 按 major/minor 数值升序排列（8.2 < 8.3 < 8.4 < 8.10）。
//
// 为什么不直接字符串排序：字符串排序会把 8.10 排到 8.2 前面。
func sortPHPVersions(list []PHPVersion) {
	less := func(a, b string) bool {
		am, aerr := phpMinor(a)
		bm, berr := phpMinor(b)
		if aerr != nil || berr != nil {
			return a < b
		}
		amaj, _ := strconv.Atoi(strings.Split(a, ".")[0])
		bmaj, _ := strconv.Atoi(strings.Split(b, ".")[0])
		if amaj != bmaj {
			return amaj < bmaj
		}
		return am < bm
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && less(list[j].Version, list[j-1].Version); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// PHP 端点的解析与分配全部在 phpfpm.go 里（ResolveEndpoint / EnsureListen）。
//
// 历史提醒：这里原来有一个 ResolveFastCGI，读不到 www.conf 时会**静默回退
// 127.0.0.1:9000**。那正是"多版本 PHP 互相抢端口"的根源 —— 每个版本被解析成
// 同一个地址，面板还以为自己按站点指定了版本；等到两个版本真的同时跑起来，
// 后起的那个 fpm 根本 bind 不上，站点请求全被先起的版本处理。
// 现在改成：解析不到就报错（错误里带版本号、文件路径与原因），绝不猜端口。

// ---------- 存储层 ----------

// Manager 提供站点的增删改查。
type Manager struct {
	st  *store.Store
	opt Options
}

// NewManager 创建站点管理器。
func NewManager(st *store.Store, opt Options) *Manager {
	return &Manager{st: st, opt: opt}
}

// Options 返回生成配置所用的选项（面板保存配置时需要保持一致）。
func (m *Manager) Options() Options { return m.opt }

const siteCols = `id,domain,aliases,root,php_version,rewrite,ssl_enabled,ssl_cert,ssl_key,
	ssl_provider,ssl_expires,proxy_pass,extra_conf,enabled,remark,created_at,updated_at`

func scanSite(sc interface{ Scan(...any) error }) (*Site, error) {
	var s Site
	var ssl, enabled int
	err := sc.Scan(&s.ID, &s.Domain, &s.Aliases, &s.Root, &s.PHPVersion, &s.Rewrite,
		&ssl, &s.SSLCert, &s.SSLKey, &s.SSLProvider, &s.SSLExpires, &s.ProxyPass,
		&s.ExtraConf, &enabled, &s.Remark, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	s.SSLEnabled = ssl == 1
	s.Enabled = enabled == 1
	return &s, nil
}

// List 返回全部站点。
func (m *Manager) List(ctx context.Context) ([]*Site, error) {
	rows, err := m.st.DB().QueryContext(ctx,
		`SELECT `+siteCols+` FROM sites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if out == nil {
		out = []*Site{}
	}
	return out, rows.Err()
}

// Get 按域名取站点。
func (m *Manager) Get(ctx context.Context, domain string) (*Site, error) {
	row := m.st.DB().QueryRowContext(ctx,
		`SELECT `+siteCols+` FROM sites WHERE domain=?`, domain)
	s, err := scanSite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("站点 %s 不存在", domain)
	}
	return s, err
}

// GetByID 按 ID 取站点。
func (m *Manager) GetByID(ctx context.Context, id int64) (*Site, error) {
	row := m.st.DB().QueryRowContext(ctx,
		`SELECT `+siteCols+` FROM sites WHERE id=?`, id)
	s, err := scanSite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("站点 ID %d 不存在", id)
	}
	return s, err
}

// Create 新建站点。
func (m *Manager) Create(ctx context.Context, s *Site) error {
	s.Domain = strings.TrimSpace(strings.ToLower(s.Domain))
	if err := ValidateDomain(s.Domain); err != nil {
		return err
	}
	for _, a := range s.AliasList() {
		if err := ValidateDomain(a); err != nil {
			return fmt.Errorf("附加域名 %s: %w", a, err)
		}
	}
	if err := ValidateProxyPass(s.ProxyPass); err != nil {
		return err
	}
	if s.Rewrite == "" {
		s.Rewrite = "none"
	}
	if _, ok := RewritePresetByName(s.Rewrite); !ok {
		return fmt.Errorf("%w: 未知伪静态模板 %q", ErrInvalid, s.Rewrite)
	}
	if !filepath.IsAbs(s.Root) {
		return fmt.Errorf("%w: 站点根目录必须是绝对路径", ErrInvalid)
	}
	// 域名唯一性：附加域名也不能与已有站点冲突，否则 nginx 会择一匹配，
	// 表现为"某个站点怎么都打不开"这种极难排查的问题。
	for _, name := range append([]string{s.Domain}, s.AliasList()...) {
		conflict, err := m.domainTaken(ctx, name, 0)
		if err != nil {
			return err
		}
		if conflict != "" {
			return fmt.Errorf("%w: 域名 %s 已被站点 %s 使用", ErrInvalid, name, conflict)
		}
	}

	res, err := m.st.DB().ExecContext(ctx,
		`INSERT INTO sites(domain,aliases,root,php_version,rewrite,proxy_pass,extra_conf,remark,enabled)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		s.Domain, s.Aliases, s.Root, s.PHPVersion, s.Rewrite, s.ProxyPass,
		s.ExtraConf, s.Remark, boolToInt(s.Enabled))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%w: 站点 %s 已存在", ErrInvalid, s.Domain)
		}
		return err
	}
	s.ID, _ = res.LastInsertId()
	return nil
}

// Update 更新站点配置。
func (m *Manager) Update(ctx context.Context, s *Site) error {
	if err := ValidateDomain(s.Domain); err != nil {
		return err
	}
	for _, a := range s.AliasList() {
		if err := ValidateDomain(a); err != nil {
			return fmt.Errorf("附加域名 %s: %w", a, err)
		}
	}
	if err := ValidateProxyPass(s.ProxyPass); err != nil {
		return err
	}
	if _, ok := RewritePresetByName(s.Rewrite); !ok {
		return fmt.Errorf("%w: 未知伪静态模板 %q", ErrInvalid, s.Rewrite)
	}
	for _, name := range append([]string{s.Domain}, s.AliasList()...) {
		conflict, err := m.domainTaken(ctx, name, s.ID)
		if err != nil {
			return err
		}
		if conflict != "" {
			return fmt.Errorf("%w: 域名 %s 已被站点 %s 使用", ErrInvalid, name, conflict)
		}
	}
	_, err := m.st.DB().ExecContext(ctx,
		`UPDATE sites SET aliases=?,root=?,php_version=?,rewrite=?,proxy_pass=?,
		 extra_conf=?,remark=?,enabled=?,ssl_enabled=?,ssl_cert=?,ssl_key=?,
		 ssl_provider=?,ssl_expires=?,updated_at=datetime('now','localtime')
		 WHERE id=?`,
		s.Aliases, s.Root, s.PHPVersion, s.Rewrite, s.ProxyPass, s.ExtraConf,
		s.Remark, boolToInt(s.Enabled), boolToInt(s.SSLEnabled), s.SSLCert, s.SSLKey,
		s.SSLProvider, s.SSLExpires, s.ID)
	return err
}

// Delete 删除站点记录。
func (m *Manager) Delete(ctx context.Context, domain string) error {
	res, err := m.st.DB().ExecContext(ctx, `DELETE FROM sites WHERE domain=?`, domain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("站点 %s 不存在", domain)
	}
	return nil
}

// domainTaken 检查域名是否已被其它站点占用（主域名或别名），
// 返回占用它的那个站点的域名。excludeID 用于更新时排除自己。
//
// 为什么要连别名一起查：nginx 的 server_name 是按最长匹配择一的，
// 如果两个站点共用同一个名字，表现为"其中一个站点怎么都打不开"，
// 而 nginx -t 不会报任何错 —— 属于最难排查的一类配置问题。
func (m *Manager) domainTaken(ctx context.Context, domain string, excludeID int64) (string, error) {
	sites, err := m.List(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range sites {
		if s.ID == excludeID {
			continue
		}
		if s.Domain == domain {
			return s.Domain, nil
		}
		for _, a := range s.AliasList() {
			if a == domain {
				return s.Domain, nil
			}
		}
	}
	return "", nil
}

// AllDomains 返回所有已被占用的域名（含别名），用于前端提示冲突。
func (m *Manager) AllDomains(ctx context.Context) (map[string]string, error) {
	sites, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range sites {
		out[s.Domain] = s.Domain
		for _, a := range s.AliasList() {
			out[a] = s.Domain
		}
	}
	return out, nil
}

// ---------- 小工具 ----------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SiteDir 返回域名的默认站点目录（~/www/<domain>）。
//
// 用域名做目录名，但必须校验字符集：域名里可能有 '-'，也可能（理论上）
// 被构造成含路径分隔符的形式，因此这里再走一次白名单校验。
func SiteDir(wwwRoot, domain string) (string, error) {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if !reDirSafe.MatchString(domain) {
		return "", fmt.Errorf("%w: 域名不能作为目录名", ErrInvalid)
	}
	root := filepath.Join(wwwRoot, domain)
	if filepath.Dir(root) != filepath.Clean(wwwRoot) {
		return "", fmt.Errorf("%w: 目录越界", ErrInvalid)
	}
	return root, nil
}

// ParsePort 从 "127.0.0.1:9000" 中取端口。
func ParsePort(pass string) int {
	if i := strings.LastIndex(pass, ":"); i >= 0 {
		if n, err := strconv.Atoi(pass[i+1:]); err == nil {
			return n
		}
	}
	return 0
}

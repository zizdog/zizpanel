// Package proxies 是「反向代理」这个独立功能的后端模型。
//
// 为什么单独做一个包、而不是继续挂在「网站管理」下面：
//
//	网站管理的模型是"一个站点 = 域名 + 根目录 + PHP/伪静态 + 可选反代"，
//	用户要建的反代规则里**大半根本没有站点目录**（只是把某个端口/域名转到
//	另一台机器的服务上）。硬塞进站点模型会出现"必须填一个并不存在的根目录"
//	这种别扭，还容易和站点 vhost 抢同一个文件名。
//
//	所以这里是一等公民：一条规则 = 监听端口 + 匹配条件（域名/Host/路径前缀）
//	+ 目标地址。生成的 nginx 配置文件用 `proxy-<id>.conf` 前缀，与站点文件
//	（<域名>.conf）不会重名。
package proxies

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Rule 是一条反向代理规则。
type Rule struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Listen 是监听端口（必填）。面板与目标都不在本机时也应该能选别的端口，
	// 所以不写死 80/443。
	Listen int `json:"listen"`
	// Domains 是域名匹配（可多个，逗号分隔）。留空 = 该端口上所有 Host 都匹配。
	Domains string `json:"domains"`
	// Path 是路径前缀匹配（可选），例如 /api。留空 = 所有路径。
	Path string `json:"path"`
	// Target 是目标地址，如 http://192.168.1.8:8090 或 https://127.0.0.1:8443。
	Target string `json:"target"`
	// PreserveHost 为 true 时把客户端的 Host 原样透传给目标。
	//
	// 默认 false：多数后端（尤其是按 Host 分站的 nginx）需要看到目标的 Host，
	// 否则会 404。反过来，少数应用必须看到原始 Host（虚拟主机型），这才给它开关。
	PreserveHost bool `json:"preserve_host"`
	// Websocket 为 true 时注入 Upgrade/Connection 与 upgrade map。
	//
	// 默认 **true**：反代一个带界面的服务（Docker 管理、面板自身、Uptime Kuma…）
	// 不代理 WebSocket 基本等于半残，而"忘了开"是很难自查的问题。
	Websocket bool `json:"websocket"`
	// Enabled 为 false 时不生成配置（相当于停用，但规则留着）。
	Enabled bool   `json:"enabled"`
	Remark  string `json:"remark"`

	// ---- HTTPS ----
	//
	// 字段命名与站点侧（sites.Site）逐字一致，这样证书来源、按域名匹配证书、
	// 到期展示这些能力可以整套复用，不需要为反代再造一套概念。
	//
	// SSLProvider 的取值与站点侧相同：self / mkcert / manual / acme。
	// acme 存的是**面板证书库的路径**（<DataDir>/certs/<primary>/...），
	// 续期是同路径覆盖，所以这里绝不复制作副本。
	SSLEnabled  bool   `json:"ssl_enabled"`
	SSLCert     string `json:"ssl_cert"`
	SSLKey      string `json:"ssl_key"`
	SSLProvider string `json:"ssl_provider"`
	// SSLExpires 只用于展示（真实到期时间以证书文件为准，见 proxySSLView）。
	SSLExpires string `json:"ssl_expires"`

	// ---- HTTPS 上游（TLS 到目标这一段）----

	// TLSName 是发往 HTTPS 上游的 SNI（对应 nginx 的 proxy_ssl_name）。
	//
	// 为什么需要它：nginx 对 `proxy_pass https://…` **默认不发 SNI**，而上游
	// （例如另一台面板/另一台 nginx）通常有多个 443 server 块 —— 没有 SNI 就会
	// 落到 default server，表现是"反代配好了，打开的却是另一个站点/另一张证书"。
	// 留空时自动推导：目标是域名就用它本身；目标是 IP 就用本条规则的第一个域名。
	TLSName string `json:"tls_name"`
	// StandardHeaders 为 true 时补齐"一键常用请求头"（对齐 Lucky 的预设）：
	// X-Forwarded-Host / X-Forwarded-Port / REMOTE-HOST。
	// 面板本来就默认发 Host / X-Real-IP / X-Forwarded-For / X-Forwarded-Proto，
	// 所以这里只补"还缺的那几个"，老规则（false）的输出保持逐字不变。
	StandardHeaders bool `json:"standard_headers"`

	Created time.Time `json:"created_at"`
	Updated time.Time `json:"updated_at"`
}

// Validate 校验一条规则，返回**给用户看**的错误。
//
// 逐项检查而不是笼统报"参数错误"：反代规则里各处都能填错，
// 而用户需要知道是哪个字段、为什么不行。
func (r *Rule) Validate() error {
	r.Name = strings.TrimSpace(r.Name)
	r.Domains = strings.TrimSpace(r.Domains)
	r.Path = strings.TrimSpace(r.Path)
	r.Target = strings.TrimSpace(r.Target)
	r.Remark = strings.TrimSpace(r.Remark)
	r.SSLCert = strings.TrimSpace(r.SSLCert)
	r.SSLKey = strings.TrimSpace(r.SSLKey)
	r.SSLProvider = strings.ToLower(strings.TrimSpace(r.SSLProvider))

	if r.Name == "" {
		return fmt.Errorf("请填规则名称（只用于你自己识别，例如「NAS 镜像站」）")
	}
	if r.Listen < 1 || r.Listen > 65535 {
		return fmt.Errorf("监听端口要在 1~65535 之间，当前是 %d", r.Listen)
	}
	if err := ValidateTarget(r.Target); err != nil {
		return err
	}
	if r.Path != "" && !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("路径前缀要以 / 开头，例如 /api（当前是 %q）", r.Path)
	}
	for _, d := range SplitDomains(r.Domains) {
		if strings.ContainsAny(d, " /:\\") {
			return fmt.Errorf("域名里有非法字符：%q（多个域名用逗号分隔，不要带 http:// 和端口）", d)
		}
	}
	// HTTPS：只校验"有没有路径、是不是绝对路径"。
	//
	// 刻意不在这里 stat 证书文件：Validate 会被仓库层每次保存调用，
	// 而"文件是否真的存在/能不能被 nginx 读"由应用配置时的请求级复核判定
	// （见 internal/web/api_proxies.go 的 verifyProxyTLSServed）——
	// 那是唯一有硬证据的判定点。
	if r.SSLEnabled {
		if r.SSLCert == "" || r.SSLKey == "" {
			return fmt.Errorf("已启用 HTTPS 但缺少证书或私钥；请先到「SSL 证书」页申请，或在证书来源里选一张")
		}
		if !filepath.IsAbs(r.SSLCert) || !filepath.IsAbs(r.SSLKey) {
			return fmt.Errorf("启用 HTTPS 时证书与私钥必须是绝对路径（当前 cert=%q key=%q）", r.SSLCert, r.SSLKey)
		}
	}
	return nil
}

// ValidateTarget 校验目标地址。
//
// 只允许 http/https：反代到别的协议（比如直接转 TCP 流）不是这个功能的范围，
// 放行只会让用户写一个永远不工作的规则。
func ValidateTarget(target string) error {
	if target == "" {
		return fmt.Errorf("请填目标地址，例如 http://192.168.1.8:8090")
	}
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("目标地址不是合法 URL：%v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("目标地址必须以 http:// 或 https:// 开头（当前是 %q）", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("目标地址缺少主机名，例如 http://192.168.1.8:8090")
	}
	if _, err := strconv.Atoi(u.Port()); err != nil && u.Port() != "" {
		return fmt.Errorf("目标端口不是数字：%q", u.Port())
	}
	return nil
}

// TargetHostPort 返回目标的主机与端口（端口缺省按 scheme 补）。
// 目标地址拼错时返回错误，调用方据此拒绝保存。
func (r *Rule) TargetHostPort() (string, int, error) {
	u, err := url.Parse(r.Target)
	if err != nil {
		return "", 0, err
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("目标地址缺少主机名")
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, cerr := strconv.Atoi(p)
		if cerr != nil {
			return "", 0, fmt.Errorf("目标端口不是数字：%q", p)
		}
		port = n
	}
	return host, port, nil
}

// SSLProviderLabel 把证书来源翻译成界面可读的中文。
//
// 放在 proxies 包里是为了让"站点侧"与"反向代理侧"共用同一套文案
// （web 包的 sslProviderLabel 直接委托到这里），避免同一个 provider
// 在两个页面显示成两个名字。
func SSLProviderLabel(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "self":
		return "自签证书"
	case "mkcert":
		return "mkcert 本地 CA"
	case "manual":
		return "手工上传"
	case "acme":
		return "ACME 自动证书（Let's Encrypt 等）"
	case "":
		return "未配置"
	default:
		return provider
	}
}

// SplitDomains 把逗号/空格分隔的域名列表拆开并去掉空项。
func SplitDomains(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(f))
	for _, d := range f {
		d = strings.TrimSpace(strings.ToLower(d))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// VhostName 返回这条规则写进 nginx vhosts 目录时的文件名（不含 .conf）。
//
// 用 id 而不是规则名：名字是中文、还可能重复，做文件名既危险又不稳；
// 前缀 proxy- 保证不会和站点文件（<域名>.conf）撞名。
func (r *Rule) VhostName() string { return fmt.Sprintf("proxy-%d", r.ID) }

// Generate 生成 nginx server 块。
//
// 生成策略（刻意保守）：
//   - 监听端口、server_name、可选 location 前缀都显式写出，不做隐式默认；
//   - 代理头逐条列全（Host / X-Real-IP / X-Forwarded-For / X-Forwarded-Proto），
//     少一个都会让后端拿到错误的客户端信息；
//   - WebSocket 需要 http 上下文里的 upgrade map，那个由面板保证存在
//     （见 sites.UpgradeMapName），这里只负责引用。
//
// **未启用 SSL 时输出与加 HTTPS 之前逐字一致**（有黄金测试钉住）：
// 历史规则不能被这次改动影响，否则一次升级就会改掉用户在用的所有反代配置。
func (r *Rule) Generate(logDir string) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	domains := SplitDomains(r.Domains)
	serverName := "_"
	if len(domains) > 0 {
		serverName = strings.Join(domains, " ")
	}

	var b strings.Builder
	b.WriteString("# 由 ZizPanel「反向代理」生成 —— 请勿手工编辑（会被面板覆盖）\n")
	if r.Name != "" {
		b.WriteString("# 规则：" + r.Name + "\n")
	}
	b.WriteString("server {\n")
	if r.SSLEnabled {
		// 注意：nginx 对"同一端口上只要有 listen ... ssl，就要求该端口**每个**
		// server 块都有 ssl_certificate"。所以兜底拒绝块也必须带证书，
		// 见 GenerateRejectWithCert。
		fmt.Fprintf(&b, "\tlisten      %d ssl;\n", r.Listen)
	} else {
		fmt.Fprintf(&b, "\tlisten      %d;\n", r.Listen)
	}
	fmt.Fprintf(&b, "\tserver_name %s;\n", serverName)

	if r.SSLEnabled {
		b.WriteString("\n\t# HTTPS（证书来源：" + SSLProviderLabel(r.SSLProvider) + "；acme 续期同路径覆盖，无需改这里）\n")
		fmt.Fprintf(&b, "\tssl_certificate     %s;\n", r.SSLCert)
		fmt.Fprintf(&b, "\tssl_certificate_key %s;\n", r.SSLKey)
		b.WriteString("\tssl_protocols       TLSv1.2 TLSv1.3;\n")
		b.WriteString("\tssl_ciphers         ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305;\n")
		b.WriteString("\tssl_prefer_server_ciphers off;\n")
		b.WriteString("\tssl_session_cache   shared:SSL:10m;\n")
		b.WriteString("\tssl_session_timeout 1d;\n")
		b.WriteString("\tssl_stapling        off;\n")
	}

	b.WriteString("\n\t# 反代目标的真实地址（日志里用得上）\n")
	fmt.Fprintf(&b, "\t# target: %s\n", r.Target)

	prefix := r.Path
	if prefix == "" {
		prefix = "/"
	}
	fmt.Fprintf(&b, "\n\tlocation %s {\n", prefix)
	fmt.Fprintf(&b, "\t\tproxy_pass %s;\n", strings.TrimRight(r.Target, "/"))

	// Host 头：默认改成目标主机（多数后端按 Host 分站，透传原始 Host 会 404）。
	if r.PreserveHost {
		b.WriteString("\t\tproxy_set_header Host $host;\n")
	} else {
		host, port, err := r.TargetHostPort()
		if err != nil {
			return "", err
		}
		u, _ := url.Parse(r.Target)
		hostHeader := host
		if (u.Scheme == "http" && port != 80) || (u.Scheme == "https" && port != 443) {
			hostHeader = net.JoinHostPort(host, strconv.Itoa(port))
		}
		fmt.Fprintf(&b, "\t\tproxy_set_header Host %s;\n", hostHeader)
	}
	// HTTPS 上游：必须显式发 SNI（见 Rule.TLSName 的说明）。
	if u, uerr := url.Parse(r.Target); uerr == nil && strings.EqualFold(u.Scheme, "https") {
		name := strings.TrimSpace(r.TLSName)
		if name == "" {
			if host, _, herr := r.TargetHostPort(); herr == nil && net.ParseIP(host) == nil {
				name = host
			} else if ds := SplitDomains(r.Domains); len(ds) > 0 {
				// 目标是 IP：上游多半按域名分站，用本条规则匹配的第一个域名当 SNI
				name = ds[0]
			}
		}
		b.WriteString("\t\t# HTTPS 上游：不发 SNI 会落到上游的默认 server（打开的可能是另一个站点）\n")
		b.WriteString("\t\tproxy_ssl_server_name on;\n")
		if name != "" {
			fmt.Fprintf(&b, "\t\tproxy_ssl_name %s;\n", name)
		}
		// 上游常用自签 / staging 证书；需要校验链路时再手动打开。
		b.WriteString("\t\tproxy_ssl_verify off;\n")
	}

	b.WriteString("\t\tproxy_set_header X-Real-IP $remote_addr;\n")
	b.WriteString("\t\tproxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString("\t\tproxy_set_header X-Forwarded-Proto $scheme;\n")
	// 一键补齐的常用请求头（Lucky 风格预设）。默认关，老规则的输出逐字不变。
	if r.StandardHeaders {
		b.WriteString("\t\t# 常用请求头（一键补齐）\n")
		b.WriteString("\t\tproxy_set_header X-Forwarded-Host $host;\n")
		b.WriteString("\t\tproxy_set_header X-Forwarded-Port $server_port;\n")
		b.WriteString("\t\tproxy_set_header REMOTE-HOST $remote_addr;\n")
	}
	b.WriteString("\t\tproxy_http_version 1.1;\n")

	if r.Websocket {
		b.WriteString("\t\t# WebSocket（upgrade map 由面板确保存在）\n")
		b.WriteString("\t\tproxy_set_header Upgrade $http_upgrade;\n")
		b.WriteString("\t\tproxy_set_header Connection $connection_upgrade;\n")
	}

	// 反代经常遇到大文件/慢后端，默认 60s 会把正常请求掐断
	b.WriteString("\n\t\tproxy_connect_timeout 60s;\n")
	b.WriteString("\t\tproxy_send_timeout    3600s;\n")
	b.WriteString("\t\tproxy_read_timeout    3600s;\n")
	b.WriteString("\t\tproxy_buffering       off;\n")
	if logDir != "" {
		fmt.Fprintf(&b, "\n\t\taccess_log %s/proxy-%d.access.log;\n", logDir, r.ID)
		fmt.Fprintf(&b, "\t\terror_log  %s/proxy-%d.error.log;\n", logDir, r.ID)
	}
	b.WriteString("\t}\n}\n")
	return b.String(), nil
}

// RejectVhostName 是某个端口上「兜底拒绝」配置的文件名。
//
// 命名与规则文件用同一前缀，保证仍落在 vhosts/ 的包含范围内，
// 且不会和站点文件（<域名>.conf）撞名。
func RejectVhostName(port int) string { return fmt.Sprintf("proxy-reject-%d", port) }

// RejectPorts 返回"需要兜底拒绝块"的端口集合。
//
// 判定条件只有一条：该端口上存在**已启用且带域名**的规则。
// 端口上全是通配规则（domains 为空）时不返回它 —— 用户的意图本来就是
// "这个端口随便什么域名都反代"，给这种端口加拒绝块会把规则一起打死。
//
// 抽成独立函数是为了让它能被单测直接钉住：真正的写盘路径要走提权助手，
// 单测碰不得（见 AGENTS.md「测试不许碰生产配置」）。
func RejectPorts(rules []*Rule) map[int]bool {
	need := map[int]bool{}
	for _, r := range rules {
		if r == nil || !r.Enabled {
			continue
		}
		if len(SplitDomains(r.Domains)) > 0 {
			need[r.Listen] = true
		}
	}
	return need
}

// OrphanRejectPorts 从 vhosts 目录的文件名里挑出"已经不需要"的兜底块端口。
//
// need 是需要保留的端口集合；返回的是应当删除的端口。
// 名字对不上 `proxy-reject-<数字>.conf` 的一律忽略（可能来自别的工具或手工创建，
// 面板没有权力删不属于自己的文件）。
func OrphanRejectPorts(names []string, need map[int]bool) []int {
	var out []int
	for _, name := range names {
		if !strings.HasPrefix(name, "proxy-reject-") || !strings.HasSuffix(name, ".conf") {
			continue
		}
		portStr := strings.TrimSuffix(strings.TrimPrefix(name, "proxy-reject-"), ".conf")
		port, err := strconv.Atoi(portStr)
		if err != nil || need[port] {
			continue
		}
		out = append(out, port)
	}
	return out
}

// GenerateReject 生成某端口的兜底 server 块。
//
// 为什么必须有它：nginx 在"某个端口上只有一个 server 块"时会把它当作**默认 server**，
// 于是 server_name 形同虚设 —— 任何 Host（以及没有 Host 的请求）都会被转发到后端。
// 2026-09-16 实测：规则写 domains=lede.zizdog.com，但不带 Host 头访问同样返回 200，
// 等于域名限制从来没有生效。这里显式声明 default_server 并直接 444（不响应即断开），
// 让"域名对不上"在 nginx 层就被拒绝，而不是把请求漏给后端。
//
// 只在**该端口存在带域名的规则**时才生成：如果端口上全是通配规则（domains 为空），
// 用户的意图本来就是"这个端口随便什么域名都反代"，兜底块会把这类规则一起打死。
func GenerateReject(port int, logDir string) string {
	var b strings.Builder
	b.WriteString("# 由 ZizPanel「反向代理」生成 —— 请勿手工编辑（会被面板覆盖）\n")
	b.WriteString("server {\n")
	fmt.Fprintf(&b, "\tlisten      %d default_server;\n", port)
	b.WriteString("\tserver_name _;\n")
	b.WriteString("\n\t# 域名对不上就断开，不把请求漏给后端\n")
	b.WriteString("\treturn 444;\n")
	if logDir != "" {
		fmt.Fprintf(&b, "\n\taccess_log %s/proxy-reject-%d.access.log;\n", logDir, port)
		fmt.Fprintf(&b, "\terror_log  %s/proxy-reject-%d.error.log;\n", logDir, port)
	}
	b.WriteString("}\n")
	return b.String()
}

// GenerateRejectWithCert 生成带证书行的兜底 server 块。
//
// 为什么需要它（2026-09-16 用 nginx 1.31.5 在本机 /tmp 沙箱实测）：
// nginx 的规则是"同一 listen 端口上只要有**一个** server 块写了 `ssl`，
// 那么该端口上**每个** server 块都必须能取到 ssl_certificate"，否则
// `nginx -t` 直接 [emerg]：
//
//	no "ssl_certificate" is defined for the "listen ... ssl" directive
//
// 而反代的兜底拒绝块（proxy-reject-<port>.conf）默认不含证书。于是"给某端口
// 开 HTTPS"这件事必须同时改两个文件，中间态很容易被 nginx 判死。两种形态：
//
//   - ssl=true ：最终形态 `listen <port> ssl default_server;` + 证书。
//   - ssl=false：切换过程中的**中性形态** `listen <port> default_server;` + 证书。
//     实测"带证书行的非 SSL server"在任何组合下都合法（端口上没有 ssl 时
//     证书行只是没用上），所以先写它、再改规则 vhost，就不会出现非法中间态。
//
// certPath/keyPath 为空时退回完全不带证书的旧形态（保证非 SSL 端口输出逐字不变）。
func GenerateRejectWithCert(port int, logDir, certPath, keyPath string, ssl bool) string {
	if certPath == "" || keyPath == "" {
		return GenerateReject(port, logDir)
	}
	var b strings.Builder
	b.WriteString("# 由 ZizPanel「反向代理」生成 —— 请勿手工编辑（会被面板覆盖）\n")
	b.WriteString("server {\n")
	if ssl {
		fmt.Fprintf(&b, "\tlisten      %d ssl default_server;\n", port)
	} else {
		fmt.Fprintf(&b, "\tlisten      %d default_server;\n", port)
	}
	b.WriteString("\tserver_name _;\n")
	// 兜底块的证书只为满足 nginx「同端口 server 块都要有证书」的要求；
	// 它永远 return 444，不会真的把一个正常请求当成该证书的站点来服务。
	b.WriteString("\n\t# 仅用于满足 nginx：同端口有 ssl 时每个 server 块都要有证书\n")
	fmt.Fprintf(&b, "\tssl_certificate     %s;\n", certPath)
	fmt.Fprintf(&b, "\tssl_certificate_key %s;\n", keyPath)
	b.WriteString("\n\t# 域名对不上就断开，不把请求漏给后端\n")
	b.WriteString("\treturn 444;\n")
	if logDir != "" {
		fmt.Fprintf(&b, "\n\taccess_log %s/proxy-reject-%d.access.log;\n", logDir, port)
		fmt.Fprintf(&b, "\terror_log  %s/proxy-reject-%d.error.log;\n", logDir, port)
	}
	b.WriteString("}\n")
	return b.String()
}

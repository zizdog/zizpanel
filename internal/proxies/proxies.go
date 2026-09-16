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
	Enabled bool      `json:"enabled"`
	Remark  string    `json:"remark"`
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
	fmt.Fprintf(&b, "\tlisten      %d;\n", r.Listen)
	fmt.Fprintf(&b, "\tserver_name %s;\n", serverName)
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
	b.WriteString("\t\tproxy_set_header X-Real-IP $remote_addr;\n")
	b.WriteString("\t\tproxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString("\t\tproxy_set_header X-Forwarded-Proto $scheme;\n")
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

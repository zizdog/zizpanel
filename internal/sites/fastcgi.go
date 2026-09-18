package sites

import (
	"strings"
)

// ============================================================================
//  FastCGI 转发参数与"当前可用 PHP 端点"
//
//  为什么把这两样导出到 sites 包：默认站点（internal/web 的「整理默认站点」、
//  internal/services 的 LNMP 收尾与 phpMyAdmin 入口）都要一段
//  `location ~ \.php$ { fastcgi_pass <端点>; <参数> }`，而**端点必须是该机器
//  当前 PHP 版本的专属端点**（多版本设计下每个版本听自己的 Unix socket）。
//
//  曾经的做法是往这些 location 里 `include <brew>/etc/nginx/includes/php-fpm.conf`，
//  而那份片段写死 `fastcgi_pass 127.0.0.1:9000;` —— 在"PHP 只监听专属 socket"的
//  机器上（真机：mini，php@8.2 听 php-fpm-8.2.sock），用户点一次
//  「整理默认站点」就会把默认站点改回 9000 → 502，等于把刚修好的东西弄坏。
//
//  所以：端点解析与参数文本都收敛在这里，**只有一份**；两处调用方各自负责把
//  `fastcgi_pass` 放在参数前面（参数块本身不含 fastcgi_pass）。
// ============================================================================

// FastCGIParamsBlock 返回 location ~ \.php$ 需要的**完整参数块（未缩进）**。
//
// 内容 = fastcgi_index + fastcgi_split_path_info + 共享的参数清单
// （fastcgiParamPairs，与站点 vhost 用的是同一份）+ 超时。
//
// 为什么另起一个名字而不是改 FastCGIParams()：后者是**给站点 vhost 用的、
// 已带缩进的片段**（Generate 已在用，改了会动所有站点配置的输出）；这里要的是
// "默认站点/phpMyAdmin 入口"能直接拼进自己缩进的完整块。两者共用同一份参数清单，
// 所以不存在"两份参数会走样"的问题。
//
// 调用方必须先写 `fastcgi_pass <专属端点>;` —— 本块**不含 fastcgi_pass**，
// 因为端点必须按机器/版本解析（见 PreferredFastCGIPass），绝不能写死 9000。
func FastCGIParamsBlock() string {
	var b strings.Builder
	// ⚠️ 必须先定义 $zp_scheme / $zp_https（schemeTrustBlock 就是那几条 set）。
	//
	// 2026-09-18 mini 真机事故：默认站点/phpMyAdmin 这个块引用了 `$zp_scheme`
	// （参数清单里的 REQUEST_SCHEME / HTTPS），却**没有**那几条 `set` ——
	// 站点 vhost 里恰好有（Generate 走的是 fastcgiParams()），于是"本机能跑、
	// mini 起不来"：mini 上没有任何站点 vhost 定义过这个变量 →
	// `nginx: [emerg] unknown "zp_scheme" variable` → **nginx 直接起不来**，
	// 用户连重装 nginx 都没用（配置还在）。凡引用就必须定义，这个块自己带上。
	b.WriteString(schemeTrustBlock())
	b.WriteString("fastcgi_index  index.php;\n")
	b.WriteString("fastcgi_split_path_info ^(.+?\\.php)(/.*)$;\n")
	b.WriteString("\n")
	// 这两条不放进共享清单：站点 vhost 的 Generate 会自己按上下文写它们
	// （放进去就会在站点配置里出现重复行）。这里补上，默认站点才完整。
	b.WriteString("fastcgi_param  SCRIPT_FILENAME    $request_filename;\n")
	b.WriteString("fastcgi_param  PATH_INFO          $fastcgi_path_info;\n")
	for _, p := range fastcgiParamPairs() {
		b.WriteString("fastcgi_param  " + p + ";\n")
	}
	b.WriteString("\n")
	b.WriteString("fastcgi_read_timeout  300;\n")
	b.WriteString("fastcgi_buffering     off;")
	return b.String()
}

// PreferredFastCGIPass 挑一个"当前可用"的 PHP FastCGI 端点。
//
// 优先级（与面板"PHP 环境"页的口径一致）：
//  1. **真的在监听**的版本（EndpointLive，Unix socket 或 TCP 都算）——
//     只有它能让页面立刻跑起来；
//  2. 配置能解析出端点的版本（配置先写对，PHP 起来即可用）；
//  3. 该版本由面板分配的端点（配置都读不出来时，至少不写 9000）。
//
// 版本按升序挑选（8.2 先于 8.3），与「默认 PHP 版本 = 8.2」一致。
// 全都没有时返回空串 —— 调用方必须**不要**写 `fastcgi_pass ;`
// （那是非法配置，会让 nginx 拒绝加载），而是明确说明"本机还没有可用 PHP"。
func PreferredFastCGIPass(brewPrefix string) (pass, version string) {
	list := DiscoverPHPVersions(brewPrefix)
	for _, pv := range list {
		if pv.Pass != "" && EndpointLive(pv.Pass) {
			return pv.Pass, pv.Version
		}
	}
	for _, pv := range list {
		if pv.Pass != "" {
			return pv.Pass, pv.Version
		}
	}
	for _, pv := range list {
		if strings.TrimSpace(pv.PreferredPass) != "" {
			return pv.PreferredPass, pv.Version
		}
	}
	return "", ""
}

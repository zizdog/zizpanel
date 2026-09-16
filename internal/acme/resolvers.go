package acme

import (
	"net"
	"strings"
)

// DefaultDNS01Resolvers 是 dns-01 预检默认使用的**递归解析器**，全部是 IP 字面量。
//
// 为什么必须是 IP 字面量（这是 2026-09-16 真机排出来的坑）：
// lego 的默认解析器是 `google-public-dns-a.google.com:53` 这类**主机名**，
// 而把主机名交给 DNS 客户端前要先解析它 —— 这一步走的是**系统解析器**。
// 本机与 mini 的路由器都通告了一个**不可达的 IPv6 DNS**（`fd58:...::1`），
// 于是整条签发卡在：
//
//	DNS call error: read udp [fd58:...]->[fd58:cbf6:7fb8::1]:53: i/o timeout
//
// 传 IP 字面量后 lego 直接向该地址发查询：不做主机名解析，也不会碰 IPv6。
// 面板里其它"IPv6 不可达会挂住"的地方也是同样的处理思路（见 qwentts 的 IPv4 补丁）。
//
// 选公共解析器而不是系统解析器：ACME 校验查的是**公网 TXT 记录**，
// 系统那台解析器可能正是坏的那个，而这里要的就是"一定能查到公网记录"。
var DefaultDNS01Resolvers = []string{
	"223.5.5.5:53",    // AliDNS
	"223.6.6.6:53",    // AliDNS
	"119.29.29.29:53", // DNSPod
	"1.1.1.1:53",      // Cloudflare
	"8.8.8.8:53",      // Google
}

// dns01Resolvers 返回本次 dns-01 要用的解析器列表（已过滤、已补 :53）。
//
// 过滤规则：**只接受 IP 字面量**，主机名一律丢弃并如实告警。
// 允许主机名等于把上面那个坑重新放回签发路径 —— 宁可少一个解析器，
// 也不让签发有机会挂在系统解析器上。
func (m *Manager) dns01Resolvers() []string {
	in := m.DNS01Resolvers
	if len(in) == 0 {
		in = DefaultDNS01Resolvers
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			host, port = s, "53"
		}
		if net.ParseIP(host) == nil {
			m.emit("忽略无效的 dns-01 解析器 %q：只接受 IP 字面量"+
				"（主机名要先做一次系统解析，IPv6 DNS 不可达时会把签发挂死）", s)
			continue
		}
		out = append(out, net.JoinHostPort(host, port))
	}
	return out
}

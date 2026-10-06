package sysinfo

// ---------------------------------------------------------------------------
//  本机对外地址（"别的设备该用哪个 IP 连我"）
//
//  用户 2026-10-06 报障：共享条目显示「地址未知：读不到本机 IP」。根因是硬编码 en0 ——
//  Mac mini 的**以太网才是 en0、Wi-Fi 是 en1**，拔了网线只用 Wi-Fi 时
//  `ipconfig getifaddr en0` 就是空的（这台 mini 的链路是 1200 Mbps Wi-Fi，正好对上）。
//  USB/雷雳网卡、iPhone USB 共享也都落在别的 enX 上。
//
//  所以这里**不 spawn 进程、不写死网卡名**，直接问系统两张表：
//    ① 默认路由的接口（UDP "连接"一个公网地址即可问出来，不发包）——系统实际在用的那条路；
//    ② 所有接口的地址表（net.Interfaces），按"物理 en* + 私有地址"排序。
//  刻意排除 Docker/colima 的 vmenet*/bridge*、VPN 的 utun*、AirDrop 的 awdl*/llw* ——
//  它们也有"看起来像内网"的地址，但别的设备根本连不上。
//
//  读不到就返回空串：调用方必须如实写「地址未知」，不许编一个地址（铁律 7）。
// ---------------------------------------------------------------------------

import (
	"context"
	"net"
	"strings"
)

// lanIface 是一个接口的候选地址（抽成结构体，排序逻辑才能单测）。
type lanIface struct {
	Name string
	IPv4 []string
}

// lanIPv4DefaultRoute 返回"当前默认路由用的源地址"（空串 = 没有默认路由）。
// 注入点：单测不许依赖真机网络。
var lanIPv4DefaultRoute = func(ctx context.Context) string {
	// UDP "连接"不产生任何报文，只是让内核按选路规则告诉我们源地址。
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", "203.0.113.1:9") // TEST-NET-3，只为选路
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return ""
	}
	if ip4 := addr.IP.To4(); ip4 != nil {
		return ip4.String()
	}
	return ""
}

// lanIPv4Interfaces 列出所有 up 且非回环接口的 IPv4（注入点，理由同上）。
var lanIPv4Interfaces = func() []lanIface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]lanIface, 0, len(ifaces))
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		item := lanIface{Name: ifi.Name}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil {
				item.IPv4 = append(item.IPv4, ip4.String())
			}
		}
		if len(item.IPv4) > 0 {
			out = append(out, item)
		}
	}
	return out
}

// LANIPv4 返回本机对外地址（读不到返回空串）。
func LANIPv4(ctx context.Context) string {
	return pickLANIPv4(lanIPv4DefaultRoute(ctx), lanIPv4Interfaces())
}

// pickLANIPv4 是纯函数：给定"默认路由的源地址 + 全部接口地址"，挑出最该用的那个。
//
// 顺序（每一条都有真机理由）：
//  1. 默认路由所在接口的地址 —— 系统正在用的那条路（Wi-Fi 也好、USB 网卡也好）；
//  2. 名字是 en* 的物理接口上的私有地址 —— 覆盖"默认路由在 VPN(utun) 上"的情况；
//  3. 其它非虚拟接口上的私有地址；
//  4. 退回默认路由地址（哪怕是公网/VPN 地址，它至少是通的）。
func pickLANIPv4(defIP string, ifaces []lanIface) string {
	if defIP != "" && !virtualIface(ifaceOwning(defIP, ifaces)) && usableIPv4(defIP) {
		return defIP
	}
	best, bestScore := "", -1
	for _, it := range ifaces {
		if virtualIface(it.Name) {
			continue
		}
		for _, ip := range it.IPv4 {
			if !usableIPv4(ip) {
				continue
			}
			score := 0
			if strings.HasPrefix(strings.ToLower(it.Name), "en") {
				score += 2
			}
			if privateIPv4(ip) {
				score++
			}
			if score > bestScore {
				best, bestScore = ip, score
			}
		}
	}
	if best != "" {
		return best
	}
	if usableIPv4(defIP) {
		return defIP
	}
	return ""
}

// ifaceOwning 反查某个地址属于哪个接口（不在列表里返回空串）。
func ifaceOwning(ip string, ifaces []lanIface) string {
	for _, it := range ifaces {
		for _, a := range it.IPv4 {
			if a == ip {
				return it.Name
			}
		}
	}
	return ""
}

// usableIPv4 排除回环、自分配（169.254/16）、组播与未指定地址。
func usableIPv4(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil {
		return false
	}
	return !p.IsLoopback() && !p.IsLinkLocalUnicast() && !p.IsLinkLocalMulticast() &&
		!p.IsMulticast() && !p.IsUnspecified()
}

// privateIPv4 判断 RFC1918 私网地址（10/8、172.16/12、192.168/16）。
func privateIPv4(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil {
		return false
	}
	return p.IsPrivate()
}

// virtualIface 判断"别的设备连不上的接口"：VPN / AirDrop / 隧道 / 容器网桥。
// 按 macOS 的命名习惯匹配（名字为空时不算虚拟，交给地址判据兜底）。
func virtualIface(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	for _, p := range []string{"utun", "awdl", "llw", "gif", "stf", "anpi", "lo", "ap", "vmenet", "bridge", "docker", "colima", "tap", "tun"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

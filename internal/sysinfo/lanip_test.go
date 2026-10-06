package sysinfo

// 「本机对外地址」门禁。
//
// 这一条抓的是真事故（用户 2026-10-06 报"共享条目显示地址未知：读不到本机 IP"）：
// 旧实现写死 `ipconfig getifaddr en0`，而 Mac mini 的**以太网才是 en0、Wi-Fi 是 en1** ——
// 拔了网线、走 Wi-Fi 的机器就永远拿不到地址。所以这里把"应该挑哪个地址"钉死：
//   ① Mac mini 真实布局（en0 无地址 + en1 Wi-Fi 有地址 + 默认路由在 en1）必须出 en1 的地址；
//   ② 默认路由在 VPN(utun) 上时必须挑物理网卡的局域网地址，**不许**把 VPN 地址给用户；
//   ③ Docker/colima 的 vmenet/bridge 地址一律不许当选（别的设备连不上）；
//   ④ 自分配地址（169.254.x）与空列表 ⇒ 空串（调用方如实写"地址未知"，不许编地址）。

import (
	"context"
	"testing"
)

func TestPickLANIPv4(t *testing.T) {
	cases := []struct {
		name     string
		defIP    string
		ifaces   []lanIface
		want     string
		wantCase string
	}{
		{
			name:  "Mac mini：en0（以太网）没插线，Wi-Fi 在 en1，默认路由走 en1",
			defIP: "192.168.1.181",
			ifaces: []lanIface{
				{Name: "en0", IPv4: []string{}},
				{Name: "en1", IPv4: []string{"192.168.1.181"}},
				{Name: "awdl0", IPv4: []string{"169.254.12.34"}},
				{Name: "utun3", IPv4: []string{"100.64.0.7"}},
			},
			want:     "192.168.1.181",
			wantCase: "拔网线的 mini 也必须给得出地址（旧实现看 en0 = 空）",
		},
		{
			name:  "默认路由在 VPN 上：必须挑物理网卡的局域网地址，而不是 utun 地址",
			defIP: "10.8.0.6",
			ifaces: []lanIface{
				{Name: "utun4", IPv4: []string{"10.8.0.6"}},
				{Name: "en0", IPv4: []string{"192.168.1.20"}},
			},
			want:     "192.168.1.20",
			wantCase: "VPN 地址给局域网设备没用",
		},
		{
			name:  "Docker/colima 网桥不许当选",
			defIP: "",
			ifaces: []lanIface{
				{Name: "vmenet0", IPv4: []string{"192.168.5.2"}},
				{Name: "bridge100", IPv4: []string{"192.168.5.1"}},
				{Name: "en0", IPv4: []string{"10.0.0.9"}},
			},
			want:     "10.0.0.9",
			wantCase: "容器网桥的地址别的设备连不上",
		},
		{
			name:  "只有自分配地址 ⇒ 空串",
			defIP: "169.254.1.1",
			ifaces: []lanIface{
				{Name: "en0", IPv4: []string{"169.254.1.1"}},
			},
			want:     "",
			wantCase: "169.254/16 不是能用的地址，必须如实返回空",
		},
		{
			name:     "一个接口都没有 ⇒ 空串",
			defIP:    "",
			ifaces:   nil,
			want:     "",
			wantCase: "读不到就空串，不许编地址",
		},
		{
			name:  "默认路由走公网地址的物理网卡 ⇒ 就用它",
			defIP: "203.0.113.9",
			ifaces: []lanIface{
				{Name: "en0", IPv4: []string{"203.0.113.9", "10.1.1.1"}},
			},
			want:     "203.0.113.9",
			wantCase: "默认路由地址就是别人连得上的那个",
		},
		{
			name:  "只有 iPhone USB 共享的地址 ⇒ 也要给出来",
			defIP: "172.20.10.2",
			ifaces: []lanIface{
				{Name: "en6", IPv4: []string{"172.20.10.2"}},
			},
			want:     "172.20.10.2",
			wantCase: "USB 共享也是有效路径（不要只认 192.168）",
		},
	}
	for _, c := range cases {
		if got := pickLANIPv4(c.defIP, c.ifaces); got != c.want {
			t.Errorf("%s：挑出 %q，应为 %q（%s）", c.name, got, c.want, c.wantCase)
		}
	}
}

// TestLANIPv4UsesInjections 确认探测函数真的走注入的默认路由/接口表（而不是硬编码 en0）。
func TestLANIPv4UsesInjections(t *testing.T) {
	oldDef, oldIfaces := lanIPv4DefaultRoute, lanIPv4Interfaces
	t.Cleanup(func() { lanIPv4DefaultRoute, lanIPv4Interfaces = oldDef, oldIfaces })
	lanIPv4DefaultRoute = func(context.Context) string { return "192.168.50.7" }
	lanIPv4Interfaces = func() []lanIface {
		return []lanIface{
			{Name: "en0", IPv4: []string{}}, // 以太网没插线（mini 的日常）
			{Name: "en5", IPv4: []string{"192.168.50.7"}},
		}
	}
	if got := LANIPv4(context.Background()); got != "192.168.50.7" {
		t.Fatalf("LANIPv4 = %q，应为 en5 的地址", got)
	}
}

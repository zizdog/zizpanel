package acme

import (
	"net"
	"strings"
	"testing"
)

// TestDefaultDNS01ResolversAreIPLiterals 是防止旧坑复发的门禁。
//
// 真机故障（2026-09-16，mini）：lego 默认拿 `google-public-dns-a.google.com:53`
// 当解析器，解析这个主机名要走系统解析器 → 命中路由器通告的**不可达 IPv6 DNS**
// → 每次签发都以 `DNS call error ... i/o timeout` 失败。
// 只要默认值里出现主机名，这条测试就必须拦住。
func TestDefaultDNS01ResolversAreIPLiterals(t *testing.T) {
	if len(DefaultDNS01Resolvers) == 0 {
		t.Fatal("默认 dns-01 解析器不能为空")
	}
	for _, s := range DefaultDNS01Resolvers {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			t.Fatalf("默认解析器 %q 缺少端口: %v", s, err)
		}
		if net.ParseIP(host) == nil {
			t.Fatalf("默认解析器 %q 的 host 不是 IP 字面量；主机名会走系统解析器，IPv6 DNS 不可达时会把签发挂死", s)
		}
		if port != "53" {
			t.Fatalf("默认解析器 %q 的端口应为 53，实际 %s", s, port)
		}
	}
}

// TestDNS01Resolvers 覆盖过滤规则：补端口、丢主机名、留 IP。
func TestDNS01Resolvers(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"留空时用默认", nil, DefaultDNS01Resolvers},
		{"没有端口时补 :53", []string{"223.5.5.5"}, []string{"223.5.5.5:53"}},
		{
			"主机名被丢弃（这正是历史故障的根因）",
			[]string{"google-public-dns-a.google.com:53", "1.1.1.1:53"},
			[]string{"1.1.1.1:53"},
		},
		{"用户显式给的 IPv6 字面量保留", []string{"[2400:3200::1]:53"}, []string{"[2400:3200::1]:53"}},
		{"空串跳过", []string{"", "  ", "8.8.8.8:53"}, []string{"8.8.8.8:53"}},
		{"全是主机名则结果为空", []string{"dns.google:53"}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, rec, _ := newTestManager(t)
			m.DNS01Resolvers = c.in
			got := m.dns01Resolvers()
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("dns01Resolvers() = %v，期望 %v", got, c.want)
			}

			// 被丢弃的主机名必须留痕，不能静默改写用户配置。
			hostnameDropped := false
			for _, s := range c.in {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				host := s
				if h, _, err := net.SplitHostPort(s); err == nil {
					host = h
				}
				if net.ParseIP(host) == nil {
					hostnameDropped = true
				}
			}
			hasLog := strings.Contains(rec.all(), "忽略无效的 dns-01 解析器")
			if hostnameDropped != hasLog {
				t.Fatalf("丢弃主机名的告警与实际情况不符：hostnameDropped=%v 日志=%q", hostnameDropped, rec.all())
			}
		})
	}
}

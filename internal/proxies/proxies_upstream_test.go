package proxies

import (
	"strings"
	"testing"
)

// TestHTTPSUpstreamGetsSNI：反代 https:// 上游必须发 SNI。
//
// 真机语境（2026-09-16）：把 panel.zizdog.com:8889 反代到 https://192.168.1.4:8443，
// nginx 默认不发 SNI → 上游落到 default server → 打开的是别的站点/别的证书。
func TestHTTPSUpstreamGetsSNI(t *testing.T) {
	cases := []struct {
		name    string
		rule    Rule
		wantSNI string
	}{
		{
			name:    "目标是域名：SNI 用它本身",
			rule:    Rule{Name: "a", Listen: 8889, Domains: "panel2.zizdog.com", Target: "https://panel.example.com:8443", Enabled: true},
			wantSNI: "proxy_ssl_name panel.example.com;",
		},
		{
			name:    "目标是 IP：SNI 取本条规则的第一个域名",
			rule:    Rule{Name: "b", Listen: 8889, Domains: " panel2.zizdog.com , other.zizdog.com ", Target: "https://192.168.1.4:8443", Enabled: true},
			wantSNI: "proxy_ssl_name panel2.zizdog.com;",
		},
		{
			name:    "显式 TLSName 优先",
			rule:    Rule{Name: "c", Listen: 8889, Domains: "panel2.zizdog.com", Target: "https://192.168.1.4:8443", TLSName: "explicit.example.com", Enabled: true},
			wantSNI: "proxy_ssl_name explicit.example.com;",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := c.rule.Generate("/tmp/logs")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "proxy_ssl_server_name on;") {
				t.Fatalf("HTTPS 上游缺少 proxy_ssl_server_name：\n%s", out)
			}
			if !strings.Contains(out, c.wantSNI) {
				t.Fatalf("SNI 期望 %q，实际输出：\n%s", c.wantSNI, out)
			}
		})
	}
}

// TestHTTPUpstreamHasNoSSLLines：http 上游不得出现 proxy_ssl_*（也别动老输出）。
func TestHTTPUpstreamHasNoSSLLines(t *testing.T) {
	r := Rule{Name: "h", Listen: 8889, Domains: "site2.zizdog.com", Target: "http://192.168.1.8:8081", Enabled: true}
	out, err := r.Generate("/tmp/logs")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "proxy_ssl_") {
		t.Fatalf("http 上游不该有 proxy_ssl_*：\n%s", out)
	}
}

// TestStandardHeadersPreset：一键常用请求头只在开关打开时输出（默认关，老规则输出不变）。
func TestStandardHeadersPreset(t *testing.T) {
	base := Rule{Name: "s", Listen: 8889, Domains: "blog.zizdog.com", Target: "http://127.0.0.1:80", Enabled: true}

	off, err := base.Generate("/tmp/logs")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"X-Forwarded-Host", "X-Forwarded-Port", "REMOTE-HOST"} {
		if strings.Contains(off, want) {
			t.Fatalf("开关关闭时不该输出 %s：\n%s", want, off)
		}
	}
	if !strings.Contains(off, "X-Forwarded-For") || !strings.Contains(off, "X-Forwarded-Proto") {
		t.Fatalf("面板默认请求头不应受影响：\n%s", off)
	}

	base.StandardHeaders = true
	on, err := base.Generate("/tmp/logs")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"X-Forwarded-Host", "X-Forwarded-Port $server_port;", "REMOTE-HOST"} {
		if !strings.Contains(on, want) {
			t.Fatalf("开关打开时应输出 %s：\n%s", want, on)
		}
	}
}

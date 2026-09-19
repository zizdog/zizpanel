package sites

import (
	"strings"
	"testing"
)

// 站点自定义监听端口：范围校验 + 生成物 + 老站点默认 80。

func TestValidateListenPortRange(t *testing.T) {
	for _, ok := range []int{1, 80, 1024, 8090, 65535} {
		if err := ValidateListenPort(ok); err != nil {
			t.Errorf("%d 应合法: %v", ok, err)
		}
	}
	for _, bad := range []int{0, -1, -80, 65536, 100000} {
		err := ValidateListenPort(bad)
		if err == nil {
			t.Errorf("%d 应被拒绝", bad)
			continue
		}
		if !strings.Contains(err.Error(), "1-65535") {
			t.Errorf("%d 的错误要写清范围，实际: %v", bad, err)
		}
	}
}

func TestEffectiveListenPortDefaultsTo80(t *testing.T) {
	if got := (&Site{}).EffectiveListenPort(); got != 80 {
		t.Errorf("未设置应按 80，实际 %d", got)
	}
	if got := (&Site{ListenPort: 0}).EffectiveListenPort(); got != 80 {
		t.Errorf("0 应按 80，实际 %d", got)
	}
	if got := (&Site{ListenPort: 70000}).EffectiveListenPort(); got != 80 {
		t.Errorf("越界值应按 80，实际 %d", got)
	}
	if got := (&Site{ListenPort: 8090}).EffectiveListenPort(); got != 8090 {
		t.Errorf("应返回 8090，实际 %d", got)
	}
}

func TestGenerateListenPort(t *testing.T) {
	// 默认（未设置）= 80，与加端口字段之前逐字一致
	def, err := (&Site{Domain: "p80.test", Root: "/tmp/p80.test", Rewrite: "none", Enabled: true}).
		Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "listen      80;") {
		t.Fatalf("默认站点必须监听 80：\n%s", def)
	}
	// 自定义端口
	custom, err := (&Site{Domain: "p8090.test", Root: "/tmp/p8090.test", Rewrite: "none",
		ListenPort: 8090, Enabled: true}).Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(custom, "listen      8090;") {
		t.Fatalf("自定义端口必须写进 vhost：\n%s", custom)
	}
	if strings.Contains(custom, "listen      80;") {
		t.Fatalf("自定义端口生效时不该还留着 80：\n%s", custom)
	}
}

// 自定义端口 + SSL：HTTPS 仍固定 443，所以跳转不能带原端口（否则跳到没人监听的端口）。
func TestGenerateListenPortWithSSLRedirectsWithoutPort(t *testing.T) {
	base := &Site{Domain: "ssl8090.test", Root: "/tmp/ssl8090.test", Rewrite: "none",
		SSLEnabled: true, SSLCert: "/tmp/a.crt", SSLKey: "/tmp/a.key", Enabled: true}
	base.ListenPort = 80
	def, err := base.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "return 301 https://$http_host$request_uri;") {
		t.Fatalf("80 + SSL 必须保持原有 $http_host 跳转（向后兼容）：\n%s", def)
	}
	base.ListenPort = 8090
	custom, err := base.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(custom, "listen      8090;") || !strings.Contains(custom, "listen      443 ssl;") {
		t.Fatalf("自定义端口 + SSL 应同时有 8090 与 443：\n%s", custom)
	}
	if !strings.Contains(custom, "return 301 https://$host$request_uri;") {
		t.Fatalf("自定义端口 + SSL 的跳转必须丢掉端口（HTTPS 在 443）：\n%s", custom)
	}
	if strings.Contains(custom, "$http_host") {
		t.Fatalf("自定义端口不该用 $http_host（会把 8090 带进 https 跳转）：\n%s", custom)
	}
}

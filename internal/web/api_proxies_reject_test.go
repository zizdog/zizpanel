package web

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/proxies"
)

// ============================================================================
//  兜底拒绝块（proxy-reject-<port>.conf）的 listen 协议选项
//
//  现象（2026-09-18 生产 error.log）：
//    nginx: [warn] protocol options redefined for 0.0.0.0:8888
//          in /opt/homebrew/etc/nginx/vhosts/proxy-reject-8888.conf:3
//
//  根因：nginx 对同一个 0.0.0.0:<port> 上重复出现的 listen 只允许
//  "选项集完全一致"，或"一个完整集 + 它的子集只出现一次"。兜底块字典序排在
//  proxy-<id>.conf 之后，它自己写不写 `ssl` 决定了会不会凑出第二种选项集：
//    · 全 ssl 端口上兜底块不写 ssl → 警告（"选项被移除"）；
//    · 混合端口（有的 ssl、有的不带）上兜底块写 ssl → 警告（第三种选项集）。
//
//  所以生成时必须让兜底块**跟端口上已有的 listen 选项一致**（proxyPortListenMode）。
// ============================================================================

// TestProxyPortListenMode：从 vhosts 目录判断端口上 listen 选项是否一致。
func TestProxyPortListenMode(t *testing.T) {
	sslBlock := "server {\n  listen 18380 ssl;\n  server_name a.test;\n}\n"
	plainBlock := "server {\n  listen 18380;\n  server_name b.test;\n}\n"

	// 每个子用例一个独立沙箱：vhosts 目录互不影响。
	run := func(t *testing.T, files map[string]string) (bool, bool) {
		t.Helper()
		srv := newProxyTestServer(t)
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(srv.Cfg.VhostDir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return srv.proxyPortListenMode(18380, proxies.RejectVhostName(18380))
	}

	t.Run("没有邻居", func(t *testing.T) {
		hasSSL, uniform := run(t, nil)
		if hasSSL || uniform {
			t.Fatalf("没有其它 vhost 时应当是 hasSSL=false uniform=false，实际 %v/%v", hasSSL, uniform)
		}
	})

	t.Run("全 ssl", func(t *testing.T) {
		hasSSL, uniform := run(t, map[string]string{
			"proxy-1.conf": sslBlock,
			"proxy-2.conf": strings.ReplaceAll(sslBlock, "a.test", "c.test"),
		})
		if !hasSSL || !uniform {
			t.Fatalf("全 ssl 端口应当 hasSSL=true uniform=true，实际 %v/%v", hasSSL, uniform)
		}
	})

	t.Run("混合", func(t *testing.T) {
		hasSSL, uniform := run(t, map[string]string{
			"proxy-1.conf": sslBlock,
			"0-site.conf":  plainBlock,
		})
		if !hasSSL || uniform {
			t.Fatalf("混合端口应当 hasSSL=true uniform=false，实际 %v/%v", hasSSL, uniform)
		}
	})

	t.Run("兜底块自己不算邻居", func(t *testing.T) {
		// 上一次写入的兜底块（ssl）必须被跳过：否则它会把自己算成"全 ssl"。
		hasSSL, uniform := run(t, map[string]string{
			proxies.RejectVhostName(18380) + ".conf": sslBlock,
		})
		if hasSSL || uniform {
			t.Fatalf("应当跳过兜底块自己，实际 %v/%v", hasSSL, uniform)
		}
	})

	t.Run("别的端口不影响", func(t *testing.T) {
		hasSSL, uniform := run(t, map[string]string{
			"other.conf": strings.ReplaceAll(sslBlock, "18380", "18381"),
		})
		if hasSSL || uniform {
			t.Fatalf("别的端口的 ssl 不该影响本端口，实际 %v/%v", hasSSL, uniform)
		}
	})
}

// TestProxyRejectBlockContentMatchesPort：兜底块生成点（proxyRejectBlockContent）
// 必须让 listen 选项与端口上已有 vhost 一致，并据此决定要不要证书行。
func TestProxyRejectBlockContentMatchesPort(t *testing.T) {
	const cert = "/tmp/zp-test-fullchain.pem"
	const key = "/tmp/zp-test-privkey.pem"

	sslPeer := "server {\n  listen %d ssl;\n  server_name %s;\n  ssl_certificate " + cert + ";\n  ssl_certificate_key " + key + ";\n}\n"
	plainCertPeer := "server {\n  listen %d;\n  server_name %s;\n  ssl_certificate " + cert + ";\n  ssl_certificate_key " + key + ";\n}\n"
	plainPeer := "server {\n  listen %d;\n  server_name %s;\n}\n"

	cases := []struct {
		name       string
		port       int
		ruleSSL    bool
		peers      map[string]string
		wantListen string
		wantCert   bool
	}{
		{
			name: "全 ssl 端口 → 兜底块必须带 ssl",
			port: 18390, ruleSSL: true,
			peers: map[string]string{
				"proxy-1.conf": fmt.Sprintf(sslPeer, 18390, "a.test"),
				"proxy-2.conf": fmt.Sprintf(sslPeer, 18390, "b.test"),
			},
			wantListen: "listen      18390 ssl default_server;",
			wantCert:   true,
		},
		{
			name: "混合端口 → 兜底块不能带 ssl",
			port: 18391, ruleSSL: true,
			peers: map[string]string{
				"0-site.conf":  fmt.Sprintf(plainCertPeer, 18391, "b.test"),
				"proxy-1.conf": fmt.Sprintf(sslPeer, 18391, "a.test"),
			},
			wantListen: "listen      18391 default_server;",
			wantCert:   true,
		},
		{
			name: "全不带 ssl 的端口 → 兜底块也不带证书",
			port: 18392, ruleSSL: false,
			peers: map[string]string{
				"proxy-1.conf": fmt.Sprintf(plainPeer, 18392, "a.test"),
			},
			wantListen: "listen      18392 default_server;",
			wantCert:   false,
		},
		{
			name: "端口上只有 ssl 规则、还没有别的 vhost → 兜底块带 ssl",
			port: 18393, ruleSSL: true,
			wantListen: "listen      18393 ssl default_server;",
			wantCert:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newProxyTestServer(t)
			for name, content := range c.peers {
				if err := os.WriteFile(filepath.Join(srv.Cfg.VhostDir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rule := &proxies.Rule{
				Name: "规则", Listen: c.port, Domains: "z.test", Target: "http://127.0.0.1:9",
				Enabled: true,
			}
			if c.ruleSSL {
				rule.SSLEnabled = true
				rule.SSLCert, rule.SSLKey = cert, key
			}
			sp := proxyRejectSpecs([]*proxies.Rule{rule})[c.port]
			got, err := srv.proxyRejectBlockContent(c.port, sp)
			if err != nil {
				t.Fatalf("生成兜底块失败：%v", err)
			}
			if !strings.Contains(got, c.wantListen) {
				t.Errorf("兜底块的 listen 与端口不一致，期望 %q：\n%s", c.wantListen, got)
			}
			if c.wantCert && !strings.Contains(got, "ssl_certificate     "+cert+";") {
				t.Errorf("端口上有 ssl 时兜底块必须带证书行：\n%s", got)
			}
			if !c.wantCert && strings.Contains(got, "ssl_certificate") {
				t.Errorf("端口上没有 ssl 时兜底块不该带证书行：\n%s", got)
			}
		})
	}
}

// TestSyncRejectBlocksUsesPortMatchedContent：syncRejectBlocks 必须走上面那个
// 生成点（而不是自己另写一份 listen），否则修复就会在真实写盘路径上被绕过。
func TestSyncRejectBlocksUsesPortMatchedContent(t *testing.T) {
	src, err := os.ReadFile("api_proxies.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	if !strings.Contains(code, "s.proxyRejectBlockContent(port, sp)") {
		t.Error("syncRejectBlocks 必须调用 proxyRejectBlockContent 生成兜底块（唯一的 listen 决策点）")
	}
}

// TestProxyRejectListenOptionsWithRealNginx 用真实 nginx -t 证明上面的选择是对的：
//   - 全 ssl 端口：带 ssl 的兜底块无警告；去掉 ssl 就会打
//     `protocol options redefined`（反例，证明用例是灵敏的）；
//   - 混合端口：不带 ssl 的兜底块无警告；带上 ssl 就会打警告。
//
// 沙箱化：临时 prefix + 随机高端口 + 自己的 vhosts 目录，
// 绝不碰生产的 /opt/homebrew/etc/nginx 与 80/443。
func TestProxyRejectListenOptionsWithRealNginx(t *testing.T) {
	nginxBin := "/opt/homebrew/bin/nginx"
	if _, err := os.Stat(nginxBin); err != nil {
		t.Skip("未安装 nginx，跳过 listen 协议选项的真实校验")
	}
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	vhostDir := filepath.Join(dir, "vhosts")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "logs")
	bodyDir := filepath.Join(dir, "body")
	for _, d := range []string{confDir, vhostDir, runDir, logDir, bodyDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	makeSeedCert(t, dir, []string{"a.test"})
	certPath := filepath.Join(dir, "fullchain.pem")
	keyPath := filepath.Join(dir, "privkey.pem")
	port := freePortForTest(t)

	main := fmt.Sprintf(`worker_processes 1;
error_log %s/error.log warn;
pid %s/nginx.pid;
events { worker_connections 32; }
http {
    include /opt/homebrew/etc/nginx/mime.types;
    default_type application/octet-stream;
    access_log off;
    client_body_temp_path %s;
    proxy_temp_path %s/proxy;
    fastcgi_temp_path %s/fastcgi;
    uwsgi_temp_path %s/uwsgi;
    scgi_temp_path %s/scgi;
    include %s/*.conf;
}
`, logDir, runDir, bodyDir, runDir, runDir, runDir, runDir, vhostDir)
	mainPath := filepath.Join(confDir, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	sslBlock := func(name string) string {
		return fmt.Sprintf("server {\n  listen %d ssl;\n  server_name %s;\n  ssl_certificate %s;\n  ssl_certificate_key %s;\n  location / { return 200 \"ok\"; }\n}\n",
			port, name, certPath, keyPath)
	}
	plainWithCertBlock := func(name string) string {
		return fmt.Sprintf("server {\n  listen %d;\n  server_name %s;\n  ssl_certificate %s;\n  ssl_certificate_key %s;\n  location / { return 200 \"ok\"; }\n}\n",
			port, name, certPath, keyPath)
	}
	rejectName := proxies.RejectVhostName(port) + ".conf"
	writeVhost := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(vhostDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	clearVhosts := func() {
		t.Helper()
		entries, err := os.ReadDir(vhostDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			_ = os.Remove(filepath.Join(vhostDir, e.Name()))
		}
	}
	nginxTest := func() string {
		t.Helper()
		out, _ := exec.Command(nginxBin, "-c", mainPath, "-t").CombinedOutput()
		return string(out)
	}

	t.Run("全 ssl 端口", func(t *testing.T) {
		clearVhosts()
		writeVhost("proxy-1.conf", sslBlock("a.test"))
		writeVhost("proxy-2.conf", sslBlock("b.test"))
		writeVhost(rejectName, proxies.GenerateRejectWithCert(port, "", certPath, keyPath, true))
		if out := nginxTest(); strings.Contains(out, "protocol options redefined") {
			t.Errorf("带 ssl 的兜底块在全 ssl 端口上不该有警告：\n%s", out)
		}
		// 反例：不带 ssl 的兜底块（旧的"中性形态"若残留）本应触发警告。
		writeVhost(rejectName, proxies.GenerateRejectWithCert(port, "", certPath, keyPath, false))
		if out := nginxTest(); !strings.Contains(out, "protocol options redefined") {
			t.Errorf("反例失效：全 ssl 端口上不带 ssl 的兜底块本应触发警告：\n%s", out)
		}
	})

	t.Run("混合端口", func(t *testing.T) {
		clearVhosts()
		// 字典序保证加载顺序：0-site（不带 ssl，但带证书行）→ proxy-1（ssl）→ 兜底块。
		writeVhost("0-site.conf", plainWithCertBlock("b.test"))
		writeVhost("proxy-1.conf", sslBlock("a.test"))
		writeVhost(rejectName, proxies.GenerateRejectWithCert(port, "", certPath, keyPath, false))
		if out := nginxTest(); strings.Contains(out, "protocol options redefined") {
			t.Errorf("不带 ssl 的兜底块在混合端口上不该有警告：\n%s", out)
		}
		// 反例：带 ssl 的兜底块就是生产 error.log 里那条警告。
		writeVhost(rejectName, proxies.GenerateRejectWithCert(port, "", certPath, keyPath, true))
		out := nginxTest()
		if !strings.Contains(out, "protocol options redefined") {
			t.Errorf("反例失效：混合端口上带 ssl 的兜底块本应触发警告：\n%s", out)
		}
		if !strings.Contains(out, rejectName) {
			t.Errorf("警告应当指向兜底块 %s：\n%s", rejectName, out)
		}
	})
}

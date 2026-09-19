package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteVhostHealsNginxEnvAndRetries 锁的是用户报障的**根修**：
//
//	"反向代理用不了了！规则已保存但 nginx 配置应用失败：配置语法错误，已回滚：
//	 nginx: [emerg] unknown "connection_upgrade" variable"
//
// 根因是面板自己的 nginx 片段没被加载（brew 重装/升级 nginx 会把 nginx.conf 还原成
// 出厂版，`include conf.d/*.conf` 与 upgrade map 一起消失）。面板本来就会修这件事
// （EnsureNginxEnv），但过去只在启动时修一次 —— 用户只看到一句"已回滚"。
//
// 这条测试用**假助手**复现"第一次写入被 nginx -t 拒掉"，断言面板会：
//
//	① 先补齐 nginx 环境；② 自动重试一次；③ 最终返回成功（而不是把重装 nginx 丢给用户）。
func TestWriteVhostHealsNginxEnvAndRetries(t *testing.T) {
	srv, _ := newTestServer(t)
	t.Setenv("ZIZPANEL_BREW_PREFIX", srv.Cfg.BrewPrefix)

	// 造一个最小的 nginx.conf，让 EnsureNginxEnv 有东西可读可改（沙箱路径）
	confDir := filepath.Join(srv.Cfg.BrewPrefix, "etc", "nginx")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(confDir, "nginx.conf")
	if err := os.WriteFile(conf, []byte("events {}\nhttp {\n    include mime.types;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 假 nginx：priv 在写入 include 后会跑一次 `nginx -t`，沙箱里没有真 nginx ——
	// 给一个永远通过的桩，否则自愈会因为"校验失败"而回滚（那是诚实行为，不是 bug）。
	nginxBin := filepath.Join(srv.Cfg.BrewPrefix, "bin", "nginx")
	if err := os.MkdirAll(filepath.Dir(nginxBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nginxBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 假助手：第一次 vhost-write 返回"变量未定义"，之后成功；调用次数记进计数文件。
	bin := srv.Cfg.ServicePath("zizpanel-helper")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(t.TempDir(), "calls.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + countFile + "\n" +
		"case \"$1\" in\n" +
		"  vhost-write)\n" +
		"    n=$(grep -c '^vhost-write' " + countFile + " 2>/dev/null || echo 0)\n" +
		"    if [ \"$n\" -le 1 ]; then\n" +
		"      printf '%s' '{\"ok\":false,\"error\":\"配置语法错误，已回滚：\\nnginx: [emerg] unknown \\\"connection_upgrade\\\" variable\"}'\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    printf '%s' '{\"ok\":true}'\n" +
		"    ;;\n" +
		"  *) printf '%s' '{\"ok\":true,\"data\":{}}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 } // 以 root 运行：直接执行助手、不套 sudo
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	err := srv.writeVhost(context.Background(), "proxy.test", "server { listen 80; }\n")
	if err != nil {
		t.Fatalf("第一次被 nginx -t 拒掉后，面板应当自愈并重试成功，实际报错：%v", err)
	}

	// 必须真的重试过（≥2 次 vhost-write 调用），而不只是"看起来成功"
	calls, _ := os.ReadFile(countFile)
	if n := strings.Count(string(calls), "vhost-write"); n < 2 {
		t.Errorf("应当自动重试写入（至少 2 次 vhost-write 调用），实际 %d 次：%s", n, calls)
	}
	// 自愈必须真的补齐 include（这是 $connection_upgrade 能被定义的唯一前提）
	after, _ := os.ReadFile(conf)
	if !strings.Contains(string(after), "include") || !strings.Contains(string(after), "conf.d") {
		t.Errorf("自愈后 nginx.conf 必须 include conf.d/*.conf（否则 map 永远不生效）：\n%s", after)
	}
}

// TestIsNginxEnvVarErr 是上面那条重试的判据门禁：只认"面板自己能修"的两类变量，
// 别的语法错误（比如真的写错了指令）不许被吞掉重试 —— 那会把真错误掩盖成"重试也没用"。
func TestIsNginxEnvVarErr(t *testing.T) {
	yes := []string{
		`配置语法错误，已回滚：nginx: [emerg] unknown "connection_upgrade" variable`,
		`nginx: [emerg] unknown "zp_scheme" variable in /x/y.conf:12`,
		`nginx: [emerg] unknown "zp_https" variable`,
	}
	for _, s := range yes {
		if !isNginxEnvVarErr(errors.New(s)) {
			t.Errorf("应当判定为「面板环境缺失」：%s", s)
		}
	}
	no := []string{
		`nginx: [emerg] unknown directive "whatever" in /x/y.conf:3`,
		`nginx: [emerg] a duplicate default server for 0.0.0.0:80`,
		`写入配置失败: 提权助手不存在`,
		``,
	}
	for _, s := range no {
		if isNginxEnvVarErr(errors.New(s)) {
			t.Errorf("不该被当成「面板环境缺失」：%s", s)
		}
	}
	if isNginxEnvVarErr(nil) {
		t.Error("nil 错误不该判定为真")
	}
}

// TestNginxEnsureEnvReportsHonestlyWhenHelperMissing 锁"修复 nginx 环境"接口的诚实性：
// 助手不可用时**不许**报成功，必须把 reason 原样带出来（用户据此知道不是"修好了"）。
func TestNginxEnsureEnvReportsHonestlyWhenHelperMissing(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	t.Setenv("ZIZPANEL_BREW_PREFIX", srv.Cfg.BrewPrefix)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/system/nginx/ensure-env", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("接口应当返回 200（把真实结果写在 body 里），实际 %d：%v", res.StatusCode, body)
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应缺少 data：%v", body)
	}
	if data["nginx_test_ok"] == true {
		t.Error("沙箱里没有可用的提权助手，绝不能报 nginx 校验通过")
	}
	if msg, _ := data["nginx_test"].(string); msg == "" {
		t.Error("校验失败时必须如实给出原因（空字符串等于什么都没说）")
	}
}

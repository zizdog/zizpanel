package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/sites"
)

// 本文件的测试全部跑在 newTestServer 造的沙箱里：
// BrewPrefix 指向临时目录（见 server_test.go 里的 SetBrewPrefixDetectorForTest），
// 所以这里可以放心地改写"www.conf"而绝不会落到真实的 /opt/homebrew 上。

// loginTestPanel 建好面板账号并返回会话 Cookie。
func loginTestPanel(t *testing.T, ts *httptest.Server) []*http.Cookie {
	t.Helper()
	res, out, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("初始化面板失败 %d: %v", res.StatusCode, out)
	}
	return cookies
}

// apiData 取出成功响应里的 data 对象（面板统一用 {ok,data} 包装）。
func apiData(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	d, _ := out["data"].(map[string]any)
	if d == nil {
		t.Fatalf("响应缺少 data: %v", out)
	}
	return d
}

// seedFakePHP 在沙箱 brew 前缀里造出若干个已安装的 PHP 版本。
func seedFakePHP(t *testing.T, brewPrefix string, versions ...string) {
	t.Helper()
	for _, v := range versions {
		if err := os.MkdirAll(filepath.Join(brewPrefix, "opt", "php@"+v, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		// bin/php 必须真的存在：sites.DiscoverPHPVersions 现在按"运行体"判定
		// （opt/<name> 能 stat 且 bin/php 在），否则 Homebrew 卸载后留下的
		// 悬空软链接会被当成"已安装"。夹具少了这个文件，就会测出 0 个版本。
		if err := os.WriteFile(filepath.Join(brewPrefix, "opt", "php@"+v, "bin", "php"),
			[]byte("#!/bin/sh\necho 'PHP "+v+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		confDir := filepath.Join(brewPrefix, "etc", "php", v, "php-fpm.d")
		if err := os.MkdirAll(confDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// 出厂配置：所有版本都写 9000（真机实测就是这样）
		body := "[global]\npid = run/php-fpm.pid\n\n[www]\nuser = tester\ngroup = staff\n" +
			";listen.owner = _www\nlisten = 127.0.0.1:9000\npm = dynamic\n"
		if err := os.WriteFile(filepath.Join(confDir, "www.conf"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// nginx.conf：让 EnsureListen 能推导出 socket 属主
	if err := os.MkdirAll(filepath.Join(brewPrefix, "etc", "nginx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brewPrefix, "etc", "nginx", "nginx.conf"),
		[]byte("user  tester staff;\nworker_processes auto;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPHPListAPI(t *testing.T) {
	srv, ts := newTestServer(t)
	seedFakePHP(t, srv.Cfg.BrewPrefix, "8.2", "8.3")

	cookies := loginTestPanel(t, ts)
	res, body, _ := doJSON(t, ts, "GET", "/api/v1/php", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/php 返回 %d: %v", res.StatusCode, body)
	}
	data := apiData(t, body)

	list, _ := data["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("应列出 2 个已安装版本，实际 %d 个: %v", len(list), body)
	}
	seen := map[string]map[string]any{}
	for _, it := range list {
		m, _ := it.(map[string]any)
		ver, _ := m["version"].(string)
		seen[ver] = m
	}
	for _, ver := range []string{"8.2", "8.3"} {
		m, okk := seen[ver]
		if !okk {
			t.Fatalf("缺少版本 %s：%v", ver, body)
		}
		// 关键：不能因为两个版本都配着 9000 就少列一个（旧实现会按端点去重掉一个）
		if m["pass"] != "127.0.0.1:9000" {
			t.Fatalf("PHP %s 的当前端点应为 9000，实际 %v", ver, m["pass"])
		}
		pref, _ := m["preferred_pass"].(string)
		if !strings.Contains(pref, "php-fpm-"+ver+".sock") {
			t.Fatalf("PHP %s 的分配端点应为含版本号的 socket，实际 %q", ver, pref)
		}
		if m["listen_ok"] != false {
			t.Fatalf("PHP %s 还没配置过，listen_ok 应为 false", ver)
		}
	}
	// 两个版本共用一个端点，必须被显式标记出来
	if seen["8.2"]["conflict"] == nil || seen["8.3"]["conflict"] == nil {
		t.Fatalf("两个版本共用 9000 时应标记 conflict：%v", body)
	}
}

// 修复端点（只写配置、不重启）：
//   - 幂等：第一次 changed=true，第二次 changed=false
//   - 端点按版本唯一
//   - 沙箱里的 www.conf 确实被改写
func TestPHPFixListenAPIWritesConfigIdempotently(t *testing.T) {
	srv, ts := newTestServer(t)
	seedFakePHP(t, srv.Cfg.BrewPrefix, "8.2", "8.3")
	cookies := loginTestPanel(t, ts)

	conf82 := sites.FPMConfPath(srv.Cfg.BrewPrefix, "8.2")
	conf83 := sites.FPMConfPath(srv.Cfg.BrewPrefix, "8.3")

	_, body, _ := doJSON(t, ts, "POST", "/api/v1/php/fix-listen",
		map[string]any{"version": "8.2", "restart": false}, cookies)
	res82, _ := apiData(t, body)["result"].(map[string]any)
	if res82 == nil {
		t.Fatalf("修复 8.2 失败：%v", body)
	}
	if res82["changed"] != true {
		t.Fatalf("首次修复应报告 changed=true：%v", res82)
	}
	ep82, _ := res82["endpoint"].(string)
	if !strings.Contains(ep82, "php-fpm-8.2.sock") {
		t.Fatalf("8.2 的端点不对：%q", ep82)
	}

	_, body3, _ := doJSON(t, ts, "POST", "/api/v1/php/fix-listen",
		map[string]any{"version": "8.3", "restart": false}, cookies)
	res83, _ := apiData(t, body3)["result"].(map[string]any)
	if res83 == nil {
		t.Fatalf("修复 8.3 失败：%v", body3)
	}
	ep83, _ := res83["endpoint"].(string)
	if ep82 == ep83 {
		t.Fatalf("两个版本修完仍然是同一个端点 %q", ep82)
	}

	// 沙箱里的真实文件必须已被改写，且两个文件内容不同
	b82, err := os.ReadFile(conf82)
	if err != nil {
		t.Fatal(err)
	}
	b83, err := os.ReadFile(conf83)
	if err != nil {
		t.Fatal(err)
	}
	// 注意两种形态的区别（真机 2026-09-17 踩过）：
	//   · 接口/nginx 侧用规范形式 `unix:/path`（fastcgi_pass 必须带前缀）
	//   · **php-fpm 的 listen 只认裸路径** —— 写 `unix:/path` 会让 fpm 启动时报
	//     `invalid port value`，所以文件里必须是不带前缀的那一个。
	if !strings.Contains(string(b82), "listen = "+strings.TrimPrefix(ep82, "unix:")) {
		t.Fatalf("8.2 的 www.conf 未被改写（php-fpm 要裸路径）:\n%s", b82)
	}
	if !strings.Contains(string(b83), "listen = "+strings.TrimPrefix(ep83, "unix:")) {
		t.Fatalf("8.3 的 www.conf 未被改写（php-fpm 要裸路径）:\n%s", b83)
	}
	if strings.Contains(string(b82), "listen = unix:") || strings.Contains(string(b83), "listen = unix:") {
		t.Fatal("php-fpm 的 listen 不能带 unix: 前缀（fpm 会以 invalid port value 启动失败）")
	}
	if string(b82) == string(b83) {
		t.Fatal("两个版本的 www.conf 内容相同：多版本共存没有真正落实")
	}
	// Unix socket 时顺带写入了属主（nginx.conf 里的 tester staff）
	if !strings.Contains(string(b82), "listen.owner = tester") {
		t.Fatalf("应写入 socket 属主以便 nginx 连接:\n%s", b82)
	}

	// 幂等：再来一次不应再改
	_, bodyAgain, _ := doJSON(t, ts, "POST", "/api/v1/php/fix-listen",
		map[string]any{"version": "8.2", "restart": false}, cookies)
	resAgain, _ := apiData(t, bodyAgain)["result"].(map[string]any)
	if resAgain == nil {
		t.Fatalf("二次修复失败：%v", bodyAgain)
	}
	if resAgain["changed"] != false {
		t.Fatalf("二次修复应幂等（changed=false）：%v", resAgain)
	}
	b82Again, _ := os.ReadFile(conf82)
	if string(b82Again) != string(b82) {
		t.Fatal("幂等性被破坏：文件内容变了")
	}
}

// 非法版本号必须被挡住，不能拼出越界路径去读写。
func TestPHPFixListenRejectsBadVersion(t *testing.T) {
	srv, ts := newTestServer(t)
	seedFakePHP(t, srv.Cfg.BrewPrefix, "8.3")
	cookies := loginTestPanel(t, ts)

	for _, bad := range []string{"", "../../etc", "8.3/../../x", "php@8.3", "abc"} {
		resp, body, _ := doJSON(t, ts, "POST", "/api/v1/php/fix-listen",
			map[string]any{"version": bad, "restart": false}, cookies)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("非法版本 %q 应被拒绝，却成功了：%v", bad, body)
		}
	}
}

// 未安装的版本要给出可读报错（含版本号与文件路径）。
func TestPHPFixListenUnknownVersion(t *testing.T) {
	srv, ts := newTestServer(t)
	seedFakePHP(t, srv.Cfg.BrewPrefix, "8.3")
	cookies := loginTestPanel(t, ts)

	resp, body, _ := doJSON(t, ts, "POST", "/api/v1/php/fix-listen",
		map[string]any{"version": "8.1", "restart": false}, cookies)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("未安装版本不应成功：%v", body)
	}
	msg, _ := body["msg"].(string)
	if !strings.Contains(msg, "8.1") || !strings.Contains(msg, "www.conf") {
		t.Fatalf("报错应包含版本号与文件路径，实际: %s", msg)
	}
}

// 站点接口必须把已安装版本一并给前端（界面用它渲染下拉框），
// 而且不能再因为"两个版本都配 9000"而少列一个。
func TestSiteListExposesAllInstalledPHPVersions(t *testing.T) {
	srv, ts := newTestServer(t)
	seedFakePHP(t, srv.Cfg.BrewPrefix, "8.2", "8.3")
	cookies := loginTestPanel(t, ts)

	_, body, _ := doJSON(t, ts, "GET", "/api/v1/sites", nil, cookies)
	list, _ := apiData(t, body)["php_versions"].([]any)
	if len(list) != 2 {
		t.Fatalf("站点列表应带出 2 个 PHP 版本，实际 %d: %v", len(list), body)
	}
}

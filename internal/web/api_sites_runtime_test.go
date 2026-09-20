package web

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  "网站环境：未就绪（未检测到运行中的：nginx）"—— 2026-09-18 用户报障
//
//  用户原话："我网站运行地好好的！" 根因：那一行只看面板的**服务记录**，
//  而 nginx 完全可以"装着、在跑、面板里没有记录"（用户自己装的/换机后记录丢了）。
//
//  这组测试锁住两条：
//    ① **在跑就必须显示在跑**（哪怕面板里没有它的记录）—— 判据来自运行体；
//    ② "没记录"要单独说成"不归面板管"，不是"未就绪"。
// ============================================================================

// stubNginxStatus 替换 nginx 运行体探测。
func stubNginxStatus(t *testing.T, status string, err error) {
	t.Helper()
	prev := nginxStatusFn
	nginxStatusFn = func() (string, error) { return status, err }
	t.Cleanup(func() { nginxStatusFn = prev })
}

// TestSitesRuntimeNginxRunningWithoutRecord 是本 bug 的回归测试：
// 进程在跑、面板**没有**记录 → running=true、registered=false（而不是"未就绪"）。
func TestSitesRuntimeNginxRunningWithoutRecord(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	stubNginxStatus(t, "running", nil) // 复刻真机：nginx 在 :80 上服务
	_ = srv                            // 面板记录里**故意**不建 nginx 条目

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites/runtime", nil, cookies)
	data, _ := out["data"].(map[string]any)
	nginx, _ := data["nginx"].(map[string]any)
	if nginx == nil {
		t.Fatalf("runtime 接口没有返回 nginx：%v", out)
	}
	if nginx["running"] != true {
		t.Errorf("nginx 进程在跑就必须报 running=true（这正是用户报的'我网站运行地好好的'）：%v", nginx)
	}
	if nginx["registered"] == true {
		t.Errorf("面板里没有 nginx 记录时应报 registered=false（归不归面板管是另一个问题）：%v", nginx)
	}
	if !strings.Contains(asString(nginx["evidence"]), "进程") {
		t.Errorf("证据要写清凭什么说它在跑，实际 %q", asString(nginx["evidence"]))
	}
}

// TestSitesRuntimeNginxStopped 真的没进程时如实说没跑。
func TestSitesRuntimeNginxStopped(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	stubNginxStatus(t, "stopped", nil)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites/runtime", nil, cookies)
	data, _ := out["data"].(map[string]any)
	nginx, _ := data["nginx"].(map[string]any)
	if nginx["running"] == true {
		t.Errorf("探测说没有 nginx 进程时不能报 running=true：%v", nginx)
	}
}

// TestSitesRuntimeProbeErrorNotReportedAsStopped：探测**失败**不等于"没在跑"。
//
// 界面上这两种情况必须区分（前者是"状态未知"，后者才是"未就绪"）——
// 否则探测一抖动，用户就会看到"你的环境没就绪"。
func TestSitesRuntimeProbeErrorNotReportedAsStopped(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	stubNginxStatus(t, "", fmt.Errorf("pgrep 执行失败"))

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites/runtime", nil, cookies)
	data, _ := out["data"].(map[string]any)
	nginx, _ := data["nginx"].(map[string]any)
	if nginx["running"] == true {
		t.Errorf("探测失败时不能报 running=true（没证据）：%v", nginx)
	}
	if asString(nginx["probe_error"]) == "" {
		t.Errorf("探测失败必须带 probe_error（界面据此显示'未知'而不是'未就绪'）：%v", nginx)
	}
}

// TestSitesRuntimeMySQLProbeUsesRealPort：MySQL 的判据是**端口真的能连**。
//
// 用一个真实监听器冒充数据库：连得上就必须报 running=true（不看服务记录）。
func TestSitesRuntimeMySQLProbeUsesRealPort(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	srv.Cfg.MySQLHost = "127.0.0.1"
	srv.Cfg.MySQLPort = port

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites/runtime", nil, cookies)
	data, _ := out["data"].(map[string]any)
	mysql, _ := data["mysql"].(map[string]any)
	if mysql["running"] != true {
		t.Errorf("端口能连上就算数据库在服务（面板没有它的记录也不影响）：%v", mysql)
	}
	if !strings.Contains(asString(mysql["evidence"]), fmt.Sprint(port)) {
		t.Errorf("证据里要写出连的是哪个地址，实际 %q", asString(mysql["evidence"]))
	}
}

// TestServiceRecordMatchingIsOnlyAboutRegistration 锁住"记录匹配只影响 registered"。
func TestServiceRecordMatchingIsOnlyAboutRegistration(t *testing.T) {
	recs := []*services.View{{Service: &services.Service{Name: "homebrew.mxcl.nginx"}}}
	if reg, st := recordFor(recs, "nginx"); !reg || st == "" {
		t.Errorf("名字里含 nginx 的记录应当被认出来（registered=true，状态 unknown）：%v %v", reg, st)
	}
	var _ = http.StatusOK
}

// listenLocalPort 起一个只接受连接的本地端口，返回端口号（测试里代替真实数据库）。
func listenLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestSitesRuntimeMySQLInstallPathComesFromEngineResolution：
// 安装路径必须来自**引擎解析结果**（MariaDB 的 opt/mariadb/bin），
// 不是写死的 <brew>/bin/mysqld（keg-only 的 mysql@8.4 根本不在那里）。
func TestSitesRuntimeMySQLInstallPathComesFromEngineResolution(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	srv.Cfg.MySQLHost, srv.Cfg.MySQLPort = "127.0.0.1", listenLocalPort(t)

	binDir := filepath.Join(srv.Cfg.BrewPrefix, "opt", "mariadb", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"mysql", "mysqld"} {
		if err := os.WriteFile(filepath.Join(binDir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites/runtime", nil, cookies)
	data, _ := out["data"].(map[string]any)
	mysql, _ := data["mysql"].(map[string]any)
	want := filepath.Join(binDir, "mysqld")
	if got := asString(mysql["install_path"]); got != want {
		t.Errorf("安装路径应来自引擎解析（%s），实际 %q", want, got)
	}
	if pe := strings.TrimSpace(asString(mysql["probe_error"])); pe != "" {
		t.Errorf("引擎解析成功时不该报「未复核」：%v", mysql)
	}
}

// TestSitesRuntimeMySQLUnverifiedWhenEngineUnreadable：
// 读不到引擎时**如实标未复核**，绝不回退到一个可能不存在的安装路径。
func TestSitesRuntimeMySQLUnverifiedWhenEngineUnreadable(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	srv.Cfg.MySQLHost, srv.Cfg.MySQLPort = "127.0.0.1", listenLocalPort(t)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/sites/runtime", nil, cookies)
	data, _ := out["data"].(map[string]any)
	mysql, _ := data["mysql"].(map[string]any)
	if got := asString(mysql["install_path"]); got != "" {
		t.Errorf("读不到引擎时不许编一个安装路径，实际 %q", got)
	}
	if pe := asString(mysql["probe_error"]); !strings.Contains(pe, "未复核") {
		t.Errorf("读不到引擎时必须如实说「未复核」，实际 probe_error=%q", pe)
	}
}

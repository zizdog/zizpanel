package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  残留记录清理的 HTTP 门禁（真机 2026-09-20）
//
//  现场：mysql@8.4 的 keg 与 plist 都卸了，services 表里只剩 sh-brew-mysql8-4
//  （port 3306），3306 上跑的是 MariaDB。真机上三条路全断：
//    · GET /market/mysql84/uninstall-plan → kind=forget + blocked（站点依赖）
//    · DELETE /market/mysql84?remove_data=1 → 400 同一句依赖拦截
//    · DELETE /services/sh-brew-mysql8-4 → 409「端口 3306 正在监听」（把 MariaDB 当成它）
//  现在：残留记录 ⇒ 计划是 forget、依赖不拦、两条删除通路都能只删面板记录，
//  并且**不会碰任何在跑的东西**；只有它自己真的还在时才拒绝。
// ============================================================================

// bindResidualTestManager 造一个完全离线的管理器：装着/端口占用/站点依赖全部注入。
func bindResidualTestManager(t *testing.T, srv *Server,
	installed map[string]string, holders []string, sites []services.SiteRef) *services.Manager {
	t.Helper()
	prefix := t.TempDir()
	brew := filepath.Join(prefix, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := services.NewManager(srv.serviceRepo, services.Options{
		BrewBin: brew, UserName: "", UserHome: filepath.Join(prefix, "home"),
		WorkDir: filepath.Join(prefix, "work"),
		SiteDependents: func() []services.SiteRef {
			return sites
		},
	})
	mgr.SetBrewUsesProbeForTest(func(context.Context, string) ([]string, bool) { return nil, true })
	mgr.SetLaunchdDirsForTest([]string{filepath.Join(prefix, "LaunchDaemons")})
	mgr.SetBrewInstalledProbeForTest(func(context.Context) (map[string]string, bool) {
		return installed, true
	})
	mgr.SetPortCheckProbeForTest(func(port int) (bool, []string, error) {
		if port == 3306 && len(holders) > 0 {
			return true, holders, nil
		}
		return false, nil, nil
	})
	srv.svcManagerOverride = func(*Server) *services.Manager { return mgr }
	return mgr
}

// seedStaleMySQLRecord 造出真机那条残留记录。
func seedStaleMySQLRecord(t *testing.T, srv *Server) {
	t.Helper()
	repo := services.NewRepository(srv.Store)
	if err := repo.Create(t.Context(), &services.Service{
		Name: "sh-brew-mysql8-4", DisplayName: "MySQL 8.4", Kind: services.KindNative,
		LaunchLabel: "sh.brew.mysql@8.4", Port: 3306, Managed: true,
	}); err != nil {
		t.Fatal(err)
	}
}

// recordGone 报告那条记录还在不在。
func recordGone(t *testing.T, srv *Server) bool {
	t.Helper()
	repo := services.NewRepository(srv.Store)
	list, err := repo.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.Name == "sh-brew-mysql8-4" {
			return false
		}
	}
	return true
}

// 门禁①：残留记录 + 3306 上跑着另一个引擎 + 站点依赖 → 两条删除通路都能删掉记录。
func TestMarketForgetResidualRecordSucceedsWhileOtherEngineRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		// 真机 UI 当时发的那条（remove_data=1）：残留 + 无 data_paths → 走 forget 语义。
		{"UI 的删除残留数据（remove_data=1）", "/api/v1/market/mysql84?remove_data=1"},
		{"显式 forget=1", "/api/v1/market/mysql84?forget=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := newTestServer(t)
			bindResidualTestManager(t, srv, map[string]string{"mariadb": "13.0.2"},
				[]string{"mariadbd (pid 35985)"},
				[]services.SiteRef{{Domain: "zizdog.cn", Enabled: true}, {Domain: "mirror.zizdog.com", Enabled: true}})
			_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
				map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
			seedStaleMySQLRecord(t, srv)

			// 计划：kind=forget、不被依赖拦（真机这里原本 blocked）。
			res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/mysql84/uninstall-plan", nil, cookies)
			if res.StatusCode != 200 {
				t.Fatalf("计划接口应 200，实际 %d：%v", res.StatusCode, out)
			}
			planData, _ := out["data"].(map[string]any)
			plan, _ := planData["uninstall"].(map[string]any)
			if asString(plan["kind"]) != "forget" {
				t.Errorf("残留记录的计划应为 forget，实际 %q", asString(plan["kind"]))
			}
			if asString(plan["blocked"]) != "" {
				t.Errorf("残留清理不该被站点依赖拦住，实际 blocked=%q", asString(plan["blocked"]))
			}
			if note := asString(plan["keep_note"]); !strings.Contains(note, "只删面板记录") {
				t.Errorf("计划要写清只删面板记录，实际 %q", note)
			}

			res2, out2, _ := doJSON(t, ts, "DELETE", tc.path, nil, cookies)
			if res2.StatusCode != http.StatusAccepted {
				t.Fatalf("残留清理应 202（开任务），实际 %d：%v", res2.StatusCode, out2)
			}
			data, _ := out2["data"].(map[string]any)
			status, errMsg, _ := waitTask(t, srv, asString(data["task_id"]))
			if status != "succeeded" {
				t.Fatalf("残留清理任务应成功，实际 %s（%s）", status, errMsg)
			}
			if !recordGone(t, srv) {
				t.Error("记录应当已被删除")
			}
		})
	}
}

// 门禁②：/services/{name} 也能删掉残留记录，并在文案里说清没有停任何东西。
func TestServiceDeleteRemovesResidualRecordWithHonestMessage(t *testing.T) {
	srv, ts := newTestServer(t)
	bindResidualTestManager(t, srv, map[string]string{"mariadb": "13.0.2"},
		[]string{"mariadbd (pid 35985)"}, nil)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedStaleMySQLRecord(t, srv)

	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/services/sh-brew-mysql8-4", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("残留记录应能只删记录，实际 %d：%v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["runtime_touched"] != false || data["stopped"] != false {
		t.Errorf("只删记录时必须如实报 runtime_touched=false / stopped=false，实际 %v", data)
	}
	msg := asString(data["msg"])
	for _, want := range []string{"只删掉了面板记录", "MariaDB", "不会被停"} {
		if !strings.Contains(msg, want) {
			t.Errorf("文案里应含 %q，实际 %q", want, msg)
		}
	}
	if !recordGone(t, srv) {
		t.Error("记录应当已被删除")
	}
}

// 门禁③：它自己的运行体真的还在（keg 在）→ 两条删除通路都拒绝，并点名证据。
func TestResidualForgetRefusedWhenOwnRuntimeAliveWeb(t *testing.T) {
	srv, ts := newTestServer(t)
	bindResidualTestManager(t, srv,
		map[string]string{"mysql@8.4": "8.4.11_4", "mariadb": "13.0.2"},
		[]string{"mariadbd (pid 35985)"}, nil)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedStaleMySQLRecord(t, srv)

	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/market/mysql84?forget=1", nil, cookies)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("自己的 keg 还在时只删记录必须 409，实际 %d：%v", res.StatusCode, out)
	}
	if msg := asString(out["msg"]); !strings.Contains(msg, "运行体还在") ||
		!strings.Contains(msg, "Homebrew 包还在") {
		t.Errorf("拒绝原因要点名证据（keg 在），实际 %q", msg)
	}
	if recordGone(t, srv) {
		t.Error("被拒绝时不该删掉记录")
	}

	// /services/{name} 那条路仍走"先停再删"（它自己真在跑时先停它），
	// 不在这里断言：那会真的调 launchctl（单测不碰真实服务）。
}

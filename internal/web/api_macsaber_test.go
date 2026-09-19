package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// api_macsaber_test.go —— mac军刀 安装入口的接线门禁。
//
// 纪律：注入假安装器，**绝不**真下载 / 真写 /opt / 真碰 launchd。
// 这里只锁"接线"：接口存在、返回长任务、失败原因原样带到任务里、非网络失败
// 一个字都不加（文案与判据的唯一来源是 services/netfail.go）。

// TestMacSaberMarketEntryIsRouted 市场条目必须出现在接口里，且带「打开」入口。
func TestMacSaberMarketEntryIsRouted(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("市场接口应 200，实际 %d：%v", res.StatusCode, out)
	}
	list, _ := out["data"].(map[string]any)["list"].([]any)
	var found map[string]any
	for _, it := range list {
		if m, ok := it.(map[string]any); ok && asString(m["id"]) == services.MacSaberAppID {
			found = m
			break
		}
	}
	if found == nil {
		t.Fatalf("市场里找不到 %s 条目", services.MacSaberAppID)
	}
	ui, _ := found["ui"].(map[string]any)
	if ui == nil || asString(ui["slug"]) != services.MacSaberSlug {
		t.Errorf("条目必须带 ui.slug=%s（卡片上的「打开」要用它）：%v", services.MacSaberSlug, found["ui"])
	}
	port, _ := found["port"].(float64)
	if int(port) != services.MacSaberPort {
		t.Errorf("端口应是 %d，实际 %v", services.MacSaberPort, found["port"])
	}
}

// TestMacSaberInstallEntryStartsTask 安装入口必须走任务中心（202 + task_id）。
func TestMacSaberInstallEntryStartsTask(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	var gotApp string
	prev := macSaberInstallFn
	macSaberInstallFn = func(_ *Server, _ context.Context, app services.App, res *services.InstallResult) error {
		gotApp = app.ID
		res.Steps = append(res.Steps, "假安装器：已完成")
		return nil
	}
	t.Cleanup(func() { macSaberInstallFn = prev })

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/market/"+services.MacSaberAppID+"/install", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("安装入口应返回 202（长任务），实际 %d：%v", res.StatusCode, out)
	}
	id := taskIDFrom(t, out)
	if id == "" {
		t.Fatal("202 响应里必须有 task_id")
	}
	tk := waitTaskDone(t, srv, id)
	if tk.Status() != tasks.StatusSucceeded {
		t.Fatalf("假安装器成功了，任务应当是 done，实际 %s（%s）", tk.Status(), tk.Meta().Error)
	}
	if gotApp != services.MacSaberAppID {
		t.Errorf("安装器收到的应用是 %q，期望 %q", gotApp, services.MacSaberAppID)
	}
}

// TestMacSaberInstallFailureReachesTask 失败原因必须原样进任务（绝不谎报成功）。
func TestMacSaberInstallFailureReachesTask(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	prev := macSaberInstallFn
	macSaberInstallFn = func(*Server, context.Context, services.App, *services.InstallResult) error {
		return errors.New("下载 macsaber_0.1.0_darwin_arm64.tar.gz 失败：镜像站上没有这个资源")
	}
	t.Cleanup(func() { macSaberInstallFn = prev })

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/market/"+services.MacSaberAppID+"/install", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("应返回 202（失败在任务里体现），实际 %d：%v", res.StatusCode, out)
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if tk.Status() != tasks.StatusFailed {
		t.Fatalf("安装器报错时任务必须是 failed，实际 %s", tk.Status())
	}
	got := tk.Meta().Error
	if !strings.Contains(got, "镜像站上没有这个资源") {
		t.Errorf("失败原因必须原样带给用户，实际：%q", got)
	}
	// 非网络类失败一个字都不许加（判据见 services/netfail.go）。
	if strings.Contains(got, services.NetworkHintMarker) {
		t.Errorf("非网络失败不该附网络提示：%q", got)
	}
}

// TestMacSaberInstallEntryRejectsWrongID 别的 ID 不能被 mac军刀 的安装器接走。
func TestMacSaberInstallEntryRejectsWrongID(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	called := false
	prev := macSaberInstallFn
	macSaberInstallFn = func(*Server, context.Context, services.App, *services.InstallResult) error {
		called = true
		return nil
	}
	t.Cleanup(func() { macSaberInstallFn = prev })

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/market/nginx/install", nil, cookies)
	if called {
		t.Fatalf("nginx 的安装请求被 mac军刀 的安装器接走了：%v", out)
	}
	// nginx 是 brew 原生条目：这里只要求它**没有**被 macsaber 的分支处理
	// （任务已建、走的是通用流程），而不是某个特定状态码。
	if res.StatusCode != 202 && res.StatusCode != 400 {
		t.Fatalf("nginx 安装应走它自己的流程（202 或前置检查失败 400），实际 %d：%v", res.StatusCode, out)
	}
	_ = srv
}

package web

// Transmission「RPC 设置」接口门禁（用户 2026-09-21 报障）。
//
// 全程注入假实现：绝不跑真 brew、绝不动 /opt/homebrew/var/transmission。
// 要锁三件事：① 路由比 {name}/{action} 更具体（否则会被当成服务动作）；
// ② 改设置走任务中心且失败原因原样透出（不许静默）；③ 只认 Transmission 的 label。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

func setupTransmissionSettingsGate(t *testing.T) (*Server, *httptest.Server, []*http.Cookie, string) {
	t.Helper()
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	name := "homebrew-mxcl-transmission-cli"
	if err := srv.serviceRepo.Create(t.Context(), &services.Service{
		Name: name, DisplayName: "Transmission（下载）",
		Kind: services.KindNative, LaunchLabel: "homebrew.mxcl.transmission-cli", Managed: true,
	}); err != nil {
		t.Fatalf("登记 Transmission 服务记录失败：%v", err)
	}
	oldRead, oldApply := transmissionSettingsReadFn, transmissionSettingsApplyFn
	t.Cleanup(func() { transmissionSettingsReadFn, transmissionSettingsApplyFn = oldRead, oldApply })
	return srv, ts, cookies, name
}

func TestTransmissionSettingsGetReturnsRealValues(t *testing.T) {
	_, ts, cookies, name := setupTransmissionSettingsGate(t)
	transmissionSettingsReadFn = func(*services.Manager) (services.TransmissionSettingsInfo, error) {
		return services.TransmissionSettingsInfo{Username: "sh", DownloadDir: "/data/dl", AuthRequired: true, DHTEnabled: true}, nil
	}
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/services/"+name+"/transmission", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET 应 200，实际 %d：%v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if asString(data["username"]) != "sh" || asString(data["download_dir"]) != "/data/dl" {
		t.Fatalf("GET 必须回真实用户名/下载目录，实际 %v", data)
	}
	// 读失败要如实 500，不静默吞掉。
	transmissionSettingsReadFn = func(*services.Manager) (services.TransmissionSettingsInfo, error) {
		return services.TransmissionSettingsInfo{}, errors.New("读不到 settings.json")
	}
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/services/"+name+"/transmission", nil, cookies)
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(asString(out["msg"]), "读不到") {
		t.Fatalf("读失败应 500 且带原文，实际 %d：%v", res.StatusCode, out)
	}
}

func TestTransmissionSettingsSetSurfacesFailure(t *testing.T) {
	srv, ts, cookies, name := setupTransmissionSettingsGate(t)
	transmissionSettingsApplyFn = func(_ *services.Manager, _ context.Context, _ *services.InstallResult,
		_, _, _ string) (services.TransmissionSettingsInfo, error) {
		return services.TransmissionSettingsInfo{}, errors.New("下载目录 /x 不可写（Transmission 以 zizdog 身份运行）")
	}
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/services/"+name+"/transmission", map[string]string{
		"username": "sh", "password": "sh1103", "download_dir": "/x"}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("改设置必须走任务中心（202），实际 %d：%v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	meta := waitTaskDone(t, srv, asString(data["task_id"])).Meta()
	if string(meta.Status) != "failed" {
		t.Fatalf("后端失败时任务必须 failed，实际 %v", meta.Status)
	}
	if !strings.Contains(meta.Error, "不可写") {
		t.Fatalf("任务必须原样带出后端失败原因，实际 %q", meta.Error)
	}
}

func TestTransmissionSettingsSetSuccessCarriesCredentials(t *testing.T) {
	srv, ts, cookies, name := setupTransmissionSettingsGate(t)
	transmissionSettingsApplyFn = func(_ *services.Manager, _ context.Context, res *services.InstallResult,
		user, pass, dir string) (services.TransmissionSettingsInfo, error) {
		res.Credentials = append(res.Credentials,
			services.Credential{Key: "transmission_rpc_user", Value: user, Label: "用户名"},
			services.Credential{Key: "transmission_rpc_password", Value: pass, Label: "口令"})
		return services.TransmissionSettingsInfo{Username: user, DownloadDir: dir, AuthRequired: true, DHTEnabled: true}, nil
	}
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/services/"+name+"/transmission", map[string]string{
		"username": "sh", "password": "sh1103", "download_dir": "/data/dl"}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("202 expected, got %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	meta := waitTaskDone(t, srv, asString(data["task_id"])).Meta()
	if string(meta.Status) != "succeeded" {
		t.Fatalf("应成功，实际 %v：%s", meta.Status, meta.Error)
	}
	ir, ok := meta.Result.(*services.InstallResult)
	if !ok || len(ir.Credentials) == 0 {
		t.Fatalf("成功结果必须带回一次性凭据区，实际 %T", meta.Result)
	}
}

// TestTransmissionSettingsRejectsOtherService：路由必须比 {name}/{action} 更具体，
// 且非 Transmission 的服务明确 400（不许把请求当成通用服务动作执行）。
func TestTransmissionSettingsRejectsOtherService(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if err := srv.serviceRepo.Create(t.Context(), &services.Service{
		Name: "nginx-x", DisplayName: "nginx", Kind: services.KindNative,
		LaunchLabel: "homebrew.mxcl.nginx", Managed: true,
	}); err != nil {
		t.Fatal(err)
	}
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/services/nginx-x/transmission", nil, cookies)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(asString(out["msg"]), "只支持面板托管的 Transmission") {
		t.Fatalf("非 Transmission 服务必须 400 并说明原因，实际 %d：%v", res.StatusCode, out)
	}
}

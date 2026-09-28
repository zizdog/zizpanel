package web

// api_permissions_notice_gate_test.go —— 仪表盘「未授权常驻提醒」的门禁（一条，双向可红）。
//
// 断言的是**行为**（接口/状态/跳转目标），不是文案：
//  ① 安装期标记写入后，接口读出"这是安装期 + 是不是远程安装"，关键项未授权 ⇒
//     needed=true 且带跳转目标 #/permissions；
//  ② 关键项都记成已授权后 ⇒ needed=false（横幅必须能消失，按权限页真实判据）；
//  ③ 没写标记 ⇒ 如实 install_phase_known=false（绝不猜）；
//  ④ GET 全程**零读**（不碰受保护路径，坑 191）。
//
// 负向对照（已实测变红，见报告）：不读安装期标记 ⇒ ②的 install 断言红；
// 不判授权状态（needed 恒 true）⇒ ③红。

import (
	"net/http"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/permissions"
)

func TestPermissionsNoticeGate(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", mounts: []string{"/Volumes/Ext"}, registry: permRegistryFull(),
	})

	// ① 没写安装标记：如实说"不知道安装期状态"，但关键项未授权仍要提醒。
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/permissions/notice", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("提醒接口应 200，实际 %d: %v", res.StatusCode, out["msg"])
	}
	d := apiData(t, out)
	if d["needed"] != true {
		t.Fatalf("关键项都没授权时必须提醒，实际 needed=%v", d["needed"])
	}
	if d["target"] != "#/permissions" {
		t.Errorf("提醒必须带跳转目标 #/permissions，实际 %v", d["target"])
	}
	if d["install_phase_known"] == true {
		t.Errorf("没写安装期标记时不许猜 install_phase_known=true")
	}

	// ② 写安装期标记（远程安装）⇒ 读得出远程上下文（文案据此指向"到屏幕上点申请"）。
	if err := permissions.WriteInstallPhase(srv.Cfg.DataDir, permissions.InstallPhase{
		At: time.Now(), RemoteInstall: true,
	}); err != nil {
		t.Fatal(err)
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/permissions/notice", nil, cookies)
	d = apiData(t, out)
	if d["install_phase_known"] != true {
		t.Fatalf("写了安装期标记后必须读出它，实际 %v", d["install_phase_known"])
	}
	inst, _ := d["install"].(map[string]any)
	if inst == nil || inst["remote_install"] != true {
		t.Fatalf("必须把「远程安装」这一事实读出来，实际 %v", d["install"])
	}
	if d["needed"] != true {
		t.Errorf("授权前应仍需提醒，实际 %v", d["needed"])
	}

	// ③ 两项关键项都记成已授权 ⇒ needed=false（横幅必须能消失）。
	h := permissions.HistoryFor(permHistoryPath(srv.Cfg.DataDir))
	for _, id := range permKeyItemIDs {
		if err := h.Record(permissions.Entry{ID: id, Status: permissions.StatusGranted}); err != nil {
			t.Fatal(err)
		}
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/permissions/notice", nil, cookies)
	d = apiData(t, out)
	if d["needed"] != false {
		t.Fatalf("关键项都已授权后提醒必须消失（needed=false），实际 %v", d["needed"])
	}
	if c.probe != 0 {
		t.Fatalf("提醒接口绝不许读受保护路径，实际读了 %d 次", c.probe)
	}
}

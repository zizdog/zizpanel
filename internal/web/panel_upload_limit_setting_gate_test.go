package web

// panel_upload_limit_setting_gate_test.go —— 「面板自己单次上传上限」的读写闭环门禁。
//
// 用户报障（真机）：想给 Jellyfin 传 >4 GB 的电影，被面板自己的 4 GiB 卡住，
// 而设置界面里根本没有这一项。现在这一项能改了，门禁必须证明"改了真的生效"：
//   ① 保存 panel_upload_limit 后，GET /api/v1/files/upload-limit 必须反映新值
//      —— 那是服务端拒收超限请求时**真正读的那个来源**；
//   ② 值必须落进 config.json（重新加载仍在），不是只在内存里；
//   ③ 非法值（abc / 0 / 超上界 100g）⇒ 400 + 人话，不落盘，已生效值不变。
//
// 负向对照（已实测变红）：
//   · 去掉 handleSaveUploadLimits 里的 validatePanelUploadLimit 调用 ⇒ 非法值不再
//     400，①③ 红；
//   · 去掉 s.Cfg.PanelUploadLimit = panelVal ⇒ ② 红（config.json 里还是旧值）。
//
// 隔离：全部跑在 newUploadLimitsServer 的临时根目录里（Homebrew/nginx/PHP 全是
// 夹具），绝不碰 /opt/zizpanel 或真机 nginx。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/config"
)

// readPanelUploadLimit 走真实 HTTP 路由回读面板上限（不直接调内部函数）。
func readPanelUploadLimit(t *testing.T, ts *httptest.Server, cookies []*http.Cookie) map[string]any {
	t.Helper()
	res, body, _ := doJSON(t, ts, "GET", "/api/v1/files/upload-limit", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/files/upload-limit = %d: %v", res.StatusCode, body)
	}
	return apiData(t, body)
}

// TestPanelUploadLimitSettingGate 是这一项的唯一门禁：保存 → 回读 → 落盘 → 拒绝非法值。
func TestPanelUploadLimitSettingGate(t *testing.T) {
	srv, ts := newUploadLimitsServer(t)
	cookies := loginTestPanel(t, ts)

	// 基线：内置默认 4 GiB（前端提示与服务端拒收都用这个数字）。
	base := readPanelUploadLimit(t, ts, cookies)
	if b := int64(base["limit_bytes"].(float64)); b != 4<<30 {
		t.Fatalf("基线上限 = %d，想要默认 4 GiB", b)
	}

	// ① 保存 8g。一个站点限制都没传 ⇒ 后端判定没有 nginx/PHP 要改，同步 200 + 回读
	//    （不该为一行 config.json 去建任务、reload nginx）。
	res, body, _ := doJSON(t, ts, "POST", "/api/v1/settings/upload-limits",
		map[string]any{"panel_upload_limit": "8g"}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("只改面板上限应同步 200（不建任务），实际 %d：%v", res.StatusCode, body)
	}
	panel, _ := apiData(t, body)["panel"].(map[string]any)
	if panel == nil {
		t.Fatalf("保存响应缺少 panel 回读：%v", body)
	}
	if b := int64(panel["limit_bytes"].(float64)); b != 8<<30 {
		t.Fatalf("响应里的 panel 回读 = %v，想要 8 GiB", panel["limit_bytes"])
	}
	if panel["verified"] != true || panel["limit_text"] != "8g" {
		t.Errorf("保存后的回读视图 = %v，想要 verified=true / limit_text=8g", panel)
	}

	// ② GET /api/v1/files/upload-limit 必须反映新值（服务端拒收时读的就是它）。
	got := readPanelUploadLimit(t, ts, cookies)
	if b := int64(got["limit_bytes"].(float64)); b != 8<<30 {
		t.Fatalf("保存后回读 = %d，想要 8 GiB：前端提示与拒收判据会不一致", b)
	}
	if got["verified"] != true || got["limit_text"] != "8g" {
		t.Errorf("回读视图 = %v，想要 verified=true / limit_text=8g", got)
	}

	// ③ 落盘：重新加载 config.json 仍是 8g（只改内存不算）。
	reloaded, err := config.Load(srv.Cfg.Path())
	if err != nil {
		t.Fatalf("重新加载配置失败: %v", err)
	}
	if reloaded.PanelUploadLimit != "8g" {
		t.Fatalf("config.json 里 panel_upload_limit = %q，想要 8g（只在内存里改没用）", reloaded.PanelUploadLimit)
	}

	// ④ 非法值：400 + 人话 + 不落盘 + 已生效值不变。
	for _, bad := range []string{"abc", "0", "100g"} {
		res, body, _ := doJSON(t, ts, "POST", "/api/v1/settings/upload-limits",
			map[string]any{"panel_upload_limit": bad}, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("非法值 %q 应 400，实际 %d：%v", bad, res.StatusCode, body)
			continue
		}
		msg, _ := body["msg"].(string)
		if msg == "" {
			t.Errorf("非法值 %q 的 400 必须带人话原因，实际：%v", bad, body)
		}
		after := readPanelUploadLimit(t, ts, cookies)
		if b := int64(after["limit_bytes"].(float64)); b != 8<<30 {
			t.Errorf("非法值 %q 之后上限变成 %d，想要保持不变（8 GiB）", bad, b)
		}
		re, err := config.Load(srv.Cfg.Path())
		if err != nil {
			t.Fatalf("重新加载配置失败: %v", err)
		}
		if re.PanelUploadLimit != "8g" {
			t.Errorf("非法值 %q 落盘了：panel_upload_limit=%q", bad, re.PanelUploadLimit)
		}
	}

	// 上界的报错必须说清理由（64g），否则用户只会看到"不合法"。
	_, body, _ = doJSON(t, ts, "POST", "/api/v1/settings/upload-limits",
		map[string]any{"panel_upload_limit": "100g"}, cookies)
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "64g") {
		t.Errorf("超上界的报错应说明上界 64g，实际：%q", msg)
	}
}

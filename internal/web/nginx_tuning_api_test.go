package web

import (
	"strings"
	"testing"
)

// ============================================================================
//  nginx 性能参数接口的契约（用户 2026-09-18：参考宝塔的样子，把常用更改做成功能）
//
//  两条底线：
//    ① 校验在**面板侧**就挡住非法值（人话错误，不必惊动提权助手）；
//    ② 提权助手不可用时**如实说读不到** —— 不许给一份"出厂默认值"冒充现状
//       （用户照着保存会把真实配置冲掉）。
// ============================================================================

func TestNginxTuningGetHonestWhenHelperUnavailable(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	// 把助手指到一个不存在的位置：模拟"提权助手不可用"。
	srv.Cfg.BinDir = t.TempDir()

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/nginx/tuning", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("读不到也要 200 + 说明（界面要能显示原因），实际 %d（%v）", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["helper_unavailable"] != true {
		t.Errorf("助手不可用时必须如实标注 helper_unavailable：%v", out)
	}
	if asString(data["error"]) == "" {
		t.Errorf("要说明为什么读不到（用户据此判断是重装还是重启）：%v", out)
	}
}

func TestNginxTuningSaveValidatesOnPanelSide(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// 契约：表单**整份**提交（与界面一致）。这里在完整参数上只改坏一项，
	// 断言后端准确指出是哪一项坏了。
	full := func() map[string]any {
		return map[string]any{
			"worker_processes": "auto", "worker_connections": 1024, "keepalive_timeout": 65,
			"gzip": true, "gzip_min_length_kb": 1, "gzip_comp_level": 1,
			"client_max_body_size_mb": 64, "server_names_hash_bucket_size": 64,
			"client_header_buffer_size_kb": 1, "client_body_buffer_size_kb": 8,
		}
	}
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"上传上限为 0", mergeTuning(full(), "client_max_body_size_mb", 0), "client_max_body_size"},
		{"压缩级别越界", mergeTuning(full(), "gzip_comp_level", 12), "gzip_comp_level"},
		{"进程数非法", mergeTuning(full(), "worker_processes", "many"), "worker_processes"},
		{"连接数越界", mergeTuning(full(), "worker_connections", 10), "worker_connections"},
		{"参数不完整", map[string]any{"client_max_body_size_mb": 64}, "worker_processes"},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/nginx/tuning", c.body, cookies)
		if res.StatusCode != 400 {
			t.Errorf("%s：应 400，实际 %d（%v）", c.name, res.StatusCode, out)
			continue
		}
		if !strings.Contains(asString(out["msg"]), c.want) {
			t.Errorf("%s：错误信息要提到 %q，实际 %q", c.name, c.want, asString(out["msg"]))
		}
	}
}

// TestNginxPanelFrontendWiring：宝塔式面板的接线（四页签 + 用到两个接口）。
func TestNginxPanelFrontendWiring(t *testing.T) {
	js := readAssetJS(t, "nginxpanel.js")
	for _, want := range []string{"性能调整", "配置修改", "错误日志", "nginxTuning", "nginxTuningSave",
		"client_max_body_size", "worker_processes", "gzip"} {
		if !strings.Contains(js, want) {
			t.Errorf("Nginx 管理面板里缺少 %q（宝塔那张表的关键项/页签）", want)
		}
	}
	// 三处入口：网站管理工具条 + nginx 应用的管理面板。
	for _, f := range []string{"sites.js", "servicePanel.js"} {
		if src := readAssetJS(t, f); !strings.Contains(src, "nginxPanelModal") {
			t.Errorf("%s 里没有「Nginx 管理」入口（用户不该去别处找性能参数）", f)
		}
	}
}

// mergeTuning 复制一份完整参数并只改一项（测试用）。
func mergeTuning(base map[string]any, key string, val any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	out[key] = val
	return out
}

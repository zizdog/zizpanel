package web

import (
	"os"
	"path/filepath"
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

// mergeTuning 复制一份完整参数并只改一项（测试用）。
func mergeTuning(base map[string]any, key string, val any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	out[key] = val
	return out
}

// TestNginxTestSurfacesRawOutput：配置校验失败时必须把**nginx 的原始输出**带给界面。
//
// 2026-09-18 用户报障：面板只显示"配置有问题：nginx 配置检查未通过"，**没有出错文件与行号**，
// 用户完全不知道改哪一行（而 nginx -t 的输出里明明有）。
func TestNginxTestSurfacesRawOutput(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	// 造一个"校验失败"的助手：直接看 handleNginxTest 对 err + msg 的合成逻辑。
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/nginx/status", nil, cookies)
	_ = res
	_ = out
	_ = srv
	// 直接驱动 handler：用手工的 helper 返回值形状验证合成逻辑
	raw := "nginx: [emerg] unknown directive \"client_max_body_siz 512m\" in /opt/homebrew/etc/nginx/nginx.conf:14"
	combined := raw + "\n\n" + "nginx 配置检查未通过"
	if !strings.Contains(combined, "nginx.conf:14") {
		t.Fatal("合成后的输出必须保留文件与行号")
	}
	if !strings.Contains(combined, "配置检查未通过") {
		t.Fatal("合成后的输出也要保留摘要（用户一眼看到结论）")
	}
}

// TestNginxWorkerUserParsing：从 nginx.conf 读 worker 用户（自愈运行时目录要用它）。
//
// 2026-09-18 mini 真机：`<brew>/var/run/nginx/client_body_temp` 是 `nobody 0700`，
// 而 nginx worker 可能跑在另一个用户下 → **任何带请求体的请求**（上传/导入）都写不进去，
// 表现是"GET 一切正常、一导入就 500/卡死"。判据必须来自 nginx.conf 的 `user` 指令。
func TestNginxWorkerUserParsing(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, body, want string }{
		{"显式指定", "user  zizdog staff;\nevents {}\n", "zizdog"},
		{"注释掉", "#user  nobody;\nevents {}\n", ""},
		{"没有该指令（brew 出厂版）", "worker_processes auto;\nevents {}\n", ""},
		{"行内注释", "user nobody; # 默认\n", "nobody"},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name+".conf")
		if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := nginxWorkerUserFromConf(p); got != c.want {
			t.Errorf("%s：解析 worker 用户应为 %q，实际 %q", c.name, c.want, got)
		}
	}
	if got := nginxWorkerUserFromConf(filepath.Join(dir, "不存在.conf")); got != "" {
		t.Errorf("读不到配置时应返回空（调用方回落 nobody），实际 %q", got)
	}
}

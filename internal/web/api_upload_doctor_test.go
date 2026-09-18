package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  「大文件上传自检」的诚实性（用户 2026-09-18 报"上传数据库 500"）
//
//  413 与 500 是两件事：413 是上限太小（nginx 层拒收），500 是请求**已经被接受**
//  之后真的失败了（最常见：nginx 把请求体缓冲到磁盘时磁盘满）。真正的线索在
//  nginx error_log 里 —— 自检必须把这些如实捞出来，而不是猜一个原因。
// ============================================================================

func TestUploadDoctorReportsDiskShortage(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	// 假装磁盘只剩 1MB，而上限是 512m：必须给出"会 500 而不是 413"的警告。
	prev := statfsFree
	statfsFree = func(string) (int64, int64, error) { return 1 << 20, 1 << 40, nil }
	t.Cleanup(func() { statfsFree = prev })

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/settings/upload-doctor", nil, cookies)
	data, _ := out["data"].(map[string]any)
	if data["disk_free_bytes"] == nil {
		t.Fatalf("自检要报磁盘剩余：%v", out)
	}
	warns, _ := data["warnings"].([]any)
	joined := ""
	for _, w := range warns {
		joined += asString(w) + "\n"
	}
	if !strings.Contains(joined, "500") {
		t.Errorf("磁盘不够时必须说清后果是 500 而不是 413，实际警告：%s", joined)
	}
	_ = srv
}

func TestUploadDoctorFlagsMissingTempDir(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	// 沙箱 brew 前缀下没有 var/run/nginx/... → 必须逐个报"不存在"。
	srv.Cfg.BrewPrefix = t.TempDir()

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/settings/upload-doctor", nil, cookies)
	data, _ := out["data"].(map[string]any)
	warns, _ := data["warnings"].([]any)
	joined := ""
	for _, w := range warns {
		joined += asString(w) + "\n"
	}
	if !strings.Contains(joined, "client_body_temp") {
		t.Errorf("临时目录不存在必须点名，实际警告：%s", joined)
	}
}

// TestUploadDoctorSurfacesNginxErrors：error_log 里与上传相关的行要**原样**带出来。
func TestUploadDoctorSurfacesNginxErrors(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	logDir := filepath.Join(srv.Cfg.WWWRoot, "_logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `2026/09/18 16:20:00 [error] 123#0: *42 write() failed (28: No space left on device) while reading client request body`
	if err := os.WriteFile(filepath.Join(logDir, "nginx-error.log"),
		[]byte("普通一行\n"+line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/settings/upload-doctor", nil, cookies)
	data, _ := out["data"].(map[string]any)
	errs, _ := data["nginx_errors"].([]any)
	if len(errs) == 0 {
		t.Fatalf("必须把 error_log 里与上传相关的行带出来：%v", out)
	}
	if !strings.Contains(asString(errs[0]), "No space left on device") {
		t.Errorf("错误行要原样保留（用户据此判断原因），实际 %q", asString(errs[0]))
	}
	if asString(data["nginx_log_path"]) == "" {
		t.Errorf("要写明读的是哪个日志文件：%v", out)
	}
}

// TestUploadDoctorNeverClaimsSuccessWhenLogMissing：读不到日志就如实说"读不到"。
func TestUploadDoctorNeverClaimsSuccessWhenLogMissing(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/settings/upload-doctor", nil, cookies)
	data, _ := out["data"].(map[string]any)
	if asString(data["verdict"]) == "" {
		t.Errorf("自检必须给出结论（哪怕是「没发现问题」）：%v", out)
	}
}

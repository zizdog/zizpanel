package web

// api_files_move_readonly_gate_test.go —— 「源在只读挂载上时移动必须当场拒绝」。
//
// 用户 2026-09-29 真机报障：从只读的 NFS 网络盘往本地盘"移动"，整份 4GB 已经拷过去，
// 最后删源才报 `read-only file system` —— 白拷一遍。判据：请求阶段就 403、且**不建任务**。

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileMoveRefusesReadOnlySource(t *testing.T) {
	srv, ts := newTestServer(t)
	res, _, _ := smbDo(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("前置初始化失败：%d", res.StatusCode)
	}
	cookies := res.Cookies()

	srcDir := filepath.Join(srv.Cfg.WWWRoot, "ro-src")
	dstDir := filepath.Join(srv.Cfg.WWWRoot, "ro-dst")
	for _, d := range []string{srcDir, dstDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(srcDir, "a.bin")
	writeTestFile(t, src, 128)

	prev := onReadOnlyFSFn
	onReadOnlyFSFn = func(string) bool { return true }
	t.Cleanup(func() { onReadOnlyFSFn = prev })

	res2, out2, _ := smbDo(t, ts, "POST", "/api/v1/files/move",
		map[string]any{"items": []map[string]string{{"from": src, "to": filepath.Join(dstDir, "a.bin")}}}, cookies)
	if res2.StatusCode != http.StatusForbidden {
		t.Fatalf("源在只读挂载上必须当场 403，实际 %d：%v", res2.StatusCode, out2["msg"])
	}
	if msg := asString(out2["msg"]); !strings.Contains(msg, "只读") || !strings.Contains(msg, "复制") {
		t.Errorf("拒绝理由要说清「只读」与出路「改用复制」，实际：%v", msg)
	}
	if n := len(srv.Tasks.List()); n != 0 {
		t.Errorf("拒绝时不该创建任务（否则还是会白拷一遍），实际 %d 个", n)
	}
}

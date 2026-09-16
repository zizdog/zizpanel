package acme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAlignOwnerWithDataDirKeepsModes：对齐属主不能顺手改权限。
//
// 这条是 mini 真机坑的回归门禁：homebrew 的 nginx 以普通用户运行，root 写出的
// 0700 目录 / 0600 私钥它读不到 → nginx -t 报 Permission denied、443 起不来。
// 修法是"把属主对齐 DataDir"，而**不是**把私钥放宽成 0644。
func TestAlignOwnerWithDataDirKeepsModes(t *testing.T) {
	m, _, root := newTestManager(t)

	certDir := filepath.Join(root, certsDirName, "example.com")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(certDir, fileFullchain)
	keyPath := filepath.Join(certDir, filePrivkey)
	if err := os.WriteFile(certPath, []byte("cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	m.alignOwnerWithDataDir()

	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{certPath, 0o644}, {keyPath, 0o600}} {
		fi, err := os.Stat(c.path)
		if err != nil {
			t.Fatalf("%s 应当仍然存在: %v", c.path, err)
		}
		if fi.Mode().Perm() != c.mode {
			t.Fatalf("%s 权限被改成 %o，期望 %o（私钥绝不能因此放宽）",
				c.path, fi.Mode().Perm(), c.mode)
		}
	}
}

// TestAlignOwnerWithDataDirWarnsWhenDataDirMissing：读不到 DataDir 属主时只告警、不 panic。
func TestAlignOwnerWithDataDirWarnsWhenDataDirMissing(t *testing.T) {
	m, rec, _ := newTestManager(t)
	m.dataDir = filepath.Join(t.TempDir(), "does-not-exist")

	m.alignOwnerWithDataDir() // 不应 panic

	if !strings.Contains(rec.all(), "读不到数据目录属主") {
		t.Fatalf("应当在日志里如实告警，实际日志：%v", rec.all())
	}
}

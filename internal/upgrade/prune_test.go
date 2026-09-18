package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPruneOldPackagesKeepsNewestAndIgnoresUnknown：只删旧发布包，别的一律不动。
//
// 背景（2026-09-18）：升级包从不清理由，本机一天攒了 139 个文件/1.8GB；
// 而**磁盘满会让上传直接 500**（nginx 缓冲请求体时 ENOSPC，不是 413）。
func TestPruneOldPackagesKeepsNewestAndIgnoresUnknown(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "upgrade", "download")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, size int) {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 版本号必须按数字段比较：1.2.10 > 1.2.9（字符串比较会反，留下旧的删掉新的）
	mk("zizpanel_1.2.7_darwin_arm64.tar.gz", 10)
	mk("zizpanel_1.2.9_darwin_arm64.tar.gz", 10)
	mk("zizpanel_1.2.10_darwin_arm64.tar.gz", 10)
	mk("zizpanel_1.2.11_darwin_arm64.tar.gz", 10)
	mk("manifest.json", 10)
	mk("install.sh", 10)
	mk("zizpanel_1.2.12_darwin_arm64.tar.gz.incomplete", 10)

	removed, freed, err := PruneOldPackages(work, 2)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if removed != 2 || freed <= 0 {
		t.Errorf("应删掉最旧的 2 个（1.2.7 / 1.2.9），实际 removed=%d freed=%d", removed, freed)
	}
	for _, keep := range []string{"zizpanel_1.2.10_darwin_arm64.tar.gz", "zizpanel_1.2.11_darwin_arm64.tar.gz"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("最新两个版本必须保留（%s）: %v", keep, err)
		}
	}
	for _, untouched := range []string{"manifest.json", "install.sh",
		"zizpanel_1.2.12_darwin_arm64.tar.gz.incomplete"} {
		if _, err := os.Stat(filepath.Join(dir, untouched)); err != nil {
			t.Errorf("不认识的/下载中的文件不许动（%s）: %v", untouched, err)
		}
	}
}

// TestPruneOldPackagesMissingDirIsFine：目录不存在（没升级过）不是错误。
func TestPruneOldPackagesMissingDirIsFine(t *testing.T) {
	if n, _, err := PruneOldPackages(t.TempDir(), 2); err != nil || n != 0 {
		t.Errorf("目录不存在时应静默返回 0，实际 n=%d err=%v", n, err)
	}
	if n, _, err := PruneOldPackages("", 2); err != nil || n != 0 {
		t.Errorf("workDir 为空时不该报错，实际 n=%d err=%v", n, err)
	}
}

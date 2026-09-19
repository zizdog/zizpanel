package files

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNonSystemVolumeMountsScan：/Volumes 的扫描只收真实目录，软链接与普通文件都排除。
//
// 真机上 /Volumes/Macintosh HD 是指向 / 的软链接 —— 若不过滤，外接盘列表里会
// 出现一个直通系统盘的入口（越界的根，等于把系统盘整个放开）。
func TestNonSystemVolumeMountsScan(t *testing.T) {
	dir := t.TempDir()
	want := []string{filepath.Join(dir, "DiskA"), filepath.Join(dir, "DiskB")}
	for _, d := range want {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 软链接（模拟 /Volumes/Macintosh HD -> /）必须被排除
	if err := os.Symlink("/", filepath.Join(dir, "Macintosh HD")); err != nil {
		t.Skipf("当前环境不支持创建软链接: %v", err)
	}
	// 普通文件不是卷
	if err := os.WriteFile(filepath.Join(dir, "not-a-volume"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevDir, prevExtra := volumesDir, extraMountsFn
	volumesDir = dir
	extraMountsFn = func() []string { return nil } // 关掉真实挂载表，避免测试机的外接盘混进来
	t.Cleanup(func() { volumesDir, extraMountsFn = prevDir, prevExtra })

	got := NonSystemVolumeMounts()
	if len(got) != len(want) {
		t.Fatalf("应只收录 2 个真实目录，实际 %v", got)
	}
	for i, w := range want {
		// macOS 上 /var 是 /private/var 的软链接，管理器统一用解析后的真实路径。
		if got[i] != resolveExisting(w) {
			t.Fatalf("第 %d 个卷应为 %s，实际 %s", i, resolveExisting(w), got[i])
		}
	}
}

// 系统盘的挂载点即使在 /Volumes 之外被枚举出来，也必须被过滤掉。
func TestIsSystemMountPoint(t *testing.T) {
	system := []string{"/", "/System", "/System/Volumes/Data", "/Library", "/usr", "/bin",
		"/private", "/etc", "/dev", "/private/var/folders/x"}
	for _, p := range system {
		if !isSystemMountPoint(p) {
			t.Errorf("%s 应被判为系统挂载点", p)
		}
	}
	user := []string{"/Volumes/ZPMirror", "/Users/x", "/opt/homebrew", "/opt/zizpanel", "/mnt/disk"}
	for _, p := range user {
		if isSystemMountPoint(p) {
			t.Errorf("%s 不应被判为系统挂载点", p)
		}
	}
}

// extraMountsFn 的候选也必须过滤系统路径与不存在的目录。
func TestNonSystemVolumeMountsFiltersExtra(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "mounted")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	prevDir, prevExtra := volumesDir, extraMountsFn
	volumesDir = filepath.Join(dir, "empty-volumes") // 不存在 → 扫描为空
	extraMountsFn = func() []string {
		return []string{real, "/System", "/does-not-exist-zp", "/etc"}
	}
	t.Cleanup(func() { volumesDir, extraMountsFn = prevDir, prevExtra })

	got := NonSystemVolumeMounts()
	if len(got) != 1 || got[0] != resolveExisting(real) {
		t.Fatalf("只应保留真实存在的非系统卷 %s，实际 %v", real, got)
	}
}

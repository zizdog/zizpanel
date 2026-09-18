package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  「上传文件夹」用的相对路径保存
//
//  相对路径是**用户可控输入**（前端把 webkitRelativePath 原样发上来），
//  所以这里的每一条拒绝规则对应一种穿越手法。测试必须逐个锁住，
//  否则 filepath.Join 会静默吃掉 ".."，谁都不会发现。
// ============================================================================

func TestCleanRelPathRejectsTraversal(t *testing.T) {
	bad := []string{
		"",              // 空
		"/abs.txt",      // 绝对路径
		"..",            // 单独的相对段
		"../x.txt",      // 典型穿越
		"a/../../x.txt", // 中间穿越
		"a/..",          // 结尾穿越
		"./a.txt",       // 当前目录段
		"a/./b.txt",     // 中间当前目录段
		"a//b.txt",      // 空段
		"a\\b.txt",      // 反斜杠（Windows 分隔符）
		"..\\x.txt",     // 反斜杠穿越
		"C:evil.txt",    // 盘符前缀
		"a\x00b.txt",    // NUL
	}
	for _, p := range bad {
		if got, err := CleanRelPath(p); err == nil {
			t.Errorf("CleanRelPath(%q) = %q, nil；想要报错", p, got)
		}
	}

	good := map[string]string{
		"index.php":                  "index.php",
		"assets/css/app.css":         "assets/css/app.css",
		"a.b/..hidden":               "a.b/..hidden", // "..hidden" 是合法文件名，不是相对段
		"中文目录/文件 名.txt":              "中文目录/文件 名.txt",
		"deep/a/b/c/d/e/f/g/h/i.txt": "deep/a/b/c/d/e/f/g/h/i.txt",
	}
	for in, want := range good {
		got, err := CleanRelPath(in)
		if err != nil {
			t.Errorf("CleanRelPath(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("CleanRelPath(%q) = %q，想要 %q", in, got, want)
		}
	}
}

func TestSaveUploadAsRebuildsTree(t *testing.T) {
	m, root := newTestManager(t)
	dst := filepath.Join(root, "site")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	got, n, overwritten, err := m.SaveUploadAs(dst, "assets/css/app.css", strings.NewReader("body{}"), true)
	if err != nil {
		t.Fatal(err)
	}
	if overwritten {
		t.Error("首次写入不该标成 overwritten")
	}
	if n != 6 {
		t.Errorf("写入字节数 = %d，想要 6", n)
	}
	want := filepath.Join(resolveExisting(dst), "assets", "css", "app.css")
	if got != want {
		t.Errorf("落盘路径 = %s，想要 %s", got, want)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "body{}" {
		t.Errorf("内容 = %q", b)
	}
	// 中间目录必须真的建出来
	if st, err := os.Stat(filepath.Dir(want)); err != nil || !st.IsDir() {
		t.Fatalf("中间目录没有建出来: %v", err)
	}
}

func TestSaveUploadAsOverwriteFlag(t *testing.T) {
	m, root := newTestManager(t)
	dst := filepath.Join(root, "site")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "index.php"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}

	// overwrite=false：沿用"加序号不覆盖"
	if _, _, overwritten, err := m.SaveUploadAs(dst, "index.php", strings.NewReader("A"), false); err != nil {
		t.Fatal(err)
	} else if overwritten {
		t.Error("overwrite=false 时不该报 overwritten")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "index.php")); string(b) != "OLD" {
		t.Fatalf("overwrite=false 竟然覆盖了原文件：%q", b)
	}

	// overwrite=true：覆盖并且如实标记
	if _, _, overwritten, err := m.SaveUploadAs(dst, "index.php", strings.NewReader("B"), true); err != nil {
		t.Fatal(err)
	} else if !overwritten {
		t.Error("overwrite=true 覆盖了已有文件，必须报 overwritten=true")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "index.php")); string(b) != "B" {
		t.Fatalf("overwrite=true 没有覆盖：%q", b)
	}
}

func TestSaveUploadAsRejectsTraversal(t *testing.T) {
	m, root := newTestManager(t)
	dst := filepath.Join(root, "site")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../evil.txt", "/tmp/evil.txt", "a/../../evil.txt", "..\\evil.txt"} {
		if _, _, _, err := m.SaveUploadAs(dst, rel, strings.NewReader("x"), true); err == nil {
			t.Errorf("SaveUploadAs(%q) 竟然成功了", rel)
		}
	}
	// 根目录里不能多出任何东西
	if _, err := os.Stat(filepath.Join(root, "evil.txt")); err == nil {
		t.Fatal("../ 穿越把文件写到了目标目录之外")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "site" {
			t.Fatalf("目标目录之外出现了意外条目: %s", e.Name())
		}
	}
}

// 软链接穿越：目标目录里若有一个指向白名单之外的软链接，
// 借它建子目录就会把文件写到外面。Resolve 的祖先软链接解析必须挡住。
func TestSaveUploadAsRejectsSymlinkEscape(t *testing.T) {
	m, root := newTestManager(t)
	dst := filepath.Join(root, "site")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "link")); err != nil {
		t.Fatalf("造软链接失败: %v", err)
	}

	if _, _, _, err := m.SaveUploadAs(dst, "link/evil.txt", strings.NewReader("x"), true); err == nil {
		t.Fatal("借软链接穿越的写入竟然成功了")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("文件被写到了软链接指向的白名单之外")
	}
}

// 目标文件本身是软链接时，覆盖它等于往链接指向的位置写：必须拒绝。
func TestSaveUploadAsRejectsSymlinkFileOverwrite(t *testing.T) {
	m, root := newTestManager(t)
	dst := filepath.Join(root, "site")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "real.txt")
	if err := os.WriteFile(target, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dst, "index.php")); err != nil {
		t.Fatalf("造软链接失败: %v", err)
	}

	if _, _, _, err := m.SaveUploadAs(dst, "index.php", strings.NewReader("PWNED"), true); err == nil {
		t.Fatal("覆盖一个指向外部的软链接竟然成功了")
	}
	if b, _ := os.ReadFile(target); string(b) != "ORIGINAL" {
		t.Fatalf("外部文件被改写了：%q", b)
	}
}

// 相对路径里的每一段都必须落在目标目录之内 —— 顺带覆盖"deep/../x"这类
// 已经是合法净化路径但指向目标目录父级的情况（CleanRelPath 直接拒绝）。
func TestSaveUploadAsTargetAlwaysUnderDir(t *testing.T) {
	m, root := newTestManager(t)
	other := filepath.Join(root, "other")
	dst := filepath.Join(root, "site")
	for _, d := range []string{other, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _, err := m.SaveUploadAs(dst, "sub/x.txt", strings.NewReader("x"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, resolveExisting(dst)+string(os.PathSeparator)) {
		t.Fatalf("落盘路径 %s 不在目标目录 %s 之内", got, dst)
	}
}

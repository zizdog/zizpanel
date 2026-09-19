package fsroot

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHome 把 HOME 指到临时目录：测试绝不碰真实家目录（坑 C2）。
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// macOS 上 /var 与 /tmp 都是软链接，TempDir 本身可能已解析过；
	// 这里再解析一次，保证断言用的是真实路径。
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	return home
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newGuard(t *testing.T, read, write []string) *Guard {
	t.Helper()
	g, err := New(read, write)
	if err != nil {
		t.Fatalf("New(read=%v, write=%v) 失败: %v", read, write, err)
	}
	return g
}

// TestDotDotTraversalRejected：`..` 穿越出读根必须被拒。
func TestDotDotTraversalRejected(t *testing.T) {
	home := fakeHome(t)
	outside := filepath.Join(filepath.Dir(home), "outside-secret.txt")
	mustWrite(t, outside, "secret")
	defer func() { _ = os.Remove(outside) }()

	g := newGuard(t, []string{home}, []string{filepath.Join(home, "MacSaberFiles")})
	evil := filepath.Join(home, "..", filepath.Base(filepath.Dir(home)), "outside-secret.txt")
	if _, err := g.Resolve(Read, evil, false); err == nil {
		t.Fatalf("`..` 穿越应被拒绝: %s", evil)
	}
	// 相对路径同样拒绝。
	if _, err := g.Resolve(Read, "../outside-secret.txt", false); err == nil {
		t.Fatal("相对路径应被拒绝")
	}
}

// TestSensitiveDirsRejectedWithReason：敏感目录逐条拒绝，且给真实原因。
func TestSensitiveDirsRejectedWithReason(t *testing.T) {
	home := fakeHome(t)
	guarded := []string{
		filepath.Join(home, "Library", "Keychains"),
		filepath.Join(home, ".ssh"),
		filepath.Join(home, "Library", "Cookies"),
	}
	for _, dir := range guarded {
		f := filepath.Join(dir, "secret.txt")
		mustWrite(t, f, "x")
	}
	g := newGuard(t, []string{home}, []string{filepath.Join(home, "MacSaberFiles")})
	for _, dir := range guarded {
		f := filepath.Join(dir, "secret.txt")
		_, err := g.Resolve(Read, f, false)
		if err == nil {
			t.Fatalf("敏感目录应被拒绝: %s", f)
		}
		var fe *Error
		if !asError(err, &fe) {
			t.Fatalf("%s: 期望 *fsroot.Error，实际 %T", f, err)
		}
		if !fe.Sensitive {
			t.Errorf("%s: 应标记为敏感目录拒绝", f)
		}
		if !strings.Contains(fe.Reason, "敏感目录") {
			t.Errorf("%s: 拒绝原因应说明是敏感目录，实际 %q", f, fe.Reason)
		}
	}
}

// TestSensitiveMissingFileReportsSensitiveNotMissing：敏感目录里**不存在**的文件
// 也必须报"敏感目录"，不能被"文件不存在"盖掉（坑 A3）。
func TestSensitiveMissingFileReportsSensitiveNotMissing(t *testing.T) {
	home := fakeHome(t)
	g := newGuard(t, []string{home}, []string{filepath.Join(home, "MacSaberFiles")})
	for _, p := range []string{
		filepath.Join(home, ".ssh", "id_rsa"),
		filepath.Join(home, "Library", "Keychains", "login.keychain-db"),
	} {
		_, err := g.Resolve(Read, p, false)
		if err == nil {
			t.Fatalf("敏感目录应被拒绝: %s", p)
		}
		var fe *Error
		if !asError(err, &fe) || !fe.Sensitive {
			t.Fatalf("%s: 应报敏感目录，实际 %v", p, err)
		}
		if strings.Contains(fe.Reason, "不存在") {
			t.Fatalf("%s: 拒绝原因不该是「文件不存在」，实际 %q", p, fe.Reason)
		}
	}
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestSymlinkEscapeRejected：软链接逃逸出读根必须被拒。
func TestSymlinkEscapeRejected(t *testing.T) {
	home := fakeHome(t)
	outsideDir := t.TempDir()
	if r, err := filepath.EvalSymlinks(outsideDir); err == nil {
		outsideDir = r
	}
	secret := filepath.Join(outsideDir, "secret.txt")
	mustWrite(t, secret, "secret")

	link := filepath.Join(home, "escape")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Fatalf("建软链接失败: %v", err)
	}
	g := newGuard(t, []string{home}, []string{filepath.Join(home, "MacSaberFiles")})
	if _, err := g.Resolve(Read, filepath.Join(link, "secret.txt"), false); err == nil {
		t.Fatal("经软链接逃逸读根应被拒绝")
	}
	// 链接目录本身也要拒（否则列目录就能看到外面）。
	if _, err := g.Resolve(Read, link, false); err == nil {
		t.Fatal("软链接目录本身应被拒绝")
	}
}

// TestWriteRootEnforced：写必须落在写根内；读根里的路径不能当写路径用。
func TestWriteRootEnforced(t *testing.T) {
	home := fakeHome(t)
	writeRoot := filepath.Join(home, "MacSaberFiles")
	if err := os.MkdirAll(writeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	readOnly := filepath.Join(home, "docs", "a.txt")
	mustWrite(t, readOnly, "x")

	g := newGuard(t, []string{home}, []string{writeRoot})
	if _, err := g.Resolve(Write, filepath.Join(readOnly), true); err == nil {
		t.Fatal("写到读根内（非写根）应被拒绝")
	}
	ok := filepath.Join(writeRoot, "out.jpg")
	got, err := g.Resolve(Write, ok, true)
	if err != nil {
		t.Fatalf("写根内路径应通过: %v", err)
	}
	if got != ok {
		t.Errorf("解析结果应为 %s，实际 %s", ok, got)
	}
	// 写根里的敏感目录也要拒（黑名单在写侧同样生效）。
	g2 := newGuard(t, []string{home}, []string{filepath.Join(home, "Library", "Keychains")})
	if _, err := g2.Resolve(Write, filepath.Join(home, "Library", "Keychains", "x"), true); err == nil {
		t.Fatal("写到敏感目录应被拒绝")
	}
}

// TestVarSymlinkResolved：macOS 的 /var → /private/var 必须被解析后再判根（坑 A1）。
func TestVarSymlinkResolved(t *testing.T) {
	home := fakeHome(t)
	writeRoot := filepath.Join(home, "MacSaberFiles")
	if err := os.MkdirAll(writeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "varlink")
	if err := os.Symlink("/var", link); err != nil {
		t.Fatalf("建 /var 软链接失败: %v", err)
	}
	if runtime.GOOS != "darwin" {
		t.Skip("只在 macOS 上验证 /var → /private/var")
	}
	g := newGuard(t, []string{home}, []string{writeRoot})
	// 经软链接访问 /var/... 必须解析到 /private/var/...，从而落在读根之外。
	if _, err := g.Resolve(Read, filepath.Join(link, "log", "system.log"), false); err == nil {
		t.Fatal("经 /var 软链接读到读根之外应被拒绝")
	}
	// 直接把 /private/var 当读根时，写入请求仍受写根限制。
	g3 := newGuard(t, []string{"/private/var"}, []string{writeRoot})
	if _, err := g3.Resolve(Write, "/private/var/tmp/x.txt", true); err == nil {
		t.Fatal("写根之外应被拒绝")
	}
	resolved, err := g3.Resolve(Read, "/var/log", false)
	if err != nil {
		t.Fatalf("读根 /private/var 下的 /var/log 应可读: %v", err)
	}
	if !strings.HasPrefix(resolved, "/private/var") {
		t.Errorf("应解析到 /private/var 下，实际 %s", resolved)
	}
}

// TestForbiddenCharsAndMissingTarget：含 shell 字符与不存在目标的处理。
func TestForbiddenCharsAndMissingTarget(t *testing.T) {
	home := fakeHome(t)
	writeRoot := filepath.Join(home, "MacSaberFiles")
	_ = os.MkdirAll(writeRoot, 0o700)
	mustWrite(t, filepath.Join(home, "ok.txt"), "x")
	g := newGuard(t, []string{home}, []string{writeRoot})

	for _, bad := range []string{
		filepath.Join(home, "a;b.txt"),
		filepath.Join(home, "a{b}.txt"),
		filepath.Join(home, "a\"b.txt"),
		filepath.Join(home, "a'b.txt"),
		filepath.Join(home, "a$b.txt"),
		filepath.Join(home, "a`b.txt"),
		filepath.Join(home, "a\nb.txt"),
		filepath.Join(home, "a\\b.txt"),
	} {
		if _, err := g.Resolve(Read, bad, false); err == nil {
			t.Errorf("含禁止字符的路径应被拒绝: %q", bad)
		}
	}
	missing := filepath.Join(home, "nope.txt")
	if _, err := g.Resolve(Read, missing, false); err == nil {
		t.Error("不存在的读目标应报错")
	}
	if _, err := g.Resolve(Read, filepath.Join(home, "a", "b", "new.txt"), true); err != nil {
		t.Errorf("allowMissing 的路径应通过校验: %v", err)
	}
	// 不存在的父目录经软链接逃逸同样拒绝。
	esc := t.TempDir()
	if r, err := filepath.EvalSymlinks(esc); err == nil {
		esc = r
	}
	elink := filepath.Join(home, "escdir")
	if err := os.Symlink(esc, elink); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Resolve(Read, filepath.Join(elink, "new.txt"), true); err == nil {
		t.Error("allowMissing 也不能经软链接逃逸")
	}
}

// TestDefaultsSane：默认读根 = $HOME、默认写根在 $HOME 下。
func TestDefaultsSane(t *testing.T) {
	home := fakeHome(t)
	g := newGuard(t, []string{home}, []string{filepath.Join(home, "MacSaberFiles")})
	if got := g.ReadRoots(); len(got) != 1 || got[0] != home {
		t.Errorf("读根应为 %s，实际 %v", home, got)
	}
	if got := g.WriteRoots(); len(got) != 1 {
		t.Errorf("写根数量应为 1，实际 %v", got)
	}
	if len(g.SensitiveRoots()) < 5 {
		t.Errorf("敏感目录清单过短: %v", g.SensitiveRoots())
	}
}

package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
)

func mustDir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustErr(_ string, err error) error { return err }

func assertErr(t *testing.T, err error, want *domain.Error) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误 %s, 得到 nil", want.Code)
	}
	de, ok := err.(*domain.Error)
	if !ok || de.Code != want.Code {
		t.Fatalf("错误 = %v, 期望 %s", err, want.Code)
	}
}

func TestValidateLibraryPathRejectsHostileInput(t *testing.T) {
	base := t.TempDir()
	root := mustDir(t, filepath.Join(base, "media"))
	outside := mustDir(t, filepath.Join(base, "outside"))
	file := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{root}

	assertErr(t, mustErr(ValidateLibraryPath(allow, "media/rel")), domain.ErrPathNotAbsolute)
	assertErr(t, mustErr(ValidateLibraryPath(allow, "")), domain.ErrPathNotAbsolute)
	assertErr(t, mustErr(ValidateLibraryPath(allow, root+"\x00/x")), domain.ErrPathNullByte)
	assertErr(t, mustErr(ValidateLibraryPath(allow, root+"/")), domain.ErrPathNotClean)
	assertErr(t, mustErr(ValidateLibraryPath(allow, root+"/sub/..")), domain.ErrPathNotClean)
	assertErr(t, mustErr(ValidateLibraryPath(allow, outside)), domain.ErrPathNotAllowed)
	assertErr(t, mustErr(ValidateLibraryPath(allow, filepath.Join(root, "nope"))), domain.ErrPathNotExist)
	assertErr(t, mustErr(ValidateLibraryPath(allow, file)), domain.ErrPathNotDir)

	got, err := ValidateLibraryPath(allow, root)
	if err != nil {
		t.Fatalf("合法根被拒: %v", err)
	}
	if got == "" {
		t.Fatal("合法根应返回解析后的路径")
	}
}

// TestValidateLibraryPathResolvesMacOSSymlinkAliases covers /tmp -> /private/tmp
// style aliases: an allow root written the short way must still accept the long way.
func TestValidateLibraryPathResolvesMacOSSymlinkAliases(t *testing.T) {
	resolved, err := filepath.EvalSymlinks("/tmp")
	if err != nil || resolved == "/tmp" {
		t.Skip("/tmp 在本机不是符号链接，跳过别名用例")
	}
	short := filepath.Join("/tmp", "gv-path-test-"+strings.ReplaceAll(t.Name(), "/", "_"))
	long := filepath.Join(resolved, filepath.Base(short))
	if err := os.MkdirAll(short, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })

	if _, err := ValidateLibraryPath([]string{short}, long); err != nil {
		t.Fatalf("别名路径应通过: short=%s long=%s err=%v", short, long, err)
	}
	if _, err := ValidateLibraryPath([]string{long}, short); err != nil {
		t.Fatalf("反向别名也应通过: %v", err)
	}
}

func TestValidateLibraryPathRejectsSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	allowed := mustDir(t, filepath.Join(base, "allowed"))
	outside := mustDir(t, filepath.Join(base, "outside"))
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// The link lives inside the allow root, but resolves outside it.
	assertErr(t, mustErr(ValidateLibraryPath([]string{allowed}, link)), domain.ErrPathEscapes)
}

func TestValidateMediaFileRules(t *testing.T) {
	base := t.TempDir()
	root := mustDir(t, filepath.Join(base, "media"))
	outside := mustDir(t, filepath.Join(base, "outside"))
	allow := []string{root}

	ok := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(ok, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateMediaFile(allow, root, ok); err != nil {
		t.Fatalf("库内文件应通过: %v", err)
	}

	outFile := filepath.Join(outside, "secret.mp4")
	if err := os.WriteFile(outFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertErr(t, mustErr(ValidateMediaFile(allow, root, outFile)), domain.ErrPathNotAllowed)

	link := filepath.Join(root, "link.mp4")
	if err := os.Symlink(outFile, link); err != nil {
		t.Fatal(err)
	}
	assertErr(t, mustErr(ValidateMediaFile(allow, root, link)), domain.ErrPathEscapes)

	assertErr(t, mustErr(ValidateMediaFile(allow, root, ok+"\x00")), domain.ErrPathNullByte)
	assertErr(t, mustErr(ValidateMediaFile(allow, root, root+"/sub/../x.mp4")), domain.ErrPathNotClean)
	assertErr(t, mustErr(ValidateMediaFile(allow, root, "relative.mp4")), domain.ErrPathNotAbsolute)
	assertErr(t, mustErr(ValidateMediaFile(allow, root, "/etc/passwd")), domain.ErrPathNotAllowed)
}

// TestValidateMediaFileAcceptsRootStoredAsRealPath is the /tmp regression: the
// allow root is stored resolved (/private/tmp/...) while WalkDir yields the
// symlinked spelling, so both comparisons must be tried (真机扫出 0 个文件的坑).
func TestValidateMediaFileAcceptsRootStoredAsRealPath(t *testing.T) {
	// Deliberately under /tmp so the symlinked spelling really differs from the
	// resolved one on macOS (/tmp -> /private/tmp).
	base, err := os.MkdirTemp("/tmp", "zv-alias-")
	if err != nil {
		t.Skipf("无法在 /tmp 下建夹具: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	if realBase == base {
		t.Skip("/tmp 不是软链接，无法复现该坑")
	}
	realRoot := mustDir(t, filepath.Join(realBase, "fixtures"))
	aliasRoot := filepath.Join(base, "fixtures")
	mustDir(t, aliasRoot)
	realFile := filepath.Join(realRoot, "clip.mp4")
	aliasFile := filepath.Join(aliasRoot, "clip.mp4")
	if err := os.WriteFile(realFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The allow root is stored resolved; WalkDir yields the alias spelling.
	if _, err := ValidateMediaFile([]string{realRoot}, aliasRoot, aliasFile); err != nil {
		t.Fatalf("越界误报: %v", err)
	}
	if _, err := ValidateMediaFile([]string{realRoot}, aliasRoot, realFile); err != nil {
		t.Fatalf("真实路径应通过: %v", err)
	}
	// A genuinely outside file must still be refused.
	outside := mustDir(t, filepath.Join(realBase, "outside"))
	outFile := filepath.Join(outside, "secret.mp4")
	if err := os.WriteFile(outFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertErr(t, mustErr(ValidateMediaFile([]string{realRoot}, aliasRoot, outFile)), domain.ErrPathNotAllowed)
}

func TestIgnoreRules(t *testing.T) {
	rules := DefaultIgnoreRules
	for _, name := range []string{".hidden", "@eaDir", "#recycle", ".DS_Store",
		"lost+found", "x.part", "y.!bt", "~$doc"} {
		if !ignored(name, rules) {
			t.Fatalf("%s 应被忽略", name)
		}
	}
	for _, name := range []string{"clip.mp4", "clip.mov", "holiday.MP4"} {
		if ignored(name, rules) {
			t.Fatalf("%s 不应被忽略", name)
		}
	}
	if !ignored("custom.tmp", []string{"*.tmp"}) {
		t.Fatal("库自定义规则应生效")
	}
}

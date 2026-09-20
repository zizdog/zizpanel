package config

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustWriteConfig(t *testing.T, path string, body string) []byte {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return []byte(body)
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAllowRootWriteIsAtomicAndReread covers the persistence contract: a valid
// add lands in config.json, survives a fresh read, and leaves 0600 + no temp file.
func TestAllowRootWriteIsAtomicAndReread(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	extra := filepath.Join(dir, "video")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteConfig(t, cfgPath, `{"listen":"127.0.0.1:7766","media_allow_roots":["/tmp"]}`+"\n")

	roots := NewRoots(cfgPath, []string{"/tmp"})
	if _, err := roots.Add(extra); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	back, err := RootsFromFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[1] != extra {
		t.Fatalf("回读 = %v, 期望包含 %s", back, extra)
	}
	cfg, _, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("写回后配置应仍可加载: %v", err)
	}
	if len(cfg.MediaAllowRoots) != 2 {
		t.Fatalf("重新加载后白名单 = %v", cfg.MediaAllowRoots)
	}
	if got := roots.List(); len(got) != 2 || got[1] != extra {
		t.Fatalf("内存快照 = %v", got)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config.json 权限 = %o, 期望 600", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".zizvideo-config-") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}

// TestRejectedRootLeavesConfigUntouched is the "config.json 一字未变" gate.
func TestRejectedRootLeavesConfigUntouched(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	media := filepath.Join(dir, "media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(media, "movie.mp4")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(media, "away")
	if err := os.Symlink("/private/tmp", link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(media, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	before := mustWriteConfig(t, cfgPath,
		`{"media_allow_roots":["`+media+`"],"scan_workers":4}`+"\n")

	bad := []struct {
		name string
		path string
	}{
		{"relative", "media/x"},
		{"missing", filepath.Join(dir, "nope")},
		{"file not dir", filePath},
		{"dirty", media + string(filepath.Separator)},
		{"null byte", media + "\x00x"},
		{"empty", ""},
		{"dangling symlink", dangling},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateAllowRoot(tc.path)
			if err == nil {
				t.Fatalf("应拒绝 %q, 却得到 %q", tc.path, got)
			}
			if now := readBytes(t, cfgPath); string(now) != string(before) {
				t.Fatalf("被拒后 config.json 被改动:\n%s\n%s", before, now)
			}
		})
	}
	// A symlink is stored as its real target, so it can never smuggle an
	// unapproved directory into the list as a different-looking path.
	if got, err := ValidateAllowRoot(link); err != nil || got != "/private/tmp" {
		t.Fatalf("软链接解析 = %q, %v", got, err)
	}
	// The failing paths must also be refused by the writer itself.
	for _, p := range []string{"relative", filepath.Join(dir, "nope"), filePath} {
		if err := SetRoots(cfgPath, []string{media, p}); err == nil {
			t.Fatalf("SetRoots 不应接受 %q", p)
		}
	}
	if now := readBytes(t, cfgPath); string(now) != string(before) {
		t.Fatalf("SetRoots 失败后文件被改动: %s", now)
	}
}

// TestSetRootsPreservesOtherFields checks the one-key rewrite contract.
func TestSetRootsPreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "listen": "127.0.0.1:7766",
  "media_allow_roots": ["/tmp"],
  "media_extensions": ["mp4", "mkv"],
  "scan_workers": 2,
  "secure_cookie": false
}
`
	mustWriteConfig(t, path, body)

	// 夹具只用临时目录：指向真实外置盘时，盘一摘这个测试就红（环境依赖，非回归；坑 209）。
	second := t.TempDir()
	if err := SetRoots(path, []string{"/tmp", second}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatalf("写回后必须仍可加载: %v", err)
	}
	if cfg.ScanWorkers != 2 || len(cfg.MediaExtensions) != 2 || cfg.SecureCookie {
		t.Fatalf("其它字段被改动: %+v", cfg)
	}
	if len(cfg.MediaAllowRoots) != 2 || cfg.MediaAllowRoots[1] != second {
		t.Fatalf("白名单 = %v", cfg.MediaAllowRoots)
	}
	var raw map[string]json.RawMessage
	raw2 := readBytes(t, path)
	if err := json.Unmarshal(raw2, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["listen"]) != `"127.0.0.1:7766"` {
		t.Fatalf("listen 被重写: %s", raw["listen"])
	}
}

func TestRemoveRefusesMissingAndLastRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	extra := filepath.Join(dir, "extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteConfig(t, path, `{"media_allow_roots":["/tmp","`+extra+`"]}`+"\n")
	roots := NewRoots(path, []string{"/tmp", extra})

	if _, err := roots.Remove(extra); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	if back, _ := RootsFromFile(path); len(back) != 1 || back[0] != "/tmp" {
		t.Fatalf("回读 = %v", back)
	}
	if _, err := roots.Remove(extra); err == nil {
		t.Fatal("删不存在的根必须报错")
	}
	if _, err := roots.Remove("/tmp"); err == nil {
		t.Fatal("删最后一个根必须报错")
	}
	if back, _ := RootsFromFile(path); len(back) != 1 || back[0] != "/tmp" {
		t.Fatalf("失败后文件被改动: %v", back)
	}
}

func TestValidateAllowRootAcceptsRealDirAndResolvesAlias(t *testing.T) {
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	got, err := ValidateAllowRoot(dir)
	if err != nil {
		t.Fatalf("合法目录被拒: %v", err)
	}
	if got != dir {
		t.Fatalf("规范化结果 = %q, 期望 %q", got, dir)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := ValidateAllowRoot(link)
	if err != nil {
		t.Fatalf("指向真实目录的软链接应被接受: %v", err)
	}
	if resolved != dir {
		t.Fatalf("软链接应解析为真实路径 %q, 得到 %q", dir, resolved)
	}
	// /tmp on macOS is an alias of /private/tmp; both spellings must validate.
	if _, err := os.Stat("/tmp"); err == nil {
		if _, err := ValidateAllowRoot("/tmp"); err != nil {
			t.Fatalf("/tmp 应被接受: %v", err)
		}
	}
}

func TestUnwritableConfigIsReported(t *testing.T) {
	dir := t.TempDir()
	roots := NewRoots(filepath.Join(dir, "missing.json"), []string{"/tmp"})
	if _, err := roots.Add("/private/tmp"); err == nil {
		t.Fatal("配置文件不存在时必须如实报错，不能假装成功")
	}
	if !roots.FileBacked() {
		t.Fatal("FileBacked 应为 true（路径非空）")
	}
}

// TestTestFixturesAvoidRealVolumes 锁住"夹具不许指向真实外置卷"：盘一摘 make check 就红，
// 属环境依赖而非回归（坑 209）。外置卷可达性只能在真机验证，不进单测。
func TestTestFixturesAvoidRealVolumes(t *testing.T) {
	needle := []byte("/Vol" + "umes/")
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, needle) {
			t.Errorf("%s 引用了真实外置卷路径，夹具请改用 t.TempDir()", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

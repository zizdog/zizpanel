package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/storage"
)

// buildBinary compiles the real binary once per test run.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "zizvideo")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build 失败: %v\n%s", err, out)
	}
	return bin
}

func runBin(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("运行失败: %v", err)
		}
	}
	return stdout.String(), stderr.String(), code
}

func asExitError(err error, dst **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*dst = ee
		return true
	}
	return false
}

func TestVersionAndHelpUseNewName(t *testing.T) {
	bin := buildBinary(t)

	stdout, _, code := runBin(t, bin, nil, "--version")
	if code != 0 {
		t.Fatalf("--version 退出码 = %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "zizvideo ") {
		t.Fatalf("--version 输出 = %q", stdout)
	}
	if strings.Contains(strings.ToLower(stdout), "govideo") {
		t.Fatalf("--version 仍含旧名: %q", stdout)
	}

	// --help exits 0 only for -h; flag prints usage to stderr and exits 2.
	_, stderr, _ := runBin(t, bin, nil, "--help")
	usage := stderr
	if !strings.Contains(usage, "zizvideo") {
		t.Fatalf("--help 未提到新名字: %q", usage)
	}
	if !strings.Contains(usage, "roots list") {
		t.Fatalf("--help 未提到 roots 子命令: %q", usage)
	}
	if strings.Contains(usage, "govideo") || strings.Contains(usage, "GV_") {
		t.Fatalf("--help 仍含旧名/旧前缀: %q", usage)
	}
}

func TestRootsCLIReadsAndWritesConfig(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	cfgPath := filepath.Join(dir, "config.json")
	// extra must be a sibling of the base root: a child would be "covered".
	base := filepath.Join(dir, "base")
	extra := filepath.Join(dir, "video")
	dataDir := filepath.Join(dir, "data")
	for _, d := range []string{base, extra, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// data_dir/database_path are pinned to the temp dir on purpose: without
	// them the subprocess would fall back to the real user home (坑 3).
	body := `{"media_allow_roots":["` + base + `"],"scan_workers":3,` +
		`"data_dir":"` + dataDir + `",` +
		`"database_path":"` + filepath.Join(dataDir, "zizvideo.db") + `"}` + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		Action     string   `json:"action"`
		OK         bool     `json:"ok"`
		Path       string   `json:"path"`
		Roots      []string `json:"roots"`
		ConfigPath string   `json:"config_path"`
		Used       []string `json:"in_use"`
		Error      string   `json:"error"`
	}
	parse := func(t *testing.T, out string) answer {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 1 {
			t.Fatalf("期望一行 JSON, 得到 %q", out)
		}
		var a answer
		if err := json.Unmarshal([]byte(lines[0]), &a); err != nil {
			t.Fatalf("解析输出失败: %v (%s)", err, out)
		}
		return a
	}

	stdout, _, code := runBin(t, bin, nil, "roots", "list", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("list 退出码 = %d: %s", code, stdout)
	}
	if a := parse(t, stdout); a.Action != "list" || len(a.Roots) != 1 {
		t.Fatalf("list 输出 = %+v", a)
	}

	stdout, _, code = runBin(t, bin, nil, "--config", cfgPath, "roots", "add", extra)
	if code != 0 {
		t.Fatalf("add 退出码 = %d: %s", code, stdout)
	}
	if a := parse(t, stdout); !a.OK || a.Path != extra || len(a.Roots) != 2 {
		t.Fatalf("add 输出 = %+v", a)
	}
	// The file must really hold it.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), extra) {
		t.Fatalf("config.json 未包含新根: %s", raw)
	}

	// ZV_CONFIG is honoured when --config is absent.
	env := []string{"ZV_CONFIG=" + cfgPath}
	stdout, _, code = runBin(t, bin, env, "roots", "list")
	if code != 0 || !strings.Contains(stdout, extra) {
		t.Fatalf("ZV_CONFIG 未生效: code=%d out=%s", code, stdout)
	}

	// Bad paths are refused and the file is untouched.
	before, _ := os.ReadFile(cfgPath)
	for _, bad := range []string{"relative/path", filepath.Join(dir, "ghost")} {
		if _, _, code := runBin(t, bin, env, "roots", "add", bad); code == 0 {
			t.Fatalf("CLI 应拒绝 %q", bad)
		}
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Fatalf("被拒后 config.json 被改动:\n%s\n%s", before, after)
	}

	stdout, _, code = runBin(t, bin, env, "roots", "remove", extra)
	if code != 0 {
		t.Fatalf("remove 退出码 = %d: %s", code, stdout)
	}
	if a := parse(t, stdout); !a.OK || len(a.Roots) != 1 {
		t.Fatalf("remove 输出 = %+v", a)
	}

	// Removing a root that a library uses is refused and names the library.
	libs := `{"media_allow_roots":["` + extra + `","` + base + `"],` +
		`"data_dir":"` + dataDir + `",` +
		`"database_path":"` + filepath.Join(dataDir, "zizvideo.db") + `"}`
	if err := os.WriteFile(cfgPath, []byte(libs), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(filepath.Join(dataDir, "zizvideo.db"))
	if err != nil {
		t.Fatal(err)
	}
	lib := &domain.Library{ID: domain.NewID("lib"), Name: "外接盘库",
		RootPath: extra, Recursive: true, Enabled: true, IgnoreRules: []string{}}
	if err := db.CreateLibrary(lib); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	libOut, libErr, code := runBin(t, bin, env, "roots", "remove", extra)
	if code != 0 {
		t.Fatalf("拒绝删除也应是一行 JSON 结果: code=%d %s", code, libErr)
	}
	refused := parse(t, libOut)
	if refused.OK || refused.Action != "remove" || len(refused.Used) != 1 || refused.Used[0] != "外接盘库" {
		t.Fatalf("拒绝输出 = %+v", refused)
	}
	if refused.Error == "" {
		t.Fatal("拒绝时必须给出原因")
	}
	kept, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(kept), extra) {
		t.Fatalf("拒绝后 config.json 不应改动: %s", kept)
	}
}

func TestRootsCLIWithoutConfigFailsHonestly(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	_, stderr, code := runBin(t, bin,
		[]string{"ZV_CONFIG=", "HOME=" + dir, "TMPDIR=" + dir}, "roots", "list")
	if code == 0 {
		t.Fatal("没有配置时必须失败，不能猜一个默认值")
	}
	if !strings.Contains(stderr, "ZV_CONFIG") && !strings.Contains(stderr, "--config") {
		t.Fatalf("错误信息应说明如何指定配置: %q", stderr)
	}
}

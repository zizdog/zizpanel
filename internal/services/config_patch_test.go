package services

// config_patch_test.go —— 声明式配置补丁在**安装路径**上的接线（C1 的缺口）。
//
// 纪律：这里只碰 t.TempDir() 里的文件，绝不读写真机配置、绝不跑 brew。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/plugins"
)

func TestApplyConfigPatchStepWritesAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	etc := filepath.Join(brew, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(etc, "app.ini")
	orig := "[server]\n; http_port = 3000\nhttp_port = 3000\n\n[auth]\nenabled = false\n"
	if err := os.WriteFile(cfg, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{opt: Options{BrewBin: filepath.Join(brew, "bin", "brew")}}
	app := App{
		ID: "patchtest", Name: "补丁测试", Kind: KindNative, BrewFormula: "patchtest",
		ConfigPath: "{brew}/etc/app.ini",
		ConfigPatch: &plugins.Patch{
			Format: plugins.PatchINI, Section: "server",
			Set: map[string]string{"http_port": "3002"},
		},
	}
	res := &InstallResult{App: app.ID, Name: app.Name}
	m.applyConfigPatchStep(context.Background(), app, res)

	got, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "http_port = 3002") {
		t.Errorf("补丁没写进去：\n%s", got)
	}
	if !strings.Contains(string(got), "; http_port = 3000") {
		t.Errorf("被注释掉的那行不该被动：\n%s", got)
	}
	if !strings.Contains(string(got), "enabled = false") {
		t.Errorf("别的 section 不该被动：\n%s", got)
	}
	// 备份必须留的是**改动前**的内容
	backup, err := os.ReadFile(cfg + ".zizpanel.bak")
	if err != nil {
		t.Fatalf("应当留一份备份：%v", err)
	}
	if string(backup) != orig {
		t.Errorf("备份内容应当是改动前的原文：\n%s", backup)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "配置补丁已应用") {
		t.Errorf("步骤里要如实写出补丁动作：%v", res.Steps)
	}
	if res.Warning != "" {
		t.Errorf("顺利路径不该有告警：%s", res.Warning)
	}

	// 幂等：再来一次不该改文件、也不该把备份覆盖成"改过之后"的版本
	res2 := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res2)
	after, _ := os.ReadFile(cfg)
	if string(after) != string(got) {
		t.Errorf("第二次应用后内容变了：\n%s", after)
	}
	backup2, _ := os.ReadFile(cfg + ".zizpanel.bak")
	if string(backup2) != orig {
		t.Error("第二次应用把备份覆盖掉了（备份必须一直是改动前的原文）")
	}
}

// TestApplyConfigPatchStepSkipsMissingFile 文件不存在且未声明 create 时，必须**如实跳过**，
// 不许抢着写一个"最小配置"（那会把应用的默认值全抹掉）。
func TestApplyConfigPatchStepSkipsMissingFile(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	if err := os.MkdirAll(filepath.Join(brew, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{opt: Options{BrewBin: filepath.Join(brew, "bin", "brew")}}
	path := filepath.Join(brew, "etc", "not-yet.conf")
	app := App{
		ID: "pending", Name: "还没生成配置", Kind: KindNative,
		ConfigPath:  "{brew}/etc/not-yet.conf",
		ConfigPatch: &plugins.Patch{Set: map[string]string{"bind": "0.0.0.0"}},
	}
	res := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res)
	if fileExistsAt(path) {
		t.Error("默认策略下不该新建配置文件")
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "跳过") {
		t.Errorf("跳过必须写进步骤（如实说明）：%v", res.Steps)
	}
	if res.Warning != "" {
		t.Errorf("可预期的跳过不该算告警：%s", res.Warning)
	}

	// 声明了 create 才新建（且只包含声明的键）
	app.ConfigPatch.IfMissing = "create"
	res2 := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res2)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("声明 create 后应当新建文件：%v", err)
	}
	if strings.TrimSpace(string(b)) != "bind = 0.0.0.0" {
		t.Errorf("新建内容只该有声明的键：%q", string(b))
	}
}

// TestApplyConfigPatchStepReportsFailure 写不进去必须进 Warning（不许谎报"已按声明配置好"）。
func TestApplyConfigPatchStepReportsFailure(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	etc := filepath.Join(brew, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(etc, "app.conf")
	if err := os.WriteFile(cfg, []byte("a = 1\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	// 只读文件 + 只读目录：写入必然失败（root 下会绕过权限，所以 root 环境跳过这一条）
	if os.Geteuid() == 0 {
		t.Skip("以 root 跑测试时文件权限挡不住写入，这个用例没有意义")
	}
	if err := os.Chmod(etc, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(etc, 0o755) })

	m := &Manager{opt: Options{BrewBin: filepath.Join(brew, "bin", "brew")}}
	app := App{
		ID: "readonly", Kind: KindNative, ConfigPath: "{brew}/etc/app.conf",
		ConfigPatch: &plugins.Patch{Set: map[string]string{"a": "2"}},
	}
	res := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res)
	if res.Warning == "" {
		t.Fatal("写失败必须如实进 Warning")
	}
	if !strings.Contains(res.Warning, "配置补丁没生效") {
		t.Errorf("告警要说清是补丁没生效：%s", res.Warning)
	}
	if strings.Contains(strings.Join(res.Steps, "\n"), "配置补丁已应用") {
		t.Error("失败时绝不能写「已应用」")
	}
}

// TestConfigPatchFlowsFromSpecToApp 声明 → App 的接线：本地插件/内建表都要能带上补丁。
func TestConfigPatchFlowsFromSpecToApp(t *testing.T) {
	spec, err := plugins.Parse([]byte(`{
	  "schema": "zizpanel.app/v1", "id": "patchapp", "name": "补丁应用", "icon": "🧪",
	  "source": {"kind": "brew", "formula": "patchapp", "checksum": "sha256"},
	  "run": {"mode": "brew-service"},
	  "config": {"path": "{brew}/etc/patchapp.conf", "format": "kv",
	             "set": {"bind": "0.0.0.0", "port": "7700"}},
	  "verify": {"any_of": [{"kind": "http", "path": "/health", "timeout": "30s"}]},
	  "health": {"kind": "http", "path": "/health"},
	  "uninstall": {"optional_data": ["{brew}/var/patchapp"], "formula": "patchapp"}
	}`), "patchapp.json")
	if err != nil {
		t.Fatalf("声明应当合法：%v", err)
	}
	app, err := SpecToApp(spec)
	if err != nil {
		t.Fatal(err)
	}
	if app.ConfigPatch == nil || app.ConfigPatch.Set["port"] != "7700" {
		t.Fatalf("补丁没有从声明流到 App：%+v", app.ConfigPatch)
	}
	// 值必须是**拷贝**：改 App 上的补丁不该污染声明（否则界面/日志会跟着漂）
	app.ConfigPatch.Set["port"] = "1"
	if spec.Config.Set["port"] != "7700" {
		t.Error("App 与声明共享了同一个 map（必须拷贝）")
	}
}

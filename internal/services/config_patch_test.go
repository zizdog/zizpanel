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
		ConfigPatches: []plugins.Patch{{
			Format: plugins.PatchINI, Section: "server",
			Set: map[string]string{"http_port": "3002"},
		}},
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
		ConfigPath: "{brew}/etc/not-yet.conf",
		// 声明了随机口令也一样：文件不存在又没写 create ⇒ **不生成、不进凭据区**
		// （否则用户会拿着一个根本不在配置里的口令去登录）。
		ConfigPatches: []plugins.Patch{{Set: map[string]string{"bind": "0.0.0.0"}, Secrets: []string{"token"}}},
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
	if len(res.Credentials) != 0 {
		t.Errorf("跳过时绝不能给出凭据（那个口令没被写进任何文件）：%+v", res.Credentials)
	}

	// 声明了 create 才新建（且只包含声明的键）
	app.ConfigPatches[0].IfMissing = "create"
	res2 := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res2)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("声明 create 后应当新建文件：%v", err)
	}
	if !strings.Contains(string(b), "bind = 0.0.0.0") {
		t.Errorf("新建内容必须包含声明的键：%q", string(b))
	}
	if len(res2.Credentials) != 1 || !strings.Contains(string(b), "token = "+res2.Credentials[0].Value) {
		t.Errorf("create 分支生成的随机口令必须真的写进新文件：%q / %+v", string(b), res2.Credentials)
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
		ConfigPatches: []plugins.Patch{{
			Set: map[string]string{"a": "2"}, Secrets: []string{"token"},
		}},
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
	// 写失败时刚生成的口令根本没进配置 —— 绝不能留在凭据区让用户拿着它去登录。
	if len(res.Credentials) != 0 {
		t.Errorf("写失败后凭据区必须清空，实际 %+v", res.Credentials)
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
	if len(app.ConfigPatches) != 1 || app.ConfigPatches[0].Set["port"] != "7700" {
		t.Fatalf("补丁没有从声明流到 App：%+v", app.ConfigPatches)
	}
	// 值必须是**拷贝**：改 App 上的补丁不该污染声明（否则界面/日志会跟着漂）
	app.ConfigPatches[0].Set["port"] = "1"
	if spec.Config.Set["port"] != "7700" {
		t.Error("App 与声明共享了同一个 map（必须拷贝）")
	}
}

// TestConfigPatchSecrets 随机口令的三条语义：
//
//	① 文件里没有 → 生成、写进去、进凭据区（且只在这里出现一次）；
//	② 已有非空值 → **复用，不轮换**（重装一次换一次口令 = 把应用弄坏）；
//	③ 写失败 → 凭据区必须清空（用户不该拿到一个不在配置里的口令）。
func TestConfigPatchSecrets(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	etc := filepath.Join(brew, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{opt: Options{BrewBin: filepath.Join(brew, "bin", "brew")}}
	app := App{
		ID: "couchlike", Kind: KindNative, ConfigPath: "{brew}/etc/app.ini",
		ConfigPatches: []plugins.Patch{{
			Format: plugins.PatchINI, Section: "admins", IfMissing: "create",
			Secrets: []string{"admin"},
		}},
	}
	cfg := filepath.Join(etc, "app.ini")

	// ① 首次：生成 + 写盘 + 凭据
	res := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res)
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("声明 create 后应当建出配置文件：%v", err)
	}
	if len(res.Credentials) != 1 || res.Credentials[0].Key != "admin" || res.Credentials[0].Value == "" {
		t.Fatalf("应当把随机口令带进凭据区：%+v", res.Credentials)
	}
	first := res.Credentials[0].Value
	if len(first) < 16 {
		t.Errorf("口令太短：%q", first)
	}
	if !strings.Contains(string(body), "admin = "+first) {
		t.Errorf("凭据里的口令必须真的写进了配置：\n%s", body)
	}

	// ② 再装一次：复用已有值，不轮换、不再出现在凭据区
	res2 := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res2)
	body2, _ := os.ReadFile(cfg)
	if !strings.Contains(string(body2), "admin = "+first) {
		t.Errorf("重装不该轮换口令：\n%s", body2)
	}
	if len(res2.Credentials) != 0 {
		t.Errorf("复用时不该再报一次凭据（用户已有）：%+v", res2.Credentials)
	}
	if !strings.Contains(strings.Join(res2.Steps, "\n"), "复用") {
		t.Errorf("复用时步骤里要写明：%v", res2.Steps)
	}

	// ③ 空值等于没设：应当重新生成（"admin = " 这种半成品配置必须补上口令）
	if err := os.WriteFile(cfg, []byte("[admins]\nadmin = \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res3 := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res3)
	if len(res3.Credentials) != 1 || res3.Credentials[0].Value == "" {
		t.Fatalf("空值应当重新生成口令：%+v", res3.Credentials)
	}
	body3, _ := os.ReadFile(cfg)
	if !strings.Contains(string(body3), "admin = "+res3.Credentials[0].Value) {
		t.Errorf("新口令必须写进配置：\n%s", body3)
	}
}

// TestConfigPatchMultiplePatches 一个文件里改多个 section（couchdb 那种形态）：
// 一次落盘、一次备份，两条补丁都生效。
func TestConfigPatchMultiplePatches(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	etc := filepath.Join(brew, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(etc, "local.ini")
	orig := "[chttpd]\n;bind_address = 127.0.0.1\n\n[admins]\n;admin = x\n"
	if err := os.WriteFile(cfg, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{opt: Options{BrewBin: filepath.Join(brew, "bin", "brew")}}
	app := App{
		ID: "couchdb", Kind: KindNative, ConfigPath: "{brew}/etc/local.ini",
		ConfigPatches: []plugins.Patch{
			{Format: plugins.PatchINI, Section: "chttpd", Set: map[string]string{"bind_address": "0.0.0.0"}},
			{Format: plugins.PatchINI, Section: "admins", Secrets: []string{"admin"}},
		},
	}
	res := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res)
	body, _ := os.ReadFile(cfg)
	if !strings.Contains(string(body), "bind_address = 0.0.0.0") {
		t.Errorf("第一条补丁没生效：\n%s", body)
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("第二条补丁应当生成口令：%+v", res.Credentials)
	}
	if !strings.Contains(string(body), "admin = "+res.Credentials[0].Value) {
		t.Errorf("第二条补丁没生效：\n%s", body)
	}
	// 只留一份备份，内容是改动前的原文
	backup, err := os.ReadFile(cfg + ".zizpanel.bak")
	if err != nil {
		t.Fatalf("应当留一份备份：%v", err)
	}
	if string(backup) != orig {
		t.Errorf("备份必须是改动前的原文：\n%s", backup)
	}
	// 幂等：再跑一次不该改文件，也不该再生成口令
	before, _ := os.ReadFile(cfg)
	res2 := &InstallResult{App: app.ID}
	m.applyConfigPatchStep(context.Background(), app, res2)
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Errorf("第二次跑不该改文件：\n%s", after)
	}
	if len(res2.Credentials) != 0 {
		t.Errorf("第二次跑不该再报凭据：%+v", res2.Credentials)
	}
}

// TestConfigPatchHonorsDeclaredFileMode 新建配置文件必须用**声明的权限**。
//
// 为什么值得一条门禁：code-server 的 config.yaml 里有随机访问口令，面板新建它时若写成
// 0644，等于把口令摊给这台机器上所有用户看。声明 0600 就必须真是 0600。
func TestConfigPatchHonorsDeclaredFileMode(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	if err := os.MkdirAll(filepath.Join(brew, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{opt: Options{BrewBin: filepath.Join(brew, "bin", "brew")}}
	path := filepath.Join(brew, "etc", "secret.conf")

	// ① 默认（没声明 mode）：按 0600 建（宁可用户改宽，也别面板替他把口令公开）
	app := App{
		ID: "sec1", Kind: KindNative, ConfigPath: "{brew}/etc/secret.conf",
		ConfigPatches: []plugins.Patch{{
			IfMissing: "create", Set: map[string]string{"token": "t"},
		}},
	}
	m.applyConfigPatchStep(context.Background(), app, &InstallResult{App: app.ID})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("默认应当是 0600，实际 %04o", got)
	}

	// ② 声明的 mode 被真的用上（0644 的配置文件：grafana.ini 那种不含口令的）
	path2 := filepath.Join(brew, "etc", "plain.conf")
	app2 := App{
		ID: "sec2", Kind: KindNative, ConfigPath: "{brew}/etc/plain.conf", ConfigMode: "0644",
		ConfigPatches: []plugins.Patch{{
			IfMissing: "create", Set: map[string]string{"port": "1"},
		}},
	}
	m.applyConfigPatchStep(context.Background(), app2, &InstallResult{App: app2.ID})
	fi2, err := os.Stat(path2)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi2.Mode().Perm(); got != 0o644 {
		t.Errorf("声明 0644 时应当是 0644，实际 %04o", got)
	}

	// ③ 已存在的文件：面板只改内容，**不动它的权限**（那是用户的文件）
	path3 := filepath.Join(brew, "etc", "mine.conf")
	if err := os.WriteFile(path3, []byte("port = 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	app3 := App{
		ID: "sec3", Kind: KindNative, ConfigPath: "{brew}/etc/mine.conf", ConfigMode: "0600",
		ConfigPatches: []plugins.Patch{{Set: map[string]string{"port": "2"}}},
	}
	m.applyConfigPatchStep(context.Background(), app3, &InstallResult{App: app3.ID})
	fi3, _ := os.Stat(path3)
	if got := fi3.Mode().Perm(); got != 0o640 {
		t.Errorf("已存在文件的权限不该被改，实际 %04o", got)
	}
}

// TestCodeServerConfigFromTableIsUsable 「表里的 code-server 声明」真的能生成可用的配置。
//
// 这条门禁的价值在"声明的**具体内容**"：引擎（ApplyPatch）早就测过了，但键名写错
// （bind-addr 写成 bind_addr）、路径写错、权限没声明，都不会被引擎发现 —— 而用户看到的
// 就是"装完了，8092 打不开"。所以这里直接拿目录里的 App（由内建表映射而来）跑一遍。
func TestCodeServerConfigFromTableIsUsable(t *testing.T) {
	app, ok := FindApp("code-server")
	if !ok {
		t.Fatal("目录里没有 code-server（插件表没加载？）")
	}
	home := t.TempDir()
	m := &Manager{opt: Options{UserHome: home, UserName: ""}}
	res := &InstallResult{App: app.ID, Name: app.Name}
	m.applyConfigPatchStep(context.Background(), app, res)

	// 官方文档里的路径：~/.config/code-server/config.yaml
	path := filepath.Join(home, ".config", "code-server", "config.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("应当生成配置文件 %s：%v（步骤：%v）", path, err, res.Steps)
	}
	cfg := string(b)
	// 官方四个键 + 面板的口径：监听所有网卡、开密码、不走 HTTPS（自签证书会被浏览器拦）
	for _, want := range []string{"bind-addr: 0.0.0.0:8092", "auth: password", "cert: false", "password: "} {
		if !strings.Contains(cfg, want) {
			t.Errorf("配置里应当有 %q：\n%s", want, cfg)
		}
	}
	// 口令必须与安装结果里给用户的那一个完全一致（否则用户拿着假口令登录）
	if len(res.Credentials) != 1 || !strings.Contains(cfg, "password: "+res.Credentials[0].Value) {
		t.Errorf("凭据区的口令必须就是配置里的那个：%+v\n%s", res.Credentials, cfg)
	}
	// 文件里有口令 ⇒ 权限必须是声明的 0600
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("含访问口令的配置必须是 0600，实际 %04o", got)
	}
	// 卡片端口必须与配置里监听的端口一致（不然健康检查永远打空）
	if app.Port != 8092 {
		t.Errorf("卡片端口应当是 8092（与 bind-addr 一致），实际 %d", app.Port)
	}
}

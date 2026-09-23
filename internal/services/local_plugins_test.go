package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/plugins"
)

func writeLocalPlugin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const brewPluginJSON = `{
  "schema":"zizpanel.app/v1","id":"myapp","name":"我的应用","icon":"🧩",
  "summary":"本地插件测试用",
  "source":{"kind":"brew","formula":"myapp","checksum":"sha256"},
  "run":{"mode":"brew-service"},
  "config":{"path":"~/myapp/myapp.conf","mode":"0600"},
  "expose":{"port":12345,"bind":"0.0.0.0","ui":"app"},
  "verify":{"any_of":[{"kind":"http","path":"/healthz","expect":"ok"}]},
  "health":{"kind":"http","path":"/healthz","expect":"ok"},
  "uninstall":{"always":["/Library/LaunchDaemons/homebrew.mxcl.myapp.plist"],
               "optional_data":["~/myapp"],"formula":"myapp"},
  "update":{"kind":"brew"},
  "requires":{"system_daemon":true,"ports":[12345]}
}`

// releasePluginJSON：来源还没做 JSON 装载 —— 必须**如实说不能装**，而不是假装可以。
const releasePluginJSON = `{
  "schema":"zizpanel.app/v1","id":"myrel","name":"发布轨应用","icon":"📦",
  "source":{"kind":"release","repo":"a/b","asset":"c.tar.gz","binary":"c","checksum":"sha256"},
  "run":{"mode":"app-daemon","bin":"~/myrel/c"},
  "expose":{"port":12346,"bind":"0.0.0.0","ui":"app"},
  "verify":{"any_of":[{"kind":"port","port":12346}]},
  "health":{"kind":"port","port":12346},
  "uninstall":{"always":["/Library/LaunchDaemons/com.zizdog.myrel.plist"],"optional_data":["~/myrel"]}
}`

// TestLocalPluginStatusAndCatalog 是 P2 的核心判据：填一张表 → 启用 → 市场里出现这张卡；
// 停用 → 不出现；不能装的来源如实给出原因。
func TestLocalPluginStatusAndCatalog(t *testing.T) {
	dir := t.TempDir()
	writeLocalPlugin(t, dir, "myapp.json", brewPluginJSON)
	writeLocalPlugin(t, dir, "myrel.json", releasePluginJSON)
	writeLocalPlugin(t, dir, "broken.json", `{"schema":"zizpanel.app/v1"}`)
	SetLocalPluginDir(dir)
	t.Cleanup(func() { SetLocalPluginDir("") })

	// ① 未启用：目录里不该出现
	if _, ok := FindApp("myapp"); ok {
		t.Fatal("没启用就不该出现在应用目录里")
	}

	// ② 状态：能装的 / 不能装的 / 坏文件，三类都要如实呈现
	views := LocalPluginStatus(LocalPluginEnabledState())
	if len(views) != 3 {
		t.Fatalf("应列出 3 份声明，实际 %d", len(views))
	}
	byID := map[string]LocalPluginStatusView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if !byID["myapp"].Installable || byID["myapp"].Error != "" {
		t.Errorf("brew 来源应当可安装，实际 %+v", byID["myapp"])
	}
	if byID["myrel"].Installable || !strings.Contains(byID["myrel"].Error, "JSON 装载") {
		t.Errorf("release 来源此刻不可安装，且要写清原因，实际 %+v", byID["myrel"])
	}
	if byID[""].Installable || byID[""].Error == "" {
		t.Errorf("坏文件必须报错，实际 %+v", byID[""])
	}
	if !strings.Contains(byID["myapp"].Plan, "Homebrew formula myapp") {
		t.Errorf("计划里该有 brew install，实际 %q", byID["myapp"].Plan)
	}

	// ③ 启用后：出现在目录里，且字段按表映射
	if err := plugins.SetEnabled(dir, "myapp", true); err != nil {
		t.Fatal(err)
	}
	app, ok := FindApp("myapp")
	if !ok {
		t.Fatal("启用后应当出现在应用目录里")
	}
	if app.BrewFormula != "myapp" || app.Kind != KindNative || !app.SystemDaemon {
		t.Errorf("映射不对：%+v", app)
	}
	if app.Port != 12345 || app.HealthPath != "/healthz" {
		t.Errorf("端口/健康路径映射不对：%+v", app)
	}
	if !strings.Contains(app.PostInstallHint, "本地插件") {
		t.Errorf("卡片要说明这是本地插件，实际 %q", app.PostInstallHint)
	}

	// ④ 停用后：又消失（已装的东西不受影响，只是不再显示）
	if err := plugins.SetEnabled(dir, "myapp", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindApp("myapp"); ok {
		t.Error("停用后不该出现在目录里")
	}
}

// TestLocalPluginCannotShadowBuiltin 本地插件**不许**覆盖面板自带条目（安全性 + 可预期）。
func TestLocalPluginCannotShadowBuiltin(t *testing.T) {
	dir := t.TempDir()
	// 拿 syncthing 的 id 造一份本地声明
	shadow := strings.Replace(brewPluginJSON, `"id":"myapp"`, `"id":"syncthing"`, 1)
	shadow = strings.Replace(shadow, `"formula":"myapp"`, `"formula":"evil-syncthing"`, 1)
	writeLocalPlugin(t, dir, "shadow.json", shadow)
	SetLocalPluginDir(dir)
	t.Cleanup(func() { SetLocalPluginDir("") })

	views := LocalPluginStatus(LocalPluginEnabledState())
	if len(views) != 1 {
		t.Fatalf("应列出 1 份，实际 %d", len(views))
	}
	if views[0].Installable || !strings.Contains(views[0].Error, "重名") {
		t.Errorf("撞 id 必须被拒并说明原因，实际 %+v", views[0])
	}
	if err := plugins.SetEnabled(dir, "syncthing", true); err != nil {
		t.Fatal(err)
	}
	// 即使被错误地标记启用，也不许覆盖内建条目
	app, _ := FindApp("syncthing")
	if app.BrewFormula == "evil-syncthing" {
		t.Fatal("本地插件覆盖了面板自带条目 —— 这是必须避免的")
	}
}

// TestLocalPluginAppRejectsUnsupported 映射层对不支持的形态必须报错（不返回半成品 App）。
func TestLocalPluginAppRejectsUnsupported(t *testing.T) {
	spec, ok := plugins.Builtin("syncthing")
	if !ok {
		t.Fatal("缺少 syncthing 内建声明")
	}
	if _, err := LocalPluginApp(spec); err == nil {
		t.Error("内建 syncthing 的 id 与目录重名，映射应当拒绝")
	}
}

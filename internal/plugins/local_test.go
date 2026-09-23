package plugins_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizpanel/internal/plugins"
)

const goodPlugin = `{
  "schema":"zizpanel.app/v1","id":"myapp","name":"我的应用","icon":"🧩",
  "source":{"kind":"brew","formula":"myapp","checksum":"sha256"},
  "run":{"mode":"brew-service"},
  "expose":{"port":12345,"bind":"0.0.0.0","ui":"app"},
  "verify":{"any_of":[{"kind":"http","path":"/healthz","expect":"ok"}]},
  "health":{"kind":"http","path":"/healthz","expect":"ok"},
  "uninstall":{"always":["/Library/LaunchDaemons/homebrew.mxcl.myapp.plist"],
               "optional_data":["~/myapp"],"formula":"myapp"}
}`

// writePlugin 在目录里写一份声明。
func writePlugin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadDirReportsInvalidInsteadOfHiding 坏文件必须**列出来**（带原因），不许静默忽略：
// 用户放了一份声明却"什么都不发生"是最难查的形态。
func TestLoadDirReportsInvalidInsteadOfHiding(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "ok.json", goodPlugin)
	writePlugin(t, dir, "bad-json.json", `{not json`)
	writePlugin(t, dir, "bad-spec.json", `{"schema":"zizpanel.app/v1","id":"x","name":"X","icon":"x",
	  "source":{"kind":"brew"},"run":{"mode":"brew-service"},
	  "verify":{"any_of":[{"kind":"port","port":1}]},"health":{"kind":"port","port":1},
	  "uninstall":{"always":["~/x"]}}`) // 缺 formula
	// enabled.json 不是插件，不能被当成插件列出来
	if err := plugins.SetEnabled(dir, "ok", true); err != nil {
		t.Fatal(err)
	}

	got := plugins.LoadDir(dir)
	if len(got) != 3 {
		t.Fatalf("应当列出 3 份声明（enabled.json 除外），实际 %d：%+v", len(got), got)
	}
	var okCount, errCount int
	for _, e := range got {
		if e.Err != "" {
			errCount++
			continue
		}
		okCount++
		if e.ID != "myapp" {
			t.Errorf("合法那份的 id 应为 myapp，实际 %q", e.ID)
		}
	}
	if okCount != 1 || errCount != 2 {
		t.Errorf("应当 1 份合法、2 份报错，实际 %d/%d", okCount, errCount)
	}
}

// TestEnabledStateRoundTrip 启用状态是显式动作、默认关闭、只有被启用的才算数。
func TestEnabledStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "myapp.json", goodPlugin)
	if s := plugins.ReadEnabled(dir); len(s) != 0 {
		t.Errorf("没有 enabled.json 时应视为全部关闭，实际 %v", s)
	}
	if err := plugins.SetEnabled(dir, "myapp", true); err != nil {
		t.Fatal(err)
	}
	if !plugins.ReadEnabled(dir)["myapp"] {
		t.Error("启用后应当读到 true")
	}
	// 状态文件的权限：它透露本机跑了什么
	info, err := os.Stat(filepath.Join(dir, plugins.EnabledFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("enabled.json 权限应为 0600，实际 %o", info.Mode().Perm())
	}
	if err := plugins.SetEnabled(dir, "myapp", false); err != nil {
		t.Fatal(err)
	}
	if plugins.ReadEnabled(dir)["myapp"] {
		t.Error("停用后应当读不到")
	}
}

// TestLoadDirMissingIsEmpty 目录不存在 = 没装插件，是正常状态（不许报错、不许 panic）。
func TestLoadDirMissingIsEmpty(t *testing.T) {
	if got := plugins.LoadDir(filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
		t.Errorf("目录不存在应返回空列表，实际 %+v", got)
	}
	if got := plugins.LoadDir(""); len(got) != 0 {
		t.Errorf("空目录参数应返回空列表，实际 %+v", got)
	}
}

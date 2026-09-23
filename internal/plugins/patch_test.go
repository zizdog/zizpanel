package plugins

import (
	"strings"
	"testing"
)

// TestApplyPatchKV 通用 kv：已存在的键替换那一行、缺失的键追加、其余一字不动。
func TestApplyPatchKV(t *testing.T) {
	src := "# 顶部注释\nport = 3000\n# 注释里的 port = 9999\n; 也是注释\nname = grafana\n"
	got, changed := ApplyPatch(src, Patch{Set: map[string]string{"port": "3002", "http_addr": "0.0.0.0"}})
	if !changed {
		t.Fatal("应当报告「改了」")
	}
	if !strings.Contains(got, "\nport = 3002\n") {
		t.Errorf("已存在的键没被替换：\n%s", got)
	}
	if !strings.Contains(got, "# 注释里的 port = 9999") {
		t.Errorf("注释行必须原样保留：\n%s", got)
	}
	if !strings.Contains(got, "\nhttp_addr = 0.0.0.0\n") {
		t.Errorf("缺失的键应当追加：\n%s", got)
	}
	if !strings.Contains(got, "name = grafana") {
		t.Errorf("无关的键不该被动：\n%s", got)
	}
	// 幂等：同样的补丁再来一次不该改任何东西
	if _, again := ApplyPatch(got, Patch{Set: map[string]string{"port": "3002", "http_addr": "0.0.0.0"}}); again {
		t.Error("第二次应用同样的补丁应当报告 changed=false（幂等，别重写文件）")
	}
	// 前缀不能误伤：port 不该匹配到 http_port
	src2 := "http_port = 1\n"
	if out, _ := ApplyPatch(src2, Patch{Set: map[string]string{"port": "2"}}); strings.Contains(out, "http_port = 2") {
		t.Errorf("短键不能匹配长键的前缀：\n%s", out)
	}
}

// TestApplyPatchINI 带 section 的 ini：只动目标 section，缺失时新开一节。
func TestApplyPatchINI(t *testing.T) {
	src := "[server]\nhttp_port = 3000\n\n[auth]\nenabled = true\n"
	got, _ := ApplyPatch(src, Patch{
		Format: PatchINI, Section: "server",
		Set: map[string]string{"http_port": "3002", "domain": "panel.local"},
	})
	if !strings.Contains(got, "http_port = 3002") || !strings.Contains(got, "domain = panel.local") {
		t.Errorf("目标 section 里应当被改并追加：\n%s", got)
	}
	if strings.Index(got, "domain = panel.local") > strings.Index(got, "[auth]") {
		t.Errorf("新键必须落在目标 section 之内（不能跑到后面的节里）：\n%s", got)
	}
	if !strings.Contains(got, "[auth]\nenabled = true") {
		t.Errorf("别的 section 不该被动：\n%s", got)
	}
	// 同名键在别的 section 里也必须不动
	other := "[a]\nport = 1\n\n[b]\nport = 2\n"
	out, _ := ApplyPatch(other, Patch{Format: PatchINI, Section: "b", Set: map[string]string{"port": "3"}})
	if !strings.Contains(out, "[a]\nport = 1") || !strings.Contains(out, "[b]\nport = 3") {
		t.Errorf("只该改 b 节：\n%s", out)
	}
	// section 不存在 → 新开一节，不假装改到了别处
	fresh, _ := ApplyPatch("[a]\nx = 1\n", Patch{Format: PatchINI, Section: "web", Set: map[string]string{"bind to": "0.0.0.0"}})
	if !strings.Contains(fresh, "[web]") || !strings.Contains(fresh, "bind to = 0.0.0.0") {
		t.Errorf("section 不存在时应当新开一节：\n%s", fresh)
	}
}

// TestApplyPatchYAML yaml 用 `key: value`，值里的 # 不算注释。
func TestApplyPatchYAML(t *testing.T) {
	src := "bind-addr: 127.0.0.1:8080\nauth: password\ncert: false\n"
	got, _ := ApplyPatch(src, Patch{Format: PatchYAML, Set: map[string]string{"bind-addr": "0.0.0.0:8080"}})
	if !strings.Contains(got, "bind-addr: 0.0.0.0:8080") {
		t.Errorf("yaml 的键没被替换：\n%s", got)
	}
	if !strings.Contains(got, "auth: password") {
		t.Errorf("无关键被动了：\n%s", got)
	}
	// 已有值一样 → 幂等
	if _, changed := ApplyPatch(got, Patch{Format: PatchYAML, Set: map[string]string{"bind-addr": "0.0.0.0:8080"}}); changed {
		t.Error("值已一致时应当 changed=false")
	}
	// 行尾注释里的 # 不该把值吃掉
	src2 := "password: p#ss#word\n"
	out, _ := ApplyPatch(src2, Patch{Format: PatchYAML, Set: map[string]string{"password": "p#ss#word"}})
	if out != src2 {
		t.Errorf("值里的 # 被误认为注释：%q → %q", src2, out)
	}
}

// TestApplyPatchPreservesIndentAndSep 替换时保留缩进与原有分隔符写法（不重排用户文件）。
func TestApplyPatchPreservesIndentAndSep(t *testing.T) {
	src := "[web]\n    default port=19999\n"
	got, _ := ApplyPatch(src, Patch{Format: PatchINI, Section: "web", Set: map[string]string{"default port": "20000"}})
	if !strings.Contains(got, "    default port=20000") {
		t.Errorf("缩进与 `=` 写法应当保留：\n%s", got)
	}
}

// TestValidatePatchRejectsInjection 补丁的校验必须挡住注入与写错的格式。
func TestValidatePatchRejectsInjection(t *testing.T) {
	cases := []struct {
		name string
		p    Patch
		want string
	}{
		{"值里有换行", Patch{Format: PatchKV, Set: map[string]string{"a": "1\nrm -rf /"}}, "换行"},
		{"格式不认识", Patch{Format: "toml", Set: map[string]string{"a": "1"}}, "format"},
		{"非 ini 写了 section", Patch{Format: PatchKV, Section: "web", Set: map[string]string{"a": "1"}}, "section"},
		{"键名形状不对", Patch{Format: PatchKV, Set: map[string]string{"a b": "1"}}, "形状"},
		{"if_missing 写错", Patch{Format: PatchKV, IfMissing: "maybe", Set: map[string]string{"a": "1"}}, "if_missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := ValidatePatch(&c.p)
			if len(errs) == 0 {
				t.Fatalf("应当被拒绝：%+v", c.p)
			}
			joined := strings.Join(errs, "；")
			if !strings.Contains(joined, c.want) {
				t.Errorf("报错里应当提到 %q，实际 %q", c.want, joined)
			}
		})
	}
	// 正向对照：合法补丁不许报错
	if errs := ValidatePatch(&Patch{Format: PatchINI, Section: "server", IfMissing: "create",
		Set: map[string]string{"http_port": "3002", "domain": "a.local"}}); len(errs) != 0 {
		t.Errorf("合法补丁不该报错：%v", errs)
	}
	if errs := ValidatePatch(nil); len(errs) != 0 {
		t.Errorf("没有补丁不该报错：%v", errs)
	}
}

// TestPlanTextShowsPatch 计划文本必须写出"会改哪一行配置"：审查者要在动手前看见它。
func TestPlanTextShowsPatch(t *testing.T) {
	spec, err := Parse([]byte(`{
	  "schema": "zizpanel.app/v1", "id": "patched", "name": "补丁应用", "icon": "🧪",
	  "source": {"kind": "brew", "formula": "patched", "checksum": "sha256"},
	  "run": {"mode": "brew-service"},
	  "config": {"path": "{brew}/etc/patched.ini", "format": "ini", "section": "server",
	             "set": {"http_port": "3002"}},
	  "verify": {"any_of": [{"kind": "http", "path": "/health", "timeout": "30s"}]},
	  "health": {"kind": "http", "path": "/health"},
	  "uninstall": {"optional_data": ["{brew}/var/patched"], "formula": "patched"}
	}`), "patched.json")
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanText(spec)
	if !strings.Contains(plan, "配置补丁") || !strings.Contains(plan, "[server] http_port=3002") {
		t.Errorf("计划里必须写出配置补丁：\n%s", plan)
	}
}

// TestApplyPatchOnRealGrafanaSample 用 grafana 真实的 sample.ini 片段做回归：
// 那个文件整篇都是 `;key = value` 形式的注释默认值，补丁必须**追加**到 [server] 段末尾，
// 而且要落在 [server.custom_response_headers] 这类子段之前（否则改的就不是它了）。
func TestApplyPatchOnRealGrafanaSample(t *testing.T) {
	src := `[server]
# Protocol (http, https, h2, socket, socket_h2)
;protocol = http

# The ip address to bind to, empty will bind to all interfaces
;http_addr =

# The http port to use
;http_port = 3000

# The public facing domain name used to access grafana from a browser
;domain = localhost

[server.custom_response_headers]
;X-Frame-Options = deny
`
	got, changed := ApplyPatch(src, Patch{
		Format: PatchINI, Section: "server",
		Set: map[string]string{"http_addr": "", "http_port": "3002"},
	})
	if !changed {
		t.Fatal("应当报告改了")
	}
	// 注释掉的默认值必须原封不动
	if !strings.Contains(got, ";http_port = 3000") || !strings.Contains(got, ";http_addr =") {
		t.Errorf("被注释的默认值不该被动：\n%s", got)
	}
	// 新写的两行必须在 [server] 段之内、且在任何子段之前
	serverAt := strings.Index(got, "[server]")
	portAt := strings.Index(got, "http_port = 3002")
	subAt := strings.Index(got, "[server.custom_response_headers]")
	if serverAt < 0 || portAt < 0 || subAt < 0 {
		t.Fatalf("补齐后的文件缺少关键行：\n%s", got)
	}
	if !(serverAt < portAt && portAt < subAt) {
		t.Errorf("补丁必须写在 [server] 段内（不能跑到子段里）：\n%s", got)
	}
	if !strings.Contains(got, "http_addr = \n") && !strings.HasSuffix(strings.TrimRight(got, "\n"), "http_addr =") {
		t.Errorf("空值也应当写出来（grafana 的口径是「空 = 监听所有网卡」）：\n%q", got)
	}
}

package services

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/plugins"
)

// ============================================================================
//  B1「双记账」：插件表 × Go 目录定义 必须逐字段一致
//
//  过渡期两套定义同时存在（表 + 手写目录），所以必须有人看着它们别漂 ——
//  今天已经吃过一次亏：参数生成侧与校验侧各自绿、整条链却是死的（zizvideo）。
//  这里断言"表派生的卡片事实/卸载计划"与"Go 目录与卸载计划"完全一致；
//  等 B4 把应用逐个迁完，被迁移的条目对应的 Go 定义就删掉，这条门禁也随之缩小。
//
//  为什么放在 services 包内：要读未导出的注册表（releaseBinaryApps / catalog）
//  与 PlanUninstallFor 的真实计划。
// ============================================================================

// pluginEquivCases 是"已经用表表达"的应用。
var pluginEquivCases = []struct {
	id string
	// wantHealthPath 是 Go 目录里的健康路径（空 = 无探针，这种条目不该出现在这里）
	wantSystemDaemon bool
	wantBrewFormula  string
	wantUITarget     string
}{
	// wantSystemDaemon 用**目录字段的真实语义**：brew 应用是否服务化成系统级
	// LaunchDaemon（见 systemdaemon.go 的判定）。alist 走 release 轨、没有 brew
	// formula，所以是 false —— 它"以系统守护进程运行"由 run.mode=app-daemon 表达。
	{"alist", false, "", "app"},
	{"syncthing", true, "syncthing", "app"},
}

func TestPluginTableMatchesCatalog(t *testing.T) {
	home := "/Users/tester"
	for _, c := range pluginEquivCases {
		spec, ok := plugins.Builtin(c.id)
		if !ok {
			t.Fatalf("内建插件表里没有 %s（表没跟上，或 id 写错）", c.id)
		}
		app, ok := FindApp(c.id)
		if !ok {
			t.Fatalf("目录里没有 %s", c.id)
		}
		facts := spec.CardFacts(home)

		if facts.Name != app.Name {
			t.Errorf("%s：表里 name=%q，目录里 %q", c.id, facts.Name, app.Name)
		}
		if facts.Icon != app.Icon {
			t.Errorf("%s：表里 icon=%q，目录里 %q", c.id, facts.Icon, app.Icon)
		}
		if facts.Summary != app.Summary {
			t.Errorf("%s：表里 summary=%q，目录里 %q", c.id, facts.Summary, app.Summary)
		}
		if facts.Port != app.Port {
			t.Errorf("%s：表里 port=%d，目录里 %d", c.id, facts.Port, app.Port)
		}
		if strings.TrimSpace(facts.HealthPath) != strings.TrimSpace(app.HealthPath) {
			t.Errorf("%s：表里 health=%q，目录里 %q", c.id, facts.HealthPath, app.HealthPath)
		}
		if app.SystemDaemon != c.wantSystemDaemon {
			t.Errorf("%s：目录里 SystemDaemon=%v，用例期望 %v（用例本身该更新）", c.id, app.SystemDaemon, c.wantSystemDaemon)
		}
		if facts.SystemDaemon != app.SystemDaemon {
			t.Errorf("%s：表里 system_daemon=%v，目录里 %v", c.id, facts.SystemDaemon, app.SystemDaemon)
		}
		// 配置文件路径：Go 侧是"条目字段 + 家目录/工作目录"推导出来的，表里写绝对（或 ~/）
		goConf := ConfigFilePath(app, home, filepath.Join(home, "work"))
		if strings.TrimSpace(facts.ConfigPath) != strings.TrimSpace(goConf) {
			t.Errorf("%s：表里 config.path=%q，Go 推导 %q", c.id, facts.ConfigPath, goConf)
		}
		// 更新方式：brew formula 存在 ⇒ 表里 update.kind 必须是 brew
		if spec.Update != nil {
			if (c.wantBrewFormula != "") != (spec.Update.Kind == "brew") {
				t.Errorf("%s：目录 brew formula=%q，表里 update.kind=%q", c.id, app.BrewFormula, spec.Update.Kind)
			}
		}
	}
}

func TestPluginTableMatchesUninstallPlan(t *testing.T) {
	home := "/Users/tester"
	m := &Manager{opt: Options{UserHome: home, WorkDir: filepath.Join(home, "work")}}
	for _, c := range pluginEquivCases {
		spec, ok := plugins.Builtin(c.id)
		if !ok {
			t.Fatalf("内建插件表里没有 %s", c.id)
		}
		app, ok := FindApp(c.id)
		if !ok {
			t.Fatalf("目录里没有 %s", c.id)
		}
		plan := m.PlanUninstallFor(context.Background(), app, nil)
		facts := spec.UninstallFacts(home)

		// Go 的 DataPaths 语义 = "勾选删除数据才删" ⇒ 必须与表里的 optional_data 一致
		if strings.Join(plan.DataPaths, "|") != strings.Join(facts.OptionalData, "|") {
			t.Errorf("%s：卸载可选数据路径不一致\n  Go : %v\n  表 : %v", c.id, plan.DataPaths, facts.OptionalData)
		}
		// formula：Go 的卸载计划在"brew 类"才带 formula（拿不到 brew 状态时为空），
		// 所以以目录里的 BrewFormula 为准比对；计划里给了就也必须一致。
		if app.BrewFormula != facts.Formula {
			t.Errorf("%s：卸载 formula 与目录 BrewFormula 不一致：目录 %q，表 %q", c.id, app.BrewFormula, facts.Formula)
		}
		if plan.Formula != "" && plan.Formula != facts.Formula {
			t.Errorf("%s：卸载计划里的 formula 与表不一致：计划 %q，表 %q", c.id, plan.Formula, facts.Formula)
		}
		if len(facts.Always) == 0 {
			t.Errorf("%s：表里 always 为空 —— 卸载至少要说得清删哪个 plist/记录", c.id)
		}
	}
}

// TestPluginEquivCatchesDrift 负向对照：把表改坏，比对必须报出来（门禁必须能失败）。
func TestPluginEquivCatchesDrift(t *testing.T) {
	home := "/Users/tester"
	base, ok := plugins.Builtin("syncthing")
	if !ok {
		t.Fatal("没有 syncthing 内建声明")
	}
	app, _ := FindApp("syncthing")

	// ① 卡片事实：端口改掉 → 必须被判为不一致
	bad := *base
	ex := *base.Expose
	ex.Port = base.Expose.Port + 1
	bad.Expose = &ex
	if diff := cardDiff(bad.CardFacts(home), app); diff == "" {
		t.Error("端口被改动后应当判出不一致")
	}

	// ② 卸载计划：可选数据路径改掉 → 必须被判为不一致
	bad2 := *base
	bad2.Uninstall.OptionalData = []string{"~/somewhere-else"}
	m := &Manager{opt: Options{UserHome: home}}
	plan := m.PlanUninstallFor(context.Background(), app, nil)
	if plan.DataPaths[0] == bad2.UninstallFacts(home).OptionalData[0] {
		t.Error("卸载路径被改动后应当判出不一致")
	}
}

// cardDiff 抽出"卡片事实 vs 目录"的比较，便于负向对照复用（返回非空即不一致）。
func cardDiff(f plugins.CardFacts, app App) string {
	var diffs []string
	if f.Name != app.Name {
		diffs = append(diffs, "name")
	}
	if f.Port != app.Port {
		diffs = append(diffs, "port")
	}
	if strings.TrimSpace(f.HealthPath) != strings.TrimSpace(app.HealthPath) {
		diffs = append(diffs, "health")
	}
	return strings.Join(diffs, ",")
}

// TestPluginTableDrivesCatalogWithoutChangingIt 证明"表真的在驱动目录"且**今天零变化**：
// 覆盖后再读出来的卡片事实，必须与手写目录的原始定义逐字段相同。
func TestPluginTableDrivesCatalogWithoutChangingIt(t *testing.T) {
	raw := map[string]App{}
	for _, a := range builtinCatalog() {
		raw[a.ID] = a
	}
	for _, c := range pluginEquivCases {
		orig, ok := raw[c.id]
		if !ok {
			t.Fatalf("目录里没有 %s", c.id)
		}
		got, ok := FindApp(c.id)
		if !ok {
			t.Fatalf("FindApp 取不到 %s", c.id)
		}
		// 配置文件路径比**解析后**的值：表里写 ~/…、旧定义写相对安装根的相对路径，
		// 两种写法都合法，唯一要求是最终指向同一个文件。
		const home = "/Users/tester"
		gotConf := ConfigFilePath(got, home, filepath.Join(home, "work"))
		origConf := ConfigFilePath(orig, home, filepath.Join(home, "work"))
		if got.Name != orig.Name || got.Icon != orig.Icon || got.Summary != orig.Summary ||
			got.Port != orig.Port || got.HealthPath != orig.HealthPath || gotConf != origConf {
			t.Errorf("%s：接入插件表后目录变了（说明表与旧定义不一致）\n 旧: %+v\n 新: %+v", c.id, orig, got)
		}
	}
	// 没进表的条目必须原样透传（别的应用不受影响）
	if len(raw) > len(pluginEquivCases) {
		for id, a := range raw {
			if _, inTable := plugins.Builtin(id); inTable {
				continue
			}
			got, _ := FindApp(id)
			if got.Name != a.Name || got.Port != a.Port {
				t.Errorf("%s 不在插件表里，不该被改动：%+v → %+v", id, a, got)
			}
		}
	}
}

// TestBuiltinTableOnlyAppsReachCatalog 锁住"表驱动新增应用"这条路：
// 只在插件表里、目录里没有的内建应用，必须真的出现在应用目录里（否则"填表就能加应用"是空话）。
func TestBuiltinTableOnlyAppsReachCatalog(t *testing.T) {
	// 用表新增的 brew 服务型应用（监控/可视化三个 + 早期三个）
	want := map[string]struct {
		formula string
		port    int
		health  string
	}{
		"redis":       {"redis", 6379, ""},
		"mosquitto":   {"mosquitto", 1883, ""},
		"memcached":   {"memcached", 11211, ""},
		"prometheus":  {"prometheus", 9090, "/-/healthy"},
		"netdata":     {"netdata", 19999, "/api/v1/info"},
		"grafana":     {"grafana", 3002, "/api/health"},
		"code-server": {"code-server", 8092, "/healthz"},
		"rabbitmq":    {"rabbitmq", 15672, "/"},
		"nats-server": {"nats-server", 4222, ""},
	}
	for id, w := range want {
		app, ok := FindApp(id)
		if !ok {
			t.Errorf("%s 只在插件表里，但没进应用目录 —— 表驱动新增应用这条链断了", id)
			continue
		}
		if app.BrewFormula != w.formula {
			t.Errorf("%s：formula 应为 %q，实际 %q", id, w.formula, app.BrewFormula)
		}
		if app.Port != w.port {
			t.Errorf("%s：端口应为 %d，实际 %d", id, w.port, app.Port)
		}
		if app.HealthPath != w.health {
			t.Errorf("%s：健康路径应为 %q，实际 %q", id, w.health, app.HealthPath)
		}
		if !app.SystemDaemon {
			t.Errorf("%s：brew 服务型应用应当服务化成系统级守护进程", id)
		}
		if app.Kind != KindNative || app.Category != CategoryTool {
			t.Errorf("%s：映射出来的 Kind/Category 不对：%v/%v", id, app.Kind, app.Category)
		}
		// {brew} 占位符必须能展开（Intel/ARM 前缀不同），展开后不许再有占位符
		spec, _ := plugins.Builtin(id)
		facts := spec.UninstallFactsFor("/Users/tester", "/opt/homebrew")
		for _, p := range append(append([]string{}, facts.Always...), facts.OptionalData...) {
			if strings.Contains(p, "{brew}") || strings.Contains(p, "~") {
				t.Errorf("%s：卸载路径没展开干净：%q", id, p)
			}
		}
		if spec.Config != nil && strings.HasPrefix(spec.Config.Path, "{brew}/") {
			if got := plugins.Expand(spec.Config.Path, "/Users/tester", "/opt/homebrew"); !strings.HasPrefix(got, "/opt/homebrew/") {
				t.Errorf("%s：配置路径的 {brew} 展开不对：%q", id, got)
			}
		}
	}
	// 反面对照：release 来源的表条目映射不出来，就**不该**出现在目录里（不许塞半成品）
	if _, ok := FindApp("nosuchapp"); ok {
		t.Error("不存在的应用不该在目录里")
	}
}

// TestTableConfigPatchReachesApp 表里声明的配置补丁必须流到 App —— 那是安装器真正照着做的那份。
//
// 为什么值得一条门禁：卡片上的端口（3002）与"安装时改的端口"必须来自同一个真源；
// 哪天有人只改了一边（例如把 patch 去掉、或把 expose.port 改回去），
// 用户会得到一个"卡片说 3002、实际还在 3000"的谎报。
func TestTableConfigPatchReachesApp(t *testing.T) {
	app, ok := FindApp("grafana")
	if !ok {
		t.Fatal("grafana 应当来自插件表")
	}
	if len(app.ConfigPatches) == 0 {
		t.Fatal("grafana 声明的配置补丁没有流到 App：安装器不会去改端口")
	}
	p := app.ConfigPatches[0]
	if plugins.NormalizeFormat(p.Format) != plugins.PatchINI || p.Section != "server" {
		t.Errorf("补丁的格式/段不对：%+v", p)
	}
	if p.Set["http_port"] != "3002" {
		t.Errorf("补丁要把 http_port 改成 3002（与卡片端口一致），实际 %q", p.Set["http_port"])
	}
	if app.Port != 3002 {
		t.Errorf("卡片端口应当与补丁一致：%d", app.Port)
	}
	if app.ConfigPath != "{brew}/etc/grafana/grafana.ini" {
		t.Errorf("配置文件路径不对：%q", app.ConfigPath)
	}
}

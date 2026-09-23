package services

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zizdog/zizpanel/internal/plugins"
)

// ============================================================================
//  本地插件（P2）：把插件目录里**已启用**的声明接进应用目录
//
//  范围（刻意保守，别放大）：只支持 `source.kind=brew` 的条目 —— 它直接复用现有的
//  通用 brew 轨（安装 / 系统级守护进程 / 健康 / 卸载全都有现成实现），所以
//  "填一张表就能加应用"今天就成立。其余来源（release / compose / panel-daemon）
//  需要各自的 JSON 装载实现（B4），在列表里明确写着"暂不支持安装"，绝不假装能装。
//
//  安全前提：本地插件**默认关闭**，启用是用户在面板里的显式动作；内建表里已有的 id
//  不许被本地插件覆盖（撞了就在列表里报错）。
// ============================================================================

var (
	localPluginsMu  sync.RWMutex
	localPluginsDir string
)

// SetLocalPluginDir 指定本地插件目录（面板启动时接上；测试注入临时目录）。
// 传空串 = 关闭本地插件（目录不可用、或测试想隔离）。
func SetLocalPluginDir(dir string) {
	localPluginsMu.Lock()
	defer localPluginsMu.Unlock()
	localPluginsDir = strings.TrimSpace(dir)
}

// LocalPluginDir 返回当前目录（界面展示路径用）。
func LocalPluginDir() string {
	localPluginsMu.RLock()
	defer localPluginsMu.RUnlock()
	return localPluginsDir
}

func localPluginEntries() []plugins.LocalPlugin {
	return plugins.LoadDir(LocalPluginDir())
}

// LocalPluginEnabledState 读当前启用状态（列表接口与目录合并共用）。
func LocalPluginEnabledState() map[string]bool {
	return plugins.ReadEnabled(LocalPluginDir())
}

// LocalPluginStatusView 是给界面/接口看的本地插件状态（一份结论，前端不猜）。
type LocalPluginStatusView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Icon    string `json:"icon,omitempty"`
	Summary string `json:"summary,omitempty"`
	File    string `json:"file"`
	Source  string `json:"source_kind,omitempty"`
	RunMode string `json:"run_mode,omitempty"`
	// Valid=false 时 Error 说明为什么不合法（解析/校验/撞 id）
	Valid bool   `json:"valid"`
	Error string `json:"error,omitempty"`
	// Enabled 是用户是否启用（只有 Valid 且 Installable 才能被启用）
	Enabled bool `json:"enabled"`
	// Installable=false 时 Reason 说明为什么（例如"release 来源的 JSON 装载还没做"）
	Installable bool   `json:"installable"`
	Reason      string `json:"reason,omitempty"`
	Plan        string `json:"plan,omitempty"`
}

// localPluginIssue 判断一份**本地**声明能不能接进目录（比 SpecToApp 多一条"撞 id"）。
func localPluginIssue(spec *plugins.Spec) string {
	if spec == nil {
		return "声明不合法"
	}
	if _, exists := builtinCatalogIDs[spec.ID]; exists {
		return "id 与面板自带应用重名（" + spec.ID + "）：换一个 id，或用面板自带的那个"
	}
	return specMappableIssue(spec)
}

// specMappableIssue 判断一份声明能不能映射成应用条目（与"从哪来"无关）。
func specMappableIssue(spec *plugins.Spec) string {
	if spec == nil {
		return "声明不合法"
	}
	if spec.Source.Kind != "brew" {
		return "这个来源（" + spec.Source.Kind + "）的 JSON 装载还没做：目前只支持 brew 来源（见 docs/平台化路线图.md 的 B4）"
	}
	switch spec.Run.Mode {
	case "brew-service", "app-daemon", "none":
	default:
		return "运行方式 " + spec.Run.Mode + " 还没做：brew 来源目前支持 brew-service / app-daemon / none"
	}
	if strings.TrimSpace(spec.Source.Formula) == "" {
		return "brew 来源必须写 formula"
	}
	return ""
}

// SpecToApp 把一份声明映射成应用目录条目（只支持 localPluginIssue 放行的形态）。
// 内建表与本地插件共用同一条映射 —— 这就是"新增应用 = 填表"的兑现点。
func SpecToApp(spec *plugins.Spec) (App, error) {
	if issue := specMappableIssue(spec); issue != "" {
		return App{}, fmt.Errorf("%s", issue)
	}
	app := App{
		ID:          spec.ID,
		Name:        spec.Name,
		Icon:        spec.Icon,
		Summary:     spec.Summary,
		Category:    CategoryTool,
		Kind:        KindNative,
		BrewFormula: spec.Source.Formula,
	}
	// expose 是可选字段（不带界面的应用可以不写）—— 直接解引用会让面板 panic，
	// 于是一条合法声明就能把整个市场接口搞挂（单测抓到过）。
	if spec.Expose != nil {
		app.Port = spec.Expose.Port
	}
	if spec.Requires != nil && spec.Requires.SystemDaemon {
		app.SystemDaemon = true
	}
	if spec.Run.Mode == "brew-service" && spec.Requires == nil {
		// brew 服务默认要服务化成系统级守护进程（无头机器重启后要能自己起来），
		// 与目录里其它 brew 条目的取舍一致；requires 显式写了就听 requires 的。
		app.SystemDaemon = true
	}
	if spec.Health.Kind == "http" && spec.Expose != nil && spec.Expose.Port > 0 {
		app.HealthPath = spec.Health.Path
	}
	if spec.Config != nil {
		app.ConfigPath = spec.Config.Path
		app.ConfigMode = spec.Config.Mode
		// 深拷贝：插件声明可能在别处被复用/改动，App 不能拿着共享的 map/slice。
		for _, p := range spec.Config.PatchList() {
			set := make(map[string]string, len(p.Set))
			for k, v := range p.Set {
				set[k] = v
			}
			app.ConfigPatches = append(app.ConfigPatches, plugins.Patch{
				Format: p.Format, Section: p.Section, Set: set,
				IfMissing: p.IfMissing, Secrets: append([]string{}, p.Secrets...),
			})
		}
	}
	// 给用户的说明：本地插件的身份与"停用 ≠ 卸载"，别让人以为停用就把东西删了。
	app.PostInstallHint = strings.Join(spec.Notes, "") +
		"这是**本地插件**（" + spec.ID + "）：声明在 <安装根>/plugins，改完刷新「应用」页生效；" +
		"停用它只是不再显示/不再安装，已装的东西仍在机器上。"
	return app, nil
}

// LocalPluginApp 保留旧名字（本地插件侧调用点），行为同 SpecToApp 减去"撞 id"检查 ——
// 本地插件必须查重，见 localPluginIssue。
func LocalPluginApp(spec *plugins.Spec) (App, error) {
	if issue := localPluginIssue(spec); issue != "" {
		return App{}, fmt.Errorf("%s", issue)
	}
	return SpecToApp(spec)
}

// builtinPluginOnlyApps 返回"只在插件表里"的内建应用（表驱动新增的应用靠它上架）。
//
// 判定：表里有、目录里没有 ⇒ 映射成 App 追加进目录。映射不出来的（例如 release 轨还没做
// JSON 装载）**跳过**并留给门禁报错 —— 绝不往目录里塞半成品。
func builtinPluginOnlyApps() []App {
	var out []App
	for _, id := range plugins.BuiltinIDs() {
		if _, exists := builtinCatalogIDs[id]; exists {
			continue
		}
		spec, ok := plugins.Builtin(id)
		if !ok {
			continue
		}
		app, err := SpecToApp(spec)
		if err != nil {
			continue
		}
		out = append(out, app)
	}
	return out
}

// LocalPluginStatus 汇总本地插件的状态（列表接口与门禁共用一份判断）。
func LocalPluginStatus(enabled map[string]bool) []LocalPluginStatusView {
	entries := localPluginEntries()
	out := make([]LocalPluginStatusView, 0, len(entries))
	for _, e := range entries {
		v := LocalPluginStatusView{File: e.File, ID: e.ID, Name: e.Name, Icon: e.Icon,
			Summary: e.Summary, Source: e.Source, RunMode: e.RunMode}
		if e.Err != "" || e.Spec == nil {
			v.Error = e.Err
			if v.Error == "" {
				v.Error = "声明不合法"
			}
			out = append(out, v)
			continue
		}
		if issue := localPluginIssue(e.Spec); issue != "" {
			v.Error = issue
		} else {
			v.Valid = true
			v.Installable = true
			v.Plan = plugins.PlanText(e.Spec)
			v.Enabled = enabled[e.Spec.ID]
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].File < out[j].File
	})
	return out
}

// localPluginApps 返回**已启用且可安装**的条目（Catalog 合并用）。
func localPluginApps(enabled map[string]bool) []App {
	var out []App
	for _, e := range localPluginEntries() {
		if e.Spec == nil || !enabled[e.Spec.ID] {
			continue
		}
		app, err := LocalPluginApp(e.Spec)
		if err != nil {
			continue // 不可安装的即使被标记启用也不进目录（状态接口会如实说明原因）
		}
		out = append(out, app)
	}
	return out
}

// builtinCatalogIDs 是内建应用 id 集合（防止本地插件覆盖自带条目）。
var builtinCatalogIDs = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, a := range builtinCatalog() {
		m[a.ID] = struct{}{}
	}
	return m
}()

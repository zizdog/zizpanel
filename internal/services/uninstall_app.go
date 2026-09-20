package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zizdog/zizpanel/internal/priv"
)

// brewUsesCache 缓存 `brew uses --installed <formula>` 的结果（包级：Manager 每次请求新建）。
// 依赖关系低频，5 分钟 TTL 足够；键是 formula，值是"依赖它的已装包"。
var (
	brewUsesMu    sync.Mutex
	brewUsesCache = map[string]brewUsesEntry{}
)

const brewUsesTTL = 5 * time.Minute

type brewUsesEntry struct {
	deps []string
	ok   bool
	at   time.Time
}

// resetBrewUsesCache 仅供测试：清掉进程级缓存。
func resetBrewUsesCache() {
	brewUsesMu.Lock()
	brewUsesCache = map[string]brewUsesEntry{}
	brewUsesMu.Unlock()
}

// 卸载"面板自己装的应用"：三类语义必须分清（否则会删掉用户自己的东西）：
// 1) managed 服务 → Manager.Uninstall；2) 面板自研安装器 → UninstallApp（卸服务+删 plist+删记录+按选择删产物）；
// 3) 纳管的第三方服务（nginx/php/mysql/用户自注册）→ **绝不卸载**，只「取消纳管」：删掉用户的 MySQL 等于删他的网站数据。
//
// 通用卸载只认托管服务，面板自研安装器注册为纳管态会被它拒绝 —— 这些应用的卸载入口只能由本文件提供。

// UninstallPlan 是"卸载这个应用会做什么"的说明。
// 卸载不可逆：确认框必须写清**会删哪些路径**与**保留什么**，不是一句"确定卸载吗"。
type UninstallPlan struct {
	// Kind: service/installer/brew/forget（forget = 用户自己装的，只允许从列表移除）
	Kind string `json:"kind"`
	// Service 是面板服务记录名（Kind=service/forget/brew 且有条记录时有意义）
	Service string `json:"service,omitempty"`
	// Steps 是卸载会做的事（人类可读，逐条列出）
	Steps []string `json:"steps"`
	// DataPaths 是"可选删除"的产物路径（remove_data=1 时才删）
	DataPaths []string `json:"data_paths,omitempty"`
	// KeepNote 说明默认会保留什么
	KeepNote string `json:"keep_note,omitempty"`
	// Blocked 非空表示现在不能卸载（例如 Docker 运行时还托着别的应用）
	Blocked string `json:"blocked,omitempty"`
	// Formula 是 Kind=brew 时要卸载的 Homebrew formula。
	// 必须用**判定时采用的那个写法**（见 ResolveBrewFormula），再猜一次就会删错包。
	Formula string `json:"formula,omitempty"`
	// PhpVersion 是 PHP 卸载时"属于这一次的版本号"（"8.4"），计划阶段算准后原样带给执行端：
	// 无版本别名 `php` 推不出 8.4，重推就可能删错版本 —— 确认框写什么就必须删什么。
	PhpVersion string `json:"php_version,omitempty"`
	// Dependents 是"这次卸载会影响谁"（站点/容器/面板应用/brew 包），前端据此逐条列出。
	// 依赖判定唯一实现见 dependents.go。
	Dependents []Dependent `json:"dependents,omitempty"`
	// DependentsChecked 表示上面那份名单**真的查过**。
	// false = 没查成（brew 不可用/超时）→ 界面必须如实写"未检查"，绝不能说"没有依赖"。
	DependentsChecked bool `json:"dependents_checked,omitempty"`
	// ForceAllowed 表示"这个计划允许强制卸载（brew uninstall --ignore-dependencies）"。
	// 用户真机卸载 python@3.13：brew 因 llvm/rust 依赖拒绝卸载，计划阶段就要说清"谁依赖它"。
	// 语义边界（必须守住）：只是"允许用户选"，**绝不允许默认加 --ignore-dependencies**（只在 force=true 时追加）。
	ForceAllowed bool `json:"force_allowed,omitempty"`
	// ForceNote 是"强制卸载会破坏什么"的逐字说明（界面直接展示，不用自己拼）。
	ForceNote string `json:"force_note,omitempty"`
}

// PlanUninstall 给出某个目录应用当前该怎么卸载（只读，不产生任何改动）。
func (m *Manager) PlanUninstall(ctx context.Context, appID string) UninstallPlan {
	app, ok := FindApp(appID)
	if !ok {
		// 条目可能已从目录下架（如 n8n），但机器上还有它的产物：
		// 用最小 App（只有 ID）继续走残留分支，否则"界面上没了、磁盘上还在、无处可点"。
		app = App{ID: appID, Name: appID}
	}
	return m.PlanUninstallFor(ctx, app, m.findServiceRecord(ctx, app))
}

// legacyComposeImages 记录**已从目录下架**、但机器上可能还有 compose 目录的应用镜像。
// 计划要写清"哪些镜像可手动删"（面板不自动 docker rmi，怕误删共用层）；条目下架后只能留一份。
var legacyComposeImages = map[string][]string{
	// 2026-09-17 下架。
	"n8n": {"n8nio/n8n:latest"},
	// 2026-09-17 下架：**条目下架 != 卸载**，计划仍要说得出镜像名，否则几百 MB 镜像无声占着。
	"minio":     {"quay.io/minio/minio:latest"},
	"portainer": {"portainer/portainer-ce:lts"},
}

// composeImagesForPlan 返回卸载计划里要提示的镜像列表（可能为空）。
func composeImagesForPlan(app App) []string {
	if imgs := composeImagesOf(app.ComposeYAML); len(imgs) > 0 {
		return imgs
	}
	return legacyComposeImages[app.ID]
}

// composeImageNote 把镜像列表拼成一句给用户看的提示。
func composeImageNote(imgs []string) string {
	if len(imgs) == 0 {
		return ""
	}
	return "面板**不自动删除镜像**（避免误删共用层）；要释放空间可手动执行：docker rmi " +
		strings.Join(imgs, " ")
}

// BrewState 是"这个条目在 Homebrew 里的真实状态"，由调用方算好传入：
// 每次 `brew list --versions` 要 0.4~0.6 秒，计划自己再跑一遍会把市场列表拖慢数秒
// （web 层已有整批缓存，见 Server.installedFormulas）。
type BrewState struct {
	// Formula 是判定采用的实际 formula，可能与目录写法不同（见 ResolveBrewFormula）。
	Formula string
	// Version 是 `brew list --versions` 的版本串（如 "8.4.7_1"），用于推导 etc 下的版本目录名 ——
	// 无版本别名的 `php` 只有靠它才知道要清理 etc/php/8.4 而不是别的版本。
	Version string
	// Installed 表示 Formula 真的在 brew list 里。
	Installed bool
}

// ResolveBrewFormula 决定"这个目录条目对应机器上哪个已装的 brew formula"（installed = formula→版本串）：
// 先精确匹配；不中且目录写 `php@8.4` 时看无后缀 formula（php）版本是否匹配。
// 只精确匹配会让装着 PHP 8.4 的机器显示"未安装"、一颗收尾按钮都不给（用户现场："没安装的显示安装"）。
func ResolveBrewFormula(catalogFormula string, installed map[string]string) (formula, version string, ok bool) {
	f := strings.TrimSpace(catalogFormula)
	if f == "" {
		return "", "", false
	}
	if v, hit := installed[f]; hit {
		return f, v, true
	}
	at := strings.Index(f, "@")
	if at <= 0 {
		return "", "", false
	}
	base, want := f[:at], f[at+1:]
	v, hit := installed[base]
	if !hit || !versionTokenMatches(v, want) {
		return "", "", false
	}
	return base, v, true
}

// versionTokenMatches 判断 brew 的版本串里是否含 want（"8.4" 匹配 "8.4.7"、"8.4.7_1"）。
func versionTokenMatches(installedVersions, want string) bool {
	if want == "" {
		return false
	}
	for _, tok := range strings.Fields(installedVersions) {
		if i := strings.Index(tok, "_"); i > 0 {
			tok = tok[:i] // 8.4.7_1 → 8.4.7
		}
		if tok == want || strings.HasPrefix(tok, want+".") {
			return true
		}
	}
	return false
}

// BrewStateFor 从整批 brew 版本结果里解析出某个条目的状态。
func BrewStateFor(catalogFormula string, installed map[string]string) BrewState {
	f, v, ok := ResolveBrewFormula(catalogFormula, installed)
	if !ok {
		return BrewState{}
	}
	return BrewState{Formula: f, Version: v, Installed: true}
}

// PlanUninstallFor 与 PlanUninstall 相同，但用调用方已经查好的记录（brew 状态在这里按需查一次）。
// "装了 brew 包但没有面板记录"这一态必须给得出卸载入口；
// 市场列表用 PlanUninstallForBrewFast（不查依赖，36 条各跑一次 brew 就是 15 秒冷启动）。
func (m *Manager) PlanUninstallFor(ctx context.Context, app App, rec *Service) UninstallPlan {
	return m.PlanUninstallForBrew(ctx, app, rec, m.resolveBrewState(ctx, app))
}

// resolveBrewState 查一次这台机器上这个条目的 brew 状态（单个应用用；列表请用 Fast 版）。
func (m *Manager) resolveBrewState(ctx context.Context, app App) BrewState {
	if app.BrewFormula == "" {
		return BrewState{}
	}
	installed := m.installedFormulaVersions(ctx)
	return BrewStateFor(app.BrewFormula, installed)
}

// installedFormulaVersions 取整批 formula → 版本（测试可注入）。
// 探测失败返回空集合："探测失败 ≠ 什么都没装"由 InstalledFormulaVersions 的第二个返回值表达，
// 消费方（市场列表）必须自己判断。
func (m *Manager) installedFormulaVersions(ctx context.Context) map[string]string {
	vers, _ := m.InstalledFormulaVersions(ctx)
	return vers
}

// PlanUninstallForBrew 与 PlanUninstallFor 相同，但 brew 状态由调用方按整批缓存传入；
// 所有分支都会过依赖引擎（dependents.go），说得出"谁在用它、该怎么办"。
func (m *Manager) PlanUninstallForBrew(ctx context.Context, app App, rec *Service, brew BrewState) UninstallPlan {
	// 卸载不可逆：完整路径必须查依赖（checkDeps=true），说得出"谁在用它、能不能强制卸载"。
	plan := m.planUninstallForBrewCore(ctx, app, rec, brew, true)
	m.ApplyDependents(ctx, app, rec, &plan)
	m.brewDependencyBlockForInstaller(ctx, app, brew, &plan)
	return plan
}

// PlanUninstallForBrewFast 是**只算本体、不查依赖**的计划（列表用）：依赖检测是一次真实
// brew 调用，列表每条跑一遍 = 36 条 15 秒冷启动，用户看到的是"正在读取应用目录…"卡住不动
// （真机实测：GET /api/v1/market 冷 14.998s、热 0.25s）。
//
// 调用方**不得**拿它断言"没有依赖"—— 它只是"还没查"，按需查走 uninstall-plan 接口。
func (m *Manager) PlanUninstallForBrewFast(app App, rec *Service, brew BrewState) UninstallPlan {
	return m.planUninstallForBrewCore(context.Background(), app, rec, brew, false)
}

// brewDependencyBlockForInstaller 给"面板安装器但实际靠 brew 卸载"的条目补上 **brew 依赖**层
// （真机卸载 python@3.13 的那一类）。只查真的会跑 brew uninstall 的安装器：python 与 phpmyadmin；
// 其余要么不动 brew、要么保留包，问了只是噪音。
func (m *Manager) brewDependencyBlockForInstaller(ctx context.Context, app App, brew BrewState, plan *UninstallPlan) {
	if plan == nil || plan.Kind != "installer" {
		return
	}
	switch app.PanelInstaller {
	case "python", "phpmyadmin":
	default:
		return
	}
	formula := plan.brewUninstallFormula()
	if formula == "" {
		formula = brew.Formula
	}
	if formula == "" {
		formula = app.BrewFormula
	}
	m.BrewDependencyBlock(ctx, formula, plan)
}

// brewUninstallFormula 从计划步骤里取回"这次到底要 brew uninstall 哪个 formula"。
// 计划那一步是唯一权威写法（与 UninstallBrewApp 的规则一致）：
// 目录写 php@8.4 而机器上装的是 php 8.4.7，拿 BrewFormula 再猜一次就删错包。
func (p UninstallPlan) brewUninstallFormula() string {
	for _, s := range p.Steps {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(s), "brew uninstall "); ok {
			rest = strings.TrimSpace(rest)
			if rest == "" || strings.HasPrefix(rest, "-") {
				// `brew uninstall --ignore-dependencies <f>` 这类写法：取最后一个非选项词。
				f := ""
				for _, tok := range strings.Fields(rest) {
					if !strings.HasPrefix(tok, "-") {
						f = tok
					}
				}
				return f
			}
			return strings.Fields(rest)[0]
		}
	}
	return ""
}

// planUninstallForBrewCore 是计划本体（不含依赖检测，便于测试单独驱动）。
// checkDeps=false 时**一次 brew 都不跑**：brewUsesInstalled 是 0.4 秒的真实 brew 启动，
// 依赖留到用户点「卸载」时按需查（见 PlanUninstallForBrewFast 注释）。
func (m *Manager) planUninstallForBrewCore(ctx context.Context, app App, rec *Service, brew BrewState, checkDeps bool) UninstallPlan {
	if rec != nil && rec.Managed && app.PanelInstaller == "" {
		if app.Kind == KindCompose || app.Kind == KindDocker {
			keep := "compose 应用只删容器与网络，**具名卷（数据）保留**"
			steps := []string{
				"docker compose down（删除容器与网络）",
				"从「服务管理」中删除这条记录",
			}
			// 不删镜像（怕误删共用层），但计划要写出"哪些镜像还占着磁盘、怎么清"。
			if note := composeImageNote(composeImagesForPlan(app)); note != "" {
				keep = keep + "；" + note
			}
			return UninstallPlan{Kind: "service", Service: rec.Name, Steps: steps, KeepNote: keep}
		}
		// brew 原生托管服务：只删服务定义的话 brew 包还原封不动（php82 的报障）。
		// 真实动作 = 停服务 + 删记录 + brew uninstall。
		if brew.Installed {
			p := m.brewUninstallPlan(ctx, app, brew, checkDeps)
			p.Service = rec.Name
			p.Steps = append([]string{
				"停止并移除「" + rec.DisplayName + "」的服务定义",
				"从「服务管理」中删除这条记录",
			}, p.Steps...)
			return p
		}
		// 记录在、brew 包却不在（用户手工 brew uninstall 过）：只剩"删记录"是诚实的。
		return UninstallPlan{
			Kind:    "service",
			Service: rec.Name,
			Steps: []string{
				"停止并移除「" + rec.DisplayName + "」的服务定义",
				"从「服务管理」中删除这条记录",
			},
			KeepNote: "Homebrew 里已经没有这个包了（可能已被手工卸载），所以没有 brew uninstall 这一步",
		}
	}
	if app.PanelInstaller != "" {
		p := m.installerPlan(ctx, app)
		if rec != nil {
			p.Service = rec.Name
		}
		return p
	}
	if rec != nil {
		// managed=false 但目录知道它是什么（有 brew formula 且真的装着）→ 给真正的卸载。
		// 用户要求：目录里的应用只有一个「🗑 卸载」动作并真的卸载，不摆"只删记录"
		// （"移除却不卸载"会让它隐身运行）。mysql84 这类用户早年自装、后登记的条目也走这条。
		if app.PanelInstaller == "" && brew.Installed &&
			app.Kind != KindCompose && app.Kind != KindDocker {
			p := m.brewUninstallPlan(ctx, app, brew, checkDeps)
			p.Service = rec.Name
			p.Steps = append([]string{
				"停止并移除「" + rec.DisplayName + "」的服务定义",
				"从「服务管理」中删除这条记录",
			}, p.Steps...)
			return p
		}
		// 面板**不认识**这条服务：只能"停止 + 从记录里移除"，并说明软件仍在磁盘上需自行卸载 ——
		// **绝不**留下一个还在运行、面板里却看不到的服务（"隐身运行"）。
		label := rec.DisplayName
		if label == "" {
			label = rec.Name
		}
		steps := []string{}
		keep := ""
		if rec.LaunchLabel != "" {
			steps = append(steps, "停止并删除 launchd 服务 "+rec.LaunchLabel)
		} else {
			steps = append(steps, "停止这个服务（如果它正在运行）")
		}
		steps = append(steps, "把「"+label+"」从面板记录里移除")
		keep = "面板**不卸载**这个软件（它不在应用目录里，面板不知道该怎么卸）：" +
			"移除后软件仍在磁盘上、需要你自己卸载；但它**不会继续在后台运行**。"
		return UninstallPlan{
			Kind:     "forget",
			Service:  rec.Name,
			Steps:    steps,
			KeepNote: keep,
		}
	}
	// 残留的**原生 release 安装**：应用已不在注册表里，但旧的原生目录/plist 还在磁盘上。
	// 卸载计划必须**以磁盘状态为准**，不能只看代码注册表（2026-09-16 用户实测："lucky 根本没被卸载掉"）——
	// 先把磁盘上真实存在的东西都收进来，再决定有没有可清理的对象。
	paths := []string{}
	if d := filepath.Join(m.composeDir(), app.ID); dirExists(d) {
		paths = append(paths, d)
	}
	if d := m.legacyNativeDir(app); d != "" {
		paths = append(paths, d)
	}
	// **brew 装了、但面板没有任何记录**（用户自己 brew install 的、或换机后记录丢了）。
	// 用户现场 nginx 就是这一态：installed=true 却 kind:"none"，一颗收尾按钮都没有
	// （"甚至没有卸载按钮"）。installed=true 必须给得出卸载路径。
	if brew.Installed {
		p := m.brewUninstallPlan(ctx, app, brew, checkDeps)
		// 残留的 launchd 服务要先停，否则 KeepAlive 会一直拉起一个已不存在的二进制。
		if label := m.brewLabelFor(brew.Formula); label != "" {
			p.Steps = append([]string{"停止并删除 launchd 服务 " + label}, p.Steps...)
		}
		if len(paths) > 0 {
			p.DataPaths = paths
			head := []string{}
			for _, d := range paths {
				head = append(head, "删除残留目录 "+d)
			}
			p.Steps = append(head, p.Steps...)
		}
		return p
	}
	if len(paths) > 0 {
		steps := []string{"停止并删除这个应用残留的 launchd 服务（如果有）"}
		for _, d := range paths {
			steps = append(steps, "删除残留目录 "+d)
		}
		keep := "删除的是磁盘上真实存在的残留产物；之后可以用「安装」装当前版本"
		// 下架条目（n8n）的镜像名也要写出来，否则用户删完目录、镜像还占着几百 MB。
		if note := composeImageNote(composeImagesForPlan(app)); note != "" {
			steps = append(steps, note)
		}
		return UninstallPlan{
			Kind:      "installer",
			Steps:     steps,
			DataPaths: paths,
			KeepNote:  keep,
		}
	}
	// 只剩 launchd 里的服务定义（"僵尸服务"，Colima 僵尸 plist 坑 161）：必须能停掉并摘掉，
	// 否则又是"installed=true 却 kind=none、一颗收尾按钮都没有"的自相矛盾。
	if app.BrewFormula != "" {
		if label := m.brewLabelFor(app.BrewFormula); label != "" {
			return UninstallPlan{
				Kind:     "installer",
				Steps:    []string{"停止并删除残留的 launchd 服务 " + label},
				KeepNote: "Homebrew 里已经没有这个包了；这一步只摘掉它残留的服务定义",
			}
		}
	}
	// 残留态（没有服务记录，但磁盘上还有 compose 目录）：不给计划的话卡片显示"未安装"
	// 却**没有任何清理入口**，那份数据永远清不掉（2026-09-16 用户反馈的同一类问题）。
	return UninstallPlan{Kind: "none", Blocked: "没有找到可卸载的对象（Homebrew 里没有这个包，面板里也没有记录与残留）"}
}

// brewUninstallPlan 给出"按 Homebrew 包卸载"的计划，steps 第一条固定是 `brew uninstall <formula>`。
// 依赖关系必须如实交代：
//
//   - 查到就点名（brew uninstall 不连带删依赖，那些包会缺依赖）；
//
//   - 查了没有 → "已检查"；没查成 → "**未检查**"，绝不假装没有依赖（铁律 11）。
func (m *Manager) brewUninstallPlan(ctx context.Context, app App, brew BrewState, checkDeps bool) UninstallPlan {
	formula := brew.Formula
	if formula == "" {
		formula = app.BrewFormula
	}
	p := UninstallPlan{Kind: "brew", Formula: formula}
	p.Steps = append(p.Steps, "brew uninstall "+formula)
	// PHP：brew 从不删 etc/php/<版本> 下的配置。计划要把**这一个版本自己的**配置目录
	// 列成可选清理项，并在确认框里说清（见 phpConfigPaths 注释）。
	phpVersion := m.phpVersionFor(formula, brew.Version)
	p.PhpVersion = phpVersion
	phpConfig := m.phpConfigPathsFor(phpVersion)
	if len(phpConfig) > 0 {
		p.DataPaths = append(p.DataPaths, phpConfig...)
		p.Steps = append(p.Steps, "brew uninstall 完成后，把 PHP "+
			phpVersion+" **自己**的配置目录清理掉（勾选「同时删除配置」才会执行）："+
			strings.Join(phpConfig, "、"))
	}
	// checkDeps=false（市场列表）：**不跑 brew**，如实标成"未检查"（一条查询 0.4 秒，36 条 15 秒）。
	deps, checked := []string(nil), false
	if checkDeps {
		deps, checked = m.brewUsesInstalled(ctx, formula)
	}
	p.DependentsChecked = checked
	for _, d := range deps {
		p.Dependents = append(p.Dependents, Dependent{Kind: "brew", Name: d,
			Detail: "Homebrew 包 " + d + " 依赖 " + formula,
			Action: "先卸载它（如果它已经不需要了），或选择强制卸载 " + formula +
				"（会破坏 " + d + "：它会缺依赖、可能无法运行）"})
	}
	switch {
	case len(deps) > 0:
		p.Steps = append(p.Steps,
			"⚠️ 下列已安装的软件依赖 "+formula+"，brew 会拒绝卸载："+strings.Join(deps, "、"))
		p.Steps = append(p.Steps,
			"如果要保留它们，请先卸载它们或保留 "+formula+"；"+
				"只有选择「强制卸载」才会执行 brew uninstall --ignore-dependencies "+formula)
		// 计划阶段就逐条列出依赖方，让用户在按确认前看清"谁依赖它、有什么选择"。
		// 强制卸载必须**逐字写出命令** `brew uninstall --ignore-dependencies <formula>`。
		p.Blocked = "brew 拒绝卸载 " + formula + "：" + strings.Join(deps, "、") + " 依赖它。" +
			"可以先卸载它们，或选择强制卸载（brew uninstall --ignore-dependencies " + formula +
			"，会破坏 " + strings.Join(deps, "、") + "：它们会缺依赖、可能无法运行）"
		// 强制开关必须只对"依赖拦下来"这一类开放。
		p.ForceAllowed = true
		p.ForceNote = "强制卸载会破坏这些包：" + strings.Join(deps, "、") +
			"（它们会缺依赖、可能无法运行）。命令：brew uninstall --ignore-dependencies " + formula
	case checked:
		p.Steps = append(p.Steps, "已检查：没有其它已安装的 Homebrew 包依赖 "+formula)
	default:
		p.Steps = append(p.Steps,
			"未检查是否有别的软件依赖 "+formula+"（可自行执行：brew uses --installed "+formula+"）")
	}
	// 必须出现在确认框里：不会自动删别的包（HOMEBREW_NO_AUTOREMOVE=1；用户真机见到
	// "Autoremoving 2 unneeded formulae"），也不会删配置目录（brew 从不删 etc/ 下的配置）。
	p.KeepNote = "brew uninstall 只删除 " + formula + " 本身：**不会**删除它依赖的包，" +
		"也**不会**自动删除其它「已不再被需要」的包（面板给卸载命令带上 HOMEBREW_NO_AUTOREMOVE=1），" +
		"更不会删除用户数据与配置目录"
	if len(phpConfig) > 0 {
		// PHP 特殊：brew warning 会把整个 etc/php（含其它版本与 phpmyadmin 的配置）一起列出来，
		// 很容易被读成"面板要删这些"，必须在确认框里翻译清楚。
		p.KeepNote = "brew uninstall 只删除 " + formula + " 本身：**不会**删除它依赖的包，" +
			"也**不会**自动删除其它「已不再被需要」的包（面板给卸载命令带上 HOMEBREW_NO_AUTOREMOVE=1）。" +
			"Homebrew 从不删除 " + m.phpEtcDir() + " 下的配置：它卸载后打印的那段 " +
			"「configuration files have not been removed」只是笼统地列出**整个目录**，" +
			"其中其它 PHP 版本（例如 8.2）的目录与 phpmyadmin 的配置**不属于本次卸载、" +
			"面板也不会碰**；本计划只涉及 " + strings.Join(phpConfig, "、") + "。" +
			"不勾选「同时删除该版本的配置」时这些也原样保留"
	}
	// 数据库引擎（MySQL 8.4 / MariaDB）：数据目录里是用户全部的库。
	// 只**点名路径**并明确保留，绝不放进 DataPaths —— 那个勾选项会把它删掉。
	if dbEngineOfFormula(formula) != "" {
		datadir := filepath.Join(m.brewPrefix(), "var", "mysql")
		p.Steps = append(p.Steps, "数据目录 "+datadir+" 会原样保留（你的库都在里面）："+
			"确认不再需要时可手工删除，面板不会自动删")
		p.KeepNote += "；数据目录 " + datadir + " 保留（你的库都在里面，" +
			"但两个引擎不能交替使用同一个数据目录：切换要先迁移数据）"
	}
	return p
}

// brewUsesInstalled 查 `brew uses --installed <formula>`；bool = **这次查询真的成功了**
// （false = 未检查，绝不能显示成"没有依赖"）。
// ⚠️ **绝不再放回市场列表渲染路径**（测试锁死 TestMarketListNeverProbesBrewDependencies）。
//
// ⚠️ 超时 45 秒（2026-09-19 实测踩到）：brew 更新/加载 tap 能挂几分钟，挂住就是整页白屏
// （TestMarketZombieColimaPlistNotInstalled 曾卡到 15 分钟超时）；拿不到结论就如实报"未检查"。
func (m *Manager) brewUsesInstalled(ctx context.Context, formula string) ([]string, bool) {
	if formula == "" {
		return nil, false
	}
	if m.brewUsesProbe != nil {
		return m.brewUsesProbe(ctx, formula)
	}
	brewUsesMu.Lock()
	if ent, ok := brewUsesCache[formula]; ok && time.Since(ent.at) < brewUsesTTL {
		brewUsesMu.Unlock()
		return ent.deps, ent.ok
	}
	brewUsesMu.Unlock()

	// 给自己一个硬上限：调用方（市场列表）的 ctx 可能没有 deadline。
	qctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := m.brewRun(qctx, 45*time.Second, "uses", "--installed", formula)
	ok := err == nil
	deps := []string{}
	if ok {
		for _, ln := range strings.Split(out, "\n") {
			ln = strings.TrimSpace(ln)
			if ln != "" {
				deps = append(deps, ln)
			}
		}
	}
	// 只缓存**成功**结果：失败缓 5 分钟会让"未检查"粘住，用户修好 brew 后刷新也还是它。
	if ok {
		brewUsesMu.Lock()
		brewUsesCache[formula] = brewUsesEntry{deps: deps, ok: ok, at: time.Now()}
		brewUsesMu.Unlock()
	}
	return deps, ok
}

// UninstallApp 执行卸载：Kind=service/forget 由 web 层走既有接口，这里只处理"面板安装器"。
// force=true 只在用户明确选了强制卸载时由 web 层传入，且只转交给真的跑 brew uninstall 的安装器
// （python / phpmyadmin）；其余安装器没有 brew 依赖这回事，忽略它。
func (m *Manager) UninstallApp(ctx context.Context, appID string, removeData, force bool, result *InstallResult) error {
	app, ok := FindApp(appID)
	if !ok {
		// 条目已从市场移除，但机器上可能**已经装了** —— 必须仍然删得掉，否则就是
		// "界面上没了、磁盘上还在跑、用户无处可点"。
		// 刻意**不填** PanelInstaller：留空才落入下面的"残留清理"分支。
		app = App{ID: appID, Name: appID}
	}
	// 没有 PanelInstaller 的应用（compose/docker 类，或旧版原生安装的残留）：删掉磁盘上真实存在的目录。
	// 必须在这里处理，否则会报"没有对应的卸载实现"（2026-09-16 "lucky 根本没被卸载掉"）。
	if app.PanelInstaller == "" {
		targets := []string{}
		// 条目已从目录移除时（!ok）不知道它当初是 compose 还是原生，两种位置都查一遍 ——
		// 漏查一种就等于"点了删不掉"。
		if !ok || app.Kind == KindCompose || app.Kind == KindDocker {
			targets = append(targets, filepath.Join(m.composeDir(), app.ID))
		}
		if d := m.legacyNativeDir(app); d != "" {
			targets = append(targets, d)
		}
		// 只删目录不摘服务的话，launchd 会一直尝试重启一个已不存在的二进制（KeepAlive）。
		// 标签候选要**多试几个**：目录里写死的 homebrew.mxcl.<f> 与磁盘上真实的 cn.zizdog.<f> /
		// sh.brew.<f> 常不一致（坑："PHP 8.3 点纳管报找不到 plist"），只试一个就会把僵尸 plist 漏掉。
		removedService := false
		for _, pl := range m.residualServicePlists(app) {
			if result != nil {
				result.step(ctx, "停止并删除残留的 launchd 服务 "+pl.Label)
			}
			_ = m.removeService(ctx, pl.Label, pl.Plist)
			removedService = true
		}
		removed := 0
		for _, d := range targets {
			if !dirExists(d) {
				continue
			}
			if result != nil {
				result.step(ctx, "删除 "+d)
			}
			if err := os.RemoveAll(d); err != nil {
				return fmt.Errorf("删除 %s 失败: %w", d, err)
			}
			removed++
		}
		if removed == 0 && !removedService {
			return fmt.Errorf("「%s」没有可清理的残留（磁盘上找不到它的目录与 launchd 服务）", app.Name)
		}
		// **不自动删镜像**（怕误删共用的层），只把名字如实告诉用户（下架条目靠 legacyComposeImages）。
		if result != nil {
			if imgs := composeImagesForPlan(app); len(imgs) > 0 {
				result.step(ctx, "镜像未删除（面板不自动 docker rmi）："+strings.Join(imgs, " "))
			}
		}
		return nil
	}
	if fn, ok := installerUninstalls[app.PanelInstaller]; ok {
		return fn(m, ctx, app, removeData, force, result)
	}
	// "官方 release 原生二进制"类应用：查注册表，免得加新应用时忘记补卸载。
	if _, ok := releaseBinaryApps[app.PanelInstaller]; ok {
		return m.UninstallReleaseBinary(ctx, app.PanelInstaller, removeData, result)
	}
	return fmt.Errorf("「%s」没有对应的卸载实现（PanelInstaller=%q）", app.Name, app.PanelInstaller)
}

// installerUninstalls 是"面板自研安装器 → 卸载实现"的**唯一注册表**（用户第三次报"装上了卸不掉"：
// 安装器清单与卸载实现分散两处时，漏补任何一处就是"有安装入口、点了报没有实现"）。
// UninstallApp 只查这张表，全目录门禁测试 TestCatalogUninstallActionMatrix 也查它 —— 有安装器却没有卸载实现，**测试阶段就会失败**。
var installerUninstalls = map[string]func(m *Manager, ctx context.Context, app App, removeData, force bool, result *InstallResult) error{
	"qwen3tts": func(m *Manager, ctx context.Context, _ App, removeData, _ bool, r *InstallResult) error {
		return m.uninstallQwen(ctx, removeData, r)
	},
	"voicereceiver": func(m *Manager, ctx context.Context, _ App, removeData, _ bool, r *InstallResult) error {
		return m.uninstallReceiver(ctx, removeData, r)
	},
	"iopaint": func(m *Manager, ctx context.Context, _ App, removeData, _ bool, r *InstallResult) error {
		return m.uninstallIOPaint(ctx, removeData, r)
	},
	"phpmyadmin": func(m *Manager, ctx context.Context, app App, removeData, force bool, r *InstallResult) error {
		return m.UninstallPhpMyAdmin(ctx, app, removeData, force, r)
	},
	// 基础依赖也允许单独卸载（用户要求"明确提示即可，不要禁止"），后果由 UninstallBaseDependency 写清；
	// 它不走 brew uninstall（保留包），force 无意义。
	"ffmpeg": func(m *Manager, ctx context.Context, app App, _, _ bool, r *InstallResult) error {
		return m.UninstallBaseDependency(ctx, app, r)
	},
	"docker-runtime": func(m *Manager, ctx context.Context, _ App, removeData, _ bool, r *InstallResult) error {
		return m.uninstallDockerRuntime(ctx, removeData, r)
	},
	"miniflux": func(m *Manager, ctx context.Context, _ App, removeData, _ bool, r *InstallResult) error {
		return m.uninstallMiniflux(ctx, removeData, r)
	},
	"syncthing": func(m *Manager, ctx context.Context, _ App, removeData, _ bool, r *InstallResult) error {
		return m.uninstallSyncthing(ctx, removeData, r)
	},
	// Python 解释器（三个版本共用这一个安装器，见 python_runtime.go）：卸载必须点名"谁还在用它"
	// （venv 只是指向它，卸掉后那些服务直接起不来）；force 由 UninstallPythonRuntime 处理。
	"python": func(m *Manager, ctx context.Context, app App, _, force bool, r *InstallResult) error {
		return m.UninstallPythonRuntime(ctx, app, force, r)
	},
	// 图片压缩（libvips）：卸载就是 brew uninstall，如实说明后果，但用户的图片一张都不会动。
	"imgcompress": func(m *Manager, ctx context.Context, app App, _, _ bool, r *InstallResult) error {
		return m.UninstallImageCompressor(ctx, app, r)
	},
	// macOS 语音合成（say）：卸载只摘服务与面板记录，**不动系统** —— say 是 macOS 的一部分，
	// 面板不会也不能删它。
	"macspeech": func(m *Manager, ctx context.Context, app App, _, _ bool, r *InstallResult) error {
		return m.UninstallMacSpeech(ctx, app, r)
	},
	// 语音转文字（whisper.cpp）：停服务 + 撤 plist + 按用户选择删模型 + brew uninstall 引擎；
	// removeData 决定"几百 MB ~ 1.5 GB 的模型权重留不留"（确认框先列出路径与体积）。
	"stt": func(m *Manager, ctx context.Context, app App, removeData, _ bool, r *InstallResult) error {
		return m.UninstallSTT(ctx, app, removeData, false, r)
	},
	// mac军刀（MacSaber）：bootout system 域 + 删系统 plist（兼容残留的旧用户级
	// agent）+ 删 /opt/macsaber；数据目录与 ~/MacSaberFiles **默认保留**
	//（里面是账号、审计日志与用户自己的文件）。
	"macsaber": func(m *Manager, ctx context.Context, app App, removeData, _ bool, r *InstallResult) error {
		return m.UninstallMacSaber(ctx, app, removeData, r)
	},
}

// HasInstallerUninstall 报告某个面板安装器有没有卸载实现。
// 导出给门禁测试用：有安装器却没有卸载实现 = 装上就卸不掉。
func HasInstallerUninstall(panelInstaller string) bool {
	if panelInstaller == "" {
		return false
	}
	if _, ok := installerUninstalls[panelInstaller]; ok {
		return true
	}
	_, ok := releaseBinaryApps[panelInstaller]
	return ok
}

// StopAndForget 停掉服务并删除面板记录，返回"是否真的停过它"。
// 只删记录会让仍在运行的服务彻底消失在面板里（"移除却不卸载"），所以：
//
//   - 停成功 → 删记录；
//
//   - 停失败但**确实不在跑** → 也删记录（没有"隐身运行"风险）；
//
//   - 停失败且仍在跑 → **拒绝删记录**并如实报错（用户要求）。
func (m *Manager) StopAndForget(ctx context.Context, name string) (bool, error) {
	if _, err := m.Action(ctx, name, "stop"); err != nil {
		if errors.Is(err, ErrServiceNotFound) {
			return false, err
		}
		drv, _, derr := m.driver(ctx, name)
		if derr != nil {
			if errors.Is(derr, ErrServiceNotFound) {
				return false, derr
			}
			// 连驱动都构造不出来 → 这个服务**不可能在运行**：允许只删记录（2026-09-17 的卡死场景）。
			if err := m.repo.Delete(ctx, name); err != nil {
				return false, err
			}
			return false, nil
		}
		if st, serr := drv.Status(ctx); serr == nil && st.Running {
			return false, fmt.Errorf("「%s」停止失败（%v）；它现在仍在运行，"+
				"面板不会把它从列表里移除 —— 否则它会变成你看不到的常驻服务。"+
				"请先把它停掉再移除", name, err)
		}
		if err := m.repo.Delete(ctx, name); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := m.repo.Delete(ctx, name); err != nil {
		return true, err
	}
	return true, nil
}

// UninstallBrewApp 执行 Kind=brew 的卸载：停服务 + （可选）删残留 + brew uninstall。
// formula 只认计划里那个（绝不拿目录里的 BrewFormula 再猜 —— 猜错就是删错包）。
// force=true 只由用户显式选择触发：计划 ForceAllowed=false 时 web 层会拒绝，这里再挡一道，绝不悄悄放行。
func (m *Manager) UninstallBrewApp(ctx context.Context, app App, plan UninstallPlan, removeData, force bool, result *InstallResult) error {
	formula := plan.Formula
	if formula == "" {
		return fmt.Errorf("「%s」的卸载计划里没有 Homebrew formula，无法卸载", app.Name)
	}
	if force && !plan.ForceAllowed {
		return fmt.Errorf("拒绝强制卸载 %s：当前计划没有查出依赖，不需要（也不允许）强制卸载", formula)
	}
	// ① 面板记录：停服务 + 删记录（managed=false 的登记记录也走这条，Manager.Uninstall 只收 managed）。
	if plan.Service != "" {
		if result != nil {
			result.step(ctx, "停止「"+plan.Service+"」并删除这条面板记录")
		}
		if _, err := m.StopAndForget(ctx, plan.Service); err != nil && !errors.Is(err, ErrServiceNotFound) {
			return err
		}
	}
	// ② 没有记录但 launchd 里还有服务：先摘掉，否则 KeepAlive 会拉起一个已不存在的二进制。
	if plan.Service == "" {
		if label := m.brewLabelFor(formula); label != "" {
			if result != nil {
				result.step(ctx, "停止并删除 launchd 服务 "+label)
			}
			if err := m.stopLaunchdService(ctx, label, SystemDaemonPlistPath(label)); err != nil {
				return err
			}
		}
	}
	// ③ 残留产物：只有用户勾了「同时删除数据/产物」才删（默认保留）。
	if removeData {
		for _, p := range plan.DataPaths {
			if err := m.removeTree(ctx, p, result); err != nil {
				return err
			}
		}
	} else if len(plan.DataPaths) > 0 && result != nil {
		result.step(ctx, "保留残留目录（需要彻底清理请勾选「同时删除数据/产物」）")
	}
	// ④ brew uninstall（真实执行，失败如实报；命令标签从参数派生）。
	if !m.brewHas(ctx, formula) {
		if result != nil {
			result.step(ctx, formula+" 已经不在 Homebrew 里（可能刚被卸载过），跳过 brew uninstall")
		}
		return nil
	}
	if err := m.brewUninstall(ctx, formula, force, result); err != nil {
		return err
	}
	if result != nil {
		result.step(ctx, formula+" 已从 Homebrew 卸载")
	}
	// PHP：按计划只清**这一个版本自己的**配置目录（brew 从不删配置，父目录只在空了时才删）。
	if removeData {
		if err := m.cleanupPHPConfigDirs(ctx, plan.PhpVersion, result); err != nil {
			return err
		}
	}
	return nil
}

// brewUninstall 跑一条 `brew uninstall`：默认**绝不加 force 开关**，只有用户明确选了强制才追加
// --ignore-dependencies。统一带 HOMEBREW_NO_AUTOREMOVE=1（用户真机：一次卸载被顺手
// Autoremove 了两个无关包）。走 brewInstallRun 以便测试注入假执行器**锁住命令形状**、标签从真实参数派生。
func (m *Manager) brewUninstall(ctx context.Context, formula string, force bool, result *InstallResult) error {
	args := []string{"uninstall"}
	if force {
		args = append(args, "--ignore-dependencies")
	}
	args = append(args, formula)
	if result != nil {
		if force {
			result.step(ctx, "⚠️ 强制卸载（ignore dependencies）：brew uninstall --ignore-dependencies "+
				formula+"（依赖它的包会缺依赖、可能无法运行）")
		} else {
			result.step(ctx, "brew uninstall "+formula)
		}
	}
	out, err := m.brewInstallRun(ctx, 10*time.Minute,
		brewInstallSource{Name: "当前镜像", Env: m.brewEnv(ctx, formula)}, args...)
	if err != nil {
		return fmt.Errorf("%s", brewUninstallErrText(formula, force, out, err))
	}
	return nil
}

// brewUninstallErrText 把 brew uninstall 的失败**翻译**成用户能据以行动的说明（真机
// 卸载 python@3.13 时面板只贴整段英文原文，用户不知道谁依赖它、还能怎么办）。
// 一句人话 + 两个明确动作；**原始输出不丢**，跟在后面供排查。
func brewUninstallErrText(formula string, force bool, out string, err error) string {
	deps := parseBrewRequiredBy(out)
	raw := strings.TrimSpace(out)
	if raw == "" {
		raw = err.Error()
	}
	prefix := "卸载 " + formula + " 失败："
	if len(deps) > 0 {
		if force {
			// 已经强制了还被拒：如实说清，并给下一步。
			return prefix + "即使加了 --ignore-dependencies 仍被 Homebrew 拒绝。" +
				"可先在终端执行 `brew uninstall --ignore-dependencies " + formula + "` 查看完整原因。" +
				"原始输出：" + truncate(raw, 500)
		}
		return prefix + strings.Join(deps, "、") + " 依赖它，brew 拒绝卸载。" +
			"可以先卸载它们（如果确实不需要了），或选择「强制卸载」" +
			"（brew uninstall --ignore-dependencies " + formula + "，会破坏 " +
			strings.Join(deps, "、") + "：它们会缺依赖、可能无法运行）。" +
			"原始输出：" + truncate(raw, 500)
	}
	if isBrewRefusingUninstall(out) {
		return prefix + "Homebrew 拒绝卸载它（有已安装的包依赖它，或它是别人的依赖）。" +
			"可以先运行 `brew uses --installed " + formula + "` 看是谁在用。" +
			"原始输出：" + truncate(raw, 500)
	}
	return prefix + truncate(raw, 500)
}

// ---------- PHP 卸载：只动"这一个版本"的配置 ----------

// phpEtcDir 是 brew 放 PHP 配置的父目录（<prefix>/etc/php）。
func (m *Manager) phpEtcDir() string {
	return filepath.Join(m.brewPrefix(), "etc", "php")
}

// phpVersionOfFormula 从 formula 取 PHP 版本号（"php@8.4" → "8.4"；"php" → ""）。
// 允许空：无版本别名 `php` 推不出 etc/php/8.4 —— 宁可不列任何配置目录，
// 也**绝不**把别的版本（8.2）写进要删的名单（用户报障）。
func phpVersionOfFormula(formula string) string {
	f := strings.TrimSpace(formula)
	if f == "php" {
		return ""
	}
	if !strings.HasPrefix(f, "php@") {
		return ""
	}
	v := strings.TrimPrefix(f, "php@")
	for _, r := range v {
		if (r < '0' || r > '9') && r != '.' {
			return ""
		}
	}
	return v
}

// phpVersionFor 决定"这个 PHP formula 在 etc/php 下的版本目录名"：先取 formula 自带版本
// （`php@8.4` → "8.4"），否则用 brew 报出的真实版本取主次号（"8.4.7" → "8.4"）。
// 两条都不成立就返回空 —— 宁可不列配置目录，也**绝不**猜版本号（猜错就删到 8.2，报障）。
func (m *Manager) phpVersionFor(formula, brewVersion string) string {
	if v := phpVersionOfFormula(formula); v != "" {
		return v
	}
	if strings.TrimSpace(formula) != "php" {
		return ""
	}
	v := strings.Split(strings.TrimSpace(brewVersion), " ")[0] // "8.4.7_1"（多版本空格分隔时取第一个）
	if i := strings.Index(v, "_"); i > 0 {
		v = v[:i] // 8.4.7_1 → 8.4.7
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return ""
	}
	major, minor := parts[0], parts[1]
	if !allDigits(major) || !allDigits(minor) {
		return ""
	}
	return major + "." + minor
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// phpConfigPathsFor 返回"这个 PHP 版本自己的配置目录"（版本为空 → 什么都不返回）。
// brew 的 warning 会笼统列出整个 etc/php：里面 8.2 的路径属于**另一个版本**、
// phpmyadmin 的配置属于另一个应用（真机）。
//
// 本函数只认 <prefix>/etc/php/<版本>，且只有真的存在才列出；父目录为空时才由 cleanupPHPConfigDirs 删。
func (m *Manager) phpConfigPathsFor(version string) []string {
	if strings.TrimSpace(version) == "" {
		return nil
	}
	dir := filepath.Join(m.phpEtcDir(), version)
	if !dirExists(dir) {
		return nil
	}
	return []string{dir}
}

// cleanupPHPConfigDirs 删除"这个 PHP 版本自己的"配置目录（**只在父目录空了**时删父目录）。
// 铁律：绝不碰其它版本（etc/php/8.2）与 phpmyadmin 的配置；父目录里有任何别的东西就保留。
//
// version 与计划算 DataPaths 用的是同一个（phpVersionFor），确认框写什么就删什么。
func (m *Manager) cleanupPHPConfigDirs(ctx context.Context, version string, result *InstallResult) error {
	if strings.TrimSpace(version) == "" {
		return nil
	}
	dir := filepath.Join(m.phpEtcDir(), version)
	if !dirExists(dir) {
		return nil
	}
	if err := m.removeTree(ctx, dir, result); err != nil {
		return err
	}
	// 父目录：只有空掉才删。ReadDir 失败（不存在/无权限）一律保留 —— 宁可留一个空目录。
	parent := m.phpEtcDir()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) > 0 {
		if err == nil && result != nil {
			result.step(ctx, "保留 "+parent+"（里面还有其它版本/应用的配置，不能删）")
		}
		return nil
	}
	if result != nil {
		result.step(ctx, "删除空目录 "+parent)
	}
	if err := os.Remove(parent); err != nil && !os.IsNotExist(err) {
		// 删不掉不是失败：目录还在也不影响任何功能，如实记一行即可。
		if result != nil {
			result.step(ctx, "没能删除空目录 "+parent+"（"+err.Error()+"），不影响使用")
		}
	}
	return nil
}

// parseBrewRequiredBy 从 brew 的拒绝文案里取出依赖方包名（"required by llvm and rust" → ["llvm","rust"]）。
// 解析不出来就返回空（调用方退回通用文案 + 原始输出），**绝不编造包名**。
func parseBrewRequiredBy(out string) []string {
	low := strings.ToLower(out)
	idx := strings.Index(low, "because it is required by")
	if idx < 0 {
		return nil
	}
	rest := out[idx+len("because it is required by"):]
	// 取到句末/换行；"llvm and rust, which are currently installed."
	if i := strings.IndexAny(rest, ".\n"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.Index(rest, ","); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil
	}
	// 用 FieldsFunc 按逗号/空白切并丢掉纯连接词，避免手写拆分漏掉 "llvm and rust" 这种形态
	// （拆错就会把整句当包名）。
	deps := []string{}
	for _, p := range strings.FieldsFunc(rest, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	}) {
		switch strings.ToLower(p) {
		case "", "and", "or", "&":
			continue
		}
		deps = append(deps, p)
	}
	return deps
}

// isBrewRefusingUninstall 识别 brew 的"因依赖而拒绝卸载"输出（各版本措辞不一）。
func isBrewRefusingUninstall(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "refusing to uninstall") ||
		strings.Contains(low, "is required by") ||
		strings.Contains(low, "ignore-dependencies")
}

// installerPlan 按安装器给出卸载计划。
func (m *Manager) installerPlan(ctx context.Context, app App) UninstallPlan {
	// "官方 release 原生二进制"类应用：一次注册表判断覆盖全部，加新条目不用回来补 case
	// （漏补的后果是卸载按钮报"没有卸载实现"，而东西确实是面板装的）。
	if plan, ok := m.releaseBinaryPlan(app.PanelInstaller); ok {
		return plan
	}
	p := UninstallPlan{Kind: "installer"}
	switch app.PanelInstaller {
	case "qwen3tts":
		p.Steps = []string{"停止并删除 launchd 服务 " + qwenLabel, "从「服务管理」移除记录"}
		// 只列**我们清单里的模型目录**，不写整个 HF hub（那里还有别的项目的模型，
		// 写它会让人以为要全删）；实现 uninstallQwen 也只删这几个目录。
		p.DataPaths = []string{filepath.Join(m.opt.UserHome, "tts", "qwen3")}
		hub := filepath.Join(m.opt.UserHome, ".cache", "huggingface", "hub")
		for _, mdl := range QwenModels {
			p.DataPaths = append(p.DataPaths,
				filepath.Join(hub, "models--"+strings.ReplaceAll(mdl.Name, "/", "--")))
		}
		p.KeepNote = "默认保留虚拟环境与模型权重（重新部署时不用重新下载约 3GB）"
	case "voicereceiver":
		p.Steps = []string{"停止并删除 launchd 服务 " + receiverLabel, "从「服务管理」移除记录"}
		p.DataPaths = []string{
			filepath.Join(m.opt.UserHome, "tts", "voice-receiver"),
			filepath.Join(m.opt.UserHome, "tts", "voice-samples"),
			filepath.Join(m.opt.UserHome, "tts", "jobs"),
		}
		p.KeepNote = "默认保留音色样本与历史合成任务（它们是你的数据）"
	case "iopaint":
		p.Steps = []string{"停止并删除 launchd 服务 " + iopaintLabel, "从「服务管理」移除记录"}
		p.DataPaths = []string{filepath.Join(m.opt.UserHome, "iopaint")}
		p.KeepNote = "默认保留 ~/iopaint（虚拟环境，重新部署时不用重装依赖）"
	case "phpmyadmin":
		// 安装体是**磁盘上的 web 根目录**（目录里 RuntimePath 声明的那个）：面板既然凭"目录在"
		// 说它已安装，卸载就必须真的删掉，否则卡片永远停在「已安装」（2026-09-16 那一类）。
		// 路径与执行端共用**同一个**解析函数，避免"计划说删 A、实际删 B"。
		pmaShare := ResolveRuntimePath(app.RuntimePath, m.brewPrefix(), m.opt.UserHome)
		if pmaShare == "" {
			pmaShare = m.pmaPaths().Share
		}
		p.Steps = []string{
			"移除 nginx 默认站点里的 phpMyAdmin 入口并重载",
			"brew uninstall phpmyadmin（Homebrew 里没有它时跳过）",
			"移除 phpMyAdmin 的 web 根目录 " + pmaShare,
		}
		p.DataPaths = []string{filepath.Join(m.brewPrefix(), "etc", "phpmyadmin.config.inc.php")}
		p.KeepNote = "面板自研的库表管理功能不受影响；你的数据库与库表**一个都不会动**" +
			"（web 根里放的是 phpMyAdmin 程序本身，配置在同级的 etc/ 下，默认保留）"
	case "docker-runtime":
		p.Steps = []string{
			"停止并删除 Colima 虚拟机（**其中的容器、镜像、卷都会消失**）",
			"从「服务管理」移除记录",
		}
		p.KeepNote = "Homebrew 包保留；要彻底移除请自行 brew uninstall colima docker"
		if users := m.composeUsers(ctx); len(users) > 0 {
			p.Blocked = "还有 " + fmt.Sprint(len(users)) + " 个 Docker 应用在用这个运行时（" +
				strings.Join(users, "、") + "），请先卸载它们"
		}
	case "python":
		// 卸载计划必须把"谁会受影响"写在用户按下按钮**之前**；判据是磁盘真实状态（venv 的 pyvenv.cfg）。
		p.Steps = []string{"brew uninstall " + app.BrewFormula}
		if deps := m.pythonRuntimeDependents(app.BrewFormula); len(deps) > 0 {
			p.Steps = append(p.Steps,
				"⚠️ 下列环境正在使用 "+app.BrewFormula+"，卸载后它们会无法启动："+strings.Join(deps, "、"))
			p.KeepNote = "卸载解释器不会删掉那些虚拟环境，但环境里的 python 是**指向它**的链接，" +
				"所以相关服务会起不来（需要重新部署）；要恢复请重装本条目"
		} else {
			p.KeepNote = "当前没有面板内的虚拟环境在用这个版本；卸载不影响其它 Python 版本" +
				"（3.11 / 3.12 / 3.13 各自独立、可并存）"
		}
	case "ffmpeg":
		// 基础依赖也能单独卸载（用户要求"明确提示即可，不要禁止"），后果逐条写进计划让用户先看见。
		p.Steps = []string{
			"⚠️ FFmpeg 是面板的基础依赖，卸载后 TTS 编码 mp3 会返回 HTTP 200 + 0 字节 body",
			"音色接收端的样本校验/转码、以及后续的音视频功能也会失效",
			"brew uninstall ffmpeg",
		}
		p.KeepNote = "需要时可随时从应用市场重新安装；面板安装脚本与 LNMP / Qwen TTS 部署也会自动补装"
	case "imgcompress":
		// 图片压缩有**两个**组成部分：网页界面服务（launchd）与 brew 的 vips 引擎。
		// 只停服务会把 vips 留在机器上（卡片永远"已安装"），只 uninstall 包会把服务留成
		// KeepAlive 复活、却找不到引擎的僵尸（端口占着、界面一直红）。
		p.Steps = []string{
			"停止并删除 launchd 服务 " + ImgCompressLabel + "（网页界面）",
			"brew uninstall " + ImgCompressFormula + "（图片压缩引擎）",
			"⚠️ 卸载后网页界面与「文件管理 → 🖼️ 图片压缩」都会不可用，直到重新安装",
			"你的图片文件**不会被删除或修改**（压缩只在你点「开始压缩」时读写你上传/选中的那些文件）",
		}
		p.KeepNote = "面板不保存任何图片副本（上传的临时文件在任务结束一段时间后自动清理），" +
			"所以没有「要清理的数据目录」；需要时随时可以从应用市场重新安装（原生 arm64 包，装完即用）"
	case "macspeech":
		// macOS 语音合成（say）：引擎 /usr/bin/say 与 afconvert 是 macOS 的一部分，
		// 卸载只摘服务与面板记录，**不动系统**（面板不会也不能删它）。
		// 这条必须写进确认框，否则用户会以为"卸载语音合成"删掉了系统语音能力。
		p.Steps = []string{
			"停止并删除 launchd 服务 " + MacSpeechLabel + "（语音合成网页界面）",
			"从「服务管理」移除记录",
			"⚠️ 卸载后网页界面与面板别名 /speech/ 都会不可用，直到重新安装",
			"系统自带的 /usr/bin/say 与你系统里安装的音色**不会被删除或修改**（那是 macOS 的一部分）",
		}
		p.KeepNote = "面板不保存任何合成音频（临时文件在任务结束后自动清理），所以没有数据目录要清理；" +
			"重新安装是幂等的、**不需要下载任何东西**（这个应用本来就没有下载点）"
	case "stt":
		// 语音转文字 = 网页界面服务（launchd）+ brew 的 whisper.cpp 引擎 + **模型权重**（几百 MB ~ 1.5 GB）。
		// 服务与 plist 总是摘掉（否则留下一个 KeepAlive 复活却找不到引擎的僵尸）。
		// 模型默认**保留**（只是下载产物，重装不用重下），只有勾选「同时删除数据」才删；brew 包总是卸。
		p.Steps = []string{
			"停止并删除 launchd 服务 " + STTLabel + "（语音转文字网页界面）",
			"从「服务管理」移除记录",
			"brew uninstall " + STTBrewFormula + "（引擎 whisper-cli / whisper-server）",
			"⚠️ 卸载后网页界面与面板别名 /" + STTSlug + "/ 都会不可用，直到重新安装",
		}
		models := m.InstalledSTTModels()
		if len(models) > 0 {
			for _, f := range models {
				p.DataPaths = append(p.DataPaths, f.Path)
				p.Steps = append(p.Steps, "可选清理的模型权重："+f.Path+
					"（档位 "+f.ID+"，"+humanBytes(f.Bytes)+"）—— 勾选「同时删除数据」才会删")
			}
			p.KeepNote = "默认**保留**模型权重（共 " + humanBytes(m.STTModelBytes()) +
				"）：它们只是下载产物，重新安装时不用重新下载；" +
				"要释放磁盘请在确认框里勾选「同时删除数据」，或手工删除 " + m.sttPaths().ModelsDir
		} else {
			p.KeepNote = "磁盘上没有模型权重文件，没有要清理的数据；" +
				"重新安装会重新下载默认档模型（约 466 MiB）"
		}
	case "miniflux":
		// 铁律：卸载应用**不删数据**。Miniflux 库里是订阅源与已读状态，删掉不可恢复 ——
		// PostgreSQL 与库一律保留，确认框里写清楚。
		p.Steps = []string{
			"停止并删除 launchd 服务（miniflux）",
			"从「服务管理」移除记录",
			"brew uninstall miniflux",
			"⚠️ 保留 PostgreSQL（postgresql@17）与 miniflux 数据库（订阅与已读状态都在库里）",
			"⚠️ 保留配置文件 " + filepath.Join(m.brewPrefix(), "etc", "miniflux.conf"),
		}
		p.KeepNote = "PostgreSQL 与 miniflux 数据库**都保留**（你的订阅数据在里面），配置文件也保留；" +
			"面板不会替你删数据。要彻底删除数据库请在终端执行：" +
			"psql -h 127.0.0.1 -U <你的登录用户名> -d postgres -c 'DROP DATABASE miniflux'"
	case "syncthing":
		// 同步目录在用户指定的位置（面板不知道），这里只谈"身份与索引"：
		// 删掉数据目录等于重置本机身份，重新同步要重新配对设备。
		p.Steps = []string{
			"停止并删除 launchd 服务（syncthing）",
			"从「服务管理」移除记录",
			"brew uninstall syncthing",
			"⚠️ 默认保留 " + filepath.Join(m.opt.UserHome, "Library", "Application Support", "Syncthing") +
				"（设备密钥、配对信息与同步索引；勾选「删除数据」才会删）",
		}
		p.DataPaths = []string{filepath.Join(m.opt.UserHome, "Library", "Application Support", "Syncthing")}
		p.KeepNote = "默认保留 Syncthing 的数据目录（设备身份与同步索引在里面）；" +
			"删掉它等于重置本机身份，重新同步要重新配对设备。**同步目录本身不在这个目录里**，不受影响。"
	case "macsaber":
		// mac军刀：产物是自研的 LaunchAgent + /opt/macsaber，没有 brew 包要卸。
		// 计划与执行端共用同一个 macSaberInstallPlan（路径只写一份，避免"计划说删 A、
		// 实际删 B"）。
		p = m.macSaberInstallPlan()
	default:
		p.Blocked = "这个应用没有卸载实现"
	}
	return p
}

// findServiceRecord 按应用的候选标签 / 名称找出面板记录。
func (m *Manager) findServiceRecord(ctx context.Context, app App) *Service {
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil
	}
	cand := map[string]bool{}
	for _, v := range []string{app.ServiceLabel, app.AdoptLabel, app.ID, app.Name} {
		if v != "" {
			cand[v] = true
		}
	}
	for _, s := range list {
		if cand[s.Name] || (s.LaunchLabel != "" && cand[s.LaunchLabel]) {
			return s
		}
	}
	return nil
}

// composeUsers 返回当前登记在面板里的 Docker/compose 应用名。
func (m *Manager) composeUsers(ctx context.Context) []string {
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range list {
		if s.Kind == KindCompose || s.Kind == KindDocker {
			out = append(out, s.DisplayName)
		}
	}
	return out
}

// ---------- 各安装器的卸载实现 ----------

func (m *Manager) uninstallQwen(ctx context.Context, removeData bool, result *InstallResult) error {
	if result != nil {
		result.step(ctx, "停止 Qwen3 TTS 服务")
	}
	p := m.qwenPaths()
	if err := m.removeService(ctx, qwenLabel, p.Plist); err != nil {
		return err
	}
	if removeData {
		if err := m.removeTree(ctx, p.Root, result); err != nil {
			return err
		}
		// 模型权重在 HF 缓存里，按目录名逐个删（只删我们清单里的那些）
		hub := filepath.Join(m.opt.UserHome, ".cache", "huggingface", "hub")
		for _, mdl := range QwenModels {
			dir := filepath.Join(hub, "models--"+strings.ReplaceAll(mdl.Name, "/", "--"))
			if err := m.removeTree(ctx, dir, result); err != nil {
				return err
			}
		}
	} else if result != nil {
		result.step(ctx, "保留 "+p.Root+"（虚拟环境与模型；需要彻底清理请勾选删除数据）")
	}
	return nil
}

func (m *Manager) uninstallReceiver(ctx context.Context, removeData bool, result *InstallResult) error {
	if result != nil {
		result.step(ctx, "停止音色接收端")
	}
	p := m.receiverPaths()
	if err := m.removeService(ctx, receiverLabel, p.Plist); err != nil {
		return err
	}
	if removeData {
		for _, dir := range []string{p.Dir, p.Samples, p.Jobs} {
			if err := m.removeTree(ctx, dir, result); err != nil {
				return err
			}
		}
	} else if result != nil {
		result.step(ctx, "保留音色样本与历史合成任务（"+p.Samples+"、"+p.Jobs+"）")
	}
	return nil
}

func (m *Manager) uninstallIOPaint(ctx context.Context, removeData bool, result *InstallResult) error {
	if result != nil {
		result.step(ctx, "停止 IOPaint 服务")
	}
	p := m.iopaintPaths()
	if err := m.removeService(ctx, iopaintLabel, p.Plist); err != nil {
		return err
	}
	if removeData {
		if err := m.removeTree(ctx, p.Root, result); err != nil {
			return err
		}
	} else if result != nil {
		result.step(ctx, "保留 "+p.Root+"（虚拟环境；需要彻底清理请勾选删除数据）")
	}
	return nil
}

func (m *Manager) uninstallDockerRuntime(ctx context.Context, removeData bool, result *InstallResult) error {
	if users := m.composeUsers(ctx); len(users) > 0 {
		return fmt.Errorf("还有 %d 个 Docker 应用在用这个运行时（%s），请先卸载它们",
			len(users), strings.Join(users, "、"))
	}
	if result != nil {
		result.step(ctx, "删除 Colima 虚拟机（容器/镜像/卷会一起消失）")
	}
	if out, err := m.runAsUser(ctx, 3*time.Minute, m.colimaBin(), "delete", "-f"); err != nil {
		return fmt.Errorf("colima delete 失败: %v（%s）", err, tailText(out, 300))
	}
	return m.removeService(ctx, ColimaLaunchLabel, ColimaPlistPath)
}

// ---------- 通用小工具 ----------

// removeService 停止并删除 launchd 服务，再从面板记录里移除。
// 顺序：先 unload/删 plist 再删记录 —— 反了的话进程还在跑、面板却已经没有它了。
func (m *Manager) removeService(ctx context.Context, label, plist string) error {
	if err := m.stopLaunchdService(ctx, label, plist); err != nil {
		return err
	}
	if list, err := m.repo.List(ctx); err == nil {
		for _, s := range list {
			if s.LaunchLabel == label || s.Name == label {
				if err := m.repo.Delete(ctx, s.Name); err != nil {
					return fmt.Errorf("删除面板记录失败: %w", err)
				}
			}
		}
	}
	return nil
}

// stopLaunchdService 停服务并删 plist（幂等：本来就不存在也算成功）。
func (m *Manager) stopLaunchdService(ctx context.Context, label, plist string) error {
	// 先正常停止，失败也继续 —— 目标是"没有它"，删 plist 才是决定性一步。
	// 用 priv.LaunchUnload 而非裸 `bootout system/<label>`：作业可能在 user/gui 域里，
	// 只打 system 域会留下卸载后还占着端口的孤儿进程。
	if label != "" {
		_ = priv.LaunchUnload(label)
	}
	// 两个位置都要清（/Library/LaunchDaemons 与 ~/Library/LaunchAgents），只删一个的话
	// 另一个会让服务在重启后又回来。
	paths := []string{plist, SystemDaemonPlistPath(label)}
	if label != "" {
		paths = append(paths, m.systemDaemonUserPlist(label))
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 %s 失败: %w", p, err)
		}
	}
	return nil
}

func (m *Manager) removeTree(ctx context.Context, path string, result *InstallResult) error {
	if path == "" || path == "/" || path == m.opt.UserHome {
		return fmt.Errorf("拒绝删除危险路径: %q", path)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	if result != nil {
		result.step(ctx, "删除 "+path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("删除 %s 失败: %w", path, err)
	}
	return nil
}

// ComposeArtifactExists 报告某个 compose 应用的**项目目录**是否还在（导出给 web 层
// 如实报"没装但有残留"，它不该知道 compose 目录怎么拼）。
func ComposeArtifactExists(workDir, appID string) bool {
	if workDir == "" || appID == "" {
		return false
	}
	return dirExists(filepath.Join(workDir, "compose", appID))
}

// residualServicePlist 是一对"launchd 标签 + 真实 plist 路径"。
type residualServicePlist struct {
	Label string
	Plist string
}

// residualServicePlists 找出这个应用**磁盘上真实存在**的 launchd 服务定义 —— 候选标签要多试：
// 目录里写 homebrew.mxcl.<f> 而磁盘上是 cn.zizdog.<f> / sh.brew.<f>（坑："PHP 8.3 点纳管报找不到 plist"），
// 只试一个就会把僵尸 plist 当成"没有可清理的残留"（installed=true 却没有任何收尾动作）。
//
// 目录集合走 m.launchdDirsOverride（测试隔离）：单测不许读真机 launchd 状态。
func (m *Manager) residualServicePlists(app App) []residualServicePlist {
	cands := []string{}
	for _, l := range []string{app.ServiceLabel, app.AdoptLabel, "com.zizdog." + app.ID, m.brewLabelFor(app.BrewFormula)} {
		if l == "" {
			continue
		}
		dup := false
		for _, c := range cands {
			if c == l {
				dup = true
				break
			}
		}
		if !dup {
			cands = append(cands, l)
		}
	}
	dirs := []string{"/Library/LaunchDaemons"}
	if len(m.launchdDirsOverride) > 0 {
		dirs = m.launchdDirsOverride
	}
	out := []residualServicePlist{}
	for _, l := range cands {
		for _, d := range dirs {
			p := filepath.Join(d, l+".plist")
			if fileExists(p) {
				out = append(out, residualServicePlist{Label: l, Plist: p})
				break
			}
		}
	}
	return out
}

// legacyNativeDir 返回"这个应用在用户家目录下**残留的**原生安装目录"（2026-09-16 Lucky 注册表
// 移除后残留既卸不掉、卡片还显示已安装）。判据刻意保守：
//   - 只认 `~/<appID>` 这一层（不递归扫），且目录里必须有**可执行文件**才算残留；compose 应用不参与。
func (m *Manager) legacyNativeDir(app App) string {
	if m.opt.UserHome == "" || app.ID == "" {
		return ""
	}
	dir := filepath.Join(m.opt.UserHome, app.ID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr == nil && info.Mode()&0o111 != 0 {
			return dir
		}
	}
	return ""
}

// LegacyNativeArtifactExists 报告"是否还有残留的原生安装目录"（导出给 web 层报"没装但有残留"，
// 内部复用同一套判据，避免两处判断漂移）。
func LegacyNativeArtifactExists(userHome, appID, kind string) bool {
	if userHome == "" || appID == "" {
		return false
	}
	m := &Manager{opt: Options{UserHome: userHome}}
	return m.legacyNativeDir(App{ID: appID}) != ""
}

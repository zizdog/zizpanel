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

// brewUsesCache 缓存 `brew uses --installed <formula>` 的结果。
//
// 为什么是**包级**而不是 Manager 字段：svcManager() 每次请求都新建一个 Manager
// （设置页保存后立刻生效），挂在 Manager 上的缓存活不过一次请求。依赖关系是
// 低频数据，5 分钟足够；键是 formula，值是"依赖它的已装包"。
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

// resetBrewUsesCache 仅供测试：清掉进程级缓存，避免用例之间互相污染。
func resetBrewUsesCache() {
	brewUsesMu.Lock()
	brewUsesCache = map[string]brewUsesEntry{}
	brewUsesMu.Unlock()
}

// ============================================================================
//  卸载"面板自己装的应用"
//
//  背景（用户反馈）：应用市场里装上应用之后**没有任何卸载入口** ——
//  而面板的通用卸载（Manager.Uninstall）只认"托管服务"，面板自研安装器装的
//  那几个（IOPaint / Qwen3 TTS / 音色接收端 / phpMyAdmin）注册时是**纳管**状态，
//  那个入口会直接拒绝（"这是纳管服务，面板不会卸载它"）。
//  于是用户装了 IOPaint，在面板里找不到任何办法把它卸掉。
//
//  这里的语义分三类，必须分清楚（否则会删掉用户自己的东西）：
//
//   1. **managed 服务**（compose 应用、面板注册的 brew 服务）
//      → 走既有的 Manager.Uninstall。
//   2. **面板自研安装器装的应用**（PanelInstaller 非空）
//      → 走本文件的 UninstallApp：卸服务 + 删 plist + 删面板记录，
//        并按用户选择删除安装产物（虚拟环境、模型、样本、任务）。
//   3. **纳管的第三方服务**（nginx / php / mysql / 用户自己注册的）
//      → **绝不卸载**。只提供「取消纳管」（把记录从面板移除，不动系统）。
//        这条是铁律：删掉用户自己装的 MySQL 等于删掉他的网站数据。
// ============================================================================

// UninstallPlan 是"卸载这个应用会做什么"的说明。
//
// 为什么要给界面：卸载是不可逆的。确认框里必须写清楚**具体会删哪些路径**、
// 以及**哪些东西会保留**（例如模型缓存、近期的合成任务），
// 而不是一句"确定卸载吗"。这也是这个项目一贯的要求：用户要知道自己按的是什么。
type UninstallPlan struct {
	// Kind: service（托管服务）/ installer（面板安装器）/ brew（Homebrew 包）
	// / forget（用户自己装的、面板只登记过 —— 只允许从列表移除）
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
	//
	// 必须用**判定时采用的那个写法**，不能拿目录里的 BrewFormula 再猜一次：
	// 目录写 `php@8.4` 而机器上装的是 `php`（当年安装时它正是 8.4）这种情况，
	// 卸载要跑的是 `brew uninstall php`（见 ResolveBrewFormula）。
	Formula string `json:"formula,omitempty"`
	// PhpVersion 是 PHP 卸载时"属于这一次的版本号"（"8.4"）。
	//
	// 为什么不能执行时重新从 Formula 推：真机上装的是无版本别名 `php`（8.4.7），
	// 从 `php` 推不出 8.4，而配置目录就叫 etc/php/8.4。计划阶段已经把版本算准
	// （phpVersionFor），这里原样带给执行端 —— 保证"确认框里写什么"与
	// "真的删什么"逐字一致（多推一次就可能推出另一个版本）。
	PhpVersion string `json:"php_version,omitempty"`
	// Dependents 是"这次卸载会影响谁"（结构化）：站点 / 容器 / 面板应用 / brew 包。
	// 前端据此逐条列出"谁在用它、该怎么办"，而不是一句笼统的"不能卸载"。
	// 见 dependents.go —— 那是唯一的依赖判定实现。
	Dependents []Dependent `json:"dependents,omitempty"`
	// DependentsChecked 表示上面那份名单**真的查过**。
	// false = 没查成（brew 不可用/超时）→ 界面必须如实写"未检查"，绝不能说"没有依赖"。
	DependentsChecked bool `json:"dependents_checked,omitempty"`
	// ForceAllowed 表示"这个计划可以用强制卸载（brew uninstall --ignore-dependencies）"。
	//
	// 2026-09-21 用户真机（卸载 python@3.13）：
	//   Error: Refusing to uninstall ... because it is required by llvm and rust,
	//   which are currently installed.
	//   You can override this and force removal with:
	//     brew uninstall --ignore-dependencies python@3.13
	// 以前面板把这段 brew 原文当错误贴回去，用户既看不懂、也不知道还能怎么办。
	// 现在计划阶段就把"谁依赖它"说清楚，并明确给出**两个选择**：
	//   · 取消（默认，什么都不做）；
	//   · 强制卸载 —— 只有这一条路会把依赖它的包弄坏。
	//
	// 语义边界（必须守住）：它只是"允许用户选"，**绝不允许默认加 --ignore-dependencies**。
	// 执行端只有在请求显式带 force=true 时才会追加那个开关（见 UninstallBrewApp）。
	ForceAllowed bool `json:"force_allowed,omitempty"`
	// ForceNote 是"强制卸载会破坏什么"的逐字说明（界面直接展示，不用自己拼）。
	ForceNote string `json:"force_note,omitempty"`
}

// PlanUninstall 给出某个目录应用当前该怎么卸载（只读，不产生任何改动）。
func (m *Manager) PlanUninstall(ctx context.Context, appID string) UninstallPlan {
	app, ok := FindApp(appID)
	if !ok {
		// 条目可能已经**从目录下架**（2026-09-17 移除 n8n），但用户机器上
		// 可能还留着它的 compose 项目目录 —— 那台机器必须仍然给得出清理计划，
		// 否则就是"界面上没了、磁盘上还在、用户无处可点"。
		// 用一个最小 App（只有 ID）继续走 PlanUninstallFor 的残留分支。
		app = App{ID: appID, Name: appID}
	}
	return m.PlanUninstallFor(ctx, app, m.findServiceRecord(ctx, app))
}

// legacyComposeImages 记录**已从目录下架**、但机器上可能还有 compose 目录的应用镜像。
//
// 为什么需要：卸载计划里应当写清"哪些镜像可以手动删掉释放空间"（面板不自动
// `docker rmi`，怕误删被别的项目共用的层）。条目还在目录里时镜像从 ComposeYAML
// 现取；下架之后没有来源，只能在这里留一份。
var legacyComposeImages = map[string][]string{
	// 2026-09-17 下架（用户已手动卸载并明确要求移除）。
	"n8n": {"n8nio/n8n:latest"},
	// 2026-09-17 下架（用户明确要求删除条目）：**条目下架 != 卸载**，
	// 这台机器上两个应用都还装着。卸载计划必须仍然说得出它们的镜像名，
	// 否则用户删完 compose 目录后镜像还占着几百 MB 而计划里一个字都不提。
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

// BrewState 是"这个条目在 Homebrew 里的真实状态"。
//
// 为什么要由调用方算好传进来：应用市场一次要给出十几个条目的计划，而每次
// `brew list --versions` 都要 0.4~0.6 秒。web 层本来就有整批缓存
// （见 Server.installedFormulas），计划函数自己再跑一遍 brew 会把列表拖慢数秒。
type BrewState struct {
	// Formula 是判定采用的实际 formula，可能与目录里的写法不同
	// （目录写 php@8.4，机器上装的是 php —— 见 ResolveBrewFormula）。
	Formula string
	// Version 是 `brew list --versions` 给出的版本串（如 "8.4.7" / "8.4.7_1"）。
	// 用于推导"这个包在 etc 下的版本目录名"（PHP 的 etc/php/<版本>）——
	// 无版本别名的 `php` 只有靠它才知道要清理的是 etc/php/8.4 而不是别的版本。
	Version string
	// Installed 表示 Formula 真的在 brew list 里。
	Installed bool
}

// ResolveBrewFormula 决定"这个目录条目对应机器上哪个已装的 brew formula"。
//
// 先精确匹配；不中且目录写的是带版本后缀的 formula（php@8.4）时，再看
// **同名的无后缀 formula**（php）装的是不是同一个版本。
//
// 为什么必须有这条规则（2026-09-21 用户现场）：这台机器上 `brew list` 是
// `php 8.4.7`（用户当年 `brew install php`，那时 php 就是 8.4），而目录条目
// php84 写的是 `php@8.4`。只做精确匹配，面板就会对一台明明装着 PHP 8.4 的
// 机器显示"未安装"（用户原话："没安装的显示安装"），并且因为没有可卸载对象
// 而**一颗收尾按钮都不给**。这是同一类问题的两个方向，必须一起修。
//
// installed 是 formula → 版本串（`brew list --versions` 的整批结果）。
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

// PlanUninstallFor 与 PlanUninstall 相同，但用调用方已经查好的记录。
//
// 计划里需要 brew 的真实状态（"装了 brew 包但没有面板记录"这一态必须给得出
// 卸载入口），这里按需查一次；web 层的市场列表用 PlanUninstallForBrew 传入
// 整批缓存，避免每个条目各跑一次 brew。
func (m *Manager) PlanUninstallFor(ctx context.Context, app App, rec *Service) UninstallPlan {
	return m.PlanUninstallForBrew(ctx, app, rec, m.resolveBrewState(ctx, app))
}

// resolveBrewState 查一次这台机器上这个条目的 brew 状态（单个应用用；
// 市场列表请用 PlanUninstallForBrew + 整批缓存）。
func (m *Manager) resolveBrewState(ctx context.Context, app App) BrewState {
	if app.BrewFormula == "" {
		return BrewState{}
	}
	installed := m.installedFormulaVersions(ctx)
	return BrewStateFor(app.BrewFormula, installed)
}

// installedFormulaVersions 取整批 formula → 版本，测试可注入。
func (m *Manager) installedFormulaVersions(ctx context.Context) map[string]string {
	if m.brewInstalledProbe != nil {
		return m.brewInstalledProbe(ctx)
	}
	return m.InstalledFormulaVersions(ctx)
}

// PlanUninstallForBrew 与 PlanUninstallFor 相同，但 brew 状态由调用方按整批缓存传入。
//
// 所有分支的产出都会过一遍依赖引擎（dependents.go）：说得出"谁在用它、该怎么办"，
// 而不是一句笼统的"不能卸载"。
func (m *Manager) PlanUninstallForBrew(ctx context.Context, app App, rec *Service, brew BrewState) UninstallPlan {
	plan := m.planUninstallForBrewCore(ctx, app, rec, brew)
	m.ApplyDependents(ctx, app, rec, &plan)
	m.brewDependencyBlockForInstaller(ctx, app, brew, &plan)
	return plan
}

// brewDependencyBlockForInstaller 给"面板安装器但实际靠 brew 卸载"的条目补上
// **brew 依赖**这一层（用户真机卸载 python@3.13 的那一类）。
//
// 为什么单独一个函数而不是写进 installerPlan：installerPlan 没有 context，
// 而 `brew uses --installed` 是一次真实 brew 调用；而且它必须与其它分支一样
// **只查一次**（brewUsesInstalled 自带 5 分钟进程级缓存）。
//
// 只有"卸载实现里真的会跑 brew uninstall"的安装器才查：python 与 phpmyadmin。
// 其余安装器（qwen3tts / iopaint / docker-runtime …）要么不动 brew，要么
// 明确保留 brew 包，问了只会得到噪音。
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
//
// 为什么要从步骤里取（而不是用目录里的 BrewFormula 再猜一次）：目录写
// php@8.4、机器上装的是 php 8.4.7，真正要卸的是后者 —— 计划里那一步是
// 唯一权威的写法（与 UninstallBrewApp 的规则一致）。
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

// planUninstallForBrewCore 是计划的本体（不含依赖检测，便于测试单独驱动）。
func (m *Manager) planUninstallForBrewCore(ctx context.Context, app App, rec *Service, brew BrewState) UninstallPlan {
	if rec != nil && rec.Managed && app.PanelInstaller == "" {
		if app.Kind == KindCompose || app.Kind == KindDocker {
			keep := "compose 应用只删容器与网络，**具名卷（数据）保留**"
			steps := []string{
				"docker compose down（删除容器与网络）",
				"从「服务管理」中删除这条记录",
			}
			// 把镜像名也写进计划：卸载不删镜像（怕误删共用层），但用户
			// 有权知道"哪些镜像还占着磁盘、怎么清"。
			if note := composeImageNote(composeImagesForPlan(app)); note != "" {
				keep = keep + "；" + note
			}
			return UninstallPlan{Kind: "service", Service: rec.Name, Steps: steps, KeepNote: keep}
		}
		// brew 原生托管服务：**软件本身也是 brew 装的**。只删服务定义的话，
		// 用户点了「卸载」、界面说成功了，brew 包还原封不动 —— 这正是用户
		// 反馈的"卸载不了"（2026-09-21：php82 的计划只有"停服务 + 删记录"）。
		// 真实动作 = 停服务 + 删记录 + brew uninstall。
		if brew.Installed {
			p := m.brewUninstallPlan(ctx, app, brew)
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
		// 记录 managed=false，但**目录说得出它是什么**（有 brew formula 且真的装着）
		// → 给真正的卸载。2026-09-21 用户明确要求：目录里的应用主动作只有一个
		// 「🗑 卸载」并真的卸载；不要再并排摆一颗"只删记录"（原话："移除却不卸载
		// 是什么意思 …… 让用户看不到却持续运行"）。
		//
		// 这条覆盖 mysql84 这类"用户早年自己 brew 装、后来在面板里登记过"的条目：
		// 面板知道怎么卸载它（brew uninstall），就必须给得出这条路，
		// 而不是把软件藏起来假装卸载了。
		if app.PanelInstaller == "" && brew.Installed &&
			app.Kind != KindCompose && app.Kind != KindDocker {
			p := m.brewUninstallPlan(ctx, app, brew)
			p.Service = rec.Name
			p.Steps = append([]string{
				"停止并移除「" + rec.DisplayName + "」的服务定义",
				"从「服务管理」中删除这条记录",
			}, p.Steps...)
			return p
		}
		// 面板**不认识**这条服务（没有目录条目、也不知道怎么卸载它）：
		// 唯一诚实的收尾动作是"停止它 + 从面板记录里移除"，并逐字说明
		// 软件仍在磁盘上、需要用户自己卸载 —— 绝不留下一个还在运行、
		// 面板里却看不到的服务（那正是用户最厌恶的"隐身运行"）。
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
	// 残留的**原生 release 安装**：应用已经不在注册表里（例如 Lucky 改成 Docker 版之后
	// 从 releaseBinaryApps 移除），但旧的原生目录/plist 还在磁盘上。
	//
	// 2026-09-16 用户实测到的严重问题：这种情况下 PlanUninstall 返回 "none"，
	// 市场既卸不掉它、也不给任何入口 —— 用户原话"lucky 根本没被卸载掉"。
	// 卸载计划必须**以磁盘状态为准**，不能只看代码注册表。
	// 判据顺序：先把**磁盘上真实存在的东西**都收进来，再决定有没有可清理的对象。
	// 只按其中一种形态判断会出现"卡片说未安装、但东西还在"的漏洞
	// （Lucky 同时存在过 ~/lucky 与 compose 项目目录两种情况）。
	paths := []string{}
	if d := filepath.Join(m.composeDir(), app.ID); dirExists(d) {
		paths = append(paths, d)
	}
	if d := m.legacyNativeDir(app); d != "" {
		paths = append(paths, d)
	}
	// **brew 装了、但面板没有任何记录**（用户自己 brew install 的、或换机后记录丢了）。
	//
	// 2026-09-21 用户现场：nginx 就是这一态 —— 市场卡片诚实地显示"已安装"
	// （brew formula 在、launchd 也在），而卸载计划是 kind:"none"，
	// 于是「⚙️ 管理」面板里**一颗收尾按钮都没有**（用户原话"甚至没有卸载按钮"）。
	// installed=true 却给不出任何可卸载对象，本身就是自相矛盾。
	// 现在给真实的 brew 卸载路径（先确认、走任务中心、失败可见）。
	if brew.Installed {
		p := m.brewUninstallPlan(ctx, app, brew)
		// 残留的 launchd 服务要先停：只删 brew 包不摘服务的话，KeepAlive 会
		// 一直尝试拉起一个已经不存在的二进制。
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
		// 下架条目（n8n）的镜像名也在这里如实写出来 —— 否则用户删完目录，
		// 镜像还静静占着几百 MB，而他完全不知道。
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
	// 只剩 launchd 里的服务定义（brew 包已经不在了、也没有残留目录）：
	// 这类"僵尸服务"必须能停掉并摘掉，否则就会出现"installed=true（plist 在）
	// 却 kind=none（没有任何可卸载对象）"的自相矛盾 —— 界面上一颗收尾按钮都没有。
	// Colima 的僵尸 plist（坑 161）就是这一类。
	if app.BrewFormula != "" {
		if label := m.brewLabelFor(app.BrewFormula); label != "" {
			return UninstallPlan{
				Kind:     "installer",
				Steps:    []string{"停止并删除残留的 launchd 服务 " + label},
				KeepNote: "Homebrew 里已经没有这个包了；这一步只摘掉它残留的服务定义",
			}
		}
	}
	// 残留态（**没有服务记录**，但磁盘上还有这个应用的 compose 目录）：
	// 卸载（保留数据）或手工删了容器之后就会落到这里。不给计划的话，
	// 卡片会显示成"未安装"却**没有任何清理入口**，那份数据永远清不掉 ——
	// 与 2026-09-16 用户反馈的"卸载后连安装入口都没有"是同一类问题。
	return UninstallPlan{Kind: "none", Blocked: "没有找到可卸载的对象（Homebrew 里没有这个包，面板里也没有记录与残留）"}
}

// brewUninstallPlan 给出"按 Homebrew 包卸载"的计划。
//
// steps 第一条固定是 `brew uninstall <formula>`（界面逐条展示的就是真实计划），
// 并在用户按确认之前如实交代依赖关系：
//   - 查到了别的已装包在用它 → 点名（brew uninstall 不会连带删依赖，那些包会缺依赖）；
//   - 查了、没有 → 明说"已检查"；
//   - 没查成 → 明说"**未检查**"，绝不假装没有依赖（铁律 11）。
func (m *Manager) brewUninstallPlan(ctx context.Context, app App, brew BrewState) UninstallPlan {
	formula := brew.Formula
	if formula == "" {
		formula = app.BrewFormula
	}
	p := UninstallPlan{Kind: "brew", Formula: formula}
	p.Steps = append(p.Steps, "brew uninstall "+formula)
	// PHP：brew 从不删除 etc/php/<版本> 下的配置（php.ini / conf.d / php-fpm.d），
	// 卸载后会留一份"上一个版本"的配置。计划里要把**这一个版本自己的**配置目录
	// 列成可选清理项，并在确认框里说清那件事（见 phpConfigPaths 的注释）。
	phpVersion := m.phpVersionFor(formula, brew.Version)
	p.PhpVersion = phpVersion
	phpConfig := m.phpConfigPathsFor(phpVersion)
	if len(phpConfig) > 0 {
		p.DataPaths = append(p.DataPaths, phpConfig...)
		p.Steps = append(p.Steps, "brew uninstall 完成后，把 PHP "+
			phpVersion+" **自己**的配置目录清理掉（勾选「同时删除配置」才会执行）："+
			strings.Join(phpConfig, "、"))
	}
	deps, checked := m.brewUsesInstalled(ctx, formula)
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
		// brew uninstall 自己也会因为依赖而拒绝；这里在**计划阶段**就把依赖方逐条列出来，
		// 让用户在按确认之前就看清"谁依赖它、有什么选择"，而不是等 brew 甩一段英文原文。
		// 强制卸载那一条必须**逐字写出命令**（用户要求：按钮/正文里写清
		// `brew uninstall --ignore-dependencies <formula>`），否则用户不知道那是什么。
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
	// 这两句都是**用户最关心的两件事**，必须出现在确认框里：
	//   · 不会自动删除别的包（HOMEBREW_NO_AUTOREMOVE=1，2026-09-21 用户真机看到
	//     "Autoremoving 2 unneeded formulae: net-snmp rtmpdump"）;
	//   · 不会删除配置目录（brew 从不删 etc/ 下的配置，但它会打印一段吓人的 warning）。
	p.KeepNote = "brew uninstall 只删除 " + formula + " 本身：**不会**删除它依赖的包，" +
		"也**不会**自动删除其它「已不再被需要」的包（面板给卸载命令带上 HOMEBREW_NO_AUTOREMOVE=1），" +
		"更不会删除用户数据与配置目录"
	if len(phpConfig) > 0 {
		// PHP 特殊：brew **从不删除** etc/php 下的配置，而它的 warning 会把整个
		// /opt/homebrew/etc/php 目录（含其它版本的 8.2 目录、phpmyadmin 的配置）
		// 一起列出来 —— 那段提示很容易被读成"面板要删这些"。必须翻译清楚。
		p.KeepNote = "brew uninstall 只删除 " + formula + " 本身：**不会**删除它依赖的包，" +
			"也**不会**自动删除其它「已不再被需要」的包（面板给卸载命令带上 HOMEBREW_NO_AUTOREMOVE=1）。" +
			"Homebrew 从不删除 " + m.phpEtcDir() + " 下的配置：它卸载后打印的那段 " +
			"「configuration files have not been removed」只是笼统地列出**整个目录**，" +
			"其中其它 PHP 版本（例如 8.2）的目录与 phpmyadmin 的配置**不属于本次卸载、" +
			"面板也不会碰**；本计划只涉及 " + strings.Join(phpConfig, "、") + "。" +
			"不勾选「同时删除该版本的配置」时这些也原样保留"
	}
	return p
}

// brewUsesInstalled 查 `brew uses --installed <formula>`：还有哪些**已安装**的包在用它。
//
// 为什么带缓存：市场列表里每个 brew 原生条目都会调一次，brew 启动本身就要 0.4 秒；
// 而依赖关系几乎不变。返回的 bool 表示**这次查询真的成功了**（false = 未检查，
// 界面与计划必须如实这么说，不能把"没查成"显示成"没有依赖"）。
//
// ⚠️ 超时上限 45 秒（2026-09-21 实测踩到）：brew 会去更新/加载 tap，在测试机
// 或网络不好的机器上**能挂几分钟**；而这条查询在**市场列表**的渲染路径上，
// 挂住就等于整个「应用」页白屏（单测里表现为 TestMarketZombieColimaPlistNotInstalled
// 卡到 15 分钟超时）。拿不到结论时**如实报"未检查"**，绝不假装"没有依赖"，
// 也绝不拖着整页一起等 —— 这正是铁律 11 的口径。
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
	// 只缓存**成功**结果：超时/失败不能缓 5 分钟（那会让"未检查"粘住，
	// 用户修好 brew 之后刷新也还是"未检查"）。
	if ok {
		brewUsesMu.Lock()
		brewUsesCache[formula] = brewUsesEntry{deps: deps, ok: ok, at: time.Now()}
		brewUsesMu.Unlock()
	}
	return deps, ok
}

// UninstallApp 执行卸载。Kind=service/forget 由 web 层走各自的既有接口，
// 这里只处理"面板安装器"这一类。
//
// force=true 只由 web 层在"用户在确认框里明确选了强制卸载"时传入；它只会被
// 转交给那些真的跑 `brew uninstall` 的安装器（python / phpmyadmin），其余安装器
// 忽略它（它们没有 brew 依赖这回事）。
func (m *Manager) UninstallApp(ctx context.Context, appID string, removeData, force bool, result *InstallResult) error {
	app, ok := FindApp(appID)
	if !ok {
		// 条目已经从应用市场移除（2026-09-16 移除了 Lucky / frps / Orbien 服务端），
		// 但用户机器上可能**已经装了** —— 那台机器必须仍然删得掉。
		// 否则就是最坏状态：界面上没了、磁盘上还在跑，而用户无处可点。
		//
		// 刻意**不填** PanelInstaller：那会让流程走进"用安装器正规卸载"的分支，
		// 而那个分支要读服务记录、要动 launchd，对一个已经不在目录里的条目并不合适。
		// PanelInstaller 留空 → 落入下面的"残留清理"分支，按 ID 删掉磁盘上真实存在的目录，
		// 并摘掉按命名约定推出的 launchd 服务。
		app = App{ID: appID, Name: appID}
	}
	// 没有 PanelInstaller 的应用（compose / docker 类，或"旧版原生安装的残留"）：
	// 清理动作就是删掉磁盘上真实存在的那些目录。
	//
	// 必须在这里处理，否则市场里「删除残留数据」会报"没有对应的卸载实现" ——
	// 而 Lucky 从原生改成 Docker 版那次事故正是这样：旧的原生目录与新公式
	// 两边都对不上，用户点哪都没有反应（2026-09-16 用户原话"lucky 根本没被卸载掉"）。
	if app.PanelInstaller == "" {
		targets := []string{}
		// 条目还在目录里时按它声明的 Kind 判断；条目**已从目录移除**时（!ok）
		// 我们不知道它当初是 compose 还是原生，就把两种位置都查一遍 ——
		// 这正是用户最需要的那种清理，漏查一种就等于"点了删不掉"。
		if !ok || app.Kind == KindCompose || app.Kind == KindDocker {
			targets = append(targets, filepath.Join(m.composeDir(), app.ID))
		}
		if d := m.legacyNativeDir(app); d != "" {
			targets = append(targets, d)
		}
		// 残留的 launchd 服务也要停掉：只删目录不摘服务的话，
		// launchd 会一直尝试重启一个已经不存在的二进制（KeepAlive）。
		//
		// 标签候选要**多试几个**：目录里写死的 homebrew.mxcl.<f> 与磁盘上真实的
		// cn.zizdog.<f> / sh.brew.<f> 常常不一致（坑："PHP 8.3 点纳管报找不到
		// plist"）。只试一个的话，僵尸 plist 会被当成"没有可清理的残留"，
		// 于是 installed=true（plist 在）却一颗收尾按钮都不给。
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
		// 目录删掉了，但镜像还在。**不自动删镜像**（怕误删被别的项目共用的层），
		// 只把名字如实告诉用户 —— 下架条目（n8n）靠 legacyComposeImages 提供。
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
	// "官方 release 原生二进制"类应用（Lucky / Orbien）：同一套安装器，
	// 用注册表查而不是在这里再抄一遍 switch，免得加了新应用忘记补卸载。
	if _, ok := releaseBinaryApps[app.PanelInstaller]; ok {
		return m.UninstallReleaseBinary(ctx, app.PanelInstaller, removeData, result)
	}
	return fmt.Errorf("「%s」没有对应的卸载实现（PanelInstaller=%q）", app.Name, app.PanelInstaller)
}

// installerUninstalls 是"面板自研安装器 → 卸载实现"的**唯一注册表**。
//
// 为什么用注册表而不是 switch（2026-09-21，用户第三次报"装上了卸不掉"）：
// 安装器清单与卸载实现分散在两处（installerPlan 的 switch、UninstallApp 的
// switch），加一个新应用时漏补任何一处，用户看到的就是"有安装入口、
// 卸载按钮报没有实现"或者"计划里没有卸载实现"。现在 UninstallApp 只查这张表，
// 全目录门禁测试（TestCatalogUninstallActionMatrix）也查它 ——
// 有安装器却没有卸载实现，**测试阶段就会失败**，不会等到用户点下去。
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
	"phpmyadmin": func(m *Manager, ctx context.Context, _ App, removeData, force bool, r *InstallResult) error {
		return m.UninstallPhpMyAdmin(ctx, removeData, force, r)
	},
	// 基础依赖也允许单独卸载，但由 UninstallBaseDependency 把后果写清楚
	// （用户要求"明确提示即可，不要禁止"）。它不走 brew uninstall（保留包），
	// 所以 force 对它没有意义。
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
	// Python 解释器（应用市场里三个版本共用这一个安装器，见 python_runtime.go）。
	// 卸载必须如实点名"谁还在用它"（面板自研服务的 venv 只是**指向**它，
	// 卸掉解释器不会报依赖缺失，而是让那些服务直接起不来）；force 交给
	// UninstallPythonRuntime 去追加 --ignore-dependencies（只由用户显式选择）。
	"python": func(m *Manager, ctx context.Context, app App, _, force bool, r *InstallResult) error {
		return m.UninstallPythonRuntime(ctx, app, force, r)
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

// StopAndForget 停掉一个服务并删除它的面板记录，返回"是否真的停过它"。
//
// 为什么"只删记录"必须先停服务（2026-09-21 用户明确要求）：只删记录会让一个
// 仍在运行的服务彻底消失在面板里 —— 用户看不到它，它却继续占着端口跑业务，
// 这正是用户最厌恶的"移除却不卸载"。所以：
//   - 停成功               → 删记录；
//   - 停失败但**确实不在跑** → 也删记录（运行时已经被删掉的卡死场景，
//     没有"隐身运行"的风险，这条通路本来就是为它准备的）；
//   - 停失败且仍在跑        → **拒绝删记录**并如实报错，让用户先停掉。
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
			// 连驱动都构造不出来（运行时被删掉、plist/compose 文件都不在了）：
			// 这个服务**不可能在运行** —— 没有可运行的东西。这正是 2026-09-17
			// 那种"记录永远删不掉"的卡死场景，允许只删记录。
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
//
// 这是"brew 装了但没有面板记录"/"结论是 brew 原生托管服务"两条矩阵行共用的执行体。
// formula 只认计划里那个（PlanUninstallForBrew 判定采用的真实写法），
// 绝不在这里拿目录里的 BrewFormula 再猜一次 —— 猜错就是删错包。
//
// force=true 表示**用户在确认框里明确选了「强制卸载」**（对应 brew 的
// `--ignore-dependencies`）。这条路径只能由用户显式选择触发：
//   - 计划阶段没查出依赖（ForceAllowed=false）时，web 层会拒绝 force 请求；
//   - 这里再挡一道：计划不允许强制时，force 请求直接报错，绝不悄悄放行。
func (m *Manager) UninstallBrewApp(ctx context.Context, app App, plan UninstallPlan, removeData, force bool, result *InstallResult) error {
	formula := plan.Formula
	if formula == "" {
		return fmt.Errorf("「%s」的卸载计划里没有 Homebrew formula，无法卸载", app.Name)
	}
	if force && !plan.ForceAllowed {
		return fmt.Errorf("拒绝强制卸载 %s：当前计划没有查出依赖，不需要（也不允许）强制卸载", formula)
	}
	// ① 面板记录：停服务 + 删记录（managed=false 的登记记录也走这条 ——
	// 它同样是"面板里的这条记录"，而 Manager.Uninstall 只收 managed 记录）。
	if plan.Service != "" {
		if result != nil {
			result.step(ctx, "停止「"+plan.Service+"」并删除这条面板记录")
		}
		if _, err := m.StopAndForget(ctx, plan.Service); err != nil && !errors.Is(err, ErrServiceNotFound) {
			return err
		}
	}
	// ② 没有记录、但 launchd 里还有这个应用的服务：先摘掉。
	// 只删 brew 包不摘服务的话，KeepAlive 会一直尝试拉起一个不存在的二进制。
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
	// PHP：卸载后清掉**这一个版本自己的**配置目录（只列了它的那种）。
	// brew 自己从不删配置，而它的 warning 会把整个 etc/php 目录列出来 ——
	// 这里按计划（只含本版本）删，父目录只在空了的情况下才删。
	if removeData {
		if err := m.cleanupPHPConfigDirs(ctx, plan.PhpVersion, result); err != nil {
			return err
		}
	}
	return nil
}

// brewUninstall 跑一条 `brew uninstall`（force 时追加 --ignore-dependencies）。
//
// 命令形状：
//
//	brew uninstall <formula>                                  ← 默认，绝不加 force 开关
//	brew uninstall --ignore-dependencies <formula>            ← 只有用户明确选了强制
//
// 环境变量由 brewCommand/brewEnv 统一带 HOMEBREW_NO_AUTOREMOVE=1：
//
//	2026-09-21 用户真机：`brew uninstall php` 结束时顺手
//	`==> Autoremoving 2 unneeded formulae: net-snmp rtmpdump` —— 用户只点了一个卸载，
//	面板却删了两个跟他这次操作无关的包。带上它就绝不会发生。
//
// 标签从真实参数派生（不手写），与实际执行的命令一致。
//
// 走 brewInstallRun 而不是裸 brewRunSource：生产路径完全一样（没有注入时它
// 直接调 brewRunSource），但测试能注入假执行器**锁住命令形状**
// （没有 force 时必须不含 --ignore-dependencies；force 时才追加它），
// 而不必真的跑一次 brew uninstall。
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

// brewUninstallErrText 把 brew uninstall 的失败**翻译**成用户能据以行动的说明。
//
// 为什么必须有（2026-09-21 用户真机，卸载 python@3.13）：
//
//	$ brew uninstall python@3.13
//	Error: Refusing to uninstall /opt/homebrew/Cellar/python@3.13/3.13.3_1
//	because it is required by llvm and rust, which are currently installed.
//	You can override this and force removal with:
//	  brew uninstall --ignore-dependencies python@3.13
//
// 以前面板把这一整段英文原文再贴一遍当错误文案：用户既不知道"谁依赖它"，
// 也不知道还能怎么办（那段 override 提示被淹在文案里）。现在改成一句人话 +
// 两个明确动作；**原始输出不丢**，跟在后面供排查。
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

// phpVersionOfFormula 从 formula 取 PHP 的版本号（"php@8.4" → "8.4"；"php" → ""）。
//
// 为什么允许空：这台机器上的 `php` 就是"当年 brew install php 时装的那个版本"
// （真机是 8.4.7，见 ResolveBrewFormula 的注释）。卸载 `php` 时配置在
// etc/php/8.4，从 formula 推不出来 —— 这时宁可不列出任何配置目录，
// 也**绝不**把别的版本（8.2）写进要删的名单（用户 2026-09-21 的报障）。
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

// phpVersionFor 决定"这个 PHP formula 在 etc/php 下的版本目录名"。
//
// 两条来源，先 exact 再回落：
//  1. formula 自己带版本（`php@8.4` → "8.4"）；
//  2. 无版本别名 `php`（这台机器上 brew list 就是 `php 8.4.7`）→ 用 **brew 报出的
//     真实版本**取主次版本号（"8.4.7" → "8.4"）。
//
// 两条都不成立时返回空 —— 这时宁可不列任何配置目录，也**绝不**去猜一个版本号：
// 猜错就是删别的版本的配置（用户 2026-09-21 报障：卸载 8.4 时看到一堆 8.2 的路径）。
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
//
// 用户 2026-09-21 真机（卸载 PHP 8.4）看到的 brew warning：
//
//	Warning: The following php configuration files have not been removed!
//	  /opt/homebrew/etc/php
//	  /opt/homebrew/etc/php/8.2
//	  /opt/homebrew/etc/php/8.2/conf.d/ext-opcache.ini
//	  …
//	  /opt/homebrew/etc/phpmyadmin.config.inc.php
//
// 那是 brew **笼统地列出整个 /opt/homebrew/etc/php 目录**（它从不删配置），
// 里面 8.2 的路径属于**另一个版本**、phpmyadmin 的配置属于另一个应用。
// 本函数只认这一个版本的子目录：
//
//	<prefix>/etc/php/<版本>        （目录，含 php.ini / php-fpm.conf /
//	                                 conf.d / php-fpm.d —— 全都在它下面）
//
// 只有真的存在于磁盘上才列出来（确认框里写不存在的路径等于骗人）。
// 父目录 etc/php **不在这里**：它只有在空掉时才由 cleanupPHPConfigDirs 删。
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

// cleanupPHPConfigDirs 删除"这个 PHP 版本自己的"配置目录，并**只在父目录空了**时删父目录。
//
// 两条铁律：
//   - 绝不碰其它版本（etc/php/8.2）与 phpmyadmin 的配置 —— 那些路径根本不在计划里；
//   - 父目录 etc/php 里有任何别的东西就保留（删它会带走别的版本与应用配置）。
//
// version 与计划里算 DataPaths 时用的是同一个（phpVersionFor），所以"确认框里
// 写了什么"与"真的删什么"逐字一致。
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

// parseBrewRequiredBy 从 brew 的拒绝文案里取出依赖方的包名。
//
//	"Error: Refusing to uninstall /opt/homebrew/Cellar/python@3.13/3.13.3_1
//	 because it is required by llvm and rust, which are currently installed."
//
// → ["llvm", "rust"]。解析不出来就返回空（调用方退回通用文案 + 原始输出，
// 绝不编造包名）。
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
	// 英文列表连接词：`A and B` / `A, B and C`（逗号已在上面截掉，这里处理 and）。
	// 用 FieldsFunc 按逗号/空白切，再丢掉纯连接词，避免手写逗号拆分漏掉
	// `llvm and rust` 这种只有 and 的形态（拆错就会把整句当包名）。
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

// isBrewRefusingUninstall 识别 brew 的"因依赖而拒绝卸载"这一类输出
// （不同 brew 版本的措辞不完全一样）。
func isBrewRefusingUninstall(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "refusing to uninstall") ||
		strings.Contains(low, "is required by") ||
		strings.Contains(low, "ignore-dependencies")
}

// installerPlan 按安装器给出卸载计划。
func (m *Manager) installerPlan(ctx context.Context, app App) UninstallPlan {
	// "官方 release 原生二进制"类应用（Lucky / Orbien 服务端与客户端 / frps / frpc）：
	// 一次注册表判断覆盖全部 —— 加了新条目不用回来补 case（漏补的后果是
	// 市场里的卸载按钮报"没有卸载实现"，而东西确实是面板装的）。
	if plan, ok := m.releaseBinaryPlan(app.PanelInstaller); ok {
		return plan
	}
	p := UninstallPlan{Kind: "installer"}
	switch app.PanelInstaller {
	case "qwen3tts":
		p.Steps = []string{"停止并删除 launchd 服务 " + qwenLabel, "从「服务管理」移除记录"}
		// 只列**我们清单里的模型目录**，不要写整个 HF hub ——
		// 那个目录里还有别的项目的模型，确认框上写它会让人以为要全删。
		// 实现（uninstallQwen）也只删这几个目录。
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
		p.Steps = []string{
			"移除 nginx 默认站点里的 phpMyAdmin 入口并重载",
			"brew uninstall phpmyadmin",
		}
		p.DataPaths = []string{filepath.Join(m.brewPrefix(), "etc", "phpmyadmin.config.inc.php")}
		p.KeepNote = "面板自研的库表管理功能不受影响"
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
		// Python 解释器：卸载计划必须把"谁会受影响"写在用户按下按钮**之前**。
		// 依赖判断基于真实磁盘状态（各服务的 venv 里的 pyvenv.cfg），不是猜的。
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
		// 基础依赖也能单独卸载（用户要求"明确提示即可，不要禁止"）。
		// 卸载计划必须把后果写在这里 —— 确认框会逐条展示，用户按下去之前就看得见。
		p.Steps = []string{
			"⚠️ FFmpeg 是面板的基础依赖，卸载后 TTS 编码 mp3 会返回 HTTP 200 + 0 字节 body",
			"音色接收端的样本校验/转码、以及后续的音视频功能也会失效",
			"brew uninstall ffmpeg",
		}
		p.KeepNote = "需要时可随时从应用市场重新安装；面板安装脚本与 LNMP / Qwen TTS 部署也会自动补装"
	case "miniflux":
		// 铁律：卸载应用**不删数据**。Miniflux 库里是订阅源与已读状态，
		// 删掉不可恢复，所以 PostgreSQL 与库一律保留，并在确认框里写清楚。
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
		// 同步目录在用户指定的位置（面板不知道），这里只谈"身份与索引"。
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
//
// 顺序：先 unload/删 plist，再删记录 —— 反了的话进程还在跑、面板却已经没有它了。
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
	// 先走正常停止（服务可能正在跑）；失败也继续 —— 目标状态是"没有它"，
	// 后面删 plist 才是决定性的那一步。
	//
	// 用 priv.LaunchUnload 而不是裸 `bootout system/<label>`：作业可能在
	// user/<uid> 或 gui/<uid> 域里（系统化迁移前后都可能），只打 system 域
	// 会留下停不掉的孤儿进程（卸载后它还占着端口）。
	if label != "" {
		_ = priv.LaunchUnload(label)
	}
	// 两个位置都要清：系统化之后 plist 在 /Library/LaunchDaemons，
	// 而迁移前/历史上装的用户级 agent 在 ~/Library/LaunchAgents —— 只删一个，
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

// ComposeArtifactExists 报告某个 compose 应用的**项目目录**是否还在。
//
// 为什么单独一个导出函数：市场列表（web 层）要据此把"没装但有残留"
// 如实报出来，而它不该知道 compose 目录怎么拼（那是 services 的内部约定）。
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

// residualServicePlists 找出这个应用**磁盘上真实存在**的 launchd 服务定义。
//
// 为什么要多候选标签（而不是用目录里写死的那一个）：磁盘上的真实标签常常是
// cn.zizdog.<f> / sh.brew.<f>，而目录里写的是 homebrew.mxcl.<f>（坑："PHP 8.3
// 点纳管报找不到 plist"）。只试一个的话，僵尸 plist 会被当成"没有可清理的残留"，
// 于是 installed=true（plist 在）却没有任何收尾动作。
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

// legacyNativeDir 返回"这个应用在用户家目录下**残留的**原生安装目录"。
//
// 为什么需要它（真机事故，2026-09-16）：Lucky 从"原生 release 二进制"改成
// Docker 版之后，注册表里不再有它，而 `installerPlan` 只按注册表判断 ——
// 于是已经装在 ~/lucky 的那份**既没有卸载入口、也删不掉**，
// 市场卡片还显示"已安装"（launchd plist 在）。用户的原话是"lucky 根本没被卸载掉"。
//
// 判据刻意保守：
//   - 只认 `~/<appID>` 这一层（与原生安装器的 RootDir 约定一致，不递归扫）；
//   - 目录里必须有**可执行文件**才算残留（避免把同名数据目录当安装）；
//   - compose 应用不参与（它们的产物是 WorkDir/compose/<id>）。
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

// LegacyNativeArtifactExists 报告"这个应用在用户家目录下是否还有残留的原生安装目录"。
//
// 导出给 web 层用（市场列表要据此把"没装但有残留"如实报出来），
// 内部复用同一套判据，避免两处判断漂移。
func LegacyNativeArtifactExists(userHome, appID, kind string) bool {
	if userHome == "" || appID == "" {
		return false
	}
	m := &Manager{opt: Options{UserHome: userHome}}
	return m.legacyNativeDir(App{ID: appID}) != ""
}

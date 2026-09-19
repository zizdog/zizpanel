package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ============================================================================
//  卸载依赖引擎（面板级）
//
//  用户原话：
//    · "有没有做卸载依赖检测？如果我要卸载 ffmpeg，tts 需要用它，就要提示
//      必须先卸载 tts。卸载 php 也一样，有没有网站正在用它，有就提示让网站
//      切换其它版本，然后才能卸载。要全局考虑！"
//    · "只要有运行中的容器，就要提醒用户有 xx 在运行，删除 colima 会怎样！！"
//
//  设计原则（与全项目一致）：
//    1. 每条判据都必须基于**真实证据**（面板记录 / 站点记录 / `docker ps` 输出 /
//       venv 里的 pyvenv.cfg），不许猜；
//    2. 拿不到证据就如实写"未检查"，绝不假装"没有依赖"（铁律 11）；
//    3. 同一个问题只在**一个地方**回答：这里是唯一的依赖判定实现，
//       PlanUninstall 的所有分支都调它。
// ============================================================================

// Dependent 是一个"会受这次卸载影响"的对象。
type Dependent struct {
	// Kind: app（面板里的应用）/ site（站点）/ container（运行中的容器）
	// / panel（面板自身）/ service（面板服务记录）
	Kind string `json:"kind"`
	// Name 是人读名（用户看得懂，不要内部 ID）。
	Name string `json:"name"`
	// Detail 是证据（哪条记录 / 哪个站点 / 哪个容器）。
	Detail string `json:"detail,omitempty"`
	// Action 是用户该怎么做（"先在网站管理里把它切到其它 PHP 版本"）。
	Action string `json:"action,omitempty"`
}

// SiteRef 是依赖检测需要的站点摘要（services 包不直接读 sites 库，
// 由 web 层注入 —— 两个 store 各有一份来源，不互相猜）。
type SiteRef struct {
	Domain     string
	PHPVersion string
	Enabled    bool
}

// UninstallDependents 找出"卸载这个应用会影响谁"，每条都给证据与建议动作。
func (m *Manager) UninstallDependents(ctx context.Context, app App, rec *Service) []Dependent {
	formula := app.BrewFormula
	if formula == "" {
		formula = app.ID
	}
	out := []Dependent{}
	switch {
	case formula == "ffmpeg" || app.ID == "ffmpeg" || app.PanelInstaller == "ffmpeg":
		out = append(out, m.ffmpegDependents(ctx)...)
	case strings.HasPrefix(formula, "php@") || app.ID == "php82" || app.ID == "php84" || app.ID == "php83":
		out = append(out, m.phpDependents(app, formula)...)
	case strings.HasPrefix(formula, "mysql"):
		out = append(out, m.siteDependents("站点会连不上数据库",
			"先在「数据库」里确认这些站点用的账号，或先把站点停用/迁移后再卸载 MySQL")...)
	case formula == "nginx" || app.ID == "nginx":
		out = append(out, m.nginxDependents()...)
	case strings.HasPrefix(formula, "postgresql"):
		out = append(out, m.postgresDependents(ctx)...)
	case strings.HasPrefix(formula, "python@") || app.PanelInstaller == "python":
		out = append(out, m.pythonDependents(formula)...)
	case app.PanelInstaller == "docker-runtime" || app.Kind == KindColima || formula == "colima":
		out = append(out, m.dockerRuntimeDependents(ctx)...)
	}
	// 通用兜底（`brew uses --installed`）在**brew 卸载计划**里做（brewUninstallPlan）：
	// 只有真的走 brew 卸载时才查，避免市场列表对每个条目白跑一次 brew。
	return dedupeDependents(out)
}

// ApplyDependents 把依赖结论并进卸载计划：结构化列表 + 一条给用户看的汇总。
//
// 只有"真的会被影响"的对象才 block；`brew uses` 那种"依赖但 brew 自己会拦住"
// 的提示不 block（用户可以在确认框里看到，brew 自己也有保护）。
func (m *Manager) ApplyDependents(ctx context.Context, app App, rec *Service, plan *UninstallPlan) {
	if plan == nil {
		return
	}
	deps := m.UninstallDependents(ctx, app, rec)
	if len(deps) == 0 {
		return
	}
	plan.Dependents = dedupeDependents(append(plan.Dependents, deps...))
	hard := []Dependent{}
	for _, d := range plan.Dependents {
		if d.Kind != "brew" {
			hard = append(hard, d)
		}
	}
	if len(hard) == 0 {
		return
	}
	names := []string{}
	for _, d := range hard {
		names = append(names, d.Name)
	}
	msg := "还有 " + fmt.Sprint(len(hard)) + " 个对象在用它（" +
		strings.Join(names, "、") + "）。请先按提示处理它们，再卸载「" + app.Name + "」。"
	if plan.Blocked == "" {
		plan.Blocked = msg
	} else {
		plan.Blocked = plan.Blocked + "；" + msg
	}
}

// ---------- 各条规则 ----------

// ffmpegDependents ffmpeg 是很多能力的运行依赖：缺了它不会报错，而是静默产出坏结果
// （TTS 返回 HTTP 200 + 0 字节 body，2026-09-16 真机事故）。
func (m *Manager) ffmpegDependents(ctx context.Context) []Dependent {
	out := []Dependent{}
	add := func(id, name, why string) {
		if m.serviceInstalledish(ctx, id) {
			out = append(out, Dependent{
				Kind: "app", Name: name, Detail: why,
				Action: "先在「应用」里卸载 " + name + "（不需要它就保留 ffmpeg）",
			})
		}
	}
	add("qwen3tts", "Qwen3 TTS（语音合成）", "合成 mp3 必须用 ffmpeg 编码")
	add("voicereceiver", "TtsVoice 音色接收端", "样本校验/转码用 ffmpeg/ffprobe")
	add("iopaint", "IOPaint", "视频处理入口用 ffmpeg")
	// 面板自身的 TTS 能力也依赖它（属于 panel 这一 kind）。
	if len(out) > 0 {
		out = append(out, Dependent{
			Kind: "panel", Name: "面板的语音能力", Detail: "上面这些面板功能共用同一个 ffmpeg",
			Action: "要么保留 ffmpeg，要么先卸载上面对应的应用",
		})
	}
	return out
}

// phpDependents 站点正在用这个 PHP 版本 → 必须先切版本。
func (m *Manager) phpDependents(app App, formula string) []Dependent {
	want := ""
	if i := strings.Index(formula, "@"); i >= 0 {
		want = formula[i+1:]
	}
	if want == "" {
		return nil
	}
	out := []Dependent{}
	for _, s := range m.siteRefs() {
		if s.PHPVersion != want {
			continue
		}
		out = append(out, Dependent{
			Kind: "site", Name: s.Domain, Detail: "站点 " + s.Domain + " 正在用 PHP " + want,
			Action: "先在「网站管理」把 " + s.Domain + " 切到其它 PHP 版本，再卸载 PHP " + want,
		})
	}
	return out
}

// siteDependents 是"任意站点存在就受影响"的通用规则（MySQL / nginx）。
func (m *Manager) siteDependents(why, action string) []Dependent {
	out := []Dependent{}
	for _, s := range m.siteRefs() {
		out = append(out, Dependent{
			Kind: "site", Name: s.Domain, Detail: "站点 " + s.Domain + "：" + why,
			Action: action,
		})
	}
	return out
}

// nginxDependents nginx 挂着所有站点，也是面板入口的反代起点。
func (m *Manager) nginxDependents() []Dependent {
	out := m.siteDependents("nginx 提供它的 vhost 与端口转发", "先把站点停用/迁走，或保留 nginx")
	out = append(out, Dependent{
		Kind: "panel", Name: "面板入口", Detail: "面板的站点管理与反向代理依赖 nginx",
		Action: "面板入口依赖 nginx：卸载后面板的部分功能会失效，请确认这确实是你要的",
	})
	return out
}

// postgresDependents PostgreSQL 主要被 Miniflux 用。
func (m *Manager) postgresDependents(ctx context.Context) []Dependent {
	if !m.serviceInstalledish(ctx, "miniflux") {
		return nil
	}
	return []Dependent{{
		Kind: "app", Name: "Miniflux", Detail: "Miniflux 的订阅与已读状态都在 PostgreSQL 里",
		Action: "先在「应用」里卸载 Miniflux（它的数据在库里，删库不可恢复）",
	}}
}

// pythonDependents 复用既有的 venv 依赖探测（谁在用它）。
func (m *Manager) pythonDependents(formula string) []Dependent {
	if formula == "" {
		return nil
	}
	out := []Dependent{}
	for _, name := range m.pythonRuntimeDependents(formula) {
		out = append(out, Dependent{
			Kind: "app", Name: name, Detail: "它的虚拟环境指向 " + formula,
			Action: "先在「应用」里卸载或重装 " + name + "（换用其它 Python 版本），再卸载 " + formula,
		})
	}
	return out
}

// dockerRuntimeDependents 删 Colima 会带走所有容器、镜像、卷 —— 必须点名。
func (m *Manager) dockerRuntimeDependents(ctx context.Context) []Dependent {
	out := []Dependent{}
	for _, name := range m.composeUsers(ctx) {
		out = append(out, Dependent{
			Kind: "app", Name: name, Detail: "面板里的 Docker 项目",
			Action: "先在「应用」里卸载 " + name + "（它的容器会随运行时一起消失）",
		})
	}
	names, checked := m.runningContainerNames(ctx)
	if !checked {
		out = append(out, Dependent{
			Kind: "container", Name: "未能列出容器",
			Detail: "连不上 Docker 引擎，无法确认还有哪些容器在跑",
			Action: "请先确认没有容器在运行；能连上引擎后这里会逐条列出容器名",
		})
		return out
	}
	for _, n := range names {
		out = append(out, Dependent{
			Kind: "container", Name: n, Detail: "容器正在运行",
			Action: "先停止这个容器（或先在「容器」页停掉全部），再删除运行时",
		})
	}
	return out
}

// brewDependents 通用兜底：`brew uses --installed`。
func (m *Manager) brewDependents(ctx context.Context, formula string) []Dependent {
	if formula == "" {
		return nil
	}
	// 与 brewUninstallPlan 同一条判据：只读 brew。以前这里有个
	// `m.opt.BrewBin == ""` 的短路，而**卸载计划走的路径从来不看它**
	// （brewUninstallPlan 直接调用 brewUsesInstalled）—— 两条路的判据必须一致，
	// 否则"面板安装器"那条路（Python 解释器就是）永远查不到 llvm/rust 依赖，
	// 用户点下去只会吃到一整段 brew 英文报错（真机）。
	deps, checked := m.brewUsesInstalled(ctx, formula)
	if !checked {
		return nil
	}
	out := []Dependent{}
	for _, d := range deps {
		out = append(out, Dependent{
			Kind: "brew", Name: d, Detail: "Homebrew 包 " + d + " 依赖 " + formula,
			Action: "先卸载它（如果它已经不需要了），或选择强制卸载 " + formula +
				"（会破坏 " + d + "：它会缺依赖、可能无法运行）",
		})
	}
	return out
}

// BrewDependencyBlock 查一次"还有哪些**已安装**的 Homebrew 包依赖 formula"，
// 把结果并进卸载计划（结构化 Dependents + 面向用户的 Blocked 文案 + ForceAllowed）。
//
// 为什么需要它（用户真机卸载 python@3.13）：
//
//	Error: Refusing to uninstall ... because it is required by llvm and rust …
//	You can override this and force removal with:
//	  brew uninstall --ignore-dependencies python@3.13
//
// 面板路径：市场条目 python313 → PanelInstaller=python → 卸载计划由 installerPlan
// 生成（格式为 `brew uninstall python@3.13`），**从来没查过 brew uses**，
// 于是用户按了确认，只拿到 brew 的英文原文，既看不懂也不知道还有"强制卸载"这条路。
//
// 现在：计划阶段就把依赖方逐条列出并 Blocked；ForceAllowed 只在"确实被依赖拦下"
// 时为 true，执行端只有收到显式 force=true 才会追加 --ignore-dependencies。
//
// 返回是否真的命中了 brew 依赖（false = 查了没有 / 没查成）。
func (m *Manager) BrewDependencyBlock(ctx context.Context, formula string, plan *UninstallPlan) bool {
	if plan == nil || formula == "" || plan.ForceAllowed {
		return false
	}
	deps := m.brewDependents(ctx, formula)
	if len(deps) == 0 {
		return false
	}
	plan.DependentsChecked = true
	plan.Dependents = dedupeDependents(append(plan.Dependents, deps...))
	names := []string{}
	for _, d := range deps {
		names = append(names, d.Name)
	}
	joined := strings.Join(names, "、")
	brewMsg := "brew 拒绝卸载 " + formula + "：" + joined + " 依赖它。" +
		"可以先卸载它们，或选择强制卸载（brew uninstall --ignore-dependencies " + formula +
		"，会破坏 " + joined + "：它们会缺依赖、可能无法运行）"
	if plan.Blocked == "" {
		plan.Blocked = brewMsg
	} else {
		plan.Blocked = plan.Blocked + "；" + brewMsg
	}
	plan.ForceAllowed = true
	plan.ForceNote = "强制卸载会破坏这些包：" + joined +
		"（它们会缺依赖、可能无法运行）。命令：brew uninstall --ignore-dependencies " + formula
	plan.Steps = append(plan.Steps,
		"⚠️ 下列已安装的软件依赖 "+formula+"，brew 会拒绝卸载："+joined,
		"要保留它们就先卸载它们或保留 "+formula+"；只有选择「强制卸载」才会执行 "+
			"brew uninstall --ignore-dependencies "+formula)
	return true
}

// ---------- 证据查询 ----------

// serviceInstalledish 报告"面板里这个应用算装着"：有服务记录，或有安装产物。
func (m *Manager) serviceInstalledish(ctx context.Context, appID string) bool {
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if s.Name == appID || strings.Contains(s.LaunchLabel, appID) {
					return true
				}
			}
		}
	}
	if a, ok := FindApp(appID); ok && a.PanelInstaller != "" && m.opt.UserHome != "" {
		return InstallerArtifactExists(m.opt.UserHome, a.PanelInstaller)
	}
	return false
}

// siteRefs 读一次站点列表并在本次 Manager 生命周期内缓存
// （市场列表会对十几个条目各算一次计划，不缓存就是十几次库查询）。
func (m *Manager) siteRefs() []SiteRef {
	if m.siteRefsLoaded {
		return m.siteRefsCache
	}
	m.siteRefsLoaded = true
	if m.opt.SiteDependents != nil {
		m.siteRefsCache = m.opt.SiteDependents()
	}
	return m.siteRefsCache
}

// runningContainerNames 返回正在运行的容器名；bool = 这次查询是否成功。
func (m *Manager) runningContainerNames(ctx context.Context) ([]string, bool) {
	if m.dockerPSProbe != nil {
		return m.dockerPSProbe(ctx)
	}
	if m.opt.DockerSocket == "" {
		return nil, false
	}
	list, err := m.DockerContainers(ctx, false)
	if err != nil {
		return nil, false
	}
	names := []string{}
	for _, c := range list {
		if !strings.EqualFold(c.State, "running") {
			continue
		}
		names = append(names, c.PrimaryName())
	}
	sort.Strings(names)
	return names, true
}

func dedupeDependents(in []Dependent) []Dependent {
	out := make([]Dependent, 0, len(in))
	seen := map[string]bool{}
	for _, d := range in {
		k := d.Kind + "|" + d.Name
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

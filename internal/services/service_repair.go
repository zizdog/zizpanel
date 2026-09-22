package services

// ============================================================================
//  "安装体在、守护服务没注册"时用户唯一能修的那一步（坑 231）
//
//  用户报障（2026-09-22，whisper.cpp）：卡片显示「已安装·服务未注册」，提示却写
//  "到「已安装」Tab 点一次启动即可自动注册（面板会用 brew services start 补上）"。
//  两处都是假话：那台机器上**没有**这条服务记录（「已安装」Tab 里根本没有卡），
//  而它的服务标签 com.zizdog.stt 是**面板安装器**写的 —— brew 补不出来。
//
//  所以判据只能在后端算一次（前端不再自己猜 brew）：
//    · 有记录 + 标签能由 brew 重建（homebrew.mxcl.* / sh.brew.*）→ 「已安装」Tab 启动；
//    · 有记录 + 基础环境（nginx 的 cn.zizdog.nginx 由 LNMP 安装器写）→ 重跑基础环境安装；
//    · 其余（面板安装器托管、或压根没记录）                  → 重跑安装（安装器幂等）。
// ============================================================================

// ServiceRepair.Action 的三个取值。
const (
	// ServiceRepairInstalledTab：「已安装」Tab 有这条记录，点一次「启动」就能重建 plist。
	ServiceRepairInstalledTab = "installed_tab"
	// ServiceRepairReinstall：重跑这个应用的安装流程（市场卡片上的「重新部署」）。
	ServiceRepairReinstall = "reinstall"
	// ServiceRepairEnvInstall：基础环境（nginx）的服务定义由网站页的环境安装器写。
	ServiceRepairEnvInstall = "env_install"
)

// ServiceRepair 是市场条目带给界面的"怎么修服务"结论。
// Needed=false 时其余字段为空 —— 界面据此决定要不要出提示与「重新部署」按钮。
type ServiceRepair struct {
	Needed bool   `json:"needed"`
	Action string `json:"action,omitempty"`
	// Hint 是**后端写好的**一句人话（谁该修、点哪里）；前端只渲染，不再自己拼。
	Hint string `json:"hint,omitempty"`
}

// ServiceRepairFor 判定"这个应用此刻的服务缺失该怎么修"。
//
// 纯函数（不读磁盘、不看进程）：四条状态组合各有唯一结论，门禁才能把它锁死。
// installed / adopted / inLaunchd 由市场列表的**同一份证据**传入（见 handleMarketList）。
func ServiceRepairFor(app App, installed, adopted, inLaunchd bool) ServiceRepair {
	// 没有守护进程的应用（phpMyAdmin / ffmpeg / python）本来就不该有 launchd 服务；
	// compose / docker 的"服务"是容器，service_in_launchd 永远为假 —— 都不是缺陷。
	// 没装、或服务明明在 launchd 里 → 无需修。
	if !installed || inLaunchd || app.NoDaemon ||
		app.Kind == KindCompose || app.Kind == KindDocker {
		return ServiceRepair{}
	}
	if adopted && brewCanRebuildLabel(app) {
		return ServiceRepair{Needed: true, Action: ServiceRepairInstalledTab,
			Hint: "服务记录还在，但 launchd 里没有它 —— 到「已安装」Tab 点一次「启动」即可重建" +
				"（面板会执行 brew services start " + app.BrewFormula + "）。"}
	}
	if adopted && IsEnvComponent(app) {
		return ServiceRepair{Needed: true, Action: ServiceRepairEnvInstall,
			Hint: "服务定义（plist）不存在，而这个标签由面板的基础环境安装器写 —— " +
				"到「网站」页重新安装一次基础环境即可重建（brew 补不上这个标签）。"}
	}
	return ServiceRepair{Needed: true, Action: ServiceRepairReinstall,
		Hint: "安装产物在，但服务没有注册（launchd 里没有它）。点「重新部署」重跑一遍安装：" +
			"会重建服务定义并启动，已下载的产物会复用。"}
}

// brewCanRebuildLabel 判断 `brew services start <formula>` 重建出来的标签，
// 是不是这个应用声明/记录的那个：
//   - 声明了 brew 标签（homebrew.mxcl.* / sh.brew.*）→ 是；
//   - 没声明标签但有 formula（miniflux / syncthing）→ 是（brew 按 formula 建）；
//   - 声明的是面板自己的标签（com.zizdog.stt / cn.zizdog.nginx）→ 不是。
func brewCanRebuildLabel(app App) bool {
	if app.BrewFormula == "" {
		return false
	}
	if app.ServiceLabel == "" {
		return true
	}
	f, ok := brewFormulaFromLabel(app.ServiceLabel)
	return ok && f == app.BrewFormula
}

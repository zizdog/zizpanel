package services

import (
	"context"
	"fmt"
	"path/filepath"
)

// ============================================================================
//  重复安装的幂等判定：已经装过的应用再点一次「安装」不该报 failed
//
//  真机证据（Mac mini，2026-09-16，27 个应用逐个安装取数）——同一个缺陷的 8 种表现：
//
//	nginx        安装前检查未通过：端口 80 已被面板管理的服务占用：homebrew-mxcl-nginx
//	mysql84      端口 3306 已被 sh-brew-mysql8-4 占用
//	ollama       端口 11434 已被 ollama 占用
//	uptime-kuma  端口 3001 已被 uptime-kuma 占用
//	php81/82/83/84  服务已安装但写入注册表失败: 服务名 php83 已存在
//
//  这一批全是"机器本来就是好的，用户又点了一次安装"。任务却以 failed 结束，
//  用户会以为把机器弄坏了。
//
//  判定只用**真实的注册证据**（面板服务记录 / launchd 里真实存在的 plist），
//  绝不用"磁盘上有产物"——那正是"卸载（保留数据）后卡片卡在已安装、
//  连安装入口都没有"那个坑的根因（见 api_market_test.go 的警告）。
//  也因此，面板卸载会把记录与 plist 一起清掉，重装不会被误判成"已安装"而跳过。
// ============================================================================

// launchDaemonsDirs 是"系统级 launchd 服务定义"的查找目录。
//
// 抽成变量是为了**测试可沙箱化**（与 web 层 api_services.go 的 launchDaemonsDir
// 同一个理由）：单测要断言"没有记录时应走正常安装"，如果去 stat 真实的
// /Library/LaunchDaemons，开发机/用户机上恰好装了 nginx 就会让结论随机器飘。
var launchDaemonsDirs = []string{"/Library/LaunchDaemons"}

// appServiceLabels 返回这个目录条目在 launchd 里可能出现的标签。
//
// 真机同一台机器上会混用多套前缀（`homebrew.mxcl.nginx`、`sh.brew.mysql@8.4`、
// 以及系统级改造后的 `cn.zizdog.nginx`），所以：
//   - 目录里写死的那两个标签；
//   - 两套 Homebrew 标准前缀 + formula 的组合（`sh.brew.*` 是这台机器的实测前缀）。
//
// 刻意不在这里读磁盘（不用 BrewLabelFor）：判定必须是纯字符串，才能被单测锁死。
// 任意前缀的那一类由 recordMatchesApp 里的 catalogEntryForLabel 按 formula 反查兜住。
func appServiceLabels(app App) []string {
	var out []string
	if app.ServiceLabel != "" {
		out = append(out, app.ServiceLabel)
	}
	if app.AdoptLabel != "" {
		out = append(out, app.AdoptLabel)
	}
	if app.BrewFormula != "" {
		out = append(out, "homebrew.mxcl."+app.BrewFormula, "sh.brew."+app.BrewFormula)
	}
	return out
}

// recordMatchesApp 判断一条服务记录是不是"这个目录条目对应的服务"。
//
// 三种写法都认，因为真机上同一个应用在注册表里就是这么混着的：
//   - 目录 ID / 展示名（`php83`、`uptime-kuma` —— 面板安装器与 compose 应用按它们登记）；
//   - 目录写死的 launchd 标签（`homebrew.mxcl.nginx` / `sh.brew.mysql@8.4`）；
//   - 标签的归一化名字（`sh-brew-mysql8-4` —— AdoptCandidate 登记时就是这么起名的）；
//   - 任意前缀但 formula 对得上的标签（走 catalogEntryForLabel 的后缀反查）。
//
// 只认一种写法会漏判，漏判的后果就是重复安装报错 —— 这正是那 8 个失败场景。
func recordMatchesApp(s *Service, app App) bool {
	if s == nil {
		return false
	}
	if app.ID != "" && s.Name == app.ID {
		return true
	}
	if app.Name != "" && s.Name == app.Name {
		return true
	}
	for _, l := range appServiceLabels(app) {
		if s.LaunchLabel == l || s.Name == NormalizeName(l) {
			return true
		}
	}
	if s.LaunchLabel != "" {
		if a, ok := catalogEntryForLabel(s.LaunchLabel); ok && a.ID == app.ID {
			return true
		}
	}
	return false
}

// installedRecordFor 找出"这个目录条目在面板注册表里的那条记录"（没有则返回 nil）。
//
// 只看注册表里**真实存在的记录**：磁盘产物不算（见文件头）。
func (m *Manager) installedRecordFor(ctx context.Context, app App) *Service {
	if m == nil || m.repo == nil {
		return nil
	}
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil
	}
	for _, s := range list {
		if recordMatchesApp(s, app) {
			return s
		}
	}
	return nil
}

// presentServiceLabel 返回"这个应用此刻真的有 launchd 服务定义"的那个标签。
//
// 用途：面板注册表里没有记录、但服务确实已经装在系统里（brew services 起过、
// 或用户自己注册过）时，安装任务同样不该报 failed —— 卡片按 plist/brew 已经
// 显示"已安装"了，任务再报失败就是界面与事实互相矛盾。
//
// 面板卸载会连 plist 一起删，所以"卸载（保留数据）后重装"不会落进这里。
func (m *Manager) presentServiceLabel(app App) string {
	for _, l := range appServiceLabels(app) {
		if l == "" {
			continue
		}
		for _, dir := range launchDaemonsDirs {
			if fileExists(filepath.Join(dir, l+".plist")) {
				return l
			}
		}
		if m.opt.UserHome != "" &&
			fileExists(filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", l+".plist")) {
			return l
		}
	}
	return ""
}

// installedSkipResult 是"重复安装"的统一出口：
//
//	· 注册表里有记录 → 幂等成功（并补齐缺失字段）；
//	· 注册表里没有、但 launchd 里真的有它 → 也判"已安装"，并提示去「纳管」。
//
// 返回 done=false 表示确实没装过，调用方继续走正常安装流程。
func (m *Manager) installedSkipResult(ctx context.Context, app App) (*InstallResult, bool) {
	if rec := m.installedRecordFor(ctx, app); rec != nil {
		return m.alreadyInstalledResult(ctx, app, rec), true
	}
	if label := m.presentServiceLabel(app); label != "" {
		res := &InstallResult{App: app.ID, Name: app.Name}
		res.Steps = append(res.Steps,
			fmt.Sprintf("检测到「%s」的服务已经在 launchd 里（%s）", app.Name, label),
			"没有重复执行安装命令（brew / docker 都没有重跑）",
			"如需重启或改配置，请到「应用 → 已安装」操作；如需在列表里看到它，请点「+ 注册服务」",
		)
		res.Message = fmt.Sprintf("「%s」已经装过了，本次跳过，没有重复安装", app.Name)
		return res, true
	}
	return nil, false
}

// alreadyInstalledResult 构造"已经装过了，本次跳过"的成功结果。
//
// 关键点（真机反馈里用户明确要的）：
//   - 终态是 succeeded，不是 failed —— 机器本来就是好的；
//   - 步骤里把"没有重复安装"写清楚，用户才知道刚才那一下干了什么。
func (m *Manager) alreadyInstalledResult(ctx context.Context, app App, rec *Service) *InstallResult {
	res := &InstallResult{App: app.ID, Name: app.Name, Service: rec}
	res.Steps = append(res.Steps, fmt.Sprintf("检测到「%s」已经安装：服务记录 %s", app.Name, rec.Name))
	if rec.Port > 0 {
		res.Steps = append(res.Steps,
			fmt.Sprintf("端口 %d 由它自己占用（正常状态，不是端口冲突）", rec.Port))
	}
	if m.reconcileInstalledRecord(ctx, app, rec) {
		res.Steps = append(res.Steps, "已顺带补齐服务记录里缺失的字段（标签/端口/健康检查地址）")
	}
	res.Steps = append(res.Steps,
		"没有重复执行安装命令（brew / docker 都没有重跑）",
		"如需重启服务或改配置，请到「服务管理」操作（本次没有重启正在运行的服务）",
	)
	res.Message = fmt.Sprintf("「%s」已经装过了，本次跳过，没有重复安装", app.Name)
	return res
}

// reconcileInstalledRecord 只补**缺失**的字段，不覆盖已有信息。
//
// 为什么只补不覆盖：记录可能来自用户自己纳管的服务，覆盖标签/端口等于把面板的
// 猜想写进真实记录（例如把标签改成目录里写死、但磁盘上不存在的那个，状态查询
// 就会永远失败）。目录改了要"双向对齐"的场景由 ReconcileHealthURLs 负责，
// 这里只处理"当年装的时候没写全"。
func (m *Manager) reconcileInstalledRecord(ctx context.Context, app App, rec *Service) bool {
	if rec == nil || m.repo == nil {
		return false
	}
	changed := false
	if rec.LaunchLabel == "" {
		if l := firstServiceLabel(app); l != "" {
			rec.LaunchLabel = l
			changed = true
		}
	}
	if rec.PlistPath == "" && rec.LaunchLabel != "" {
		if p := plistPathForLabel(m.opt.UserHome, rec.LaunchLabel); p != "" {
			rec.PlistPath = p
			changed = true
		}
	}
	if rec.Port == 0 && app.Port > 0 {
		rec.Port = app.Port
		changed = true
	}
	if rec.HealthURL == "" {
		if hu := healthURLFor(app); hu != "" {
			rec.HealthURL = hu
			changed = true
		}
	}
	if !changed {
		return false
	}
	return m.repo.Update(ctx, rec) == nil
}

// firstServiceLabel 取目录条目声明的首选 launchd 标签。
func firstServiceLabel(app App) string {
	if app.ServiceLabel != "" {
		return app.ServiceLabel
	}
	if app.AdoptLabel != "" {
		return app.AdoptLabel
	}
	if app.BrewFormula != "" {
		return "homebrew.mxcl." + app.BrewFormula
	}
	return ""
}

// plistPathForLabel 按 label 在系统域 / 用户域找真实存在的 plist（找不到返回空）。
func plistPathForLabel(userHome, label string) string {
	if label == "" {
		return ""
	}
	for _, dir := range launchDaemonsDirs {
		if p := filepath.Join(dir, label+".plist"); fileExists(p) {
			return p
		}
	}
	if userHome != "" {
		if p := filepath.Join(userHome, "Library", "LaunchAgents", label+".plist"); fileExists(p) {
			return p
		}
	}
	return ""
}

// registerAppService 把这次安装写进服务注册表。
//
// 同名记录已存在时**不再直接报"服务名已存在"**：先看它是不是就是这个应用
// （recordMatchesApp）。是 → 幂等成功；不是 → 如实报冲突并说清是谁占了这个名字。
//
// 真机上 php81/82/83/84 就是死在这里：
//
//	服务已安装但写入注册表失败: 服务名 php83 已存在
//
// 前面 installViaBrew 已经把包与服务都弄好了，只差这一行记录，却让整个任务变红。
func (m *Manager) registerAppService(ctx context.Context, app App, svc *Service) (*Service, error) {
	exists, err := m.repo.Exists(ctx, svc.Name)
	if err == nil && exists {
		rec, gerr := m.repo.Get(ctx, svc.Name)
		if gerr == nil && recordMatchesApp(rec, app) {
			m.reconcileInstalledRecord(ctx, app, rec)
			return rec, nil
		}
		label := ""
		if rec != nil {
			label = rec.LaunchLabel
		}
		who := label
		if who == "" {
			who = "记录没有 launchd 标签"
		}
		return nil, fmt.Errorf("服务名 %s 已存在，且与「%s」不是同一条记录（已有记录：%s）；"+
			"请先在「服务管理」里处理它", svc.Name, app.Name, who)
	}
	if err := m.repo.Create(ctx, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

// selfPortOccupiers 返回"占着这个端口、而且就是这个应用自己的服务记录名"。
//
// 自己占自己的端口不是冲突：应用本来就在跑（nginx 听 80、MySQL 听 3306、
// Ollama 听 11434、Uptime Kuma 听 3001）。旧代码一律判成冲突，
// 于是这 4 个应用"再点一次安装"必然 failed。
//
// 判据刻意**只看面板自己的注册记录**，不看进程名：进程名匹配会把
// "另一个恰好同名的进程"吞成"已安装"，而真冲突必须失败并点名占用者（不许吞）。
func (m *Manager) selfPortOccupiers(ctx context.Context, app App) []string {
	if app.Port <= 0 || m.repo == nil {
		return nil
	}
	names, err := m.repo.CountByPort(ctx, app.Port, "")
	if err != nil || len(names) == 0 {
		return nil
	}
	occupying := map[string]bool{}
	for _, n := range names {
		occupying[n] = true
	}
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range list {
		if occupying[s.Name] && recordMatchesApp(s, app) {
			out = append(out, s.Name)
		}
	}
	return out
}

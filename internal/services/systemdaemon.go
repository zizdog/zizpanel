package services

// systemdaemon.go —— 把"必须常驻"的服务从**用户级 LaunchAgent** 改造成
// **系统级 LaunchDaemon**（以真实用户身份运行）。
//
// ============================================================================
//  为什么必须这么做（坑 130，2026-09-17 mini 真机 5 次重启验证）
//
//  无头 macOS（服务器模式：`who` 为空、没人进图形会话）开机**不会**加载
//  ~/Library/LaunchAgents。于是 `brew services start` 装出来的用户级服务重启后
//  一个都不会自己回来 —— mini 实测：nginx(8889/443)、mysqld(3306)、php@8.2
//  起来了（本来就是系统级），而 postgresql@17、miniflux、syncthing **全停着**
//  （`launchctl print gui/501` 报 125、`brew services list` 全是 none，
//   plist 文件却好端端躺在 LaunchAgents 里）。
//
//  修法：把这类服务装成 /Library/LaunchDaemons/<同 label>.plist，注入
//  UserName=<真实用户> + RunAtLoad。launchd 在开机时（不需要任何人登录）以该
//  用户身份拉起它，数据与配置的属主不变。这与 qwen3tts / iopaint /
//  release 二进制 / colima / 面板计划任务的做法一致 —— 它们早就是系统级，
//  只有 brew services 这条路漏了。
// ============================================================================
//  哪些应用需要（判断标准：**面板对用户的承诺依赖它开机就在**）
//
//    ✅ 数据库 postgresql17、Web 服务 miniflux、同步守护 syncthing、
//       推理后端 ollama、站点赖以运行的 PHP-FPM php@8.x
//    ❌ ffmpeg / phpMyAdmin：没有常驻进程（NoDaemon）
//    ❌ typecho / wordpress：产物是文件+数据库，不是服务
//    ❌ Docker 类：常驻的是 Colima（早已是系统级），容器靠 compose 的
//       `restart: unless-stopped` 自己回来
//    ❌ nginx / mysql@8.4：brew 已经以 root 装成系统级 LaunchDaemon；
//       改成"以用户身份运行"会砸掉它 root 所有的数据目录
//  目录里用 App.SystemDaemon 显式声明，逐条可查、可测。
// ============================================================================
//  怎么改造：**复用** tools/system-services.sh（内嵌在二进制里）
//
//  那把脚本是唯一在真机上验证过的实现（LNMP 的 nginx/php@8.2/mysql 就是它
//  系统化的），它做了：读 brew 的 plist → PlistBuddy 注入 UserName/RunAtLoad
//  → chown root:wheel 0644 → 清同名用户级 agent → 等旧实例真的消失
//  → bootstrap system（重试 + kickstart 兜底）→ 按端口/socket 验证。
//  重新写一份 Go 版只会多出一份"只在真机上才会暴露差异"的实现，所以这里
//  只补脚本没覆盖的那一段：**裸奔在 user/<uid> 域里的旧作业**。
//  脚本用 `launchctl bootout gui/<uid>/<label>` 清用户级 agent，而无头机器上
//  gui 域根本不存在（坑 125），作业其实挂在 user/<uid> 里 —— 不清掉就会出现
//  "同一服务两份实例抢同一个端口"。所以改造前先走一次 priv.LaunchUnload
//  （它按域探测，user/gui 都覆盖）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

// SystemLaunchDaemonsDir 是系统级 LaunchDaemon 的目录。
//
// 做成变量是为了单测能指向 t.TempDir()：测试绝不允许碰真实的
// /Library/LaunchDaemons（AGENTS.md 第三节）。
var SystemLaunchDaemonsDir = "/Library/LaunchDaemons"

// SystemDaemonPlistPath 返回某个 label 的系统级 plist 路径。
func SystemDaemonPlistPath(label string) string {
	return filepath.Join(SystemLaunchDaemonsDir, label+".plist")
}

// systemDaemonEnsureFn 是"执行系统化"的注入点。
//
// 单测里面板不是以 root 跑的（真实面板是 root LaunchDaemon），也没法真的写
// /Library/LaunchDaemons，所以测试需要模拟这一段的执行结果：既能验证
// "顺利路径不产生告警"，也能显式验证"失败必须如实降级、不许谎报成功"。
var systemDaemonEnsureFn = func(m *Manager, ctx context.Context, app App, res *InstallResult) (string, string, error) {
	return m.ensureSystemDaemon(ctx, app, res)
}

// systemDaemonNeeded 报告这个应用是否必须装成系统级 LaunchDaemon。
func systemDaemonNeeded(app App) bool {
	return app.SystemDaemon && !app.NoDaemon && strings.TrimSpace(app.BrewFormula) != ""
}

// appBrewLabel 解析应用对应的 launchd label。
//
// 顺序：brew 的权威答复（`brew services info --json`）→ 磁盘上已有 plist 的
// 前缀约定（homebrew.mxcl.* 与 sh.brew.* 并存过，写死会漏掉一半）→ 兜底约定。
func (m *Manager) appBrewLabel(ctx context.Context, formula string) string {
	if label, _, _ := m.brewServiceInfo(ctx, formula); strings.TrimSpace(label) != "" {
		return label
	}
	if label := BrewLabelFor(m.opt.UserHome, formula); label != "" {
		return label
	}
	return "sh.brew." + formula
}

// systemDaemonUserPlist 返回某个 label 在真实用户家目录下的用户级 plist 路径。
func (m *Manager) systemDaemonUserPlist(label string) string {
	home := m.opt.UserHome
	if home == "" && m.opt.UserName != "" {
		home = "/Users/" + m.opt.UserName
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

// systemDaemonLabels 返回某个 formula 可能用到的**全部** launchd label。
//
// 为什么不能只看一个：Homebrew 有两套命名（`homebrew.mxcl.<formula>` 与
// `sh.brew.<formula>`），而且**可能同时存在**。真机（本机 2026-09-17 php@8.3）：
// 系统域里跑的是 `homebrew.mxcl.php@8.3`（脚本按 `opt/<formula>/*.plist` 里的
// Label 写出来的），而 `~/Library/LaunchAgents` 里还躺着一份 `sh.brew.php@8.3`
// 的旧 agent（`brew services info` 报的就是这个名字）。只看一个 label 的后果：
// 要么"搬完了却复核不到"（把成功报成失败），要么把另一份用户级 agent 留在机器上
// —— 重启时 launchd 会同时拉起用户级与系统级**两份实例**，抢同一个端口/socket。
func (m *Manager) systemDaemonLabels(ctx context.Context, formula string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(l string) {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			return
		}
		seen[l] = true
		out = append(out, l)
	}
	add(m.appBrewLabel(ctx, formula))
	add("homebrew.mxcl." + formula)
	add("sh.brew." + formula)
	return out
}

// systemDaemonExistingLabel 找出**已经在系统域**的那个 label（没有则返回空）。
//
// 以磁盘为准而不是以 brew 的答复为准：脚本写下去的 label 来自
// `opt/<formula>/*.plist` 里的 Label，可能与 `brew services info` 报的不一样；
// 最后再用一次通配兜底，跨过历史遗留的其它前缀。
func (m *Manager) systemDaemonExistingLabel(ctx context.Context, formula string) string {
	for _, l := range m.systemDaemonLabels(ctx, formula) {
		if fileExists(SystemDaemonPlistPath(l)) {
			return l
		}
	}
	if g, err := filepath.Glob(filepath.Join(SystemLaunchDaemonsDir, "*."+formula+".plist")); err == nil && len(g) > 0 {
		return strings.TrimSuffix(filepath.Base(g[0]), ".plist")
	}
	return ""
}

// hasUserAgent 报告某个 formula 是否有用户级 plist（迁移只关心这种）。
func (m *Manager) hasUserAgent(ctx context.Context, formula string) bool {
	for _, l := range m.systemDaemonLabels(ctx, formula) {
		if p := m.systemDaemonUserPlist(l); p != "" && fileExists(p) {
			return true
		}
	}
	return false
}

// cleanupUserAgents 删掉某个 formula 在真实用户家目录下的用户级 plist（两套命名都清）。
//
// 系统化之后必须清：留着它，开机时 launchd 会同时加载系统级与用户级两份，
// 起两个进程抢同一个端口。删不掉要**如实报错**（面板是 root，删不掉通常意味着
// 权限或路径异常，而后果正是"重启后两份实例打架"）。
func (m *Manager) cleanupUserAgents(ctx context.Context, formula string) error {
	for _, l := range m.systemDaemonLabels(ctx, formula) {
		p := m.systemDaemonUserPlist(l)
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除用户级 plist %s 失败: %w", p, err)
		}
	}
	return nil
}

// ensureSystemDaemon 把应用的服务改造成系统级 LaunchDaemon（幂等）。
//
// 返回真实的 label 与系统级 plist 路径，调用方据此登记服务记录。
// 失败一律返回 error —— 这个功能的意义就是"重启后它必须自己起来"，
// 做不到就不能让安装流程显示成功（AGENTS.md 铁律 10）。
func (m *Manager) ensureSystemDaemon(ctx context.Context, app App, res *InstallResult) (string, string, error) {
	if !systemDaemonNeeded(app) {
		return "", "", fmt.Errorf("「%s」不在必须常驻清单里，不做系统级改造", app.Name)
	}
	formula := app.BrewFormula

	// ① 已经在系统域：以磁盘上真实的 label 为准（可能与 brew 报的不一致），
	//    顺手清掉可能残留的用户级 agent（否则重启后两份实例抢端口）。
	if label := m.systemDaemonExistingLabel(ctx, formula); label != "" {
		plist := SystemDaemonPlistPath(label)
		if err := m.cleanupUserAgents(ctx, formula); err != nil {
			return label, plist, err
		}
		if st, err := priv.LaunchStatus(label); err != nil {
			return label, plist, fmt.Errorf("复核系统级服务状态失败: %w", err)
		} else if !st.Loaded {
			if err := priv.LaunchLoad(label); err != nil {
				return label, plist, fmt.Errorf("系统级服务没能加载: %w", err)
			}
		}
		if app.Port > 0 && !waitPort(ctx, app.Port, 45*time.Second) {
			return label, plist, fmt.Errorf("系统级服务已装载，但 %d 秒内端口 %d 没有监听；"+
				"看日志 %s，或在「服务管理」里点「⟳ 重启」", 45, app.Port, m.serviceLogHint(app, label))
		}
		m.syncServiceRecords(ctx, formula, label, plist)
		return label, plist, nil
	}

	// ② 还没系统化：非 root 写不了 /Library/LaunchDaemons，如实失败。
	if os.Geteuid() != 0 {
		return "", "", fmt.Errorf("面板不是以 root 身份运行，写不了 %s（系统级服务必须由 root 安装）",
			filepath.Join(SystemLaunchDaemonsDir, "<label>.plist"))
	}
	res.step(ctx, "改造为系统级服务（开机自启、以 "+m.opt.UserName+" 身份运行）："+formula)

	// ③ 先清扫旧实例：脚本只 bootout `gui/<uid>`，而无头机器上作业在
	//    `user/<uid>`（gui 域根本不存在）。不清掉就会出现两份实例抢端口。
	//    两套命名都要卸 —— 它们可能同时挂着。
	for _, l := range m.systemDaemonLabels(ctx, formula) {
		if err := priv.LaunchUnload(l); err != nil {
			return "", "", fmt.Errorf("卸载旧的用户级服务 %s 失败（不清掉会出现两份实例）: %w", l, err)
		}
	}
	if err := m.cleanupUserAgents(ctx, formula); err != nil {
		return "", "", err
	}

	// ④ 交给那把已经在真机上验证过的脚本（注入 UserName/RunAtLoad、
	//    chown root:wheel、bootstrap system、按端口/socket 验证）。
	if err := m.installSystemDaemonsFor(ctx, res, []string{formula}, 6*time.Minute); err != nil {
		return "", "", err
	}

	// ⑤ 复核**磁盘上真实生成的那份**：脚本只对 nginx/php/mysql 做验证，
	//    其余 formula 的"成功"必须我们自己验，否则就是谎报成功。
	label := m.systemDaemonExistingLabel(ctx, formula)
	if label == "" {
		return "", "", fmt.Errorf("脚本执行完毕，但 %s 下没有生成 *.%s.plist —— 不能当作系统化成功",
			SystemLaunchDaemonsDir, formula)
	}
	plist := SystemDaemonPlistPath(label)
	if st, err := priv.LaunchStatus(label); err != nil {
		return label, plist, fmt.Errorf("复核系统级服务状态失败: %w", err)
	} else if !st.Loaded {
		if err := priv.LaunchLoad(label); err != nil {
			return label, plist, fmt.Errorf("系统级服务没能加载: %w", err)
		}
	}
	if app.Port > 0 && !waitPort(ctx, app.Port, 45*time.Second) {
		return label, plist, fmt.Errorf("系统级服务已装载，但 %d 秒内端口 %d 没有监听；"+
			"看日志 %s，或在「服务管理」里点「⟳ 重启」", 45, app.Port, m.serviceLogHint(app, label))
	}
	res.step(ctx, "已装成系统级服务（重启后不需要任何人登录就会自动起来）")
	m.syncServiceRecords(ctx, formula, label, plist)
	return label, plist, nil
}

// serviceLogHint 给出排障用的日志路径提示（拿不到就返回通用说明）。
func (m *Manager) serviceLogHint(app App, label string) string {
	if p := strings.TrimSpace(app.LogPath); p != "" {
		return p
	}
	if _, _, logPath := m.brewServiceInfo(context.Background(), app.BrewFormula); logPath != "" {
		return logPath
	}
	if home := m.opt.UserHome; home != "" {
		return filepath.Join(home, "Library", "Logs", label+".log")
	}
	return "/opt/homebrew/var/log/"
}

// syncServiceRecords 把面板里指向这个 formula 的记录对齐到**真实的**
// label 与系统级 plist 路径。
//
// 为什么必须做（真机 2026-09-17，坑 133）：面板记录里存的 label 可能来自旧安装
// （`sh.brew.php@8.1`），而系统化脚本按 `opt/<formula>/*.plist` 里的 Label 写下去
// 的是 `homebrew.mxcl.php@8.1`。记录与 launchd 对不上时，服务**明明在跑**，
// 界面上却是"找不到 plist 文件（服务可能已被移除）"—— 假失败，正是本项目最忌讳的
// 那一类。这里以磁盘为准回写记录（只改 label 与 plist 路径，不动别的字段）。
func (m *Manager) syncServiceRecords(ctx context.Context, formula, label, plist string) int {
	if m.repo == nil || label == "" {
		return 0
	}
	aliases := map[string]bool{}
	for _, l := range m.systemDaemonLabels(ctx, formula) {
		aliases[l] = true
	}
	list, err := m.repo.List(ctx)
	if err != nil {
		return 0
	}
	fixed := 0
	for _, rec := range list {
		if rec == nil {
			continue
		}
		// 只碰"同一个服务"的记录：label 属于这个 formula 的候选名字，
		// 或者它的 plist 路径正好是我们要接管的那两份之一。
		if !aliases[rec.LaunchLabel] && rec.PlistPath != plist {
			continue
		}
		if rec.LaunchLabel == label && rec.PlistPath == plist {
			continue
		}
		rec.LaunchLabel = label
		rec.PlistPath = plist
		if err := m.repo.Update(ctx, rec); err == nil {
			fixed++
		}
	}
	return fixed
}

// StartBrewService 启动 brew 应用对应的服务，**优先走 launchctl**。
//
// 为什么不一律 `brew services start`：系统化之后 brew 已经不管这个服务了
// （它只认 ~/Library/LaunchAgents），`brew services start` 会写出**第二份**
// 用户级 plist 并把服务拉成两份。所以：系统级 plist 在 → launchctl；
// 不在 → 老路（brew services），没系统化的应用与老机器行为不变。
func (m *Manager) StartBrewService(ctx context.Context, formula string) error {
	label := m.appBrewLabel(ctx, formula)
	if fileExists(SystemDaemonPlistPath(label)) {
		return priv.LaunchLoad(label)
	}
	_, err := m.brewRun(ctx, 3*time.Minute, "services", "start", formula)
	return err
}

// StopBrewService 停止服务，同样优先走 launchctl（见 StartBrewService）。
func (m *Manager) StopBrewService(ctx context.Context, formula string) error {
	label := m.appBrewLabel(ctx, formula)
	if fileExists(SystemDaemonPlistPath(label)) {
		return priv.LaunchUnload(label)
	}
	_, err := m.brewRun(ctx, 3*time.Minute, "services", "stop", formula)
	return err
}

// RestartBrewService 重启服务（改完配置要它重新读）。
//
// 已系统化：kickstart -k（先杀后拉）；kickstart 失败说明作业没加载，
// 退回 LaunchLoad（它会 bootstrap）再报错。未系统化：brew services restart。
func (m *Manager) RestartBrewService(ctx context.Context, formula string) error {
	label := m.appBrewLabel(ctx, formula)
	if fileExists(SystemDaemonPlistPath(label)) {
		if err := priv.LaunchKickstart(label); err == nil {
			return nil
		}
		return priv.LaunchLoad(label)
	}
	_, err := m.brewRun(ctx, 3*time.Minute, "services", "restart", formula)
	return err
}

// MigrateSystemDaemons 把**已经装过**的用户级服务搬迁到系统域。
//
// 为什么需要启动迁移：修好安装路径只对"以后装的"生效，而用户机器上已经躺着
// 用户级 agent 的（mini 上 7 个）不搬迁的话重启后照样不会自己起来 ——
// 那正是这个功能要解决的问题本身。幂等：没装 / 已是系统级的直接跳过。
//
// 返回 (搬迁成功数, 失败原因)。失败不 panic、不中止其他应用：一台机器上
// 某个服务的 formula 服务定义没了（brew 换版）不该挡住别的服务。
func (m *Manager) MigrateSystemDaemons(ctx context.Context) (int, []string) {
	if os.Geteuid() != 0 {
		return 0, []string{"面板不是以 root 运行，跳过系统级服务迁移"}
	}
	var migrated int
	var errs []string
	for _, app := range Catalog() {
		if !systemDaemonNeeded(app) {
			continue
		}
		if ctx.Err() != nil {
			errs = append(errs, "迁移被中断："+ctx.Err().Error())
			break
		}
		// 只处理"用户级 plist 还在、系统级还没有"的（两套命名都算）。
		if label := m.systemDaemonExistingLabel(ctx, app.BrewFormula); label != "" {
			// 已经是系统级：把可能残留的用户级 agent 清掉，避免重启后两份实例；
			// 并把记录对齐到真实的 label/plist（旧安装留下的另一套命名会让界面
			// 显示成"找不到 plist"，而服务其实在跑）。
			if err := m.cleanupUserAgents(ctx, app.BrewFormula); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", app.Name, err))
			}
			m.syncServiceRecords(ctx, app.BrewFormula, label, SystemDaemonPlistPath(label))
			continue
		}
		if !m.hasUserAgent(ctx, app.BrewFormula) {
			continue
		}
		res := &InstallResult{App: app.ID, Name: app.Name, Steps: []string{}}
		if _, _, err := m.ensureSystemDaemon(ctx, app, res); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", app.Name, err))
			continue
		}
		migrated++
	}
	return migrated, errs
}

package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/zizpanel/internal/mysql"
	"github.com/zizdog/zizpanel/internal/priv"
)

// ============================================================================
//  数据库引擎的互斥护栏（用户 2026-09-20 定的产品规则）
//
//  **MySQL 与 MariaDB 只可能选装一个**，面板不支持共存，也不做"帮你停/卸"：
//    1. 目标引擎自己已装 → 放行（幂等重装/启动，同一个引擎）；
//    2. 另一个引擎已装（不管在不在跑）→ **拒绝**，并给一句能照做的人话
//       （brew services stop X && brew uninstall X）——换引擎由用户自己手动做；
//    3. 两个都装着（异常/历史状态）→ 如实报"只支持装一个"，指出 3306 上
//       当前生效的是哪个；两个入口都拒绝，**唯一例外**是"它自己就是当前
//       生效引擎"的幂等重装。
//
//  两个引擎的 Homebrew formula 都把数据目录编译成 <brew>/var/mysql、服务都监听
//  3306（mariadb 的 cmake 参数 -DMYSQL_DATADIR=#{var}/mysql），这正是不许共存的原因。
//
//  ⚠️ 护栏只拒绝，**绝不替用户停服务/卸包**：正在跑的数据库可能是用户的生产库。
//
//  判据全部复用现成探测：brew 已装列表（InstalledFormulaVersions）+ 端口占用者
//  （checkPort/lsof，形如 "mariadbd (pid 35985)"）；探测函数可注入，便于单测。
// ============================================================================

// dbEngineFormulas 是引擎 → 要装的 formula（唯一映射）。
var dbEngineFormulas = map[string]string{
	"mysql":   "mysql@8.4",
	"mariadb": "mariadb",
}

// dbEngineOfFormula 反查 formula 属于哪个引擎（"" = 不是数据库引擎）。
//
// 认前缀而不是只认目录里那两个 formula：用户手工装的 mysql / mysql@8.0 也是
// MySQL 引擎，凭据闭环与数据目录初始化同样要按它来。
func dbEngineOfFormula(formula string) string {
	f := strings.TrimSpace(formula)
	switch {
	case f == "mariadb" || strings.HasPrefix(f, "mariadb@"):
		return "mariadb"
	case f == "mysql" || strings.HasPrefix(f, "mysql@"):
		return "mysql"
	}
	return ""
}

// DBEngineOfFormula 是 dbEngineOfFormula 的导出包装：web 层判"当前生效引擎"
// （读 brew services 状态）时要用同一套 formula→引擎 映射，不能再抄一份。
func DBEngineOfFormula(formula string) string { return dbEngineOfFormula(formula) }

// DBEngineOtherFormula 返回"另一个数据库引擎"的 formula（不是引擎时 ok=false）。
// web 层也用它（市场卡片提前提示"装了另一个、点安装会被拒"）。
func DBEngineOtherFormula(formula string) (string, bool) {
	switch dbEngineOfFormula(formula) {
	case "mysql":
		return dbEngineFormulas["mariadb"], true
	case "mariadb":
		return dbEngineFormulas["mysql"], true
	}
	return "", false
}

// portSharingAllowed 判断两个目录条目是否可以**故意**共用同一个端口。
//
// 只有"同一个组件位的两个引擎"（MySQL 8.4 / MariaDB：默认数据目录与 3306 都相同，
// 只允许装一个，装之前有互斥护栏）才允许。目录门禁据此放行这一对，
// 其余任何端口重复仍然是错误。
func portSharingAllowed(a, b App) bool {
	ea, eb := dbEngineOfFormula(a.BrewFormula), dbEngineOfFormula(b.BrewFormula)
	return ea != "" && eb != "" && ea != eb
}

// dbEngineDisplay 是引擎的展示名（用户可见文案里不许把 MariaDB 写成 MySQL）。
func dbEngineDisplay(engine string) string {
	if engine == "mariadb" {
		return "MariaDB"
	}
	if engine == "mysql" {
		return "MySQL"
	}
	return engine
}

// dbEngineFormulaDisplay 按 formula 给出展示名（下拉框/错误信息用）。
func dbEngineFormulaDisplay(formula string) string {
	if e := dbEngineOfFormula(formula); e != "" {
		return dbEngineDisplay(e)
	}
	return strings.TrimSpace(formula)
}

// dbEngineProcName 是"这个引擎在 lsof 里长什么样"（唯一实现在 internal/mysql 的
// engineKegs.Proc，这里只做 string ↔ DBEngine 的转接）。
func dbEngineProcName(engine string) string {
	return mysql.EngineProcessName(mysql.DBEngine(engine))
}

// DBEngineStatus 是一次"数据库引擎现状"的探测结论（两个引擎都问）。
//
// 导出是为了让 web 层单测能注入（SetDBEngineProbeForTest）：默认实现会真的跑
// brew list 与 lsof —— 单测不许碰真实服务，结论也不该随开发机装没装数据库而漂。
type DBEngineStatus struct {
	// Target 是这次要装/要检查的引擎（"mysql" / "mariadb"）。
	Target mysql.DBEngine
	// TargetInstalled = brew 里装着这次要装的那个引擎。
	TargetInstalled bool
	// OtherInstalled = brew 里装着另一个引擎（**不管在不在跑**，按产品规则都要拒）。
	OtherInstalled bool
	// Holders 是 3306 上的监听者（lsof 的 "名字 (pid N)" 形式；空 = 端口空闲）。
	Holders []string
	// ProbeOK=false 表示 brew 探测失败（"未复核"，绝不是"什么都没装"）。
	ProbeOK bool
	// DataDir 是两者共用的数据目录；DataDirNonEmpty 表示它已存在且非空。
	DataDir         string
	DataDirNonEmpty bool
}

// DBEngineConflict 是护栏的结论。
type DBEngineConflict struct {
	// Blocked 非空 = 必须拒绝这次安装（人话原因 + 出路）。
	Blocked string
	// Warning 非空 = 没拦住，但必须如实告知（例如数据目录里还有上一个引擎的数据）。
	Warning string
}

// SetDBEngineProbeForTest 替换"某个引擎装没装/在不在跑"的探测，返回值供测试恢复。
func (m *Manager) SetDBEngineProbeForTest(fn func(ctx context.Context, target mysql.DBEngine) DBEngineStatus) func() {
	prev := m.dbEngineProbe
	m.dbEngineProbe = fn
	return func() { m.dbEngineProbe = prev }
}

// CheckDBEngineConflict 判断"要装 formula 这个引擎"能不能进行。
//
// formula 不是数据库引擎时返回零值（不干涉）。规则见文件头；判据顺序：
//  1. 目标与对方**都装着** → 只有"目标自己就是当前生效引擎"才放行，其余拒绝
//     并指出 3306 上生效的是哪个；
//  2. 对方已装（不管在不在跑）→ 拒绝，给 brew services stop/uninstall 的可照做命令；
//  3. 端口上跑着对方引擎（brew 里查不到记录，例如手工装）→ 拒绝；
//  4. 目标自己在 3306 上 → 放行（幂等重装）；
//  5. 3306 被无关进程占用 → 拒绝并点名（装起来也绑不上）；
//  6. brew 没复核成 → 数据目录空就照常装（全新机器），有数据则拒绝（不猜）。
func (m *Manager) CheckDBEngineConflict(ctx context.Context, formula string) DBEngineConflict {
	engine := dbEngineOfFormula(formula)
	if engine == "" {
		return DBEngineConflict{}
	}
	target := mysql.DBEngine(engine)
	otherEngine := "mariadb"
	if engine == "mariadb" {
		otherEngine = "mysql"
	}
	targetFormula, otherFormula := dbEngineFormulas[engine], dbEngineFormulas[otherEngine]
	me, other := dbEngineDisplay(engine), dbEngineDisplay(otherEngine)

	st := m.dbEngineProbeFn()(ctx, target)
	datadir := strings.TrimSpace(st.DataDir)
	if datadir == "" {
		datadir = filepath.Join(m.brewPrefix(), "var", "mysql")
	}
	selfOnPort := m.portShowsEngine(st.Holders, target)
	otherHolder, otherOnPort := holderMatching(st.Holders, dbEngineProcName(otherEngine))

	// ① 两个都装着（历史/异常状态）：面板只支持一个，如实说清并要求用户自己卸一个。
	if st.ProbeOK && st.TargetInstalled && st.OtherInstalled {
		if selfOnPort {
			return DBEngineConflict{} // 唯一例外：它自己就是当前生效引擎 → 幂等重装
		}
		effective := "3306 上没有检测到其中任何一个在跑，面板判断不出该保留哪个"
		if otherOnPort {
			effective = fmt.Sprintf("3306 上生效的是 %s（%s）", other, otherHolder)
		}
		return DBEngineConflict{Blocked: fmt.Sprintf(
			"检测到两个引擎都装着（%s 与 %s），面板只支持装一个。%s。"+
				"请自己决定卸载哪个（面板不会替你停/卸）："+
				"`brew services stop <要卸的> && brew uninstall <要卸的>`",
			targetFormula, otherFormula, effective)}
	}

	// ② 另一个引擎已装（不管在不在跑）→ 拒绝：只允许装一个，换引擎由用户手动做。
	if st.ProbeOK && st.OtherInstalled {
		return DBEngineConflict{Blocked: fmt.Sprintf(
			"只能装一个数据库引擎：检测到已安装 %s，请先卸载它再装 %s"+
				"（brew services stop %s && brew uninstall %s）",
			otherFormula, targetFormula, otherFormula, otherFormula)}
	}

	// ③ 端口上跑着对方引擎，但 brew 里没有它的记录（手工装/换了路径）→ 同样拒绝。
	if otherOnPort {
		return DBEngineConflict{Blocked: fmt.Sprintf(
			"只能装一个数据库引擎：3306 上检测到另一个引擎在运行（%s），"+
				"而 Homebrew 里没有它的安装记录。请先停掉它再装 %s —— "+
				"面板不会替你停正在运行的数据库", otherHolder, targetFormula)}
	}

	// ④ 目标自己就在 3306 上跑 → 放行（幂等重装，不碰它）。
	if selfOnPort {
		return DBEngineConflict{}
	}

	// ⑤ 3306 被别的进程占着 → 拒绝并点名（装起来也绑不上这个端口）。
	if len(st.Holders) > 0 {
		return DBEngineConflict{Blocked: fmt.Sprintf(
			"3306 已被 %s 占用：%s 起来后绑不上这个端口。"+
				"请先确认那是什么程序并腾出 3306（面板不会停别人的进程）",
			strings.Join(st.Holders, "、"), me)}
	}

	// ⑥ brew 探测失败：不能凭"装没装"下结论。全新机器上 brew 可能都还没有，
	//    所以只在数据目录已有数据（说不清属于谁）时才拒绝。
	if !st.ProbeOK {
		if st.DataDirNonEmpty {
			return DBEngineConflict{Blocked: fmt.Sprintf(
				"无法复核 Homebrew 里装了哪个数据库引擎（brew 探测失败），而数据目录 %s 已有数据："+
					"面板不猜，请先确认这份数据属于哪个引擎、并卸载另一个", datadir)}
		}
		return DBEngineConflict{}
	}

	// ⑦ 没装任何引擎、端口空闲。数据目录非空时提示一句（可能是上一个引擎留下的
	//    数据，仍可能让新引擎起不来），但不拦 —— 这不构成"共存"。
	if st.DataDirNonEmpty {
		return DBEngineConflict{Warning: fmt.Sprintf(
			"数据目录 %s 已有数据（可能是上一个引擎留下的）：本次安装会直接用它，"+
				"如果新引擎起不来，请确认这份数据属于谁", datadir)}
	}
	return DBEngineConflict{}
}

// portShowsEngine 判断端口监听者里有没有"这个引擎自己的服务端进程"
// （进程名吻合，或它自己的 launchd 作业 PID 就是占用者）。
func (m *Manager) portShowsEngine(holders []string, engine mysql.DBEngine) bool {
	if _, ok := holderMatching(holders, dbEngineProcName(string(engine))); ok {
		return true
	}
	label := m.brewLabelFor(dbEngineFormulas[string(engine)])
	if label == "" {
		return false
	}
	st, err := priv.LaunchStatus(label)
	if err != nil || st.PID <= 0 {
		return false
	}
	return holdersContainPID(holders, st.PID)
}

// dbEngineProbeFn 返回当前生效的探测函数（注入优先）。
func (m *Manager) dbEngineProbeFn() func(ctx context.Context, target mysql.DBEngine) DBEngineStatus {
	if m.dbEngineProbe != nil {
		return m.dbEngineProbe
	}
	return m.realDBEngineProbe
}

// realDBEngineProbe 是生产探测：brew 已装列表（两个引擎都查）+ 3306 占用者 + 数据目录。
func (m *Manager) realDBEngineProbe(ctx context.Context, target mysql.DBEngine) DBEngineStatus {
	res := DBEngineStatus{Target: target, DataDir: filepath.Join(m.brewPrefix(), "var", "mysql")}
	if vers, ok := m.InstalledFormulaVersions(ctx); ok {
		res.ProbeOK = true
		res.TargetInstalled = dbEngineInstalledIn(vers, dbEngineFormulas[string(target)])
		other := "mariadb"
		if string(target) == "mariadb" {
			other = "mysql"
		}
		res.OtherInstalled = dbEngineInstalledIn(vers, dbEngineFormulas[other])
	}
	if info, err := m.checkPort(3306); err == nil {
		res.Holders = info.Holders
	}
	if entries, err := os.ReadDir(res.DataDir); err == nil && len(entries) > 0 {
		res.DataDirNonEmpty = true
	}
	return res
}

// dbEngineInstalledIn 判断"brew 已装列表里有没有这个引擎"。
// 既认精确 formula（mariadb），也认版本化别名（mariadb@13.0）与无后缀回落（mysql@8.4 ← mysql）。
func dbEngineInstalledIn(vers map[string]string, formula string) bool {
	if _, _, ok := ResolveBrewFormula(formula, vers); ok {
		return true
	}
	for f := range vers {
		if strings.HasPrefix(f, formula+"@") {
			return true
		}
	}
	return false
}

// ResolveDBEngine 解析"面板现在该连哪个数据库引擎"（数据库页 / 站点运行时用）。
//
// 两个引擎都装着时用 3306 的**监听者**判开（mariadbd → MariaDB，mysqld → MySQL）；
// 判不开就把"无法判断"原样带回，绝不挑一个默认值。只查端口（lsof），不跑 brew。
func (m *Manager) ResolveDBEngine(brewPrefix, socket string) (mysql.EnginePaths, error) {
	return mysql.ResolveEnginePreferring(brewPrefix, socket, m.dbPortHolders(3306))
}

// dbPortHolders 返回端口上的监听者（形如 ["mariadbd (pid 35985)"]）。
// 读不到（lsof 失败）返回 nil —— 调用方据此"判不开"，不许当成"端口空闲"。
func (m *Manager) dbPortHolders(port int) []string {
	if info, err := m.checkPort(port); err == nil {
		return info.Holders
	}
	return nil
}

// holderMatching 在端口占用者列表里找"进程名吻合"的那一个。
// Holders 形如 ["mysqld (pid 950)"]（见 priv.CheckPort）。
func holderMatching(holders []string, proc string) (string, bool) {
	if proc == "" {
		return "", false
	}
	for _, h := range holders {
		name := strings.ToLower(strings.TrimSpace(strings.SplitN(h, "(", 2)[0]))
		if name != "" && strings.Contains(name, proc) {
			return h, true
		}
	}
	return "", false
}

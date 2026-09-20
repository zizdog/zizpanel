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
//  数据库引擎的互斥护栏（MySQL 8.4 / MariaDB 只能二选一）
//
//  两个引擎的 Homebrew formula 把数据目录都编译成 <brew>/var/mysql，服务都监听
//  3306（mariadb 的 cmake 参数 -DMYSQL_DATADIR=#{var}/mysql，service 里
//  `mariadbd-safe --datadir=#{var}/mysql`）。所以"装 A 时 B 正在跑"不是端口冲突
//  那么轻：新引擎会去开**另一个引擎的数据目录**。
//
//  ⚠️ 护栏只拒绝，**绝不替用户停服务**：正在跑的数据库可能是用户的生产库。
//  停它是用户的事，面板只负责把话说清。
//
//  判据全部复用现成探测：brew 已装列表（InstalledFormulaVersions）+ 3306 端口
//  占用者（checkPort/lsof，形如 "mysqld (pid 950)"）；探测函数可注入，便于单测。
// ============================================================================

// dbEngineFormulas 是引擎 → 要装的 formula（唯一映射，两处都从它取）。
var dbEngineFormulas = map[string]string{
	"mysql":   "mysql@8.4",
	"mariadb": "mariadb",
}

// dbEngineOfFormula 反查 formula 属于哪个引擎（"" = 不是数据库引擎）。
//
// 认前缀而不是只认目录里那两个 formula：用户手工装的 mysql / mysql@8.0 也是
// MySQL 引擎，凭据闭环与数据目录初始化同样要按它来（旧实现只认 mysql@8.4|mysql，
// 装别的版本就静默跳过收尾）。
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

// portSharingAllowed 判断两个目录条目是否可以**故意**共用同一个端口。
//
// 只有"同一个组件位的两个引擎"（MySQL 8.4 / MariaDB：默认数据目录与 3306 都相同，
// 装之前有互斥护栏，同一时刻只能跑一个）才允许。目录门禁据此放行这一对，
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

// DBEngineProbeResult 是一次"某个引擎现在什么状态"的探测结果。
//
// 导出是为了让 web 层单测能注入（SetDBEngineProbeForTest）：默认实现会真的跑
// brew list 与 lsof —— 单测不许碰真实服务，结论也不该随开发机装没装数据库而漂。
type DBEngineProbeResult struct {
	// Installed 是 brew 里装着这个 formula。
	Installed bool
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
	// Warning 非空 = 没拦住，但必须如实告知（对方已装未跑 / 数据目录已有数据）。
	Warning string
}

// SetDBEngineProbeForTest 替换"某个引擎装没装/在不在跑"的探测，返回值供测试恢复。
func (m *Manager) SetDBEngineProbeForTest(fn func(ctx context.Context, formula string) DBEngineProbeResult) func() {
	prev := m.dbEngineProbe
	m.dbEngineProbe = fn
	return func() { m.dbEngineProbe = prev }
}

// CheckDBEngineConflict 判断"要装 formula 这个引擎"能不能进行。
//
// formula 不是数据库引擎时返回零值（不干涉）。判据与顺序：
//  1. 3306 的占用者是**要装的那个引擎自己**（launchd 托管或进程名吻合）→ 放行
//     （幂等重跑，不是冲突）；
//  2. 占用者是**另一个引擎** → 拒绝（绝不停它）；
//  3. 3306 被别的东西占着 → 拒绝（装起来也绑不上端口，说清占用者）；
//  4. 另一个引擎装着但没跑 → 放行 + 告警（共用数据目录，不能交替使用）；
//  5. brew 没复核成 / 数据目录已有数据 → 放行 + 如实告警。
func (m *Manager) CheckDBEngineConflict(ctx context.Context, formula string) DBEngineConflict {
	engine := dbEngineOfFormula(formula)
	if engine == "" {
		return DBEngineConflict{}
	}
	otherEngine := "mariadb"
	if engine == "mariadb" {
		otherEngine = "mysql"
	}
	otherFormula := dbEngineFormulas[otherEngine]
	res := m.dbEngineProbeFn()(ctx, otherFormula)
	datadir := strings.TrimSpace(res.DataDir)
	if datadir == "" {
		datadir = filepath.Join(m.brewPrefix(), "var", "mysql")
	}
	me, other := dbEngineDisplay(engine), dbEngineDisplay(otherEngine)

	// ①②③ 先看 3306 上是谁。
	if h, ok := holderMatching(res.Holders, dbEngineProcName(otherEngine)); ok {
		// 进程名像另一个引擎时，先确认它**不是我们要装的那个引擎自己的实例**
		// （MariaDB 的 mysqld 兼容符号被执行时 p_comm 会是 mysqld）。
		if m.dbEngineHolderIsLaunchd(formula, res.Holders) {
			return DBEngineConflict{}
		}
		return DBEngineConflict{Blocked: fmt.Sprintf(
			"%s 正在运行（%s），两者默认共用数据目录 %s 与 3306 端口，不能同时跑。"+
				"请先停掉它（「服务管理」里停止，或 `brew services stop %s`）—— "+
				"面板不会替你停正在运行的数据库",
			other, h, datadir, otherFormula)}
	}
	if _, ok := holderMatching(res.Holders, dbEngineProcName(engine)); ok {
		return DBEngineConflict{}
	}
	if len(res.Holders) > 0 {
		return DBEngineConflict{Blocked: fmt.Sprintf(
			"3306 已被 %s 占用：%s 起来后绑不上这个端口。"+
				"请先确认那是什么程序并腾出 3306（面板不会停别人的进程）",
			strings.Join(res.Holders, "、"), me)}
	}

	// ④⑤ 端口空闲：能不能装，取决于另一个引擎与数据目录的现状。
	if !res.ProbeOK {
		return DBEngineConflict{Warning: fmt.Sprintf(
			"面板没能复核 Homebrew 里装了什么（brew 探测失败），无法确认 %s 是否已装；"+
				"两者默认共用数据目录 %s，请自行确认", otherFormula, datadir)}
	}
	if res.Installed {
		return DBEngineConflict{Warning: fmt.Sprintf(
			"%s（%s）已经装着（当前没在跑）。两者默认共用数据目录 %s 与 3306："+
				"装完不能同时启动，同一个数据目录也不能给两个引擎交替使用",
			other, otherFormula, datadir)}
	}
	if res.DataDirNonEmpty {
		return DBEngineConflict{Warning: fmt.Sprintf(
			"数据目录 %s 已存在且非空：本次安装会直接用它。"+
				"如果那是另一个引擎留下的数据，新引擎可能起不来 —— 请先确认这份数据属于谁", datadir)}
	}
	return DBEngineConflict{}
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

// dbEngineProbeFn 返回当前生效的探测函数（注入优先）。
func (m *Manager) dbEngineProbeFn() func(ctx context.Context, formula string) DBEngineProbeResult {
	if m.dbEngineProbe != nil {
		return m.dbEngineProbe
	}
	return m.realDBEngineProbe
}

// realDBEngineProbe 是生产探测：brew 已装列表 + 3306 占用者 + 数据目录现状。
func (m *Manager) realDBEngineProbe(ctx context.Context, formula string) DBEngineProbeResult {
	res := DBEngineProbeResult{DataDir: filepath.Join(m.brewPrefix(), "var", "mysql")}
	if vers, ok := m.InstalledFormulaVersions(ctx); ok {
		res.ProbeOK = true
		res.Installed = dbEngineInstalledIn(vers, formula)
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
// 既认精确 formula（mariadb），也认版本化别名（mariadb@13.0）与 php 那种
// 无后缀回落（mysql@8.4 ← mysql）。
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

// dbEngineHolderIsLaunchd 判断 3306 的占用者是不是**这个 formula 自己的**
// launchd 实例（进程名可能被兼容符号改掉，比如 MariaDB 以 mysqld 起）。
// 判据是 PID 相等（复用 holdersContainPID），读不到 label/PID 时返回 false。
func (m *Manager) dbEngineHolderIsLaunchd(formula string, holders []string) bool {
	label := m.brewLabelFor(formula)
	if label == "" {
		return false
	}
	st, err := priv.LaunchStatus(label)
	if err != nil || st.PID <= 0 {
		return false
	}
	return holdersContainPID(holders, st.PID)
}

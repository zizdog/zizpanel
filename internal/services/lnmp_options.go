package services

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ============================================================================
//  一键 LNMP 的"版本选择"
//
//  2026-09-19 用户要求：点「一键 LNMP」时**先让用户选各服务版本**，
//  而不是直接按写死的 nginx + php@8.2 + mysql@8.4 开装。
//
//  这个文件承担三件事：
//    1. LNMPSelection —— "这次要装哪三个 formula"的唯一载体，带校验与派生
//       （Formulas / Ports / ComponentsText），InstallLNMP 与所有收尾路径
//       都从它取值，不再读全局默认值；
//    2. 候选**从应用目录推导**（Catalog() 里 category=网站环境的条目按 formula
//       前缀分组）—— 将来目录里加 php@8.5，这里自动就多一个候选，不会漏改；
//    3. Manager.LNMPOptions —— 给前端的只读候选接口数据（含"已安装"探测）。
//
//  默认值（nginx / php@8.2 / mariadb）是**产品默认**，用户 2026-09-20 明确要求
//  数据库默认 MariaDB（MySQL 仍可选，但两者只能装一个，见 lnmp_engine.go）。
// ============================================================================

// LNMPSelection 是一次一键 LNMP 安装所选的三件套。
//
// 三个字段都是 **brew formula**（如 "nginx" / "php@8.4" / "mysql@8.4"），
// 不是目录 ID（php84）。用 formula 是因为安装/注册/验证的全部链路都以
// formula 为键（brew install、installSystemDaemons、BrewLabelFor、
// phpVersionFromFormula）；用目录 ID 反而要多一次映射，多一处会走样的地方。
//
// 字段用 string 而不是指针：零值 "" 表示"没选"，Validate 会当场拒绝并说清
// 是哪个组件没选 —— 半成品选择绝不允许进入安装流程。
type LNMPSelection struct {
	Nginx string `json:"nginx"`
	PHP   string `json:"php"`
	MySQL string `json:"mysql"`
}

// lnmpComponent 是 LNMP 三件套里的一个"组件位"（不是具体版本）。
//
// 三个组件是**并列且必须各有一个**的：nginx 是入口、PHP 是解析器、
// MySQL 是数据库。候选版本可以有很多个，但每次只能选一个。
type lnmpComponent struct {
	Key   string `json:"key"`   // nginx / php / mysql（前端按它分组，是稳定契约）
	Label string `json:"label"` // 中文显示名（Nginx / PHP / MySQL）；也是人读文案里的组件名
	// GroupLabel 是候选分组的标题；空 = 用 Label。
	// 「数据库」这一位有两个引擎（MySQL 8.4 / MariaDB），只写 MySQL 会让 MariaDB 看不见。
	GroupLabel string `json:"-"`
	// Prefixes 是"哪些 formula 属于这个组件"的判据，例如 "php@"。
	// 用前缀而不是枚举版本号：目录里加一个新版本时这里不用改。
	// mysql 位有两个引擎，所以是两条前缀（mysql@… 与 mariadb）。
	Prefixes []string `json:"-"`
	// Port 是"是否真的起来了"的判定端口（PHP 恒为 0，见 Ports() 的说明）。
	Port int `json:"port"`
}

// lnmpComponents 是"面板能正确收尾的那几类"的**显式白名单**。
//
// 为什么必须是白名单而不是"目录里 category=网站环境 的全部"：
// 一键 LNMP 的收尾工作是**为 nginx / PHP / MySQL 这三者写的** ——
//
//	· nginx：改 listen 80、建 vhosts、补 include、默认站点、WS 升级映射；
//	· PHP  ：每版本专属 socket 的端点闭环、站点 fastcgi_pass；
//	· MySQL：数据目录初始化、root 凭据闭环（面板 config 与服务器对齐）、3306 判定。
//
// 目录里 category=网站环境 还有一个 **postgresql@17**（它是给 Miniflux 这类
// 自托管应用用的）。把它选进"一键 LNMP"会得到最难查的一种失败：包装上了、
// 系统级守护进程也注册了，然后面板去做 MySQL 的初始化与凭据闭环 ——
// 而机器上根本没有 MySQL；同时 nginx/PHP 那套收尾对 PG 一点用都没有。
// 所以这里**按前缀明确拒绝**它，并在错误信息里说清原因（用户看得到"为什么不行"）。
//
// 判据是前缀 + 注释说明，不是把版本号抄一份：将来加 php@8.5 只需目录里加条目。
var lnmpComponents = []lnmpComponent{
	{Key: "nginx", Label: "Nginx", Prefixes: []string{"nginx"}, Port: 80},
	{Key: "php", Label: "PHP", Prefixes: []string{"php@"}, Port: 0},
	// 数据库位有两个引擎（MySQL 8.4 / MariaDB），两者默认共用数据目录与 3306，
	// 所以它们是**同一个组件位的两个候选**，不是两个组件（见 lnmp_engine.go 的护栏）。
	{Key: "mysql", Label: "MySQL", GroupLabel: "MySQL / MariaDB",
		Prefixes: []string{"mysql@", "mariadb"}, Port: 3306},
}

// lnmpUnknownGroupHint 是"这个 formula 不属于 LNMP 三件套"时给出的人话原因。
//
// 单独抽出来是因为两处都要用（校验与候选推导），而且这句话是**给用户看的
// 产品解释**，不该散落在两个地方各写一个版本。模板参数是 formula 名。
const lnmpUnknownGroupHint = "%s 不属于一键 LNMP 能收尾的组件（只支持 nginx / PHP / MySQL / MariaDB）。" +
	"PostgreSQL 虽然也在「网站环境」里，但它需要独立的数据目录与账号体系，" +
	"一键 LNMP 的收尾（默认站点、数据库初始化与 root 凭据闭环）会对它无效 —— " +
	"请到「应用市场 → 网站环境」里单独安装它"

// lnmpGroupOf 返回某个 formula 属于哪个组件位；不属于三件套时返回 false。
//
// 判据是前缀（见 lnmpComponents 的注释）。空串一律不属于。
func lnmpGroupOf(formula string) (lnmpComponent, bool) {
	f := strings.TrimSpace(formula)
	if f == "" {
		return lnmpComponent{}, false
	}
	for _, c := range lnmpComponents {
		for _, p := range c.Prefixes {
			if strings.HasPrefix(f, p) {
				return c, true
			}
		}
	}
	return lnmpComponent{}, false
}

// DefaultLNMPSelection 是"用户不选"时用的默认三件套（= **产品默认**，不是历史行为）。
//
// 数据库默认 **MariaDB**（用户 2026-09-20 要求）：老 body 缺 db_engine 时也走它。
// 与 LNMPFormulas 是**同一件事的两种写法**，由测试锁死它们不许漂
// （见 TestDefaultLNMPSelectionMatchesLNMPFormulas）。
func DefaultLNMPSelection() LNMPSelection {
	return LNMPSelection{Nginx: "nginx", PHP: "php@8.2", MySQL: "mariadb"}
}

// Validate 校验这次选择能不能装，失败时返回**人话**错误。
//
// 规则（都来自"面板真的会正确收尾"这个能力边界，不是偏好）：
//  1. 三个组件必须各选一个（缺一个就拒绝，并点名是哪个）；
//  2. 组件不许重复（同一个组件位选了两次说明前端/调用方拼错了）；
//  3. 必须是目录里真实存在、category=网站环境、且有 BrewFormula 的条目
//     —— 目录是"面板真的知道怎么装它"的唯一权威来源；
//  4. 必须落在 lnmpComponents 白名单里（前缀判据），PostgreSQL 会被这里拒绝。
func (s LNMPSelection) Validate() error {
	if err := s.ensureComponentsPresent(); err != nil {
		return err
	}
	seen := map[string]string{} // 组件 key → 已选的 formula
	for _, f := range []string{s.Nginx, s.PHP, s.MySQL} {
		f = strings.TrimSpace(f)
		group, ok := lnmpGroupOf(f)
		if !ok {
			return fmt.Errorf(lnmpUnknownGroupHint, f)
		}
		if prev, dup := seen[group.Key]; dup {
			return fmt.Errorf("「%s」组件选了不止一个版本（%s 与 %s）：一键 LNMP 每次只能装一个 %s，"+
				"要多版本共存请装完再用「应用市场 → 网站环境」单独添加",
				group.Label, prev, f, group.Label)
		}
		seen[group.Key] = f

		app, ok := lnmpCatalogApp(f)
		if !ok {
			return fmt.Errorf("应用目录里没有 %s 这个组件（可能版本号写错了）", f)
		}
		if app.Category != CategoryLNMP {
			return fmt.Errorf("%s 不在「网站环境」分类里（实际分类 %q），一键 LNMP 只装网站环境组件",
				f, app.Category)
		}
		if strings.TrimSpace(app.BrewFormula) == "" {
			return fmt.Errorf("%s 没有 Homebrew formula（面板不知道怎么装它），不能用于一键 LNMP", f)
		}
	}
	return nil
}

// ensureComponentsPresent 检查三个组件位都填了值。
//
// 单独一步是为了让"什么都没选"（空 body 解析失败 / 前端漏传）得到一句
// 明确的话，而不是落到"不属于一键 LNMP"这种听起来像版本号写错的错误上。
func (s LNMPSelection) ensureComponentsPresent() error {
	missing := []string{}
	for _, c := range lnmpComponents {
		if strings.TrimSpace(s.formulaFor(c.Key)) == "" {
			missing = append(missing, c.Label)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("一键 LNMP 需要在 %s 各选一个版本，"+
		"现在没有选：%s（一个都不能少：nginx 是入口、PHP 负责解析站点、数据库是 MySQL 或 MariaDB）",
		lnmpComponentLabels(), strings.Join(missing, "、"))
}

// lnmpComponentLabels 生成"Nginx / PHP / MySQL"给用户看的组件清单（错误信息用）。
func lnmpComponentLabels() string {
	labels := make([]string, 0, len(lnmpComponents))
	for _, c := range lnmpComponents {
		labels = append(labels, c.Label)
	}
	return strings.Join(labels, "、")
}

// formulaFor 按组件 key 取这次选中的 formula（空串 = 没选）。
func (s LNMPSelection) formulaFor(key string) string {
	switch key {
	case "nginx":
		return s.Nginx
	case "php":
		return s.PHP
	case "mysql":
		return s.MySQL
	}
	return ""
}

// Formulas 返回这次要装的 formula 列表，**顺序固定为 nginx 在前**。
//
// 顺序有意义（与老 LNMPFormulas 的约定一致）：nginx 是入口，先装它，
// 用户能在最早的时刻看到东西；PHP 的端点闭环也必须排在注册系统级服务之前。
// 用 lnmpComponents 的顺序生成，而不是手写字面量切片 —— 加组件时不会漏。
//
// 只在选择已经过 Validate 时才是完整的三条；未校验的零值会返回空列表，
// 调用方据此判断"没得可装"（而不是拿空列表去跑安装）。
func (s LNMPSelection) Formulas() []string {
	out := make([]string, 0, len(lnmpComponents))
	for _, c := range lnmpComponents {
		f := strings.TrimSpace(s.formulaFor(c.Key))
		if f == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Ports 返回每个 formula 的判定端口（"是否真的起来了"用）。
//
// 与老 LNMPPorts 的两点差别：
//   - 按**组件**给端口，所以 php@8.4 与 php@8.2 都能拿到 0（走专属 socket 判定）、
//     mysql@9.0 也能拿到 3306（3306 是 MySQL 协议的既成事实，不随版本变）；
//   - PHP 恒为 0 是**有意义**的：面板让每个 php@x.y 监听自己专属的 Unix socket，
//     留一个假的 9000 只会误报"PHP 没在监听"（详见 lnmp.go 里 LNMPPorts 的注释）。
func (s LNMPSelection) Ports() map[string]int {
	out := map[string]int{}
	for _, f := range s.Formulas() {
		group, ok := lnmpGroupOf(f)
		if !ok {
			continue
		}
		out[f] = group.Port
	}
	return out
}

// ComponentsText 生成"nginx / PHP 8.4 / MySQL 8.4"这种人看的组件清单。
//
// 组件名 + 版本号，而不是直接抄目录条目的展示名（"PHP 8.2 (FPM)"）：
// 任务标题里出现 "(FPM)" 是噪声，而目录里的 MySQL 叫 "MySQL 8.4"、PHP 叫
// "PHP 8.2 (FPM)" —— 两者形态不一致，拼出来的标题会像两个来源各写了一半。
// 版本号从 formula 的 @ 后面取（不猜）；nginx 没有版本后缀，原样输出
// （硬塞一个并不存在的版本号只会变成假信息）。
func (s LNMPSelection) ComponentsText() string {
	parts := make([]string, 0, len(lnmpComponents))
	for _, c := range lnmpComponents {
		f := strings.TrimSpace(s.formulaFor(c.Key))
		if f == "" {
			continue
		}
		parts = append(parts, lnmpComponentTextName(c, f))
	}
	return strings.Join(parts, " / ")
}

// lnmpComponentTextName 返回某个组件在人读清单里的名字。
//
// 数据库位有两个引擎：MariaDB 的 formula 没有 @版本，沿用组件名会变成
// "日志说 MySQL、装的是 MariaDB"。所以按 formula 取名，取不到版本时用
// 目录展示名（MariaDB 13.0），再取不到才退回 formula 全名。
func lnmpComponentTextName(c lnmpComponent, formula string) string {
	mariadb := c.Key == "mysql" && dbEngineOfFormula(formula) == "mariadb"
	if !mariadb {
		if v := lnmpFormulaVersion(formula); v != "" {
			return c.Label + " " + v
		}
		return formula
	}
	if v := lnmpFormulaVersion(formula); v != "" {
		return "MariaDB " + v
	}
	if a, ok := lnmpCatalogApp(formula); ok && strings.TrimSpace(a.Name) != "" {
		return a.Name
	}
	return formula
}

// DBEngine 返回本次选择的数据库引擎（"mysql" / "mariadb"；没选/认不出时空串）。
//
// 引擎是 formula 的函数（唯一映射见 lnmp_engine.go 的 dbEngineFormulas），
// 所以不额外存一份状态 —— 两份状态必然漂。
func (s LNMPSelection) DBEngine() string {
	return dbEngineOfFormula(strings.TrimSpace(s.MySQL))
}

// lnmpFormulaVersion 从 formula 里取版本号（"php@8.4" → "8.4"，"nginx" → ""）。
//
// 只认 @ 之后那一段。取不到就返回空串，调用方退回 formula 全名 ——
// 绝不猜一个版本号（猜错就是"日志说 8.2、装的是 8.4"那种假信息）。
func lnmpFormulaVersion(formula string) string {
	i := strings.Index(formula, "@")
	if i < 0 || i == len(formula)-1 {
		return ""
	}
	return formula[i+1:]
}

// ---------------------------------------------------------------------------
//  候选（从应用目录推导）
// ---------------------------------------------------------------------------

// lnmpOption 是候选接口里的一项（一个可选的 formula）。
type lnmpOption struct {
	Formula string `json:"formula"`
	// Name / Summary 来自目录条目（目录查不到时用 formula 兜底）。
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Recommended 与默认选择一致（PHP 8.2 为推荐）。
	Recommended bool `json:"recommended"`
	// Installed 是**真实探测**结果（本机装没装），不是猜的。
	Installed bool `json:"installed"`
	// Note 是给用户看的补充说明（目前用于 PostgreSQL 那种"为什么不在候选里"的
	// 相邻提示，以及缺失目录条目时的实话）。没有就别发。
	Note string `json:"note,omitempty"`
}

// lnmpOptionGroup 是候选接口里的一组（一个组件位及其所有候选版本）。
type lnmpOptionGroup struct {
	Key     string       `json:"key"`
	Label   string       `json:"label"`
	Options []lnmpOption `json:"options"`
	// Selected 是这一组默认该选哪个（= 默认选择的那一项）。
	// 前端据此把 radio 选中；它一定出现在 Options 里（有测试锁死）。
	Selected string `json:"selected"`
}

// lnmpOptionsFromCatalog 从应用目录推导三组候选，**不碰任何系统状态**。
//
// 为什么从 Catalog() 推导而不是在 API 里抄一份版本清单：目录是"面板真的
// 知道怎么装/怎么收尾"的唯一权威来源。抄一份的后果是将来目录加了 php@8.5、
// 弹窗里却选不到（用户以为面板不支持），或者反过来选得到一个目录里没有的
// formula（安装路径当场失败）。
//
// 只有 category=网站环境、有 BrewFormula、且能归到三件套里（前缀判据）的条目
// 才进候选 —— postgresql@17 因此被排除，且排除原因在 lnmpComponents 的注释里
// 写清了产品理由。
func lnmpOptionsFromCatalog() []lnmpOptionGroup {
	type bucket struct {
		options []lnmpOption
		ids     map[string]bool
	}
	buckets := map[string]*bucket{}
	for _, c := range lnmpComponents {
		buckets[c.Key] = &bucket{ids: map[string]bool{}}
	}
	formulaSeen := map[string]bool{}
	for _, a := range Catalog() {
		if a.Category != CategoryLNMP {
			continue
		}
		f := strings.TrimSpace(a.BrewFormula)
		if f == "" || formulaSeen[f] {
			continue
		}
		group, ok := lnmpGroupOf(f)
		if !ok {
			// 目录里的 PostgreSQL：不在候选里（理由见 lnmpComponents 注释）。
			continue
		}
		formulaSeen[f] = true
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = f
		}
		buckets[group.Key].options = append(buckets[group.Key].options, lnmpOption{
			Formula: f,
			Name:    name,
			Summary: strings.TrimSpace(a.Summary),
		})
	}
	out := make([]lnmpOptionGroup, 0, len(lnmpComponents))
	for _, c := range lnmpComponents {
		opts := buckets[c.Key].options
		// 排序：版本新的在前（"8.4" 在 "8.2" 之前），跨引擎保持目录顺序。
		// 这样弹窗里默认推荐（php@8.2 / MariaDB）不会因为目录顺序变化而漂。
		sortLNMPOptions(opts)
		label := strings.TrimSpace(c.GroupLabel)
		if label == "" {
			label = c.Label
		}
		out = append(out, lnmpOptionGroup{Key: c.Key, Label: label, Options: opts})
	}
	return out
}

// sortLNMPOptions 是候选排序的**唯一实现**（生产与测试都用它，免得两处漂）。
func sortLNMPOptions(opts []lnmpOption) {
	sort.SliceStable(opts, func(i, j int) bool {
		fi, fj := opts[i].Formula, opts[j].Formula
		// 跨家族（mysql@8.4 vs mariadb）不比版本号：它们的版本不可比，
		// 硬比会把没有 @版本 的 mariadb 排到默认项前面。
		if lnmpOptionFamily(fi) != lnmpOptionFamily(fj) {
			return false
		}
		return compareFormulaVersion(fi, fj) > 0
	})
}

// lnmpOptionFamily 是候选的"引擎/版本家族"：同族才比版本号。
func lnmpOptionFamily(formula string) string {
	if e := dbEngineOfFormula(formula); e != "" {
		return e
	}
	if i := strings.Index(formula, "@"); i > 0 {
		return formula[:i]
	}
	return formula
}

// compareFormulaVersion 比较两个同组 formula 的版本（"php@8.4" vs "php@8.2"）。
//
// 逐段比较数字，段数不同时短的算小（8.2 < 8.2.1）。取不到版本后缀的一律算相等，
// 交给 SliceStable 保持目录原顺序 —— **不许**用字典序：那会把 "8.10" 排到 "8.9" 前面。
func compareFormulaVersion(a, b string) int {
	va, vb := lnmpFormulaVersion(a), lnmpFormulaVersion(b)
	pa, pb := strings.Split(va, "."), strings.Split(vb, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := parseVersionPart(pa[i])
		nb, eb := parseVersionPart(pb[i])
		if !ea || !eb {
			continue // 不是数字（极少见）：这一段不参与比较
		}
		if na != nb {
			if na > nb {
				return 1
			}
			return -1
		}
	}
	switch {
	case len(pa) > len(pb):
		return 1
	case len(pa) < len(pb):
		return -1
	}
	return 0
}

// parseVersionPart 把版本段解析成整数；非数字返回 ok=false。
func parseVersionPart(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// ---------------------------------------------------------------------------
//  Manager：候选接口（含"已安装"真实探测）
// ---------------------------------------------------------------------------

// LNMPOptions 返回给前端的候选数据：三组 + 每组默认选中项 + 真实"已安装"标记。
//
// "已安装"用面板已有的探测，**不猜**：
//   - brew：brewHas（`brew list --versions <formula>`，以真实用户身份跑）；
//   - 面板服务记录 / launchd plist：formula 在注册表里、或磁盘上有它的 plist。
//
// 三者任一命中即为已安装 —— 判据与「应用市场」列表页保持一致（同一件事
// 两处口径不同，用户就会看到"市场说已装、弹窗说没装"）。
func (m *Manager) LNMPOptions(ctx context.Context) []lnmpOptionGroup {
	groups := lnmpOptionsFromCatalog()
	def := DefaultLNMPSelection()

	// 注册表只查一次（不是每个候选查一次）
	recorded := map[string]bool{}
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, svc := range list {
				if svc.Name != "" {
					recorded[svc.Name] = true
				}
				if svc.LaunchLabel != "" {
					recorded[svc.LaunchLabel] = true
				}
			}
		}
	}

	installedFn := m.lnmpInstalledProbe
	if installedFn == nil {
		// brew 的已装集合**整批查一次**（`brew list --versions`），而不是每个候选
		// 查一次（brewHas）。候选最多 4 个，每个查一次相当于把用户点开弹窗的
		// 等待时间翻几倍 —— 这正是坑 134（"进页面顺手探测 8 秒"）的同一类错误。
		brewSet := m.InstalledFormulas(ctx)
		installedFn = func(formula string) bool {
			if brewSet[formula] {
				return true
			}
			if recorded[formula] {
				return true
			}
			for _, label := range m.brewLabelCandidates(formula) {
				if recorded[label] || fileExists(filepath.Join(SystemLaunchDaemonsDir, label+".plist")) {
					return true
				}
			}
			return false
		}
	}

	want := map[string]string{"nginx": def.Nginx, "php": def.PHP, "mysql": def.MySQL}
	for gi := range groups {
		g := &groups[gi]
		g.Selected = want[g.Key]
		for oi := range g.Options {
			o := &g.Options[oi]
			o.Recommended = o.Formula == g.Selected
			o.Installed = installedFn(o.Formula)
		}
	}
	return groups
}

// lnmpInstalledProbe 是 Manager 上的一个测试注入点（见 services.go 的字段说明）。
// 这里不再定义方法：字段为 nil 时走真实探测（brew + 面板记录 + launchd plist）。

// brewLabelCandidates 返回某个 formula 在 launchd 里可能用的**全部**标签。
//
// 磁盘上真实存在的那一个排在最前（brewLabelFor），后面是两套历史命名兜底：
// Homebrew 混用 `homebrew.mxcl.<formula>` 与 `sh.brew.<formula>`（真机上
// php 是后者、mysql 是前者），只认一套就会把"已在跑的服务"判成没装。
func (m *Manager) brewLabelCandidates(formula string) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(l string) {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			return
		}
		seen[l] = true
		out = append(out, l)
	}
	add(m.brewLabelFor(formula))
	add("homebrew.mxcl." + formula)
	add("sh.brew." + formula)
	return out
}

// ---------------------------------------------------------------------------
//  请求体解析（POST /api/v1/market/install-lnmp）
// ---------------------------------------------------------------------------

// lnmpSelectionInput 是 POST /market/install-lnmp 的 JSON body 形状。
//
// 用指针是为了区分"字段没传"与"字段传了空串"：
//   - 没传（nil）   → 用默认选择里的那一项（老客户端只发 {} 或干脆不发表格：
//     数据库默认 **MariaDB**，见 DefaultLNMPSelection）；
//   - 传了空串      → 显式拒绝（400）：这几乎一定是前端拼错了，
//     静默替它填默认值会让用户以为"我选了 8.4"而实际装的是 8.2。
type lnmpSelectionInput struct {
	Nginx *string `json:"nginx"`
	PHP   *string `json:"php"`
	MySQL *string `json:"mysql"`
	// DBEngine 是**数据库引擎维度**（"mysql" / "mariadb"）。它决定 mysql 组件位
	// 装哪个 formula（唯一映射见 lnmp_engine.go 的 dbEngineFormulas）。
	// 与 MySQL 字段同时出现时必须一致（否则 400）——两者都指同一件事，不一致就是拼错了。
	DBEngine *string `json:"db_engine"`
}

// ParseLNMPSelection 把请求体解析成一份**已校验**的选择。
//
// body 为空（nil / 零长度 / 空对象）时返回默认三件套（数据库 = **MariaDB**）：
// 老前端不带 body 时也拿到产品默认值；想要 MySQL 必须显式带 db_engine=mysql
// （或 mysql="mysql@8.4"）——两者只能装一个（见 lnmp_engine.go）。
//
// 任何解析/校验失败都返回**人话**错误，由 web 层原样回 400。
func ParseLNMPSelection(body []byte) (LNMPSelection, error) {
	sel := DefaultLNMPSelection()
	raw := strings.TrimSpace(string(body))
	if raw != "" && raw != "null" {
		var in lnmpSelectionInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return LNMPSelection{}, fmt.Errorf("请求内容不是合法的版本选择（%v）："+
				"应为 {\"nginx\":\"nginx\",\"php\":\"php@8.2\",\"db_engine\":\"mysql\"} 这样的 JSON", err)
		}
		// 引擎维度先落地（它给 mysql 组件位定 formula），再用显式 mysql 字段覆盖。
		if in.DBEngine != nil {
			e := strings.TrimSpace(*in.DBEngine)
			formula, ok := dbEngineFormulas[e]
			if !ok {
				if e == "" {
					return LNMPSelection{}, fmt.Errorf("db_engine 传了空值：请选 mysql 或 mariadb" +
						"（要默认值就别传这个字段）")
				}
				return LNMPSelection{}, fmt.Errorf("未知的数据库引擎 %q：只支持 mysql（MySQL 8.4）或 mariadb", e)
			}
			sel.MySQL = formula
		}
		apply := func(p *string, dst *string, label string) error {
			if p == nil {
				return nil
			}
			v := strings.TrimSpace(*p)
			if v == "" {
				return fmt.Errorf("%s 的版本传了空值：一键 LNMP 三个组件都必须各选一个（要默认值就别传这个字段）", label)
			}
			*dst = v
			return nil
		}
		if err := apply(in.Nginx, &sel.Nginx, "Nginx"); err != nil {
			return LNMPSelection{}, err
		}
		if err := apply(in.PHP, &sel.PHP, "PHP"); err != nil {
			return LNMPSelection{}, err
		}
		if err := apply(in.MySQL, &sel.MySQL, "MySQL"); err != nil {
			return LNMPSelection{}, err
		}
		// 两个字段都指同一件事，不一致就是拼错了：静默取一个会让用户以为装的是另一个。
		if in.DBEngine != nil && in.MySQL != nil {
			if want := strings.TrimSpace(*in.DBEngine); dbEngineOfFormula(sel.MySQL) != want {
				return LNMPSelection{}, fmt.Errorf("db_engine=%q 与 mysql=%q 不一致："+
					"引擎决定装哪个 formula，请只按其中一个填（要 mariadb 就写 db_engine=mariadb）",
					want, sel.MySQL)
			}
		}
	}
	if err := sel.Validate(); err != nil {
		return LNMPSelection{}, err
	}
	return sel, nil
}

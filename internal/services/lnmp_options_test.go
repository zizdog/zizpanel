package services

// lnmp_options_test.go —— 一键 LNMP 的**版本选择**单元测试。
//
// 2026-09-19 用户要求：「一键安装 lnmp 时，弹窗出来先让用户选择各服务版本，
// 而不是直接执行安装。」这一组测试锁住四类东西：
//
//	① 校验规则（默认选择合法；未知 formula / postgresql / 空选择 / 重复组件被拒，
//	   且错误信息含原因）；
//	② 反漂移：候选必须**来自应用目录**（目录里 php82/php84 都能被选到，
//	   postgresql17 不能进 LNMP）；
//	③ 派生方法对 8.4 的选择给出 8.4（而不是默认的 8.2）—— Formulae 顺序、
//	   Ports、ComponentsText 三样都锁；
//	④ 源码结构锁：lnmp.go 里**不许**再有生产代码读全局 LNMPFormulas/LNMPPorts，
//	   否则"用户选了 8.4、注册/文案还是 8.2"这类"只改了一条路径"的坑会复发
//	   （DEVELOPMENT.md 140/141）。
//
// 全部是纯函数/纯结构断言：不装任何东西、不碰真实 brew / launchd / 家目录。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLNMPFormulas / testLNMPPorts 是**旧测试**给 registerLNMPComponents 传的
// "本次选择"。它们每次调用都读全局默认值，所以那些测试里
// `LNMPFormulas = []string{...}` 的写法一字不用改。
//
// 为什么写成函数而不是两个变量：变量只在包初始化那一刻取到旧值，
// 测试随后给 `LNMPFormulas` 重新赋值就影响不到它们了（第一次跑就这么红过）。
//
// 新写的测试**不要**用它们：那是默认值，不是"用户选了什么"。要测选择就用
// LNMPSelection 显式构造（见 TestLNMPSelectionDerivationsFollowPHP84）。
func testLNMPFormulas() []string    { return LNMPFormulas }
func testLNMPPorts() map[string]int { return LNMPPorts }

// TestDefaultLNMPSelectionMatchesLNMPFormulas 锁住"默认值只有一份事实"。
//
// 默认值现在有两种写法（LNMPSelection 结构体与历史导出变量 LNMPFormulas），
// 它们漂掉的后果是"不选时装的"与"界面显示默认选中"的不一致。
func TestDefaultLNMPSelectionMatchesLNMPFormulas(t *testing.T) {
	def := DefaultLNMPSelection()
	if got, want := strings.Join(def.Formulas(), "/"), strings.Join(LNMPFormulas, "/"); got != want {
		t.Errorf("DefaultLNMPSelection().Formulas() = %q，而 LNMPFormulas = %q（默认值必须逐字一致）", got, want)
	}
	for _, f := range def.Formulas() {
		if _, ok := LNMPPorts[f]; !ok {
			t.Errorf("默认组件 %s 在全局 LNMPPorts 里没有判定端口", f)
		}
		if got, want := def.Ports()[f], LNMPPorts[f]; got != want {
			t.Errorf("%s 的默认端口：Ports()=%d 而 LNMPPorts=%d", f, got, want)
		}
	}
}

// TestLNMPSelectionValidation：合法/非法选择的判据与**人话错误信息**。
func TestLNMPSelectionValidation(t *testing.T) {
	cases := []struct {
		name    string
		sel     LNMPSelection
		wantErr string // 空 = 应该通过；非空 = 错误信息必须包含它
	}{
		{
			name: "默认选择合法",
			sel:  DefaultLNMPSelection(),
		},
		{
			name: "PHP 8.4 合法（目录里有这个条目）",
			sel:  LNMPSelection{Nginx: "nginx", PHP: "php@8.4", MySQL: "mysql@8.4"},
		},
		{
			name:    "未知 formula 被拒，且点名是它",
			sel:     LNMPSelection{Nginx: "nginx", PHP: "php@9.9", MySQL: "mysql@8.4"},
			wantErr: "php@9.9",
		},
		{
			name:    "postgresql 被拒，且解释为什么不行",
			sel:     LNMPSelection{Nginx: "nginx", PHP: "php@8.2", MySQL: "postgresql@17"},
			wantErr: "PostgreSQL",
		},
		{
			name:    "postgresql 也不能顶替 nginx",
			sel:     LNMPSelection{Nginx: "postgresql@17", PHP: "php@8.2", MySQL: "mysql@8.4"},
			wantErr: "PostgreSQL",
		},
		{
			name:    "空选择被拒，并说清三个组件一个都不能少",
			sel:     LNMPSelection{},
			wantErr: "Nginx",
		},
		{
			name:    "只缺 MySQL 也要点名 MySQL",
			sel:     LNMPSelection{Nginx: "nginx", PHP: "php@8.2"},
			wantErr: "MySQL",
		},
		{
			name:    "PHP 与 MySQL 选同一个（重复组件）被拒",
			sel:     LNMPSelection{Nginx: "nginx", PHP: "mysql@8.4", MySQL: "mysql@8.4"},
			wantErr: "不止一个版本",
		},
		{
			name:    "空白字符要按没选处理",
			sel:     LNMPSelection{Nginx: "  ", PHP: "php@8.2", MySQL: "mysql@8.4"},
			wantErr: "Nginx",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.sel.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("本应通过校验，实际报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("本应被拒绝（错误信息里要有 %q），实际通过了", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("错误信息里必须含 %q（用户要知道为什么不行），实际：%v", c.wantErr, err)
			}
		})
	}
}

// TestLNMPSelectionDerivationsFollowPHP84 是本轮的**核心反漂移断言**：
// 选了 8.4 之后，三样派生出来的东西都必须是 8.4，而不是默认的 8.2。
//
// 为什么必须测：Formulas/Ports/ComponentsText 是安装、注册、验证、文案
// 四条路径的取值入口。只要有一条还在读全局 LNMPFormulas/LNMPPorts，
// 用户就会看到"装的是 8.4、登记/日志还是 8.2"——这正是本项目反复踩的
// "多条路径只改了一条"。
func TestLNMPSelectionDerivationsFollowPHP84(t *testing.T) {
	sel := LNMPSelection{Nginx: "nginx", PHP: "php@8.4", MySQL: "mysql@8.4"}
	if err := sel.Validate(); err != nil {
		t.Fatalf("8.4 的选择应当合法：%v", err)
	}

	// ① Formulas：nginx 在前，且是 8.4 不是 8.2
	got := sel.Formulas()
	want := []string{"nginx", "php@8.4", "mysql@8.4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Formulas() = %v，期望 %v（顺序也必须 nginx 在前）", got, want)
	}
	for _, f := range got {
		if f == "php@8.2" {
			t.Error("Formulas() 里不该出现默认的 php@8.2（用户选的是 8.4）")
		}
	}

	// ② Ports：按**组件**给端口，所以 8.4 也有正确判定目标
	ports := sel.Ports()
	if ports["mysql@8.4"] != 3306 {
		t.Errorf("mysql@8.4 的判定端口应为 3306，实际 %d", ports["mysql@8.4"])
	}
	if p, ok := ports["php@8.4"]; !ok || p != 0 {
		t.Errorf("php@8.4 的判定端口应为 0（走专属 socket），实际 %d（存在=%v）", p, ok)
	}
	if ports["nginx"] != 80 {
		t.Errorf("nginx 的判定端口应为 80，实际 %d", ports["nginx"])
	}
	if _, ok := ports["php@8.2"]; ok {
		t.Error("Ports() 里不该出现没选的 php@8.2")
	}

	// ③ ComponentsText：人读文案要写 8.4（任务标题也用这个）
	if txt := sel.ComponentsText(); txt != "nginx / PHP 8.4 / MySQL 8.4" {
		t.Errorf("ComponentsText() = %q，期望 %q", txt, "nginx / PHP 8.4 / MySQL 8.4")
	}
	// 非默认版本也要正确（将来目录加 php@8.5 时文案不会写错）
	future := LNMPSelection{Nginx: "nginx", PHP: "php@8.5", MySQL: "mysql@9.0"}
	if txt := future.ComponentsText(); txt != "nginx / PHP 8.5 / MySQL 9.0" {
		t.Errorf("未来版本的 ComponentsText() = %q，期望 %q", txt, "nginx / PHP 8.5 / MySQL 9.0")
	}
}

// TestLNMPOptionsComeFromCatalog 是**反漂移**断言：候选来自应用目录。
//
// 目录里加了 php@8.5 时这里会自动多一个候选（测试不用改）；而目录里的
// postgresql@17 绝不能出现在候选里 —— 一键 LNMP 的收尾工作（默认站点、
// MySQL 初始化与 root 凭据闭环）对 PG 无效，放进去就是一个装得起来、
// 用不了的假功能。
func TestLNMPOptionsComeFromCatalog(t *testing.T) {
	groups := lnmpOptionsFromCatalog()
	if len(groups) != 3 {
		t.Fatalf("候选应有 nginx/PHP/MySQL 三组，实际 %d 组：%+v", len(groups), groups)
	}
	keys := []string{}
	byKey := map[string][]string{}
	for _, g := range groups {
		keys = append(keys, g.Key)
		for _, o := range g.Options {
			byKey[g.Key] = append(byKey[g.Key], o.Formula)
		}
	}
	if strings.Join(keys, ",") != "nginx,php,mysql" {
		t.Fatalf("分组的顺序/键应为 nginx,php,mysql（前端按它渲染），实际 %v", keys)
	}

	// ① 目录里的每个 LNMP 组件版本都要能被选到（php82/php84 都在）
	wantFormulas := map[string]bool{}
	for _, a := range Catalog() {
		if a.Category != CategoryLNMP {
			continue
		}
		if _, ok := lnmpGroupOf(a.BrewFormula); ok {
			wantFormulas[a.BrewFormula] = true
		}
	}
	gotFormulas := map[string]bool{}
	for k, fs := range byKey {
		if len(fs) == 0 {
			t.Errorf("「%s」组一个候选都没有（用户没法选）", k)
		}
		for _, f := range fs {
			gotFormulas[f] = true
		}
	}
	for f := range wantFormulas {
		if !gotFormulas[f] {
			t.Errorf("目录里的 %s 没出现在候选里（用户会以为面板不支持它）", f)
		}
	}
	for f := range gotFormulas {
		if !wantFormulas[f] {
			t.Errorf("候选里的 %s 不是目录里的 LNMP 组件（安装路径会失败）", f)
		}
	}
	// 显式钉住本次的产品事实：PHP 只有 8.2 / 8.4
	if got := byKey["php"]; strings.Join(got, ",") != "php@8.4,php@8.2" {
		t.Errorf("PHP 候选应为 php@8.4 与 php@8.2（新版在前），实际 %v", got)
	}
	if got := byKey["nginx"]; strings.Join(got, ",") != "nginx" {
		t.Errorf("nginx 只应有一个候选，实际 %v", got)
	}
	if got := byKey["mysql"]; strings.Join(got, ",") != "mysql@8.4,mariadb" {
		t.Errorf("数据库候选应为 mysql@8.4 与 mariadb（默认项在前），实际 %v", got)
	}
	// ② postgresql 绝不能进 LNMP 候选
	for _, fs := range byKey {
		for _, f := range fs {
			if strings.Contains(f, "postgres") {
				t.Errorf("候选里出现了 %s：一键 LNMP 的收尾对 PostgreSQL 无效，不允许混进来", f)
			}
		}
	}
	// ③ 候选不能给出目录里没有的 formula（这里反过来用它自己的目录扫描验证）
	for _, fs := range byKey {
		for _, f := range fs {
			app, ok := lnmpCatalogApp(f)
			if !ok || app.Category != CategoryLNMP || app.BrewFormula == "" {
				t.Errorf("候选 %s 在目录里查不到（或缺分类/公式），点安装必然失败", f)
			}
		}
	}
}

// TestLNMPOptionsRecommendedAndSelectedMatchDefault：弹窗默认选中 = 后端默认选择。
//
// 两者漂掉的后果是"用户不点直接确认"装出来的与界面显示的不一样。
// installed 必须来自注入的探测（真实探测在 web 层跑，单测不许碰 brew）。
func TestLNMPOptionsRecommendedAndSelectedMatchDefault(t *testing.T) {
	m, repo := sandboxManager(t)
	// 注入"装没装"：只把 php@8.2 报成已装，用于验证字段真的被用上。
	var probed []string
	m.lnmpInstalledProbe = func(formula string) bool {
		probed = append(probed, formula)
		return formula == "php@8.2"
	}
	// 面板记录也算"已安装"的判据（这条走真实代码路径，仓库是临时库）
	if err := repo.Create(context.Background(), &Service{
		Name: "sh.brew.nginx", DisplayName: "Nginx", LaunchLabel: "sh.brew.nginx",
		Category: CategoryLNMP, Port: 80,
	}); err != nil {
		t.Fatalf("登记一条假记录失败：%v", err)
	}
	if list, err := repo.List(context.Background()); err != nil || len(list) != 1 {
		t.Fatalf("临时仓库里应有一条记录：%v / %d", err, len(list))
	}

	def := DefaultLNMPSelection()
	wantSelected := map[string]string{"nginx": def.Nginx, "php": def.PHP, "mysql": def.MySQL}
	for _, g := range m.LNMPOptions(context.Background()) {
		if g.Selected != wantSelected[g.Key] {
			t.Errorf("「%s」组默认选中应为 %s，实际 %q", g.Label, wantSelected[g.Key], g.Selected)
		}
		found := false
		for _, o := range g.Options {
			if o.Formula == g.Selected {
				found = true
				if !o.Recommended {
					t.Errorf("%s 是默认选中项，Recommended 应为 true", o.Formula)
				}
			} else if o.Recommended {
				t.Errorf("%s 不是默认选中项，不该标 Recommended", o.Formula)
			}
			if o.Name == "" || o.Name == o.Formula {
				t.Errorf("%s 的展示名应来自目录（不是裸 formula），实际 %q", o.Formula, o.Name)
			}
		}
		if !found {
			t.Errorf("「%s」组的 Selected=%q 不在候选列表里（前端会选中一个不存在的项）", g.Label, g.Selected)
		}
	}
	if len(probed) == 0 {
		t.Error("installed 必须来自真实探测（注入点没被调用）")
	}
	// installed 字段真的被填：php@8.2 报已装
	for _, g := range m.LNMPOptions(context.Background()) {
		for _, o := range g.Options {
			if o.Formula == "php@8.2" && !o.Installed {
				t.Error("探测说 php@8.2 已装，接口却报未安装")
			}
			if o.Formula == "php@8.4" && o.Installed {
				t.Error("探测说 php@8.4 没装，接口却报已安装（凭猜测填字段）")
			}
		}
	}
}

// TestParseLNMPSelectionEmptyBodyUsesDefaults：空 body = 默认（向后兼容）。
//
// 老前端/脚本调 POST /market/install-lnmp 时**不带 body**，行为必须与
// 改造前完全一致 —— 否则升级面板会把"重跑一次一键 LNMP"变成
// "报 400 说没选版本"。
func TestParseLNMPSelectionEmptyBodyUsesDefaults(t *testing.T) {
	for _, body := range []string{"", "  ", "{}", "null"} {
		sel, err := ParseLNMPSelection([]byte(body))
		if err != nil {
			t.Errorf("body=%q 应当用默认选择，却报错：%v", body, err)
			continue
		}
		def := DefaultLNMPSelection()
		if sel != def {
			t.Errorf("body=%q 得到 %+v，期望默认 %+v", body, sel, def)
		}
	}
}

// TestParseLNMPSelectionPartialAndInvalid：部分字段 + 非法值的行为。
func TestParseLNMPSelectionPartialAndInvalid(t *testing.T) {
	// 只传 php：其余两项用默认值
	sel, err := ParseLNMPSelection([]byte(`{"php":"php@8.4"}`))
	if err != nil {
		t.Fatalf("只传 php 应当合法（其余用默认）：%v", err)
	}
	if sel.PHP != "php@8.4" || sel.Nginx != "nginx" || sel.MySQL != "mysql@8.4" {
		t.Errorf("部分字段合并结果不对：%+v", sel)
	}

	// 非法值：400 的判据
	bad := []struct {
		body string
		want string
	}{
		{`{"php":"php@9.9"}`, "php@9.9"},
		{`{"mysql":"postgresql@17"}`, "PostgreSQL"},
		{`{"php":""}`, "空值"},
		{`{"php":`, "JSON"},
		{`[]`, "JSON"},
	}
	for _, c := range bad {
		if _, err := ParseLNMPSelection([]byte(c.body)); err == nil {
			t.Errorf("body=%s 应当被拒绝（400），实际通过", c.body)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("body=%s 的错误信息应含 %q，实际：%v", c.body, c.want, err)
		}
	}
}

// TestLNMPNoProductionPathReadsGlobalDefaults 是**源码结构锁**（反漂移）。
//
// 为什么必须有：哪怕校验与派生方法都写对了，只要 InstallLNMP 或它的收尾
// 路径里还有一处读全局 LNMPFormulas / LNMPPorts，用户选了 8.4 之后那条路径
// 就会退回默认值。这类"多条路径只改了一条"的坑本项目踩过两次
// （DEVELOPMENT.md 140/141），所以用源码文本把它钉死。
//
// 允许读它们的地方只有：
//   - 变量定义本身（LNMPFormulas / LNMPPorts 的声明，兼容旧调用方）；
//   - lnmp_options.go 的 DefaultLNMPSelection 与测试辅助。
func TestLNMPNoProductionPathReadsGlobalDefaults(t *testing.T) {
	files := []string{"lnmp.go", "lnmp_options.go", "lnmp_mysql_credentials.go"}
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, ln := range strings.Split(string(b), "\n") {
			code := strings.TrimSpace(ln)
			// 跳过注释行（注释里提到 LNMPFormulas 是有意的说明）
			if strings.HasPrefix(code, "//") {
				continue
			}
			for _, glob := range []string{"LNMPFormulas", "LNMPPorts"} {
				if !strings.Contains(code, glob) {
					continue
				}
				// 声明行本身不算"读"
				if strings.HasPrefix(code, "var ") || strings.HasPrefix(code, glob+" =") ||
					strings.HasPrefix(code, glob+" [") {
					continue
				}
				t.Errorf("%s:%d 生产路径里读了全局 %s（%q）："+
					"一键 LNMP 的版本必须全部来自本次选择（LNMPSelection），"+
					"否则会出现「用户选了 8.4、这条路径还是 8.2」", name, i+1, glob, code)
			}
		}
	}
}

// TestLNMPOptionsInstalledProbeRealPath 验证**真实探测路径**（不注入探针）：
// brew 已装集合 + launchd plist 两条来源都要合并，且全部在临时目录里跑。
//
// 为什么要单独一条：上面那条测试把探测整个换成了桩，只证明了"字段被填"。
// 真实路径（brew + plist + 面板记录）才是用户看到的那个结论，而它很容易
// 因为"只认一套标签前缀/只认一条来源"而误报——真机上 nginx 在系统域、
// mysql 在 user 域、php 又用另一套前缀，只认一条就会显示"未安装"。
//
// 隔离手段：brew 指向临时目录里的假脚本（只回一行 brew list --versions 输出），
// SystemLaunchDaemonsDir 指向临时目录 —— 不碰真 /opt/homebrew、不碰真 launchd。
func TestLNMPOptionsInstalledProbeRealPath(t *testing.T) {
	m, _ := sandboxManager(t)
	prefix := t.TempDir()
	brew := filepath.Join(prefix, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	// 假 brew：`brew list --versions` 输出形如真机的两列格式
	script := "#!/bin/sh\nif [ \"$1\" = \"list\" ]; then\n" +
		"  echo 'nginx 1.31.5'\n  echo 'php@8.2 8.2.29'\nfi\nexit 0\n"
	if err := os.WriteFile(brew, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m.opt.BrewBin = brew
	m.opt.UserName = "" // 免得走 sudo -u 真实用户
	// launchd 目录隔离 + 造一份 mysql@8.4 的系统级 plist（真机上 MySQL 常在这里）
	daemons := t.TempDir()
	SystemLaunchDaemonsDir = daemons
	t.Cleanup(func() { SystemLaunchDaemonsDir = "/Library/LaunchDaemons" })
	m.launchdDirsOverride = []string{daemons}
	if err := os.MkdirAll(daemons, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(daemons, "homebrew.mxcl.mysql@8.4.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, g := range m.LNMPOptions(context.Background()) {
		for _, o := range g.Options {
			got[o.Formula] = o.Installed
		}
	}
	// nginx / php@8.2 来自假 brew 的输出
	if !got["nginx"] || !got["php@8.2"] {
		t.Errorf("brew 已装集合没被用上（nginx=%v php@8.2=%v）—— 用户会看到「已装好的还显示未安装」",
			got["nginx"], got["php@8.2"])
	}
	// mysql@8.4 来自系统级 plist（brew 那份输出里没有它）
	if !got["mysql@8.4"] {
		t.Error("系统级 plist 的已装判定没生效（该组件会被显示成未安装）")
	}
	// php@8.4 两条来源都没有 → 必须如实报未安装（不许猜）
	if got["php@8.4"] {
		t.Error("php@8.4 在假 brew 与 plist 里都不存在，却被报成已安装（凭空捏造状态）")
	}
}

// TestLNMPSelectionFormulasOrderIsStable：Formulas() 的顺序必须是 nginx → PHP → MySQL。
//
// 顺序有意义（nginx 是入口，先装它；PHP 端点闭环要在注册守护进程之前），
// 而它由 lnmpComponents 的顺序生成 —— 有人调整那个切片就会静默改变安装顺序。
func TestLNMPSelectionFormulasOrderIsStable(t *testing.T) {
	sel := LNMPSelection{Nginx: "nginx", PHP: "php@8.2", MySQL: "mysql@8.4"}
	got := sel.Formulas()
	if len(got) != 3 || got[0] != "nginx" || got[1] != "php@8.2" || got[2] != "mysql@8.4" {
		t.Fatalf("Formulas() 顺序应为 [nginx php@8.2 mysql@8.4]，实际 %v", got)
	}
	// 组件白名单的顺序也一并钉住
	keys := []string{}
	for _, c := range lnmpComponents {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, ",") != "nginx,php,mysql" {
		t.Errorf("lnmpComponents 的顺序变了（会改变安装顺序，也改变候选分组顺序）：%v", keys)
	}
}

// TestCompareFormulaVersion：候选排序不能把 8.10 排到 8.9 前面（字典序陷阱）。
func TestCompareFormulaVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"php@8.4", "php@8.2", 1},
		{"php@8.2", "php@8.4", -1},
		{"php@8.2", "php@8.2", 0},
		{"php@8.10", "php@8.9", 1}, // 字典序会判反
		{"php@8.2", "php@8.2.1", -1},
		{"nginx", "nginx", 0},
	}
	for _, c := range cases {
		got := compareFormulaVersion(c.a, c.b)
		if (got > 0) != (c.want > 0) || (got < 0) != (c.want < 0) {
			t.Errorf("compareFormulaVersion(%s,%s) = %d，期望符号 %d", c.a, c.b, got, c.want)
		}
	}
}

// TestLNMPOptionsGroupOptionsAreSortedStable：候选顺序要稳定（前端不做二次排序）。
//
// 排序规则本身是生产代码 sortLNMPOptions，测试直接调它 —— 两处各写一份比较器必然漂。
func TestLNMPOptionsGroupOptionsAreSortedStable(t *testing.T) {
	byKey := map[string]string{}
	for _, g := range lnmpOptionsFromCatalog() {
		got := []string{}
		for _, o := range g.Options {
			got = append(got, o.Formula)
		}
		byKey[g.Key] = strings.Join(got, ",")
		again := append([]lnmpOption{}, g.Options...)
		sortLNMPOptions(again)
		againFormulas := []string{}
		for _, o := range again {
			againFormulas = append(againFormulas, o.Formula)
		}
		if strings.Join(againFormulas, ",") != strings.Join(got, ",") {
			t.Errorf("「%s」组的候选顺序不稳定：%v vs %v", g.Label, got, againFormulas)
		}
	}
	if got := byKey["php"]; got != "php@8.4,php@8.2" {
		t.Errorf("PHP 候选应新版在前，实际 %v", got)
	}
	// 数据库位有两个引擎：默认项 mysql@8.4 必须在前（跨引擎不比版本号）。
	if got := byKey["mysql"]; got != "mysql@8.4,mariadb" {
		t.Errorf("数据库候选应默认项在前（mysql@8.4,mariadb），实际 %v", got)
	}
}

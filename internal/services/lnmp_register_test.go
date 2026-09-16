package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  一键 LNMP 的"回流"：装完必须登记进面板自己的服务注册表
//
//  真机反馈：一键 LNMP 跑完，日志里 nginx/PHP/MySQL 都装好了，但
//    · 「服务管理」里看不到它们；
//    · 应用市场里这几条没有变成"已安装（已纳管）"。
//  原因就是 InstallLNMP 只做了 launchd 层面的注册（installSystemDaemons），
//  从不写面板的 services 表。这一组测试锁住"必须登记"、"幂等"与"失败如实报"。
// ============================================================================

// writeAgentPlist 造出"这个服务已装在本机"的现场：
// ~/Library/LaunchAgents/<label>.plist。
//
// 为什么用 LaunchAgents 而不是 /Library/LaunchDaemons：单测不许碰真实系统目录。
// m.opt.UserHome 已被 sandboxManager 指到 t.TempDir()，所以这里写的是一份
// 完全隔离的假现场，AdoptCandidate/BrewLabelFor 走的就是真实代码路径。
func writeAgentPlist(t *testing.T, home, label string) {
	t.Helper()
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + label + `</string>
	<key>ProgramArguments</key>
	<array><string>/usr/bin/true</string></array>
	<key>StandardOutPath</key>
	<string>/tmp/` + label + `.log</string>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(dir, label+".plist"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRegisterLNMPComponentsRegistersAllThree 是这一轮的核心断言。
//
// 故意让三个服务用**三种不同前缀**的标签（真机就是这样混着的）：
// `sh.brew.nginx` 连目录里写的 `homebrew.mxcl.nginx` 都不是。
// 登记必须按磁盘上的真实标签走，否则记录会指向一个不存在的 plist ——
// 服务管理里状态永远查不到，用户看到的还是"什么都没有"。
func TestRegisterLNMPComponentsRegistersAllThree(t *testing.T) {
	m, repo := sandboxManager(t)
	ctx := context.Background()

	// formula → 本机真实标签（与目录里写死的 ServiceLabel 不完全一致）
	// 注意与 LNMPFormulas 保持一致：默认 PHP 已是 8.2（用户 2026-09-17 要求），
	// 用 8.3 会让这条测试锁住一个过期的默认值。
	wantLabel := map[string]string{
		"nginx":     "sh.brew.nginx",           // 目录写的是 homebrew.mxcl.nginx
		"php@8.2":   "sh.brew.php@8.2",         // 目录写的是 homebrew.mxcl.php@8.2
		"mysql@8.4": "homebrew.mxcl.mysql@8.4", // 目录写的是 sh.brew.mysql@8.4
	}
	for _, label := range wantLabel {
		writeAgentPlist(t, m.opt.UserHome, label)
	}

	result := &InstallResult{}
	m.registerLNMPComponents(ctx, result)

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("一键 LNMP 装完应登记 3 个服务，实际 %d 个（用户会在服务管理里看不到它们）", len(list))
	}
	byLabel := map[string]*Service{}
	for _, s := range list {
		byLabel[s.LaunchLabel] = s
	}
	for formula, label := range wantLabel {
		s := byLabel[label]
		if s == nil {
			t.Errorf("%s 没按本机真实标签 %s 登记：记录会指向不存在的 plist", formula, label)
			continue
		}
		if s.Category != "lnmp" {
			t.Errorf("%s 的分类应为 lnmp，实际 %q（服务管理里会归错组）", formula, s.Category)
		}
		if want := LNMPPorts[formula]; s.Port != want {
			t.Errorf("%s 的端口应为 %d，实际 %d", formula, want, s.Port)
		}
	}
	// nginx 的健康检查地址必须被补上：目录里 HealthPath="/"，
	// 只按精确 label 匹配的话（sh.brew.nginx 不匹配目录里的 homebrew.mxcl.nginx）
	// 这里会是空 —— 界面又变成"未配置"，正是历史上反复踩的那个坑。
	if nginx := byLabel["sh.brew.nginx"]; nginx != nil {
		if nginx.HealthURL != "http://127.0.0.1:80/" {
			t.Errorf("nginx 健康地址应为 http://127.0.0.1:80/，实际 %q", nginx.HealthURL)
		}
		if nginx.DisplayName == "" || nginx.DisplayName == nginx.LaunchLabel {
			t.Errorf("nginx 应显示软件名而不是 launchd 标签，实际 %q", nginx.DisplayName)
		}
	}
	// 全程不该谎报失败，也不该留下警告
	for _, msg := range result.Steps {
		if strings.Contains(msg, "登记失败") {
			t.Errorf("登记本应成功却报失败：%s", msg)
		}
	}
	if result.Warning != "" {
		t.Errorf("登记成功时不该有警告：%s", result.Warning)
	}

	// ---- 幂等：再跑一次一键安装不能出现重复条目 ----
	again := &InstallResult{}
	m.registerLNMPComponents(ctx, again)
	list2, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list2) != 3 {
		t.Fatalf("重复执行一键安装产生了重复记录：%d 条（RegisterInstalledService 应幂等）", len(list2))
	}
	for _, msg := range again.Steps {
		if strings.Contains(msg, "登记失败") {
			t.Errorf("幂等重跑不该报登记失败：%s", msg)
		}
	}
}

// TestRegisterLNMPComponentsReportsFailureHonestly 锁住"失败不吞、不谎报"。
//
// 造一个必然失败的现场：nginx 的 plist 叫 `com.apple.nginx`（系统标签，
// AdoptCandidate 明确拒绝纳管）。此时：
//
//	· 不能返回"已登记"；
//	· 失败原因必须出现在 Steps 与 Warning 里，并告诉用户怎么补登记；
//	· 注册表里不能多出一条假记录。
//
// 只留 nginx 一个 formula，避免另外两个走 launchctl 探测（那会依赖真机状态）。
func TestRegisterLNMPComponentsReportsFailureHonestly(t *testing.T) {
	m, repo := sandboxManager(t)
	ctx := context.Background()

	writeAgentPlist(t, m.opt.UserHome, "com.apple.nginx") // isSystemLabel → 拒绝

	orig := LNMPFormulas
	LNMPFormulas = []string{"nginx"}
	defer func() { LNMPFormulas = orig }()

	result := &InstallResult{}
	m.registerLNMPComponents(ctx, result)

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("登记失败却写进了 %d 条记录（谎报成功）", len(list))
	}
	joined := strings.Join(result.Steps, "\n")
	if !strings.Contains(joined, "登记失败") || !strings.Contains(joined, "nginx") {
		t.Errorf("失败必须在 Steps 里写清是哪个组件、为什么，实际：%s", joined)
	}
	if !strings.Contains(result.Warning, "登记失败") {
		t.Errorf("失败必须进 Warning，否则用户看摘要会以为全都成功了，实际 Warning=%q", result.Warning)
	}
	if strings.Contains(joined, "已登记进「服务管理」") {
		t.Errorf("失败时不能出现成功字样：%s", joined)
	}
}

// TestRegisterLNMPComponentsMissingLaunchdDefinition：
// 服务不在 launchd 里（连候选标签都没有）时也要如实报，不许静默跳过。
func TestRegisterLNMPComponentsMissingLaunchdDefinition(t *testing.T) {
	m, repo := sandboxManager(t)
	ctx := context.Background()

	// 这个 formula 在目录里不存在，磁盘上也不可能有对应 plist —— 全隔离，
	// 不依赖真机上装没装东西。
	orig := LNMPFormulas
	LNMPFormulas = []string{"zizpanel-test-nope"}
	defer func() { LNMPFormulas = orig }()

	result := &InstallResult{}
	m.registerLNMPComponents(ctx, result)

	if list, err := repo.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("没有 launchd 定义却登记出了记录：%v / %d 条", err, len(list))
	}
	if !strings.Contains(strings.Join(result.Steps, "\n"), "登记失败") {
		t.Errorf("找不到 launchd 定义必须如实报失败，实际 Steps=%v", result.Steps)
	}
	if result.Warning == "" {
		t.Error("失败必须同时进 Warning")
	}
}

// TestCatalogEntryForLabelFallsBackToBrewFormula：
// 目录条目只写一种 ServiceLabel，但磁盘上前缀是混的。
// 必须能按 formula 后缀反查，否则按真实标签登记/纳管的服务会丢掉
// 端口、分类与健康地址（界面显示"未配置"）。
func TestCatalogEntryForLabelFallsBackToBrewFormula(t *testing.T) {
	cases := map[string]string{
		// 精确匹配照旧
		"homebrew.mxcl.nginx": "nginx",
		"sh.brew.mysql@8.4":   "mysql84",
		// 另一套前缀
		"homebrew.mxcl.php@8.3": "php83", // 8.3 条目保留：用户仍可自装
		"sh.brew.php@8.2":       "php82", // 默认版本

		// 系统级改造后的自研前缀（本机 nginx 真的长这样）
		"cn.zizdog.nginx":       "nginx",
		"com.example.mysql@8.4": "mysql84",
	}
	for label, wantID := range cases {
		app, ok := catalogEntryForLabel(label)
		if !ok {
			t.Errorf("%s 应按 formula 反查到目录条目", label)
			continue
		}
		if app.ID != wantID {
			t.Errorf("%s 应命中 %s，实际 %s", label, wantID, app.ID)
		}
	}
	// 不认识的一律不猜（猜错比不猜更糟）
	for _, label := range []string{"", "com.apple.something", "sh.brew.nope"} {
		if app, ok := catalogEntryForLabel(label); ok {
			t.Errorf("%q 不该命中任何目录条目，实际 %s", label, app.ID)
		}
	}
}

// TestAppendLNMPWarning 警告是追加而不是覆盖 —— 否则登记失败会被
// 后面 phpMyAdmin/端口验证那几处赋值抹掉。
func TestAppendLNMPWarning(t *testing.T) {
	if got := appendLNMPWarning("", "第一次失败"); got != "第一次失败" {
		t.Errorf("空警告应原样填入，实际 %q", got)
	}
	got := appendLNMPWarning("第一次失败", "第二次失败")
	if !strings.Contains(got, "第一次失败") || !strings.Contains(got, "第二次失败") {
		t.Errorf("追加警告把前面的内容弄丢了：%q", got)
	}
}

// TestInstallLNMPCallsRegistrationOutsideRunningBranch 锁住**调用位置**。
//
// registerLNMPComponents 本身的行为上面已经测了，但如果它被挪进
// `else`（也就是"服务没在跑"那个分支）里，或者被挪到端口验证之后，
// 真机上"LNMP 早就跑着"的机器就永远登记不上 —— 而这正是用户反馈的场景。
// InstallLNMP 没法直接跑单测（它第一件事就要求 root），所以按本项目
// homebrew_clt_mirror_test.go 的做法，直接读源码锁住结构。
func TestInstallLNMPCallsRegistrationOutsideRunningBranch(t *testing.T) {
	b, err := os.ReadFile("lnmp.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	start := strings.Index(src, "func (m *Manager) InstallLNMP(")
	end := strings.Index(src, "\n// fixNginxBaseConfig")
	if start < 0 || end < 0 || end < start {
		t.Fatal("在 lnmp.go 里找不到 InstallLNMP 函数体")
	}
	body := src[start:end]

	iIf := strings.Index(body, "if m.allLNMPRunning(ctx) {")
	if iIf < 0 {
		t.Fatal("InstallLNMP 里应保留 allLNMPRunning 的跳过分支")
	}
	// 跳过分支 = if 块 + else 块。定位 else 块的收尾花括号时只匹配行首单 tab 的
	// `\n\t}`：嵌套的 if 是两个 tab，不会被误命中。
	rel := body[iIf:]
	iElse := strings.Index(rel, "} else {")
	if iElse < 0 {
		t.Fatal("allLNMPRunning 分支结构变了，无法判断登记调到哪去了")
	}
	iElseClose := strings.Index(rel[iElse:], "\n\t}")
	if iElseClose < 0 {
		t.Fatal("找不到 allLNMPRunning 的 else 块结尾")
	}
	branchEnd := iIf + iElse + iElseClose

	iReg := strings.Index(body, "m.registerLNMPComponents(ctx, result)")
	if iReg < 0 {
		t.Fatal("InstallLNMP 必须在收尾阶段调用 m.registerLNMPComponents(ctx, result)：" +
			"否则一键装完服务管理里看不到 nginx/PHP/MySQL")
	}
	if iReg < branchEnd {
		t.Error("登记调用在 allLNMPRunning 的 if/else 里面（或之前）—— " +
			"必须放在整个分支之后，否则「服务已经在跑」的机器永远登记不上")
	}
	// 还必须在端口验证之前：验证失败会提前 return，登记不能被它跳过。
	iVerify := strings.Index(body, `result.step(ctx, "正在验证服务是否真的可用")`)
	if iVerify < 0 {
		t.Fatal("找不到端口验证步骤（这段结构变了，请同步更新本测试）")
	}
	if iReg > iVerify {
		t.Error("登记必须在端口验证之前：验证不通过会提前 return，登记会被跳过")
	}
}

// TestPHPVersionFromFormula 锁住"只有 php@x.y 才触发端点闭环"。
//
// 为什么要测：这个判定决定"装完 PHP 自动分配专属端点"会不会被触发。
// 认错（比如把无版本后缀的 `php` 也认了）会拿一个漂移的版本号去改
// www.conf —— 那是改用户的 php-fpm 配置，不能猜。
func TestPHPVersionFromFormula(t *testing.T) {
	cases := []struct {
		formula string
		want    string
		ok      bool
	}{
		{"php@8.4", "8.4", true},
		{"php@8.1", "8.1", true},
		{"php", "", false}, // 无版本后缀：实际版本随 Homebrew 漂移，不猜
		{"php@", "", false},
		{"nginx", "", false},
		{"mysql@8.4", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := phpVersionFromFormula(c.formula)
		if got != c.want || ok != c.ok {
			t.Errorf("phpVersionFromFormula(%q) = (%q,%v)，期望 (%q,%v)",
				c.formula, got, ok, c.want, c.ok)
		}
	}
}

// TestAppendWarningKeepsAll 锁住"多条告警不被后续覆盖"。
//
// 真机教训：result.Warning 是单字符串，多步都写它；直接赋值会让前一条
// 被后一条悄悄吞掉（例如"PHP 端点配置失败"被"phpMyAdmin 失败"覆盖）。
func TestAppendWarningKeepsAll(t *testing.T) {
	if got := appendWarning("", "第一条"); got != "第一条" {
		t.Errorf("空值时应直接返回，实际 %q", got)
	}
	got := appendWarning("第一条", "第二条")
	if !strings.Contains(got, "第一条") || !strings.Contains(got, "第二条") {
		t.Errorf("两条都该保留，实际 %q", got)
	}
}

// TestRegisterLNMPComponentsRepairsAfterPlistAppears 锁住"重跑一次能修好"。
//
// 用户机器上最典型的状态就是"包装在 Cellar 里、launchd 里没有它"：
// 第一次一键安装时登记必然失败（找不到 launchd 定义）；等系统级服务注册好了
// （installSystemDaemons 会把 plist 装进 /Library/LaunchDaemons），
// **重跑一次**必须能把记录补齐 —— 这就是"装了一半的机器能修好"的判据。
// 前半段同时锁住"失败如实报"：不许假装登记成功。
//
// 为什么用合成的 formula 名而不是 nginx/php@8.2：开发机（本机）launchd 里
// **真的**加载着 nginx，`AdoptCandidate` 允许"没有 plist 但 launchd 里有"的
// 纳管，于是"第一次必然失败"这个前提在真机上不成立 —— 测试会随机器状态飘。
// 这里要锁的是"先失败、补上 plist 后重跑成功"这个流程，用合成名字才确定。
func TestRegisterLNMPComponentsRepairsAfterPlistAppears(t *testing.T) {
	m, repo := sandboxManager(t)
	ctx := context.Background()

	orig := LNMPFormulas
	LNMPFormulas = []string{"zizpanel-test-nginx", "zizpanel-test-php@8.2", "zizpanel-test-mysql@8.4"}
	defer func() { LNMPFormulas = orig }()

	// 第一次：launchd 里什么都没有（模拟"装了一半"）
	first := &InstallResult{}
	m.registerLNMPComponents(ctx, first)
	if list, err := repo.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("launchd 里没有定义时不该登记出记录：%v / %d 条", err, len(list))
	}
	if !strings.Contains(strings.Join(first.Steps, "\n"), "登记失败") {
		t.Errorf("找不到 launchd 定义必须如实报失败，实际 Steps=%v", first.Steps)
	}
	if first.Warning == "" {
		t.Error("失败必须同时进 Warning（否则摘要里看起来像成功）")
	}

	// 模拟"系统级服务已经注册好"：三个 plist 出现
	for _, f := range LNMPFormulas {
		writeAgentPlist(t, m.opt.UserHome, "sh.brew."+f)
	}

	second := &InstallResult{}
	m.registerLNMPComponents(ctx, second)
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("重跑一次应把三个组件都补齐，实际 %d 条：%v", len(list), list)
	}
	if strings.Contains(strings.Join(second.Steps, "\n"), "登记失败") {
		t.Errorf("plist 已经齐了，重跑不该再报登记失败：%v", second.Steps)
	}
	if second.Warning != "" {
		t.Errorf("重跑成功时不该留告警：%s", second.Warning)
	}

	// 再跑第三次：仍然 3 条（幂等，不产生重复记录）
	m.registerLNMPComponents(ctx, &InstallResult{})
	if list3, err := repo.List(ctx); err != nil || len(list3) != 3 {
		t.Fatalf("重复登记应幂等（仍是 3 条），实际 %v / %d 条", err, len(list3))
	}
}

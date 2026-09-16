package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  phpMyAdmin 入口的三份生成器合并成一份（D14 / D10）
//
//  背景：这里曾经有三份各自手写的入口生成器（services 的最小默认站点、
//  services 的"插入已有默认站点"、internal/web 的完整默认站点），而且已经漂移 ——
//  insert 版既没有 allow/deny 也没有 301，于是"默认站点不是面板创建的"这条路径上
//  phpMyAdmin 对局域网/全网敞开。
//
//  这些测试锁死：安全指令只有一份、三处产出必须原样嵌入它、insert 版必须带守卫。
// ============================================================================

// TestPMAEntryBlockAlwaysRestrictsAccess：唯一生成器在**任何**输入下都必须带访问限制。
func TestPMAEntryBlockAlwaysRestrictsAccess(t *testing.T) {
	share := "/opt/homebrew/share/phpmyadmin"
	cases := []struct {
		name string
		pass string
	}{
		{"有端点", "unix:/opt/homebrew/var/run/php-fpm-8.2.sock"},
		{"没有端点", ""},
	}
	for _, c := range cases {
		block := PMAEntryBlock(PMAEntryOptions{Share: share, FastCGIPass: c.pass, PHPVersion: "8.2"})
		for _, want := range []string{
			"allow 127.0.0.1;",
			"allow ::1;",
			"deny all;",
			"location = /phpmyadmin",
			"return 301 /phpmyadmin/;",
			pmaMarker,
			"alias " + share + ";",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("%s：入口块缺少安全/定位指令 %q：\n%s", c.name, want, block)
			}
		}
		if strings.Count(block, "{") != strings.Count(block, "}") {
			t.Errorf("%s：花括号不配对：\n%s", c.name, block)
		}
		if c.pass == "" {
			// 硬性要求 1：没有可用端点时绝不写 `fastcgi_pass ;`（非法配置会把整个 nginx 拖挂）。
			if strings.Contains(block, "fastcgi_pass") {
				t.Errorf("没有 PHP 端点时不能写 fastcgi_pass：\n%s", block)
			}
			if !strings.Contains(block, "没有可用 PHP 端点") {
				t.Errorf("没有 PHP 端点时应退化成一行说明：\n%s", block)
			}
		} else if !strings.Contains(block, "fastcgi_pass "+c.pass+";") {
			t.Errorf("有端点时必须写显式端点：\n%s", block)
		}
	}
}

// TestPMAEntryGeneratorsAllUseSharedBlock：三份生成器必须原样嵌入同一份入口块。
//
// 锁死 D14（合并成一份）与 D10（三份都带卸载标记）。
func TestPMAEntryGeneratorsAllUseSharedBlock(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	share := m.pmaPaths().Share
	pass := "unix:" + filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	want := PMAEntryBlock(PMAEntryOptions{Share: share, FastCGIPass: pass, PHPVersion: "8.2"})

	// 生成器 1：插入已有默认站点
	insert := pmaVhostInsertBlock(share, pass, "8.2")
	if normalizeNginxBlock(insert) != normalizeNginxBlock(want) {
		t.Errorf("insert 版与统一生成器不一致：\n--- insert ---\n%s\n--- want ---\n%s", insert, want)
	}
	// 这一条直接锁 D14：insert 版**必须**含 allow/deny 与 301。
	for _, d := range []string{"allow 127.0.0.1;", "allow ::1;", "deny all;", "return 301 /phpmyadmin/;"} {
		if !strings.Contains(insert, d) {
			t.Errorf("insert 版缺少 %q（D14 的暴露面）：\n%s", d, insert)
		}
	}

	// 生成器 2：一键 LNMP 收尾的最小默认站点
	www := filepath.Join(prefix, "home", "www")
	vhost := m.pmaDefaultVhostContent(www, filepath.Join(www, "localhost"), pass, "8.2")
	if !strings.Contains(vhost, want) {
		t.Errorf("最小默认站点没有原样嵌入统一生成器：\n--- vhost ---\n%s\n--- want ---\n%s", vhost, want)
	}

	// 安全相关指令集合一致（逐条比对出现次数，避免"某一份多一条/少一条"）。
	for _, d := range []string{"allow 127.0.0.1;", "allow ::1;", "deny all;", "return 301 /phpmyadmin/;"} {
		if a, b := strings.Count(insert, d), strings.Count(vhost, d); a != b {
			t.Errorf("insert 版与最小默认站点的 %q 数量不一致：%d vs %d", d, a, b)
		}
		if strings.Count(vhost, d) == 0 {
			t.Errorf("最小默认站点缺少安全指令 %q：\n%s", d, vhost)
		}
	}
}

// ============================================================================
//  D44：默认站点标记统一 + 兼容两个旧值
// ============================================================================

// TestDefaultVhostMarkerCompatibility：判定"是不是面板创建的"时两个旧值都认，
// 写盘时写新的统一值。
func TestDefaultVhostMarkerCompatibility(t *testing.T) {
	newMinimal := DefaultVhostMarker + "\n" + defaultVhostKindMinimal + "\nserver {}\n"
	newFull := DefaultVhostMarker + "\n" + DefaultVhostKindFull + "\nserver {}\n"
	// 旧版 internal/web 的头注释正好等于新的统一值 —— 天然向后兼容。
	oldWeb := "# 默认站点（由 ZizPanel 生成 —— 请勿手工编辑，面板会整份重写）\nserver {}\n"
	oldServices := "# 由 ZizPanel 创建：默认站点（一键 LNMP 的收尾）\nserver {}\n"
	userFile := "server {\n    listen 80 default_server;\n    server_name mysite;\n}\n"

	for _, ours := range []string{newMinimal, newFull, oldWeb, oldServices} {
		if !IsZizPanelDefaultVhost(ours) {
			t.Errorf("必须被认成面板创建的（新旧标记都要认）：\n%s", ours)
		}
	}
	if IsZizPanelDefaultVhost(userFile) {
		t.Error("用户自己的默认站点不能被认成面板创建的")
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"新最小模板", newMinimal, "upgrade"},
		{"旧 services 标记", oldServices, "upgrade"},
		{"完整模板（新）", newFull, "keep"},      // 含应用代理，最小模板这条路径绝不能盖它
		{"完整模板（旧，认不出种类）", oldWeb, "keep"}, // 保守：不碰
		{"用户的文件", userFile, "keep"},
	}
	for _, c := range cases {
		if got := defaultVhostAction(c.in); got != c.want {
			t.Errorf("%s：defaultVhostAction = %q，期望 %q", c.name, got, c.want)
		}
	}

	// 写的时候必须写新的统一值（而不是旧 services 标记）。
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	www := filepath.Join(prefix, "home", "www")
	pass := "unix:" + filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	vhost := m.pmaDefaultVhostContent(www, filepath.Join(www, "localhost"), pass, "8.2")
	if !strings.HasPrefix(vhost, DefaultVhostMarker) {
		t.Errorf("最小默认站点必须写统一的新标记：\n%s", vhost)
	}
	if !strings.Contains(vhost, defaultVhostKindMinimal) {
		t.Errorf("最小默认站点必须带种类标记（否则被当成完整模板）：\n%s", vhost)
	}
	if strings.Contains(vhost, defaultVhostMarkerLegacy) {
		t.Errorf("不该再写旧标记：\n%s", vhost)
	}
}

// ============================================================================
//  D30：跳过前要比对内容，而不是"含 /phpmyadmin 字样就跳过"
// ============================================================================

// TestPMAVhostPlanComparesContent：三种输入必须得到三种正确动作。
func TestPMAVhostPlanComparesContent(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	share := m.pmaPaths().Share
	pass := "unix:" + filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	want := PMAEntryBlock(PMAEntryOptions{Share: share, FastCGIPass: pass, PHPVersion: "8.2"})

	// 头注释里带 "/phpmyadmin/" 字样（完整模板就是这样），但它是注释、不是入口：
	// 不能被当成"已经有入口"或"认不出的入口"。
	base := "# 用户的默认站点\n#   · /phpmyadmin/  **只允许 127.0.0.1**\nserver {\n" +
		"    listen 80 default_server;\n    location / {\n        try_files $uri $uri/ =404;\n    }\n}\n"

	// 1) 没有入口 → 插入
	action, out, err := pmaVhostPlan(base, want, share)
	if err != nil {
		t.Fatalf("插入规划报错: %v", err)
	}
	if action != pmaVhostInsert {
		t.Errorf("没有入口时应为 insert，实际 %q", action)
	}
	if !strings.Contains(out, want) || strings.Count(out, "location ^~ /phpmyadmin") != 1 {
		t.Errorf("插入结果不正确：\n%s", out)
	}

	// 2) 已有正确入口 → keep，且一个字节都不改
	okText, err := insertPMABlock(base, want)
	if err != nil {
		t.Fatal(err)
	}
	action, out, err = pmaVhostPlan(okText, want, share)
	if err != nil {
		t.Fatalf("已就绪时规划报错: %v", err)
	}
	if action != pmaVhostKeep || out != okText {
		t.Errorf("内容一致时应 keep 且不改写，实际 action=%q changed=%v", action, out != okText)
	}

	// 3) 旧 insert 版（含 /phpmyadmin 但没有 allow/deny/301）→ 必须原地替换
	legacyBlock := "\n    " + pmaMarker + "\n    location ^~ /phpmyadmin {\n" +
		"        alias " + share + ";\n        index index.php;\n    }\n"
	legacy := strings.Replace(okText, "\n"+want, legacyBlock, 1)
	if strings.Contains(legacy, "deny all;") {
		t.Fatal("测试构造失败：legacy 不该含 deny all")
	}
	action, out, err = pmaVhostPlan(legacy, want, share)
	if err != nil {
		t.Fatalf("替换旧入口时报错: %v", err)
	}
	if action != pmaVhostReplace {
		t.Errorf("旧入口（没有访问限制）必须被替换，实际 %q", action)
	}
	for _, d := range []string{"allow 127.0.0.1;", "deny all;", "return 301 /phpmyadmin/;"} {
		if !strings.Contains(out, d) {
			t.Errorf("替换后的入口缺少 %q：\n%s", d, out)
		}
	}
	if n := strings.Count(out, "location ^~ /phpmyadmin"); n != 1 {
		t.Errorf("替换后不能出现重复 location，实际 %d 处：\n%s", n, out)
	}
	if strings.Count(out, "{") != strings.Count(out, "}") {
		t.Errorf("花括号不配对：\n%s", out)
	}
	if !strings.Contains(out, "location / {") {
		t.Error("替换不能误删用户的其它 location")
	}

	// 4) 认不出的 /phpmyadmin → 必须报错（再插一份 = duplicate location → nginx 拒绝加载）
	foreign := strings.Replace(base, "    location / {",
		"    location /phpmyadmin {\n        alias /somewhere/else;\n    }\n    location / {", 1)
	if _, _, err := pmaVhostPlan(foreign, want, share); err == nil {
		t.Error("遇到认不出的 /phpmyadmin 入口必须报错，不能静默插第二份")
	}
}

// ============================================================================
//  D10：卸载时找不到要删的块要如实报告，不能静默跳过然后报成功
// ============================================================================

func TestPlanPMAEntryRemoval(t *testing.T) {
	share := "/opt/homebrew/share/phpmyadmin"
	block := PMAEntryBlock(PMAEntryOptions{Share: share, FastCGIPass: "unix:/x.sock", PHPVersion: "8.2"})
	// 注意这段头注释里就写着 "/phpmyadmin/" 字样：它不是入口，不能被当成残留
	// （完整默认站点的模板里就有这么一行）。
	body := "server {\n    #   · /phpmyadmin/  **只允许 127.0.0.1**：面板登录后反代过来才能用\n" +
		"    listen 80;\n    location / {\n        try_files $uri $uri/ =404;\n    }\n"

	// 1) 新格式：带标记 + 301 + 主体，必须整段摘干净
	newFmt := body + "\n" + block + "}\n"
	action, out, err := planPMAEntryRemoval(newFmt, share)
	if err != nil {
		t.Fatalf("新格式移除报错: %v", err)
	}
	if action != pmaRemovalRemove {
		t.Errorf("新格式应可移除，实际 %q", action)
	}
	if hasPMAEntryLine(out) {
		t.Errorf("phpMyAdmin 入口没被摘干净：\n%s", out)
	}
	if !strings.Contains(out, "location / {") {
		t.Error("别的 location 被误删了")
	}
	if !strings.Contains(out, "/phpmyadmin/") {
		t.Error("头注释里的 /phpmyadmin/ 不该被一起删掉")
	}
	if strings.Count(out, "{") != strings.Count(out, "}") {
		t.Errorf("花括号不配对：\n%s", out)
	}

	// 2) 旧格式（没有标记，alias 指向 phpMyAdmin 目录）也必须能识别并移除
	legacy := body + "    location = /phpmyadmin {\n        return 301 /phpmyadmin/;\n    }\n" +
		"    location ^~ /phpmyadmin {\n        allow 127.0.0.1;\n        alias " + share + ";\n        index index.php;\n    }\n}\n"
	action, out, err = planPMAEntryRemoval(legacy, share)
	if err != nil {
		t.Fatalf("旧格式移除报错: %v", err)
	}
	if action != pmaRemovalRemove {
		t.Errorf("旧格式（无标记）应能按 alias 识别并移除，实际 %q", action)
	}
	if hasPMAEntryLine(out) {
		t.Errorf("旧格式入口没被摘干净：\n%s", out)
	}

	// 3) 认不出的 /phpmyadmin：必须报 unmanaged，由调用方如实报错
	foreign := body + "    location /phpmyadmin {\n        alias /somewhere/else;\n    }\n}\n"
	action, _, err = planPMAEntryRemoval(foreign, share)
	if err != nil {
		t.Fatalf("认不出的入口不该在这里就报错（由调用方带路径报错）: %v", err)
	}
	if action != pmaRemovalUnmanaged {
		t.Errorf("认不出的 /phpmyadmin 必须报 unmanaged（可能有残留），实际 %q", action)
	}

	// 4) 完全没有入口 → none（这才是真正的"无需移除"）
	action, _, err = planPMAEntryRemoval(body+"}\n", share)
	if err != nil {
		t.Fatal(err)
	}
	if action != pmaRemovalNone {
		t.Errorf("没有入口时应为 none，实际 %q", action)
	}
}

// ============================================================================
//  D43：AllowNoPassword 那一行的幂等改写
// ============================================================================

func TestPMAAllowNoPasswordLine(t *testing.T) {
	conf := pmaConfig("0123456789abcdef0123456789abcdef", "/tmp/pma", true)
	if got, ok := pmaAllowNoPasswordFromConfig(conf); !ok || !got {
		t.Fatalf("生成的配置应含 AllowNoPassword=true，实际 ok=%v got=%v", ok, got)
	}

	off := setPMAAllowNoPassword(conf, false)
	if got, ok := pmaAllowNoPasswordFromConfig(off); !ok || got {
		t.Errorf("改写后应为 false，实际 ok=%v got=%v", ok, got)
	}
	if !strings.Contains(off, "$cfg['Servers'][$i]['AllowNoPassword'] = false;") {
		t.Errorf("改写必须只动那一行的值：\n%s", off)
	}
	// 幂等：再改一次字节完全一致
	if again := setPMAAllowNoPassword(off, false); again != off {
		t.Error("幂等失败：同一个值改写两次结果不一致")
	}
	// 其它行不能被动
	if strings.Count(off, "blowfish_secret") != 1 ||
		!strings.Contains(off, "$cfg['Servers'][$i]['auth_type']       = 'cookie';") {
		t.Errorf("改写影响了别的配置行：\n%s", off)
	}

	// 找不到那一行时不许凭空插入，也不许猜
	junk := "<?php\n$i = 0;\n"
	if _, ok := pmaAllowNoPasswordFromConfig(junk); ok {
		t.Error("没有那一行时必须返回 ok=false")
	}
	if got := setPMAAllowNoPassword(junk, true); got != junk {
		t.Errorf("没有那一行时必须原样返回，实际：%q", got)
	}
}

// TestMySQLRootPasswordUnknownWithoutClient：判不出口令状态时不许猜。
func TestMySQLRootPasswordUnknownWithoutClient(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix) // 沙箱前缀下没有 bin/mysql

	if got := m.mysqlRootPasswordState(context.Background()); got != mysqlRootPasswordUnknown {
		t.Errorf("没有 mysql 客户端时应为 unknown，实际 %v", got)
	}
	if m.mysqlRootHasNoPassword(context.Background()) {
		t.Error("判不出来时不能当成空口令（会写出与实际不符的 AllowNoPassword）")
	}
}

// ============================================================================
//  沙箱护栏：这批纯生成器测试绝不碰生产 nginx 配置
// ============================================================================

func TestPMAEntryGeneratorsDoNotTouchProductionVhost(t *testing.T) {
	const prod = "/opt/homebrew/etc/nginx/vhosts/000-default.conf"
	before, beforeErr := os.ReadFile(prod)

	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	if strings.HasPrefix(m.brewPrefix(), "/opt/homebrew") || strings.HasPrefix(m.brewPrefix(), "/usr/local") {
		t.Fatalf("测试 Manager 的 brew 前缀指向真实环境：%s", m.brewPrefix())
	}
	www := filepath.Join(prefix, "home", "www")
	pass := "unix:" + filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	_ = m.pmaDefaultVhostContent(www, filepath.Join(www, "localhost"), pass, "8.2")
	_ = pmaVhostInsertBlock(m.pmaPaths().Share, pass, "8.2")
	_ = PMAEntryBlock(PMAEntryOptions{Share: m.pmaPaths().Share, FastCGIPass: pass, PHPVersion: "8.2"})

	after, afterErr := os.ReadFile(prod)
	if beforeErr != nil || afterErr != nil {
		if os.IsNotExist(beforeErr) && os.IsNotExist(afterErr) {
			t.Skip("本机没有生产 000-default.conf，跳过内容比对")
		}
		t.Fatalf("读取生产 vhost 失败：before=%v after=%v", beforeErr, afterErr)
	}
	if string(before) != string(after) {
		t.Fatal("生产 000-default.conf 被测试改动了！这正是 2026-09-14 那次事故的形态")
	}
}

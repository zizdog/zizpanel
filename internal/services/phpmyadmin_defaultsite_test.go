package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  默认站点（000-default.conf）缺失时的补齐 —— 真机 2026-09-17 mini
//
//  现场：一键 LNMP 装完 phpMyAdmin 打不开（`/phpmyadmin/` 404），因为
//  <brew>/etc/nginx/vhosts/000-default.conf **根本不存在** —— phpMyAdmin 的
//  入口是"默认站点里的一个 location"，没有默认站点就没处挂它。
//
//  这里锁两条：
//    1. 已存在 → **一个字都不改**（这台机器上有正在用的 LNMP，绝不覆盖）；
//    2. 生成的内容必须是最小但可用的（default_server + phpMyAdmin 受控入口）。
// ============================================================================

// TestEnsureDefaultVhostLeavesExistingFileUntouched 是防"把用户默认站点改坏"的护栏。
//
// 本机（MacBook Air）的 000-default.conf 是正在用的：里面有应用代理、
// phpMyAdmin 入口与日志路径。任何"顺手重写"都会把它弄坏，所以这里逐字节断言不变。
func TestEnsureDefaultVhostLeavesExistingFileUntouched(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	m.opt.UserHome = filepath.Join(t.TempDir(), "home") // 与真实家目录彻底隔离

	dir := filepath.Join(prefix, "etc", "nginx", "vhosts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "000-default.conf")
	sentinel := "# 用户/面板正在用的默认站点\nserver {\n    listen 80 default_server;\n    server_name _;\n    location ^~ /phpmyadmin/ { alias /x; }\n}\n"
	if err := os.WriteFile(conf, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	// 把 mtime 往前拨一点，任何写入都会让它变化（内容相同也算被碰过）
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(conf, old, old); err != nil {
		t.Fatal(err)
	}

	res := &InstallResult{}
	if err := m.ensureDefaultVhost(context.Background(), res); err != nil {
		t.Fatalf("已存在默认站点时不该报错: %v", err)
	}
	after, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != sentinel {
		t.Fatalf("已存在的默认站点被改动了！\n--- 之前 ---\n%s\n--- 之后 ---\n%s", sentinel, after)
	}
	st, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(before.ModTime().Truncate(time.Second)) && st.ModTime().After(old) {
		t.Errorf("默认站点被碰过（mtime 变化）：%v → %v", old, st.ModTime())
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "未改动") {
		t.Errorf("应当如实说明「已存在、未改动」，实际 Steps=%v", res.Steps)
	}
}

// TestPMADefaultVhostContent：默认站点的内容必须最小、可用、且**真的能跑 PHP**。
//
// 用户要求（2026-09-17）：一键装完要有一个 localhost/IP 能访问的默认站点，
// 里面有一个默认 index.php，而且**必须被 PHP 执行**（fastcgi_pass 指向该机器
// 当前 PHP 版本的专属 socket，不能写死 9000 —— 多版本下 9000 上没人听）。
func TestPMADefaultVhostContent(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	www := filepath.Join(prefix, "home", "www")
	siteRoot := filepath.Join(www, "localhost")
	sock := filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")

	content := m.pmaDefaultVhostContent(www, siteRoot, "unix:"+sock, "8.2")
	for _, want := range []string{
		"listen       80 default_server;", // 必须抢到默认 server，否则 localhost/IP 访问不到
		"server_name  _;",
		"root   " + siteRoot + ";",
		"index  index.html index.php;", // 用户要求：默认站点是纯静态的，index.html 优先
		filepath.Join(www, "_logs", "localhost.access.log"),
		"location = /phpmyadmin",
		"location ^~ /phpmyadmin",
		"allow 127.0.0.1;",
		"deny all;",
		"fastcgi_pass unix:" + sock + ";", // **关键**：专属端点，不是 9000
		"fastcgi_param  SCRIPT_FILENAME    $request_filename;",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("默认站点内容缺少 %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "127.0.0.1:9000") {
		t.Errorf("默认站点不该写死 9000（多版本下 9000 上没人监听）：\n%s", content)
	}
	if n := strings.Count(content, "fastcgi_pass"); n != 2 {
		t.Errorf("应有且仅有两处 fastcgi_pass（默认站点 + phpMyAdmin），实际 %d：\n%s", n, content)
	}
	if strings.Count(content, "{") != strings.Count(content, "}") {
		t.Errorf("花括号不配对，nginx 会拒绝加载：\n%s", content)
	}
	// 所有文件系统路径都必须在 brew 前缀或站点根下（换了机器不能指向空气）
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "root", "alias", "access_log", "error_log", "include":
			p := strings.TrimSuffix(f[1], ";")
			if !strings.HasPrefix(p, prefix+"/") && !strings.HasPrefix(p, www+"/") {
				t.Errorf("路径 %s 既不在 brew 前缀也不在站点根下：%s", p, line)
			}
		case "fastcgi_pass":
			p := strings.TrimSuffix(strings.TrimPrefix(f[1], "unix:"), ";")
			if !strings.HasPrefix(p, prefix+"/") {
				t.Errorf("fastcgi 端点 %s 不在 brew 前缀下：%s", p, line)
			}
		}
	}
}

// TestPHPMYAdminLooksOK：验证必须认得出"这真的是 phpMyAdmin 页面"。
//
// 真机事故（2026-09-17 mini）：一键 LNMP 报"phpMyAdmin 已可用"，实际访问是
// nginx 的 404 页 —— 旧实现只要 body 非空、且不含 phpMyAdmin 的错误关键字就算成功。
// 这里把判据钉成"正向标志"：nginx 的错误页永远不该通过。
func TestPHPMYAdminLooksOK(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"真的登录页", `<html><body><form><input name="pma_username"></form></body></html>`, true},
		{"品牌名", `<html><title>phpMyAdmin</title></html>`, true},
		{"nginx 404 页", "<html>\n<head><title>404 Not Found</title></head>\n<body>\n<center><h1>404 Not Found</h1></center>\n</body>\n</html>\n", false},
		{"nginx 502 页", "<html><head><title>502 Bad Gateway</title></head></html>", false},
		{"别的站点页面", "<html><body><h1>Welcome to my blog</h1></body></html>", false},
		{"空", "", false},
	}
	for _, c := range cases {
		if got := phpMyAdminLooksOK(c.body); got != c.want {
			t.Errorf("%s：phpMyAdminLooksOK = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestDescribePMAFailure：失败原因必须能让用户直接行动。
func TestDescribePMAFailure(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"<html><title>404 Not Found</title></html>", "404"},
		{"<html><title>502 Bad Gateway</title></html>", "502"},
		{"", "没有任何响应"},
		{"Existing configuration file is not readable", "is not readable"},
		{"<html>某个不认识的页面</html>", "不是 phpMyAdmin"},
	}
	for _, c := range cases {
		got := describePMAFailure(c.body)
		if !strings.Contains(got, c.want) {
			t.Errorf("describePMAFailure(%q) = %q，应包含 %q", c.body, got, c.want)
		}
	}
}

// TestDefaultVhostAction：三种"已存在"必须被区别对待（护栏）。
//
// create  = 不存在/空 → 建
// upgrade = 我们早期创建的（带标记）→ 可以原地升级（否则老机器永远没有 PHP 默认站点）
// keep    = 别人的内容 → **一个字都不改**（MacBook 上有正在用的默认站点）
func TestDefaultVhostAction(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"不存在", "", "create"},
		{"只有空白", "  \n\t ", "create"},
		{"我们早期创建的", "# 由 ZizPanel 创建：默认站点（一键 LNMP 的收尾）\nserver {}\n", "upgrade"},
		{"用户自己的", "server {\n    listen 80;\n    server_name mysite;\n}\n", "keep"},
		{"面板完整模板（web 生成）", "# 默认站点（由 ZizPanel 生成 —— 请勿手工编辑，面板会整份重写）\nserver {}\n", "keep"},
	}
	for _, c := range cases {
		if got := defaultVhostAction(c.in); got != c.want {
			t.Errorf("%s：defaultVhostAction = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestDefaultVhostUpgradeOnlyWritesWhenContentDiffers：幂等 = 内容一致就不折腾。
//
// 真机 2026-09-17：一次任务里 InstallLNMP 与 phpMyAdmin 都会确保默认站点，
// 第二次把一模一样的字节又写了一遍 → nginx -t + reload 各多一次、mtime 也变了。
// 这里锁住"内容一致 → 不写盘"这条判断（写盘与否由内容比较决定，纯逻辑、可单测）。
func TestDefaultVhostUpgradeOnlyWritesWhenContentDiffers(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	www := filepath.Join(prefix, "home", "www")
	siteRoot := filepath.Join(www, "localhost")
	pass := "unix:" + filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	content := m.pmaDefaultVhostContent(www, siteRoot, pass, "8.2")

	// 内容一致 → 不该写
	if defaultVhostNeedsWrite(content, content) {
		t.Error("内容完全一致时不该再写盘（幂等 = 不折腾）")
	}
	// 是我们早期创建的、但内容旧（例如没有 fastcgi_pass 的版本）→ 要写
	legacy := "# 由 ZizPanel 创建：默认站点（一键 LNMP 的收尾）\nserver {\n    index index.html index.php;\n}\n"
	if !defaultVhostNeedsWrite(legacy, content) {
		t.Error("我们自己创建的旧版本应被原地升级（写盘）")
	}
	// 别人的内容 → 不写（护栏）
	userFile := "server {\n    listen 80 default_server;\n    server_name mysite;\n}\n"
	if defaultVhostNeedsWrite(userFile, content) {
		t.Error("非面板创建的内容绝不能被改写")
	}
	// 不存在 → 写（创建）
	if !defaultVhostNeedsWrite("", content) {
		t.Error("默认站点缺失时应创建")
	}
}

// TestPMAVhostInsertBlockUsesExplicitEndpoint：往"已有的默认站点"里插 phpMyAdmin
// 入口时，也必须写显式的专属端点，不能 include 那份写死 9000 的历史片段。
//
// 真机风险：用户的默认站点不是面板生成的（"keep"三态里的那一类）时，走的就是
// 这条插入路径；写错在"PHP 只听专属 socket"的机器上就是 phpMyAdmin 502/404。
func TestPMAVhostInsertBlockUsesExplicitEndpoint(t *testing.T) {
	share := "/opt/homebrew/share/phpmyadmin"
	pass := "unix:/opt/homebrew/var/run/php-fpm-8.2.sock"
	block := pmaVhostInsertBlock(share, pass, "8.2")

	if !strings.Contains(block, "fastcgi_pass "+pass+";") {
		t.Errorf("插入块必须写显式的专属端点：\n%s", block)
	}
	for _, bad := range []string{"127.0.0.1:9000", "includes/php-fpm.conf", "include "} {
		if strings.Contains(block, bad) {
			t.Errorf("插入块不该出现 %q（历史片段写死 9000）：\n%s", bad, block)
		}
	}
	for _, want := range []string{
		"location ^~ /phpmyadmin",
		"alias " + share + ";",
		"allow 127.0.0.1", // 注意：这个块本身不含 allow/deny，守卫在默认站点模板里
		"fastcgi_param  SCRIPT_FILENAME    $request_filename;",
	} {
		if want == "allow 127.0.0.1" {
			continue // 插入块只管 location；访问限制由默认站点模板/面板入口负责
		}
		if !strings.Contains(block, want) {
			t.Errorf("插入块缺少 %q：\n%s", want, block)
		}
	}
	if strings.Count(block, "{") != strings.Count(block, "}") {
		t.Errorf("花括号不配对：\n%s", block)
	}
}

// TestDefaultSiteStaysStaticNoIndexPHP：默认站点必须是**纯静态**的。
//
// 用户 2026-09-18 明确要求（原话）：
//
//	"我从没要求默认站点要能跑 PHP！从没有！我一直说得是默认站点是纯表态的。
//	 只需要一个 index.html！！！"
//
// 以前 phpMyAdmin 安装器会顺手往默认站点里放一个 index.php，而它在 nginx 的
// index 指令里排在 index.html 前面 —— 结果首页永远由 PHP 渲染，
// "默认站点真的生效了吗"的请求级复核（靠 index.html 里的标记）**永远失败**，
// 那一步失败还会中止整个上传上限任务（真机事故，见 DEVELOPMENT 坑 173）。
//
// 这条门禁两件事一起锁：① 面板不再创建默认站点的 index.php；
// ② 生成的默认站点 vhost 里 index.html 排在 index.php 前面。
func TestDefaultSiteStaysStaticNoIndexPHP(t *testing.T) {
	// ① 源码级：phpMyAdmin 安装器不许再调 EnsureDefaultSitePHPIndex
	src, err := os.ReadFile("phpmyadmin.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "EnsureDefaultSitePHPIndex") {
		t.Error("phpMyAdmin 安装器又在往默认站点里放 index.php —— 默认站点必须是纯静态的" +
			"（并且会让默认站点复核永远失败）")
	}
	// ② 生成器级：这两处"最小默认站点"文案里，index.html 必须在前面
	for _, f := range []string{"phpmyadmin.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "index  index.php index.html;") {
			t.Errorf("%s 里还有把 index.php 排在前面的默认站点配置", f)
		}
	}
}

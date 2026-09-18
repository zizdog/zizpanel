package services

import (
	"context"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  一键 LNMP 的默认值与 PHP 端点闭环
//
//  这一组测试锁住两件用户明确要求、且真机上反复出问题的事：
//
//   1. 默认装 **PHP 8.2**（不是 8.3）——连日志文案一起；
//   2. 一键 LNMP 装出来的 PHP 必须被固定到**自己专属的端点**（默认 Unix
//      socket），而不是继续听 9000。历史上只有"应用市场单独装"那条路径做了
//      这件事，一键 LNMP 没做 → 随后装第二个版本必然互相抢端口。
//
//  全部在 t.TempDir() 里跑：不碰真实 /opt/homebrew、不碰真实站点配置。
// ============================================================================

// TestLNMPDefaultsToPHP82 锁住"默认 PHP 版本 = 8.2"。
//
// 为什么要有这条测试：默认值散落在 LNMPFormulas、LNMPPorts、配置默认值、
// 站点默认版本好几处，改一处漏一处的结果是"装的版本"和"登记的版本/站点默认"
// 不一致 —— 用户看到的就是"装不上/装了看不到"。
func TestLNMPDefaultsToPHP82(t *testing.T) {
	joined := strings.Join(LNMPFormulas, " ")
	if !strings.Contains(joined, "php@8.2") {
		t.Errorf("LNMPFormulas 必须包含 php@8.2（用户要求默认装 8.2），实际 %v", LNMPFormulas)
	}
	for _, f := range LNMPFormulas {
		if f == "php@8.3" {
			t.Errorf("LNMPFormulas 不该再把 php@8.3 当默认（8.3 应保留为**可选**条目），实际 %v", LNMPFormulas)
		}
	}
	// 端口表的键必须跟着默认版本走：registerLNMPComponents 与验证都按
	// LNMPPorts[formula] 取值，键对不上就会登记成端口 0/验证成"没在监听"。
	if _, ok := LNMPPorts["php@8.2"]; !ok {
		t.Errorf("LNMPPorts 缺少 php@8.2 的键（登记与验证会拿不到判定目标），实际 %v", LNMPPorts)
	}
	if _, ok := LNMPPorts["php@8.3"]; ok {
		t.Errorf("LNMPPorts 不该还留着 php@8.3 的键（默认已换成 8.2），实际 %v", LNMPPorts)
	}
	// PHP 不占固定端口：面板的多版本设计是"每版本一个专属 socket"，
	// 所以它的判定端口必须是 0（表示走端点判定），不能再是 9000。
	if got := LNMPPorts["php@8.2"]; got != 0 {
		t.Errorf("php@8.2 不该占固定端口（应为 0，走专属 socket 判定），实际 %d", got)
	}
	// 每个 formula 都必须有判定目标，否则验证阶段会静默漏掉一个组件。
	for _, f := range LNMPFormulas {
		if _, ok := LNMPPorts[f]; !ok {
			t.Errorf("%s 在 LNMPPorts 里没有判定目标", f)
		}
	}
	// 默认版本必须在应用目录里有条目：登记时要靠它取显示名/图标/分类。
	if _, ok := lnmpCatalogApp("php@8.2"); !ok {
		t.Error("应用目录里缺少 BrewFormula=php@8.2 的条目，一键 LNMP 登记时会退化成裸 formula 名")
	}
}

// shortPHPPrefix 造一个**足够短**的临时 Homebrew 前缀。
//
// 为什么不用 t.TempDir()：macOS 上它形如
// /var/folders/jd/<40 个随机字符>/T/TestXxx/001，光前缀就 60+ 字符，
// 而 Unix domain socket 的 sun_path 上限是 104 字节（真机上
// /opt/homebrew/var/run/php-fpm-8.2.sock 只有 41 字符）。用 t.TempDir()
// 会让"路径过长"这条真实错误在测试里先被撞上，测的就不是要看的东西了。
// 与 internal/sites 的 shortTempDir 同一套做法（不同包，各自留一份）。
func shortPHPPrefix(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zpphp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// sandboxManagerWithBrew 造一个"brew 前缀指向临时目录"的 Manager。
//
// 为什么不改 priv 的环境变量钩子：BrewBin 是生产代码真实的推导入口
// （phpBrewPrefix 优先用它），用它能顺带验证那条推导路径本身是对的。
func sandboxManagerWithBrew(t *testing.T, prefix string) *Manager {
	t.Helper()
	m, _ := sandboxManager(t)
	m.opt.BrewBin = filepath.Join(prefix, "bin", "brew")
	// 留空 UserName：本测试不验证 socket 属主（那会去查真实系统用户），
	// 也避免在临时目录上做 chown。
	m.opt.UserName = ""
	// launchd 目录必须隔离：默认实现读真机 /Library/LaunchDaemons，而本机恰好
	// 把 php@8.2 装成了系统级守护进程 —— 那会让"服务尚未注册到 launchd"这个
	// 测试前提在真机上不成立（同一提交在空机器上绿、在本机红，2026-09-18 真踩到）。
	m.launchdDirsOverride = []string{filepath.Join(t.TempDir(), "LaunchDaemons")}
	return m
}

// writeFakePHPConf 造出某个 PHP 版本的 www.conf（Homebrew 出厂版：听 9000）。
func writeFakePHPConf(t *testing.T, prefix, version, listen string) string {
	t.Helper()
	conf := filepath.Join(prefix, "etc", "php", version, "php-fpm.d", "www.conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[global]\npid = " + filepath.Join(prefix, "var", "run", "php-fpm.pid") + "\n\n" +
		"[www]\nuser = zizdog\ngroup = staff\nlisten = " + listen + "\npm = dynamic\n"
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return conf
}

// TestEnsurePHPListenEndpointWritesOwnSocket 是端点闭环的核心断言。
//
// 它同时验证四件事：
//  1. 目标版本的 www.conf 被改写成"该版本专属"的 socket（不再听 9000）；
//  2. **别的版本**的 www.conf 一个字都没被动（真机上 8.3 可能就是用户在用）；
//  3. 服务还没进 launchd 时，如实说"稍后注册时会按端点启动"，
//     绝不谎报"已重启生效"；
//  4. 幂等：第二次跑不再改文件，且明说"无需改动"。
func TestEnsurePHPListenEndpointWritesOwnSocket(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	ctx := context.Background()

	conf82 := writeFakePHPConf(t, prefix, "8.2", "127.0.0.1:9000")
	// 另一个版本：必须保持原样（面板只碰被安装的那个版本）
	conf83 := writeFakePHPConf(t, prefix, "8.3", "127.0.0.1:9000")
	before83, err := os.ReadFile(conf83)
	if err != nil {
		t.Fatal(err)
	}

	res := &InstallResult{}
	if err := m.ensurePHPListenEndpoint(ctx, "php@8.2", res); err != nil {
		t.Fatalf("端点闭环不该失败（服务未注册不是失败）：%v\nSteps=%v", err, res.Steps)
	}

	wantSock := filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	text := readFileOrFail(t, conf82)
	// php-fpm 的 listen 只认裸的绝对路径；带 unix: 前缀会启动失败（真机实测）。
	if !strings.Contains(text, "listen = "+wantSock) {
		t.Errorf("PHP 8.2 的 www.conf 没被改成专属 socket。\n期望含: listen = %s\n实际:\n%s", wantSock, text)
	}
	if strings.Contains(text, "listen = unix:") {
		t.Errorf("php-fpm 的 listen 不能带 unix: 前缀（会以 invalid port value 启动失败）：\n%s", text)
	}
	if strings.Contains(text, "127.0.0.1:9000") {
		t.Errorf("PHP 8.2 仍然听着 9000（多版本会互相抢）：\n%s", text)
	}
	if after83 := readFileOrFail(t, conf83); after83 != string(before83) {
		t.Errorf("PHP 8.3 的配置被误改了（面板只该动本次处理的那个版本）：\n%s", after83)
	}
	// 备份留着，用户能回到出厂配置
	if _, err := os.Stat(conf82 + ".zizpanel.bak"); err != nil {
		t.Errorf("首次改写应留一份一次性备份 %s.zizpanel.bak：%v", conf82, err)
	}

	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, wantSock) {
		t.Errorf("Steps 必须写明实际端点（用户排查时第一件事就是看它）：\n%s", joined)
	}
	// 服务不在 launchd 里 → 必须如实说明"稍后注册时生效"
	if !strings.Contains(joined, "launchd") {
		t.Errorf("服务未注册时应如实说明「稍后注册时按该端点启动」，实际 Steps：\n%s", joined)
	}
	for _, bad := range []string{"已监听在", "错误：", "重启失败"} {
		if strings.Contains(joined, bad) {
			t.Errorf("没重启也没验证过，不该出现 %q（谎报成功/无中生有的失败）：\n%s", bad, joined)
		}
	}
	if res.Warning != "" {
		t.Errorf("服务未注册属于流程内的正常状态，不该产生告警：%q", res.Warning)
	}

	// ---- 幂等：再跑一次不改文件、不报错 ----
	before := readFileOrFail(t, conf82)
	res2 := &InstallResult{}
	if err := m.ensurePHPListenEndpoint(ctx, "php@8.2", res2); err != nil {
		t.Fatalf("第二次执行端点闭环失败：%v", err)
	}
	if after := readFileOrFail(t, conf82); after != before {
		t.Errorf("第二次执行又改了配置文件（应幂等）：\n%s", after)
	}
	if !strings.Contains(strings.Join(res2.Steps, "\n"), "无需改动") {
		t.Errorf("幂等时应明说「无需改动」，实际 Steps=%v", res2.Steps)
	}
}

// TestEnsurePHPListenEndpointDegradesHonestlyWhenRestartNotPermitted：
// 服务由系统域 launchd 托管、当前身份又不是 root 时（真机 kickstart 报
// "Operation not permitted"），**必须当失败如实报**，不能因为"端点写进去了"
// 就假装整条闭环成功：服务此刻可能还在旧端点上跑，站点会 502。
//
// 与"尚未注册到 launchd"（那种情况随后注册会按新端点首次 bind）是两回事，
// 所以这里断言的是"有错误 + 有 Warning + 没有成功字样"。
func TestEnsurePHPListenEndpointDegradesHonestlyWhenRestartNotPermitted(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	// 系统域里的 plist 存在（所以 label 解析得出来、走的是"重启"这条分支），
	// 但重启本身没权限 —— 真机上就是这么报的。
	daemons := filepath.Join(t.TempDir(), "LaunchDaemons")
	if err := os.MkdirAll(daemons, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(daemons, "homebrew.mxcl.php@8.2.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.launchdDirsOverride = []string{daemons}
	m.phpRestartOverride = func(context.Context, string) error {
		return errors.New("launchctl kickstart 失败: Operation not permitted")
	}
	writeFakePHPConf(t, prefix, "8.2", "127.0.0.1:9000")

	res := &InstallResult{}
	err := m.ensurePHPListenEndpoint(context.Background(), "php@8.2", res)
	if err == nil {
		t.Fatal("无权重启时必须返回错误：端点虽已写入，但服务还没按新端点跑，不能算成功")
	}
	joined := strings.Join(res.Steps, "\n")
	wantSock := filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	if !strings.Contains(joined, wantSock) {
		t.Errorf("失败说明里必须写清端点写到哪了（用户第一眼要看它）：\n%s", joined)
	}
	if !strings.Contains(res.Warning, "8.2") {
		t.Errorf("必须进 Warning，否则只看摘要会以为成功：%q", res.Warning)
	}
	for _, bad := range []string{"已监听在", "已生效", "已重启"} {
		if strings.Contains(joined, bad) {
			t.Errorf("没重启成功就出现 %q（谎报成功）：\n%s", bad, joined)
		}
	}
}

// TestEnsurePHPListenEndpointReportsFailureHonestly：
// 配置读不出来时必须"报错 + 进 Warning + 不出现成功字样"。
func TestEnsurePHPListenEndpointReportsFailureHonestly(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	ctx := context.Background()

	// 故意不创建 www.conf（模拟"包没装好/配置被删"）
	res := &InstallResult{}
	err := m.ensurePHPListenEndpoint(ctx, "php@8.2", res)
	if err == nil {
		t.Fatal("www.conf 不存在时必须返回错误，不能假装配好了")
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "错误：") || !strings.Contains(joined, "8.2") {
		t.Errorf("失败必须写清是哪个版本、什么原因，实际：\n%s", joined)
	}
	if !strings.Contains(res.Warning, "8.2") {
		t.Errorf("失败必须进 Warning（否则用户只看摘要会以为成功），实际 %q", res.Warning)
	}
	if strings.Contains(joined, "已监听在") {
		t.Error("失败时不能出现「已监听」字样")
	}
}

// TestEnsurePHPListenEndpointSkipsNonVersionedPHP：
// 无版本后缀的 php 与其它 formula 一律不碰 —— 它的实际版本随 Homebrew 漂移，
// 拿它去猜一个版本号改 www.conf 是改用户配置，不能猜。
func TestEnsurePHPListenEndpointSkipsNonVersionedPHP(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	res := &InstallResult{}
	for _, f := range []string{"php", "nginx", "mysql@8.4", "", "php@"} {
		if err := m.ensurePHPListenEndpoint(context.Background(), f, res); err != nil {
			t.Errorf("%q 不该触发端点闭环，却返回错误 %v", f, err)
		}
	}
	if len(res.Steps) != 0 {
		t.Errorf("不该产生任何步骤，实际 %v", res.Steps)
	}
}

// TestLNMPComponentLiveUsesSocketForPHP：存活判定对 PHP 必须走专属 socket。
//
// 用真实的 Unix socket（net.Listen("unix", ...)）+ 临时 brew 前缀，
// 证明"socket 在监听 → 组件算活着"，而不是靠 TCP 9000 误判。
func TestLNMPComponentLiveUsesSocketForPHP(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	ctx := context.Background()

	label, live := m.lnmpComponentLive(ctx, "php@8.2", nil)
	if live {
		t.Fatal("socket 还没建出来，不该判定为在监听")
	}
	if !strings.Contains(label, "php-fpm-8.2.sock") {
		t.Errorf("标签里必须写明实际端点（用户要能一眼看到它）：%q", label)
	}
	if !strings.Contains(label, "unix:") {
		t.Errorf("PHP 的存活判定应是 Unix socket，实际标签 %q", label)
	}

	// 真的把一个 socket 建出来
	sock := filepath.Join(prefix, "var", "run", "php-fpm-8.2.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("建测试 socket 失败：%v", err)
	}
	defer func() { _ = ln.Close() }()

	label, live = m.lnmpComponentLive(ctx, "php@8.2", nil)
	if !live {
		t.Errorf("socket 已在监听，应判定为活着，实际标签 %q", label)
	}
}

// TestInstallLNMPCallsSharedPHPListenFix 锁住**调用位置与复用**。
//
// 2026-09-19：PHP 端点闭环从"内联在 InstallLNMP 里的循环"提成了
// ensureLNMPPHPEndpoints（因为本次选择要作为参数传进来）。位置约束一字未变，
// 所以断言跟着改成：InstallLNMP 必须调用这个共用实现，且该调用必须出现在
// installSystemDaemons **之前**。
//
// InstallLNMP 没法直接跑单测（第一件事就要求 root），所以与
// TestInstallLNMPCallsRegistrationOutsideRunningBranch 一样直接读源码锁结构：
//
//	① 必须调用共用实现 ensurePHPListenEndpoint（不许再抄一份 EnsureListen）；
//	② 必须在 installSystemDaemons **之前** 调用 —— 注册那一步 bootstrap
//	   LaunchDaemon 时 php-fpm 才第一次读 www.conf，先写端点才能一次 bind 对；
//	③ 只对 php@x.y 生效（别把 nginx/mysql 也塞进去）。
func TestInstallLNMPCallsSharedPHPListenFix(t *testing.T) {
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

	iFix := strings.Index(body, "m.ensureLNMPPHPEndpoints(ctx, result, formulas)")
	if iFix < 0 {
		t.Fatal("一键 LNMP 必须调用共用实现 m.ensureLNMPPHPEndpoints(ctx, result, formulas)（它内部调 ensurePHPListenEndpoint）：" +
			"否则一键装出来的 php-fpm 仍然听 9000，随后装第二个版本就互相抢端口")
	}
	iDaemon := strings.Index(body, "m.installSystemDaemons(ctx, result, formulas)")
	if iDaemon < 0 {
		t.Fatal("找不到 installSystemDaemons 调用（这段结构变了，请同步更新本测试）")
	}
	if iFix > iDaemon {
		t.Error("端点闭环必须在注册系统级服务**之前**：LaunchDaemon 在那一步启动，" +
			"先写端点才能一次 bind 到专属 socket")
	}
	// 过滤逻辑留在 ensureLNMPPHPEndpoints 里（它必须按 phpVersionFromFormula 过滤），
	// 而那个函数必须**复用** ensurePHPListenEndpoint，不许自己抄一份。
	helper := src[strings.Index(src, "func (m *Manager) ensureLNMPPHPEndpoints("):]
	if iEnd := strings.Index(helper, "\n// installSystemDaemons"); iEnd > 0 {
		helper = helper[:iEnd]
	}
	if !strings.Contains(helper, "phpVersionFromFormula(f)") {
		t.Error("端点闭环必须按 phpVersionFromFormula 过滤：nginx / mysql 不该走 PHP 的端点逻辑")
	}
	if !strings.Contains(helper, "m.ensurePHPListenEndpoint(ctx, f, result)") {
		t.Error("ensureLNMPPHPEndpoints 必须复用 ensurePHPListenEndpoint（与通用 brew 安装路径同一份实现）")
	}
	if strings.Contains(body, "sites.EnsureListen") || strings.Contains(helper, "sites.EnsureListen") {
		t.Error("一键 LNMP 不该自己调 sites.EnsureListen —— 必须复用 ensurePHPListenEndpoint，" +
			"两份实现迟早走样（这正是历史上通用路径修了、一键路径没修的根因）")
	}

	// 通用 brew 路径也必须走同一份实现（不能各写一遍）
	ib, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatal(err)
	}
	ibody := string(ib)
	s0 := strings.Index(ibody, "func (m *Manager) installViaBrew(")
	if s0 < 0 {
		t.Fatal("找不到 installViaBrew")
	}
	s1 := strings.Index(ibody[s0:], "\n// brewServiceInfo")
	if s1 < 0 {
		t.Fatal("找不到 installViaBrew 的结尾")
	}
	vbody := ibody[s0 : s0+s1]
	if !strings.Contains(vbody, "m.ensurePHPListenEndpoint(ctx, app.BrewFormula, res)") {
		t.Error("应用市场的通用安装路径也必须复用 ensurePHPListenEndpoint")
	}
	if strings.Contains(vbody, "sites.EnsureListen") {
		t.Error("通用路径不该自己调 sites.EnsureListen —— 与一键 LNMP 共用同一份实现才不会再走样")
	}
}

// TestHoldersAllMatch：端口占用者判定的安全边界。
//
// 这个函数决定"要不要动手停掉占用 80 的进程"，认错就是误杀别人的服务。
func TestHoldersAllMatch(t *testing.T) {
	cases := []struct {
		holders []string
		want    string
		ok      bool
	}{
		{[]string{"nginx (pid 39411)"}, "nginx", true},
		{[]string{"nginx (pid 1)", "nginx (pid 2)"}, "nginx", true},
		// 只要有一个认不出来，就绝不动手
		{[]string{"nginx (pid 1)", "node (pid 2)"}, "nginx", false},
		{[]string{"python3.13 (pid 7)"}, "nginx", false},
		{nil, "nginx", false},
		{[]string{" (pid 7)"}, "nginx", false},
	}
	for _, c := range cases {
		if got := holdersAllMatch(c.holders, c.want); got != c.ok {
			t.Errorf("holdersAllMatch(%v, %q) = %v，期望 %v", c.holders, c.want, got, c.ok)
		}
	}
}

// readFileOrFail 读文件内容，失败就 Fatal。
func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

// TestChownTreeToWalksAndIsIdempotent：日志属主修正的实现细节。
//
// 这条链路修的是真机上的 `[emerg] open() ... Permission denied`：面板（root）
// 建出来的日志目录，nginx 以真实用户运行时写不进去。测试只能在临时目录里
// 用"当前用户"跑（非 root 不能 chown 成别人），所以断言的是：
// 遍历覆盖到每个条目、本来就是目标属主时不产生改动、幂等。
func TestChownTreeToWalksAndIsIdempotent(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("取不到当前用户: %v", err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proxy"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.access.log", "a.error.log", filepath.Join("proxy", "b.access.log")} {
		if err := os.WriteFile(filepath.Join(root, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	changed, total, err := chownTreeTo(me.Username, root)
	if err != nil {
		t.Fatalf("chownTreeTo 失败: %v", err)
	}
	// root 本身 + proxy 目录 + 3 个文件
	if total < 5 {
		t.Errorf("应遍历到每个条目（root+目录+3 文件），实际 %d", total)
	}
	if changed != 0 {
		t.Errorf("本来就属于当前用户，不该产生改动，实际 %d 项", changed)
	}
	// 幂等
	changed2, _, err := chownTreeTo(me.Username, root)
	if err != nil || changed2 != 0 {
		t.Errorf("第二次执行应无改动，实际 changed=%d err=%v", changed2, err)
	}

	// 不存在的目录：调用方（ensureNginxLogOwnership）会跳过，但要能报错而不是假装成功
	if _, _, err := chownTreeTo(me.Username, filepath.Join(root, "nope")); err == nil {
		t.Error("目录不存在时应返回错误，调用方据此跳过（不能假装修好了）")
	}
}

// TestEnsureNginxLogOwnershipSkipsMissingDirs：目录不存在时静默跳过（不产生步骤/告警）。
func TestEnsureNginxLogOwnershipSkipsMissingDirs(t *testing.T) {
	prefix := shortPHPPrefix(t)
	m := sandboxManagerWithBrew(t, prefix)
	m.opt.UserName = "" // 交给真实用户的那一步在测试里不做（会 chown 真实用户）
	res := &InstallResult{}
	m.ensureNginxLogOwnership(context.Background(), res)
	if len(res.Steps) != 0 || res.Warning != "" {
		t.Errorf("没有日志目录时不该有任何输出，实际 steps=%v warning=%q", res.Steps, res.Warning)
	}
}

// TestHoldersContainPID：判断"占着 80 的进程是不是 launchd 托管的那个实例"。
//
// 这个判断决定"要不要接管 80"。真机教训（2026-09-17 mini）：只看
// "/Library/LaunchDaemons 里有没有 plist" 会把"上一轮失败留下的僵尸 plist"
// 误判成"已托管"，于是 root 起的游离 nginx 永远占着 80，而界面显示 running。
func TestHoldersContainPID(t *testing.T) {
	cases := []struct {
		holders []string
		pid     int
		want    bool
	}{
		{[]string{"nginx (pid 74181)", "nginx (pid 74182)"}, 74181, true},
		{[]string{"nginx (pid 74181)"}, 999, false},
		{[]string{"nginx (pid 74181)"}, 0, false}, // launchd 说没进程 → 不是它的
		{nil, 74181, false},
		// 不能把 7418 当成 74181 的前缀误命中（needle 带右括号）
		{[]string{"nginx (pid 999)", "nginx (pid 74181)"}, 7418, false},
		{[]string{"nginx (pid 74181)"}, 74181, true},
	}
	for _, c := range cases {
		if got := holdersContainPID(c.holders, c.pid); got != c.want {
			t.Errorf("holdersContainPID(%v, %d) = %v，期望 %v", c.holders, c.pid, got, c.want)
		}
	}
}

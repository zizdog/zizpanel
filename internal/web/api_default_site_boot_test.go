package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  "装完面板就该有一个默认静态站点"（用户要求）
//
//  这条测试锁的是**自动**那一步的真实性与诚实性：
//    · 有 nginx + 没有 PHP → 也必须建成一份**纯静态**可用的默认站点
//      （用户的诉求就是"不依赖 lnmp"，所以没有 PHP 不是失败理由）；
//    · 没有 nginx → 如实记录"缺 nginx"，绝不谎报已创建，界面上给「只安装 Nginx」；
//    · 非 root（调试实例）→ 跳过，且理由写明（AGENTS 坑 162：调试实例不许写真机配置）。
// ============================================================================

// seedFakeNginx 在沙箱里放一个假 nginx 可执行文件，让"二进制在不在"这条判据成立。
func seedFakeNginx(t *testing.T, srv *Server) {
	t.Helper()
	// 必须放进沙箱：newTestServer 的 NginxBin 指向真机 /opt/homebrew/bin/nginx
	// （单测不许往真实 Homebrew 目录写东西，见 TestTestServerSandboxedAwayFromRealHome）。
	srv.Cfg.NginxBin = filepath.Join(t.TempDir(), "bin", "nginx")
	if err := os.MkdirAll(filepath.Dir(srv.Cfg.NginxBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srv.Cfg.NginxBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stubDefaultSiteApply 把默认站点落地所需的三个外部动作钉成桩：
// 写 vhost（**真的写进沙箱 vhost 目录**，因为状态判据要读它）、reload、请求级复核。
func stubDefaultSiteApply(t *testing.T, srv *Server) *string {
	t.Helper()
	written := ""
	prevWrite, prevReload, prevProbe := siteWriteVhostFn, siteReloadFn, defaultProbeFn
	siteWriteVhostFn = func(s *Server, _ context.Context, domain, content string) error {
		if err := os.MkdirAll(s.Cfg.VhostDir, 0o755); err != nil {
			return err
		}
		written = content
		return os.WriteFile(filepath.Join(s.Cfg.VhostDir, domain+".conf"), []byte(content), 0o644)
	}
	siteReloadFn = func(*Server, context.Context) error { return nil }
	// 复核：默认站点接口会请求 http://127.0.0.1/ 并检查页面含占位页标记。
	// 这里直接答"拿到了标记"，等价于真实机器上 nginx 已生效。
	defaultProbeFn = func(string) (int, string) { return 200, "这是本机 Web 服务的默认站点" }
	t.Cleanup(func() {
		siteWriteVhostFn, siteReloadFn, defaultProbeFn = prevWrite, prevReload, prevProbe
	})
	return &written
}

// TestDefaultSiteAutoCreateWithoutPHP：机器上**只有 nginx、没有 PHP** 时，
// 面板启动也要把默认站点建起来，且生成的配置必须是纯静态、语法完整。
func TestDefaultSiteAutoCreateWithoutPHP(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeNginx(t, srv) // 不 seed PHP：这正是用户说的"不依赖 lnmp"
	written := stubDefaultSiteApply(t, srv)
	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 } // 假装是 root（生产里面板就是 root）
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	srv.ensureDefaultSiteOnStart(context.Background())

	index := filepath.Join(srv.Cfg.WWWRoot, "localhost", "index.html")
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("默认站点的占位页应当被创建：%v", err)
	}
	if !strings.Contains(*written, "没有可用 PHP 端点") {
		t.Errorf("没有 PHP 时默认站点必须是纯静态（并写明原因），实际生成：\n%s", *written)
	}
	if strings.Contains(*written, "fastcgi_pass") {
		t.Errorf("没有 PHP 端点时写 fastcgi_pass 会让 nginx 拒绝加载整个配置：\n%s", *written)
	}
	if !strings.Contains(*written, filepath.Join(srv.Cfg.WWWRoot, "localhost")) {
		t.Errorf("默认站点的 root 应指向 www/localhost：\n%s", *written)
	}
	// 状态接口必须说"已就绪"，而不是"未知"或"未创建"。
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/system/default-site", nil, cookies)
	data, _ := out["data"].(map[string]any)
	if data["applied"] != true {
		t.Errorf("建好后状态接口应当报 applied=true，实际 %v", out)
	}
	if data["vhost_marked"] != true {
		t.Errorf("磁盘上的 000-default.conf 应认出是面板生成的（带统一标记），实际 %v", out)
	}
	if data["waiting_nginx"] == true {
		t.Errorf("nginx 在的时候不该显示「等待 nginx」：%v", out)
	}
}

// TestDefaultSiteAutoCreateHonestWithoutNginx：没有 nginx 时**不许**谎报，
// 也不许刷错误日志 —— 这是新机器的正常状态，界面上给「只安装 Nginx」。
func TestDefaultSiteAutoCreateHonestWithoutNginx(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	srv.Cfg.NginxBin = filepath.Join(t.TempDir(), "no-such-nginx")
	written := stubDefaultSiteApply(t, srv)

	srv.ensureDefaultSiteOnStart(context.Background())

	if *written != "" {
		t.Errorf("没有 nginx 时不该写任何 vhost，实际写了：\n%s", *written)
	}
	if _, err := os.Stat(filepath.Join(srv.Cfg.VhostDir, "000-default.conf")); !os.IsNotExist(err) {
		t.Errorf("没有 nginx 时不该在磁盘上留下 vhost 文件（err=%v）", err)
	}
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/system/default-site", nil, cookies)
	data, _ := out["data"].(map[string]any)
	if data["applied"] == true {
		t.Errorf("没有 nginx 时绝不能报 applied=true：%v", out)
	}
	if data["nginx_present"] != false || data["waiting_nginx"] != true {
		t.Errorf("没有 nginx 时应报 nginx_present=false + waiting_nginx=true：%v", out)
	}
	if !strings.Contains(asString(data["needs_action"]), "Nginx") {
		t.Errorf("要告诉用户下一步是装 Nginx，实际 %q", asString(data["needs_action"]))
	}
	if !strings.Contains(asString(data["error"]), "没有 nginx") {
		t.Errorf("原因要如实写明（本机没有 nginx），实际 %q", asString(data["error"]))
	}
	// 手动创建同样要给出人话 + 409（而不是 500 或一段提权助手报错）。
	res, out2, _ := doJSON(t, ts, "POST", "/api/v1/system/default-site/apply", map[string]any{}, cookies)
	if res.StatusCode != 409 {
		t.Fatalf("没有 nginx 时手动创建应 409，实际 %d（body=%v）", res.StatusCode, out2)
	}
	if !strings.Contains(asString(out2["msg"]), "只装 Nginx") {
		t.Errorf("409 文案要给出「可以先只装 Nginx」这条路，实际 %q", asString(out2["msg"]))
	}
}

// TestDefaultSiteAutoCreateSkipsWhenNotRoot：调试实例（非 root）必须**跳过**。
//
// 理由不是洁癖：`make run-local` 只临时化配置与数据目录，BrewPrefix / WWWRoot
// 仍是真机路径（AGENTS 坑 162），在这里写默认站点 = 改真实 nginx 配置。
func TestDefaultSiteAutoCreateSkipsWhenNotRoot(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeNginx(t, srv)
	written := stubDefaultSiteApply(t, srv)
	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 501 } // 普通用户
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	srv.ensureDefaultSiteOnStart(context.Background())

	if *written != "" {
		t.Errorf("非 root 时不该写 vhost（调试实例会改到真机配置），实际写了：\n%s", *written)
	}
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/system/default-site", nil, cookies)
	data, _ := out["data"].(map[string]any)
	if data["applied"] == true {
		t.Errorf("非 root 跳过时不能报 applied=true：%v", out)
	}
	if !strings.Contains(asString(data["error"]), "root") {
		t.Errorf("要如实说明「不是以 root 运行」（跳过原因），实际 %q", asString(data["error"]))
	}
}

// TestDefaultSiteAutoCreateIsIdempotent：成功过之后不再重复写配置/reload。
//
// 面板每次启动都会调用这一步；如果每次都写盘 + reload，用户升级面板就会
// 反复动 nginx（而这份 vhost 里还有应用代理 location），风险远大于收益。
func TestDefaultSiteAutoCreateIsIdempotent(t *testing.T) {
	srv, ts := newTestServer(t)
	doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeNginx(t, srv)
	stubDefaultSiteApply(t, srv)
	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 }
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	srv.ensureDefaultSiteOnStart(context.Background())

	// 第二次：把写入计数清零，成功路径下应当一次都不写。
	writes := 0
	prevWrite := siteWriteVhostFn
	siteWriteVhostFn = func(s *Server, ctx context.Context, domain, content string) error {
		writes++
		return prevWrite(s, ctx, domain, content)
	}
	t.Cleanup(func() { siteWriteVhostFn = prevWrite })

	srv.ensureDefaultSiteOnStart(context.Background())
	if writes != 0 {
		t.Errorf("已就绪的默认站点不该在启动时被重写（实际写了 %d 次）—— 这让每次升级都动 nginx 配置", writes)
	}
}

// TestDefaultSiteNeverOverwritesForeignVhost：80 端口上若已有一份**不是面板生成的**
// 000-default.conf（用户自己写的 server 块），启动时**绝不自动覆盖**。
//
// 这是"自动"这一步最大的风险：自动动作没有确认框。所以判据必须保守 ——
// 不认识的东西一律不碰，只如实报出来，把覆盖留给用户在界面上明确选择。
func TestDefaultSiteNeverOverwritesForeignVhost(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeNginx(t, srv)
	written := stubDefaultSiteApply(t, srv)
	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 }
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	// 用户自己的 80 端口站点（没有面板标记）。
	if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "# 我自己写的站点\nserver {\n    listen 80 default_server;\n    server_name _;\n    root /tmp/mine;\n}\n"
	vhost := filepath.Join(srv.Cfg.VhostDir, "000-default.conf")
	if err := os.WriteFile(vhost, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	srv.ensureDefaultSiteOnStart(context.Background())

	if *written != "" {
		t.Errorf("不该自动覆盖用户自己的默认站点，实际写了：\n%s", *written)
	}
	if b, _ := os.ReadFile(vhost); string(b) != foreign {
		t.Errorf("用户自己的 000-default.conf 必须一字未动，实际：\n%s", b)
	}
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/system/default-site", nil, cookies)
	data, _ := out["data"].(map[string]any)
	if data["foreign_vhost"] != true {
		t.Errorf("状态接口要如实报 foreign_vhost=true（并说明面板不会自动覆盖）：%v", out)
	}
	if data["applied"] == true {
		t.Errorf("用户自己的站点不算「面板的默认站点」，不能报 applied=true：%v", out)
	}
	if !strings.Contains(asString(data["needs_action"]), "覆盖") {
		t.Errorf("要告诉用户怎么改成面板的默认站点，实际 %q", asString(data["needs_action"]))
	}
}

// TestDefaultSiteLeavesExistingPanelSiteAlone：已有**面板的**默认站点时，
// 启动不做任何写盘/reload（升级上来的老机器本来就有，不该被反复动）。
func TestDefaultSiteLeavesExistingPanelSiteAlone(t *testing.T) {
	srv, ts := newTestServer(t)
	doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeNginx(t, srv)
	written := stubDefaultSiteApply(t, srv)
	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 }
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	// 复刻"9 月 15 日那份由面板生成、但状态文件还不存在"的老机器现场。
	if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.Cfg.VhostDir, "000-default.conf"),
		[]byte("# 默认站点（由 ZizPanel 生成 —— 请勿手工编辑，面板会整份重写）\nserver { listen 80; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srv.Cfg.WWWRoot, "localhost"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.Cfg.WWWRoot, "localhost", "index.html"), []byte("<h1>old</h1>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv.ensureDefaultSiteOnStart(context.Background())
	if *written != "" {
		t.Errorf("已经有的面板默认站点不该在启动时被重写：\n%s", *written)
	}
	// 状态要"补记"成已就绪（以前是手动建的），这样界面不会一直提示"未创建"。
	srv2 := srv
	if st := srv2.readDefaultSiteState(); !st.Applied {
		t.Errorf("现场已满足时应当补记状态为已就绪，实际 %+v", st)
	}
	if got := srv2.defaultSiteStatusNow(); !got.Applied {
		t.Errorf("状态接口应报 applied=true，实际 %+v", got)
	}
}

// TestDefaultSiteApplyBacksUpForeignVhost：用户**明确**点「覆盖为面板默认站点」时，
// 必须先把原文件备份下来 —— 界面上写着"会先备份"，就必须真的备份。
func TestDefaultSiteApplyBacksUpForeignVhost(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeNginx(t, srv)
	stubDefaultSiteApply(t, srv)

	if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "# 我自己写的站点\nserver {\n    listen 80 default_server;\n    server_name _;\n}\n"
	vhost := filepath.Join(srv.Cfg.VhostDir, "000-default.conf")
	if err := os.WriteFile(vhost, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/default-site/apply", map[string]any{}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("用户明确点覆盖时应成功，实际 %d（body=%v）", res.StatusCode, out)
	}
	bak, err := os.ReadFile(vhost + ".zizpanel.bak")
	if err != nil {
		t.Fatalf("覆盖前必须备份原文件到 .zizpanel.bak：%v", err)
	}
	if string(bak) != foreign {
		t.Errorf("备份内容必须与用户原文件逐字一致，实际：\n%s", bak)
	}
	after, _ := os.ReadFile(vhost)
	if string(after) == foreign {
		t.Error("用户点了覆盖，vhost 应当已经换成面板的默认站点")
	}
	if !strings.Contains(string(after), services.DefaultVhostMarker) {
		t.Errorf("覆盖后的 vhost 应当是面板生成的那份：\n%s", after)
	}
}

// TestDefaultSiteHealedAfterNginxInstalledLater 锁的是用户的报障：
// "全新安装的面板，没有默认网站！"
//
// 真机顺序就长这样：先装面板（那一刻机器上连 Homebrew/nginx 都没有），再在面板里
// 装 nginx。启动时那一次检查只能如实记下"等待 nginx" —— **必须有东西在之后补上**，
// 否则"装上 Nginx 后面板会自动建默认站点"就是一句谎话（这正是当时的行为）。
// 这条测试先在没有 nginx 时跑一次，再把 nginx 放上去，靠巡检把它建出来：
// 少了 watchWebEnv 这条链，这里就必然失败。
func TestDefaultSiteHealedAfterNginxInstalledLater(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, _ = doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	// 只装桩（不读它的 *written）：断言改从磁盘上的 vhost 读，
	// 避免与巡检 goroutine 的赋值构成数据竞争，也避免下面那个竞态（见 ③）。
	stubDefaultSiteApply(t, srv)
	// 整条自愈链都会碰 priv 包的路径（conf.d / 运行时目录），必须沙箱化，
	// 否则一次 go test 就会去读写真机 /opt/homebrew/etc/nginx。
	t.Setenv("ZIZPANEL_BREW_PREFIX", srv.Cfg.BrewPrefix)

	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 }
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	// ① 明确"这台机器还没有 nginx"：必须指向一个不存在的路径
	// （newTestServer 的 Cfg.NginxBin 默认指向真机的 /opt/homebrew/bin/nginx）。
	srv.Cfg.NginxBin = filepath.Join(t.TempDir(), "no-nginx-yet", "nginx")
	srv.maybeEnsureDefaultSite(context.Background())
	st := srv.readDefaultSiteState()
	if st.Applied {
		t.Fatalf("没有 nginx 时绝不能报「已创建」（状态：%+v）", st)
	}
	if st.NginxPresent {
		t.Errorf("状态里应当如实记下「当前没有 nginx」：%+v", st)
	}

	// ② nginx 装上了 —— 等价于用户刚在面板里跑完一键 LNMP / 只安装 Nginx。
	seedFakeNginx(t, srv)

	// ③ 巡检必须自动补上，不需要用户重启面板、也不需要再去点什么。
	prevInterval := defaultSiteWatchInterval
	defaultSiteWatchInterval = 20 * time.Millisecond
	t.Cleanup(func() { defaultSiteWatchInterval = prevInterval })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.watchWebEnv(ctx) }()

	// 等待**两件事同时成立**：占位页出现、且带标记的 vhost 已落到磁盘。
	//
	// 为什么不能只等 index：createDefaultSite 的顺序是"先建占位页，再
	// buildDefaultVhost + 写 vhost"。只等 index 就会在写 vhost 之前往下走，
	// 断言 `*written` 时它还是空串 —— 这是一条**固有的竞态**（2026-09-18 实测：
	// 同一份代码时红时绿）。vhost 文件本身就是"真的写完了"的判据，直接读盘最稳，
	// 也顺带避开与巡检 goroutine 同时读写 *written 的数据竞争。
	index := filepath.Join(srv.Cfg.WWWRoot, "localhost", "index.html")
	vhostPath := filepath.Join(srv.Cfg.VhostDir, "000-default.conf")
	deadline := time.Now().Add(5 * time.Second)
	var vhostBody string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(vhostPath); err == nil {
			vhostBody = string(b)
			if _, err := os.Stat(index); err == nil &&
				strings.Contains(vhostBody, services.DefaultVhostMarker) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(index); err != nil {
		cancel()
		t.Fatalf("nginx 装上之后，面板必须自动把默认站点建起来（用户报障就是这里没人做）：%v", err)
	}
	if !strings.Contains(vhostBody, services.DefaultVhostMarker) {
		t.Errorf("自动创建的 vhost 必须是面板生成的默认站点：\n%s", vhostBody)
	}
	// 必须**等巡检真的退出**再结束测试：t.Setenv 会在测试结束后还原
	// ZIZPANEL_BREW_PREFIX，而还在飞的巡检会拿真机前缀去写真机 nginx 配置
	//（这正是这个项目反复踩的"测试污染真机"，绝不能留给运气）。
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("巡检 goroutine 没有在 ctx 取消后退出")
	}
}

// TestEnvHealWritesPHPLimitsOnceAndNeverFightsTheUser 锁的是同一类报障的另一半：
// "全新安装的面板，导入数据库文件，phpMyAdmin 也会卡死"。
//
// 根因是 PHP 侧一直是 brew 出厂值（upload 2M / post 8M / 30s）—— 面板只在用户
// 点「保存上传/执行上限」时才写 conf.d 片段，而"装完 PHP 之后"没人再写一次。
// 两条不变量：
//  1. 片段不存在时，自愈要按面板配置写出来（默认 512M/512M/300s）；
//  2. 片段已存在（用户改过 / 用户删了又想自己写）时，自愈**一个字都不改** ——
//     这个文件的文档语义就是"删掉它即恢复出厂限制"，自愈把它重写回去会毁掉那条退路。
func TestEnvHealWritesPHPLimitsOnceAndNeverFightsTheUser(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, _ = doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 }
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	// 造一个"装了 php@8.2"的 brew 前缀（DiscoverPHPVersions 认 opt/php@x.y/bin/php）。
	verDir := filepath.Join(srv.Cfg.BrewPrefix, "etc", "php", "8.2")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	phpBin := filepath.Join(srv.Cfg.BrewPrefix, "opt", "php@8.2", "bin", "php")
	if err := os.MkdirAll(filepath.Dir(phpBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(phpBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	frag := filepath.Join(verDir, "conf.d", "99-zizpanel-limits.ini")
	srv.ensurePHPLimitsOnStart(context.Background())
	b, err := os.ReadFile(frag)
	if err != nil {
		t.Fatalf("PHP 已安装时自愈必须写出上传/执行上限片段（否则导入稍大的 SQL 就是卡死/500）：%v", err)
	}
	for _, want := range []string{"upload_max_filesize = 512M", "post_max_size = 512M", "max_execution_time = 300"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("片段里缺 %q：\n%s", want, b)
		}
	}

	// 用户自己的改动不许被自愈覆盖。
	custom := "; 我自己调的\nupload_max_filesize = 1G\n"
	if err := os.WriteFile(frag, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.ensurePHPLimitsOnStart(context.Background())
	after, _ := os.ReadFile(frag)
	if string(after) != custom {
		t.Errorf("自愈不许覆盖用户已经改过的片段：\n期望：%s\n实际：%s", custom, after)
	}
}

// TestTaskCompletionTriggersEnvHealAndDefaultSite 锁住**中心钩子**这条接线：
// 任何任务收尾（成功或失败）都必须跑一次环境自愈 + 默认站点核对。
//
// 为什么用"直接调 launchTask"而不是打某个安装接口：安装接口会真的去 brew 装东西
// （AGENTS 坑 162：在调试实例上点写操作按钮＝在真机上执行）。这里只验证接线本身：
// 任务体是测试自己给的，不碰任何外部命令。
func TestTaskCompletionTriggersEnvHealAndDefaultSite(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, _ = doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	stubDefaultSiteApply(t, srv)
	t.Setenv("ZIZPANEL_BREW_PREFIX", srv.Cfg.BrewPrefix)
	seedFakeNginx(t, srv)
	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 }
	t.Cleanup(func() { defaultSiteEuid = prevEuid })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/test-hook", nil)
	srv.launchTask(rec, req, "test", "hook-env-heal", "接线测试", "test_hook",
		func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			return map[string]any{"ok": true}, nil
		})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("任务应当以 202 提交，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}

	// 等任务结束（RunningFor 返回 nil 即已完成）——钩子是**后台**跑的，
	// 任务本身不该被自愈拖住（这正是第一版写成同步后踩到的坑）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Tasks.RunningFor("hook-env-heal") == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Tasks.RunningFor("hook-env-heal") != nil {
		t.Fatal("测试任务没有在 5 秒内结束")
	}

	// 后台自愈完成时必须已经建好默认站点（nginx 是"任务开始前"就装好的，
	// 等价于"用户刚装完 nginx 的那个任务"）。
	index := filepath.Join(srv.Cfg.WWWRoot, "localhost", "index.html")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(index); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("任务收尾必须核对默认站点并把它建起来：%v", err)
	}

	// 必须等后台自愈**真的跑完**再结束测试：t.Setenv 会在测试结束后还原
	// ZIZPANEL_BREW_PREFIX，还在飞的自愈会拿真机前缀去写真机 nginx 配置。
	// 判据是"能抢到它的锁" —— 抢到了说明没有自愈在跑（立刻还回去）。
	joinDeadline := time.Now().Add(10 * time.Second)
	joined := false
	for time.Now().Before(joinDeadline) {
		if webEnvHealMu.TryLock() {
			webEnvHealMu.Unlock()
			joined = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !joined {
		t.Fatal("后台环境自愈没有在 10 秒内结束")
	}
}

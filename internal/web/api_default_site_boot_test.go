package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  "装完面板就该有一个默认静态站点"（用户 2026-09-21 要求）
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

package web

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  非 80 端口"只拒真冲突"（坑 215，2026-09-20 生产事故）
//
//  旧实现只要端口相同就拒（"一个端口只给一个站点独占"），把镜像站这种
//  "同端口多域名"的正常用法挡死，用户只能绕道建反代规则。nginx 本身支持
//  同端口多 server_name，所以判据收紧到：域名/别名重复、反代规则同域名、
//  被非 nginx 进程占用。80 行为保持不变（本来就允许多站点）。
// ============================================================================

func seedPortSite(t *testing.T, srv *Server, domain, aliases string, port int) {
	t.Helper()
	st := &sites.Site{
		Domain: domain, Aliases: aliases, Rewrite: "none", ListenPort: port, Enabled: true,
		Root: filepath.Join(srv.Cfg.WWWRoot, domain),
	}
	if err := srv.siteMgr().Create(context.Background(), st); err != nil {
		t.Fatalf("建站点 %s 失败: %v", domain, err)
	}
}

// stubPortHolders 注入"端口被谁占着"的探针（单测不许调提权助手/碰真实端口）。
func stubPortHolders(t *testing.T, holders string, err error) {
	t.Helper()
	prev := sitePortHoldersFn
	sitePortHoldersFn = func(context.Context, int) (string, error) { return holders, err }
	t.Cleanup(func() { sitePortHoldersFn = prev })
}

// TestSiteListenPortAllowsSamePortDifferentDomains：同端口不同域名 = 允许。
func TestSiteListenPortAllowsSamePortDifferentDomains(t *testing.T) {
	srv, _ := newTestServer(t)
	stubPortHolders(t, "", nil)
	seedPortSite(t, srv, "sp-a.test", "", 18098)

	if err := srv.validateSiteListenPort(context.Background(), 18098, "sp-b.test", ""); err != nil {
		t.Fatalf("同端口不同域名必须允许（nginx 支持多 server_name），实际被拒: %v", err)
	}
}

// TestSiteListenPortRejectsDomainOverlap：同端口下域名/别名重复 = 拒绝且指出是谁。
func TestSiteListenPortRejectsDomainOverlap(t *testing.T) {
	srv, _ := newTestServer(t)
	stubPortHolders(t, "", nil)
	seedPortSite(t, srv, "ov-a.test", "shared-alias.test", 18099)

	err := srv.validateSiteListenPort(context.Background(), 18099, "ov-b.test", "shared-alias.test")
	if err == nil {
		t.Fatal("同端口下别名撞上已有站点的别名必须被拒")
	}
	if msg := err.Error(); !strings.Contains(msg, "ov-a.test") || !strings.Contains(msg, "域名重复") {
		t.Errorf("拒绝原因必须指出重复的域名与占用它的站点，实际: %s", msg)
	}
	// 新站点主域名撞上已有站点的别名，同样是真冲突
	err = srv.validateSiteListenPort(context.Background(), 18099, "shared-alias.test", "")
	if err == nil {
		t.Fatal("新站点主域名撞上已有站点的别名必须被拒")
	}
	// 互不冲突则放行
	if err := srv.validateSiteListenPort(context.Background(), 18099, "ov-c.test", "ov-alias.test"); err != nil {
		t.Fatalf("域名互不冲突时应放行，实际被拒: %v", err)
	}
}

// TestSiteListenPortRejectsNonNginxHolder：端口被非 nginx 进程占用 = 拒绝；被 nginx 占用 = 放行。
func TestSiteListenPortRejectsNonNginxHolder(t *testing.T) {
	srv, _ := newTestServer(t)
	stubPortHolders(t, "otherd (pid 4242)", nil)

	err := srv.validateSiteListenPort(context.Background(), 18097, "occ.test", "")
	if err == nil {
		t.Fatal("端口被非 nginx 进程占用必须被拒（nginx 绑不上）")
	}
	if msg := err.Error(); !strings.Contains(msg, "otherd") || !strings.Contains(msg, "占用") {
		t.Errorf("拒绝原因必须指出是谁占用了，实际: %s", msg)
	}

	// nginx 正在听这个端口是正常状态（已有站点就在上面），必须放行。
	stubPortHolders(t, "nginx (pid 1)", nil)
	if err := srv.validateSiteListenPort(context.Background(), 18097, "occ2.test", ""); err != nil {
		t.Fatalf("nginx 占用的端口必须放行（同端口加站点就是加 server 块），实际被拒: %v", err)
	}
	// 探针拿不到结论时不拦（生产走助手，读不到就交给 nginx -t/reload 如实失败）。
	stubPortHolders(t, "", context.DeadlineExceeded)
	if err := srv.validateSiteListenPort(context.Background(), 18097, "occ3.test", ""); err != nil {
		t.Fatalf("探测失败不该拦下合法操作，实际被拒: %v", err)
	}
}

// TestSiteListenPortRejectsProxySameDomain：同端口反代规则服务同一域名 = 拒绝。
func TestSiteListenPortRejectsProxySameDomain(t *testing.T) {
	srv, _ := newTestServer(t)
	stubPortHolders(t, "", nil)
	seedProxyRule(t, srv, &proxies.Rule{
		Name: "同域规则", Listen: 18096, Domains: "proxy-clash.test", Enabled: true,
		Target: "http://127.0.0.1:9001",
	})

	err := srv.validateSiteListenPort(context.Background(), 18096, "proxy-clash.test", "")
	if err == nil {
		t.Fatal("同端口下反代规则已服务同一域名必须被拒（真冲突）")
	}
	if msg := err.Error(); !strings.Contains(msg, "同域规则") || !strings.Contains(msg, "域名重复") {
		t.Errorf("拒绝原因必须指出是哪条规则，实际: %s", msg)
	}
	// 规则服务别的域名时，同端口可以建站点
	if err := srv.validateSiteListenPort(context.Background(), 18096, "proxy-ok.test", ""); err != nil {
		t.Fatalf("规则服务别的域名时同端口应放行，实际被拒: %v", err)
	}
}

// TestSiteListenPort80Regression：80 端口行为保持原样（多站点共享，不参与冲突判定）。
func TestSiteListenPort80Regression(t *testing.T) {
	srv, _ := newTestServer(t)
	stubPortHolders(t, "otherd (pid 4242)", nil) // 80 上也不该被占用探针拦
	seedPortSite(t, srv, "eighty-a.test", "eighty-shared.test", 80)

	if err := srv.validateSiteListenPort(context.Background(), 80, "eighty-b.test", ""); err != nil {
		t.Fatalf("80 必须允许多站点共享，实际被拒: %v", err)
	}
	// 80 是共享端口，连别名重复也不在这里判（保持原行为；域名唯一性仍由 Manager 管）
	if err := srv.validateSiteListenPort(context.Background(), 80, "eighty-c.test", "eighty-shared.test"); err != nil {
		t.Fatalf("80 的行为必须与改动前逐字一致（直接放行），实际被拒: %v", err)
	}
}

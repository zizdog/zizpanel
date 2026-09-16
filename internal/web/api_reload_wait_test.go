package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  "写盘 → reload → 立即复核"这条通道的等待与回滚回归测试
//
//  真机缺陷（2026-09-16 mini，反复复现）：
//    `nginx -s reload` 是**异步**的。写完配置立刻探测时，旧 worker 还在用旧配置
//    应答（404/000），于是面板报"配置已写入、reload 成功，但复核发现新配置没有
//    生效"，而配置其实是对的 —— 重试即成功。更糟的是失败会留下写入残留
//    （空数据库 / store 里 ssl_enabled=true），让重试被自己挡住。
//
//  这里锁死三件事：
//    1. 窗口内的"旧配置特征"（404/000）**不算失败**，要轮询到新配置生效；
//    2. 只有 reload 命令本身失败、或等满窗口仍不生效，才判失败；
//    3. 判失败必须撤销本次写入（vhost / store 字段 / 空库），使重试干净。
//
//  所有测试都在 newTestServer 的沙箱里：不碰真实 nginx、不真发请求、
//  等待窗口压到几十毫秒（不许真睡 6 秒）。
// ============================================================================

// siteApplyStub 记录复核通道上的调用，供断言"重试了几次 / 回滚做了什么"。
type siteApplyStub struct {
	mu      sync.Mutex
	writes  []string // 每次写入的内容（按顺序）
	deletes int      // 回滚时删除 vhost 的次数
	probes  int
	code    string // 探针当前返回的状态码
}

func (st *siteApplyStub) setCode(code string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.code = code
}

func (st *siteApplyStub) probeCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.probes
}

func (st *siteApplyStub) writeCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.writes)
}

func (st *siteApplyStub) lastWrite() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.writes) == 0 {
		return ""
	}
	return st.writes[len(st.writes)-1]
}

// stubSiteApplyChannel 把站点复核通道换成毫秒级假实现。
// 默认"旧 vhost 不存在"（回滚走删除分支），需要还原旧内容时测试自行覆盖 siteReadVhostFn。
func stubSiteApplyChannel(t *testing.T, code string) *siteApplyStub {
	t.Helper()
	st := &siteApplyStub{code: code}
	prevWrite, prevReload, prevProbe := siteWriteVhostFn, siteReloadFn, siteProbeFn
	prevRead, prevDelete := siteReadVhostFn, siteDeleteVhostFn
	prevWait, prevEvery := siteVerifyWait, siteVerifyEvery

	siteWriteVhostFn = func(_ *Server, _ context.Context, _, content string) error {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.writes = append(st.writes, content)
		return nil
	}
	siteReloadFn = func(_ *Server, _ context.Context) error { return nil }
	siteProbeFn = func(_ context.Context, _, _ string, _ int, _ string, _ time.Duration) (string, string, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.probes++
		return st.code, "", nil
	}
	siteReadVhostFn = func(_ *Server, _ string) ([]byte, error) { return nil, os.ErrNotExist }
	siteDeleteVhostFn = func(_ *Server, _ context.Context, _ string) error {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.deletes++
		return nil
	}
	siteVerifyWait = 80 * time.Millisecond
	siteVerifyEvery = time.Millisecond

	t.Cleanup(func() {
		siteWriteVhostFn, siteReloadFn, siteProbeFn = prevWrite, prevReload, prevProbe
		siteReadVhostFn, siteDeleteVhostFn = prevRead, prevDelete
		siteVerifyWait, siteVerifyEvery = prevWait, prevEvery
	})
	return st
}

// TestApplySitePollsUntilReloadTakesEffect 是真机缺陷的直接回归：
// 前几次探测看到旧配置（404）绝不能判失败，新配置一接管就必须通过。
func TestApplySitePollsUntilReloadTakesEffect(t *testing.T) {
	srv, _ := newTestServer(t)
	st := stubSiteApplyChannel(t, "404")
	siteVerifyWait = 3 * time.Second // 给足重试窗口（但每次探测都是毫秒级假实现）
	siteProbeFn = func(_ context.Context, _, _ string, _ int, _ string, _ time.Duration) (string, string, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.probes++
		if st.probes <= 2 {
			return "404", "", nil // 旧 worker 还在应答
		}
		return "403", "", nil // 新 worker 接管
	}

	site := &sites.Site{Domain: "slow-reload.test", Root: filepath.Join(srv.Cfg.WWWRoot, "slow-reload.test"),
		Enabled: true, Rewrite: "none"}
	if err := os.MkdirAll(site.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := srv.applySite(context.Background(), site); err != nil {
		t.Fatalf("旧配置还在应答只是『还没生效』，必须重试到成功，实际：%v", err)
	}
	if st.probeCount() < 3 {
		t.Fatalf("应当至少探测 3 次，实际 %d 次", st.probeCount())
	}
	if st.writeCount() != 1 {
		t.Fatalf("成功路径不该重复写 vhost（更不该回滚重写），实际写入 %d 次", st.writeCount())
	}
}

// TestApplySiteTimeoutRollsBackAndReportsChecklist：等满窗口仍不生效才判失败，
// 且失败信息必须能指导排查（nginx -t / pid 权限 / include 目录 / error_log），
// 同时撤销本次写入。
func TestApplySiteTimeoutRollsBackAndReportsChecklist(t *testing.T) {
	srv, _ := newTestServer(t)
	st := stubSiteApplyChannel(t, "404")

	site := &sites.Site{Domain: "stuck.test", Root: filepath.Join(srv.Cfg.WWWRoot, "stuck.test"),
		Enabled: true, Rewrite: "none"}
	err := srv.applySite(context.Background(), site)
	if err == nil {
		t.Fatal("等满窗口仍不生效必须判失败（否则就是谎报成功）")
	}
	msg := err.Error()
	for _, want := range []string{
		"nginx 重载也已发出", "内新配置仍未生效", "秒",
		"nginx -t", "nginx.pid", "include", "error_log",
		filepath.Join(srv.Cfg.VhostDir, "stuck.test.conf"),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("超时错误里应包含 %q，便于用户排查；实际：%s", want, msg)
		}
	}
	if st.deletes != 1 {
		t.Fatalf("复核失败必须撤销本次写入的 vhost（本次是新建 → 删除），实际删除 %d 次", st.deletes)
	}
}

// TestApplySiteFailureRestoresPreviousVhost：覆盖已有 vhost 时失败，
// 必须把**写入前的内容**原样写回，而不是留着新的、更不能删掉。
func TestApplySiteFailureRestoresPreviousVhost(t *testing.T) {
	srv, _ := newTestServer(t)
	st := stubSiteApplyChannel(t, "404")
	const old = "# 旧配置，必须原样还原\nserver { listen 80; }\n"
	siteReadVhostFn = func(_ *Server, _ string) ([]byte, error) { return []byte(old), nil }
	prevDelete := siteDeleteVhostFn
	siteDeleteVhostFn = func(_ *Server, _ context.Context, _ string) error {
		t.Error("已有 vhost 失败时不该删除它")
		return nil
	}
	t.Cleanup(func() { siteDeleteVhostFn = prevDelete })

	site := &sites.Site{Domain: "restore.test", Root: filepath.Join(srv.Cfg.WWWRoot, "restore.test"),
		Enabled: true, Rewrite: "none"}
	if err := srv.applySite(context.Background(), site); err == nil {
		t.Fatal("复核超时必须判失败")
	}
	if st.writeCount() != 2 {
		t.Fatalf("应写两次（新配置 + 还原旧配置），实际 %d 次", st.writeCount())
	}
	if st.lastWrite() != old {
		t.Fatalf("最后一次写入必须是旧配置（回滚），实际：%q", st.lastWrite())
	}
}

// TestApplySiteReloadErrorRollsBack：reload 命令本身失败属于（a）类失败，
// 同样要回滚，且错误里要写明"重载失败"。
func TestApplySiteReloadErrorRollsBack(t *testing.T) {
	srv, _ := newTestServer(t)
	st := stubSiteApplyChannel(t, "403")
	siteReloadFn = func(_ *Server, _ context.Context) error { return fmt.Errorf("nginx -s reload boom") }

	site := &sites.Site{Domain: "reload-boom.test", Root: filepath.Join(srv.Cfg.WWWRoot, "reload-boom.test"),
		Enabled: true, Rewrite: "none"}
	err := srv.applySite(context.Background(), site)
	if err == nil || !strings.Contains(err.Error(), "重载失败") {
		t.Fatalf("reload 失败必须如实上报，实际：%v", err)
	}
	if st.deletes != 1 {
		t.Fatalf("reload 失败也必须撤销本次写入，实际删除 %d 次", st.deletes)
	}
}

// TestSiteCreateEndpointRetrySucceedsAfterVerifyFailure 是缺陷 "用户再点一次
// 能干净地重试成功" 的端到端护栏：第一次复核超时 → 非 2xx + 记录/vhost 都回滚；
// 第二次（新配置已生效）→ 直接成功。
func TestSiteCreateEndpointRetrySucceedsAfterVerifyFailure(t *testing.T) {
	srv, ts := newTestServer(t)
	st := stubSiteApplyChannel(t, "404")
	cookies := loginTestPanel(t, ts)

	body := map[string]any{"domain": "retry.test", "rewrite": "none"}
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", body, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("复核超时时不能返回 2xx: %v", out)
	}
	if _, err := srv.siteMgr().Get(context.Background(), "retry.test"); err == nil {
		t.Fatal("失败后站点记录必须被回滚，否则用户重试会被『站点已存在』挡住")
	}
	if st.deletes == 0 {
		t.Fatal("失败后本次写入的 vhost 必须被撤销")
	}

	// 第二次：nginx 已经加载了新配置（说明第一次只是探测太早）。
	st.setCode("403")
	res2, out2, _ := doJSON(t, ts, "POST", "/api/v1/sites", body, cookies)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("残留已清理，重试必须干净成功，实际 %d: %v", res2.StatusCode, out2)
	}
	if _, err := srv.siteMgr().Get(context.Background(), "retry.test"); err != nil {
		t.Fatalf("重试成功后站点记录应存在：%v", err)
	}
}

// TestSiteSSLEndpointRollsBackStoreAndVhost：SSL 绑定失败时，
// store 里的 ssl_enabled/证书字段必须退回原样，磁盘 vhost 也要还原 ——
// 这正是真机"store 说 ssl_enabled=true、nginx 却没在服务"的不一致。
func TestSiteSSLEndpointRollsBackStoreAndVhost(t *testing.T) {
	srv, ts := newTestServer(t)
	seedSite(t, srv, "ssl-rollback.test")
	st := stubSiteApplyChannel(t, "404")

	const old = "# 未启用 SSL 的旧配置\nserver { listen 80; }\n"
	siteReadVhostFn = func(_ *Server, _ string) ([]byte, error) { return []byte(old), nil }

	cookies := loginTestPanel(t, ts)
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites/ssl-rollback.test/ssl", map[string]any{
		"provider": "manual",
		"cert":     "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n",
		"key":      "-----BEGIN PRIVATE KEY-----\nZmFrZQ==\n-----END PRIVATE KEY-----\n",
	}, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("SSL 复核超时必须返回非 2xx: %v", out)
	}

	got, err := srv.siteMgr().Get(context.Background(), "ssl-rollback.test")
	if err != nil {
		t.Fatalf("站点记录不该被删：%v", err)
	}
	if got.SSLEnabled || got.SSLCert != "" || got.SSLKey != "" {
		t.Fatalf("失败后 store 里的 SSL 字段必须回滚，实际 enabled=%v cert=%q key=%q",
			got.SSLEnabled, got.SSLCert, got.SSLKey)
	}
	if st.lastWrite() != old {
		t.Fatalf("失败后 vhost 必须还原成旧内容，实际：%q", st.lastWrite())
	}
}

// TestDefaultVhostWaitsAndRollsBack：默认站点同一套"写盘 → reload → 等一下 → 复核"。
func TestDefaultVhostWaitsAndRollsBack(t *testing.T) {
	t.Run("旧内容还在时不算失败", func(t *testing.T) {
		srv, _ := newTestServer(t)
		st := stubSiteApplyChannel(t, "403")
		prevProbe, prevWait, prevEvery := defaultProbeFn, defaultVerifyWait, defaultVerifyEvery
		probes := 0
		defaultProbeFn = func(string) (int, string) {
			probes++
			if probes <= 2 {
				return http.StatusNotFound, "旧默认站点" // 旧配置还在应答
			}
			return http.StatusOK, "<html>" + sites.LocalhostIndexMarker + "</html>"
		}
		defaultVerifyWait, defaultVerifyEvery = 3*time.Second, time.Millisecond
		t.Cleanup(func() {
			defaultProbeFn, defaultVerifyWait, defaultVerifyEvery = prevProbe, prevWait, prevEvery
		})

		if err := srv.applyDefaultVhost(context.Background()); err != nil {
			t.Fatalf("旧配置还在应答只是『还没生效』，不该判失败：%v", err)
		}
		if probes < 3 {
			t.Fatalf("应当至少探测 3 次，实际 %d 次", probes)
		}
		if st.writeCount() != 1 {
			t.Fatalf("成功路径不该重复写 000-default，实际 %d 次", st.writeCount())
		}
	})

	t.Run("超时后还原旧的 000-default", func(t *testing.T) {
		srv, _ := newTestServer(t)
		st := stubSiteApplyChannel(t, "403")
		prevProbe, prevWait, prevEvery := defaultProbeFn, defaultVerifyWait, defaultVerifyEvery
		const old = "# 旧默认站点，必须还原\n"
		siteReadVhostFn = func(_ *Server, _ string) ([]byte, error) { return []byte(old), nil }
		defaultProbeFn = func(string) (int, string) { return http.StatusNotFound, "旧默认站点" }
		defaultVerifyWait, defaultVerifyEvery = 60*time.Millisecond, time.Millisecond
		t.Cleanup(func() {
			defaultProbeFn, defaultVerifyWait, defaultVerifyEvery = prevProbe, prevWait, prevEvery
		})

		err := srv.applyDefaultVhost(context.Background())
		if err == nil {
			t.Fatal("超时后必须判失败")
		}
		for _, want := range []string{"nginx 重载也已发出", "nginx -t", "error_log", "000-default"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误信息应包含 %q，实际：%v", want, err)
			}
		}
		if st.lastWrite() != old {
			t.Fatalf("默认站点失败后必须还原旧内容，实际：%q", st.lastWrite())
		}
	})
}

// TestProxyApplyFailureRestoresPreviousVhost：反代侧同样"失败即回滚"，
// 且回滚写回的是写入前的内容（不是留着失败的新配置）。
func TestProxyApplyFailureRestoresPreviousVhost(t *testing.T) {
	srv := newProxyTestServer(t)
	h := stubProxyHooks(t)
	rule := testProxyRule(21, 18090, "")
	const old = "# 旧反代配置\nserver { listen 18090; }\n"

	var written []string
	proxyWriteVhostFn = func(_ *Server, _ context.Context, _, content string) error {
		h.Order = append(h.Order, "write")
		written = append(written, content)
		return nil
	}
	// 旧内容存在于沙箱 vhost 目录里：失败回滚必须读它、写回它。
	if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vhost := filepath.Join(srv.Cfg.VhostDir, rule.VhostName()+".conf")
	if err := os.WriteFile(vhost, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		h.Order = append(h.Order, "probe")
		return "000", "", fmt.Errorf("Connection refused") // 旧配置/没加载
	}

	err := srv.applyProxy(context.Background(), rule)
	if err == nil {
		t.Fatal("复核超时必须返回错误")
	}
	if len(written) != 2 || written[1] != old {
		t.Fatalf("失败后必须把旧反代配置写回，实际写入 %d 次，最后一次：%q", len(written), written[len(written)-1])
	}
}

// TestRemoveProxyConfigRestoresFileWhenStillServed：删除规则时如果复核发现
// "它仍在生效"，必须把文件还原 —— 否则记录还在、配置却没了，是最典型的不一致。
func TestRemoveProxyConfigRestoresFileWhenStillServed(t *testing.T) {
	srv := newProxyTestServer(t)
	h := stubProxyHooks(t)
	rule := testProxyRule(22, 18091, "")
	const old = "# 仍在生效的反代配置\nserver { listen 18091; }\n"
	if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vhost := filepath.Join(srv.Cfg.VhostDir, rule.VhostName()+".conf")
	if err := os.WriteFile(vhost, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	prevDelete := siteDeleteVhostFn
	siteDeleteVhostFn = func(s *Server, _ context.Context, name string) error {
		return os.Remove(filepath.Join(s.Cfg.VhostDir, name+".conf"))
	}
	t.Cleanup(func() { siteDeleteVhostFn = prevDelete })
	// 回滚写回走的是 proxyWriteVhostFn（与写入同一条通道），这里让它真的落盘，
	// 才能断言"文件被还原了"。
	proxyWriteVhostFn = func(s *Server, _ context.Context, name, content string) error {
		h.Order = append(h.Order, "write")
		return os.WriteFile(filepath.Join(s.Cfg.VhostDir, name+".conf"), []byte(content), 0o644)
	}

	// 每次探测都让该规则自己的访问日志增长 → 说明它仍在被 nginx 使用。
	logPath := proxyAccessLogPath(srv.proxyLogDir(), rule.ID)
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		h.Order = append(h.Order, "probe")
		appendToFile(t, logPath)
		return "200", "still-here", nil
	}

	err := srv.removeProxyConfig(context.Background(), rule)
	if err == nil {
		t.Fatal("规则仍在生效时删除必须失败")
	}
	back, rerr := os.ReadFile(vhost)
	if rerr != nil {
		t.Fatalf("失败后应把配置文件还原回来：%v", rerr)
	}
	if string(back) != old {
		t.Fatalf("还原的内容不对：%q", string(back))
	}
}

package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizpanel/internal/priv"
)

// TestEnsureNginxRuntimeDirsOwnerDiscipline 锁"临时目录属主只能来自运行体"。
//
// 用户报障（推音色样本 → nginx 自己的 500 HTML 页）暴露的规则：
// 临时目录属主不对时 worker 写不进去，症状与"目录不存在"完全一样。所以
//
//	① 属主判不出来（没有 worker、nginx.conf 里也没有 user）→ 建目录但**一次都不 chown**，
//	   绝不猜 nobody（猜错=没修，还把真正的原因藏起来）；
//	② 判得出来 → 必须按运行体给的 uid/gid 改。
func TestEnsureNginxRuntimeDirsOwnerDiscipline(t *testing.T) {
	srv, _ := newTestServer(t)
	seedFakeNginx(t, srv)
	t.Setenv("ZIZPANEL_BREW_PREFIX", srv.Cfg.BrewPrefix)
	prevEuid := defaultSiteEuid
	defaultSiteEuid = func() int { return 0 }
	t.Cleanup(func() { defaultSiteEuid = prevEuid })
	prevChown := runtimeDirChownFn
	t.Cleanup(func() { runtimeDirChownFn = prevChown })

	dir := filepath.Join(srv.Cfg.BrewPrefix, "var", "run", "nginx")

	// ① 属主未知：不许 chown
	restore := priv.SetNginxWorkerProbeForTest(func() (int, int, int, bool) { return 0, 0, 0, false })
	defer restore()
	conf := filepath.Join(srv.Cfg.BrewPrefix, "etc", "nginx", "nginx.conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("worker_processes auto;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var gotUID, gotGID int
	runtimeDirChownFn = func(string, int, int) error { calls++; return nil }
	srv.ensureNginxRuntimeDirs()
	if calls != 0 {
		t.Errorf("属主未知时绝不许 chown（猜错等于没修），实际调用了 %d 次", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "client_body_temp")); err != nil {
		t.Errorf("临时目录仍然要建出来（否则 nginx 一样报 500）：%v", err)
	}

	// ② 属主已知（运行体给出）：必须照它改
	restore2 := priv.SetNginxWorkerProbeForTest(func() (int, int, int, bool) { return 777, 20, 4321, true })
	defer restore2()
	calls = 0
	runtimeDirChownFn = func(_ string, uid, gid int) error { calls++; gotUID, gotGID = uid, gid; return nil }
	srv.ensureNginxRuntimeDirs()
	if calls == 0 {
		t.Fatal("属主已知时必须 chown（否则 worker 写不进去，大请求体还是 500）")
	}
	if gotUID != 777 || gotGID != 20 {
		t.Errorf("chown 必须用运行体给出的 uid/gid，实际 %d/%d", gotUID, gotGID)
	}
}

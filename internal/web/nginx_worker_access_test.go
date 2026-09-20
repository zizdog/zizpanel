package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  写 vhost 前必须验证 root 真的能被 nginx 读到（坑 212）
//
//  2026-09-20 事故：root 指向外置盘、nginx 没有 TCC 授权 ⇒ 唯一 worker 卡死。
//  门禁：不可读 ⇒ 不写 vhost、不 reload、返回明确原因与出路；可读 ⇒ 正常写入。
// ============================================================================

// stubNginxReadPrecheck 注入"worker 读目录"的判据，返回一个可改写的错误开关。
func stubNginxReadPrecheck(t *testing.T, fail *error) {
	t.Helper()
	prev := nginxWorkerReadDirFn
	t.Cleanup(func() { nginxWorkerReadDirFn = prev })
	nginxWorkerReadDirFn = func(context.Context, string, int, int) error { return *fail }
}

func TestSiteApplyRefusesUnreadableRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	st := stubSiteApplyChannel(t, "403")
	root := filepath.Join(srv.Cfg.WWWRoot, "unreadable.test")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	readErr := errors.New("ls: /Volumes/X/mirror: Operation not permitted")
	stubNginxReadPrecheck(t, &readErr)
	reloads := 0
	prevReload := siteReloadFn
	siteReloadFn = func(*Server, context.Context) error { reloads++; return nil }
	t.Cleanup(func() { siteReloadFn = prevReload })

	site := &sites.Site{Domain: "unreadable.test", Root: root, Enabled: true, Rewrite: "none"}
	err := srv.applySite(context.Background(), site)
	if err == nil {
		t.Fatal("nginx 读不到 root 时必须拒绝写入")
	}
	msg := err.Error()
	first := strings.SplitN(msg, "\n", 2)[0]
	if !strings.Contains(first, "已拒绝写入") || len([]rune(first)) > 40 {
		t.Errorf("首行应当是 ≤40 字的拒绝结论，实际：%q", first)
	}
	for _, want := range []string{root, "chmod", "chown"} {
		if !strings.Contains(msg, want) {
			t.Errorf("拒绝原因里应给出可执行出路 %q，实际：%s", want, msg)
		}
	}
	if st.writeCount() != 0 {
		t.Errorf("不可读时一个 vhost 都不该写，实际写了 %d 次", st.writeCount())
	}
	if reloads != 0 {
		t.Errorf("不可读时不该 reload nginx，实际 %d 次", reloads)
	}
}

// TestSiteApplyUnreadableRootOnExternalVolumeMentionsAuth：外接卷路径要叠加授权判据。
func TestSiteApplyUnreadableRootOnExternalVolumeMentionsAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	stubSiteApplyChannel(t, "403")
	vol := filepath.Join(t.TempDir(), "ZPMirror")
	root := filepath.Join(vol, "mirror")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	prevMounts := nonSystemVolumeMountsFn
	nonSystemVolumeMountsFn = func() []string { return []string{vol} }
	t.Cleanup(func() { nonSystemVolumeMountsFn = prevMounts })

	readErr := errors.New("Operation not permitted")
	stubNginxReadPrecheck(t, &readErr)

	site := &sites.Site{Domain: "vol.test", Root: root, Enabled: true, Rewrite: "none"}
	err := srv.applySite(context.Background(), site)
	if err == nil {
		t.Fatal("外接卷上读不到 root 时必须拒绝写入")
	}
	if !strings.Contains(err.Error(), "外接盘授权") {
		t.Errorf("外接卷的拒绝理由要指向「磁盘」页授权，实际：%s", err)
	}
}

// TestSiteApplyReadableRootWrites：同一入口在"读得到"时必须正常写入 ——
// 负向对照：拒绝确实来自预检，而不是别的原因让这次写入失败。
func TestSiteApplyReadableRootWrites(t *testing.T) {
	srv, _ := newTestServer(t)
	st := stubSiteApplyChannel(t, "403")
	root := filepath.Join(srv.Cfg.WWWRoot, "readable.test")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	var nilErr error
	stubNginxReadPrecheck(t, &nilErr)

	site := &sites.Site{Domain: "readable.test", Root: root, Enabled: true, Rewrite: "none"}
	if err := srv.applySite(context.Background(), site); err != nil {
		t.Fatalf("可读时必须正常写入，实际：%v", err)
	}
	if st.writeCount() != 1 {
		t.Errorf("可读时应当写一次 vhost，实际 %d 次", st.writeCount())
	}
	if !strings.Contains(st.lastWrite(), root) {
		t.Errorf("写出的 vhost 应指向 root %s：\n%s", root, st.lastWrite())
	}
}

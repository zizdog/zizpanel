package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  应用失败后的分级判据（坑 211）
//
//  2026-09-20 事故的教训：失败后必须"先探再判、判完真做"，而且不许给无证据的
//  root 属主猜测。这里逐条锁住分支文案与恢复动作。
// ============================================================================

// stubHealthSeams 注入健康探测/恢复动作，返回调用计数。
func stubHealthSeams(t *testing.T) (probe, reload, restart *int) {
	t.Helper()
	var np, nr, ns int
	prevProbe, prevReload, prevRestart := nginxHealthProbeFn, nginxHealthReloadFn, nginxHealthRestartFn
	t.Cleanup(func() {
		nginxHealthProbeFn, nginxHealthReloadFn, nginxHealthRestartFn = prevProbe, prevReload, prevRestart
	})
	nginxHealthProbeFn = func(context.Context, string, int) (string, error) {
		np++
		return "200", nil
	}
	nginxHealthReloadFn = func(*Server, context.Context) error { nr++; return nil }
	nginxHealthRestartFn = func(*Server, context.Context) error { ns++; return nil }
	return &np, &nr, &ns
}

func TestClassifyApplyFailureNginxDownReloadRecovers(t *testing.T) {
	srv, _ := newTestServer(t)
	_, reload, restart := stubHealthSeams(t)

	v := srv.classifyApplyFailure(context.Background(), "http", 8080, "000", true)
	if v.Kind != applyFailNginxDown || !v.Recovered || v.Action != "reload" {
		t.Fatalf("无响应且 reload 后恢复：kind=%s recovered=%v action=%s", v.Kind, v.Recovered, v.Action)
	}
	if v.Summary != "nginx 无响应，已重载恢复" {
		t.Errorf("结论文案不对：%q", v.Summary)
	}
	if *reload != 1 {
		t.Errorf("必须真的 reload 一次，实际 %d", *reload)
	}
	if *restart != 0 {
		t.Errorf("reload 已恢复就不该重启，实际 %d", *restart)
	}
}

func TestClassifyApplyFailureRestartsWhenReloadIneffective(t *testing.T) {
	srv, _ := newTestServer(t)
	_, reload, restart := stubHealthSeams(t)
	// reload 后仍无响应；restart 后恢复。
	probes := 0
	nginxHealthProbeFn = func(context.Context, string, int) (string, error) {
		probes++
		if probes >= 2 { // 第二次 = restart 之后的复核
			return "200", nil
		}
		return "000", errors.New("no response")
	}
	nginxHealthReloadFn = func(*Server, context.Context) error { *reload++; return nil }
	nginxHealthRestartFn = func(*Server, context.Context) error { *restart++; return nil }

	v := srv.classifyApplyFailure(context.Background(), "http", 8081, "000", true)
	if !v.Recovered || v.Action != "restart" || v.Summary != "nginx 无响应，已重启恢复" {
		t.Fatalf("reload 无效后必须重启并如实说：kind=%s recovered=%v action=%s summary=%q",
			v.Kind, v.Recovered, v.Action, v.Summary)
	}
	if *reload != 1 || *restart != 1 {
		t.Errorf("应当 reload 1 次 + restart 1 次，实际 reload=%d restart=%d", *reload, *restart)
	}
}

func TestClassifyApplyFailureReportsUnrecovered(t *testing.T) {
	srv, _ := newTestServer(t)
	_, reload, restart := stubHealthSeams(t)
	nginxHealthProbeFn = func(context.Context, string, int) (string, error) {
		return "000", errors.New("still stuck")
	}
	nginxHealthReloadFn = func(*Server, context.Context) error { *reload++; return errors.New("reload boom") }
	nginxHealthRestartFn = func(*Server, context.Context) error { *restart++; return nil }

	v := srv.classifyApplyFailure(context.Background(), "http", 8082, "000", true)
	if v.Recovered {
		t.Fatal("复核仍无响应时绝不能报已恢复（那就是谎报）")
	}
	if !strings.Contains(v.Summary, "未恢复") {
		t.Errorf("结论必须如实说未恢复：%q", v.Summary)
	}
	if *reload != 1 || *restart != 1 {
		t.Errorf("重载+重启都要真的试过，实际 reload=%d restart=%d", *reload, *restart)
	}
}

func TestClassifyApplyFailureNotEffectiveAndUpstream(t *testing.T) {
	srv, _ := newTestServer(t)
	_, reload, restart := stubHealthSeams(t)

	v := srv.classifyApplyFailure(context.Background(), "http", 8083, "404", true)
	if v.Kind != applyFailNotEffective || v.Summary != "配置已写入但未生效" {
		t.Errorf("配置没生效的结论不对：kind=%s summary=%q", v.Kind, v.Summary)
	}
	if *reload != 0 || *restart != 0 {
		t.Errorf("配置没生效不该做恢复动作（reload=%d restart=%d）", *reload, *restart)
	}

	u := srv.classifyApplyFailure(context.Background(), "http", 8084, "502", true)
	if u.Kind != applyFailUpstreamDown || !strings.Contains(u.Summary, "目标无响应") {
		t.Errorf("上游 502 应当单独归为「目标无响应」：kind=%s summary=%q", u.Kind, u.Summary)
	}
	if *reload != 0 || *restart != 0 {
		t.Errorf("上游没响应不是 nginx 的问题，不该动 nginx（reload=%d restart=%d）", *reload, *restart)
	}
}

func TestClassifyApplyFailureProbesWhenNotProbed(t *testing.T) {
	srv, _ := newTestServer(t)
	probe, _, _ := stubHealthSeams(t)
	nginxHealthProbeFn = func(context.Context, string, int) (string, error) { (*probe)++; return "000", nil }

	v := srv.classifyApplyFailure(context.Background(), "http", 8085, "", false)
	if *probe == 0 {
		t.Error("调用方还没探过时必须先补一次有界探测")
	}
	if v.Kind != applyFailNginxDown {
		t.Errorf("探测无响应应归为 nginx_down，实际 %s", v.Kind)
	}
}

func TestRootOwnedHintNeedsRealEvidence(t *testing.T) {
	srv, _ := newTestServer(t)
	prev := nginxRootOwnedFilesFn
	t.Cleanup(func() { nginxRootOwnedFilesFn = prev })

	nginxRootOwnedFilesFn = func(*Server) []string { return nil }
	if got := srv.rootOwnedNginxHint(); got != "" {
		t.Errorf("没有 root 属主文件时不得给这条提示，实际：%q", got)
	}

	nginxRootOwnedFilesFn = func(*Server) []string { return []string{"/tmp/logs/x.access.log"} }
	got := srv.rootOwnedNginxHint()
	if !strings.Contains(got, "/tmp/logs/x.access.log") {
		t.Errorf("发现 root 属主文件时必须给出实际路径，实际：%q", got)
	}
}

// TestSiteVerifyFailureClassifiedAndNoRootGuess：站点复核失败的错误首行是分类结论；
// 没有 root 属主文件时不得出现"属主是 root"。
func TestSiteVerifyFailureClassifiedAndNoRootGuess(t *testing.T) {
	srv, _ := newTestServer(t)
	prev := nginxRootOwnedFilesFn
	t.Cleanup(func() { nginxRootOwnedFilesFn = prev })
	nginxRootOwnedFilesFn = func(*Server) []string { return nil }

	site := &sites.Site{Domain: "cls.test", Root: "/tmp/cls.test", Enabled: true}
	err := srv.siteVerifyTimeoutError(context.Background(), site, "http", 8086, 3, "404", nil)
	msg := err.Error()
	if first := strings.SplitN(msg, "\n", 2)[0]; first != "配置已写入但未生效" {
		t.Errorf("首行应当是分类结论，实际：%q", first)
	}
	if strings.Contains(msg, "属主是 root") || strings.Contains(msg, "日志属主") {
		t.Errorf("没有 root 属主文件时不得出现该猜测：%s", msg)
	}
	// 细节仍在（可折叠），不能因为分类把排查信息丢了。
	for _, want := range []string{"nginx -t", "include", "error_log"} {
		if !strings.Contains(msg, want) {
			t.Errorf("细节里应保留 %q，实际：%s", want, msg)
		}
	}
}

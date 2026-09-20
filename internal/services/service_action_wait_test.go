package services

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  服务动作快路径的门禁（坑 225）
//
//  现象：面板重启服务要等约 10s，命令行同级操作 <0.1s。根因是动作后的就绪等待
//  写了"先固定睡 500ms、最多 16 次"（8s），而真正等服务的时间远小于它。
//  这里锁死两件事：立即就绪不得引入固定等待；等待必须有界、读不到就如实说未确认。
// ============================================================================

// 门禁：立即就绪时**一次探测即返回**，不得有任何预等。
func TestWaitServiceStateReturnsImmediatelyWhenReady(t *testing.T) {
	calls := 0
	start := time.Now()
	st, ok := waitServiceState(context.Background(), func(context.Context) (State, error) {
		calls++
		return State{Status: "running", Running: true}, nil
	}, true)
	cost := time.Since(start)
	if !ok || !st.Running {
		t.Fatalf("立即就绪应判为已就绪，实际 ok=%v state=%+v", ok, st)
	}
	if calls != 1 {
		t.Fatalf("立即就绪应只探测一次，实际 %d 次（说明存在预等/重复探测）", calls)
	}
	if cost > 100*time.Millisecond {
		t.Fatalf("立即就绪不得引入固定等待，实际耗时 %s（旧实现先睡 500ms）", cost)
	}
}

// 门禁：一直不就绪时等待必须有界，并返回最后一次真实观测。
func TestWaitServiceStateIsBounded(t *testing.T) {
	prev := serviceActionWaitCap
	serviceActionWaitCap = 400 * time.Millisecond // 别让单测真等 3 秒
	t.Cleanup(func() { serviceActionWaitCap = prev })

	calls := 0
	start := time.Now()
	st, ok := waitServiceState(context.Background(), func(context.Context) (State, error) {
		calls++
		return State{Status: "stopped", Detail: "进程还没起来"}, nil
	}, true)
	cost := time.Since(start)
	if ok {
		t.Fatal("一直未就绪时不该判为已就绪")
	}
	if st.Status != "stopped" {
		t.Fatalf("应返回最后一次真实观测，实际 %+v", st)
	}
	if calls < 2 {
		t.Fatalf("应细粒度轮询多次，实际只探测 %d 次", calls)
	}
	if cost < serviceActionWaitCap || cost > serviceActionWaitCap+time.Second {
		t.Fatalf("等待应贴近上限 %s，实际 %s", serviceActionWaitCap, cost)
	}
}

// 门禁：新的就绪等待预算必须仍是秒级（≤5s）且步长细粒度（≤200ms）。
// 谁把它改回"睡 1s、等 30s"就红。
func TestServiceActionWaitBudget(t *testing.T) {
	if serviceActionWaitCap > 5*time.Second || serviceActionWaitCap < time.Second {
		t.Fatalf("就绪等待上限应为 1~5s，实际 %s", serviceActionWaitCap)
	}
	if serviceActionPollStep <= 0 || serviceActionPollStep > 200*time.Millisecond {
		t.Fatalf("就绪轮询步长应为 (0,200ms]，实际 %s", serviceActionPollStep)
	}
}

// 门禁：动作快路径只做针对性探测 —— 不得调用全局昂贵探测，也不许固定 sleep。
// 昂贵探测（brew services list / 全量 ListServices）放动作路径就是"面板比命令行慢
// 一个数量级"的复发点（坑 165/193）。
func TestServiceActionFastPathHasNoGlobalProbe(t *testing.T) {
	forbidden := []string{
		"m.List(", "ListServices", "BrewCapture",
		`"services", "list"`, "brew services list",
	}
	bodies := map[string]string{
		"services.go Action":           mustFuncBody(t, "services.go", "func (m *Manager) Action("),
		"services.go waitServiceState": mustFuncBody(t, "services.go", "func waitServiceState("),
		"native.go Restart":            mustFuncBody(t, "native.go", "func (d *nativeDriver) Restart("),
	}
	for name, body := range bodies {
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s 出现全局昂贵探测 %q（坑 165/193）", name, bad)
			}
		}
	}
	if body := bodies["services.go Action"]; strings.Contains(body, "time.Sleep") {
		t.Error("Action 不得有固定 sleep：就绪即返回，超时如实说未确认（坑 225）")
	}
	// 先 LaunchLoad（对已加载作业会 kick）再 LaunchKickstart = 连发两次 kickstart，
	// 第二次撞 launchd 的 10s 节流窗口（真机实测 10.0s）。
	if body := bodies["native.go Restart"]; strings.Contains(body, "LaunchLoad") {
		t.Error("nativeDriver.Restart 不得先 LaunchLoad：会与 LaunchKickstart 叠成两次 kickstart（坑 225）")
	}
}

// mustFuncBody 取出 path 里以 sig 开头那个顶层函数的源码（到下一个顶层 func 为止）。
// 源码扫描门禁用它：只看被测函数，不误伤同文件里的其它函数。
func mustFuncBody(t *testing.T, path, sig string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	src := string(b)
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("%s 里找不到函数 %q", path, sig)
	}
	rest := src[i:]
	if j := strings.Index(rest[1:], "\nfunc "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

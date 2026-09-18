package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  卸载 python@3.13 的真机流程（2026-09-21 用户原文）
//
//  brew uninstall python@3.13 →
//    Error: Refusing to uninstall … because it is required by llvm and rust …
//    You can override this and force removal with:
//      brew uninstall --ignore-dependencies python@3.13
//
//  以前面板把这段英文原文整段贴回给用户，界面上**只有一条死路**。
//  现在接口层的契约是：
//    ① 没带 force（默认）→ 409，文案点名 llvm/rust 并给出两个选择；
//    ② force=1 且计划允许   → 202 + task_id（任务被创建，真正的卸载在任务里）；
//    ③ force=1 但计划不允许 → 400（绝不因为多一个参数就加破坏性开关）。
//
//  这个测试**全部走桩**：brew 是一个记录调用的假脚本，永远不会真的卸载任何东西。
//  它同时锁住 "brew uses --installed" 真的被调用过（依赖检测不是摆设）。
// ============================================================================

// fakeBrewForUninstall 在沙箱前缀里放一个假 brew：
//   - `list --versions <f>` → 报告 php 8.4.7 装着（供计划判定）；
//   - 其它调用只记一笔（绝不真跑）。
//
// 返回记录文件的路径。
//
// ⚠️ `brew uses --installed` **不走这里**：newTestServer 已经把依赖探测钉成桩
// （stubBrewUsesProbe，单测绝不跑真实 brew —— 它可能挂十几分钟）。
// 需要"有人依赖它"的用例自己用 stubBrewUsesDependents 覆盖。
func fakeBrewForUninstall(t *testing.T, srv *Server) string {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "brew-calls.log")
	script := `#!/bin/sh
echo "$@" >> ` + logFile + `
case "$1" in
  list) echo "php 8.4.7" ;;
esac
exit 0
`
	if err := os.WriteFile(srv.Cfg.BrewBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return logFile
}

// stubBrewUsesDependents 让这台测试服务器认为"formula 被 deps 依赖"。
func stubBrewUsesDependents(t *testing.T, srv *Server, deps ...string) {
	t.Helper()
	mgr := srv.svcManager()
	restore := mgr.SetBrewUsesProbeForTest(
		func(context.Context, string) ([]string, bool) { return deps, true })
	prev := srv.svcManagerOverride
	srv.svcManagerOverride = func(*Server) *services.Manager { return mgr }
	t.Cleanup(func() {
		restore()
		srv.svcManagerOverride = prev
	})
}

// recordUninstall 是"面板里登记过、目录也说得出怎么卸"的 PHP 8.4（真机形态：
// 机器上是别名 `php` 8.4.7，目录条目 php84 写 php@8.4）。
func recordPHP84(t *testing.T, srv *Server) {
	t.Helper()
	repo := services.NewRepository(srv.Store)
	// managed=false —— 与"用户早已登记"的真机形态一致（managed=true 会走
	// Manager.Uninstall 那条只会停 launchd 的路，反而测不到 brew 计划）。
	if err := repo.Create(t.Context(), &services.Service{
		Name: "sh-brew-php8-4", DisplayName: "PHP 8.4", Kind: services.KindNative,
		LaunchLabel: "sh.brew.php@8.4", Category: "lnmp", Managed: false,
	}); err != nil {
		t.Fatalf("登记 PHP 8.4 失败: %v", err)
	}
}

func TestMarketUninstallBlockedByBrewDependentsThenForce(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	calls := fakeBrewForUninstall(t, srv)
	recordPHP84(t, srv)
	// 依赖命中：真机 `brew uses --installed php` 输出的形态（llvm、rust 各一行）。
	stubBrewUsesDependents(t, srv, "llvm", "rust")

	// ① 市场列表里**看不到**依赖结论 —— 这是刻意的（列表不查依赖，见
	// TestMarketListNeverProbesBrewDependencies）：一个应用一条 `brew uses --installed`，
	// 36 条串起来就是 15 秒冷启动，用户看到的是"正在读取应用目录…"卡住不动。
	// 用户在列表里至少要能看见**可卸载的入口**（kind 仍是 brew），
	// 真正的依赖判定在点「卸载」时按需查（②）。
	it := marketItem(t, ts, cookies, "php84")
	plan, _ := it["uninstall"].(map[string]any)
	if plan == nil {
		t.Fatal("php84 应有卸载计划")
	}
	if asString(plan["kind"]) != "brew" {
		t.Errorf("列表里的计划仍要说得出怎么卸（kind=brew），实际 %+v", plan)
	}

	// ② 点「卸载」时按需查：这个接口**必须**给出 blocked + force_allowed + 结构化依赖方。
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/php84/uninstall-plan", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("取卸载计划应 200，实际 %d（body=%v）", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	full, _ := data["uninstall"].(map[string]any)
	if full == nil {
		t.Fatalf("卸载计划接口必须返回 uninstall，实际 %v", out)
	}
	if asString(full["blocked"]) == "" {
		t.Errorf("llvm/rust 依赖时计划必须 blocked，实际 %+v", full)
	}
	if full["force_allowed"] != true {
		t.Errorf("被依赖拦下时必须允许用户选择强制卸载（force_allowed=true），实际 %+v", full)
	}
	deps, _ := full["dependents"].([]any)
	if len(deps) == 0 {
		t.Error("依赖方必须结构化透出（前端要逐条列出谁依赖它）")
	}
	for _, d := range deps {
		m, _ := d.(map[string]any)
		if m["kind"] == "brew" && m["name"] == "llvm" {
			if act := asString(m["action"]); !strings.Contains(act, "强制卸载") {
				t.Errorf("llvm 的建议动作要给「强制卸载」这条路，实际 %q", act)
			}
		}
	}
	if raw, _ := os.ReadFile(calls); !strings.Contains(string(raw), "list --versions") {
		t.Errorf("市场列表要真的查 brew 装了哪些包（计划与已安装判定都靠它），实际调用：\n%s", raw)
	}

	// ③ 默认（不带 force）→ 409 + 人话；**绝不创建任务**。
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/market/php84?remove_data=0", nil, cookies)
	if res.StatusCode != 409 {
		t.Fatalf("有 brew 依赖且没选强制卸载时应 409，实际 %d（body=%v）", res.StatusCode, out)
	}
	msg := asString(out["msg"])
	for _, want := range []string{"llvm", "rust", "强制卸载", "ignore-dependencies"} {
		if !strings.Contains(msg, want) {
			t.Errorf("409 文案必须包含 %q（把 brew 的英文原文翻译成人话），实际：%s", want, msg)
		}
	}
	if strings.Contains(msg, "Error: Refusing to uninstall") && !strings.Contains(msg, "依赖") {
		t.Errorf("不能只把 brew 英文原文贴回来，实际：%s", msg)
	}

	// ④ 计划不允许强制时传 force → 400（防"前端多传一个参数就变破坏性开关"）。
	//    用一个目录外的 id：面板给不出任何可卸载计划（kind=none、ForceAllowed=false），
	//    此时 force=1 必须被直白拒绝（400），而不是悄悄放行。
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/market/uitest-not-installed?remove_data=0&force=1", nil, cookies)
	if res.StatusCode != 400 {
		t.Errorf("计划没查出依赖时 force=1 应 400，实际 %d（body=%v）", res.StatusCode, out)
	}

	// ⑤ 用户明确选强制 → 202 + task_id（任务被创建；这里只看命令形状与请求体，
	//    真正的 brew uninstall 在任务里，测试不会跑它）。
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/market/php84?remove_data=0&force=1", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("用户选择强制卸载后应创建任务（202），实际 %d（body=%v）", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	if asString(data["task_id"]) == "" {
		t.Errorf("202 必须带 task_id（关掉窗口也要能找回进度），实际 %v", out)
	}
}

// TestMarketListNeverProbesBrewDependencies 锁住"市场列表一条依赖都不查"这条**类级**
// 不变量（用户 2026-09-21 报的"应用又开始卡了：正在读取应用目录…"）。
//
// 根因：列表对**每一条**应用跑一遍完整卸载计划，其中依赖检测是一次真实的
// `brew uses --installed`（brew 启动本身约 0.4s）。36 条 → 冷启动 15 秒。
// 修法是把依赖检测推迟到用户点「卸载」时按需查（GET /market/{id}/uninstall-plan）。
//
// 这条测试用**计数探测**而不是计时：计数是确定性的，计时在负载变化时会假绿/假红。
// 反过来它也锁住"点卸载时真的查了"——否则 blocked/强制卸载那条路会静默失效。
func TestMarketListNeverProbesBrewDependencies(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	fakeBrewForUninstall(t, srv)
	recordPHP84(t, srv)
	// 覆盖默认桩：这次要**数**它被调了多少次。
	mgr := srv.svcManager()
	var probes int32
	restore := mgr.SetBrewUsesProbeForTest(func(context.Context, string) ([]string, bool) {
		atomic.AddInt32(&probes, 1)
		return []string{"llvm", "rust"}, true
	})
	prev := srv.svcManagerOverride
	srv.svcManagerOverride = func(*Server) *services.Manager { return mgr }
	t.Cleanup(func() {
		restore()
		srv.svcManagerOverride = prev
	})

	it := marketItem(t, ts, cookies, "php84")
	plan, _ := it["uninstall"].(map[string]any)
	if n := atomic.LoadInt32(&probes); n != 0 {
		t.Errorf("市场列表不该做任何 brew 依赖探测（每个条目一次真实 brew 调用，36 条就是 15 秒冷启动），实际探测 %d 次", n)
	}
	if asString(plan["blocked"]) != "" {
		t.Errorf("列表**还没查**依赖，就不能声称被依赖拦下（否则界面会在没查的情况下断言「没有依赖」/「有依赖」），实际 %+v", plan)
	}

	// 点「卸载」→ 按需查：必须真的探测，并把结论给出来。
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/php84/uninstall-plan", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("取卸载计划应 200，实际 %d（body=%v）", res.StatusCode, out)
	}
	if atomic.LoadInt32(&probes) == 0 {
		t.Error("点「卸载」时必须真的查依赖，否则 blocked / 强制卸载这条路无从谈起")
	}
	data, _ := out["data"].(map[string]any)
	full, _ := data["uninstall"].(map[string]any)
	if asString(full["blocked"]) == "" || full["force_allowed"] != true {
		t.Errorf("按需计划必须给出 blocked + force_allowed，实际 %+v", full)
	}
}

// TestMarketUninstallWithoutForceStillWorks 锁住"默认绝不带破坏性开关"这一条：
// 不带 force 的请求在**没有依赖**时也应该正常放行（202），而不是要求 force。
//
// 用 ollama（formula 就叫 ollama，既没有依赖规则、也与别的用例的 php
// 不共享 brew uses 缓存）：假 brew 报它装着、且没有任何包依赖它。
func TestMarketUninstallWithoutForceStillWorks(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	script := `#!/bin/sh
case "$1" in
  list) echo "ollama 0.12.0" ;;
  uses) : ;;
esac
exit 0
`
	if err := os.WriteFile(srv.Cfg.BrewBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := services.NewRepository(srv.Store)
	if err := repo.Create(t.Context(), &services.Service{
		Name: "sh-brew-ollama", DisplayName: "Ollama", Kind: services.KindNative,
		LaunchLabel: "sh.brew.ollama", Category: "tool", Managed: false,
	}); err != nil {
		t.Fatalf("登记 ollama 失败: %v", err)
	}

	it := marketItem(t, ts, cookies, "ollama")
	plan, _ := it["uninstall"].(map[string]any)
	if plan["blocked"] != nil && asString(plan["blocked"]) != "" {
		t.Fatalf("没有依赖时不该 blocked，实际 %v", plan)
	}
	if plan["force_allowed"] == true {
		t.Errorf("没有 brew 依赖时不该允许强制卸载，实际 %v", plan)
	}
	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/market/ollama?remove_data=0", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("没有 brew 依赖时普通卸载应照样能建任务（202），实际 %d（body=%v）", res.StatusCode, out)
	}
}

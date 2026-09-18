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

	// ① 市场列表里就要看得见"被依赖拦下 + 允许强制"：界面据此弹两个选项。
	it := marketItem(t, ts, cookies, "php84")
	plan, _ := it["uninstall"].(map[string]any)
	if plan == nil {
		t.Fatal("php84 应有卸载计划")
	}
	if asString(plan["blocked"]) == "" {
		t.Errorf("llvm/rust 依赖时计划必须 blocked，实际 %+v", plan)
	}
	if plan["force_allowed"] != true {
		t.Errorf("被依赖拦下时必须允许用户选择强制卸载（force_allowed=true），实际 %+v", plan)
	}
	deps, _ := plan["dependents"].([]any)
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

	// ② 默认（不带 force）→ 409 + 人话；**绝不创建任务**。
	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/market/php84?remove_data=0", nil, cookies)
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

	// ③ 计划不允许强制时传 force → 400（防"前端多传一个参数就变破坏性开关"）。
	//    用一个目录外的 id：面板给不出任何可卸载计划（kind=none、ForceAllowed=false），
	//    此时 force=1 必须被直白拒绝（400），而不是悄悄放行。
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/market/uitest-not-installed?remove_data=0&force=1", nil, cookies)
	if res.StatusCode != 400 {
		t.Errorf("计划没查出依赖时 force=1 应 400，实际 %d（body=%v）", res.StatusCode, out)
	}

	// ④ 用户明确选强制 → 202 + task_id（任务被创建；这里只看命令形状与请求体，
	//    真正的 brew uninstall 在任务里，测试不会跑它）。
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/market/php84?remove_data=0&force=1", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("用户选择强制卸载后应创建任务（202），实际 %d（body=%v）", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if asString(data["task_id"]) == "" {
		t.Errorf("202 必须带 task_id（关掉窗口也要能找回进度），实际 %v", out)
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

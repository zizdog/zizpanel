package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// Homebrew 目录缺失修复接口的门禁（坑 178）：202+task_id、requireAuth、回读失败不谎报。
// 全程用临时前缀里的假 brew，不碰 /opt/homebrew、不跑真 brew。
// bindFakeBrew 把测试服务器的服务管理器指向临时前缀里的**假 brew**。
//
// 两个安全前提（违反任何一条都会直接 Fatal，绝不继续）：
//  1. prefix 必须在临时目录里，不能是 /opt/homebrew、/usr/local；
//  2. UserName 置空 —— 测试不过 sudo，`InstalledFormulaVersionsErr` 因此直连假 brew。
func bindFakeBrew(t *testing.T, srv *Server, prefix, body string) string {
	t.Helper()
	for _, real := range []string{"/opt/homebrew", "/usr/local"} {
		if prefix == real || strings.HasPrefix(prefix, real+"/") {
			t.Fatalf("测试前缀 %s 指向真实 Homebrew，拒绝继续（只许用临时目录）", prefix)
		}
	}
	brew := filepath.Join(prefix, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	// 造一个只指向这个临时前缀的 Manager，并固定给这台测试服务器用。
	// 它的 brew 探测（list --versions / --prefix）会真的去执行那个**假脚本**，
	// 结论完全由脚本决定，与开发机装了什么无关。
	mgr := services.NewManager(srv.serviceRepo, services.Options{
		BrewBin:  brew,
		UserName: "", // 测试不过 sudo
		UserHome: filepath.Join(prefix, "home"),
		WorkDir:  filepath.Join(prefix, "work"),
	})
	// 卸载计划的依赖检测会跑 `brew uses --installed`（列表路径本不查，这里再钉一道）：
	// 绝不允许多跑哪怕一次真 brew。
	mgr.SetBrewUsesProbeForTest(func(context.Context, string) ([]string, bool) { return nil, true })
	srv.svcManagerOverride = func(*Server) *services.Manager { return mgr }
	return brew
}

// taskJoinedLog 把任务日志拼成一段文本（用于断言"如实说了什么"）。
func taskJoinedLog(t *testing.T, srv *Server, id string) string {
	t.Helper()
	task := srv.Tasks.Get(id)
	if task == nil {
		t.Fatalf("任务 %s 不存在", id)
	}
	lines, _, _, _ := task.Snapshot(0, 4000)
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// TestBrewRepairDirsEndpointFailsHonestlyWhenReadbackFails 是本组的核心门禁：
// 目录补建动作成功，但回读 `brew list --versions` 仍失败 ——
// 任务必须是 failed、错误里写着"仍未通过"，绝不许报成功。
func TestBrewRepairDirsEndpointFailsHonestlyWhenReadbackFails(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	prefix := shortTempDir(t)
	cask := filepath.Join(prefix, "Caskroom")
	bindFakeBrew(t, srv, prefix, fmt.Sprintf(
		"case \"$1\" in\n  --prefix) echo %q; exit 0 ;;\n  list) echo %q >&2; exit 1 ;;\nesac\nexit 0\n",
		prefix, "Error: No such file or directory @ dir_initialize - "+cask))

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/brew/repair-dirs", map[string]any{}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("修复必须走任务中心（202 + task_id），实际 %d：%v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	id := asString(data["task_id"])
	if id == "" {
		t.Fatalf("202 响应里必须有 task_id，实际 %v", out)
	}
	waitTaskDone(t, srv, id)

	meta := srv.Tasks.Get(id).Meta()
	if string(meta.Status) != "failed" {
		t.Fatalf("回读复核失败时任务必须是 failed（不许谎报成功），实际 %v：%s", meta.Status, meta.Error)
	}
	if !strings.Contains(meta.Error, "仍未") {
		t.Errorf("失败原因必须明确说「仍未复核通过」，实际：%s", meta.Error)
	}
	// 失败点在回读，而不是更早：目录确实被补建了。
	if st, err := os.Stat(cask); err != nil || !st.IsDir() {
		t.Errorf("Caskroom 应已被补建（失败发生在回读复核），实际 err=%v", err)
	}
	// 任务日志里也要有真实 stderr，用户能照着排查。
	logText := taskJoinedLog(t, srv, id)
	if !strings.Contains(logText, "dir_initialize") {
		t.Errorf("任务日志里必须保留真实 stderr（dir_initialize）：\n%s", logText)
	}
	if !strings.Contains(logText, "仍未通过") {
		t.Errorf("任务日志里必须如实说「仍未通过」：\n%s", logText)
	}
}

// TestBrewRepairDirsEndpointSucceedsAndMarketRecovers：目录补齐后回读通过才算成功，
// 并且市场缓存被主动失效（用户不必等 TTL 才看到警告消失）。
func TestBrewRepairDirsEndpointSucceedsAndMarketRecovers(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	prefix := shortTempDir(t)
	cask := filepath.Join(prefix, "Caskroom")
	bindFakeBrew(t, srv, prefix, fmt.Sprintf(
		"case \"$1\" in\n  --prefix) echo %q; exit 0 ;;\n  list)\n    if [ -d %q ]; then echo 'nginx 1.31.5'; exit 0; fi\n    echo %q >&2; exit 1 ;;\nesac\nexit 0\n",
		prefix, cask, "Error: No such file or directory @ dir_initialize - "+cask))

	// 先制造"未复核"现场：市场冷启动同步探测走到假 brew，失败。
	_, mout, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if got := mout["data"].(map[string]any)["brew_probe_ok"]; got != false {
		t.Fatalf("前置条件不成立：Caskroom 缺失时 brew_probe_ok 应为 false，实际 %v", got)
	}

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/brew/repair-dirs", map[string]any{}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("修复必须走任务中心（202），实际 %d：%v", res.StatusCode, out)
	}
	id := asString(out["data"].(map[string]any)["task_id"])
	waitTaskDone(t, srv, id)
	if meta := srv.Tasks.Get(id).Meta(); string(meta.Status) != "succeeded" {
		t.Fatalf("目录补齐后回读应通过，任务应 succeeded，实际 %v：%s", meta.Status, meta.Error)
	}

	// 修好之后市场必须立刻能复核（handler 主动失效了缓存）。
	_, mout, _ = doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	if got := mout["data"].(map[string]any)["brew_probe_ok"]; got != true {
		t.Errorf("修复成功后市场必须能复核（brew_probe_ok=true），实际 %v —— "+
			"否则用户修完还要盯着「未能复核已装软件」等 TTL", got)
	}
}

// TestBrewDirsStatusReportsActionableAdvice：状态接口把生产机那条真实失败
// 翻译成"缺哪个目录 / 怎么补 / 归属交给谁"，而且**不跑任何命令**。
func TestBrewDirsStatusReportsActionableAdvice(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	prev := setMarketProbeErrForTest(t,
		"`/opt/homebrew/bin/brew list --versions` 失败（以用户 zizdog 通过 sudo -n 执行）："+
			"Error: No such file or directory @ dir_initialize - /opt/homebrew/Caskroom")
	defer prev()

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/brew/dirs", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("状态接口应 200，实际 %d：%v", res.StatusCode, out)
	}
	d, _ := out["data"].(map[string]any)
	if d["brew_probe_ok"] != false || d["repairable"] != true {
		t.Fatalf("应判定为「可修复的目录缺失」，实际 %v", d)
	}
	if got := asString(d["missing_dir"]); got != "/opt/homebrew/Caskroom" {
		t.Errorf("missing_dir = %q，期望 /opt/homebrew/Caskroom", got)
	}
	advice := asString(d["advice"])
	for _, want := range []string{"/opt/homebrew/Caskroom", "install -d", "Homebrew 目录"} {
		if !strings.Contains(advice, want) {
			t.Errorf("建议里缺少 %q：\n%s", want, advice)
		}
	}
	if cmd := asString(d["command"]); !strings.Contains(cmd, "/opt/homebrew/Caskroom") {
		t.Errorf("command 必须点名缺失目录，实际 %q", cmd)
	}
}

// 不是「目录缺失」这一类（例如超时）：如实显示原因，但**不假装**这个按钮能修好。
func TestBrewDirsStatusDoesNotPretendOtherFailuresAreRepairable(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	prev := setMarketProbeErrForTest(t, "`/opt/homebrew/bin/brew list --versions` 超过 30 秒没有返回（超时）")
	defer prev()

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/brew/dirs", nil, cookies)
	d, _ := out["data"].(map[string]any)
	if d["repairable"] != false {
		t.Errorf("超时不是目录缺失，不许标成可修复：%v", d)
	}
	if asString(d["error"]) == "" {
		t.Error("失败原因必须如实给出")
	}
	if asString(d["advice"]) != "" {
		t.Error("不可修复时不该编一段建议出来")
	}
}

// TestBrewDirsEndpointsRequireAuth：两个接口都必须过 requireAuth。
func TestBrewDirsEndpointsRequireAuth(t *testing.T) {
	_, ts := newTestServer(t)
	res, _, _ := doJSON(t, ts, "GET", "/api/v1/brew/dirs", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录 GET /brew/dirs 应 401，实际 %d", res.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/brew/repair-dirs", map[string]any{}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录 POST /brew/repair-dirs 应 401，实际 %d", res.StatusCode)
	}
}

// TestBrewRepairWiringIsComplete 锁住"路由 / requireAuth / 任务中心 / 前端按钮"四件套：
// 少任何一环，用户要么看不到入口，要么点了没有任何可见进度。
func TestBrewRepairWiringIsComplete(t *testing.T) {
	srv := readGoSource(t, "server.go")
	for _, want := range []string{
		`"GET /api/v1/brew/dirs"`,
		`"POST /api/v1/brew/repair-dirs"`,
		"s.requireAuth(s.handleBrewDirs)",
		"s.requireAuth(s.handleBrewRepairDirs)",
	} {
		if !strings.Contains(srv, want) {
			t.Errorf("server.go 缺少 %q（路由必须注册且受 requireAuth 保护）", want)
		}
	}
	if api := readGoSource(t, "api_brew_dirs.go"); !strings.Contains(api, "s.launchTask(") {
		t.Error("修复接口必须走任务中心（launchTask → 202 + task_id）")
	}
	js := readAssetJS(t, "systemsettings.js")
	for _, want := range []string{"brew/repair-dirs", "补建缺失的 brew 目录", "taskCenter.start("} {
		if !strings.Contains(js, want) {
			t.Errorf("systemsettings.js 缺少 %q（按钮 / 接口调用 / 任务中心接线）", want)
		}
	}
}

// setMarketProbeErrForTest 写入/恢复包级的"上一次探测失败原因"。
func setMarketProbeErrForTest(t *testing.T, msg string) func() {
	t.Helper()
	marketProbeErrMu.Lock()
	prev := marketProbeErr
	marketProbeErr = msg
	marketProbeErrMu.Unlock()
	return func() {
		marketProbeErrMu.Lock()
		marketProbeErr = prev
		marketProbeErrMu.Unlock()
	}
}

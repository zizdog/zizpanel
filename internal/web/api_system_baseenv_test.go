package web

import (
	"context"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  基础环境（运行依赖层）接口
//
//  背景（2026-09-19 产品负责人拆分）：
//    · 运行依赖层（基础环境）= CLT → Homebrew → ffmpeg，所有 brew 类应用都要；
//    · 网站环境层（LNMP）    = nginx / PHP / MySQL / phpMyAdmin，只有网站相关要。
//  首页横幅过去那颗「一键 LNMP」把两层一起装了，用户看不出"缺的是哪一层、
//  点一下会装什么"。这两个接口只管运行依赖层；LNMP 仍走
//  POST /api/v1/market/install-lnmp（本次不删、不改）。
//
//  契约（与前端逐字约定）：
//    GET  /api/v1/system/base-env         → 200, data{clt_ok,brew_ok,deps_ok,ready,missing}
//    POST /api/v1/system/base-env/install → 202, data{task_id,title:"安装基础环境"}
//
//  探测与安装都用注入点替换：默认实现会执行 xcode-select / stat 真实 Homebrew
//  前缀 / 真的调 brew，结论与副作用都会随测试机而变（违反"单测不许碰真实环境"）。
// ============================================================================

// TestBaseEnvEndpointContract 只读接口的字段名与含义必须与契约一致。
func TestBaseEnvEndpointContract(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.baseEnvProbeOverride = func(context.Context) services.BaseEnvStatus {
		return services.BaseEnvStatus{
			CLTOK: true, BrewOK: false, DepsOK: false, Ready: false,
			Missing: []string{"Homebrew", "ffmpeg"},
		}
	}
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/base-env", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("基础环境接口应 200，实际 %d: %v", res.StatusCode, out)
	}
	data := apiData(t, out)
	// 逐字锁字段名：前端横幅直接读它们，改名等于接口契约破坏。
	for _, key := range []string{"clt_ok", "brew_ok", "deps_ok", "ready"} {
		if _, ok := data[key].(bool); !ok {
			t.Errorf("data[%q] 必须是布尔，实际 %v", key, data[key])
		}
	}
	if data["clt_ok"] != true || data["brew_ok"] != false || data["deps_ok"] != false || data["ready"] != false {
		t.Errorf("响应与探测结果不一致: %v", data)
	}
	missing, _ := data["missing"].([]any)
	if len(missing) != 2 || missing[0] != "Homebrew" || missing[1] != "ffmpeg" {
		t.Errorf("missing 应为 [\"Homebrew\",\"ffmpeg\"]（契约样例），实际 %v", data["missing"])
	}
}

// TestBaseEnvEndpointRequiresAuth 只读接口也要登录：它暴露这台机器装了什么。
func TestBaseEnvEndpointRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t)
	res, _, _ := doJSON(t, ts, "GET", "/api/v1/system/base-env", nil, nil)
	if res.StatusCode != 401 {
		t.Fatalf("未登录访问基础环境应 401，实际 %d", res.StatusCode)
	}
}

// TestBaseEnvInstallRequiresCSRF 安装是写操作，必须走 CSRF 双提交校验。
func TestBaseEnvInstallRequiresCSRF(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	srv.baseEnvInstallOverride = func(context.Context, *services.InstallResult) error {
		t.Error("CSRF 校验失败时绝不该启动安装任务")
		return nil
	}

	res, _, _ := doJSONOpt(t, ts, "POST", "/api/v1/system/base-env/install", nil, cookies, true)
	if res.StatusCode != 403 {
		t.Fatalf("缺 CSRF 头的写操作应 403，实际 %d", res.StatusCode)
	}
}

// TestBaseEnvInstallLaunchesTaskCenterTask 安装必须走任务中心（202 + task_id）。
//
// 同步执行会让用户只能看"请等待"，而且任务挂在 r.Context() 上 —— 一刷新就把
// brew 杀了（SPEC-任务中心.md）。这里同时锁标题（前端进度窗直接显示它）。
func TestBaseEnvInstallLaunchesTaskCenterTask(t *testing.T) {
	srv, ts := newTestServer(t)
	var called bool
	srv.baseEnvInstallOverride = func(_ context.Context, res *services.InstallResult) error {
		called = true
		res.Steps = append(res.Steps, "（测试桩）基础环境已就绪")
		return nil
	}
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/base-env/install", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("安装基础环境应立刻 202（长任务），实际 %d: %v", res.StatusCode, out)
	}
	data := apiData(t, out)
	if data["title"] != "安装基础环境" {
		t.Errorf("任务标题应为「安装基础环境」，实际 %v", data["title"])
	}
	id := taskIDFrom(t, out)

	tk := waitTaskDone(t, srv, id)
	if !called {
		t.Fatal("任务体没有被执行（注入的安装桩一次都没被调用）")
	}
	if tk.Status() != tasks.StatusSucceeded {
		t.Fatalf("桩成功时任务应成功，实际 %s: %s", tk.Status(), tk.Meta().Error)
	}
}

// TestBaseEnvInstallFailureIsHonest 任务失败必须如实落到任务的 Error 上，
// 绝不把"没装上"写成"已完成"（AGENTS.md 铁律 11）。
func TestBaseEnvInstallFailureIsHonest(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.baseEnvInstallOverride = func(context.Context, *services.InstallResult) error {
		return context.DeadlineExceeded // 借用任意错误：重点是"失败要如实报出来"
	}
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/base-env/install", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("长任务应先 202，实际 %d", res.StatusCode)
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if tk.Status() != tasks.StatusFailed {
		t.Fatalf("安装失败时任务状态应为 failed，实际 %s", tk.Status())
	}
	if strings.TrimSpace(tk.Meta().Error) == "" {
		t.Error("失败必须带真实原因（Meta.Error 为空等于谎报成功）")
	}
}

// TestBaseEnvProbeUsesServicesLayer 默认路径（不注入）必须真的走 services 层探测，
// 而不是在 web 层重写一份口径 —— 两份口径必然漂。
//
// 测试服务器的 brew 前缀是沙箱里的临时目录（newTestServer 造了一个假的
// bin/brew），所以这里只能断言"接口返回的是 services.BaseEnvStatus 的形状"，
// 具体缺不缺由 services 层的测试负责（baseenv_test.go）。
func TestBaseEnvProbeUsesServicesLayer(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	_ = srv

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/base-env", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("应 200，实际 %d", res.StatusCode)
	}
	data := apiData(t, out)
	if _, ok := data["ready"].(bool); !ok {
		t.Fatalf("ready 必须是布尔，实际 %v", data["ready"])
	}
	if _, ok := data["missing"].([]any); !ok {
		t.Fatalf("missing 必须是数组（前端遍历它显示还缺什么），实际 %v", data["missing"])
	}
}

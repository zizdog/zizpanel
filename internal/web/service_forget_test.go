package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  「从面板移除该服务」通路（DELETE /api/v1/services/{name}）
//
//  用户改写了这条通路的语义（原话："移除却不卸载是什么意思 ……
//  让用户看不到却持续运行"）：**先停、再移除**。只删记录会让一个仍在运行的
//  服务彻底从面板消失（用户看不到、它却继续占端口跑业务）。
//
//  三类结果：
//    · 停成功               → 删记录，runtime_touched:true；
//    · 停不掉但确认不在跑     → 删记录，runtime_touched:false（卡死场景的出口）；
//    · 停不掉且仍在运行       → 409 拒绝，记录保留。
//
//  仍然保留的历史约束：这条通路**不卸载软件**（它只服务"面板不认识、也不知道
//  怎么卸载"的服务），也绝不顺手删磁盘上的项目文件。
// ============================================================================

// runtimeSentinel 造一个"一旦构造服务管理器就记账"的注入点。
//
// 新语义下这条通路**必须**构造 Manager（它要先去停服务）；这个哨兵用来证明
// 我们确实走了"停一下"的路径，而不是绕过去只删库。
func runtimeSentinel(touched *bool) func(*Server) *services.Manager {
	return func(s *Server) *services.Manager {
		*touched = true
		return services.NewManager(s.serviceRepo, services.Options{})
	}
}

// TestServiceForgetStopsThenDeletesRecord 记录存在、运行时不可用（容器不可能在跑）：
// 记录删掉、如实回报"没有停过任何东西"、compose 文件一字未动。
func TestServiceForgetStopsThenDeletesRecord(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// 一条 managed=true 的 compose 记录 —— 正是真机上"运行时没了却只能走卸载"的那条。
	composeFile := filepath.Join(srv.Cfg.WorkDir, "compose", "stirling-pdf.yml")
	if err := os.MkdirAll(filepath.Dir(composeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	const composeBody = "services:\n  pdf:\n    image: example/stirling-pdf:arm64\n"
	if err := os.WriteFile(composeFile, []byte(composeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &services.Service{
		Name: "stirling-pdf", DisplayName: "Stirling PDF", Kind: services.KindCompose,
		ComposeFile: composeFile, Managed: true, Enabled: true,
	}
	if err := srv.serviceRepo.Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	runtimeTouched := false
	srv.svcManagerOverride = runtimeSentinel(&runtimeTouched)
	t.Cleanup(func() { srv.svcManagerOverride = nil })

	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/services/stirling-pdf", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删除记录应 200，实际 %d: %v", res.StatusCode, out)
	}
	if out["ok"] != true {
		t.Errorf("响应必须是 ok:true，实际 %v", out)
	}
	data, _ := out["data"].(map[string]any)
	if data["removed"] != true {
		t.Errorf("响应必须如实说 removed:true，实际 %v", out)
	}
	// 没有 Docker 时容器不可能在跑：runtime_touched/stopped 必须如实为 false。
	if data["runtime_touched"] != false || data["stopped"] != false {
		t.Errorf("运行时不可用时应如实回报没停过任何东西，实际 %v", data)
	}
	if !runtimeTouched {
		t.Error("新语义下这条通路必须构造服务管理器（先去停服务），不能绕过去只删库")
	}
	if exists, err := srv.serviceRepo.Exists(context.Background(), "stirling-pdf"); err != nil || exists {
		t.Errorf("记录应已从面板移除，exists=%v err=%v", exists, err)
	}
	// 磁盘上的 compose 文件也必须一字未动（这条通路不许卸载软件、不许删文件）。
	if b, err := os.ReadFile(composeFile); err != nil || string(b) != composeBody {
		t.Errorf("compose 文件被改动了：err=%v 内容=%q", err, string(b))
	}
}

// TestServiceForgetMissingRecordIs404 记录不存在：404，且说明里点名"不在面板记录里"。
func TestServiceForgetMissingRecordIs404(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/services/no-such-service-here", nil, cookies)
	if res.StatusCode != 404 {
		t.Fatalf("删除不存在的记录应 404，实际 %d: %v", res.StatusCode, out)
	}
	msg := asString(out["msg"])
	if !strings.Contains(msg, "不在面板记录里") {
		t.Errorf("404 的说明要点名记录不存在，实际 %q", msg)
	}
}

// TestServiceUninstallRuntimeUnavailableSaysNothingStopped 卸载时运行时不可用的文案。
//
// 注入一个"检测不到 Docker socket"的管理器：DriverFor 返回带 RuntimeUnavailable
// 标记的错误，**不执行任何命令、不碰真实 Docker**。断言：
//   - 任务如实失败（不是假装卸掉了）；
//   - 报错说清"没有停止任何容器"，并指出可以只删记录；
//   - 记录原样保留（失败＝什么都没发生）。
func TestServiceUninstallRuntimeUnavailableSaysNothingStopped(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	if err := srv.serviceRepo.Create(context.Background(), &services.Service{
		Name: "stirling-pdf", DisplayName: "Stirling PDF", Kind: services.KindCompose,
		ComposeFile: filepath.Join(srv.Cfg.WorkDir, "compose", "stirling-pdf.yml"),
		Managed:     true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	srv.svcManagerOverride = func(s *Server) *services.Manager {
		// DockerSocket 为空 = 本机没有可用的容器运行时（Docker/Colima 被删掉的现场）。
		return services.NewManager(s.serviceRepo, services.Options{DockerSocket: ""})
	}
	t.Cleanup(func() { srv.svcManagerOverride = nil })

	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/services/stirling-pdf/uninstall", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("卸载是长任务，应 202，实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	task := srv.Tasks.Get(asString(data["task_id"]))
	if task == nil {
		t.Fatalf("task_id 必须能在任务中心查到，实际 %v", out)
	}
	select {
	case <-task.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("运行时不可用的卸载应快速失败")
	}
	if task.Status() != tasks.StatusFailed {
		t.Fatalf("运行时不可用的卸载必须如实失败，实际 %s", task.Status())
	}
	msg := task.Meta().Error
	if !strings.Contains(msg, "没有停止任何容器") {
		t.Errorf("报错必须说清没有停止任何容器，实际 %q", msg)
	}
	// 文案在 2026-09-19 改成用户语言「从列表移除（不卸载软件）」——断言要跟着走，
	// 否则真机上的出口还在、测试却红（或者反过来：改了文案而测试没跟上）。
	if !strings.Contains(msg, "从列表移除") {
		t.Errorf("报错应指出可以只删记录（「从列表移除（不卸载软件）」），实际 %q", msg)
	}
	if exists, _ := srv.serviceRepo.Exists(context.Background(), "stirling-pdf"); !exists {
		t.Error("卸载失败时不该把面板记录删掉（失败＝什么都没发生）")
	}
}

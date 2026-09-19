package web

// api_disks_volumeauth_test.go —— 「申请授权」按钮的门禁。
//
// 这一条功能会**真的让 macOS 弹授权窗**，所以门禁的重点是"什么时候绝对不许读"：
//   · 没有非系统卷 → 4xx，不建任务，一个字节都不读；
//   · 控制台用户为空 / root / loginwindow（没人在屏幕前）→ 4xx，不建任务，一个字节都不读；
//   · 有卷 + 有人在 → 202 + 任务真的被创建，且**读盘动作是注入的桩**（绝不碰用户真实外接盘）；
//   · 任务读被拒 → 结论必须含"仍被拒 + 手动授权路径"（不许谎报成功）；
//   · 任务读通 → 结论是"已可访问"。
//
// 另外锁死任务体里的**二次预检**：handler 通过后、任务真正跑之前人走开了，
// 任务必须直接失败，仍然一个字节都不读（TOCTOU）。

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// stubDiskVolumeAuth 注入"控制台用户 / 非系统卷 / 读盘动作"三个判据。
// 读盘动作**永远**被换成桩：单测绝不去读用户真实的外接盘（只统计调用次数）。
func stubDiskVolumeAuth(t *testing.T, consoleUser string, mounts []string, req func() files.VolumeAuthResult) *int {
	t.Helper()
	prevConsole, prevMounts, prevReq := diskVolumeAuthConsoleUserFn, diskVolumeAuthMountsFn, diskVolumeAuthRequestFn
	calls := 0
	diskVolumeAuthConsoleUserFn = func() string { return consoleUser }
	diskVolumeAuthMountsFn = func() []string { return mounts }
	diskVolumeAuthRequestFn = func(context.Context, time.Duration, func(string, ...any)) files.VolumeAuthResult {
		calls++
		if req == nil {
			return files.VolumeAuthResult{}
		}
		return req()
	}
	t.Cleanup(func() {
		diskVolumeAuthConsoleUserFn, diskVolumeAuthMountsFn, diskVolumeAuthRequestFn = prevConsole, prevMounts, prevReq
	})
	return &calls
}

// diskVolumeAuthConfirmBody 是"用户在前端确认框里点了确定"之后的请求体。
// 后端要求显式 confirm=true（坑 192）：不带就 4xx，且不建任务、不读盘。
var diskVolumeAuthConfirmBody = map[string]any{"confirm": true}

// assertVolumeAuthRejected 断言一条拒绝：状态码、逐字 msg、机器可读 reason、
// 读盘 0 次、任务表为空。msg 为空表示"只断 reason"。
func assertVolumeAuthRejected(t *testing.T, srv *Server, res *http.Response, out map[string]any, wantCode int, wantMsg, wantReason string, calls int, why string) {
	t.Helper()
	if res.StatusCode != wantCode {
		t.Errorf("%s：应 %d 拒绝，实际 %d: %v", why, wantCode, res.StatusCode, out["msg"])
	}
	if wantMsg != "" {
		if got, _ := out["msg"].(string); got != wantMsg {
			t.Errorf("%s：拒绝理由应逐字为 %q，实际 %q", why, wantMsg, got)
		}
	}
	if got, _ := out["reason"].(string); got != wantReason {
		t.Errorf("%s：响应 reason 应为 %q（测试按它断言），实际 %q", why, wantReason, got)
	}
	if calls != 0 {
		t.Errorf("%s：绝不许读外接卷，实际读了 %d 次", why, calls)
	}
	noDiskTaskCreated(t, srv, why)
}

func taskLogText(tk *tasks.Task) string {
	lines, _, _, _ := tk.Snapshot(0, 2000)
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln.Text + "\n")
	}
	return b.String()
}

// TestDiskVolumeAuthRejectsWhenNoOneAtConsole：铁律 12 —— 没人在屏幕前，绝不读、绝不建任务。
func TestDiskVolumeAuthRejectsWhenNoOneAtConsole(t *testing.T) {
	for _, consoleUser := range []string{"", "root", "loginwindow"} {
		srv, ts, cookies := newDiskServer(t, newFakeWorld())
		calls := stubDiskVolumeAuth(t, consoleUser, []string{"/Volumes/ZPMirror"}, nil)

		res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/volume-auth", diskVolumeAuthConfirmBody, cookies)
		assertVolumeAuthRejected(t, srv, res, out, http.StatusConflict,
			diskVolumeAuthNoSessionReason, diskVolumeAuthReasonNoConsole, *calls,
			"控制台用户 "+consoleUser+" 申请授权")
	}
}

// TestDiskVolumeAuthRejectsWithoutExternalVolume：没盘就拒绝，且不读、不建任务。
func TestDiskVolumeAuthRejectsWithoutExternalVolume(t *testing.T) {
	srv, ts, cookies := newDiskServer(t, newFakeWorld())
	calls := stubDiskVolumeAuth(t, "zizdog", nil, nil)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/volume-auth", diskVolumeAuthConfirmBody, cookies)
	assertVolumeAuthRejected(t, srv, res, out, http.StatusConflict,
		diskVolumeAuthNoVolumeReason, diskVolumeAuthReasonNoVolume, *calls, "没有外接卷申请授权")
}

// TestDiskVolumeAuthRequiresExplicitConfirm：后端强制"显式确认"（坑 192）——
// 只点按钮不算：缺 confirm（或 confirm=false）→ 4xx + reason=confirm_required +
// **任务表为空 + 读盘 0 次**，绝不靠前端自觉。
func TestDiskVolumeAuthRequiresExplicitConfirm(t *testing.T) {
	for _, body := range []any{nil, map[string]any{}, map[string]any{"confirm": false}} {
		srv, ts, cookies := newDiskServer(t, newFakeWorld())
		calls := stubDiskVolumeAuth(t, "zizdog", []string{"/Volumes/ZPMirror"}, nil)

		res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/volume-auth", body, cookies)
		if res.StatusCode < 400 || res.StatusCode >= 500 {
			t.Errorf("缺 confirm 时应 4xx 拒绝，实际 %d: %v", res.StatusCode, out["msg"])
		}
		assertVolumeAuthRejected(t, srv, res, out, http.StatusBadRequest,
			diskVolumeAuthConfirmRequiredReason, diskVolumeAuthReasonConfirmRequired, *calls,
			"缺 confirm 申请授权")
	}
}

// TestDiskVolumeAuthRejectionOrder：三条预检的**顺序**不能乱 ——
// 没盘 + 没会话 + 没确认 → 报"没盘"；有盘 + 没会话 + 没确认 → 报"没人在场"；
// 只有前两条都过，才轮到"缺确认"。每条 reason 独立可断言。
func TestDiskVolumeAuthRejectionOrder(t *testing.T) {
	cases := []struct {
		why         string
		consoleUser string
		mounts      []string
		wantMsg     string
		wantReason  string
	}{
		{"没盘优先于没会话与缺确认", "zizdog", nil, diskVolumeAuthNoVolumeReason, diskVolumeAuthReasonNoVolume},
		{"没会话优先于缺确认", "", []string{"/Volumes/ZPMirror"}, diskVolumeAuthNoSessionReason, diskVolumeAuthReasonNoConsole},
	}
	for _, c := range cases {
		srv, ts, cookies := newDiskServer(t, newFakeWorld())
		calls := stubDiskVolumeAuth(t, c.consoleUser, c.mounts, nil)
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/volume-auth", nil, cookies)
		assertVolumeAuthRejected(t, srv, res, out, http.StatusConflict, c.wantMsg, c.wantReason, *calls, c.why)
	}
}

// TestDiskVolumeAuthRouteRequiresAuth：新接口同样要登录（写操作另有 CSRF，由 requireAuth 统一处理）。
func TestDiskVolumeAuthRouteRequiresAuth(t *testing.T) {
	_, ts, _ := newDiskServer(t, newFakeWorld())
	res, _, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/volume-auth", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录申请授权应 401，实际 %d", res.StatusCode)
	}
}

// TestDiskVolumeAuthTaskReportsDeniedAndManualPath：读被拒 → 任务失败，结论"仍被拒 + 手动授权路径"。
func TestDiskVolumeAuthTaskReportsDeniedAndManualPath(t *testing.T) {
	srv, ts, cookies := newDiskServer(t, newFakeWorld())
	calls := stubDiskVolumeAuth(t, "zizdog", []string{"/Volumes/ZPMirror"}, func() files.VolumeAuthResult {
		return files.VolumeAuthResult{ConsoleUser: "zizdog", Attempted: []string{"/Volumes/ZPMirror"}}
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/volume-auth", diskVolumeAuthConfirmBody, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("预检通过必须走任务中心（202 + task_id），实际 %d: %v", res.StatusCode, out["msg"])
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if *calls != 1 {
		t.Fatalf("授权任务应恰好读一次外接卷，实际 %d 次", *calls)
	}
	if tk.Status() != tasks.StatusFailed {
		t.Errorf("读被拒时任务必须如实判失败（不许谎报成功），实际 %s", tk.Status())
	}
	errMsg := tk.Meta().Error
	if !strings.Contains(errMsg, "仍被拒") {
		t.Errorf("被拒结论必须写明「仍被拒」，实际：%s", errMsg)
	}
	if !strings.Contains(errMsg, "完全磁盘访问权限") || !strings.Contains(errMsg, panelBinaryForGuide) {
		t.Errorf("被拒结论必须给出手动授权路径（系统设置 → 完全磁盘访问权限 → %s），实际：%s", panelBinaryForGuide, errMsg)
	}
	if strings.Contains(errMsg, "已可访问") {
		t.Errorf("一个都没读通时不许出现「已可访问」：%s", errMsg)
	}
	logs := taskLogText(tk)
	if !strings.Contains(logs, "已向系统发起授权请求，请在这台机器的屏幕上点『允许』") {
		t.Errorf("任务日志必须告诉屏幕前的人去点「允许」，实际日志：\n%s", logs)
	}
	if !strings.Contains(logs, "仍被拒") {
		t.Errorf("任务日志也要如实写出每个卷的结果，实际日志：\n%s", logs)
	}
}

// TestDiskVolumeAuthTaskReportsAccessible：读通 → 任务成功且结论是"已可访问"。
func TestDiskVolumeAuthTaskReportsAccessible(t *testing.T) {
	srv, ts, cookies := newDiskServer(t, newFakeWorld())
	calls := stubDiskVolumeAuth(t, "zizdog", []string{"/Volumes/ZPMirror"}, func() files.VolumeAuthResult {
		return files.VolumeAuthResult{ConsoleUser: "zizdog", Okay: []string{"/Volumes/ZPMirror"}}
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/volume-auth", diskVolumeAuthConfirmBody, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("预检通过必须走任务中心（202 + task_id），实际 %d: %v", res.StatusCode, out["msg"])
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if *calls != 1 {
		t.Fatalf("授权任务应恰好读一次外接卷，实际 %d 次", *calls)
	}
	if tk.Status() != tasks.StatusSucceeded {
		t.Fatalf("读通时任务应成功，实际 %s（%s）", tk.Status(), tk.Meta().Error)
	}
	r := diskTaskInstallResult(t, tk)
	if !strings.Contains(r.Message, "已可访问") {
		t.Errorf("读通结论应为「已可访问」，实际：%s", r.Message)
	}
	if !stepsContain(r.Steps, "已可访问") {
		t.Errorf("结果步骤要逐卷写明「已可访问」，实际：%v", r.Steps)
	}
}

// TestDiskVolumeAuthTaskRechecksConsoleSession：任务体里的二次预检 ——
// handler 通过后、任务跑之前人走开了（或盘拔了）→ 任务失败，仍是一个字节都不读。
func TestDiskVolumeAuthTaskRechecksConsoleSession(t *testing.T) {
	srv, ts, cookies := newDiskServer(t, newFakeWorld())
	prevConsole, prevMounts, prevReq := diskVolumeAuthConsoleUserFn, diskVolumeAuthMountsFn, diskVolumeAuthRequestFn
	t.Cleanup(func() {
		diskVolumeAuthConsoleUserFn, diskVolumeAuthMountsFn, diskVolumeAuthRequestFn = prevConsole, prevMounts, prevReq
	})
	calls := 0
	reads := 0
	// 第一次调用（handler 同步预检）有会话，之后（任务体里）变成"人走了"。
	diskVolumeAuthConsoleUserFn = func() string {
		calls++
		if calls == 1 {
			return "zizdog"
		}
		return ""
	}
	diskVolumeAuthMountsFn = func() []string { return []string{"/Volumes/ZPMirror"} }
	diskVolumeAuthRequestFn = func(context.Context, time.Duration, func(string, ...any)) files.VolumeAuthResult {
		reads++
		return files.VolumeAuthResult{Okay: []string{"/Volumes/ZPMirror"}}
	}

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/volume-auth", diskVolumeAuthConfirmBody, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("handler 预检通过时应 202，实际 %d: %v", res.StatusCode, out["msg"])
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if tk.Status() != tasks.StatusFailed {
		t.Errorf("任务体里发现没人在场时必须失败，实际 %s", tk.Status())
	}
	if reads != 0 {
		t.Errorf("任务体预检不过时绝不许读外接卷，实际读了 %d 次", reads)
	}
	if !strings.Contains(tk.Meta().Error, diskVolumeAuthNoSessionReason) {
		t.Errorf("失败原因应是预检理由，实际：%s", tk.Meta().Error)
	}
}

// TestDiskListExposesVolumeAuthButtonState：列表下发按钮判据（不读盘的那两个）。
func TestDiskListExposesVolumeAuthButtonState(t *testing.T) {
	_, ts, cookies := newDiskServer(t, newFakeWorld())
	stubDiskVolumeAuth(t, "zizdog", []string{"/Volumes/ZPMirror"}, nil)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/disks", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("磁盘列表应 200，实际 %d", res.StatusCode)
	}
	data := apiData(t, out)
	va, _ := data["volume_auth"].(map[string]any)
	if va == nil {
		t.Fatal("磁盘列表必须下发 volume_auth —— 前端靠它决定「申请授权」能不能点")
	}
	if va["has_console_session"] != true {
		t.Errorf("有人登录时应 has_console_session=true，实际 %v", va["has_console_session"])
	}
	if got, _ := va["count"].(float64); got != 1 {
		t.Errorf("非系统卷数量应为 1，实际 %v", va["count"])
	}
	mounts, _ := va["mounts"].([]any)
	if len(mounts) != 1 || mounts[0] != "/Volumes/ZPMirror" {
		t.Errorf("mounts 应逐条下发，实际 %v", va["mounts"])
	}

	stubDiskVolumeAuth(t, "", []string{"/Volumes/ZPMirror"}, nil)
	_, out2, _ := doJSON(t, ts, "GET", "/api/v1/system/disks", nil, cookies)
	va2, _ := apiData(t, out2)["volume_auth"].(map[string]any)
	if va2 == nil || va2["has_console_session"] != false {
		t.Errorf("没人在场时 has_console_session 必须为 false，实际 %v", va2)
	}
}

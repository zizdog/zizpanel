package web

// api_permissions_test.go —— 「权限」页的门禁。
//
// 这一页会**真的让 macOS 弹授权窗**，所以门禁的重点是"什么时候绝对不许读"：
//   · GET 列表：注入的读函数调用次数必须为 0（坑 191：读一次就替用户记 denial）；
//   · apply 预检顺序固定：无控制台 → 4xx no_console；缺 confirm → 400
//     confirm_required；前两步都不建任务、一个字节都不读；
//   · 「外部应用条件授权入口」用**假外部应用**跑通整条链路：未安装 ⇒ GET 连字段都没有；
//     已安装 ⇒ 带版本/可执行文件/签名身份；申请 ⇒ 以它自己的 CLI 回答（面板零读）；
//   · zizvideo 已改为面板托管 ⇒ 条目关闭：既不显示，apply 也如实拒绝。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/permissions"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

const (
	fakeAppID        = "fakeapp"
	fakeAppSigningID = "cn.fakeapp.serve"
)

// fakeExternalApp 是门禁用的假外部应用：证明"注册表里加一条"就能走通整条授权链路。
func fakeExternalApp() permissions.ExternalApp {
	return permissions.ExternalApp{
		ID: fakeAppID, Name: "假外部应用", Title: "假外部应用", Why: "验证外部应用授权入口",
		Enabled: true, InstallPath: "/opt/fakeapp/bin/fakeapp",
		Label: "cn.fakeapp.serve", Port: 7799, SigningID: fakeAppSigningID,
		CheckVerb: "check-access", DataSubdir: "Library/Application Support/fakeapp",
		HealthPath: "healthz",
		RootsArgs:  func(cfg string) []string { return []string{"roots", "list", "--config", cfg} },
	}
}

func installedFakeApp() permissions.DetectedApp {
	return permissions.DetectedApp{
		Spec: fakeExternalApp(), Installed: true, Version: "1.2.3",
		ExecPath: "/opt/fakeapp/bin/fakeapp", Owner: "zizdog",
		Roots: []string{"/Volumes/ZPMirror/video"}, SigningID: fakeAppSigningID, Port: 7799,
	}
}

// permCounters 记录三个注入点各自的调用次数（"零调用"是本文件的核心断言）。
type permCounters struct {
	probe  int // 面板自己读受保护路径（会弹窗）
	check  int // 外部应用自检
	detect int // 外部应用探测
}

// permEnvOpts 是一次权限页测试要注入的全部假世界。
type permEnvOpts struct {
	consoleUser string
	mounts      []string
	registry    []permissions.ExternalApp
	detect      func(spec permissions.ExternalApp) permissions.DetectedApp
	probe       func() []permissions.PathResult
	check       func(d permissions.DetectedApp, path string) (permissions.CheckResult, error)
}

// stubPermissionsEnv 注入权限页的全部外部动作；探针永远不碰真实受保护路径。
func stubPermissionsEnv(t *testing.T, o permEnvOpts) *permCounters {
	t.Helper()
	prevConsole, prevMounts, prevProbe := permConsoleUserFn, permVolumeMountsFn, permProbeFn
	prevDetect, prevCheck, prevHome := permExternalDetectFn, permExternalCheckFn, permUserHomeFn
	prevRegistry, prevLookup := permRegistryFn, permLookupAppFn
	c := &permCounters{}
	if o.detect == nil {
		o.detect = func(permissions.ExternalApp) permissions.DetectedApp { return permissions.DetectedApp{} }
	}
	if o.check == nil {
		o.check = func(permissions.DetectedApp, string) (permissions.CheckResult, error) {
			return permissions.CheckResult{Supported: true, Readable: true}, nil
		}
	}
	permConsoleUserFn = func() string { return o.consoleUser }
	permVolumeMountsFn = func() []string { return o.mounts }
	permUserHomeFn = func(string) (string, error) { return "/Users/zizdog", nil }
	permProbeFn = func(context.Context, []string) []permissions.PathResult {
		c.probe++
		if o.probe == nil {
			return nil
		}
		return o.probe()
	}
	permRegistryFn = func() []permissions.ExternalApp {
		out := make([]permissions.ExternalApp, 0, len(o.registry))
		for _, a := range o.registry {
			if a.Enabled {
				out = append(out, a)
			}
		}
		return out
	}
	permLookupAppFn = func(id string) (permissions.ExternalApp, bool) {
		for _, a := range o.registry {
			if a.ID == id {
				return a, true
			}
		}
		return permissions.ExternalApp{}, false
	}
	permExternalDetectFn = func(_ context.Context, spec permissions.ExternalApp) permissions.DetectedApp {
		c.detect++
		return o.detect(spec)
	}
	permExternalCheckFn = func(_ context.Context, d permissions.DetectedApp, path string) (permissions.CheckResult, error) {
		c.check++
		return o.check(d, path)
	}
	t.Cleanup(func() {
		permConsoleUserFn, permVolumeMountsFn, permProbeFn = prevConsole, prevMounts, prevProbe
		permExternalDetectFn, permExternalCheckFn, permUserHomeFn = prevDetect, prevCheck, prevHome
		permRegistryFn, permLookupAppFn = prevRegistry, prevLookup
	})
	return c
}

// permRegistryFull 是默认注册表：zizvideo（关闭）+ 传入的额外条目。
func permRegistryFull(extra ...permissions.ExternalApp) []permissions.ExternalApp {
	return append([]permissions.ExternalApp{permissions.ZizvideoApp()}, extra...)
}

func newPermissionsServer(t *testing.T) (*Server, *httptest.Server, []*http.Cookie) {
	t.Helper()
	srv, ts := newTestServer(t)
	res, out, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("初始化失败 %d: %v", res.StatusCode, out)
	}
	return srv, ts, cookies
}

func permItems(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	data := apiData(t, out)
	raw, _ := data["items"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, _ := it.(map[string]any)
		if m != nil {
			items = append(items, m)
		}
	}
	return items
}

func permFindItem(t *testing.T, out map[string]any, id string) map[string]any {
	t.Helper()
	for _, it := range permItems(t, out) {
		if it["id"] == id {
			return it
		}
	}
	return nil
}

func permTaskResult(t *testing.T, tk *tasks.Task) *services.InstallResult {
	t.Helper()
	r, _ := tk.Meta().Result.(*services.InstallResult)
	if r == nil {
		t.Fatalf("任务结果应为 *services.InstallResult，实际 %#v（error=%q）", tk.Meta().Result, tk.Meta().Error)
	}
	return r
}

func permNoTask(t *testing.T, srv *Server, why string) {
	t.Helper()
	if list := srv.Tasks.List(); len(list) != 0 {
		t.Errorf("%s：拒绝时不应创建任何任务，实际 %d 个", why, len(list))
	}
}

// TestPermissionsCopyIsShort：用户可见文案（why / 状态提示 / 关闭说明）一句话 ≤40 字。
func TestPermissionsCopyIsShort(t *testing.T) {
	_, ts, cookies := newPermissionsServer(t)
	stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", mounts: []string{"/Volumes/ZPMirror"},
		registry: permRegistryFull(fakeExternalApp()),
		detect:   func(permissions.ExternalApp) permissions.DetectedApp { return installedFakeApp() },
	})

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/permissions", nil, cookies)
	items := permItems(t, out)
	if len(items) != 3 {
		t.Fatalf("应有 3 项（完全磁盘访问 / 可移除宗卷 / 已装的假外部应用），实际 %d", len(items))
	}
	for _, it := range items {
		for _, field := range []string{"why", "status_hint"} {
			s, _ := it[field].(string)
			if n := len([]rune(s)); n > 40 {
				t.Errorf("%v 的 %s 超过 40 字（%d）：%s", it["id"], field, n, s)
			}
		}
	}
	if n := len([]rune(permissions.ZizvideoApp().DisabledNote)); n > 40 {
		t.Errorf("关闭说明超过 40 字（%d）：%s", n, permissions.ZizvideoApp().DisabledNote)
	}
}

// TestPermissionApplyWritesAudit：申请与结果都写审计（供「日志 → 操作审计」查）。
func TestPermissionApplyWritesAudit(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", registry: permRegistryFull(),
		probe: func() []permissions.PathResult {
			return []permissions.PathResult{{Path: "/Volumes/ZPMirror", Readable: true}}
		},
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/removable/apply", map[string]any{"confirm": true}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("应 202，实际 %d", res.StatusCode)
	}
	waitTaskDone(t, srv, taskIDFrom(t, out))

	var n int
	if err := srv.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE target='permission-removable'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("申请（task_start）与结果至少各写一条审计，实际 %d 条", n)
	}
}

// TestPermissionsListNeverReadsProtectedPaths：GET 只用不碰内容的判据（坑 191）。
func TestPermissionsListNeverReadsProtectedPaths(t *testing.T) {
	_, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", mounts: []string{"/Volumes/ZPMirror"},
		registry: permRegistryFull(fakeExternalApp()),
		// 假应用"没装"：探测是便宜的，但连自检都不许发生。
	})

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/permissions", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("权限列表应 200，实际 %d: %v", res.StatusCode, out["msg"])
	}
	if c.probe != 0 {
		t.Fatalf("GET 绝不许读受保护路径，实际读了 %d 次", c.probe)
	}
	if c.check != 0 {
		t.Fatalf("GET 绝不许调用外部应用自检，实际 %d 次", c.check)
	}
	data := apiData(t, out)
	if data["has_console_session"] != true || data["console_user"] != "zizdog" {
		t.Errorf("应如实下发控制台会话判据，实际 %v / %v", data["has_console_session"], data["console_user"])
	}
	if permFindItem(t, out, permissions.ItemFullDisk) == nil {
		t.Error("列表必须包含完全磁盘访问项")
	}
	if permFindItem(t, out, permissions.ItemRemovable) == nil {
		t.Error("列表必须包含可移除宗卷项")
	}
}

// TestPermissionsListHidesDisabledAndUninstalledApps：关闭的条目与未安装的外部应用
// 都**整块不出现**（响应里连字段都没有，前端因此不渲染灰色占位）。
func TestPermissionsListHidesDisabledAndUninstalledApps(t *testing.T) {
	_, ts, cookies := newPermissionsServer(t)
	stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog",
		registry:    permRegistryFull(fakeExternalApp()),
		// detect 默认返回"未安装"。
	})

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/permissions", nil, cookies)
	raw, _ := json.Marshal(out)
	low := strings.ToLower(string(raw))
	if strings.Contains(low, "zizvideo") {
		t.Errorf("关闭的 zizvideo 条目不该出现：%s", raw)
	}
	if strings.Contains(low, fakeAppID) {
		t.Errorf("未安装的假外部应用不该出现（前端因此不渲染这一块）：%s", raw)
	}
}

// TestPermissionsListShowsExternalAppWhenInstalled：装了才出现，且带版本/可执行文件/签名身份。
func TestPermissionsListShowsExternalAppWhenInstalled(t *testing.T) {
	_, ts, cookies := newPermissionsServer(t)
	stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog",
		registry:    permRegistryFull(fakeExternalApp()),
		detect:      func(permissions.ExternalApp) permissions.DetectedApp { return installedFakeApp() },
	})

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/permissions", nil, cookies)
	it := permFindItem(t, out, fakeAppID)
	if it == nil {
		t.Fatal("装了假外部应用时必须出现它的条目")
	}
	if it["version"] != "1.2.3" || it["exec_path"] != "/opt/fakeapp/bin/fakeapp" {
		t.Errorf("必须带版本与可执行文件路径，实际 %v / %v", it["version"], it["exec_path"])
	}
	if it["signing_id"] != fakeAppSigningID {
		t.Errorf("必须带签名身份，实际 %v", it["signing_id"])
	}
	if it["accepts_path"] != true {
		t.Error("外部应用项必须让前端选/填目标目录")
	}
}

// TestPermissionApplyRejectsWithoutConsole：① 没人在机器前 → 4xx no_console，
// 不建任务、不读任何路径、连外部应用都不探测。
func TestPermissionApplyRejectsWithoutConsole(t *testing.T) {
	for _, consoleUser := range []string{"", "root", "loginwindow"} {
		srv, ts, cookies := newPermissionsServer(t)
		c := stubPermissionsEnv(t, permEnvOpts{
			consoleUser: consoleUser, mounts: []string{"/Volumes/ZPMirror"},
			registry: permRegistryFull(fakeExternalApp()),
			detect:   func(permissions.ExternalApp) permissions.DetectedApp { return installedFakeApp() },
		})

		res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/full_disk/apply", map[string]any{"confirm": true}, cookies)
		if res.StatusCode < 400 || res.StatusCode >= 500 {
			t.Errorf("控制台用户 %q 时应 4xx 拒绝，实际 %d", consoleUser, res.StatusCode)
		}
		if out["reason"] != permReasonNoConsole {
			t.Errorf("reason 应为 %q，实际 %v", permReasonNoConsole, out["reason"])
		}
		if c.probe != 0 || c.check != 0 || c.detect != 0 {
			t.Errorf("没人在场时必须先拒绝：不许探测/读，实际 probe=%d check=%d detect=%d", c.probe, c.check, c.detect)
		}
		permNoTask(t, srv, "控制台用户 "+consoleUser)
	}
}

// TestPermissionApplyRequiresConfirm：② 缺 confirm → 400 confirm_required，且不读、不建任务。
func TestPermissionApplyRequiresConfirm(t *testing.T) {
	for _, body := range []any{nil, map[string]any{}, map[string]any{"confirm": false}} {
		srv, ts, cookies := newPermissionsServer(t)
		c := stubPermissionsEnv(t, permEnvOpts{
			consoleUser: "zizdog", mounts: []string{"/Volumes/ZPMirror"},
			registry: permRegistryFull(fakeExternalApp()),
			detect:   func(permissions.ExternalApp) permissions.DetectedApp { return installedFakeApp() },
		})

		res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/full_disk/apply", body, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("缺 confirm 应 400，实际 %d", res.StatusCode)
		}
		if out["reason"] != permReasonConfirmRequired {
			t.Errorf("reason 应为 %q，实际 %v", permReasonConfirmRequired, out["reason"])
		}
		if c.probe != 0 || c.check != 0 || c.detect != 0 {
			t.Errorf("缺确认时必须先拒绝：不许探测/读，实际 probe=%d check=%d detect=%d", c.probe, c.check, c.detect)
		}
		permNoTask(t, srv, "缺 confirm")
	}
}

// TestPermissionApplyPrecheckOrder：顺序固定 —— 没人在场优先于缺确认。
func TestPermissionApplyPrecheckOrder(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{registry: permRegistryFull()})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/full_disk/apply", nil, cookies)
	if res.StatusCode != http.StatusConflict || out["reason"] != permReasonNoConsole {
		t.Errorf("既没人在场又缺确认时应先报 no_console，实际 %d / %v", res.StatusCode, out["reason"])
	}
	if c.probe != 0 {
		t.Errorf("预检不过时一个字节都不许读，实际 %d", c.probe)
	}
	permNoTask(t, srv, "预检顺序")
}

// TestPermissionApplyRouteRequiresAuth：接口同样要登录。
func TestPermissionApplyRouteRequiresAuth(t *testing.T) {
	srv, ts := newTestServer(t)
	_ = srv
	res, _, _ := doJSON(t, ts, "GET", "/api/v1/permissions", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录读权限列表应 401，实际 %d", res.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/permissions/full_disk/apply", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录申请授权应 401，实际 %d", res.StatusCode)
	}
}

// TestPermissionApplyPanelReadReportsHonest：面板自己读的那两项 —— 逐项如实报 +
// 写进历史（GET 之后能看到上次真实结果），被拒时点名系统设置里给谁开。
func TestPermissionApplyPanelReadReportsHonest(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", mounts: []string{"/Volumes/ZPMirror"}, registry: permRegistryFull(),
		probe: func() []permissions.PathResult {
			return []permissions.PathResult{
				{Path: "/Users/zizdog/Desktop", Readable: true},
				{Path: "/Users/zizdog/Documents", Denied: true, Reason: "operation not permitted"},
				{Path: "/Users/zizdog/Downloads", Denied: true, Reason: "operation not permitted"},
			}
		},
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/full_disk/apply", map[string]any{"confirm": true}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("预检通过必须走任务中心（202 + task_id），实际 %d: %v", res.StatusCode, out["msg"])
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if c.probe != 1 {
		t.Fatalf("授权任务应恰好读一次，实际 %d 次", c.probe)
	}
	if tk.Status() != tasks.StatusFailed {
		t.Errorf("有目标被拒时必须如实判失败，实际 %s", tk.Status())
	}
	errMsg := tk.Meta().Error
	if !strings.Contains(errMsg, "仍被拒") || !strings.Contains(errMsg, "完全磁盘访问权限") {
		t.Errorf("被拒结论要含「仍被拒」与手动授权路径，实际：%s", errMsg)
	}
	if strings.Contains(errMsg, "/Users/zizdog") {
		t.Errorf("审计/结果摘要里不许出现真实路径片段：%s", errMsg)
	}
	// 历史：GET 必须显示上一次的真实结果。
	_, out2, _ := doJSON(t, ts, "GET", "/api/v1/permissions", nil, cookies)
	it := permFindItem(t, out2, permissions.ItemFullDisk)
	if it == nil || it["status"] != permissions.StatusDenied || it["last_checked_at"] == "" {
		t.Errorf("被拒结果必须写进历史供 GET 显示，实际 %v", it)
	}
}

func TestPermissionApplyPanelReadReportsGranted(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", mounts: []string{"/Volumes/ZPMirror"}, registry: permRegistryFull(),
		probe: func() []permissions.PathResult {
			return []permissions.PathResult{{Path: "/Volumes/ZPMirror", Readable: true}}
		},
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/removable/apply", map[string]any{"confirm": true}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("应 202，实际 %d: %v", res.StatusCode, out["msg"])
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if c.probe != 1 {
		t.Fatalf("应恰好读一次，实际 %d", c.probe)
	}
	if tk.Status() != tasks.StatusSucceeded {
		t.Fatalf("读通时任务应成功，实际 %s（%s）", tk.Status(), tk.Meta().Error)
	}
	if r := permTaskResult(t, tk); !strings.Contains(r.Message, "已可访问") {
		t.Errorf("结论应为「已可访问」，实际：%s", r.Message)
	}
	_, out2, _ := doJSON(t, ts, "GET", "/api/v1/permissions", nil, cookies)
	it := permFindItem(t, out2, permissions.ItemRemovable)
	if it == nil || it["status"] != permissions.StatusGranted {
		t.Errorf("成功结果必须写进历史，实际 %v", it)
	}
}

// TestPermissionApplyExternalUsesItsOwnCLI：外部应用申请由它自己的 CLI 回答，
// 面板侧的读函数必须**零调用**（否则弹的是面板的授权，对它没用）。
func TestPermissionApplyExternalUsesItsOwnCLI(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	gotPath := ""
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", registry: permRegistryFull(fakeExternalApp()),
		detect: func(permissions.ExternalApp) permissions.DetectedApp { return installedFakeApp() },
		check: func(_ permissions.DetectedApp, path string) (permissions.CheckResult, error) {
			gotPath = path
			return permissions.CheckResult{Supported: true, Readable: true}, nil
		},
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/"+fakeAppID+"/apply",
		map[string]any{"confirm": true, "path": "/Volumes/ZPMirror/video"}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("应 202，实际 %d: %v", res.StatusCode, out["msg"])
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if tk.Status() != tasks.StatusSucceeded {
		t.Fatalf("自检可读时任务应成功，实际 %s（%s）", tk.Status(), tk.Meta().Error)
	}
	if c.check != 1 || gotPath != "/Volumes/ZPMirror/video" {
		t.Errorf("必须把用户选的目录交给该应用自检，实际调用 %d 次、path=%q", c.check, gotPath)
	}
	if c.probe != 0 {
		t.Errorf("外部应用申请绝不许由面板去读那个目录，实际读了 %d 次", c.probe)
	}
}

func TestPermissionApplyExternalReportsDenied(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", registry: permRegistryFull(fakeExternalApp()),
		detect: func(permissions.ExternalApp) permissions.DetectedApp { return installedFakeApp() },
		check: func(permissions.DetectedApp, string) (permissions.CheckResult, error) {
			return permissions.CheckResult{Supported: true, Readable: false, Reason: "operation not permitted"}, nil
		},
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/"+fakeAppID+"/apply",
		map[string]any{"confirm": true, "path": "/Volumes/ZPMirror/video"}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("应 202，实际 %d", res.StatusCode)
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if tk.Status() != tasks.StatusFailed {
		t.Errorf("不可读时必须如实判失败，实际 %s", tk.Status())
	}
	errMsg := tk.Meta().Error
	if !strings.Contains(errMsg, "/opt/fakeapp/bin/fakeapp") || !strings.Contains(errMsg, fakeAppSigningID) {
		t.Errorf("失败要点名给哪个二进制 + 签名身份，实际：%s", errMsg)
	}
	if c.probe != 0 {
		t.Errorf("不许由面板去读，实际读了 %d 次", c.probe)
	}
}

// TestPermissionApplyExternalUnsupportedIsHonest：旧版本没有自检动词 ⇒
// 如实报"不支持自检，请升级"，绝不退化去读。
func TestPermissionApplyExternalUnsupportedIsHonest(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", registry: permRegistryFull(fakeExternalApp()),
		detect: func(permissions.ExternalApp) permissions.DetectedApp { return installedFakeApp() },
		check: func(permissions.DetectedApp, string) (permissions.CheckResult, error) {
			return permissions.CheckResult{Supported: false, Reason: "该外部应用版本不支持自检，请升级"}, nil
		},
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/"+fakeAppID+"/apply",
		map[string]any{"confirm": true, "path": "/Volumes/ZPMirror/video"}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("应 202，实际 %d", res.StatusCode)
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if tk.Status() != tasks.StatusFailed {
		t.Errorf("不支持时必须如实失败，实际 %s", tk.Status())
	}
	if !strings.Contains(tk.Meta().Error, "不支持自检") {
		t.Errorf("失败原因应是「不支持自检，请升级」，实际：%s", tk.Meta().Error)
	}
	if c.probe != 0 {
		t.Errorf("不支持时绝不许由面板去读那个目录，实际读了 %d 次", c.probe)
	}
}

// TestPermissionApplyExternalNeedsPath：拿不到允许根又没填目录 ⇒ 不猜、不建任务。
func TestPermissionApplyExternalNeedsPath(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	d := installedFakeApp()
	d.Roots = nil
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", registry: permRegistryFull(fakeExternalApp()),
		detect: func(permissions.ExternalApp) permissions.DetectedApp { return d },
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/"+fakeAppID+"/apply", map[string]any{"confirm": true}, cookies)
	if res.StatusCode != http.StatusBadRequest || out["reason"] != permReasonPathRequired {
		t.Errorf("没有目录时应 400 path_required，实际 %d / %v", res.StatusCode, out["reason"])
	}
	if c.check != 0 || c.probe != 0 {
		t.Errorf("没有目录时不许执行任何读，实际 check=%d probe=%d", c.check, c.probe)
	}
	permNoTask(t, srv, "外部应用缺目录")
}

// TestPermissionApplyExternalNotInstalled：apply 也按"未安装"拒绝（不建任务、不读）。
func TestPermissionApplyExternalNotInstalled(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", registry: permRegistryFull(fakeExternalApp()),
		// detect 默认"未安装"。
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/"+fakeAppID+"/apply",
		map[string]any{"confirm": true, "path": "/Volumes/x"}, cookies)
	if res.StatusCode != http.StatusNotFound || out["reason"] != permReasonNotInstalled {
		t.Errorf("未安装时应 404 not_installed，实际 %d / %v", res.StatusCode, out["reason"])
	}
	if c.check != 0 || c.probe != 0 {
		t.Errorf("未安装时不许执行任何读，实际 check=%d probe=%d", c.check, c.probe)
	}
	permNoTask(t, srv, "外部应用未安装")
}

// TestPermissionApplyExternalFallsBackToAllowRoot：目录从该实例的允许根取，
// 前端没传时用第一条（不是面板猜的路径）。
func TestPermissionApplyExternalFallsBackToAllowRoot(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	gotPath := ""
	c := stubPermissionsEnv(t, permEnvOpts{
		consoleUser: "zizdog", registry: permRegistryFull(fakeExternalApp()),
		detect: func(permissions.ExternalApp) permissions.DetectedApp { return installedFakeApp() },
		check: func(_ permissions.DetectedApp, path string) (permissions.CheckResult, error) {
			gotPath = path
			return permissions.CheckResult{Supported: true, Readable: true}, nil
		},
	})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/"+fakeAppID+"/apply", map[string]any{"confirm": true}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("有允许根时应 202，实际 %d: %v", res.StatusCode, out["msg"])
	}
	waitTaskDone(t, srv, taskIDFrom(t, out))
	if c.check != 1 || gotPath != "/Volumes/ZPMirror/video" {
		t.Errorf("目录应取自该实例的允许根，实际调用 %d 次、path=%q", c.check, gotPath)
	}
}

// TestPermissionApplyZizvideoDisabledIsHonest：zizvideo 已改为面板托管 ⇒
// 条目关闭：GET 不显示它，apply 如实拒绝（点名原因），不探测、不读、不建任务。
func TestPermissionApplyZizvideoDisabledIsHonest(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{consoleUser: "zizdog", registry: permRegistryFull()})

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/permissions", nil, cookies)
	if raw, _ := json.Marshal(out); strings.Contains(strings.ToLower(string(raw)), "zizvideo") {
		t.Errorf("关闭的 zizvideo 条目不该在列表里出现：%s", raw)
	}

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/"+permissions.ItemZizvideo+"/apply", map[string]any{"confirm": true}, cookies)
	if res.StatusCode != http.StatusNotFound || out["reason"] != permReasonDisabled {
		t.Fatalf("关闭的条目应 404 disabled，实际 %d / %v", res.StatusCode, out["reason"])
	}
	if out["msg"] != permissions.ZizvideoApp().DisabledNote {
		t.Errorf("拒绝文案应是关闭说明（无需单独授权），实际 %v", out["msg"])
	}
	if c.detect != 0 || c.check != 0 || c.probe != 0 {
		t.Errorf("关闭的条目不许探测/读，实际 detect=%d check=%d probe=%d", c.detect, c.check, c.probe)
	}
	permNoTask(t, srv, "关闭的 zizvideo 条目")
}

// TestPermissionApplyUnknownItem：未知 id 如实拒绝，不建任务。
func TestPermissionApplyUnknownItem(t *testing.T) {
	srv, ts, cookies := newPermissionsServer(t)
	c := stubPermissionsEnv(t, permEnvOpts{consoleUser: "zizdog", registry: permRegistryFull()})

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/permissions/nope/apply", map[string]any{"confirm": true}, cookies)
	if res.StatusCode != http.StatusNotFound || out["reason"] != permReasonUnknownItem {
		t.Errorf("未知项应 404 unknown_item，实际 %d / %v", res.StatusCode, out["reason"])
	}
	if c.probe != 0 {
		t.Errorf("未知项不许读任何路径，实际 %d", c.probe)
	}
	permNoTask(t, srv, "未知项")
}

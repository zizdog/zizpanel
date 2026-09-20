package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  File Browser「重置口令」门禁（用户 2026-09-20 要求）
//
//  全程用**假 CLI + 假 launchd + 假回读探测**：绝不执行真机
//  /Users/zizdog/filebrowser/filebrowser、绝不碰 /bin/launchctl、绝不连 8081。
//  要锁的四件事：停→改→起的顺序、失败也起回来、只有两项验证都过才报成功、
//  口令不进日志与审计。
// ============================================================================

// fbUsersLSTable 是 v2.63.23 真实输出的表格部分（表头带空格、Admin 在第 8 列）。
const fbUsersLSTable = `2026/09/20 21:32:08 No config file used
2026/09/20 21:32:08 Using database: /tmp/zp-fbprobe/probe.db
ID  Username  Scope  Locale  V. Mode  S.Click  Red. After C/M  Admin  Execute  Create  Rename  Modify  Delete  Share  Download  Pwd Lock
1   zizdog    /      en      list     false    false           true   true     true    true    true    true    true   true      false`

// fakeFilebrowserCLI 是假 CLI + 假 launchd：记录调用顺序与参数，
// 并用一个可变状态模拟"服务在不在、端口在不在听"。
type fakeFilebrowserCLI struct {
	mu sync.Mutex
	// calls 是按发生顺序记录的 `命令 参数...`（含真实口令，仅存在于测试内存里）。
	calls []string
	// stopped 表示 launchd 作业已卸载；listening 表示端口在监听。
	stopped   bool
	listening bool

	usersLS   string
	configCat string

	failUpdate    bool
	failBootstrap bool
	healthCode    int
	loginCode     int

	updatedUser string
	updatedPass string
	rootSet     string
}

// fbSub 从 argv 里取子命令（users ls / users update / config set / config cat）。
func fbSub(args []string) string {
	for i, a := range args {
		if (a == "users" || a == "config") && i+1 < len(args) {
			return a + " " + args[i+1]
		}
	}
	return ""
}

func fbArgValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeFilebrowserCLI) exec(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))

	if name == "/bin/launchctl" {
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		switch sub {
		case "bootout":
			f.stopped, f.listening = true, false
			return "", nil
		case "bootstrap":
			if f.failBootstrap {
				return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
			}
			f.stopped, f.listening = false, true
			return "", nil
		case "kickstart":
			f.stopped, f.listening = false, true
			return "", nil
		case "print":
			if f.stopped {
				return "Could not find service in domain", errors.New("exit status 113")
			}
			return "state = running", nil
		}
		return "", nil
	}

	switch fbSub(args) {
	case "users ls":
		return f.usersLS, nil
	case "users update":
		f.updatedUser = ""
		// `users update <用户名> --password <新口令> ...`
		for i, a := range args {
			if a == "update" && i+1 < len(args) {
				f.updatedUser = args[i+1]
			}
		}
		f.updatedPass = fbArgValue(args, "--password")
		// 故意把口令回显在输出里：日志脱敏门禁必须挡住它。
		out := fmt.Sprintf("user %s updated with password %s", f.updatedUser, f.updatedPass)
		if f.failUpdate {
			return out + "\nError: timeout", errors.New("exit status 1")
		}
		return out, nil
	case "config set":
		f.rootSet = fbArgValue(args, "--root")
		return "Configuration updated", nil
	case "config cat":
		return f.configCat, nil
	}
	return "", nil
}

func (f *fakeFilebrowserCLI) probe(_ context.Context, _, rawURL string, _ []byte) (int, error) {
	if strings.HasSuffix(rawURL, "/health") {
		return f.healthCode, nil
	}
	return f.loginCode, nil
}

// fbGate 是一次门禁测试的全部接线。
type fbGate struct {
	srv  *Server
	ts   *httptest.Server
	cook []*http.Cookie
	fake *fakeFilebrowserCLI
	root string
	dir  string
}

// writeFakeFilebrowserPlist 造一份**临时** plist：ProgramArguments 指向假二进制，
// -r / -d 指向临时路径。单测绝不读真机 /Library/LaunchDaemons。
func writeFakeFilebrowserPlist(t *testing.T, path, bin, db, root string) {
	t.Helper()
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.zizdog.filebrowser</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>-b</string>
        <string>/filebrowser</string>
        <string>-a</string>
        <string>127.0.0.1</string>
        <string>-p</string>
        <string>8081</string>
        <string>-r</string>
        <string>%s</string>
        <string>-d</string>
        <string>%s</string>
    </array>
</dict>
</plist>
`, bin, root, db)
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupFilebrowserGate 起一台测试服务器，登记 File Browser 服务记录，
// 把服务管理器钉到"假 CLI + 假 launchd + 假探测 + 临时 plist"。
func setupFilebrowserGate(t *testing.T, f *fakeFilebrowserCLI) *fbGate {
	t.Helper()
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	if err := srv.serviceRepo.Create(t.Context(), &services.Service{
		Name: "com-zizdog-filebrowser", DisplayName: "File Browser（网页文件管理）",
		Kind: services.KindNative, LaunchLabel: services.FilebrowserServiceLabel, Managed: true,
	}); err != nil {
		t.Fatalf("登记 File Browser 服务记录失败：%v", err)
	}

	dir := shortTempDir(t)
	root := "/Users/me"
	bin := filepath.Join(dir, "fake-filebrowser")
	db := filepath.Join(dir, "filebrowser.db")
	plist := filepath.Join(dir, "com.zizdog.filebrowser.plist")
	writeFakeFilebrowserPlist(t, plist, bin, db, root)

	mgr := services.NewManager(srv.serviceRepo, services.Options{
		UserHome: filepath.Join(dir, "home"), // 故意不用真机家目录
		WorkDir:  filepath.Join(dir, "work"),
		// UserName 留空：测试不过 sudo、不 chown 真机文件。
	})
	mgr.SetFilebrowserPlistForTest(plist)
	mgr.SetFilebrowserExecForTest(f.exec)
	mgr.SetFilebrowserProbeForTest(f.probe)
	mgr.SetPortCheckProbeForTest(func(int) (bool, []string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.listening, nil, nil
	})
	srv.svcManagerOverride = func(*Server) *services.Manager { return mgr }

	return &fbGate{srv: srv, ts: ts, cook: cookies, fake: f, root: root, dir: dir}
}

// callIndexes 返回若干子串在调用序列里的下标（找不到 = -1）。
func (g *fbGate) callIndexes(subs ...string) []int {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	out := make([]int, len(subs))
	for i := range out {
		out[i] = -1
	}
	for idx, call := range g.fake.calls {
		for i, sub := range subs {
			if out[i] < 0 && strings.Contains(call, sub) {
				out[i] = idx
			}
		}
	}
	return out
}

// postReset 提交重置口令请求，返回 (状态码, task_id)。
func (g *fbGate) postReset(t *testing.T, path string, body any) (int, string, map[string]any) {
	t.Helper()
	res, out, _ := doJSON(t, g.ts, "POST", path, body, g.cook)
	id := ""
	if data, ok := out["data"].(map[string]any); ok {
		id = asString(data["task_id"])
	}
	return res.StatusCode, id, out
}

// passwordCredential 从任务结果的凭据区取新口令（唯一允许出现口令的地方）。
func passwordCredential(t *testing.T, task any) (string, string) {
	t.Helper()
	res, ok := task.(*services.InstallResult)
	if !ok || res == nil {
		t.Fatalf("任务结果应是 *services.InstallResult，实际 %T", task)
	}
	user, pass := "", ""
	for _, c := range res.Credentials {
		switch c.Key {
		case "filebrowser_username":
			user = c.Value
		case "filebrowser_password":
			pass = c.Value
		}
	}
	return user, pass
}

// TestFilebrowserResetPasswordSuccessOrderAndRootAlign 是主门禁：
// 顺序必须是 停 → 改 → 起、root 对齐落到假 CLI 参数、口令不进日志与审计。
func TestFilebrowserResetPasswordSuccessOrderAndRootAlign(t *testing.T) {
	const pw = "Zz_Custom_Pass_1234"
	fake := &fakeFilebrowserCLI{
		listening: true, usersLS: fbUsersLSTable,
		configCat:  "Server:\n  Log:  stdout\n  Root:  /Users/me\n",
		healthCode: 200, loginCode: 200,
	}
	g := setupFilebrowserGate(t, fake)

	code, id, out := g.postReset(t, "/api/v1/services/com-zizdog-filebrowser/filebrowser-password",
		map[string]string{"password": pw})
	if code != http.StatusAccepted {
		t.Fatalf("必须走任务中心（202 + task_id），实际 %d：%v", code, out)
	}
	task := waitTaskDone(t, g.srv, id)
	meta := task.Meta()
	if string(meta.Status) != "succeeded" {
		t.Fatalf("两项验证都过时应成功，实际 %v：%s", meta.Status, meta.Error)
	}

	user, got := passwordCredential(t, meta.Result)
	if user != "zizdog" || got != pw {
		t.Fatalf("结果里应带用户名与新口令，实际 user=%q pass=%q", user, got)
	}

	// 顺序：bootout → users update → bootstrap。config set 必须在 bootstrap 之前（停着的窗口内）。
	order := g.callIndexes(
		"bootout system/com.zizdog.filebrowser",
		"users update zizdog",
		"config set --root /Users/me",
		"bootstrap system")
	iBootout, iUpdate, iConfigSet, iBootstrap := order[0], order[1], order[2], order[3]
	if iBootout < 0 || iUpdate < 0 || iConfigSet < 0 || iBootstrap < 0 {
		t.Fatalf("调用序列不完整：%v", fake.calls)
	}
	if !(iBootout < iUpdate && iUpdate < iBootstrap) {
		t.Errorf("顺序必须是 停 → 改 → 起，实际 bootout=%d update=%d bootstrap=%d：%v",
			iBootout, iUpdate, iBootstrap, fake.calls)
	}
	if !(iConfigSet < iBootstrap) {
		t.Errorf("root 对齐必须在服务停着的窗口内完成（config set 在 bootstrap 之前）：%v", fake.calls)
	}
	if fake.rootSet != g.root {
		t.Errorf("config set --root 的参数应是面板记录的实际根 %q，实际 %q", g.root, fake.rootSet)
	}

	// 口令绝不进任务日志（假 CLI 故意把口令回显在输出里，脱敏必须挡住）。
	logText := taskJoinedLog(t, g.srv, id)
	if strings.Contains(logText, pw) {
		t.Fatalf("口令出现在任务日志里：\n%s", logText)
	}
	if !strings.Contains(logText, "****") {
		t.Errorf("日志里应有脱敏后的命令标签（****）：\n%s", logText)
	}

	// 审计文本同样不许出现口令。
	rows, err := g.srv.Store.DB().Query(`SELECT COALESCE(detail,''), COALESCE(message,'') FROM audit_logs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var detail, message string
		if err := rows.Scan(&detail, &message); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, pw) || strings.Contains(message, pw) {
			t.Fatalf("审计里出现口令：detail=%q message=%q", detail, message)
		}
	}
}

// TestFilebrowserResetPasswordGeneratedWhenEmpty：不给口令就生成强随机口令（≥16 位）。
func TestFilebrowserResetPasswordGeneratedWhenEmpty(t *testing.T) {
	fake := &fakeFilebrowserCLI{
		listening: true, usersLS: fbUsersLSTable,
		configCat: "Server:\n  Root:  /Users/me\n", healthCode: 200, loginCode: 200,
	}
	g := setupFilebrowserGate(t, fake)

	code, id, out := g.postReset(t, "/api/v1/services/com-zizdog-filebrowser/filebrowser-password", nil)
	if code != http.StatusAccepted {
		t.Fatalf("空 body 也应接受（自动生成口令），实际 %d：%v", code, out)
	}
	task := waitTaskDone(t, g.srv, id)
	meta := task.Meta()
	if string(meta.Status) != "succeeded" {
		t.Fatalf("应成功，实际 %v：%s", meta.Status, meta.Error)
	}
	user, pw := passwordCredential(t, meta.Result)
	if user != "zizdog" {
		t.Errorf("用户名应从库里解析，实际 %q", user)
	}
	if len(pw) < 16 {
		t.Errorf("自动生成的口令必须 ≥16 位，实际 %d 位：%q", len(pw), pw)
	}
	if strings.Contains(taskJoinedLog(t, g.srv, id), pw) {
		t.Error("自动生成的口令也不许进任务日志")
	}
}

// TestFilebrowserResetPasswordStartsServiceBackEvenWhenCLIFails：
// CLI 失败时服务仍必须被起回来，且接口如实报错（绝不谎报成功）。
func TestFilebrowserResetPasswordStartsServiceBackEvenWhenCLIFails(t *testing.T) {
	fake := &fakeFilebrowserCLI{
		listening: true, usersLS: fbUsersLSTable, failUpdate: true,
		configCat: "Server:\n  Root:  /Users/me\n", healthCode: 200, loginCode: 200,
	}
	g := setupFilebrowserGate(t, fake)

	code, id, _ := g.postReset(t, "/api/v1/services/com-zizdog-filebrowser/filebrowser-password",
		map[string]string{"password": "Zz_Custom_Pass_1234"})
	if code != http.StatusAccepted {
		t.Fatalf("应是 202，实际 %d", code)
	}
	meta := waitTaskDone(t, g.srv, id).Meta()
	if string(meta.Status) != "failed" {
		t.Fatalf("CLI 失败时任务必须 failed，实际 %v", meta.Status)
	}
	if !strings.Contains(meta.Error, "改口令失败") {
		t.Errorf("失败原因应点名改口令失败，实际：%s", meta.Error)
	}
	fake.mu.Lock()
	listening := fake.listening
	fake.mu.Unlock()
	if !listening {
		t.Error("CLI 失败后服务没有被起回来（端口仍不在监听）")
	}
	idx := g.callIndexes("users update zizdog", "bootstrap system")
	if idx[0] < 0 || idx[1] < idx[0] {
		t.Errorf("失败路径也必须 改 → 起 回来，实际：%v", fake.calls)
	}
	if strings.Contains(meta.Error, "Zz_Custom_Pass_1234") {
		t.Errorf("失败原因里出现了口令：%s", meta.Error)
	}
}

// TestFilebrowserResetPasswordFailsWhenHealthNotOK：health 不过 ⇒ 报失败，不许报成功。
func TestFilebrowserResetPasswordFailsWhenHealthNotOK(t *testing.T) {
	fake := &fakeFilebrowserCLI{
		listening: true, usersLS: fbUsersLSTable,
		configCat: "Server:\n  Root:  /Users/me\n", healthCode: 500, loginCode: 200,
	}
	g := setupFilebrowserGate(t, fake)

	_, id, _ := g.postReset(t, "/api/v1/services/com-zizdog-filebrowser/filebrowser-password",
		map[string]string{"password": "Zz_Custom_Pass_1234"})
	meta := waitTaskDone(t, g.srv, id).Meta()
	if string(meta.Status) != "failed" {
		t.Fatalf("health 不是 200 时必须 failed，实际 %v", meta.Status)
	}
	if !strings.Contains(meta.Error, "健康检查") || !strings.Contains(meta.Error, "未通过验证") {
		t.Errorf("失败原因应说清是健康检查未过，实际：%s", meta.Error)
	}
}

// TestFilebrowserResetPasswordFailsWhenLoginNotOK：health 过了但新口令登录不过 ⇒ 报失败。
func TestFilebrowserResetPasswordFailsWhenLoginNotOK(t *testing.T) {
	fake := &fakeFilebrowserCLI{
		listening: true, usersLS: fbUsersLSTable,
		configCat: "Server:\n  Root:  /Users/me\n", healthCode: 200, loginCode: 403,
	}
	g := setupFilebrowserGate(t, fake)

	_, id, _ := g.postReset(t, "/api/v1/services/com-zizdog-filebrowser/filebrowser-password",
		map[string]string{"password": "Zz_Custom_Pass_1234"})
	meta := waitTaskDone(t, g.srv, id).Meta()
	if string(meta.Status) != "failed" {
		t.Fatalf("新口令登录不过时必须 failed，实际 %v", meta.Status)
	}
	if !strings.Contains(meta.Error, "登录") || !strings.Contains(meta.Error, "未通过验证") {
		t.Errorf("失败原因应说清是登录验证未过，实际：%s", meta.Error)
	}
}

// TestFilebrowserResetPasswordReportsStartFailure：起不回来必须单独说清。
func TestFilebrowserResetPasswordReportsStartFailure(t *testing.T) {
	fake := &fakeFilebrowserCLI{
		listening: true, usersLS: fbUsersLSTable, failBootstrap: true,
		configCat: "Server:\n  Root:  /Users/me\n", healthCode: 200, loginCode: 200,
	}
	g := setupFilebrowserGate(t, fake)

	_, id, _ := g.postReset(t, "/api/v1/services/com-zizdog-filebrowser/filebrowser-password",
		map[string]string{"password": "Zz_Custom_Pass_1234"})
	meta := waitTaskDone(t, g.srv, id).Meta()
	if string(meta.Status) != "failed" {
		t.Fatalf("起不回来时必须 failed，实际 %v", meta.Status)
	}
	if !strings.Contains(meta.Error, "重新启动服务失败") {
		t.Errorf("失败原因应单独点明起服务失败，实际：%s", meta.Error)
	}
}

// TestFilebrowserResetPasswordRootMismatchWarnsButKeepsSuccess：
// root 回读不一致要如实警告，但不否定已经成功的口令重置。
func TestFilebrowserResetPasswordRootMismatchWarnsButKeepsSuccess(t *testing.T) {
	fake := &fakeFilebrowserCLI{
		listening: true, usersLS: fbUsersLSTable,
		configCat: "Server:\n  Root:  /somewhere/else\n", healthCode: 200, loginCode: 200,
	}
	g := setupFilebrowserGate(t, fake)

	_, id, _ := g.postReset(t, "/api/v1/services/com-zizdog-filebrowser/filebrowser-password",
		map[string]string{"password": "Zz_Custom_Pass_1234"})
	meta := waitTaskDone(t, g.srv, id).Meta()
	if string(meta.Status) != "succeeded" {
		t.Fatalf("口令与登录都过了就该成功，实际 %v：%s", meta.Status, meta.Error)
	}
	res, _ := meta.Result.(*services.InstallResult)
	if res == nil || !strings.Contains(res.Warning, "未复核") {
		t.Errorf("root 回读不一致必须如实警告，实际 warning=%q", res.Warning)
	}
}

// TestFilebrowserResetPasswordRequiresAdmin：非管理员 403。
func TestFilebrowserResetPasswordRequiresAdmin(t *testing.T) {
	fake := &fakeFilebrowserCLI{listening: true, usersLS: fbUsersLSTable, healthCode: 200, loginCode: 200}
	g := setupFilebrowserGate(t, fake)

	if _, err := g.srv.Auth.CreateUser(t.Context(), "viewer", "viewer-pass-123456", false); err != nil {
		t.Fatal(err)
	}
	_, _, viewerCookies := doJSON(t, g.ts, "POST", "/api/v1/login",
		map[string]string{"username": "viewer", "password": "viewer-pass-123456"}, nil)

	res, out, _ := doJSON(t, g.ts, "POST",
		"/api/v1/services/com-zizdog-filebrowser/filebrowser-password",
		map[string]string{"password": "Zz_Custom_Pass_1234"}, viewerCookies)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("非管理员必须 403，实际 %d：%v", res.StatusCode, out)
	}
	if len(fake.calls) != 0 {
		t.Errorf("被拒的请求不该执行任何命令，实际：%v", fake.calls)
	}
}

// TestFilebrowserResetPasswordRejectsNonFilebrowserService：{name} 不是 File Browser 时拒绝。
func TestFilebrowserResetPasswordRejectsNonFilebrowserService(t *testing.T) {
	fake := &fakeFilebrowserCLI{listening: true, usersLS: fbUsersLSTable, healthCode: 200, loginCode: 200}
	g := setupFilebrowserGate(t, fake)

	if err := g.srv.serviceRepo.Create(t.Context(), &services.Service{
		Name: "nginx-x", DisplayName: "nginx", Kind: services.KindNative,
		LaunchLabel: "homebrew.mxcl.nginx", Managed: true,
	}); err != nil {
		t.Fatal(err)
	}
	res, out, _ := doJSON(t, g.ts, "POST", "/api/v1/services/nginx-x/filebrowser-password",
		map[string]string{"password": "Zz_Custom_Pass_1234"}, g.cook)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("目标不是 File Browser 时必须拒绝（400），实际 %d：%v", res.StatusCode, out)
	}
	if len(fake.calls) != 0 {
		t.Errorf("被拒的请求不该执行任何命令，实际：%v", fake.calls)
	}
}

// TestFrontendFilebrowserResetPasswordButtonWired 静态锁住前端接线：
// 卡片上有「重置口令」按钮、走确认框与任务中心、口令不落 localStorage/URL。
func TestFrontendFilebrowserResetPasswordButtonWired(t *testing.T) {
	js := readAssetJS(t, "services.js")
	mustContain(t, "services.js", js, "🔑 重置口令")
	mustContain(t, "services.js", js, "async function resetFilebrowserPassword(s)")
	mustContain(t, "services.js", js, "confirmBox(")
	mustContain(t, "services.js", js, "api.resetFilebrowserPassword(s.name, pwd)")
	mustContain(t, "services.js", js, "taskCenter.start({")

	// 函数体里不许出现 localStorage / URL 拼接口令。
	m := regexp.MustCompile(`(?s)async function resetFilebrowserPassword\(s\) \{.*?\n\}`).FindString(js)
	if m == "" {
		t.Fatal("找不到 resetFilebrowserPassword 函数体")
	}
	for _, bad := range []string{"localStorage", "sessionStorage", "location", "?password="} {
		if strings.Contains(m, bad) {
			t.Errorf("口令处理里不该出现 %s", bad)
		}
	}
	apiJS := readAssetJS(t, "api.js")
	mustContain(t, "api.js", apiJS, "/filebrowser-password")
}

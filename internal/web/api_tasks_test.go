package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  任务中心接口（SPEC-任务中心.md）
//
//  这些测试**不触发任何真实安装**：任务体由测试自己用 srv.Tasks.Start 造，
//  只验证接口行为（列表/详情/SSE/中断）与"异步安装"的约定。
//  真实安装会动用户机器上的 brew/launchd，绝不能在单测里跑。
// ============================================================================

// startStubTask 造一个受测试控制的任务。
func startStubTask(t *testing.T, srv *Server, target string, release <-chan struct{}) *tasks.Task {
	t.Helper()
	return srv.Tasks.Start("install", target, "安装 "+target, func(ctx context.Context, log tasks.LogFunc) (any, error) {
		log(tasks.LevelStep, "第一步")
		log(tasks.LevelOut, "正在下载 demo 包")
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				// 真实安装代码这里是被 exec.CommandContext 杀掉子进程后返回错误
				return nil, ctx.Err()
			}
		}
		log(tasks.LevelOK, "都好了")
		return map[string]any{"token": "tk-demo"}, nil
	})
}

func TestTasksListAndDetail(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	done := make(chan struct{})
	close(done)
	task := startStubTask(t, srv, "demo-app", nil)

	// 等任务结束，确保列表里能看到终态
	select {
	case <-task.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("任务没结束")
	}

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/tasks", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("列表应 200，实际 %d", res.StatusCode)
	}
	data, _ := out["data"].(map[string]any)
	list, _ := data["tasks"].([]any)
	if len(list) != 1 {
		t.Fatalf("应返回 1 个任务，实际 %d", len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["id"] != task.ID() || first["status"] != "succeeded" {
		t.Errorf("列表里的任务不对: %v", first)
	}
	if first["line_count"].(float64) < 4 {
		t.Errorf("应带上行数（前端要显示规模），实际 %v", first["line_count"])
	}
	// 列表的 last 是**最后一行日志**（含收尾行）—— 用于"一眼看它现在在干什么"
	if asString(first["last"]) == "" {
		t.Error("列表应带上最后一行日志")
	}
	// 关键：成功任务的业务结果必须能取到（前端要展示 token/address）
	if first["result"] == nil {
		t.Error("成功的任务应保留 result")
	}

	// 详情：日志 + 续传游标
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/tasks/"+task.ID(), nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("详情应 200，实际 %d", res.StatusCode)
	}
	detail, _ := out["data"].(map[string]any)
	lines, _ := detail["lines"].([]any)
	if len(lines) < 4 {
		t.Fatalf("详情应带日志，实际 %d 行", len(lines))
	}
	next := int(detail["next_after"].(float64))
	if next != len(lines) {
		t.Errorf("next_after 应等于最后一行 seq，实际 %d（共 %d 行）", next, len(lines))
	}
	if detail["done"] != true {
		t.Error("已结束的任务 done 应为 true")
	}
	if int(detail["oldest_seq"].(float64)) != 1 {
		t.Error("没有回绕时 oldest_seq 应为 1")
	}

	// after= 增量：不应重复给已经拿过的行
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/tasks/"+task.ID()+"?after="+itoa(next), nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("增量拉取应 200，实际 %d", res.StatusCode)
	}
	detail, _ = out["data"].(map[string]any)
	if got, _ := detail["lines"].([]any); len(got) != 0 {
		t.Errorf("after 之后不应再有行，实际 %d 行", len(got))
	}
}

func TestTaskDetailNotFound(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/tasks/t-不存在", nil, cookies)
	if res.StatusCode != 404 {
		t.Errorf("不存在的任务应 404，实际 %d", res.StatusCode)
	}
	if out["ok"] != false {
		t.Error("失败响应 ok 应为 false")
	}
}

// TestTaskStreamSendsMetaLinesAndStatus 是进度窗的契约：
// 先 meta，再 lines，结束时 status。
func TestTaskStreamSendsMetaLinesAndStatus(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	release := make(chan struct{})
	task := startStubTask(t, srv, "stream-app", release)

	req, err := http.NewRequest("GET", ts.URL+"/api/v1/tasks/"+task.ID()+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Fatalf("SSE 应 200，实际 %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 event-stream，实际 %q", ct)
	}

	events := readSSEUntil(t, res.Body, "status", 10, func() { close(release) })
	// 顺序：meta → lines ...（结束时先把剩余行 drain 出来）→ status
	if events[0].name != "meta" {
		t.Errorf("第一个事件应为 meta，实际 %q", events[0].name)
	}
	if !strings.Contains(events[0].data, `"title"`) {
		t.Errorf("meta 事件应带任务元信息，实际 %s", events[0].data)
	}
	if events[1].name != "lines" {
		t.Errorf("第二个事件应为 lines，实际 %q", events[1].name)
	}
	if !strings.Contains(events[1].data, "正在下载 demo 包") {
		t.Errorf("lines 事件应带日志文本，实际 %s", events[1].data)
	}
	if events[1].id == "" {
		t.Error("lines 事件必须带 id（EventSource 靠它做 Last-Event-ID 续传）")
	}
	last := events[len(events)-1]
	if last.name != "status" {
		t.Errorf("最后一个事件应为 status，实际 %q", last.name)
	}
	if !strings.Contains(last.data, `"succeeded"`) {
		t.Errorf("结束时应报 succeeded，实际 %s", last.data)
	}
}

// TestTaskStreamResumesFromLastEventID 是"关掉窗口再打开"的核心：
// 带 Last-Event-ID 重连时只补之后的行，不从头再来。
func TestTaskStreamResumesFromLastEventID(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	task := startStubTask(t, srv, "resume-app", nil)
	select {
	case <-task.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("任务没结束")
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/tasks/"+task.ID()+"/stream", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	req.Header.Set("Last-Event-ID", "3") // 假装前端已经收到 seq=3
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	events := readSSE(t, res.Body, 2, nil)
	var joined string
	for _, e := range events {
		if e.name == "lines" {
			joined += e.data
		}
	}
	for _, old := range []string{"第一步", "正在下载 demo 包"} {
		if strings.Contains(joined, old) {
			t.Errorf("Last-Event-ID=3 之后不该再补发 %q（会造成重复日志）：%s", old, joined)
		}
	}
	if !strings.Contains(joined, "都好了") {
		t.Errorf("应补发 seq>3 的行，实际 %s", joined)
	}
}

// TestTaskCancelEndpoint 验证中断接口与错误语义。
func TestTaskCancelEndpoint(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	release := make(chan struct{})
	defer close(release)
	task := startStubTask(t, srv, "cancel-app", release)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/tasks/"+task.ID()+"/cancel", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("中断应 200，实际 %d: %v", res.StatusCode, out)
	}
	select {
	case <-task.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("中断后任务应结束")
	}
	if got := task.Status(); got != tasks.StatusCanceled {
		t.Errorf("中断后状态应为 canceled，实际 %s", got)
	}

	// 再中断一次必须如实报错（不能假装成功）
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/tasks/"+task.ID()+"/cancel", nil, cookies)
	if res.StatusCode == 200 {
		t.Error("已结束的任务再次中断应当失败")
	}
}

// TestInstallEndpointIsAsyncAndGuarded 锁住"异步安装"的两条约定：
//   - 返回 202 + task_id（而不是傻等几分钟）
//   - 同一个应用已有任务在跑时返回 409，不重复启动
//
// 注意：这里**不会真的安装**，因为目标应用先用一个占位任务占住了。
func TestInstallEndpointIsAsyncAndGuarded(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// 未知应用：同步 400（不该为此建任务）
	res, _, _ := doJSON(t, ts, "POST", "/api/v1/market/不存在的应用/install", nil, cookies)
	if res.StatusCode != 400 {
		t.Errorf("未知应用应 400，实际 %d", res.StatusCode)
	}

	// 用占位任务占住 phpmyadmin，模拟"已经在装"
	release := make(chan struct{})
	defer close(release)
	stub := startStubTask(t, srv, "phpmyadmin", release)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/market/phpmyadmin/install", nil, cookies)
	if res.StatusCode != 409 {
		t.Fatalf("已有任务在跑时应 409（不重复启动），实际 %d: %v", res.StatusCode, out)
	}
	if msg := asString(out["msg"]); !strings.Contains(msg, "进行中") {
		t.Errorf("409 的提示要说清原因，实际 %q", msg)
	}
	if srv.Tasks.RunningFor("phpmyadmin").ID() != stub.ID() {
		t.Error("占位任务不该被顶掉")
	}
	_ = stub
}

// TestServiceUninstallIsAsync 验证卸载也走任务（卸载 docker 应用可能要好几分钟）。
func TestServiceUninstallIsAsync(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// 不存在的服务名 → 任务会失败，但接口本身必须是 202 + task_id
	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/services/不存在的服务/uninstall", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("卸载应立刻返回 202（长任务），实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	id := asString(data["task_id"])
	if id == "" {
		t.Fatalf("应返回 task_id，实际 %v", out)
	}
	task := srv.Tasks.Get(id)
	if task == nil {
		t.Fatal("task_id 必须能在任务中心查到")
	}
	select {
	case <-task.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("卸载不存在服务的任务应快速失败")
	}
	if task.Status() != tasks.StatusFailed {
		t.Errorf("卸载不存在的服务应失败，实际 %s", task.Status())
	}
	if !strings.Contains(task.Meta().Error, "不存在") {
		t.Errorf("失败原因应说清服务不存在，实际 %q", task.Meta().Error)
	}
}

// ---------- 小工具 ----------

type sseEvent struct {
	name string
	id   string
	data string
}

// readSSE 读 n 个事件（跳过心跳注释）。after 在读到第一个事件后触发，
// 用来"放行"被测试卡住的任务。
func readSSE(t *testing.T, body interface{ Read([]byte) (int, error) }, n int, after func()) []sseEvent {
	t.Helper()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var events []sseEvent
	var cur sseEvent
	fired := false
	deadline := time.Now().Add(15 * time.Second)
	for len(events) < n && time.Now().Before(deadline) {
		if !sc.Scan() {
			break
		}
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"):
			continue // 心跳
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.name == "" {
				continue
			}
			events = append(events, cur)
			cur = sseEvent{}
			if !fired && after != nil {
				fired = true
				after()
			}
		}
	}
	if len(events) < n {
		t.Fatalf("只读到 %d 个 SSE 事件（期望 %d）", len(events), n)
	}
	return events
}

// readSSEUntil 读到 name 事件为止（最多 max 个），用于"末尾事件"类断言。
func readSSEUntil(t *testing.T, body interface{ Read([]byte) (int, error) }, name string, max int, after func()) []sseEvent {
	t.Helper()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var events []sseEvent
	var cur sseEvent
	fired := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			break
		}
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.name == "" {
				continue
			}
			events = append(events, cur)
			done := cur.name == name
			cur = sseEvent{}
			if !fired && after != nil {
				fired = true
				after()
			}
			if done || len(events) >= max {
				return events
			}
		}
	}
	t.Fatalf("没读到事件 %q（已读 %d 个）", name, len(events))
	return events
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestComposeUpIsAsync 验证 Docker 页的「部署」也走任务中心。
//
// 用一个**不存在的项目名**：接口必须立刻 202 + task_id（而不是傻等镜像拉取），
// 真正执行时快速失败并把原因写进任务 —— 全程不碰 Docker。
func TestComposeUpIsAsync(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/compose/不存在的项目/actions?action=up", nil, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("部署应立刻返回 202，实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	task := srv.Tasks.Get(asString(data["task_id"]))
	if task == nil {
		t.Fatal("应能在任务中心查到该任务")
	}
	if task.Meta().Kind != "deploy" {
		t.Errorf("compose 部署的任务类型应为 deploy，实际 %q", task.Meta().Kind)
	}
	select {
	case <-task.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("不存在的项目应快速失败")
	}
	if task.Status() != tasks.StatusFailed {
		t.Errorf("不存在的项目应失败，实际 %s", task.Status())
	}

	// 秒级动作（stop）必须仍然是同步的：前端依赖即时返回
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/docker/compose/不存在的项目/actions?action=stop", nil, cookies)
	if res.StatusCode == 202 {
		t.Error("stop 是秒级动作，不该也变成异步任务")
	}
}

// TestForgetMissingServiceIs404 锁住一个语义细节：
// 删除（Forget）一个**不存在**的服务记录是 404，不是 500。
//
// 原本这里是 500 —— 后果不只是语义难看：UI 测试/清理脚本调用它清理
// "上一次可能留下的测试服务"时，会凭空冒出一条假的服务端 500 错误，
// 让整轮 UI 验证因为一条不存在的记录而失败。
func TestForgetMissingServiceIs404(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/services/no-such-service-here", nil, cookies)
	if res.StatusCode != 404 {
		t.Fatalf("删除不存在的服务应是 404，实际 %d: %v", res.StatusCode, out)
	}
	if !strings.Contains(asString(out["msg"]), "不存在") {
		t.Errorf("404 的说明应写清「服务不存在」，实际 %q", out["msg"])
	}
}

// ============================================================================
//  任务输入通道（安装时限时询问：如 MySQL root 口令）
//
//  这些测试只用测试自己造的任务，不触发任何真实安装、也不碰真实 MySQL。
// ============================================================================

// TestTaskInputEndpointAndSSEEvent 覆盖前端契约：
//   - SSE 里出现 `input_required` 事件，带 key/label/倒计时/截止时间；
//   - POST /api/v1/tasks/{id}/input 能投递；
//   - 落定后再发一次 `input_required`（值为 null）让前端收起输入框；
//   - 晚到的/答错题的任务输入明确报错（不静默吞掉）。
func TestTaskInputEndpointAndSSEEvent(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	const secret = "Secret-From-UI-123456"
	got := make(chan string, 1)
	task := srv.Tasks.StartWithTask("install", "mysql84", "安装 MySQL 8.4",
		func(ctx context.Context, tk *tasks.Task) (any, error) {
			v, ok := tk.WaitInput(ctx, tasks.InputRequest{
				Key: "mysql_root_password", Label: "MySQL root 口令", Secret: true,
			}, 20*time.Second)
			if !ok {
				return nil, nil
			}
			got <- v
			// 刻意不把值放进返回值：任务的 result 会随 GET/SSE 反复下发。
			return map[string]any{"saved": true}, nil
		})
	// 等任务真的在等输入，再连 SSE —— 这样 meta 事件里就带着 input_required。
	waitTaskPending(t, task)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/tasks/"+task.ID()+"/stream", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	// ① 首次 meta 事件里应带结构化的 input_required（前端据此渲染输入框+倒计时）
	events := readSSE(t, res.Body, 1, nil)
	if events[0].name != "meta" {
		t.Fatalf("第一个事件应为 meta，实际 %q", events[0].name)
	}
	if !strings.Contains(events[0].data, `"input_required"`) ||
		!strings.Contains(events[0].data, `"mysql_root_password"`) {
		t.Fatalf("meta 里应带 input_required，实际 %s", events[0].data)
	}
	if !strings.Contains(events[0].data, `"deadline"`) || !strings.Contains(events[0].data, `"timeout_seconds"`) {
		t.Errorf("input_required 应带 deadline 与 timeout_seconds（前端要显示倒计时）：%s", events[0].data)
	}

	// ② 投递口令
	res2, out2, _ := doJSON(t, ts, "POST", "/api/v1/tasks/"+task.ID()+"/input",
		map[string]any{"key": "mysql_root_password", "value": secret}, cookies)
	if res2.StatusCode != 200 {
		t.Fatalf("投递应 200，实际 %d: %v", res2.StatusCode, out2)
	}
	// 响应里绝不能回显值
	if body, _ := json.Marshal(out2); strings.Contains(string(body), secret) {
		t.Error("投递接口的响应里回显了口令")
	}
	select {
	case v := <-got:
		if v != secret {
			t.Fatalf("任务收到的值不对: %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("任务没收到输入")
	}

	// ③ 落定事件：input_required 应为 null，input_result 说明结局
	events = readSSEUntil(t, res.Body, "input_required", 20, nil)
	last := events[len(events)-1]
	if !strings.Contains(last.data, `"input_required":null`) {
		t.Errorf("落定后应下发 input_required:null（前端据此收起输入框），实际 %s", last.data)
	}
	if !strings.Contains(last.data, `"submitted"`) {
		t.Errorf("input_result 应为 submitted，实际 %s", last.data)
	}
	if strings.Contains(last.data, secret) {
		t.Errorf("SSE 事件里出现了口令：%s", last.data)
	}

	// ④ 再投一次：任务已经不在等这个 key（甚至可能已经结束）→ 必须报错
	res3, _, _ := doJSON(t, ts, "POST", "/api/v1/tasks/"+task.ID()+"/input",
		map[string]any{"key": "mysql_root_password", "value": "again"}, cookies)
	if res3.StatusCode == 200 {
		t.Error("重复投递不该成功")
	}

	// ⑤ 不存在的任务 / 缺 key：明确报错
	res4, _, _ := doJSON(t, ts, "POST", "/api/v1/tasks/t-nope/input",
		map[string]any{"key": "mysql_root_password", "value": "x"}, cookies)
	if res4.StatusCode != 404 {
		t.Errorf("不存在的任务应 404，实际 %d", res4.StatusCode)
	}
	res5, _, _ := doJSON(t, ts, "POST", "/api/v1/tasks/"+task.ID()+"/input",
		map[string]any{"value": "x"}, cookies)
	if res5.StatusCode != 400 {
		t.Errorf("缺 key 应 400，实际 %d", res5.StatusCode)
	}

	select {
	case <-task.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("收到输入后任务应继续跑完")
	}
	// ⑥ 任务详情（GET）里也绝不能有口令
	res6, out6, _ := doJSON(t, ts, "GET", "/api/v1/tasks/"+task.ID(), nil, cookies)
	if res6.StatusCode != 200 {
		t.Fatalf("详情应 200，实际 %d", res6.StatusCode)
	}
	body, _ := json.Marshal(out6)
	if strings.Contains(string(body), secret) {
		t.Error("GET /api/v1/tasks/{id} 的返回里出现了口令")
	}
}

// TestTaskInputWrongKeyRejected 答错题（key 不匹配）必须报错并保留仍可提交。
func TestTaskInputWrongKeyRejected(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	task := srv.Tasks.StartWithTask("install", "mysql84", "安装 MySQL 8.4",
		func(ctx context.Context, tk *tasks.Task) (any, error) {
			tk.WaitInput(ctx, tasks.InputRequest{Key: "mysql_root_password", Label: "口令"}, 20*time.Second)
			return nil, nil
		})
	waitTaskPending(t, task)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/tasks/"+task.ID()+"/input",
		map[string]any{"key": "wrong_key", "value": "x"}, cookies)
	if res.StatusCode != 409 {
		t.Fatalf("答错题应 409，实际 %d: %v", res.StatusCode, out)
	}
	if !strings.Contains(asString(out["msg"]), "mysql_root_password") {
		t.Errorf("报错要说明当前在等哪个 key，实际 %q", out["msg"])
	}
	// 正确的 key 仍然可以提交
	res2, _, _ := doJSON(t, ts, "POST", "/api/v1/tasks/"+task.ID()+"/input",
		map[string]any{"key": "mysql_root_password", "value": "pw-123456789"}, cookies)
	if res2.StatusCode != 200 {
		t.Errorf("正确的 key 应被接受，实际 %d", res2.StatusCode)
	}
	select {
	case <-task.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("任务应结束")
	}
}

// waitTaskPending 等任务进入"等待输入"状态。
func waitTaskPending(t *testing.T, task *tasks.Task) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if task.PendingInput() != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("任务没有进入等待输入状态")
}

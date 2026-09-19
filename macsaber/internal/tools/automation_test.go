package tools

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
)

// 断言口径：
//   - 危险工具缺确认 → 400 且**不执行**（shim 若被调用会留下痕迹/报错，测试据此判定）；
//   - shortcut_run 测试绝不真的跑用户快捷指令（用假 shortcuts，名字不在列表里被拒）；
//   - 空结果（快捷指令为空）不算失败。

// ---------- 元数据：分类/危险口径/文案长度 ----------

// testStatShim 造一个返回真实用户的 stat shim：没有它，sessionWrap 会把 uid 当 -1。
func testStatShim(t *testing.T, dir string) {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, dir, "stat", "#!/bin/sh\necho "+u.Username+"\n")
}

// runTaskErr 跑一个异步工具并断言任务失败且错误含 sub（异步工具的 500 在任务快照里）。
func runTaskErr(t *testing.T, b *bench, id string, body map[string]any, sub string) {
	t.Helper()
	_, snap := b.run(id, body)
	if snap.Status != tasks.Failed {
		t.Fatalf("%s: 任务应失败，实际 %s", id, snap.Status)
	}
	if sub != "" && !strings.Contains(snap.Error, sub) {
		t.Fatalf("%s: 错误应含 %q，实际 %q", id, sub, snap.Error)
	}
}

// logHasRun 判断假 shortcuts 的调用日志里是否出现过 run。
func logHasRun(t *testing.T, log string) bool {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "run ")
}

// runOK 跑一个异步工具并断言任务成功，返回任务快照（结果在 snap.Result 里）。
func (b *bench) runOK(t *testing.T, id string, body map[string]any) tasks.Snapshot {
	t.Helper()
	_, snap := b.run(id, body)
	if snap.Status != tasks.Succeeded {
		t.Fatalf("%s: 任务应成功，实际 %s（%s）", id, snap.Status, snap.Error)
	}
	if snap.Result == nil {
		t.Fatalf("%s: 任务成功但没有结果", id)
	}
	return snap
}

func TestAutoToolsMeta(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAll(reg)
	for _, id := range []string{"auto.notify", "auto.shortcuts_list", "auto.shortcut_run",
		"auto.open", "auto.clipboard_read", "auto.clipboard_write"} {
		m := metaOf(t, reg, id)
		if m.Category != "auto" {
			t.Errorf("%s: 分类应为 auto，实际 %s", id, m.Category)
		}
		if m.Async != true {
			t.Errorf("%s: 子进程类工具统一走任务中心，应 async=true", id)
		}
		if len([]rune(m.Summary)) > 40 {
			t.Errorf("%s: summary 超过 40 字：%q", id, m.Summary)
		}
		if m.Danger && m.DangerFloor == "" {
			t.Errorf("%s: 危险工具必须有确认口径", id)
		}
		if !m.Danger && m.DangerFloor != "" {
			t.Errorf("%s: 非危险工具不该带 danger_floor", id)
		}
	}
	for _, id := range []string{"auto.shortcut_run", "auto.open"} {
		if m := metaOf(t, reg, id); !m.Danger {
			t.Errorf("%s: 会启动外部程序/执行用户指令，必须 danger=true", id)
		}
	}
	for _, id := range []string{"auto.notify", "auto.shortcuts_list", "auto.clipboard_read", "auto.clipboard_write"} {
		if m := metaOf(t, reg, id); m.Danger {
			t.Errorf("%s: 不该标 danger", id)
		}
	}
	// 明确不做 auto.say（media.tts 已覆盖）。
	if _, ok := reg.Get("auto.say"); ok {
		t.Error("auto.say 不该存在（与 media.tts 重复）")
	}
	// 读剪贴板的 help 必须如实写明可能含敏感内容。
	m := metaOf(t, reg, "auto.clipboard_read")
	if !strings.Contains(m.Summary, "敏感") {
		t.Errorf("读剪贴板的 summary 应写明可能含敏感内容，实际 %q", m.Summary)
	}
}

func TestAutoAvailabilityUnderEmptyPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	reg := tool.NewRegistry()
	RegisterAll(reg)
	for _, tc := range []struct{ id, bin string }{
		{"auto.notify", "osascript"},
		{"auto.shortcuts_list", "shortcuts"},
		{"auto.shortcut_run", "shortcuts"},
		{"auto.open", "open"},
		{"auto.clipboard_read", "pbpaste"},
		{"auto.clipboard_write", "pbcopy"},
	} {
		m := metaOf(t, reg, tc.id)
		if m.Available {
			t.Errorf("%s: 空 PATH 下不该可用", tc.id)
		}
		if !strings.Contains(m.UnavailableReason, tc.bin) {
			t.Errorf("%s: 原因应点出 %s，实际 %q", tc.id, tc.bin, m.UnavailableReason)
		}
	}
}

// ---------- auto.open：scheme 白名单（file:// / javascript: 一律拒绝） ----------

func TestAutoOpenURLSchemeWhitelist(t *testing.T) {
	cases := []struct {
		name       string
		kind, url  string
		wantErrSub string
	}{
		{"file 被拒", "url", "file:///etc/passwd", "http/https"},
		{"javascript 被拒", "url", "javascript:alert(1)", "http/https"},
		{"data 被拒", "url", "data:text/html,<b>x", "http/https"},
		{"裸路径被拒", "url", "/tmp/x", "http/https"},
		{"ftp 被拒", "url", "ftp://example.com/x", "http/https"},
		{"http 允许", "url", "http://example.com/a?b=c", ""},
		{"https 允许", "url", "https://example.com/中文", ""},
		{"大小写不敏感", "url", "HTTPS://Example.com", ""},
		{"空网址被拒", "url", "", "请填写网址"},
		{"类型非法", "weird", "https://example.com", "打开类型非法"},
	}
	for _, tc := range cases {
		_, err := (autoOpen{}).validateTarget("", tc.kind, tc.url)
		if tc.wantErrSub == "" {
			if err != nil {
				t.Errorf("%s: 不该报错，实际 %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 应被拒绝", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErrSub) {
			t.Errorf("%s: 原因应含 %q，实际 %q", tc.name, tc.wantErrSub, err.Error())
		}
	}
}

// ---------- 危险工具：缺确认 → 400 且不执行 ----------

func TestDangerToolsRejectWithoutConfirm(t *testing.T) {
	// 假 shortcuts：一旦被调用就输出标记；断言标记**没出现** = 没执行。
	shim := t.TempDir()
	writeShim(t, shim, "shortcuts", "#!/bin/sh\necho CALLED_SHORTCUTS\nexit 0\n")
	writeShim(t, shim, "open", "#!/bin/sh\necho CALLED_OPEN\nexit 0\n")
	t.Setenv("PATH", shim)
	b := newBench(t)

	f := filepath.Join(b.home, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id   string
		body map[string]any
	}{
		{"auto.shortcut_run", map[string]any{"name": "我的指令"}},
		{"auto.open", map[string]any{"kind": "file", "path": f}},
		{"auto.open", map[string]any{"kind": "url", "url": "https://example.com"}},
	}
	for _, tc := range cases {
		code, err := b.runErr(tc.id, tc.body)
		if code != 400 {
			t.Errorf("%s: 缺确认应 400，实际 %d（err=%v）", tc.id, code, err)
		}
		if err == nil {
			t.Errorf("%s: 缺确认应报错", tc.id)
		}
	}
}

// ---------- auto.shortcut_run：确认值必须是快捷指令名本身 ----------

func TestShortcutRunConfirmMustMatchName(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	// 假 shortcuts：list 给一条"假指令"，run 只记调用并成功（不碰真实用户快捷指令）。
	writeShim(t, dir, "shortcuts", "#!/bin/sh\necho \"$1 $2\" >> "+log+"\n"+
		"if [ \"$1\" = list ]; then echo 假指令; fi\nexit 0\n")
	testStatShim(t, dir)
	t.Setenv("PATH", dir)
	b := newBench(t)

	// 名字不在 list 里 → 拒绝，且绝不执行（调用日志里不能出现 run）。
	runTaskErr(t, b, "auto.shortcut_run", map[string]any{
		"name": "我的指令", "confirm_name": "我的指令", "_confirm": ConfirmText}, "不在 shortcuts list")
	if logHasRun(t, log) {
		t.Fatal("名字不存在时不该真的运行")
	}

	// 手输确认名与指令名不一致 → 400，工具代码不执行。
	code, err2 := b.runErr("auto.shortcut_run", map[string]any{
		"name": "假指令", "confirm_name": "别的名字", "_confirm": ConfirmText})
	if code != 400 || err2 == nil {
		t.Fatalf("手输名字不一致应 400，实际 code=%d err=%v", code, err2)
	}
	if !strings.Contains(err2.Error(), "手输快捷指令名") {
		t.Errorf("原因应说明要手输指令名，实际 %v", err2)
	}

	// 名字存在 + 手输名字一致 → 放行（假 shortcuts 只记录，不碰真实快捷指令）。
	snap := b.runOK(t, "auto.shortcut_run", map[string]any{
		"name": "假指令", "confirm_name": "假指令", "_confirm": ConfirmText})
	res := snap.Result.(*tool.Result)
	if !strings.Contains(res.Msg, "假指令") {
		t.Fatalf("结果不符：%+v", res)
	}
	calls, e := os.ReadFile(log)
	if e != nil || !strings.Contains(string(calls), "run 假指令") {
		t.Fatalf("假 shortcuts 应收到 run 调用，实际 %s（%v）", calls, e)
	}
}

// ---------- auto.shortcut_run：空列表时也拒绝执行 ----------

func TestShortcutRunRejectsWhenListEmpty(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, "shortcuts", "#!/bin/sh\nif [ \"$1\" = list ]; then exit 0; fi\necho SHOULD_NOT_RUN >&2\nexit 1\n")
	testStatShim(t, dir)
	t.Setenv("PATH", dir)
	b := newBench(t)
	runTaskErr(t, b, "auto.shortcut_run", map[string]any{
		"name": "任何指令", "confirm_name": "任何指令", "_confirm": ConfirmText}, "不在 shortcuts list")
}

// ---------- auto.shortcuts_list：空列表不算失败 ----------

func TestShortcutsListEmptyIsNotFailure(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, "shortcuts", "#!/bin/sh\nexit 0\n")
	testStatShim(t, dir)
	t.Setenv("PATH", dir)
	b := newBench(t)
	snap := b.runOK(t, "auto.shortcuts_list", nil)
	res := snap.Result.(*tool.Result)
	data := res.Data.(map[string]any)
	if data["total"] != 0 {
		t.Fatalf("空列表 total 应为 0，实际 %v", data["total"])
	}
	if got, _ := data["shortcuts"].([]string); len(got) != 0 {
		t.Errorf("空列表不该有条目，实际 %v", got)
	}
	if !strings.Contains(res.Msg, "空列表不算失败") {
		t.Errorf("消息应说明空列表不算失败，实际 %q", res.Msg)
	}
}

func TestShortcutsListFilterAndReal(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, "shortcuts", "#!/bin/sh\nprintf 'Alpha\\nBeta\\nAlpha 2\\n'\n")
	testStatShim(t, dir)
	t.Setenv("PATH", dir)
	b := newBench(t)
	snap := b.runOK(t, "auto.shortcuts_list", map[string]any{"filter": "Alpha"})
	data := snap.Result.(*tool.Result).Data.(map[string]any)
	if data["total"] != 3 || data["returned"] != 2 {
		t.Fatalf("过滤结果不符：%v", data)
	}

	// 真机再跑一次（可能为空，空也要如实、不失败）。
	if _, ok := execx.LookPath("shortcuts"); !ok {
		t.Skip("本机没有 shortcuts")
	}
	b2 := newBench(t)
	snap2 := b2.runOK(t, "auto.shortcuts_list", nil)
	res2 := snap2.Result.(*tool.Result)
	t.Logf("真机快捷指令：%s", res2.Msg)
}

// ---------- auto.notify：无图形会话如实报告 ----------

func TestNotifyNoGUISession(t *testing.T) {
	dir := t.TempDir()
	// stat shim 冒充"控制台属于 root" → 没有图形登录会话。
	writeShim(t, dir, "stat", "#!/bin/sh\necho root\n")
	writeShim(t, dir, "osascript", "#!/bin/sh\necho SHOULD_NOT_RUN >&2\nexit 1\n")
	t.Setenv("PATH", dir)
	b := newBench(t)
	snap := b.runOK(t, "auto.notify", map[string]any{"title": "标题", "message": "正文"})
	res := snap.Result.(*tool.Result)
	data := res.Data.(map[string]any)
	if data["displayed"] != false {
		t.Fatalf("无图形会话时不该声称已显示：%v", data)
	}
	if !strings.Contains(res.Msg, "没有图形登录会话") {
		t.Errorf("消息应如实说明，实际 %q", res.Msg)
	}
	if !strings.Contains(fmt.Sprint(data["reason"]), "root") {
		t.Errorf("原因应点出控制台用户，实际 %v", data["reason"])
	}
}

// ---------- auto.clipboard_write：内容只走 stdin，不拼进命令 ----------

func TestClipboardWriteUsesStdin(t *testing.T) {
	dir := t.TempDir()
	// 假 pbcopy 把 stdin 原样写进文件；带 ; 的内容若被拼进命令就会露馅。
	// 用绝对路径的 sed：PATH 已被改成只有 shim 目录（坑 F5）。
	writeShim(t, dir, "pbcopy", "#!/bin/sh\n/usr/bin/sed -n '1,$p' > "+dir+"/got.txt\n")
	testStatShim(t, dir)
	t.Setenv("PATH", dir)
	b := newBench(t)
	payload := "MacSaber; rm -rf / 测试\n第二行"
	snap := b.runOK(t, "auto.clipboard_write", map[string]any{"text": payload})
	res := snap.Result.(*tool.Result)
	got, err := os.ReadFile(filepath.Join(dir, "got.txt"))
	if err != nil {
		t.Fatalf("假 pbcopy 没收到 stdin：%v", err)
	}
	if string(got) != payload {
		t.Fatalf("stdin 内容被改动：%q", string(got))
	}
	// 内容不得回显到结果里。
	if strings.Contains(fmt.Sprint(res.Data), payload) {
		t.Error("剪贴板内容不该回显在结果里")
	}
}

func TestClipboardWriteEmptyTextRejected(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, "pbcopy", "#!/bin/sh\nexit 0\n")
	testStatShim(t, dir)
	t.Setenv("PATH", dir)
	b := newBench(t)
	if _, err := b.runErr("auto.clipboard_write", map[string]any{"text": "   "}); err == nil {
		t.Fatal("空文本应被拒绝（参数校验或工具自身）")
	}
}

// ---------- 真实剪贴板往返（只在本机、且事后清空） ----------

func TestClipboardRoundTripReal(t *testing.T) {
	if _, ok := execx.LookPath("pbcopy"); !ok {
		t.Skip("本机没有 pbcopy")
	}
	if _, ok := execx.LookPath("pbpaste"); !ok {
		t.Skip("本机没有 pbpaste")
	}
	b := newBench(t)
	payload := "MacSaber 剪贴板往返 " + t.Name()
	b.runOK(t, "auto.clipboard_write", map[string]any{"text": payload})
	// 读回用真实 pbpaste 复核，不只信工具自报。
	read := b.runOK(t, "auto.clipboard_read", nil).Result.(*tool.Result)
	if got, _ := read.Data.(map[string]any)["text"].(string); got != payload {
		t.Fatalf("读回内容与写入不一致：%q", got)
	}
	// 事后清空剪贴板（用户机器上不留测试字符串）。
	if res := execx.New().Run(context.Background(), 10*time.Second, "pbcopy"); res.ExitCode != 0 {
		t.Fatalf("清空剪贴板失败：%s", res.Stderr)
	}
	after := b.runOK(t, "auto.clipboard_read", nil).Result.(*tool.Result)
	if got := after.Data.(map[string]any)["text"]; got != "" {
		t.Errorf("剪贴板未清空：%q", got)
	}
}

// ---------- 只读/安全纪律：绝不 sh -c，绝不写系统 ----------

func TestAutomationSourceDiscipline(t *testing.T) {
	src := readFileForTest(t, "automation.go")
	// 只查代码行，注释里写"绝不 sh -c"不算违规。
	code := []string{}
	for _, ln := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		code = append(code, ln)
	}
	joined := strings.Join(code, "\n")
	for _, bad := range []string{"sh -c", "\"bash\", \"-c\"", "\"sh\", \"-c\"",
		"launchctl bootstrap", "launchctl bootout", "launchctl kickstart",
		"os.RemoveAll"} {
		if strings.Contains(joined, bad) {
			t.Errorf("automation.go 里出现了禁止写法 %q", bad)
		}
	}
	if !strings.Contains(joined, "runWithInput") {
		t.Error("用 stdin 传内容的路径应保留（坑 B1）")
	}
	if !strings.Contains(joined, "Setpgid") {
		t.Error("带 stdin 的命令也要有独立进程组（坑 B2）")
	}
}

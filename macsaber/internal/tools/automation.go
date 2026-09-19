package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// 自动化桥接：全部命令逐参传、绝不 sh -c；通知正文/剪贴板内容只走 argv 或 stdin。
// 无图形会话时如实报告（坑 T1）；读剪贴板读的是当前登录用户，可能含敏感内容。

// ----------------------------------------------------------------------------
// 图形会话探测（本仓库没有 files.ConsoleUser，自己用 stat -f %Su /dev/console 判）
// ----------------------------------------------------------------------------

// consoleSession 返回控制台登录用户名与 UID；"root"/空 表示没有图形登录会话。
func consoleSession(ctx context.Context, c *tool.Ctx) (name string, uid int, hasGUI bool) {
	res := c.Exec.Run(ctx, 8*time.Second, "stat", "-f", "%Su", "/dev/console")
	if res.TimedOut || res.ExitCode != 0 {
		return "", -1, false
	}
	name = strings.TrimSpace(res.Stdout)
	if name == "" || name == "root" {
		return name, -1, false
	}
	if u, err := user.Lookup(name); err == nil {
		if n, err := strconv.Atoi(u.Uid); err == nil {
			return name, n, true
		}
	}
	return name, -1, false
}

// sessionWrap 在 root 下用 launchctl asuser 把工具送进图形会话执行。
// 只做 launchctl 的 **asuser 执行**，绝不 bootstrap/bootout/kickstart（铁律）。
// 未验证项：本机以普通用户跑测（os.Getuid()!=0），root 分支未真机验证。
func sessionWrap(ctx context.Context, name string, uid int) (string, []string) {
	if os.Getuid() != 0 || uid <= 0 {
		return name, nil
	}
	return "launchctl", []string{"asuser", strconv.Itoa(uid), name}
}

// cmdOut 是 execx.Result 的最小替身：给需要 stdin 的命令用（execx 没有 stdin 口）。
type cmdOut struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

func (r cmdOut) Output() string {
	if strings.TrimSpace(r.Stdout) != "" {
		return r.Stdout
	}
	return r.Stderr
}

// runWithInput 跑一条命令并把 stdin 作为**数据**喂进去（绝不拼命令，坑 B1）。
// 进程组 kill 语义与 execx 一致：超时不能只杀直接子进程（坑 B2）。
func runWithInput(ctx context.Context, timeout time.Duration, stdin, name string, args ...string) cmdOut {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := cmdOut{Stdout: out.String(), Stderr: errb.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			if res.Stderr == "" {
				res.Stderr = err.Error()
			}
		}
		if cctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
			res.ExitCode = -1
			res.Stderr = strings.TrimRight(res.Stderr, "\n") + fmt.Sprintf("\n命令超过 %s 被强制终止", timeout)
		}
	}
	return res
}

// ============================================================================
//  auto.notify —— 发 macOS 通知（非危险）
// ============================================================================

type autoNotify struct{}

func init() { Add(autoNotify{}) }

func (autoNotify) Meta() tool.Meta {
	_, ok := execx.LookPath("osascript")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/osascript"
	}
	return tool.Meta{
		ID: "auto.notify", Name: "发送通知", Category: "auto", Icon: "bell",
		Summary:   "发一条 macOS 通知；无图形会话时不显示并如实说明。",
		Async:     true,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "title", Label: "标题", Type: tool.TypeText, Required: true,
				Placeholder: "Mac军刀", Help: "通知标题，一句话。"},
			{Name: "message", Label: "正文", Type: tool.TypeText, Required: true,
				Placeholder: "任务已完成", Help: "通知正文，支持中文。"},
			{Name: "subtitle", Label: "副标题", Type: tool.TypeText,
				Help: "可选，留空不显示。"},
			{Name: "sound", Label: "提示音", Type: tool.TypeText, Default: "default",
				Help: "留空静音；默认 default 用系统提示音。"},
		},
	}
}

// 通知脚本把正文/标题从 argv 取；argv 模式中文实测正常（真机往返核对）。
// 一旦 argv 变乱码就改读 UTF-8 临时文件（坑 T2）。

func (autoNotify) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	title := in.Str("title")
	body := in.Str("message")
	sub := in.Str("subtitle")
	sound := in.Str("sound")

	name, uid, hasGUI := consoleSession(ctx, c)
	if !hasGUI {
		who := name
		if who == "" {
			who = "未知用户"
		}
		return &tool.Result{OK: true, Msg: "当前没有图形登录会话，通知不会显示", Data: map[string]any{
			"displayed": false, "console_user": who,
			"reason": "控制台登录用户不是图形会话（/dev/console 归属 " + who + "）",
		}}, nil
	}

	c.Logf("发送通知给 %s", name)
	bin, pre := sessionWrap(ctx, "osascript", uid)
	// 实测复核：让脚本把收到的 argv 写回文件，证明中文往返没变乱码（坑 T2）。
	probe := filepath.Join(c.TempDir, "macsaber-notify-argv.txt")
	wrapped := `on run argv
	set theTitle to item 1 of argv
	set theBody to item 2 of argv
	set theSub to item 3 of argv
	set theSound to item 4 of argv
	try
		set fh to open for access POSIX file "` + probe + `" with write permission
		set eof fh to 0
		write (theTitle & "\n" & theBody & "\n" & theSub) to fh as «class utf8»
		close access fh
	end try
	if theSub is "" and theSound is "" then
		display notification theBody with title theTitle
	else if theSub is "" then
		display notification theBody with title theTitle sound name theSound
	else if theSound is "" then
		display notification theBody with title theTitle subtitle theSub
	else
		display notification theBody with title theTitle subtitle theSub sound name theSound
	end if
	return theTitle & "\n" & theBody & "\n" & theSub
end run`
	args := append(append([]string{}, pre...), "-e", wrapped, title, body, sub, sound)
	res := c.Exec.Run(ctx, 20*time.Second, bin, args...)
	if res.TimedOut {
		return nil, fmt.Errorf("osascript 超过 20 秒未返回，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("通知发送失败（退出码 %d）：%s", res.ExitCode, redactHome(c, failureReason(res)))
	}

	// 复核 argv 往返：读回脚本写下的内容，不对就如实报"中文可能变乱码"。
	roundTrip := ""
	argvOK := true
	if b, err := os.ReadFile(probe); err == nil {
		roundTrip = string(b)
		argvOK = strings.Contains(roundTrip, body) && strings.Contains(roundTrip, title)
		_ = os.Remove(probe)
	}
	data := map[string]any{
		"displayed": true, "console_user": name,
		"title": title, "message": body, "subtitle": sub, "sound": sound,
		"argv_round_trip": roundTrip, "argv_round_trip_ok": argvOK,
	}
	if !argvOK {
		data["warning"] = "脚本收到的 argv 与提交内容不一致，中文可能显示为乱码"
		return &tool.Result{OK: true, Msg: "通知已发送，但中文 argv 往返核对不一致", Data: data}, nil
	}
	return &tool.Result{OK: true, Msg: "通知已发送给 " + name, Data: data}, nil
}

// ============================================================================
//  auto.shortcuts_list —— 列出快捷指令（只读）
// ============================================================================

type autoShortcutsList struct{}

func init() { Add(autoShortcutsList{}) }

func (autoShortcutsList) Meta() tool.Meta {
	_, ok := execx.LookPath("shortcuts")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/shortcuts"
	}
	return tool.Meta{
		ID: "auto.shortcuts_list", Name: "快捷指令列表", Category: "auto", Icon: "command",
		Summary:   "列出当前用户的快捷指令；空列表不算失败。",
		Async:     true,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "filter", Label: "名称过滤", Type: tool.TypeText,
				Help: "可选，按子串过滤（区分大小写）。"},
		},
	}
}

func (autoShortcutsList) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	filter := in.Str("filter")
	bin, pre := sessionWrap(ctx, "shortcuts", consoleUID(ctx, c))
	res := c.Exec.Run(ctx, 30*time.Second, bin, append(append([]string{}, pre...), "list")...)
	if res.TimedOut {
		return nil, fmt.Errorf("shortcuts list 超过 30 秒未返回，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("shortcuts list 失败（退出码 %d）：%s", res.ExitCode, redactHome(c, failureReason(res)))
	}
	all := []string{}
	for _, ln := range strings.Split(res.Stdout, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		all = append(all, ln)
	}
	shown := all
	if filter != "" {
		shown = []string{}
		for _, s := range all {
			if strings.Contains(s, filter) {
				shown = append(shown, s)
			}
		}
	}
	data := map[string]any{"shortcuts": shown, "total": len(all), "returned": len(shown), "filter": filter}
	msg := fmt.Sprintf("共 %d 条快捷指令", len(all))
	if len(all) == 0 {
		// 空列表是正常结果，不是工具失败（坑 T3）。
		data["note"] = "当前用户没有快捷指令，空列表是正常结果"
		msg = "当前用户没有快捷指令（空列表不算失败）"
	} else if filter != "" {
		msg = fmt.Sprintf("匹配 %d / 共 %d 条", len(shown), len(all))
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// consoleUID 取图形会话 UID；没有会话时返回 -1（调用方在 root 下就不用 asuser）。
func consoleUID(ctx context.Context, c *tool.Ctx) int {
	_, uid, ok := consoleSession(ctx, c)
	if !ok {
		return -1
	}
	return uid
}

// ============================================================================
//  auto.shortcut_run —— 运行快捷指令（危险：必须手输快捷指令名）
// ============================================================================

type autoShortcutRun struct{}

func init() { Add(autoShortcutRun{}) }

// DangerFloor 是**固定**文案（后端先强校验它）；真正的"手输快捷指令名"
// 由 ConfirmOK 复核（DangerFloor 拿不到参数值，不能放名字）。
func (autoShortcutRun) DangerFloor() string { return ConfirmText }

func (autoShortcutRun) Meta() tool.Meta {
	_, ok := execx.LookPath("shortcuts")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/shortcuts"
	}
	return tool.Meta{
		ID: "auto.shortcut_run", Name: "运行快捷指令", Category: "auto", Icon: "command",
		Summary: "运行一条快捷指令；必须手输指令名确认。",
		Async:   true, Danger: true,
		DangerFloor: ConfirmText,
		Available:   ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "name", Label: "快捷指令名", Type: tool.TypeText, Required: true,
				Placeholder: "与 shortcuts list 里的名字完全一致",
				Help:        "名字必须已存在，不存在会被拒绝。"},
			{Name: "confirm_name", Label: "手输指令名确认", Type: tool.TypeText, Required: true,
				Placeholder: "再手输一遍上面的指令名",
				Help:        "必须与指令名一字不差，防止点错。"},
			{Name: "input", Label: "传入文本", Type: tool.TypeTextarea,
				Help: "可选，作为快捷指令输入；留空不传。"},
		},
	}
}

// ConfirmOK 复核：固定文案（后端已强校验）+ 手输的指令名必须与要跑的名字一致。
// DangerFloor 放不了参数值，所以"手输名字"用独立参数 confirm_name 承载。
func (autoShortcutRun) ConfirmOK(in tool.Input) error {
	if in.Str(tool.ConfirmField) != ConfirmText {
		return fmt.Errorf("需要确认「%s」", ConfirmText)
	}
	name := strings.TrimSpace(in.Str("name"))
	if name == "" {
		return fmt.Errorf("需要先填快捷指令名")
	}
	if strings.TrimSpace(in.Str("confirm_name")) != name {
		return fmt.Errorf("需要手输快捷指令名「%s」确认", name)
	}
	return nil
}

func (autoShortcutRun) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	name := strings.TrimSpace(in.Str("name"))
	// 工具自身再判一次：不轻信后端或前端传来的确认（危险工具契约）。
	if err := (autoShortcutRun{}).ConfirmOK(in); err != nil {
		return nil, err
	}
	bin, pre := sessionWrap(ctx, "shortcuts", consoleUID(ctx, c))
	// 名字不在 shortcuts list 里直接拒绝（不猜、不试）。
	lst := c.Exec.Run(ctx, 30*time.Second, bin, append(append([]string{}, pre...), "list")...)
	if lst.TimedOut {
		return nil, fmt.Errorf("shortcuts list 超时，无法核对名字")
	}
	if lst.ExitCode != 0 {
		return nil, fmt.Errorf("shortcuts list 失败（退出码 %d），无法核对名字：%s",
			lst.ExitCode, redactHome(c, firstLine(lst.Output())))
	}
	known := strings.Split(strings.TrimSpace(lst.Stdout), "\n")
	found := false
	for _, k := range known {
		if strings.TrimSpace(k) == name {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("快捷指令「%s」不在 shortcuts list 里，已拒绝", name)
	}

	input := in.Str("input")
	if input != "" {
		// 文本走 stdin，绝不拼进命令。
		c.Logf("运行快捷指令：%s（带输入）", name)
		runArgs := append(append([]string{}, pre...), "run", name)
		res := runWithInput(ctx, 10*time.Minute, input, bin, runArgs...)
		if res.TimedOut {
			return nil, fmt.Errorf("快捷指令超过 10 分钟未完成，已终止")
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("快捷指令失败（退出码 %d）：%s", res.ExitCode, redactHome(c, firstLine(res.Output())))
		}
		return &tool.Result{OK: true, Msg: "已运行快捷指令：" + name, Data: map[string]any{
			"name": name, "input_sent": true, "output": strings.TrimSpace(res.Stdout),
		}}, nil
	}

	c.Logf("运行快捷指令：%s", name)
	res := c.Exec.Run(ctx, 10*time.Minute, bin, append(append([]string{}, pre...), "run", name)...)
	if res.TimedOut {
		return nil, fmt.Errorf("快捷指令超过 10 分钟未完成，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("快捷指令失败（退出码 %d）：%s", res.ExitCode, redactHome(c, failureReason(res)))
	}
	return &tool.Result{OK: true, Msg: "已运行快捷指令：" + name, Data: map[string]any{
		"name": name, "input_sent": false, "output": strings.TrimSpace(res.Stdout),
	}}, nil
}

// ============================================================================
//  auto.open —— 用默认程序打开路径或 URL（危险：会启动外部程序）
// ============================================================================

type autoOpen struct{}

func init() { Add(autoOpen{}) }

func (autoOpen) DangerFloor() string { return ConfirmText }

func (autoOpen) ConfirmOK(in tool.Input) error {
	if in.Str(tool.ConfirmField) != ConfirmText {
		return fmt.Errorf("需要确认「%s」", ConfirmText)
	}
	return nil
}

func (autoOpen) Meta() tool.Meta {
	_, ok := execx.LookPath("open")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/open"
	}
	return tool.Meta{
		ID: "auto.open", Name: "打开文件或网址", Category: "auto", Icon: "external-link",
		Summary: "用默认程序打开文件/目录，或打开 http(s) 网址。",
		Async:   true, Danger: true,
		DangerFloor: ConfirmText,
		Available:   ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "kind", Label: "打开类型", Type: tool.TypeSelect, Required: true,
				Default: "url", Options: []tool.Option{
					{Value: "url", Label: "网址"}, {Value: "file", Label: "文件/目录"},
				}, Help: "网址只允许 http/https；文件走路径闸门。"},
			{Name: "url", Label: "网址", Type: tool.TypeText,
				Placeholder: "https://example.com", Help: "只允许 http/https，其它 scheme 一律拒绝。"},
			{Name: "path", Label: "文件或目录", Type: tool.TypePath,
				Help: "只能读允许的读根内的文件或目录。"},
		},
	}
}

func (autoOpen) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	if err := (autoOpen{}).ConfirmOK(in); err != nil {
		return nil, err
	}
	kind := in.Str("kind")
	target, err := (autoOpen{}).validateTarget(in.Path("path"), kind, in.Str("url"))
	if err != nil {
		return nil, err
	}

	c.Logf("打开：%s", redactHome(c, target))
	// -- 终止选项解析，避免目标被当成 open 的开关。
	res := c.Exec.Run(ctx, 30*time.Second, "open", "--", target)
	if res.TimedOut {
		return nil, fmt.Errorf("open 超过 30 秒未返回，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("open 失败（退出码 %d）：%s", res.ExitCode, redactHome(c, failureReason(res)))
	}
	return &tool.Result{OK: true, Msg: "已交给系统打开：" + redactHome(c, target), Data: map[string]any{
		"kind": kind, "target": redactHome(c, target),
		"note": "由系统默认程序处理，本工具无法确认前台是否真的显示",
	}}, nil
}

// validateTarget 是 auto.open 的判定核心：URL 只放行 http/https，路径只认闸门解析结果。
func (autoOpen) validateTarget(resolvedPath, kind, raw string) (string, error) {
	switch kind {
	case "url":
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return "", fmt.Errorf("请填写网址")
		}
		if err := checkURLScheme(raw); err != nil {
			return "", err
		}
		if strings.HasPrefix(raw, "-") {
			return "", fmt.Errorf("网址不能以 - 开头（会被当成 open 的参数）")
		}
		return raw, nil
	case "file":
		if resolvedPath == "" {
			return "", fmt.Errorf("请填写文件或目录路径")
		}
		if strings.HasPrefix(resolvedPath, "-") {
			return "", fmt.Errorf("路径不能以 - 开头（会被当成 open 的参数）")
		}
		return resolvedPath, nil
	default:
		return "", fmt.Errorf("打开类型非法：%s", kind)
	}
}

// checkURLScheme 是 URL scheme 白名单：http/https 以外一律拒绝并给原因（坑 T4）。
func checkURLScheme(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("网址无法解析：%s", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	}
	return fmt.Errorf("不支持的 URL scheme %q：只允许 http/https", u.Scheme)
}

// ============================================================================
//  auto.clipboard_read / auto.clipboard_write —— 剪贴板读写
// ============================================================================

type autoClipboardRead struct{}

func init() { Add(autoClipboardRead{}) }

func (autoClipboardRead) Meta() tool.Meta {
	_, ok := execx.LookPath("pbpaste")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/pbpaste"
	}
	return tool.Meta{
		ID: "auto.clipboard_read", Name: "读剪贴板", Category: "auto", Icon: "clipboard",
		Summary:   "读当前登录用户的剪贴板文本，可能含敏感内容。",
		Async:     true,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "max_bytes", Label: "最多返回字节", Type: tool.TypeNumber, Default: 65536,
				Min: Num(256), Max: Num(1048576), Help: "超长内容截断，避免界面卡住。"},
		},
	}
}

func (autoClipboardRead) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	limit := in.Int("max_bytes", 65536)
	if limit <= 0 {
		limit = 65536
	}
	bin, pre := sessionWrap(ctx, "pbpaste", consoleUID(ctx, c))
	res := c.Exec.Run(ctx, 20*time.Second, bin, pre...)
	if res.TimedOut {
		return nil, fmt.Errorf("pbpaste 超过 20 秒未返回，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("pbpaste 失败（退出码 %d）：%s", res.ExitCode, redactHome(c, failureReason(res)))
	}
	text := res.Stdout
	truncated := false
	if len(text) > limit {
		text = text[:limit]
		truncated = true
	}
	data := map[string]any{
		"text": text, "bytes": len(res.Stdout), "truncated": truncated,
		"note": "读的是当前登录用户的剪贴板，可能含敏感内容",
	}
	msg := fmt.Sprintf("已读取剪贴板 %d 字节", len(res.Stdout))
	if truncated {
		msg = fmt.Sprintf("剪贴板 %d 字节，已截断到 %d", len(res.Stdout), limit)
	}
	if strings.TrimSpace(text) == "" {
		msg = "剪贴板为空或没有文本内容"
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

type autoClipboardWrite struct{}

func init() { Add(autoClipboardWrite{}) }

func (autoClipboardWrite) Meta() tool.Meta {
	_, ok := execx.LookPath("pbcopy")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/pbcopy"
	}
	return tool.Meta{
		ID: "auto.clipboard_write", Name: "写剪贴板", Category: "auto", Icon: "clipboard",
		Summary:   "把文本写进当前登录用户的剪贴板，会覆盖原内容。",
		Async:     true,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "text", Label: "文本", Type: tool.TypeTextarea, Required: true,
				Secret: true, Placeholder: "要写进剪贴板的内容",
				Help: "内容会覆盖剪贴板，且不写进审计日志。"},
		},
	}
}

func (autoClipboardWrite) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	text := in.Str("text")
	if text == "" {
		return nil, fmt.Errorf("文本为空，请填写要写入的内容")
	}
	bin, pre := sessionWrap(ctx, "pbcopy", consoleUID(ctx, c))
	// 内容只走 stdin，绝不拼进命令（坑 B1）。
	res := runWithInput(ctx, 20*time.Second, text, bin, pre...)
	if res.TimedOut {
		return nil, fmt.Errorf("pbcopy 超过 20 秒未返回，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("pbcopy 失败（退出码 %d）：%s", res.ExitCode, redactHome(c, firstLine(res.Output())))
	}
	return &tool.Result{OK: true, Msg: fmt.Sprintf("已写入剪贴板 %d 字节", len(text)), Data: map[string]any{
		"bytes": len(text), "note": "内容未写进审计日志，也不会回显",
	}}, nil
}

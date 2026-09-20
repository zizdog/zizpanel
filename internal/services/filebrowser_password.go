package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  File Browser「重置口令」
//
//  用户 2026-09-20 要求："给 filebrowser 加重置口令功能。"
//
//  为什么必须停服务再改：它的库是 **BoltDB**，实例运行时 CLI 拿不到文件锁，
//  `users update` 会超时或静默失败（见 docs/坑清单.md 坑 221）。所以流程固定为
//  **停 → 改 → 起**，且无论改口令成败都必须把服务起回来。
//
//  安全约定：口令只放进 InstallResult.Credentials（前端只显示一次），
//  命令标签与命令输出都做脱敏，绝不进任务日志/审计。
// ============================================================================

// filebrowserMinPasswordLength 与库里的 minimumPasswordLength 一致（真机实测 12）。
const filebrowserMinPasswordLength = 12

// filebrowserLoginPaths 是登录探测的候选路径，按实测优先级排列。
// 真机实测（v2.63.23，2026-09-20）：登录端点是 **/api/login**，
// /api/v1/login 返回 404。两个都试是为了上游改路径时不至于永远误报失败。
var filebrowserLoginPaths = []string{"/api/login", "/api/v1/login"}

// filebrowserPlistPath 返回 plist 的真实路径（单测注入临时文件，见 Manager 字段）。
func (m *Manager) filebrowserPlistPath(d AppDescriptor) string {
	if m.filebrowserPlistOverride != "" {
		return m.filebrowserPlistOverride
	}
	return d.Service.PlistPath
}

// randomFilebrowserPassword 生成强随机口令（crypto/rand 的 26 位 base32，约 130 bit）。
// 字符集没有引号/空格，命令行与日志都不会遇到转义问题。
func randomFilebrowserPassword() (string, error) {
	return rand.Text(), nil
}

// filebrowserRedact 把文本里的口令替换成 ****。
// 为什么会出现在文本里：命令标签是 cmd.Args 拼的（含 `--password <值>`），
// 而日志会被长期保存/转发 —— 口令绝不能进日志（坑 221）。
func filebrowserRedact(text, secret string) string {
	if secret == "" || text == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "****")
}

// filebrowserRun 执行 File Browser 流程里的一条外部命令。
//
// 命令标签从实际执行的 argv 派生（与 streamCmd 同一约定），但**先脱敏再输出**：
// 口令出现在标签或输出里时一律换成 ****。这个函数不走 streamCmd ——
// 它会把 cmd.Args 原样写进日志，那正是口令泄漏的路径。
func (m *Manager) filebrowserRun(ctx context.Context, timeout time.Duration,
	secret, name string, args ...string) (string, error) {

	emit(ctx, tasks.LevelCmd, "$ "+filebrowserRedact(strings.Join(append([]string{name}, args...), " "), secret))

	var out string
	var err error
	if m.filebrowserExecOverride != nil {
		out, err = m.filebrowserExecOverride(ctx, timeout, name, args...)
	} else {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, name, args...)
		raw, e := cmd.CombinedOutput()
		out, err = string(raw), e
		if cctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("超过 %s 没有返回（超时）", timeout)
		}
		for _, ln := range strings.Split(strings.TrimRight(filebrowserRedact(out, secret), "\n"), "\n") {
			if strings.TrimSpace(ln) != "" {
				emit(ctx, tasks.LevelOut, ln)
			}
		}
	}
	out = filebrowserRedact(out, secret)
	if err == nil {
		return out, nil
	}
	msg := filebrowserRedact(err.Error(), secret)
	if strings.TrimSpace(out) != "" {
		msg += "；输出：" + tailText(strings.TrimSpace(stripANSI(out)), 300)
	}
	return out, errors.New(msg)
}

// filebrowserPlistArgs 从 plist 文本里取（可执行文件 / 数据库 / 根目录）三个值。
// 一律以 plist 为准：那才是服务真正跑起来用的参数（面板记录与实际漂移时以现实为准）。
func filebrowserPlistArgs(plist string) (bin, db, root string) {
	bin = plistProgram(plist)
	db, _ = plistArgValue(plist, "-d")
	root, _ = plistArgValue(plist, "-r")
	return bin, db, root
}

// plistProgram 取 ProgramArguments 数组里的第一个 <string>（要执行的可执行文件）。
func plistProgram(plist string) string {
	seen := false
	for _, ln := range strings.Split(plist, "\n") {
		t := strings.TrimSpace(ln)
		if strings.Contains(t, "<key>ProgramArguments</key>") {
			seen = true
			continue
		}
		if !seen {
			continue
		}
		if strings.Contains(t, "</array>") {
			return ""
		}
		if v, ok := plistStringValue(t); ok {
			return v
		}
	}
	return ""
}

// filebrowserPathsFromSpec 在 plist 读不到时按描述符的启动参数兜底。
func (m *Manager) filebrowserPathsFromSpec() (bin, db, root string) {
	d, ok := FilebrowserDescriptor()
	if !ok {
		return "", "", ""
	}
	p := m.binaryReleasePathsFor(d)
	spec := releaseBinaryApps[d.ID]
	vars := map[string]string{
		"{root}": p.Root, "{home}": m.opt.UserHome,
		"{user}": m.opt.UserName, "{vardir}": m.opt.WorkDir,
	}
	args := make([]string, 0, len(spec.Args))
	for _, a := range spec.Args {
		args = append(args, expandVars(a, vars))
	}
	return p.Binary, argFlagValue(args, "-d"), argFlagValue(args, "-r")
}

// argFlagValue 取 `-flag value` 形式的参数值（取第一个命中）。
func argFlagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// parseFilebrowserUsername 从 `filebrowser users ls` 的表格里解析用户名（不写死 zizdog）。
//
// 实测输出（v2.63.23，2026-09-20）：两行 stderr 日志 + 表头 + 数据行；
// 表头里 "V. Mode" / "Red. After C/M" 带空格，按空白切出来的下标与数据行**不对齐**，
// 所以 Username 只能取表头中它自己的下标（第 1 列，安全），Admin 列按数据行
// 末尾固定偏移（末 9 列是 Admin，实测 16 列）判定。
func parseFilebrowserUsername(out string) (string, error) {
	userIdx, headerSeen, hasAdmin := -1, false, false
	var rows [][]string
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) == 0 {
			continue
		}
		if !headerSeen {
			if f[0] != "ID" {
				continue
			}
			for i, name := range f {
				switch name {
				case "Username":
					userIdx = i
				case "Admin":
					hasAdmin = true
				}
			}
			headerSeen = userIdx >= 0
			continue
		}
		rows = append(rows, f)
	}
	if !headerSeen {
		return "", fmt.Errorf("没能解析 `filebrowser users ls` 的表头")
	}
	var users []string
	for _, f := range rows {
		if len(f) <= userIdx {
			continue
		}
		users = append(users, f[userIdx])
		// Admin 列在数据行里的固定位置：末 9 列（实测 16 列 → 下标 7）。
		if hasAdmin && len(f) >= 10 && f[len(f)-9] == "true" {
			return f[userIdx], nil
		}
	}
	switch len(users) {
	case 0:
		return "", fmt.Errorf("库里没有任何用户")
	case 1:
		return users[0], nil
	}
	return "", fmt.Errorf("库里有 %d 个用户（%s）且都没有管理员标记，面板无法确定改哪一个",
		len(users), strings.Join(users, "、"))
}

// parseFilebrowserConfigRoot 从 `config cat` 的文本里取 Server.Root。
func parseFilebrowserConfigRoot(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "Root:") {
			return strings.TrimSpace(strings.TrimPrefix(t, "Root:"))
		}
	}
	return ""
}

// filebrowserStop 停服务并**等它真的停下来**（端口不再监听）。
//
// launchd 的 bootout 是异步的：一返回不等于已卸载完（bootstrapService 的注释踩过）。
// 服务本来没加载时 bootout 会报错，但那时端口已经不在了 —— 这不算失败。
func (m *Manager) filebrowserStop(ctx context.Context, label string, port int) error {
	out, err := m.filebrowserRun(ctx, 20*time.Second, "", "/bin/launchctl", "bootout", "system/"+label)
	bootoutFailed := err != nil
	if port <= 0 {
		return nil
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !m.portHasListener(port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待服务停止时被中断：%v", ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
	if bootoutFailed {
		return fmt.Errorf("停止服务失败（launchctl bootout）：%s；且端口 %d 仍在监听",
			tailText(strings.TrimSpace(out), 200), port)
	}
	return fmt.Errorf("已执行 launchctl bootout，但端口 %d 在 15 秒内仍在监听（可能不是 launchd 拉起的进程）", port)
}

// filebrowserStart 把服务装回去（bootout 之后 bootstrap；已加载时 kickstart 重启）。
func (m *Manager) filebrowserStart(ctx context.Context, label, plist string) error {
	var last string
	for attempt := 1; attempt <= 5; attempt++ {
		out, err := m.filebrowserRun(ctx, 30*time.Second, "", "/bin/launchctl", "bootstrap", "system", plist)
		if err == nil {
			return nil
		}
		last = strings.TrimSpace(out)
		// 已经加载时 bootstrap 会失败：改用 kickstart 重启它。
		if _, perr := m.filebrowserRun(ctx, 10*time.Second, "", "/bin/launchctl", "print", "system/"+label); perr == nil {
			if _, kerr := m.filebrowserRun(ctx, 20*time.Second, "", "/bin/launchctl", "kickstart", "-k", "system/"+label); kerr == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("重新装载服务被中断：%v", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("重新装载服务失败（已重试 5 次）：%s", tailText(last, 200))
}

// filebrowserWaitPort 等端口开始监听（"服务真的活了"的证据）。
func (m *Manager) filebrowserWaitPort(ctx context.Context, port int, timeout time.Duration) bool {
	if port <= 0 {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.portHasListener(port) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
	return false
}

// filebrowserHTTP 打一个回环 HTTP 请求（单测可替换，见 filebrowserProbeOverride）。
func (m *Manager) filebrowserHTTP(ctx context.Context, method, rawURL string, body []byte) (int, error) {
	if m.filebrowserProbeOverride != nil {
		return m.filebrowserProbeOverride(ctx, method, rawURL, body)
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// filebrowserVerify 回读验证：/health 必须 200，且用新口令登录必须 200。
func (m *Manager) filebrowserVerify(ctx context.Context, port int,
	username, password string) (healthCode, loginCode int, detail string, err error) {

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	healthCode, err = m.filebrowserHTTP(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return 0, 0, "", fmt.Errorf("健康检查请求失败：%w", err)
	}
	if healthCode != http.StatusOK {
		return healthCode, 0, "", nil
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	var tried []string
	for _, p := range filebrowserLoginPaths {
		code, cerr := m.filebrowserHTTP(ctx, http.MethodPost, base+p, body)
		if cerr != nil {
			return healthCode, 0, strings.Join(tried, "、"), fmt.Errorf("登录验证请求失败：%w", cerr)
		}
		if code == http.StatusOK {
			return healthCode, code, p + " 返回 200", nil
		}
		tried = append(tried, fmt.Sprintf("%s 返回 %d", p, code))
		loginCode = code
		// 只有 404 才说明这条路径不存在、值得试下一个；403/401 说明路径在、口令不对，
		// 再打一次别的路径只会多一次失败登录（可能触发限流）。
		if code != http.StatusNotFound {
			break
		}
	}
	return healthCode, loginCode, strings.Join(tried, "、"), nil
}

// filebrowserAlignDBRoot 把库里的 config.root 对齐成 plist 的 -r（幂等）。
//
// 依据是**面板记录的这条服务的实际根**（plist 的 -r，从真实安装配置读回），不是猜的；
// 这样"库里"与"运行参数"两者一致。库里的旧值只在**不带 -r 启动**时才会露出来。
// 这是顺手做的一致性动作：失败只写警告，不否定已经成功的口令重置。
func (m *Manager) filebrowserAlignDBRoot(ctx context.Context, bin, db, root string) string {
	if root == "" {
		return "⚠️ 读不到 plist 的 -r，跳过数据库 config.root 对齐"
	}
	if _, err := m.filebrowserRun(ctx, 20*time.Second, "", bin, "-d", db, "config", "set", "--root", root); err != nil {
		return fmt.Sprintf("⚠️ 对齐数据库 config.root 失败（不影响口令）：%s", tailText(err.Error(), 200))
	}
	out, err := m.filebrowserRun(ctx, 20*time.Second, "", bin, "-d", db, "config", "cat")
	if err != nil {
		return fmt.Sprintf("⚠️ 已请求把数据库 config.root 改成 %s，但回读失败（未复核）", root)
	}
	if got := parseFilebrowserConfigRoot(out); got != root {
		return fmt.Sprintf("⚠️ 数据库 config.root 回读到 %q，与 plist 的 -r %q 不一致（未复核）", got, root)
	}
	if m.opt.UserHome != "" && root == m.opt.UserHome {
		// 如实提醒：这时候"对齐"并不能消除"不带 -r 就露出整个家目录"的风险 ——
		// 根因在 -r 本身就是家目录，收窄它要走「📁 主目录」。
		return fmt.Sprintf("⚠️ 已对齐成 %s，但它就是整个家目录（收窄请用「📁 主目录」改 -r）", root)
	}
	return fmt.Sprintf("已把数据库 config.root 对齐成 %s（与启动参数 -r 一致，回读通过）", root)
}

// FilebrowserResetPassword 重置 File Browser 的口令。
//
// 固定顺序：**停服务 → 改口令（顺带对齐库里的 root）→ 把服务起回来 → 回读验证**。
// 无论中途哪一步失败，只要服务被停过就必须尝试起回来；只有 /health 200 **且**
// 新口令登录 200 才回报成功。新口令只放在返回值的 Credentials 里（前端只显示一次）。
func (m *Manager) FilebrowserResetPassword(ctx context.Context, newPassword string) (*InstallResult, error) {
	d, ok := FilebrowserDescriptor()
	if !ok {
		return nil, fmt.Errorf("面板内部错误：找不到 filebrowser 的描述符")
	}
	plistPath := m.filebrowserPlistPath(d)
	res := &InstallResult{App: d.ID, Name: d.Name}

	bin, db, root := "", "", ""
	if data, err := os.ReadFile(plistPath); err == nil {
		bin, db, root = filebrowserPlistArgs(string(data))
	}
	if bin == "" || db == "" || root == "" {
		fbin, fdb, froot := m.filebrowserPathsFromSpec()
		if bin == "" {
			bin = fbin
		}
		if db == "" {
			db = fdb
		}
		if root == "" {
			root = froot
		}
	}
	if bin == "" || db == "" {
		return res, fmt.Errorf("读不到 File Browser 的启动参数（plist：%s）—— 请重新安装后再试", plistPath)
	}
	// 单测注入假 CLI 时跳过真实产物检查（临时 plist 指向的是假脚本）。
	if m.filebrowserExecOverride == nil {
		if _, err := os.Stat(bin); err != nil {
			return res, fmt.Errorf("找不到 File Browser 可执行文件 %s（可能没装好）：%w", bin, err)
		}
	}

	pw := strings.TrimSpace(newPassword)
	generated := false
	if pw == "" {
		var err error
		if pw, err = randomFilebrowserPassword(); err != nil {
			return res, fmt.Errorf("生成随机口令失败：%w", err)
		}
		generated = true
	} else if n := utf8.RuneCountInString(pw); n < filebrowserMinPasswordLength {
		return res, fmt.Errorf("口令太短：File Browser 的策略要求至少 %d 位，实际 %d 位",
			filebrowserMinPasswordLength, n)
	}

	// ① 停止服务（运行中它的 BoltDB 被锁住，CLI 改不动库）
	res.step(ctx, "① 停止 File Browser（运行中它的库被 BoltDB 锁住，CLI 改不动）")
	if err := m.filebrowserStop(ctx, d.Service.Label, d.Port); err != nil {
		startErr := m.filebrowserStart(ctx, d.Service.Label, plistPath)
		msg := "停止服务失败：" + err.Error()
		if startErr != nil {
			msg += "；随后尝试把它启动回来也失败：" + startErr.Error() + "（请到「服务管理」手动启动）"
		} else {
			msg += "（已尝试把它启动回来）"
		}
		res.step(ctx, "✗ "+msg)
		return res, errors.New(msg)
	}

	// ② 读用户名并改口令
	res.step(ctx, "② 从库里读出用户名（不写死）并改口令")
	username := ""
	var cliErr error
	lsOut, lsErr := m.filebrowserRun(ctx, 20*time.Second, "", bin, "-d", db, "users", "ls")
	if lsErr != nil {
		cliErr = fmt.Errorf("读取用户列表失败：%s", tailText(stripANSI(lsOut), 300))
	} else if username, cliErr = parseFilebrowserUsername(lsOut); cliErr != nil {
		cliErr = fmt.Errorf("没能确定要改哪个用户：%w", cliErr)
	}
	if cliErr == nil {
		// --perm.execute=false 是加固：库里的 execute 权限保持关闭（全局 enableExec 也是 false）。
		if _, err := m.filebrowserRun(ctx, 30*time.Second, pw, bin, "-d", db,
			"users", "update", username, "--password", pw, "--perm.execute=false"); err != nil {
			cliErr = fmt.Errorf("改口令失败：%w", err)
		} else if generated {
			res.step(ctx, fmt.Sprintf("已为用户 %s 设置新口令（自动生成 %d 位，值只在结果里显示）",
				username, utf8.RuneCountInString(pw)))
		} else {
			res.step(ctx, fmt.Sprintf("已为用户 %s 设置新口令（值只在结果里显示，不写日志）", username))
		}
	}

	// ③ 顺手把库里的 config.root 对齐成 plist 的 -r（幂等）
	if note := m.filebrowserAlignDBRoot(ctx, bin, db, root); note != "" {
		res.step(ctx, note)
		if strings.HasPrefix(note, "⚠️") {
			res.Warning = note
		}
	}

	// ④ 无论上面成败，都要把服务起回来
	res.step(ctx, "④ 重新启动 File Browser（无论成败，服务都必须起回来）")
	startErr := m.filebrowserStart(ctx, d.Service.Label, plistPath)

	if cliErr != nil {
		msg := cliErr.Error()
		if startErr != nil {
			msg += "；把它重新启动也失败：" + startErr.Error() + "（请到「服务管理」手动启动）"
		} else {
			msg += "（服务已重新启动，原口令仍然有效）"
		}
		res.step(ctx, "✗ "+msg)
		return res, errors.New(msg)
	}
	if startErr != nil {
		msg := "口令已改成功，但重新启动服务失败：" + startErr.Error() +
			"（请到「服务管理」手动启动；新口令见本次任务结果）"
		res.step(ctx, "✗ "+msg)
		return res, errors.New(msg)
	}
	if !m.filebrowserWaitPort(ctx, d.Port, 15*time.Second) {
		msg := fmt.Sprintf("口令已改、服务已请求启动，但端口 %d 在 15 秒内没有监听 —— 未验证，请查看服务日志", d.Port)
		res.step(ctx, "✗ "+msg)
		return res, errors.New(msg)
	}

	// ⑤ 回读验证：只有两项都过才算成功
	res.step(ctx, "⑤ 回读验证：/health 返回 200，且用新口令登录成功")
	hc, lc, detail, verr := m.filebrowserVerify(ctx, d.Port, username, pw)
	switch {
	case verr != nil:
		msg := "口令已改，但验证请求失败：" + verr.Error() + "（未验证）"
		res.step(ctx, "✗ "+msg)
		return res, errors.New(msg)
	case hc != http.StatusOK:
		msg := fmt.Sprintf("口令已改，但健康检查 /health 返回 %d（期望 200）—— 未通过验证", hc)
		res.step(ctx, "✗ "+msg)
		return res, errors.New(msg)
	case lc != http.StatusOK:
		msg := fmt.Sprintf("口令已改，但用新口令登录没有通过（%s）—— 未通过验证", detail)
		res.step(ctx, "✗ "+msg)
		return res, errors.New(msg)
	}

	// 成功：口令只放进 Credentials（唯一允许出现口令的地方，前端只显示一次）
	res.Message = "File Browser 口令已重置并通过回读验证。"
	res.Credentials = []Credential{
		{Key: "filebrowser_username", Value: username, Label: "登录用户名"},
		{Key: "filebrowser_password", Value: pw, Label: "新口令（只显示这一次，请立刻保存或改掉）"},
	}
	res.step(ctx, "✓ 验证通过：/health 200，新口令登录 200。")
	return res, nil
}

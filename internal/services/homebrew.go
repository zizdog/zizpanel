package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
//  面板内安装 Homebrew（含命令行开发者工具）
//
//  为什么必须有：全新 macOS 上什么都没有 —— 没有 Homebrew，而 Homebrew 自己
//  又需要「命令行开发者工具（CLT）」。以前面板遇到"没 brew"只会拒绝：
//
//     未安装 Homebrew，无法自动安装 LNMP。请先安装 Homebrew
//
//  这就把用户赶回命令行，与"装完面板后所有操作都在面板里完成"矛盾。
//  所以这里把这两步都做成面板里的任务（有进度、可重试），并且**优先走无需点击的路径**。
//
//  国内网络注意：Homebrew 的安装脚本在 raw.githubusercontent.com 上，大陆直连不通，
//  所以要按顺序试镜像（与市场下载同一套思路）。
// ============================================================================

// Homebrew 官方安装脚本 + 国内可达的加速前缀。
const brewInstallScriptURL = "https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh"

// brewInstallScriptCandidates 返回安装脚本的候选地址（官方优先，镜像兜底）。
func brewInstallScriptCandidates() []string {
	out := []string{brewInstallScriptURL}
	for _, m := range gitHubReleaseMirrors {
		out = append(out, strings.TrimSuffix(m, "/")+"/"+brewInstallScriptURL)
	}
	return out
}

// cltInstalled 判断命令行开发者工具是否已装。
//
// 只看 /usr/bin/xcode-select -p 是不够的：它可能返回一个**并不存在**的路径
// （苹果的已知行为：装过又删掉 CLT 之后仍然打印路径）。所以再 stat 一下。
func (m *Manager) cltInstalled(ctx context.Context) bool {
	out, err := m.runRoot(ctx, 20*time.Second, "/usr/bin/xcode-select", "-p")
	if err != nil {
		return false
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		return false
	}
	if _, serr := os.Stat(dir); serr != nil {
		return false
	}
	// 真正的 CLT 在 /Library/Developer/CommandLineTools；
	// 若指向 Xcode.app，也说明工具可用（有完整 Xcode 的用户不该再被要求装 CLT）
	return true
}

// parseCLTLabel 从 `softwareupdate -l` 的输出里找出命令行工具的条目名。
//
// 抽成纯函数是因为这段解析最容易写错又最难复现：输出是英文/本地化混排、
// 条目名形如 "Command Line Tools for Xcode-16.2"。返回空串表示没找到。
func parseCLTLabel(out string) string {
	// 先按行找含 "Command Line Tools" 的行，再取 * 后面的整个标题
	re := regexp.MustCompile(`(?m)^\s*\*\s*(Label:\s*)?(.+Command Line Tools.+)$`)
	for _, mt := range re.FindAllStringSubmatch(out, -1) {
		label := strings.TrimSpace(mt[2])
		label = strings.TrimSpace(strings.TrimPrefix(label, "Label:"))
		if label != "" {
			return label
		}
	}
	// 兜底：有些版本会把标题写在 "Title:" 后面
	re2 := regexp.MustCompile(`(?m)Title:\s*(.+Command Line Tools.+),\s*Version`)
	if mt := re2.FindStringSubmatch(out); len(mt) == 2 {
		return strings.TrimSpace(mt[1])
	}
	return ""
}

// installCLT 装命令行开发者工具，按可靠性从高到低试三条路，并且如实报告走了哪条。
//
//  1. **镜像整包安装**（`installCLTFromMirror`）—— 苹果原始 pkg，走项目自己的
//     国内镜像。真机实测苹果 CDN 在国内无代理时 15 分钟只下 1 MB，所以这条路优先。
//  2. `softwareupdate -i <label>` —— 苹果自己的静默路径，网络好的时候最快。
//  3. `xcode-select --install` —— 会弹**图形对话框**，远程/无人值守场景等于死等，
//     所以放最后，并且明确告诉用户要点什么。
//
// 第 2 条为什么需要那个 touch 文件：它能让 CLT 出现在 `softwareupdate -l` 里
// （苹果的 install-on-demand 技巧）。新版 macOS 上不一定还有效，所以三条路都留着。
func (m *Manager) installCLT(ctx context.Context, result *InstallResult) error {
	result.step(ctx, "缺少「命令行开发者工具」（Homebrew 与 Python 的前置依赖），开始安装")

	// ---- 路 1：镜像 ----
	if err := m.installCLTFromMirror(ctx, result); err == nil {
		result.step(ctx, "命令行开发者工具安装完成（镜像）")
		return nil
	} else {
		result.step(ctx, "镜像这条路没走通："+err.Error())
	}

	// ---- 路 2：softwareupdate（苹果官方，静默）----
	touch := "/tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress"
	if err := os.WriteFile(touch, []byte(""), 0o644); err == nil {
		defer func() { _ = os.Remove(touch) }()
	}

	if out, err := m.runRoot(ctx, 3*time.Minute, "/usr/sbin/softwareupdate", "-l"); err == nil {
		if label := parseCLTLabel(out); label != "" {
			result.step(ctx, "用 softwareupdate 静默安装："+label+"（可能要几分钟，请勿关闭）")
			// softwareupdate 一定要自己 fork 子进程，所以必须真 root：
			// runRoot 即直接执行；runAsUser 会插 `sudo -n -u <用户>`，
			// 那样 softwareupdate 会以"非 root 又没降权目标"的状态跑，必失败。
			iout, ierr := m.runRoot(ctx, 40*time.Minute, "/usr/sbin/softwareupdate", "-i", label)
			if ierr == nil && m.cltInstalled(ctx) && m.python3Works(ctx) {
				result.step(ctx, "命令行开发者工具安装完成（softwareupdate）")
				return nil
			}
			// 关键：把 apple 那边的真实输出带进任务步骤。以前只记"没成功"，
			// 真机上排查时完全看不到是 DNS、证书还是下载中断。
			result.step(ctx, "softwareupdate 失败："+lastLines(iout, 4))
		} else {
			result.step(ctx, "softwareupdate 列表里没有命令行工具条目")
		}
	} else {
		result.step(ctx, "softwareupdate -l 执行失败："+lastLines(out, 3))
	}

	// ---- 路 3：弹窗安装 ----
	result.step(ctx, "现在会弹出一个「安装命令行开发者工具」的对话框 —— "+
		"**请点「安装」并同意许可**，面板会在这里等（最多 30 分钟）")
	if _, err := m.runAsUser(ctx, time.Minute, "/usr/bin/xcode-select", "--install"); err != nil {
		// 已经装过时这条命令也会返回非 0，所以不当成致命错误，继续轮询
		result.step(ctx, "xcode-select --install 返回非 0（可能已经在装或已装好），继续等待")
	}
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		if m.cltInstalled(ctx) && m.python3Works(ctx) {
			result.step(ctx, "命令行开发者工具安装完成（弹窗）")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("三条路都没能装上命令行开发者工具（镜像 / softwareupdate / 弹窗）。" +
		"可以手动执行 `xcode-select --install`，或检查镜像 clt/index.json 是否可用")
}

// EnsureCLT 保证「命令行开发者工具」可用。
//
// 为什么单独暴露：**全新 macOS 上 /usr/bin/python3 只是个占位程序** ——
// 直接跑它会弹"安装开发者工具"的图形对话框，脚本里拿不到任何输出。
// 而 TTS 两件套（Qwen3 TTS 与音色接收端）都要真正的 Python 3
// （venv/pip/接收端脚本），所以它们安装前必须先过这一关。
// 真机实测（抹机后的 mini）：没装 CLT 时 python3 --version 只会打印
// "xcode-select: note: No developer tools were found, requesting install."，
// 接收端 plist 指向的 /usr/bin/python3 起来就会失败。
func (m *Manager) EnsureCLT(ctx context.Context, result *InstallResult) error {
	if result == nil {
		result = &InstallResult{Steps: []string{}}
	}
	if m.cltInstalled(ctx) {
		result.step(ctx, "命令行开发者工具已就绪")
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装命令行开发者工具需要以 root 运行")
	}
	return m.installCLT(ctx, result)
}

// EnsureHomebrew 保证 Homebrew 可用；缺什么装什么。已经是好的就直接返回。

// ============================================================================
//  brew 路径校正（2026-09-17 生产机真机事故）
//
//  现象：一键 LNMP 里"命令行开发者工具 + Homebrew"都装成功了，最后一步却失败：
//        env: /usr/local/bin/brew: No such file or directory
//        任务失败：Homebrew 装完了但执行不了
//  根因：**全新机器上配置是在"Homebrew 还不存在"时写下的**（那时按 Intel 路径
//        存了 /usr/local/bin/brew），而 Apple Silicon 上 Homebrew 装到
//        /opt/homebrew/bin/brew。装完之后没人重新探测，于是"装完了却执行不了"。
//  修法：装完（以及进来时）按候选位置**重新探测真的那个**，改写 m.opt.BrewBin。
//        服务侧所有由 brew 推导的路径（php 前缀、services 等）都来自 m.opt.BrewBin，
//        所以改这一个字段就够了；配置文件的持久化由 web 层每个请求的
//        Config.ReconcilePaths() 负责（它检测到真装了 /opt/homebrew 就会改写并保存）。
// ============================================================================

// brewBinCandidates 是 Homebrew 可能装到的位置：Apple Silicon 优先，其次 Intel。
// 变量而不是函数常量：单测要能把它指到 t.TempDir()（否则测试会依赖真机的 /opt/homebrew，
// 违反"单测不许碰真实环境"）。
var brewBinCandidates = func() []string {
	return []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"}
}

// reconcileBrewBin 在配置里的 brew 路径不存在时，找**真的装了**的那个并改用它。
// 返回是否做了修正。
func (m *Manager) reconcileBrewBin() bool {
	if m.opt.BrewBin != "" {
		if st, err := os.Stat(m.opt.BrewBin); err == nil && !st.IsDir() {
			return false
		}
	}
	for _, c := range brewBinCandidates() {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			m.opt.BrewBin = c
			return true
		}
	}
	return false
}

func (m *Manager) EnsureHomebrew(ctx context.Context, result *InstallResult) error {
	if result == nil {
		result = &InstallResult{Steps: []string{}}
	}
	// 先按实际安装位置校正一次：配置里可能写着 Intel 的 /usr/local/bin/brew，
	// 而机器上装的是 /opt/homebrew/bin/brew（Apple Silicon）——见文件末尾的说明。
	if m.reconcileBrewBin() {
		result.step(ctx, "已按实际安装位置修正 brew 路径："+m.opt.BrewBin)
	}
	if _, err := os.Stat(m.opt.BrewBin); err == nil {
		result.step(ctx, "Homebrew 已安装："+m.opt.BrewBin)
		return nil
	}

	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 Homebrew 需要以 root 运行（面板正式安装时由 LaunchDaemon 以 root 启动）")
	}
	if m.opt.UserName == "" {
		return fmt.Errorf("无法确定运行该服务的真实用户")
	}

	// 1) 命令行开发者工具（Homebrew 的前置依赖）
	if err := m.EnsureCLT(ctx, result); err != nil {
		return err
	}

	// 2) 下载安装脚本（官方不通就走镜像）
	scriptPath := "/tmp/zizpanel-brew-install.sh"
	var lastErr error
	for i, u := range brewInstallScriptCandidates() {
		label := "官方地址"
		if i > 0 {
			label = "加速镜像（第三方）"
		}
		result.step(ctx, "下载 Homebrew 安装脚本（"+label+"）")
		if _, err := m.runAsUser(ctx, 3*time.Minute, "/usr/bin/curl",
			"-fsSL", "--connect-timeout", "20", "--max-time", "120", "-o", scriptPath, u); err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return fmt.Errorf("下载 Homebrew 安装脚本失败（官方与镜像都不通）：%w", lastErr)
	}
	defer func() { _ = os.Remove(scriptPath) }()

	// 3) 装 Homebrew。
	//
	//    这里有三个必须同时满足、单独看都反直觉的条件：
	//      a) **不能以 root 跑安装脚本** —— Homebrew 检测到 EUID=0 会直接拒绝；
	//      b) **降权之后又有几步需要 root** —— 安装脚本自己会调
	//         `sudo install -d -o <user> /opt/homebrew` 建目录；
	//      c) **面板不是终端** —— sudo 没地方输密码，会直接
	//         "sudo: a terminal is required to read the password" 而失败。
	//    真机（抹机后的 mini）就是这么挂的：CLT 装完了、Homebrew 卡在
	//    `/usr/bin/sudo /usr/bin/install -d -o root -g wheel /opt/homebrew`，
	//    任务只留一句"安装 Homebrew 失败"。
	//
	//    关键事实：**官方安装脚本硬编码 `/usr/bin/sudo`**（`execute_sudo()`），
	//    所以"往 PATH 里塞一个 sudo 垫片"这条看似聪明的路走不通 ——
	//    第一版就是这么失败的，任务日志里那一行仍然是 `/usr/bin/sudo …`。
	//
	//    唯一有效的做法：让那个 sudo 真的能免密成功。事实前提是真机
	//    `sudo -n -l` 已确认的 —— 面板用户本来就是管理员、本来就是 `(ALL) ALL`，
	//    这里改的只是"安装期间不必输密码"，**装完立即撤销**，不改变用户
	//    机器的长期提权策略。垫片（brewSudoShim）保留为第二道保险，
	//    让 `sudo -u <user>` 这类调用保持"降权"的本意。
	revoke, grantErr := m.brewSudoGrant(ctx, result)
	if grantErr != nil {
		result.step(ctx, "未能临时授予免密 sudo："+grantErr.Error())
	} else {
		defer revoke()
	}
	shimDir, shimErr := m.brewSudoShim()
	if shimErr != nil {
		result.step(ctx, "未能准备提权垫片（改用直接注入 AllowRoot）："+shimErr.Error())
	} else {
		defer func() { _ = os.RemoveAll(shimDir) }()
	}
	result.step(ctx, "开始安装 Homebrew（国内镜像下通常几分钟）")
	env := append(m.brewEnv(ctx, "brew"),
		"HOMEBREW_BREW_GIT_REMOTE=https://mirrors.ustc.edu.cn/brew.git",
		"HOMEBREW_CORE_GIT_REMOTE=https://mirrors.tuna.tsinghua.edu.cn/git/homebrew/homebrew-core.git",
		// AllowRoot + 垫片 + 临时免密 sudo 是同一个目的的三道保险：
		// 三道都不成立时才会真正失败，而那种情况会**如实报出来**（不谎报成功）。
		"HOMEBREW_ALLOW_ROOT=1",
		"NONINTERACTIVE=1",
		"CI=1",
	)
	if shimDir != "" {
		env = append(env, "PATH="+shimDir+":/usr/bin:/bin:/usr/sbin:/sbin")
	}
	args := append([]string{"-n", "-u", m.opt.UserName, "/usr/bin/env"}, env...)
	args = append(args, "/bin/bash", scriptPath)
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", args...)
	cmd.Env = append(os.Environ(), "HOME="+m.opt.UserHome)
	out, err := streamCmd(ctx, cmd)
	if err != nil {
		return fmt.Errorf("安装 Homebrew 失败（输出末尾）：%s", truncate(strings.TrimSpace(out), 800))
	}

	// 4) **先校正路径再验证**：安装脚本按 Apple Silicon 规范装到 /opt/homebrew，
	//    配置里却可能还是 /usr/local —— 不校正就会误报"装完了但执行不了"。
	if m.reconcileBrewBin() {
		result.step(ctx, "已按实际安装位置修正 brew 路径："+m.opt.BrewBin)
	}
	ver, err := m.brewRun(ctx, time.Minute, "--version")
	if err != nil {
		return fmt.Errorf("Homebrew 装完了但执行不了：%s", truncate(strings.TrimSpace(ver), 300))
	}
	result.step(ctx, "Homebrew 安装完成："+strings.SplitN(strings.TrimSpace(ver), "\n", 2)[0])
	return nil
}

// ============================================================================
//  brewSudoShim：让"降权运行的 Homebrew 安装脚本"还能完成需要 root 的几步
// ============================================================================

// brewShimSudo 是垫片脚本的内容。
//
// 它在安装脚本的 PATH 里冒充 `sudo`，做两件事：
//  1. 有 `-u <用户>`：拿掉它，把命令**以当前（面板）用户**执行 ——
//     安装脚本用 `sudo -u "$USER"` 处理需要用户属主的目录，这是安全的，
//     而且正是它本意；
//  2. 没有 `-u`：原来会被以 root 跑。这里改成清掉 HOME 后**以面板用户**跑，
//     并把结果如实返回。Homebrew 的安装脚本真正需要 root 的场景只有
//     `install -d /opt/homebrew` 这类建目录，而 Homebrew 自己在 EUID=0 时
//     会做一件有意思的事：它把目录属主设成 `$SUDO_USER`。既然我们是以
//     面板用户身份跑的，`install -d` 直接就会把目录建给这个用户 —— 结果一样。
//
// 为什么不直接给用户配 NOPASSWD sudo：
//
//	那会**永久改变用户机器的提权策略**（比装个面板严重得多）。
//	这个垫片只在安装期间存在，且只作用于安装脚本自己的 PATH。
const brewShimSudo = `#!/bin/bash
# 由 ZizPanel 临时生成，仅用于 Homebrew 安装期间，安装结束后立即删除。
set -u
args=("$@")
if [ "${#args[@]}" -ge 2 ] && [ "${args[0]}" = "-u" ]; then
  args=("${args[@]:2}")
elif [ "${#args[@]}" -ge 1 ] && [ "${args[0]}" = "-n" ]; then
  args=("${args[@]:1}")
fi
exec /usr/bin/env -u HOME "${args[@]}"
`

// brewSudoShim 在临时目录里放一个 `sudo` 垫片，返回该目录（失败返回错误，
// 调用方会退回"只靠 AllowRoot"）。
//
// 目录用 root:wheel 0755 + 脚本 0755：安装脚本是降权跑的，
// 必须**读得到**这个脚本、也**执行得了**它。
func (m *Manager) brewSudoShim() (string, error) {
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("面板不是以 root 运行，垫片不会被降权进程读到")
	}
	dir, err := os.MkdirTemp("", "zizpanel-brewshim-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	path := filepath.Join(dir, "sudo")
	if err := os.WriteFile(path, []byte(brewShimSudo), 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if out, cerr := exec.Command("/bin/chmod", "0755", dir).CombinedOutput(); cerr != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("chmod %s: %v %s", dir, cerr, strings.TrimSpace(string(out)))
	}
	return dir, nil
}

// ============================================================================
//  brewSudoGrant：Homebrew 官方安装脚本必须有"免密 sudo"
// ============================================================================

// brewSudoGrantDir 是临时授予免密 sudo 的 sudoers 目录。
var brewSudoGrantDir = "/etc/sudoers.d"

// brewSudoGrantPath 是这次安装临时写入的 sudoers 文件。
func (m *Manager) brewSudoGrantPath() string {
	return filepath.Join(brewSudoGrantDir, "zizpanel-homebrew-install")
}

// brewSudoGrant 临时给安装用户开免密 sudo，返回撤销函数。
//
// 为什么必须这么做（这是真机上花了两次才定位到的一步）：
//   - Homebrew 的官方安装脚本**硬编码调用 `/usr/bin/sudo`**（`execute_sudo()`），
//     所以 PATH 垫片拦不住它 —— 我第一版就是栽在这里，任务日志里那一行
//     仍然是 `/usr/bin/sudo /usr/bin/install -d …`；
//   - 而面板是 LaunchDaemon，sudo **没有终端可以读密码**，
//     于是 `sudo: a terminal is required to read the password`，必然失败；
//   - 安装脚本自己会用 `sudo -n -l mkdir` 探一次权限，不通就直接
//     `abort "Need sudo access on macOS"`。
//
// 事实前提（真机 `sudo -n -l` 已确认）：面板用户本来就是管理员、
// 本来就拥有 `(ALL) ALL` —— 也就是"输一次密码可以 root"。这里改的只是
// **安装期间**不必输密码，安装结束后立刻撤销，不改变用户机器的长期提权策略。
func (m *Manager) brewSudoGrant(ctx context.Context, result *InstallResult) (func(), error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("面板不是以 root 运行，无法临时授予免密 sudo")
	}
	if m.opt.UserName == "" {
		return nil, fmt.Errorf("无法确定面板用户")
	}
	if _, err := os.Stat("/usr/bin/sudo"); err != nil {
		return nil, fmt.Errorf("系统里没有 sudo：%w", err)
	}
	path := m.brewSudoGrantPath()
	if err := os.MkdirAll(brewSudoGrantDir, 0o755); err != nil {
		return nil, err
	}
	body := "# ZizPanel 临时授权：只在安装 Homebrew 期间存在，装完立即删除。\n" +
		"# Homebrew 官方安装脚本要免密 sudo 才能建 /opt/homebrew 与 /etc/paths.d/homebrew。\n" +
		m.opt.UserName + " ALL=(root) NOPASSWD: ALL\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o440); err != nil {
		return nil, err
	}
	if err := os.Chown(tmp, 0, 0); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	// 语法检查：写坏 sudoers 会让**整个系统的 sudo 失效**，必须验证后再启用
	if out, verr := m.runRoot(ctx, 30*time.Second, "/usr/sbin/visudo", "-c", "-f", tmp); verr != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("临时 sudoers 语法检查失败：%s", truncate(strings.TrimSpace(out), 200))
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	revoke := func() {
		if err := os.Remove(path); err == nil {
			// -k 让已经缓存的 sudo 时间戳失效，避免"撤了但还能免密一会儿"
			_, _ = m.runRoot(context.Background(), 20*time.Second, "/usr/bin/sudo", "-k")
			if result != nil {
				result.step(context.Background(), "已撤销安装期间的临时免密 sudo 授权")
			}
		}
	}
	if result != nil {
		result.step(ctx, "已临时授予安装用户免密 sudo（仅安装期间，装完自动撤销）")
	}
	return revoke, nil
}

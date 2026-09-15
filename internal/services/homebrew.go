package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// installCLT 装命令行开发者工具：优先**无界面**路径，失败才回退到会弹窗的 xcode-select。
//
// 为什么要这样：`xcode-select --install` 会弹一个**图形对话框**，用户不点就永远卡住 ——
// 远程/无人值守的场景直接死在那里。历史上可以用一个 touch 文件让 CLT 出现在
// `softwareupdate -l` 里，从而用 `softwareupdate -i` 静默安装。
// 这个技巧在新版 macOS 上不一定还有效，所以两条路都留着，并且如实报告走了哪条。
func (m *Manager) installCLT(ctx context.Context, result *InstallResult) error {
	result.step(ctx, "缺少「命令行开发者工具」（Homebrew 的前置依赖），开始安装")

	touch := "/tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress"
	if err := os.WriteFile(touch, []byte(""), 0o644); err == nil {
		defer func() { _ = os.Remove(touch) }()
	}

	if out, err := m.runRoot(ctx, 3*time.Minute, "/usr/sbin/softwareupdate", "-l"); err == nil {
		if label := parseCLTLabel(out); label != "" {
			result.step(ctx, "用 softwareupdate 静默安装："+label+"（可能要几分钟，请勿关闭）")
			if _, ierr := m.runAsUser(ctx, 40*time.Minute, "/usr/sbin/softwareupdate", "-i", label); ierr == nil {
				if m.cltInstalled(ctx) {
					result.step(ctx, "命令行开发者工具安装完成")
					return nil
				}
			}
			result.step(ctx, "softwareupdate 这条路没成功，改用会弹窗的方式")
		} else {
			result.step(ctx, "softwareupdate 列表里没有命令行工具条目，改用会弹窗的方式")
		}
	}

	// 回退：弹窗安装。用户不点会一直等，所以把话说明白，并轮询而不是死等。
	result.step(ctx, "现在会弹出一个「安装命令行开发者工具」的对话框 —— "+
		"**请点「安装」并同意许可**，面板会在这里等（最多 30 分钟）")
	if _, err := m.runAsUser(ctx, time.Minute, "/usr/bin/xcode-select", "--install"); err != nil {
		// 已经装过时这条命令也会返回非 0，所以不当成致命错误，继续轮询
		result.step(ctx, "xcode-select --install 返回非 0（可能已经在装或已装好），继续等待")
	}
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		if m.cltInstalled(ctx) {
			result.step(ctx, "命令行开发者工具安装完成")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("等待命令行开发者工具超时（30 分钟）。请手动执行 xcode-select --install 后重试")
}

// EnsureHomebrew 保证 Homebrew 可用；缺什么装什么。已经是好的就直接返回。
func (m *Manager) EnsureHomebrew(ctx context.Context, result *InstallResult) error {
	if result == nil {
		result = &InstallResult{Steps: []string{}}
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
	if !m.cltInstalled(ctx) {
		if err := m.installCLT(ctx, result); err != nil {
			return err
		}
	} else {
		result.step(ctx, "命令行开发者工具已就绪")
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

	// 3) 装 Homebrew。必须降权（Homebrew 拒绝 root），并给它国内 git 镜像 +
	//    NONINTERACTIVE=1（否则它会"按回车继续"，在面板任务里就是永远卡住）。
	result.step(ctx, "开始安装 Homebrew（国内镜像下通常几分钟）")
	env := append(m.brewEnv(),
		"HOMEBREW_BREW_GIT_REMOTE=https://mirrors.ustc.edu.cn/brew.git",
		"HOMEBREW_CORE_GIT_REMOTE=https://mirrors.tuna.tsinghua.edu.cn/git/homebrew/homebrew-core.git",
		"NONINTERACTIVE=1",
		"CI=1",
	)
	args := append([]string{"-n", "-u", m.opt.UserName, "/usr/bin/env"}, env...)
	args = append(args, "/bin/bash", scriptPath)
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", args...)
	cmd.Env = append(os.Environ(), "HOME="+m.opt.UserHome)
	out, err := streamCmd(ctx, cmd)
	if err != nil {
		return fmt.Errorf("安装 Homebrew 失败：%s", truncate(strings.TrimSpace(out), 500))
	}

	// 4) 验证真的可用（不看退出码，跑一次 brew --version）
	ver, err := m.brewRun(ctx, time.Minute, "--version")
	if err != nil {
		return fmt.Errorf("Homebrew 装完了但执行不了：%s", truncate(strings.TrimSpace(ver), 300))
	}
	result.step(ctx, "Homebrew 安装完成："+strings.SplitN(strings.TrimSpace(ver), "\n", 2)[0])
	return nil
}

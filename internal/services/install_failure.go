package services

// install_failure.go —— 把**安装/升级失败**翻译成"下一步能照着做"的一句话。
//
// 为什么需要它（用户 2026-09-24 选定的优先项之一："一键安装的失败信息"）：
// 网络类失败早有 netfail.go 分类；但其它高频失败（磁盘满、端口被占、被 macOS
// 隐私保护挡住、formula 不存在、launchd 拒绝装载、产物架构不对）过去只会把原始
// 报错**原样**丢给用户 —— 一段英文 + 路径，用户只能猜。
//
// 两条纪律（与 netfail.go 一致，别松）：
//   · 只匹配**确定性证据**（面板自己或 brew/launchd 的固定措辞）；
//   · 拿不准就返回空串，绝不猜方向 —— 照着一个错的建议折腾比没有建议更糟。

import (
	"errors"
	"fmt"
	"strings"
)

// installAdviceMarker 让提示可以重复追加而不重复（与 NetworkHintMarker 同一思路）。
const installAdviceMarker = "【下一步】"

// InstallAdvice 返回给用户的一句可执行建议；没有确定匹配时返回空串。
//
// app 只用于把话说得更具体（端口号、应用名）；传零值也可以。
func InstallAdvice(app App, err error) string {
	if err == nil {
		return ""
	}
	label := strings.TrimSpace(app.Name)
	if label == "" {
		label = strings.TrimSpace(app.ID)
	}
	port := app.WebPort()
	msg := strings.ToLower(err.Error())

	switch {
	case containsAny(msg, []string{
		"no space left on device", "not enough space", "insufficient disk space", "disk full",
	}):
		return "磁盘空间不足：安装包要先落到磁盘再解压。先到「磁盘」页清理（升级暂存包也占地方），再重试。"

	case IsRuntimeUnavailable(err):
		return "本机没有可用的容器运行时：先装 Docker 运行时（应用市场里的 Colima / OrbStack），再重试。"

	case containsAny(msg, []string{"no available formula", "no formula found", "no formulae found"}):
		return "Homebrew 里没有这个包：名字可能已经变了或已下架。到「基础环境」确认 brew 正常，" +
			"或告诉面板维护者把 formula 名改掉。"

	case containsAny(msg, []string{"address already in use", "only one usage of each socket address"}):
		if port > 0 {
			return fmt.Sprintf("端口 %d 已经被别的程序占用：先停掉占用它的程序，"+
				"或在「应用 → 已安装 → %s → ⚙️ 管理」里改一个端口再重试。", port, label)
		}
		return "它要用的端口已经被别的程序占用：先停掉占用它的程序，或换一个端口再重试。"

	case containsAny(msg, []string{
		"operation not permitted", "read-only file system",
	}):
		return "被 macOS 的隐私/权限保护挡住（不是安装包的问题）：到「mac设置 → 权限」给面板" +
			"「完全磁盘访问权限」，或把安装目录换到不受保护的路径后重试。"

	case containsAny(msg, []string{
		"bad cpu type", "incompatible architecture", "but is an incompatible architecture",
	}):
		return "这个产物不是 arm64：上游可能没提供 Apple Silicon 版本。别硬装 —— " +
			"先在终端确认它有没有 darwin-arm64 包，再决定用手动方式。"

	case containsAny(msg, []string{"bootstrap failed", "load failed: 5", "input/output error"}):
		return "系统服务没能注册成功（launchd 拒绝装载）：到「应用 → 已安装 → " + label +
			" → ⚙️ 管理」点「重装」重试；仍失败请把「日志」页最后几行发出来（常见原因是 plist 权限或路径不存在）。"

	case containsAny(msg, []string{"已有一个安装任务", "已有一个任务", "already running"}):
		return "面板里已经有一个安装/升级任务在跑：等它结束再点（顶栏「任务中心」能看到进度）。"
	}
	return ""
}

// AppendInstallAdvice 把建议追加到错误里（原文一点不丢；幂等；没匹配就原样返回）。
func AppendInstallAdvice(app App, err error) error {
	if err == nil {
		return nil
	}
	advice := InstallAdvice(app, err)
	if advice == "" || strings.Contains(err.Error(), installAdviceMarker) {
		return err
	}
	return fmt.Errorf("%w\n%s %s", err, installAdviceMarker, advice)
}

// AppendInstallAdviceFor 按应用 ID 追加建议（找不到这个 ID 就不加 —— 目录之外的
// 安装动作，例如一键 LNMP，用的不是应用条目）。
func AppendInstallAdviceFor(appID string, err error) error {
	if err == nil {
		return nil
	}
	app, ok := FindApp(appID)
	if !ok {
		return err
	}
	return AppendInstallAdvice(app, err)
}

// ErrInstallBusy 是"已经有安装任务在跑"这一类错误的判据（供上层统一文案用）。
func ErrInstallBusy(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errInstallBusySentinel) ||
		strings.Contains(err.Error(), "已有一个安装任务") ||
		strings.Contains(err.Error(), "已有一个任务")
}

var errInstallBusySentinel = errors.New("已有安装任务在进行中")

package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
//  一键安装 LNMP（nginx + PHP-FPM + MySQL）
//
//  为什么需要这个"组合动作"，而不是让用户逐个点市场条目：
//    brew 装完这三个包之后，**网站功能还是不能用的**，实测有三个坑：
//      1. brew 的 nginx 默认 `listen 8080`，不是 80 —— 站点和面板入口都不通
//      2. nginx 的 vhosts 目录与 include 不会自动创建 —— 面板写不进站点配置
//      3. MySQL 数据目录没初始化时 mysqld 直接拒绝启动
//    再加上第四个容易被忽略的：
//      4. `brew services start` 装的是**用户级** LaunchAgent ——
//         服务器不接显示器、重启后停在登录界面时，它不会启动。
//
//    这四条都不是"包管理"能解决的，属于本机约定的收尾工作，
//    所以必须有一个人把它们串起来。串在这里，而不是写在文档里让用户照做。
//
//  设计取舍：真正"写系统 plist"那一步复用随包分发的
//  tools/system-services.sh。那段逻辑（find plist → 注入 UserName →
//  装到 /Library/LaunchDaemons → bootstrap）在真机上验证过，
//  在 Go 里重写一遍只会多一份会走样的实现。
// ============================================================================

// LNMPFormulas 是要安装的三个 formula，顺序有意义：nginx 是入口，先装它，
// 用户能在最早的时刻看到东西。
var LNMPFormulas = []string{"nginx", "php@8.3", "mysql@8.4"}

// LNMPPorts 是每个服务用于"是否真的起来了"的判定端口。
//
// 用端口而不是 HTTP：PHP-FPM 说 FastCGI、MySQL 说 MySQL 协议，
// 它们永远不可能通过 HTTP 检查（真机上就是这么被误判成"异常"的）。
var LNMPPorts = map[string]int{
	"nginx":     80,
	"php@8.3":   9000,
	"mysql@8.4": 3306,
}

// InstallLNMP 一键把 LNMP 环境装好并跑起来。
//
// 每一步都追加到 result.Steps，前端按顺序展示 —— 这个流程动辄十几分钟，
// 用户必须能看到"现在到哪了"，否则会以为卡死。
func (m *Manager) InstallLNMP(ctx context.Context, result *InstallResult) error {
	// 全新 macOS 上没有 Homebrew，而 LNMP 三件套全靠它。
	// 以前这里只会拒绝（"请先安装 Homebrew"），把用户赶回命令行 ——
	// 与"装完面板后所有操作都在面板里完成"矛盾。现在缺什么装什么（含前置的 CLT）。
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		result.step(ctx, "没有 Homebrew，先在面板里把它装上（含命令行开发者工具）")
		if err := m.EnsureHomebrew(ctx, result); err != nil {
			return err
		}
	}
	// 写 /Library/LaunchDaemons 需要 root。正式安装的面板由 LaunchDaemon
	// 以 root 运行；本地调试实例不是，这时明确拒绝而不是半途而废。
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 LNMP 需要以 root 运行（面板正式安装时由 LaunchDaemon 以 root 启动）")
	}

	result.step(ctx, "开始安装 LNMP 环境（nginx / PHP 8.3 / MySQL 8.4）")

	// ---- 1. 逐包安装 ----
	for _, f := range LNMPFormulas {
		if m.brewHas(ctx, f) {
			result.step(ctx, f+" 已安装，跳过")
			continue
		}
		result.Steps = append(result.Steps,
			fmt.Sprintf("正在 brew install %s（国内镜像下通常几分钟）", f))
		if _, err := m.brewRun(ctx, 40*time.Minute, "install", f); err != nil {
			return fmt.Errorf("安装 %s 失败: %w", f, err)
		}
		result.step(ctx, f+" 安装完成")
	}

	// ---- 2. 本机约定的收尾工作 ----
	if m.brewHas(ctx, "nginx") {
		if err := m.fixNginxBaseConfig(ctx, result); err != nil {
			// nginx 的基础配置修不好，后面站点功能一定是坏的，
			// 所以这里当成致命错误，而不是"警告但继续"。
			return err
		}
	}
	if m.brewHas(ctx, "mysql@8.4") {
		if err := m.initMySQLDataDir(ctx, result); err != nil {
			return err
		}
	}

	// ---- 3. 注册为系统级守护进程（不依赖用户登录）----
	//
	// 安全前提：**已经在跑的服务不要动**。
	//
	// 这一步会摘掉用户级 agent、改写 plist、重建 launchd 注册。
	// 对一台刚 brew install 完、什么也没跑的干净机器，这正是我们要的；
	// 但对一台**已经调好了 LNMP 的机器**（例如本机），它会拆掉能用的配置 ——
	// 我在本机实测时就是把正常工作的服务搞成失败的。
	// 所以先看端口：都在监听就说明环境已经好了，直接跳过。
	if m.allLNMPRunning(ctx) {
		result.Steps = append(result.Steps,
			"nginx / PHP / MySQL 均已在运行，跳过服务注册（不改动现有配置）")
	} else {
		result.step(ctx, "正在注册为系统级后台服务（不依赖用户登录）")
		if err := m.installSystemDaemons(ctx, result); err != nil {
			return err
		}
	}

	// ---- 4. 验证：只认端口真的在监听 ----
	result.step(ctx, "正在验证服务是否真的可用")
	var notUp []string
	for _, f := range LNMPFormulas {
		port := LNMPPorts[f]
		if !waitPort(ctx, port, 20*time.Second) {
			notUp = append(notUp, fmt.Sprintf("%s(端口 %d)", f, port))
		}
	}
	if len(notUp) > 0 {
		result.Warning = "以下服务未能确认在监听：" + strings.Join(notUp, "、") +
			"。可到「服务管理」逐个查看日志。"
		result.step(ctx, "警告："+result.Warning)
		return nil
	}
	result.step(ctx, "nginx(80) / PHP-FPM(9000) / MySQL(3306) 均已在监听")

	// ---- 5. phpMyAdmin ----
	// 放在最后：它依赖 nginx 与 PHP-FPM 都已就绪，否则装完也打不开。
	// 失败不阻断整个 LNMP —— 网站功能已经可用了，phpMyAdmin 可以稍后单独装。
	result.step(ctx, "正在部署 phpMyAdmin（数据库管理界面）")
	if err := m.InstallPhpMyAdmin(ctx, result); err != nil {
		result.Warning = "LNMP 已就绪，但 phpMyAdmin 部署失败：" + err.Error()
		result.step(ctx, "警告："+result.Warning)
	}
	return nil
}

// fixNginxBaseConfig 修正 brew 默认 nginx 配置，使其适合当服务器用。
//
// 做三件事：
//  1. `listen 8080` → `80`：brew 默认监听 8080，而站点与 /_panel 入口都按 80 写。
//     不修的话装完 nginx 也访问不到任何站点。
//  2. 建 vhosts 目录：面板的站点管理往这里写配置。
//  3. 在 nginx.conf 里 include vhosts/*.conf：不 include 的话，写进去也不生效。
func (m *Manager) fixNginxBaseConfig(ctx context.Context, result *InstallResult) error {
	conf := filepath.Join(m.brewPrefix(), "etc", "nginx", "nginx.conf")
	data, err := os.ReadFile(conf)
	if err != nil {
		return fmt.Errorf("读取 nginx.conf 失败: %w", err)
	}
	text := string(data)
	orig := text

	if strings.Contains(text, "listen       8080;") {
		text = strings.Replace(text, "listen       8080;", "listen       80;", 1)
		result.step(ctx, "已把 nginx 默认端口 8080 改为 80")
	}

	vhostDir := filepath.Join(m.brewPrefix(), "etc", "nginx", "vhosts")
	if err := os.MkdirAll(vhostDir, 0o755); err != nil {
		return fmt.Errorf("创建 vhosts 目录失败: %w", err)
	}
	// 目录归属真实用户：nginx 以该用户运行，面板也要能往里写
	if m.opt.UserName != "" {
		_ = chownTo(m.opt.UserName, vhostDir)
	}

	if !strings.Contains(text, "vhosts/*.conf") {
		// 插到最后一个 } 之前 —— 也就是 http{} 块的末尾
		i := strings.LastIndex(strings.TrimRight(text, "\n"), "}")
		if i < 0 {
			return fmt.Errorf("nginx.conf 结构异常（找不到 http 块结束符），拒绝修改")
		}
		insert := "\n    # 由 ZizPanel 添加：加载站点配置\n" +
			"    include " + vhostDir + "/*.conf;\n"
		text = text[:i] + insert + text[i:]
		result.step(ctx, "已在 nginx.conf 中启用 vhosts 目录")
	}

	if text != orig {
		// 改之前先备份一次，方便用户对照/回退
		if _, err := os.Stat(conf + ".zizpanel.bak"); err != nil {
			_ = os.WriteFile(conf+".zizpanel.bak", []byte(orig), 0o644)
		}
		// 原子写入：配置写到一半会直接让 nginx 起不来
		tmp := conf + ".tmp"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			return fmt.Errorf("写入 nginx.conf 失败: %w", err)
		}
		if err := os.Rename(tmp, conf); err != nil {
			return fmt.Errorf("替换 nginx.conf 失败: %w", err)
		}
	}
	result.step(ctx, "nginx 基础配置已就绪")
	return nil
}

// initMySQLDataDir 在数据目录为空时初始化 MySQL。
//
// 必须降权到真实用户：mysqld 拒绝以 root 运行，而且数据目录的归属
// 必须与后续运行身份一致，否则启动时会报权限错误。
func (m *Manager) initMySQLDataDir(ctx context.Context, result *InstallResult) error {
	datadir := filepath.Join(m.brewPrefix(), "var", "mysql")
	if entries, err := os.ReadDir(datadir); err == nil && len(entries) > 0 {
		return nil // 已经初始化过
	}
	result.step(ctx, "MySQL 数据目录未初始化，正在初始化（root 初始无密码）")
	if err := os.MkdirAll(datadir, 0o755); err != nil {
		return fmt.Errorf("创建 MySQL 数据目录失败: %w", err)
	}
	if m.opt.UserName != "" {
		_ = chownTo(m.opt.UserName, datadir)
	}
	mysqld := filepath.Join(m.brewPrefix(), "bin", "mysqld")
	if _, err := m.runAsUser(ctx, 5*time.Minute, mysqld,
		"--initialize-insecure", "--datadir="+datadir); err != nil {
		return fmt.Errorf("MySQL 初始化失败: %w", err)
	}
	result.step(ctx, "MySQL 数据目录初始化完成")
	return nil
}

// installSystemDaemons 把三个服务注册成系统级 LaunchDaemon。
func (m *Manager) installSystemDaemons(ctx context.Context, result *InstallResult) error {
	script := filepath.Join(m.opt.WorkDir, "..", "system-services.sh")
	if _, err := os.Stat(script); err != nil {
		// 退回随包分发的相对路径（开发态）
		script = "tools/system-services.sh"
		if _, err2 := os.Stat(script); err2 != nil {
			return fmt.Errorf("找不到 system-services.sh，无法注册系统级服务")
		}
	}
	args := append([]string{script}, LNMPFormulas...)
	out, err := m.runRoot(ctx, 10*time.Minute, "/bin/bash", args...)
	if err != nil {
		return fmt.Errorf("注册系统级服务失败: %v（输出：%s）", err, tailText(stripANSI(out), 500))
	}
	result.step(ctx, "已注册为系统级 LaunchDaemon（开机自启，不依赖登录）")
	return nil
}

// ---------------------------------------------------------------------------
//  小工具
// ---------------------------------------------------------------------------

func (m *Manager) brewPrefix() string {
	return filepath.Dir(filepath.Dir(m.opt.BrewBin))
}

// allLNMPRunning 判断 LNMP 三个服务是否都已在监听。
//
// 用它来避免"重复改造"：环境已经能用的时候，动 plist 只有坏处没有好处。
func (m *Manager) allLNMPRunning(ctx context.Context) bool {
	for _, f := range LNMPFormulas {
		if !portOpen(ctx, LNMPPorts[f]) {
			return false
		}
	}
	return true
}

// stripANSI 去掉终端颜色转义。
//
// 子脚本（system-services.sh）是给人看的，会输出颜色码；
// 原样塞进 JSON 会让界面显示一堆 [1m[34m 之类的噪声。
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !(s[j] >= '@' && s[j] <= '~') {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// waitPort 轮询等端口起来。服务启动需要时间，立即检查必然失败。
func waitPort(ctx context.Context, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portOpen(ctx, port) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(400 * time.Millisecond):
		}
	}
	return false
}

// runAsUser 以真实用户身份执行命令（brew / mysqld 都拒绝 root）。
func (m *Manager) runAsUser(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append([]string{"-n", "-u", m.opt.UserName, name}, args...)
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", full...)
	// brew 需要正确的 HOME 才能找到 Cellar 与缓存
	if m.opt.UserHome != "" {
		cmd.Env = append(os.Environ(), "HOME="+m.opt.UserHome)
	}
	return streamCmd(ctx, cmd)
}

// runRoot 以 root 执行命令（面板本身通常就是 root）。
func (m *Manager) runRoot(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return streamCmd(ctx, exec.CommandContext(ctx, name, args...))
}

func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// portOpen 判断本机端口是否可连。
func portOpen(ctx context.Context, port int) bool {
	return portListening(ctx, port)
}

// chownTo 把路径归属改为指定用户（保持原有组）。
func chownTo(user, path string) error {
	uid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-u", user)))
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-g", user)))
	if err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func runOutput(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return string(out)
}

// ---------------------------------------------------------------------------
//  launchd 服务的安全装载
// ---------------------------------------------------------------------------

// bootstrapService 卸载旧实例并重新装载，**等旧实例真正消失再装**。
//
// 为什么必须等：`launchctl bootout` 是**异步**的，它一返回并不代表服务
// 已经卸载完成。此时立刻 bootstrap 会失败（Bootstrap failed: 5），
// 而失败的 bootstrap 又让后续 kickstart 无对象可踢 —— 结果是服务没了。
//
// 这个坑在本项目的 install.sh 里踩过一次并修好了（wait_service_stopped），
// 但这段 Go 代码最初没照做，于是又在真机上复现了一遍（Qwen 服务被 bootout
// 后没起来、8880 无监听）。同一个教训要落实在所有地方，不能只在修过的那处。
func (m *Manager) bootstrapService(ctx context.Context, label, plist string) error {
	_, _ = m.runRoot(ctx, 20*time.Second, "/bin/launchctl", "bootout", "system/"+label)

	// ① 等 launchd 真的把服务摘掉
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.runRoot(ctx, 5*time.Second,
			"/bin/launchctl", "print", "system/"+label); err != nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	// ② 带重试地装载：卸载刚结束的一小段时间内 bootstrap 仍可能失败
	var lastOut string
	for attempt := 1; attempt <= 5; attempt++ {
		out, err := m.runRoot(ctx, 30*time.Second,
			"/bin/launchctl", "bootstrap", "system", plist)
		if err == nil {
			return nil
		}
		lastOut = out
		// 已加载的情况：重启它即可
		if _, perr := m.runRoot(ctx, 10*time.Second,
			"/bin/launchctl", "print", "system/"+label); perr == nil {
			if _, kerr := m.runRoot(ctx, 20*time.Second,
				"/bin/launchctl", "kickstart", "-k", "system/"+label); kerr == nil {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("装载服务失败（已重试 5 次）：%s", tailText(stripANSI(lastOut), 300))
}

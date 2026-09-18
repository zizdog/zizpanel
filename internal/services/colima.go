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
//  Docker 运行时（Colima）
//
//  为什么把 Docker 引擎本身也做成一个「服务」：
//    Docker 在这台机器上不是系统自带的守护进程，而是 Colima 起的一个 Linux
//    虚拟机。虚拟机没起来时所有容器都没了，而用户此前只能 SSH 上去手敲
//    `colima start` —— 面板既然号称"全部网页操作"，就必须能管它。
//
//  为什么选 Colima 而不是 OrbStack：
//    OrbStack 依赖 GUI 会话与 Rosetta，在无人登录的 Mac mini 上安装即失败
//    （实测报 OSLaunchdErrorDomain Code=125）。Colima 基于 Lima，纯命令行、
//    原生 aarch64，无 GUI 也能跑。
// ============================================================================

const (
	// ColimaServiceName 是登记到「服务管理」里的服务名。
	ColimaServiceName = "docker-runtime"
	// ColimaDisplayName 是界面上显示的名字。
	ColimaDisplayName = "Docker 运行时（Colima）"
	// ColimaLaunchLabel 是开机自启用的 LaunchDaemon 标签。
	ColimaLaunchLabel = "com.zizdog.colima"
	// ColimaPlistPath 是开机自启的 plist 路径（系统域，无需登录）。
	ColimaPlistPath = "/Library/LaunchDaemons/" + ColimaLaunchLabel + ".plist"
)

// colimaBin 返回 colima 可执行文件路径。
func (m *Manager) colimaBin() string {
	return filepath.Join(filepath.Dir(m.opt.BrewBin), "colima")
}

// ColimaInstalled 报告本机是否装了 colima。
func (m *Manager) ColimaInstalled() bool { return fileExists(m.colimaBin()) }

// colimaEnv 返回调用 colima 时需要注入的环境变量。
//
// **PATH 是必须的，这不是可选项。** colima 会去 PATH 里查找 limactl/ssh 等依赖；
// 而面板、launchd、`sudo -n -u` 三者的默认 PATH 都不含 /opt/homebrew/bin，
// 于是 colima 会报
//
//	level=fatal msg="Error: dependency check failed for VM: lima not found,
//	                 run 'brew install lima' to install"
//
// 这个报错极具误导性：lima 明明装好了，只是找不到而已。实测正是如此 ——
// 同一条命令带上 PATH 就走通、不带就失败。因此所有 colima 调用都必须经过这里。
func (m *Manager) colimaEnv() []string {
	brewBin := filepath.Dir(m.opt.BrewBin) // 例如 /opt/homebrew/bin
	path := brewBin + ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	env := []string{
		"PATH=" + path,
		"HOME=" + m.opt.UserHome,
	}
	// 保留 TMPDIR：colima 与 lima 都要在家目录之外找临时目录，
	// launchd 环境里缺少它时会出现权限怪问题。
	if v := os.Getenv("TMPDIR"); v != "" {
		env = append(env, "TMPDIR="+v)
	}
	return env
}

// runColima 以真实用户身份执行 colima 子命令。
//
// 必须以真实用户运行：Colima 的虚拟机、socket、配置全在该用户家目录下，
// 用 root 跑会另外建一套 ~/.colima（变成 /var/root/.colima），
// 表现为"面板启动了 Docker 但用户看不到自己的容器"。
func (m *Manager) runColima(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	bin := m.colimaBin()
	if !fileExists(bin) {
		return "", fmt.Errorf("未安装 Colima")
	}
	full := append([]string{"-n", "-u", m.opt.UserName, "/usr/bin/env"}, append(m.colimaEnv(), append([]string{bin}, args...)...)...)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", full...)
	// colima 启动虚拟机可能要几十秒到几分钟，逐行流式让用户看得到进展。
	// 标签必须带上 sudo 本身：日志里显示的命令要是**能照着复制**的完整命令，
	// 少了 sudo 用户会以为是没降权的直接调用（那就完全是另一回事了）。
	out, err := streamCmd(ctx, cmd)
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s", firstMeaningfulLine(text))
		}
		return text, err
	}
	return text, nil
}

// firstMeaningfulLine 从 colima 的日志式输出里挑出有信息量的一行。
//
// colima 把日志写成 `time="..." level=fatal msg="..."`，直接整段丢给用户
// 既长又难读，这里只取 msg 后面的内容。
//
// 要优先找带 msg=" 的行，而不是简单取第一行非空内容：colima 经常先打几行
// level=info 的进度信息，真正的失败原因在后面 —— 取第一行会把它丢掉。
// 只有整段都没有 msg=" 时，才退回第一行非空内容。
func firstMeaningfulLine(out string) string {
	fallback := ""
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if fallback == "" {
			fallback = ln
		}
		i := strings.Index(ln, `msg="`)
		if i < 0 {
			continue
		}
		rest := ln[i+len(`msg="`):]
		if j := strings.LastIndex(rest, `"`); j >= 0 {
			rest = rest[:j]
		}
		if rest != "" {
			return rest
		}
	}
	return fallback
}

// ---------------------------------------------------------------------------
//  驱动
// ---------------------------------------------------------------------------

type colimaDriver struct {
	opt Options
	svc *Service
}

func newColimaDriver(opt Options, s *Service) *colimaDriver {
	return &colimaDriver{opt: opt, svc: s}
}

func (d *colimaDriver) Kind() Kind { return KindColima }

// Status 查询虚拟机与 Docker 引擎的状态。
// colimaFastState 用**文件系统判据**判断 Colima 虚拟机是否在跑。
//
// 为什么不直接跑 `colima status`：实测它要 **1.07 秒**（起 CLI、读配置、连 API），
// 而服务管理页每次刷新都会查它 —— 整页 1.4s 里有 1.07s 花在这一条上。
// 用户抱怨"服务管理页面加载慢"，根因就是这里。
//
// Lima 把实例状态放在 ~/.colima/_lima/<instance>/ 下：
//
//	ha.pid    hostagent 的 pid（虚拟机在跑时它一定活着）
//	ssh.sock  串口/ssh 复用 socket
//
// 读这两个文件只要几毫秒。判据取"pid 活着 **且** socket 存在"——
// 只判 pid 会被 PID 复用骗到，只判 socket 会被残留文件骗到。
//
// found=false 表示根本没找到实例目录（即从未启动过），调用方据此报"未启动"。
func (m *Manager) colimaFastState() (running bool, detail string, found bool) {
	home := m.opt.UserHome
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return false, "", false
	}

	// LIMA_HOME 可覆盖；Colima 默认用 ~/.colima/_lima。
	base := os.Getenv("LIMA_HOME")
	if base == "" {
		base = filepath.Join(home, ".colima", "_lima")
	}
	// 目录布局与判据的**唯一实现**在 readColimaFastState（docker_runtime.go）：
	// DockerRuntimeStatus 也要用同一套（pid 活着 **且** ssh.sock 在），
	// 判据抄两份迟早会分叉，出现"这里说在跑、那里说没跑"。
	return readColimaFastState(base)
}

func (d *colimaDriver) Status(ctx context.Context) (State, error) {
	m := &Manager{opt: d.opt}
	if !m.ColimaInstalled() {
		return State{Status: "not-installed", Detail: "未安装 Colima（brew install colima docker docker-compose）"}, nil
	}
	// 快路径：读 Lima 实例目录（毫秒级）
	if running, detail, found := m.colimaFastState(); found {
		if !running {
			return State{Status: "stopped", Detail: detail}, nil
		}
		st := State{Status: "running", Running: true, Detail: detail}
		if sock := m.opt.DockerSocket; sock != "" {
			st.Detail += "（socket: " + sock + "）"
		}
		return st, nil
	}

	// 慢路径：没找到实例目录时仍然问一次 CLI。
	// （理论上不该走到这里；留着是为了"判据失效"时行为不倒退，
	//   宁可这一条慢，也不要误报成未安装/未运行。）
	out, err := m.runColima(ctx, 20*time.Second, "status")
	if err != nil {
		// 虚拟机没起来时 colima status 返回非 0，这属于正常状态而非错误。
		return State{
			Status: "stopped",
			Detail: firstMeaningfulLine(out),
		}, nil
	}

	st := State{Status: "running", Running: true}
	sock := ""
	for _, ln := range strings.Split(out, "\n") {
		if i := strings.Index(ln, "docker socket:"); i >= 0 {
			sock = strings.TrimSpace(strings.TrimPrefix(ln[i:], "docker socket:"))
		}
	}
	if sock == "" {
		sock = m.opt.DockerSocket
	}
	st.Detail = "虚拟机运行中"
	if sock != "" {
		st.Detail += "（socket: " + sock + "）"
	}
	return st, nil
}

func (d *colimaDriver) Start(ctx context.Context) error {
	m := &Manager{opt: d.opt}
	// 先把 compose 数据目录的挂载补进配置（D13）：挂载不是 colima 的"固定配置"，
	// 可以在已有虚拟机上 stop→start 生效，**不需要重建 VM**。放在 start 之前，
	// 是因为已经跑着的实例上 `colima start` 是空操作（源码：already running,
	// ignoring），改完不重启等于没改。
	if err := m.ApplyColimaWorkDirMount(ctx); err != nil {
		return fmt.Errorf("配置 compose 数据目录挂载失败: %w", err)
	}
	// 冷启动要拉起虚拟机；首次还要下 317MiB 的 guest 镜像（公网实测约 71 分钟，
	// 已从自建镜像站预热则几秒），所以超时按 colimaStartTimeout（90 分钟）给。
	if _, err := m.runColima(ctx, colimaStartTimeout, "start"); err != nil {
		return fmt.Errorf("启动 Docker 运行时失败: %w", err)
	}
	return nil
}

func (d *colimaDriver) Stop(ctx context.Context) error {
	m := &Manager{opt: d.opt}
	if _, err := m.runColima(ctx, 2*time.Minute, "stop"); err != nil {
		return fmt.Errorf("停止 Docker 运行时失败: %w", err)
	}
	return nil
}

func (d *colimaDriver) Restart(ctx context.Context) error {
	if err := d.Stop(ctx); err != nil {
		return err
	}
	return d.Start(ctx)
}

// Logs 返回虚拟机宿主代理的日志。
//
// 选 ha.stderr.log 而不是 colima 自己的输出：前者是 lima 里的 host agent，
// 容器/挂载/网络的问题都在这里；后者只记录 `colima start` 那一次的结果。
func (d *colimaDriver) Logs(ctx context.Context, lines int) (string, error) {
	path := d.logPath()
	content, err := tailFile(path, lines)
	if err != nil {
		return "", fmt.Errorf("读取 Docker 运行时日志失败: %w", err)
	}
	return content, nil
}

func (d *colimaDriver) logPath() string {
	if d.svc.LogPath != "" {
		return d.svc.LogPath
	}
	return filepath.Join(d.opt.UserHome, ".colima", "_lima", "colima", "ha.stderr.log")
}

// LogStream 用轮询实现增量推送，与 dockerDriver 保持同一套策略。
func (d *colimaDriver) LogStream(ctx context.Context) (<-chan string, error) {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		last := ""
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				content, err := tailFile(d.logPath(), 50)
				if err != nil || content == last {
					continue
				}
				chunk := diffLog(last, content)
				last = content
				if chunk != "" {
					select {
					case ch <- chunk:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch, nil
}

// Uninstall 拒绝卸载 Docker 运行时。
//
// 面板不负责装/卸容器运行时（那是 brew 的事），而且它是所有 docker 应用的前提，
// 让面板能一键删掉它只会制造事故。这里明确报错而不是静默成功。
func (d *colimaDriver) Uninstall(ctx context.Context) error {
	return fmt.Errorf("Docker 运行时由 Homebrew 管理，面板不提供卸载；如需移除请执行 brew uninstall colima")
}

// Health 对运行时本身没有可探的 HTTP 端点，健康与否直接由 Status 反映。
func (d *colimaDriver) Health(ctx context.Context) Health {
	return Health{Checked: false}
}

// ---------------------------------------------------------------------------
//  开机自启与登记
// ---------------------------------------------------------------------------

// EnsureColimaRuntime 保证「Docker 运行时」可用且已登记到服务管理。
//
// 做两件事，都是幂等的：
//  1. 若装了 colima 但开机自启的 LaunchDaemon 缺失，则补上（这是可复现性的关键：
//     Mini 上那份是手工建的，换一台 Mac 就不会有）；
//  2. 把 Docker 运行时登记成一条服务记录，让用户在「服务管理」里能启停看日志。
//
// 返回登记数量（0 或 1）以及是否新建了 plist。
func (m *Manager) EnsureColimaRuntime(ctx context.Context) (registered int, plistCreated bool) {
	if !m.ColimaInstalled() {
		return 0, false
	}
	// 1. 开机自启
	if !fileExists(ColimaPlistPath) && m.opt.UserName != "" {
		if err := m.writeColimaPlist(); err == nil {
			plistCreated = true
			_, _ = m.runRoot(ctx, 20*time.Second, "/bin/launchctl", "bootstrap", "system", ColimaPlistPath)
		}
	}
	// 2. 登记服务
	if err := m.RegisterColimaRuntime(ctx); err == nil {
		registered = 1
	}
	return registered, plistCreated
}

// writeColimaPlist 写出 Colima 的开机自启 plist。
//
// 用 LaunchDaemon 而不是 LaunchAgent：Daemon 属于系统域，开机即运行，
// 不需要任何人登录图形界面 —— 这正是 Mac mini 作为无人值守服务器的前提。
// 同时必须设 UserName，因为 Colima 的 VM 与 socket 都在真实用户家目录下。
func (m *Manager) writeColimaPlist() error {
	home := m.opt.UserHome
	if home == "" {
		return fmt.Errorf("未知真实用户家目录，无法配置 Colima 开机自启")
	}
	logDir := filepath.Join(home, ".colima")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	// 面板以 root 运行，MkdirAll 会把目录建成 root 所有；但 Colima 是以真实用户
	// 身份运行的，之后要在这个目录里创建 default/ 等子目录。root 拥有的目录会让它
	// 直接失败：
	//   cannot make required directory:
	//   mkdir /Users/<user>/.colima/default: permission denied
	// 这个坑只在**干净机器**上才会暴露 —— 已装过 Colima 的机器该目录早已存在
	// 且归属正确，所以照着旧机器测是测不出来的。
	if err := chownPath(m.opt.UserName, logDir); err != nil {
		return fmt.Errorf("设置 %s 归属失败: %w", logDir, err)
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <!-- 以真实用户运行：Colima 的虚拟机与 socket 都在该用户家目录下 -->
    <key>UserName</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>start</string>
    </array>
    <!-- 一次性任务：把虚拟机拉起来就退出，不需要常驻，也不用 KeepAlive -->
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <false/>
    <key>EnvironmentVariables</key>
    <dict>
        <!-- PATH 必须显式给出：colima 靠它找 limactl，缺了就报 lima not found -->
        <key>PATH</key>
        <string>%s:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
        <key>HOME</key>
        <string>%s</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, ColimaLaunchLabel, m.opt.UserName, m.colimaBin(), filepath.Dir(m.opt.BrewBin), home,
		filepath.Join(logDir, "launchd.out.log"), filepath.Join(logDir, "launchd.err.log"))

	tmp := ColimaPlistPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	// plist 必须 root:wheel 0644，否则 launchd 会拒绝加载。
	_ = os.Chown(tmp, 0, 0)
	_ = os.Chmod(tmp, 0o644)
	return os.Rename(tmp, ColimaPlistPath)
}

// RegisterColimaRuntime 把 Docker 运行时登记为一条可管理的服务（幂等）。
func (m *Manager) RegisterColimaRuntime(ctx context.Context) error {
	// 清理历史遗留：早期版本里 AutoRegisterKnown 会把 com.zizdog.colima
	// 当作普通 launchd 服务登记成 kind=native，导致列表里同一个东西出现两次。
	// 这里按 label 找出并删掉那条错误记录。
	if list, err := m.repo.List(ctx); err == nil {
		for _, s := range list {
			if s.LaunchLabel == ColimaLaunchLabel && s.Kind != KindColima {
				_ = m.repo.Delete(ctx, s.Name)
			}
		}
	}
	if ok, err := m.repo.Exists(ctx, ColimaServiceName); err == nil && ok {
		return nil // 已登记，保持幂等
	}
	svc := &Service{
		Name:        ColimaServiceName,
		DisplayName: ColimaDisplayName,
		Kind:        KindColima,
		Category:    "runtime",
		Icon:        "🐳",
		Description: "Docker 引擎所在的 Linux 虚拟机。停止后所有容器都会不可用。",
		// LaunchLabel 必须与目录条目的 ServiceLabel 一致：市场靠它判断
		// "这个服务是否已经纳管"，否则会重复出现纳管入口。
		LaunchLabel: ColimaLaunchLabel,
		Autostart:   true,
		Enabled:     true,
		Managed:     false,
		LogPath:     filepath.Join(m.opt.UserHome, ".colima", "_lima", "colima", "ha.stderr.log"),
	}
	return m.repo.Create(ctx, svc)
}

// InstallColimaRuntime 一键装好 Docker 运行时并纳入管理。
//
// 步骤刻意分得很清楚：brew 装三个 formula → 写开机自启 → 起虚拟机 → 验证引擎可用。
// 其中"验证"必须真的调一次 Docker API，不能只看命令返回码：
// 本项目多次踩过"命令成功 ≠ 服务可用"的坑（nginx -t 通过但入口没生效、
// 端口在监听但页面报错、HTTP 200 却是错误页）。
func (m *Manager) InstallColimaRuntime(ctx context.Context, result *InstallResult) error {
	if m.opt.BrewBin == "" || !fileExists(m.opt.BrewBin) {
		return fmt.Errorf("未安装 Homebrew，无法安装 Docker 运行时")
	}

	// ---- 1. 装 colima / docker / docker-compose ----
	// docker-compose 不在 colima 的依赖里，但 compose 类应用要用它，一并装上。
	need := []string{}
	for _, f := range []string{"colima", "docker", "docker-compose"} {
		if !m.brewFormulaInstalled(ctx, f) {
			need = append(need, f)
		}
	}
	if len(need) > 0 {
		// 走 brewInstall（内部用 brewRun 同款降权+镜像环境，但失败会自动换源）：
		// 面板是 LaunchDaemon，读不到用户 shell 里的 HOMEBREW_*，
		// 不注入就只有官方源可用；而只注入不兜底，一旦镜像上的瓶是坏的就整单失败。
		if _, err := m.brewInstall(ctx, result, 15*time.Minute, need...); err != nil {
			return fmt.Errorf("安装 %s 失败: %w", strings.Join(need, " "), err)
		}
		result.step(ctx, "已安装 "+strings.Join(need, "、"))
	} else {
		result.step(ctx, "colima / docker / docker-compose 已存在，跳过安装")
	}

	// ---- 2. 开机自启 + 登记 ----
	if _, created := m.EnsureColimaRuntime(ctx); created {
		result.step(ctx, "已配置开机自启（系统级 LaunchDaemon，无需登录）")
	} else {
		result.step(ctx, "开机自启已存在，保持不动")
	}

	// ---- 3. 首次启动前先解决两个"必然卡住"的文件 ----
	//
	//  order 很重要：这两件事都必须在**首次 colima start 之前**写进配置，
	//  否则第一次拉取仍然没有加速源、第一次启动仍然没有挂载表。
	//  ① guest 镜像：Colima 从 GitHub 下 317MiB，公网实测约 71 分钟，
	//     而旧代码只给 5 分钟超时 → 必然被掐断。这里先按 Colima 自己的
	//     缓存命名规则从自建镜像站预热，命中缓存就一个字节都不用下。
	//  ② 加速源 + 挂载：见 EnsureDockerMirrorsForRuntime / ApplyColimaWorkDirMount。
	if note := m.PrewarmColimaGuestImage(ctx, result); note != "" {
		result.step(ctx, note)
	}
	m.EnsureDockerMirrorsForRuntime(ctx, result)
	if err := m.ApplyColimaWorkDirMount(ctx); err != nil {
		return fmt.Errorf("配置 compose 数据目录挂载失败: %w", err)
	}

	// ---- 4. 起虚拟机 ----
	// 已经跑着时 start 是幂等的，不会重复建 VM。
	// 失败必须**报错**、不能只写 Warning：运行时是 9 个 Docker 应用的前提，
	// 报"装好了"而实际没起来就是典型的谎报成功。
	if _, err := m.runColima(ctx, colimaStartTimeout, "start"); err != nil {
		return fmt.Errorf("Docker 虚拟机启动失败：%w（guest 镜像若需从公网下载约 317MiB，"+
			"国内实测可能需要 1 小时以上；可用镜像站的 colima 目录预热后重试）", err)
	}
	result.step(ctx, "Docker 虚拟机已启动")
	m.logColimaGuestSource(ctx)

	// ---- 5. 验证：真的调一次 Docker API ----
	sock := filepath.Join(m.opt.UserHome, ".colima", "default", "docker.sock")
	if !waitFile(sock, 60*time.Second) {
		return fmt.Errorf("虚拟机已启动，但 60 秒内未出现 Docker socket（%s）—— 运行时不可用，请查看运行时日志", sock)
	}
	client := newDockerClient(sock)
	ver := ""
	for i := 0; i < 20; i++ {
		if v, err := client.serverVersion(ctx); err == nil && v != "" {
			ver = v
			break
		}
		time.Sleep(time.Second)
	}
	if ver == "" {
		return fmt.Errorf("Docker socket 已就绪，但引擎 API 无响应 —— 运行时不可用，请查看运行时日志")
	}
	result.step(ctx, "Docker 引擎已就绪（Server "+ver+"）")

	// ---- 6. 把"镜像到底走哪条路"写进日志（D18）----
	result.step(ctx, m.DockerMirrorRuntimeNote(ctx))
	result.Message = "Docker 运行时安装完成，现在可以安装 Docker 类应用了"
	return nil
}

// brewFormulaInstalled 判断某个 formula 是否已安装。
func (m *Manager) brewFormulaInstalled(ctx context.Context, formula string) bool {
	out, err := m.runAsUser(ctx, time.Minute, m.opt.BrewBin, "list", "--formula", formula)
	if err != nil {
		return false
	}
	return strings.Contains(out, formula)
}

// waitFile 等待文件出现，用于等 Docker socket 就绪。
func waitFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fileExists(path) {
			return true
		}
		time.Sleep(time.Second)
	}
	return fileExists(path)
}

// chownPath 把单个路径的归属改为指定用户（非递归）。
//
// 与 chownTree 的区别：这里只改目录本身。用于"面板以 root 建了用户的目录"这种
// 场景 —— 只需让目录属主正确，用户就能在其中创建子目录，不必递归整个 VM 目录。
func chownPath(user, path string) error {
	uid, gid, err := lookupUIDGID(user)
	if err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

// lookupUIDGID 解析用户名对应的 uid 与 gid。
func lookupUIDGID(user string) (int, int, error) {
	uid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-u", user)))
	if err != nil {
		return 0, 0, fmt.Errorf("解析用户 %s 的 uid 失败: %w", user, err)
	}
	gid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-g", user)))
	if err != nil {
		return 0, 0, fmt.Errorf("解析用户 %s 的 gid 失败: %w", user, err)
	}
	return uid, gid, nil
}

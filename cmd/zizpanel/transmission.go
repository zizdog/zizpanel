// Transmission（下载器）的常驻 supervisor（`zizpanel transmission-supervise`）。
//
// 它由**系统级 LaunchDaemon** com.zizdog.transmission 以 root 拉起，职责很小：
// fork 一个 `transmission-daemon --foreground --config-dir … --log-info --logfile …`
// 子进程并保活（意外退出 → 1s/2s/5s 退避重启），收到 SIGTERM/SIGINT 时**转发给
// 子进程并等它退出**（daemon 要趁这时候把内存里的 settings.json 回写落盘），
// 等不到再强杀。
//
// 为什么是"面板自己的子命令"（2026-10-06 用户点名，与 aria2 同一套）：
// 面板二进制 fork 后 setuid 到真实用户，下载文件归属用户；与面板同一代码要求
// ⇒ 用户把下载目录指到受保护目录/外接盘时继承面板的「完全磁盘访问权限」（坑 217）。
// 直跑 brew 的 transmission-daemon 不行：它 adhoc 签名、身份里带二进制哈希，
// 每次 brew 升级都变，给它的授权随即失效（后台服务弹不出授权框，open() 会挂住）。
//
// 为什么以 root 跑 supervisor、以真实用户跑子进程：LaunchDaemon 在系统域，root 才能
// fork 后 setuid；下载文件必须属于用户（root 写出来的文件用户在 Finder 里都删不掉）。
// 降权用 SysProcAttr.Credential 直接 setuid/setgid，**不经 sudo**。
//
// ⚠️ 参数是**冻结契约**：plist 由 internal/services/transmission.go 的
// TransmissionSuperviseArgs 生成，两边必须逐字一致（咬合门禁在
// cmd/zizpanel/supervise_contract_test.go）。改任一 flag 名 = 破坏兼容。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// transmissionLookupUser 是用户查询的注入口（单测注入假用户，绝不依赖真机账号）。
var transmissionLookupUser = user.Lookup

// transmissionSuperviseOptions 是 supervisor 一次运行的全部输入（uid/gid 已解析）。
type transmissionSuperviseOptions struct {
	User string
	UID  int
	GID  int
	// Bin 是 brew 装出来的 transmission-daemon 绝对路径。
	Bin string
	// ConfigDir 是 daemon 的 --config-dir（settings.json 与日志都在里面）。
	ConfigDir string
	// LogFile 是 daemon 的 --logfile（与 --log-info 一起传给子进程）。
	LogFile string
	// Home 是真实用户家目录（作为子进程 HOME）。
	Home string
}

// transmissionSuperviseOptionsFrom 校验参数并解析真实用户的 uid/gid。
func transmissionSuperviseOptionsFrom(userName, bin, configDir, logFile, home string) (transmissionSuperviseOptions, error) {
	o := transmissionSuperviseOptions{}
	userName = strings.TrimSpace(userName)
	if userName == "" {
		return o, errors.New("缺少 --user：Transmission 必须以真实用户身份运行，不能是 root")
	}
	u, err := transmissionLookupUser(userName)
	if err != nil {
		return o, fmt.Errorf("查不到用户 %s：%w", userName, err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(u.Uid))
	if err != nil || uid <= 0 {
		return o, fmt.Errorf("用户 %s 的 uid 不可用（%q）：拒绝以 root/系统用户运行 Transmission", userName, u.Uid)
	}
	gid, err := strconv.Atoi(strings.TrimSpace(u.Gid))
	if err != nil {
		return o, fmt.Errorf("用户 %s 的 gid 不可用（%q）", userName, u.Gid)
	}
	for _, kv := range []struct{ name, val string }{
		{"--bin", bin},
		{"--config-dir", configDir},
		{"--log", logFile},
		{"--home", home},
	} {
		if !filepath.IsAbs(strings.TrimSpace(kv.val)) {
			return o, fmt.Errorf("%s 必须是绝对路径，实际 %q", kv.name, kv.val)
		}
	}
	return transmissionSuperviseOptions{
		User:      userName,
		UID:       uid,
		GID:       gid,
		Bin:       filepath.Clean(strings.TrimSpace(bin)),
		ConfigDir: filepath.Clean(strings.TrimSpace(configDir)),
		LogFile:   filepath.Clean(strings.TrimSpace(logFile)),
		Home:      filepath.Clean(strings.TrimSpace(home)),
	}, nil
}

// transmissionChildCmd 返回**尚未启动**的 transmission-daemon 子进程。
//
// 子进程参数与 brew formula 的 service 块一致（冻结契约）：
//
//	--foreground --config-dir <configDir> --log-info --logfile <logFile>
//
// 身份：SysProcAttr.Credential 直接 setuid/setgid 到真实用户（supervisor 是 root）；
// 环境只给用户自己的 HOME/USER/LOGNAME 与固定的 PATH（launchd 默认环境里没有
// /opt/homebrew/bin）。**不启动**是为了让门禁能断言这份参数与身份。
func transmissionChildCmd(o transmissionSuperviseOptions) *exec.Cmd {
	cmd := exec.Command(o.Bin,
		"--foreground",
		"--config-dir", o.ConfigDir,
		"--log-info",
		"--logfile", o.LogFile,
	)
	cmd.Dir = o.ConfigDir
	cmd.Env = []string{
		"HOME=" + o.Home,
		"USER=" + o.User,
		"LOGNAME=" + o.User,
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}
	// 只有 root 能 setuid；非 root 时（本机手工验收：euid 已经是目标用户）不能再设，
	// 否则 cmd.Start 会 EPERM。
	if os.Geteuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: uint32(o.UID),
			Gid: uint32(o.GID),
		}}
	}
	return cmd
}

// runTransmissionSupervisor 拉起并保活 transmission-daemon，直到收到 SIGTERM/SIGINT。
//
// 日志：子进程直接复用 supervisor 的 stdout/stderr（launchd 会把它们落到 plist 的
// StandardOutPath/StandardErrorPath），所以 daemon 的输出与 supervisor 自己的
// `[supervise]` 行落在同一份日志里 —— 排查时不用两头找。
func runTransmissionSupervisor(ctx context.Context, o transmissionSuperviseOptions) error {
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s [supervise] "+format+"\n",
			append([]any{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
	}
	logf("supervisor 启动：以 %s（uid=%d gid=%d，HOME=%s）运行 %s --foreground --config-dir %s",
		o.User, o.UID, o.GID, o.Home, o.Bin, o.ConfigDir)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	backoffs := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		cmd := transmissionChildCmd(o)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		logf("启动 transmission-daemon：%s", strings.Join(cmd.Args, " "))
		start := time.Now()
		if err := cmd.Start(); err != nil {
			// 起不来（二进制不在、config-dir 不可读）也要继续退避重试：LaunchDaemon 的
			// KeepAlive 只管 supervisor 自己，supervisor 退出就没人再拉 daemon 了。
			logf("启动 transmission-daemon 失败：%v", err)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case s := <-sigCh:
				// 关键：先把信号交给 daemon，让它把内存里的 settings.json 回写落盘。
				logf("收到信号 %s，转发给 transmission-daemon 并等它退出", s)
				_ = cmd.Process.Signal(s)
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					logf("transmission-daemon 15 秒内没有退出，强制结束")
					_ = cmd.Process.Kill()
					<-done
				}
				return nil
			case werr := <-done:
				if werr != nil {
					logf("transmission-daemon 退出：%v", werr)
				} else {
					logf("transmission-daemon 正常退出（exit 0）")
				}
			}
		}
		// 稳定跑满 1 分钟才重置退避：崩溃循环不会退化成"秒级疯狂重启刷日志"。
		if time.Since(start) >= time.Minute {
			attempt = 0
		}
		delay := backoffs[attempt]
		if attempt < len(backoffs)-1 {
			attempt++
		}
		logf("%s 后重启 transmission-daemon", delay)
		select {
		case s := <-sigCh:
			logf("收到信号 %s，退出 supervisor", s)
			return nil
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// transmissionOptionsFromArgs 解析 supervisor 的 argv（供入口与"参数咬合"门禁共用）。
func transmissionOptionsFromArgs(args []string) (transmissionSuperviseOptions, error) {
	fs := flag.NewFlagSet("transmission-supervise", flag.ContinueOnError)
	userName := fs.String("user", "", "以哪个真实用户的身份运行 Transmission（必填）")
	bin := fs.String("bin", "/opt/homebrew/bin/transmission-daemon", "transmission-daemon 可执行文件")
	configDir := fs.String("config-dir", "", "daemon 的配置目录（必填，settings.json 在这里）")
	logFile := fs.String("log", "", "daemon 的日志文件（必填，--logfile 用它）")
	home := fs.String("home", "", "真实用户家目录（必填，作为子进程 HOME）")
	if err := fs.Parse(args); err != nil {
		return transmissionSuperviseOptions{}, err
	}
	return transmissionSuperviseOptionsFrom(*userName, *bin, *configDir, *logFile, *home)
}

// cmdTransmissionSupervise 是 `zizpanel transmission-supervise` 的入口。
func cmdTransmissionSupervise(args []string) error {
	o, err := transmissionOptionsFromArgs(args)
	if err != nil {
		return err
	}
	// 真实部署是 root（LaunchDaemon）。允许"已经是目标用户"是用例：本机手工验收
	// （没有免密 sudo 的机器也能跑一遍），此时不需要也不能再 setuid。
	if os.Geteuid() != 0 && os.Geteuid() != o.UID {
		return fmt.Errorf("transmission-supervise 必须以 root（LaunchDaemon）或目标用户 %s 运行，当前 euid=%d",
			o.User, os.Geteuid())
	}
	return runTransmissionSupervisor(context.Background(), o)
}

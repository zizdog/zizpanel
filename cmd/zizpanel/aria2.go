// aria2（下载器）的常驻 supervisor（`zizpanel aria2-supervise`）。
//
// 它由**系统级 LaunchDaemon** com.zizdog.aria2 以 root 拉起，职责很小：
// fork 一个 `aria2c --conf-path=…` 子进程并保活（意外退出 → 1s/2s/5s 退避重启），
// 收到 SIGTERM/SIGINT 时**转发给 aria2 并等它退出**（aria2 要趁这时候把下载队列
// 写进会话文件，下次起来能续传），等不到再强杀。
//
// 为什么是"面板自己的子命令"（用户 2026-09-23 点名，与 zizvideo 同一套）：
// macOS 的 TCC 授权按 responsible process 的**代码要求**判定。这个 supervisor 与面板
// 同一代码要求（com.zizpanel.panel，固定自签身份）⇒ 面板在安装时拿到的那次
// 「完全磁盘访问权限」直接继承给 aria2，`dir=~/Downloads` 才写得进去。
//
// 直跑 `/opt/homebrew/bin/aria2c` 不行：它是 adhoc 签名、身份里带二进制哈希
// （aria2c-55554944…），每次 brew 升级都变，给它的授权就失效；而后台服务弹不出
// 授权框，系统会把 open() **挂住** —— 真机表现是"进程在跑、6800 在听、界面永远
// 连接中…"（2026-09-23 在 mini 上拿 sample 抓到的栈就卡在 BufferedFile 的 open 上）。
//
// 为什么以 root 跑 supervisor、以真实用户跑子进程：LaunchDaemon 在系统域，root 才能
// fork 后 setuid；下载文件必须属于用户（root 写出来的文件用户在 Finder 里都删不掉）。
// 降权用 SysProcAttr.Credential 直接 setuid/setgid，**不经 sudo**。
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

// aria2LookupUser 是用户查询的注入口（单测注入假用户，绝不依赖真机账号）。
var aria2LookupUser = user.Lookup

// aria2SuperviseOptions 是 supervisor 一次运行的全部输入（uid/gid 已解析）。
type aria2SuperviseOptions struct {
	User string
	UID  int
	GID  int
	Bin  string
	Conf string
	Root string
	Home string
}

// aria2SuperviseOptionsFrom 校验参数并解析真实用户的 uid/gid。
func aria2SuperviseOptionsFrom(userName, bin, conf, root, home string) (aria2SuperviseOptions, error) {
	o := aria2SuperviseOptions{}
	userName = strings.TrimSpace(userName)
	if userName == "" {
		return o, errors.New("缺少 --user：aria2 必须以真实用户身份运行，不能是 root")
	}
	u, err := aria2LookupUser(userName)
	if err != nil {
		return o, fmt.Errorf("查不到用户 %s：%w", userName, err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(u.Uid))
	if err != nil || uid <= 0 {
		return o, fmt.Errorf("用户 %s 的 uid 不可用（%q）：拒绝以 root/系统用户运行 aria2", userName, u.Uid)
	}
	gid, err := strconv.Atoi(strings.TrimSpace(u.Gid))
	if err != nil {
		return o, fmt.Errorf("用户 %s 的 gid 不可用（%q）", userName, u.Gid)
	}
	for _, kv := range []struct{ name, val string }{
		{"--bin", bin},
		{"--conf", conf},
		{"--root", root},
		{"--home", home},
	} {
		if !filepath.IsAbs(strings.TrimSpace(kv.val)) {
			return o, fmt.Errorf("%s 必须是绝对路径，实际 %q", kv.name, kv.val)
		}
	}
	o = aria2SuperviseOptions{
		User: userName,
		UID:  uid,
		GID:  gid,
		Bin:  filepath.Clean(strings.TrimSpace(bin)),
		Conf: filepath.Clean(strings.TrimSpace(conf)),
		Root: filepath.Clean(strings.TrimSpace(root)),
		Home: filepath.Clean(strings.TrimSpace(home)),
	}
	return o, nil
}

// aria2ChildCmd 返回**尚未启动**的 aria2c 子进程。
//
// 身份：SysProcAttr.Credential 直接 setuid/setgid 到真实用户（supervisor 是 root）。
// 环境只给用户自己的 HOME/USER/LOGNAME 与固定的 PATH（launchd 的默认环境里没有
// /opt/homebrew/bin，缺了它 aria2 里任何 exec 都会找不到东西）。
// **不启动**是为了让门禁能断言这份参数与身份。
func aria2ChildCmd(o aria2SuperviseOptions) *exec.Cmd {
	cmd := exec.Command(o.Bin, "--conf-path="+o.Conf)
	cmd.Dir = o.Root
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

// runAria2Supervisor 拉起并保活 aria2，直到收到 SIGTERM/SIGINT。
//
// 日志：子进程直接复用 supervisor 的 stdout/stderr（launchd 会把它们落到 plist 的
// StandardOutPath/StandardErrorPath），所以 aria2 的输出与 supervisor 自己的
// `[supervise]` 行落在同一份日志里 —— 排查时不用两头找。
func runAria2Supervisor(ctx context.Context, o aria2SuperviseOptions) error {
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s [supervise] "+format+"\n",
			append([]any{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
	}
	logf("supervisor 启动：以 %s（uid=%d gid=%d，HOME=%s）运行 %s --conf-path=%s",
		o.User, o.UID, o.GID, o.Home, o.Bin, o.Conf)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	backoffs := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		cmd := aria2ChildCmd(o)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		logf("启动 aria2：%s", strings.Join(cmd.Args, " "))
		start := time.Now()
		if err := cmd.Start(); err != nil {
			// 起不来（二进制不在、conf 不可读）也要继续退避重试：LaunchDaemon 的
			// KeepAlive 只管 supervisor 自己，supervisor 退出就没人再拉 aria2 了。
			logf("启动 aria2 失败：%v", err)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case s := <-sigCh:
				// 关键：先把信号交给 aria2，让它把会话写出去（下载队列才能续传）。
				logf("收到信号 %s，转发给 aria2 并等它退出", s)
				_ = cmd.Process.Signal(s)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					logf("aria2 10 秒内没有退出，强制结束")
					_ = cmd.Process.Kill()
					<-done
				}
				return nil
			case werr := <-done:
				if werr != nil {
					logf("aria2 退出：%v", werr)
				} else {
					logf("aria2 正常退出（exit 0）")
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
		logf("%s 后重启 aria2", delay)
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

// cmdAria2Supervise 是 `zizpanel aria2-supervise` 的入口。
func cmdAria2Supervise(args []string) error {
	fs := flag.NewFlagSet("aria2-supervise", flag.ContinueOnError)
	userName := fs.String("user", "", "以哪个真实用户的身份运行 aria2（必填）")
	bin := fs.String("bin", "/opt/homebrew/bin/aria2c", "aria2c 可执行文件")
	conf := fs.String("conf", "", "aria2 的配置文件（必填）")
	root := fs.String("root", "", "aria2 的安装/工作目录（必填，作为子进程的工作目录）")
	home := fs.String("home", "", "真实用户家目录（必填，作为子进程 HOME）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	o, err := aria2SuperviseOptionsFrom(*userName, *bin, *conf, *root, *home)
	if err != nil {
		return err
	}
	// 真实部署是 root（LaunchDaemon）。允许"已经是目标用户"是用例：本机手工验收
	// （没有免密 sudo 的机器也能跑一遍），此时不需要也不能再 setuid。
	if os.Geteuid() != 0 && os.Geteuid() != o.UID {
		return fmt.Errorf("aria2-supervise 必须以 root（LaunchDaemon）或目标用户 %s 运行，当前 euid=%d", o.User, os.Geteuid())
	}
	return runAria2Supervisor(context.Background(), o)
}

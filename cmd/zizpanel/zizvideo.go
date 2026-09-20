// zizvideo（短视频服务）的常驻 supervisor（`zizpanel zizvideo-supervise`）。
//
// 它由**系统级 LaunchDaemon** cn.zizpanel.zizvideo 以 root 拉起，职责很小：
// fork 一个 `zizvideo --config …` 子进程并保活（意外退出 → 1s/2s/5s 退避重启）。
//
// 为什么是"面板自己的子命令"：macOS 的 TCC 授权按 responsible process 的**代码
// 要求**判定。这个 supervisor 与面板**同一代码要求** ⇒ 面板的授权直接继承给
// zizvideo，不需要第二套授权、升级也不会失效（AGENTS 铁律 12、docs/坑清单.md 202）。
//
// 为什么以 root 跑 supervisor、以真实用户跑子进程：LaunchDaemon 在系统域，root 才能
// fork 后 setuid；zizvideo 写的是用户数据目录，以 root 跑产物会变成 root 所有。
// 降权用 SysProcAttr.Credential 直接 setuid/setgid，**不经 sudo**（理由同 macsaber）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// zizvideoLookupUser 是用户查询的注入口（单测注入假用户，绝不依赖真机账号）。
var zizvideoLookupUser = user.Lookup

// zizvideoSuperviseOptions 是 supervisor 一次运行的全部输入（uid/gid 已解析）。
type zizvideoSuperviseOptions struct {
	User       string
	UID        int
	GID        int
	Zizvideo   string
	ConfigPath string
	Home       string
	LogDir     string
}

// zizvideoSuperviseOptionsFrom 校验参数并解析真实用户的 uid/gid。
func zizvideoSuperviseOptionsFrom(userName, zizvideoBin, configPath, home, listen, logDir string) (zizvideoSuperviseOptions, error) {
	o := zizvideoSuperviseOptions{}
	userName = strings.TrimSpace(userName)
	if userName == "" {
		return o, errors.New("缺少 --user：zizvideo 必须以真实用户身份运行，不能是 root")
	}
	u, err := zizvideoLookupUser(userName)
	if err != nil {
		return o, fmt.Errorf("查不到用户 %s：%w", userName, err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(u.Uid))
	if err != nil || uid <= 0 {
		return o, fmt.Errorf("用户 %s 的 uid 不可用（%q）：拒绝以 root/系统用户运行 zizvideo", userName, u.Uid)
	}
	gid, err := strconv.Atoi(strings.TrimSpace(u.Gid))
	if err != nil {
		return o, fmt.Errorf("用户 %s 的 gid 不可用（%q）", userName, u.Gid)
	}
	for _, kv := range []struct{ name, val string }{
		{"--zizvideo", zizvideoBin},
		{"--config", configPath},
		{"--home", home},
	} {
		if !filepath.IsAbs(strings.TrimSpace(kv.val)) {
			return o, fmt.Errorf("%s 必须是绝对路径，实际 %q", kv.name, kv.val)
		}
	}
	if !zizvideoLoopbackListen(listen) {
		return o, fmt.Errorf("--listen 只允许回环地址（实际 %q）：这个界面能读写本机文件，不能裸奔", listen)
	}
	logDir = strings.TrimSpace(logDir)
	if logDir == "" {
		logDir = filepath.Join(strings.TrimSpace(home), "Library", "Logs")
	}
	if !filepath.IsAbs(logDir) {
		return o, fmt.Errorf("--log-dir 必须是绝对路径，实际 %q", logDir)
	}
	o = zizvideoSuperviseOptions{
		User:       userName,
		UID:        uid,
		GID:        gid,
		Zizvideo:   filepath.Clean(strings.TrimSpace(zizvideoBin)),
		ConfigPath: filepath.Clean(strings.TrimSpace(configPath)),
		Home:       filepath.Clean(strings.TrimSpace(home)),
		LogDir:     filepath.Clean(logDir),
	}
	return o, nil
}

// zizvideoLoopbackListen 判断监听地址是否只绑回环（127.0.0.1 / ::1 / localhost）。
func zizvideoLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// zizvideoChildCmd 返回**尚未启动**的 zizvideo 子进程。
//
// 身份：SysProcAttr.Credential 直接 setuid/setgid 到真实用户（supervisor 是 root）；
// 环境只给用户自己的 HOME/USER/LOGNAME/PATH。**不启动**是为了让门禁能断言这份参数与身份。
func zizvideoChildCmd(o zizvideoSuperviseOptions) *exec.Cmd {
	cmd := exec.Command(o.Zizvideo, services.ZizvideoServeArgs(o.ConfigPath)...)
	cmd.Dir = filepath.Dir(o.ConfigPath)
	cmd.Env = []string{
		"HOME=" + o.Home,
		"USER=" + o.User,
		"LOGNAME=" + o.User,
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}
	// 直接 fork+setuid，不经 sudo：理由见文件头（TCC 归属 + 少一个进程）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(o.UID),
		Gid: uint32(o.GID),
	}}
	return cmd
}

// zizvideoSuperviseRuntime 持有三个日志文件句柄。
type zizvideoSuperviseRuntime struct {
	childOut *os.File
	childErr *os.File
	logFile  *os.File
	logf     func(format string, args ...any)
}

func (rt *zizvideoSuperviseRuntime) close() {
	for _, f := range []*os.File{rt.childOut, rt.childErr, rt.logFile} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// openZizvideoLog 打开（或新建）一个日志文件，并把归属交还真实用户（同坑 163）。
func openZizvideoLog(path string, uid, gid int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		if cerr := f.Chown(uid, gid); cerr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("把日志 %s 交还用户失败：%w", path, cerr)
		}
	}
	return f, nil
}

// prepareZizvideoLogs 建好日志目录并打开子进程日志与 supervisor 日志。
func prepareZizvideoLogs(o zizvideoSuperviseOptions) (*zizvideoSuperviseRuntime, error) {
	if err := os.MkdirAll(o.LogDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录 %s 失败：%w", o.LogDir, err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(o.LogDir, o.UID, o.GID); err != nil {
			return nil, fmt.Errorf("把日志目录 %s 交还用户失败：%w", o.LogDir, err)
		}
	}
	childOut, err := openZizvideoLog(filepath.Join(o.LogDir, "zizvideo.out.log"), o.UID, o.GID)
	if err != nil {
		return nil, err
	}
	childErr, err := openZizvideoLog(filepath.Join(o.LogDir, "zizvideo.err.log"), o.UID, o.GID)
	if err != nil {
		_ = childOut.Close()
		return nil, err
	}
	logFile, err := openZizvideoLog(filepath.Join(o.LogDir, "zizvideo-supervise.log"), o.UID, o.GID)
	if err != nil {
		_ = childOut.Close()
		_ = childErr.Close()
		return nil, err
	}
	// launchd 按 plist 的 StandardOutPath/StandardErrorPath 先建了这两个文件（root 所有）；
	// 顺手 chown 一下让用户读得到，失败不影响运行。
	for _, p := range []string{
		filepath.Join(o.LogDir, "zizvideo-supervise.out.log"),
		filepath.Join(o.LogDir, "zizvideo-supervise.err.log"),
	} {
		if os.Geteuid() == 0 {
			_ = os.Chown(p, o.UID, o.GID)
		}
	}
	return &zizvideoSuperviseRuntime{
		childOut: childOut,
		childErr: childErr,
		logFile:  logFile,
		logf: func(format string, args ...any) {
			fmt.Fprintf(logFile, "%s "+format+"\n", append([]any{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
		},
	}, nil
}

// runZizvideoSupervisor 拉起并保活 zizvideo，直到收到 SIGTERM/SIGINT。
func runZizvideoSupervisor(ctx context.Context, o zizvideoSuperviseOptions) error {
	rt, err := prepareZizvideoLogs(o)
	if err != nil {
		return err
	}
	defer rt.close()

	rt.logf("supervisor 启动：以 %s（uid=%d gid=%d）运行 %s", o.User, o.UID, o.GID, o.Zizvideo)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	backoffs := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		cmd := zizvideoChildCmd(o)
		cmd.Stdout = rt.childOut
		cmd.Stderr = rt.childErr
		rt.logf("启动 zizvideo：%s", strings.Join(cmd.Args, " "))
		start := time.Now()
		startErr := cmd.Start()
		if startErr != nil {
			rt.logf("启动 zizvideo 失败：%v", startErr)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case s := <-sigCh:
				rt.logf("收到信号 %s，转发给 zizvideo 并等它退出", s)
				_ = cmd.Process.Signal(s)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					rt.logf("zizvideo 10 秒内没有退出，强制结束")
					_ = cmd.Process.Kill()
					<-done
				}
				return nil
			case werr := <-done:
				if werr != nil {
					rt.logf("zizvideo 退出：%v", werr)
				} else {
					rt.logf("zizvideo 正常退出（exit 0）")
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
		rt.logf("%s 后重启 zizvideo", delay)
		select {
		case s := <-sigCh:
			rt.logf("收到信号 %s，退出 supervisor", s)
			return nil
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// cmdZizvideoSupervise 是 `zizpanel zizvideo-supervise` 的入口。
func cmdZizvideoSupervise(args []string) error {
	fs := flag.NewFlagSet("zizvideo-supervise", flag.ContinueOnError)
	userName := fs.String("user", "", "以哪个真实用户的身份运行 zizvideo（必填）")
	zizvideoBin := fs.String("zizvideo", "/opt/zizvideo/bin/zizvideo", "zizvideo 可执行文件")
	configPath := fs.String("config", "", "zizvideo 的 config.json（必填，数据目录下）")
	home := fs.String("home", "", "真实用户家目录（必填，作为子进程 HOME）")
	listen := fs.String("listen", fmt.Sprintf("127.0.0.1:%d", services.ZizvideoPort), "监听地址（只允许回环）")
	logDir := fs.String("log-dir", "", "日志目录（默认 <home>/Library/Logs）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	o, err := zizvideoSuperviseOptionsFrom(*userName, *zizvideoBin, *configPath, *home, *listen, *logDir)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("zizvideo-supervise 必须以 root 运行（它要 fork 后 setuid 降权到 %s；由系统级 LaunchDaemon 拉起）", o.User)
	}
	return runZizvideoSupervisor(context.Background(), o)
}

// mac军刀（macsaber）的常驻 supervisor（`zizpanel macsaber-supervise`）。
//
// 它由**系统级 LaunchDaemon** cn.zizpanel.macsaber 以 root 拉起，职责很小：
// fork 一个 `macsaber serve` 子进程并保活（意外退出 → 1s/2s/5s 退避重启）。
//
// 为什么是"面板自己的子命令"而不是第二个可执行文件（本设计的核心）：
// macOS 的 TCC 授权按 responsible process 的**代码要求**判定。面板已用固定自签证书
// 签名（判据 identifier + certificate root，不含 cdhash），授权对它及其子进程生效；
// 这个 supervisor 与面板**同一代码要求** ⇒ 面板的授权直接继承给 macsaber，
// 不需要第二套授权、升级也不会失效（AGENTS.md 铁律 12、docs/坑清单.md 184/202）。
//
// 为什么以 root 跑 supervisor、以真实用户跑子进程：
//   - LaunchDaemon 在系统域，root 才能 fork 后 setuid 到真实用户；
//   - macsaber 写的是用户家目录与 ~/MacSaberFiles，以 root 跑产物会变成 root 所有，
//     用户反而拿不走；say/osascript/shortcuts/pbcopy 这些也要真实用户的图形会话环境。
//
// 降权用 Go 的 SysProcAttr.Credential 直接 setuid/setgid，**不经 sudo**：
// 中间多一个 setuid 的 sudo 进程会让 responsible process 的判定更容易被带偏，
// 直系父子继承面板授权更可靠，也少一个进程与一次 PATH/环境差异。
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

// macSaberLookupUser 是用户查询的注入口（单测注入假用户，绝不依赖真机账号）。
var macSaberLookupUser = user.Lookup

// macSaberSuperviseOptions 是 supervisor 一次运行的全部输入（uid/gid 已解析）。
type macSaberSuperviseOptions struct {
	User        string
	UID         int
	GID         int
	MacSaberBin string
	DataDir     string
	Listen      string
	ReadRoot    string
	WriteRoot   string
	LogDir      string
}

// macSaberSuperviseOptionsFrom 校验参数并解析真实用户的 uid/gid。
func macSaberSuperviseOptionsFrom(userName, macsaberBin, dataDir, listen, readRoot, writeRoot, logDir string) (macSaberSuperviseOptions, error) {
	o := macSaberSuperviseOptions{}
	userName = strings.TrimSpace(userName)
	if userName == "" {
		return o, errors.New("缺少 --user：macsaber 必须以真实用户身份运行，不能是 root")
	}
	u, err := macSaberLookupUser(userName)
	if err != nil {
		return o, fmt.Errorf("查不到用户 %s：%w", userName, err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(u.Uid))
	if err != nil || uid <= 0 {
		return o, fmt.Errorf("用户 %s 的 uid 不可用（%q）：拒绝以 root/系统用户运行 macsaber", userName, u.Uid)
	}
	gid, err := strconv.Atoi(strings.TrimSpace(u.Gid))
	if err != nil {
		return o, fmt.Errorf("用户 %s 的 gid 不可用（%q）", userName, u.Gid)
	}
	for _, kv := range []struct{ name, val string }{
		{"--macsaber", macsaberBin},
		{"--data", dataDir},
		{"--read-root", readRoot},
		{"--write-root", writeRoot},
	} {
		if !filepath.IsAbs(strings.TrimSpace(kv.val)) {
			return o, fmt.Errorf("%s 必须是绝对路径，实际 %q", kv.name, kv.val)
		}
	}
	if !macSaberLoopbackListen(listen) {
		return o, fmt.Errorf("--listen 只允许回环地址（实际 %q）：这个界面能读写本机文件，不能裸奔", listen)
	}
	logDir = strings.TrimSpace(logDir)
	if logDir == "" {
		logDir = filepath.Join(strings.TrimSpace(readRoot), "Library", "Logs")
	}
	if !filepath.IsAbs(logDir) {
		return o, fmt.Errorf("--log-dir 必须是绝对路径，实际 %q", logDir)
	}
	o = macSaberSuperviseOptions{
		User:        userName,
		UID:         uid,
		GID:         gid,
		MacSaberBin: filepath.Clean(strings.TrimSpace(macsaberBin)),
		DataDir:     filepath.Clean(strings.TrimSpace(dataDir)),
		Listen:      strings.TrimSpace(listen),
		ReadRoot:    filepath.Clean(strings.TrimSpace(readRoot)),
		WriteRoot:   filepath.Clean(strings.TrimSpace(writeRoot)),
		LogDir:      filepath.Clean(logDir),
	}
	return o, nil
}

// macSaberLoopbackListen 判断监听地址是否只绑回环（127.0.0.1 / ::1 / localhost）。
func macSaberLoopbackListen(addr string) bool {
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

// macSaberChildCmd 返回**尚未启动**的 macsaber 子进程。
//
// 身份：SysProcAttr.Credential 直接 setuid/setgid 到真实用户（supervisor 是 root）；
// 环境刻意只给用户自己的 HOME/USER/LOGNAME/PATH —— 图形工具要 HOME，
// 而 root 的整套环境不该漏给子进程。**不启动**是为了让门禁能断言这份参数与身份。
func macSaberChildCmd(o macSaberSuperviseOptions) *exec.Cmd {
	cmd := exec.Command(o.MacSaberBin, services.MacSaberServeArgs(o.DataDir, o.Listen, o.ReadRoot, o.WriteRoot)...)
	cmd.Dir = o.ReadRoot
	cmd.Env = []string{
		"HOME=" + o.ReadRoot,
		"USER=" + o.User,
		"LOGNAME=" + o.User,
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}
	// 直接 fork+setuid，不经 sudo：理由见文件头（TCC 归属 + 少一个进程）。
	// Groups 留空 = 清掉 root 的附加组，只保留主 gid（不泄漏 wheel）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(o.UID),
		Gid: uint32(o.GID),
	}}
	return cmd
}

// macSaberLogFunc 是 supervisor 的最少量日志输出。
type macSaberLogFunc func(format string, args ...any)

// macSaberSuperviseRuntime 持有三个日志文件句柄。
type macSaberSuperviseRuntime struct {
	childOut *os.File
	childErr *os.File
	logFile  *os.File
	logf     macSaberLogFunc
}

func (rt *macSaberSuperviseRuntime) close() {
	for _, f := range []*os.File{rt.childOut, rt.childErr, rt.logFile} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// openMacSaberLog 打开（或新建）一个日志文件，并把归属交还真实用户。
//
// 面板/守护进程以 root 往用户家目录写东西必须当场 chown：留着 root 所有的日志，
// 用户自己反而读不了（同坑 163 的归属纪律）。
func openMacSaberLog(path string, uid, gid int) (*os.File, error) {
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

// prepareMacSaberLogs 建好日志目录并打开子进程日志与 supervisor 日志。
func prepareMacSaberLogs(o macSaberSuperviseOptions) (*macSaberSuperviseRuntime, error) {
	if err := os.MkdirAll(o.LogDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录 %s 失败：%w", o.LogDir, err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(o.LogDir, o.UID, o.GID); err != nil {
			return nil, fmt.Errorf("把日志目录 %s 交还用户失败：%w", o.LogDir, err)
		}
	}
	childOut, err := openMacSaberLog(filepath.Join(o.LogDir, "macsaber.out.log"), o.UID, o.GID)
	if err != nil {
		return nil, err
	}
	childErr, err := openMacSaberLog(filepath.Join(o.LogDir, "macsaber.err.log"), o.UID, o.GID)
	if err != nil {
		_ = childOut.Close()
		return nil, err
	}
	logFile, err := openMacSaberLog(filepath.Join(o.LogDir, "macsaber-supervise.log"), o.UID, o.GID)
	if err != nil {
		_ = childOut.Close()
		_ = childErr.Close()
		return nil, err
	}
	// launchd 按 plist 的 StandardOutPath/StandardErrorPath 先建了这两个文件（root 所有）；
	// 顺手 chown 一下让用户读得到，失败不影响运行。
	for _, p := range []string{
		filepath.Join(o.LogDir, "macsaber-supervise.out.log"),
		filepath.Join(o.LogDir, "macsaber-supervise.err.log"),
	} {
		if os.Geteuid() == 0 {
			_ = os.Chown(p, o.UID, o.GID)
		}
	}
	return &macSaberSuperviseRuntime{
		childOut: childOut,
		childErr: childErr,
		logFile:  logFile,
		logf: func(format string, args ...any) {
			fmt.Fprintf(logFile, "%s "+format+"\n", append([]any{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
		},
	}, nil
}

// runMacSaberSupervisor 拉起并保活 macsaber，直到收到 SIGTERM/SIGINT。
func runMacSaberSupervisor(ctx context.Context, o macSaberSuperviseOptions) error {
	rt, err := prepareMacSaberLogs(o)
	if err != nil {
		return err
	}
	defer rt.close()

	rt.logf("supervisor 启动：以 %s（uid=%d gid=%d）运行 %s", o.User, o.UID, o.GID, o.MacSaberBin)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	backoffs := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		cmd := macSaberChildCmd(o)
		cmd.Stdout = rt.childOut
		cmd.Stderr = rt.childErr
		rt.logf("启动 macsaber：%s", strings.Join(cmd.Args, " "))
		start := time.Now()
		startErr := cmd.Start()
		if startErr != nil {
			rt.logf("启动 macsaber 失败：%v", startErr)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case s := <-sigCh:
				// 收到停止信号：转发给子进程并等它退出，别把它留在机器上。
				rt.logf("收到信号 %s，转发给 macsaber 并等它退出", s)
				_ = cmd.Process.Signal(s)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					rt.logf("macsaber 10 秒内没有退出，强制结束")
					_ = cmd.Process.Kill()
					<-done
				}
				return nil
			case werr := <-done:
				if werr != nil {
					rt.logf("macsaber 退出：%v", werr)
				} else {
					rt.logf("macsaber 正常退出（exit 0）")
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
		rt.logf("%s 后重启 macsaber", delay)
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

// cmdMacSaberSupervise 是 `zizpanel macsaber-supervise` 的入口。
func cmdMacSaberSupervise(args []string) error {
	fs := flag.NewFlagSet("macsaber-supervise", flag.ContinueOnError)
	userName := fs.String("user", "", "以哪个真实用户的身份运行 macsaber（必填）")
	macsaberBin := fs.String("macsaber", "/opt/macsaber/bin/macsaber", "macsaber 可执行文件")
	dataDir := fs.String("data", "", "macsaber 数据目录（必填）")
	listen := fs.String("listen", fmt.Sprintf("127.0.0.1:%d", services.MacSaberPort), "监听地址（只允许回环）")
	readRoot := fs.String("read-root", "", "可读根（真实用户家目录，必填）")
	writeRoot := fs.String("write-root", "", "可写根（必填）")
	logDir := fs.String("log-dir", "", "日志目录（默认 <read-root>/Library/Logs）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	o, err := macSaberSuperviseOptionsFrom(*userName, *macsaberBin, *dataDir, *listen, *readRoot, *writeRoot, *logDir)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("macsaber-supervise 必须以 root 运行（它要 fork 后 setuid 降权到 %s；由系统级 LaunchDaemon 拉起）", o.User)
	}
	return runMacSaberSupervisor(context.Background(), o)
}

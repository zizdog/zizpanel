// Jellyfin（媒体服务器）的常驻 supervisor（`zizpanel jellyfin-supervise`）。
//
// 它由**系统级 LaunchDaemon** com.zizdog.jellyfin 以 root 拉起，职责很小：
// fork 一个 `jellyfin --datadir … --configdir … --cachedir … --logdir … --ffmpeg … --webdir …`
// 子进程并保活（意外退出 → 1s/2s/5s 退避重启）。
//
// 为什么是"面板自己的子命令"（用户 2026-09-29 拍板 A 方案）：macOS 的 TCC 授权按
// responsible process 的**代码要求**判定。这个 supervisor 与面板**同一代码要求**
// ⇒ 面板已获得的授权（外接盘 / 可移除宗卷）直接继承给 Jellyfin，不需要第二套授权、
// 升级也不会失效（AGENTS 铁律、docs/坑清单.md 202）。
//
// 为什么以 root 跑 supervisor、以真实用户跑子进程：LaunchDaemon 在系统域，root 才能
// fork 后 setuid；Jellyfin 写的是用户数据目录与媒体库元数据，以 root 跑产物会变成
// root 所有。降权用 SysProcAttr.Credential 直接 setuid/setgid，**不经 sudo**。
//
// ⚠️ 参数是**冻结契约**：plist 由 internal/services/jellyfin.go 的
// JellyfinSuperviseArgs 生成，子进程参数由 services.JellyfinServeArgs 生成，
// 两边必须逐字一致（咬合门禁在 cmd/zizpanel/supervise_contract_test.go）。
// 改任一 flag 名 = 破坏兼容，必须同时改生成侧与门禁。
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

	"github.com/zizdog/zizpanel/internal/services"
)

// jellyfinLookupUser 是用户查询的注入口（单测注入假用户，绝不依赖真机账号）。
var jellyfinLookupUser = user.Lookup

// jellyfinSuperviseOptions 是 supervisor 一次运行的全部输入（uid/gid 已解析）。
type jellyfinSuperviseOptions struct {
	User string
	UID  int
	GID  int
	// Jellyfin 是服务端可执行文件（<安装根>/apps/jellyfin/current/jellyfin）。
	Jellyfin string
	// DataDir / ConfigDir / CacheDir / LogDir 是交给 Jellyfin 的四个目录。
	DataDir   string
	ConfigDir string
	CacheDir  string
	LogDir    string
	// FFmpeg 是外部 ffmpeg（面板基础依赖）；同时用它的目录拼子进程 PATH。
	FFmpeg string
	// WebDir 是网页界面资源目录（<版本目录>/jellyfin-web）。
	WebDir string
}

// jellyfinSuperviseOptionsFrom 校验参数并解析真实用户的 uid/gid。
func jellyfinSuperviseOptionsFrom(userName, jellyfinBin, dataDir, configDir, cacheDir, logDir, ffmpeg, webDir string) (jellyfinSuperviseOptions, error) {
	o := jellyfinSuperviseOptions{}
	userName = strings.TrimSpace(userName)
	if userName == "" {
		return o, errors.New("缺少 --user：Jellyfin 必须以真实用户身份运行，不能是 root")
	}
	u, err := jellyfinLookupUser(userName)
	if err != nil {
		return o, fmt.Errorf("查不到用户 %s：%w", userName, err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(u.Uid))
	if err != nil || uid <= 0 {
		return o, fmt.Errorf("用户 %s 的 uid 不可用（%q）：拒绝以 root/系统用户运行 Jellyfin", userName, u.Uid)
	}
	gid, err := strconv.Atoi(strings.TrimSpace(u.Gid))
	if err != nil {
		return o, fmt.Errorf("用户 %s 的 gid 不可用（%q）", userName, u.Gid)
	}
	for _, kv := range []struct{ name, val string }{
		{"--jellyfin", jellyfinBin},
		{"--datadir", dataDir},
		{"--configdir", configDir},
		{"--cachedir", cacheDir},
		{"--logdir", logDir},
		{"--ffmpeg", ffmpeg},
	} {
		if !filepath.IsAbs(strings.TrimSpace(kv.val)) {
			return o, fmt.Errorf("%s 必须是绝对路径，实际 %q", kv.name, kv.val)
		}
	}
	webDir = strings.TrimSpace(webDir)
	if webDir != "" && !filepath.IsAbs(webDir) {
		return o, fmt.Errorf("--webdir 必须是绝对路径，实际 %q", webDir)
	}
	return jellyfinSuperviseOptions{
		User:      userName,
		UID:       uid,
		GID:       gid,
		Jellyfin:  filepath.Clean(strings.TrimSpace(jellyfinBin)),
		DataDir:   filepath.Clean(strings.TrimSpace(dataDir)),
		ConfigDir: filepath.Clean(strings.TrimSpace(configDir)),
		CacheDir:  filepath.Clean(strings.TrimSpace(cacheDir)),
		LogDir:    filepath.Clean(strings.TrimSpace(logDir)),
		FFmpeg:    filepath.Clean(strings.TrimSpace(ffmpeg)),
		WebDir:    filepath.Clean(webDir),
	}, nil
}

// jellyfinChildCmd 返回**尚未启动**的 Jellyfin 子进程。
//
// 子进程参数来自 services.JellyfinServeArgs（与 plist 生成侧同源）。
// 身份：SysProcAttr.Credential 直接 setuid/setgid 到真实用户（supervisor 是 root）；
// 环境只给用户自己的 HOME/USER/LOGNAME 与含 ffmpeg 目录的 PATH。**不启动**是为了
// 让门禁能断言这份参数与身份。
func jellyfinChildCmd(o jellyfinSuperviseOptions) *exec.Cmd {
	cmd := exec.Command(o.Jellyfin,
		services.JellyfinServeArgs(o.DataDir, o.ConfigDir, o.CacheDir, o.LogDir, o.FFmpeg, o.WebDir)...)
	cmd.Dir = filepath.Dir(o.Jellyfin)
	path := "/usr/bin:/bin:/usr/sbin:/sbin"
	if d := filepath.Dir(o.FFmpeg); d != "" && d != "." && d != "/" {
		path = d + ":" + path
	}
	cmd.Env = []string{
		"HOME=" + userHomeOf(o),
		"USER=" + o.User,
		"LOGNAME=" + o.User,
		"PATH=" + path,
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(o.UID),
		Gid: uint32(o.GID),
	}}
	return cmd
}

// userHomeOf 取真实用户家目录（查不到就退回 /Users/<name>，与目录约定一致）。
func userHomeOf(o jellyfinSuperviseOptions) string {
	if u, err := jellyfinLookupUser(o.User); err == nil && strings.TrimSpace(u.HomeDir) != "" {
		return u.HomeDir
	}
	return "/Users/" + o.User
}

// jellyfinSuperviseRuntime 持有日志文件句柄与日志函数。
type jellyfinSuperviseRuntime struct {
	childOut *os.File
	childErr *os.File
	logFile  *os.File
	logf     func(format string, args ...any)
}

func (rt *jellyfinSuperviseRuntime) close() {
	for _, f := range []*os.File{rt.childOut, rt.childErr, rt.logFile} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// openJellyfinLog 打开（或新建）一个日志文件，并把归属交还真实用户（坑 163）。
func openJellyfinLog(path string, uid, gid int) (*os.File, error) {
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

// prepareJellyfinLogs 建好日志目录并打开子进程日志与 supervisor 日志。
//
// launchd 按 plist 的 StandardOutPath/StandardErrorPath 先建了 supervisor 自己的
// 两个日志（root 所有），这里顺手 chown 让用户读得到（失败不影响运行）。
func prepareJellyfinLogs(o jellyfinSuperviseOptions) (*jellyfinSuperviseRuntime, error) {
	if err := os.MkdirAll(o.LogDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录 %s 失败：%w", o.LogDir, err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(o.LogDir, o.UID, o.GID); err != nil {
			return nil, fmt.Errorf("把日志目录 %s 交还用户失败：%w", o.LogDir, err)
		}
	}
	childOut, err := openJellyfinLog(filepath.Join(o.LogDir, "jellyfin.out.log"), o.UID, o.GID)
	if err != nil {
		return nil, err
	}
	childErr, err := openJellyfinLog(filepath.Join(o.LogDir, "jellyfin.err.log"), o.UID, o.GID)
	if err != nil {
		_ = childOut.Close()
		return nil, err
	}
	logFile, err := openJellyfinLog(filepath.Join(o.LogDir, "jellyfin-supervise.log"), o.UID, o.GID)
	if err != nil {
		_ = childOut.Close()
		_ = childErr.Close()
		return nil, err
	}
	for _, p := range []string{
		filepath.Join(o.LogDir, "jellyfin-supervise.out.log"),
		filepath.Join(o.LogDir, "jellyfin-supervise.err.log"),
	} {
		if os.Geteuid() == 0 {
			_ = os.Chown(p, o.UID, o.GID)
		}
	}
	return &jellyfinSuperviseRuntime{
		childOut: childOut,
		childErr: childErr,
		logFile:  logFile,
		logf: func(format string, args ...any) {
			fmt.Fprintf(logFile, "%s "+format+"\n", append([]any{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
		},
	}, nil
}

// runJellyfinSupervisor 拉起并保活 Jellyfin，直到收到 SIGTERM/SIGINT。
func runJellyfinSupervisor(ctx context.Context, o jellyfinSuperviseOptions) error {
	rt, err := prepareJellyfinLogs(o)
	if err != nil {
		return err
	}
	defer rt.close()

	rt.logf("supervisor 启动：以 %s（uid=%d gid=%d）运行 %s", o.User, o.UID, o.GID, o.Jellyfin)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	backoffs := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		cmd := jellyfinChildCmd(o)
		cmd.Stdout = rt.childOut
		cmd.Stderr = rt.childErr
		rt.logf("启动 Jellyfin：%s", strings.Join(cmd.Args, " "))
		start := time.Now()
		startErr := cmd.Start()
		if startErr != nil {
			rt.logf("启动 Jellyfin 失败：%v", startErr)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case s := <-sigCh:
				rt.logf("收到信号 %s，转发给 Jellyfin 并等它退出", s)
				_ = cmd.Process.Signal(s)
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					rt.logf("Jellyfin 15 秒内没有退出，强制结束")
					_ = cmd.Process.Kill()
					<-done
				}
				return nil
			case werr := <-done:
				if werr != nil {
					rt.logf("Jellyfin 退出：%v", werr)
				} else {
					rt.logf("Jellyfin 正常退出（exit 0）")
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
		rt.logf("%s 后重启 Jellyfin", delay)
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

// jellyfinOptionsFromArgs 解析 supervisor 的 argv（供入口与"参数咬合"门禁共用）。
func jellyfinOptionsFromArgs(args []string) (jellyfinSuperviseOptions, error) {
	fs := flag.NewFlagSet("jellyfin-supervise", flag.ContinueOnError)
	userName := fs.String("user", "", "以哪个真实用户的身份运行 Jellyfin（必填）")
	jellyfinBin := fs.String("jellyfin", "", "Jellyfin 服务端二进制（必填，绝对路径）")
	dataDir := fs.String("datadir", "", "数据目录（必填，绝对路径）")
	configDir := fs.String("configdir", "", "配置目录（必填，绝对路径）")
	cacheDir := fs.String("cachedir", "", "缓存目录（必填，绝对路径）")
	logDir := fs.String("logdir", "", "日志目录（必填，绝对路径）")
	ffmpeg := fs.String("ffmpeg", "", "外部 ffmpeg（必填，绝对路径；它的目录会进子进程 PATH）")
	webDir := fs.String("webdir", "", "网页界面资源目录（可选；空 = Jellyfin 自己找）")
	if err := fs.Parse(args); err != nil {
		return jellyfinSuperviseOptions{}, err
	}
	return jellyfinSuperviseOptionsFrom(*userName, *jellyfinBin, *dataDir, *configDir,
		*cacheDir, *logDir, *ffmpeg, *webDir)
}

// cmdJellyfinSupervise 是 `zizpanel jellyfin-supervise` 的入口。
func cmdJellyfinSupervise(args []string) error {
	o, err := jellyfinOptionsFromArgs(args)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("jellyfin-supervise 必须以 root 运行（它要 fork 后 setuid 降权到 %s；由系统级 LaunchDaemon 拉起）", o.User)
	}
	return runJellyfinSupervisor(context.Background(), o)
}

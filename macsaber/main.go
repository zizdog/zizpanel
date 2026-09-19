// mac军刀（macsaber）：macOS 原生能力的薄壳 + 胶水，Go 单二进制 Web 服务。
//
// 用法：
//
//	macsaber serve --data <dir> --listen 127.0.0.1:8895 [--read-root P]... [--write-root P]...
//	macsaber version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/macsaber/internal/config"
	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
	"github.com/zizdog/macsaber/internal/tools"
	"github.com/zizdog/macsaber/internal/web"
)

// version 是产品版本；构建时可用 -ldflags "-X main.version=..." 覆盖。
var version = "0.1.0"

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "version", "-v", "--version":
		fmt.Printf("mac军刀 macsaber %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mac军刀 macsaber —— macOS 原生小工具箱

用法:
  macsaber serve [选项]     启动本地 Web 服务
  macsaber version          打印版本

serve 选项:
  --data <dir>              数据目录（默认 ~/Library/Application Support/macsaber）
  --listen <host:port>      监听地址（默认 127.0.0.1:8895，只允许本机）
  --read-root <path>        可读根，可重复（默认 $HOME）
  --write-root <path>       可写根，可重复（默认 ~/MacSaberFiles，首次启动自动创建）
  --log-level <level>       debug|info|warn|error（默认 info）
`)
}

func defaultHome() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

func runServe(args []string) int {
	fset := flag.NewFlagSet("serve", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	dataDir := fset.String("data", config.DefaultDataDir(), "数据目录")
	listen := fset.String("listen", "127.0.0.1:8895", "监听地址")
	logLevel := fset.String("log-level", "info", "日志级别")
	var readRoots, writeRoots multiFlag
	fset.Var(&readRoots, "read-root", "可读根，可重复")
	fset.Var(&writeRoots, "write-root", "可写根，可重复")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	log := newLogger(*logLevel)
	// 只监听回环地址：对外暴露是用户自己的事（反代），默认绝不裸奔。
	if !isLoopback(*listen) {
		log.warn("监听地址 %s 不是回环地址，请确认这是你要的", *listen)
	}

	if len(readRoots) == 0 {
		readRoots = multiFlag{defaultHome()}
	}
	if len(writeRoots) == 0 {
		writeRoots = multiFlag{config.DefaultWriteRoot()}
	}
	for _, w := range writeRoots {
		if err := os.MkdirAll(w, 0o700); err != nil {
			log.err("创建可写根失败 %s: %v", w, err)
			return 1
		}
	}

	store, err := config.Open(*dataDir)
	if err != nil {
		log.err("打开数据目录失败: %v", err)
		return 1
	}
	guard, err := fsroot.New(readRoots, writeRoots)
	if err != nil {
		log.err("路径闸门初始化失败: %v", err)
		return 1
	}
	audit, err := tool.OpenAudit(filepath.Join(store.DataDir(), "audit.log"))
	if err != nil {
		log.err("打开审计日志失败: %v", err)
		return 1
	}
	defer func() { _ = audit.Close() }()

	ex := execx.New()
	probes := execx.NewProbeCache()
	tm := tasks.NewManager()
	reg := tool.NewRegistry()
	tools.RegisterAll(reg)

	runner := &tool.Runner{
		Reg: reg, Tasks: tm, Audit: audit,
		Ctx: &tool.Ctx{
			Ctx: context.Background(), Guard: guard, Exec: ex, Probes: probes,
			TempDir: os.TempDir(),
			Log:     func(string) {}, Progress: func(int, string) {},
		},
		Logger: log.warn,
	}
	srv := web.New(store, guard, reg, runner, tm, version)
	srv.Logf = log.info

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.err("监听 %s 失败: %v", *listen, err)
		return 1
	}
	log.info("mac军刀 %s 已启动：http://%s", version, ln.Addr())
	log.info("数据目录 %s", store.DataDir())
	log.info("审计日志 %s", audit.Path())
	log.info("可读根 %s", strings.Join(guard.ReadRoots(), ", "))
	log.info("可写根 %s", strings.Join(guard.WriteRoots(), ", "))
	if !store.Inited() {
		log.info("尚未初始化：请在浏览器打开上面的地址设置用户名与口令")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		log.err("服务异常退出: %v", err)
		return 1
	case sig := <-stop:
		log.info("收到 %s，正在关闭…", sig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	return 0
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// logger 是最小日志器：级别过滤，输出到 stderr（launchd 收进标准错误文件）。
type logger struct {
	level int
}

const (
	lvlDebug = iota
	lvlInfo
	lvlWarn
	lvlError
)

func newLogger(level string) *logger {
	l := lvlInfo
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		l = lvlDebug
	case "warn", "warning":
		l = lvlWarn
	case "error":
		l = lvlError
	}
	return &logger{level: l}
}

func (l *logger) write(lv int, tag, format string, args ...any) {
	if lv < l.level {
		return
	}
	fmt.Fprintf(os.Stderr, "%s [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), tag, fmt.Sprintf(format, args...))
}

func (l *logger) debug(format string, args ...any) { l.write(lvlDebug, "debug", format, args...) }
func (l *logger) info(format string, args ...any)  { l.write(lvlInfo, "info", format, args...) }
func (l *logger) warn(format string, args ...any)  { l.write(lvlWarn, "warn", format, args...) }
func (l *logger) err(format string, args ...any)   { l.write(lvlError, "error", format, args...) }

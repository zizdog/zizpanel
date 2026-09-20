// Command zizvideo serves a local short-video library over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zizdog/zizvideo/internal/api"
	"github.com/zizdog/zizvideo/internal/auth"
	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/ffmpeg"
	"github.com/zizdog/zizvideo/internal/media"
	"github.com/zizdog/zizvideo/internal/storage"
	"github.com/zizdog/zizvideo/internal/task"
	"github.com/zizdog/zizvideo/internal/web"
)

func main() {
	args := os.Args[1:]
	// The verb may follow --config, so index it explicitly.
	if i := indexVerb(args, "roots"); i >= 0 {
		if err := runRoots(append(append([]string{}, args[:i]...), args[i+1:]...)); err != nil {
			fmt.Fprintln(os.Stderr, "zizvideo:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "zizvideo:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "配置文件路径 (JSON)")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Usage = printUsage
	flag.Parse()
	if *showVersion {
		fmt.Println("zizvideo", api.Version)
		return nil
	}

	cfg, used, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	if err := os.MkdirAll(cfg.CoversDir(), 0o700); err != nil {
		return fmt.Errorf("创建封面目录失败: %w", err)
	}

	db, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PurgeExpiredSessions(); err != nil {
		logger.Warn("清理过期会话失败", "error", err.Error())
	}

	secret, err := auth.LoadSecret(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("加载会话密钥失败: %w", err)
	}
	authMgr := auth.NewManager(db, secret, cfg.SessionTTL(), cfg.LockoutThreshold, cfg.LockoutWindow())

	runner := ffmpeg.ExecRunner{}
	roots := config.NewRoots(used, cfg.MediaAllowRoots)
	scanner := media.NewScanner(cfg, db, roots, runner, logger)
	tasks := task.NewManager(cfg, db, roots, scanner, logger)
	tasks.Recover()
	defer tasks.Stop()

	srv := api.NewServer(cfg, db, authMgr, tasks, roots, runner, logger)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.Router(srv),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Range streaming must not be capped by a global write deadline.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	caps := srv.Capabilities()
	logger.Info("服务启动",
		"version", api.Version,
		"listen", cfg.Listen,
		"data_dir", cfg.DataDir,
		"config", orNone(used),
		"allow_roots_count", len(roots.List()),
		"scan_workers", cfg.ScanWorkers,
		"ffmpeg_ok", caps.FFmpegOK,
		"ffprobe_ok", caps.FFprobeOK,
		"videotoolbox_h264", caps.VideoToolboxH264)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		return fmt.Errorf("监听 %s 失败: %w", cfg.Listen, err)
	case <-ctx.Done():
	}

	logger.Info("收到退出信号，开始收尾")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP 收尾超时", "error", err.Error())
	}
	tasks.Stop()
	logger.Info("已停止")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// indexVerb finds a bare subcommand while skipping flag values.
func indexVerb(args []string, verb string) int {
	skipNext := false
	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case arg == "--":
			return -1
		case arg == "-config" || arg == "--config":
			skipNext = true
		case arg == verb:
			return i
		}
	}
	return -1
}

func printUsage() {
	fmt.Fprint(os.Stderr, `zizvideo — 本地短视频服务

用法:
  zizvideo [--config <file>] [--version]
  zizvideo roots list|add <绝对路径>|remove <绝对路径> [--config <file>]

root 子命令直接读写同一份 config.json，输出一行 JSON。`)
	fmt.Fprintln(os.Stderr)
}

func orNone(s string) string {
	if s == "" {
		return "(默认配置)"
	}
	return s
}

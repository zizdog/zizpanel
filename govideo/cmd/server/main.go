// Command govideo serves a local short-video library over HTTP.
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

	"github.com/zizdog/govideo/internal/api"
	"github.com/zizdog/govideo/internal/auth"
	"github.com/zizdog/govideo/internal/config"
	"github.com/zizdog/govideo/internal/ffmpeg"
	"github.com/zizdog/govideo/internal/media"
	"github.com/zizdog/govideo/internal/storage"
	"github.com/zizdog/govideo/internal/task"
	"github.com/zizdog/govideo/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "govideo:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "配置文件路径 (JSON)")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println("govideo", api.Version)
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
	scanner := media.NewScanner(cfg, db, runner, logger)
	tasks := task.NewManager(cfg, db, scanner, logger)
	tasks.Recover()
	defer tasks.Stop()

	srv := api.NewServer(cfg, db, authMgr, tasks, runner, logger)

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
		"allow_roots_count", len(cfg.MediaAllowRoots),
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

func orNone(s string) string {
	if s == "" {
		return "(默认配置)"
	}
	return s
}

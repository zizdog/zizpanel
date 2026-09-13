// Package logx 提供面板统一日志。
//
// 同时写文件（按天滚动）和 stderr，满足 launchd 抓取。
// 格式：[2006-01-02 15:04:05] [LEVEL] [模块] 消息
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	}
	return "?"
}

var (
	mu       sync.Mutex
	logFile  *os.File
	logDir   string
	curDate  string
	minLevel = LevelInfo
)

// Init 初始化日志：logDir 下的 panel-YYYYMMDD.log，同时输出到 stderr。
func Init(dir string, level Level) error {
	mu.Lock()
	defer mu.Unlock()
	minLevel = level
	logDir = dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := rotateLocked(); err != nil {
		return err
	}
	log.SetFlags(0)
	log.SetOutput(io.MultiWriter(os.Stderr, &fileWriter{}))
	return nil
}

// fileWriter 在每次写入时检查日期，实现按天滚动。
type fileWriter struct{}

func (w *fileWriter) Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := rotateLocked(); err != nil {
		return len(p), nil // 日志失败不能影响主流程
	}
	if logFile == nil {
		return len(p), nil
	}
	return logFile.Write(p)
}

func rotateLocked() error {
	today := time.Now().Format("20060102")
	if logFile != nil && curDate == today {
		return nil
	}
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	curDate = today
	path := filepath.Join(logDir, "panel-"+today+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	logFile = f
	// 顺手清理 30 天前的日志
	cleanupOld(logDir, 30)
	return nil
}

func cleanupOld(dir string, keepDays int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < len("panel-20060102.log") || name[:6] != "panel-" {
			continue
		}
		datePart := name[6:min(14, len(name))]
		t, err := time.ParseInLocation("20060102", datePart, time.Local)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func output(level Level, module, format string, args ...any) {
	if level < minLevel {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	log.Printf("[%s] [%s] [%s] %s", ts, level, module, msg)
}

// Logger 是带模块名的日志器。
type Logger struct{ module string }

// New 返回指定模块的日志器。
func New(module string) *Logger { return &Logger{module: module} }

func (l *Logger) Debug(format string, args ...any) { output(LevelDebug, l.module, format, args...) }
func (l *Logger) Info(format string, args ...any)  { output(LevelInfo, l.module, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { output(LevelWarn, l.module, format, args...) }
func (l *Logger) Error(format string, args ...any) { output(LevelError, l.module, format, args...) }

// ParseLevel 解析字符串日志级别。
func ParseLevel(s string) Level {
	switch s {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

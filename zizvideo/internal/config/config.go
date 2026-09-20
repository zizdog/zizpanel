// Package config loads JSON config, applies ZV_ environment overrides, validates.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the whole runtime configuration.
type Config struct {
	Listen             string   `json:"listen"`
	DataDir            string   `json:"data_dir"`
	DatabasePath       string   `json:"database_path"`
	MediaAllowRoots    []string `json:"media_allow_roots"`
	MediaExtensions    []string `json:"media_extensions"`
	ScanWorkers        int      `json:"scan_workers"`
	ScanDeleteRatio    float64  `json:"scan_delete_threshold_ratio"`
	ScanDeleteCount    int      `json:"scan_delete_threshold_count"`
	ScanRetryBackoffMS []int    `json:"scan_retry_backoff_ms"`
	ProbeTimeoutSec    int      `json:"probe_timeout_seconds"`
	CoverQuality       int      `json:"cover_jpeg_quality"`
	FFmpegBin          string   `json:"ffmpeg_bin"`
	FFprobeBin         string   `json:"ffprobe_bin"`
	LogLevel           string   `json:"log_level"`
	TrustedProxies     []string `json:"trusted_proxies"`
	SessionTTLHours    int      `json:"session_ttl_hours"`
	LockoutThreshold   int      `json:"lockout_threshold"`
	LockoutWindowMin   int      `json:"lockout_window_minutes"`
	SecureCookie       bool     `json:"secure_cookie"`
}

// Default returns the built-in defaults; allow roots default to $HOME/Movies
// and $HOME/Downloads, matching the documented MVP behaviour.
func Default() *Config {
	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, "Library", "Application Support", "zizvideo")
	return &Config{
		Listen:             "127.0.0.1:7766",
		DataDir:            dataDir,
		DatabasePath:       filepath.Join(dataDir, "zizvideo.db"),
		MediaAllowRoots:    []string{filepath.Join(home, "Movies"), filepath.Join(home, "Downloads")},
		MediaExtensions:    []string{"mp4", "mov", "m4v", "mkv", "webm", "ts", "mts", "avi", "flv", "hevc"},
		ScanWorkers:        4,
		ScanDeleteRatio:    0.10,
		ScanDeleteCount:    50,
		ScanRetryBackoffMS: []int{1000, 4000, 16000},
		ProbeTimeoutSec:    30,
		CoverQuality:       3,
		FFmpegBin:          "ffmpeg",
		FFprobeBin:         "ffprobe",
		LogLevel:           "info",
		SessionTTLHours:    336,
		LockoutThreshold:   5,
		LockoutWindowMin:   15,
	}
}

// Load reads the JSON file at path (missing file is not an error), then applies
// ZV_ overrides, then validates. Returns the config and the path actually used.
func Load(path string) (*Config, string, error) {
	c := Default()
	used := ""
	if path == "" {
		if p := os.Getenv("ZV_CONFIG"); p != "" {
			path = p
		}
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, path, fmt.Errorf("读取配置失败: %w", err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(c); err != nil {
			return nil, path, fmt.Errorf("解析配置失败: %w", err)
		}
		used = path
	}
	applyEnv(c)
	if err := c.Validate(); err != nil {
		return nil, used, err
	}
	return c, used, nil
}

// setAllowRoots mirrors the store's snapshot for tests and CLI helpers; the
// running code reads config.Roots instead (坑 1：只有一个真源).
func (c *Config) setAllowRoots(roots []string) {
	if c == nil {
		return
	}
	c.MediaAllowRoots = make([]string, len(roots))
	copy(c.MediaAllowRoots, roots)
}

func applyEnv(c *Config) {
	setStr(&c.Listen, "ZV_LISTEN")
	setStr(&c.DataDir, "ZV_DATA_DIR")
	setStr(&c.DatabasePath, "ZV_DATABASE_PATH")
	setStr(&c.FFmpegBin, "ZV_FFMPEG_BIN")
	setStr(&c.FFprobeBin, "ZV_FFPROBE_BIN")
	setStr(&c.LogLevel, "ZV_LOG_LEVEL")
	setInt(&c.ScanWorkers, "ZV_SCAN_WORKERS")
	setInt(&c.ProbeTimeoutSec, "ZV_PROBE_TIMEOUT_SECONDS")
	setInt(&c.LockoutThreshold, "ZV_LOCKOUT_THRESHOLD")
	setInt(&c.SessionTTLHours, "ZV_SESSION_TTL_HOURS")
	setBool(&c.SecureCookie, "ZV_SECURE_COOKIE")
	if v := os.Getenv("ZV_MEDIA_ALLOW_ROOTS"); v != "" {
		c.MediaAllowRoots = splitList(v)
	}
	if v := os.Getenv("ZV_MEDIA_EXTENSIONS"); v != "" {
		c.MediaExtensions = splitList(v)
	}
	if v := os.Getenv("ZV_TRUSTED_PROXIES"); v != "" {
		c.TrustedProxies = splitList(v)
	}
	if c.DatabasePath == "" || os.Getenv("ZV_DATA_DIR") != "" && os.Getenv("ZV_DATABASE_PATH") == "" {
		c.DatabasePath = filepath.Join(c.DataDir, "zizvideo.db")
	}
}

func setStr(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func setInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func setBool(dst *bool, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v == "1" || strings.EqualFold(v, "true")
	}
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate rejects configurations that would make the service unsafe or broken.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen 不能为空")
	}
	if c.ScanWorkers < 1 || c.ScanWorkers > 16 {
		return fmt.Errorf("scan_workers 必须在 1-16 之间")
	}
	if c.DataDir == "" || !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data_dir 必须是绝对路径")
	}
	if c.DatabasePath == "" || !filepath.IsAbs(c.DatabasePath) {
		return fmt.Errorf("database_path 必须是绝对路径")
	}
	if len(c.MediaAllowRoots) == 0 {
		return fmt.Errorf("media_allow_roots 不能为空")
	}
	for _, r := range c.MediaAllowRoots {
		if !filepath.IsAbs(r) {
			return fmt.Errorf("media_allow_roots 必须是绝对路径: %s", r)
		}
	}
	if len(c.MediaExtensions) == 0 {
		return fmt.Errorf("media_extensions 不能为空")
	}
	if c.ProbeTimeoutSec <= 0 {
		return fmt.Errorf("probe_timeout_seconds 必须为正")
	}
	if c.CoverQuality < 2 || c.CoverQuality > 31 {
		return fmt.Errorf("cover_jpeg_quality 必须在 2-31 之间")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level 只能是 debug/info/warn/error")
	}
	return nil
}

// ProbeTimeout returns the ffprobe/ffmpeg timeout as a duration.
func (c *Config) ProbeTimeout() time.Duration {
	return time.Duration(c.ProbeTimeoutSec) * time.Second
}

// SessionTTL returns the session lifetime.
func (c *Config) SessionTTL() time.Duration {
	return time.Duration(c.SessionTTLHours) * time.Hour
}

// LockoutWindow returns how long an account stays locked after too many failures.
func (c *Config) LockoutWindow() time.Duration {
	return time.Duration(c.LockoutWindowMin) * time.Minute
}

// CoversDir is where extracted JPEG covers live.
func (c *Config) CoversDir() string { return filepath.Join(c.DataDir, "covers") }

// ExtAllowed reports whether a lowercase extension is in the whitelist.
func (c *Config) ExtAllowed(ext string) bool {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	for _, e := range c.MediaExtensions {
		if strings.EqualFold(e, ext) {
			return true
		}
	}
	return false
}

// AllowRoot resolves an allow root through symlinks so that macOS aliases such
// as /var -> /private/var compare equal on both sides.
func AllowRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

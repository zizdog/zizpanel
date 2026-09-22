// Package config loads JSON config, applies ZV_ environment overrides, validates.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// AllowRegister 默认关：关闭时唯一公开注册入口 POST /auth/register 直接拒绝。
	AllowRegister bool `json:"allow_register"`
}

// Default returns the built-in defaults; the only allow root is $HOME/Movies
// (a system folder on every Mac), so nothing points at an external volume.
func Default() *Config {
	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, "Library", "Application Support", "zizvideo")
	return &Config{
		Listen:             "127.0.0.1:7766",
		DataDir:            dataDir,
		DatabasePath:       filepath.Join(dataDir, "zizvideo.db"),
		MediaAllowRoots:    []string{filepath.Join(home, "Movies")},
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
		AllowRegister:      false,
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
	setBool(&c.AllowRegister, "ZV_ALLOW_REGISTER")
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

// ============================================================================
//  allow_register live switch（条目 8）
// ============================================================================

// RegisterSwitch is the live allow_register value. Writes go to config.json and
// are only adopted after a successful read-back, so "保存成功" 永远等于"已生效"
// （坑 164）。读不到就由 State() 如实标未复核。
type RegisterSwitch struct {
	mu    sync.Mutex
	fpath string
	on    atomic.Bool
}

// NewRegisterSwitch builds the switch; ZV_ALLOW_REGISTER wins over the file.
func NewRegisterSwitch(fpath string, initial bool) *RegisterSwitch {
	r := &RegisterSwitch{fpath: fpath}
	if v, ok := os.LookupEnv("ZV_ALLOW_REGISTER"); ok && v != "" {
		r.on.Store(v == "1" || strings.EqualFold(v, "true"))
	} else {
		r.on.Store(initial)
	}
	return r
}

// On reports the value currently in effect.
func (r *RegisterSwitch) On() bool { return r.on.Load() }

// Path is the config file this switch persists to; "" means no file.
func (r *RegisterSwitch) Path() string { return r.fpath }

// FileBacked reports whether a write can be persisted at all.
func (r *RegisterSwitch) FileBacked() bool { return r.fpath != "" }

// EnvOverridden reports whether ZV_ALLOW_REGISTER replaces the file value.
func (r *RegisterSwitch) EnvOverridden() bool {
	v, ok := os.LookupEnv("ZV_ALLOW_REGISTER")
	return ok && v != ""
}

// RegisterState is what the admin page reads back: effective value plus whether
// it could actually be re-read from config.json.
type RegisterState struct {
	AllowRegister bool   `json:"allow_register"`
	Verified      bool   `json:"verified"`
	Source        string `json:"source"`
	FileValue     *bool  `json:"file_value"`
	ConfigPath    string `json:"config_path"`
	EnvOverride   bool   `json:"env_override"`
	Note          string `json:"note,omitempty"`
}

// State re-reads the config file and compares it with the live value. A missing
// or unreadable file yields verified=false with an honest note.
func (r *RegisterSwitch) State() RegisterState {
	st := RegisterState{AllowRegister: r.On(), ConfigPath: r.fpath, EnvOverride: r.EnvOverridden()}
	if st.EnvOverride {
		st.Source, st.Verified = "env", true
		st.Note = "ZV_ALLOW_REGISTER 已覆盖配置文件"
		return st
	}
	if r.fpath == "" {
		st.Source = "default"
		st.Note = "没有配置文件，无法回读生效值"
		return st
	}
	v, err := RegisterFromFile(r.fpath)
	if err != nil {
		st.Source = "unreadable"
		st.Note = "读取配置文件失败: " + err.Error()
		return st
	}
	st.Source = "config"
	st.FileValue = &v
	if v == st.AllowRegister {
		st.Verified = true
	} else {
		st.Note = "配置文件值与进程内生效值不一致（需重启）"
	}
	return st
}

// Set writes allow_register, re-reads it, and only adopts the value when the
// read-back matches. Env override / missing file are refused with a reason.
func (r *RegisterSwitch) Set(on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.EnvOverridden() {
		return fmt.Errorf("ZV_ALLOW_REGISTER 已覆盖，配置文件不会生效")
	}
	if r.fpath == "" {
		return fmt.Errorf("没有配置文件，无法写入（用 --config 或 ZV_CONFIG 指定）")
	}
	raw, err := readConfigMap(r.fpath)
	if err != nil {
		return err
	}
	raw["allow_register"] = json.RawMessage(strconv.FormatBool(on))
	if err := writeAtomic(r.fpath, raw); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	back, err := RegisterFromFile(r.fpath)
	if err != nil {
		return fmt.Errorf("回读配置失败: %w", err)
	}
	if back != on {
		return fmt.Errorf("回读配置与写入不一致，未生效")
	}
	r.on.Store(on)
	return nil
}

// RegisterFromFile reads allow_register from config.json; a missing key means
// the documented default (false).
func RegisterFromFile(path string) (bool, error) {
	raw, err := readConfigMap(path)
	if err != nil {
		return false, err
	}
	v, ok := raw["allow_register"]
	if !ok {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false, fmt.Errorf("allow_register 必须是布尔值")
	}
	return b, nil
}

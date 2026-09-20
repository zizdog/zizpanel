package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsMatchDocumentedMVP(t *testing.T) {
	c := Default()
	if c.Listen != "127.0.0.1:7766" {
		t.Fatalf("listen = %q", c.Listen)
	}
	if c.ScanWorkers != 4 {
		t.Fatalf("scan_workers = %d", c.ScanWorkers)
	}
	if len(c.MediaAllowRoots) != 2 {
		t.Fatalf("默认白名单 = %v", c.MediaAllowRoots)
	}
	for _, root := range c.MediaAllowRoots {
		if !filepath.IsAbs(root) {
			t.Fatalf("默认白名单不是绝对路径: %s", root)
		}
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(c.MediaAllowRoots[0], home) {
		t.Fatalf("默认白名单未落在用户家目录: %v", c.MediaAllowRoots)
	}
	for _, ext := range []string{"mp4", "mov", "mkv", "webm", "hevc"} {
		if !c.ExtAllowed("." + ext) {
			t.Fatalf("默认扩展名缺少 %s", ext)
		}
	}
	if c.ExtAllowed("txt") {
		t.Fatal("txt 不应在白名单内")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	t.Setenv("ZV_LISTEN", "127.0.0.1:17766")
	t.Setenv("ZV_SCAN_WORKERS", "7")
	t.Setenv("ZV_MEDIA_ALLOW_ROOTS", "/tmp/a, /tmp/b")
	t.Setenv("ZV_MEDIA_EXTENSIONS", "mp4,mkv")
	t.Setenv("ZV_LOG_LEVEL", "debug")

	c, used, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if used != "" {
		t.Fatalf("未指定配置文件时 used 应为空, 得到 %q", used)
	}
	if c.Listen != "127.0.0.1:17766" || c.ScanWorkers != 7 || c.LogLevel != "debug" {
		t.Fatalf("环境变量未生效: %+v", c)
	}
	if len(c.MediaAllowRoots) != 2 || c.MediaAllowRoots[1] != "/tmp/b" {
		t.Fatalf("白名单 = %v", c.MediaAllowRoots)
	}
	if c.ExtAllowed("mov") {
		t.Fatal("扩展名覆盖未生效")
	}
}

func TestLoadFromFileAndRejectUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"listen":"127.0.0.1:18888","scan_workers":2}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, used, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if used != path || c.Listen != "127.0.0.1:18888" || c.ScanWorkers != 2 {
		t.Fatalf("配置未生效: %+v", c)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"unknown_key":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(bad); err == nil {
		t.Fatal("未知字段必须报错，避免配置被静默忽略")
	}
}

func TestValidateRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"workers too low", func(c *Config) { c.ScanWorkers = 0 }},
		{"workers too high", func(c *Config) { c.ScanWorkers = 32 }},
		{"relative data dir", func(c *Config) { c.DataDir = "data" }},
		{"relative db path", func(c *Config) { c.DatabasePath = "db.sqlite" }},
		{"empty allow roots", func(c *Config) { c.MediaAllowRoots = nil }},
		{"relative allow root", func(c *Config) { c.MediaAllowRoots = []string{"media"} }},
		{"empty extensions", func(c *Config) { c.MediaExtensions = nil }},
		{"bad log level", func(c *Config) { c.LogLevel = "verbose" }},
		{"bad probe timeout", func(c *Config) { c.ProbeTimeoutSec = 0 }},
		{"bad cover quality", func(c *Config) { c.CoverQuality = 99 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mut(c)
			if err := c.Validate(); err == nil {
				t.Fatal("非法配置必须被拒绝")
			}
		})
	}
}

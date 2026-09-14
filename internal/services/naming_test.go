package services

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizpanel/internal/store"
)

// ============================================================================
//  第 3 项：服务管理要显示软件名，而不是 launchd 标签
// ============================================================================

func TestFriendlyName(t *testing.T) {
	cases := map[string]string{
		// 用户实际看到的那些（本机 Homebrew 用的是 sh.brew. 前缀）
		"sh.brew.mysql@8.4":   "MySQL 8.4",
		"sh.brew.php@8.3":     "PHP 8.3",
		"homebrew.mxcl.nginx": "Nginx",
		"homebrew.mxcl.httpd": "Apache httpd",
		"com.zizdog.qwen3tts": "Qwen3 TTS",
		"com.zizdog.iopaint":  "IOPaint",
		// 版本号要保留
		"sh.brew.redis@7.2":    "Redis 7.2",
		"homebrew.mxcl.colima": "Colima",
		// 不认识的一律原样返回 —— 猜错比不猜更糟
		"com.apple.something": "com.apple.something",
		"my-own-service":      "my-own-service",
		"":                    "",
		"sh.brew.node@20":     "Node.js 20",
	}
	for in, want := range cases {
		if got := FriendlyName(in); got != want {
			t.Errorf("FriendlyName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestDisplayNameKeepsCustomNames 用户手写的名字不能被自动映射覆盖。
func TestDisplayNameKeepsCustomNames(t *testing.T) {
	custom := &Service{Name: "docker-runtime", LaunchLabel: "com.zizdog.colima",
		DisplayName: "Docker 运行时（Colima）"}
	if got := displayNameOf(custom); got != "Docker 运行时（Colima）" {
		t.Errorf("手写名被改掉了: %q", got)
	}

	// 名字与标签相同时才替换
	ugly := &Service{Name: "sh-brew-mysql8-4", LaunchLabel: "sh.brew.mysql@8.4",
		DisplayName: "sh.brew.mysql@8.4"}
	if got := displayNameOf(ugly); got != "MySQL 8.4" {
		t.Errorf("标签式显示名应被替换成友好名，实际 %q", got)
	}

	// display_name 为空时也要兜底
	empty := &Service{Name: "x", LaunchLabel: "sh.brew.php@8.3"}
	if got := displayNameOf(empty); got != "PHP 8.3" {
		t.Errorf("空显示名应兜底成友好名，实际 %q", got)
	}
}

// ============================================================================
//  第 5 项：brew 服务的真实 launchd 标签必须按磁盘上的 plist 推
// ============================================================================

func TestBrewLabelFor(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"sh.brew.php@8.3", "homebrew.mxcl.nginx", "my.custom.redis"} {
		if err := os.WriteFile(filepath.Join(agents, n+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := map[string]string{
		"php@8.3": "sh.brew.php@8.3",     // 本机是 sh.brew. 前缀
		"nginx":   "homebrew.mxcl.nginx", // 标准前缀
		"redis":   "my.custom.redis",     // 兜底：任意前缀，按 .<formula>.plist 匹配
		"nope":    "",                    // 找不到就返回空，交给调用方回退
		"":        "",
	}
	for formula, want := range cases {
		if got := BrewLabelFor(home, formula); got != want {
			t.Errorf("BrewLabelFor(%q) = %q，期望 %q", formula, got, want)
		}
	}

	// 关键回归：绝不能凭空造出 homebrew.mxcl.<formula>。
	// 用户就是因为这个报"找不到 homebrew.mxcl.php@8.3 的 plist"。
	if got := BrewLabelFor(home, "nope"); got == "homebrew.mxcl.nope" {
		t.Error("找不到 plist 时不该硬编码 homebrew.mxcl.* —— 那正是用户遇到的报错来源")
	}
}

// ============================================================================
//  第 6 项：孤儿态识别（安装产物还在，但服务没注册）
// ============================================================================

func TestInstallerArtifactExists(t *testing.T) {
	home := t.TempDir()

	// 什么都没装
	for _, id := range []string{"iopaint", "qwen3tts", "voicereceiver", "unknown-app"} {
		if InstallerArtifactExists(home, id) {
			t.Errorf("%s 没装却报告有安装产物", id)
		}
	}

	// 造出三个应用的产物
	mk := func(rel string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("iopaint/.venv/bin/iopaint")
	mk("tts/qwen3/.venv/bin/python")
	mk("tts/voice-receiver/receiver.py")

	for _, id := range []string{"iopaint", "qwen3tts", "voicereceiver"} {
		if !InstallerArtifactExists(home, id) {
			t.Errorf("%s 明明装了却报告没有安装产物（会导致市场显示「安装」，用户重装已有的东西）", id)
		}
	}
}

// TestSelfLabelExcludesPanelCron 面板自己的作业不能出现在可纳管列表里。
//
// 真机上"扫描可纳管服务"把 cn.zizpanel.cron.daily-backup 列了出来 ——
// 那是面板自己的计划任务（有专门页面管理），纳管进来毫无意义。
func TestSelfLabelExcludesPanelCron(t *testing.T) {
	for _, l := range []string{"cn.zizpanel.cron.daily-backup", "cn.zizpanel.panel",
		"cn.zizpanel.anything"} {
		if !isSelfLabel(l) {
			t.Errorf("%s 应被排除（面板自身的作业）", l)
		}
	}
	for _, l := range []string{"sh.brew.php@8.3", "com.zizdog.qwen3tts", "com.vix.cron"} {
		if isSelfLabel(l) {
			t.Errorf("%s 不该被当成面板自身作业", l)
		}
	}
}

// TestRepositoryAppliesFriendlyNameOnRead 是第 3 项的集成断言：
// 已经登记过的老记录（display_name 存的就是 launchd 标签）**不用重新纳管**，
// 从注册表读出来时就应该是友好名。
func TestRepositoryAppliesFriendlyNameOnRead(t *testing.T) {
	repo := newTestRepo(t)
	// 模拟一条老记录：display_name 与标签完全相同
	if err := repo.Create(t.Context(), &Service{
		Name: "sh-brew-mysql8-4", DisplayName: "sh.brew.mysql@8.4",
		Kind: KindNative, LaunchLabel: "sh.brew.mysql@8.4", Category: "lnmp",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(t.Context(), "sh-brew-mysql8-4")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "MySQL 8.4" {
		t.Errorf("读取时应把标签式显示名换成友好名，实际 %q", got.DisplayName)
	}

	// 用户手写的名字要原样保留
	if err := repo.Create(t.Context(), &Service{
		Name: "docker-runtime", DisplayName: "Docker 运行时（Colima）",
		Kind: KindColima, LaunchLabel: "com.zizdog.colima", Category: "tool",
	}); err != nil {
		t.Fatal(err)
	}
	got2, err := repo.Get(t.Context(), "docker-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if got2.DisplayName != "Docker 运行时（Colima）" {
		t.Errorf("手写显示名被覆盖了: %q", got2.DisplayName)
	}
}

// newTestRepo 建一个用临时 SQLite 的注册表。
func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewRepository(st)
}

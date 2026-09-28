package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  Jellyfin 安装器门禁（一条，含 5 个子用例，每个子用例都能被变异弄红）
//
//  为什么现有门禁抓不到：
//   · 市场反漂移只比对声明与目录字段，Jellyfin 的产物事实（文件名/sha256/镜像路径）
//     原先没有任何判据 —— 所以补在 MarketDeclarationProblems 的 release_binary 分支里；
//   · .tar.xz 解包 / current 链接 / "面板二进制而不是 jellyfin 本体"这几件事
//     在仓库里是**第一份**（此前没有任何应用用 .tar.xz + 版本目录 + supervisor），
//     没有既有门禁能覆盖；
//   · 媒体目录的同身份试读也是第一份。
//
//  单测只用 t.TempDir() + 注入的假取件/假解包/假服务，不联网、不落真盘、不碰 launchd。
// ============================================================================

// jellyfinFakeEnv 造一个隔离环境并注入全部假实现；返回恢复函数。
type jellyfinFakeEnv struct {
	m        *Manager
	root     string
	upCalls  int
	downCall int
}

func jellyfinFakeEnvSetup(t *testing.T) *jellyfinFakeEnv {
	t.Helper()
	m, _ := sandboxManager(t)
	// 镜像必须"已配置"，否则安装第一步就按"没有公网源"拒绝。
	m.opt.MirrorBase = "https://mirror.example.com:8888"

	env := &jellyfinFakeEnv{m: m, root: filepath.Join(t.TempDir(), "apps", "jellyfin")}

	prevRoot := JellyfinInstallRoot
	JellyfinInstallRoot = env.root
	prevLaunchd := SystemLaunchDaemonsDir
	SystemLaunchDaemonsDir = t.TempDir()
	prevFetch, prevExtract := jellyfinFetch, jellyfinExtract
	prevProbe, prevDeps := jellyfinProbe, jellyfinEnsureDeps
	prevSHA, prevFFmpeg := jellyfinExpectedSHA256, jellyfinFfmpegPath
	prevPanel, prevUp, prevDown := jellyfinPanelExecutable, jellyfinServiceUp, jellyfinServiceDown
	prevRead := jellyfinReadProbe

	t.Cleanup(func() {
		JellyfinInstallRoot = prevRoot
		SystemLaunchDaemonsDir = prevLaunchd
		jellyfinFetch, jellyfinExtract = prevFetch, prevExtract
		jellyfinProbe, jellyfinEnsureDeps = prevProbe, prevDeps
		jellyfinExpectedSHA256, jellyfinFfmpegPath = prevSHA, prevFFmpeg
		jellyfinPanelExecutable, jellyfinServiceUp, jellyfinServiceDown = prevPanel, prevUp, prevDown
		jellyfinReadProbe = prevRead
	})

	// 默认假实现：镜像探测与依赖补齐都是空操作；ffmpeg 指向一个假的绝对路径。
	jellyfinProbe = func(*Manager, context.Context, string) error { return nil }
	jellyfinEnsureDeps = func(*Manager, context.Context, *InstallResult) error { return nil }
	jellyfinFfmpegPath = func() string { return "/opt/homebrew/bin/ffmpeg" }
	jellyfinPanelExecutable = func() (string, error) { return "/opt/zizpanel/bin/zizpanel", nil }
	jellyfinServiceUp = func(*Manager, context.Context, JellyfinPaths, string, *InstallResult) error {
		env.upCalls++
		return nil
	}
	jellyfinServiceDown = func(*Manager, context.Context, JellyfinPaths, *InstallResult) error {
		env.downCall++
		return nil
	}
	return env
}

// jellyfinFakeFetch 写一份"假应用包"（内容可控）。
func jellyfinFakeFetch(content string) func(*Manager, context.Context, string, string, *InstallResult) error {
	return func(_ *Manager, _ context.Context, _, dst string, _ *InstallResult) error {
		return os.WriteFile(dst, []byte(content), 0o644)
	}
}

// jellyfinFakeExtract 造出解包后的目录结构（二进制故意不给可执行位，逼安装器 chmod）。
func jellyfinFakeExtract(t *testing.T) func(*Manager, context.Context, string, string) error {
	t.Helper()
	return func(_ *Manager, _ context.Context, _, dest string) error {
		if err := os.MkdirAll(filepath.Join(dest, "jellyfin-web"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dest, "jellyfin-web", "index.html"), []byte("<html></html>"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dest, "jellyfin"), []byte("#!/bin/sh\nexit 0\n"), 0o644)
	}
}

func sha256HexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestJellyfinGate 是唯一的新门禁（5 个子用例）。
func TestJellyfinGate(t *testing.T) {
	app, ok := FindApp(JellyfinAppID)
	if !ok {
		t.Fatal("目录里没有 jellyfin 条目")
	}
	ctx := context.Background()

	// ---- ① 校验和不符 ⇒ 安装失败且什么都没装 ----
	t.Run("校验和不符则中止且什么都不装", func(t *testing.T) {
		env := jellyfinFakeEnvSetup(t)
		jellyfinFetch = jellyfinFakeFetch("tampered-archive")
		jellyfinExtract = jellyfinFakeExtract(t)
		// 期望值与内容不符（模拟镜像上的包被换掉/传坏）。
		jellyfinExpectedSHA256 = func() string { return strings.Repeat("a", 64) }

		res := &InstallResult{App: app.ID, Steps: []string{}}
		err := env.m.InstallJellyfin(ctx, app, res)
		if err == nil {
			t.Fatal("sha256 不匹配必须中止安装，实际返回成功（谎报）")
		}
		if !strings.Contains(err.Error(), "sha256 校验不通过") {
			t.Errorf("错误信息要说清是 sha256 校验失败：%v", err)
		}
		if dirExists(env.m.jellyfinPaths().VersionDir) || fileExists(env.m.jellyfinPaths().Current) {
			t.Errorf("校验失败后安装目录必须为空，实际留下 %s", env.root)
		}
		if env.upCalls != 0 {
			t.Errorf("校验失败就不该去注册/启动服务，实际调用了 %d 次", env.upCalls)
		}
	})

	// ---- ② 正常路径 ⇒ 解包目录结构 + 二进制可执行位 + current 链接 ----
	t.Run("正常路径的目录结构与可执行位", func(t *testing.T) {
		env := jellyfinFakeEnvSetup(t)
		content := "fake-jellyfin-archive-bytes"
		jellyfinFetch = jellyfinFakeFetch(content)
		jellyfinExtract = jellyfinFakeExtract(t)
		jellyfinExpectedSHA256 = func() string { return sha256HexOf(content) }

		res := &InstallResult{App: app.ID, Steps: []string{}}
		if err := env.m.InstallJellyfin(ctx, app, res); err != nil {
			t.Fatalf("正常路径不该失败：%v", err)
		}
		p := env.m.jellyfinPaths()
		if !dirExists(filepath.Join(p.VersionDir, "jellyfin-web")) {
			t.Errorf("解包后应有 jellyfin-web/ 目录：%s", p.VersionDir)
		}
		if !fileExecutable(filepath.Join(p.VersionDir, "jellyfin")) {
			t.Errorf("解包后的二进制必须带可执行位：%s", filepath.Join(p.VersionDir, "jellyfin"))
		}
		target, lerr := os.Readlink(p.Current)
		if lerr != nil {
			t.Fatalf("current 必须是符号链接：%v", lerr)
		}
		if target != JellyfinVersionDir {
			t.Errorf("current 应指向 %q，实际 %q", JellyfinVersionDir, target)
		}
		if !fileExecutable(p.Bin) {
			t.Errorf("通过 current 应能执行到二进制：%s", p.Bin)
		}
		if env.upCalls != 1 {
			t.Errorf("安装成功必须注册一次服务，实际 %d 次", env.upCalls)
		}
	})

	// ---- ③ plist 必须执行**面板二进制**，不是 jellyfin 本体 ----
	t.Run("plist执行面板二进制而不是jellyfin本体", func(t *testing.T) {
		env := jellyfinFakeEnvSetup(t)
		p := env.m.jellyfinPaths()
		const panelBin = "/opt/zizpanel/bin/zizpanel"
		const ffmpeg = "/opt/homebrew/bin/ffmpeg"

		args := JellyfinSuperviseArgs(panelBin, "zizdog", p, ffmpeg)
		if args[0] != panelBin {
			t.Fatalf("ProgramArguments[0] 必须是面板二进制 %q，实际 %q（运行身份方案 A 的前提）", panelBin, args[0])
		}
		if args[1] != "jellyfin-supervise" {
			t.Fatalf("ProgramArguments[1] 必须是子命令 jellyfin-supervise，实际 %q", args[1])
		}
		if args[0] == p.Bin || strings.Contains(args[0], "jellyfin/current") {
			t.Fatalf("不许直接执行 jellyfin 本体（那样拿不到面板的 TCC 授权）：%v", args)
		}
		content := jellyfinPlistContent(JellyfinLabel, args, p.SuperviseOutLog, p.SuperviseErrLog)
		// 取 ProgramArguments 数组里的第一个 <string>：必须是面板二进制。
		arrayStart := strings.Index(content, "<key>ProgramArguments</key>")
		if arrayStart < 0 {
			t.Fatal("plist 里没有 ProgramArguments")
		}
		first := content[arrayStart:]
		i := strings.Index(first, "<string>")
		j := strings.Index(first, "</string>")
		if i < 0 || j < 0 {
			t.Fatalf("ProgramArguments 里没有 <string>：\n%s", content)
		}
		if got := first[i+len("<string>") : j]; got != panelBin {
			t.Fatalf("plist 的第一个参数必须是面板二进制 %q，实际 %q", panelBin, got)
		}
		// 运行身份不再写 UserName（supervisor 是 root，fork 后 setuid）。
		if strings.Contains(content, "<key>UserName</key>") {
			t.Error("plist 不该写 UserName（作业必须以 root 跑 supervisor，降权在 fork 后做）")
		}
		// 冻结契约：目录与 ffmpeg 都要传给 supervisor。
		for _, want := range []string{"--user", "--jellyfin", "--datadir", "--configdir",
			"--cachedir", "--logdir", "--ffmpeg", "--webdir", p.Bin, p.DataDir, ffmpeg, p.WebDir} {
			if !strings.Contains(content, want) {
				t.Errorf("plist 缺少冻结契约里的 %q", want)
			}
		}
	})

	// ---- ④ 卸载 ⇒ 停服务 + 删安装目录，但用户数据保留 ----
	t.Run("卸载删安装目录但保留用户数据", func(t *testing.T) {
		env := jellyfinFakeEnvSetup(t)
		p := env.m.jellyfinPaths()
		// 造出"装好了"的磁盘状态 + 用户数据里的一个文件。
		if err := os.MkdirAll(p.VersionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(p.UserDataDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dataFile := filepath.Join(p.UserDataDir, "config", "system.xml")
		if err := os.MkdirAll(filepath.Dir(dataFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dataFile, []byte("<config/>"), 0o644); err != nil {
			t.Fatal(err)
		}

		res := &InstallResult{App: app.ID, Steps: []string{}}
		if err := env.m.UninstallJellyfin(ctx, app, false, false, res); err != nil {
			t.Fatalf("卸载失败：%v", err)
		}
		if env.downCall != 1 {
			t.Errorf("卸载必须先停服务，实际 down 调用 %d 次", env.downCall)
		}
		if dirExists(env.root) {
			t.Errorf("卸载后安装目录必须删掉：%s", env.root)
		}
		if !fileExists(dataFile) {
			t.Errorf("默认卸载**必须保留**用户数据：%s 没了", dataFile)
		}
	})

	// ---- ⑤ 媒体目录：试读失败必须如实提示 + 给可执行出路 ----
	t.Run("媒体目录试读失败要如实提示并给出路", func(t *testing.T) {
		env := jellyfinFakeEnvSetup(t)
		dir := t.TempDir()
		movie := filepath.Join(dir, "movie.mp4")
		if err := os.WriteFile(movie, []byte("not-a-real-video"), 0o644); err != nil {
			t.Fatal(err)
		}

		// 失败：以真实用户身份读被 TCC 挡住。
		jellyfinReadProbe = func(*Manager, context.Context, string) (string, error) {
			return "", errors.New("head: /Volumes/Media/movie.mp4: Operation not permitted")
		}
		c := env.m.JellyfinMediaReadCheck(ctx, dir)
		if c.OK {
			t.Fatal("读不到却报 OK（谎报）")
		}
		if !strings.Contains(c.Message, "读不到") {
			t.Errorf("失败提示要说清读不到：%q", c.Message)
		}
		for _, want := range []string{"完全磁盘访问", "内建盘", "挂载"} {
			if !strings.Contains(c.Remedy, want) {
				t.Errorf("出路里必须提到 %q，实际：%q", want, c.Remedy)
			}
		}

		// 空目录：如实说"会当空库"，不能报可读。
		empty := t.TempDir()
		if e := env.m.JellyfinMediaReadCheck(ctx, empty); e.OK || !strings.Contains(e.Message, "空库") {
			t.Errorf("空目录应如实报空库，实际 ok=%v message=%q", e.OK, e.Message)
		}

		// 成功：真的读到了 1 字节。
		jellyfinReadProbe = func(*Manager, context.Context, string) (string, error) { return "x", nil }
		if s := env.m.JellyfinMediaReadCheck(ctx, dir); !s.OK || !strings.Contains(s.Message, "已读到") {
			t.Errorf("可读时必须报成功，实际 ok=%v message=%q", s.OK, s.Message)
		}
	})
}

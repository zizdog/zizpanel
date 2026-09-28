package main

// jellyfin_test.go —— Jellyfin supervisor 的 cmd 侧门禁。
//
// 判据是"构造出来的子进程"而不是真跑一个进程：
//   - 面板写进 plist 的参数必须被 supervisor 原样接受（参数咬合）；
//   - 子进程身份必须是 SysProcAttr.Credential 直接 setuid/setgid 到真实用户（不经 sudo）；
//   - 子进程参数必须与 services.JellyfinServeArgs 同源。
//
// 单测绝不真起 Jellyfin、绝不碰真实 launchd/家目录（AGENTS 第三节）。

import (
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// stubJellyfinUser 注入一个假用户查询（绝不依赖真机账号）。
func stubJellyfinUser(t *testing.T, uid, gid string) {
	t.Helper()
	prev := jellyfinLookupUser
	jellyfinLookupUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, Uid: uid, Gid: gid, HomeDir: "/Users/" + name}, nil
	}
	t.Cleanup(func() { jellyfinLookupUser = prev })
}

// testJellyfinPaths 造一份目录约定（家目录是临时目录）。
func testJellyfinPaths(t *testing.T) services.JellyfinPaths {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "apps", "jellyfin")
	p := services.JellyfinPaths{
		Home:            home,
		Root:            root,
		VersionDir:      filepath.Join(root, "v12.1"),
		Current:         filepath.Join(root, "current"),
		Bin:             filepath.Join(root, "current", "jellyfin"),
		WebDir:          filepath.Join(root, "current", "jellyfin-web"),
		UserDataDir:     filepath.Join(home, "Library", "Application Support", "jellyfin"),
		DataDir:         filepath.Join(home, "Library", "Application Support", "jellyfin", "data"),
		ConfigDir:       filepath.Join(home, "Library", "Application Support", "jellyfin", "config"),
		CacheDir:        filepath.Join(home, "Library", "Application Support", "jellyfin", "cache"),
		LogDir:          filepath.Join(home, "Library", "Application Support", "jellyfin", "log"),
		SuperviseOutLog: filepath.Join(home, "Library", "Application Support", "jellyfin", "log", "jellyfin-supervise.out.log"),
		SuperviseErrLog: filepath.Join(home, "Library", "Application Support", "jellyfin", "log", "jellyfin-supervise.err.log"),
		Plist:           "/Library/LaunchDaemons/com.zizdog.jellyfin.plist",
	}
	return p
}

// TestJellyfinSuperviseArgsFromPanelAreAccepted：面板写进 plist 的那串参数，
// supervisor 必须原样接受（参数是冻结契约，改一边不改另一边会让服务起不来）。
func TestJellyfinSuperviseArgsFromPanelAreAccepted(t *testing.T) {
	stubJellyfinUser(t, "501", "20")
	p := testJellyfinPaths(t)
	const panelBin = "/opt/zizpanel/bin/zizpanel"
	const ffmpeg = "/opt/homebrew/bin/ffmpeg"

	args := services.JellyfinSuperviseArgs(panelBin, "zizdog", p, ffmpeg)
	if args[1] != "jellyfin-supervise" {
		t.Fatalf("参数形状变了：%v", args)
	}
	o, err := jellyfinOptionsFromArgs(args[2:])
	if err != nil {
		t.Fatalf("面板生成的参数被 supervisor 自己拒了：%v\n参数：%v", err, args)
	}
	if o.User != "zizdog" || o.UID != 501 || o.GID != 20 {
		t.Errorf("身份解析不对：%+v", o)
	}
	if o.Jellyfin != p.Bin || o.DataDir != p.DataDir || o.ConfigDir != p.ConfigDir ||
		o.CacheDir != p.CacheDir || o.LogDir != p.LogDir || o.FFmpeg != ffmpeg || o.WebDir != p.WebDir {
		t.Errorf("路径解析不对：%+v", o)
	}
}

// TestJellyfinChildCmdRunsAsRealUser：子进程必须以真实用户身份 fork（setuid/setgid），
// 子进程参数与 services.JellyfinServeArgs 同源，且不经 sudo。
func TestJellyfinChildCmdRunsAsRealUser(t *testing.T) {
	stubJellyfinUser(t, "501", "20")
	p := testJellyfinPaths(t)
	const ffmpeg = "/opt/homebrew/bin/ffmpeg"
	args := services.JellyfinSuperviseArgs("/opt/zizpanel/bin/zizpanel", "zizdog", p, ffmpeg)
	o, err := jellyfinOptionsFromArgs(args[2:])
	if err != nil {
		t.Fatal(err)
	}

	cmd := jellyfinChildCmd(o) // 只构造，绝不 Start
	if cmd.Path != o.Jellyfin {
		t.Errorf("子进程可执行文件应是 %s，实际 %s", o.Jellyfin, cmd.Path)
	}
	want := append([]string{o.Jellyfin},
		services.JellyfinServeArgs(o.DataDir, o.ConfigDir, o.CacheDir, o.LogDir, o.FFmpeg, o.WebDir)...)
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("子进程参数必须与 services.JellyfinServeArgs 同源：\n得到 %v\n期望 %v", cmd.Args, want)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, need := range []string{"--datadir", "--configdir", "--cachedir", "--logdir", "--ffmpeg", "--webdir"} {
		if !strings.Contains(joined, need) {
			t.Errorf("子进程参数缺少 %q：%s", need, joined)
		}
	}
	if strings.Contains(joined, "sudo") {
		t.Errorf("不许经 sudo 降权（会多一个 setuid 进程、带偏 responsible process 判定）：%s", joined)
	}
	env := strings.Join(cmd.Env, " ")
	if !strings.Contains(env, "HOME=/Users/zizdog") {
		t.Errorf("子进程环境要有真实用户 HOME：%s", env)
	}
	if !strings.Contains(env, "PATH=/opt/homebrew/bin:") {
		t.Errorf("子进程 PATH 要含 ffmpeg 所在目录（/opt/homebrew/bin）：%s", env)
	}

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("必须以 SysProcAttr.Credential 直接降权（supervisor 是 root，fork 后 setuid）")
	}
	if got := int(cmd.SysProcAttr.Credential.Uid); got != o.UID {
		t.Errorf("子进程 uid 应是 %d，实际 %d", o.UID, got)
	}
	if got := int(cmd.SysProcAttr.Credential.Gid); got != o.GID {
		t.Errorf("子进程 gid 应是 %d，实际 %d", o.GID, got)
	}
}

package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  「参数咬合」门禁（2026-09-23 事故）
//
//  面板把 supervisor 的启动参数写进 launchd plist，supervisor 起来后自己再校验一遍。
//  这两边曾经**各写各的**：用户要求"所有应用都绑 0.0.0.0"后，面板改成
//  `--listen 0.0.0.0:7766`，而 supervisor 的校验还只放行回环 ⇒ 一 bootstrap 就退出、
//  launchd KeepAlive 反复重启，装 zizvideo 三次全失败，报错只有一句
//  "网页界面未就绪"，日志藏在 launchd 抓的 stderr 里。
//
//  所以：**面板生成的参数必须能通过 supervisor 自己的解析与校验**。
//  这类门禁抓的是"两边漂移"，不是某一条参数的值 —— 加一个 supervisor 模块就照着加一条。
// ============================================================================

// TestZizvideoSuperviseArgsFromPanelAreAccepted 面板写进 plist 的那串参数，
// supervisor 必须原样接受（含 --listen 现在默认 0.0.0.0 的局域网直连要求）。
func TestZizvideoSuperviseArgsFromPanelAreAccepted(t *testing.T) {
	stubZizvideoUser(t, "501", "20")
	home := t.TempDir()
	p := services.ZizvideoPaths{
		Home:       home,
		Root:       "/opt/zizvideo",
		Bin:        "/opt/zizvideo/bin/zizvideo",
		Plist:      "/Library/LaunchDaemons/cn.zizpanel.zizvideo.plist",
		DataDir:    filepath.Join(home, "Library", "Application Support", "zizvideo"),
		ConfigPath: filepath.Join(home, "Library", "Application Support", "zizvideo", "config.json"),
		LogDir:     filepath.Join(home, "Library", "Logs"),
	}
	args := services.ZizvideoSuperviseArgs("/opt/zizpanel/bin/zizpanel", "zizdog", p)
	if args[1] != "zizvideo-supervise" {
		t.Fatalf("参数形状变了：%v", args)
	}
	o, err := zizvideoOptionsFromArgs(args[2:])
	if err != nil {
		t.Fatalf("面板生成的参数被 supervisor 自己拒了（就是 2026-09-23 那次装不上 zizvideo 的原因）：%v\n参数：%v", err, args)
	}
	if o.User != "zizdog" || o.UID != 501 || o.GID != 20 {
		t.Errorf("身份解析不对：%+v", o)
	}
	if o.Zizvideo != p.Bin || o.ConfigPath != p.ConfigPath || o.Home != p.Home || o.LogDir != p.LogDir {
		t.Errorf("路径解析不对：%+v", o)
	}
}

// TestAria2SuperviseArgsFromPanelAreAccepted 同上，aria2 那一套。
func TestAria2SuperviseArgsFromPanelAreAccepted(t *testing.T) {
	stubAria2User(t, "501", "20")
	home := t.TempDir()
	p := services.Aria2Paths{
		Home:        home,
		Root:        filepath.Join(home, "aria"),
		Conf:        filepath.Join(home, "aria", "aria2.conf"),
		Session:     filepath.Join(home, "aria", "aria2.session"),
		DownloadDir: filepath.Join(home, "Downloads"),
		Bin:         "/opt/homebrew/bin/aria2c",
		Plist:       "/Library/LaunchDaemons/com.zizdog.aria2.plist",
		OutLog:      filepath.Join(home, "Library", "Logs", "zizpanel-aria2.out.log"),
		ErrLog:      filepath.Join(home, "Library", "Logs", "zizpanel-aria2.err.log"),
	}
	args := services.Aria2SuperviseArgs("/opt/zizpanel/bin/zizpanel", "zizdog", p)
	if args[1] != "aria2-supervise" {
		t.Fatalf("参数形状变了：%v", args)
	}
	o, err := aria2OptionsFromArgs(args[2:])
	if err != nil {
		t.Fatalf("面板生成的参数被 supervisor 自己拒了：%v\n参数：%v", err, args)
	}
	if o.Bin != p.Bin || o.Conf != p.Conf || o.Root != p.Root || o.Home != p.Home {
		t.Errorf("路径解析不对：%+v", o)
	}
	if !strings.HasSuffix(o.Conf, "aria2.conf") {
		t.Errorf("配置文件路径不对：%q", o.Conf)
	}
}

// stubTransmissionUser 注入假用户（绝不依赖真机账号）。
func stubTransmissionUser(t *testing.T, uid, gid string) {
	t.Helper()
	prev := transmissionLookupUser
	transmissionLookupUser = func(name string) (*user.User, error) {
		return &user.User{Uid: uid, Gid: gid, Username: name, HomeDir: "/Users/" + name}, nil
	}
	t.Cleanup(func() { transmissionLookupUser = prev })
}

// TestTransmissionSuperviseArgsFromPanelAreAccepted 面板写进 plist 的那串参数，
// supervisor 必须原样接受；子进程参数必须与 brew formula 的 service 块一致
// （冻结契约：改任一 flag 名都要两边同步 + 改这条门禁）。
func TestTransmissionSuperviseArgsFromPanelAreAccepted(t *testing.T) {
	stubTransmissionUser(t, "501", "20")
	home := t.TempDir()
	p := services.TransmissionPaths{
		Home:      home,
		ConfigDir: filepath.Join(home, "brew-var", "transmission"),
		Settings:  filepath.Join(home, "brew-var", "transmission", "settings.json"),
		LogFile:   filepath.Join(home, "brew-var", "transmission", "transmission-daemon.log"),
		Bin:       "/opt/homebrew/bin/transmission-daemon",
		Plist:     "/Library/LaunchDaemons/com.zizdog.transmission.plist",
		OutLog:    filepath.Join(home, "Library", "Logs", "zizpanel-transmission.out.log"),
		ErrLog:    filepath.Join(home, "Library", "Logs", "zizpanel-transmission.err.log"),
	}
	args := services.TransmissionSuperviseArgs("/opt/zizpanel/bin/zizpanel", "zizdog", p)
	if args[0] != "/opt/zizpanel/bin/zizpanel" || args[1] != "transmission-supervise" {
		t.Fatalf("参数形状变了：%v", args)
	}
	o, err := transmissionOptionsFromArgs(args[2:])
	if err != nil {
		t.Fatalf("面板生成的参数被 supervisor 自己拒了：%v\n参数：%v", err, args)
	}
	if o.User != "zizdog" || o.UID != 501 || o.GID != 20 {
		t.Errorf("身份解析不对：%+v", o)
	}
	if o.Bin != p.Bin || o.ConfigDir != p.ConfigDir || o.LogFile != p.LogFile || o.Home != p.Home {
		t.Errorf("路径解析不对：%+v", o)
	}
	// 子进程参数是冻结契约：与 brew formula 的 service 块逐字一致。
	cmd := transmissionChildCmd(o)
	want := []string{p.Bin, "--foreground", "--config-dir", p.ConfigDir, "--log-info", "--logfile", p.LogFile}
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("子进程参数与 brew service 块不一致：\n实际 %v\n期望 %v", cmd.Args, want)
	}
	if cmd.Dir != p.ConfigDir {
		t.Errorf("子进程工作目录应是配置目录 %s，实际 %s", p.ConfigDir, cmd.Dir)
	}
	if !envHas(cmd.Env, "HOME="+p.Home) || !envHas(cmd.Env, "USER=zizdog") {
		t.Errorf("子进程环境必须带真实用户的 HOME/USER，实际 %v", cmd.Env)
	}
	// 非 root 时不能再 setuid（cmd.Start 会 EPERM）。
	if os.Geteuid() != 0 && cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
		t.Error("非 root 时不能设 SysProcAttr.Credential（会 EPERM）")
	}
}

// TestTransmissionSuperviseOptionsRejectsBadInput 参数校验：宁可 supervisor 起来就报错，
// 也不要出现"以 root 跑 daemon、下载文件变成 root 所有"或"plist 里写相对路径"。
func TestTransmissionSuperviseOptionsRejectsBadInput(t *testing.T) {
	home := t.TempDir()
	goodBin := filepath.Join(home, "bin", "transmission-daemon")
	goodDir := filepath.Join(home, "var", "transmission")
	goodLog := filepath.Join(goodDir, "transmission-daemon.log")
	cases := []struct {
		name, userName, bin, configDir, logFile, home, wantErr string
		uid, gid                                               string
	}{
		{"缺用户名", "", goodBin, goodDir, goodLog, home, "--user", "501", "20"},
		{"uid 是 0（root）", "zizdog", goodBin, goodDir, goodLog, home, "root", "0", "0"},
		{"二进制非绝对路径", "zizdog", "transmission-daemon", goodDir, goodLog, home, "绝对路径", "501", "20"},
		{"配置目录非绝对路径", "zizdog", goodBin, "var/transmission", goodLog, home, "绝对路径", "501", "20"},
		{"日志非绝对路径", "zizdog", goodBin, goodDir, "daemon.log", home, "绝对路径", "501", "20"},
		{"家目录为空", "zizdog", goodBin, goodDir, goodLog, "", "绝对路径", "501", "20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubTransmissionUser(t, tc.uid, tc.gid)
			_, err := transmissionSuperviseOptionsFrom(tc.userName, tc.bin, tc.configDir, tc.logFile, tc.home)
			if err == nil {
				t.Fatalf("必须拒绝：%s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息应包含 %q，实际 %v", tc.wantErr, err)
			}
		})
	}
}

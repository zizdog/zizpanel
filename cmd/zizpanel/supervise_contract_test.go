package main

import (
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

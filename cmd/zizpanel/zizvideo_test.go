package main

// zizvideo_test.go —— zizvideo supervisor 的门禁测试（cmd 侧）。
//
// 判据是"构造出来的子进程"而不是真跑一个进程：
//   - 参数必须来自**真实用户**的 config/家目录；
//   - 身份必须是 SysProcAttr.Credential 直接 setuid/setgid 到该用户（不经 sudo）。
//
// 单测绝不真起 zizvideo、绝不碰真实 launchd/家目录（AGENTS 第三节）。

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// stubZizvideoUser 注入一个假用户查询（绝不依赖真机账号）。
func stubZizvideoUser(t *testing.T, uid, gid string) {
	t.Helper()
	prev := zizvideoLookupUser
	zizvideoLookupUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, Uid: uid, Gid: gid, HomeDir: "/Users/" + name}, nil
	}
	t.Cleanup(func() { zizvideoLookupUser = prev })
}

// testZizvideoOptions 造一份通过校验的运行参数（家目录是临时目录）。
func testZizvideoOptions(t *testing.T) zizvideoSuperviseOptions {
	t.Helper()
	stubZizvideoUser(t, "501", "20")
	home := t.TempDir()
	o, err := zizvideoSuperviseOptionsFrom(
		"zizdog",
		"/opt/zizvideo/bin/zizvideo",
		filepath.Join(home, "Library", "Application Support", "zizvideo", "config.json"),
		home,
		"127.0.0.1:7766",
		filepath.Join(home, "Library", "Logs"),
	)
	if err != nil {
		t.Fatalf("合法参数不该报错：%v", err)
	}
	return o
}

// TestZizvideoChildCmdRunsAsRealUserAndUsesUserPaths 是核心门禁。
func TestZizvideoChildCmdRunsAsRealUserAndUsesUserPaths(t *testing.T) {
	o := testZizvideoOptions(t)
	cmd := zizvideoChildCmd(o) // 只构造，绝不 Start

	if cmd.Path != o.Zizvideo {
		t.Errorf("子进程可执行文件应是 %s，实际 %s", o.Zizvideo, cmd.Path)
	}
	want := append([]string{o.Zizvideo}, services.ZizvideoServeArgs(o.ConfigPath)...)
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("子进程参数必须与 services.ZizvideoServeArgs 同源：\n得到 %v\n期望 %v", cmd.Args, want)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, need := range []string{o.ConfigPath, "--config"} {
		if !strings.Contains(joined, need) {
			t.Errorf("子进程参数缺少 %q：%s", need, joined)
		}
	}
	if strings.Contains(joined, "sudo") {
		t.Errorf("不许经 sudo 降权（会多一个 setuid 进程、带偏 responsible process 判定）：%s", joined)
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
	if o.UID == 0 {
		t.Error("uid=0 会让子进程变回 root，直接违反本设计")
	}
	if cmd.Dir != filepath.Dir(o.ConfigPath) {
		t.Errorf("工作目录应是数据目录 %s，实际 %s", filepath.Dir(o.ConfigPath), cmd.Dir)
	}
	if !envHas(cmd.Env, "HOME="+o.Home) {
		t.Errorf("子进程环境必须带 HOME=%s，实际 %v", o.Home, cmd.Env)
	}
	if !envHas(cmd.Env, "USER="+o.User) {
		t.Errorf("子进程环境必须带 USER=%s，实际 %v", o.User, cmd.Env)
	}
}

// TestZizvideoSuperviseOptionsRejectsBadInput 参数校验必须挡住危险输入。
func TestZizvideoSuperviseOptionsRejectsBadInput(t *testing.T) {
	home := t.TempDir()
	goodCfg := filepath.Join(home, "d", "config.json")
	cases := []struct {
		name    string
		user    string
		uid     string
		gid     string
		listen  string
		bin     string
		cfg     string
		home    string
		wantErr string
	}{
		{"缺用户名", "", "501", "20", "127.0.0.1:7766", "/opt/zizvideo/bin/zizvideo", goodCfg, home, "--user"},
		{"uid 是 0（root）", "zizdog", "0", "0", "127.0.0.1:7766", "/opt/zizvideo/bin/zizvideo", goodCfg, home, "root"},
		{"监听写成具体内网地址", "zizdog", "501", "20", "192.0.2.7:7766", "/opt/zizvideo/bin/zizvideo", goodCfg, home, "回环"},
		{"二进制非绝对路径", "zizdog", "501", "20", "127.0.0.1:7766", "zizvideo", goodCfg, home, "绝对路径"},
		{"配置非绝对路径", "zizdog", "501", "20", "127.0.0.1:7766", "/opt/zizvideo/bin/zizvideo", "config.json", home, "绝对路径"},
		{"家目录为空", "zizdog", "501", "20", "127.0.0.1:7766", "/opt/zizvideo/bin/zizvideo", goodCfg, "", "绝对路径"},
		{"监听缺端口", "zizdog", "501", "20", "127.0.0.1", "/opt/zizvideo/bin/zizvideo", goodCfg, home, "回环"},
		{"监听写主机名", "zizdog", "501", "20", "example.com:7766", "/opt/zizvideo/bin/zizvideo", goodCfg, home, "回环"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubZizvideoUser(t, tc.uid, tc.gid)
			_, err := zizvideoSuperviseOptionsFrom(tc.user, tc.bin, tc.cfg, tc.home, tc.listen, "")
			if err == nil {
				t.Fatalf("必须拒绝：%s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息应包含 %q，实际 %v", tc.wantErr, err)
			}
		})
	}
}

// TestZizvideoListenAllowed 监听地址**只放行回环**（zizvideo/CONTRACT.md 的约定）。
func TestZizvideoListenAllowed(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:7766", "localhost:7766", "[::1]:7766", "0.0.0.0:7766", "[::]:7766"} {
		if !zizvideoListenAllowed(ok) {
			t.Errorf("%q 是回环/通配地址，应放行", ok)
		}
	}
	// 203.0.113.0/24 是 RFC 5737 文档保留地址（仓库门禁禁止作者家里的内网地址）。
	for _, bad := range []string{":7766", "192.0.2.7:7766", "127.0.0.1", "example.com:7766"} {
		if zizvideoListenAllowed(bad) {
			t.Errorf("%q 既不是回环也不是通配，必须拒绝", bad)
		}
	}
}

// TestZizvideoSuperviseRefusesNonRoot 非 root 运行 supervisor 必须如实失败。
func TestZizvideoSuperviseRefusesNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 跑测试时这条分支不成立")
	}
	stubZizvideoUser(t, "501", "20")
	home := t.TempDir()
	err := cmdZizvideoSupervise([]string{
		"--user", "zizdog",
		"--config", filepath.Join(home, "d", "config.json"),
		"--home", home,
	})
	if err == nil {
		t.Fatal("非 root 时必须报错，不能假装降权成功")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("错误信息要说清需要 root，实际 %v", err)
	}
}

// envHas 判断环境变量切片里有没有某个完整键值。
func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// TestZizvideoSuperviseForwardsListenToChild 是 2026-09-23 那次"装不上 zizvideo"的
// **三段咬合**门禁：面板写进 plist 的参数 → supervisor 接受 → 真的传给了子进程。
//
// 为什么必须三段一起断言：老门禁只查"plist 里出现过 --listen 0.0.0.0:7766"，
// 所以生成侧绿、校验侧绿、整条链却是死的（supervisor 起来即 exit 1；就算放行，
// 这个值在当时也**没有**任何字段承载、从来没传给 zizvideo）。
func TestZizvideoSuperviseForwardsListenToChild(t *testing.T) {
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

	// ① 校验侧接受面板生成的参数（含 0.0.0.0）
	args := services.ZizvideoSuperviseArgs("/opt/zizpanel/bin/zizpanel", "zizdog", p)
	o, err := zizvideoOptionsFromArgs(args[2:])
	if err != nil {
		t.Fatalf("面板生成的参数被 supervisor 拒了：%v（参数 %v）", err, args)
	}
	if o.Listen != "0.0.0.0:7766" {
		t.Fatalf("--listen 没被解析进 options（这正是当时「传不下去」的原因）：%+v", o)
	}

	// ② 子进程 env 里必须真的有 ZV_LISTEN —— zizvideo 只认 config.json 的 listen
	//    或环境变量 ZV_LISTEN（internal/config/config.go applyEnv），不认命令行。
	cmd := zizvideoChildCmd(o)
	if !envHas(cmd.Env, "ZV_LISTEN=0.0.0.0:7766") {
		t.Errorf("子进程没拿到 ZV_LISTEN=0.0.0.0:7766（局域网直连就是假的）：%v", cmd.Env)
	}
	if envHas(cmd.Env, "ZV_LISTEN=127.0.0.1:7766") {
		t.Errorf("不该把回环地址传下去：%v", cmd.Env)
	}
	// ③ 参数里不许出现 --listen（zizvideo 不认它，出现即误导）
	for _, a := range cmd.Args {
		if strings.HasPrefix(a, "--listen") {
			t.Errorf("不要把 --listen 传给 zizvideo（它只认 config.json / ZV_LISTEN）：%v", cmd.Args)
		}
	}
}

package main

// macsaber_test.go —— mac军刀 supervisor 的门禁测试（cmd 侧）。
//
// 判据都是"构造出来的子进程"而不是真跑一个进程：
//   - 参数必须来自**真实用户**的家目录/数据目录/写根；
//   - 身份必须是 SysProcAttr.Credential 直接 setuid/setgid 到该用户（不经 sudo）。
//
// 单测绝不真起 macsaber、绝不碰真实 launchd/家目录（AGENTS 第三节）。

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// stubMacSaberUser 注入一个假用户查询（绝不依赖真机账号）。
func stubMacSaberUser(t *testing.T, uid, gid string) {
	t.Helper()
	prev := macSaberLookupUser
	macSaberLookupUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, Uid: uid, Gid: gid, HomeDir: "/Users/" + name}, nil
	}
	t.Cleanup(func() { macSaberLookupUser = prev })
}

// testSuperviseOptions 造一份通过校验的运行参数（家目录是临时目录）。
func testSuperviseOptions(t *testing.T) macSaberSuperviseOptions {
	t.Helper()
	stubMacSaberUser(t, "501", "20")
	home := t.TempDir()
	o, err := macSaberSuperviseOptionsFrom(
		"zizdog",
		"/opt/macsaber/bin/macsaber",
		filepath.Join(home, "Library", "Application Support", "macsaber"),
		"127.0.0.1:8895",
		home,
		filepath.Join(home, "MacSaberFiles"),
		filepath.Join(home, "Library", "Logs"),
	)
	if err != nil {
		t.Fatalf("合法参数不该报错：%v", err)
	}
	return o
}

// TestMacSaberChildCmdRunsAsRealUserAndUsesUserPaths 是核心门禁：
// 子进程参数来自真实用户的路径，身份直接 setuid/setgid 到该用户，且不经 sudo。
func TestMacSaberChildCmdRunsAsRealUserAndUsesUserPaths(t *testing.T) {
	o := testSuperviseOptions(t)
	cmd := macSaberChildCmd(o) // 只构造，绝不 Start

	if cmd.Path != o.MacSaberBin {
		t.Errorf("子进程可执行文件应是 %s，实际 %s", o.MacSaberBin, cmd.Path)
	}
	want := append([]string{o.MacSaberBin},
		services.MacSaberServeArgs(o.DataDir, o.Listen, o.ReadRoot, o.WriteRoot)...)
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("子进程参数必须与 services.MacSaberServeArgs 同源：\n得到 %v\n期望 %v", cmd.Args, want)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, need := range []string{o.DataDir, o.ReadRoot, o.WriteRoot, "127.0.0.1:8895"} {
		if !strings.Contains(joined, need) {
			t.Errorf("子进程参数缺少真实用户路径 %q：%s", need, joined)
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

	// 工作目录与 HOME 都必须指向真实用户家目录：macsaber 与它 spawn 的
	// say/osascript/pbcopy 都靠这个环境。
	if cmd.Dir != o.ReadRoot {
		t.Errorf("工作目录应是用户家目录 %s，实际 %s", o.ReadRoot, cmd.Dir)
	}
	if !envHas(cmd.Env, "HOME="+o.ReadRoot) {
		t.Errorf("子进程环境必须带 HOME=%s，实际 %v", o.ReadRoot, cmd.Env)
	}
	if !envHas(cmd.Env, "USER="+o.User) {
		t.Errorf("子进程环境必须带 USER=%s，实际 %v", o.User, cmd.Env)
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

// TestMacSaberSuperviseOptionsRejectsBadInput 参数校验必须挡住危险输入。
func TestMacSaberSuperviseOptionsRejectsBadInput(t *testing.T) {
	home := t.TempDir()
	good := filepath.Join(home, "d")
	cases := []struct {
		name     string
		user     string
		uid, gid string
		listen   string
		bin      string
		data     string
		readRoot string
		write    string
		wantErr  string
	}{
		{"缺用户名", "", "501", "20", "127.0.0.1:8895", "/opt/macsaber/bin/macsaber", good, home, filepath.Join(home, "w"), "--user"},
		{"uid 是 0（root）", "zizdog", "0", "0", "127.0.0.1:8895", "/opt/macsaber/bin/macsaber", good, home, filepath.Join(home, "w"), "root"},
		{"监听非回环", "zizdog", "501", "20", "0.0.0.0:8895", "/opt/macsaber/bin/macsaber", good, home, filepath.Join(home, "w"), "回环"},
		{"二进制非绝对路径", "zizdog", "501", "20", "127.0.0.1:8895", "macsaber", good, home, filepath.Join(home, "w"), "绝对路径"},
		{"数据目录非绝对路径", "zizdog", "501", "20", "127.0.0.1:8895", "/opt/macsaber/bin/macsaber", "data", home, filepath.Join(home, "w"), "绝对路径"},
		{"读根为空", "zizdog", "501", "20", "127.0.0.1:8895", "/opt/macsaber/bin/macsaber", good, "", filepath.Join(home, "w"), "绝对路径"},
		{"监听缺端口", "zizdog", "501", "20", "127.0.0.1", "/opt/macsaber/bin/macsaber", good, home, filepath.Join(home, "w"), "回环"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubMacSaberUser(t, tc.uid, tc.gid)
			_, err := macSaberSuperviseOptionsFrom(tc.user, tc.bin, tc.data, tc.listen, tc.readRoot, tc.write, "")
			if err == nil {
				t.Fatalf("必须拒绝：%s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息应包含 %q，实际 %v", tc.wantErr, err)
			}
		})
	}
}

// TestMacSaberLoopbackListen 只允许回环监听（这个界面能读写本机文件）。
func TestMacSaberLoopbackListen(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8895", "localhost:8895", "[::1]:8895"} {
		if !macSaberLoopbackListen(ok) {
			t.Errorf("%q 是回环地址，应放行", ok)
		}
	}
	// 用 203.0.113.0/24（RFC 5737 文档保留地址）当"非回环"样本：
	// 门禁禁止仓库里出现作者家里的内网地址（internal/config/mirror_public_only_test.go）。
	for _, bad := range []string{"0.0.0.0:8895", ":8895", "203.0.113.5:8895", "127.0.0.1", "example.com:8895"} {
		if macSaberLoopbackListen(bad) {
			t.Errorf("%q 不是回环地址，必须拒绝", bad)
		}
	}
}

// TestMacSaberSuperviseRefusesNonRoot 非 root 运行 supervisor 必须如实失败：
// 它要 fork 后 setuid 降权，普通用户做不到。
func TestMacSaberSuperviseRefusesNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 跑测试时这条分支不成立")
	}
	stubMacSaberUser(t, "501", "20")
	home := t.TempDir()
	err := cmdMacSaberSupervise([]string{
		"--user", "zizdog",
		"--data", filepath.Join(home, "d"),
		"--read-root", home,
		"--write-root", filepath.Join(home, "w"),
	})
	if err == nil {
		t.Fatal("非 root 时必须报错，不能假装降权成功")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("错误信息要说清需要 root，实际 %v", err)
	}
}

package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// stubAria2User 注入假用户（绝不依赖真机账号）。
func stubAria2User(t *testing.T, uid, gid string) {
	t.Helper()
	prev := aria2LookupUser
	aria2LookupUser = func(name string) (*user.User, error) {
		return &user.User{Uid: uid, Gid: gid, Username: name, HomeDir: "/Users/" + name}, nil
	}
	t.Cleanup(func() { aria2LookupUser = prev })
}

// TestAria2SuperviseOptionsRejectsBadInput 参数校验：宁可 supervisor 起来就报错，
// 也不要出现"以 root 跑 aria2、下载文件变成 root 所有"或"plist 里写相对路径"
// （launchd 的工作目录不是用户家目录，相对路径到那时才发现就太晚）。
func TestAria2SuperviseOptionsRejectsBadInput(t *testing.T) {
	home := t.TempDir()
	goodBin := filepath.Join(home, "bin", "aria2c")
	goodConf := filepath.Join(home, "aria", "aria2.conf")
	goodRoot := filepath.Join(home, "aria")
	cases := []struct {
		name, user, bin, conf, root, home, wantErr string
		uid, gid                                   string
	}{
		{"缺用户名", "", goodBin, goodConf, goodRoot, home, "--user", "501", "20"},
		{"uid 是 0（root）", "zizdog", goodBin, goodConf, goodRoot, home, "root", "0", "0"},
		{"二进制非绝对路径", "zizdog", "aria2c", goodConf, goodRoot, home, "绝对路径", "501", "20"},
		{"配置非绝对路径", "zizdog", goodBin, "aria2.conf", goodRoot, home, "绝对路径", "501", "20"},
		{"root 非绝对路径", "zizdog", goodBin, goodConf, "aria", home, "绝对路径", "501", "20"},
		{"家目录为空", "zizdog", goodBin, goodConf, goodRoot, "", "绝对路径", "501", "20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubAria2User(t, tc.uid, tc.gid)
			_, err := aria2SuperviseOptionsFrom(tc.user, tc.bin, tc.conf, tc.root, tc.home)
			if err == nil {
				t.Fatalf("必须拒绝：%s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息应包含 %q，实际 %v", tc.wantErr, err)
			}
		})
	}
}

// TestAria2SuperviseOptionsAcceptsGoodInput 正面对照：一份合法输入必须解析成功，
// 且 uid/gid 被如实带下去（降权靠它）。
func TestAria2SuperviseOptionsAcceptsGoodInput(t *testing.T) {
	stubAria2User(t, "501", "20")
	home := t.TempDir()
	o, err := aria2SuperviseOptionsFrom("zizdog",
		filepath.Join(home, "bin", "aria2c"),
		filepath.Join(home, "aria", "aria2.conf"),
		filepath.Join(home, "aria"), home)
	if err != nil {
		t.Fatalf("合法输入不该被拒：%v", err)
	}
	if o.UID != 501 || o.GID != 20 || o.User != "zizdog" {
		t.Errorf("uid/gid/user 解析不对：%+v", o)
	}
}

// TestAria2ChildCmdCarriesRealUserHome 子进程参数与身份：跑的是 aria2c + conf，
// 工作目录是 ~/aria，HOME 是真实家目录；**非 root 时不能再 setuid**（会 EPERM）。
func TestAria2ChildCmdCarriesRealUserHome(t *testing.T) {
	home := t.TempDir()
	o := aria2SuperviseOptions{
		User: "zizdog", UID: os.Getuid(), GID: os.Getgid(),
		Bin:  filepath.Join(home, "bin", "aria2c"),
		Conf: filepath.Join(home, "aria", "aria2.conf"),
		Root: filepath.Join(home, "aria"),
		Home: home,
	}
	cmd := aria2ChildCmd(o)
	if got, want := cmd.Args[0], o.Bin; got != want {
		t.Errorf("子进程应是 aria2c 本身（%s），实际 %q", want, got)
	}
	if got, want := cmd.Args[1], "--conf-path="+o.Conf; got != want {
		t.Errorf("子进程参数应是 %q，实际 %q", want, got)
	}
	if cmd.Dir != o.Root {
		t.Errorf("工作目录应是 %s，实际 %s", o.Root, cmd.Dir)
	}
	if !envHas(cmd.Env, "HOME="+home) {
		t.Errorf("HOME 必须是真实家目录（aria2 的 dht.dat/.aria2 都在里面），实际 %v", cmd.Env)
	}
	if envHas(cmd.Env, "HOME=/root") {
		t.Errorf("绝不能把 root 的家目录给下载器：%v", cmd.Env)
	}
	if !envHas(cmd.Env, "USER=zizdog") {
		t.Errorf("USER 应是真实用户，实际 %v", cmd.Env)
	}
	if os.Geteuid() == 0 {
		if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil ||
			int(cmd.SysProcAttr.Credential.Uid) != o.UID || int(cmd.SysProcAttr.Credential.Gid) != o.GID {
			t.Errorf("root 跑 supervisor 时必须以目标用户 setuid（%d:%d）", o.UID, o.GID)
		}
		return
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
		t.Error("非 root 时不能再 setuid（cmd.Start 会 EPERM）")
	}
}

package smb

// mount_test.go —— 执行身份的纯函数门禁（真机教训：root 挂的 smbfs 别的用户读不到，
// 2026-09-29 实测 zizdog `ls /opt/zizpanel/mnt/<盘>` 直接 Permission denied）。
// 这里只钉"把用户名映射成 uid/gid/家目录"这一步：绝不真的 setuid。

import (
	"errors"
	"os/user"
	"strings"
	"testing"
)

func TestResolveRunAsMapsUserAndRefusesToGuess(t *testing.T) {
	prevLookup, prevEUID := runAsLookupFn, runAsEUIDFn
	t.Cleanup(func() { runAsLookupFn, runAsEUIDFn = prevLookup, prevEUID })

	runAsEUIDFn = func() int { return 0 } // 假装面板是 root
	runAsLookupFn = func(name string) (*user.User, error) {
		if name != "mediauser" {
			return nil, errors.New("no such user")
		}
		return &user.User{Username: "mediauser", Uid: "501", Gid: "20", HomeDir: "/Users/mediauser"}, nil
	}

	got := resolveRunAs("mediauser")
	if got == nil || got.Name != "mediauser" || got.UID != 501 || got.GID != 20 || got.Home != "/Users/mediauser" {
		t.Fatalf("身份映射不对：%+v", got)
	}
	// 负向对照：解析不到的用户绝不猜（宁可照当前身份跑）。
	if resolveRunAs("nobody-here") != nil {
		t.Errorf("解析不到的用户必须返回 nil")
	}
	// 负向对照：自己不是 root 就不降权（调试实例以当前用户跑）。
	runAsEUIDFn = func() int { return 501 }
	if resolveRunAs("mediauser") != nil {
		t.Errorf("非 root 进程不该 setuid")
	}
	// 负向对照：空名 / root 目标都不降权。
	runAsEUIDFn = func() int { return 0 }
	if resolveRunAs("  ") != nil {
		t.Errorf("空名必须返回 nil")
	}
	runAsLookupFn = func(string) (*user.User, error) {
		return &user.User{Username: "root", Uid: "0", Gid: "0", HomeDir: "/var/root"}, nil
	}
	if resolveRunAs("root") != nil {
		t.Errorf("目标是 root 时没有降权意义，必须返回 nil")
	}
}

// TestParseShareTableAndStatusHints —— 「列出共享」解析 + SMB 状态码翻译（纯函数）。
func TestParseShareTableAndStatusHints(t *testing.T) {
	out := `Password for 192.0.2.10: 
Share                                           Type    Comments
-------------------------------
Media                                           Disk    
Photos                                          Disk    
IPC$                                            Pipe    IPC Service (fake nas)

3 shares listed
`
	got := ParseShareTable(out)
	if len(got) != 2 || got[0].Name != "Media" || got[1].Name != "Photos" {
		t.Fatalf("解析共享表不对（只该留 Disk）：%+v", got)
	}
	if len(ParseShareTable("smbutil: server rejected the authentication")) != 0 {
		t.Errorf("没有表格时不该猜出共享")
	}

	// 真机事故：mini 上共享名填成 fnnas-media ⇒ 服务端回 -1073741275（0xC0000225）。
	h, ok := smbStatusHint("mount_smbfs: mount error: /opt/zizpanel/mnt/x: unknown error: -1073741275")
	if !ok || !strings.Contains(h.msg, "找不到这个共享") {
		t.Errorf("0xC0000225 必须翻成「找不到共享」，实际 ok=%v %+v", ok, h)
	}
	if _, ok := smbStatusHint("mount_smbfs: mount error: /x: unknown error: c000006d"); !ok {
		t.Errorf("十六进制状态码也要认")
	}
	if _, ok := smbStatusHint("mount_smbfs: ok"); ok {
		t.Errorf("没有状态码时不该硬套结论")
	}
}

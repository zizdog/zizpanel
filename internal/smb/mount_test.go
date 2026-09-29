package smb

// mount_test.go —— 执行身份的纯函数门禁（真机教训：root 挂的 smbfs 别的用户读不到，
// 2026-09-29 实测 zizdog `ls /opt/zizpanel/mnt/<盘>` 直接 Permission denied）。
// 这里只钉"把用户名映射成 uid/gid/家目录"这一步：绝不真的 setuid。

import (
	"errors"
	"os/user"
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

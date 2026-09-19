package files

// volumeauth_test.go —— 「一次性外接卷授权请求」的门禁。
//
// 用户 2026-09-19 的铁律：**非真机 / 无人在场时，绝不触发任何系统授权弹窗**。
// 这里锁死三件事：
//  ① 没有一次性标记 → 一个字节都不读；
//  ② 有标记但**没有图形登录会话**（控制台用户为空/root/loginwindow）→ 仍然一个字节都不读，
//     而且标记必须**保留**（等有人在机器前时再试），绝不能删掉当作"处理过了"；
//  ③ 有标记 + 有人在 → 才真的读；读通/被拒都要如实记录，并且标记一次性消费掉。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubVolumeAuth 注入"控制台用户 / 外接卷列表 / 读目录动作"，并返回一个计数器。
func stubVolumeAuth(t *testing.T, consoleUser string, mounts []string, readErr error) *int {
	t.Helper()
	prevConsole, prevMounts, prevRead := consoleUserFn, volumeMountsForAuthFn, readDirFn
	calls := 0
	consoleUserFn = func() string { return consoleUser }
	volumeMountsForAuthFn = func() []string { return mounts }
	readDirFn = func(string) ([]os.DirEntry, error) {
		calls++
		return nil, readErr
	}
	t.Cleanup(func() {
		consoleUserFn, volumeMountsForAuthFn, readDirFn = prevConsole, prevMounts, prevRead
	})
	return &calls
}

func writeMarker(t *testing.T, dir string) string {
	t.Helper()
	p := VolumeAuthMarkerPath(dir)
	if err := os.WriteFile(p, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestVolumeAuthNoMarkerDoesNothing：没有标记就什么都不做（远程安装/普通启动都是这条路径）。
func TestVolumeAuthNoMarkerDoesNothing(t *testing.T) {
	dir := t.TempDir()
	calls := stubVolumeAuth(t, "zizdog", []string{"/Volumes/Ext"}, nil)

	var logs []string
	res := RequestVolumeAuthorizationOnce(context.Background(), VolumeAuthMarkerPath(dir), time.Second,
		func(f string, a ...any) { logs = append(logs, f) })

	if !res.Skipped {
		t.Errorf("没有标记时必须 Skipped，实际 %+v", res)
	}
	if *calls != 0 {
		t.Errorf("没有标记时不许读任何卷，实际读了 %d 次", *calls)
	}
	if res.MarkerConsumed {
		t.Error("没有标记时不该报告消费了标记")
	}
}

// TestVolumeAuthNoConsoleSessionNeverTouchesVolume：**铁律的核心门禁** ——
// 没人在屏幕前（无登录会话）时一个字节都不许读，且标记必须留着。
func TestVolumeAuthNoConsoleSessionNeverTouchesVolume(t *testing.T) {
	for _, consoleUser := range []string{"", "root", "loginwindow"} {
		dir := t.TempDir()
		marker := writeMarker(t, dir)
		calls := stubVolumeAuth(t, consoleUser, []string{"/Volumes/Ext"}, nil)

		res := RequestVolumeAuthorizationOnce(context.Background(), marker, time.Second, nil)

		if !res.Skipped {
			t.Errorf("控制台用户 %q 时必须 Skipped，实际 %+v", consoleUser, res)
		}
		if *calls != 0 {
			t.Errorf("控制台用户 %q 时绝不许碰外接卷（会触发没人能点的弹窗），实际读了 %d 次",
				consoleUser, *calls)
		}
		if res.MarkerConsumed {
			t.Errorf("控制台用户 %q 时标记必须保留（等人来了再试），却被消费了", consoleUser)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("控制台用户 %q 时标记文件应仍然存在：%v", consoleUser, err)
		}
		if len(res.Attempted) != 0 {
			t.Errorf("控制台用户 %q 时不该有「被拒」记录，实际 %v", consoleUser, res.Attempted)
		}
	}
}

// TestVolumeAuthNoExternalVolumeConsumesMarker：有人在、但没有外接卷 → 消费标记，不读任何东西。
func TestVolumeAuthNoExternalVolumeConsumesMarker(t *testing.T) {
	dir := t.TempDir()
	marker := writeMarker(t, dir)
	calls := stubVolumeAuth(t, "zizdog", nil, nil)

	res := RequestVolumeAuthorizationOnce(context.Background(), marker, time.Second, nil)
	if !res.Skipped || !res.MarkerConsumed {
		t.Errorf("没有外接卷时应 Skipped 且消费标记，实际 %+v", res)
	}
	if *calls != 0 {
		t.Errorf("没有外接卷时不该读任何东西，实际读了 %d 次", *calls)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("标记应已被删除，实际 err=%v", err)
	}
}

// TestVolumeAuthAlreadyReadable：已经在可读的卷（早就授权过）→ 记为 Okay，不等待。
func TestVolumeAuthAlreadyReadable(t *testing.T) {
	dir := t.TempDir()
	marker := writeMarker(t, dir)
	calls := stubVolumeAuth(t, "zizdog", []string{"/Volumes/A", "/Volumes/B"}, nil)

	res := RequestVolumeAuthorizationOnce(context.Background(), marker, time.Second, nil)
	if len(res.Okay) != 2 || len(res.Attempted) != 0 {
		t.Errorf("两个卷都读得通时应全部记为 Okay，实际 %+v", res)
	}
	if *calls != 2 {
		t.Errorf("应恰好读两次（每卷一次），实际 %d 次", *calls)
	}
	if !res.MarkerConsumed {
		t.Error("处理过就必须消费标记")
	}
}

// TestVolumeAuthDeniedThenAllowed：被拒 → 等用户点「允许」→ 之后读通，记为 Okay。
func TestVolumeAuthDeniedThenAllowed(t *testing.T) {
	dir := t.TempDir()
	marker := writeMarker(t, dir)
	prevConsole, prevMounts, prevRead := consoleUserFn, volumeMountsForAuthFn, readDirFn
	t.Cleanup(func() { consoleUserFn, volumeMountsForAuthFn, readDirFn = prevConsole, prevMounts, prevRead })
	consoleUserFn = func() string { return "zizdog" }
	volumeMountsForAuthFn = func() []string { return []string{"/Volumes/Ext"} }
	n := 0
	readDirFn = func(string) ([]os.DirEntry, error) {
		n++
		if n == 1 {
			return nil, &fs.PathError{Op: "open", Path: "/Volumes/Ext", Err: fs.ErrPermission}
		}
		return nil, nil // 用户点了允许
	}

	res := RequestVolumeAuthorizationOnce(context.Background(), marker, 5*time.Second, nil)
	if len(res.Attempted) != 1 {
		t.Errorf("第一次被拒必须如实记为 Attempted，实际 %+v", res)
	}
	if len(res.Okay) != 1 {
		t.Errorf("用户点允许后应记为 Okay，实际 %+v", res)
	}
	if n < 2 {
		t.Errorf("被拒后必须重读确认，实际只读了 %d 次", n)
	}
}

// TestVolumeAuthDeniedStaysDenied：被拒且用户一直没点 → 如实记为"仍未授权"，不谎报成功。
func TestVolumeAuthDeniedStaysDenied(t *testing.T) {
	dir := t.TempDir()
	marker := writeMarker(t, dir)
	var logs []string
	calls := stubVolumeAuth(t, "zizdog", []string{"/Volumes/Ext"},
		&fs.PathError{Op: "open", Path: "/Volumes/Ext", Err: fs.ErrPermission})

	res := RequestVolumeAuthorizationOnce(context.Background(), marker, 0,
		func(f string, a ...any) { logs = append(logs, f) })

	if len(res.Attempted) != 1 || len(res.Okay) != 0 {
		t.Errorf("被拒且未授权时应只在 Attempted 里，实际 %+v", res)
	}
	if *calls == 0 {
		t.Error("应该真的去读过一次")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "仍未授权") {
		t.Errorf("被拒后应如实说\"仍未授权\"并给指引，实际日志 %v", logs)
	}
}

// TestVolumeAuthNonPermissionErrorIsNotAuthorizationFailure：
// 卷已卸载之类的问题不算"授权失败"，也不该触发弹窗逻辑（不重读）。
func TestVolumeAuthNonPermissionErrorIsNotAuthorizationFailure(t *testing.T) {
	dir := t.TempDir()
	marker := writeMarker(t, dir)
	calls := stubVolumeAuth(t, "zizdog", []string{"/Volumes/Gone"},
		&fs.PathError{Op: "open", Path: "/Volumes/Gone", Err: fs.ErrNotExist})

	res := RequestVolumeAuthorizationOnce(context.Background(), marker, 5*time.Second, nil)
	if len(res.Attempted) != 0 || len(res.Okay) != 0 {
		t.Errorf("非权限错误不该被当成授权失败，实际 %+v", res)
	}
	if *calls != 1 {
		t.Errorf("非权限错误不该反复重读，实际读了 %d 次", *calls)
	}
}

// TestVolumeAuthMarkerPathIsInsideDataDir：标记必须落在 DataDir（只有 root 能写）。
func TestVolumeAuthMarkerPathIsInsideDataDir(t *testing.T) {
	got := VolumeAuthMarkerPath("/opt/zizpanel/data/")
	want := filepath.Join("/opt/zizpanel/data", "request-volume-auth.once")
	if got != want {
		t.Errorf("标记路径应为 %q，实际 %q", want, got)
	}
}

// ---------- 按需入口（用户点「申请授权」） ----------

// TestRequestVolumeAuthorizationNowNoConsoleSessionNeverReads：按需入口守着同一条铁律 ——
// 没人在屏幕前时一个字节都不读（它没有一次性标记可依赖，全靠这条预检）。
func TestRequestVolumeAuthorizationNowNoConsoleSessionNeverReads(t *testing.T) {
	for _, consoleUser := range []string{"", "root", "loginwindow"} {
		calls := stubVolumeAuth(t, consoleUser, []string{"/Volumes/Ext"}, nil)

		res := RequestVolumeAuthorizationNow(context.Background(), time.Second, nil)

		if !res.Skipped {
			t.Errorf("控制台用户 %q 时按需入口必须 Skipped，实际 %+v", consoleUser, res)
		}
		if *calls != 0 {
			t.Errorf("控制台用户 %q 时按需入口也绝不许碰外接卷，实际读了 %d 次", consoleUser, *calls)
		}
		if len(res.Attempted) != 0 {
			t.Errorf("控制台用户 %q 时不该有「被拒」记录，实际 %v", consoleUser, res.Attempted)
		}
		if res.MarkerConsumed {
			t.Errorf("控制台用户 %q 时按需入口不该报告消费标记（它根本没有标记语义）", consoleUser)
		}
	}
}

// TestRequestVolumeAuthorizationNowNoExternalVolumeDoesNotRead：没盘就不读。
func TestRequestVolumeAuthorizationNowNoExternalVolumeDoesNotRead(t *testing.T) {
	calls := stubVolumeAuth(t, "zizdog", nil, nil)

	res := RequestVolumeAuthorizationNow(context.Background(), time.Second, nil)
	if !res.Skipped {
		t.Errorf("没有外接卷时按需入口应 Skipped，实际 %+v", res)
	}
	if *calls != 0 {
		t.Errorf("没有外接卷时不该读任何东西，实际读了 %d 次", *calls)
	}
}

// TestRequestVolumeAuthorizationNowReadsWhenSomeoneIsPresent：有人在 + 有盘 → 才真的读。
func TestRequestVolumeAuthorizationNowReadsWhenSomeoneIsPresent(t *testing.T) {
	calls := stubVolumeAuth(t, "zizdog", []string{"/Volumes/A", "/Volumes/B"}, nil)

	res := RequestVolumeAuthorizationNow(context.Background(), time.Second, nil)
	if len(res.Okay) != 2 || len(res.Attempted) != 0 {
		t.Errorf("两个卷都读得通时应全部记为 Okay，实际 %+v", res)
	}
	if *calls != 2 {
		t.Errorf("应恰好读两次（每卷一次），实际 %d 次", *calls)
	}
	if res.Skipped || res.MarkerConsumed {
		t.Errorf("按需入口不该有一次性标记语义，实际 %+v", res)
	}
}

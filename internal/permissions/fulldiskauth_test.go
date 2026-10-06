package permissions

// fulldiskauth_test.go —— 「一次性完全磁盘访问授权请求」的门禁。
//
// 与 files.volumeauth_test.go 同一条铁律：**没人在屏幕前时一个字节都不读**
// （弹了没人点，系统只会记成 denial）。这里锁死：
//  ① 没有标记 → 不读；
//  ② 有标记但无图形登录会话 → 不读，且标记保留；
//  ③ 有标记 + 有人在 → 才读一次；读通/被拒都如实记录并消费标记。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"
)

func stubFullDiskAuth(t *testing.T, results []PathResult) *int {
	t.Helper()
	prevProbe := fullDiskProbeFn
	prevFDA := FullDiskAccessProbeFn
	FullDiskAccessProbeFn = func(string) FullDiskProbe { return FullDiskProbe{} }
	calls := 0
	fullDiskProbeFn = func(context.Context, []string) []PathResult {
		calls++
		return results
	}
	t.Cleanup(func() { fullDiskProbeFn = prevProbe; FullDiskAccessProbeFn = prevFDA })
	return &calls
}

func writeFullDiskMarker(t *testing.T, dir string) string {
	t.Helper()
	p := FullDiskAuthMarkerPath(dir)
	if err := os.WriteFile(p, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFullDiskAuthNoMarkerDoesNothing(t *testing.T) {
	dir := t.TempDir()
	calls := stubFullDiskAuth(t, []PathResult{{Path: "/Users/zizdog/Desktop", Readable: true}})

	res := RequestFullDiskAuthorizationOnce(context.Background(), FullDiskAuthMarkerPath(dir),
		"/Users/zizdog", "zizdog", time.Second, nil)

	if !res.Skipped || res.MarkerConsumed {
		t.Errorf("没有标记时必须 Skipped 且不消费标记，实际 %+v", res)
	}
	if *calls != 0 {
		t.Errorf("没有标记时不许读受保护目录，实际读了 %d 次", *calls)
	}
}

func TestFullDiskAuthNoConsoleSessionNeverReads(t *testing.T) {
	for _, consoleUser := range []string{"", "root", "loginwindow"} {
		dir := t.TempDir()
		marker := writeFullDiskMarker(t, dir)
		calls := stubFullDiskAuth(t, nil)

		res := RequestFullDiskAuthorizationOnce(context.Background(), marker, "/Users/zizdog", consoleUser, time.Second, nil)

		if !res.Skipped {
			t.Errorf("控制台用户 %q 时必须 Skipped，实际 %+v", consoleUser, res)
		}
		if *calls != 0 {
			t.Errorf("控制台用户 %q 时绝不许读受保护目录（会触发没人能点的弹窗），实际读了 %d 次",
				consoleUser, *calls)
		}
		if res.MarkerConsumed {
			t.Errorf("控制台用户 %q 时标记必须保留（等人来了再试），却被消费了", consoleUser)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("控制台用户 %q 时标记文件应仍然存在：%v", consoleUser, err)
		}
	}
}

func TestFullDiskAuthReadsOnceAndConsumesMarker(t *testing.T) {
	dir := t.TempDir()
	marker := writeFullDiskMarker(t, dir)
	calls := stubFullDiskAuth(t, []PathResult{
		{Path: "/Users/zizdog/Desktop", Readable: true},
		{Path: "/Users/zizdog/Documents", Denied: true, Reason: "operation not permitted"},
	})

	res := RequestFullDiskAuthorizationOnce(context.Background(), marker, "/Users/zizdog", "zizdog", 0, nil)

	if res.Skipped {
		t.Errorf("有人在场且已确认时必须真的读一次，实际 %+v", res)
	}
	if *calls != 1 {
		t.Errorf("wait=0 时应恰好读一次，实际 %d 次", *calls)
	}
	if res.Readable != 1 || res.Denied != 1 {
		t.Errorf("应如实记录 1 可读 / 1 被拒，实际 %+v", res)
	}
	if !res.MarkerConsumed {
		t.Error("读完必须消费一次性标记")
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("标记应已被删除，实际 err=%v", err)
	}
}

func TestFullDiskAuthNoHomeConsumesMarker(t *testing.T) {
	dir := t.TempDir()
	marker := writeFullDiskMarker(t, dir)
	calls := stubFullDiskAuth(t, nil)

	res := RequestFullDiskAuthorizationOnce(context.Background(), marker, "", "zizdog", time.Second, nil)
	if !res.Skipped || !res.MarkerConsumed {
		t.Errorf("拿不到家目录时应 Skipped 且消费标记，实际 %+v", res)
	}
	if *calls != 0 {
		t.Errorf("没有目标目录时不该读，实际读了 %d 次", *calls)
	}
}

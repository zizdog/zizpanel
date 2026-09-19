package web

// api_disks_custommount_test.go —— 「把卷挂到 <安装根>/mnt 下」这条 API 能力的门禁。
//
// 🚨 这个能力**不是** macOS 隐私保护（TCC）的解法 —— 2026-09-19 实测证伪：
// 把卷挂到 /Volumes 之外后挂载成功，面板读那个路径仍然 operation not permitted
// （TCC 按**卷**判定，与挂载点无关，见 api_files.go 顶部）。前端的
// 「挂载到自定义挂载点…」入口已因此删除，这里只锁后端 API 行为本身。
//
// 安全边界（面板是 root，必须严）：
//   - 只允许 <面板安装根>/mnt/<单个目录名>，别处一律 400 且不执行任何 diskutil 写命令；
//   - 解析软链接后仍必须在这个前缀内（防 symlink 逃逸）；
//   - /Volumes 下的挂载点直接拒绝（那不叫「自定义」位置）。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unmountZPMirror 先把 disk4s1 卸掉，让自定义挂载成为真正的"从 0 到挂上"。
func unmountZPMirror(t *testing.T, ts *httptest.Server, cookies []*http.Cookie) {
	t.Helper()
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/unmount", map[string]any{}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("前置卸载失败 %d: %v", res.StatusCode, out["msg"])
	}
}

// mountCalls 只取挂载写命令（diskutil mount ...），不含 list/info 这类只读枚举。
func (w *fakeWorld) mountCalls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, c := range w.calls {
		if c == "mount" || strings.HasPrefix(c, "mount ") {
			out = append(out, c)
		}
	}
	return out
}

func TestDiskMountAtCustomMountPointReadsBack(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	base := srv.customMountBase()
	target := filepath.Join(base, "ZPMirror")

	unmountZPMirror(t, ts, cookies)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/mount",
		map[string]any{"mount_point": target}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("挂载到自定义挂载点应 200，实际 %d: %v", res.StatusCode, out["msg"])
	}
	d := mapGet(out, "data")
	if asString(mapGet(d, "action")) != "mount" {
		t.Errorf("action 应为 mount，实际 %v", mapGet(d, "action"))
	}
	if mapGet(d, "verified") != true || mapGet(d, "mounted") != true {
		t.Errorf("必须回读确认已挂载，实际 verified=%v mounted=%v",
			mapGet(d, "verified"), mapGet(d, "mounted"))
	}
	// 真实路径（/tmp 在 macOS 上是 /private/tmp 的软链接，后端返回解析后的挂载点）。
	wantMP := target
	if r, err := filepath.EvalSymlinks(target); err == nil {
		wantMP = r
	}
	if got := asString(mapGet(d, "mount_point")); got != wantMP {
		t.Errorf("回读挂载点应为 %q，实际 %q", wantMP, got)
	}
	// 命令必须是 -mountPoint 形态，且确实是这一条被执行过。
	if cmd := asString(mapGet(d, "command")); !strings.Contains(cmd, "-mountPoint") {
		t.Errorf("命令应使用 -mountPoint，实际 %q", cmd)
	}
	if !w.called("mount -mountPoint " + wantMP + " disk4s1") {
		t.Errorf("应真的执行 diskutil mount -mountPoint <dir> disk4s1，实际调用：%v", w.callList())
	}
	// 目录不存在则先建（diskutil 要求挂载点目录已存在）。
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		t.Errorf("挂载点目录应已创建：stat=%v err=%v", st, err)
	}
}

func TestDiskListExposesCustomMountBase(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/disks", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("磁盘列表应 200，实际 %d", res.StatusCode)
	}
	d := mapGet(out, "data")
	if got, want := asString(mapGet(d, "custom_mount_base")), srv.customMountBase(); got != want {
		t.Errorf("custom_mount_base 应为 %q，实际 %q", want, got)
	}
	// panel_binary：外接盘被 TCC 拒绝时，前端要用它告诉用户"去系统设置里给谁授权"。
	// 必须是真实存在的可执行文件路径，不能是猜的默认安装位置。
	bin := asString(mapGet(d, "panel_binary"))
	if !filepath.IsAbs(bin) || filepath.Base(bin) != "zizpanel" {
		t.Errorf("panel_binary 应是绝对路径且以 zizpanel 结尾，实际 %q", bin)
	}
	if want := filepath.Join(srv.Cfg.BinDir, "zizpanel"); bin != want {
		t.Errorf("panel_binary 应来自配置的 BinDir（%q），实际 %q", want, bin)
	}
}

func TestDiskCustomMountPointRejectsOutsideBase(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	base := srv.customMountBase()

	bad := []string{
		"/tmp/zp-evil",                      // 绝对路径但不在安装根下
		"/etc",                              // 系统目录
		base,                                // 前缀本身（必须是它的子目录）
		filepath.Join(base, "..", "escape"), // 规范化后越出
		"/Volumes/ZPMirror",                 // 正是要被绕开的位置
		"relative/path",                     // 非绝对路径
	}
	for _, mp := range bad {
		// 只读枚举（list/info/apfs list）是拒绝路径本身的必要校验；
		// 这里要锁的是"不得执行任何 mount 写命令"。
		before := w.mountCalls()
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/mount",
			map[string]any{"mount_point": mp}, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("挂载点 %q 应 400，实际 %d: %v", mp, res.StatusCode, out["msg"])
		}
		if got := w.mountCalls(); len(got) != len(before) {
			t.Errorf("挂载点 %q 被拒时不得执行 mount，新增调用：%v", mp, got[len(before):])
		}
	}
}

// TestDiskCustomMountPointRejectsSymlinkEscape：前缀内的软链接指向外部时也必须拒绝。
func TestDiskCustomMountPointRejectsSymlinkEscape(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	base := srv.customMountBase()
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(base, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("创建软链接失败（%v），跳过 symlink 逃逸用例", err)
	}

	before := w.mountCalls()
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/mount",
		map[string]any{"mount_point": filepath.Join(link, "sub")}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("symlink 逃逸的挂载点应 400，实际 %d: %v", res.StatusCode, out["msg"])
	}
	if got := w.mountCalls(); len(got) != len(before) {
		t.Errorf("symlink 逃逸被拒时不得执行 mount，新增调用：%v", got[len(before):])
	}
}

// TestDiskUnmountIgnoresMountPoint：unmount 不接受 mount_point（body 里带了也不改行为）。
func TestDiskUnmountIgnoresMountPoint(t *testing.T) {
	w := newFakeWorld()
	_, ts, cookies := newDiskServer(t, w)
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/unmount",
		map[string]any{"mount_point": "/tmp/zp-evil"}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unmount 应忽略 mount_point 并成功，实际 %d: %v", res.StatusCode, out["msg"])
	}
	if w.called("mount") {
		t.Errorf("unmount 不得执行 mount，实际：%v", w.callList())
	}
}

package web

// api_files_tcc_test.go —— 外接卷被 macOS 隐私保护（TCC）拒绝时的错误映射门禁。
//
// 用户报障原文：「打开目录失败: open /Volumes/ZPMirror: operation not permitted」。
// 根因：面板以 root 的 LaunchDaemon 运行、没有用户会话，读写外接盘会被 TCC 以
// EPERM 拒绝，且**系统连询问窗口都不会弹**（Background Session → record_denial）。
// **本机调试实例（make run-local）跑在用户会话里，复现不出这条路径** ——
// 所以这里用**构造的 errno + 注入的挂载点列表**覆盖映射本身。
//
// 要锁死的行为：
//  ① EPERM/EACCES + 路径落在非系统卷上 → 403，错误体含**人工授权**的完整步骤；
//  ② 指引里**不得**再把"换挂载点"写成解法 —— 2026-09-19 实测证伪：
//     挂到 /Volumes 之外后挂载成功，但面板读它仍然 operation not permitted；
//  ③ 同一 errno 但路径不在任何非系统卷上（如 /opt/...、用户目录）→ 不误报成 TCC；
//  ④ 路径在卷上但不是权限错误（ENOENT）→ 不误报成 TCC；
//  ⑤ 文本里写着 "operation not permitted" 但 errno 不是权限错误 → 不靠字符串匹配；
//  ⑥ 挂在**非 /Volumes 位置**的外接盘（用户/面板手工挂载）同样要被识别 ——
//     这正是实验踩到的场景，只认 /Volumes 会让真实错误退化成干巴巴的 errno。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zizdog/zizpanel/internal/config"
)

// withVolumeMounts 把"本机非系统卷挂载点"换成固定列表。
//
// 必须注入：真实实现读的是**跑测试那台机器**（本机就插着一块 4T 外接盘），
// 不注入的话断言会随机器上插没插盘而变。
func withVolumeMounts(t *testing.T, mounts ...string) {
	t.Helper()
	prev := nonSystemVolumeMountsFn
	nonSystemVolumeMountsFn = func() []string { return mounts }
	t.Cleanup(func() { nonSystemVolumeMountsFn = prev })
}

// fileErrResponse 解出 fail() 的错误体。
func fileErrResponse(t *testing.T, rec *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	var body struct {
		OK  bool   `json:"ok"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 JSON: %v（原文 %q）", err, rec.Body.String())
	}
	if body.OK {
		t.Errorf("错误响应的 ok 必须为 false，实际 body=%q", rec.Body.String())
	}
	return rec.Code, body.Msg
}

// assertTCCGuide 断言这是"外接卷被 TCC 拦住"的 403 + 完整指引。
func assertTCCGuide(t *testing.T, rec *httptest.ResponseRecorder, wantPath string, sub string) {
	t.Helper()
	code, msg := fileErrResponse(t, rec)
	if code != http.StatusForbidden {
		t.Errorf("%s：EPERM + 外接卷必须是 403（不是 500/400），实际 %d：%s", sub, code, msg)
	}
	for _, want := range []string{
		wantPath, "隐私保护",
		// 解法必须真的可执行：确切位置 + 权限名，而不是含糊的"去授权"。
		"完全磁盘访问权限", "系统设置", "bin/zizpanel",
		// 2026-09-19 实测到两条路：弹窗点允许 / 自己去系统设置加。**两条都要在**——
		// 只写系统设置会让用户白跑一趟，只写弹窗则在没有 UI 会话的机器上无路可走。
		"点「允许」",
		// 升级会让二进制变化 → 授权可能失效，必须提前说清楚。
		"再授权一次",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s：错误体缺 %q，实际：%s", sub, want, msg)
		}
	}
	// 被实测证伪的说法不许再出现：
	//   · 换挂载点是解法（实测：挂载成功但读卷仍被拒）；
	//   · "连询问窗口都不会弹"（实测 21:15:26 弹了，用户点允许后全通）。
	for _, forbidden := range []string{
		"挂载到自定义挂载点", "mount -mountPoint", "连询问窗口都不会弹",
	} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("%s：指引里不得出现已被实测证伪的说法 %q，实际：%s", sub, forbidden, msg)
		}
	}
}

// TestExternalVolumePermissionMapsTo403Guide 覆盖真实会出现的几种 errno 包装。
func TestExternalVolumePermissionMapsTo403Guide(t *testing.T) {
	withVolumeMounts(t, "/Volumes/ZPMirror")
	cases := []struct {
		name string
		err  error
	}{
		{
			// 报障原文的形态：List 把 os.Open 的错误包成「打开目录失败: %w」。
			name: "List 包装的 open EPERM",
			err: fmt.Errorf("打开目录失败: %w",
				&fs.PathError{Op: "open", Path: "/Volumes/ZPMirror", Err: syscall.EPERM}),
		},
		{
			name: "裸 PathError EPERM",
			err:  &fs.PathError{Op: "open", Path: "/Volumes/ZPMirror", Err: syscall.EPERM},
		},
		{
			name: "EACCES 同样处理",
			err:  &fs.PathError{Op: "open", Path: "/Volumes/ZPMirror", Err: syscall.EACCES},
		},
		{
			// 重命名/复制跨卷时是 *os.LinkError（Old/New 两个路径）。
			name: "LinkError 的 Old 在外接卷",
			err: &os.LinkError{Op: "rename", Old: "/Volumes/ZPMirror/a.txt",
				New: "/tmp/b.txt", Err: syscall.EPERM},
		},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		failFileErr(rec, tc.err)
		assertTCCGuide(t, rec, "/Volumes/ZPMirror", tc.name)
	}
}

// TestVolumePermissionAtRelocatedMountPoint 锁死那次实验的结论：
// 卷被挂到 /Volumes 之外（面板的「自定义挂载点」或用户手工 mount）之后，
// 同样的 EPERM **仍然**是 TCC 问题，必须给出同样完整的指引。
func TestVolumePermissionAtRelocatedMountPoint(t *testing.T) {
	withVolumeMounts(t, "/opt/zizpanel/mnt/ZPMirror", "/mnt/mirror")
	for _, p := range []string{
		"/opt/zizpanel/mnt/ZPMirror",
		"/opt/zizpanel/mnt/ZPMirror/tcc-probe/probe.txt",
		"/mnt/mirror/a/b",
	} {
		rec := httptest.NewRecorder()
		failFileErr(rec, &fs.PathError{Op: "open", Path: p, Err: syscall.EPERM})
		assertTCCGuide(t, rec, filepath.Clean(p), "换挂载点后的 "+p)
	}
}

// TestVolumePermissionWithExplicitRequestPath 覆盖"errno 是真实的、路径只有请求里有"
// 的情形（调用方把请求路径传进 failFileErr）。
func TestVolumePermissionWithExplicitRequestPath(t *testing.T) {
	withVolumeMounts(t, "/Volumes/ZPMirror")
	rec := httptest.NewRecorder()
	// 没有 PathError 的权限错误：只有调用方传进来的请求路径。
	err := fmt.Errorf("打开目录失败: %w", syscall.EPERM)
	failFileErr(rec, err, "/Volumes/ZPMirror")
	assertTCCGuide(t, rec, "/Volumes/ZPMirror", "显式请求路径")
}

// TestPermissionOutsideVolumesIsNotTCC：判据必须有"路径落在非系统卷上"这一条。
func TestPermissionOutsideVolumesIsNotTCC(t *testing.T) {
	withVolumeMounts(t, "/Volumes/ZPMirror")
	for _, p := range []string{
		"/opt/zizpanel/data/config.json",
		"/Users/someone/www/index.php",
		"/Volumes2/ZPMirror",            // 裸前缀匹配会把它误判成 /Volumes 下
		"/opt/zizpanel/mnt/ZPMirror2/x", // 同前缀但不是那个挂载点
		"/Volumes/ZPMirror-backup/x",    // 卷名的前缀不算在里面
		"/",                             // 根目录不是外接卷
	} {
		rec := httptest.NewRecorder()
		failFileErr(rec, &fs.PathError{Op: "open", Path: p, Err: syscall.EPERM})
		code, msg := fileErrResponse(t, rec)
		if code == http.StatusForbidden && strings.Contains(msg, "隐私保护") {
			t.Errorf("%s：非外接卷的 EPERM 不得误报成 TCC，实际 %d：%s", p, code, msg)
		}
		if strings.Contains(msg, "完全磁盘访问权限") {
			t.Errorf("%s：普通权限错误里不应出现 TCC 指引，实际：%s", p, msg)
		}
	}
}

// TestNonPermissionErrorOnVolumesIsNotTCC：路径在卷上但 errno 不是权限错误。
func TestNonPermissionErrorOnVolumesIsNotTCC(t *testing.T) {
	withVolumeMounts(t, "/Volumes/ZPMirror")
	rec := httptest.NewRecorder()
	failFileErr(rec, &fs.PathError{Op: "open", Path: "/Volumes/ZPMirror", Err: syscall.ENOENT})
	code, msg := fileErrResponse(t, rec)
	if code == http.StatusForbidden && strings.Contains(msg, "隐私保护") {
		t.Errorf("ENOENT 不是权限错误，不得报成 TCC，实际 %d：%s", code, msg)
	}
	if strings.Contains(msg, "完全磁盘访问权限") {
		t.Errorf("ENOENT 不应带 TCC 指引：%s", msg)
	}
}

// TestTCCJudgementDoesNotMatchStrings：错误文本里写着 "operation not permitted"
// 但 errno 不是权限错误（或没有 errno）时，绝不能靠字符串猜。
func TestTCCJudgementDoesNotMatchStrings(t *testing.T) {
	withVolumeMounts(t, "/Volumes/ZPMirror")
	plain := errors.New("open /Volumes/ZPMirror: operation not permitted")
	rec := httptest.NewRecorder()
	failFileErr(rec, plain)
	if p, ok := volumeTCCPath(plain); ok {
		t.Errorf("纯文本错误不得被判成 TCC（判据必须是真实 errno），却返回了路径 %q", p)
	}
	code, msg := fileErrResponse(t, rec)
	if strings.Contains(msg, "完全磁盘访问权限") {
		t.Errorf("纯文本通知不应被当成 TCC：%d %s", code, msg)
	}
}

// TestExplicitVolumePathDoesNotOverrideTheRealErrorPath：真正出错的是普通目录、
// 只是请求里另一个参数恰好是外接卷时，不能张冠李戴报成 TCC
// （例如"把文件从用户目录重命名进外接卷"，失败在源路径上）。
func TestExplicitVolumePathDoesNotOverrideTheRealErrorPath(t *testing.T) {
	withVolumeMounts(t, "/Volumes/ZPMirror")
	rec := httptest.NewRecorder()
	err := &fs.PathError{Op: "rename", Path: "/Users/someone/www/a.txt", Err: syscall.EPERM}
	failFileErr(rec, err, "/Users/someone/www/a.txt", "/Volumes/ZPMirror/a.txt")
	code, msg := fileErrResponse(t, rec)
	if code == http.StatusForbidden && strings.Contains(msg, "隐私保护") {
		t.Errorf("真正出错的是普通目录，不得误报成外接卷 TCC：%d %s", code, msg)
	}
	if strings.Contains(msg, "完全磁盘访问权限") {
		t.Errorf("普通目录的权限错误不应带 TCC 指引：%s", msg)
	}
}

// TestOnNonSystemVolumePathPrefix：按路径分段判断，别把 /Volumes2 与
// "/Volumes/ZPMirror-backup" 这类同前缀路径算进来。
func TestOnNonSystemVolumePathPrefix(t *testing.T) {
	withVolumeMounts(t, "/Volumes/ZPMirror", "/opt/zizpanel/mnt/ZPMirror")
	yes := []string{
		"/Volumes/ZPMirror", "/Volumes/ZPMirror/a/b", "/Volumes/ZPMirror/带空格 的目录",
		"/opt/zizpanel/mnt/ZPMirror", "/opt/zizpanel/mnt/ZPMirror/x",
	}
	no := []string{
		"", ".", "/", "/Volumes", "/Volumes2", "/Volumes/ZPMirror-bak",
		"/opt/zizpanel/mnt", "/opt/zizpanel/mnt/ZPMirror2", "/Users/x/Volumes/ZPMirror",
	}
	for _, p := range yes {
		if !onNonSystemVolume(p) {
			t.Errorf("onNonSystemVolume(%q) 应为 true", p)
		}
	}
	for _, p := range no {
		if onNonSystemVolume(p) {
			t.Errorf("onNonSystemVolume(%q) 应为 false", p)
		}
	}
}

// TestOnNonSystemVolumeUsesRealMountList：真实实现必须能认出**本机真插着**的外接盘。
// 只在真的存在非系统卷时断言（CI/无盘机器上跳过），避免把"环境没有"当成"代码错了"。
func TestOnNonSystemVolumeUsesRealMountList(t *testing.T) {
	prev := nonSystemVolumeMountsFn
	nonSystemVolumeMountsFn = prev
	mounts := prev()
	if len(mounts) == 0 {
		t.Skip("本机当前没有任何非系统卷（未插外接盘），跳过真实探测断言")
	}
	for _, mp := range mounts {
		if !onNonSystemVolume(mp) {
			t.Errorf("真实挂载点 %q 必须被判成非系统卷", mp)
		}
	}
}

// TestFileListPermissionDeniedOutsideVolumesIsNotTCC 走**真实 handler**（不是只测映射函数）：
// 一个不可读的普通目录 → 普通权限错误，绝不能报成 TCC。
// 单测以 root 跑时权限位不起作用，跳过（否则断言无意义）。
func TestFileListPermissionDeniedOutsideVolumesIsNotTCC(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时 0000 目录仍可读，无法构造 EACCES")
	}
	withVolumeMounts(t, "/Volumes/ZPMirror")
	srv, _ := newTestServer(t)
	dir := filepath.Join(srv.Cfg.WWWRoot, "no-read")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/files?path="+dir, nil)
	rec := httptest.NewRecorder()
	srv.handleFileList(rec, req)

	code, msg := fileErrResponse(t, rec)
	if code == http.StatusForbidden && strings.Contains(msg, "隐私保护") {
		t.Errorf("普通目录的 EACCES 不得报成外接卷 TCC 问题：%d %s", code, msg)
	}
}

// TestTCCGuideNamesTheConfiguredBinary：指引里必须写**配置解析出来的**二进制路径。
//
// 这里曾经写死 config.DefaultRoot：默认安装根下恰好正确，非默认安装根（BinDir 配在别处）
// 就会指着一个**不存在的文件**让用户去系统设置里授权 —— 而磁盘页下发的 panel_binary
// 是按配置算的，同一事实两个来源必然分叉。实测复验时就是在这个自带实例上发现的。
func TestTCCGuideNamesTheConfiguredBinary(t *testing.T) {
	withVolumeMounts(t, "/Volumes/ZPMirror")
	prev := panelBinaryForGuide
	panelBinaryForGuide = "/opt/custom-root/bin/zizpanel"
	t.Cleanup(func() { panelBinaryForGuide = prev })

	rec := httptest.NewRecorder()
	failFileErr(rec, &fs.PathError{Op: "open", Path: "/Volumes/ZPMirror", Err: syscall.EPERM})
	_, msg := fileErrResponse(t, rec)
	if !strings.Contains(msg, panelBinaryForGuide) {
		t.Errorf("指引应使用配置解析出的二进制路径 %q，实际：%s", panelBinaryForGuide, msg)
	}
	if strings.Contains(msg, config.DefaultRoot+"/bin/zizpanel") {
		t.Errorf("指引不得写死默认安装根路径，实际：%s", msg)
	}
}

// TestSetPanelBinaryForGuide：按 BinDir 解析；空值不许把它清成空字符串。
func TestSetPanelBinaryForGuide(t *testing.T) {
	prev := panelBinaryForGuide
	t.Cleanup(func() { panelBinaryForGuide = prev })

	setPanelBinaryForGuide("/opt/custom/bin/")
	if want := "/opt/custom/bin/zizpanel"; panelBinaryForGuide != want {
		t.Errorf("setPanelBinaryForGuide 应为 %q，实际 %q", want, panelBinaryForGuide)
	}
	setPanelBinaryForGuide("   ")
	if panelBinaryForGuide == "" {
		t.Error("空 BinDir 不许把路径清空（否则指引里会出现空路径）")
	}
}

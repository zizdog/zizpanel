package web

// api_files_tcc_test.go —— 外接卷被 macOS 隐私保护（TCC）拒绝时的错误映射门禁。
//
// 用户报障原文：「打开目录失败: open /Volumes/ZPMirror: operation not permitted」。
// 根因：面板以 root 的 LaunchDaemon 运行、没有用户会话，读写 /Volumes 下的外接盘
// 会被 TCC 以 EPERM 拒绝。**本机调试实例（make run-local）跑在用户会话里，
// 复现不出这条路径** —— 所以这里用**构造的 errno** 覆盖映射本身。
//
// 要锁死的行为：
//  ① EPERM/EACCES + /Volumes 路径 → 403，错误体含「挂载到自定义挂载点」的解法；
//  ② 同一 errno 但路径不在 /Volumes（如 /opt/...）→ 不误报成 TCC；
//  ③ 路径在 /Volumes 但不是权限错误（ENOENT）→ 不误报成 TCC；
//  ④ 文本里写着 "operation not permitted" 但 errno 不是权限错误 → 不靠字符串匹配；
//  ⑤ 给用户的指引里**不得**出现"完全磁盘访问权限/人工授权"（无头机器做不到）。

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
)

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

// assertTCCGuide 断言这是"外接卷被 TCC 拦住"的 403 + 完整解法。
func assertTCCGuide(t *testing.T, rec *httptest.ResponseRecorder, wantPath string, sub string) {
	t.Helper()
	code, msg := fileErrResponse(t, rec)
	if code != http.StatusForbidden {
		t.Errorf("%s：EPERM + /Volumes 必须是 403（不是 500/400），实际 %d：%s", sub, code, msg)
	}
	for _, want := range []string{wantPath, "隐私保护", "挂载到自定义挂载点", "diskutil mount -mountPoint"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s：错误体缺 %q，实际：%s", sub, want, msg)
		}
	}
	// 新硬约束：不能要求人工授权（无头机器做不到），指引里不许出现这条。
	for _, forbidden := range []string{"完全磁盘访问权限", "隐私与安全性", "系统设置"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("%s：指引里不得出现人工授权路径 %q，实际：%s", sub, forbidden, msg)
		}
	}
}

// TestExternalVolumePermissionMapsTo403Guide 覆盖真实会出现的几种 errno 包装。
func TestExternalVolumePermissionMapsTo403Guide(t *testing.T) {
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
			name: "LinkError 的 Old 在 /Volumes",
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

// TestVolumePermissionWithExplicitRequestPath 覆盖"errno 是真实的、路径只有请求里有"
// 的情形（调用方把请求路径传进 failFileErr）。
func TestVolumePermissionWithExplicitRequestPath(t *testing.T) {
	rec := httptest.NewRecorder()
	// 没有 PathError 的权限错误：只有调用方传进来的请求路径。
	err := fmt.Errorf("打开目录失败: %w", syscall.EPERM)
	failFileErr(rec, err, "/Volumes/ZPMirror")
	assertTCCGuide(t, rec, "/Volumes/ZPMirror", "显式请求路径")

	// 请求路径就是 /Volumes 本身也算命中。
	rec = httptest.NewRecorder()
	failFileErr(rec, err, "/Volumes")
	assertTCCGuide(t, rec, "/Volumes", "/Volumes 根")
}

// TestPermissionOutsideVolumesIsNotTCC：判据必须有"路径在 /Volumes 下"这一条。
func TestPermissionOutsideVolumesIsNotTCC(t *testing.T) {
	for _, p := range []string{
		"/opt/zizpanel/data/config.json",
		"/Users/someone/www/index.php",
		"/Volumes2/ZPMirror", // 裸前缀匹配会把它误判成 /Volumes 下
	} {
		rec := httptest.NewRecorder()
		failFileErr(rec, &fs.PathError{Op: "open", Path: p, Err: syscall.EPERM})
		code, msg := fileErrResponse(t, rec)
		if code == http.StatusForbidden && strings.Contains(msg, "隐私保护") {
			t.Errorf("%s：非 /Volumes 的 EPERM 不得误报成 TCC，实际 %d：%s", p, code, msg)
		}
		if strings.Contains(msg, "挂载到自定义挂载点") {
			t.Errorf("%s：普通权限错误里不应出现 TCC 解法，实际：%s", p, msg)
		}
	}
}

// TestNonPermissionErrorOnVolumesIsNotTCC：路径在 /Volumes 但 errno 不是权限错误。
func TestNonPermissionErrorOnVolumesIsNotTCC(t *testing.T) {
	rec := httptest.NewRecorder()
	failFileErr(rec, &fs.PathError{Op: "open", Path: "/Volumes/ZPMirror", Err: syscall.ENOENT})
	code, msg := fileErrResponse(t, rec)
	if code == http.StatusForbidden && strings.Contains(msg, "隐私保护") {
		t.Errorf("ENOENT 不是权限错误，不得报成 TCC，实际 %d：%s", code, msg)
	}
	if strings.Contains(msg, "挂载到自定义挂载点") {
		t.Errorf("ENOENT 不应带 TCC 解法：%s", msg)
	}
}

// TestTCCJudgementDoesNotMatchStrings：错误文本里写着 "operation not permitted"
// 但 errno 不是权限错误（或没有 errno）时，绝不能靠字符串猜。
func TestTCCJudgementDoesNotMatchStrings(t *testing.T) {
	rec := httptest.NewRecorder()
	failFileErr(rec, errors.New("open /Volumes/ZPMirror: operation not permitted"))
	if p, ok := volumeTCCPath(errors.New("open /Volumes/ZPMirror: operation not permitted")); ok {
		t.Errorf("纯文本错误不得被判成 TCC（判据必须是真实 errno），却返回了路径 %q", p)
	}
	code, msg := fileErrResponse(t, rec)
	if strings.Contains(msg, "挂载到自定义挂载点") {
		t.Errorf("纯文本通知不应被当成 TCC：%d %s", code, msg)
	}
}

// TestExplicitVolumePathDoesNotOverrideTheRealErrorPath：真正出错的是普通目录、
// 只是请求里另一个参数恰好是 /Volumes 时，不能张冠李戴报成 TCC
// （例如"把文件从用户目录重命名进外接卷"，失败在源路径上）。
func TestExplicitVolumePathDoesNotOverrideTheRealErrorPath(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &fs.PathError{Op: "rename", Path: "/Users/someone/www/a.txt", Err: syscall.EPERM}
	failFileErr(rec, err, "/Users/someone/www/a.txt", "/Volumes/ZPMirror/a.txt")
	code, msg := fileErrResponse(t, rec)
	if code == http.StatusForbidden && strings.Contains(msg, "隐私保护") {
		t.Errorf("真正出错的是普通目录，不得误报成外接卷 TCC：%d %s", code, msg)
	}
	if strings.Contains(msg, "挂载到自定义挂载点") {
		t.Errorf("普通目录的权限错误不应带 TCC 解法：%s", msg)
	}
}

// TestWithinVolumesPathPrefix：按路径分段判断，别把 /Volumes2 算进来。
func TestWithinVolumesPathPrefix(t *testing.T) {
	yes := []string{"/Volumes", "/Volumes/ZPMirror", "/Volumes/a/b", "/Volumes/带空格 的卷"}
	no := []string{"", ".", "/", "/Volumes2", "/VolumesX/y", "/volumes/z", "/Users/x/Volumes"}
	for _, p := range yes {
		if !withinVolumes(p) {
			t.Errorf("withinVolumes(%q) 应为 true", p)
		}
	}
	for _, p := range no {
		if withinVolumes(p) {
			t.Errorf("withinVolumes(%q) 应为 false", p)
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

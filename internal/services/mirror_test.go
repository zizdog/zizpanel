package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  应用包镜像（镜像站）的契约
//
//  政策（用户原话，2026-09-16 最终版）：
//    "对所有能用到的模型、软件，如 ffmpeg，都要以镜像站优先，不通再走别的！"
//  下面这些断言把这句话变成代码能守住的东西：
//    · 布局固定为 <base>/apps/<app-id>/<版本>/<文件名>（与同步工具一致）；
//    · 镜像上有这个包 → 镜像排在下载候选第一位（并且**保留**公网回落候选）；
//    · 镜像上缺包/不可达 → 自动回落到公网源，绝不让安装直接失败；
//    · 缺包的错误信息仍要说清怎么补（make sync-apps），否则用户不知道镜像漏了；
//    · 走镜像时校验用**镜像清单**里的 sha256（Orbien 上游根本没有 checksums 文件）。
//
//  注：本文件头原先写"镜像生效时候选里绝不出现 github.com / 缺包就明确失败"，
//  那是更早一版"唯一来源"的需求，与现在代码和断言都相反，已按政策纠正。
// ============================================================================

// TestMirrorAssetURLs 锁住镜像布局与基址规范化。
func TestMirrorAssetURLs(t *testing.T) {
	m := &Manager{opt: Options{MirrorBase: "https://mirror.example.com:8888/"}}

	if got, want := m.appAssetURL("lucky", "v2.27.2", "lucky_2.27.2_darwin_arm64.tar.gz"),
		"https://mirror.example.com:8888/apps/lucky/v2.27.2/lucky_2.27.2_darwin_arm64.tar.gz"; got != want {
		t.Errorf("应用包地址不对：\n got %s\nwant %s", got, want)
	}
	if got, want := m.appManifestURL("lucky", "v2.27.2"),
		"https://mirror.example.com:8888/apps/lucky/v2.27.2/manifest.json"; got != want {
		t.Errorf("清单地址不对：\n got %s\nwant %s", got, want)
	}
	if got, want := m.mirrorSubPath("pypi/simple"), "https://mirror.example.com:8888/pypi/simple"; got != want {
		t.Errorf("其它来源子路径不对：\n got %s\nwant %s", got, want)
	}
	if !m.MirrorEnabled() {
		t.Error("配了基址就该报告镜像已启用")
	}
	if (&Manager{}).MirrorEnabled() {
		t.Error("基址为空时镜像应处于关闭状态（应急回退路径）")
	}
}

// TestReleaseBinaryDownloadURLsPreferMirror 是用户需求的核心断言：
// **镜像优先**，且镜像不可达时要能回落到原来的公网候选（不能因为镜像挂了就装不上）。
func TestReleaseBinaryDownloadURLsPreferMirror(t *testing.T) {
	// 用 frpc 当样例（2026-09-16 后注册表里只剩两个客户端）。
	spec, ok := releaseBinaryApps["frpc"]
	if !ok {
		t.Fatal("注册表里应有 frpc")
	}

	// 1) 镜像上有这个包 → 镜像排在第一位
	mux := http.NewServeMux()
	okPath := "/apps/" + spec.ID + "/" + spec.Tag + "/" + spec.Asset
	mux.HandleFunc(okPath, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	ts := httptest.NewServer(mux)
	defer ts.Close()
	m := &Manager{opt: Options{MirrorBase: ts.URL}}
	urls := m.downloadURLsFor(context.Background(), spec)
	if len(urls) < 2 {
		t.Fatalf("镜像可用时也应保留公网回落候选，实际 %v", urls)
	}
	if want := m.appAssetURL(spec.ID, spec.Tag, spec.Asset); urls[0] != want {
		t.Errorf("镜像可用时第一位应是镜像：\n got %s\nwant %s", urls[0], want)
	}
	if !strings.Contains(urls[0], "/apps/"+spec.ID+"/"+spec.Tag+"/") {
		t.Errorf("地址要走 apps/<app>/<版本>/ 布局：%s", urls[0])
	}
	if !strings.HasPrefix(urls[1], "https://github.com/") {
		t.Errorf("第二位应是官方源（回落顺序不变），实际 %s", urls[1])
	}

	// 2) 镜像上没有这个包（404）→ 直接回落公网，第一位不是镜像
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	ts2 := httptest.NewServer(mux2)
	defer ts2.Close()
	m2 := &Manager{opt: Options{MirrorBase: ts2.URL}}
	fallback := m2.downloadURLsFor(context.Background(), spec)
	if strings.HasPrefix(fallback[0], ts2.URL) {
		t.Errorf("镜像上缺包时不该把镜像排在第一位：%v", fallback)
	}
	if !strings.HasPrefix(fallback[0], "https://github.com/") {
		t.Errorf("回落时应从官方源开始，实际 %s", fallback[0])
	}

	// 3) 关掉镜像 → 原行为（官方 + 第三方加速）
	off := (&Manager{}).downloadURLsFor(context.Background(), spec)
	if len(off) < 2 || !strings.HasPrefix(off[0], "https://github.com/") {
		t.Errorf("关闭镜像时应回到原有候选列表（官方优先），实际 %v", off)
	}
}

// TestCheckMirrorURLTellsHowToFix 镜像上没有资源时，报错必须**说清怎么补**。
//
// （这个函数本身只负责"如实报告"，是否回退由调用方决定：安装流程会回落到公网，
// 但错误信息仍要告诉用户"镜像上缺这个包、可以怎么补"，否则他永远不知道镜像漏了。）
func TestCheckMirrorURLTellsHowToFix(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/lucky/v2.27.2/ok.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	m := &Manager{opt: Options{MirrorBase: ts.URL}}

	if err := m.checkMirrorURL(context.Background(), ts.URL+"/apps/lucky/v2.27.2/ok.tar.gz"); err != nil {
		t.Errorf("存在且 200 时不该报错：%v", err)
	}
	err := m.checkMirrorURL(context.Background(), ts.URL+"/apps/lucky/v2.27.2/missing.tar.gz")
	if err == nil {
		t.Fatal("镜像上没有这个包时必须报错（绝不静默回退到公网）")
	}
	if !strings.Contains(err.Error(), "make sync-apps") {
		t.Errorf("错误里要告诉用户怎么把包补上，实际：%v", err)
	}
	if !strings.Contains(err.Error(), ts.URL) {
		t.Errorf("错误里要带上具体地址，实际：%v", err)
	}
}

// TestPreflightMirrorAssetFallsBackWithoutManifest：
// 包在、清单不在时 **不阻塞安装**，而是回落公网（清单是镜像校验 sha256 的来源，
// 缺了它只是没法用镜像校验，不该让用户装不上）。
func TestPreflightMirrorAssetFallsBackWithoutManifest(t *testing.T) {
	spec := releaseBinaryApps["frpc"]
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/frpc/"+spec.Tag+"/"+spec.Asset, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	m := &Manager{opt: Options{MirrorBase: ts.URL}}
	res := &InstallResult{Steps: []string{}}

	if got := m.preflightMirrorAsset(context.Background(), spec, res); got != "" {
		t.Errorf("包在但清单不在时不该判定为走镜像（拿不到期望 sha256），实际返回 %q", got)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "改用公网源") {
		t.Errorf("要如实告诉用户这次改走了公网源、为什么，实际步骤：%q", joined)
	}
}

// TestPreflightMirrorAssetUsesMirrorWhenBothPresent：包与清单都在 → 走镜像。
func TestPreflightMirrorAssetUsesMirrorWhenBothPresent(t *testing.T) {
	spec := releaseBinaryApps["frpc"]
	mux := http.NewServeMux()
	for _, p := range []string{
		"/apps/frpc/" + spec.Tag + "/" + spec.Asset,
		"/apps/frpc/" + spec.Tag + "/manifest.json",
	} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()
	m := &Manager{opt: Options{MirrorBase: ts.URL}}
	res := &InstallResult{Steps: []string{}}
	got := m.preflightMirrorAsset(context.Background(), spec, res)
	if got == "" {
		t.Errorf("包与清单都在时应走镜像，步骤：%q", strings.Join(res.Steps, "\n"))
	}
	// 返回的必须就是**它探测过的那个包地址** —— 只有这样执行器把它插到
	// 候选第一位时，"探的"与"下的"才不可能指向两个地方。
	want := ts.URL + "/apps/frpc/" + spec.Tag + "/" + spec.Asset
	if got != want {
		t.Errorf("预检必须返回它 HEAD 成功的那个地址：got %q，want %q", got, want)
	}
}

// TestVerifyMirrorChecksum 用镜像清单核对产物：对得上就过，清单里没有这个包就中止。
func TestVerifyMirrorChecksum(t *testing.T) {
	dir := t.TempDir()
	// 用 frpc 当样例（2026-09-16 后注册表里只剩两个客户端）。
	asset := "frp_0.71.0_darwin_arm64.tar.gz"
	path := filepath.Join(dir, asset)
	payload := []byte("fake tarball for test\n")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)

	body, _ := json.Marshal(mirrorManifest{
		App: "frpc", Version: "v0.71.0",
		Assets: []mirrorAsset{{Name: asset, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload))}},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/frpc/v0.71.0/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	spec := releaseBinaryApps["frpc"]
	m := &Manager{opt: Options{MirrorBase: ts.URL}}
	res := &InstallResult{App: "frpc", Steps: []string{}}
	if err := m.verifyMirrorChecksum(context.Background(), spec, binaryReleasePaths{Asset: path}, res); err != nil {
		t.Fatalf("清单里的 sha256 与实际文件一致时应通过，实际：%v", err)
	}
	// 校验过了要有可读的步骤记录（任务中心里用户看得见）
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "SHA-256 校验通过") {
		t.Errorf("任务日志里要如实写出校验结果，实际：%q", joined)
	}

	// 清单里没有这个包 → 中止并提示重新同步
	empty, _ := json.Marshal(mirrorManifest{App: "lucky", Version: "v2.27.2"})
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(empty)
	}))
	defer ts2.Close()
	m2 := &Manager{opt: Options{MirrorBase: ts2.URL}}
	err := m2.verifyMirrorChecksum(context.Background(), spec, binaryReleasePaths{Asset: path}, &InstallResult{Steps: []string{}})
	if err == nil {
		t.Fatal("清单里查不到这个包时必须中止（不能跳过校验）")
	}
	if !strings.Contains(err.Error(), "make sync-apps") {
		t.Errorf("错误要给出补救办法，实际：%v", err)
	}
}

// TestMirrorChecksumRejectsMismatch 坏文件必须被拦住（镜像清单模式下同样如此）。
func TestMirrorChecksumRejectsMismatch(t *testing.T) {
	dir := t.TempDir()
	// 用 frpc 当样例（2026-09-16 后注册表里只剩两个客户端）。
	asset := "frp_0.71.0_darwin_arm64.tar.gz"
	path := filepath.Join(dir, asset)
	if err := os.WriteFile(path, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(mirrorManifest{
		App: "frpc", Version: "v0.71.0",
		Assets: []mirrorAsset{{Name: asset, SHA256: strings.Repeat("ab", 32)}},
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	m := &Manager{opt: Options{MirrorBase: ts.URL}}
	err := m.verifyMirrorChecksum(context.Background(), releaseBinaryApps["frpc"],
		binaryReleasePaths{Asset: path}, &InstallResult{Steps: []string{}})
	if err == nil {
		t.Fatal("sha256 不一致必须中止安装")
	}
	if !strings.Contains(err.Error(), "SHA-256 校验不通过") {
		t.Errorf("错误信息应说明是校验不通过，实际：%v", err)
	}
}

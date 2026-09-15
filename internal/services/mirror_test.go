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
//  应用包镜像（NAS）的契约
//
//  用户要求（2026-09-16）："所有安装过程先检查镜像的资源能不能访问，不能再走其它。"
//  下面这些断言把这些话变成代码能守住的东西：
//    · 布局固定为 <base>/apps/<app-id>/<版本>/<文件名>（与同步工具一致）；
//    · 镜像生效时下载候选**只有镜像**，绝不出现 github.com / ghfast.top / gh-proxy.com；
//    · 镜像上没有资源时**明确失败并说清怎么补**（make sync-apps），不静默回退；
//    · 校验用镜像清单里的 sha256（Lucky/Orbien 上游根本没有 checksums 文件）。
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

// TestReleaseBinaryDownloadURLsAreMirrorOnly 是用户需求的核心断言：
// 镜像生效时**只有一个**下载候选，且不能是任何第三方地址。
func TestReleaseBinaryDownloadURLsAreMirrorOnly(t *testing.T) {
	spec, ok := releaseBinaryApps["lucky"]
	if !ok {
		t.Fatal("注册表里应有 lucky")
	}
	m := &Manager{opt: Options{MirrorBase: "https://mirror.example.com:8888"}}
	urls := m.downloadURLsFor(spec)
	if len(urls) != 1 {
		t.Fatalf("镜像生效时只允许一个下载地址，实际 %d 个：%v", len(urls), urls)
	}
	if want := m.appAssetURL(spec.ID, spec.Tag, spec.Asset); urls[0] != want {
		t.Errorf("下载地址应是镜像地址：\n got %s\nwant %s", urls[0], want)
	}
	for _, bad := range []string{"github.com", "ghfast.top", "gh-proxy.com"} {
		if strings.Contains(urls[0], bad) {
			t.Errorf("镜像模式下不能出现第三方地址 %q：%s", bad, urls[0])
		}
	}
	if !strings.Contains(urls[0], "/apps/lucky/"+spec.Tag+"/") {
		t.Errorf("地址要走 apps/<app>/<版本>/ 布局：%s", urls[0])
	}

	// 关掉镜像时保持原行为（应急路径：官方 + 第三方加速）
	off := (&Manager{}).downloadURLsFor(spec)
	if len(off) < 2 || !strings.HasPrefix(off[0], "https://github.com/") {
		t.Errorf("关闭镜像时应回到原有候选列表（官方优先），实际 %v", off)
	}
}

// TestCheckMirrorURLTellsHowToFix 镜像上没有资源时必须明确失败**并说清怎么补**，
// 因为按需求不回退公网 —— 用户得知道该去 NAS 上做什么。
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

// TestPreflightMirrorAssetRequiresPackageAndManifest：
// 包在、清单不在也要拦下（清单是校验 sha256 的唯一来源，缺了就没法校验）。
func TestPreflightMirrorAssetRequiresPackageAndManifest(t *testing.T) {
	spec := releaseBinaryApps["lucky"]
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/lucky/"+spec.Tag+"/"+spec.Asset, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	m := &Manager{opt: Options{MirrorBase: ts.URL}}

	err := m.preflightMirrorAsset(context.Background(), spec, &InstallResult{Steps: []string{}})
	if err == nil {
		t.Fatal("有包但没清单时必须拦下（否则安装无法校验）")
	}
	if !strings.Contains(err.Error(), "make sync-apps") {
		t.Errorf("错误要给出补救办法，实际：%v", err)
	}
}

// TestVerifyMirrorChecksum 用镜像清单核对产物：对得上就过，清单里没有这个包就中止。
func TestVerifyMirrorChecksum(t *testing.T) {
	dir := t.TempDir()
	asset := "lucky_2.27.2_darwin_arm64.tar.gz"
	path := filepath.Join(dir, asset)
	payload := []byte("fake tarball for test\n")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)

	body, _ := json.Marshal(mirrorManifest{
		App: "lucky", Version: "v2.27.2",
		Assets: []mirrorAsset{{Name: asset, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload))}},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/lucky/v2.27.2/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	spec := releaseBinaryApps["lucky"]
	m := &Manager{opt: Options{MirrorBase: ts.URL}}
	res := &InstallResult{App: "lucky", Steps: []string{}}
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
	asset := "lucky_2.27.2_darwin_arm64.tar.gz"
	path := filepath.Join(dir, asset)
	if err := os.WriteFile(path, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(mirrorManifest{
		App: "lucky", Version: "v2.27.2",
		Assets: []mirrorAsset{{Name: asset, SHA256: strings.Repeat("ab", 32)}},
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	m := &Manager{opt: Options{MirrorBase: ts.URL}}
	err := m.verifyMirrorChecksum(context.Background(), releaseBinaryApps["lucky"],
		binaryReleasePaths{Asset: path}, &InstallResult{Steps: []string{}})
	if err == nil {
		t.Fatal("sha256 不一致必须中止安装")
	}
	if !strings.Contains(err.Error(), "SHA-256 校验不通过") {
		t.Errorf("错误信息应说明是校验不通过，实际：%v", err)
	}
}

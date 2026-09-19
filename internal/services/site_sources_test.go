package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  一键建站源码包镜像（Typecho / WordPress）的契约
//
//  缺陷 D34：原先只有"官方 + 一个写死的镜像"，不校验哈希、不上报来源；
//  实测发现 Typecho 的 jsdelivr 备用地址早已 404、WordPress 两个源都通。
//  下面这些断言把用户要求变成代码能守住的东西：
//    · 版本**写死**，绝不出现 latest（否则"昨天能装、今天装不上"）；
//    · sha256 是 64 位十六进制，且不指向 jsdelivr 那种实测失效的地址；
//    · 镜像站探通 → 镜像排第一；探不通 → 回落官方并把原因写进日志；
//    · 校验不通过必然删文件，绝不留下坏包；
//    · 单测全程走 httptest / 注入点，不碰真实镜像站、不碰真实网络。
// ============================================================================

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestSiteSourceRegistryPinnedRealHashes 锁住"固定版本 + 真实哈希 + 地址可用"。
func TestSiteSourceRegistryPinnedRealHashes(t *testing.T) {
	for _, id := range []string{"typecho", "wordpress"} {
		src, ok := SiteSourceFor(id)
		if !ok {
			t.Fatalf("注册表里缺少 %s", id)
		}
		if src.Version == "" || strings.EqualFold(src.Version, "latest") {
			t.Errorf("%s 的版本必须是写死的具体版本，实际 %q", id, src.Version)
		}
		if len(src.SHA256) != 64 {
			t.Errorf("%s 的 sha256 必须是 64 位十六进制，实际 %q", id, src.SHA256)
		}
		if strings.ToLower(src.SHA256) != src.SHA256 {
			t.Errorf("%s 的 sha256 应为小写：%q", id, src.SHA256)
		}
		if src.Size <= 0 {
			t.Errorf("%s 缺少实测大小", id)
		}
		if len(src.Upstreams) == 0 {
			t.Fatalf("%s 至少要有一个官方回落地址", id)
		}
		for _, u := range src.Upstreams {
			if strings.Contains(u, "/latest") || strings.Contains(u, "latest/download") {
				t.Errorf("%s 的地址不能追 latest：%s", id, u)
			}
			// jsdelivr@master 是"源码包"而不是 release 产物，实测 404 且内容不同；
			// 放进来会 100% 校验失败，还会让用户以为"回落了备源却失败"。
			if strings.Contains(u, "jsdelivr") {
				t.Errorf("%s 不该再使用 jsdelivr 作为备源（实测 404）：%s", id, u)
			}
		}
		if !strings.Contains(src.Upstreams[0], src.Version) {
			t.Errorf("%s 的首选地址应包含固定版本 %s：%s", id, src.Version, src.Upstreams[0])
		}
	}
}

// TestSitePackageSourcesMirrorFirst：镜像站探得通 → 镜像排第一，并如实标注。
func TestSitePackageSourcesMirrorFirst(t *testing.T) {
	src, _ := SiteSourceFor("typecho")
	m := &Manager{opt: Options{MirrorBase: "https://mirror.example.com:8888"}}
	m.mirrorFileProbeOverride = func(_ context.Context, url string) (int64, error) {
		if !strings.HasSuffix(url, "/sites/typecho/1.3.0/typecho.zip") {
			t.Errorf("探测地址不符合 sites/<app>/<版本>/<文件> 布局：%s", url)
		}
		return 578232, nil
	}
	var logs []string
	srcs := m.sitePackageSources(context.Background(), src, func(s string) { logs = append(logs, s) })
	if len(srcs) < 2 {
		t.Fatalf("镜像可用时也必须保留官方回落候选，实际 %v", srcs)
	}
	want := "https://mirror.example.com:8888/sites/typecho/1.3.0/typecho.zip"
	if srcs[0].URL != want {
		t.Errorf("镜像可用时第一位应是镜像：\n got %s\nwant %s", srcs[0].URL, want)
	}
	if srcs[0].Label != "镜像站" {
		t.Errorf("第一位应标注为镜像站，实际 %q", srcs[0].Label)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "镜像站上有") {
		t.Errorf("日志要如实写出走了镜像，实际：%q", joined)
	}
}

// TestSitePackageSourcesFallbackWhenMirrorMissing：镜像上没有/探不通 → 回落官方，
// 而且第一位**不能**是镜像地址（否则等于把坏地址当成可用源）。
func TestSitePackageSourcesFallbackWhenMirrorMissing(t *testing.T) {
	src, _ := SiteSourceFor("wordpress")
	m := &Manager{opt: Options{MirrorBase: "https://mirror.example.com:8888"}}
	m.mirrorFileProbeOverride = func(context.Context, string) (int64, error) {
		return -1, os.ErrNotExist
	}
	var logs []string
	srcs := m.sitePackageSources(context.Background(), src, func(s string) { logs = append(logs, s) })
	if len(srcs) == 0 {
		t.Fatal("回落时也要有官方候选")
	}
	if strings.HasPrefix(srcs[0].URL, "https://mirror.example.com") {
		t.Errorf("镜像探不通时不该把镜像排在第一位：%v", srcs)
	}
	if srcs[0].URL != src.Upstreams[0] {
		t.Errorf("回落时应从官方源开始，实际 %s", srcs[0].URL)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "回落到官方源") {
		t.Errorf("要如实告诉用户这次走了官方、为什么，实际：%q", joined)
	}
}

// TestSitePackageSourcesMirrorDisabled：关掉镜像（基址为空）→ 直接用官方。
func TestSitePackageSourcesMirrorDisabled(t *testing.T) {
	src, _ := SiteSourceFor("typecho")
	m := &Manager{}
	var logs []string
	srcs := m.sitePackageSources(context.Background(), src, func(s string) { logs = append(logs, s) })
	if len(srcs) == 0 || srcs[0].URL != src.Upstreams[0] {
		t.Errorf("关闭镜像时应直接用官方源，实际 %v", srcs)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "未启用镜像站") {
		t.Errorf("要如实说明未启用镜像，实际：%q", logs)
	}
}

// TestFetchVerifiedSitePackageRejectsMismatchAndDeletes 是本任务最关键的断言：
// 哈希不符 → 删文件 + 如实失败，绝不留下坏包。
func TestFetchVerifiedSitePackageRejectsMismatchAndDeletes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is NOT the pinned artifact"))
	}))
	defer ts.Close()

	src := SiteSource{
		App: "fake", Name: "Fake", Version: "1.0",
		File: "fake.zip", SHA256: sha256Hex([]byte("expected bytes")), Size: 14,
	}
	dest := filepath.Join(t.TempDir(), "fake.zip")
	m := &Manager{}
	var logs []string
	_, err := m.fetchVerifiedSitePackage(context.Background(), src,
		[]weightSource{{URL: ts.URL + "/fake.zip", Label: "官方源"}}, dest,
		func(s string) { logs = append(logs, s) })
	if err == nil {
		t.Fatal("sha256 不符必须失败")
	}
	if !strings.Contains(err.Error(), "SHA-256 校验不通过") {
		t.Errorf("错误信息应说明是校验不通过，实际：%v", err)
	}
	if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
		t.Errorf("校验失败后必须删除下载文件，实际 stat err=%v", serr)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "已删除下载文件") {
		t.Errorf("日志要如实写出已删除坏文件，实际：%q", logs)
	}
}

// TestFetchVerifiedSitePackageFallsBackToNextSource：坏源只删自己的产物，
// 继续试下一个源；全部失败才报错。
func TestFetchVerifiedSitePackageFallsBackToNextSource(t *testing.T) {
	payload := []byte("good artifact bytes")
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer good.Close()

	src := SiteSource{
		App: "fake", Name: "Fake", Version: "1.0",
		File: "fake.zip", SHA256: sha256Hex(payload), Size: int64(len(payload)),
	}
	dest := filepath.Join(t.TempDir(), "fake.zip")
	m := &Manager{}
	var logs []string
	used, err := m.fetchVerifiedSitePackage(context.Background(), src,
		[]weightSource{
			{URL: bad.URL + "/fake.zip", Label: "镜像站"},
			{URL: good.URL + "/fake.zip", Label: "官方源"},
		}, dest, func(s string) { logs = append(logs, s) })
	if err != nil {
		t.Fatalf("第二个源是好文件时应成功，实际：%v", err)
	}
	if used.Label != "官方源" {
		t.Errorf("应记录真实走的是官方源，实际 %q", used.Label)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil || !strings.EqualFold(sha256Hex(got), src.SHA256) {
		t.Errorf("产物内容不对：err=%v content=%q", rerr, got)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "回落") {
		t.Errorf("回落要写进日志，实际：%q", joined)
	}
	if !strings.Contains(joined, "SHA-256 校验通过") {
		t.Errorf("校验通过要写进日志，实际：%q", joined)
	}
}

// TestDownloadSitePackagePrefersMirrorAndVerifies：走完整入口，
// 镜像上有正确文件时用镜像、标注来源、产物哈希正确。
func TestDownloadSitePackagePrefersMirrorAndVerifies(t *testing.T) {
	payload := []byte("mirrored site package")
	sum := sha256Hex(payload)

	mux := http.NewServeMux()
	mux.HandleFunc("/sites/fakesite/9.9/fake.zip", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/upstream/fake.zip", func(w http.ResponseWriter, r *http.Request) {
		t.Error("镜像可用时不该去官方源")
		_, _ = w.Write(payload)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 临时登记一个假应用，避免依赖真实上游/真实网络。
	prev, had := siteSources["fakesite"]
	siteSources["fakesite"] = SiteSource{
		App: "fakesite", Name: "FakeSite", Version: "9.9", File: "fake.zip",
		SHA256: sum, Size: int64(len(payload)),
		Upstreams: []string{ts.URL + "/upstream/fake.zip"},
	}
	t.Cleanup(func() {
		if had {
			siteSources["fakesite"] = prev
		} else {
			delete(siteSources, "fakesite")
		}
	})

	m := &Manager{opt: Options{MirrorBase: ts.URL}}
	dest := filepath.Join(t.TempDir(), "fake.zip")
	var logs []string
	src, label, err := m.DownloadSitePackage(context.Background(), "fakesite", dest,
		func(s string) { logs = append(logs, s) })
	if err != nil {
		t.Fatalf("下载应成功：%v", err)
	}
	if src.Version != "9.9" {
		t.Errorf("应返回固定版本 9.9，实际 %q", src.Version)
	}
	if label != "镜像站" {
		t.Errorf("镜像可用时来源应是镜像站，实际 %q", label)
	}
	got, _ := os.ReadFile(dest)
	if sha256Hex(got) != sum {
		t.Errorf("产物哈希不对：%s", sha256Hex(got))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "用时") {
		t.Errorf("日志要写出耗时，实际：%q", logs)
	}
}

// TestDownloadSitePackageUnknownApp：未登记固定版本的应用必须明确报错，
// 由调用方（web 层）回落到目录条目里的旧逻辑。
func TestDownloadSitePackageUnknownApp(t *testing.T) {
	m := &Manager{}
	if _, _, err := m.DownloadSitePackage(context.Background(), "not-registered",
		filepath.Join(t.TempDir(), "x.zip"), nil); err == nil {
		t.Fatal("未登记固定版本的应用应显式报错")
	}
}

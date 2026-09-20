package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/upgrade"
)

// ============================================================================
//  镜像发布件同步（坑 217，2026-09-20 事故后新增）
//
//  镜像站文档根在外置盘上、只有面板守护进程有 TCC 授权能写。这个动作的信任链
//  与在线升级完全一致：清单先验签、资产逐个核 sha256、全部通过才从临时目录就位。
//  门禁用 httptest 假源覆盖四条：验签失败拒绝写 / 某个资产 sha 不符拒绝写且不留
//  半截文件 / 成功路径写全布局 / 幂等重跑。
// ============================================================================

const fakeMirrorVersion = "9.9.9"

func testSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type fakeMirror struct {
	base      string
	assetName string
	manifest  []byte
	sig       []byte
	install   []byte
	asset     []byte
}

// startFakeMirror 起一个假发布源。shaOverride 非空时清单里写这个（错的）sha，
// 用来验证"对不上就失败且不留半截文件"；wrongKey 时用另一把公钥验签。
func startFakeMirror(t *testing.T, shaOverride string, wrongKey bool) *fakeMirror {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// 单测绝不联网升级：把内置发布公钥换成测试公钥。
	old := upgrade.PubKeyHex
	t.Cleanup(func() { upgrade.PubKeyHex = old })
	if wrongKey {
		other, _, oerr := ed25519.GenerateKey(rand.Reader)
		if oerr != nil {
			t.Fatal(oerr)
		}
		upgrade.PubKeyHex = hex.EncodeToString(other)
	} else {
		upgrade.PubKeyHex = hex.EncodeToString(pub)
	}

	fs := &fakeMirror{
		assetName: fmt.Sprintf("zizpanel_%s_darwin_arm64.tar.gz", fakeMirrorVersion),
		install:   []byte("#!/bin/sh\necho fake-install\n"),
		asset:     []byte("fake-panel-tarball-" + strings.Repeat("x", 4096)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fs.manifest) })
	mux.HandleFunc("/manifest.json.sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fs.sig) })
	mux.HandleFunc("/install.sh", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fs.install) })
	mux.HandleFunc("/download/"+fakeMirrorVersion+"/"+fs.assetName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fs.asset)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fs.base = srv.URL

	sha := testSHA256(fs.asset)
	if shaOverride != "" {
		sha = shaOverride
	}
	manifest := map[string]any{
		"version":      fakeMirrorVersion,
		"notes":        "测试",
		"published_at": "2026-09-20T00:00:00Z",
		"assets": map[string]any{
			"darwin_arm64": map[string]any{
				"url":    fs.base + "/download/" + fakeMirrorVersion + "/" + fs.assetName,
				"sha256": sha,
				"size":   len(fs.asset),
			},
		},
	}
	fs.manifest, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	fs.sig = upgrade.SignManifest(priv, fs.manifest)
	return fs
}

func noopTaskLog(string, string) {}

func listLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".zp-sync-*"))
	return left
}

// TestMirrorSyncRejectsBadSignature：验签不过时一个字节都不许写进镜像目录。
func TestMirrorSyncRejectsBadSignature(t *testing.T) {
	src := startFakeMirror(t, "", true) // 用另一把公钥验签 ⇒ 必失败
	dir := t.TempDir()

	res, err := runMirrorSync(context.Background(), noopTaskLog, dir, src.base)
	if err == nil {
		t.Fatalf("验签失败必须报错，实际成功: %+v", res)
	}
	if !strings.Contains(err.Error(), "验签") {
		t.Errorf("错误里必须说清是验签失败，实际: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "manifest.json")); serr == nil {
		t.Error("验签失败时 manifest.json 不许出现在镜像目录")
	}
	if _, serr := os.Stat(filepath.Join(dir, "download")); serr == nil {
		t.Error("验签失败时不许建 download/")
	}
	if left := listLeftovers(t, dir); len(left) != 0 {
		t.Errorf("临时目录必须清干净，残留: %v", left)
	}
}

// TestMirrorSyncRejectsAssetSHA256Mismatch：某个资产 sha 不符 ⇒ 失败，且不留半截文件。
func TestMirrorSyncRejectsAssetSHA256Mismatch(t *testing.T) {
	src := startFakeMirror(t, strings.Repeat("0", 64), false)
	dir := t.TempDir()

	res, err := runMirrorSync(context.Background(), noopTaskLog, dir, src.base)
	if err == nil {
		t.Fatalf("资产 sha 不符必须报错，实际成功: %+v", res)
	}
	// 校验不通过 ⇒ 什么都没就位（连已经验过签的 manifest.json 也不许先落）
	for _, rel := range []string{"manifest.json", "manifest.json.sig", "install.sh"} {
		if _, serr := os.Stat(filepath.Join(dir, rel)); serr == nil {
			t.Errorf("资产校验失败时 %s 不许就位（必须整批通过才写）", rel)
		}
	}
	if _, serr := os.Stat(filepath.Join(dir, "download", fakeMirrorVersion)); serr == nil {
		t.Error("下载失败时不许留下 download/<版本>/")
	}
	if left := listLeftovers(t, dir); len(left) != 0 {
		t.Errorf("临时目录必须清干净，残留: %v", left)
	}
}

// TestMirrorSyncWritesFullLayout：成功路径写全布局（顶层 + download/<版本>/ + latest）。
func TestMirrorSyncWritesFullLayout(t *testing.T) {
	src := startFakeMirror(t, "", false)
	dir := t.TempDir()

	res, err := runMirrorSync(context.Background(), noopTaskLog, dir, src.base)
	if err != nil {
		t.Fatalf("同步应成功: %v", err)
	}
	if res.Version != fakeMirrorVersion {
		t.Errorf("结果版本 = %q，期望 %q", res.Version, fakeMirrorVersion)
	}
	latestName := "zizpanel_latest_darwin_arm64.tar.gz"
	want := map[string][]byte{
		"manifest.json":     src.manifest,
		"manifest.json.sig": src.sig,
		"install.sh":        src.install,
		filepath.Join("download", fakeMirrorVersion, src.assetName): src.asset,
		filepath.Join("download", "latest", latestName):             src.asset,
		latestName: src.asset,
	}
	for rel, content := range want {
		got, rerr := os.ReadFile(filepath.Join(dir, rel))
		if rerr != nil {
			t.Errorf("布局里缺少 %s: %v", rel, rerr)
			continue
		}
		if string(got) != string(content) {
			t.Errorf("%s 内容不对（%d 字节，期望 %d）", rel, len(got), len(content))
		}
	}
	if len(res.Files) == 0 {
		t.Error("逐文件结果不能为空（用户要知道哪个成功/失败）")
	}
	for _, f := range res.Files {
		if !f.OK || f.Error != "" {
			t.Errorf("成功路径里不该有失败项: %+v", f)
		}
	}
	if left := listLeftovers(t, dir); len(left) != 0 {
		t.Errorf("成功后临时目录必须删掉，残留: %v", left)
	}
}

// TestMirrorSyncIdempotent：重复跑结果一致（覆盖同版本同 sha 的文件）。
func TestMirrorSyncIdempotent(t *testing.T) {
	src := startFakeMirror(t, "", false)
	dir := t.TempDir()

	snap := func() map[string]string {
		out := map[string]string{}
		err := filepath.Walk(dir, func(p string, fi os.FileInfo, werr error) error {
			if werr != nil || fi.IsDir() {
				return werr
			}
			rel, _ := filepath.Rel(dir, p)
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[filepath.ToSlash(rel)] = testSHA256(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if _, err := runMirrorSync(context.Background(), noopTaskLog, dir, src.base); err != nil {
		t.Fatalf("第一次同步应成功: %v", err)
	}
	first := snap()
	if len(first) == 0 {
		t.Fatal("第一次同步没有产出任何文件")
	}
	// 先塞一个旧内容的同名文件，第二次必须被覆盖成源上的内容。
	stale := filepath.Join(dir, "download", "latest", "zizpanel_latest_darwin_arm64.tar.gz")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runMirrorSync(context.Background(), noopTaskLog, dir, src.base); err != nil {
		t.Fatalf("第二次同步应成功: %v", err)
	}
	second := snap()
	if len(second) != len(first) {
		t.Errorf("幂等重跑后文件数变了：%d → %d", len(first), len(second))
	}
	for rel, sha := range first {
		if second[rel] != sha {
			t.Errorf("幂等重跑后 %s 摘要变了：%s → %s", rel, sha, second[rel])
		}
	}
	if left := listLeftovers(t, dir); len(left) != 0 {
		t.Errorf("重跑后临时目录必须清干净，残留: %v", left)
	}
}

// TestMirrorSyncEndpointAsyncAndAdmin：接口走任务中心（202 + task_id）且真能同步成功。
func TestMirrorSyncEndpointAsyncAndAdmin(t *testing.T) {
	srv, ts := newTestServer(t)
	src := startFakeMirror(t, "", false)
	dir := t.TempDir()
	cookies := loginTestPanel(t, ts)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/mirror/sync",
		map[string]any{"dir": dir, "source": src.base}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("同步必须走任务中心返回 202，实际 %d: %v", res.StatusCode, out)
	}
	// 等任务真的落定（否则后台写盘会和 t.TempDir 清理抢目录）。
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if string(tk.Status()) != "succeeded" {
		t.Fatalf("假源同步应成功，实际 %s：%v", tk.Status(), tk.Meta().Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Errorf("任务成功后镜像目录里必须有 manifest.json: %v", err)
	}
	// 没配目录、请求体也空 → 明确 4xx，不猜
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/mirror/sync", map[string]any{}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("没给目录必须 400 并提示去设置里填，实际 %d: %v", res.StatusCode, out)
	}
}

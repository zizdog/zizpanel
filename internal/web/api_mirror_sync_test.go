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
	"sync"
	"testing"

	"github.com/zizdog/zizpanel/internal/upgrade"
)

// ============================================================================
//  镜像发布件同步（坑 217/218）
//
//  镜像站文档根在外置盘上、只有面板守护进程有 TCC 授权能写。这个动作的信任链
//  与在线升级完全一致：清单先验签、资产逐个核 sha256、全部通过才从临时目录就位。
//  门禁用 httptest 假源覆盖：镜像版清单优先 / 没有则回落并如实提示 / 签名坏一律
//  拒绝且不留半截文件 / 资产回源站取 / 幂等重跑。
// ============================================================================

const fakeMirrorVersion = "9.9.9"

func testSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type fakeMirrorCfg struct {
	shaOverride  string // 非空：清单里写这个（错的）sha
	wrongKey     bool   // 用另一把公钥验签
	withMirror   bool   // 源上提供 manifest-mirror.json
	mirrorPath   string // 镜像 base 在假源上的路径（默认 /mirror/zizpanel，且不提供资产）
	badMirrorSig bool   // 镜像版清单用坏签名
	badSourceSig bool   // 源站清单用坏签名
}

type fakeMirror struct {
	base           string
	mirrorBase     string
	assetName      string
	manifest       []byte
	sig            []byte
	mirrorManifest []byte
	mirrorSig      []byte
	install        []byte
	asset          []byte
}

// startFakeMirror 起一个假发布源。镜像版清单的 url 指向 mirrorPath（该路径下**不**提供
// 资产）：如果同步照清单 url 下载就会 404，从而证明资产确实回源站取（坑 218）。
func startFakeMirror(t *testing.T, cfg fakeMirrorCfg) *fakeMirror {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// 单测绝不联网升级：把内置发布公钥换成测试公钥。
	old := upgrade.PubKeyHex
	t.Cleanup(func() { upgrade.PubKeyHex = old })
	if cfg.wrongKey {
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
	if cfg.withMirror {
		mux.HandleFunc("/manifest-mirror.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fs.mirrorManifest) })
		mux.HandleFunc("/manifest-mirror.json.sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fs.mirrorSig) })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fs.base = srv.URL

	sha := testSHA256(fs.asset)
	if cfg.shaOverride != "" {
		sha = cfg.shaOverride
	}
	fs.manifest = fakeManifestJSON(t, fs.base+"/download/"+fakeMirrorVersion+"/"+fs.assetName, sha)
	fs.sig = upgrade.SignManifest(priv, fs.manifest)
	if cfg.badSourceSig {
		fs.sig[0] ^= 0xFF
	}
	if cfg.withMirror {
		path := cfg.mirrorPath
		if path == "" {
			path = "/mirror/zizpanel"
		}
		fs.mirrorBase = fs.base + path
		fs.mirrorManifest = fakeManifestJSON(t, fs.mirrorBase+"/download/"+fakeMirrorVersion+"/"+fs.assetName, sha)
		fs.mirrorSig = upgrade.SignManifest(priv, fs.mirrorManifest)
		if cfg.badMirrorSig {
			fs.mirrorSig[0] ^= 0xFF
		}
	}
	return fs
}

func fakeManifestJSON(t *testing.T, url, sha string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"version":      fakeMirrorVersion,
		"notes":        "测试",
		"published_at": "2026-09-20T00:00:00Z",
		"assets": map[string]any{
			"darwin_arm64": map[string]any{"url": url, "sha256": sha, "size": 4096},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type mirrorLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *mirrorLog) log(level, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+": "+text)
}

func (l *mirrorLog) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(strings.Join(l.lines, "\n"), sub)
}

// noopMirrorLog 给不检查日志的用例用。
func noopMirrorLog(string, string) {}

func listLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".zp-sync-*"))
	return left
}

// assertNothingWritten 断言镜像目录里没有就位的文件、也没留临时目录。
func assertNothingWritten(t *testing.T, dir string) {
	t.Helper()
	for _, rel := range []string{"manifest.json", "manifest.json.sig", "install.sh"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			t.Errorf("失败时 %s 不许就位（必须整批通过才写）", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "download")); err == nil {
		t.Error("失败时不许建 download/")
	}
	if left := listLeftovers(t, dir); len(left) != 0 {
		t.Errorf("临时目录必须清干净，残留: %v", left)
	}
}

// TestMirrorSyncDefaultSourceIsPublicSource：镜像同步的默认源必须是公网发布源，不许是镜像自己。
func TestMirrorSyncDefaultSourceIsPublicSource(t *testing.T) {
	if upgrade.DefaultSource == upgrade.MirrorSource {
		t.Fatal("公网发布源与镜像源常量相同，测试前提不成立")
	}
	srv, _ := newTestServer(t)
	if got := srv.mirrorDefaultSource(); got != upgrade.DefaultSource {
		t.Errorf("默认同步源 = %q，必须是公网发布源 %q", got, upgrade.DefaultSource)
	}
	if got := srv.mirrorDefaultSource(); got == upgrade.MirrorSource {
		t.Errorf("默认同步源不许是镜像自己 %q", upgrade.MirrorSource)
	}
	// 不传 / 只传空白 source 的解析路径也必须落到公网发布源。
	for _, raw := range []string{"", "   "} {
		if got := srv.resolveMirrorSource(raw); got != upgrade.DefaultSource {
			t.Errorf("source=%q 解析出 %q，必须是公网发布源 %q", raw, got, upgrade.DefaultSource)
		}
	}
	// 显式传的源优先，不被默认值覆盖。
	if got := srv.resolveMirrorSource("https://example.com/zizpanel/"); got != "https://example.com/zizpanel/" {
		t.Errorf("显式 source 被改了：%q", got)
	}
}

// TestMirrorSyncRejectsSourceEqualToTarget：源 == 目标镜像 base（同步到自己）直接拒绝，
// 且在任何网络动作之前就失败。
func TestMirrorSyncRejectsSourceEqualToTarget(t *testing.T) {
	for _, base := range []string{
		"https://mirror.zizdog.com:8888/zizpanel",
		"https://mirror.zizdog.com:8888/zizpanel/", // 尾斜杠不同也算同一个
	} {
		dir := t.TempDir()
		lg := &mirrorLog{}
		res, err := runMirrorSync(context.Background(), lg.log, dir, base, strings.TrimRight(base, "/"))
		if err == nil {
			t.Fatalf("源与目标相同必须拒绝，实际成功: %+v", res)
		}
		if !strings.Contains(err.Error(), "相同") {
			t.Errorf("错误里必须说清源与目标相同，实际: %v", err)
		}
		if !lg.has("配置错误") {
			t.Errorf("日志里要如实说明是配置错误，实际:\n%s", strings.Join(lg.lines, "\n"))
		}
		assertNothingWritten(t, dir)
	}
}

// TestMirrorSyncPrefersMirrorManifest：源上有镜像版清单 ⇒ 写进去的就是它，
// asset url 指向给定镜像 base，且资产仍回源站取（镜像路径下没有资产）。
func TestMirrorSyncPrefersMirrorManifest(t *testing.T) {
	src := startFakeMirror(t, fakeMirrorCfg{withMirror: true})
	dir := t.TempDir()
	lg := &mirrorLog{}

	res, err := runMirrorSync(context.Background(), lg.log, dir, src.base, src.mirrorBase)
	if err != nil {
		t.Fatalf("有镜像版清单时同步应成功: %v", err)
	}
	if res.Manifest != mirrorManifestName {
		t.Errorf("结果里的清单名 = %q，期望 %q", res.Manifest, mirrorManifestName)
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if rerr != nil {
		t.Fatalf("读写入的清单失败: %v", rerr)
	}
	if string(got) != string(src.mirrorManifest) {
		t.Error("写进镜像的清单必须原样是镜像版清单（不是源站清单）")
	}
	bases := manifestBases(t, got)
	if len(bases) != 1 || !bases[src.mirrorBase] {
		t.Errorf("asset url 必须指向镜像 base %s，实际 %v", src.mirrorBase, bases)
	}
	if lg.has("镜像分发可能没生效") {
		t.Error("指向镜像时不该报地址不一致")
	}
}

// TestMirrorSyncFallsBackToSourceManifest：源上没有镜像版清单 ⇒ 回落源站清单，
// 并在日志里如实说明下载地址可能仍指向源站。
func TestMirrorSyncFallsBackToSourceManifest(t *testing.T) {
	src := startFakeMirror(t, fakeMirrorCfg{})
	dir := t.TempDir()
	mirrorBase := src.base + "/mirror/zizpanel"
	lg := &mirrorLog{}

	res, err := runMirrorSync(context.Background(), lg.log, dir, src.base, mirrorBase)
	if err != nil {
		t.Fatalf("回落源站清单也应能同步: %v", err)
	}
	if res.Manifest != "manifest.json" {
		t.Errorf("结果里的清单名 = %q，期望 manifest.json", res.Manifest)
	}
	if !lg.has("源上没有镜像版清单，已用源站清单，下载地址可能仍指向源站") {
		t.Errorf("回落时必须在日志里如实说明，实际日志:\n%s", strings.Join(lg.lines, "\n"))
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if rerr != nil {
		t.Fatalf("读写入的清单失败: %v", rerr)
	}
	if string(got) != string(src.manifest) {
		t.Error("没有镜像版清单时就该写源站清单")
	}
	if !lg.has("镜像分发可能没生效") {
		t.Error("源站清单地址与镜像 base 不一致，必须如实报出来（不阻断）")
	}
}

// TestMirrorSyncRejectsBadSignature：验签不过时一个字节都不许写进镜像目录。
func TestMirrorSyncRejectsBadSignature(t *testing.T) {
	src := startFakeMirror(t, fakeMirrorCfg{wrongKey: true})
	dir := t.TempDir()

	res, err := runMirrorSync(context.Background(), noopMirrorLog, dir, src.base, src.mirrorBase)
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

// TestMirrorSyncRejectsBothManifestsBadlySigned：两份清单签名都坏（含"镜像版在但签名坏、
// 不许悄悄回落"与"镜像版没有、源站版也坏"两种）⇒ 拒绝写入、不留半截文件。
func TestMirrorSyncRejectsBothManifestsBadlySigned(t *testing.T) {
	cases := []struct {
		name string
		cfg  fakeMirrorCfg
	}{
		{"镜像版清单签名坏", fakeMirrorCfg{withMirror: true, badMirrorSig: true, badSourceSig: true}},
		{"回落源站清单签名坏", fakeMirrorCfg{badSourceSig: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := startFakeMirror(t, tc.cfg)
			dir := t.TempDir()
			_, err := runMirrorSync(context.Background(), noopMirrorLog, dir, src.base, src.mirrorBase)
			if err == nil {
				t.Fatal("签名坏必须拒绝写入")
			}
			if !strings.Contains(err.Error(), "验签") {
				t.Errorf("错误里必须说清是验签失败，实际: %v", err)
			}
			assertNothingWritten(t, dir)
		})
	}
}

// TestMirrorSyncRejectsAssetSHA256Mismatch：某个资产 sha 不符 ⇒ 失败，且不留半截文件。
func TestMirrorSyncRejectsAssetSHA256Mismatch(t *testing.T) {
	src := startFakeMirror(t, fakeMirrorCfg{shaOverride: strings.Repeat("0", 64)})
	dir := t.TempDir()

	res, err := runMirrorSync(context.Background(), noopMirrorLog, dir, src.base, src.mirrorBase)
	if err == nil {
		t.Fatalf("资产 sha 不符必须报错，实际成功: %+v", res)
	}
	// 校验不通过 ⇒ 什么都没就位（连已经验过签的 manifest.json 也不许先落）
	assertNothingWritten(t, dir)
}

// TestMirrorSyncWritesFullLayout：成功路径写全布局（顶层 + download/<版本>/ + latest）。
func TestMirrorSyncWritesFullLayout(t *testing.T) {
	src := startFakeMirror(t, fakeMirrorCfg{withMirror: true})
	dir := t.TempDir()

	res, err := runMirrorSync(context.Background(), noopMirrorLog, dir, src.base, src.mirrorBase)
	if err != nil {
		t.Fatalf("同步应成功: %v", err)
	}
	if res.Version != fakeMirrorVersion {
		t.Errorf("结果版本 = %q，期望 %q", res.Version, fakeMirrorVersion)
	}
	latestName := "zizpanel_latest_darwin_arm64.tar.gz"
	want := map[string][]byte{
		"manifest.json":     src.mirrorManifest,
		"manifest.json.sig": src.mirrorSig,
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
	src := startFakeMirror(t, fakeMirrorCfg{withMirror: true})
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

	if _, err := runMirrorSync(context.Background(), noopMirrorLog, dir, src.base, src.mirrorBase); err != nil {
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
	if _, err := runMirrorSync(context.Background(), noopMirrorLog, dir, src.base, src.mirrorBase); err != nil {
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
	src := startFakeMirror(t, fakeMirrorCfg{withMirror: true})
	// 镜像基址与假源的镜像 base 对齐：面板自证应认为地址指向镜像自己。
	srv.Cfg.MirrorBase = strings.TrimSuffix(src.mirrorBase, "/zizpanel")
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
	got, rerr := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if rerr != nil {
		t.Fatalf("任务成功后镜像目录里必须有 manifest.json: %v", rerr)
	}
	if string(got) != string(src.mirrorManifest) {
		t.Error("接口路径也必须写镜像版清单")
	}
	// 没配目录、请求体也空 → 明确 4xx，不猜
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/mirror/sync", map[string]any{}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("没给目录必须 400 并提示去设置里填，实际 %d: %v", res.StatusCode, out)
	}
}

// manifestBases 解析清单并汇总 asset url 指向的 base（供门禁断言）。
func manifestBases(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	m, err := upgrade.ParseManifest(data)
	if err != nil {
		t.Fatalf("解析清单失败: %v", err)
	}
	out := map[string]bool{}
	for _, ref := range m.Assets {
		out[mirrorURLBase(ref.URL)] = true
	}
	return out
}

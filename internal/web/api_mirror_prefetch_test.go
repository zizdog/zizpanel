package web

// api_mirror_prefetch_test.go —— 镜像站「先备好、再广播」的门禁。
//
// 全程假上游（httptest）+ t.TempDir() 假镜像目录：**绝不联网、绝不碰真实镜像卷**。
// 每条断言都必须能失败（负向对照），见交付报告里的变异验证记录。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

const (
	testPrefetchApp     = "frpc"
	testPrefetchAsset   = "frp_0.71.0_darwin_arm64.tar.gz"
	testPrefetchVersion = "v0.71.0"
)

// fakeUpstream 是一个假上游（GitHub release 的替身）：产物 + sha256 清单 + 公告。
type fakeUpstream struct {
	mu        sync.Mutex
	assets    map[string][]byte // 文件名 → 字节
	checksums map[string]string // 文件名 → 清单里写的 sha256（可以是错的）
	gets      map[string]int    // 路径 → GET 次数（幂等断言用它）
	heads     map[string]int    // 路径 → HEAD 次数
	// blockPath 非空时：该路径的 GET 会先写一部分再挂住，直到请求 ctx 结束（模拟中途取消）。
	blockPath string
	started   chan struct{}
	startOnce sync.Once
}

func newFakeUpstream(t *testing.T) (*fakeUpstream, *httptest.Server) {
	t.Helper()
	u := &fakeUpstream{
		assets: map[string][]byte{}, checksums: map[string]string{},
		gets: map[string]int{}, heads: map[string]int{}, started: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		u.mu.Lock()
		for name, sum := range u.checksums {
			b.WriteString(sum + "  " + name + "\n")
		}
		u.mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		u.mu.Lock()
		body, ok := u.assets[name]
		if r.Method == http.MethodHead {
			u.heads[name]++
		} else {
			u.gets[name]++
		}
		block := u.blockPath != "" && u.blockPath == name
		u.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		if !block {
			_, _ = w.Write(body)
			return
		}
		// 先写一半再挂住：模拟"下到一半被取消"。
		half := len(body) / 2
		_, _ = w.Write(body[:half])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		u.startOnce.Do(func() { close(u.started) })
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return u, srv
}

func (u *fakeUpstream) getCount(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.gets[name]
}

func (u *fakeUpstream) headCount(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.heads[name]
}

func testSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// addAsset 注册一个产物及其（正确的）sha256 清单条目。
func (u *fakeUpstream) addAsset(name string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.assets[name] = body
	u.checksums[name] = testSHA(body)
}

func (u *fakeUpstream) setChecksum(name, sum string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checksums[name] = sum
}

// prefetchTargetFor 造一个指向假上游的预取目标。
func prefetchTargetFor(srv *httptest.Server, appID, version, asset string) mirrorPrefetchTarget {
	return mirrorPrefetchTarget{
		AppID: appID, Name: appID, Version: version, Asset: asset,
		UpstreamURL: srv.URL + "/" + asset, ChecksumName: "checksums.txt", Arch: "arm64",
	}
}

func runPrefetch(t *testing.T, dir string, targets []mirrorPrefetchTarget, tweak func(*mirrorPrefetchOptions)) (*mirrorPrefetchResult, error, *mirrorLog) {
	t.Helper()
	lg := &mirrorLog{}
	opt := mirrorPrefetchOptions{Dir: dir, Targets: targets}
	if tweak != nil {
		tweak(&opt)
	}
	res, err := runMirrorAppPrefetch(context.Background(), lg.log, opt)
	return res, err, lg
}

func readAppIndex(t *testing.T, dir, appID string) *mirrorAppIndex {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, mirrorAppsSubdir, appID, mirrorManifestName))
	if err != nil {
		t.Fatalf("读应用索引失败: %v", err)
	}
	var idx mirrorAppIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatalf("应用索引不是合法 JSON: %v", err)
	}
	return &idx
}

// seedMirrorVersion 在镜像里预置一个"已就位且校验通过"的版本（含版本清单）。
func seedMirrorVersion(t *testing.T, dir, appID, version, asset string, body []byte) {
	t.Helper()
	vd := filepath.Join(dir, mirrorAppsSubdir, appID, version)
	if err := os.MkdirAll(vd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vd, asset), body, 0o644); err != nil {
		t.Fatal(err)
	}
	vm := mirrorVersionManifest{App: appID, Version: version,
		Assets: []mirrorVersionAsset{{Name: asset, SHA256: testSHA(body), Size: int64(len(body))}}}
	b, _ := json.MarshalIndent(vm, "", "  ")
	if err := os.WriteFile(filepath.Join(vd, mirrorManifestName), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedAppIndex(t *testing.T, dir, appID, latest string, assets []mirrorAppIndexAsset) []byte {
	t.Helper()
	idx := mirrorAppIndex{App: appID, Latest: latest, PublishedAt: "2026-01-01T00:00:00Z", Assets: assets}
	b, _ := json.MarshalIndent(idx, "", "  ")
	p := filepath.Join(dir, mirrorAppsSubdir, appID, mirrorManifestName)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------
//  1. 缺产物 ⇒ 下载 + 校验 + 索引出现；已存在且校验通过 ⇒ 第二次不再下载
// ---------------------------------------------------------------------------

func TestMirrorPrefetchDownloadsThenSkipsWhenVerified(t *testing.T) {
	up, srv := newFakeUpstream(t)
	body := []byte("fake-frpc-tarball-" + strings.Repeat("a", 2048))
	up.addAsset(testPrefetchAsset, body)
	dir := t.TempDir()
	targets := []mirrorPrefetchTarget{prefetchTargetFor(srv, testPrefetchApp, testPrefetchVersion, testPrefetchAsset)}

	res, err, lg := runPrefetch(t, dir, targets, nil)
	if err != nil {
		t.Fatalf("第一次预取应成功: %v\n日志:\n%s", err, strings.Join(lg.lines, "\n"))
	}
	if res.Downloaded != 1 || res.Skipped != 0 {
		t.Errorf("第一次应下载 1 个、跳过 0 个，实际 %d/%d", res.Downloaded, res.Skipped)
	}
	// 产物真的落盘、内容一致
	got, rerr := os.ReadFile(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, testPrefetchVersion, testPrefetchAsset))
	if rerr != nil {
		t.Fatalf("产物必须落盘: %v", rerr)
	}
	if string(got) != string(body) {
		t.Error("落盘产物与上游字节不一致")
	}
	// 索引出现该版本，且 sha256/size 与上游一致
	idx := readAppIndex(t, dir, testPrefetchApp)
	if idx.Latest != testPrefetchVersion {
		t.Errorf("索引 latest = %q，应为 %q", idx.Latest, testPrefetchVersion)
	}
	if len(idx.Assets) != 1 || idx.Assets[0].Version != testPrefetchVersion ||
		idx.Assets[0].SHA256 != testSHA(body) || idx.Assets[0].Size != int64(len(body)) {
		t.Errorf("索引里的产物条目不对: %+v", idx.Assets)
	}
	if idx.PublishedAt == "" {
		t.Error("索引必须带 published_at")
	}
	// 公告也发布了
	if _, err := os.Stat(filepath.Join(dir, mirrorAppsSubdir, mirrorAnnounceName)); err != nil {
		t.Errorf("预取成功后必须有公告: %v", err)
	}

	// 第二次：已存在且校验通过 ⇒ 一个字节都不再下载（假上游 GET 计数不变）
	firstGets := up.getCount(testPrefetchAsset)
	res2, err2, _ := runPrefetch(t, dir, targets, nil)
	if err2 != nil {
		t.Fatalf("第二次预取应成功: %v", err2)
	}
	if res2.Downloaded != 0 || res2.Skipped != 1 {
		t.Errorf("第二次应下载 0 个、跳过 1 个（幂等），实际 %d/%d", res2.Downloaded, res2.Skipped)
	}
	if n := up.getCount(testPrefetchAsset); n != firstGets {
		t.Errorf("第二次不许再下载：GET 次数 %d → %d", firstGets, n)
	}
}

// ---------------------------------------------------------------------------
//  2. sha256 不符 ⇒ 不写索引、不发布、日志如实报错
// ---------------------------------------------------------------------------

func TestMirrorPrefetchChecksumMismatchPublishesNothing(t *testing.T) {
	up, srv := newFakeUpstream(t)
	body := []byte("tampered-body-" + strings.Repeat("b", 1024))
	up.addAsset(testPrefetchAsset, body)
	// 上游清单写一个**别的** sha256（模拟传输损坏/上游清单错误）
	up.setChecksum(testPrefetchAsset, testSHA([]byte("not-the-body")))
	dir := t.TempDir()
	targets := []mirrorPrefetchTarget{prefetchTargetFor(srv, testPrefetchApp, testPrefetchVersion, testPrefetchAsset)}

	_, err, lg := runPrefetch(t, dir, targets, nil)
	if err == nil {
		t.Fatal("sha256 不符必须失败")
	}
	if !lg.has("sha256 不符") {
		t.Errorf("日志必须如实报 sha256 不符，实际:\n%s", strings.Join(lg.lines, "\n"))
	}
	if _, serr := os.Stat(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, mirrorManifestName)); serr == nil {
		t.Error("校验失败时绝不许发布应用索引")
	}
	if _, serr := os.Stat(filepath.Join(dir, mirrorAppsSubdir, mirrorAnnounceName)); serr == nil {
		t.Error("校验失败时绝不许发布公告")
	}
	if _, serr := os.Stat(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, testPrefetchVersion)); serr == nil {
		t.Error("校验失败时产物不许出现在 apps/ 里（半截文件会外露）")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".zp-appfetch-*")); len(left) != 0 {
		t.Errorf("临时目录必须清干净: %v", left)
	}
}

// ---------------------------------------------------------------------------
//  3. 空间不足 ⇒ 提前失败、说清数字、不发布
// ---------------------------------------------------------------------------

func TestMirrorPrefetchFailsEarlyWhenDiskTooSmall(t *testing.T) {
	up, srv := newFakeUpstream(t)
	body := []byte(strings.Repeat("c", 4096))
	up.addAsset(testPrefetchAsset, body)
	dir := t.TempDir()
	targets := []mirrorPrefetchTarget{prefetchTargetFor(srv, testPrefetchApp, testPrefetchVersion, testPrefetchAsset)}

	const free = int64(1024)
	_, err, lg := runPrefetch(t, dir, targets, func(o *mirrorPrefetchOptions) {
		o.FreeBytes = func(string) (int64, error) { return free, nil }
	})
	if err == nil {
		t.Fatal("空间不足必须提前失败")
	}
	msg := err.Error()
	// 必须说清：需要多少、剩多少（原始数字要在）
	if !strings.Contains(msg, strconv.Itoa(int(free))) {
		t.Errorf("错误必须写清剩余空间（%d 字节）: %s", free, msg)
	}
	need := int64(len(body)) + mirrorPrefetchReserveBytes
	if !strings.Contains(msg, strconv.Itoa(int(need))) {
		t.Errorf("错误必须写清需要多少（%d 字节）: %s", need, msg)
	}
	if !strings.Contains(msg, "未写入任何文件") {
		t.Errorf("错误要说明「没有写入」: %s", msg)
	}
	if n := up.getCount(testPrefetchAsset); n != 0 {
		t.Errorf("提前失败时不该下载任何字节，实际 GET %d 次", n)
	}
	if _, serr := os.Stat(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, mirrorManifestName)); serr == nil {
		t.Error("空间不足时不许发布索引")
	}
	_ = lg
}

// ---------------------------------------------------------------------------
//  4. 原子性：rename 前被杀 ⇒ 索引仍是旧的完整版
// ---------------------------------------------------------------------------

func TestMirrorPrefetchPublishIsAtomicOnAbort(t *testing.T) {
	up, srv := newFakeUpstream(t)
	oldBody := []byte("old-version-bytes")

	dir := t.TempDir()
	seedMirrorVersion(t, dir, testPrefetchApp, "v1.0.0", "frp_1.0.0.tar.gz", oldBody)
	oldIndex := seedAppIndex(t, dir, testPrefetchApp, "v1.0.0", []mirrorAppIndexAsset{
		{Name: "frp_1.0.0.tar.gz", Version: "v1.0.0", Arch: "arm64", SHA256: testSHA(oldBody), Size: int64(len(oldBody))},
	})

	newBody := []byte(strings.Repeat("d", 2048))
	up.addAsset(testPrefetchAsset, newBody)
	targets := []mirrorPrefetchTarget{prefetchTargetFor(srv, testPrefetchApp, testPrefetchVersion, testPrefetchAsset)}

	killed := false
	_, err, _ := runPrefetch(t, dir, targets, func(o *mirrorPrefetchOptions) {
		o.BeforePublish = func() error {
			killed = true
			return errFakeKilled
		}
	})
	if err == nil || !killed {
		t.Fatal("注入的发布前中止必须让预取失败")
	}
	// 索引必须**逐字节**还是旧的完整版
	got, rerr := os.ReadFile(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, mirrorManifestName))
	if rerr != nil {
		t.Fatalf("旧索引必须还在: %v", rerr)
	}
	if string(got) != string(oldIndex) {
		t.Errorf("中止后索引必须保持旧版本不变:\n旧: %s\n新: %s", oldIndex, got)
	}
	if _, serr := os.Stat(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, testPrefetchVersion)); serr == nil {
		t.Error("中止后新版本目录不许出现")
	}
	// 旧索引仍然合法可解析
	idx := readAppIndex(t, dir, testPrefetchApp)
	if idx.Latest != "v1.0.0" {
		t.Errorf("旧索引应仍指向 v1.0.0，实际 %q", idx.Latest)
	}
}

// ---------------------------------------------------------------------------
//  5. 旧版本清理：保留最近 3 个，第 4 个被删且日志写明
// ---------------------------------------------------------------------------

func TestMirrorPrefetchKeepsThreeNewestVersions(t *testing.T) {
	up, srv := newFakeUpstream(t)
	versions := []string{"v1.0.0", "v1.0.1", "v1.0.2", "v1.0.3"}
	var targets []mirrorPrefetchTarget
	for _, v := range versions {
		name := "frp_" + strings.TrimPrefix(v, "v") + ".tar.gz"
		up.addAsset(name, []byte("body-"+v))
		targets = append(targets, prefetchTargetFor(srv, testPrefetchApp, v, name))
	}
	dir := t.TempDir()
	_, err, lg := runPrefetch(t, dir, targets, nil)
	if err != nil {
		t.Fatalf("预取应成功: %v\n%s", err, strings.Join(lg.lines, "\n"))
	}
	if _, serr := os.Stat(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, "v1.0.0")); !os.IsNotExist(serr) {
		t.Error("第 4 个（最旧）版本必须被删掉")
	}
	for _, v := range versions[1:] {
		if _, serr := os.Stat(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, v)); serr != nil {
			t.Errorf("最近 3 个版本 %s 必须保留: %v", v, serr)
		}
	}
	if !lg.has("已删除旧版本") || !lg.has("v1.0.0") {
		t.Errorf("日志必须写明删了什么，实际:\n%s", strings.Join(lg.lines, "\n"))
	}
	idx := readAppIndex(t, dir, testPrefetchApp)
	if idx.Latest != "v1.0.3" {
		t.Errorf("latest 应为 v1.0.3，实际 %q", idx.Latest)
	}
	for _, a := range idx.Assets {
		if a.Version == "v1.0.0" {
			t.Error("被删掉的版本不许再出现在索引里")
		}
	}
}

// ---------------------------------------------------------------------------
//  索引只列**已落盘且校验通过**的产物（坏产物不许借旧索引留在索引里）
// ---------------------------------------------------------------------------

func TestMirrorPrefetchIndexOnlyListsVerifiedArtifacts(t *testing.T) {
	up, srv := newFakeUpstream(t)
	dir := t.TempDir()
	const brokenVer = "v9.9.9"
	// 预置一个"字节被改坏"的版本：旧清单写的是 original 的 sha256，磁盘上却是别的字节
	// （长度故意相同，这样只有 sha256 校验能发现它）。
	orig := []byte("aaaaaaaaaaaaaa")
	bad := []byte("bbbbbbbbbbbbbb")
	vd := filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, brokenVer)
	if err := os.MkdirAll(vd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vd, "broken.tar.gz"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	vm := mirrorVersionManifest{App: testPrefetchApp, Version: brokenVer,
		Assets: []mirrorVersionAsset{{Name: "broken.tar.gz", SHA256: testSHA(orig), Size: int64(len(orig))}}}
	b, _ := json.MarshalIndent(vm, "", "  ")
	if err := os.WriteFile(filepath.Join(vd, mirrorManifestName), b, 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧索引里还列着这个坏版本（模拟"坏字节已经混进索引"的最坏开局）。
	seedAppIndex(t, dir, testPrefetchApp, brokenVer, []mirrorAppIndexAsset{
		{Name: "broken.tar.gz", Version: brokenVer, Arch: "arm64", SHA256: testSHA(orig), Size: int64(len(orig))},
	})

	body := []byte("good-version-bytes")
	up.addAsset(testPrefetchAsset, body)
	targets := []mirrorPrefetchTarget{prefetchTargetFor(srv, testPrefetchApp, testPrefetchVersion, testPrefetchAsset)}
	if _, err, lg := runPrefetch(t, dir, targets, nil); err != nil {
		t.Fatalf("预取应成功: %v\n%s", err, strings.Join(lg.lines, "\n"))
	}
	idx := readAppIndex(t, dir, testPrefetchApp)
	if idx.Latest != testPrefetchVersion {
		t.Errorf("latest 应为校验通过的 %s，实际 %q", testPrefetchVersion, idx.Latest)
	}
	for _, a := range idx.Assets {
		if a.Version == brokenVer {
			t.Errorf("校验不通过的产物不许出现在索引里: %+v", a)
		}
	}
}

// ---------------------------------------------------------------------------
//  7. 任务取消：不留半截索引（索引要么旧要么新）
// ---------------------------------------------------------------------------

var errFakeKilled = &fakeKillError{}

type fakeKillError struct{}

func (*fakeKillError) Error() string { return "模拟发布前进程被杀" }

func TestMirrorPrefetchCancelLeavesIndexIntact(t *testing.T) {
	up, srv := newFakeUpstream(t)
	dir := t.TempDir()
	oldBody := []byte("old-index-target")
	seedMirrorVersion(t, dir, testPrefetchApp, "v1.0.0", "frp_1.0.0.tar.gz", oldBody)
	oldIndex := seedAppIndex(t, dir, testPrefetchApp, "v1.0.0", []mirrorAppIndexAsset{
		{Name: "frp_1.0.0.tar.gz", Version: "v1.0.0", Arch: "arm64", SHA256: testSHA(oldBody), Size: int64(len(oldBody))},
	})

	big := []byte(strings.Repeat("e", 128<<10))
	up.addAsset(testPrefetchAsset, big)
	up.mu.Lock()
	up.blockPath = testPrefetchAsset
	up.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	targets := []mirrorPrefetchTarget{prefetchTargetFor(srv, testPrefetchApp, testPrefetchVersion, testPrefetchAsset)}
	done := make(chan error, 1)
	go func() {
		_, err := runMirrorAppPrefetch(ctx, noopMirrorLog, mirrorPrefetchOptions{Dir: dir, Targets: targets})
		done <- err
	}()
	select {
	case <-up.started:
	case <-time.After(5 * time.Second):
		t.Fatal("假上游没有开始写响应（测试自身的问题）")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消后预取必须如实失败")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后预取没有及时返回")
	}
	got, rerr := os.ReadFile(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, mirrorManifestName))
	if rerr != nil {
		t.Fatalf("旧索引必须还在: %v", rerr)
	}
	if string(got) != string(oldIndex) {
		t.Errorf("取消后不许留半截索引:\n旧: %s\n新: %s", oldIndex, got)
	}
	if _, serr := os.Stat(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, testPrefetchVersion)); serr == nil {
		t.Error("取消后新版本不许出现")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".zp-appfetch-*")); len(left) != 0 {
		t.Errorf("取消后临时目录必须清干净: %v", left)
	}
}

// ---------------------------------------------------------------------------
//  端到端：假上游 → 预取/校验/原子发布 → 镜像目录被真实静态服务 → 面板看到公告
// ---------------------------------------------------------------------------

func TestMirrorPrefetchEndToEndPublishesAnnouncement(t *testing.T) {
	up, upstream := newFakeUpstream(t)
	assetBody := []byte(strings.Repeat("x", 3072))
	up.addAsset(testPrefetchAsset, assetBody)

	dir := t.TempDir()
	targets := []mirrorPrefetchTarget{prefetchTargetFor(upstream, testPrefetchApp, testPrefetchVersion, testPrefetchAsset)}

	// 索引"发布前"：不存在（原子发布的对照）。
	indexPath := filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, mirrorManifestName)
	if _, err := os.Stat(indexPath); err == nil {
		t.Fatal("预取前不该有索引（测试前提）")
	}
	res1, err, _ := runPrefetch(t, dir, targets, nil)
	if err != nil {
		t.Fatalf("第一次预取应成功: %v", err)
	}
	idx := readAppIndex(t, dir, testPrefetchApp)
	announcePath := filepath.Join(dir, mirrorAppsSubdir, mirrorAnnounceName)
	ann1, rerr := os.ReadFile(announcePath)
	if rerr != nil {
		t.Fatalf("必须有公告: %v", rerr)
	}
	// 第二次：一个字节都不再下载（省下的就是第一次的下载量）。
	res2, err2, _ := runPrefetch(t, dir, targets, nil)
	if err2 != nil {
		t.Fatalf("第二次预取应成功: %v", err2)
	}
	ann2, _ := os.ReadFile(announcePath)
	if string(ann1) != string(ann2) {
		t.Error("内容没变时公告必须逐字节不变（面板才不会被打扰）")
	}
	if res1.Downloaded != 1 || res2.Downloaded != 0 || res2.Skipped != 1 {
		t.Errorf("幂等不对：第一次下载 %d、第二次下载 %d / 跳过 %d", res1.Downloaded, res2.Downloaded, res2.Skipped)
	}

	// 把镜像目录用**真实**静态文件服务暴露出去（会自己发 ETag、自己回 304）。
	mirror := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer mirror.Close()

	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)
	srv.Cfg.MirrorBase = mirror.URL
	var probes int64
	srv.marketUpdateCheckOverride = func(_ context.Context, id string) services.ZizvideoUpdateCheck {
		probes++
		return services.ZizvideoUpdateCheck{
			App: id, Installed: "0.1.0", Latest: "0.2.0", UpdateAvailable: true,
			CheckedAt: time.Now().Format(time.RFC3339), Source: "mirror-index",
		}
	}

	get := func() map[string]any {
		t.Helper()
		res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/announce", nil, cookies)
		if res.StatusCode != 200 {
			t.Fatalf("announce 应 200，实际 %d: %v", res.StatusCode, out)
		}
		return out["data"].(map[string]any)
	}
	d1 := get()
	if d1["changed"] != true || d1["prompt"] != true || asInt(d1["available"]) != 1 {
		t.Fatalf("面板应看到公告变化并算出 1 个可更新应用：%v", d1)
	}
	d2 := get()
	if d2["changed"] != false || d2["prompt"] != false {
		t.Errorf("静态服务回 304 后不许重复提示：%v", d2)
	}

	t.Logf("端到端原始数字：上游产物 %d 字节；第一次下载 %d 个（%d 字节，sha256 已校验）；"+
		"第二次下载 %d 个（省下 %d 字节）；索引条目 %d 条（latest=%s，published_at=%s）；"+
		"公告 %d 字节 revision=%s；面板第一次 changed=%v available=%v，第二次 304 changed=%v；探测次数 %d",
		len(assetBody), res1.Downloaded, len(assetBody), res2.Downloaded, len(assetBody),
		len(idx.Assets), idx.Latest, idx.PublishedAt, len(ann1), res1.AnnounceRevision,
		d1["changed"], d1["available"], d2["changed"], probes)
}

// ---------------------------------------------------------------------------
//  接口形态：走任务中心（202 + task_id）
// ---------------------------------------------------------------------------

func TestMirrorPrefetchEndpointAsyncAndNeedsDir(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)
	up, fake := newFakeUpstream(t)
	body := []byte("endpoint-body-" + strings.Repeat("f", 512))
	up.addAsset(testPrefetchAsset, body)
	// 注入假上游：这个接口在测试里绝不打 GitHub。
	srv.mirrorPrefetchOptionsOverride = func(dir string) mirrorPrefetchOptions {
		return mirrorPrefetchOptions{Dir: dir, Targets: []mirrorPrefetchTarget{
			prefetchTargetFor(fake, testPrefetchApp, testPrefetchVersion, testPrefetchAsset),
		}}
	}

	// 没给目录、设置里也没配 ⇒ 明确 400，不猜
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/mirror/prefetch", map[string]any{}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("没配镜像目录必须 400，实际 %d: %v", res.StatusCode, out)
	}

	dir := t.TempDir()
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/mirror/prefetch", map[string]any{"dir": dir}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("预取必须走任务中心返回 202，实际 %d: %v", res.StatusCode, out)
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if string(tk.Status()) != "succeeded" {
		t.Fatalf("假上游预取应成功，实际 %s：%v", tk.Status(), tk.Meta().Error)
	}
	if _, err := os.Stat(filepath.Join(dir, mirrorAppsSubdir, testPrefetchApp, mirrorManifestName)); err != nil {
		t.Errorf("任务成功后镜像里应有应用索引: %v", err)
	}
}

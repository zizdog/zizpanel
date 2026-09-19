package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/upgrade"
)

// armUpgradeTestFetcher 让升级检查/暂存在**完全不联网**的前提下拿到一份
// 用测试公钥签过名的清单：
//   - 只有 base 命中 okBases 的候选返回签名清单；
//   - 其它候选一律返回错误（模拟"不通"），从而证明 API 会按候选顺序回落。
//
// 注入的只是"下载"这一步；验签仍由 upgrade.FetchManifestAny 用真实实现完成，
// 所以测试同时锁住了"换源不换签名"。
func armUpgradeTestFetcher(t *testing.T, manifestVersion string, okBases ...string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	prevKey := upgrade.PubKeyHex
	upgrade.PubKeyHex = hex.EncodeToString(pub)
	t.Cleanup(func() { upgrade.PubKeyHex = prevKey })

	m := upgrade.Manifest{
		Version: manifestVersion,
		Notes:   "测试清单",
		Assets: map[string]upgrade.ManifestRef{
			upgrade.AssetKey(runtime.GOOS, runtime.GOARCH): {
				URL:    "https://example.invalid/zizpanel.tar.gz",
				SHA256: strings.Repeat("a", 64),
			},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig := upgrade.SignManifest(priv, data)

	allowed := make(map[string]bool, len(okBases))
	for _, b := range okBases {
		allowed[strings.TrimRight(b, "/")] = true
	}

	prevFetch := upgradeManifestFetcher
	upgradeManifestFetcher = func(ctx context.Context, base string) ([]byte, []byte, error) {
		if allowed[strings.TrimRight(base, "/")] {
			return data, sig, nil
		}
		return nil, nil, fmt.Errorf("测试环境：候选 %s 不可达", base)
	}
	t.Cleanup(func() { upgradeManifestFetcher = prevFetch })
}

// TestUpgradeCheckWithEmptySourceUsesCandidatesAndDoesNotPersist 锁住本轮改造的核心：
//
//	① 空 source 不再 400「尚未配置升级源地址」，而是走候选列表；
//	② 默认候选**不会**被写回配置（否则每台机器都被钉死在一个源上）；
//	③ status 里能看到"这次实际命中了哪个源"。
func TestUpgradeCheckWithEmptySourceUsesCandidatesAndDoesNotPersist(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	// 用户从没显式配过源（配置字段为空 = 走默认候选）
	srv.Cfg.UpgradeSource = ""
	// 只有默认主源可用，前面的候选（这里是同一个）之外的都不可达
	armUpgradeTestFetcher(t, "999.0.0", upgrade.DefaultSource)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/upgrade/check", map[string]any{}, cookies)
	if res.StatusCode == http.StatusBadRequest {
		t.Fatalf("空 source 不应再返回 400：%v", out)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("检查更新应 200，实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if got := asString(data["source"]); got != upgrade.DefaultSource {
		t.Errorf("实际命中的源应为 %s，实际 %q", upgrade.DefaultSource, got)
	}
	if got := asString(data["effective_source"]); got != upgrade.DefaultSource {
		t.Errorf("effective_source 应为 %s，实际 %q", upgrade.DefaultSource, got)
	}
	if got := asString(data["configured_source"]); got != "" {
		t.Errorf("configured_source 应保持空（用户没配过），实际 %q", got)
	}
	// 关键断言：默认候选绝不能被写回配置
	if srv.Cfg.UpgradeSource != "" {
		t.Fatalf("默认候选被写回了配置（UpgradeSource=%q）—— 这台机器会被钉死在一个源上", srv.Cfg.UpgradeSource)
	}

	// status 要能看到实际用过的源
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/system/upgrade", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("升级状态应 200，实际 %d: %v", res.StatusCode, out)
	}
	sdata, _ := out["data"].(map[string]any)
	if got := asString(sdata["effective_source"]); got != upgrade.DefaultSource {
		t.Errorf("status.effective_source 应为 %s，实际 %q", upgrade.DefaultSource, got)
	}
	if got := asString(sdata["source"]); got != "" {
		t.Errorf("status.source 应是配置值（空），实际 %q", got)
	}
}

// TestUpgradeCheckPersistsExplicitSource 显式传入的源必须被记住，
// 且下一次不带 source 时会优先用它。
func TestUpgradeCheckPersistsExplicitSource(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	srv.Cfg.UpgradeSource = ""
	explicit := "https://explicit.example/zizpanel"
	armUpgradeTestFetcher(t, "999.0.0", explicit)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/upgrade/check",
		map[string]any{"source": explicit}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("显式指定源时检查应 200，实际 %d: %v", res.StatusCode, out)
	}
	if srv.Cfg.UpgradeSource != explicit {
		t.Fatalf("显式传入的源应被写回配置，实际 %q", srv.Cfg.UpgradeSource)
	}
	data, _ := out["data"].(map[string]any)
	if got := asString(data["configured_source"]); got != explicit {
		t.Errorf("configured_source 应为 %q，实际 %q", explicit, got)
	}

	// 再请求一次（不带 source）：应优先用配置里的源
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/upgrade/check", map[string]any{}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("再次检查应 200，实际 %d: %v", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	if got := asString(data["source"]); got != explicit {
		t.Errorf("不带 source 时应优先命中配置里的源 %q，实际 %q", explicit, got)
	}
}

// TestUpgradeStageWithEmptySourceUsesCandidates 下载/暂存这条路也不能再报 400。
//
// 清单版本故意低于当前版本，让 stage 在"取清单 + 比版本"之后就返回，
// 不会去下载任何东西（单测不许联网）。
func TestUpgradeStageWithEmptySourceUsesCandidates(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	srv.Cfg.UpgradeSource = ""
	armUpgradeTestFetcher(t, "0.0.1", upgrade.DefaultSource)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/upgrade/stage", map[string]any{}, cookies)
	if res.StatusCode == http.StatusBadRequest {
		t.Fatalf("空 source 的暂存不应再返回 400：%v", out)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("暂存应 200，实际 %d: %v", res.StatusCode, out)
	}
	if srv.Cfg.UpgradeSource != "" {
		t.Fatalf("暂存同样不应把候选写回配置，实际 %q", srv.Cfg.UpgradeSource)
	}
	// 取到清单后要记下"实际用了哪个源"
	if got := upgrade.LoadState(srv.Cfg.WorkDir).SourceBase; got != upgrade.DefaultSource {
		t.Errorf("state.source_base 应为 %s，实际 %q", upgrade.DefaultSource, got)
	}
}

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

	"github.com/zizdog/zizpanel/internal/tasks"
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

// TestUpgradeWarnsAboutRunningTasks 锁住"升级会打断正在跑的任务"这条提示。
//
// 2026-10-06 用户实测：转码过程中升级面板 ⇒ 面板重启，ffmpeg 被一起收走，这次编码白跑
// （任务记录本来就只在内存里）。这不是 bug，但用户必须**先知道**再决定 —— 所以：
//
//	① 状态接口要把正在跑的任务列出来（前端凭它显示警告）；
//	② apply 在没有 force 时必须 409（直接调 API 也不能静默丢掉几小时的工作）；
//	③ 没有任务在跑时不许瞎拦（负向对照）。
func TestUpgradeWarnsAboutRunningTasks(t *testing.T) {
	// ① 纯函数：只挑 running，已结束的不算。
	metas := []tasks.Meta{
		{ID: "t-1", Kind: "video_compress", Title: "压缩 4K 电影", Status: tasks.StatusRunning},
		{ID: "t-2", Kind: "video_compress", Title: "压缩 已结束", Status: tasks.StatusSucceeded},
	}
	if got := blockingUpgradeTasks(metas); len(got) != 1 || got[0]["title"] != "压缩 4K 电影" {
		t.Fatalf("blockingUpgradeTasks 应只返回正在跑的那个，实际 %v", got)
	}
	if got := blockingUpgradeTasks(nil); len(got) != 0 {
		t.Fatalf("没有任务时不该有阻塞项，实际 %v", got)
	}
	// ② 那句话必须点名任务、并给出"仍然升级"的出路；没有任务时必须是空串（不拦）。
	msg := upgradeBlockedMessage(blockingUpgradeTasks(metas))
	for _, want := range []string{"1 个任务", "压缩 4K 电影", "仍然升级"} {
		if !strings.Contains(msg, want) {
			t.Errorf("升级中断警告里缺少 %q：%s", want, msg)
		}
	}
	if msg := upgradeBlockedMessage(nil); msg != "" {
		t.Errorf("没有任务在跑时不该产出阻塞文案，实际 %q", msg)
	}

	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	// ② 状态接口带上 blocking_tasks（前端据此显示警告）。
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/system/upgrade", nil, cookies)
	if got, ok := mapGet(mapGet(out, "data"), "blocking_tasks").([]any); !ok || len(got) != 0 {
		t.Fatalf("没有任务时 blocking_tasks 应为空数组，实际 %v", mapGet(mapGet(out, "data"), "blocking_tasks"))
	}

	// 起一个真的在跑的任务（一直等到测试放开为止）。
	release := make(chan struct{})
	srv.Tasks.Start("video_compress", "dir:/tmp/probe", "压缩 4K 电影",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			<-release
			return nil, nil
		})
	t.Cleanup(func() { close(release) })

	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/upgrade", nil, cookies)
	list, _ := mapGet(mapGet(out, "data"), "blocking_tasks").([]any)
	if len(list) != 1 {
		t.Fatalf("有转码在跑时 blocking_tasks 应有 1 条，实际 %v", mapGet(mapGet(out, "data"), "blocking_tasks"))
	}
	first, _ := list[0].(map[string]any)
	if asString(first["kind"]) != "video_compress" || asString(first["title"]) != "压缩 4K 电影" {
		t.Fatalf("blocking_tasks 内容不对：%v", first)
	}
}

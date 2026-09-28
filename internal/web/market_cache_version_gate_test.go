package web

// market_cache_version_gate_test.go —— 「市场/服务的本地缓存必须按面板版本失效」的唯一门禁。
//
// 为什么现有门禁抓不到（用户报障"mini 升级到 1.11.1 后市场里搜不到新上架的 Jellyfin"
// 的直接根因）：前端把 GET /api/v1/market + GET /api/v1/services 的结论按时间戳存进
// sessionStorage，**升级面板不会让它失效** —— 旧条目一直渲染到用户手动点「⟳ 更新」。
// 仓库里没有任何一条判据约束"缓存的判据里必须有面板版本"：既有门禁只管语法、模块引用、
// 文案去重、单份实现，没有一条看缓存的失效条件。
//
// 只断言**源码结构**（不断言文案/CSS，见 AGENTS 第六节 ④）：
//   ① 落盘必须带当前面板版本（saveDataCache 里 version: panelVersion）；
//   ② 读回必须把版本当判据：版本不一致 ⇒ 丢弃（return null），不是只存不比；
//   ③ 版本来自真实接口 /api/v1/health（api.js 有 health、ensurePanelVersion 调它），
//      不是写死的常量。
//
// 负向对照（已实测变红）：把 loadDataCache 里那两行版本判据删掉 ⇒ ② 红。

import (
	"regexp"
	"strings"
	"testing"
)

func TestMarketCacheVersionGate(t *testing.T) {
	apps := readAssetJS(t, "apps.js")
	api := readAssetJS(t, "api.js")

	// ② 版本不一致必须丢弃：结构是 "const cachedVersion = String(o.version …)"
	//    加上 "if (…cachedVersion !== panelVersion) return null"（不夹注释，便于钉结构）。
	load := jsFuncBody(t, apps, "loadDataCache")
	mustContain(t, "loadDataCache", load, "o.version")
	re := regexp.MustCompile(`if\s*\([^)]*cachedVersion[^)]*(?:!==|!=)[^)]*\)\s*\{?\s*return null`)
	if !re.MatchString(load) {
		t.Error("loadDataCache 没有把「缓存的版本 !== 当前面板版本 ⇒ return null」写成判据 —— " +
			"升级面板后旧缓存会继续渲染（用户报障：升级后市场里搜不到新上架的应用）")
	}
	if !strings.Contains(load, "panelVersion") {
		t.Error("loadDataCache 的判据里没有 panelVersion —— 缓存没有可以对比的当前版本")
	}

	// ① 落盘必须带版本，否则读的时候没有可比的东西。
	save := jsFuncBody(t, apps, "saveDataCache")
	mustContain(t, "saveDataCache", save, "version: panelVersion")

	// ③ 版本必须来自 /api/v1/health（不是新造接口、也不是写死的版本常量）。
	if !strings.Contains(api, "health: () => request('GET', `${API_BASE}/health`)") {
		t.Error("api.js 没有 health()（/api/v1/health）—— 前端没有取当前面板版本的入口")
	}
	pv := jsFuncBody(t, apps, "ensurePanelVersion")
	if !strings.Contains(pv, "api.health()") {
		t.Error("ensurePanelVersion 不调用 api.health() —— panelVersion 不是从真实接口取的")
	}
	// 拿不到版本时不许认缓存（否则等于回到"只按时间戳"的老判据）。
	if !strings.Contains(load, "!panelVersion") {
		t.Error("loadDataCache 在 panelVersion 为空时仍认缓存 —— 版本没取到就必须当没有")
	}
}

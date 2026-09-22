package web

import (
	"os"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  合并结果的 ID 唯一性门禁（坑 228）
//
//  用户报障：「软件市场里出现了两个同应用卡片」。真机证据：services 表里同时有
//  当前标签的记录与一条旧标签的残留记录，前端按 label 归一化去重认不出它们是
//  同一个应用 ⇒ 两张卡片。这里用 zizvideo 复刻同一类数据。
//
//  修法：接口把**目录 ID**（app_id）交给前端当稳定键。下面这条遍历门禁锁死：
//    · /api/v1/market 每条都有 app_id == id，且 app_id 全局唯一；
//    · /api/v1/services 每条记录都带它的目录 app_id（含旧标签的记录）；
//    · 同一 app_id 在"市场条目 ∪ 服务记录"里只对应一张卡片。
// ============================================================================

// TestMarketListAppIDsAreUniqueAndStable 市场条目必须带稳定键 app_id，且全局唯一。
func TestMarketListAppIDsAreUniqueAndStable(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	list, _ := out["data"].(map[string]any)["list"].([]any)
	if len(list) == 0 {
		t.Fatal("市场列表为空 —— 遍历没跑到，门禁等于没跑")
	}
	seen := map[string]string{} // app_id → id
	for _, it := range list {
		m, _ := it.(map[string]any)
		id := asString(m["id"])
		appID := asString(m["app_id"])
		if appID == "" {
			t.Errorf("市场条目 %s 没有 app_id（前端的合并去重键就落空了）", id)
			continue
		}
		if appID != id {
			t.Errorf("市场条目 %s 的 app_id 是 %q，应与 id 一致", id, appID)
		}
		if prev, dup := seen[appID]; dup {
			t.Errorf("app_id %q 被条目 %s 与 %s 重复声明 —— 市场里会出现两张同应用的卡片", appID, prev, id)
		}
		seen[appID] = id
	}
}

// seedLegacyLabelRecords 复刻真机实况：同一个应用的两条记录 —— 当前标签一条、
// 换过部署方式留下的旧标签一条（旧标签靠"记录名 == 目录 ID"认回同一个应用）。
func seedLegacyLabelRecords(t *testing.T, repo *services.Repository) {
	t.Helper()
	for _, r := range []struct{ name, label string }{
		{"cn-zizpanel-zizvideo", services.ZizvideoLabel},
		{services.ZizvideoAppID, "cn.zizvideo.legacy"},
	} {
		if err := repo.Create(t.Context(), &services.Service{
			Name: r.name, DisplayName: "zizvideo", Kind: services.KindNative,
			LaunchLabel: r.label, Icon: "🎬", Port: services.ZizvideoPort,
		}); err != nil {
			t.Fatalf("准备记录 %s 失败: %v", r.name, err)
		}
	}
}

// TestServiceRecordsCarryCatalogAppID 服务记录必须带目录 app_id；旧标签的记录也认。
func TestServiceRecordsCarryCatalogAppID(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)
	seedLegacyLabelRecords(t, services.NewRepository(srv.Store))

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/services?health=0", nil, cookies)
	list, _ := out["data"].(map[string]any)["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d", len(list))
	}
	for _, it := range list {
		m, _ := it.(map[string]any)
		if got := asString(m["app_id"]); got != services.ZizvideoAppID {
			t.Errorf("记录 %s（label %s）的 app_id 应是 %s，实际 %q —— "+
				"旧标签的记录认不出来就会多出一张卡片",
				asString(m["name"]), asString(m["launch_label"]), services.ZizvideoAppID, got)
		}
	}
}

// TestInstalledMergeYieldsOneCardPerAppID 模拟前端的合并：同一 app_id 只允许一张卡片。
//
// 前端 mergeAppEntries / dedupeMarketEntries 都以 appKeyOf 为键，而 appKeyOf 现在
// 优先返回 'id:'+app_id。这里断言喂给它的两份数据满足"app_id 唯一"这个前提。
func TestInstalledMergeYieldsOneCardPerAppID(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)
	seedLegacyLabelRecords(t, services.NewRepository(srv.Store))

	_, mout, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	mlist, _ := mout["data"].(map[string]any)["list"].([]any)
	_, sout, _ := doJSON(t, ts, "GET", "/api/v1/services?health=0", nil, cookies)
	slist, _ := sout["data"].(map[string]any)["list"].([]any)

	// 卡片 = 按 app_id 分组；没有 app_id 的（纯自建服务）按记录名各算一张。
	cards := map[string][]string{}
	marketCards := 0
	for _, it := range mlist {
		m, _ := it.(map[string]any)
		if id := asString(m["app_id"]); id != "" {
			cards[id] = append(cards[id], "market:"+asString(m["id"]))
			marketCards++
		}
	}
	for _, it := range slist {
		m, _ := it.(map[string]any)
		id := asString(m["app_id"])
		if id == "" {
			id = "svc:" + asString(m["name"])
		}
		cards[id] = append(cards[id], "svc:"+asString(m["name"]))
	}
	// 该应用的两条记录 + 那一条市场条目必须落在同一个分组里。
	if got := cards[services.ZizvideoAppID]; len(got) != 3 {
		t.Fatalf("zizvideo 应合成一张卡片（1 条市场条目 + 2 条记录），实际分组 %v", got)
	}
	// 市场条目数 == 分组数 ⇒ 没有任何条目因为键不同而单独成卡。
	if marketCards != len(cards) {
		t.Errorf("市场条目 %d 个，分组 %d 个 —— 有应用被拆成了多张卡片", marketCards, len(cards))
	}
	for id, members := range cards {
		if id == "" {
			t.Errorf("有条目落进了空分组：%v", members)
		}
	}
}

// TestFrontendMergeKeysByAppID 前端必须以 app_id 为合并键。
//
// 静态钉住：把这条从 appKeyOf 里删掉就退回"按 label 猜身份"，正是重复卡片的成因。
func TestFrontendMergeKeysByAppID(t *testing.T) {
	sp := readAssetJS(t, "servicePanel.js")
	mustContain(t, "servicePanel.js", sp, "const appID = typeof x.app_id === 'string' ? x.app_id.trim().toLowerCase() : '';")
	mustContain(t, "servicePanel.js", sp, "if (appID) return 'id:' + appID;")
	// 目录声明的服务标识必须在合并时当代表，避免旧标签的残留记录抢走卡片的启停动作。
	mustContain(t, "servicePanel.js", sp, "const matched = want.length")
	mustContain(t, "servicePanel.js", sp, "e.market.uninstall && e.market.uninstall.service")
	// 合并与去重必须共用同一个键函数（两份实现就会漂）。
	apps := readAssetJS(t, "apps.js")
	if !strings.Contains(apps, "dedupeMarketEntries((cache?.list || [])") {
		t.Error("apps.js 的市场渲染必须走 dedupeMarketEntries（按稳定键去重），不能自己 set 一遍")
	}
}

// TestMarketUninstallForgetsEveryAppIdentityLabel 卸载后必须按**全部身份键**删记录。
//
// 只删 app.ServiceLabel 的旧写法会留下旧标签的残留记录，卸载后它以残留卡片再冒出来
// （坑 228）。这里静态钉住调用点，行为面由 internal/services 的
// TestForgetByLabelsRemovesEveryAppIdentity 遍历全目录锁死。
func TestMarketUninstallForgetsEveryAppIdentityLabel(t *testing.T) {
	src, err := os.ReadFile("api_services.go")
	if err != nil {
		t.Fatalf("读不到 api_services.go: %v", err)
	}
	s := string(src)
	mustContain(t, "api_services.go", s, "labels := services.AppIdentityLabels(app)")
	mustContain(t, "api_services.go", s, "ForgetByLabels(ctx, labels...)")
	if strings.Contains(s, "ForgetByLabel(ctx, app.ServiceLabel)") {
		t.Error("卸载路径又退回只删 ServiceLabel —— 旧标签的残留记录会以残留卡片冒出来（坑 228）")
	}
	if got := strings.Count(s, "labels := services.AppIdentityLabels(app)"); got != 2 {
		t.Errorf("installer 与 brew 两条卸载路径都应覆盖全部身份键，实际只有 %d 处调用", got)
	}
}

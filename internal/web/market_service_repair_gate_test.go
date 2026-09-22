package web

import "testing"

// ============================================================================
//  "怎么修服务"必须由后端带给界面（坑 231 的接线半边）
//
//  判据本身在 services.ServiceRepairFor（那条门禁在 services 包里）；这里只保证
//  /api/v1/market 每条都真的带上它 —— 掉字段的话前端会静默退回"自己猜 brew"，
//  就又会出现 whisper.cpp 那种点了没反应的提示。
// ============================================================================

func TestMarketItemsCarryServiceRepair(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	// 造出用户报障的那个真实现场：运行体（引擎）在磁盘上、launchd 里没有它、
	// 面板里也没有任何记录。brew 探测故意失败 —— 这时"装了"只能靠真实产物，
	// 正是 whisper.cpp 卡住的那条路（坑 231）。
	if n := seedDeclaredRuntimeBodies(t, srv); n == 0 {
		t.Fatal("一个运行体都没造出来 —— 门禁的前提没了")
	}
	seedFakeBrew(t, srv, nil, false)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/market", nil, cookies)
	list, _ := out["data"].(map[string]any)["list"].([]any)
	if len(list) == 0 {
		t.Fatal("市场列表为空 —— 遍历没跑到，门禁等于没跑")
	}
	needy := 0
	for _, it := range list {
		m, _ := it.(map[string]any)
		sr, ok := m["service_repair"].(map[string]any)
		if !ok {
			t.Errorf("条目 %s 没有 service_repair 字段：界面拿不到「怎么修」，只能自己猜 brew",
				asString(m["id"]))
			continue
		}
		needed, isBool := sr["needed"].(bool)
		if !isBool {
			t.Errorf("条目 %s 的 service_repair.needed 不是布尔值", asString(m["id"]))
			continue
		}
		if !needed {
			continue
		}
		needy++
		// 需要修 ⇒ 必须给动作与一句人话（否则界面只能给一个没有下一步的警告）。
		if asString(sr["action"]) == "" || asString(sr["hint"]) == "" {
			t.Errorf("条目 %s 报告服务未注册却没给修法与提示：%v", asString(m["id"]), sr)
		}
		// 报告需要修 ⇒ 状态必须自洽：launchd 里确实没有它。
		if m["service_in_launchd"] == true {
			t.Errorf("条目 %s 的服务明明在 launchd 里，却报告需要修", asString(m["id"]))
		}
	}
	if needy == 0 {
		t.Fatal("造了整目录的运行体却没有任何条目报告「服务未注册」—— 这条门禁等于没跑")
	}
}

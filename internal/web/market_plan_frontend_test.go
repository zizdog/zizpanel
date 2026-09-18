package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  "昂贵的依赖检测不许放在列表渲染路径上"这一类的前端门禁
//
//  2026-09-21 报障（用户原文："应用又开始卡了：正在读取应用目录…不是早就开发了
//  缓存功能吗！！？？"）的根因不在缓存，而在**市场列表对每一条应用跑了一遍完整
//  卸载计划**：其中的依赖检测是一次真实的 `brew uses --installed`（brew 启动本身
//  约 0.4s），36 条串起来 = 冷启动 14.998s（修复后实测 0.33s）。
//
//  修法：列表只算便宜的部分（GET /api/v1/market 的 uninstall 计划**不含**依赖结论），
//  依赖检测推迟到用户真的点「卸载」时按需查（GET /api/v1/market/{id}/uninstall-plan）。
//
//  这条按需接口的前提是**前端真的用它**。后端不再往列表里塞依赖结论之后，
//  任何"从市场列表里读 blocked / force_allowed / dependents"的代码都会静默失效 ——
//  表现是「强制卸载」那条路凭空消失、或者界面断言"没有依赖"（而其实没查过）。
//  这类 bug 不会报错，只会悄悄少一个按钮，所以补成门禁。
// ============================================================================

// TestMarketPlanConsumersUseOnDemandEndpoint 遍历**全部**前端 JS：
// 用 `api.market()`（列表）的地方不得消费依赖类字段；依赖类判定只能来自
// `api.marketUninstallPlan(...)`（按需接口）。
//
// 遍历全目录而不是只查两个文件，是因为这类问题的形态就是"下次在别处再犯一次"。
func TestMarketPlanConsumersUseOnDemandEndpoint(t *testing.T) {
	dir := filepath.Join("assets", "js")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读不到前端目录 %s: %v", dir, err)
	}
	// 依赖类字段：只有"完整计划"（按需接口）才可能有值。
	depFields := []string{"force_allowed", "dependents", "force_note", "blocked"}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		js := readAssetJS(t, e.Name())
		// api.js 是接口定义的所在地，不消费计划。
		if e.Name() == "api.js" {
			continue
		}
		checked++
		// 该文件是否出现"从市场列表拿 uninstall 计划"的形态：
		//   · (mkt.list || []).find(...).uninstall
		//   · api.market() 与某个 .uninstall 组合
		// 判据刻意宽松（宁可多报也不漏报），命中后再逐字段检查。
		usesListPlan := strings.Contains(js, "api.market()") || strings.Contains(js, "mkt.list")
		if !usesListPlan {
			continue
		}
		usesOnDemand := strings.Contains(js, "marketUninstallPlan")
		for _, f := range depFields {
			if !strings.Contains(js, f) {
				continue
			}
			if !usesOnDemand {
				t.Errorf("%s：既从市场列表取计划（api.market()/mkt.list），又读依赖类字段 %q —— "+
					"列表的计划**不含**依赖结论（它为省 15 秒冷启动刻意不查），"+
					"必须改用 api.marketUninstallPlan(id) 的按需计划，否则这个判断会静默失效",
					e.Name(), f)
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个前端 JS 都没检查到 —— 目录或文件名猜错了，门禁等于没跑")
	}
}

// TestMarketPlanEndpointWired 锁住按需接口三件套都在：后端路由、前端 api 方法、
// 以及**真的有人调用**。任意一环丢了，用户点「卸载」就再也看不到依赖说明。
func TestMarketPlanEndpointWired(t *testing.T) {
	srv := readGoSource(t, "server.go")
	if !strings.Contains(srv, `"GET /api/v1/market/{id}/uninstall-plan"`) {
		t.Error("server.go 里没有 GET /api/v1/market/{id}/uninstall-plan 路由")
	}
	api := readAssetJS(t, "api.js")
	if !strings.Contains(api, "marketUninstallPlan") {
		t.Error("api.js 里没有 marketUninstallPlan（前端拿不到按需计划）")
	}
	// 两个已知的消费方：市场卡片的「卸载」（servicePanel.js）与 PHP 环境里的
	// 「🗑 卸载」（sites.js）。少一个，那条路上的依赖说明就没了。
	for _, f := range []string{"servicePanel.js", "sites.js"} {
		if js := readAssetJS(t, f); !strings.Contains(js, "marketUninstallPlan") {
			t.Errorf("%s 没用按需计划接口 —— 这条路会看不到依赖/强制卸载选项", f)
		}
	}
}

// readGoSource 读 internal/web 下的一个 Go 源文件（门禁用）。
func readGoSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读不到 %s: %v", name, err)
	}
	return string(b)
}

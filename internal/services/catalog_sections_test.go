package services

import "testing"

// TestMarketSectionsBaseEnvironmentIsLast 锁住用户明确提的两条：
//  1. 应用市场最后一个板块叫「基础环境」（原来是「其它」）；
//  2. 兜底板块必须**排在最后**（改名后位置不能变）。
//
// 这两个名字只允许在 catalog.go 的 categoryLabels 里定义一次 ——
// 前端 apps.js 不再自带分类中文名，所以这条测试同时也是"改名一处生效"的证据。
func TestMarketSectionsBaseEnvironmentIsLast(t *testing.T) {
	secs := MarketSections()
	if len(secs) < 2 {
		t.Fatalf("市场板块至少要有普通板块 + 兜底板块，实际 %d 个", len(secs))
	}
	seen := map[string]bool{}
	fallbacks := 0
	for _, s := range secs {
		if s.Key == "" || s.Label == "" {
			t.Fatalf("板块缺少 key 或中文名: %+v", s)
		}
		if seen[s.Key] {
			t.Fatalf("板块 key 重复: %s", s.Key)
		}
		seen[s.Key] = true
		if s.Fallback {
			fallbacks++
		}
		if s.Label == "其它" {
			t.Errorf("板块 %s 不该再叫「其它」—— 用户要求改名为「基础环境」", s.Key)
		}
	}
	if fallbacks != 1 {
		t.Fatalf("兜底板块应当且只能有一个，实际 %d 个", fallbacks)
	}
	last := secs[len(secs)-1]
	if !last.Fallback {
		t.Errorf("兜底板块必须排在最后（用户要求「基础环境」仍在最后一个板块），"+
			"实际最后一个是 %s（%s）", last.Key, last.Label)
	}
	if last.Key != CategoryOther {
		t.Errorf("兜底板块的 key 应为 %q，实际 %q", CategoryOther, last.Key)
	}
	if last.Label != "基础环境" {
		t.Errorf("最后一个板块应显示为「基础环境」，实际 %q", last.Label)
	}
}

// TestCategoryLabelsShareOneSource 保证"一处定义、到处生效"：
// 板块显示名必须取自 CategoryLabels 同一份字典，且返回值是副本。
func TestCategoryLabelsShareOneSource(t *testing.T) {
	labels := CategoryLabels()
	for _, s := range MarketSections() {
		if labels[s.Key] != s.Label {
			t.Errorf("板块 %s 的显示名 %q 与 CategoryLabels()[%s]=%q 不一致 —— 中文名必须只有一处定义",
				s.Key, s.Label, s.Key, labels[s.Key])
		}
	}
	if labels[CategoryOther] != "基础环境" {
		t.Errorf("分类 %q 的中文名应为「基础环境」，实际 %q", CategoryOther, labels[CategoryOther])
	}
	// 返回值必须是副本：一个调用方改动它不能污染全局字典（否则改名会变成随机行为）。
	labels[CategoryOther] = "被改了"
	if CategoryLabels()[CategoryOther] != "基础环境" {
		t.Error("CategoryLabels 返回的必须是副本（调用方改动不能污染全局字典）")
	}
}

// TestLNMPIsNotAMarketCard 锁住「一键 LNMP」的定位：它是组合动作，不是目录条目。
//
// 用户要求把这个入口放在「网站管理」；市场里不能再出现一张 lnmp 卡片。
// web 层还有一条防御性排除（internal/web 的 marketHiddenApps），
// 这条测试保证目录本身也不会冒出 ID=lnmp 的条目。
func TestLNMPIsNotAMarketCard(t *testing.T) {
	if app, ok := FindApp("lnmp"); ok {
		t.Fatalf("目录里不该有 ID=lnmp 的条目（一键 LNMP 是组合动作，入口在网站管理），实际: %+v", app)
	}
	// lnmp / runtime 没有专属板块，应当落到兜底板块（基础环境）：
	// 分别是 nginx / PHP / MySQL 与 Colima 容器运行时。
	named := map[string]bool{}
	for _, s := range MarketSections() {
		if !s.Fallback {
			named[s.Key] = true
		}
	}
	lnmp, runtime := 0, 0
	for _, a := range Catalog() {
		switch a.Category {
		case CategoryLNMP:
			lnmp++
		case CategoryRuntime:
			runtime++
		}
		if named[a.Category] && (a.Category == CategoryLNMP || a.Category == CategoryRuntime) {
			t.Errorf("分类 %s 不该有专属板块（它应归「基础环境」兜底板块）", a.Category)
		}
	}
	// 目录里确实有这两类条目，否则上面的断言就是空的（测试会假绿）。
	if lnmp == 0 || runtime == 0 {
		t.Fatalf("目录里应当同时有 %s(%d) 与 %s(%d) 两类条目，否则基础环境板块的归类没有被真正验证",
			CategoryLNMP, lnmp, CategoryRuntime, runtime)
	}
}

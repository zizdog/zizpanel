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

// TestWebsiteEnvIsItsOwnSection 锁住 2026-09-19 的「基础环境 / 网站环境」两层拆分：
//
//   - 「网站环境」（CategoryLNMP）必须是一个**专属板块**（不是兜底板块）；
//   - nginx / PHP / MySQL / PostgreSQL 全部归它 —— 一个都不能再落进「基础环境」；
//   - 「基础环境」（兜底板块）只收纳没有专属板块的分类，且里面**不能有理应是
//     网站环境层的组件**（否则市场会把 Web 服务器/数据库混进"跨应用依赖"里）。
//
// 这是"目录 ↔ 板块声明双向一致"的那条反漂移门禁：
// 目录改了分类、或 MarketSections 漏了某个 key，这里立刻红。
func TestWebsiteEnvIsItsOwnSection(t *testing.T) {
	// 反向：声明里必须有 lnmp 板块，且它不是兜底板块、显示为「网站环境」。
	secs := MarketSections()
	var webenv MarketSection
	var hasWebenv bool
	named := map[string]bool{}
	for _, s := range secs {
		if !s.Fallback {
			named[s.Key] = true
		}
		if s.Key == CategoryLNMP {
			webenv, hasWebenv = s, true
		}
	}
	if !hasWebenv {
		t.Fatalf("MarketSections 里必须有 %q 板块（nginx / PHP / MySQL / PostgreSQL 的归属）", CategoryLNMP)
	}
	if webenv.Fallback {
		t.Errorf("%s 不该是兜底板块（它收纳的是明确的网站环境组件）", CategoryLNMP)
	}
	if webenv.Label != "网站环境" {
		t.Errorf("%s 板块应显示为「网站环境」，实际 %q", CategoryLNMP, webenv.Label)
	}

	// 正向：网站环境层的四个组件必须都在这个分类里。
	websiteEnvIDs := []string{"nginx", "php82", "php84", "mysql84", "postgresql17"}
	for _, id := range websiteEnvIDs {
		app, found := FindApp(id)
		if !found {
			t.Fatalf("目录里找不到 %s", id)
		}
		if app.Category != CategoryLNMP {
			t.Errorf("%s（%s）的分类应为 %q（网站环境），实际 %q —— "+
				"归错会落进「基础环境」兜底板块，用户会在跨应用依赖里看到 Web 服务器",
				app.ID, app.Name, CategoryLNMP, app.Category)
		}
	}

	// 兜底板块的收纳口径必须与板块声明一致：任何"没有专属板块"的分类才落进去，
	// 而网站环境组件因为有了专属板块，绝不能再出现在兜底里。
	fallback := secs[len(secs)-1]
	if !fallback.Fallback {
		t.Fatalf("最后一个板块必须是兜底板块，实际 %s", fallback.Key)
	}
	for _, a := range Catalog() {
		inFallback := !named[a.Category]
		if inFallback {
			for _, id := range websiteEnvIDs {
				if a.ID == id {
					t.Errorf("%s 落进了兜底板块（%s）—— 网站环境组件必须在「网站环境」专属板块里",
						a.ID, fallback.Label)
				}
			}
		}
	}
}

// TestFallbackSectionIsCrossAppDependenciesOnly 锁住 2026-09-19 拆分的另一半：
// 「基础环境」兜底板块里只剩**跨应用运行依赖**（CLT/Homebrew/ffmpeg 这类），
// 不能混进网站组件（nginx/PHP/MySQL/PostgreSQL）。
//
// 为什么单独一条：这条是"用户看到的市场分类是否真的分成两层"的直接门禁 ——
// 前一条只验证分类字段，这条验证**兜底板块最终装了什么**。
func TestFallbackSectionIsCrossAppDependenciesOnly(t *testing.T) {
	named := map[string]bool{}
	for _, s := range MarketSections() {
		if !s.Fallback {
			named[s.Key] = true
		}
	}
	// ffmpeg 是跨应用运行依赖，必须在兜底板块里（2026-09-19 起它从「运维工具」移回）。
	ffmpeg, found := FindApp("ffmpeg")
	if !found {
		t.Fatal("目录里找不到 ffmpeg")
	}
	if named[ffmpeg.Category] {
		t.Errorf("ffmpeg 不该归到有专属板块的分类 %q —— 它是「基础环境」的跨应用依赖",
			ffmpeg.Category)
	}
	// 兜底板块里出现过的分类只允许是"跨应用依赖类"的：
	// other（基础环境本身）/ runtime（容器运行时，所有 Docker 应用的前提）。
	allowed := map[string]bool{CategoryOther: true, CategoryRuntime: true}
	for _, a := range Catalog() {
		if named[a.Category] {
			continue // 有专属板块，不落兜底
		}
		if !allowed[a.Category] {
			t.Errorf("%s 的分类 %q 没有专属板块、又不在允许的跨应用依赖分类里 —— "+
				"要么给它建板块（MarketSections），要么把它归到「基础环境」", a.ID, a.Category)
		}
	}
}

// TestLNMPIsNotAMarketCard 锁住「一键 LNMP」的定位：它是组合动作，不是目录条目。
//
// 2026-09-19 拆分后它的四个组件归「网站环境」板块，但 ID=lnmp 本身仍不能是
// 一张市场卡片：它的入口在「网站管理」（sites.js），市场里不能再出现重复入口。
// web 层还有一条防御性排除（internal/web 的 marketHiddenApps），
// 这条测试保证目录本身也不会冒出 ID=lnmp 的条目。
func TestLNMPIsNotAMarketCard(t *testing.T) {
	if app, ok := FindApp("lnmp"); ok {
		t.Fatalf("目录里不该有 ID=lnmp 的条目（一键 LNMP 是组合动作，入口在网站管理），实际: %+v", app)
	}
}

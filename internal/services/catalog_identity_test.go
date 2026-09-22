package services

import (
	"testing"
)

// ============================================================================
//  目录身份唯一性门禁（坑 228）
//
//  重复卡片的根因是合并层按 label 归一化猜身份，而一个应用换过部署方式后
//  会留下旧标签的服务记录。修法是让后端解析出**目录 ID** 当稳定键。这条门禁遍历
//  整个目录，锁死"一个 ID / 标签只属于一个应用"与"每种写法都能唯一解析回去"——
//  任何新条目抢用别人的标签、或同一个 ID 出现两次，都在这里失败。
// ============================================================================

// TestCatalogAppIDsUnique 目录里 ID 必须非空且唯一（ID 是所有接口的稳定键）。
func TestCatalogAppIDsUnique(t *testing.T) {
	seen := map[string]int{}
	for _, a := range Catalog() {
		if a.ID == "" {
			t.Errorf("目录条目 %q 没有 ID", a.Name)
			continue
		}
		seen[a.ID]++
	}
	dup := 0
	for id, n := range seen {
		if n > 1 {
			t.Errorf("目录 ID %q 出现了 %d 次 —— 合并后必然渲染成多张卡片", id, n)
			dup++
		}
	}
	if dup == 0 && len(seen) == 0 {
		t.Fatal("目录一个条目都没有 —— 遍历没跑到，门禁等于没跑")
	}
}

// TestCatalogIdentityKeysUniqueAcrossApps 一个 label / 名称 / ID 只能属于一个应用。
//
// 为什么必须：解析是"第一个匹配就返回"，两个应用抢同一个标签时结论会随目录顺序漂，
// 表现就是同一张卡片在两次刷新之间换了个应用。
func TestCatalogIdentityKeysUniqueAcrossApps(t *testing.T) {
	owner := map[string]string{}
	checked := 0
	for _, a := range Catalog() {
		for _, k := range append(identityKeysOf(a), a.ID) {
			if k == "" {
				continue
			}
			checked++
			if prev, ok := owner[k]; ok && prev != a.ID {
				t.Errorf("标识 %q 同时属于 %s 与 %s —— 解析结果会随目录顺序漂", k, prev, a.ID)
			}
			owner[k] = a.ID
		}
	}
	if checked == 0 {
		t.Fatal("一个目录标识都没检查到 —— 门禁等于没跑")
	}
}

// TestEveryCatalogIdentityResolvesToItsOwnApp 遍历目录：每条声明的标识都必须唯一解析回自己。
//
// 这条覆盖"换过部署方式留下的旧标签"（AliasLabels）：漏掉一个，那条旧记录
// 就会在「已安装」里多渲染成一张同名的卡片。
func TestEveryCatalogIdentityResolvesToItsOwnApp(t *testing.T) {
	checked := 0
	for _, a := range Catalog() {
		for _, k := range identityKeysOf(a) {
			if k == "" {
				continue
			}
			checked++
			got, ok := FindAppByService(&Service{LaunchLabel: k})
			if !ok {
				t.Errorf("%s 声明的标识 %q 解析不到任何应用", a.ID, k)
				continue
			}
			if got.ID != a.ID {
				t.Errorf("标识 %q 解析成了 %s，期望 %s", k, got.ID, a.ID)
			}
			// 名称/显示名两种写法也必须指向同一个应用（记录不一定带标签）。
			if gotName, okName := FindAppByService(&Service{DisplayName: k}); okName && gotName.ID != a.ID {
				t.Errorf("标识 %q 作为 display_name 解析成了 %s，期望 %s", k, gotName.ID, a.ID)
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个目录标识都没检查到 —— 门禁等于没跑")
	}
}

// TestCatalogEntryBelongsToOneAppTab 一个条目只能属于一个页内 Tab。
//
// 前端把条目分流到「应用市场 / docker / 一键建站」三个 Tab（apps.js 的 isDockerRec
// 与 site_app）。一个条目同时满足两类，就会在两个 Tab 里各出现一次 —— 那也是重复。
func TestCatalogEntryBelongsToOneAppTab(t *testing.T) {
	checked := 0
	for _, a := range Catalog() {
		dockerRec := a.DockerReference || a.Kind == KindCompose || a.Kind == KindDocker
		if a.SiteApp != nil && dockerRec {
			t.Errorf("%s 同时是 site_app 与 docker 推荐项目 —— 会在两个 Tab 里各出现一张卡片", a.ID)
		}
		if a.SiteApp != nil && a.PanelInstaller != "" {
			t.Errorf("%s 同时是 site_app 与面板安装器条目 —— 安装入口会在两个地方出现", a.ID)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("目录一个条目都没检查到 —— 门禁等于没跑")
	}
}

// identityKeysOf 返回一个目录条目声明的**服务记录标识**（不含 ID：调用方单独加）。
func identityKeysOf(a App) []string {
	out := []string{a.ServiceLabel, a.AdoptLabel, a.Name}
	out = append(out, a.AliasLabels...)
	return out
}

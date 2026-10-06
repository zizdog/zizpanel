package services

import "testing"

// TestRemovalFormulasCoverPanelApps 是"卸载不许漏包"的门禁（2026-10-06 用户报障：
// 彻底卸载重装后 aria2 / transmission / vips / colima 还在）。
//
// 手抄的映射会漂（uninstall.sh 里那张表漏了 aria2 等），所以现在由目录导出、脚本引用。
// 这个门禁锁住"导出的表里必须有这些应用及其 brew 包" —— 谁把目录里的 BrewFormula 删了、
// 或者改了标签，这里立刻红，而不是等用户重装后发现软件还在。
func TestRemovalFormulasCoverPanelApps(t *testing.T) {
	want := map[string]string{
		"com.zizdog.aria2":        "aria2",
		"com.zizdog.transmission": "transmission-cli",
		"com.zizdog.colima":       "colima",
		"com.zizdog.imgcompress":  "vips",
		"installer:phpmyadmin":    "phpmyadmin",
	}
	got := map[string]string{}
	for _, rf := range RemovalFormulas() {
		got[rf.Label] = rf.Formula
	}
	for label, formula := range want {
		if got[label] != formula {
			t.Errorf("目录导出的卸载表里 %s 的 brew 包应为 %q，实际 %q（漏了它，彻底卸载就会留下这个包）",
				label, formula, got[label])
		}
	}
	if len(got) < 5 {
		t.Fatalf("导出的卸载表只有 %d 条，明显不完整", len(got))
	}
}

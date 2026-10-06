package services

import (
	"sort"
	"strings"
)

// RemovalFormula 是"某个应用依赖的 brew 包"，供**卸载脚本**使用。
type RemovalFormula struct {
	// Label 是这条记录在卸载脚本里的匹配键：优先用 launchd 标签（服务记录里就是它），
	// 没有标签的用 "installer:<PanelInstaller>"（面板装过但没有常驻服务的应用，如 phpMyAdmin）。
	Label string
	// Formula 是 brew 包名。
	Formula string
	// App 是目录 ID（日志/排障用）。
	App string
}

// RemovalFormulas 把"面板装过的应用 → 它依赖的 brew 包"从**目录**导出给卸载脚本。
//
// 为什么由 Go 导出、而不是让卸载脚本自己维护一张表（2026-10-06 用户报障）：
// `uninstall.sh` 里的 `engine_formula_for_label` 是**手抄的子集**，早就漂了 ——
// aria2 / transmission-cli / colima / vips 都不在里面，于是"彻底卸载"只摘掉了服务定义、
// 把 brew 包装修留着，用户重装后面板照样显示"已安装"（他原话："除了 python 和 ffmpeg
// 以外，其它的软件应该不存在才对"）。目录是唯一真相，这里把它导出去（与坑 238 同一条教训）。
//
// 只导出**有 brew 包**的条目：compose/docker/纯面板内置的应用没有可 brew 卸载的东西。
func RemovalFormulas() []RemovalFormula {
	out := make([]RemovalFormula, 0, 32)
	seen := map[string]bool{}
	for _, a := range Catalog() {
		formula := strings.TrimSpace(a.BrewFormula)
		if formula == "" {
			continue
		}
		label := strings.TrimSpace(a.ServiceLabel)
		if label == "" && strings.TrimSpace(a.PanelInstaller) != "" {
			label = "installer:" + strings.TrimSpace(a.PanelInstaller)
		}
		if label == "" {
			continue
		}
		key := label + "\x1f" + formula
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, RemovalFormula{Label: label, Formula: formula, App: a.ID})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label == out[j].Label {
			return out[i].Formula < out[j].Formula
		}
		return out[i].Label < out[j].Label
	})
	return out
}

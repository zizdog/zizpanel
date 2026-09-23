package services

import "github.com/zizdog/zizpanel/internal/plugins"

// applyPluginTable 让**内建插件表**成为"表已覆盖字段"的真源（过渡期机制）。
//
// 为什么敢这么改：`plugin_equiv_test.go` 逐字段断言表与手写目录一致（不一致即红），
// 所以这一步在今天是**零行为变化**；等 B4 把应用逐个迁完，对应的手写定义再删掉。
// 覆盖范围刻意只到卡片事实（名称/图标/摘要/端口/健康路径/配置文件位置）——
// 描述、注意事项、徽标这类文案仍以目录定义为准（AGENTS 第六节：门禁不碰文案）。
func applyPluginTable(apps []App) []App {
	if len(apps) == 0 {
		return apps
	}
	out := apps
	copied := false
	for i := range apps {
		spec, ok := plugins.Builtin(apps[i].ID)
		if !ok {
			continue
		}
		f := spec.CardFacts("") // home 未知：配置文件路径保持表里的 ~/ 形式，ConfigFilePath 支持
		if !copied {
			out = append([]App(nil), apps...)
			copied = true
		}
		a := &out[i]
		a.Name, a.Icon, a.Summary = f.Name, f.Icon, f.Summary
		if f.Port > 0 {
			a.Port = f.Port
		}
		if f.HealthPath != "" {
			a.HealthPath = f.HealthPath
		}
		if spec.Config != nil {
			a.ConfigPath = spec.Config.Path
		}
	}
	return out
}

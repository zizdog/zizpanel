package services

import (
	"strings"

	"github.com/zizdog/zizpanel/internal/plugins"
)

// sameRail 判断"表里的来源/运行方式"与"目录里那条"是不是同一个东西。
//
// brew 条目：formula 必须一致；release 条目：目录里必须是同一条 release 注册项；
// 其它（compose/docker/自研安装器）一律**不允许**被表覆盖。
func sameRail(app App, spec *plugins.Spec) bool {
	if spec == nil {
		return false
	}
	switch spec.Source.Kind {
	case "brew":
		return app.Kind == KindNative && strings.TrimSpace(app.BrewFormula) == strings.TrimSpace(spec.Source.Formula)
	case "release":
		return IsReleaseBinaryApp(app.ID) && !ReleaseBinaryIsMirrorOnly(app.ID)
	default:
		return false
	}
}

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
		// 只在**同一轨**时覆盖：表里写 brew、目录里却是 compose/docker（或被别的安装器接管）
		// 时绝不覆盖 —— 那是两个不同的东西，覆盖只会把卡片改成四不像
		//（真机上 gitea 就撞过：目录里是 Docker 推荐项，表里是 brew 服务）。
		if !sameRail(apps[i], spec) {
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

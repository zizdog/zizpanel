package services

import (
	"context"
	"strings"
	"testing"
)

// TestUninstallDependentsRealEvidence 逐条锁住依赖引擎的判据（全部基于真实证据）。
//
// 用户 2026-09-21 原话："如果我要卸载 ffmpeg，tts 需要用它，就要提示必须先卸载 tts。
// 卸载 php 也一样，有没有网站正在用它 …… 只要有运行中的容器，就要提醒用户。"
func TestUninstallDependentsRealEvidence(t *testing.T) {
	m := matrixManager(t)
	ctx := context.Background()

	ffmpegApp, ok := FindApp("ffmpeg")
	if !ok {
		t.Fatal("目录里应有 ffmpeg")
	}
	php82App, _ := FindApp("php82")

	// ① ffmpeg：面板里真的装着 Qwen3 TTS（服务记录就是证据）→ 必须点名并 block。
	if err := m.repo.Create(ctx, &Service{
		Name: "com-zizdog-qwen3tts", DisplayName: "Qwen3 TTS", Kind: KindNative,
		LaunchLabel: qwenLabel, Managed: false,
	}); err != nil {
		t.Fatal(err)
	}
	deps := m.UninstallDependents(ctx, ffmpegApp, nil)
	if !hasDependent(deps, "app", "Qwen3 TTS（语音合成）") {
		t.Errorf("装了 TTS 时依赖检测应点名它，实际 %+v", deps)
	}
	plan := m.PlanUninstall(ctx, "ffmpeg")
	if plan.Blocked == "" {
		t.Error("有 TTS 在用时卸载 ffmpeg 必须被 block")
	}
	if len(plan.Dependents) == 0 {
		t.Error("依赖必须结构化透出（dependents），不能只有一句 blocked 文案")
	}
	// 没有依赖时**不许**误 block：构造一个干净的 Manager。
	clean := matrixManager(t)
	if p := clean.PlanUninstall(ctx, "ffmpeg"); p.Blocked != "" {
		t.Errorf("没有依赖时不应被 block，实际 %q", p.Blocked)
	}

	// ② php：站点正在用 8.2 → 点名站点 + 给出"先切版本"的动作。
	m.opt.SiteDependents = func() []SiteRef {
		return []SiteRef{{Domain: "a.test", PHPVersion: "8.2", Enabled: true},
			{Domain: "b.test", PHPVersion: "8.4", Enabled: true}}
	}
	m.siteRefsLoaded = false
	deps = m.UninstallDependents(ctx, php82App, nil)
	if !hasDependent(deps, "site", "a.test") {
		t.Errorf("php82 应点名用 8.2 的站点，实际 %+v", deps)
	}
	if hasDependent(deps, "site", "b.test") {
		t.Errorf("用 8.4 的站点不该被算成 php82 的依赖，实际 %+v", deps)
	}
	for _, d := range deps {
		if d.Kind == "site" && !strings.Contains(d.Action, "切换") && !strings.Contains(d.Action, "切到") {
			t.Errorf("站点的建议动作必须说清\"先切版本\"：%+v", d)
		}
	}

	// ③ mysql：任意站点存在 → 点名（站点会连不上库）。
	mysqlApp, _ := FindApp("mysql84")
	deps = m.UninstallDependents(ctx, mysqlApp, nil)
	if !hasDependent(deps, "site", "a.test") {
		t.Errorf("有站点时卸载 MySQL 必须点名它们，实际 %+v", deps)
	}

	// ④ docker 运行时：有正在运行的容器 → 默认 block 并列出容器名。
	dockerApp, _ := FindApp("docker-runtime")
	m.dockerPSProbe = func(context.Context) ([]string, bool) { return []string{"redis", "gitea"}, true }
	deps = m.UninstallDependents(ctx, dockerApp, nil)
	if !hasDependent(deps, "container", "redis") || !hasDependent(deps, "container", "gitea") {
		t.Errorf("应逐条列出运行中的容器，实际 %+v", deps)
	}
	dp := m.PlanUninstallFor(context.Background(), dockerApp, nil)
	if dp.Blocked == "" || len(dp.Dependents) == 0 {
		t.Errorf("有运行中容器时删除运行时必须 block 并给出依赖，实际 %+v", dp)
	}

	// ⑤ 连不上 docker：如实写"未能列出容器"，绝不假装没有容器。
	m.dockerPSProbe = func(context.Context) ([]string, bool) { return nil, false }
	deps = m.UninstallDependents(ctx, dockerApp, nil)
	if !hasDependent(deps, "container", "未能列出容器") {
		t.Errorf("连不上引擎时要如实说未列出，实际 %+v", deps)
	}
}

func hasDependent(deps []Dependent, kind, name string) bool {
	for _, d := range deps {
		if d.Kind == kind && d.Name == name {
			return true
		}
	}
	return false
}

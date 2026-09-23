// 从插件声明**派生**面板需要的东西：卡片事实、安装步骤（计划级）、卸载计划。
//
// B1 的用法：这些派生结果与现有 Go 目录定义**逐字段比对**（internal/services/
// plugin_equiv_test.go），一致才允许让表去驱动；不一致就红 —— 这就是"双记账"。
package plugins

import (
	"fmt"
	"strings"
)

// CardFacts 是市场卡片/服务列表要用的事实（只列**表已覆盖**的字段，别贪多）。
type CardFacts struct {
	ID           string
	Name         string
	Icon         string
	Summary      string
	Port         int
	HealthPath   string
	ConfigPath   string // 已展开 ~/，未展开则由调用方给 home
	SystemDaemon bool
	UISlug       string
	UIKind       string
}

// CardFacts 从声明派生卡片事实；home 用于展开 ~/。
func (s *Spec) CardFacts(home string) CardFacts {
	f := CardFacts{
		ID:      s.ID,
		Name:    s.Name,
		Icon:    s.Icon,
		Summary: s.Summary,
	}
	if s.Expose != nil {
		f.Port = s.Expose.Port
		f.UISlug = s.ID
		f.UIKind = s.Expose.UI
	}
	if s.Health.Kind == "http" {
		f.HealthPath = s.Health.Path
	}
	if s.Config != nil {
		f.ConfigPath = Home(s.Config.Path, home)
	}
	if s.Requires != nil {
		f.SystemDaemon = s.Requires.SystemDaemon
	}
	return f
}

// InstallSteps 返回**计划级**的安装步骤（人类可读 + 稳定顺序）。
//
// 它刻意只到"步骤骨架"这一层：真正每一步怎么做仍由面板的执行器决定。
// 之所以能做等价性比对，是因为骨架（取件→校验→落配置→注册服务→验收→登记）
// 对每一类 rail 都是固定的；步骤内部的细节留给各自的执行器。
func (s *Spec) InstallSteps() []string {
	var steps []string
	switch s.Source.Kind {
	case "brew":
		steps = append(steps, "brew install "+s.Source.Formula)
	case "release":
		steps = append(steps,
			"从镜像/上游取 "+s.Source.Asset,
			"校验强度 "+orDash(s.Source.Checksum)+" 复核后解包并取 "+s.Source.Binary)
	case "compose":
		steps = append(steps, "写 docker compose 并拉起容器")
	}
	if s.Config != nil {
		if strings.TrimSpace(s.Config.Seed) == "" {
			steps = append(steps, "登记配置文件位置（内容由应用自己创建）")
		} else {
			steps = append(steps, "写入配置（模板 "+s.Config.Seed+"，权限 "+orDefault(s.Config.Mode, "0600")+"）")
		}
	}
	switch s.Run.Mode {
	case "panel-daemon":
		steps = append(steps, "注册系统级守护进程（面板托管，继承面板 TCC 授权）")
	case "app-daemon":
		steps = append(steps, "注册系统级守护进程（launchd 直接运行应用二进制）")
	case "brew-service":
		steps = append(steps, "注册并启动 brew service")
	case "compose":
		steps = append(steps, "等待容器就绪")
	case "none":
		steps = append(steps, "无守护进程（由 nginx/面板提供入口）")
	}
	steps = append(steps, "等待验收探针通过（失败即报错，端口在听不算）", "登记进「服务管理」")
	return steps
}

// UninstallFacts 是从声明派生的卸载事实（与面板卸载对话框语义一致）。
type UninstallFacts struct {
	Always       []string // 一定删
	OptionalData []string // 勾选「删除数据」才删
	Formula      string
	KeepNote     string
}

// UninstallFacts 派生卸载计划（home 用于展开 ~/）。
func (s *Spec) UninstallFacts(home string) UninstallFacts {
	f := UninstallFacts{Formula: s.Uninstall.Formula, KeepNote: s.Uninstall.KeepNote}
	for _, p := range s.Uninstall.Always {
		f.Always = append(f.Always, Home(p, home))
	}
	for _, p := range s.Uninstall.OptionalData {
		f.OptionalData = append(f.OptionalData, Home(p, home))
	}
	return f
}

// ProbeSummary 给一行人类可读的探针描述（日志/自检用）。
func (s *Spec) ProbeSummary() string {
	var out []string
	for _, p := range s.Verify.AnyOf {
		out = append(out, probeText(p))
	}
	return fmt.Sprintf("verify=%s；health=%s", strings.Join(out, " 或 "), probeText(s.Health.Probe))
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// Homebrew 目录缺失：如实报告 + **用户点击才执行**的修复（坑 186）。

// handleBrewDirs 返回上一次「未能复核已装软件」的真实原因与可操作建议。
// 只读、**不跑任何命令**（只解析内存里保存的上次失败原因），所以首屏可以便宜地调用。
func (s *Server) handleBrewDirs(w http.ResponseWriter, r *http.Request) {
	probeErr := marketProbeErrorForAPI()
	resp := map[string]any{
		"brew_probe_ok": probeErr == "",
		"error":         probeErr,
		"repairable":    false,
	}
	// 只有确实是「目录缺失」这一类才给"可修复"建议；别的失败如实显示原文。
	if d, okd := services.DiagnoseBrewDirError(probeErr); okd {
		own := services.BrewDirOwnerOf(d.Prefix)
		resp["repairable"] = true
		resp["missing_dir"] = d.MissingDir
		resp["prefix"] = d.Prefix
		resp["advice"] = services.FormatBrewDirAdvice(d, own)
		resp["command"] = services.BrewDirInstallCommand(d.MissingDir, own)
	}
	ok(w, resp)
}

// handleBrewRepairDirs 补建缺失的 Homebrew 目录（用户点击才执行，绝不自动改系统）。
// 走任务中心（202 + task_id）；成功的唯一判据是回读 `brew list --versions` 跑通。
func (s *Server) handleBrewRepairDirs(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	// 上次让市场"未能复核"的那个目录一并纳入候选；不属于当前前缀的会被如实跳过。
	extra := []string{}
	if d, okd := services.DiagnoseBrewDirError(marketProbeErrorForAPI()); okd {
		extra = append(extra, d.MissingDir)
	}
	s.launchTask(w, r, "brew-repair", "brew-dirs", "补建 Homebrew 缺失目录", "brew_repair_dirs",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			res := &services.InstallResult{Steps: []string{}}
			err := mgr.RepairBrewDirs(ctx, res, extra...)
			if err == nil {
				// 修好了 → "未能复核"的旧结论必须立刻作废，别让用户等 TTL。
				s.InvalidateMarketCache()
			}
			return res, err
		})
}

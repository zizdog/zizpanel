package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/sysconfig"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  「系统设置」页的接口：把 macOS 的服务器化配置做成面板里可点的按钮。
//
//  设计要点（对应用户的要求"点击一键执行系列命令"）：
//   - 每个动作都走**任务中心**：用户能看见到底跑了哪几条命令、每条的输出与复核结果，
//     而不是一个转圈然后"完成"。关掉窗口也不会中断（任务在后台跑）。
//   - 状态探测是**只读**的，随时可以刷新；判断依据写在界面上（偏好值 + hosts 标记），
//     不靠"上次点过按钮"这种记忆。
//   - 明确区分"本机不支持"与"没设置"：pmset 不支持的键会直接标出来
//     （因为 `pmset -a autorestart 1` 在不支持的机器上会静默返回成功）。
// ============================================================================

// handleSystemSettings 返回系统设置的当前真实状态。
func (s *Server) handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	st := sysconfig.Probe(r.Context())
	// 内网段预授权单独走可注入的探针（见 api_systemsettings_lan.go），
	// 这样它的状态探测与写入动作都能在单测里换成假实现，绝不碰真实偏好域。
	st.LANPreauth = withLANWarning(lanPreauthProbeFn(r.Context()))
	ok(w, st)
}

// handleSystemSettingsAction 执行一个设置动作（异步任务）。
func (s *Server) handleSystemSettingsAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")

	type spec struct {
		title string
		audit string
		run   func(ctx context.Context, log tasks.LogFunc) error
	}
	specs := map[string]spec{
		"server-mode": {
			title: "一键设为服务器模式（电源 + 阻断更新 + 静默诊断 + 关索引 + 开 SSH）",
			audit: "sysconfig_server_mode",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.ApplyServerMode(ctx, sysconfig.LogFunc(log))
			},
		},
		"power": {
			title: "关闭睡眠与节能策略",
			audit: "sysconfig_power",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.ApplyServerPower(ctx, sysconfig.LogFunc(log))
			},
		},
		"block-updates": {
			title: "彻底阻止系统更新",
			audit: "sysconfig_block_updates",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.BlockUpdates(ctx, sysconfig.LogFunc(log))
			},
		},
		"restore-updates": {
			title: "恢复系统更新（撤销阻断）",
			audit: "sysconfig_restore_updates",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.RestoreUpdates(ctx, sysconfig.LogFunc(log))
			},
		},
		"verify-updates": {
			title: "验证更新阻断（真的去问一次系统）",
			audit: "sysconfig_verify_updates",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.VerifyUpdatesBlocked(ctx, sysconfig.LogFunc(log))
			},
		},
		"silence-diagnostics": {
			title: "关闭崩溃报告弹窗与诊断上报",
			audit: "sysconfig_silence_diagnostics",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.SilenceDiagnostics(ctx, sysconfig.LogFunc(log))
			},
		},
		"spotlight-off": {
			title: "关闭 Spotlight 索引",
			audit: "sysconfig_spotlight_off",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.SetSpotlight(ctx, false, sysconfig.LogFunc(log))
			},
		},
		"spotlight-on": {
			title: "开启 Spotlight 索引",
			audit: "sysconfig_spotlight_on",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.SetSpotlight(ctx, true, sysconfig.LogFunc(log))
			},
		},
		"enable-ssh": {
			title: "开启远程登录（SSH）",
			audit: "sysconfig_enable_ssh",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.EnableSSH(ctx, sysconfig.LogFunc(log))
			},
		},
		"disable-autologin": {
			title: "关闭自动登录",
			audit: "sysconfig_disable_autologin",
			run: func(ctx context.Context, log tasks.LogFunc) error {
				return sysconfig.DisableAutoLogin(ctx, sysconfig.LogFunc(log))
			},
		},
	}

	sp, found := specs[action]
	if !found {
		fail(w, http.StatusBadRequest, "未知的系统设置动作："+action)
		return
	}
	// target 用动作名：同一个动作不会被重复点成两个任务（并发保护）
	s.launchTask(w, r, "sysconfig", action, sp.title, sp.audit,
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			// 直接把任务日志当 sysconfig 的日志：用户要看到每条命令与输出。
			return map[string]any{"action": action}, sp.run(ctx, log)
		})
}

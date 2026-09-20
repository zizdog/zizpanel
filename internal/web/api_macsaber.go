package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// macSaberInstallFn 是安装入口；单测注入假实现（真实安装要联网下载 + 碰 launchd，
// 单测绝不允许跑 —— AGENTS 第三节）。
var macSaberInstallFn = func(s *Server, ctx context.Context, app services.App, res *services.InstallResult) error {
	return s.svcManager().InstallMacSaber(ctx, app, res)
}

// handleInstallMacSaber 安装应用市场里的「mac军刀」。
//
// 为什么走自研安装器而不是通用流程（两条都不满足）：
//   - **没有上游**：产物是本仓库 macsaber/ 自己编的 darwin/arm64 单二进制，
//     只发公网镜像站 apps/macsaber/<ver>/，releaseBinaryApps 那套（GitHub 回落）
//     在这里没有意义；
//   - **服务形态不同**：它的可执行文件是**面板自己的二进制**
//     （`zizpanel macsaber-supervise`），装成系统级 LaunchDaemon
//     （label cn.zizpanel.macsaber）—— 与面板同一代码要求，所以共用面板的
//     TCC 授权；supervisor 以 root fork 后 setuid 到真实用户跑 macsaber。
//
// 走任务中心：整包下载 + 起服务不是秒级动作，同步请求会让用户关掉窗口就
// 找不回进度（任务挂在 r.Context() 上，一刷新就把它杀了）。
func (s *Server) handleInstallMacSaber(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found || app.PanelInstaller != services.MacSaberAppID {
		fail(w, http.StatusBadRequest, "应用市场中找不到 mac军刀 条目 "+id)
		return
	}
	// 用 launchInstallTask（= launchTask + netHinted）：下载失败是网络类错误时
	// 统一附上提示（判据与文案的唯一来源是 services/netfail.go）。
	s.launchInstallTask(w, r, "install", id,
		"安装 "+app.Name+"（"+services.MacSaberVersion+"，仅镜像站）",
		"install_macsaber", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: app.ID, Steps: []string{}}
			if err := macSaberInstallFn(s, ctx, app, res); err != nil {
				return res, err
			}
			res.Name = app.Name
			res.Message = "mac军刀 " + services.MacSaberVersion + " 已就绪"
			return res, nil
		})
}

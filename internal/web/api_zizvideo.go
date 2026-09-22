package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// zizvideoInstallFn 是安装入口；单测注入假实现（真实安装要碰 launchd，
// 单测绝不允许跑 —— AGENTS 第三节）。
var zizvideoInstallFn = func(s *Server, ctx context.Context, app services.App, res *services.InstallResult) error {
	return s.svcManager().InstallZizvideo(ctx, app, res)
}

// handleInstallZizvideo 安装应用市场里的「zizvideo」（短视频模块）。
//
// 为什么走自研安装器而不是通用流程：
//   - **没有上游**：二进制不随面板包分发（2026-09-22 起），安装时从应用包镜像
//     apps/zizvideo/<版本>/ 按需下载并核 sha256（本地开发用面板旁的 <bin>/zizvideo）；
//   - **服务形态不同**：它的可执行文件是**面板自己的二进制**
//     （`zizpanel zizvideo-supervise`），装成系统级 LaunchDaemon
//     （label cn.zizpanel.zizvideo）—— 与面板同一代码要求，所以与面板**共用文件权限**；
//     supervisor 以 root fork 后 setuid 到真实用户跑 zizvideo。
//
// 走任务中心：写 plist + bootstrap + 等 /healthz 就绪不是秒级动作。
func (s *Server) handleInstallZizvideo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found || app.PanelInstaller != services.ZizvideoAppID {
		fail(w, http.StatusBadRequest, "应用市场中找不到 zizvideo 条目 "+id)
		return
	}
	s.launchInstallTask(w, r, "install", id,
		"安装 "+app.Name+"（从镜像站下载）",
		"install_zizvideo", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: app.ID, Steps: []string{}}
			if err := zizvideoInstallFn(s, ctx, app, res); err != nil {
				return res, err
			}
			res.Name = app.Name
			res.Message = "zizvideo " + services.ZizvideoVersion + " 已就绪"
			return res, nil
		})
}

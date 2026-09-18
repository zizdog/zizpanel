package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// handleInstallMacSpeech 安装应用市场里的「macOS 语音合成（say）」。
//
// 与别的面板安装器最大的不同：**它不做任何下载**。
//
//	① 复核引擎真的能用（/usr/bin/say 在、`say -v '?'` 报得出音色）；
//	② 把面板托管的网页界面注册成系统级 launchd（com.zizdog.macosspeech）；
//	③ 等 /healthz 真的报 ok:true（能力探活，不是"端口在听就算成功"）。
//
// 因为不下载，它走同步快路径没有意义 —— 仍然统一走任务中心：
// 用户看到的是同一套日志/进度界面，安装历史也可查（与其它条目一致）。
func (s *Server) handleInstallMacSpeech(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found || app.PanelInstaller != "macspeech" {
		fail(w, http.StatusBadRequest, "应用市场中找不到 macOS 语音合成条目 "+id)
		return
	}
	s.launchTask(w, r, "install", id, "安装 "+app.Name+"（系统自带 say，无需下载）",
		"install_macspeech", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: app.ID, Steps: []string{}}
			if err := s.svcManager().InstallMacSpeech(ctx, app, res); err != nil {
				return res, err
			}
			return res, nil
		})
}

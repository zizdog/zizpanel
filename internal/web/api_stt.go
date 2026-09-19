package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// sttInstallFn 是安装入口；单测注入假实现（真实安装要联网 + brew，单测不许跑）。
var sttInstallFn = func(s *Server, ctx context.Context, app services.App, res *services.InstallResult) error {
	return s.svcManager().InstallSTT(ctx, app, res)
}

// handleInstallSTT 安装应用市场里的「语音转文字（whisper.cpp）」。
//
// 四步，每一步都真的复核（不靠"上一步退出码 0"）：
//
//	① 补齐基础依赖（ffmpeg）—— 转码链路要用它，依赖关系从目录的 Requires 读；
//	② `brew install whisper.cpp`（走多源兜底：镜像 → 清华 → 官方）；
//	③ 下载并校验默认档模型（small；判据是精确字节数 + ggml 魔数）；
//	④ 注册面板托管的网页界面 launchd 服务，并等 /healthz **真的报 ok:true**
//	   ——那一刻 = 模型在 + 引擎在 + 真的跑通了一次极短音频转写。
//
// 全程走任务中心：brew 装瓶与模型下载都是分钟级动作，同步请求会让用户
// 关掉窗口就找不回进度，而且任务挂在 r.Context() 上 —— 用户一刷新就把
// brew 杀了（AGENTS 第三节）。
func (s *Server) handleInstallSTT(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found || app.PanelInstaller != "stt" {
		fail(w, http.StatusBadRequest, "应用市场中找不到语音转文字条目 "+id)
		return
	}
	s.launchTask(w, r, "install", id,
		"安装 "+app.Name+"（"+services.STTBrewFormula+" + 默认档模型）",
		"install_stt", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: app.ID, Steps: []string{}}
			if err := sttInstallFn(s, ctx, app, res); err != nil {
				// brew 装瓶 + 模型下载都是联网动作：网络类失败附统一提示（见 services/netfail.go）。
				return res, services.AppendNetworkHint(err)
			}
			return res, nil
		})
}

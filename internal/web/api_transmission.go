package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// transmissionInstallFn 是安装入口；单测注入假实现（真实安装要联网 + brew，单测不许跑）。
var transmissionInstallFn = func(s *Server, ctx context.Context, res *services.InstallResult) error {
	return s.svcManager().InstallTransmission(ctx, res)
}

// handleInstallTransmission 安装 Transmission（下载器）并设好 RPC 口令。
//
// 与其它面板安装器一样走任务中心（202 + task_id，进度走 SSE）：brew install 会拉
// libevent/libpsl/miniupnpc 依赖，动辄几分钟；同步请求挂在 r.Context() 上，
// 用户一刷新就把 brew 杀了。
//
// 口令只通过 InstallResult.Credentials（一次性凭据区块）返回，任务步骤与实时
// 日志里没有它（transmission 写进 settings.json 的明文也会被它自己换成哈希）。
func (s *Server) handleInstallTransmission(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "transmission", "安装 Transmission（下载）",
		"install_transmission", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "transmission", Name: "Transmission（下载）", Steps: []string{}}
			if err := transmissionInstallFn(s, ctx, res); err != nil {
				// brew 装包是联网动作：网络类失败附统一提示（见 services/netfail.go）。
				return res, services.AppendNetworkHint(err)
			}
			return res, nil
		})
}

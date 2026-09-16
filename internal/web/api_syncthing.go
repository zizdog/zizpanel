package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// handleInstallSyncthing 安装 Syncthing（文件同步）并把它的 Web GUI 安全地
// 开放到局域网。
//
// 与其它面板安装器一样走任务中心（202 + task_id，进度走 SSE）：brew install /
// brew services start / 两段各 60 秒的就绪轮询，动辄几分钟；同步请求挂在
// r.Context() 上，用户一刷新就把 brew 杀了。
//
// 口令只通过 InstallResult.Credentials（一次性凭据区块）返回，任务步骤与
// 实时日志里没有它。
func (s *Server) handleInstallSyncthing(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "syncthing", "安装 Syncthing（文件同步）",
		"install_syncthing", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "syncthing", Name: "Syncthing（文件同步）", Steps: []string{}}
			if err := s.svcManager().InstallSyncthing(ctx, res); err != nil {
				return res, err
			}
			return res, nil
		})
}

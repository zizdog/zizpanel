package web

import (
	"context"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// handleInstallMiniflux 安装 Miniflux（RSS 阅读器）及其 PostgreSQL 17 数据库。
//
// 与其它面板安装器一样走任务中心（202 + task_id，进度走 SSE）：
// brew install + 建库建角色 + 迁移 + 起服务动辄几分钟，同步请求挂在
// r.Context() 上，用户一刷新就把 brew 杀了。
func (s *Server) handleInstallMiniflux(w http.ResponseWriter, r *http.Request) {
	s.launchTask(w, r, "install", "miniflux", "安装 Miniflux（RSS 阅读器）",
		"install_miniflux", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "miniflux", Name: "Miniflux（RSS 阅读器）", Steps: []string{}}
			if err := s.svcManager().InstallMiniflux(ctx, res); err != nil {
				return res, err
			}
			return res, nil
		})
}

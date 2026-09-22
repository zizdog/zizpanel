package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

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

// ============================================================================
//  Transmission RPC 设置（用户 2026-09-21 报障：手改 settings.json 不生效）
//
//  面板必须提供正规入口：动作固定「停 → 等端口释放 → 写 → 启动 → 回读逐字段核对」
//  （services.SetTransmissionRPCSettings，坑 226）。手工编辑 settings.json 后直接重启
//  会被 daemon 退出时的内存回写覆盖，所以界面上要明确劝退。
// ============================================================================

type transmissionSettingsReq struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DownloadDir string `json:"download_dir"`
}

var errNotTransmission = errors.New("不是面板托管的 Transmission 服务")

// transmissionSettingsReadFn / transmissionSettingsApplyFn 是"读/改 RPC 设置"的注入点：
// 单测不许真停 brew 服务、真改 /opt/homebrew/var/transmission（与上面的安装入口同一约定）。
var (
	transmissionSettingsReadFn = func(m *services.Manager) (services.TransmissionSettingsInfo, error) {
		return m.TransmissionSettingsInfo()
	}
	transmissionSettingsApplyFn = func(m *services.Manager, ctx context.Context, res *services.InstallResult,
		username, password, downloadDir string) (services.TransmissionSettingsInfo, error) {
		return m.SetTransmissionRPCSettings(ctx, res, username, password, downloadDir)
	}
)

// ensureTransmissionService 用 launchd label 判据确认目标（brew 两套前缀都认）。
func (s *Server) ensureTransmissionService(ctx context.Context, name string) error {
	svc, err := s.svcManager().Get(ctx, name)
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(svc.LaunchLabel)) {
	case "homebrew.mxcl.transmission-cli", "sh.brew.transmission-cli":
		return nil
	}
	return errNotTransmission
}

// handleTransmissionSettingsGet 读当前 RPC 用户名 / 下载目录（口令只报"有没有设置"）。
func (s *Server) handleTransmissionSettingsGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.ensureTransmissionService(r.Context(), name); err != nil {
		fail(w, http.StatusBadRequest, "这个操作只支持面板托管的 Transmission："+err.Error())
		return
	}
	info, err := transmissionSettingsReadFn(s.svcManager())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, info)
}

// handleTransmissionSettingsSet 改 RPC 凭据 / 下载目录（仅管理员，走任务中心）。
func (s *Server) handleTransmissionSettingsSet(w http.ResponseWriter, r *http.Request) {
	if u := userFrom(r.Context()); u == nil || !u.IsAdmin {
		fail(w, http.StatusForbidden, "只有管理员能修改 Transmission 的 RPC 设置")
		return
	}
	name := r.PathValue("name")
	if err := s.ensureTransmissionService(r.Context(), name); err != nil {
		fail(w, http.StatusBadRequest, "这个操作只支持面板托管的 Transmission："+err.Error())
		return
	}
	var req transmissionSettingsReq
	if err := decodeOptional(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.svcManager()
	s.launchTask(w, r, "transmission-settings", name, "修改 Transmission RPC 设置", "transmission_set_rpc",
		func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: "transmission", Name: "Transmission（下载）", Steps: []string{}}
			info, err := transmissionSettingsApplyFn(mgr, ctx, res, req.Username, req.Password, req.DownloadDir)
			if err != nil {
				return res, err
			}
			res.Message = "RPC 设置已保存并回读生效（下载目录 " + info.DownloadDir + "）"
			return res, nil
		})
}

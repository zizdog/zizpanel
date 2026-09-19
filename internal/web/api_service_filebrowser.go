package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  File Browser「主目录」设置（用户 2026-09-19 要求）
//
//  用户原话："面板里要能改主目录并重启服务生效（一个输入框 + 保存 → 改 plist 的 -r
//  → 重启 → 回读生效值，读不到就说未复核）"。
//
//  读取/写入/回读全部在 services/filebrowser_settings.go 里（可直接单测），
//  这里只做鉴权、参数解析与审计。
//  ============================================================================

type filebrowserRootReq struct {
	Root string `json:"root"`
}

// errNotFilebrowser 表示目标服务不是面板托管的 File Browser。
var errNotFilebrowser = errors.New("不是面板托管的 File Browser 服务")

// ensureFilebrowserService 确认 {name} 这条服务就是面板托管的 File Browser。
//
// 不靠"ID 猜"：服务记录名可能是目录 ID（filebrowser）、也可能是 label
// （com.zizdog.filebrowser）或显示名，唯一可靠的判据是它的 launchd label。
func (s *Server) ensureFilebrowserService(ctx context.Context, name string) error {
	svc, err := s.svcManager().Get(ctx, name)
	if err != nil {
		return err
	}
	if svc.LaunchLabel != services.FilebrowserServiceLabel {
		return errNotFilebrowser
	}
	return nil
}

// handleFilebrowserRootGet 读取当前主目录（真实来源：launchd plist 的 -r）。
func (s *Server) handleFilebrowserRootGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.ensureFilebrowserService(r.Context(), name); err != nil {
		fail(w, http.StatusBadRequest, "这个操作只支持面板托管的 File Browser："+err.Error())
		return
	}
	info, err := s.svcManager().FilebrowserRootInfo()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, info)
}

// handleFilebrowserRootSet 改主目录：校验 → 改 plist 的 -r → 重启 → 回读核对。
func (s *Server) handleFilebrowserRootSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.ensureFilebrowserService(r.Context(), name); err != nil {
		fail(w, http.StatusBadRequest, "这个操作只支持面板托管的 File Browser："+err.Error())
		return
	}
	var req filebrowserRootReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := s.svcManager().SetFilebrowserRoot(r.Context(), req.Root)
	if err != nil {
		s.audit(r, "filebrowser_set_root", name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "filebrowser_set_root", name,
		"root="+info.Root+" verified="+boolText(info.Verified), true, "")
	ok(w, info)
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

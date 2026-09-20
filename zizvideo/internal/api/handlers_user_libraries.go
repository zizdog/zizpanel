package api

import (
	"net/http"
	"strconv"

	"github.com/zizdog/zizvideo/internal/domain"
)

// P3 管理端：用户可访问库的整体替换 + 默认可见库预览/补发。
// 可见性判据仍只在 api/authz.go 的 resolveScope；本文件只写 user_libraries 行（source 仅标注）。

type userLibrariesView struct {
	UserID     string                    `json:"user_id"`
	LibraryIDs []string                  `json:"library_ids"`
	Grants     []domain.UserLibraryGrant `json:"grants"`
}

func (s *Server) userLibrariesView(userID string) (userLibrariesView, error) {
	grants, err := s.DB.ListUserLibraryGrants(userID)
	if err != nil {
		return userLibrariesView{}, err
	}
	ids := make([]string, 0, len(grants))
	for _, g := range grants {
		ids = append(ids, g.LibraryID)
	}
	return userLibrariesView{UserID: userID, LibraryIDs: ids, Grants: grants}, nil
}

// HandleGetUserLibraries 回读某用户的授权行（含来源，仅显示用）。
func (s *Server) HandleGetUserLibraries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.DB.GetUserByID(id); err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.userLibrariesView(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, view, nil)
}

type userLibrariesReq struct {
	LibraryIDs []string `json:"library_ids"`
}

// HandlePutUserLibraries 整体替换授权：未勾选整行删除、勾选归一 source='admin'；
// 响应是提交后从 DB 重新读出的行（回读不一致就不算成功）。
func (s *Server) HandlePutUserLibraries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req userLibrariesReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.DB.GetUserByID(id); err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.DB.SetUserLibraries(id, cleanIDs(req.LibraryIDs)); err != nil {
		s.audit(r, "user.libraries", "user:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	view, err := s.userLibrariesView(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "user.libraries", "user:"+id, true, "count:"+strconv.Itoa(len(view.LibraryIDs)))
	respond(w, http.StatusOK, view, nil)
}

type defaultLibrariesView struct {
	DefaultLibraryIDs     []string `json:"default_library_ids"`
	DefaultCount          int      `json:"default_count"`
	UsersTotal            int      `json:"users_total"`
	UsersWithoutLibraries int      `json:"users_without_libraries"`
}

func (s *Server) buildDefaultLibrariesView() (defaultLibrariesView, error) {
	ids, err := s.DB.DefaultLibraryIDs()
	if err != nil {
		return defaultLibrariesView{}, err
	}
	total, err := s.DB.CountUsers()
	if err != nil {
		return defaultLibrariesView{}, err
	}
	noLib, err := s.DB.CountUsersWithoutLibraries()
	if err != nil {
		return defaultLibrariesView{}, err
	}
	return defaultLibrariesView{
		DefaultLibraryIDs: ids, DefaultCount: len(ids),
		UsersTotal: total, UsersWithoutLibraries: noLib,
	}, nil
}

// HandleGetDefaultLibraries 给出默认库集合与"未授权用户"人数（补发按钮的影响人数）。
func (s *Server) HandleGetDefaultLibraries(w http.ResponseWriter, r *http.Request) {
	view, err := s.buildDefaultLibrariesView()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, view, nil)
}

type backfillReq struct {
	Confirm bool `json:"confirm"`
}

// HandleBackfillDefaultLibraries 同步补发（用户数少，本阶段不走任务中心）。
// 未设默认 ⇒ 409（绝不"补发 0 个还报成功"）；幂等，只碰 0 授权的普通用户。
func (s *Server) HandleBackfillDefaultLibraries(w http.ResponseWriter, r *http.Request) {
	var req backfillReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !req.Confirm {
		s.fail(w, r, domain.New("VALIDATION_CONFIRM", "请先确认补发操作", 400))
		return
	}
	ids, err := s.DB.DefaultLibraryIDs()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(ids) == 0 {
		s.audit(r, "libraries.default_backfill", "unset", false, "DEFAULT_LIBRARIES_UNSET")
		s.fail(w, r, domain.New("DEFAULT_LIBRARIES_UNSET", "未设置默认可见库，无法补发", 409))
		return
	}
	res, err := s.DB.BackfillDefaultLibraries()
	if err != nil {
		s.audit(r, "libraries.default_backfill", "error", false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "libraries.default_backfill", "granted:"+strconv.Itoa(res.UsersGranted), true,
		"skipped:"+strconv.Itoa(res.UsersSkipped))
	respond(w, http.StatusOK, res, nil)
}

package api

import (
	"net/http"
	"strings"

	"github.com/zizdog/govideo/internal/auth"
	"github.com/zizdog/govideo/internal/domain"
	"github.com/zizdog/govideo/internal/storage"
)

// HandleListUsers returns every live account (admin only).
func (s *Server) HandleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.DB.ListUsers()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"list": users}, nil)
}

type createUserReq struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// HandleCreateUser adds an account.
func (s *Server) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !validUsername(req.Username) {
		s.fail(w, r, domain.New("VALIDATION_USERNAME", "用户名需 3-32 位字母数字或 _-.", 400))
		return
	}
	if len(req.Password) < 8 {
		s.fail(w, r, domain.New("VALIDATION_PASSWORD", "口令至少 8 位", 400))
		return
	}
	if req.Role == "" {
		req.Role = domain.RoleUser
	}
	if req.Role != domain.RoleAdmin && req.Role != domain.RoleUser {
		s.fail(w, r, domain.New("VALIDATION_ROLE", "角色只能是 admin 或 user", 400))
		return
	}
	if _, err := s.DB.GetUserByUsername(req.Username); err == nil {
		s.fail(w, r, domain.ErrConflict)
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := &domain.User{
		ID: domain.NewID("usr"), Username: req.Username,
		DisplayName: strings.TrimSpace(req.DisplayName), PasswordHash: hash,
		Role: req.Role, Status: domain.StatusActive,
	}
	if u.DisplayName == "" {
		u.DisplayName = u.Username
	}
	if err := s.DB.CreateUser(u); err != nil {
		s.audit(r, "user.create", "user:"+req.Username, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "user.create", "user:"+u.ID, true, "")
	respond(w, http.StatusCreated, u, nil)
}

type patchUserReq struct {
	DisplayName *string `json:"display_name"`
	Role        *string `json:"role"`
	Status      *string `json:"status"`
	Password    *string `json:"password"`
}

// HandlePatchUser updates role, status, display name or password.
func (s *Server) HandlePatchUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req patchUserReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	target, err := s.DB.GetUserByID(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	me := UserFrom(r.Context())

	if req.Role != nil && *req.Role != domain.RoleAdmin && *req.Role != domain.RoleUser {
		s.fail(w, r, domain.New("VALIDATION_ROLE", "角色只能是 admin 或 user", 400))
		return
	}
	if req.Status != nil && *req.Status != domain.StatusActive && *req.Status != domain.StatusDisabled {
		s.fail(w, r, domain.New("VALIDATION_STATUS", "状态只能是 active 或 disabled", 400))
		return
	}
	losingAdmin := target.Role == domain.RoleAdmin &&
		((req.Role != nil && *req.Role != domain.RoleAdmin) ||
			(req.Status != nil && *req.Status == domain.StatusDisabled))
	if losingAdmin {
		if me != nil && me.ID == target.ID {
			if req.Status != nil && *req.Status == domain.StatusDisabled {
				s.fail(w, r, domain.New("FORBIDDEN_SELF", "不能禁用当前账号", 409))
				return
			}
		}
		n, err := s.adminCount()
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if n <= 1 {
			s.fail(w, r, domain.New("FORBIDDEN_LAST_ADMIN", "必须保留至少一个管理员", 409))
			return
		}
	}

	patch := storage.UserPatch{DisplayName: req.DisplayName, Role: req.Role, Status: req.Status}
	if req.Password != nil {
		if len(*req.Password) < 8 {
			s.fail(w, r, domain.New("VALIDATION_PASSWORD", "口令至少 8 位", 400))
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		patch.PasswordHash = &hash
	}
	updated, err := s.DB.UpdateUser(id, patch)
	if err != nil {
		s.audit(r, "user.update", "user:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	// Disabling an account or resetting its password must kill live sessions at
	// once; the role/status check already re-reads the DB on every request.
	if (req.Status != nil && *req.Status == domain.StatusDisabled) || req.Password != nil {
		if err := s.DB.DeleteUserSessions(id); err != nil {
			s.Log.Error("撤销会话失败", "user_id", id, "error", err.Error())
		}
	}
	s.audit(r, "user.update", "user:"+id, true, "")
	respond(w, http.StatusOK, updated, nil)
}

func (s *Server) adminCount() (int, error) {
	users, err := s.DB.ListUsers()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		if u.Role == domain.RoleAdmin && u.Status == domain.StatusActive {
			n++
		}
	}
	return n, nil
}

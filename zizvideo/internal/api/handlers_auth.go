package api

import (
	"net/http"
	"strings"
	"sync"

	"github.com/zizdog/zizvideo/internal/auth"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/storage"
)

// audit records every write operation: who, what, on which object, and whether
// it succeeded. Failures to audit are logged but never block the response.
func (s *Server) audit(r *http.Request, action, object string, ok bool, detail string) {
	u := UserFrom(r.Context())
	e := &storage.AuditEntry{
		Action: action, Object: object, OK: ok, Detail: detail,
		RequestID: RequestID(r.Context()),
	}
	if u != nil {
		e.UserID, e.Username = u.ID, u.Username
	}
	if err := s.DB.AddAudit(e); err != nil {
		s.Log.Error("写审计失败", "request_id", RequestID(r.Context()), "error", err.Error())
	}
}

func validUsername(name string) bool {
	if len(name) < 3 || len(name) > 32 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.':
		default:
			return false
		}
	}
	return true
}

// ============================================================================
//  First-run setup
// ============================================================================

var setupMu sync.Mutex

// HandleSetupStatus tells the frontend whether an admin has been created.
func (s *Server) HandleSetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.DB.CountUsers()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"needs_setup": n == 0}, nil)
}

type setupReq struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

// HandleSetup creates the first administrator. It is disabled once any user exists.
func (s *Server) HandleSetup(w http.ResponseWriter, r *http.Request) {
	var req setupReq
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
	setupMu.Lock()
	defer setupMu.Unlock()
	n, err := s.DB.CountUsers()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n > 0 {
		s.fail(w, r, domain.New("VALIDATION_ALREADY_SETUP", "已完成初始化，请直接登录", 409))
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := &domain.User{
		ID: domain.NewID("usr"), Username: req.Username,
		DisplayName:  strings.TrimSpace(req.DisplayName),
		PasswordHash: hash, Role: domain.RoleAdmin, Status: domain.StatusActive,
	}
	if u.DisplayName == "" {
		u.DisplayName = u.Username
	}
	if err := s.DB.CreateUser(u); err != nil {
		s.fail(w, r, err)
		return
	}
	token, err := s.Auth.IssueSession(u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setSessionCookies(w, r, token)
	s.audit(r, "setup.create_admin", "user:"+u.ID, true, "")
	respond(w, http.StatusCreated, u, nil)
}

// ============================================================================
//  Session endpoints
// ============================================================================

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// HandleLogin verifies credentials and opens a session.
func (s *Server) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Username == "" || req.Password == "" {
		s.fail(w, r, domain.New("VALIDATION_CREDENTIALS", "请输入用户名和口令", 400))
		return
	}
	u, token, err := s.Auth.Login(clientIPFrom(r.Context()), req.Username, req.Password)
	if err != nil {
		s.audit(r, "auth.login", "user:"+req.Username, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.setSessionCookies(w, r, token)
	s.audit(r, "auth.login", "user:"+u.ID, true, "")
	respond(w, http.StatusOK, u, nil)
}

// HandleLogout revokes the current session.
func (s *Server) HandleLogout(w http.ResponseWriter, r *http.Request) {
	s.Auth.Logout(sessionToken(r))
	s.clearSessionCookies(w)
	s.audit(r, "auth.logout", "session", true, "")
	respond(w, http.StatusOK, map[string]any{"ok": true}, nil)
}

// HandleMe returns the current user; the frontend uses it as the boot probe.
func (s *Server) HandleMe(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, UserFrom(r.Context()), nil)
}

func errCode(err error) string {
	var de *domain.Error
	if ok := asDomainError(err, &de); ok {
		return de.Code
	}
	return "ERROR"
}

func asDomainError(err error, target **domain.Error) bool {
	for err != nil {
		if de, ok := err.(*domain.Error); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

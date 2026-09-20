package api

import (
	"context"
	"net/http"

	"github.com/zizdog/zizvideo/internal/domain"
)

// LibraryScope 是调用方可见的库集合；判据只此一处（ITERATION-2 B.3）。
type LibraryScope = domain.LibraryScope

// scopeAll 是全见范围的唯一构造点（门禁锁死：别处 new All 就是绕过判据）。
func scopeAll() LibraryScope { return LibraryScope{All: true} }

// scopeOnly 把已判权的范围收窄到一个库（feed 的 library_id 选择）。
func scopeOnly(libraryID string) LibraryScope {
	return LibraryScope{IDs: map[string]bool{libraryID: true}}
}

// resolveScope 是唯一判据：admin 全见，普通用户只认 user_libraries 的授权行。
func (s *Server) resolveScope(u *domain.User) (LibraryScope, error) {
	if u == nil {
		return LibraryScope{}, nil
	}
	if u.Role == domain.RoleAdmin {
		return scopeAll(), nil
	}
	ids, err := s.DB.UserLibraryIDs(u.ID)
	if err != nil {
		return LibraryScope{}, err
	}
	if len(ids) == 0 {
		return LibraryScope{}, nil // 0 行 = 0 个库，绝不回落成全部
	}
	return LibraryScope{IDs: ids}, nil
}

// WithLibraryScope 挂在 RequireAuth 内层；把范围放进 ctx 供 handler 取用。
func (s *Server) WithLibraryScope(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, err := s.resolveScope(UserFrom(r.Context()))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h(w, r.WithContext(withScope(r.Context(), sc)))
	}
}

// ScopeFrom 返回当前请求的库范围；没有 scope 时是零值（fail-closed）。
func ScopeFrom(ctx context.Context) LibraryScope {
	v, _ := ctx.Value(ctxScope).(LibraryScope)
	return v
}

func withScope(ctx context.Context, sc LibraryScope) context.Context {
	return context.WithValue(ctx, ctxScope, sc)
}

// canAccessMedia 把"不存在"与"无权"合并成同一个 404（B.5：不用 403 泄露存在性）。
func (s *Server) canAccessMedia(ctx context.Context, mediaID string) (*domain.Media, error) {
	return s.DB.GetMediaIn(ScopeFrom(ctx), mediaID)
}

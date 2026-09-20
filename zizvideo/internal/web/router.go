// Package web wires the router, the embedded assets and the API handlers.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/zizdog/govideo/internal/api"
)

// assetsFS holds the no-build frontend: plain ESM, hand-written CSS.
//
//go:embed all:assets
var assetsFS embed.FS

// Router builds the full handler tree: API first, static assets last.
func Router(s *api.Server) http.Handler {
	mux := http.NewServeMux()

	// Liveness and readiness are intentionally unauthenticated.
	mux.HandleFunc("GET /healthz", s.HandleHealthz)
	mux.HandleFunc("GET /readyz", s.HandleReadyz)

	mux.HandleFunc("GET /api/v1/setup/status", s.HandleSetupStatus)
	mux.HandleFunc("POST /api/v1/setup", s.HandleSetup)
	mux.HandleFunc("POST /api/v1/auth/login", s.HandleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.RequireAuth(s.HandleLogout))
	mux.HandleFunc("GET /api/v1/auth/me", s.RequireAuth(s.HandleMe))

	mux.HandleFunc("GET /api/v1/users", s.RequireAdmin(s.HandleListUsers))
	mux.HandleFunc("POST /api/v1/users", s.RequireAdmin(s.HandleCreateUser))
	mux.HandleFunc("PATCH /api/v1/users/{id}", s.RequireAdmin(s.HandlePatchUser))

	mux.HandleFunc("GET /api/v1/libraries", s.RequireAdmin(s.HandleListLibraries))
	mux.HandleFunc("POST /api/v1/libraries", s.RequireAdmin(s.HandleCreateLibrary))
	mux.HandleFunc("GET /api/v1/libraries/{id}", s.RequireAdmin(s.HandleGetLibrary))
	mux.HandleFunc("PATCH /api/v1/libraries/{id}", s.RequireAdmin(s.HandlePatchLibrary))
	mux.HandleFunc("DELETE /api/v1/libraries/{id}", s.RequireAdmin(s.HandleDeleteLibrary))
	mux.HandleFunc("POST /api/v1/libraries/{id}/scan", s.RequireAdmin(s.HandleStartScan))
	mux.HandleFunc("GET /api/v1/scan-tasks/{id}", s.RequireAdmin(s.HandleGetScanTask))

	mux.HandleFunc("GET /api/v1/media", s.RequireAuth(s.HandleListMedia))
	mux.HandleFunc("GET /api/v1/media/{id}", s.RequireAuth(s.HandleGetMedia))
	mux.HandleFunc("GET /api/v1/media/{id}/stream", s.RequireAuth(s.HandleStream))
	mux.HandleFunc("GET /api/v1/media/{id}/cover", s.RequireAuth(s.HandleCover))
	mux.HandleFunc("GET /api/v1/feed/next", s.RequireAuth(s.HandleFeedNext))

	mux.HandleFunc("PATCH /api/v1/me/progress/{mediaId}", s.RequireAuth(s.HandlePatchProgress))
	mux.HandleFunc("GET /api/v1/me/progress", s.RequireAuth(s.HandleListProgress))
	mux.HandleFunc("POST /api/v1/me/favorites/{mediaId}", s.RequireAuth(s.HandleAddFavorite))
	mux.HandleFunc("DELETE /api/v1/me/favorites/{mediaId}", s.RequireAuth(s.HandleRemoveFavorite))
	mux.HandleFunc("POST /api/v1/media/{id}/reactions", s.RequireAuth(s.HandleSetReaction))
	mux.HandleFunc("PATCH /api/v1/media/{id}/reactions", s.RequireAuth(s.HandleSetReaction))
	mux.HandleFunc("DELETE /api/v1/media/{id}/reactions", s.RequireAuth(s.HandleDeleteReaction))

	mux.HandleFunc("GET /api/v1/admin/system/info", s.RequireAdmin(s.HandleSystemInfo))
	mux.HandleFunc("GET /api/v1/admin/audit", s.RequireAdmin(s.HandleAuditList))

	// Unknown API paths must answer JSON, not the SPA shell.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"data":null,"meta":{},"error":{"code":"VALIDATION_NOT_FOUND","message":"接口不存在"}}`))
	})
	mux.Handle("/", staticHandler())

	return s.Middleware(mux)
}

// staticHandler serves the embedded frontend. index.html is never cached so a
// rebuilt binary is picked up on refresh.
func staticHandler() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

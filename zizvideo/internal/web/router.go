// Package web wires the router, the embedded assets and the API handlers.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/zizdog/zizvideo/internal/api"
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
	// P3：某用户的可见库（整体替换，回读一致）+ 默认可见库预览/补发。
	mux.HandleFunc("GET /api/v1/admin/users/{id}/libraries", s.RequireAdmin(s.HandleGetUserLibraries))
	mux.HandleFunc("PUT /api/v1/admin/users/{id}/libraries", s.RequireAdmin(s.HandlePutUserLibraries))
	mux.HandleFunc("GET /api/v1/admin/libraries/defaults", s.RequireAdmin(s.HandleGetDefaultLibraries))
	mux.HandleFunc("POST /api/v1/admin/libraries/defaults/backfill", s.RequireAdmin(s.HandleBackfillDefaultLibraries))

	mux.HandleFunc("GET /api/v1/libraries", s.RequireAdmin(s.HandleListLibraries))
	mux.HandleFunc("POST /api/v1/libraries", s.RequireAdmin(s.HandleCreateLibrary))
	mux.HandleFunc("GET /api/v1/libraries/{id}", s.RequireAdmin(s.HandleGetLibrary))
	mux.HandleFunc("PATCH /api/v1/libraries/{id}", s.RequireAdmin(s.HandlePatchLibrary))
	mux.HandleFunc("DELETE /api/v1/libraries/{id}", s.RequireAdmin(s.HandleDeleteLibrary))
	mux.HandleFunc("POST /api/v1/libraries/{id}/scan", s.RequireAdmin(s.HandleStartScan))
	mux.HandleFunc("GET /api/v1/scan-tasks/{id}", s.RequireAdmin(s.HandleGetScanTask))

	// 用户端读内容一律走 s.WithLibraryScope（唯一判据）；列表类静默过滤，单条越权 404。
	mux.HandleFunc("GET /api/v1/media", s.RequireAuth(s.WithLibraryScope(s.HandleListMedia)))
	mux.HandleFunc("GET /api/v1/media/{id}", s.RequireAuth(s.WithLibraryScope(s.HandleGetMedia)))
	mux.HandleFunc("GET /api/v1/media/{id}/stream", s.RequireAuth(s.WithLibraryScope(s.HandleStream)))
	mux.HandleFunc("GET /api/v1/media/{id}/cover", s.RequireAuth(s.WithLibraryScope(s.HandleCover)))
	mux.HandleFunc("GET /api/v1/feed/next", s.RequireAuth(s.WithLibraryScope(s.HandleFeedNext)))
	mux.HandleFunc("GET /api/v1/feed/settings", s.RequireAuth(s.HandleGetFeedSettings))
	mux.HandleFunc("PATCH /api/v1/feed/settings", s.RequireAuth(s.HandlePatchFeedSettings))

	mux.HandleFunc("PATCH /api/v1/me/progress/{mediaId}", s.RequireAuth(s.WithLibraryScope(s.HandlePatchProgress)))
	mux.HandleFunc("GET /api/v1/me/progress", s.RequireAuth(s.WithLibraryScope(s.HandleListProgress)))
	mux.HandleFunc("DELETE /api/v1/me/progress", s.RequireAuth(s.HandleClearProgress))
	mux.HandleFunc("GET /api/v1/me/favorites", s.RequireAuth(s.WithLibraryScope(s.HandleListFavorites)))
	mux.HandleFunc("DELETE /api/v1/me/favorites", s.RequireAuth(s.HandleClearFavorites))
	mux.HandleFunc("GET /api/v1/me/likes", s.RequireAuth(s.WithLibraryScope(s.HandleListLikes)))
	mux.HandleFunc("DELETE /api/v1/me/likes", s.RequireAuth(s.HandleClearLikes))
	mux.HandleFunc("GET /api/v1/me/libraries", s.RequireAuth(s.WithLibraryScope(s.HandleMyLibraries)))
	mux.HandleFunc("POST /api/v1/me/favorites/{mediaId}", s.RequireAuth(s.WithLibraryScope(s.HandleAddFavorite)))
	mux.HandleFunc("DELETE /api/v1/me/favorites/{mediaId}", s.RequireAuth(s.WithLibraryScope(s.HandleRemoveFavorite)))
	mux.HandleFunc("POST /api/v1/media/{id}/reactions", s.RequireAuth(s.WithLibraryScope(s.HandleSetReaction)))
	mux.HandleFunc("PATCH /api/v1/media/{id}/reactions", s.RequireAuth(s.WithLibraryScope(s.HandleSetReaction)))
	mux.HandleFunc("DELETE /api/v1/media/{id}/reactions", s.RequireAuth(s.WithLibraryScope(s.HandleDeleteReaction)))

	// 剧场（短剧）：读给所有登录用户，写只有管理员；剧集只引用已有 media。
	mux.HandleFunc("GET /api/v1/series", s.RequireAuth(s.WithLibraryScope(s.HandleListSeries)))
	mux.HandleFunc("GET /api/v1/series/{id}", s.RequireAuth(s.WithLibraryScope(s.HandleGetSeries)))
	mux.HandleFunc("POST /api/v1/admin/series", s.RequireAdmin(s.HandleCreateSeries))
	mux.HandleFunc("PATCH /api/v1/admin/series/{id}", s.RequireAdmin(s.HandlePatchSeries))
	mux.HandleFunc("DELETE /api/v1/admin/series/{id}", s.RequireAdmin(s.HandleDeleteSeries))
	mux.HandleFunc("POST /api/v1/admin/series/{id}/media", s.RequireAdmin(s.HandleAddSeriesMedia))
	mux.HandleFunc("DELETE /api/v1/admin/series/{id}/media/{mediaId}", s.RequireAdmin(s.HandleRemoveSeriesMedia))
	mux.HandleFunc("PUT /api/v1/admin/series/{id}/order", s.RequireAdmin(s.HandleReorderSeries))
	mux.HandleFunc("POST /api/v1/admin/series/{id}/detect", s.RequireAdmin(s.HandleDetectSeries))
	// P2：批量一键识别（confirm 两段式）+ 跨库任务进度（job_tasks，迁移 0008）。
	mux.HandleFunc("POST /api/v1/admin/series/detect-all", s.RequireAdmin(s.HandleDetectAll))
	mux.HandleFunc("GET /api/v1/admin/tasks/{id}", s.RequireAdmin(s.HandleGetJobTask))
	// P4：按目录建剧场。A=剧场内导入一个目录；B=按一级子目录批量建剧场（走任务中心）。
	mux.HandleFunc("POST /api/v1/admin/series/{id}/import-dir/preview", s.RequireAdmin(s.HandlePreviewSeriesDirImport))
	mux.HandleFunc("POST /api/v1/admin/series/{id}/import-dir", s.RequireAdmin(s.HandleSeriesDirImport))
	mux.HandleFunc("POST /api/v1/admin/series/import-dirs/preview", s.RequireAdmin(s.HandlePreviewSeriesDirs))
	mux.HandleFunc("POST /api/v1/admin/series/import-dirs", s.RequireAdmin(s.HandleSeriesDirsImport))
	// P5：上传（start / PUT 流式 / finish）。PUT 单独放开读超时（见 handlers_uploads.go）。
	mux.HandleFunc("POST /api/v1/admin/uploads/start", s.RequireAdmin(s.HandleUploadStart))
	mux.HandleFunc("PUT /api/v1/admin/uploads/{id}/{index}", s.RequireAdmin(s.HandleUploadPut))
	mux.HandleFunc("POST /api/v1/admin/uploads/{id}/finish", s.RequireAdmin(s.HandleUploadFinish))

	mux.HandleFunc("GET /api/v1/admin/system/info", s.RequireAdmin(s.HandleSystemInfo))
	mux.HandleFunc("GET /api/v1/admin/audit", s.RequireAdmin(s.HandleAuditList))

	// 条目 8：注册开关（默认关）+ 唯一公开自助注册入口。
	mux.HandleFunc("GET /api/v1/admin/settings", s.RequireAdmin(s.HandleGetAdminSettings))
	mux.HandleFunc("PATCH /api/v1/admin/settings", s.RequireAdmin(s.HandlePatchAdminSettings))
	mux.HandleFunc("POST /api/v1/auth/register", s.HandleRegister)

	// 自动扫描：设置读写、状态回读、立即触发一轮（扫描仍走任务中心 202+task_id）。
	mux.HandleFunc("GET /api/v1/admin/autoscan", s.RequireAdmin(s.HandleGetAutoScan))
	mux.HandleFunc("PATCH /api/v1/admin/autoscan", s.RequireAdmin(s.HandlePatchAutoScan))
	mux.HandleFunc("POST /api/v1/admin/autoscan/run", s.RequireAdmin(s.HandleRunAutoScan))

	// 条目 11：媒体去重（判据 size+duration，默认只删记录）。
	mux.HandleFunc("GET /api/v1/admin/duplicates", s.RequireAdmin(s.HandleListDuplicates))
	mux.HandleFunc("POST /api/v1/admin/duplicates/delete-records", s.RequireAdmin(s.HandleDeleteDuplicateRecords))
	mux.HandleFunc("POST /api/v1/admin/duplicates/delete-files", s.RequireAdmin(s.HandleDeleteDuplicateFiles))

	// Allow roots + the directory picker. The literal "roots" path is registered
	// before the wildcard, so /media/roots never falls through to /media/{id}.
	mux.HandleFunc("GET /api/v1/media/roots", s.RequireAdmin(s.HandleListMediaRoots))
	mux.HandleFunc("POST /api/v1/media/roots", s.RequireAdmin(s.HandleAddMediaRoot))
	mux.HandleFunc("DELETE /api/v1/media/roots", s.RequireAdmin(s.HandleRemoveMediaRoot))
	mux.HandleFunc("GET /api/v1/fs/browse", s.RequireAdmin(s.HandleBrowseFS))

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

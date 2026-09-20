package api

import (
	"net/http"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
	"github.com/zizdog/zizvideo/internal/storage"
)

// HandleListLibraries returns every registered library.
func (s *Server) HandleListLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.DB.ListLibraries()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"list": libs}, nil)
}

type libraryReq struct {
	Name               string   `json:"name"`
	RootPath           string   `json:"root_path"`
	Recursive          *bool    `json:"recursive"`
	Enabled            *bool    `json:"enabled"`
	IgnoreRules        []string `json:"ignore_rules"`
	DefaultForNewUsers *bool    `json:"default_for_new_users"`
}

// HandleCreateLibrary registers a media root after full path validation.
func (s *Server) HandleCreateLibrary(w http.ResponseWriter, r *http.Request) {
	var req libraryReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 64 {
		s.fail(w, r, domain.New("VALIDATION_NAME", "库名称需 1-64 个字符", 400))
		return
	}
	if _, err := media.ValidateLibraryPath(s.Roots.List(), req.RootPath); err != nil {
		s.audit(r, "library.create", "path:"+req.RootPath, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	lib := &domain.Library{
		ID: domain.NewID("lib"), Name: req.Name, RootPath: req.RootPath,
		Recursive: true, Enabled: true, IgnoreRules: req.IgnoreRules,
	}
	if req.Recursive != nil {
		lib.Recursive = *req.Recursive
	}
	if req.Enabled != nil {
		lib.Enabled = *req.Enabled
	}
	if lib.IgnoreRules == nil {
		lib.IgnoreRules = []string{}
	}
	if err := s.DB.CreateLibrary(lib); err != nil {
		s.audit(r, "library.create", "name:"+req.Name, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "library.create", "library:"+lib.ID, true, "")
	s.AutoScanChanged()
	respond(w, http.StatusCreated, lib, nil)
}

// HandleGetLibrary returns one library plus its counters.
func (s *Server) HandleGetLibrary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	lib, err := s.DB.GetLibrary(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	count, err := s.DB.CountMedia(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	failed, err := s.DB.CountFailedMedia(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	last, err := s.DB.LatestScanTask(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{
		"library": lib,
		"stats": map[string]any{
			"media_count": count, "failed_count": failed, "last_scan": last,
		},
	}, nil)
}

// HandlePatchLibrary updates a library, re-validating any new root path.
func (s *Server) HandlePatchLibrary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req libraryReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	patch := storage.LibraryPatch{}
	if req.Name != "" {
		name := strings.TrimSpace(req.Name)
		patch.Name = &name
	}
	if req.RootPath != "" {
		if _, err := media.ValidateLibraryPath(s.Roots.List(), req.RootPath); err != nil {
			s.fail(w, r, err)
			return
		}
		root := req.RootPath
		patch.RootPath = &root
	}
	if req.Recursive != nil {
		patch.Recursive = req.Recursive
	}
	if req.Enabled != nil {
		patch.Enabled = req.Enabled
	}
	if req.IgnoreRules != nil {
		rules := req.IgnoreRules
		patch.IgnoreRules = &rules
	}
	if req.DefaultForNewUsers != nil {
		patch.DefaultForNewUsers = req.DefaultForNewUsers
	}
	lib, err := s.DB.UpdateLibrary(id, patch)
	if err != nil {
		s.audit(r, "library.update", "library:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "library.update", "library:"+id, true, "")
	s.AutoScanChanged()
	respond(w, http.StatusOK, lib, nil)
}

// HandleDeleteLibrary soft-deletes a library and its media rows.
func (s *Server) HandleDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.DB.DeleteLibrary(id); err != nil {
		s.audit(r, "library.delete", "library:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "library.delete", "library:"+id, true, "")
	s.AutoScanChanged()
	respond(w, http.StatusOK, map[string]any{"ok": true}, nil)
}

type scanReq struct {
	Kind string `json:"kind"`
}

// HandleStartScan queues a scan and answers immediately with the task id.
func (s *Server) HandleStartScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req scanReq
	if r.ContentLength > 0 {
		if err := s.decodeJSON(w, r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	t, err := s.Tasks.StartScan(id, req.Kind)
	if err != nil {
		s.audit(r, "scan.start", "library:"+id, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "scan.start", "library:"+id, true, "task:"+t.ID)
	respond(w, http.StatusAccepted, map[string]any{"task_id": t.ID}, nil)
}

// HandleGetScanTask returns live progress for a scan task.
func (s *Server) HandleGetScanTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.Tasks.Get(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, t, nil)
}

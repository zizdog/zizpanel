package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/fsbrowse"
	"github.com/zizdog/zizvideo/internal/media"
)

// rootState is one allow root plus the honest probe of its current state.
type rootState struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	IsDir     bool   `json:"is_dir"`
	Readable  bool   `json:"readable"`
	InUse     bool   `json:"in_use"`
	Libraries int    `json:"libraries"`
}

func (s *Server) rootsBody() map[string]any {
	raw := s.Roots.List()
	libs, err := s.DB.ListLibraries()
	if err != nil {
		libs = nil
	}
	out := make([]rootState, 0, len(raw))
	for _, r := range raw {
		st := rootState{Path: r}
		if info, serr := os.Stat(r); serr == nil {
			st.Exists = true
			st.IsDir = info.IsDir()
		}
		st.Readable = st.IsDir && readableDir(r)
		names := librariesUnder(libs, r)
		st.Libraries = len(names)
		st.InUse = len(names) > 0
		out = append(out, st)
	}
	return map[string]any{
		"roots":        out,
		"config_path":  s.Roots.Path(),
		"env_override": s.Roots.EnvOverridden(),
		"starts":       s.browseStarts(),
	}
}

// librariesUnder lists library names whose stored root sits inside an allow
// root, comparing both the literal and the symlink-resolved spellings.
func librariesUnder(libs []domain.Library, root string) []string {
	names := []string{}
	alias := ""
	if a, err := filepath.EvalSymlinks(root); err == nil {
		alias = a
	}
	for _, l := range libs {
		real := l.RootPath
		if r, err := filepath.EvalSymlinks(l.RootPath); err == nil {
			real = r
		}
		if media.Within(l.RootPath, root) || media.Within(real, root) {
			names = append(names, l.Name)
			continue
		}
		if alias != "" && (media.Within(l.RootPath, alias) || media.Within(real, alias)) {
			names = append(names, l.Name)
		}
	}
	return names
}

func readableDir(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err == nil || errors.Is(err, io.EOF)
}

// HandleListMediaRoots returns the allow roots and their real state.
func (s *Server) HandleListMediaRoots(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, s.rootsBody(), nil)
}

type mediaRootReq struct {
	Path string `json:"path"`
}

// HandleAddMediaRoot validates one directory and persists it into config.json.
func (s *Server) HandleAddMediaRoot(w http.ResponseWriter, r *http.Request) {
	var req mediaRootReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	clean, verr := config.ValidateAllowRoot(req.Path)
	if verr != nil {
		s.fail(w, r, domain.New("MEDIA_ROOT_INVALID", verr.Error(), 400))
		return
	}
	if !s.Roots.FileBacked() {
		s.fail(w, r, domain.New("MEDIA_CONFIG_UNAVAILABLE",
			"没有 config.json，复制 config.example.json 或用 --config 指定", 409))
		return
	}
	if s.Roots.EnvOverridden() {
		s.fail(w, r, domain.New("MEDIA_ROOT_ENV_OVERRIDE",
			"ZV_MEDIA_ALLOW_ROOTS 已覆盖，配置文件不会生效", 409))
		return
	}
	current := s.Roots.List()
	for _, existing := range current {
		if existing == clean {
			s.fail(w, r, domain.ErrPathRootExists)
			return
		}
	}
	if covering := media.CoveringRoot(current, clean); covering != "" {
		s.fail(w, r, domain.New("MEDIA_ROOT_COVERED", "该目录已在允许根内: "+covering, 409))
		return
	}
	next, err := s.Roots.Add(clean)
	if err != nil {
		s.audit(r, "media_root.add", "path:"+clean, false, errCode(err))
		s.fail(w, r, domain.New("MEDIA_ROOT_WRITE_FAILED", "写入配置失败: "+err.Error(), 500))
		return
	}
	s.audit(r, "media_root.add", "path:"+clean, true, "")
	body := s.rootsBody()
	body["added"] = clean
	body["roots_list"] = next
	respond(w, http.StatusCreated, body, nil)
}

// HandleRemoveMediaRoot removes one allow root, refusing while a library uses it.
func (s *Server) HandleRemoveMediaRoot(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	if raw == "" {
		s.fail(w, r, domain.New("MEDIA_ROOT_INVALID", "缺少 path 参数", 400))
		return
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		s.fail(w, r, domain.ErrPathNotAbsolute)
		return
	}
	// Compare on the resolved spelling so /tmp/x and /private/tmp/x agree.
	target := config.ResolvePath(clean)
	if !s.Roots.FileBacked() {
		s.fail(w, r, domain.New("MEDIA_CONFIG_UNAVAILABLE",
			"没有 config.json，复制 config.example.json 或用 --config 指定", 409))
		return
	}
	if s.Roots.EnvOverridden() {
		s.fail(w, r, domain.New("MEDIA_ROOT_ENV_OVERRIDE",
			"ZV_MEDIA_ALLOW_ROOTS 已覆盖，配置文件不会生效", 409))
		return
	}
	current := s.Roots.List()
	found := false
	for _, existing := range current {
		if existing == target || existing == clean {
			clean = existing
			found = true
			break
		}
	}
	if !found {
		s.fail(w, r, domain.New("MEDIA_ROOT_NOT_FOUND", "该路径不在允许根里", 404))
		return
	}
	libs, err := s.DB.ListLibraries()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if names := librariesUnder(libs, clean); len(names) > 0 {
		s.audit(r, "media_root.remove", "path:"+clean, false, "in_use")
		s.fail(w, r, domain.New("MEDIA_ROOT_IN_USE",
			"以下媒体库正在使用，请先处理: "+strings.Join(names, "、"), 409))
		return
	}
	next, err := s.Roots.Remove(clean)
	if err != nil {
		s.audit(r, "media_root.remove", "path:"+clean, false, errCode(err))
		s.fail(w, r, domain.New("MEDIA_ROOT_WRITE_FAILED", "写入配置失败: "+err.Error(), 500))
		return
	}
	s.audit(r, "media_root.remove", "path:"+clean, true, "")
	body := s.rootsBody()
	body["removed"] = clean
	body["roots_list"] = next
	respond(w, http.StatusOK, body, nil)
}

// HandleBrowseFS answers the directory picker with sub-directory names only.
func (s *Server) HandleBrowseFS(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	limit := queryInt(r, "limit", 200, 1, 1000)
	offset := queryInt(r, "offset", 0, 0, 1<<30)
	res, err := fsbrowse.Browse(raw, s.browseStarts(), offset, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, res, nil)
}

// browseStarts are convenient entry points: /Volumes, $HOME and the allow roots.
func (s *Server) browseStarts() []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add("/Volumes")
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	for _, root := range s.Roots.List() {
		add(root)
	}
	return out
}

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
// status 把"为什么不可用"说成一句人话，UI 直接显示（旧根脱机也要能看见/能删）。
type rootState struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	IsDir     bool   `json:"is_dir"`
	Readable  bool   `json:"readable"`
	Status    string `json:"status"`
	Note      string `json:"note,omitempty"`
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
		st := rootState{Path: r, Status: "ok"}
		if info, serr := os.Stat(r); serr == nil {
			st.Exists = true
			st.IsDir = info.IsDir()
		}
		st.Readable = st.IsDir && readableDir(r)
		switch {
		case !st.Exists:
			st.Status, st.Note = "unavailable", "不可用（路径不存在）"
		case !st.IsDir:
			st.Status, st.Note = "unavailable", "不可用（不是目录）"
		case !st.Readable:
			st.Status, st.Note = "unavailable", "不可用（不可读）"
		}
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
		// 说清是"这条路径不可用"，别让用户以为是 config.json 写不进去。
		s.fail(w, r, domain.New("MEDIA_ROOT_INVALID", "不能加为允许根："+verr.Error(), 400))
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
	s.AutoScanChanged()
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
	// 删空会让媒体库整体不可用：在接口层拒绝，错误文案不许说成"写入失败"。
	if len(current) <= 1 {
		s.fail(w, r, domain.New("MEDIA_ROOT_LAST",
			"至少要保留一个允许根，先添加一个可用目录再删", 409))
		return
	}
	next, err := s.Roots.Remove(clean)
	if err != nil {
		s.audit(r, "media_root.remove", "path:"+clean, false, errCode(err))
		s.fail(w, r, domain.New("MEDIA_ROOT_WRITE_FAILED", "写入配置失败: "+err.Error(), 500))
		return
	}
	s.audit(r, "media_root.remove", "path:"+clean, true, "")
	s.AutoScanChanged()
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

// browseStarts are convenient entry points: $HOME first, then the allow roots.
// 不写死外置盘挂载点：盘不在场时它只是个空目录，入口交给用户自己选。
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
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	for _, root := range s.Roots.List() {
		add(root)
	}
	return out
}

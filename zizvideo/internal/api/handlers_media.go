package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
	"github.com/zizdog/zizvideo/internal/storage"
)

// mediaItem is the single shape returned by /media, /media/{id} and /feed/next.
type mediaItem struct {
	ID            string        `json:"id"`
	LibraryID     string        `json:"library_id"`
	Title         string        `json:"title"`
	Path          string        `json:"path,omitempty"`
	Size          int64         `json:"size"`
	DurationMS    int64         `json:"duration_ms"`
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	Codecs        domain.Codecs `json:"codecs"`
	Container     string        `json:"container"`
	FPS           float64       `json:"fps"`
	Bitrate       int64         `json:"bitrate"`
	Status        string        `json:"status"`
	ErrorClass    string        `json:"error_class,omitempty"`
	CoverURL      string        `json:"cover_url"`
	StreamURL     string        `json:"stream_url"`
	Compatibility compatibility `json:"compatibility"`
	Progress      progressBrief `json:"progress"`
	Favorite      bool          `json:"favorite"`
	Reaction      any           `json:"reaction"`
	CreatedAt     string        `json:"created_at"`
}

type progressBrief struct {
	PositionMS int64 `json:"position_ms"`
	Completed  bool  `json:"completed"`
}

// buildItems decorates media rows for one viewer.
func (s *Server) buildItems(rows []domain.Media, r *http.Request, withPath bool) []mediaItem {
	u := UserFrom(r.Context())
	ua := parseUA(r.Header.Get("User-Agent"))
	progs := map[string]domain.Progress{}
	favs := map[string]bool{}
	reactions := map[string]string{}
	if u != nil {
		if m, err := s.DB.ProgressMap(u.ID); err == nil {
			progs = m
		}
		if m, err := s.DB.Favorites(u.ID); err == nil {
			favs = m
		}
		if m, err := s.DB.Reactions(u.ID); err == nil {
			reactions = m
		}
	}
	out := make([]mediaItem, 0, len(rows))
	for _, m := range rows {
		item := mediaItem{
			ID: m.ID, LibraryID: m.LibraryID, Title: m.Title,
			Size: m.Size, DurationMS: m.DurationMS, Width: m.Width, Height: m.Height,
			Codecs: m.Codecs, Container: m.Container, FPS: m.FPS, Bitrate: m.Bitrate,
			Status: m.Status, ErrorClass: m.ErrorClass,
			CoverURL:  "/api/v1/media/" + m.ID + "/cover",
			StreamURL: "/api/v1/media/" + m.ID + "/stream",
			Favorite:  favs[m.ID], CreatedAt: m.CreatedAt,
		}
		if withPath {
			item.Path = m.Path
		}
		item.Compatibility = evaluateCompat(m.Codecs, ua)
		if reason := audioProblem(m.Codecs); reason != "" && item.Compatibility.Direct {
			item.Compatibility.Direct = false
			item.Compatibility.Reason = reason
		}
		if p, ok := progs[m.ID]; ok {
			item.Progress = progressBrief{PositionMS: p.PositionMS, Completed: p.Completed}
		}
		if k, ok := reactions[m.ID]; ok {
			item.Reaction = k
		}
		out = append(out, item)
	}
	return out
}

// HandleListMedia returns a filtered page of media.
func (s *Server) HandleListMedia(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1, 1, 1_000_000)
	per := queryInt(r, "per_page", 20, 1, 100)
	sort := r.URL.Query().Get("sort")
	switch sort {
	case "", "id", "title", "created_at", "size", "duration_ms":
	default:
		sort = "id"
	}
	desc := r.URL.Query().Get("order") != "asc"
	rows, total, err := s.DB.ListMedia(ScopeFrom(r.Context()), struct2Filter(r, page, per, sort, desc))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := s.buildItems(rows, r, false)
	respond(w, http.StatusOK, map[string]any{"list": items}, map[string]any{
		"page": page, "per_page": per, "total": total,
		"has_more": page*per < total,
	})
}

func struct2Filter(r *http.Request, page, per int, sort string, desc bool) storage.MediaFilter {
	return storage.MediaFilter{
		Page:      page,
		PerPage:   per,
		Sort:      sort,
		Desc:      desc,
		LibraryID: r.URL.Query().Get("library_id"),
		Query:     strings.TrimSpace(r.URL.Query().Get("q")),
		Status:    r.URL.Query().Get("status"),
	}
}

// HandleGetMedia returns one item with its path (admins only) and progress.
func (s *Server) HandleGetMedia(w http.ResponseWriter, r *http.Request) {
	m, err := s.canAccessMedia(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := UserFrom(r.Context())
	items := s.buildItems([]domain.Media{*m}, r, u != nil && u.Role == domain.RoleAdmin)
	respond(w, http.StatusOK, items[0], nil)
}

// HandleFeedNext lives in handlers_feed.go (seeded random cycle).

// HandleStream serves the raw file with full Range semantics.
func (s *Server) HandleStream(w http.ResponseWriter, r *http.Request) {
	m, err := s.canAccessMedia(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	lib, err := s.DB.GetLibrary(m.LibraryID)
	if err != nil {
		s.fail(w, r, domain.ErrNotFound)
		return
	}
	if !lib.Enabled {
		s.fail(w, r, domain.New("FORBIDDEN_LIBRARY_DISABLED", "媒体库已禁用", 403))
		return
	}
	// Whitelist check happens before the file is opened.
	if _, err := media.ValidateMediaFile(s.Roots.List(), lib.RootPath, m.Path); err != nil {
		s.audit(r, "media.stream", "media:"+m.ID, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	f, err := os.Open(m.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.fail(w, r, domain.New("MEDIA_FILE_GONE", "文件已不在磁盘上", 404))
			return
		}
		s.fail(w, r, domain.New("MEDIA_FILE_UNREADABLE", "文件不可读", 403))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		s.fail(w, r, domain.New("MEDIA_FILE_UNREADABLE", "文件不可读", 403))
		return
	}
	if ct := contentTypeFor(m.Path); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	// Multiple ranges would make net/http answer multipart/byteranges; the spec
	// calls for a plain 200 full-body fallback instead.
	if isMultiRange(r.Header.Get("Range")) {
		r.Header.Del("Range")
	}
	http.ServeContent(w, r, filepath.Base(m.Path), st.ModTime(), &liveFile{
		File: f, path: m.Path, lastCheck: time.Now(),
	})
}

// isMultiRange reports a Range header asking for more than one span.
func isMultiRange(header string) bool {
	header = strings.TrimSpace(strings.ToLower(header))
	if !strings.HasPrefix(header, "bytes=") {
		return false
	}
	return strings.Contains(strings.TrimPrefix(header, "bytes="), ",")
}

func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".ts", ".mts":
		return "video/mp2t"
	case ".avi":
		return "video/x-msvideo"
	case ".flv":
		return "video/x-flv"
	case ".hevc":
		return "video/hevc"
	}
	return ""
}

// liveFile ends an in-flight stream when the underlying file disappears.
//
// The stat is throttled: one syscall per second, not per read.
type liveFile struct {
	*os.File
	path      string
	lastCheck time.Time
	mu        sync.Mutex
}

// Read implements io.Reader with the existence check.
func (f *liveFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.mu.Lock()
	check := time.Since(f.lastCheck) > time.Second
	if check {
		f.lastCheck = time.Now()
	}
	f.mu.Unlock()
	if check {
		if _, serr := os.Stat(f.path); serr != nil {
			return n, os.ErrNotExist
		}
	}
	return n, err
}

// HandleCover serves the extracted JPEG, or a placeholder when there is none.
func (s *Server) HandleCover(w http.ResponseWriter, r *http.Request) {
	// 无权必须 404，绝不回落到占位图（占位 200 会泄露"这个 id 存在"）。
	m, err := s.canAccessMedia(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := filepath.Join(s.Cfg.CoversDir(), m.ID+".jpg")
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		if st, err := f.Stat(); err == nil && !st.IsDir() {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "private, max-age=300")
			http.ServeContent(w, r, m.ID+".jpg", st.ModTime(), f)
			return
		}
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(placeholderCover))
}

const placeholderCover = `<svg xmlns="http://www.w3.org/2000/svg" width="360" height="640">` +
	`<rect width="100%" height="100%" fill="#111"/><text x="50%" y="50%" fill="#666" ` +
	`font-family="sans-serif" font-size="20" text-anchor="middle">无封面</text></svg>`

// HandleDeleteMedia 是管理员在**前台播放页**的删除入口：默认只删面板记录
// （磁盘文件不动，重扫会回来）；delete_file=true 时先验路径再删文件，**文件删不掉就不删记录**
// 并如实报错（否则会出现"记录没了、文件还在"的谎报）。
func (s *Server) HandleDeleteMedia(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.fail(w, r, domain.New("VALIDATION_ID", "缺少媒体 id", 400))
		return
	}
	var req struct {
		DeleteFile bool   `json:"delete_file"`
		Confirm    string `json:"confirm"`
	}
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	found, missing, err := s.DB.MediaByIDs([]string{id})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(missing) > 0 || len(found) == 0 {
		s.fail(w, r, domain.New("VALIDATION_ID", "这条记录已不存在，请刷新后重试", 404))
		return
	}
	m := found[0]

	fileDeleted := false
	if req.DeleteFile {
		if strings.TrimSpace(req.Confirm) != deleteFilesConfirm {
			s.audit(r, "media.delete_file", m.ID, false, "missing_confirm")
			s.fail(w, r, domain.New("MEDIA_CONFIRM_REQUIRED",
				"危险操作：请手动输入「删除文件」确认", 400))
			return
		}
		if s.Tasks.Busy(m.LibraryID) {
			s.fail(w, r, domain.New("MEDIA_LIBRARY_BUSY", "该媒体库正在扫描，请稍后再试", 409))
			return
		}
		lib, lerr := s.DB.GetLibrary(m.LibraryID)
		if lerr != nil {
			s.fail(w, r, lerr)
			return
		}
		canonical, verr := media.ValidateMediaFile(s.Roots.List(), lib.RootPath, m.Path)
		if verr != nil {
			s.audit(r, "media.delete_file", m.ID, false, pathReason(verr))
			s.fail(w, r, domain.New("MEDIA_PATH_REFUSED", "路径校验未通过，未删文件："+pathReason(verr), 400))
			return
		}
		if rerr := os.Remove(canonical); rerr != nil {
			s.audit(r, "media.delete_file", m.ID, false, rerr.Error())
			s.fail(w, r, domain.New("MEDIA_FILE_DELETE_FAILED", "删除文件失败："+rerr.Error(), 500))
			return
		}
		// 删完必须回读核对：文件还在就是没删掉，不许当成成功。
		if _, serr := os.Stat(canonical); serr == nil {
			s.fail(w, r, domain.New("MEDIA_FILE_DELETE_FAILED", "删除后文件仍在，未确认成功", 500))
			return
		} else if !os.IsNotExist(serr) {
			s.fail(w, r, domain.New("MEDIA_FILE_DELETE_FAILED", "删除后核对失败："+serr.Error(), 500))
			return
		}
		fileDeleted = true
	}

	n, err := s.DB.SoftDeleteMedia([]string{m.ID})
	if err != nil {
		s.audit(r, "media.delete", m.ID, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	action, note := "media.delete", "仅删除面板记录，磁盘文件未动；重新扫描会再登记"
	if fileDeleted {
		action, note = "media.delete_file", "记录与磁盘文件都已删除，不可恢复"
	}
	s.audit(r, action, m.ID, true, fmt.Sprintf("file=%v", fileDeleted))
	respond(w, http.StatusOK, map[string]any{
		"deleted": n, "file_deleted": fileDeleted, "note": note,
	}, nil)
}

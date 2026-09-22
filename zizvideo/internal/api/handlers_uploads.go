package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zizdog/zizvideo/internal/dirimport"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
)

// 上传：三步（start / PUT 流式 / finish），目标目录必须在允许根内、必须属于某个媒体库。
// 会话只存内存：进程重启后未完成的上传会 404，重新 start 即可（start 会清掉 .zvpart）。

const (
	partSuffix       = ".zvpart"
	uploadSessionTTL = 6 * time.Hour
	longUploadWindow = 2 * time.Hour
)

type uploadFile struct {
	Index    int
	Name     string
	Size     int64
	Final    string
	Received bool
}

type uploadSession struct {
	mu        sync.Mutex
	ID        string
	Dir       string
	LibraryID string
	SeriesID  string
	Overwrite bool
	Created   time.Time
	Files     []*uploadFile
}

type uploadStore struct {
	mu   sync.Mutex
	live map[string]*uploadSession
}

func newUploadStore() *uploadStore { return &uploadStore{live: map[string]*uploadSession{}} }

func (u *uploadStore) put(s *uploadSession) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.live[s.ID] = s
}

func (u *uploadStore) get(id string) (*uploadSession, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.live[id]
	return s, ok
}

func (u *uploadStore) drop(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.live, id)
}

// prune 丢掉超时会话（浏览器中途关掉就不再有人 finish）；磁盘上的 .zvpart 由下次 start 清。
func (u *uploadStore) prune(now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, s := range u.live {
		if now.Sub(s.Created) > uploadSessionTTL {
			delete(u.live, id)
		}
	}
}

// ============================================================================
//  start
// ============================================================================

type uploadStartFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type uploadStartReq struct {
	LibraryID     string            `json:"library_id"`
	SeriesID      string            `json:"series_id"`
	NewSeriesName string            `json:"new_series_name"`
	DirPath       string            `json:"dir_path"`
	Overwrite     bool              `json:"overwrite"`
	Files         []uploadStartFile `json:"files"`
}

type uploadFileJSON struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Path  string `json:"path"`
	URL   string `json:"url"`
}

type uploadStartJSON struct {
	UploadID  string           `json:"upload_id"`
	Dir       string           `json:"dir"`
	LibraryID string           `json:"library_id"`
	SeriesID  string           `json:"series_id,omitempty"`
	MaxFileMB int              `json:"max_file_mb"`
	FreeBytes int64            `json:"free_bytes"`
	Files     []uploadFileJSON `json:"files"`
}

// HandleUploadStart 决定落点、查空间、分配每个文件的目标名与上传地址。
func (s *Server) HandleUploadStart(w http.ResponseWriter, r *http.Request) {
	var req uploadStartReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(req.Files) == 0 || len(req.Files) > dirImportMaxFiles {
		s.fail(w, r, domain.New("UPLOAD_FILES_INVALID",
			fmt.Sprintf("一次需要 1-%d 个文件", dirImportMaxFiles), 400))
		return
	}
	lib, dir, seriesID, err := s.resolveUploadTarget(&req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	maxBytes := s.Cfg.UploadMaxBytes()
	var total int64
	files := make([]*uploadFile, 0, len(req.Files))
	seen := map[string]bool{}
	for i, item := range req.Files {
		name, nerr := safeFileName(item.Name)
		if nerr != nil {
			s.fail(w, r, nerr)
			return
		}
		// 扩展名白名单与扫描器一致：别让 .txt 也能登记成"视频"进库。
		if ext := strings.ToLower(filepath.Ext(name)); !s.Cfg.ExtAllowed(ext) {
			s.fail(w, r, domain.New("UPLOAD_EXT_NOT_ALLOWED",
				fmt.Sprintf("%s 不是允许的视频扩展名（可改 config.json 的 media_extensions）", name), 400))
			return
		}
		if item.Size < 0 || item.Size > maxBytes {
			s.fail(w, r, domain.New("UPLOAD_FILE_TOO_LARGE",
				fmt.Sprintf("文件 %s 超过单文件上限 %d MB（可改 config.json 的 upload_max_file_mb）",
					name, s.Cfg.UploadMaxFileMB), 413))
			return
		}
		// 同一批里重名也要各留一份：uniqueName 只看磁盘，pending 的必须自己避让。
		for seen[name] {
			name = uniqueName(dir, name)
		}
		seen[name] = true
		final := filepath.Join(dir, name)
		if !req.Overwrite {
			if _, serr := os.Stat(final); serr == nil {
				final = filepath.Join(dir, uniqueName(dir, name))
			}
		}
		total += item.Size
		files = append(files, &uploadFile{Index: i, Name: filepath.Base(final),
			Size: item.Size, Final: final})
	}
	free, ferr := availableBytes(dir)
	if ferr != nil {
		s.fail(w, r, domain.New("UPLOAD_STATFS_FAILED", "无法读取磁盘剩余空间："+ferr.Error(), 500))
		return
	}
	if total > free {
		s.fail(w, r, domain.New("UPLOAD_NO_SPACE",
			fmt.Sprintf("磁盘剩余空间不足：需要 %s，可用 %s", humanBytes(total), humanBytes(free)), 409))
		return
	}
	s.Uploads.prune(time.Now())
	session := &uploadSession{ID: domain.NewID("upl"), Dir: dir, LibraryID: lib.ID,
		SeriesID: seriesID, Overwrite: req.Overwrite, Created: time.Now(), Files: files}
	s.Uploads.put(session)
	// start 顺手清掉本目录里中断上传留下的 .zvpart（它们只可能是我们的）。
	cleanParts(dir)

	out := uploadStartJSON{UploadID: session.ID, Dir: dir, LibraryID: lib.ID,
		SeriesID: seriesID, MaxFileMB: s.Cfg.UploadMaxFileMB, FreeBytes: free,
		Files: []uploadFileJSON{}}
	for _, f := range files {
		out.Files = append(out.Files, uploadFileJSON{Index: f.Index, Name: f.Name,
			Size: f.Size, Path: f.Final,
			URL: "/api/v1/admin/uploads/" + session.ID + "/" + strconv.Itoa(f.Index)})
	}
	s.audit(r, "upload.start", "dir:"+dir, true, fmt.Sprintf("files:%d", len(files)))
	respond(w, http.StatusCreated, out, nil)
}

// resolveUploadTarget 把三类目标解析成 (库, 落点目录, 剧场 id)。
func (s *Server) resolveUploadTarget(req *uploadStartReq) (*domain.Library, string, string, error) {
	seriesID := strings.TrimSpace(req.SeriesID)
	if seriesID != "" {
		series, err := s.DB.GetSeries(seriesID)
		if err != nil {
			return nil, "", "", err
		}
		if series.DirPath != "" {
			lib, dir, rerr := s.libraryDir(series.DirPath)
			if rerr != nil {
				return nil, "", "", rerr
			}
			return lib, dir, seriesID, nil
		}
		libID := strings.TrimSpace(req.LibraryID)
		if libID == "" {
			libID = series.LibraryID
		}
		if libID == "" {
			return nil, "", "", domain.New("UPLOAD_NEED_LIBRARY",
				"这个剧场还没有目录：请选择要落入的媒体库", 400)
		}
		lib, err := s.DB.GetLibrary(libID)
		if err != nil {
			return nil, "", "", err
		}
		dir, derr := s.ensureSeriesDir(lib, series.Title)
		if derr != nil {
			return nil, "", "", derr
		}
		if err := s.DB.SetSeriesDir(seriesID, dir); err != nil {
			return nil, "", "", err
		}
		return lib, dir, seriesID, nil
	}

	if name := strings.TrimSpace(req.NewSeriesName); name != "" {
		if _, err := safeDirName(name); err != nil {
			return nil, "", "", err
		}
		var lib *domain.Library
		var dir string
		if req.DirPath != "" {
			l, d, err := s.libraryDir(req.DirPath)
			if err != nil {
				return nil, "", "", err
			}
			lib, dir = l, d
		} else {
			l, err := s.uploadLibrary(req.LibraryID)
			if err != nil {
				return nil, "", "", err
			}
			lib = l
			dir, err = s.ensureSeriesDir(lib, name)
			if err != nil {
				return nil, "", "", err
			}
		}
		series, err := s.DB.SeriesByTitle(name)
		if err != nil {
			return nil, "", "", err
		}
		if series == nil {
			series, err = s.createSeriesNamed(name, lib.ID)
			if err != nil {
				return nil, "", "", err
			}
		}
		if series.DirPath == "" {
			if err := s.DB.SetSeriesDir(series.ID, dir); err != nil {
				return nil, "", "", err
			}
		}
		return lib, dir, series.ID, nil
	}

	if req.DirPath != "" {
		lib, dir, err := s.libraryDir(req.DirPath)
		if err != nil {
			return nil, "", "", err
		}
		return lib, dir, "", nil
	}
	if req.LibraryID != "" {
		lib, err := s.uploadLibrary(req.LibraryID)
		if err != nil {
			return nil, "", "", err
		}
		return lib, filepath.Clean(lib.RootPath), "", nil
	}
	return nil, "", "", domain.New("UPLOAD_TARGET_MISSING",
		"缺少上传目标：库 / 剧场 / 新建剧场至少给一个", 400)
}

// libraryDir 校验一个已存在的目录在允许根内，并找到它所属的媒体库。
// 返回调用方的拼写（不解析符号链接），与扫描器登记 media.path 的口径一致：
// 否则 /tmp 与 /private/tmp 会各登记一份，重复上传就不幂等了。
func (s *Server) libraryDir(raw string) (*domain.Library, string, error) {
	if _, err := validateExistingDir(s.Roots.List(), raw); err != nil {
		return nil, "", err
	}
	dir := filepath.Clean(raw)
	lib := s.libraryForDir(dir)
	if lib == nil {
		return nil, "", domain.New("UPLOAD_DIR_NO_LIBRARY",
			"该目录不在任何媒体库内：请先在后台建库指向它", 409)
	}
	return lib, dir, nil
}

// uploadLibrary 按 id 取库并复核它仍在允许根内。
func (s *Server) uploadLibrary(libID string) (*domain.Library, error) {
	if strings.TrimSpace(libID) == "" {
		return nil, domain.New("UPLOAD_NEED_LIBRARY", "请选择媒体库", 400)
	}
	lib, err := s.DB.GetLibrary(libID)
	if err != nil {
		return nil, err
	}
	if err := media.ValidateAllowedLibrary(s.Roots.List(), lib.RootPath); err != nil {
		return nil, err
	}
	if st, serr := os.Stat(lib.RootPath); serr != nil || !st.IsDir() {
		return nil, domain.New("UPLOAD_LIBRARY_UNAVAILABLE", "媒体库根目录不可用", 409)
	}
	return lib, nil
}

// ensureSeriesDir 建出 <库根>/<剧场名> 并复核它在允许根内。
func (s *Server) ensureSeriesDir(lib *domain.Library, title string) (string, error) {
	name, err := safeDirName(title)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Clean(lib.RootPath), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", domain.New("UPLOAD_DIR_CREATE_FAILED", "创建剧场目录失败："+err.Error(), 500)
	}
	if _, err := validateExistingDir(s.Roots.List(), dir); err != nil {
		return "", err
	}
	return dir, nil
}

// ============================================================================
//  PUT（流式落到 .zvpart，成功才原子改名）
// ============================================================================

// HandleUploadPut 边收边写：io.Copy 到 .zvpart，绝不 read-all 进内存。
// Content-Range 给断点位置时可续传；读失败/超限/大小不符一律删掉 .part。
func (s *Server) HandleUploadPut(w http.ResponseWriter, r *http.Request) {
	// 大文件要跑很久：按面板同名思路把读截止时间往后推（保留上限，不清零）。
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(longUploadWindow))

	session, ok := s.Uploads.get(r.PathValue("id"))
	if !ok {
		s.fail(w, r, domain.New("UPLOAD_SESSION_NOT_FOUND", "上传会话不存在或已过期，请重新开始", 404))
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 || index >= len(session.Files) {
		s.fail(w, r, domain.New("UPLOAD_INDEX_INVALID", "上传序号不合法", 400))
		return
	}
	file := session.Files[index]
	maxBytes := s.Cfg.UploadMaxBytes()
	if file.Size > maxBytes {
		s.fail(w, r, domain.New("UPLOAD_FILE_TOO_LARGE",
			fmt.Sprintf("文件超过单文件上限 %d MB", s.Cfg.UploadMaxFileMB), 413))
		return
	}
	part := file.Final + partSuffix
	offset, hasRange, oerr := resumeOffset(r.Header.Get("Content-Range"))
	if oerr != nil {
		s.fail(w, r, domain.New("UPLOAD_RANGE_INVALID", "断点信息不合法", 400))
		return
	}
	if file.Size > 0 && offset > file.Size {
		s.fail(w, r, domain.New("UPLOAD_RANGE_INVALID", "断点位置超过文件大小", 400))
		return
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		st, serr := os.Stat(part)
		if serr != nil || st.Size() != offset {
			s.fail(w, r, domain.New("UPLOAD_RESUME_MISMATCH",
				"断点位置与已收到的内容不一致，请从头重传", 409))
			return
		}
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		s.fail(w, r, domain.New("UPLOAD_OPEN_FAILED", "无法写入目标目录："+err.Error(), 500))
		return
	}
	written, cerr := io.Copy(f, http.MaxBytesReader(w, r.Body, maxBytes))
	syncErr := f.Sync()
	closeErr := f.Close()
	if cerr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(part)
		if isTooLarge(cerr) {
			s.fail(w, r, domain.New("UPLOAD_FILE_TOO_LARGE",
				fmt.Sprintf("文件超过单文件上限 %d MB", s.Cfg.UploadMaxFileMB), 413))
			return
		}
		s.fail(w, r, domain.New("UPLOAD_WRITE_FAILED", "上传中断，已清理临时文件", 400))
		return
	}
	total := offset + written
	if file.Size > 0 && total > file.Size {
		_ = os.Remove(part)
		s.fail(w, r, domain.New("UPLOAD_SIZE_MISMATCH",
			fmt.Sprintf("收到的字节数超过声明（收到 %d，声明 %d）", total, file.Size), 400))
		return
	}
	if file.Size > 0 && total < file.Size {
		if !hasRange {
			// 整文件 PUT 没传完：删掉半截，提示重传。
			_ = os.Remove(part)
			s.fail(w, r, domain.New("UPLOAD_SIZE_MISMATCH",
				fmt.Sprintf("收到的字节数与声明不符（收到 %d，声明 %d）", total, file.Size), 400))
			return
		}
		// 分块上传：保留 .part，回 202 让客户端接着传。
		respond(w, http.StatusAccepted, map[string]any{
			"received": false, "index": index, "received_bytes": total}, nil)
		return
	}
	final := file.Final
	if !session.Overwrite {
		if _, serr := os.Stat(final); serr == nil {
			final = filepath.Join(session.Dir, uniqueName(session.Dir, filepath.Base(final)))
		}
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		s.fail(w, r, domain.New("UPLOAD_RENAME_FAILED", "落盘失败："+err.Error(), 500))
		return
	}
	session.mu.Lock()
	file.Final, file.Name, file.Received = final, filepath.Base(final), true
	if file.Size == 0 {
		file.Size = total
	}
	session.mu.Unlock()
	respond(w, http.StatusOK, map[string]any{
		"received": true, "index": index, "name": filepath.Base(final),
		"path": final, "size": total}, nil)
}

func isTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// resumeOffset reads "bytes <start>-<end>/<total>"; empty means start from zero.
// hasRange=true 表示这是分块（未传完时保留 .part 等下一块）。
func resumeOffset(header string) (int64, bool, error) {
	h := strings.TrimSpace(header)
	if h == "" {
		return 0, false, nil
	}
	if !strings.HasPrefix(strings.ToLower(h), "bytes ") {
		return 0, true, errors.New("bad unit")
	}
	spec := strings.TrimSpace(h[len("bytes "):])
	spec = strings.SplitN(spec, "/", 2)[0]
	start := strings.SplitN(spec, "-", 2)[0]
	n, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	if err != nil || n < 0 {
		return 0, true, errors.New("bad start")
	}
	return n, true, nil
}

// ============================================================================
//  finish（只登记这批文件）
// ============================================================================

type uploadFinishJSON struct {
	Dir          string `json:"dir"`
	SeriesID     string `json:"series_id,omitempty"`
	Registered   int    `json:"registered"`
	Added        int    `json:"added"`
	Skipped      int    `json:"skipped"`
	Recognized   int    `json:"recognized"`
	Unidentified int    `json:"unidentified"`
	EpisodeCount int    `json:"episode_count"`
}

// HandleUploadFinish 只登记本会话收到的文件：建 media、按文件名识别集号建集；
// 不顺带扫全库，重复 finish 也不会重复建（幂等）。
func (s *Server) HandleUploadFinish(w http.ResponseWriter, r *http.Request) {
	session, ok := s.Uploads.get(r.PathValue("id"))
	if !ok {
		s.fail(w, r, domain.New("UPLOAD_SESSION_NOT_FOUND", "上传会话不存在或已过期，请重新开始", 404))
		return
	}
	lib, err := s.DB.GetLibrary(session.LibraryID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files := []dirimport.File{}
	session.mu.Lock()
	for _, f := range session.Files {
		if !f.Received {
			_ = os.Remove(f.Final + partSuffix)
			continue
		}
		info, serr := os.Stat(f.Final)
		if serr != nil {
			continue
		}
		files = append(files, dirimport.Describe(f.Final, info.Size()))
	}
	session.mu.Unlock()

	out := uploadFinishJSON{Dir: session.Dir, SeriesID: session.SeriesID}
	for _, f := range files {
		if f.Recognized {
			out.Recognized++
		} else {
			out.Unidentified++
		}
	}
	if session.SeriesID != "" {
		reg, added, skipped, unid, rerr := s.registerAndLink(r.Context(), session.SeriesID, lib, files)
		if rerr != nil {
			s.fail(w, r, rerr)
			return
		}
		out.Registered, out.Added, out.Skipped, out.Unidentified = reg, added, skipped, unid
		fresh, gerr := s.DB.GetSeries(session.SeriesID)
		if gerr == nil {
			out.EpisodeCount = fresh.EpisodeCount
		}
	} else {
		reg, unid, rerr := s.registerMediaOnly(lib, files)
		if rerr != nil {
			s.fail(w, r, rerr)
			return
		}
		out.Registered, out.Unidentified = reg, unid
	}
	s.Uploads.drop(session.ID)
	cleanParts(session.Dir)
	s.audit(r, "upload.finish", "dir:"+session.Dir, true,
		fmt.Sprintf("files:%d added:%d", len(files), out.Added))
	respond(w, http.StatusOK, out, nil)
}

// ============================================================================
//  小工具
// ============================================================================

// safeFileName 只接受纯文件名（无路径分隔、无隐藏前缀、长度合理）。
func safeFileName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || name == "." || name == ".." {
		return "", domain.New("UPLOAD_NAME_INVALID", "文件名不合法", 400)
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", domain.New("UPLOAD_NAME_INVALID", "文件名不能带路径："+name, 400)
	}
	if strings.HasPrefix(name, ".") {
		return "", domain.New("UPLOAD_NAME_INVALID", "不接受隐藏文件："+name, 400)
	}
	if len([]rune(name)) > 200 {
		return "", domain.New("UPLOAD_NAME_INVALID", "文件名过长："+name, 400)
	}
	return name, nil
}

// safeDirName 用作剧场目录名：拒绝路径分隔与隐藏名。
func safeDirName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || name == "." || name == ".." {
		return "", domain.New("UPLOAD_DIR_NAME_INVALID", "剧场名不能用作目录："+raw, 400)
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) || strings.HasPrefix(name, ".") {
		return "", domain.New("UPLOAD_DIR_NAME_INVALID", "剧场名不能用作目录："+raw, 400)
	}
	return name, nil
}

// uniqueName 在 dir 里找不冲突的名字：a.mp4 → a (1).mp4。
func uniqueName(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
	return fmt.Sprintf("%s (%d)%s", stem, time.Now().UnixNano(), ext)
}

// cleanParts 删掉目录里中断上传残留的 .zvpart（只删我们自己后缀的文件，不递归）。
func cleanParts(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), partSuffix) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// availableBytes 返回目标目录所在卷的可用字节数（Statfs）。
func availableBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	value := float64(n)
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[idx])
	}
	return fmt.Sprintf("%.1f %s", value, units[idx])
}

// validateExistingDir 复用 media.ValidateLibraryPath：绝对/规范/允许根内/可读。
func validateExistingDir(roots []string, raw string) (string, error) {
	return media.ValidateLibraryPath(roots, raw)
}

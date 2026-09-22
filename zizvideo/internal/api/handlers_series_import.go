package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/zizdog/zizvideo/internal/dirimport"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/fsbrowse"
	"github.com/zizdog/zizvideo/internal/media"
	"github.com/zizdog/zizvideo/internal/storage"
)

// 按目录建剧场：只遍历用户选中的目录，集号识别走 internal/detect，
// 不在任何媒体库内的目录、超限目录一律如实拒绝（A 单剧场 / B 批量）。

const (
	dirImportMaxFiles = 5000 // A：单目录上限
	dirBatchMaxDirs   = 1000 // B：一级子目录上限
	dirBatchMaxFiles  = 5000 // B：每个子目录上限（与 A 一致）
	dirImportMaxDepth = 5
)

type dirImportReq struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
	Depth     int    `json:"depth"`
}

// depth 把"包含子目录"翻译成下钻层数：不勾=0，勾了默认 1 层，最多 5 层。
func (r dirImportReq) walkDepth() int {
	if !r.Recursive {
		return 0
	}
	d := r.Depth
	if d < 1 {
		d = 1
	}
	if d > dirImportMaxDepth {
		d = dirImportMaxDepth
	}
	return d
}

func (s *Server) extAllowed(ext string) bool { return s.Cfg.ExtAllowed(ext) }

// resolveImportDir 校验"绝对/规范/允许根内/可读"（复用 media.ValidateLibraryPath），
// 再找它所属的媒体库；不在任何库内就拒绝——媒体记录必须有库可挂。
// 返回的目录用调用方的拼写（不解析符号链接）：与扫描器按 library.root_path 拼写登记
// 的 media.path 保持一致，否则 /tmp 与 /private/tmp 会各登记一份。
func (s *Server) resolveImportDir(raw string) (*domain.Library, string, error) {
	clean, err := fsbrowse.Validate(raw)
	if err != nil {
		return nil, "", err
	}
	if _, err := media.ValidateLibraryPath(s.Roots.List(), clean); err != nil {
		return nil, "", err
	}
	lib := s.libraryForDir(clean)
	if lib == nil {
		return nil, "", domain.New("SERIES_DIR_NO_LIBRARY",
			"该目录不在任何媒体库内：请先在后台建库指向它", 409)
	}
	return lib, clean, nil
}

// libraryForDir picks the deepest library root containing dir (literal or resolved).
func (s *Server) libraryForDir(dir string) *domain.Library {
	libs, err := s.DB.ListLibraries()
	if err != nil {
		return nil
	}
	realDir := resolved(dir)
	var best *domain.Library
	bestLen := -1
	for i := range libs {
		root := filepath.Clean(libs[i].RootPath)
		realRoot := resolved(root)
		if !media.Within(dir, root) && !media.Within(dir, realRoot) &&
			!media.Within(realDir, root) && !media.Within(realDir, realRoot) {
			continue
		}
		if len(root) > bestLen {
			bestLen, best = len(root), &libs[i]
		}
	}
	return best
}

func resolved(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// validDirFiles drops paths that fail the allow-root/file check and counts the
// names detect could not read.
func (s *Server) validDirFiles(lib *domain.Library, files []dirimport.File) ([]dirimport.File, int, []string) {
	roots := s.Roots.List()
	valid := make([]dirimport.File, 0, len(files))
	paths := make([]string, 0, len(files))
	unidentified := 0
	for _, f := range files {
		if _, verr := media.ValidateMediaFile(roots, lib.RootPath, f.Path); verr != nil {
			continue // 符号链接越界之类：跳过并如实不计入
		}
		if !f.Recognized {
			unidentified++
		}
		valid = append(valid, f)
		paths = append(paths, f.Path)
	}
	return valid, unidentified, paths
}

// ensureMedia 按需登记 media 行（不 probe、不扫全库），返回 path→id 的权威映射。
// 先查后插、插入用 OR IGNORE、插入后再回读：同一路径不会重复登记（幂等）。
func (s *Server) ensureMedia(lib *domain.Library, files []dirimport.File) (map[string]string, int, error) {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	before, err := s.DB.MediaIDsByPaths(lib.ID, paths)
	if err != nil {
		return nil, 0, err
	}
	toInsert := make([]*domain.Media, 0, len(files))
	for _, f := range files {
		if _, ok := before[f.Path]; ok {
			continue
		}
		toInsert = append(toInsert, &domain.Media{
			ID: domain.NewID("med"), LibraryID: lib.ID, Path: f.Path,
			Title: strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path)),
			Size:  f.Size,
		})
	}
	if len(toInsert) > 0 {
		if _, err := s.DB.InsertMediaBatch(toInsert); err != nil {
			return nil, 0, err
		}
	}
	ids, err := s.DB.MediaIDsByPaths(lib.ID, paths)
	if err != nil {
		return nil, 0, err
	}
	return ids, len(toInsert), nil
}

// registerAndLink 是唯一的导入写入口：按需登记 media，再按识别到的集号追加进剧场。
// 已在剧场的不重复建（幂等）；AddSeriesMedia 只新增，手动排过的集号不会被覆盖。
func (s *Server) registerAndLink(ctx context.Context, seriesID string, lib *domain.Library,
	files []dirimport.File) (registered, added, skipped, unidentified int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, 0, err
	}
	valid, unidentified, _ := s.validDirFiles(lib, files)
	ids, registered, err := s.ensureMedia(lib, valid)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	inputs := make([]storage.SeriesMediaInput, 0, len(valid))
	for _, f := range orderDirFiles(valid) {
		mediaID := ids[f.Path]
		if mediaID == "" {
			continue
		}
		input := storage.SeriesMediaInput{MediaID: mediaID}
		if f.Recognized {
			input.Season, input.Episode = f.Season, f.Episode
			input.Source = domain.EpisodeSourceFilename
		}
		inputs = append(inputs, input)
	}
	addedIDs, err := s.DB.AddSeriesMedia(seriesID, inputs)
	if err != nil {
		return registered, 0, 0, unidentified, err
	}
	return registered, len(addedIDs), len(inputs) - len(addedIDs), unidentified, nil
}

// registerMediaOnly 只登记媒体行（上传到库根，不建集）。
func (s *Server) registerMediaOnly(lib *domain.Library, files []dirimport.File) (registered, unidentified int, err error) {
	valid, unidentified, _ := s.validDirFiles(lib, files)
	_, registered, err = s.ensureMedia(lib, valid)
	return registered, unidentified, err
}

// orderDirFiles 按播放顺序（season→episode→文件名自然序，未识别末尾）排好，
// 让写入的 position 也稳定可复现。
func orderDirFiles(files []dirimport.File) []dirimport.File {
	keys := make([]media.OrderKey, len(files))
	for i, f := range files {
		keys[i] = media.OrderKey{Season: f.Season, Episode: f.Episode,
			Position: i, Name: filepath.Base(f.Path)}
	}
	order := media.SortOrderKeys(keys)
	out := make([]dirimport.File, len(files))
	for i, idx := range order {
		out[i] = files[idx]
	}
	return out
}

// scanDirOrReject 把超限/不可遍历翻译成人话错误（409，提示用另一条路）。
func (s *Server) scanDirOrReject(dir string, depth, maxFiles int) ([]dirimport.File, error) {
	files, err := dirimport.Scan(dir, depth, maxFiles, s.extAllowed)
	if errors.Is(err, dirimport.ErrTooManyFiles) {
		return nil, domain.New("SERIES_DIR_TOO_LARGE",
			fmt.Sprintf("目录内视频超过 %d 个，请改用「按子目录批量建剧场」", maxFiles), 409)
	}
	if err != nil {
		return nil, domain.New("SERIES_DIR_UNREADABLE", "读取目录失败："+err.Error(), 400)
	}
	return files, nil
}

// ============================================================================
//  A：剧场内「从目录导入剧集」
// ============================================================================

type dirEntryJSON struct {
	Path             string `json:"path"`
	Season           *int   `json:"season"`
	Episode          *int   `json:"episode"`
	EpisodeLabel     string `json:"episode_label"`
	InSeries         bool   `json:"in_series"`
	OtherSeriesID    string `json:"other_series_id"`
	OtherSeriesTitle string `json:"other_series_title"`
}

type dirPreviewJSON struct {
	Path         string         `json:"path"`
	LibraryID    string         `json:"library_id"`
	LibraryName  string         `json:"library_name"`
	Recursive    bool           `json:"recursive"`
	Depth        int            `json:"depth"`
	MaxFiles     int            `json:"max_files"`
	TotalFiles   int            `json:"total_files"`
	Recognized   int            `json:"recognized"`
	Unidentified int            `json:"unidentified"`
	Entries      []dirEntryJSON `json:"entries"`
}

// HandlePreviewSeriesDirImport 同步预览：只遍历这一个目录，最多 5000 条。
func (s *Server) HandlePreviewSeriesDirImport(w http.ResponseWriter, r *http.Request) {
	seriesID := r.PathValue("id")
	if _, err := s.DB.GetSeries(seriesID); err != nil {
		s.fail(w, r, err)
		return
	}
	var req dirImportReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	lib, dir, err := s.resolveImportDir(req.Path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files, err := s.scanDirOrReject(dir, req.walkDepth(), dirImportMaxFiles)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := dirPreviewJSON{
		Path: dir, LibraryID: lib.ID, LibraryName: lib.Name,
		Recursive: req.Recursive, Depth: req.walkDepth(), MaxFiles: dirImportMaxFiles,
		TotalFiles: len(files), Entries: []dirEntryJSON{},
	}
	paths := make([]string, 0, len(files))
	mediaIDs := make([]string, 0, len(files))
	for _, f := range files {
		if f.Recognized {
			out.Recognized++
		} else {
			out.Unidentified++
		}
		paths = append(paths, f.Path)
	}
	ids, err := s.DB.MediaIDsByPaths(lib.ID, paths)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, f := range files {
		if id, ok := ids[f.Path]; ok {
			mediaIDs = append(mediaIDs, id)
		}
	}
	refs, err := s.DB.SeriesRefsByMedia(mediaIDs)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, f := range files {
		entry := dirEntryJSON{Path: f.Path, Season: f.Season, Episode: f.Episode,
			EpisodeLabel: f.Label}
		if id, ok := ids[f.Path]; ok {
			for _, ref := range refs[id] {
				if ref.ID == seriesID {
					entry.InSeries = true
					continue
				}
				if entry.OtherSeriesID == "" {
					entry.OtherSeriesID, entry.OtherSeriesTitle = ref.ID, ref.Title
				}
			}
		}
		out.Entries = append(out.Entries, entry)
	}
	respond(w, http.StatusOK, out, nil)
}

type dirImportResultJSON struct {
	Path         string `json:"path"`
	LibraryID    string `json:"library_id"`
	SeriesID     string `json:"series_id"`
	Files        int    `json:"files"`
	Registered   int    `json:"registered"`
	Added        int    `json:"added"`
	Skipped      int    `json:"skipped"`
	Unidentified int    `json:"unidentified"`
	EpisodeCount int    `json:"episode_count"`
}

// HandleSeriesDirImport 确认导入：登记 media + 按集号建集，手动排过的集号不动。
func (s *Server) HandleSeriesDirImport(w http.ResponseWriter, r *http.Request) {
	seriesID := r.PathValue("id")
	if _, err := s.DB.GetSeries(seriesID); err != nil {
		s.fail(w, r, err)
		return
	}
	var req dirImportReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	lib, dir, err := s.resolveImportDir(req.Path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files, err := s.scanDirOrReject(dir, req.walkDepth(), dirImportMaxFiles)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	registered, added, skipped, unid, err := s.registerAndLink(r.Context(), seriesID, lib, files)
	if err != nil {
		s.audit(r, "series.dir_import", "series:"+seriesID, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	fresh, err := s.DB.GetSeries(seriesID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "series.dir_import", "series:"+seriesID, true,
		fmt.Sprintf("files:%d added:%d", len(files), added))
	respond(w, http.StatusOK, dirImportResultJSON{
		Path: dir, LibraryID: lib.ID, SeriesID: seriesID, Files: len(files),
		Registered: registered, Added: added, Skipped: skipped,
		Unidentified: unid, EpisodeCount: fresh.EpisodeCount,
	}, nil)
}

// ============================================================================
//  B：剧场列表「按子目录批量建剧场」
// ============================================================================

type dirBatchEntryJSON struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	FileCount     int    `json:"file_count"`
	Recognized    int    `json:"recognized"`
	Unidentified  int    `json:"unidentified"`
	OverLimit     bool   `json:"over_limit"`
	SeriesID      string `json:"series_id"`
	ExistingTitle string `json:"existing_title"`
	WillReuse     bool   `json:"will_reuse"`
}

type dirBatchPreviewJSON struct {
	Path      string              `json:"path"`
	LibraryID string              `json:"library_id"`
	MaxDirs   int                 `json:"max_dirs"`
	Subdirs   int                 `json:"subdirs"`
	FileCount int                 `json:"file_count"`
	Entries   []dirBatchEntryJSON `json:"entries"`
}

// HandlePreviewSeriesDirs 列出目录下每个一级子目录 = 一个剧场（子目录内递归计数）。
func (s *Server) HandlePreviewSeriesDirs(w http.ResponseWriter, r *http.Request) {
	var req dirImportReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	lib, dir, err := s.resolveImportDir(req.Path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	subdirs, err := dirimport.ListSubdirs(dir, dirBatchMaxDirs)
	if errors.Is(err, dirimport.ErrTooManyDirs) {
		s.fail(w, r, domain.New("SERIES_DIRS_TOO_MANY",
			fmt.Sprintf("一级子目录超过 %d 个，请分批导入", dirBatchMaxDirs), 409))
		return
	}
	if err != nil {
		s.fail(w, r, domain.New("SERIES_DIR_UNREADABLE", "读取目录失败："+err.Error(), 400))
		return
	}
	out := dirBatchPreviewJSON{Path: dir, LibraryID: lib.ID, MaxDirs: dirBatchMaxDirs,
		Subdirs: len(subdirs), Entries: []dirBatchEntryJSON{}}
	for _, sub := range subdirs {
		name := filepath.Base(sub)
		counts, cerr := dirimport.Count(sub, dirimport.Unlimited, dirBatchMaxFiles, s.extAllowed)
		entry := dirBatchEntryJSON{Name: name, Path: sub, FileCount: counts.Files,
			Recognized: counts.Recognized, Unidentified: counts.Unidentified,
			OverLimit: counts.OverLimit}
		if cerr != nil && !errors.Is(cerr, dirimport.ErrTooManyFiles) {
			entry.Unidentified = counts.Unidentified
		}
		if series, serr := s.DB.SeriesByTitle(name); serr == nil && series != nil {
			entry.SeriesID, entry.ExistingTitle, entry.WillReuse = series.ID, series.Title, true
		}
		out.FileCount += counts.Files
		out.Entries = append(out.Entries, entry)
	}
	respond(w, http.StatusOK, out, nil)
}

// importSummaryJSON is one 子目录's outcome inside the job summary.
type importSummaryJSON struct {
	Name         string `json:"name"`
	SeriesID     string `json:"series_id"`
	Created      bool   `json:"created"`
	Files        int    `json:"files"`
	Registered   int    `json:"registered"`
	Added        int    `json:"added"`
	Unidentified int    `json:"unidentified"`
	Error        string `json:"error,omitempty"`
}

type importJobSummaryJSON struct {
	DirTotal      int                 `json:"dir_total"`
	SeriesCreated int                 `json:"series_created"`
	SeriesReused  int                 `json:"series_reused"`
	FilesTotal    int                 `json:"files_total"`
	Registered    int                 `json:"registered"`
	EpisodesAdded int                 `json:"episodes_added"`
	Unidentified  int                 `json:"unidentified"`
	FailedTotal   int                 `json:"failed_total"`
	PerDir        []importSummaryJSON `json:"per_dir"`
}

// HandleSeriesDirsImport 确认后走任务中心（202 + task_id），全程幂等可重跑。
func (s *Server) HandleSeriesDirsImport(w http.ResponseWriter, r *http.Request) {
	var req dirImportReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	lib, dir, err := s.resolveImportDir(req.Path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	subdirs, err := dirimport.ListSubdirs(dir, dirBatchMaxDirs)
	if errors.Is(err, dirimport.ErrTooManyDirs) {
		s.fail(w, r, domain.New("SERIES_DIRS_TOO_MANY",
			fmt.Sprintf("一级子目录超过 %d 个，请分批导入", dirBatchMaxDirs), 409))
		return
	}
	if err != nil {
		s.fail(w, r, domain.New("SERIES_DIR_UNREADABLE", "读取目录失败："+err.Error(), 400))
		return
	}
	if len(subdirs) == 0 {
		respond(w, http.StatusOK, map[string]any{
			"task_id": "", "total": 0, "note": "这个目录下没有子目录"}, nil)
		return
	}
	t := &domain.JobTask{ID: domain.NewID("job"), Kind: domain.JobKindSeriesImport,
		Trigger: domain.JobTriggerDirBatch, Total: len(subdirs)}
	if err := s.DB.CreateJobTask(t); err != nil {
		s.fail(w, r, err)
		return
	}
	go s.runImportDirsJob(t.ID, lib, subdirs)
	s.audit(r, "series.dirs_import", "job:"+t.ID, true, "dirs:"+fmt.Sprint(len(subdirs)))
	respond(w, http.StatusAccepted, map[string]any{"task_id": t.ID, "total": len(subdirs)}, nil)
}

// runImportDirsJob 逐个一级子目录建/补剧场；失败的目录计数上报，不假装成功。
func (s *Server) runImportDirsJob(jobID string, lib *domain.Library, subdirs []string) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Error("批量建剧场任务崩溃", "job_id", jobID)
			_ = s.DB.FinishJobTask(jobID, domain.TaskFailed, "任务异常退出", "{}", 0, false, "")
		}
	}()
	ctx := context.Background()
	existing, err := s.DB.ListSeries(scopeAll())
	if err != nil {
		_ = s.DB.FinishJobTask(jobID, domain.TaskFailed, "读取现有剧场失败", "{}", 0, false, "")
		return
	}
	byTitle := map[string]string{}
	for _, item := range existing {
		if _, ok := byTitle[item.Title]; !ok {
			byTitle[item.Title] = item.ID
		}
	}
	processed, updated, failed, unidentified := 0, 0, 0, 0
	created, reused, filesTotal, registered := 0, 0, 0, 0
	perDir := []importSummaryJSON{}
	problems := []string{}
	for _, sub := range subdirs {
		name := filepath.Base(sub)
		row := importSummaryJSON{Name: name}
		seriesID := byTitle[name]
		if seriesID == "" {
			series, cerr := s.createSeriesNamed(name, lib.ID)
			if cerr != nil {
				failed++
				row.Error = humanErr(cerr)
				problems = appendProblem(problems, name, row.Error)
				processed++
				perDir = append(perDir, row)
				_ = s.DB.UpdateJobTaskProgress(jobID, processed, updated, failed, 0)
				continue
			}
			seriesID = series.ID
			byTitle[name] = seriesID
			created++
			row.Created = true
		} else {
			reused++
		}
		row.SeriesID = seriesID
		files, ferr := dirimport.Scan(sub, dirimport.Unlimited, dirBatchMaxFiles, s.extAllowed)
		if ferr != nil {
			failed++
			row.Error = dirScanErr(ferr, dirBatchMaxFiles)
			problems = appendProblem(problems, name, row.Error)
			processed++
			perDir = append(perDir, row)
			_ = s.DB.UpdateJobTaskProgress(jobID, processed, updated, failed, 0)
			continue
		}
		reg, added, _, unid, ierr := s.registerAndLink(ctx, seriesID, lib, files)
		if ierr != nil {
			failed++
			row.Error = humanErr(ierr)
			problems = appendProblem(problems, name, row.Error)
		} else {
			row.Files, row.Registered, row.Added, row.Unidentified = len(files), reg, added, unid
			filesTotal += len(files)
			registered += reg
			updated += added
			unidentified += unid
		}
		processed++
		perDir = append(perDir, row)
		if perr := s.DB.UpdateJobTaskProgress(jobID, processed, updated, failed, 0); perr != nil {
			s.Log.Error("更新导入任务进度失败", "job_id", jobID, "error", perr.Error())
		}
	}
	status, errMsg := domain.TaskSuccess, ""
	if failed > 0 {
		status = domain.TaskFailed
		errMsg = fmt.Sprintf("%d/%d 个目录导入失败：%s", failed, len(subdirs), strings.Join(problems, "；"))
	}
	degraded := unidentified > 0
	reason := ""
	if degraded {
		reason = fmt.Sprintf("%d 个文件名识别不到集号，已按未识别导入", unidentified)
	}
	summary, _ := json.Marshal(importJobSummaryJSON{
		DirTotal: len(subdirs), SeriesCreated: created, SeriesReused: reused,
		FilesTotal: filesTotal, Registered: registered, EpisodesAdded: updated,
		Unidentified: unidentified, FailedTotal: failed, PerDir: perDir,
	})
	if err := s.DB.FinishJobTask(jobID, status, errMsg, string(summary), unidentified, degraded, reason); err != nil {
		s.Log.Error("写入导入任务终态失败", "job_id", jobID, "error", err.Error())
	}
	s.Log.Info("批量建剧场结束", "job_id", jobID, "status", status, "dirs", len(subdirs),
		"created", created, "reused", reused, "added", updated, "failed", failed)
}

func appendProblem(problems []string, name, msg string) []string {
	if len(problems) < 5 {
		return append(problems, name+": "+msg)
	}
	return problems
}

func dirScanErr(err error, max int) string {
	if errors.Is(err, dirimport.ErrTooManyFiles) {
		return fmt.Sprintf("目录内视频超过 %d 个，已跳过", max)
	}
	return "读取目录失败：" + err.Error()
}

// createSeriesNamed 以目录名建剧场（重名不去重，B 的调用方已按 title 复用）。
func (s *Server) createSeriesNamed(name, libraryID string) (*domain.Series, error) {
	title, err := s.seriesTitle(name)
	if err != nil {
		return nil, domain.New("SERIES_DIR_NAME_INVALID", "目录名无法作为剧场名："+name, 400)
	}
	series := &domain.Series{ID: domain.NewID("ser"), Title: title, LibraryID: libraryID}
	if err := s.DB.CreateSeries(series); err != nil {
		return nil, err
	}
	return s.DB.GetSeries(series.ID)
}

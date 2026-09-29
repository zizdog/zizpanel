package web

// api_media_clean.go —— 文件管理 → 🧹 去广告（无损）。
//
// 两件事：
//   · POST /files/media-clean-plan 只读统计（扩展名口径，不跑 ffmpeg）—— 确认窗要
//     一个**真数字**（"本次会处理 N 个文件"），文件夹还要递归，前端算不出来；
//   · POST /files/media-clean 走任务中心（202 + task_id），复用文件管理既有的
//     白名单预检、只读挂载预检、进度与结果结构（files.OpProgress/BatchResult）。
//
// 单文件流程与失败语义在 internal/mediaclean（新包）：那是"无损流拷贝 + 元数据清理"，
// 与视频压缩（videoopt 的码率规划/转码）是两件事，混在一起只会让两边都变形。

import (
	"context"
	"fmt"
	"net/http"

	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/mediaclean"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// mediaCleanReq 是两个接口共用的请求体（选中项的绝对路径，文件与文件夹都支持）。
type mediaCleanReq struct {
	Paths []string `json:"paths"`
}

// mediaCleanEngine 定位这台机器上的 ffmpeg/ffprobe（复用面板既有的 LocateCommand）。
func (s *Server) mediaCleanEngine() *mediaclean.Engine {
	ffmpeg, _ := services.LocateCommand("ffmpeg", s.Cfg.BrewPrefix)
	ffprobe, _ := services.LocateCommand("ffprobe", s.Cfg.BrewPrefix)
	return &mediaclean.Engine{Ffmpeg: ffmpeg, Ffprobe: ffprobe}
}

// resolveMediaCleanPaths 白名单预检 + 解析成真实路径（越界当场 403，不建任务）。
func (s *Server) resolveMediaCleanPaths(paths []string) ([]string, error) {
	mgr := s.fileManager()
	pairs := make([]files.FilePair, 0, len(paths))
	for _, p := range paths {
		pairs = append(pairs, files.FilePair{From: p})
	}
	if err := s.checkFileOpContainment(pairs, false); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		real, err := mgr.Resolve(p, false)
		if err != nil {
			return nil, err
		}
		out = append(out, real)
	}
	return out, nil
}

// handleFileMediaCleanPlan 只读统计：递归找出受支持的视频，给确认窗一个真数字。
//
// **绝不跑 ffmpeg/ffprobe**（那是任务里的事）：这里只按扩展名统计，
// 所以"本次会处理 N 个"不会因为探测慢而让确认窗干等。
func (s *Server) handleFileMediaCleanPlan(w http.ResponseWriter, r *http.Request) {
	var req mediaCleanReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	if len(req.Paths) == 0 {
		fail(w, http.StatusBadRequest, "请先选择要处理的视频或文件夹")
		return
	}
	resolved, err := s.resolveMediaCleanPaths(req.Paths)
	if err != nil {
		failFileErr(w, err, req.Paths...)
		return
	}
	col, cerr := mediaclean.Collect(r.Context(), resolved, nil)
	if cerr != nil {
		fail(w, http.StatusBadRequest, "统计被中断："+cerr.Error())
		return
	}
	total, selectedSkipped := 0, 0
	var bytes int64
	for _, it := range col.Items {
		if it.SkipReason == "" {
			total++
			bytes += it.Bytes
		} else {
			selectedSkipped++
		}
	}
	ok(w, map[string]any{
		"total":     total,
		"bytes":     bytes,
		"skipped":   selectedSkipped,
		"ignored":   col.IgnoredUnsupported,
		"exts":      mediaclean.SupportedExts(),
		"reason":    mediaclean.NotSupportedReason,
		"temp_note": "失败/取消时临时文件会删掉，源文件不动",
	})
}

// handleFileMediaClean 开始处理（202 + task_id）。
//
// 顺序刻意如此：先白名单（403）、再引擎（409）、再只读挂载（403），
// 全都通过才建任务 —— 绝不建一个注定失败的任务。
func (s *Server) handleFileMediaClean(w http.ResponseWriter, r *http.Request) {
	var req mediaCleanReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	if len(req.Paths) == 0 {
		fail(w, http.StatusBadRequest, "请先选择要处理的视频或文件夹")
		return
	}
	resolved, err := s.resolveMediaCleanPaths(req.Paths)
	if err != nil {
		failFileErr(w, err, req.Paths...)
		return
	}
	eng := s.mediaCleanEngine()
	if aerr := eng.Available(); aerr != nil {
		fail(w, http.StatusConflict, aerr.Error())
		return
	}
	// 源在只读挂载上就当场拒绝：去广告要写临时文件、还要把源改名成 .bak（与移动同一条判据）。
	for _, p := range resolved {
		if onReadOnlyFSFn(p) {
			fail(w, http.StatusForbidden, p+" 在只读挂载上，去广告需要写入与改名：到「磁盘管理」把这条盘改成读写并重新挂载，或先把文件复制到本地盘")
			return
		}
	}
	mgr := s.fileManager()
	pairs := make([]files.FilePair, 0, len(resolved))
	for _, p := range resolved {
		pairs = append(pairs, files.FilePair{From: p})
	}
	title := fmt.Sprintf("去广告（无损）%d 项", len(resolved))
	s.launchTask(w, r, "file_media_clean", fileOpTarget(pairs), title, "file_media_clean",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return s.runFileOpTask(ctx, log, files.OpPhaseClean,
				func(c context.Context, onProgress files.OpProgressFunc) (*files.BatchResult, error) {
					col, cerr := mediaclean.Collect(c, resolved, func(done int, current string) {
						onProgress(files.OpProgress{Phase: files.OpPhaseScan, Scanned: done, Current: current})
					})
					if cerr != nil {
						return nil, cerr
					}
					res, rerr := mediaclean.CleanBatch(c, col.Items, eng, mediaclean.Hooks{
						OnProgress: onProgress,
						// 面板以 root 跑：新文件必须交还真实用户，否则用户改不动。
						Chown: mgr.ChownRealUser,
					})
					return res, rerr
				})
		})
}

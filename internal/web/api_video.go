package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
	"github.com/zizdog/zizpanel/internal/videoopt"
)

// ============================================================================
//  文件管理 → 🎬 压缩视频（走任务中心的长任务）
//
//  与图片压缩的分工：图片压缩是独立小服务（上传式，另一个进程）；视频压缩
//  就放在文件管理里，压缩**当前目录**（非递归）里的视频，产物写进 output/。
//
//  两条硬要求（用户点名）：
//    · 转码码率绝不超过原视频 —— 实际码率 = min(用户选的, 原码率×0.95)，
//      产物回读后若 ≥ 原文件就删掉并如实记为"跳过"（绝不留下更大的文件）；
//    · 必须后台跑 —— 202 + task_id，进度与日志走任务中心 SSE，关掉页面不影响；
//      取消任务时 kill ffmpeg（exec.CommandContext）并删掉没写完的半成品。
//
//  规划只有 videoopt 里那一份：/video-plan 与 /video-compress 共用
//  videoopt.BuildPlan，所以面板上看到的计划与真正执行的东西不可能漂移。
// ============================================================================

// videoEngineAppID 是市场里提供 ffmpeg/ffprobe 的条目 ID（引擎缺失时的下一步）。
const videoEngineAppID = "ffmpeg"

// videoShrinkNote 是计划面板上必须写清楚的一句话（用户点名的口径；两种模式都成立）。
const videoShrinkNote = "产物只会更小：码率封顶在原片码率×0.95 以内"

// videoRunnerOverride 仅供单测：门禁不许跑真 ffmpeg（慢、且结论随开发机装没装而变），
// 但又必须能断言执行期的"产物更大就删掉"判据 —— 注入假 Runner 后全用假文件断言。
var videoRunnerOverride videoopt.Runner

// videoRunner 返回这台机器上的 ffmpeg 执行器（就地探测，不缓存：用户可能刚装完）。
func (s *Server) videoRunner() videoopt.Runner {
	if videoRunnerOverride != nil {
		return videoRunnerOverride
	}
	ffmpeg, _ := services.LocateCommand("ffmpeg", s.Cfg.BrewPrefix)
	ffprobe, _ := services.LocateCommand("ffprobe", s.Cfg.BrewPrefix)
	return &videoopt.FFmpegRunner{Ffmpeg: ffmpeg, Ffprobe: ffprobe}
}

// fileVideoReq 是视频压缩两个接口共用的请求体。
//
// 新字段（encoder/mode/quality/two_pass）全部**可选**：老请求不带时按 videoopt
// 的具名默认值走（DefaultEncoder + DefaultMode），接口形状只增不改。
type fileVideoReq struct {
	Dir    string `json:"dir"`
	Preset string `json:"preset"`
	// KBps 是用户选的码率（0 = 用档位下限）。
	KBps    int    `json:"kbps"`
	Encoder string `json:"encoder"`
	Mode    string `json:"mode"`
	// Quality 是质量档（CPU=CRF、硬件=-q:v；0 = 用该编码器默认档）。
	Quality int  `json:"quality"`
	TwoPass bool `json:"two_pass"`
}

// videoPlanResponse 是 POST /api/v1/files/video-plan 的响应。
type videoPlanResponse struct {
	// Available 为 false 时 Reason 是给用户的下一步（去应用市场装 FFmpeg）。
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
	Engine      string `json:"engine,omitempty"`
	MarketAppID string `json:"market_app_id"`

	Dir    string `json:"dir,omitempty"`
	OutDir string `json:"out_dir,omitempty"`
	Preset string `json:"preset"`
	KBps   int    `json:"kbps"`

	Presets        []videoopt.Preset        `json:"presets"`
	BitrateChoices []videoopt.BitrateChoice `json:"bitrate_choices"`

	// 编码器 / 模式 / 质量档：默认值与选项都由后端给，前端不重复写数字。
	Encoder        string                   `json:"encoder"`
	Mode           string                   `json:"mode"`
	Quality        int                      `json:"quality"`
	TwoPass        bool                     `json:"two_pass"`
	Encoders       []videoopt.Choice        `json:"encoders"`
	Modes          []videoopt.Choice        `json:"modes"`
	QualityChoices []videoopt.QualityChoice `json:"quality_choices"`

	Total    int             `json:"total"`
	Runnable int             `json:"runnable"`
	Skipped  int             `json:"skipped"`
	EstBytes int64           `json:"est_bytes"`
	Rows     []videoopt.Plan `json:"rows"`
	Note     string          `json:"note"`

	// 体积对比与"这样压不会变小"的警告（判据全部来自 planner）。
	TotalSourceBytes int64  `json:"total_source_bytes"`
	EstSavedBytes    int64  `json:"est_saved_bytes"`
	EstPercent       int    `json:"est_percent"`
	EstimateUnknown  bool   `json:"estimate_unknown"`
	CappedRunnable   int    `json:"capped_runnable"`
	AllCapped        bool   `json:"all_capped"`
	Warning          string `json:"warning,omitempty"`
}

// parseVideoReq 校验共用的请求参数（档位 / 码率 / 编码器 / 模式 / 质量档 / 2-pass）。
//
// 空字段 = 用默认（向后兼容）；非法组合当场 400（2-pass 与硬件/质量优先互斥）。
func parseVideoReq(req fileVideoReq) (videoopt.Options, error) {
	id := strings.TrimSpace(req.Preset)
	if id == "" {
		id = videoopt.DefaultPresetID
	}
	preset, ok := videoopt.FindPreset(id)
	if !ok {
		return videoopt.Options{}, fmt.Errorf("档位只能是 360p / 480p / 720p")
	}
	if req.KBps != 0 && (req.KBps < videoopt.MinKBps || req.KBps > videoopt.MaxKBps) {
		return videoopt.Options{}, fmt.Errorf("码率必须是 %d~%d kbps 的整数", videoopt.MinKBps, videoopt.MaxKBps)
	}
	opts := videoopt.Options{
		Preset: preset, KBps: req.KBps,
		Encoder: req.Encoder, Mode: req.Mode, Quality: req.Quality, TwoPass: req.TwoPass,
	}
	if err := videoopt.ValidateOptions(opts); err != nil {
		return videoopt.Options{}, err
	}
	return videoopt.NormalizeOptions(opts), nil
}

// newVideoPlanResponse 填好与目录无关的字段（档位/码率/选项始终要有，
// 引擎缺失时前端也要能把面板画出来，并给出「一键安装 FFmpeg」）。
func newVideoPlanResponse(opts videoopt.Options) videoPlanResponse {
	return videoPlanResponse{
		Preset:         opts.Preset.ID,
		KBps:           videoopt.ResolveKBps(opts.Preset, opts.KBps),
		Presets:        videoopt.Presets(),
		BitrateChoices: videoopt.BitrateChoices(opts.Preset),
		Encoder:        opts.Encoder,
		Mode:           opts.Mode,
		Quality:        opts.Quality,
		TwoPass:        opts.TwoPass,
		Encoders:       videoopt.EncoderChoices(),
		Modes:          videoopt.ModeChoices(),
		QualityChoices: videoopt.QualityChoices(opts.Encoder),
		MarketAppID:    videoEngineAppID,
		Rows:           []videoopt.Plan{},
		Note:           videoShrinkNote,
	}
}

// videoCapWarning 是"用户选的码率 ≥ 原片码率"时那句必须醒目的话（判据来自 planner 的 capped）。
//
// 全部文件都这样时说得更直接；只有一部分时逐行标（行内 Note 已有）。
func videoCapWarning(preset videoopt.Preset, capped, runnable int) string {
	if capped == 0 || runnable == 0 {
		return ""
	}
	if capped >= runnable {
		return fmt.Sprintf("原码率都不高：这样压基本不会变小。建议 ≤ %d kbps 或改用质量优先", preset.DefaultKbps)
	}
	return fmt.Sprintf("有 %d 个视频原码率不高，压完基本不变小（见下表）；建议 ≤ %d kbps",
		capped, preset.DefaultKbps)
}

// handleFileVideoPlan 只读规划：每个视频一行（原分辨率/原码率/目标/预计大小/跳过原因）。
func (s *Server) handleFileVideoPlan(w http.ResponseWriter, r *http.Request) {
	var req fileVideoReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	opts, err := parseVideoReq(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := newVideoPlanResponse(opts)

	runner := s.videoRunner()
	if err := runner.Available(); err != nil {
		// 如实说：没有引擎就没法规划（绝不假装能压，也绝不返回空计划让用户干瞪眼）。
		resp.Reason = err.Error()
		ok(w, resp)
		return
	}
	dir, derr := s.fileResolveDir(req.Dir, false)
	if derr != nil {
		failFileErr(w, derr, req.Dir)
		return
	}
	res, berr := videoopt.BuildPlan(r.Context(), videoopt.PlanRequest{
		Dir: dir, Options: opts,
	}, runner)
	if berr != nil {
		failFileErr(w, berr, dir)
		return
	}
	resp.Available = true
	resp.Engine = runner.Version(r.Context())
	resp.Dir, resp.OutDir = res.Dir, res.OutDir
	resp.KBps = res.KBps
	resp.Total, resp.Runnable, resp.Skipped, resp.EstBytes = len(res.Rows), res.Runnable, res.Skipped, res.EstBytes
	resp.Rows = res.Rows
	resp.TotalSourceBytes, resp.EstSavedBytes = res.TotalSourceBytes, res.EstSavedBytes
	resp.EstPercent, resp.EstimateUnknown = res.EstPercent, res.EstimateUnknown
	resp.CappedRunnable, resp.AllCapped = res.CappedRunnable, res.AllCapped
	resp.Warning = videoCapWarning(opts.Preset, res.CappedRunnable, res.Runnable)
	ok(w, resp)
}

// handleFileVideoCompress 开始压缩（202 + task_id，长任务中心）。
//
// 与 /video-plan 共用 videoopt.BuildPlan：这里是"规划一次 → 按同一个计划执行"，
// 所以任务里压的每一条、每个码率都与用户在计划表上看到的一致。
func (s *Server) handleFileVideoCompress(w http.ResponseWriter, r *http.Request) {
	var req fileVideoReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	opts, err := parseVideoReq(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	runner := s.videoRunner()
	if err := runner.Available(); err != nil {
		// 引擎缺失：409 + 可照做的下一步（不建一个注定失败的任务）。
		fail(w, http.StatusConflict, err.Error())
		return
	}
	dir, derr := s.fileResolveDir(req.Dir, false)
	if derr != nil {
		failFileErr(w, derr, req.Dir)
		return
	}
	plan, berr := videoopt.BuildPlan(r.Context(), videoopt.PlanRequest{
		Dir: dir, Options: opts,
	}, runner)
	if berr != nil {
		failFileErr(w, berr, dir)
		return
	}
	if plan.Runnable == 0 {
		// 一个都压不了就**别开任务**：开一个只会立刻失败的任务等于让用户白等。
		fail(w, http.StatusBadRequest, fmt.Sprintf(
			"没有可压缩的视频：%d 个都跳过了（原因见计划表）", plan.Skipped))
		return
	}

	mgr := s.fileManager()
	title := fmt.Sprintf("压缩视频（%d 个 · %s）", plan.Runnable, opts.Preset.ID)
	s.launchTask(w, r, "video_compress", dir, title, "file_video_compress",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			res, rerr := videoopt.RunPlan(ctx, plan.OutDir, plan.Rows, runner, videoopt.Hooks{
				Log: log,
				// 面板以 root 跑：产物必须交还真实用户，否则用户在 Finder 里改不动。
				Chown: mgr.ChownRealUser,
			})
			if rerr != nil {
				return res, rerr
			}
			return res, nil
		})
}

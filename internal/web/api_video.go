package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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

// videoShrinkNote 是计划面板上必须写清楚的一句话（用户点名的口径）。
//
// 两种模式分开说：目标码率体积可预估；质量优先（CRF/q:v）**体积不可预估**，
// 可能压不小 —— 那时执行兜底会删掉产物、原样放进 output 并如实说明。
const videoShrinkNote = "产物只会更小：码率封顶在原片码率×0.95 以内"

// videoQualityNote 是质量优先模式下必须写出的那句风险提示（用户点名）。
const videoQualityNote = "质量优先不预估体积：可能不小于原文件，届时会跳过并说明"

// videoEngineTTL 是 ffmpeg 版本串的缓存存活期（版本只在装/升级 ffmpeg 时变）。
const videoEngineTTL = 5 * time.Minute

// videoRunnerOverride 仅供单测：门禁不许跑真 ffmpeg（慢、且结论随开发机装没装而变），
// 但又必须能断言执行期的"产物更大就删掉"判据 —— 注入假 Runner 后全用假文件断言。
var videoRunnerOverride videoopt.Runner

// videoRunner 返回这台机器上的 ffmpeg 执行器（路径就地定位，不缓存：用户可能刚装完；
// 探到的**视频信息**另有进程级缓存 ProbeCache）。
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
	// Sources 是弹窗计划表里"可压行"的指纹（文件名 + 源字节数，可空）。
	// 任务里重新探测时用它核对文件有没有在两次探测之间变过（见 videoopt.PlanRequest.Expect）。
	Sources []fileVideoSource `json:"sources,omitempty"`
	// Recursive 为 true 时连子目录里的视频一起处理，产物按原目录结构放进 output/。
	// 缺省 false = 与既有行为逐字节一致（只看当前这一层）。
	Recursive bool `json:"recursive,omitempty"`
	// Rescan 为 true 时先让该目录的探测缓存失效再规划 ——「⟳ 重新扫描」按钮走它，
	// 用于"文件内容变了但 size/mtime 没变"或用户就是想强制重读一遍。
	Rescan bool `json:"rescan,omitempty"`
	// Names 是"只处理选中的这些视频"（当前目录下的**文件名**）；空 = 处理全部视频。
	// 不合法（带路径/..）当场 400；不是视频或不存在则逐条如实标记；全不合法整体 400。
	Names []string `json:"names,omitempty"`
}

// fileVideoSource 是计划表一行的指纹（只用来发现"文件变了"，不参与规划判据）。
// 递归时带 rel_path（同名文件散在不同子目录时靠它区分）。
type fileVideoSource struct {
	Name    string `json:"name"`
	RelPath string `json:"rel_path,omitempty"`
	Bytes   int64  `json:"bytes"`
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
	Encoder string `json:"encoder"`
	// EncoderCodec 是真正传给 ffmpeg 的 -c:v（硬件档 = hevc_videotoolbox）。
	EncoderCodec   string                   `json:"encoder_codec"`
	Mode           string                   `json:"mode"`
	Quality        int                      `json:"quality"`
	TwoPass        bool                     `json:"two_pass"`
	Encoders       []videoopt.Choice        `json:"encoders"`
	Modes          []videoopt.Choice        `json:"modes"`
	QualityChoices []videoopt.QualityChoice `json:"quality_choices"`

	// Names/Rejects 回显"只处理选中的这些"（Names 空 = 全部视频）与逐条拒绝原因。
	Names   []string              `json:"names,omitempty"`
	Rejects []videoopt.NameReject `json:"rejects,omitempty"`

	Total    int             `json:"total"`
	Runnable int             `json:"runnable"`
	Skipped  int             `json:"skipped"`
	EstBytes int64           `json:"est_bytes"`
	Rows     []videoopt.Plan `json:"rows"`
	Note     string          `json:"note"`
	// Probed 是这一次真正跑了 ffprobe 的次数（缓存命中不算）。
	Probed int `json:"probed"`

	// 体积对比与"码率已到极限 ⇒ 不转码、原样放进 output"的提醒（判据全部来自 planner）。
	TotalSourceBytes int64  `json:"total_source_bytes"`
	EstSavedBytes    int64  `json:"est_saved_bytes"`
	EstPercent       int    `json:"est_percent"`
	EstimateUnknown  bool   `json:"estimate_unknown"`
	CappedSkipped    int    `json:"capped_skipped"`
	PlaceCount       int    `json:"place_count"`
	Warning          string `json:"warning,omitempty"`
	// WarningDetail 是那句提醒的细节（硬链接 / 清单文件名），面板放进 title。
	WarningDetail string `json:"warning_detail,omitempty"`

	// Recursive 回显这次是否扫了子目录；ScanSkipped/ScanNotes 是递归扫描的如实说明
	// （跳过了哪些子目录、哪里到了深度/数量上限），面板逐条展示。
	Recursive   bool                `json:"recursive,omitempty"`
	ScanSkipped []videoopt.ScanSkip `json:"scan_skipped,omitempty"`
	ScanNotes   []string            `json:"scan_notes,omitempty"`
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
		return videoopt.Options{}, fmt.Errorf("档位只能是 360p / 480p / 720p / 1080p / 原始")
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

// parseVideoNames 校验"只处理选中的这些文件"的名字（路径安全：只能是本目录下的文件名）。
func parseVideoNames(names []string) ([]string, error) {
	return videoopt.NormalizeNames(names)
}

// requestedNamesErr 把 planner 的"一个都不是视频"错误映射成 400（其余照旧）。
func requestedNamesErr(w http.ResponseWriter, err error) bool {
	var rn *videoopt.RequestedNamesError
	if errors.As(err, &rn) {
		fail(w, http.StatusBadRequest, rn.Error())
		return true
	}
	return false
}

// namesPreflight 在**建任务之前**做一次"选了但一个都不是视频"的预检（同步、不跑 ffprobe）。
//
// 为什么不在任务里 400：接口已经回了 202，用户拿到的是一个注定失败的任务。
func namesPreflight(dir string, names []string) error {
	usable, rejects := videoopt.CheckRequestedNames(dir, names)
	if len(usable) == 0 {
		return &videoopt.RequestedNamesError{Rejects: rejects}
	}
	return nil
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
		EncoderCodec:   videoopt.EncoderCodec(opts.Encoder),
		Mode:           opts.Mode,
		Quality:        opts.Quality,
		TwoPass:        opts.TwoPass,
		Encoders:       videoopt.EncoderChoices(),
		Modes:          videoopt.ModeChoices(),
		QualityChoices: videoopt.QualityChoices(opts.Encoder),
		MarketAppID:    videoEngineAppID,
		Rows:           []videoopt.Plan{},
		Note:           videoPlanNote(opts.Mode),
	}
}

// videoPlanNote 按模式给计划面板那句说明（质量优先必须明说"体积不可预估"）。
func videoPlanNote(mode string) string {
	if videoopt.ResolveMode(mode) == videoopt.ModeQuality {
		return videoQualityNote
	}
	return videoShrinkNote
}

// videoSkipNotice 是"码率已到极限 ⇒ 不转码、原样放进 output"的提醒（判据来自 planner 的 Capped）。
//
// 主句一句话 ≤40 字；细节（硬链接 / 清单文件名）走 WarningDetail，面板放进 title。
func videoSkipNotice(capped int) string {
	if capped <= 0 {
		return ""
	}
	return fmt.Sprintf("有 %d 个视频码率已到极限：不会转码，但会原样放进 output，方便你整体处理", capped)
}

// videoSkipNoticeDetail 是那条提醒的细节（收进 title / 折叠，不占主句）。
const videoSkipNoticeDetail = "跳过的文件仍会放进 output：同卷用硬链接、不占额外空间；并写了 " +
	videoopt.SkippedListName

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
	names, err := parseVideoNames(req.Names)
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
	// 「⟳ 重新扫描」：显式丢掉这个目录的探测结论后重探一次。
	if req.Rescan {
		s.probeCache.InvalidateDir(dir)
	}
	// 首次扫描要能看到"读到第几个/共几个 + 当前文件名"：扫的过程中把进度记进
	// scanProgress，前端在请求飞行期间轮询 /video-plan-progress（改配置走缓存时
	// 整个扫描是毫秒级、也一次 ffprobe 都不跑 ⇒ 轮询拿到的 active=false）。
	s.scanProgress.Begin(dir)
	res, berr := videoopt.BuildPlanProgress(r.Context(), videoopt.PlanRequest{
		Dir: dir, Options: opts, Cache: s.probeCache, Names: names, Recursive: req.Recursive,
		OnProbe: func() { s.scanProgress.CountProbe(dir) },
	}, runner, func(done, total int, name string) {
		s.scanProgress.Update(dir, done, total, name)
	})
	s.scanProgress.End(dir)
	if berr != nil {
		if requestedNamesErr(w, berr) {
			return
		}
		failFileErr(w, berr, dir)
		return
	}
	resp.Available = true
	resp.Names, resp.Rejects = res.Names, res.Rejects
	// 引擎版本串走进程级短缓存：热路径上每次都 spawn `ffmpeg -version` 是纯浪费。
	resp.Engine = s.engineVer.Get(func() string { return runner.Version(r.Context()) })
	resp.Dir, resp.OutDir = res.Dir, res.OutDir
	resp.KBps = res.KBps
	resp.Probed = res.Probed
	resp.Total, resp.Runnable, resp.Skipped, resp.EstBytes = len(res.Rows), res.Runnable, res.Skipped, res.EstBytes
	resp.Rows = res.Rows
	resp.TotalSourceBytes, resp.EstSavedBytes = res.TotalSourceBytes, res.EstSavedBytes
	resp.EstPercent, resp.EstimateUnknown = res.EstPercent, res.EstimateUnknown
	resp.CappedSkipped, resp.PlaceCount = res.CappedSkipped, res.PlaceCount
	resp.Recursive, resp.ScanSkipped, resp.ScanNotes = res.Recursive, res.ScanSkipped, res.ScanNotes
	resp.Warning = videoSkipNotice(res.CappedSkipped)
	if resp.Warning != "" {
		resp.WarningDetail = videoSkipNoticeDetail
	}
	ok(w, resp)
}

// videoPlanProgress 是首次扫描的进度响应（前端在 /video-plan 飞行期间轮询）。
type videoPlanProgress struct {
	// Active=false 表示这个目录当前没有"真在探测"的扫描（已缓存/空闲/刚跑完）。
	Active  bool   `json:"active"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// handleFileVideoPlanProgress 只读回"这次扫描读到第几个了"。
//
// 只在扫描**真的在跑 ffprobe** 时 active=true：缓存命中的扫描（改配置/二次打开）
// 是毫秒级且 0 次 ffprobe ⇒ 前端不会再闪出「正在读取视频信息…」这个过程。
func (s *Server) handleFileVideoPlanProgress(w http.ResponseWriter, r *http.Request) {
	dir, derr := s.fileResolveDir(r.URL.Query().Get("dir"), false)
	if derr != nil {
		failFileErr(w, derr, r.URL.Query().Get("dir"))
		return
	}
	p, _ := s.scanProgress.Get(dir)
	ok(w, videoPlanProgress{
		Active: p.Probed > 0, Done: p.Done, Total: p.Total, Name: p.Name, Message: p.Message,
	})
}

// handleFileVideoCompress 开始压缩（202 + task_id，长任务中心）。
//
// **接口在返回前绝不跑 ffprobe**（用户实测的空档根因）：以前这里同步调
// videoopt.BuildPlan（每个视频一次探测）才回 202，而前端此时已把弹窗关掉 ⇒
// "面板消失 → 干等 → 进度窗才出现"。现在规划/探测整体挪进任务，任务第一段
// 就是 `phase=scan` 的探测阶段（结构化进度如实上报第几个/共几个）。
//
// 规划仍只有 videoopt 那一份实现（BuildPlanProgress → PlanOne）：弹窗计划表
// 与真正执行是同一套判据；请求里带的是计划表指纹（Sources），任务重新探测后
// 逐条核对，变了/不见了就如实跳过（绝不静默按旧计划压）。
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
	names, err := parseVideoNames(req.Names)
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
	// 选了但一个都不是视频：当场 400（绝不建一个注定失败的任务）。
	// 递归时不在这一层预检（子目录里的文件名字在基准目录里 Stat 不到，会误拒）。
	if len(names) > 0 && !req.Recursive {
		if nerr := namesPreflight(dir, names); nerr != nil {
			fail(w, http.StatusBadRequest, nerr.Error())
			return
		}
	}
	expect := make(map[string]int64, len(req.Sources))
	for _, src := range req.Sources {
		// 指纹键与 planner 同口径：递归用相对路径，非递归用文件名。
		key := strings.TrimSpace(src.RelPath)
		if key == "" {
			key = strings.TrimSpace(src.Name)
		}
		if key != "" {
			expect[key] = src.Bytes
		}
	}
	// 标题不带个数：个数要等任务里探测完才知道（接口不许为它等）。
	suffix := ""
	if req.Recursive {
		suffix = " · 含子目录"
	}
	title := fmt.Sprintf("压缩视频（%s%s）", opts.Preset.ID, suffix)
	s.launchTask(w, r, "video_compress", dir, title, "file_video_compress",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return s.runVideoCompress(ctx, dir, opts, names, expect, req.Recursive, runner, log)
		})
}

// runVideoCompress 是任务体：先探测规划（如实上报 scan 进度），再按同一份计划执行。
func (s *Server) runVideoCompress(ctx context.Context, dir string, opts videoopt.Options,
	names []string, expect map[string]int64, recursive bool, runner videoopt.Runner, log tasks.LogFunc) (*videoopt.RunResult, error) {

	tasks.ReportProgress(ctx, tasks.Progress{Phase: "scan", Message: "正在读取视频信息…"})
	plan, err := videoopt.BuildPlanProgress(ctx, videoopt.PlanRequest{
		Dir: dir, Options: opts, Expect: expect, Cache: s.probeCache, Names: names, Recursive: recursive,
	}, runner, func(done, total int, name string) {
		tasks.ReportProgress(ctx, tasks.Progress{
			Phase: "scan", FilesDone: done, FilesTotal: total,
			Message: fmt.Sprintf("正在读取视频信息 %d/%d：%s", done+1, total, name),
		})
	})
	if err != nil {
		tasks.ReportProgress(ctx, tasks.Progress{Phase: "scan", Message: "读取视频信息失败：" + err.Error()})
		return nil, err
	}
	// 递归扫描的如实说明（跳过哪些子目录 / 哪里到了上限）必须先于执行说清楚。
	for _, sk := range plan.ScanSkipped {
		log(tasks.LevelWarn, "↷ 已跳过子目录 "+sk.RelPath+"："+sk.Reason)
	}
	for _, note := range plan.ScanNotes {
		log(tasks.LevelWarn, "⚠ "+note)
	}
	tasks.ReportProgress(ctx, tasks.Progress{
		Phase: "scan", FilesDone: len(plan.Rows), FilesTotal: len(plan.Rows),
		Message: fmt.Sprintf("读取完成：%d 个可压，%d 个跳过", plan.Runnable, plan.Skipped),
	})
	// 一个都压不了但**有跳过的文件要放进 output/** 时照跑：这一趟的产出就是"完整一套"
	// （用户要的工作流）—— 绝不在这里提前退出，否则 output/ 会缺那些文件。
	if plan.Runnable == 0 && plan.PlaceCount == 0 {
		msg := fmt.Sprintf("没有可压缩的视频：%d 个都跳过了（原因见上方日志）", plan.Skipped)
		tasks.ReportProgress(ctx, tasks.Progress{Phase: "done", Message: msg})
		return nil, fmt.Errorf("%s", msg)
	}
	log(tasks.LevelStep, fmt.Sprintf("计划：%d 个可压、%d 个跳过（其中 %d 个原样放进 output）",
		plan.Runnable, plan.Skipped, plan.PlaceCount))

	mgr := s.fileManager()
	res, rerr := videoopt.RunPlan(ctx, plan.OutDir, plan.Rows, runner, videoopt.Hooks{
		Log: log,
		// 面板以 root 跑：产物必须交还真实用户，否则用户在 Finder 里改不动。
		Chown: mgr.ChownRealUser,
		OnProgress: func(p videoopt.ItemProgress) {
			msg := fmt.Sprintf("正在压缩 %d/%d：%s", p.Done+1, p.Total, p.Row.DisplayName())
			if p.SavedBytes > 0 {
				msg += " · 已省 " + humanBytes(p.SavedBytes)
				// 已处理源体积作分母；分母为 0（还没压出一个）时不写百分比。
				if pct := videoopt.FormatSavedPercent(p.BytesDone, p.BytesDone-p.SavedBytes); pct != "" {
					msg += "（" + pct + "）"
				}
			}
			tasks.ReportProgress(ctx, tasks.Progress{
				Phase: "compress", Done: p.BytesDone, Total: p.BytesTotal,
				FilesDone: p.Done, FilesTotal: p.Total, Message: msg,
			})
		},
	})
	// 收尾：把汇总（含百分比与原样放入 output 的个数）落进进度窗 —— 任务结束后进度
	// 不会再变，用户随时能回看。
	final := tasks.Progress{Phase: "done"}
	if res != nil {
		final.FilesDone, final.FilesTotal = res.Done, res.Total
		final.Placed = res.Placed
	}
	switch {
	case rerr != nil:
		// 失败/中断：不确定走到哪了，进度条保持"不确定"，只如实写原因。
		final.Message = "失败：" + rerr.Error()
	case res != nil:
		// 全部文件处理完：进度条拉满，文案分开计数（压缩 / 原样放入 output / 其它跳过）
		// + 本次转码用时（用户点名：进度窗要看到耗时）。
		final.Done, final.Total = res.BeforeBytes, res.BeforeBytes
		final.Message = res.SummaryText
		if res.DurationText != "" {
			final.Message += " · " + res.DurationText
		}
	default:
		final.Message = "未产生结果"
	}
	tasks.ReportProgress(ctx, final)
	if rerr != nil {
		return res, rerr
	}
	return res, nil
}

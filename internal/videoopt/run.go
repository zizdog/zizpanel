package videoopt

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  执行：ffprobe 探测 + ffmpeg 转码
//
//  为什么把执行器抽象成 Runner：门禁不许跑真 ffmpeg（慢且脆），
//  但又必须能断言"产物比原文件大 ⇒ 删掉产物并记为跳过"这类**执行期**判据。
//  注入假 Runner 后这些判据全用假文件断言。
//
//  编码参数（fixed）：libx264 -preset veryfast -profile:v main -pix_fmt yuv420p
//  -movflags +faststart，音频 aac；-b:v 与 -maxrate 同值（保证不超上限），
//  bufsize=2×。取消时 exec.CommandContext 直接 kill ffmpeg，半成品写在
//  <产物>.part.mp4，失败/取消后删除 —— 绝不留一个没写完的 mp4 冒充成品。
// ============================================================================

// Source 是一个候选视频文件（目录这一层的扫描结果）。
type Source struct {
	Name  string
	Path  string
	Bytes int64
}

// Progress 是一次转码进度（Percent 0~100）。
type Progress struct {
	Percent float64
	Speed   string
}

// TranscodeRequest 是一次转码的全部参数（已由 planner 定死）。
type TranscodeRequest struct {
	Src, Dst      string
	Width, Height int
	// VideoKbps 是目标视频码率；质量优先模式下是 -maxrate 上限（不超原片码率）。
	VideoKbps int
	// AudioKbps<=0 表示产物不要音轨（-an）。
	AudioKbps   int
	DurationSec float64
	// Encoder 是 EncoderHardware / EncoderCPU；Mode 是 ModeBitrate / ModeQuality。
	Encoder string
	Mode    string
	// Quality 是质量档：CPU=CRF（越小越好）、硬件=-q:v（越大越好）。
	Quality int
	// TwoPass 为 true 时先跑第一遍分析（只写 -passlogfile），再跑第二遍出片。
	TwoPass bool
	// PassLog 是 2-pass 的日志前缀（必须落在临时目录，任务结束清理）。
	PassLog string
}

// TranscodeArgs 返回**一遍**转码的完整 ffmpeg 参数（pass=0 单遍，1/2 是两遍）。
//
// 这里是"面板选项 → ffmpeg 命令行"的**唯一映射点**：门禁直接断言它，
// 不用真跑 ffmpeg 就能保证硬件/CPU、目标码率/质量优先、2-pass 真的传到了命令行。
//
// quality 与 passlogfile 都走参数，绝不落进 shell 字符串拼接。
func TranscodeArgs(req TranscodeRequest, pass int) []string {
	args := []string{"-hide_banner", "-nostdin", "-y", "-i", req.Src}
	if ResolveEncoder(req.Encoder) == EncoderHardware {
		// VideoToolbox：硬件编码，快很多；同码率画质略逊于 x264，码率控制也不够准。
		args = append(args, "-c:v", "h264_videotoolbox")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-profile:v", "main")
	}
	args = append(args, "-pix_fmt", "yuv420p")

	if ResolveMode(req.Mode) == ModeQuality {
		if ResolveEncoder(req.Encoder) == EncoderHardware {
			// -q:v 越大文件越大/画质越好（与 CRF 相反），实测见 plan.go 的常量注释。
			args = append(args, "-q:v", strconv.Itoa(req.Quality))
		} else {
			args = append(args, "-crf", strconv.Itoa(req.Quality))
		}
	} else {
		args = append(args, "-b:v", fmt.Sprintf("%dk", req.VideoKbps))
	}
	// 上限：目标码率模式下 -b:v 本身就是上限（同值）；质量优先模式靠 -maxrate
	// 保证码率不超原片（没有它，CRF/q:v 可能压出比原片还大的文件）。
	if req.VideoKbps > 0 {
		args = append(args, "-maxrate", fmt.Sprintf("%dk", req.VideoKbps),
			"-bufsize", fmt.Sprintf("%dk", req.VideoKbps*2))
	}
	if req.Width > 0 && req.Height > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", req.Width, req.Height))
	}
	if req.TwoPass && pass > 0 {
		args = append(args, "-pass", strconv.Itoa(pass), "-passlogfile", req.PassLog)
	}
	if pass == 1 {
		// 第一遍只做分析：不要音轨、不写成品（唯一产物是 passlog）。
		args = append(args, "-an", "-f", "null", os.DevNull)
		return args
	}
	if req.AudioKbps > 0 {
		args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", req.AudioKbps))
	} else {
		args = append(args, "-an")
	}
	// -f mp4 是必须的：半成品文件名是 .part.mp4 之外的形态也无所谓，
	// 但显式指定容器才不会因为临时名后缀而让 ffmpeg 猜错封装。
	args = append(args,
		"-movflags", "+faststart",
		"-f", "mp4",
		"-progress", "pipe:1", "-nostats", "-loglevel", "error",
		req.Dst,
	)
	return args
}

// Runner 是"探测一个文件 + 转码一个文件"的能力。
type Runner interface {
	// Available 返回 nil 表示 ffmpeg/ffprobe 都可用；否则是给用户看的下一步。
	Available() error
	// Version 返回引擎版本（读不到返回空串，不报错）。
	Version(ctx context.Context) string
	Probe(ctx context.Context, path string) (MediaInfo, error)
	Transcode(ctx context.Context, req TranscodeRequest, onProgress func(Progress)) error
}

// ----------------------------------------------------------------------------
//  真实执行器
// ----------------------------------------------------------------------------

// FFmpegRunner 用真实的 ffmpeg / ffprobe 干活。
type FFmpegRunner struct {
	Ffmpeg  string
	Ffprobe string
}

// ErrEngineMissing 是"引擎不可用"的哨兵错误（web 层据此返回 409/如实标记）。
var ErrEngineMissing = fmt.Errorf("缺少 ffmpeg / ffprobe")

// Available 检查两个命令是否都定位到了。
func (r *FFmpegRunner) Available() error {
	if r == nil || strings.TrimSpace(r.Ffmpeg) == "" || strings.TrimSpace(r.Ffprobe) == "" {
		return fmt.Errorf("%w。请到「应用市场 → FFmpeg（音视频工具）」安装后重试", ErrEngineMissing)
	}
	return nil
}

// Version 跑一次 `ffmpeg -version`（失败返回空串：版本只是展示信息）。
func (r *FFmpegRunner) Version(ctx context.Context) string {
	if r == nil || r.Ffmpeg == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, r.Ffmpeg, "-version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

// probeTimeout 是单次 ffprobe 的上限（读不出就如实跳过，绝不挂住整个任务）。
const probeTimeout = 60 * time.Second

// Probe 用 ffprobe 读一个文件的信息。
func (r *FFmpegRunner) Probe(ctx context.Context, path string) (MediaInfo, error) {
	if r == nil || r.Ffprobe == "" {
		return MediaInfo{}, ErrEngineMissing
	}
	var fileBytes int64
	if st, err := os.Stat(path); err == nil {
		fileBytes = st.Size()
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.Ffprobe,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	var errBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &errBuf, n: 2048}
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return MediaInfo{}, fmt.Errorf("ffprobe 读取失败: %s", oneLine(msg))
	}
	return ParseProbeJSON(out, fileBytes)
}

// Transcode 转码一个文件；onProgress 只在有意义的进度点被调用（由调用方限流）。
//
// 2-pass 在这里展开成两次 ffmpeg 调用：第一遍只分析（进度不报），第二遍出片。
func (r *FFmpegRunner) Transcode(ctx context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	if r == nil || r.Ffmpeg == "" {
		return ErrEngineMissing
	}
	if req.TwoPass {
		if err := r.runPass(ctx, req, 1, nil); err != nil {
			return err
		}
		return r.runPass(ctx, req, 2, onProgress)
	}
	return r.runPass(ctx, req, 0, onProgress)
}

// runPass 跑一遍 ffmpeg（参数由 TranscodeArgs 生成，这里只负责进程与进度解析）。
func (r *FFmpegRunner) runPass(ctx context.Context, req TranscodeRequest, pass int, onProgress func(Progress)) error {
	cmd := exec.CommandContext(ctx, r.Ffmpeg, TranscodeArgs(req, pass)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &errBuf, n: 4096}
	if err := cmd.Start(); err != nil {
		return err
	}

	// 读 -progress 的 key=value 流；speed 与 out_time 分两行到达，所以先缓存 speed。
	var curSpeed string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch key {
		case "speed":
			curSpeed = strings.TrimSpace(val)
		case "out_time_us", "out_time_ms":
			// 注意：ffmpeg 的 out_time_ms 实际也是**微秒**（历史命名问题），
			// 两者都按微秒处理，否则进度会差 1000 倍。
			us, perr := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			if perr != nil || req.DurationSec <= 0 || onProgress == nil {
				continue
			}
			pct := float64(us) / 1e6 / req.DurationSec * 100
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			onProgress(Progress{Percent: pct, Speed: curSpeed})
		}
	}
	werr := cmd.Wait()
	if werr != nil {
		// 取消是"任务被中断"，不是文件坏了：如实带上 ctx 错误，让调用方区分。
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = werr.Error()
		}
		return fmt.Errorf("ffmpeg 转码失败: %s", oneLine(msg))
	}
	return nil
}

// limitedWriter 只保留前 n 个字节（ffmpeg 的错误输出可能很长，别把内存吃掉）。
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	b := p
	if len(b) > l.n {
		b = b[:l.n]
	}
	l.n -= len(b)
	if l.w != nil {
		_, _ = l.w.Write(b)
	}
	return len(p), nil
}

// oneLine 把多行错误压成一行（任务日志一行一条，别让一条错误刷十几行）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}

// ----------------------------------------------------------------------------
//  扫描 + 规划
// ----------------------------------------------------------------------------

// Scan 列出目录**这一层**的视频文件（非递归）。
//
// 非递归是刻意的：output/ 之类的子目录天然不会被扫到，也不会把整棵大盘读一遍。
func Scan(dir string) ([]Source, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Source
	for _, e := range ents {
		if e.IsDir() || !IsVideoName(e.Name()) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		out = append(out, Source{Name: e.Name(), Path: filepath.Join(dir, e.Name()), Bytes: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PlanProgressFunc 是规划期的探测进度回调（done 是已探测个数，total 是本目录候选数）。
type PlanProgressFunc func(done, total int, name string)

// BuildPlan 扫目录并逐条规划 —— **/video-plan 与 /video-compress 共用这一份**。
//
// runner.Probe 会碰真实文件（ffprobe），但所有决策都在纯函数里（PlanOne）。
func BuildPlan(ctx context.Context, req PlanRequest, runner Runner) (PlanResult, error) {
	return BuildPlanProgress(ctx, req, runner, nil)
}

// BuildPlanProgress 与 BuildPlan 是同一份实现，只是多了"探测进度"回调。
//
// 为什么需要它（用户实测的空档）：视频压缩接口以前在**请求里**同步跑完这个函数
// 才回 202，前端此时已经把弹窗关掉 ⇒ "面板消失 → 干等 → 进度窗才出现"。
// 现在探测挪进任务，任务靠这个回调如实上报 `phase=scan`（第几个/共几个）。
func BuildPlanProgress(ctx context.Context, req PlanRequest, runner Runner, onProgress PlanProgressFunc) (PlanResult, error) {
	if strings.TrimSpace(req.Dir) == "" {
		return PlanResult{}, fmt.Errorf("缺少目录")
	}
	if strings.TrimSpace(req.OutDir) == "" {
		req.OutDir = filepath.Join(req.Dir, OutputDirName)
	}
	opts := NormalizeOptions(req.Options)
	res := PlanResult{
		Dir: req.Dir, OutDir: req.OutDir, Preset: opts.Preset, KBps: ResolveKBps(opts.Preset, opts.KBps),
		Encoder: opts.Encoder, Mode: opts.Mode, Quality: opts.Quality, TwoPass: opts.TwoPass,
		Rows: []Plan{},
	}
	sources, err := Scan(req.Dir)
	if err != nil {
		return res, fmt.Errorf("读取目录失败: %w", err)
	}
	// seen 记录计划表指纹里"这次真的扫到"的文件（用于收尾补"已不存在"的行）。
	seen := make(map[string]bool, len(sources))
	for i, src := range sources {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if onProgress != nil {
			onProgress(i, len(sources), src.Name)
		}
		seen[src.Name] = true
		outPath := filepath.Join(req.OutDir, OutputName(src.Name, opts.Preset.ID))
		_, statErr := os.Stat(outPath)
		outExists := statErr == nil
		info, perr := runner.Probe(ctx, src.Path)
		if perr != nil {
			res.Rows = append(res.Rows, Plan{
				Name: src.Name, Path: src.Path,
				OutName:       OutputName(src.Name, opts.Preset.ID),
				OutPath:       outPath,
				PlaceName:     PlaceName(src.Name, opts.Preset.ID),
				PlacePath:     filepath.Join(req.OutDir, PlaceName(src.Name, opts.Preset.ID)),
				SourceBytes:   src.Bytes,
				PlaceInOutput: true,
				SkipReason:    "读不出视频信息（不是视频或文件损坏）",
			})
			res.Skipped++
			res.PlaceCount++
			continue
		}
		row := PlanOne(src.Name, src.Path, req.OutDir, info, opts, outExists)
		// 与面板计划表核对：文件变了/不在表里就如实跳过，绝不按旧计划压。
		if row.Runnable() {
			if skip := expectSkip(req.Expect, row); skip != "" {
				row.SkipReason = skip
				// 源文件还在，照样原样放进 output/（保证 output 是完整一套）。
				row.PlaceInOutput = true
			}
		}
		if row.Runnable() {
			res.Runnable++
			res.EstBytes += row.EstBytes
			res.TotalSourceBytes += row.SourceBytes
			if row.EstimateUnknown {
				res.EstimateUnknown = true
			}
		} else {
			res.Skipped++
			if row.PlaceInOutput {
				res.PlaceCount++
			}
			if row.Capped {
				res.CappedSkipped++
			}
		}
		res.Rows = append(res.Rows, row)
	}
	// 计划表里有、这次扫不到的（文件被删/移走）：补一行并如实说明（没有源可放）。
	for name, bytes := range req.Expect {
		if seen[name] {
			continue
		}
		res.Rows = append(res.Rows, Plan{
			Name: name, Path: filepath.Join(req.Dir, name),
			OutName:     OutputName(name, opts.Preset.ID),
			OutPath:     filepath.Join(req.OutDir, OutputName(name, opts.Preset.ID)),
			SourceBytes: bytes,
			SkipReason:  "文件已不存在，已跳过",
		})
		res.Skipped++
	}
	if !res.EstimateUnknown {
		res.EstSavedBytes = res.TotalSourceBytes - res.EstBytes
		res.EstPercent = savePercent(res.TotalSourceBytes, res.EstBytes)
	}
	return res, nil
}

// expectSkip 用面板计划表的指纹核对这一次探测结果；空串 = 一致（照压）。
//
// 用户点名：两次探测之间变了/读不到了要**如实跳过并说明**，不许静默按旧计划压。
func expectSkip(expect map[string]int64, row Plan) string {
	if len(expect) == 0 {
		return ""
	}
	want, ok := expect[row.Name]
	if !ok {
		return "不在计划表里（目录有变化），已跳过"
	}
	if want != row.SourceBytes {
		return fmt.Sprintf("文件已变化（计划 %s → 实际 %s），已跳过",
			humanBytes(want), humanBytes(row.SourceBytes))
	}
	return ""
}

// ----------------------------------------------------------------------------
//  执行计划
// ----------------------------------------------------------------------------

// Hooks 是执行期的外部依赖：日志（任务中心）与产物归属。
type Hooks struct {
	Log   tasks.LogFunc
	Chown func(path string)
	// OnProgress 回报执行期的结构化进度（nil = 不上报）。
	OnProgress func(ItemProgress)
}

// ItemProgress 是执行期"开始处理第 N 个文件"时的一次进度快照。
type ItemProgress struct {
	// Done 是**已完成**的个数（处理 Row 之前的值）；Total 是本批总数。
	Done  int
	Total int
	Row   Plan
	// SavedBytes 是到此刻已经省下的字节（只算已成功写出的文件）。
	SavedBytes int64
	// BytesDone/BytesTotal 是源字节进度（Total = 可压行的源大小合计）。
	BytesDone  int64
	BytesTotal int64
}

func (h Hooks) log(level, msg string) {
	if h.Log != nil {
		h.Log(level, msg)
	}
}

func (h Hooks) progress(p ItemProgress) {
	if h.OnProgress != nil {
		h.OnProgress(p)
	}
}

// RunItem 是单个文件的执行结果（任务结果 JSON，前端据此汇总）。
type RunItem struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	RelPath   string `json:"rel_path,omitempty"`
	OutName   string `json:"out_name,omitempty"`
	OutPath   string `json:"out_path,omitempty"`
	Before    int64  `json:"before"`
	After     int64  `json:"after,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	VideoKbps int    `json:"video_kbps,omitempty"`
	Capped    bool   `json:"capped,omitempty"`
	// SourceKbps/SourceWidth/SourceHeight 是原片信息（《已跳过清单》要逐条写清楚）。
	SourceKbps   int    `json:"source_kbps,omitempty"`
	SourceWidth  int    `json:"source_width,omitempty"`
	SourceHeight int    `json:"source_height,omitempty"`
	SkipReason   string `json:"skip_reason,omitempty"`
	Error        string `json:"error,omitempty"`
	// Placement 是这个文件"原样放进 output/"的方式：link / copy / none；
	// PlaceReason 是放法（或没放）的原因，PlaceName 是放进 output/ 的名字。
	Placement   string `json:"placement,omitempty"`
	PlaceReason string `json:"place_reason,omitempty"`
	PlaceName   string `json:"place_name,omitempty"`
	// SavedPercentText 是这个文件省下的百分比文本（如 "-44.6%"）；
	// 源 0 字节/读不到大小时为空（面板只显示体积，绝不写 NaN%）。
	SavedPercentText string `json:"saved_percent_text,omitempty"`
}

// RunResult 是一次批量压缩的汇总。
type RunResult struct {
	Total       int   `json:"total"`
	Done        int   `json:"done"`
	Skipped     int   `json:"skipped"`
	Failed      int   `json:"failed"`
	BeforeBytes int64 `json:"before_bytes"`
	AfterBytes  int64 `json:"after_bytes"`
	SavedBytes  int64 `json:"saved_bytes"`
	// Placed 是"原样放进 output/"（没转码）的个数；CappedSkipped 是其中因码率已到极限的。
	Placed        int `json:"placed"`
	CappedSkipped int `json:"capped_skipped"`
	// SavedPercentText 是汇总百分比文本（如 "-44.6%"）；源 0 字节时为空。
	SavedPercentText string `json:"saved_percent_text,omitempty"`
	// SummaryText 是分开计数的汇总：压缩 N 个（省 X，-Y%）· 原样放入 output M 个 · 其它跳过 K 个。
	SummaryText string    `json:"summary_text,omitempty"`
	Items       []RunItem `json:"items"`
}

// RunPlan 顺序执行计划里的每个文件（一个任务压完整个目录）。
//
// 顺序而不是并发：libx264 veryfast 本身就吃满多核，并发只会互相抢 CPU
// 且让"第几个"的进度彻底失序。
func RunPlan(ctx context.Context, outDir string, rows []Plan, runner Runner, hooks Hooks) (*RunResult, error) {
	res := &RunResult{Total: len(rows), Items: make([]RunItem, 0, len(rows))}
	if len(rows) == 0 {
		return res, nil
	}
	if outDir == "" {
		outDir = filepath.Dir(rows[0].OutPath)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return res, fmt.Errorf("创建输出目录失败: %w", err)
	}
	if hooks.Chown != nil {
		hooks.Chown(outDir)
	}

	// 2-pass 的 passlogfile 必须落在临时目录，任务结束（含中断）整目录清掉。
	passDir := ""
	if hasTwoPass(rows) {
		d, terr := os.MkdirTemp("", "zizpanel-2pass-")
		if terr != nil {
			return res, fmt.Errorf("创建 2-pass 临时目录失败: %w", terr)
		}
		passDir = d
		defer os.RemoveAll(passDir)
	}

	total := len(rows)
	// 源字节前缀和：开始第 i 个文件时，前 i 个的源大小合计（进度条用）。
	prefix := make([]int64, total+1)
	for i, r := range rows {
		prefix[i+1] = prefix[i]
		if r.SkipReason == "" {
			prefix[i+1] += r.SourceBytes
		}
	}
	bytesTotal := prefix[total]
	for i, row := range rows {
		item := RunItem{Index: i, Name: row.Name, Before: row.SourceBytes}
		if err := ctx.Err(); err != nil {
			return res, cancelledErr(res)
		}
		// 结构化进度：只对真会压的行回报（跳过的行不算"正在压缩"）。
		if row.Runnable() {
			hooks.progress(ItemProgress{
				Done: i, Total: total, Row: row,
				SavedBytes: res.SavedBytes, BytesDone: prefix[i], BytesTotal: bytesTotal,
			})
		}
		if row.SkipReason != "" {
			item.SkipReason = row.SkipReason
			item.Capped = row.Capped
			item.SourceKbps, item.SourceWidth, item.SourceHeight = row.SourceVideoKbps, row.SourceWidth, row.SourceHeight
			item.RelPath = relName(row)
			res.Skipped++
			if row.Capped {
				res.CappedSkipped++
			}
			// 跳过的文件也原样放进 output/（硬链接优先），保证 output/ 是完整一套。
			if row.PlaceInOutput {
				if placeInOutput(row, &item, hooks) {
					res.Placed++
				}
			} else {
				item.Placement = PlacementNone
				// 产物已存在 vs 源已不存在：原因必须如实区分，不能都说成"已有产物"。
				if _, err := os.Stat(row.OutPath); err == nil {
					item.PlaceReason = "output 里已有产物，未重复放入"
				} else {
					item.PlaceReason = "没有可放入的源文件"
				}
			}
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelWarn, "↷ "+row.Name+"："+row.SkipReason)
			continue
		}
		// 幂等：产物已存在就不再压（计划里可能还是"可压"，执行时再确认一次）。
		// 这种情况 output/ 里已经有东西，**不要再链接/复制**（用户点名区分）。
		if _, err := os.Stat(row.OutPath); err == nil {
			item.SkipReason = "产物已存在，跳过"
			item.Placement = PlacementNone
			item.PlaceReason = "output 里已有产物，未重复放入"
			item.RelPath = relName(row)
			res.Skipped++
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelWarn, "↷ "+row.Name+"：产物已存在，跳过")
			continue
		}

		modeTag := fmt.Sprintf("%d kbps", row.VideoKbps)
		if row.Mode == ModeQuality {
			modeTag = fmt.Sprintf("质量档 %d（上限 %d kbps）", row.Quality, row.MaxRateKbps)
		}
		passTag := ""
		if row.TwoPass {
			passTag = " · 2-pass"
		}
		hooks.log(tasks.LevelStep, fmt.Sprintf("▶ 第 %d/%d 个：%s（%dx%d → %dx%d，%s%s）",
			i+1, total, row.Name, row.SourceWidth, row.SourceHeight,
			row.TargetWidth, row.TargetHeight, modeTag, passTag))
		if row.Capped && row.Note != "" {
			hooks.log(tasks.LevelWarn, row.Note)
		}

		srcBytes := row.SourceBytes
		if st, err := os.Stat(row.Path); err == nil {
			srcBytes = st.Size()
		}
		part := row.OutPath + ".part.mp4"
		_ = os.Remove(part)

		passLog := ""
		if row.TwoPass && passDir != "" {
			passLog = filepath.Join(passDir, fmt.Sprintf("pass%d", i))
		}
		throttle := &progressThrottle{}
		rerr := runner.Transcode(ctx, TranscodeRequest{
			Src: row.Path, Dst: part,
			Width: row.TargetWidth, Height: row.TargetHeight,
			VideoKbps: row.VideoKbps, AudioKbps: row.AudioKbps,
			DurationSec: row.DurationSec,
			Encoder:     row.Encoder, Mode: row.Mode, Quality: row.Quality,
			TwoPass: row.TwoPass, PassLog: passLog,
		}, func(p Progress) {
			if !throttle.allow(p.Percent) {
				return
			}
			line := fmt.Sprintf("%d%%", int(p.Percent))
			if strings.TrimSpace(p.Speed) != "" {
				line += "（" + p.Speed + "）"
			}
			hooks.log(tasks.LevelOut, line)
		})
		if rerr != nil {
			_ = os.Remove(part)
			if ctx.Err() != nil {
				hooks.log(tasks.LevelWarn, "已中断，删掉没写完的半成品："+row.OutName)
				return res, cancelledErr(res)
			}
			item.Error = rerr.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelErr, "✗ "+row.Name+"："+rerr.Error())
			continue
		}

		afterBytes := int64(0)
		if st, err := os.Stat(part); err == nil {
			afterBytes = st.Size()
		}
		item.After = afterBytes
		item.Width, item.Height, item.VideoKbps, item.Capped = row.TargetWidth, row.TargetHeight, row.VideoKbps, row.Capped

		// 硬要求：产物绝不比原文件大。大了就删掉并如实跳过，然后把**源文件**原样
		// 放进 output/（用户要的完整一套；放进去的是原片，不是那个更大的产物）。
		if afterBytes <= 0 || afterBytes >= srcBytes {
			_ = os.Remove(part)
			item.After = 0
			item.SkipReason = "压不小，已跳过（产物不小于原文件）"
			item.SourceKbps, item.SourceWidth, item.SourceHeight = row.SourceVideoKbps, row.SourceWidth, row.SourceHeight
			item.RelPath = relName(row)
			res.Skipped++
			if placeInOutput(row, &item, hooks) {
				res.Placed++
			}
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelWarn, fmt.Sprintf("↷ %s：压不小，已跳过（%s ≥ 原 %s）",
				row.Name, humanBytes(afterBytes), humanBytes(srcBytes)))
			continue
		}

		if err := os.Rename(part, row.OutPath); err != nil {
			_ = os.Remove(part)
			item.Error = "写出产物失败：" + err.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelErr, "✗ "+row.Name+"："+item.Error)
			continue
		}
		if hooks.Chown != nil {
			hooks.Chown(row.OutPath)
		}
		item.OutName, item.OutPath = row.OutName, row.OutPath
		item.SavedPercentText = FormatSavedPercent(srcBytes, afterBytes)
		res.Done++
		res.BeforeBytes += srcBytes
		res.AfterBytes += afterBytes
		res.SavedBytes += srcBytes - afterBytes
		res.Items = append(res.Items, item)
		// 明细行同时给体积与百分比（算不出百分比时只写体积，绝不写 NaN%）。
		if item.SavedPercentText != "" {
			hooks.log(tasks.LevelOK, fmt.Sprintf("✓ %s：%s → %s（省 %s，%s）",
				row.Name, humanBytes(srcBytes), humanBytes(afterBytes),
				humanBytes(srcBytes-afterBytes), item.SavedPercentText))
		} else {
			hooks.log(tasks.LevelOK, fmt.Sprintf("✓ %s：%s → %s（省 %s）",
				row.Name, humanBytes(srcBytes), humanBytes(afterBytes), humanBytes(srcBytes-afterBytes)))
		}
	}

	if err := ctx.Err(); err != nil {
		return res, cancelledErr(res)
	}
	res.SavedPercentText = FormatSavedPercent(res.BeforeBytes, res.AfterBytes)
	res.SummaryText = SummaryRunText(res)
	hooks.log(tasks.LevelStep, "完成："+res.SummaryText)
	// 跳过清单是**给人看的**（UTF-8 纯文本），只在真有跳过时写。
	if res.Skipped > 0 {
		listPath, lerr := writeSkippedList(outDir, res, res.Items)
		if lerr != nil {
			hooks.log(tasks.LevelWarn, "写《"+SkippedListName+"》失败："+oneLine(lerr.Error()))
		} else {
			if hooks.Chown != nil {
				hooks.Chown(listPath)
			}
			hooks.log(tasks.LevelStep, "已跳过清单："+listPath)
		}
	}
	if res.Failed == res.Total && res.Total > 0 {
		return res, fmt.Errorf("全部 %d 个都失败了，第一个的原因见日志", res.Failed)
	}
	return res, nil
}

// relName 是文件在任务目录里的相对路径（清单里"原相对路径"那一行）。
func relName(row Plan) string {
	if rel, err := filepath.Rel(filepath.Dir(row.Path), row.Path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return row.Name
}

// placeInOutput 把跳过的源文件原样放进 output/，并把放法与原因写进 item。
//
// 硬链接共享 inode ⇒ **绝不 chown**（那会改到源文件本身的属主）；只有复制才交还属主。
func placeInOutput(row Plan, item *RunItem, hooks Hooks) bool {
	dst := row.PlacePath
	if strings.TrimSpace(dst) == "" {
		if strings.TrimSpace(row.PlaceName) == "" {
			item.Placement = PlacementNone
			item.PlaceReason = "缺少源文件或目标路径"
			hooks.log(tasks.LevelWarn, "↷ "+row.Name+"：未放入 output（"+item.PlaceReason+"）")
			return false
		}
		dst = filepath.Join(filepath.Dir(row.OutPath), row.PlaceName)
	}
	pr := placeFile(row.Path, dst)
	item.Placement, item.PlaceReason, item.PlaceName = pr.Placement, pr.Reason, filepath.Base(dst)
	switch pr.Placement {
	case PlacementLink:
		hooks.log(tasks.LevelOK, "🔗 "+row.Name+"：原样放入 output（硬链接，不占额外空间）")
		return true
	case PlacementCopy:
		if hooks.Chown != nil {
			hooks.Chown(dst)
		}
		hooks.log(tasks.LevelOK, "📄 "+row.Name+"：原样放入 output（"+pr.Reason+"）")
		return true
	default:
		hooks.log(tasks.LevelWarn, "↷ "+row.Name+"：未放入 output（"+pr.Reason+"）")
		return false
	}
}

// cancelledErr 是中断时的错误：任务中心会据此如实标「已中断」。
func cancelledErr(res *RunResult) error {
	return fmt.Errorf("任务被取消（已成功 %d，跳过 %d，失败 %d，共 %d 个）",
		res.Done, res.Skipped, res.Failed, res.Total)
}

// SummarySavedText 是"省了 X（-Y%）· 源 A → B"的汇总文本（日志与任务进度共用）。
//
// 源 0 字节 / 读不到大小时**不写百分比**（用户点名：不许 NaN% / -Infinity%），
// 只如实说"省了 X"。
func SummarySavedText(before, after int64) string {
	saved := before - after
	if saved < 0 {
		saved = 0
	}
	txt := "省了 " + humanBytes(saved)
	if pct := FormatSavedPercent(before, after); pct != "" {
		txt += "（" + pct + "）"
	}
	if before > 0 && after > 0 {
		txt += fmt.Sprintf(" · 源 %s → %s", humanBytes(before), humanBytes(after))
	}
	return txt
}

// SummaryRunText 是分开计数的任务汇总（用户点名）：
// 压缩 N 个（省 X，-Y%）· 原样放入 output M 个（码率已到极限）· 其它跳过 K 个。
//
// "其它跳过" = 跳过里没能放进 output 的那部分（产物已存在 / 空间不足 / 源已不存在…）。
func SummaryRunText(res *RunResult) string {
	if res == nil {
		return ""
	}
	saved := res.BeforeBytes - res.AfterBytes
	if saved < 0 {
		saved = 0
	}
	savedTxt := "省 " + humanBytes(saved)
	if pct := FormatSavedPercent(res.BeforeBytes, res.AfterBytes); pct != "" {
		savedTxt += "，" + pct
	}
	other := res.Skipped - res.Placed
	if other < 0 {
		other = 0
	}
	parts := []string{
		fmt.Sprintf("压缩 %d 个（%s）", res.Done, savedTxt),
		fmt.Sprintf("原样放入 output %d 个（码率已到极限）", res.Placed),
		fmt.Sprintf("其它跳过 %d 个", other),
	}
	if res.Failed > 0 {
		parts = append(parts, fmt.Sprintf("失败 %d 个", res.Failed))
	}
	return strings.Join(parts, " · ")
}

// hasTwoPass 判断这批计划里有没有 2-pass（没有就不建临时目录）。
func hasTwoPass(rows []Plan) bool {
	for _, r := range rows {
		if r.Runnable() && r.TwoPass {
			return true
		}
	}
	return false
}

// maxProgressLines 是单个文件的进度行上限（任务日志有行数上限，绝不刷屏）。
const maxProgressLines = 20

// progressThrottle 限流进度行：每 10% 或每 5 秒一条，且单文件最多 20 条。
type progressThrottle struct {
	lastPct int
	lastAt  time.Time
	n       int
}

func (t *progressThrottle) allow(pct float64) bool {
	if t.n >= maxProgressLines {
		return false
	}
	i := int(pct)
	if i <= t.lastPct {
		return false
	}
	if t.lastPct > 0 && i-t.lastPct < 10 && time.Since(t.lastAt) < 5*time.Second {
		return false
	}
	t.lastPct = i
	t.lastAt = time.Now()
	t.n++
	return true
}

// humanBytes 给日志用的体积文本。
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	idx := -1
	for v >= unit && idx < len(units)-1 {
		v /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", v, units[idx])
}

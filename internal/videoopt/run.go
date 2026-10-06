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
	"sync"
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

// Source 是一个候选视频文件（扫描结果）。
type Source struct {
	Name string
	Path string
	// RelPath 是相对基准目录的斜杠路径（非递归时 == Name；递归时含子目录）。
	RelPath string
	Bytes   int64
	// ModTime 是文件的修改时间（探测缓存的失效判据之一）。
	ModTime time.Time
}

// Progress 是一次转码进度（Percent 0~100）。
type Progress struct {
	Percent float64
	Speed   string
	// Frame 是 ffmpeg -progress 的已编码帧数（读不到为 0）。
	// 用途：心跳行如实写"编码仍在进行（帧数 N）"—— 帧数在涨就是真的在编码。
	Frame int64
	// Pass 是第几遍（0 单遍 / 1、2 是 2-pass 的两遍）。
	// 第一遍只做分析、没有"出片进度"可显示，但**必须喂看门狗**，
	// 否则一遍健康的分析会被当成卡死（见 RunPlan 的进度回调）。
	Pass int
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
	// Fast 表示走 VideoToolbox 全 GPU 快路（硬解 → scale_vt → 硬编，帧不回内存）。
	// 只有"软件解码扛不住"的源才会开（见 needFastDecode）；跑不通由 RunPlan 逐级回退。
	Fast bool
	// SourceFPS 是源帧率（0 = 读不到）：>30 时在缩放前降到 30fps。
	SourceFPS float64
	// SrcBitDepth 是源位深（8/10；0 = 认不出）：决定 hwdownload 指名的软件格式。
	SrcBitDepth int
}

// EncoderCodec 是"编码器选项 → ffmpeg -c:v"的**唯一映射点**（面板/日志/执行都读它）。
//
// 硬件档固定用 HEVC（hevc_videotoolbox）：用户点名"选硬件加速时默认用 HEVC"。
// `-tag:v hvc1` 是 macOS/QuickTime 的硬要求：ffmpeg 默认写 hev1，QuickTime 放不了。
func EncoderCodec(encoder string) string {
	if ResolveEncoder(encoder) == EncoderHardware {
		return "hevc_videotoolbox"
	}
	return "libx264"
}

// TranscodeArgs 返回**一遍**转码的完整 ffmpeg 参数（pass=0 单遍，1/2 是两遍）。
//
// 这里是"面板选项 → ffmpeg 命令行"的**唯一映射点**：门禁直接断言它，
// 不用真跑 ffmpeg 就能保证硬件/CPU、目标码率/质量优先、2-pass 真的传到了命令行。
//
// quality 与 passlogfile 都走参数，绝不落进 shell 字符串拼接。
//
// 硬件档只换 -c:v，码率/质量参数与 CPU **逐字节相同**（用户点名"码率不变"）。
//
// 硬件档 = 快路：能硬解就整条管线留在 VideoToolbox（-hwaccel_output_format
// videotoolbox_vld + scale_vt），源高于 30fps 时在缩放前降到 30fps，编码器加
// -prio_speed 1；不能硬解（8bit/低码率源，硬解反而慢）就还是软解 + 软缩放。
func TranscodeArgs(req TranscodeRequest, pass int) []string {
	hw := ResolveEncoder(req.Encoder) == EncoderHardware
	fast := hw && req.Fast
	// -hwaccel 是**输入选项**，必须出现在 -i 之前。
	args := []string{"-hide_banner", "-nostdin", "-y"}
	if fast {
		args = append(args, "-hwaccel", "videotoolbox", "-hwaccel_output_format", "videotoolbox_vld")
	}
	args = append(args, "-i", req.Src)
	if hw {
		// VideoToolbox 硬件编码，快很多；同码率画质略逊于 x264，码率控制也不够准。
		args = append(args, "-c:v", EncoderCodec(req.Encoder), "-tag:v", "hvc1",
			// -prio_speed 1 实测把硬件编码器吞吐从 5.5x 提到 9.6x（同码率 SSIM 差 <0.1%）。
			"-prio_speed", "1")
	} else {
		args = append(args, "-c:v", EncoderCodec(req.Encoder), "-preset", "veryfast", "-profile:v", "main")
	}
	args = append(args, "-pix_fmt", "yuv420p")

	if ResolveMode(req.Mode) == ModeQuality {
		// 质量档在这里也归一化一次：0 = 该编码器的默认档（-crf 0 是"无损"，
		// 绝不是"没设置"的意思，漏掉归一化会静默压出巨大文件）。
		q := ResolveQuality(req.Encoder, req.Quality)
		if ResolveEncoder(req.Encoder) == EncoderHardware {
			// -q:v 越大文件越大/画质越好（与 CRF 相反），实测见 plan.go 的常量注释。
			args = append(args, "-q:v", strconv.Itoa(q))
		} else {
			args = append(args, "-crf", strconv.Itoa(q))
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
		args = append(args, "-vf", scaleFilter(req, hw))
	}
	if req.TwoPass && pass > 0 {
		args = append(args, "-pass", strconv.Itoa(pass), "-passlogfile", req.PassLog)
	}
	if pass == 1 {
		// 第一遍只做分析：不要音轨、不写成品（唯一产物是 passlog）。
		// 仍要 -progress：看门狗靠它区分"正在分析"与"真的卡死"，
		// 没有进度输出的话一遍 10 分钟以上的健康分析会被误杀。
		args = append(args, "-an", "-f", "null",
			"-progress", "pipe:1", "-nostats", os.DevNull)
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

// scaleFilter 返回缩放的滤镜链（三种形态，门禁逐字断言）。
//
//	快路（Fast）：硬解帧留在 GPU，scale_vt 缩放后下载成 8bit yuv420p 交给编码器
//	  —— 10bit 源的 VT 帧是 p010le、8bit 是 nv12，指名软件格式（猜错 ffmpeg 直接 -22）。
//	硬件档非快路：软解 + 软缩放，只在源高于 30fps 时先降帧（省一半缩放/编码）。
//	CPU 档：不变（scale=W:H，保持源帧率）。
func scaleFilter(req TranscodeRequest, hw bool) string {
	capFPS := hw && req.SourceFPS > maxOutputFPS
	if req.Fast {
		parts := []string{}
		if capFPS {
			parts = append(parts, fmt.Sprintf("fps=%d", maxOutputFPS))
		}
		parts = append(parts, fmt.Sprintf("scale_vt=w=%d:h=%d", req.Width, req.Height))
		if req.SrcBitDepth >= 10 {
			parts = append(parts, "hwdownload,format=p010le", "format=yuv420p")
		} else {
			parts = append(parts, "hwdownload,format=nv12")
		}
		return strings.Join(parts, ",")
	}
	if capFPS {
		return fmt.Sprintf("fps=%d,scale=%d:%d", maxOutputFPS, req.Width, req.Height)
	}
	return fmt.Sprintf("scale=%d:%d", req.Width, req.Height)
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
		// 第一遍也把进度回调传下去：RunPlan 只用它喂看门狗、不打印（Pass==1）。
		if err := r.runPass(ctx, req, 1, onProgress); err != nil {
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

	// 读 -progress 的 key=value 流；speed/frame 与 out_time 分几行到达，所以先缓存。
	var curSpeed string
	var curFrame int64
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
		case "frame":
			if n, ferr := strconv.ParseInt(strings.TrimSpace(val), 10, 64); ferr == nil {
				curFrame = n
			}
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
			onProgress(Progress{Percent: pct, Speed: curSpeed, Frame: curFrame, Pass: pass})
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
		out = append(out, Source{
			Name: e.Name(), Path: filepath.Join(dir, e.Name()), RelPath: e.Name(),
			Bytes: info.Size(), ModTime: info.ModTime(),
		})
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
	// "只处理选中的文件"：names 非空时只计划这些（探测也只探这些，缓存键不变）。
	names, nerr := NormalizeNames(req.Names)
	if nerr != nil {
		return res, nerr
	}
	res.Names = names
	// 扫描：默认只看这一层（与既有行为逐字节一致）；recursive 时走上限受控的 scanTree。
	var sources []Source
	var report ScanReport
	if req.Recursive {
		var serr error
		sources, report, serr = scanTree(ctx, req.Dir, req.OutDir, req.Limits, req.OnScan)
		if serr != nil {
			return res, fmt.Errorf("读取目录失败: %w", serr)
		}
	} else {
		var serr error
		sources, serr = Scan(req.Dir)
		if serr != nil {
			return res, fmt.Errorf("读取目录失败: %w", serr)
		}
	}
	res.Recursive = req.Recursive
	res.ScanSkipped, res.ScanNotes = report.Skipped, report.Notes
	// 扫描阶段的真实计数只属于递归（非递归响应形状必须与既有行为逐字节一致）。
	if req.Recursive {
		res.ScanDirs, res.ScanVideos = report.DirsScanned, len(sources)
	}
	if len(names) > 0 {
		usable, rejects := CheckRequestedNames(req.Dir, names)
		if req.Recursive {
			// 递归时不能只在基准目录这一层查"名字还在不在"（会误拒子目录里的文件）：
			// 改成在扫描结果里按文件名匹配，同名散在不同子目录时全部命中。
			usable, rejects = matchScannedNames(sources, names)
		}
		res.Rejects = rejects
		if len(usable) == 0 {
			// 一个都不合法：整体报错（接口层 400），绝不假装"计划为空、无事可做"。
			return res, &RequestedNamesError{Rejects: rejects}
		}
		want := make(map[string]bool, len(usable))
		for _, n := range usable {
			want[n] = true
		}
		filtered := make([]Source, 0, len(usable))
		for _, src := range sources {
			if want[src.Name] || want[src.RelPath] {
				filtered = append(filtered, src)
			}
		}
		sources = filtered
	}
	// seen 记录计划表指纹里"这次真的扫到"的文件（用于收尾补"已不存在"的行）。
	seen := make(map[string]bool, len(sources))
	for i, src := range sources {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if onProgress != nil {
			onProgress(i, len(sources), src.RelPath)
		}
		rel := src.RelPath
		if rel == "" {
			rel = src.Name
		}
		seen[rel] = true
		// 递归：产物按源相对目录分层（`动漫/第1季/01.mkv` → `output/动漫/第1季/01.480p.mp4`）。
		rowOutDir := outputDirFor(req.OutDir, rel, req.Recursive)
		outName := OutputName(src.Name, opts.Preset.ID)
		outPath := filepath.Join(rowOutDir, outName)
		_, statErr := os.Stat(outPath)
		outExists := statErr == nil
		// 探测结果按「路径 + size + mtime」缓存：配置变更（档位/码率/编码器/模式/2-pass）
		// 只走下面的纯函数重算 ⇒ 0 次 ffprobe（用户点名的性能硬要求）。
		// 文件一变（size 或 mtime）就是未命中，必须重探 —— 绝不拿旧数据糊。
		info, cached := req.Cache.Get(src.Path, src.Bytes, src.ModTime)
		var perr error
		if !cached {
			info, perr = runner.Probe(ctx, src.Path)
			res.Probed++
			if req.OnProbe != nil {
				req.OnProbe()
			}
			if perr == nil {
				req.Cache.Put(src.Path, src.Bytes, src.ModTime, info)
			}
		}
		if perr != nil {
			res.Rows = append(res.Rows, Plan{
				Name: src.Name, Path: src.Path, RelPath: planRelPath(req.Recursive, rel),
				OutName:       outName,
				OutPath:       outPath,
				PlaceName:     PlaceName(src.Name, opts.Preset.ID),
				PlacePath:     filepath.Join(rowOutDir, PlaceName(src.Name, opts.Preset.ID)),
				SourceBytes:   src.Bytes,
				PlaceInOutput: true,
				SkipReason:    "读不出视频信息（不是视频或文件损坏）",
			})
			res.Skipped++
			res.PlaceCount++
			continue
		}
		row := PlanOne(src.Name, src.Path, rowOutDir, info, opts, outExists)
		row.RelPath = planRelPath(req.Recursive, rel)
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
	// names 里被拒的（不是视频 / 不存在）：逐条如实标记，绝不静默忽略。
	for _, rj := range res.Rejects {
		res.Rows = append(res.Rows, Plan{
			Name:       rj.Name,
			Path:       filepath.Join(req.Dir, rj.Name),
			SkipReason: "已跳过：" + rj.Reason,
		})
		res.Skipped++
	}
	// 计划表里有、这次扫不到的（文件被删/移走）：补一行并如实说明（没有源可放）。
	// 指纹的键在递归时是相对路径，非递归时就是文件名（与面板 sources 同口径）。
	for key, bytes := range req.Expect {
		if seen[key] {
			continue
		}
		name := filepath.Base(filepath.FromSlash(key))
		rowOutDir := outputDirFor(req.OutDir, filepath.FromSlash(key), req.Recursive)
		res.Rows = append(res.Rows, Plan{
			Name: name, Path: filepath.Join(req.Dir, filepath.FromSlash(key)),
			RelPath:     planRelPath(req.Recursive, key),
			OutName:     OutputName(name, opts.Preset.ID),
			OutPath:     filepath.Join(rowOutDir, OutputName(name, opts.Preset.ID)),
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

// planRelPath 只在递归时把相对路径写进 Plan（非递归为空：响应形状与既有行为一致）。
func planRelPath(recursive bool, rel string) string {
	if recursive {
		return rel
	}
	return ""
}

// outputDirFor 算出某个源文件对应的产物目录：递归时按相对目录分层，
// 非递归恒为 outDir（与既有行为逐字节一致）。
func outputDirFor(outDir, rel string, recursive bool) string {
	if !recursive {
		return outDir
	}
	if d := filepath.Dir(filepath.FromSlash(rel)); d != "." && d != "" {
		return filepath.Join(outDir, d)
	}
	return outDir
}

// matchScannedNames 在**扫描结果**里按文件名匹配"只处理选中的这些"（递归模式）。
//
// NormalizeNames 已拒绝带路径分隔符的名字，所以这里只按文件名比；
// 同名文件散在不同子目录时全部命中（面板会逐条列出相对路径）。
func matchScannedNames(sources []Source, names []string) ([]string, []NameReject) {
	used := map[string]bool{}
	for _, src := range sources {
		for _, n := range names {
			if src.Name == n {
				used[n] = true
			}
		}
	}
	var usable []string
	var rejects []NameReject
	for _, n := range names {
		if used[n] {
			usable = append(usable, n)
			continue
		}
		reason := "含子目录的扫描结果里没有这个视频"
		if !IsVideoName(n) {
			reason = "不是视频文件"
		}
		rejects = append(rejects, NameReject{Name: n, Reason: reason})
	}
	return usable, rejects
}

// expectSkip 用面板计划表的指纹核对这一次探测结果；空串 = 一致（照压）。
//
// 用户点名：两次探测之间变了/读不到了要**如实跳过并说明**，不许静默按旧计划压。
func expectSkip(expect map[string]int64, row Plan) string {
	if len(expect) == 0 {
		return ""
	}
	key := row.RelPath
	if strings.TrimSpace(key) == "" {
		key = row.Name
	}
	want, ok := expect[key]
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
	// EnvNotes 返回任务期如实观察到的环境提示（nil = 不检测，单测默认）。
	// 只写日志：绝不改参数、绝不动别人的进程。
	EnvNotes func() []string
	// Stall 是"卡死看门狗"的参数（零值 = 默认 3 分钟警告 / 10 分钟中止）。
	// 门禁注入假时钟与毫秒级阈值，不必真等 10 分钟。
	Stall StallPolicy
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
	// EncoderFallback 表示这个文件硬件编码失败后**回退软件编码**才成功
	// （结果与日志都必须如实标出来，绝不假装没发生）。
	EncoderFallback bool `json:"encoder_fallback,omitempty"`
	// DurationSec/DurationText 是**这个文件**的转码耗时（用户点名：明细行能看到）。
	DurationSec  float64 `json:"duration_sec,omitempty"`
	DurationText string  `json:"duration_text,omitempty"`
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
	SummaryText string `json:"summary_text,omitempty"`
	// DurationSec/DurationText 是**本次任务**的总耗时（进度窗与完成 toast 都显示）。
	DurationSec  float64   `json:"duration_sec,omitempty"`
	DurationText string    `json:"duration_text,omitempty"`
	Items        []RunItem `json:"items"`
}

// RunPlan 顺序执行计划里的每个文件（一个任务压完整个目录）。
//
// 顺序而不是并发：libx264 veryfast 本身就吃满多核，并发只会互相抢 CPU
// 且让"第几个"的进度彻底失序。
func RunPlan(ctx context.Context, outDir string, rows []Plan, runner Runner, hooks Hooks) (*RunResult, error) {
	startedAt := time.Now()
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

	// 环境提示：开始时查一次，之后每 envCheckEvery 个文件复查一次（Jellyfin 之类
	// 可能中途才开始抢 CPU），同一条只打印一次，绝不刷屏。
	seenNotes := map[string]bool{}
	checkEnv := func() {
		if hooks.EnvNotes == nil {
			return
		}
		for _, n := range hooks.EnvNotes() {
			s := strings.TrimSpace(n)
			if s == "" || seenNotes[s] {
				continue
			}
			seenNotes[s] = true
			hooks.log(tasks.LevelWarn, "⚠ "+s)
		}
	}
	checkEnv()

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
		if i > 0 && i%envCheckEvery == 0 {
			checkEnv()
		}
		item := RunItem{Index: i, Name: row.Name, RelPath: relName(row), Before: row.SourceBytes}
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
				if placeInOutput(row, &item, hooks, outDir) {
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
			hooks.log(tasks.LevelWarn, "↷ "+row.DisplayName()+"："+row.SkipReason)
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
			hooks.log(tasks.LevelWarn, "↷ "+row.DisplayName()+"：产物已存在，跳过")
			continue
		}

		// 递归产物按源目录结构分层：先把这一层的目录建好（并交还属主）。
		if derr := ensureDir(filepath.Dir(row.OutPath), outDir, hooks.Chown); derr != nil {
			item.Error = "创建产物目录失败：" + oneLine(derr.Error())
			res.Failed++
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelErr, "✗ "+row.DisplayName()+"："+item.Error)
			continue
		}

		modeTag := fmt.Sprintf("%d kbps", row.VideoKbps)
		if row.Mode == ModeQuality {
			// CRF（越小越好）与 -q:v（越大越好）不是同一把尺子，日志分开写名称。
			if row.Encoder == EncoderCPU {
				modeTag = fmt.Sprintf("CRF %d（上限 %d kbps）", row.Quality, row.MaxRateKbps)
			} else if row.EstKbps > 0 {
				// 硬件无真 CRF：如实写"有上限质量模式"与按标定算出的实际码率。
				modeTag = fmt.Sprintf("质量档 %d（有上限 %d kbps，实际≈%d kbps）",
					row.Quality, row.MaxRateKbps, row.EstKbps)
			} else {
				modeTag = fmt.Sprintf("质量档 %d（上限 %d kbps）", row.Quality, row.MaxRateKbps)
			}
		}
		passTag := ""
		if row.TwoPass {
			passTag = " · 2-pass"
		}
		// 硬件档的两种形态都要在日志里说清楚（用户点名"不支持时如实回退 + 日志写明"）。
		pipeTag := ""
		if row.Encoder == EncoderHardware {
			if row.FastPipeline {
				pipeTag = " · 全 GPU（硬解+GPU 缩放）"
			} else {
				pipeTag = " · 软解 + 硬编"
			}
			if row.CapFPS30 {
				pipeTag += fmt.Sprintf(" · 降到 %dfps", maxOutputFPS)
			}
		}
		hooks.log(tasks.LevelStep, fmt.Sprintf("▶ 第 %d/%d 个：%s（%dx%d → %dx%d，%s %s%s%s）",
			i+1, total, row.DisplayName(), row.SourceWidth, row.SourceHeight,
			row.TargetWidth, row.TargetHeight, EncoderCodec(row.Encoder), modeTag, pipeTag, passTag))
		if row.Capped && row.Note != "" {
			hooks.log(tasks.LevelWarn, row.Note)
		}
		if row.Aligned && row.AlignFrom != "" {
			// 对齐只改编码尺寸（比例算出来的是 AlignFrom）：必须说出来，
			// 别让用户以为分辨率写错了。
			hooks.log(tasks.LevelStep, fmt.Sprintf("尺寸按硬件编码器要求对齐：%s → %dx%d",
				row.AlignFrom, row.TargetWidth, row.TargetHeight))
		}

		srcBytes := row.SourceBytes
		if st, err := os.Stat(row.Path); err == nil {
			srcBytes = st.Size()
		}
		part := row.OutPath + PartSuffix
		_ = os.Remove(part)

		passLog := ""
		if row.TwoPass && passDir != "" {
			passLog = filepath.Join(passDir, fmt.Sprintf("pass%d", i))
		}
		itemStart := time.Now()
		req := TranscodeRequest{
			Src: row.Path, Dst: part,
			Width: row.TargetWidth, Height: row.TargetHeight,
			VideoKbps: row.VideoKbps, AudioKbps: row.AudioKbps,
			DurationSec: row.DurationSec,
			Encoder:     row.Encoder, Mode: row.Mode, Quality: row.Quality,
			TwoPass: row.TwoPass, PassLog: passLog,
			Fast: row.FastPipeline, SourceFPS: row.SourceFPS, SrcBitDepth: row.SourceBitDepth,
		}
		// runTranscode 跑一遍（自动挂看门狗）：stalled=true 表示被看门狗中止。
		runTranscode := func(rreq TranscodeRequest) (err error, stalled bool) {
			throttle := &progressThrottle{}
			rctx, cancel := context.WithCancel(ctx)
			w := startStallWatch(hooks.Stall, row.DisplayName(), rreq.Encoder, cancel, hooks.log)
			err = runner.Transcode(rctx, rreq, func(p Progress) {
				// 先喂看门狗再限流：限流只影响日志行数，绝不影响"有没有进度"的判据。
				w.nudge()
				if p.Pass == 1 {
					// 2-pass 第一遍只做分析：喂看门狗就够了，不打印（没有出片进度可显示）。
					return
				}
				okLine, heartbeat := throttle.allow(p.Percent)
				if !okLine {
					return
				}
				hooks.log(tasks.LevelOut, progressLine(p, heartbeat))
			})
			reason := w.stop()
			cancel()
			if w.killed() {
				return fmt.Errorf("%s", reason), true
			}
			return err, false
		}
		rerr, stalled := runTranscode(req)
		// 全 GPU 快路跑不通（老 ffmpeg 没有 scale_vt / 这个源的硬解或位深不兼容…）：
		// 如实说明原因，回退**软件解码 + 软件缩放**再跑一遍（硬件编码不变）。
		if rerr != nil && req.Fast && ctx.Err() == nil && !stalled {
			hooks.log(tasks.LevelWarn, "⚠ 全 GPU 加速不可用，已回退软件解码/缩放重试："+
				row.DisplayName()+"（"+oneLine(rerr.Error())+"）")
			_ = os.Remove(part)
			rerr, _ = runTranscode(safeFallback(req))
		}
		// 硬件编码失败：如实回退一次软件编码（结果里标出来），绝不整批跟着挂。
		// 看门狗中止的不在此列：那种情况按用户口径记失败并继续下一个文件。
		if rerr != nil && row.Encoder == EncoderHardware && ctx.Err() == nil && !stalled {
			hooks.log(tasks.LevelWarn, "⚠ 硬件编码失败，已回退软件编码重试："+row.DisplayName()+"（"+oneLine(rerr.Error())+"）")
			_ = os.Remove(part)
			if ferr, _ := runTranscode(cpuFallback(req)); ferr == nil {
				rerr, item.EncoderFallback = nil, true
			} else {
				rerr = ferr
			}
		}
		if rerr != nil {
			_ = os.Remove(part)
			if ctx.Err() != nil {
				hooks.log(tasks.LevelWarn, "已中断，删掉没写完的半成品："+row.OutName)
				fillDuration(res, startedAt)
				return res, cancelledErr(res)
			}
			item.Error = rerr.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelErr, "✗ "+row.DisplayName()+"："+rerr.Error())
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
			if placeInOutput(row, &item, hooks, outDir) {
				res.Placed++
			}
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelWarn, fmt.Sprintf("↷ %s：压不小，已跳过（%s ≥ 原 %s）",
				row.DisplayName(), humanBytes(afterBytes), humanBytes(srcBytes)))
			continue
		}

		if err := os.Rename(part, row.OutPath); err != nil {
			_ = os.Remove(part)
			item.Error = "写出产物失败：" + err.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelErr, "✗ "+row.DisplayName()+"："+item.Error)
			continue
		}
		if hooks.Chown != nil {
			hooks.Chown(row.OutPath)
		}
		item.OutName, item.OutPath = row.OutName, row.OutPath
		item.SavedPercentText = FormatSavedPercent(srcBytes, afterBytes)
		// 单文件耗时（顺手给，不是必须的判据）：明细日志行与结果 JSON 都带上。
		item.DurationSec = time.Since(itemStart).Seconds()
		item.DurationText = FormatDuration(time.Since(itemStart))
		res.Done++
		res.BeforeBytes += srcBytes
		res.AfterBytes += afterBytes
		res.SavedBytes += srcBytes - afterBytes
		res.Items = append(res.Items, item)
		// 明细行同时给体积与百分比（算不出百分比时只写体积，绝不写 NaN%）。
		used := " · 用时 " + item.DurationText
		if item.EncoderFallback {
			used += " · 已回退软件编码"
		}
		if item.SavedPercentText != "" {
			hooks.log(tasks.LevelOK, fmt.Sprintf("✓ %s：%s → %s（省 %s，%s%s）",
				row.DisplayName(), humanBytes(srcBytes), humanBytes(afterBytes),
				humanBytes(srcBytes-afterBytes), item.SavedPercentText, used))
		} else {
			hooks.log(tasks.LevelOK, fmt.Sprintf("✓ %s：%s → %s（省 %s%s）",
				row.DisplayName(), humanBytes(srcBytes), humanBytes(afterBytes),
				humanBytes(srcBytes-afterBytes), used))
		}
	}

	if err := ctx.Err(); err != nil {
		fillDuration(res, startedAt)
		return res, cancelledErr(res)
	}
	res.SavedPercentText = FormatSavedPercent(res.BeforeBytes, res.AfterBytes)
	fillDuration(res, startedAt)
	res.SummaryText = SummaryRunText(res)
	hooks.log(tasks.LevelStep, "完成："+res.SummaryText+" · "+res.DurationText)
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
//
// 递归时用 Plan.RelPath（含子目录）；非递归为空时回落到"相对源目录"== 文件名，
// 与既有清单输出逐字节一致。
func relName(row Plan) string {
	if strings.TrimSpace(row.RelPath) != "" {
		return row.RelPath
	}
	if rel, err := filepath.Rel(filepath.Dir(row.Path), row.Path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return row.Name
}

// ensureDir 建出产物目录（递归产物按源目录结构分层），并把新建的每一层交还属主 ——
// 面板以 root 跑，目录留给 root 的话用户在 Finder 里写不进去。
//
// root 是这次任务的 output 根：只在它之内逐层 chown，绝不往上 chown 到用户目录。
func ensureDir(dir, root string, chown func(string)) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if chown == nil {
		return nil
	}
	root = filepath.Clean(root)
	for p := filepath.Clean(dir); ; {
		chown(p)
		if root == "" || root == "." || p == root {
			return nil
		}
		parent := filepath.Dir(p)
		if parent == p || (parent != root && !strings.HasPrefix(parent, root+string(filepath.Separator))) {
			return nil
		}
		p = parent
	}
}

// placeInOutput 把跳过的源文件原样放进 output/，并把放法与原因写进 item。
//
// 硬链接共享 inode ⇒ **绝不 chown**（那会改到源文件本身的属主）；只有复制才交还属主。
// 目录是另说的：递归产物的子目录由面板（root）新建，必须交还属主。
func placeInOutput(row Plan, item *RunItem, hooks Hooks, root string) bool {
	dst := row.PlacePath
	if strings.TrimSpace(dst) == "" {
		if strings.TrimSpace(row.PlaceName) == "" {
			item.Placement = PlacementNone
			item.PlaceReason = "缺少源文件或目标路径"
			hooks.log(tasks.LevelWarn, "↷ "+row.DisplayName()+"：未放入 output（"+item.PlaceReason+"）")
			return false
		}
		dst = filepath.Join(filepath.Dir(row.OutPath), row.PlaceName)
	}
	if derr := ensureDir(filepath.Dir(dst), root, hooks.Chown); derr != nil {
		item.Placement = PlacementNone
		item.PlaceReason = "创建 output 目录失败：" + oneLine(derr.Error())
		hooks.log(tasks.LevelWarn, "↷ "+row.DisplayName()+"：未放入 output（"+item.PlaceReason+"）")
		return false
	}
	pr := placeFile(row.Path, dst)
	item.Placement, item.PlaceReason, item.PlaceName = pr.Placement, pr.Reason, filepath.Base(dst)
	switch pr.Placement {
	case PlacementLink:
		hooks.log(tasks.LevelOK, "🔗 "+row.DisplayName()+"：原样放入 output（硬链接，不占额外空间）")
		return true
	case PlacementCopy:
		if hooks.Chown != nil {
			hooks.Chown(dst)
		}
		hooks.log(tasks.LevelOK, "📄 "+row.DisplayName()+"：原样放入 output（"+pr.Reason+"）")
		return true
	default:
		hooks.log(tasks.LevelWarn, "↷ "+row.DisplayName()+"：未放入 output（"+pr.Reason+"）")
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

// FormatDuration 把耗时写成给用户看的短句（<1 秒不谎报成 0 秒）。
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return "<1 秒"
	}
	sec := int(d.Round(time.Second) / time.Second)
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
	default:
		return fmt.Sprintf("%d 小时 %d 分", sec/3600, (sec%3600)/60)
	}
}

// SummaryDurationText 是"本次转码用时 X（N 个文件）"那一句（进度窗与 toast 共用）。
func SummaryDurationText(d time.Duration, done int) string {
	return fmt.Sprintf("本次转码用时 %s（%d 个文件）", FormatDuration(d), done)
}

// fillDuration 把总耗时写进结果（成功/中断两条路径都调它，绝不漏）。
func fillDuration(res *RunResult, startedAt time.Time) {
	if res == nil {
		return
	}
	d := time.Since(startedAt)
	res.DurationSec = d.Seconds()
	res.DurationText = SummaryDurationText(d, res.Done)
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

// progressHeartbeatEvery 是"到行数上限之后"的心跳间隔。
//
// 为什么必须有：以前到 20 行就**永久静默**，46 分钟的长片每 1% 约 10 秒 ⇒
// 日志正好停在 20% 不动，用户只能看到"卡在 20%"（2026-10-05 报障）。
// 现在改成每 60 秒最多一条，既不刷屏，也绝不无声。
const progressHeartbeatEvery = 60 * time.Second

// envCheckEvery 是"每处理多少个文件复查一次环境提示"（16578 行的长任务也要能中途发现被抢 CPU）。
const envCheckEvery = 20

// progressThrottle 限流进度行：每 10% 或每 5 秒一条，且单文件最多 20 条；
// 到上限后降级成每 progressHeartbeatEvery 一条心跳（绝不彻底静默）。
type progressThrottle struct {
	lastPct int
	lastAt  time.Time
	n       int
	// now 是取当前时间的函数（nil = time.Now）；门禁注入假时钟，不必真等 60 秒。
	now func() time.Time
}

func (t *progressThrottle) nowFn() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// allow 返回 (要不要打印, 是不是心跳行)。
//
// 心跳行的语义：进度行已经到上限，但 ffmpeg 仍在编码（帧数在涨）——
// 必须让用户看得到"还在跑"，否则就是"卡在 20%"那个报障。
func (t *progressThrottle) allow(pct float64) (bool, bool) {
	i := int(pct)
	if t.n >= maxProgressLines {
		if t.nowFn().Sub(t.lastAt) < progressHeartbeatEvery {
			return false, false
		}
		t.lastPct, t.lastAt, t.n = i, t.nowFn(), t.n+1
		return true, true
	}
	if i <= t.lastPct {
		return false, false
	}
	if t.lastPct > 0 && i-t.lastPct < 10 && t.nowFn().Sub(t.lastAt) < 5*time.Second {
		return false, false
	}
	t.lastPct = i
	t.lastAt = t.nowFn()
	t.n++
	return true, false
}

// progressLine 渲染一条进度日志行；心跳行必须带上帧数（读得到时）——
// "帧数在涨"是"编码没卡死"的唯一直接证据。
func progressLine(p Progress, heartbeat bool) string {
	if heartbeat {
		if p.Frame > 0 {
			return fmt.Sprintf("编码仍在进行（帧数 %d，%d%%）", p.Frame, int(p.Percent))
		}
		return fmt.Sprintf("编码仍在进行（%d%%）", int(p.Percent))
	}
	line := fmt.Sprintf("%d%%", int(p.Percent))
	if strings.TrimSpace(p.Speed) != "" {
		line += "（" + p.Speed + "）"
	}
	return line
}

// ----------------------------------------------------------------------------
//  卡死看门狗
// ----------------------------------------------------------------------------

// StallPolicy 是"进度长时间不推进"的判据（零值 = 默认：3 分钟警告、10 分钟中止）。
//
// 为什么需要（2026-10-05 用户报障）：硬件编码器可能停在某一帧不再吐进度，
// 任务会一直挂着不动、批次后面的文件也不跑。参数可注入：门禁用假时钟 +
// 毫秒级阈值断言，不必真等 10 分钟。
type StallPolicy struct {
	WarnAfter time.Duration // 无进度多久先警告一次
	KillAfter time.Duration // 无进度累计多久终止**这一个文件**
	Tick      time.Duration // 检查间隔
	// Now 是取当前时间的函数（nil = time.Now）；门禁注入假时钟。
	Now func() time.Time
}

const (
	// DefaultStallWarn / DefaultStallKill 是生产默认阈值（用户拍板）。
	DefaultStallWarn = 3 * time.Minute
	DefaultStallKill = 10 * time.Minute
	defaultStallTick = 5 * time.Second
)

func (p StallPolicy) withDefaults() StallPolicy {
	if p.WarnAfter <= 0 {
		p.WarnAfter = DefaultStallWarn
	}
	if p.KillAfter <= 0 {
		p.KillAfter = DefaultStallKill
	}
	if p.Tick <= 0 {
		p.Tick = defaultStallTick
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	return p
}

// stallWatch 盯着"最后一次进度"的时刻：超 warn 档警告一次，超 kill 档
// 中止**当前文件的 ffmpeg**（cancel 只取消这一个文件的上下文，批次照旧往下走）。
type stallWatch struct {
	policy  StallPolicy
	name    string
	encoder string
	log     func(level, msg string)
	cancel  context.CancelFunc

	mu     sync.Mutex
	last   time.Time
	warned bool
	dead   bool
	reason string

	done chan struct{}
	once sync.Once
}

func startStallWatch(p StallPolicy, name, encoder string, cancel context.CancelFunc, log func(level, msg string)) *stallWatch {
	p = p.withDefaults()
	w := &stallWatch{
		policy: p, name: name, encoder: encoder, log: log, cancel: cancel,
		last: p.Now(), done: make(chan struct{}),
	}
	go w.loop()
	return w
}

// nudge 记下"进度又动了"（每次进度回调都调，不受日志限流影响）。
func (w *stallWatch) nudge() {
	w.mu.Lock()
	w.last = w.policy.Now()
	w.mu.Unlock()
}

func (w *stallWatch) loop() {
	t := time.NewTicker(w.policy.Tick)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
			if w.check() {
				return
			}
		}
	}
}

// check 返回 true 表示已经中止（看门狗可以退出）。
func (w *stallWatch) check() bool {
	w.mu.Lock()
	idle := w.policy.Now().Sub(w.last)
	dead, warned := w.dead, w.warned
	w.mu.Unlock()
	if dead {
		return true
	}
	if idle >= w.policy.KillAfter {
		w.mu.Lock()
		w.dead = true
		w.reason = w.killReason()
		w.mu.Unlock()
		if w.cancel != nil {
			w.cancel()
		}
		return true
	}
	if idle >= w.policy.WarnAfter && !warned {
		w.mu.Lock()
		w.warned = true
		w.mu.Unlock()
		if w.log != nil {
			w.log(tasks.LevelWarn, fmt.Sprintf("⚠ %s：已 %s 无进度，可能在等硬件编码器",
				w.name, shortWait(w.policy.WarnAfter)))
		}
	}
	return false
}

// killReason 是中止原因（写进任务日志与结果，指明下一步怎么办）。
func (w *stallWatch) killReason() string {
	wait := shortWait(w.policy.KillAfter)
	if ResolveEncoder(w.encoder) == EncoderHardware {
		return fmt.Sprintf("疑似硬件编码器卡死（已 %s 无进度），已中止；可改用 CPU 编码重试", wait)
	}
	return fmt.Sprintf("疑似编码器卡死（已 %s 无进度），已中止", wait)
}

// stop 停掉看门狗并返回"被中止的原因"（空串 = 没被中止）。
func (w *stallWatch) stop() string {
	w.once.Do(func() { close(w.done) })
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

func (w *stallWatch) killed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dead
}

// shortWait 把阈值写成给用户看的短句（门禁注入毫秒级阈值时也能读）。
func shortWait(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	case d >= time.Second:
		return fmt.Sprintf("%d 秒", int(d/time.Second))
	default:
		return fmt.Sprintf("%d 毫秒", int(d/time.Millisecond))
	}
}

// safeFallback 把"全 GPU 快路"降级成"软件解码 + 软件缩放 + 硬件编码"：
// 只有解码/缩放变回软件，编码器与所有码率/质量参数一个不动。
func safeFallback(req TranscodeRequest) TranscodeRequest {
	out := req
	out.Fast = false
	return out
}

// cpuFallback 把一次硬件转码请求改成 CPU（libx264）等价请求：
// 码率/尺寸一个不动，只有质量优先模式要把 -q:v 折算成 -crf。
// Fast 必须清掉：CPU 档的解码/缩放一律走软件（-hwaccel/scale_vt 是硬件档专属）。
func cpuFallback(req TranscodeRequest) TranscodeRequest {
	out := req
	out.Encoder = EncoderCPU
	out.Fast = false
	if ResolveMode(req.Mode) == ModeQuality {
		out.Quality = HardwareQualityToCRF(req.Quality)
	}
	return out
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

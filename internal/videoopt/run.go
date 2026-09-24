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
	// VideoKbps 是实际视频码率；AudioKbps<=0 表示产物不要音轨（-an）。
	VideoKbps   int
	AudioKbps   int
	DurationSec float64
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
func (r *FFmpegRunner) Transcode(ctx context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	if r == nil || r.Ffmpeg == "" {
		return ErrEngineMissing
	}
	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", req.Src,
		"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "main",
		"-pix_fmt", "yuv420p",
		"-b:v", fmt.Sprintf("%dk", req.VideoKbps),
		"-maxrate", fmt.Sprintf("%dk", req.VideoKbps),
		"-bufsize", fmt.Sprintf("%dk", req.VideoKbps*2),
		"-vf", fmt.Sprintf("scale=%d:%d", req.Width, req.Height),
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

	cmd := exec.CommandContext(ctx, r.Ffmpeg, args...)
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

// BuildPlan 扫目录并逐条规划 —— **/video-plan 与 /video-compress 共用这一份**。
//
// runner.Probe 会碰真实文件（ffprobe），但所有决策都在纯函数里（PlanOne）。
func BuildPlan(ctx context.Context, req PlanRequest, runner Runner) (PlanResult, error) {
	if strings.TrimSpace(req.Dir) == "" {
		return PlanResult{}, fmt.Errorf("缺少目录")
	}
	if strings.TrimSpace(req.OutDir) == "" {
		req.OutDir = filepath.Join(req.Dir, OutputDirName)
	}
	kbps := ResolveKBps(req.Preset, req.KBps)
	res := PlanResult{
		Dir: req.Dir, OutDir: req.OutDir, Preset: req.Preset, KBps: kbps,
		Rows: []Plan{},
	}
	sources, err := Scan(req.Dir)
	if err != nil {
		return res, fmt.Errorf("读取目录失败: %w", err)
	}
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		outPath := filepath.Join(req.OutDir, OutputName(src.Name, req.Preset.ID))
		_, statErr := os.Stat(outPath)
		outExists := statErr == nil
		info, perr := runner.Probe(ctx, src.Path)
		if perr != nil {
			res.Rows = append(res.Rows, Plan{
				Name: src.Name, Path: src.Path,
				OutName: OutputName(src.Name, req.Preset.ID), OutPath: outPath,
				SourceBytes: src.Bytes,
				SkipReason:  "读不出视频信息（不是视频或文件损坏）",
			})
			res.Skipped++
			continue
		}
		row := PlanOne(src.Name, src.Path, req.OutDir, info, req.Preset, kbps, outExists)
		if row.Runnable() {
			res.Runnable++
			res.EstBytes += row.EstBytes
		} else {
			res.Skipped++
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// ----------------------------------------------------------------------------
//  执行计划
// ----------------------------------------------------------------------------

// Hooks 是执行期的外部依赖：日志（任务中心）与产物归属。
type Hooks struct {
	Log   tasks.LogFunc
	Chown func(path string)
}

func (h Hooks) log(level, msg string) {
	if h.Log != nil {
		h.Log(level, msg)
	}
}

// RunItem 是单个文件的执行结果（任务结果 JSON，前端据此汇总）。
type RunItem struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	OutName    string `json:"out_name,omitempty"`
	OutPath    string `json:"out_path,omitempty"`
	Before     int64  `json:"before"`
	After      int64  `json:"after,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	VideoKbps  int    `json:"video_kbps,omitempty"`
	Capped     bool   `json:"capped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

// RunResult 是一次批量压缩的汇总。
type RunResult struct {
	Total       int       `json:"total"`
	Done        int       `json:"done"`
	Skipped     int       `json:"skipped"`
	Failed      int       `json:"failed"`
	BeforeBytes int64     `json:"before_bytes"`
	AfterBytes  int64     `json:"after_bytes"`
	SavedBytes  int64     `json:"saved_bytes"`
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

	total := len(rows)
	for i, row := range rows {
		item := RunItem{Index: i, Name: row.Name, Before: row.SourceBytes}
		if err := ctx.Err(); err != nil {
			return res, cancelledErr(res)
		}
		if row.SkipReason != "" {
			item.SkipReason = row.SkipReason
			res.Skipped++
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelWarn, "↷ "+row.Name+"："+row.SkipReason)
			continue
		}
		// 幂等：产物已存在就不再压（计划里可能还是"可压"，执行时再确认一次）。
		if _, err := os.Stat(row.OutPath); err == nil {
			item.SkipReason = "产物已存在，跳过"
			res.Skipped++
			res.Items = append(res.Items, item)
			hooks.log(tasks.LevelWarn, "↷ "+row.Name+"：产物已存在，跳过")
			continue
		}

		hooks.log(tasks.LevelStep, fmt.Sprintf("▶ 第 %d/%d 个：%s（%dx%d → %dx%d，%d kbps）",
			i+1, total, row.Name, row.SourceWidth, row.SourceHeight,
			row.TargetWidth, row.TargetHeight, row.VideoKbps))
		if row.Capped && row.Note != "" {
			hooks.log(tasks.LevelWarn, row.Note)
		}

		srcBytes := row.SourceBytes
		if st, err := os.Stat(row.Path); err == nil {
			srcBytes = st.Size()
		}
		part := row.OutPath + ".part.mp4"
		_ = os.Remove(part)

		throttle := &progressThrottle{}
		rerr := runner.Transcode(ctx, TranscodeRequest{
			Src: row.Path, Dst: part,
			Width: row.TargetWidth, Height: row.TargetHeight,
			VideoKbps: row.VideoKbps, AudioKbps: row.AudioKbps,
			DurationSec: row.DurationSec,
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

		// 硬要求：产物绝不比原文件大。大了就删掉并如实跳过（绝不留下更大的文件）。
		if afterBytes <= 0 || afterBytes >= srcBytes {
			_ = os.Remove(part)
			item.After = 0
			item.SkipReason = "压不小，已跳过（产物不小于原文件）"
			res.Skipped++
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
		res.Done++
		res.BeforeBytes += srcBytes
		res.AfterBytes += afterBytes
		res.SavedBytes += srcBytes - afterBytes
		res.Items = append(res.Items, item)
		hooks.log(tasks.LevelOK, fmt.Sprintf("✓ %s：%s → %s（省 %s）",
			row.Name, humanBytes(srcBytes), humanBytes(afterBytes), humanBytes(srcBytes-afterBytes)))
	}

	if err := ctx.Err(); err != nil {
		return res, cancelledErr(res)
	}
	hooks.log(tasks.LevelStep, fmt.Sprintf("完成：成功 %d，跳过 %d，失败 %d；共省 %s",
		res.Done, res.Skipped, res.Failed, humanBytes(res.SavedBytes)))
	if res.Failed == res.Total && res.Total > 0 {
		return res, fmt.Errorf("全部 %d 个都失败了，第一个的原因见日志", res.Failed)
	}
	return res, nil
}

// cancelledErr 是中断时的错误：任务中心会据此如实标「已中断」。
func cancelledErr(res *RunResult) error {
	return fmt.Errorf("任务被取消（已成功 %d，跳过 %d，失败 %d，共 %d 个）",
		res.Done, res.Skipped, res.Failed, res.Total)
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

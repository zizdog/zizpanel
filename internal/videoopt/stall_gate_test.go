package videoopt

// stall_gate_test.go —— 转码"卡死看门狗"+ 尺寸对齐 + 编码器回退的唯一门禁。
//
// 为什么现有门禁抓不到（2026-10-05 用户报障，mini 转码到 20% 日志不动）：
// ① 进度行到 maxProgressLines 就永久静默 ⇒ 长片看起来"卡在 20%"；
// ② 硬件编码器真的不吐进度时没有任何超时，任务会挂在同一个文件上，
//    批次后面的文件也跟着不跑；③ 目标尺寸 1406 不是 16 的倍数，
//    硬件编码器对此敏感却没有对齐与说明；④ 硬件编码失败整批跟着失败。
// 全用假 Runner + 假时钟（t.TempDir()），一次 ffmpeg 都不跑。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// fakeClock 是门禁的假时钟：看门狗读它，测试自己推进它。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// logSpy 收任务日志（看门狗与 RunPlan 都写它），并发安全。
type logSpy struct {
	mu   sync.Mutex
	rows []string
}

func (l *logSpy) log(level, text string) {
	l.mu.Lock()
	l.rows = append(l.rows, level+"|"+text)
	l.mu.Unlock()
}

func (l *logSpy) outCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.rows {
		if strings.HasPrefix(r, tasks.LevelOut+"|") {
			n++
		}
	}
	return n
}

func (l *logSpy) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.rows {
		if strings.HasPrefix(r, tasks.LevelWarn+"|") {
			n++
		}
	}
	return n
}

// waitWarn 等第一条警告出现（看门狗在自己的 goroutine 里写日志）。
func (l *logSpy) waitWarn(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if l.warnCount() > 0 {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return l.warnCount() > 0
}

// mkRow 造一行**可直接执行**的计划（RunPlan 不探测，门禁不必碰 ffprobe）。
func mkRow(t *testing.T, dir string, i int, srcBytes int64) Plan {
	t.Helper()
	src := filepath.Join(dir, fmt.Sprintf("src%d.mp4", i))
	if err := os.WriteFile(src, make([]byte, srcBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	return Plan{
		Name: filepath.Base(src), Path: src,
		OutName:     fmt.Sprintf("src%d.1080p.mp4", i),
		OutPath:     filepath.Join(dir, "output", fmt.Sprintf("src%d.1080p.mp4", i)),
		PlaceName:   fmt.Sprintf("src%d.1080p.mp4", i),
		PlacePath:   filepath.Join(dir, "output", fmt.Sprintf("src%d.1080p.mp4", i)),
		SourceBytes: srcBytes,
		DurationSec: 100,
		SourceWidth: 2812, SourceHeight: 2160, SourceVideoKbps: 7000,
		TargetWidth: 1408, TargetHeight: 1080,
		VideoKbps: 6868, MaxRateKbps: 6868, AudioKbps: 96,
		Encoder: EncoderHardware, Mode: ModeQuality, Quality: 50,
	}
}

// silentRunner：第一次调用只报一次进度就彻底静默（模拟硬件编码器卡死），
// 之后每次调用都正常写出比源小的产物。
type silentRunner struct {
	mu    sync.Mutex
	reqs  []TranscodeRequest
	calls int
}

func (r *silentRunner) Available() error               { return nil }
func (r *silentRunner) Version(context.Context) string { return "fake" }
func (r *silentRunner) Probe(context.Context, string) (MediaInfo, error) {
	return MediaInfo{}, nil
}

func (r *silentRunner) Transcode(ctx context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	if n == 1 {
		if onProgress != nil {
			onProgress(Progress{Percent: 1, Speed: "2.7x"})
		}
		<-ctx.Done() // 卡死：不再有任何进度
		return ctx.Err()
	}
	if err := os.MkdirAll(filepath.Dir(req.Dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(req.Dst, []byte("small"), 0o644)
}

func (r *silentRunner) callsCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestVideoProgressHeartbeatGate ⑥：进度日志**永不永久静默**。
//
// 这是“看起来卡在 20%”的直接成因：以前单文件 20 行封顶后就再也不打印，
// 46 分钟的长片每 1% 约 10 秒 ⇒ 日志正好停在 20%。现在到顶后每 60 秒一条心跳。
func TestVideoProgressHeartbeatGate(t *testing.T) {
	clock := newFakeClock()
	th := &progressThrottle{now: clock.Now}
	lines, beats := 0, 0
	// 模拟 46 分钟长片：每 1% 推进 10 秒（≈2.7x 速度），走完 100%。
	for pct := 1; pct <= 100; pct++ {
		clock.Advance(10 * time.Second)
		okLine, beat := th.allow(float64(pct))
		if okLine {
			lines++
		}
		if beat {
			beats++
		}
	}
	if beats == 0 {
		t.Fatal("到上限后没有走心跳分支（帧数就没机会显示出来）")
	}
	// 心跳行必须带上帧数：这是"编码仍在进行"的直接证据（用户点名）。
	if line := progressLine(Progress{Percent: 42, Frame: 12345}, true); !strings.Contains(line, "12345") {
		t.Fatalf("心跳行没有如实显示帧数：%q", line)
	}
	// 复现"卡在 20%"：2.7x（mini 上两个任务并发时的实测速度）⇒ 每 1% 要 10 秒，
	// 节流的 5 秒规则拦不住 ⇒ 第 20 行（行数上限）正好落在 20%。
	clock2 := newFakeClock()
	th2 := &progressThrottle{now: clock2.Now}
	n, at20 := 0, 0
	for pct := 1; pct <= 40; pct++ {
		clock2.Advance(10 * time.Second)
		if okLine, _ := th2.allow(float64(pct)); okLine {
			n++
			if n == maxProgressLines {
				at20 = pct
			}
		}
	}
	if at20 != 20 {
		t.Fatalf("行数上限应当正好落在 20%%（用户报障的现象），实际落在 %d%%", at20)
	}
	if lines <= maxProgressLines {
		t.Fatalf("进度日志到 %d 行后永久静默（用户看到的就是“卡在 20%%”）：%d 行", maxProgressLines, lines)
	}
	if lines > 2*maxProgressLines {
		t.Fatalf("心跳太密（%d 行 > %d）：会刷屏", lines, 2*maxProgressLines)
	}
}

// TestVideoStallWatchdogGate ①②：假时钟推进 ⇒ 先警告、再中止该文件
// （源文件不变）⇒ 批次继续跑下一个文件。
func TestVideoStallWatchdogGate(t *testing.T) {
	dir := t.TempDir()
	rows := []Plan{mkRow(t, dir, 1, 4096), mkRow(t, dir, 2, 4096)}
	src1 := rows[0].Path
	before, err := os.Stat(src1)
	if err != nil {
		t.Fatal(err)
	}

	clock := newFakeClock()
	spy := &logSpy{}
	runner := &silentRunner{}
	type outcome struct {
		res  *RunResult
		rerr error
	}
	done := make(chan outcome, 1)
	go func() {
		res, rerr := RunPlan(context.Background(), filepath.Join(dir, "output"), rows, runner, Hooks{
			Log: spy.log,
			Stall: StallPolicy{
				WarnAfter: 3 * time.Minute, KillAfter: 10 * time.Minute,
				Tick: 2 * time.Millisecond, Now: clock.Now,
			},
		})
		done <- outcome{res, rerr}
	}()

	// 等到第 1 个文件真的开始转码（卡在那里不吐进度）。
	deadline := time.Now().Add(3 * time.Second)
	for runner.callsCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if runner.callsCount() == 0 {
		t.Fatal("第 1 个文件没有开始转码")
	}
	// ① 只推进到 warn 档（4 分钟 ≥ 3 分钟、< 10 分钟）：必须有警告，且任务还在跑。
	clock.Advance(4 * time.Minute)
	if !spy.waitWarn(2 * time.Second) {
		t.Fatal("超过 warn 阈值后没有警告日志")
	}
	select {
	case <-done:
		t.Fatal("只到 warn 档就结束了（kill 阈值提前生效 / 没有继续等）")
	default:
	}
	// ② 推进到 kill 档（累计 15 分钟 ≥ 10 分钟）：必须中止该文件、批次继续。
	clock.Advance(11 * time.Minute)
	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("超过 kill 阈值后没有中止（看门狗没生效）")
	}
	res := got.res
	if res == nil {
		t.Fatal("RunPlan 返回 nil 结果")
	}
	if res.Failed != 1 || res.Done != 1 {
		t.Fatalf("期望 失败 1 个 / 成功 1 个，实际 失败 %d / 成功 %d（rerr=%v）", res.Failed, res.Done, got.rerr)
	}
	if res.Items[0].Error == "" || !strings.Contains(res.Items[0].Error, "卡死") {
		t.Fatalf("第 1 个文件的失败原因没有指明“卡死”：%q", res.Items[0].Error)
	}
	after, err := os.Stat(src1)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("源文件被动过：%v/%v → %v/%v", before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
	if res.Items[1].SkipReason != "" || res.Items[1].Error != "" {
		t.Fatalf("后续文件没有继续处理：%+v", res.Items[1])
	}
	if runner.callsCount() != 2 {
		t.Fatalf("期望 2 次转码调用（第 1 个卡死 + 第 2 个成功），实际 %d", runner.callsCount())
	}
}

// TestVideoStallWarnThenKillGate ①②：假时钟先推进到 warn 档（必须出现警告），
// 再推进到 kill 档（必须中止）—— 证明"先警告、后中止"两个阈值都真的生效。
func TestVideoStallWarnThenKillGate(t *testing.T) {
	dir := t.TempDir()
	rows := []Plan{mkRow(t, dir, 1, 4096)}

	clock := newFakeClock()
	spy := &logSpy{}
	runner := &blockRunner{reached: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = RunPlan(context.Background(), filepath.Join(dir, "output"), rows, runner, Hooks{
			Log: spy.log,
			Stall: StallPolicy{
				WarnAfter: 3 * time.Minute, KillAfter: 10 * time.Minute,
				Tick: 2 * time.Millisecond, Now: clock.Now,
			},
		})
	}()
	<-runner.reached
	// 推进到 4 分钟（≥ warn、< kill）：必须只有警告，任务还在跑。
	clock.Advance(4 * time.Minute)
	if !spy.waitWarn(2 * time.Second) {
		t.Fatal("超过 warn 阈值后没有警告日志")
	}
	select {
	case <-done:
		t.Fatal("只到 warn 档就被中止了（kill 阈值提前生效）")
	default:
	}
	// 推进到 11 分钟（≥ kill）：必须中止。
	clock.Advance(11 * time.Minute)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("超过 kill 阈值后没有中止（看门狗没生效）")
	}
}

// blockRunner 只报一次进度就阻塞到 ctx 被取消（模拟硬件编码器卡死）。
type blockRunner struct {
	reached chan struct{}
	once    sync.Once
}

func (r *blockRunner) Available() error               { return nil }
func (r *blockRunner) Version(context.Context) string { return "fake" }
func (r *blockRunner) Probe(context.Context, string) (MediaInfo, error) {
	return MediaInfo{}, nil
}

func (r *blockRunner) Transcode(ctx context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	if onProgress != nil {
		onProgress(Progress{Percent: 1, Speed: "2.7x"})
	}
	r.once.Do(func() { close(r.reached) })
	<-ctx.Done()
	return ctx.Err()
}

// TestVideoProgressAliveNotKilledGate ③：有正常进度推进时绝不误杀。
func TestVideoProgressAliveNotKilledGate(t *testing.T) {
	dir := t.TempDir()
	rows := []Plan{mkRow(t, dir, 1, 4096)}

	clock := newFakeClock()
	spy := &logSpy{}
	// 每次进度回调前把假时钟推进 1 分钟，但进度**一直在动** ⇒ 永不超时。
	// 累计推进 40 分钟 > kill 阈值 10 分钟，足以证明判据是"有没有进度"而不是"跑了多久"。
	runner := &progressRunner{clock: clock, step: time.Minute, times: 40, pause: 3 * time.Millisecond}
	res, rerr := RunPlan(context.Background(), filepath.Join(dir, "output"), rows, runner, Hooks{
		Log: spy.log,
		Stall: StallPolicy{
			WarnAfter: 3 * time.Minute, KillAfter: 10 * time.Minute,
			Tick: 2 * time.Millisecond, Now: clock.Now,
		},
	})
	if rerr != nil {
		t.Fatalf("有进度时整批不该失败：%v", rerr)
	}
	if res.Failed != 0 || res.Done != 1 {
		t.Fatalf("有进度时被误杀：失败 %d / 成功 %d", res.Failed, res.Done)
	}
	if spy.warnCount() != 0 {
		t.Fatal("有进度时不该出现任何警告日志")
	}
}

// progressRunner 一直报进度（同时推进假时钟），最后写出产物。
type progressRunner struct {
	clock *fakeClock
	step  time.Duration
	times int
	pause time.Duration
}

func (r *progressRunner) Available() error               { return nil }
func (r *progressRunner) Version(context.Context) string { return "fake" }
func (r *progressRunner) Probe(context.Context, string) (MediaInfo, error) {
	return MediaInfo{}, nil
}

func (r *progressRunner) Transcode(ctx context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	for i := 0; i < r.times; i++ {
		r.clock.Advance(r.step)
		if onProgress != nil {
			onProgress(Progress{Percent: float64(i), Speed: "3x"})
		}
		time.Sleep(r.pause)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := os.MkdirAll(filepath.Dir(req.Dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(req.Dst, []byte("small"), 0o644)
}

// TestVideoTwoPassNotKilledGate ⑦：2-pass 第一遍只做分析、没有"出片进度"，
// 但它会喂看门狗 —— 一遍 30 分钟（假时钟）的健康分析绝不许被当成卡死。
func TestVideoTwoPassNotKilledGate(t *testing.T) {
	dir := t.TempDir()
	row := mkRow(t, dir, 1, 4096)
	row.TwoPass = true
	row.Encoder = EncoderCPU
	row.Mode = ModeBitrate

	clock := newFakeClock()
	spy := &logSpy{}
	runner := &twoPassRunner{clock: clock}
	res, rerr := RunPlan(context.Background(), filepath.Join(dir, "output"), []Plan{row}, runner, Hooks{
		Log: spy.log,
		Stall: StallPolicy{
			WarnAfter: 3 * time.Minute, KillAfter: 10 * time.Minute,
			Tick: 2 * time.Millisecond, Now: clock.Now,
		},
	})
	if rerr != nil {
		t.Fatalf("2-pass 不该失败：%v", rerr)
	}
	if res.Failed != 0 || res.Done != 1 {
		t.Fatalf("2-pass 第一遍被误杀：失败 %d / 成功 %d", res.Failed, res.Done)
	}
	if spy.warnCount() != 0 {
		t.Fatal("健康的第一遍分析不该出现警告（那是误报卡死）")
	}
	if n := spy.outCount(); n > 6 {
		t.Fatalf("第一遍不该打印进度行（只喂看门狗），实际 out 行 %d", n)
	}
}

// twoPassRunner 模拟 2-pass：第一遍只报 Pass=1 的进度（同时推进假时钟很久），
// 第二遍才写出产物。
type twoPassRunner struct{ clock *fakeClock }

func (r *twoPassRunner) Available() error               { return nil }
func (r *twoPassRunner) Version(context.Context) string { return "fake" }
func (r *twoPassRunner) Probe(context.Context, string) (MediaInfo, error) {
	return MediaInfo{}, nil
}

func (r *twoPassRunner) Transcode(_ context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	// 第一遍：30 分钟（假时钟）纯分析 —— 远超 10 分钟的 kill 阈值。
	for i := 0; i < 30; i++ {
		r.clock.Advance(time.Minute)
		if onProgress != nil {
			onProgress(Progress{Percent: float64(i) * 3, Pass: 1})
		}
		time.Sleep(3 * time.Millisecond)
	}
	// 第二遍：出片进度（这一遍才打印）。
	for i := 0; i <= 100; i += 25 {
		if onProgress != nil {
			onProgress(Progress{Percent: float64(i), Pass: 2})
		}
	}
	if err := os.MkdirAll(filepath.Dir(req.Dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(req.Dst, []byte("small"), 0o644)
}

// TestVideoAlignGate ④：尺寸对齐只影响编码尺寸，绝不放大、不顶破档位封顶边，
// 日志有说明（结构化字段 Aligned/AlignFrom），CPU 档不对齐。
func TestVideoAlignGate(t *testing.T) {
	preset1080, _ := FindPreset("1080p")
	preset480, _ := FindPreset("480p")
	info := MediaInfo{
		Width: 2812, Height: 2160, DurationSec: 2766, FileBytes: 2500139555,
		VideoKbps: 7230, HasVideo: true, HasAudio: true,
	}
	hw := PlanOne("a.mkv", "/tmp/a.mkv", "/tmp/out",
		info, Options{Preset: preset1080, Encoder: EncoderHardware, Mode: ModeQuality, Quality: 50}, false)
	// 比例算出来是 1406x1080（2806 不是 16 的倍数）⇒ 硬件档对齐到 1408x1080。
	if hw.TargetWidth != 1408 || hw.TargetHeight != 1080 {
		t.Fatalf("硬件档没有对齐到 16 的倍数：%dx%d", hw.TargetWidth, hw.TargetHeight)
	}
	if !hw.Aligned || hw.AlignFrom != "1406x1080" {
		t.Fatalf("对齐没有记录下来（Aligned=%v AlignFrom=%q）", hw.Aligned, hw.AlignFrom)
	}
	if hw.TargetHeight > 1080 || hw.TargetWidth > info.Width {
		t.Fatalf("对齐顶破了档位封顶边/放大了：%dx%d", hw.TargetWidth, hw.TargetHeight)
	}
	// 编码尺寸真的落到命令行上。
	args := strings.Join(TranscodeArgs(TranscodeRequest{
		Src: "s", Dst: "d", Width: hw.TargetWidth, Height: hw.TargetHeight,
		VideoKbps: hw.VideoKbps, Mode: ModeQuality, Encoder: EncoderHardware, Quality: 50,
	}, 0), " ")
	if !strings.Contains(args, "scale=1408:1080") {
		t.Fatalf("对齐后的尺寸没进 ffmpeg 参数：%s", args)
	}

	// CPU 档不对齐（x264 任意偶数都收）。
	cpu := PlanOne("a.mkv", "/tmp/a.mkv", "/tmp/out",
		info, Options{Preset: preset1080, Encoder: EncoderCPU, Mode: ModeQuality, Quality: 26}, false)
	if cpu.TargetWidth != 1406 || cpu.TargetHeight != 1080 || cpu.Aligned {
		t.Fatalf("CPU 档不该对齐：%dx%d Aligned=%v", cpu.TargetWidth, cpu.TargetHeight, cpu.Aligned)
	}

	// 对齐绝不放大：源 1406x1080 已经没有可对齐的空间（向上会超过源宽）。
	noup := PlanOne("b.mkv", "/tmp/b.mkv", "/tmp/out",
		MediaInfo{Width: 1406, Height: 1080, DurationSec: 100, FileBytes: 1000, VideoKbps: 7000, HasVideo: true},
		Options{Preset: preset1080, Encoder: EncoderHardware, Mode: ModeQuality, Quality: 50}, false)
	if noup.TargetWidth != 1406 || noup.TargetHeight != 1080 || noup.Aligned {
		t.Fatalf("对齐放大了源尺寸：%dx%d Aligned=%v", noup.TargetWidth, noup.TargetHeight, noup.Aligned)
	}

	// 档位封顶边不许被顶破：720p 档的封顶高必须 ≤ 720。
	cap720, _ := FindPreset("720p")
	small := PlanOne("c.mkv", "/tmp/c.mkv", "/tmp/out",
		MediaInfo{Width: 1000, Height: 562, DurationSec: 100, FileBytes: 1000, VideoKbps: 3000, HasVideo: true},
		Options{Preset: cap720, Encoder: EncoderHardware, Mode: ModeQuality, Quality: 45}, false)
	if small.TargetHeight > 720 {
		t.Fatalf("对齐顶破了 720p 封顶：%dx%d", small.TargetWidth, small.TargetHeight)
	}
	// 480p 档：854→864（16 的倍数），封顶高 480 不动。
	small480 := PlanOne("d.mkv", "/tmp/d.mkv", "/tmp/out",
		MediaInfo{Width: 1000, Height: 562, DurationSec: 100, FileBytes: 1000, VideoKbps: 3000, HasVideo: true},
		Options{Preset: preset480, Encoder: EncoderHardware, Mode: ModeQuality, Quality: 45}, false)
	if small480.TargetHeight != 480 || small480.TargetWidth%16 != 0 || small480.TargetWidth > 1000 {
		t.Fatalf("480p 对齐结果不对：%dx%d", small480.TargetWidth, small480.TargetHeight)
	}
}

// TestVideoEncoderFallbackGate ⑤：硬件编码报错 ⇒ 二次用 CPU 编码成功，
// 结果如实标"已回退软件编码"，质量档按 q:v→CRF 折算。
func TestVideoEncoderFallbackGate(t *testing.T) {
	dir := t.TempDir()
	rows := []Plan{mkRow(t, dir, 1, 4096)}
	runner := &failFirstRunner{}
	spy := &logSpy{}
	res, rerr := RunPlan(context.Background(), filepath.Join(dir, "output"), rows, runner, Hooks{Log: spy.log})
	if rerr != nil {
		t.Fatalf("回退后应当成功：%v", rerr)
	}
	if res.Done != 1 || res.Failed != 0 {
		t.Fatalf("期望 1 个成功、0 个失败，实际 %d/%d", res.Done, res.Failed)
	}
	if !res.Items[0].EncoderFallback {
		t.Fatal("结果没有标出「已回退软件编码」")
	}
	reqs := runner.reqsSnapshot()
	if len(reqs) != 2 {
		t.Fatalf("期望 2 次转码调用（硬件失败 + CPU 重试），实际 %d", len(reqs))
	}
	if reqs[0].Encoder != EncoderHardware || reqs[1].Encoder != EncoderCPU {
		t.Fatalf("回退的编码器不对：%s → %s", reqs[0].Encoder, reqs[1].Encoder)
	}
	// 档位语义不许偷偷改：尺寸/码率上限原样，只有质量刻度按 q:v→CRF 折算。
	if reqs[1].Width != reqs[0].Width || reqs[1].Height != reqs[0].Height ||
		reqs[1].VideoKbps != reqs[0].VideoKbps {
		t.Fatalf("回退改了尺寸/码率：%+v → %+v", reqs[0], reqs[1])
	}
	if reqs[0].Quality != 50 || reqs[1].Quality != 25 {
		t.Fatalf("质量档折算不对：q:v %d → CRF %d（期望 50 → 25）", reqs[0].Quality, reqs[1].Quality)
	}
	if spy.warnCount() == 0 {
		t.Fatal("回退没有留下警告日志")
	}
}

// failFirstRunner：第一次（硬件）直接报错，第二次写出产物。
type failFirstRunner struct {
	mu    sync.Mutex
	reqs  []TranscodeRequest
	calls int
}

func (r *failFirstRunner) Available() error               { return nil }
func (r *failFirstRunner) Version(context.Context) string { return "fake" }
func (r *failFirstRunner) Probe(context.Context, string) (MediaInfo, error) {
	return MediaInfo{}, nil
}

func (r *failFirstRunner) reqsSnapshot() []TranscodeRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TranscodeRequest(nil), r.reqs...)
}

func (r *failFirstRunner) Transcode(_ context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	if n == 1 {
		return fmt.Errorf("hevc_videotoolbox: cannot create compression session")
	}
	if err := os.MkdirAll(filepath.Dir(req.Dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(req.Dst, []byte("small"), 0o644)
}

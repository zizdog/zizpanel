//go:build ffmpeg_e2e

package videoopt

// decode_bench_e2e_test.go —— 真 ffmpeg 的 4K→480p「解码方式」对照（用户报障的同一形态）。
//
//	go test -tags ffmpeg_e2e ./internal/videoopt/ -run TestDecodeBenchE2E -v
//
// 两遍都是真跑：① 生产代码路径（BuildPlan + RunPlan，当前 = 软件解码）；
// ② 同样的命令把 -hwaccel videotoolbox 插到 -i 之前（父代理假设的"改动后"）。
// 报告 speed= 与墙钟 —— 这正是"该不该默认加硬解"的判据。
// 样本全在 t.TempDir()，不碰用户目录。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// e2eRunArgs 跑一条完整 ffmpeg 命令（-progress 已由 TranscodeArgs 带上），
// 返回墙钟秒数与最后一行的 speed=。
func e2eRunArgs(t *testing.T, ffmpeg string, args []string) (float64, string) {
	t.Helper()
	cmd := exec.Command(ffmpeg, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg 失败：%v\n%s", err, errb.String())
	}
	wall := time.Since(start).Seconds()
	speed := "-"
	for _, line := range strings.Split(out.String(), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "speed="); ok {
			speed = strings.TrimSpace(v)
		}
	}
	return wall, speed
}

func TestDecodeBenchE2E(t *testing.T) {
	ffmpeg := e2eBin(t, "ffmpeg")
	ffprobe := e2eBin(t, "ffprobe")
	dir := t.TempDir()
	src := filepath.Join(dir, "src4k.mp4")

	mk := exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=3840x2160:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=15",
		"-t", "15", "-shortest",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "14", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", src)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("造 4K 样本失败：%v\n%s", err, out)
	}

	runner := &FFmpegRunner{Ffmpeg: ffmpeg, Ffprobe: ffprobe}
	preset, ok := FindPreset("480p")
	if !ok {
		t.Fatal("预设 480p 不存在")
	}
	plan, err := BuildPlan(context.Background(), PlanRequest{
		Dir: dir, OutDir: filepath.Join(dir, "output"),
		Options: Options{Preset: preset, KBps: 800, Mode: ModeBitrate},
	}, runner)
	if err != nil || plan.Runnable != 1 {
		t.Fatalf("规划失败：err=%v runnable=%d", err, plan.Runnable)
	}
	row := plan.Rows[0]

	// ① 生产路径（RunPlan）：从任务日志里取真实的 speed=。
	var speeds []string
	res, err := RunPlan(context.Background(), plan.OutDir, plan.Rows, runner, Hooks{
		Log: func(level, msg string) {
			if level == tasks.LevelOut && strings.HasSuffix(msg, "）") {
				if i := strings.LastIndex(msg, "（"); i >= 0 {
					// "（"是 3 字节：按字符长度切，别按字节 +1（会切出半个汉字）。
					speeds = append(speeds, strings.TrimSuffix(msg[i+len("（"):], "）"))
				}
			}
		},
	})
	if err != nil || res.Done != 1 {
		t.Fatalf("生产路径转码失败：err=%v done=%d", err, res.Done)
	}
	prodSpeed := "-"
	if len(speeds) > 0 {
		prodSpeed = speeds[len(speeds)-1]
	}
	t.Logf("① 生产路径（软件解码）：墙钟 %.2fs · speed=%s · 任务日志 %s", res.DurationSec, prodSpeed, res.DurationText)

	// ② 同一命令 + -hwaccel videotoolbox（-hwaccel 是输入选项，必须插在 -i 之前）。
	hwArgs := []string{}
	inserted := false
	for _, a := range TranscodeArgs(TranscodeRequest{
		Src: row.Path, Dst: filepath.Join(dir, "hw.mp4"),
		Width: row.TargetWidth, Height: row.TargetHeight,
		VideoKbps: row.VideoKbps, AudioKbps: row.AudioKbps, DurationSec: row.DurationSec,
		Encoder: row.Encoder, Mode: row.Mode, Quality: row.Quality,
	}, 0) {
		if a == "-i" && !inserted {
			hwArgs = append(hwArgs, "-hwaccel", "videotoolbox")
			inserted = true
		}
		hwArgs = append(hwArgs, a)
	}
	if !inserted {
		t.Fatal("没找到 -i，命令形态变了")
	}
	// 断言生产 argv 本身没有硬解（本测试的另一半判据）。
	for _, a := range TranscodeArgs(TranscodeRequest{
		Src: row.Path, Dst: filepath.Join(dir, "prod.mp4"),
		Width: row.TargetWidth, Height: row.TargetHeight,
		VideoKbps: row.VideoKbps, AudioKbps: row.AudioKbps, DurationSec: row.DurationSec,
		Encoder: row.Encoder, Mode: row.Mode, Quality: row.Quality,
	}, 0) {
		if a == "-hwaccel" {
			t.Fatal("生产 argv 里出现了 -hwaccel（实测更慢，不该有）")
		}
	}
	hwWall, hwSpeed := e2eRunArgs(t, ffmpeg, hwArgs)
	_ = os.Remove(filepath.Join(dir, "hw.mp4"))
	t.Logf("② 假设改动（-hwaccel videotoolbox）：墙钟 %.2fs · speed=%s", hwWall, hwSpeed)

	t.Logf("结论：软件解码 %.2fs(%s) vs 硬解 %.2fs(%s) —— 快了 %.2f 倍的是%s",
		res.DurationSec, prodSpeed, hwWall, hwSpeed,
		hwWall/res.DurationSec, map[bool]string{true: "硬解", false: "软件解码"}[hwWall < res.DurationSec])
}

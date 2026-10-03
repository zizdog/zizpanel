//go:build ffmpeg_e2e

package videoopt

// ffmpeg_e2e_test.go —— **唯一**真跑 ffmpeg 的端到端验证（用户点名的验证项）。
//
// 平时不参与 make check（构建标签 ffmpeg_e2e），因为门禁/单测必须走垫片：
// 真 ffmpeg 慢且依赖这台机器装没装。手动跑：
//
//	go test -tags ffmpeg_e2e ./internal/videoopt/ -run TestRecursiveFFmpegE2E -v
//
// 造两层嵌套目录 + 3 个小视频（真编码），递归规划 + 递归执行，
// 断言产物路径是 output/<子目录>/<名字>.480p.mp4，并报告压缩前后的体积差。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// e2eBin 定位本机的 ffmpeg / ffprobe（PATH 与 Homebrew 都找不到就跳过）。
func e2eBin(t *testing.T, name string) string {
	t.Helper()
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/" + name, "/usr/local/bin/" + name} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	t.Skipf("本机没有 %s，跳过真 ffmpeg E2E", name)
	return ""
}

// e2eMakeVideo 用 lavfi 造一个真视频（3 秒、640x360、H.264/AAC）。
func e2eMakeVideo(t *testing.T, ffmpeg, dst, pattern string) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", pattern,
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-t", "3", "-shortest",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "16",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "96k",
		dst,
	}
	cmd := exec.Command(ffmpeg, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造视频失败（%s）：%v\n%s", filepath.Base(dst), err, out)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// TestRecursiveFFmpegE2E 是用户点名的真 ffmpeg 小样本验证。
func TestRecursiveFFmpegE2E(t *testing.T) {
	ffmpeg := e2eBin(t, "ffmpeg")
	ffprobe := e2eBin(t, "ffprobe")
	base := t.TempDir()

	type src struct {
		rel     string
		pattern string
	}
	inputs := []src{
		{"第1季/01.mkv", "testsrc2=size=1280x720:rate=25"},
		{"第1季/动漫/02.mp4", "testsrc2=size=960x540:rate=25"},
		{"剧场版/03.mp4", "testsrc2=size=1280x720:rate=30"},
	}
	before := map[string]int64{}
	for _, in := range inputs {
		p := filepath.Join(base, filepath.FromSlash(in.rel))
		before[in.rel] = e2eMakeVideo(t, ffmpeg, p, in.pattern)
	}

	runner := &FFmpegRunner{Ffmpeg: ffmpeg, Ffprobe: ffprobe}
	preset, ok := FindPreset("480p")
	if !ok {
		t.Fatal("预设 480p 不存在")
	}
	plan, err := BuildPlan(context.Background(), PlanRequest{
		Dir: base, Recursive: true,
		Options: Options{Preset: preset, KBps: 800, Mode: ModeBitrate},
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Recursive {
		t.Fatal("递归回显丢了")
	}
	if plan.Runnable != len(inputs) {
		t.Fatalf("应有 %d 个可压，实际 runnable=%d skipped=%d（%+v）",
			len(inputs), plan.Runnable, plan.Skipped, plan.Rows)
	}

	res, err := RunPlan(context.Background(), plan.OutDir, plan.Rows, runner, Hooks{})
	if err != nil {
		t.Fatalf("递归压缩失败：%v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("有 %d 个失败：%+v", res.Failed, res.Items)
	}

	var totalBefore, totalAfter int64
	for _, in := range inputs {
		want := filepath.Join(base, OutputDirName, filepath.FromSlash(in.rel))
		want = want[:len(want)-len(filepath.Ext(want))] + ".480p.mp4"
		st, serr := os.Stat(want)
		if serr != nil {
			t.Fatalf("产物路径不对（%s → %s）：%v", in.rel, want, serr)
		}
		after := st.Size()
		totalBefore += before[in.rel]
		totalAfter += after
		pct := float64(before[in.rel]-after) / float64(before[in.rel]) * 100
		t.Logf("E2E %-22s %8d B → %8d B（-%.1f%%）  产物 %s",
			in.rel, before[in.rel], after, pct, want)
	}
	t.Logf("E2E 合计：%d B → %d B（-%.1f%%），compress=%d placed=%d skipped=%d，%s",
		totalBefore, totalAfter,
		float64(totalBefore-totalAfter)/float64(totalBefore)*100,
		res.Done, res.Placed, res.Skipped, res.DurationText)
	// 至少要有产物真的变小：否则这次"验证"什么也没证明。
	if totalAfter >= totalBefore {
		t.Fatalf("递归压缩没有让总体积变小：%d → %d", totalBefore, totalAfter)
	}
}

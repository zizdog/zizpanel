package videoopt

// recurse_gate_test.go —— 「递归子目录 + 按原目录结构输出」的门禁（扫描/规划层）。
//
// 为什么此前门禁抓不到：视频压缩只扫**当前这一层**（Scan 用 os.ReadDir 只看一层），
// 既没有递归、也没有"相对路径"这个概念；`output/` 天然不会被扫到所以没人管它。
// 一旦加上递归，四件会真伤到用户的事就都能一路绿：
//   ① 跟着符号链接走出基准目录，把盘外的东西也压了/放了；
//   ② 把 output/ 里的产物当源再扫一遍 ⇒ 反复压缩、自噬；
//   ③ 深度/数量不设限，选了一个大盘就卡死且**静默只处理一半**；
//   ④ 一个读不到的子目录让整个任务失败。
//
// 一条门禁覆盖整类：递归扫描的相对路径 / 跳过 output / 不跟符号链接 /
// 输出结构 / recursive=false 回归 / 上限如实说明 / 无权限子目录跳过。
// 全用 t.TempDir() + 假 Runner，不跑真 ffmpeg（真 ffmpeg 只在 ffmpeg_e2e_test.go）。
// 403 越界语义在 web 层（api_video_recursive_gate_test.go），因为那是白名单校验的职责。

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// recurseRunner 是执行器替身：Probe 对任何路径都给"能压"的信息，绝不跑 ffmpeg。
type recurseRunner struct {
	outBytes int64
	reqs     []TranscodeRequest
}

func (r *recurseRunner) Available() error               { return nil }
func (r *recurseRunner) Version(context.Context) string { return "fake-recurse" }

func (r *recurseRunner) Probe(context.Context, string) (MediaInfo, error) {
	return MediaInfo{
		Width: 1280, Height: 720, DurationSec: 4, FileBytes: 4096,
		VideoKbps: 4000, HasVideo: true,
	}, nil
}

func (r *recurseRunner) Transcode(_ context.Context, req TranscodeRequest, _ func(Progress)) error {
	r.reqs = append(r.reqs, req)
	if err := os.MkdirAll(filepath.Dir(req.Dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(req.Dst, bytes.Repeat([]byte{7}, int(r.outBytes)), 0o644)
}

// recFile 造一个（可能带子目录的）文件。
func recFile(t *testing.T, dir, rel string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// recPlan 在 dir 上跑一次规划（递归与否由参数决定）。
func recPlan(t *testing.T, dir string, recursive bool, limits ScanLimits) (PlanResult, *recurseRunner) {
	t.Helper()
	preset, ok := FindPreset("480p")
	if !ok {
		t.Fatal("预设 480p 不存在")
	}
	runner := &recurseRunner{outBytes: 100}
	res, err := BuildPlan(context.Background(), PlanRequest{
		Dir: dir, Recursive: recursive, Limits: limits,
		Options: Options{Preset: preset, KBps: 2000, Mode: ModeBitrate},
	}, runner)
	if err != nil {
		t.Fatalf("规划失败：%v", err)
	}
	return res, runner
}

// recRels 取计划里的相对路径（升序）。
func recRels(res PlanResult) []string {
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, r.RelPath)
	}
	sort.Strings(out)
	return out
}

// TestVideoRecursiveGate 是这一条门禁。
func TestVideoRecursiveGate(t *testing.T) {
	content := bytes.Repeat([]byte{1}, 4096)

	t.Run("① 递归扫描：三层嵌套 + 中文目录 + 混入非视频 ⇒ 只收视频且 rel_path 正确", func(t *testing.T) {
		dir := t.TempDir()
		recFile(t, dir, "00.mp4", content)
		recFile(t, dir, "第1季/01.mkv", content)
		recFile(t, dir, "第1季/动漫/02.mp4", content)
		recFile(t, dir, "第1季/动漫/深层/03.webm", content)
		recFile(t, dir, "第1季/readme.txt", []byte("不是视频"))
		recFile(t, dir, "第1季/动漫/cover.jpg", []byte("也不是视频"))

		res, _ := recPlan(t, dir, true, ScanLimits{})
		if !res.Recursive {
			t.Error("res.Recursive 必须回显 true")
		}
		want := []string{"00.mp4", "第1季/01.mkv", "第1季/动漫/02.mp4", "第1季/动漫/深层/03.webm"}
		if got := recRels(res); !reflect.DeepEqual(got, want) {
			t.Fatalf("递归扫描的相对路径 %v，期望 %v", got, want)
		}
		for _, r := range res.Rows {
			if want := filepath.Join(dir, filepath.FromSlash(r.RelPath)); r.Path != want {
				t.Errorf("Path=%s，期望 %s", r.Path, want)
			}
		}
	})

	t.Run("② 跳过基准目录下的 output/（含指向它的软链接）", func(t *testing.T) {
		dir := t.TempDir()
		recFile(t, dir, "a.mp4", content)
		// 已有产物目录里放视频：若不跳过，它们会被当成源反复压缩/自噬。
		recFile(t, dir, "output/老产物.mp4", content)
		recFile(t, dir, "output/子目录/更深.mp4", content)
		// 以及"任何指向它的路径"：一个指向 output 的软链接。
		if err := os.Symlink(filepath.Join(dir, "output"), filepath.Join(dir, "ptr")); err != nil {
			t.Fatal(err)
		}

		res, _ := recPlan(t, dir, true, ScanLimits{})
		// 负向对照：删掉 scanTree 里的 isSameDir 判断，这里会多出 output 里的两行 ⇒ 变红。
		if got := recRels(res); !reflect.DeepEqual(got, []string{"a.mp4"}) {
			t.Fatalf("output/ 里的视频绝不能被扫进来，实际 %v", got)
		}
		for _, r := range res.Rows {
			if strings.HasPrefix(r.RelPath, "output/") || strings.HasPrefix(r.RelPath, "ptr/") {
				t.Errorf("产物目录被当成源了：%s", r.RelPath)
			}
		}
	})

	t.Run("③ 不跟随符号链接：盘外目录/盘外文件都不许被处理、不许越界", func(t *testing.T) {
		outside := t.TempDir()
		recFile(t, outside, "leak.mp4", content)
		dir := t.TempDir()
		recFile(t, dir, "ok.mp4", content)
		// 指向盘外目录的链接（不许被遍历）与指向盘外视频文件的链接（不许被当成源）。
		// 后者是这条判据真正的负向对照：目录靠 e.IsDir() 本来就进不去，
		// 而**文件**链接只有显式的 ModeSymlink 判断才拦得住。
		if err := os.Symlink(outside, filepath.Join(dir, "外链")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "leak.mp4"), filepath.Join(dir, "leak-link.mp4")); err != nil {
			t.Fatal(err)
		}

		res, runner := recPlan(t, dir, true, ScanLimits{})
		// 负向对照：去掉 scanTree 里的 ModeSymlink 判断，leak-link.mp4 会被扫进来 ⇒ 变红。
		if got := recRels(res); !reflect.DeepEqual(got, []string{"ok.mp4"}) {
			t.Fatalf("符号链接指向的目录/文件绝不能被扫，实际 %v", got)
		}
		for _, r := range res.Rows {
			if strings.HasPrefix(r.Path, outside) {
				t.Errorf("越界扫到了盘外文件：%s", r.Path)
			}
		}
		// 执行也不许碰盘外的东西（链接本身或它指向的目标）。
		if _, err := RunPlan(context.Background(), res.OutDir, res.Rows, runner, Hooks{}); err != nil {
			t.Fatal(err)
		}
		for _, req := range runner.reqs {
			if strings.HasPrefix(req.Src, outside) || strings.Contains(req.Src, "leak-link") {
				t.Errorf("越界转码了盘外文件：%s", req.Src)
			}
		}
		if _, err := os.Stat(filepath.Join(outside, "leak.480p.mp4")); !os.IsNotExist(err) {
			t.Error("盘外目录里绝不该出现产物")
		}
	})

	t.Run("④ 输出结构：OutPath == output/<rel_dir>/<名字>（原样放入同理）", func(t *testing.T) {
		dir := t.TempDir()
		recFile(t, dir, "动漫/第1季/01.mkv", content)
		recFile(t, dir, "动漫/02.mp4", content)
		recFile(t, dir, "03.mp4", content)

		res, _ := recPlan(t, dir, true, ScanLimits{})
		if want := filepath.Join(dir, OutputDirName); res.OutDir != want {
			t.Fatalf("OutDir=%s，期望 %s", res.OutDir, want)
		}
		for _, r := range res.Rows {
			relDir := filepath.Dir(filepath.FromSlash(r.RelPath))
			wantOut := filepath.Join(res.OutDir, relDir, OutputName(r.Name, "480p"))
			if r.OutPath != wantOut {
				t.Errorf("%s 的 OutPath=%s，期望 %s", r.RelPath, r.OutPath, wantOut)
			}
			wantPlace := filepath.Join(res.OutDir, relDir, PlaceName(r.Name, "480p"))
			if r.PlacePath != wantPlace {
				t.Errorf("%s 的 PlacePath=%s，期望 %s", r.RelPath, r.PlacePath, wantPlace)
			}
			if !strings.HasPrefix(r.OutPath, res.OutDir+string(filepath.Separator)) {
				t.Errorf("产物必须落在 output/ 里：%s", r.OutPath)
			}
		}
	})

	t.Run("⑤ recursive=false 回归：只扫当前这一层、rel_path 为空", func(t *testing.T) {
		dir := t.TempDir()
		recFile(t, dir, "top.mp4", content)
		recFile(t, dir, "第1季/nested.mp4", content)

		res, _ := recPlan(t, dir, false, ScanLimits{})
		if res.Recursive {
			t.Error("非递归时 res.Recursive 必须是 false")
		}
		if got := recRels(res); !reflect.DeepEqual(got, []string{""}) {
			t.Fatalf("非递归不许扫子目录，实际 %v", got)
		}
		if len(res.Rows) != 1 || res.Rows[0].Name != "top.mp4" {
			t.Fatalf("非递归只该有 top.mp4，实际 %+v", res.Rows)
		}
		if len(res.ScanNotes) != 0 || len(res.ScanSkipped) != 0 {
			t.Errorf("非递归不该有递归扫描说明：notes=%v skipped=%v", res.ScanNotes, res.ScanSkipped)
		}
		// 原有 Scan 仍只看一层（与既有行为逐字节一致的底层保证）。
		srcs, err := Scan(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(srcs) != 1 || srcs[0].Name != "top.mp4" {
			t.Fatalf("Scan 必须只返回这一层的视频，实际 %+v", srcs)
		}
	})

	t.Run("⑥ 深度/数量超限 ⇒ 如实说明（达到上限，未继续）", func(t *testing.T) {
		// 深度：基准目录 = 第 0 层；MaxDepth=2 ⇒ a/b 可进，a/b/c 不进。
		dir := t.TempDir()
		recFile(t, dir, "a/one.mp4", content)
		recFile(t, dir, "a/b/two.mp4", content)
		recFile(t, dir, "a/b/c/deep.mp4", content)
		res, _ := recPlan(t, dir, true, ScanLimits{MaxDepth: 2, MaxFiles: 100})
		notes := strings.Join(res.ScanNotes, "|")
		if !strings.Contains(notes, "达到上限，未继续") {
			t.Fatalf("深度超限必须如实说明（含「达到上限，未继续」），实际 notes=%v", res.ScanNotes)
		}
		for _, r := range res.Rows {
			if strings.Contains(r.RelPath, "deep.mp4") {
				t.Errorf("超过深度上限的文件不该被扫进来：%s", r.RelPath)
			}
		}
		if len(res.Rows) != 2 {
			t.Errorf("深度上限内应有 2 个视频，实际 %d（%v）", len(res.Rows), recRels(res))
		}

		// 数量：3 个视频、上限 2 ⇒ 只收 2 个且必须说明。
		dir2 := t.TempDir()
		for _, n := range []string{"a.mp4", "b.mp4", "c.mp4"} {
			recFile(t, dir2, n, content)
		}
		res2, _ := recPlan(t, dir2, true, ScanLimits{MaxFiles: 2})
		if !strings.Contains(strings.Join(res2.ScanNotes, "|"), "达到上限，未继续") {
			t.Fatalf("数量超限必须如实说明，实际 notes=%v", res2.ScanNotes)
		}
		if len(res2.Rows) > 2 {
			t.Errorf("数量上限 2，实际收了 %d 个", len(res2.Rows))
		}
		if len(res2.Rows) == 0 {
			t.Error("上限内至少要有 1 个（绝不是静默一个都不收）")
		}
	})

	t.Run("⑦ 无权限子目录 ⇒ 跳过并计入清单，任务不整体失败", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("以 root 跑时 0 权限目录仍可读，这条判据不成立（本机 make check 不是 root）")
		}
		dir := t.TempDir()
		recFile(t, dir, "ok.mp4", content)
		locked := filepath.Join(dir, "locked")
		recFile(t, dir, "locked/hidden.mp4", content)
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) // 让 t.TempDir 能清理

		res, runner := recPlan(t, dir, true, ScanLimits{})
		if got := recRels(res); !reflect.DeepEqual(got, []string{"ok.mp4"}) {
			t.Fatalf("读不到的子目录必须跳过，实际 %v", got)
		}
		if len(res.ScanSkipped) != 1 || res.ScanSkipped[0].RelPath != "locked" {
			t.Fatalf("读不到的子目录必须记入跳过清单，实际 %+v", res.ScanSkipped)
		}
		if strings.TrimSpace(res.ScanSkipped[0].Reason) == "" {
			t.Error("跳过清单必须带原因（面板要如实展示）")
		}
		// 任务照常跑完（不是整体失败）。
		out, err := RunPlan(context.Background(), res.OutDir, res.Rows, runner, Hooks{})
		if err != nil {
			t.Fatalf("一条子目录读不到不该让整个任务失败：%v", err)
		}
		if out.Done != 1 {
			t.Errorf("ok.mp4 必须被处理，实际 done=%d", out.Done)
		}
	})

	t.Run("⑧ 递归执行：产物真的落在 output/<子目录>/ 下", func(t *testing.T) {
		dir := t.TempDir()
		recFile(t, dir, "第1季/01.mkv", content)
		res, runner := recPlan(t, dir, true, ScanLimits{})
		out, err := RunPlan(context.Background(), res.OutDir, res.Rows, runner, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Done != 1 {
			t.Fatalf("应完成 1 个，实际 done=%d failed=%d skipped=%d", out.Done, out.Failed, out.Skipped)
		}
		want := filepath.Join(dir, OutputDirName, "第1季", "01.480p.mp4")
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("产物必须按原目录结构落盘（%s）：%v", want, err)
		}
		// 相对路径也要进结果项（面板/清单靠它区分同名文件）。
		if out.Items[0].RelPath != "第1季/01.mkv" {
			t.Errorf("结果项的 RelPath=%q，期望 第1季/01.mkv", out.Items[0].RelPath)
		}
	})
}

// TestVideoRecursivePlaceStructure 单独覆盖"原样放入也按结构"：
// 封顶即跳过的源文件必须硬链接进 output/<子目录>/，而不是平铺在 output/ 根下。
func TestVideoRecursivePlaceStructure(t *testing.T) {
	dir := t.TempDir()
	low := recFile(t, dir, "剧集/第一季/老片.mkv", []byte("low-bitrate-content"))

	preset, _ := FindPreset("480p")
	runner := &recurseRunner{outBytes: 100}
	// 源码率 300 < 480p 下限 800×0.95 ⇒ 封顶即跳过 ⇒ 原样放入。
	res, err := BuildPlan(context.Background(), PlanRequest{
		Dir: dir, Recursive: true,
		Options: Options{Preset: preset, KBps: 0, Mode: ModeBitrate},
	}, &lowRunner{inner: runner, kbps: 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || !res.Rows[0].PlaceInOutput {
		t.Fatalf("低码率源必须走原样放入，实际 %+v", res.Rows)
	}
	out, rerr := RunPlan(context.Background(), res.OutDir, res.Rows, runner, Hooks{})
	if rerr != nil {
		t.Fatal(rerr)
	}
	if out.Placed != 1 {
		t.Fatalf("应放入 1 个，实际 placed=%d（%+v）", out.Placed, out.Items)
	}
	placed := filepath.Join(dir, OutputDirName, "剧集", "第一季", "老片.480p.mkv")
	if _, err := os.Stat(placed); err != nil {
		t.Fatalf("原样放入也要按原目录结构（%s）：%v", placed, err)
	}
	s1, _ := os.Stat(low)
	s2, _ := os.Stat(placed)
	if !os.SameFile(s1, s2) {
		t.Error("同卷下原样放入必须是硬链接（同一 inode）")
	}
}

// lowRunner 让 Probe 固定报一个低码率（构造"封顶即跳过"）。
type lowRunner struct {
	inner Runner
	kbps  int
}

func (l *lowRunner) Available() error                   { return nil }
func (l *lowRunner) Version(ctx context.Context) string { return l.inner.Version(ctx) }
func (l *lowRunner) Probe(context.Context, string) (MediaInfo, error) {
	return MediaInfo{
		Width: 1280, Height: 720, DurationSec: 4, FileBytes: 1024,
		VideoKbps: l.kbps, HasVideo: true,
	}, nil
}
func (l *lowRunner) Transcode(ctx context.Context, req TranscodeRequest, on func(Progress)) error {
	return l.inner.Transcode(ctx, req, on)
}

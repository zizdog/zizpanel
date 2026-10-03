package videoopt

// scan_progress_gate_test.go —— 「递归扫描要有真实进度、要能取消、要节流」的门禁。
//
// 为什么此前门禁抓不到：递归扫描此前**没有任何进度输出**，前端只能显示一句静态
// 「正在刷新计划…」；用户报了"视频很多时以为卡死了"。没有这条门禁，下面三件会
// 真伤到用户的事都能一路绿：
//   ① 报出去的进度是估的/会回退，或者终值与真实扫描结果对不上（界面在骗人）；
//   ② 扫描不看 ctx：用户点「取消」后仍在后台把整棵大盘读完（界面说取消了、实际没停）；
//   ③ 逐文件回调：900 个文件推 900 次，把调用方也拖慢。
//
// 一条门禁覆盖整类：大目录树的进度单调且终值精确 / 取消真的停且不产计划 /
// 非递归零回调（回归）/ 节流后回调数远小于文件数。
// 全用 t.TempDir() + 假 Runner，不跑真 ffmpeg。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// scanSample 是一次进度回调的快照（含本地时钟，用来断言节流）。
type scanSample struct {
	dirs, videos int
	cur          string
	ms           int64
}

// buildScanTree 造 groups×perGroup 个子目录、每个子目录 perDir 个视频，
// 返回基准目录与"真实目录数 / 视频数"（基准目录自身也算一个目录）。
func buildScanTree(t *testing.T, groups, perGroup, perDir int) (dir string, dirs, videos int) {
	t.Helper()
	dir = t.TempDir()
	dirs = 1 // 基准目录自身也要被读过
	content := []byte{1}
	for g := 0; g < groups; g++ {
		gdir := filepath.Join(dir, fmt.Sprintf("季%02d", g))
		if err := os.MkdirAll(gdir, 0o755); err != nil {
			t.Fatal(err)
		}
		dirs++
		for d := 0; d < perGroup; d++ {
			sub := filepath.Join(gdir, fmt.Sprintf("第%d集", d))
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			dirs++
			for v := 0; v < perDir; v++ {
				if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("v%d.mp4", v)), content, 0o644); err != nil {
					t.Fatal(err)
				}
				videos++
			}
		}
	}
	return dir, dirs, videos
}

// recPlanProgress 在 dir 上跑一次规划并把进度回调收集下来。
func recPlanProgress(t *testing.T, dir string, recursive bool, onScan ScanProgressFunc) (PlanResult, []scanSample) {
	t.Helper()
	preset, ok := FindPreset("480p")
	if !ok {
		t.Fatal("预设 480p 不存在")
	}
	var mu sync.Mutex
	var samples []scanSample
	wrap := func(dirs, videos int, cur string) {
		mu.Lock()
		samples = append(samples, scanSample{dirs: dirs, videos: videos, cur: cur, ms: time.Now().UnixMilli()})
		mu.Unlock()
		if onScan != nil {
			onScan(dirs, videos, cur)
		}
	}
	res, err := BuildPlan(context.Background(), PlanRequest{
		Dir: dir, Recursive: recursive,
		Options: Options{Preset: preset, KBps: 2000, Mode: ModeBitrate},
		OnScan:  wrap,
	}, &recurseRunner{outBytes: 100})
	if err != nil {
		t.Fatalf("规划失败：%v", err)
	}
	return res, samples
}

// TestVideoScanProgressGate 是这一条门禁。
func TestVideoScanProgressGate(t *testing.T) {
	// 300 个子目录 / 900 个视频（用户点名的规模口径）。
	dir, totalDirs, totalVideos := buildScanTree(t, 30, 10, 3)
	if totalDirs != 331 || totalVideos != 900 {
		t.Fatalf("夹具不对：dirs=%d videos=%d（期望 331 / 900）", totalDirs, totalVideos)
	}

	t.Run("① 进度单调不减、终值与真实扫描结果一致、带当前相对路径", func(t *testing.T) {
		res, samples := recPlanProgress(t, dir, true, nil)
		if len(samples) < 2 {
			t.Fatalf("300 个目录的扫描至少要有多于 1 次进度回调，实际 %d", len(samples))
		}
		for i, s := range samples {
			if s.dirs < 1 {
				t.Errorf("第 %d 次回调的已扫描目录数为 %d（至少要是读完的基准目录 1）", i, s.dirs)
			}
			if i == 0 {
				continue
			}
			prev := samples[i-1]
			if s.dirs < prev.dirs || s.videos < prev.videos {
				t.Fatalf("进度必须单调不减，第 %d 次回退：%+v → %+v", i, prev, s)
			}
		}
		// 终值必须就是真扫出来的数，不是估的（负向对照：把终值改成 len(Rows) 之类会变红）。
		last := samples[len(samples)-1]
		if last.dirs != totalDirs || res.ScanDirs != totalDirs {
			t.Errorf("目录数终值必须等于真实目录数 %d：回调=%d ScanDirs=%d", totalDirs, last.dirs, res.ScanDirs)
		}
		if last.videos != totalVideos || res.ScanVideos != totalVideos {
			t.Errorf("视频数终值必须等于真实视频数 %d：回调=%d ScanVideos=%d", totalVideos, last.videos, res.ScanVideos)
		}
		// 当前路径必须是**相对**路径（绝不把绝对路径漏给界面，也绝不越界）。
		sawCur := false
		for _, s := range samples {
			if s.cur == "" {
				continue
			}
			sawCur = true
			if filepath.IsAbs(s.cur) || filepath.IsAbs(filepath.FromSlash(s.cur)) {
				t.Errorf("当前路径必须是相对基准目录的路径，实际 %q", s.cur)
			}
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(s.cur))); err != nil {
				t.Errorf("上报的当前目录必须真实存在（%q）：%v", s.cur, err)
			}
		}
		if !sawCur {
			t.Error("进度里必须带“当前在哪一层目录”（cur 全空）")
		}
	})

	t.Run("② 取消：立刻停、如实报已取消、不产生计划", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var mu sync.Mutex
		var maxDirs, seen int
		preset, _ := FindPreset("480p")
		start := time.Now()
		res, err := BuildPlan(ctx, PlanRequest{
			Dir: dir, Recursive: true,
			Options: Options{Preset: preset, KBps: 2000, Mode: ModeBitrate},
			OnScan: func(dirs, videos int, cur string) {
				mu.Lock()
				defer mu.Unlock()
				seen++
				if dirs > maxDirs {
					maxDirs = dirs
				}
				if seen == 1 {
					cancel() // 第一次进度回调就取消（模拟用户点「取消」）
				}
			},
		}, &recurseRunner{outBytes: 100})
		elapsed := time.Since(start)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后必须返回 context.Canceled，实际 %v", err)
		}
		if len(res.Rows) != 0 {
			t.Fatalf("取消后绝不许产生计划，实际 %d 行", len(res.Rows))
		}
		if elapsed >= 2*time.Second {
			t.Errorf("取消后扫描必须在 2s 内停下，实际 %v", elapsed)
		}
		// 这条是"去掉扫描循环里的 ctx.Err() 检查 ⇒ 用例变红"的判据：
		// 取消后扫描不许再读完整棵树（终值会等于真实目录数）。
		mu.Lock()
		gotMax := maxDirs
		mu.Unlock()
		if gotMax >= totalDirs {
			t.Errorf("取消后仍把整棵树扫完了（已扫描目录数 %d ≥ 真实 %d）——扫描循环没看 ctx", gotMax, totalDirs)
		}
	})

	t.Run("③ 非递归回归：只看当前这一层、零进度回调、不带上限说明", func(t *testing.T) {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "top.mp4"), []byte{1}, 0o644); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(d, "第1季")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "nested.mp4"), []byte{1}, 0o644); err != nil {
			t.Fatal(err)
		}
		res, samples := recPlanProgress(t, d, false, nil)
		if len(samples) != 0 {
			t.Fatalf("非递归不许出现递归扫描进度回调，实际 %d 次：%+v", len(samples), samples)
		}
		if res.Recursive || res.ScanDirs != 0 || res.ScanVideos != 0 {
			t.Errorf("非递归必须与既有响应形状一致：recursive=%v scan_dirs=%d scan_videos=%d",
				res.Recursive, res.ScanDirs, res.ScanVideos)
		}
		if len(res.Rows) != 1 || res.Rows[0].Name != "top.mp4" || res.Rows[0].RelPath != "" {
			t.Fatalf("非递归只该有 top.mp4 且 rel_path 为空，实际 %+v", res.Rows)
		}
		if len(res.ScanNotes) != 0 || len(res.ScanSkipped) != 0 {
			t.Errorf("非递归不该有递归扫描说明：notes=%v skipped=%v", res.ScanNotes, res.ScanSkipped)
		}
	})

	t.Run("④ 节流：900 个视频不许产生海量回调（远小于文件数）", func(t *testing.T) {
		_, samples := recPlanProgress(t, dir, true, nil)
		// 负向对照：去掉 scanReporter 的节流（回调改回每次目录/每个文件都调）这里必红。
		if len(samples) >= totalVideos/10 {
			t.Fatalf("900 个视频却产生了 %d 次回调（必须远小于 %d）——节流没生效",
				len(samples), totalVideos)
		}
		if len(samples) > totalDirs/5 {
			t.Fatalf("回调次数 %d 不该接近目录数 %d", len(samples), totalDirs)
		}
		// 同一毫秒内也不许堆出一大把回调（节流的时间维度）。
		perMS := map[int64]int{}
		for _, s := range samples {
			perMS[s.ms]++
		}
		for ms, n := range perMS {
			if n > scanReportEveryDirs {
				t.Fatalf("同一毫秒（%d）内产生了 %d 次回调（> 每 %d 个目录一次）", ms, n, scanReportEveryDirs)
			}
		}
	})

	t.Run("⑤ 连点选项：被中断的旧扫描不许删掉/覆盖新一轮的进度", func(t *testing.T) {
		store := NewScanProgressStore()
		old := store.Begin("/d", true)
		store.UpdateScan("/d", old, 3, 1, "a")
		fresh := store.Begin("/d", true) // 用户连点：新一轮登记
		store.UpdateScan("/d", fresh, 9, 2, "b")
		store.End("/d", old) // 旧请求（已被 abort）收尾
		p, ok := store.Get("/d")
		if !ok || p.DirsScanned != 9 {
			t.Fatalf("旧请求的 End 不许删掉新一轮的进度：ok=%v %+v", ok, p)
		}
		store.UpdateScan("/d", old, 1, 0, "stale")
		if p, _ = store.Get("/d"); p.DirsScanned != 9 {
			t.Fatalf("旧代号不许覆盖新一轮的进度，实际 %+v", p)
		}
		store.End("/d", fresh)
		if _, ok := store.Get("/d"); ok {
			t.Error("本轮结束后必须清掉条目（绝不留陈旧进度）")
		}
	})
}

package web

// api_files_progress_gate_test.go —— 「复制/移动/删除走任务中心 + 字节级进度」的唯一门禁。
//
// 为什么现有门禁抓不到：改前这三个操作是**同步 HTTP 请求**（请求里干完才返回），
// 既没有进度回调、也没有 ctx 取消路径 —— 所以任何测试都不可能断言"进度对不对"
// 或"取消后源文件还在不在"。internal/files/move_test.go 只测单条目语义，
// 而且全在同一卷的临时目录里，**永远触发不到跨卷回退**（那条路才是最容易删错源的分支）。
//
// 一条门禁覆盖整类问题：
//   ① 80 个文件时进度回调的 done/total/百分比正确且单调；
//   ② 取消：半成品删掉、**源文件一个都不能少**、汇总如实说"未处理 N 项"；
//   ③ 三个接口：合法请求 202 + task_id、越界 403、空请求 400，任务真的把活干完。
// 全部在 t.TempDir()/测试沙箱里造小文件，不碰真实大文件与用户目录。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// waitFileOpTask 等任务结束并断言它成功了（复用既有的 waitTaskDone，别再造一个等待器）。
func waitFileOpTask(t *testing.T, srv *Server, id string) {
	t.Helper()
	tk := waitTaskDone(t, srv, id)
	if got := tk.Status(); got != tasks.StatusSucceeded {
		t.Fatalf("任务 %s 状态 %s，错误=%v", id, got, tk.Meta().Error)
	}
}

// writeTestFile 在测试沙箱里造一个指定大小的文件。
func writeTestFile(t *testing.T, p string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFileOpProgressGate 是这一条门禁。
func TestFileOpProgressGate(t *testing.T) {
	srv, _ := newTestServer(t)
	mgr := srv.fileManager()

	t.Run("① 80 个文件：进度回调的 done/total 与百分比正确且单调", func(t *testing.T) {
		srcDir := filepath.Join(srv.Cfg.WWWRoot, "op-src")
		dstDir := filepath.Join(srv.Cfg.WWWRoot, "op-dst")
		// 目标目录必须存在：粘贴的目标永远是"用户当前打开的目录"，
		// 面板不会替用户凭空造出一棵目录树（老 Copy 也是这样）。
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			t.Fatal(err)
		}
		const n = 80
		const size = 4096
		pairs := make([]files.FilePair, 0, n)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("f%02d.bin", i)
			writeTestFile(t, filepath.Join(srcDir, name), size)
			pairs = append(pairs, files.FilePair{From: filepath.Join(srcDir, name), To: filepath.Join(dstDir, name)})
		}

		var samples []files.OpProgress
		res, err := mgr.CopyItems(context.Background(), pairs, files.MoveConflictRename, func(p files.OpProgress) {
			samples = append(samples, p)
		})
		if err != nil {
			t.Fatalf("复制失败: %v", err)
		}
		if res.Done != n || res.Failed != 0 || res.Skipped != 0 {
			t.Fatalf("汇总不对：done=%d failed=%d skipped=%d（%s）", res.Done, res.Failed, res.Skipped, res.Msg)
		}
		if len(samples) == 0 {
			t.Fatal("一次进度回调都没有 —— 前端进度条会永远空着")
		}
		// 单调不减 + 计数最终正确。
		prevDone, prevBytes := 0, int64(0)
		sawMid := false
		for i, p := range samples {
			if p.FilesDone < prevDone || p.DoneBytes < prevBytes {
				t.Fatalf("第 %d 次进度回退了：files %d→%d，bytes %d→%d", i, prevDone, p.FilesDone, prevBytes, p.DoneBytes)
			}
			prevDone, prevBytes = p.FilesDone, p.DoneBytes
			if p.TotalBytes == int64(n*size) && p.DoneBytes > 0 && p.DoneBytes < p.TotalBytes {
				sawMid = true
			}
		}
		last := samples[len(samples)-1]
		if last.FilesTotal != n || last.FilesDone != n {
			t.Errorf("最终文件数不对：%d/%d（期望 %d/%d）", last.FilesDone, last.FilesTotal, n, n)
		}
		if last.TotalBytes != int64(n*size) || last.DoneBytes != last.TotalBytes {
			t.Errorf("最终字节数不对：%d/%d（期望 %d）", last.DoneBytes, last.TotalBytes, n*size)
		}
		if !sawMid {
			t.Error("没有任何「进行到一半」的进度 —— 进度条只会从 0 跳到 100")
		}
		// 面板展示的那句话必须能被前端直接读懂（12/80 文件 · 1.2 GB / 9.6 GB · 45%）。
		// 分子分母口径一致：都是**文件个数**（目录按其中包含的文件数展开）。
		msg := fileOpMessage(last)
		if !strings.Contains(msg, fmt.Sprintf("%d/%d 文件", n, n)) || !strings.Contains(msg, "100%") {
			t.Errorf("进度文案不对：%q", msg)
		}
		// 产物真的都在。
		for i := 0; i < n; i++ {
			if _, err := os.Stat(filepath.Join(dstDir, fmt.Sprintf("f%02d.bin", i))); err != nil {
				t.Fatalf("第 %d 个产物不存在：%v", i, err)
			}
		}
	})

	t.Run("② 跨盘移动被取消：半成品删掉，源文件一个都不能少", func(t *testing.T) {
		srcDir := filepath.Join(srv.Cfg.WWWRoot, "cancel-src")
		dstDir := filepath.Join(srv.Cfg.WWWRoot, "cancel-dst")
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			t.Fatal(err)
		}
		const n = 4
		const size = 1 << 20 // 每个 1MB：足够让取消发生在"复制到一半"
		pairs := make([]files.FilePair, 0, n)
		names := make([]string, 0, n)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("m%d.bin", i)
			writeTestFile(t, filepath.Join(srcDir, name), size)
			names = append(names, name)
			pairs = append(pairs, files.FilePair{From: filepath.Join(srcDir, name), To: filepath.Join(dstDir, name)})
		}
		// 注入 EXDEV：同卷临时目录永远触发不到跨卷回退，而那是唯一会"复制后删源"的分支。
		restore := files.SetRenameFuncForTest(func(_, _ string) error { return syscall.EXDEV })
		defer restore()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var copiedBytes int64
		res, err := mgr.MoveItems(ctx, pairs, files.MoveConflictRename, func(p files.OpProgress) {
			if p.Phase == files.OpPhaseMove && p.DoneBytes >= size {
				copiedBytes = p.DoneBytes
				cancel() // 中断发生在复制途中
			}
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("中断必须如实返回 context.Canceled，实际 %v", err)
		}
		if copiedBytes == 0 {
			t.Fatal("取消前没有任何字节进度 —— 这个用例没测到「复制到一半」")
		}
		if res == nil {
			t.Fatal("中断也必须带回部分结果（前端要据此说清「哪些完成了」）")
		}
		// ① 源文件一个都不能少（这是本用例最要紧的断言）。
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(srcDir, name)); err != nil {
				t.Fatalf("中断删掉了源文件 %s：%v（跨卷移动中断绝不能删源）", name, err)
			}
		}
		// ② 半成品不能留在目标目录里。
		ents, rerr := os.ReadDir(dstDir)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(ents) != 0 {
			left := make([]string, 0, len(ents))
			for _, e := range ents {
				left = append(left, e.Name())
			}
			t.Fatalf("目标目录里留下了半成品：%v", left)
		}
		// ③ 汇总要如实说清"哪些没做"。
		if res.Pending == 0 && res.Done == n {
			t.Errorf("中断却报告全部完成：done=%d pending=%d", res.Done, res.Pending)
		}
		if !strings.Contains(res.Msg, "中断") || !strings.Contains(res.Msg, "未处理") {
			t.Errorf("中断汇总必须说清未处理项，实际 %q", res.Msg)
		}
	})

	t.Run("③ 接口：202+task_id / 越界 403 / 空 400，且任务真的干完", func(t *testing.T) {
		srv, _ := newTestServer(t)
		src := filepath.Join(srv.Cfg.WWWRoot, "api-src.bin")
		writeTestFile(t, src, 2048)
		dstDir := filepath.Join(srv.Cfg.WWWRoot, "api-dst")
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dstDir, "api-src.bin")

		// 越界（白名单外）必须当场 403，绝不建一个注定失败的任务。
		rec, _ := postJSON(t, srv.handleFileCopy,
			`{"items":[{"from":"/etc/hosts","to":"`+dst+`"}]}`)
		if rec.Code != 403 {
			t.Errorf("白名单外的复制必须 403，实际 %d", rec.Code)
		}
		// 空请求 400。
		rec, _ = postJSON(t, srv.handleFileCopy, `{}`)
		if rec.Code != 400 {
			t.Errorf("空 items 必须 400，实际 %d", rec.Code)
		}

		// 合法批量复制 → 202 + task_id。
		rec, body := postJSON(t, srv.handleFileCopy,
			`{"items":[{"from":"`+src+`","to":"`+dst+`"}],"on_conflict":"rename"}`)
		if rec.Code != 202 {
			t.Fatalf("复制必须 202 + task_id，实际 %d：%s", rec.Code, rec.Body.String())
		}
		waitFileOpTask(t, srv, taskIDFrom(t, body))
		if _, err := os.Stat(dst); err != nil {
			t.Fatalf("复制任务跑完了但产物不在：%v", err)
		}

		// 删除 → 202，任务跑完后源真的没了。
		rec, body = postJSON(t, srv.handleFileDelete,
			`{"paths":["`+src+`"],"recursive":false}`)
		if rec.Code != 202 {
			t.Fatalf("删除必须 202 + task_id，实际 %d：%s", rec.Code, rec.Body.String())
		}
		waitFileOpTask(t, srv, taskIDFrom(t, body))
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Fatalf("删除任务跑完了但文件还在：%v", err)
		}
	})
}

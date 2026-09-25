package videoopt

// place_gate_test.go —— 「封顶即跳过 + 跳过的文件放进 output/」的唯一门禁。
//
// 为什么现有门禁抓不到：`TestVideoCompressGate` 只断言了 planner 的码率封顶 /
// 不放大 / "产物更大就删掉"，`TestVideoOptionsGate` 只断言"选项 → ffmpeg 命令行" ——
// 两条都**没有任何"跳过项要放进 output"的判据**：跳过就是什么都不做，
// 于是"output/ 缺文件、扩展名被改成 .mp4、跨卷复制写爆磁盘"全都能一路绿。
//
// 一条门禁覆盖整类：① 封顶 ⇒ 跳过且**一次都不转码**（负向对照：不封顶就转码 1 次）；
// ② 硬链接真的指向同一 inode、内容一致、扩展名保留；③ link 失败退复制（负向对照）；
// ④ "产物已存在"既不链接也不复制（负向对照）；⑤《已跳过清单》写对；
// ⑥ 空间不足不复制、不留半个文件。全用 t.TempDir() + 假 Runner，不跑真 ffmpeg、不碰真实文件。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateRunner 是执行器替身：只按脚本写产物，绝不跑 ffmpeg。
type gateRunner struct {
	infos    map[string]MediaInfo
	outBytes int64
	reqs     []TranscodeRequest
}

func (g *gateRunner) Available() error               { return nil }
func (g *gateRunner) Version(context.Context) string { return "fake-gate" }
func (g *gateRunner) Probe(_ context.Context, path string) (MediaInfo, error) {
	if info, ok := g.infos[path]; ok {
		return info, nil
	}
	return MediaInfo{}, fmt.Errorf("没有这个文件的探测结果：%s", path)
}

func (g *gateRunner) Transcode(_ context.Context, req TranscodeRequest, onProgress func(Progress)) error {
	g.reqs = append(g.reqs, req)
	if err := os.MkdirAll(filepath.Dir(req.Dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(req.Dst, bytes.Repeat([]byte{7}, int(g.outBytes)), 0o644)
}

// gateSrc 造一个测试源文件并返回它的路径与内容。
func gateSrc(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// gateInfo 是"码率已到极限"的源信息（预设 480p 下限 800 kbps > 300×0.95=285 ⇒ 封顶）。
func gateInfo(bytes int64, kbps int) MediaInfo {
	return MediaInfo{
		Width: 1280, Height: 720, DurationSec: 6, FileBytes: bytes,
		VideoKbps: kbps, HasVideo: true, HasAudio: false,
	}
}

func gatePlan(t *testing.T, dir, name string, bytes int64, kbps, reqKbps int) (Plan, *gateRunner) {
	t.Helper()
	src := filepath.Join(dir, name)
	runner := &gateRunner{
		infos:    map[string]MediaInfo{src: gateInfo(bytes, kbps)},
		outBytes: 100,
	}
	preset, ok := FindPreset(DefaultPresetID)
	if !ok {
		t.Fatal("默认预设不存在")
	}
	plan, err := BuildPlan(context.Background(), PlanRequest{
		Dir: dir, Options: Options{Preset: preset, KBps: reqKbps},
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rows) != 1 {
		t.Fatalf("应该有 1 行计划，实际 %d", len(plan.Rows))
	}
	return plan.Rows[0], runner
}

// TestVideoSkipPlaceGate 是这一条门禁。
func TestVideoSkipPlaceGate(t *testing.T) {
	t.Run("① 被封顶的文件标跳过且一次都不转码（负向对照：不封顶就转码 1 次）", func(t *testing.T) {
		dir := t.TempDir()
		content := bytes.Repeat([]byte{1}, 4096)
		gateSrc(t, dir, "low.mp4", content)
		row, runner := gatePlan(t, dir, "low.mp4", int64(len(content)), 400, 0)

		if row.SkipReason == "" || !strings.Contains(row.SkipReason, "码率") {
			t.Fatalf("封顶的文件必须标跳过且原因含「码率」，实际 %q", row.SkipReason)
		}
		if !row.Capped || !row.PlaceInOutput {
			t.Errorf("必须标记 Capped 且要原样放进 output：Capped=%v PlaceInOutput=%v", row.Capped, row.PlaceInOutput)
		}
		if row.Runnable() {
			t.Error("标了跳过就不能再 Runnable")
		}
		res, err := RunPlan(context.Background(), filepath.Dir(row.OutPath), []Plan{row}, runner, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		// 负向对照：删掉"封顶即跳过"，这里会变成 1 次 ⇒ 变红。
		if len(runner.reqs) != 0 {
			t.Fatalf("封顶即跳过：转码调用次数必须是 0，实际 %d（%+v）", len(runner.reqs), runner.reqs)
		}
		if res.Done != 0 || res.Skipped != 1 || res.Placed != 1 {
			t.Errorf("应 0 压缩 / 1 跳过 / 1 放入 output，实际 done=%d skipped=%d placed=%d",
				res.Done, res.Skipped, res.Placed)
		}

		// 负向对照的另一半：请求码率压到 300（< 400×0.95=380）就不再封顶 ⇒ 真的转码 1 次。
		dir2 := t.TempDir()
		gateSrc(t, dir2, "low.mp4", content)
		row2, runner2 := gatePlan(t, dir2, "low.mp4", int64(len(content)), 400, 300)
		if !row2.Runnable() {
			t.Fatalf("请求码率 300 < 380 时不该封顶，实际 %q", row2.SkipReason)
		}
		if _, err := RunPlan(context.Background(), filepath.Dir(row2.OutPath), []Plan{row2}, runner2, Hooks{}); err != nil {
			t.Fatal(err)
		}
		if len(runner2.reqs) != 1 {
			t.Fatalf("负向对照：不封顶时必须转码 1 次，实际 %d", len(runner2.reqs))
		}
	})

	t.Run("② 放进 output 的是硬链接：同一 inode、内容一致、保留 .mkv", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("low-bitrate-mkv-content")
		src := gateSrc(t, dir, "a.mkv", content)
		row, runner := gatePlan(t, dir, "a.mkv", int64(len(content)), 300, 0)

		res, err := RunPlan(context.Background(), filepath.Dir(row.OutPath), []Plan{row}, runner, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		placed := filepath.Join(dir, OutputDirName, "a.480p.mkv")
		if _, err := os.Stat(placed); err != nil {
			t.Fatalf("跳过的文件必须原样放进 output：%v", err)
		}
		// 扩展名/容器必须保留：绝不能把 mkv 内容改名成 .mp4（那是谎报格式）。
		if _, err := os.Stat(filepath.Join(dir, OutputDirName, "a.480p.mp4")); !os.IsNotExist(err) {
			t.Error("绝不能把 mkv 内容放进 .mp4 文件名")
		}
		s1, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := os.Stat(placed)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(s1, s2) {
			t.Error("硬链接必须与源文件指向同一 inode（os.SameFile）")
		}
		got, err := os.ReadFile(placed)
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("放入 output 的内容必须与源一致：err=%v", err)
		}
		if res.Items[0].Placement != PlacementLink {
			t.Errorf("放法必须是 link，实际 %q（reason=%q）", res.Items[0].Placement, res.Items[0].PlaceReason)
		}
	})

	t.Run("③ 硬链接失败 ⇒ 退回复制、内容一致（负向对照：删掉回退这条就红）", func(t *testing.T) {
		restore := linkFile
		linkFile = func(_, _ string) error { return fmt.Errorf("EXDEV: cross-device link") }
		t.Cleanup(func() { linkFile = restore })

		dir := t.TempDir()
		content := []byte("copy-fallback-content")
		src := gateSrc(t, dir, "b.mkv", content)
		row, runner := gatePlan(t, dir, "b.mkv", int64(len(content)), 300, 0)

		res, err := RunPlan(context.Background(), filepath.Dir(row.OutPath), []Plan{row}, runner, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		placed := filepath.Join(dir, OutputDirName, "b.480p.mkv")
		got, rerr := os.ReadFile(placed)
		// 负向对照：把 placeFile 里的复制回退删掉，这里会读不到文件 ⇒ 变红。
		if rerr != nil || !bytes.Equal(got, content) {
			t.Fatalf("link 失败必须退回复制且内容一致：err=%v", rerr)
		}
		s1, _ := os.Stat(src)
		s2, _ := os.Stat(placed)
		if os.SameFile(s1, s2) {
			t.Error("复制出来的文件不该与源同 inode")
		}
		if res.Items[0].Placement != PlacementCopy {
			t.Errorf("放法必须是 copy，实际 %q", res.Items[0].Placement)
		}
		if res.Placed != 1 {
			t.Errorf("复制成功也要计入 placed，实际 %d", res.Placed)
		}
	})

	t.Run("④ 产物已存在 ⇒ 既不链接也不复制（负向对照：无视该判据就多出 a.480p.mkv）", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("source-content-should-not-be-placed")
		src := gateSrc(t, dir, "a.mkv", content)
		outDir := filepath.Join(dir, OutputDirName)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		existing := filepath.Join(outDir, "a.480p.mp4")
		if err := os.WriteFile(existing, []byte("already-compressed"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 不封顶的源（2000 kbps）：走到的是"产物已存在"这条判据。
		row, runner := gatePlan(t, dir, "a.mkv", int64(len(content)), 2000, 0)
		if !strings.Contains(row.SkipReason, "产物已存在") {
			t.Fatalf("应是「产物已存在」跳过，实际 %q", row.SkipReason)
		}
		if row.PlaceInOutput {
			t.Error("产物已存在时不该再要求放入 output")
		}
		res, err := RunPlan(context.Background(), outDir, []Plan{row}, runner, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.reqs) != 0 {
			t.Errorf("产物已存在就不该转码，实际 %d 次", len(runner.reqs))
		}
		if res.Items[0].Placement != PlacementNone {
			t.Errorf("产物已存在时 placement 必须是 none，实际 %q", res.Items[0].Placement)
		}
		// 负向对照：把 PlaceInOutput 判据当空气，这里会多出一个硬链接 ⇒ 变红。
		if _, err := os.Stat(filepath.Join(outDir, "a.480p.mkv")); !os.IsNotExist(err) {
			t.Error("产物已存在时不能再链接/复制源文件")
		}
		if got, _ := os.ReadFile(existing); string(got) != "already-compressed" {
			t.Errorf("已有产物必须原样不动，实际 %q", string(got))
		}
		s1, _ := os.Stat(src)
		s2, _ := os.Stat(existing)
		if os.SameFile(s1, s2) {
			t.Error("已有产物被源文件的硬链接覆盖了")
		}
	})

	t.Run("⑤ output/ 里的《已跳过清单》含文件名与原因", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("listed-in-the-txt")
		gateSrc(t, dir, "c.mkv", content)
		row, runner := gatePlan(t, dir, "c.mkv", int64(len(content)), 300, 0)
		if _, err := RunPlan(context.Background(), filepath.Dir(row.OutPath), []Plan{row}, runner, Hooks{}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, OutputDirName, SkippedListName))
		if err != nil {
			t.Fatalf("任务结束必须写《%s》：%v", SkippedListName, err)
		}
		txt := string(raw)
		for _, want := range []string{"c.mkv", "码率", "原相对路径", "硬链接"} {
			if !strings.Contains(txt, want) {
				t.Errorf("清单里必须含 %q，实际内容：\n%s", want, txt)
			}
		}
	})

	t.Run("⑥ 空间不足 ⇒ 不复制、如实回报、不留半个文件", func(t *testing.T) {
		restoreLink, restoreFree := linkFile, freeBytes
		linkFile = func(_, _ string) error { return fmt.Errorf("EXDEV") }
		freeBytes = func(string) (int64, bool) { return 1, true } // 只剩 1 字节
		t.Cleanup(func() { linkFile, freeBytes = restoreLink, restoreFree })

		dir := t.TempDir()
		content := bytes.Repeat([]byte{9}, 2048)
		gateSrc(t, dir, "d.mkv", content)
		row, runner := gatePlan(t, dir, "d.mkv", int64(len(content)), 300, 0)
		res, err := RunPlan(context.Background(), filepath.Dir(row.OutPath), []Plan{row}, runner, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Items[0].Placement != PlacementNone {
			t.Errorf("空间不足时不能放进去，实际 placement=%q", res.Items[0].Placement)
		}
		if !strings.Contains(res.Items[0].PlaceReason, "空间不足") {
			t.Errorf("空间不足必须如实说明，实际 %q", res.Items[0].PlaceReason)
		}
		placed := filepath.Join(dir, OutputDirName, "d.480p.mkv")
		if _, err := os.Stat(placed); !os.IsNotExist(err) {
			t.Error("空间不足时绝不能留下目标文件")
		}
		if _, err := os.Stat(placed + ".part"); !os.IsNotExist(err) {
			t.Error("绝不能留下半个文件（.part）")
		}
		if res.Placed != 0 {
			t.Errorf("没放进去就不能计入 placed，实际 %d", res.Placed)
		}
	})
}

package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
//  结构化进度 / 日志（2026-09-23：升级下载必须有真实进度）
// ---------------------------------------------------------------------------

// TestStateWriterRecordsRealBytesAndSpeed 锁住"进度来自真实字节数与真实时间"。
//
// 这里不涉及任何 HTTP：StateWriter 只接收调用方喂进来的真实写入字节，
// 所以断言可以直接对着数字看 —— 如果哪天有人改成"按时间递增百分比"，
// 用固定时钟的这条测试会立刻失败。
func TestStateWriterRecordsRealBytesAndSpeed(t *testing.T) {
	work := t.TempDir()
	clock := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

	sw := NewStateWriter(work, &State{Status: StatusDownloading})
	sw.saveInterval = 0 // 单测不需要节流
	sw.now = func() time.Time { return clock }

	sw.StartDownload(1000)
	sw.Stage(StageDownload)

	sw.SetDownloaded(250, 1000)
	p := sw.State().Progress
	if p.DownloadedBytes != 250 || p.TotalBytes != 1000 {
		t.Fatalf("字节数应为 250/1000，实际 %d/%d", p.DownloadedBytes, p.TotalBytes)
	}
	if p.Percent != 25 {
		t.Errorf("250/1000 的百分比应为 25，实际 %v", p.Percent)
	}
	if p.BytesPerSecond != 0 {
		t.Errorf("第一个采样点不应给出速度（还没有时间差），实际 %v", p.BytesPerSecond)
	}

	clock = clock.Add(time.Second)
	sw.SetDownloaded(500, 1000)
	p = sw.State().Progress
	if p.Percent != 50 {
		t.Errorf("500/1000 的百分比应为 50，实际 %v", p.Percent)
	}
	// 平均速度 = (500-0) 真实字节 ÷ 1 秒真实时间。
	if p.BytesPerSecond != 500 {
		t.Errorf("平均速度应为 500 B/s，实际 %v", p.BytesPerSecond)
	}

	// 必须落盘：轮询接口读的是磁盘上的 state.json。
	reloaded := LoadState(work)
	if reloaded.Progress == nil {
		t.Fatal("重新加载后 progress 丢了 —— 进度没有落盘，刷新页面就看不到了")
	}
	if reloaded.Progress.DownloadedBytes != 500 || reloaded.Progress.TotalBytes != 1000 {
		t.Errorf("落盘后字节数应为 500/1000，实际 %d/%d",
			reloaded.Progress.DownloadedBytes, reloaded.Progress.TotalBytes)
	}
}

// TestStateWriterUnknownTotalStaysUnknown 锁住"总大小未知时不编造百分比"。
func TestStateWriterUnknownTotalStaysUnknown(t *testing.T) {
	sw := NewStateWriter(t.TempDir(), &State{Status: StatusDownloading})
	sw.saveInterval = 0

	sw.StartDownload(0)
	sw.SetDownloaded(100, 0)
	if p := sw.State().Progress; p.Percent != -1 || p.TotalBytes != 0 {
		t.Fatalf("总大小未知时应为 percent=-1/total=0，实际 %v/%d", p.Percent, p.TotalBytes)
	}

	// 服务端随后给了 Content-Length（回调的 total）→ 采用它并算出真实百分比。
	sw.SetDownloaded(100, 400)
	p := sw.State().Progress
	if p.TotalBytes != 400 || p.Percent != 25 {
		t.Errorf("回落到 Content-Length 后应为 400/25%%，实际 %d/%v", p.TotalBytes, p.Percent)
	}
}

// TestStateWriterKeepsOnlyRecentLogs 锁住日志上限，避免 state.json 无限增长。
func TestStateWriterKeepsOnlyRecentLogs(t *testing.T) {
	sw := NewStateWriter(t.TempDir(), &State{Status: StatusDownloading})
	sw.saveInterval = 0
	for i := 0; i < maxLogLines+30; i++ {
		sw.Logf("info", "第 %d 行", i)
	}
	logs := sw.State().Logs
	if len(logs) != maxLogLines {
		t.Fatalf("日志应被截到 %d 行，实际 %d", maxLogLines, len(logs))
	}
	// 保留的必须是**最新**的：最后一行是最后写进去的那条。
	if logs[len(logs)-1].Text != "第 "+strconv.Itoa(maxLogLines+29)+" 行" {
		t.Errorf("最后一行日志不对：%q", logs[len(logs)-1].Text)
	}
	if logs[0].Text != "第 30 行" {
		t.Errorf("第一行日志应为被保留下来的最旧一条（第 30 行），实际 %q", logs[0].Text)
	}
}

// TestStateWriterFailKeepsReason 锁住"失败原因必须写在用户看得到的地方"。
func TestStateWriterFailKeepsReason(t *testing.T) {
	work := t.TempDir()
	sw := NewStateWriter(work, &State{Status: StatusDownloading})
	sw.saveInterval = 0
	sw.Fail(errors.New("升级包校验和不匹配"))

	st := LoadState(work)
	if st.Error != "升级包校验和不匹配" {
		t.Errorf("失败原因应写进 st.Error，实际 %q", st.Error)
	}
	if st.Progress == nil || st.Progress.Stage != StageFailed {
		t.Errorf("失败后阶段应为 %s，实际 %+v", StageFailed, st.Progress)
	}
	if len(st.Logs) == 0 || st.Logs[len(st.Logs)-1].Level != "error" {
		t.Errorf("失败必须留下一条 error 日志，实际 %+v", st.Logs)
	}
}

// TestStageLabelsCoverAllStages 保证每个阶段 id 都有中文名 ——
// 前端直接显示 StageLabel，漏一个就会在界面上出现英文 id。
func TestStageLabelsCoverAllStages(t *testing.T) {
	for _, s := range []string{
		StageManifest, StageDownload, StageVerify, StageExtract,
		StageSmoke, StageApply, StageRestart, StageDone, StageFailed,
	} {
		if StageLabel(s) == s {
			t.Errorf("阶段 %q 没有中文名", s)
		}
	}
}

// TestDownloadObserverReportsVerifyPhase 锁住"校验"是一个真实上报的阶段。
//
// 没有它，进度条会停在 100% 一动不动 —— 用户正是在这种情况下刷新重试的。
func TestDownloadObserverReportsVerifyPhase(t *testing.T) {
	payload := []byte("zizpanel fake tarball payload for progress test")
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.tar.gz")
	var lastWritten int64
	var phases []string

	got, err := DownloadTarballWithObserver(
		t.Context(), srv.URL+"/pkg.tar.gz", dest, want,
		func(written, total int64) { lastWritten = written },
		func(phase, _ string) { phases = append(phases, phase) },
	)
	if err != nil {
		t.Fatalf("下载应成功：%v", err)
	}
	if got != dest {
		t.Errorf("返回路径应为 %s，实际 %s", dest, got)
	}
	if lastWritten != int64(len(payload)) {
		t.Errorf("进度回调的最终字节数应为 %d，实际 %d", len(payload), lastWritten)
	}
	if len(phases) != 1 || phases[0] != PhaseVerify {
		t.Errorf("应如实上报一次 %s 阶段，实际 %v", PhaseVerify, phases)
	}
}

// TestStateJSONBackwardCompatible 保证"加字段不破坏老前端/老状态"。
//
// 老前端读不到 progress/logs 只会忽略；老 state.json（没有这两个字段）
// 反序列化后必须是零值而不是报错。
func TestStateJSONBackwardCompatible(t *testing.T) {
	old := `{"status":"downloading","to":"1.0.1","message":"已下载 1.0 MB"}`
	var st State
	if err := json.Unmarshal([]byte(old), &st); err != nil {
		t.Fatalf("老状态 JSON 必须仍能解析：%v", err)
	}
	if st.Progress != nil || st.Logs != nil {
		t.Errorf("老状态里不应凭空出现 progress/logs：%+v %+v", st.Progress, st.Logs)
	}
	if st.Status != StatusDownloading {
		t.Errorf("status 应保持 downloading，实际 %s", st.Status)
	}
	// 新字段必须带 omitempty：老前端拿到的 JSON 结构不发生任何变化。
	b, err := json.Marshal(&State{Status: StatusIdle})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["progress"]; ok {
		t.Error("没有进度时 JSON 里不应出现 progress 键")
	}
	if _, ok := m["logs"]; ok {
		t.Error("没有日志时 JSON 里不应出现 logs 键")
	}
}

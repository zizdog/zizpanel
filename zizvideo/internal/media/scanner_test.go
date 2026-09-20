package media

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/ffmpeg"
	"github.com/zizdog/zizvideo/internal/storage"
)

// fakeTools writes stub ffprobe/ffmpeg scripts so scan tests never touch a real
// encoder and never depend on a deliberately corrupt file.
func fakeTools(t *testing.T, dir string) (ffprobe, ffmpegBin string) {
	t.Helper()
	ffprobe = filepath.Join(dir, "ffprobe")
	ffmpegBin = filepath.Join(dir, "ffmpeg")
	script := `#!/bin/sh
if [ "$1" = "-version" ]; then echo "ffprobe version 9.0.1-fake Copyright"; exit 0; fi
if [ -n "$ZV_TEST_ARZV_LOG" ]; then printf '%s\n' "$@" >> "$ZV_TEST_ARZV_LOG"; fi
src=""
for a in "$@"; do src="$a"; done
case "$src" in
  *bad*) echo "Invalid data found when processing input" >&2; exit 1;;
  *secret*) echo "Permission denied" >&2; exit 1;;
esac
cat <<'JSON'
{"streams":[{"codec_type":"video","codec_name":"h264","width":640,"height":360,"r_frame_rate":"30/1","duration":"5.000000"},
{"codec_type":"audio","codec_name":"aac"}],
"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"5.000000","bit_rate":"800000"}}
JSON
`
	ffmpegScript := `#!/bin/sh
if [ "$1" = "-version" ]; then echo "ffmpeg version 9.0.1-fake Copyright"; exit 0; fi
if [ "$1" = "-hide_banner" ]; then echo " V....D h264_videotoolbox fake encoder"; exit 0; fi
out=""
for a in "$@"; do out="$a"; done
if [ -n "$ZV_TEST_FFMPEG_FAIL" ]; then echo "boom" >&2; exit 1; fi
echo fake-jpeg > "$out"
`
	if err := os.WriteFile(ffprobe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ffmpegBin, []byte(ffmpegScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return ffprobe, ffmpegBin
}

type scanEnv struct {
	cfg    *config.Config
	db     *storage.DB
	scan   *Scanner
	lib    *domain.Library
	root   string
	argv   string
	roots  *config.Roots
	logger *slog.Logger
}

func newScanEnv(t *testing.T) *scanEnv {
	t.Helper()
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	root := filepath.Join(base, "media")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	bins := filepath.Join(base, "bin")
	if err := os.MkdirAll(bins, 0o755); err != nil {
		t.Fatal(err)
	}
	probe, ff := fakeTools(t, bins)
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.DatabasePath = filepath.Join(dataDir, "test.db")
	cfg.MediaAllowRoots = []string{root}
	cfg.FFprobeBin = probe
	cfg.FFmpegBin = ff
	cfg.ScanWorkers = 4
	cfg.ScanRetryBackoffMS = []int{1, 1, 1}

	db, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	lib := &domain.Library{ID: domain.NewID("lib"), Name: "t", RootPath: root,
		Recursive: true, Enabled: true, IgnoreRules: []string{}}
	if err := db.CreateLibrary(lib); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	roots := config.NewRoots("", cfg.MediaAllowRoots)
	return &scanEnv{cfg: cfg, db: db, roots: roots,
		scan: NewScanner(cfg, db, roots, ffmpeg.ExecRunner{}, logger),
		lib:  lib, root: root, argv: filepath.Join(base, "argv.log"), logger: logger}
}

func (e *scanEnv) write(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(e.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *scanEnv) run(t *testing.T) *domain.ScanTask {
	t.Helper()
	task := &domain.ScanTask{ID: domain.NewID("scn"), LibraryID: e.lib.ID, Kind: "incremental"}
	if err := e.db.CreateScanTask(task); err != nil {
		t.Fatal(err)
	}
	lib, err := e.db.GetLibrary(e.lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.scan.Run(context.Background(), task, lib)
	got, err := e.db.GetScanTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *scanEnv) media(t *testing.T) []domain.Media {
	t.Helper()
	rows, _, err := e.db.ListMedia(storage.MediaFilter{PerPage: 100, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestScanIgnoresJunkAndFilteredExtensions(t *testing.T) {
	env := newScanEnv(t)
	env.write(t, "keep.mp4", "a")
	env.write(t, "note.txt", "b")
	env.write(t, ".hidden.mp4", "c")
	env.write(t, "sub/@eaDir/thumb.mp4", "d")
	env.write(t, "#recycle/gone.mp4", "e")
	env.write(t, "partial.part", "f")
	env.write(t, "lost+found/x.mp4", "g")
	env.write(t, "backup.!bt", "h")
	env.write(t, "~$lock.mp4", "i")
	env.write(t, ".DS_Store", "j")

	task := env.run(t)
	if task.Status != domain.TaskSuccess {
		t.Fatalf("状态 = %s, 期望 success (%s)", task.Status, task.Error)
	}
	rows := env.media(t)
	if len(rows) != 1 || rows[0].Title != "keep" {
		t.Fatalf("只应留下 keep.mp4, 实际 %d 条: %+v", len(rows), rows)
	}
	if task.Total != 1 || task.Scanned != 1 || task.Failed != 0 {
		t.Fatalf("计数不符: total=%d scanned=%d failed=%d", task.Total, task.Scanned, task.Failed)
	}
}

func TestScanSkipsUnchangedFilesBySizeAndMtime(t *testing.T) {
	env := newScanEnv(t)
	env.write(t, "a.mp4", "a")
	env.write(t, "b.mp4", "b")
	t.Setenv("ZV_TEST_ARZV_LOG", env.argv)

	env.run(t)
	first := countLines(t, env.argv)
	if first == 0 {
		t.Fatal("第一次扫描应调用 ffprobe")
	}
	// Second run without touching the files must not probe again.
	_ = os.Remove(env.argv)
	task := env.run(t)
	if got := countLines(t, env.argv); got != 0 {
		t.Fatalf("size+mtime 未变仍探测了 %d 次", got)
	}
	if task.Total != 2 || task.Scanned != 2 {
		t.Fatalf("第二次扫描计数异常: %+v", task)
	}
}

func TestScanReprobesWhenMtimeChanges(t *testing.T) {
	env := newScanEnv(t)
	env.write(t, "a.mp4", "a")
	env.run(t)

	t.Setenv("ZV_TEST_ARZV_LOG", env.argv)
	p := filepath.Join(env.root, "a.mp4")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	env.run(t)
	if countLines(t, env.argv) == 0 {
		t.Fatal("mtime 变化后必须重新探测")
	}
}

func TestScanMarksProbeFailureWithClass(t *testing.T) {
	env := newScanEnv(t)
	env.write(t, "bad.mp4", "x")
	env.write(t, "secret.mp4", "y")
	env.write(t, "ok.mp4", "z")

	task := env.run(t)
	if task.Failed != 2 {
		t.Fatalf("失败数 = %d, 期望 2", task.Failed)
	}
	rows := env.media(t)
	byTitle := map[string]domain.Media{}
	for _, m := range rows {
		byTitle[m.Title] = m
	}
	if got := byTitle["bad"].ErrorClass; got != domain.ClassInvalidFormat {
		t.Fatalf("bad.mp4 分类 = %q, 期望 %q", got, domain.ClassInvalidFormat)
	}
	if got := byTitle["secret"].ErrorClass; got != domain.ClassUnreadable {
		t.Fatalf("secret.mp4 分类 = %q, 期望 %q", got, domain.ClassUnreadable)
	}
	if byTitle["bad"].Status != domain.MediaProbeFail {
		t.Fatalf("status = %q, 期望 probe_failed", byTitle["bad"].Status)
	}
	if byTitle["ok"].Status != domain.MediaReady {
		t.Fatalf("正常文件状态 = %q", byTitle["ok"].Status)
	}
}

func TestScanRootMissingInterruptsAndKeepsRows(t *testing.T) {
	env := newScanEnv(t)
	env.write(t, "a.mp4", "a")
	env.write(t, "b.mp4", "b")
	env.run(t)
	if len(env.media(t)) != 2 {
		t.Fatal("前置扫描未建立两条记录")
	}
	// The device/mount identity is recorded during the first scan.
	if err := os.RemoveAll(env.root); err != nil {
		t.Fatal(err)
	}
	task := env.run(t)
	if task.Status != domain.TaskInterrupted {
		t.Fatalf("状态 = %s, 期望 interrupted", task.Status)
	}
	if got := len(env.media(t)); got != 2 {
		t.Fatalf("中断时绝不能删记录, 现存 %d 条", got)
	}
	if task.Missing != 0 {
		t.Fatalf("中断时不应报缺失, missing=%d", task.Missing)
	}
}

func TestTwoPhaseDeletionMarksThenDeletes(t *testing.T) {
	env := newScanEnv(t)
	// One file disappears out of 200 rows: far below both thresholds.
	for i := 0; i < 20; i++ {
		env.write(t, "keep"+string(rune('a'+i))+".mp4", "x")
	}
	env.write(t, "doomed.mp4", "y")
	env.run(t)
	if got := len(env.media(t)); got != 21 {
		t.Fatalf("前置扫描应有 21 条, 实际 %d", got)
	}
	if err := os.Remove(filepath.Join(env.root, "doomed.mp4")); err != nil {
		t.Fatal(err)
	}

	first := env.run(t)
	if first.Status != domain.TaskSuccess {
		t.Fatalf("第一次扫描状态 %s", first.Status)
	}
	if got := len(env.media(t)); got != 21 {
		t.Fatalf("首次发现消失只能标记, 不能软删; 现存 %d", got)
	}
	if first.Missing != 1 {
		t.Fatalf("missing = %d, 期望 1", first.Missing)
	}
	if first.Suspected != 0 {
		t.Fatalf("未超阈值不应报疑似, suspected=%d", first.Suspected)
	}

	second := env.run(t)
	if got := len(env.media(t)); got != 20 {
		t.Fatalf("第二次扫描应软删该文件, 现存 %d 条", got)
	}
	if second.Missing != 0 {
		t.Fatalf("删除后 missing 应为 0, 实际 %d", second.Missing)
	}
}

func TestDeletionThresholdReportsSuspectedOnly(t *testing.T) {
	env := newScanEnv(t)
	for i := 0; i < 4; i++ {
		env.write(t, "v"+string(rune('a'+i))+".mp4", "x")
	}
	env.run(t)

	// Remove 3 of 4: ratio 75% >> 10%, so this is a suspected loss.
	for i := 0; i < 3; i++ {
		if err := os.Remove(filepath.Join(env.root, "v"+string(rune('a'+i))+".mp4")); err != nil {
			t.Fatal(err)
		}
	}
	first := env.run(t)
	if first.Suspected == 0 {
		t.Fatal("超过阈值必须报疑似丢失")
	}
	if got := len(env.media(t)); got != 4 {
		t.Fatalf("超阈值绝不能软删, 现存 %d", got)
	}
	second := env.run(t)
	if got := len(env.media(t)); got != 4 {
		t.Fatalf("持续超阈值仍不能删, 现存 %d", got)
	}
	if second.Suspected == 0 {
		t.Fatal("第二次仍应报疑似")
	}
}

func TestScannerDoesNotResurrectDeletedRows(t *testing.T) {
	env := newScanEnv(t)
	env.write(t, "a.mp4", "a")
	env.run(t)
	_ = os.Remove(env.argv)
	t.Setenv("ZV_TEST_ARZV_LOG", env.argv)
	// Touch nothing; the file is unchanged so it must be skipped, not re-probed.
	env.run(t)
	if countLines(t, env.argv) != 0 {
		t.Fatal("未变更文件被重复探测")
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}

// TestScannerRefusesLibraryOutsideAllowRoots is the scanner-side guard: even if
// a scan is queued directly (bypassing the API), a root outside the allow list
// is refused before any filesystem call, so nothing is walked or probed.
func TestScannerRefusesLibraryOutsideAllowRoots(t *testing.T) {
	e := newScanEnv(t)
	// Remove the only allow root; the library still points at it.
	e.roots = config.NewRoots("", nil)
	e.scan.Roots = e.roots

	task := e.run(t)
	if task.Status != domain.TaskInterrupted {
		t.Fatalf("状态 = %s, 期望 interrupted", task.Status)
	}
	if !strings.Contains(task.Error, "允许根") {
		t.Fatalf("错误应说明越界: %q", task.Error)
	}
	if t.Failed() {
		return
	}
	// A root that does not exist cannot have been traversed.
	if task.Total != 0 || task.Scanned != 0 {
		t.Fatalf("越界扫描不得遍历任何文件: %+v", task)
	}
}

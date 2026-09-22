package dirimport_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizvideo/internal/dirimport"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exts(ext string) bool { return ext == ".mp4" || ext == ".mkv" }

// 门禁：识别只走 detect（S01E01 认、SP01 不认），depth 控制下钻层数。
func TestScanDepthAndDetection(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "S01E01.mp4"))
	write(t, filepath.Join(root, "S01E02.mp4"))
	write(t, filepath.Join(root, "SP01.mkv"))
	write(t, filepath.Join(root, "乱名.mp4"))
	write(t, filepath.Join(root, "sub", "S01E03.mp4"))
	write(t, filepath.Join(root, "sub", "deeper", "S01E04.mp4"))
	write(t, filepath.Join(root, "readme.txt")) // 扩展名白名单外

	files, err := dirimport.Scan(root, 0, 100, exts)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("depth=0 应只看到本层 4 个视频，得到 %d", len(files))
	}
	recognized := 0
	for _, f := range files {
		if f.Recognized {
			recognized++
		}
	}
	if recognized != 2 {
		t.Fatalf("本层识别到 %d 集，期望 2（S01E01/S01E02；SP01 与乱名必须未识别）", recognized)
	}

	files, err = dirimport.Scan(root, 1, 100, exts)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("depth=1 应有 5 个视频（含 sub/），得到 %d", len(files))
	}

	files, err = dirimport.Scan(root, dirimport.Unlimited, 100, exts)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 6 {
		t.Fatalf("Unlimited 应有 6 个视频，得到 %d", len(files))
	}
}

// 负向对照：超限必须报错，不许截断后当成功。
func TestScanRejectsOverLimit(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"S01E01.mp4", "S01E02.mp4", "S01E03.mp4"} {
		write(t, filepath.Join(root, name))
	}
	write(t, filepath.Join(root, "sub", "x.mp4"))
	if _, err := dirimport.Scan(root, 0, 2, exts); !errors.Is(err, dirimport.ErrTooManyFiles) {
		t.Fatalf("超过 2 个应返回 ErrTooManyFiles，得到 %v", err)
	}
	if _, err := dirimport.ListSubdirs(root, 0); !errors.Is(err, dirimport.ErrTooManyDirs) {
		t.Fatalf("子目录超限应返回 ErrTooManyDirs，得到 %v", err)
	}
}

// 门禁：Count 只计数不保留路径，且超限要如实标记 over_limit。
func TestCountFlagsOverLimit(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "S01E01.mp4"))
	write(t, filepath.Join(root, "a", "乱名.mp4"))
	write(t, filepath.Join(root, "b", "S01E01.mp4"))

	subs, err := dirimport.ListSubdirs(root, 10)
	if err != nil || len(subs) != 2 {
		t.Fatalf("一级子目录 = %v (err=%v)，期望 2", subs, err)
	}
	counts, err := dirimport.Count(filepath.Join(root, "a"), dirimport.Unlimited, 10, exts)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files != 2 || counts.Recognized != 1 || counts.Unidentified != 1 || counts.OverLimit {
		t.Fatalf("计数不对: %+v", counts)
	}
	counts, err = dirimport.Count(filepath.Join(root, "a"), dirimport.Unlimited, 1, exts)
	if !errors.Is(err, dirimport.ErrTooManyFiles) || !counts.OverLimit {
		t.Fatalf("超限应 ErrTooManyFiles + OverLimit: %+v (%v)", counts, err)
	}
}

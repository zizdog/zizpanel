package files

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// ============================================================================
//  Move（剪切粘贴）语义
//
//  用户批准的「能力对齐宝塔」里，剪切粘贴走后端新增的 /files/move：
//    · 同卷优先 os.Rename（原子、不复制数据）；
//    · 跨卷（EXDEV）回退 copy + delete，并**如实**报告用的是哪种方式；
//    · 目标已存在时按用户选择 rename / overwrite / skip，绝不静默覆盖。
// ============================================================================

func TestMoveSameVolumeRenames(t *testing.T) {
	m, root := newTestManager(t)
	src := filepath.Join(root, "a.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(sub, "a.txt")

	res, err := m.Move(src, dst, "")
	if err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if res.Way != MoveStrategyRename {
		t.Fatalf("同卷移动的方式应为 rename，实际 %q", res.Way)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "data" {
		t.Fatalf("目标内容不正确: %q (%v)", string(b), err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("移动后源文件应已消失")
	}
}

func TestMoveConflictRenameKeepsBoth(t *testing.T) {
	m, root := newTestManager(t)
	from := filepath.Join(root, "from.txt")
	to := filepath.Join(root, "to.txt")
	_ = os.WriteFile(from, []byte("new"), 0o644)
	_ = os.WriteFile(to, []byte("old"), 0o644)

	res, err := m.Move(from, to, MoveConflictRename)
	if err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if res.To == to {
		t.Fatal("重名时不应直接覆盖目标，应自动改名")
	}
	if b, _ := os.ReadFile(to); string(b) != "old" {
		t.Fatalf("原有文件被改动了: %q", string(b))
	}
	if b, _ := os.ReadFile(res.To); string(b) != "new" {
		t.Fatalf("改名后的文件内容不正确: %q", string(b))
	}
}

func TestMoveConflictSkip(t *testing.T) {
	m, root := newTestManager(t)
	from := filepath.Join(root, "from.txt")
	to := filepath.Join(root, "to.txt")
	_ = os.WriteFile(from, []byte("new"), 0o644)
	_ = os.WriteFile(to, []byte("old"), 0o644)

	res, err := m.Move(from, to, MoveConflictSkip)
	if err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if !res.Skipped {
		t.Fatal("skip 策略应标记为已跳过")
	}
	if _, err := os.Stat(from); err != nil {
		t.Fatal("跳过时源文件必须原样保留")
	}
	if b, _ := os.ReadFile(to); string(b) != "old" {
		t.Fatalf("跳过时目标不应被改动: %q", string(b))
	}
}

func TestMoveConflictOverwrite(t *testing.T) {
	m, root := newTestManager(t)
	from := filepath.Join(root, "from.txt")
	to := filepath.Join(root, "to.txt")
	_ = os.WriteFile(from, []byte("new"), 0o644)
	_ = os.WriteFile(to, []byte("old"), 0o644)

	res, err := m.Move(from, to, MoveConflictOverwrite)
	if err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if !res.Overwritten {
		t.Fatal("overwrite 策略应标记为已覆盖")
	}
	if b, _ := os.ReadFile(to); string(b) != "new" {
		t.Fatalf("覆盖后目标内容不正确: %q", string(b))
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatal("覆盖移动后源文件应已消失")
	}
}

// 跨卷回退：注入一个永远返回 EXDEV 的 rename，验证"复制 + 删除源"这条路真的能走通。
//
// 单测里所有临时目录都在同一卷上，不注入就永远触发不到 EXDEV —— 而它恰恰是
// 移动语义里最容易写错的分支（复制一半失败、删源失败都会留下不一致状态）。
func TestMoveCrossVolumeFallsBackToCopyDelete(t *testing.T) {
	m, root := newTestManager(t)
	src := filepath.Join(root, "tree")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "moved")

	prev := renameFunc
	renameFunc = func(a, b string) error {
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFunc = prev })

	res, err := m.Move(src, dst, "")
	if err != nil {
		t.Fatalf("跨卷移动失败: %v", err)
	}
	if res.Way != MoveStrategyCopyDelete {
		t.Fatalf("跨卷移动的方式应为 copy+delete，实际 %q", res.Way)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "sub", "x.txt")); err != nil || string(b) != "x" {
		t.Fatalf("跨卷移动后目标内容不正确: %q (%v)", string(b), err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("跨卷移动成功后源目录应已删除")
	}
}

func TestMoveRefusesSelfNesting(t *testing.T) {
	m, root := newTestManager(t)
	dir := filepath.Join(root, "parent")
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move(dir, filepath.Join(dir, "child", "moved"), ""); err == nil {
		t.Fatal("把目录移动到自己的子目录应被拒绝")
	}
}

// 越界移动必须返回 ErrForbidden（web 层据此回 403），且源文件不受影响。
func TestMoveOutsideRootForbidden(t *testing.T) {
	m, root := newTestManager(t)
	from := filepath.Join(root, "a.txt")
	if err := os.WriteFile(from, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move(from, "/etc/zp-move-should-not-exist.txt", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("越界移动应返回 ErrForbidden，实际 %v", err)
	}
	if _, err := os.Stat(from); err != nil {
		t.Fatal("越界失败后源文件必须原样保留")
	}
	// 源越界同样拒绝
	if _, err := m.Move("/etc/hosts", filepath.Join(root, "hosts"), ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("越界源路径应返回 ErrForbidden，实际 %v", err)
	}
}

// 不允许把白名单根目录本身作为覆盖目标删掉。
func TestMoveOverwriteRefusesRoot(t *testing.T) {
	m, root := newTestManager(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(root, "a.txt")
	_ = os.WriteFile(from, []byte("x"), 0o644)
	if _, err := m.Move(from, root, MoveConflictOverwrite); err == nil {
		t.Fatal("覆盖根目录应被拒绝")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("根目录不应被删除")
	}
}

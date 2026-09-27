package web

// api_files_dangling_gate_test.go —— 悬空软链接必须"列得出、删得掉，且只删链接不删目标"。
//
// 为什么现有门禁抓不到：此前所有软链接用例（TestResolveBlocksSymlinkEscape、
// TestWriteBlocksSymlinkEscape、TestFileUploadFolderRejectsSymlinkEscape、
// TestDiskCustomMountPointRejectsSymlinkEscape）判的都是"链接能解析、越界要拦住"，
// 没有任何一条覆盖"目标不存在、链接本身还在"这一态：列表碰巧用 Lstat 能列出来，
// 但删除预检/Delete 走 Resolve（Stat，跟随链接）⇒ 悬空链接被当成"文件不存在"，
// 面板上就是"列得出来但删不掉"（实测镜像站 71 个悬空链接返回 400）。也**没有**任何
// 判据断言"删链接只删链接、不删它指向的目标"这条安全不变量。
//
// 负向对照（已实测变红）：把 Delete/预检改回跟随链接的实现 —— 悬空链接删不掉、
// 指向根内目录的链接会把整棵目标树删掉，本用例随即失败。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/files"
)

func TestDanglingSymlinkListDeleteGate(t *testing.T) {
	s, f := newFakeFileRootsServer(t)
	root := f.www

	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "good")
	if err := os.Symlink("real.txt", good); err != nil {
		t.Skipf("当前环境不支持创建软链接: %v", err)
	}
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink("missing.txt", dangling); err != nil {
		t.Fatal(err)
	}

	mgr := s.fileManager()

	// ① 两个链接都必须能被列出；悬空那个标成"链接已失效"、大小按链接本身（Lstat）。
	res, err := mgr.List(root, false)
	if err != nil {
		t.Fatalf("列目录失败（悬空链接不该让它整体报错）: %v", err)
	}
	byName := map[string]files.Entry{}
	for _, e := range res.Entries {
		byName[e.Name] = e
	}
	ge, ok := byName["good"]
	if !ok {
		t.Fatalf("可解析的软链接 good 丢了：%+v", res.Entries)
	}
	dl, ok := byName["dangling"]
	if !ok {
		t.Fatalf("悬空软链接 dangling 没被列出来（被当成『文件不存在』整条跳过了）")
	}
	if !dl.Symlink || !dl.SymlinkBroken {
		t.Errorf("dangling 必须标成失效链接：symlink=%v symlink_broken=%v", dl.Symlink, dl.SymlinkBroken)
	}
	if dl.IsDir {
		t.Errorf("悬空链接不该被当成目录")
	}
	li, err := os.Lstat(dangling)
	if err != nil {
		t.Fatal(err)
	}
	if dl.Size != li.Size() {
		t.Errorf("悬空链接的大小必须按链接本身（Lstat %d），实际 %d", li.Size(), dl.Size)
	}
	if ge.SymlinkBroken {
		t.Errorf("能解析的软链接 good 不该被标失效")
	}

	// ② 删除预检（生产路径 handleFileDelete → checkFileOpContainment）必须放行悬空链接。
	if err := s.checkFileOpContainment([]files.FilePair{{From: dangling}}, true); err != nil {
		t.Fatalf("删除预检把悬空软链接误报为不可删: %v", err)
	}
	if _, err := mgr.DeleteItems(context.Background(), []string{dangling}, false, nil); err != nil {
		t.Fatalf("删除悬空链接失败: %v", err)
	}
	if _, err := os.Lstat(dangling); !os.IsNotExist(err) {
		t.Errorf("悬空链接没被删掉（err=%v）", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "real.txt")); err != nil || string(b) != "keep me" {
		t.Errorf("删除悬空链接动了目标位置的东西: %q err=%v", string(b), err)
	}
	if _, err := os.Lstat(good); err != nil {
		t.Errorf("删 dangling 时把 good 也删了: %v", err)
	}

	// ③ 安全不变量：根内的链接指向根外 ⇒ 只许删链接，根外目标必须原样存在。
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	outLink := filepath.Join(root, "out-link")
	if err := os.Symlink(outsideFile, outLink); err != nil {
		t.Fatal(err)
	}
	// 正常路径校验不许被放宽：跟随链接后落在根外，Resolve 仍须拒绝。
	if _, err := mgr.Resolve(outLink, false); !errors.Is(err, files.ErrForbidden) {
		t.Fatalf("指向根外的软链接经 Resolve 必须仍然 403，实际 err=%v", err)
	}
	if err := s.checkFileOpContainment([]files.FilePair{{From: outLink}}, true); err != nil {
		t.Fatalf("根内链接本身应当可删（只删链接）: %v", err)
	}
	if _, err := mgr.DeleteItems(context.Background(), []string{outLink}, false, nil); err != nil {
		t.Fatalf("删除根内链接失败: %v", err)
	}
	if _, err := os.Lstat(outLink); !os.IsNotExist(err) {
		t.Errorf("根内链接没被删掉（err=%v）", err)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("根外目标被删掉了 —— 安全不变量被破坏: %v", err)
	}
	// 根外路径本身仍然 403（用真实 handler 的错误映射确认状态码）。
	werr := s.checkFileOpContainment([]files.FilePair{{From: outsideFile}}, true)
	if !errors.Is(werr, files.ErrForbidden) {
		t.Fatalf("根外路径必须 ErrForbidden，实际 %v", werr)
	}
	rec := httptest.NewRecorder()
	failFileErr(rec, werr, outsideFile)
	if rec.Code != http.StatusForbidden {
		t.Errorf("根外路径删除必须 403，实际 %d：%s", rec.Code, rec.Body.String())
	}

	// ④ 指向根内目录的链接：递归删除也只许删链接，目标目录必须原样在
	//（跟随链接的旧实现会把整棵目标树删掉，只剩一个悬空链接）。
	targetDir := filepath.Join(root, "realdir")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(root, "dirlink")
	if err := os.Symlink(targetDir, dirLink); err != nil {
		t.Fatal(err)
	}
	if err := s.checkFileOpContainment([]files.FilePair{{From: dirLink}}, true); err != nil {
		t.Fatalf("目录链接的删除预检失败: %v", err)
	}
	if _, err := mgr.DeleteItems(context.Background(), []string{dirLink}, true, nil); err != nil {
		t.Fatalf("递归删除目录链接失败: %v", err)
	}
	if _, err := os.Lstat(dirLink); !os.IsNotExist(err) {
		t.Errorf("目录链接没被删掉（err=%v）", err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "keep.txt")); err != nil {
		t.Fatalf("删链接把目标目录一起删了（跟随链接的旧行为）: %v", err)
	}

	// ⑤ 写入口也不许跟随悬空链接：Touch / 上传都必须在链接所在位置落条目，
	// 绝不在链接指向的（根外）位置凭空创建文件或写入内容。
	outTarget := filepath.Join(outside, "created-by-write.txt")
	wlink := filepath.Join(root, "wlink")
	if err := os.Symlink(outTarget, wlink); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Touch(wlink); err == nil {
		t.Errorf("Touch 一个悬空链接本该按『已存在同名条目』拒绝，却成功了")
	}
	if _, err := os.Stat(outTarget); err == nil {
		t.Fatalf("Touch 跟随悬空链接在根外建出了文件: %s", outTarget)
	}
	if _, _, err := mgr.SaveUpload(root, "wlink", strings.NewReader("x")); err != nil {
		t.Errorf("与悬空链接同名的上传应当另存唯一名，实际报错: %v", err)
	}
	if _, err := os.Stat(outTarget); err == nil {
		t.Fatalf("上传跟随悬空链接往根外写了内容: %s", outTarget)
	}
	if li, err := os.Lstat(wlink); err != nil || li.Mode()&os.ModeSymlink == 0 {
		t.Errorf("上传不该动那个悬空链接本身（err=%v）", err)
	}
	// 覆盖上传（上传文件夹的默认策略）同理：替换链接本身，绝不写到根外。
	ovTarget := filepath.Join(outside, "created-by-overwrite.txt")
	ovLink := filepath.Join(root, "ovlink")
	if err := os.Symlink(ovTarget, ovLink); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := mgr.SaveUploadAs(root, "ovlink", strings.NewReader("y"), true); err != nil {
		t.Errorf("覆盖上传到悬空链接应替换链接本身，实际报错: %v", err)
	}
	if _, err := os.Stat(ovTarget); err == nil {
		t.Fatalf("覆盖上传跟随悬空链接往根外写了内容: %s", ovTarget)
	}
	if li, err := os.Lstat(ovLink); err != nil || li.Mode()&os.ModeSymlink != 0 {
		t.Errorf("覆盖上传后该位置应是一个普通文件（链接已被替换）: err=%v", err)
	}
}

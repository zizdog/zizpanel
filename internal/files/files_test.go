package files

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestManager 构造一个以 tmp 为唯一根目录的管理器。
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	m := NewManager(Options{
		Roots:    []string{root},
		UserName: "nobody",
	})
	// 返回解析后的根路径：macOS 上 /var 是 /private/var 的软链接，
	// 管理器内部统一使用真实路径，测试也必须用同一口径比较。
	return m, m.Roots()[0]
}

// ---------------------------------------------------------------------------
//  安全测试：这是本包最重要的部分
//
//  旧面板的 site_del 只用 str_starts_with 做前缀校验，
//  软链接可以绕过。文件管理器的后果更严重（可删除任意文件），
//  因此必须用测试锁死这些攻击路径。
// ---------------------------------------------------------------------------

func TestResolveBlocksTraversal(t *testing.T) {
	m, root := newTestManager(t)
	_ = os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hi"), 0o644)

	// 这些必须被拒绝
	bad := []string{
		"/etc/passwd",
		"/etc/hosts",
		root + "/../../etc/passwd",
		root + "/sub/../../../etc/passwd",
		filepath.Join(root, "..", "..") + "/etc/passwd",
		"relative/path",
		"",
		"/",
		"/Users",
	}
	for _, p := range bad {
		if _, err := m.Resolve(p, false); err == nil {
			t.Fatalf("越界路径未被拒绝: %q", p)
		}
	}

	// 这些必须通过
	good := []string{root, filepath.Join(root, "ok.txt"), filepath.Join(root, "sub")}
	for _, p := range good {
		if _, err := m.Resolve(p, false); err != nil {
			t.Fatalf("合法路径被拒绝: %q (%v)", p, err)
		}
	}
}

// 软链接逃逸：根目录内放一个指向 /etc 的软链接，
// 通过它访问外部的路径必须被拒绝。
func TestResolveBlocksSymlinkEscape(t *testing.T) {
	m, root := newTestManager(t)
	link := filepath.Join(root, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skip("当前环境不支持创建软链接")
	}

	// 关键断言：软链接指向根目录之外时，**连软链接本身都不允许访问**。
	//
	// 这里刻意采用最保守的策略：Resolve 会把路径解析为真实路径，
	// 解析后落在 /etc（根目录之外），因此直接拒绝。
	// 比"允许打开链接但限制后续访问"更安全 —— 后者需要每个操作都重新校验，
	// 一旦某处漏掉就形成漏洞。
	if _, err := m.Resolve(link, false); err == nil {
		t.Fatal("指向根目录之外的软链接必须被拒绝")
	}
	// 穿过软链接访问外部文件当然也要拒绝
	target := filepath.Join(link, "passwd")
	if _, err := m.Resolve(target, false); err == nil {
		t.Fatal("通过软链接访问根目录之外的文件必须被拒绝（旧面板的漏洞就在这里）")
	}
	if _, err := m.List(target, false); err == nil {
		t.Fatal("通过软链接列目录必须被拒绝")
	}
	// 反向验证：指向根目录内部的软链接应该正常工作
	inner := filepath.Join(root, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	okLink := filepath.Join(root, "ok-link")
	if err := os.Symlink(inner, okLink); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(okLink, false); err != nil {
		t.Fatalf("指向根目录内部的软链接应可访问: %v", err)
	}
}

// 通过软链接穿越写入也要被拦住（不仅是读取）。
func TestWriteBlocksSymlinkEscape(t *testing.T) {
	m, root := newTestManager(t)
	outside := t.TempDir()
	link := filepath.Join(root, "out")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("当前环境不支持创建软链接")
	}
	// 往软链接指向的外部目录里写文件：必须拒绝
	target := filepath.Join(link, "evil.txt")
	if err := m.Write(target, "pwned", true); err == nil {
		t.Fatal("通过软链接向外写入必须被拒绝")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("外部目录竟然被写入了文件")
	}
}

// 在"目标不存在"的情况下（新建文件）也要正确拦截。
// 这是只做 realpath 校验会漏掉的情况：目标不存在时 realpath 失败。
func TestResolveMissingTargetStaysInside(t *testing.T) {
	m, root := newTestManager(t)
	outside := t.TempDir()
	link := filepath.Join(root, "out")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("环境不支持软链接")
	}
	// 目标不存在，父级是软链接
	if _, err := m.Resolve(filepath.Join(link, "new.txt"), true); err == nil {
		t.Fatal("新建路径穿过软链接时必须被拒绝")
	}
	// 完全不存在的越界路径
	if _, err := m.Resolve("/tmp/whatever-not-exists", true); err == nil {
		t.Fatal("根目录之外的路径必须被拒绝（即使文件不存在）")
	}
}

func TestDeleteRefusesRoot(t *testing.T) {
	m, root := newTestManager(t)
	if err := m.Delete(root, true); err == nil {
		t.Fatal("不允许删除根目录本身")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("根目录不应被删除")
	}
}

func TestDeleteRequiresRecursiveForNonEmptyDir(t *testing.T) {
	m, root := newTestManager(t)
	dir := filepath.Join(root, "d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 非递归删除非空目录必须被拒绝
	if err := m.Delete(dir, false); err == nil {
		t.Fatal("非空目录的非递归删除应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal("文件不应被删除")
	}
	// 递归删除可以
	if err := m.Delete(dir, true); err != nil {
		t.Fatalf("递归删除应成功: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("目录应已被删除")
	}
}

// 上传文件名必须被清洗，不能借 ../ 覆盖上层文件。
func TestSaveUploadCleansFilename(t *testing.T) {
	m, root := newTestManager(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 恶意文件名：尝试写到根目录
	got, _, err := m.SaveUpload(sub, "../../evil.txt", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if filepath.Dir(got) != resolveExisting(sub) {
		t.Fatalf("文件被写到了预期之外的位置: %s（期望在 %s 内）", got, sub)
	}
	if _, err := os.Stat(filepath.Join(root, "evil.txt")); err == nil {
		t.Fatal("恶意的 ../ 文件名竟然逃出了目标目录")
	}
}

// 同名上传不能静默覆盖已有文件。
func TestSaveUploadDoesNotOverwrite(t *testing.T) {
	m, root := newTestManager(t)
	existing := filepath.Join(root, "a.txt")
	if err := os.WriteFile(existing, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := m.SaveUpload(root, "a.txt", strings.NewReader("new content"))
	if err != nil {
		t.Fatal(err)
	}
	if got == existing {
		t.Fatal("同名上传不应覆盖已有文件")
	}
	b, _ := os.ReadFile(existing)
	if string(b) != "original" {
		t.Fatal("原文件内容被改动了")
	}
}

// ---------------------------------------------------------------------------
//  功能测试
// ---------------------------------------------------------------------------

func TestListSortsDirsFirst(t *testing.T) {
	m, root := newTestManager(t)
	_ = os.MkdirAll(filepath.Join(root, "zdir"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "adir"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "b.txt"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, ".hidden"), []byte("x"), 0o644)

	res, err := m.List(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 3 {
		t.Fatalf("默认应隐藏点文件，期望 3 项，实际 %d", res.Total)
	}
	// 目录在前
	if !res.Entries[0].IsDir || !res.Entries[1].IsDir {
		t.Fatal("目录应排在文件前面")
	}
	if res.Entries[0].Name != "adir" {
		t.Fatalf("目录应按名称排序，第一个应为 adir，实际 %s", res.Entries[0].Name)
	}

	res2, _ := m.List(root, true)
	if res2.Total != 4 {
		t.Fatalf("显示隐藏文件时期望 4 项，实际 %d", res2.Total)
	}
}

func TestReadWriteRoundTrip(t *testing.T) {
	m, root := newTestManager(t)
	p := filepath.Join(root, "edit.txt")
	if err := m.Write(p, "第一行\n第二行\n", true); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	r, err := m.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "第一行\n第二行\n" {
		t.Fatalf("内容不一致: %q", r.Content)
	}
	if r.Binary {
		t.Fatal("文本文件不应被判为二进制")
	}
}

func TestReadDetectsBinary(t *testing.T) {
	m, root := newTestManager(t)
	p := filepath.Join(root, "bin.dat")
	if err := os.WriteFile(p, []byte{0x00, 0x01, 0x02, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := m.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Binary {
		t.Fatal("含 NUL 字节的文件应被判为二进制")
	}
	if r.Content != "" {
		t.Fatal("二进制文件不应返回内容")
	}
}

func TestReadRefusesLargeFile(t *testing.T) {
	m, root := newTestManager(t)
	m.maxEditSize = 100
	p := filepath.Join(root, "big.txt")
	if err := os.WriteFile(p, []byte(strings.Repeat("a", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := m.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if !r.TooLarge {
		t.Fatal("超大文件应标记 TooLarge 而不是返回内容")
	}
}

func TestWritePreservesPermissions(t *testing.T) {
	m, root := newTestManager(t)
	p := filepath.Join(root, "exec.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(p, "#!/bin/sh\necho hi\n", false); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("写入后权限应保持 755，实际 %o", st.Mode().Perm())
	}
}

func TestRenameAndCopy(t *testing.T) {
	m, root := newTestManager(t)
	src := filepath.Join(root, "a.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 重命名
	dst := filepath.Join(root, "b.txt")
	if err := m.Rename(src, dst); err != nil {
		t.Fatalf("重命名失败: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal("重命名后目标不存在")
	}
	// 重命名到已存在的目标必须失败
	if err := m.Rename(dst, dst); err != nil {
		t.Fatalf("重命名到自己应是无操作: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Rename(dst, filepath.Join(root, "c.txt")); err == nil {
		t.Fatal("重命名到已存在的文件应失败")
	}
	// 复制
	cp := filepath.Join(root, "copy.txt")
	if err := m.Copy(dst, cp); err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	b, _ := os.ReadFile(cp)
	if string(b) != "data" {
		t.Fatal("复制内容不一致")
	}
}

// 不允许把目录移动/复制到它自己的子目录（会失败并可能损坏数据）。
func TestRenameAndCopyRefuseSelfNesting(t *testing.T) {
	m, root := newTestManager(t)
	dir := filepath.Join(root, "parent")
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Rename(dir, filepath.Join(dir, "child", "moved")); err == nil {
		t.Fatal("把目录移动到自己的子目录应被拒绝")
	}
	if err := m.Copy(dir, filepath.Join(dir, "child", "copy")); err == nil {
		t.Fatal("把目录复制到自己的子目录应被拒绝")
	}
}

func TestParseMode(t *testing.T) {
	good := map[string]os.FileMode{"644": 0o644, "755": 0o755, "0600": 0o600, "777": 0o777}
	for in, want := range good {
		got, err := ParseMode(in)
		if err != nil {
			t.Fatalf("ParseMode(%q) 出错: %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseMode(%q) = %o，期望 %o", in, got, want)
		}
	}
	bad := []string{"", "99", "9999", "abc", "8", "12", "12345"}
	for _, in := range bad {
		if _, err := ParseMode(in); err == nil {
			t.Fatalf("非法权限未被拒绝: %q", in)
		}
	}
}

// ---------------------------------------------------------------------------
//  归档测试
// ---------------------------------------------------------------------------

func TestCompressAndExtractRoundTrip(t *testing.T) {
	m, root := newTestManager(t)
	srcDir := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("content-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "nested", "b.txt"), []byte("content-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"zip", "tar.gz", "tar"} {
		t.Run(format, func(t *testing.T) {
			out, err := m.Compress(context.Background(), root, []string{srcDir}, format, "pack."+format)
			if err != nil {
				t.Fatalf("压缩失败: %v", err)
			}
			if _, err := os.Stat(out); err != nil {
				t.Fatal("归档文件不存在")
			}
			destDir := filepath.Join(root, "out-"+strings.ReplaceAll(format, ".", "-"))
			if err := os.MkdirAll(destDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Extract(context.Background(), out, destDir); err != nil {
				t.Fatalf("解压失败: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(destDir, "src", "nested", "b.txt"))
			if err != nil {
				t.Fatalf("解压后文件缺失: %v", err)
			}
			if string(got) != "content-b" {
				t.Fatalf("解压内容不一致: %q", got)
			}
		})
	}
}

// zip-slip 防护：归档里含 ../ 时禁止解压。
//
// 这是经典的压缩包攻击：解压后文件会落到目标目录之外。
func TestExtractBlocksZipSlip(t *testing.T) {
	m, root := newTestManager(t)
	evilDir := filepath.Join(root, "evil")
	if err := os.MkdirAll(evilDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 直接构造一个带 ../ 路径的 tar（用 tar 的 -P 无法产生，手工写字节）
	// 更简单可靠：构造一个正常的 tar，然后验证检查函数能识别越界条目。
	// 这里直接测 Extract 对归档越界条目（../）的拒绝逻辑。
	destDir := filepath.Join(root, "dest")
	_ = os.MkdirAll(destDir, 0o755)

	// 造一个真实含 ../ 的 tar 文件：先在子目录里创建文件再打包（相对路径会带目录名），
	// 然后用 python 生成一个含 ../ 的 tar（macOS 自带 python3）。
	evilTar := filepath.Join(root, "evil.tar")
	script := `import tarfile, io, os, sys
with tarfile.open(sys.argv[1], "w") as tf:
    data = b"pwned"
    info = tarfile.TarInfo("../escaped.txt")
    info.size = len(data)
    tf.addfile(info, io.BytesIO(data))
`
	py := filepath.Join(root, "make_evil.py")
	if err := os.WriteFile(py, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(context.Background(), 30_000_000_000, "", "/usr/bin/python3", py, evilTar); err != nil {
		t.Skipf("无法构造测试用恶意归档: %v", err)
	}

	if _, err := m.Extract(context.Background(), evilTar, destDir); err == nil {
		t.Fatal("含 ../ 的归档必须拒绝解压（zip-slip）")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.txt")); err == nil {
		t.Fatal("文件逃出了目标目录")
	}
}

func TestCompressRejectsOutputOutsideRoot(t *testing.T) {
	m, root := newTestManager(t)
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	// 输出名会被 filepath.Base 清洗，所以这里主要验证不会写到根目录之外
	out, err := m.Compress(context.Background(), root, []string{"a.txt"}, "zip", "../../escape.zip")
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if !strings.HasPrefix(out, root) {
		t.Fatalf("归档被写到了根目录之外: %s（根目录 %s）", out, root)
	}
	// 文件名里的 ../ 必须被清洗掉
	if filepath.Base(out) != "escape.zip" {
		t.Fatalf("输出文件名未被清洗: %s", out)
	}
}

// ---------------------------------------------------------------------------
//  搜索
// ---------------------------------------------------------------------------

func TestSearchByNameAndContent(t *testing.T) {
	m, root := newTestManager(t)
	_ = os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "findme.txt"), []byte("hello"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "sub", "other.md"), []byte("关键字在里面\n第二行"), 0o644)

	// 按名字
	r, err := m.Search(context.Background(), root, "findme", "name", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Hits) != 1 || !strings.HasSuffix(r.Hits[0].Path, "findme.txt") {
		t.Fatalf("按名字搜索失败: %+v", r.Hits)
	}

	// 按内容
	r2, err := m.Search(context.Background(), root, "关键字", "content", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Hits) != 1 {
		t.Fatalf("按内容搜索失败: %+v", r2.Hits)
	}
	if r2.Hits[0].LineNo != 1 {
		t.Fatalf("命中行号应为 1，实际 %d", r2.Hits[0].LineNo)
	}
	if !strings.Contains(r2.Hits[0].MatchLine, "关键字") {
		t.Fatalf("命中的行内容不正确: %q", r2.Hits[0].MatchLine)
	}
}

// 搜索不能越出根目录。
func TestSearchStaysInsideRoot(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Search(context.Background(), "/etc", "passwd", "name", 10); err == nil {
		t.Fatal("在允许范围之外搜索必须被拒绝")
	}
}

func TestReplaceInFileTreatsInputLiterally(t *testing.T) {
	m, root := newTestManager(t)
	p := filepath.Join(root, "code.txt")
	// 文件里同时有 "a.b" 与 "axb"
	if err := os.WriteFile(p, []byte("a.b and axb and a.b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 查找 "a.b" 应只匹配字面的 a.b，不能把 . 当通配符
	n, err := m.ReplaceInFile(p, "a.b", "X", true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应替换 2 处（不能把 . 当正则通配），实际 %d", n)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "X and axb and X\n" {
		t.Fatalf("替换结果不正确: %q", string(b))
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.00 KB", 1048576: "1.00 MB"}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Fatalf("FormatSize(%d) = %q，期望 %q", in, got, want)
		}
	}
}

// TestExtractReportsProgress：解压必须按**条目**报进度（用户 2026-09-18 报障：
// "900M 的文件解压没有任何进度"）。
//
// 判据：进度回调被调用过、总条目数 > 0、最后一条的 done 等于总条目数
// （也就是"真的走到了最后"，不是在中途停住还报成功）。
func TestExtractReportsProgress(t *testing.T) {
	m, root := newTestManager(t)
	if err := os.MkdirAll(filepath.Join(root, "src", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.txt", "b.txt", "sub/c.txt"} {
		if err := os.WriteFile(filepath.Join(root, "src", n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Compress(context.Background(), filepath.Join(root, "src"), []string{"a.txt", "b.txt", "sub"}, "zip", "pack.zip"); err != nil {
		t.Fatalf("打包失败: %v", err)
	}

	var seen []struct{ done, total int }
	dest, err := m.ExtractWithProgress(context.Background(),
		filepath.Join(root, "src", "pack.zip"), filepath.Join(root, "src"), func(done, total int, note string) {
			seen = append(seen, struct{ done, total int }{done, total})
			if note == "" {
				t.Errorf("进度回调必须带上当前条目名（用户要看到「正在解压哪个文件」）")
			}
		})
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("解压过程一次进度都没报 —— 用户看到的就是「没有任何进度」")
	}
	if seen[0].total <= 0 {
		t.Errorf("总条目数必须 > 0（界面据此显示 N/M），实际 %+v", seen[0])
	}
	last := seen[len(seen)-1]
	if last.done != last.total {
		t.Errorf("结束时应报 done==total，实际 %+v（进度会在半路停住）", last)
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", "c.txt")); err != nil {
		t.Errorf("解压结果不完整: %v", err)
	}
}

// TestCheckExtractTargetsRejectsZipSlipBeforeTask：安全校验必须在**开任务之前**做。
//
// 让用户在任务日志里看到"拒绝解压"太晚了（他已经在等进度了）。
func TestCheckExtractTargetsRejectsZipSlipBeforeTask(t *testing.T) {
	m, root := newTestManager(t)
	// 手工造一个含 ../ 的 zip
	bad := filepath.Join(root, "bad.zip")
	f, err := os.Create(bad)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("../escape.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("pwn"))
	_ = zw.Close()
	_ = f.Close()

	if err := m.CheckExtractTargets(context.Background(), bad, root); err == nil {
		t.Error("含目录穿越的归档必须在预检阶段就被拒绝")
	}
}

package files

// permission_errno_test.go —— Resolve 遇到权限拒绝时必须**保留真实 errno**。
//
// 背景：外接卷被 macOS 隐私保护（TCC）拦住时 stat 就是 EPERM/EACCES。老实现
// 把任何 stat 失败都写成"文件不存在"，于是 web 层既看不到 errno、也拿不到
// 路径，用户只得到一句谎报（"文件不存在"）而不是可操作指引。
// 见 internal/web/api_files.go::volumeTCCPath。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveKeepsPermissionErrno(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时 0000 目录仍可穿越，无法构造 EACCES")
	}
	m, root := newTestManager(t)
	// sub 没有执行权限：stat root/sub/missing 会 EACCES（而不是 ENOENT）。
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	_, err := m.Resolve(filepath.Join(sub, "missing"), false)
	if err == nil {
		t.Fatal("无权限的路径不该 Resolve 成功")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("必须保留真实权限 errno（web 层据此映射 TCC 指引），实际：%v", err)
	}
	if strings.Contains(err.Error(), "文件不存在") {
		t.Errorf("权限拒绝不得谎报成\"文件不存在\"：%v", err)
	}
}

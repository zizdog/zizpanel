package sites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureLocalhostPlaceholder：占位页"缺了才建、绝不覆盖"。
//
// 为什么单独测这条：这份占位页同时是"默认站点真的生效"的判据
// （面板会取 http://127.0.0.1/ 并检查 LocalhostIndexMarker）。用户把它换成自己的
// 内容后，我们再覆盖一次就等于动了用户的东西；而标记丢了又会让面板的复核误报失败。
func TestEnsureLocalhostPlaceholder(t *testing.T) {
	root := t.TempDir()
	path, created, err := EnsureLocalhostPlaceholder(root)
	if err != nil {
		t.Fatalf("创建占位页失败: %v", err)
	}
	if !created {
		t.Error("第一次应报告 created=true")
	}
	if path != filepath.Join(root, "localhost", "index.html") {
		t.Errorf("路径不对: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), LocalhostIndexMarker) {
		t.Errorf("占位页必须含判据标记 %q", LocalhostIndexMarker)
	}

	// 用户改过之后：再跑必须一个字都不动
	custom := "<html>用户自己的内容</html>"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, created, err := EnsureLocalhostPlaceholder(root); err != nil || created {
		t.Fatalf("已存在时不该再创建（err=%v created=%v）", err, created)
	}
	after, _ := os.ReadFile(path)
	if string(after) != custom {
		t.Errorf("覆盖了用户自己的占位页：\n%s", after)
	}

	if _, _, err := EnsureLocalhostPlaceholder(""); err == nil {
		t.Error("网站根目录为空时应报错，而不是写到奇怪的地方")
	}
}

// TestEnsureDefaultSitePHPIndex：默认站点的 index.php —— 缺了才建、绝不覆盖。
//
// 用户要求："一键安装完成后默认建一个站…有一个默认 index.php"。
// 同时必须守住：用户改过之后不再覆盖（护栏与 index.html 一致）。
func TestEnsureDefaultSitePHPIndex(t *testing.T) {
	root := t.TempDir()
	path, created, err := EnsureDefaultSitePHPIndex(root)
	if err != nil {
		t.Fatalf("创建 index.php 失败: %v", err)
	}
	if !created {
		t.Error("第一次应报告 created=true")
	}
	if path != filepath.Join(root, "localhost", "index.php") {
		t.Errorf("路径不对: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, DefaultIndexPHPMarker) {
		t.Errorf("index.php 必须带标记 %q（用于「缺了才建」与自证来源）", DefaultIndexPHPMarker)
	}
	// 必须真的自证 PHP 可用：版本 + 时间
	for _, want := range []string{"<?php", "PHP_VERSION", "date("} {
		if !strings.Contains(body, want) {
			t.Errorf("index.php 应包含 %q（自证 PHP 可用）：\n%s", want, body)
		}
	}
	// 用户改过之后：一个字都不动
	custom := "<?php echo 'my site';"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, created, err := EnsureDefaultSitePHPIndex(root); err != nil || created {
		t.Fatalf("已存在时不该再创建（err=%v created=%v）", err, created)
	}
	after, _ := os.ReadFile(path)
	if string(after) != custom {
		t.Errorf("覆盖了用户自己的 index.php：\n%s", after)
	}
	if _, _, err := EnsureDefaultSitePHPIndex(""); err == nil {
		t.Error("网站根目录为空时应报错")
	}
}

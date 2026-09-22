package web

// 前端资源运行体门禁（2026-09-22 瘦身）：原来 45 个"读源码 + Contains"的接线断言
// 只证明某个字符串出现过，改个名字/换句文案就假红；这里只留三件删了会出事的：
// 真 ES 解析、模块引用存在、appKeyOf 稳定键。共享助手也收在本文件。

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRootDir 从测试工作目录（包目录）向上找到仓库根（含 go.mod）。
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("向上找不到仓库根（go.mod）")
		}
		dir = parent
	}
}

func TestFrontendAssetsGate(t *testing.T) {
	root := repoRootDir(t)

	// ① 每个 .js 都必须过真正的 ES 解析器 —— 浏览器拒绝执行的语法错误让整页白屏，
	//    而 `node --check` 会放过它（等价于 make check 里的 acorn 那一步）。
	t.Run("ES 语法", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("找不到 node，跳过前端 ES 语法门禁（make check 的 acorn 检查也需要它）")
		}
		args := []string{
			filepath.Join("tools", "check-js-syntax.mjs"),
			"internal/web/assets/js", "internal/web/assets/nav", "zizvideo/internal/web/assets",
		}
		cmd := exec.Command(node, args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("前端 JS 语法门禁失败（白屏级错误）：\n%s", out)
		}
	})

	// ② index.html 与 assets/js 里引用的相对模块必须真实存在：缺文件的 import
	//    会被浏览器静默漏掉，只有运行时才炸（或整页空白）。
	t.Run("模块引用存在", func(t *testing.T) {
		assetRoot := filepath.Join(root, "internal", "web", "assets")
		jsDir := filepath.Join(assetRoot, "js")
		type ref struct{ from, target string }
		var refs []ref

		html, err := os.ReadFile(filepath.Join(assetRoot, "index.html"))
		if err != nil {
			t.Fatalf("读不到 index.html：%v", err)
		}
		for _, m := range regexp.MustCompile(`(?:src|href)="(\.[^"]+)"`).FindAllStringSubmatch(string(html), -1) {
			refs = append(refs, ref{from: filepath.Join(assetRoot, "index.html"), target: m[1]})
		}
		impRe := regexp.MustCompile(`(?:from\s+|import\s+)['"](\.[^'"]+)['"]`)
		for _, name := range listAssetJS(t) {
			src := readAssetJS(t, name)
			for _, m := range impRe.FindAllStringSubmatch(src, -1) {
				refs = append(refs, ref{from: filepath.Join(jsDir, name), target: m[1]})
			}
		}
		if len(refs) == 0 {
			t.Fatal("一个相对资源引用都没解析到 —— 解析器或目录猜错了，门禁等于没跑")
		}
		for _, r := range refs {
			p := filepath.Join(filepath.Dir(r.from), filepath.FromSlash(r.target))
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s 引用了不存在的 %s（浏览器会静默漏掉这个模块）",
					filepath.Base(r.from), r.target)
			}
		}
	})

	// ③ appKeyOf 必须优先用目录 ID（app_id）当合并键：退回按 label 猜身份，
	//    "同一个应用两张卡片"（坑 228）就会复发。
	t.Run("appKeyOf 优先 app_id", func(t *testing.T) {
		body := jsFuncBody(t, readAssetJS(t, "servicePanel.js"), "appKeyOf")
		if !strings.Contains(body, "app_id") {
			t.Error("appKeyOf 不再读 app_id —— 同一应用的两张卡片会复发")
		}
		if !regexp.MustCompile(`'id:'\s*\+\s*appID`).MatchString(body) {
			t.Error("appKeyOf 没有优先返回 'id:' + app_id 这种稳定键")
		}
		if i, j := strings.Index(body, "app_id"), strings.Index(body, "launch_label"); i < 0 || (j >= 0 && i > j) {
			t.Error("app_id 分支必须排在 launch_label/service_label 兜底之前")
		}
	})
}

// ---- 共享助手（原分散在已删的 *_frontend_test.go 里） ----

func readAssetJS(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("assets", "js", name))
	if err != nil {
		t.Fatalf("读不到前端资源 %s: %v", name, err)
	}
	return string(b)
}

func mustContain(t *testing.T, file, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s 里找不到 %q", file, needle)
	}
}

// listAssetJS 返回 assets/js 下的全部 .js 文件名。
func listAssetJS(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("assets", "js"))
	if err != nil {
		t.Fatalf("读不到前端目录: %v", err)
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		out = append(out, e.Name())
	}
	if len(out) == 0 {
		t.Fatal("assets/js 里一个 .js 都没有 —— 目录猜错了")
	}
	return out
}

// jsFuncBody 取出 `function <name>(...)` 配对花括号里的函数体（跳过字符串与注释）。
func jsFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	marker := "function " + name + "("
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("找不到函数 %s —— 实现改名/删除了，门禁判据必须同步", name)
	}
	rel := strings.Index(src[i:], "{")
	if rel < 0 {
		t.Fatalf("%s 没有函数体", name)
	}
	start := i + rel
	depth := 0
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '\'', '"', '`':
			q := src[j]
			j++
			for j < len(src) && src[j] != q {
				if src[j] == '\\' {
					j++
				}
				j++
			}
		case '/':
			if j+1 < len(src) && src[j+1] == '/' {
				for j < len(src) && src[j] != '\n' {
					j++
				}
			} else if j+1 < len(src) && src[j+1] == '*' {
				j += 2
				for j+1 < len(src) && !(src[j] == '*' && src[j+1] == '/') {
					j++
				}
				j++
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : j+1]
			}
		}
	}
	t.Fatalf("%s 的花括号不配对 —— 门禁解析失败", name)
	return ""
}

// readGoSource 读 internal/web 下的一个 Go 源文件（门禁用）。
func readGoSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读不到 %s: %v", name, err)
	}
	return string(b)
}

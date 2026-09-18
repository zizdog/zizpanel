package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ============================================================================
//  编辑器门禁：内嵌 CodeMirror 的资源必须齐全、语言依赖不许写错
//
//  背景（用户 2026-09-22）："让文件管理器和编辑器变成可用的现代的工具，写不好
//  可以直接引入开源项目。" 自研的"透明 textarea + 高亮层"两次出用户可见故障
//  （光标偏移、首次聚焦跳开头），所以编辑器换成了内嵌的 CodeMirror 5
//  （MIT，assets/vendor/codemirror/）。
//
//  这类内嵌资源的坏法很隐蔽：
//    · 少一个插件文件 → 功能静默消失（查找框打不开、折叠没反应）；
//    · 少一个模式文件 → CodeMirror **不报错**，直接退化成纯文本，用户只看到
//      "高亮没了"；
//  所以用测试把"资源清单"和"CM_LANGS 的依赖"钉在一起 —— 谁改了依赖却忘了
//  把对应的 mode 文件放进 vendor/，这里就会红。
// ============================================================================

func TestEditorUsesVendoredCodeMirror(t *testing.T) {
	js := readAssetJS(t, "files.js")
	for _, want := range []string{"ensureCodeMirror(", "vendor/codemirror/", "CM_LANGS", "CodeMirror(host"} {
		if !strings.Contains(js, want) {
			t.Errorf("files.js 里缺少 %q —— 编辑器要么没接上 CodeMirror，要么被改回了自研实现", want)
		}
	}
	// 自研编辑器留下的痕迹不该回来了（它们正是两次故障的来源）
	for _, bad := range []string{"zpf-ta", "paintHighlight", "ZPF_LINE_HEIGHT"} {
		if strings.Contains(js, bad) {
			t.Errorf("files.js 里又出现了自研编辑器的 %q —— 见本文件顶部说明（光标偏移/跳开头都来自它）", bad)
		}
	}
}

func TestEditorCodeMirrorAssetsPresent(t *testing.T) {
	base := filepath.Join("assets", "vendor", "codemirror")
	core := []string{
		"codemirror.min.js", "codemirror.min.css",
		"addon/search/search.min.js", "addon/search/searchcursor.min.js",
		"addon/dialog/dialog.min.js", "addon/dialog/dialog.min.css",
		"addon/edit/matchbrackets.min.js", "addon/edit/closebrackets.min.js",
		"addon/fold/foldcode.min.js", "addon/fold/foldgutter.min.js",
		"addon/fold/brace-fold.min.js", "addon/fold/xml-fold.min.js",
		"addon/fold/comment-fold.min.js", "addon/selection/active-line.min.js",
		"addon/scroll/simplescrollbars.min.js", "addon/comment/comment.min.js",
		"theme/monokai.min.css", "LICENSE",
	}
	for _, f := range core {
		if _, err := os.Stat(filepath.Join(base, f)); err != nil {
			t.Errorf("内嵌编辑器资源缺失：%s（编辑器在二进制里就会打不开/功能静默消失）", filepath.Join(base, f))
		}
	}

	// CM_LANGS 的每条 deps 都必须有对应的 mode 文件
	js := readAssetJS(t, "files.js")
	re := regexp.MustCompile(`deps: \[([^\]]*)\]`)
	matches := re.FindAllStringSubmatch(js, -1)
	if len(matches) < 10 {
		t.Fatalf("CM_LANGS 里只解析出 %d 条 deps，测试判据可能已过期（改了写法要同步这里）", len(matches))
	}
	for _, m := range matches {
		for _, name := range strings.Split(m[1], ",") {
			name = strings.Trim(strings.TrimSpace(name), `'"`)
			if name == "" {
				continue
			}
			p := filepath.Join(base, "mode", name+".min.js")
			if _, err := os.Stat(p); err != nil {
				t.Errorf("CM_LANGS 依赖模式 %s，但 %s 不存在 —— CodeMirror 会**静默**退化成纯文本", name, p)
			}
		}
	}
}

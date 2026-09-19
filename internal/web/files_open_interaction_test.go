package web

import (
	"regexp"
	"strings"
	"testing"
)

// ============================================================================
//  文件列表"单击 = 选中 / 双击 = 打开"门禁
//
//  用户报障：双击文件名"没反应"。根因不是双击没绑，而是**文件名单击就 openAny** ——
//  第一次点击弹出查看器/播放器，第二次点击正好落在弹窗遮罩上（ui.js 的 modal 在
//  mousedown 就关），弹窗被自己关掉；同时行单击里的 renderTable() 整表重绘，
//  让第二次点击落到新节点上，浏览器根本不发 dblclick。
//
//  这两条都是"改回去就复发"的类别，所以判据落在**函数体内部**（不是全文件里
//  出现过某个词 —— 那样右键菜单里的 openAny 就能骗过门禁）。
// ============================================================================

// jsFuncBody 取出 `function <name>(...)` 配对花括号里的函数体（跳过字符串与注释）。
// 抽取失败即判据过期，直接 Fatal —— 宁可红，也不要静默放行。
func jsFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	marker := "function " + name + "("
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("files.js 里找不到函数 %s —— 交互实现改名/删除了，门禁判据必须同步", name)
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

func TestFilesSingleClickSelectsDoubleClickOpens(t *testing.T) {
	js := readAssetJS(t, "files.js")

	// ① 行单击：不许整表重绘（那会让 dblclick 永远不触发），也不许直接打开文件。
	click := jsFuncBody(t, js, "handleRowClick")
	if strings.Contains(click, "renderTable(") {
		t.Error("行单击处理里又出现了 renderTable() —— 整表重绘把第二次点击换到新节点上，" +
			"浏览器不再发 dblclick，双击打开会再次失效")
	}
	if strings.Contains(click, "openAny(") {
		t.Error("行单击处理里出现了 openAny() —— 单击就弹窗，第二次点击会落在遮罩上把窗口关掉")
	}
	if !strings.Contains(click, "syncSelectionUI(") {
		t.Error("行单击处理没有原地同步选中态（syncSelectionUI）—— 一旦改回整表重绘，双击就打不开")
	}
	if !strings.Contains(click, "closest('button, input')") {
		t.Error("行单击处理的忽略清单变了：文件名 <a> 必须能被行单击接住（不能把 a 也忽略掉，" +
			"否则点文件名既不选中也不打开）")
	}

	// ② 行双击：文件必须交给 openAny（图片查看器 / 播放器 / 编辑器同一入口），目录进入。
	open := jsFuncBody(t, js, "handleRowOpen")
	if !regexp.MustCompile(`e\.is_dir\s*\)\s*load\(e\.path\)\s*;\s*else\s*openAny\(e\)`).MatchString(open) {
		t.Errorf("双击处理器不再把文件交给 openAny（或目录不再进入）：%s", strings.TrimSpace(open))
	}

	// ③ 文件名节点：文件分支绝不能出现 openAny（单击只选中）；目录的"单击进入"必须保留。
	name := jsFuncBody(t, js, "nameCell")
	if strings.Contains(name, "openAny(") {
		t.Error("文件名节点里出现了 openAny() —— 文件名又变回「单击直接打开」，" +
			"第二次点击会落在弹窗遮罩上把窗口关掉（本次报障的根因）")
	}
	if !strings.Contains(name, "load(e.path)") {
		t.Error("目录的「单击进入」既有路径没了（nameCell 里找不到 load(e.path)）")
	}
	if !strings.Contains(name, "双击") {
		t.Error("文件名的 title 没有告诉用户「双击打开」—— 只靠肌肉记忆不够")
	}
	// 文件名格的既有标记不能顺手丢掉（改这一格最容易漏）。
	for _, want := range []string{"fileIcon(", "e.symlink", "e.read_only", "e.sensitive"} {
		if !strings.Contains(name, want) {
			t.Errorf("nameCell 里缺少 %q —— 图标/链接/只读/敏感标记被改没了", want)
		}
	}

	// ④ 接线：renderTable 必须真的把这两个处理器挂到行上，否则 ①②③ 只是死代码。
	rt := jsFuncBody(t, js, "renderTable")
	for _, want := range []string{"ondblclick:", "handleRowOpen(e)", "handleRowClick(e, idx, ev)"} {
		if !strings.Contains(rt, want) {
			t.Errorf("renderTable 里缺少 %q —— 处理器没接到行上，双击路径实际不存在", want)
		}
	}

	// ⑤ 工具栏不许随选中增减控件：换行会让列表整体下移，双击的第二下落到**别的行**上
	//    （1280 宽实测：双击 pic.png 打开的是 note.txt）。选中相关控件必须恒定占位。
	bar := jsFuncBody(t, js, "renderToolbar")
	if strings.Contains(bar, "selCount > 0 ?") {
		t.Error("renderToolbar 里又用 `selCount > 0 ?` 增减控件了 —— 选中时工具栏换行会把列表推下去，" +
			"双击的第二次点击落到别的行，会打开错误的文件")
	}
	for _, want := range []string{"disabled: selCount === 0", "visibility: selCount ?"} {
		if !strings.Contains(bar, want) {
			t.Errorf("renderToolbar 缺少 %q —— 选中相关控件必须恒定占位（隐藏/禁用），不许改变工具栏宽度", want)
		}
	}
}

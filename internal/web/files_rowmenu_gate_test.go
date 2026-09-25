package web

// files_rowmenu_gate_test.go —— 「行操作菜单只允许有一处定义」的唯一门禁。
//
// 为什么现有门禁抓不到：此前的行操作有两份独立实现 —— 行右键（rowContextMenu）
// 与「操作」列的「更多」（moreMenu）各自手写一份按钮/菜单项，改一处另一处就悄悄走样
// （复制/移动/解压/收藏都只在其中一份里出现过）。门禁里没有任何一条约束
// "同一份 UI 内容不许写两份"：ES 语法门禁只管能不能解析，模块引用门禁只管文件在不在，
// 行为门禁又只覆盖后端接口。前端结构走样是"用户点开发现少了一项"这类事故的直接来源。
//
// 一条门禁覆盖整类问题：
//   ① rowMenuItems 只有一处定义（不能有第二份实现）；
//   ② 两个入口（行右键 rowContextMenu + 「更多」rowMoreMenu）都引用它；
//   ③ 两个入口体内不许再写内联菜单项（没有 label/run 字面量）—— 这是"内联第二份"的判据。
// 负向对照（已实测变红）：把「更多」改成内联 items、或删掉入口里的调用。

import (
	"strings"
	"testing"
)

func TestFilesRowMenuSingleSourceGate(t *testing.T) {
	src := readAssetJS(t, "files.js")

	// ① 唯一来源只定义一次。
	if n := strings.Count(src, "function rowMenuItems("); n != 1 {
		t.Fatalf("rowMenuItems 定义了 %d 次（必须恰好 1 次）—— 行操作菜单出现第二份实现了", n)
	}

	// ② 两个入口都必须调它。先确认两个入口还在（改名/删除要显式报错，不能静默通过）。
	for _, entry := range []string{"rowContextMenu", "rowMoreMenu"} {
		body := jsFuncBody(t, src, entry)
		if !strings.Contains(body, "rowMenuItems(") {
			t.Errorf("%s 没有引用 rowMenuItems —— 它自己写了一份菜单内容", entry)
		}
		// ③ 入口体内不许出现菜单项字面量（label:/run:）—— 那是"内联第二份"的形态。
		for _, marker := range []string{"label:", "run:"} {
			if strings.Contains(body, marker) {
				t.Errorf("%s 体内出现 %q：菜单项必须只定义在 rowMenuItems 里", entry, marker)
			}
		}
	}

	// 判据有效性自检：「更多」这颗按钮必须真的调 rowMoreMenu（否则门禁在测空气）。
	if !strings.Contains(src, "rowMoreMenu(e)") {
		t.Error("「操作」列的「更多」按钮不再调用 rowMoreMenu —— 门禁的第二个入口已经不存在了")
	}
}

package web

// files_pure_gate_test.go —— 「列表排序 / 传输速度」纯函数的**接线**门禁。
//
// tools/check-files-pure.mjs 断言的是逻辑本身；这一条断言的是"浏览器真的用它"：
// 纯模块如果只是躺在 assets/js 里而 files.js/tasks.js 各自又写了一份比较器/滑窗，
// 那边界门禁全绿、界面行为却是旧的第二份实现。所以两件事一起锁：
//   ① 比较器只有 sortfiles.js 有，files.js 的 sortedEntries 必须委托给它；
//   ② 速度/ETA 只有 transfereta.js 有，且 files.js 与 tasks.js 都通过
//      taskCenter.progressRateText 取用（同一个采样，不各写一份）；
//   ③ 内存里的纯函数门禁脚本必须能跑（node 缺失时明确跳过，不假装通过）。
//
// 负向对照（已实测变红）：把 sortedEntries 改回内联比较器 ⇒ ① 红；
// 把 transfereta.js 的窗口裁剪删掉 ⇒ check-files-pure.mjs 红（见变异记录）。

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesPureWiringGate(t *testing.T) {
	sortMod := readAssetJS(t, "sortfiles.js")
	etaMod := readAssetJS(t, "transfereta.js")
	files := readAssetJS(t, "files.js")
	tasks := readAssetJS(t, "tasks.js")

	// ① 比较器只有一处。
	if n := strings.Count(sortMod, "export function compareEntries("); n != 1 {
		t.Fatalf("compareEntries 在 sortfiles.js 里定义了 %d 次（必须恰好 1 次）", n)
	}
	body := jsFuncBody(t, files, "sortedEntries")
	if !strings.Contains(body, "sortEntries(") {
		t.Error("files.js 的 sortedEntries 没有委托给 sortfiles.js 的 sortEntries —— 列表排序又出现了第二份实现")
	}
	// 委托判据的有效性自检：真正的比较逻辑（目录优先）不该留在 files.js 里。
	if strings.Contains(body, "is_dir") {
		t.Error("sortedEntries 里还有 is_dir 比较 —— 目录优先规则被复制出了第二份")
	}
	if n := strings.Count(files, "localeCompare(String(a.name || ''), 'zh')"); n != 0 {
		t.Errorf("files.js 里残留 %d 处内联名字比较 —— 排序逻辑必须只有 sortfiles.js 一份", n)
	}

	// ② 速度/ETA 只有一处，两个消费方都走同一个入口。
	if n := strings.Count(etaMod, "export function rateEtaText("); n != 1 {
		t.Fatalf("rateEtaText 在 transfereta.js 里定义了 %d 次（必须恰好 1 次）", n)
	}
	if n := strings.Count(etaMod, "export function pushSample("); n != 1 {
		t.Fatalf("pushSample 在 transfereta.js 里定义了 %d 次（必须恰好 1 次）", n)
	}
	if !strings.Contains(tasks, "from './transfereta.js'") {
		t.Error("tasks.js 没有 import transfereta.js —— 速度窗口又会出现第二份实现")
	}
	if n := strings.Count(tasks, "function progressRateText("); n != 1 {
		t.Fatalf("progressRateText 定义了 %d 次（必须恰好 1 次：唯一的采样入口）", n)
	}
	if !strings.Contains(tasks, "progressRateText,") {
		t.Error("taskCenter 没有导出 progressRateText —— 就地进度条拿不到同一份速度")
	}
	if !strings.Contains(files, "taskCenter.progressRateText(") {
		t.Error("files.js 的就地进度条没有用 taskCenter.progressRateText —— 文件操作不显示速度")
	}
	// 采样状态只允许在一个模块里（files.js 不许自己 new 一个采样器）。
	if strings.Contains(files, "createRateSampler(") {
		t.Error("files.js 自己创建了采样器 —— 同一条任务会出现两个互相打架的速度")
	}

	// ③ 纯函数门禁脚本必须能跑（node 缺失时跳过并说明，不假装通过）。
	t.Run("纯函数断言脚本", func(t *testing.T) {
		root := repoRootDir(t)
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("找不到 node，跳过纯函数断言（make check 的那一步也需要它）")
		}
		cmd := exec.Command(node, filepath.Join("tools", "check-files-pure.mjs"))
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("纯函数门禁失败（排序或速度/ETA 逻辑被改坏）：\n%s", out)
		}
	})
}

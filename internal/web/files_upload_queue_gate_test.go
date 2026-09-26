package web

// files_upload_queue_gate_test.go —— 「上传队列只有一处、分文件行只从队列渲染、
// 拖拽只有一个入口」的唯一门禁。
//
// 为什么现有门禁抓不到（用户报障"上传界面形同虚设"的直接原因）：
//   · ES 语法门禁只管能不能被浏览器解析（白屏级错误）；
//   · 模块引用门禁只管 import 的文件在不在；
//   · 「UI 内容单份」那条门禁只管行操作菜单（rowMenuItems）。
//   没有任何一条判据约束"上传的状态与分文件渲染只有一处"。所以上传面板可以
//   只剩一句总体进度、甚至给拖拽区挂一个**装饰性** drop handler 而什么都不入队 ——
//   语法全绿、门禁全绿，用户点开看到的却是一个空壳窗口。这正是一次真实报障。
//
// 一条门禁覆盖整类（只断言结构，不断言文案/CSS）：
//   ① 队列只有一处定义，且 files.js 只实例化一次（唯一的创建点）；
//   ② 逐文件行渲染只有一处（uploadRow），且 mountUploadProgress 真的遍历
//      队列条目（q.items / totals()）来画 —— 而不是自己维护一份文件状态；
//   ③ drop 绑定只有一处（addEventListener('drop' 恰好一次），且它调共享的
//      handleDrop —— 面板与页面不允许各写一份 drop。
//
// 负向对照（已实测变红）：
//   · 把 mountUploadProgress 改成不读队列（渲染来自本地数组）⇒ ② 红；
//   · 把面板的 drop 复制成第二份 addEventListener('drop' ⇒ ③ 红。

import (
	"strings"
	"testing"
)

func TestFilesUploadQueueSingleSourceGate(t *testing.T) {
	queue := readAssetJS(t, "uploadqueue.js")
	files := readAssetJS(t, "files.js")

	// ① 队列只有一处定义，files.js 只实例化一次。
	if n := strings.Count(queue, "export function createUploadQueue("); n != 1 {
		t.Fatalf("createUploadQueue 在 uploadqueue.js 里定义了 %d 次（必须恰好 1 次）—— 队列出现第二份定义了", n)
	}
	if n := strings.Count(files, "createUploadQueue("); n != 1 {
		t.Fatalf("files.js 里 createUploadQueue( 出现 %d 次（必须恰好 1 次：唯一的队列创建点）", n)
	}
	if !strings.Contains(queue, "function totals(") {
		t.Error("队列没有 totals()：总计区会在渲染层各自累加，两处必然打架")
	}

	// ② 逐文件行只有一处渲染器，且由进度渲染器从队列读出来画。
	if n := strings.Count(files, "function uploadRow("); n != 1 {
		t.Fatalf("uploadRow 定义了 %d 次（必须恰好 1 次）—— 分文件行出现第二份实现了", n)
	}
	body := jsFuncBody(t, files, "mountUploadProgress")
	if !strings.Contains(body, "uploadRow(") {
		t.Error("mountUploadProgress 不调用 uploadRow —— 每文件行不是从这一处渲染的")
	}
	if !strings.Contains(body, "q.items") {
		t.Error("mountUploadProgress 不遍历队列条目（q.items）—— 它在自己维护一份文件状态")
	}
	if !strings.Contains(body, "totals(") {
		t.Error("mountUploadProgress 不读队列 totals() —— 总计区在自己累加")
	}
	// 每文件进度条只允许出现在 uploadRow 里（"各行各写一份进度条"的判据）。
	if n := strings.Count(files, "zp-up-bar"); n != 1 {
		t.Fatalf("每文件进度条（zp-up-bar）在 files.js 里出现 %d 次（必须恰好 1 次，且只在 uploadRow 里）", n)
	}

	// ③ 拖拽只有一个 drop 绑定入口，且它调共享的 handleDrop。
	if n := strings.Count(files, "addEventListener('drop'"); n != 1 {
		t.Fatalf("addEventListener('drop' 在 files.js 里出现 %d 次（必须恰好 1 次）—— 拖拽入口被复制成第二份了", n)
	}
	wire := jsFuncBody(t, files, "wireDropZone")
	if !strings.Contains(wire, "handleDrop(") {
		t.Error("wireDropZone 的 drop 处理器不调用 handleDrop —— 那只是一个装饰性 handler，拖进来什么都不会入队")
	}
	// 判据有效性自检：必须有 ≥2 个真实调用点（页面卡片 + 上传面板），否则是在测空气。
	if n := strings.Count(files, "wireDropZone("); n < 3 {
		t.Errorf("wireDropZone( 只出现 %d 次（定义 + 页面 + 面板应 ≥3）—— 面板或页面的拖拽接线被删了", n)
	}
}

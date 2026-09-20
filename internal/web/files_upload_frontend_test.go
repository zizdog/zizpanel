package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ============================================================================
//  前端上传路径的不变量门禁
//
//  为什么放在 Go 测试里：面板前端是"无构建步骤"的原生 ESM，没有测试运行器。
//  而这几个不变量一旦破了，用户报障会原样复发。
//
//  的报障（978MB 上传"点了没反应"）里，前端有两个独立的原因：
//    1. ui.js 的 appendAll 只会 appendChild，而 files.js 拿它往 FormData 里塞字段
//       → `parent.appendChild is not a function`，抛在任何 toast 之前，
//         于是**任何大小**的上传都是彻底无声失败；
//    2. 用 fetch 上传 → 拿不到 upload.onprogress，大文件只剩一句干等的 toast。
//  这两条都不是"某一行写错"，而是"下次还会再犯"的类别，所以补成门禁。
//
//  真机报障（4.16 GB 文件夹被拒）的类别是"判据用错层级"：拿**整批总大小**去比
//  **单次请求上限**，还建议用户用同一个功能分流。对应的门禁见
//  upload_batch_gate_test.go（分批纯函数 + 文案不许把自己当解法）。
// ============================================================================

func readAssetJS(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("assets", "js", name))
	if err != nil {
		t.Fatalf("读不到前端资源 %s: %v", name, err)
	}
	return string(b)
}

// TestFilesFrontendUploadLimitIsReadNotHardcoded 锁"上限只有一个来源"。
//
// 旧实现把 4 GiB 写死在 files.js（还靠测试锁前后端两个数字相等）。写死必然
// 出现"提示里的数字不是真实生效的上限"，而上限一旦可配，写死的数字立刻说谎。
// 现在唯一来源是 config → GET /api/v1/files/upload-limit，前端必须回读。
func TestFilesFrontendUploadLimitIsReadNotHardcoded(t *testing.T) {
	js := readAssetJS(t, "files.js")
	if regexp.MustCompile(`MAX_UPLOAD`).MatchString(js) {
		t.Error("files.js 里还有写死的 MAX_UPLOAD —— 上限必须回读（api.fileUploadLimit）")
	}
	if !strings.Contains(js, "api.fileUploadLimit()") {
		t.Error("files.js 没有回读面板上传上限（api.fileUploadLimit）—— " +
			"提示里的数字会与真实生效值脱节")
	}
	// 未复核时必须如实说"未复核"，不许拿默认值冒充实测值。
	if !strings.Contains(js, "上限未复核") {
		t.Error("files.js 没有「未复核」这条如实说明 —— 读不到配置时会编造数字")
	}
}

// TestFilesFrontendUploadHasFolderEntry 必须有「上传文件夹」入口（webkitdirectory）。
func TestFilesFrontendUploadHasFolderEntry(t *testing.T) {
	js := readAssetJS(t, "files.js")
	for _, want := range []string{"webkitdirectory", "上传文件夹", "webkitRelativePath"} {
		if !strings.Contains(js, want) {
			t.Errorf("files.js 里缺少 %q —— 用户报障的诉求之一就是「没有上传文件夹选项」", want)
		}
	}
	// 必须真的发给后端，否则后端永远用不到相对路径
	if !strings.Contains(js, "relpaths") {
		t.Error("files.js 没有把相对路径（relpaths）发给后端")
	}
}

// TestFilesFrontendUploadUsesXHRProgress 上传必须用 XHR 的 upload.onprogress。
//
// fetch 没有"上传进度"能力（只有响应体的 reader），用它就永远做不出进度条，
// 用户看到的就是"没反应"。
func TestFilesFrontendUploadUsesXHRProgress(t *testing.T) {
	js := readAssetJS(t, "files.js")
	if !strings.Contains(js, "XMLHttpRequest") {
		t.Error("files.js 没有用 XMLHttpRequest —— fetch 拿不到 upload.onprogress，做不出进度")
	}
	if !strings.Contains(js, "upload.onprogress") {
		t.Error("files.js 没有监听 upload.onprogress —— 用户看不到「在动」")
	}
	for _, want := range []string{"onerror", "onabort", "ontimeout", "serverReason"} {
		if !strings.Contains(js, want) {
			t.Errorf("files.js 缺少 %s 分支 —— 失败路径会静默", want)
		}
	}
	// 上传不能再退回 fetch（有进度的事件只有 XHR 有）
	if regexp.MustCompile(`fetch\(\s*apiURL\(\s*['"]files/upload`).MatchString(js) {
		t.Error("files.js 又用 fetch 做上传了 —— 那会丢掉上传进度")
	}
}

// TestAppendAllRejectsFormDataLoudly 锁住 appendAll 对 FormData 的行为。
//
// 事故回顾：appendAll(fd, 'dir', cwd)（fd 是 FormData）在旧实现里抛
// `parent.appendChild is not a function`，而且抛在任何 toast 之前 →
// 用户看到的是彻底的无反应。修法有两层，两层都要有门禁：
//  1. 上传路径改用 fd.append(name, value) 显式调用（见下面的 wiring 测试）；
//  2. appendAll 遇到 FormData **大声报错**（描述性 TypeError），而不是留给
//     浏览器回一句 "2 arguments required" 这种看不懂的话。
func TestAppendAllRejectsFormDataLoudly(t *testing.T) {
	js := readAssetJS(t, "ui.js")
	if !strings.Contains(js, "parent instanceof FormData") {
		t.Error("ui.js 的 appendAll 没有显式识别 FormData —— " +
			"它必须大声拒绝并给出可操作的报错，而不是抛一句浏览器的 " +
			"「Failed to execute 'append' on 'FormData'」把整个上传悄悄杀掉")
	}
	if !strings.Contains(js, "appendAll 用于 DOM，不能追加到 FormData") {
		t.Error("ui.js 的 appendAll 缺少给后来人的可操作报错文案")
	}
	if !strings.Contains(js, "typeof parent.appendChild !== 'function'") {
		t.Error("ui.js 的 appendAll 没有区分 DOM / 非 DOM 目标")
	}
}

// TestFilesFrontendUploadWiring 保证文件管理器页面的上传确实接到新的实现，
// 并且没有任何可能"静默失败"的路径。
func TestFilesFrontendUploadWiring(t *testing.T) {
	js := readAssetJS(t, "files.js")
	if !strings.Contains(js, "uploadEntries(") {
		t.Fatal("files.js 里没有 uploadEntries 调用 —— 新的上传实现没有被接上")
	}
	// 三个入口（文件按钮、文件夹按钮、拖拽）都必须走 uploadEntries
	if n := strings.Count(js, "uploadEntries("); n < 3 {
		t.Errorf("uploadEntries 只出现 %d 次，期望至少 3 次（文件按钮 / 文件夹按钮 / 拖拽）", n)
	}
	// 旧的 uploadFiles 必须彻底消失（留着就是两条实现，迟早分叉）
	if regexp.MustCompile(`function\s+uploadFiles`).MatchString(js) {
		t.Error("files.js 里还留着旧的 uploadFiles —— 两条上传实现会分叉")
	}
	// FormData 必须显式 append（绝不能再用 DOM 助手 appendAll 塞 FormData）
	if !strings.Contains(js, "fd.append('dir', cwd)") {
		t.Error("files.js 没有用 fd.append('dir', cwd) 显式设置目标目录")
	}
	if regexp.MustCompile(`appendAll\(\s*fd\b`).MatchString(js) {
		t.Fatal("files.js 又用 appendAll 往 FormData 里塞字段了 —— " +
			"这正是「点上传没反应」的直接原因（异常抛在提示之前）")
	}
	// 任何未预料的异常都必须变成用户看得见的提示
	if !regexp.MustCompile(`catch\s*\(err\)\s*\{[^}]*toast\(`).MatchString(js) &&
		!strings.Contains(js, "'上传未能开始：'") {
		t.Error("files.js 的上传入口没有兜底 try/catch + toast —— 未捕获的异常会再次变成「没反应」")
	}
	// 拼 FormData 的异常必须显示在进度窗里
	if !strings.Contains(js, "无法准备上传数据") {
		t.Error("files.js 没有处理「拼请求体失败」这条路径（历史上它就是静默的那一条）")
	}
}

// TestFilesFrontendUploadAsksAboutConflicts 锁"同名文件必须先问用户"这条接线
// （用户："上传文件，如果相同名字应该询问是否覆盖还是共存"）。
//
// 为什么用静态断言而不是只靠后端测试：后端的两种策略早就存在，用户报的是
// **没人问他**。少了这三处任何一处，功能就等于没有：
//
//	① uploadEntries 发现重名要真去问；② 询问结果要能回传（on_conflict）；
//	③ 默认必须是"不覆盖"（覆盖是最危险的那个动作，绝不做默认值）。
func TestFilesFrontendUploadAsksAboutConflicts(t *testing.T) {
	js := readAssetJS(t, "files.js")
	for _, want := range []string{
		"askUploadConflict(",
		"conflictNames(",
		"fd.append('on_conflict', onConflict)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("files.js 缺少 %s —— 同名文件就不会问用户（或问了也传不到后端）", want)
		}
	}
	// 默认选择必须是「保留两者」：不能让覆盖成为默认值
	if !strings.Contains(js, "let picked = 'rename'") {
		t.Error("冲突弹窗的默认选项必须是「保留两者」（rename）；默认覆盖是不可逆的危险行为")
	}
	// 上传文件夹不弹这个窗（它的语义就是按原结构覆盖，进度窗里已明说）
	if !strings.Contains(js, "if (!opts.folder) {") {
		t.Error("只有普通上传才该弹「覆盖/共存」；上传文件夹是「按原结构覆盖」的语义")
	}
}

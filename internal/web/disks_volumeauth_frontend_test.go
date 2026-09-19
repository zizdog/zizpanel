package web

// disks_volumeauth_frontend_test.go —— 「磁盘管理」页空状态 + 申请授权按钮的静态资产门禁。
//
// 前端是无构建步骤的原生 ESM，跑不了单测；这里按 internal/web 既有的 readAssetJS 风格
// 锁住**接线**（不只是"字符串在不在"）：
//   · 空状态必须真的渲染成标题节点，且挂在"没有外接盘"的条件上；
//   · 「申请授权」按钮的可用性必须由"有没有盘 + 有没有人在场"共同决定；
//   · 按钮必须真的走任务中心打新接口（不是同步请求，也不是只画了个按钮）；
//   · 侧栏 NAV 的旧名「磁盘」必须换成「磁盘管理」（且不许留下旧名）。

import (
	"regexp"
	"strings"
	"testing"
)

func TestDisksFrontendHasEmptyStateAndAuthButtonWiring(t *testing.T) {
	js := readAssetJS(t, "disks.js")

	// 空状态：必须是**真的渲染出来的节点**（只在注释里出现不算），而且挂在"没有外接盘"的分支上。
	if !regexp.MustCompile(`h\('h4',\s*\{\s*text:\s*'您当前没有外接磁盘'\s*\}\)`).MatchString(js) {
		t.Error("空状态必须把「您当前没有外接磁盘」渲染成一个 h4 节点（不只是注释/字符串里出现过）")
	}
	if !strings.Contains(js, "!groups.length && !hasVolumes") {
		t.Error("disks.js 的空状态必须挂在 `!groups.length && !hasVolumes`（没有外接盘）这个条件上")
	}

	// 按钮可用性 = 有盘 且 有会话（少了任一半，没人在场也会去弹窗）。
	re := regexp.MustCompile(`const\s+disabled\s*=\s*([^;]+);`)
	m := re.FindStringSubmatch(js)
	if m == nil {
		t.Fatal("disks.js 里找不到 `const disabled = ...;` —— 「申请授权」按钮的可用性判据不见了")
	}
	if !strings.Contains(m[1], "hasVolumes") || !strings.Contains(m[1], "hasSession") {
		t.Errorf("按钮禁用条件必须同时看「有没有盘」和「有没有会话」，实际：%s", m[1])
	}

	// 两种"不能点"的原因必须在**按钮的状态里**（不能只躺在注释里）。
	why := regexp.MustCompile(`const\s+why\s*=\s*([\s\S]*?);`).FindStringSubmatch(js)
	if why == nil {
		t.Fatal("disks.js 里找不到 `const why = ...;` —— 按钮禁用原因不见了")
	}
	for _, want := range []string{"先接上外接硬盘", "现在没人在机器前"} {
		if !strings.Contains(why[1], want) {
			t.Errorf("按钮禁用原因里必须逐字包含 %q，实际：%s", want, why[1])
		}
	}

	// 状态字段必须真的从后端下发里读（少了它按钮就永远禁用/永远可用）。
	if !regexp.MustCompile(`data\.volume_auth`).MatchString(js) || !strings.Contains(js, "has_console_session") {
		t.Error("按钮状态必须读后端下发的 volume_auth（含 has_console_session）")
	}

	// 按钮必须真的走任务中心（202 + task_id，进度走 SSE），不是同步请求。
	if !regexp.MustCompile(`taskCenter\.start\(\{`).MatchString(js) {
		t.Error("「申请授权」必须走 taskCenter.start（任务中心），不能做成同步请求")
	}
	if !regexp.MustCompile(`apiURL\(\s*'system/disks/volume-auth'\s*\)`).MatchString(js) {
		t.Error("「申请授权」按钮必须调 POST system/disks/volume-auth")
	}
	if !strings.Contains(js, "disk_volume_auth") {
		t.Error("任务 kind 必须是 disk_volume_auth（失败时结果区据此再给按钮）")
	}
	// 请求体必须带 confirm:true —— 后端强制显式确认，不带就是 4xx。
	if !regexp.MustCompile(`system/disks/volume-auth'\s*\)\s*,\s*\{\s*confirm:\s*true\s*\}`).MatchString(js) {
		t.Error("「申请授权」请求体必须带 {confirm:true}（后端要求的显式确认）")
	}
}

// TestDisksRequestAuthConfirmBoxBeforeRequest —— 二次确认的门禁（坑 192）。
//
// 只断言"字符串存在"是不够的：确认框可以写了却没接在按钮路径上，也可以放在请求之后
// （那时任务已经建了，确认框形同虚设）。所以这里**按函数体 + 顺序**断言：
//
//	① requestAuth 函数体里 confirmBox( 必须出现在 apiURL('system/disks/volume-auth') 之前；
//	② 确认文案里必须逐字写明"请在这台机器的屏幕上点「允许」"（那正是要用户做的事）。
func TestDisksRequestAuthConfirmBoxBeforeRequest(t *testing.T) {
	js := readAssetJS(t, "disks.js")

	body := requestAuthBody(t, js)
	if body == "" {
		t.Fatal("disks.js 里找不到 requestAuth 函数体（确认框的接线门禁失效）")
	}
	reqAt := regexp.MustCompile(`apiURL\(\s*'system/disks/volume-auth'\s*\)`).FindStringIndex(body)
	if reqAt == nil {
		t.Fatal("requestAuth 函数体里找不到 apiURL('system/disks/volume-auth') —— 确认框没接在按钮路径上？")
	}
	// ① 顺序：函数体里**每一处** confirmBox 都必须在发请求之前。
	all := regexp.MustCompile(`confirmBox\(`).FindAllStringIndex(body, -1)
	if len(all) == 0 {
		t.Fatal("申请授权必须先弹二次确认框（ui.js 的 confirmBox）—— 确认框没接在按钮路径上")
	}
	for _, m := range all {
		if m[0] > reqAt[0] {
			t.Errorf("确认框必须在发请求**之前**（先 confirmBox 后 apiURL）：confirmBox@%d 晚于请求@%d", m[0], reqAt[0])
		}
	}
	// ② 取消 = 什么都不做：必须在确认之后、发请求之前就 return。
	if !regexp.MustCompile(`if\s*\(\s*!\s*ok\s*\)\s*return\s*;`).MatchString(body[:reqAt[0]]) {
		t.Error("用户取消时必须直接 return（不发请求、不建任务）")
	}

	// ③ 确认文案关键字：必须告诉用户在**这台机器的屏幕上点「允许」**。
	if !strings.Contains(body, "屏幕上点「允许」") {
		t.Error("确认框正文必须含「屏幕上点「允许」」（用户要知道去哪儿点）")
	}
	if !strings.Contains(body, "可移除宗卷") {
		t.Error("确认框正文必须写明系统弹窗的原文关键字「可移除宗卷」")
	}
}

// requestAuthBody 取出 requestAuth 的**函数体**（从函数头到下一个顶层 function/const）。
//
// 为什么要按函数体取：坑 191 的教训是"字符串在文件里存在"不等于"接在按钮路径上"；
// 确认框写在别的函数里、或写在请求之后，都必须判红。
func requestAuthBody(t *testing.T, js string) string {
	t.Helper()
	head := strings.Index(js, "async function requestAuth(")
	if head < 0 {
		return ""
	}
	rest := js[head:]
	// 下一个顶层声明（`async function` / `function` / `const` 开头）就是函数体边界。
	next := regexp.MustCompile(`\n  (?:async function |function |const )`).FindStringIndex(rest[1:])
	if next == nil {
		return rest
	}
	return rest[:next[0]+1]
}

func TestAppJsNavDiskTitleIsDiskManagement(t *testing.T) {
	js := readAssetJS(t, "app.js")

	re := regexp.MustCompile(`\{\s*id:\s*'disks',\s*title:\s*'([^']+)'`)
	m := re.FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js 的 NAV 里找不到 id 'disks' 的条目")
	}
	if m[1] != "磁盘管理" {
		t.Errorf("侧栏「磁盘」应改名「磁盘管理」，实际 %q", m[1])
	}
	// 负向对照：不许留下旧的侧栏名。
	if regexp.MustCompile(`title:\s*'磁盘'\s*[,}]`).MatchString(js) {
		t.Error("app.js 里还留着旧侧栏名 title: '磁盘'")
	}
}

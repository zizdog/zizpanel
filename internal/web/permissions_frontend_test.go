package web

// permissions_frontend_test.go —— 「权限」页前端的静态资产门禁。
//
// 前端是无构建步骤的原生 ESM，跑不了单测；这里锁**接线**而不是"字符串在不在"：
//   · 「申请」必须先二次确认再发请求（坑 191），取消 = 什么都不做；
//   · 请求必须走任务中心 + 带 confirm:true。

import (
	"regexp"
	"strings"
	"testing"
)

// TestPermissionsMountedInPanelSettings：「权限」2026-09-20 从侧栏独立项搬进
// 「面板设置」的一个 Tab，且侧栏不再留第二个入口。判据锁"接线"：
//
//	· app.js 的 NAV 里**不许**再有 id 'permissions'（否则又是两处入口）；
//	· 旧 hash `#/permissions` 必须被别名到 settings 的 permissions Tab（书签不白屏）；
//	· views.js 的 SettingsView 必须真的导入并调用 PermissionsView（不是只写了个标题）。
func TestPermissionsMountedInPanelSettings(t *testing.T) {
	appJS := readAssetJS(t, "app.js")
	if regexp.MustCompile(`\{\s*id:\s*'permissions'`).MatchString(appJS) {
		t.Error("app.js 的 NAV 里不该再有 'permissions' 条目 —— 它已搬进「面板设置」")
	}
	if !regexp.MustCompile(`permissions:\s*\{\s*id:\s*'settings',\s*tab:\s*'permissions'\s*\}`).MatchString(appJS) {
		t.Error("app.js 必须把旧 hash #/permissions 别名到面板设置的 permissions Tab")
	}

	viewsJS := readAssetJS(t, "views.js")
	if !strings.Contains(viewsJS, "import { PermissionsView } from './permissions.js'") {
		t.Error("views.js 必须真的导入 PermissionsView（否则设置页里是空引用）")
	}
	if !regexp.MustCompile(`\{\s*id:\s*'permissions',\s*title:\s*'权限'\s*\}`).MatchString(viewsJS) {
		t.Error("面板设置必须有标题「权限」的 Tab")
	}
	if !regexp.MustCompile(`active === 'permissions'`).MatchString(viewsJS) {
		t.Error("SettingsView 的 renderBody 必须处理 permissions Tab")
	}
	if !regexp.MustCompile(`PermissionsView\(\s*box`).MatchString(viewsJS) {
		t.Error("面板设置必须真的调用 PermissionsView(...) 渲染权限内容")
	}
}

func TestPermissionsFrontendRequestWiring(t *testing.T) {
	js := readAssetJS(t, "permissions.js")

	if !regexp.MustCompile(`apiURL\(\s*'permissions'\s*\)`).MatchString(js) {
		t.Error("列表必须调 GET permissions（相对 apiURL）")
	}
	if !regexp.MustCompile(`taskCenter\.start\(\{`).MatchString(js) {
		t.Error("「申请」必须走 taskCenter.start（任务中心），不能做成同步请求")
	}
	if !strings.Contains(js, "permission_apply") {
		t.Error("任务 kind 必须是 permission_apply")
	}
	if !regexp.MustCompile(`apiURL\(\s*'permissions/'\s*\+\s*encodeURIComponent\(`).MatchString(js) {
		t.Error("申请必须调 POST permissions/<id>/apply（id 要转义）")
	}
	// 请求体必须带 confirm:true —— 后端强制显式确认，不带就是 4xx。
	if !regexp.MustCompile(`confirm:\s*true`).MatchString(js) {
		t.Error("申请请求体必须带 confirm:true（后端要求的显式确认）")
	}
}

// TestPermissionsApplyConfirmBeforeRequest：只断言"字符串存在"不够 ——
// 确认框可以写在别的函数里、也可以放在请求之后（那时任务已经建了）。
func TestPermissionsApplyConfirmBeforeRequest(t *testing.T) {
	js := readAssetJS(t, "permissions.js")
	body := jsFuncBody(t, js, "applyPermission")
	if body == "" {
		t.Fatal("找不到 applyPermission 函数体")
	}

	reqAt := regexp.MustCompile(`apiURL\(\s*'permissions/'`).FindStringIndex(body)
	if reqAt == nil {
		t.Fatal("applyPermission 里找不到申请请求 —— 确认框没接在按钮路径上？")
	}
	all := regexp.MustCompile(`confirmBox\(`).FindAllStringIndex(body, -1)
	if len(all) == 0 {
		t.Fatal("申请必须先弹二次确认框（ui.js 的 confirmBox）")
	}
	for _, m := range all {
		if m[0] > reqAt[0] {
			t.Errorf("确认框必须在发请求之前：confirmBox@%d 晚于请求@%d", m[0], reqAt[0])
		}
	}
	if !regexp.MustCompile(`if\s*\(\s*!\s*ok\s*\)\s*return\s*;`).MatchString(body[:reqAt[0]]) {
		t.Error("用户取消时必须直接 return（不发请求、不建任务）")
	}
	// 确认文案要说清三件事：会读一次、系统会弹窗、需要你在机器前点。
	for _, want := range []string{"读一次", "在这台机器的屏幕上点「允许」", "记成拒绝"} {
		if !strings.Contains(body, want) {
			t.Errorf("确认文案必须含 %q（用户要知道会发生什么）", want)
		}
	}
}

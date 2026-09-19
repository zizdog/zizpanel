package web

import (
	"os"
	"strings"
	"testing"
)

// TestUpgradeLongOpsAreNotTiedToRequestContext 是"用户刷新别把升级下载杀掉"的门禁。
//
// 2026-09-19 真机事故（用户原话）：
//
//	升级未执行：Get "https://zizdog.com/zizpanel/download/1.4.1/zizpanel_1.4.1_darwin_arm64.tar.gz": context canceled
//
// 根因是 `context.WithTimeout(r.Context(), 15*time.Minute)` —— 下载挂在**请求上下文**上，
// 用户在下载期间刷新/切走页面就把它取消了。这正是 AGENTS 第三节记的那一类：
// "任务挂在 r.Context() 上 —— 用户一刷新就把任务杀了"，反复复发，所以这里用源码门禁钉死。
//
// 判据很直接：`api_upgrade.go` 里**不许**再出现 `WithTimeout(r.Context()`。
// （读取请求、审计这些短操作当然仍可以用 r.Context()，门禁只盯这个容易写错的组合。）
func TestUpgradeLongOpsAreNotTiedToRequestContext(t *testing.T) {
	b, err := os.ReadFile("api_upgrade.go")
	if err != nil {
		t.Fatalf("读不到 api_upgrade.go：%v", err)
	}
	src := string(b)
	if strings.Contains(src, "WithTimeout(r.Context()") {
		t.Error("api_upgrade.go 里还有 `WithTimeout(r.Context(), ...)`：\n" +
			"「检查更新」与「升级下载」都是长任务，不能挂在请求上下文上 ——\n" +
			"用户刷新/切走页面会把它取消成 `context canceled`（2026-09-19 真机）。\n" +
			"请改用 `context.WithoutCancel(r.Context())` + 自己的超时。")
	}
	// 反向确认修复真的在（避免有人把整段删掉却以为"没有坏写法"就等于修好了）。
	if !strings.Contains(src, "context.WithoutCancel(r.Context())") {
		t.Error("api_upgrade.go 里没有见到 `context.WithoutCancel(r.Context())`：\n" +
			"升级相关的长任务应当是「断开请求取消 + 自己兜底超时」，而不是什么都没写。")
	}
}

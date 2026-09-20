package web

import (
	"os"
	"strings"
	"testing"
)

// TestTerminalWSFailureFrontendIsActionable 门禁（坑 219）：终端页连不上时
// 必须显示可行动结论（含"WebSocket 透传"），不许只剩错误码；结论由服务端给，
// 前端只负责取回并折叠展示。
//
// 真正的语法门禁是 tools/check-js-syntax.mjs；这里防"接线又断回只显示 1006"。
func TestTerminalWSFailureFrontendIsActionable(t *testing.T) {
	src := readJSSource(t, "assets/js/terminal.js")

	for _, need := range []string{
		"WebSocket 透传",        // 可行动字样
		"terminalWSDiagnose(", // 向面板取回结论（浏览器拿不到失败握手响应体）
		"showConnectFailure",  // 首行结论 + 折叠细节的渲染
		"h('details'",         // 细节折叠，别把长文糊在首屏
		"diagnoseAndShow(",    // 从未连上时才去要结论
		"v.advice",            // 文案由服务端给，前端不改写
	} {
		if !strings.Contains(src, need) {
			t.Errorf("assets/js/terminal.js 里缺少 %q —— 失败原因会被吞", need)
		}
	}

	// 旧写法（只给错误码 + 重连）不许回来。
	if strings.Contains(src, `连接已断开（代码 ${ev.code}）`) {
		t.Error("只显示错误码的旧文案回来了：必须显示可行动结论")
	}

	api := readJSSource(t, "assets/js/api.js")
	if !strings.Contains(api, "terminalWSDiagnose:") {
		t.Error("assets/js/api.js 缺 terminalWSDiagnose —— 前端拿不到服务端结论")
	}
}

func readJSSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	return string(b)
}

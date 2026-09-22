package web

// 前端门禁：Transmission 的 RPC 设置入口必须①走共享 request（非 2xx / ok:false 会抛），
// ②把失败原样显示出来（不许静默），③明确劝退"手改 settings.json 再重启"（坑 226）。
//
// 读的是**真正被浏览器加载的** assets/js（不是测试里另抄一份规则）。

import (
	"strings"
	"testing"
)

func TestTransmissionFrontendSurfacesRPCErrors(t *testing.T) {
	api := readAssetJS(t, "api.js")
	for _, needle := range []string{
		"transmissionSettings: (name) =>",
		"setTransmissionSettings: (name, body) =>",
		"/transmission`",
	} {
		if !strings.Contains(api, needle) {
			t.Errorf("api.js 缺少 %q —— 调用必须走共享 request（它会对非 2xx 抛错）", needle)
		}
	}
	// 两条都必须真的用 request(...) 发出去（自己写 fetch 会绕过统一的错误处理）。
	if strings.Count(api, "setTransmissionSettings: (name, body) =>") != 1 ||
		!strings.Contains(api, "request('POST', `${API_BASE}/services/${encodeURIComponent(name)}/transmission`, body)") {
		t.Error("setTransmissionSettings 必须用 request('POST', …) 调用，不得绕过统一错误处理")
	}

	svc := readAssetJS(t, "services.js")
	for _, needle := range []string{
		"openTransmissionSettingsModal",
		"'homebrew.mxcl.transmission-cli', 'sh.brew.transmission-cli'",
		"api.setTransmissionSettings(",
		// 失败必须显示后端原文，绝不沉默（坑 154）。
		"task.status !== 'succeeded'",
		"toast('修改失败：'",
		// 明确劝退手改配置（坑 226）。
		"不要手工编辑 settings.json",
	} {
		if !strings.Contains(svc, needle) {
			t.Errorf("services.js 缺少契约：%q", needle)
		}
	}
	// 两个 label 都必须注册出按钮（brew 两套前缀都可能写进服务记录）。
	for _, label := range []string{"TRANSMISSION_LABELS[0]", "TRANSMISSION_LABELS[1]"} {
		if !strings.Contains(svc, "["+label+"]") {
			t.Errorf("WIDGET_BUTTONS 必须为 %s 注册「⚙️ RPC 设置」按钮", label)
		}
	}

	// 类级门禁：任何调用 setTransmissionSettings 的 JS 文件都必须带失败显示，
	// 否则以后有人新写一个页面又会静默吞掉错误。
	for _, name := range listAssetJS(t) {
		body := readAssetJS(t, name)
		if !strings.Contains(body, "setTransmissionSettings(") {
			continue
		}
		if !strings.Contains(body, "'err'") || !strings.Contains(body, "onDone") {
			t.Errorf("assets/js/%s 调用了 setTransmissionSettings 却没有失败提示（onDone + toast 'err'）", name)
		}
	}
}

package web

// nav_listen_frontend_test.go —— 「导航页独立端口」设置项的前端接线门禁（坑 222）。
//
// 前端是无构建步骤的原生 ESM，跑不了单测，所以这里锁**接线**而不是"字符串在不在"：
//   · 保存请求体的字段名必须与 settingsReq 的 json tag 逐字一致
//     （不一致 = 用户点了保存、后端当没看见，静默不生效）；
//   · 界面显示的"本地地址"必须来自后端回读（nav_listen_url），
//     不许拿输入框的值在浏览器里拼一个"看起来生效了"的地址（坑 164 同类）。

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestNavListenSettingsFieldsMatchBackend 锁前后端字段名同源。
func TestNavListenSettingsFieldsMatchBackend(t *testing.T) {
	views := readAssetJS(t, "views.js")

	tags := map[string]bool{}
	rt := reflect.TypeOf(settingsReq{})
	for i := 0; i < rt.NumField(); i++ {
		tags[strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for _, key := range []string{"nav_listen_port", "nav_listen_enabled"} {
		if !tags[key] {
			t.Errorf("settingsReq 缺少 json tag %q —— 前端发了也会被后端丢掉", key)
		}
		if !strings.Contains(views, key) {
			t.Errorf("views.js 没有把 %q 放进保存请求体，设置项等于没接线", key)
		}
	}

	// 生效状态字段必须由后端回传、前端消费（缺一个都会让界面显示不出真实状态）。
	// 后端确实下发这几个键由 TestNavListenSettingsEndpointAppliesAndReports 断言。
	for _, key := range []string{"nav_listen_url", "nav_listen_running", "nav_listen_active_port", "nav_listen_error"} {
		if !strings.Contains(views, key) {
			t.Errorf("views.js 没有消费 %q —— 生效值必须来自后端回读", key)
		}
	}
}

// TestNavListenFrontendNeverFabricatesURL 禁止在浏览器里拼"生效地址"。
func TestNavListenFrontendNeverFabricatesURL(t *testing.T) {
	views := readAssetJS(t, "views.js")
	if strings.Contains(views, "http://127.0.0.1:") {
		t.Error("views.js 不许自己拼 127.0.0.1 地址：显示的必须是后端回读的 nav_listen_url")
	}
	// 开关 + 端口输入 + 生效地址三件套都要有（缺一个用户就填不进隧道配置）。
	for _, needle := range []string{"zp-nav-port-enabled", "zp-nav-port-input", "zp-nav-port-url"} {
		if !strings.Contains(views, needle) {
			t.Errorf("views.js 缺少 %s 这个设置项锚点", needle)
		}
	}
	// 端口输入必须与后端同一个范围（1024~65535），否则用户先撞墙才知道上限。
	if !regexp.MustCompile(`min:\s*1024[^}]*max:\s*65535`).MatchString(views) {
		t.Error("端口输入框缺 min:1024 / max:65535（与 ValidateNavListenPort 同范围）")
	}
	// 隧道那条提示必须给出 service 写法，用户才知道填什么。
	if !strings.Contains(views, "service = ") {
		t.Error("界面必须显示隧道要填的 service = \"127.0.0.1:<端口>\"")
	}
}

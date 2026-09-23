package services

// 类级门禁（用户 2026-09-23：「所有应用 ip:端口都要允许 0.0.0.0:端口，我需要局域网访问」）：
// 应用自己监听的地址必须显式是 0.0.0.0，谁都不许靠"空值 = 回环"的默认值悄悄退回本机
// （那种回归用户只看到"局域网打不开"，没有任何报错）。
//
// 为什么逐条断言不够：三个原生 plist + 描述符 + brew 应用各写各的绑定字符串，
// 漏一个就少一个入口 —— 这里按**注册表**遍历，新增应用自动纳管。

import (
	"strings"
	"testing"
)

// allInterfaces 是"允许局域网直连"的唯一写法。
const allInterfaces = "0.0.0.0"

// bindIsAllInterfaces 抽成函数，便于负向对照（见测试末尾）。
func bindIsAllInterfaces(bind string) bool {
	return strings.TrimSpace(bind) == allInterfaces
}

func TestReleaseAppsExplicitlyBindAllInterfaces(t *testing.T) {
	if len(releaseBinaryApps) == 0 {
		t.Fatal("releaseBinaryApps 为空，门禁等于没跑")
	}
	for id, spec := range releaseBinaryApps {
		if spec.Port <= 0 {
			continue // 不监听端口（纯 CLI）不适用
		}
		if !bindIsAllInterfaces(spec.BindAddress) {
			t.Errorf("%s 的 BindAddress=%q：必须显式写 %q，否则局域网打不开（空值会被当成 127.0.0.1）",
				id, spec.BindAddress, allInterfaces)
		}
		// 参数里写死回环的也算退化（描述符说 0.0.0.0、参数却绑回环）。
		joined := strings.Join(spec.Args, " ")
		for _, loop := range []string{"127.0.0.1", "localhost"} {
			if strings.Contains(joined, loop) {
				t.Errorf("%s 的启动参数里出现 %q（用户要求绑 0.0.0.0）：%s", id, loop, joined)
			}
		}
	}
}

func TestNativeAppPlistsBindAllInterfaces(t *testing.T) {
	cases := []struct {
		name  string
		plist string
		port  string
	}{
		{"stt", sttPlist("/bin/zp", STTPort, "/opt/homebrew", "/tmp/r", "small",
			"https://mirror.example.com", "u", "/o", "/e"), allInterfaces + ":8892"},
		{"imgcompress", imgCompressPlist("/bin/zp", ImgCompressPort, "/opt/homebrew", "zizdog", "/o", "/e"),
			allInterfaces + ":8890"},
		{"macspeech", macSpeechPlist("/bin/zp", MacSpeechPort, "zizdog", "/o", "/e"),
			allInterfaces + ":8891"},
	}
	for _, c := range cases {
		if !strings.Contains(c.plist, "<string>"+c.port+"</string>") {
			t.Errorf("%s 的 plist 里没有 %q：局域网打不开", c.name, c.port)
		}
	}
}

// 负向对照：门禁必须能失败 —— 回环、空值、主机名都不算"绑 0.0.0.0"。
func TestBindIsAllInterfacesNegativeControl(t *testing.T) {
	for _, bad := range []string{"127.0.0.1", "", "localhost", "::1", "  "} {
		if bindIsAllInterfaces(bad) {
			t.Errorf("%q 被误判成绑 0.0.0.0，门禁失效", bad)
		}
	}
	if !bindIsAllInterfaces(" 0.0.0.0 ") {
		t.Error("0.0.0.0 应被接受")
	}
}

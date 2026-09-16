package services

import "testing"

// TestExtractCredentials 锁住"服务详情里能看到登录凭据"。
//
// 真机反馈（2026-09-16）：面板给 frpc 随机生成了 admin UI 的用户名/口令，
// 但只在安装任务日志里出现过一次，用户要登录 7400 时找不到 →
// "安装的时候也没让设置啊！我用你妈逼登陆吗"。
// 所以必须能从面板生成的配置里把凭据解析出来。
//
// 同时锁住**不要多拿**：serverAddr / localPort 这类不是密码，不该被当成凭据
// 展示（把它们混进去会让人误以为都是机密，也更容易被骗着贴出去）。
func TestExtractCredentials(t *testing.T) {
	cfg := `# 由 ZizPanel 生成。改完重启服务生效。
serverAddr = "127.0.0.1"
serverPort = 7000
auth.method = "token"
auth.token = "abc123def456"

webServer.addr = "0.0.0.0"
webServer.port = 7400
webServer.user = "d4b3ad5a"
webServer.password = "c093b51b5b5eb025"
`
	got := ExtractCredentials(cfg)
	byKey := map[string]string{}
	for _, c := range got {
		byKey[c.Key] = c.Value
	}
	for k, want := range map[string]string{
		"webServer.user":     "d4b3ad5a",
		"webServer.password": "c093b51b5b5eb025",
		"auth.token":         "abc123def456",
	} {
		if byKey[k] != want {
			t.Errorf("凭据 %s 应为 %q，实际 %q（全部：%+v）", k, want, byKey[k], got)
		}
	}
	// 不是凭据的键不能被带出来
	for _, bad := range []string{"serverAddr", "serverPort", "webServer.port", "webServer.addr"} {
		if _, ok := byKey[bad]; ok {
			t.Errorf("%s 不是凭据，不该出现在结果里", bad)
		}
	}
	// 空值不该出现（否则界面上是一行没用的空输入框）
	if len(ExtractCredentials("webServer.password = \"\"\n")) != 0 {
		t.Error("空值不该被当成凭据")
	}
	// 重复键只留一条
	if n := len(ExtractCredentials("webServer.user = \"a\"\nwebserver.user = \"b\"\n")); n != 1 {
		t.Errorf("同一个键重复出现应只留一条，实际 %d 条", n)
	}
}

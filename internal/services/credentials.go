package services

import (
	"regexp"
	"strings"
)

// Credential 是一条"用户要拿去登录"的凭据（来自面板生成的应用配置）。
type Credential struct {
	// Key 是配置里的原始键名（如 webServer.user），保留原样方便用户对照文件
	Key string `json:"key"`
	// Value 是键的值
	Value string `json:"value"`
	// Label 是给人看的中文说明
	Label string `json:"label"`
}

// credLineRe 匹配 `key = "value"` 形式（面板生成的配置都是这种单行写法）。
var credLineRe = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_.\[\]-]+)\s*=\s*"([^"]*)"\s*$`)

// credKeySuffixes 是"算作凭据"的键名后缀（小写比较）。
//
// 刻意用后缀白名单而不是"把所有键都列出来"：配置文件里还有 serverAddr、
// localPort 这类**不是密码**的值，全列出来既没用又会让人误以为都是机密。
var credKeySuffixes = []string{"user", "username", "password", "passwd", "token", "secret"}

// credLabels 给常见键配中文说明（查不到就留空，前端直接显示键名）。
var credLabels = map[string]string{
	"webserver.user":     "管理界面用户名",
	"webserver.password": "管理界面口令",
	"auth.token":         "鉴权 token（需与服务端一致）",
	"user":               "用户名",
	"password":           "口令",
	"token":              "token",
}

// ExtractCredentials 从面板生成的应用配置里挑出凭据行。
//
// 为什么需要：面板给 frpc 这类应用**随机生成** admin UI 的用户名/口令，
// 写进 frpc.toml 之后只在安装任务日志里出现过一次 —— 用户事后要登录 7400
// 只能自己翻配置文件，甚至根本不知道有凭据这回事
// （2026-09-16 用户原话："安装的时候也没让设置啊！我用你妈逼登陆吗"）。
//
// 安全边界：本函数**只被"面板管理的应用配置文件"调用**（见 web 层
// handleServiceCredentials），不是通用的密钥扫描器 —— 通用扫描很容易
// 把不该透出的东西带出来。
func ExtractCredentials(cfg string) []Credential {
	var out []Credential
	seen := map[string]bool{}
	for _, m := range credLineRe.FindAllStringSubmatch(cfg, -1) {
		key, val := m[1], m[2]
		if strings.TrimSpace(val) == "" {
			continue
		}
		lower := strings.ToLower(key)
		ok := false
		for _, suf := range credKeySuffixes {
			if strings.HasSuffix(lower, suf) {
				ok = true
				break
			}
		}
		if !ok || seen[lower] {
			continue
		}
		seen[lower] = true
		label := credLabels[lower]
		if label == "" {
			label = key
		}
		out = append(out, Credential{Key: key, Value: val, Label: label})
	}
	return out
}

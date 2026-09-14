package services

import (
	"path/filepath"
	"strings"
)

// ============================================================================
//  服务的显示名
//
//  背景：面板纳管本机服务时，注册表里的 display_name 直接存了 launchd 标签
//  （`sh.brew.mysql@8.4`、`com.zizdog.qwen3tts`）。技术上没错，但对用户是噪音：
//  界面上该显示的是「MySQL 8.4」而不是 `sh.brew.mysql@8.4`。
//
//  这里提供一个**纯函数**做映射，并在读取注册表时兜底应用 —— 这样已经登记过的
//  老记录不用重装/重新纳管也能立刻显示成友好名。
//
//  刻意不做的事：不覆盖用户自己起过的名字。只有 display_name 为空、或与标签/名字
//  完全相同时才替换，避免把「Docker 运行时（Colima）」这种手写名改掉。
// ============================================================================

// prettyFormula 是常见 formula 的官方大小写写法。
//
// 为什么要手写这张表：`strings.Title("mysql")` 得到 "Mysql"，而产品名是 "MySQL"。
// 这种"看起来像笔误"的小写错误会一直被人看到，值得列全。
var prettyFormula = map[string]string{
	"mysql":         "MySQL",
	"mariadb":       "MariaDB",
	"php":           "PHP",
	"nginx":         "Nginx",
	"httpd":         "Apache httpd",
	"apache":        "Apache",
	"python":        "Python",
	"node":          "Node.js",
	"npm":           "npm",
	"redis":         "Redis",
	"postgresql":    "PostgreSQL",
	"postgres":      "PostgreSQL",
	"mongodb":       "MongoDB",
	"elasticsearch": "Elasticsearch",
	"opensearch":    "OpenSearch",
	"grafana":       "Grafana",
	"minio":         "MinIO",
	"ollama":        "Ollama",
	"colima":        "Colima",
	"docker":        "Docker",
	"qwen3tts":      "Qwen3 TTS",
	"voicereceiver": "TtsVoice 音色接收端",
	"iopaint":       "IOPaint",
	"phpmyadmin":    "phpMyAdmin",
	"uptime-kuma":   "Uptime Kuma",
	"stirling-pdf":  "Stirling PDF",
	"tailscale":     "Tailscale",
}

// labelPrefixes 是各来源给 launchd 标签加的前缀。剥掉它们才能看到"软件名"。
//
// `sh.brew.` 是这台机器上真实出现的（Homebrew 前缀被定制过），
// `homebrew.mxcl.` 是标准写法，两套都得认。
var labelPrefixes = []string{
	"homebrew.mxcl.",
	"sh.brew.",
	"com.zizdog.",
	"cn.zizpanel.",
}

// FriendlyName 把 launchd 标签/服务名映射成界面上的软件名。
//
//	sh.brew.mysql@8.4   → MySQL 8.4
//	homebrew.mxcl.nginx → Nginx
//	sh.brew.php@8.3     → PHP 8.3
//	com.zizdog.qwen3tts → Qwen3 TTS
//	com.apple.foo        → 原样返回（不是我们的东西，别乱猜）
func FriendlyName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// 剥前缀（可能叠加，例如 com.zizdog.iopaint 之外还有更长的）
	base := s
	for _, p := range labelPrefixes {
		if strings.HasPrefix(base, p) {
			base = strings.TrimPrefix(base, p)
			break
		}
	}

	// 名字 + 版本：mysql@8.4 / php@8.3 / node@20
	name, version := base, ""
	if i := strings.Index(base, "@"); i > 0 {
		name = base[:i]
		version = base[i+1:]
	}

	// 只认我们认识的名字；不认识的（例如 com.apple.*、用户自定义）原样返回 ——
	// 猜错比不猜更糟。
	pretty, ok := prettyFormula[strings.ToLower(name)]
	if !ok {
		return s
	}
	if version != "" {
		return pretty + " " + version
	}
	return pretty
}

// displayNameOf 决定一条服务记录该显示什么名字。
//
// 只在"没起过名字"或"名字与标签/服务名完全一样"时才替换 —— 用户手写的名字
// （比如「Docker 运行时（Colima）」）必须保留。
func displayNameOf(s *Service) string {
	cur := strings.TrimSpace(s.DisplayName)
	if cur == "" || cur == s.Name || cur == s.LaunchLabel {
		if f := FriendlyName(s.LaunchLabel); f != "" && f != s.LaunchLabel {
			return f
		}
		if f := FriendlyName(s.Name); f != "" && f != s.Name {
			return f
		}
	}
	return cur
}

// BrewLabelFor 推导某个 Homebrew formula 在 launchd 里的**真实**标签。
//
// 为什么不能硬编码 `homebrew.mxcl.<formula>`：实测这台机器上的 Homebrew 生成的是
// `sh.brew.mysql@8.4` / `sh.brew.php@8.3`，而同一个系统里 httpd 又是
// `homebrew.mxcl.httpd` —— 两套前缀并存。硬编码的后果是应用市场里
// PHP/nginx 显示"已安装·未纳管"，点「纳管」报
// "找不到 homebrew.mxcl.php@8.3 的 plist，且该服务未在 launchd 中加载"。
//
// 所以这里**按磁盘上真实存在的 plist 文件**来定，而不是猜：
//  1. 先试两个已知前缀的精确名（快，覆盖绝大多数情况）
//  2. 退一步扫目录，找任意以 `.<formula>.plist` 结尾的
//
// 都找不到时返回空串，让调用方回退到目录条目里写死的标签。
func BrewLabelFor(userHome, formula string) string {
	if strings.TrimSpace(formula) == "" {
		return ""
	}
	dirs := []string{"/Library/LaunchDaemons"}
	if userHome != "" {
		dirs = append([]string{filepath.Join(userHome, "Library", "LaunchAgents")}, dirs...)
	}

	for _, d := range dirs {
		for _, prefix := range []string{"homebrew.mxcl.", "sh.brew."} {
			if fileExists(filepath.Join(d, prefix+formula+".plist")) {
				return prefix + formula
			}
		}
	}
	// 兜底：任意前缀，只要以 .<formula>.plist 结尾
	for _, d := range dirs {
		entries, err := filepath.Glob(filepath.Join(d, "*."+formula+".plist"))
		if err != nil || len(entries) == 0 {
			continue
		}
		base := filepath.Base(entries[0])
		return strings.TrimSuffix(base, ".plist")
	}
	return ""
}

package services

import (
	"os"
	"path/filepath"
)

// ============================================================================
//  "面板自研安装器"部署的服务的安装产物探测
//
//  为什么需要它：判断一个应用是否"已安装"，原来只看两处 ——
//    1. 面板服务记录里有没有它的 label
//    2. /Library/LaunchDaemons 下有没有它的 plist
//  这两处都只反映"服务注册了没有"，不反映"东西还在不在磁盘上"。
//  于是会出现一种**孤儿态**：安装目录还在（~/iopaint/.venv、~/tts/qwen3/.venv），
//  但 plist 没了/从没注册成功。市场会说"未安装"（用户就重装一遍已有的东西），
//  服务管理里也找不到它，而"纳管"按钮又因为 installed=false 而不显示 ——
//  用户被卡在中间，什么都点不了。
//
//  同一个漏项还有第二个后果（2026-09-22 用户报障 whisper.cpp 时查出）：卸载时
//  默认**保留**的数据（~/stt/models 那 574 MB 模型权重）看不见、也清不掉 ——
//  因为界面上「删除残留数据」按钮只在 artifacts=true 时出现。
//  所以"面板安装器 → 它的磁盘产物"必须逐条表态，漏掉一个就是一个孤儿态。
//  门禁 TestInstallerArtifactProbeCoversEveryPanelInstaller 会拦住漏掉的条目。
// ============================================================================

// installerArtifactPaths 是每个面板安装器的**家目录相对产物路径**（任一存在即算"磁盘上有东西"）。
// 键就是目录条目的 PanelInstaller 值。
var installerArtifactPaths = map[string][]string{
	"iopaint":       {filepath.Join("iopaint", ".venv", "bin", "iopaint")},
	"qwen3tts":      {filepath.Join("tts", "qwen3", ".venv", "bin", "python")},
	"voicereceiver": {filepath.Join("tts", "voice-receiver", "receiver.py")},
	// stt：模型权重（几百 MB ~ 1.5 GB）默认在卸载时**保留**，是这里唯一要看的产物。
	"stt": {filepath.Join("stt", "models")},
}

// installerArtifactExempt 说明"为什么这个面板安装器不需要产物探测"。
// 只许写真正的理由：产物是 brew 管的（brew 自己知道装没装）、或应用本来就没有磁盘产物。
var installerArtifactExempt = map[string]string{
	"phpmyadmin":     "产物是 brew keg（share/phpmyadmin），由 brew 判据覆盖",
	"ffmpeg":         "基础依赖，产物就是 brew 的 ffmpeg，没有自家数据目录",
	"python":         "基础依赖，产物就是 brew 的 python@x.y",
	"imgcompress":    "引擎是 brew 的 vips，界面由面板二进制托管，无自家数据目录",
	"macspeech":      "引擎是系统自带 /usr/bin/say，无下载产物",
	"miniflux":       "brew formula + 配置在 brew etc，无家目录产物",
	"syncthing":      "brew formula，数据目录由上游自己管",
	"transmission":   "brew formula + 配置在 brew etc，无家目录产物",
	"docker-runtime": "容器运行时由它自己的探测报产物（见 market 的 artifacts 分流）",
}

// installerArtifactProbed 报告某个 PanelInstaller 是否已被产物探测覆盖
// （自家产物表，或"官方 release 原生二进制"注册表）。
func installerArtifactProbed(appID string) bool {
	if _, ok := installerArtifactPaths[appID]; ok {
		return true
	}
	_, ok := releaseBinaryApps[appID]
	return ok
}

// InstallerArtifactExists 报告某个面板自研安装器的应用是否已在磁盘上装好。
//
// appID 用的是目录条目的 PanelInstaller 字段（iopaint / qwen3tts / voicereceiver / stt）。
// 未知的 appID 一律返回 false：宁可说"没装"，也不要凭猜给用户一个
// 点了必然失败的按钮。
//
// 刻意做成**包级函数**而不是 Manager 方法：调用方是应用市场处理器，
// 而每次构造 Manager 都会顺带做一次 Docker socket 探测（没装 Docker 时要等
// 800ms 超时）—— 那会让市场页白白变慢。这里只需要一个家目录。
func InstallerArtifactExists(userHome, appID string) bool {
	home := userHome
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return false
	}

	for _, rel := range installerArtifactPaths[appID] {
		if fileExists(filepath.Join(home, rel)) {
			return true
		}
	}
	// "官方 release 原生二进制"类（frpc / Orbien 客户端 / ddns-go …）：
	// 产物路径就是安装器注册表里的 <家目录>/<RootDir>/<Binary>，直接查表 ——
	// 在这里再抄一份路径，加新条目时漏掉的话市场会一直显示"未安装"。
	if spec, ok := releaseBinaryApps[appID]; ok {
		return fileExists(filepath.Join(home, spec.RootDir, spec.Binary))
	}
	return false
}

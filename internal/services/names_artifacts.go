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
//  用户被卡在中间，什么都点不了。这就是"IOPaint 既不在服务管理里、
//  也不显示纳管按钮"的成因。
//
//  这里按**安装器自己的路径约定**去探测产物，让市场能说清是
//  「没装」还是「装了但服务没起来」。
// ============================================================================

// InstallerArtifactExists 报告某个面板自研安装器的应用是否已在磁盘上装好。
//
// appID 用的是目录条目的 PanelInstaller 字段（iopaint / qwen3tts / voicereceiver）。
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

	switch appID {
	case "iopaint":
		// 安装器把 venv 建在 ~/iopaint/.venv，可执行文件是 venv/bin/iopaint
		return fileExists(filepath.Join(home, "iopaint", ".venv", "bin", "iopaint"))
	case "qwen3tts":
		// ~/tts/qwen3/.venv 里有 python 就说明环境建好了
		return fileExists(filepath.Join(home, "tts", "qwen3", ".venv", "bin", "python"))
	case "voicereceiver":
		return fileExists(filepath.Join(home, "tts", "voice-receiver", "receiver.py"))
	}
	// "官方 release 原生二进制"类（frpc / Orbien 客户端）：
	// 产物路径就是安装器注册表里的 <家目录>/<RootDir>/<Binary>，直接查表 ——
	// 在这里再抄一份路径，加新条目时漏掉的话市场会一直显示"未安装"。
	if spec, ok := releaseBinaryApps[appID]; ok {
		return fileExists(filepath.Join(home, spec.RootDir, spec.Binary))
	}
	return false
}

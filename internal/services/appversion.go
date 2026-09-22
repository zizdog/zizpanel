package services

import "strings"

// AppUpdate 是一条"检查更新"的结论。字段名就是接口契约（前端 updateChecks 直接吃它）。
//
// Unknown=true 表示**这次没查成**（读不到已装版本 / 读不到最新版 / 版本号无法比较）——
// 界面必须如实说"未知"，**绝不许**把 Unknown 或空 Latest 当成"已是最新"。
type AppUpdate struct {
	App             string `json:"app"`
	Installed       string `json:"installed"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Unknown         bool   `json:"unknown"`
	CheckedAt       string `json:"checked_at"`
	Error           string `json:"error,omitempty"`
	// Source 说清"最新版"是从哪来的：brew（brew outdated）/ mirror-index（镜像站索引）。
	// 用户报障时要能一眼看出面板是拿什么比的，而不是只知道"它说有新版"。
	Source string `json:"source,omitempty"`
}

// AppVersion 返回**目录声明的版本**（面板认为该装哪个版本），没有就返回空。
//
// 为什么不给每个条目手写一个版本号：手写的数字一定会和事实源漂 —— 上游换了 tag
// 没人记得回来改，于是界面显示的版本和实际装的东西对不上（这正是"谎报"的一种）。
// 每个轨都已经有唯一事实源，这里只是把它取出来：
//
//   - 独立产物（tarball / release binary）：注册表里写死的 Tag（`releaseBinaryApps`）；
//   - 动态条目（zizvideo 这类 Tag 留空的）：**空** —— 版本真源在镜像索引，
//     只有运行时读索引才知道，这里不猜（调用方按 SupportsUpdateCheck 另行处理）；
//   - compose：第一个镜像的 tag；`:latest` 不算版本声明（返回空）；
//   - brew：**空** —— 版本完全由 brew 决定，面板不 pin；已装版本要从 brew 读；
//   - 自研安装器：空（今天没有版本真源就是没有，如实留空）。
//
// **空是结论**（"面板这边没有版本真源"），不是"版本是空字符串"；界面不许拿它当
// "已是最新"，也不许补一个默认值。
func AppVersion(a App) string {
	if spec, ok := releaseBinaryApps[a.ID]; ok {
		if tag := strings.TrimSpace(spec.Tag); tag != "" {
			return tag
		}
		return "" // Dynamic 条目：真源在镜像索引，不猜
	}
	if a.Kind == KindCompose || a.Kind == KindDocker {
		return composeImageVersion(a.ComposeYAML)
	}
	return ""
}

// composeImageVersion 从 compose 内容里取第一个镜像的 tag（`:latest` / 无 tag = 空）。
//
// 为什么不返回 "latest"：它不是版本，把它当版本会让"已装 latest、仓库也是 latest"
// 判成"已是最新"，而实际上镜像可能早就更新了（那正是 compose 应用更新检测要另做
// 真实探测的原因）。如实返回空，交给调用方说"未知"。
func composeImageVersion(yaml string) string {
	imgs := composeImagesOf(yaml)
	if len(imgs) == 0 {
		return ""
	}
	ref := imgs[0]
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		tag := strings.TrimSpace(ref[i+1:])
		if tag != "" && tag != "latest" {
			return tag
		}
	}
	return ""
}

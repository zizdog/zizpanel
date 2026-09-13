// Package version 保存面板版本号。
//
// 发布流程：每次发布递增 Version，并同步更新
//  1. build/build.sh 里的打包文件名
//  2. web/ 前端的版本显示（由 /api/v1/session 下发，无需手改）
//  3. README.md 更新日志
package version

// Version 是当前面板版本。语义化版本：主版本.次版本.修订号
var Version = "0.3.1"

// Commit 与 BuildTime 由构建脚本通过 -ldflags 注入，源码运行时为空。
var (
	Commit    = "dev"
	BuildTime = "unknown"
)

// Full 返回用于界面展示的完整版本串。
func Full() string {
	if Commit == "" || Commit == "dev" {
		return Version
	}
	if len(Commit) > 7 {
		return Version + "+" + Commit[:7]
	}
	return Version + "+" + Commit
}

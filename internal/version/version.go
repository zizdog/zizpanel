// Package version 保存面板版本号。
//
// 发布流程：每次发布递增 Version，并同步更新
//  1. build/build.sh 里的打包文件名
//  2. web/ 前端的版本显示（由 /api/v1/session 下发，无需手改）
//  3. README.md 更新日志
package version

// Version 是当前面板版本。语义化版本：主版本.次版本.修订号
var Version = "1.1.0"

// 递增规则：修订号 +1 到 10 后进位并归零（见 tools/bump-version.py）。
//
// ⚠️ 历史版本记录**刻意写在 `var Version` 下面**：有些脚本（含旧版 Makefile）
// 用"文件里第一个 x.y.z"取版本号，注释写在上面会让它们取到历史版本，
// 于是包名/清单是旧版本、二进制却是新的，而发布流程不会报错
// （2026-09-17 实测踩到，见 DEVELOPMENT 坑 153）。`make check` 里有门禁锁这条。

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

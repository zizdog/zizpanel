// zizpanel-assets 打印"需要同步/打包到镜像站的文件"清单（JSON）。
//
// 给三个工具用：
//
//	tools/sync-nas-apps.sh        —— 默认子命令 `apps`（不需要参数）
//	tools/build-offline-bundle.sh —— 子命令 `plan`
//	tools/sync-nas-compose.sh     —— 子命令 `compose`（推荐 Docker 项目的 compose 参考文件）
//
// 为什么清单必须从代码读：应用、版本、文件名全部来自 internal/services 的
// 注册表与目录。手抄的那份在加应用/升版本时一定会漏，而漏掉的后果是
// "镜像/离线包看起来完整、实际缺件"（tools/sync-nas-apps.sh 的 ssh 吞 stdin
// 事件就是这么静默跳过应用却报成功的）。
//
// 用法：
//
//	go run ./cmd/zizpanel-assets            # {"apps":[...]}（保持原样，兼容旧脚本）
//	go run ./cmd/zizpanel-assets apps       # 同上，显式写出来
//	go run ./cmd/zizpanel-assets plan       # {"schema":..., "apps":[离线打包计划]}
//	go run ./cmd/zizpanel-assets compose    # {"items":[推荐 Docker 项目的 compose 参考文件]}
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/zizdog/zizpanel/internal/services"
)

func main() {
	cmd := "apps"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	var payload map[string]any
	switch cmd {
	case "apps":
		// 默认输出保持不变：sync-nas-apps.sh 按 {"apps":[...]} 解析。
		payload = map[string]any{"apps": services.ReleaseBinaryAssets()}
	case "plan":
		payload = map[string]any{
			"schema": services.OfflineSchemaVersion,
			"apps":   services.OfflinePlan(),
		}
	case "audit":
		// 应用市场审计（第 3 层）：一条命令审全部 27 个应用，有缺口就非零退出。
		// 参数解析与输出都在 audit.go 里，这里只做分流，不动既有子命令的行为。
		os.Exit(runAudit(os.Args[2:]))
	case "compose":
		// 推荐 Docker 项目的预配置 compose 参考文件（内容 + .env.example + README），
		// 给 tools/sync-nas-compose.sh 发布到镜像站 /compose/ 用。
		//
		// 内容全部来自 services 包（ComposeReferences 读 Catalog 的 ComposeYAML），
		// **不在这里手抄任何 compose** —— 手抄的那份一定会和面板展示的漂移。
		// MIRROR_BASE_URL 只影响索引 README 里写出来的基址。
		var items []map[string]string
		for _, r := range services.ComposeReferences() {
			items = append(items, map[string]string{
				"id":          r.ID,
				"compose":     r.ComposeYAML,
				"env_example": r.EnvExample,
				"readme":      services.ComposeReferenceReadme(r),
			})
		}
		payload = map[string]any{
			"base":  os.Getenv("MIRROR_BASE_URL"),
			"index": services.ComposeReferenceIndexMarkdown(os.Getenv("MIRROR_BASE_URL")),
			"items": items,
		}
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q；可用：apps（默认）、plan、audit、compose\n", cmd)
		os.Exit(2)
	}

	if err := enc.Encode(payload); err != nil {
		os.Exit(1)
	}
}

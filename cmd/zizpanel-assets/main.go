// zizpanel-assets 打印"需要同步到镜像站的应用包"清单（JSON）。
//
// 给 tools/sync-nas-apps.sh 用：应用、版本、文件名全部从 internal/services 的
// 注册表读出来，不在脚本里手抄 —— 手抄的那份在加应用/升版本时一定会漏，
// 而漏掉的后果是"镜像上没有这个包"，按设计那会**直接让安装失败**。
package main

import (
	"encoding/json"
	"os"

	"github.com/zizdog/zizpanel/internal/services"
)

func main() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"apps": services.ReleaseBinaryAssets(),
	}); err != nil {
		os.Exit(1)
	}
}

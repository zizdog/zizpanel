// `zizpanel plugin …` —— 应用插件声明的校验与**干跑**。
//
// 用法：
//
//	zizpanel plugin validate <file.json> [更多文件…]   # 只校验，不碰机器
//	zizpanel plugin plan     <file.json>               # 打印"装这台机器会做什么"
//
// 为什么先做这两个而不是直接做安装器：插件规范能不能站住，取决于**能不能覆盖真实应用**；
// validate 让作者立刻知道哪条不合法，plan 让审查者（包括我）在真机动手之前看到全流程。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/zizpanel/internal/plugins"
)

func cmdPlugin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：zizpanel plugin validate <file.json>… | plan <file.json>")
	}
	switch args[0] {
	case "validate":
		fs := flag.NewFlagSet("plugin validate", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		files := fs.Args()
		if len(files) == 0 {
			return fmt.Errorf("用法：zizpanel plugin validate <file.json> [更多文件…]")
		}
		bad := 0
		for _, f := range files {
			s, err := plugins.Load(f)
			if err != nil {
				bad++
				fmt.Printf("❌ %s\n   %v\n", filepath.Base(f), err)
				continue
			}
			fmt.Printf("✅ %s：%s（%s）· 来源 %s · 运行 %s\n", filepath.Base(f), s.Name, s.ID, s.Source.Kind, s.Run.Mode)
		}
		if bad > 0 {
			return fmt.Errorf("%d/%d 份声明不合法", bad, len(files))
		}
		return nil
	case "init":
		fs := flag.NewFlagSet("plugin init", flag.ContinueOnError)
		out := fs.String("out", "", "写到哪个文件（默认打到标准输出）")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		tpl := initTemplate()
		if strings.TrimSpace(*out) == "" {
			fmt.Print(tpl)
			return nil
		}
		if err := os.WriteFile(*out, []byte(tpl), 0o644); err != nil {
			return err
		}
		fmt.Printf("已写出模板：%s\n下一步：改完跑 `zizpanel plugin validate %s`\n", *out, *out)
		return nil
	case "plan":
		fs := flag.NewFlagSet("plugin plan", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("用法：zizpanel plugin plan <file.json>")
		}
		s, err := plugins.Load(fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Print(plugins.PlanText(s))
		return nil
	default:
		return fmt.Errorf("未知子命令 %q（只有 validate / plan）", args[0])
	}
}

// initTemplate 是一份可直接改的模板：brew 来源 + 服务化 + 端口健康探针（当前能装的那一类）。
func initTemplate() string {
	return `{
  "schema": "zizpanel.app/v1",
  "id": "myapp",
  "name": "我的应用",
  "icon": "🧩",
  "summary": "一句话说明它是什么",
  "notes": ["安装后要做的第一件事（一句话）"],

  "source": { "kind": "brew", "formula": "myapp", "checksum": "sha256" },

  "run": { "mode": "brew-service" },

  "config": { "path": "~/myapp/myapp.conf", "mode": "0600", "own": "user" },

  "expose": { "port": 12345, "bind": "0.0.0.0", "ui": "app" },

  "verify": { "any_of": [ { "kind": "http", "path": "/healthz", "expect": "ok", "timeout": "60s" } ] },

  "health": { "kind": "http", "path": "/healthz", "expect": "ok", "interval": "30s" },

  "uninstall": { "always": ["/Library/LaunchDaemons/homebrew.mxcl.myapp.plist"],
                 "optional_data": ["~/myapp"],
                 "formula": "myapp",
                 "keep_note": "默认保留 ~/myapp（配置与数据）；勾选「删除数据」才会删。" },

  "update": { "kind": "brew" },
  "requires": { "system_daemon": true, "ports": [12345] }
}
`
}

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
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/plugins"
	"github.com/zizdog/zizpanel/internal/upgrade"
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
	case "remote":
		return cmdPluginRemote(args[1:])
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

// cmdPluginRemote 管远端签名插件：list / install / remove。
func cmdPluginRemote(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：zizpanel plugin remote list|install|remove …")
	}
	fs := flag.NewFlagSet("plugin remote", flag.ContinueOnError)
	source := fs.String("source", "", "插件目录地址（例如 https://mirror.zizdog.com:8888/plugins/index.json）；远端插件默认关闭，必须显式给")
	dir := fs.String("dir", "", "本地插件目录（默认 <安装根>/plugins）")
	yes := fs.Bool("yes", false, "install 时跳过确认（先看清它要什么权限）")
	sub := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	pluginDir := strings.TrimSpace(*dir)
	if pluginDir == "" {
		pluginDir = filepath.Join(config.DefaultRoot, "plugins")
	}
	// 接上信任根：与面板升级用的是同一把内嵌发布公钥（唯一信任根，别造第二把）。
	plugins.VerifySignature = upgrade.VerifyManifest
	ctx := context.Background()
	idx, err := plugins.FetchRemoteIndex(ctx, *source)
	if err != nil {
		return err
	}
	rec := plugins.RemoteRecords(pluginDir)

	switch sub {
	case "list":
		fmt.Printf("插件目录：%s（%d 条，已验签）\n", strings.TrimSpace(*source), len(idx.Plugins))
		for _, e := range idx.Plugins {
			mark := "  "
			if _, ok := rec[e.ID]; ok {
				mark = "✅"
			}
			fmt.Printf("%s %s %s（%s）%s\n", mark, e.Icon, orDashStr(e.Name), e.ID,
				versionNote(e.Version))
		}
		fmt.Println("\n✅ = 已装到本地插件目录（还要在面板里「启用」才会出现在应用市场）")
		return nil
	case "install":
		if fs.NArg() != 1 {
			return fmt.Errorf("用法：zizpanel plugin remote install <id> --source <index.json> [--yes]")
		}
		id := fs.Arg(0)
		entry, ok := idx.Find(id)
		if !ok {
			return fmt.Errorf("目录里没有 id=%s 的插件", id)
		}
		spec, err := entry.FetchSpec(ctx, *source)
		if err != nil {
			return err
		}
		fmt.Printf("即将安装远端插件：%s %s（%s）\n来源：%s\n运行方式：%s · 来源类型：%s\n",
			spec.Icon, spec.Name, spec.ID, resolveDisplay(*source, entry.URL), spec.Run.Mode, spec.Source.Kind)
		if spec.Requires != nil {
			var need []string
			if spec.Requires.FullDisk {
				need = append(need, "完全磁盘访问权限")
			}
			if spec.Requires.RemovableVolume {
				need = append(need, "可移除宗卷")
			}
			if spec.Requires.LocalNetwork {
				need = append(need, "本地网络")
			}
			if spec.Requires.SystemDaemon {
				need = append(need, "系统级守护进程")
			}
			if len(spec.Requires.Ports) > 0 {
				need = append(need, fmt.Sprintf("端口 %v", spec.Requires.Ports))
			}
			if len(need) > 0 {
				fmt.Printf("⚠️ 它声称需要：%s\n", strings.Join(need, "、"))
			}
		}
		fmt.Print(plugins.PlanText(spec))
		if !*yes {
			return fmt.Errorf("没有 --yes：请先看上面的计划与权限清单，确认后重跑并加 --yes")
		}
		installed, err := plugins.InstallRemote(ctx, pluginDir, entry, plugins.InstallOptions{
			Reserved: func(x string) bool { _, ok := plugins.Builtin(x); return ok },
			IndexURL: *source, Version: entry.Version,
		})
		if err != nil {
			return err
		}
		fmt.Printf("\n已装入本地插件目录：%s/%s.json（**默认未启用**）\n"+
			"下一步：面板 → 应用 → 应用市场 → 本地插件 → 启用\n", pluginDir, installed.ID)
		return nil
	case "remove":
		if fs.NArg() != 1 {
			return fmt.Errorf("用法：zizpanel plugin remote remove <id> --dir <本地插件目录>")
		}
		if err := plugins.RemoveRemote(pluginDir, fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("已移除远端插件 %s（文件与来源记录都删了；已安装的应用不受影响）\n", fs.Arg(0))
		return nil
	default:
		return fmt.Errorf("未知子命令 %q（只有 list / install / remove）", sub)
	}
}

func versionNote(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return " 版本 " + v
}

func resolveDisplay(indexURL, ref string) string {
	if strings.HasPrefix(ref, "http") {
		return ref
	}
	if i := strings.LastIndex(indexURL, "/"); i > 0 {
		return indexURL[:i] + "/" + strings.TrimLeft(ref, "/")
	}
	return ref
}

func orDashStr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(未署名)"
	}
	return s
}

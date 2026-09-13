// Command zizpanel-helper 是面板的受限提权助手。
//
// 设计原则（安全边界）：
//
//  1. 只接受「子命令 + 结构化参数」，绝不接受 shell 字符串。
//     这就是为什么旧面板里 `run('sudo '.escapeshellarg($ctl).' ...')` 那种写法
//     必须废弃 —— 参数只要有一处转义遗漏就是 root 命令注入。
//
//  2. 每个子命令只做一件事，参数在内部重新校验：
//     路径必须落在白名单目录内、域名必须符合正则、端口必须是数字。
//
//  3. 不提供通用的「执行任意命令」入口。面板若需要新能力，
//     必须在这里显式加一个子命令 —— 增加能力要走代码评审，而不是配置。
//
// 授权方式：/etc/sudoers.d/zizpanel 允许面板进程免密调用本程序。
// 帮助程序本身以 root 运行，但能做的事实质上被限制在下面这些操作里。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/sites"
)

// result 是统一的 JSON 输出结构，方便面板解析。
type result struct {
	OK    bool   `json:"ok"`
	Msg   string `json:"msg"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func main() {
	// 要求必须以 root 执行：否则任何用户都能调用它做特权操作
	// 与主程序保持同一份 upgrade map 内容（单一数据源在 sites 包）
	priv.SetUpgradeMapContent(sites.UpgradeMapConf())

	if os.Geteuid() != 0 {
		emit(result{OK: false, Error: "zizpanel-helper 必须以 root 身份运行"})
		os.Exit(1)
	}

	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {

	// ---------------- nginx ----------------
	case "nginx-status":
		var st string
		st, err = priv.NginxStatus()
		if err == nil {
			emit(result{OK: true, Msg: st})
			return
		}
	case "nginx-test":
		var out string
		out, err = priv.NginxTest()
		if err == nil {
			emit(result{OK: true, Msg: out})
			return
		}
		// 配置测试失败要返回错误内容，而不是只报失败
		emit(result{OK: false, Msg: out, Error: err.Error()})
		os.Exit(1)
	case "nginx-reload":
		err = priv.NginxReload()
	case "nginx-start":
		err = priv.NginxStart()
	case "nginx-stop":
		err = priv.NginxStop()
	case "nginx-restart":
		err = priv.NginxRestart()
	case "nginx-hard-restart":
		err = priv.NginxHardRestart()

	// ---------------- vhost 文件 ----------------
	case "vhost-read":
		var name string
		if name, err = requireArg(rest, "域名/文件名"); err == nil {
			var content string
			content, err = priv.ReadVhost(name)
			if err == nil {
				emit(result{OK: true, Data: map[string]string{"content": content}})
				return
			}
		}
	case "vhost-write":
		// 从 stdin 读取内容：避免超长配置作为命令行参数（会被 ps 看到，也可能超限）
		var name string
		if name, err = requireArg(rest, "域名/文件名"); err == nil {
			var content []byte
			content, err = readStdin()
			if err == nil {
				err = priv.WriteVhostAtomic(name, string(content))
			}
		}
	case "vhost-delete":
		var name string
		if name, err = requireArg(rest, "域名/文件名"); err == nil {
			err = priv.DeleteVhost(name)
		}
	case "vhost-list":
		var list []string
		list, err = priv.ListVhosts()
		if err == nil {
			emit(result{OK: true, Data: list})
			return
		}

	// ---------------- hosts ----------------
	case "hosts-add":
		var d string
		if d, err = requireArg(rest, "域名"); err == nil {
			err = priv.HostsAdd(d)
		}
	case "hosts-del":
		var d string
		if d, err = requireArg(rest, "域名"); err == nil {
			err = priv.HostsDel(d)
		}
	case "hosts-on":
		err = priv.HostsToggle(true)
	case "hosts-off":
		err = priv.HostsToggle(false)

	// ---------------- launchd 服务 ----------------
	case "launch-load":
		var label string
		if label, err = requireArg(rest, "label"); err == nil {
			err = priv.LaunchLoad(label)
		}
	case "launch-unload":
		var label string
		if label, err = requireArg(rest, "label"); err == nil {
			err = priv.LaunchUnload(label)
		}
	case "launch-kickstart":
		var label string
		if label, err = requireArg(rest, "label"); err == nil {
			err = priv.LaunchKickstart(label)
		}
	case "launch-status":
		var label string
		if label, err = requireArg(rest, "label"); err == nil {
			var st priv.LaunchState
			st, err = priv.LaunchStatus(label)
			if err == nil {
				emit(result{OK: true, Data: st})
				return
			}
		}

	// ---------------- 端口 ----------------
	case "port-check":
		var p string
		if p, err = requireArg(rest, "端口号"); err == nil {
			var info priv.PortInfo
			info, err = priv.CheckPort(p)
			if err == nil {
				emit(result{OK: true, Data: info})
				return
			}
		}

	// ---------------- 防火墙 ----------------
	case "firewall-ensure-app":
		fs := flag.NewFlagSet("firewall-ensure-app", flag.ContinueOnError)
		app := fs.String("app", "", "可执行文件绝对路径")
		name := fs.String("name", "ZizPanel", "App 显示名")
		appsDir := fs.String("apps-dir", "", "App 安装目录（默认 /Applications）")
		if err = fs.Parse(rest); err != nil {
			break
		}
		if *app == "" {
			err = fmt.Errorf("缺少 --app 参数")
			break
		}
		var out string
		out, err = priv.EnsureFirewallApp(*app, *name, *appsDir)
		if err == nil {
			emit(result{OK: true, Msg: out})
			return
		}
	case "firewall-state":
		var st string
		st, err = priv.FirewallState()
		if err == nil {
			emit(result{OK: true, Msg: st})
			return
		}
	case "firewall-open-port":
		// 用 socketfilterfw 放行端口（部分 macOS 版本不支持，失败不算致命）
		var p string
		if p, err = requireArg(rest, "端口号"); err == nil {
			var out string
			out, err = priv.FirewallOpenPort(p)
			if err == nil {
				emit(result{OK: true, Msg: out})
				return
			}
		}

	// ---------------- 证书 ----------------
	case "mkcert":
		var out string
		out, err = priv.MkcertTrust()
		if err == nil {
			emit(result{OK: true, Msg: out})
			return
		}
	case "mkcert-issue":
		fs := flag.NewFlagSet("mkcert-issue", flag.ContinueOnError)
		hosts := fs.String("hosts", "", "逗号分隔的域名/IP")
		cert := fs.String("cert", "", "证书输出路径")
		key := fs.String("key", "", "私钥输出路径")
		if err = fs.Parse(rest); err != nil {
			break
		}
		if *cert == "" || *key == "" || *hosts == "" {
			err = fmt.Errorf("需要 --hosts、--cert、--key 三个参数")
			break
		}
		var out string
		out, err = priv.MkcertIssue(splitCSV(*hosts), *cert, *key)
		if err == nil {
			emit(result{OK: true, Msg: out})
			return
		}

	// ---------------- nginx 环境自愈 ----------------
	case "nginx-ensure-env":
		// 写入 WebSocket 升级 map，并确保 nginx.conf 的 http 块 include 了 conf.d。
		// 这是反向代理能工作的前提条件（详见 priv.EnsureUpgradeMap 的注释）。
		var out string
		out, err = priv.EnsureNginxEnv()
		if err == nil {
			emit(result{OK: true, Msg: out})
			return
		}

	case "nginx-conf-include-status":
		var ok bool
		var detail string
		ok, detail, err = priv.ConfDIncluded()
		if err == nil {
			emit(result{OK: true, Data: map[string]any{"included": ok, "detail": detail}})
			return
		}

	case "nginx-conf-backup":
		var path string
		path, err = priv.BackupNginxConf()
		if err == nil {
			emit(result{OK: true, Msg: "已备份", Data: map[string]string{"path": path}})
			return
		}

	// ---------------- 站点证书 ----------------
	case "site-cert-self":
		fs := flag.NewFlagSet("site-cert-self", flag.ContinueOnError)
		domain := fs.String("domain", "", "站点域名")
		cert := fs.String("cert", "", "证书输出路径")
		key := fs.String("key", "", "私钥输出路径")
		days := fs.Int("days", 825, "有效期天数")
		if err = fs.Parse(rest); err != nil {
			break
		}
		if *domain == "" || *cert == "" || *key == "" {
			err = fmt.Errorf("需要 --domain、--cert、--key 三个参数")
			break
		}
		var out string
		out, err = priv.MakeSelfSignedCert(*domain, *cert, *key, *days)
		if err == nil {
			subject, issuer, notAfter, ciErr := priv.CertInfo(out)
			if ciErr != nil {
				emit(result{OK: true, Msg: "证书已生成", Data: map[string]string{"cert": out}})
				return
			}
			emit(result{OK: true, Msg: "证书已生成", Data: map[string]any{
				"cert": out, "key": *key, "subject": subject, "issuer": issuer,
				"expires": notAfter.Format("2006-01-02 15:04:05"),
			}})
			return
		}

	case "site-cert-info":
		var certPath string
		if certPath, err = requireArg(rest, "证书路径"); err == nil {
			var subject, issuer string
			var notAfter time.Time
			subject, issuer, notAfter, err = priv.CertInfo(certPath)
			if err == nil {
				emit(result{OK: true, Data: map[string]any{
					"subject": subject, "issuer": issuer,
					"expires": notAfter.Format("2006-01-02 15:04:05"),
				}})
				return
			}
		}

	// ---------------- 自检 ----------------
	case "selftest":
		emit(result{OK: true, Msg: "zizpanel-helper 工作正常",
			Data: map[string]any{"uid": os.Geteuid(), "time": time.Now().Format(time.RFC3339)}})
		return

	default:
		err = fmt.Errorf("未知子命令: %s", cmd)
	}

	if err != nil {
		emit(result{OK: false, Error: err.Error()})
		os.Exit(1)
	}
	emit(result{OK: true, Msg: "完成"})
}

func emit(r result) {
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
}

func usage() {
	fmt.Print(`zizpanel-helper —— ZizPanel 受限提权助手（仅供面板调用）

用法: zizpanel-helper <子命令> [参数...]

  selftest                       自检（确认 sudoers 授权生效）
  nginx-status|test|reload|start|stop|restart|hard-restart
  vhost-list
  vhost-read   <域名>             读取 vhost 配置
  vhost-write  <域名>            从 stdin 读取内容并原子写入 vhost
  vhost-delete <域名>
  hosts-add|hosts-del <域名>
  hosts-on|hosts-off
  launch-load|launch-unload|launch-kickstart <label>
  launch-status <label>
  port-check <端口>
  firewall-state
  firewall-ensure-app --app <路径> [--name <名称>]
  firewall-open-port <端口>
  mkcert                         用 mkcert 生成并信任本地 CA

安全说明: 本程序只做上述白名单操作，不接受任意命令，不做 shell 拼接。
`)
}

func requireArg(args []string, what string) (string, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return "", fmt.Errorf("缺少参数: %s", what)
	}
	return args[0], nil
}

// splitCSV 拆分逗号分隔参数并去掉空白项。
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func readStdin() ([]byte, error) { // 限制 4MB，防止被塞入超大文件耗尽内存
	const max = 4 << 20
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(tmp)
		if n > 0 {
			if len(buf)+n > max {
				return nil, fmt.Errorf("输入超过 %d 字节上限", max)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

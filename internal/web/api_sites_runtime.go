package web

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  网站环境的**真实运行时**探测（nginx / PHP / MySQL）
//
//  2026-09-18 用户报障："本机面板显示：网站环境：未就绪（未检测到运行中的：nginx）。
//  我网站运行地好好的！"
//
//  根因是**判据错配**：那一行过去只看面板自己的**服务记录**
//  （`api/v1/services` 里有没有一条 running 的 nginx），而 nginx 完全可以
//  "装着、在跑、但面板里没有记录"（用户自己装的、或换机后记录丢了）——
//  于是面板对着一个正在服务的 nginx 说"未就绪"，还顺势建议一键 LNMP。
//  这正是本项目最忌讳的那类错误：**没看见 ≠ 没在跑**。
//
//  规矩（本次固定的类级不变量）：**"在不在跑"只能靠运行体证据**
//  —— 进程、监听端口/socket、真实请求；面板记录只用来回答"归不归面板管"。
//  两者分开报：跑了但没记录 = "在跑（面板里暂无记录）"，绝不是"未就绪"。
// ============================================================================

// nginxStatusFn 是 nginx 运行体探测的可注入点（单测不能依赖真机跑着 nginx）。
// 生产实现就是 priv.NginxStatus（pgrep `nginx: master`）。
var nginxStatusFn = priv.NginxStatus

// runtimeProbe 是一个组件的真实运行状态。
type runtimeProbe struct {
	// Running 是有运行体证据时为 true。
	Running bool `json:"running"`
	// Evidence 是人话证据（"进程 nginx: master 在跑" / "127.0.0.1:3306 可连接"）。
	Evidence string `json:"evidence"`
	// InstallPath 是二进制路径（空 = 这台机器没装）。
	InstallPath string `json:"install_path,omitempty"`
	// Registered 表示面板「服务管理」里有没有对应记录（归不归面板管）。
	Registered bool `json:"registered"`
	// RecordState 是记录里的运行状态（unknown/stopped/running），没有记录时为空。
	RecordState string `json:"record_state,omitempty"`
	// ProbeError 非空表示这次探测本身没做成（**不许**当成"没在跑"）。
	ProbeError string `json:"probe_error,omitempty"`
}

// handleSitesRuntime 返回 nginx / PHP / MySQL 的真实运行状态。
//
// 只读、无副作用：进程探测 + 一次 TCP 连接（700ms 超时），不写任何文件、
// 不改任何服务状态。界面用它画"网站环境"那一行与按钮。
func (s *Server) handleSitesRuntime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	recs := s.runtimeServiceRecords(ctx)

	nginx := runtimeProbe{}
	nginx.InstallPath = s.Cfg.NginxBin
	if st, err := nginxStatusFn(); err != nil {
		nginx.ProbeError = err.Error()
	} else if st == "running" {
		nginx.Running = true
		nginx.Evidence = "nginx 主进程在运行（pgrep nginx: master）"
	} else {
		nginx.Evidence = "没有找到 nginx 主进程"
	}
	nginx.Registered, nginx.RecordState = recordFor(recs, "nginx")

	php := runtimeProbe{}
	versions := s.detectPHPVersions(ctx)
	var live []string
	var all []string
	for _, v := range versions {
		all = append(all, v.Version)
		if v.Running {
			live = append(live, v.Version)
		}
	}
	php.InstallPath = filepath.Join(s.Cfg.BrewPrefix, "bin", "php")
	php.Running = len(live) > 0
	if len(versions) == 0 {
		php.Evidence = "这台机器上没发现 PHP（" + s.Cfg.BrewPrefix + "/opt 下没有 php@x.y）"
	} else if php.Running {
		php.Evidence = "PHP-FPM 在跑：版本 " + strings.Join(live, "、")
	} else {
		php.Evidence = "装了 PHP（" + strings.Join(all, "、") + "）但 FPM 端点没人听"
	}
	php.Registered, php.RecordState = recordFor(recs, "php")

	mysql := s.probeMySQL(ctx, recs)

	ok(w, map[string]any{
		"nginx": nginx,
		"php":   php,
		"mysql": mysql,
	})
}

// runtimeServiceRecords 读一次面板服务记录（三种组件共用一次查询）。
func (s *Server) runtimeServiceRecords(ctx context.Context) []*services.View {
	// withHealth=false：这里只要"有没有这条记录 + 记录里的运行状态"，
	// 健康检查会发 HTTP 请求（慢且与"在不在跑"无关）。
	list, err := s.svcManager().List(ctx, false)
	if err != nil {
		return nil
	}
	return list
}

// recordFor 返回"面板服务记录里有没有这个组件"以及记录中的运行状态。
//
// 匹配范围刻意宽（name / display_name / label 前缀）：面板记录里的名字历史上有
// nginx / sh-brew-nginx / homebrew.mxcl.nginx 等多种写法，判"归不归面板管"要认全，
// 但**它只影响 Registered，绝不影响 Running**（Running 只来自运行体探测）。
func recordFor(recs []*services.View, prefix string) (bool, string) {
	for _, rec := range recs {
		if rec == nil || rec.Service == nil {
			continue
		}
		keys := []string{rec.Name, rec.DisplayName, rec.LaunchLabel}
		for _, k := range keys {
			if k == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(k), prefix) ||
				strings.Contains(strings.ToLower(k), strings.ToLower(prefix)) {
				st := "unknown"
				switch {
				case rec.State.Running:
					st = "running"
				default:
					st = "stopped"
				}
				return true, st
			}
		}
	}
	return false, ""
}

// probeMySQL 探测 MySQL/MariaDB 的真实运行状态。
//
// 判据两条**任一成立**即算在跑，证据如实写出来：
//  1. 有人监听配置里的 MySQL 地址（真实 TCP 连接，700ms 超时）；
//  2. 有 mysqld / mariadbd 进程。
//
// 为什么不像 nginx 那样只看进程：mysqld 有 brew 的 launchd 包装、也有用户手工
// 拉起的形态，而"端口能连上"才是"数据库真的在服务"的直接证据。
func (s *Server) probeMySQL(ctx context.Context, recs []*services.View) runtimeProbe {
	p := runtimeProbe{}
	// 二进制路径按**当前生效的引擎**解析（MySQL 8.4 / MariaDB），不写死：
	// keg-only 的 mysql@8.4 根本不在 <brew>/bin 下（装了也显示"没装"），
	// MariaDB 的 mysqld/mariadbd 则在 opt/mariadb/bin。读不到就如实标"未复核"，
	// **不回退**到一个可能不存在的路径。
	eng, engErr := s.databaseEngine()
	switch {
	case engErr != nil:
		p.ProbeError = "数据库引擎未复核：" + engErr.Error()
	case strings.TrimSpace(eng.BinDir) != "":
		if bin := existingServerBinary(eng.BinDir); bin != "" {
			p.InstallPath = bin
		} else {
			p.ProbeError = "数据库引擎未复核：在 " + eng.BinDir + " 里没找到 mysqld/mariadbd"
		}
	default:
		note := strings.TrimSpace(eng.Note)
		if note == "" {
			note = "读不到当前生效的数据库引擎"
		}
		p.ProbeError = "数据库引擎未复核：" + note
	}
	host := s.Cfg.MySQLHost
	if host == "" {
		host = "127.0.0.1"
	}
	port := s.Cfg.MySQLPort
	if port == 0 {
		port = 3306
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		p.Running = true
		p.Evidence = addr + " 可以连接（数据库在服务）"
	} else if out, perr := exec.CommandContext(ctx, "/usr/bin/pgrep", "-f", "mysqld|mariadbd").Output(); perr == nil && strings.TrimSpace(string(out)) != "" {
		p.Running = true
		p.Evidence = "mysqld/mariadbd 进程在跑，但 " + addr + " 连不上（可能还没起好或改了端口）"
	} else {
		p.Evidence = addr + " 连不上，也没有 mysqld 进程"
	}
	p.Registered, p.RecordState = recordFor(recs, "mysql")
	if p.Registered == false {
		if reg, st := recordFor(recs, "mariadb"); reg {
			p.Registered, p.RecordState = reg, st
		}
	}
	return p
}

// existingServerBinary 在解析出的引擎 bin 目录里找服务端二进制。
// 两个引擎都提供 mysqld（MariaDB 是兼容符号），所以顺序不影响正确性。
func existingServerBinary(binDir string) string {
	for _, name := range []string{"mysqld", "mariadbd"} {
		bin := filepath.Join(binDir, name)
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}
	return ""
}

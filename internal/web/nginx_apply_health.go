package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

// ============================================================================
//  应用失败后的分级判据 + 真动作（坑 211）
//
//  2026-09-20 事故：nginx 唯一 worker 卡死时，面板只回滚文件并猜"日志属主是
//  root"（实测那个目录里一个 root 属主文件都没有，真因是 worker 卡住导致复核
//  拿不到任何响应）。现在改成：先做有界探测 → 按结果分三类 → 一类真的去恢复。
// ============================================================================

const (
	applyFailNginxDown    = "nginx_down"    // 探测超时/没有任何 HTTP 响应
	applyFailNotEffective = "not_effective" // nginx 在应答，但新配置没生效
	applyFailUpstreamDown = "upstream_down" // nginx 已加载配置，上游没响应
)

// nginxHealthProbeTimeout 是单次探测上限：超时即视为"没有响应"。
var nginxHealthProbeTimeout = 3 * time.Second

// nginxHealthProbeFn 对回环上的目标发一次有界请求；code 空/"000" = 无响应。
var nginxHealthProbeFn = func(ctx context.Context, scheme string, port int) (string, error) {
	if port <= 0 {
		return "", errors.New("端口无效，无法探测 nginx 是否在响应")
	}
	code, _, err := curlSite(ctx, scheme, "127.0.0.1", port, "/", nginxHealthProbeTimeout)
	return code, err
}

// nginxHealthReloadFn / nginxHealthRestartFn 是恢复动作（生产走提权助手）。
var (
	nginxHealthReloadFn  = func(s *Server, ctx context.Context) error { return s.nginxReload(ctx) }
	nginxHealthRestartFn = func(s *Server, ctx context.Context) error { return s.nginxRestart(ctx) }
)

// nginxRestart 重启 nginx（helper 的 nginx-restart）。
func (s *Server) nginxRestart(ctx context.Context) error {
	_, err := s.callHelper(ctx, "nginx-restart")
	return err
}

// nginxRootOwnedFilesFn 只在**真的发现** uid 0 属主的文件时返回路径；测试注入。
var nginxRootOwnedFilesFn = scanRootOwnedNginxFiles

// applyFailureVerdict 是一次失败的分类结论（Summary 是给用户的一句话，≤40 字）。
type applyFailureVerdict struct {
	Kind      string
	Summary   string
	Recovered bool
	Action    string // reload / restart / ""
}

// classifyApplyFailure 按探测结果分类；只有"nginx 无响应"才真的恢复并复核。
// probed=false 表示调用方还没探过（例如 reload 直接失败），这里补一次有界探测。
func (s *Server) classifyApplyFailure(ctx context.Context, scheme string, port int, code string, probed bool) applyFailureVerdict {
	if !probed {
		c, _ := nginxHealthProbeFn(ctx, scheme, port)
		code = c
	}
	switch {
	case code == "" || code == "000":
		action, recovered := s.recoverUnresponsiveNginx(ctx, scheme, port)
		return applyFailureVerdict{
			Kind: applyFailNginxDown, Summary: nginxDownSummary(action, recovered),
			Recovered: recovered, Action: action,
		}
	case code == "502" || code == "504":
		return applyFailureVerdict{Kind: applyFailUpstreamDown, Summary: "目标无响应（nginx 已加载配置）"}
	default:
		return applyFailureVerdict{Kind: applyFailNotEffective, Summary: "配置已写入但未生效"}
	}
}

func nginxDownSummary(action string, recovered bool) string {
	switch {
	case recovered && action == "reload":
		return "nginx 无响应，已重载恢复"
	case recovered && action == "restart":
		return "nginx 无响应，已重启恢复"
	default:
		return "nginx 无响应，重载与重启后仍未恢复"
	}
}

// recoverUnresponsiveNginx 先 reload，仍无响应再 restart，每次都复核后才下结论。
func (s *Server) recoverUnresponsiveNginx(ctx context.Context, scheme string, port int) (string, bool) {
	action := ""
	if err := nginxHealthReloadFn(s, ctx); err == nil {
		action = "reload"
		if s.nginxHealthyAgain(ctx, scheme, port) {
			return action, true
		}
	}
	if err := nginxHealthRestartFn(s, ctx); err == nil {
		action = "restart"
		if s.nginxHealthyAgain(ctx, scheme, port) {
			return action, true
		}
	}
	if action == "" {
		action = "restart" // 两个动作都没成功，如实说尝试过重启
	}
	return action, false
}

func (s *Server) nginxHealthyAgain(ctx context.Context, scheme string, port int) bool {
	code, _ := nginxHealthProbeFn(ctx, scheme, port)
	return code != "" && code != "000"
}

// recoveryNote 是"真动作"的如实回执（没做动作就返回空串）。
func recoveryNote(v applyFailureVerdict) string {
	switch {
	case v.Action == "":
		return ""
	case v.Recovered && v.Action == "reload":
		return "已自动重载 nginx 并复核：nginx 已恢复响应。"
	case v.Recovered && v.Action == "restart":
		return "已自动重载（无效）后重启 nginx 并复核：nginx 已恢复响应。"
	default:
		return "已自动重载并重启 nginx，复核仍无响应：请到「服务管理」看 nginx 状态与 error_log。"
	}
}

// rootOwnedNginxHint 只在真的发现 root 属主文件时才给这条提示（附实际路径）。
func (s *Server) rootOwnedNginxHint() string {
	files := nginxRootOwnedFilesFn(s)
	if len(files) == 0 {
		return ""
	}
	worker, _, _, _, ok := priv.NginxWorkerOwner(s.Cfg.NginxConf)
	if !ok {
		worker = "nginx 的 worker 用户"
	}
	return "发现属主是 root 的文件：" + strings.Join(files, "、") + "（nginx 以 " + worker + " 运行，读写不了）"
}

// scanRootOwnedNginxFiles 扫 nginx 要读写的日志/vhost 目录，返回 uid 0 属主的文件（最多 5 个）。
func scanRootOwnedNginxFiles(s *Server) []string {
	var dirs []string
	add := func(p string) {
		if strings.TrimSpace(p) != "" {
			dirs = append(dirs, p)
		}
	}
	add(s.siteLogDir())
	add(s.proxyLogDir())
	add(s.Cfg.VhostDir)
	add(filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx"))

	var out []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if len(out) >= 5 {
				return out
			}
			if e.IsDir() {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			if sys, ok := info.Sys().(*syscall.Stat_t); ok && sys.Uid == 0 {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out
}

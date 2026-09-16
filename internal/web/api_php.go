package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  PHP 多版本共存接口
//
//  为什么单独开一组接口，而不是塞进站点接口：
//
//   1. "这台机器上有哪些 PHP 版本"与"某个站点怎么配"是两件事，界面也要分开用
//      （站点创建向导需要一个只读的版本清单）。
//   2. 让某个版本真正监听在它独有的端点上，是一个会**改磁盘文件 + 重启服务**的
//      写操作，必须能被单独审计、单独失败、单独重试。
//
//  与站点配置的关系：站点 vhost 里的 fastcgi_pass 由 sites.ResolveEndpoint 得出，
//  它读的就是这里写进去的那个端点。所以"先修好端点、再建站"是唯一可靠的顺序。
// ============================================================================

// handlePHPList 列出本机已安装的 PHP 版本及其端点状态。
//
// 只读接口：界面用它渲染"已安装版本"下拉框，以及"哪些版本还没配好"的提示。
// 顺带返回面板分配的 socket 路径，方便用户排查（php-fpm 起不来时第一件事就是看它）。
func (s *Server) handlePHPList(w http.ResponseWriter, r *http.Request) {
	list := s.detectPHPVersions(r.Context())
	socketDir := filepath.Join(s.Cfg.BrewPrefix, "var", "run")
	needFix := 0
	for _, p := range list {
		if !p.ListenOK {
			needFix++
		}
	}
	ok(w, map[string]any{
		"list":        list,
		"socket_dir":  socketDir,
		"need_fix":    needFix,
		"brew_prefix": s.Cfg.BrewPrefix,
		// 端点分配规则由 sites 包唯一决定（Unix socket 优先，见 EnsureListen）。
		// 这里把它摊开给界面，免得前端自己拼路径、两处规则打架。
		"note": "每个 PHP 版本使用独立的 Unix socket 端点；" +
			"未配置的版本仍然监听 Homebrew 出厂的 9000，多版本会互相抢占。",
	})
}

// phpFixReq 是修复请求体。
type phpFixReq struct {
	Version string `json:"version"`
	// Restart 默认 true：改了 listen 必须重启 fpm 才生效。
	Restart *bool `json:"restart"`
}

// handlePHPFixListen 让某个版本的 php-fpm 监听在面板分配的端点上。
//
// 步骤（每一步失败都如实返回，不做"看起来成功"的处理）：
//
//	① 改写该版本的 www.conf：listen = unix:<brew>/var/run/php-fpm-<版本>.sock
//	   （幂等：已经是目标值就什么都不写）
//	② 重启该版本的 fpm 服务（让它按新配置绑定端点）
//	③ 轮询等待套接字真的出现 —— 只报"重启命令返回 0"是不够的，
//	   php-fpm 完全可能因为配置错误立刻退出，而 launchctl 的退出码多次谎报成功
//	   （见 AGENTS.md 第三节：能谎报成功的功能比没做更糟）。
func (s *Server) handlePHPFixListen(w http.ResponseWriter, r *http.Request) {
	var req phpFixReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	version := strings.TrimSpace(req.Version)
	if version == "" {
		fail(w, http.StatusBadRequest, "缺少 version 参数（例如 8.3）")
		return
	}
	if _, err := sites.PreferredEndpoint(s.Cfg.BrewPrefix, version); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// ① 改写配置
	res, err := sites.EnsureListen(s.Cfg.BrewPrefix, version, sites.EnsureListenOptions{
		// 用配置里的 BrewPrefix 推 nginx.conf，而不是 priv 包的全局探测：
		// 测试必须能把整条链路指到 t.TempDir()（本项目铁律：测试不许碰真实环境）。
		NginxConfPath: filepath.Join(s.Cfg.BrewPrefix, "etc", "nginx", "nginx.conf"),
		PanelUser:     s.Cfg.User,
	})
	if err != nil {
		s.audit(r, "php_fix_listen", version, "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "改写 php-fpm 配置失败: "+err.Error())
		return
	}
	// 套接字目录尽量归属真实用户：php-fpm 启动时要在这个目录里 bind socket，
	// 目录不可写的话它会以 "unable to bind listening socket" 启动失败。
	// 与"站点根目录归属真实用户"同一套做法（失败不致命，只提示）。
	if dir := filepath.Dir(strings.TrimPrefix(res.Endpoint, "unix:")); strings.HasPrefix(res.Endpoint, "unix:") {
		if s.Cfg.User != "" && s.Cfg.User != "root" {
			if uid, gid, lerr := lookupIDs(s.Cfg.User); lerr == nil {
				_ = os.Chown(dir, uid, gid)
			}
		}
	}

	restart := true
	if req.Restart != nil {
		restart = *req.Restart
	}
	// ② 重启服务（改了 listen 必须重启才生效）
	if restart {
		svcName := s.phpServiceName(version)
		if svcName == "" {
			s.audit(r, "php_fix_listen", version, "无对应服务记录", false, "")
			fail(w, http.StatusBadRequest, fmt.Sprintf(
				"PHP %s 的配置已改到 %s，但面板里没有它的服务记录，无法重启它。"+
					"请先在「应用市场」安装该版本，或手工重启 php-fpm", version, res.Endpoint))
			return
		}
		mgr := s.svcManager()
		if _, err := mgr.Action(r.Context(), svcName, "restart"); err != nil {
			s.audit(r, "php_fix_listen", version, "配置已改但重启失败: "+err.Error(), false, "")
			fail(w, http.StatusInternalServerError, fmt.Sprintf(
				"已把 PHP %s 的配置改到 %s，但重启服务 %s 失败: %v。"+
					"请到「服务」页查看它的日志后重试", version, res.Endpoint, svcName, err))
			return
		}
		// ③ 等套接字真的出现 —— 只报"重启命令返回 0"是不够的
		if !waitEndpointLive(res.Endpoint, 15*time.Second) {
			s.audit(r, "php_fix_listen", version, "端点未监听", false, "")
			fail(w, http.StatusInternalServerError, fmt.Sprintf(
				"配置已写入并重启了服务，但 %s 上仍然没有进程监听：PHP %s 的 php-fpm 没起来。"+
					"常见原因是端口/套接字被别的版本占用，或 php-fpm 配置有语法错误。"+
					"请看「服务」页里 %s 的日志（也可在终端跑 php-fpm -t 校验）",
				res.Endpoint, version, svcName))
			return
		}
		msg := fmt.Sprintf("PHP %s 已监听在 %s（已重启服务 %s）", version, res.Endpoint, svcName)
		s.audit(r, "php_fix_listen", version, msg, true, "")
		ok(w, map[string]any{
			"result": res, "service": svcName, "msg": msg, "running": true,
		})
		return
	}

	// 只写配置、不重启（给"修好配置，我自己择机重启"这种场景用）
	msg := fmt.Sprintf("PHP %s 的监听端点已是 %s（未改动）", version, res.Endpoint)
	if res.Changed {
		msg = fmt.Sprintf("PHP %s 的监听端点已写入配置：%s（未重启服务，重启后生效）",
			version, res.Endpoint)
	}
	s.audit(r, "php_fix_listen", version, msg, true, "")
	ok(w, map[string]any{
		"result": res, "service": "", "msg": msg, "running": sites.EndpointLive(res.Endpoint),
	})
}

// phpServiceName 找到某个 PHP 版本对应的面板服务记录名。
//
// 为什么不直接拼 "php@8.3"：面板的服务记录名不一定等于 formula 名
// （实测 Homebrew 的 label 有 homebrew.mxcl.* 与 sh.brew.* 两套前缀），
// 所以按"版本号"去服务列表里匹配，匹配不到就如实返回空。
func (s *Server) phpServiceName(version string) string {
	mgr := s.svcManager()
	list, err := mgr.List(context.Background(), false)
	if err != nil {
		return ""
	}
	want := "php@" + version
	for _, svc := range list {
		if svc.Name == want {
			return svc.Name
		}
	}
	// 退化匹配：服务记录名里含 "php" 且版本号一致
	for _, svc := range list {
		if strings.Contains(svc.Name, "php") && strings.Contains(svc.Name, version) {
			return svc.Name
		}
	}
	return ""
}

// waitEndpointLive 轮询等待端点出现。
func waitEndpointLive(endpoint string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if sites.EndpointLive(endpoint) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

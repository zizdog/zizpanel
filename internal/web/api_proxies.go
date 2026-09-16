package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/proxies"
)

// ============================================================================
//  「反向代理」独立功能
//
//  与「网站管理」的区别（这是刻意的分工，不是重复）：
//    · 网站管理：一个站点 = 域名 + 根目录 + PHP/伪静态（可选带反代）；
//    · 反向代理：一条规则 = 监听端口 + 域名/Host + 路径前缀 + 目标地址。
//      它**不需要站点目录**，多数用法就是"把某个端口上的请求转到另一台机器"。
//
//  规则存在 proxies 表里，生成的 nginx 配置写进 vhosts/proxy-<id>.conf
//  （前缀 proxy- 保证与站点文件 <域名>.conf 不撞名），写入与 reload 都
//  复用已有的 helper（含 nginx -t 校验与失败回滚）。
// ============================================================================

// proxyLogDir 是反代规则自己的日志目录（与站点日志分开，便于按规则排查）。
func (s *Server) proxyLogDir() string {
	dir := filepath.Join(s.Cfg.LogRoot, "proxy")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func (s *Server) proxyRepo() *proxies.Repository {
	return proxies.NewRepository(s.Store)
}

// handleProxyList 列出全部规则，并带上每条规则的实时状态。
//
// 状态里最要紧的是 `port_listening`：规则"已启用"不等于 nginx 真的在听
// —— 端口被别的进程占了、或 nginx 没起来时，界面必须如实显示，
// 否则用户会以为规则生效了却怎么都访问不到。
func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	list, err := s.proxyRepo().List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, rule := range list {
		items = append(items, s.proxyView(r.Context(), rule))
	}
	ok(w, map[string]any{
		"list": items,
		"nginx": map[string]any{
			"installed": s.nginxInstalled(),
			"config":    s.Cfg.NginxConf,
		},
	})
}

// proxyView 组装一条规则给前端的样子（含实时状态）。
func (s *Server) proxyView(ctx context.Context, rule *proxies.Rule) map[string]any {
	listening := false
	if rule.Enabled {
		listening = portListening(ctx, rule.Listen)
	}
	host, port, _ := rule.TargetHostPort()
	reachable, detail := probeTarget(ctx, rule.Target)
	vhost := filepath.Join(s.Cfg.VhostDir, rule.VhostName()+".conf")
	_, statErr := os.Stat(vhost)
	return map[string]any{
		"rule":           rule,
		"id":             rule.ID,
		"name":           rule.Name,
		"listen":         rule.Listen,
		"domains":        rule.Domains,
		"path":           rule.Path,
		"target":         rule.Target,
		"preserve_host":  rule.PreserveHost,
		"websocket":      rule.Websocket,
		"enabled":        rule.Enabled,
		"remark":         rule.Remark,
		"created_at":     rule.Created,
		"updated_at":     rule.Updated,
		"port_listening": listening,
		"target_host":    host,
		"target_port":    port,
		"target_ok":      reachable,
		"target_detail":  detail,
		"config_written": statErr == nil,
		"config_path":    vhost,
	}
}

func (s *Server) nginxInstalled() bool {
	if _, err := os.Stat(s.Cfg.NginxBin); err == nil {
		return true
	}
	return false
}

// portListening 判断本机某个端口是否有人在听。
func portListening(ctx context.Context, port int) bool {
	if port <= 0 {
		return false
	}
	d := net.Dialer{Timeout: 800 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// probeTarget 探一次目标是否可达（TCP 连接），返回 (是否可达, 给人看的说明)。
//
// 只做 TCP 连接、不发 HTTP 请求：目标是 http 还是 https、要不要 Host 头，
// 由 nginx 去处理；这里只回答用户最关心的问题"这个地址到底通不通"。
func probeTarget(ctx context.Context, target string) (bool, string) {
	host, port, err := (&proxies.Rule{Target: target}).TargetHostPort()
	if err != nil {
		return false, err.Error()
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	start := time.Now()
	c, derr := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if derr != nil {
		return false, fmt.Sprintf("连不上 %s:%d（%v）", host, port, derr)
	}
	_ = c.Close()
	return true, fmt.Sprintf("可达 %s:%d（%dms）", host, port, time.Since(start).Milliseconds())
}

type proxyReq struct {
	Name         *string `json:"name"`
	Listen       *int    `json:"listen"`
	Domains      *string `json:"domains"`
	Path         *string `json:"path"`
	Target       *string `json:"target"`
	PreserveHost *bool   `json:"preserve_host"`
	Websocket    *bool   `json:"websocket"`
	Enabled      *bool   `json:"enabled"`
	Remark       *string `json:"remark"`
}

func (req proxyReq) apply(rule *proxies.Rule) {
	if req.Name != nil {
		rule.Name = *req.Name
	}
	if req.Listen != nil {
		rule.Listen = *req.Listen
	}
	if req.Domains != nil {
		rule.Domains = *req.Domains
	}
	if req.Path != nil {
		rule.Path = *req.Path
	}
	if req.Target != nil {
		rule.Target = *req.Target
	}
	if req.PreserveHost != nil {
		rule.PreserveHost = *req.PreserveHost
	}
	if req.Websocket != nil {
		rule.Websocket = *req.Websocket
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if req.Remark != nil {
		rule.Remark = *req.Remark
	}
}

// handleProxyCreate 新建规则：先校验、查冲突、再落库并应用。
func (s *Server) handleProxyCreate(w http.ResponseWriter, r *http.Request) {
	var req proxyReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	rule := &proxies.Rule{Listen: 80, Websocket: true, Enabled: true}
	req.apply(rule)
	if err := rule.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.nginxInstalled() {
		fail(w, http.StatusConflict, "反向代理需要 nginx：请先到「应用市场 → 网站环境」安装 nginx")
		return
	}
	if other, cerr := s.proxyRepo().ConflictWith(r.Context(), rule); cerr == nil && other != nil {
		fail(w, http.StatusConflict, fmt.Sprintf(
			"和已有规则「%s」（同端口 %d、同域名/路径）冲突：nginx 只会用先加载的那一条，"+
				"请改端口、域名或路径", other.Name, other.Listen))
		return
	}
	created, err := s.proxyRepo().Create(r.Context(), rule)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.applyProxy(r.Context(), created); err != nil {
		// 配置写不进去就把记录删掉，避免留下一条"看着在、其实没生效"的规则
		_ = s.proxyRepo().Delete(r.Context(), created.ID)
		fail(w, http.StatusBadGateway, "规则已保存但 nginx 配置应用失败："+err.Error())
		return
	}
	s.audit(r, "proxy_create", created.Name, fmt.Sprintf("%d → %s", created.Listen, created.Target), true, "")
	ok(w, s.proxyView(r.Context(), created))
}

// handleProxyUpdate 修改规则：先写新配置，成功后再落库。
func (s *Server) handleProxyUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	repo := s.proxyRepo()
	cur, err := repo.Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req proxyReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	next := *cur
	req.apply(&next)
	if err := next.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if other, cerr := repo.ConflictWith(r.Context(), &next); cerr == nil && other != nil {
		fail(w, http.StatusConflict, fmt.Sprintf(
			"和规则「%s」（同端口 %d、同域名/路径）冲突，nginx 只会用先加载的那一条", other.Name, other.Listen))
		return
	}
	// 先按新配置写盘（含 nginx -t 校验、失败回滚），成功后才更新数据库 ——
	// 反过来的话，配置写失败会留下"数据库说已改、文件还是旧的"的不一致。
	next.ID = cur.ID
	if err := s.applyProxy(r.Context(), &next); err != nil {
		_ = s.applyProxy(r.Context(), cur) // 回滚成旧配置
		fail(w, http.StatusBadGateway, "应用新配置失败（已回滚）："+err.Error())
		return
	}
	saved, err := repo.Update(r.Context(), &next)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "proxy_update", saved.Name, fmt.Sprintf("%d → %s", saved.Listen, saved.Target), true, "")
	ok(w, s.proxyView(r.Context(), saved))
}

// handleProxyDelete 删除规则：先删配置文件并 reload，再删记录。
func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	repo := s.proxyRepo()
	cur, err := repo.Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.removeProxyConfig(r.Context(), cur); err != nil {
		fail(w, http.StatusBadGateway, "删除 nginx 配置失败："+err.Error())
		return
	}
	if err := repo.Delete(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "proxy_delete", cur.Name, fmt.Sprintf("%d → %s", cur.Listen, cur.Target), true, "")
	ok(w, map[string]any{"msg": "已删除规则「" + cur.Name + "」并移除它的 nginx 配置"})
}

// handleProxyToggle 启用/停用一条规则。
//
// 停用 = 把配置文件删掉再 reload（而不是注释掉配置）：nginx 不加载它就一定不生效，
// 而"注释掉"要保证注释语法完全正确，多行配置里更容易出错。
func (s *Server) handleProxyToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	repo := s.proxyRepo()
	cur, err := repo.Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := *cur
	next.Enabled = !cur.Enabled
	if next.Enabled {
		if err := s.applyProxy(r.Context(), &next); err != nil {
			fail(w, http.StatusBadGateway, "启用失败："+err.Error())
			return
		}
	} else {
		if err := s.removeProxyConfig(r.Context(), &next); err != nil {
			fail(w, http.StatusBadGateway, "停用失败："+err.Error())
			return
		}
	}
	saved, err := repo.Update(r.Context(), &next)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	action := "已停用"
	if saved.Enabled {
		action = "已启用"
	}
	s.audit(r, "proxy_toggle", saved.Name, action, true, "")
	ok(w, s.proxyView(r.Context(), saved))
}

// handleProxyTest 在保存前试一次目标可达性（界面上的「测试连通」按钮）。
//
// 单独一个接口而不是保存时顺带做：用户想在**不落库**的情况下先确认地址对不对。
func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string `json:"target"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	reachable, detail := probeTarget(r.Context(), strings.TrimSpace(req.Target))
	ok(w, map[string]any{"ok": reachable, "detail": detail})
}

// applyProxy 生成并应用一条规则的 nginx 配置。
func (s *Server) applyProxy(ctx context.Context, rule *proxies.Rule) error {
	if !rule.Enabled {
		return s.removeProxyConfig(ctx, rule)
	}
	content, err := rule.Generate(s.proxyLogDir())
	if err != nil {
		return err
	}
	// 复用站点的写盘通道：它经提权助手做原子写 + nginx -t 校验 + 失败回滚
	if err := s.writeVhost(ctx, rule.VhostName(), content); err != nil {
		return err
	}
	return s.nginxReload(ctx)
}

// removeProxyConfig 移除一条规则的配置文件并 reload。
func (s *Server) removeProxyConfig(ctx context.Context, rule *proxies.Rule) error {
	if err := s.deleteVhost(ctx, rule.VhostName()); err != nil {
		return err
	}
	return s.nginxReload(ctx)
}

// deleteVhost 删除 vhost 文件（经提权助手）。
//
// 文件本来就不存在时视为成功：删除是幂等的，而"已经没有了"不该报错。
func (s *Server) deleteVhost(ctx context.Context, name string) error {
	path := filepath.Join(s.Cfg.VhostDir, name+".conf")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	_, err := s.callHelper(ctx, "vhost-delete", name)
	return err
}

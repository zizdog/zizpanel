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
	"github.com/zizdog/zizpanel/internal/sites"
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
//
// 目录由面板以 root 创建，而真正往里写日志的是**以真实用户运行的 nginx**，
// 所以建完立刻把归属交还真实用户（目录里的日志文件由 chownProxyLogTrees
// 在 reload 前整棵递归修正 —— 那才是 reload 静默失败的根源）。
func (s *Server) proxyLogDir() string {
	dir := filepath.Join(s.Cfg.LogRoot, "proxy")
	_ = os.MkdirAll(dir, 0o755)
	s.chownTreeToUser(dir)
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
	// 域名兜底块是否存在：界面要能看出"域名限制到底有没有生效"，
	// 否则用户只能靠"不带 Host 试探"才能发现限制被 nginx 的默认 server 吃掉了。
	reject := filepath.Join(s.Cfg.VhostDir, proxies.RejectVhostName(rule.Listen)+".conf")
	_, rejectErr := os.Stat(reject)
	domainGuard := len(proxies.SplitDomains(rule.Domains)) > 0
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
		"domain_guard":   domainGuard,
		"reject_written": rejectErr == nil,
		"reject_path":    reject,
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
	detail := fmt.Sprintf("%d → %s", created.Listen, created.Target)
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_create", created.Name,
			"规则已创建并生效（"+detail+"）", err)
		return
	}
	s.audit(r, "proxy_create", created.Name, detail, true, "")
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
	detail := fmt.Sprintf("%d → %s", saved.Listen, saved.Target)
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_update", saved.Name,
			"规则已保存并生效（"+detail+"）", err)
		return
	}
	s.audit(r, "proxy_update", saved.Name, detail, true, "")
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
	detail := fmt.Sprintf("%d → %s", cur.Listen, cur.Target)
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_delete", cur.Name,
			"规则已删除（"+detail+"）", err)
		return
	}
	s.audit(r, "proxy_delete", cur.Name, detail, true, "")
	ok(w, map[string]any{"msg": "已删除规则「" + cur.Name + "」并移除它的 nginx 配置"})
}

// rejectGuardFailed 把"域名兜底拒绝块没生效"如实上报为非 2xx。
//
// 为什么不再像以前那样"只写进 detail 仍然返回 200"：那正是本项目最贵的教训
// ——配置写了但没被 nginx 加载，面板却说成功。主操作确实已经成功，所以消息里
// 必须写清楚"什么已经成功、什么没生效"，让用户知道该修什么。
func (s *Server) rejectGuardFailed(w http.ResponseWriter, r *http.Request,
	action, name, done string, err error) {
	s.audit(r, action, name, done+"（域名兜底拒绝块未生效: "+err.Error()+"）", false, err.Error())
	fail(w, http.StatusBadGateway, done+"，但域名兜底拒绝块没有生效："+err.Error()+
		"。副作用：域名对不上的请求可能被转发到后端，请修正后重试")
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
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_toggle", saved.Name,
			"规则"+action+"并已生效", err)
		return
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

// ============================================================================
//  反代落盘通道：chown 日志树 → reload → 请求级复核
//
//  为什么必须先 chown 再 reload：
//  写 vhost 的提权助手（priv.WriteVhostAtomic）会以 **root** 跑 `nginx -t`，
//  而 `nginx -t` 在校验时就会把 access_log/error_log **创建出来**（root 属主）。
//  随后以真实用户运行的 nginx master 打不开这些文件：
//
//      [emerg] open() ".../proxy-1.access.log" failed (13: Permission denied)
//
//  于是 reload **根本没加载新配置**，但 `nginx -s reload` 的退出码仍然是 0 ——
//  这正是"面板报成功、配置却没生效"的根因（phpMyAdmin 已经踩过一次）。
//
//  为什么必须请求级复核：
//  reload 命令成功 ≠ 配置被加载。文件写进去了（proxyView 的 config_written
//  只是 os.Stat）更不等于 nginx 在用。只有真的发一个请求、确认该规则的
//  **自己的访问日志**长出了新内容，才算证明"nginx 加载了这个 server 块、
//  并且成功打开了这个日志文件"—— 而这恰好就是失败时打不开的那个文件。
// ============================================================================

// proxyProbeFn / proxyReloadFn / proxyChownLogsFn 是可注入步骤：
// 生产环境指向真实实现，单测里替换它们，避免碰真实服务与真实 nginx。
var (
	// proxyProbeFn 做一次请求级探测（生产 = curlSite，用 --resolve 钉到 127.0.0.1）。
	proxyProbeFn = curlSite
	// proxyReloadFn 重载 nginx（生产 = s.nginxReload，即提权助手的 nginx-reload）。
	//
	// 注意：s.nginxReload 内部**只有** `nginx -s reload`，既不做 `nginx -t`，
	// 也不 chown、更不复核；`-t` 是在 writeVhost（helper）里以 root 跑的。
	// 所以 chown 与复核必须由本文件的通道补上。
	proxyReloadFn = func(s *Server, ctx context.Context) error { return s.nginxReload(ctx) }
	// proxyChownLogsFn 把 nginx 日志树递归交还真实用户。
	proxyChownLogsFn = func(s *Server) { s.chownProxyLogTrees() }
	// proxyWriteVhostFn 写 vhost（生产 = s.writeVhost，helper 内含 nginx -t）。
	// 做成变量只为让单测能覆盖"写盘 → chown → reload → 复核"这条顺序，
	// 生产行为与直接调用完全一致（与 api_sites.go 的 siteWriteVhostFn 同一做法）。
	proxyWriteVhostFn = func(s *Server, ctx context.Context, name, content string) error {
		return s.writeVhost(ctx, name, content)
	}
)

// proxyVerifyWait / proxyVerifyEvery / proxyLogSettle 控制复核的等待窗口。
//
// 为什么要等：`nginx -s reload` 只是给 master 发信号，新监听端口 / 新 server
// 块生效有很短的延迟；写完立刻探测会把一次正常重载误判成失败。
// 同时日志是 worker 在响应之后写的，读大小前留一点落盘时间。
var (
	proxyVerifyWait  = 6 * time.Second
	proxyVerifyEvery = 300 * time.Millisecond
	proxyLogSettle   = 150 * time.Millisecond
)

// proxyProbeTimeout 是单次请求级探测的超时。不能太长：面板要在这个请求里等复核。
const proxyProbeTimeout = 4 * time.Second

// proxyProbe 是一次请求级复核拿到的原始结果。
type proxyProbe struct {
	code string // HTTP 状态码；"000" = 没拿到响应（连不上 / 被 444 断开 / 超时）
	body string
	err  error
}

// chownTreeToUser 把一棵树递归交给面板的"真实用户"。
//
// 抽出来是为了让反代各条路径只有一种改归属的写法（与 applyDefaultVhost /
// applySite 的判据一致：必须是 root、必须是具体用户）。
func (s *Server) chownTreeToUser(path string) {
	if path == "" || s.Cfg.User == "" || s.Cfg.User == "root" || os.Geteuid() != 0 {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	_ = chownTreeTo(path, s.Cfg.User)
}

// chownProxyLogTrees 把 nginx 需要写的日志目录**递归**交给真实用户。
//
// 必须在 writeVhost（内部以 root 跑 `nginx -t`，会创建 root 属主的日志文件）
// 之后、`-s reload` 之前调用。覆盖整棵树而不是只改目录本身：
// 触发故障的正是 `nginx -t` 新建出来的 proxy-*.access.log（文件级属主不对）。
//
// 与 services 侧 chownNginxLogTrees / applySite 的 chown 是同一件事，
// 只是这里覆盖反代用到的目录（含 <brew>/var/log/nginx）。
func (s *Server) chownProxyLogTrees() {
	for _, root := range s.proxyLogRoots() {
		s.chownTreeToUser(root)
	}
}

// proxyLogRoots 是反代需要写的日志目录（去重后按"最具体 → 最大"排列）。
func (s *Server) proxyLogRoots() []string {
	roots := make([]string, 0, 3)
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		for _, r := range roots {
			if r == p {
				return
			}
		}
		roots = append(roots, p)
	}
	add(filepath.Join(s.Cfg.LogRoot, "proxy")) // 反代规则自己的日志
	add(s.Cfg.LogRoot)                         // 站点 / 默认站点日志的那棵树
	add(filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx"))
	return roots
}

// proxyAccessLogPath 是 Rule.Generate 写进 vhost 的那条 access_log 的路径。
//
// 命名必须与 internal/proxies 的 `proxy-<id>.access.log` 一致 ——
// 复核就是靠"这个文件有没有长出新内容"来判断规则是否真的被 nginx 使用。
func proxyAccessLogPath(logDir string, id int64) string {
	return filepath.Join(logDir, fmt.Sprintf("proxy-%d.access.log", id))
}

// proxyLogSize 返回日志文件大小；不存在按 0 处理（存在与否不影响判据：
// 我们只看"有没有增长"）。
func proxyLogSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// proxyProbePath 是复核要请求的路径：必须落在规则的 location 前缀里。
func proxyProbePath(rule *proxies.Rule) string {
	if p := strings.TrimSpace(rule.Path); p != "" {
		return p
	}
	return "/"
}

// proxyProbeHost 选一个"能命中该规则"的 Host。
//
// 通配域名（server_name *.example.com）要换成一个能匹配它的具体名字，
// 否则请求会落到兜底拒绝块上，把"配置正常"误判成失败。
//
// 没有域名时 internal/proxies 生成的 server_name 是 `_`（一个**字面**名字，
// 不是通配），所以必须原样发 `Host: _` 才能命中它 —— 同一端口上如果还有别的
// 带域名规则，它的兜底拒绝块会把 `Host: 127.0.0.1` 这类请求先抢走（444）。
func proxyProbeHost(rule *proxies.Rule) string {
	for _, d := range proxies.SplitDomains(rule.Domains) {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if strings.HasPrefix(d, "*.") {
			return "zp-probe." + d[2:]
		}
		if strings.Contains(d, "*") {
			continue // 其它通配写法无法稳定构造出一个匹配的 Host
		}
		return d
	}
	return "_"
}

// probeProxyOnce 向规则的监听端口发一次请求（Host 由调用方指定）。
func (s *Server) probeProxyOnce(ctx context.Context, rule *proxies.Rule, host string) proxyProbe {
	code, body, err := proxyProbeFn(ctx, "http", host, rule.Listen, proxyProbePath(rule), proxyProbeTimeout)
	return proxyProbe{code: code, body: body, err: err}
}

// proxyProbeServed 判断一次探测的**响应**是否像"被 nginx 处理了"。
//
// 判据刻意不是"2xx 就算通"（nginx 的 404/502 页也有内容）：
//   - 必须真的拿到 HTTP 响应；"000" = 连不上 / 被 444 断开 / 超时 → 没生效；
//   - 在 80 端口上，响应体是默认站点（000-default）的占位页 → 请求落到了默认
//     站点，说明这条规则没被加载。只在 80 端口判这条：默认站点只监听 80，
//     而"反代到本机默认站点"（别的端口 → 127.0.0.1:80）是合法配置，
//     一律判会把正常规则误判成失败；
//   - nginx 自己生成的 502/504 也算"已加载"：那说明 location 命中了，
//     只是上游没起来（上游健康另有 target_ok 字段如实展示）。
func proxyProbeServed(p proxyProbe, listen int) bool {
	code := strings.TrimSpace(p.code)
	if code == "" || code == "000" {
		return false
	}
	if listen == 80 && strings.Contains(p.body, sites.LocalhostIndexMarker) {
		return false
	}
	return true
}

// describeProxyProbe 给复核失败一个可读的现场描述。
func describeProxyProbe(p proxyProbe, rule *proxies.Rule) string {
	code := strings.TrimSpace(p.code)
	if code == "" || code == "000" {
		if p.err != nil {
			return "没有拿到任何 HTTP 响应（" + p.err.Error() + "）"
		}
		return "没有拿到任何 HTTP 响应"
	}
	if rule.Listen == 80 && strings.Contains(p.body, sites.LocalhostIndexMarker) {
		return "HTTP " + code + "，但内容是默认站点（000-default）的占位页"
	}
	if code == "502" || code == "504" {
		return "HTTP " + code + "（nginx 已命中该反代规则，但目标 " + rule.Target + " 没响应）"
	}
	return "HTTP " + code
}

// proxyServeCheck 是一次"规则是否真的生效"复核的完整证据。
type proxyServeCheck struct {
	Served  bool       // nginx 是否真的在用这份配置
	LogGrew bool       // 该规则自己的访问日志是否长出了新内容
	LogPath string     // 该规则的访问日志路径
	Probe   proxyProbe // 最后一次探测的响应
}

// waitProxyServed 轮询到"规则真的生效"，返回完整证据。
//
// 最硬的证据是**该规则自己的访问日志增长**：只有 nginx 真的加载了这个
// server 块、并成功打开了这个日志文件，请求才可能被写进去 —— 而"打不开
// 日志文件"正是 reload 静默失败的原因。响应内容只作为辅助判据与现场描述。
func (s *Server) waitProxyServed(ctx context.Context, rule *proxies.Rule) proxyServeCheck {
	host := proxyProbeHost(rule)
	logPath := proxyAccessLogPath(s.proxyLogDir(), rule.ID)
	deadline := time.Now().Add(proxyVerifyWait)
	chk := proxyServeCheck{LogPath: logPath}
	for {
		before := proxyLogSize(logPath)
		chk.Probe = s.probeProxyOnce(ctx, rule, host)
		if proxyLogSettle > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(proxyLogSettle):
			}
		}
		chk.LogGrew = proxyLogSize(logPath) > before
		chk.Served = chk.LogGrew && proxyProbeServed(chk.Probe, rule.Listen)
		if chk.Served {
			return chk
		}
		if !time.Now().Before(deadline) {
			return chk
		}
		select {
		case <-ctx.Done():
			return chk
		case <-time.After(proxyVerifyEvery):
		}
	}
}

// reloadProxyAndVerify 是反代**写入/更新**后的统一收尾：
// chown 日志树 → reload → 请求级复核（该规则必须真的被 nginx 使用）。
//
// 复核不通过一律返回错误；调用方据此返回非 2xx（绝不"失败只记日志"）。
func (s *Server) reloadProxyAndVerify(ctx context.Context, rule *proxies.Rule) error {
	proxyChownLogsFn(s)
	if err := proxyReloadFn(s, ctx); err != nil {
		return fmt.Errorf("配置已写入，但 nginx 重载失败: %w", err)
	}
	chk := s.waitProxyServed(ctx, rule)
	if !chk.Served {
		why := "以 Host=" + proxyProbeHost(rule) + " 请求 127.0.0.1:" +
			strconv.Itoa(rule.Listen) + proxyProbePath(rule) + " 得到 " +
			describeProxyProbe(chk.Probe, rule)
		if !chk.LogGrew {
			why += "，且该规则自己的访问日志 " + chk.LogPath + " 没有任何新增"
		}
		return fmt.Errorf(
			"规则「%s」的配置已写入、nginx 重载命令也返回成功，但复核发现**新配置没有生效**：%s。"+
				"最常见的原因是日志文件属主是 root（写 vhost 时以 root 跑过 `nginx -t`，"+
				"它会在 %s 下创建 root 属主的日志），以真实用户运行的 nginx 打不开它们 → "+
				"reload 失败而退出码仍是 0。请到「日志中心 → nginx error_log」看 [emerg] 行",
			rule.Name, why, filepath.Dir(chk.LogPath))
	}
	return s.verifyProxyDomainGuard(ctx, rule)
}

// reloadProxyAndVerifyGone 是反代**删除/停用**后的统一收尾。
//
// 只 chown + reload + 复核就够：删除不需要写配置，但"删了却没卸载"同样是
// 静默失败（reload 退出码 0），所以复核不能省。
func (s *Server) reloadProxyAndVerifyGone(ctx context.Context, rule *proxies.Rule) error {
	proxyChownLogsFn(s)
	if err := proxyReloadFn(s, ctx); err != nil {
		return fmt.Errorf("配置已删除，但 nginx 重载失败: %w", err)
	}
	return s.waitProxyGone(ctx, rule)
}

// waitProxyGone 轮询到"这条规则真的不再被 nginx 使用"。
//
// 判据仍然是**该规则自己的访问日志不再增长**：同一个端口上可能还有别的启用
// 规则在应答，"端口有没有响应"区分不出到底是谁在服务，而每条规则的 location
// 只写自己的 access_log —— 日志不再增长才说明这条规则真的被卸载了。
func (s *Server) waitProxyGone(ctx context.Context, rule *proxies.Rule) error {
	logPath := proxyAccessLogPath(s.proxyLogDir(), rule.ID)
	prev := proxyLogSize(logPath)
	deadline := time.Now().Add(proxyVerifyWait)
	var last proxyProbe
	for {
		last = s.probeProxyOnce(ctx, rule, proxyProbeHost(rule))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(proxyVerifyEvery):
		}
		cur := proxyLogSize(logPath)
		if cur <= prev {
			return nil // 这条规则的日志不再增长 → 它已经不再被 nginx 使用
		}
		prev = cur
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"规则「%s」的 nginx 配置已删除、重载命令也返回成功，但复核发现**它仍在生效**："+
					"再请求 127.0.0.1:%d%s 后，它自己的访问日志 %s 仍在增长（最后一次响应 %s）。"+
					"这通常意味着 nginx 没有真正重载（日志属主是 root 时 reload 会失败但退出码仍是 0）",
				rule.Name, rule.Listen, proxyProbePath(rule), logPath, describeProxyProbe(last, rule))
		}
	}
}

// verifyProxyDomainGuard 复核"域名对不上的 Host 不会被误转发到后端"。
//
// 只有该端口确实应该存在兜底拒绝块（文件在）时才检查：
//   - 端口 80 上 000-default.conf 已经占了 default_server，拒绝块写不进去，
//     但这不算失败 —— 不匹配的 Host 会被默认站点接住，同样漏不到反代后端；
//   - 通配规则（domains 为空）本来就该匹配所有 Host，跳过。
func (s *Server) verifyProxyDomainGuard(ctx context.Context, rule *proxies.Rule) error {
	if len(proxies.SplitDomains(rule.Domains)) == 0 {
		return nil
	}
	reject := filepath.Join(s.Cfg.VhostDir, proxies.RejectVhostName(rule.Listen)+".conf")
	if _, err := os.Stat(reject); err != nil {
		return nil // 没有兜底块（例如 80 端口被默认站点占了），无从复核
	}
	deadline := time.Now().Add(proxyVerifyWait)
	var last proxyProbe
	for {
		last = s.probeProxyOnce(ctx, rule, "zp-probe.invalid")
		if !proxyProbeServed(last, rule.Listen) {
			return nil // 被兜底块拒绝（444 / 连不上）→ 域名限制生效
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"规则「%s」限制了域名 %q，但复核发现域名对不上的 Host 也被转发到了后端（%s）："+
					"兜底拒绝块 %s 没有生效。这通常意味着 nginx 没有真正重载。"+
					"请到「日志中心 → nginx error_log」看 [emerg] 行",
				rule.Name, rule.Domains, describeProxyProbe(last, rule), reject)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(proxyVerifyEvery):
		}
	}
}

// isDuplicateDefaultServer 判断"兜底拒绝块写不进去"是不是因为该端口已经有
// 别的 default_server（典型：000-default.conf 的 `listen 80 default_server`）。
//
// 这不是失败：那个默认 server 会把域名对不上的 Host 接住，同样漏不到后端。
// 把它当失败会让**所有 80 端口上的带域名规则都无法创建**（最常见用法）。
func isDuplicateDefaultServer(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate default server")
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
	if err := proxyWriteVhostFn(s, ctx, rule.VhostName(), content); err != nil {
		return err
	}
	// **写盘之后、reload 之前**把日志树交还真实用户，并在 reload 后做请求级复核。
	return s.reloadProxyAndVerify(ctx, rule)
}

// removeProxyConfig 移除一条规则的配置文件并 reload。
func (s *Server) removeProxyConfig(ctx context.Context, rule *proxies.Rule) error {
	if err := s.deleteVhost(ctx, rule.VhostName()); err != nil {
		return err
	}
	return s.reloadProxyAndVerifyGone(ctx, rule)
}

// syncRejectBlocks 把"兜底拒绝块"与当前规则集对齐。
//
// 每次增删改/启停规则后都要调用：只有这样才能保证
//   - 端口上出现了带域名的规则 → 立刻补上 default_server 拒绝块；
//   - 该端口再没有带域名的规则 → 把拒绝块删掉（否则会把通配规则一起打死）。
//
// 收尾同样是"chown 日志树 → reload → 请求级复核"：兜底块也是配置，
// "写了没生效"对用户来说等于域名限制没落实，必须如实上报。
//
// 判定用的是**数据库里的**规则集，而不是调用方内存里那条：调用方都应先落库、再调用。
// 唯一的例外是"删掉域名"的更新 —— 那种情况下数据库还是旧值，会多留一个兜底块；
// 表现为"该域名仍被保护"，不会把请求漏给后端，属于安全侧的多余，下次改动即收敛。
func (s *Server) syncRejectBlocks(ctx context.Context) error {
	list, err := s.proxyRepo().List(ctx)
	if err != nil {
		return err
	}
	need := map[int]bool{}
	// rep[port] 存该端口上任意一条带域名的启用规则，供复核构造探测请求。
	rep := map[int]*proxies.Rule{}
	for _, r := range list {
		if r.Enabled && len(proxies.SplitDomains(r.Domains)) > 0 {
			need[r.Listen] = true
			if rep[r.Listen] == nil {
				rep[r.Listen] = r
			}
		}
	}

	changed := false
	for port := range need {
		content := proxies.GenerateReject(port, s.proxyLogDir())
		if err := s.writeVhost(ctx, proxies.RejectVhostName(port), content); err != nil {
			if isDuplicateDefaultServer(err) {
				// 这个端口已经有别的 default_server 了（典型就是 000-default.conf
				// 的 `listen 80 default_server`）。不匹配的 Host 会被那个默认 server
				// 接住，同样漏不到反代后端 —— 我们的兜底块既是写不进去、也不需要。
				// 以前这里会把它当成"写入失败"报一笔，其实是虚惊。
				delete(need, port)
				delete(rep, port)
				continue
			}
			return fmt.Errorf("写端口 %d 的兜底拒绝块失败：%w", port, err)
		}
		changed = true
	}
	// 清掉已经不需要的兜底块（扫目录，避免依赖内存里的端口集合）
	entries, derr := os.ReadDir(s.Cfg.VhostDir)
	if derr == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "proxy-reject-") || !strings.HasSuffix(name, ".conf") {
				continue
			}
			portStr := strings.TrimSuffix(strings.TrimPrefix(name, "proxy-reject-"), ".conf")
			port, perr := strconv.Atoi(portStr)
			if perr != nil || need[port] {
				continue
			}
			if err := s.deleteVhost(ctx, strings.TrimSuffix(name, ".conf")); err != nil {
				return fmt.Errorf("删除端口 %s 的兜底拒绝块失败：%w", portStr, err)
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.reloadRejectBlocksAndVerify(ctx, need, rep)
}

// reloadRejectBlocksAndVerify 让兜底拒绝块生效，并复核"域名限制真的落实了"。
func (s *Server) reloadRejectBlocksAndVerify(ctx context.Context, need map[int]bool, rep map[int]*proxies.Rule) error {
	proxyChownLogsFn(s)
	if err := proxyReloadFn(s, ctx); err != nil {
		return fmt.Errorf("兜底拒绝块的配置已写入，但 nginx 重载失败: %w", err)
	}
	for port := range need {
		rule := rep[port]
		if rule == nil {
			continue
		}
		if err := s.verifyProxyDomainGuard(ctx, rule); err != nil {
			return err
		}
	}
	return nil
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

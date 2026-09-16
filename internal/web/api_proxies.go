package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/acme"
	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tlsx"
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

// proxyLookupHostFn 是"目标是公网还是局域网"判定用的 DNS 解析器。
//
// 做成变量有两个原因：
//  1. 单测必须能钉住域名解析结果 —— 否则一次 `go test` 就会去查真实 DNS；
//  2. 生产里它就是 net.LookupHost，行为与面板其它探测一致。
//
// 注意：这里只用于**判定与提示**。真正决定"要不要起转发器"的是
// Manager.NeedsForward（它用注入到 Manager 里的同一个解析器）。
var proxyLookupHostFn = net.LookupHost

// proxyProbeTargetFn 是"面板自己能不能直连目标"的探针。
//
// 做成变量是为了让 directLANBlockedAdvice 可单测：那条判据要证明
// "面板连得上、只有 nginx 连不上"，而真去连一个局域网地址在单测里是不允许的。
var proxyProbeTargetFn = probeTarget

// proxyLANForwardAdvice 是"nginx 直连局域网目标失败"时给用户的话。
//
// 必须写清楚三件事：是什么（macOS 15 本地网络授权）、为什么无头服务器救不了
// （没人点弹窗）、怎么修（改成经面板转发，或手工授权）。写不清就是让用户在
// 502 面前瞎猜 —— 而这正是这个功能存在的理由。
const proxyLANForwardAdvice = "nginx 连不上局域网目标（macOS 15 本地网络授权）：无头服务器无法弹窗授权；" +
	"建议把本规则改为『经面板转发』（推荐），或到 系统设置 → 隐私与安全性 → 本地网络 里给 nginx 授权"

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
	reachable, detail := proxyProbeTargetFn(ctx, rule.Target)
	vhost := filepath.Join(s.Cfg.VhostDir, rule.VhostName()+".conf")
	_, statErr := os.Stat(vhost)
	// 域名兜底块是否存在：界面要能看出"域名限制到底有没有生效"，
	// 否则用户只能靠"不带 Host 试探"才能发现限制被 nginx 的默认 server 吃掉了。
	reject := filepath.Join(s.Cfg.VhostDir, proxies.RejectVhostName(rule.Listen)+".conf")
	_, rejectErr := os.Stat(reject)
	domainGuard := len(proxies.SplitDomains(rule.Domains)) > 0

	// 局域网出口状态：三态 + 是否真的在转发 + 实时连接/流量 + 最近一次转发错误。
	// 目标是私有网段却在直连时，界面必须显眼提示"可能因 macOS 授权失效而 502"。
	fwd, fwdListening := s.forwarders.Status(rule.ID)
	scope := s.forwarders.TargetScope(rule.Target)
	return map[string]any{
		"rule":          rule,
		"id":            rule.ID,
		"name":          rule.Name,
		"listen":        rule.Listen,
		"domains":       rule.Domains,
		"path":          rule.Path,
		"target":        rule.Target,
		"preserve_host": rule.PreserveHost,
		"websocket":     rule.Websocket,
		"enabled":       rule.Enabled,
		"remark":        rule.Remark,
		// 列表/编辑表单要能读回这两个字段（漏了会让"编辑规则"把已保存的
		// SNI 与常用请求头开关悄悄清掉）。
		"tls_name":         rule.TLSName,
		"standard_headers": rule.StandardHeaders,
		"redirect_http":    rule.RedirectHTTP,
		"created_at":       rule.Created,
		"updated_at":       rule.Updated,
		"port_listening":   listening,
		"target_host":      host,
		"target_port":      port,
		"target_ok":        reachable,
		"target_detail":    detail,
		"config_written":   statErr == nil,
		"config_path":      vhost,
		"domain_guard":     domainGuard,
		"reject_written":   rejectErr == nil,
		"reject_path":      reject,
		// HTTPS：既要能被列表行直接读（ssl_enabled 等平铺字段），
		// 也要有一份"证书文件实际内容"的汇总（ssl.days_left / ssl.domains）。
		"ssl_enabled":  rule.SSLEnabled,
		"ssl_cert":     rule.SSLCert,
		"ssl_key":      rule.SSLKey,
		"ssl_provider": rule.SSLProvider,
		"ssl_expires":  rule.SSLExpires,
		"ssl":          s.proxySSLView(rule),
		// ---- 局域网出口 ----
		"lan_forward":          rule.LANForwardMode(),
		"lan_forward_label":    proxies.LANForwardLabel(rule.LANForwardMode()),
		"forward_port":         rule.ForwardPort,
		"forward_active":       fwdListening,
		"forward_upstream":     fwd.Upstream,
		"forward_active_conns": fwd.ActiveConns,
		"forward_total_conns":  fwd.TotalConns,
		"forward_bytes_in":     fwd.BytesIn,
		"forward_bytes_out":    fwd.BytesOut,
		"forward_last_error":   fwd.LastError,
		"forward_error_count":  fwd.ErrorCount,
		"target_scope":         string(scope),
		// 直连 + 局域网目标 = 随时可能被 macOS 隐私门拦成 502。
		"lan_direct_warning": scope == proxies.ScopePrivate && !fwdListening && rule.Enabled,
	}
}

// proxySSLView 汇总反代规则证书的展示字段（与站点侧 siteSSLView 同一套口径）。
//
// 到期时间优先取自**真实证书文件**：文件才是 nginx 实际加载的东西，
// 数据库里的 ssl_expires 只是上一次写入时的快照（acme 续期后可能还没更新）。
// 读不到文件时如实标出 renew_hint，绝不显示成"正常"。
func (s *Server) proxySSLView(rule *proxies.Rule) map[string]any {
	v := map[string]any{
		"enabled":        rule.SSLEnabled,
		"provider":       rule.SSLProvider,
		"provider_label": proxies.SSLProviderLabel(rule.SSLProvider),
		"cert_path":      rule.SSLCert,
		"key_path":       rule.SSLKey,
		"expires":        rule.SSLExpires,
		"not_after":      "",
		"days_left":      -1,
		"domains":        []string{},
		"renew_hint":     "",
	}
	if !rule.SSLEnabled || rule.SSLCert == "" {
		return v
	}
	if names := certDomainNames(rule.SSLCert); len(names) > 0 {
		v["domains"] = names
	}
	if notAfter, err := tlsx.CertExpiry(rule.SSLCert); err == nil && !notAfter.IsZero() {
		days := int(time.Until(notAfter).Hours() / 24)
		v["not_after"] = notAfter.Format(time.RFC3339)
		v["expires"] = notAfter.Format("2006-01-02 15:04:05")
		v["days_left"] = days
		switch {
		case days < 0:
			v["renew_hint"] = "证书已过期，请立即续期或重新绑定"
		case days <= certRenewThresholdDays:
			v["renew_hint"] = fmt.Sprintf("证书将在 %d 天内到期", days)
		}
		return v
	}
	v["renew_hint"] = "无法读取证书文件（可能已被删除或权限不足）"
	return v
}

// certDomainNames 读取证书覆盖的域名（SAN，缺省退回 CN）。
//
// 纯 Go 解析，不起 openssl 子进程：列表页每次刷新都要显示证书域名，
// 不值得为它 fork。
func certDomainNames(certPath string) []string {
	b, err := os.ReadFile(certPath)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	names := append([]string{}, crt.DNSNames...)
	if len(names) == 0 && crt.Subject.CommonName != "" {
		names = append(names, crt.Subject.CommonName)
	}
	return names
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

	// HTTPS 字段。**签发/选择证书不走这个接口**，走
	// POST /api/v1/proxies/{id}/ssl（与站点侧的 SSL Tab 对称）。
	// 这里带上它们是为了：
	//   - 前端"关闭 HTTPS"时能一次性把 ssl_enabled=false 落库；
	//   - 允许高级用法直接指定已有证书路径。
	SSLEnabled  *bool   `json:"ssl_enabled"`
	SSLCert     *string `json:"ssl_cert"`
	SSLKey      *string `json:"ssl_key"`
	SSLProvider *string `json:"ssl_provider"`
	SSLExpires  *string `json:"ssl_expires"`

	// HTTPS 上游 SNI（留空自动推导）与"一键常用请求头"。
	TLSName         *string `json:"tls_name"`
	StandardHeaders *bool   `json:"standard_headers"`
	RedirectHTTP    *bool   `json:"redirect_http"`

	// 局域网出口三态：auto / on / off。
	//
	// 刻意**不接受** forward_port 输入：它是转发器分配出来的回环端口，
	// 让前端能随便指定会让 nginx 指向一个没人听的端口（或撞上别的服务）。
	LANForward *string `json:"lan_forward"`
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
	if req.SSLCert != nil {
		rule.SSLCert = *req.SSLCert
	}
	if req.SSLKey != nil {
		rule.SSLKey = *req.SSLKey
	}
	if req.SSLProvider != nil {
		rule.SSLProvider = *req.SSLProvider
	}
	if req.SSLExpires != nil {
		rule.SSLExpires = *req.SSLExpires
	}
	if req.TLSName != nil {
		rule.TLSName = *req.TLSName
	}
	if req.StandardHeaders != nil {
		rule.StandardHeaders = *req.StandardHeaders
	}
	if req.RedirectHTTP != nil {
		rule.RedirectHTTP = *req.RedirectHTTP
	}
	if req.LANForward != nil {
		rule.LANForward = *req.LANForward
	}
	if req.SSLEnabled != nil {
		rule.SSLEnabled = *req.SSLEnabled
		// 关闭 HTTPS 时把证书字段一并清空：留着旧路径会让"已关闭"的规则
		// 在数据库里看起来还引用着某张证书（证书页也据此判断能否删除）。
		if !*req.SSLEnabled {
			rule.SSLCert = ""
			rule.SSLKey = ""
			rule.SSLProvider = ""
			rule.SSLExpires = ""
		}
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
	if err := s.checkProxySSLPortMix(r.Context(), rule); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	// 与**站点 vhost** 的冲突：规则还没写盘，所以 selfFile 传空。
	if err := s.checkProxyAgainstSiteVhosts(rule, ""); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	created, err := s.proxyRepo().Create(r.Context(), rule)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 先起回环转发器（如果需要），再生成 nginx 配置：proxy_pass 里的端口是
	// 转发器分配出来的，顺序反了就会写成一个没人听的端口。
	if err := s.syncForwarder(created); err != nil {
		s.forwarders.Stop(created.ID)
		_ = s.proxyRepo().Delete(r.Context(), created.ID)
		fail(w, http.StatusBadGateway, "规则已保存但回环转发器无法启动："+err.Error())
		return
	}
	if created.ForwardPort > 0 {
		if err := s.proxyRepo().SetForwardPort(r.Context(), created.ID, created.ForwardPort); err != nil {
			s.forwarders.Stop(created.ID)
			_ = s.proxyRepo().Delete(r.Context(), created.ID)
			fail(w, http.StatusInternalServerError, "回环转发端口落库失败："+err.Error())
			return
		}
	}
	if err := s.applyProxy(r.Context(), created); err != nil {
		// 配置写不进去就把记录删掉，避免留下一条"看着在、其实没生效"的规则
		s.forwarders.Stop(created.ID)
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
	if err := s.checkProxySSLPortMix(r.Context(), &next); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	// 与**站点 vhost** 的冲突（跳过这份规则自己的文件）。
	if err := s.checkProxyAgainstSiteVhosts(&next, fmt.Sprintf("proxy-%d.conf", id)); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	// 先按新配置写盘（含 nginx -t 校验、失败回滚），成功后才更新数据库 ——
	// 反过来的话，配置写失败会留下"数据库说已改、文件还是旧的"的不一致。
	next.ID = cur.ID
	// 转发器先对齐（起/停/换目标；含不需要转发时把 ForwardPort 清 0），
	// 这样紧接着生成的 proxy_pass 才会用对端口。
	if err := s.syncForwarder(&next); err != nil {
		fail(w, http.StatusBadGateway, "回环转发器无法启动（配置未改动）："+err.Error())
		return
	}
	// SSL 开关切换时，该端口的域名兜底块必须先进入"带证书的中性形态"，
	// 否则中间态会被 nginx 判成 [emerg]（见 stabilizeProxyReject）。
	if next.SSLEnabled != cur.SSLEnabled && next.Enabled {
		cert, key := next.SSLCert, next.SSLKey
		if cert == "" || key == "" {
			cert, key = cur.SSLCert, cur.SSLKey
		}
		if err := s.stabilizeProxyReject(r.Context(), next.Listen, cert, key); err != nil {
			_ = s.syncForwarder(cur) // 转发器退回旧状态
			fail(w, http.StatusBadGateway, "切换 HTTPS 时调整域名兜底块失败："+err.Error())
			return
		}
	}
	if err := s.applyProxy(r.Context(), &next); err != nil {
		_ = s.applyProxy(r.Context(), cur)  // 回滚成旧配置
		_ = s.syncForwarder(cur)            // 转发器也跟着回滚（含端口）
		_ = s.syncRejectBlocks(r.Context()) // 兜底块也回到数据库描述的状态
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
	// 规则没了，回环监听器也必须跟着消失：留着就是"删了规则却还开着端口"。
	s.forwarders.Stop(id)
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
		if err := s.checkProxySSLPortMix(r.Context(), &next); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		// 启用时先起转发器（可能在停用期间被停掉了），再写配置。
		if err := s.syncForwarder(&next); err != nil {
			fail(w, http.StatusBadGateway, "启动回环转发器失败："+err.Error())
			return
		}
		if err := s.applyProxy(r.Context(), &next); err != nil {
			// 配置没写成 → 转发器也不能留着（否则会有一个监听器对应一条"没启用"的规则）
			s.forwarders.Stop(next.ID)
			fail(w, http.StatusBadGateway, "启用失败："+err.Error())
			return
		}
	} else {
		if err := s.removeProxyConfig(r.Context(), &next); err != nil {
			fail(w, http.StatusBadGateway, "停用失败："+err.Error())
			return
		}
		// 停用就关掉监听器（ForwardPort 保留，重新启用时复用同一个端口）。
		s.forwarders.Stop(next.ID)
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
//  反向代理 SSL
//
//  与「网站管理」的 SSL Tab 是同一套体验，只是入口挂到反代规则上：
//    self / mkcert / manual 三种来源把证书放到 <DataDir>/proxy-certs/<规则>/；
//    acme **直接引用面板证书库路径**（<DataDir>/certs/<primary>/fullchain.pem），
//    绝不复制 —— 续期是同路径覆盖，复制一份会让续期后线上还是旧证书。
//
//  写盘 → reload → 请求级复核与站点侧一致：复核不通过一律非 2xx。
// ============================================================================

// proxySSLReq 是 POST /api/v1/proxies/{id}/ssl 的请求体。
//
// 字段与站点侧 siteSSLReq 刻意保持一致（provider/cert/key/extra_san/
// cert_primary/domain），这样前端只需要维护一套交互，后端也能复用匹配逻辑。
type proxySSLReq struct {
	Provider string   `json:"provider"` // self / mkcert / manual / acme
	Cert     string   `json:"cert"`
	Key      string   `json:"key"`
	ExtraSAN []string `json:"extra_san"`

	// CertPrimary 指定要绑定的 ACME 证书（primary 名）。
	// 不给时按规则域名自动匹配（见 matchCertForProxy）。
	CertPrimary string `json:"cert_primary"`
	// Domain 是 cert_primary 的容错写法：前端直接给一个域名也能匹配。
	Domain string `json:"domain"`
}

// proxyCertHosts 返回给自签 / mkcert 用的域名列表。
//
// 规则可以没有域名（匹配该端口上所有 Host）。那种情况下没有可放进 SAN 的名字，
// 用 127.0.0.1 兜底（自签与 mkcert 都能签 IP），至少不生成一张空 SAN 的证书。
func proxyCertHosts(rule *proxies.Rule) []string {
	domains := proxies.SplitDomains(rule.Domains)
	if len(domains) == 0 {
		return []string{"127.0.0.1"}
	}
	return domains
}

// proxyCertDir 是 self/mkcert/manual 三种来源的证书目录。
//
// 用 VhostName()（proxy-<id>）而不是规则名：规则名是中文且可能重复，
// 做目录名既危险又不稳。
func (s *Server) proxyCertDir(rule *proxies.Rule) string {
	return filepath.Join(s.Cfg.DataDir, "proxy-certs", rule.VhostName())
}

// alignProxyCertOwner 把证书目录的属主对齐到 DataDir 的属主。
//
// 与 internal/acme/store.go 的 alignOwnerWithDataDir 是同一个真机坑：
// macOS 上 homebrew 的 nginx 以**普通用户**运行，而面板以 root 运行 ——
// root 用 0600 写出的私钥 nginx 读不到（`nginx -t` 报 Permission denied，
// 443 直接连不上）。对齐 DataDir 属主即可让同用户的 nginx 读到，同时私钥
// 仍是 0600，而不是放宽成全局可读。
//
// 失败只告警不报错：绑定是否真的成功由随后的请求级 TLS 复核判定
// （会取回真实证书比对），属主不对一定会在那里被如实挡下。
func (s *Server) alignProxyCertOwner(dir string) {
	if dir == "" || os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(s.Cfg.DataDir)
	if err != nil {
		s.Log.Warn("读不到数据目录属主（%v），反代证书属主未调整（nginx 可能读不到证书）", err)
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	uid, gid := int(st.Uid), int(st.Gid)
	if err := os.Chown(dir, uid, gid); err != nil {
		s.Log.Warn("调整 %s 属主失败：%v", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if err := os.Chown(filepath.Join(dir, e.Name()), uid, gid); err != nil {
			s.Log.Warn("调整 %s 属主失败：%v", filepath.Join(dir, e.Name()), err)
		}
	}
}

// handleProxySSL 为一条反代规则签发/绑定证书。四种来源与站点侧完全一致。
//
// 顺序：先把新配置写进 nginx（含请求级复核），成功后才落库 ——
// 与 handleProxyUpdate 同一取舍，避免出现"数据库说开了 HTTPS、nginx 其实没生效"。
// 失败时会尽力把兜底块恢复成数据库描述的状态，并返回非 2xx。
func (s *Server) handleProxySSL(w http.ResponseWriter, r *http.Request) {
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
	if !s.nginxInstalled() {
		fail(w, http.StatusConflict, "HTTPS 由 nginx 提供：请先到「应用市场 → 网站环境」安装 nginx")
		return
	}
	var req proxySSLReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var (
		certPath string
		keyPath  string
		expires  string
	)
	switch req.Provider {
	case "self", "mkcert", "manual":
		certDir := s.proxyCertDir(cur)
		if err := os.MkdirAll(certDir, 0o755); err != nil {
			fail(w, http.StatusInternalServerError, "创建证书目录失败: "+err.Error())
			return
		}
		certPath = filepath.Join(certDir, "fullchain.pem")
		keyPath = filepath.Join(certDir, "privkey.pem")
		hosts := proxyCertHosts(cur)
		switch req.Provider {
		case "self":
			if _, err := s.callHelper(r.Context(), "site-cert-self",
				"--domain", hosts[0], "--cert", certPath, "--key", keyPath); err != nil {
				fail(w, http.StatusInternalServerError, "签发自签证书失败: "+err.Error())
				return
			}
		case "mkcert":
			allHosts := append(append([]string{}, hosts...), req.ExtraSAN...)
			if _, err := s.callHelper(r.Context(), "mkcert-issue",
				"--hosts", strings.Join(allHosts, ","),
				"--cert", certPath, "--key", keyPath); err != nil {
				fail(w, http.StatusInternalServerError,
					"mkcert 签发失败: "+err.Error()+"（可先执行 brew install mkcert nss && mkcert -install）")
				return
			}
		case "manual":
			if strings.TrimSpace(req.Cert) == "" || strings.TrimSpace(req.Key) == "" {
				fail(w, http.StatusBadRequest, "手工模式需要提供证书与私钥内容")
				return
			}
			if err := os.WriteFile(certPath, []byte(req.Cert), 0o644); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			if err := os.WriteFile(keyPath, []byte(req.Key), 0o600); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		// nginx 以真实用户运行，root 写出的 0600 私钥它读不到 → 对齐属主。
		s.alignProxyCertOwner(certDir)
		notAfter, cerr := tlsx.CertExpiry(certPath)
		if cerr != nil || notAfter.IsZero() {
			fail(w, http.StatusBadRequest, "证书文件无法解析（"+certPath+"）："+errString(cerr)+
				"；请确认粘贴的是 PEM 格式的证书（fullchain）")
			return
		}
		expires = notAfter.Format("2006-01-02 15:04:05")
	case "acme":
		cert, merr := s.matchCertForProxy(cur, req)
		if merr != nil {
			// 400 而不是 500：这是"还没申请证书"这种可预期的用户状态。
			fail(w, http.StatusBadRequest, merr.Error())
			return
		}
		certPath, keyPath = cert.CertPath, cert.KeyPath
		expires = cert.NotAfter.Format("2006-01-02 15:04:05")
		if !dirExists(filepath.Dir(certPath)) || !fileExists(certPath) || !fileExists(keyPath) {
			fail(w, http.StatusBadRequest,
				"证书记录存在但文件缺失（"+certPath+"）：请在「证书」页重新申请或续期后再绑定")
			return
		}
	default:
		fail(w, http.StatusBadRequest, "不支持的证书来源: "+req.Provider)
		return
	}

	next := *cur
	next.SSLEnabled = true
	next.SSLCert = certPath
	next.SSLKey = keyPath
	next.SSLProvider = req.Provider
	next.SSLExpires = expires
	if err := next.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.checkProxySSLPortMix(r.Context(), &next); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	// 兜底块先进入"带证书的中性形态"：同时改规则 vhost 与兜底块时，
	// 中间态若被 nginx 判 [emerg] 会让整个切换必然失败（见 stabilizeProxyReject）。
	if err := s.stabilizeProxyReject(r.Context(), next.Listen, next.SSLCert, next.SSLKey); err != nil {
		fail(w, http.StatusBadGateway, "调整域名兜底块失败："+err.Error())
		return
	}
	// 证书操作会重写整份 vhost，这里顺手确认转发器还在（幂等，通常零成本）。
	if err := s.syncForwarder(&next); err != nil {
		fail(w, http.StatusBadGateway, "回环转发器无法启动："+err.Error())
		return
	}
	if err := s.applyProxy(r.Context(), &next); err != nil {
		_ = s.syncRejectBlocks(r.Context()) // 兜底块回到数据库描述的状态
		fail(w, http.StatusBadGateway, "证书已就绪，但 nginx 配置应用失败（未生效）："+err.Error())
		return
	}
	saved, err := repo.Update(r.Context(), &next)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_ssl", saved.Name,
			"规则「"+saved.Name+"」的证书已绑定并生效", err)
		return
	}
	s.audit(r, "proxy_ssl", saved.Name, "绑定证书 provider="+req.Provider+" 到期="+expires, true, "")
	view := s.proxyView(r.Context(), saved)
	ok(w, map[string]any{
		"msg":            "HTTPS 已启用",
		"rule":           saved,
		"cert":           certPath,
		"key":            keyPath,
		"expires":        expires,
		"provider":       req.Provider,
		"provider_label": proxies.SSLProviderLabel(req.Provider),
		"days_left":      siteSSLDaysLeft(certPath),
		"ssl":            view["ssl"],
	})
}

// handleProxySSLDisable 关闭一条反代规则的 HTTPS。
//
// 与站点侧不同：关掉 SSL 后规则**仍然监听原端口**（只是回到 HTTP），
// 不做"额外在 80 上 301"那种隐式行为（理由见 handleProxySSL 上方的说明）。
func (s *Server) handleProxySSLDisable(w http.ResponseWriter, r *http.Request) {
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
	next.SSLEnabled = false
	next.SSLCert = ""
	next.SSLKey = ""
	next.SSLProvider = ""
	next.SSLExpires = ""
	if err := next.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if cur.SSLEnabled {
		// 中性形态：兜底块先带证书，规则 vhost 改回非 SSL 后再收敛掉证书行。
		if err := s.stabilizeProxyReject(r.Context(), next.Listen, cur.SSLCert, cur.SSLKey); err != nil {
			fail(w, http.StatusBadGateway, "关闭 HTTPS 时调整域名兜底块失败："+err.Error())
			return
		}
	}
	if next.Enabled {
		if err := s.syncForwarder(&next); err != nil {
			fail(w, http.StatusBadGateway, "回环转发器无法启动："+err.Error())
			return
		}
		if err := s.applyProxy(r.Context(), &next); err != nil {
			_ = s.applyProxy(r.Context(), cur)  // 回滚成仍启用 SSL 的配置
			_ = s.syncRejectBlocks(r.Context()) // 兜底块也回到数据库描述的状态
			fail(w, http.StatusBadGateway, "关闭 HTTPS 失败（已回滚）："+err.Error())
			return
		}
	}
	saved, err := repo.Update(r.Context(), &next)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_ssl_disable", saved.Name,
			"规则「"+saved.Name+"」已关闭 HTTPS", err)
		return
	}
	s.audit(r, "proxy_ssl_disable", saved.Name, "关闭 HTTPS", true, "")
	ok(w, s.proxyView(r.Context(), saved))
}

// matchCertForProxy 为一条反代规则找到要用的 ACME 证书。
//
// 复用与站点侧同一套匹配逻辑（matchCertForNames）：显式 cert_primary/domain
// 优先，其次按规则域名精确命中，再退到通配。匹配不到就明确报错并指路，
// **不在这里就地签发**（签发是几十秒到几分钟的长任务，必须走任务中心）。
func (s *Server) matchCertForProxy(rule *proxies.Rule, req proxySSLReq) (*acme.Cert, error) {
	names := proxies.SplitDomains(rule.Domains)
	label := strings.Join(names, "、")
	if label == "" {
		label = "该端口上的任意域名"
	}
	return s.matchCertForNames(names, req.CertPrimary, req.Domain, label)
}

// proxiesUsingCert 返回正在引用该证书的反向代理规则（给证书页展示与删除保护用）。
//
// 判定同样用**证书路径**而不是域名：acme 续期是同路径覆盖，只有路径能精确
// 对应"哪个 vhost 里写着 ssl_certificate <这份文件>"。
func (s *Server) proxiesUsingCert(c *acme.Cert) []string {
	if c == nil || c.CertPath == "" {
		return []string{}
	}
	list, err := s.proxyRepo().List(context.Background())
	if err != nil {
		return []string{}
	}
	var out []string
	for _, r := range list {
		if r == nil || !r.SSLEnabled || r.SSLCert == "" {
			continue
		}
		if filepath.Clean(r.SSLCert) == filepath.Clean(c.CertPath) {
			out = append(out, "反向代理「"+r.Name+"」")
		}
	}
	return out
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
//
// 与站点侧同一口径：轮询间隔取 200ms（150–250ms 区间），总窗口 6 秒；
// 两者都可注入，单测压到毫秒级（不许真睡 6 秒）。
var (
	proxyVerifyWait  = 6 * time.Second
	proxyVerifyEvery = 200 * time.Millisecond
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

// probeProxyScheme 是复核该发 HTTP 还是 HTTPS。
//
// 启用 SSL 的规则只监听 TLS，用 http:// 探测只会拿到 "000"（连接被重置），
// 从而把一条正常规则误判成"没生效"。
func probeProxyScheme(rule *proxies.Rule) string {
	if rule.SSLEnabled {
		return "https"
	}
	return "http"
}

// probeProxyOnce 向规则的监听端口发一次请求（Host 由调用方指定）。
func (s *Server) probeProxyOnce(ctx context.Context, rule *proxies.Rule, host string) proxyProbe {
	code, body, err := proxyProbeFn(ctx, probeProxyScheme(rule), host, rule.Listen,
		proxyProbePath(rule), proxyProbeTimeout)
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
	msg := describeProxyProbePlain(p, rule)
	code := strings.TrimSpace(p.code)
	if code == "502" || code == "504" {
		if directToLAN(rule) {
			// 这是 macOS 15「本地网络」隐私门的典型症状：nginx 连不上局域网，
			// 而面板（Go、linker-signed）能连上。把"是什么 + 怎么修"写在错误里。
			msg += "。" + proxyLANForwardAdvice
		}
	}
	return msg
}

// describeProxyProbePlain 是不含"局域网授权"建议的现场描述（供已经自己拼
// 建议文案的调用方使用，避免同一句话出现两遍）。
func describeProxyProbePlain(p proxyProbe, rule *proxies.Rule) string {
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

// directToLAN 判断"这条规则是 nginx 直连模式、且目标是局域网地址"。
//
// 只在 502/504 的诊断路径上调用：它可能需要一次 DNS 解析，而失败现场本来
// 就没有性能要求。解析不了按私有处理（与 lan_forward=auto 的语义一致）。
func directToLAN(rule *proxies.Rule) bool {
	if rule == nil || rule.Forwarding() {
		return false
	}
	host, _, err := rule.TargetHostPort()
	if err != nil {
		return false
	}
	return proxies.ClassifyTarget(host, proxyLookupHostFn) == proxies.ScopePrivate
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
		msg := fmt.Sprintf(
			"规则「%s」的配置已写入，nginx 重载也已发出，但 %s 内新配置仍未生效"+
				"（复核发现新配置没有生效）：%s。"+
				"最常见的原因是日志文件属主是 root（写 vhost 时以 root 跑过 `nginx -t`，"+
				"它会在 %s 下创建 root 属主的日志），以真实用户运行的 nginx 打不开它们 → "+
				"reload 失败而退出码仍是 0。请依次检查："+
				"① `nginx -t` 是否通过；"+
				"② `ps -o user,pid,command -p $(cat %s)` 里的用户能否读 %s；"+
				"③ vhost %s 是否被 %s 的 include 覆盖；"+
				"④ 全局 error_log：`tail -n 20 %s` 看有没有 [emerg]。",
			rule.Name, humanWait(proxyVerifyWait), why, filepath.Dir(chk.LogPath),
			filepath.Join(s.Cfg.BrewPrefix, "var", "run", "nginx.pid"), chk.LogPath,
			filepath.Join(s.Cfg.VhostDir, rule.VhostName()+".conf"), s.Cfg.NginxConf,
			filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log"))
		if tail := s.nginxErrorLogTail(5); tail != "" {
			msg += "\n（nginx error_log 末几行）\n" + tail
		}
		return errors.New(msg)
	}
	// HTTPS 规则还要证明"端口上真的端出了这份证书"，光有 HTTP 响应不够。
	if err := s.verifyProxyTLSServed(ctx, rule); err != nil {
		return err
	}
	// 配置确实生效了，但"生效"不等于"能用"：nginx 返回 502/504 说明它连不上上游。
	// 直连模式 + 局域网目标 + **面板自己能连上** ⇒ 这不是上游挂了，而是
	// macOS 15 的本地网络隐私门只拦了 nginx。这时必须明确报错并给出修法，
	// 否则用户只会看到一个"已生效"的规则和一个永远 502 的页面。
	if advice := s.directLANBlockedAdvice(ctx, rule, chk.Probe); advice != "" {
		return errors.New(advice)
	}
	return s.verifyProxyDomainGuard(ctx, rule)
}

// directLANBlockedAdvice 在"只可能是 macOS 本地网络授权把 nginx 拦了"时返回诊断文本。
//
// 判据（缺一不可，避免把"上游本身没起来"误诊成授权问题）：
//  1. 规则是 nginx 直连模式（没有走面板转发）；
//  2. 探测拿到 502/504（nginx 命中了规则但连不上上游）；
//  3. 目标是私有/链路本地地址；
//  4. 面板自己能直连目标 —— 面板从不受这道门限制，所以这条硬证据说明
//     "只有 nginx 连不上"，而不是服务没起。
func (s *Server) directLANBlockedAdvice(ctx context.Context, rule *proxies.Rule, p proxyProbe) string {
	if rule == nil || rule.Forwarding() {
		return ""
	}
	code := strings.TrimSpace(p.code)
	if code != "502" && code != "504" {
		return ""
	}
	if _, _, err := rule.TargetHostPort(); err != nil {
		return ""
	}
	if s.forwarders.TargetScope(rule.Target) != proxies.ScopePrivate {
		return ""
	}
	if ok, _ := proxyProbeTargetFn(ctx, rule.Target); !ok {
		return "" // 面板也连不上 → 上游本身没起来，交给 target_ok 展示
	}
	return fmt.Sprintf("规则「%s」的配置已写入并被 nginx 加载，但请求 127.0.0.1:%d%s 得到 %s —— "+
		"面板自己可以直连 %s，只有 nginx 连不上。%s",
		rule.Name, rule.Listen, proxyProbePath(rule), describeProxyProbePlain(p, rule), rule.Target, proxyLANForwardAdvice)
}

// ---- HTTPS 复核：真实取回对端证书 ----

// tlsPeerInfo 是对端在 TLS 握手里**实际端出来**的证书信息。
type tlsPeerInfo struct {
	NotAfter time.Time
	DNSNames []string
	Subject  string
}

// proxyTLSPeerFn 取回该端口上真实提供的证书。
//
// 生产实现用 Go 的 crypto/tls 直接拨号。刻意 InsecureSkipVerify：
// 这里要证明的是"nginx 有没有把这份证书端出来"，不是链路可信性 ——
// 自签证书同样必须能通过复核，所以不能校验证书链。
var proxyTLSPeerFn = probeTLSPeerCert

func probeTLSPeerCert(ctx context.Context, port int, serverName string, timeout time.Duration) (tlsPeerInfo, error) {
	var out tlsPeerInfo
	if port <= 0 {
		return out, fmt.Errorf("监听端口无效：%d", port)
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		&tls.Config{
			InsecureSkipVerify: true,
			ServerName:         serverName,
			MinVersion:         tls.VersionTLS12,
		})
	if err != nil {
		return out, err
	}
	defer func() { _ = conn.Close() }()
	st := conn.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return out, errors.New("TLS 握手成功但没有拿到对端证书")
	}
	leaf := st.PeerCertificates[0]
	return tlsPeerInfo{NotAfter: leaf.NotAfter, DNSNames: leaf.DNSNames, Subject: leaf.Subject.String()}, nil
}

// proxyTLSServerName 选一个能命中该规则的 SNI。
//
// `_` 不是合法主机名（internal/proxies 在没有域名时就是这么写 server_name），
// 拿它做 SNI 没有意义：此时让 nginx 用它自己的默认 server 应答，
// 而"没有域名的规则"正是该端口的默认 server（这类端口不会生成兜底拒绝块）。
func proxyTLSServerName(rule *proxies.Rule) string {
	h := strings.TrimSpace(proxyProbeHost(rule))
	if h == "" || h == "_" || strings.Contains(h, "*") {
		return "localhost"
	}
	return h
}

// verifyProxyTLSServed 复核"HTTPS 真的起来了，而且端出来的就是我们配的那份证书"。
//
// 判据不是"端口能连上"，而是：
//  1. 配置里写的证书文件能被解析（读不到就别谈生效）；
//  2. 对该端口做一次真实 TLS 握手，拿回对端 leaf 证书；
//  3. 对端证书的 NotAfter 必须与配置文件的 NotAfter 一致 —— 这能抓到
//     "ssl_certificate 没生效 / 加载的还是旧证书 / 请求落在了别的 server 块"。
//
// 这是"失败不许谎报"的落点：证书文件写进去了、`nginx -s reload` 退出码是 0，
// 都不等于能握手成功。
func (s *Server) verifyProxyTLSServed(ctx context.Context, rule *proxies.Rule) error {
	if !rule.SSLEnabled {
		return nil
	}
	want, err := tlsx.CertExpiry(rule.SSLCert)
	if err != nil {
		return fmt.Errorf("规则「%s」已启用 HTTPS，但证书文件读不到（%s）：%v。"+
			"nginx 会因此加载失败或起不来，请先在「SSL 证书」页重新签发/续期",
			rule.Name, rule.SSLCert, err)
	}
	info, derr := proxyTLSPeerFn(ctx, rule.Listen, proxyTLSServerName(rule), proxyProbeTimeout)
	if derr != nil {
		return fmt.Errorf("规则「%s」的 HTTPS 复核失败：在 127.0.0.1:%d 上做 TLS 握手时 %v。"+
			"最常见的原因是 nginx 没有真正重载（证书文件属主是 root 时，以普通用户运行的 nginx "+
			"读不到它 → reload 失败但退出码仍是 0），或该端口上的默认 server 抢先应答了。"+
			"请到「日志中心 → nginx error_log」看 [emerg] 行",
			rule.Name, rule.Listen, derr)
	}
	if !info.NotAfter.Equal(want) {
		return fmt.Errorf("规则「%s」的 HTTPS 复核失败：配置里写的是 %s（到期 %s），"+
			"但 127.0.0.1:%d 实际端出来的证书到期时间是 %s —— 说明这份 ssl_certificate 没有生效",
			rule.Name, rule.SSLCert, want.Format("2006-01-02 15:04:05"),
			rule.Listen, info.NotAfter.Format("2006-01-02 15:04:05"))
	}
	return nil
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
				"规则「%s」的 nginx 配置已删除、重载命令也返回成功，但等待 %s 后复核发现"+
					"**它仍在生效**（每次请求后它自己的访问日志 %s 仍在增长，最后一次响应 %s）。"+
					"这通常意味着 nginx 没有真正重载（日志属主是 root 时 reload 会失败但退出码仍是 0）。"+
					"请检查：`nginx -t`、`ps -o user,pid,command -p $(cat %s)` 的运行用户权限、"+
					"以及全局 error_log（`tail -n 20 %s`）里的 [emerg]。",
				rule.Name, humanWait(proxyVerifyWait), logPath, describeProxyProbe(last, rule),
				filepath.Join(s.Cfg.BrewPrefix, "var", "run", "nginx.pid"),
				filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log"))
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
//
// 与 applySite 一样：失败时撤销本次写入的 vhost（还原旧内容 / 删掉本次新建的）。
// 反代的调用方（创建/更新/启停/绑证书）都靠这一点才能做到
// "接口非 2xx = 这次没做成、重试不会被上一次的残留挡住"。
func (s *Server) applyProxy(ctx context.Context, rule *proxies.Rule) error {
	if !rule.Enabled {
		return s.removeProxyConfig(ctx, rule)
	}
	content, err := rule.Generate(s.proxyLogDir())
	if err != nil {
		return err
	}
	snap := s.snapshotVhost(rule.VhostName())
	// 复用站点的写盘通道：它经提权助手做原子写 + nginx -t 校验 + 失败回滚
	if err := proxyWriteVhostFn(s, ctx, rule.VhostName(), content); err != nil {
		return err
	}
	// **写盘之后、reload 之前**把日志树交还真实用户，并在 reload 后做请求级复核。
	if err := s.reloadProxyAndVerify(ctx, rule); err != nil {
		return s.rollbackVhostWrite(ctx, snap, proxyWriteVhostFn, proxyReloadFn, err)
	}
	return nil
}

// syncForwarder 让一条规则的回环转发器与当前配置对齐，并把分配到的端口写回
// rule.ForwardPort。
//
// **必须在 applyProxy / Generate 之前调用**：生成的 proxy_pass 用的是
// rule.ForwardPort，而端口由管理器分配（优先复用数据库里的旧值，重启后
// nginx 配置才不会指向一个没人听的端口）。
//
// 不需要转发（off / 公网 / 回环目标）时它会停掉监听器并把 ForwardPort 清 0，
// 渲染随之退回直连 —— 这样"关掉转发"是真的关掉，而不是留个半死不活的监听器。
func (s *Server) syncForwarder(rule *proxies.Rule) error {
	if rule == nil {
		return nil
	}
	if !rule.Enabled {
		s.forwarders.Stop(rule.ID)
		return nil
	}
	if _, err := s.forwarders.Ensure(rule); err != nil {
		return err
	}
	return nil
}

// reconcileForwarders 在面板启动时把转发器对齐到数据库里的规则。
//
// 三件事：
//  1. 该起的起（enabled 且需要转发）、该停的停（停用/删除/不再需要转发）；
//  2. 端口分配结果落库（重启后优先复用同一个端口）；
//  3. 端口变了就重写对应 vhost —— 否则 nginx 里的 proxy_pass 会指向旧端口，
//     表现为"转发器起来了、规则却是 502"。
func (s *Server) reconcileForwarders(ctx context.Context) {
	list, err := s.proxyRepo().List(ctx)
	if err != nil {
		s.Log.Warn("读取反向代理规则失败，跳过回环转发器对齐: %v", err)
		return
	}
	changed := s.forwarders.Reconcile(list)
	if len(changed) == 0 {
		return
	}
	changedSet := make(map[int64]bool, len(changed))
	for _, id := range changed {
		changedSet[id] = true
	}
	for _, rule := range list {
		if !changedSet[rule.ID] {
			continue
		}
		if err := s.proxyRepo().SetForwardPort(ctx, rule.ID, rule.ForwardPort); err != nil {
			s.Log.Warn("保存规则 %d 的回环转发端口失败: %v", rule.ID, err)
		}
		if !rule.Enabled {
			continue
		}
		if err := s.applyProxy(ctx, rule); err != nil {
			// nginx 没装/没起来时不要让整个启动失败：转发器已经起了，
			// 下一次保存规则会重新写 vhost。如实记一笔。
			s.Log.Warn("回环转发端口变化后重写规则 %d 的 nginx 配置失败: %v", rule.ID, err)
		}
	}
	if len(changed) > 0 {
		s.Log.Info("已对齐回环转发器：%d 条规则的端口有变化", len(changed))
	}
}

// removeProxyConfig 移除一条规则的配置文件并 reload。
//
// 复核发现"删了却仍在生效"时**把文件还原回去**：那时记录还在、配置却没了，
// 是最典型的不一致状态；还原后重试删除才是干净的。
func (s *Server) removeProxyConfig(ctx context.Context, rule *proxies.Rule) error {
	snap := s.snapshotVhost(rule.VhostName())
	if err := siteDeleteVhostFn(s, ctx, rule.VhostName()); err != nil {
		return err
	}
	if err := s.reloadProxyAndVerifyGone(ctx, rule); err != nil {
		return s.rollbackVhostWrite(ctx, snap, proxyWriteVhostFn, proxyReloadFn, err)
	}
	return nil
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
	specs := proxyRejectSpecs(list)
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
	for port, sp := range specs {
		if sp.SSL && (sp.Cert == "" || sp.Key == "") {
			return fmt.Errorf("端口 %d 上有已启用 HTTPS 的规则，但数据库里没有证书路径："+
				"nginx 要求同一端口上每个 server 块都有证书，无法为域名兜底块提供证书", port)
		}
		content := proxies.GenerateRejectWithCert(port, s.proxyLogDir(), sp.Cert, sp.Key, sp.SSL)
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

// proxyRejectSpec 描述某端口上"域名兜底拒绝块"该长什么样。
type proxyRejectSpec struct {
	SSL  bool   // 该端口上是否至少有已启用的 HTTPS 规则
	Cert string // SSL=true 时兜底块要带的证书（否则 nginx [emerg]）
	Key  string
}

// proxyRejectSpecs 从规则集推导出每个端口的兜底块形态。
//
// 抽成纯函数是为了让单测能直接钉住"SSL 端口必须带证书"这条判据
// （真机实测：同一端口上只要有一个 server 块写了 ssl，所有 server 块
// 都必须有 ssl_certificate，否则 `nginx -t` 直接 [emerg]）。
func proxyRejectSpecs(rules []*proxies.Rule) map[int]proxyRejectSpec {
	out := map[int]proxyRejectSpec{}
	for _, r := range rules {
		if r == nil || !r.Enabled || len(proxies.SplitDomains(r.Domains)) == 0 {
			continue
		}
		sp := out[r.Listen]
		if r.SSLEnabled {
			sp.SSL = true
			if sp.Cert == "" {
				sp.Cert, sp.Key = r.SSLCert, r.SSLKey
			}
		}
		out[r.Listen] = sp
	}
	return out
}

// stabilizeProxyReject 在 SSL 开关切换的写盘之前，把该端口的兜底块改成
// "listen <port> default_server;" + 证书行的中性形态。
//
// 为什么需要（真机 nginx 1.31.5 实测）：SSL 开/关都要同时改两个文件
// （规则 vhost 与兜底块），而写每个文件都会跑一次 `nginx -t`。中间态里
// "有 ssl 的 server + 没证书的 server"会被判 [emerg] 并回滚，导致切换永远失败。
// 带证书行的非 SSL server 在任何组合下都合法，所以先落它、再改规则 vhost。
func (s *Server) stabilizeProxyReject(ctx context.Context, port int, certPath, keyPath string) error {
	if certPath == "" || keyPath == "" {
		return nil
	}
	list, err := s.proxyRepo().List(ctx)
	if err != nil {
		return err
	}
	if !proxies.RejectPorts(list)[port] {
		return nil // 这个端口不需要兜底块（例如全是通配规则）
	}
	content := proxies.GenerateRejectWithCert(port, s.proxyLogDir(), certPath, keyPath, false)
	return s.writeVhost(ctx, proxies.RejectVhostName(port), content)
}

// checkProxySSLPortMix 拦住"同一端口上 HTTP 与 HTTPS 混用"。
//
// 为什么必须拦：nginx 的同一 listen 端口只有一种协议 —— 只要有一个 server 块
// 写了 `ssl`，整个端口就按 TLS 处理，另一个"以为自己是 HTTP"的规则会静默失效
// （客户端用 http:// 访问会握手失败）。这种失效用户完全看不出来，所以宁可在
// 保存时明确拒绝，也不写出一份"两条都显示已启用、只有一条能用"的配置。
//
// 同时拦住"端口 80 + HTTPS"：80 由面板默认站点（000-default.conf）占着
// default_server，而它没有证书 —— 一旦该端口出现 ssl，nginx 会直接 [emerg]。
func (s *Server) checkProxySSLPortMix(ctx context.Context, rule *proxies.Rule) error {
	if rule == nil || !rule.Enabled {
		return nil // 停用的规则不写配置，不会造成混用
	}
	if rule.SSLEnabled && rule.Listen == 80 {
		return fmt.Errorf("端口 80 是面板默认站点的 HTTP 端口，不能作为 HTTPS 端口：" +
			"请把监听端口改成 443 或其它端口（nginx 要求同端口的每个 server 块都有证书）")
	}
	list, err := s.proxyRepo().List(ctx)
	if err != nil {
		return err
	}
	for _, other := range list {
		if other == nil || other.ID == rule.ID || !other.Enabled || other.Listen != rule.Listen {
			continue
		}
		if other.SSLEnabled == rule.SSLEnabled {
			continue
		}
		// other 与 rule 的 SSL 一定不同（相同的上面 continue 了），两者的模式互为反面。
		//
		// ⚠️ 这里曾经把两个标签写反（赋值与打印顺序对不上）：用户明明"新规则没开
		// HTTPS、已有规则是 HTTPS"，弹出来的却是「已有HTTP规则…而这条是HTTPS」——
		// 正好把人往反方向带（2026-09-17 用户实测报障）。
		// 这条是 HTTPS ⇒ 那条是 HTTP；这条是 HTTP ⇒ 那条是 HTTPS。
		otherMode, myMode := "HTTP", "HTTPS"
		if !rule.SSLEnabled {
			otherMode, myMode = "HTTPS", "HTTP"
		}
		return fmt.Errorf("端口 %d 上已有%s规则「%s」，而这条是%s：nginx 的同一端口不能同时跑 "+
			"HTTP 与 HTTPS（只要有一个 server 块启用 ssl，整个端口就变成 TLS）。\n"+
			"三条出路：① 让这条也启用 HTTPS（同一端口必须同为 TLS；域名有证书时这条最简单）；"+
			"② 把这条规则改到别的端口；③ 先停用/删除「%s」",
			rule.Listen, otherMode, other.Name, myMode, other.Name)
	}
	return nil
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

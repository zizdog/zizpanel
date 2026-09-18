package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tlsx"
	"github.com/zizdog/zizpanel/internal/upgrade"
)

// ============================================================================
//  站点管理接口
//
//  职责划分：
//    - 面板侧（本文件）：校验输入、生成 nginx 配置、维护数据库、编排操作顺序
//    - 助手侧（zizpanel-helper）：白名单路径内原子落盘、语法校验、reload
//
//  所有会改动 nginx 的操作都遵循同一个安全顺序：
//    写配置（helper 内部校验并允许回滚）→ reload → 失败则还原数据库状态
// ============================================================================

// siteLogDir 返回站点日志目录。约定与现有 LNMP 环境一致（~/www/_logs）。
func (s *Server) siteLogDir() string {
	return filepath.Join(s.Cfg.WWWRoot, "_logs")
}

// siteMgr 构造站点管理器。
func (s *Server) siteMgr() *sites.Manager {
	return sites.NewManager(s.Store, sites.Options{LogDir: s.siteLogDir()})
}

// helperCall 调用提权助手并返回解析后的结果（包级函数，便于其它模块复用）。
func helperCall(ctx context.Context, args ...string) (map[string]any, error) {
	bin := "/opt/zizpanel/bin/zizpanel-helper"
	if v := os.Getenv("ZIZPANEL_WORKDIR"); v != "" {
		bin = filepath.Join(filepath.Dir(v), "bin", "zizpanel-helper")
	}
	return helperCallBin(ctx, bin, args...)
}

// helperCallBin 用指定路径的助手执行。
func helperCallBin(ctx context.Context, bin string, args ...string) (map[string]any, error) {
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("提权助手不存在: %s", bin)
	}
	cmdArgs := args
	if os.Geteuid() != 0 {
		cmdArgs = append([]string{"-n", bin}, args...)
		bin = "/usr/bin/sudo"
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := execCommand(ctx, bin, cmdArgs...)
	// stderr 必须单独收集并带进错误信息。
	// 提权失败最常见的原因是 sudoers 规则不对/未生效，而原因只写在 stderr 上
	// （例如 "sudo: a password is required"）。只用 Output() 会把它丢掉，
	// 用户看到的是一句毫无信息量的 "exit status 1 ()"，根本没法排查。
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	var parsed map[string]any
	if len(out) > 0 {
		_ = jsonUnmarshal(out, &parsed)
	}
	if err != nil {
		if parsed != nil {
			if msg, _ := parsed["error"].(string); msg != "" {
				return parsed, errors.New(msg)
			}
		}
		detail := strings.TrimSpace(stderrBuf.String())
		if detail == "" {
			detail = strings.TrimSpace(string(out))
		}
		if detail == "" {
			detail = "助手没有输出任何信息"
		}
		return parsed, fmt.Errorf("调用提权助手失败: %v（%s）", err, detail)
	}
	if parsed == nil {
		// 退出码 0 但输出不是 JSON：不能当成成功，
		// 否则调用方会以为操作生效了（例如"站点已创建"）。
		return nil, fmt.Errorf("提权助手返回了无法解析的结果: %s", strings.TrimSpace(string(out)))
	}
	if okv, _ := parsed["ok"].(bool); !okv {
		msg, _ := parsed["error"].(string)
		if msg == "" {
			msg, _ = parsed["msg"].(string)
		}
		return parsed, errors.New(msg)
	}
	return parsed, nil
}

// callHelper 调用提权助手并解析 JSON 结果。
//
// 助手必须由 root 调用（sudoers 白名单）或当前进程本身就是 root。
// 具体执行逻辑统一在 helperCallBin 里，这里只负责解析路径：
// 两份重复实现曾经分叉过（一处修了错误信息、另一处没修），不要再复制。
func (s *Server) callHelper(ctx context.Context, args ...string) (map[string]any, error) {
	return helperCallBin(ctx, s.Cfg.ServicePath("zizpanel-helper"), args...)
}

// writeVhost 把配置写入 nginx vhost 目录（经助手，含语法校验与回滚）。
func (s *Server) writeVhost(ctx context.Context, domain, content string) error {
	bin := s.Cfg.ServicePath("zizpanel-helper")
	args := []string{"vhost-write", domain}
	if os.Geteuid() != 0 {
		args = append([]string{"-n", bin}, args...)
		bin = "/usr/bin/sudo"
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := execCommand(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	var parsed map[string]any
	if len(out) > 0 {
		_ = jsonUnmarshal(out, &parsed)
	}
	if err != nil {
		if parsed != nil {
			if msg, _ := parsed["error"].(string); msg != "" {
				return errors.New(msg)
			}
		}
		return fmt.Errorf("写入配置失败: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// nginxReload 重载 nginx。
func (s *Server) nginxReload(ctx context.Context) error {
	_, err := s.callHelper(ctx, "nginx-reload")
	return err
}

// applySite 生成并应用一个站点的配置。
//
// 顺序很关键：先生成内容 → 让 helper 写入并校验 → reload → **按真实结果复核**。
// helper 在写入前会做 nginx -t，不通过会回滚文件，
// 因此这里不需要额外处理"配置已写坏"的情况。
//
// PHP 端点用 ResolveEndpoint：它要求该端点**真的在监听**。
// 宁可在保存时明确失败（"PHP 8.4 没在运行"），也不要写出一份
// 指向空气的 vhost —— 那种情况下站点是 502，用户得翻 nginx 错误日志
// 才能知道是 PHP 版本没起来。
//
// 失败时**撤销本次写入的 vhost**（还原成写入前的内容，或删掉本次新建的）：
// 只有这样，"接口返回非 2xx"才真的等于"这次没做成"，
// 用户再点一次也不会撞上 "store 里 ssl_enabled=true、nginx 却没在服务" 的自相矛盾状态。
func (s *Server) applySite(ctx context.Context, site *sites.Site) error {
	pass := ""
	if site.PHPVersion != "" {
		resolved, err := sites.ResolveEndpoint(s.Cfg.BrewPrefix, site.PHPVersion)
		if err != nil {
			return err
		}
		pass = resolved
	}
	content, err := site.Generate(sites.Options{
		LogDir:      s.siteLogDir(),
		FastCGIPass: pass,
		// 请求体上限来自面板配置（默认 512m）：nginx 出厂 1m 会让
		// phpMyAdmin 导入几十 MB 的 SQL 直接 413（用户报障原文）。
		ClientMaxBodySize: s.uploadLimits().ClientMaxBodySize,
	})
	if err != nil {
		return err
	}
	snap := s.snapshotVhost(site.Domain)
	if err := siteWriteVhostFn(s, ctx, site.Domain, content); err != nil {
		return err
	}
	// 写 vhost 的 helper 会以 root 跑 `nginx -t`，而 nginx **在校验时就会创建
	// access_log/error_log** —— 于是日志文件是 root 属主；而已启动的 nginx master
	// 以真实用户运行，reload 时打不开它们：
	//   [emerg] open() "/Users/zizdog/www/_logs/xxx.access.log" failed (13: Permission denied)
	// 结果是**配置根本没加载**（站点 404），而 reload 命令仍返回 0。
	// 所以 reload 之前把日志目录交还真实用户（与"整理默认站点"同一套做法）。
	// 真机 2026-09-17：mini 上 nginx 已改成以真实用户运行，这条路径必须先修。
	if s.Cfg.User != "" && os.Geteuid() == 0 {
		_ = chownTreeTo(s.siteLogDir(), s.Cfg.User)
	}
	if err := siteReloadFn(s, ctx); err != nil {
		return s.rollbackVhostWrite(ctx, snap, siteWriteVhostFn, siteReloadFn,
			fmt.Errorf("配置已写入但 nginx 重载失败: %w", err))
	}
	// reload 命令成功 ≠ 新配置生效：nginx 读配置失败（例如日志/证书文件打不开）
	// 时会在错误日志里写 [emerg]，而 `nginx -s reload` **退出码依然是 0**。
	// 不复核的话，"面板说创建成功、用户打开是 404/502"就是必然结果。
	if err := s.verifySiteServed(ctx, site); err != nil {
		return s.rollbackVhostWrite(ctx, snap, siteWriteVhostFn, siteReloadFn, err)
	}
	return nil
}

// siteWriteVhostFn / siteReloadFn / siteProbeFn / siteDeleteVhostFn 是
// "写 vhost → reload → 复核"这条通道的可注入步骤。
//
// 为什么做成包级变量：这条通道的收尾复核与失败回滚必须能被单测覆盖，而单测
// **不允许调用提权助手、不允许真发网络请求**。生产环境这些变量指向
// 真实实现，行为与直接调用完全一致。
//
// siteWriteVhostFn 同时被 applyDefaultVhost 与失败回滚（还原旧内容）复用：
// 只保留一种"把 vhost 写进 nginx 目录"的写法，避免回滚走一条没人测过的路径。
var (
	siteWriteVhostFn = func(s *Server, ctx context.Context, domain, content string) error {
		return s.writeVhost(ctx, domain, content)
	}
	siteReloadFn = func(s *Server, ctx context.Context) error {
		return s.nginxReload(ctx)
	}
	// siteProbeFn 的签名与 sites_check.go 的 curlSite 一致（用 --resolve 钉到 127.0.0.1）。
	siteProbeFn = curlSite
	// siteDeleteVhostFn 删除一份 vhost（回滚"本次新建的"文件时用）。
	siteDeleteVhostFn = func(s *Server, ctx context.Context, name string) error {
		return s.deleteVhost(ctx, name)
	}
	// siteReadVhostFn 读取一份 vhost 的当前内容（回滚要还原它，就必须先读出来）。
	siteReadVhostFn = func(s *Server, path string) ([]byte, error) {
		return os.ReadFile(path)
	}
)

// siteVerifyWait / siteVerifyEvery 控制"等新配置生效"的轮询窗口。
//
// 为什么必须等：`nginx -s reload` 只是给 master 发信号，新 worker 接管旧 worker
// 之间的**旧配置仍然在应答**（真机 2026-09-16：写完立刻探测拿到 404，随后独立
// 复测同一请求返回期望的 403 —— 配置从头到尾都是对的，只是探测太早）。
// 所以窗口内的 404/000/"403 与期望不符" 一律视为**还没生效**，不是失败。
//
// 做成变量：单测必须能把它压到毫秒级（不许真睡 5 秒）。
var (
	siteVerifyWait  = 6 * time.Second
	siteVerifyEvery = 200 * time.Millisecond
)

// siteProbeTimeout 是单次探针的超时。不能太长：复核要在窗口内轮询多次，
// 单次探测挂满整个窗口就等于只测了一次。
const siteProbeTimeout = 4 * time.Second

// siteVhostProbePath 是"配置是否真的生效"的探测路径。
//
// 为什么是这个点开头的路径：每个站点 vhost（internal/sites 生成）都带这条规则
//
//	location ~ /\. { deny all; access_log off; log_not_found off; }
//
// 正则 location 的优先级高于 `location /`，所以**只要该站点的 vhost 真的被
// nginx 加载了**，这个请求必定返回 403。而如果 vhost 没加载，请求会落到默认
// 站点（000-default）—— 它没有这条规则，`try_files $uri $uri/ =404` 会返回 404。
//
// 于是"403 = 配置生效 / 404 = 配置没生效"是一个与站点自身内容无关的硬判据：
//   - 不受"站点根目录还没有 index 文件"影响（那种情况首页也是 404，
//     但那是站点内容问题，不是配置没加载）；
//   - 不受反向代理上游是否健康影响（正则规则在上游之前命中）；
//   - 不需要往用户站点目录里写探针文件。
const siteVhostProbePath = "/.zp-vhost-probe"

// verifySiteServed 在 reload 之后按真实请求复核"这份 vhost 真的生效了"。
//
// 注意用 curlSite（--resolve 钉到 127.0.0.1），所以域名没有 DNS 解析也能测；
// 也不能改成直接 fetchLocal("<域名>/")：那会走真实 DNS。
//
// **轮询**而不是一次性判定：`nginx -s reload` 是异步的，旧 worker 在新配置
// 生效前仍会用旧配置应答。窗口内拿到 404/000/别的状态码只说明"还没轮到新配置"，
// 不是失败；只有窗口耗尽仍拿不到 403 才算失败。
func (s *Server) verifySiteServed(ctx context.Context, site *sites.Site) error {
	scheme, port := "http", 80
	if site.SSLEnabled {
		scheme, port = "https", 443
	}
	deadline := time.Now().Add(siteVerifyWait)
	var (
		lastCode string
		lastErr  error
		tries    int
	)
	for {
		tries++
		code, _, err := siteProbeFn(ctx, scheme, site.Domain, port, siteVhostProbePath, siteProbeTimeout)
		lastCode, lastErr = code, err
		if err == nil && code == "403" {
			return nil
		}
		// 请求上下文结束（用户关掉页面/任务被取消）：不再等，如实上报。
		if ctxErr := ctx.Err(); ctxErr != nil {
			lastErr = ctxErr
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
		case <-time.After(siteVerifyEvery):
			continue
		}
		break
	}
	return s.siteVerifyTimeoutError(site, scheme, port, tries, lastCode, lastErr)
}

// siteVerifyTimeoutError 是"等了整个窗口仍没生效"时的错误：必须能指导排查。
//
// 只写"复核失败"等于让用户自己猜；这里把四个检查点直接写进错误里，
// 并把 nginx 全局 error_log 的末几行**附在消息里**（用户不必再去翻日志中心）。
func (s *Server) siteVerifyTimeoutError(site *sites.Site, scheme string, port, tries int,
	code string, perr error) error {
	logDir := s.siteLogDir()
	vhostPath := filepath.Join(s.Cfg.VhostDir, site.Domain+".conf")
	pidPath := filepath.Join(s.Cfg.BrewPrefix, "var", "run", "nginx.pid")
	errLog := filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log")

	var b strings.Builder
	fmt.Fprintf(&b, "站点 %s 的配置已写入，nginx 重载也已发出，但 %s 内新配置仍未生效"+
		"（共探测 %d 次，最后一次 %s://%s:%d%s 期望 403，实际 %s）。"+
		"先别急着改配置：这类情况多半是 nginx 没有真正加载新配置，而不是配置写错了。请依次检查：",
		site.Domain, humanWait(siteVerifyWait), tries,
		scheme, site.Domain, port, siteVhostProbePath, describeProbeCode(code, perr))
	b.WriteString("① `nginx -t` 是否通过（面板「网站管理」页可直接校验配置）；")
	fmt.Fprintf(&b, "② nginx 进程与权限：`ps -o user,pid,command -p $(cat %s)` 里的用户"+
		"能不能读 %s 与日志目录 %s；", pidPath, vhostPath, logDir)
	fmt.Fprintf(&b, "③ vhost 是否真的在 include 目录里：%s 应存在，且被 %s 的 include 覆盖；",
		vhostPath, s.Cfg.NginxConf)
	fmt.Fprintf(&b, "④ 全局 error_log 末几行：`tail -n 20 %s`（面板「日志中心 → nginx 主错误日志」）"+
		"看有没有 [emerg]。", errLog)
	if tail := s.nginxErrorLogTail(5); tail != "" {
		b.WriteString("\n（nginx error_log 末几行）\n")
		b.WriteString(tail)
	}
	switch code {
	case "404":
		fmt.Fprintf(&b, "\n404 说明请求落到了默认站点：最常见的原因是 %s 里的日志文件"+
			"归属/权限不对（nginx worker 打不开 access_log 时 reload 会失败但退出码仍是 0），"+
			"或 vhost 文件没写进 conf.d。", logDir)
	case "000", "":
		fmt.Fprintf(&b, "\n连不上 nginx：确认 nginx 正在运行、%d 端口在监听%s。", port, errSuffix(perr))
	default:
		b.WriteString("\n该状态码不是站点 vhost 的隐藏文件规则给出的，" +
			"通常意味着自定义配置（extra_conf）覆盖了 `location ~ /\\.`，请检查后重试。")
	}
	return errors.New(b.String())
}

// humanWait 把等待窗口写成"6 秒"/"0.2 秒"，用于错误文案。
func humanWait(d time.Duration) string {
	if d >= time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%d 秒", int(d/time.Second))
	}
	return fmt.Sprintf("%.1f 秒", d.Seconds())
}

// nginxErrorLogTail 读取 nginx 全局 error_log 的末几行（读不到返回空串）。
//
// 复核超时的错误里直接**附上**这几行：用户不必先去日志中心翻，
// 而 [emerg] 那一行往往就是"reload 退出码是 0 但配置没加载"的全部原因。
func (s *Server) nginxErrorLogTail(lines int) string {
	path := filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log")
	tail, _, err := tailFile(path, lines)
	if err != nil || strings.TrimSpace(tail) == "" {
		return ""
	}
	return tail
}

func describeProbeCode(code string, err error) string {
	if code == "" {
		if err != nil {
			return "无响应（" + err.Error() + "）"
		}
		return "无响应"
	}
	return code
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return "（" + err.Error() + "）"
}

// ============================================================================
//  vhost 写入的回滚
//
//  为什么需要：复核失败时如果只在数据库里回滚、不管磁盘上的 vhost，
//  就会出现"接口说没做成、nginx 里却留着一份新配置（或旧记录配新配置）"。
//  更糟的是重试时被上一次的残留挡住。这里保存写入前的字节，
//  失败时原样写回；本次新建的文件则删掉。
// ============================================================================

// vhostSnapshot 是一份 vhost 文件被本次写入覆盖之前的样子。
type vhostSnapshot struct {
	name    string
	existed bool   // 写入前文件是否存在（false = 回滚时应删除）
	prev    []byte // 写入前的内容
	unknown bool   // 读旧内容失败（不是"不存在"）：回滚不安全，宁可不做
}

// snapshotVhost 读取 vhost 的当前内容作为回滚依据。
//
// 三种情况必须分开，混在一起会**删掉用户的旧配置**：
//   - 文件不存在 → 本次是新建，回滚删除；
//   - 读到了内容 → 回滚原样写回；
//   - 读失败（权限/IO）→ 回滚不安全，标记 unknown，回滚时如实报告而不是删文件。
func (s *Server) snapshotVhost(name string) vhostSnapshot {
	path := filepath.Join(s.Cfg.VhostDir, name+".conf")
	b, err := siteReadVhostFn(s, path)
	if err == nil {
		return vhostSnapshot{name: name, existed: true, prev: b}
	}
	if os.IsNotExist(err) {
		return vhostSnapshot{name: name}
	}
	return vhostSnapshot{name: name, unknown: true}
}

// rollbackVhostWrite 撤销本次对 vhost 的写入，并尽力让 nginx 与磁盘重新对齐。
//
// writeFn / reloadFn 由调用方给（站点侧 = siteWriteVhostFn/siteReloadFn，
// 反代侧 = proxyWriteVhostFn/proxyReloadFn），这样回滚走的是与写入**完全相同**
// 的那条通道，不会出现"回滚路径没人测过、真机才发现调不动"的情况。
//
// 回滚后再发一次 reload：复核失败只说明"没等到生效"，并不保证 nginx 当前
// 加载的是哪一份；把文件还原后再 reload 一次，至少让"磁盘 = nginx 正在用的"
// 重新成立。reload 失败只记日志 —— 绝不能覆盖真正的失败原因。
func (s *Server) rollbackVhostWrite(ctx context.Context, snap vhostSnapshot,
	writeFn func(*Server, context.Context, string, string) error,
	reloadFn func(*Server, context.Context) error, cause error) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	path := filepath.Join(s.Cfg.VhostDir, snap.name+".conf")
	if snap.unknown {
		return fmt.Errorf("%w（另外：写入前读不到 %s 的旧内容，为避免删掉旧配置**未自动回滚**；"+
			"请手工检查/还原这个文件）", cause, path)
	}
	var rerr error
	if snap.existed {
		rerr = writeFn(s, rctx, snap.name, string(snap.prev))
	} else {
		rerr = siteDeleteVhostFn(s, rctx, snap.name)
	}
	if rerr != nil {
		return fmt.Errorf("%w（另外：撤销本次写入 %s 也失败了：%v；"+
			"磁盘上可能仍留着这次写入的配置，请手工删除/还原后重试）", cause, path, rerr)
	}
	if reloadFn != nil {
		if rerr := reloadFn(s, rctx); rerr != nil {
			s.Log.Warn("回滚 %s 后重载 nginx 失败（磁盘已还原，nginx 可能仍在用内存里的旧配置）: %v",
				snap.name, rerr)
		}
	}
	return cause
}

// ensureSiteRoot 创建站点根目录。
//
// 面板以 root 运行，但站点目录必须归属真实用户，否则用户无法用编辑器/上传文件。
func (s *Server) ensureSiteRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("创建站点目录失败: %w", err)
	}
	// 归属真实用户（root 运行时 $HOME 不同，必须用配置里的用户名）
	if s.Cfg.User != "" && s.Cfg.User != "root" {
		if uid, gid, err := lookupIDs(s.Cfg.User); err == nil {
			_ = os.Chown(root, uid, gid)
		}
	}
	return nil
}

// ---------- HTTP 接口 ----------

// handleSiteList 列出所有站点（含实时状态）。
func (s *Server) handleSiteList(w http.ResponseWriter, r *http.Request) {
	mgr := s.siteMgr()
	list, err := mgr.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取站点失败: "+err.Error())
		return
	}
	// 探测每个站点的 nginx 配置是否已存在（数据库与文件是否一致）
	type item struct {
		*sites.Site
		ConfExists bool `json:"conf_exists"`
		Running    bool `json:"running"`
	}
	out := make([]item, 0, len(list))
	vhostDir := s.Cfg.VhostDir
	for _, st := range list {
		confPath := filepath.Join(vhostDir, st.Domain+".conf")
		_, err := os.Stat(confPath)
		out = append(out, item{Site: st, ConfExists: err == nil, Running: err == nil && st.Enabled})
	}
	ok(w, map[string]any{
		"list":         out,
		"www_root":     s.Cfg.WWWRoot,
		"log_dir":      s.siteLogDir(),
		"vhost_dir":    vhostDir,
		"brew_prefix":  s.Cfg.BrewPrefix,
		"presets":      sites.RewritePresets,
		"php_versions": s.detectPHPVersions(r.Context()),
		// 手工编辑配置文件的入口清单（宝塔式的基本操作，见 api_config_files.go）。
		// 路径由后端给：brew 前缀在 Apple Silicon / Intel 不同，前端不拼字符串。
		"config_files": s.panelConfigFiles(r.Context()),
	})
}

// detectPHPVersions 探测本机已安装的 PHP 版本，并补齐运行时状态。
//
// 已安装版本的发现逻辑在 sites.DiscoverPHPVersions（按 Homebrew 实际安装情况推导，
// 不写死列表）。这里只做两件本层才做得了的事：
//
//  1. 探测端点是否真的在监听（界面上"运行中/未运行"必须是真的）；
//  2. 用一次真实请求探测端点背后实际运行的版本（X-Powered-By）。
//
// 为什么不再按 formula 名逐个列出、也不再按端点去重：
// 旧实现里每个版本都被解析成 127.0.0.1:9000，于是 php@8.3 与 php@8.4 会在
// 去重后**只剩一个**，用户根本看不到第二个版本、更谈不上按站点选它。
// 现在每个版本有唯一端点（sites.PreferredEndpoint），去重不再需要，
// "两个版本共用端点"这种情况反而要**显式暴露**出来（PHPVersion.Conflict）。
func (s *Server) detectPHPVersions(ctx context.Context) []sites.PHPVersion {
	out := sites.DiscoverPHPVersions(s.Cfg.BrewPrefix)
	for i := range out {
		if out[i].Pass != "" {
			out[i].Running = sites.EndpointLive(out[i].Pass)
		}
		// "默认版本"标记：配置里 PHPSvc 指向的就是默认（例如 php@8.3）。
		//
		// 注意不能只在 out[i].IsDefault 为 true 时才判断 —— DiscoverPHPVersions
		// 是按版本号去重的，`php` 别名与 `php@8.4` 会合并成一条，
		// 保留下来的那条未必叫 php，光看名字会漏掉真正的默认版本。
		out[i].IsDefault = s.Cfg.PHPSvc == out[i].Service ||
			s.Cfg.PHPSvc == "php@"+out[i].Version ||
			(s.Cfg.PHPSvc == "php" && out[i].Service == "php")
	}
	s.probePHPVersions(ctx, out)
	return out
}

// probePHPVersions 通过一次真实请求探测当前 FPM 实际运行的版本。
//
// 让"配置里写的版本"与"实际处理的版本"不一致时一眼可见。
func (s *Server) probePHPVersions(ctx context.Context, list []sites.PHPVersion) {
	if len(list) == 0 {
		return
	}
	probeDir := filepath.Join(s.Cfg.WWWRoot, "_default")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return
	}
	probeFile := filepath.Join(probeDir, "__zp_ver.php")
	if err := os.WriteFile(probeFile, []byte("<?php echo PHP_VERSION;"), 0o644); err != nil {
		return
	}
	defer func() { _ = os.Remove(probeFile) }()

	// 用 -D - 把响应头打到 stdout，从中读 X-Powered-By。
	//
	// Host 必须是 **127.0.0.1**（不是 localhost）：面板写的默认站点是
	// `server_name _;` + `listen 80 default_server`，而 Homebrew 自带的默认站点是
	// `server_name localhost;`。用 Host: localhost 会**按名字精确命中 brew 那块**
	// （root 是 Cellar/nginx/html）→ `__zp_ver.php` 404 —— mini 真机的 error_log 里
	// 整整一屏都是这个 404，PHP 版本探测因此永远"未复核"。
	// 用 127.0.0.1 才会落到 default_server（面板的默认站点）上。
	rctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := execCommand(rctx, "/usr/bin/curl", "-sS", "-D", "-", "-o", "/dev/null",
		"--max-time", "5", "-H", "Host: 127.0.0.1",
		"http://127.0.0.1/__zp_ver.php").Output()
	if err != nil {
		return
	}
	actual := parsePoweredBy(string(out))
	if actual == "" {
		return
	}
	for i := range list {
		if sameMajorMinor(list[i].Version, actual) {
			list[i].ActualVersion = actual
		}
	}
}

// parsePoweredBy 从响应头里提取 X-Powered-By 的版本号。
func parsePoweredBy(headers string) string {
	for _, ln := range strings.Split(headers, "\n") {
		ln = strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		lower := strings.ToLower(ln)
		v, ok := strings.CutPrefix(lower, "x-powered-by:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if rest, ok := strings.CutPrefix(v, "php/"); ok {
			return strings.TrimSpace(rest)
		}
		return v
	}
	return ""
}

// majorMinor 从 "8.4.7" 提取 "8.4"。
func majorMinor(v string) string {
	v = strings.TrimSpace(v)
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

func sameMajorMinor(a, b string) bool {
	return majorMinor(a) == majorMinor(b)
}

type siteCreateReq struct {
	Domain     string `json:"domain"`
	Aliases    string `json:"aliases"`
	PHPVersion string `json:"php_version"`
	Rewrite    string `json:"rewrite"`
	ProxyPass  string `json:"proxy_pass"`
	Remark     string `json:"remark"`
	// CreateDir 为 false 时不创建目录（站点根目录可能已存在）
	CreateDir *bool `json:"create_dir"`
}

// handleSiteCreate 新建站点。
func (s *Server) handleSiteCreate(w http.ResponseWriter, r *http.Request) {
	var req siteCreateReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	if err := sites.ValidateDomain(req.Domain); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	mgr := s.siteMgr()
	if _, err := mgr.Get(r.Context(), req.Domain); err == nil {
		fail(w, http.StatusConflict, "站点 "+req.Domain+" 已存在")
		return
	}

	root, err := sites.SiteDir(s.Cfg.WWWRoot, req.Domain)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// 伪静态为 Laravel/ThinkPHP 时，运行目录要落到 public 子目录
	runRoot := root
	if p, okk := sites.RewritePresetByName(req.Rewrite); okk && p.PublicDir != "" {
		runRoot = filepath.Join(root, p.PublicDir)
	}

	site := &sites.Site{
		Domain:     req.Domain,
		Aliases:    strings.TrimSpace(req.Aliases),
		Root:       runRoot,
		PHPVersion: req.PHPVersion,
		Rewrite:    req.Rewrite,
		ProxyPass:  strings.TrimSpace(req.ProxyPass),
		Remark:     strings.TrimSpace(req.Remark),
		Enabled:    true,
	}

	createDir := true
	if req.CreateDir != nil {
		createDir = *req.CreateDir
	}
	if createDir {
		if err := s.ensureSiteRoot(runRoot); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Laravel/ThinkPHP 的 public 目录需要存在，否则 nginx 会 404
		if runRoot != root {
			if err := s.ensureSiteRoot(root); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if err := s.seedIndexPHP(runRoot, site); err != nil {
			s.Log.Warn("写入默认首页失败: %v", err)
		}
	}

	// 先落数据库（拿到 ID），再写 nginx 配置；配置失败则回滚数据库
	if err := mgr.Create(r.Context(), site); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.applySite(r.Context(), site); err != nil {
		// 回滚：删掉刚建的记录，避免出现"数据库有、nginx 没有"的不一致状态。
		// 本次写入的 vhost 已由 applySite 自己撤销（见 rollbackVhostWrite）。
		// 回滚失败要**如实说出来**：否则用户重试会被"站点已存在"挡住却不知道为什么。
		if derr := mgr.Delete(r.Context(), site.Domain); derr != nil {
			msg := "创建站点失败: " + err.Error() +
				"（另外：回滚站点记录失败：" + derr.Error() +
				"，请到「网站管理」手动删除 " + site.Domain + " 后重试）"
			s.audit(r, "site_create", site.Domain, msg, false, "")
			fail(w, http.StatusInternalServerError, msg)
			return
		}
		s.audit(r, "site_create", site.Domain, "创建失败（已回滚，可直接重试）: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "创建站点失败: "+err.Error()+"（已回滚本次写入，可直接重试）")
		return
	}

	s.audit(r, "site_create", site.Domain,
		fmt.Sprintf("根目录=%s PHP=%s 伪静态=%s", runRoot, req.PHPVersion, req.Rewrite), true, "")
	ok(w, map[string]any{"site": site, "root": runRoot})
}

// seedIndexPHP 为新站点写入一个初始首页，方便建完立刻能看到效果。
func (s *Server) seedIndexPHP(root string, site *sites.Site) error {
	indexPath := filepath.Join(root, "index.php")
	if !strings.HasSuffix(site.Root, "public") && site.PHPVersion == "" {
		// 纯静态站点写 index.html
		indexPath = filepath.Join(root, "index.html")
	}
	if _, err := os.Stat(indexPath); err == nil {
		return nil // 已有首页，不动它
	}
	var content string
	if strings.HasSuffix(indexPath, ".php") {
		content = fmt.Sprintf(`<?php
// 站点 %s 的默认首页，由 ZizPanel 创建。
// 部署你的程序时可以直接删除本文件，或覆盖整个目录。
$ip = $_SERVER['SERVER_ADDR'] ?? 'localhost';
echo "<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">";
echo "<title>%s</title><style>body{font-family:-apple-system,'PingFang SC',sans-serif;";
echo "display:grid;place-items:center;height:100vh;margin:0;background:#0f1116;color:#e6e9f0}";
echo ".c{text-align:center}h1{margin:0 0 10px;font-size:22px}";
echo "p{color:#9aa3b5;margin:4px 0;font-size:14px}code{background:#1b1f2a;padding:2px 6px;border-radius:4px}</style>";
echo "</head><body><div class=\"c\"><h1>%s</h1>";
echo "<p>站点已创建成功，PHP 运行正常</p>";
echo "<p>PHP 版本：<code>" . PHP_VERSION . "</code></p>";
echo "<p>站点目录：<code>%s</code></p>";
echo "<p>现在可以把程序文件放到该目录，或直接删除本文件。</p>";
echo "</div></body></html>";
`, site.Domain, site.Domain, site.Domain, root)
	} else {
		content = fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>%s</title>
<style>
  body { font-family: -apple-system, "PingFang SC", sans-serif; display: grid;
         place-items: center; height: 100vh; margin: 0; background: #0f1116; color: #e6e9f0; }
  .c { text-align: center; }
  h1 { margin: 0 0 10px; font-size: 22px; }
  p { color: #9aa3b5; margin: 4px 0; font-size: 14px; }
  code { background: #1b1f2a; padding: 2px 6px; border-radius: 4px; }
</style>
</head>
<body>
  <div class="c">
    <h1>%s</h1>
    <p>站点已创建成功</p>
    <p>站点目录：<code>%s</code></p>
    <p>现在可以把网站文件放到该目录，或直接删除本文件。</p>
  </div>
</body>
</html>
`, site.Domain, site.Domain, root)
	}
	if err := os.WriteFile(indexPath, []byte(content), 0o644); err != nil {
		return err
	}
	if s.Cfg.User != "" && s.Cfg.User != "root" {
		if uid, gid, err := lookupIDs(s.Cfg.User); err == nil {
			_ = os.Chown(indexPath, uid, gid)
		}
	}
	return nil
}

// handleSiteGet 返回单个站点的完整信息（含配置内容）。
func (s *Server) handleSiteGet(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	confPath := filepath.Join(s.Cfg.VhostDir, domain+".conf")
	conf, _ := os.ReadFile(confPath)
	// 这里用 ResolveConfigEndpoint 而不是 ResolveEndpoint：
	// 详情页即使 FPM 没在跑也要能显示"配置里写的是哪个端点"，
	// 并把错误原文交给界面展示（而不是只给一个空白）。
	pass := ""
	passErr := ""
	if site.PHPVersion != "" {
		var rerr error
		pass, rerr = sites.ResolveConfigEndpoint(s.Cfg.BrewPrefix, site.PHPVersion)
		passErr = errString(rerr)
	}
	generated, genErr := site.Generate(sites.Options{
		LogDir: s.siteLogDir(), FastCGIPass: pass,
		ClientMaxBodySize: s.uploadLimits().ClientMaxBodySize,
	})
	ok(w, map[string]any{
		"site":         site,
		"conf":         string(conf),
		"conf_path":    confPath,
		"generated":    generated,
		"generate_err": errString(genErr),
		"fastcgi_pass": pass,
		"fastcgi_err":  passErr,
		"log_dir":      s.siteLogDir(),
		"access_log":   filepath.Join(s.siteLogDir(), domain+".access.log"),
		"error_log":    filepath.Join(s.siteLogDir(), domain+".error.log"),
		"presets":      sites.RewritePresets,
		"php_versions": s.detectPHPVersions(r.Context()),
		// 证书来源/到期/剩余天数的完整字段（前端据此显示"哪来的、还剩几天"）。
		"ssl": s.siteSSLView(site),
	})
}

// siteSSLView 汇总站点证书的展示字段。
//
// 到期时间优先取自**真实证书文件**（tlsx.CertExpiry），而不是数据库里的
// ssl_expires 字符串：文件才是 nginx 实际加载的东西；数据库字段可能是
// 上一次写入时的快照（例如 acme 续期后还没重新保存站点记录）。
// 读不到文件时退回数据库字段，days_left 用 -1 表示"无法判断"，不猜。
func (s *Server) siteSSLView(site *sites.Site) map[string]any {
	v := map[string]any{
		"enabled":        site.SSLEnabled,
		"provider":       site.SSLProvider,
		"provider_label": sslProviderLabel(site.SSLProvider),
		"cert_path":      site.SSLCert,
		"key_path":       site.SSLKey,
		"expires":        site.SSLExpires,
		"not_after":      "",
		"days_left":      -1,
		"renew_hint":     "",
	}
	if !site.SSLEnabled || site.SSLCert == "" {
		return v
	}
	// 用 tlsx（纯 Go）直接读证书文件：文件才是 nginx 实际加载的东西，
	// 而数据库里的 ssl_expires 只是上一次写入时的快照（acme 续期后可能还没更新）。
	if notAfter, err := tlsx.CertExpiry(site.SSLCert); err == nil && !notAfter.IsZero() {
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
	// 读不到文件（被删了/权限不对）—— 如实标出来，不要显示成"正常"。
	v["renew_hint"] = "无法读取证书文件（可能已被删除或权限不足）"
	return v
}

// handleSiteConfSave 直接保存站点 vhost（「配置」页手工编辑后点保存）。
//
// 与 handleSiteUpdate 的分工：后者从**面板的站点模型**重新生成配置；这里保存的是
// 用户手写的整份文件 —— nginx 真正加载的就是磁盘上这一份。用户明确要求
// （2026-09-17）："站点管理中应该能直接编辑配置文件，我要改默认端口"。
//
// 安全网一条都不能少（每一层都对应一个真机踩过的坑）：
//  1. helper 写入前跑 `nginx -t`，不通过就不落盘（见 writeVhost）；
//  2. reload 失败 → 回滚成写入前的内容；
//  3. reload 成功但**新配置里的 listen 端口一个都不应答** → 同样回滚 ——
//     nginx 读配置失败（日志/证书打不开）时 `nginx -s reload` 的退出码依然是 0，
//     "命令返回 0"不等于"配置生效"。
//
// 不复用 verifySiteServed：它探的是**站点模型里**的 80/443 并要求 403，
// 而"把站点统一收到 8889"正是这个页面的主要用途，那样会把正确的手工改动误判成失败。
func (s *Server) handleSiteConfSave(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	ctx := r.Context()
	if _, err := s.siteMgr().Get(ctx, domain); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		fail(w, http.StatusBadRequest, "配置内容不能为空（要停用站点请改 enabled 开关或删除站点）")
		return
	}
	// 保存前先拦"同端口 + 同 server_name"的冲突：nginx 只加载其中一份、另一份静默失效，
	// 用户会看到"站点指向了别的站"（真机事故见 checkVhostCollisionWithFiles 的注释）。
	if hit, err := checkVhostCollisionWithFiles(s.Cfg.VhostDir, req.Content, domain+".conf"); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	} else if hit.FileName != "" {
		fail(w, http.StatusConflict, fmt.Sprintf(
			"配置里的 %s 与 %s 冲突：两者都在监听 %d 且 server_name=%s。"+
				"nginx 只会加载其中一份（被忽略的那份会**静默失效**，站点会指向别的站）。\n"+
				"请改端口或域名；如果 %s 是反代规则，也可以先在「反向代理」里停用/删除它。",
			domain+".conf", hit.FileName, hit.Port, hit.Name, hit.FileName))
		return
	}
	snap := s.snapshotVhost(domain)
	if err := siteWriteVhostFn(s, ctx, domain, req.Content); err != nil {
		// helper 自己已经做了 nginx -t 且没有落盘（失败即回滚文件），
		// 所以这里不需要再回滚一次 —— 只把 nginx 的原文交回界面。
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 与 applySite 同一处理：nginx -t 会以 root 创建日志文件，
	// 而 nginx master 以真实用户运行，属主不对会 reload 失败（且退出码仍是 0）。
	if s.Cfg.User != "" && os.Geteuid() == 0 {
		_ = chownTreeTo(s.siteLogDir(), s.Cfg.User)
	}
	if err := siteReloadFn(s, ctx); err != nil {
		err = s.rollbackVhostWrite(ctx, snap, siteWriteVhostFn, siteReloadFn,
			fmt.Errorf("配置已写入但 nginx 重载失败: %w", err))
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ports := siteListenPorts(req.Content)
	if len(ports) > 0 {
		alive := false
		var lastErr error
		deadline := time.Now().Add(siteVerifyWait)
		for !alive && time.Now().Before(deadline) {
			for _, p := range ports {
				scheme := "http"
				if p.SSL {
					scheme = "https"
				}
				if _, _, perr := siteProbeFn(ctx, scheme, domain, p.Port, siteVhostProbePath, siteProbeTimeout); perr == nil {
					alive = true
					break
				} else {
					lastErr = perr
				}
			}
			if alive {
				break
			}
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
			case <-time.After(siteVerifyEvery):
				continue
			}
			break
		}
		if !alive {
			names := make([]string, 0, len(ports))
			for _, p := range ports {
				names = append(names, strconv.Itoa(p.Port))
			}
			err := s.rollbackVhostWrite(ctx, snap, siteWriteVhostFn, siteReloadFn,
				fmt.Errorf("配置已写入并重载，但新配置里的监听端口（%s）没有任何一个能应答：%v。"+
					"已回滚成修改前的内容", strings.Join(names, "、"), lastErr))
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	ok(w, map[string]any{"content": req.Content, "conf_path": filepath.Join(s.Cfg.VhostDir, domain+".conf")})
}

// siteListenPort 是一条 listen 指令解析出来的结果。
type siteListenPort struct {
	Port int
	SSL  bool
}

// isNginxTokenBoundary 判断某个下标是不是 nginx 指令词的边界（前一个字符是分隔符）。
//
// 用途：按"指令词 + 到分号/花括号"扫描时，避免匹配到 `ssl_listen` 这类以指令词
// 结尾的标识符。抽成函数是为了 server_name / listen 两处用同一个判据。
func isNginxTokenBoundary(s string, i int) bool {
	if i <= 0 {
		return true
	}
	switch s[i-1] {
	case ' ', '\t', '\n', '\r', ';', '{', '}':
		return true
	}
	return false
}

// siteServerBlocks 返回内容里每一个 `server { … }` 块的原文（花括号配对）。
//
// 为什么按块而不是全文件扫描：同一份 vhost 里通常有 80 与 443 两个 server 块，
// 它们各自有自己的 server_name。把"全文件的端口 × 全文件的名字"做笛卡尔积会
// 造出根本不存在的组合（a.com:443），进而产生**误报**式的冲突拦截。
func siteServerBlocks(content string) []string {
	var out []string
	for i := 0; i+len("server") <= len(content); i++ {
		if content[i:i+len("server")] != "server" || !isNginxTokenBoundary(content, i) {
			continue
		}
		j := i + len("server")
		for j < len(content) && (content[j] == ' ' || content[j] == '\t' || content[j] == '\n' || content[j] == '\r') {
			j++
		}
		if j >= len(content) || content[j] != '{' {
			continue
		}
		depth, end := 0, -1
		for k := j; k < len(content); k++ {
			switch content[k] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = k
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		out = append(out, content[i:end+1])
		i = end
	}
	return out
}

// siteServerNames 抽出内容里全部 server_name（跳过 `_` / 正则 / 变量）。
func siteServerNames(content string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i+len("server_name") <= len(content); i++ {
		if content[i:i+len("server_name")] != "server_name" || !isNginxTokenBoundary(content, i) {
			continue
		}
		rest := content[i+len("server_name"):]
		semi := strings.IndexByte(rest, ';')
		if semi < 0 {
			break
		}
		for _, f := range strings.Fields(rest[:semi]) {
			if f == "" || f == "_" || strings.HasPrefix(f, "~") || strings.HasPrefix(f, "$") || seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
		i += len("server_name") + semi
	}
	return out
}

// vhostIdentity 是一份 vhost 里"nginx 用来选虚拟主机"的一条身份：端口 + server_name。
type vhostIdentity struct {
	Port     int
	Name     string
	FileName string
}

// vhostIdentities 逐 server 块抽出 (端口, server_name) 组合。
func vhostIdentities(content, fileName string) []vhostIdentity {
	var out []vhostIdentity
	for _, block := range siteServerBlocks(content) {
		ports := siteListenPorts(block)
		if len(ports) == 0 {
			continue
		}
		for _, name := range siteServerNames(block) {
			for _, p := range ports {
				out = append(out, vhostIdentity{Port: p.Port, Name: name, FileName: fileName})
			}
		}
	}
	return out
}

// collideVhosts 在两组身份里找"同端口 + 同 server_name"的冲突，返回第一条。
func collideVhosts(mine, others []vhostIdentity) (vhostIdentity, bool) {
	for _, m := range mine {
		for _, o := range others {
			if m.Port == o.Port && m.Name == o.Name {
				return vhostIdentity{Port: m.Port, Name: m.Name, FileName: o.FileName}, true
			}
		}
	}
	return vhostIdentity{}, false
}

// checkVhostCollisionWithFiles 检查"这份配置的 (端口, server_name)"是否与 vhosts 目录里
// **别的** .conf 重复（selfFile 是要保存/更新的那份，跳过它自己）。
//
// 为什么必须拦：同一端口上重复的 server_name，nginx 只会加载其中一份（按 include
// 顺序），另一份**静默失效**，`nginx -t` 只给一行 warning。真机事故（2026-09-17）：
// 用户把 wp.zizdog.com 的 vhost 存成 `listen 8889 ssl` + 同名，撞上反代规则 9
// （也是 8889 + 同名）；proxy-9.conf 字典序在前，于是站点那份被忽略、443 上又没有
// wp 的 server 块 → 8889 的流量落到了 blog.zizdog.com，用户看到"我的站变成了别的站"。
func checkVhostCollisionWithFiles(vhostDir, content, selfFile string) (vhostIdentity, error) {
	mine := vhostIdentities(content, selfFile)
	if len(mine) == 0 {
		return vhostIdentity{}, nil
	}
	entries, err := os.ReadDir(vhostDir)
	if err != nil {
		// 读不到目录就不阻断保存（保存本身还会被 nginx -t 兜住）。
		return vhostIdentity{}, nil
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == selfFile || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(vhostDir, e.Name()))
		if err != nil {
			continue
		}
		if hit, ok := collideVhosts(mine, vhostIdentities(string(b), e.Name())); ok {
			return hit, nil
		}
	}
	return vhostIdentity{}, nil
}

// siteListenPorts 从 vhost 内容里提取全部 listen 端口（用于"改完真的在服务吗"的复核）。
//
// 按"listen 这个单词 + 到下一个分号"扫描，而不是按行：nginx 的写法有
// `listen 80;` / `listen 443 ssl;` / `listen 127.0.0.1:8889;` / `listen [::]:80;`，
// 也可能写在同一行的花括号里（`server { listen 8892; }`）—— 只认"行首 listen"
// 会漏掉整类写法，而漏掉的后果是**跳过复核**，正是"报告成功但其实没生效"。
// 前置字符必须是分隔符，避免匹配到 `ssl_listen` 这类以 listen 结尾的标识符。
// 解析不出端口的（例如 `listen unix:/tmp/x.sock;`）直接跳过 —— 那些不该被探。
func siteListenPorts(content string) []siteListenPort {
	var out []siteListenPort
	seen := map[int]bool{}
	for i := 0; i+len("listen") <= len(content); i++ {
		if content[i:i+len("listen")] != "listen" {
			continue
		}
		if i > 0 {
			switch content[i-1] {
			case ' ', '\t', '\n', '\r', ';', '{', '}':
			default:
				continue
			}
		}
		rest := content[i+len("listen"):]
		semi := strings.IndexByte(rest, ';')
		if semi < 0 {
			break
		}
		fields := strings.Fields(rest[:semi])
		if len(fields) == 0 {
			continue
		}
		addr := fields[0]
		if k := strings.LastIndex(addr, ":"); k >= 0 {
			addr = addr[k+1:]
		}
		port, err := strconv.Atoi(addr)
		if err != nil || port <= 0 || port > 65535 || seen[port] {
			continue
		}
		seen[port] = true
		ssl := false
		for _, f := range fields[1:] {
			if f == "ssl" {
				ssl = true
			}
		}
		out = append(out, siteListenPort{Port: port, SSL: ssl})
	}
	return out
}

type siteUpdateReq struct {
	Aliases    *string `json:"aliases"`
	PHPVersion *string `json:"php_version"`
	Rewrite    *string `json:"rewrite"`
	ProxyPass  *string `json:"proxy_pass"`
	ExtraConf  *string `json:"extra_conf"`
	Remark     *string `json:"remark"`
	Enabled    *bool   `json:"enabled"`
}

// handleSiteUpdate 更新站点并重新生成配置。
func (s *Server) handleSiteUpdate(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	var req siteUpdateReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	before := *site // 用于失败回滚
	if req.Aliases != nil {
		site.Aliases = strings.TrimSpace(*req.Aliases)
	}
	if req.PHPVersion != nil {
		site.PHPVersion = strings.TrimSpace(*req.PHPVersion)
	}
	if req.Rewrite != nil {
		site.Rewrite = strings.TrimSpace(*req.Rewrite)
	}
	if req.ProxyPass != nil {
		site.ProxyPass = strings.TrimSpace(*req.ProxyPass)
	}
	if req.ExtraConf != nil {
		site.ExtraConf = *req.ExtraConf
	}
	if req.Remark != nil {
		site.Remark = strings.TrimSpace(*req.Remark)
	}
	if req.Enabled != nil {
		site.Enabled = *req.Enabled
	}

	// 伪静态切到 Laravel/ThinkPHP 时，运行目录要跟着切到 public；
	// 切回来则要退回站点根目录。这里按"域名目录 + 模板要求"重新推导，
	// 避免用户改模板后目录还停在 public 导致 404。
	baseDir, err := sites.SiteDir(s.Cfg.WWWRoot, site.Domain)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if p, okk := sites.RewritePresetByName(site.Rewrite); okk && p.PublicDir != "" {
		site.Root = filepath.Join(baseDir, p.PublicDir)
	} else if strings.HasPrefix(site.Root, baseDir) {
		site.Root = baseDir
	}

	if err := mgr.Update(r.Context(), site); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if site.Enabled {
		if err := s.applySite(r.Context(), site); err != nil {
			// 回滚数据库，保证"数据库状态 = nginx 实际状态"
			_ = mgr.Update(r.Context(), &before)
			_ = s.applySite(r.Context(), &before)
			s.audit(r, "site_update", domain, "更新失败: "+err.Error(), false, "")
			fail(w, http.StatusInternalServerError, "更新失败: "+err.Error())
			return
		}
	} else {
		// 停用：从 nginx 移除配置并重载
		if _, err := s.callHelper(r.Context(), "vhost-delete", domain); err != nil {
			s.Log.Warn("删除 vhost 失败: %v", err)
		}
		if err := s.nginxReload(r.Context()); err != nil {
			s.Log.Warn("重载 nginx 失败: %v", err)
		}
	}

	s.audit(r, "site_update", domain, "更新站点配置", true, "")
	ok(w, map[string]any{"site": site})
}

// handleSiteDelete 删除站点。
func (s *Server) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	removeFiles := r.URL.Query().Get("remove_files") == "1"

	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}

	if _, err := s.callHelper(r.Context(), "vhost-delete", domain); err != nil {
		s.Log.Warn("删除 vhost 失败: %v", err)
	}
	if err := mgr.Delete(r.Context(), domain); err != nil {
		fail(w, http.StatusInternalServerError, "删除站点记录失败: "+err.Error())
		return
	}
	if err := s.nginxReload(r.Context()); err != nil {
		s.Log.Warn("重载 nginx 失败: %v", err)
	}

	var filesMsg string
	if removeFiles {
		// 只允许删除站点根目录（严格限定在 www 根下，且必须是该域名的目录）
		base := filepath.Join(s.Cfg.WWWRoot, domain)
		cleanBase := filepath.Clean(base)
		if filepath.Dir(cleanBase) != filepath.Clean(s.Cfg.WWWRoot) {
			filesMsg = "目录路径校验失败，未删除文件"
		} else if err := os.RemoveAll(cleanBase); err != nil {
			filesMsg = "删除文件失败: " + err.Error()
		} else {
			filesMsg = "已删除站点目录"
		}
	}

	// 审计里记录被删站点的关键信息，便于事后追溯"删掉了什么"
	detail := fmt.Sprintf("删除站点（根目录=%s，SSL=%v，PHP=%s）%s",
		site.Root, site.SSLEnabled, site.PHPVersion, filesMsg)
	s.audit(r, "site_delete", domain, detail, true, "")
	ok(w, map[string]any{"msg": "站点已删除", "files": filesMsg})
}

// ---------- SSL ----------

type siteSSLReq struct {
	Provider string   `json:"provider"` // self / mkcert / manual / acme
	Cert     string   `json:"cert"`
	Key      string   `json:"key"`
	ExtraSAN []string `json:"extra_san"`
	Enable   bool     `json:"enable"`

	// CertPrimary 指定要绑定的 ACME 证书（primary 名）。
	// 不给时按站点域名/别名自动匹配（见 matchCertForSite）。
	CertPrimary string `json:"cert_primary"`
	// Domain 是 cert_primary 的容错写法：允许前端直接给一个域名，
	// 由面板去证书库里找覆盖它的那一张。
	Domain string `json:"domain"`
}

// handleSiteSSL 为站点签发/配置证书。支持四种来源：
//   - self   ：openssl 自签（无需任何外部依赖，浏览器会提示不受信任）
//   - mkcert ：使用 mkcert 的本地 CA（在已信任 mkcert CA 的机器上无提示）
//   - manual ：用户粘贴证书与私钥
//   - acme   ：直接引用 internal/acme 已签发的证书（Let's Encrypt 等）
//
// acme 来源**不在这里签发**：签发要等 CA 完成 DNS/HTTP 校验，是几十秒到
// 几分钟的长任务，必须走任务中心（POST /api/v1/certs）。这里只按域名匹配
// 已有证书，匹配不到就明确提示先去证书页申请 —— 同步接口挂几分钟既违反
// "长任务必须走任务中心"的约定，用户也看不到任何进度。
//
// 关键点：站点写回的是 acme 引擎自己的路径 `<DataDir>/certs/<primary>/...`，
// **不是复制一份到 site-certs**。因为续期是原地覆盖同一份文件，
// 站点 vhost 里的 ssl_certificate 一个字都不用改就能用上新证书。
func (s *Server) handleSiteSSL(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	var req siteSSLReq
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
		// 这三个来源都把证书复制/生成到站点自己的目录里（与 acme 分开存放，
		// 避免"面板的证书库"和"站点私有证书"混在一起）。
		certDir := filepath.Join(s.Cfg.DataDir, "site-certs", domain)
		if err := os.MkdirAll(certDir, 0o755); err != nil {
			fail(w, http.StatusInternalServerError, "创建证书目录失败: "+err.Error())
			return
		}
		certPath = filepath.Join(certDir, "fullchain.pem")
		keyPath = filepath.Join(certDir, "privkey.pem")
		switch req.Provider {
		case "self":
			if _, err := s.callHelper(r.Context(), "site-cert-self",
				"--domain", domain, "--cert", certPath, "--key", keyPath); err != nil {
				fail(w, http.StatusInternalServerError, "签发自签证书失败: "+err.Error())
				return
			}
		case "mkcert":
			if _, err := s.callHelper(r.Context(), "mkcert-issue",
				"--hosts", strings.Join(append([]string{domain}, req.ExtraSAN...), ","),
				"--cert", certPath, "--key", keyPath); err != nil {
				fail(w, http.StatusInternalServerError,
					"mkcert 签发失败: "+err.Error()+"（可先执行 brew install mkcert nss && mkcert -install）")
				return
			}
		case "manual":
			if req.Cert == "" || req.Key == "" {
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
		// 读取证书到期时间，便于前端提示续期
		if res, err := s.callHelper(r.Context(), "site-cert-info", certPath); err == nil {
			if data, okk := res["data"].(map[string]any); okk {
				expires, _ = data["expires"].(string)
			}
		}
	case "acme":
		cert, err := s.matchCertForSite(site, req)
		if err != nil {
			// 400 而不是 500：这是"还没申请证书"这种可预期的用户状态。
			fail(w, http.StatusBadRequest, err.Error())
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

	// 记下写入前的站点记录：SSL 绑定失败时必须把 store 恢复原样，
	// 否则会出现"面板说绑了、store 里 ssl_enabled=true，nginx 却没在服务"。
	before := *site

	site.SSLEnabled = true
	site.SSLCert = certPath
	site.SSLKey = keyPath
	site.SSLProvider = req.Provider
	site.SSLExpires = expires

	if err := mgr.Update(r.Context(), site); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// acme 来源同样走 applySite —— 因此自动获得"写 vhost → reload →
	// 403 探针复核"这条完整链路，不会出现"面板说绑定成功、站点其实没生效"。
	if err := s.applySite(r.Context(), site); err != nil {
		// applySite 已撤销本次写入的 vhost；这里把 store 里的 SSL 字段也退回去，
		// 让"接口非 2xx = 这次没做成"在数据库、磁盘、界面三处保持一致。
		rbMsg := ""
		if uerr := mgr.Update(r.Context(), &before); uerr != nil {
			rbMsg = "（另外：回滚 SSL 字段失败：" + uerr.Error() +
				"，请到「网站管理 → " + domain + " → SSL」确认状态后重试）"
		}
		s.audit(r, "site_ssl", domain, "绑定失败: "+err.Error()+rbMsg, false, "")
		fail(w, http.StatusInternalServerError,
			"证书已签发但应用配置失败（已回滚，可直接重试）: "+err.Error()+rbMsg)
		return
	}

	s.audit(r, "site_ssl", domain, "绑定证书 provider="+req.Provider+" 到期="+expires, true, "")
	ok(w, map[string]any{
		"site": site, "cert": certPath, "key": keyPath, "expires": expires,
		// 前端可直接用这些字段显示"哪来的/还剩几天"，不用自己去解析证书。
		"provider_label": sslProviderLabel(req.Provider),
		"days_left":      siteSSLDaysLeft(certPath),
	})
}

// handleSiteSSLDisable 关闭 SSL。
func (s *Server) handleSiteSSLDisable(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	before := *site
	site.SSLEnabled = false
	site.SSLCert = ""
	site.SSLKey = ""
	site.SSLProvider = ""
	site.SSLExpires = ""
	if err := mgr.Update(r.Context(), site); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.applySite(r.Context(), site); err != nil {
		// 同上：vhost 由 applySite 撤销，store 字段在这里退回"仍然启用 SSL"。
		rbMsg := ""
		if uerr := mgr.Update(r.Context(), &before); uerr != nil {
			rbMsg = "（另外：回滚 SSL 字段失败：" + uerr.Error() + "）"
		}
		s.audit(r, "site_ssl_disable", domain, "关闭失败: "+err.Error()+rbMsg, false, "")
		fail(w, http.StatusInternalServerError,
			"关闭 SSL 应用配置失败（已回滚，SSL 仍然启用）: "+err.Error()+rbMsg)
		return
	}
	s.audit(r, "site_ssl_disable", domain, "关闭 SSL", true, "")
	ok(w, map[string]any{"site": site})
}

// ---------- 校验与诊断 ----------

// handleSiteCheck 用真实 HTTP 请求检查站点是否可访问，并检测 PHP 是否被当静态文件吐出。
func (s *Server) handleSiteCheck(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	mgr := s.siteMgr()
	site, err := mgr.Get(r.Context(), domain)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result := checkSite(ctx, site, s.siteLogDir())
	ok(w, result)
}

// handleNginxTest 校验整个 nginx 配置。
func (s *Server) handleNginxTest(w http.ResponseWriter, r *http.Request) {
	res, err := s.callHelper(r.Context(), "nginx-test")
	if err != nil {
		// ⚠️ 失败时必须把**助手的原始输出**（`nginx -t` 的正文，含文件名与行号）
		// 带给界面。2026-09-18 用户报障："配置有问题：nginx 配置检查未通过" ——
		// 只有这一句，**看不出哪一行错**，用户只能干瞪眼。
		// 助手在 ok:false 时把正文放在 msg、错误摘要放在 error，这里两个都要给。
		detail := err.Error()
		if m, _ := res["msg"].(string); strings.TrimSpace(m) != "" {
			detail = strings.TrimSpace(m) + "\n\n" + detail
		}
		ok(w, map[string]any{"ok": false, "output": detail})
		return
	}
	msg, _ := res["msg"].(string)
	ok(w, map[string]any{"ok": true, "output": msg})
}

// handleNginxStatus 返回 nginx 运行状态与站点数量统计。
func (s *Server) handleNginxStatus(w http.ResponseWriter, r *http.Request) {
	status := "stopped"
	if res, err := s.callHelper(r.Context(), "nginx-status"); err == nil {
		if m, okk := res["msg"].(string); okk {
			status = m
		}
	}
	mgr := s.siteMgr()
	list, _ := mgr.List(r.Context())
	enabled := 0
	for _, st := range list {
		if st.Enabled {
			enabled++
		}
	}
	ok(w, map[string]any{"status": status, "total": len(list), "enabled": enabled})
}

// handleSiteReload 重新生成所有站点配置并重载 nginx。
//
// 用途：配置文件被手工改坏、或升级面板后需要按最新模板重建。
func (s *Server) handleSiteReload(w http.ResponseWriter, r *http.Request) {
	mgr := s.siteMgr()
	list, err := mgr.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var rebuilt, failed []string
	for _, site := range list {
		if !site.Enabled {
			// 停用的站点应确保配置不存在
			_, _ = s.callHelper(r.Context(), "vhost-delete", site.Domain)
			continue
		}
		if err := s.applySite(r.Context(), site); err != nil {
			failed = append(failed, site.Domain+": "+err.Error())
			continue
		}
		rebuilt = append(rebuilt, site.Domain)
	}
	if err := s.nginxReload(r.Context()); err != nil {
		failed = append(failed, "reload: "+err.Error())
	}
	s.audit(r, "site_reload", "all",
		fmt.Sprintf("重建 %d 个站点，失败 %d 个", len(rebuilt), len(failed)), len(failed) == 0, "")
	ok(w, map[string]any{"rebuilt": rebuilt, "failed": failed})
}

// handleNginxEnvRepair 修复 nginx 环境（WebSocket map 与 conf.d include）。
func (s *Server) handleNginxEnvRepair(w http.ResponseWriter, r *http.Request) {
	res, err := s.callHelper(r.Context(), "nginx-ensure-env")
	if err != nil {
		fail(w, http.StatusInternalServerError, "修复失败: "+err.Error())
		return
	}
	msg, _ := res["msg"].(string)
	// 修复后重载，让变更生效
	reloadErr := s.nginxReload(r.Context())
	s.audit(r, "nginx_env_repair", "nginx", msg, reloadErr == nil, "")
	ok(w, map[string]any{"msg": msg, "reload_ok": reloadErr == nil})
}

// ---------- 站点日志 ----------

// handleSiteLog 返回站点访问/错误日志内容。
func (s *Server) handleSiteLog(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	kind := r.URL.Query().Get("kind")
	if kind != "error" {
		kind = "access"
	}
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			lines = n
		}
	}
	path := filepath.Join(s.siteLogDir(), fmt.Sprintf("%s.%s.log", domain, kind))
	if filepath.Base(domain) != domain {
		fail(w, http.StatusBadRequest, "非法域名")
		return
	}
	content, total, err := tailFile(path, lines)
	if err != nil {
		if os.IsNotExist(err) {
			ok(w, map[string]any{
				"content": "", "path": path, "lines": 0,
				"msg": "日志文件尚不存在（站点可能还没有被访问过）",
			})
			return
		}
		fail(w, http.StatusInternalServerError, "读取日志失败: "+err.Error())
		return
	}
	ok(w, map[string]any{"content": content, "path": path, "lines": total})
}

// ---------- 小工具 ----------

// tailFile 读取文件末尾若干行，返回内容与总行数。
func tailFile(path string, lines int) (string, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	all := strings.Split(string(b), "\n")
	// 末尾通常是空行，去掉以免显示出一行空白
	if len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	total := len(all)
	if total > lines {
		all = all[total-lines:]
	}
	return strings.Join(all, "\n"), total, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Shutdown 在面板退出时清理资源。
//
// 必须关掉终端会话：它们持有真实的 shell 子进程，
// 面板退出后这些 shell 如果继续存在，就成了没人管的孤儿进程，
// 而且它们的审计上下文（哪个用户开的）也丢失了。
func (s *Server) Shutdown() {
	if s.termMgr != nil {
		s.termMgr.CloseAll()
	}
	// 关掉所有回环转发器：面板都退出了，还留着监听端口只会让 nginx 把请求
	// 转进一个没有人处理的 socket（表现为挂起而不是干脆的 502）。
	if s.forwarders != nil {
		s.forwarders.StopAll()
	}
}

// Startup 在面板启动时做一次环境准备与自愈。
//
// 做三件事：
//  1. 把 upgrade map 的内容源注入 priv 包（保持单一数据源，避免两处硬编码）
//  2. 确保站点日志目录存在（nginx 打不开日志目录会直接启动失败）
//  3. 确保 nginx 有 WebSocket 升级 map 与 conf.d include
//
// 这些在安装脚本里也会做一遍，但启动时再检查一次能覆盖
// "用户换了 nginx 配置""重装了 nginx""恢复了旧备份"等情况。
func (s *Server) Startup(ctx context.Context) {
	priv.SetUpgradeMapContent(sites.UpgradeMapConf())

	logDir := s.siteLogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		s.Log.Warn("创建站点日志目录失败 %s: %v", logDir, err)
	}

	s.ensureNginxEnvOnStart(ctx)

	// 把回环转发器对齐到数据库里的反代规则（macOS 15 本地网络隐私门的修法）。
	// 必须在任何 nginx reload 之前把监听器起好，否则重启后的第一次请求会打到
	// 一个还没人听的回环端口上。
	s.reconcileForwarders(ctx)

	// 清掉升级暂存目录里的旧发布包（每个约 20~26MB，从不清理会滚到 GB 级 ——
	// 而磁盘满的后果就是"上传/导入大文件直接 500"：nginx 缓冲突请求体需要空间）。
	// 只保留最近 2 个版本；不认识的文件的**一个都不动**。
	go func() {
		if n, freed, err := upgrade.PruneOldPackages(s.Cfg.WorkDir, 2); err != nil {
			s.Log.Warn("清理旧升级包失败: %v", err)
		} else if n > 0 {
			s.Log.Info("已清理 %d 个旧升级包，释放 %.1f MB（保留最近 2 个版本）",
				n, float64(freed)/1024/1024)
		}
	}()

	// 用审计日志回填"用户主动停止"的意图（升级后的老库）：
	// 修复之前 stop/start 不记录意图，于是用户手动停掉的服务会被当成"需要处理"。
	// 只认**面板自己审计里最后一条成功的启停动作**，查不到就一个字都不改。
	if n, err := s.svcManager().ReconcileUserIntentFromAudit(ctx); err != nil {
		s.Log.Warn("回填服务启停意图失败: %v", err)
	} else if n > 0 {
		s.Log.Info("已按审计日志回填 %d 条服务的启停意图（用户手动停止的不再计入「需要处理」）", n)
	}

	// 证书自动续期：登记到面板既有的调度器里（每日检查一次，实现见 api_certs.go）。
	// 这里只登记，不做立即续期 —— 真正决定签发的门槛是 NeedsRenewal，
	// 启动路径上不该引入额外的网络等待。
	s.startCertRenewal(ctx)

	// 安装后自动建一个**纯静态**默认站点（用户 2026-09-21 要求"装完就有"）。
	// 幂等：成功过就只做一次现场复核；没有 nginx 就如实记"等待 nginx"，不报错刷屏。
	// 放在 reconcileForwarders 之后：默认站点 vhost 里写着应用代理的 location，
	// 那些回环端口要先对齐好，否则写出来的 proxy_pass 会指向旧端口。
	s.ensureDefaultSiteOnStart(ctx)

	// 空闲终端会话回收：
	// WebSocket 断开时会关闭会话，但网络异常（客户端崩溃、断网）
	// 可能导致连接一直挂着。这里做兜底清理。
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if s.termMgr != nil {
				if n := s.termMgr.ReapIdle(); n > 0 {
					s.Log.Info("回收空闲终端会话 %d 个", n)
				}
			}
		}
	}()
}

// ensureNginxEnvOnStart 在面板启动时确保 nginx 具备所需环境。
//
// 这一步是"自愈"：即使安装脚本没跑、或用户换了 nginx 配置，
// 面板启动后也会把 WebSocket map 与 conf.d include 补齐，
// 否则反向代理站点会在 nginx -t 阶段直接失败。
func (s *Server) ensureNginxEnvOnStart(ctx context.Context) {
	if os.Geteuid() != 0 {
		// 非 root 时尝试通过助手（sudoers 已授权）
		if _, err := s.callHelper(ctx, "nginx-ensure-env"); err != nil {
			s.Log.Warn("nginx 环境自愈失败（反向代理可能不可用）: %v", err)
		}
		return
	}
	msg, err := priv.EnsureNginxEnv()
	if err != nil {
		s.Log.Warn("nginx 环境自愈失败（反向代理可能不可用）: %v", err)
		return
	}
	// 只有真的改动了 nginx 配置才重载。
	// 每次启动无条件 reload 会在开机时给所有站点造成一次无谓的请求抖动。
	if priv.NginxEnvChanged(msg) {
		if err := s.nginxReload(ctx); err != nil {
			s.Log.Warn("nginx 环境已更新但重载失败: %v", err)
		} else {
			s.Log.Info("nginx 环境已更新并重载: %s", msg)
		}
		return
	}
	s.Log.Info("nginx 环境检查: %s", msg)

	// 全局请求体上限也要自愈：`brew reinstall/upgrade nginx` 会把 nginx.conf
	// **还原成 brew 出厂版** —— 那里面既没有 vhosts include（上面刚补），也没有
	// 面板设的 `client_max_body_size`（出厂的 1m）。真机后果（2026-09-18）：
	// "文件在、服务在、就是不生效"——4MB 的 SQL 导入直接被 nginx 以 1m 拒掉，
	// 用户看到的是"phpMyAdmin 导入失败/卡住"，而面板里怎么看都正常。
	s.ensureGlobalBodySize(ctx)
}

// ensureGlobalBodySize 确保 nginx.conf 的 http 块里有面板配置的全局请求体上限。
//
// 只读判断 → 不一致才写（写之前会备份、写后 nginx -t、失败回滚，见 priv.NginxTuningWrite）。
// 这是幂等的：一致时一次文件读 + 一次比较，不做任何改动、不 reload。
func (s *Server) ensureGlobalBodySize(ctx context.Context) {
	want := strings.TrimSpace(s.Cfg.NginxClientMaxBodySize)
	if want == "" {
		return
	}
	b, err := os.ReadFile(s.Cfg.NginxConf)
	if err != nil {
		return
	}
	cur, _ := sites.FindClientMaxBodySize(string(b))
	if strings.TrimSpace(cur) == want {
		return
	}
	mb, ok := sites.ParseSizeBytes(want)
	if !ok || mb <= 0 {
		return
	}
	mbInt := int(mb / (1024 * 1024))
	if mbInt < 1 {
		mbInt = 1
	}
	tv, err := s.readNginxTuning(ctx)
	if err != nil {
		s.Log.Warn("全局请求体上限自愈失败（读不到 nginx 当前参数）：%v", err)
		return
	}
	tv.ClientMaxBodySizeMB = mbInt
	enc, err := priv.TuningEncode(tv)
	if err != nil {
		return
	}
	if _, werr := s.callHelper(ctx, "nginx-tuning-write", "-values", enc); werr != nil {
		s.Log.Warn("全局请求体上限自愈失败（%s → %s）：%v", cur, want, werr)
		return
	}
	s.Log.Info("已把全局 client_max_body_size 自愈为 %s（原来 %q —— 常见于 brew 重装/升级 nginx 还原了 nginx.conf）",
		want, cur)
}

// checkProxyAgainstSiteVhosts 检查一条反代规则是否与 vhosts 目录里已有文件
// （站点 vhost / 别的规则）在"同端口 + 同 server_name"上冲突。
//
// 与 checkVhostCollisionWithFiles 同一判据、反方向调用：那边是"保存站点配置时"，
// 这边是"新建/修改反代规则时"。两边都要拦，否则谁先谁后决定了哪一份被静默忽略。
func (s *Server) checkProxyAgainstSiteVhosts(rule *proxies.Rule, selfFile string) error {
	if rule == nil || rule.Listen <= 0 {
		return nil
	}
	names := proxies.SplitDomains(rule.Domains)
	if len(names) == 0 {
		return nil
	}
	mine := make([]vhostIdentity, 0, len(names))
	for _, n := range names {
		mine = append(mine, vhostIdentity{Port: rule.Listen, Name: n})
	}
	entries, err := os.ReadDir(s.Cfg.VhostDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == selfFile || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(s.Cfg.VhostDir, e.Name()))
		if rerr != nil {
			continue
		}
		if hit, ok := collideVhosts(mine, vhostIdentities(string(b), e.Name())); ok {
			return fmt.Errorf("与 %s 冲突：两者都在监听 %d 且 server_name=%s。"+
				"nginx 只会加载其中一份（另一份**静默失效**）。请改端口或域名，"+
				"或先在「网站管理」里把那个站点的端口改掉", hit.FileName, hit.Port, hit.Name)
		}
	}
	return nil
}

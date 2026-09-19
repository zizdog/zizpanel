package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	userpkg "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tlsx"
	"github.com/zizdog/zizpanel/internal/upgrade"
)

// 站点管理接口：面板侧校验输入、生成 nginx 配置、维护数据库、编排顺序；
// 助手侧（zizpanel-helper）在白名单路径内原子落盘、语法校验、reload。
// 会改动 nginx 的操作统一走：写配置（helper 校验并允许回滚）→ reload → 失败则还原数据库。

// siteLogDir 返回站点日志目录。约定与现有 LNMP 环境一致（~/www/_logs）。
func (s *Server) siteLogDir() string {
	return filepath.Join(s.Cfg.WWWRoot, "_logs")
}

func (s *Server) siteMgr() *sites.Manager {
	return sites.NewManager(s.Store, sites.Options{LogDir: s.siteLogDir()})
}

func helperCall(ctx context.Context, args ...string) (map[string]any, error) {
	bin := "/opt/zizpanel/bin/zizpanel-helper"
	if v := os.Getenv("ZIZPANEL_WORKDIR"); v != "" {
		bin = filepath.Join(filepath.Dir(v), "bin", "zizpanel-helper")
	}
	return helperCallBin(ctx, bin, args...)
}

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
	// stderr 必须单独带进错误信息（sudoers 不对时原因只写在 stderr 上），
	// 否则用户只看到一句毫无信息量的 "exit status 1 ()"。
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
		// 退出码 0 但输出不是 JSON 不能当成功，否则调用方会以为操作生效了。
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

// 助手必须由 root 调用（sudoers 白名单）或当前进程本身就是 root。
// 实现只有 helperCallBin 一份：曾分叉过（一处修了错误信息、另一处没修），不要再复制。
func (s *Server) callHelper(ctx context.Context, args ...string) (map[string]any, error) {
	return helperCallBin(ctx, s.Cfg.ServicePath("zizpanel-helper"), args...)
}

// writeVhost 把配置写入 nginx vhost 目录（经助手，含语法校验与回滚）。
// 写前先确保 nginx 基础片段已加载：brew 重装/升级会把 nginx.conf 还原成出厂版、
// 没有 conf.d include ⇒ 反代规则过不了 nginx -t（2026-09-22 报障）；报变量未定义时自愈重试一次。
func (s *Server) writeVhost(ctx context.Context, domain, content string) error {
	s.ensureNginxEnvOnStart(ctx)
	err := s.writeVhostOnce(ctx, domain, content)
	if err == nil || !isNginxEnvVarErr(err) {
		return err
	}
	if s.Log != nil {
		s.Log.Warn("写入 %s 时 nginx -t 报变量未定义（面板的 nginx 片段没被加载）——"+
			"自动补齐 conf.d 加载与 WebSocket map 后重试：%v", domain, err)
	}
	if err2 := s.writeVhostOnce(ctx, domain, content); err2 == nil {
		if s.Log != nil {
			s.Log.Info("nginx 环境自愈后重试写入 %s 成功", domain)
		}
		return nil
	}
	return fmt.Errorf("%w；面板已自动补齐 nginx 的 conf.d 加载与 WebSocket map 后重试，仍然失败", err)
}

// isNginxEnvVarErr 判断 nginx -t 的报错是不是"面板该铺好的变量没定义"：
// $connection_upgrade（反代 map）、$zp_scheme / $zp_https（fastcgi 参数块）。
func isNginxEnvVarErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown "`) || !strings.Contains(msg, `" variable`) {
		return false
	}
	return strings.Contains(msg, "connection_upgrade") ||
		strings.Contains(msg, "zp_scheme") || strings.Contains(msg, "zp_https")
}

func (s *Server) writeVhostOnce(ctx context.Context, domain, content string) error {
	bin := s.Cfg.ServicePath("zizpanel-helper")
	args := []string{"vhost-write", domain}
	// 用可注入的 root 判据（与环境自愈链同一个 seam）：单测要能验证
	// "以 root 运行时直接执行助手、不套 sudo"这条路径。
	if defaultSiteEuid() != 0 {
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

func (s *Server) nginxReload(ctx context.Context) error {
	_, err := s.callHelper(ctx, "nginx-reload")
	return err
}

// applySite 生成并应用一个站点的配置：写 vhost（helper 内 nginx -t，失败即回滚文件）
// → reload → 按真实结果复核。失败时撤销本次写入的 vhost，让"接口非 2xx = 这次没做成"成立。
// PHP 端点用 ResolveEndpoint：端点没在监听就直接失败，不写出指向空气的 vhost。
//
// 否则用户再点一次会撞上"store 里 ssl_enabled=true、nginx 却没在服务"的自相矛盾状态。
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
	// nginx -t 会以 root 创建 access_log/error_log，而 nginx master 以真实用户运行，
	// reload 时打不开 → 配置根本没加载（站点 404），reload 退出码却仍是 0。
	// 所以 reload 前把日志目录交还真实用户（真机 2026-09-17 确认必须）。
	if s.Cfg.User != "" && os.Geteuid() == 0 {
		_ = chownTreeTo(s.siteLogDir(), s.Cfg.User)
	}
	if err := siteReloadFn(s, ctx); err != nil {
		return s.rollbackVhostWrite(ctx, snap, siteWriteVhostFn, siteReloadFn,
			fmt.Errorf("配置已写入但 nginx 重载失败: %w", err))
	}
	// reload 成功 ≠ 新配置生效：nginx 读配置失败（日志/证书打不开）时只写 [emerg]，
	// nginx -s reload 退出码依然是 0；不复核就必然"面板说成功、用户打开 404/502"。
	if err := s.verifySiteServed(ctx, site); err != nil {
		return s.rollbackVhostWrite(ctx, snap, siteWriteVhostFn, siteReloadFn, err)
	}
	return nil
}

// 这几个包级变量是"写 vhost → reload → 复核"通道的可注入步骤：单测不允许调用
// 提权助手、不允许真发网络请求（生产指向真实实现）。回滚走同一条通道，不另开没测过的路径。
var (
	siteWriteVhostFn = func(s *Server, ctx context.Context, domain, content string) error {
		return s.writeVhost(ctx, domain, content)
	}
	siteReloadFn = func(s *Server, ctx context.Context) error {
		return s.nginxReload(ctx)
	}
	siteProbeFn       = curlSite
	siteDeleteVhostFn = func(s *Server, ctx context.Context, name string) error {
		return s.deleteVhost(ctx, name)
	}
	siteReadVhostFn = func(s *Server, path string) ([]byte, error) {
		return os.ReadFile(path)
	}
)

// siteVerifyWait / siteVerifyEvery 是"等新配置生效"的轮询窗口：nginx -s reload 是
// 异步的，新 worker 接管前旧配置仍在应答（真机 2026-09-16），窗口内 404/000 一律
// 视为还没生效。做成变量以便单测压到毫秒级。
var (
	siteVerifyWait  = 6 * time.Second
	siteVerifyEvery = 200 * time.Millisecond
)

// siteProbeTimeout 是单次探针超时；太长会让窗口内只探到一次。
const siteProbeTimeout = 4 * time.Second

// siteVhostProbePath 是"配置是否真的生效"的探测路径：每个站点 vhost 都带
// `location ~ /\. { deny all; }`，正则 location 优先于 `location /`，所以
// 403 = vhost 已加载 / 404（落到默认站点）= 没加载 —— 与站点内容、上游健康无关。
//
// vhost 没加载时请求落到默认站点（000-default），它没有这条规则 → 返回 404。
const siteVhostProbePath = "/.zp-vhost-probe"

// verifySiteServed 在 reload 后按真实请求复核"这份 vhost 真的生效了"（403）。
// 探针必须用 curlSite（--resolve 钉到 127.0.0.1），不能走真实 DNS；
// 窗口耗尽仍拿不到 403 才算失败（reload 是异步的，旧配置还会应答一阵）。
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

// siteVerifyTimeoutError 必须能指导排查：把四个检查点与 error_log 末几行写进错误里。
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

func humanWait(d time.Duration) string {
	if d >= time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%d 秒", int(d/time.Second))
	}
	return fmt.Sprintf("%.1f 秒", d.Seconds())
}

// existingLogPath 只在文件真的存在时返回路径，否则返回空串 —— 判据必须是"文件在不在"：
// 绝不拿推导出来的字符串冒充真实路径，读不到就让界面显示"路径未知"（用户 2026-09-22 要求不写死）。
func existingLogPath(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// nginxSiteErrorLog 是站点日志树（Cfg.LogRoot，默认 ~/www/_logs）里的 nginx 错误日志。
func (s *Server) nginxSiteErrorLog() string {
	if strings.TrimSpace(s.Cfg.LogRoot) == "" {
		return ""
	}
	return existingLogPath(filepath.Join(s.Cfg.LogRoot, "nginx-error.log"))
}

func (s *Server) nginxBrewErrorLog() string {
	if strings.TrimSpace(s.Cfg.BrewPrefix) == "" {
		return ""
	}
	return existingLogPath(filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log"))
}

// nginxErrorLogTail 读全局 error_log 末几行（读不到返回空串）；[emerg] 那行往往就是根因。
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

// vhost 写入的回滚：保存写入前的字节，失败时原样写回、本次新建的删掉。
// 只管数据库不管磁盘会留下残留，重试还会被它挡住。

// vhostSnapshot 是一份 vhost 文件被本次写入覆盖之前的样子。
type vhostSnapshot struct {
	name    string
	existed bool   // 写入前文件是否存在（false = 回滚时应删除）
	prev    []byte // 写入前的内容
	unknown bool   // 读旧内容失败（不是"不存在"）：回滚不安全，宁可不做
}

// snapshotVhost 读取 vhost 的当前内容作为回滚依据。
// 三种情况必须分开（混在一起会删掉用户的旧配置）：不存在 → 回滚删除；读到内容 →
// 原样写回；读失败（权限/IO）→ 回滚不安全，标记 unknown，如实报告而不是删文件。
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

// rollbackVhostWrite 撤销本次对 vhost 的写入：writeFn/reloadFn 由调用方给，
// 回滚走与写入完全相同的通道（避免没人测过的回滚路径）。还原后再 reload 一次，
// 让"磁盘 = nginx 正在用的"成立；reload 失败只记日志，绝不覆盖真正的失败原因。
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

// ensureSiteRoot 创建站点根目录；面板以 root 运行，目录必须归属真实用户，否则用户写不进去。
func (s *Server) ensureSiteRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("创建站点目录失败: %w", err)
	}
	if s.Cfg.User != "" && s.Cfg.User != "root" {
		if uid, gid, err := lookupIDs(s.Cfg.User); err == nil {
			_ = os.Chown(root, uid, gid)
		}
	}
	return nil
}

// ---------- HTTP 接口 ----------

func (s *Server) handleSiteList(w http.ResponseWriter, r *http.Request) {
	mgr := s.siteMgr()
	list, err := mgr.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取站点失败: "+err.Error())
		return
	}
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
		"list":        out,
		"www_root":    s.Cfg.WWWRoot,
		"log_dir":     s.siteLogDir(),
		"vhost_dir":   vhostDir,
		"brew_prefix": s.Cfg.BrewPrefix,
		// 前端不许自己拼路径（会写死用户名与 brew 前缀），读不到就是空串 → 界面显示"路径未知"。
		"nginx_error_log":      s.nginxSiteErrorLog(),
		"nginx_brew_error_log": s.nginxBrewErrorLog(),
		"presets":              sites.RewritePresets,
		"php_versions":         s.detectPHPVersions(r.Context()),
		// 路径由后端给（Apple Silicon/Intel 的 brew 前缀不同），前端不拼字符串。
		"config_files": s.panelConfigFiles(r.Context()),
	})
}

// detectPHPVersions 探测已装 PHP 版本并补齐运行时状态：发现逻辑在
// sites.DiscoverPHPVersions（不写死列表），这里只探测端点是否真在监听 + 实际运行版本。
// 每个版本有唯一端点（sites.PreferredEndpoint），"两版本共用端点"要显式暴露（Conflict）。
func (s *Server) detectPHPVersions(ctx context.Context) []sites.PHPVersion {
	out := sites.DiscoverPHPVersions(s.Cfg.BrewPrefix)
	for i := range out {
		if out[i].Pass != "" {
			out[i].Running = sites.EndpointLive(out[i].Pass)
		}
		// "默认版本" = 配置里的 PHPSvc 指向它。不能只看名字：DiscoverPHPVersions 按版本号
		// 去重后 `php` 别名与 `php@8.4` 合并成一条，保留的那条未必叫 php。
		out[i].IsDefault = s.Cfg.PHPSvc == out[i].Service ||
			s.Cfg.PHPSvc == "php@"+out[i].Version ||
			(s.Cfg.PHPSvc == "php" && out[i].Service == "php")
	}
	s.probePHPVersions(ctx, out)
	return out
}

// probePHPVersions 用一次真实请求探测 FPM 实际运行的版本，让"配置写的"与"实际跑的"一眼可见。
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

	// 用 -D - 把响应头打到 stdout 读 X-Powered-By。
	// Host 必须是 127.0.0.1（不是 localhost）：localhost 会精确命中 brew 自带的默认站点
	// → __zp_ver.php 404，PHP 版本探测永远"未复核"（mini 真机 error_log 证实）。
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

	// Laravel/ThinkPHP：运行目录要落到 public 子目录
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
		// 回滚刚建的记录，避免"数据库有、nginx 没有"（vhost 已由 applySite 撤销）。
		// 回滚失败要如实说出来，否则用户重试会被"站点已存在"挡住却不知道为什么。
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

func (s *Server) seedIndexPHP(root string, site *sites.Site) error {
	indexPath := filepath.Join(root, "index.php")
	if !strings.HasSuffix(site.Root, "public") && site.PHPVersion == "" {
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
	// 用 ResolveConfigEndpoint：FPM 没在跑也要能显示配置里写的端点，并把错误原文交给界面。
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
		"ssl":          s.siteSSLView(site),
	})
}

// siteSSLView 汇总证书展示字段：到期时间优先取真实证书文件（tlsx.CertExpiry），
// 数据库字段只是上次写入的快照；读不到文件退回数据库，days_left = -1 表示不猜。
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

// handleSiteConfSave 保存用户手写的整份 vhost（「配置」页手工编辑后点保存）。
// 与 handleSiteUpdate 的分工：后者按面板站点模型重新生成配置，这里以磁盘上这份为准。
//
// 用户 2026-09-17 要求："站点管理中应该能直接编辑配置文件，我要改默认端口"。
//
// 安全网一条都不能少：① helper 写入前 nginx -t，不通过不落盘；② reload 失败 → 回滚；
// ③ reload 成功但新配置的 listen 端口一个都不应答 → 同样回滚 —— 命令返回 0 不等于配置生效。
//
// 不复用 verifySiteServed：它探站点模型的 80/443 并要求 403，会误判"统一收到 8889"的手工改动。
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
	// 保存前拦"同端口 + 同 server_name"冲突：nginx 只加载一份，另一份静默失效（事故见下方注释）。
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
		// helper 已做 nginx -t 且失败不落盘，这里不必再回滚，只把 nginx 原文交回界面。
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// nginx -t 以 root 创建日志文件，属主不对会让 reload 失败且退出码仍是 0（同 applySite）。
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

type siteListenPort struct {
	Port int
	SSL  bool
}

// isNginxTokenBoundary 判断下标是否为 nginx 指令词边界，避免匹配到 ssl_listen 这类标识符。
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

// siteServerBlocks 返回每个 `server { … }` 块的原文（花括号配对）。
// 必须按块而非全文件：端口×名字的笛卡尔积会造出 a.com:443 这种不存在的组合，导致误报拦截。
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

// checkVhostCollisionWithFiles 检查这份配置的 (端口, server_name) 是否与 vhosts 目录里
// 别的 .conf 重复（selfFile 跳过自己）。必须拦：同端口重复 server_name 时 nginx 只加载
// 先 include 的那份，另一份静默失效、nginx -t 只给一行 warning（真机事故 2026-09-17）。
//
// 真机事故：wp 站存成 8889+同名、撞上反代规则 9，8889 的流量落到了别的站。
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

// siteListenPorts 从 vhost 内容提取全部 listen 端口（用于"改完真的在服务吗"的复核）。
// 按"listen 词 + 到分号"扫描而非按行：一行内的 `server { listen 8892; }` 等写法不能漏，
// 漏掉就等于跳过复核；前置字符须是分隔符以免匹配 ssl_listen；解析不出的（unix:）跳过。
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

	// 伪静态切到 Laravel/ThinkPHP 时运行目录跟着切到 public，切回则退回站点根目录；
	// 按"域名目录 + 模板要求"重新推导，避免改模板后目录还停在 public 导致 404。
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

	// CertPrimary 指定要绑定的 ACME 证书（primary 名）；不给时按域名/别名自动匹配。
	CertPrimary string `json:"cert_primary"`
	// Domain 是 cert_primary 的容错写法：前端直接给域名，由面板去证书库里找覆盖它的那张。
	Domain string `json:"domain"`
}

// handleSiteSSL 为站点签发/配置证书：self（openssl 自签）、mkcert（本地 CA）、
// manual（用户粘贴）、acme（引用已签发证书，不在本接口签发 —— 长任务必须走任务中心）。
// 站点写回 acme 自己的路径（非复制到 site-certs），续期原地覆盖、vhost 不用改。
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
		// 这三个来源把证书存到站点自己的目录（与 acme 证书库分开存放）。
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

	// 记下写入前的记录：SSL 绑定失败必须把 store 恢复原样，否则"面板说绑了、nginx 却没服务"。
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
	// acme 也走 applySite，因此自动获得"写 vhost → reload → 403 复核"整条链路。
	if err := s.applySite(r.Context(), site); err != nil {
		// applySite 已撤销 vhost；这里把 store 的 SSL 字段也退回，让"非 2xx = 没做成"三处一致。
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
		"provider_label": sslProviderLabel(req.Provider),
		"days_left":      siteSSLDaysLeft(certPath),
	})
}

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

func (s *Server) handleNginxTest(w http.ResponseWriter, r *http.Request) {
	res, err := s.callHelper(r.Context(), "nginx-test")
	if err != nil {
		// ⚠️ 失败时必须把助手的原始输出（nginx -t 正文，含文件名与行号）带给界面：
		// 2026-09-18 用户只看到"nginx 配置检查未通过"这一句，看不出哪一行错。
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

// Shutdown 在面板退出时清理资源：必须关掉终端会话，否则它们持有的真实 shell
// 会变成没人管的孤儿进程，审计上下文（哪个用户开的）也会丢失。
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

// Startup 在启动时做一次环境准备与自愈：注入 upgrade map 内容源（单一数据源）、
// 确保站点日志目录存在（nginx 打不开日志目录会直接启动失败）、补齐 WebSocket
// map 与 conf.d include。安装脚本也做，但启动再查一次能覆盖换配置/重装/恢复旧备份。
func (s *Server) Startup(ctx context.Context) {
	priv.SetUpgradeMapContent(sites.UpgradeMapConf())

	logDir := s.siteLogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		s.Log.Warn("创建站点日志目录失败 %s: %v", logDir, err)
	}

	s.healWebEnv(ctx)

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

	// 安装后自动建一个纯静态默认站点（用户 2026-09-21 要求"装完就有"），幂等：
	// 成功过只做一次现场复核，没有 nginx 就如实记"等待 nginx"。必须在
	// reconcileForwarders 之后，否则 vhost 里的 proxy_pass 会指向旧回环端口。
	s.ensureDefaultSiteOnStart(ctx)

	// 默认站点持续自愈：新机器是先装面板再装 nginx，启动那次检查必然"没有 nginx"，
	// 没有这条巡检就再也没人建默认站点（2026-09-22 报障）。判据见 maybeEnsureDefaultSite。
	go s.watchWebEnv(ctx)

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

// ensureNginxEnvOnStart 是自愈：安装脚本没跑或用户换了 nginx 配置时，
// 启动后也补齐 WebSocket map 与 conf.d include，否则反代站点过不了 nginx -t。
func (s *Server) ensureNginxEnvOnStart(ctx context.Context) {
	if defaultSiteEuid() != 0 {
		// 非 root 时尝试通过助手（sudoers 已授权）
		if _, err := s.callHelper(ctx, "nginx-ensure-env"); err != nil {
			s.Log.Warn("nginx 环境自愈失败（反向代理可能不可用）: %v", err)
		}
		// 后面的两步会**写真实 nginx 配置**（全局上限 / 运行时目录属主），
		// 而调试实例（make run-local）的 BrewPrefix/NginxConf 指向真机 —— 必须到此为止，
		// 否则就重现了 AGENTS 坑 162（调试实例真的动了真机）。
		return
	}
	// ① 环境片段（conf.d include / WebSocket upgrade map）。
	//
	// ⚠️ 三步必须互不遮挡：过去"环境一变就 return"，于是同一轮的 ②③ 永远轮不到
	//（新机器是装完面板才装 nginx），表现就是 2026-09-22 报的"导入数据库卡死"
	//（nginx 还是出厂 1m、client_body_temp 属主也不对）。
	msg, err := priv.EnsureNginxEnv()
	if err != nil {
		s.Log.Warn("nginx 环境自愈失败（反向代理可能不可用）: %v", err)
	} else if priv.NginxEnvChanged(msg) {
		// 只有真的改动了 nginx 配置才重载。
		// 每次启动无条件 reload 会在开机时给所有站点造成一次无谓的请求抖动。
		if rerr := s.nginxReload(ctx); rerr != nil {
			s.Log.Warn("nginx 环境已更新但重载失败: %v", rerr)
		} else {
			s.Log.Info("nginx 环境已更新并重载: %s", msg)
		}
	} else {
		s.Log.Info("nginx 环境检查: %s", msg)
	}

	// ② brew reinstall/upgrade 会把 nginx.conf 还原成出厂版（没有 client_max_body_size，
	// 出厂 1m）。真机后果（2026-09-18）：4MB 的 SQL 导入被 1m 拒掉，用户看到
	// "phpMyAdmin 导入失败/卡住"，面板里却怎么看都正常。
	s.ensureGlobalBodySize(ctx)

	// ③ nginx 运行时目录（带请求体的请求要往这里落盘）。
	s.ensureNginxRuntimeDirs()
}

// ensureGlobalBodySize 确保 nginx.conf 的 http 块有面板配置的全局请求体上限。
// 只读判断 → 不一致才写（备份、nginx -t、失败回滚见 priv.NginxTuningWrite）；
// 幂等：一致时只读一次文件，不改动、不 reload。
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

// ensureNginxRuntimeDirs 确保 nginx 运行时目录存在且属于真正的 worker 用户：
// nginx 把超内存缓冲的请求体落盘到 client_body_temp（真机上是 nobody:admin 0700），
// 属主不对就写不进去 —— GET 正常、一上传就 500/卡死（2026-09-18 mini 真机报障）。
//
// runtimeDirChownFn 是可注入点：单测要在非 root 下验证"属主未知时绝不乱改"。
var runtimeDirChownFn = os.Chown

func (s *Server) ensureNginxRuntimeDirs() {
	if defaultSiteEuid() != 0 {
		return
	}
	// 没有 nginx 就别自愈：这一步会 MkdirAll，若 brew 前缀不存在，root 会把
	// 整个前缀目录树建出来（"本来没有 Homebrew，面板却造了一个 /opt/homebrew"）。
	if present, _ := s.nginxPresent(); !present {
		return
	}
	// 属主判据必须来自运行体（正在跑的 worker 进程），读不到再退回 nginx.conf，
	// 都读不到就不改属主 —— 绝不猜（旧版猜 "nobody" 导致大请求体仍 500）。
	worker, uid, gid, how, ok := priv.NginxWorkerOwner(s.Cfg.NginxConf)
	base := filepath.Join(s.Cfg.BrewPrefix, "var", "run", "nginx")
	for _, sub := range []string{"", "client_body_temp", "proxy_temp", "fastcgi_temp", "uwsgi_temp", "scgi_temp"} {
		dir := filepath.Join(base, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			s.Log.Warn("创建 nginx 运行时目录 %s 失败：%v", dir, err)
			continue
		}
		if !ok {
			s.Log.Warn("nginx worker 用户无法确定（%s）：%s 已存在但**未修改属主**；"+
				"大请求体（音色样本、大 SQL 导入）仍可能被 nginx 自己回 500 —— "+
				"请在 %s 里写上 `user <用户名> <组>;` 后点「🔧 修复 Nginx 环境」", how, dir, s.Cfg.NginxConf)
			continue
		}
		st, serr := os.Stat(dir)
		if serr == nil {
			if sys, sok := st.Sys().(*syscall.Stat_t); sok && int(sys.Uid) == uid && int(sys.Gid) == gid {
				continue // 已经对了：一次写都不做（巡检每 2 分钟跑一次，必须安静）
			}
		}
		if err := runtimeDirChownFn(dir, uid, gid); err != nil {
			s.Log.Warn("调整 %s 属主为 %s 失败（上传/导入可能仍失败）：%v", dir, worker, err)
		} else {
			s.Log.Info("已把 nginx 运行时目录 %s 的属主改为 %s（判据：%s）——"+
				"否则带请求体的请求会写不进去：上传/导入 500 或卡住", dir, worker, how)
		}
	}
}

// healWebEnv 是"环境层面自愈"的总入口：nginx 侧 + PHP 侧一次做完。
// 面板启动、任何任务收尾（launchTask）、默认站点巡检三处都触发同一入口 ——
// "启动之后才装 nginx/PHP"暴露的问题，只在启动时跑一次是覆盖不到的（2026-09-22 报障）。
func (s *Server) healWebEnv(ctx context.Context) {
	s.ensureNginxEnvOnStart(ctx)
	s.ensurePHPLimitsOnStart(ctx)
}

// ensurePHPLimitsOnStart 把面板配置的上传/执行上限落到已装 PHP 版本的
// conf.d/99-zizpanel-limits.ini（幂等）。新机器是先装面板再装 PHP、启动检查早过了，
// PHP 一直是出厂值（2M/8M/30s），导入稍大 SQL 被掐死（2026-09-22 报障）。
//
// 只在片段不存在时创建（片段语义是"删掉即恢复出厂限制"），且只在真的写入后重启。
func (s *Server) ensurePHPLimitsOnStart(ctx context.Context) {
	if defaultSiteEuid() != 0 {
		return
	}
	lim := s.uploadLimits().Normalize()
	versions := sites.DiscoverPHPVersions(s.Cfg.BrewPrefix)
	if len(versions) == 0 {
		return
	}
	for _, v := range versions {
		path := sites.PHPConfDPath(s.Cfg.BrewPrefix, v.Version)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			continue // 已存在：面板的设置页与用户自己都能改它，自愈不碰。
		}
		lr, err := sites.EnsurePHPLimits(s.Cfg.BrewPrefix, v.Version, lim, true)
		if err != nil {
			s.Log.Warn("PHP %s 的上传/执行限制片段写入失败（导入大 SQL 会失败）：%v", v.Version, err)
			continue
		}
		if !lr.Changed {
			continue
		}
		s.Log.Info("已写入 PHP %s 的上传/执行限制片段 %s（upload=%s post=%s memory=%s max_execution_time=%ds）——"+
			"全新机器上 PHP 出厂值只有 2M/8M，导入稍大的 SQL 会失败",
			v.Version, lr.Path, lim.UploadMaxFilesize, lim.PostMaxSize, lim.MemoryLimit, lim.MaxExecutionTime)
		svcName := s.phpServiceName(v.Version)
		if svcName == "" {
			s.Log.Info("PHP %s 的限制片段已写入，但面板里没有它的服务记录，"+
				"无法重启：该版本下次启动时自动生效（也可到「网站管理 → PHP 环境」修复端点并重启）", v.Version)
			continue
		}
		if _, err := s.svcManager().Action(ctx, svcName, "restart"); err != nil {
			s.Log.Warn("PHP %s 的限制片段已写入，但重启 %s 失败（新上限还没生效）：%v", v.Version, svcName, err)
			continue
		}
		s.Log.Info("已重启 %s，使新的上传/执行上限生效", svcName)
	}
}

// nginxWorkerUserFromConf 读 nginx.conf 的 `user` 指令（实现只有 priv 一份）。
// 读不到返回空 —— 调用方必须据此放弃改属主，绝不猜一个。
func nginxWorkerUserFromConf(confPath string) string {
	return priv.NginxWorkerUserFromConf(confPath)
}

// chownToUser 把路径属主改成指定用户（同属主时返回 changed=false）。用户不存在返回错误。
func chownToUser(path, user string) (changed bool, err error) {
	u, lerr := userpkg.Lookup(user)
	if lerr != nil {
		return false, lerr
	}
	uid, aerr := strconv.Atoi(u.Uid)
	gid, berr := strconv.Atoi(u.Gid)
	if aerr != nil || berr != nil {
		return false, fmt.Errorf("解析用户 %s 的 uid/gid 失败", user)
	}
	st, serr := os.Stat(path)
	if serr != nil {
		return false, serr
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		if int(sys.Uid) == uid && int(sys.Gid) == gid {
			return false, nil
		}
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return false, err
	}
	return true, nil
}

// checkProxyAgainstSiteVhosts 检查一条反代规则是否与已有 vhost 在"同端口 +
// 同 server_name"上冲突。与 checkVhostCollisionWithFiles 同一判据、反方向调用；
// 两边都要拦，否则谁先谁后决定了哪一份被静默忽略。
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

// webEnvHealMu 串行化环境自愈：多个任务同时收尾（或任务与巡检撞上）时，
// 只让一个真的动手，其余的**跳过而不是排队** —— 它们要做的事一模一样，
// 排队只会让最后一个在几秒后再重复一遍已经做完的事。
var webEnvHealMu sync.Mutex

// kickEnvHeal 在后台跑一次「环境自愈 + 默认站点核对」（任务中心的"结束"不该被它拖住）。
// 用独立后台 ctx + 2 分钟上限：面板可能在写完配置前被升级重启，但每次写入本身
// 原子且带校验，最坏只是这次没做完，下一轮巡检接着做。
func (s *Server) kickEnvHeal() {
	if !webEnvHealMu.TryLock() {
		return
	}
	go func() {
		defer webEnvHealMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.healWebEnv(ctx)
		s.maybeEnsureDefaultSite(ctx)
	}()
}

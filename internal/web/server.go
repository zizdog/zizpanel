// Package web 提供面板的 HTTP 层：路由、中间件、API 与静态资源。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zizdog/zizpanel/internal/auth"
	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/logs"
	"github.com/zizdog/zizpanel/internal/logx"
	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/store"
	"github.com/zizdog/zizpanel/internal/sysinfo"
	"github.com/zizdog/zizpanel/internal/tasks"
	"github.com/zizdog/zizpanel/internal/term"
	"github.com/zizdog/zizpanel/internal/version"
)

//go:embed all:assets
var assetsFS embed.FS

// Server 汇总面板 HTTP 层所需的一切依赖。
type Server struct {
	Cfg   *config.Config
	Store *store.Store
	Auth  *auth.Manager
	Info  *sysinfo.Collector
	Procs *sysinfo.ProcSampler
	Log   *logx.Logger

	// serviceRepo 是服务注册表（服务管理与应用市场共用）
	serviceRepo *services.Repository

	// ---- 应用市场用的短缓存 ----
	//
	// 为什么需要：市场列表要为每个条目判断"装了没有"，
	// 而判断手段都很贵 ——
	//   · brew list --versions <formula>  单次约 0.4~0.6 秒
	//   · Docker 套接字探测               每个候选路径 800ms 超时
	// 早期实现是**在循环里逐个调用**，11 个条目就累加到 4.4 秒
	// （实测），页面明显卡顿。
	// 现在改成"整批查一次 + 短 TTL 缓存"：brew 全部 formula 一次只要
	// 0.58 秒（和查单个几乎一样），Docker 只探一次。
	mktMu         sync.Mutex
	mktRefreshing atomic.Bool // 防止市场缓存刷新叠加
	mktBrew       map[string]bool
	// mktBrewVer 与 mktBrew 同一次 `brew list --versions` 的**版本**投影
	// （formula → 版本串）。卸载计划要把目录里的 `php@8.4` 对到机器上真实装的
	// `php 8.4.7`（见 services.ResolveBrewFormula）—— 没有版本就只能精确匹配，
	// 于是"装着 PHP 8.4 的机器"在市场里显示成未安装、连卸载入口都没有。
	mktBrewVer map[string]string
	// mktBrewOK 表示 mktBrew 这一次是**真的探测成功**的结论（而不是"brew 探测失败
	// 时返回的空集合"）。二者必须分开：把失败的空集合当成"这台机器什么都没装"，
	// 会让全部只靠 brew 证据的条目一起显示「安装」（用户报障的那一类）。
	// false 时缓存按 marketProbeMissTTL 很快重试，且前端会如实标"未复核"。
	mktBrewOK   bool
	mktBrewAt   time.Time
	mktDocker   string
	mktDockerV  string
	mktDockerAt time.Time

	// ---- Docker 加速源「上次检测」的内存缓存（带时间戳）----
	//
	// 为什么放在 Server 而不是 services.Manager：svcManager() **每次请求都新建**
	// Manager（设置页保存后立刻生效，见 api_services.go），缓存挂在 Manager 上
	// 活不过一次请求。也不落库：这只是"上次检测结果"的展示数据，面板重启后
	// 重新检测一次即可，为它建表属于过度设计（用户明确要求别这么做）。
	//
	// 只由用户主动点「检测」写入（POST /api/v1/docker/mirrors/probe），
	// 读接口 GET /api/v1/docker/mirrors/cached 绝不触发探测。
	dockerMirrorMu      sync.Mutex
	dockerMirrorProbes  []services.DockerMirrorProbe
	dockerMirrorChecked time.Time

	// logCat 是日志目录（惰性初始化，因为要读取服务注册表）
	logCat  *logs.Catalog
	logOnce sync.Once

	// termMgr 是 Web 终端管理器（惰性初始化；开关可在运行时改变）
	termMgr  *term.Manager
	termOnce sync.Once

	// Tasks 是任务中心：安装/卸载这类长任务在后台跑，进度走 SSE。
	// 它**不属于任何一次请求**，所以不注册进 HTTP 层，也不做持久化
	// （面板重启后"正在安装"本身就是假的，见 SPEC-任务中心.md）。
	Tasks *tasks.Manager

	// forwarders 是「局域网出口」的回环 TCP 转发器管理器（见 api_proxies.go）。
	//
	// 为什么它属于面板而不是 nginx：macOS 15 的「本地网络」隐私门会拦 Homebrew
	// 的 nginx（ad-hoc 签名，标识随二进制 UUID 变），无头服务器没人点弹窗 →
	// nginx 连局域网直接 "No route to host"，反代全部 502；而面板自己
	// （Go、linker-signed）从来没被拦过。所以把局域网出口收回面板：
	// 转发器在 127.0.0.1 上听，nginx 只连回环。
	forwarders *proxies.Manager

	// ---- ACME 证书（见 api_certs.go）----
	//
	// acmeMgr 惰性构造：构造它要读 DataDir，而绝大多数面板请求用不到证书功能；
	// acmeOverride 是单测注入点（假实现，绝不联网、不碰真实 CA）。
	acmeMu       sync.Mutex
	acmeMgr      acmeManager
	acmeOverride acmeManager
	// acmeCredMu 保护 DNS 凭据文件（DataDir 下 0600）的读写。
	acmeCredMu sync.Mutex

	// ---- 基础环境（运行依赖层，见 api_system.go）----
	//
	// baseEnvProbeOverride / baseEnvInstallOverride 是单测注入点：
	// 默认探测会执行 /usr/bin/xcode-select 并 stat 真实 Homebrew 前缀，
	// 默认安装会真的调用 EnsureHomebrew（可能联网跑 brew 安装脚本）——
	// 结论与副作用都会随测试机而变，违反"单测不许碰真实环境"。
	baseEnvProbeOverride   func(ctx context.Context) services.BaseEnvStatus
	baseEnvInstallOverride func(ctx context.Context, res *services.InstallResult) error

	// svcManagerOverride 仅供单测：替换服务管理器的构造。
	//
	// 为什么需要它：`DELETE /api/v1/services/{name}`（只删记录）的语义硬要求是
	// **绝不触碰运行时**，而"是否触碰运行时"的唯一入口就是有没有构造 Manager
	// （构造会探测 Docker socket、ReconcilePaths 写配置）。单测把这里替换成
	// 一个会记账的假实现，就能断言删除通路根本没有走到运行时那一步。
	svcManagerOverride func(*Server) *services.Manager

	// dockerRuntimeProbeOverride 仅供单测：替换"容器运行时到底装没装/在不在跑"的探测。
	//
	// 为什么需要它：市场卡片对 docker-runtime 的"已安装"判据必须是**真实探测**
	// （见 api_services.go 与 services/docker_runtime.go），而默认探测会
	// 看 PATH 上有没有 colima、连一次本机 docker socket ——
	// 结论会随开发机装没装 Colima 而变，且真的去碰了系统（违反"单测不许碰真实
	// 服务"）。更关键的是要能造出用户实测的那个现场：**只有僵尸 plist、
	// 二进制不在**，并断言此时 installed 必须是 false。
	dockerRuntimeProbeOverride func(ctx context.Context) services.DockerRuntimeState

	static  fs.FS
	handler http.Handler
	startAt time.Time

	loginSvc loginLimiter
	procMu   sync.Mutex
	procAt   time.Time
	procList []sysinfo.Proc
}

// New 构造 Server 并注册所有路由。
func New(cfg *config.Config, st *store.Store, am *auth.Manager, col *sysinfo.Collector) (*Server, error) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, fmt.Errorf("加载内置前端资源失败: %w", err)
	}
	s := &Server{
		Cfg:         cfg,
		Store:       st,
		Auth:        am,
		Info:        col,
		Procs:       sysinfo.NewProcSampler(),
		Log:         logx.New("web"),
		serviceRepo: services.NewRepository(st),
		Tasks:       tasks.NewManager(),
		static:      sub,
		startAt:     time.Now(),
	}
	// TCC 指引里"要去系统设置里授权"的那个二进制路径按配置解析一次：
	// 非默认安装根（BinDir 配在别处）也能给出正确路径。
	setPanelBinaryForGuide(cfg.BinDir)
	// 转发器的解析器走 web 包的可注入变量（proxyLookupHostFn），
	// 这样单测能把域名解析钉成假数据，绝不碰真实网络。
	s.forwarders = proxies.NewManager(proxies.ManagerOptions{
		Logf:       func(format string, args ...any) { s.Log.Warn(format, args...) },
		LookupHost: func(host string) ([]string, error) { return proxyLookupHostFn(host) },
	})
	s.handler = s.routes()
	return s, nil
}

// ServeHTTP 让 Server 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// routes 注册全部路由。
//
// 约定：
//   - /api/v1/*  ← JSON 接口，全部走 accessControl + JSON 错误
//   - /*         ← 内置前端（SPA），未知路径回落到 index.html
func (s *Server) routes() http.Handler {
	root := http.NewServeMux()

	// ---------- 公开接口 ----------
	root.HandleFunc("GET /api/v1/ping", s.handlePing)
	root.HandleFunc("GET /api/v1/health", s.handleHealth)
	root.HandleFunc("GET /api/v1/setup/status", s.handleSetupStatus)
	root.HandleFunc("POST /api/v1/setup", s.handleSetup)
	root.HandleFunc("POST /api/v1/login", s.handleLogin)
	root.HandleFunc("POST /api/v1/login/totp", s.handleLoginTOTP)
	root.HandleFunc("POST /api/v1/logout", s.handleLogout)

	// ---------- 需要登录 ----------
	root.HandleFunc("GET /api/v1/session", s.requireAuth(s.handleSession))
	root.HandleFunc("GET /api/v1/system/info", s.requireAuth(s.handleSystemInfo))
	root.HandleFunc("GET /api/v1/system/stream", s.requireAuth(s.handleSystemStream))
	root.HandleFunc("GET /api/v1/system/processes", s.requireAuth(s.handleProcesses))
	// 基础依赖（ffmpeg / ffprobe）状态：曾经静默消失过（被 brew autoremove 带走），
	// 表现是 TTS 返回 200 + 空 body —— 必须能一眼看到缺没缺（见 basedep.go）。
	root.HandleFunc("GET /api/v1/system/deps", s.requireAuth(s.handleSystemDeps))
	// 基础环境（运行依赖层：CLT → Homebrew → ffmpeg）状态与一键安装。
	//
	// 它和「网站环境（LNMP）」是两层：base-env 只管运行依赖，安装任务绝不装
	// nginx / PHP / MySQL（那走 POST /api/v1/market/install-lnmp）。
	// 见 services/baseenv.go 与 api_system.go 的 handleSystemBaseEnv。
	root.HandleFunc("GET /api/v1/system/base-env", s.requireAuth(s.handleSystemBaseEnv))
	root.HandleFunc("POST /api/v1/system/base-env/install", s.requireAuth(s.handleSystemBaseEnvInstall))

	// 系统设置（macOS 服务器化）：状态探测 + 一键动作（动作走任务中心）
	root.HandleFunc("GET /api/v1/system/settings", s.requireAuth(s.handleSystemSettings))
	// 内网段预授权：秒级同步接口（写 defaults + 读回复核），比 `{action}` 更具体，
	// 所以会优先命中（见 api_systemsettings_lan.go）。
	root.HandleFunc("POST /api/v1/system/settings/lan-preauth", s.requireAuth(s.handleLANPreauth))
	root.HandleFunc("POST /api/v1/system/settings/{action}", s.requireAuth(s.handleSystemSettingsAction))
	// 磁盘管理（见 api_disks.go / api_disks_ops.go）：只读枚举 + 挂载/卸载 +
	// 开机自动挂载 + 非系统盘的抹盘/格式化/建卷/删卷/重命名。
	// 全部 requireAuth；写操作另有 CSRF 双提交（requireAuth 内统一处理）。
	// 系统盘相关设备一律 403，且被拒时不会执行任何 diskutil 写命令。
	root.HandleFunc("GET /api/v1/system/disks", s.requireAuth(s.handleDiskList))
	// 「申请授权」：读了外接卷才会让 macOS 弹窗，所以同步预检不通过就当场 4xx，
	// 通过才走任务中心（见 api_disks_ops.go 的 handleDiskVolumeAuthRequest）。
	root.HandleFunc("POST /api/v1/system/disks/volume-auth", s.requireAuth(s.handleDiskVolumeAuthRequest))
	root.HandleFunc("POST /api/v1/system/disks/{id}/mount", s.requireAuth(s.handleDiskMount))
	root.HandleFunc("POST /api/v1/system/disks/{id}/unmount", s.requireAuth(s.handleDiskUnmount))
	root.HandleFunc("POST /api/v1/system/disks/{id}/auto-mount", s.requireAuth(s.handleDiskAutoMount))
	// 危险动作（前端要手输设备标识；后端执行前重新枚举校验，见 beginDiskWrite）
	root.HandleFunc("POST /api/v1/system/disks/{id}/erase", s.requireAuth(s.handleDiskErase))
	root.HandleFunc("POST /api/v1/system/disks/{id}/volume-create", s.requireAuth(s.handleDiskVolumeCreate))
	root.HandleFunc("POST /api/v1/system/disks/{id}/volume-delete", s.requireAuth(s.handleDiskVolumeDelete))
	root.HandleFunc("POST /api/v1/system/disks/{id}/volume-rename", s.requireAuth(s.handleDiskVolumeRename))
	// init-volume 是第一版接口名，保留为 volume-create 的别名，不破坏既有调用。
	root.HandleFunc("POST /api/v1/system/disks/{id}/init-volume", s.requireAuth(s.handleDiskVolumeCreate))
	// 「权限」页（见 api_permissions.go）：逐项申请 macOS 授权。
	// GET 只用不碰受保护路径的判据；apply 同步预检不通过就当场 4xx，通过才走任务中心。
	root.HandleFunc("GET /api/v1/permissions", s.requireAuth(s.handlePermissionsList))
	root.HandleFunc("POST /api/v1/permissions/{id}/apply", s.requireAuth(s.handlePermissionApply))
	// 操作审计：检索 + 游标分页 + 导出（facets 给下拉框提供真实出现过的动作名）
	root.HandleFunc("GET /api/v1/audit", s.requireAuth(s.handleAuditList))
	root.HandleFunc("GET /api/v1/audit/facets", s.requireAuth(s.handleAuditFacets))
	root.HandleFunc("GET /api/v1/audit/export", s.requireAuth(s.handleAuditExport))

	// 任务中心：安装/卸载的实时进度（见 SPEC-任务中心.md）
	root.HandleFunc("GET /api/v1/tasks", s.requireAuth(s.handleTasksList))
	root.HandleFunc("GET /api/v1/tasks/{id}", s.requireAuth(s.handleTaskGet))
	root.HandleFunc("GET /api/v1/tasks/{id}/stream", s.requireAuth(s.handleTaskStream))
	root.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.requireAuth(s.handleTaskCancel))
	// 任务输入：安装过程中"限时询问"的通道（如 MySQL root 口令）。
	// 只有正在等待该 key 的任务才接受，见 tasks.Task.SubmitInput。
	root.HandleFunc("POST /api/v1/tasks/{id}/input", s.requireAuth(s.handleTaskInput))

	root.HandleFunc("POST /api/v1/account/password", s.requireAuth(s.handleChangePassword))
	// 改用户名：要当前密码确认，但不吊销会话（会话按 user_id 关联）
	root.HandleFunc("POST /api/v1/account/username", s.requireAuth(s.handleRenameUser))
	root.HandleFunc("POST /api/v1/account/totp/setup", s.requireAuth(s.handleTOTPSetup))
	root.HandleFunc("POST /api/v1/account/totp/enable", s.requireAuth(s.handleTOTPEnable))
	root.HandleFunc("POST /api/v1/account/totp/disable", s.requireAuth(s.handleTOTPDisable))
	root.HandleFunc("GET /api/v1/account/sessions", s.requireAuth(s.handleListSessions))

	// ---------- 站点管理 ----------
	// ---- 反向代理（独立功能）----
	root.HandleFunc("GET /api/v1/proxies", s.requireAuth(s.handleProxyList))
	root.HandleFunc("POST /api/v1/proxies", s.requireAuth(s.handleProxyCreate))
	root.HandleFunc("POST /api/v1/proxies/test", s.requireAuth(s.handleProxyTest))
	root.HandleFunc("POST /api/v1/proxies/{id}", s.requireAuth(s.handleProxyUpdate))
	root.HandleFunc("POST /api/v1/proxies/{id}/toggle", s.requireAuth(s.handleProxyToggle))
	// 大请求体探测：只在用户点按钮时调用（真的发 64KB），绝不进列表渲染路径。
	root.HandleFunc("POST /api/v1/proxies/{id}/probe-body", s.requireAuth(s.handleProxyBodyProbe))
	// 反向代理的 HTTPS：与站点侧 /sites/{domain}/ssl 对称。
	// acme 来源直接引用 <DataDir>/certs/<primary>/（续期同路径覆盖），
	// self/mkcert/manual 落到 <DataDir>/proxy-certs/<规则>/。
	root.HandleFunc("POST /api/v1/proxies/{id}/ssl", s.requireAuth(s.handleProxySSL))
	root.HandleFunc("DELETE /api/v1/proxies/{id}/ssl", s.requireAuth(s.handleProxySSLDisable))
	root.HandleFunc("DELETE /api/v1/proxies/{id}", s.requireAuth(s.handleProxyDelete))

	root.HandleFunc("GET /api/v1/sites", s.requireAuth(s.handleSiteList))
	root.HandleFunc("POST /api/v1/sites", s.requireAuth(s.handleSiteCreate))
	root.HandleFunc("POST /api/v1/sites/reload", s.requireAuth(s.handleSiteReload))
	root.HandleFunc("GET /api/v1/sites/{domain}", s.requireAuth(s.handleSiteGet))
	root.HandleFunc("POST /api/v1/sites/{domain}", s.requireAuth(s.handleSiteUpdate))
	root.HandleFunc("DELETE /api/v1/sites/{domain}", s.requireAuth(s.handleSiteDelete))
	root.HandleFunc("POST /api/v1/sites/{domain}/ssl", s.requireAuth(s.handleSiteSSL))
	root.HandleFunc("DELETE /api/v1/sites/{domain}/ssl", s.requireAuth(s.handleSiteSSLDisable))
	root.HandleFunc("POST /api/v1/sites/{domain}/conf", s.requireAuth(s.handleSiteConfSave))
	root.HandleFunc("GET /api/v1/sites/{domain}/check", s.requireAuth(s.handleSiteCheck))
	root.HandleFunc("GET /api/v1/sites/{domain}/log", s.requireAuth(s.handleSiteLog))

	// ---------- ACME 证书 ----------
	// 证书文件由 internal/acme 落在 <DataDir>/certs/<primary>/；
	// 站点侧通过 handleSiteSSL 的 provider=acme 直接引用同一份路径（续期同路径覆盖）。
	root.HandleFunc("GET /api/v1/certs", s.requireAuth(s.handleCertsList))
	root.HandleFunc("POST /api/v1/certs", s.requireAuth(s.handleCertsIssue))
	// 必须注册在 {primary} 之前也无所谓（Go 1.22 mux 按最具体匹配），
	// 但放在一起更清楚：这是一条独立路径，不是某张证书的子资源。
	root.HandleFunc("GET /api/v1/certs/dns-providers", s.requireAuth(s.handleCertDNSProviders))
	root.HandleFunc("POST /api/v1/certs/{primary}/renew", s.requireAuth(s.handleCertRenew))
	// 失败条目的「一键重试」：用保存的条目 + 服务端已存凭据重签，用户不必重填。
	root.HandleFunc("POST /api/v1/certs/{primary}/retry", s.requireAuth(s.handleCertRetry))
	root.HandleFunc("DELETE /api/v1/certs/{primary}", s.requireAuth(s.handleCertDelete))

	// ---------- PHP 多版本 ----------
	// 已安装的 PHP 版本清单（界面用它渲染"PHP 版本"下拉框）
	root.HandleFunc("GET /api/v1/php", s.requireAuth(s.handlePHPList))
	// 让某个版本真正监听在它独有的端点上（改 www.conf + 重启 fpm）
	root.HandleFunc("POST /api/v1/php/fix-listen", s.requireAuth(s.handlePHPFixListen))

	// ---------- 数据库管理 ----------
	root.HandleFunc("GET /api/v1/database", s.requireAuth(s.handleDatabaseOverview))
	root.HandleFunc("GET /api/v1/database/tables", s.requireAuth(s.handleDatabaseTables))
	root.HandleFunc("POST /api/v1/database", s.requireAuth(s.handleDatabaseCreate))
	root.HandleFunc("DELETE /api/v1/database/{name}", s.requireAuth(s.handleDatabaseDrop))
	root.HandleFunc("POST /api/v1/database/user", s.requireAuth(s.handleDatabaseUserCreate))
	root.HandleFunc("DELETE /api/v1/database/user", s.requireAuth(s.handleDatabaseUserDrop))
	root.HandleFunc("POST /api/v1/database/user/password", s.requireAuth(s.handleDatabaseUserPassword))
	root.HandleFunc("POST /api/v1/database/grant", s.requireAuth(s.handleDatabaseGrant))
	root.HandleFunc("GET /api/v1/database/grants", s.requireAuth(s.handleDatabaseGrants))
	root.HandleFunc("POST /api/v1/database/query", s.requireAuth(s.handleDatabaseQuery))
	root.HandleFunc("POST /api/v1/database/dump", s.requireAuth(s.handleDatabaseDump))
	root.HandleFunc("POST /api/v1/database/import", s.requireAuth(s.handleDatabaseImport))
	root.HandleFunc("GET /api/v1/database/backups", s.requireAuth(s.handleDatabaseBackups))
	root.HandleFunc("GET /api/v1/database/slow-queries", s.requireAuth(s.handleDatabaseSlowQueries))

	// ---------- 日志中心 ----------
	root.HandleFunc("GET /api/v1/logs", s.requireAuth(s.handleLogList))
	root.HandleFunc("GET /api/v1/logs/read", s.requireAuth(s.handleLogRead))
	root.HandleFunc("GET /api/v1/logs/stream", s.requireAuth(s.handleLogStream))
	root.HandleFunc("GET /api/v1/logs/download", s.requireAuth(s.handleLogDownload))
	root.HandleFunc("POST /api/v1/logs/truncate", s.requireAuth(s.handleLogTruncate))
	root.HandleFunc("DELETE /api/v1/logs", s.requireAuth(s.handleLogDelete))

	// ---------- 计划任务 ----------
	root.HandleFunc("GET /api/v1/cron", s.requireAuth(s.handleCronList))
	root.HandleFunc("POST /api/v1/cron", s.requireAuth(s.handleCronCreate))
	root.HandleFunc("GET /api/v1/cron/preview", s.requireAuth(s.handleCronPreview))
	root.HandleFunc("POST /api/v1/cron/sync", s.requireAuth(s.handleCronSync))
	root.HandleFunc("GET /api/v1/cron/{id}", s.requireAuth(s.handleCronGet))
	root.HandleFunc("POST /api/v1/cron/{id}", s.requireAuth(s.handleCronUpdate))
	root.HandleFunc("DELETE /api/v1/cron/{id}", s.requireAuth(s.handleCronDelete))
	root.HandleFunc("POST /api/v1/cron/{id}/toggle", s.requireAuth(s.handleCronToggle))
	root.HandleFunc("POST /api/v1/cron/{id}/run", s.requireAuth(s.handleCronRun))
	root.HandleFunc("GET /api/v1/cron/{id}/log", s.requireAuth(s.handleCronLog))
	root.HandleFunc("GET /api/v1/backups", s.requireAuth(s.handleBackupList))
	root.HandleFunc("POST /api/v1/backups", s.requireAuth(s.handleBackupCreate))
	// 注意：/backups/upload 与 /backups/restore 是字面量，比 /backups/{name} 更具体，
	// Go 1.22 的 ServeMux 会优先匹配它们（方法不同也不会冲突）。
	root.HandleFunc("POST /api/v1/backups/upload", s.requireAuth(s.handleBackupUpload))
	root.HandleFunc("POST /api/v1/backups/restore", s.requireAuth(s.handleBackupRestore))
	root.HandleFunc("GET /api/v1/backups/{name}", s.requireAuth(s.handleBackupInfo))
	root.HandleFunc("DELETE /api/v1/backups/{name}", s.requireAuth(s.handleBackupDelete))
	root.HandleFunc("GET /api/v1/backups/{name}/download", s.requireAuth(s.handleBackupDownload))

	// ---------- 文件管理器 ----------
	root.HandleFunc("GET /api/v1/files", s.requireAuth(s.handleFileList))
	root.HandleFunc("GET /api/v1/files/read", s.requireAuth(s.handleFileRead))
	root.HandleFunc("POST /api/v1/files/write", s.requireAuth(s.handleFileWrite))
	root.HandleFunc("POST /api/v1/files/mkdir", s.requireAuth(s.handleFileMkdir))
	root.HandleFunc("POST /api/v1/files/touch", s.requireAuth(s.handleFileTouch))
	root.HandleFunc("POST /api/v1/files/rename", s.requireAuth(s.handleFileRename))
	root.HandleFunc("POST /api/v1/files/copy", s.requireAuth(s.handleFileCopy))
	root.HandleFunc("POST /api/v1/files/move", s.requireAuth(s.handleFileMove))
	root.HandleFunc("POST /api/v1/files/chmod", s.requireAuth(s.handleFileChmod))
	root.HandleFunc("POST /api/v1/files/delete", s.requireAuth(s.handleFileDelete))
	root.HandleFunc("POST /api/v1/files/compress", s.requireAuth(s.handleFileCompress))
	root.HandleFunc("POST /api/v1/files/extract", s.requireAuth(s.handleFileExtract))
	root.HandleFunc("GET /api/v1/files/download", s.requireAuth(s.handleFileDownload))
	root.HandleFunc("POST /api/v1/files/upload", s.requireAuth(s.handleFileUpload))
	// 面板自己的单次上传上限（前端本地预检与分批都靠它，不许再写死常数）。
	root.HandleFunc("GET /api/v1/files/upload-limit", s.requireAuth(s.handleGetPanelUploadLimit))
	root.HandleFunc("POST /api/v1/files/search", s.requireAuth(s.handleFileSearch))
	root.HandleFunc("POST /api/v1/files/replace", s.requireAuth(s.handleFileReplace))

	// ---------- Web 终端 ----------
	root.HandleFunc("GET /api/v1/terminal", s.requireAuth(s.handleTerminalStatus))
	root.HandleFunc("GET /api/v1/terminal/ws", s.handleTerminalWS) // 自行校验会话（WS 无法带自定义头）
	root.HandleFunc("DELETE /api/v1/terminal/{id}", s.requireAuth(s.handleTerminalKill))

	// ---------- 服务管理 ----------
	root.HandleFunc("GET /api/v1/services", s.requireAuth(s.handleServiceList))
	root.HandleFunc("POST /api/v1/services", s.requireAuth(s.handleServiceCreate))
	root.HandleFunc("GET /api/v1/services/{name}", s.requireAuth(s.handleServiceGet))
	root.HandleFunc("POST /api/v1/services/{name}", s.requireAuth(s.handleServiceUpdate))
	root.HandleFunc("DELETE /api/v1/services/{name}", s.requireAuth(s.handleServiceDelete))
	root.HandleFunc("POST /api/v1/services/{name}/{action}", s.requireAuth(s.handleServiceAction))
	root.HandleFunc("GET /api/v1/services/{name}/credentials", s.requireAuth(s.handleServiceCredentials))
	// File Browser「主目录」设置：读取/修改 launchd plist 的 -r 并回读核对。
	// 这两条比上面的 {name}/{action} 更具体，Go 1.22 的 mux 会优先匹配它们。
	root.HandleFunc("GET /api/v1/services/{name}/filebrowser-root", s.requireAuth(s.handleFilebrowserRootGet))
	root.HandleFunc("POST /api/v1/services/{name}/filebrowser-root", s.requireAuth(s.handleFilebrowserRootSet))
	// brew 的真实状态（服务/可升级/已装），面板直接渲染它，不再只信自己的记录表。
	root.HandleFunc("GET /api/v1/brew/overview", s.requireAuth(s.handleBrewOverview))
	// Homebrew 目录缺失（「未能复核已装软件」的修复入口，坑 186）：
	// GET 只解析上次失败原因（不跑 brew）；POST 走任务中心，只有用户点击才执行、做完回读复核。
	root.HandleFunc("GET /api/v1/brew/dirs", s.requireAuth(s.handleBrewDirs))
	root.HandleFunc("POST /api/v1/brew/repair-dirs", s.requireAuth(s.handleBrewRepairDirs))
	root.HandleFunc("GET /api/v1/services/{name}/logs", s.requireAuth(s.handleServiceLogs))
	root.HandleFunc("GET /api/v1/services/{name}/logs/stream", s.requireAuth(s.handleServiceLogStream))
	root.HandleFunc("DELETE /api/v1/services/{name}/uninstall", s.requireAuth(s.handleServiceUninstall))

	// Qwen3 TTS 的双模型管理。
	// 刻意用独立前缀而不是 /services/{name}/models：
	// 后者会和上面那条 {name}/{action} 通配路由抢匹配，语义也含糊。
	root.HandleFunc("GET /api/v1/qwen/models", s.requireAuth(s.handleQwenModels))
	root.HandleFunc("POST /api/v1/qwen/model", s.requireAuth(s.handleQwenModelSwitch))
	root.HandleFunc("DELETE /api/v1/qwen/model", s.requireAuth(s.handleQwenModelUnload))

	// ---------- 应用市场 ----------
	root.HandleFunc("GET /api/v1/market", s.requireAuth(s.handleMarketList))
	root.HandleFunc("GET /api/v1/market/{id}/preflight", s.requireAuth(s.handleMarketPreflight))
	root.HandleFunc("POST /api/v1/market/{id}/install", s.requireAuth(s.handleMarketInstall))
	// 一键装 LNMP：比逐个装市场条目多做了四件收尾工作，见 services/lnmp.go
	// 先取版本候选（弹窗用）再开任务：POST 的 body 里就是用户在这次弹窗里的选择。
	root.HandleFunc("GET /api/v1/market/lnmp-options", s.requireAuth(s.handleLNMPOptions))
	root.HandleFunc("POST /api/v1/market/install-lnmp", s.requireAuth(s.handleInstallLNMP))
	root.HandleFunc("POST /api/v1/market/install-phpmyadmin", s.requireAuth(s.handleInstallPhpMyAdmin))
	// 按 TtsVoice 插件的部署契约安装 Qwen3 TTS
	root.HandleFunc("POST /api/v1/market/install-qwentts", s.requireAuth(s.handleInstallQwenTTS))
	// 音色样本接收端 + 带鉴权的反向代理（网站唯一该访问的入口）
	root.HandleFunc("POST /api/v1/market/install-voicereceiver", s.requireAuth(s.handleInstallVoiceReceiver))
	// 多密钥 + 每密钥额度 + 用量统计（receiver v1.6.0），见 api_voice_keys.go
	root.HandleFunc("GET /api/v1/voice/receiver/keys", s.requireAuth(s.handleVoiceKeys))
	root.HandleFunc("POST /api/v1/voice/receiver/keys", s.requireAuth(s.handleVoiceKeyAdd))
	root.HandleFunc("PATCH /api/v1/voice/receiver/keys/{id}", s.requireAuth(s.handleVoiceKeyUpdate))
	root.HandleFunc("DELETE /api/v1/voice/receiver/keys/{id}", s.requireAuth(s.handleVoiceKeyDelete))
	root.HandleFunc("GET /api/v1/voice/receiver/usage", s.requireAuth(s.handleVoiceUsage))
	// 清零用量（管理动作，接收端要求管理密钥，见 handleVoiceUsageReset）
	root.HandleFunc("POST /api/v1/voice/receiver/usage/reset", s.requireAuth(s.handleVoiceUsageReset))
	// 音色来源（receiver v1.5.0）：列表 / 替换 / 删除，见 api_voice_sources.go
	root.HandleFunc("GET /api/v1/voice/receiver/sources", s.requireAuth(s.handleVoiceSources))
	root.HandleFunc("POST /api/v1/voice/receiver/sources", s.requireAuth(s.handleVoiceSourceUpload))
	root.HandleFunc("DELETE /api/v1/voice/receiver/sources", s.requireAuth(s.handleVoiceSourceDelete))
	// 图片去水印 / 物体擦除（IOPaint）
	root.HandleFunc("POST /api/v1/market/install-iopaint", s.requireAuth(s.handleInstallIOPaint))
	root.HandleFunc("GET /api/v1/adoptable", s.requireAuth(s.handleAdoptScan))
	root.HandleFunc("POST /api/v1/adopt", s.requireAuth(s.handleAdopt))
	root.HandleFunc("GET /api/v1/ports/free", s.requireAuth(s.handlePortCandidates))

	// ---------- Docker ----------
	// 两条硬约束决定了下面的形状：
	//
	//  1. **容器名/镜像名/卷名可能含 `/`**（nginx:1.27、library/redis、my_proj_db），
	//     所以标识符必须走尾部通配 `{path...}` —— Go 1.22 的 `{id}` 不跨 `/`。
	//  2. **`{path...}` 只能出现在模式末尾**（写在中间会直接 panic：
	//     "wildcard not at end"）。所以凡是"标识符后面还有子路径或动作"的接口，
	//     一律把标识符挪到查询参数里（logs / inspect / container actions）。
	root.HandleFunc("GET /api/v1/docker/info", s.requireAuth(s.handleDockerInfo))
	root.HandleFunc("GET /api/v1/docker/containers", s.requireAuth(s.handleDockerContainers))
	root.HandleFunc("POST /api/v1/docker/containers", s.requireAuth(s.handleDockerContainerCreate))
	root.HandleFunc("GET /api/v1/docker/containers/logs", s.requireAuth(s.handleDockerContainerLogs))
	root.HandleFunc("GET /api/v1/docker/containers/inspect", s.requireAuth(s.handleDockerContainerInspect))
	root.HandleFunc("POST /api/v1/docker/containers/action", s.requireAuth(s.handleDockerContainerAction))
	root.HandleFunc("DELETE /api/v1/docker/containers/{path...}", s.requireAuth(s.handleDockerContainerRemove))
	root.HandleFunc("GET /api/v1/docker/images", s.requireAuth(s.handleDockerImages))
	root.HandleFunc("POST /api/v1/docker/images/pull", s.requireAuth(s.handleDockerImagePull))
	root.HandleFunc("POST /api/v1/docker/images/prune", s.requireAuth(s.handleDockerImagePrune))
	root.HandleFunc("DELETE /api/v1/docker/images/{path...}", s.requireAuth(s.handleDockerImageRemove))
	root.HandleFunc("GET /api/v1/docker/volumes", s.requireAuth(s.handleDockerVolumes))
	root.HandleFunc("POST /api/v1/docker/volumes/prune", s.requireAuth(s.handleDockerVolumePrune))
	root.HandleFunc("DELETE /api/v1/docker/volumes/{path...}", s.requireAuth(s.handleDockerVolumeRemove))
	root.HandleFunc("GET /api/v1/docker/networks", s.requireAuth(s.handleDockerNetworks))
	root.HandleFunc("POST /api/v1/docker/networks/prune", s.requireAuth(s.handleDockerNetworkPrune))
	root.HandleFunc("DELETE /api/v1/docker/networks/{path...}", s.requireAuth(s.handleDockerNetworkRemove))
	root.HandleFunc("GET /api/v1/docker/compose", s.requireAuth(s.handleDockerComposeProjects))
	root.HandleFunc("POST /api/v1/docker/compose", s.requireAuth(s.handleDockerComposeSave))
	root.HandleFunc("GET /api/v1/docker/compose/{name}", s.requireAuth(s.handleDockerComposeRead))
	root.HandleFunc("POST /api/v1/docker/compose/{name}/actions", s.requireAuth(s.handleDockerComposeAction))
	root.HandleFunc("DELETE /api/v1/docker/compose/{name}", s.requireAuth(s.handleDockerComposeDelete))

	// ---------- 环境与诊断 ----------
	root.HandleFunc("POST /api/v1/system/nginx/test", s.requireAuth(s.handleNginxTest))
	root.HandleFunc("GET /api/v1/system/nginx/status", s.requireAuth(s.handleNginxStatus))
	// nginx 性能参数（宝塔式「性能调整」表单）：读含"真实生效值回读"，保存走
	// 备份 → 改写 nginx.conf → nginx -t → 失败回滚 → reload → 回读（见 priv/nginxtuning.go）。
	root.HandleFunc("GET /api/v1/system/nginx/tuning", s.requireAuth(s.handleNginxTuningGet))
	root.HandleFunc("POST /api/v1/system/nginx/tuning", s.requireAuth(s.handleNginxTuningSave))
	// 手动触发一次 nginx 环境自愈（补 conf.d/vhosts include 与 WebSocket map）。
	// 反向代理报 unknown "connection_upgrade" variable 时的**一键修复**入口。
	root.HandleFunc("POST /api/v1/system/nginx/ensure-env", s.requireAuth(s.handleNginxEnsureEnv))

	// 网站环境的**真实运行时**（nginx / PHP / MySQL）：只看运行体证据（进程/端口/socket），
	// 面板服务记录只回答"归不归面板管"。修的是"nginx 明明在跑却显示未就绪"。
	root.HandleFunc("GET /api/v1/sites/runtime", s.requireAuth(s.handleSitesRuntime))
	root.HandleFunc("POST /api/v1/system/nginx/repair", s.requireAuth(s.handleNginxEnvRepair))

	// 应用界面子路径：探测（只读）与生成 nginx 入口（写配置 + reload）
	root.HandleFunc("GET /api/v1/market/proxies", s.requireAuth(s.handleAppProxyProbe))
	root.HandleFunc("POST /api/v1/market/proxies/apply", s.requireAuth(s.handleAppProxyApply))
	// 从市场卸载（service / installer 两类；纳管的走 DELETE /api/v1/services/{name}）
	root.HandleFunc("DELETE /api/v1/market/{id}", s.requireAuth(s.handleMarketUninstall))
	// 卸载前的**完整**计划（含依赖检测，会真的跑一次 brew uses）：
	// 列表刻意不查依赖（36 条要 15 秒），用户点「卸载」时才按需查（见该 handler 注释）。
	root.HandleFunc("GET /api/v1/market/{id}/uninstall-plan", s.requireAuth(s.handleMarketUninstallPlan))
	// 一键建站（Typecho / WordPress …）：建目录 + 建库 + 建站点 + 伪静态
	root.HandleFunc("POST /api/v1/market/{id}/install-site", s.requireAuth(s.handleSiteAppInstall))

	// Docker 镜像加速源（换源）：现状 / 上次检测的缓存 / 检测可用性 / 保存并重启
	//
	// `/mirrors/cached` 与 `/mirrors` 都是字面量模式，Go 1.22 的 ServeMux
	// 按"最具体"匹配，不会互相抢。缓存接口是**只读**的：页面靠它立即渲染，
	// 探测只由用户点「检测」触发（见 api_docker_mirrors.go 的文件头说明）。
	root.HandleFunc("GET /api/v1/docker/mirrors", s.requireAuth(s.handleDockerMirrors))
	root.HandleFunc("GET /api/v1/docker/mirrors/cached", s.requireAuth(s.handleDockerMirrorCached))
	root.HandleFunc("POST /api/v1/docker/mirrors/probe", s.requireAuth(s.handleDockerMirrorProbe))
	root.HandleFunc("POST /api/v1/docker/mirrors", s.requireAuth(s.handleDockerMirrorSave))

	// ---------- 在线升级 ----------
	// 升级会替换二进制并重启面板，所以每一个写操作都必须经过鉴权；
	// 执行升级额外要求进程是 root（见 handleUpgradeApply）。
	root.HandleFunc("GET /api/v1/system/upgrade", s.requireAuth(s.handleUpgradeStatus))
	root.HandleFunc("POST /api/v1/system/upgrade/check", s.requireAuth(s.handleUpgradeCheck))
	root.HandleFunc("POST /api/v1/system/upgrade/stage", s.requireAuth(s.handleUpgradeStage))
	root.HandleFunc("POST /api/v1/system/upgrade/upload", s.requireAuth(s.handleUpgradeUpload))
	root.HandleFunc("POST /api/v1/system/upgrade/apply", s.requireAuth(s.handleUpgradeApply))
	root.HandleFunc("POST /api/v1/system/upgrade/dismiss", s.requireAuth(s.handleUpgradeDismiss))

	// phpMyAdmin：只有登录面板的人才能进（路径在安全后缀之下）
	root.HandleFunc("/phpmyadmin/", s.handlePhpMyAdmin)
	root.HandleFunc("/phpmyadmin", s.handlePhpMyAdmin)

	// ---------- 图片压缩（应用「图片压缩（libvips）」的能力入口）----------
	// engine 是只读探测（引擎在不在 + 目录里有几张图），compress 走任务中心。
	root.HandleFunc("GET /api/v1/images/engine", s.requireAuth(s.handleImageEngine))
	root.HandleFunc("POST /api/v1/images/compress", s.requireAuth(s.handleImageCompress))

	// 默认站点（建 www/localhost + 重写 000-default.conf，去掉 /_panel）。
	// 面板启动时会**自动**做一次（见 api_default_site_boot.go）—— 这两个接口是
	// 给"看一眼状态 / 自动没成功时手动重试 / 还没有 nginx 时只装 nginx"用的。
	root.HandleFunc("GET /api/v1/system/default-site", s.requireAuth(s.handleDefaultSiteStatus))
	root.HandleFunc("POST /api/v1/system/default-site/apply", s.requireAuth(s.handleDefaultSiteApply))

	root.HandleFunc("GET /api/v1/settings", s.requireAuth(s.handleGetSettings))
	root.HandleFunc("POST /api/v1/settings", s.requireAuth(s.handleSaveSettings))
	// 上传与执行限制（nginx client_max_body_size + PHP upload/post/memory/执行时间）：
	// GET 回读生效值，POST 校验后走任务中心应用（写配置 → reload nginx → 重启 php-fpm）。
	root.HandleFunc("GET /api/v1/settings/upload-limits", s.requireAuth(s.handleGetUploadLimits))
	root.HandleFunc("POST /api/v1/settings/upload-limits", s.requireAuth(s.handleSaveUploadLimits))
	// 「大文件上传自检」：磁盘空间 / nginx 临时目录可写性 / error_log 相关行。
	// 用户报 500 时用它一眼看出卡在哪（413 与 500 是两件事，见 handler 注释）。
	root.HandleFunc("GET /api/v1/settings/upload-doctor", s.requireAuth(s.handleUploadDoctor))

	// ---------- 导航页（sun-panel 风格图标网格首页，见 api_nav.go）----------
	//
	// 面板内页面 `#/nav` 用这组接口，全部 requireAuth；写操作都进审计。
	// 独立的匿名别名页（GET /nav/）注册在下面的外层 mux，不在这里 ——
	// 它必须在不带安全后缀时也能打开，而这里的所有路径都在后缀闸门之后。
	root.HandleFunc("GET /api/v1/nav/tree", s.requireAuth(s.handleNavTree))
	root.HandleFunc("GET /api/v1/nav/groups", s.requireAuth(s.handleNavGroupsList))
	root.HandleFunc("POST /api/v1/nav/groups", s.requireAuth(s.handleNavGroupCreate))
	root.HandleFunc("POST /api/v1/nav/groups/reorder", s.requireAuth(s.handleNavGroupReorder))
	root.HandleFunc("PUT /api/v1/nav/groups/{id}", s.requireAuth(s.handleNavGroupUpdate))
	root.HandleFunc("DELETE /api/v1/nav/groups/{id}", s.requireAuth(s.handleNavGroupDelete))
	root.HandleFunc("GET /api/v1/nav/items", s.requireAuth(s.handleNavItemsList))
	root.HandleFunc("POST /api/v1/nav/items", s.requireAuth(s.handleNavItemCreate))
	root.HandleFunc("POST /api/v1/nav/items/reorder", s.requireAuth(s.handleNavItemReorder))
	root.HandleFunc("PUT /api/v1/nav/items/{id}", s.requireAuth(s.handleNavItemUpdate))
	root.HandleFunc("DELETE /api/v1/nav/items/{id}", s.requireAuth(s.handleNavItemDelete))
	root.HandleFunc("GET /api/v1/nav/export", s.requireAuth(s.handleNavExport))
	root.HandleFunc("POST /api/v1/nav/import", s.requireAuth(s.handleNavImport))
	// 导航页的本地图标（上传本地图片 + 在已上传的里面挑）：
	// 读文件走公开的 /nav/icons/（在下面的外层 mux，导航页是匿名首页），
	// 这里这三条是**写/列**——必须登录面板。
	root.HandleFunc("GET /api/v1/nav/icons", s.requireAuth(s.handleNavIconsList))
	root.HandleFunc("POST /api/v1/nav/icons", s.requireAuth(s.handleNavIconUpload))
	root.HandleFunc("DELETE /api/v1/nav/icons/{name}", s.requireAuth(s.handleNavIconDelete))
	// 导航页外观设置（用户 2026-09-18 要求：标题可改 / 自定义背景图 / 指定主题色）：
	// 背景图上传走独立入口（上限 8 MiB，图标是 512 KiB），读取复用公开的 /nav/icons/。
	root.HandleFunc("POST /api/v1/nav/background", s.requireAuth(s.handleNavBackgroundUpload))
	root.HandleFunc("GET /api/v1/nav/settings", s.requireAuth(s.handleNavSettingsGet))
	root.HandleFunc("POST /api/v1/nav/settings", s.requireAuth(s.handleNavSettingsSave))

	// ---------- 前端静态资源 ----------
	// 必须注册在 handleStatic 之前 —— 后者是 SPA 回落，任何未知路径都会
	// 返回面板自己的 index.html（**状态码还是 200**），
	// 也就是说漏注册不会报错，只会让用户看到一个"打不开的应用页面"。
	root.HandleFunc("/", s.handleStatic)

	// ---------- 安全后缀闸门（见 panelGate 的说明）----------
	//
	// 结构：外层 mux 只放**公开**入口，其余全部交给闸门 + 内层 mux。
	// 应用界面代理（/iopaint/ 等）必须挂在外层 —— 它们是给用户浏览器直接访问的，
	// 不该要求先知道面板后缀。第一版把它们注册在内层，结果闸门先拦下来 →
	// 应用入口全变成 404（真机验证时抓到）。
	outer := http.NewServeMux()
	s.registerAppProxy(outer)
	// 面板自带的「导航页」匿名别名入口（GET /nav/，见 api_nav.go）。
	// 与应用界面代理同层：都必须在安全后缀之外，用户才能直接当浏览器首页用。
	s.registerNavPage(outer)
	outer.Handle("/", s.panelGate(root))

	return s.accessControl(outer)
}

// ---------- 静态资源 ----------

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeErr(w, http.StatusNotFound, "接口不存在")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	// 缓存策略按文件类型区分。
	//
	// 为什么 JS/CSS 也不能长缓存：面板是"就地升级"的（替换二进制即可），
	// 文件名里没有内容指纹，浏览器缓存了旧版 JS 就会出现
	// "后端已修好、页面还是老行为"的诡异现象 —— 开发过程中确实因此
	// 浪费过一轮排查（工具栏一直显示 null，实际代码早已修复）。
	// 用 no-cache（而非 no-store）：仍允许本地缓存，但每次都要向服务端校验，
	// 配合 ETag 兼顾性能与正确性。
	switch {
	case p == "index.html", strings.HasSuffix(p, ".js"), strings.HasSuffix(p, ".css"):
		w.Header().Set("Cache-Control", "no-cache")
	default:
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	b, err := fs.ReadFile(s.static, p)
	if err != nil {
		// SPA 回落：前端路由（如 /sites）也返回 index.html
		b, err = fs.ReadFile(s.static, "index.html")
		if err != nil {
			writeErr(w, http.StatusNotFound, "页面不存在")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
		return
	}
	w.Header().Set("Content-Type", contentTypeByExt(p))
	_, _ = w.Write(b)
}

func contentTypeByExt(p string) string {
	switch {
	case strings.HasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(p, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(p, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(p, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".ico"):
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

// ---------- 接口辅助 ----------

// writeJSON 统一 JSON 输出。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ok 输出成功响应。
func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

// fail 输出失败响应。
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "msg": msg})
}

func writeErr(w http.ResponseWriter, code int, msg string) { fail(w, code, msg) }

// decode 解析请求体 JSON，并限制大小防止内存滥用。
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求格式错误: %w", err)
	}
	return nil
}

// handlePing 用于安装脚本与外部探活判断面板是否已就绪。
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	count, _ := s.Auth.CountUsers(r.Context())
	ok(w, map[string]any{
		"app":       "zizpanel",
		"version":   version.Version,
		"full":      version.Full(),
		"uptime":    int64(time.Since(s.startAt).Seconds()),
		"installed": count > 0,
	})
}

// handleHealth 是轻量健康检查：不查数据库、不做耗时操作。
//
// 与 ping 分开：安装脚本与监控只需要知道"服务能否响应"，
// 健康检查必须永远快速返回，可以被高频轮询。
//
// version 用 version.Version（纯版本号），**不要用 Full()**：
// 升级看门狗把这里的 version 与清单里的版本号做字符串比较，而 Full() 在正式发布
// 的二进制上会带 git commit（"0.3.1+9a304af"），于是**新版正常服务却匹配不上**，
// 看门狗判定"启动失败"并回滚 —— 真机上连续回滚了三次，面板一直升不上去。
//
// 这个死结的麻烦之处在于"自举"：看门狗脚本是**升级前那个旧版本**生成的，
// 所以改新版本里的匹配逻辑救不了当次升级；必须让健康检查返回旧版断言所期望的形式。
// 两侧都做：这里返回纯版本号（治本），watchdog 那边也接受 +commit 后缀（兜底）。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"status": "ok", "version": version.Version})
}

// handleSetupStatus 告知前端是否需要走首次初始化向导。
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	has, err := s.Auth.HasAnyUser(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取账号信息失败")
		return
	}
	ok(w, map[string]any{"needs_setup": !has, "version": version.Full()})
}

type setupReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleSetup 首次创建管理员账号。已有账号时永久拒绝，防止被用来重置密码。
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	has, err := s.Auth.HasAnyUser(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取账号信息失败")
		return
	}
	if has {
		fail(w, http.StatusForbidden, "面板已完成初始化，如需重置密码请在终端执行：zizpanel reset-password")
		return
	}
	var req setupReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.Auth.CreateUser(r.Context(), req.Username, req.Password, true)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "setup", u.Username, "创建管理员账号", true, "")
	tok, exp, err := s.Auth.NewSession(r.Context(), u.ID, s.clientIP(r), r.UserAgent())
	if err != nil {
		fail(w, http.StatusInternalServerError, "创建会话失败")
		return
	}
	s.setSessionCookies(w, r, tok, exp)
	ok(w, map[string]any{
		"user":       s.userView(u),
		"expires_at": exp.Format(time.RFC3339),
		"first_time": true,
	})
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ip := s.clientIP(r)
	if !s.loginSvc.allow(ip, req.Username) {
		s.audit(r, "login", req.Username, "登录过于频繁被限流", false, "")
		fail(w, http.StatusTooManyRequests, "尝试过于频繁，请稍后再试")
		return
	}
	res, err := s.Auth.Login(r.Context(), req.Username, req.Password, req.Code, ip, r.UserAgent())
	if err != nil {
		s.loginSvc.fail(ip, req.Username)
		s.audit(r, "login", req.Username, err.Error(), false, "")
		code := http.StatusUnauthorized
		if errors.Is(err, auth.ErrAccountLocked) {
			code = http.StatusLocked
		}
		fail(w, code, err.Error())
		return
	}
	if res.NeedTOTP {
		// 密码正确但还需要验证码：下发中间令牌，不建会话
		s.loginSvc.success(ip, req.Username)
		ok(w, map[string]any{
			"need_totp": true,
			"challenge": s.Auth.IssueTOTPChallenge(res.User.ID),
		})
		return
	}
	s.loginSvc.success(ip, req.Username)
	s.setSessionCookies(w, r, res.Token, res.ExpiresAt)
	s.audit(r, "login", res.User.Username, "登录成功", true, "")
	ok(w, map[string]any{
		"user":       s.userView(res.User),
		"expires_at": res.ExpiresAt.Format(time.RFC3339),
	})
}

type totpLoginReq struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

func (s *Server) handleLoginTOTP(w http.ResponseWriter, r *http.Request) {
	var req totpLoginReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ip := s.clientIP(r)
	if !s.loginSvc.allow(ip, "totp") {
		fail(w, http.StatusTooManyRequests, "尝试过于频繁，请稍后再试")
		return
	}
	uid, err := s.Auth.ParseTOTPChallenge(req.Challenge)
	if err != nil {
		s.loginSvc.fail(ip, "totp")
		fail(w, http.StatusUnauthorized, "验证会话已过期，请重新登录")
		return
	}
	res, err := s.Auth.CompleteTOTP(r.Context(), uid, req.Code, ip, r.UserAgent())
	if err != nil {
		s.loginSvc.fail(ip, "totp")
		s.audit(r, "login", fmt.Sprint(uid), "两步验证失败", false, "")
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.loginSvc.success(ip, "totp")
	s.setSessionCookies(w, r, res.Token, res.ExpiresAt)
	s.audit(r, "login", res.User.Username, "两步验证通过，登录成功", true, "")
	ok(w, map[string]any{
		"user":       s.userView(res.User),
		"expires_at": res.ExpiresAt.Format(time.RFC3339),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := s.sessionToken(r); tok != "" {
		_ = s.Auth.Revoke(r.Context(), tok)
	}
	s.clearSessionCookies(w)
	ok(w, map[string]any{"msg": "已退出登录"})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	count, _ := s.Auth.CountUsers(r.Context())
	stats, _ := s.Store.Stats(r.Context())
	ok(w, map[string]any{
		"user":    s.userView(u),
		"version": version.Full(),
		"config": map[string]any{
			"www_root":    s.Cfg.WWWRoot,
			"listen":      s.Cfg.Listen,
			"access_mode": s.Cfg.AccessMode,
			"tls":         s.Cfg.TLSEnable,
			"data_dir":    s.Cfg.DataDir,
			"user_count":  count,
			"trust_proxy": s.Cfg.TrustProxy,
			"started_at":  s.startAt.Format(time.RFC3339),
			"server_time": time.Now().Format("2006-01-02 15:04:05"),
			"go_version":  goVersion(),
			"install_id":  s.Cfg.InstallID,
			"panel_path":  s.Cfg.DataDir,
			// 面板入口（含安全后缀）：前端用它拼所有"打开"地址，
			// 这样无论从 127.0.0.1:8443、局域网还是隧道访问，入口都是**同一个**。
			"panel_entry":  s.PanelEntryPath(),
			"panel_suffix": s.Cfg.PanelSuffix,
		},
		"counts": stats,
	})
}

func (s *Server) userView(u *auth.User) map[string]any {
	return map[string]any{
		"id":            u.ID,
		"username":      u.Username,
		"is_admin":      u.IsAdmin,
		"totp_enabled":  u.TOTPEnabled,
		"last_login_at": u.LastLoginAt,
		"last_login_ip": u.LastLoginIP,
	}
}

// ---------- 安全后缀（安全入口） ----------

// panelGate 把面板的界面与接口挪到 /<suffix>/ 之下，其余一律 404。
//
// 为什么这么做（用户明确要求，模仿宝塔的"安全入口"）：
// 面板直接开在 `https://<ip>:8443/` 上时，任何扫到端口的人都能看到登录页，
// 而且像 `/_panel/` 这种固定路径一旦被 nginx 暴露到 80 端口，就更容易被发现。
// 加一个随机后缀后，不知道后缀的人连登录页都看不到。
//
// 为什么用"闸门"而不是把所有路由都加上前缀：
//
//	· 内部路由一个字都不用改（少一处改动就少一类回归）；
//	· 应用界面代理（`/iopaint/` 等）注册在**外层**，不经过这里，保持公开
//	  —— 它们是给用户浏览器直接访问的，本来就不该要求先登录面板；
//	· 升级看门狗的健康检查留在根路径（`/api/v1/health`）：它是升级流程的契约，
//	  写在后缀里会让"升级过程中改后缀"变成一次断线。
func (s *Server) panelGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 后缀**每次请求现读**，不在启动时固化：
		// 用户在「面板设置」里改后缀后必须立刻生效，
		// 否则会处于"设置说改了、实际还能从老路径进"的状态（比不改更糟）。
		suffix := strings.Trim(s.Cfg.PanelSuffix, "/")
		if suffix == "" {
			// 未启用（本地开发/测试，或用户显式清空）：保持原样
			next.ServeHTTP(w, r)
			return
		}
		prefix := "/" + suffix
		p := r.URL.Path
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(p, prefix)
			if r2.URL.Path == "" {
				r2.URL.Path = "/"
			}
			next.ServeHTTP(w, r2)
			return
		}
		// 升级看门狗与外部探活用的公开接口：留在根路径（不带任何敏感信息）
		if p == "/api/v1/health" || p == "/api/v1/ping" {
			next.ServeHTTP(w, r)
			return
		}
		// 其余一律当作"不存在"：不透露这里有面板，也不给任何跳转提示
		writeErr(w, http.StatusNotFound, "404 page not found")
	})
}

// PanelEntryPath 返回面板入口路径（含后缀），供界面与 CLI 显示。
func (s *Server) PanelEntryPath() string {
	suffix := strings.Trim(s.Cfg.PanelSuffix, "/")
	if suffix == "" {
		return "/"
	}
	return "/" + suffix + "/"
}

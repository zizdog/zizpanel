// Package services 实现网站与应用的统一服务管理。
//
// 核心抽象：
//
//	Service（一条注册记录）
//	  └── Driver（执行者）
//	        ├── NativeDriver  launchd / brew services（裸装）
//	        └── DockerDriver  Docker 容器（可选，装了 Docker 才有）
//
// 设计要点：
//
//  1. 面板把服务分成两类：**纳管（adopt）** 与 **托管（managed）**。
//     纳管 = 你原来就装好的服务（比如 com.zizdog.qwen3tts），面板只做启停/日志/健康检查；
//     托管 = 面板通过应用市场安装的服务，面板负责完整生命周期（含卸载）。
//     这个区分很重要：对纳管服务做"卸载"会把用户自己的东西删掉。
//
//  2. 「运行状态」永远以系统真实状态为准，不信任数据库里的 enabled 字段。
//     数据库只记录"这个服务是什么、怎么管"，状态每次实时查询。
//
//  3. 日志读取走 tail 语义（读末尾 N 行），而不是把整个文件加载进内存 ——
//     服务日志动辄几百 MB，全量读会拖死面板。
package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RuntimeUnavailableError 标记"本机没有可用的容器运行时，命令根本没执行"。
//
// 它的 Error() **原样返回底层文案**（用户已经看到的那句话不变），只是让 HTTP 层
// 能用 errors.As 认出这种情况，补上更精准的说明（"没有停止任何容器"、
// "可以只删记录"）—— 见 web 层 handleServiceUninstall。
//
// 为什么需要它（2026-09-17 真机缺陷）：用户把本机 Docker/Colima 全删掉后，
// 面板里 managed=true 的 compose 记录走「卸载」只会报一句"未找到 docker compose
// 命令"，用户既不知道"东西其实一点没动"，也不知道"记录还可以只删掉"。
type RuntimeUnavailableError struct{ Err error }

func (e *RuntimeUnavailableError) Error() string { return e.Err.Error() }
func (e *RuntimeUnavailableError) Unwrap() error { return e.Err }

// MarkRuntimeUnavailable 给错误打上"运行时不可用"的标记；nil 原样返回。
func MarkRuntimeUnavailable(err error) error {
	if err == nil {
		return nil
	}
	return &RuntimeUnavailableError{Err: err}
}

// IsRuntimeUnavailable 判断错误是否属于"运行时不可用，命令根本没执行"。
func IsRuntimeUnavailable(err error) bool {
	var e *RuntimeUnavailableError
	return errors.As(err, &e)
}

// Kind 是服务的类型。
type Kind string

const (
	// KindNative 系统原生服务：由 launchd 托管（launchctl / brew services）。
	KindNative Kind = "native"
	// KindDocker 单个 Docker 容器。
	KindDocker Kind = "docker"
	// KindCompose 由 docker compose 管理的一组容器。
	KindCompose Kind = "compose"
	// KindColima 是容器运行时本身（Colima 起的 Linux 虚拟机）。
	// 它必须独立于 KindDocker：Docker 引擎挂掉时，其它 docker 驱动都会因为
	// "Docker 不可用"而构造失败，而这恰恰是最需要能操作运行时的时刻。
	KindColima Kind = "colima"
)

// Service 是一条服务注册记录。
type Service struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Kind        Kind   `json:"kind"`
	Category    string `json:"category"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Port        int    `json:"port"`

	// native
	LaunchLabel string `json:"launch_label"`
	PlistPath   string `json:"plist_path"`
	WorkDir     string `json:"work_dir"`
	StartCmd    string `json:"start_cmd"`

	// docker / compose
	Container   string `json:"container"`
	ComposeFile string `json:"compose_file"`
	Image       string `json:"image"`

	// 通用
	HealthURL    string `json:"health_url"`
	HealthExpect string `json:"health_expect"`
	LogPath      string `json:"log_path"`
	Autostart    bool   `json:"autostart"`
	Enabled      bool   `json:"enabled"`
	Managed      bool   `json:"managed"` // true=面板安装（可卸载），false=仅纳管

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// State 是服务的实时运行状态。
type State struct {
	Running bool   `json:"running"`
	Status  string `json:"status"` // running / stopped / error / unknown / not-installed
	PID     int    `json:"pid"`
	Detail  string `json:"detail"`
	// ExitCode 仅在 launchd 报告异常时有效
	ExitCode int `json:"exit_code"`
	// Endpoint 实际访问地址（若已知）
	Endpoint string `json:"endpoint"`
	// Warning 是"操作完成但结果没被确认"的如实说明（例如启动请求已发出、8 秒内
	// 还没看到它跑起来）。界面必须显示它，否则用户会把"没确认"当成"已经好了"。
	Warning string `json:"warning,omitempty"`
}

// Health 是健康检查结果。
type Health struct {
	Checked   bool   `json:"checked"`
	OK        bool   `json:"ok"`
	URL       string `json:"url"`
	Code      int    `json:"code"`
	Latency   int64  `json:"latency_ms"`
	Message   string `json:"message"`
	CheckedAt string `json:"checked_at"`
}

// Driver 是服务操作接口。所有实现都必须满足：
//   - 幂等：重复 start 已运行的服务不报错
//   - 不阻塞：调用方给出 ctx，实现必须尊重超时
//   - 状态真实：Status 必须查询系统，不读缓存
type Driver interface {
	// Kind 返回驱动类型。
	Kind() Kind
	// Status 查询真实运行状态。
	Status(ctx context.Context) (State, error)
	// Start 启动服务。
	Start(ctx context.Context) error
	// Stop 停止服务。
	Stop(ctx context.Context) error
	// Restart 重启服务。
	Restart(ctx context.Context) error
	// Logs 读取末尾 lines 行日志。follow 为 true 时返回一个持续输出的通道。
	Logs(ctx context.Context, lines int) (string, error)
	// LogStream 返回持续输出日志的通道（用于 SSE）。
	LogStream(ctx context.Context) (<-chan string, error)
	// Health 做一次健康检查。
	Health(ctx context.Context) Health
	// Uninstall 移除服务（仅对托管服务调用）。
	Uninstall(ctx context.Context) error
}

// Manager 是服务管理器。
type Manager struct {
	repo *Repository
	opt  Options
	// qwenPortOverride 仅供测试：把模型状态查询指向一个指定端口。
	// 没有它的话，单元测试会打到本机真实运行的 Qwen 服务上（8880）——
	// 实测就这么发生过：断言"服务不可用时应降级"的测试，因为本机恰好
	// 有另一个项目在跑 Qwen 而失败，测出来的根本不是被测代码的行为。
	qwenPortOverride int
	// baseEnvCLTProbe 仅供测试：替换"命令行开发者工具装没装"的探测。
	// 默认实现会执行 /usr/bin/xcode-select -p（见 cltInstalled），结论会随
	// 测试机是否装了 CLT 而变 —— 那正是"单测不许碰真实环境"禁止的
	// （见 baseenv.go 的 BaseEnvStatus）。
	baseEnvCLTProbe func(ctx context.Context) bool
	// launchdDirsOverride 仅供测试：替换 launchd 里找 plist 的目录集合。
	//
	// 没有它，单测会去读真机 /Library/LaunchDaemons —— 本机恰好把 php@8.2
	// 装成了系统级守护进程，于是"该服务尚未注册到 launchd"这个测试前提
	// 在真机上不成立，断言的结论随测试机的安装状态漂移（2026-09-18 真踩到：
	// 同一个提交在空机器上绿、在本机红）。
	launchdDirsOverride []string
	// colimaChownOverride 仅供测试：替换"把 ~/.colima 交还真实用户"的 chown 调用。
	// 没有它，测这条链路要么真的 chown（单测不许动真实家目录），要么只能测到"没跑"。
	colimaChownOverride func(user, root string) (changed, total int, err error)
	// brewInstalledProbe 仅供测试：替换"本机装了哪些 brew formula（含版本）"的探测。
	//
	// 没有它，卸载计划的单测会去跑真实的 `brew list --versions`（违反"单测不许
	// 碰真实服务"），而且结论会随开发机装没装 php/nginx 而漂 —— 本机恰好装着一堆。
	brewInstalledProbe func(ctx context.Context) map[string]string
	// brewUsesProbe 仅供测试：替换 `brew uses --installed <formula>` 的探测。
	// 返回 (依赖它的已装包, 这次查询是否真的成功)，后者决定计划里写"已检查"还是"未检查"。
	brewUsesProbe func(ctx context.Context, formula string) ([]string, bool)
	// dockerPSProbe 仅供测试：替换"列出正在运行的容器"的探测。
	// 返回 (容器名, 这次查询是否成功)；后者决定依赖报告写"未检查/未列出"。
	dockerPSProbe func(ctx context.Context) ([]string, bool)
	// siteRefsCache/siteRefsLoaded 缓存注入的站点列表：市场列表会对十几个条目
	// 各算一次计划，站点列表在一次请求内只该读一次。
	siteRefsCache  []SiteRef
	siteRefsLoaded bool
	// phpRestartOverride 仅供测试：替换"重启某个 PHP 服务"这一步，
	// 以便验证"无权重启时如实降级"这条分支（真机上要 root 才复现）。
	phpRestartOverride func(ctx context.Context, formula string) error
	// mirrorProbeOverride 仅供测试：替换 brew 镜像探测（它会发真实网络请求）。
	// 没有它的话，每个碰 brewEnv 的单测都会去访问阿里云/中科大，既慢又依赖外网 ——
	// 违反"单测不许碰真实服务"。
	mirrorProbeOverride func(ctx context.Context, probeFormula string) (apiDomain, bottleDomain string)
	// lnmpInstalledProbe 仅供测试：替换"某个 LNMP formula 在本机装没装"的探测。
	//
	// 没有它，候选接口（GET /market/lnmp-options）的单测会去跑真实的
	// `brew list` 与 /Library/LaunchDaemons（违反"单测不许碰真实服务"），
	// 结论也会随开发机装没装东西而漂（本机恰好装着 php@8.2 的系统守护进程）。
	lnmpInstalledProbe func(formula string) bool
	// mirrorProbeCache 缓存探测结果：一次会话只探一次。
	//
	// 为什么必须有：brewEnv 会被**每次 brew 调用**用到（brew list --versions、
	// 每个 install …），而一次 LNMP 安装有几十次调用。每次探测最多 4 秒 × 3 个候选，
	// 加起来就是用户感受到的"怎么这么慢" —— 而且这是在**安装之前**白白等掉的。
	mirrorProbeCache map[string]string
	// portCheckOverride 仅供测试：替换 lsof 端口探测，返回"是否占用 + 占用者"。
	//
	// 没有它，"端口被别的进程占用"这条分支只能依赖测试机上恰好有东西在听 ——
	// 结论随机器而变，而且真的去查了系统端口（违反"单测不许碰真实服务"）。
	portCheckOverride func(port int) (inUse bool, holders []string, err error)
	// depExecOverride 仅供测试：替换"基础依赖能否真的跑起来"的执行动作。
	// 没有它，VerifyBaseDependencies 的单测会去执行测试机上真实的 ffmpeg，
	// 结论随机器而变（见 basedep.go）。
	depExecOverride func(path string) string
	// mysqlAdminOverride 仅供测试：替换"连 MySQL / 改 root 口令"的动作。
	// 没有它，凭据闭环的单测会去连本机真实运行的 MySQL —— 那既违反
	// "单测不许碰真实服务"，结论也会随开发机装没装 MySQL 而变。
	mysqlAdminOverride mysqlAdmin
	// mysqlProbeWaitOverride 仅供测试：把"等 MySQL 起来"的时长调小。
	// 否则"服务没起来"这条分支的测试要真的等 30 秒。
	mysqlProbeWaitOverride time.Duration
	// mirrorFileProbeOverride 仅供测试：替换"镜像上有没有这个**具体文件**"的 HEAD 探测。
	// 没有它，碰镜像文件探测的单测会去访问真实镜像站（违反"单测不许碰真实服务"）。
	mirrorFileProbeOverride func(ctx context.Context, url string) (int64, error)
	// iopaintWaitPortOverride 仅供测试：替换"等 IOPaint 端口就绪"。
	// 真机实现要轮询 180 秒，单测既不可能真等、也不该真去开一个端口。
	iopaintWaitPortOverride func(ctx context.Context, port int, timeout time.Duration) bool
	// iopaintFetchOverride 仅供测试：替换真实的 HTTP 下载（单测不许联网）。
	// 有了它才能构造"下载停滞 / 被截断 / md5 不符"这些真机上很难复现的场景。
	iopaintFetchOverride func(ctx context.Context, url, dest string, onProgress fetchProgressFunc) error
	// iopaintWeightTimeoutOverride / iopaintStallTimeoutOverride 仅供测试：
	// 把"30 分钟总超时 / 90 秒停滞判定"缩短，好让"超时=如实失败，而不是永远 running"
	// 这条约束能在毫秒级被验证，而不是靠人工等半小时。
	iopaintWeightTimeoutOverride time.Duration
	iopaintStallTimeoutOverride  time.Duration
	// minifluxExecOverride 仅供测试：替换 Miniflux 安装流程里"以真实用户身份执行命令"
	// 的动作（pg_isready / psql / miniflux -migrate）。没有它，单测会去执行真实的
	// PostgreSQL 客户端与 miniflux 二进制（违反"单测不许碰真实服务"），结论也会
	// 随开发机装没装 postgresql@17 而变化。
	minifluxExecOverride func(ctx context.Context, timeout time.Duration, stdin, name string, args ...string) (string, error)
	// minifluxHTTPOverride 仅供测试：替换 /healthz 与 /v1/me 的 HTTP 探测。
	// 没有它，单测会真的去连本机 8087（开发机上可能恰好有别的服务在听）。
	minifluxHTTPOverride func(ctx context.Context, url, user, password string) (int, error)
	// minifluxHealthTimeoutOverride 仅供测试：把"等 /healthz 就绪"的 60 秒缩短。
	// 否则"健康检查失败=如实报错"这条分支的单测要真的轮询满 60 秒。
	minifluxHealthTimeoutOverride time.Duration
	// brewSourceRunOverride 仅供测试：替换"用指定源跑一次 brew install"。
	//
	// 没有它，"失败即换源"的单测会去执行真实的 brew install（违反"单测不许碰真实
	// brew"），而且 python@3.11 那种"镜像上是 0 字节瓶"的场景根本没法在测试机上
	// 复现。有了它才能构造"第一个源校验失败 → 换源 → 第二个源成功"这类序列。
	brewSourceRunOverride func(ctx context.Context, timeout time.Duration, src brewInstallSource, args ...string) (string, error)
	// brewCacheDirOverride 仅供测试：替换 brew 的下载缓存目录
	// （默认是真实用户家目录下的 ~/Library/Caches/Homebrew/downloads）。
	// 没有它，碰"清缓存只删相关条目"的单测会去读/删开发机真实用户的缓存 ——
	// 那正是 AGENTS.md 第三节禁止的"单测碰真实用户家目录"。
	brewCacheDirOverride string
	// brewCacheRemoveOverride 仅供测试：替换"以真实用户身份删除缓存条目"的动作。
	// 没有它，测删除路径要么真的删文件、要么真的起 sudo。
	brewCacheRemoveOverride func(ctx context.Context, paths []string) error
	// dockerBinProbeOverride 仅供测试：替换"colima 二进制在不在"的探测。
	//
	// 没有它，DockerRuntimeStatus 的单测结论会随开发机装没装 colima 而变
	// （开发机装了、CI 没装，"没装"那条分支就永远测不到）——
	// 这正是"单测不许碰真实环境"要排除的（AGENTS.md 第三节）。
	// 注入的函数返回 ("", error) 表示没装，返回 ("/path/colima", nil) 表示装了。
	dockerBinProbeOverride func(command string) (string, error)
	// dockerVersionOverride 仅供测试：替换"连一次 Docker API 问引擎版本"的动作。
	//
	// 没有它，单测要么真去连开发机上的 Docker（如果恰好装了，结论随机器变），
	// 要么只能测到"连不上"一态；返回非空字符串 = 引擎在跑，返回 "" = 连不上。
	dockerVersionOverride func(ctx context.Context, sock string) string
}

// Options 是管理器需要的环境信息。
type Options struct {
	// HelperBin 是提权助手路径（用于需要 root 的操作）
	HelperBin string
	// BrewBin 是 brew 路径（brew services 类服务需要）
	BrewBin string
	// MirrorBase 是应用包镜像基址（来自 Config.MirrorBase）。
	// 非空 = 镜像是**优先来源**：安装前先检查资源在不在，在就从镜像下、
	// 缺件或不可达则回落到公网源（见 mirror.go）；空 = 关闭镜像（应急用）。
	MirrorBase string
	// MirrorProbeSeconds 是镜像资源探测超时（秒，来自 Config.MirrorProbeSeconds）；
	// <=0 按 4 秒处理。
	MirrorProbeSeconds int
	// OfflineOnly 为 true 时进入**仅走 NAS（离线）模式**（来自
	// Config.OfflineOnly）：各安装器必须先用 MirrorOfflineOnly(ctx) 判断，
	// **禁止任何外网回落**，缺资源就明确失败。见 mirror.go 的说明。
	OfflineOnly bool
	// DockerSocket 是 Docker socket 路径，为空表示 Docker 不可用
	DockerSocket string
	// UserHome 是真实用户家目录（用于找 LaunchAgents 与日志）
	UserHome string
	// UserName 是真实用户名
	UserName string
	// UID 是真实用户 uid（launchd gui/<uid> 域需要）
	UID int
	// WorkDir 是面板工作目录（compose 文件放在 <WorkDir>/compose 下）
	WorkDir string
	// LookPath 覆盖"命令是否存在"的探测，仅供测试注入（nil = 用默认实现）。
	//
	// 为什么需要它：基础依赖探测（basedep.go）默认会看真实文件系统，
	// 而"这台机器有没有 ffmpeg"不该决定单测的结论 ——
	// 开发机装了、CI 没装，"缺/不缺"两条路径就会有一条测不到。
	LookPath func(command string) (string, error)

	// ---------- MySQL root 凭据闭环 ----------
	//
	// 为什么放在 Options 里：安装流程要知道"面板当前持有的 MySQL 凭据是什么"，
	// 并在设置完口令后把它写回 config.json。这两件事属于**面板配置**的能力
	// （由 web 层注入），services 不自己造一份凭据存储 ——
	// 两份存储必然不同步，而"面板以为的口令"与"MySQL 实际的口令"不一致
	// 正是 2026-09-16 mini 那次把面板锁在门外的根因。
	//
	// MySQLCredential 读面板当前持有的凭据（nil = 未接入，安装流程会如实说明并跳过闭环）
	MySQLCredential func() MySQLCredential
	// SetMySQLRootPassword 把新口令写回 config.json（nil = 未接入；
	// 它返回错误必须被当成严重问题：MySQL 已经改了而面板没记住＝立刻锁死）
	SetMySQLRootPassword func(password string) error
	// MySQLInputTimeout 是"限时询问 root 口令"的等待时长；<=0 按 60 秒。
	MySQLInputTimeout time.Duration

	// SiteDependents 返回面板里的站点摘要（域名 + PHP 版本）。
	//
	// 为什么由 web 层注入：站点数据在另一个 store（sites 包），services 不直接
	// 读它 —— 两份来源各写各的必然漂移。卸载依赖检测（dependents.go）靠它回答
	// "有没有站点正在用这个 PHP 版本 / 会被 MySQL、nginx 的卸载影响"。
	// nil = 未接入 → 依赖检测会如实说"没有站点记录可用"，绝不假装没有站点。
	SiteDependents func() []SiteRef
}

// MySQLCredential 是"面板持有的 MySQL 超级账号凭据"（来自 config.json）。
//
// 刻意不复用 mysql.Options：那里的 Password 是"这次连接用哪个口令"，
// 这里描述的是"面板认为服务器上的口令是什么"，语义不同。
type MySQLCredential struct {
	Host     string
	Port     int
	Socket   string
	User     string
	Password string
}

// NewManager 创建服务管理器。
func NewManager(repo *Repository, opt Options) *Manager {
	return &Manager{repo: repo, opt: opt}
}

// DriverFor 为一条服务记录构造对应的驱动。
func (m *Manager) DriverFor(s *Service) (Driver, error) {
	switch s.Kind {
	case KindNative:
		// 有 launchd 标签/plist 的用 launchd 驱动；
		// 只有启动命令的用命令驱动（进程随面板退出，界面上会标注）
		if s.LaunchLabel != "" || s.PlistPath != "" {
			return newNativeDriver(m.opt, s), nil
		}
		if strings.TrimSpace(s.StartCmd) != "" {
			return newCommandDriver(m.opt, s), nil
		}
		return newNativeDriver(m.opt, s), nil
	case KindDocker:
		if m.opt.DockerSocket == "" {
			return nil, MarkRuntimeUnavailable(
				fmt.Errorf("Docker 不可用：未检测到 %s", "/var/run/docker.sock"))
		}
		return newDockerDriver(m.opt, s), nil
	case KindCompose:
		if m.opt.DockerSocket == "" {
			return nil, MarkRuntimeUnavailable(
				fmt.Errorf("Docker 不可用，无法管理 compose 项目"))
		}
		return newComposeDriver(m.opt, s), nil
	case KindColima:
		// 刻意不检查 DockerSocket：运行时停着的时候 socket 自然不存在，
		// 而这正是用户最需要把它启动起来的场景。
		return newColimaDriver(m.opt, s), nil
	default:
		return nil, fmt.Errorf("未知的服务类型: %s", s.Kind)
	}
}

// View 是返回给前端的服务视图（注册信息 + 实时状态 + 健康）。
type View struct {
	*Service
	State  State  `json:"state"`
	Health Health `json:"health"`
	// DriverReady 表示当前环境能否管理该服务（如 Docker 未装时为 false）
	DriverReady bool   `json:"driver_ready"`
	DriverError string `json:"driver_error,omitempty"`
}

// List 返回所有服务及其实时状态。
//
// 状态查询是并发进行的：每个服务都要 fork 一次 launchctl/ps，
// 串行查询在服务多时会让页面明显变慢。
func (m *Manager) List(ctx context.Context, withHealth bool) ([]*View, error) {
	all, err := m.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]*View, len(all))
	type result struct {
		idx    int
		state  State
		health Health
		ready  bool
		derr   string
	}
	ch := make(chan result, len(all))

	for i, s := range all {
		go func(i int, s *Service) {
			r := result{idx: i, state: State{Status: "unknown"}}
			drv, err := m.DriverFor(s)
			if err != nil {
				r.derr = err.Error()
				r.state.Status = "unavailable"
				ch <- r
				return
			}
			r.ready = true
			// 状态查询单独限时，避免某个服务卡住整个列表
			sctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			st, err := drv.Status(sctx)
			if err != nil {
				r.state = State{Status: "error", Detail: err.Error()}
			} else {
				r.state = st
			}
			if withHealth && s.HealthURL != "" {
				hctx, hcancel := context.WithTimeout(ctx, 8*time.Second)
				defer hcancel()
				r.health = drv.Health(hctx)
			}
			ch <- r
		}(i, s)
	}

	for range all {
		r := <-ch
		views[r.idx] = &View{
			Service:     all[r.idx],
			State:       r.state,
			Health:      r.health,
			DriverReady: r.ready,
			DriverError: r.derr,
		}
	}
	return views, nil
}

// Get 返回单个服务的视图。
func (m *Manager) Get(ctx context.Context, name string) (*View, error) {
	s, err := m.repo.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	v := &View{Service: s, State: State{Status: "unknown"}}
	drv, err := m.DriverFor(s)
	if err != nil {
		v.DriverError = err.Error()
		v.State.Status = "unavailable"
		return v, nil
	}
	v.DriverReady = true
	st, err := drv.Status(ctx)
	if err != nil {
		v.State = State{Status: "error", Detail: err.Error()}
	} else {
		v.State = st
	}
	if s.HealthURL != "" {
		v.Health = drv.Health(ctx)
	}
	return v, nil
}

// driver 是内部辅助：取驱动或返回错误。
func (m *Manager) driver(ctx context.Context, name string) (Driver, *Service, error) {
	s, err := m.repo.Get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	drv, err := m.DriverFor(s)
	if err != nil {
		return nil, s, err
	}
	return drv, s, nil
}

// Action 执行一次服务操作（start/stop/restart）。
//
// 操作后不立刻返回状态：服务启动需要时间，
// 这里等待一小段并轮询，尽量让前端拿到"操作后"的真实状态。
func (m *Manager) Action(ctx context.Context, name, action string) (State, error) {
	drv, s, err := m.driver(ctx, name)
	if err != nil {
		return State{Status: "error", Detail: err.Error()}, err
	}
	switch action {
	case "start":
		err = drv.Start(ctx)
	case "stop":
		err = drv.Stop(ctx)
	case "restart":
		err = drv.Restart(ctx)
	default:
		return State{}, fmt.Errorf("不支持的操作: %s", action)
	}
	if err != nil {
		return State{Status: "error", Detail: err.Error()}, err
	}
	// 等待状态稳定（最多 8 秒）
	want := action != "stop"
	var last State
	for i := 0; i < 16; i++ {
		time.Sleep(500 * time.Millisecond)
		st, serr := drv.Status(ctx)
		if serr == nil {
			last = st
			if st.Running == want {
				return st, nil
			}
		}
	}
	if last.Status == "" {
		last = State{Status: "unknown"}
	}
	_ = s
	if !want {
		// stop 等不到"不再运行"是**真失败**，必须如实报错。
		//
		// 真机报障（2026-09-17 用户）：面板点「停止」返回 200 ok:true，而同一个 PID
		// 仍在 *:8384 上监听 —— 这就是本项目最忌讳的"能谎报成功"。根因（launchd 域判断
		// 错误）已在 priv 层修掉；这里补上**结果层**的最后一道：命令返回不代表状态到了。
		detail := ""
		if last.PID > 0 {
			detail = fmt.Sprintf("（pid %d 仍在运行）", last.PID)
		}
		if last.Detail != "" {
			detail += "：" + last.Detail
		}
		return last, fmt.Errorf("已请求停止，但 8 秒内它仍在运行%s。"+
			"可能是被 KeepAlive 反复拉起、进程拒绝退出，或它根本不受 launchd 管理；"+
			"请打开「📜 日志」看原因，或在终端执行 lsof -nP -iTCP:<端口> -sTCP:LISTEN 确认是谁在占用",
			detail)
	}
	// start / restart 等不到"已运行"**不一定是失败**（有的服务启动慢、状态上报滞后），
	// 所以仍然返回成功 —— 但必须让界面显示"还没确认"，不能让用户以为已经好了。
	last.Warning = appendWarning(last.Warning,
		"已请求启动，但 8 秒内还没确认它在运行（可能仍在启动中，请点「⟳ 刷新」确认）")
	return last, nil
}

// Logs 读取服务日志。
func (m *Manager) Logs(ctx context.Context, name string, lines int) (string, error) {
	drv, _, err := m.driver(ctx, name)
	if err != nil {
		return "", err
	}
	return drv.Logs(ctx, lines)
}

// LogStream 订阅服务日志。
func (m *Manager) LogStream(ctx context.Context, name string) (<-chan string, error) {
	drv, _, err := m.driver(ctx, name)
	if err != nil {
		return nil, err
	}
	return drv.LogStream(ctx)
}

// Uninstall 卸载一个托管服务。纳管服务拒绝卸载。
func (m *Manager) Uninstall(ctx context.Context, name string) error {
	drv, s, err := m.driver(ctx, name)
	if err != nil {
		return err
	}
	if !s.Managed {
		return fmt.Errorf("「%s」是面板只做了登记的服务（软件由你自己安装），面板不会卸载它。"+
			"如需停止请用「停止」，如需从列表移除请用「从列表移除（不卸载软件）」", s.DisplayName)
	}
	if err := drv.Uninstall(ctx); err != nil {
		return err
	}
	return m.repo.Delete(ctx, name)
}

// Adopt 纳管一个已存在的服务（写入注册表，不做任何系统改动）。
func (m *Manager) Adopt(ctx context.Context, s *Service) error {
	s.Managed = false
	return m.repo.Create(ctx, s)
}

// ForgetByLabel 按 launchd label 把服务记录从注册表里删掉（不触碰系统）。
//
// 为什么需要：市场卸载的 **installer 那条路**（frpc / Orbien 客户端 / IOPaint
// / Qwen3 TTS 这类面板自己装的应用）只删了文件与 plist，**没有删服务记录** ——
// 于是用户在市场里卸载完，切到「服务管理」它还在，看着像"没卸掉"
// （2026-09-16 用户反馈："用户在面板卸载一个软件，服务管理也应该消失"）。
//
// 返回删掉的条数；label 为空直接返回 0（避免误删）。
func (m *Manager) ForgetByLabel(ctx context.Context, label string) (int, error) {
	if strings.TrimSpace(label) == "" {
		return 0, nil
	}
	list, err := m.repo.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range list {
		if s.LaunchLabel != label {
			continue
		}
		if err := m.repo.Delete(ctx, s.Name); err != nil {
			continue
		}
		n++
	}
	return n, nil
}

// Forget 从注册表移除（不触碰系统）。
//
// 注意：HTTP 的「只删记录」通路（DELETE /api/v1/services/{name}）**刻意不用它**，
// 而是直接落 Repository.Delete —— 构造 Manager 会探测 Docker socket 并
// ReconcilePaths 写配置，那条通路的语义要求"连运行时的边都不沾"（见
// web 层 handleServiceDelete）。
func (m *Manager) Forget(ctx context.Context, name string) error {
	return m.repo.Delete(ctx, name)
}

// ---------- 工具 ----------

// NormalizeName 把展示名规范成服务标识（小写字母数字与连字符）。
func NormalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '.':
			b.WriteRune('-')
		case r == '-':
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}

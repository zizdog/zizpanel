// Package config 负责面板配置的加载、默认值与持久化。
//
// 设计要点：
//   - 配置文件是 JSON，落在数据目录（默认 /opt/zizpanel/data/config.json）。
//   - 首次启动由 Bootstrap 生成，并写入随机 JWT/会话密钥。
//   - 所有路径集中在这里，其他模块不再硬编码系统路径。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Config 是面板的完整运行时配置。
type Config struct {
	// ---------- 面板自身 ----------
	Version   string `json:"version"`
	DataDir   string `json:"data_dir"`   // 数据目录（db、证书、备份清单）
	LogDir    string `json:"log_dir"`    // 日志目录
	RunDir    string `json:"run_dir"`    // 运行时文件（pid、socket）
	WorkDir   string `json:"work_dir"`   // 工作目录（compose 文件、服务代码）
	BinDir    string `json:"bin_dir"`    // 安装目录下的 bin
	Secret    string `json:"secret"`     // 会话签名密钥，首次启动随机生成
	InstallID string `json:"install_id"` // 安装唯一标识，用于遥测/迁移识别

	// ---------- 数据库连接（面板管理 MySQL 用）----------
	//
	// 为什么必须可配置：早期只从 ~/www/.env.local 读 root 密码 ——
	// 那是某个具体项目的约定。换一台机器（或那台机器的 MySQL root 有密码
	// 而文件不存在）就永远连不上，界面上只显示"无法连接 MySQL"，
	// 用户完全不知道该怎么办。真机上就是这么卡住的。
	// 现在：面板里可以直接填，并且会持久化。
	MySQLHost     string `json:"mysql_host"`
	MySQLPort     int    `json:"mysql_port"`
	MySQLSocket   string `json:"mysql_socket"`
	MySQLUser     string `json:"mysql_user"`
	MySQLPassword string `json:"mysql_password"`
	// MySQLInputTimeoutSeconds 是"装 MySQL 时限时询问 root 口令"的等待秒数。
	//
	// 为什么要可配：默认 60 秒是"用户就在屏幕前"的合理值；批处理/无人值守
	// 场景下用户可以调小（1 秒＝等于全自动生成）。超时不是失败 ——
	// 到点自动生成强随机口令并继续，见 services/lnmp_mysql_credentials.go。
	MySQLInputTimeoutSeconds int `json:"mysql_input_timeout_seconds"`

	// ---------- 应用包镜像（自建 NAS） ----------
	// MirrorBase 是应用包镜像基址，例如 https://mirror.zizdog.com:8888。
	//
	// 为什么要有它：面板要下的东西来自很多不同上游（GitHub Release、Homebrew、
	// PyPI、huggingface、苹果 CLT 包……），各自维护一套"国内加速源"既散又容易过期。
	// 统一指向自建镜像后，从哪下、下什么、怎么校验都由我们自己控制。
	//
	// 语义（用户原话："对所有能用到的模型、软件，如 ffmpeg，
	// 都要以 NAS 镜像优先，不通再走别的！"）：
	// 有值时镜像是**优先来源** —— 安装前先检查镜像上有没有这个资源，
	// 有就从镜像下（并把地址写进任务日志）；镜像上缺件或不可达时**自动回落**
	// 到各来源内置的公网/国内镜像，而不是让安装失败。
	// 留空 = 完全不用镜像（应急用）。
	//
	// 注意：这与本字段更早一版的注释（"唯一来源、不回退"）相反，那是过期的
	// 需求描述，代码从来不是那样跑的（真按"唯一来源"跑会在镜像站抖一下时
	// 就让用户装不上东西）。以这里为准。
	MirrorBase string `json:"mirror_base"`
	// MirrorBaseLAN 是同一个镜像站的**局域网地址**（可选，例如 http://192.168.1.8:8090）。
	//
	// 为什么需要（2026-09-18 用户报障："ddns-go 等没有 nas 缓存！装不上啊！"）：
	// 公网入口（mirror.zizdog.com:8888）坏掉时，面板会**回落公网**（GitHub/第三方加速），
	// 而国内直连 GitHub 很慢甚至不通 —— 用户就看到"装不上"。而同一台 NAS 的局域网
	// 入口（8090）往往是好的、还快得多。配了这个地址后：镜像探测会**依次尝试**
	// 公网基址 → 局域网基址，谁先命中用谁，都不可达才回落公网。
	//
	// 留空 = 不尝试局域网地址（默认给作者家里的 NAS，见 DefaultMirrorBaseLAN）。
	MirrorBaseLAN string `json:"mirror_base_lan"`
	// MirrorProbeSeconds 是"镜像上有没有这个资源"的单次探测超时（秒）。
	//
	// 必须短：镜像不可达时不能让每次安装都白等。默认 4 秒 —— 局域网/同城镜像
	// 正常在 100ms 内应答，4 秒足够区分"慢"和"不通"，又不会把安装拖得很难看。
	MirrorProbeSeconds int `json:"mirror_probe_seconds"`

	// OfflineOnly 打开后进入**仅走 NAS（离线）模式**：所有安装过程只用镜像站上的
	// 资源，**禁止任何外网回落**；缺资源就明确失败并要求补到 NAS 上。
	//
	// 与 MirrorBase 的关系（两者语义不同，别混）：
	//   MirrorBase 非空 + OfflineOnly=false → "镜像优先 + 探不通回落公网"（默认）；
	//   MirrorBase 非空 + OfflineOnly=true  → "只用镜像，缺件即失败"；
	//   MirrorBase 为空 + OfflineOnly=true  → 自相矛盾（没有镜像可用），
	//     此时各安装器必须**明确报错**，而不是悄悄走公网。
	//
	// 为什么需要它：用户的真实场景是"整机断外网/迁移到隔离网络"，那时
	// "静默回落公网"会变成"装到一半卡死"，比直接失败更糟 —— 用户不知道自己在等什么。
	// 这个开关就是"宁可明确失败，也不要静默变慢"。
	OfflineOnly bool `json:"offline_only"`

	// ---------- 在线升级 ----------
	// UpgradeSource 是升级源地址（放 manifest.json / manifest.json.sig 的目录）。
	//
	// 语义（2026-09 改造后）：这里存的是**用户显式选择的源**。
	// 留空不再等于"不能升级"，而是"按 internal/upgrade 的候选顺序自动选源"
	// （同网段 NAS → 公网主源 → 备用镜像 → GitHub 兜底，见 CandidateSources）。
	// 只有用户在设置页/接口显式填过地址，才会被写进这里；
	// "检查更新"不会再把它自动写成一个候选，否则每台机器都会被钉死在一个源上。
	UpgradeSource string `json:"upgrade_source"`
	// UpgradeNotes 缓存最近一次检查更新拿到的发布说明，供界面展示。
	UpgradeNotes string `json:"upgrade_notes"`

	// ---------- 监听 ----------
	Listen    string `json:"listen"`     // 面板监听地址，如 :8443
	TLSEnable bool   `json:"tls_enable"` // 是否启用 HTTPS（自签或自有证书）
	TLSCert   string `json:"tls_cert"`
	TLSKey    string `json:"tls_key"`
	// AccessMode: any=任意来源, local=仅本机, whitelist=仅白名单
	AccessMode  string   `json:"access_mode"`
	IPWhitelist []string `json:"ip_whitelist"`
	// TrustProxy 为 true 时，从 X-Forwarded-For 取真实 IP（面板挂在 nginx 后面时开）。
	TrustProxy bool `json:"trust_proxy"`

	// ---------- Web 终端 ----------
	// TerminalEnabled 默认 false：终端等于把 shell 交给浏览器，
	// 必须由用户显式开启，而不是"装好就有"。
	TerminalEnabled     bool   `json:"terminal_enabled"`
	TerminalShell       string `json:"terminal_shell"`
	TerminalIdleMins    int    `json:"terminal_idle_mins"`
	TerminalMaxSessions int    `json:"terminal_max_sessions"`
	// FileRoots 是文件管理器允许访问的根目录（留空则用默认集合）
	FileRoots []string `json:"file_roots"`

	// ---------- 安全 ----------
	SessionHours   int    `json:"session_hours"`   // 会话有效期
	LoginMaxFail   int    `json:"login_max_fail"`  // 连续失败几次锁定
	LoginLockMins  int    `json:"login_lock_mins"` // 锁定时长（分钟）
	Require2FA     bool   `json:"require_2fa"`     // 强制所有账号开启两步验证
	PanelPublicURL string `json:"panel_public_url"`
	// PanelSuffix 是面板的**安全后缀**（宝塔那种"安全入口"）：
	// 面板的界面与接口只在这个前缀下提供服务，直接访问 / 只会得到 404。
	//
	// 空串 = 不启用（本地开发/测试用）。真实安装时由 Bootstrap 随机生成，
	// 也可以在「面板设置」里改或清空（清空要显式确认，那等于把面板放回根路径）。
	PanelSuffix string `json:"panel_suffix"`
	// AppProxyAuth 控制"应用界面（/iopaint/、/squoosh/ 等）是否要求先登录面板"。
	//
	// 默认 **true**：这些界面挂在面板同一个端口上，不要求登录就等于
	// "知道 URL 就能用"（Squoosh 这类完全没有自己的鉴权）。
	// 代价是访问前先过一次面板登录 —— 面板本来就是这台机器的总入口。
	AppProxyAuth bool `json:"app_proxy_auth"`
	// AppProxy 控制"把有界面的应用挂到 /<slug>/ 下"这个能力（默认开）。
	//
	// 为什么做成开关：面板的 /<slug>/ 是**公开路径**（应用自己鉴权，面板不拦），
	// 于是它对"把面板暴露到公网"的用户来说等于多开了一组入口。
	// 默认开是因为直连端口本来就在局域网上开着、且这是用户明确要的便利；
	// 但只要用户觉得不合适，一个开关就能全关掉（页面上的入口也会跟着消失）。
	AppProxy bool `json:"app_proxy"`

	// ---------- 环境（LNMP 等由面板管理的系统组件） ----------
	User     string `json:"user"`      // 面板运行用户（安装时确定）
	UserHome string `json:"user_home"` // 该用户家目录
	UserUID  int    `json:"user_uid"`  // 该用户 uid（launchd gui/<uid> 域需要）
	WWWRoot  string `json:"www_root"`  // 网站根目录，如 /Users/zizdog/www

	BrewPrefix string `json:"brew_prefix"` // /opt/homebrew
	BrewBin    string `json:"brew_bin"`
	NginxBin   string `json:"nginx_bin"`
	NginxConf  string `json:"nginx_conf"`
	VhostDir   string `json:"vhost_dir"`
	LogRoot    string `json:"log_root"`
	PHPSvc     string `json:"php_svc"` // 默认 PHP 服务名，如 php@8.2
	PHPVer     string `json:"php_ver"`
	PHPEtc     string `json:"php_etc"`
	MySQLSvc   string `json:"mysql_svc"`
	MySQLBin   string `json:"mysql_bin"`
	PmaDir     string `json:"pma_dir"`

	// ---------- 上传与执行限制（面板设置 → 上传与执行限制）----------
	//
	// 为什么放在面板配置里：用户报障"phpMyAdmin 导入 413，面板找不到入口"。
	// nginx 出厂 client_max_body_size 只有 1m、PHP 出厂 upload 2M/post 8M，
	// 两组上限都必须能在面板里改、且**默认就要能用**。这里存的是值，
	// 真正的落点由 sites 包负责（vhost 的 server 块 + PHP conf.d 片段）。
	NginxClientMaxBodySize string `json:"nginx_client_max_body_size"`
	PHPUploadMaxFilesize   string `json:"php_upload_max_filesize"`
	PHPPostMaxSize         string `json:"php_post_max_size"`
	PHPMemoryLimit         string `json:"php_memory_limit"`
	PHPMaxExecutionTime    int    `json:"php_max_execution_time"`

	// DockerSocket 为空表示 Docker 不可用。
	DockerSocket string `json:"docker_socket"`

	mu   sync.RWMutex
	path string
}

// DefaultRoot 是面板的默认安装根目录。
const DefaultRoot = "/opt/zizpanel"

// root 返回面板安装根目录。
//
// 支持环境变量重定位有两个实际用途：
//   - 把面板装到外置盘或其他路径（改 ZIZPANEL_ROOT 即可）
//   - 单元测试隔离，避免触碰 /opt 下的真实数据
func root() string {
	if v := strings.TrimSpace(os.Getenv("ZIZPANEL_ROOT")); v != "" {
		return filepath.Clean(v)
	}
	return DefaultRoot
}

// DefaultConfigPath 返回默认配置文件路径（受 ZIZPANEL_ROOT 影响）。
// DefaultMirrorBase 是应用包镜像的默认基址（自建 NAS，经 mirror.zizdog.com 反代）。
//
// 面板里所有安装过程都**先**检查它：有就用它，它缺件/不可达时才回落公网源。
const DefaultMirrorBase = "https://mirror.zizdog.com:8888"

// DefaultMirrorBaseLAN 是镜像站的**局域网**入口（同一个 NAS 的另一个入口）。
//
// 它只是"公网入口不可达时的第二候选"，不是替代品：面板先探公网镜像，
// 探不通（站点挂了 / 不在同一网络 / 缺件）再探这个，最后才回落公网源。
// 2026-09-18 实测：公网入口 TLS 握手成功但返回空响应（NAS 侧反代坏了），
// 而 http://192.168.1.8:8090 完全正常 —— 有这条候选就不会"装不上"。
const DefaultMirrorBaseLAN = "http://192.168.1.8:8090"

// DefaultUpgradeSource 是在线升级的**公网主源**（用户要求"以后探测以公网 zizdog.com 为主"）。
//
// 它与 internal/upgrade.CandidateSources 里的默认候选是同一个值
// （那里直接引用本常量，保证不会漂移）。注意语义：
//   - 它只是默认配置值，不代表"用户显式选了它"；
//   - 候选顺序里同网段的 NAS 会排在它前面（局域网快约 100 倍）；
//   - UpgradeSource 被显式清空后，候选列表会照常包含它 —— 清空配置
//     不等于禁用网络升级，只等于"让面板自己按优先级选"。
const DefaultUpgradeSource = "https://zizdog.com/zizpanel"

func DefaultConfigPath() string {
	return filepath.Join(root(), "data", "config.json")
}

// PanelUser 返回面板应当使用的"真实用户"。
//
// 探测顺序（重要性递减）：
//  1. ZIZPANEL_USER  —— 安装脚本写入 launchd 环境变量。
//     面板以 root 运行时 $HOME 是 /var/root、$SUDO_USER 为空，
//     只有这个变量能可靠告知"网站目录该属于谁"。
//  2. SUDO_USER / SUDO_UID —— 通过 sudo 直接运行时可用。
//  3. 当前用户（非 root 时）。
//
// 如果全都没有，才退回 root。这一步错了会导致网站根目录算成
// /var/root/www，所有站点都找不到 —— 所以顺序不能随意调整。
func PanelUser() string {
	if v := strings.TrimSpace(os.Getenv("ZIZPANEL_USER")); v != "" && v != "root" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("SUDO_USER")); v != "" && v != "root" {
		return v
	}
	if cu, err := user.Current(); err == nil && cu.Username != "root" {
		return cu.Username
	}
	if cu, err := user.LookupId(os.Getenv("SUDO_UID")); err == nil && cu.Username != "root" {
		return cu.Username
	}
	return "root"
}

// Default 返回一份默认配置，账号与网站目录基于真实用户推导。
func Default() *Config {
	u := PanelUser()
	home := ""
	if cu, err := user.Lookup(u); err == nil {
		home = cu.HomeDir
	}
	if home == "" {
		home = os.Getenv("HOME")
	}

	brew := "/opt/homebrew"
	if _, err := os.Stat(brew); err != nil {
		brew = "/usr/local" // Intel Mac 或自定义安装
	}

	r := root()
	c := &Config{
		Version:    "0.1.0",
		DataDir:    filepath.Join(r, "data"),
		LogDir:     filepath.Join(r, "logs"),
		RunDir:     filepath.Join(r, "run"),
		WorkDir:    filepath.Join(r, "work"),
		BinDir:     filepath.Join(r, "bin"),
		Listen:     ":8443",
		TLSEnable:  true,
		AccessMode: "any",
		// 有界面的应用默认挂到 /<slug>/ 下（用户明确要求；可在设置里关掉）
		AppProxy: true,
		// 且默认要求先登录面板（Squoosh 这类应用自己没有鉴权）
		AppProxyAuth: true,
		// 应用包镜像：默认指向自建 NAS。面板所有安装过程**优先**从这里下，
		// 镜像上缺件/不可达时自动回落公网源（保证"镜像抖一下就装不上"不会发生）。
		// 留空 = 关闭镜像（应急用）。
		MirrorBase:         DefaultMirrorBase,
		MirrorBaseLAN:      DefaultMirrorBaseLAN,
		MirrorProbeSeconds: 4,
		// 在线升级：默认指向公网主源 zizdog.com。
		//
		// 只在这里（新建配置的默认值）设，**不放进 fill()**：
		// fill() 是"老配置补默认"用的，若在那里补，用户显式清空的值会被
		// 重新填回来 —— 那样"清空升级源"就永远做不到，设置页也永远清不掉
		// （这个坑真机上出现过）。所以：配置文件里显式写了什么就是什么，
		// 只有整个字段缺失的老配置才会继承这个默认值。
		UpgradeSource: DefaultUpgradeSource,
		SessionHours:  72,
		LoginMaxFail:  5,
		LoginLockMins: 15,
		// 终端默认关闭；文件管理器默认只开放网站目录与面板目录
		TerminalEnabled:     false,
		TerminalIdleMins:    30,
		TerminalMaxSessions: 3,
		MySQLHost:           "127.0.0.1",
		MySQLPort:           3306,
		MySQLSocket:         "/tmp/mysql.sock",
		MySQLUser:           "root",
		// 60 秒：够用户看清楚提示并输入，又不至于让"人不在"的安装白等太久。
		MySQLInputTimeoutSeconds: 60,
		User:                     u,
		UserHome:                 home,
		UserUID:                  lookupUID(u),
		WWWRoot:                  filepath.Join(home, "www"),
		LogRoot:                  filepath.Join(home, "www", "_logs"),
		PHPSvc:                   "php@8.2",
		PHPVer:                   "8.2",
		MySQLSvc:                 "mysql@8.4",
		DockerSocket:             "/var/run/docker.sock",
		// 上传与执行限制的默认值。**必须与 sites.DefaultLimits() 一致**：
		// 面板生成的 vhost / PHP 片段在字段为空时会退回 sites 包的默认，
		// 两处不一致会导致"界面显示的默认值"与实际写入的不是同一个。
		// 有测试（sites 包的 TestConfigDefaultsMatchSitesDefaults）锁死这一点。
		//
		// 为什么默认就要是 512m/512M：nginx 出厂 1m、PHP 出厂 2M/8M，
		// phpMyAdmin 导入几十 MB 的 SQL 必然 413 —— 用户不该先撞墙才知道要改。
		NginxClientMaxBodySize: "512m",
		PHPUploadMaxFilesize:   "512M",
		PHPPostMaxSize:         "512M",
		PHPMemoryLimit:         "512M",
		PHPMaxExecutionTime:    300,
	}
	c.applyBrewPrefix(brew)
	c.TLSCert = filepath.Join(c.DataDir, "tls", "panel.crt")
	c.TLSKey = filepath.Join(c.DataDir, "tls", "panel.key")
	return c
}

// brewPrefixes 是 Homebrew 前缀的候选路径。
//
// 顺序有含义：Apple Silicon 用 /opt/homebrew，Intel 或自定义安装用 /usr/local。
// 两个都不在时才退回 /opt/homebrew（错误信息里会显示这个"期望路径"）。
//
// 变量而不是常量：单测要能把它指到临时目录，
// 否则"重新探测前缀"这段逻辑只能靠真机验证（而它正是真机上才暴露的 bug）。
var brewPrefixes = func() []string {
	return []string{"/opt/homebrew", "/usr/local"}
}

// detectBrewPrefix 找到**真的装了** Homebrew 的前缀；都没有则返回 ""。
//
// 为什么必须真的去 stat `bin/brew`：`Default()` 在"面板刚启动"时求值，
// 而**全新机器上面板是先于 Homebrew 存在的** —— 那一刻 /opt/homebrew 还没有，
// 于是前缀被写成 /usr/local 并**持久化进 config.json**；
// 之后面板在任务里装好 Homebrew，配置里的路径却永远是错的。
// 真机（抹机后的 mini）就是这么复现的：Homebrew 明明装好了，
// 任务却报 `env: /usr/local/bin/brew: No such file or directory`。
// 变量而不是函数：单测必须能把它指到 t.TempDir()（用 SetBrewPrefixDetectorForTest）。
// 否则 `ReconcilePaths` 会在测试里把沙箱前缀**改回真机的 /opt/homebrew**，
// 于是"PHP 多版本"这类会改 www.conf 的功能就会在 `go test` 时
// 写用户真实的 /opt/homebrew/etc/php/<版本>/php-fpm.d/www.conf。
// 本项目已经因为"测试漏沙箱化"把生产 nginx 配置改坏过一次，不再犯第二次。
var detectBrewPrefix = detectBrewPrefixReal

// detectBrewPrefixReal 是生产环境的实现（见上面的说明）。
func detectBrewPrefixReal() string {
	for _, p := range brewPrefixes() {
		if st, err := os.Stat(filepath.Join(p, "bin", "brew")); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// SetBrewPrefixDetectorForTest 替换 Homebrew 前缀探测，返回值供测试恢复原实现。
//
// **只给测试用。** 测试服务器（internal/web）会把它指向 t.TempDir()，
// 否则任何"按 brew 前缀推导路径"的功能都会在测试里落到真机上。
func SetBrewPrefixDetectorForTest(fn func() string) func() string {
	prev := detectBrewPrefix
	if fn != nil {
		detectBrewPrefix = fn
	}
	return prev
}

// applyBrewPrefix 把由 Homebrew 前缀推导出来的路径统一写进配置。
//
// 抽成一个函数的原因：这些路径散落在站点、数据库、日志、phpMyAdmin 等十几处，
// 一旦"前缀变了但派生路径没跟着变"，症状会是"某个功能莫名找不到文件"。
func (c *Config) applyBrewPrefix(brew string) {
	c.BrewPrefix = brew
	c.BrewBin = filepath.Join(brew, "bin", "brew")
	c.NginxBin = filepath.Join(brew, "bin", "nginx")
	c.NginxConf = filepath.Join(brew, "etc", "nginx", "nginx.conf")
	c.VhostDir = filepath.Join(brew, "etc", "nginx", "vhosts")
	c.PHPEtc = filepath.Join(brew, "etc", "php", "8.2")
	c.MySQLBin = filepath.Join(brew, "opt", "mysql@8.4", "bin", "mysql")
	c.PmaDir = filepath.Join(brew, "share", "phpmyadmin")
}

// ReconcilePaths 修正"配置里记着、但机器上已经变了"的路径，返回是否有变化。
//
// 现存问题（真机复现）：配置在**面板刚启动**那一刻生成，
// 那时 Homebrew 可能还没装，前缀被写成 /usr/local 并持久化；
// 之后无论装多少次 Homebrew 都不会自我修正。
// 这里在每次读取配置时做一次便宜的校正（两次 stat），
// 有变化才写回文件（避免无谓的磁盘写入与权限变更）。
func (c *Config) ReconcilePaths() bool {
	if c == nil {
		return false
	}
	want := detectBrewPrefix()
	if want == "" {
		// 没装 Homebrew：不要瞎改用户的配置（他可能手工填了自定义前缀）
		return false
	}
	// 用户的配置已经指向一个**真实存在**的安装 → 尊重它，不动
	if st, err := os.Stat(filepath.Join(c.BrewPrefix, "bin", "brew")); err == nil && !st.IsDir() {
		return false
	}
	if want == c.BrewPrefix && c.BrewBin == filepath.Join(want, "bin", "brew") {
		return false
	}
	c.applyBrewPrefix(want)
	_ = c.Save()
	return true
}

// Load 读取配置文件；不存在则返回错误 ErrNotInstalled。
var ErrNotInstalled = errors.New("面板尚未初始化：配置文件不存在")

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotInstalled
		}
		return nil, err
	}
	c := Default()
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("配置文件解析失败 %s: %w", path, err)
	}
	c.path = path
	c.fill()
	// 修正在"面板先于 Homebrew 存在"那一刻写下的 /usr/local 前缀。
	// 放在 Load 里而不是各个调用点：每个功能都去关心"brew 前缀对不对"
	// 是重复的，而且总有人忘记。
	c.ReconcilePaths()
	return c, nil
}

// Bootstrap 首次初始化：确保目录存在、生成密钥、写出配置文件。
// 已存在配置文件时直接加载，不会覆盖。
//
// 关键行为：数据/日志/运行等目录都相对"配置文件所在目录"推导，
// 而不是固定用 /opt/zizpanel。这样 --config 指向别处时，
// 面板会完整地装在那个位置，既不会误改 /opt 下的目录，也不会因权限失败。
func Bootstrap(path string) (*Config, bool, error) {
	if c, err := Load(path); err == nil {
		return c, false, nil
	} else if !errors.Is(err, ErrNotInstalled) {
		return nil, false, err
	}

	c := Default()
	c.path = path
	c.Secret = randomHex(32)
	c.InstallID = randomHex(8)
	// 新建配置 = 真实安装：按用户要求生成安全后缀（宝塔式的"必须带一串随机路径"）。
	// 已存在的配置不会被改动 —— 用户可能已经把它设成自己好记的值，或者故意清空。
	c.PanelSuffix = RandomPanelSuffix()

	// 以配置文件所在目录作为本实例的数据目录，兄弟目录作为 logs/run/work/bin
	base := filepath.Dir(path)
	parent := filepath.Dir(base)
	c.DataDir = base
	c.LogDir = filepath.Join(parent, "logs")
	c.RunDir = filepath.Join(parent, "run")
	c.WorkDir = filepath.Join(parent, "work")
	c.BinDir = filepath.Join(parent, "bin")
	c.TLSCert = filepath.Join(base, "tls", "panel.crt")
	c.TLSKey = filepath.Join(base, "tls", "panel.key")
	c.fill()

	// 先建目录再写配置：Save 需要目录已存在
	for _, d := range c.allDirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, false, fmt.Errorf("创建目录失败 %s: %w", d, err)
		}
	}
	if err := c.Save(); err != nil {
		return nil, false, err
	}
	// 首次初始化时也要修正数据目录归属（Save 只处理文件与父目录）
	c.fixDataDirOwnership()
	return c, true, nil
}

// fixDataDirOwnership 让数据目录归属真实用户，便于用户查看与备份。
func (c *Config) fixDataDirOwnership() {
	if os.Geteuid() != 0 {
		return
	}
	uid, gid := c.ownerIDs()
	if uid == 0 {
		return
	}
	for _, d := range []string{c.DataDir, filepath.Join(c.DataDir, "tls")} {
		_ = os.Chown(d, uid, gid)
	}
}

// fill 补齐缺失字段（老版本配置升级用），保证默认值不被空串覆盖。
// RandomPanelSuffix 生成一个安全后缀：8 位小写字母+数字。
//
// 为什么用这个字符集：要能直接放进 URL 路径、且不容易被念错/抄错
// （去掉 0/o/1/l 之类易混字符）；长度取 8 位，暴力猜中概率可忽略。
func RandomPanelSuffix() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // 去掉 i/l/o/0/1
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "panel" + randomHex(4)
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out)
}

// NormalizePanelSuffix 把用户填的后缀规范化成"可以放进 URL 路径"的形式。
// 去掉斜杠与空白；只保留小写字母、数字、连字符与下划线；最长 32 位。
func NormalizePanelSuffix(raw string) string {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

func (c *Config) fill() {
	d := Default()
	if c.Secret == "" {
		c.Secret = randomHex(32)
	}
	// 后缀统一规范化（配置文件可能被手工改过；带斜杠/空格的写法会让 URL 拼错）
	c.PanelSuffix = NormalizePanelSuffix(c.PanelSuffix)
	if c.InstallID == "" {
		c.InstallID = randomHex(8)
	}
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.SessionHours <= 0 {
		c.SessionHours = d.SessionHours
	}
	if c.LoginMaxFail <= 0 {
		c.LoginMaxFail = d.LoginMaxFail
	}
	if c.LoginLockMins <= 0 {
		c.LoginLockMins = d.LoginLockMins
	}
	if c.MirrorProbeSeconds <= 0 {
		c.MirrorProbeSeconds = d.MirrorProbeSeconds
	}
	if c.AccessMode == "" {
		c.AccessMode = d.AccessMode
	}
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.BrewPrefix == "" {
		c.BrewPrefix = d.BrewPrefix
	}
	if c.DockerSocket == "" {
		c.DockerSocket = d.DockerSocket
	}
	if c.UserHome == "" {
		c.UserHome = d.UserHome
	}
	if c.UserUID == 0 {
		c.UserUID = lookupUID(c.User)
	}
	if c.WWWRoot == "" {
		c.WWWRoot = d.WWWRoot
	}
	if c.RunDir == "" {
		c.RunDir = d.RunDir
	}
	// 老配置文件没有这些字段：补齐默认，避免 0 端口之类的问题
	if c.MySQLHost == "" {
		c.MySQLHost = d.MySQLHost
	}
	if c.MySQLPort == 0 {
		c.MySQLPort = d.MySQLPort
	}
	if c.MySQLSocket == "" {
		c.MySQLSocket = d.MySQLSocket
	}
	if c.MySQLUser == "" {
		c.MySQLUser = d.MySQLUser
	}
	// 老配置没有这个字段（0）：补默认，否则"限时询问"会变成 0 秒＝立刻超时。
	if c.MySQLInputTimeoutSeconds <= 0 {
		c.MySQLInputTimeoutSeconds = d.MySQLInputTimeoutSeconds
	}
	if c.WorkDir == "" {
		c.WorkDir = d.WorkDir
	}
	if c.LogDir == "" {
		c.LogDir = d.LogDir
	}
	if c.TLSCert == "" {
		c.TLSCert = filepath.Join(c.DataDir, "tls", "panel.crt")
	}
	if c.TLSKey == "" {
		c.TLSKey = filepath.Join(c.DataDir, "tls", "panel.key")
	}
	// 上传与执行限制：老配置没有这些字段 → 补默认值。
	//
	// 注意这里**不能**在用户显式清空时重新填回去：这个设置块的保存接口
	// 会用 ParseSize 校验（空值直接被拒），所以字段要么合法、要么缺失，
	// 不存在"用户故意留空"的语义（与 UpgradeSource 那种可清空的字段不同）。
	if c.NginxClientMaxBodySize == "" {
		c.NginxClientMaxBodySize = d.NginxClientMaxBodySize
	}
	if c.PHPUploadMaxFilesize == "" {
		c.PHPUploadMaxFilesize = d.PHPUploadMaxFilesize
	}
	if c.PHPPostMaxSize == "" {
		c.PHPPostMaxSize = d.PHPPostMaxSize
	}
	if c.PHPMemoryLimit == "" {
		c.PHPMemoryLimit = d.PHPMemoryLimit
	}
	if c.PHPMaxExecutionTime <= 0 {
		c.PHPMaxExecutionTime = d.PHPMaxExecutionTime
	}
}

func (c *Config) allDirs() []string {
	return []string{
		c.DataDir, c.LogDir, c.RunDir, c.WorkDir, c.BinDir,
		filepath.Join(c.DataDir, "tls"),
		filepath.Join(c.WorkDir, "compose"),
		filepath.Join(c.WorkDir, "services"),
		filepath.Join(c.WorkDir, "backup"),
	}
}

// EnsureDirs 在每次启动时调用，防止用户手工删掉目录。
//
// 同时顺带修正配置文件与数据目录的归属：老版本安装可能留下
// root 拥有的 config.json，导致普通用户执行 `zizpanel status`
// 报权限错误。放在启动路径上可以自愈，不需要用户重装。
func (c *Config) EnsureDirs() error {
	for _, d := range c.allDirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建目录失败 %s: %w", d, err)
		}
	}
	// 自愈：修正归属（仅在以 root 运行时生效）
	if path := c.Path(); path != "" {
		if _, err := os.Stat(path); err == nil {
			c.fixOwnership(path)
		}
	}
	c.fixDataDirOwnership()
	// 日志目录也要让真实用户能读（排障时经常需要直接看日志）
	if os.Geteuid() == 0 {
		if uid, gid := c.ownerIDs(); uid != 0 {
			for _, d := range []string{c.LogDir, c.RunDir, c.WorkDir} {
				_ = os.Chown(d, uid, gid)
			}
		}
	}
	return nil
}

// SetMySQLPassword 更新面板持有的 MySQL 口令（带写锁）。
//
// 为什么要有它：改口令的请求与读配置的请求是并发的（设置页读、数据库页写），
// 直接赋值会和 Save() 里的 json 序列化构成数据竞争。
// 面板运行在 root 下，这里出错没有第二次机会，所以宁可多一把锁。
func (c *Config) SetMySQLPassword(pw string) {
	c.mu.Lock()
	c.MySQLPassword = pw
	c.mu.Unlock()
}

// MySQLPasswordValue 读取面板持有的 MySQL 口令（带读锁）。
func (c *Config) MySQLPasswordValue() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MySQLPassword
}

// Save 原子写回配置文件（先写临时文件再 rename，避免半截文件）。
func (c *Config) Save() error {
	c.mu.RLock()
	b, err := json.MarshalIndent(c, "", "  ")
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	path := c.Path()
	if path == "" {
		return errors.New("配置路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	c.fixOwnership(path)
	return nil
}

// fixOwnership 让配置文件归属真实用户。
//
// 为什么需要：面板由 LaunchDaemon 以 root 启动，首次初始化时写出的
// config.json 属于 root 且权限 0600。于是普通用户在终端执行
// `zizpanel status` 会得到 "permission denied" —— 而这条命令
// 恰恰是在面板打不开时用来排障的，读不到配置就失去了意义。
//
// 只改归属、不放宽权限：文件仍然只有真实用户与 root 可读，
// 而里面含有会话签名密钥，不应该让所有用户都读到。
func (c *Config) fixOwnership(path string) {
	if os.Geteuid() != 0 {
		return // 非 root 时文件本来就属于当前用户
	}
	uid, gid := c.ownerIDs()
	if uid == 0 {
		return
	}
	_ = os.Chown(path, uid, gid)
	// 目录本身也让真实用户可以进入（否则连遍历都做不到）
	if dir := filepath.Dir(path); dir != "" {
		if st, err := os.Stat(dir); err == nil && st.Mode().Perm()&0o100 == 0 {
			_ = os.Chmod(dir, 0o700)
		}
		_ = os.Chown(dir, uid, gid)
	}
}

// ownerIDs 返回真实用户的 uid/gid。
func (c *Config) ownerIDs() (int, int) {
	if c.User == "" || c.User == "root" {
		return 0, 0
	}
	u, err := user.Lookup(c.User)
	if err != nil {
		return 0, 0
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return uid, gid
}

// Path 返回配置文件路径。
func (c *Config) Path() string {
	if c.path != "" {
		return c.path
	}
	return filepath.Join(c.DataDir, "config.json")
}

// SetPath 用于安装阶段指定非默认路径。
func (c *Config) SetPath(p string) { c.path = p }

// ---------- 小工具 ----------

// Port 从 Listen 中解析端口号。
func (c *Config) Port() int {
	_, p, err := splitHostPort(c.Listen)
	if err != nil {
		return 0
	}
	return p
}

func splitHostPort(addr string) (string, int, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("非法监听地址: %s", addr)
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return "", 0, err
	}
	return addr[:i], p, nil
}

// ServicePath 把服务名（如 nginx）解析为面板 bin 目录下的包装脚本路径。
func (c *Config) ServicePath(name string) string {
	return filepath.Join(c.BinDir, name)
}

// lookupUID 把用户名解析为 uid，失败返回 0。
func lookupUID(name string) int {
	if name == "" || name == "root" {
		return 0
	}
	if u, err := user.Lookup(name); err == nil {
		if n, err := strconv.Atoi(u.Uid); err == nil {
			return n
		}
	}
	return 0
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于系统级异常，退化为时间戳也要保证能启动
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}

// RepairRuntimeAccess 在启动最早期修复"面板自己不在意的那些文件"的归属与权限。
//
// 为什么需要它（真机事故，不是理论问题）：
//
//	面板自身的文件有三类身份来源 —— 安装脚本以 root 写、面板进程按 plist 运行、
//	某些操作又按"真实用户"（config.user）落地。只要这三者漂移一次，就会出现：
//	  · TLS 私钥是 root:600，而这次面板以 zizdog 身份被拉起 → 读证书时报
//	    permission denied，进程在**写下任何日志之前**就退出（launchd 只报
//	    `last exit code = 78: EX_CONFIG`），远程看到的现象就是"升级完面板没了"。
//	  · 日志文件属于另一个身份 → logx.Init 打不开，同样在启动最早期失败。
//
//	两种情况的共同点是：**面板还没有日志可看**，所以排查成本极高。
//	这里在打开日志之前先自我修复一次；修不动的只记警告，绝不阻断启动。
//
// 只在以 root 运行时才改归属：普通用户没有权限，也没必要。
func (c *Config) RepairRuntimeAccess() []string {
	// 面板这次以什么身份在跑：root（uid 0）就不必改归属 —— root 本来就能读能写。
	if os.Geteuid() == 0 {
		return nil
	}
	return c.repairRuntimeAccessFor(os.Geteuid(), os.Getegid())
}

// repairRuntimeAccessFor 是 RepairRuntimeAccess 的可测内核：
// 身份由参数传入，测试里可以模拟"面板不是以这个文件的属主身份运行"。
func (c *Config) repairRuntimeAccessFor(uid, gid int) []string {
	var warnings []string

	// heal 尝试把一个文件修成"当前身份可读/可写"，失败时把原因写进 warnings。
	//
	// 关键：**不能用归属来判断是否已达目标**。文件属于自己、但权限位被改成
	// 000 的情况真实存在（安装脚本、手工 chmod、编辑器另存都可能造成），
	// 那时它依然读不到 —— 而这正是面板会在启动最早期静默退出的原因。
	// 所以一律以"真的能不能打开"为准。
	//
	// 两种修法，按代价从低到高：
	//  1. 自己就是属主 → chmod 恢复权限即可（不需要 root）
	//  2. 不是属主     → 得 chown；非 root 身份做不了，只能如实报出可执行指令
	heal := func(path, what string, mode os.FileMode, usable func(string) bool) {
		if usable(path) {
			return
		}
		if isOwnedBy(path, uid) {
			if err := os.Chmod(path, mode); err == nil && usable(path) {
				warnings = append(warnings, "已修复 "+path+" 的权限（"+what+"，原权限无法打开）")
				return
			}
		}
		if err := os.Chown(path, uid, gid); err == nil && usable(path) {
			warnings = append(warnings, "已把 "+path+" 的归属修正为当前运行身份（"+what+"）")
			return
		}
		warnings = append(warnings, path+" "+what+"，且无法自动修复。请以 root 执行："+
			" chown -R "+c.User+" "+filepath.Dir(path))
	}

	// 日志与运行时文件：打不开日志会让 logx.Init 直接失败，同样发生在最早期。
	for _, dir := range []string{c.LogDir, c.RunDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(dir, e.Name())
			heal(p, "不可写", 0o644, canWrite)
		}
	}

	// TLS 私钥与证书：读不到就必然起不来，且失败得太早、日志都来不及写。
	// 还没生成的（首次安装）直接跳过，交给 tlsx 自己生成。
	for _, f := range []string{c.TLSKey, c.TLSCert} {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			continue
		}
		heal(f, "不可读（TLS 起不来）", 0o600, canRead)
	}

	return warnings
}

// isOwnedBy 判断文件属主是否为指定 uid。
func isOwnedBy(path string, uid int) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(sys.Uid) == uid
}

// canRead / canWrite 用"真的打开一次"来判断权限，
// 而不是解析 mode 位 —— ACL、继承位、SIP 这些用 mode 位都算不准。
func canRead(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func canWrite(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

// App 是应用市场里的一个可安装应用。
//
// 一个 App 描述"怎么把这个应用装起来并纳入管理"，它本身不落库；
// 安装成功后会在 services 表里生成一条 managed=true 的记录。
type App struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Category    string `json:"category"`
	// Kind 决定用哪种方式安装与管理
	Kind Kind `json:"kind"`
	// DockerReference 为 true 表示这是一个「推荐的 Docker 项目」：
	// 面板**不代用户安装**（安装接口会明确拒绝并给出 compose 地址），只在应用市场
	// 的 docker Tab 里列出，并提供预配置的 compose 参考文件（内容 + 镜像站下载地址）。
	//
	// 为什么不直接看 Kind==KindCompose：Kind 描述"历史上用哪种方式装"，
	// 而这个字段描述"面板现在对它承担什么承诺"。两者语义不同，不要合并。
	// 目前目录里**所有** KindCompose 条目都是 true，并有测试锁住
	// （见 compose_reference_test.go 的 TestAllComposeEntriesAreDockerReferences）。
	// 前端的稳定契约：true → 卡片进 docker Tab，隐藏安装按钮。
	DockerReference bool `json:"docker_reference,omitempty"`
	// AdoptLabel 非空表示这是一个"纳管"应用：不安装，只把已存在的服务接进来
	AdoptLabel string `json:"adopt_label"`

	Port        int    `json:"port"`
	HealthPath  string `json:"health_path"`
	HealthExact string `json:"health_exact"`
	LogPath     string `json:"log_path"`

	// UIPort 是"网页界面"端口，**只在它不等于 Port 时**填写。
	//
	// 为什么需要两个端口：frps 的 Port 是协议口 bindPort(7000)，它会和 macOS 的
	// 「隔空播放接收器」抢端口 —— 安装前检查必须查 7000 才有意义；而用户真正要打开的
	// 是 dashboard(7500)，健康检查与反向代理也都该指向它。合成一个字段只能二选一：
	// 要么丢掉 7000 的冲突检查（装完起不来），要么把「打开」指到一个说 frp 协议口
	// 的地址（浏览器直接报错）。
	// 留 0 表示与 Port 相同（绝大多数条目都是这样）。
	UIPort int `json:"ui_port,omitempty"`
	// DirectPort 是「打开 / 直链」按钮应当指向的宿主端口，**只在它不等于 WebPort()
	// 时**填写（留 0 表示用 WebPort()）。
	//
	// 为什么不能直接用 UIPort 兼这个职责：UIPort 同时喂健康检查（healthURLFor）
	// 与反向代理目标。MinIO（2026-09-17 已从目录下架）就是反例 —— 它的 S3 API
	// 在 9010、控制台在 9001，健康检查必须打 9010 的 /minio/health/live（200），
	// 而用户要打开的界面是 9001。把 UIPort 挪到 9001 会让健康检查变成 404，
	// 于是"服务健康"永远红灯。**当前目录里没有条目用它**（唯一使用者 MinIO 已下架），
	// 但字段与 EntryPort() 保留：那是通用能力，不是 MinIO 专用。
	// 所以入口 URL 单独一个字段：它只影响 api_services.go 生成的 port_url。
	DirectPort int `json:"direct_port,omitempty"`
	// ConfigPath 是这个应用的配置文件名（**相对它自己的安装目录**，如 frps.toml）。
	//
	// 非空时，服务详情里会多一个「📝 编辑配置文件」入口：面板直接读写这个文件，
	// 复用既有的 /api/v1/files/read 与 /api/v1/files/write（不新写一套文件读写）。
	// 绝对路径由 services.ConfigFilePath 解析（原生装在用户家目录、compose 装在 work/compose）。
	ConfigPath string `json:"config_path,omitempty"`

	// Native 安装：brew 包名
	BrewFormula string `json:"brew_formula"`
	// ServiceLabel 是面板自研安装器注册的 launchd label。
	// 有了它，"已安装"判断才能对这种应用生效 —— 它既不在 brew 里，
	// 面板记录里的名字也未必等于目录 ID（如记录名 com-zizdog-qwen3tts、
	// 目录 ID qwen3tts），不给出 label 就只能一直显示"未安装"。
	ServiceLabel string `json:"service_label,omitempty"`
	// PanelInstaller 非空表示这个应用由**面板自己的安装器**部署
	// （要建虚拟环境、改 nginx、注册系统级守护进程等，通用 brew/compose
	// 流程做不了），值即安装器标识，web 层据此分流。
	// 有了它，目录校验就不该再要求这类条目必须有 BrewFormula。
	PanelInstaller string `json:"panel_installer,omitempty"`
	// ComposeYAML 是**预配置 compose 参考文件**的内容（DockerReference 条目直接
	// 通过 GET /api/v1/services（市场列表）的 `compose_yaml` 字段给前端"复制"）。
	//
	// 它过去是"安装时写进 <WorkDir>/compose/<id>/docker-compose.yml 的内容"；
	// 2026-09-17 起 Docker 类条目不再由面板安装，它只剩参考用途 ——
	// 同一份内容也会由 `cmd/zizpanel-assets compose` 生成、由
	// `tools/sync-nas-compose.sh` 发布到镜像站 /compose/<id>/docker-compose.yml。
	// 里面用 ${DATA_ROOT:-.} 之类的变量让用户自己决定数据目录（配合 .env.example）。
	ComposeYAML string `json:"compose_yaml"`
	// ComposeSecrets 声明 compose 应用在**安装时**要随机生成的环境变量密钥。
	//
	// 为什么需要它：compose 条目过去只能是一份静态 YAML，任何密钥都写死在模板里
	// （Activepieces 的 AP_JWT_SECRET / Immich 的 DB_PASSWORD 都是这种），
	// 而"全网同一个默认口令"是最危险的一种默认。声明式写法的完整契约见
	// ComposeSecret 与 install.go 的 prepareComposeProject：
	//   · 每次安装只为**不存在的**变量生成一次随机值，写进 <compose 目录>/.env（0600）；
	//   · ComposeYAML 里用 ${VAR} 引用，docker compose 自动读同目录 .env；
	//   · 明文只进安装结果的 Credentials 区块，**绝不**进任务步骤/日志/审计；
	//   · 重装/升级先读已有 .env，有值就复用（否则 Immich 的 DB 口令一变数据库就连不上）。
	ComposeSecrets []ComposeSecret `json:"compose_secrets,omitempty"`
	// 手工安装提示（无法自动安装时给出命令）
	ManualHint string `json:"manual_hint"`

	// Requires 是安装前置条件（用于预检查）
	Requires []Requirement `json:"requires"`
	// PostInstallHint 安装后要做的事（例如拉模型）
	PostInstallHint string `json:"post_install_hint"`
	// DocsURL 官方文档
	DocsURL string `json:"docs_url"`

	// UI 非空表示这个应用有**网页界面**：市场里会给一个「打开」入口，
	// 面板还会在 `/<slug>/` 上给它挂一个反向代理（见 internal/appproxy）。
	// 没有界面的（纯 API / 数据库 / 运行时）留空，免得给出一个打不开的按钮。
	UI *AppUI `json:"ui,omitempty"`
	// SiteApp 非空表示这是「一键建站」类应用：装完得到的是一个**网站**
	// （目录 + 数据库 + 伪静态 + nginx vhost），而不是一个常驻服务。
	// 面板按这个描述自动完成建站流程，用户只需要填域名与管理员账号。
	SiteApp *SiteAppSpec `json:"site_app,omitempty"`
	// NoDaemon 表示这个应用**没有常驻进程**：装完就是一个网页入口
	// （phpMyAdmin 就是这种：nginx alias + php-fpm，没有自己的守护进程）。
	// 有了它，市场就不会把"launchd 里找不到服务"当成异常去吓用户。
	NoDaemon bool `json:"no_daemon,omitempty"`
	// RuntimePath 是这个应用**装出来的真实产物**（安装体 / 运行体）在磁盘上的路径。
	//
	// 为什么需要它：目录里有一批条目**没有常驻服务**（NoDaemon），面板不会给它们
	// 建服务记录（建了就是"永远没有状态的假记录"，ffmpeg 当年就是这么被误报的），
	// 于是它们的「已安装」过去**只**来自 `brew list --versions` 的一句话。
	// 那句话一旦缺席（探测失败/超时、市场缓存还没刷新、用户是手工装的），
	// 判据就整个落空 —— 界面把明明装着的东西显示成「安装」。
	// 2026-09-23 用户报障的正是这一类：
	//   · 「图片压缩（libvips）安装成功后没变化、不在已安装里、还显示安装按钮」；
	//   · 「phpMyAdmin 在应用市场里是未安装状态，很明显是判断错误。它是有状态的
	//      目录，只要判断这个目录在，就是安装！」
	//
	// 判据贴着**运行体**（AGENTS 第三节），不看服务登记、也不看"有没有 plist"：
	//   · 指向**可执行文件** → 文件存在且带可执行位才算（vips / ffmpeg / python3.x）；
	//     悬空软链接（指向已删除的 Cellar）不算 —— 与 BrewStateFor 同一条口径；
	//   · 指向**目录**       → 目录存在，且 RuntimeEntry（入口文件，如 index.php）也在。
	// 支持 `{brew}` 前缀与 `~/` 家目录展开（与 ConfigPath 同一套约定）。
	//
	// ⚠️ 只给"装出来的东西就是它本身"的条目声明（纯 CLI 引擎 / 网页入口）。
	// 面板自研安装器的应用（iopaint / qwen3tts / voicereceiver …）**不要**声明：
	// 它们的虚拟环境/模型在卸载后会作为"残留数据"留下（InstallerArtifactExists），
	// 声明成安装体就会把卡片永久钉在「已安装」上、用户再也点不到「安装」
	//（2026-09-16 用户反馈的"卸载完成后连安装的入口都没有"）。
	RuntimePath string `json:"runtime_path,omitempty"`
	// RuntimeEntry 是 RuntimePath 为**目录**时要求的入口文件（相对路径）。
	// 目录在、入口文件不在 = 没装好（例如 web 根被清空了），不算已安装。
	RuntimeEntry string `json:"runtime_entry,omitempty"`
	// SystemDaemon 表示这个应用**必须开机就在**（数据库 / Web 服务 / 同步守护 /
	// 推理后端 / 站点赖以运行的 PHP-FPM），因此要装成**系统级 LaunchDaemon**
	// （以真实用户身份运行），而不是 brew 默认的用户级 LaunchAgent。
	//
	// 为什么必须区分（坑 130，2026-09-17 mini 真机 5 次重启验证）：无头 macOS
	// （服务器模式、没人进图形会话）开机**不会**加载 ~/Library/LaunchAgents，
	// 用户级服务重启后一个都不会自己回来。判断标准是"面板对用户的承诺是否
	// 依赖它开机就在"，不要凭感觉加；见 internal/services/systemdaemon.go 顶部。
	SystemDaemon bool `json:"system_daemon,omitempty"`
}

// WebPort 返回"网页界面 / 健康检查 / 反向代理"应当指向的端口。
//
// 绝大多数条目 UIPort 为 0，等价于 Port；frps 这种"协议口与界面口不同"的条目
// 填了 UIPort，于是安装前检查仍查协议口、界面入口走 dashboard。
func (a App) WebPort() int {
	if a.UIPort > 0 {
		return a.UIPort
	}
	return a.Port
}

// EntryPort 返回「打开 / 直链」按钮应当指向的宿主端口。
//
// 绝大多数条目与 WebPort() 相同；只有"界面端口 ≠ 健康检查端口"的条目用
// DirectPort 单独指定（历史上只有 MinIO 这样，它已于 2026-09-17 下架）。
// **不要**用它去做健康检查或反代 ——
// 那两件事必须用 WebPort()，理由见 DirectPort 字段的注释。
func (a App) EntryPort() int {
	if a.DirectPort > 0 {
		return a.DirectPort
	}
	return a.WebPort()
}

// ComposeSecret 声明 compose 应用的一个**安装时随机密钥**。
//
// 它只描述"要生成什么"，不携带任何值：值在安装时由 crypto/rand 现生成，
// 写进 <compose 目录>/.env（0600），并且只出现在安装结果的 Credentials 区块里。
type ComposeSecret struct {
	// Env 是环境变量名：既是 .env 里的键，也是 ComposeYAML 里 ${Env} 引用的名字。
	Env string `json:"env"`
	// Encoding 是生成方式，必须是下面三个常量之一：
	//   SecretHex      —— N 字节随机数的 hex（2N 个字符；Activepieces 的
	//                     AP_ENCRYPTION_KEY 要求**恰好 32 个字符**，见条目注释）
	//   SecretBase64   —— N 字节随机数的 URL-safe base64（不含 +/=，能直接进 .env）
	//   SecretPassword —— N 个随机字符（A-Za-z0-9，刻意不含 shell/.env 敏感字符）
	Encoding string `json:"encoding"`
	// Bytes 是随机字节数（hex / base64），或字符数（password）。
	Bytes int `json:"bytes"`
	// Label 是给用户看的说明（会出现在凭据区块里）；留空则显示 Env。
	Label string `json:"label,omitempty"`
}

const (
	// SecretHex 生成 N 字节随机数的 hex 表示。
	SecretHex = "hex"
	// SecretBase64 生成 N 字节随机数的 URL-safe base64（无填充）。
	SecretBase64 = "base64"
	// SecretPassword 生成 N 个字母数字字符（不含 shell / .env 敏感字符）。
	SecretPassword = "password"
)

// composeSecretMinBytes 是生成长度的下限（防止有人填 0 / 负数得到空口令）。
const composeSecretMinBytes = 8

// ConfigFilePath 解析目录条目声明的配置文件的**绝对路径**（没有则返回空串）。
//
// 目录是静态数据（拿不到 UserHome / WorkDir），所以路径按安装位置在运行期拼出来：
//   - compose 类应用：装到 <workDir>/compose/<appID>/（见 installViaCompose）；
//   - 面板自研的 release 二进制类：装到 <userHome>/<RootDir>/（见 binaryReleasePaths）；
//   - 配置文件**不在上述两类位置**的应用（brew 类）：目录里直接写绝对路径或 `~/`
//     开头的路径，这里原样解析 —— 例如 Miniflux 的配置位置由 formula 的 service 块
//     写死在 /opt/homebrew/etc/miniflux.conf，Syncthing 的在
//     ~/Library/Application Support/Syncthing/config.xml。这两条路径面板改不了，
//     只能如实声明；不声明的话服务详情里就没有「📝 编辑配置文件」入口。
//
// 用途只有一个：服务详情里的「编辑配置文件」入口要知道读哪个文件 ——
// 读写的鉴权/白名单仍由既有的 files.Manager 负责。
func ConfigFilePath(app App, userHome, workDir string) string {
	if app.ConfigPath == "" {
		return ""
	}
	if app.Kind == KindCompose || app.Kind == KindDocker {
		if workDir == "" {
			return ""
		}
		return filepath.Join(workDir, "compose", app.ID, app.ConfigPath)
	}
	// {brew} 占位符：Homebrew 前缀在 Apple Silicon 是 /opt/homebrew、Intel 是 /usr/local。
	// 配置文件路径由 formula 决定，写死前缀在另一种架构上就是错的（点开编辑器报
	// "文件不存在"，用户以为面板坏了）。前缀从环境推导，推导不到就退回 /opt/homebrew。
	if strings.HasPrefix(app.ConfigPath, "{brew}/") {
		return filepath.Join(brewPrefixDefault(), strings.TrimPrefix(app.ConfigPath, "{brew}/"))
	}
	// 绝对路径（含 ~/ 展开）：brew 类应用的配置文件位置由 formula 决定，
	// 不在家目录下的安装目录里。
	if filepath.IsAbs(app.ConfigPath) {
		return app.ConfigPath
	}
	if strings.HasPrefix(app.ConfigPath, "~/") {
		if userHome == "" {
			return ""
		}
		return filepath.Join(userHome, strings.TrimPrefix(app.ConfigPath, "~/"))
	}
	if spec, ok := releaseBinaryApps[app.PanelInstaller]; ok && userHome != "" {
		return filepath.Join(userHome, spec.RootDir, app.ConfigPath)
	}
	return ""
}

// brewPrefixDefault 返回本机 Homebrew 前缀（推不出来时退回 Apple Silicon 的默认值）。
//
// 顺序：环境变量 → 磁盘上真实存在的那个。**不看架构猜**：Intel 机器上装的是
// /usr/local，写死 /opt/homebrew 会让"编辑配置文件"点开就是"文件不存在"。
func brewPrefixDefault() string {
	if p := strings.TrimSpace(os.Getenv("HOMEBREW_PREFIX")); p != "" {
		return p
	}
	for _, p := range []string{"/opt/homebrew", "/usr/local"} {
		if fi, err := os.Stat(filepath.Join(p, "bin", "brew")); err == nil && !fi.IsDir() {
			return p
		}
	}
	return "/opt/homebrew"
}

// AppUI 描述一个应用的网页界面，以及"把它挂到子路径下"需要知道的事。
//
// 为什么要有 Rewrites：绝大多数前端是用**绝对路径**引资源的
// （`<script src="/assets/index-xxx.js">`、`fetch("/api/v1")`、`io("/socket.io")`）。
// 挂在 `/iopaint/` 下时，这些请求会打到站点根路径 → 页面白屏或接口 404。
// 所以每个应用要声明"哪些绝对前缀需要改写到子路径下"，由面板统一改写
// （见 internal/appproxy：响应体 + Location 响应头；真机实测过 IOPaint 与 Uptime Kuma）。
//
// 为什么要有 Note：子路径不是万能的 —— 有些应用必须自己在配置里设置
// root_url / base path，光靠改写会坏。这种情况如实写出来，
// 界面会在「打开」（子路径）上加 ⚠️ 与这段实话，并让「直链」承担真正的入口
// （而不是把「打开」偷偷换成直连，或不给任何提示地给一个点开白屏的地址）。
type AppUI struct {
	// Slug 是挂在面板与 nginx 上的子路径（不带斜杠），例如 iopaint
	Slug string `json:"slug"`
	// Target 是应用内部的入口路径，默认 "/"
	Target string `json:"target,omitempty"`
	// Rewrites 是响应体里的绝对路径改写规则，To 里可用 {slug} 占位
	Rewrites []UIRewrite `json:"rewrites,omitempty"`
	// Websocket 表示界面需要 WebSocket（socket.io / ws），代理必须放行 Upgrade
	Websocket bool `json:"websocket,omitempty"`
	// SelfBase 表示**应用自己就会带上级路径**（例如 File Browser 的 `-b /filebrowser`）。
	// 面板这时**不做任何路径改写** —— 否则会双重加前缀：
	// 应用生成 /filebrowser/static/assets/x.js，面板再改写成
	// /filebrowser/static/filebrowser/assets/x.js，资源全 404（真机实测）。
	SelfBase bool `json:"self_base,omitempty"`
	// SelfConf 表示**安装器自己已经写了 nginx location**（如 phpMyAdmin 的 alias），
	// 面板不要再生成代理，只提供「打开」入口。
	SelfConf bool `json:"self_conf,omitempty"`
	// ConsoleOnly 表示这个网页界面是**应用自带的控制台**（frpc 的 admin UI 这类），
	// 不是本面板提供的"使用入口"。
	//
	// 用户明确要求（2026-09-16）：不要在设计里让用户跳到 frpc:7400 / Orbien:8020
	// 这种自带控制台网页去管理 —— 那是应用自己的东西，不是面板的职责。
	// 这类应用在「应用市场」与「服务管理」里只给两个动作：
	// 「📝 编辑配置文件」+「🔄 重启服务」，用户完全不必 SSH、也不必开外部网页。
	//
	// 别和 PreferDirect 混了：n8n / Gitea / Stirling 是**应用本体**，用户就是要
	// 打开它，只是子路径挂不上、得走端口直连 —— 那些仍然保留「打开」。
	ConsoleOnly bool `json:"console_only,omitempty"`
	// Headers 是反代时要附加（或覆盖）的响应头。
	//
	// 为什么需要：有些前端不是"路径不对"，而是**缺安全头就跑不起来** ——
	// Squoosh 的编解码器要用 SharedArrayBuffer，浏览器要求页面处于
	// crossOriginIsolated 状态（即带 COOP/COEP）；少了这两个头，它会
	// 直接报 "Failed to load app"，而页面本身与资源请求全都是 200。
	Headers map[string]string `json:"headers,omitempty"`
	// PreferDirect 表示"这个应用挂子路径实测不可用（或需要它自己先配置 base path）"。
	//
	// 与 Note 的区别：Note 只是说明，PreferDirect 会改变界面呈现 ——
	// 「打开」（面板子路径 /<slug>/）仍然保留，但按钮上加 ⚠️ 并把 Note 里的实话
	// 写进 title，提示用户改用旁边的「直链」（端口直连）。
	//
	// **不要**再用它（或 /api/v1/market/proxies 的探测结果）去调换「打开」与
	// 「直链」的归属：用户 2026-09-17 明确要求这两颗按钮语义固定（打开＝子路径，
	// 直链＝port_url），而且那个探测不带面板会话（子路径返回 401），proxy_ok
	// 几乎恒为 false —— 拿它决定归属会把 it-tools 这类应用的「打开」错变成直连。
	// 为什么需要人工标一项：自动探测只能发现"资源 404"这类问题，
	// 发现不了"资源都 200、但前端路由不认这个路径"（Uptime Kuma 就是这样：
	// 页面标题都对、请求全 200，正文却是 Page Not Found）。
	// 这一条由真机实测得出，别凭猜改。
	PreferDirect bool `json:"prefer_direct,omitempty"`
	// Note 是给用户看的实话：这条子路径有什么前提或限制
	Note string `json:"note,omitempty"`
}

// UIRewrite 是一条字面量替换规则。
type UIRewrite struct {
	From string `json:"from"`
	// To 支持 {slug} 占位符
	To string `json:"to"`
}

// SiteAppSpec 描述一个"一键建站"应用怎么装。
//
// 为什么单独一类而不是塞进 ComposeYAML/BrewFormula：这类应用既不是服务也不是容器，
// 它的产出是一个**站点**（www/<域名> 下的文件 + 一个数据库 + 一段伪静态）。
// 安装流程也因此完全不同（见 internal/web/api_site_apps.go）：
// 下载源码 → 解压 → 建库建用户 → 写配置文件 → 建站点（带伪静态）→ 探测首页。
type SiteAppSpec struct {
	// DownloadURL 是主下载地址；MirrorURLs 是备用地址（国内网络下主地址常常很慢）
	DownloadURL string   `json:"download_url"`
	MirrorURLs  []string `json:"mirror_urls,omitempty"`
	// Archive: zip / tar.gz
	Archive string `json:"archive"`
	// StripTopDir 表示压缩包里有一层顶层目录需要剥掉（如 typecho/）
	StripTopDir bool `json:"strip_top_dir,omitempty"`
	// Rewrite 是伪静态预设名（与 sites.RewritePresets 的 name 对应）
	Rewrite string `json:"rewrite"`
	// FinishPath 是安装向导入口（装完提示用户去这里继续）
	FinishPath string `json:"finish_path"`
	// NeedsDB 表示要建数据库与用户
	NeedsDB bool `json:"needs_db"`
	// MinPHP 是这个应用要求的最低 PHP 版本（如 "8.2"）；空 = 不检查版本。
	//
	// 一键建站会在**建目录之前**用本机该版本真实的 php 二进制复核（php -r 'echo PHP_VERSION;'），
	// 不满足就直接拒绝安装 —— 绝不装出一个打不开的站点（见 api_site_apps.go 的前置检查）。
	MinPHP string `json:"min_php,omitempty"`
	// PHPExts 是必需的 PHP 扩展（`php -m` 里的条目名）；空 = 不检查扩展。
	// 缺任何一个都在安装前拒绝，并把"缺什么"写清楚。
	PHPExts []string `json:"php_exts,omitempty"`
	// Notes 是给用户的关键提醒（数据库名/密码怎么填等）
	Notes []string `json:"notes,omitempty"`
}

// Requirement 是一条前置条件。
type Requirement struct {
	// Type: brew / command / docker / font / python
	Type  string `json:"type"`
	Value string `json:"value"`
	Hint  string `json:"hint"`
}

// Preflight 是一次安装前检查的结果。
type Preflight struct {
	App      string        `json:"app"`
	Ready    bool          `json:"ready"`
	Checks   []CheckResult `json:"checks"`
	PortFree bool          `json:"port_free"`
	PortNote string        `json:"port_note"`
	// AlreadyInstalled 表示"这个应用已经装好了"：注册表里有它的记录，
	// 或者它的端口正被它自己的服务占用。界面据此把按钮显示成"已安装"，
	// 而不是给出一个点下去只会被跳过的「安装」。
	//
	// 为什么单独一个字段：端口被自己占用时 PortFree 是 true、PortNote 也不是
	// 冲突描述，只看这两个字段分不清"端口空闲"与"已安装"。
	AlreadyInstalled bool `json:"already_installed"`
}

// CheckResult 是单条检查结果。
type CheckResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
	Fixable bool   `json:"fixable"`
	FixCmd  string `json:"fix_cmd"`
}

// ============================================================================
// 应用分类：key 与中文显示名的**唯一来源**
//
// App.Category 存的是下面这些 **key**；中文名只在本文件里定义一次。
//
// 为什么必须集中定义：应用市场页曾经在前端硬编码了一份分类中文名
// （apps.js 里的 `{ key: 'other', label: '其它' }`），与后端漂移了也没人发现。
// 用户要求把市场最后一个板块「其它」改名为「基础环境」—— 如果前端再抄一份，
// 下次改名还会漂。现在板块顺序与显示名都由 MarketSections() 给出，
// 前端只按 key 分组、不再写任何分类中文名。
// ============================================================================
const (
	CategorySite = "site" // 一键建站
	CategoryAI   = "ai"   // AI 服务
	CategoryTool = "tool" // 运维工具
	// CategoryLNMP 是「网站环境」：应用市场的一个专属板块，同时是服务管理里
	// nginx / PHP / MySQL / PostgreSQL 的归类。**一个 key 管两处**，不再另造
	// 一个同义 key（两个 key 同一个中文名必然漂）。
	//
	// 为什么 PostgreSQL 也在这里（2026-09-19 产品负责人拍板）：它虽然目前只服务
	// 自托管应用（Miniflux），但和 MySQL 一样是**数据库组件**，与 nginx/PHP 同属
	// "网站/应用运行环境"，不该再混在「基础环境」里（那是 CLT/Homebrew/ffmpeg
	// 这类跨应用、与网站无关的运行依赖）。
	CategoryLNMP    = "lnmp"    // 网站环境（nginx / PHP / MySQL / PostgreSQL）
	CategoryRuntime = "runtime" // 容器运行时（Docker 运行时 Colima）
	// CategoryOther 同时是**兜底分类**：目录里没有专属板块的分类
	// （runtime / 以及将来新增的）都会被它收下。
	// 显示名取"最能描述这一整类"的 —— 用户 2026-09 要求显示为「基础环境」，
	// 2026-09-19 起它只收纳**跨应用依赖**（CLT / Homebrew / ffmpeg、Colima 运行时）。
	CategoryOther = "other"
)

// categoryLabels 是分类 key → 中文显示名的唯一字典。
//
// 只在本包内使用；导出的是副本（CategoryLabels），避免调用方改动它。
var categoryLabels = map[string]string{
	CategorySite:    "一键建站",
	CategoryAI:      "AI 服务",
	CategoryTool:    "运维工具",
	CategoryLNMP:    "网站环境",
	CategoryRuntime: "容器运行时",
	CategoryOther:   "基础环境",
	"custom":        "自定义",
}

// MarketSection 描述应用市场里的一个板块。
type MarketSection struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Fallback 为 true 表示这是**兜底板块**：收纳所有没有专属板块的分类。
	Fallback bool `json:"fallback,omitempty"`
}

// MarketSections 返回应用市场板块的**顺序与显示名**，是前端的唯一数据源。
//
// 三个硬约束：
//  1. 显示名来自 categoryLabels（与 CategoryLabels 同源）——"改一处、到处生效"；
//  2. 兜底板块必须排在**最后**（用户明确要求「基础环境」仍在最后一个板块）。
//     它收纳的是没有专属板块的分类，排在中间会让后面的板块与它混在一起；
//  3. 「网站环境」（CategoryLNMP）必须是一个**专属板块**：nginx / PHP / MySQL /
//     PostgreSQL 属于网站环境层，不能再落进「基础环境」兜底板块
//     （那是 CLT/Homebrew/ffmpeg 这类跨应用运行依赖的地盘，2026-09-19 拆分）。
func MarketSections() []MarketSection {
	keys := []string{CategorySite, CategoryAI, CategoryTool, CategoryLNMP, CategoryOther}
	out := make([]MarketSection, 0, len(keys))
	for _, k := range keys {
		out = append(out, MarketSection{
			Key:      k,
			Label:    categoryLabels[k],
			Fallback: k == CategoryOther,
		})
	}
	return out
}

// CategoryLabels 返回分类 key → 中文显示名的副本（服务列表接口用）。
//
// 与 MarketSections 共用 categoryLabels：应用市场改名时，服务管理那边
// 同一个分类的显示名自动跟着变，不会只改一处。
func CategoryLabels() map[string]string {
	out := make(map[string]string, len(categoryLabels))
	for k, v := range categoryLabels {
		out[k] = v
	}
	return out
}

// Catalog 返回内置应用目录。
//
// 选品原则：优先"这台机器上真的用得上、且能自动装起来"的。
// 每个条目的端口、健康检查路径、日志路径都经过实际验证，
// 而不是从文档里抄一个大概 —— 端口写错会让用户装完发现打不开。
func Catalog() []App {
	audioHealth := "/v1/models"
	return []App{
		// ---------------- 面板一键部署（自研安装器） ----------------
		//
		// 这几个项目的安装逻辑不是通用的 brew/compose 流程，而是本项目
		// 自己的安装器（见 internal/services 里的 iopaint.go / qwentts.go /
		// voicereceiver.go / phpmyadmin.go）。写进目录是为了让它们在
		// **应用市场里可见、状态可追踪**，而不是只藏在页头的按钮里 ——
		// 用户找不到的功能等于不存在。
		//
		// 真正的安装由 handleMarketInstall 按 ID 路由到对应安装器
		// （见 PanelInstaller 字段）；"已安装"则靠 ServiceLabel
		// 或 BrewFormula 判断。
		// 描述只留用户要决定的：原生安装 + 加速 + 模型约 200MB。安装用 LaMa 模型并启用 MPS；
		// 更强的 MAT / ZITS / LDM 在 M 系列上不支持 MPS，所以不推荐。
		{
			ID: "iopaint", Name: "IOPaint（图片去水印）", Icon: "🖼️",
			// 子路径：真机实测（mini，2026-09-14）。它的前端把 API 与 socket.io
			// 都写成绝对路径，资源在 /assets/ 下，所以四条改写缺一不可。
			UI: &AppUI{
				Slug:      "iopaint",
				Websocket: true,
				Rewrites: []UIRewrite{
					{From: `"/api/v1"`, To: `"/{slug}/api/v1"`},
					{From: "/socket.io", To: "/{slug}/socket.io"},
				},
			},
			Summary:     "AI 擦除水印与杂物，支持批量与视频",
			Description: "上传图片涂抹即可去水印，原生安装并启用 Apple Silicon 加速；模型约 200MB。",
			Category:    "tool", Kind: KindNative, PanelInstaller: "iopaint", ServiceLabel: "com.zizdog.iopaint",
			Port: 8080,
			// IOPaint 的**视频**去水印路径要 ffmpeg 拆帧/合帧（市场描述里的"支持视频"
			// 就是它）；图片路径不需要。面板不直接调用 ffmpeg，所以这里只作提示 +
			// 部署时一并装好 —— 装上不会有副作用（它本来就是基础环境）。
			Requires: []Requirement{
				{Type: "brew_formula", Value: "ffmpeg",
					Hint: "brew install ffmpeg（IOPaint 的视频处理需要它，安装时会一并安装）"},
				{Type: "brew_formula", Value: "python@3.11",
					Hint: "brew install python@3.11（IOPaint 跑在自己的 Python venv 里，安装时会一并安装）"},
			},
			DocsURL: "https://github.com/Sanster/IOPaint",
		},
		// 按 TtsVoice 插件契约原生安装（Python 3.11 + mlx-audio[server]）：插件只支持
		// 「自定义音色」，所以只需要 1.7B-Base-8bit 这一个模型（约 2.9GB）；Qwen 加鉴权后
		// 只监听本机，对外由音色接收端（8899）带密钥反代。
		{
			ID: "qwen3tts", Name: "Qwen3 TTS（语音合成）", Icon: "🗣️",
			Summary:     "本地语音合成，支持音色克隆",
			Description: "本地语音合成，支持音色克隆；首次安装要下载约 2.9GB 模型。",
			Category:    "ai", Kind: KindNative, PanelInstaller: "qwen3tts", ServiceLabel: "com.zizdog.qwen3tts",
			Port:       8880,
			HealthPath: audioHealth,
			// ffmpeg 是它的**硬依赖**：mlx_audio 编码 mp3 必须靠它，缺了就会返回
			// HTTP 200 + 0 字节 body（2026-09-16 事故的确切成因）。用目录既有的
			// Requires 机制声明 —— 安装前检查会提示，部署时由
			// EnsureBaseDependencies 一并装好，不需要前端配合新字段。
			// 依赖要**在这里全部声明**（用户 2026-09-18 报障："它依赖 ffmpeg，却没有
			// 安装 ffmpeg，并且应该给出提示，知道会一并安装" / "Python 3.11 也是 tts 等的
			// 依赖，也没有一并安装"）——声明了才会出现在安装前的检查里、才会在任务日志
			// 里事先说明"将一并安装"，安装器也才会真的把它们装好并复核。
			// python@3.11 不是偏好：mlx-audio 只在 cp311 有预编译 wheel（见 qwentts.go）。
			Requires: []Requirement{
				{Type: "brew_formula", Value: "ffmpeg",
					Hint: "brew install ffmpeg（TTS 编码 mp3 必需，部署时会一并安装）"},
				{Type: "brew_formula", Value: "python@3.11",
					Hint: "brew install python@3.11（mlx-audio 只在 3.11 有预编译 wheel，部署时会一并安装）"},
			},
			DocsURL: "https://github.com/Blaizzy/mlx-audio",
		},
		// 样本必须落到本机磁盘 —— 上游只接受本地文件路径，不能把上传流直接转发过去。
		{
			ID: "voicereceiver", Name: "TtsVoice 音色接收端", Icon: "🔐",
			Summary:     "接收音色样本 + 带鉴权的反向代理",
			Description: "网站插件用的对外入口：接收音色样本并带密钥转发给本机 TTS。",
			Category:    "ai", Kind: KindNative, PanelInstaller: "voicereceiver", ServiceLabel: "com.zizdog.voicereceiver",
			Port: 8899,
			// 接收端有免鉴权的 GET /health（实测 200；/jobs 等才要密钥）。
			// 这里原先**没配** HealthPath —— 后果是面板"纳管了却不监测"：
			// 服务列表里它的 health 永远是 checked=false/ok=false，
			// 用户看到的就是"TTS 没在管理/不知道死活"（2026-09-16 用户反馈）。
			HealthPath: "/health",
			// 接收端自己就用 ffmpeg / ffprobe（样本真解码校验 + 转码与归一），
			// 同时它是上游 Qwen 编码 mp3 的必经环节。用目录既有的 Requires 声明，
			// 安装前检查会提示，部署时由 EnsureBaseDependencies 一并装好。
			Requires: []Requirement{
				{Type: "brew_formula", Value: "ffmpeg",
					Hint: "brew install ffmpeg（样本校验与音频转码必需，部署时会一并安装）"},
				{Type: "brew_formula", Value: "python@3.11",
					Hint: "brew install python@3.11（接收端跑在自己的 Python venv 里，部署时会一并安装）"},
			},
			// 契约来源是网站侧插件目录里的 HANDOFF-TO-MINI.md。
			// 这里原先错填成 phpmyadmin.net（复制粘贴残留），会把人引到无关文档。
			DocsURL: "https://github.com/Blaizzy/mlx-audio",
		},
		{
			ID: "phpmyadmin", Name: "phpMyAdmin", Icon: "🐬",
			// phpMyAdmin 没有守护进程（nginx alias + php-fpm），装完就是一个网页入口。
			// 它的 nginx location 由安装器自己写，所以 SelfConf=true：面板只给「打开」。
			UI:          &AppUI{Slug: "phpmyadmin", SelfConf: true},
			NoDaemon:    true,
			Summary:     "数据库管理界面（推荐入口）",
			Description: "库表管理界面（面板自带的只作应急）；依赖 nginx + PHP，装完从面板打开。",
			Category:    "tool", Kind: KindNative, PanelInstaller: "phpmyadmin", BrewFormula: "phpmyadmin",
			// 安装体 = **web 根目录**（不是服务、也不是 brew 记录）：
			// 用户 2026-09-23 的原话是"它是有状态的目录，只要判断这个目录在，就是安装"。
			// 路径与 internal/services/phpmyadmin.go 的 pmaPaths().Share 同源。
			RuntimePath:  "{brew}/share/phpmyadmin",
			RuntimeEntry: "index.php",
			Port:         0,
			HealthPath:   "/phpmyadmin/",
			DocsURL:      "https://www.phpmyadmin.net",
		},

		// ---------------- 网站环境（LNMP，原生安装） ----------------
		//
		// 2026-09-19 起这是应用市场里的一个**专属板块**「网站环境」
		// （category key = CategoryLNMP，见 MarketSections）：nginx / 各版本 PHP /
		// MySQL / PostgreSQL 都归它。它与「基础环境」（CLT/Homebrew/ffmpeg 这类
		// 跨应用运行依赖）是两层，探测与安装都分开
		// （见 baseenv.go 的 BaseEnvStatus / EnsureBaseEnvironment）。
		//
		// ⚠️ 这里只有**单个组件**（nginx / 各版本 PHP / MySQL / PostgreSQL），
		// **没有** "lnmp" 这个条目 —— 「一键 LNMP」是一个组合动作
		// （装三个包 + 默认站点 / vhosts 目录 / MySQL 初始化 / 系统级守护进程
		// 等收尾工作，见 internal/services/lnmp.go），不是"一个可安装的应用"。
		//
		// 所以它本来就不会渲染成应用市场的卡片（市场只遍历 Catalog()）；
		// 它的界面入口在「网站管理」页（站点为空时的大按钮 / 站点非空时的
		// 工具条按钮，见 sites.js），后端能力仍是 POST /api/v1/market/install-lnmp。
		// 市场上还刻意留了一条防御性排除（internal/web 的 marketHiddenApps），
		// 万一将来有人真把 lnmp 加进目录，也不会在市场里冒出一张重复入口的卡片。
		//
		// 为什么这三条必须存在：面板最核心的功能是"网站管理"与"数据库"，
		// 而它们依赖 nginx / PHP-FPM / MySQL。早期版本的安装脚本只提示
		// "去应用市场装"，可市场里根本没有这三条 —— 用户会在那里白找一圈，
		// 然后发现面板的主功能是空的。安装路径（brew install + brew services）
		// 早就有了，缺的只是条目。
		//
		// 顺序有意义：nginx 在最前，它是网站功能的入口。
		// 装好后面板会自动补齐 include 上下文与 WebSocket 升级映射，用户不必手改 nginx.conf。
		{
			ID: "nginx", Name: "Nginx", Icon: "🌐",
			Summary:     "Web 服务器，网站管理功能的基础",
			Description: "Web 服务器，「网站管理」的基础：新建站点、伪静态、SSL 都由它提供。",
			Category:    "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.nginx",
			Port: 80, HealthPath: "/",
			BrewFormula: "nginx",
			// 手动改 nginx 的入口（用户 2026-09-18："面板也要有手动改 php 和 nginx 的入口"）：
			// 管理面板里的「📝 编辑配置文件」直接打开这一份。
			ConfigPath: "{brew}/etc/nginx/nginx.conf",
			LogPath:    "~/Library/Logs/homebrew.mxcl.nginx.log",
			DocsURL:    "https://nginx.org",
		},
		// 面板会把它配置成监听自己专属的端点（默认 Unix socket
		// /opt/homebrew/var/run/php-fpm-8.2.sock），不会与其它版本抢 9000 端口；
		// 「修复端点」会改写该版本的 www.conf 并重启 fpm。
		{
			ID: "php82", Name: "PHP 8.2 (FPM)", Icon: "🐘",
			Summary:     "PHP FastCGI 进程管理器，供站点解析 PHP",
			Description: "PHP-FPM 8.2，供站点解析 PHP；与其它版本共存，端点未配置时点「修复」。",
			Category:    "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.php@8.2",
			// PHP-FPM 说的是 FastCGI 协议、不是 HTTP：不能做 HTTP 健康检查，端点由面板按版本分配。
			Port:        0,
			HealthPath:  "",
			BrewFormula: "php@8.2",
			// 手动改 PHP 的入口：该版本的 php.ini（php-fpm 的 www.conf 在「文件管理」里也能改）。
			ConfigPath: "{brew}/etc/php/8.2/php.ini",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			LogPath:      "~/Library/Logs/homebrew.mxcl.php@8.2.log",
			DocsURL:      "https://www.php.net",
		},
		// 面板会把它配置成监听自己专属的端点（默认 Unix socket
		// /opt/homebrew/var/run/php-fpm-8.4.sock），不会与其它版本抢 9000 端口；
		// 「修复端点」会改写该版本的 www.conf 并重启 fpm。
		{
			ID: "php84", Name: "PHP 8.4 (FPM)", Icon: "🐘",
			Summary:     "PHP FastCGI 进程管理器，供站点解析 PHP",
			Description: "PHP-FPM 8.4，供站点解析 PHP；与其它版本共存，端点未配置时点「修复」。",
			Category:    "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.php@8.4",
			// PHP-FPM 说的是 FastCGI 协议、不是 HTTP：不能做 HTTP 健康检查，端点由面板按版本分配。
			Port:        0,
			HealthPath:  "",
			BrewFormula: "php@8.4",
			ConfigPath:  "{brew}/etc/php/8.4/php.ini",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			LogPath:      "~/Library/Logs/homebrew.mxcl.php@8.4.log",
			DocsURL:      "https://www.php.net",
		},
		// macOS 上 brew 版默认用 /tmp/mysql.sock（不是 TCP 3306），连接时按这个来。
		{
			ID: "mysql84", Name: "MySQL 8.4", Icon: "🐬",
			Summary:     "关系型数据库，供站点与面板的数据库管理使用",
			Description: "MySQL 8.4，供站点与面板的数据库管理使用；brew 版 root 初始无密码。",
			Category:    "lnmp", Kind: KindNative, ServiceLabel: "sh.brew.mysql@8.4",
			Port: 3306, HealthPath: "",
			BrewFormula: "mysql@8.4",
			ConfigPath:  "{brew}/etc/my.cnf",
			LogPath:     "~/Library/Logs/homebrew.mxcl.mysql@8.4.log",
			DocsURL:     "https://dev.mysql.com",
		},

		// ---------------- 基础环境（原生安装，跨应用运行依赖） ----------------
		//
		// 2026-09-19 产品负责人拆分了「基础环境」与「网站环境」两层：
		//   · 本板块（基础环境 / CategoryOther 兜底）= 运行依赖层：
		//     CLT → Homebrew → ffmpeg（python3 随 CLT 来，不单列）；
		//   · 「网站环境」（CategoryLNMP）= nginx / PHP / MySQL / PostgreSQL。
		// 所以这里只剩 ffmpeg 这类**跨应用、与网站无关**的依赖
		// （Colima 容器运行时没有专属板块，也落在本兜底板块）。
		//
		// ffmpeg 不属于任何单个应用，但很多功能都靠它：
		//   · Qwen3 TTS 用 mlx_audio 编码 mp3 **必须**有它；
		//   · 音色接收端用 ffprobe 真解码校验上传样本、用 ffmpeg 转码与归一；
		//   · IOPaint 的视频处理、后续的音视频功能也要用。
		//
		// 为什么值得单独一个条目（用户明确要求"有单独安装入口，作为一个独立软件"）：
		// 2026-09-16 真机事故就是它被弄丢（头号嫌疑是卸载流程里的 brew autoremove
		// 把共享依赖一起带走）。服务照样启动、健康检查全绿，只有合成 mp3 时返回
		// HTTP 200 + 0 字节 body —— 用户所有 TTS 作业全败却看不出问题在哪。
		// 有独立条目，用户才能主动查、主动装、主动补。
		//
		// 它是纯命令行工具：UI 必须留空（给了 UI 市场就会渲染一个点开必然打不开的
		// 「打开」按钮），也不该有常驻进程与端口。
		// PanelInstaller 指向面板自己的安装器（handleMarketInstall 按 ID 分流）：
		// 走它而不是通用 brew 流程，是因为 ffmpeg **没有 brew service** ——
		// 通用流程会去 `brew services start ffmpeg`，得到一个"启动了但启动失败"的
		// 假警告，还会在服务管理里留下一条永远没有状态的假记录。
		{
			ID: "ffmpeg", Name: "FFmpeg（音视频工具）", Icon: "🎬",
			Summary:     "音视频转码基础工具（TTS 编码 mp3 依赖它）",
			Description: "音视频转码命令行工具；Qwen TTS 编码 mp3 必须依赖它，没有网页界面。",
			Category:    CategoryOther, Kind: KindNative,
			PanelInstaller: "ffmpeg",
			BrewFormula:    "ffmpeg",
			// 纯命令行工具：没有守护进程、没有端口、没有网页界面。
			NoDaemon: true,
			// 安装体 = brew 前缀下的可执行文件（"装了没有"贴着它判，不看服务登记）。
			RuntimePath: "{brew}/bin/ffmpeg",
			Port:        0,
			DocsURL:     "https://ffmpeg.org",
		},
		// 图片压缩（libvips）。用户 2026-09-18 要求"为软件市场添加一个图片压缩软件"
		//（附了一份 govips 的参考文档）。
		//
		// 为什么是 brew 的 vips 命令行、而不是面板里链 govips（CGO 绑定）：
		//   · 面板是纯 Go 单二进制（交叉构建 arm64/amd64、-trimpath 注版本号），
		//     链路里没有 CGO；更要紧的是运行期 —— CGO 会把 libvips 的动态库绑到
		//     面板进程上，用户机器少一个 dylib 面板就起不来。管所有东西的进程
		//     不能这样冒险。`vips` 命令行就是 libvips 本身（与 govips/sharp 同一引擎），
		//     导出参数逐字可用，代价只是"每个文件一个进程"（几十毫秒）。
		//   · 它**没有 brew service**（纯 CLI），所以走面板自研安装器而不是通用
		//     brew 流程（否则会 brew services start 一个没有 service 的定义，留下
		//     "已安装但启动失败"的假警告 —— 与 ffmpeg 同一条理由）。
		// 界面（2026-09-23 用户要求"给它加一个 webui 通过端口和别名调用"）：
		//   · 一个面板托管的常驻网页界面 —— 独立端口 8890（直连
		//     http://127.0.0.1:8890/），服务由安装器注册成系统级 launchd
		//     （com.zizdog.imgcompress，见 imgcompress.go）；
		//   · 同时挂面板别名 /imgcompress/（UI.Slug，由 internal/appproxy 反代），
		//     所以经 nginx 的 http://<主机>/imgcompress/ 也能用。
		// 两个入口是同一份界面（由 cmd/zizpanel 的 `imgcompress-serve` 子命令内嵌提供）。
		{
			ID: "imgcompress", Name: "图片压缩（libvips）", Icon: "🖼️",
			UI:      &AppUI{Slug: "imgcompress"},
			Summary: "批量把图片压小（WebP / AVIF / JPEG/PNG），拖进去就能压，带进度与下载",
			Description: "用 libvips 批量压缩图片：拖入多张，可调质量/最长边/格式，" +
				"显示压前压后大小并可下载。带独立网页界面，也可在文件管理里按目录跑。",
			Category: CategoryOther, Kind: KindNative,
			PanelInstaller: "imgcompress",
			BrewFormula:    "vips",
			ServiceLabel:   ImgCompressLabel,
			// 安装体仍是 `<brew>/bin/vips` 这个可执行文件（与 internal/imgopt 的
			// DetectEngine 同一条口径）：网页界面只是它的一个调用者，
			// "引擎在不在"才是这个应用的能力判据。
			RuntimePath: "{brew}/bin/vips",
			Port:        ImgCompressPort,
			HealthPath:  "/healthz",
			// ⚠️ 刻意**不填** SystemDaemon：那个字段驱动的是"把 brew services 的
			// 用户级 agent 搬进系统域"这条迁移路径，而 vips 没有 brew service。
			// 这里的网页界面由面板安装器**自己**写系统级 LaunchDaemon
			// （RunAtLoad=true），开机自启不依赖 SystemDaemon 字段。
			PostInstallHint: "网页界面：直连 http://127.0.0.1:8890/ ，或从卡片点「打开」走面板别名 /imgcompress/。" +
				"也可以到「文件管理」选中目录点工具条上的「🖼️ 图片压缩」批量处理目录：" +
				"可调质量 / 最长边 / 输出格式（保持原格式、WebP、AVIF、JPEG、PNG）。" +
				"默认**另存为 xxx.min.jpg**（不动原文件）；压完更大时会自动保留原文件。",
			DocsURL: "https://www.libvips.org/",
		},
		// macOS 语音合成（say）。用户 2026-09-25 要求"评估 macos-speech-server
		// 加入应用市场，并写好 webui"。
		//
		// 评估结论（完整版见 docs/应用市场-speech评估.md）：**用系统自带的 say**。
		//   · 零安装、零下载、零运行时：/usr/bin/say 是 macOS 的一部分
		//     （Mach-O universal，含 arm64e），比"Homebrew 原生"更原生；
		//   · 完全离线：本机合成，不下载模型、不联网；
		//   · 中文可用：`say -v '?'` 里有 zh_CN / zh_TW / zh_HK 音色；
		//   · 落选者：dokterbob/macos-speech-server（同名项目：Swift/AGPL-3.0，
		//     第三方 tap，首次启动要下 ~700MB FluidAudio 模型）、
		//     alvinj/MacSpeechServer（Scala/JVM，2015 年后没动过，无 arm64 产物）、
		//     easychen/miniaiapi（Node 18 + Python + ffmpeg，不是"一个可装的包"）、
		//     soniqo/speech-swift 的 brew `speech`（Apache-2.0、有 arm64 瓶，
		//     但 HTTP 服务只到 /v1/audio/transcriptions + /v1/realtime，
		//     没有 OpenAI 兼容的 /v1/audio/speech，且 TTS 要下模型
		//     ——它是"要更高音质时"的升级路径，不是本条的依赖）。
		//
		// 与已有的 Qwen3 TTS **不重叠**，是两条互补的路线：
		//   · Qwen3 TTS = 音色克隆 / 高质量模型（要 Python + 2.9GB 权重 + ffmpeg）；
		//   · 本条     = "装上就能出声"的轻量合成（秒级安装、离线、可做网页朗读/提示音）。
		//
		// 界面：面板托管的常驻网页界面（独立端口 8891 直连
		// http://127.0.0.1:8891/，服务由安装器注册成系统级 launchd
		// com.zizdog.macosspeech，见 macspeech_install.go），
		// 同时挂面板别名 /speech/（UI.Slug，由 internal/appproxy 反代）。
		// 两个入口是同一份界面（cmd/zizpanel 的 `speech-serve` 子命令内嵌提供）。
		{
			ID: MacSpeechAppID, Name: "macOS 语音合成（say）", Icon: "🔊",
			UI:      &AppUI{Slug: MacSpeechSlug},
			Summary: "系统自带语音合成：零安装、零下载、离线可用；OpenAI 兼容接口 + 网页界面",
			// Description 上限 80 字（TestCatalogDescriptionsStayShort）：
			// 只留用户决定要不要用的那一句，细节全在注释/文档里。
			Description: "用 macOS 自带的 say 合成语音：零下载、完全离线，带网页界面与 OpenAI 兼容接口。",
			Category:    CategoryAI, Kind: KindNative,
			PanelInstaller: "macspeech",
			ServiceLabel:   MacSpeechLabel,
			Port:           MacSpeechPort,
			HealthPath:     "/healthz",
			// ⚠️ 刻意**不填** Requires / BrewFormula：这个应用没有任何下载点，
			// 也没有 brew 包 —— 引擎是操作系统的一部分。装了它只是"把面板托管的
			// 网页界面注册起来"，所以它也是市场里安装最快的一个（秒级）。
			PostInstallHint: "网页界面：直连 http://127.0.0.1:8891/ ，或从卡片点「打开」走面板别名 /speech/。" +
				"接口：POST /v1/audio/speech（OpenAI 兼容：input / voice / speed / format），" +
				"GET /v1/voices（来自 `say -v '?'`，中文音色带 chinese=true）。" +
				"超过 600 字的文本会自动转入任务并给出真实进度（按句分段合成）。" +
				"音色更多/更好听可在「系统设置 → 辅助功能 → 朗读内容 → 系统声音」里添加。",
			DocsURL: "https://keith.github.io/xcode-man-pages/say.1.html",
		},
		// 语音转文字（whisper.cpp）。用户 2026-09-25 要求"在市场里加入一个语音
		// 转文字服务！要求：不要 docker，不要 gui 软件；有 webui 或 api
		// （你自行开发配套 webui）"。
		//
		// 评估结论：**用 Homebrew 的 whisper.cpp**（本轮实测通过）。
		//   · 原生 arm64 单二进制、Metal 加速（实测 `ggml_metal_device_init:
		//     GPU name: MTL0 (Apple M4)`），不需要 Docker、不需要 GUI、
		//     **不需要 Python venv** —— 最后这条尤其重要：2026-09-18
		//     `python@3.11` 被外力删除让 Qwen3 TTS 挂了两小时，本轮刻意
		//     不再引入任何 Python 运行时；
		//   · 落选者：sherpa-onnx/SenseVoice（websocket 语义、要更多胶水）、
		//     FunASR/SenseVoiceSmall（PyTorch 重依赖）、Speaches（Docker 一把起
		//     → 违反"不要 docker"）、pfrankov/whisper-server（菜单栏 GUI App
		//     → 违反"不要 gui"）、WhisperKit serve（要 Swift 构建链、无 brew
		//     formula）、mlx-whisper + FastAPI（能用但要再造一套 Python venv，
		//     仅作最后退路，本轮**没有启用**）、Vosk（中文精度弱）。
		//
		// ⚠️ BrewFormula 必须写 **`whisper.cpp`**（正名），不是用户习惯的
		// `whisper-cpp`：后者只是 Old Name，而镜像/官方的清单 JSON 只有正名那个
		// （`/api/formula/whisper-cpp.json` → 404）。写别名会让三家国内镜像全部
		// 被判为不可用、静默回落 ghcr.io（实测同一个 brew info 走 USTC 镜像
		// 2.1 秒、走官方默认 8 分 28 秒）。详见 stt.go 里 STTBrewFormula 的注释。
		//
		// 界面：面板托管的常驻网页界面（独立端口 8892 直连
		// http://127.0.0.1:8892/，服务由安装器注册成系统级 launchd
		// com.zizdog.stt，见 stt_install.go），同时挂面板别名 /stt/
		// （UI.Slug，由 internal/appproxy 反代）。
		// 两个入口是同一份界面（cmd/zizpanel 的 `stt-serve` 子命令内嵌提供）。
		// RuntimePath 贴的是**引擎本身**（whisper-cli）：界面只是它的调用者，
		// "引擎在不在"才是这个应用的能力判据。
		{
			ID: STTAppID, Name: "语音转文字（whisper.cpp）", Icon: "🎙️",
			UI:      &AppUI{Slug: STTSlug},
			Summary: "把音频转成文字（中文/英语/日语…）：原生 arm64 加速、离线可用，带网页界面",
			Description: "用 whisper.cpp 把音频转成文字：可拖入文件或用麦克风录音，" +
				"三档模型可选，结果带时间戳、可导出 txt/srt。",
			Category: CategoryAI, Kind: KindNative,
			PanelInstaller: "stt",
			BrewFormula:    STTBrewFormula,
			ServiceLabel:   STTLabel,
			// 引擎本体 = brew 前缀下的 whisper-cli（与安装器复核的是同一个命令名）。
			RuntimePath: "{brew}/bin/" + STTCLIBinName,
			Port:        STTPort,
			HealthPath:  "/healthz",
			// ffmpeg 是转码链路的硬前置（brew 的 whisper.cpp 没链 ffmpeg，
			// 它只吃得下 16 kHz 单声道 WAV）：由基础依赖设施 EnsureBaseDependencies
			// 幂等补齐，缺了会在安装流程第一步就如实报出来。
			Requires: []Requirement{{Type: "brew_formula", Value: "ffmpeg",
				Hint: "语音转文字要用 ffmpeg 把上传的音频转成 16 kHz 单声道 WAV（应用市场里的基础依赖，会自动一并装好）"}},
			PostInstallHint: "网页界面：直连 http://127.0.0.1:8892/ ，或从卡片点「打开」走面板别名 /stt/。" +
				"接口：POST /v1/audio/transcriptions（OpenAI 兼容：file / language / model / response_format）、" +
				"GET /v1/models（装了哪些档、当前档是谁）、GET /healthz（能力探活）。" +
				"模型三档（small / large-v3-turbo / medium）可在界面上按需下载，界面会显示各自体积与内存建议。" +
				"超过 60 秒的音频会自动转入任务并给出真实进度（按 60 秒切片）。",
			DocsURL: "https://github.com/ggml-org/whisper.cpp",
		},
		// ---------------- Python 解释器（基础环境的运行时，用户 2026-09-18 要求） ----------------
		//
		// 为什么把 Python 解释器当"应用"上架：面板自研的两个服务（Qwen3 TTS、
		// IOPaint）各自需要一整套 Python 环境，而面板过去只会**偷偷**在它们自己的
		// 安装流程里 `brew install python@3.11` —— 用户看不见、也无法选择版本，
		// 更没法在装之前先把它准备好。用户 2026-09-18 明确要求：
		//   "应用市场添加至少 3 个常用版本的 python。"
		//
		// 三条都有的意义是**用户可选**：不同应用对 Python 版本的要求不同
		// （mlx-audio 走 3.11 最稳，torch 系在 3.12 上也没问题），面板不再替用户
		// 赌一个版本。Qwen3 TTS / IOPaint 的预置版本仍是 panelPythonFormula
		// （见 python_runtime.go），与这里上架的版本是两件事：这里是"给用户装的"，
		// 那里是"面板自己的两个服务默认用的"。
		//
		// PanelInstaller=python：解释器**没有 brew service**（装了就是命令行工具 +
		// 库），走通用 brew 流程会 `brew services start python@3.11` 并留下一条
		// 永远没有状态的假服务记录、外加一句"已安装但启动失败"的假警告
		// （ffmpeg 当年就是这么被误报的）。所以它有自己的安装/卸载路径。
		// 上游从 3.12 起不再提供 macOS Intel 瓶，只有 3.10 / 3.11 有。
		// 装完是命令行工具（/opt/homebrew/opt/python@3.10/bin/python3.10），没有常驻进程，
		// 也不会自动创建虚拟环境。
		{
			ID: "python310", Name: "Python 3.10", Icon: "🐍",
			Summary:     "Python 解释器（老项目/Intel Mac 需要时的稳妥选择）",
			Description: "Python 3.10 解释器（含 pip）；Intel Mac 与老项目选它，3.12+ 无 Intel 包。",
			Category:    CategoryOther, Kind: KindNative,
			PanelInstaller: "python",
			BrewFormula:    "python@3.10",
			NoDaemon:       true,
			// 安装体 = keg 里的解释器可执行文件（python@3.x 是 keg-only，
			// `<brew>/bin/python3.x` 不一定有软链，`opt/<formula>/bin/` 才是稳的那个）。
			RuntimePath: "{brew}/opt/python@3.10/bin/python3.10",
			Port:        0,
			DocsURL:     "https://docs.python.org/3.10/",
		},
		// 面板自己的两个 Python 服务默认用这一版：mlx-audio 在 3.11 上有预编译 wheel，
		// 整条链路真机验证过。装了它不会自动创建虚拟环境 —— 虚拟环境由各应用按需创建。
		{
			ID: "python311", Name: "Python 3.11", Icon: "🐍",
			Summary:     "Python 解释器（面板自研运行时 Qwen3 TTS / IOPaint 的预置版本）",
			Description: "Python 3.11 解释器（含 pip）；面板的 Qwen3 TTS / IOPaint 默认用这一版。",
			Category:    CategoryOther, Kind: KindNative,
			PanelInstaller: "python",
			BrewFormula:    "python@3.11",
			// 解释器没有守护进程、没有端口、没有网页界面（与 ffmpeg 同类）。
			NoDaemon:    true,
			RuntimePath: "{brew}/opt/python@3.11/bin/python3.11",
			Port:        0,
			DocsURL:     "https://docs.python.org/3.11/",
		},
		// 版本化 formula 各自独立，与 3.11 / 3.13 并存、互不覆盖。
		{
			ID: "python312", Name: "Python 3.12", Icon: "🐍",
			Summary:     "Python 解释器（较新的稳定版本，适合需要 3.12 的应用）",
			Description: "Python 3.12 解释器（含 pip）；⚠️ Intel Mac 装不了（上游无 Intel 包）。",
			Category:    CategoryOther, Kind: KindNative,
			PanelInstaller: "python",
			BrewFormula:    "python@3.12",
			NoDaemon:       true,
			RuntimePath:    "{brew}/opt/python@3.12/bin/python3.12",
			Port:           0,
			DocsURL:        "https://docs.python.org/3.12/",
		},
		// 部分需要预编译 wheel 的 ML 包可能还没适配 3.13；装包报错就换回 3.11 / 3.12。
		{
			ID: "python313", Name: "Python 3.13", Icon: "🐍",
			Summary:     "Python 解释器（最新稳定版，尝鲜/新项目用）",
			Description: "Python 3.13 解释器（含 pip）；⚠️ Intel Mac 装不了，部分 ML 包可能未适配。",
			Category:    CategoryOther, Kind: KindNative,
			PanelInstaller: "python",
			BrewFormula:    "python@3.13",
			NoDaemon:       true,
			RuntimePath:    "{brew}/opt/python@3.13/bin/python3.13",
			Port:           0,
			DocsURL:        "https://docs.python.org/3.13/",
		},
		// PostgreSQL 归「网站环境」（CategoryLNMP）是**产品负责人 2026-09-19 的拆分**：
		//   · 它是数据库组件，与 MySQL 同类；网站环境层 = nginx / PHP / MySQL /
		//     PostgreSQL / phpMyAdmin，统一收在「网站环境」板块；
		//   · 「基础环境」只留 CLT / Homebrew / ffmpeg 这类跨应用运行依赖
		//     （它曾按 2026-09-17 的旧决定放在这里，本次拆分把它移出）。
		//
		// 安装 / 启停 / 卸载仍是独立的（不塞进 Miniflux 的安装事务）：
		//   · brew 的 postgresql@17 是**一个 cluster 一个数据目录**
		//     （/opt/homebrew/var/postgresql@17）。若 PG 由 Miniflux 的安装流程顺带装，
		//     就会出现"Miniflux 装失败该不该卸 PG"的两难 —— 卸掉会伤到别的依赖方，
		//     不卸就留一个孤儿；更糟的是 cluster 的归属会随安装顺序漂移。
		//   · 独立条目则安装 / 启停 / 卸载各自幂等、与应用解耦；卸载 Miniflux
		//     不会碰这个 cluster（数据保留语义见 uninstall 计划）。
		{
			ID: "postgresql17", Name: "PostgreSQL 17", Icon: "🐘",
			Summary:     "关系型数据库（自托管应用用，例如 Miniflux）",
			Description: "PostgreSQL 17，供自托管应用（如 Miniflux）使用；卸载不会删数据目录。",
			Category:    CategoryLNMP, Kind: KindNative, ServiceLabel: "sh.brew.postgresql@17",
			// PostgreSQL 说的是自己的线路协议、不是 HTTP：不能做 HTTP 健康检查，
			// 否则会永远显示"不健康"（与 PHP-FPM 同理）。留空 = 只按进程与端口判断。
			Port: 5432, HealthPath: "",
			BrewFormula: "postgresql@17",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			LogPath:      "/opt/homebrew/var/log/postgresql@17.log",
			DocsURL:      "https://www.postgresql.org",
		},

		// ---------------- AI 服务（原生，用 Metal 加速） ----------------
		{
			ID: "ollama", Name: "Ollama", Icon: "🦙",
			Summary:     "本地大模型推理，支持 Metal 加速",
			Description: "本地大模型推理（Llama / Qwen 等），原生安装才能用 Metal 加速。",
			Category:    "ai", Kind: KindNative,
			Port: 11434, HealthPath: "/api/tags",
			BrewFormula: "ollama", LogPath: "~/.ollama/ollama.log",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon:    true,
			PostInstallHint: "安装后执行 ollama pull qwen2.5:7b 下载一个模型即可开始使用",
			Requires: []Requirement{
				{Type: "brew_formula", Value: "ollama", Hint: "brew install ollama"},
			},
			DocsURL: "https://ollama.com",
		},

		// ---------------- 容器运行时 ----------------
		//
		// 放在所有 Docker 应用之前：它是那些应用的前提。面板不把它当普通应用，
		// 而是同时登记进「服务管理」，让它像 nginx/MySQL 一样可启停、看日志。
		// Colima 在轻量 Linux 虚拟机里跑 Docker 引擎，原生支持 Apple Silicon，
		// 装好后开机自启、无需登录桌面；停止它会让所有容器一起停掉。
		{
			ID: "docker-runtime", Name: "Docker 运行时（Colima）", Icon: "🐳",
			Summary:     "容器引擎，Docker 类应用的前提",
			Description: "Docker 引擎（Colima）：需要 Linux 虚拟机，首次启动较慢；停止它容器全停。",
			Category:    "runtime", Kind: KindColima, PanelInstaller: "docker-runtime",
			// BrewFormula / ServiceLabel 仍然要给（安装器与 launchd 作业要靠它们），
			// 但**"装没装"不看它们、也不看 plist**：容器运行时的运行体是"二进制 + VM +
			// socket"，只剩一份僵尸 plist 也照样是"没装"（2026-09-19 用户实测的假"已安装"，
			// 见 DEVELOPMENT 坑 161）。真实判定在 api_services.go 的 dockerRuntimeStatus。
			BrewFormula: "colima", ServiceLabel: ColimaLaunchLabel,
			DocsURL: "https://github.com/abiosoft/colima",
		},
		// ---------------- 运维工具（Docker） ----------------
		//
		// arm64 证据（2026-09-16 逐条实测；铁律②要求"镜像必须自带 linux/arm64"）：
		//   Hub 上的条目经自建 NAS 镜像站读**同一份 image index**（镜像站是
		//   registry:2 pull-through，拿到就是 Hub 原始 index，不是第三方转存）：
		//     GET <mirror>/docker/v2/<repo>/manifests/<tag>
		//   · louislam/uptime-kuma:1                → linux/amd64 + linux/arm64
		//   · gitea/gitea:latest                    → linux/amd64 + linux/arm64
		//   · stirlingtools/stirling-pdf:latest      → linux/amd64 + linux/arm64
		//   · filebrowser/filebrowser:latest         → linux/amd64 + linux/arm64 + arm/v7
		//   · pjmeca/squoosh:1.1.0                   → linux/amd64 + linux/arm64 + arm/v7
		//   · n8nio/n8n:latest                       → linux/amd64 + linux/arm64
		//   非 Hub 的单独查（加速源不覆盖它们，实测均可直连）：
		//   · quay.io/minio/minio:latest             → linux/amd64 + linux/arm64
		//   · ghcr.io/corentinth/it-tools:latest     → linux/amd64 + linux/arm64
		//   · ghcr.io/metatube-community/metatube-server:latest
		//                                            → linux/amd64 + linux/arm64
		//   ⚠️ 唯一一个**镜像站上游缺件**的：pjmeca/squoosh —— docker.m.daocloud.io
		//      明确拒绝它（DENIED / not in the allowlist），docker.1panel.live 能服务。
		//      所以面板自动配加速源时**必须配多个**、靠 docker 逐条回落，只配一个
		//      daocloud 会让 squoosh 装不上（见 docker_mirror_nas.go 顶部说明）。
		// 镜像用官方滚动 tag louislam/uptime-kuma:2（1.x 已停维护）；v1→v2 会自动迁移
		// 数据库且不可逆，UI.Note 与 PostInstallHint 里都提醒先备份 ./data。
		// 子路径那 5 条改写真机是按 v1 调的，v2 前端路由改版后未重测（见 UI.Note）。
		{
			ID: "uptime-kuma", Name: "Uptime Kuma", Icon: "📡",
			UI: &AppUI{
				Slug:      "uptime-kuma",
				Websocket: true,
				Rewrites: []UIRewrite{
					{From: "/socket.io", To: "/{slug}/socket.io"},
					{From: `"/api/`, To: `"/{slug}/api/`},
					{From: "/icon.svg", To: "/{slug}/icon.svg"},
					{From: "/apple-touch-icon.png", To: "/{slug}/apple-touch-icon.png"},
					{From: "/manifest.json", To: "/{slug}/manifest.json"},
				},
				Note: "Uptime Kuma 官方不支持子路径（改写到页面前端路由后是 Page Not Found，实测于 mini）；" +
					"下面这 5 条改写是**照 v1 调的**，v2 前端路由改版后是否仍成立需要重新实测。" +
					"另：v1 → v2 会**自动迁移数据库**，请先看 Description 里的升级提醒。",
				// 真机实测：/uptime-kuma/ 能返回页面、资源全 200，但正文是
				// "Page Not Found" —— 它的 Vue 路由不认这个前缀，只有官方
				// 那套改 entrypoint 的社区方案才能挂子路径，面板不做那种侵入。
				PreferDirect: true,
			},
			Summary:     "自托管服务监控与告警",
			Description: "网站与服务的可用性监控，支持多种通知渠道；v1→v2 会自动迁移数据库。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 3001,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// host 网络（2026-09-17 mini 实测：Colima/Lima 会把 VM 内监听的端口自动
			// 转发到 Mac 宿主，host 网络下没有 ports: 也能从宿主访问）。Uptime Kuma
			// 是单容器、无交互，容器内默认就监听 3001，与面板保留端口
			// （80 nginx / 8080 IOPaint / 9000 PHP-FPM）都不冲突，所以直接用 host。
			// 换端口：设 UPTIME_KUMA_PORT（见 .env.example），**不要**再加 ports:。
			ComposeYAML: `services:
  uptime-kuma:
    image: louislam/uptime-kuma:2
    container_name: uptime-kuma
    # host 网络：容器内 3001 直接就是 VM 内端口，Colima 自动转发到 Mac 宿主。
    network_mode: host
    environment:
      # 想换端口就改 .env 里的 UPTIME_KUMA_PORT（默认 3001），不要再加 ports:。
      UPTIME_KUMA_PORT: ${UPTIME_KUMA_PORT:-3001}
    volumes:
      # 数据目录由 DATA_ROOT 决定（默认 . 即本 compose 文件所在目录）
      - ${DATA_ROOT:-.}/data:/app/data
    restart: unless-stopped
`,
			DocsURL: "https://github.com/louislam/uptime-kuma",
		},
		{
			ID: "gitea", Name: "Gitea", Icon: "🍵",
			UI: &AppUI{
				Slug:         "gitea",
				Note:         "Gitea 需要在 app.ini 里设 ROOT_URL 带子路径才能用（面板只做改写，不保证可用）",
				PreferDirect: true,
			},
			Summary:     "轻量自建 Git 服务",
			Description: "资源占用极小的 Git 托管（含 Web 界面、Issue、CI 入口）。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 3000,
			HealthPath: "/api/healthz",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: composeTemplate("gitea", "gitea/gitea:latest", 3000, 3000, `
    # 为什么不用 host 网络：Gitea 容器内还监听 22（Git over SSH），
    # host 网络会让它去抢 Colima VM 的 SSH 端口 —— 保留端口映射（宿主 3000）。
    environment:
      - USER_UID=1000
      - USER_GID=1000
    volumes:
      # 数据目录由 DATA_ROOT 决定（默认 . 即本 compose 文件所在目录）
      - ${DATA_ROOT:-.}/data:/data
      - /etc/timezone:/etc/timezone:ro
      - /etc/localtime:/etc/localtime:ro
    restart: unless-stopped`),
			DocsURL: "https://gitea.com",
		},
		{
			ID: "stirling-pdf", Name: "Stirling PDF", Icon: "📄",
			UI: &AppUI{
				Slug: "stirling-pdf",
				Note: "Stirling 需要自己在配置里设 base path，面板只能硬挂；探测不通过时请用端口直连 8082。" +
					"**首次使用要先自己注册账号**：它默认开启登录，初始账号为空，打开 / 会 302 到 /login，" +
					"请点「Sign up」自己开一个号（面板健康检查拿到的 401 是它正常的鉴权响应，不代表故障）。",
				PreferDirect: true,
			},
			Summary:     "本地 PDF 工具箱",
			Description: "合并、拆分、压缩、OCR、转图片等 PDF 操作，全部在本机完成，不上传云端。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 8082,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: composeTemplate("stirling-pdf", "stirlingtools/stirling-pdf:latest", 8082, 8080, `
    # 为什么不用 host 网络：容器内监听 8080，而 8080 是面板给 IOPaint 保留的端口，
    # host 网络会直接抢不到 —— 保留端口映射（宿主 8082）。
    volumes:
      # 数据目录由 DATA_ROOT 决定（默认 . 即本 compose 文件所在目录）
      - ${DATA_ROOT:-.}/data:/configs
    restart: unless-stopped`),
			// 真机现象（2026-09-17 只读排查）：容器在跑但 RestartCount=184，
			// dmesg 实证是 **Colima VM OOM**（VM 只有 1.9GiB）—— Stirling 的
			// 图像/OCR 处理很吃内存，被内核 OOM 杀掉后 compose 不断重启它。
			// 这不是面板的 bug，如实写进提示里，别让用户以为是"装坏了"。
			PostInstallHint: "① 首次打开 http://<本机地址>:8082 会跳到登录页 —— 请点「Sign up」" +
				"自己注册一个管理员账号（默认开启登录且初始账号为空，不注册进不去）。" +
				"② 它做 OCR / 图片转换时很吃内存：Colima 虚拟机内存不足时容器会被内核 OOM 杀掉，" +
				"表现为容器反复重启（docker ps 里 RESTARTS 不断增大）。遇到这种情况请给 Colima " +
				"分配更多内存后重启运行时。",
			DocsURL: "https://github.com/Stirling-Tools/Stirling-PDF",
		},

		// ---------------- 2026-09 新增（选品规则见 README「应用市场」） ----------------
		//
		// 长期规则（用户 2026-09-14 定下）：
		//   ① **能原生就原生**：Homebrew formula 优先（也可用官方 darwin-arm64
		//      预编译产物，前提是目录里已有对应的安装路径）；
		//   ② **只能 Docker 时，镜像必须自带 linux/arm64**：
		//      compose 里绝不写 `platform: linux/amd64`，也不靠 Rosetta 转译 ——
		//      转译既慢又占内存，与"原生优先"的初衷相悖；
		//   ③ 2026-09 之前面板**没有**"下载 GitHub release 二进制 → 写 launchd
		//      plist"的通用安装器，所以"有官方 arm64 二进制但没有 brew formula"
		//      的应用只能先走 Docker（metatube-server 就是这样）。
		//      **2026-09 起这条已经补上**：见 binary_release.go（服务 Lucky 与
		//      Orbien 两个条目，不是给单个应用临时发明的）。现在这类应用的
		//      判断顺序是：brew formula → 官方 darwin-arm64 产物 + 该安装器 →
		//      多架构 Docker（且镜像必须自带 linux/arm64）。
		//
		// 为什么每条 Docker 条目都把 arm64 证据写进注释：这条规则只能靠"查过"
		// 来保证，写下来下次换镜像/换 tag 时才有对照。实测命令：
		//   docker manifest inspect <image>:<tag>        # 看 platforms 列表
		// 本机 registry-1.docker.io / hub.docker.com 直连超时（见 README），
		// Docker Hub 上的镜像改经镜像站读同一份 image index：
		//   docker manifest inspect docker.1ms.run/<image>:<tag>
		// ghcr.io 可直连，官方 ghcr 镜像一律直查。
		{
			ID: "it-tools", Name: "IT-Tools（开发者工具箱）", Icon: "🧰",
			// 纯前端应用：所有逻辑在浏览器里跑，没有后端 API，只有静态资源前缀要改写。
			UI:          &AppUI{Slug: "it-tools"},
			Summary:     "几十个开发者常用小工具，纯前端",
			Description: "开发者小工具合集（JSON、Base64、UUID、哈希等），纯前端执行、输入不上传。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 8083,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// 官方镜像（README 里给的就是 corentinth/it-tools 与 ghcr.io/corentinth/it-tools
			// 两个地址）。这里用 ghcr：与本机网络实测有关 —— ghcr.io 可直连，
			// Docker Hub 直连超时；两者是同一个官方项目，不是第三方转存。
			// arm64 证据（ghcr.io 直查）：
			//   docker manifest inspect ghcr.io/corentinth/it-tools:latest
			//   → linux/amd64、linux/arm64（另有两个 unknown/unknown 的 attestation）
			ComposeYAML: composeTemplate("it-tools", "ghcr.io/corentinth/it-tools:latest", 8083, 80, `
    # 为什么不用 host 网络：镜像里的 nginx 固定监听 80，而 80 被面板自带的 nginx
    # 占着 —— host 网络下它起不来，故保留端口映射（宿主 8083）。
    restart: unless-stopped`),
			DocsURL: "https://github.com/CorentinTh/it-tools",
		},
		{
			ID: "filebrowser", Name: "File Browser（网页文件管理）", Icon: "🗂️",
			UI: &AppUI{
				Slug: "filebrowser",
				// 原生版用 `-b /filebrowser` 启动，应用自己就带这个前缀，
				// 所以面板不再改写路径（否则会 /filebrowser/static/filebrowser/… 双重加前缀）。
				SelfBase: true,
				Note: "File Browser 以 `-b /filebrowser` 启动，自带这个前缀；" +
					"原生版由面板托管（launchd，二进制与数据库都在 ~/filebrowser/），不再需要 Docker。",
			},
			Summary:     "在浏览器里管理这台 Mac 上的文件（原生）",
			Description: "在浏览器里管理这台 Mac 上的文件，支持多用户与权限；官方 darwin-arm64 原生安装。",
			Category:    "tool", Kind: KindNative,
			PanelInstaller: "filebrowser", ServiceLabel: "com.zizdog.filebrowser",
			// 8081 是它一贯的端口（原 Docker 版就是它，目录内唯一）。
			Port: 8081, HealthPath: "/health",
			// 安装体 = 解压出来的可执行文件（官方 release 产物，file(1) 复核 Mach-O arm64）。
			RuntimePath: "~/filebrowser/filebrowser",
			PostInstallHint: "默认用户名 admin，密码是首次启动随机生成的 —— 面板会从启动日志里把" +
				"**用户名与口令**都读出来显示在安装结果里（日志里没读到时会明说，不会编）。没看到就去" +
				"「服务管理 → File Browser → 日志」找 `randomly generated password` 那一行，登录后请立刻改掉。" +
				"文件根目录默认是你的**整个家目录**；想换目录用服务详情里的「主目录」设置（改 -r → 重启 → 回读生效值）。" +
				"⚠️ 谁能登录这个 Web UI，谁就能读写整个家目录（含 .ssh 等）；它只绑 127.0.0.1、只经面板入口访问，" +
				"不要直接开到局域网/公网。⚠️ 上游项目已于 2026-09-01 归档，之后不再发版、不再修安全问题。",
			DocsURL: "https://filebrowser.org",
		},
		{
			// 原 Docker 条目**降级成"参考"**（不删用户可能还在用的路径）：
			// 面板不再代装，但保留预配置 compose 与镜像下载点；ID 加 `-docker` 后缀
			// 以区别于上面这条原生条目。
			ID: "filebrowser-docker", Name: "File Browser（Docker 版参考）", Icon: "🐳",
			Summary:     "File Browser 的 Docker 部署参考（面板不再代装）",
			Description: "给已经用 Docker/Colima 的用户保留的 File Browser 参考 compose；面板只在 docker 页给出文件。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 8097,
			HealthPath: "/health",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			ComposeYAML: `services:
  filebrowser:
    image: filebrowser/filebrowser:latest
    container_name: filebrowser
    # 官方镜像默认以 uid 1000 的 user 运行，而面板创建的 compose 目录与
    # bind mount 属主是 root —— 它的 init.sh 会因写不了 /config/settings.json
    # 直接退出（set -e）。容器内以 root 运行即可，与目录里其它镜像一致。
    user: "0:0"
    # 子路径：镜像的 /init.sh 会把参数透传给 filebrowser，所以这一行就能生效
    # host 网络（2026-09-17 mini 实测：Colima/Lima 会把 VM 内监听的端口自动转发到
    # Mac 宿主，host 网络下没有 ports: 也能从宿主访问）。镜像默认监听 80，会和
    # 面板自带的 nginx 抢端口，所以用 -a/-p 显式改到 8097（容器内端口就是 VM 内端口）。
    # 8097 是刻意避开原生版占用的 8081：两者可以并存做迁移对照，不会抢端口。
    # 不要再加 ports:（host 网络下会被 docker 静默忽略）。
    network_mode: host
    command: ["-b", "/filebrowser", "-a", "0.0.0.0", "-p", "${FILEBROWSER_PORT:-8097}"]
    volumes:
      # 只挂这几个常用目录（界面上就是 /srv 下的 5 个条目）。
      # 想改挂载：Docker → Compose → filebrowser → 编辑 yml → 重新部署。
      # ⚠️ macOS 的「文档/下载/桌面」等目录受隐私保护（TCC）：没给 Colima
      #    （以及面板）授予「完全磁盘访问权限」时，容器里是空的、终端里会报
      #    Operation not permitted。
      # 用 ${HOME} 而不是 ~ ：2026-09-17 实测波浪号在 compose 里的展开随版本变化，
      # 老版本会把它当相对路径，挂载变成 <项目目录>/~/Documents（容器里是空的）。
      - ${HOME}/Documents:/srv/文档
      - ${HOME}/Downloads:/srv/下载
      - ${HOME}/Music:/srv/音乐
      - ${HOME}/Movies:/srv/影片
      - ${HOME}/Desktop:/srv/桌面
      # 配置与索引：DATA_ROOT 决定放哪（默认 . 即本 compose 文件所在目录）
      - ${DATA_ROOT:-.}/config:/config
      - ${DATA_ROOT:-.}/database:/database
    restart: unless-stopped
`,
			// 首次启动会自动生成 admin 密码并打在容器日志里；面板部署完会把它捞出来
			// 直接显示在安装结果里（用户不用再去服务管理翻日志）。
			PostInstallHint: "这是**参考** compose：面板不再代装。自己 docker compose up -d 后，" +
				"默认用户名 admin，密码在容器日志里（`randomly generated password`）。" +
				"想改成原生（不用 Docker）请用上面那条「File Browser（网页文件管理）」；" +
				"原生版占 8081、这份参考占 8097，两者可以并存做迁移对照。",
			DocsURL: "https://filebrowser.org",
		},
		// ---------------- 内网穿透 / 反向代理（全部原生） ----------------
		//
		// 2026-09 新增。四条**没有一条走 Docker**，选路理由与证据逐条写在下面：
		//   · frps / frpc：homebrew-core 有 formula，且 formula 里真的有
		//     `service do` 块（`brew info --json=v2` 的 service 字段非 null，
		//     2026-09 实测）—— 这正是 filebrowser 缺的那一块，有它就能走
		//     通用的 brew 原生路径。
		//   · lucky / orbien：没有 formula，但有官方 darwin-arm64 预编译产物，
		//     由 binary_release.go 那套通用安装器装（见该文件顶部说明）。
		// 客户端要暴露的是**这台 Mac 上**的服务，放进容器后 127.0.0.1 会指向容器自己，
		// 所以走原生（官方 darwin-arm64 tarball 解压到 ~/frpc + 系统级 launchd）。
		{
			ID: "frpc", Name: "frpc（frp 客户端）", Icon: "🧷",
			// frpc 的 admin UI 是它自己监听的 7400（协议上它是主动往外连的客户端，
			// 但 webServer 会开一个本地面板），直连与子路径都指向它。
			UI: &AppUI{
				Slug:         "frpc",
				Note:         "frpc 的 admin UI 在 7400；面板默认给端口直连 http://<地址>:7400",
				PreferDirect: true,
				// 7400 是 frpc 自带的 admin 控制台，不是面板的使用入口 ——
				// 用户明确不要在面板里跳过去（见 AppUI.ConsoleOnly）。
				// 市场与服务管理只给「📝 编辑配置文件」+「🔄 重启服务」。
				ConsoleOnly: true,
			},
			Summary:     "把本机端口映射到 frps（客户端，带 admin UI）",
			Description: "frp 客户端：把本机端口映射到你的 frps；面板不提供 frps，需自填地址与 token。",
			Category:    "tool", Kind: KindNative,
			PanelInstaller: "frpc", ServiceLabel: "com.zizdog.frpc",
			// 7400 是它唯一监听的端口（admin UI），所以健康检查也查它。
			Port: 7400, HealthPath: "/",
			ConfigPath: "frpc.toml",
			PostInstallHint: "① serverAddr/serverPort 要改成你自己 frps 的地址与端口" +
				"（面板不提供 frps，模板里 127.0.0.1:7000 只是占位）。" +
				"② auth.token 必须与你 frps 的完全一致 —— 模板里那个是面板随机生成的占位值。" +
				"③ 在 [[proxies]] 里加要暴露的端口（localIP 写 127.0.0.1）。" +
				"改完点「📝 编辑配置文件」，保存后按提示重启服务生效。",
			DocsURL: "https://github.com/fatedier/frp",
		},
		// 同 frpc：原生下 [[tunnels]] 的 service 直接写 127.0.0.1，容器里会指向容器自己，
		// 所以走原生（官方 darwin-arm64 产物解压到 ~/orbien-client + 系统级 launchd）。
		{
			ID: "orbien-client", Name: "Orbien 客户端（CLI）", Icon: "🛰️",
			// 客户端没有自己的 Web 界面（就是上游的 orbien CLI），所以不给 UI 入口 ——
			// 不渲染一个点开必然打不开的按钮。它的可视化配置入口是服务详情里的
			// 「📝 编辑配置文件」（orbien.toml），要看图表请开服务端的 Dashboard(8020)。
			Summary:     "连上 Orbien 服务端，把本机端口穿透出去（客户端）",
			Description: "Orbien 客户端（CLI），把本机端口穿透到你的服务端；面板不提供服务端。",
			Category:    "tool", Kind: KindNative,
			PanelInstaller: "orbien-client", ServiceLabel: "com.zizdog.orbien-client",
			Port:       0,
			ConfigPath: "orbien.toml",
			PostInstallHint: "① server 要改成你自己的 Orbien 服务端地址" +
				"（面板不提供服务端，模板里 127.0.0.1:9527 只是占位）。" +
				"② 服务端启用了 auth.token 时，客户端也要填同一个值。" +
				"③ 在 [[tunnels]] 里加要暴露的端口：service 写 127.0.0.1:<本地端口>，" +
				"remotePort 写服务端上的端口。" +
				"④ 这个客户端**没有网页界面**（纯 CLI）：改完点「📝 编辑配置文件」保存，" +
				"再重启服务生效；隧道是否连上、流量多大，在服务端的 Dashboard" +
				"（http://<服务端地址>:8020）里看。",
			DocsURL: "https://github.com/orbien-org/orbien",
		},
		// ---------------- 动态域名解析（DDNS，原生） ----------------
		//
		// 2026-09-16 新增 ddns-go。选路理由（真机核对过，不要凭猜改）：
		//   · homebrew-core 有 ddns-go formula，但**没有 service 块** ——
		//     `brew info --json=v2 ddns-go` 的 service 字段为 null，
		//     `brew services start ddns-go` 直接报 "has not implemented #plist,
		//     #service or provided a locatable service file"。
		//     所以走 brew 只会得到一个"装了但没有任何守护进程"的包，
		//     与「有守护进程、要监听 9876」矛盾。
		//   · 官方 release 有 darwin-arm64 产物（ddns-go_6.17.7_darwin_arm64.tar.gz），
		//     且有 checksums.txt 可核对 sha256，于是交给 binary_release.go 那套
		//     通用安装器（与 frpc / Orbien 客户端同一条路，不新造流程）。
		{
			ID: "ddns-go", Name: "DDNS-Go（动态域名解析）", Icon: "🌐",
			// 方案 A：**保留一次「打开」** —— 首次要在它自己的网页界面里添加
			// DNS 服务商与域名（那是应用自带能力，面板无法代填 AccessKey）。
			// 所以**不设** ConsoleOnly（设了就按"自带控制台不算使用入口"把「打开」藏掉）。
			//
			// 2026-09-17：原先标了 PreferDirect（担心子路径不可用），只读排查证实
			// 子路径其实是**好的** —— 未登录 GET / 返回 307，Location 已被面板正确
			// 改写；它自己的前端资源走相对路径。那块 ⚠️ 属于过度保守，已去掉。
			UI: &AppUI{
				Slug: "ddns-go",
				Note: "首次配置（添加 DNS 服务商与域名）要在 ddns-go 自己的网页界面里做。" +
					"子路径（/ddns-go/）与端口直连 http://<地址>:9876 都能用。",
			},
			Summary:     "动态公网 IP 变化时自动更新到 DNS 解析（Cloudflare / 阿里云 / DNSPod …）",
			Description: "公网 IP 变化时自动更新域名解析，支持 Cloudflare / 阿里云 / DNSPod 等。",
			Category:    "tool", Kind: KindNative,
			PanelInstaller: "ddns-go", ServiceLabel: "com.zizdog.ddns-go",
			// 9876 是它网页界面端口，也是唯一监听端口（装机实测：启动后 *:9876 LISTEN）。
			Port: 9876, HealthPath: "/",
			ConfigPath: "ddns-go.yaml",
			// 方案 A 的两步（用户 2026-09-16 定下的规矩：日常不跳网页）：
			PostInstallHint: "① 首次：点「打开」进 http://<本机地址>:9876 ，先设置 ddns-go 的" +
				"用户名口令，再到「DNS服务商」里添加服务商（Cloudflare / 阿里云 / 腾讯云 / " +
				"DNSPod / 华为云 / 百度云 等）与要更新的域名。" +
				"② 之后日常：在「服务管理 → DDNS-Go」里用「📝 编辑配置文件」改 " +
				"~/ddns-go/ddns-go.yaml，保存后点「🔄 重启服务」生效 —— 不必再打开网页。",
			DocsURL: "https://github.com/jeessy2/ddns-go",
		},

		// ---------------- 自托管应用（原生，2026-09-17 新增） ----------------
		//
		// 这三个是用户 2026-09-17 选定的"先上三个轻的"。选路结论（都核对过证据，
		// 不要凭猜改）：
		//   · Miniflux：homebrew-core 有 formula **且有 service 块**（2.3.3），
		//     但它**只支持 PostgreSQL** —— 所以它需要一个数据库，见 postgresql17 条目。
		//   · Syncthing：用户明确要"formula，不是 cask"—— 正确。官方 service 块就是
		//     `syncthing --no-browser --no-restart`（无头设计），cask 才是菜单栏 App。
		//   · Alist：**从未进过 homebrew-core**（formulae.brew.sh/api/formula/alist.json
		//     是 404，homebrew-core 提交历史为空），所以走官方 darwin-arm64 release
		//     产物，由 binary_release.go 那套通用 tarball 安装器托管。
		{
			ID: "miniflux", Name: "Miniflux（RSS 阅读器）", Icon: "📰",
			// Miniflux **支持**子路径（BASE_URL 带上路径即可），但面板这一轮不代写
			// 它的 BASE_URL：装完直接给端口直连。要挂到域名下面，用面板的
			// 「反向代理」指向 127.0.0.1:8087，并把 BASE_URL 改成那个地址。
			UI: &AppUI{
				Slug:         "miniflux",
				PreferDirect: true,
				Note: "Miniflux 默认监听 8087（不是上游默认的 8080 —— 8080 被 IOPaint 占着）。" +
					"「打开」给端口直连 http://<本机地址>:8087；要挂域名/HTTPS 请在面板" +
					"「反向代理」里加规则指向 127.0.0.1:8087，并把配置里的 BASE_URL 改成对应地址。",
			},
			Summary:     "极简自托管 RSS 阅读器（需要 PostgreSQL）",
			Description: "极简 RSS 阅读器（需要 PostgreSQL）；装完建库建账号，初始口令只显示一次。",
			Category:    "tool", Kind: KindNative,
			PanelInstaller: "miniflux",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			BrewFormula:  "miniflux",
			// 8087：上游默认 8080，而 8080 已经被 IOPaint 占用（真机核对）。
			Port: 8087, HealthPath: "/healthz",
			// 配置写在 formula 的 service 块写死的位置（/opt/homebrew/etc/miniflux.conf），
			// 所以这里声明**绝对路径**（ConfigFilePath 已支持绝对路径 + ~/ 前缀）。
			ConfigPath: "/opt/homebrew/etc/miniflux.conf",
			LogPath:    "/opt/homebrew/var/log/miniflux.log",
			// 只提示不自动装 —— 但 miniflux 的安装器自己**会**装它（先确保 PG 就绪），
			// 这条声明的价值是"安装前检查里能看见这层依赖"，以及卸载计划里说清
			// "PG 不会被一起卸掉"。
			Requires: []Requirement{{Type: "brew_formula", Value: "postgresql@17",
				Hint: "brew install postgresql@17（Miniflux 只能用 PostgreSQL；装 Miniflux 时会自动装好并建库）"}},
			PostInstallHint: "用「打开」进 http://<本机地址>:8087 登录（用户名 admin，口令见安装结果）。" +
				"要换域名/HTTPS：在面板「反向代理」里加一条指向 127.0.0.1:8087 的规则，" +
				"再把 /opt/homebrew/etc/miniflux.conf 里的 BASE_URL 改成新地址并重启服务。",
			DocsURL: "https://miniflux.app",
		},
		{
			ID: "syncthing", Name: "Syncthing（文件同步）", Icon: "🔄",
			// 上游默认把 GUI 绑在 127.0.0.1:8384，局域网里根本打不开。
			// 面板的安装器会**在设好随机口令的前提下**把 GUI 监听到 0.0.0.0:8384
			// （两者必须同时做：非回环 + 无口令 = 局域网里任何人都能控制同步）。
			UI: &AppUI{
				Slug:         "syncthing",
				Websocket:    true,
				PreferDirect: true,
				Note: "GUI 在 8384。「打开」给端口直连 http://<本机地址>:8384；" +
					"用户名口令由面板在安装时随机生成（见安装结果），可在 GUI 设置里改。" +
					"Syncthing 的 GUI 不支持挂在子路径下（它用绝对路径 /rest/*），" +
					"所以子路径入口只作备用。",
			},
			Summary:     "去中心化文件同步（不经过云盘）",
			Description: "去中心化文件同步（不经过云盘）；面板安装时会同时设置随机登录口令。",
			Category:    "tool", Kind: KindNative,
			PanelInstaller: "syncthing",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			BrewFormula:  "syncthing",
			Port:         8384, HealthPath: "/rest/noauth/health",
			// 配置文件在 ~/Library/Application Support/Syncthing/config.xml。
			ConfigPath: "~/Library/Application Support/Syncthing/config.xml",
			LogPath:    "/opt/homebrew/var/log/syncthing.log",
			PostInstallHint: "① 在 GUI 里登录后，先用面板给的设备 ID 在两台机器上互相添加设备。" +
				"② 局域网内同步需要 macOS 的「本地网络」权限（系统设置 → 隐私与安全性 → 本地网络）；" +
				"被拒绝了就在那里打开它，或在面板设置里允许免授权访问内网段。",
			DocsURL: "https://syncthing.net",
		},
		// ⚠️ 上游只发布 md5 校验清单（没有 sha256）：安装这一步只能做架构复核 + md5 互证，
		// 面板不假装自己校验过 sha256。
		{
			ID: "alist", Name: "Alist（文件列表）", Icon: "📂",
			UI: &AppUI{
				Slug: "alist",
				// 子路径根因（去混淆 Alist 前端 bundle 得到）：
				// Alist 把 base_path 内联在 HTML 里（`window.ALIST = {..., base_path: '/'}`），
				// 前端拿它拼 API baseURL（bundle 里没有 `"/api/` 这样的字面量，
				// 所以通用的 `/api/` 改写规则一定无效）。这里做两条**针对性**改写：
				// base_path 本身，以及静态资源前缀 `/static/`。
				// ✅ 2026-09-17 真机验证（mini，0.14.1）：改写后页面内联的 base_path 变成
				// `/alist/`，`/alist/api/public/settings` 从 404 变 **200**、
				// `/alist/static/manifest.json` = 200，所以子路径可用，不再标 PreferDirect。
				Rewrites: []UIRewrite{
					{From: "base_path: '/'", To: "base_path: '/{slug}/'"},
					{From: "/static/", To: "/{slug}/static/"},
				},
				Note: "Alist 的 Web 界面在 5244（直链）或面板的 /alist/ 子路径（两条都可用）。" +
					"初始管理员口令由 Alist 在**首次启动时**随机生成并写进它自己的日志，" +
					"面板会把这一条从日志里抓出来放进安装结果（抓不到时会说明）。",
			},
			Summary:     "把网盘、对象存储、本地目录挂成一个网页文件站",
			Description: "把网盘、对象存储、本地目录挂成一个网页文件站，并提供 WebDAV。",
			Category:    "tool", Kind: KindNative,
			PanelInstaller: "alist", ServiceLabel: "com.zizdog.alist",
			Port: 5244, HealthPath: "/",
			ConfigPath: "data/config.json",
			PostInstallHint: "① 用 admin + 安装结果里的初始口令登录，登录后立刻在" +
				"「个人资料」里改口令。② 初始口令只在首次启动的日志里出现一次；" +
				"忘了就 SSH 执行 `~/alist/alist admin set <新口令>`。" +
				"③ 添加存储（网盘/本地目录）在「管理 → 存储」里做。",
			DocsURL: "https://alistgo.com",
		},

		// ---------------- 一键建站（Category: site） ----------------
		{
			ID: "typecho", Name: "Typecho", Icon: "📝",
			Summary:     "轻量博客程序，一键装好并配好伪静态",
			Description: "轻量博客程序（PHP + MySQL），面板一键装好；装完到 /install.php 收尾。",
			Category:    "site", Kind: KindNative, Port: 0,
			SiteApp: &SiteAppSpec{
				DownloadURL: "https://github.com/typecho/typecho/releases/latest/download/typecho.zip",
				MirrorURLs:  []string{"https://cdn.jsdelivr.net/gh/typecho/typecho@master/typecho.zip"},
				Archive:     "zip", StripTopDir: true,
				Rewrite: "typecho", FinishPath: "/install.php", NeedsDB: true,
				Notes: []string{
					"安装向导里的「数据库地址」填 localhost，库名/用户名/密码见安装结果",
					"Typecho 需要写权限：面板已把整个站点目录交给运行用户",
				},
			},
			DocsURL: "https://typecho.org",
		},
		{
			ID: "freshrss", Name: "FreshRSS", Icon: "📰",
			Summary:     "自托管 RSS 阅读器（原生：nginx + PHP + SQLite）",
			Description: "多用户 RSS 阅读器，原生安装并用 SQLite；装完到 /i/ 走完向导。",
			Category:    "site", Kind: KindNative, Port: 0,
			SiteApp: &SiteAppSpec{
				// 上游 releases **一直没有资产**（1.26.3~1.30.0 连续 8 个版本 assets 为空），
				// 只能拿源码归档。固定 1.30.0 而不是 rolling master：只有固定版本才能把
				// sha256/字节数写进镜像清单（滚动地址无法校验，安装器就只能"下完即信"）。
				DownloadURL: "https://gh-proxy.com/https://github.com/FreshRSS/FreshRSS/archive/refs/tags/1.30.0.tar.gz",
				MirrorURLs:  []string{"https://codeload.github.com/FreshRSS/FreshRSS/tar.gz/refs/tags/1.30.0"},
				Archive:     "tar.gz", StripTopDir: true,
				// 入口必须指向 p/：FreshRSS 的全部个人数据在 data/（与 p/ 同级），
				// 指错目录等于把订阅数据放进 web 根。
				Rewrite: "freshrss", FinishPath: "/i/", NeedsDB: false,
				Notes: []string{
					"安装向导里「数据库类型」选 **SQLite**（不需要填 MySQL 的库名/账号）",
					"站点入口已被面板指向 <站点目录>/p —— 不要改回站点根目录",
					"向导里自己设置管理员账号与口令（FreshRSS 没有默认口令）",
				},
			},
			DocsURL: "https://freshrss.org",
		},
		{
			ID: "flarum", Name: "Flarum", Icon: "💬",
			Summary:     "轻量论坛程序，一键装好并配好伪静态",
			Description: "现代轻量论坛（PHP + MySQL）；装完到站点首页走完安装向导，数据库信息照安装结果填。",
			Category:    "site", Kind: KindNative, Port: 0,
			SiteApp: &SiteAppSpec{
				// 官方**预编译安装包**（flarum/installation-packages，vendor/ 已打包）：固定
				// 1.8.19 + php8.2，不追 latest（latest 会让 sha256 过期、且与所选 PHP 版本脱钩）。
				// 官方文档的安装章节就是这个仓库：https://docs.flarum.org/install
				DownloadURL: "https://github.com/flarum/installation-packages/raw/main/packages/v1.x/v1.8.19/flarum-v1.8.19-php8.2.zip",
				MirrorURLs: []string{
					"https://gh-proxy.com/https://github.com/flarum/installation-packages/raw/main/packages/v1.x/v1.8.19/flarum-v1.8.19-php8.2.zip",
					"https://cdn.jsdelivr.net/gh/flarum/installation-packages@main/packages/v1.x/v1.8.19/flarum-v1.8.19-php8.2.zip",
				},
				Archive: "zip",
				// 包内**没有**单一顶层目录（.editorconfig/.nginx.conf/composer.json/public/...
				// 都是平级），剥顶层目录会剥错；docroot 靠伪静态预设的 PublicDir=public
				// 落到 public/（Flarum 要求 web 根是 public，源码与 storage/ 不能暴露）。
				StripTopDir: false,
				Rewrite:     "flarum", FinishPath: "/", NeedsDB: true,
				// php8.2 预编译包的 vendor 带 platform_check，要求 PHP >= 8.2；
				// 换 PHP 版本要用对应的 flarum-vX-phpY.zip（面板只登记了这一份）。
				MinPHP: "8.2",
				// 官方 Server Requirements（docs.flarum.org/install）里的扩展清单。
				PHPExts: []string{"curl", "dom", "fileinfo", "gd", "json", "mbstring",
					"openssl", "pdo_mysql", "tokenizer", "zip", "session"},
				Notes: []string{
					"安装向导在站点首页（打开站点就是安装页）：数据库地址填 localhost，" +
						"库名/用户名/密码照安装结果填（面板已建好库与账号，口令在结果里）",
					"Flarum 需要写权限（config.php、storage/、assets/）：面板已把整个站点目录交给运行用户",
					"伪静态用官方 .nginx.conf 的同一条 try_files 规则；docroot 必须是 public/ 子目录",
					"装完请立刻在 Flarum 后台设置管理员账号；本包固定为 1.8.19 + PHP 8.2",
				},
			},
			DocsURL: "https://docs.flarum.org/install",
		},
		{
			ID: "emlog", Name: "emlog", Icon: "📓",
			Summary:     "经典国产博客程序，一键装好并配好伪静态",
			Description: "emlog Pro（PHP + MySQL）博客系统；装完到 /install.php 走完向导并填安装结果里的数据库信息。",
			Category:    "site", Kind: KindNative, Port: 0,
			SiteApp: &SiteAppSpec{
				// 官方发布包（GitHub releases 固定 tag pro-2.6.31；emlog.net/download 也指到这里）。
				DownloadURL: "https://github.com/emlog/emlog/releases/download/pro-2.6.31/emlog_pro_2.6.31.zip",
				MirrorURLs: []string{
					"https://gh-proxy.com/https://github.com/emlog/emlog/releases/download/pro-2.6.31/emlog_pro_2.6.31.zip",
					"https://gitee.com/snowsun/emlog/releases/download/pro-2.6.31/emlog_pro_2.6.31.zip",
				},
				Archive: "zip",
				// 包内是 admin/index.php/include/... 平级（无顶层目录），不能剥。
				StripTopDir: false,
				Rewrite:     "emlog", FinishPath: "/install.php", NeedsDB: true,
				// 上游 README：PHP 5.6/7/8，**推荐 7.4 及以上**；这里按推荐值卡。
				MinPHP: "7.4",
				// README「环境准备」+ 代码实际用到的扩展：数据库驱动是 mysqli（默认）
				// 或 PDO（USE_MYSQL_PDO），这里按默认的 mysqli 要求。
				PHPExts: []string{"mysqli", "gd", "mbstring", "curl", "json", "zip", "openssl"},
				Notes: []string{
					"安装向导在 /install.php：数据库地址填 localhost，库名/用户名/密码照安装结果填",
					"向导会写 config.php，所以站点目录必须可写（面板已把目录交给运行用户）",
					"后台入口是 /admin/（装完向导会让你设置管理员账号）",
					"伪静态用官方 nginx 规则：文件不存在时 rewrite 到 /index.php",
				},
			},
			DocsURL: "https://www.emlog.net/docs/",
		},
		{
			ID: "kodbox", Name: "可道云（kodbox）", Icon: "☁️",
			Summary:     "私有云网盘 / 在线文件管理，一键装好并配好伪静态",
			Description: "可道云 kodbox（PHP + MySQL）私有云盘；装完到站点首页走完安装向导，数据库信息照安装结果填。",
			Category:    "site", Kind: KindNative, Port: 0,
			SiteApp: &SiteAppSpec{
				// 官方仓库固定 tag 归档（kalcaddle/kodbox 1.69.03）；官方安装包入口
				// https://api.kodcloud.com/?app/version&download=server.link 是"永远最新"，
				// 无法登记稳定 sha256，所以这里用固定 tag 的归档（同样来自官方仓库）。
				DownloadURL: "https://github.com/kalcaddle/kodbox/archive/refs/tags/1.69.03.zip",
				MirrorURLs: []string{
					"https://gh-proxy.com/https://github.com/kalcaddle/kodbox/archive/refs/tags/1.69.03.zip",
				},
				Archive: "zip",
				// 归档里有一层顶层目录 kodbox-1.69.03/，必须剥掉。
				StripTopDir: true,
				Rewrite:     "kodbox", FinishPath: "/", NeedsDB: true,
				// 官方依赖说明：PHP 7.0+ / 8.0+ 推荐（docs.kodcloud.com/setup/environment）。
				MinPHP: "7.0",
				// 官方「依赖的 PHP 扩展」必需清单（同名文档），按 MySQL 连接补 pdo_mysql。
				PHPExts: []string{"curl", "dom", "gd", "json", "libxml", "mbstring",
					"openssl", "session", "xml", "zip", "zlib", "pdo_mysql"},
				Notes: []string{
					"安装向导在站点首页：数据库类型选 MySQL，地址填 localhost，" +
						"库名/用户名/密码照安装结果填（面板已建好库与账号）",
					"可道云需要写权限（config/、data/）：面板已把整个站点目录交给运行用户",
					"管理员账号由你在安装向导里设置（面板不预设）",
					"伪静态用官方 README_zh-CN 的 nginx 规则：try_files $uri $uri/ /index.php?$query_string",
				},
			},
			DocsURL: "https://docs.kodcloud.com/setup/environment/",
		},
		// 要显示容器状态需要额外挂 Docker socket —— 面板刻意没有默认挂上
		// （那等于把 Docker 控制权交给它），需要的话自己往 compose 里加。
		{
			ID: "homepage", Name: "Homepage", Icon: "🏠",
			UI: &AppUI{
				Slug: "homepage",
				// 用户反馈（2026-09-17 真机）：/homepage/ 打开后**无样式** ——
				// Homepage 是 Next.js 构建产物，HTML 里是根绝对路径
				// `/_next/static/…`、`/api/…`，不改写就会打到站点根 404。
				// 下面这几条把它们拉回 /homepage/ 前缀。去掉 PreferDirect：
				// 加前缀后这些路径实测 200（见 mini 验证记录）。
				Rewrites: []UIRewrite{
					{From: "/_next/", To: "/{slug}/_next/"},
					{From: "/api/", To: "/{slug}/api/"},
					{From: "/site.webmanifest", To: "/{slug}/site.webmanifest"},
					{From: "/safari-pinned-tab.svg", To: "/{slug}/safari-pinned-tab.svg"},
					{From: "/apple-touch-icon.png", To: "/{slug}/apple-touch-icon.png"},
					{From: "/favicon-16x16.png", To: "/{slug}/favicon-16x16.png"},
					{From: "/favicon-32x32.png", To: "/{slug}/favicon-32x32.png"},
					{From: "/homepage.ico", To: "/{slug}/homepage.ico"},
				},
			},
			Summary:     "自建导航首页 / 服务仪表盘",
			Description: "把服务、书签与常用链接汇总成一个导航首页；首次打开是空的，需自己加卡片。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 3010,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			ComposeYAML: `services:
  homepage:
    image: ghcr.io/gethomepage/homepage:v2.3.0
    container_name: homepage
    ports:
      - "3010:3000"
    # 为什么不用 host 网络：容器内监听 3000，而 Gitea 的宿主映射也用 3000，
    # 两者不能同时存在 —— 保留端口映射（宿主 3010）。
    environment:
      # 官方要求必须设置，否则启动后直接 host validation failed（页面打不开）。
      # 这里放开是因为面板装的时候还不知道用户会用哪个地址访问；
      # 只在内网用是可接受的取舍 —— 要收紧就在 .env 里写
      # HOMEPAGE_ALLOWED_HOSTS=192.168.1.4:3010,localhost:3010 这种显式清单。
      HOMEPAGE_ALLOWED_HOSTS: "${HOMEPAGE_ALLOWED_HOSTS:-*}"
    volumes:
      # 配置目录由 DATA_ROOT 决定（默认 . 即本 compose 文件所在目录）
      - ${DATA_ROOT:-.}/config:/app/config
    restart: unless-stopped
`,
			PostInstallHint: "首次打开是空首页：在 <应用目录>/config/services.yaml 里加服务卡片" +
				"（「管理 → 编辑配置文件」可直接改，改完点重启）。",
			DocsURL: "https://gethomepage.dev",
		},
		{
			// Trilium（上游 TriliumNext Notes）为什么**只能走 Docker**：
			//   · Homebrew 里只有 cask `trilium-notes`（Electron GUI），无头/服务器机器用不了；
			//   · 官方服务端产物只有 Linux（TriliumNotes-Server-*-linux-arm64.tar.xz），
			//     没有 darwin-arm64 → 原生无头不可行；
			//   · 镜像 triliumnext/trilium 自带 linux/arm64（不写 platform、不转译）。
			// 端口为什么是 8091：容器默认 8080，而 **8080 在本项目里是 IOPaint 的保留端口**
			// （iopaint 条目 Port=8080；mini 真机实测 8080 正被 Python/IOPaint 监听）。
			ID: "trilium", Name: "Trilium Notes", Icon: "🌳",
			// 子路径：上游没有一等公民的子路径支持（discussion #7090 还在讨论，
			// issue #8500「server does not work with custom path」），面板只能硬挂
			// 绝对路径改写 —— 不确定可用就以「直链」为主，并如实说明理由。
			UI: &AppUI{
				Slug:         "trilium",
				PreferDirect: true,
				Note: "Trilium 上游没有官方的子路径支持（TriliumNext/Notes discussion #7090 / issue #8500），" +
					"面板只做绝对路径改写、不保证它的前端路由认子路径 —— 建议用「直链」" +
					"http://<本机地址>:8091（子路径未实测）",
			},
			Summary:     "自托管个人知识库 / 笔记（全文搜索、关系图、脚本）",
			Description: "层级化笔记与知识库（全文搜索、关系图）；首次打开自行创建管理员账号。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 8091,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// 上游 compose（develop 分支 docker-compose.yml）的等价物，三处**故意不同**：
			//   ① 镜像名：上游仓库文件还写 triliumnext/notes，但那是**冻结的旧镜像**
			//      （notes:latest = v0.95.0 / 2025-06-15）；官方文档现在用的是
			//      triliumnext/trilium（latest = v0.105.0 / 2026-08-19），所以用后者；
			//   ② 端口：8080 → 8091（让开 IOPaint）；
			//   ③ 去掉 /etc/timezone、/etc/localtime 两个挂载：宿主是 macOS（没有
			//      /etc/timezone），而 Colima 下 bind 源是在 Linux VM 里解析的，
			//      挂 VM 的 /etc 文件收益很小；要统一时区用 TZ 环境变量。
			ComposeYAML: `services:
  trilium:
    image: triliumnext/trilium:v0.105.0
    container_name: trilium
    ports:
      # 宿主 8091 → 容器 8080：8080 已被 IOPaint 占用（本项目保留端口），
      # 所以不能改用 host 网络 —— 保留端口映射。
      - "8091:8080"
    environment:
      # 容器内数据目录（官方 compose 也显式设它）；挂载点见下
      - TRILIUM_DATA_DIR=/home/node/trilium-data
      # 官方 docker 文档：容器（entrypoint）需要以 root 启动，且**不支持** --user；
      # 要改数据文件属主就用官方推荐的 USER_UID/USER_GID。
      # macOS 首个管理员用户是 501:20；面板用户不是 501 时改成 id -u / id -g 的值。
      # 已知上游 issue TriliumNext/Notes#331：某些版本根本不读这两个变量；本机在
      # notes:0.95.0 与 trilium:0.105.0 上实测**生效**（容器内 node 进程 uid=501、
      # 宿主侧数据文件属主 501:20），所以保留；万一不生效也只是容器以 root 跑。
      - USER_UID=501
      - USER_GID=20
    volumes:
      # 数据目录由 DATA_ROOT 决定（默认 . 即本 compose 文件所在目录）；
      # 想统一放到别处就设 DATA_ROOT=${HOME}/docker/trilium（见 .env.example；
      # 别用 ~，它在不同 compose 版本里展开行为不一致）。
      # document.db + config.ini + log/ 都在这个目录里。
      - ${DATA_ROOT:-.}/data:/home/node/trilium-data
    restart: unless-stopped
`,
			PostInstallHint: "首次打开 http://<本机地址>:8091/ 会进入初始化向导：创建管理员账号与口令" +
				"（服务端没有默认口令，不设就进不去）。**不要直接开 /setup** —— 本镜像（v0.105.0）里" +
				"该服务端路由会 500（assets/views 没打进镜像），根路径的页面会自己引导初始化。",
			DocsURL: "https://docs.triliumnotes.org/user-guide/setup/server/installation/docker",
		},
		{
			// Activepieces 为什么走 Docker：没有 homebrew formula，官方也没有
			// darwin-arm64 服务端产物（发布渠道只有 npm 包与容器镜像），
			// 镜像自带 linux/arm64（证据见下）。
			ID: "activepieces", Name: "Activepieces", Icon: "🪄",
			UI: &AppUI{
				Slug: "activepieces",
				// 它没有子路径支持：前端按根路径构建，AP_FRONTEND_URL 只影响
				// webhook/回调地址，不能把界面挂到 /activepieces/ —— 如实标直连。
				PreferDirect: true,
				Note: "Activepieces 官方不支持子路径（AP_FRONTEND_URL 只决定 webhook/回调地址，" +
					"不能把界面挂到 /activepieces/）；请用「直链」http://<本机地址>:8090。",
			},
			Summary:     "可视化自动化工作流（开源 Zapier 替代）",
			Description: "可视化自动化工作流（开源 Zapier 替代）；密钥与数据库口令安装时随机生成。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 8090,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// arm64 证据（2026-09-17 本机 `docker manifest inspect` 直查 ghcr.io）：
			//   ghcr.io/activepieces/activepieces:0.91.0 → linux/amd64、linux/arm64
			//   （另有两个 unknown/unknown 的 attestation）
			//   pgvector/pgvector:0.8.0-pg14 → Docker Hub 多架构（含 linux/arm64）
			//   redis:7.0.7 → Docker Hub 多架构（含 linux/arm64/v8）；
			//   compose 里写成规范形式 library/redis:7.0.7（等价 redis，静态门禁要求带仓库前缀）
			//
			// 为什么用 WORKER_AND_APP：镜像的 docker-entrypoint.sh 里这个值就是默认值，
			// 同一个容器同时跑 API 与一个 worker —— 单机自用足够；官方 compose 起
			// 5 个 worker 副本是给生产横向扩展用的，在迷你机上没有收益。
			//
			// AP_ENCRYPTION_KEY 为什么是 **16 字节（32 个 hex 字符）**而不是 32 字节：
			//   源码 packages/server/api/src/app/helper/encryption.ts 用
			//   `Buffer.from(secret, 'binary')` 取密钥、算法是 `aes-256-cbc`。
			//   'binary'（latin1）一个字符 = 一字节，所以密钥**必须恰好 32 个字符**；
			//   给 64 个 hex 字符会变成 64 字节 → createCipheriv 抛 Invalid key length。
			//   官方文档也写 "32-character (16 bytes) hexadecimal key / openssl rand -hex 16"。
			//   AP_JWT_SECRET 官方用 `openssl rand -hex 32`（64 字符），照办。
			//
			// AP_FRONTEND_URL 为什么带默认值：面板在安装时**拿不到**用户会用哪个地址
			//   访问（可能是 LAN IP，也可能是隧道域名），只能给一个安全默认值
			//   http://127.0.0.1:8090；要对外用 webhook/触发器时，请在应用目录的 .env
			//   里把它改成真实可达地址再重启容器（见 PostInstallHint）。
			ComposeYAML: `services:
  activepieces:
    image: ghcr.io/activepieces/activepieces:0.91.0
    container_name: activepieces
    ports:
      # 宿主 8090 → 容器 80：8080 是本项目 IOPaint 的保留端口。
      # 这里**不能**用 host 网络：三个容器之间靠 compose 网络的服务名互相寻址
      # （AP_POSTGRES_HOST=activepieces-postgres 等），host 网络下没有这套 DNS。
      - "8090:80"
    environment:
      AP_CONTAINER_TYPE: WORKER_AND_APP
      # 面板不知道用户用哪个地址访问，默认只适用于本机；局域网/隧道访问请改 .env
      AP_FRONTEND_URL: ${AP_FRONTEND_URL:-http://127.0.0.1:8090}
      AP_ENCRYPTION_KEY: ${AP_ENCRYPTION_KEY}
      AP_JWT_SECRET: ${AP_JWT_SECRET}
      AP_POSTGRES_HOST: activepieces-postgres
      AP_POSTGRES_PORT: "5432"
      AP_POSTGRES_DATABASE: activepieces
      AP_POSTGRES_USERNAME: postgres
      AP_POSTGRES_PASSWORD: ${AP_POSTGRES_PASSWORD}
      AP_REDIS_HOST: activepieces-redis
      AP_REDIS_PORT: "6379"
    volumes:
      - ${DATA_ROOT:-.}/data:/usr/src/app/cache
    depends_on:
      - activepieces-postgres
      - activepieces-redis
    restart: unless-stopped
  activepieces-postgres:
    image: pgvector/pgvector:0.8.0-pg14
    container_name: activepieces-postgres
    # 不发布宿主端口：库只在 compose 网络内被 app 访问
    environment:
      POSTGRES_DB: activepieces
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: ${AP_POSTGRES_PASSWORD}
    volumes:
      - ${DATA_ROOT:-.}/postgres:/var/lib/postgresql/data
    restart: unless-stopped
  activepieces-redis:
    image: library/redis:7.0.7
    container_name: activepieces-redis
    # 不发布宿主端口：只在 compose 网络内被 app 访问
    volumes:
      - ${DATA_ROOT:-.}/redis:/data
    restart: unless-stopped
`,
			ComposeSecrets: []ComposeSecret{
				{Env: "AP_ENCRYPTION_KEY", Encoding: SecretHex, Bytes: 16,
					Label: "Activepieces 连接加密密钥（加密已保存的第三方凭据；必须恰好 32 个 hex 字符）"},
				{Env: "AP_JWT_SECRET", Encoding: SecretHex, Bytes: 32,
					Label: "Activepieces JWT 签名密钥"},
				{Env: "AP_POSTGRES_PASSWORD", Encoding: SecretHex, Bytes: 32,
					Label: "Activepieces 数据库口令（用户 postgres / 库 activepieces）"},
			},
			PostInstallHint: "① 首次打开 http://<本机地址>:8090 注册第一个账号，它就是平台管理员" +
				"（Activepieces 不预设默认口令）。" +
				"② 安装结果里的凭据区块有数据库口令与两个密钥，请自行留存；重装不会重新生成。" +
				"③ 要用**外部 webhook/触发器**时，把 <应用目录>/.env 里的 AP_FRONTEND_URL 改成" +
				"外部可达的地址（如 http://192.168.1.4:8090 或你的隧道域名），再重启容器 —— " +
				"否则生成的回调地址会指向 127.0.0.1，外部触发打不进来。",
			DocsURL: "https://www.activepieces.com/docs/install/options/docker-compose",
		},
		// DB_PASSWORD 重装会复用 .env 旧值、绝不重新生成 —— 数据库初始化后换口令会直接连不上。
		{
			// Immich 为什么走 Docker：官方只发布容器镜像；没有 brew formula、
			// 也没有 darwin-arm64 服务端产物。四个镜像都自带 linux/arm64（证据见下）。
			ID: "immich", Name: "Immich", Icon: "📷",
			UI: &AppUI{
				Slug: "immich",
				// 官方不支持子路径（前端按根路径构建）；如实标直连。
				PreferDirect: true,
				Note: "Immich 官方不支持子路径（前端按根路径构建）；请用「直链」" +
					"http://<本机地址>:2283，手机 App 里也填这个地址。" +
					"⚠️ 官方明确非 Linux 宿主 strongly discouraged（macOS 只能靠 Colima 虚拟机跑）；" +
					"macOS **没有硬件转码**（无 QSV/VAAPI/NVENC 直通），视频只能 CPU 软转、很吃 CPU；" +
					"首次启动要下载人脸/CLIP 模型到 model-cache/，期间搜索不可用。" +
					"DB_PASSWORD 重装会复用 .env 旧值、绝不重新生成 —— 数据库初始化后换口令会直接连不上。",
			},
			Summary:     "自托管照片 / 视频备份（Google Photos 替代）",
			Description: "自托管照片/视频备份；macOS 无硬件转码只能软转，首次启动要下模型。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 2283,
			// 官方健康端点：GET /api/server/ping → 200 {"res":"pong"}
			HealthPath: "/api/server/ping",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// arm64 证据（2026-09-17 本机 `docker manifest inspect` 直查）：
			//   ghcr.io/immich-app/immich-server:release            → linux/amd64、linux/arm64
			//   ghcr.io/immich-app/immich-machine-learning:release  → linux/amd64、linux/arm64
			//   ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0 → linux/amd64、linux/arm64
			//   valkey/valkey:9 （经自建 NAS 镜像站读同一份 index）  → linux/amd64、linux/arm64
			//   （ghcr 三个各另有 unknown/unknown 的 attestation）
			//
			// 与官方 compose 的三处**故意差异**（都是本项目约定，别改回去）：
			//   ① 容器名加 immich- 前缀：官方用 postgres / redis 这种通用名，
			//      在同一台机器上和别的 compose 项目会撞名；
			//   ② 用明确的 environment 而不是 env_file：模板里只需要 DB_PASSWORD
			//      一个密钥，其余写死即可；密钥由用户在 .env 里填（见 .env.example），
			//      少一个"用户忘了配 .env 就起不来"的坑；
			//   ③ 去掉 /etc/localtime 挂载：宿主是 macOS，Colima 下 bind 源在 Linux VM
			//      里解析，挂 VM 的 /etc 文件收益很小（与 trilium 同一处理）；
			//   ④ 数据目录统一走 ${DATA_ROOT:-.}：默认就在 compose 文件旁边，
			//      想集中放就把 DATA_ROOT 指到 ${HOME}/docker/immich（见 .env.example）。
			ComposeYAML: `services:
  immich-server:
    image: ghcr.io/immich-app/immich-server:release
    container_name: immich-server
    ports:
      # 宿主 2283 → 容器 2283。**不能**用 host 网络：四个容器靠 compose 网络的
      # 服务名互访（DB_HOSTNAME=immich-postgres / REDIS_HOSTNAME=immich-redis），
      # host 网络下没有这套 DNS。
      - "2283:2283"
    environment:
      DB_HOSTNAME: immich-postgres
      DB_USERNAME: postgres
      DB_PASSWORD: ${DB_PASSWORD}
      DB_DATABASE_NAME: immich
      REDIS_HOSTNAME: immich-redis
    volumes:
      # 上传目录（官方 UPLOAD_LOCATION）
      - ${DATA_ROOT:-.}/data:/data
    depends_on:
      - immich-redis
      - immich-postgres
    restart: always
  immich-machine-learning:
    image: ghcr.io/immich-app/immich-machine-learning:release
    container_name: immich-machine-learning
    volumes:
      # 机器学习模型缓存（官方 model-cache）
      - ${DATA_ROOT:-.}/model-cache:/cache
    restart: always
  immich-redis:
    image: valkey/valkey:9
    container_name: immich-redis
    restart: always
  immich-postgres:
    image: ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0
    container_name: immich-postgres
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
      POSTGRES_USER: postgres
      POSTGRES_DB: immich
      POSTGRES_INITDB_ARGS: "--data-checksums"
    volumes:
      # 数据库数据目录（官方 DB_DATA_LOCATION）
      - ${DATA_ROOT:-.}/postgres:/var/lib/postgresql/data
    shm_size: 128mb
    restart: always
`,
			ComposeSecrets: []ComposeSecret{
				{Env: "DB_PASSWORD", Encoding: SecretHex, Bytes: 32,
					Label: "Immich 数据库口令（用户 postgres / 库 immich）"},
			},
			PostInstallHint: "① 首次打开 http://<本机地址>:2283 注册第一个管理员账号（官方不设默认口令）。" +
				"② 手机 App 里把服务器地址填成同一个 URL 即可开始备份。" +
				"③ 首次启动 immich-machine-learning 会下载人脸/CLIP 模型到 model-cache/，" +
				"期间搜索/人脸识别不可用，别以为是装坏了。" +
				"④ macOS **没有硬件转码**：视频转码走 CPU、很慢且吃满 CPU；" +
				"对转码性能有要求的话建议把 Immich 放在 Linux 机器上。" +
				"⑤ **不要重新生成 DB_PASSWORD**：数据库初始化后再换口令会直接连不上。" +
				"重装/升级会复用 .env 里的旧口令，面板不会替你改它。",
			DocsURL: "https://docs.immich.app/install/docker-compose",
		},
		{
			ID: "wordpress", Name: "WordPress", Icon: "🌐",
			Summary:     "最流行的建站程序，一键装好并配好伪静态",
			Description: "最流行的建站程序（PHP + MySQL）；装完到 /wp-admin/install.php 填站点信息。",
			Category:    "site", Kind: KindNative, Port: 0,
			SiteApp: &SiteAppSpec{
				// 用官方中文站（国内可达性明显好于 wordpress.org）
				DownloadURL: "https://cn.wordpress.org/latest-zh_CN.zip",
				MirrorURLs:  []string{"https://wordpress.org/latest.zip"},
				Archive:     "zip", StripTopDir: true,
				Rewrite: "wordpress", FinishPath: "/wp-admin/install.php", NeedsDB: true,
				Notes: []string{
					"wp-config.php 已按安装结果里的库名/账号写好，向导里直接点「开始」即可",
					"管理员账号密码由你在向导里设置（面板不预设，避免弱口令）",
				},
			},
			DocsURL: "https://cn.wordpress.org",
		},
		{
			ID: "metatube-server", Name: "MetaTube（媒体元数据服务）", Icon: "🎬",
			Summary:     "给 Emby / Jellyfin 刮削影片元数据",
			Description: "给 Emby / Jellyfin 刮削影片元数据的服务；默认不鉴权，建议只在内网用。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 8084,
			// 项目自带 Web 首页（GET / 返回 app 与 version 的 JSON，见 route/route.go），
			// 拿它当探活路径既真实又不受后续 API 变更影响。
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// 上游 release（metatube-community/metatube-server-releases v1.4.0）
			// 确实提供 metatube-server-darwin-arm64.zip，但**本轮没有**把它切到原生：
			// 通用的 release 二进制安装器（binary_release.go）目前只处理 .tar.gz
			// （Lucky / Orbien 的产物都是 tar.gz），要收 .zip 得先扩它 —— 那是另一件事，
			// 不属于"新增目录条目"。在切换之前这里用**官方** ghcr 镜像（不是第三方转存）。
			// arm64 证据（ghcr.io 直查）：
			//   docker manifest inspect ghcr.io/metatube-community/metatube-server:latest
			//   → linux/amd64、linux/arm64（另有两个 unknown/unknown 的 attestation）
			// 官方镜像默认 DSN 为空 = 内存 SQLite（只存影评缓存，重启即失，与上游
			// Dockerfile 的默认值一致），因此不挂卷；要持久化自行加 DSN 与卷。
			ComposeYAML: composeTemplate("metatube-server",
				"ghcr.io/metatube-community/metatube-server:latest", 8084, 8080, `
    # 为什么不用 host 网络：容器内监听 8080，与 IOPaint 的保留端口冲突 ——
    # 保留端口映射（宿主 8084）。
    restart: unless-stopped`),
			PostInstallHint: "默认不鉴权（TOKEN 为空）：只建议在局域网内使用。" +
				"要让局域网外访问，请在 compose 里加 TOKEN 环境变量，并让客户端带上同一个值。",
			DocsURL: "https://github.com/metatube-community/metatube-sdk-go",
		},
		{
			ID: "squoosh", Name: "Squoosh（图片压缩）", Icon: "🗜️",
			UI: &AppUI{
				Slug: "squoosh",
				// 真机实测（mini）：它的构建产物把**所有**资源放在 `/c/` 下
				// （`self.nextDefineUri=location.origin+"/c/initial-app-….js"`），
				// 子路径下不改写就会 "Failed to load app"。
				Rewrites: []UIRewrite{
					{From: "/c/", To: "/{slug}/c/"},
					{From: "/manifest.json", To: "/{slug}/manifest.json"},
				},
				// 编解码器用 SharedArrayBuffer → 浏览器要求 crossOriginIsolated，
				// 也就是必须带上 COOP/COEP；少了这两个头只会看到 "Failed to load app"。
				Headers: map[string]string{
					"Cross-Origin-Opener-Policy":   "same-origin",
					"Cross-Origin-Embedder-Policy": "require-corp",
				},
			},
			Summary:     "浏览器里的图片压缩，本地 wasm 完成",
			Description: "浏览器里的图片压缩（PNG / JPEG / WebP / AVIF），本机 wasm 完成、不上传。",
			Category:    "tool", Kind: KindCompose, DockerReference: true, Port: 8085,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// 唯一一个没有官方镜像的条目，如实标注并固定版本号：
			//   · 上游 GoogleChromeLabs/squoosh 仓库里没有任何 Dockerfile / 镜像发布
			//     流程（`dev` 分支只有 codecs/*.Dockerfile，那是编译 wasm 编解码器用的）；
			//   · 社区镜像里 pjmeca/squoosh 最新（2024-07 构建）、用 nginx 托管静态构建
			//     （history 里可见 COPY app/build → /usr/share/nginx/html、rm sw.js、
			//     CMD nginx），比 2022 年那批 npm run serve 的镜像干净；
			//   · 固定 1.1.0 而不是 latest：这类无人维护的镜像不该跟着 tag 漂。
			// arm64 证据（Docker Hub 经镜像站读同一份 index；两个 tag 的 arm64
			// digest 相同）：
			//   docker manifest inspect docker.1ms.run/pjmeca/squoosh:1.1.0
			//   → linux/amd64、linux/arm64、linux/arm/v7
			ComposeYAML: composeTemplate("squoosh", "pjmeca/squoosh:1.1.0", 8085, 80, `
    # 为什么不用 host 网络：镜像里的 nginx 固定监听 80，与面板自带 nginx 冲突 ——
    # 保留端口映射（宿主 8085）。
    restart: unless-stopped`),
			DocsURL: "https://github.com/GoogleChromeLabs/squoosh",
		},
	}
}

// composeTemplate 生成一个最小可用的 compose 文件。
//
// 统一用 `container_name` 固定容器名：面板按名字管理容器，
// 不固定的话 compose 会加项目前缀，重启后名字变化导致管理失效。
func composeTemplate(container, image string, hostPort, containerPort int, extra string) string {
	return fmt.Sprintf(`services:
  %s:
    image: %s
    container_name: %s
    ports:
      - "%d:%d"%s
`, container, image, container, hostPort, containerPort, extra)
}

// FindApp 按 ID 查目录。
func FindApp(id string) (App, bool) {
	for _, a := range Catalog() {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// checkPort 探测端口占用。
//
// 抽成方法是为了**可测试**：默认实现会执行真实的 lsof，而单测不该依赖
// "测试机上恰好有东西在听某个端口"（结论会随机器变）。测试通过
// m.portCheckOverride 注入假实现（与 mirrorProbeOverride 同一个理由）。
func (m *Manager) checkPort(port int) (priv.PortInfo, error) {
	if m.portCheckOverride != nil {
		inUse, holders, err := m.portCheckOverride(port)
		return priv.PortInfo{Port: port, InUse: inUse, Holders: holders}, err
	}
	return priv.CheckPort(strconv.Itoa(port))
}

// Preflight 检查一个应用能否安装。
//
// 为什么必须做预检查：用户点了"安装"再等几分钟才发现缺 Docker / 缺 brew，
// 体验很差。这里把所有前置条件一次列清楚，并给出可直接复制执行的修复命令。
func (m *Manager) Preflight(ctx context.Context, app App) Preflight {
	pf := Preflight{App: app.ID, Checks: []CheckResult{}}

	// 纳管类应用不做端口占用检查：服务本来就在运行，
	// 端口被它自己占用是预期状态，报"冲突"会误导用户。
	if app.AdoptLabel != "" {
		pf.PortFree = true
		pf.PortNote = "已有服务，无需检查端口占用"
		for _, req := range app.Requires {
			pf.Checks = append(pf.Checks, m.checkRequirement(ctx, req))
		}
		pf.Ready = true
		for _, c := range pf.Checks {
			if !c.OK {
				pf.Ready = false
			}
		}
		return pf
	}

	// 已经有面板记录 → 这不是"待安装"，而是"已经装过了"。
	// 界面据此把它显示成"已安装（跳过）"，与安装任务的终态保持一致。
	if m.installedRecordFor(ctx, app) != nil {
		pf.AlreadyInstalled = true
	}

	// 端口占用检查
	if app.Port > 0 {
		if info, err := m.checkPort(app.Port); err == nil {
			if info.InUse {
				// 占用者是不是**这个应用自己**的服务？
				//
				// 这是 2026-09-16 真机上 4 个"重复安装报 failed"的直接成因：
				// nginx(80) / mysql84(3306) / ollama(11434) / uptime-kuma(3001)
				// 本来就在跑、端口被它自己占着，旧代码却一律判成冲突。
				// 自己占自己的端口不是冲突，是"已安装"。
				//
				// 真冲突（占用者是别的服务/进程）仍然失败，并点名占用者 ——
				// 见下面的两个分支，行为与过去一致。
				if self := m.selfPortOccupiers(ctx, app); len(self) > 0 {
					pf.PortFree = true
					pf.AlreadyInstalled = true
					pf.PortNote = fmt.Sprintf("端口 %d 已被本应用自己的服务占用（%s）——已经装过了，不会重复安装",
						app.Port, strings.Join(self, ", "))
				} else {
					names, _ := m.repo.CountByPort(ctx, app.Port, "")
					if len(names) > 0 {
						pf.PortFree = false
						pf.PortNote = fmt.Sprintf("端口 %d 已被面板管理的服务占用：%s", app.Port, strings.Join(names, ", "))
					} else {
						pf.PortFree = false
						pf.PortNote = fmt.Sprintf("端口 %d 已被其它进程占用：%s",
							app.Port, strings.Join(info.Holders, ", "))
					}
				}
			} else {
				pf.PortFree = true
				pf.PortNote = fmt.Sprintf("端口 %d 空闲", app.Port)
			}
		}
	} else {
		pf.PortFree = true
	}

	// 依赖检查
	for _, req := range app.Requires {
		pf.Checks = append(pf.Checks, m.checkRequirement(ctx, req))
	}

	pf.Ready = pf.PortFree
	for _, c := range pf.Checks {
		if !c.OK && !c.Fixable {
			pf.Ready = false
		}
	}
	// 真端口冲突（占用者是别的服务/进程）不可安装，错误里会点名占用者。
	// "被自己占用"在上面已经判成 PortFree=true + AlreadyInstalled=true，不会到这里。
	if !pf.PortFree {
		pf.Ready = false
	}
	return pf
}

func (m *Manager) checkRequirement(ctx context.Context, req Requirement) CheckResult {
	switch req.Type {
	case "docker":
		if m.opt.DockerSocket != "" {
			return CheckResult{Name: "Docker", OK: true, Detail: "已检测到 Docker socket: " + m.opt.DockerSocket}
		}
		return CheckResult{
			Name: "Docker", OK: false, Detail: "未检测到 Docker",
			Fixable: false,
			FixCmd:  "brew install colima docker docker-compose",
		}

	case "brew_formula":
		if m.brewHas(ctx, req.Value) {
			return CheckResult{Name: "Homebrew 包 " + req.Value, OK: true, Detail: "已安装"}
		}
		if _, err := os.Stat(m.opt.BrewBin); err != nil {
			return CheckResult{
				Name: "Homebrew", OK: false, Detail: "未安装 Homebrew",
				Fixable: false,
				FixCmd:  `brew install ` + req.Value,
			}
		}
		return CheckResult{
			Name: "Homebrew 包 " + req.Value, OK: false,
			Detail:  "未安装（安装时会自动执行 brew install " + req.Value + "）",
			Fixable: true,
			FixCmd:  "brew install " + req.Value,
		}

	case "launchd":
		plist := filepath.Join("/Library/LaunchDaemons", req.Value+".plist")
		if _, err := os.Stat(plist); err == nil {
			return CheckResult{Name: "服务 " + req.Value, OK: true, Detail: "已存在 " + plist}
		}
		if m.opt.UserHome != "" {
			agent := filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", req.Value+".plist")
			if _, err := os.Stat(agent); err == nil {
				return CheckResult{Name: "服务 " + req.Value, OK: true, Detail: "已存在 " + agent}
			}
		}
		return CheckResult{Name: "服务 " + req.Value, OK: false, Detail: "未找到该 launchd 服务"}

	case "command":
		if p, err := exec.LookPath(req.Value); err == nil {
			return CheckResult{Name: "命令 " + req.Value, OK: true, Detail: p}
		}
		return CheckResult{Name: "命令 " + req.Value, OK: false, Detail: "未找到", Fixable: true, FixCmd: req.Hint}

	default:
		return CheckResult{Name: req.Type, OK: true, Detail: "无需检查"}
	}
}

// InstalledFormulas 一次性返回已安装的 formula 集合。
//
// 为什么要"一次性"：`brew list --versions <单个>` 和
// `brew list --versions`（全部）耗时几乎一样（实测 0.5s vs 0.58s）——
// 因为开销主要在 brew 自身启动，不在查询。所以逐个查 N 次
// 等于白付 N 次启动成本；市场列表有十几个条目，累加就是几秒。
//
// **唯一实现**是 InstalledFormulaVersions（install.go）：这里只是它的 bool 投影，
// 不再自己拼一遍 `brew list --versions`（那种"同一个动作两条代码路径"必然漂移）。
// 降权规则（Homebrew 拒绝 root）也在那里。
//
// ⚠️ 探测失败返回的是**空集合**，与"真的什么都没装"长得一样。需要区分这两件事的
// 调用方（市场列表）必须用 InstalledFormulaVersions 的第二个返回值，不能读这里。
func (m *Manager) InstalledFormulas(ctx context.Context) map[string]bool {
	vers, _ := m.InstalledFormulaVersions(ctx)
	set := make(map[string]bool, len(vers))
	for f := range vers {
		set[f] = true
	}
	return set
}

// HasBrewFormula 报告某个 formula 是否已由 Homebrew 安装。
//
// 导出它是因为 web 层要判断"应用市场里这个应用是否已经装过" ——
// 只看面板自己的服务记录是不够的（用户可能早就用 brew 装好了），
// 真机上就出现过"装好的 nginx 在市场里还显示安装按钮"。
func (m *Manager) HasBrewFormula(ctx context.Context, formula string) bool {
	return m.brewHas(ctx, formula)
}

// brewHas 判断某个 Homebrew 包是否已安装（单个）。
//
// Homebrew 拒绝以 root 运行，而面板以 root 运行，所以必须降权到真实用户执行。
// 批量场景不要循环调它：见 InstalledFormulas —— 逐个查 N 次等于白付 N 次 brew 启动。
func (m *Manager) brewHas(ctx context.Context, formula string) bool {
	if formula == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "-u", m.opt.UserName,
			m.opt.BrewBin, "list", "--versions", formula)
	} else {
		cmd = exec.CommandContext(ctx, m.opt.BrewBin, "list", "--versions", formula)
	}
	return cmd.Run() == nil
}

// ScanAdoptable 扫描本机上"可以纳管"但还没纳入的服务。
//
// 扫描范围：/Library/LaunchDaemons 与用户的 LaunchAgents，
// 排除系统自带（com.apple.*）与已被纳管的。
func (m *Manager) ScanAdoptable(ctx context.Context) ([]AdoptCandidate, error) {
	known := map[string]bool{}
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		if s.LaunchLabel != "" {
			known[s.LaunchLabel] = true
		}
	}

	// 按 brew formula 也去重一次：记录里的 label 前缀可能是旧的
	// （真机上记录是 homebrew.mxcl.ollama，磁盘上是 sh.brew.ollama），
	// 只比 label 会让同一个服务在"可纳管"里再出现一遍。
	knownFormula := map[string]bool{}
	for label := range known {
		if f, ok := brewFormulaFromLabel(label); ok {
			knownFormula[f] = true
		}
	}

	candidates := []AdoptCandidate{}
	dirs := []string{"/Library/LaunchDaemons"}
	if m.opt.UserHome != "" {
		dirs = append(dirs, filepath.Join(m.opt.UserHome, "Library", "LaunchAgents"))
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".plist") {
				continue
			}
			label := strings.TrimSuffix(name, ".plist")
			if known[label] {
				continue
			}
			if isSystemLabel(label) || isSelfLabel(label) {
				continue
			}
			if f, ok := brewFormulaFromLabel(label); ok && knownFormula[f] {
				continue // 已经纳管过（只是 label 前缀不同）
			}
			path := filepath.Join(dir, name)
			c := AdoptCandidate{Label: label, PlistPath: path}
			c.Program = plistString(path, "Program")
			if c.Program == "" {
				// ProgramArguments 的第一项就是可执行文件
				c.Program = plistString(path, "ProgramArguments:0")
			}
			c.DisplayName = label
			c.LogPath = plistString(path, "StandardOutPath")
			if c.LogPath == "" {
				c.LogPath = plistString(path, "StandardErrorPath")
			}
			// 读运行状态
			if st, err := priv.LaunchStatus(label); err == nil {
				c.Running = st.Running
				c.PID = st.PID
				c.Loaded = st.Loaded
			}
			candidates = append(candidates, c)
		}
	}

	// 补充：**已加载但没有 plist 的作业**。
	//
	// 只看 plist 文件会漏掉一类真实存在的服务：作业还挂在 launchd 里跑着，
	// 但 /Library/LaunchDaemons 下已经没有它的 plist 了（用户手工注册、
	// 或清理过 plist）。真机上就是这样（com.vix.cron 等）——
	// 服务管理里看不到、可纳管列表里也找不到，用户完全无从下手。
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.Label] = true
	}
	for _, label := range m.loadedLabels(ctx) {
		if seen[label] || known[label] || knownFormula[label] || isSystemLabel(label) || isSelfLabel(label) {
			continue
		}
		// Apple 自带的作业（plist 只在 /System/Library/...）不该出现在"可纳管"里：
		// 它们属于 macOS 本身，纳管既没意义也容易被误停。
		if isAppleSystemJob(label) {
			continue
		}
		c := AdoptCandidate{Label: label, DisplayName: label}
		if st, err := priv.LaunchStatus(label); err == nil {
			c.Running = st.Running
			c.PID = st.PID
			c.Loaded = st.Loaded
		}
		candidates = append(candidates, c)
	}

	// 按可执行文件去重：同一个程序可能有多套 label
	// （例如 brew 的 homebrew.mxcl.php 与 sh.brew.php@8.3 指向同一个 php-fpm），
	// 只保留"正在运行"的那个，否则用户会在列表里看到重复且互相矛盾的条目。
	byProgram := map[string]int{}
	deduped := make([]AdoptCandidate, 0, len(candidates))
	for _, c := range candidates {
		key := c.Program
		if key == "" {
			deduped = append(deduped, c)
			continue
		}
		if idx, seen := byProgram[key]; seen {
			// 已有同程序的条目：运行中的优先
			if c.Running && !deduped[idx].Running {
				deduped[idx] = c
			}
			continue
		}
		byProgram[key] = len(deduped)
		deduped = append(deduped, c)
	}
	return deduped, nil
}

// AdoptCandidate 是一个可纳管候选项。
type AdoptCandidate struct {
	Label       string `json:"label"`
	DisplayName string `json:"display_name"`
	PlistPath   string `json:"plist_path"`
	Program     string `json:"program"`
	LogPath     string `json:"log_path"`
	Running     bool   `json:"running"`
	PID         int    `json:"pid"`
	// Loaded 表示这个作业此刻挂在 launchd 里（不管有没有进程）。
	//
	// 为什么需要：macOS 很多作业是**按需启动**的（launchd 在需要时才拉起，
	// 平时没有进程）。只报"已停止"会让用户以为服务坏了 ——
	// com.vix.cron（系统 cron）就是这样：loaded、无 PID，一切正常。
	Loaded bool `json:"loaded"`
}

// selfLabels 是不允许纳管的服务：面板自身与关键基础设施。
//
// 为什么必须排除面板自己：如果用户在面板里"停止面板服务"，
// 会立刻失去访问入口，这是一个不可恢复的操作（除非有 SSH）。
// nginx 同理 —— 停止它会让所有站点离线，属于应该由专门入口管理的对象。
var selfLabels = map[string]bool{
	"cn.zizpanel.panel": true,
	"cn.zizdog.nginx":   true,
}

// isSelfLabel 判断是否为面板自身或关键基础设施。
//
// 除了点名的那两个，还一刀切排除 `cn.zizpanel.` 前缀 ——
// 那是面板自己的作业（例如计划任务生成的 cn.zizpanel.cron.daily-backup）。
// 真机上"可纳管扫描"就把面板自己的定时任务列了出来：它对用户毫无意义
// （计划任务有专门页面管理），纳管进来还会多出一条莫名其妙的记录。
func isSelfLabel(label string) bool {
	if selfLabels[label] {
		return true
	}
	return strings.HasPrefix(label, "cn.zizpanel.")
}

// isSystemLabel 判断是否为系统自带服务（不应出现在纳管列表里）。
func isSystemLabel(label string) bool {
	systemPrefixes := []string{
		"com.apple.", "com.openssh.", "org.cups.", "com.microsoft.",
		// com.vix.cron 是 macOS 自带的 cron（/System/Library/LaunchDaemons）——
		// 它属于系统，纳管它没有意义，报"已停止"更是误导。
		"com.vix.",
		"com.google.", "com.adobe.", "com.docker.vmnetd", "com.openssh.sshd",
		// 网络扩展（Tailscale / VPN 等）由系统网络栈托管，
		// 面板没有能力也没有必要去启停它 —— 列出来只会让用户误点。
		"NetworkExtension.", "com.tailscale.", "io.tailscale.",
	}
	for _, p := range systemPrefixes {
		if strings.HasPrefix(label, p) {
			return true
		}
	}
	return false
}

// AutoRegisterKnown 把「面板已知且本机确实存在」的服务自动登记进服务管理。
//
// 为什么需要它：服务管理应该是"本机装了什么"的如实反映，而不是
// "用户手工登记过什么"。早期只有在安装那一刻才登记，于是：
//
//	· 换台机器/重装面板后，已经在跑的服务全都不见了
//	· 用户以为面板坏了，其实只是没登记
//
// 现在面板启动时自动对齐一次 —— 幂等，已登记的会跳过。
//
// 只自动登记**目录里已知 label** 的服务（nginx/php/mysql/Qwen/接收端/IOPaint…）：
// 这些面板知道该怎么管（端口、健康检查、日志位置）。
// 其它任意第三方服务仍然走手动纳管 —— 自动接管一个我们不了解的服务，
// 会在面板里显示成一条信息不全、状态不准的记录，反而更误导。
func (m *Manager) AutoRegisterKnown(ctx context.Context) int {
	// 先把已有记录按「label 家族」建成集合：同一家族只允许一条记录。
	//
	// 真机现象（2026-09-17 用户反馈）：同一个 php-fpm 在服务管理里出现两条 ——
	// `php82` 与 `homebrew-mxcl-php8-2`。根因是两处的 label 写法互为别名
	// （`homebrew.mxcl.` / `sh.brew.` 是两套前缀，`.` 与 `-` 只是归一化差异），
	// 而 RegisterInstalledService 的幂等判定只看**完全相等**的 label，
	// 于是同一家族被登记了两遍。前端会做展示去重，但源头也要修。
	knownFamily := map[string]bool{}
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if fam := labelFamily(s.LaunchLabel); fam != "" {
					knownFamily[fam] = true
				}
			}
		}
	}
	n := 0
	for _, a := range Catalog() {
		if a.ServiceLabel == "" {
			continue
		}
		// Docker 运行时走 EnsureColimaRuntime 的专用登记路径（kind=colima）。
		// 如果在这里也登记一遍，同一个东西会在「服务管理」里出现两次：
		// 一次是运行中的运行时，另一次是 kind=native 且永远显示"已加载但未运行"
		// —— 因为 colima 的开机任务是"起来就退出"的一次性作业。
		if a.Kind == KindColima {
			continue
		}
		fam := labelFamily(a.ServiceLabel)
		if fam != "" && knownFamily[fam] {
			continue
		}
		if !m.launchServicePresent(ctx, a.ServiceLabel) {
			continue
		}
		icon := a.Icon
		if icon == "" {
			icon = "🧩"
		}
		if err := m.RegisterInstalledService(ctx, a.ServiceLabel, a.Name, icon, a.Category, a.Port); err == nil {
			n++
			if fam != "" {
				// 同一次循环里也要防住：万一目录里还有同家族的另一个条目。
				knownFamily[fam] = true
			}
		}
	}
	return n
}

// labelFamily 把一个 launchd 标签 / 服务记录名归一化成"家族键"，
// 用来判断两条记录是不是**同一个东西**。
//
// 归一化规则（照着真机上真实出现的两种写法来）：
//   - `homebrew.mxcl.` 与 `sh.brew.` 互为别名，都剥掉（点号形态与连字符形态都认）；
//   - 其余交给 NormalizeName：`.` / `_` / 空格 → `-`，`@` 直接丢弃
//     （所以标签 `homebrew.mxcl.php@8.2` 与记录名 `homebrew-mxcl-php8-2`
//     归一化后都是 `php8-2`）。
//
// 只用于**去重**判定，不参与任何展示：展示名仍走 FriendlyName。
func labelFamily(label string) string {
	s := strings.ToLower(strings.TrimSpace(label))
	if s == "" {
		return ""
	}
	for _, p := range []string{
		"homebrew.mxcl.", "sh.brew.", // 标签形态
		"homebrew-mxcl-", "sh-brew-", // 记录名形态（NormalizeName 之后）
	} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	return NormalizeName(s)
}

// launchServicePresent 判断某个 launchd 服务在本机是否存在。
//
// 两个来源都要看：plist 文件（常规），以及 launchd 已加载的作业。
// 实测有服务**没有 plist 但作业在跑**（用户手工注册、或 plist 被清掉了），
// 只看文件会把它们判成"不存在"。
func (m *Manager) launchServicePresent(ctx context.Context, label string) bool {
	if label == "" {
		return false
	}
	if fileExists(filepath.Join("/Library/LaunchDaemons", label+".plist")) {
		return true
	}
	if m.opt.UserHome != "" &&
		fileExists(filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")) {
		return true
	}
	// 系统域或用户域里已加载也算存在
	if _, err := m.runRoot(ctx, 8*time.Second, "/bin/launchctl", "print", "system/"+label); err == nil {
		return true
	}
	if m.opt.UID > 0 {
		if _, err := m.runRoot(ctx, 8*time.Second, "/bin/launchctl", "print",
			fmt.Sprintf("gui/%d/%s", m.opt.UID, label)); err == nil {
			return true
		}
	}
	return false
}

// loadedLabels 列出 launchd 里已加载的作业 label。
//
// 用 `launchctl list` 而不是 `launchctl print <domain>`：
// 后者的服务列表格式随系统版本变化，用正则去抠很脆
// （我第一版就是那么写的，实测一条都没匹配上，于是"已加载但无 plist"
//
//	的服务在可纳管列表里依然看不见）。
//
// `launchctl list` 是稳定的三列格式：PID  Status  Label。
func (m *Manager) loadedLabels(ctx context.Context) []string {
	res, err := m.runRoot(ctx, 15*time.Second, "/bin/launchctl", "list")
	if err != nil {
		return nil
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)
	out := []string{}
	for _, ln := range strings.Split(string(res), "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		label := f[len(f)-1]
		// 跳过表头：launchctl list 的第一行是 "PID  Status  Label"，
		// 不排除的话会多出一条名叫 "Label" 的假服务（实测踩到过）。
		if strings.EqualFold(label, "Label") || strings.EqualFold(f[0], "PID") {
			continue
		}
		if !valid.MatchString(label) {
			continue
		}
		out = append(out, label)
	}
	return out
}

// isAppleSystemJob 判断某个 launchd 作业是不是 macOS 自带的。
//
// 判据：它的 plist 只在 /System/Library/LaunchDaemons 或 LaunchAgents 下。
// 这类作业（cron、各种 com.apple.*）属于系统本身，
// 面板既不该纳管、也不该在"可纳管服务"里列出来让用户误停。
func isAppleSystemJob(label string) bool {
	for _, dir := range []string{"/System/Library/LaunchDaemons", "/System/Library/LaunchAgents"} {
		if _, err := os.Stat(filepath.Join(dir, label+".plist")); err == nil {
			return true
		}
	}
	return false
}

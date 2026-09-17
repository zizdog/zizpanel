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
	// Compose 安装：compose 文件内容
	ComposeYAML string `json:"compose_yaml"`
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
// 面板探测到代理不可用时会把「直连端口」作为首选入口，而不是假装能打开。
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
	// 与 Note 的区别：Note 只是说明，PreferDirect 会改变界面行为 ——
	// 「打开」按钮直接给端口直连，子路径降级成次要入口（"试试子路径"）。
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
	CategorySite    = "site"    // 一键建站
	CategoryAI      = "ai"      // AI 服务
	CategoryTool    = "tool"    // 运维工具
	CategoryLNMP    = "lnmp"    // 网站环境（服务管理里 nginx / PHP / MySQL 的归类）
	CategoryRuntime = "runtime" // 容器运行时（Docker 运行时 Colima）
	// CategoryOther 同时是**兜底分类**：目录里没有专属板块的分类
	// （lnmp / runtime / 以及将来新增的）都会被它收下。
	// 所以它的显示名取"最能描述这一整类"的 —— 用户 2026-09 要求显示为「基础环境」。
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
// 两个硬约束：
//  1. 显示名来自 categoryLabels（与 CategoryLabels 同源）——"改一处、到处生效"；
//  2. 兜底板块必须排在**最后**（用户明确要求「基础环境」仍在最后一个板块）。
//     它收纳的是没有专属板块的分类，排在中间会让后面的板块与它混在一起。
func MarketSections() []MarketSection {
	keys := []string{CategorySite, CategoryAI, CategoryTool, CategoryOther}
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
			Summary: "AI 擦除水印与杂物，支持批量与视频",
			Description: "上传图片、涂抹要去掉的水印即可擦除；走原生安装，用 LaMa 模型并启用 " +
				"Apple Silicon MPS 加速，模型约 200MB。可切换更强模型，" +
				"但 MAT / ZITS / LDM 在 M 系列上不支持 MPS。",
			Category: "tool", Kind: KindNative, PanelInstaller: "iopaint", ServiceLabel: "com.zizdog.iopaint",
			Port: 8080,
			// IOPaint 的**视频**去水印路径要 ffmpeg 拆帧/合帧（市场描述里的"支持视频"
			// 就是它）；图片路径不需要。面板不直接调用 ffmpeg，所以这里只作提示 +
			// 部署时一并装好 —— 装上不会有副作用（它本来就是基础环境）。
			Requires: []Requirement{{Type: "brew_formula", Value: "ffmpeg",
				Hint: "brew install ffmpeg（IOPaint 的视频处理需要它，安装时会一并安装）"}},
			DocsURL: "https://github.com/Sanster/IOPaint",
		},
		{
			ID: "qwen3tts", Name: "Qwen3 TTS（语音合成）", Icon: "🗣️",
			Summary: "本地语音合成，支持音色克隆",
			Description: "按 TtsVoice 插件契约原生安装：Python 3.11 + mlx-audio[server] " +
				"+ 1.7B-Base-8bit 模型（约 2.9GB，用于音色克隆），网站插件只支持" +
				"「自定义音色」，故只需这一个模型。" +
				"加鉴权后 Qwen 只监听本机，对外由音色接收端（8899）带密钥反代。",
			Category: "ai", Kind: KindNative, PanelInstaller: "qwen3tts", ServiceLabel: "com.zizdog.qwen3tts",
			Port:       8880,
			HealthPath: audioHealth,
			// ffmpeg 是它的**硬依赖**：mlx_audio 编码 mp3 必须靠它，缺了就会返回
			// HTTP 200 + 0 字节 body（2026-09-16 事故的确切成因）。用目录既有的
			// Requires 机制声明 —— 安装前检查会提示，部署时由
			// EnsureBaseDependencies 一并装好，不需要前端配合新字段。
			Requires: []Requirement{{Type: "brew_formula", Value: "ffmpeg",
				Hint: "brew install ffmpeg（TTS 编码 mp3 必需，部署时会一并安装）"}},
			DocsURL: "https://github.com/Blaizzy/mlx-audio",
		},
		{
			ID: "voicereceiver", Name: "TtsVoice 音色接收端", Icon: "🔐",
			Summary: "接收音色样本 + 带鉴权的反向代理",
			Description: "给网站插件用的对外入口：接收上传的音色样本（落到本机，" +
				"因为上游只认本地文件路径），并把 /v1/* 反代给只监听本机的 8880。" +
				"共享密钥可指定或自动生成。",
			Category: "ai", Kind: KindNative, PanelInstaller: "voicereceiver", ServiceLabel: "com.zizdog.voicereceiver",
			Port: 8899,
			// 接收端有免鉴权的 GET /health（实测 200；/jobs 等才要密钥）。
			// 这里原先**没配** HealthPath —— 后果是面板"纳管了却不监测"：
			// 服务列表里它的 health 永远是 checked=false/ok=false，
			// 用户看到的就是"TTS 没在管理/不知道死活"（2026-09-16 用户反馈）。
			HealthPath: "/health",
			// 接收端自己就用 ffmpeg / ffprobe（样本真解码校验 + 转码与归一），
			// 同时它是上游 Qwen 编码 mp3 的必经环节。用目录既有的 Requires 声明，
			// 安装前检查会提示，部署时由 EnsureBaseDependencies 一并装好。
			Requires: []Requirement{{Type: "brew_formula", Value: "ffmpeg",
				Hint: "brew install ffmpeg（样本校验与音频转码必需，部署时会一并安装）"}},
			// 契约来源是网站侧插件目录里的 HANDOFF-TO-MINI.md。
			// 这里原先错填成 phpmyadmin.net（复制粘贴残留），会把人引到无关文档。
			DocsURL: "https://github.com/Blaizzy/mlx-audio",
		},
		{
			ID: "phpmyadmin", Name: "phpMyAdmin", Icon: "🐬",
			// phpMyAdmin 没有守护进程（nginx alias + php-fpm），装完就是一个网页入口。
			// 它的 nginx location 由安装器自己写，所以 SelfConf=true：面板只给「打开」。
			UI:       &AppUI{Slug: "phpmyadmin", SelfConf: true},
			NoDaemon: true,
			Summary:  "数据库管理界面（推荐入口）",
			Description: "面板自研的库表管理功能有限，日常的库/表/权限/导入导出建议用它。" +
				"装好后接入 nginx 默认站点，访问 http://<本机地址>/phpmyadmin/；" +
				"面板内置那套保留为应急入口。",
			Category: "tool", Kind: KindNative, PanelInstaller: "phpmyadmin", BrewFormula: "phpmyadmin",
			Port:       0,
			HealthPath: "/phpmyadmin/",
			DocsURL:    "https://www.phpmyadmin.net",
		},

		// ---------------- 网站环境（LNMP，原生安装） ----------------
		//
		// ⚠️ 这里只有**单个组件**（nginx / 各版本 PHP / MySQL），
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
		{
			ID: "nginx", Name: "Nginx", Icon: "🌐",
			Summary: "Web 服务器，网站管理功能的基础",
			Description: "面板的「网站管理」依赖它：新建站点、伪静态、SSL、反向代理" +
				"都是往它的 vhosts 目录写配置并 reload。" +
				"装好后面板会自动补齐 include 上下文与 WebSocket 升级映射。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.nginx",
			Port: 80, HealthPath: "/",
			BrewFormula: "nginx",
			LogPath:     "~/Library/Logs/homebrew.mxcl.nginx.log",
			DocsURL:     "https://nginx.org",
		},
		{
			ID: "php83", Name: "PHP 8.3 (FPM)", Icon: "🐘",
			Summary: "PHP FastCGI 进程管理器，供站点解析 PHP",
			Description: "以 FastCGI 方式监听 自己专属的 Unix socket 端点，由 nginx 转发 PHP 请求。" +
				"面板的站点配置默认指向这个地址；多版本 PHP 可以再装其它版本共存。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.php@8.3",
			Port: 0,
			// PHP-FPM 说的是 FastCGI 协议，不是 HTTP —— 不能做 HTTP 健康检查，
			// 否则会永远显示不健康。留空表示"只按进程与端口判断"。
			HealthPath:  "",
			BrewFormula: "php@8.3",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			LogPath:      "~/Library/Logs/homebrew.mxcl.php@8.3.log",
			DocsURL:      "https://www.php.net",
		},
		{
			ID: "php81", Name: "PHP 8.1 (FPM)", Icon: "🐘",
			Summary: "PHP FastCGI 进程管理器，供站点解析 PHP",
			Description: "与其它 PHP 版本**共存**：面板会把它配置成监听自己专属的端点" +
				"（默认 Unix socket /opt/homebrew/var/run/php-fpm-8.1.sock），" +
				"因此不会和其它版本抢 9000 端口；站点在「网站管理」里按站点选择用哪个版本。" +
				"装完若提示「端点未配置」，点「🔧 修复端点并重启」即可（会改写该版本的 www.conf 并重启 fpm）。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.php@8.1",
			// PHP-FPM 说的是 FastCGI 协议、不是 HTTP：不能做 HTTP 健康检查，端点由面板按版本分配。
			Port:        0,
			HealthPath:  "",
			BrewFormula: "php@8.1",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			LogPath:      "~/Library/Logs/homebrew.mxcl.php@8.1.log",
			DocsURL:      "https://www.php.net",
		},
		{
			ID: "php82", Name: "PHP 8.2 (FPM)", Icon: "🐘",
			Summary: "PHP FastCGI 进程管理器，供站点解析 PHP",
			Description: "与其它 PHP 版本**共存**：面板会把它配置成监听自己专属的端点" +
				"（默认 Unix socket /opt/homebrew/var/run/php-fpm-8.2.sock），" +
				"因此不会和其它版本抢 9000 端口；站点在「网站管理」里按站点选择用哪个版本。" +
				"装完若提示「端点未配置」，点「🔧 修复端点并重启」即可（会改写该版本的 www.conf 并重启 fpm）。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.php@8.2",
			// PHP-FPM 说的是 FastCGI 协议、不是 HTTP：不能做 HTTP 健康检查，端点由面板按版本分配。
			Port:        0,
			HealthPath:  "",
			BrewFormula: "php@8.2",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			LogPath:      "~/Library/Logs/homebrew.mxcl.php@8.2.log",
			DocsURL:      "https://www.php.net",
		},
		{
			ID: "php84", Name: "PHP 8.4 (FPM)", Icon: "🐘",
			Summary: "PHP FastCGI 进程管理器，供站点解析 PHP",
			Description: "与其它 PHP 版本**共存**：面板会把它配置成监听自己专属的端点" +
				"（默认 Unix socket /opt/homebrew/var/run/php-fpm-8.4.sock），" +
				"因此不会和其它版本抢 9000 端口；站点在「网站管理」里按站点选择用哪个版本。" +
				"装完若提示「端点未配置」，点「🔧 修复端点并重启」即可（会改写该版本的 www.conf 并重启 fpm）。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.php@8.4",
			// PHP-FPM 说的是 FastCGI 协议、不是 HTTP：不能做 HTTP 健康检查，端点由面板按版本分配。
			Port:        0,
			HealthPath:  "",
			BrewFormula: "php@8.4",
			// 必须开机就在：装成系统级 LaunchDaemon（无头机器开机不加载用户级 agent，坑 130）
			SystemDaemon: true,
			LogPath:      "~/Library/Logs/homebrew.mxcl.php@8.4.log",
			DocsURL:      "https://www.php.net",
		},
		{
			ID: "mysql84", Name: "MySQL 8.4", Icon: "🐬",
			Summary: "关系型数据库，供站点与面板的数据库管理使用",
			Description: "面板的「数据库管理」通过它管理库、表、账号与导入导出。" +
				"macOS 上 brew 版默认用 /tmp/mysql.sock，root 初始无密码。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "sh.brew.mysql@8.4",
			Port: 3306, HealthPath: "",
			BrewFormula: "mysql@8.4",
			LogPath:     "~/Library/Logs/homebrew.mxcl.mysql@8.4.log",
			DocsURL:     "https://dev.mysql.com",
		},

		// ---------------- 基础环境（原生安装，命令行工具） ----------------
		//
		// ffmpeg 是"基础环境"，不属于任何单个应用，但很多功能都靠它：
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
			Summary: "音视频转码基础工具（TTS 编码 mp3 依赖它）",
			Description: "面板的基础环境之一：Qwen TTS 用 mlx_audio 编码 mp3 必须靠它，" +
				"音色接收端用 ffprobe 校验上传的音色样本、用 ffmpeg 做转码与响度归一，" +
				"后续的音视频功能也都要用。" +
				"缺了它的典型症状是「合成接口返回 HTTP 200 但 body 是 0 字节」——" +
				"服务看起来一切正常，用户却一个作业都跑不成。" +
				"没有网页界面，装好后供面板与其它应用在后台调用。",
			Category: "tool", Kind: KindNative,
			PanelInstaller: "ffmpeg",
			BrewFormula:    "ffmpeg",
			// 纯命令行工具：没有守护进程、没有端口、没有网页界面。
			NoDaemon: true,
			Port:     0,
			DocsURL:  "https://ffmpeg.org",
		},
		// PostgreSQL 放在「基础环境」是**用户 2026-09-17 的决定**，理由与 MySQL 不同：
		//   · MySQL 是「网站环境」的一部分（一键 LNMP 装它，Typecho / WordPress 用它）；
		//   · PostgreSQL 目前只服务于自托管应用（Miniflux 必须要它），
		//     不属于网站栈，所以归到「基础环境」，与 mysql84 一样是
		//     **独立可安装、可启停、可卸载**的条目。
		//
		// 为什么不把它塞进 Miniflux 的安装事务（研究与真机结论都支持）：
		//   · brew 的 postgresql@17 是**一个 cluster 一个数据目录**
		//     （/opt/homebrew/var/postgresql@17）。若 PG 由 Miniflux 的安装流程顺带装，
		//     就会出现"Miniflux 装失败该不该卸 PG"的两难 —— 卸掉会伤到别的依赖方，
		//     不卸就留一个孤儿；更糟的是 cluster 的归属会随安装顺序漂移。
		//   · 独立条目则安装 / 启停 / 卸载各自幂等、与应用解耦；卸载 Miniflux
		//     不会碰这个 cluster（数据保留语义见 uninstall 计划）。
		{
			ID: "postgresql17", Name: "PostgreSQL 17", Icon: "🐘",
			Summary: "关系型数据库（自托管应用用，例如 Miniflux）",
			Description: "PostgreSQL 17（brew formula postgresql@17）。给需要它的自托管应用用，" +
				"目前是 Miniflux（装 Miniflux 时会自动确认并安装它）。" +
				"brew 安装时**已经自动 initdb 建好 cluster**（数据目录 /opt/homebrew/var/postgresql@17），" +
				"本机连接走 trust 认证，超级用户就是当前登录用户。" +
				"卸载本条目**不会**删除数据目录（面板默认保留你的数据）。",
			Category: "other", Kind: KindNative, ServiceLabel: "sh.brew.postgresql@17",
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
			Summary: "本地大模型推理，支持 Metal 加速",
			Description: "一行命令跑起本地大模型（Llama / Qwen / DeepSeek 等）。" +
				"原生安装才能用 Apple Silicon 的 GPU 加速，Docker 里用不了 Metal。",
			Category: "ai", Kind: KindNative,
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
		{
			ID: "docker-runtime", Name: "Docker 运行时（Colima）", Icon: "🐳",
			Summary: "容器引擎，Docker 类应用的前提",
			Description: "Colima 在轻量 Linux 虚拟机里跑 Docker 引擎，原生支持 Apple Silicon，" +
				"装好后开机自启、无需登录桌面；停止它会让所有容器一起停掉。",
			Category: "runtime", Kind: KindColima, PanelInstaller: "docker-runtime",
			// BrewFormula 用于判断"装没装"；ServiceLabel 用于判断"纳没纳管"。
			// 两者都要给，否则市场会显示"已安装·未纳管"并给出一个点了会报错的纳管按钮。
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
				Note: "Uptime Kuma 官方不支持子路径（改写到页面前端路由后是 Page Not Found，实测于 mini）",
				// 真机实测：/uptime-kuma/ 能返回页面、资源全 200，但正文是
				// "Page Not Found" —— 它的 Vue 路由不认这个前缀，只有官方
				// 那套改 entrypoint 的社区方案才能挂子路径，面板不做那种侵入。
				PreferDirect: true,
			},
			Summary:     "自托管服务监控与告警",
			Description: "监控网站与服务的可用性，支持多种通知渠道（Telegram / Bark / 邮件等）。",
			Category:    "tool", Kind: KindCompose, Port: 3001,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			ComposeYAML: composeTemplate("uptime-kuma", "louislam/uptime-kuma:1", 3001, 3001, `
    volumes:
      - ./data:/app/data
    restart: unless-stopped`),
			DocsURL: "https://github.com/louislam/uptime-kuma",
		},
		{
			ID: "minio", Name: "MinIO", Icon: "🪣",
			UI: &AppUI{
				Slug:         "minio",
				Note:         "MinIO 控制台需要在容器环境变量里设 MINIO_BROWSER_REDIRECT_URL 才能用子路径（面板只做改写，不保证可用）",
				PreferDirect: true,
			},
			Summary: "S3 兼容的对象存储",
			// 刻意避开 9000：那是 PHP-FPM 的固定端口（站点 vhost 都指向
			// 127.0.0.1:9000），MinIO 默认也用 9000，两者会真的抢端口 ——
			// 在装了 PHP 的机器上 MinIO 会直接起不来。
			// PHP 的 9000 是约定不能动，所以让路的是 MinIO。
			Description: "自建对象存储，适合存放图片、备份、模型文件。" +
				"API 用 9010、控制台用 9011（避开 PHP-FPM 占用的 9000）。",
			Category: "tool", Kind: KindCompose, Port: 9010,
			HealthPath: "/minio/health/live",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: `services:
  minio:
    image: quay.io/minio/minio:latest
    container_name: minio
    ports:
      # 宿主机改用 9010/9011：9000 留给 PHP-FPM
      - "9010:9000"
      - "9011:9001"
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    command: server /data --console-address ":9001"
    volumes:
      - ./data:/data
    restart: unless-stopped
`,
			PostInstallHint: "默认账号密码均为 minioadmin，请登录后立即修改。控制台：http://127.0.0.1:9001",
			DocsURL:         "https://min.io",
		},
		{
			ID: "n8n", Name: "n8n", Icon: "🔗",
			UI: &AppUI{
				Slug:         "n8n",
				Websocket:    true,
				Note:         "n8n 需要在环境变量里设 N8N_PATH=/n8n/ 才能用子路径（面板只做改写，不保证可用）",
				PreferDirect: true,
			},
			Summary:     "可视化自动化工作流",
			Description: "用节点拖拽的方式编排自动化流程，可以对接 HTTP / 数据库 / AI 接口。",
			Category:    "tool", Kind: KindCompose, Port: 5678,
			HealthPath: "/healthz",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			// 为什么用 Docker Hub 的 `n8nio/n8n` 而不是它自己的 `docker.n8n.io/n8nio/n8n`
			// （2026-09-16 实测后改，别改回去）：
			//   · `docker.n8n.io/v2/n8nio/n8n/manifests/latest` 返回 401，而
			//     WWW-Authenticate 的 realm 指向 **auth.docker.io** —— 也就是说
			//     n8n 自己的 registry 把鉴权委托给了 Docker Hub，而 auth.docker.io
			//     在国内直连超时 → **拉不动**（域名 401 只说明"活着"，不代表能用）。
			//   · 更关键的是 **registry-mirrors 只对 Docker Hub 生效**：配了 NAS /
			//     公共加速源也救不了非 Hub 的域名。所以走 Hub 才是能加速的那条路。
			//   · 这是**同一个官方项目**（n8n 官方把 Hub 的 n8nio/n8n 作为发布渠道，
			//     不是第三方转存）。
			// arm64 证据（经自建 NAS 镜像站读同一份 index，2026-09-16 实测）：
			//   GET <mirror>/docker/v2/n8nio/n8n/manifests/latest
			//   → linux/amd64、linux/arm64；arm64 子清单 15 层共 282.9MB
			ComposeYAML: composeTemplate("n8n", "n8nio/n8n:latest", 5678, 5678, `
    environment:
      - N8N_SECURE_COOKIE=false
      - GENERIC_TIMEZONE=Asia/Shanghai
    volumes:
      - ./data:/home/node/.n8n
    restart: unless-stopped`),
			DocsURL: "https://n8n.io",
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
			Category:    "tool", Kind: KindCompose, Port: 3000,
			HealthPath: "/api/healthz",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: composeTemplate("gitea", "gitea/gitea:latest", 3000, 3000, `
    environment:
      - USER_UID=1000
      - USER_GID=1000
    volumes:
      - ./data:/data
      - /etc/timezone:/etc/timezone:ro
      - /etc/localtime:/etc/localtime:ro
    restart: unless-stopped`),
			DocsURL: "https://gitea.com",
		},
		{
			ID: "stirling-pdf", Name: "Stirling PDF", Icon: "📄",
			UI: &AppUI{
				Slug:         "stirling-pdf",
				Note:         "Stirling 需要自己在配置里设 base path，面板只能硬挂；探测不通过时请用端口直连",
				PreferDirect: true,
			},
			Summary:     "本地 PDF 工具箱",
			Description: "合并、拆分、压缩、OCR、转图片等 PDF 操作，全部在本机完成，不上传云端。",
			Category:    "tool", Kind: KindCompose, Port: 8082,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: composeTemplate("stirling-pdf", "stirlingtools/stirling-pdf:latest", 8082, 8080, `
    volumes:
      - ./data:/configs
    restart: unless-stopped`),
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
			UI:      &AppUI{Slug: "it-tools"},
			Summary: "几十个开发者常用小工具，纯前端",
			Description: "JSON 格式化、Base64/URL 编解码、UUID/哈希、时间戳、正则、" +
				"JWT、CIDR 等开发者小工具合集。走 Docker，纯前端本地执行、" +
				"输入不上传，端口 8083。",
			Category: "tool", Kind: KindCompose, Port: 8083,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// 官方镜像（README 里给的就是 corentinth/it-tools 与 ghcr.io/corentinth/it-tools
			// 两个地址）。这里用 ghcr：与本机网络实测有关 —— ghcr.io 可直连，
			// Docker Hub 直连超时；两者是同一个官方项目，不是第三方转存。
			// arm64 证据（ghcr.io 直查）：
			//   docker manifest inspect ghcr.io/corentinth/it-tools:latest
			//   → linux/amd64、linux/arm64（另有两个 unknown/unknown 的 attestation）
			ComposeYAML: composeTemplate("it-tools", "ghcr.io/corentinth/it-tools:latest", 8083, 80, `
    restart: unless-stopped`),
			DocsURL: "https://github.com/CorentinTh/it-tools",
		},
		{
			ID: "filebrowser", Name: "File Browser（网页文件管理）", Icon: "🗂️",
			UI: &AppUI{
				Slug: "filebrowser",
				// compose 里用 `command: ["-b", "/filebrowser"]` 让它自带前缀，
				// 所以面板不再改写路径（否则会 /filebrowser/static/filebrowser/…）
				SelfBase: true,
				Note:     "File Browser 需要以 -b /filebrowser 启动才能用子路径；compose 里已带上，改动过 compose 的话请同步",
			},
			Summary: "在浏览器里管理服务器上的文件",
			Description: "浏览器里浏览、上传、下载、分享文件，支持多用户与细粒度权限；" +
				"它不像面板自带文件管理器那样限定白名单目录。" +
				"因 Homebrew 版没有 service 定义，仍走 Docker，端口 8081，" +
				"默认管理 compose 目录下的 data/。",
			Category: "tool", Kind: KindCompose, Port: 8081,
			// 官方镜像的 healthcheck 打的就是 /health（见仓库 docker/common/healthcheck.sh），
			// 不是猜测的路径。
			HealthPath: "/health",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			// 为什么有 brew formula 却走 Docker（唯一的例外，理由要写清楚）：
			// homebrew/core 的 filebrowser（2.63.23，bottle 覆盖 arm64_tahoe/sequoia）
			// **没有 service 定义** —— 读 `brew info --json=v2 filebrowser` 的 service
			// 字段是 null，formula 源码里也没有 `service do` 块。
			// 而面板的原生路径装完必须 `brew services start`，对这类 formula 必然失败：
			// 结果是一条 launchd label 指向不存在 plist 的"托管"记录 —— 市场说已安装，
			// 服务管理里却永远起不来。宁可用官方 Docker 镜像，也不留这种假成功。
			// arm64 证据（Docker Hub 经镜像站读同一份 index）：
			//   docker manifest inspect docker.1ms.run/filebrowser/filebrowser:latest
			//   → linux/amd64、linux/arm64、linux/arm/v7
			//
			// 2026-09-14 用户反馈后的两处调整：
			//  ① **子路径**：它的前端用绝对路径（/static/…、/api/…），挂在
			//     /filebrowser/ 下会一直转圈打不开（真机实测）。官方支持 `-b/--baseurl`，
			//     而镜像的 /init.sh 会把参数原样转发给 filebrowser，所以直接加
			//     `command: ["-b", "/filebrowser"]` —— 比在代理里改写路径可靠得多。
			//  ② **默认目录**：用户要的是"Mac 用户那几个常用目录，别的不要看到"，
			//     所以把 5 个目录分别挂成 /srv/<中文名>，而不是把整个家目录挂进去。
			ComposeYAML: `services:
  filebrowser:
    image: filebrowser/filebrowser:latest
    container_name: filebrowser
    # 官方镜像默认以 uid 1000 的 user 运行，而面板创建的 compose 目录与
    # bind mount 属主是 root —— 它的 init.sh 会因写不了 /config/settings.json
    # 直接退出（set -e）。容器内以 root 运行即可，与目录里其它镜像一致。
    user: "0:0"
    # 子路径：镜像的 /init.sh 会把参数透传给 filebrowser，所以这一行就能生效
    command: ["-b", "/filebrowser"]
    ports:
      - "8081:80"
    volumes:
      # 只挂这几个常用目录（界面上就是 /srv 下的 5 个条目）。
      # 想改挂载：Docker → Compose → filebrowser → 编辑 yml → 重新部署。
      # ⚠️ macOS 的「文档/下载/桌面」等目录受隐私保护（TCC）：没给 Colima
      #    （以及面板）授予「完全磁盘访问权限」时，容器里是空的、终端里会报
      #    Operation not permitted。
      - ~/Documents:/srv/文档
      - ~/Downloads:/srv/下载
      - ~/Music:/srv/音乐
      - ~/Movies:/srv/影片
      - ~/Desktop:/srv/桌面
      - ./config:/config
      - ./database:/database
    restart: unless-stopped
`,
			// 首次启动会自动生成 admin 密码并打在容器日志里；面板部署完会把它捞出来
			// 直接显示在安装结果里（用户不用再去服务管理翻日志）。
			PostInstallHint: "默认用户名 admin，密码是首次启动随机生成的 —— 面板会把它显示在安装结果里。" +
				"没看到就去「服务管理 → File Browser → 日志」找 `randomly generated password`，登录后请立刻改掉。",
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
			Summary: "把本机端口映射到 frps（客户端，带 admin UI）",
			Description: "fatedier/frp 的客户端：连上你自己的 frps，把本机端口映射出去。" +
				"**走原生（不走 Docker）**：官方 darwin-arm64 tarball 解压到 ~/frpc " +
				"并用系统级 launchd 托管 —— 客户端要暴露的是" +
				"**这台 Mac 上**的服务，放进容器后 127.0.0.1 会指向容器自己。" +
				"admin UI 在 7400。**面板不提供 frps**，服务端地址与 token 请自己填。",
			Category: "tool", Kind: KindNative,
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
		{
			ID: "orbien-client", Name: "Orbien 客户端（CLI）", Icon: "🛰️",
			// 客户端没有自己的 Web 界面（就是上游的 orbien CLI），所以不给 UI 入口 ——
			// 不渲染一个点开必然打不开的按钮。它的可视化配置入口是服务详情里的
			// 「📝 编辑配置文件」（orbien.toml），要看图表请开服务端的 Dashboard(8020)。
			Summary: "连上 Orbien 服务端，把本机端口穿透出去（客户端）",
			Description: "Orbien 的**客户端**（上游 CLI）。**走原生（不走 Docker）**：" +
				"官方 darwin-arm64 产物解压到 ~/orbien-client，由系统级 launchd 托管 ——" +
				"原生下 [[tunnels]] 的 service 直接写 127.0.0.1，容器里会指向容器自己。" +
				"客户端主动外连、不监听端口。**面板不提供服务端**，地址请自己填。",
			Category: "tool", Kind: KindNative,
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
			// PreferDirect 与 frpc 同一写法：默认给端口直连 http://<地址>:9876。
			// 已在真机核对过直连可用（未登录 GET / 是 307 → /login，浏览器正常进登录页）；
			// 子路径入口只作备用 —— 它的前端资源与接口是相对路径（./static、baseURL './'），
			// 在面板的子路径反代下是否完全可用**没有真机验证过**，不拿它当首选。
			UI: &AppUI{
				Slug:         "ddns-go",
				PreferDirect: true,
				Note: "首次配置（添加 DNS 服务商与域名）要在 ddns-go 自己的网页界面里做：" +
					"面板默认给端口直连 http://<地址>:9876，子路径入口仅作备用。",
			},
			Summary: "动态公网 IP 变化时自动更新到 DNS 解析（Cloudflare / 阿里云 / DNSPod …）",
			Description: "把变化的公网 IP 自动更新到你的域名解析，支持 Cloudflare、阿里云、" +
				"腾讯云、DNSPod、华为云、百度云等。**走原生（不走 Docker）**：官方 darwin-arm64 " +
				"预编译产物解压到 ~/ddns-go，由系统级 launchd 托管（homebrew 的 formula " +
				"没有 service 块，不能用 brew services 托管）。",
			Category: "tool", Kind: KindNative,
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
			Summary: "极简自托管 RSS 阅读器（需要 PostgreSQL）",
			Description: "自托管的 RSS 阅读器：没有广告、没有推荐算法，界面干净、键盘操作友好。" +
				"**必须要有 PostgreSQL**（上游 README 明确写 Works only with PostgreSQL）：" +
				"装它的时候面板会先确保 postgresql@17 已安装并启动，再自动建库建账号、" +
				"写好配置、跑完数据库迁移，最后随机生成管理员口令。" +
				"管理员口令只在安装结果里出现一次，请自行保存（之后可在服务详情里改配置）。",
			Category: "tool", Kind: KindNative,
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
			Summary: "去中心化文件同步（不经过云盘）",
			Description: "在多台设备之间直接同步文件，不经过任何云服务，支持版本历史与" +
				"选择性同步。**走原生（不走 Docker）**：homebrew 的 formula 自带 service 块，" +
				"官方就是按无头方式设计的（cask 那个才是菜单栏 App）。" +
				"面板会把 GUI 从上游默认的 127.0.0.1:8384 改成 0.0.0.0:8384，" +
				"**并同时设置随机用户名口令** —— 否则等于把远程控制台无口令暴露在局域网里。",
			Category: "tool", Kind: KindNative,
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
		{
			ID: "alist", Name: "Alist（文件列表）", Icon: "📂",
			UI: &AppUI{
				Slug:         "alist",
				PreferDirect: true,
				Note: "Alist 的 Web 界面在 5244。「打开」给端口直连 http://<本机地址>:5244；" +
					"初始管理员口令由 Alist 在**首次启动时**随机生成并写进它自己的日志，" +
					"面板会把这一条从日志里抓出来放进安装结果（抓不到时会说明）。",
			},
			Summary: "把网盘、对象存储、本地目录挂成一个网页文件站",
			Description: "支持 40+ 存储后端（阿里云盘、OneDrive、Google Drive、S3、WebDAV、" +
				"本地目录……），统一成一个可浏览、可分享的网页文件列表，并提供 WebDAV 接口。" +
				"**走原生（不走 Docker）**：官方 alist-darwin-arm64.tar.gz 解压到 ~/alist，" +
				"由系统级 launchd 托管。" +
				"⚠️ 上游只发布 md5 校验清单（没有 sha256），所以这一步只能做架构复核 + md5 互证，" +
				"面板不假装自己校验过 sha256。",
			Category: "tool", Kind: KindNative,
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
			Summary: "轻量博客程序，一键装好并配好伪静态",
			Description: "轻量博客程序（PHP + MySQL）。面板会自动下载官方最新版、" +
				"解压到 ~/www/<域名>、建库建用户、写 config.inc.php 并套用 Typecho 伪静态。" +
				"完成后到 http://<域名>/install.php 走完最后一步（数据库信息已预填）。",
			Category: "site", Kind: KindNative, Port: 0,
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
			ID: "wordpress", Name: "WordPress", Icon: "🌐",
			Summary: "最流行的建站程序，一键装好并配好伪静态",
			Description: "最流行的建站程序（PHP + MySQL）。面板会自动下载官方中文版、" +
				"解压到 ~/www/<域名>、建库建用户、生成 wp-config.php 并套用 WordPress 伪静态。" +
				"完成后到 http://<域名>/wp-admin/install.php 填站点标题与管理员账号即可。",
			Category: "site", Kind: KindNative, Port: 0,
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
			Summary: "给 Emby / Jellyfin 刮削影片元数据",
			Description: "MetaTube 的 API 服务端：聚合 20+ 元数据提供方，供 Emby / Jellyfin 的 " +
				"MetaTube 插件调用，按番号抓取封面、简介、演员等信息。" +
				"装好后把插件里的服务地址填成 http://<本机地址>:8084。",
			Category: "tool", Kind: KindCompose, Port: 8084,
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
			Summary: "浏览器里的图片压缩，本地 wasm 完成",
			Description: "PNG / JPEG / WebP / AVIF 压缩与尺寸调整，编解码全在浏览器里用 " +
				"wasm 完成，图片不上传。⚠️ 上游没有官方镜像，这里用社区镜像 " +
				"pjmeca/squoosh:1.1.0（固定版本），只托管静态文件、无服务端逻辑；端口 8085。",
			Category: "tool", Kind: KindCompose, Port: 8085,
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
		pf.PortNote = "纳管已有服务，无需检查端口占用"
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

// brewHas 判断某个 Homebrew 包是否已安装。
//
// homebrew 拒绝以 root 运行，而面板以 root 运行，
// 因此必须降权到真实用户执行（与安装脚本里的处理一致）。
// InstalledFormulas 一次性返回已安装的 formula 集合。
//
// 为什么要"一次性"：`brew list --versions <单个>` 和
// `brew list --versions`（全部）耗时几乎一样（实测 0.5s vs 0.58s）——
// 因为开销主要在 brew 自身启动，不在查询。所以逐个查 N 次
// 等于白付 N 次启动成本；市场列表有十几个条目，累加就是几秒。
func (m *Manager) InstalledFormulas(ctx context.Context) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	set := map[string]bool{}
	var cmd *exec.Cmd
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "-u", m.opt.UserName,
			m.opt.BrewBin, "list", "--versions")
	} else {
		cmd = exec.CommandContext(ctx, m.opt.BrewBin, "list", "--versions")
	}
	out, err := cmd.Output()
	if err != nil {
		return set
	}
	// 输出形如：nginx 1.31.5 / php@8.3 8.3.33
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 {
			set[f[0]] = true
		}
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
		if !m.launchServicePresent(ctx, a.ServiceLabel) {
			continue
		}
		icon := a.Icon
		if icon == "" {
			icon = "🧩"
		}
		if err := m.RegisterInstalledService(ctx, a.ServiceLabel, a.Name, icon, a.Category, a.Port); err == nil {
			n++
		}
	}
	return n
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

package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 「官方 release 原生二进制」安装器：服务 frpc / orbien-client / ddns-go / alist / filebrowser 五个条目。
// 不变量：官方地址永远第一，慢或失败才退第三方镜像；有官方 sha256 清单就必须核对；解压后用
// /usr/bin/file 复核确实是 arm64（绝不放行 amd64、绝不走 Rosetta）；注册为系统级 LaunchDaemon。
// 实测官方 release 直连约 46KB/s、镜像约 640KB/s；ddns-go 的 brew formula 没有 service 块（给不出 launchd）。
// Lucky / Orbien 服务端 / frps 已于 2026-09-16 按要求彻底移除，不要再加回来。

// releaseBinaryApp 描述一个用官方 GitHub release 的 darwin-arm64 产物安装的应用。
type releaseBinaryApp struct {
	ID       string
	Label    string
	Name     string
	Icon     string
	Category string
	// RootDir 是安装根目录（相对真实用户家目录）：二进制、配置、日志都放这里
	RootDir string
	// Repo / Tag / Asset 精确定位官方 release 产物。
	// Tag 与 Asset 都写死：跟着 latest 漂会在上游改名/换架构时静默装错东西。
	Repo  string
	Tag   string
	Asset string
	// Binary 是解压后的可执行文件名
	Binary string
	// TarStrip / PickBinary：tarball 含多个二进制时只解压需要的那一个 —— TarStrip 剥顶层目录，
	// PickBinary 只取 Binary 成员（frp 包里还有 frps 与上游示例配置，全解压会污染安装目录）。
	TarStrip   int
	PickBinary bool
	// Args 是启动参数，{root} 会被替换成安装根目录
	Args []string
	// Port 用于端口存活判断（比 launchd 状态更可信）
	Port int
	// UIPort 是网页界面端口，**只在不等于 Port 时**填：健康检查、服务记录端口与「打开」入口都用它。
	UIPort int
	// HealthPath 交给面板做 HTTP 健康检查（空表示只按端口判断）
	HealthPath string
	// BindAddress 是应用**自己真正监听**的地址（"127.0.0.1" / "0.0.0.0"）：描述符据此决定
	// 广告哪个地址，猜错就是"点了必然打不开的按钮"。空 = 127.0.0.1。
	BindAddress string
	// ConfigFile 是安装目录里的配置文件名（空 = 这个应用没有独立配置文件）。
	// 面板服务详情里的「📝 编辑配置文件」按它定位（见 catalog.App.ConfigPath）。
	ConfigFile string
	// ConfigSeed 是首次安装时写入配置文件的模板；{token} / {user} / {password}
	// 会被替换成随机值（见 ensureReleaseConfig）。空表示由应用专属逻辑生成。
	ConfigSeed string
	// PreserveExistingConfig：配置由**应用自己维护**（ddns-go 保存时整体重写 YAML，面板 marker
	// 活不过第一次保存）。默认规则"有 marker 就保留"对它失效，重装会把用户填好的 DNS 密钥与
	// 域名直接冲掉，所以这类应用改成"文件存在就一律保留"（判据见 ensureReleaseConfig）。
	PreserveExistingConfig bool
	// ChecksumAsset 是上游提供的 SHA-256 清单文件名（空 = 上游不提供，只能靠 file(1)
	// 架构复核，README 里已如实说明）。
	ChecksumAsset string
	// CheckPortConflict：安装前**主动**查端口占用。就绪判定是"端口在监听"，被**别人**占着
	// 会让 assertReady 把"别人的端口在听"当成"我们装好了"（谎报成功），所以宁可提前如实失败
	// 并点名占用者。已被本应用自己的服务记录占用不算冲突（前面的幂等门禁已处理）。
	CheckPortConflict bool
	// Notes 是安装结果里要额外告诉用户的话
	Notes []string
	// MirrorOnly 表示这个包**只在镜像站上有**（没有 GitHub 官方地址，也没有第三方加速源）。
	//
	// 为什么需要它：自研产物没有上游，`releaseURL()` 对它没有意义
	// （Repo/Tag 留空会拼出一个不存在的 github.com//releases/... 地址）。
	// 声明它之后下载候选只保留镜像站地址 —— 而不是"先试一个假地址再回落"。
	//
	// ⚠️ 这类应用**不走** releaseBinaryApps 的通用安装/卸载流程（那套是"系统级
	// LaunchDaemon + 家目录安装"），由它自己的安装器负责；登记在这里是因为市场
	// 门禁要求 release_binary 下载点在 ReleaseBinaryAssets() 里有一份可核对的事实。
	MirrorOnly bool
}

// webPort 返回网页界面/健康检查应当使用的端口（UIPort 为 0 时等于 Port）。
func (a releaseBinaryApp) webPort() int {
	if a.UIPort > 0 {
		return a.UIPort
	}
	return a.Port
}

const (
	frpcLabel         = "com.zizdog.frpc"
	orbienClientLabel = "com.zizdog.orbien-client"
	// releaseBinaryReadyTimeout 是「等这个服务真正起来」的上限。
	// 超时即**如实失败**（见 waitReleaseBinaryReady）。
	releaseBinaryReadyTimeout = 60 * time.Second
)

// gitHubReleaseMirrors 是 GitHub release 的加速前缀（拼在完整官方 URL 前面）。
// 顺序即优先级：官方永远第一，仅在失败或被 --max-time 掐断时退镜像（实测 2026-09：
// 官方约 46KB/s、镜像约 640KB/s）—— 不能反过来把第三方放在前面。
var gitHubReleaseMirrors = []string{
	"https://ghfast.top/",
	"https://gh-proxy.com/",
}

// orderBySpeed 按实测速度把候选地址从快到慢排序（速度 <=0 的排最后，保持原相对顺序）。
// 为什么要先探测：无代理时 GitHub Release 可能完全不通（实测 20 秒 0 字节），
// 官方排第一 + `--max-time 150` 会让每个应用先白等 150 秒。
func orderBySpeed(urls []string, speeds []int64) []string {
	type item struct {
		url   string
		speed int64
		idx   int
	}
	items := make([]item, 0, len(urls))
	for i, u := range urls {
		sp := int64(0)
		if i < len(speeds) {
			sp = speeds[i]
		}
		items = append(items, item{url: u, speed: sp, idx: i})
	}
	sort.SliceStable(items, func(a, b int) bool {
		// 有速度的都排在没速度的前面；同速按原始顺序（官方优先）
		if (items[a].speed > 0) != (items[b].speed > 0) {
			return items[a].speed > 0
		}
		if items[a].speed != items[b].speed {
			return items[a].speed > items[b].speed
		}
		return items[a].idx < items[b].idx
	})
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.url)
	}
	return out
}

// releaseBinaryApps 是被本安装器服务的应用表。
// 判定顺序（README「应用市场」第 2 条）：官方 release 有 darwin_arm64 产物、且没有 brew formula。
// 只放通用参数，不为单个应用发明专属流程。
var releaseBinaryApps = map[string]releaseBinaryApp{
	// 2026-09-16 按要求彻底移除 Lucky / Orbien 服务端 / frps，不要再加回来。
	// 加回条目时注意：市场安装入口按 IsReleaseBinaryApp(id) 分流，releaseBinaryApps 与
	// catalog.go 必须同时有，否则会出现"条目是 compose、实际却走 release 安装器"的隐蔽错配。
	"frpc": {
		ID: "frpc", Label: frpcLabel, Name: "frp 客户端 (frpc)", Icon: "🧷",
		Category: "tool", RootDir: "frpc",
		// 与 frps 同一个上游 tarball（内含 frps/frpc），只挑 frpc 那一个成员。
		Repo: "fatedier/frp", Tag: "v0.71.0", Asset: "frp_0.71.0_darwin_arm64.tar.gz",
		Binary:   "frpc",
		TarStrip: 1, PickBinary: true,
		Args: []string{"-c", "{root}/frpc.toml"},
		// frpc 是主动往外连的客户端，唯一监听的端口是它自己的 admin UI(7400)。
		Port: 7400, HealthPath: "/",
		ConfigFile:    "frpc.toml",
		ConfigSeed:    frpcConfigSeed,
		ChecksumAsset: frpChecksumAsset,
		Notes: []string{
			"frpc.toml 已生成：admin UI 在 7400。",
			"⚠️ serverAddr / serverPort / auth.token 都要改成你自己 frps 的值 —— " +
				"面板不再提供 frps，生成的 token 是随机的，不改连不上。",
			"[[proxies]] 里现在只有一条注释示例 —— 不加隧道的话它连上了也没有任何转发。",
		},
	},
	"orbien-client": {
		ID: "orbien-client", Label: orbienClientLabel, Name: "Orbien 客户端 (orbien)", Icon: "🛰️",
		Category: "tool", RootDir: "orbien-client",
		// arm64 证据：v3.6.0 资产有 orbien_3.6.0_darwin_arm64.tar.gz（2,104,350 B）；实测解压后
		// file 报 Mach-O arm64、--help 输出 "orbien client"；上游**没有** checksums，只能靠 file(1)。
		Repo: "orbien-org/orbien", Tag: "v3.6.0", Asset: "orbien_3.6.0_darwin_arm64.tar.gz",
		Binary: "orbien",
		// tarball 里只有 orbien 一个二进制（外加上游示例 orbien.toml）；示例配置会被 marker 覆盖。
		Args: []string{"-c", "{root}/orbien.toml"},
		// 客户端不监听端口（纯出站连接），没有端口/健康检查；是否在跑只看 launchd。
		Port:       0,
		ConfigFile: "orbien.toml",
		ConfigSeed: orbienClientConfigSeed,
		Notes: []string{
			"orbien.toml 已生成：server=127.0.0.1:9527（本机服务端）。",
			"[[tunnels]] 里现在只有一条注释示例 —— 不加隧道它连上了也没有转发。",
			"隧道连接状态与流量在服务端的 Dashboard（http://<服务端地址>:8020）里看。",
		},
	},
	"ddns-go": {
		ID: "ddns-go", Label: "com.zizdog.ddns-go", Name: "DDNS-Go（动态域名解析）", Icon: "🌐",
		Category: "tool", RootDir: "ddns-go",
		// arm64 证据：v6.17.7 资产 ddns-go_6.17.7_darwin_arm64.tar.gz（4,386,545 B，sha256 9dac9d82…）；
		// 2026-09-16 实下核对：tarball 内 4 个成员平级，file(1) 报 Mach-O 64-bit executable arm64。
		Repo: "jeessy2/ddns-go", Tag: "v6.17.7", Asset: "ddns-go_6.17.7_darwin_arm64.tar.gz",
		Binary: "ddns-go",
		// tarball **没有**顶层目录（4 个成员平级）→ TarStrip=0；PickBinary 只解压 ddns-go 那一个。
		TarStrip: 0, PickBinary: true,
		// -c 写死到安装目录：上游默认 $HOME/.ddns_go_config.yaml 会把配置散在家目录根下，
		// 「📝 编辑配置文件」也定位不到；-l :9876 写出来才不会因上游改默认而静默失联。
		Args: []string{"-c", "{root}/ddns-go.yaml", "-l", ":9876"},
		// 9876 是它唯一的监听端口；HealthPath "/"：实测未登录 GET / 返回 307 → /login，
		// 面板健康判定把 2xx/3xx 都算健康（见 health.go），能稳定反映"服务活着"。
		Port: 9876, HealthPath: "/",
		ConfigFile: "ddns-go.yaml",
		ConfigSeed: ddnsGoConfigSeed,
		// 网页界面保存会整体重写 YAML（marker 被抹掉）→ 判据必须是"文件存在就保留"，
		// 否则重装会冲掉用户的 DNS 密钥（见字段注释）。
		PreserveExistingConfig: true,
		// 上游有 checksums.txt 且含这份产物 —— 有清单就必须核对（回落第三方镜像时它是唯一内容校验）。
		ChecksumAsset: "checksums.txt",
		Notes: []string{
			"ddns-go.yaml 已生成：现在只是一份带说明的骨架，首次配置在它自己的网页界面里做。",
			"① 首次：打开 http://<本机地址>:9876，先设 ddns-go 的用户名口令，" +
				"再到「DNS服务商」里添加服务商与要更新的域名。",
			"② 之后日常：在「服务管理 → DDNS-Go」点「📝 编辑配置文件」改 ddns-go.yaml，" +
				"保存后点「🔄 重启服务」——不必再开网页。",
		},
	},
	"alist": {
		ID: "alist", Label: "com.zizdog.alist", Name: "Alist（文件列表）", Icon: "📂",
		Category: "tool", RootDir: "alist",
		// 走 release 而非 brew：alist **从未进过 homebrew-core**（2026-09-17 实测：brew info 无
		// formula、API 404、提交历史空数组；唯一三方 tap 2024-01 已死）。arm64 证据：v3.64.0 的
		// alist-darwin-arm64.tar.gz（43,021,495 B），tar 内平级单成员、file(1) 报 Mach-O arm64。
		Repo: "AlistGo/alist", Tag: "v3.64.0", Asset: "alist-darwin-arm64.tar.gz",
		Binary: "alist",
		// tarball 只有 alist 一个平级成员 → TarStrip=0；PickBinary 开着，以后加了 README 也不会摊进来。
		TarStrip: 0, PickBinary: true,
		// --data 写死到安装目录下 data/：不写会在**当前工作目录**下建 data/
		// （launchd 的 WorkingDirectory 不是我们想要的），「📝 编辑配置文件」也会定位不到。
		Args: []string{"server", "--data", "{root}/data"},
		Port: 5244, HealthPath: "/",
		// Alist 默认监听 0.0.0.0:5244（实测日志 "start HTTP server @ 0.0.0.0:5244"），
		// 广告地址必须是 LAN 地址，否则用户看到 127.0.0.1:5244 会以为只能本机用。
		BindAddress: "0.0.0.0",
		// Alist 首次启动自己创建 data/config.json（并写入 jwt_secret 等）；面板**不**写它，
		// 只登记位置好让「📝 编辑配置文件」能定位（ConfigSeed 为空）。
		ConfigFile: "data/config.json",
		// 上游**只有 md5.txt、没有 sha256 清单** → ChecksumAsset 留空：这条轨此时如实说明
		// "只做了架构复核 + md5 互证"，而不是假装校验过 sha256（见 tarballDescriptor else 分支）。
		Notes: []string{
			"安装目录：~/alist（二进制、data/、日志都在这里）。",
			"初始管理员口令由 Alist 首次启动时随机生成 —— 面板已从启动日志里抓出来放在上面的凭据区块里；" +
				"忘了口令就在终端执行 ~/alist/alist admin set <新口令>。",
			"⚠️ Alist 上游只发布 md5 校验清单，没有 sha256：这一步的内容校验是" +
				"「架构复核（file -b）+ 与上游 md5 互证」，强度弱于有 sha256 清单的应用（frpc / ddns-go）。",
		},
	},
	"filebrowser": {
		ID: "filebrowser", Label: "com.zizdog.filebrowser", Name: "File Browser（网页文件管理）", Icon: "🗂️",
		Category: "tool", RootDir: "filebrowser",
		// 走 release 而非 brew：homebrew/core 的 filebrowser **没有 service 定义**（与 ddns-go 同因），
		// 通用 brew 路径会留下"装了但永远起不来"的假服务记录。arm64 证据：v2.63.23 的
		// darwin-arm64-filebrowser.tar.gz（15,258,752 B）；2026-09-19 本机实测 file(1) 报 Mach-O arm64。
		Repo: "filebrowser/filebrowser", Tag: "v2.63.23", Asset: "darwin-arm64-filebrowser.tar.gz",
		Binary: "filebrowser",
		// tarball 里 4 个**平级**成员（无顶层目录）→ TarStrip=0；PickBinary 只取 filebrowser 那一个。
		TarStrip: 0, PickBinary: true,
		// -b /filebrowser 与 AppUI.SelfBase 一致；-a 127.0.0.1 **只绑回环**（家目录有 .ssh/
		// 凭据，暴露面必须收住）；-r 指**真实用户家目录**（用户 2026-09-19 要求管理整个目录）；
		// -d 指面板工作目录 {vardir}（数据库放家目录会被自己列出来、也更容易被误改）。
		Args: []string{"-b", "/filebrowser", "-a", "127.0.0.1", "-p", "8081",
			"-r", "{home}", "-d", "{vardir}/filebrowser/filebrowser.db"},
		Port: 8081, HealthPath: "/health",
		// 只绑回环：广告地址也用 127.0.0.1（见 releaseBinaryApp.BindAddress 的注释）。
		BindAddress: "127.0.0.1",
		// 端口语义强（就绪判定就看它）→ 安装前主动查占用，避免把别人的端口当成自己的。
		CheckPortConflict: true,
		// 上游 release **有** sha256 清单 → 必须核对（回落到第三方加速镜像时它是唯一内容校验）。
		ChecksumAsset: "filebrowser_2.63.23_checksums.txt",
		Notes: []string{
			"初始管理员凭据（用户名 + 随机口令）由 File Browser 首次启动时生成 —— 面板已从启动日志里抓出来" +
				"放进上面的凭据区块；日志里抓不到时会如实说明，绝不编造。",
			"⚠️ 上游项目已于 2026-09-01 归档：之后不再发版、不再修安全问题，请只在内网使用。",
		},
	},
	// memos：官方 darwin-arm64 单二进制 + SQLite，装完即用；**端口必须显式 5230**
	// （代码默认 8081，与 filebrowser 撞）。校验清单 checksums.txt 覆盖全部平台产物。
	"memos": {
		ID: "memos", Label: "com.zizdog.memos", Name: "Memos（笔记）", Icon: "📝",
		Category: "tool", RootDir: "memos",
		// arm64 证据：v0.31.0 的 memos_0.31.0_darwin_arm64.tar.gz（21,058,705 B，
		// sha256 96b40160…）；2026-09-20 本机实下解压后 file(1) 报 Mach-O arm64、
		// `memos --help` 正常输出。
		Repo: "usememos/memos", Tag: "v0.31.0", Asset: "memos_0.31.0_darwin_arm64.tar.gz",
		Binary:   "memos",
		TarStrip: 0, PickBinary: true,
		// --data 写死到安装目录下 data/：不写会按上游默认散到当前工作目录。
		Args: []string{"--port", "5230", "--data", "{root}/data"},
		Port: 5230, HealthPath: "/healthz",
		// 实测监听 *:5230（不是只绑回环），广告 LAN 地址。
		BindAddress: "0.0.0.0",
		// 数据库由 memos 首次启动自己建（memos_prod.db），面板只登记位置。
		ConfigFile:    "data/memos_prod.db",
		ChecksumAsset: "checksums.txt",
		Notes: []string{
			"安装目录：~/memos（二进制、data/memos_prod.db、日志都在这里）。",
			"首次打开 http://<本机地址>:5230 注册的**第一个**账号就是实例管理员（没有默认口令）；" +
				"在此之后注册的都是普通用户。",
		},
	},
	// navidrome：官方 darwin-arm64 单二进制 + SQLite；**端口 4533 且只绑 127.0.0.1**
	// （上游默认 address=0.0.0.0，绝不许裸奔）。音乐库目录写进 navidrome.toml。
	"navidrome": {
		ID: "navidrome", Label: "com.zizdog.navidrome", Name: "Navidrome（音乐）", Icon: "🎵",
		Category: "tool", RootDir: "navidrome",
		// arm64 证据：v0.64.0 的 navidrome_0.64.0_darwin_arm64.tar.gz（23,263,336 B，
		// sha256 ad35e6f0…）；2026-09-20 本机实下解压后 file(1) 报 Mach-O arm64、
		// `navidrome --help` 正常输出。
		Repo: "navidrome/navidrome", Tag: "v0.64.0", Asset: "navidrome_0.64.0_darwin_arm64.tar.gz",
		Binary:   "navidrome",
		TarStrip: 0, PickBinary: true,
		Args: []string{"--address", "127.0.0.1", "--port", "4533",
			"--datafolder", "{root}/data", "--configfile", "{root}/navidrome.toml"},
		Port: 4533, HealthPath: "/ping",
		// 只绑回环：广告地址也用 127.0.0.1（上游默认 0.0.0.0，必须显式收住）。
		BindAddress: "127.0.0.1",
		// 配置文件由安装器生成（含 MusicFolder 骨架）；面板保留用户改动。
		ConfigFile:    "navidrome.toml",
		ConfigSeed:    navidromeConfigSeed,
		ChecksumAsset: "navidrome_checksums.txt",
		Notes: []string{
			"安装目录：~/navidrome（二进制、data/navidrome.db、navidrome.toml、日志）。",
			"**音乐库目录默认留空**：在「📝 编辑配置文件」里把 MusicFolder 改成你的音乐目录，" +
				"再点「🔄 重启服务」；不设置时 Navidrome 照常起来，但扫描会报 'no such file'、一首歌都没有。",
			"首次打开 http://127.0.0.1:4533 自行创建管理员账号（无默认口令）；" +
				"它只绑回环，手机在局域网里直连 4533 是打不开的（走面板入口或反代）。",
		},
	},
}

// panelConfigMarker 是"面板生成配置"的标记（重装时据此决定保留还是覆盖）。
// 模板占位符：{token} 随机 32 位 hex、{user} 随机用户名（模板里不出现则 admin）、
// {password} 随机 16 位 hex（替换逻辑见 generateConfigSecrets / expandConfigSeed）。
const (
	panelConfigMarker = "由 ZizPanel 生成"
)

// frpcConfigSeed 是 frpc.toml 模板；面板**不再提供 frps 服务端**（2026-09-16 按要求移除），
// serverAddr/serverPort 是占位值，请改成你自己的 frps 地址与端口。
const frpcConfigSeed = `# ` + panelConfigMarker + `。改完在「服务管理 → frp 客户端」里重启服务生效，
# 也可以直接在服务详情里点「📝 编辑配置文件」修改本文件。
#
# ⚠️ 两个必须改的地方：
#   ① serverAddr / serverPort —— 指向你自己的 frps 服务端（面板不再提供 frps）。
#   ② auth.token —— 必须与 frps 的 auth.token 完全一致，否则 frps 会拒绝登录。
#      下面这个是面板随机生成的，**不是**你服务端的 token，请务必替换。
serverAddr = "127.0.0.1"
serverPort = 7000

# 连不上服务端时**不要退出**，继续重试。
# frp 的默认值 loginFailExit = true：第一次登录失败就整个进程退出。
# 这在面板托管的场景下是个陷阱 —— 用户先装 frpc、后装 frps（或 frps 暂时没起）时，
# frpc 会立刻死掉，launchd 又 KeepAlive 反复拉起，日志变成刷屏，
# 界面上的表现是"装了但 7400 打不开"。设成 false 后它会安静地重连。
loginFailExit = false

# 必须与 frps 的 auth.token 一致，否则 frps 会拒绝登录。
# 面板不提供 frps，所以这里生成的随机值需要你改成服务端的 token。
auth.method = "token"
auth.token = "{token}"

# frpc 自己的 admin UI（面板服务详情里的「打开」指向 http://<地址>:7400）
webServer.addr = "0.0.0.0"
webServer.port = 7400
webServer.user = "{user}"
webServer.password = "{password}"

# 一条注释掉的示例：把本机的 8880 暴露到 frps 的 6000 端口。
# 原生安装，localIP 直接写 127.0.0.1 即可（它就在这台 Mac 上）。
# [[proxies]]
# name = "example"
# type = "tcp"
# localIP = "127.0.0.1"
# localPort = 8880
# remotePort = 6000
`

// orbienClientConfigSeed 是 orbien.toml 模板；面板**不再提供 Orbien 服务端**
// （2026-09-16 按要求移除），server 是占位值：请改成你自己的服务端地址。客户端没有 Web 界面。
const orbienClientConfigSeed = `# ` + panelConfigMarker + `。改完在「服务管理 → Orbien 客户端」里重启服务生效，
# 也可以直接在服务详情里点「📝 编辑配置文件」修改本文件。
# ⚠️ Orbien 服务端地址：面板不再提供服务端，请改成你自己的 <IP>:9527。
server = "127.0.0.1:9527"

# 服务端启用了 auth.token 才需要填，并且必须与服务端一致。
# auth.token = ""

# 一条注释掉的示例：把本机的 8880 暴露到服务端的 9000 端口。
# 原生安装，service 直接写 127.0.0.1 即可（它就在这台 Mac 上）。
# [[tunnels]]
# name = "example"
# protocol = "tcp"
# service = "127.0.0.1:8880"
# remotePort = 9000
`

// ddnsGoConfigSeed 是 ddns-go 的配置骨架。**只有注释**的 YAML 合法（yaml.Unmarshal 得到全零
// Config，ddns-go 照常起来，实测正常监听 :9876、GET / 307 跳 /login），首次保存会覆写成完整配置。
// 文件必须先存在，否则「📝 编辑配置文件」点开就是一个"读文件失败"。
const ddnsGoConfigSeed = `# ` + panelConfigMarker + `（ddns-go 配置骨架）。改完在
# 「服务管理 → DDNS-Go」里点「🔄 重启服务」生效，也可以直接点「📝 编辑配置文件」修改本文件。
#
# ── 首次配置（只需做一次）──────────────────────────────────────────────
# 打开 http://<本机地址>:9876 ，在 ddns-go 自己的网页界面里：
#   ① 先设置 ddns-go 的登录用户名与口令（这是它自己的登录，不是 ZizPanel 的）；
#   ② 到「DNS服务商」添加服务商：Cloudflare / 阿里云 / 腾讯云 / DNSPod /
#      华为云 / 百度云 等，填 AccessKey / API Token；
#   ③ 填要更新的域名（如 home.example.com）。
# 保存后 ddns-go 会把**完整配置**写回本文件（覆盖现在这份骨架），
# 之后日常改配置就直接改这个文件，不必再打开网页。
#
# ── 说明 ───────────────────────────────────────────────────────────────
# ddns-go 每 300 秒检查一次公网 IP（启动参数 -f 可改），只有 IP 变化时才调 DNS 接口。
# 想改监听端口或检查频率：改的是启动参数，不是这个文件 —— 那要改 launchd 的
# plist（/Library/LaunchDaemons/com.zizdog.ddns-go.plist）。
`

// navidromeConfigSeed 是 Navidrome 的配置骨架。**只有注释**是合法 TOML（实测
// 照常起来，默认 MusicFolder=music → 扫描报 no such file）。用户把 MusicFolder
// 改成自己的音乐目录后点「🔄 重启服务」生效。
//
// ⚠️ 真机实测：改 MusicFolder **只在首次扫描前有效** —— 库路径一旦写进
// navidrome.db 就不会再被 toml 改掉。换库要删 data/navidrome.db 再重启（会丢播放记录）。
const navidromeConfigSeed = `# ` + panelConfigMarker + `（Navidrome 配置骨架）。改完在
# 「服务管理 → Navidrome」里点「🔄 重启服务」生效。
#
# ── 音乐库（必填，否则一首歌都没有）────────────────────────────────────
# MusicFolder = "/Users/你的用户名/Music"
#
# 数据目录（面板已用启动参数固定为 ~/navidrome/data，改这里没用）
# DataFolder = "/Users/你的用户名/navidrome/data"
#
# 只绑本机回环（面板启动参数已固定；要局域网访问请用面板入口或反代）
# Address = "127.0.0.1"
# Port = 4533
#
# ⚠️ 换音乐库目录：MusicFolder 只在 data/navidrome.db 里还没有库记录时生效。
# 已经启动过一次之后再改目录，必须先删掉 ~/navidrome/data/navidrome.db 并重启
# （会丢播放记录/收藏），或者在 Web UI 的「设置 → 媒体库」里改。
`

// frp 的官方校验清单文件名（frpc 安装时用它核对下载产物）。
const frpChecksumAsset = "frp_sha256_checksums.txt"

// releaseURL 是官方下载地址。
func (a releaseBinaryApp) releaseURL() string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", a.Repo, a.Tag, a.Asset)
}

// downloadURLs 返回按优先级排列的下载地址：官方第一，其余为加速镜像。
func (a releaseBinaryApp) downloadURLs() []string {
	// MirrorOnly：只在镜像站上有（自研产物）。返回空列表 = "没有公网候选"，
	// 调用方必须据此如实失败，而不是去试一个拼出来的假 GitHub 地址。
	if a.MirrorOnly {
		return nil
	}
	official := a.releaseURL()
	urls := []string{official}
	for _, m := range gitHubReleaseMirrors {
		urls = append(urls, m+official)
	}
	return urls
}

// binaryReleasePaths 是这套安装器用到的全部路径，**从描述符推导**（见 binaryReleasePathsFor）——
// 两份路径计算漂移正是"编辑配置文件按钮指向一个不存在的文件"这类问题的根因。
type binaryReleasePaths struct {
	Root   string
	Binary string
	Asset  string
	Config string // 配置文件（应用没有独立配置时留空）
	OutLog string
	ErrLog string
	Plist  string
}

// binaryReleasePathsFor 按描述符算出全部路径。
func (m *Manager) binaryReleasePathsFor(d AppDescriptor) binaryReleasePaths {
	root := descriptorRoot(m.opt.UserHome, d)
	p := binaryReleasePaths{
		Root:   root,
		Binary: filepath.Join(root, d.Paths.Binary),
		Plist:  d.Service.PlistPath,
		OutLog: filepath.Join(root, d.Paths.OutLog),
		ErrLog: filepath.Join(root, d.Paths.ErrLog),
	}
	if len(d.Artifacts) > 0 {
		p.Asset = filepath.Join(root, d.Artifacts[0].Name)
	}
	if d.Paths.ConfigFile != "" {
		p.Config = filepath.Join(root, d.Paths.ConfigFile)
	}
	return p
}

// binaryReleasePaths（老签名，按参数表查）内部改用描述符；描述符没注册时退回按参数表算 ——
// 宁可给出一条正确路径，也不要在这里 panic 掉整个面板。
func (m *Manager) binaryReleasePaths(a releaseBinaryApp) binaryReleasePaths {
	if d, ok := FindDescriptor(a.ID); ok {
		return m.binaryReleasePathsFor(d)
	}
	root := filepath.Join(m.opt.UserHome, a.RootDir)
	p := binaryReleasePaths{
		Root:   root,
		Binary: filepath.Join(root, a.Binary),
		Asset:  filepath.Join(root, a.Asset),
		OutLog: filepath.Join(root, "launchd.out.log"),
		ErrLog: filepath.Join(root, "launchd.err.log"),
		Plist:  "/Library/LaunchDaemons/" + a.Label + ".plist",
	}
	if a.ConfigFile != "" {
		p.Config = filepath.Join(root, a.ConfigFile)
	}
	return p
}

// descriptorRoot 解析安装根目录（相对真实用户家目录）。
func descriptorRoot(userHome string, d AppDescriptor) string {
	root := d.Paths.RootDir
	if strings.HasPrefix(root, "/") {
		return root
	}
	return filepath.Join(userHome, root)
}

// InstallReleaseBinary 部署一个"官方 release 原生二进制"应用并注册为系统级服务。
// 2026-09-17 模块化后它只是一层兼容壳：参数表与 steps DSL 都在描述符里，这里做前置校验 →
// 装配执行器 → 执行步骤；对外契约（202 + task_id + SSE + 进度）一个字都不能变。
func (m *Manager) InstallReleaseBinary(ctx context.Context, id string, result *InstallResult) error {
	d, ok := FindDescriptor(id)
	if !ok || d.Rail != RailTarball {
		return fmt.Errorf("没有 %s 的原生二进制安装器", id)
	}
	// 幂等门禁（与 brew / compose 同一份判定）：装过的再点安装必须直接短路成"已装跳过"，
	// **不执行任何安装命令** —— 否则会重跑下载/解包并重启用户正在用的隧道服务（真机 frpc/ddns-go 复现）。
	// 位置刻意在 root/用户校验**之前**；判定只看真实注册证据（记录/plist），不看磁盘产物。
	if app, found := FindApp(id); found {
		if res, done := m.installedSkipResult(ctx, app); done {
			// "装过"不等于"现在是好的"：tarball 轨的登记发生在验收**之前**，失败的安装同样会
			// 留下记录与 plist（2026-09-17 真机 Alist：端口从未监听，市场却永远显示已安装）。所以
			// 补一条**真实存活**判据：端口真的在监听才跳过；无端口可查的应用保持原判据不变。
			if app.Port <= 0 || m.portHasListener(app.Port) {
				adoptInstallResult(result, res)
				return nil
			}
			if result != nil {
				result.Steps = append(result.Steps,
					fmt.Sprintf("检测到「%s」已登记，但端口 %d 并没有在监听 —— 本次**不跳过**，"+
						"继续按描述符重新安装（下载会命中已缓存的文件，服务会先 bootout 再注册）",
						app.Name, app.Port))
			}
		}
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("部署 %s 需要以 root 运行", d.Name)
	}
	if m.opt.UserName == "" || m.opt.UserHome == "" {
		return fmt.Errorf("无法确定运行该服务的真实用户与家目录")
	}
	// 端口占用检查（只对显式声明 CheckPortConflict 的应用）：被**别人**占着时提前失败，
	// 而不是让 assertReady 把"别人的端口在听"当成"我们装好了"（最危险的一种谎报成功）。
	if spec, ok := releaseBinaryApps[d.ID]; ok && spec.CheckPortConflict && spec.Port > 0 {
		if info, err := m.checkPort(spec.Port); err == nil && info.InUse {
			var self []string
			if app, found := FindApp(d.ID); found {
				self = m.selfPortOccupiers(ctx, app)
			}
			if len(self) == 0 {
				return fmt.Errorf("端口 %d 已被其它进程占用（%s）——「%s」起不来，"+
					"本次安装已中止，不会留下「装好了但打不开」的假象。"+
					"请先停掉占用它的服务（如果那是旧 Docker 版的 File Browser，"+
					"先在 Docker/Compose 里 docker compose down）再重试",
					spec.Port, strings.Join(info.Holders, ", "), spec.Name)
			}
		}
	}
	// filebrowser 的数据库放面板工作目录（家目录之外）：先建目录并把归属交给真实用户
	// （服务以该用户身份运行，要能写 Bolt 库）。
	if d.ID == "filebrowser" {
		if err := m.prepareFilebrowserDataDir(); err != nil {
			return err
		}
	}
	if err := m.OrchestrateTarballInstall(ctx, d, result); err != nil {
		return err
	}
	// Alist 的初始口令由**它自己**首次启动时随机生成并打进日志（上游没有"由外部指定初始口令"
	// 的参数，`alist admin set` 要把口令放进 argv，本项目有过 argv 泄漏真凭据的事故，不采用）；
	// 只能在安装完成后从日志里抓，抓不到就如实说明、绝不编造口令。
	if d.ID == "alist" {
		m.appendAlistInitialPassword(d, result)
	}
	// File Browser 同理：首次 quick setup 会随机生成 admin 口令并打进 stdout，
	// 从日志里读出来放进凭据区（读不到就明说，不编造）。
	if d.ID == "filebrowser" {
		m.appendFilebrowserInitialPassword(d, result)
	}
	return nil
}

// alistInitialPasswordRe 匹配 Alist 首次启动时的那行日志（v3.64.0 实测：上游是 logrus 的
// msg 字段，口令后紧跟一个引号 → 字符集严格限定 [A-Za-z0-9]，否则会把引号一起抓进来）。
var alistInitialPasswordRe = regexp.MustCompile(`initial password is:\s*([A-Za-z0-9]+)`)

// appendAlistInitialPassword 把 Alist 日志里的初始口令搬进安装结果的**凭据区**。
// 口令**只进 InstallResult.Credentials**，不进任务步骤文本：步骤会进任务日志 / SSE / 审计，
// 口令出现在那里等于多一份长期留存。这里只读日志、不写任何东西；日志里没有那行就明说"没抓到"。
func (m *Manager) appendAlistInitialPassword(d AppDescriptor, result *InstallResult) {
	if result == nil {
		return
	}
	for _, c := range result.Credentials {
		if c.Key == "alist_admin_password" {
			return
		}
	}
	p := m.binaryReleasePathsFor(d)
	password := ""
	for _, f := range []string{p.OutLog, p.ErrLog} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if mm := alistInitialPasswordRe.FindSubmatch(data); len(mm) == 2 {
			password = string(mm[1])
			break
		}
	}
	spec := releaseBinaryApps[d.ID]
	uiURL := fmt.Sprintf("http://%s:%d", m.primaryIP(), spec.webPort())
	if password == "" {
		result.Steps = append(result.Steps,
			"没有在启动日志里找到初始口令（"+p.OutLog+"）——可能是这次重装保留了已有 data/ 目录，口令还是上一次那个。",
			"如果登录不上，请在终端执行：~/alist/alist admin set <新口令>")
		return
	}
	result.Credentials = append(result.Credentials, Credential{
		Key: "alist_admin_password", Value: password,
		Label: "Alist 初始管理员口令（由 Alist 首次启动时生成，在界面里改过之后就失效）",
	})
	result.Steps = append(result.Steps,
		"Alist 首次启动时生成了一个初始管理员口令（用户名 admin），已放进本次安装的凭据区。",
		"界面地址 "+uiURL+"；登录后请立刻在「个人资料」里改口令。")
	if p.Config != "" {
		result.Steps = append(result.Steps,
			"配置文件 "+p.Config+"（服务详情里可「📝 编辑配置文件」）")
	}
}

// filebrowserInitialCredRe 匹配 File Browser 首次 quick setup 打的那行日志（实测输出：
// "User 'admin' initialized with randomly generated password: OMh0r3E_RmVzNFrx"）—— 用户名与
// 口令**都在这一行里**，两个都抓，绝不把 admin 写死当默认；口令含下划线，用 \S+ 而非只认字母数字。
var filebrowserInitialCredRe = regexp.MustCompile(
	`User '([^']+)' initialized with randomly generated password:\s*(\S+)`)

// filebrowserPasswordOnlyRe 是防御性兜底：上游若换成只打口令、不打用户名的格式也把口令
// 抓出来，但**不编用户名**。
var filebrowserPasswordOnlyRe = regexp.MustCompile(
	`(?i)(?:random(?:ly)? generated )password[:\s]+([A-Za-z0-9_\-]{6,})`)

// appendFilebrowserInitialPassword 把 File Browser 日志里的初始凭据搬进安装结果的**凭据区**。
// 上游没有"由外部指定初始口令"的参数（--password 要的是 bcrypt 哈希），抓不到就明说"没抓到、
// 去哪儿看"，绝不编造 —— 本仓库因编造凭据出过事。凭据**只进 Credentials**，不进步骤叙述。
func (m *Manager) appendFilebrowserInitialPassword(d AppDescriptor, result *InstallResult) {
	if result == nil {
		return
	}
	for _, c := range result.Credentials {
		if c.Key == "filebrowser_admin_password" {
			return
		}
	}
	p := m.binaryReleasePathsFor(d)
	var username, password string
	for _, f := range []string{p.OutLog, p.ErrLog} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if mm := filebrowserInitialCredRe.FindSubmatch(data); len(mm) == 3 {
			username, password = string(mm[1]), string(mm[2])
			break
		}
		if password == "" {
			if mm := filebrowserPasswordOnlyRe.FindSubmatch(data); len(mm) == 2 {
				password = string(mm[1]) // 只捞到口令：用户名留空，绝不写死 admin
			}
		}
	}
	uiURL := fmt.Sprintf("http://%s:%d/filebrowser/", m.primaryIP(), releaseBinaryApps[d.ID].webPort())
	if password == "" {
		result.Steps = append(result.Steps,
			"没能从启动日志里读到 File Browser 的首次凭据 —— 请到「服务管理 → File Browser → 日志」"+
				"（或终端执行 `tail -n 50 "+p.OutLog+"`）查看带 `randomly generated password` 的那一行；"+
				"确实登录不上时，可删除 ~/filebrowser/filebrowser.db 后重装以重置管理员口令。")
		return
	}
	label := "File Browser 初始管理员口令（首次启动随机生成；在界面里改过之后就失效）"
	if username == "" {
		label = "File Browser 初始管理员口令（用户名未能从日志解析出来，请对照日志那一行；改过之后就失效）"
	}
	result.Credentials = append(result.Credentials, Credential{
		Key: "filebrowser_admin_password", Value: password, Label: label,
	})
	if username != "" {
		result.Credentials = append(result.Credentials, Credential{
			Key: "filebrowser_admin_user", Value: username,
			Label: "File Browser 初始管理员用户名（首次启动随机生成/quick setup 默认用户名）",
		})
		result.Steps = append(result.Steps,
			fmt.Sprintf("File Browser 首次启动生成了初始凭据：用户名 %s + 随机口令（口令已放进本次安装的凭据区）。", username))
	} else {
		result.Steps = append(result.Steps,
			"File Browser 首次启动生成了随机口令，已放进本次安装的凭据区；"+
				"用户名没能从日志里解析出来，请对照日志里 `User '<用户名>' initialized with randomly generated password` 那一行。")
	}
	result.Steps = append(result.Steps,
		"入口 "+uiURL+"（面板子路径，需登录；登录后请立刻改口令）。"+
			"它只监听 127.0.0.1:8081，局域网/公网用 IP:8081 打不开是设计如此。")
}

// prepareFilebrowserDataDir 建好**家目录之外**的数据目录并把归属交给真实用户（服务以真实
// 用户身份运行，要能写）：File Browser 管理整个家目录，但它的 filebrowser.db 不能放家目录里。
func (m *Manager) prepareFilebrowserDataDir() error {
	if strings.TrimSpace(m.opt.WorkDir) == "" {
		return fmt.Errorf("无法确定面板工作目录，不能把 File Browser 数据库放到家目录之外")
	}
	dir := filepath.Join(m.opt.WorkDir, "filebrowser")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 File Browser 数据目录 %s 失败: %w", dir, err)
	}
	if m.opt.UserName != "" {
		if err := chownTree(m.opt.UserName, dir); err != nil {
			return fmt.Errorf("把 %s 的归属交给 %s 失败: %w", dir, m.opt.UserName, err)
		}
	}
	return nil
}

// portHasListener 判断某个端口此刻真的有进程在监听（tarball 轨的幂等跳过判据）。用端口而非
// launchd 状态：launchd "已加载"不代表进程活着（KeepAlive 会不停重启一个起不来的进程），
// 端口才是"真的在提供能力"的证据。
func (m *Manager) portHasListener(port int) bool {
	if port <= 0 {
		return false
	}
	info, err := m.checkPort(port)
	return err == nil && info.InUse
}

// adoptInstallResult 把幂等跳过的结果原样搬进调用方传入的 result：老签名（error-only + 就地
// 写 result）是 internal/web 与任务中心依赖的契约，不许改；就地写既保住签名又带上"本次跳过"。
func adoptInstallResult(dst, src *InstallResult) {
	if dst == nil || src == nil {
		return
	}
	if src.App != "" {
		dst.App = src.App
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	dst.Message = src.Message
	dst.Steps = append(dst.Steps, src.Steps...)
	if src.Service != nil {
		dst.Service = src.Service
	}
}

// waitReleaseBinaryReady 等 release 二进制服务真的就绪。**超时返回错误，不是警告。**
// Port > 0 时端口在监听是「服务真的活着」最强的证据；Port == 0（纯出站连接）只能看 launchd
// 有没有把作业真的拉起来。失败**如实返回 error** 并附日志尾部，绝不出现"装完却报任务完成"。
func (m *Manager) waitReleaseBinaryReady(ctx context.Context, spec releaseBinaryApp,
	p binaryReleasePaths, registered bool, result *InstallResult) error {

	d, ok := FindDescriptor(spec.ID)
	if !ok {
		return fmt.Errorf("没有 %s 的描述符（面板内部错误）", spec.ID)
	}
	ec := &ExecConfig{
		Ctx:    ctx,
		Spec:   d,
		Result: result,
		Runner: &plistOnlyRunner{},
		pathVars: map[string]string{
			"{root}": p.Root, "{home}": "", "{user}": "",
		},
		registered: registered,
	}
	return ec.assertReady(AssertReadyAction{
		What:    spec.Name,
		LogPath: p.ErrLog,
		Remedy: "按下面的日志尾部里的报错修好后重新部署；" +
			"也可以在「服务管理」里点「重启服务」再试" +
			"（配置文件在 " + p.Config + "）",
	})
}

// configSeedSecrets 是生成配置时替换进模板的随机凭据，显式带出来好让用户在安装结果里看到
// 并按需复制（dashboard 账号口令、frpc 要填的 auth.token）；口令只放结果的可复制区块，不写进
// 步骤叙述（步骤会被折叠、也会进审计日志）。
type configSeedSecrets struct {
	Token    string
	User     string
	Password string
}

// generateConfigSecrets 按模板里出现的占位符生成凭据 —— 只生成模板真正用到的那些，避免
// "生成了却没人用"的假信息（模板里没有 {user} 的应用就不生成随机用户名）。
func generateConfigSecrets(seed, reusableToken string) (configSeedSecrets, error) {
	s := configSeedSecrets{User: "admin"}
	var err error
	if strings.Contains(seed, "{token}") {
		// 优先复用本机 frps 的 token：同机的 frpc 装完就能连上，不用手抄一遍。
		s.Token = reusableToken
		if s.Token == "" {
			if s.Token, err = randomHex(16); err != nil {
				return s, fmt.Errorf("生成 auth.token 失败: %w", err)
			}
		}
	}
	if strings.Contains(seed, "{user}") {
		if s.User, err = randomHex(4); err != nil {
			return s, fmt.Errorf("生成用户名失败: %w", err)
		}
	}
	if strings.Contains(seed, "{password}") {
		if s.Password, err = randomHex(8); err != nil {
			return s, fmt.Errorf("生成口令失败: %w", err)
		}
	}
	return s, nil
}

// expandConfigSeed 把凭据替换进模板。
func expandConfigSeed(seed string, s configSeedSecrets) string {
	r := strings.NewReplacer(
		"{token}", s.Token,
		"{user}", s.User,
		"{password}", s.Password,
	)
	return r.Replace(seed)
}

// writeConfigSeed 把模板展开后写入 path（0600：文件里是明文凭据）。它会**覆盖**已存在的文件 ——
// "要不要保留旧文件"由调用方判断：原生路径靠 marker 区分"面板生成的"与"上游示例"（上游示例必须
// 被覆盖），compose 路径则"有就不动"（用户改 serverAddr/token 是常态）。
func writeConfigSeed(path, seed string, s configSeedSecrets) error {
	if err := os.WriteFile(path, []byte(expandConfigSeed(seed, s)), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

// matchChecksum 是校验的判定本身（纯函数，便于单测证明"坏文件真的会被拦住"——
// TestReleaseBinaryChecksumRejectsMismatch 直接喂错哈希）。拆出来是因为 verifyReleaseChecksum
// 要下载、要 root、要跑 curl，没法在单测里造一个"哈希不匹配"的真实场景。
func matchChecksum(asset, want, got, source, root string) error {
	if strings.EqualFold(want, got) {
		return nil
	}
	// 明确报错并中止，不静默继续：这正是镜像/网络篡改或损坏的样子。
	return fmt.Errorf("SHA-256 校验不通过，已中止安装：官方清单里 %s = %s，"+
		"下载到的文件 = %s（清单来源：%s）。请重试；若反复不一致，"+
		"可手动下载产物放到 %s 再安装",
		asset, want, got, source, root)
}

// checksumURLs 返回校验清单的下载地址（官方优先，镜像兜底）。
func (a releaseBinaryApp) checksumURLs() []string {
	if a.ChecksumAsset == "" {
		return nil
	}
	official := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", a.Repo, a.Tag, a.ChecksumAsset)
	urls := []string{official}
	for _, m := range gitHubReleaseMirrors {
		urls = append(urls, m+official)
	}
	return urls
}

// fetchChecksumList 下载并返回清单内容，同时返回"来源"用于如实写进日志。先试**官方**清单
// 地址（文件只有 1.6KB，慢链路也能秒下）再退镜像 —— 这样"tarball 来自镜像、清单来自官方"时
// 校验才有真实意义；官方完全不可达时退化成"防传输损坏"，安装日志里会写明清单来源。
func (m *Manager) fetchChecksumList(ctx context.Context, spec releaseBinaryApp, root string) (string, string, error) {
	urls := spec.checksumURLs()
	tmp := filepath.Join(root, spec.ChecksumAsset+".part")
	defer func() { _ = os.Remove(tmp) }()
	var lastErr error
	for i, u := range urls {
		source := "官方地址"
		if i > 0 {
			source = "加速镜像（第三方）"
		}
		// 清单很小，不需要 150 秒那么久；给 60 秒足够，且失败就换下一个。
		out, err := m.runAsUser(ctx, 2*time.Minute, "/usr/bin/curl",
			"-fL", "--retry", "1", "--connect-timeout", "15", "--max-time", "60",
			"-o", tmp, u)
		if err != nil {
			lastErr = fmt.Errorf("%s 失败: %v（%s）", u, err, tailText(out, 160))
			continue
		}
		b, rerr := os.ReadFile(tmp)
		if rerr != nil || len(b) == 0 {
			lastErr = fmt.Errorf("%s 下载内容为空: %v", u, rerr)
			continue
		}
		return string(b), source, nil
	}
	return "", "", lastErr
}

// checksumFor 从清单里取出某个文件的期望 sha256。清单格式是 GNU coreutils 的
// "<64位hex>  <文件名>"（两个空格），这里按空白切分，对单/双空格都成立；文件名用 base
// （清单里只有文件名、没有路径）。
func checksumFor(list, asset string) (string, error) {
	want := ""
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == asset || filepath.Base(fields[1]) == filepath.Base(asset) {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return "", fmt.Errorf("官方校验清单里没有 %s（清单里没有任何一行匹配）", asset)
	}
	if len(want) != 64 {
		return "", fmt.Errorf("官方校验清单里的 %s 不是 64 位 sha256：%q", asset, want)
	}
	return want, nil
}

// fileSHA256 计算文件的 sha256（下载产物 12.7MB，直接读没问题）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractArgs 组装 tar 的解压参数（默认 `-xzf <asset> -C <root>`，tar 的选项必须在成员名前）。
// PickBinary 时只解压 Binary 那一个成员；成员名带不带顶层目录取决于 TarStrip：>0 时必须是
// <顶层目录>/<binary>，==0 时就是 <binary>（ddns-go 实测无顶层目录，猜错会让 tar 直接解压失败）。
func (a releaseBinaryApp) extractArgs(asset, root string) []string {
	args := []string{"-xzf", asset, "-C", root}
	if a.PickBinary && a.Binary != "" {
		member := a.Binary
		if a.TarStrip > 0 {
			args = append(args, fmt.Sprintf("--strip-components=%d", a.TarStrip))
			top := strings.TrimSuffix(filepath.Base(a.Asset), ".tar.gz")
			if top != filepath.Base(a.Asset) && top != "" {
				member = top + "/" + a.Binary
			}
		}
		args = append(args, member)
	}
	return args
}

// ensureReleaseConfig 生成配置文件：已有**面板生成**的（含 marker）原样保留（用户可能改过端口/
// 口令），其余（没有文件 / 只有上游示例）一律写面板这份 —— "存在即保留"会让面板永远写不进配置、
// 用户装完只有默认值且没有报错。例外是 PreserveExistingConfig（ddns-go，判据见字段注释）。
func (m *Manager) ensureReleaseConfig(spec releaseBinaryApp, p binaryReleasePaths) (configSeedSecrets, bool, error) {
	if configKeepsExisting(spec.PreserveExistingConfig, p.Config) {
		return configSeedSecrets{}, false, nil
	}
	s, err := generateConfigSecrets(spec.ConfigSeed, "")
	if err != nil {
		return configSeedSecrets{}, false, err
	}
	if err := writeConfigSeed(p.Config, spec.ConfigSeed, s); err != nil {
		return configSeedSecrets{}, false, err
	}
	return s, true, nil
}

// configKeepsExisting 是"要不要保留磁盘上这份配置"的**唯一判据**（执行器的 ensure_config 步骤
// 也调它 —— 两份判据漂移会让重装一次冲掉一次用户配置）。preserveExisting（ddns-go）**文件存在
// 即保留**（其界面会整体重写 YAML）；其余含 marker 才保留，否则视为上游示例、必须被面板覆盖。
func configKeepsExisting(preserveExisting bool, path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if preserveExisting {
		return true
	}
	return strings.Contains(string(b), panelConfigMarker)
}

// credentialBlock 生成安装结果里的"可复制凭据区块"。口令与 token 只出现在这里，不写进步骤
// 叙述：步骤会被折叠、也会进审计日志，明文口令混在叙述里既容易漏看又容易被顺手复制走。
func credentialBlock(name, configPath, uiURL string, s configSeedSecrets) []string {
	if s.Password == "" && s.Token == "" {
		return nil
	}
	out := []string{
		"",
		"──────────────────────────────────────────────",
		"  " + name + " 凭据（面板随机生成，请自行保存）",
		"──────────────────────────────────────────────",
	}
	if uiURL != "" {
		out = append(out, "  界面地址 = "+uiURL)
	}
	if s.User != "" {
		out = append(out, "  用户名   = "+s.User)
	}
	if s.Password != "" {
		out = append(out, "  密    码 = "+s.Password)
	}
	if s.Token != "" {
		out = append(out, "  auth.token = "+s.Token+"（另一端必须填同一个）")
	}
	if configPath != "" {
		out = append(out, "  配置文件 = "+configPath+"（服务详情里可「📝 编辑配置文件」）")
	}
	return out
}

// releaseBinaryPlist 生成系统级 LaunchDaemon 定义。
func releaseBinaryPlist(a releaseBinaryApp, p binaryReleasePaths, user string) string {
	args := make([]string, 0, len(a.Args)+1)
	args = append(args, p.Binary)
	for _, arg := range a.Args {
		args = append(args, strings.ReplaceAll(arg, "{root}", p.Root))
	}
	var b strings.Builder
	for _, arg := range args {
		b.WriteString("        <string>")
		b.WriteString(xmlEscape(arg))
		b.WriteString("</string>\n")
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>UserName</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, a.Label, user, b.String(), p.Root, p.OutLog, p.ErrLog)
}

// xmlEscape 转义放进 plist <string> 里的路径。家目录理论上不含 & < >，
// 但这是拼进 XML 的字符串，不该靠"理论上"。
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// plistOnlyRunner 是"只算不碰"的兼容入口（渲染 plist、走就绪判定）用的空 Runner：不碰文件
// 系统、不碰 launchd、不发网络请求 —— 那正是单测不该做的事。就绪判定用到的三个探针走的仍是
// ready.go 里的包级注入点，所以 ready_test.go 的桩照常生效。
type plistOnlyRunner struct{}

func (plistOnlyRunner) Stat(string) (bool, bool, error)             { return false, false, nil }
func (plistOnlyRunner) MkdirAll(string, os.FileMode) error          { return nil }
func (plistOnlyRunner) WriteFile(string, []byte, os.FileMode) error { return nil }
func (plistOnlyRunner) ReadFile(string) ([]byte, error)             { return nil, os.ErrNotExist }
func (plistOnlyRunner) Size(string) int64                           { return -1 }
func (plistOnlyRunner) Chmod(string, os.FileMode) error             { return nil }
func (plistOnlyRunner) Rename(string, string) error                 { return nil }
func (plistOnlyRunner) Remove(string) error                         { return nil }
func (plistOnlyRunner) RemoveAll(string) error                      { return nil }
func (plistOnlyRunner) SHA256(string) (string, error)               { return "", nil }
func (plistOnlyRunner) ChownTree(string, string) error              { return nil }
func (plistOnlyRunner) CopyTree(string, string) error               { return nil }
func (plistOnlyRunner) FileType(context.Context, string) (string, error) {
	return "", nil
}
func (plistOnlyRunner) RunAsUser(context.Context, time.Duration, string, ...string) (string, error) {
	return "", nil
}
func (plistOnlyRunner) RunAsUserEnv(context.Context, time.Duration, []string, string, ...string) (string, error) {
	return "", nil
}
func (plistOnlyRunner) RunRoot(context.Context, time.Duration, string, ...string) (string, error) {
	return "", nil
}
func (plistOnlyRunner) Bootout(context.Context, string) error           { return nil }
func (plistOnlyRunner) Bootstrap(context.Context, string, string) error { return nil }
func (plistOnlyRunner) WaitPort(ctx context.Context, port int, timeout time.Duration) bool {
	// 走 ready.go 的包级注入点：单测用它替换真实探针（不许联网、不许等真实时间）。
	return readyWaitPort(ctx, port, timeout)
}
func (plistOnlyRunner) LaunchRunning(label string) (bool, string) { return readyLaunchRunning(label) }
func (plistOnlyRunner) HTTPGet(ctx context.Context, url string, timeout time.Duration) (int, error) {
	return httpProbeStatus(ctx, url, timeout)
}
func (plistOnlyRunner) PrimaryIP(context.Context) string { return "" }

func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func max64(a float64, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// IsReleaseBinaryApp 判断某个应用 ID 是否由"官方 release 原生二进制"安装器部署，web 层据此
// 把安装请求分流到 InstallReleaseBinary。用注册表判断而不是在 web 层再抄一份 ID 列表：
// 抄的那份在加新条目时一定会漏（漏了的后果是市场点了没反应）。
func IsReleaseBinaryApp(id string) bool {
	_, ok := releaseBinaryApps[id]
	return ok
}

// ReleaseBinaryIsMirrorOnly 报告某个 release 二进制条目是不是"只在镜像站上有"
// （自研产物：没有官方 GitHub 地址）。
//
// 导出给 web 层分流用：这类条目虽然登记在注册表里（市场门禁要求），
// 但**不能**走通用的 release 安装流程（那套会去 GitHub 拼地址、装系统级 daemon）。
func ReleaseBinaryIsMirrorOnly(id string) bool {
	spec, ok := releaseBinaryApps[id]
	return ok && spec.MirrorOnly
}

// ReleaseBinaryAppConfigRelPaths 返回由本安装器部署的应用"家目录下的配置文件"相对路径
// （<RootDir>/<ConfigFile>），键是应用 ID。这些文件常有第三方 token（ddns-go 的 DNS token、
// frpc 的 auth.token 等），备份勾选权交给用户（默认不勾）；清单**从注册表派生**（有门禁测试锁死）。
func ReleaseBinaryAppConfigRelPaths() map[string]string {
	out := map[string]string{}
	for id, spec := range releaseBinaryApps {
		if spec.ConfigFile == "" {
			continue
		}
		out[id] = spec.RootDir + "/" + spec.ConfigFile
	}
	return out
}

// hostOf 取 URL 的主机名，用于日志（不要把完整 URL 塞进一行日志）。
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// UninstallReleaseBinary 停止并删除服务，按需删除安装目录。实现体是 m.UninstallByDescriptor
// （见 tarball_descriptor.go）—— 老函数名保留是为了 internal/services/uninstall_app.go 的
// 分流与既有单测，流程只有一份。
func (m *Manager) UninstallReleaseBinary(ctx context.Context, id string, removeData bool, result *InstallResult) error {
	d, ok := FindDescriptor(id)
	if !ok || d.Rail != RailTarball {
		return fmt.Errorf("没有 %s 的卸载实现", id)
	}
	return m.UninstallByDescriptor(ctx, d, removeData, result)
}

// releaseBinaryPlan 给出安装器类应用的卸载计划（实现见 uninstallPlanForDescriptor）。
func (m *Manager) releaseBinaryPlan(id string) (UninstallPlan, bool) {
	d, ok := FindDescriptor(id)
	if !ok || d.Rail != RailTarball {
		return UninstallPlan{}, false
	}
	return m.uninstallPlanForDescriptor(d)
}

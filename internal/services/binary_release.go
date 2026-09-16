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

// ============================================================================
//  「官方 release 原生二进制」安装器
//
//  为什么需要它：AGENTS.md 铁律 8 说"能原生就原生"，原生有两条路 ——
//  Homebrew formula，或者**官方 darwin-arm64 预编译产物**。后一条路原本不存在，
//  README「应用市场」第 2 条当时写的是"先走 Docker，等通用安装器做出来再换"。
//
//  现在这套安装器服务**四个**条目（见 releaseBinaryApps）：
//    frpc（frp 客户端）、orbien-client（Orbien CLI 客户端）、ddns-go（动态域名解析）
//    与 alist（文件列表，2026-09-17 新增 —— 它从未进过 homebrew-core，只能走这条路）。
//  2026-09-16 用户要求**彻底移除**三个条目：Lucky、Orbien 服务端、frps。
//  它们已从 releaseBinaryApps 与 catalog.go 中删除；不要再加回来。
//  这三个的共同点是：官方 release 有 darwin-arm64 产物，且在 macOS 上**不适合 Docker**
//  （理由见 catalog.go 里各条目的注释：Colima 里跑的是 Linux 虚拟机，容器看到的
//  是虚拟机的网络，不是 Mac 的 —— 内网穿透这类"贴着网络栈"的工具放进容器就是错的）。
//
//  ddns-go 为什么也走这条路（而不是 AGENTS.md 铁律 8 里优先的 brew formula）：
//  homebrew-core 里**有** ddns-go 这个 formula，但它**没有 service 块**
//  （2026-09-16 在 Mac mini 上实测：`brew info --json=v2 ddns-go` 的 service 字段是
//  null，`brew services start ddns-go` 直接报
//  "has not implemented #plist, #service or provided a locatable service file"）。
//  也就是说 brew 只能把包装上，**给不出任何 launchd 守护进程** —— 用户会得到一个
//  "装了但从来没在跑、:9876 也打不开"的面板条目。所以这里改用官方 release 产物，
//  由通用安装器写系统级 LaunchDaemon（上游 release 里还有 checksums.txt 可核对 sha256）。
//
//  安装流程（每个应用只有参数不同）：
//    建目录 → curl 下载官方 tarball（官方直连失败时退到加速镜像）
//    → **有官方 sha256 清单就核对**（frp 有；Orbien 上游没有）
//    → tar 解压（含"一个 tarball 里只挑需要的二进制"）
//    → **/usr/bin/file 复核二进制确实是 arm64**（不是就报错退出，
//      这条是"绝不用 Rosetta / 绝不放行 amd64"在运行期的兜底）
//    → 写配置文件（模板 + 随机凭据）→ 由真实用户拥有
//    → 写系统级 LaunchDaemon（开机自启、不依赖登录）→ bootstrap
//    → 等端口真的监听 → 登记进「服务管理」（带健康检查地址）
//
//  为什么注册成系统级 LaunchDaemon 而不是用户级 LaunchAgent：
//  与 IOPaint / Qwen3 TTS / 音色接收端一致 —— Mac mini 无人登录时也要在跑。
//
//  为什么还要有加速镜像：本机实测（2026-09）官方 release 地址**可达但很慢** ——
//  12.9MB 的产物直连花了 293 秒（约 46KB/s，几乎顶到 --max-time 300），
//  同一份产物经 ghfast.top 只要 21 秒。所以顺序是"官方优先"，慢过头或失败才退镜像。
//  代价要写清楚：加速镜像（ghfast.top / gh-proxy.com）是**第三方**服务，
//  它转发的就是我们随后以 root 执行的二进制。frp 有官方 sha256 清单可核对；
//  Orbien 客户端的 release 里**没有** checksums 文件，只能做架构复核。
//  不接受的用户可以自己下载产物放进安装目录 —— 所有自动地址都失败时面板会
//  直接用它（下载先落 .part 再改名，不会覆盖你放好的文件；有清单的条目**仍然会校验**）。
// ============================================================================

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
	// TarStrip / PickBinary：一个 tarball 里含多个二进制时，只解压需要的那一个。
	// frp 的 frp_0.71.0_darwin_arm64.tar.gz 里同时有 frps、frpc 与一份上游示例
	// frps.toml —— 全解压会让安装目录里多出一个用不到的 frpc，还会把上游示例配置
	// 落在我们生成配置的位置上。TarStrip=1 剥掉 tarball 的顶层目录（frp_0.71.0_darwin_arm64/），
	// PickBinary 只取 Binary 那一个成员。
	TarStrip   int
	PickBinary bool
	// Args 是启动参数，{root} 会被替换成安装根目录
	Args []string
	// Port 用于端口存活判断（比 launchd 状态更可信）
	Port int
	// UIPort 是网页界面的端口，**只在它不等于 Port 时**填。
	// frps 的 Port 是协议口 bindPort(7000)，UIPort 是 dashboard(7500)：
	// 健康检查、服务记录端口与「打开」入口都该用后者。
	UIPort int
	// HealthPath 交给面板做 HTTP 健康检查（空表示只按端口判断）
	HealthPath string
	// BindAddress 是应用**自己真正监听**的地址（"127.0.0.1" / "0.0.0.0"）。
	//
	// 为什么必须声明而不是一律猜回环：描述符的 Urls.AdvertisedURL 会据此决定
	// 广告哪个地址 —— 只绑回环的服务广告成 LAN 地址就是"点了必然打不开的按钮"，
	// 反过来绑 0.0.0.0 的服务只广告 127.0.0.1 又会让用户以为只能用本机。
	// 空 = 127.0.0.1（这条轨上 frpc / ddns-go / orbien 都是这个默认：
	// frpc 的 admin UI 只绑回环；ddns-go 用 -l :9876 绑通配，但它已经在目录里
	// 显式给了 PreferDirect 的端口直连入口，不受这里的广告地址影响）。
	BindAddress string
	// ConfigFile 是安装目录里的配置文件名（空 = 这个应用没有独立配置文件）。
	// 面板服务详情里的「📝 编辑配置文件」按它定位（见 catalog.App.ConfigPath）。
	ConfigFile string
	// ConfigSeed 是首次安装时写入配置文件的模板；{token} / {user} / {password}
	// 会被替换成随机值（见 ensureReleaseConfig）。空表示由应用专属逻辑生成。
	ConfigSeed string
	// PreserveExistingConfig 表示这个应用的配置文件由**应用自己维护**
	// （ddns-go 的网页界面点"保存"时会整体重写 YAML），面板写进去的 marker 注释
	// 活不过第一次保存。
	//
	// 为什么必须单独标出来：默认规则是"有面板 marker 就保留、否则当上游示例覆盖"，
	// 对 frpc / orbien 成立（那两个应用的配置只有人会改，marker 一直在）。
	// 但 ddns-go 保存一次后 marker 就没了，重装时会被当成"上游示例"覆盖 ——
	// 那等于把用户填好的 DNS 服务商密钥与域名直接冲掉。所以这类应用改成
	// "文件存在就一律保留"（判据见 ensureReleaseConfig）。
	PreserveExistingConfig bool
	// ChecksumAsset 是上游提供的 SHA-256 清单文件名（空 = 上游不提供，无法校验）。
	// frp 是这套安装器里唯一提供校验清单的上游；Lucky / Orbien 的 release 里没有，
	// 那它们就只能靠 file(1) 的架构复核（见 README 的如实说明）。
	ChecksumAsset string
	// Notes 是安装结果里要额外告诉用户的话
	Notes []string
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

// gitHubReleaseMirrors 是 GitHub release 的加速前缀（拼接在完整官方 URL 前面）。
//
// 顺序即优先级：官方地址永远排第一，镜像只在它失败或慢到被 --max-time 掐断时用。
// 实测（2026-09）：官方地址可达但约 46KB/s，镜像约 640KB/s —— 所以退路是必要的，
// 但不能反过来把第三方放在前面。
var gitHubReleaseMirrors = []string{
	"https://ghfast.top/",
	"https://gh-proxy.com/",
}

// orderBySpeed 按实测速度把候选地址从快到慢排序（速度 <=0 的排最后，保持原相对顺序）。
//
// 为什么要先探测：中国大陆无代理时 GitHub Release **完全不通**（实测 20 秒 0 字节），
// 而官方地址排第一 + `--max-time 150` 意味着每个应用都先白等 150 秒。
// 探测一次只要几秒，之后直接下最快的那个 —— 有代理/直连快的用户也不会被拖慢。
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
//
// 判定顺序（README「应用市场」第 2 条）：官方 release 有 darwin_arm64 产物、
// 且**没有** brew formula。
// 这里只放"一项能力服务多个应用"的通用参数，不为单个应用发明专属流程。
var releaseBinaryApps = map[string]releaseBinaryApp{
	// 2026-09-16 用户要求彻底移除 Lucky / Orbien 服务端 / frps 三个条目，
	// 只保留两个客户端。它们的配置与安装目录**不再由面板管理**；
	// 想把某个条目加回来时注意：市场的安装入口按 IsReleaseBinaryApp(id) 分流，
	// 注册表与 catalog.go 必须同时有，否则会出现"条目是 compose、
	// 实际却走 release 安装器去 GitHub 下 darwin 二进制"这种隐蔽错配。
	"frpc": {
		ID: "frpc", Label: frpcLabel, Name: "frp 客户端 (frpc)", Icon: "🧷",
		Category: "tool", RootDir: "frpc",
		// 与 frps **同一个**上游 tarball（里面 frps / frpc 各一个二进制），只挑 frpc 那一个成员。
		// 面板不提供 frps，但用官方包解出的客户端版本自然与主流服务端一致。
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
		// arm64 证据：v3.6.0 的资产里有 orbien_3.6.0_darwin_arm64.tar.gz（2,104,350 B）；
		// 实测解压出的 orbien 用 file 报 Mach-O arm64、`orbien --help` 输出 "orbien client"。
		// 上游**没有** checksums 文件，所以只能靠 file(1) 复核（如实说明）。
		Repo: "orbien-org/orbien", Tag: "v3.6.0", Asset: "orbien_3.6.0_darwin_arm64.tar.gz",
		Binary: "orbien",
		// tarball 里只有 orbien 一个二进制（外加一份上游示例 orbien.toml），整体解压即可；
		// 示例配置会被面板生成的配置按 marker 覆盖（同 orbien 服务端）。
		Args: []string{"-c", "{root}/orbien.toml"},
		// 客户端不监听端口（纯出站连接），所以没有端口/健康检查；
		// 是否在跑只看 launchd。
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
		// arm64 证据：v6.17.7 的资产里有 ddns-go_6.17.7_darwin_arm64.tar.gz（4,386,545 B，
		// sha256 9dac9d82…），tarball 内是平级的 ddns-go / README.md / README_EN.md / LICENSE
		// （2026-09-16 实下核对过目录结构），file(1) 报 Mach-O 64-bit executable arm64。
		Repo: "jeessy2/ddns-go", Tag: "v6.17.7", Asset: "ddns-go_6.17.7_darwin_arm64.tar.gz",
		Binary: "ddns-go",
		// tarball **没有**顶层目录（4 个成员平级），所以 TarStrip=0；
		// PickBinary 只解压 ddns-go 那一个，不把 README / LICENSE 摊进安装目录。
		TarStrip: 0, PickBinary: true,
		// -c 写死到安装目录：ddns-go 的默认值是 $HOME/.ddns_go_config.yaml ——
		// 那会把它的配置散落在用户家目录根下，面板的「📝 编辑配置文件」也就定位不到
		// （面板按 <home>/<RootDir>/<ConfigFile> 解析）。
		// -l 显式写 :9876：上游默认也是它，但写出来才不会因为上游改默认而静默失联。
		Args: []string{"-c", "{root}/ddns-go.yaml", "-l", ":9876"},
		// 9876 是它的网页界面端口，也是它唯一监听的端口。
		// HealthPath 用 "/"：实测未登录时 GET / 返回 307 → /login，
		// 而面板的健康判定把 2xx/3xx 都算健康（见 health.go），能稳定反映"服务活着"。
		Port: 9876, HealthPath: "/",
		ConfigFile: "ddns-go.yaml",
		ConfigSeed: ddnsGoConfigSeed,
		// ddns-go 的网页界面保存时会整体重写 YAML，面板 marker 会被抹掉 ——
		// 所以判据必须是"文件存在就保留"，否则重装会冲掉用户的 DNS 密钥（见字段注释）。
		PreserveExistingConfig: true,
		// 上游 release 里有 checksums.txt，且其中一行就是这份 darwin_arm64 产物 ——
		// 有清单就必须核对（回落到第三方加速镜像时这是唯一的内容校验）。
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
		// 为什么走 release 产物而不是 brew：**alist 从未进过 homebrew-core**。
		// 2026-09-17 逐条核对过：`brew info alist` → "No available formula"；
		// formulae.brew.sh/api/formula/alist.json → 404；homebrew-core 的提交历史里
		// 与 alist 相关的提交是**空数组**（不是"被删掉了"）。唯一的三方 tap 停在
		// 2024-01 且已死。上游 AlistGo/alist 仍在更新（v3.64.0，2026-09-03）。
		// arm64 证据：v3.64.0 资产里有 alist-darwin-arm64.tar.gz（43,021,495 B）；
		// 2026-09-17 实下核对：tarball 内**只有平级的 alist 一个成员**（无顶层目录），
		// file(1) 报 Mach-O 64-bit executable arm64，`alist --help` 输出正常。
		Repo: "AlistGo/alist", Tag: "v3.64.0", Asset: "alist-darwin-arm64.tar.gz",
		Binary: "alist",
		// tarball 只有 alist 一个成员且平级 → TarStrip=0；PickBinary 仍然开着，
		// 上游以后若在包里加 README/LICENSE 也不会摊进安装目录。
		TarStrip: 0, PickBinary: true,
		// --data 写死到安装目录下的 data/：不写的话 Alist 会在**当前工作目录**
		// 下建 data/（launchd 的 WorkingDirectory 不是我们想要的），
		// 面板的「📝 编辑配置文件」也会定位不到。
		Args: []string{"server", "--data", "{root}/data"},
		Port: 5244, HealthPath: "/",
		// Alist 默认监听 0.0.0.0:5244（实测：日志 "start HTTP server @ 0.0.0.0:5244"），
		// 所以广告地址必须是 LAN 地址 —— 否则用户看到 127.0.0.1:5244 会以为
		// 只能本机用（见 releaseBinaryApp.BindAddress 的注释）。
		BindAddress: "0.0.0.0",
		// Alist 自己在首次启动时创建 data/config.json（并写入 jwt_secret 等）。
		// 面板**不**写这个文件（ConfigSeed 为空），只是告诉面板它在哪里，
		// 让服务详情里的「📝 编辑配置文件」能定位到它。
		ConfigFile: "data/config.json",
		// 上游 release **只有 md5.txt，没有 sha256 清单**，所以 ChecksumAsset 留空：
		// 这条轨在 ChecksumAsset 为空时的行为是"如实说明只做了架构复核 + md5 互证"，
		// 而不是假装校验过 sha256（见 tarballDescriptor 里的 else 分支）。
		Notes: []string{
			"安装目录：~/alist（二进制、data/、日志都在这里）。",
			"初始管理员口令由 Alist 首次启动时随机生成 —— 面板已从启动日志里抓出来放在上面的凭据区块里；" +
				"忘了口令就在终端执行 ~/alist/alist admin set <新口令>。",
			"⚠️ Alist 上游只发布 md5 校验清单，没有 sha256：这一步的内容校验是" +
				"「架构复核（file -b）+ 与上游 md5 互证」，强度弱于有 sha256 清单的应用（frpc / ddns-go）。",
		},
	},
}

// frpsConfigSeed / orbienServerConfigSeed 是面板生成配置文件的模板。
//
// 占位符（替换逻辑见 generateConfigSecrets / expandConfigSeed）：
//
//	{token}    —— 随机 32 位十六进制，用于 auth.token
//	{user}     —— 随机用户名（模板里出现才生成随机值，否则用 admin）
//	{password} —— 随机 16 位十六进制口令
//
// 为什么不写成"每应用一个生成函数"：两个应用只有字段名与端口不同，
// 模板 + 占位符既省一份重复逻辑，也让人一眼看清写进磁盘的到底是什么。
const (
	panelConfigMarker = "由 ZizPanel 生成"
)

// frpcConfigSeed 是 frpc（客户端）的 frpc.toml 模板。
//
// 面板**不再提供 frps 服务端**（2026-09-16 用户要求移除），所以这里的
// serverAddr/serverPort 是占位值：请改成你自己的 frps 地址与端口。
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

// orbienClientConfigSeed 是 orbien（CLI 客户端）的 orbien.toml 模板。
//
// 面板**不再提供 Orbien 服务端**（2026-09-16 用户要求移除），所以这里的 server
// 是占位值：请改成你自己的 Orbien 服务端地址。客户端没有 Web 界面。
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

// ddnsGoConfigSeed 是 ddns-go 的配置骨架。
//
// 为什么只写注释、不预填 DNS 字段：ddns-go 的完整配置（服务商 ID/Secret、域名、
// webhook、登录口令哈希）是它自己的网页界面在"保存"时生成的，面板凭空拼一份
// 完整 YAML 反而容易拼错字段名；而**只有注释**的 YAML 是合法的 ——
// yaml.Unmarshal 得到全零 Config，ddns-go 照常起来（真机实测：正常监听 :9876，
// GET / 307 跳 /login），随后第一次保存会把它覆写成完整配置。
//
// 为什么必须让这个文件先存在：服务详情里的「📝 编辑配置文件」按
// <home>/ddns-go/ddns-go.yaml 定位，文件不存在时点开就是一个"读文件失败"。
// 用户第一次打开面板就能看到这份说明，而不是先去网页配置一轮才发现按钮点不开。
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

// frp 的官方校验清单文件名（frpc 安装时用它核对下载产物）。
const frpChecksumAsset = "frp_sha256_checksums.txt"

// releaseURL 是官方下载地址。
func (a releaseBinaryApp) releaseURL() string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", a.Repo, a.Tag, a.Asset)
}

// downloadURLs 返回按优先级排列的下载地址：官方第一，其余为加速镜像。
func (a releaseBinaryApp) downloadURLs() []string {
	official := a.releaseURL()
	urls := []string{official}
	for _, m := range gitHubReleaseMirrors {
		urls = append(urls, m+official)
	}
	return urls
}

// binaryReleasePaths 是这套安装器用到的全部路径。
//
// 它现在是**从描述符推导**出来的（见 binaryReleasePathsFor），不是另一份独立的
// 路径计算 —— 两份路径计算漂移正是"编辑配置文件按钮指向一个不存在的文件"
// 这类问题的根因。
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

// binaryReleasePaths（老签名，按参数表查）内部改用描述符。
func (m *Manager) binaryReleasePaths(a releaseBinaryApp) binaryReleasePaths {
	if d, ok := FindDescriptor(a.ID); ok {
		return m.binaryReleasePathsFor(d)
	}
	// 描述符没注册（理论上不会发生）时退回按参数表算：宁可给出一条正确路径，
	// 也不要在这里 panic 掉整个面板。
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
//
// 2026-09-17 模块化改造：这个函数**已经不再自己实现流程**，它是一层兼容壳 ——
//
//	· 参数表（releaseBinaryApps）与流程（steps DSL）都在描述符里
//	  （见 descriptor.go / steps.go / tarball_descriptor.go）；
//	· 这里的全部工作是"做前置校验 → 装配执行器 → 执行步骤"。
//
// 为什么要保留这个函数名：internal/web 的 install 分流、uninstall_app.go 的
// 卸载分流、以及一批单测都按它接线；对外契约（202 + task_id + SSE + 进度）
// 一个字都不能变（用户明确要求）。改的只是"实现放在哪"。
func (m *Manager) InstallReleaseBinary(ctx context.Context, id string, result *InstallResult) error {
	d, ok := FindDescriptor(id)
	if !ok || d.Rail != RailTarball {
		return fmt.Errorf("没有 %s 的原生二进制安装器", id)
	}
	// 幂等门禁（与 brew / compose 同一份判定，见 install_idempotent.go）：
	// 已经装过的 tarball 应用再点一次「安装」，必须直接短路成"已装跳过"，
	// **不执行任何安装命令** —— 否则会重跑下载/解包/bootout/bootstrap，
	// 既慢又会重启用户正在用的隧道服务（真机 frpc / ddns-go 复现）。
	//
	// 位置刻意放在 root / 用户校验**之前**：跳过是纯读判定（面板注册表 +
	// launchd plist），不需要 root；已经装好的机器上再点一次不该被权限校验挡住。
	// 判定只看真实注册证据（记录 / plist），不看磁盘产物，所以"卸载（保留数据）
	// 后重装"不会被误跳过；真冲突（记录属于别的应用、plist 标签对不上）时
	// 这里返回 false，继续走下面的正常安装，由 registerAppService 如实报错。
	if app, found := FindApp(id); found {
		if res, done := m.installedSkipResult(ctx, app); done {
			// "装过"不等于"现在是好的"：tarball 轨的**登记发生在验收之前**
			// （见 tarballInstallSteps 的顺序说明），所以一次失败的安装同样会留下
			// 记录与 plist —— 只凭它们判定，就会把"再点一次安装"永远挡在门外。
			// 2026-09-17 mini 真机的 Alist 就是这样：plist 参数写错、端口从未监听，
			// 而市场永远显示"已安装"，用户没有任何恢复入口。
			//
			// 所以这里补一条**真实存活**判据：端口真的在监听才跳过。
			// 没有端口可查的应用（orbien 客户端是纯出站连接）保持原判据不变。
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
	if err := m.OrchestrateTarballInstall(ctx, d, result); err != nil {
		return err
	}
	// Alist 的初始管理员口令是**它自己**在首次启动时随机生成、并打进启动日志的
	// （上游没有"由外部指定初始口令"的参数；`alist admin set` 需要把口令放进 argv，
	// 而本项目有过 argv 泄漏真凭据的事故，不采用）。所以只能在安装完成后从日志里抓，
	// 抓不到就如实说明、绝不编造口令。
	if d.ID == "alist" {
		m.appendAlistInitialPassword(d, result)
	}
	return nil
}

// alistInitialPasswordRe 匹配 Alist 首次启动时的那行日志。
//
// 上游原文（v3.64.0 实测）：Successfully created the admin user and the initial password is: b1bvB58Z
// 注意这行是 logrus 的 `msg="…"` 字段，口令后面紧跟一个引号 —— 所以字符集
// 严格限定为字母数字（Alist 生成的就是 [A-Za-z0-9]{8}），否则会把引号一起抓进来。
var alistInitialPasswordRe = regexp.MustCompile(`initial password is:\s*([A-Za-z0-9]+)`)

// appendAlistInitialPassword 把 Alist 日志里的初始口令搬进安装结果的**凭据区**。
//
// 为什么值得单写一段：Alist 装好之后用户第一件事就是登录，而初始口令只出现
// 在服务日志里 —— 让用户自己去翻 launchd 日志才算"装好了但用不了"。
//
// 口令**只进 InstallResult.Credentials**（面板 UI 的凭据区），不进任务步骤文本：
// 步骤会进任务日志 / SSE / 审计，口令出现在那里等于多一份长期留存。
// 这里只读日志、不写任何东西；日志里没有那行时明说"没抓到"，绝不编造。
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

// portHasListener 判断某个端口此刻真的有进程在监听。
//
// 用途：tarball 轨的幂等跳过判据（见 InstallReleaseBinary）。用端口而不是
// launchd 状态，理由与 assertReady 一致 —— launchd "已加载"不代表进程活着
// （KeepAlive 会不停重启一个起不来的进程），端口才是"真的在提供能力"的证据。
func (m *Manager) portHasListener(port int) bool {
	if port <= 0 {
		return false
	}
	info, err := m.checkPort(port)
	return err == nil && info.InUse
}

// adoptInstallResult 把幂等跳过的结果原样搬进调用方传入的 result。
//
// 为什么是"搬进"而不是让 InstallReleaseBinary 返回新的 *InstallResult：
// 老签名（error-only + 就地写 result）是 internal/web 与任务中心依赖的契约，
// 不许改。就地写字段既保住签名，又让任务终态带上"已经装过了，本次跳过"。
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
//
// 2026-09-17 模块化改造：就绪判定的实现搬进了描述符执行器
// （ExecConfig.assertReady，见 steps.go），这里只保留**兼容入口** ——
// 判定语义一个字都没变：
//   - Port > 0（frpc 7400 / ddns-go 9876）：端口在监听是「服务真的活着」最强的
//     证据（比 launchd 状态更可信）；
//   - Port == 0（orbien 客户端是纯出站连接，不监听任何端口）：没有端口可等，
//     只能看 launchd 有没有把这个作业真的拉起来（原来的 waitPort(0, 60s)
//     必然超时，每次安装都误报一次）。
//
// 失败**如实返回 error**，并附上进程错误日志尾部 —— 绝不再出现
// 「装完了但用不了」和「任务完成 ✅」同时显示在界面上的情况。
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

// configSeedSecrets 是生成配置时替换进模板的随机凭据。
//
// 为什么要显式带出来：安装结果里要能让用户看到并按需复制（dashboard 账号口令、
// frpc 要填的 auth.token），而不是让他装完再去翻文件。口令只放结果的可复制区块，
// 不写进步骤叙述里（步骤会被折叠、也会进审计日志）。
type configSeedSecrets struct {
	Token    string
	User     string
	Password string
}

// generateConfigSecrets 按模板里出现的占位符生成凭据。
//
// 只生成模板真正用到的那些：模板里没有 {user} 的应用（Orbien 服务端固定 admin）
// 就不生成随机用户名，避免"生成了却没人用"的假信息。
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

// writeConfigSeed 把模板展开后写入 path（0600：文件里是明文凭据）。
//
// 它会**覆盖**已存在的文件 —— "要不要保留旧文件"由调用方判断：
// 原生路径用 marker 区分"面板生成的"与"上游示例"（上游示例必须被覆盖），
// compose 路径则"有就不动"（用户改 serverAddr/token 是常态）。
// 两种情况语义不同，所以判定不放在这里。
func writeConfigSeed(path, seed string, s configSeedSecrets) error {
	if err := os.WriteFile(path, []byte(expandConfigSeed(seed, s)), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

// matchChecksum 是校验的判定本身（纯函数，便于单测证明"坏文件真的会被拦住"）。
//
// 拆出来的原因：verifyReleaseChecksum 要下载、要 root、要跑 curl，没法在单测里
// 造一个"哈希不匹配"的真实场景；而这条判定恰恰是整个安全收益的关键一步，
// 不能只靠人工试一次。单测 TestReleaseBinaryChecksumRejectsMismatch 直接喂错哈希。
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

// fetchChecksumList 下载并返回清单内容，同时返回"来源"用于如实写进日志。
//
// 顺序上先试**官方**清单地址（文件只有 1.6KB，慢链路也能秒下），再退镜像：
// 这样"tarball 来自镜像、清单来自官方"时校验才有真实意义。
// 局限要说清：如果官方地址完全不可达、清单也只能从同一个镜像取，这一步退化成
// "防传输损坏"而非"防镜像作恶"——安装日志里会把清单来源写出来。
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

// checksumFor 从清单里取出某个文件的期望 sha256。
//
// 清单格式是 GNU coreutils 的 "<64位hex>  <文件名>"（两个空格）；这里按空白切分，
// 对单/双空格都成立。文件名用 base，因为清单里只有文件名、没有路径。
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

// extractArgs 组装 tar 的解压参数。
//
// 默认是 `-xzf <asset> -C <root>`（tar 的选项必须在成员名前，所以这里返回
// "-xzf asset -C root [--strip-components=N] [member]" 的全部参数）。
// PickBinary 时只解压 Binary 那一个成员；成员名要不要带"顶层目录"取决于
// TarStrip：
//   - TarStrip > 0：tarball 里有一层顶层目录（frp 的 tarball 是
//     frp_0.71.0_darwin_arm64/…），tar 的成员匹配按归档内的完整路径，
//     所以成员名必须是 <顶层目录>/<binary>；
//   - TarStrip == 0：tarball 里就是平级的成员（ddns-go 的 tarball 是
//     ddns-go / README.md / README_EN.md / LICENSE），成员名就是 <binary>。
//
// 这条区分是真机踩出来的：ddns-go 的产物没有顶层目录，如果沿用"成员名一定带
// 顶层目录（用 asset 名去掉 .tar.gz 猜）"的老写法，tar 会去找一个不存在的
// ddns-go_6.17.7_darwin_arm64/ddns-go，解压直接失败。
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

// ensureReleaseConfig 生成"release 二进制"类应用的配置文件。
//
// 已有**面板生成**的配置时原样保留（用户可能自己改过端口/口令，重装不该把它抹掉），
// 其余情况（没有文件 / 只有上游示例 / 上游换了示例内容）一律写面板这份。
// 为什么靠 marker 而不是"存在即保留"：orbien 客户端的 tarball 自带一份上游示例配置，
// 解压后正好落在目标路径上；按"存在即保留"处理的话，面板永远写不进自己的配置，
// 用户装完只有默认值、没有界面入口，而且没有任何报错。
//
// 例外是 PreserveExistingConfig（ddns-go）：那个应用的配置由它自己的网页界面重写，
// marker 活不过第一次保存，所以对它判据退化成"文件存在即保留"（详见字段注释）。
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

// configKeepsExisting 是"要不要保留磁盘上这份配置"的**唯一判据**
// （老 ensureReleaseConfig 的判据，逐字保留；执行器的 ensure_config 步骤
// 也调它 —— 两份判据漂移会让重装一次冲掉一次用户配置）。
//
//   - preserveExisting（ddns-go）：**文件存在即保留**。它的网页界面保存时会
//     整体重写 YAML，面板写的 marker 注释活不过第一次保存；按 marker 判断的话，
//     重装会把用户填好的 DNS 服务商密钥与域名直接冲掉。
//   - 其余（frpc / orbien-client）：含面板 marker 才保留（用户可能自己改过端口/
//     口令）；否则视为"上游示例配置"，必须被面板这份覆盖 —— 否则面板永远写不进
//     自己的配置，用户装完只有默认值、没有界面入口，而且没有任何报错。
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

// credentialBlock 生成安装结果里的"可复制凭据区块"。
//
// 口令与 token 只出现在这里，不写进步骤叙述：步骤会被折叠、也会进审计日志，
// 明文口令混在叙述里既容易漏看又容易被顺手复制走。
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

// xmlEscape 转义放进 plist <string> 里的路径。
// 家目录理论上不含 & < >，但这是拼进 XML 的字符串，不该靠"理论上"。
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// plistOnlyRunner 是"只算不碰"的兼容入口（渲染 plist、走就绪判定）用的空 Runner。
//
// 它不碰文件系统、不碰 launchd、不发网络请求 —— 那正是单测不该做的事。
// 就绪判定用到的三个探针（WaitPort / LaunchRunning / HTTPGet）走的仍是
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

// IsReleaseBinaryApp 判断某个应用 ID 是否由"官方 release 原生二进制"安装器部署。
//
// web 层据此把安装请求分流到 InstallReleaseBinary。用注册表判断而不是在 web 层
// 再抄一份 ID 列表：抄的那份在加新条目时一定会漏（漏了的后果是市场点了没反应）。
func IsReleaseBinaryApp(id string) bool {
	_, ok := releaseBinaryApps[id]
	return ok
}

// hostOf 取 URL 的主机名，用于日志（不要把完整 URL 塞进一行日志）。
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// UninstallReleaseBinary 停止并删除服务，按需删除安装目录。
//
// 实现体是 m.UninstallByDescriptor（见 tarball_descriptor.go）——
// 老函数名保留是为了 internal/services/uninstall_app.go 的分流与既有单测，
// 流程只有一份。
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

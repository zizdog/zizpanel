package services

// ============================================================================
//  应用市场的**下载点声明**
//
//  用户的原话（2026-09-16）：
//    "对于应用市场，应该有能用高效的工作流，如果以后每加一个应用都要一点一点
//     慢慢调试，那这个应用市场就没什么实用价值了。"
//
//  这一份文件就是那个"工作流"的地基：把"每个应用安装时到底要从网上拉什么、
//  镜像站上有没有、超时是多少、arm64 证据在哪"变成**声明**，于是"加一个应用"
//  等于"填空"，而不是"一个一个踩坑"。
//
//  三层结构（见 docs/新增应用工作流.md）：
//    第 1 层  本文件            —— 声明（填空的地方）
//    第 2 层  MarketInvariantProblems  —— 静态门禁（不联网，进 go test / make check）
//    第 3 层  cmd/zizpanel-assets audit —— 在线审计（一条命令审全部在售条目）
//    第 4 层  文档              —— 操作手册
//
//  三条不许违反的原则：
//    1. **不许把缺口写成"不需要"。** NAS 的处置必须三选一（mirrored / missing /
//       not_needed），选 not_needed 必须给出经得起看的理由（带实测数字或架构事实）；
//       实测下来"该镜像但还没镜像"的，必须老实写 missing —— 审计会把它报成 ❌。
//    2. **不许编造 sha256 / 体积 / 平台。** 拿不到就留空并在 Note 里写"未验证"。
//       本仓库因为编造 sha256 出过"镜像永不命中、静默回落慢源"的事故。
//    3. **目录改了、声明没改 → 测试必须失败。** 反漂移见 market_downloads_test.go：
//       它从 services.Catalog() 读**真实目录**逐字段比对（只读，不改）。
//
//  数据来源：docs/应用市场下载点清点.md（559 行只读取数报告，含真实实测数字与
//  file:line），以及写这份声明时**重新核对过**的当前代码（并发代理在改
//  catalog.go / colima.go / api_site_apps.go，所以以代码现状为准，不抄旧报告）。
// ============================================================================

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// MarketFetchKind 是一个下载点的**用途**。
//
// 这些字符串会写进 `make market-audit --json` 的输出，是对外契约：审计工具、
// 以后的 CI、以及人读的体检报告都按它分组。改名等于改协议。
type MarketFetchKind string

const (
	// MarketFetchBrewBottle 是 Homebrew 二进制瓶（含整棵依赖闭包）。
	MarketFetchBrewBottle MarketFetchKind = "brew_bottle"
	// MarketFetchBrewInstaller 是 Homebrew 的安装脚本（raw.githubusercontent）。
	MarketFetchBrewInstaller MarketFetchKind = "brew_installer"
	// MarketFetchBrewGit 是 brew 自身的 git 源（clone/fetch）。
	MarketFetchBrewGit MarketFetchKind = "brew_git"
	// MarketFetchReleaseBinary 是 GitHub release 上的 darwin-arm64 原生产物。
	MarketFetchReleaseBinary MarketFetchKind = "release_binary"
	// MarketFetchDockerImage 是容器镜像（必须自带 linux/arm64，铁律②）。
	MarketFetchDockerImage MarketFetchKind = "docker_image"
	// MarketFetchPipPackage 是 PyPI 包（wheel / sdist）。
	MarketFetchPipPackage MarketFetchKind = "pip_package"
	// MarketFetchModelFile 是模型权重文件。
	MarketFetchModelFile MarketFetchKind = "model_file"
	// MarketFetchSiteSource 是"一键建站"的站点源码包。
	MarketFetchSiteSource MarketFetchKind = "site_source"
	// MarketFetchCLT 是 Xcode Command Line Tools（安装包 + 分片）。
	MarketFetchCLT MarketFetchKind = "clt"
	// MarketFetchVMImage 是 Colima/Lima 的虚拟机磁盘镜像。
	MarketFetchVMImage MarketFetchKind = "vm_image"
)

// MarketNASState 是一个下载点在**镜像站**上的处置。
//
// 为什么必须三态而不是"有路径 / 没路径"：没有路径有两种完全不同的情况 ——
// "镜像了也没有收益"（脚本只有 33 KB、直连 0.5 s）与"该镜像但还没镜像"
// （站点源码、ollama 模型）。把它们混成一种，就会出现用户最反感的那种
// "为了审计通过而把缺口写成不需要"。所以 missing 与 not_needed 分开，
// 前者审计一律报 ❌。
type MarketNASState string

const (
	// NASMirrored：镜像站上有确定路径，审计会真的去 HEAD/Range GET 它。
	NASMirrored MarketNASState = "mirrored"
	// NASMissing：**该镜像但还没镜像** —— 明确的缺口，审计报 ❌。
	NASMissing MarketNASState = "missing"
	// NASNotNeeded：镜像了也没有收益（理由必须带实测数字或架构事实），审计放行。
	NASNotNeeded MarketNASState = "not_needed"
)

// MarketNAS 回答"这个下载点 NAS 优先吗？"。
type MarketNAS struct {
	State MarketNASState
	// Path 是**镜像站相对路径**（相对镜像基址，不以 / 开头）。
	// State==NASMirrored 时必填；其余情况留空。
	Path string
	// Reason 是"为什么不需要 / 为什么还没有"。
	// State!=NASMirrored 时必填，且必须是**经得起看**的理由（≥20 字，带实测数字
	// 或架构事实）。写"不需要"三个字会被静态门禁拦下。
	Reason string
}

// MarketUpstream 描述上游来源。
type MarketUpstream struct {
	// ID 是上游标识：brew 的 formula 名 / 镜像名 / github repo@tag / PyPI 包名 /
	// 模型 repo；其它情况直接写 URL。
	ID string
	// URL 是"上游真实可达性"要探测的地址。空 = 这一步没有单一上游 URL
	// （例如 brew 瓶要由 brew 自己按 formula 解析），此时 Note 要写清为什么。
	URL string
	// Repo / Tag / Asset 是 GitHub release 三元组（release_binary / vm_image 用）。
	// 声明了 Repo 时：URL 必须等于 releaseURL() 拼出来的官方地址（防手抄错），
	// 且 release 二进制条目还要与 ReleaseBinaryAssets() 注册表交叉校验。
	Repo, Tag, Asset string
	// Size 是已知字节数；**0 = 未知（不许猜）**。非 0 时必须在 Note 里写清
	// 是"实测"还是"上游声明"。
	Size int64
	// Note 是来源与限制（实测数字、回落顺序、已知不一致）。
	Note string
}

// MarketChecksum 描述一个下载点的内容校验现状。
//
// 为什么 SHA256 与 Source 必须成对：单写一个 sha256 没写来源，下一个会话
// 就无从判断它是实测的、上游声明的、还是手抄错的 —— 这个仓库因为
// "编造 sha256 导致镜像永不命中"出过事故，所以来源是字段，不是注释。
type MarketChecksum struct {
	// Asset 是上游/镜像提供的校验清单文件名（如 "checksums.txt"）；
	// 空 = 没有清单文件（此时 SHA256 或 Note 必须至少有一个）。
	Asset string
	// SHA256 是**已取得的完整**期望值（小写 hex）；空 = 本次没有可信值。
	SHA256 string
	// Source 是 SHA256 的来源；SHA256 非空时必填。
	Source string
	// UpstreamFile 是**上游校验清单的文件名**（如 "checksums.txt" /
	// "frp_sha256_checksums.txt"）；空 = 上游没有清单文件。
	// 在线审计据此去上游取清单、与镜像站上的文件实算比对。
	UpstreamFile string
	// Note 说明现状与缺口。"没有校验"这件事必须写出来，不许留空默认通过。
	Note string
}

// MarketDownloadPoint 是**一个网络下载点**（一步会去网上取东西的命令）。
type MarketDownloadPoint struct {
	// Purpose 是用途（brew 瓶 / release 产物 / 镜像 / pip 包 / 模型 / 站点源码 / CLT / VM 镜像）。
	Purpose MarketFetchKind
	// Label 是人读的一步，会直接出现在 `make market-audit` 的表里与失败说明里。
	Label string
	// Upstream 是上游来源。
	Upstream MarketUpstream
	// NAS 回答"NAS 优先吗"。
	NAS MarketNAS
	// Timeout 是代码里**真实存在**的超时。
	// 0 = 真的没有超时，此时必须写 TimeoutReason（历史上的"没有超时 → 永远挂住"
	// 就是这么来的；把它显式写出来，审计报 ⚠️，而不是假装有超时）。
	Timeout time.Duration
	// TimeoutReason 说明为什么可以没有独立超时；Timeout==0 时必填。
	TimeoutReason string
	// Required 表示这一步失败会不会让安装失败。
	Required bool
	// OptionalImpact 是"可选步骤失败会少掉什么"；Required==false 时必填。
	// 为什么必须有：iopaint 的 ffmpeg 失败**只写 warning**，用户看到的"装好了"
	// 其实是"视频去水印不可用"—— 这种降级必须写进声明，否则没人知道。
	OptionalImpact string
	// Checksum 是内容校验现状。
	Checksum MarketChecksum
	// ARM64 是"这个下载点在 arm64 上可用"的证据（怎么验证的，不是"应该可以"）。
	// 空 = 未验证（审计报 ⚠️）。compose 条目必须非空（铁律②）。
	ARM64 string
	// Note 补充说明（回落顺序、已知不一致、坑）。
	Note string
}

// MarketRuntimeMode 是"装完之后它是怎么被管起来的"。
type MarketRuntimeMode string

const (
	// MarketRuntimeLaunchd：有守护进程（面板自研安装器或 brew services 注册的 launchd 服务）。
	MarketRuntimeLaunchd MarketRuntimeMode = "launchd"
	// MarketRuntimeContainer：docker compose 容器，由 docker compose 管。
	MarketRuntimeContainer MarketRuntimeMode = "container"
	// MarketRuntimeSite：一键建站，产出是一个网站，没有常驻服务。
	MarketRuntimeSite MarketRuntimeMode = "site"
	// MarketRuntimeNone：**没有常驻进程**（phpMyAdmin 是 nginx alias + php-fpm，
	// ffmpeg 是个命令行工具）。
	MarketRuntimeNone MarketRuntimeMode = "none"
)

// MarketRuntime 回答"服务语义"（第 2 层的第 3 条不变量）。
type MarketRuntime struct {
	Mode MarketRuntimeMode
	// Label 是运行期真实的 launchd label（Mode==launchd 时填）；其余模式留空。
	Label string
	// LabelSource 是 label 的证据来源（本机 launchctl / 仓库常量 / 目录字段）。
	LabelSource string
	// CatalogGap 非空 = 目录里**没有**声明 ServiceLabel 也没有 NoDaemon，
	// 这里必须写清"运行期靠什么兜底、要补哪里"。空 = 与目录一致。
	CatalogGap string
}

// MarketApp 是一个目录条目的完整下载点声明。
type MarketApp struct {
	// ID 必须与目录里的 ID 完全一致。
	ID string
	// Kind / BrewFormula / PanelInstaller / ServiceLabel / NoDaemon / ComposeImage
	// 都**必须与目录一致**（反漂移比对，见 market_downloads_test.go）。
	Kind           Kind
	BrewFormula    string
	PanelInstaller string
	ServiceLabel   string
	NoDaemon       bool
	// ComposeImage 是**单容器** compose 里 `image:` 那一行。
	// 多容器 compose（Activepieces / Immich）改用 ComposeImages 列全部镜像。
	// 两者至少有一个（Kind==KindCompose 时），且必须与目录 compose 里的镜像集合一致。
	ComposeImage string
	// ComposeImages 是多容器 compose 的全部镜像；单容器条目留空、用 ComposeImage。
	// 反漂移比对会把两边归一化成集合，并额外要求每个镜像都有一条 docker_image 下载点。
	ComposeImages []string
	// Runtime 说明运行期怎么被管。
	Runtime MarketRuntime
	// Downloads 是全部网络下载点，**按执行顺序**。
	Downloads []MarketDownloadPoint
	// Note 是补充说明（没有下载点的条目也要说清为什么）。
	Note string
}

// ---------------------------------------------------------------------------
//  构造小工具：同类下载点的处置保持一致，避免逐个手抄抄出漂移。
//  （这些只是**写法**上的复用：展开后的数据仍然要逐字段过反漂移测试。）
// ---------------------------------------------------------------------------

func nasMirrored(path string) MarketNAS { return MarketNAS{State: NASMirrored, Path: path} }

func nasMissing(reason string) MarketNAS { return MarketNAS{State: NASMissing, Reason: reason} }

func nasNotNeeded(reason string) MarketNAS { return MarketNAS{State: NASNotNeeded, Reason: reason} }

// brewBottlePoint 造一个 Homebrew 瓶下载点（所有 brew 应用共用同一条处置：
// 源由 brewEnv/probeBrewMirrors 决定，镜像站的 /brew 是第一个候选）。
func brewBottlePoint(formula string, timeout time.Duration, label string) MarketDownloadPoint {
	return MarketDownloadPoint{
		Purpose: MarketFetchBrewBottle,
		Label:   label,
		Upstream: MarketUpstream{
			ID: formula,
			Note: "brew install " + formula + "；实际会拉整棵依赖闭包（ffmpeg 一条 ≈ 15 个瓶）。" +
				"源由 brewEnv/probeBrewMirrors 决定：镜像站 <base>/brew → 中科大 → 清华 → 阿里云 → 官方 ghcr.io",
		},
		NAS:      nasMirrored("brew"),
		Timeout:  timeout,
		Required: true,
		Checksum: MarketChecksum{Asset: "Homebrew bottle 的 OCI manifest（brew 自己按 sha256 校验每个瓶）"},
		ARM64: "brew 按本机架构选瓶的 tag（arm64_sequoia / arm64_tahoe 等）；" +
			"镜像站 /brew 的 _cache 里已实测有 arm64 瓶文件（php@8.2 21,158,472 B、mysql@8.4 82,880,352 B）",
		Note: "`brew services start` 失败只写 warning 不报错（历史上表现为'装好了但服务没起'）",
	}
}

// pipPoint 造一个 PyPI 下载点（两个 Python 应用都固定走清华源）。
func pipPoint(pkg string, timeout time.Duration, label, note string) MarketDownloadPoint {
	simple := strings.ToLower(strings.Split(pkg, "[")[0])
	return MarketDownloadPoint{
		Purpose: MarketFetchPipPackage,
		Label:   label,
		Upstream: MarketUpstream{
			ID:   pkg,
			URL:  qwenPipMirror + "/" + simple + "/",
			Note: "固定走清华源（常量 qwenPipMirror）；" + note,
		},
		NAS: nasMissing("镜像站没有 /pypi 入口（实测 <base>/pypi/simple/ → 404），" +
			"两个 Python 应用恒走清华源。这是**缺口**不是'不需要'：NAS 化要先给镜像站加 pypi 反代。" +
			"当前不致命（实测 iopaint sdist 2,952,739 B / 4.6 MB/s），但它是「镜像优先」原则上的一个洞"),
		Timeout:  timeout,
		Required: true,
		Checksum: MarketChecksum{Asset: "PyPI 的 sha256（pip 自己按摘要核对每个 wheel / sdist）"},
		ARM64: "纯 Python 或 macosx_arm64 wheel：iopaint 已核实是纯 Python 打包（sdist 里没有 ext_modules）；" +
			"mlx-audio 的依赖 mlx / miniaudio / scipy / numpy 都有 cp311 macosx_arm64 wheel（实测清华上有 139 个 torch arm64 轮子）。" +
			"例外：webrtcvad 只有 sdist，必须现场用 CLT 里的 clang 编",
		Note: note,
	}
}

// dockerImagePoint 造一个容器镜像下载点。
//
// 2026-09-17 起 Docker 类条目都是**推荐项目**（面板不再代安装），但这些镜像
// 仍然要同步到镜像站：用户会从那份预配置 compose 自己 `docker compose up -d`，
// 镜像先从镜像站拉。所以声明保留，只是 Note 里写清新语义。
func dockerImagePoint(image string, timeout time.Duration, nas MarketNAS, note string) MarketDownloadPoint {
	return MarketDownloadPoint{
		Purpose: MarketFetchDockerImage,
		Label:   "docker compose up -d（拉 " + image + "）",
		Upstream: MarketUpstream{
			ID: image,
			Note: "用户取用镜像站 <base>/compose/<id>/docker-compose.yml，" +
				"改完自行 docker compose up -d（面板自 2026-09-17 起不再代装；" +
				"安装接口对这些条目返回 4xx 并给出该地址）",
		},
		NAS:      nas,
		Timeout:  timeout,
		Required: true,
		Checksum: MarketChecksum{Asset: "镜像 manifest 的 digest（docker 按 digest 校验每一层，无需另存 sha256）"},
		ARM64:    note,
		Note: "compose 文件里绝不写 `platform:`（铁律②，测试锁死）；arm64 由镜像自带。" +
			"**该条目是推荐项目**：镜像仍供拉取/镜像准备，但面板不再自动安装 —— " +
			"这条 Note 与 compose 参考文件一起说清新语义",
	}
}

// ---------------------------------------------------------------------------
//  声明本体：全部在售条目，逐个列全部网络下载点（条数以 Catalog() 为准，别在这里写死）。
// ---------------------------------------------------------------------------

var marketDownloadApps = []MarketApp{
	// ---------------- 面板一键部署（自研安装器） ----------------

	{
		ID: "iopaint", Kind: KindNative, PanelInstaller: "iopaint", ServiceLabel: "com.zizdog.iopaint",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "com.zizdog.iopaint",
			LabelSource: "目录 ServiceLabel（与声明一致）",
		},
		Downloads: []MarketDownloadPoint{
			// 用 panelPythonFormula 而不是字面量：声明必须跟着预置版本走，
			// 否则换版本时会出现"代码装 3.12、声明写 3.11"的静默不一致。
			brewBottlePoint(panelPythonFormula, 20*time.Minute,
				"brew install "+panelPythonFormula+"（面板自研运行时的预置版本）"),
			pipPoint("pip", 5*time.Minute, "pip install -U pip", "先把 pip 自己升到最新，避免旧 pip 解析不出新的 wheel tag"),
			pipPoint("iopaint", 40*time.Minute, "pip install iopaint",
				"PyPI/清华上 iopaint 只有 sdist（实测 iopaint-1.6.0.tar.gz 2,952,739 B），"+
					"但已核实是纯 Python 打包（setup.py 只有 find_packages/install_requires，无 ext_modules/Cython/cffi）；"+
					"依赖树含 torch>=2.0.0 / opencv-python / diffusers 等，**总下载量未实测**（代码注释只写'约 1~2GB'）"),
			{
				Purpose: MarketFetchModelFile,
				Label:   "下载 big-lama.pt（NAS 优先）",
				Upstream: MarketUpstream{
					ID:   "Sanster/models@add_big_lama/big-lama.pt",
					URL:  "https://github.com/Sanster/models/releases/download/add_big_lama/big-lama.pt",
					Repo: "Sanster/models", Tag: "add_big_lama", Asset: "big-lama.pt",
					Size: 205669692,
					Note: "实测 Content-Length = 205,669,692 B，与代码常量 iopaintWeightSize 完全一致；" +
						"GitHub 直连实测 110,992 B/s → 约 31 min，**超过它自己的 30 min 上限** → 走镜像站才是可行路径",
				},
				NAS:      nasMirrored("models/iopaint/big-lama.pt"),
				Timeout:  30 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{
					Source: "上游只公布 MD5 e3aa4aaa15225a33ec84f9f4bc47e500（代码常量），平台没有 sha256",
					Note:   "生产路径只按 MD5 校验；iopaintWeightSize 在生产代码里从不被读取（只有测试引用）—— 体积不校验",
				},
				ARM64: "权重文件与架构无关（任何架构同一份）",
				Note: "落盘 <userHome>/.cache/torch/hub/checkpoints/big-lama.pt；" +
					"另有 90 s「连续零字节即判停滞」看门狗，比外层 30 min 更早发现问题",
			},
			{
				Purpose: MarketFetchBrewBottle,
				Label:   "brew install ffmpeg（可选）",
				Upstream: MarketUpstream{
					ID:   "ffmpeg",
					Note: "与 ffmpeg 条目同一条路（basedep.go 的 EnsureBaseDependencies）",
				},
				NAS:            nasMirrored("brew"),
				Timeout:        30 * time.Minute,
				Required:       false,
				OptionalImpact: "视频去水印不可用；面板只写 warning、**不中止安装**，所以用户看到的「装好了」里少了一部分能力",
				Checksum:       MarketChecksum{Asset: "Homebrew bottle 的 OCI manifest"},
				ARM64:          "同 brew：按本机架构选瓶",
			},
		},
	},

	{
		ID: "qwen3tts", Kind: KindNative, PanelInstaller: "qwen3tts", ServiceLabel: "com.zizdog.qwen3tts",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "com.zizdog.qwen3tts",
			LabelSource: "目录 ServiceLabel（与声明一致）",
		},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchCLT,
				Label:   "EnsureCLT：镜像整包 / softwareupdate / 弹窗 三条路",
				Upstream: MarketUpstream{
					ID:  "Command Line Tools 16.2（clt/index.json 清单）",
					URL: "https://mirror.zizdog.com:8888/zizpanel/clt/index.json",
					// 这一条声明的 Size **故意留 0**：URL 指向的是 index.json（清单本身只有
					// 1374 B），661,802,053 是它声明的**载荷总字节**（两个 pkg 之和）。
					// 把载荷体积挂在清单 URL 上，审计会报"体积变了"的假警报 ——
					// 体积写进 Note，别挂在错误的 URL 上。
					Note: "镜像整包走 <base>/clt/index.json（先 <base>/clt，再 <base>/zizpanel/clt，都不通回落静态常量 zizdog.com）；" +
						// 2026-09-17 用户定的分工：zizdog.com 只当**安装脚本与面板本体/在线升级**的源（数据量小）；
						// 市场里的软件、CLT（632MB）这类大件一律走**快镜像**（mirror.zizdog.com:8888 = NAS，
						// 实测 5.6–8.9MB/s；NAS 对 /zizpanel/ 有按需回源，首次取完即缓存）。
						// 实测：<mirror>/zizpanel/clt/index.json = 200（<mirror>/clt/index.json 是 404）。" +
						"清单 bytes=661,802,053（含 CLTools_Executables.pkg 604,642,024 + CLTools_macOSNMOS_SDK.pkg）；" +
						"镜像路失败还有 softwareupdate -l(3 min) / softwareupdate -i(40 min) / xcode-select --install 弹窗(1 min + 轮询 30 min) 两条退路",
				},
				NAS:      nasMirrored("zizpanel/clt/index.json"),
				Timeout:  45 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{
					Asset: "index.json 里逐文件的 sha256（清单自带两条）",
					Note:  "⚠️ 已知不一致：homebrew_clt_mirror.go 注释宣称「每片单独校验」，实际 fetchParts 只校验**大小**，没有逐片 sha256",
				},
				ARM64: "pkg 是通用二进制（上游同一份给 Apple Silicon 与 Intel）；清单 max_os=15 只覆盖到 macOS 15",
				Note: "⚠️ 清单只有一条 max_os=15：目标机若是 macOS 26（Tahoe）会选不中条目 → 静默回落苹果 CDN（历史记录：15 分钟只下 1 MB）。" +
					"分片路径 20 min/片 × 4 路并行、单文件路径 45 min；清单本身 30 s（fetchSmallText）",
			},
			{
				Purpose: MarketFetchBrewInstaller,
				Label:   "下载 Homebrew 安装脚本",
				Upstream: MarketUpstream{
					ID:   "Homebrew/install@HEAD/install.sh",
					URL:  "https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh",
					Size: 33604,
					Note: "候选顺序：官方 raw.githubusercontent → ghfast.top → gh-proxy.com（homebrew.go 里的常量）；" +
						"官方实测 200 / 0.502 s / 33,604 B",
				},
				NAS: nasNotNeeded("脚本只有 33,604 B，官方直连实测 200 / 0.5 s，且代码已有 ghfast.top / gh-proxy.com 两条回落；" +
					"把它搬上镜像站没有收益（真正的大件是 CLT 与 brew 瓶，那两个已经镜像）"),
				Timeout:  120 * time.Second,
				Required: true,
				Checksum: MarketChecksum{
					Note: "上游没有 checksum 文件；代码也不校验（curl -fsSL 直下直执行）。这是一个**诚实的缺口**：" +
						"若上游脚本被篡改，面板无从发现 —— 与 brew 官方安装流程同样的暴露面",
				},
				ARM64: "shell 脚本，与架构无关",
			},
			{
				Purpose: MarketFetchBrewGit,
				Label:   "执行 Homebrew 安装脚本（git 源）",
				Upstream: MarketUpstream{
					ID:  "mirrors.ustc.edu.cn/brew.git",
					URL: "https://mirrors.ustc.edu.cn/brew.git/info/refs?service=git-upload-pack",
					Note: "git 源固定为中科大/清华（homebrew.go 里的常量，**不读 MirrorBase**）；" +
						"实测中科大 info/refs 200 / 0.175 s",
				},
				NAS: nasNotNeeded("中科大 / 清华的 git 源实测 200 / 0.175 s，比镜像站经公网 DDNS 还快；" +
					"镜像站没有 git 反代，也没有必要加（这是 git 协议不是静态文件）"),
				Timeout: 0,
				TimeoutReason: "执行安装脚本这一条**没有独立超时**，只受任务 ctx 约束（homebrew.go 裸 exec.CommandContext）。" +
					"如实声明：脚本卡住时只能靠用户取消任务。它是历史上那类'没有超时 → 永远挂住'的直接残留",
				Required: true,
				Checksum: MarketChecksum{
					Note: "git fetch 的完整性由 git 自己按对象哈希保证",
				},
				ARM64: "shell 脚本 + git，与架构无关",
			},
			// 用 panelPythonFormula 而不是字面量：声明必须跟着预置版本走，
			// 否则换版本时会出现"代码装 3.12、声明写 3.11"的静默不一致。
			brewBottlePoint(panelPythonFormula, 20*time.Minute,
				"brew install "+panelPythonFormula+"（面板自研运行时的预置版本）"),
			pipPoint("pip", 5*time.Minute, "pip install -U pip", "只升级 pip 自己"),
			pipPoint("mlx-audio[server]", 40*time.Minute, "pip install mlx-audio[server]",
				"实测 mlx-audio 本身有 py3-none-any 纯 wheel；但 server extra 的依赖 webrtcvad **只有 .tar.gz、没有任何 wheel** → "+
					"现场用 C 编译器构建（mlx-audio 特意 pin setuptools<81），所以这一步依赖第 1 步装好的 CLT 里的 clang"),
			{
				Purpose: MarketFetchModelFile,
				Label:   "hf download Qwen3-TTS-12Hz-1.7B-Base-8bit（NAS 优先）",
				Upstream: MarketUpstream{
					ID:   "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
					URL:  qwenHFMirror + "/mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit/resolve/main/model.safetensors",
					Size: 2417320525,
					Note: "端点由 qwenHFEndpoint 决定：MirrorEnabled 时只 HEAD <base>/hf/（4 s 超时），通就用镜像站，否则 hf-mirror.com；" +
						"model.safetensors 实测 2,417,320,525 B（整份模型目录 du -sh = 2.9G）；" +
						"镜像站 LAN 读 26.8 MB/s ≈ 1.8 min，hf-mirror 直连 739 KB/s ≈ 65 min",
				},
				// 镜像站上的相对路径就是 HF 的文件路径（<endpoint>/<repo>/resolve/main/<file>），
				// 这样审计能直接 HEAD 到**那一份权重**，而不是只探 /hf/ 根
				// （只探根正是"镜像通但缺这个 repo 却不回落"那个坑的根源）。
				NAS:      nasMirrored("hf/mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit/resolve/main/model.safetensors"),
				Timeout:  40 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{
					Asset:  "HF 上的文件 ETag / sha256（hf download 自己核对）",
					Source: "huggingface_hub 内置校验（下载后按 repo 里记录的哈希核对）",
					Note:   "面板自己不校验；已实测镜像站 _cache/hf-mirror.com 里这份模型是整份缓存的",
				},
				ARM64: "权重文件与架构无关；mlx 运行时只支持 Apple Silicon（这正是不走 Docker 的原因）",
				Note: "每次 40 min、最多 3 次、间隔 5 s。⚠️ 已知缺口：只探根路径，探通后 3 次重试全用同一个端点，" +
					"**不会因为'镜像站上没有这个 repo'回落 hf-mirror**（qwenHFEndpoint 只 HEAD /hf/ 根）",
			},
			brewBottlePoint("ffmpeg", 30*time.Minute, "brew install ffmpeg"),
		},
	},

	{
		ID: "voicereceiver", Kind: KindNative, PanelInstaller: "voicereceiver", ServiceLabel: "com.zizdog.voicereceiver",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "com.zizdog.voicereceiver",
			LabelSource: "目录 ServiceLabel（与声明一致）",
		},
		Note: "接收端脚本与默认音色都是 //go:embed（voicereceiver.go），安装过程本身**没有任何下载点**；" +
			"下面两条是它复用的公共依赖（与 Qwen 同一条路）",
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchCLT,
				Label:   "EnsureCLT（与 qwen3tts 同一条路）",
				Upstream: MarketUpstream{
					ID:   "Command Line Tools 16.2（clt/index.json 清单）",
					URL:  "https://zizdog.com/zizpanel/clt/index.json",
					Size: 0, // 同 qwen3tts：URL 指向清单（1374 B），661,802,053 是载荷总量，别挂错地方
					Note: "与 qwen3tts 的 CLT 完全同一条路（同一份清单、同一份 pkg；载荷 661,802,053 B）",
				},
				NAS:      nasMirrored("zizpanel/clt/index.json"),
				Timeout:  45 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{
					Asset: "index.json 里逐文件的 sha256",
					Note:  "同 qwen3tts：分片只校验大小，不校验逐片 sha256",
				},
				ARM64: "pkg 是通用二进制",
				Note:  "失败即中止安装",
			},
			brewBottlePoint("ffmpeg", 30*time.Minute, "brew install ffmpeg"),
		},
	},

	{
		ID: "phpmyadmin", Kind: KindNative, PanelInstaller: "phpmyadmin", BrewFormula: "phpmyadmin",
		NoDaemon: true,
		Runtime: MarketRuntime{
			Mode: MarketRuntimeNone, LabelSource: "目录 NoDaemon=true（nginx alias + php-fpm，没有自己的守护进程）",
		},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchBrewBottle,
				Label:   "brew install phpmyadmin",
				Upstream: MarketUpstream{
					ID: "phpmyadmin",
					Note: "代码注释说明为什么不用官网：files.phpmyadmin.net 国内完全不可达（本次未复测）；" +
						"镜像站 _cache 里实测有 phpmyadmin-5.2.3.all.bottle.1.tar.gz = 14,506,838 B",
				},
				NAS:      nasMirrored("brew"),
				Timeout:  15 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{Asset: "Homebrew bottle 的 OCI manifest"},
				ARM64:    "phpmyadmin 是 `all` 瓶（纯 PHP，无架构依赖）；依赖的 php 由本机 php@8.x 满足",
				Note:     "失败即中止安装（hard error）",
			},
		},
	},

	// ---------------- LNMP（Homebrew 原生） ----------------

	{
		ID: "nginx", Kind: KindNative, BrewFormula: "nginx", ServiceLabel: "homebrew.mxcl.nginx",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "homebrew.mxcl.nginx",
			LabelSource: "目录 ServiceLabel；catalog 的兜底匹配也认 sh.brew.nginx / cn.zizdog.nginx 这类同 formula 后缀",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("nginx", 30*time.Minute, "brew install nginx"),
		},
	},

	{
		ID: "php82", Kind: KindNative, BrewFormula: "php@8.2", ServiceLabel: "homebrew.mxcl.php@8.2",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "homebrew.mxcl.php@8.2",
			LabelSource: "目录 ServiceLabel",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("php@8.2", 30*time.Minute, "brew install php@8.2"),
		},
	},

	{
		ID: "php84", Kind: KindNative, BrewFormula: "php@8.4", ServiceLabel: "homebrew.mxcl.php@8.4",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "homebrew.mxcl.php@8.4",
			LabelSource: "目录 ServiceLabel",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("php@8.4", 30*time.Minute, "brew install php@8.4"),
		},
	},

	{
		ID: "mysql84", Kind: KindNative, BrewFormula: "mysql@8.4", ServiceLabel: "sh.brew.mysql@8.4",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "sh.brew.mysql@8.4",
			LabelSource: "目录 ServiceLabel（本机 LaunchAgent 就是这个文件名，实测存在）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("mysql@8.4", 30*time.Minute, "brew install mysql@8.4"),
		},
	},

	// ---------------- 基础环境里的数据库（PostgreSQL） ----------------

	{
		// 与 mysql84 完全对称的独立条目：**不跟任何应用绑定安装**。
		// 为什么不做成 Miniflux 的附属（会随安装顺序漂移、卸载语义两难）见
		// catalog.go 里 postgresql17 条目的注释。
		ID: "postgresql17", Kind: KindNative, BrewFormula: "postgresql@17", ServiceLabel: "sh.brew.postgresql@17",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "sh.brew.postgresql@17",
			LabelSource: "目录 ServiceLabel；formula 的 service 块存在（postgres -D /opt/homebrew/var/postgresql@17），" +
				"运行期由 brewServiceInfo 读真实 label（历史经验：本机 brew 写的是 sh.brew.<formula>）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("postgresql@17", 30*time.Minute, "brew install postgresql@17"),
		},
	},

	{
		ID: "ffmpeg", Kind: KindNative, BrewFormula: "ffmpeg", PanelInstaller: "ffmpeg",
		NoDaemon: true,
		Runtime: MarketRuntime{
			Mode: MarketRuntimeNone, LabelSource: "目录 NoDaemon=true（命令行工具，没有常驻进程）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("ffmpeg", 30*time.Minute, "brew install ffmpeg"),
		},
	},

	// 图片压缩（libvips）：用户 2026-09-18 要求上架。
	//
	// 下载点只有 Homebrew 瓶（`brew install vips`）：原生 arm64 包，装完就是
	// /opt/homebrew/bin/vips 一个命令行；面板自己的「文件管理 → 🖼️ 图片压缩」
	// 直接调它（为什么不用 govips/CGO：见 internal/imgopt 与 catalog.go 里的取舍）。
	// 运行期没有任何常驻进程、没有端口、没有网页界面。
	{
		ID: "imgcompress", Kind: KindNative, BrewFormula: "vips", PanelInstaller: "imgcompress",
		NoDaemon: true,
		Runtime: MarketRuntime{
			Mode:        MarketRuntimeNone,
			LabelSource: "目录 NoDaemon=true（命令行引擎，没有常驻进程；界面是面板自己的页面）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("vips", 30*time.Minute,
				"brew install vips（libvips 8.18.x，arm64 瓶；装完复核 `vips --version` 真的能跑）"),
		},
	},

	// ---------------- Python 解释器（基础环境，用户 2026-09-18 要求上架 3 个版本） ----------------
	//
	// 三个条目走**同一条**安装路径（PanelInstaller=python，见 python_runtime.go 的
	// InstallPythonRuntime）：brew 装 formula → 如实报告实际补丁版本 → 不注册任何服务。
	// 为什么必须显式声明 PanelInstaller：通用 brew 流程会去 `brew services start`
	// 一个没有 service 定义的 formula，得到"已安装但启动失败"的假警告 + 一条假服务记录。
	{
		ID: "python310", Kind: KindNative, BrewFormula: "python@3.10", PanelInstaller: "python",
		NoDaemon: true,
		Runtime: MarketRuntime{
			Mode:        MarketRuntimeNone,
			LabelSource: "目录 NoDaemon=true（解释器，没有常驻进程）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("python@3.10", 30*time.Minute,
				"brew install python@3.10（Intel Mac 与老项目用；3.12/3.13 上游没有 Intel macOS 瓶）"),
		},
	},
	{
		ID: "python311", Kind: KindNative, BrewFormula: "python@3.11", PanelInstaller: "python",
		NoDaemon: true,
		Runtime: MarketRuntime{
			Mode: MarketRuntimeNone,
			LabelSource: "目录 NoDaemon=true（解释器：没有常驻进程，装完是命令行工具；" +
				"用它建 venv 是各应用安装时的事）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("python@3.11", 30*time.Minute,
				"brew install python@3.11（面板自研运行时 Qwen3 TTS / IOPaint 的预置版本）"),
		},
	},
	{
		ID: "python312", Kind: KindNative, BrewFormula: "python@3.12", PanelInstaller: "python",
		NoDaemon: true,
		Runtime: MarketRuntime{
			Mode:        MarketRuntimeNone,
			LabelSource: "目录 NoDaemon=true（解释器，没有常驻进程）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("python@3.12", 30*time.Minute, "brew install python@3.12"),
		},
	},
	{
		ID: "python313", Kind: KindNative, BrewFormula: "python@3.13", PanelInstaller: "python",
		NoDaemon: true,
		Runtime: MarketRuntime{
			Mode:        MarketRuntimeNone,
			LabelSource: "目录 NoDaemon=true（解释器，没有常驻进程）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("python@3.13", 30*time.Minute, "brew install python@3.13"),
		},
	},

	// ---------------- AI 服务 ----------------

	{
		ID: "ollama", Kind: KindNative, BrewFormula: "ollama",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "homebrew.mxcl.ollama",
			LabelSource: "仓库注释记录的真机事实（catalog.go: " +
				"真机上记录是 homebrew.mxcl.ollama，磁盘上是 sh.brew.ollama）；本机当前没装 ollama 服务，**未在本机复核**",
			CatalogGap: "目录**没有**声明 ServiceLabel 也没有 NoDaemon（ollama 是有守护进程的服务）。" +
				"现在的兜底是 catalogEntryForLabel 按 BrewFormula 后缀反查（install.go），能工作但不显式 —— " +
				"要补的是 catalog.go 里 ollama 条目加 ServiceLabel/AdoptLabel（catalog.go 属另一个代理，本轮不动）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("ollama", 30*time.Minute, "brew install ollama"),
			{
				Purpose: MarketFetchModelFile,
				Label:   "ollama pull qwen2.5:7b（**面板不参与**）",
				Upstream: MarketUpstream{
					ID:   "registry.ollama.ai/library/qwen2.5:7b",
					URL:  "https://registry.ollama.ai/v2/library/qwen2.5/manifests/7b",
					Size: 4683073952,
					Note: "实测 manifest 里**最大的一个 layer** = 4,683,073,952 B（不是所有 layer 之和），" +
						"下载实测 701,063 B/s → 约 111 min；整仓只有 catalog 里一句 PostInstallHint 文案，面板不代拉、不看进度、失败也不知道",
				},
				NAS: nasMissing("镜像站没有缓存 registry.ollama.ai 的任何东西，面板也没有接管这一步的任何代码路径 —— " +
					"用户只能手工在终端等约 2 小时，且失败了面板不知道。这是**明确的缺口**（P1-4）"),
				Timeout: 0,
				TimeoutReason: "面板完全不参与这条命令（只有文案提示），所以面板里不存在它的超时；" +
					"如实声明而不是假装有超时。要么把模型镜像到 NAS 并接管进度，要么在界面上明说'这一步面板不管'",
				Required: true,
				Checksum: MarketChecksum{
					Asset:  "OCI manifest 的 layer digest",
					Source: "registry.ollama.ai 的 manifest（ollama 自己按 digest 校验）",
					Note:   "面板不校验（它不参与这一步）",
				},
				ARM64: "原生 arm64（Metal 加速）；这也是它不走 Docker 的原因",
				Note:  "面板的 PostInstallHint 只写了一句 ollama pull qwen2.5:7b，没有进度入口",
			},
		},
	},

	// ---------------- 容器运行时（Docker 类应用的前提） ----------------

	{
		ID: "docker-runtime", Kind: KindColima, PanelInstaller: "docker-runtime",
		BrewFormula: "colima", ServiceLabel: ColimaLaunchLabel,
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: ColimaLaunchLabel,
			LabelSource: "目录 ServiceLabel = 常量 ColimaLaunchLabel（colima.go）",
		},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchBrewBottle,
				Label:   "brew install colima docker docker-compose",
				Upstream: MarketUpstream{
					ID: "colima",
					Note: "一条 brew install 装三个 formula（colima / docker / docker-compose，compose 不在 colima 的依赖里，" +
						"但 compose 类应用要用它，所以一并装上）；源同 brew 公共路径",
				},
				NAS:      nasMirrored("brew"),
				Timeout:  15 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{Asset: "Homebrew bottle 的 OCI manifest"},
				ARM64:    "colima / docker CLI 都是 brew 的 arm64 瓶",
				Note:     "失败即中止（没有运行时，9 个 Docker 应用全废）",
			},
			{
				Purpose: MarketFetchVMImage,
				Label:   "colima start：guest 虚拟机镜像（NAS 预热，缺了才回 GitHub）",
				Upstream: MarketUpstream{
					ID:   "abiosoft/colima-core@v0.10.4/ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz",
					URL:  "https://github.com/abiosoft/colima-core/releases/download/v0.10.4/ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz",
					Repo: "abiosoft/colima-core", Tag: "v0.10.4", Asset: "ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz",
					Size: 332354401,
					Note: "实测 332,354,401 B（317 MiB gzip，解压 3.5 GB）；公网 GitHub 实测 77,477 B/s → 约 71.5 min。" +
						"版本号**不能写死**：colima 会把对应的 colima-core 版本内嵌进二进制（实测 colima 0.10.3 内嵌 v0.10.4），" +
						"PrewarmColimaGuestImage 是从 colima 二进制里正则抠出来的",
				},
				NAS:      nasMirrored("apps/colima-core/v0.10.4/ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz"),
				Timeout:  90 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{
					Asset:  "<file>.sha512sum（官方 release 里就有，镜像站原样搬）",
					Source: "官方 release 的 sha512sum 文件（PrewarmColimaGuestImage 逐字节核对）",
					Note:   "本机缓存文件 b0992ab8… 与 URL 的 sha256 相等、内容 sha512 与官方一致（仓库注释记录的实测）",
				},
				ARM64: "arm64 专用的 Ubuntu cloud image（amd64 那份是另一个 358 MB 的文件）；Colima 用 Virtualization.framework 原生跑",
				Note: "⚠️ 同步脚本要从 colima 二进制取版本，不能写死 v0.10.4 —— 升级 colima 后 URL 里的版本号会变。" +
					"历史坑：旧代码只给 colima start 5 min 超时（对 71.5 min 的下载必然掐断），现在已改为 colimaStartTimeout = 90 min 并**失败即报错**",
			},
		},
	},

	// ---------------- 运维工具（Docker compose） ----------------

	{
		ID: "uptime-kuma", Kind: KindCompose, ComposeImage: "louislam/uptime-kuma:2",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose（docker compose 管）"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("louislam/uptime-kuma:2", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据（2026-09-17 经自建 NAS 的 /docker pull-through 读同一份 OCI index）："+
					"tag `:2` 有 linux/amd64、linux/arm64、linux/arm/v7；"+
					"同时核实 **`:v2` 这个 tag 不存在（manifest 404）**，`latest` 也不是 v2 —— 只能用 `:2`。"+
					"1.x 已停止维护（应用自己会告警 \"1.23.17 is a v1 tag\"），compose 里不写 platform（铁律②）"),
		},
		Note: "v1 → v2 会**自动迁移数据库**（不可逆），目录 Description 与 UI.Note 里都写了提醒；" +
			"原有的 5 条子路径改写是照 v1 调的，v2 是否仍适用**未重测**。",
	},

	{
		ID: "gitea", Kind: KindCompose, ComposeImage: "gitea/gitea:latest",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("gitea/gitea:latest", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据：Docker Hub 的 index 里有 linux/arm64（实测 arm64 层合计 66.1 MiB）"),
		},
	},

	{
		ID: "stirling-pdf", Kind: KindCompose, ComposeImage: "stirlingtools/stirling-pdf:latest",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("stirlingtools/stirling-pdf:latest", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据：Docker Hub 的 index 里有 linux/arm64（实测 arm64 层合计 977.4 MiB —— **全部条目里最大**，"+
					"20 min 超时是否够取决于实际带宽，未实测完整 pull）"),
		},
	},

	{
		ID: "it-tools", Kind: KindCompose, ComposeImage: "ghcr.io/corentinth/it-tools:latest",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("ghcr.io/corentinth/it-tools:latest", 20*time.Minute,
				nasNotNeeded("镜像站的 /docker 只反代 Docker Hub，不覆盖 ghcr.io；ghcr.io /v2/ 实测 405 / 0.66 s 直连可达，"+
					"arm64 层合计只有 22.5 MiB。（诚实标注：未实测完整 pull 的吞吐）"),
				"arm64 证据：ghcr.io 的 index 里有 linux/arm64（实测层合计 22.5 MiB）"),
		},
	},

	{
		ID: "filebrowser", Kind: KindCompose, ComposeImage: "filebrowser/filebrowser:latest",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("filebrowser/filebrowser:latest", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据：Docker Hub 的 index 里有 linux/arm64（实测 arm64 层合计 15.2 MiB；另有 arm/v7）"),
		},
	},

	{
		ID: "metatube-server", Kind: KindCompose, ComposeImage: "ghcr.io/metatube-community/metatube-server:latest",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("ghcr.io/metatube-community/metatube-server:latest", 20*time.Minute,
				nasNotNeeded("镜像站的 /docker 只反代 Docker Hub，不覆盖 ghcr.io；ghcr.io /v2/ 实测 405 / 0.66 s 直连可达，"+
					"arm64 层合计只有 19.3 MiB。（诚实标注：未实测完整 pull 的吞吐）"),
				"arm64 证据：ghcr.io 的 index 里有 linux/arm64（实测层合计 19.3 MiB）"),
		},
	},

	{
		ID: "squoosh", Kind: KindCompose, ComposeImage: "pjmeca/squoosh:1.1.0",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Note: "目录里最麻烦的一个：上游没有官方镜像（GoogleChromeLabs/squoosh 仓库没有任何 Dockerfile），" +
			"用的是社区镜像 pjmeca/squoosh:1.1.0（固定版本，不跟 latest 漂）",
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("pjmeca/squoosh:1.1.0", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据：Docker Hub 经加速源读同一份 index → linux/amd64、linux/arm64、linux/arm/v7（实测层合计 30.8 MiB）。"+
					"⚠️ daocloud 明确拒绝该镜像（DENIED … not in the allowlist），1panel / dockerproxy 能服务它 —— "+
					"所以 Docker 加速源必须是**有序列表**且不止一条，只配 daocloud 会装不上"),
		},
	},

	// ---------------- GitHub release 原生二进制 ----------------

	{
		ID: "frpc", Kind: KindNative, PanelInstaller: "frpc", ServiceLabel: "com.zizdog.frpc",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "com.zizdog.frpc",
			LabelSource: "目录 ServiceLabel（与 releaseBinaryApps 注册表里的 frpcLabel 一致）",
		},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchReleaseBinary,
				Label:   "下载 frp_0.71.0_darwin_arm64.tar.gz",
				Upstream: MarketUpstream{
					ID:   "github.com/fatedier/frp@v0.71.0/frp_0.71.0_darwin_arm64.tar.gz",
					URL:  "https://github.com/fatedier/frp/releases/download/v0.71.0/frp_0.71.0_darwin_arm64.tar.gz",
					Repo: "fatedier/frp", Tag: "v0.71.0", Asset: "frp_0.71.0_darwin_arm64.tar.gz",
					Size: 12680181,
					Note: "实测 12,680,181 B；直连实测 66,554 B/s → 190 s，**超过官方源 150 s 上限** → 自动换 gh-proxy（约 4 s）。" +
						"候选顺序：镜像站（无测速直下）→ 官方 + ghfast.top + gh-proxy.com 按实测速度重排",
				},
				NAS:      nasMirrored("apps/frpc/v0.71.0/frp_0.71.0_darwin_arm64.tar.gz"),
				Timeout:  150 * time.Second,
				Required: true,
				Checksum: MarketChecksum{
					Asset:        "上游 checksums 清单 + 镜像 manifest.json",
					UpstreamFile: frpChecksumAsset,
					SHA256:       "45be02b186860d375ed49a8941ae9569628a54bf14e67fc36b29c98c99dabcc6",
					Source:       "镜像站 manifest.json 声明；2026-09-16 把整包从 NAS 下下来实算 sha256 **完全一致**（清点报告记录）",
					Note:         "解压前校验，另外还有 file -b 复核 Mach-O arm64",
				},
				ARM64: "上游 release 资产名自带 darwin_arm64；实测解压出的二进制 file(1) 报 Mach-O arm64",
				Note:  "⚠️ 已知不一致：--max-time 按**位置**判定（i==0 → 150 s），镜像排在第一位时只拿到 150 s，且日志把它标成「官方地址」",
			},
		},
	},

	{
		ID: "orbien-client", Kind: KindNative, PanelInstaller: "orbien-client", ServiceLabel: "com.zizdog.orbien-client",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "com.zizdog.orbien-client",
			LabelSource: "目录 ServiceLabel（与注册表 orbienClientLabel 一致）",
		},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchReleaseBinary,
				Label:   "下载 orbien_3.6.0_darwin_arm64.tar.gz",
				Upstream: MarketUpstream{
					ID:   "github.com/orbien-org/orbien@v3.6.0/orbien_3.6.0_darwin_arm64.tar.gz",
					URL:  "https://github.com/orbien-org/orbien/releases/download/v3.6.0/orbien_3.6.0_darwin_arm64.tar.gz",
					Repo: "orbien-org/orbien", Tag: "v3.6.0", Asset: "orbien_3.6.0_darwin_arm64.tar.gz",
					Size: 2104350,
					Note: "实测 2,104,350 B；直连 66,554 B/s → 约 32 s，能过 150 s 上限",
				},
				NAS:      nasMirrored("apps/orbien-client/v3.6.0/orbien_3.6.0_darwin_arm64.tar.gz"),
				Timeout:  150 * time.Second,
				Required: true,
				Checksum: MarketChecksum{
					Asset: "镜像 manifest.json（公网模式**没有**上游清单）",
					Note: "⚠️ 明确的缺口：上游没有 checksums 文件 → 公网模式**完全没有内容校验**（binary_release.go 里 ChecksumAsset 为空时直接 return nil）。" +
						"只要镜像站在就没事；镜像站不可达时，第三方加速源回来的字节不经任何校验",
				},
				ARM64: "上游资产名自带 darwin_arm64；实测解压出的 orbien --help 输出 'orbien client'，file(1) 报 Mach-O arm64",
				Note:  "与 frpc 同一条回落链",
			},
		},
	},

	{
		ID: "ddns-go", Kind: KindNative, PanelInstaller: "ddns-go", ServiceLabel: "com.zizdog.ddns-go",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "com.zizdog.ddns-go",
			LabelSource: "目录 ServiceLabel（与注册表一致）",
		},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchReleaseBinary,
				Label:   "下载 ddns-go_6.17.7_darwin_arm64.tar.gz",
				Upstream: MarketUpstream{
					ID:   "github.com/jeessy2/ddns-go@v6.17.7/ddns-go_6.17.7_darwin_arm64.tar.gz",
					URL:  "https://github.com/jeessy2/ddns-go/releases/download/v6.17.7/ddns-go_6.17.7_darwin_arm64.tar.gz",
					Repo: "jeessy2/ddns-go", Tag: "v6.17.7", Asset: "ddns-go_6.17.7_darwin_arm64.tar.gz",
					Size: 4386545,
					Note: "实测 4,386,545 B；直连 66,554 B/s → 约 66 s，能过 150 s 上限",
				},
				NAS:      nasMirrored("apps/ddns-go/v6.17.7/ddns-go_6.17.7_darwin_arm64.tar.gz"),
				Timeout:  150 * time.Second,
				Required: true,
				Checksum: MarketChecksum{
					Asset:        "上游 checksums 清单 + 镜像 manifest.json",
					UpstreamFile: "checksums.txt",
					Note: "清点报告只记录到 sha256 前缀 9dac9d82…，**完整值未记录** → 这里不写（不编造）；" +
						"在线审计会从上游 checksums.txt 取完整值再与镜像站上的文件实算比对",
				},
				ARM64: "上游资产名自带 darwin_arm64；实测 file(1) 报 Mach-O 64-bit executable arm64（仓库注释记录）",
				Note:  "与 frpc 同一条回落链",
			},
		},
	},

	// ---------------- 自托管应用（原生，2026-09-17 新增） ----------------

	{
		ID: "miniflux", Kind: KindNative, BrewFormula: "miniflux", PanelInstaller: "miniflux",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "homebrew.mxcl.miniflux",
			LabelSource: "目录没有 ServiceLabel（面板安装器自己 brew services start 后按 brewServiceInfo 读真实 label）；" +
				"formula 的 service 块实测存在（miniflux -c /opt/homebrew/etc/miniflux.conf）",
			CatalogGap: "目录条目**没有**声明 ServiceLabel —— 与 ollama 同一类情况：" +
				"安装器装完直接读 brew 的真实 label 并登记，不靠目录猜。" +
				"要补的是 catalog.go 里给 miniflux 加 ServiceLabel（本轮刻意不动，避免与 brew 实际写法漂移）",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("miniflux", 30*time.Minute, "brew install miniflux"),
			// 第二个下载点：Miniflux **只能**用 PostgreSQL，安装器会先确保它。
			// 如实单列，而不是藏在"依赖会自动装"这句话里 —— 审计要能看见这两次网络下载。
			brewBottlePoint("postgresql@17", 30*time.Minute,
				"brew install postgresql@17（Miniflux 的数据库，安装器会先确保它已安装并启动）"),
		},
	},

	{
		ID: "syncthing", Kind: KindNative, BrewFormula: "syncthing", PanelInstaller: "syncthing",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "homebrew.mxcl.syncthing",
			LabelSource: "目录没有 ServiceLabel（面板安装器 brew services start 后按 brewServiceInfo 读真实 label）；" +
				"formula 的 service 块实测存在（syncthing --no-browser --no-restart，官方就是无头设计）",
			CatalogGap: "同 miniflux：ServiceLabel 由安装器在运行期从 brew 读取后登记，目录里没写死",
		},
		Downloads: []MarketDownloadPoint{
			brewBottlePoint("syncthing", 30*time.Minute, "brew install syncthing"),
		},
	},

	{
		ID: "alist", Kind: KindNative, PanelInstaller: "alist", ServiceLabel: "com.zizdog.alist",
		Runtime: MarketRuntime{
			Mode: MarketRuntimeLaunchd, Label: "com.zizdog.alist",
			LabelSource: "目录 ServiceLabel（与 releaseBinaryApps 注册表里的 Label 一致）",
		},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchReleaseBinary,
				Label:   "下载 alist-darwin-arm64.tar.gz",
				Upstream: MarketUpstream{
					ID:   "github.com/AlistGo/alist@v3.64.0/alist-darwin-arm64.tar.gz",
					URL:  "https://github.com/AlistGo/alist/releases/download/v3.64.0/alist-darwin-arm64.tar.gz",
					Repo: "AlistGo/alist", Tag: "v3.64.0", Asset: "alist-darwin-arm64.tar.gz",
					Size: 43021495,
					Note: "实测 43,021,495 B（2026-09-17 从 ghfast.top 下载整包实算 sha256；" +
						"本站 ≠ 上游声明，是**本机实测**）。直连 github.com 在本机 25s 0 字节（与 frpc 同一现象），" +
						"所以实际会落到加速镜像；候选顺序：镜像站（无测速直下）→ 官方 + ghfast.top + gh-proxy.com 按实测速度重排",
				},
				NAS:      nasMirrored("apps/alist/v3.64.0/alist-darwin-arm64.tar.gz"),
				Timeout:  150 * time.Second,
				Required: true,
				Checksum: MarketChecksum{
					Asset: "上游 md5.txt（**只有 md5**）+ 镜像 manifest.json",
					// 上游没有 sha256 清单 → 这条轨的 ChecksumAsset 留空（不做 sha256 校验），
					// 只做 verify_arm64 的架构复核。这里把我们实算出来的 sha256 写下来，
					// 供镜像站/在线审计核对（镜像 manifest.json 里就是这个值）。
					SHA256: "5f3cd409b1ba5c25d240ccb93bb77fa8a8e8cabbd21a467849ac90443e8f8140",
					Source: "2026-09-17 本机把整包从 ghfast.top 下下来实算 sha256；" +
						"同时与上游 md5.txt 的 md5 591823b7f114d4b4d79af268e1e9bdac 互证一致（两条独立算法都吻合）",
					Note: "⚠️ 明确的强度缺口：上游只发布 md5.txt，没有 sha256 清单 → " +
						"binary_release.go 那条轨（ChecksumAsset 为空）**不做内容校验**，" +
						"只有 file(1) 的架构复核 + 与上游 md5 的互证。镜像站上有 manifest.json（sha256）时会按它校验",
				},
				ARM64: "上游资产名自带 darwin-arm64；实测解压出的 alist 用 file(1) 报 Mach-O 64-bit executable arm64，" +
					"`alist --help` 正常输出（本机实测）",
				Note: "与 frpc / ddns-go 同一条回落链；tarball 内只有平级的 alist 一个成员（无顶层目录）",
			},
		},
	},

	// ---------------- 一键建站（站点源码） ----------------

	{
		ID: "typecho", Kind: KindNative,
		Runtime: MarketRuntime{Mode: MarketRuntimeSite, LabelSource: "目录 SiteApp 非空（一键建站，产出是网站不是服务）"},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchSiteSource,
				Label:   "下载 typecho.zip（主址 GitHub）",
				Upstream: MarketUpstream{
					ID:   "github.com/typecho/typecho@latest/typecho.zip",
					URL:  "https://github.com/typecho/typecho/releases/latest/download/typecho.zip",
					Size: 578232,
					Note: "实测 578,232 B、93,009 B/s（6.2 s 下完）",
				},
				NAS: nasMissing("镜像站上没有站点源码包（apps/typecho/... 不存在）。这不是'不需要'——" +
					"它是**缺口**：一旦 GitHub 不可达，Typecho 就装不上。建议同步到 apps/typecho/<ver>/typecho.zip 并把候选顺序改成 镜像 → 主址 → 备址"),
				Timeout:  9 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{
					Note: "没有校验清单；代码只做'文件 < 1024 B 即失败'这种健全性检查（api_site_apps.go）",
				},
				ARM64: "PHP 站点源码（zip），与架构无关",
				Note: "downloadFile 现在有 --max-time 540 + --speed-limit 1024/60s（外层 ctx 10 min）；" +
					"清点报告里的'没有 --max-time'已被并发代理修掉 —— 声明以当前代码为准",
			},
			{
				Purpose: MarketFetchSiteSource,
				Label:   "下载 typecho.zip（备址 jsdelivr）",
				Upstream: MarketUpstream{
					ID:  "cdn.jsdelivr.net/gh/typecho/typecho@master/typecho.zip",
					URL: "https://cdn.jsdelivr.net/gh/typecho/typecho@master/typecho.zip",
					Note: "⚠️ 实测 **404**（'Couldn't find the requested file /typecho.zip'）→ 这个备址实际上是失效的，" +
						"Typecho 现在只剩 GitHub 一个可用源（单点）",
				},
				NAS:      nasNotNeeded("备址本身已经 404，镜像它没有意义；TS 该做的是把这个失效地址换掉或补镜像站（见主址那条 missing）"),
				Timeout:  9 * time.Minute,
				Required: false,
				OptionalImpact: "备址失效 → 主址（GitHub）挂了就没有任何退路，Typecho 装不上；" +
					"代码仍会逐个尝试，所以表现为'所有源都失败'而不是'缺一个备源'",
				Checksum: MarketChecksum{Note: "上游没有校验清单（连文件都没有）"},
				ARM64:    "PHP 站点源码，与架构无关",
				Note:     "catalog.go 的 MirrorURLs 字段；改名/换址时声明里的 URL 必须同步（反漂移测试会比对）",
			},
		},
	},

	{
		ID: "freshrss", Kind: KindNative,
		Runtime: MarketRuntime{Mode: MarketRuntimeSite, LabelSource: "目录 SiteApp 非空（一键建站，产出是网站不是服务）"},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchSiteSource,
				Label:   "下载 FreshRSS 1.30.0 源码包（gh-proxy 主址）",
				Upstream: MarketUpstream{
					ID:   "github.com/FreshRSS/FreshRSS@1.30.0/source.tar.gz",
					URL:  "https://gh-proxy.com/https://github.com/FreshRSS/FreshRSS/archive/refs/tags/1.30.0.tar.gz",
					Size: 4807475,
					Note: "实测 4,807,475 B；sha256 c58e045272c8b051da700559e2a8bb9205084285ee0a25a3efb504012e48483e" +
						"（codeload 直连与 gh-proxy 两条路下到的字节完全相同）",
				},
				NAS: nasMissing("镜像站上没有 FreshRSS 源码包（apps/freshrss/... 不存在）。它是**缺口**：" +
					"GitHub 不可达就装不上；建议同步到 apps/freshrss/1.30.0/source.tar.gz 并把候选顺序改成 镜像 → 主址 → 备址"),
				Timeout:  9 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{
					SHA256: "c58e045272c8b051da700559e2a8bb9205084285ee0a25a3efb504012e48483e",
					Source: "2026-09-17 本机把归档整包下下来实算 sha256；gh-proxy 与 codeload 两条独立链路" +
						"下到的字节完全相同（4,807,475 B），所以这个值不是「我以为」而是实测值。" +
						"注意：站点轨（api_site_apps.go）当前只做「文件 > 1024 B」的健全性检查，" +
						"**不做**内容校验 —— 值在此登记供镜像站与在线审计核对",
					Note: "固定版本 + 实测 sha256；镜像站补包后应把它写进 manifest.json 并让安装器按它校验",
				},
				ARM64: "PHP 站点源码（tar.gz），与架构无关；原生跑在面板已有的 nginx + php-fpm 上",
				Note:  "上游 releases 连续 8 个版本 assets 为空，只能拿源码归档；固定 1.30.0 而不是 master，才能校验内容",
			},
			{
				Purpose: MarketFetchSiteSource,
				Label:   "下载 FreshRSS 1.30.0 源码包（codeload 备址）",
				Upstream: MarketUpstream{
					ID:   "codeload.github.com/FreshRSS/FreshRSS@1.30.0",
					URL:  "https://codeload.github.com/FreshRSS/FreshRSS/tar.gz/refs/tags/1.30.0",
					Size: 4807475,
					Note: "与主址字节相同（实测）",
				},
				NAS:            nasNotNeeded("与主址同一份字节；镜像站补主址即可，不必两条都镜像"),
				Timeout:        9 * time.Minute,
				Required:       false,
				OptionalImpact: "备址失效只会少一条退路（主址是 gh-proxy，国内可达性较好）",
				Checksum: MarketChecksum{
					SHA256: "c58e045272c8b051da700559e2a8bb9205084285ee0a25a3efb504012e48483e",
					Source: "与主址同一份字节（2026-09-17 两条链路实算比对一致）",
				},
				ARM64: "PHP 站点源码（tar.gz），与架构无关",
			},
		},
	},

	{
		ID: "homepage", Kind: KindCompose, ComposeImage: "ghcr.io/gethomepage/homepage:v2.3.0",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("ghcr.io/gethomepage/homepage:v2.3.0", 20*time.Minute,
				nasNotNeeded("镜像站的 /docker 只反代 Docker Hub，不覆盖 ghcr.io；ghcr.io 可直连（本机实测），"+
					"arm64 层与 amd64 同 index 已确认"),
				"arm64 证据：ghcr.io 的 index 里同时有 amd64 与 arm64（:v2.3.0 与 :latest 都确认过）"),
		},
	},

	{
		ID: "trilium", Kind: KindCompose, ComposeImage: "triliumnext/trilium:v0.105.0",
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("triliumnext/trilium:v0.105.0", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据：Docker Hub 的 OCI index 里同时有 linux/arm64 与 linux/amd64"+
					"（2026-09-17 经本机 Colima 的加速源 dockerproxy.net 读同一份 index，arm64 层 digest "+
					"sha256:621b7323fcccf7a5…）；且在 Mac mini 的 Colima 虚机（Ubuntu 24.04 aarch64）上 "+
					"docker compose up -d 真的跑起来：docker image inspect 报 os=linux arch=arm64、"+
					"容器 healthy、宿主 8091 在听 —— 是实测，不是「应该可以」"),
		},
		Note: "镜像改名（2026-09-17 核实）：上游把镜像从 triliumnext/notes 改成了 triliumnext/trilium。" +
			"notes:latest 已冻结在 v0.95.0（镜像构建时间 2025-06-15），trilium:latest = v0.105.0（2026-08-19）；" +
			"两者都自带 linux/arm64，但只有后者是当前版（上游仓库里的 docker-compose.yml 仍写 notes，是改名后没同步）。" +
			"声明按约定只写 latest；顺带核实 triliumnext/trilium:v0.105.0 这个 tag **确实存在**" +
			"（镜像源 HTTP 200），而 0.105.0 / v0.105 不存在 —— 需要可复现安装时可以钉住 v0.105.0。",
	},

	{
		// Activepieces：3 容器（app 兼任 worker + PostgreSQL/pgvector + Redis），
		// 下载点必须逐一列全（反漂移会比对声明镜像集合与目录 compose 里的集合）。
		ID: "activepieces", Kind: KindCompose,
		ComposeImages: []string{
			"ghcr.io/activepieces/activepieces:0.91.0",
			"pgvector/pgvector:0.8.0-pg14",
			"library/redis:7.0.7",
		},
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("ghcr.io/activepieces/activepieces:0.91.0", 20*time.Minute,
				nasNotNeeded("镜像站的 /docker 只反代 Docker Hub，不覆盖 ghcr.io；ghcr.io 本机实测可直连"+
					"（docker manifest inspect 直接成功），arm64 层大小未实测。（诚实标注：未实测完整 pull 的吞吐）"),
				"arm64 证据（2026-09-17 本机 `docker manifest inspect ghcr.io/activepieces/activepieces:0.91.0` 直查）："+
					"index 里有 linux/amd64 与 linux/arm64（另两个是 unknown/unknown 的 attestation）"),
			dockerImagePoint("pgvector/pgvector:0.8.0-pg14", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据（2026-09-17 经自建 NAS 的 /docker pull-through 读同一份 OCI index）："+
					"linux/amd64、linux/arm64（另两个 unknown/unknown attestation）"),
			dockerImagePoint("library/redis:7.0.7", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据（2026-09-17 经自建 NAS 的 /docker pull-through 读同一份 manifest list）："+
					"linux/amd64、linux/arm64/v8、linux/arm/v5、linux/arm/v7、386、mips64le、ppc64le、s390x。"+
					"compose 里写规范形式 library/redis:7.0.7（等价官方 redis:7.0.7）"),
		},
		Note: "宿主端口 8090（8080 是 IOPaint 的保留端口）；PG/Redis **不发布宿主端口**。" +
			"AP_ENCRYPTION_KEY 用 16 字节 hex（32 个字符）——源码 `Buffer.from(secret,'binary')` + aes-256-cbc " +
			"要求密钥恰好 32 字符，给 64 个 hex 字符会 Invalid key length（见 catalog.go 条目注释）。" +
			"AP_FRONTEND_URL 面板拿不到 LAN IP，模板里是 ${AP_FRONTEND_URL:-http://127.0.0.1:8090}，" +
			"要对外用 webhook 需自行改 .env。",
	},

	{
		// Immich：官方 4 容器，下载点逐一列全。
		ID: "immich", Kind: KindCompose,
		ComposeImages: []string{
			"ghcr.io/immich-app/immich-server:release",
			"ghcr.io/immich-app/immich-machine-learning:release",
			"ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0",
			"valkey/valkey:9",
		},
		Runtime: MarketRuntime{Mode: MarketRuntimeContainer, LabelSource: "目录 Kind=KindCompose"},
		Downloads: []MarketDownloadPoint{
			dockerImagePoint("ghcr.io/immich-app/immich-server:release", 30*time.Minute,
				nasNotNeeded("镜像站的 /docker 只反代 Docker Hub，不覆盖 ghcr.io；本机实测 ghcr.io 可直连"+
					"（docker manifest inspect 直接成功）。immich-server 体积较大，30 min 超时未实测完整 pull。"),
				"arm64 证据（2026-09-17 本机 `docker manifest inspect ghcr.io/immich-app/immich-server:release` 直查）："+
					"index 里有 linux/amd64 与 linux/arm64（另两个是 unknown/unknown 的 attestation）"),
			dockerImagePoint("ghcr.io/immich-app/immich-machine-learning:release", 30*time.Minute,
				nasNotNeeded("镜像站的 /docker 只反代 Docker Hub，不覆盖 ghcr.io；本机实测 ghcr.io 可直连。"+
					"首次启动还要从外部下载 ML 模型权重，那部分不在这个镜像里。"),
				"arm64 证据（2026-09-17 本机 `docker manifest inspect ghcr.io/immich-app/immich-machine-learning:release` 直查）："+
					"index 里有 linux/amd64 与 linux/arm64（另两个是 unknown/unknown 的 attestation）"),
			dockerImagePoint("ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0", 20*time.Minute,
				nasNotNeeded("镜像站的 /docker 只反代 Docker Hub，不覆盖 ghcr.io；本机实测 ghcr.io 可直连"+
					"（docker manifest inspect 直接成功）。与 Immich 官方 compose 用的是同一个 tag。"),
				"arm64 证据（2026-09-17 本机 `docker manifest inspect ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0` 直查）："+
					"index 里有 linux/amd64 与 linux/arm64"),
			dockerImagePoint("valkey/valkey:9", 20*time.Minute,
				nasMirrored("docker"),
				"arm64 证据（2026-09-17 经自建 NAS 的 /docker pull-through 读同一份 OCI index）："+
					"linux/amd64、linux/arm64、linux/arm/v7、ppc64le"),
		},
		Note: "官方 4 容器；DB_PASSWORD 由安装时随机生成（hex 32 字节），重装复用不重新生成 —— " +
			"数据库初始化后再换口令会直接连不上（这是本条目最危险的一点，单测锁死）。" +
			"官方明确非 Linux 宿主 strongly discouraged；macOS 无硬件转码（只能 CPU 软转），" +
			"首次启动要下 ML 模型 —— 三条都写进了目录 Description。",
	},

	{
		ID: "wordpress", Kind: KindNative,
		Runtime: MarketRuntime{Mode: MarketRuntimeSite, LabelSource: "目录 SiteApp 非空"},
		Downloads: []MarketDownloadPoint{
			{
				Purpose: MarketFetchSiteSource,
				Label:   "下载 latest-zh_CN.zip（主址 cn.wordpress.org）",
				Upstream: MarketUpstream{
					ID:   "cn.wordpress.org/latest-zh_CN.zip",
					URL:  "https://cn.wordpress.org/latest-zh_CN.zip",
					Size: 44762801,
					Note: "实测 44,762,801 B、585,747 B/s → 约 76 s",
				},
				NAS: nasMissing("镜像站上没有站点源码包（apps/wordpress/... 不存在）—— **缺口**：" +
					"44.7 MB 的整包每次安装都要从公网重下。建议同步到 apps/wordpress/<ver>/latest-zh_CN.zip 并让候选顺序镜像优先"),
				Timeout:  9 * time.Minute,
				Required: true,
				Checksum: MarketChecksum{
					Note: "没有校验清单；只有'文件 < 1024 B 即失败'的健全性检查",
				},
				ARM64: "PHP 站点源码（zip），与架构无关",
				Note:  "downloadFile：--max-time 540 + 停滞看门狗（当前代码）",
			},
			{
				Purpose: MarketFetchSiteSource,
				Label:   "下载 latest.zip（备址 wordpress.org 官方）",
				Upstream: MarketUpstream{
					ID:   "wordpress.org/latest.zip",
					URL:  "https://wordpress.org/latest.zip",
					Size: 37216004,
					Note: "实测 37,216,004 B、502,109 B/s。注意：这是**英文原版**，不是中文版（Plan B 的代价）",
				},
				NAS:            nasMissing("与主址同一个缺口：镜像站上没有 WordPress 源码包。这条是备源，同样是公网直连"),
				Timeout:        9 * time.Minute,
				Required:       false,
				OptionalImpact: "备址失败 → 只剩 cn.wordpress.org 单点；主址挂了就装不上",
				Checksum:       MarketChecksum{Note: "上游没有校验清单"},
				ARM64:          "PHP 站点源码，与架构无关",
				Note:           "两个源的**内容不同**（中文版 vs 英文原版），清点报告实测体积差 7.5 MB",
			},
		},
	},
}

// ---------------------------------------------------------------------------
//  只读访问器
// ---------------------------------------------------------------------------

// MarketApps 返回全部下载点声明（按 ID 排序，输出稳定）。
func MarketApps() []MarketApp {
	out := make([]MarketApp, len(marketDownloadApps))
	copy(out, marketDownloadApps)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// MarketAppFor 按 ID 找声明（审计的 --only 用）。
func MarketAppFor(id string) (MarketApp, bool) {
	for _, a := range marketDownloadApps {
		if a.ID == id {
			return a, true
		}
	}
	return MarketApp{}, false
}

// declaredComposeImages 把声明里的镜像归一化成一个**有序集合**
// （单容器的 ComposeImage 与多容器的 ComposeImages 统一处理）。
func (m MarketApp) declaredComposeImages() []string {
	if len(m.ComposeImages) > 0 {
		out := make([]string, len(m.ComposeImages))
		copy(out, m.ComposeImages)
		sort.Strings(out)
		return out
	}
	if m.ComposeImage != "" {
		return []string{m.ComposeImage}
	}
	return nil
}

// equalStringSlices 比较两个已排序的字符串切片是否逐元素相同。
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// MarketIDs 返回全部应用 ID（按目录顺序，用于 --only 的报错提示）。
func MarketIDs() []string {
	out := make([]string, 0, len(marketDownloadApps))
	for _, a := range marketDownloadApps {
		out = append(out, a.ID)
	}
	sort.Strings(out)
	return out
}

// MarketImageAcceptHeader 是查镜像 manifest 时要带的 Accept。
//
// 它是 docker_mirror_nas.go 里 imageAcceptHeader 的导出别名 —— 审计工具
// （package main）要用它，但**不另抄一份常量**：漏一个 media type 会让
// 多架构 index 被当成单架构 manifest，arm64 判断直接失真。
const MarketImageAcceptHeader = imageAcceptHeader

// MarketImageRef 把目录里的镜像名拆成 (registry, repo, tag)。
//
// 复用 docker_mirror_nas.go 里已有的 splitImageRef —— 审计不另造一套解析
// （两套解析对 `host:5000/repo` 这种引用的处理一定会分叉）。
func MarketImageRef(image string) (host, repo, tag string) { return splitImageRef(image) }

// ---------------------------------------------------------------------------
//  第 2 层：静态不变量（不联网；进 go test，也就进了 make check）
// ---------------------------------------------------------------------------

// marketInvariantMinReason 是"为什么不需要镜像"这类理由的最小长度。
//
// 为什么要有它：不设下限时，填空的人会写"不需要"三个字，于是审计全绿而缺口
// 一个没解决 —— 用户最反感的就是这种"谎报成功"。20 个字逼着写清实测数字或
// 架构事实。
const marketInvariantMinReason = 20

// MarketInvariantProblems 跑全部静态不变量，返回问题清单（空 = 全过）。
//
// 这是"第 2 层门禁"的唯一实现：测试与 `make market-audit --offline` 都调它，
// 免得两边各写一套规则（那一定会分叉）。
func MarketInvariantProblems() []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	catalog := Catalog()
	byID := map[string]App{}
	for _, a := range catalog {
		byID[a.ID] = a
	}
	declared := map[string]bool{}

	for _, m := range marketDownloadApps {
		if declared[m.ID] {
			add("声明里 %s 出现了两次", m.ID)
		}
		declared[m.ID] = true

		app, ok := byID[m.ID]
		if !ok {
			add("声明里有 %s，但目录 Catalog() 里没有这个条目（条目被删/改名了？）", m.ID)
			continue
		}

		problems = append(problems, MarketDeclarationProblems(m, app)...)
	}

	// ---- 反向：目录里的每个条目都必须有声明 ----
	for _, a := range catalog {
		if !declared[a.ID] {
			add("目录里有 %s（%s），但 market_downloads.go 里**没有声明**它的下载点 —— "+
				"加应用时必须同时填空，否则这个工作流就退化成'一个一个踩坑'", a.ID, a.Name)
		}
	}

	sort.Strings(problems)
	return problems
}

// MarketDeclarationProblems 把**一份声明**与**一个目录条目**逐字段比对，
// 返回全部问题（空 = 与目录一致，且这份声明自身的不变量都成立）。
//
// 为什么单独抽出来：反漂移这件事必须能被**证明**而不是被**声称**。
// market_downloads_test.go 拿一个故意改坏的目录条目调它、断言一定报错 ——
// 于是"目录改了、声明没改 → 测试失败"不是一句承诺，而是一条被测过的代码路径。
func MarketDeclarationProblems(m MarketApp, app App) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// ---- 反漂移：声明与目录逐字段比对 ----
	if m.Kind != app.Kind {
		add("%s: 声明的 Kind=%s，目录是 %s", m.ID, m.Kind, app.Kind)
	}
	if m.BrewFormula != app.BrewFormula {
		add("%s: 声明的 BrewFormula=%q，目录是 %q", m.ID, m.BrewFormula, app.BrewFormula)
	}
	if m.PanelInstaller != app.PanelInstaller {
		add("%s: 声明的 PanelInstaller=%q，目录是 %q", m.ID, m.PanelInstaller, app.PanelInstaller)
	}
	if m.ServiceLabel != app.ServiceLabel {
		add("%s: 声明的 ServiceLabel=%q，目录是 %q", m.ID, m.ServiceLabel, app.ServiceLabel)
	}
	if m.NoDaemon != app.NoDaemon {
		add("%s: 声明的 NoDaemon=%v，目录是 %v", m.ID, m.NoDaemon, app.NoDaemon)
	}

	// 反漂移（release 二进制）：与 ReleaseBinaryAssets() 注册表交叉校验。
	//
	// 注册表是安装器**真正**会去下的东西（版本、文件名、校验清单），
	// 声明与它不一致 = 这份声明在说谎，而"审计全绿、实际下的是别的文件"
	// 正是最危险的一种绿。
	var releaseRef *ReleaseBinaryAsset
	for _, ra := range ReleaseBinaryAssets() {
		if ra.ID == m.ID {
			ra := ra
			releaseRef = &ra
			break
		}
	}

	// compose 条目：镜像集合必须与目录里的 compose 文件一致，且不许出现 platform。
	// 支持多容器 compose（Activepieces 3 容器 / Immich 4 容器）：声明用
	// ComposeImages 列出**全部**镜像，并**每个镜像一条 docker_image 下载点** ——
	// 少一条就等于"有一个容器没有任何下载点声明"，审计会把它的 arm64 证据漏掉。
	imgs := composeImagesOf(app.ComposeYAML)
	switch {
	case m.Kind == KindCompose:
		declaredImgs := m.declaredComposeImages()
		if len(declaredImgs) == 0 {
			add("%s: compose 条目必须在声明里列出镜像（单容器用 ComposeImage，多容器用 ComposeImages）", m.ID)
		} else if !equalStringSlices(declaredImgs, imgs) {
			add("%s: 声明的镜像集合 %v 与目录 compose 里的 %v 不一致", m.ID, declaredImgs, imgs)
		}
		if strings.Contains(app.ComposeYAML, "platform:") {
			add("%s: compose 里出现了 `platform:`（铁律②：不许写 platform，更不许 linux/amd64）", m.ID)
		}
		pointed := map[string]bool{}
		for _, d := range m.Downloads {
			if d.Purpose == MarketFetchDockerImage {
				pointed[d.Upstream.ID] = true
			}
		}
		for _, img := range imgs {
			if !pointed[img] {
				add("%s: compose 里的镜像 %q 没有对应的 docker_image 下载点声明", m.ID, img)
			}
		}
		// Docker 推荐项目（2026-09-17 起所有 compose 条目）：镜像仍然要同步，
		// 但**声明必须写清新语义** —— 否则审计/清点报告会继续把它当成"可安装应用"，
		// 而安装接口对这些条目是明确拒绝的。两边说法必须一致。
		if app.DockerReference {
			for _, d := range m.Downloads {
				if d.Purpose == MarketFetchDockerImage && !strings.Contains(d.Note, "推荐项目") {
					add("%s: 是推荐 Docker 项目，docker_image 下载点的 Note 必须写清"+
						"「镜像仍供拉取，但面板不再自动安装」（当前 Note=%q）", m.ID, d.Note)
				}
			}
		}
	case len(m.ComposeImages) > 0 || m.ComposeImage != "":
		add("%s: 声明了 ComposeImage/ComposeImages 但目录不是 compose 条目", m.ID)
	}

	// ---- 服务语义：必须显式回答（launchd / container / site / none）----
	if err := marketRuntimeProblem(m, app, len(imgs)); err != "" {
		add("%s: %s", m.ID, err)
	}

	// ---- 每个下载点的不变量 ----
	if len(m.Downloads) == 0 {
		add("%s: 一个下载点都没有声明。真有「安装不需要任何下载」的条目，请在 Note 里写清为什么", m.ID)
	}
	for i, d := range m.Downloads {
		where := fmt.Sprintf("%s[%d] %q", m.ID, i, d.Label)
		if !isKnownMarketFetchKind(d.Purpose) {
			add("%s: 用途 %q 不是已知类型", where, d.Purpose)
		}
		if strings.TrimSpace(d.Label) == "" {
			add("%s: Label 为空", where)
		}
		// 1) 每个下载点必须声明超时（或写清为什么真的没有）。
		if d.Timeout <= 0 && strings.TrimSpace(d.TimeoutReason) == "" {
			add("%s: **没有超时也没有理由** —— 历史上「没有超时 → 永远挂住」就是这么来的。"+
				"要么给出真实超时，要么写 TimeoutReason", where)
		}
		if d.Timeout > 0 && strings.TrimSpace(d.TimeoutReason) != "" {
			add("%s: 有超时（%s）就不该再写 TimeoutReason（声明自相矛盾）", where, d.Timeout)
		}
		// 2) 每个下载点必须回答「NAS 优先？」。
		switch d.NAS.State {
		case NASMirrored:
			if strings.TrimSpace(d.NAS.Path) == "" {
				add("%s: NAS 状态是 mirrored 但 Path 为空", where)
			}
			if strings.HasPrefix(d.NAS.Path, "/") {
				add("%s: NAS Path 必须是相对路径（不以 / 开头），现在是 %q", where, d.NAS.Path)
			}
			if strings.TrimSpace(d.NAS.Reason) != "" {
				add("%s: NAS 状态是 mirrored 却还写了 Reason（自相矛盾）", where)
			}
		case NASMissing, NASNotNeeded:
			if len([]rune(strings.TrimSpace(d.NAS.Reason))) < marketInvariantMinReason {
				add("%s: NAS 状态 %s 的理由太短（%d 字 < %d）—— 必须写清「缺什么 / 为什么不需要」，"+
					"理由要带实测数字或架构事实；不许写「不需要」三个字就过",
					where, d.NAS.State, len([]rune(strings.TrimSpace(d.NAS.Reason))), marketInvariantMinReason)
			}
			if strings.TrimSpace(d.NAS.Path) != "" {
				add("%s: NAS 状态 %s 时不该有 Path（%q）", where, d.NAS.State, d.NAS.Path)
			}
		default:
			add("%s: NAS 状态 %q 不合法（必须是 mirrored / missing / not_needed）—— "+
				"留空默认通过是不允许的", where, d.NAS.State)
		}
		// 3) 可选步骤必须写清降级后果。
		if !d.Required && strings.TrimSpace(d.OptionalImpact) == "" {
			add("%s: Required=false 但没写 OptionalImpact（用户会以为「装好了」，其实少一部分能力）", where)
		}
		if d.Required && strings.TrimSpace(d.OptionalImpact) != "" {
			add("%s: Required=true 就不该有 OptionalImpact", where)
		}
		// 4) sha256：有值必须有来源；两者都没有必须写清「为什么没有校验」。
		c := d.Checksum
		if c.SHA256 != "" && strings.TrimSpace(c.Source) == "" {
			add("%s: 声明了 sha256 但没写 Source（来源不明的 sha256 曾经导致「镜像永不命中、静默回落慢源」）", where)
		}
		if c.SHA256 == "" && strings.TrimSpace(c.Asset) == "" && strings.TrimSpace(c.Note) == "" {
			add("%s: 既没有校验清单、也没有 sha256、也没有说明 —— 不许留空默认通过", where)
		}
		// 5) arm64 证据。
		if strings.TrimSpace(d.ARM64) == "" {
			add("%s: 没有 arm64 证据（写清「怎么验证的」；拿不到就写「未验证」）", where)
		}
		if d.Purpose == MarketFetchDockerImage && !strings.Contains(d.ARM64, "arm64") {
			add("%s: 容器镜像的 arm64 证据里没有出现 arm64", where)
		}
		// 6) 上游必须至少有一个标识。
		if strings.TrimSpace(d.Upstream.ID) == "" && strings.TrimSpace(d.Upstream.URL) == "" {
			add("%s: 上游既没有 ID 也没有 URL", where)
		}
		if d.Upstream.Size > 0 && strings.TrimSpace(d.Upstream.Note) == "" {
			add("%s: 写了体积但没写来源（实测还是上游声明？不许编造体积）", where)
		}
		// 7) GitHub release 三元组：URL 必须是官方地址（防手抄错），
		//    且 release 二进制必须与安装器注册表完全一致。
		if d.Upstream.Repo != "" {
			want := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s",
				d.Upstream.Repo, d.Upstream.Tag, d.Upstream.Asset)
			if d.Upstream.URL != want {
				add("%s: 上游 URL 拼错了。声明 %q，按 Repo/Tag/Asset 应该是 %q", where, d.Upstream.URL, want)
			}
		}
		if d.Purpose == MarketFetchReleaseBinary {
			if releaseRef == nil {
				add("%s: 声明成 release 二进制，但安装器注册表 ReleaseBinaryAssets() 里**没有** %s —— "+
					"这种错配会表现为「条目在市场里、装的却是别的东西」", where, m.ID)
			} else {
				if d.Upstream.Tag != releaseRef.Tag || d.Upstream.Asset != releaseRef.Asset {
					add("%s: 与注册表不一致。声明 %s/%s，注册表是 %s/%s",
						where, d.Upstream.Tag, d.Upstream.Asset, releaseRef.Tag, releaseRef.Asset)
				}
				if releaseRef.ChecksumAsset != "" && d.Checksum.UpstreamFile != releaseRef.ChecksumAsset {
					add("%s: 上游校验清单与注册表不一致。声明 %q，注册表是 %q",
						where, d.Checksum.UpstreamFile, releaseRef.ChecksumAsset)
				}
			}
		}
	}

	sort.Strings(problems)
	return problems
}

// isKnownMarketFetchKind 判断用途是否是已定义的类型。
func isKnownMarketFetchKind(k MarketFetchKind) bool {
	switch k {
	case MarketFetchBrewBottle, MarketFetchBrewInstaller, MarketFetchBrewGit,
		MarketFetchReleaseBinary, MarketFetchDockerImage, MarketFetchPipPackage,
		MarketFetchModelFile, MarketFetchSiteSource, MarketFetchCLT, MarketFetchVMImage:
		return true
	}
	return false
}

// marketRuntimeProblem 检查"服务语义"这一条不变量（空串 = 没问题）。
func marketRuntimeProblem(m MarketApp, app App, composeImages int) string {
	switch m.Runtime.Mode {
	case MarketRuntimeLaunchd:
		if strings.TrimSpace(m.Runtime.Label) == "" {
			return "Mode=launchd 但没写 Label"
		}
		if app.ServiceLabel != "" {
			if m.Runtime.Label != app.ServiceLabel {
				return fmt.Sprintf("运行期 label=%q，目录 ServiceLabel=%q（不一致）", m.Runtime.Label, app.ServiceLabel)
			}
		} else if strings.TrimSpace(m.Runtime.CatalogGap) == "" {
			return "运行期是 launchd 服务，但目录既没有 ServiceLabel 也没有 NoDaemon，" +
				"这里必须写 CatalogGap 说清靠什么兜底、要补哪里"
		}
	case MarketRuntimeContainer:
		if app.Kind != KindCompose && app.Kind != KindDocker {
			return fmt.Sprintf("Mode=container 但目录 Kind=%s", app.Kind)
		}
		if composeImages == 0 {
			return "Mode=container 但目录里没有 compose"
		}
	case MarketRuntimeSite:
		if app.SiteApp == nil {
			return "Mode=site 但目录没有 SiteApp"
		}
	case MarketRuntimeNone:
		if !app.NoDaemon {
			return "Mode=none 但目录没有标 NoDaemon（服务语义没有显式回答）"
		}
	default:
		return fmt.Sprintf("Runtime.Mode=%q 不合法（必须显式回答 launchd/container/site/none）", m.Runtime.Mode)
	}
	if app.NoDaemon && m.Runtime.Mode != MarketRuntimeNone {
		return fmt.Sprintf("目录标了 NoDaemon，但声明说 Mode=%s（不一致）", m.Runtime.Mode)
	}
	if strings.TrimSpace(m.Runtime.LabelSource) == "" {
		return "没写 Runtime.LabelSource（label 的证据来源）"
	}
	return ""
}

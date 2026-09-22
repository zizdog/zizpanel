package services

import (
	"regexp"
	"sort"
	"strings"
)

// ============================================================================
//  离线包（镜像站侧）——清单驱动的"一个应用安装所需的全部文件"
//
//  背景（用户原话，2026-09-16）：
//    "要确保应用市场里的所有软件都能顺利安装！有必要的话可以把所有文件卡点
//     都放到镜像站。甚至直接将软件'打包'一键迁移回 mac 系统里。"
//
//  本文件只做**一件事**：把"应用市场里每个应用需要从网上拿哪些文件"变成
//  一份**机器可读的计划**（OfflinePlan），交给构建工具
//  tools/build-offline-bundle.sh 去镜像站上汇总成离线包。
//
//  为什么计划必须从代码生成、而不是在 shell 脚本里手抄：
//    tools/sync-nas-apps.sh 的教训 —— 手抄的那份在加应用/升版本时一定会漏，
//    而漏掉的后果是"离线包看起来完整、实际缺件"。这里复用 Catalog() 与
//    releaseBinaryApps，加应用/改公式会自动出现在计划里。
//
//  ⚠️ 本文件**不下载任何东西**，也不做网络探测：它必须是纯函数（可单测、
//  不会因为镜像站不在线而失败）。真正取件、算 sha256 的是构建工具。
//
//  离线包布局（与 tools/build-offline-bundle.sh 严格一致）：
//    <base>/offline/<app-id>/index.json              ← 有哪些版本、current 是哪个
//    <base>/offline/<app-id>/<version>/manifest.json ← 本文件描述的清单
//    <base>/offline/<app-id>/<version>/artifacts/...  ← 真实文件（sha256 实算）
// ============================================================================

// OfflineSchemaVersion 是离线包 manifest.json 的 schema 标识。
//
// 面板解析清单前先看它：版本对不上就明确报错，而不是拿旧格式硬解、
// 得到一堆空字段（那会退化成"看起来装了、其实用的是别的文件"）。
const OfflineSchemaVersion = "zizpanel.offline/v1"

// 离线包里一个 artifact 的 kind。
//
// 这些字符串会写进 manifest.json，是**对外契约**：面板、构建工具、
// 以后的"一键迁移回 Mac"都按它分流。改名等于改协议，不要偷偷改。
const (
	OfflineKindBrewBottle   = "brew_bottle"       // Homebrew 二进制瓶（.bottle.tar.gz）
	OfflineKindBrewManifest = "brew_manifest"     // brew 取瓶前先拉的 OCI manifest
	OfflineKindBrewFormula  = "brew_formula_json" // brew API 的 formula 清单
	OfflineKindPipWheel     = "pip_wheel"         // Python wheel / sdist
	OfflineKindModel        = "model"             // 模型权重（iopaint / HF）
	OfflineKindSiteTarball  = "site_tarball"      // 一键建站的源码包（zip/tar.gz）
	OfflineKindGitHubBinary = "github_binary"     // 官方 release 的 darwin-arm64 产物
	OfflineKindDockerImage  = "docker_image_tar"  // docker save 出来的镜像 tar
	OfflineKindVMImage      = "vm_image"          // Colima/Lima 的虚拟机磁盘镜像
	OfflineKindOther        = "other"             // 兜底，必须带 note 说明是什么
)

// OfflineArtifact 是离线包里的一个文件。
type OfflineArtifact struct {
	Kind string `json:"kind"`
	// Path 是文件在离线包里的相对路径（相对 <bundle>/ 目录，形如
	// "artifacts/<file>"）。构建工具按它落盘，面板按它校验。
	Path string `json:"path"`
	// MirrorPath 是**镜像站相对路径**（相对镜像基址），构建工具按它取件。
	// 空串表示"镜像上现在还没有这个文件" —— 构建工具必须把它报成缺件，
	// 不许静默跳过（sync-nas-apps.sh 曾经因为 ssh 吞 stdin 静默跳过应用却报成功）。
	MirrorPath string `json:"mirror_path,omitempty"`
	// UpstreamURL 是上游地址，**仅供参考与兜底**：离线安装永远不用它。
	// 留着是为了"缺件时告诉用户该去哪补"，以及人工核对用。
	UpstreamURL string `json:"upstream_url,omitempty"`
	// Note 是给人和给缺件报告用的一句话。
	Note string `json:"note,omitempty"`
}

// OfflineAppPlan 是一个应用的离线打包计划。
type OfflineAppPlan struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Version 是清单里的版本号。构建工具会用**真实解析出来的**版本覆盖它
	// （例如 brew 的 stable 版本要从公式清单里读）。
	Version string `json:"version,omitempty"`
	// InstallMethod 是安装方式标识（brewing / compose / binary_release / ...），
	// 只用于报告与分组，不参与安装决策。
	InstallMethod string `json:"install_method"`
	// Category 取自目录（lnmp / tool / ai / runtime / site）。
	Category string `json:"category"`

	// BrewFormula 非空表示这个应用走 Homebrew：构建工具要按**依赖闭包**展开
	// （ffmpeg 一个条目 = 15 个瓶，含 openssl@3 等传递依赖）。闭包只能在构建
	// 时联网解析，所以这里只给根公式名。
	BrewFormula string `json:"brew_formula,omitempty"`
	// DockerImages 是 compose 文件里引用的镜像（原样，未加 registry 前缀）。
	DockerImages []string `json:"docker_images,omitempty"`

	// Artifacts 是"已经能确定镜像路径"的文件。
	Artifacts []OfflineArtifact `json:"artifacts"`
	// Gaps 是**已知无法镜像**的缺口（有体积或来源信息时一并写清）。
	//
	// 与"构建工具发现的缺件"分开：这里写的是**设计上就还缺**的东西
	// （例如 PyPI 依赖闭包还没镜像），构建工具发现的写进它自己的报告。
	Gaps []string `json:"gaps,omitempty"`
}

// composeImageRe 从 compose 文件内容里抠出 `image:` 那一行。
//
// 为什么用正则而不是引入 YAML 解析器：目录里的 compose 全是本项目自己用
// composeTemplate 生成的简单结构，缩进固定、镜像名不含引号。多引一个
// YAML 依赖只为了读一行，收益不划算；真解析失败时下面的函数会返回空，
// 那会在计划里表现成"这个 compose 应用没有任何镜像"——一眼能看出来。
var composeImageRe = regexp.MustCompile(`(?m)^\s*image:\s*([^\s#]+)`)

// composeImagesOf 返回 compose 内容里引用的全部镜像。
func composeImagesOf(yaml string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range composeImageRe.FindAllStringSubmatch(yaml, -1) {
		img := strings.Trim(strings.TrimSpace(m[1]), `"'`)
		if img == "" || seen[img] {
			continue
		}
		seen[img] = true
		out = append(out, img)
	}
	sort.Strings(out)
	return out
}

// offlineSelfContained 列出"安装不需要额外下载任何文件"的面板自研安装器。
//
// voicereceiver 的接收端脚本是仓库里 voicereceiver.go 内嵌的（部署时被写成
// 一个文件），不需要从网上取任何东西；它唯一的外部依赖是 ffmpeg，而那是
// ffmpeg 那一条离线包负责的。显式列出来，是为了让报告里不出现
// "voicereceiver 缺东西"这种假缺口 —— 假缺口和真缺口一样有害：
// 用户会去补一个根本不需要补的文件。
var offlineSelfContained = map[string]string{
	"voicereceiver": "无需额外文件：接收端脚本内嵌在面板二进制里；唯一外部依赖 ffmpeg 由 ffmpeg 条目覆盖",
	// macOS 语音合成（say）是目录里**唯一真正零下载**的条目：
	// 合成引擎 /usr/bin/say 与格式转换 /usr/bin/afconvert 都是 macOS 自带的
	// （不动点：它们随系统走，镜像站上没有也不该有它们的副本 —— 把系统二进制
	// 塞进离线包既没意义又会让"离线包完整"变成假的）；网页界面是面板自己的
	// 二进制（`zizpanel speech-serve`，随面板升级）。所以离线包对它**零要求**。
	// 不写这一条就会在报告里冒出一个假缺口，用户会去补一个根本不存在的东西。
	"macspeech": "无需额外文件：合成引擎 /usr/bin/say 与转换工具 /usr/bin/afconvert 都是 macOS 自带的" +
		"（不随离线包分发），网页界面由面板自己的二进制提供（cmd/zizpanel 的 speech-serve 子命令）",
}

// offlinePythonGaps 是面板自研 Python 安装器**已知还没镜像**的依赖。
//
// 这些属于 /pypi/ 镜像线（另一个代理在做）的覆盖范围；写在这里是为了让
// 缺口报告逐条可数，而不是让 iopaint/qwen3tts 看起来"离线包已经齐了"。
var offlinePythonGaps = map[string][]string{
	"iopaint": {
		"PyPI 依赖闭包（pip install iopaint 及其全部传递依赖）尚未镜像；" +
			"镜像站上还没有 /pypi/simple，真离线时装不上",
	},
	"qwen3tts": {
		"PyPI 依赖闭包（pip install 'mlx-audio[server]' 及其全部传递依赖）尚未镜像；" +
			"镜像站上还没有 /pypi/simple，真离线时装不上",
		"HF 模型整目录（约 2.9GB）目前只走 /hf/ 按需缓存代理，未落成静态文件",
	},
}

// offlineInstallerExtra 是"面板自研安装器需要但 brew 瓶里没有"的文件。
//
// 与 offlinePythonExtra 分开是刻意的：那张表是**Python 线**的（pip 依赖闭包），
// 而语音转文字要的是**模型权重**（与 Python 一点关系都没有）。混在一起会让
// 下一轮排查的人以为 whisper 是个 Python 应用。
//
// 路径必须与安装器代码里**实际使用**的镜像路径一致（见 stt.go 的
// STTModelSourceList：静态目录 <base>/models/whisper/<file> 与
// HF 代理 <base>/hf/ggerganov/whisper.cpp/resolve/main/<file>）。
// 改下载点时**必须**同步这里，否则离线包会缺件。
var offlineInstallerExtra = map[string][]OfflineArtifact{
	"stt": {
		{
			Kind:        OfflineKindModel,
			Path:        "artifacts/models/whisper/ggml-large-v3-turbo-q5_0.bin",
			MirrorPath:  "hf/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo-q5_0.bin",
			UpstreamURL: "https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo-q5_0.bin",
			Note: "**默认档**模型（574,041,195 B，**本轮实测**：Range 读 Content-Range 得到）。" +
				"用户要求把默认档从 small 换成它，所以离线包的必需件也随之改成这一档。" +
				"安装流程只下这一档；small（487,601,967 B）与 medium（1,533,763,059 B）" +
				"按需下载、不在离线包的必需件里 —— 要真离线也必须把用得到的档位一起落盘。",
		},
	},
}

// offlinePythonExtra 是"面板自研 Python 安装器"需要但**不在目录里**的文件。
//
// 这些路径写在这里而不是去改 iopaint.go / qwentts.go（那两个文件正被别的
// 代理改动）：刻意做成"离线打包这一层自己的知识"，避免抢同一批文件。
// 路径与安装器代码里实际使用的镜像路径一致（如 iopaint 的模型走
// <base>/models/iopaint/<file>，见 iopaint.go 的 weightSource），
// 改安装器时**必须**同步这里，否则离线包会缺件。
var offlinePythonExtra = map[string][]OfflineArtifact{
	"iopaint": {
		{
			Kind:        OfflineKindModel,
			Path:        "artifacts/models/iopaint/big-lama.pt",
			MirrorPath:  "models/iopaint/big-lama.pt",
			UpstreamURL: "https://github.com/Sanster/models/releases/download/add_big_lama/big-lama.pt",
			Note:        "IOPaint 默认权重（~197MB），镜像上的 /models/iopaint/big-lama.pt 已就绪",
		},
	},
	"qwen3tts": {
		{
			Kind:        OfflineKindModel,
			Path:        "artifacts/hf/mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
			MirrorPath:  "hf/mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
			UpstreamURL: "https://hf-mirror.com/mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
			Note:        "HF 模型目录（约 2.9GB），走 /hf/ 按需缓存代理；要真离线必须整目录落盘",
		},
	},
}

// OfflinePlan 返回"应用市场 27 个应用各自的离线打包计划"。
//
// 顺序与 Catalog() 一致（目录顺序是有意义的：nginx 在最前，Docker 运行时
// 在所有 compose 应用之前），报告里逐条列出时不会看起来像乱序。
func OfflinePlan() []OfflineAppPlan {
	apps := Catalog()

	// 二进制 release 应用按 ID 建索引：它们的镜像路径由注册表算出来，
	// 不能手抄 tag 与文件名。
	binByID := map[string]releaseBinaryApp{}
	for _, spec := range releaseBinaryApps {
		binByID[spec.ID] = spec
	}

	out := make([]OfflineAppPlan, 0, len(apps))
	for _, app := range apps {
		p := OfflineAppPlan{
			ID:            app.ID,
			Name:          app.Name,
			InstallMethod: string(app.Kind),
			Category:      app.Category,
		}

		switch {
		case app.Kind == KindColima:
			// Colima：brew formula + Lima 虚拟机镜像。VM 镜像单独登记（见下），别抢。
			p.InstallMethod = "colima"
			p.BrewFormula = app.BrewFormula
			// 2026-09-16：VM 镜像**已经镜像到镜像站并接进面板**了，这条缺口关掉。
			//   镜像站：<base>/apps/colima-core/v0.10.4/ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz
			//        （332,354,401 B，sha256 与上游 .sha512sum 交叉验证过）
			//   面板: internal/services/colima_image.go 在 colima start 之前
			//        按「sha256(URL)」把它预热进 Colima 的下载缓存
			p.Artifacts = append(p.Artifacts, OfflineArtifact{
				Kind: OfflineKindVMImage,
				Path: "apps/colima-core/v0.10.4/ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz",
				Note: "Colima guest 虚拟机镜像（arm64 + docker 运行时变体）。" +
					"面板按 colima 自己的缓存命名规则预热，命中后一个字节都不用从 GitHub 下",
			})

		case app.Kind == KindCompose:
			p.InstallMethod = "compose"
			p.DockerImages = composeImagesOf(app.ComposeYAML)
			if len(p.DockerImages) > 0 {
				p.Artifacts = append(p.Artifacts, OfflineArtifact{
					Kind: OfflineKindOther,
					Path: "artifacts/docker/README.md",
					Note: "镜像 tar（docker save）体积大，镜像站**不提供** /docker 端点；" +
						"要离线加载请自行 docker save 后放进离线包。本计划只登记 compose 引用的镜像名：" +
						strings.Join(p.DockerImages, ", "),
				})
			}
			p.Gaps = append(p.Gaps,
				"compose 镜像尚未落成可离线加载的 tar："+strings.Join(p.DockerImages, ", "))

		case app.SiteApp != nil:
			p.InstallMethod = "site"
			p.Version = "latest"
			p.Artifacts = append(p.Artifacts, OfflineArtifact{
				Kind:        OfflineKindSiteTarball,
				Path:        "artifacts/" + siteTarballName(app.ID, app.SiteApp.Archive),
				UpstreamURL: app.SiteApp.DownloadURL,
				Note:        "一键建站源码包；镜像上还没有（缺口见报告）",
			})
			p.Gaps = append(p.Gaps,
				"建站源码包尚未镜像："+app.SiteApp.DownloadURL+
					"（镜像站建议路径 sites/"+app.ID+"/latest/"+siteTarballName(app.ID, app.SiteApp.Archive)+"）")

		default: // KindNative
			switch {
			// MirrorOnly 的自研产物（zizvideo）不在这条轨：它没有公网地址，
			// 版本/产物由 make release 的应用级索引决定（见下面的缺口说明）。
			case app.PanelInstaller != "" && binByID[app.PanelInstaller].ID != "" && !binByID[app.PanelInstaller].MirrorOnly:
				// 面板自研的 release 二进制安装器（frpc / orbien-client / ddns-go）
				spec := binByID[app.PanelInstaller]
				p.InstallMethod = "binary_release"
				p.Version = spec.Tag
				p.Artifacts = append(p.Artifacts, OfflineArtifact{
					Kind:        OfflineKindGitHubBinary,
					Path:        "artifacts/" + spec.Asset,
					MirrorPath:  "apps/" + spec.ID + "/" + spec.Tag + "/" + spec.Asset,
					UpstreamURL: spec.releaseURL(),
					Note:        "官方 " + spec.Repo + " " + spec.Tag + " darwin-arm64 产物",
				})
			default:
				if app.BrewFormula != "" {
					p.InstallMethod = "brew"
					p.BrewFormula = app.BrewFormula
				} else {
					p.InstallMethod = "panel_installer"
				}
				if app.ID == ZizvideoAppID {
					// 版本/产物由镜像索引运行时决定：离线包必须自带索引与它点名的产物。
					p.Gaps = append(p.Gaps, "zizvideo 的版本与产物由镜像索引 apps/zizvideo/manifest.json 决定，"+
						"离线包需自带该索引与对应产物（make release 产出到 dist/apps/zizvideo/，"+
						"把整目录传到镜像 apps/zizvideo/；只带二进制不带索引会装不上）")
				}
				if extra, ok := offlinePythonExtra[app.ID]; ok {
					p.Artifacts = append(p.Artifacts, extra...)
				}
				if extra, ok := offlineInstallerExtra[app.ID]; ok {
					p.Artifacts = append(p.Artifacts, extra...)
				}
				if gaps, ok := offlinePythonGaps[app.ID]; ok {
					p.Gaps = append(p.Gaps, gaps...)
				}
				if note, ok := offlineSelfContained[app.ID]; ok {
					p.Artifacts = append(p.Artifacts, OfflineArtifact{
						Kind: OfflineKindOther,
						Path: "artifacts/self-contained.txt",
						Note: note,
					})
				}
			}
		}

		// 面板自研安装器即使走了 brew（phpmyadmin / ffmpeg），也可能有额外文件。
		if app.PanelInstaller != "" && app.PanelInstaller != app.ID {
			if extra, ok := offlinePythonExtra[app.PanelInstaller]; ok {
				p.Artifacts = append(p.Artifacts, extra...)
			}
			if gaps, ok := offlinePythonGaps[app.PanelInstaller]; ok {
				p.Gaps = append(p.Gaps, gaps...)
			}
		}

		out = append(out, p)
	}
	return out
}

// siteTarballName 给建站源码包起一个稳定的离线文件名。
func siteTarballName(appID, archive string) string {
	ext := "zip"
	if archive != "" {
		ext = archive
	}
	return appID + "-latest." + ext
}

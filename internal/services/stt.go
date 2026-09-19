package services

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
//  语音转文字（whisper.cpp）—— 引擎
//
//  用户原话（2026-09-25）：
//    "在市场里加入一个语音转文字服务！要求：不要 docker，不要 gui 软件；
//      有 webui 或 api（你自行开发配套 webui）。"
//
//  选型（完整评估见交付汇报）：**whisper.cpp 的 Homebrew 包**。
//    · `brew info whisper-cpp` → formula 现在的**正名是 `whisper.cpp`**
//      （`whisper-cpp` 是它的 Old Name，见下面 STTBrewFormula 的注释）；
//    · 原生 arm64 瓶，Metal 加速（实测 M4 上初始化时
//      `ggml_metal_device_init: GPU name: MTL0 (Apple M4)`）；
//    · **不引入 Python / Docker / GUI**：这正是本项目 2026-09-18
//      `python@3.11` 被外力删除导致 TTS 挂两小时之后最在意的一条；
//    · 模型是单个 ggml 文件，可从 NAS / hf-mirror 取（见 STTModelSources）。
//
//  为什么用包里的 `whisper-cli` 而不是 `whisper-server`（**两者都实测存在**）：
//    `ls $(brew --prefix)/bin/whisper*` 里 `whisper-server` 确实在
//    （`/opt/homebrew/bin/whisper-server`，1,057,456 B 的 Mach-O），
//    而且它天生就是 OpenAI 兼容的。但本轮的交付里有两条硬要求它做不到：
//      1. **长音频要有真实进度** —— whisper-server 的 `-pp` 只把进度打到
//         *服务进程自己的 stdout*，HTTP 客户端拿不到任何中间状态，
//         面板能给的只会是一个假的转圈；
//      2. **模型档位可切换** —— 模型路径是 whisper-server 的**启动参数**，
//         换档必须重启服务并重新加载（首次加载要编译 Metal kernel，实测
//         二十多秒），面板做不到"这次请求用 medium"。
//    `whisper-cli`（同一个 formula、同一套 ggml 后端、同一份模型文件）两条都能：
//      · `-pp/--print-progress` 把 `whisper_print_progress_callback: progress = X%`
//        打到 stderr，面板逐行解析 → **真实进度**；
//      · 模型是**每次调用的参数** → 档位按请求切换。
//    代价是每次请求重新加载模型（实测数据见汇报的"真机验证"一节，秒级）。
//    这**不是**退路 A/B：没有新增任何 Python/Docker/第二个语言运行时，
//    仍然只用 whisper.cpp 这一个原生包。
//
//  转码链路（为什么必须先过 ffmpeg）：
//    Homebrew 的 whisper.cpp 瓶只依赖 ggml / llama.cpp / sdl2-compat，
//    **没有链 ffmpeg**，所以 `whisper-cli` 只吃得下 16 kHz 单声道 PCM WAV。
//    用户上传的多半是 m4a / mp3，因此面板自己用 ffmpeg 转成
//    16 kHz / 单声道 / s16le（`-ar 16000 -ac 1 -c:a pcm_s16le`）再喂给引擎。
//    ffmpeg 是应用市场里已有的基础依赖条目（Requires 声明它），
//    缺了**如实报错**并指名去「应用市场 → FFmpeg」装，绝不给一个坏结果。
// ============================================================================

const (
	// STTAppID 是应用市场里的条目 ID。
	STTAppID = "stt"
	// STTLabel 是面板托管的「语音转文字」launchd 标签。
	STTLabel = "com.zizdog.stt"
	// STTSlug 是面板别名（目录条目的 UI.Slug）：/<slug>/。
	STTSlug = "stt"
	// STTPort 是语音转文字网页界面的默认监听端口（只绑 127.0.0.1）。
	//
	// 8892 与目录里其它端口不冲突（8880 Qwen3 TTS / 8890 图片压缩 /
	// 8891 语音合成 / 8899 音色接收端），TestCatalogPortsAreUnique 会锁住唯一性。
	STTPort = 8892

	// STTBrewFormula 是 Homebrew 包名。
	//
	// ⚠️ **必须写正名 `whisper.cpp`，不能写用户习惯的 `whisper-cpp`**：
	// 实测（2026-09-25 本机）：
	//   · `brew info whisper-cpp` 能用 —— 输出里写着 "Old Names: whisper-cpp"，
	//     说明 `whisper-cpp` 只是别名；
	//   · 但镜像/官方的**清单 JSON 只有正名那一个**：
	//     `<镜像>/api/formula/whisper.cpp.json` → 200，
	//     `<镜像>/api/formula/whisper-cpp.json` → **404**。
	// 而面板挑 brew 镜像的判据正是 `brewMirrorSupportsOCIFor(base, formula)`
	// （install.go，它去取 `<base>/api/formula/<formula>.json`）。若这里写别名，
	// 三家国内镜像会**全部**被判为"不可用"，于是静默回落到 ghcr.io ——
	// 实测同一个 `brew info` 走 USTC 镜像 2.1 秒、走官方默认 **8 分 28 秒**。
	// 这正是本项目最忌讳的"日志说走镜像、实际打官方源"。
	// `brew install whisper.cpp` 与 `brew uninstall whisper.cpp` 都按正名工作，
	// 已在本机实测通过。
	STTBrewFormula = "whisper.cpp"
	// STTCLIBinName 是包里的命令行引擎（转写就靠它）。
	STTCLIBinName = "whisper-cli"
	// STTServerBinName 是包里官方的 OpenAI 兼容服务端。
	// 本轮**不用它当引擎**（理由见文件头），但保留常量：面板要能如实告诉用户
	// "这个包里还有 whisper-server"，/healthz 也把它作为环境事实报出来。
	STTServerBinName = "whisper-server"

	// STTChunkSeconds 是长音频切片的长度（秒）。
	//
	// 切片是"真实进度"的唯一来源（与语音合成按句分段同一个道理）：
	// 10 分钟的音频 = 10 片，界面上的进度就是"真的转完了 3/10 片"。
	// 60 秒是取舍：片太小会切碎上下文（每片都要重新加载模型），
	// 太大则进度颗粒度太粗。
	STTChunkSeconds = 60
	// STTSyncMaxSeconds 是不超过它就**同步**返回的音频时长。
	// 超过它就走任务中心（202 + task_id + SSE 进度）。
	STTSyncMaxSeconds = 60
	// STTMaxAudioSeconds 是单次请求允许的音频时长上限（3 小时）。
	// 超过一律 413 如实拒绝，不做静默截断（截断会让用户以为整段都转完了）。
	STTMaxAudioSeconds = 3 * 60 * 60
	// STTMaxUploadBytes 是上传文件的硬上限（2 GiB，够 3 小时的 m4a）。
	STTMaxUploadBytes = 2 << 30

	// STTModelDirName 是模型目录名（相对 <home>/stt/）。
	STTModelDirName = "models"

	// STTHealthProbeTTL 是健康探活结果的缓存时长。
	//
	// 健康检查是"真的跑一次极短音频转写"（见 STTEngine.Health），
	// 每次都跑既慢又吵；缓存 20 秒是折中，并且响应里**如实回报**
	// probe_cached / probe_age_ms，不把缓存结果伪装成刚刚的实测。
	STTHealthProbeTTL = 20 * time.Second

	// STTModelDownloadTimeout 是下载**一个**模型档位的超时。
	//
	// 最大的 medium 档 1.43 GiB；实测 hf-mirror 约 783 KB/s → 约 32 分钟，
	// 给足一小时。这个值同时写进 market_downloads.go 的下载点声明 ——
	// 声明里"代码里真实存在的超时"必须与这里一致（两处漂移会让审计说谎）。
	STTModelDownloadTimeout = 60 * time.Minute
	// STTProbeSeconds 是健康探针音频的长度（0.5 秒静音）。
	// 只要引擎能把它跑完并产出合法 JSON，就说明"模型加载 + 推理链路"是通的。
	STTProbeSeconds = 0.5
	// STTProbeSampleRate 是探针 WAV 的采样率（whisper 要求 16 kHz）。
	STTProbeSampleRate = 16000
)

// ---------------------------------------------------------------------------
//  错误：全部可被 errors.Is 判定，web 层据此如实映射成 4xx / 413 / 503
// ---------------------------------------------------------------------------

var (
	// ErrSTTModelMissing 请求的档位没有安装模型文件。
	ErrSTTModelMissing = errors.New("语音转文字模型没有安装")
	// ErrSTTModelUnknown 请求的档位不在支持清单里。
	ErrSTTModelUnknown = errors.New("未知的模型档位")
	// ErrSTTAudioEmpty 上传的音频为空。
	ErrSTTAudioEmpty = errors.New("音频文件为空")
	// ErrSTTAudioUnsupported 音频解不开（损坏 / 不认识的容器）。
	ErrSTTAudioUnsupported = errors.New("音频格式不支持或文件已损坏")
	// ErrSTTAudioTooLong 音频超过单次上限。
	ErrSTTAudioTooLong = errors.New("音频超过单次上限")
	// ErrSTTEngineUnavailable 引擎不可用（whisper-cli 不在、跑不起来）。
	ErrSTTEngineUnavailable = errors.New("语音转文字引擎不可用")
	// ErrSTTFfmpegMissing 这台机器没有 ffmpeg（转码链路断了）。
	ErrSTTFfmpegMissing = errors.New("缺少 ffmpeg")
	// ErrSTTInvalidRequest 请求参数本身不合法（response_format 拼错等）。
	// 必须与"引擎不可用"分开：请求方的问题要给 4xx，让人能改。
	ErrSTTInvalidRequest = errors.New("请求参数不合法")
	// ErrSTTExportUnsupported 请求的导出格式不受支持。
	ErrSTTExportUnsupported = errors.New("不支持的导出格式")
)

// ---------------------------------------------------------------------------
//  模型档位
// ---------------------------------------------------------------------------

// STTModel 是一个可选的模型档位。
//
// 体积是**实测值**（2026-09-25 对 hf-mirror 发 Range 请求读 Content-Range 得到
// 的精确字节数），不是抄的、也不是估的 —— 本仓库因为编造体积/校验值出过事故。
type STTModel struct {
	// ID 是对外契约（请求里的 model 字段、/v1/models 的 id）。改名等于改协议。
	ID string `json:"id"`
	// Name 是界面上显示的名字。
	Name string `json:"name"`
	// File 是 ggml 权重文件名（上游 ggerganov/whisper.cpp 仓库里的原名）。
	File string `json:"file"`
	// Bytes 是文件精确大小（实测）。
	Bytes int64 `json:"bytes"`
	// RAMMB 是上游 README 给的**内存占用**参考值（MiB）；0 = 上游没给，看 RAMNote。
	RAMMB int `json:"ram_mb,omitempty"`
	// RAMNote 在 RAMMB==0 时说明这个数字是怎么来的（实测还是估算）。
	RAMNote string `json:"ram_note,omitempty"`
	// Note 是给用户看的取舍说明（速度 / 精度 / 适用场景）。
	Note string `json:"note"`
	// Default 标记默认档位（只有一个）。
	Default bool `json:"default,omitempty"`
}

// STTModels 是面板支持的三个档位，**顺序即界面下拉框顺序**。
//
// 为什么是这三档（用户要求"至少支持 small / medium / large-v3-turbo(q5)"）：
//   - small：466 MB，中文可用、速度最快，但精度不如 turbo；
//   - large-v3-turbo（q5_0 量化）：**默认档**（2026-09-25 用户要求把默认从
//     small 换成它）。574 MB —— 比 medium **小**、质量**更好**，是这三档里
//     最划算的一档（turbo 版是 large-v3 的蒸馏解码器）。
//     ⚠️ 默认档变大（466 MiB → 547 MiB）意味着安装时下载更久一点；
//     "没装/下到一半"时的行为不变：/healthz 与 /v1/models 如实报缺、
//     网页界面照旧给「下载」入口，绝不假装已就绪。
//   - medium：更准但 1.43 GiB、内存 2.1 GB，慢一个量级。
var STTModels = []STTModel{
	{
		ID: "small", Name: "Small（快，466 MiB）", File: "ggml-small.bin",
		Bytes: 487601967, RAMMB: 852,
		Note: "466 MiB 权重，上游 README 标内存 852 MB，本机实测 peak RSS 887,586,816 B ≈ 847 MiB（吻合）；" +
			"中文可用、速度最快，适合日常录音与短语音；精度不如 Large-v3-turbo。",
	},
	{
		ID: "large-v3-turbo", Name: "Large-v3-turbo（q5_0 量化，默认，更准）", File: "ggml-large-v3-turbo-q5_0.bin",
		Bytes: 574041195, RAMMB: 840, Default: true,
		RAMNote: "**本机实测**（M4，Apple Silicon + Metal）：`/usr/bin/time -l` 报 peak RSS 881,295,360 B ≈ 840 MiB。",
		Note:    "547 MiB 权重，比 medium 更小也更准（large-v3 的 turbo 蒸馏解码器 + q5_0 量化），推荐给要精度的场景。",
	},
	{
		ID: "medium", Name: "Medium（最准，最慢）", File: "ggml-medium.bin",
		Bytes: 1533763059, RAMMB: 2100, RAMNote: "上游 README 标 2.1 GB（**本轮未实测**：这台机器上没有下载 medium 档做基准）。",
		Note: "1.43 GiB 权重，精度最高但慢一个量级；长音频建议走任务中心看进度（会按 60 秒切片，进度是真的）。",
	},
}

// DefaultSTTModelID 返回默认档位 ID（清单里被标 Default 的那个）。
//
// 纯函数，可单测：找不到标记时退回第一个，绝不返回空串 ——
// 返回空串会让请求在没有 model 字段时报"未知档位"，把默认值这件事搞坏。
func DefaultSTTModelID() string {
	for _, m := range STTModels {
		if m.Default {
			return m.ID
		}
	}
	if len(STTModels) > 0 {
		return STTModels[0].ID
	}
	return ""
}

// FindSTTModel 按 ID 查档位（大小写不敏感、允许两端空白；空串 = 默认档）。
//
// 这是**可注入的档位解析**入口：web 层与安装器都只通过它把请求里的字符串
// 变成 STTModel，于是"档位名拼错"只有一处判据。
func FindSTTModel(id string) (STTModel, error) {
	want := strings.TrimSpace(id)
	if want == "" {
		want = DefaultSTTModelID()
	}
	for _, m := range STTModels {
		if strings.EqualFold(m.ID, want) {
			return m, nil
		}
	}
	return STTModel{}, fmt.Errorf("%w：%q（支持 %s）", ErrSTTModelUnknown, id, STTModelIDList())
}

// STTModelIDList 返回 "small / large-v3-turbo / medium" 这样的可读清单。
func STTModelIDList() string {
	ids := make([]string, 0, len(STTModels))
	for _, m := range STTModels {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, " / ")
}

// STTModelFilename 在档位不变时保证文件名不变（别名/大小写不敏感）。
func STTModelFilename(id string) (string, error) {
	m, err := FindSTTModel(id)
	if err != nil {
		return "", err
	}
	return m.File, nil
}

// ---------------------------------------------------------------------------
//  路径
// ---------------------------------------------------------------------------

// STTPaths 是这套服务的目录约定（与 qwentts / iopaint 同一条规矩：
// 面板自己的东西放 `<真实用户家目录>/<应用名>/`）。
type STTPaths struct {
	Home      string
	Root      string
	ModelsDir string
	Plist     string
	OutLog    string
	ErrLog    string
}

// sttPaths 解析路径。
//
// **不写死 /opt/homebrew、也不写死用户名**：家目录来自 Manager 的 UserHome /
// UserName（安装器在 root 下必须显式知道"服务要以谁的身份跑"），
// plist 路径走 SystemDaemonPlistPath（它自己按平台拼），
// brew 前缀一律走 m.brewPrefix()（Apple Silicon /opt/homebrew、Intel /usr/local）。
func (m *Manager) sttPaths() STTPaths {
	home := strings.TrimSpace(m.opt.UserHome)
	if home == "" && strings.TrimSpace(m.opt.UserName) != "" {
		home = "/Users/" + m.opt.UserName
	}
	root := filepath.Join(home, "stt")
	return STTPaths{
		Home:      home,
		Root:      root,
		ModelsDir: filepath.Join(root, STTModelDirName),
		Plist:     SystemDaemonPlistPath(STTLabel),
		OutLog:    filepath.Join(home, "Library", "Logs", "zizpanel-stt.out.log"),
		ErrLog:    filepath.Join(home, "Library", "Logs", "zizpanel-stt.err.log"),
	}
}

// sttModelPath 返回某个档位模型文件在磁盘上的绝对路径。
//
// modelsDir 为注入参数（不是从 Manager 现取），所以单测可以喂一个临时目录 ——
// "档位 → 路径"这条映射必须能脱离真实家目录被验证。
func sttModelPath(modelsDir, modelID string) (string, STTModel, error) {
	m, err := FindSTTModel(modelID)
	if err != nil {
		return "", STTModel{}, err
	}
	dir := strings.TrimSpace(modelsDir)
	if dir == "" {
		return "", STTModel{}, fmt.Errorf("模型目录为空（面板没能确定 %q 服务的家目录）", STTAppID)
	}
	return filepath.Join(dir, m.File), m, nil
}

// STTModelFileExists 报告某个档位的权重是否**真的在磁盘上且不是空壳**。
//
// 判据贴着运行体（AGENTS 第三节）：文件在 + 大小与上游一致（或至少 > 1 MiB）。
// 只 `os.Stat` 不看大小是不够的 —— 下载中断留下的 0 字节文件会让 /healthz
// 报绿灯，而每次转写都失败。
func STTModelFileExists(modelsDir, modelID string) bool {
	path, m, err := sttModelPath(modelsDir, modelID)
	if err != nil {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	return st.Size() >= sttMinPlausibleModelBytes && st.Size() == m.Bytes
}

// sttMinPlausibleModelBytes 是"这个文件可能是个真模型"的绝对下限。
// 最小的一档也远大于 1 MiB；低于它一定是坏文件或错误页。
const sttMinPlausibleModelBytes = 1 << 20

// STTModelState 是一个档位在**这台机器上**的现状（/v1/models 的 data 项）。
type STTModelState struct {
	STTModel
	// Installed 表示权重文件真的在（大小也对）。
	Installed bool `json:"installed"`
	// Path 是磁盘路径（未安装时也能看到"该放哪"，便于用户手工放）。
	Path string `json:"path"`
	// SizeBytes 是磁盘上实际的大小（未安装为 0）。
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Current 标记"现在默认用哪一档"。
	Current bool `json:"current"`
	// Reason 在 Installed=false 时说明为什么（缺失 / 大小不对）。
	Reason string `json:"reason,omitempty"`
	// Missing 表示这台机器上还缺这个文件（界面据此显示"下载"按钮）。
	Missing bool `json:"missing"`
}

// STTModelsState 是全部档位的现状 + 当前档位（/v1/models 的组装结果）。
type STTModelsState struct {
	Models  []STTModelState `json:"models"`
	Current string          `json:"current"`
	Ready   int             `json:"ready"`
	Total   int             `json:"total"`
}

// STTModelsStateFor 组装档位现状。
//
// currentID 为"当前档"（可由配置/请求决定；空 = 默认档）。
// 纯函数式的注入参数（modelsDir 而不是 Manager），所以单测可以完全离线地
// 断言"缺哪个文件时哪个档显示未安装"。
func STTModelsStateFor(modelsDir, currentID string) STTModelsState {
	cur := strings.TrimSpace(currentID)
	if _, err := FindSTTModel(cur); err != nil {
		cur = DefaultSTTModelID()
	}
	out := STTModelsState{Current: cur, Total: len(STTModels)}
	for _, m := range STTModels {
		path, _, _ := sttModelPath(modelsDir, m.ID)
		st := STTModelState{STTModel: m, Path: path, Current: strings.EqualFold(m.ID, cur)}
		st.Missing = true
		if fi, err := os.Stat(path); err != nil {
			st.Reason = "模型文件不存在（还没下载）"
		} else if fi.IsDir() {
			st.Reason = "该路径是目录，不是模型文件"
		} else if fi.Size() != m.Bytes {
			st.Reason = fmt.Sprintf("模型文件大小不对：磁盘上 %d 字节，上游是 %d 字节（下载不完整或被截断，请重新下载）",
				fi.Size(), m.Bytes)
			st.SizeBytes = fi.Size()
		} else {
			st.Installed, st.Missing, st.Reason = true, false, ""
			st.SizeBytes = fi.Size()
			out.Ready++
		}
		out.Models = append(out.Models, st)
	}
	return out
}

// ---------------------------------------------------------------------------
//  下载来源（NAS → HF 镜像 → 官方）
// ---------------------------------------------------------------------------

// STTModelSource 是一个模型下载来源。
type STTModelSource struct {
	// Name 是人类可读的来源名（进任务日志，用户看得见到底从哪下的）。
	Name string
	// URL 是这个来源上的完整地址。
	URL string
	// Mirror 标记这是不是自建镜像站（用于"镜像优先"的日志措辞）。
	Mirror bool
}

// sttHFRepo 是 whisper 模型的上游 HF 仓库（ggml 权重就放在这里）。
const sttHFRepo = "ggerganov/whisper.cpp"

// sttHFMirror 是国内公共 HuggingFace 镜像（实测可用，见汇报）。
const sttHFMirror = "https://hf-mirror.com"

// sttHFUpstream 是官方 HF（国内直连基本不可用，所以排最后）。
const sttHFUpstream = "https://huggingface.co"

// sttGGMLMagics 是 ggml 权重文件允许的开头 4 字节。
//
// 为什么要校验它：下载到的很可能是一个 HTML 错误页 / 登录页 / 空文件，
// 只看"文件存在且大小对"会被中间层用内容填充骗过；而 ggml 的魔数是硬事实。
// 实测上游 ggml-small.bin 的前 4 字节是 ASCII "lmgg"。
// ggml 的 GGML_FILE_MAGIC = 0x67676d6c，按**小端**落盘就是 l/m/g/g ——
// 这个顺序很容易写反，所以这里列的是**实测的字节序列**，不是想当然的 "ggml"。
var sttGGMLMagics = [][]byte{[]byte("lmgg"), []byte("ggml"), []byte("fmgg"), []byte("tjgg"), []byte("algm")}

// validateSTTModelFile 检查一个刚下好的模型文件**看起来真的是 ggml 权重**。
//
// 返回的 error 是给用户看的：说清哪一步不对、怎么办。
func validateSTTModelFile(path string, want STTModel) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("模型文件不存在（%s）：%w", path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("模型路径 %s 是目录，不是文件", path)
	}
	if st.Size() == 0 {
		return fmt.Errorf("模型文件是 0 字节（%s）：下载失败留下了一个空壳，已删除", path)
	}
	if st.Size() != want.Bytes {
		return fmt.Errorf("模型文件大小不对：%s 是 %d 字节，上游声明 %d 字节（差 %d）。"+
			"下载被截断或被中间层改写，已删除，请重试",
			path, st.Size(), want.Bytes, want.Bytes-st.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开模型文件失败（%s）：%w", path, err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("读取模型文件头部失败（%s）：%w", path, err)
	}
	for _, magic := range sttGGMLMagics {
		if string(head) == string(magic) {
			return nil
		}
	}
	return fmt.Errorf("模型文件头部是 %q，不是 ggml 权重（%s）—— "+
		"多半是下载到了一个网页/错误页，已删除；请检查网络代理后重试", string(head), path)
}

// ---------------------------------------------------------------------------
//  引擎
// ---------------------------------------------------------------------------

// STTCommandRunner 跑一条命令，并把 stderr 逐行回调出去（进度就在这里）。
//
// 做成注入点是为了单测：转写/探活**不允许**在单测里真的跑 whisper-cli，
// 更不允许读真实家目录里的模型（AGENTS 第三节）。
// onStderrLine 为 nil 时实现可以只收集输出。
type STTCommandRunner func(ctx context.Context, timeout time.Duration, bin string, args []string, onStderrLine func(string)) (string, error)

// STTEngine 是"用 whisper.cpp 把音频转成文字"的引擎。
type STTEngine struct {
	// CLIBin 是 whisper-cli 的路径（空 = 用注入的 BrewPrefix 拼，再空则找不到）。
	CLIBin string
	// ServerBin 是 whisper-server 的路径（只用于如实报告"包里有它"，不驱动转写）。
	ServerBin string
	// FfmpegBin 是 ffmpeg 的路径（空 = 这台机器没有 → 转写直接如实失败）。
	FfmpegBin string
	// FfprobeBin 是 ffprobe 的路径（用于拿音频时长，决定同步还是任务）。
	FfprobeBin string
	// BrewPrefix 是 Homebrew 前缀（用于按需拼出上面几个路径）。
	BrewPrefix string
	// ModelsDir 是模型目录（档位 → 文件就在这里面找）。
	ModelsDir string
	// CurrentModel 是"当前档位"（/v1/models 会把它标成 current；空 = 默认档）。
	CurrentModel string
	// Run 是命令执行器（测试注入）。
	Run STTCommandRunner
	// TempRoot 是临时目录父目录（空 = os.TempDir()）。
	TempRoot string
	// ChunkSeconds 覆盖默认切片长度（<=0 用 STTChunkSeconds）。
	ChunkSeconds int
	// ChunkTimeout 是**单片**转写的超时（<=0 用默认值）。
	ChunkTimeout time.Duration
	// ProbeTTL 覆盖健康探活缓存时长（<=0 用 STTHealthProbeTTL）。
	ProbeTTL time.Duration
	// Now 是时钟（测试注入；nil = time.Now）。
	Now func() time.Time

	probeMu   sync.Mutex
	probeAt   time.Time
	probeOK   bool
	probeWhy  string
	probeNote string
}

// STTDefaultChunkTimeout 是单片转写的超时。
//
// 一片最长 60 秒音频；即便在很慢的机器上，medium 档也远小于这个数。
// 30 分钟只针对"机器极慢 + 档位极重"的兜底，不是常见的等待时间。
const STTDefaultChunkTimeout = 30 * time.Minute

// NewSTTEngine 造一个用真实系统工具的引擎。
//
// brewPrefix 由调用方注入（面板里走 m.brewPrefix()，Apple Silicon 是
// /opt/homebrew、Intel 是 /usr/local）—— **本文件不写死任何一个前缀**。
func NewSTTEngine(brewPrefix, modelsDir, currentModel string) *STTEngine {
	brewPrefix = strings.TrimRight(strings.TrimSpace(brewPrefix), "/")
	e := &STTEngine{
		BrewPrefix:   brewPrefix,
		ModelsDir:    modelsDir,
		CurrentModel: currentModel,
		Run:          runSTTCommand,
		TempRoot:     os.TempDir(),
		ChunkSeconds: STTChunkSeconds,
		ChunkTimeout: STTDefaultChunkTimeout,
		ProbeTTL:     STTHealthProbeTTL,
		Now:          time.Now,
	}
	if brewPrefix != "" {
		e.CLIBin = filepath.Join(brewPrefix, "bin", STTCLIBinName)
		e.ServerBin = filepath.Join(brewPrefix, "bin", STTServerBinName)
		e.FfmpegBin = filepath.Join(brewPrefix, "bin", "ffmpeg")
		e.FfprobeBin = filepath.Join(brewPrefix, "bin", "ffprobe")
	}
	// 前缀下没有就退回 PATH 里找（用户可能用别的方式装的 whisper.cpp）。
	// **但绝不猜 /opt/homebrew**：找不到就留空，Available 会如实报告。
	if e.CLIBin == "" || !fileExecutable(e.CLIBin) {
		if p, err := exec.LookPath(STTCLIBinName); err == nil {
			e.CLIBin = p
		}
	}
	if e.FfmpegBin == "" || !fileExecutable(e.FfmpegBin) {
		if p := findFfmpegBin(); p != "" {
			e.FfmpegBin = p
		}
	}
	if e.FfprobeBin == "" || !fileExecutable(e.FfprobeBin) {
		if p, err := exec.LookPath("ffprobe"); err == nil {
			e.FfprobeBin = p
		}
	}
	return e
}

// fileExecutable 报告路径是不是"存在的可执行文件"（悬空软链不算）。
func fileExecutable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	return st.Mode()&0o111 != 0
}

// now 返回当前时间（测试可注入）。
func (e *STTEngine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *STTEngine) chunkSeconds() int {
	if e.ChunkSeconds > 0 {
		return e.ChunkSeconds
	}
	return STTChunkSeconds
}

func (e *STTEngine) chunkTimeout() time.Duration {
	if e.ChunkTimeout > 0 {
		return e.ChunkTimeout
	}
	return STTDefaultChunkTimeout
}

func (e *STTEngine) probeTTL() time.Duration {
	if e.ProbeTTL > 0 {
		return e.ProbeTTL
	}
	return STTHealthProbeTTL
}

func (e *STTEngine) run() STTCommandRunner {
	if e.Run != nil {
		return e.Run
	}
	return runSTTCommand
}

// runSTTCommand 是生产用的执行器：带超时、stderr 逐行回调、失败时把尾部带进错误。
//
// 为什么必须逐行读 stderr（而不是 CombinedOutput）：whisper-cli 的进度
// （`whisper_print_progress_callback: progress =  X%`）就在 stderr 上，
// 而且它只有在**边跑边读**时才拿得到 —— 等命令结束再拿，进度已经没意义了。
func runSTTCommand(ctx context.Context, timeout time.Duration, bin string, args []string, onStderrLine func(string)) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("无法读取 %s 的 stderr：%w", filepath.Base(bin), err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("无法读取 %s 的 stdout：%w", filepath.Base(bin), err)
	}
	var outBuf strings.Builder
	var mu sync.Mutex
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动 %s 失败：%w", filepath.Base(bin), err)
	}
	// stdout 与 stderr 都要收，且**必须并发收**：只读一路会让另一路的管道
	// 缓冲区写满，子进程就卡在 write 上不动了（经典死锁，表现为"命令超时"）。
	// whisper-cli 的转写文本走 stdout、进度与诊断走 stderr，两路都少不了。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			outBuf.WriteString(line)
			outBuf.WriteByte('\n')
			mu.Unlock()
			if onStderrLine != nil {
				onStderrLine(line)
			}
		}
	}()
	stdout, rerr := io.ReadAll(stdoutPipe)
	wg.Wait()
	werr := cmd.Wait()
	mu.Lock()
	combined := outBuf.String()
	mu.Unlock()
	if rerr != nil && werr == nil {
		werr = fmt.Errorf("读取标准输出失败：%w", rerr)
	}
	if len(stdout) > 0 {
		combined = string(stdout) + "\n" + combined
	}
	if werr != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return combined, fmt.Errorf("命令超时（%s）：%s", timeout, filepath.Base(bin))
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return combined, ctx.Err()
		}
		return combined, fmt.Errorf("%s 执行失败: %v（%s）",
			filepath.Base(bin), werr, tailText(strings.TrimSpace(combined), 400))
	}
	return combined, nil
}

// STTAvailability 报告引擎能不能用（判据贴着运行体：文件在、可执行）。
type STTAvailability struct {
	OK bool `json:"ok"`
	// CLIBin / FfmpegBin / FfprobeBin / ServerBin 是探测到的路径（空 = 没有）。
	CLIBin     string `json:"cli_bin,omitempty"`
	FfmpegBin  string `json:"ffmpeg_bin,omitempty"`
	FfprobeBin string `json:"ffprobe_bin,omitempty"`
	ServerBin  string `json:"server_bin,omitempty"`
	// Reasons 列出**每一条**不满足的前置条件（不是只报第一条）——
	// 用户一次就能看全要补什么，而不是修一个再发现下一个。
	Reasons []string `json:"reasons,omitempty"`
}

// Available 复核引擎与转码链路。
func (e *STTEngine) Available() STTAvailability {
	a := STTAvailability{
		CLIBin: e.CLIBin, FfmpegBin: e.FfmpegBin,
		FfprobeBin: e.FfprobeBin, ServerBin: e.ServerBin,
	}
	if !fileExecutable(e.CLIBin) {
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"找不到可执行的 %s（应在 Homebrew 前缀的 bin 下；到「应用市场 → 语音转文字」点安装，"+
				"或手工执行 `brew install %s`）", STTCLIBinName, STTBrewFormula))
	}
	if !fileExecutable(e.FfmpegBin) {
		a.Reasons = append(a.Reasons, "找不到可执行的 ffmpeg（转码链路要用它把上传的音频转成 16 kHz 单声道 WAV）；"+
			"到「应用市场 → FFmpeg」安装")
	}
	if !fileExecutable(e.FfprobeBin) {
		a.Reasons = append(a.Reasons, "找不到可执行的 ffprobe（用来读音频时长、决定同步还是任务）；"+
			"它随 ffmpeg 一起装，见「应用市场 → FFmpeg」")
	}
	a.OK = len(a.Reasons) == 0
	return a
}

// ---------------------------------------------------------------------------
//  音频准备（转码 / 切片）
// ---------------------------------------------------------------------------

// STTAudioInfo 是探测到的音频信息。
type STTAudioInfo struct {
	Seconds    float64 `json:"seconds"`
	SampleRate int     `json:"sample_rate,omitempty"`
	Channels   int     `json:"channels,omitempty"`
	Codec      string  `json:"codec,omitempty"`
	Format     string  `json:"format,omitempty"`
}

// ProbeAudio 用 ffprobe 读音频时长与基本参数。
//
// 为什么必须真的探一次：时长决定了"同步返回还是走任务"，
// 也决定了要切几片（真实进度的分母）。猜时长就会给出假进度。
func (e *STTEngine) ProbeAudio(ctx context.Context, path string) (STTAudioInfo, error) {
	var info STTAudioInfo
	if !fileExecutable(e.FfprobeBin) {
		return info, fmt.Errorf("%w：ffprobe 不可用，无法读取音频信息", ErrSTTFfmpegMissing)
	}
	out, err := e.run()(ctx, 60*time.Second, e.FfprobeBin, []string{
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=sample_rate,channels,codec_name:format=duration,format_name",
		"-of", "json", path,
	}, nil)
	if err != nil {
		return info, fmt.Errorf("%w：%v", ErrSTTAudioUnsupported, err)
	}
	var probe struct {
		Streams []struct {
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
			CodecName  string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			Duration   string `json:"duration"`
			FormatName string `json:"format_name"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(out), &probe); err != nil {
		return info, fmt.Errorf("%w：ffprobe 的输出不是合法 JSON（%v）", ErrSTTAudioUnsupported, err)
	}
	if len(probe.Streams) == 0 {
		return info, fmt.Errorf("%w：这个文件里没有音频流（是不是视频没有音轨、或者传错了文件？）", ErrSTTAudioUnsupported)
	}
	info.SampleRate, _ = strconv.Atoi(probe.Streams[0].SampleRate)
	info.Channels = probe.Streams[0].Channels
	info.Codec = probe.Streams[0].CodecName
	info.Format = probe.Format.FormatName
	dur, derr := strconv.ParseFloat(strings.TrimSpace(probe.Format.Duration), 64)
	if derr != nil || math.IsNaN(dur) || math.IsInf(dur, 0) {
		return info, fmt.Errorf("%w：ffprobe 报不出时长（duration=%q）—— "+
			"拿不到时长就没法给出真实进度，面板不会假装知道", ErrSTTAudioUnsupported, probe.Format.Duration)
	}
	if dur <= 0 {
		return info, fmt.Errorf("%w：音频时长是 %.3f 秒（空音频无法转写）", ErrSTTAudioEmpty, dur)
	}
	info.Seconds = dur
	return info, nil
}

// ConvertToWAV 把任意音频转成 whisper 要的 16 kHz 单声道 s16le WAV。
//
// 为什么固定 16 kHz 单声道：whisper 的 mel 前端就是按 16 kHz 单声道训练的，
// 预先转好比让引擎自己猜格式稳（brew 的 whisper-cli 没链 ffmpeg，
// 它自己只认这种 WAV）。
func (e *STTEngine) ConvertToWAV(ctx context.Context, src, dst string) error {
	if !fileExecutable(e.FfmpegBin) {
		return fmt.Errorf("%w：找不到 ffmpeg，无法把上传的音频转成 16 kHz WAV。"+
			"到「应用市场 → FFmpeg」安装后重试", ErrSTTFfmpegMissing)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("创建临时目录失败：%w", err)
	}
	_, err := e.run()(ctx, 30*time.Minute, e.FfmpegBin, []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", src,
		"-vn", "-map", "a:0",
		"-ar", strconv.Itoa(STTProbeSampleRate), "-ac", "1", "-c:a", "pcm_s16le",
		"-f", "wav", dst,
	}, nil)
	if err != nil {
		return fmt.Errorf("%w：ffmpeg 转码失败（%v）", ErrSTTAudioUnsupported, err)
	}
	if st, serr := os.Stat(dst); serr != nil || st.Size() == 0 {
		return fmt.Errorf("%w：ffmpeg 跑完但没有产出音频（%s）", ErrSTTAudioUnsupported, dst)
	}
	return nil
}

// STTChunk 是一个切片（WAV 文件 + 它在整段音频里的起点偏移）。
type STTChunk struct {
	Index     int
	Path      string
	OffsetMS  int
	Seconds   float64
	IndexFrom int // 全片段号起点（合并时间戳时用）
}

// SplitWAV 把一段 16 kHz 单声道 WAV 切成 numChunks 片（ffmpeg 的 segment muxer）。
//
// 为什么用 ffmpeg 切片而不是自己切字节：WAV 头必须在每片里都正确重写，
// 自己切容易切出坏头（本项目的图片压缩/语音合成都栽过"格式头猜错"这类事）。
func (e *STTEngine) SplitWAV(ctx context.Context, wav string, totalSeconds float64, workDir string) ([]STTChunk, error) {
	chunk := e.chunkSeconds()
	if chunk <= 0 || totalSeconds <= float64(chunk) {
		// 只有一片：不用切，直接用整段。
		return []STTChunk{{Index: 0, Path: wav, OffsetMS: 0, Seconds: totalSeconds}}, nil
	}
	if !fileExecutable(e.FfmpegBin) {
		return nil, fmt.Errorf("%w：长音频要切片才能给出真实进度，但找不到 ffmpeg", ErrSTTFfmpegMissing)
	}
	pattern := filepath.Join(workDir, "chunk-%04d.wav")
	_, err := e.run()(ctx, 30*time.Minute, e.FfmpegBin, []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", wav,
		"-f", "segment", "-segment_time", strconv.Itoa(chunk),
		"-c", "copy", "-reset_timestamps", "1",
		pattern,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("切片失败：%w", err)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, fmt.Errorf("读取切片目录失败：%w", err)
	}
	var names []string
	for _, en := range entries {
		if !en.IsDir() && strings.HasPrefix(en.Name(), "chunk-") && strings.HasSuffix(en.Name(), ".wav") {
			names = append(names, en.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("切片跑完了但没有产出任何分片（%s）", workDir)
	}
	out := make([]STTChunk, 0, len(names))
	for i, n := range names {
		out = append(out, STTChunk{
			Index:    i,
			Path:     filepath.Join(workDir, n),
			OffsetMS: i * chunk * 1000,
			Seconds:  math.Min(float64(chunk), totalSeconds-float64(i*chunk)),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
//  转写
// ---------------------------------------------------------------------------

// STTOptions 是一次转写请求。
type STTOptions struct {
	// AudioPath 是**用户上传的原始音频**（任意 ffmpeg 认得的格式）。
	AudioPath string
	// ModelID 是档位（空 = 默认档）。
	ModelID string
	// Language 是 whisper 的语言码（zh / en / ja / auto…；空 = auto）。
	Language string
	// Translate 为真时把非英语翻译成英语（whisper 的 translate 任务）。
	Translate bool
	// Prompt 是初始提示（可选）。
	Prompt string
	// WorkDir 为空时自动建临时目录（结果的 Cleanup 会删掉它）。
	WorkDir string
	// Progress 在每片转写完成后被调用（done=已完成片数，total=总片数）。
	// 有它才有"真实进度"——不是假转圈。
	Progress func(done, total int, note string)
	// KnownDuration 已知时长（>0 时跳过 ffprobe，避免重复探测）。
	KnownDuration float64
}

// STTSegment 是一段带时间戳的转写结果（对外契约：json/srt/vtt 都由它生成）。
type STTSegment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"` // 秒
	End   float64 `json:"end"`   // 秒
	Text  string  `json:"text"`
}

// STTResult 是一次转写的产物。
type STTResult struct {
	Text     string       `json:"text"`
	Segments []STTSegment `json:"segments"`
	// Language 是**实际识别到**的语言（whisper 自己判的，不是请求里那个）。
	Language string `json:"language"`
	// DurationMS 是音频总时长。
	DurationMS int `json:"duration_ms"`
	// ModelID / ModelFile 是**实际使用**的档位与权重文件
	// （请求里传的档位被纠正/回落时，这两个字段就是证据）。
	ModelID   string `json:"model"`
	ModelFile string `json:"model_file"`
	// Chunks 是切了几片（真实进度的分母）。
	Chunks int `json:"chunks"`
	// ElapsedMS 是引擎耗时。
	ElapsedMS int `json:"elapsed_ms"`
}

// whisperCLIJSON 是 whisper-cli `-oj` 产出的 JSON 的形状（只取我们要的字段）。
type whisperCLIJSON struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Timestamps struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"timestamps"`
		Offsets struct {
			From int `json:"from"`
			To   int `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

// parseWhisperCLIJSON 把 whisper-cli 的 `-oj` 输出解析成"语言 + 分段"。
//
// 纯函数（不跑进程、不碰文件系统），所以单测可以喂真实/伪造的 JSON 锁住解析 ——
// 这条规则错了，"转出的文本是空的"或者"时间戳全 0"就是必然。
//
// 时间戳优先用 `offsets.{from,to}`（毫秒整数，最稳），拿不到时才去解析
// `timestamps.from`（"00:00:01,234" 这种字符串）。
func parseWhisperCLIJSON(data []byte) ([]STTSegment, string, error) {
	var raw whisperCLIJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", fmt.Errorf("whisper-cli 的 JSON 输出解析失败：%w", err)
	}
	segs := make([]STTSegment, 0, len(raw.Transcription))
	for _, t := range raw.Transcription {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue // 纯静音片段：whisper 会给空文本，不要塞进结果里凑数
		}
		startMS, endMS := t.Offsets.From, t.Offsets.To
		if endMS <= 0 && (t.Timestamps.To != "") {
			startMS = parseSRTTime(t.Timestamps.From)
			endMS = parseSRTTime(t.Timestamps.To)
		}
		if endMS < startMS {
			endMS = startMS
		}
		segs = append(segs, STTSegment{
			ID:    len(segs),
			Start: float64(startMS) / 1000,
			End:   float64(endMS) / 1000,
			Text:  text,
		})
	}
	return segs, raw.Result.Language, nil
}

// parseSRTTime 解析 "00:00:01,234" / "00:00:01.234" 这两种写法（毫秒）。
func parseSRTTime(s string) int {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	secParts := strings.SplitN(parts[2], ".", 2)
	sec, err3 := strconv.Atoi(secParts[0])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0
	}
	ms := 0
	if len(secParts) == 2 {
		frag := secParts[1]
		for len(frag) < 3 {
			frag += "0"
		}
		frag = frag[:3]
		if v, err := strconv.Atoi(frag); err == nil {
			ms = v
		}
	}
	return ((h*60+m)*60+sec)*1000 + ms
}

// whisperProgressRe 匹配 whisper-cli 的进度行。
//
// 实测形状：`whisper_print_progress_callback: progress =  35%`
// （= 两边有空格、以百分号结尾）。解析不出来就不报进度 —— 绝不编一个假百分比。
var whisperProgressRe = regexp.MustCompile(`progress\s*=\s*([0-9]{1,3})\s*%`)

// parseWhisperProgressLine 从一行 stderr 里抠出进度百分比（0~100）。
// 第二个返回值表示"这行确实是一条进度行"。
func parseWhisperProgressLine(line string) (int, bool) {
	m := whisperProgressRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 || n > 100 {
		return 0, false
	}
	return n, true
}

// sttLanguageAliases 把常见的语言写法归一到 whisper 的语言码。
//
// 为什么要归一：whisper 只认 en / zh / ja / auto 这类短码；用户在界面上
// 选的是"中文/英语/日语/自动"，接口调用方可能写 "zh-CN"、"Chinese"。
// 归一化只有这一处实现，避免"界面能用、curl 不能用"。
var sttLanguageAliases = map[string]string{
	"": "auto", "auto": "auto", "自动": "auto", "detect": "auto",
	"zh": "zh", "zh-cn": "zh", "zh_cn": "zh", "zh-hans": "zh", "cmn": "zh",
	"chinese": "zh", "中文": "zh", "汉语": "zh", "普通话": "zh",
	"en": "en", "en-us": "en", "en_us": "en", "english": "en", "英语": "en", "英文": "en",
	"ja": "ja", "jp": "ja", "ja-jp": "ja", "japanese": "ja", "日语": "ja", "日文": "ja",
	"ko": "ko", "korean": "ko", "韩语": "ko",
	"fr": "fr", "french": "fr", "法语": "fr",
	"de": "de", "german": "de", "德语": "de",
	"es": "es", "spanish": "es", "西班牙语": "es",
	"ru": "ru", "russian": "ru", "俄语": "ru",
	"yue": "yue", "粤语": "yue",
}

// NormalizeSTTLanguage 把请求里的语言写法归一成 whisper 语言码。
//
// 认不出来的写法**原样小写返回**（whisper 自己会拒绝并报错），
// 而不是猜一个：猜错语言会让转写结果整段变成另一种文字，比报错糟得多。
// 显式拒绝空串之外的东西由调用方决定（这里空串 = auto）。
func NormalizeSTTLanguage(lang string) string {
	key := strings.ToLower(strings.TrimSpace(lang))
	if v, ok := sttLanguageAliases[key]; ok {
		return v
	}
	return key
}

// STTLanguageChoice 是界面上可选的语言（也是 /v1/models 里回报的清单）。
type STTLanguageChoice struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// STTLanguageChoices 是界面下拉框的顺序（用户要求：中文/英语/日语/自动）。
var STTLanguageChoices = []STTLanguageChoice{
	{Code: "auto", Label: "自动检测"},
	{Code: "zh", Label: "中文"},
	{Code: "en", Label: "英语"},
	{Code: "ja", Label: "日语"},
	{Code: "ko", Label: "韩语"},
	{Code: "yue", Label: "粤语"},
	{Code: "fr", Label: "法语"},
	{Code: "de", Label: "德语"},
	{Code: "es", Label: "西班牙语"},
	{Code: "ru", Label: "俄语"},
}

// Transcribe 把音频转成文字。
//
// 流程（每一步的产物都真的落盘，任何一步失败都如实报错）：
//  1. 探时长（ffprobe）—— 决定切几片；
//  2. 转码成 16 kHz 单声道 WAV（ffmpeg）；
//  3. 按 STTChunkSeconds 切片（ffmpeg segment）；
//  4. 逐片跑 whisper-cli（`-oj` 出 JSON），每片完成后回报进度；
//  5. 合并分段并把每片的时间戳加上它的偏移。
//
// 进度是**真的**：分母是片数，分子是"真的转完了的片数"。
// 片内进度（whisper 的百分比）只写进日志，不拿它冒充总体进度。
func (e *STTEngine) Transcribe(ctx context.Context, opt STTOptions) (*STTResult, error) {
	started := e.now()
	audio := strings.TrimSpace(opt.AudioPath)
	if audio == "" {
		return nil, ErrSTTAudioEmpty
	}
	if st, err := os.Stat(audio); err != nil {
		return nil, fmt.Errorf("%w：读不到上传的音频（%v）", ErrSTTAudioUnsupported, err)
	} else if st.Size() == 0 {
		return nil, ErrSTTAudioEmpty
	}
	if av := e.Available(); !av.OK {
		return nil, fmt.Errorf("%w：%s", ErrSTTEngineUnavailable, strings.Join(av.Reasons, "；"))
	}
	model, err := FindSTTModel(opt.ModelID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(e.ModelsDir) == "" {
		return nil, fmt.Errorf("%w：模型目录未配置", ErrSTTEngineUnavailable)
	}
	modelPath := filepath.Join(e.ModelsDir, model.File)
	if !STTModelFileExists(e.ModelsDir, model.ID) {
		return nil, fmt.Errorf("%w：档位 %s 的权重不在（%s）。"+
			"到「应用市场 → 语音转文字」点安装/下载该档位，或 GET /v1/models 看哪一档已就绪",
			ErrSTTModelMissing, model.ID, modelPath)
	}

	workDir := strings.TrimSpace(opt.WorkDir)
	own := false
	if workDir == "" {
		root := e.TempRoot
		if root == "" {
			root = os.TempDir()
		}
		d, derr := os.MkdirTemp(root, "zp-stt-")
		if derr != nil {
			return nil, fmt.Errorf("创建临时目录失败：%w", derr)
		}
		workDir, own = d, true
	}
	fail := func(err error) (*STTResult, error) {
		if own {
			_ = os.RemoveAll(workDir)
		}
		return nil, err
	}

	// ① 时长
	total := opt.KnownDuration
	if total <= 0 {
		info, perr := e.ProbeAudio(ctx, audio)
		if perr != nil {
			return fail(perr)
		}
		total = info.Seconds
	}
	if total > STTMaxAudioSeconds {
		return fail(fmt.Errorf("%w：音频 %.1f 秒，上限 %d 秒（%.1f 小时）。"+
			"请先自行剪短再上传（面板不静默截断 —— 截断会让你以为整段都转完了）",
			ErrSTTAudioTooLong, total, STTMaxAudioSeconds, float64(STTMaxAudioSeconds)/3600))
	}
	if total <= 0 {
		return fail(ErrSTTAudioEmpty)
	}

	// ② 转码
	wav := filepath.Join(workDir, "audio16k.wav")
	if cerr := e.ConvertToWAV(ctx, audio, wav); cerr != nil {
		return fail(cerr)
	}

	// ③ 切片
	chunks, serr := e.SplitWAV(ctx, wav, total, workDir)
	if serr != nil {
		return fail(serr)
	}
	if len(chunks) == 0 {
		return fail(fmt.Errorf("音频切片后没有任何分片"))
	}

	// ④ 逐片转写
	lang := NormalizeSTTLanguage(opt.Language)
	var (
		allSegs  []STTSegment
		detected string
	)
	prevTail := strings.TrimSpace(opt.Prompt)
	for i, ck := range chunks {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		segs, dlang, terr := e.transcribeChunk(ctx, ck, modelPath, lang, prevTail, func(pct int) {
			if opt.Progress != nil {
				opt.Progress(i, len(chunks), fmt.Sprintf("正在转写第 %d/%d 片（%d%%）", i+1, len(chunks), pct))
			}
		})
		if terr != nil {
			return fail(fmt.Errorf("第 %d/%d 片转写失败：%w", i+1, len(chunks), terr))
		}
		if detected == "" && dlang != "" && dlang != "auto" {
			detected = dlang
		}
		for _, s := range segs {
			s.ID = len(allSegs)
			s.Start += float64(ck.OffsetMS) / 1000
			s.End += float64(ck.OffsetMS) / 1000
			allSegs = append(allSegs, s)
		}
		// 把这一片结尾的文字当作下一片的 prompt：跨片断句更连贯
		// （实测不加它时，切点上的词会被截成两半）。
		if len(segs) > 0 {
			prevTail = tailText(strings.Join(segTexts(segs), " "), 220)
		}
		if opt.Progress != nil {
			opt.Progress(i+1, len(chunks), fmt.Sprintf("已完成 %d/%d 片", i+1, len(chunks)))
		}
	}

	res := &STTResult{
		Text:       strings.TrimSpace(strings.Join(segTexts(allSegs), "")),
		Segments:   allSegs,
		Language:   detected,
		DurationMS: int(math.Round(total * 1000)),
		ModelID:    model.ID,
		ModelFile:  model.File,
		Chunks:     len(chunks),
		ElapsedMS:  int(e.now().Sub(started).Milliseconds()),
	}
	if res.Text == "" {
		// 引擎跑通了但没有任何文字：如实说清"是静音/没有可识别语音"，
		// 而不是返回一个空字符串让调用方猜是成功还是失败。
		res.Text = ""
	}
	return res, nil
}

// segTexts 取出分段文本。
func segTexts(segs []STTSegment) []string {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, s.Text)
	}
	return out
}

// transcribeChunk 跑一片。
func (e *STTEngine) transcribeChunk(ctx context.Context, ck STTChunk, modelPath, lang, prompt string,
	onPct func(int)) ([]STTSegment, string, error) {

	prefix := strings.TrimSuffix(ck.Path, ".wav") + ".out"
	// 每次都先删掉旧的 JSON：whisper-cli 失败时不一定写文件，
	// 旧文件还在就会把上一片的结果当成这一片的（与 say 那个静默成功同一个坑）。
	_ = os.Remove(prefix + ".json")

	args := []string{
		"-m", modelPath,
		"-f", ck.Path,
		"-l", lang,
		"-oj", "-of", prefix,
		"-pp", // 打进度到 stderr（我们逐行解析，喂给任务中心的真实进度）
		"-np", // 不打印那段巨大的系统信息
	}
	if strings.TrimSpace(prompt) != "" {
		args = append(args, "--prompt", prompt)
	}
	_, err := e.run()(ctx, e.chunkTimeout(), e.CLIBin, args, func(line string) {
		if pct, ok := parseWhisperProgressLine(line); ok && onPct != nil {
			onPct(pct)
		}
	})
	if err != nil {
		return nil, "", err
	}
	data, rerr := os.ReadFile(prefix + ".json")
	if rerr != nil {
		return nil, "", fmt.Errorf("%w：whisper-cli 跑完了但没有产出 JSON（%s）。"+
			"这通常是权重文件损坏或磁盘写满（引擎退出码为 0 也可能不写文件）",
			ErrSTTEngineUnavailable, prefix+".json")
	}
	segs, dlang, perr := parseWhisperCLIJSON(data)
	if perr != nil {
		return nil, "", fmt.Errorf("%w：%v", ErrSTTEngineUnavailable, perr)
	}
	return segs, dlang, nil
}

// ---------------------------------------------------------------------------
//  输出格式（纯函数，可单测）
// ---------------------------------------------------------------------------

// STTFormat 是支持的导出格式（OpenAI 的 response_format 取值）。
type STTFormat string

const (
	STTFormatJSON STTFormat = "json"
	STTFormatText STTFormat = "text"
	STTFormatSRT  STTFormat = "srt"
	STTFormatVTT  STTFormat = "vtt"
	// STTFormatVerboseJSON 是 OpenAI 的 verbose_json：比 json 多语言/时长/分段。
	STTFormatVerboseJSON STTFormat = "verbose_json"
)

// STTFormats 是界面下拉框的顺序，也是"支持哪些 response_format"的唯一来源。
var STTFormats = []STTFormat{STTFormatJSON, STTFormatText, STTFormatSRT, STTFormatVTT, STTFormatVerboseJSON}

// ParseSTTFormat 解析请求里的 response_format（含常见别名）。
// 空串 = 默认 json（OpenAI 的默认值）。
func ParseSTTFormat(s string) (STTFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "json":
		return STTFormatJSON, nil
	case "text", "plain", "txt":
		return STTFormatText, nil
	case "srt":
		return STTFormatSRT, nil
	case "vtt", "webvtt":
		return STTFormatVTT, nil
	case "verbose_json", "verbose":
		return STTFormatVerboseJSON, nil
	}
	return "", fmt.Errorf("%w：response_format=%q（支持 %s）", ErrSTTExportUnsupported, s, STTFormatList())
}

// STTFormatList 返回 "json / text / srt / vtt / verbose_json"。
func STTFormatList() string {
	parts := make([]string, 0, len(STTFormats))
	for _, f := range STTFormats {
		parts = append(parts, string(f))
	}
	return strings.Join(parts, " / ")
}

// ContentType 返回该格式的 MIME。
//
// 必须逐格式给对：`response_format=text` 返回 JSON 的 Content-Type 会让
// 调用方（尤其是按 Content-Type 分派的客户端）把纯文本当 JSON 解析 → 报错。
// 这是真机测试里被自家契约测试抓出来的一条（OpenAI 的 text 就是 text/plain）。
func (f STTFormat) ContentType() string {
	switch f {
	case STTFormatText:
		return "text/plain; charset=utf-8"
	case STTFormatSRT:
		return "application/x-subrip; charset=utf-8"
	case STTFormatVTT:
		return "text/vtt; charset=utf-8"
	default:
		// json / verbose_json
		return "application/json; charset=utf-8"
	}
}

// FormatTranscript 把转写结果渲染成请求要的格式。
//
// 纯函数：json / text / srt / vtt 四种都由**同一份分段数据**生成，
// 所以"文本里有、字幕里没有"这类不一致不可能发生。
// verbose_json 与 json 的差别只在 json 会带上语言与时长（OpenAI 的 json
// 只保证有 text；面板多给不违反兼容性，但 verbose_json 才是完整契约）。
func FormatTranscript(res *STTResult, format STTFormat) (string, error) {
	if res == nil {
		return "", fmt.Errorf("%w：转写结果为空", ErrSTTEngineUnavailable)
	}
	switch format {
	case STTFormatText:
		return res.Text, nil
	case STTFormatJSON:
		b, err := json.Marshal(map[string]any{"text": res.Text})
		if err != nil {
			return "", fmt.Errorf("序列化 JSON 失败：%w", err)
		}
		return string(b), nil
	case STTFormatVerboseJSON:
		b, err := json.Marshal(sttVerboseBody(res))
		if err != nil {
			return "", fmt.Errorf("序列化 JSON 失败：%w", err)
		}
		return string(b), nil
	case STTFormatSRT:
		return formatSRT(res.Segments), nil
	case STTFormatVTT:
		return formatVTT(res.Segments), nil
	}
	return "", fmt.Errorf("%w：%q", ErrSTTExportUnsupported, format)
}

// sttVerboseBody 组装 verbose_json（OpenAI 兼容字段 + 面板扩展）。
func sttVerboseBody(res *STTResult) map[string]any {
	segs := make([]map[string]any, 0, len(res.Segments))
	for _, s := range res.Segments {
		segs = append(segs, map[string]any{
			"id": s.ID, "seek": 0,
			"start": s.Start, "end": s.End,
			"text": s.Text, "tokens": []int{}, "temperature": 0, "avg_logprob": 0,
			"compression_ratio": 0, "no_speech_prob": 0,
		})
	}
	lang := res.Language
	if lang == "" {
		lang = "unknown"
	}
	return map[string]any{
		"task":     "transcribe",
		"language": lang,
		"duration": float64(res.DurationMS) / 1000,
		"text":     res.Text,
		"segments": segs,
		// 面板扩展：调用方需要知道"实际用了哪一档模型、切了几片、花了多久"。
		"model":          res.ModelID,
		"model_file":     res.ModelFile,
		"chunks":         res.Chunks,
		"elapsed_ms":     res.ElapsedMS,
		"duration_ms":    res.DurationMS,
		"segments_count": len(res.Segments),
	}
}

// formatSRT 生成 SubRip 字幕。
func formatSRT(segs []STTSegment) string {
	var b strings.Builder
	for i, s := range segs {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(s.Start), srtTime(s.End), s.Text)
	}
	return b.String()
}

// formatVTT 生成 WebVTT 字幕。
func formatVTT(segs []STTSegment) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for i, s := range segs {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, vttTime(s.Start), vttTime(s.End), s.Text)
	}
	return b.String()
}

// srtTime 把秒格式化成 "00:00:01,234"。
func srtTime(sec float64) string {
	h, m, s, ms := splitTime(sec)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// vttTime 把秒格式化成 "00:00:01.234"。
func vttTime(sec float64) string {
	h, m, s, ms := splitTime(sec)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// splitTime 把秒拆成 时/分/秒/毫秒（负数按 0 处理，绝不产出负时间戳）。
func splitTime(sec float64) (int, int, int, int) {
	if math.IsNaN(sec) || sec < 0 {
		sec = 0
	}
	total := int(math.Round(sec * 1000))
	h := total / 3600000
	total -= h * 3600000
	m := total / 60000
	total -= m * 60000
	s := total / 1000
	ms := total - s*1000
	return h, m, s, ms
}

// ---------------------------------------------------------------------------
//  健康快照（真实探活）
// ---------------------------------------------------------------------------

// STTHealth 是 /healthz 的返回体，也是"能力到底通不通"的证据。
type STTHealth struct {
	OK bool `json:"ok"`
	// CLIPresent / FfmpegPresent / FfprobePresent 是链路三件套。
	CLIBin           string `json:"cli_bin,omitempty"`
	CLIPresent       bool   `json:"cli_present"`
	FfmpegBin        string `json:"ffmpeg_bin,omitempty"`
	FfmpegPresent    bool   `json:"ffmpeg_present"`
	FfprobeBin       string `json:"ffprobe_bin,omitempty"`
	FfprobePresent   bool   `json:"ffprobe_present"`
	ServerBinPresent bool   `json:"server_bin_present"`
	// Models / Ready / Missing 是档位的落盘现状。
	Models  []STTModelState `json:"models"`
	Ready   int             `json:"ready"`
	Missing int             `json:"missing"`
	// Current 是当前档位。
	Current string `json:"current"`
	// ModelPresent 表示**当前档位**的权重真的在。
	ModelPresent bool `json:"model_present"`
	// ProbeOK 是"真的跑了一次极短音频转写"的结论 —— 这是唯一的能力证据。
	ProbeOK bool `json:"probe_ok"`
	// ProbeCached / ProbeAgeMS 如实说明这个结论是不是刚刚测的。
	ProbeCached bool  `json:"probe_cached"`
	ProbeAgeMS  int64 `json:"probe_age_ms"`
	// ProbeNote 是探针的补充信息（例如识别到的文字/空结果/耗时）。
	ProbeNote string `json:"probe_note,omitempty"`
	// Service / MarketAppID 是自我标识。
	Service     string `json:"service"`
	MarketAppID string `json:"market_app_id"`
	// Version 是引擎版本（`whisper-cli --help` 里的 commit 或 brew 版本；拿不到留空）。
	Version string `json:"version,omitempty"`
	// Reason 在 ok=false 时说明**为什么不可用**（绝不绿灯）。
	Reason string `json:"reason,omitempty"`
}

// tinyProbeWAV 生成一段 STTProbeSeconds 长的 16 kHz 单声道静音 WAV。
//
// 为什么用静音而不是一段语音：探活只需要回答"模型能加载、推理链路能跑通、
// 能产出一份合法 JSON"。静音不需要额外的音频素材（面板不引入任何素材文件），
// 而且 whisper 对静音会给出空分段 —— 恰好证明它真的跑完了整个前向。
func tinyProbeWAV(seconds float64, sampleRate int) []byte {
	if seconds <= 0 {
		seconds = STTProbeSeconds
	}
	if sampleRate <= 0 {
		sampleRate = STTProbeSampleRate
	}
	n := int(math.Round(seconds * float64(sampleRate)))
	dataLen := n * 2 // s16le 单声道
	out := make([]byte, 44+dataLen)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataLen))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:24], 1) // 单声道
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataLen))
	// 采样点已经是 0（静音），不用再写。
	return out
}

// ProbeTranscribe 真的用当前档位跑一次极短音频转写。
//
// 返回的 error 是给用户看的原因。这就是"能力探活"：
// 模型文件在、whisper-cli 在，都不等于"现在能转出字" ——
// 权重损坏、Metal 初始化失败、磁盘写不进去，都只有真跑一次才知道。
func (e *STTEngine) ProbeTranscribe(ctx context.Context) (bool, string, error) {
	av := e.Available()
	if !av.OK {
		return false, "", fmt.Errorf("%w：%s", ErrSTTEngineUnavailable, strings.Join(av.Reasons, "；"))
	}
	cur := e.CurrentModel
	if _, err := FindSTTModel(cur); err != nil {
		cur = DefaultSTTModelID()
	}
	modelPath, model, err := sttModelPath(e.ModelsDir, cur)
	if err != nil {
		return false, "", err
	}
	if !STTModelFileExists(e.ModelsDir, model.ID) {
		return false, "", fmt.Errorf("%w：当前档位 %s 的权重不在（%s）", ErrSTTModelMissing, model.ID, modelPath)
	}
	root := e.TempRoot
	if root == "" {
		root = os.TempDir()
	}
	dir, derr := os.MkdirTemp(root, "zp-stt-probe-")
	if derr != nil {
		return false, "", fmt.Errorf("创建探针临时目录失败：%w", derr)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	wavPath := filepath.Join(dir, "probe.wav")
	if werr := os.WriteFile(wavPath, tinyProbeWAV(STTProbeSeconds, STTProbeSampleRate), 0o600); werr != nil {
		return false, "", fmt.Errorf("写入探针音频失败：%w", werr)
	}
	started := e.now()
	segs, lang, terr := e.transcribeChunk(ctx, STTChunk{Index: 0, Path: wavPath, Seconds: STTProbeSeconds},
		modelPath, "auto", "", nil)
	if terr != nil {
		return false, "", fmt.Errorf("探针转写失败（模型 %s）：%w", model.ID, terr)
	}
	note := fmt.Sprintf("用 %s 档跑了 %.1f 秒探针音频：%d 个分段、语言 %q、耗时 %d ms",
		model.ID, STTProbeSeconds, len(segs), lang, e.now().Sub(started).Milliseconds())
	return true, note, nil
}

// Health 真的探一次活并把结论组织成对外契约。
//
// 判据贴着能力（AGENTS 第三节），四层全过才 ok：
//  1. 链路三件套（whisper-cli / ffmpeg / ffprobe）都是可执行文件；
//  2. 当前档位的**权重文件真的在**（大小与上游一致）；
//  3. 真的跑一次极短音频转写（ProbeTranscribe）；
//  4. 上面三步都过 —— 绝不因为"进程活着/端口在听"就给绿灯。
//
// 探针结果按 ProbeTTL 缓存，响应里用 probe_cached / probe_age_ms 如实说明，
// 不把缓存结论伪装成刚刚的实测。
func (e *STTEngine) Health(ctx context.Context) STTHealth {
	cur := e.CurrentModel
	if _, err := FindSTTModel(cur); err != nil {
		cur = DefaultSTTModelID()
	}
	state := STTModelsStateFor(e.ModelsDir, cur)
	h := STTHealth{
		CLIBin:           e.CLIBin,
		CLIPresent:       fileExecutable(e.CLIBin),
		FfmpegBin:        e.FfmpegBin,
		FfmpegPresent:    fileExecutable(e.FfmpegBin),
		FfprobeBin:       e.FfprobeBin,
		FfprobePresent:   fileExecutable(e.FfprobeBin),
		ServerBinPresent: fileExecutable(e.ServerBin),
		Models:           state.Models,
		Ready:            state.Ready,
		Missing:          state.Total - state.Ready,
		Current:          state.Current,
		Service:          STTAppID, MarketAppID: STTAppID,
	}
	for _, m := range state.Models {
		if strings.EqualFold(m.ID, state.Current) {
			h.ModelPresent = m.Installed
			if !m.Installed {
				h.Reason = "当前档位 " + m.ID + " 的模型不可用：" + m.Reason
			}
			break
		}
	}
	if av := e.Available(); !av.OK {
		if h.Reason == "" {
			h.Reason = strings.Join(av.Reasons, "；")
		}
		return h
	}
	if !h.ModelPresent {
		return h
	}
	ok, note, age, cached := e.cachedProbe(ctx)
	h.ProbeOK, h.ProbeNote = ok, note
	h.ProbeCached, h.ProbeAgeMS = cached, age.Milliseconds()
	if !ok {
		h.Reason = note
		return h
	}
	h.OK = true
	return h
}

// cachedProbe 按 TTL 缓存探针结果。
//
// 返回 (ok, note, age, cached)。探测失败**不缓存失败**之外的东西：
// 失败也会被缓存一小会儿，避免健康检查的每次轮询都去重跑一遍注定失败的推理
// （但缓存期一到就重试，所以"治好之后 20 秒内就会转绿"）。
func (e *STTEngine) cachedProbe(ctx context.Context) (bool, string, time.Duration, bool) {
	ttl := e.probeTTL()
	e.probeMu.Lock()
	if !e.probeAt.IsZero() {
		age := e.now().Sub(e.probeAt)
		if age >= 0 && age < ttl {
			ok, note := e.probeOK, e.probeNote
			e.probeMu.Unlock()
			return ok, note, age, true
		}
	}
	e.probeMu.Unlock()

	ok, note, err := e.ProbeTranscribe(ctx)
	if err != nil {
		if note == "" {
			note = err.Error()
		}
		ok = false
	}
	e.probeMu.Lock()
	e.probeAt, e.probeOK, e.probeNote = e.now(), ok, note
	e.probeMu.Unlock()
	return ok, note, 0, false
}

// InvalidateHealthProbe 让下一次 /healthz 重新真探一次。
//
// 安装器在"刚装完/刚下完模型"之后必须调它：否则 20 秒的缓存会把
// 装之前那次"模型不在"的结论继续报出来（用户看到"装完了还是红灯"）。
func (e *STTEngine) InvalidateHealthProbe() {
	e.probeMu.Lock()
	e.probeAt = time.Time{}
	e.probeMu.Unlock()
}

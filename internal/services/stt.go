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

// 语音转文字（whisper.cpp）引擎：用 brew 包的 whisper-cli 逐片转写，面板先用
// ffmpeg 转 16 kHz 单声道 WAV（实测 whisper-cli 没链 ffmpeg，只认这种 WAV）。
// 选 whisper-cli 而非 whisper-server：实测 `-pp` 把进度打到 stderr，面板逐行
// 解析出真实进度，且模型档位是每次调用的参数；whisper-server 两样都做不到。
// 不引入 Python / Docker / GUI；ffmpeg 缺失时如实报错，绝不静默降级。

const (
	// STTAppID 是应用市场里的条目 ID。
	STTAppID = "stt"
	// STTLabel 是面板托管的「语音转文字」launchd 标签。
	STTLabel = "com.zizdog.stt"
	// STTSlug 是面板别名（目录条目的 UI.Slug）：/<slug>/。
	STTSlug = "stt"
	// STTPort 是网页界面默认监听端口（只绑 127.0.0.1）；与目录里其它端口
	// 不冲突（8880 / 8890 / 8891 / 8899），唯一性由 TestCatalogPortsAreUnique 锁死。
	STTPort = 8892

	// STTBrewFormula 是 Homebrew 包名 —— **必须写正名 `whisper.cpp`**，不能写
	// 别名 `whisper-cpp`：镜像清单 JSON 只有正名（别名 404），写别名会被三家
	// 国内镜像全判为不可用并静默回落 ghcr.io（实测 2.1 秒 vs 8 分 28 秒）。
	STTBrewFormula = "whisper.cpp"
	// STTCLIBinName 是包里的命令行引擎（转写就靠它）。
	STTCLIBinName = "whisper-cli"
	// STTServerBinName 是包里官方的 OpenAI 兼容服务端；本轮不当引擎用，
	// 只用于如实告诉用户"这个包里还有 whisper-server"（/healthz 会报出来）。
	STTServerBinName = "whisper-server"

	// STTChunkSeconds 是长音频切片长度（秒），也是"真实进度"的唯一来源：
	// 10 分钟音频 = 10 片，进度就是真的转完了 3/10 片。60 秒是取舍。
	STTChunkSeconds = 60
	// STTSyncMaxSeconds 是同步返回的时长上限；超过它走任务中心（202 + task_id + SSE）。
	STTSyncMaxSeconds = 60
	// STTMaxAudioSeconds 是单次请求允许的音频时长上限（3 小时）。
	// 超过一律 413 如实拒绝，不做静默截断（截断会让用户以为整段都转完了）。
	STTMaxAudioSeconds = 3 * 60 * 60
	// STTMaxUploadBytes 是上传文件的硬上限（2 GiB，够 3 小时的 m4a）。
	STTMaxUploadBytes = 2 << 30

	// STTModelDirName 是模型目录名（相对 <home>/stt/）。
	STTModelDirName = "models"

	// STTHealthProbeTTL 是健康探活缓存时长：探活是"真的跑一次极短转写"（见
	// STTEngine.Health），响应用 probe_cached / probe_age_ms 如实回报，不伪装成实测。
	STTHealthProbeTTL = 20 * time.Second

	// STTModelDownloadTimeout 是下载一个档位的超时：最大档 1.43 GiB，实测
	// hf-mirror 约 783 KB/s → 约 32 分钟，给足一小时。此值必须与
	// market_downloads.go 的下载点声明一致（两处漂移会让审计说谎）。
	STTModelDownloadTimeout = 60 * time.Minute
	// STTProbeSeconds 是探针音频长度（0.5 秒静音）：跑完并产出合法 JSON 即证明链路通。
	STTProbeSeconds = 0.5
	// STTProbeSampleRate 是探针 WAV 的采样率（whisper 要求 16 kHz）。
	STTProbeSampleRate = 16000
)

// 错误：全部可被 errors.Is 判定，web 层据此如实映射成 4xx / 413 / 503。

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
	// ErrSTTInvalidRequest 请求参数不合法（response_format 拼错等）；必须与"引擎不可用"分开：前者给 4xx。
	ErrSTTInvalidRequest = errors.New("请求参数不合法")
	// ErrSTTExportUnsupported 请求的导出格式不受支持。
	ErrSTTExportUnsupported = errors.New("不支持的导出格式")
)

// --- 模型档位 ---

// STTModel 是一个可选的模型档位；体积是**实测值**（2026-09-25 读 hf-mirror Content-Range），不是抄的/估的。
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

// STTModels 是面板支持的三个档位，**顺序即界面下拉框顺序**；默认档是
// large-v3-turbo（q5_0，547 MiB，比 medium 更小更准）。没装/下到一半时
// /healthz 与 /v1/models 如实报缺、界面照旧给「下载」入口，绝不假装已就绪。
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
// 纯函数，可单测：找不到标记时退回第一个，绝不返回空串（空串会让请求报"未知档位"）。
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
// 这是档位解析的唯一入口，"档位名拼错"只有一处判据。
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

// --- 路径 ---

// STTPaths 是目录约定：面板自己的东西放 <真实用户家目录>/stt/（与 qwentts / iopaint 同规矩）。
type STTPaths struct {
	Home      string
	Root      string
	ModelsDir string
	Plist     string
	OutLog    string
	ErrLog    string
}

// sttPaths 解析路径。**不写死 /opt/homebrew、也不写死用户名**：家目录来自
// Manager 的 UserHome / UserName，brew 前缀一律走 m.brewPrefix()。
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

// sttModelPath 返回档位模型文件绝对路径；modelsDir 是注入参数，单测可喂临时目录。
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

// STTModelFileExists 报告权重是否**真的在磁盘上且不是空壳**：文件在 + 大小与
// 上游一致。只 os.Stat 不看大小不够 —— 0 字节文件会让 /healthz 报绿灯而转写必失败。
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

// sttMinPlausibleModelBytes 是"这个文件可能是个真模型"的绝对下限：最小档也远大于 1 MiB。
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

// STTModelsStateFor 组装档位现状（currentID 空 = 默认档）；纯注入参数，单测可离线断言缺文件时的显示。
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

// --- 下载来源（NAS → HF 镜像 → 官方）---

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

// sttGGMLMagics 是 ggml 权重文件允许的开头 4 字节。必须校验：下载到的很可能
// 是 HTML 错误页/登录页/空文件，"文件存在且大小对"骗得过。实测 ggml-small.bin
// 前 4 字节是 ASCII "lmgg"（GGML_FILE_MAGIC=0x67676d6c 小端落盘），顺序容易写反。
var sttGGMLMagics = [][]byte{[]byte("lmgg"), []byte("ggml"), []byte("fmgg"), []byte("tjgg"), []byte("algm")}

// validateSTTModelFile 检查刚下好的模型文件看起来真的是 ggml 权重；
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

// --- 引擎 ---

// STTCommandRunner 跑一条命令并把 stderr 逐行回调（进度就在这里）。做成注入点是
// 为了单测：转写/探活**不允许**在单测里真的跑 whisper-cli 或读真实家目录的模型。
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

// STTDefaultChunkTimeout 是单片转写超时：一片最长 60 秒音频，30 分钟只针对
// "机器极慢 + 档位极重"的兜底，不是常见的等待时间。
const STTDefaultChunkTimeout = 30 * time.Minute

// NewSTTEngine 造一个用真实系统工具的引擎；brewPrefix 由调用方注入，
// **本文件不写死任何一个前缀**。
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
// 必须逐行读 stderr —— whisper-cli 的进度只有边跑边读才拿得到，等命令结束就没意义了。
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
	// stdout 与 stderr **必须并发收**：只读一路会让管道缓冲区写满，子进程卡在
	// write 上（经典死锁，表现为"命令超时"）。whisper-cli 的文本走 stdout、进度走 stderr。
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

// --- 音频准备（转码 / 切片）---

// STTAudioInfo 是探测到的音频信息。
type STTAudioInfo struct {
	Seconds    float64 `json:"seconds"`
	SampleRate int     `json:"sample_rate,omitempty"`
	Channels   int     `json:"channels,omitempty"`
	Codec      string  `json:"codec,omitempty"`
	Format     string  `json:"format,omitempty"`
}

// ProbeAudio 用 ffprobe 读音频时长与基本参数。时长决定同步还是任务、
// 也决定切几片（真实进度的分母）—— 猜时长就会给出假进度。
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

// ConvertToWAV 把任意音频转成 whisper 要的 16 kHz 单声道 s16le WAV；
// 实测 brew 的 whisper-cli 没链 ffmpeg，只认这种 WAV。
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

// SplitWAV 用 ffmpeg 的 segment muxer 把一段 16 kHz 单声道 WAV 切成 numChunks 片；
// 不自己切字节 —— WAV 头必须每片都正确重写，切坏了就是坏头。
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

// --- 转写 ---

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
	// Progress 在每片转写完成后被调用（done=已完成片数，total=总片数）；有它才有真实进度。
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
	// ModelID / ModelFile 是**实际使用**的档位与权重文件（档位被纠正/回落时就是证据）。
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

// parseWhisperCLIJSON 把 whisper-cli 的 `-oj` 输出解析成"语言 + 分段"。纯函数，
// 单测锁住解析 —— 这条错了"文本是空的/时间戳全 0"就是必然。时间戳优先用
// offsets.{from,to}（毫秒整数最稳），拿不到才解析 timestamps.from 字符串。
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

// whisperProgressRe 匹配 whisper-cli 的进度行，实测形状
// `whisper_print_progress_callback: progress =  35%`；解析不出来就不报进度，绝不编假百分比。
var whisperProgressRe = regexp.MustCompile(`progress\s*=\s*([0-9]{1,3})\s*%`)

// parseWhisperProgressLine 从一行 stderr 里抠出进度百分比；第二个返回值表示这行是进度行。
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

// sttLanguageAliases 把常见语言写法归一到 whisper 语言码（en / zh / ja / auto…）；
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

// NormalizeSTTLanguage 把请求里的语言写法归一成 whisper 语言码。认不出来的
// **原样小写返回**（whisper 自己会拒绝），绝不猜 —— 猜错会让整段变成另一种文字。
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

// Transcribe 把音频转成文字：探时长 → 转码 16 kHz WAV → 切片 → 逐片跑
// whisper-cli（-oj）→ 合并分段并把每片时间戳加上偏移。任何一步失败都如实报错；
// 进度是**真的**（分母＝片数），片内百分比只进日志，不冒充总体进度。
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
		// 这一片结尾的文字当作下一片的 prompt（实测不加它时切点上的词会被截成两半）。
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
		// 引擎跑通但没有任何文字：如实说明"静音/无可识别语音"，不让调用方猜成功还是失败。
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

// --- 输出格式（纯函数，可单测）---

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

// ParseSTTFormat 解析 response_format（含常见别名）；空串 = 默认 json。
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

// ContentType 返回该格式的 MIME。必须逐格式给对：`response_format=text`
// 返回 JSON 的 Content-Type 会让按 Content-Type 分派的客户端解析失败
// （真机契约测试抓出来的一条；OpenAI 的 text 就是 text/plain）。
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

// FormatTranscript 把结果渲染成请求要的格式。json / text / srt / vtt 都由
// **同一份分段数据**生成，"文本里有、字幕里没有"这类不一致不可能发生；纯函数。
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

// --- 健康快照（真实探活）---

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

// tinyProbeWAV 生成一段 STTProbeSeconds 长的 16 kHz 单声道静音 WAV：
// 静音不需要任何素材文件，whisper 对静音给空分段 —— 恰好证明它跑完了整个前向。
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

// ProbeTranscribe 真的用当前档位跑一次极短音频转写，error 是给用户看的原因。
// 文件在、whisper-cli 在都不等于"现在能转出字"—— 权重损坏、Metal 初始化失败、
// 磁盘写不进去，只有真跑一次才知道。
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

// Health 真的探一次活并把结论组织成对外契约。四层全过才 ok：链路三件套可执行、
// 当前档位权重真的在、真的跑一次极短转写，**绝不因为"进程活着/端口在听"就给绿灯**。
// 探针结果按 ProbeTTL 缓存，响应里用 probe_cached / probe_age_ms 如实说明。
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

// cachedProbe 按 TTL 缓存探针结果，返回 (ok, note, age, cached)。失败也缓存
// 一小会儿，避免每次轮询都重跑注定失败的推理；缓存期一到就重试（20 秒内转绿）。
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

// InvalidateHealthProbe 让下一次 /healthz 重新真探。安装器在"刚装完/刚下完模型"
// 之后必须调它，否则缓存会把装之前"模型不在"的结论继续报出来（用户看到装完还红灯）。
func (e *STTEngine) InvalidateHealthProbe() {
	e.probeMu.Lock()
	e.probeAt = time.Time{}
	e.probeMu.Unlock()
}

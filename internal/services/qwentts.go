package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/logx"
)

// qwenLog 是日志出口：守温循环的失败必须能在面板日志里看到，
// 否则"网站第一次请求为什么慢"会变成一个查不出来的问题。
var qwenLog = logx.New("services")

// ==== Qwen3 TTS（mlx-audio）部署：严格照 TtsVoice 手册的部署契约（端口/路径/模型名/启动方式都不能自由发挥）====
// 与手册唯一的刻意不同：面板写 /Library/LaunchDaemons + UserName=<真实用户>；用户级 agent 要登录图形界面才跑，等于开机不自启。
// 契约要点（照抄手册，不要"优化"）：端口 8880，绑 127.0.0.1 还是 0.0.0.0 取决于鉴权（见 qwenBindHost）。
// 模型只有 Base（克隆）一个（2026-09-14 起 CustomVoice 下线）；Python 3.11 + pip 清华源 + hf-mirror。
// **HF_HUB_DISABLE_XET=1 必须设**：hf-mirror 不代理 Xet 后端，不设会下载到一半报 CAS Client Error。

// QwenModel 描述一个可用的 TTS 模型。2026-09-14 起只有一个（Base/克隆），
// 预置音色（CustomVoice）整体下线。保留结构体与列表是因为它还承担界面状态展示
// 与 QwenUnloadStale 的差集计算。
type QwenModel struct {
	// Name 是 HuggingFace 仓库名，同时也是 API 请求里 model 字段的取值
	Name string `json:"name"`
	// Role 是能力标签：clone（克隆）/ preset（预置音色）
	Role  string `json:"role"`
	Label string `json:"label"`
	Note  string `json:"note"`
}

// QwenModels 是可用模型清单。顺序即界面顺序，第一个是默认；只有一个 Base（克隆）。
// CustomVoice 条目已移除：留着只会让界面显示出"可以切过去"的假选项（权重已删，点它就是失败）。
var QwenModels = []QwenModel{
	{
		Name:  "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
		Role:  "clone",
		Label: "Base（音色克隆）",
		Note:  "支持参考音频克隆音色；网站插件现在只用这一个模型（预置音色已下线）",
	},
}

// qwenDefaultModel 是插件默认该填的那个，也是唯一的那个。
// 2026-09-14 起网站侧全面切到 1.7B 单模型，0.6B 与 CustomVoice 都已停用。
// 面板必须跟着改，否则界面会把已经不用的模型当成可选项。
const qwenDefaultModel = "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit"

const (
	qwenPort  = 8880
	qwenLabel = "com.zizdog.qwen3tts"
	// 预置的 Python 版本只有一处定义（python_runtime.go 的 panelPythonFormula），
	// 换版本不会漏改解释器路径 / site-packages 路径。
	qwenPythonVer = panelPythonFormula
	qwenPipMirror = "https://pypi.tuna.tsinghua.edu.cn/simple"
	qwenHFMirror  = "https://hf-mirror.com"
	qwenMinDiskGB = 10
	qwenMinMemGB  = 15
	// qwenReadyTimeout 是「等 Qwen 端口监听」的上限：首次加载模型要十几秒，取 90 秒给慢机器留余量；
	// 超时即**如实失败**（见 waitQwenReady）。
	qwenReadyTimeout = 90 * time.Second
)

// QwenLabel 是 Qwen 服务的 launchd 标签：别的包判断"刚动过的是不是 Qwen 服务"时用它，
// 硬抄字符串迟早会抄错。
const QwenLabel = qwenLabel

// qwenPaths 是这套服务的目录约定（与手册一致）。
type qwenPaths struct {
	Home   string // ~/tts
	Root   string // ~/tts/qwen3
	Venv   string // ~/tts/qwen3/.venv
	Python string // venv python
	OutLog string
	ErrLog string
	Plist  string // /Library/LaunchDaemons/com.zizdog.qwen3tts.plist
}

func (m *Manager) qwenPaths() qwenPaths {
	home := m.opt.UserHome
	if home == "" {
		home = "/Users/" + m.opt.UserName
	}
	root := filepath.Join(home, "tts", "qwen3")
	return qwenPaths{
		Home:   filepath.Join(home, "tts"),
		Root:   root,
		Venv:   filepath.Join(root, ".venv"),
		Python: filepath.Join(root, ".venv", "bin", "python"),
		OutLog: filepath.Join(root, "launchd.out.log"),
		ErrLog: filepath.Join(root, "launchd.err.log"),
		Plist:  "/Library/LaunchDaemons/" + qwenLabel + ".plist",
	}
}

// QwenOptions 是部署 Qwen3 TTS 时用户可做的选择。mlx-audio 上游完全没有鉴权，所以对外暴露与否是**真实的取舍**：
// 加鉴权（默认）只监听 127.0.0.1，由 8899 的接收端做带密钥的反代；不加鉴权则绑 0.0.0.0（同内网谁能连上谁就白用这块 GPU）。
// 网站插件两种都支持（`openaiKey` 允许为空）。
type QwenOptions struct {
	// Auth 为 false 时 Qwen 绑 0.0.0.0 且不部署反代
	Auth bool
	// Token 是共享密钥。Auth=true 且为空时自动生成
	Token string
}

// InstallQwenTTS 部署 Qwen3 TTS 服务并把插件要用到的信息一并返回。
func (m *Manager) InstallQwenTTS(ctx context.Context, result *InstallResult, opt QwenOptions) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("部署 Qwen3 TTS 需要以 root 运行")
	}
	if m.opt.UserName == "" {
		return fmt.Errorf("无法确定运行该服务的真实用户")
	}

	// ---- 0a. 依赖链：命令行开发者工具 → Homebrew → python@3.11 ----
	// 全新 macOS 上三样都没有（/usr/bin/python3 只是会弹图形对话框的占位程序）；EnsureHomebrew 缺什么装什么。
	if err := m.EnsureHomebrew(ctx, result); err != nil {
		return err
	}

	// ---- 0b. 基础依赖：ffmpeg（放最前面，装不上就致命中止）----
	// 2026-09-16 事故：缺 ffmpeg 时服务"能启动、能回 wav、合成 mp3 返回 HTTP 200 + 0 字节 body"，
	// 而安装任务**显示成功**、健康检查全绿 —— 绝不再交付这种"看起来装好了"的半残服务。
	m.AnnounceAppDependencies(ctx, "qwen3tts", result)
	if err := m.EnsureBaseDependencies(ctx, result); err != nil {
		return fmt.Errorf("缺少基础依赖（TTS 编码 mp3 必需），已中止部署：%w", err)
	}

	// ---- 0. 前置检查：这两个不够会在装到一半时失败，且失败原因很难懂 ----
	if err := m.checkQwenPreconditions(ctx, result); err != nil {
		return err
	}

	p := m.qwenPaths()

	// ---- 1. Python 3.11 ----
	// 指定 3.11 不是偏好：mlx-audio 在该版本有预编译 wheel（cp311），更高版本可能退化成源码编译甚至装不上。
	if !m.brewHas(ctx, qwenPythonVer) {
		result.step(ctx, "正在安装 "+qwenPythonVer)
		// 2026-09-19 事故就是这一步：镜像是坏的（0 字节瓶）→ 必须换源重试。
		// python@3.11 的 arm64 瓶国内镜像常常没有，只能靠官方源兜底。
		if _, err := m.brewInstall(ctx, result, 20*time.Minute, qwenPythonVer); err != nil {
			return fmt.Errorf("安装 %s 失败: %w", qwenPythonVer, err)
		}
	} else {
		result.step(ctx, qwenPythonVer+" 已安装，跳过")
	}
	pyBin := panelPythonInterpreter(m.brewPrefix(), qwenPythonVer)
	if _, err := os.Stat(pyBin); err != nil {
		return fmt.Errorf("找不到 %s", pyBin)
	}

	// ---- 2. 虚拟环境 ----
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", p.Root, err)
	}
	// 必须**递归**改归属：上面是以 root 身份 mkdir 的，只 chown 父目录的话子目录仍是 root 所有 ——
	// 接着以用户身份建 .venv 就 Permission denied（真机就是这么失败的）。
	if m.opt.UserName != "" {
		_ = chownTree(m.opt.UserName, p.Home)
	}
	if _, err := os.Stat(p.Python); err != nil {
		result.step(ctx, "正在创建 Python 虚拟环境")
		if out, err := m.runAsUser(ctx, 5*time.Minute, pyBin, "-m", "venv", p.Venv); err != nil {
			return fmt.Errorf("创建虚拟环境失败: %v（%s）", err, tailText(out, 300))
		}
	} else {
		result.step(ctx, "虚拟环境已存在，跳过创建")
	}
	// 2b) 强制 IPv4：这个网络上 IPv6 地址**不可达但会挂住**（不是立刻失败）。
	// 写不进去只警告，后面下载失败会如实报错。
	if err := m.installIPv4Sitecustomize(ctx, p, result); err != nil {
		// 不致命：下不动模型时下载步骤会如实报错，而不是在这里假装成功
		result.step(ctx, "警告：未能写入 IPv4 优先补丁（"+err.Error()+"），下载可能变慢")
	}

	// ---- 3. 安装 mlx-audio[server] ----
	// [server] extra 最容易漏：不带它只有库、没有 HTTP 服务。
	if err := m.pipInstall(ctx, p, result); err != nil {
		return err
	}

	// ---- 4. 下载模型 ----
	if err := m.downloadQwenModel(ctx, p, result); err != nil {
		return err
	}

	// ---- 5. 注册为系统级守护进程 ----
	// 绑定地址由"要不要鉴权"决定：不加鉴权就必须对外暴露（否则网站连不上）。
	// 见 qwenBindHost。
	if err := m.installQwenService(ctx, p, result, opt.Auth); err != nil {
		return err
	}

	// ---- 6. 先登记进服务管理，再验收 ----
	// 顺序刻意（2026-09-17 审计）：验收失败会如实返回错误，登记留在后面会留下「任务失败、服务管理里又找不到它」的半成品。
	// 登记失败**不**让整个部署失败：launchd 服务本身是好的，只是面板列表暂时没有它（刻意接受的降级）。
	registered := true
	if err := m.RegisterInstalledService(ctx, qwenLabel, "Qwen3 TTS", "🗣️", "ai", qwenPort); err != nil {
		registered = false
		result.step(ctx, "（自动登记到面板失败："+err.Error()+"，可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
	}

	// ---- 6b. 验证：端口真的在监听才算成功（首次加载模型要十几秒）----
	result.step(ctx, "正在等待服务加载模型（首次约十几秒）")
	if err := m.waitQwenReady(ctx, p, registered, result); err != nil {
		return err
	}

	// 预热模型：首次加载需 20-30 秒，放在安装时做掉，
	// 网站第一次请求就能直接出声，而不是让用户等半分钟以为坏了。
	result.step(ctx, "正在预热模型（首次约 20-30 秒）…")
	if n := m.EnsureQwenModelsLoaded(ctx); n > 0 {
		result.step(ctx, fmt.Sprintf("已加载 %d 个模型，音色克隆可立即使用", n))
	}

	// ---- 6c. 收尾验收：用**服务进程的 PATH** 真的跑一次 ffmpeg ----
	// "brew 说装好了"不等于"服务进程能用"（动态库坏 / PATH 里没 Homebrew）：真机事故里最难查的正是这种"文件在、跑不通"。
	// 失败就明确报错 —— 宁可任务显示失败，也不要交付一个一合成 mp3 就返回空 body 的服务。
	if problems := m.VerifyBaseDependencies(ctx); len(problems) > 0 {
		msg := "基础依赖验收未通过：" + describeDependencyProblems(problems) +
			"。服务已注册，但合成 mp3 会失败（HTTP 200 + 空 body）；" +
			"修好后请重新部署，或手工执行 `brew install ffmpeg`"
		result.Warning = msg
		result.step(ctx, "错误："+msg)
		return fmt.Errorf("%s", msg)
	}
	result.step(ctx, "已确认 ffmpeg / ffprobe 可执行（mlx_audio 编码 mp3 依赖它）")

	// ---- 7. 把插件要填的东西直接列出来（照抄即可，不用回去翻手册）----
	host := m.primaryIP()
	result.Address = host

	if opt.Auth {
		// 加鉴权时 Qwen 只监听 127.0.0.1，网站**只能**通过 8899 访问，所以顺手把反代装好。
		// 反代失败必须让**整个部署失败**（2026-09-17 审计，第 4 处同族缺陷）：任务显示「✅ 完成」而网站根本连不上，
		// 正是我们在清的那一族「能谎报成功」。详见 finishQwenAuthEntry。
		return m.finishQwenAuthEntry(ctx, result, opt.Token, m.InstallVoiceReceiver)
	}

	result.Steps = append(result.Steps,
		"",
		"┌─────────────────────────────────────────────┐",
		"│  未启用鉴权：请把下面这些填进网站插件       │",
		"└─────────────────────────────────────────────┘",
		"  服务商        = OpenAI 兼容",
		fmt.Sprintf("  openaiBaseUrl = http://%s:%d/v1", host, qwenPort),
		"  openaiKey     = （留空）",
		"  openaiModel   = "+qwenDefaultModel,
		"",
		"  ↑ 只有一个模型（网站侧已只支持自定义音色/克隆），插件里就填它：",
		"     "+QwenModels[0].Name,
		"         → 音色克隆（必须先上传音色样本；没有样本网站会直接拒绝生成）",
		"  openaiFlavor  = qwen3tts    ← 选它才走异步模式",
		"  openaiFormat  = wav         ← 网站侧现在用 wav（24kHz 单声道）",
		"",
		"⚠️ 当前 8880 对全网开放且无鉴权：同内网任何人都能白用这块 GPU。",
		"   如需音色克隆，再点「部署音色接收端」并把密钥留空即可。",
	)
	return nil
}

// waitQwenReady 等 Qwen 服务端口就绪。**超时返回错误，不是警告。**
// 2026-09-17 审计：原来 90 秒没起来只写一条 Warning 就 return nil，任务显示「任务完成 ✅」而 8880 根本没监听，
// 连带 TtsVoice 全站失联；现在改成如实失败（写法照抄 iopaint.go 的 waitIOPaintReady）。
func (m *Manager) waitQwenReady(ctx context.Context, p qwenPaths, registered bool, result *InstallResult) error {
	wait := readyWaitPort
	state := "Python 环境、模型权重与 launchd 服务都已就位，服务也已登记进服务管理"
	if !registered {
		state = "Python 环境、模型权重与 launchd 服务都已就位" +
			"（但登记进面板失败，可在「应用 → 已安装」里点「+ 注册服务」手动加入）"
	}
	return assertReady(ctx, readySpec{
		What: "Qwen3 TTS 服务",
		Expect: fmt.Sprintf("TCP 端口 %d 开始监听（网站与 TtsVoice 都只连这个端口）",
			qwenPort),
		Timeout: qwenReadyTimeout,
		Probe: func(ctx context.Context) readyVerdict {
			if wait(ctx, qwenPort, qwenReadyTimeout) {
				return readyVerdict{OK: true,
					Actual: fmt.Sprintf("已就绪，监听 %d 端口", qwenPort)}
			}
			return readyVerdict{Actual: fmt.Sprintf(
				"连 127.0.0.1:%d 一直失败，端口始终没有监听", qwenPort)}
		},
		LogPath: p.ErrLog,
		State:   state,
		Missing: "但服务实际上不可用，网站与 TtsVoice 现在都连不上它。" +
			"注意：模型权重已下载完成，所以这**不是**「还在下载模型」",
		Remedy: "按下面的日志尾部里的报错修好后重新部署；" +
			"也可以先在「服务管理」里点「重启服务」或执行 " +
			"`sudo launchctl kickstart -k system/" + qwenLabel + "` 再重试",
		Result: result,
	})
}

// finishQwenAuthEntry 在「加鉴权」时部署对外入口（音色接收端反代），失败即**整个部署失败**。
// 加鉴权时 Qwen 只绑 127.0.0.1，反代没起来 = 网站连不上，服务等于还没装成（2026-09-17 审计第 4 处同族缺陷）。
// deploy 作为参数传入：单测要在不触发真实安装（要 root、要真 launchd）的前提下，锁死「失败必须升级」这条约束。
func (m *Manager) finishQwenAuthEntry(ctx context.Context, result *InstallResult, token string,
	deploy func(context.Context, *InstallResult, ReceiverOptions) error) error {

	result.step(ctx, "",
		"已选择加鉴权 → 继续部署对外入口（带共享密钥的反向代理）")
	if err := deploy(ctx, result, ReceiverOptions{Token: token}); err != nil {
		rp := m.receiverPaths()
		msg := "反向代理（音色接收端）部署失败，已中止 Qwen3 TTS 部署：" + err.Error() +
			"。当前状态：Qwen3 TTS 本身已就绪，并已登记为系统级 launchd 服务；" +
			"但它只监听 127.0.0.1，对外入口没有起来，所以网站（TtsVoice）现在" +
			"连不上 8880，服务等于还不能用。" +
			"可以：① 按上面的报错处理后直接重试部署（每一步都是幂等的，已就绪的部分会跳过）；" +
			"② 查看接收端日志 " + rp.ErrLog + " 里的报错；" +
			"③ 如果你确实不需要鉴权，可以在部署时改选「不启用鉴权」——" +
			"此时 Qwen 会绑定 0.0.0.0 让网站直连，但同内网任何人都能白用这块 GPU"
		result.Warning = msg
		result.step(ctx, "错误："+msg)
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// checkQwenPreconditions 检查内存与磁盘：内存不足只警告（低于 16GB 仍能跑），磁盘不足直接失败。
// 1.7B 单模型常驻约 3GB；磁盘按实测口径：模型约 2.9GB + 环境约 0.5GB。
// 要在用户动手前就提示，别让他等 20 分钟下载完才失败。
func (m *Manager) checkQwenPreconditions(ctx context.Context, result *InstallResult) error {
	memGB := memoryGB()
	diskGB := freeDiskGB(m.opt.UserHome)
	result.Steps = append(result.Steps,
		fmt.Sprintf("环境检查：内存 %dGB，可用磁盘 %dGB", memGB, diskGB))

	if memGB > 0 && memGB < qwenMinMemGB {
		result.Warning = fmt.Sprintf(
			"内存只有 %dGB（建议 16GB 起）：1.7B 模型常驻约 3GB，跑起来会偏紧", memGB)
		result.step(ctx, "警告："+result.Warning)
	}
	if diskGB > 0 && diskGB < qwenMinDiskGB {
		return fmt.Errorf("可用磁盘只有 %dGB，建议至少 %dGB（环境约 0.5GB + 模型约 2.9GB）",
			diskGB, qwenMinDiskGB)
	}
	return nil
}

// pipInstall 装 pip 与 mlx-audio[server]。
func (m *Manager) pipInstall(ctx context.Context, p qwenPaths, result *InstallResult) error {
	pip := filepath.Join(p.Venv, "bin", "pip")
	// pip 索引按"镜像站优先、探不通回落清华"现算（见 pypi_mirror.go），
	// 选择与原因由它写进任务日志，与 brew/HF 是同一套语义。
	pipIdx, err := m.pipMirrorArgs(ctx, result)
	if err != nil {
		return err
	}
	// 先升级 pip：旧 pip 解析 mlx-audio 的依赖树可能失败
	if out, err := m.runAsUser(ctx, 5*time.Minute, pip,
		append([]string{"install", "-U", "pip"}, pipIdx...)...); err != nil {
		return fmt.Errorf("升级 pip 失败: %v（%s）", err, tailText(out, 300))
	}
	result.step(ctx, "pip 已升级")

	// 判断是否已装：重复执行时不该再花十几分钟重装
	out, _ := m.runAsUser(ctx, time.Minute, pip, "list")
	if strings.Contains(out, "mlx-audio") {
		result.step(ctx, "mlx-audio 已安装，跳过")
		return nil
	}
	result.step(ctx, "正在安装 mlx-audio[server]（依赖较多，请耐心等待）")
	if out, err := m.runAsUser(ctx, 40*time.Minute, pip,
		append([]string{"install", "mlx-audio[server]"}, pipIdx...)...); err != nil {
		return fmt.Errorf("安装 mlx-audio[server] 失败: %v（%s）", err, tailText(out, 500))
	}
	result.step(ctx, "mlx-audio[server] 安装完成")
	return nil
}

// downloadQwenModel 下载模型。两个环境变量缺一不可：
// HF_ENDPOINT=https://hf-mirror.com 国内直连 huggingface.co 基本不可用；
// HF_HUB_DISABLE_XET=1 hf-mirror 不代理 Xet 后端，不关会在下载中途报 CAS Client Error。
func (m *Manager) downloadQwenModel(ctx context.Context, p qwenPaths, result *InstallResult) error {
	// hf 是新版 CLI 名；旧版叫 huggingface-cli。
	// 两个都试一下，避免版本差异卡住。
	hf := filepath.Join(p.Venv, "bin", "hf")
	if _, err := os.Stat(hf); err != nil {
		hf = filepath.Join(p.Venv, "bin", "huggingface-cli")
	}
	if _, err := os.Stat(hf); err != nil {
		return fmt.Errorf("找不到 hf / huggingface-cli（mlx-audio 可能没装全）")
	}

	// 清单里有什么就下什么（现在只有一个 Base）。
	// 保留这个循环是为了"以后清单再变"时不用改这里。
	for _, mdl := range QwenModels {
		if err := m.downloadOneQwenModel(ctx, hf, mdl, result); err != nil {
			return err
		}
	}
	return nil
}

// qwenHFEndpoint 按"优先级 + 可用性"挑 HuggingFace 端点（用户要求：能用镜像的尽量用，模型文件也一样）：
//  1. 面板设置里的镜像基址 + /hf，但**必须探通**（HEAD <base>/hf/），不通就跳过；
//  2. 内置 hf-mirror.com（国内公共镜像，实测可用）。探测失败不报错，只决定"用哪个端点"。
func (m *Manager) qwenHFEndpoint(ctx context.Context) string {
	if m.MirrorEnabled() {
		base := m.mirrorSubPath("hf")
		if m.hfEndpointUsable(ctx, base) {
			return base
		}
	}
	return qwenHFMirror
}

// hfEndpointUsable 判断一个 HuggingFace 端点**真的能跑 hf download**。
// 2026-09-18 真机事故：镜像站的 /hf/ 首页是 200，但 /hf/api/models/<repo> 是 **502** —— 浅探测会把"下载失败"伪装成"网络问题"。
// 判据贴 CLI 真实需求：① API 能列出模型且是 JSON ② <repo>/resolve/main/config.json 取得到内容；探针用清单第一个模型。
func (m *Manager) hfEndpointUsable(ctx context.Context, base string) bool {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || len(QwenModels) == 0 {
		return false
	}
	repo := QwenModels[0].Name
	cctx, cancel := context.WithTimeout(ctx, m.mirrorProbeTimeout())
	defer cancel()
	get := func(url string) (int, int64, string) {
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, 0, ""
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, 0, ""
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return res.StatusCode, res.ContentLength, string(b)
	}
	// ① API：必须 200 且像 JSON（错误页/302 登录页都不算）。
	code, _, body := get(base + "/api/models/" + repo)
	if code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(body), "{") {
		return false
	}
	code, size, body := get(base + "/" + repo + "/resolve/main/config.json")
	if code != http.StatusOK || (size == 0 && len(body) == 0) {
		return false
	}
	return true
}

// downloadOneQwenModel 下载单个模型并处理续传重试。
func (m *Manager) downloadOneQwenModel(ctx context.Context, hf string, mdl QwenModel, result *InstallResult) error {
	if modelDownloaded(m.opt.UserHome, mdl.Name) {
		result.step(ctx, mdl.Label+" 已下载，跳过")
		return nil
	}

	// 端点每次下载前重新判断：镜像站可能中途挂掉，也可能刚配好。
	hfEndpoint := m.qwenHFEndpoint(ctx)
	result.Steps = append(result.Steps,
		"正在下载 "+mdl.Label+"（约 2GB / 14 个文件，视网络 5~15 分钟）")
	result.Steps = append(result.Steps, "模型来源："+hfEndpoint)
	env := []string{"HF_ENDPOINT=" + hfEndpoint, "HF_HUB_DISABLE_XET=1"}

	// 重试：2GB 下载中途遇到 "Connection reset by peer" 很常见（实测就遇到一次），
	// 而 huggingface 的下载是**可续传**的 —— 已经下好的分片不会重下。
	// 不重试的话，用户会看到一次网络抖动就让整个部署白跑二十分钟。
	var lastOut string
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		out, err := m.runAsUserEnv(ctx, 40*time.Minute, env, hf, "download", mdl.Name)
		if err == nil {
			result.step(ctx, mdl.Label+" 下载完成")
			return nil
		}
		lastOut, lastErr = out, err
		if modelDownloaded(m.opt.UserHome, mdl.Name) {
			// 命令报错但文件其实齐了（hf 有时在收尾阶段断连）
			result.step(ctx, mdl.Label+" 下载完成（末次连接中断，但文件已齐）")
			return nil
		}
		if attempt < 3 {
			result.Steps = append(result.Steps,
				fmt.Sprintf("%s 第 %d 次下载中断，正在续传重试…", mdl.Label, attempt))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	return fmt.Errorf("下载 %s 失败（已重试 3 次）: %v（%s）",
		mdl.Label, lastErr, tailText(lastOut, 500))
}

// modelDownloaded 粗判模型是否已下载完整。
// HuggingFace 缓存的 snapshots/ 下是**指向 blobs/ 的符号链接**，filepath.Walk 给的是 Lstat 信息 ——
// 直接用 info.Size() 会拿到链接自身的长度（实测 76 字节），"权重明明下好了"却被判成没下好；所以必须 os.Stat 跟随链接。
func modelDownloaded(userHome, model string) bool {
	base := filepath.Join(userHome, ".cache", "huggingface", "hub",
		"models--"+strings.ReplaceAll(model, "/", "--"), "snapshots")
	found := false
	_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "model.safetensors" {
			return nil
		}
		real, serr := os.Stat(p) // 跟随符号链接拿真实大小
		if serr == nil && real.Size() > 100<<20 {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// installQwenService 写系统级 plist 并加载（LaunchDaemon + UserName，与手册的差异见文件头）。
// 先写 .tmp 再 rename，保证 launchd 不会读到半截 plist。
func (m *Manager) installQwenService(ctx context.Context, p qwenPaths, result *InstallResult, auth bool) error {
	// 端点按"镜像站优先"现算后写进 plist：**服务进程**日后自己补拉模型时用的是 plist 里的值，
	// 写死 hf-mirror.com 会让"面板配了镜像站、服务却永远走公网"。
	hfEndpoint := m.qwenHFEndpoint(ctx)
	plist := qwenPlist(p, m.opt.UserName, auth, hfEndpoint)
	if err := os.WriteFile(p.Plist+".tmp", []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return fmt.Errorf("安装 plist 失败: %w", err)
	}
	// 等旧实例真正卸载完再装载（bootout 是异步的，见 bootstrapService 说明）
	if err := m.bootstrapService(ctx, qwenLabel, p.Plist); err != nil {
		return err
	}
	result.step(ctx, "服务进程的 HuggingFace 端点："+hfEndpoint)
	result.Steps = append(result.Steps,
		"已注册为系统级后台服务（开机自启、不依赖用户登录）")
	return nil
}

// qwenPlist 生成 LaunchDaemon 定义；绑定地址由 qwenBindHost(auth) 决定。
// hfEndpoint 由调用方按"镜像站优先"现算（见 installQwenService）。
func qwenPlist(p qwenPaths, user string, auth bool, hfEndpoint string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <!-- 以真实用户运行：mlx 与用户目录下的模型缓存都按该身份预期 -->
    <key>UserName</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>-m</string>
        <string>uvicorn</string>
        <string>mlx_audio.server:app</string>
        <!--
            绑定地址取决于"要不要鉴权"（用户在部署时选）：
              · 加鉴权  → 127.0.0.1，对外由 8899 的接收端做带密钥的反代
              · 不加鉴权 → 0.0.0.0，网站直连 8880
            mlx-audio 上游没有任何鉴权（带任何 Authorization 头都被忽略），
            所以绑 0.0.0.0 就意味着同内网谁能连上谁就白用这块 GPU。
        -->
        <string>--host</string>
        <string>%s</string>
        <string>--port</string>
        <string>%d</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HF_ENDPOINT</key>
        <string>%s</string>
        <key>HF_HUB_DISABLE_XET</key>
        <string>1</string>
        <!-- launchd 拿不到 shell 的 PATH，必须显式给 -->
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, qwenLabel, user, p.Python, qwenBindHost(auth), qwenPort, p.Root, hfEndpoint, p.OutLog, p.ErrLog)
}

// execCommandCtx 是 os/exec 的薄封装，便于本文件单独测试。
func execCommandCtx(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// runAsUserEnv 以真实用户身份执行命令，并附加环境变量。
func (m *Manager) runAsUserEnv(ctx context.Context, timeout time.Duration, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// **必须用 /usr/bin/env 显式注入环境变量**（2026-09-18 用户真机事故）：`sudo` 默认 env_reset，
	// 把 env 塞进 cmd.Env 子进程收不到 → hf download 去连 huggingface.co 超时，而面板日志看起来一切正常。
	// brew 那条链路早就踩过同一个坑（见 install.go 的 brewEnvArgs），这里跟进。
	full := asUserEnvArgs(m.opt.UserName, m.opt.UserHome, env, name, args...)
	cmd := execCommandCtx(ctx, "/usr/bin/sudo", full...)
	// 模型下载动辄几百 MB，必须逐行流式，否则用户整个下载期间只能看到一句"正在下载模型"。
	return streamCmd(ctx, cmd)
}

// asUserEnvArgs 构造 `sudo -n -u <user> /usr/bin/env HOME=<home> KEY=VAL… <cmd> args…`。
// 抽出来是为了**能被单测锁住**：形状错了 hf download 就会去连 huggingface.co，
// 表现是"配了镜像却零速度"（2026-09-18 真机事故）。
func asUserEnvArgs(user, home string, env []string, name string, args ...string) []string {
	full := []string{"-n", "-u", user, "/usr/bin/env"}
	if home != "" {
		full = append(full, "HOME="+home)
	}
	full = append(full, env...)
	full = append(full, name)
	return append(full, args...)
}

// memoryGB 返回物理内存 GB，取不到返回 0。
func memoryGB() int {
	return atoiSafe(runOutput("/usr/sbin/sysctl", "-n", "hw.memsize")) / (1 << 30)
}

// freeDiskGB 返回给用户家目录所在卷的可用空间 GB。
func freeDiskGB(path string) int {
	if path == "" {
		path = "/"
	}
	out := runOutput("/bin/df", "-g", path)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0
	}
	return atoiSafe(f[3])
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// primaryIP 返回本机局域网地址，用于拼给插件填的 URL；PrimaryIP 导出给 web 层用。
// 不能让 web 层用请求里的客户端 IP —— 那是**浏览器**的地址，不是服务所在机器的地址
// （2026-09-16 真机踩到，凭据弹窗显示成了用户自己电脑的 IP）。
func (m *Manager) PrimaryIP() string { return m.primaryIP() }

func (m *Manager) primaryIP() string {
	out := runOutput("/usr/sbin/ipconfig", "getifaddr", "en0")
	if ip := strings.TrimSpace(out); ip != "" {
		return ip
	}
	return "<本机地址>"
}

// chownTree 递归把目录树归属改为指定用户。
// **只对"我们自己创建的目录"使用**（~/tts），别拿它去动别处 —— 递归改归属是危险动作，范围必须明确。
func chownTree(user, root string) error {
	uid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-u", user)))
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-g", user)))
	if err != nil {
		return err
	}
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return nil // 单个条目失败不影响其余
		}
		_ = os.Chown(path, uid, gid)
		return nil
	})
}

// qwenBindHost 决定 Qwen 服务绑哪个地址：加鉴权绑回环（对外只由 8899 的接收端提供）；
// 不加鉴权绑 0.0.0.0（网站那台机器要能直接连上 8880）。
func qwenBindHost(auth bool) string {
	if auth {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

// ==== 模型切换 ====
// mlx_audio.server 自带模型管理接口（GET /v1/models 列出驻留、POST 加载、DELETE 卸载）：
// 按请求 model 字段按需加载、加载后就一直留着，**没有任何淘汰机制** —— 所以面板显式管理：切换时加载目标、卸载其余。

// QwenModelState 是单个模型在界面上的完整状态。
type QwenModelState struct {
	QwenModel
	// Downloaded 权重是否已在本机
	Downloaded bool `json:"downloaded"`
	// Loaded 是否已驻留内存（可立即推理）
	Loaded bool `json:"loaded"`
	// Default 是否为插件默认填写的那个
	Default bool `json:"default"`
}

// qwenBaseURL 返回本机 Qwen 服务的地址。
// 单测用 qwenPortOverride 改端口，隔离真实 8880 上的服务。
func (m *Manager) qwenBaseURL() string {
	port := qwenPort
	if m.qwenPortOverride > 0 {
		port = m.qwenPortOverride
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// qwenAPI 调一次 Qwen 服务端的 HTTP 接口。
func (m *Manager) qwenAPI(ctx context.Context, method, path string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, m.qwenBaseURL()+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 Qwen 服务失败（可能未启动）: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("Qwen 服务返回 %d: %s", resp.StatusCode, tailText(string(body), 200))
	}
	return body, nil
}

// qwenLoadedModels 返回当前驻留内存的模型名。
func (m *Manager) qwenLoadedModels(ctx context.Context) (map[string]bool, error) {
	body, err := m.qwenAPI(ctx, http.MethodGet, "/v1/models", 15*time.Second)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析 /v1/models 响应失败: %w", err)
	}
	set := map[string]bool{}
	for _, d := range out.Data {
		set[d.ID] = true
	}
	return set, nil
}

// QwenModelsStatus 汇总各模型的下载与驻留状态。
// 服务没起来时不报错：把 Loaded 全置 false 即可，
// 界面仍能告诉用户"装没装"，而不是整个接口失败。
func (m *Manager) QwenModelsStatus(ctx context.Context) []QwenModelState {
	loaded, err := m.qwenLoadedModels(ctx)
	if err != nil {
		loaded = map[string]bool{}
	}
	out := make([]QwenModelState, 0, len(QwenModels))
	for _, mdl := range QwenModels {
		out = append(out, QwenModelState{
			QwenModel:  mdl,
			Downloaded: modelDownloaded(m.opt.UserHome, mdl.Name),
			Loaded:     loaded[mdl.Name],
			Default:    mdl.Name == qwenDefaultModel,
		})
	}
	return out
}

// SetQwenModel 确保指定模型已加载，可以立即推理（幂等：已驻留直接返回，不重复读权重）。
// 清单里只有一个模型，所以实际就是"加载 Base"；保留名字与形状是为了调用点不随清单变化。
// 与 UnloadQwenModel 的分工：mlx-audio 没有淘汰机制，只有 DELETE 才释放内存。
func (m *Manager) SetQwenModel(ctx context.Context, name string) error {
	var target *QwenModel
	for i := range QwenModels {
		if QwenModels[i].Name == name {
			target = &QwenModels[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("未知的模型: %s", name)
	}
	if !modelDownloaded(m.opt.UserHome, name) {
		return fmt.Errorf("%s 的权重尚未下载，请先安装", target.Label)
	}
	// 已经驻留就不用再走一次加载（服务端加载一次约 20-30 秒）。
	if loaded, err := m.qwenLoadedModels(ctx); err == nil && loaded[name] {
		return nil
	}
	if _, err := m.qwenAPI(ctx, http.MethodPost,
		"/v1/models?model_name="+url.QueryEscape(name), 5*time.Minute); err != nil {
		return fmt.Errorf("加载 %s 失败: %w", target.Label, err)
	}
	return nil
}

// UnloadQwenModel 把指定模型从内存中释放；释放后下次用到会自动重新加载。
// 提供它的理由：同时跑 Docker/数据库的机器上，用户可能仍想主动腾出内存。
func (m *Manager) UnloadQwenModel(ctx context.Context, name string) error {
	var target *QwenModel
	for i := range QwenModels {
		if QwenModels[i].Name == name {
			target = &QwenModels[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("未知的模型: %s", name)
	}
	if _, err := m.qwenAPI(ctx, http.MethodDelete,
		"/v1/models?model_name="+url.QueryEscape(name), time.Minute); err != nil {
		return fmt.Errorf("释放 %s 失败: %w", target.Label, err)
	}
	return nil
}

// QwenUnloadStale 释放"驻留在内存里、但已不在面板模型清单里"的模型。
// mlx-audio 没有淘汰机制（只有显式 DELETE 才释放），清单会随网站侧升级而变，
// 所以换模型后自动回收内存，不用人工重启服务；清单内的模型**无论如何不碰**。
func (m *Manager) QwenUnloadStale(ctx context.Context) (unloaded []string, failed int) {
	loaded, err := m.qwenLoadedModels(ctx)
	if err != nil {
		return nil, 0
	}
	managed := map[string]bool{}
	for _, mdl := range QwenModels {
		managed[mdl.Name] = true
	}
	for name := range loaded {
		if managed[name] {
			continue
		}
		if _, err := m.qwenAPI(ctx, http.MethodDelete,
			"/v1/models?model_name="+url.QueryEscape(name), time.Minute); err != nil {
			qwenLog.Warn("释放已不在清单里的模型 %s 失败: %v", name, err)
			failed++
			continue
		}
		unloaded = append(unloaded, name)
	}
	if len(unloaded) > 0 {
		qwenLog.Info("已释放不再使用的模型：%s（清单里已经没有它们了）",
			strings.Join(unloaded, ", "))
	}
	return unloaded, failed
}

// EnsureQwenModelsLoaded 把清单里已下载的模型都加载好（部署完成后立刻预热）。
// 这样网站上第一次请求就不用等 20 多秒的模型加载，直接出声。
func (m *Manager) EnsureQwenModelsLoaded(ctx context.Context) int {
	n := 0
	for _, mdl := range QwenModels {
		if !modelDownloaded(m.opt.UserHome, mdl.Name) {
			continue
		}
		if err := m.SetQwenModel(ctx, mdl.Name); err == nil {
			n++
		}
	}
	return n
}

// QwenActiveModel 返回当前驻留的模型名（可能为空 = 尚未加载任何模型）。
func (m *Manager) QwenActiveModel(ctx context.Context) string {
	loaded, err := m.qwenLoadedModels(ctx)
	if err != nil {
		return ""
	}
	for _, mdl := range QwenModels {
		if loaded[mdl.Name] {
			return mdl.Name
		}
	}
	return ""
}

// QwenWarmResident 确保已下载的模型都驻留内存，返回本次补载的个数。
// mlx-audio 的 ModelProvider 是普通 dict（只增不删、无 LRU/上限/TTL），交替请求不会互相挤掉；
// 但它是**进程内存**，服务一重启就全变冷 —— 冷加载实测 25 秒（热 2.1 秒），会逼近网站 60 秒超时，故后台周期补载。
func (m *Manager) QwenWarmResident(ctx context.Context) (warmed, failed int) {
	loaded, err := m.qwenLoadedModels(ctx)
	if err != nil {
		// 服务没起来 / 连不上：静默返回（常驻循环每轮都会遇到的正常情况，不该刷日志）。
		return 0, 0
	}
	for _, mdl := range QwenModels {
		if loaded[mdl.Name] || !modelDownloaded(m.opt.UserHome, mdl.Name) {
			continue
		}
		if err := m.SetQwenModel(ctx, mdl.Name); err != nil {
			qwenLog.Warn("预热 %s 失败: %v", mdl.Label, err)
			failed++
			continue
		}
		warmed++
	}
	return warmed, failed
}

// qwenServiceRunning 判断面板登记的 Qwen 服务当前是否真的在跑。
// 查不到记录、或状态查询失败时返回 false —— 宁可不预热，也不要对着一台
// 没装这个服务的机器反复发请求。
func (m *Manager) qwenServiceRunning(ctx context.Context) bool {
	if m.repo == nil {
		return false
	}
	list, err := m.repo.List(ctx)
	if err != nil {
		return false
	}
	for _, s := range list {
		if s.LaunchLabel != qwenLabel {
			continue
		}
		drv, err := m.DriverFor(s)
		if err != nil {
			return false
		}
		st, err := drv.Status(ctx)
		if err != nil {
			return false
		}
		return st.Running
	}
	return false
}

// QwenKeepWarmInterval 是后台守温的检查间隔；每轮只是一次 GET /v1/models（毫秒级），间隔短代价很小。
// 取 2 分钟是为启动竞态：面板与 Qwen 一起开机时第一轮可能撞上它还没起来，间隔越长重启后的空窗越久。
const QwenKeepWarmInterval = 2 * time.Minute

// StartQwenKeepWarm 常驻守温，直到 ctx 结束。
// 首次检查**立即**执行（不等第一个 tick）：最常见的场景就是"机器刚重启完、面板与服务一起起来"。
func (m *Manager) StartQwenKeepWarm(ctx context.Context) {
	check := func() {
		if !m.qwenServiceRunning(ctx) {
			return
		}
		// 顺序有讲究：① 先清"已不在清单里"的旧模型 ② 再补载清单里的模型。
		// 必须在清理**之后**，否则会出现"先卸掉唯一驻留的模型、再慢慢加载"的空窗期。
		m.QwenUnloadStale(ctx)
		warmed, _ := m.QwenWarmResident(ctx)
		if warmed > 0 {
			qwenLog.Info("守温：已补载 %d 个模型（网站下一个请求不必再等冷加载）", warmed)
		}
	}

	check()

	t := time.NewTicker(QwenKeepWarmInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// qwenIPv4Sitecustomize 是写进 venv 的 sitecustomize.py 内容。
// 真机实测：本网络下 hf-mirror 解析出的 IPv6 **不可达却也不立刻拒绝**，python 会挂到 Errno 60 超时；强制 IPv4 后一次下全 14 个文件。
// 用 sitecustomize 而不是 PYTHONSTARTUP（后者只在**交互式**解释器生效），且只影响这个 venv。
const qwenIPv4Sitecustomize = `"""由 ZizPanel 生成：让这个虚拟环境优先使用 IPv4。

这台机器所处的网络里，某些域名会解析出**不可达但不会立刻拒绝**的 IPv6 地址，
Python 客户端会一直挂在 connect 上直到超时（症状是
"Local entry not found ... Operation timed out"）。
把 getaddrinfo 限制到 AF_INET 可以绕开，实测下载速度与稳定性都正常。
"""

import socket

_orig = socket.getaddrinfo


def _zizpanel_v4(host, port, family=0, type=0, proto=0, flags=0):
    return _orig(host, port, socket.AF_INET, type, proto, flags)


socket.getaddrinfo = _zizpanel_v4
`

// installIPv4Sitecustomize 把 IPv4 优先补丁写进 venv 的 site-packages。
// 刻意不做成"失败就算了"的静默降级：症状是"下载变慢/超时"，写不进去要留下痕迹（调用方写进任务步骤）。
func (m *Manager) installIPv4Sitecustomize(ctx context.Context, p qwenPaths, result *InstallResult) error {
	// 目录名随 Python 版本变（python3.11 / python3.12 …），所以从同一个常量推导，
	// 不写死 —— 写错的表现是补丁文件被写进一个不存在的目录，而且当场不报错。
	sp := panelPythonSitePackages(p.Venv, qwenPythonVer)
	if sp == "" {
		return fmt.Errorf("无法从 %q 推导 site-packages 目录", qwenPythonVer)
	}
	if st, err := os.Stat(sp); err != nil || !st.IsDir() {
		return fmt.Errorf("找不到 %s", sp)
	}
	dst := filepath.Join(sp, "sitecustomize.py")
	// 已经是我们写的那份就不重复写（保持幂等，重装时不会来回改文件）
	if b, err := os.ReadFile(dst); err == nil && strings.Contains(string(b), "_zizpanel_v4") {
		return nil
	}
	if err := os.WriteFile(dst, []byte(qwenIPv4Sitecustomize), 0o644); err != nil {
		return err
	}
	if m.opt.UserName != "" {
		_ = chownTree(m.opt.UserName, dst)
	}
	if result != nil {
		result.step(ctx, "已写入 IPv4 优先补丁（绕开不可达的 IPv6 地址）")
	}
	return nil
}

// qwenIPv4SitecustomizeForTest 把补丁内容暴露给测试（仅测试使用）。
func qwenIPv4SitecustomizeForTest() string { return qwenIPv4Sitecustomize }

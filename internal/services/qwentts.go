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

// qwenLog 是这个包的日志出口。守温是后台常驻循环，它的失败必须能在
// 面板日志里看到（否则"网站第一次请求为什么慢"会变成一个查不出来的问题）。
var qwenLog = logx.New("services")

// ============================================================================
//  Qwen3 TTS（mlx-audio）部署
//
//  严格按网站插件 TtsVoice 的部署契约实现（zizdog.cn 的
//  usr/plugins/TtsVoice/DEPLOY-QWEN.md），因为面板装的这个服务
//  是要给那个插件调用的 —— 端口、路径、模型名、启动方式都不能自由发挥。
//
//  与手册的**一处刻意不同**（也是面板存在的意义）：
//    手册把服务写成 ~/Library/LaunchAgents 下的**用户级** agent。
//    用户级服务只有在该用户登录进图形界面后才会运行 ——
//    对一台不接显示器、重启后停在登录界面的服务器来说，这等于开机不自启。
//    面板改为写 /Library/LaunchDaemons + UserName=<真实用户>：
//    既开机自启，又以普通用户身份运行（不违反 mlx 的权限预期）。
//
//  契约要点（照抄手册，不要"优化"）：
//    · 端口 8880；**绑哪个地址取决于鉴权**（见 qwenBindHost）——
//      加鉴权时绑 127.0.0.1，由 8899 的接收端做带密钥的反代；
//      不加鉴权时才绑 0.0.0.0，此时同内网谁能连上谁就白用这块 GPU。
//      早期这里写的是"必须 0.0.0.0"，那是引入鉴权前的旧契约，已作废。
//    · 模型只有一个：Base（克隆）。2026-09-14 起网站侧只支持自定义音色，
//      CustomVoice（预置音色）整体下线，权重也已从两台机器上删掉腾空间。
//    · Python 3.11（mlx-audio 在 3.11 上有预编译 wheel）
//    · pip 走清华源、模型走 hf-mirror
//    · **HF_HUB_DISABLE_XET=1 必须设**：不设会下载到一半报
//      "CAS Client Error ... us.gcp.cdn.hf.co"，看着像网络问题，
//      其实是 hf-mirror 不代理 Xet 后端。这是最容易白折腾半天的一条。
// ============================================================================

// QwenModel 描述一个可用的 TTS 模型。
//
// 2026-09-14 起**只有一个模型**（见 usr/plugins/TtsVoice/HANDOFF-TO-PANEL-1.7B.md）：
// 网站侧插件已只支持「自定义音色」（克隆），预置音色（CustomVoice）整体下线。
//
// 这里保留结构体与列表概念（而不是退化成一个字符串常量），理由是它同时承担
// 三件事：界面上要显示"下没下载 / 驻没驻留"、下载时要逐个处理、
// 以及"驻留集合里出现了清单之外的模型就释放掉"（QwenUnloadStale 靠它算差集）。
type QwenModel struct {
	// Name 是 HuggingFace 仓库名，同时也是 API 请求里 model 字段的取值
	Name string `json:"name"`
	// Role 是能力标签：clone（克隆）/ preset（预置音色）
	Role string `json:"role"`
	// Label 是界面上的显示名
	Label string `json:"label"`
	// Note 说明这个模型能做什么、不能做什么
	Note string `json:"note"`
}

// QwenModels 是可用模型清单。顺序即界面顺序，第一个是默认。
//
// 只有一个：Base（克隆）。CustomVoice 相关的条目已全部移除 ——
// 网站侧不再发来那个 model 名，留着条目只会让界面显示出"可以切过去"的假选项，
// 而那个模型的权重已经被删掉腾空间了（点它就是失败）。
var QwenModels = []QwenModel{
	{
		Name:  "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
		Role:  "clone",
		Label: "Base（音色克隆）",
		Note:  "支持参考音频克隆音色；网站插件现在只用这一个模型（预置音色已下线）",
	},
}

// qwenDefaultModel 是插件默认该填的那个，也是唯一的那个。
//
// 2026-09-14 起网站侧全面切到 1.7B 单模型（见 usr/plugins/TtsVoice/HANDOFF-TO-PANEL-1.7B.md），
// 0.6B 与 CustomVoice 都不再使用 —— 面板这里必须跟着改，否则面板的"驻留模型"
// 页面会把已经不用的模型当成可选项，用户点了等于切到一个不存在的权重上。
const qwenDefaultModel = "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit"

const (
	qwenPort      = 8880
	qwenLabel     = "com.zizdog.qwen3tts"
	qwenPythonVer = "python@3.11"
	qwenPipMirror = "https://pypi.tuna.tsinghua.edu.cn/simple"
	qwenHFMirror  = "https://hf-mirror.com"
	qwenMinDiskGB = 10
	qwenMinMemGB  = 15
	// qwenReadyTimeout 是「等 Qwen 端口监听」的上限。首次加载模型要十几秒，
	// 取 90 秒给慢机器留余量；超时即**如实失败**（见 waitQwenReady）。
	qwenReadyTimeout = 90 * time.Second
)

// QwenLabel 是 Qwen 服务的 launchd 标签。别的包（如 web 的服务操作）
// 需要判断"刚动过的是不是 Qwen 服务"，硬抄字符串迟早会抄错。
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

// QwenOptions 是部署 Qwen3 TTS 时用户可做的选择。
//
// 为什么把"要不要鉴权"做成选项而不是写死：
// mlx-audio 上游完全没有鉴权，所以对外暴露与否是一个**真实的取舍**——
//
//	· 加鉴权（默认）：Qwen 只监听 127.0.0.1，由 8899 的接收端做带密钥的反代。
//	  代价是多一个进程，好处是同内网别人用不了你的 GPU。
//	· 不加鉴权：Qwen 直接监听 0.0.0.0，网站直连 8880、密钥留空。
//	  只有在自己完全可控的网络里才该这么用。
//
// 网站插件两种都支持（`openaiKey` 允许为空），所以选哪种都能对接。
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
	// 全新 macOS 上三样都没有：/usr/bin/python3 只是占位程序（跑它会弹图形对话框）、
	// 没有 brew、更没有 python@3.11。这一整套（venv + pip + mlx-audio）全都要它们。
	// EnsureHomebrew 会先装 CLT 再装 brew；缺什么装什么，已就绪就秒过。
	if err := m.EnsureHomebrew(ctx, result); err != nil {
		return err
	}

	// ---- 0b. 基础依赖：ffmpeg ----
	//
	// 这一步是 2026-09-16 事故的**确切发生点**：mini 被抹机后由面板重装 Qwen TTS，
	// 整条安装链里没有 ffmpeg（这个文件当时全文搜不到它）。mlx_audio 编码 mp3
	// 必须靠 ffmpeg，于是装出来的服务"能启动、能回 wav、一合成 mp3 就返回
	// HTTP 200 + 0 字节 body" —— 接收端只看到 IncompleteRead(0 bytes read)，
	// 用户所有 TTS 作业全败，而安装任务**显示成功**、健康检查全绿。
	// （Qwen 日志里 "RuntimeError: ffmpeg not found!" 出现 601 次。）
	//
	// 所以放在最前面、并要求致命失败：装不上 ffmpeg 就中止，绝不再交付
	// 一个"看起来装好了"的半残服务。
	// 先按目录里声明的 Requires 提示一句"需要 ffmpeg"，再真的装（两个动作分开，
	// 是为了任务日志里"为什么需要"出现在"正在安装"之前）。
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
	// 指定 3.11 不是偏好：mlx-audio 在该版本有预编译 wheel（cp311），
	// 更高版本可能没有 wheel，会退化成源码编译甚至装不上。
	if !m.brewHas(ctx, qwenPythonVer) {
		result.step(ctx, "正在安装 "+qwenPythonVer)
		// 2026-09-20 事故就是这一步：镜像是坏的（0 字节瓶）→ 必须换源重试；
		// python@3.11 的 arm64 瓶国内镜像常常没有，只能靠官方源兜底。
		if _, err := m.brewInstall(ctx, result, 20*time.Minute, qwenPythonVer); err != nil {
			return fmt.Errorf("安装 %s 失败: %w", qwenPythonVer, err)
		}
	} else {
		result.step(ctx, qwenPythonVer+" 已安装，跳过")
	}
	py311 := filepath.Join(m.brewPrefix(), "opt", qwenPythonVer, "bin", "python3.11")
	if _, err := os.Stat(py311); err != nil {
		return fmt.Errorf("找不到 %s", py311)
	}

	// ---- 2. 虚拟环境 ----
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", p.Root, err)
	}
	// 必须**递归**改归属：上面是以 root 身份 mkdir 的，
	// 只 chown 父目录的话，~/tts/qwen3 仍是 root 所有 ——
	// 接着以用户身份建 .venv 就会 Permission denied。
	// 真机上就是这么失败的（只差了子目录这一层）。
	if m.opt.UserName != "" {
		_ = chownTree(m.opt.UserName, p.Home)
	}
	if _, err := os.Stat(p.Python); err != nil {
		result.step(ctx, "正在创建 Python 虚拟环境")
		if out, err := m.runAsUser(ctx, 5*time.Minute, py311, "-m", "venv", p.Venv); err != nil {
			return fmt.Errorf("创建虚拟环境失败: %v（%s）", err, tailText(out, 300))
		}
	} else {
		result.step(ctx, "虚拟环境已存在，跳过创建")
	}
	// 2b) 强制 IPv4：这个网络上 IPv6 地址**不可达但会挂住**（不是立刻失败）。
	if err := m.installIPv4Sitecustomize(ctx, p, result); err != nil {
		// 不致命：下不动模型时下载步骤会如实报错，而不是在这里假装成功
		result.step(ctx, "警告：未能写入 IPv4 优先补丁（"+err.Error()+"），下载可能变慢")
	}

	// ---- 3. 安装 mlx-audio[server] ----
	// [server] 这个 extra 是最容易漏的一步：不带它只有库、没有 HTTP 服务。
	if err := m.pipInstall(ctx, p, result); err != nil {
		return err
	}

	// ---- 4. 下载模型 ----
	if err := m.downloadQwenModel(ctx, p, result); err != nil {
		return err
	}

	// ---- 5. 注册为系统级守护进程 ----
	// 绑定地址由"要不要鉴权"决定：不加鉴权就必须对外暴露（否则网站连不上）
	if err := m.installQwenService(ctx, p, result, opt.Auth); err != nil {
		return err
	}

	// ---- 6. 先登记进服务管理，再验收 ----
	//
	// 顺序是刻意的（2026-09-17 审计）：原来登记在验收**之后**，而验收失败
	// 只写一条 Warning 就 return nil。一旦把验收改成如实失败（见 waitQwenReady），
	// 登记留在后面就会留下「任务失败、服务管理里又找不到它」的查不下去的半成品。
	// 先登记，失败时错误信息才能如实说「服务已登记，但端口没监听」。
	// 登记失败**不**让整个部署失败：launchd 服务本身是好的、网站也连得上，
	// 只是面板列表里暂时没有它（可在「可纳管」里手工加入）。这是刻意接受的降级，
	// 理由：「能用但没登记」比「能用却报失败、让用户重装」更不容易造成损失；
	// 下面的就绪验收仍会如实判定服务到底起没起来，并把登记结果写进失败信息。
	registered := true
	if err := m.RegisterInstalledService(ctx, qwenLabel, "Qwen3 TTS", "🗣️", "ai", qwenPort); err != nil {
		registered = false
		result.step(ctx, "（自动登记到服务管理失败："+err.Error()+"，可在「可纳管」里手动加入）")
	}

	// ---- 6b. 验证：端口真的在监听才算成功（首次加载模型要十几秒）----
	result.step(ctx, "正在等待服务加载模型（首次约十几秒）")
	if err := m.waitQwenReady(ctx, p, registered, result); err != nil {
		return err
	}

	// 预热模型。首次加载需 20-30 秒，放在这里做掉，
	// 网站上第一次请求就能直接出声，而不是让用户等半分钟以为坏了。
	result.step(ctx, "正在预热模型（首次约 20-30 秒）…")
	if n := m.EnsureQwenModelsLoaded(ctx); n > 0 {
		result.step(ctx, fmt.Sprintf("已加载 %d 个模型，音色克隆可立即使用", n))
	}

	// ---- 6c. 收尾验收：用**服务进程的 PATH** 真的跑一次 ffmpeg ----
	//
	// EnsureBaseDependencies 已经装过它，但"brew 说装好了"不等于"服务进程能用"：
	// 动态库坏了、没链接上、或者服务进程的 PATH 里没有 Homebrew 都可能发生 ——
	// 真机事故里最难查的正是这种"文件在、跑不通"的状态（服务照常启动，
	// 只在合成 mp3 时才以"200 + 空 body"的形式暴露）。
	// 所以这里按服务 plist 的 PATH 实跑 `ffmpeg -version`；失败就明确报错，
	// 宁可任务显示失败，也不要交付一个一合成 mp3 就返回空 body 的服务。
	if problems := m.VerifyBaseDependencies(ctx); len(problems) > 0 {
		msg := "基础依赖验收未通过：" + describeDependencyProblems(problems) +
			"。服务已注册，但合成 mp3 会失败（HTTP 200 + 空 body）；" +
			"修好后请重新部署，或手工执行 `brew install ffmpeg`"
		result.Warning = msg
		result.step(ctx, "错误："+msg)
		return fmt.Errorf("%s", msg)
	}
	result.step(ctx, "已确认 ffmpeg / ffprobe 可执行（mlx_audio 编码 mp3 依赖它）")

	// ---- 7. 把插件要填的东西直接列出来 ----
	// 这一步是"部署"和"能用"之间的差距：光装好服务，用户还得回去翻手册
	// 才知道插件里填什么。直接给出来，照抄即可。
	host := m.primaryIP()
	result.Address = host

	if opt.Auth {
		// 加了鉴权就必须有反代 —— 此时 Qwen 只监听 127.0.0.1，
		// 网站**只能**通过 8899 访问。所以顺手把它一起装好，
		// 否则用户会得到一个"装好了但连不上"的服务。
		//
		// 反代失败必须让**整个部署失败**（2026-09-17 审计，第 4 处同族缺陷）：
		// 原来这里只写一条 Warning 就 return nil，任务显示「✅ 完成」，
		// 而网站因为反代没起来根本连不上 —— 这正是我们在清的那一族
		// 「能谎报成功」。详见 finishQwenAuthEntry。
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
//
// 2026-09-17 审计：原来这里 90 秒没起来只写一条 Warning 就 return nil ——
// 任务于是显示「任务完成 ✅」，而 8880 根本没监听：qwen3tts 装完不能用，
// 并且**连带 TtsVoice 全站失联**（网站只连这个端口）。这个早退还顺带跳过了
// 后面的 ffmpeg 验收与 Address 回填。
//
// 现在改成如实失败，并把「装好了什么 / 还差什么 / 能做什么」写进错误里
// （写法照抄 iopaint.go 的 waitIOPaintReady）。
func (m *Manager) waitQwenReady(ctx context.Context, p qwenPaths, registered bool, result *InstallResult) error {
	wait := readyWaitPort
	state := "Python 环境、模型权重与 launchd 服务都已就位，服务也已登记进服务管理"
	if !registered {
		state = "Python 环境、模型权重与 launchd 服务都已就位" +
			"（但登记进服务管理失败，可在「可纳管」里手动加入）"
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

// finishQwenAuthEntry 在「加鉴权」时部署对外入口（音色接收端反代），
// 并把它失败时的后果**如实升级为整个部署失败**。
//
// 为什么这不是「可接受的降级」：加鉴权时 Qwen 只绑定 127.0.0.1，对外访问
// **只能**经过这个反代。反代没起来 = 网站（TtsVoice）连不上，服务等于还没装成。
// 让任务报成功只会让用户对着绿灯排查半天（2026-09-17 审计的第 4 处同族缺陷）。
//
// 为什么不套 assertReady：这不是「轮询等某个探针就绪」，而是一个子安装动作
// 本身失败（InstallVoiceReceiver 内部已经做过它自己的就绪验收，并会返回带
// 日志尾部的错误）。这里做的是「把它的失败升级为整单失败」，并把
// 「哪一步成了 / 哪一步没成 / 用户能怎么办」讲清楚。
//
// deploy 作为参数传入而不是直接调 m.InstallVoiceReceiver：单测需要在不触发
// 真实安装（要 root、要真 launchd）的前提下，锁死「失败必须升级」这条约束。
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

// checkQwenPreconditions 检查内存与磁盘。
//
// 为什么要检查：1.7B 单模型常驻约 3GB，加系统与其它服务，
// 16GB 是舒服的起点；低于它仍能跑，但要在用户动手前就提示，别让他
// 等 20 分钟下载完才失败。磁盘按实测口径：模型约 2.9GB + 环境约 0.5GB。
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
	// pip 索引按"NAS 优先、探不通回落清华"现算（见 pypi_mirror.go），
	// 选择与原因由它写进任务日志 —— 与 brew/HF 的"优先+回落"是同一套语义。
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

// downloadQwenModel 下载模型。
//
// 两个环境变量缺一不可：
//
//	HF_ENDPOINT=https://hf-mirror.com   国内直连 huggingface.co 基本不可用
//	HF_HUB_DISABLE_XET=1                新版 huggingface_hub 默认走 Xet CDN，
//	                                    而 hf-mirror 不代理那个后端 ——
//	                                    不关掉会下载到一半报 CAS Client Error，
//	                                    看着像网络抖动，其实是这个原因。
func (m *Manager) downloadQwenModel(ctx context.Context, p qwenPaths, result *InstallResult) error {
	// hf 是新版 CLI 名；旧版叫 huggingface-cli。两个都试一下，避免版本差异卡住。
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

// qwenHFEndpoint 按"优先级 + 可用性"挑 HuggingFace 端点。
//
// 顺序（2026-09-16 用户要求："能用镜像的尽量用，模型文件也一样"）：
//  1. 面板设置里的镜像基址 + /hf —— 自建镜像站的 HuggingFace 反代/缓存。
//     模型动辄 2~3GB，走同城镜像省流量也快得多。
//     但**必须探通**（HEAD <base>/hf/），不通就跳过；
//  2. 内置的 hf-mirror.com（国内公共镜像，实测可用）。
//
// 探测失败不报错：模型下载本来就有 3 次重试，这里只决定"用哪个端点"。
func (m *Manager) qwenHFEndpoint(ctx context.Context) string {
	if m.MirrorEnabled() {
		base := m.mirrorSubPath("hf")
		if err := m.checkMirrorURL(ctx, base+"/"); err == nil {
			return base
		}
	}
	return qwenHFMirror
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
//
// 注意：HuggingFace 缓存里 snapshots/ 下是**指向 blobs/ 的符号链接**，
// 而 filepath.Walk 给的是 Lstat 信息 —— 直接用 info.Size() 会拿到
// 链接自身的长度（实测 76 字节），于是"权重明明下好了"却被判成没下好。
// 所以这里必须用 os.Stat 跟随链接。
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

// installQwenService 写系统级 plist 并加载。
//
// 与手册的关键差异：这里用 LaunchDaemon + UserName，而不是
// LaunchAgents。理由见文件头。
func (m *Manager) installQwenService(ctx context.Context, p qwenPaths, result *InstallResult, auth bool) error {
	// 端点按"NAS 优先"现算后写进 plist：安装期下载模型时已经算过一次，
	// 但**服务进程**日后自己补拉模型（缓存被动过、换模型）时用的是 plist 里的值。
	// 原来这里写死 hf-mirror.com —— 与安装期的选择不一致，等于面板配了 NAS
	// 镜像、服务自己却永远走公网，属于"镜像优先"漏掉的一处。
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

// qwenPlist 生成 LaunchDaemon 定义。
//
// hfEndpoint 由调用方按"NAS 优先"现算（m.qwenHFEndpoint）—— 见 installQwenService。
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
	full := append([]string{"-n", "-u", m.opt.UserName, name}, args...)
	cmd := execCommandCtx(ctx, "/usr/bin/sudo", full...)
	e := append(os.Environ(), "HOME="+m.opt.UserHome)
	e = append(e, env...)
	cmd.Env = e
	// 模型下载（hf download）动辄几百 MB，必须逐行流式，
	// 否则用户在整个下载期间只能看到"正在下载模型"一句。
	return streamCmd(ctx, cmd)
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

// primaryIP 返回本机局域网地址，用于拼给插件填的 URL。
// PrimaryIP 导出给 web 层用：拼"服务实际跑在哪台机器上"的地址。
//
// 为什么不能让 web 层直接用请求里的客户端 IP：那是**浏览器**的地址，
// 不是服务所在机器的地址 —— 2026-09-16 真机踩到，凭据弹窗里显示成了
// 用户自己电脑的 192.168.1.179，而服务其实装在被管的那台机器上。
func (m *Manager) PrimaryIP() string { return m.primaryIP() }

func (m *Manager) primaryIP() string {
	out := runOutput("/usr/sbin/ipconfig", "getifaddr", "en0")
	if ip := strings.TrimSpace(out); ip != "" {
		return ip
	}
	return "<本机地址>"
}

// chownTree 递归把目录树归属改为指定用户。
//
// 只对"我们自己创建的目录"使用（~/tts），不要拿它去动别处 ——
// 递归改归属是个危险动作，范围必须明确。
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

// qwenBindHost 决定 Qwen 服务绑哪个地址。
//
// 加鉴权时绑回环：对外只由 8899 的接收端提供（带共享密钥）。
// 不加鉴权时绑 0.0.0.0：网站那台机器要能直接连上 8880。
func qwenBindHost(auth bool) string {
	if auth {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

// ============================================================================
//  模型切换
//
//  Qwen 服务端（mlx_audio.server）本身就带模型管理接口：
//      GET    /v1/models                     列出**已驻留内存**的模型
//      POST   /v1/models?model_name=X        加载
//      DELETE /v1/models?model_name=X        卸载
//  它是"按请求里的 model 字段按需加载、加载后就一直留着"的策略 ——
//  没有任何淘汰机制。所以两个模型各用一次之后会同时驻留（约 10GB），
//  在 16GB 机器上这是危险的。面板因此显式管理：切换时加载目标、卸载其余。
// ============================================================================

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

// QwenModelsStatus 汇总两个模型的下载与驻留状态。
//
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

// SetQwenModel 确保指定模型已加载，可以立即推理。
//
// 现在清单里只有一个模型，所以这个方法实际就是"加载 Base"。
// 保留名字与形状是为了：① 安装流程、常驻守温和界面都用它；
// ② 万一以后清单再变，调用点不用跟着改。
//
// 与 UnloadQwenModel 的分工：加载是幂等的（已驻留直接返回，不重复读权重），
// 卸载是显式的（mlx-audio 没有淘汰机制，只有 DELETE 才释放内存）。
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

// UnloadQwenModel 把指定模型从内存中释放。
//
// 提供这个操作是因为"两个都常驻"虽然实测只占约 4GB，但在同时跑 Docker、
// 数据库的机器上，用户可能仍想主动腾出内存。释放后下次用到会自动重新加载。
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
//
// 为什么需要：模型清单会随网站侧升级而变（2026-09-14 从 0.6B/双模型换成
// 1.7B 单模型）。老模型仍然占着内存，而 mlx-audio 没有淘汰机制 ——
// 只有显式 DELETE 才释放。两台机器都是 16GB，本机实测 swap 用到 7GB/8GB，
// 把 CustomVoice / 0.6B 那些已经不用的（每个约 3GB）还回去是有意义的；
// 而且**以后换模型也会自动清理**，不用人工重启服务。
//
// 这条同时覆盖了旧版 QwenEnforceSingleResident 的职责（"内存吃紧只留一个"）：
// 现在清单里本来就只有一个模型，任何多余的驻留都是"不在清单里"，
// 于是"只保留默认模型"就是这条规则的自然结果，不需要再单独写一份策略。
//
// 只动"不在 QwenModels 里"的模型：清单内的模型无论如何不碰（那是我们自己的策略）。
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

// EnsureQwenModelsLoaded 把清单里已下载的模型都加载好。
//
// 部署完成后立刻预热：这样网站上第一次请求就不用等 20 多秒的模型加载，直接出声。
// 现在清单里只有一个（Base），所以就是把它加载好。
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

// QwenWarmResident 确保两个已下载的模型都驻留在内存里，返回本次补载的个数。
//
// 为什么需要它（这是一个真实的缺口，不是保险丝）：
//
//	mlx-audio 的 ModelProvider 就是一个普通 dict —— `load_model` 只增不删，
//	没有 LRU、没有上限、没有 TTL，加载过就一直驻留。所以"两个模型交替请求
//	会不会互相挤掉"这件事**不会发生**，网站按请求切 model 是安全的。
//
//	但 dict 是**进程内存**：Qwen 服务一重启（重启机器、崩溃自愈、手动
//	kickstart），两个模型全变冷。冷加载实测要 **25 秒**（同一句话热的时候
//	只要 2.1 秒），叠加 30~60 秒的合成本身，会逼近网站插件那边的 60 秒超时，
//	表现成"合成失败"——看起来正像服务端没把模型加载对。
//
//	安装流程里已经预热过一次（InstallQwen 第 6 步），这里补的是之后的所有
//	重启场景：面板后台周期性调用，缺哪个补哪个。SetQwenModel 自己会跳过
//	已驻留的模型，所以重复调用是廉价的。
func (m *Manager) QwenWarmResident(ctx context.Context) (warmed, failed int) {
	loaded, err := m.qwenLoadedModels(ctx)
	if err != nil {
		// 服务没起来 / 连不上：静默返回。这是常驻循环每轮都会遇到的正常情况，
		// 不该每 5 分钟往日志里塞一条错误。
		return 0, 0
	}
	// 清单里现在只有一个模型（Base），逐项补载即可。
	// 内存门槛（qwenWarmAllMemGB / canWarmAllQwenModels）随双模型策略一起删掉了：
	// 那套逻辑存在的唯一理由是"两个 1.7B 同时常驻会吃 10GB"，单模型时没有意义。
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

// QwenKeepWarmInterval 是后台守温的检查间隔。
//
// 每轮只是一次 GET /v1/models（毫秒级），只有缺模型时才会真的触发加载，
// 所以间隔取得短一点代价很小。取 2 分钟是因为要考虑一个启动竞态：
// 面板与 Qwen 服务一起开机启动时，第一轮检查可能正好撞上 Qwen 还没起来，
// 那时只能等下一轮；间隔越长，重启后的空窗就越久。
const QwenKeepWarmInterval = 2 * time.Minute

// StartQwenKeepWarm 常驻守温，直到 ctx 结束。
//
// 首次检查**立即**执行（不等第一个 tick），因为最常见的触发场景就是
// "机器刚重启完、面板与服务一起起来"，这时候越早补载越好。
func (m *Manager) StartQwenKeepWarm(ctx context.Context) {
	check := func() {
		if !m.qwenServiceRunning(ctx) {
			return
		}
		// 顺序有讲究：
		//  ① 先清"已不在清单里"的旧模型（换模型后自动回收内存；
		//     预置音色下线后，这就是把 CustomVoice 还回去的那一步）
		//  ② 再补载清单里的模型（现在只有一个 Base）——
		//     必须在清理**之后**，否则可能出现"先卸掉唯一驻留的模型、
		//     再慢慢加载"的空窗期，这期间网站请求只能冷加载。
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
//
// 为什么需要它（真机实测）：这台网络环境下 hf-mirror.com 会解析出 IPv6 地址，
// 而那条 IPv6 路径**不可达却也不立刻拒绝** —— python 客户端就一直挂在 connect 上，
// 最终报 `Error: Local entry not found. [Errno 60] Operation timed out`。
// 同一个 URL 用 curl 走 IPv4 是 5 MB/s，用强制 IPv4 的 python 一次下全 14 个文件。
//
// 为什么写 sitecustomize 而不是设 PYTHONSTARTUP：
//   - PYTHONSTARTUP 只在**交互式**解释器里生效（实测：`python script.py` 下不执行）；
//   - sitecustomize 是 site 模块在启动时自动 import 的，对 `hf` CLI 这种
//     控制台入口脚本**一定生效**，而且 venv 自己的 site-packages 里放一份
//     只影响这个 venv，不动系统 Python。
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
//
// 刻意不做成"失败就算了"的静默降级：写不进去时要留下痕迹（调用方会写进任务步骤），
// 因为它的症状是"下载变慢/超时"，没有痕迹的话下次又得从头排查。
func (m *Manager) installIPv4Sitecustomize(ctx context.Context, p qwenPaths, result *InstallResult) error {
	sp := filepath.Join(p.Venv, "lib", "python3.11", "site-packages")
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

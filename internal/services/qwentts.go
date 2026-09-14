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
//    · 模型有两个：Base（克隆）与 CustomVoice（预置音色），两个都要装 ——
//      网站插件按请求里的 model 字段二选一。只装一个必然有一半是坏的。
//    · Python 3.11（mlx-audio 在 3.11 上有预编译 wheel）
//    · pip 走清华源、模型走 hf-mirror
//    · **HF_HUB_DISABLE_XET=1 必须设**：不设会下载到一半报
//      "CAS Client Error ... us.gcp.cdn.hf.co"，看着像网络问题，
//      其实是 hf-mirror 不代理 Xet 后端。这是最容易白折腾半天的一条。
// ============================================================================

// QwenModel 描述一个可用的 TTS 模型。
//
// 为什么要有"两个模型"这个概念，而不是一个字符串常量：
// 这两个模型能力**互斥**，一个只能干一件事 ——
//
//	· Base        支持参考音频克隆，但会**静默忽略**预置音色参数
//	· CustomVoice 支持 9 个预置音色，但不支持克隆
//
// 网站上"上传了自己的音色就用克隆、没上传就用默认音色"要两全，就必须两个都能用。
//
// 内存（0.6B 上实测，mini 16GB）：单个驻留 2.14GB、**两个都驻留 3.99GB**
// （mlx 用内存映射，`ps` 的 RSS 常只有几百 MB，权重按页换入）。
// 手册里"各 5.6GB、同时 10GB"是**网站侧 1.7B CustomVoice** 的数字，
// 不要拿它来设计 mini 的切换策略 —— 见 SetQwenModel 的说明。
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

// QwenModels 是两个可用模型。顺序即界面顺序，第一个是默认。
var QwenModels = []QwenModel{
	{
		Name:  "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
		Role:  "clone",
		Label: "Base（音色克隆）",
		Note:  "支持上传参考音频克隆音色；会静默忽略 voice 预置音色参数",
	},
	{
		Name:  "mlx-community/Qwen3-TTS-12Hz-1.7B-CustomVoice-8bit",
		Role:  "preset",
		Label: "CustomVoice（预置音色）",
		Note:  "支持 9 个预置音色（vivian/serena/ryan/aiden/eric/dylan/uncle_fu/ono_anna/sohee）；不支持克隆",
	},
}

// qwenDefaultModel 是插件默认该填的那个：克隆是主用法，且历史配置都是它。
//
// 2026-09-14 起网站侧全面切到 1.7B（见 usr/plugins/TtsVoice/HANDOFF-TO-PANEL-1.7B.md），
// 0.6B 不再使用 —— 面板这里必须跟着改，否则面板的"驻留模型"页面会把已经不用的
// 0.6B 当成默认项，用户点了等于切回旧模型。
const qwenDefaultModel = "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit"

const (
	qwenPort      = 8880
	qwenLabel     = "com.zizdog.qwen3tts"
	qwenPythonVer = "python@3.11"
	qwenPipMirror = "https://pypi.tuna.tsinghua.edu.cn/simple"
	qwenHFMirror  = "https://hf-mirror.com"
	qwenMinDiskGB = 10
	// qwenWarmAllMemGB 是"允许把**两个**模型都常驻"的内存门槛。
	//
	// 为什么需要它：0.6B 时代两个模型都常驻约 4GB，16GB 机器很宽裕；
	// 2026-09-14 换成 1.7B 之后两个都要 10GB 上下，再叠加 Docker / MySQL /
	// Ollama / 面板自身，16GB 的机器会开始换页（实测本机 swap 已用 7GB/8GB）。
	// 交接文档的口径也是"吃紧就让它冷加载（首次约 25 秒，不影响正确性）"。
	// 低于这个门槛就只常驻**一个**（默认那个，克隆是主用法），其余的按需冷加载。
	qwenWarmAllMemGB = 24
	qwenMinMemGB     = 15
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
		if _, err := m.brewRun(ctx, 20*time.Minute, "install", qwenPythonVer); err != nil {
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

	// ---- 6. 验证：等端口起来（首次加载模型要十几秒）----
	result.step(ctx, "正在等待服务加载模型（首次约十几秒）")
	if !waitPort(ctx, qwenPort, 90*time.Second) {
		result.Warning = fmt.Sprintf("服务已注册，但 %d 秒内 %d 端口未监听。"+
			"可查看日志：%s", 90, qwenPort, p.ErrLog)
		result.step(ctx, "警告："+result.Warning)
		return nil
	}
	result.step(ctx, fmt.Sprintf("Qwen3 TTS 已就绪，监听 %d 端口", qwenPort))

	// 预热两个模型。首次加载各需 20-30 秒，放在这里做掉，
	// 网站上第一次请求就能直接出声，而不是让用户等半分钟以为坏了。
	result.step(ctx, "正在预热两个模型（各自约 20-30 秒）…")
	if n := m.EnsureQwenModelsLoaded(ctx); n > 0 {
		result.step(ctx, fmt.Sprintf("已加载 %d 个模型，克隆与预置音色都可立即使用", n))
	}

	// 自动登记进服务管理（用户不必再手工纳管）
	if err := m.RegisterInstalledService(ctx, qwenLabel, "Qwen3 TTS", "🗣️", "ai", qwenPort); err != nil {
		result.step(ctx, "（自动登记到服务管理失败："+err.Error()+"，可在「可纳管」里手动加入）")
	}

	// ---- 7. 把插件要填的东西直接列出来 ----
	// 这一步是"部署"和"能用"之间的差距：光装好服务，用户还得回去翻手册
	// 才知道插件里填什么。直接给出来，照抄即可。
	host := m.primaryIP()
	result.Address = host

	if opt.Auth {
		// 加了鉴权就必须有反代 —— 此时 Qwen 只监听 127.0.0.1，
		// 网站**只能**通过 8899 访问。所以顺手把它一起装好，
		// 否则用户会得到一个"装好了但连不上"的服务。
		result.step(ctx, "",
			"已选择加鉴权 → 继续部署对外入口（带共享密钥的反向代理）")
		if err := m.InstallVoiceReceiver(ctx, result, ReceiverOptions{Token: opt.Token}); err != nil {
			result.Warning = "Qwen 已就绪，但反向代理部署失败：" + err.Error() +
				"（此时 8880 只监听本机，网站连不上，请重试）"
			result.step(ctx, "警告："+result.Warning)
		}
		return nil
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
		"  ↑ 已装两个模型，插件里按需要填其中之一：",
		"     "+QwenModels[0].Name,
		"         → 音色克隆（上传了参考音频时用）",
		"     "+QwenModels[1].Name,
		"         → 9 个预置音色（vivian/serena/ryan/aiden/eric/dylan/uncle_fu/ono_anna/sohee）",
		"     两个都已加载好，改插件配置即可切换，不用重启服务。",
		"  openaiFlavor  = qwen3tts    ← 选它才走异步模式",
		"  openaiFormat  = mp3",
		"",
		"⚠️ 当前 8880 对全网开放且无鉴权：同内网任何人都能白用这块 GPU。",
		"   如需音色克隆，再点「部署音色接收端」并把密钥留空即可。",
	)
	return nil
}

// checkQwenPreconditions 检查内存与磁盘。
//
// 为什么要检查：两个 0.6B 模型同时驻留实测约 4GB，加系统与其它服务，
// 16GB 是舒服的起点；低于它仍能跑，但要在用户动手前就提示，别让他
// 等 20 分钟下载完才失败。磁盘按实测口径：两个模型约 3.7GB + 环境约 0.5GB。
func (m *Manager) checkQwenPreconditions(ctx context.Context, result *InstallResult) error {
	memGB := memoryGB()
	diskGB := freeDiskGB(m.opt.UserHome)
	result.Steps = append(result.Steps,
		fmt.Sprintf("环境检查：内存 %dGB，可用磁盘 %dGB", memGB, diskGB))

	if memGB > 0 && memGB < qwenMinMemGB {
		result.Warning = fmt.Sprintf(
			"内存只有 %dGB（建议 16GB 起）：两个模型同时驻留约 4GB，跑起来会偏紧", memGB)
		result.step(ctx, "警告："+result.Warning)
	}
	if diskGB > 0 && diskGB < qwenMinDiskGB {
		return fmt.Errorf("可用磁盘只有 %dGB，建议至少 %dGB（环境约 0.5GB + 两个模型约 3.7GB）",
			diskGB, qwenMinDiskGB)
	}
	return nil
}

// pipInstall 装 pip 与 mlx-audio[server]。
func (m *Manager) pipInstall(ctx context.Context, p qwenPaths, result *InstallResult) error {
	pip := filepath.Join(p.Venv, "bin", "pip")
	// 先升级 pip：旧 pip 解析 mlx-audio 的依赖树可能失败
	if out, err := m.runAsUser(ctx, 5*time.Minute, pip, "install", "-U", "pip", "-i", qwenPipMirror); err != nil {
		return fmt.Errorf("升级 pip 失败: %v（%s）", err, tailText(out, 300))
	}
	result.step(ctx, "pip 已升级（走清华源）")

	// 判断是否已装：重复执行时不该再花十几分钟重装
	out, _ := m.runAsUser(ctx, time.Minute, pip, "list")
	if strings.Contains(out, "mlx-audio") {
		result.step(ctx, "mlx-audio 已安装，跳过")
		return nil
	}
	result.step(ctx, "正在安装 mlx-audio[server]（依赖较多，请耐心等待）")
	if out, err := m.runAsUser(ctx, 40*time.Minute, pip, "install", "mlx-audio[server]", "-i", qwenPipMirror); err != nil {
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

	// 两个模型都要下：只装一个的话，另一种能力在运行时才发现用不了
	// （Base 会静默忽略预置音色，CustomVoice 不会克隆），排查起来很费劲。
	for _, mdl := range QwenModels {
		if err := m.downloadOneQwenModel(ctx, hf, mdl, result); err != nil {
			return err
		}
	}
	return nil
}

// downloadOneQwenModel 下载单个模型并处理续传重试。
func (m *Manager) downloadOneQwenModel(ctx context.Context, hf string, mdl QwenModel, result *InstallResult) error {
	if modelDownloaded(m.opt.UserHome, mdl.Name) {
		result.step(ctx, mdl.Label+" 已下载，跳过")
		return nil
	}

	result.Steps = append(result.Steps,
		"正在下载 "+mdl.Label+"（约 2GB / 14 个文件，视网络 5~15 分钟）")
	env := []string{"HF_ENDPOINT=" + qwenHFMirror, "HF_HUB_DISABLE_XET=1"}

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
	plist := qwenPlist(p, m.opt.UserName, auth)
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
	result.Steps = append(result.Steps,
		"已注册为系统级后台服务（开机自启、不依赖用户登录）")
	return nil
}

// qwenPlist 生成 LaunchDaemon 定义。
func qwenPlist(p qwenPaths, user string, auth bool) string {
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
`, qwenLabel, user, p.Python, qwenBindHost(auth), qwenPort, p.Root, qwenHFMirror, p.OutLog, p.ErrLog)
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
// 关于"要不要卸载另一个模型"，这里以**实测**为准，而不是沿用文档的估算：
// 文档写的是"单模型约 5.6GB、两个同时驻留峰值 10GB"，据此看 16GB 机器很紧张。
// 但在 0.6B-8bit 上实测（mini，16GB）：
//
//	只加载 Base         → Qwen RSS 2.14GB，系统可用内存 92%，swap 0
//	两个都驻留          → Qwen RSS 3.99GB，系统可用内存 92%，swap 0
//
// 两个加起来才约 4GB，远没有到需要互相驱逐的程度。
//
// 因此默认**保留另一个模型**：这样插件无论发来哪个 model 名都能立刻响应，
// 不必等 20 多秒重新加载。需要腾内存时，界面上可以显式卸载（UnloadQwenModel）。
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

// QwenEnforceSingleResident 在内存不宽裕的机器上强制"只驻留一个模型"。
//
// 策略来自用户明确要求（2026-09-14）：**1.7B 只驻留 1 个，默认常驻克隆用的 Base。**
// 为什么必须"强制"而不只是"不主动预热"：网站按请求切模型（上传过样本走 Base、
// 否则走 CustomVoice），只要两边都用过一次，两个 1.7B 就都会留在内存里 ——
// mlx-audio **没有淘汰机制**，只有显式 DELETE 才释放。两台机器都是 16GB，
// 实测两个 1.7B 常驻时本机 swap 用到 7.3GB/8GB，已经明显换页。
//
// 内存宽裕（≥ qwenWarmAllMemGB）的机器不动：那时候两个都常驻反而更快。
// 被卸掉的那个不会"坏"——下次请求会冷加载（约 25 秒），交接文档明确接受这一点。
func (m *Manager) QwenEnforceSingleResident(ctx context.Context) (unloaded []string) {
	if m.canWarmAllQwenModels() {
		return nil
	}
	loaded, err := m.qwenLoadedModels(ctx)
	if err != nil {
		return nil
	}
	if len(loaded) <= 1 {
		return nil // 0 或 1 个驻留：没什么可省的
	}
	if !loaded[qwenDefaultModel] {
		// 默认模型不在驻留集合里：不乱卸。调用点会先补载默认模型，
		// 下一轮再走到这里自然就会把多余的那个清掉 ——
		// 立刻卸会在"唯一能用的模型"上开天窗。
		return nil
	}
	for name := range loaded {
		if name == qwenDefaultModel {
			continue
		}
		if _, err := m.qwenAPI(ctx, http.MethodDelete,
			"/v1/models?model_name="+url.QueryEscape(name), time.Minute); err != nil {
			qwenLog.Warn("释放多余驻留的模型 %s 失败: %v", name, err)
			continue
		}
		unloaded = append(unloaded, name)
	}
	if len(unloaded) > 0 {
		qwenLog.Info("只保留 1 个常驻模型（内存吃紧）：已释放 %s，保留 %s",
			strings.Join(unloaded, ", "), qwenDefaultModel)
	}
	return unloaded
}

// QwenUnloadStale 释放"驻留在内存里、但已不在面板模型清单里"的模型。
//
// 为什么需要：模型清单会随网站侧升级而变（2026-09-14 从 0.6B 换成 1.7B）。
// 老模型仍然占着内存，而 mlx-audio 没有淘汰机制 —— 只有显式 DELETE 才释放。
// 两台机器都是 16GB，本机实测 swap 用到 7GB/8GB，把两个已经不用的 0.6B
// （约 4GB）还回去是有意义的；而且**下次换模型也会自动清理**，不用人工重启服务。
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

// EnsureQwenModelsLoaded 把两个模型都加载好。
//
// 部署完成后立刻预热：这样网站上第一次请求（无论要克隆还是预置音色）
// 都不用等 20 多秒的模型加载，直接出声。
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
	targets := QwenModels
	if !m.canWarmAllQwenModels() {
		// 内存吃紧：只保证**默认模型**常驻（网站"没上传样本就用预置音色"那条路
		// 仍会冷加载一次 CustomVoice，约 25 秒）。已经因为真实请求而驻留的模型
		// 不会被卸载 —— 这里只是不再**主动**把两个都塞进去。
		for _, mdl := range QwenModels {
			if mdl.Name == qwenDefaultModel {
				targets = []QwenModel{mdl}
				break
			}
		}
		qwenLog.Info("守温：内存 %dGB 低于 %dGB，只常驻 %s（另一个按需冷加载）",
			memoryGB(), qwenWarmAllMemGB, qwenDefaultModel)
	}
	for _, mdl := range targets {
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

// canWarmAllQwenModels 判断这台机器是否宽裕到可以同时常驻两个模型。
//
// 取不到内存信息时**保守**返回 false：宁可让第二个模型冷加载，
// 也不要把一台内存不明的机器（可能是 16GB 的笔记本）推进换页。
func (m *Manager) canWarmAllQwenModels() bool {
	// 只有测试会设置它（与 qwenPortOverride 同一风格）：
	// 阈值逻辑必须能在"16GB 机器"和"64GB 机器"两种情形下都被断言到，
	// 而真机的内存是固定的。
	gb := memoryGB()
	if m.qwenMemGBOverride > 0 {
		gb = m.qwenMemGBOverride
	}
	if gb <= 0 {
		return false
	}
	return gb >= qwenWarmAllMemGB
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
		//  ① 先清"已不在清单里"的旧模型（换模型后自动回收内存）
		//  ② 再补载默认模型 —— 必须在"强制单驻留"**之前**，
		//     否则会出现"先卸掉唯一驻留的模型、再慢慢加载"的空窗期，
		//     这期间网站请求必然冷加载。
		//  ③ 最后把多余的那个卸掉（内存吃紧时只留默认模型）
		m.QwenUnloadStale(ctx)
		warmed, _ := m.QwenWarmResident(ctx)
		m.QwenEnforceSingleResident(ctx)
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

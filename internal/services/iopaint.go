package services

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
//  IOPaint（图片去水印 / 物体擦除 / 扩图）
//
//  为什么选它（对比过其它方案后的结论）：
//    · **IOPaint**（原名 lama-cleaner）是这类需求的事实标准：自带 Web UI、
//      支持批量处理、支持视频去水印、pip 一条命令装完，单进程、资源占用小。
//      对"给图片去掉水印"这个具体用途，它是最贴合的工具。
//    · **ComfyUI** 更强（SD 工作流、可做的事多得多），但那是个通用节点引擎，
//      几个 GB 的模型 + 更复杂的交互；16GB 的机器能跑，但为了去水印
//      去装它属于用大锤敲钉子。适合以后有更复杂图像需求时再上。
//    · **PowerPaint** 效果更好（擦除同时保持背景语义），但它其实是
//      IOPaint 里的一个模型选项，不需要另装一个项目 —— 需要时换模型即可。
//    · **Stirling PDF**（市场里已有）能做 PDF/图片的基本处理，但**不含**
//      AI 修补，去水印做不了。
//
//  Apple Silicon 的现实（必须说清楚，否则用户会以为"装了就能用所有模型"）：
//    IOPaint 的模型对 MPS 的支持是**部分**的 —— LaMa 与 MI-GAN 能跑，
//    部分较重或较老的模型（MAT / ZITS / LDM 等）在 M1/M2 上会报
//    "不支持 mps"。所以这里默认用 LaMa（又快又够用），
//    并且把"设备回退到 CPU"这条路留着，避免一上来就失败。
//
//  ⚠️ 权重来自 GitHub，不是 HuggingFace（2026-09-16 真机踩过，务必看清）：
//    LaMa 的权重地址写在 iopaint/model/lama.py 里，是 **GitHub release**
//    （Sanster/models）。所以：
//      · plist 里的 HF_ENDPOINT 对**它完全无效** —— 以前只配了 HF_ENDPOINT
//        就以为"走镜像了"，实际每个字节都打在 GitHub 上（真机 lsof 可证：
//        进程连的是 185.199.109.133:443，国内实测 35~65KB/s，196MB 要 50 分钟+）；
//      · torch.hub 的 download_url_to_file **没有超时**，网络一挂就无限期停在
//        "正在等待服务就绪"，任务日志一行不动，用户看到的是"卡住了"。
//    所以现在由**面板先按镜像优先把权重下好**（见 ensureIOPaintWeight），
//    IOPaint 启动时发现缓存已存在就直接用、根本不联网。
//    HF_ENDPOINT 保留，是给"用户在界面里切换到 SD / PowerPaint 等
//    **确实来自 HuggingFace** 的模型"用的 —— 日志里会如实分开写。
// ============================================================================

const (
	iopaintLabel = "com.zizdog.iopaint"
	iopaintPort  = 8080
	// 默认模型：LaMa —— 在 Apple Silicon 上可用，速度快，去水印够用
	iopaintModel = "lama"
)

// LaMa 权重（默认模型）的确切来源与校验值。
//
// 三处必须与上游一致，改任何一处都要重新核对 iopaint 的 lama.py：
//   - URL 与文件名：iopaint/model/lama.py 的 LAMA_MODEL_URL
//   - MD5：同文件里的 LAMA_MODEL_MD5（**上游唯一公布的校验值**；
//     这里用 MD5 不是安全选择，而是"必须和上游一致"的完整性校验）
//   - 大小：上游 release 的 Content-Length（用于进度与"下完没有"的判断）
const (
	iopaintWeightFile = "big-lama.pt"
	iopaintWeightMD5  = "e3aa4aaa15225a33ec84f9f4bc47e500"
	iopaintWeightURL  = "https://github.com/Sanster/models/releases/download/add_big_lama/big-lama.pt"
	iopaintWeightSize = 205669692
	// iopaintWeightMirrorPath 是权重在镜像站上的**静态**路径。
	//
	// 为什么是静态文件、而不是走 /hf/ 那条按需缓存：/hf/ 反代的是 hf-mirror.com，
	// 覆盖不到 GitHub release；而这个权重恰恰来自 GitHub。
	// 镜像根就是 nginx 的 docroot，/apps/ 与 /zizpanel/ 都是根下的普通目录，
	// 所以这里放一个 models/ 目录即可 —— **不用改 NAS 容器配置、不用重启容器**。
	iopaintWeightMirrorPath = "models/iopaint/" + iopaintWeightFile
)

// 三个超时。它们的存在本身就是需求（2026-09-16 用户原话）：
// "模型下载/等待服务就绪必须有超时：超时后如实失败，不允许无限期 running"。
const (
	// iopaintWeightTimeout 是单个来源下载权重的总上限。
	// 30 分钟对 196MB 相当于平均 110KB/s —— 比国内直连 GitHub 快、比镜像慢得多，
	// 是"慢但在动就让它下完，彻底不动就早点失败"的折中（停滞另有看门狗）。
	iopaintWeightTimeout = 30 * time.Minute
	// iopaintStallTimeout 是"多久没有任何新字节"就判定卡死。
	// 90 秒：正常下载每秒都有数据，连续 90 秒零字节只可能是断了或上游不响应。
	iopaintStallTimeout = 90 * time.Second
	// iopaintReadyTimeout 是等端口就绪的上限。权重已被面板预取并校验，
	// 这里等的是 torch 把权重加载进内存（实测十几秒到一分钟）。
	iopaintReadyTimeout = 180 * time.Second
	// iopaintProgressEvery 是任务日志里下载进度的最小间隔。
	// 太快会把 4000 行的任务日志缓冲冲掉（真正的错误反而看不见）。
	iopaintProgressEvery = 10 * time.Second
)

type iopaintPaths struct {
	Root    string // ~/iopaint
	Venv    string
	Python  string
	Pip     string
	IOPaint string // venv 里的 iopaint 可执行文件
	OutLog  string
	ErrLog  string
	Plist   string
}

func (m *Manager) iopaintPaths() iopaintPaths {
	home := m.opt.UserHome
	if home == "" {
		home = "/Users/" + m.opt.UserName
	}
	root := filepath.Join(home, "iopaint")
	return iopaintPaths{
		Root:    root,
		Venv:    filepath.Join(root, ".venv"),
		Python:  filepath.Join(root, ".venv", "bin", "python"),
		Pip:     filepath.Join(root, ".venv", "bin", "pip"),
		IOPaint: filepath.Join(root, ".venv", "bin", "iopaint"),
		OutLog:  filepath.Join(root, "launchd.out.log"),
		ErrLog:  filepath.Join(root, "launchd.err.log"),
		Plist:   "/Library/LaunchDaemons/" + iopaintLabel + ".plist",
	}
}

// InstallIOPaint 部署 IOPaint 并注册为系统级后台服务。
func (m *Manager) InstallIOPaint(ctx context.Context, result *InstallResult) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("部署 IOPaint 需要以 root 运行")
	}
	if m.opt.UserName == "" {
		return fmt.Errorf("无法确定运行该服务的真实用户")
	}

	// ---- 0. 资源预检 ----
	// torch + 模型加起来不小，磁盘不够会在装到一半失败
	diskGB := freeDiskGB(m.opt.UserHome)
	result.step(ctx, fmt.Sprintf("环境检查：可用磁盘 %dGB", diskGB))
	if diskGB > 0 && diskGB < 8 {
		return fmt.Errorf("可用磁盘只有 %dGB，建议至少 8GB（torch 约 2GB + 模型）", diskGB)
	}

	p := m.iopaintPaths()

	// ---- 0b. 基础依赖：ffmpeg ----
	// IOPaint 的**视频**去水印路径要 ffmpeg 拆帧/合帧（目录的 Requires 里声明了）。
	// 图片路径不需要它，所以这里失败**不致命** —— 但必须如实说清"哪部分会不可用"，
	// 而不是让用户在处理视频时撞上一个没有解释的失败。
	m.AnnounceAppDependencies(ctx, "iopaint", result)
	if err := m.EnsureBaseDependencies(ctx, result); err != nil {
		result.Warning = appendBaseDepWarning(result.Warning,
			"基础依赖（ffmpeg）未就绪："+err.Error()+"；图片处理不受影响，视频去水印会失败")
		result.step(ctx, "警告："+result.Warning)
	}

	// ---- 1. Python 3.11 ----
	if !m.brewHas(ctx, qwenPythonVer) {
		result.step(ctx, "正在安装 "+qwenPythonVer)
		if _, err := m.brewInstall(ctx, result, 20*time.Minute, qwenPythonVer); err != nil {
			return fmt.Errorf("安装 %s 失败: %w", qwenPythonVer, err)
		}
	} else {
		result.step(ctx, qwenPythonVer+" 已安装，跳过")
	}
	py311 := filepath.Join(m.brewPrefix(), "opt", qwenPythonVer, "bin", "python3.11")

	// ---- 2. 虚拟环境 ----
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", p.Root, err)
	}
	// 递归改归属：只 chown 父目录的话子目录仍是 root 所有，
	// 用户身份建 venv 会 Permission denied（在 Qwen 那边踩过）
	_ = chownTree(m.opt.UserName, p.Root)
	if _, err := os.Stat(p.Python); err != nil {
		result.step(ctx, "正在创建 Python 虚拟环境")
		if out, err := m.runAsUser(ctx, 5*time.Minute, py311, "-m", "venv", p.Venv); err != nil {
			return fmt.Errorf("创建虚拟环境失败: %v（%s）", err, tailText(out, 300))
		}
	} else {
		result.step(ctx, "虚拟环境已存在，跳过创建")
	}
	_ = chownTree(m.opt.UserName, p.Root)

	// ---- 3. 安装 iopaint ----
	if err := m.pipInstallIOPaint(ctx, p, result); err != nil {
		return err
	}

	// ---- 4. 模型权重：面板先下好（NAS 优先），再让服务起来 ----
	//
	// 顺序很关键：**权重必须在服务启动之前就位**。原因见文件头 ——
	// IOPaint 自己去 GitHub 下权重时既没有进度、也没有超时，装完一半
	// 卡在"正在等待服务就绪"是它最典型的失败样子。
	device := m.pickIOPaintDevice(ctx, p, result)
	weightURL, err := m.ensureIOPaintWeight(ctx, result)
	if err != nil {
		return err
	}

	// ---- 4b. 注册为系统级服务 ----
	// 其它模型（SD / PowerPaint 等，用户自己在界面里切换才需要）确实来自
	// HuggingFace，这条端点单独如实写出来 —— 不要把它和 LaMa 的事混为一谈。
	hfEndpoint := m.qwenHFEndpoint(ctx)
	result.step(ctx, "其它模型（SD / PowerPaint 等，需在界面里切换）的 HuggingFace 端点："+hfEndpoint)
	plist := iopaintPlist(p, m.opt.UserName, device, hfEndpoint, weightURL)
	if err := os.WriteFile(p.Plist+".tmp", []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return fmt.Errorf("安装 plist 失败: %w", err)
	}
	if err := m.bootstrapService(ctx, iopaintLabel, p.Plist); err != nil {
		return err
	}
	result.Steps = append(result.Steps,
		"已注册为系统级后台服务（开机自启、不依赖用户登录）")

	// ---- 5. 验证 ----
	// 权重已在第 4 步下好并校验过，这里等的是 torch 加载权重（通常十几秒）。
	result.step(ctx, "正在等待服务就绪（权重已预取并校验，这里是加载时间）")
	if err := m.waitIOPaintReady(ctx, p, result); err != nil {
		return err
	}

	host := m.primaryIP()
	result.Address = host

	if err := m.RegisterInstalledService(ctx, iopaintLabel, "IOPaint（图片去水印）", "🖼️", "tool", iopaintPort); err != nil {
		result.step(ctx, "（自动登记到服务管理失败："+err.Error()+"）")
	}

	result.Steps = append(result.Steps,
		fmt.Sprintf("IOPaint 已就绪：http://%s:%d", host, iopaintPort),
		"",
		"用法：打开上面的地址 → 上传图片 → 涂抹要去掉的水印/物体 → 点「擦除」",
		"模型："+iopaintModel+"（Apple Silicon 上支持 MPS 加速）",
		"",
		"想换更强的模型（如 PowerPaint，擦除同时保持背景语义）：",
		"  在界面右上角切换模型即可，首次使用会下载对应权重（较大）",
		"  ⚠️ 这些额外权重**目前只有公网源**（GitHub / HuggingFace），镜像站上还没有，",
		"     国内第一次切换会很慢；默认的 "+iopaintModel+" 已经预置好（走 NAS 镜像下好并校验过）",
		"  注意：MAT / ZITS / LDM 等模型在 M1/M2 上不支持 MPS，会失败",
	)
	return nil
}

// pipInstallIOPaint 装 pip 与 iopaint。
func (m *Manager) pipInstallIOPaint(ctx context.Context, p iopaintPaths, result *InstallResult) error {
	// pip 索引按"NAS 优先、探不通回落清华"现算（见 pypi_mirror.go）；
	// 镜像路径会自动带上 --timeout 180 --retries 3，因为 NAS 的按需缓存
	// 是"整份落盘后才回第一个字节"，冷缓存拉大轮子会超过 pip 默认的 15 秒。
	pipIdx, err := m.pipMirrorArgs(ctx, result)
	if err != nil {
		return err
	}
	if out, err := m.runAsUser(ctx, 5*time.Minute, p.Pip,
		append([]string{"install", "-U", "pip"}, pipIdx...)...); err != nil {
		return fmt.Errorf("升级 pip 失败: %v（%s）", err, tailText(out, 300))
	}
	out, _ := m.runAsUser(ctx, time.Minute, p.Pip, "list")
	if strings.Contains(out, "iopaint") || strings.Contains(out, "IOPaint") {
		result.step(ctx, "iopaint 已安装，跳过")
		return nil
	}
	// iopaint 会拉 torch —— 这是本流程里最大的一块（约 1~2GB），
	// 国内直连 pypi 很慢，所以必须走镜像；超时给到 40 分钟。
	result.Steps = append(result.Steps,
		"正在安装 iopaint（含 torch，约 1~2GB，走配置的 pip 索引，请耐心等待）")
	if out, err := m.runAsUser(ctx, 40*time.Minute, p.Pip,
		append([]string{"install", "iopaint"}, pipIdx...)...); err != nil {
		return fmt.Errorf("安装 iopaint 失败: %v（%s）", err, tailText(out, 500))
	}
	result.step(ctx, "iopaint 安装完成")
	return nil
}

// pickIOPaintDevice 选推理设备：Apple Silicon 优先 MPS，不可用则回退 CPU。
//
// 为什么要探测而不是写死 mps：IOPaint 的**部分模型**在 M1/M2 上不支持 MPS
// （实测与上游 issue 都确认过）。写死 mps 会让服务直接起不来；
// 探测失败就回退 CPU —— 慢一点但能用，比"完全不能用"好。
func (m *Manager) pickIOPaintDevice(ctx context.Context, p iopaintPaths, result *InstallResult) string {
	script := "import torch;print('mps' if torch.backends.mps.is_available() else 'cpu')"
	out, err := m.runAsUser(ctx, 2*time.Minute, p.Python, "-c", script)
	dev := strings.TrimSpace(out)
	if err != nil || (dev != "mps" && dev != "cpu") {
		result.step(ctx, "未能探测到 MPS，回退到 CPU（较慢）")
		return "cpu"
	}
	if dev == "mps" {
		result.step(ctx, "已启用 Apple Silicon MPS 加速")
	} else {
		result.step(ctx, "MPS 不可用，使用 CPU（较慢）")
	}
	return dev
}

// iopaintPlist 生成系统级 LaunchDaemon 定义。
//
// 三个来源参数必须分清（2026-09-16 真机踩过"日志说谎"的坑）：
//   - weightURL：**默认模型 LaMa 的权重地址**。它来自 **GitHub release**
//     （Sanster/models），不是 HuggingFace —— 面板已按"NAS 优先"算好并写进来。
//     IOPaint 的 iopaint/model/lama.py 会读 LAMA_MODEL_URL/LAMA_MODEL_MD5
//     （这是上游**官方**读的环境变量，不需要也不会去 patch 它的 venv）。
//     正常情况下权重已被面板预取到 torch 缓存里，服务启动时直接命中、不联网；
//     这两个变量是"缓存万一丢了/删了"时的兜底来源，以及必须的一致性校验。
//   - hfEndpoint：**其它模型**（SD / PowerPaint 等，用户在界面里切换才需要）
//     的 HuggingFace 端点，由调用方按"NAS 优先"现算（m.qwenHFEndpoint(ctx)）。
//     用的是 Qwen 那套端点选择器：它的**名字**是历史原因，机制是通用的
//     （探 <mirror>/hf/，通就用 NAS，不通回 hf-mirror.com），复用比再造一个稳。
func iopaintPlist(p iopaintPaths, user, device, hfEndpoint, weightURL string) string {
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
        <string>%s</string>
        <string>start</string>
        <string>--model=%s</string>
        <string>--device=%s</string>
        <string>--port=%d</string>
        <!-- 0.0.0.0：面板与其它设备要能打开它的 Web UI。
             这是本机自用工具，没有账号体系，所以不要映射到公网。 -->
        <string>--host=0.0.0.0</string>
    </array>
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
        <!-- 默认模型 LaMa 的权重：**GitHub release**，不是 HuggingFace。
             面板已按"NAS 镜像优先 → GitHub 原址"探通并写在这里（见任务日志
             "模型权重来源"那几行）。MD5 是上游公布的唯一校验值：
             对不上时 IOPaint 会删掉文件并退出，而不是拿错权重算出错图。 -->
        <key>LAMA_MODEL_URL</key>
        <string>%s</string>
        <key>LAMA_MODEL_MD5</key>
        <string>%s</string>
        <!-- 其它模型（SD / PowerPaint 等）走 HuggingFace；国内直连不可用，走镜像 -->
        <key>HF_ENDPOINT</key>
        <string>%s</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, iopaintLabel, user, p.IOPaint, iopaintModel, device, iopaintPort,
		p.Root, weightURL, iopaintWeightMD5, hfEndpoint, p.OutLog, p.ErrLog)
}

// ---------------------------------------------------------------------------
//  模型权重：来源选择 + 带进度/超时/校验的下载
// ---------------------------------------------------------------------------

// fetchProgressFunc 报告一次下载进度（字节；total <= 0 表示上游没给长度）。
type fetchProgressFunc func(got, total int64)

// weightSource 是权重的一个候选来源（Label 用于任务日志如实标注"这次走的哪个"）。
type weightSource struct {
	URL   string
	Label string
}

// iopaintWeightPath 返回 IOPaint（torch.hub）期望的权重落点。
//
// 为什么是这个目录：iopaint/helper.py 的 get_cache_path_by_url 用
// `torch.hub.get_dir()/checkpoints/<URL 里的文件名>`，而 torch.hub.get_dir()
// 先看 TORCH_HOME、再看 XDG_CACHE_HOME，都没有才是 ~/.cache/torch/hub。
// 面板生成的 plist **不设**这两个变量，所以这里算出来的路径与服务进程里一致。
// ⚠️ 以后若给 plist 加 TORCH_HOME / XDG_CACHE_HOME，必须同步改这里，
// 否则面板会把权重下到一个服务根本不看的目录里（表现就是"又去 GitHub 下了"）。
func (m *Manager) iopaintWeightPath() string {
	home := m.opt.UserHome
	if home == "" {
		home = "/Users/" + m.opt.UserName
	}
	return filepath.Join(home, ".cache", "torch", "hub", "checkpoints", iopaintWeightFile)
}

// iopaintWeightTimeoutDur / iopaintStallTimeoutDur 取生效的超时（测试可缩短）。
func (m *Manager) iopaintWeightTimeoutDur() time.Duration {
	if m.iopaintWeightTimeoutOverride > 0 {
		return m.iopaintWeightTimeoutOverride
	}
	return iopaintWeightTimeout
}

func (m *Manager) iopaintStallTimeoutDur() time.Duration {
	if m.iopaintStallTimeoutOverride > 0 {
		return m.iopaintStallTimeoutOverride
	}
	return iopaintStallTimeout
}

// iopaintWeightSources 决定这次从哪儿拿权重，并把**真实来源**写进任务日志。
//
// 为什么必须"探通才算数"（而不是无脑写一行 NAS 地址）：镜像站的 /hf/ 反代的是
// hf-mirror.com，覆盖不到 GitHub release —— 而这个权重恰恰来自 GitHub。
// 真机事故（2026-09-16 mini）就是：日志写着"走 NAS 镜像"，进程却在直连 GitHub。
// 所以这里探的是**静态镜像路径** <base>/models/iopaint/big-lama.pt
// （与 /apps/、/zizpanel/ 一样是镜像根下的普通目录，不用动 NAS 容器配置），
// 探到了才把镜像排第一并如实标注；探不到就明说"镜像上没有，回落 GitHub"。
func (m *Manager) iopaintWeightSources(ctx context.Context, result *InstallResult) []weightSource {
	upstream := weightSource{URL: iopaintWeightURL, Label: "GitHub release（Sanster/models）"}
	if !m.MirrorEnabled() {
		if result != nil {
			result.step(ctx, "未启用镜像站，模型权重来源："+iopaintWeightURL)
		}
		return []weightSource{upstream}
	}
	mirrorURL := m.mirrorSubPath(iopaintWeightMirrorPath)
	size, err := m.probeMirrorFile(ctx, mirrorURL)
	if err != nil {
		if result != nil {
			result.step(ctx, fmt.Sprintf(
				"NAS 镜像上没有这个模型（%v），权重来源回落到 GitHub 原址：%s", err, iopaintWeightURL))
		}
		return []weightSource{upstream}
	}
	if result != nil {
		sizeText := "大小未知"
		if size > 0 {
			sizeText = humanBytes(size)
		}
		result.step(ctx, fmt.Sprintf("模型权重来源：NAS 镜像 %s（已探通，%s）", mirrorURL, sizeText))
	}
	return []weightSource{{URL: mirrorURL, Label: "NAS 镜像"}, upstream}
}

// ensureIOPaintWeight 把 LaMa 权重在**服务启动之前**准备好，返回"该写进 plist 的来源"。
//
// 为什么必须由面板先下（而不是让 IOPaint 自己去下）：
//
//	· 这个权重来自 **GitHub release**，plist 里的 HF_ENDPOINT 对它完全无效；
//	  真机实测（2026-09-16 mini）直连 GitHub 只有 35~65KB/s，196MB 要 50 分钟以上；
//	· torch.hub 的 download_url_to_file **没有超时**：网络一断就无限期停在那里，
//	  任务日志一行都不动（用户看到的就是"卡住了、没进度"）；
//	· 面板自己下才有进度、有超时、有 MD5 校验；镜像上的静态副本实测 10MB/s，
//	  19.5 秒下完（同一份文件、同一台机器）。
//
// IOPaint 侧**不需要改任何代码**：它的 helper.download_model 发现缓存文件已存在
// 就直接用（`if not os.path.exists(cached_file)` 才下载），也就不再碰网络。
func (m *Manager) ensureIOPaintWeight(ctx context.Context, result *InstallResult) (string, error) {
	dest := m.iopaintWeightPath()
	srcs := m.iopaintWeightSources(ctx, result)

	// torch 的缓存目录要能被**真实用户**写：服务是以该用户身份跑的，
	// 万一日后缓存丢了，它还得能自己往里写（否则会以 Permission denied 起不来）。
	if err := ensureUserWritableDir(m.opt.UserName, m.opt.UserHome, filepath.Dir(dest)); err != nil {
		// 只告警不失败：文件下载本身仍能进行（面板是 root），
		// 但必须让用户看见 —— 这会影响"服务自己补下模型"的能力。
		if result != nil {
			result.step(ctx, "警告：模型缓存目录属主未能改给 "+m.opt.UserName+"："+err.Error())
		}
	}

	if err := m.ensureWeightFile(ctx, dest, iopaintWeightMD5, srcs, result); err != nil {
		return srcs[0].URL, err
	}
	return srcs[0].URL, nil
}

// ensureWeightFile 把权重准备到 dest：已存在且 MD5 相符就跳过；否则按 srcs 顺序
// 逐个下载，每下一个都校验 MD5；**全部失败返回可读错误**（绝不让服务带着半个
// 权重或坏权重启动 —— 坏权重比没有权重更糟：服务能起来，擦出来的图是错的）。
func (m *Manager) ensureWeightFile(ctx context.Context, dest, wantMD5 string,
	srcs []weightSource, result *InstallResult) error {

	if got, err := md5OfFile(dest); err == nil && strings.EqualFold(got, wantMD5) {
		if result != nil {
			result.step(ctx, fmt.Sprintf("模型权重已存在且 MD5 校验通过，跳过下载：%s（%s）",
				dest, humanBytes(fileSizeOrZero(dest))))
		}
		return nil
	}

	var lastErr error
	for i, s := range srcs {
		label := s.Label
		if i > 0 {
			label += "，回落"
		}
		if result != nil {
			result.step(ctx, fmt.Sprintf("正在下载模型权重 %s ← %s", iopaintWeightFile, label))
		}
		started := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, m.iopaintWeightTimeoutDur())
		err := m.fetchFile(attemptCtx, s.URL, dest, func(got, total int64) {
			if result == nil {
				return
			}
			progress := ""
			if total > 0 {
				progress = fmt.Sprintf("%.0f%%（%s / %s）",
					float64(got)*100/float64(total), humanBytes(got), humanBytes(total))
			} else {
				progress = humanBytes(got)
			}
			result.step(ctx, fmt.Sprintf("下载中：%s，已用 %.0f 秒", progress, time.Since(started).Seconds()))
		})
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("%s 下载失败：%w", label, err)
			if result != nil {
				result.step(ctx, "该来源失败："+lastErr.Error())
			}
			continue
		}

		got, merr := md5OfFile(dest)
		if merr != nil {
			lastErr = fmt.Errorf("%s：下载完成但算不出 MD5：%w", label, merr)
			_ = os.Remove(dest) // 算不出来就不能留下，免得被当成好文件
			continue
		}
		if !strings.EqualFold(got, wantMD5) {
			lastErr = fmt.Errorf("%s 的文件 MD5 = %s，与上游公布的 %s 不一致", label, got, wantMD5)
			// **必须删掉**：留着它，服务会带着错权重跑起来，输出错图且毫无提示。
			_ = os.Remove(dest)
			if result != nil {
				result.step(ctx, "校验不通过，已删除损坏文件："+lastErr.Error())
			}
			continue
		}
		if result != nil {
			result.step(ctx, fmt.Sprintf("模型权重就绪：%s（%s，MD5 校验通过，来源：%s，用时 %.0f 秒）",
				dest, humanBytes(fileSizeOrZero(dest)), label, time.Since(started).Seconds()))
		}
		return nil
	}

	return fmt.Errorf("模型权重下载失败（已试过 %d 个来源，最后一次：%v）。"+
		"请检查网络后重新点安装；也可以在网络可达的机器上手动下载 %s 放到 %s 再重试",
		len(srcs), lastErr, iopaintWeightURL, dest)
}

// fetchFile 是下载动作的生产实现（测试用 iopaintFetchOverride 替换，单测不许联网）。
func (m *Manager) fetchFile(ctx context.Context, url, dest string, onProgress fetchProgressFunc) error {
	if m.iopaintFetchOverride != nil {
		return m.iopaintFetchOverride(ctx, url, dest, onProgress)
	}
	return fetchToFile(ctx, &http.Client{}, url, dest, m.iopaintStallTimeoutDur(), onProgress)
}

// fetchToFile 把 url 下载到 dest，带"停滞看门狗"与进度回调。
//
// 语义与仓库里下载 release 包那条 curl 路线一致（binary_release.go）：
// 先写 <dest>.part，成功才改名 —— 失败绝不留下半个文件冒充好文件。
// 用 Go 而不是 curl 的三个理由：
//  1. 停滞判定要能被单测注入并验证（"不动了就失败"不能只靠人工等 30 秒）；
//  2. 进度要能进任务日志（curl 的进度条在 stderr 是管道时会被它自己关掉，
//     这正是"面板看不见模型在下"的原因之一）；
//  3. 下载完要就地算 MD5 并决定"留还是删"。
func fetchToFile(ctx context.Context, client *http.Client, url, dest string, stall time.Duration, onProgress fetchProgressFunc) error {
	if stall <= 0 {
		stall = iopaintStallTimeout
	}
	// 看门狗挂在自己的 ctx 上：取消它会让 transport 关掉连接，
	// 从而**中断阻塞中的 Read** —— 只 return 是不行的，io 会一直等在那里。
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()

	req, err := http.NewRequestWithContext(watchCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("下载地址不合法（%s）：%w", url, err)
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("连接失败（%s）：%w", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d（%s）", res.StatusCode, url)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败：%w", filepath.Dir(dest), err)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("创建文件 %s 失败：%w", tmp, err)
	}
	// 失败路径统一清掉 .part；成功路径会先改名，这里再删一次不存在也无害。
	defer func() { _ = f.Close(); _ = os.Remove(tmp) }()

	// 每收到数据就喂一次看门狗；stall 时间内一次都没喂 → 判定停滞。
	tick := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(stall)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
				cancelWatch() // 触发上面的"中断 Read"
				return
			case <-tick:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(stall)
			}
		}
	}()

	var got int64
	total := res.ContentLength
	buf := make([]byte, 256<<10)
	lastEmit := time.Now().Add(-iopaintProgressEvery) // 第一次读就报一次进度
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return fmt.Errorf("写入 %s 失败（已收 %s）：%w", tmp, humanBytes(got), werr)
			}
			got += int64(n)
			select {
			case tick <- struct{}{}:
			default:
			}
			if onProgress != nil && time.Since(lastEmit) >= iopaintProgressEvery {
				lastEmit = time.Now()
				onProgress(got, total)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			// 分清"任务/请求被取消"、"看门狗判定停滞"和"真的读失败" ——
			// 用户看到的必须是他能动手解决的那句话。
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if watchCtx.Err() != nil {
				return fmt.Errorf("下载停滞：%s 内没有收到任何新数据（已收 %s）", stall, humanBytes(got))
			}
			if errors.Is(rerr, io.ErrUnexpectedEOF) {
				return fmt.Errorf("下载不完整：连接提前结束（已收 %s / %s）", humanBytes(got), humanBytes(total))
			}
			return fmt.Errorf("读取响应失败（已收 %s）：%w", humanBytes(got), rerr)
		}
	}
	if total > 0 && got < total {
		return fmt.Errorf("下载不完整：只收到 %s / %s", humanBytes(got), humanBytes(total))
	}
	if got == 0 {
		return fmt.Errorf("下载内容为空（%s）", url)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭 %s 失败：%w", tmp, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("保存到 %s 失败：%w", dest, err)
	}
	return nil
}

// md5OfFile 算文件的 MD5（文件不存在/读不动时返回错误）。
//
// 这里用 MD5 不是"安全选择"，而是**必须与上游一致**：IOPaint 公布的
// LAMA_MODEL_MD5 就是 MD5，我们用它做完整性校验（坏包/半包/被替换的包）。
func md5OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileSizeOrZero 取文件大小（取不到按 0，只用于日志）。
func fileSizeOrZero(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// ensureUserWritableDir 创建目录（含父级）并把**从家目录往下新建的那几层**
// 交给真实用户。
//
// 为什么必须逐级改：面板以 root 建目录，属主是 root；而 IOPaint 以真实用户
// 身份运行 —— 只改最内层没用，父级进不去一样写不了。只向上走到家目录为止，
// 不碰家目录以上的任何东西（也不递归进 ~/.cache 里已有的其它内容）。
func ensureUserWritableDir(user, home, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	user = strings.TrimSpace(user)
	if user == "" || user == "root" {
		return nil
	}
	home = strings.TrimSpace(home)
	for p := dir; ; p = filepath.Dir(p) {
		if err := chownTo(user, p); err != nil {
			return fmt.Errorf("改 %s 属主失败：%w", p, err)
		}
		if home == "" || p == home || p == "/" || filepath.Dir(p) == p {
			break
		}
	}
	return nil
}

// waitIOPaintReady 等服务端口就绪。**超时返回错误，不是警告。**
//
// 为什么必须是错误（2026-09-16 真机教训）：原来这里超时只写一条 Warning
// 然后 return nil，任务于是显示"任务完成 ✅" —— 而端口根本没起来、IOPaint
// 用不了。用户拿到的是一个"安装成功"的空壳，这属于最忌讳的**谎报成功**
// （真机上就是这么发生的：16:49:16 任务 succeeded，服务当时还在慢慢下权重）。
// 现在改成如实失败，并把"权重已预取、日志在哪、文件应该在哪"写进错误里，
// 用户与我们都一眼能判断下一步做什么。
func (m *Manager) waitIOPaintReady(ctx context.Context, p iopaintPaths, result *InstallResult) error {
	wait := waitPort
	if m.iopaintWaitPortOverride != nil {
		wait = m.iopaintWaitPortOverride
	}
	if wait(ctx, iopaintPort, iopaintReadyTimeout) {
		result.step(ctx, fmt.Sprintf("IOPaint 已就绪，监听 %d 端口", iopaintPort))
		return nil
	}
	return fmt.Errorf("%d 秒内 %d 端口没有监听，IOPaint 没有起来。"+
		"注意：模型权重已由面板预取并校验（%s），所以这**不是**“还在下载模型”。"+
		"请查看服务日志 %s 里的报错；修好后重新点安装即可",
		int(iopaintReadyTimeout.Seconds()), iopaintPort, m.iopaintWeightPath(), p.ErrLog)
}

package services

import (
	"context"
	"fmt"
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
// ============================================================================

const (
	iopaintLabel = "com.zizdog.iopaint"
	iopaintPort  = 8080
	// 默认模型：LaMa —— 在 Apple Silicon 上可用，速度快，去水印够用
	iopaintModel = "lama"
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

	// ---- 1. Python 3.11 ----
	if !m.brewHas(ctx, qwenPythonVer) {
		result.step(ctx, "正在安装 "+qwenPythonVer)
		if _, err := m.brewRun(ctx, 20*time.Minute, "install", qwenPythonVer); err != nil {
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

	// ---- 4. 注册为系统级服务 ----
	device := m.pickIOPaintDevice(ctx, p, result)
	plist := iopaintPlist(p, m.opt.UserName, device)
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
	// 首次启动会下载模型（LaMa 约 200MB），所以给足时间
	result.step(ctx, "正在等待服务就绪（首次会下载模型，约 200MB）")
	if !waitPort(ctx, iopaintPort, 180*time.Second) {
		result.Warning = fmt.Sprintf("服务已注册，但 180 秒内 %d 端口未监听。请看日志：%s",
			iopaintPort, p.ErrLog)
		result.step(ctx, "警告："+result.Warning)
		return nil
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
		"  注意：MAT / ZITS / LDM 等模型在 M1/M2 上不支持 MPS，会失败",
	)
	return nil
}

// pipInstallIOPaint 装 pip 与 iopaint。
func (m *Manager) pipInstallIOPaint(ctx context.Context, p iopaintPaths, result *InstallResult) error {
	if out, err := m.runAsUser(ctx, 5*time.Minute, p.Pip, "install", "-U", "pip", "-i", qwenPipMirror); err != nil {
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
		"正在安装 iopaint（含 torch，约 1~2GB，走清华源，请耐心等待）")
	if out, err := m.runAsUser(ctx, 40*time.Minute, p.Pip, "install", "iopaint", "-i", qwenPipMirror); err != nil {
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
func iopaintPlist(p iopaintPaths, user, device string) string {
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
        <!-- 模型权重从 HuggingFace 拉，国内直连不可用，走镜像 -->
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
		p.Root, qwenHFMirror, p.OutLog, p.ErrLog)
}

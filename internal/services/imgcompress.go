package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
//  图片压缩（libvips）的安装 / 卸载
//
//  历史：这个条目原本是**纯能力型应用** —— 装的是 libvips 的 `vips` 命令行，
//  没有守护进程、没有端口、没有网页界面（界面是面板自己的「文件管理 →
//  🖼️ 图片压缩」）。所以它走自研安装器而不是通用 brew 流程
//  （`brew services start vips` 会去启动一个根本不存在的 service 定义，
//  留下"已安装但启动失败"的假警告）。
//
//  2026-09-23 用户要求："给它加一个 webui 通过端口和别名调用"。
//  于是它现在**同时**是：
//    · 一个命令行引擎（vips，RuntimePath 判据不变）；
//    · 一个由面板托管的常驻网页界面（本文件注册的 launchd 服务
//      com.zizdog.imgcompress，默认 127.0.0.1:8890，见 cmd/zizpanel 的
//      `imgcompress-serve` 子命令与 internal/web/imgcompress_web.go）。
//  两条入口都通：直连端口 http://127.0.0.1:8890/ ，以及经面板的别名
//  http://<主机>/imgcompress/ （目录条目的 UI.Slug，由 internal/appproxy 反代）。
//
//  为什么服务就是面板自己的二进制（`zizpanel imgcompress-serve`）：
//    · 不引入第二个可执行文件、不需要构建步骤、内嵌前端随二进制走；
//    · vips 只是被这个进程 spawn 出来的 CLI，面板进程本身仍是纯 Go、无 CGO。
//
//  为什么不用 govips（用户参考文档给的 CGO 绑定）：见 internal/imgopt 顶部。
//  一句话：面板是纯 Go 单二进制，运行期不能把 libvips 的动态库绑到自己身上。
// ============================================================================

// ImgCompressFormula 是图片压缩引擎的 Homebrew formula。
//
// 只在这里定义一次：安装、卸载、引擎探测（web 层）都引用它 ——
// 三处各写一遍 strings 必然漂移（本项目为这类漂移踩过坑）。
const ImgCompressFormula = "vips"

// ImgCompressBinName 是引擎的命令名。
const ImgCompressBinName = "vips"

const (
	// ImgCompressLabel 是面板托管的「图片压缩网页界面」launchd 标签。
	ImgCompressLabel = "com.zizdog.imgcompress"
	// ImgCompressPort 是网页界面默认监听端口（只绑 127.0.0.1）。
	//
	// 8890 与面板自研的两个 Python 服务（8880 Qwen3 TTS / 8899 音色接收端）
	// 相邻但不冲突，且不在目录里任何其它条目的占用清单里
	// （TestCatalogPortsAreUnique 会锁住唯一性）。
	ImgCompressPort = 8890
)

// ImgCompressHealthURL 是网页界面的健康检查地址（目录里的默认端口）。
//
// 它返回 {"ok":true,...} 时才算真的好 —— 服务进程活着但 vips 不在时
// 必须报 ok:false/503，这样市场与服务页会如实标红（绝不绿灯）。
func ImgCompressHealthURL() string {
	return imgCompressHealthURLFor(ImgCompressPort)
}

// imgCompressHealthURLFor 按实际端口拼健康检查地址。
//
// 单独一个函数是为了不让"目录里的端口"与"安装时真正用的端口"漂移：
// waitImgCompressReady 拿到的是 app.WebPort()，而不是写死的常量。
func imgCompressHealthURLFor(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
}

// ImgCompressBin 返回这台机器上引擎二进制的路径（按 brew 前缀推导）。
func (m *Manager) ImgCompressBin() string {
	return filepath.Join(m.brewPrefix(), "bin", ImgCompressBinName)
}

// imgCompressSlug 返回面板别名（目录条目的 UI.Slug），装完提示里要用。
//
// 兜底写 "imgcompress"：目录条目一定会声明它（有测试锁住 slug/端口存在），
// 这里兜底只是不让一个错误的目录数据把安装流程本身弄挂。
func imgCompressSlug(app App) string {
	if app.UI != nil && strings.TrimSpace(app.UI.Slug) != "" {
		return app.UI.Slug
	}
	return "imgcompress"
}

// ---------------------------------------------------------------------------
//  仅测试用的注入点
//
//  安装/卸载这一段会写 /Library/LaunchDaemons、调 launchctl bootstrap，
//  单测既不可能真等、也绝不允许碰真实 launchd（AGENTS 第三节）。
//  做成包级变量在生产路径永远是默认实现，测试替换后恢复。
//  与 ready.go 顶部的注入点是同一套做法。
// ---------------------------------------------------------------------------

var (
	// imgCompressExecutable 返回面板二进制自身的路径（launchd 要重新执行它）。
	imgCompressExecutable = os.Executable
	// imgCompressPlistPath 返回服务 plist 的绝对路径。
	imgCompressPlistPath = func() string { return SystemDaemonPlistPath(ImgCompressLabel) }
	// imgCompressLaunch 装载 launchd 服务（默认 bootout + bootstrap + 等待）。
	imgCompressLaunch = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.bootstrapService(ctx, label, plist)
	}
	// imgCompressStop 停止服务、删 plist、删面板记录。
	imgCompressStop = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.removeService(ctx, label, plist)
	}
	// imgCompressHealthy 打一次 /healthz 并等待 ok:true。
	imgCompressHealthy = func(ctx context.Context, url string, timeout time.Duration) bool {
		return readyWaitJSONBool(ctx, url, "ok", true, timeout)
	}
)

// imgCompressPaths 是这套服务的目录约定。
type imgCompressPaths struct {
	Plist  string
	OutLog string
	ErrLog string
}

func (m *Manager) imgCompressPaths() imgCompressPaths {
	home := m.opt.UserHome
	if home == "" && m.opt.UserName != "" {
		home = "/Users/" + m.opt.UserName
	}
	logDir := filepath.Join(home, "Library", "Logs")
	return imgCompressPaths{
		Plist:  imgCompressPlistPath(),
		OutLog: filepath.Join(logDir, "zizpanel-imgcompress.out.log"),
		ErrLog: filepath.Join(logDir, "zizpanel-imgcompress.err.log"),
	}
}

// imgCompressListen 返回服务应当监听的地址。
//
// 只绑回环：界面里能做的事（读用户上传的图片、spawn vips）对局域网没有意义，
// 也不该在用户没明确要的情况下扩大暴露面。要给别人用就走面板的 /imgcompress/
// 别名（那条路要求先登录面板）。
func imgCompressListen(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// imgCompressPlist 生成 LaunchDaemon 定义。
//
// 以**真实用户**身份运行：它要写临时文件、spawn 用户 PATH 下的 vips；
// 以 root 跑会让输出文件属主变成 root，用户反而拿不走。
func imgCompressPlist(panelBin string, port int, brewPrefix, user, outLog, errLog string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <!-- 以真实用户运行：上传的临时文件与输出文件的属主要归该用户 -->
    <key>UserName</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>imgcompress-serve</string>
        <string>--listen</string>
        <string>%s</string>
        <string>--brew-prefix</string>
        <string>%s</string>
    </array>
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
`, ImgCompressLabel, user, panelBin, imgCompressListen(port), brewPrefix, outLog, errLog)
}

// InstallImageCompressor 安装 libvips（幂等），并注册/启动面板托管的网页界面。
//
// 装完**必须复核**：二进制存在 + `vips --version` 真的能报出版本，
// 界面服务能起来且 /healthz 真的报 ok:true。
// 只跑完 brew 就报成功是不够的（brew 退出码 0 而库缺依赖的情况真的存在），
// 用户点「打开」时才发现不能用，那就是谎报。
func (m *Manager) InstallImageCompressor(ctx context.Context, app App, result *InstallResult) error {
	formula := app.BrewFormula
	if formula == "" {
		formula = ImgCompressFormula
	}
	if result != nil {
		result.App = app.ID
	}
	if m.brewHas(ctx, formula) {
		if result != nil {
			result.step(ctx, formula+" 已经装好了（Homebrew 里已有），跳过安装")
		}
	} else {
		if result != nil {
			result.step(ctx, "正在 brew install "+formula+"（原生 arm64 包，不需要 Docker/Node）")
		}
		// 必须走 brewInstall（**多源兜底**）而不是 brewRun：
		// brewRun 只用"当前镜像"这一个源，而镜像站经常缺某个瓶文件
		//（2026-09-18 实测：aliyun 镜像上 gcc-16.2.0.arm64_sequoia.bottle.1.tar.gz 是 404，
		//  而 vips 依赖 gcc 的 OpenMP 运行时 → 单源直接失败）。
		// brewInstall 会清掉坏缓存并依次换源，最后回落到官方源。
		if _, err := m.brewInstall(ctx, result, 30*time.Minute, formula); err != nil {
			return fmt.Errorf("安装 %s 失败: %w", formula, err)
		}
	}
	bin := m.ImgCompressBin()
	ver, err := m.imgCompressVersion(ctx, bin)
	if err != nil {
		return err
	}
	if result != nil {
		result.step(ctx, "引擎已就绪："+bin+"（"+ver+"）")
	}
	if err := m.installImgCompressService(ctx, app, result); err != nil {
		return err
	}
	port := app.WebPort()
	if port <= 0 {
		port = ImgCompressPort
	}
	if result != nil {
		result.Steps = append(result.Steps,
			"网页界面：直连 http://127.0.0.1:"+fmt.Sprint(port)+
				"/ ，或从「应用市场 → 图片压缩」点「打开」走面板别名 /"+
				imgCompressSlug(app)+"/（要求先登录面板）")
		result.Steps = append(result.Steps,
			"用法：到「文件管理」选中目录 → 点工具条上的「🖼️ 图片压缩」"+
				"（可调质量 / 最长边 / 输出格式；默认另存为 xxx.min.<ext>，不动原文件）")
	}
	return nil
}

// installImgCompressService 写系统级 plist、装载、登记、等健康。
func (m *Manager) installImgCompressService(ctx context.Context, app App, result *InstallResult) error {
	if strings.TrimSpace(m.opt.UserName) == "" {
		return fmt.Errorf("无法确定运行图片压缩网页界面的真实用户（UserName 为空）")
	}
	panelBin, err := imgCompressExecutable()
	if err != nil {
		return fmt.Errorf("找不到面板自身的可执行文件路径（launchd 要用它启动界面）: %w", err)
	}
	if strings.TrimSpace(panelBin) == "" {
		return fmt.Errorf("面板自身的可执行文件路径为空，无法注册图片压缩网页界面")
	}
	p := m.imgCompressPaths()
	if strings.TrimSpace(p.Plist) == "" {
		return fmt.Errorf("图片压缩网页界面的 plist 路径为空")
	}
	port := app.WebPort()
	if port <= 0 {
		port = ImgCompressPort
	}
	// 日志目录必须存在（launchd 会直接打开 StandardOutPath，目录不在会让作业起不来）。
	if err := os.MkdirAll(filepath.Dir(p.OutLog), 0o755); err != nil {
		return fmt.Errorf("创建日志目录 %s 失败: %w", filepath.Dir(p.OutLog), err)
	}
	plist := imgCompressPlist(panelBin, port, m.brewPrefix(), m.opt.UserName, p.OutLog, p.ErrLog)
	if err := os.WriteFile(p.Plist+".tmp", []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 plist %s 失败（面板需要以 root 运行）: %w", p.Plist, err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return fmt.Errorf("安装 plist %s 失败: %w", p.Plist, err)
	}
	if result != nil {
		result.step(ctx, "正在注册并启动网页界面服务 "+ImgCompressLabel+
			"（"+imgCompressListen(port)+"）")
	}
	if err := imgCompressLaunch(m, ctx, ImgCompressLabel, p.Plist); err != nil {
		return fmt.Errorf("启动图片压缩网页界面失败: %w", err)
	}
	if err := m.RegisterInstalledService(ctx, ImgCompressLabel, app.Name, app.Icon, app.Category, port); err != nil {
		// 登记失败不该把"界面已经起来"报成安装失败，但必须如实留下警告。
		if result != nil {
			result.step(ctx, "警告：界面已启动，但登记进「服务管理」失败："+err.Error()+
				"（可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
		}
	}
	if result != nil {
		result.Steps = append(result.Steps,
			"已注册为系统级后台服务（开机自启、不依赖用户登录）")
	}
	return m.waitImgCompressReady(ctx, p, port, result)
}

// waitImgCompressReady 等到 /healthz 真的报 ok:true。
//
// 为什么不是"端口在听就算成功"（AGENTS 第三节）：服务进程可以起来而 vips
// 不可用（brew 半装、库缺失），这时界面能打开但任何压缩都会失败 ——
// 端口探测会把它报成健康，用户点下去才发现是坏的。
func (m *Manager) waitImgCompressReady(ctx context.Context, p imgCompressPaths, port int, result *InstallResult) error {
	const timeout = 60 * time.Second
	url := imgCompressHealthURLFor(port)
	wait := imgCompressHealthy
	return assertReady(ctx, readySpec{
		What:    "图片压缩网页界面",
		Expect:  url + " 在 " + timeout.String() + " 内返回 {\"ok\":true}",
		Timeout: timeout,
		Probe: func(ctx context.Context) readyVerdict {
			if wait(ctx, url, timeout) {
				return readyVerdict{OK: true, Actual: "已就绪，健康检查确认引擎可用"}
			}
			return readyVerdict{Actual: url + " 没有返回 ok:true（服务进程没起来，或 vips 引擎不可用）"}
		},
		LogPath: p.ErrLog,
		State:   "引擎 vips 已复核可执行，服务已登记进「服务管理」",
		Missing: "但网页界面不可用，市场里的「打开」与直连端口都会打不开",
		Remedy: "在「服务管理 → 图片压缩」里点「重启服务」再试；" +
			"仍然失败请看下面的日志尾部（常见原因：vips 未安装完整）",
		Result: result,
	})
}

// UninstallImageCompressor 卸载：先停掉面板托管的网页界面，再摘掉引擎。
//
// 如实说明后果：卸载后网页界面与「文件管理 → 图片压缩」都会不可用，
// 但**不会**动用户任何一张图片 —— 压缩是就地读、输出到目标路径的操作，
// 没有"面板的数据目录"要清理。
func (m *Manager) UninstallImageCompressor(ctx context.Context, app App, result *InstallResult) error {
	if err := m.removeImgCompressService(ctx, result); err != nil {
		return err
	}
	formula := app.BrewFormula
	if formula == "" {
		formula = ImgCompressFormula
	}
	if !m.brewHas(ctx, formula) {
		if result != nil {
			result.step(ctx, formula+" 未安装（Homebrew 里没有它），无需卸载")
		}
		return nil
	}
	if result != nil {
		result.step(ctx, "正在 brew uninstall "+formula)
	}
	if _, err := m.brewRun(ctx, 10*time.Minute, "uninstall", formula); err != nil {
		return fmt.Errorf("卸载 %s 失败: %w", formula, err)
	}
	if result != nil {
		result.step(ctx, "已卸载 "+formula+
			"：网页界面与「文件管理 → 图片压缩」都会显示引擎不可用（图片本身没有任何改动）")
	}
	return nil
}

// removeImgCompressService 停止并删除网页界面服务（幂等）。
func (m *Manager) removeImgCompressService(ctx context.Context, result *InstallResult) error {
	p := m.imgCompressPaths()
	hasPlist := fileExists(p.Plist)
	hasRecord := false
	if list, err := m.repo.List(ctx); err == nil {
		for _, s := range list {
			if s.LaunchLabel == ImgCompressLabel {
				hasRecord = true
				break
			}
		}
	}
	if !hasPlist && !hasRecord {
		if result != nil {
			result.step(ctx, "图片压缩网页界面服务本来就没有注册，跳过停止")
		}
		return nil
	}
	if result != nil {
		result.step(ctx, "停止并删除 launchd 服务 "+ImgCompressLabel+"（网页界面）")
	}
	if err := imgCompressStop(m, ctx, ImgCompressLabel, p.Plist); err != nil {
		return fmt.Errorf("停止图片压缩网页界面失败: %w", err)
	}
	return nil
}

// imgCompressVersion 跑一次 `vips --version` 复核引擎真的能用。
func (m *Manager) imgCompressVersion(ctx context.Context, bin string) (string, error) {
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("安装似乎完成了，但 %s 不存在：%w"+
			"（这通常是 Homebrew 下载/链接失败，请重试或在终端跑 brew install %s）",
			bin, err, ImgCompressFormula)
	}
	// 直接执行（不需要降权：`vips --version` 只是打印版本，任何人可跑）。
	// 用 sudo -u 反而会把"用户不存在"这种测试/环境问题混进来。
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	outRaw, err := exec.CommandContext(cctx, bin, "--version").CombinedOutput()
	out := string(outRaw)
	if err != nil {
		return "", fmt.Errorf("引擎装好了但跑不起来（%s --version 失败）：%v\n%s",
			bin, err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

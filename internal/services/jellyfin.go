package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ============================================================================
//  Jellyfin（开源自托管媒体服务器）的安装 / 卸载 —— 面板自研安装器
//
//  为什么不用通用 brew 轨：Homebrew 没有 jellyfin（上游给的是 macOS arm64 的
//  .tar.xz，不是 formula），通用流程会 `brew install` 一个不存在的包。
//  为什么不用「官方 release 原生二进制」通用轨（releaseBinaryApps）：那条轨的
//  extractArgs 写死 `-xzf`（Jellyfin 是 .tar.xz），产物名里也没有 darwin
//  （上游就叫 jellyfin_12.1-arm64.tar.xz），而且它把「系统级 LaunchDaemon +
//  运行身份」整套隐式生成。
//
//  运行方式（用户 2026-09-29 拍板 A 方案）：系统级 LaunchDaemon
//  com.zizdog.jellyfin 执行**面板二进制 + `jellyfin-supervise`**（不是 jellyfin
//  本体），supervisor 以 root fork 后 setuid 到真实用户运行 jellyfin ——
//  与面板**同一代码要求** ⇒ 继承面板已获得的 TCC 授权（读外接盘媒体库不需要
//  第二套授权，升级也不失效）。契约见 cmd/zizpanel/jellyfin.go。
//
//  安装布局（放**面板自己的应用目录**，版本目录 + current 链接，便于回退）：
//    <安装根>/apps/jellyfin/
//      ├── v12.1/            本版本完整解包内容（jellyfin 二进制 + jellyfin-web/）
//      └── current -> v12.1  运行/回退共用的稳定入口（plist 指向它）
//  下载包落在 <WorkDir>/jellyfin-download/，装完即删（85MB 不常驻）。
//  用户数据（配置/元数据/播放记录）在 <家目录>/Library/Application Support/jellyfin，
//  **卸载默认保留**，绝不进安装目录的删除路径。
//
//  版本真源：镜像站上**没有** apps/jellyfin/manifest.json（2026-09-29 实测 404）
//  ⇒ 面板不猜、不爬网，版本就是下面的 JellyfinVersion 常量。新版本要同步到
//  apps/jellyfin/vX.Y/ 并同时更新 JellyfinVersion / JellyfinVersionDir /
//  JellyfinAsset / JellyfinSHA256；因为没有索引，**不做在线「可更新」探测**。
// ============================================================================

const (
	// JellyfinAppID 是应用市场里的条目 ID。
	JellyfinAppID = "jellyfin"
	// JellyfinLabel 是系统级 LaunchDaemon 的 launchd 标签。
	JellyfinLabel = "com.zizdog.jellyfin"
	// JellyfinPort 是 Jellyfin 默认 HTTP 端口（网页界面 / 健康检查都在这）。
	JellyfinPort = 8096
	// JellyfinHealthPath 是就绪端点；未配置前也返回 Healthy。
	JellyfinHealthPath = "/health"
	// JellyfinHealthExpect 是健康检查期望的响应内容（Jellyfin 返回 Healthy）。
	JellyfinHealthExpect = "Healthy"

	// JellyfinVersion 是面板认为该装的版本（镜像目录段与文件名都从这里派生）。
	JellyfinVersion = "12.1"
	// JellyfinVersionDir 是安装目录/镜像上的版本目录名。
	JellyfinVersionDir = "v" + JellyfinVersion
	// JellyfinAsset 是镜像站上的产物文件名（上游原名，没有 darwin 字样）。
	JellyfinAsset = "jellyfin_12.1-arm64.tar.xz"
	// JellyfinSHA256 是 JellyfinAsset 的**完整** sha256。
	//
	// 来源：2026-09-29 对镜像站上的整包实测复算（shasum -a 256）；同一份文件的
	// MD5 = 11c3638ae93dc356ab444ca36b110828 与 Jellyfin 官网公布值一致。
	JellyfinSHA256 = "139931bebdc0dd6997d722816b45659e648ffac1b8dae84134373983f005abaf"
	// JellyfinArtifactBytes 是产物的精确字节数（实测）。
	JellyfinArtifactBytes = 85662936

	// JellyfinDownloadTimeout 是下载 85MB 应用包的超时。
	JellyfinDownloadTimeout = 30 * time.Minute
	// JellyfinExtractTimeout 是解包超时（2842 个成员）。
	JellyfinExtractTimeout = 10 * time.Minute
	// JellyfinReadyTimeout 是等 /health 真的返回 Healthy 的上限。
	JellyfinReadyTimeout = 90 * time.Second
)

// JellyfinInstallRoot 是安装根（默认面板应用目录；单测指向 t.TempDir()）。
//
// 与 zizvideo 的 ZizvideoInstallRoot 同一套约定：做成变量，单测才能隔离 ——
// 测试绝不允许往真实的 /opt/zizpanel 写任何东西（AGENTS 第三节）。
var JellyfinInstallRoot = "/opt/zizpanel/apps/jellyfin"

// ---------------------------------------------------------------------------
//  仅测试用的注入点
//
//  安装会联网下载、跑 tar、写 /Library/LaunchDaemons、动 launchd、以真实用户
//  身份试读文件；单测既不许联网、也不许碰真实系统状态（AGENTS 第二节 7 / 第三节 4）。
//  做成包级变量，生产路径永远是默认实现，测试替换后由 t.Cleanup 恢复。
// ---------------------------------------------------------------------------

var (
	// jellyfinFetch 下载应用包（默认走 panel 的 curl 下载器）。
	jellyfinFetch = func(m *Manager, ctx context.Context, url, dst string, result *InstallResult) error {
		_, err := m.downloadToFile(ctx, url, dst, JellyfinDownloadTimeout, result, "下载 Jellyfin 应用包")
		return err
	}
	// jellyfinExtract 解包（默认 /usr/bin/tar -xJf，剥掉顶层 jellyfin/ 目录）。
	jellyfinExtract = func(m *Manager, ctx context.Context, archive, dest string) error {
		out, err := m.runRoot(ctx, JellyfinExtractTimeout, "/usr/bin/tar",
			"-xJf", archive, "-C", dest, "--strip-components=1")
		if err != nil {
			return fmt.Errorf("解包 %s 失败：%s", filepath.Base(archive), tailText(strings.TrimSpace(out), 300))
		}
		return nil
	}
	// jellyfinProbe 探测镜像上有没有这个包（默认 HEAD，短超时）。
	jellyfinProbe = func(m *Manager, ctx context.Context, url string) error {
		return m.checkMirrorURL(ctx, url)
	}
	// jellyfinExpectedSHA256 返回期望的 sha256（默认取常量）。
	// 做成注入点是为了让门禁能在**不下载真包**的前提下验证"校验和不符即中止"：
	// 喂一份假内容 + 一个不匹配的期望值，安装必须失败且什么都不装。
	jellyfinExpectedSHA256 = func() string { return JellyfinSHA256 }
	// jellyfinEnsureDeps 补齐基础依赖（默认走公共的 EnsureBaseDependencies）。
	jellyfinEnsureDeps = func(m *Manager, ctx context.Context, result *InstallResult) error {
		return m.EnsureBaseDependencies(ctx, result)
	}
	// jellyfinPanelExecutable 返回面板二进制自身的路径（plist 要执行它）。
	jellyfinPanelExecutable = os.Executable
	// jellyfinLaunch 装载 launchd 服务（默认 bootout + bootstrap + 等旧实例消失）。
	jellyfinLaunch = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.bootstrapService(ctx, label, plist)
	}
	// jellyfinStop 停止并删除服务（幂等）。
	jellyfinStop = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.stopLaunchdService(ctx, label, plist)
	}
	// jellyfinHealthy 打一次 /health 并确认返回 Healthy。
	jellyfinHealthy = func(m *Manager, ctx context.Context, url string, timeout time.Duration) bool {
		body, code, err := runCurlCtx(ctx, url, int(timeout.Seconds()))
		return err == nil && code == 200 && strings.Contains(body, JellyfinHealthExpect)
	}
	// jellyfinReadProbe 以真实用户身份**真的读一次**文件（默认 head -c 1）。
	// 它验证的是"Jellyfin 那个身份读得到吗"—— 外接盘/可移除宗卷上这正是会栽的地方。
	jellyfinReadProbe = func(m *Manager, ctx context.Context, file string) (string, error) {
		return m.runAsUser(ctx, 20*time.Second, "/usr/bin/head", "-c", "1", file)
	}
)

// JellyfinPaths 是一次安装/卸载要用到的全部位置。
type JellyfinPaths struct {
	// Home 是真实用户家目录（用户数据的根）。
	Home string
	// Root 是安装根（<面板根>/apps/jellyfin）。
	Root string
	// VersionDir 是本版本的完整解包目录（<Root>/v12.1）。
	VersionDir string
	// Current 是稳定入口符号链接（<Root>/current，指向 VersionDir）。
	Current string
	// Bin 是服务端二进制（<Current>/jellyfin）。
	Bin string
	// WebDir 是网页界面资源目录（<Current>/jellyfin-web）。
	WebDir string
	// UserDataDir 是用户数据根（配置/元数据/播放记录）—— 卸载**默认保留**。
	UserDataDir string
	// DataDir / ConfigDir / CacheDir / LogDir 是传给 Jellyfin 的四个目录。
	DataDir   string
	ConfigDir string
	CacheDir  string
	LogDir    string
	// SuperviseOutLog / SuperviseErrLog 是 launchd 抓 supervisor 自身的输出。
	SuperviseOutLog string
	SuperviseErrLog string
	// Plist 是系统级 LaunchDaemon 定义路径。
	Plist string
}

// JellyfinPathsFor 按当前 Manager 的用户信息解析全部路径。
//
// **不写死用户名**：家目录来自 Manager 的 UserHome，拿不到才用 /Users/<name>。
func (m *Manager) JellyfinPathsFor() JellyfinPaths {
	root := strings.TrimSpace(JellyfinInstallRoot)
	if root == "" {
		root = "/opt/zizpanel/apps/jellyfin"
	}
	p := JellyfinPaths{
		Root:       root,
		VersionDir: filepath.Join(root, JellyfinVersionDir),
		Current:    filepath.Join(root, "current"),
		Plist:      SystemDaemonPlistPath(JellyfinLabel),
	}
	p.Bin = filepath.Join(p.Current, "jellyfin")
	p.WebDir = filepath.Join(p.Current, "jellyfin-web")
	home := strings.TrimSpace(m.opt.UserHome)
	if home == "" && strings.TrimSpace(m.opt.UserName) != "" {
		home = "/Users/" + strings.TrimSpace(m.opt.UserName)
	}
	if home == "" {
		return p
	}
	p.Home = home
	// 四个 Jellyfin 目录都放在同一个用户数据根下：卸载时只有**一个**路径要说明/保留，
	// 不会出现"删了 datadir 却漏了 configdir"的半清理。
	p.UserDataDir = filepath.Join(home, "Library", "Application Support", JellyfinAppID)
	p.DataDir = filepath.Join(p.UserDataDir, "data")
	p.ConfigDir = filepath.Join(p.UserDataDir, "config")
	p.CacheDir = filepath.Join(p.UserDataDir, "cache")
	p.LogDir = filepath.Join(p.UserDataDir, "log")
	p.SuperviseOutLog = filepath.Join(p.LogDir, "jellyfin-supervise.out.log")
	p.SuperviseErrLog = filepath.Join(p.LogDir, "jellyfin-supervise.err.log")
	return p
}

// jellyfinPaths 与 JellyfinPathsFor 同义（包内短名）。
func (m *Manager) jellyfinPaths() JellyfinPaths { return m.JellyfinPathsFor() }

// JellyfinMirrorPath 返回应用包在镜像站上的**相对路径**。
func JellyfinMirrorPath() string {
	return strings.Join([]string{mirrorAppsDir, JellyfinAppID, JellyfinVersionDir, JellyfinAsset}, "/")
}

// jellyfinAssetURL 拼出运行时生效的镜像地址（基址来自面板设置）。
func (m *Manager) jellyfinAssetURL() string {
	return m.appAssetURL(JellyfinAppID, JellyfinVersionDir, JellyfinAsset)
}

// ---------------------------------------------------------------------------
//  冻结契约：launchd 参数与 Jellyfin 子进程参数
// ---------------------------------------------------------------------------

// JellyfinServeArgs 返回 **Jellyfin 本体**的 argv（不含 argv[0]）。
//
// 这是 supervisor 与门禁共用的唯一真源；plist 传给 supervisor 的路径参数则是
// JellyfinSuperviseArgs。两边改任一 flag 名都会让"参数咬合"门禁变红。
func JellyfinServeArgs(dataDir, configDir, cacheDir, logDir, ffmpeg, webDir string) []string {
	args := []string{
		"--datadir", dataDir,
		"--configdir", configDir,
		"--cachedir", cacheDir,
		"--logdir", logDir,
		"--ffmpeg", ffmpeg,
	}
	if strings.TrimSpace(webDir) != "" {
		args = append(args, "--webdir", webDir)
	}
	return args
}

// JellyfinSuperviseArgs 拼出系统 plist 里的 ProgramArguments：
// **面板二进制** + `jellyfin-supervise` + 真实用户与全部路径。
//
// ⚠️ 冻结契约（用户 2026-09-29）：ProgramArguments[0] **必须是面板二进制**，
// 不是 jellyfin 本体 —— 只有这样 Jellyfin 才作为面板子进程继承面板的 TCC 授权。
// 参数名与 cmd/zizpanel/jellyfin.go 的 flag 一一对应，有咬合门禁锁死。
func JellyfinSuperviseArgs(panelBin, userName string, p JellyfinPaths, ffmpeg string) []string {
	args := []string{
		panelBin, "jellyfin-supervise",
		"--user", userName,
		"--jellyfin", p.Bin,
		"--datadir", p.DataDir,
		"--configdir", p.ConfigDir,
		"--cachedir", p.CacheDir,
		"--logdir", p.LogDir,
		"--ffmpeg", ffmpeg,
	}
	if strings.TrimSpace(p.WebDir) != "" {
		args = append(args, "--webdir", p.WebDir)
	}
	return args
}

// jellyfinPlistContent 渲染系统级 LaunchDaemon 定义。
//
// 刻意**不写 UserName**：这个作业必须以 root 运行 —— supervisor 要 fork 之后
// setuid 降权到真实用户（见 docs/坑清单.md 202）。
func jellyfinPlistContent(label string, args []string, outLog, errLog string) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "    <key>Label</key>\n    <string>%s</string>\n", xmlEscape(label))
	b.WriteString("    <key>ProgramArguments</key>\n    <array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "        <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("    </array>\n")
	b.WriteString("    <key>RunAtLoad</key>\n    <true/>\n")
	b.WriteString("    <key>KeepAlive</key>\n    <true/>\n")
	if len(args) > 0 {
		b.WriteString("    <key>WorkingDirectory</key>\n")
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(filepath.Dir(args[0])))
	}
	b.WriteString("    <key>EnvironmentVariables</key>\n    <dict>\n        <key>PATH</key>\n")
	b.WriteString("        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n")
	b.WriteString("    </dict>\n")
	fmt.Fprintf(&b, "    <key>StandardOutPath</key>\n    <string>%s</string>\n", xmlEscape(outLog))
	fmt.Fprintf(&b, "    <key>StandardErrorPath</key>\n    <string>%s</string>\n", xmlEscape(errLog))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// ---------------------------------------------------------------------------
//  安装
// ---------------------------------------------------------------------------

// InstallJellyfin 安装 Jellyfin（镜像下载 → sha256 → 解包 → current 链接 →
// 注册服务 → 等 /health 返回 Healthy）。任何一步失败都**回滚安装目录**，
// 绝不留下半成品；用户数据目录全程不碰。
func (m *Manager) InstallJellyfin(ctx context.Context, app App, result *InstallResult) error {
	if result != nil {
		result.App = app.ID
		result.Name = app.Name
	}
	p := m.jellyfinPaths()
	if strings.TrimSpace(p.Home) == "" {
		return fmt.Errorf("无法确定真实用户家目录（面板拿不到运行用户），不能安装 Jellyfin")
	}
	if strings.TrimSpace(p.Root) == "" {
		return fmt.Errorf("Jellyfin 安装根为空（面板内部错误）")
	}
	if strings.TrimSpace(m.opt.UserName) == "" {
		return fmt.Errorf("无法确定运行 Jellyfin 的真实用户（UserName 为空）：" +
			"它要写该用户的数据目录，不能以 root 运行")
	}
	if !m.MirrorEnabled() {
		return fmt.Errorf("Jellyfin 只在应用包镜像站上有（没有公网源）：请先在「设置 → 应用包镜像」"+
			"里填镜像基址，或确认镜像站已同步 %s", JellyfinMirrorPath())
	}

	// ① 基础依赖：ffmpeg（转码链路要用）。依赖关系从目录 Requires 读出来提示。
	m.AnnounceAppDependencies(ctx, app.ID, result)
	if err := jellyfinEnsureDeps(m, ctx, result); err != nil {
		return fmt.Errorf("补齐基础依赖失败：%w（Jellyfin 转码要用 ffmpeg）", err)
	}
	ffmpeg := jellyfinFfmpegPath()
	if ffmpeg == "" {
		return fmt.Errorf("找不到 ffmpeg：Jellyfin 的转码链路要用它。" +
			"请先在应用市场安装「ffmpeg」基础依赖，再重试")
	}

	// ② 下载（镜像站是唯一来源；先探一次，缺件立刻如实失败，不白下 85MB）。
	url := m.jellyfinAssetURL()
	if err := jellyfinProbe(m, ctx, url); err != nil {
		return fmt.Errorf("镜像站上没有 Jellyfin %s 的应用包（%s）：%v",
			JellyfinVersion, JellyfinMirrorPath(), err)
	}
	if result != nil {
		result.step(ctx, fmt.Sprintf("下载 Jellyfin %s（%s，%s）",
			JellyfinVersion, JellyfinAsset, humanBytes(JellyfinArtifactBytes)))
	}
	dlDir := filepath.Join(strings.TrimSpace(m.opt.WorkDir), "jellyfin-download")
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		return fmt.Errorf("创建下载目录 %s 失败：%w", dlDir, err)
	}
	archive := filepath.Join(dlDir, JellyfinAsset)
	defer func() { _ = os.Remove(archive) }() // 应用包不留在磁盘上
	if err := jellyfinFetch(m, ctx, url, archive, result); err != nil {
		return err
	}

	// ③ 校验 sha256 —— 不一致**绝不继续**（在解包之前就失败）。
	want := strings.ToLower(strings.TrimSpace(jellyfinExpectedSHA256()))
	got := strings.ToLower(strings.TrimSpace(sha256OfFile(archive)))
	if got == "" {
		return fmt.Errorf("算不出 %s 的 sha256（文件读不了），已中止安装，什么都没装", archive)
	}
	if want == "" {
		_ = os.Remove(archive)
		return fmt.Errorf("面板没有 Jellyfin %s 的可信 sha256：拒绝安装一个无法校验的包", JellyfinVersion)
	}
	if got != want {
		return fmt.Errorf("Jellyfin 应用包 sha256 校验不通过：期望 %s，实际 %s。"+
			"已中止安装，什么都没装（镜像上的包与面板声明的版本不一致，请重新同步 %s）",
			want, got, JellyfinMirrorPath())
	}
	if result != nil {
		result.step(ctx, "sha256 校验通过："+want)
	}

	// ④ 解包 → 复核结构 → 原子换版 → 切 current。
	if err := m.jellyfinInstallTree(ctx, p, archive, result); err != nil {
		return err
	}

	// ⑤ 注册并启动服务（面板二进制 + jellyfin-supervise）。失败即回滚安装目录。
	if err := jellyfinServiceUp(m, ctx, p, ffmpeg, result); err != nil {
		_ = os.Remove(p.Current)
		_ = os.RemoveAll(p.VersionDir)
		return fmt.Errorf("%w（本次安装已回滚：删除 %s 与 current 链接；用户数据 %s 未动）",
			err, p.VersionDir, p.UserDataDir)
	}
	m.RecordInstalledVersion(ctx, JellyfinLabel, JellyfinVersion)

	if result != nil {
		result.Address = m.primaryIP()
		result.Steps = append(result.Steps,
			"安装目录："+p.Root+"（current → "+JellyfinVersionDir+"）",
			"用户数据："+p.UserDataDir+"（卸载默认保留）",
			"服务："+JellyfinLabel+" 执行「面板二进制 jellyfin-supervise」，以 "+m.opt.UserName+
				" 身份运行 Jellyfin（继承面板的文件访问授权）",
			fmt.Sprintf("首次访问：浏览器打开 http://%s:%d/web/ 走安装向导（未配置前没有鉴权）",
				m.primaryIP(), JellyfinPort),
		)
	}
	return nil
}

// jellyfinInstallTree 把应用包解包进版本目录、复核结构、切 current 链接。
//
// 每一步都复核（不看上一步退出码 0）：解包后必须真的看到 jellyfin 二进制与
// jellyfin-web/ 目录 —— 上游改打包方式时这里会**如实失败**，而不是留下一个
// 起不来又说不清的目录。
func (m *Manager) jellyfinInstallTree(ctx context.Context, p JellyfinPaths, archive string, result *InstallResult) error {
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return fmt.Errorf("创建安装根目录 %s 失败：%w", p.Root, err)
	}
	// 先解到暂存目录：中途失败绝不碰正在用的那个版本。
	staging := filepath.Join(p.Root, ".staging-"+JellyfinVersionDir)
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("创建暂存目录 %s 失败：%w", staging, err)
	}
	defer func() {
		if dirExists(staging) {
			_ = os.RemoveAll(staging)
		}
	}()
	if result != nil {
		result.step(ctx, "解包到 "+staging+"（保留完整版本目录，便于回退）")
	}
	if err := jellyfinExtract(m, ctx, archive, staging); err != nil {
		return err
	}
	// 顶层 jellyfin/ 被 --strip-components=1 剥掉 ⇒ 这里直接是二进制与网页目录。
	bin := filepath.Join(staging, "jellyfin")
	web := filepath.Join(staging, "jellyfin-web")
	if !fileExists(bin) {
		return fmt.Errorf("解包后找不到服务端二进制 %s（包结构与面板预期不符，"+
			"上游可能改了打包方式）", bin)
	}
	if !dirExists(web) {
		return fmt.Errorf("解包后找不到网页界面目录 %s（缺了它 Jellyfin 起来也没有可用的界面）", web)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("设置可执行位失败（%s）：%w", bin, err)
	}
	// 原子换版：新内容完全就位后才动 current。
	if err := os.RemoveAll(p.VersionDir); err != nil {
		return fmt.Errorf("清理旧版本目录 %s 失败：%w", p.VersionDir, err)
	}
	if err := os.Rename(staging, p.VersionDir); err != nil {
		return fmt.Errorf("把新版本就位失败（%s → %s）：%w", staging, p.VersionDir, err)
	}
	tmpLink := p.Current + ".tmp"
	_ = os.Remove(tmpLink)
	if err := os.Symlink(JellyfinVersionDir, tmpLink); err != nil {
		return fmt.Errorf("创建 current 符号链接失败：%w", err)
	}
	if err := os.Rename(tmpLink, p.Current); err != nil {
		_ = os.Remove(tmpLink)
		return fmt.Errorf("切换 current 符号链接失败：%w", err)
	}
	if result != nil {
		result.step(ctx, "已就位："+p.VersionDir+"（current → "+JellyfinVersionDir+"）")
	}
	return nil
}

// jellyfinServiceUp / jellyfinServiceDown 是"注册/停止系统级守护进程"的注入点。
// 默认实现走 jellyfinInstallService / jellyfinRemoveService；单测注入假实现
// （绝不写 /Library/LaunchDaemons、绝不动 launchd）。
var (
	jellyfinServiceUp = func(m *Manager, ctx context.Context, p JellyfinPaths, ffmpeg string, result *InstallResult) error {
		return m.jellyfinInstallService(ctx, p, ffmpeg, result)
	}
	jellyfinServiceDown = func(m *Manager, ctx context.Context, p JellyfinPaths, result *InstallResult) error {
		return m.jellyfinRemoveService(ctx, p, result)
	}
	// jellyfinFfmpegPath 返回 ffmpeg 路径（默认按固定候选 + LookPath 找）。
	jellyfinFfmpegPath = findFfmpegBin
)

// jellyfinInstallService 是服务注册的默认实现：写系统级 plist
// （执行面板二进制 + jellyfin-supervise）、装载、登记，并等 /health 真的返回 Healthy。
func (m *Manager) jellyfinInstallService(ctx context.Context, p JellyfinPaths, ffmpeg string, result *InstallResult) error {
	panelBin, err := jellyfinPanelExecutable()
	if err != nil || strings.TrimSpace(panelBin) == "" {
		return fmt.Errorf("找不到面板自身的可执行文件路径（系统守护进程要用它执行 jellyfin-supervise）：%v", err)
	}
	// 数据/日志目录：由面板（root）建好并把归属交还真实用户 —— 服务以该用户运行，
	// 属主不对就写不进东西（用户数据目录属主必须是真实用户，真机会复核）。
	for _, dir := range []string{p.DataDir, p.ConfigDir, p.CacheDir, p.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败：%w", dir, err)
		}
	}
	if err := chownTree(m.opt.UserName, p.UserDataDir); err != nil {
		return fmt.Errorf("把用户数据目录 %s 交还 %s 失败：%w（服务以该用户运行，属主不对就写不进东西）",
			p.UserDataDir, m.opt.UserName, err)
	}
	if err := os.MkdirAll(filepath.Dir(p.Plist), 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败：%w", filepath.Dir(p.Plist), err)
	}
	args := JellyfinSuperviseArgs(panelBin, m.opt.UserName, p, ffmpeg)
	if strings.TrimSpace(args[0]) == "" || args[1] != "jellyfin-supervise" {
		return fmt.Errorf("Jellyfin 的启动参数形状不对（内部错误）：%v", args)
	}
	content := jellyfinPlistContent(JellyfinLabel, args, p.SuperviseOutLog, p.SuperviseErrLog)
	tmp := p.Plist + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 plist %s 失败（面板需要以 root 运行）：%w", p.Plist, err)
	}
	if err := os.Chown(tmp, 0, 0); err != nil && os.Geteuid() == 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("把 plist 归属改为 root:wheel 失败（%s）：%w", tmp, err)
	}
	if err := os.Rename(tmp, p.Plist); err != nil {
		return fmt.Errorf("安装 plist %s 失败：%w", p.Plist, err)
	}
	if err := os.Chown(p.Plist, 0, 0); err != nil && os.Geteuid() == 0 {
		return fmt.Errorf("修正 plist 属主失败（%s）：%w", p.Plist, err)
	}
	if result != nil {
		result.step(ctx, "正在注册并启动系统级守护进程 "+JellyfinLabel+
			"（可执行文件是面板自己：jellyfin-supervise，服务以 "+m.opt.UserName+" 身份运行 Jellyfin）")
	}
	if err := jellyfinLaunch(m, ctx, JellyfinLabel, p.Plist); err != nil {
		return fmt.Errorf("启动 Jellyfin 失败：%w", err)
	}
	app, _ := FindApp(JellyfinAppID)
	if err := m.RegisterInstalledService(ctx, JellyfinLabel, app.Name, app.Icon, app.Category, JellyfinPort); err != nil {
		if result != nil {
			result.step(ctx, "警告：服务已启动，但登记进「服务管理」失败："+err.Error()+
				"（可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
		}
	}
	return m.waitJellyfinReady(ctx, p, result)
}

// waitJellyfinReady 等 /health 真的返回 Healthy（端口在听不算，AGENTS 第三节）。
func (m *Manager) waitJellyfinReady(ctx context.Context, p JellyfinPaths, result *InstallResult) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", JellyfinPort, JellyfinHealthPath)
	var last string
	deadline := time.Now().Add(JellyfinReadyTimeout)
	for time.Now().Before(deadline) {
		if jellyfinHealthy(m, ctx, url, 10*time.Second) {
			return assertReady(ctx, readySpec{
				What:    "Jellyfin 网页界面",
				Expect:  url + " 在 " + JellyfinReadyTimeout.String() + " 内返回 " + JellyfinHealthExpect,
				Timeout: JellyfinReadyTimeout,
				Probe: func(context.Context) readyVerdict {
					return readyVerdict{OK: true, Actual: "已就绪：" + url + " 返回 " + JellyfinHealthExpect}
				},
				LogPath: filepath.Join(p.LogDir, "jellyfin-supervise.log"),
				Result:  result,
			})
		}
		last = url + " 还没有返回 " + JellyfinHealthExpect
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(800 * time.Millisecond):
		}
	}
	return assertReady(ctx, readySpec{
		What:    "Jellyfin 网页界面",
		Expect:  url + " 在 " + JellyfinReadyTimeout.String() + " 内返回 " + JellyfinHealthExpect,
		Timeout: JellyfinReadyTimeout,
		Probe: func(context.Context) readyVerdict {
			return readyVerdict{Actual: last}
		},
		LogPath: filepath.Join(p.LogDir, "jellyfin-supervise.log"),
		State: "应用包已校验解包（" + p.VersionDir + "），系统级守护进程 " + JellyfinLabel +
			" 执行面板二进制的 jellyfin-supervise，服务已按 " + m.opt.UserName + " 身份启动",
		Missing: "但 127.0.0.1:" + fmt.Sprint(JellyfinPort) + JellyfinHealthPath +
			" 没有返回 " + JellyfinHealthExpect + "，浏览器打不开它",
		Remedy: "在「服务管理 → Jellyfin」里点「重启服务」再试；仍然失败请看下面的日志尾部" +
			"（常见原因：端口被占、用户数据目录属主不对、ffmpeg 不在）",
		Result: result,
	})
}

// ---------------------------------------------------------------------------
//  卸载
// ---------------------------------------------------------------------------

// UninstallJellyfin 卸载：停服务 + 删安装目录；用户数据默认保留。
//
// removeData=true（用户在确认框勾选「同时删除数据」）时才删用户数据目录。
// 媒体文件（用户的电影/剧集）**从来不在**这两个路径里，永远不碰。
func (m *Manager) UninstallJellyfin(ctx context.Context, app App, removeData, force bool, result *InstallResult) error {
	_ = force // 没有 brew 依赖这回事，忽略它
	_ = app
	p := m.jellyfinPaths()
	if err := jellyfinServiceDown(m, ctx, p, result); err != nil {
		return err
	}
	if dirExists(p.Root) {
		if result != nil {
			result.step(ctx, "删除安装目录 "+p.Root)
		}
		if err := os.RemoveAll(p.Root); err != nil {
			return fmt.Errorf("删除安装目录 %s 失败：%w", p.Root, err)
		}
	} else if result != nil {
		result.step(ctx, "安装目录本来就不在（"+p.Root+"），跳过")
	}
	if removeData {
		if dirExists(p.UserDataDir) {
			if result != nil {
				result.step(ctx, "按你的选择删除用户数据目录 "+p.UserDataDir)
			}
			if err := os.RemoveAll(p.UserDataDir); err != nil {
				return fmt.Errorf("删除用户数据目录 %s 失败：%w", p.UserDataDir, err)
			}
		} else if result != nil {
			result.step(ctx, "用户数据目录本来就不在（"+p.UserDataDir+"），跳过")
		}
		return nil
	}
	if result != nil {
		result.step(ctx, "**保留**用户数据目录："+p.UserDataDir+
			"（配置/元数据/播放记录；要一并清理请在确认框勾选「同时删除数据」）")
	}
	return nil
}

// jellyfinRemoveService 是停止的默认实现：停止并删除服务、删 plist（幂等）。
func (m *Manager) jellyfinRemoveService(ctx context.Context, p JellyfinPaths, result *InstallResult) error {
	hasPlist := p.Plist != "" && fileExists(p.Plist)
	hasRecord := false
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if s.LaunchLabel == JellyfinLabel {
					hasRecord = true
					break
				}
			}
		}
	}
	if hasPlist || hasRecord {
		if result != nil {
			result.step(ctx, "停止并删除 launchd 服务 "+JellyfinLabel)
		}
		if err := m.removeService(ctx, JellyfinLabel, p.Plist); err != nil {
			return fmt.Errorf("停止 Jellyfin 失败：%w", err)
		}
	} else if result != nil {
		result.step(ctx, "Jellyfin 服务本来就没有注册，跳过停止")
	}
	// plist 兜底再删一次（stop 只管"停止"，文件必须真的没有 —— 坑 161）。
	if p.Plist != "" {
		if err := os.Remove(p.Plist); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 plist %s 失败：%w", p.Plist, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
//  媒体目录：面板侧的"同身份试读"检查
//
//  Jellyfin 的媒体库路径由它自己的安装向导/后台添加 —— 面板不往它的数据库里
//  写库路径（那是上游私有格式）。面板能做、且**必须**做的是一件事：
//  用 Jellyfin 将要以之运行的那个身份（真实用户）**真的读一次**用户挑的目录，
//  读不到就当场说清并给可执行出路 —— 否则用户会在 Jellyfin 里看到一个空库，
//  却完全不知道是外接盘的隐私保护把它挡住了。
// ---------------------------------------------------------------------------

// JellyfinMediaCheck 是一个媒体目录的试读结论（JSON 契约，前端直接渲染 message）。
type JellyfinMediaCheck struct {
	Dir string `json:"dir"`
	// OK=true 表示以真实用户身份真的读到了一个文件。
	OK bool `json:"ok"`
	// Sample 是这次读到的那个文件（空 = 没读到）。
	Sample string `json:"sample,omitempty"`
	// Message 是给用户看的一句话（成功/失败都说清）。
	Message string `json:"message"`
	// Remedy 是失败时的**可执行出路**（成功时为空）。
	Remedy string `json:"remedy,omitempty"`
}

// JellyfinPickMediaSample 在目录里找一个用于试读的普通文件（有界遍历）。
//
// 有界是硬要求：媒体库里可能有几十万文件；这里最多看 walkBudget 个条目。
// 跳过隐藏文件与隐藏子目录；返回第一个普通文件的路径，找不到返回空串。
func JellyfinPickMediaSample(dir string) string {
	const walkBudget = 4000
	seen := 0
	var found string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 读不动的子项跳过，不影响其它
		}
		seen++
		if seen > walkBudget || found != "" {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			found = path
		}
		return nil
	})
	return found
}

// JellyfinMediaReadCheck 用真实用户身份试读目录里的一个文件，返回结论与出路。
//
// 判据贴着**运行体**：不是"目录存在"就算可读，而是真的以 Jellyfin 的身份打开
// 并读出 1 个字节。外接盘 / ~/Downloads / ~/Desktop 这些受 TCC 保护的位置，
// 只有这样做才能提前发现"装好了却看不到媒体"。
func (m *Manager) JellyfinMediaReadCheck(ctx context.Context, dir string) JellyfinMediaCheck {
	out := JellyfinMediaCheck{Dir: strings.TrimSpace(dir)}
	dir = out.Dir
	if dir == "" {
		out.Message = "还没有选媒体目录。"
		out.Remedy = "在下面填一个目录（例如 ~/Movies），或先把电影/剧集放进去再选。"
		return out
	}
	if !filepath.IsAbs(dir) {
		out.Message = "媒体目录必须是绝对路径（现在填的是 " + dir + "）。"
		out.Remedy = "用 / 开头的绝对路径，例如 " + filepath.Join(m.opt.UserHome, "Movies") + "。"
		return out
	}
	st, err := os.Stat(dir)
	if err != nil {
		out.Message = "读不到这个目录：" + dir + "（" + err.Error() + "）。"
		out.Remedy = "确认目录还在、外接盘已挂载、路径没写错；外接盘要先在 Finder 里挂载再重试。"
		return out
	}
	if !st.IsDir() {
		out.Message = dir + " 不是一个目录。"
		out.Remedy = "换成放电影/剧集/音乐的目录。"
		return out
	}
	if strings.TrimSpace(m.opt.UserName) == "" {
		out.Message = "面板读不到真实用户名，无法验证 Jellyfin 身份能不能读这个目录。"
		out.Remedy = "先在面板设置里确认运行用户，再重试。"
		return out
	}
	sample := JellyfinPickMediaSample(dir)
	if sample == "" {
		out.OK = false
		out.Message = "目录里一个普通文件都没有：Jellyfin 会把它当空库。"
		out.Remedy = "把电影/剧集/音乐放进去，或换一个有内容的目录（只放 .mp4/.mkv/.mp3 等媒体文件即可）。"
		return out
	}
	out.Sample = sample
	if _, rerr := jellyfinReadProbe(m, ctx, sample); rerr != nil {
		lower := strings.ToLower(rerr.Error())
		switch {
		case strings.Contains(lower, "operation not permitted"), strings.Contains(lower, "permission denied"):
			out.Message = m.opt.UserName + " 读不到 " + sample + "（权限被拒）。" +
				"Jellyfin 会看到一个空库。"
			out.Remedy = "① 若媒体在外接盘/可移除宗卷：到「系统设置 → 隐私与安全性 → 完全磁盘访问」" +
				"把面板（zizpanel）打开，然后重试；② 或把媒体放到内建盘（例如 " +
				filepath.Join(m.opt.UserHome, "Movies") + "）；③ 确认那个卷已挂载、" + dir + " 确实存在。"
		case strings.Contains(lower, "no such file"), strings.Contains(lower, "does not exist"):
			out.Message = "试读时文件不见了：" + sample + "（卷可能被拔掉/路径变了）。"
			out.Remedy = "重新挂载外接盘后重试，或换一个内建盘目录。"
		default:
			out.Message = m.opt.UserName + " 读不到 " + sample + "：" + tailText(strings.TrimSpace(rerr.Error()), 200)
			out.Remedy = "检查目录权限（ls -lde " + dir + "）与卷是否挂载；外接盘请先给面板" +
				"「完全磁盘访问」再重试。"
		}
		return out
	}
	out.OK = true
	out.Message = "以 " + m.opt.UserName + " 身份已读到 " + sample + "（1 字节）。"
	return out
}

// SupportsMediaDirs 报告这个条目有没有「媒体目录」配置（面板保存目录 + 以应用身份试读）。
//
// 派生给市场列表的 media_dirs 字段：前端据此决定是否渲染媒体目录区，
// 而不是在 servicePanel.js 里写 `m.id === 'jellyfin'`（见那里的文件头纪律）。
func SupportsMediaDirs(id string) bool { return id == JellyfinAppID }

// NormalizeJellyfinMediaDirs 去重、去空、排序（保存前统一形状）。
func NormalizeJellyfinMediaDirs(dirs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
//  市场反漂移：安装器自管的镜像产物事实
// ---------------------------------------------------------------------------

// PanelInstallerArtifact 是「面板自研安装器自己管的镜像产物」的事实。
//
// 为什么要有它：市场声明里的 release_binary 下载点，原来只与 releaseBinaryApps
// 注册表交叉核对；Jellyfin 不走那条通用轨（见文件头），如果不在市场门禁里
// 补上"与安装器常量比对"，声明就可以随便写而不被发现 —— 那正是
// "审计全绿、装的却是别的东西"。这里把安装器的常量暴露给门禁，两处只有一份真源。
type PanelInstallerArtifact struct {
	AppID   string
	Version string // 镜像/安装目录的版本段（v12.1）
	Asset   string
	SHA256  string
	Size    int64
	NASPath string // 镜像站相对路径
}

// PanelInstallerArtifacts 返回全部"自管镜像产物"（按 AppID）。
func PanelInstallerArtifacts() map[string]PanelInstallerArtifact {
	return map[string]PanelInstallerArtifact{
		JellyfinAppID: {
			AppID:   JellyfinAppID,
			Version: JellyfinVersionDir,
			Asset:   JellyfinAsset,
			SHA256:  JellyfinSHA256,
			Size:    JellyfinArtifactBytes,
			NASPath: JellyfinMirrorPath(),
		},
	}
}

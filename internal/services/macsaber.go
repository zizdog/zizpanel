package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

// ============================================================================
//  mac军刀（MacSaber）的安装 / 卸载
//
//  这个条目与目录里别的条目都不同：它**没有上游**。mac军刀是本项目自己的
//  Go 单二进制（仓库 macsaber/），产物由 tools/macsaber-release.sh 自己打，
//  只发到公网镜像站 `<base>/apps/macsaber/<ver>/`，没有任何 GitHub 回落源。
//
//  因此安装流程不能走 releaseBinaryApps 那套注册表（它的语义是"去别人家下载"），
//  这里独立实现，但**复用同一批约定**：
//    · 安装根 /opt/macsaber（二进制 /opt/macsaber/bin/macsaber）；
//    · 服务是 **LaunchAgent**（label cn.macsaber.web，plist 落在真实用户的
//      ~/Library/LaunchAgents/），不是系统级 LaunchDaemon —— 它读的是该用户的
//      家目录、写的是该用户的 ~/MacSaberFiles，以 root 跑反而看不到用户的东西；
//    · 下载、校验、解包、复核、起服务、验收，每一步失败都如实报错（铁律 11）。
//
//  判据贴着**运行体**（AGENTS 第三节）：二进制在 + `macsaber version` 对得上
//  + 127.0.0.1:<MacSaberPort>/api/version 真的返回这个版本，三条都过才算装好。
//  "plist 写出来了"永远不算装好 —— 僵尸 plist 让市场谎报已装的坑本仓库踩过。
// ============================================================================

// ---------------------------------------------------------------------------
//  仅测试用的注入点
//
//  单测绝不允许真下载、真写 /opt、真调 launchctl（AGENTS 第三节）。
//  生产路径永远是下面的默认实现，测试替换后恢复。
// ---------------------------------------------------------------------------

var (
	// MacSaberInstallRoot 是安装根。默认 /opt/macsaber（面板以 root 创建），
	// 单测指向 t.TempDir()。
	MacSaberInstallRoot = "/opt/macsaber"
	// macSaberFetch 把一个 URL 下到本地文件（含停滞看门狗 + .part 原子改名）。
	macSaberFetch = func(ctx context.Context, url, dest string, onProgress func(got, total int64)) error {
		return fetchToFile(ctx, &http.Client{}, url, dest, iopaintStallTimeout, onProgress)
	}
	// macSaberExtract 解归档（默认调系统 tar；测试注入，绝不真解包）。
	macSaberExtract = func(m *Manager, ctx context.Context, archive, dest string) error {
		_, err := m.runRoot(ctx, 3*time.Minute, "/usr/bin/tar", "-xzf", archive, "-C", dest)
		return err
	}
	// macSaberVerifyArm64 用 file(1) 复核 Mach-O arm64（测试注入：
	// 单测里的"二进制"是脚本，不可能真是 arm64，而真实那把 file 也不会被跳过）。
	macSaberVerifyArm64 = verifyMacSaberArm64
	// macSaberLaunch 装载 launchd 作业（默认走 priv 那套按域探测 + 复核的实现）。
	macSaberLaunch = func(label, plist string) error { return privLaunchLoad(label) }
	// macSaberStop 停止并卸载 launchd 作业（幂等）。
	macSaberStop = func(label, plist string) error { return privLaunchUnload(label) }
	// macSaberVersionFn 执行 `macsaber version` 并把输出原样带回（以真实用户身份）。
	macSaberVersionFn = func(m *Manager, ctx context.Context, bin string) (string, error) {
		out, err := m.runAsUser(ctx, 30*time.Second, bin, "version")
		if err != nil {
			return out, fmt.Errorf("执行 %s version 失败：%w（%s）", bin, err, tailText(strings.TrimSpace(out), 200))
		}
		return out, nil
	}
	// macSaberHTTPGet 取一个 HTTP 端点（默认 curl；测试注入假实现，绝不联网）。
	macSaberHTTPGet = func(ctx context.Context, url string) (body string, code int, err error) {
		return runCurlCtx(ctx, url, 4)
	}
)

// privLaunchLoad / privLaunchUnload 是对 internal/priv 的薄包装。
// 单独包一层是为了让"默认实现"在同一处可见：它们按 user/<uid> 与 gui/<uid>
// 两个域探测并**复核终态**，正是 LaunchAgent 需要的做法（不要另发明一套 launchctl）。
func privLaunchLoad(label string) error   { return priv.LaunchLoad(label) }
func privLaunchUnload(label string) error { return priv.LaunchUnload(label) }

const (
	// MacSaberAppID 是应用市场里的条目 ID。
	MacSaberAppID = "macsaber"
	// MacSaberLabel 是 launchd 标签（目录 README 预留的就是它）。
	MacSaberLabel = "cn.macsaber.web"
	// MacSaberSlug 是面板别名（/<slug>/）。
	MacSaberSlug = "macsaber"
	// MacSaberVersion 是这一版面板所打包/验收的 mac军刀 版本。
	// 与 tools/macsaber-release.sh 里的默认值同源；镜像站的目录名也用它。
	MacSaberVersion = "0.1.0"
	// MacSaberPort 是网页界面端口（只绑 127.0.0.1）。
	//
	// 用 8895 而不是 8899：8899 已经是「TtsVoice 音色接收端」（receiverPort，绑 0.0.0.0，
	// 是网站插件 + 面板 Qwen 反代依赖的对外入口），端口唯一性有测试锁死
	// （TestCatalogPortsAreUnique / TestSTTPortIsUnique）；那套链路属另一个项目，
	// 按铁律 2 不许我们动它的配置。8895 是本机与目录里都空着的端口。
	// 改端口必须重装一次（plist 写的是启动参数，不会自己更新）。
	MacSaberPort = 8895
	// macSaberHealthPath 是**不需要登录**的健康端点（/api/version 是公开的），
	// 返回 {"ok":true,"version":"0.1.0","name":"mac军刀"}。
	macSaberHealthPath = "/api/version"
	// macSaberDownloadTimeout 是整包下载上限（产物只有几 MB，150 秒与仓库里
	// release 二进制那条轨一致）。
	macSaberDownloadTimeout = 150 * time.Second
	// macSaberReadyTimeout 是等网页界面就绪的上限。
	macSaberReadyTimeout = 60 * time.Second
)

// MacSaberArchiveSHA256 是打包时**实测**的归档 sha256（tools/macsaber-release.sh 打印的那一个）。
//
// 它的位置是"镜像站 manifest.json 拿不到时的期望值"—— 公网镜像缺清单时仍然
// 有内容校验，而不是静默跳过。换版本时**必须**一起改（测试会锁住"版本与它成对"）。
var MacSaberArchiveSHA256 = "7437172d21522c5fdcb92d8ee1d260121624593a680e5cba442b099e6e553693"

// macSaberArchiveSizeHint 是同一份归档的字节数（2026-09-20 实跑发布脚本得到）。
// 声明里写它，在线审计就能发现"镜像站上同步了别的版本"。
const macSaberArchiveSizeHint = int64(3261135)

// MacSaberArtifactName 是产物文件名（镜像站布局 <base>/apps/macsaber/<ver>/<name>）。
func MacSaberArtifactName(version string) string {
	return "macsaber_" + strings.TrimSpace(version) + "_darwin_arm64.tar.gz"
}

// MacSaberPaths 是一次安装要落盘的全部位置。
type MacSaberPaths struct {
	// Root 是安装根（/opt/macsaber）。
	Root string
	// Bin 是可执行文件（/opt/macsaber/bin/macsaber）。
	Bin string
	// Plist 是 LaunchAgent 定义（真实用户家目录下）。
	Plist string
	// DataDir 是应用数据目录（配置与审计日志）—— 卸载时**默认保留**。
	DataDir string
	// ReadRoot 是 --read-root（默认整个家目录）。
	ReadRoot string
	// WriteRoot 是 --write-root（默认 ~/MacSaberFiles）。
	WriteRoot string
	// OutLog / ErrLog 是 launchd 的标准输出/错误。
	OutLog string
	ErrLog string
}

// MacSaberHome 返回真实用户家目录（拿不到返回空串，调用方必须如实失败）。
func (m *Manager) MacSaberHome() string {
	if h := strings.TrimSpace(m.opt.UserHome); h != "" {
		return h
	}
	if u := strings.TrimSpace(m.opt.UserName); u != "" {
		return "/Users/" + u
	}
	return ""
}

// MacSaberPathsFor 按当前 Manager 的用户信息解析全部路径。
func (m *Manager) MacSaberPathsFor() MacSaberPaths {
	home := m.MacSaberHome()
	root := strings.TrimSpace(MacSaberInstallRoot)
	if root == "" {
		root = "/opt/macsaber"
	}
	p := MacSaberPaths{
		Root: root,
		Bin:  filepath.Join(root, "bin", "macsaber"),
	}
	if home == "" {
		return p
	}
	p.Plist = filepath.Join(home, "Library", "LaunchAgents", MacSaberLabel+".plist")
	p.DataDir = filepath.Join(home, "Library", "Application Support", "macsaber")
	p.ReadRoot = home
	p.WriteRoot = filepath.Join(home, "MacSaberFiles")
	logDir := filepath.Join(home, "Library", "Logs")
	p.OutLog = filepath.Join(logDir, "macsaber.out.log")
	p.ErrLog = filepath.Join(logDir, "macsaber.err.log")
	return p
}

// MacSaberBin 返回可执行文件路径（给"安装体"判定与测试用）。
func MacSaberBin() string {
	root := strings.TrimSpace(MacSaberInstallRoot)
	if root == "" {
		root = "/opt/macsaber"
	}
	return filepath.Join(root, "bin", "macsaber")
}

// macSaberServeArgs 拼出 plist 里的 ProgramArguments。
// 参数以 macsaber/main.go 的 serve 子命令为准（别照文档猜）。
func macSaberServeArgs(p MacSaberPaths) []string {
	if p.Bin == "" {
		return nil
	}
	args := []string{
		p.Bin, "serve",
		"--data", p.DataDir,
		"--listen", fmt.Sprintf("127.0.0.1:%d", MacSaberPort),
		"--read-root", p.ReadRoot,
		"--write-root", p.WriteRoot,
	}
	return args
}

// macSaberPlistContent 渲染 LaunchAgent 定义。
//
// 刻意**不写 UserName**：作业装载在 gui/<uid> 域里，本来就以该用户身份运行；
// 把 UserName 写进 LaunchAgent 会被 launchd 当成"daemon 语义"（真机上会报警告）。
// 运行身份由"plist 落在谁的家目录 + 装进哪个域"决定。
func macSaberPlistContent(label string, args []string, outLog, errLog string) string {
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
	b.WriteString("    <key>WorkingDirectory</key>\n")
	fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(filepath.Dir(args[0])))
	b.WriteString("    <key>EnvironmentVariables</key>\n    <dict>\n        <key>PATH</key>\n")
	b.WriteString("        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n")
	b.WriteString("    </dict>\n")
	fmt.Fprintf(&b, "    <key>StandardOutPath</key>\n    <string>%s</string>\n", xmlEscape(outLog))
	fmt.Fprintf(&b, "    <key>StandardErrorPath</key>\n    <string>%s</string>\n", xmlEscape(errLog))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// ---------------------------------------------------------------------------
//  路径闸门：产物 / 服务 / 归档校验
// ---------------------------------------------------------------------------

// MacSaberBinaryInstalled 报告"磁盘上有可执行文件，且 `macsaber version` 真能跑"。
//
// 这是 installed 判据的**前两条证据**。刻意真跑一次 `version`：
// 文件存在但架构不对 / 被截断 / 权限不对时，装了也起不来，不能算装好。
func (m *Manager) MacSaberBinaryInstalled(ctx context.Context) (bool, string) {
	bin := MacSaberBin()
	if !fileExecutable(bin) {
		return false, fmt.Sprintf("%s 不存在或不可执行", bin)
	}
	out, err := macSaberVersionFn(m, ctx, bin)
	if err != nil {
		return false, err.Error()
	}
	return true, strings.TrimSpace(out)
}

// MacSaberVersionServing 报告"127.0.0.1:<MacSaberPort>/api/version 真的返回这一版"。
//
// 只按端口判断是不够的：端口可能被**别的**进程占着（本机历史上 frps 的 7000
// 就被隔空播放接收器抢过），那会把"别人的端口在听"当成"我们装好了"。
// 所以必须解析 JSON 里的 version 字段并与打包版本逐字比对。
func (m *Manager) MacSaberVersionServing(ctx context.Context) (bool, string) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", MacSaberPort, macSaberHealthPath)
	body, code, err := macSaberHTTPGet(ctx, url)
	if err != nil {
		return false, fmt.Sprintf("%s 取不到（%v）", url, err)
	}
	if code != 200 {
		return false, fmt.Sprintf("%s 返回 HTTP %d", url, code)
	}
	var payload struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return false, fmt.Sprintf("%s 的响应不是合法 JSON（%s）", url, tailText(strings.TrimSpace(body), 120))
	}
	if payload.Version != MacSaberVersion {
		return false, fmt.Sprintf("%s 报的版本是 %q，期望 %q", url, payload.Version, MacSaberVersion)
	}
	return true, fmt.Sprintf("%s 返回 ok=%v version=%s", url, payload.OK, payload.Version)
}

// MacSaberServeCheck 是"这个应用现在可用吗"的**唯一判据**（供测试与状态查询用）。
// 返回 (是否可用, 人话结论)。
func (m *Manager) MacSaberServeCheck(ctx context.Context) (bool, string) {
	if ok, detail := m.MacSaberBinaryInstalled(ctx); !ok {
		return false, detail
	}
	return m.MacSaberVersionServing(ctx)
}

// verifyMacSaberSHA256 校验归档的 sha256（拿不到期望值就失败，不静默跳过）。
func verifyMacSaberSHA256(path, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return fmt.Errorf("没有可用的 sha256 期望值：镜像站的 manifest.json 与代码里的声明都拿不到，" +
			"拒绝跳过内容校验（静默跳过等于把「有校验」变成「看运气」）")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取产物失败（%s）：%w", path, err)
	}
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("产物 sha256 不匹配：期望 %s，实际 %s（%s）。"+
			"镜像站上的包可能被改过或同步错了，已中止安装", want, got, path)
	}
	return nil
}

// MacSaberMirrorSHA256 从镜像站的 manifest.json 取归档的 sha256。
// 镜像站不可达 / 没有清单时返回空串（调用方回落代码里的声明值）。
func (m *Manager) MacSaberMirrorSHA256(ctx context.Context, version string) string {
	if !m.MirrorEnabled() {
		return ""
	}
	mm, err := m.fetchMirrorManifest(ctx, MacSaberAppID, version)
	if err != nil {
		return ""
	}
	return mm.sha256For(MacSaberArtifactName(version))
}

// MacSaberAssetURL 返回归档在镜像站上的地址（基址为空时返回空串 = 装不上）。
func (m *Manager) MacSaberAssetURL(version string) string {
	if !m.MirrorEnabled() {
		return ""
	}
	return m.appAssetURL(MacSaberAppID, version, MacSaberArtifactName(version))
}

// ---------------------------------------------------------------------------
//  安装
// ---------------------------------------------------------------------------

// InstallMacSaber 安装 mac军刀。
//
// 顺序（每一步都真的复核，不信"上一步退出码 0"）：
//  1. 下载归档（只从镜像站；没有上游回落源）；
//  2. sha256 校验（镜像清单优先，回落代码里的声明值）；
//  3. 解包 + `file -b` 复核 Mach-O arm64；
//  4. 以真实用户身份跑 `macsaber version`，必须报出打包版本；
//  5. 落盘 /opt/macsaber/bin/macsaber + 交还归属；
//  6. 写 LaunchAgent plist（真实用户家目录）并 bootstrap；
//  7. 登记进「服务管理」；
//  8. 验收 /api/version 真的返回这一版（**失败即 error**，不是写个 plist 就算完）。
func (m *Manager) InstallMacSaber(ctx context.Context, app App, result *InstallResult) error {
	if result != nil {
		result.App = app.ID
	}
	p := m.MacSaberPathsFor()
	if p.Plist == "" || p.DataDir == "" {
		return fmt.Errorf("无法确定运行 mac军刀 的真实用户（UserName/UserHome 为空），" +
			"它的 LaunchAgent 必须落在该用户家目录下")
	}
	if strings.TrimSpace(m.opt.UserName) == "" {
		return fmt.Errorf("无法确定运行 mac军刀 的真实用户（UserName 为空）：" +
			"它要读该用户的家目录、写该用户的 ~/MacSaberFiles，不能以 root 运行")
	}

	// ① 准备目录（面板以 root 跑，建完必须把用户那两份交还用户）。
	if err := os.MkdirAll(filepath.Dir(p.Bin), 0o755); err != nil {
		return fmt.Errorf("创建安装目录 %s 失败：%w", filepath.Dir(p.Bin), err)
	}
	for _, dir := range []string{p.DataDir, p.WriteRoot, filepath.Dir(p.Plist), filepath.Dir(p.OutLog)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败：%w", dir, err)
		}
	}
	if err := chownTree(m.opt.UserName, p.DataDir); err != nil {
		return fmt.Errorf("把数据目录交还用户 %s 失败：%w（服务以该用户运行，属主不对就写不进配置）",
			m.opt.UserName, err)
	}
	if err := chownTree(m.opt.UserName, p.WriteRoot); err != nil {
		return fmt.Errorf("把可写根交还用户 %s 失败：%w", m.opt.UserName, err)
	}
	if err := chownTree(m.opt.UserName, filepath.Dir(p.Plist)); err != nil {
		return fmt.Errorf("把 LaunchAgents 目录交还用户 %s 失败：%w（launchd 会因属主不对拒绝装载）",
			m.opt.UserName, err)
	}

	// ② 下载 + 校验 + 解包 + 复核，全部在临时目录里做完再落盘 ——
	//    校验不过就中止，绝不把坏二进制写到 /opt。
	staged, cleanupStage, err := m.macSaberStageBinary(ctx, result)
	if err != nil {
		return err
	}
	defer cleanupStage()

	// ③ 落盘 + 架构复核 + 可执行位。
	target := p.Bin + ".new"
	if err := installFileExecutable(staged, target); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", target, err)
	}
	if err := macSaberVerifyArm64(m, ctx, target); err != nil {
		_ = os.Remove(target)
		return err
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return fmt.Errorf("设置可执行位失败（%s）：%w", target, err)
	}
	// 换掉正在执行的 Mach-O 会被系统杀掉，但这时旧实例还没 bootstrap，
	// 先停一次（幂等：本来没有算成功），再原子改名。
	if err := macSaberStop(MacSaberLabel, p.Plist); err != nil {
		if result != nil {
			result.step(ctx, "提示：停止旧实例时出错（继续安装）："+err.Error())
		}
	}
	if err := os.Rename(target, p.Bin); err != nil {
		return fmt.Errorf("安装二进制到 %s 失败：%w", p.Bin, err)
	}
	if err := chownTree(m.opt.UserName, filepath.Dir(p.Bin)); err != nil {
		if result != nil {
			result.step(ctx, "警告：安装目录属主没有交还用户："+err.Error())
		}
	}

	// ④ plist + bootstrap + 登记 + 验收。
	return m.macSaberInstallService(ctx, app, p, result)
}

// macSaberStageBinary 走完"下载 → 校验 → 解包 → 架构复核 → version 复核"，
// 返回暂存好的二进制路径与清理函数。
func (m *Manager) macSaberStageBinary(ctx context.Context, result *InstallResult) (string, func(), error) {
	noop := func() {}
	version := MacSaberVersion
	asset := MacSaberArtifactName(version)
	mirrorURL := m.MacSaberAssetURL(version)
	if mirrorURL == "" {
		return "", noop, fmt.Errorf("镜像站基址为空（面板设置里的 mirror_base 没填）——"+
			"mac军刀 没有公开的上游下载源，只能从镜像站取 %s", asset)
	}
	// 离线模式（仅走镜像站）：缺件必须明确失败，绝不偷偷出网。
	// 这里本来就只有镜像一个来源，所以这一句是"把事实写出来"而不是额外限制。
	if err := m.MirrorOfflinePreflight(ctx, "mac军刀 "+version, mirrorURL); err != nil {
		return "", noop, err
	}

	stage, err := os.MkdirTemp("", "macsaber-install-")
	if err != nil {
		return "", noop, fmt.Errorf("创建临时目录失败：%w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stage) }

	archive := filepath.Join(stage, asset)
	if result != nil {
		result.step(ctx, "下载 mac军刀 "+version+"："+mirrorURL)
	}
	dctx, cancel := context.WithTimeout(ctx, macSaberDownloadTimeout)
	defer cancel()
	err = macSaberFetch(dctx, mirrorURL, archive, func(got, total int64) {
		if result == nil {
			return
		}
		if total > 0 {
			result.step(ctx, fmt.Sprintf("下载中：%.0f%%（%s / %s）",
				float64(got)*100/float64(total), humanBytes(got), humanBytes(total)))
		} else {
			result.step(ctx, "下载中："+humanBytes(got))
		}
	})
	if err != nil {
		cleanup()
		return "", noop, fmt.Errorf("下载 %s 失败：%w（mac军刀 没有公开回落源，"+
			"请确认镜像站可达：%s）", asset, err, mirrorURL)
	}

	// sha256：镜像清单优先（覆盖内容），回落代码里的**打包时实测值**。
	want := m.MacSaberMirrorSHA256(ctx, version)
	source := "镜像站 manifest.json（" + m.appManifestURL(MacSaberAppID, version) + "）"
	if want == "" {
		want = MacSaberArchiveSHA256
		source = "代码里记录的打包实测值（镜像站没有可读的 manifest.json）"
	}
	if err := verifyMacSaberSHA256(archive, want); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("%w（期望值来源：%s）", err, source)
	}
	if result != nil {
		result.step(ctx, fmt.Sprintf("SHA-256 校验通过：%s（来源：%s）", asset, source))
	}

	// 解包（归档是平铺的：macsaber + README.md，没有顶层目录，不剥层）。
	extract := filepath.Join(stage, "x")
	if err := os.MkdirAll(extract, 0o755); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("创建解包目录失败：%w", err)
	}
	if err := macSaberExtract(m, ctx, archive, extract); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("解包 %s 失败：%w", asset, err)
	}
	staged := filepath.Join(extract, "macsaber")
	if info, err := os.Stat(staged); err != nil || info.IsDir() {
		cleanup()
		return "", noop, fmt.Errorf("归档里没有 macsaber 可执行文件（%s）："+
			"请确认镜像站上的包是 tools/macsaber-release.sh 打的那一个", archive)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("设置暂存二进制的可执行位失败：%w", err)
	}
	if err := macSaberVerifyArm64(m, ctx, staged); err != nil {
		cleanup()
		return "", noop, err
	}

	// 复核"解出来的真的是这一版"（不是文件名对、内容却是别的版本）。
	out, err := macSaberVersionFn(m, ctx, staged)
	if err != nil {
		cleanup()
		return "", noop, err
	}
	if !strings.Contains(out, version) {
		cleanup()
		return "", noop, fmt.Errorf("归档里的 macsaber 报的版本不是 %s：%q。"+
			"镜像站上的包可能是别的版本，已中止安装", version, tailText(strings.TrimSpace(out), 160))
	}
	if result != nil {
		result.step(ctx, "二进制已复核："+strings.TrimSpace(out)+"（Mach-O arm64）")
	}
	return staged, cleanup, nil
}

// runRootCtx 已由 macSaberExtract 注入点承担（见文件头的仅测试注入点）。

// installFileExecutable 把一个文件复制到目标路径并给可执行位（复制到 .new 再改名，
// 避免覆盖正在运行的二进制时留下半个文件）。
func installFileExecutable(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	tmp := to + ".part"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, to); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// verifyMacSaberArm64 用 file(1) 复核二进制是 arm64。
//
// 与 steps.go 的 verify_arm64 同一条判据（铁律：不许 amd64、不许 Rosetta），
// 只是这里不走描述符执行器 —— 这个应用没有描述符。
func verifyMacSaberArm64(m *Manager, ctx context.Context, path string) error {
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return fmt.Errorf("找不到可执行文件 %s", path)
	}
	out, err := m.runRoot(ctx, 30*time.Second, "/usr/bin/file", "-b", path)
	if err != nil {
		return fmt.Errorf("复核二进制架构失败：%v（%s）", err, tailText(strings.TrimSpace(out), 200))
	}
	if !strings.Contains(out, "arm64") {
		return fmt.Errorf("下载到的 %s 不是 arm64 原生二进制（file 报告：%s）。"+
			"面板只允许原生 arm64（不跑 Rosetta 转译），已中止安装",
			filepath.Base(path), strings.TrimSpace(out))
	}
	return nil
}

// macSaberInstallService 写 plist、装载、登记、等 /api/version 真的对得上。
func (m *Manager) macSaberInstallService(ctx context.Context, app App, p MacSaberPaths, result *InstallResult) error {
	args := macSaberServeArgs(p)
	if len(args) == 0 {
		return fmt.Errorf("mac军刀 的启动参数为空（内部错误）")
	}
	content := macSaberPlistContent(MacSaberLabel, args, p.OutLog, p.ErrLog)
	tmp := p.Plist + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 plist %s 失败（面板需要以 root 运行）：%w", p.Plist, err)
	}
	// 归属必须是真实用户：launchd 对 gui 域里 plist 的属主与权限很敏感，
	// root:wheel 会被拒绝装载（"Path had bad ownership/permissions"）。
	if err := chownTree(m.opt.UserName, tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("把 plist 交还用户 %s 失败：%w", m.opt.UserName, err)
	}
	if err := os.Rename(tmp, p.Plist); err != nil {
		return fmt.Errorf("安装 plist %s 失败：%w", p.Plist, err)
	}
	if err := chownTree(m.opt.UserName, p.Plist); err != nil {
		return fmt.Errorf("修正 plist 属主失败：%w", err)
	}
	if result != nil {
		result.step(ctx, "正在注册并启动 LaunchAgent "+MacSaberLabel+
			fmt.Sprintf("（http://127.0.0.1:%d/，以 %s 身份运行）", MacSaberPort, m.opt.UserName))
	}
	if err := macSaberLaunch(MacSaberLabel, p.Plist); err != nil {
		return fmt.Errorf("启动 mac军刀 网页界面失败：%w", err)
	}
	if err := m.RegisterInstalledService(ctx, MacSaberLabel, app.Name, app.Icon, app.Category, MacSaberPort); err != nil {
		// 登记失败不该把"界面已经起来"报成安装失败，但必须如实留下警告。
		if result != nil {
			result.step(ctx, "警告：界面已启动，但登记进「服务管理」失败："+err.Error()+
				"（可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
		}
	}
	if result != nil {
		result.Steps = append(result.Steps,
			"已注册为用户级 LaunchAgent（"+MacSaberLabel+"，登录后自动启动）",
			"打开：http://127.0.0.1:"+fmt.Sprint(MacSaberPort)+"/ ，或从「应用市场 → mac军刀」点「打开」走面板别名 /"+
				MacSaberSlug+"/",
			"数据目录："+p.DataDir+"（卸载时默认保留；配置与审计日志都在这里）",
			"可写根："+p.WriteRoot+"（工具产物落在这里；卸载时默认保留）",
		)
	}
	return m.waitMacSaberReady(ctx, p, result)
}

// waitMacSaberReady 等 /api/version 真的返回打包版本。
//
// 为什么不是"端口在听就算成功"（AGENTS 第三节）：端口可能被别的进程占着，
// 而"plist 写出来了"更是完全不能说明服务起来了 —— 僵尸 plist 正是本仓库
// 谎报已装的根因（坑 161）。所以判据是**内容**：JSON 里的 version 字段。
func (m *Manager) waitMacSaberReady(ctx context.Context, p MacSaberPaths, result *InstallResult) error {
	deadline := time.Now().Add(macSaberReadyTimeout)
	var last string
	for time.Now().Before(deadline) {
		ok, detail := m.MacSaberVersionServing(ctx)
		last = detail
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(600 * time.Millisecond):
		}
	}
	if last == "" {
		last = "没有任何可观测的就绪迹象"
	}
	return assertReady(ctx, readySpec{
		What:    "mac军刀 网页界面",
		Expect:  fmt.Sprintf("127.0.0.1:%d%s 在 %s 内返回 version=%s", MacSaberPort, macSaberHealthPath, macSaberReadyTimeout, MacSaberVersion),
		Timeout: macSaberReadyTimeout,
		Probe: func(context.Context) readyVerdict {
			ok, detail := m.MacSaberVersionServing(ctx)
			if ok {
				return readyVerdict{OK: true, Actual: "已就绪：" + detail}
			}
			return readyVerdict{Actual: last}
		},
		LogPath: p.ErrLog,
		State: "二进制已落盘并复核（" + p.Bin + "），LaunchAgent " + MacSaberLabel +
			" 已 bootstrap，服务已登记进「服务管理」",
		Missing: "但网页界面没有返回打包的那一版，市场里的「打开」会打不开",
		Remedy: "在「服务管理 → mac军刀」里点「重启服务」再试；仍然失败请看下面的日志尾部" +
			"（常见原因：" + fmt.Sprint(MacSaberPort) + " 端口被别的进程占用、plist 属主不对导致 launchd 拒绝装载）",
		Result: result,
	})
}

// ---------------------------------------------------------------------------
//  卸载
// ---------------------------------------------------------------------------

// UninstallMacSaber 卸载：bootout + 删 plist + 删安装根。
//
// 数据目录（配置里是口令哈希、审计日志里是操作记录）与可写根（工具产物）
// **默认保留**并如实告知路径 —— 用户以为卸载清干净了、结果密钥还在磁盘上，
// 是真实的安全问题；反过来"没问就删掉用户的文件"更糟。
func (m *Manager) UninstallMacSaber(ctx context.Context, app App, removeData bool, result *InstallResult) error {
	p := m.MacSaberPathsFor()
	hasPlist := p.Plist != "" && fileExists(p.Plist)
	hasRecord := false
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if s.LaunchLabel == MacSaberLabel {
					hasRecord = true
					break
				}
			}
		}
	}
	if hasPlist || hasRecord {
		if result != nil {
			result.step(ctx, "停止并删除 LaunchAgent "+MacSaberLabel)
		}
		if err := macSaberStop(MacSaberLabel, p.Plist); err != nil {
			return fmt.Errorf("停止 mac军刀 网页界面失败：%w", err)
		}
	} else if result != nil {
		result.step(ctx, "mac军刀 的服务本来就没有注册，跳过停止")
	}
	// plist 兜底再删一次：macSaberStop 只负责"停止"，文件必须真的没有。
	if p.Plist != "" {
		if err := os.Remove(p.Plist); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 plist %s 失败：%w", p.Plist, err)
		}
	}
	// 安装根（二进制）总是删 —— 留着它就是一个"看起来已安装"的假象。
	root := p.Root
	if root == "" {
		root = MacSaberInstallRoot
	}
	if err := m.removeTree(ctx, root, result); err != nil {
		return err
	}
	if removeData {
		for _, dir := range []string{p.DataDir, p.WriteRoot} {
			if dir == "" {
				continue
			}
			if result != nil {
				result.step(ctx, "按你的选择删除 "+dir)
			}
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("删除 %s 失败：%w", dir, err)
			}
		}
		if result != nil {
			result.step(ctx, "数据与产物已删除（账号、审计日志、~/MacSaberFiles 里的文件都不再保留）")
		}
	} else if result != nil {
		result.step(ctx, "按你的选择**保留**数据目录 "+p.DataDir+
			"（账号与审计日志）与可写根 "+p.WriteRoot+"（工具产物）；要一并清理请勾选「同时删除数据」")
	}
	if result != nil {
		result.step(ctx, "提示：mac军刀 不会在系统里留别的东西（不写 /Library、不装系统扩展）")
	}
	return nil
}

// macSaberInstallPlan 生成卸载计划（确认框用；只读，不产生任何改动）。
func (m *Manager) macSaberInstallPlan() UninstallPlan {
	p := m.MacSaberPathsFor()
	steps := []string{
		"停止并删除 LaunchAgent " + MacSaberLabel + "（" + p.Plist + "）",
		"删除安装目录 " + p.Root + "（二进制）",
		"从「服务管理」移除记录",
		"⚠️ 卸载后网页界面与面板别名 /" + MacSaberSlug + "/ 都会不可用，直到重新安装",
	}
	plan := UninstallPlan{Kind: "installer", Steps: steps}
	if p.DataDir != "" {
		plan.DataPaths = append(plan.DataPaths, p.DataDir)
	}
	if p.WriteRoot != "" {
		plan.DataPaths = append(plan.DataPaths, p.WriteRoot)
	}
	plan.KeepNote = "默认保留数据目录 " + p.DataDir + "（里面有账号口令哈希与审计日志）与可写根 " +
		p.WriteRoot + "（工具产物，可能就是你要处理的文件）；" +
		"要一并删除请在确认框里勾选「同时删除数据」"
	return plan
}

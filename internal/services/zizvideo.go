package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
//  zizvideo（短视频服务）的安装 / 卸载
//
//  zizvideo 是**本仓库自己的模块**（仓库 zizvideo/，module github.com/zizdog/zizvideo）。
//  自 2026-09-22 起**不随面板发布包分发**（面板包瘦身）：安装时按应用级索引
//  `apps/zizvideo/manifest.json` 给出的 version/asset/sha256 从镜像站按需下载；
//  读不到索引才回落到本文件里的常量（已知可用兜底，不再是发布前置条件）；
//  本地开发（make dev）仍可用面板二进制旁边的 <bin>/zizvideo 直接装。
//  它由面板托管：系统级 LaunchDaemon
//  cn.zizpanel.zizvideo 执行 `<面板二进制> zizvideo-supervise`，supervisor 以 root
//  fork 后 setuid 到真实用户跑 zizvideo —— 与面板同一代码要求 ⇒ 与面板**共用文件
//  权限**（AGENTS 铁律 12、docs/坑清单.md 202）。
//
//  判据贴着**运行体**：二进制在 + `zizvideo --version` 对得上 + 127.0.0.1:7766/healthz
//  真的返回 ok，三条都过才算装好（"plist 写出来了"不算 —— 坑 161）。
//
//  媒体根是用户自己的数据，**永远不进卸载的 DataPaths**；数据目录
//  （DB、封面、config.json）默认保留。
// ============================================================================

// ---------------------------------------------------------------------------
//  仅测试用的注入点（单测绝不联网、绝不碰 launchd、绝不写 /opt 与真实家目录）
// ---------------------------------------------------------------------------

var (
	// ZizvideoInstallRoot 是安装根（默认 /opt/zizvideo；单测指向 t.TempDir()）。
	ZizvideoInstallRoot = "/opt/zizvideo"
	// zizvideoPanelExecutable 返回面板自身的可执行文件路径（系统守护进程执行它）。
	zizvideoPanelExecutable = os.Executable
	// zizvideoSourceBin 由面板二进制路径推导"本地开发/旧发布包"里的 zizvideo：
	// 它就在面板二进制旁边（<bin>/zizvideo）。正式发布包**不再携带**它。
	zizvideoSourceBin = func(panelBin string) string {
		return filepath.Join(filepath.Dir(panelBin), "zizvideo")
	}
	// zizvideoFetch 下载指定版本的 zizvideo 产物（默认 curl；单测注入假实现，绝不联网）。
	// 版本不进 label：真实版本来自运行时索引，写死常量会与索引说的版本不符。
	zizvideoFetch = func(m *Manager, ctx context.Context, url, dst string, result *InstallResult) error {
		_, err := m.downloadToFile(ctx, url, dst, 5*time.Minute, result, "下载 zizvideo 应用包")
		return err
	}
	// defaultZizvideoFetch / defaultZizvideoExpectedSHA256 保存默认实现（单测替换后恢复）。
	defaultZizvideoFetch = zizvideoFetch
	// zizvideoExpectedSHA256 返回镜像没有 manifest.json 时的期望 sha256（单测注入）。
	zizvideoExpectedSHA256        = func() string { return ZizvideoBinarySHA256 }
	defaultZizvideoExpectedSHA256 = zizvideoExpectedSHA256
	// zizvideoLaunch 在 system 域装载 supervisor。
	zizvideoLaunch = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.bootstrapService(ctx, label, plist)
	}
	// zizvideoStop 停止并删除服务（幂等：本来没有也算成功）。
	zizvideoStop = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.stopLaunchdService(ctx, label, plist)
	}
	// zizvideoVersionFn 执行 `zizvideo --version`（以真实用户身份）。
	zizvideoVersionFn = func(m *Manager, ctx context.Context, bin string) (string, error) {
		out, err := m.runAsUser(ctx, 30*time.Second, bin, "--version")
		if err != nil {
			return out, fmt.Errorf("执行 %s --version 失败：%w（%s）", bin, err, tailText(strings.TrimSpace(out), 200))
		}
		return out, nil
	}
	// zizvideoArm64Fn 用 file(1) 复核原生 arm64。
	zizvideoArm64Fn = func(m *Manager, ctx context.Context, path string) error {
		return verifyZizvideoArm64(m, ctx, path)
	}
	// zizvideoHTTPGet 取一个 HTTP 端点（默认 curl；单测注入假实现，绝不联网）。
	zizvideoHTTPGet = func(ctx context.Context, url string) (body string, code int, err error) {
		return runCurlCtx(ctx, url, 4)
	}
	// zizvideoRemovedWait / zizvideoRemovedPoll 是卸载终态复核的等待参数。
	zizvideoRemovedWait = 5 * time.Second
	zizvideoRemovedPoll = 300 * time.Millisecond
)

const (
	// ZizvideoAppID 是应用标识（面板托管模块唯一名）。
	ZizvideoAppID = "zizvideo"
	// ZizvideoLabel 是系统级守护进程的 launchd 标签（面板托管 ⇒ 用面板的域前缀）。
	ZizvideoLabel = "cn.zizpanel.zizvideo"
	// ZizvideoVersion 是镜像索引读不到时的兜底版本（不再要求跟着 zizvideo 发版更新）。
	ZizvideoVersion = "0.1.1-mvp"
	// ZizvideoBinarySHA256 是兜底版本 arm64 产物的 sha256（已知可用）；运行期以
	// 镜像索引 apps/zizvideo/manifest.json 的 sha256 为准，这个常量只在读不到索引时用。
	ZizvideoBinarySHA256 = "d639e133be7f9db9245b7be03a299aff0cc0948071445ae085fb2182c6bfe42a"
	// ZizvideoPort 是网页界面端口（只绑 127.0.0.1）。
	ZizvideoPort = 7766
	// zizvideoHealthPath 是公开的就绪端点，进程活着就返回 ok。
	zizvideoHealthPath = "/healthz"
	// zizvideoReadyTimeout 是等网页界面就绪的上限。
	zizvideoReadyTimeout = 60 * time.Second
)

// ZizvideoArtifactName 是产物命名约定（臂架构；索引里的 name 才是运行时真源）。
func ZizvideoArtifactName(ver string) string {
	return "zizvideo_" + ver + "_darwin_arm64"
}

// zizvideoIndex 是镜像站上的**应用级索引** apps/zizvideo/manifest.json：
// version/asset/sha256 的事实来源；改 zizvideo 只需更新它 + 传产物，不必再发面板版本。
type zizvideoIndex struct {
	App    string               `json:"app"`
	Latest string               `json:"latest"`
	Assets []zizvideoIndexAsset `json:"assets"`
}

type zizvideoIndexAsset struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// zizvideoRelease 是本次安装/刷新要用的事实。FromIndex=false 表示索引不可用、用了面板常量兜底。
type zizvideoRelease struct {
	Version   string
	Asset     string
	SHA256    string
	FromIndex bool
}

// builtinZizvideoRelease 是"已知可用兜底"：走 zizvideoExpectedSHA256 变量以便单测注入。
func builtinZizvideoRelease() zizvideoRelease {
	return zizvideoRelease{
		Version: ZizvideoVersion,
		Asset:   ZizvideoArtifactName(ZizvideoVersion),
		SHA256:  zizvideoExpectedSHA256(),
	}
}

// zizvideoVersionSource 给报错文案说明版本从哪来（索引 / 面板内置）。
func zizvideoVersionSource(rel zizvideoRelease) string {
	if rel.FromIndex {
		return "镜像索引"
	}
	return "面板内置版本"
}

// zizvideoIndexURL 拼应用级索引地址：<mirror_base>/apps/zizvideo/manifest.json。
func (m *Manager) zizvideoIndexURL() string {
	return strings.Join([]string{m.mirrorBase(), mirrorAppsDir, ZizvideoAppID, mirrorManifestName}, "/")
}

// fetchZizvideoIndex 读应用级索引（≤1 MB、短超时）。失败即"索引不可用"，由调用方决定兜底或如实失败。
func (m *Manager) fetchZizvideoIndex(ctx context.Context) (*zizvideoIndex, error) {
	if !m.MirrorEnabled() {
		return nil, fmt.Errorf("面板没有配置应用包镜像基址（设置 → 应用包镜像），读不到镜像索引")
	}
	u := m.zizvideoIndexURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("镜像索引地址不合法（%s）：%w", u, err)
	}
	cl := &http.Client{Timeout: 2 * m.mirrorProbeTimeout()}
	res, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("取镜像索引失败（%s）：%w", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("镜像索引不可用（%s，HTTP %d）", u, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读镜像索引失败（%s）：%w", u, err)
	}
	var idx zizvideoIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("镜像索引不是合法 JSON（%s）：%w", u, err)
	}
	return &idx, nil
}

// resolveZizvideoRelease 解析"这一版该装哪个 zizvideo"（镜像索引优先）。
// 读不到索引返回 error：下载方回落常量，刷新方如实失败（绝不谎报"已是最新"）。
func (m *Manager) resolveZizvideoRelease(ctx context.Context) (zizvideoRelease, error) {
	idx, err := m.fetchZizvideoIndex(ctx)
	if err != nil {
		return zizvideoRelease{}, err
	}
	ver := strings.TrimSpace(idx.Latest)
	if ver == "" {
		return zizvideoRelease{}, fmt.Errorf("镜像索引 %s 里没有 latest 版本", m.zizvideoIndexURL())
	}
	asset, sha := "", ""
	for _, a := range idx.Assets {
		if strings.TrimSpace(a.Version) != ver {
			continue
		}
		name := strings.TrimSpace(a.Name)
		// 面板只支持原生 arm64；索引没写 arch 时按产物名里的 darwin_arm64 认。
		if a.Arch == "arm64" || (a.Arch == "" && strings.Contains(name, "darwin_arm64")) {
			asset, sha = name, strings.ToLower(strings.TrimSpace(a.SHA256))
			break
		}
	}
	if asset == "" {
		return zizvideoRelease{}, fmt.Errorf("镜像索引 %s 里没有 %s 的 arm64 产物", m.zizvideoIndexURL(), ver)
	}
	if !isSHA256Hex(sha) {
		return zizvideoRelease{}, fmt.Errorf("镜像索引 %s 里 %s 的 sha256 不是 64 位十六进制（%q）：拒绝安装无法校验的二进制",
			m.zizvideoIndexURL(), asset, sha)
	}
	return zizvideoRelease{Version: ver, Asset: asset, SHA256: sha, FromIndex: true}, nil
}

// isSHA256Hex 判断是不是 64 位小写/大写十六进制（索引给坏值时必须拒绝，不能装无法校验的包）。
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// ZizvideoPaths 是一次安装要落盘的全部位置。
type ZizvideoPaths struct {
	// Home 是真实用户家目录（子进程的 HOME 与数据目录的根）。
	Home string
	// Root 是安装根（/opt/zizvideo）。
	Root string
	// Bin 是可执行文件（/opt/zizvideo/bin/zizvideo）。
	Bin string
	// Plist 是系统级 LaunchDaemon 定义（/Library/LaunchDaemons/）。
	Plist string
	// DataDir 是数据目录（DB、封面、config.json）—— 卸载时**默认保留**。
	DataDir string
	// ConfigPath 是 zizvideo 读的 config.json（数据目录下）。
	ConfigPath string
	// LogDir 是日志目录（~/Library/Logs）。
	LogDir string
	// OutLog / ErrLog 是 zizvideo 子进程的标准输出/错误。
	OutLog string
	ErrLog string
	// SuperviseOutLog / SuperviseErrLog 是 supervisor 自身的标准输出/错误。
	SuperviseOutLog string
	SuperviseErrLog string
	// SuperviseLog 是 supervisor 自己写的最少量日志。
	SuperviseLog string
}

// ZizvideoPathsFor 按当前 Manager 的用户信息解析全部路径。
func (m *Manager) ZizvideoPathsFor() ZizvideoPaths {
	p := ZizvideoPaths{Root: strings.TrimSpace(ZizvideoInstallRoot)}
	if p.Root == "" {
		p.Root = "/opt/zizvideo"
	}
	p.Bin = filepath.Join(p.Root, "bin", "zizvideo")
	p.Plist = SystemDaemonPlistPath(ZizvideoLabel)
	home := m.zizvideoHome()
	if home == "" {
		return p
	}
	p.Home = home
	p.DataDir = filepath.Join(home, "Library", "Application Support", "zizvideo")
	p.ConfigPath = filepath.Join(p.DataDir, "config.json")
	p.LogDir = filepath.Join(home, "Library", "Logs")
	p.OutLog = filepath.Join(p.LogDir, "zizvideo.out.log")
	p.ErrLog = filepath.Join(p.LogDir, "zizvideo.err.log")
	p.SuperviseOutLog = filepath.Join(p.LogDir, "zizvideo-supervise.out.log")
	p.SuperviseErrLog = filepath.Join(p.LogDir, "zizvideo-supervise.err.log")
	p.SuperviseLog = filepath.Join(p.LogDir, "zizvideo-supervise.log")
	return p
}

// zizvideoHome 返回真实用户家目录（拿不到返回空串，调用方必须如实失败）。
func (m *Manager) zizvideoHome() string {
	if h := strings.TrimSpace(m.opt.UserHome); h != "" {
		return h
	}
	if u := strings.TrimSpace(m.opt.UserName); u != "" {
		return "/Users/" + u
	}
	return ""
}

// ZizvideoBin 返回可执行文件路径（给"安装体"判定与测试用）。
func ZizvideoBin() string {
	root := strings.TrimSpace(ZizvideoInstallRoot)
	if root == "" {
		root = "/opt/zizvideo"
	}
	return filepath.Join(root, "bin", "zizvideo")
}

// zizvideoSourceBinary 返回要装进去的二进制路径与解析出的事实：本地携带位版本对得上就用它，
// 否则按镜像索引（读不到则用面板常量）从镜像站下载；两条都没有就如实失败（不编地址）。
func (m *Manager) zizvideoSourceBinary(ctx context.Context, panelBin string, result *InstallResult) (string, zizvideoRelease, error) {
	rel, err := m.resolveZizvideoRelease(ctx)
	if err != nil {
		rel = builtinZizvideoRelease()
		if result != nil {
			result.step(ctx, "镜像索引不可用，用面板内置版本 "+rel.Version+"（"+err.Error()+"）")
		}
	}
	if f := strings.TrimSpace(zizvideoSourceBin(panelBin)); f != "" && fileExecutable(f) {
		// 升级上来的机器会残留旧发布包放下的携带位：与解析到的版本对不上就改走镜像站
		// 按需下载（否则会一直装旧构建，坑 216 的翻版）。
		if out, verr := zizvideoVersionFn(m, ctx, f); verr == nil && strings.Contains(out, rel.Version) {
			return f, rel, nil
		} else if result != nil {
			result.step(ctx, "本地的 "+f+" 不是 zizvideo "+rel.Version+"，改用镜像站按需下载")
		}
	}
	src, derr := m.downloadZizvideoRelease(ctx, rel, result)
	return src, rel, derr
}

// downloadZizvideoBinary 供模块刷新用：先解析索引，读不到才回落面板常量（并如实写一句）。
func (m *Manager) downloadZizvideoBinary(ctx context.Context, result *InstallResult) (string, error) {
	rel, err := m.resolveZizvideoRelease(ctx)
	if err != nil {
		rel = builtinZizvideoRelease()
		if result != nil {
			result.step(ctx, "镜像索引不可用，用面板内置版本 "+rel.Version+"（"+err.Error()+"）")
		}
	}
	return m.downloadZizvideoRelease(ctx, rel, result)
}

// downloadZizvideoRelease 按解析到的 version/asset/sha256 从镜像站取件：先核 sha256，
// 再交给调用方复核架构与版本。拿不到可信 sha256 就**拒绝安装**无法校验的二进制。
func (m *Manager) downloadZizvideoRelease(ctx context.Context, rel zizvideoRelease, result *InstallResult) (string, error) {
	if !m.MirrorEnabled() {
		return "", fmt.Errorf("zizvideo %s 不再随面板包分发，需要从应用包镜像下载，"+
			"但面板没有配置镜像基址（设置 → 应用包镜像）；"+
			"也可以把二进制放到面板二进制旁边（<面板二进制目录>/zizvideo）再装", rel.Version)
	}
	url := m.appAssetURL(ZizvideoAppID, rel.Version, rel.Asset)
	if err := m.MirrorOfflinePreflight(ctx, "zizvideo "+rel.Version, url); err != nil {
		return "", err
	}
	if result != nil {
		result.step(ctx, "按镜像索引安装 zizvideo "+rel.Version+"（"+rel.Asset+"）")
	}
	want := rel.SHA256
	if !rel.FromIndex {
		// 兜底路径仍兼容旧的"每版本目录 manifest.json"：有就用，没有才用面板常量。
		if mm, err := m.fetchMirrorManifest(ctx, ZizvideoAppID, rel.Version); err == nil {
			if sum := mm.sha256For(rel.Asset); sum != "" {
				want = sum
			}
		} else if result != nil {
			result.step(ctx, "提示：镜像上没有 "+rel.Asset+" 的 manifest.json，改用面板内置的 sha256 校验（"+err.Error()+"）")
		}
	}
	dst := filepath.Join(strings.TrimSpace(m.opt.WorkDir), "zizvideo-download", rel.Asset)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("创建下载目录 %s 失败：%w", filepath.Dir(dst), err)
	}
	if err := zizvideoFetch(m, ctx, url, dst, result); err != nil {
		return "", err
	}
	if want == "" {
		_ = os.Remove(dst)
		return "", fmt.Errorf("镜像索引没有 %s 的 sha256，面板内置常量也为空："+
			"拒绝安装一个无法校验的二进制", rel.Asset)
	}
	if got := sha256OfFile(dst); got != want {
		_ = os.Remove(dst)
		return "", fmt.Errorf("%s 的 sha256 不匹配：期望 %s，实际 %s（文件已删除；"+
			"镜像上的包与索引声明不一致，请重新同步 apps/zizvideo）", rel.Asset, want, got)
	}
	// curl 以 root 下载默认是 0644：没有执行位时，紧接着"以真实用户跑 --version"会报
	// `command not found`（2026-09-22 真机就是这么失败的）。
	if err := os.Chmod(dst, 0o755); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("设置可执行位失败（%s）：%w", dst, err)
	}
	return dst, nil
}

// ZizvideoServeArgs 返回 zizvideo 的启动参数（**不含可执行文件本身**）。
//
// 参数以 zizvideo/cmd/server/main.go 为准（别照文档猜）：它没有 serve 子动词，
// 直接 `zizvideo --config <config.json>` 启动服务。
func ZizvideoServeArgs(configPath string) []string {
	return []string{"--config", configPath}
}

// ZizvideoSuperviseArgs 拼出系统 plist 里的 ProgramArguments：
// **面板自己的二进制** + zizvideo-supervise + 真实用户与它的全部路径。
func ZizvideoSuperviseArgs(panelBin, userName string, p ZizvideoPaths) []string {
	return []string{
		panelBin, "zizvideo-supervise",
		"--user", userName,
		"--zizvideo", p.Bin,
		"--config", p.ConfigPath,
		"--home", p.Home,
		"--listen", fmt.Sprintf("127.0.0.1:%d", ZizvideoPort),
		"--log-dir", p.LogDir,
	}
}

// zizvideoPlistContent 渲染**系统级 LaunchDaemon** 定义。
//
// 刻意**不写 UserName**：这个作业必须以 root 运行 —— supervisor 要 fork 之后
// setuid 降权到真实用户（见 docs/坑清单.md 202）。
func zizvideoPlistContent(label string, args []string, outLog, errLog string) string {
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

// verifyZizvideoArm64 用 file(1) 复核二进制是 arm64（铁律：不许 amd64、不许 Rosetta）。
func verifyZizvideoArm64(m *Manager, ctx context.Context, path string) error {
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return fmt.Errorf("找不到可执行文件 %s", path)
	}
	out, err := m.runRoot(ctx, 30*time.Second, "/usr/bin/file", "-b", path)
	if err != nil {
		return fmt.Errorf("复核二进制架构失败：%v（%s）", err, tailText(strings.TrimSpace(out), 200))
	}
	if !strings.Contains(out, "arm64") {
		return fmt.Errorf("安装的 zizvideo 不是 arm64 原生二进制（file 报告：%s）。"+
			"面板只允许原生 arm64（不跑 Rosetta 转译），已中止安装", strings.TrimSpace(out))
	}
	return nil
}

// ---------------------------------------------------------------------------
//  就绪判据
// ---------------------------------------------------------------------------

// ZizvideoBinaryInstalled 报告"磁盘上有可执行文件，且 `zizvideo --version` 真能跑"。
func (m *Manager) ZizvideoBinaryInstalled(ctx context.Context) (bool, string) {
	bin := ZizvideoBin()
	if !fileExecutable(bin) {
		return false, fmt.Sprintf("%s 不存在或不可执行", bin)
	}
	out, err := zizvideoVersionFn(m, ctx, bin)
	if err != nil {
		return false, err.Error()
	}
	return true, strings.TrimSpace(out)
}

// ZizvideoServing 报告"127.0.0.1:7766/healthz 真的返回 ok"。
//
// 只按端口判断不够：端口可能被别的进程占着（历史上 frps 的 7000 被隔空播放
// 接收器抢过）。健康端点返回 ok 才说明是我们这个服务在应答。
func (m *Manager) ZizvideoServing(ctx context.Context) (bool, string) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", ZizvideoPort, zizvideoHealthPath)
	body, code, err := zizvideoHTTPGet(ctx, url)
	if err != nil {
		return false, fmt.Sprintf("%s 取不到（%v）", url, err)
	}
	if code != 200 {
		return false, fmt.Sprintf("%s 返回 HTTP %d", url, code)
	}
	if !strings.Contains(strings.ToLower(strings.TrimSpace(body)), "ok") {
		return false, fmt.Sprintf("%s 的应答不是 ok（%s）", url, tailText(strings.TrimSpace(body), 120))
	}
	return true, fmt.Sprintf("%s 返回 ok", url)
}

// ZizvideoReady 是"这个模块现在可用吗"的**唯一判据**（供测试与状态查询用）。
func (m *Manager) ZizvideoReady(ctx context.Context) (bool, string) {
	if ok, detail := m.ZizvideoBinaryInstalled(ctx); !ok {
		return false, detail
	}
	return m.ZizvideoServing(ctx)
}

// ---------------------------------------------------------------------------
//  更新检查（"装了但镜像上有更新吗"；只在用户要看时才探测一次）
// ---------------------------------------------------------------------------

// ZizvideoUpdateCheck 是"已装版本 vs 镜像最新版本"的结论。
//
// Unknown=true 表示**没拿到可信的期望值**（索引不可达 / 已装模块跑不起来 /
// 版本号解析不了）：此时调用方既不能说"有更新"、也不能说"已是最新"。
type ZizvideoUpdateCheck struct {
	App             string `json:"app"`
	Installed       string `json:"installed"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Unknown         bool   `json:"unknown"`
	CheckedAt       string `json:"checked_at"`
	Error           string `json:"error"`
}

// CheckZizvideoUpdate 按需探测一次：先跑已装模块的 --version，再读镜像索引比对。
//
// 未安装（二进制不在）⇒ Installed 为空，前端据此不给徽标（市场本来显示「安装」）；
// 其余拿不到可信值的路径一律 Unknown，**绝不假装**有更新或已最新（铁律 11）。
func (m *Manager) CheckZizvideoUpdate(ctx context.Context) ZizvideoUpdateCheck {
	out := ZizvideoUpdateCheck{App: ZizvideoAppID, CheckedAt: time.Now().Format(time.RFC3339)}
	bin := m.ZizvideoPathsFor().Bin
	if !fileExecutable(bin) {
		return out
	}
	raw, err := zizvideoVersionFn(m, ctx, bin)
	if err != nil {
		out.Unknown, out.Error = true, "读不到已装版本："+err.Error()
		return out
	}
	if out.Installed = installedVersionFromOutput(raw); out.Installed == "" {
		out.Unknown, out.Error = true,
			"已装模块的 --version 输出里没有版本号（"+tailText(strings.TrimSpace(raw), 120)+"）"
		return out
	}
	rel, err := m.resolveZizvideoRelease(ctx)
	if err != nil {
		out.Unknown, out.Error = true, "读不到镜像上的最新版本："+err.Error()
		return out
	}
	out.Latest = rel.Version
	cmp, ok := compareDottedVersions(out.Installed, out.Latest)
	if !ok {
		out.Unknown, out.Error = true,
			"版本号无法比较（已装 "+out.Installed+"，镜像 "+out.Latest+"）"
		return out
	}
	out.UpdateAvailable = cmp < 0
	return out
}

// installedVersionFromOutput 从 `zizvideo --version` 的输出里取版本号
// （"zizvideo 0.1.1-mvp" → "0.1.1-mvp"）；找不到就返回空串。
func installedVersionFromOutput(out string) string {
	for _, f := range strings.Fields(out) {
		if _, ok := numericVersionParts(f); ok {
			return strings.TrimPrefix(f, "v")
		}
	}
	return ""
}

// compareDottedVersions 比对点分数字版本（忽略 -/+ 后缀）：返回 -1/0/1 与"可比较"标志。
// 解析不了就返回 false —— 调用方据此报 unknown，而不是猜一个大小。
func compareDottedVersions(a, b string) (int, bool) {
	na, oka := numericVersionParts(a)
	nb, okb := numericVersionParts(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < len(na) || i < len(nb); i++ {
		x, y := 0, 0
		if i < len(na) {
			x = na[i]
		}
		if i < len(nb) {
			y = nb[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// numericVersionParts 把 "v0.1.1-mvp" 拆成 [0,1,1]；有任何非数字段就报 false。
func numericVersionParts(v string) ([]int, bool) {
	core := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	if core == "" {
		return nil, false
	}
	parts := strings.Split(core, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// ---------------------------------------------------------------------------
//  安装
// ---------------------------------------------------------------------------

// InstallZizvideo 安装 zizvideo（面板托管模块）。
//
// 顺序（每一步都真的复核，不信"上一步退出码 0"）：
//  1. 准备目录（安装根 root 所有；数据/日志目录交还真实用户）；
//  2. 取件（本地携带位版本对得上，或按镜像索引下载并核 sha256）、复核 arm64 与版本；
//  3. 落盘 /opt/zizvideo/bin/zizvideo；
//  4. 写系统级 plist（指向 `zizpanel zizvideo-supervise`）并 bootstrap；
//  5. 登记进「服务管理」；
//  6. 验收 /healthz 真的返回 ok（失败即 error）。
func (m *Manager) InstallZizvideo(ctx context.Context, app App, result *InstallResult) error {
	if result != nil {
		result.App = app.ID
	}
	if strings.TrimSpace(m.opt.UserName) == "" {
		return fmt.Errorf("无法确定运行 zizvideo 的真实用户（UserName 为空）：" +
			"它要写该用户的数据目录，不能以 root 运行")
	}
	p := m.ZizvideoPathsFor()
	if p.Home == "" || p.DataDir == "" || p.ConfigPath == "" {
		return fmt.Errorf("无法确定 zizvideo 的运行位置（UserHome/UserName 为空）：" +
			"数据目录、config.json 与 plist 都要从真实用户推导")
	}
	panelBin, err := zizvideoPanelExecutable()
	if err != nil || strings.TrimSpace(panelBin) == "" {
		return fmt.Errorf("找不到面板自身的可执行文件路径（系统守护进程要用它执行 zizvideo-supervise）: %v", err)
	}
	source, rel, err := m.zizvideoSourceBinary(ctx, panelBin, result)
	if err != nil {
		return err
	}
	if err := zizvideoArm64Fn(m, ctx, source); err != nil {
		return err
	}
	out, err := zizvideoVersionFn(m, ctx, source)
	if err != nil {
		return err
	}
	if !strings.Contains(out, rel.Version) {
		return fmt.Errorf("zizvideo 报的版本不是 %s（期望来自%s）：%q",
			rel.Version, zizvideoVersionSource(rel), tailText(strings.TrimSpace(out), 160))
	}

	// ① 目录：安装根保持 root（系统安装）；用户那两份建好并交还用户。
	if err := os.MkdirAll(filepath.Dir(p.Bin), 0o755); err != nil {
		return fmt.Errorf("创建安装目录 %s 失败：%w", filepath.Dir(p.Bin), err)
	}
	for _, dir := range []string{p.DataDir, p.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败：%w", dir, err)
		}
	}
	for _, dir := range []string{p.DataDir, p.LogDir} {
		if err := chownTree(m.opt.UserName, dir); err != nil {
			return fmt.Errorf("把 %s 交还用户 %s 失败：%w（服务以该用户运行，属主不对就写不进东西）",
				dir, m.opt.UserName, err)
		}
	}
	// config.json 只在**不存在**时补一份空配置：绝不覆盖用户已有配置。
	if _, err := os.Stat(p.ConfigPath); os.IsNotExist(err) {
		if werr := os.WriteFile(p.ConfigPath, []byte("{}\n"), 0o600); werr != nil {
			return fmt.Errorf("写入默认配置 %s 失败：%w", p.ConfigPath, werr)
		}
		if cerr := chownTree(m.opt.UserName, p.ConfigPath); cerr != nil {
			return fmt.Errorf("把 %s 交还用户 %s 失败：%w", p.ConfigPath, m.opt.UserName, cerr)
		}
	}

	// ② 落盘：复制到 .new → 架构/版本已复核 → 原子改名。
	target := p.Bin + ".new"
	if err := installFileExecutable(source, target); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", target, err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return fmt.Errorf("设置可执行位失败（%s）：%w", target, err)
	}
	if err := zizvideoStop(m, ctx, ZizvideoLabel, p.Plist); err != nil && result != nil {
		result.step(ctx, "提示：停止旧实例时出错（继续安装）："+err.Error())
	}
	if err := os.Rename(target, p.Bin); err != nil {
		return fmt.Errorf("安装二进制到 %s 失败：%w", p.Bin, err)
	}

	// ③ 系统 plist + bootstrap + 登记 + 验收。
	return m.zizvideoInstallService(ctx, app, p, panelBin, result)
}

// zizvideoInstallService 写系统级 plist、装载、登记，并等 /healthz 返回 ok。
func (m *Manager) zizvideoInstallService(ctx context.Context, app App, p ZizvideoPaths, panelBin string, result *InstallResult) error {
	args := ZizvideoSuperviseArgs(panelBin, m.opt.UserName, p)
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("zizvideo 的启动参数为空（内部错误）")
	}
	content := zizvideoPlistContent(ZizvideoLabel, args, p.SuperviseOutLog, p.SuperviseErrLog)
	if err := os.MkdirAll(filepath.Dir(p.Plist), 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败：%w", filepath.Dir(p.Plist), err)
	}
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
		result.step(ctx, "正在注册并启动系统级守护进程 "+ZizvideoLabel+
			fmt.Sprintf("（可执行文件是面板自己：%s zizvideo-supervise，服务以 %s 身份运行）",
				panelBin, m.opt.UserName))
	}
	if err := zizvideoLaunch(m, ctx, ZizvideoLabel, p.Plist); err != nil {
		return fmt.Errorf("启动 zizvideo 失败：%w", err)
	}
	if err := m.RegisterInstalledService(ctx, ZizvideoLabel, app.Name, app.Icon, app.Category, ZizvideoPort); err != nil {
		if result != nil {
			result.step(ctx, "警告：服务已启动，但登记进「服务管理」失败："+err.Error()+
				"（可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
		}
	}
	if result != nil {
		result.Steps = append(result.Steps,
			"已注册为系统级守护进程（"+ZizvideoLabel+"，与面板共用文件权限，开机自启）",
			"打开：http://127.0.0.1:"+fmt.Sprint(ZizvideoPort)+"/",
			"数据目录："+p.DataDir+"（卸载时默认保留；DB、封面、config.json 都在这里）",
			"媒体根由 config.json 的 media_allow_roots 决定，卸载时**永远不动**",
		)
	}
	return m.waitZizvideoReady(ctx, p, result)
}

// waitZizvideoReady 等 /healthz 真的返回 ok。
func (m *Manager) waitZizvideoReady(ctx context.Context, p ZizvideoPaths, result *InstallResult) error {
	deadline := time.Now().Add(zizvideoReadyTimeout)
	var last string
	for time.Now().Before(deadline) {
		ok, detail := m.ZizvideoServing(ctx)
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
		What:    "zizvideo 网页界面",
		Expect:  fmt.Sprintf("127.0.0.1:%d%s 在 %s 内返回 ok", ZizvideoPort, zizvideoHealthPath, zizvideoReadyTimeout),
		Timeout: zizvideoReadyTimeout,
		Probe: func(context.Context) readyVerdict {
			ok, detail := m.ZizvideoServing(ctx)
			if ok {
				return readyVerdict{OK: true, Actual: "已就绪：" + detail}
			}
			return readyVerdict{Actual: last}
		},
		LogPath: p.ErrLog,
		State: "二进制已落盘并复核（" + p.Bin + "），系统级守护进程 " + ZizvideoLabel +
			" 已 bootstrap，服务已登记进「服务管理」",
		Missing: "但网页界面没有返回 ok，浏览器打不开它",
		Remedy: "在「服务管理 → zizvideo」里点「重启服务」再试；仍然失败请看下面的日志尾部" +
			"（常见原因：" + fmt.Sprint(ZizvideoPort) + " 端口被别的进程占用、config.json 写坏了）",
		Result: result,
	})
}

// ---------------------------------------------------------------------------
//  卸载
// ---------------------------------------------------------------------------

// UninstallZizvideo 卸载：bootout system 域 + 删 plist + 删安装根，并**复核**。
//
// 数据目录（DB、封面、config.json）**默认保留**；媒体根**任何情况下都不动**。
func (m *Manager) UninstallZizvideo(ctx context.Context, app App, removeData bool, result *InstallResult) error {
	p := m.ZizvideoPathsFor()
	hasPlist := p.Plist != "" && fileExists(p.Plist)
	hasRecord := false
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if s.LaunchLabel == ZizvideoLabel {
					hasRecord = true
					break
				}
			}
		}
	}
	if hasPlist || hasRecord {
		if result != nil {
			result.step(ctx, "停止并删除系统级守护进程 "+ZizvideoLabel)
		}
		if err := zizvideoStop(m, ctx, ZizvideoLabel, p.Plist); err != nil {
			return fmt.Errorf("停止 zizvideo 失败：%w", err)
		}
	} else if result != nil {
		result.step(ctx, "zizvideo 的服务本来就没有注册，跳过停止")
	}
	// plist 兜底再删一次（stop 只负责"停止"，文件必须真的没有 —— 坑 161）。
	if p.Plist != "" {
		if err := os.Remove(p.Plist); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 plist %s 失败：%w", p.Plist, err)
		}
	}
	// 安装根（二进制）总是删 —— 留着它就是一个"看起来已安装"的假象。
	root := p.Root
	if root == "" {
		root = ZizvideoInstallRoot
	}
	if err := m.removeTree(ctx, root, result); err != nil {
		return err
	}
	if removeData {
		if p.DataDir != "" {
			if result != nil {
				result.step(ctx, "按你的选择删除 "+p.DataDir)
			}
			if err := os.RemoveAll(p.DataDir); err != nil {
				return fmt.Errorf("删除 %s 失败：%w", p.DataDir, err)
			}
		}
		if result != nil {
			result.step(ctx, "数据已删除（DB、封面、config.json 都不再保留；媒体文件不受影响）")
		}
	} else if result != nil {
		result.step(ctx, "按你的选择**保留**数据目录 "+p.DataDir+
			"（DB、封面、config.json）；要一并清理请勾选「同时删除数据」")
	}
	// 复核：目录没了 / plist 没了 / 7766 不再返回 ok。
	if err := m.zizvideoVerifyRemoved(ctx, p); err != nil {
		return err
	}
	if result != nil {
		result.step(ctx, "卸载复核通过：安装目录与 plist 都已删除，"+fmt.Sprint(ZizvideoPort)+
			" 端口不再返回 zizvideo")
	}
	return nil
}

// zizvideoVerifyRemoved 是卸载的终态复核（判据贴着运行体）。
func (m *Manager) zizvideoVerifyRemoved(ctx context.Context, p ZizvideoPaths) error {
	deadline := time.Now().Add(zizvideoRemovedWait)
	for {
		if ok, _ := m.ZizvideoServing(ctx); !ok {
			break
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("卸载后 127.0.0.1:%d 仍在返回 ok，服务没有被真正停掉", ZizvideoPort)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(zizvideoRemovedPoll):
		}
	}
	if p.Root != "" && fileExists(p.Root) {
		return fmt.Errorf("卸载后安装目录 %s 仍然存在", p.Root)
	}
	if p.Plist != "" && fileExists(p.Plist) {
		return fmt.Errorf("卸载后系统 plist %s 仍然存在", p.Plist)
	}
	return nil
}

// zizvideoInstallPlan 生成卸载计划（确认框用；只读，不产生任何改动）。
//
// DataPaths **只含数据目录**：媒体根是用户自己的文件，永远不进这里。
func (m *Manager) zizvideoInstallPlan() UninstallPlan {
	p := m.ZizvideoPathsFor()
	steps := []string{
		"停止并删除系统级守护进程 " + ZizvideoLabel + "（" + p.Plist + "）",
		"删除安装目录 " + p.Root + "（二进制）",
		"从「服务管理」移除记录",
		"⚠️ 卸载后 127.0.0.1:" + fmt.Sprint(ZizvideoPort) + " 会不可用，直到重新安装",
	}
	plan := UninstallPlan{Kind: "installer", Steps: steps}
	if p.DataDir != "" {
		plan.DataPaths = append(plan.DataPaths, p.DataDir)
	}
	plan.KeepNote = "默认保留数据目录 " + p.DataDir + "（DB、封面、config.json）；" +
		"媒体根由 media_allow_roots 指向的文件**任何情况下都不会动**；" +
		"要一并删除数据请在确认框里勾选「同时删除数据」"
	return plan
}

// installFileExecutable 把一个文件复制到目标路径并给可执行位（先写 .part 再改名，
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

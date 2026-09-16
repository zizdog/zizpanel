package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ============================================================================
//  应用包镜像（自建 NAS）
//
//  政策（用户原话，2026-09-16 最终版）：
//    "对所有能用到的模型、软件，如 ffmpeg，都要以 NAS 镜像优先，不通再走别的！"
//
//  也就是说镜像的语义是**优先来源，不是唯一来源**：
//    · 镜像上有 → **一定**用镜像（地址写进任务日志，用户看得见到底从哪下的）；
//    · 镜像上缺这个资源 / 镜像站不可达 → **自动回落**既有的公网源，
//      并把"这次没走镜像、为什么"写进任务步骤（见 preflightMirrorAsset）。
//
//  ⚠️ 历史坑：本文件与 config.go 的注释一度写的是"唯一来源、不回退、
//  没有就明确失败"（那是更早一版的需求）。代码后来按用户要求改成了"优先+回落"，
//  注释却没跟着改 —— 文档说一套、代码做一套，是最容易让下一个人改错的一类不一致。
//  现在两处注释都以本节为准；**行为**在各类下载点上统一为"优先+回落"。
//
//  布局（与 tools/sync-nas-apps.sh 严格一致）：
//    <base>/apps/<app-id>/<version>/<原始文件名>
//    <base>/apps/<app-id>/<version>/manifest.json   ← 每个包的 sha256/大小
//
//  其它来源统一走同一台镜像的子路径（NAS 侧反代，见 NAS 上 zizpanel-mirror
//  容器的 nginx.conf）：
//    HF → <base>/hf、brew → <base>/brew、CLT → <base>/zizpanel/clt
//    （pypi 目前 NAS 上没有，见交付说明）
//
//  镜像基址来自面板设置（Config.MirrorBase → Options.MirrorBase），
//  留空表示**关闭镜像**（应急用；那时各来源回到内置的公网/国内镜像）。
// ============================================================================

const (
	// mirrorAppsDir 是镜像站上应用包的目录名。
	mirrorAppsDir = "apps"
	// mirrorManifestName 是每个"应用 + 版本"目录下的清单名。
	mirrorManifestName = "manifest.json"
	// mirrorDefaultProbeSeconds 是探测超时的兜底值（Options.MirrorProbeSeconds <=0 时用）。
	mirrorDefaultProbeSeconds = 4
)

// mirrorProbeTimeout 是"镜像上有没有这个资源"的单次探测超时。
//
// 必须短（默认 4 秒）：局域网/同城镜像正常在 100ms 内应答，4 秒足够区分"慢"与
// "不通"，又不会在镜像站故障时把每次安装都拖很久。
func (m *Manager) mirrorProbeTimeout() time.Duration {
	sec := m.opt.MirrorProbeSeconds
	if sec <= 0 {
		sec = mirrorDefaultProbeSeconds
	}
	return time.Duration(sec) * time.Second
}

// ReleaseBinaryAsset 描述"要同步到镜像站的一个包"（给 NAS 同步工具用）。
type ReleaseBinaryAsset struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Repo          string `json:"repo"`
	Tag           string `json:"tag"`
	Asset         string `json:"asset"`
	ChecksumAsset string `json:"checksum_asset,omitempty"`
	// UpstreamURL 是官方下载地址（同步工具用它去上游取包）。
	UpstreamURL string `json:"upstream_url"`
}

// ReleaseBinaryAssets 导出注册表里全部"官方 release 原生二进制"应用的下载信息。
//
// 为什么要有它：NAS 同步工具要按 apps/<id>/<版本>/<文件名> 存包，这个布局与版本
// 必须和代码一致。让工具从注册表读（而不是在 shell 里手抄），就不会出现
// "代码升了版本、镜像还停在旧版本"的错位，加新应用也会自动带上。
func ReleaseBinaryAssets() []ReleaseBinaryAsset {
	out := make([]ReleaseBinaryAsset, 0, len(releaseBinaryApps))
	for _, spec := range releaseBinaryApps {
		out = append(out, ReleaseBinaryAsset{
			ID: spec.ID, Name: spec.Name, Repo: spec.Repo, Tag: spec.Tag,
			Asset: spec.Asset, ChecksumAsset: spec.ChecksumAsset,
			UpstreamURL: spec.releaseURL(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// mirrorBase 返回生效的镜像基址（去掉末尾的 /）。
// 空串 = 用户显式关掉了镜像（Config.MirrorBase 留空）。
func (m *Manager) mirrorBase() string {
	return strings.TrimRight(strings.TrimSpace(m.opt.MirrorBase), "/")
}

// MirrorEnabled 表示当前是否启用"镜像优先"（界面/日志用）。
//
// 注意语义：启用 != 只用镜像。启用后镜像**优先**，但它缺件或不可达时
// 各来源会回落到自己的公网/国内源（见文件头注释）。留空 = 完全不用镜像。
func (m *Manager) MirrorEnabled() bool { return m.mirrorBase() != "" }

// appAssetURL 拼应用包在镜像上的地址：<base>/apps/<appID>/<version>/<asset>。
//
// appID 与 version 都取自代码里的注册表（releaseBinaryApps），不手抄 ——
// 加新应用时自动带上，不会出现"代码里有、镜像路径写错"的错位。
func (m *Manager) appAssetURL(appID, version, asset string) string {
	return strings.Join([]string{m.mirrorBase(), mirrorAppsDir, appID, version, asset}, "/")
}

// appManifestURL 拼清单地址（与包同目录）。
func (m *Manager) appManifestURL(appID, version string) string {
	return m.appAssetURL(appID, version, mirrorManifestName)
}

// mirrorSubPath 拼"其它来源"在镜像上的子路径（NAS 侧反代）：
// mirrorSubPath("pypi/simple") → <base>/pypi/simple。
func (m *Manager) mirrorSubPath(sub string) string {
	return m.mirrorBase() + "/" + strings.Trim(sub, "/")
}

// mirrorAsset / mirrorManifest 与同步工具生成的 manifest.json 一一对应。
type mirrorAsset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type mirrorManifest struct {
	App     string        `json:"app"`
	Version string        `json:"version"`
	Assets  []mirrorAsset `json:"assets"`
}

// sha256For 查某个文件在清单里的 sha256（没有返回空串，调用方据此报错）。
func (mm *mirrorManifest) sha256For(name string) string {
	if mm == nil {
		return ""
	}
	for _, a := range mm.Assets {
		if a.Name == name {
			return a.SHA256
		}
	}
	return ""
}

// fetchMirrorManifest 取镜像上的清单（下载包之前用它拿 sha256）。
func (m *Manager) fetchMirrorManifest(ctx context.Context, appID, version string) (*mirrorManifest, error) {
	u := m.appManifestURL(appID, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("镜像清单地址不合法（%s）：%w", u, err)
	}
	cl := &http.Client{Timeout: 2 * m.mirrorProbeTimeout()}
	res, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("取镜像清单失败（%s）：%w", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("镜像清单不可用（%s，HTTP %d）", u, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读镜像清单失败（%s）：%w", u, err)
	}
	var mm mirrorManifest
	if err := json.Unmarshal(body, &mm); err != nil {
		return nil, fmt.Errorf("镜像清单不是合法 JSON（%s）：%w", u, err)
	}
	return &mm, nil
}

// checkMirrorURL 检查镜像上的一个地址是否可用（HEAD）。
//
// 返回的 error 是**给用户看**的：说清"缺什么"和"怎么补"（404 时提示
// `make sync-apps`）。调用方据此决定**回落到公网源**还是跳过镜像，
// 所以这里的措辞不要写成"安装失败"，那与"优先+回落"的语义不符。
func (m *Manager) checkMirrorURL(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return fmt.Errorf("镜像地址不合法（%s）：%w", url, err)
	}
	cl := &http.Client{Timeout: m.mirrorProbeTimeout()}
	res, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("镜像站访问不了（%s）：%v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode == http.StatusOK:
		return nil
	case res.StatusCode == http.StatusNotFound:
		return fmt.Errorf("镜像站上没有这个资源（%s，HTTP 404）。"+
			"请在能访问上游的机器上执行 `make sync-apps` 把它同步到 NAS 后重试", url)
	default:
		return fmt.Errorf("镜像站返回 HTTP %d（%s）", res.StatusCode, url)
	}
}

// downloadURLsFor 返回这个应用**允许使用**的下载地址，**镜像优先**。
//
// 语义（2026-09-16 用户明确要求）：
//  1. 配了镜像基址时，第一个候选永远是镜像上的包（省流量、速度快）；
//  2. 镜像**探测不通**（站点挂了/不在同一网络）时，自动回落到原来的
//     "官方 + 国内第三方加速"列表 —— 绝不能因为镜像挂了就装不上；
//  3. 没配镜像基址时就是原来的列表。
//
// 探测是**按资源**做的（HEAD 那个具体文件），不是只探站点根：
// 站点活着但缺这个包时，也要能回落到公网，否则用户会卡在"镜像上没有这个资源"。
func (m *Manager) downloadURLsFor(ctx context.Context, spec releaseBinaryApp) []string {
	fallback := spec.downloadURLs()
	if !m.MirrorEnabled() {
		return fallback
	}
	mirrorURL := m.appAssetURL(spec.ID, spec.Tag, spec.Asset)
	if err := m.checkMirrorURL(ctx, mirrorURL); err != nil {
		// 不可达/缺包：回落。把原因留给调用方记进任务日志（这里只返回列表）。
		return fallback
	}
	return append([]string{mirrorURL}, fallback...)
}

// mirrorReachable 探测镜像站是否可用（只探站点根，用于"整条链路"级别的判断）。
//
// 与 checkMirrorURL 的分工：那个探**具体资源**（用来决定某个包从哪下），
// 这个探**站点本身**（用来决定 pip / HF 这类"整条链路"要不要走镜像）。
func (m *Manager) mirrorReachable(ctx context.Context) bool {
	if !m.MirrorEnabled() {
		return false
	}
	return m.checkMirrorURL(ctx, m.mirrorBase()+"/") == nil
}

// probeMirrorFile 探镜像上一个**具体文件**是否可用，并返回它的大小
// （上游没给 Content-Length 时返回 -1）。
//
// 与 checkMirrorURL 是同一件事（HEAD + 同样的短超时），只是多要一个长度：
// 模型权重动辄 200MB~3GB，任务日志要把"多大、下到哪了"如实写出来，
// 只说"通/不通"不够；调用方也用它来拒绝 0 字节的假文件。
func (m *Manager) probeMirrorFile(ctx context.Context, url string) (int64, error) {
	if m.mirrorFileProbeOverride != nil {
		return m.mirrorFileProbeOverride(ctx, url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return -1, fmt.Errorf("镜像地址不合法（%s）：%w", url, err)
	}
	cl := &http.Client{Timeout: m.mirrorProbeTimeout()}
	res, err := cl.Do(req)
	if err != nil {
		return -1, fmt.Errorf("镜像站访问不了（%s）：%v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusOK:
		return res.ContentLength, nil
	case http.StatusNotFound:
		return -1, fmt.Errorf("镜像上没有这个文件（HTTP 404，%s）", url)
	default:
		return -1, fmt.Errorf("镜像返回 HTTP %d（%s）", res.StatusCode, url)
	}
}

// preflightMirrorAsset 下载前确认镜像上真的有这个包与它的清单。
//
// 这是用户要求的那一步"先检查"。**返回 true 表示这次会走镜像**
// （包与清单都在），false 表示要回落到公网源。
//
// 为什么改成"回落"而不是"直接失败"（2026-09-16 用户补充要求）：
// 镜像站可能临时挂掉、或者这台机器根本不在能访问镜像的网络里。
// 那时**装不上**才是更糟的结果，所以缺资源/不可达一律回落，
// 并把原因写进任务步骤（用户看得见"这次没走镜像、为什么"）。
func (m *Manager) preflightMirrorAsset(ctx context.Context, spec releaseBinaryApp, result *InstallResult) bool {
	if !m.MirrorEnabled() {
		return false
	}
	pkg := m.appAssetURL(spec.ID, spec.Tag, spec.Asset)
	man := m.appManifestURL(spec.ID, spec.Tag)
	if result != nil {
		result.step(ctx, "先检查镜像资源："+pkg)
	}
	if err := m.checkMirrorURL(ctx, pkg); err != nil {
		if result != nil {
			result.step(ctx, "镜像上没有这个包，改用公网源："+err.Error())
		}
		return false
	}
	// 清单也要在：它是镜像模式下校验 sha256 的唯一来源。
	// 包在、清单不在时同样回落（否则会拿不到期望值而中止）。
	if err := m.checkMirrorURL(ctx, man); err != nil {
		if result != nil {
			result.step(ctx, "镜像上缺 sha256 清单，改用公网源："+err.Error())
		}
		return false
	}
	if result != nil {
		result.step(ctx, "镜像可用，将从镜像站下载（省流量、更快）")
	}
	return true
}

// verifyMirrorChecksum 用镜像清单里的 sha256 核对下载到的产物。
//
// 为什么不是上游的 checksum 文件：一是镜像模式下不再访问任何其它来源，
// 二是 Lucky / Orbien 上游根本没有 checksums 文件 —— 镜像清单覆盖全部条目，
// 比原来（只对 frp 校验）更严。
//
// 期望值的取法走 mirrorChecksumFor（steps.go 里描述符执行器的钩子用的是同一个）——
// "清单在哪、怎么取、取不到怎么报错"只有一份实现，避免两处漂移。
func (m *Manager) verifyMirrorChecksum(ctx context.Context, spec releaseBinaryApp,
	p binaryReleasePaths, result *InstallResult) error {

	want, err := m.mirrorChecksumFor(ctx, spec)
	if err != nil {
		return err
	}
	got, err := fileSHA256(p.Asset)
	if err != nil {
		return fmt.Errorf("计算 %s 的 sha256 失败: %w", p.Asset, err)
	}
	source := "镜像清单 " + m.appManifestURL(spec.ID, spec.Tag)
	if err := matchChecksum(spec.Asset, want, got, source, p.Root); err != nil {
		return err
	}
	if result != nil {
		result.step(ctx, fmt.Sprintf("SHA-256 校验通过：%s 与镜像清单一致（来源：%s）",
			spec.Asset, source))
	}
	return nil
}

// ============================================================================
//  离线模式（仅走 NAS，禁止外网回落）
//
//  设置项：Config.OfflineOnly → Options.OfflineOnly。
//
//  与"镜像优先"的区别（两个语义都要在，别用一个替代另一个）：
//    · 镜像优先（默认）：能用镜像就用；缺件/不可达**回落公网** —— 好处是
//      镜像站抖一下也不会让用户装不上，代价是"到底走的哪条路"不稳定。
//    · 离线模式（本开关）：只用镜像；缺件**明确失败**并列出缺哪个文件 ——
//      用于"整机断外网 / 隔离网络 / 迁移到新 Mac"这类场景。那时回落公网
//      只会变成"装到一半卡死"，比直接失败更糟：用户不知道自己在等什么。
//
//  ⚠️ 各安装器**必须显式接线**才会遵守这个开关 —— 这个函数只是一个判断，
//  不会自动拦截任何东西。需要接线的清单见交付说明里的"接线清单"。
//  ============================================================================

// MirrorOfflineOnly 报告当前是否处于"仅走 NAS（离线）模式"。
//
// 各安装器在下决心回落公网之前**必须**先问它：
//
//	if m.MirrorOfflineOnly(ctx) {
//	    return m.offlineOnlyFail("Homebrew 瓶 "+formula, mirrorURL)
//	}
//
// 参数 ctx 目前不参与判断，保留它是为了将来能在需要时做一次镜像可达性探测
// 而不改调用方签名（签名稳定比省一个参数重要）。
func (m *Manager) MirrorOfflineOnly(ctx context.Context) bool {
	_ = ctx
	return m.opt.OfflineOnly
}

// offlineOnlyFail 生成"离线模式下缺资源"的统一错误。
//
// 三个必须说清的点（用户的真实诉求是"缺什么、去哪补"）：
//  1. 这是**离线模式**主动拒绝，不是网络故障 —— 免得用户去查网络；
//  2. 缺的是**哪个资源**、镜像上应该在哪（resource 与 mirrorURL）；
//  3. 怎么补：用 tools/build-offline-bundle.sh 打包，或关掉这个开关。
func (m *Manager) offlineOnlyFail(resource, mirrorURL string) error {
	where := mirrorURL
	if where == "" {
		where = "（镜像基址为空：设置里的 mirror_base 没填）"
	}
	return fmt.Errorf(
		"离线模式（仅走 NAS）已开启，禁止回落外网，但镜像上没有这个资源：%s\n"+
			"  镜像上应存在的位置：%s\n"+
			"  补法：在能访问上游的机器上执行 "+
			"`bash tools/build-offline-bundle.sh --app <应用> --upload` 把它打进 NAS 离线包；"+
			"或在「设置 → 访问与安全」里关闭「仅走 NAS（离线）」",
		resource, where)
}

// MirrorOfflinePreflight 是给安装器用的"离线模式缺件检查"。
//
// 语义：离线模式下**先确认镜像上真的有这个资源**（HEAD，走 probeMirrorFile
// 以便单测注入、并顺带拿到大小），
//   - 有 → 返回 nil，调用方继续按镜像地址下载；
//   - 没有/不可达 → 返回**面向用户**的明确错误（不做任何公网回落）。
//
// 非离线模式下它什么都不做（返回 nil），让调用方保留原有的"优先+回落"逻辑。
// 这样接线只需要两行，不会改变默认行为 —— 这正是本次刻意收敛改动范围的原因。
func (m *Manager) MirrorOfflinePreflight(ctx context.Context, resource, mirrorURL string) error {
	if !m.MirrorOfflineOnly(ctx) {
		return nil
	}
	if m.mirrorBase() == "" {
		return m.offlineOnlyFail(resource, "")
	}
	if _, err := m.probeMirrorFile(ctx, mirrorURL); err != nil {
		return m.offlineOnlyFail(resource+"（探测失败："+err.Error()+"）", mirrorURL)
	}
	return nil
}

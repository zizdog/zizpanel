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
//  需求（2026-09-16，用户明确要求）：
//    "所有安装过程先检查 https://mirror.zizdog.com:8888 的资源能不能访问，
//      不能再走其它。"
//
//  也就是说镜像不是"加速源之一"，而是**唯一来源**：
//    · 镜像上有 → 用镜像（地址写进任务日志，用户看得见到底从哪下的）；
//    · 镜像上没有/不可达 → **明确失败**，并说清缺哪个路径、怎么补，
//      绝不静默回退到 GitHub。回退会让"这台机器到底能不能装"变成不可预测，
//      也违背"统一在 NAS 上管控"的初衷。
//
//  布局（与 tools/sync-nas-apps.sh 严格一致）：
//    <base>/apps/<app-id>/<version>/<原始文件名>
//    <base>/apps/<app-id>/<version>/manifest.json   ← 每个包的 sha256/大小
//
//  其它来源统一走同一台镜像的子路径（NAS 侧反代）：
//    pip → <base>/pypi/simple、HF → <base>/hf、brew → <base>/brew
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

// MirrorEnabled 表示当前是否强制走镜像（界面/日志用）。
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
// 返回的 error 是**给用户看**的：说清"缺什么"和"怎么补"。因为按需求镜像
// 是唯一来源、不回退公网，用户必须知道该去 NAS 上做什么才能把包补上。
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

// downloadURLsFor 返回这个应用**允许使用**的下载地址。
//
// 镜像生效时只有一个候选（用户要求："先检查镜像站的资源能不能访问，不能再走其它"）。
// 镜像关掉时才回到原来的"官方 + 第三方加速"列表。
func (m *Manager) downloadURLsFor(spec releaseBinaryApp) []string {
	if !m.MirrorEnabled() {
		return spec.downloadURLs()
	}
	return []string{m.appAssetURL(spec.ID, spec.Tag, spec.Asset)}
}

// preflightMirrorAsset 下载前确认镜像上真的有这个包与它的清单。
//
// 这是用户要求的那一步"先检查"。检查不过就中止安装（不回退），
// 错误里带上具体路径与补救命令。
func (m *Manager) preflightMirrorAsset(ctx context.Context, spec releaseBinaryApp, result *InstallResult) error {
	if !m.MirrorEnabled() {
		return nil
	}
	pkg := m.appAssetURL(spec.ID, spec.Tag, spec.Asset)
	if result != nil {
		result.step(ctx, "检查镜像资源："+pkg)
	}
	if err := m.checkMirrorURL(ctx, pkg); err != nil {
		return fmt.Errorf("「%s」不能安装：%w", spec.Name, err)
	}
	// 清单也要在：它是校验 sha256 的唯一来源（镜像模式下不访问上游）。
	if err := m.checkMirrorURL(ctx, m.appManifestURL(spec.ID, spec.Tag)); err != nil {
		return fmt.Errorf("「%s」不能安装：%w", spec.Name, err)
	}
	return nil
}

// verifyMirrorChecksum 用镜像清单里的 sha256 核对下载到的产物。
//
// 为什么不是上游的 checksum 文件：一是镜像模式下不再访问任何其它来源，
// 二是 Lucky / Orbien 上游根本没有 checksums 文件 —— 镜像清单覆盖全部条目，
// 比原来（只对 frp 校验）更严。
func (m *Manager) verifyMirrorChecksum(ctx context.Context, spec releaseBinaryApp,
	p binaryReleasePaths, result *InstallResult) error {

	url := m.appManifestURL(spec.ID, spec.Tag)
	mm, err := m.fetchMirrorManifest(ctx, spec.ID, spec.Tag)
	if err != nil {
		return fmt.Errorf("校验失败，已中止安装：%w"+
			"（清单由同步工具生成：在能访问上游的机器上执行 `make sync-apps`）", err)
	}
	want := mm.sha256For(spec.Asset)
	if want == "" {
		return fmt.Errorf("镜像清单里没有 %s 的 sha256（%s）。已中止安装；"+
			"请重新执行 `make sync-apps` 让清单与包一致", spec.Asset, url)
	}
	got, err := fileSHA256(p.Asset)
	if err != nil {
		return fmt.Errorf("计算 %s 的 sha256 失败: %w", p.Asset, err)
	}
	if err := matchChecksum(spec.Asset, want, got, "镜像清单 "+url, p.Root); err != nil {
		return err
	}
	if result != nil {
		result.step(ctx, fmt.Sprintf("SHA-256 校验通过：%s 与镜像清单一致（来源：%s）", spec.Asset, url))
	}
	return nil
}

package services

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// ============================================================================
//  一键建站源码包（Typecho / WordPress）的镜像来源
//
//  缺陷 D34：这两类应用的源码包原先在 internal/web/api_site_apps.go 里
//  **直接 curl 官方地址**（只有"官方 + 一个写死的镜像"），既没有 NAS 优先、
//  也没有测速、更不上报来源，而且完全不校验哈希。实测还发现 Typecho 那个
//  写死的 jsdelivr 备用地址**已经 404**（2026-09-16），等于只有一个源。
//
//  本文件把它纳入与 internal/services/mirror.go 一致的镜像政策：
//    · 版本**写死**（不追 latest）：追 latest 会让"昨天能装、今天装不上"，
//      而且 sha256 必然过期；
//    · NAS 优先：<base>/sites/<app>/<version>/<文件名>（与 /apps/ 同一套布局）；
//    · 探不通就**回落**官方源，并把真实来源与耗时写进任务日志；
//    · 每下一个都核对 **真实 sha256**，不符就删文件并如实失败。
//
//  ⚠️ 版本号与 sha256 都是 2026-09-16 **实测**得到（下载官方固定版本地址、
//  在 NAS 上再算一遍），不是抄文档、不是编的。这个仓库因为编造 sha256 出过
//  "镜像永不命中、静默回落慢源"的事故，所以这里只认真算过的值。
//
//  ⚠️ Upstreams 只允许放**字节完全一致**的地址：这里每下一个都要核对同一个
//  SHA256，放一个内容不同的版本（例如 WordPress 英文包 vs 中文包）会 100%
//  校验失败，等于没有这个回落源。
// ============================================================================

// SiteSource 描述一个"一键建站"应用的固定版本源码包。
type SiteSource struct {
	// App 是目录条目 ID（typecho / wordpress），也是镜像路径里的目录名。
	App string
	// Name 是给用户看的名字。
	Name string
	// Version 是**写死**的版本（不追 latest）。
	Version string
	// File 是原始文件名（镜像与上游保持一致，便于按名拼 URL）。
	File string
	// SHA256 是该文件的真实 sha256（实测）。
	SHA256 string
	// Size 是文件字节数（实测，用于日志与探测对照）。
	Size int64
	// Upstreams 是同一份产物的官方地址，按优先顺序回落。
	Upstreams []string
}

// siteSources 是已登记固定版本的一键建站应用。
//
// 为什么用代码里的表而不是目录条目（catalog.go）里的 URL：
//   - catalog 的两个地址追 latest，无法配一个稳定的 sha256；
//   - 目录条目由多个模块共用，改它会影响"未登记固定版本"的应用，
//     这里只覆盖登记过的应用（见 DownloadSitePackage 的回落分支）。
var siteSources = map[string]SiteSource{
	"typecho": {
		App: "typecho", Name: "Typecho", Version: "1.3.0",
		File:   "typecho.zip",
		SHA256: "c34cd65b2f944f464bcaa8f43b10f665f1dae13158c694546b22c35ddc296668",
		Size:   578232,
		Upstreams: []string{
			// gh-proxy 放在官方直连**之前**：2026-09-16 实测 GitHub release
			// 直连 34~65 KB/s、且复测时出现过连不上（HTTP 000）；gh-proxy 实测
			// 2.6 MB/s 且代理的是同一份文件。拉完照样核对 sha256，走代理不影响
			// 完整性 —— "官方直连"在这里只是名义上更正统，实际更慢更不稳。
			"https://gh-proxy.com/https://github.com/typecho/typecho/releases/download/v1.3.0/typecho.zip",
			// 官方 release（固定 tag，不是 latest）。
			"https://github.com/typecho/typecho/releases/download/v1.3.0/typecho.zip",
		},
	},
	"wordpress": {
		App: "wordpress", Name: "WordPress", Version: "7.1",
		File:   "wordpress-7.1-zh_CN.zip",
		SHA256: "6bd9237178dd870f7b47cf33e7129219d9bd2a85a92be641cf487c2cd61a2dba",
		Size:   44762801,
		Upstreams: []string{
			"https://cn.wordpress.org/wordpress-7.1-zh_CN.zip",
			// downloads.wordpress.org 的 zh_CN 版，实测与上面**逐字节一致**。
			"https://downloads.wordpress.org/release/zh_CN/wordpress-7.1.zip",
		},
	},
}

// SiteSourceFor 查某个一键建站应用的固定版本源码包。
func SiteSourceFor(appID string) (SiteSource, bool) {
	s, ok := siteSources[strings.TrimSpace(appID)]
	return s, ok
}

// mirrorSitesDir 是镜像站上源码包的目录名（与 /apps/ 平级）。
const mirrorSitesDir = "sites"

// sitePackageTimeout 是"一次源码包下载"的总超时。
//
// 之所以给足 15 分钟：WordPress 约 44 MB，公网源实测最低到过 586 KB/s（≈76 秒），
// 但上游抖动时会更慢；而这里已经有 90 秒的停滞看门狗（fetchToFile）兜底，
// 总超时只是防止"读得动但永远下不完"这一种病态情况。
const sitePackageTimeout = 15 * time.Minute

// sitePackageMirrorURL 拼源码包在镜像上的地址：<base>/sites/<app>/<版本>/<文件名>。
func (m *Manager) sitePackageMirrorURL(src SiteSource) string {
	return strings.Join([]string{
		m.mirrorBase(), mirrorSitesDir, src.App, src.Version, src.File,
	}, "/")
}

// sitePackageSources 选出这次的下载候选：**NAS 优先，探不通再回落官方**。
//
// 风格照抄 iopaintWeightSources：先探 NAS 上的**具体文件**（不是站点根），
// 探到了才排第一并如实标注；探不到就明说"镜像上没有这个版本，回落官方"。
// 探测走 m.probeMirrorFile，单测用 mirrorFileProbeOverride 注入（不联网）。
func (m *Manager) sitePackageSources(ctx context.Context, src SiteSource, logf func(string)) []weightSource {
	if logf == nil {
		logf = func(string) {}
	}
	fallback := make([]weightSource, 0, len(src.Upstreams))
	for i, u := range src.Upstreams {
		switch {
		case strings.Contains(u, "gh-proxy.com"):
			fallback = append(fallback, weightSource{URL: u, Label: "GitHub 加速（gh-proxy.com）"})
		case i == 0:
			fallback = append(fallback, weightSource{URL: u, Label: "官方源"})
		default:
			fallback = append(fallback, weightSource{URL: u, Label: "官方备用源"})
		}
	}
	if len(fallback) == 0 {
		return nil
	}
	if !m.MirrorEnabled() {
		logf("未启用 NAS 镜像，源码包来源：" + fallback[0].URL)
		return fallback
	}
	mirrorURL := m.sitePackageMirrorURL(src)
	size, err := m.probeMirrorFile(ctx, mirrorURL)
	if err != nil {
		logf(fmt.Sprintf("NAS 镜像上没有 %s %s（%v），源码包来源回落到官方源",
			src.Name, src.Version, err))
		return fallback
	}
	sizeText := "大小未知"
	if size > 0 {
		sizeText = humanBytes(size)
	}
	logf(fmt.Sprintf("NAS 镜像上有 %s %s（已探通，%s），优先从镜像站下载",
		src.Name, src.Version, sizeText))
	return append([]weightSource{{URL: mirrorURL, Label: "NAS 镜像"}}, fallback...)
}

// fetchVerifiedSitePackage 按候选顺序下载源码包，**每下一个都核对 SHA256**。
//
// 语义（用户明确要求"不许编造 sha256、不许静默用坏文件"）：
//   - 校验不通过 → **立刻删掉产物**，记下错误，继续试下一个源；
//   - 全部源都失败 → 返回可读错误（日志里已经写明每个源各失败在哪）；
//   - 绝不把"哈希不符的文件"留在 dest 上，也绝不把安装继续下去。
func (m *Manager) fetchVerifiedSitePackage(ctx context.Context, src SiteSource,
	srcs []weightSource, dest string, logf func(string)) (weightSource, error) {

	if logf == nil {
		logf = func(string) {}
	}
	var lastErr error
	for i, s := range srcs {
		label := s.Label
		if i > 0 {
			label += "（回落）"
		}
		logf(fmt.Sprintf("下载 %s %s ← %s", src.Name, src.Version, label))
		started := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, sitePackageTimeout)
		err := m.fetchFile(attemptCtx, s.URL, dest, siteProgressLogger(logf))
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("%s：%w", label, err)
			logf("  下载失败：" + err.Error())
			continue
		}
		got, err := fileSHA256(dest)
		if err != nil {
			lastErr = fmt.Errorf("%s：计算 sha256 失败：%w", label, err)
			_ = os.Remove(dest)
			logf("  " + lastErr.Error())
			continue
		}
		if !strings.EqualFold(got, src.SHA256) {
			_ = os.Remove(dest)
			lastErr = fmt.Errorf("%s：SHA-256 校验不通过（期望 %s，实际 %s），已删除下载文件",
				label, src.SHA256, got)
			logf("  " + lastErr.Error())
			continue
		}
		logf(fmt.Sprintf("  来源：%s，SHA-256 校验通过（%s，用时 %.1f 秒）",
			label, humanBytes(fileSizeOrZero(dest)), time.Since(started).Seconds()))
		return s, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的下载地址")
	}
	return weightSource{}, fmt.Errorf("下载 %s %s 失败：%w", src.Name, src.Version, lastErr)
}

// siteProgressLogger 生成"把下载进度写进任务日志"的回调（节流）。
//
// 为什么节流：fetchToFile 约每 10 秒回调一次；如果一个 44 MB 的包把每次回调
// 都追加成一条任务步骤，任务日志会被进度刷屏。这里按**每满 25%** 或**上游没给
// 长度时每 15 秒**打一条，既让用户看到"在动"，又不淹没来源/校验这些关键行。
func siteProgressLogger(logf func(string)) fetchProgressFunc {
	lastMark := -1
	var lastEmit time.Time
	return func(got, total int64) {
		if total <= 0 {
			// 上游没给长度：定期打一条"已收多少"。
			if time.Since(lastEmit) > 15*time.Second {
				logf(fmt.Sprintf("  下载中：已收 %s（上游未给长度）", humanBytes(got)))
				lastEmit = time.Now()
			}
			return
		}
		mark := int(float64(got) * 100 / float64(total))
		if mark >= lastMark+25 {
			lastMark = mark
			logf(fmt.Sprintf("  下载进度：%d%%（%s / %s）", mark, humanBytes(got), humanBytes(total)))
		}
	}
}

// DownloadSitePackage 把某个一键建站应用的源码包下载到 dest，返回选中的来源。
//
// 这是 web 层唯一需要调用的入口：**NAS 优先 → 回落官方 → 强制 SHA256 校验**。
// 返回的 src 让调用方知道"固定版本是多少"，label 是给用户看的真实来源
// （如 "NAS 镜像" / "官方源"）。
func (m *Manager) DownloadSitePackage(ctx context.Context, appID, dest string,
	logf func(string)) (SiteSource, string, error) {

	if logf == nil {
		logf = func(string) {}
	}
	src, ok := SiteSourceFor(appID)
	if !ok {
		return SiteSource{}, "", fmt.Errorf("没有为「%s」登记固定版本源码包", appID)
	}
	srcs := m.sitePackageSources(ctx, src, logf)
	used, err := m.fetchVerifiedSitePackage(ctx, src, srcs, dest, logf)
	if err != nil {
		return src, "", err
	}
	return src, used.Label, nil
}

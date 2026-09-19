package services

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ============================================================================
//  pip 索引：镜像站优先 + 回落
//
//  面板有三个 Python 应用：iopaint / qwen3tts / voicereceiver。其中 iopaint
//  与 qwen3tts 各自建 venv 用 pip 装依赖（voicereceiver 只用系统
//  /usr/bin/python3，不装包）。大陆直连 pypi.org 慢且会断，所以两者原本都
//  写死清华源；现在改成与其它来源一致的"镜像站优先、探不通回落清华"
//  （政策见 mirror.go 文件头）。
//
//  镜像站侧怎么做到**轮子也走镜像**（2026-09-16 实测确认，别再改成改写 HTML）：
//    清华 TUNA 的 simple 索引里文件链接是**相对路径** ../../packages/…，
//    pip 用 urljoin 按"请求索引页时用的基址"解析，于是
//      <base>/pypi/simple/<pkg>/   →   <base>/pypi/packages/…
//    刚好落回镜像自身。所以镜像站的 nginx 只要有 /pypi/simple/ 与
//    /pypi/packages/ 两条 location 就够了（见镜像站上 zizpanel-mirror 的 nginx.conf）。
//    ⚠️ 换成反代 pypi.org、或链接写死 files.pythonhosted.org 的镜像就**必须**
//    改写正文：gzip 会让 sub_filter 失效，而 pip 默认优先要 PEP 691 的 JSON
//    接口，HTML 改写对它完全无效 —— 那种方案在这里是死路。
//
//  局限（必须知道，否则会把"索引过期"当成 pip 的 bug）：
//    镜像站侧 8091 的按需缓存**没有 TTL**，索引页一旦落盘就不再回源。上游发了
//    新版本后，镜像上要手动清一次：
//      rm -rf /vol2/zizpanel-mirror/_cache/pypi.tuna.tsinghua.edu.cn/simple/*
//    （packages/ 下已缓存的轮子不用动。）
// ============================================================================

// pipIndexFallback 是没有可用镜像时的索引（内置清华源）。
// 复用 qwenPipMirror：两个应用本来就是同一个源，别再各写一份常量。
const pipIndexFallback = qwenPipMirror

// pipIndexProbePackage 是探测镜像 pip 索引用的包名。
//
// 用 pip 自己当探针：每次安装都会用到它，所以索引里一定有；它的索引页只有
// 几十 KB（实测 73764 字节），探测成本低；它能出来就说明 simple 索引这条链是通的。
const pipIndexProbePackage = "pip"

// pipMirrorPath 是 PyPI 索引在镜像上的子路径（镜像站侧 nginx 的 location 前缀）。
const pipMirrorPath = "pypi/simple"

// pipMirrorArgs 决定这次 pip 用哪个索引，返回要追加到 pip 命令后的参数。
//
// 语义（与 mirror.go 的"优先 + 回落"完全一致）：
//  1. 配了镜像基址、且 <base>/pypi/simple/pip/ 探得通 → 用镜像，并**放宽超时**：
//     镜像站侧 8091 的按需缓存是**整个文件落盘后**才回第一个字节的，冷缓存拉大
//     轮子可能几十秒收不到任何数据（实测 31.9MB 的 mlx 轮子首字节等了 10.0 秒、
//     48.3MB 的 opencv 轮子等了 357.7 秒），而 pip 默认 15 秒读超时会直接掐断
//     （真机实测：127MB 的 torch 轮子冷缓存必失败）。所以给
//     --timeout 180 --retries 3：中途超时也会重试，重试时缓存通常已经热了。
//  2. 探不通 → 回落清华源（pip 直连时是流式的，不需要放宽超时）。
//  3. 离线模式（仅走镜像站）下探不通 → **明确失败、不回落**（见 MirrorOfflineOnly）。
//
// 探测复用 probeMirrorFile，所以单测可以用 m.mirrorFileProbeOverride 注入，
// 不需要为此新增一个覆盖字段（单测不许碰真实镜像站）。
func (m *Manager) pipMirrorArgs(ctx context.Context, result *InstallResult) ([]string, error) {
	if !m.MirrorEnabled() {
		if result != nil {
			result.step(ctx, "未启用镜像站，pip 索引："+pipIndexFallback)
		}
		return []string{"-i", pipIndexFallback}, nil
	}
	indexURL := m.mirrorSubPath(pipMirrorPath) + "/"
	probe := indexURL + pipIndexProbePackage + "/"
	if _, err := m.probeMirrorFile(ctx, probe); err != nil {
		if m.MirrorOfflineOnly(ctx) {
			return nil, m.offlineOnlyFail("PyPI 索引 "+probe, probe)
		}
		if result != nil {
			result.step(ctx, fmt.Sprintf(
				"镜像站上没有 PyPI 索引（%v），pip 改用清华源：%s", err, pipIndexFallback))
		}
		return []string{"-i", pipIndexFallback}, nil
	}
	if result != nil {
		result.step(ctx, "pip 索引：镜像站 "+indexURL+"（已探通；第二次安装直接命中镜像站缓存）")
	}
	args := []string{"-i", indexURL, "--timeout", "180", "--retries", "3"}
	// 镜像基址是 http（例如用户自建的内网镜像）时 pip 默认拒绝，
	// 必须显式信任这台主机；https 基址（例如 https://mirror.zizdog.com:8888）
	// 不需要，但按需加更稳，也避免以后把基址换成 http 时静默失效。
	if u, err := url.Parse(indexURL); err == nil && !strings.EqualFold(u.Scheme, "https") {
		if h := u.Hostname(); h != "" {
			args = append(args, "--trusted-host", h)
		}
	}
	return args, nil
}

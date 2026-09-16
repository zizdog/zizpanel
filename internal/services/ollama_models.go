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
//  Ollama 模型镜像（NAS 优先 + 回落公网 registry）
//
//  现状（2026-09-16 实测）：应用市场里的 ollama 条目只给一句
//  `ollama pull qwen2.5:7b`，面板**不代下**；模型来自 registry.ollama.ai。
//  本文件把"从 NAS 拉模型"这件事做成可测试的能力：
//    · 版本写死（qwen2.5:7b），并钉住清单本身的 sha256；
//    · NAS 优先（<mirror>/models/ollama，与 ~/.ollama/models 同布局），
//      探不通就回落公网 registry.ollama.ai，并把真实来源写进日志；
//    · 每个 blob 按它自己的 digest **逐个校验 sha256**，不符就删掉重来；
//    · 落到 Ollama 的本地模型仓库（OLLAMA_MODELS，默认 ~/.ollama/models），
//      这样 `ollama run qwen2.5:7b` 直接用本地文件、不再联网。
//
//  ⚠️ 这不是在反代 OCI registry 协议，而是"预置 blob + 写清单"：
//  绕开了 Ollama 对自定义 registry 的 HTTPS/域名要求（详见交付说明）。
//  局限：模型体积大（qwen2.5:7b 单个 blob 4.68 GB），首次同步要 NAS 能访问
//  registry.ollama.ai；之后面板/脚本从 NAS 取就是局域网速度。
// ============================================================================

const (
	// ollamaRegistryHost 是 Ollama 官方 registry（也是清单在本地仓库里的目录名）。
	ollamaRegistryHost = "registry.ollama.ai"
	// ollamaMirrorSubdir 是 NAS 镜像上模型仓库的子路径（与 ~/.ollama/models 同布局）。
	ollamaMirrorSubdir = "models/ollama"
	// ollamaBlobSeparator 是 Ollama 本地 blob 文件名的分隔符（sha256-<hex>）。
	ollamaBlobSeparator = "sha256-"
)

// OllamaBlob 是模型仓库里的一个内容寻址 blob。
type OllamaBlob struct {
	// Digest 是裸 sha256 十六进制（不带 "sha256:" 前缀）。
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// OllamaModelSpec 是一个已登记固定版本的 Ollama 模型。
type OllamaModelSpec struct {
	Name      string `json:"name"`      // qwen2.5
	Tag       string `json:"tag"`       // 7b
	Namespace string `json:"namespace"` // library
	// ManifestSHA256 钉住清单本身的字节（版本漂移会让下面所有 digest 失效）。
	ManifestSHA256 string       `json:"manifest_sha256"`
	Config         OllamaBlob   `json:"config"`
	Layers         []OllamaBlob `json:"layers"`
	// upstreamBase 仅供测试：替换公网 registry 基址（默认 https://registry.ollama.ai）。
	// 没有它，涉及"公网回落"的单测就会真连 registry.ollama.ai —— 违反
	// "单测不许碰真实服务"（AGENTS.md 第三节）。
	upstreamBase string
}

// Ref 返回 "名称:标签"（用户 `ollama pull` 时用的写法）。
func (s OllamaModelSpec) Ref() string { return s.Name + ":" + s.Tag }

// blobs 返回配置与全部层。
func (s OllamaModelSpec) blobs() []OllamaBlob {
	out := make([]OllamaBlob, 0, len(s.Layers)+1)
	out = append(out, s.Config)
	out = append(out, s.Layers...)
	return out
}

// TotalBytes 是模型全部 blob 的字节数（日志用）。
func (s OllamaModelSpec) TotalBytes() int64 {
	var n int64
	for _, b := range s.blobs() {
		n += b.Size
	}
	return n
}

// manifestRelPath 是清单在 Ollama 本地模型仓库里的相对路径。
func (s OllamaModelSpec) manifestRelPath() string {
	return filepath.Join("manifests", ollamaRegistryHost, s.Namespace, s.Name, s.Tag)
}

// upstreamManifestURL / upstreamBlobURL 是公网 registry 的地址。
func (s OllamaModelSpec) upstreamBaseURL() string {
	if s.upstreamBase != "" {
		return strings.TrimRight(s.upstreamBase, "/")
	}
	return "https://" + ollamaRegistryHost
}

func (s OllamaModelSpec) upstreamManifestURL() string {
	return fmt.Sprintf("%s/v2/%s/%s/manifests/%s",
		s.upstreamBaseURL(), s.Namespace, s.Name, s.Tag)
}

func (s OllamaModelSpec) upstreamBlobURL(digest string) string {
	return fmt.Sprintf("%s/v2/%s/%s/blobs/sha256:%s",
		s.upstreamBaseURL(), s.Namespace, s.Name, digest)
}

// ollamaModels 是已登记固定版本的模型（键是 `ollama pull` 的写法）。
//
// 数值全部来自 2026-09-16 从 registry.ollama.ai 实测取到的清单，
// 清单 sha256 与 registry 返回的 `ollama-content-digest` 头一致。
var ollamaModels = map[string]OllamaModelSpec{
	"qwen2.5:7b": {
		Name: "qwen2.5", Tag: "7b", Namespace: "library",
		ManifestSHA256: "845dbda0ea48ed749caafd9e6037047aa19acfcfd82e704d7ca97d631a0b697e",
		Config:         OllamaBlob{Digest: "2f15b3218f0552c60647ce60ada83632d2c09755b16259b13e3e4458e9ae419d", Size: 487},
		Layers: []OllamaBlob{
			{Digest: "2bada8a7450677000f678be90653b85d364de7db25eb5ea54136ada5f3933730", Size: 4683073952},
			{Digest: "66b9ea09bd5b7099cbb4fc820f31b575c0366fa439b08245566692c6784e281e", Size: 68},
			{Digest: "eb4402837c7829a690fa845de4d7f3fd842c2adee476d5341da8a46ea9255175", Size: 1482},
			{Digest: "832dd9e00a68dd83b3c3fb9f5588dad7dcf337a0db50f7d9483f310cd292e92e", Size: 11343},
		},
	},
}

// OllamaModelFor 查一个已登记固定版本的模型（ref 形如 "qwen2.5:7b"）。
func OllamaModelFor(ref string) (OllamaModelSpec, bool) {
	s, ok := ollamaModels[strings.TrimSpace(ref)]
	return s, ok
}

// OllamaModelMirrorURL 返回模型清单在 NAS 镜像上的地址。
//
// 用 <base>/models/ollama/manifests/registry.ollama.ai/... （Ollama 本地仓库
// 的同一套相对路径）而不是造一个新前缀：这样镜像目录能被直接拷进
// ~/.ollama/models，少一层映射、少一类"路径写错"。
func (m *Manager) OllamaModelMirrorURL(spec OllamaModelSpec) string {
	return strings.Join([]string{
		m.mirrorSubPath(ollamaMirrorSubdir), "manifests", ollamaRegistryHost,
		spec.Namespace, spec.Name, spec.Tag,
	}, "/")
}

// ollamaSource 是这次拉取的一个候选来源。
type ollamaSource struct {
	label       string
	manifestURL string
	blobURL     func(digest string) string
}

// ollamaModelSources 选出这次从哪儿拉模型：NAS 优先，探不通回落公网 registry。
//
// 与 iopaintWeightSources 同一风格：探的是 NAS 上**这个模型的清单**，
// 探到了才排第一并如实标注；探不到就明说"镜像上没有，回落公网"。
func (m *Manager) ollamaModelSources(ctx context.Context, spec OllamaModelSpec, logf func(string)) []ollamaSource {
	if logf == nil {
		logf = func(string) {}
	}
	upstream := ollamaSource{
		label:       "公网 registry.ollama.ai",
		manifestURL: spec.upstreamManifestURL(),
		blobURL:     spec.upstreamBlobURL,
	}
	if !m.MirrorEnabled() {
		logf("未启用 NAS 镜像，模型来源：" + upstream.manifestURL)
		return []ollamaSource{upstream}
	}
	mirrorManifest := m.OllamaModelMirrorURL(spec)
	if _, err := m.probeMirrorFile(ctx, mirrorManifest); err != nil {
		logf(fmt.Sprintf("NAS 镜像上没有 %s 这个模型（%v），回落到公网 registry：%s",
			spec.Ref(), err, upstream.manifestURL))
		return []ollamaSource{upstream}
	}
	mirrorBase := m.mirrorSubPath(ollamaMirrorSubdir)
	mirror := ollamaSource{
		label:       "NAS 镜像",
		manifestURL: mirrorManifest,
		blobURL: func(digest string) string {
			return mirrorBase + "/blobs/" + ollamaBlobSeparator + digest
		},
	}
	logf(fmt.Sprintf("NAS 镜像上有 %s（已探通，清单 %s），优先从 NAS 拉取", spec.Ref(), mirrorManifest))
	return []ollamaSource{mirror, upstream}
}

// ollamaModelTimeout 是单个 blob 的下载超时。
//
// qwen2.5:7b 的最大 blob 有 4.68 GB；NAS 局域网实测 6+ MB/s，但公网回落到
// 2~3 MB/s 时要约半小时。给 2 小时是为了不误杀，同时 fetchToFile 的停滞
// 看门狗会拦住"读得动但永远下不完"的病态情况。
const ollamaModelTimeout = 2 * time.Hour

// EnsureOllamaModel 把某个模型落到 modelsDir（Ollama 的 OLLAMA_MODELS 目录）。
//
// NAS 优先 → 回落公网 → 逐 blob 校验 sha256。返回**真实来源标签**。
// 已经存在且校验通过的 blob 会被跳过，所以重复调用是廉价的（断点续传式的幂等）。
func (m *Manager) EnsureOllamaModel(ctx context.Context, ref, modelsDir string,
	logf func(string)) (string, error) {

	if logf == nil {
		logf = func(string) {}
	}
	spec, ok := OllamaModelFor(ref)
	if !ok {
		return "", fmt.Errorf("没有为 Ollama 模型「%s」登记固定版本（不追 latest）", ref)
	}
	if strings.TrimSpace(modelsDir) == "" {
		return "", fmt.Errorf("缺少 Ollama 模型目录")
	}
	srcs := m.ollamaModelSources(ctx, spec, logf)
	var lastErr error
	for i, src := range srcs {
		label := src.label
		if i > 0 {
			label += "（回落）"
		}
		logf(fmt.Sprintf("拉取 Ollama 模型 %s ← %s", spec.Ref(), label))
		if err := m.materializeOllamaModel(ctx, spec, src, modelsDir, logf); err != nil {
			lastErr = fmt.Errorf("%s：%w", label, err)
			logf("  失败：" + err.Error())
			continue
		}
		logf(fmt.Sprintf("  来源：%s，全部 blob 校验通过（共 %s）",
			label, humanBytes(spec.TotalBytes())))
		return label, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的来源")
	}
	return "", fmt.Errorf("拉取 Ollama 模型 %s 失败：%w", spec.Ref(), lastErr)
}

// materializeOllamaModel 从 src 把清单与全部 blob 落到 modelsDir 并校验。
func (m *Manager) materializeOllamaModel(ctx context.Context, spec OllamaModelSpec,
	src ollamaSource, modelsDir string, logf func(string)) error {

	// ① 清单：先下到 .part，校验清单 sha256（钉住版本）后再落位。
	manifestPath := filepath.Join(modelsDir, spec.manifestRelPath())
	if got, err := fileSHA256(manifestPath); err == nil && strings.EqualFold(got, spec.ManifestSHA256) {
		logf("  清单已存在且 sha256 一致，跳过下载")
	} else {
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
			return fmt.Errorf("创建清单目录失败：%w", err)
		}
		if err := m.fetchFile(ctx, src.manifestURL, manifestPath, nil); err != nil {
			return fmt.Errorf("下载清单失败：%w", err)
		}
		got, err := fileSHA256(manifestPath)
		if err != nil {
			_ = os.Remove(manifestPath)
			return fmt.Errorf("计算清单 sha256 失败：%w", err)
		}
		if !strings.EqualFold(got, spec.ManifestSHA256) {
			_ = os.Remove(manifestPath)
			return fmt.Errorf("清单 sha256 不符（期望 %s，实际 %s），已删除",
				spec.ManifestSHA256, got)
		}
		logf("  清单 sha256 校验通过")
	}

	// ② 每个 blob 按**它自己的 digest** 校验（内容寻址，这是最强的校验）。
	blobDir := filepath.Join(modelsDir, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return fmt.Errorf("创建 blob 目录失败：%w", err)
	}
	for _, b := range spec.blobs() {
		dest := filepath.Join(blobDir, ollamaBlobSeparator+b.Digest)
		if got, err := fileSHA256(dest); err == nil && strings.EqualFold(got, b.Digest) {
			logf(fmt.Sprintf("  blob %s… 已存在且校验通过（%s）",
				b.Digest[:12], humanBytes(fileSizeOrZero(dest))))
			continue
		}
		logf(fmt.Sprintf("  下载 blob %s…（%s）", b.Digest[:12], humanBytes(b.Size)))
		attemptCtx, cancel := context.WithTimeout(ctx, ollamaModelTimeout)
		started := time.Now()
		err := m.fetchFile(attemptCtx, src.blobURL(b.Digest), dest, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("下载 blob %s… 失败：%w", b.Digest[:12], err)
		}
		got, err := fileSHA256(dest)
		if err != nil {
			_ = os.Remove(dest)
			return fmt.Errorf("计算 blob %s… 的 sha256 失败：%w", b.Digest[:12], err)
		}
		if !strings.EqualFold(got, b.Digest) {
			_ = os.Remove(dest)
			return fmt.Errorf("blob %s… sha256 不符（期望 %s，实际 %s），已删除",
				b.Digest[:12], b.Digest, got)
		}
		logf(fmt.Sprintf("  blob %s… 校验通过（用时 %.0f 秒）",
			b.Digest[:12], time.Since(started).Seconds()))
	}
	return nil
}

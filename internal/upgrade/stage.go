package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// 尺寸与超时上限。存在的意义是"被恶意源拖死/撑爆磁盘"这类问题：
// 面板是长期在线的服务，一次失控的下载就能把它拖垮。
const (
	maxManifestBytes = 256 << 10 // 256 KB
	maxTarballBytes  = 512 << 20 // 512 MB
	maxExtractedBin  = 128 << 20 // 单个二进制上限 128 MB
	maxTarEntries    = 4096      // 防止归档炸弹塞进海量小文件
	manifestTimeout  = 20 * time.Second
	downloadTimeout  = 10 * time.Minute
)

// httpClient 是升级专用的客户端。
//
// 刻意不复用面板其它地方的客户端：升级涉及长下载与外部主机，
// 需要自己的超时策略；同时禁止重定向到非 http(s) 的奇怪协议。
func httpClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向次数过多")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("拒绝重定向到不支持的协议: %s", req.URL.Scheme)
			}
			return nil
		},
	}
}

// Source 描述升级包的来源地址（一个目录，里面放 manifest.json 与 manifest.json.sig）。
type Source struct {
	// BaseURL 形如 https://example.com/zizpanel/releases
	BaseURL string
}

// Normalize 校验并规范化来源地址。
//
// 安全取舍：这里不强制 HTTPS，因为签名验证才是真正的防线 ——
// 攻击者即使完全控制下载通道，也拿不出配对的私钥。
// 但对明文 HTTP 给出明确警告，让用户知道自己在信任什么。
func (s Source) Normalize() (string, error) {
	raw := strings.TrimSpace(s.BaseURL)
	if raw == "" {
		return "", errors.New("升级源地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("升级源地址不合法: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// 允许，但只对回环/内网地址静默放行；公网明文给出提示由上层展示
	default:
		return "", fmt.Errorf("升级源只支持 http/https，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("升级源地址缺少主机名")
	}
	return strings.TrimRight(raw, "/"), nil
}

// IsPlainHTTPToPublicHost 判断是否在用明文 HTTP 访问非内网地址。
// 上层用它决定是否给用户一个显著警告。
func IsPlainHTTPToPublicHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast()
	}
	return true
}

// FetchManifestRaw 下载清单与签名原文，但**不做验签/解析**。
//
// 把"下载"与"验签"拆成两步，是为了让多候选回落（FetchManifestAny）在每次
// 候选下载后统一走同一份验签实现，而不是让下载函数各自决定信不信。
func FetchManifestRaw(ctx context.Context, baseURL string) ([]byte, []byte, error) {
	src := Source{BaseURL: baseURL}
	base, err := src.Normalize()
	if err != nil {
		return nil, nil, err
	}
	// 没配公钥就根本不发请求：fail closed，避免把"没验证"当成"验证通过"。
	if !HasPublicKey() {
		return nil, nil, ErrNoPublicKey
	}

	manifestURL := base + "/manifest.json"
	sigURL := base + "/manifest.json.sig"

	data, err := getBytes(ctx, manifestURL, maxManifestBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("下载清单失败: %w", err)
	}
	sig, err := getBytes(ctx, sigURL, maxManifestBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("下载清单签名失败: %w", err)
	}
	return data, sig, nil
}

// FetchManifest 下载并**验签**清单。这是信任链的起点。
func FetchManifest(ctx context.Context, baseURL string) (*Manifest, []byte, error) {
	data, sig, err := FetchManifestRaw(ctx, baseURL)
	if err != nil {
		// 单源失败：网络原因附统一提示（多候选在 FetchManifestAny 汇总层附，避免重复）。
		return nil, nil, services.AppendNetworkHint(err)
	}
	if err := VerifyManifest(data, sig); err != nil {
		return nil, nil, err
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, nil, err
	}
	return m, data, nil
}

// getBytes 做一次带上限的 GET。
func getBytes(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "zizpanel-upgrade")
	resp, err := httpClient(manifestTimeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 多读 1 字节，用来判断"是否超限"，而不是靠 Content-Length（可能是假的）
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("响应体超过 %d 字节上限，已中止", limit)
	}
	return b, nil
}

// DownloadTarball 下载升级包到 destPath，并校验 SHA-256。
//
// 先写 .part 再原子改名：中途失败/断电不会留下一个"看起来完整"的半截文件，
// 否则下次升级可能拿它去做校验（失败）或者更糟 —— 通过尺寸检查后被解包。
func DownloadTarball(ctx context.Context, rawURL, destPath, wantSHA string) (string, error) {
	return DownloadTarballWithProgress(ctx, rawURL, destPath, wantSHA, nil)
}

// ProgressFunc 报告下载进度：written = 已写入字节，total = 服务端声明的总字节
// （-1 表示服务端没给 Content-Length）。
type ProgressFunc func(written, total int64)

// PhaseFunc 报告下载内部的**真实阶段切换**（下载完成 → 计算并比对 SHA-256）。
//
// 为什么要单独一个回调：界面上"下载中"和"校验中"是两件事，而校验（对 25MB
// 的包算 SHA-256，慢机器上要几百毫秒到数秒）发生在下载函数内部。没有这个回调，
// 面板只能在下载返回后补一句"已校验"，用户看到的就是进度条停在 100% 不动、
// 不知道是在校验还是卡死了。phase 是机器可读 id（见 PhaseVerify 等）。
type PhaseFunc func(phase, detail string)

// 下载内部阶段 id（PhaseFunc 的 phase 参数）。
const (
	PhaseVerify = "verify" // 正在计算/比对下载文件的 SHA-256
)

// DownloadTarballWithProgress 与 DownloadTarball 完全一致，只是每读一块就回调一次进度。
//
// 为什么需要（2026-09-20 用户要求"升级过程要有详细的内容展示"）：升级是一次
// 几十秒到十几分钟的长任务，界面上只写"正在下载"等于让用户干等；有了进度回调，
// 面板可以把"已下载 12.3 MB / 24.4 MB（50%）"实时写进升级状态，用户看得到在动。
//
// 回调是**同步**调用的（同一个 goroutine），实现里不要做重活；调用方会自己节流。
func DownloadTarballWithProgress(ctx context.Context, rawURL, destPath, wantSHA string, onProgress ProgressFunc) (string, error) {
	return DownloadTarballWithObserver(ctx, rawURL, destPath, wantSHA, onProgress, nil)
}

// DownloadTarballWithObserver 在 DownloadTarballWithProgress 之上，把下载内部的
// 真实阶段切换（下载完成 → 校验）也报告给调用方。
func DownloadTarballWithObserver(ctx context.Context, rawURL, destPath, wantSHA string, onProgress ProgressFunc, onPhase PhaseFunc) (string, error) {
	if err := validateSHA256(wantSHA); err != nil {
		return "", fmt.Errorf("期望的 sha256 不合法: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", err
	}
	part := destPath + ".part"
	_ = os.Remove(part)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "zizpanel-upgrade")
	resp, err := httpClient(downloadTimeout).Do(req)
	if err != nil {
		// 建连失败（DNS/拒绝/超时/TLS）：网络原因附统一提示。
		return "", services.AppendNetworkHint(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载升级包失败: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxTarballBytes {
		return "", fmt.Errorf("升级包声明的尺寸 %d 字节超过上限", resp.ContentLength)
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	var n int64
	var copyErr error
	if onProgress == nil {
		n, copyErr = io.Copy(f, io.LimitReader(resp.Body, maxTarballBytes+1))
	} else {
		n, copyErr = copyWithProgress(f, resp.Body, maxTarballBytes+1, resp.ContentLength, onProgress)
	}
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(part)
		// 传输中途断开（i/o timeout / connection reset by peer / unexpected EOF）：
		// 网络原因才附提示；写盘失败（no space left on device）不会被误报。
		return "", services.AppendNetworkHint(copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(part)
		return "", closeErr
	}
	if n > maxTarballBytes {
		_ = os.Remove(part)
		return "", fmt.Errorf("升级包超过 %d 字节上限，已中止", maxTarballBytes)
	}

	// 下载已结束，下面是真实的校验步骤 —— 如实告诉调用方，别让界面停在 100%。
	if onPhase != nil {
		onPhase(PhaseVerify, "正在计算并比对安装包的 SHA-256")
	}
	got, err := FileSHA256(part)
	if err != nil {
		_ = os.Remove(part)
		return "", err
	}
	if !EqualSHA256(got, wantSHA) {
		_ = os.Remove(part)
		return "", fmt.Errorf("升级包校验和不匹配（期望 %s，实际 %s），文件已丢弃", short(wantSHA), short(got))
	}
	if err := os.Rename(part, destPath); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	return destPath, nil
}

// copyWithProgress 是带进度回调的 io.Copy（上限语义与 io.Copy(LimitReader) 一致）。
//
// 为什么自己写循环而不是包一层 io.Reader：包装 reader 只能知道"读了多少"，
// 而这里要同时保证"写入成功后才计入进度"——写盘失败时必须立刻停下，
// 否则进度会显示成"下完了"而文件其实是坏的（谎报进度的另一种形式）。
func copyWithProgress(dst io.Writer, src io.Reader, limit int64, total int64, onProgress ProgressFunc) (int64, error) {
	buf := make([]byte, 256<<10)
	var written int64
	for {
		if written >= limit {
			// 触到上限：让调用方按"超过上限"处理（与 io.Copy(LimitReader) 的语义一致）。
			return written, nil
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
			onProgress(written, total)
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// ExtractBinaries 从 tar.gz 中解出我们需要的二进制到 destDir。
//
// 安全要点（每一条都对应一类真实攻击）：
//   - 路径穿越（zip-slip）：只接受白名单文件名，且断言 Clean 后不含 ".."
//   - 符号链接：直接拒绝，否则可以用链接把后续写入引到任意位置
//   - 归档炸弹：限制条目数与单文件大小
//   - 权限位：不继承归档里的权限，统一用给定的模式
func ExtractBinaries(tarPath, destDir string, wanted []string) (map[string]string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("不是合法的 gzip 文件: %w", err)
	}
	defer func() { _ = gz.Close() }()

	want := make(map[string]string, len(wanted))
	for _, name := range wanted {
		want["./"+name] = name // 发布包里的路径形如 ./zizpanel
		want[name] = name
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	out := make(map[string]string)

	tr := tar.NewReader(gz)
	entries := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档失败: %w", err)
		}
		entries++
		if entries > maxTarEntries {
			return nil, fmt.Errorf("归档条目数超过 %d，疑似归档炸弹", maxTarEntries)
		}

		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			return nil, fmt.Errorf("归档内含非法路径 %q，已拒绝", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			return nil, fmt.Errorf("归档内含链接 %q，出于安全考虑已拒绝", hdr.Name)
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			continue
		}

		target, ok := want[name]
		if !ok {
			continue // 不关心的文件（install.sh、tools/ 等）直接跳过
		}
		if hdr.Size > maxExtractedBin {
			return nil, fmt.Errorf("%s 尺寸 %d 超过上限", name, hdr.Size)
		}

		dst := filepath.Join(destDir, target)
		w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(w, io.LimitReader(tr, maxExtractedBin+1)); err != nil {
			_ = w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		out[target] = dst
	}

	for _, name := range wanted {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("升级包里缺少 %s（这可能是一个不完整或伪造的包）", name)
		}
	}
	return out, nil
}

// Package imgopt 用 libvips 批量压缩图片（面板「文件管理 → 🖼️ 图片压缩」的引擎层）。
//
// 为什么走 **vips 命令行**而不是把 libvips 链进面板二进制（用户的参考文档给的是
// govips 这条 CGO 路线）：
//
//   - 面板是**纯 Go 单二进制**：`make release` 要交叉构建 arm64/amd64、`-trimpath`
//     注入版本号，链路里没有 CGO 工具链；引入 CGO 还要每个构建机都装 libvips。
//   - 更要紧的是**运行期**：CGO 版会把 libvips 的动态库绑到面板进程上，用户机器上
//     少一个 dylib 面板就起不来 —— 面板是管所有东西的那个进程，不能用这种方式冒险。
//   - 引擎完全一样：`vips` 命令行就是 libvips 本身（与 govips/sharp 同一个底层），
//     文档里那些导出参数（Q / strip / effort / compression）逐字可用。
//   - 代价只是"每个文件一个进程"（实测启动几十毫秒），而单张图的编解码才是耗时大头。
//
// 判据同样遵守项目的铁律：**能做到才说能做到**。引擎不在就是不在（返回
// DegradeReason），不许静默退化成一个"看起来压缩了、其实只是复制了文件"的动作。
package imgopt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Format 是导出格式。
type Format string

const (
	// FormatKeep 保持原扩展名（只重新编码）。
	FormatKeep Format = "keep"
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
	FormatWebP Format = "webp"
	FormatAVIF Format = "avif"
)

// Formats 是界面可选的格式（顺序即界面顺序）。
var Formats = []Format{FormatKeep, FormatWebP, FormatAVIF, FormatJPEG, FormatPNG}

// ValidFormat 判断格式是否受支持。
func ValidFormat(f Format) bool {
	for _, x := range Formats {
		if x == f {
			return true
		}
	}
	return false
}

// ExtFor 返回某个格式对应的输出扩展名（FormatKeep 返回空串 = 用原扩展名）。
func ExtFor(f Format) string {
	switch f {
	case FormatJPEG:
		return ".jpg"
	case FormatPNG:
		return ".png"
	case FormatWebP:
		return ".webp"
	case FormatAVIF:
		return ".avif"
	}
	return ""
}

// ImageExts 是**允许处理**的输入扩展名（白名单）。
//
// 为什么要白名单：这个功能会对文件做**原地覆盖**（用户可选），只有图像才允许
// 进入这条路径。凭"vips 能打开"来判定等于让任意文件都可能被覆盖。
var ImageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".avif": true, ".heic": true, ".heif": true, ".tif": true, ".tiff": true,
	".gif": true, ".bmp": true,
}

// IsImagePath 判断路径是不是允许处理的图片（只看扩展名）。
func IsImagePath(p string) bool {
	return ImageExts[strings.ToLower(filepath.Ext(p))]
}

// Options 是一次压缩的参数。
type Options struct {
	// Quality 1-100；<=0 时用 DefaultQuality。
	Quality int `json:"quality"`
	// Format 导出格式（keep = 保持原格式）。
	Format Format `json:"format"`
	// MaxEdge 最长边（像素）；0 = 不缩放。只缩小、绝不放大。
	MaxEdge int `json:"max_edge"`
	// StripMetadata 为 true 时去掉 EXIF/ICC 等元数据（体积更小）；
	// false 时保留（照片的拍摄信息、色彩描述还在）。
	StripMetadata bool `json:"strip_metadata"`
	// Overwrite 为 true 时**原地覆盖**原文件；false 时另存为 <名字>.min.<ext>。
	Overwrite bool `json:"overwrite"`
}

// DefaultQuality 是默认质量（文档给的经验值 82）。
const DefaultQuality = 82

// Normalize 补齐默认值并校验范围（返回人话错误）。
func (o Options) Normalize() (Options, error) {
	if o.Quality <= 0 {
		o.Quality = DefaultQuality
	}
	if o.Quality > 100 {
		return o, fmt.Errorf("质量只能在 1-100 之间，收到 %d", o.Quality)
	}
	if o.Format == "" {
		o.Format = FormatKeep
	}
	if !ValidFormat(o.Format) {
		return o, fmt.Errorf("不支持的格式 %q（可选：keep / webp / avif / jpeg / png）", o.Format)
	}
	if o.MaxEdge < 0 || o.MaxEdge > 20000 {
		return o, fmt.Errorf("最长边只能是 0（不缩放）或 1-20000 像素，收到 %d", o.MaxEdge)
	}
	return o, nil
}

// Engine 是"这台机器上的 libvips 命令行"。
type Engine struct {
	// Bin 是 vips 可执行文件的绝对路径（空 = 没找到）。
	Bin string
	// Version 是 `vips --version` 的输出（例如 "vips-8.18.6"）。
	Version string
	// Reason 非空表示引擎不可用（人话：为什么、怎么办）。
	Reason string
}

// Available 表示引擎可用。
func (e Engine) Available() bool { return e.Bin != "" && e.Reason == "" }

// DetectEngine 探测引擎：先看二进制在不在（**只看运行体**，不看 brew 记录），
// 再看它真的能跑起来（`--version`）。
//
// brewPrefix 来自面板配置；为空时退回 PATH 查找。
//
// 三种结果严格区分，绝不混为一谈：
//   - 二进制不存在            → 没装（Reason 说"去应用市场装"）
//   - 二进制在、但执行失败    → 装了但不可用（Reason 带上真实错误）
//   - 二进制在、能报版本      → 可用
func DetectEngine(brewPrefix string) Engine {
	cands := []string{}
	if brewPrefix != "" {
		cands = append(cands, filepath.Join(brewPrefix, "bin", "vips"))
	}
	if p, err := exec.LookPath("vips"); err == nil {
		cands = append(cands, p)
	}
	for _, bin := range cands {
		st, err := os.Stat(bin)
		if err != nil || st.IsDir() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
		cancel()
		ver := strings.TrimSpace(string(out))
		if err != nil {
			return Engine{Bin: bin, Version: ver, Reason: fmt.Sprintf(
				"找到 %s，但它执行失败（%v）：%s。可能是 libvips 的动态库缺失或损坏，"+
					"可在终端跑 %s --version 看完整报错", bin, err, ver, bin)}
		}
		return Engine{Bin: bin, Version: ver}
	}
	return Engine{Reason: "这台机器上还没有 libvips（vips 命令）。" +
		"到「应用市场 → 图片压缩（libvips）」一键安装即可 —— 它是原生 arm64 包，" +
		"不需要 Docker，也不需要 PHP/Node。"}
}

// Result 是单个文件的压缩结果。
type Result struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	// Before / After 是字节数（After=0 表示没产出，见 Error/Skipped）。
	Before int64 `json:"before"`
	After  int64 `json:"after"`
	// Skipped 为 true 表示"压完反而更大，已保留原文件"（After 记录压出来的大小）。
	Skipped bool `json:"skipped"`
	// Error 非空表示这个文件失败（其他文件继续）。
	Error string `json:"error,omitempty"`
}

// Saved 返回省下的字节数（负数表示压完更大）。
func (r Result) Saved() int64 { return r.Before - r.After }

// MaxEdgeArgs 生成缩放入参；MaxEdge<=0 时返回 nil（不缩放）。
//
// 用 `vips thumbnail`：它按 EXIF 自动旋转、对 JPEG 走 shrink-on-load（快得多），
// 且**只缩不放**（上游语义：图比目标小就原样保留）。文档强调的"绝不放大"就靠它。
func (o Options) thumbnailArgs(src, dst string) []string {
	args := []string{"thumbnail", src, dst}
	if o.MaxEdge > 0 {
		args = append(args, fmt.Sprintf("%d", o.MaxEdge))
		// --height 给一个上限，确保是"装进正方形"而不是只限宽。
		args = append(args, "--height", fmt.Sprintf("%d", o.MaxEdge))
	}
	return args
}

// exportBracket 生成 vips 的导出参数（写在输出文件名后的方括号里）。
//
// 这些参数逐字对应 libvips 的导出器字段（也是 govips 那几个 ExportParams 的
// 命令行写法）：Q 质量、strip 去元数据、effort 压缩努力、interlace 渐进式。
func (o Options) exportBracket(dstExt string) string {
	opts := []string{}
	switch strings.ToLower(dstExt) {
	case ".jpg", ".jpeg":
		opts = append(opts, "Q="+fmt.Sprint(o.Quality), "optimize-coding")
	case ".webp":
		opts = append(opts, "Q="+fmt.Sprint(o.Quality), "effort=4")
	case ".avif":
		opts = append(opts, "Q="+fmt.Sprint(o.Quality), "effort=4")
	case ".png":
		// PNG 是无损格式："质量"在它这里没有意义，用压缩级别代替。
		// 刻意**不**声明 Q（免得用户以为调质量能减小 PNG）。
		opts = append(opts, "compression=9")
	}
	if o.StripMetadata {
		opts = append(opts, "strip")
	}
	if len(opts) == 0 {
		return ""
	}
	return "[" + strings.Join(opts, ",") + "]"
}

// OutputPath 算出输出路径：Overwrite 时就是原路径；否则 <dir>/<base>.min<ext>。
//
// ext 为 FormatKeep 时用原扩展名（只重新编码，不改容器）。
func OutputPath(src string, o Options) string {
	if o.Overwrite {
		return src
	}
	ext := filepath.Ext(src)
	if e := ExtFor(o.Format); e != "" {
		ext = e
	}
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	return filepath.Join(filepath.Dir(src), base+".min"+ext)
}

// CompressFile 压缩一个文件。
//
// 行为约定（每条都有测试锁着）：
//   - 输出比输入**大**时：删掉输出、保留原文件，Result.Skipped=true（绝不把文件弄大）；
//   - Overwrite=false 时输出不覆盖已有文件（同名的 .min 会先删掉重写）；
//   - 失败一律带回 vips 的真实 stderr，不吞错。
func (e Engine) CompressFile(ctx context.Context, src string, o Options) (Result, error) {
	res := Result{Src: src}
	if !e.Available() {
		return res, fmt.Errorf("图片压缩引擎不可用：%s", e.Reason)
	}
	if !IsImagePath(src) {
		return res, fmt.Errorf("%s 不是支持的图片格式（%s）", filepath.Base(src),
			strings.Join(extList(), " / "))
	}
	st, err := os.Stat(src)
	if err != nil {
		return res, fmt.Errorf("读取 %s 失败: %w", src, err)
	}
	res.Before = st.Size()

	dst := OutputPath(src, o)
	res.Dst = dst
	ext := strings.ToLower(filepath.Ext(dst))
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(src))
	}
	// 输出与输入同一个文件时（Overwrite），vips 必须先写临时文件再替换 ——
	// 直接写原路径会读到写坏的一半（libvips 会拒绝，但报错很难懂）。
	// 所以这里一律"先写临时文件，成功后原子替换"。
	tmp := dst + ".zp-tmp-" + fmt.Sprint(os.Getpid()) + ext
	defer func() { _ = os.Remove(tmp) }()

	args := o.thumbnailArgs(src, tmp+o.exportBracket(ext))
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, e.Bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return res, fmt.Errorf("压缩 %s 超时（超过 10 分钟）", filepath.Base(src))
		}
		return res, fmt.Errorf("vips 处理 %s 失败: %v\n%s", filepath.Base(src), err,
			strings.TrimSpace(string(out)))
	}
	ost, err := os.Stat(tmp)
	if err != nil {
		return res, fmt.Errorf("vips 没有产出 %s 的结果: %w", filepath.Base(src), err)
	}
	res.After = ost.Size()

	// 压完更大：保留原文件，如实记 Skipped。这条很重要 —— JPEG 重压、已经是
	// WebP 的图、极小的图标都可能变大，把用户的文件弄大还报"成功"是谎报。
	if res.After >= res.Before && !o.Overwrite {
		res.Skipped = true
		return res, nil
	}
	if res.After >= res.Before && o.Overwrite {
		// 覆盖模式下压完更大：也保留原文件（同样不许把文件弄大）。
		res.Skipped = true
		res.Dst = src
		return res, nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		return res, fmt.Errorf("写入 %s 失败: %w", dst, err)
	}
	return res, nil
}

// extList 把白名单扩展名整理成人读列表（错误信息用）。
func extList() []string {
	out := []string{".jpg", ".jpeg", ".png", ".webp", ".avif", ".heic", ".tif", ".gif", ".bmp"}
	return out
}

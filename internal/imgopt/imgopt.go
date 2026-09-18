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
	"regexp"
	"strconv"
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
	if o.MaxEdge < 0 || o.MaxEdge > maxEdgeLimit {
		return o, fmt.Errorf("最长边只能是 0（不缩放）或 1-%d 像素，收到 %d", maxEdgeLimit, o.MaxEdge)
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
	// Ops 是 `vips -l` 里**真实可用**的操作名集合（jpegload / webpsave …）。
	//
	// 为什么需要它：Homebrew 的 vips 瓶（sharp-libvips 构建）把动态模块路径编译成了
	// 构建机路径，heif / jxl / magick 这些模块运行时加载不了 —— `vips --vips-config`
	// 里写 true 也不算数。所以"这台机器能读写哪些格式"必须问**运行体**（见 probeOps）。
	// nil = 没探测成功：此时调用方不做格式预检（交给 vips 自己报错），绝不误判成全都支持。
	Ops map[string]bool
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
		return Engine{Bin: bin, Version: ver, Ops: probeOps(bin)}
	}
	return Engine{Reason: "这台机器上还没有 libvips（vips 命令）。" +
		"到「应用市场 → 图片压缩（libvips）」一键安装即可 —— 它是原生 arm64 包，" +
		"不需要 Docker，也不需要 PHP/Node。"}
}

// probeOps 跑一次 `vips -l` 把**真实可用的操作名**收集起来。
//
// 为什么要这一步（2026-09-18 实测）：Homebrew 的 vips 8.18.6 瓶是拿 sharp-libvips
// 构建的，动态模块（heif / jxl / magick / poppler / openslide）的搜索路径被编译成了
// 构建机路径 `/Users/runner/work/sharp-libvips/sharp-libvips/target/lib/...` ——
// 运行时那些模块**根本不会加载**，于是 `vips --vips-config` 里明明写着
// "HEIC/AVIF load/save with libheif: true"，实际 `vips -l` 里连 heifload 都没有，
// 读写 HEIC 直接 `is not a known file format`。
//
// 所以"支持哪些格式"这件事**必须问运行体**，不能信编译期配置，更不能硬编码。
// 解析失败返回 nil：调用方据此不做预检（交给 vips 自己报错），而不是误判成"全都支持"。
func probeOps(bin string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-l").Output()
	if err != nil {
		return nil
	}
	ops := map[string]bool{}
	for _, m := range regexp.MustCompile(`\(([a-z0-9_]+)\)`).FindAllStringSubmatch(string(out), -1) {
		ops[m[1]] = true
	}
	if len(ops) == 0 {
		return nil
	}
	return ops
}

// loaderForExt / saverForExt 把输入输出扩展名对到 vips 的操作名。
//
// 只列"这台项目允许处理的格式"：没列到的扩展名不做预检（交给 vips 自己报错）。
var (
	loaderForExt = map[string]string{
		".jpg": "jpegload", ".jpeg": "jpegload", ".png": "pngload", ".webp": "webpload",
		".tif": "tiffload", ".tiff": "tiffload", ".gif": "gifload",
		".heic": "heifload", ".heif": "heifload", ".avif": "avifload", ".bmp": "magickload",
	}
	saverForExt = map[string]string{
		".jpg": "jpegsave", ".jpeg": "jpegsave", ".png": "pngsave", ".webp": "webpsave",
		".tif": "tiffsave", ".tiff": "tiffsave", ".gif": "gifsave",
		".heic": "heifsave", ".heif": "heifsave", ".avif": "avifsave",
	}
)

// canLoad / canSave：扩展名对应的操作在不在。
//
// Ops 为空（没探测成功）时**一律返回 true**：预检是"提前给出可操作的好错误"，
// 不是新的拦截 —— 探测失败时不能反过来把能用的格式也拦掉（那才是谎报）。
func (e Engine) canLoad(ext string) bool {
	if len(e.Ops) == 0 {
		return true
	}
	op := loaderForExt[ext]
	return op == "" || e.Ops[op]
}

func (e Engine) canSave(ext string) bool {
	if len(e.Ops) == 0 {
		return true
	}
	op := saverForExt[ext]
	return op == "" || e.Ops[op]
}

// unsupportedLoadErr 是"读不了这个输入格式"的人话错误。
func unsupportedLoadErr(name, ext, op string) error {
	return fmt.Errorf("%s：这台机器上的 vips 读不了 %s（缺少 %s 模块 —— Homebrew 的 vips 瓶"+
		"把动态模块路径编译成了构建机路径，运行时加载不了）。请先把它转成 JPEG / PNG 再压",
		name, strings.TrimPrefix(ext, "."), op)
}

// unsupportedSaveErr 是"写不了这个输出格式"的人话错误。
//
// 最常见的触发场景：用户拿 iPhone 的 HEIC 来压，而「输出格式」是默认的
// 「保持原格式」—— 这台机器上的 vips 写不了 HEIC。必须给出**能照做的下一步**，
// 而不是抛一句 vips 的 `VipsForeignSave: ... is not a known file format`。
func unsupportedSaveErr(name, ext, op string) error {
	return fmt.Errorf("%s：这台机器上的 vips 不能输出 %s（缺少 %s 模块 —— Homebrew 的 vips 瓶"+
		"把动态模块路径编译成了构建机路径，运行时加载不了）。"+
		"请把「输出格式」改成 WebP（同质量通常更小）或 JPEG / PNG 再压一次",
		name, strings.TrimPrefix(ext, "."), op)
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

// maxEdgeLimit 是「最长边」允许的最大像素值。
//
// 两个用途：① Validate 用它拒绝过大的值；② MaxEdge<=0（用户选"不缩放"）时，
// thumbnail 命令**必须**有一个宽度参数，用它当"足够大的上限"（见 thumbnailArgs）。
const maxEdgeLimit = 20000

// thumbnailArgs 生成缩放入参。
//
// ⚠️ 这里出过一次**默认值就坏**的事故（2026-09-18 用户报障）：
// 老实现是 `["thumbnail", src, dst]`，只有 MaxEdge>0 时才补宽度 ——
// 而 `vips thumbnail` 至少要 in / out / width **三个位置参数**，
// 少给宽度它会直接 `thumbnail: too few arguments` 退出 1。
// 而界面上「最长边」的下拉框**默认选中的就是「不缩放」（MaxEdge=0）**，
// 于是"默认设置下每一张图都压缩失败"，用户看到的正是
// `vips 处理 xxx.heic 失败: exit status 1 / thumbnail: too few arguments`。
//
// 现在的做法：MaxEdge<=0 时给一个"足够大的上限"（maxEdgeLimit），而不是省略参数：
//
//	· 保留 `thumbnail` 的两个关键好处：按 EXIF 自动旋转（否则手机竖拍会被转横）、
//	  对 JPEG 走 shrink-on-load（快得多）；
//	· thumbnail 的语义是"只缩不放"，所以对不超过该上限的图**完全等价于不缩放**；
//	· 唯一的偏差：比 maxEdgeLimit 还大的图会被缩到 maxEdgeLimit。这一档本就是
//	  "不缩放"（缩小而非放大），偏差写在这里备查，不藏在注释里。
//
// 这条不变量有门禁盯着：所有 MaxEdge 取值产出的命令都必须带宽度（见 imgopt_test.go）。
func (o Options) thumbnailArgs(src, dst string) []string {
	edge := o.MaxEdge
	if edge <= 0 {
		edge = maxEdgeLimit
	}
	// ⚠️ `--size down` 不是装饰，它是"只缩不放"这条承诺的**唯一**保障：
	// vips thumbnail 默认的 size 语义会**把小图放大到目标框**——实测
	// 900×700 的图 + `1280 --height 1280` → 输出 1280×996（放大 1.4 倍），
	// 而界面与文档都对用户承诺"比目标小的图保持原尺寸"。
	// 放大还有个更硬的后果：MaxEdge<=0 时 edge=20000，放大后直接超过 libwebp
	// 的 16383 上限 → `webpsave: image too large`（实测）。
	return []string{"thumbnail", src, dst, fmt.Sprintf("%d", edge),
		"--height", fmt.Sprintf("%d", edge), "--size", "down"}
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

// ---------------------------------------------------------------------------
//  HEIC / HEIF：vips 读不了时，用 macOS 自带的 sips 先转 PNG
// ---------------------------------------------------------------------------

// SipsBin 是 macOS 自带的图像转换工具（本项目只做 macOS，见 AGENTS 铁律 1）。
const SipsBin = "/usr/bin/sips"

// sipsReadableExts 是"这台机器的 vips 读不了、但 sips 能读"的输入格式。
//
// 为什么需要这条兜底（2026-09-18 实测，本机与生产机同款 Homebrew vips 8.18.6）：
// Homebrew 的 vips 瓶是拿 sharp-libvips 构建的，**动态模块**（heif / jxl / magick /
// poppler / openslide）的搜索路径被编译成了构建机路径 —— 运行时 vips 去找
// `/Users/runner/work/sharp-libvips/sharp-libvips/target/lib/vips-modules-8.18`，
// 于是 `vips -l` 里根本没有 heifload，读 HEIC 直接
// `VipsForeignLoad: "x.heic" is not a known file format`。
// `VIPS_MODULE_PATH` / `VIPS_LIBDIR` / `VIPS_PREFIX` 都覆盖不了（实测无效）。
//
// 而用户最常见的输入恰恰是 iPhone 拍的 HEIC。macOS 自带的 sips 用系统解码器
// 能读 HEIC，所以这类输入先转成 PNG（无损）再交给 vips 缩放/重编码。
// 这不是"假装支持"：sips 不在、或转换失败时，CompressFile 会**如实报错**
// 并说明真正的原因（缺 heif 模块 / sips 转换失败）。
var sipsReadableExts = map[string]bool{".heic": true, ".heif": true}

// convertViaSips 把 HEIC/HEIF 转成临时 PNG（放在系统临时目录，不动源目录 ——
// 文件管理里压缩时源目录可能是只读的）。
//
// 调用方负责删除返回的中间文件。
func (e Engine) convertViaSips(ctx context.Context, src string) (string, error) {
	if _, err := os.Stat(SipsBin); err != nil {
		return "", fmt.Errorf("%s 是 HEIC/HEIF，而这台机器上的 vips 读不了它"+
			"（Homebrew 的 vips 瓶把动态模块路径编译成了构建机路径，heif 模块加载不了），"+
			"系统自带的 sips 也用不了：%w", filepath.Base(src), err)
	}
	png := filepath.Join(os.TempDir(),
		"zp-sips-"+fmt.Sprint(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".png")
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(runCtx, SipsBin, "-s", "format", "png", src, "--out", png).
		CombinedOutput()
	if err != nil {
		_ = os.Remove(png)
		return "", fmt.Errorf("用系统 sips 把 %s 转成 PNG 失败: %v\n%s",
			filepath.Base(src), err, strings.TrimSpace(string(out)))
	}
	if _, serr := os.Stat(png); serr != nil {
		return "", fmt.Errorf("sips 说转换成功，却没有产出中间文件 %s：%w", png, serr)
	}
	return png, nil
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
	inExt := strings.ToLower(filepath.Ext(src))

	// 格式能力预检（判据来自 `vips -l` 的真实操作表，见 probeOps）。
	//
	// 为什么要提前拦：不拦的话用户拿到的是 vips 的原文
	// `VipsForeignSave: "x.min.heic" is not a known file format` —— 既看不出
	// "是这台机器的 vips 缺模块"，也看不出下一步该做什么。
	// Mac 上最常见的组合就是：iPhone 的 HEIC + 默认「保持原格式」。
	if !e.canSave(ext) {
		return res, unsupportedSaveErr(filepath.Base(src), ext, saverForExt[ext])
	}
	if !sipsReadableExts[inExt] && !e.canLoad(inExt) {
		return res, unsupportedLoadErr(filepath.Base(src), inExt, loaderForExt[inExt])
	}

	// 输出与输入同一个文件时（Overwrite），vips 必须先写临时文件再替换 ——
	// 直接写原路径会读到写坏的一半（libvips 会拒绝，但报错很难懂）。
	// 所以这里一律"先写临时文件，成功后原子替换"。
	tmp := dst + ".zp-tmp-" + fmt.Sprint(os.Getpid()) + ext
	defer func() { _ = os.Remove(tmp) }()

	// HEIC/HEIF：这台机器上的 vips 读不了（Homebrew 的模块路径编译问题，
	// 见 sipsReadableExts 的说明），先用系统 sips 转成 PNG 再压。
	// 输出路径仍按**原文件**算（用户看到的产物名不会变成 .png）。
	vipsSrc := src
	if sipsReadableExts[inExt] {
		png, cerr := e.convertViaSips(ctx, src)
		if cerr != nil {
			return res, cerr
		}
		defer func() { _ = os.Remove(png) }()
		vipsSrc = png
	}

	args := o.thumbnailArgs(vipsSrc, tmp+o.exportBracket(ext))
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

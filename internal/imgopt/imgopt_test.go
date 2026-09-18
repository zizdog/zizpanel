package imgopt

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ============================================================================
//  图片压缩引擎（libvips 命令行）的单元测试
//
//  全部走**假 vips**：单测不许依赖真机上装没装 vips（真机行为另有一条
//  TestCompressFileRealVips，装了才跑）。这里锁的是三件事：
//    ① 引擎探测要**如实**（不存在 / 存在但跑不起来 / 可用，三种分开）；
//    ② 命令参数的形状必须是 libvips 真正认的那套（Q / strip / effort / compression）；
//    ③ "压完更大就保留原文件"这条不许被绕过（把用户文件弄大 = 谎报成功）。
// ============================================================================

// fakeVips 写一个假 vips：记录收到的参数，并按脚本要求产出文件。
//
//	mode=ok     → 产出一个比输入小的文件（正常压缩）
//	mode=bigger → 产出一个比输入大的文件（必须触发 Skipped）
//	mode=fail   → 退出码 1 + stderr（模拟 libvips 报错）
func fakeVips(t *testing.T, dir, mode string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "vips-args.log")
	bin = filepath.Join(dir, "vips")
	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
out=""
for a in "$@"; do
  case "$a" in
    *.*) out="$a" ;;
  esac
done
out="${out%%[*}"
case "` + mode + `" in
  fail) echo "vips: unable to load from file" >&2; exit 1 ;;
  bigger) [ -n "$out" ] && head -c 4096 /dev/zero > "$out" ;;
  *) [ -n "$out" ] && printf 'x' > "$out" ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func TestDetectEngineHonest(t *testing.T) {
	// ① 完全没有 vips：必须明确说"没有"，并给出安装入口（应用市场）。
	//
	// ⚠️ 必须把 PATH 也清干净：DetectEngine 会回落到 exec.LookPath("vips")
	// （那是**真实产品行为** —— 用户可能从别处装了 vips），而开发机上现在真的
	// 装了 vips，不清 PATH 这条断言就变成"看这台机器装没装"，而不是在测代码。
	empty := t.TempDir()
	t.Setenv("PATH", filepath.Join(empty, "bin"))
	eng := DetectEngine(empty)
	if eng.Available() {
		t.Fatalf("沙箱里没有 vips 却被判成可用：%+v", eng)
	}
	if !strings.Contains(eng.Reason, "应用市场") {
		t.Errorf("没装时要告诉用户去哪装，实际 %q", eng.Reason)
	}

	// ② 二进制在、能报版本 → 可用，且版本原样带出来。
	ok := t.TempDir()
	bin := filepath.Join(ok, "bin", "vips")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho vips-8.18.6\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng = DetectEngine(ok)
	if !eng.Available() {
		t.Fatalf("能报版本的 vips 应判为可用：%+v", eng)
	}
	if !strings.Contains(eng.Version, "8.18.6") {
		t.Errorf("版本要原样带出来，实际 %q", eng.Version)
	}

	// ③ 二进制在、但执行失败（动态库缺失等）→ **不可用**，且原因里要有真实报错。
	broken := t.TempDir()
	bbin := filepath.Join(broken, "bin", "vips")
	if err := os.MkdirAll(filepath.Dir(bbin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bbin, []byte("#!/bin/sh\necho 'dyld: Library not loaded' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng = DetectEngine(broken)
	if eng.Available() {
		t.Fatalf("跑不起来的 vips 不能说成可用：%+v", eng)
	}
	if !strings.Contains(eng.Reason, "dyld") {
		t.Errorf("原因要带上真实报错（用户据此判断是重装还是修库），实际 %q", eng.Reason)
	}
}

func TestOptionsNormalize(t *testing.T) {
	if o, err := (Options{}).Normalize(); err != nil || o.Quality != DefaultQuality || o.Format != FormatKeep {
		t.Errorf("空选项应补默认值（质量 %d / keep），实际 %+v err=%v", DefaultQuality, o, err)
	}
	for _, bad := range []Options{{Quality: 101}, {Quality: -5, Format: "bmp"}, {Format: "gif"}, {MaxEdge: 99999}} {
		if _, err := bad.Normalize(); err == nil {
			t.Errorf("非法选项必须报错，实际放过：%+v", bad)
		}
	}
	if o, err := (Options{Quality: 60, Format: FormatWebP, MaxEdge: 1920, StripMetadata: true}).Normalize(); err != nil {
		t.Errorf("合法选项被拒：%v", err)
	} else if o.Quality != 60 {
		t.Errorf("质量不该被改：%+v", o)
	}
}

func TestOutputPathRules(t *testing.T) {
	src := "/tmp/a/photo.jpg"
	if got := OutputPath(src, Options{}); got != "/tmp/a/photo.min.jpg" {
		t.Errorf("默认必须另存为 .min 且不动原文件，实际 %s", got)
	}
	if got := OutputPath(src, Options{Format: FormatWebP}); got != "/tmp/a/photo.min.webp" {
		t.Errorf("换格式时扩展名要跟着换，实际 %s", got)
	}
	if got := OutputPath(src, Options{Overwrite: true}); got != src {
		t.Errorf("覆盖模式输出就是原路径，实际 %s", got)
	}
}

// TestCompressArgsShape 锁住传给 libvips 的参数形状。
//
// 这些参数就是参考文档里 govips 那几个 ExportParams 的命令行写法；
// 写错（例如把 Q= 写成 quality=）vips 会**忽略**它，压缩率悄悄变差而没人发现。
func TestCompressArgsShape(t *testing.T) {
	dir := t.TempDir()
	bin, logPath := fakeVips(t, dir, "ok")
	src := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(src, []byte(strings.Repeat("A", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := Engine{Bin: bin, Version: "vips-8.18.6"}
	res, err := eng.CompressFile(context.Background(), src,
		Options{Quality: 82, Format: FormatWebP, MaxEdge: 1920, StripMetadata: true})
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if !strings.HasSuffix(res.Dst, ".min.webp") {
		t.Errorf("输出应是 .min.webp，实际 %s", res.Dst)
	}
	raw, _ := os.ReadFile(logPath)
	args := strings.TrimSpace(string(raw))
	for _, want := range []string{"thumbnail", src, "1920", "--height", "Q=82", "effort=4", "strip"} {
		if !strings.Contains(args, want) {
			t.Errorf("vips 参数里缺少 %q，实际：%s", want, args)
		}
	}
	// PNG 是无损的：不该出现 Q=，要用 compression=9（写了 Q 会让人以为调质量有用）。
	pngDir := t.TempDir()
	pbin, plog := fakeVips(t, pngDir, "ok")
	psrc := filepath.Join(pngDir, "pic.png")
	if err := os.WriteFile(psrc, []byte(strings.Repeat("A", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	e2 := Engine{Bin: pbin}
	if _, err := e2.CompressFile(context.Background(), psrc, Options{Quality: 70, Format: FormatPNG}); err != nil {
		t.Fatal(err)
	}
	praw, _ := os.ReadFile(plog)
	if strings.Contains(string(praw), "Q=") {
		t.Errorf("PNG 不该带 Q=（无损格式，质量无效）：%s", praw)
	}
	if !strings.Contains(string(praw), "compression=9") {
		t.Errorf("PNG 应当用 compression=9：%s", praw)
	}
}

// TestCompressKeepsOriginalWhenBigger：压完更大 → 保留原文件、如实记 Skipped。
//
// 这是"不许把用户的文件弄大"的底线：JPEG 重压、已经是 WebP 的图、小图标都可能变大。
func TestCompressKeepsOriginalWhenBigger(t *testing.T) {
	dir := t.TempDir()
	bin, _ := fakeVips(t, dir, "bigger")
	src := filepath.Join(dir, "small.jpg")
	if err := os.WriteFile(src, []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := Engine{Bin: bin}
	res, err := eng.CompressFile(context.Background(), src, Options{Quality: 95})
	if err != nil {
		t.Fatalf("压缩不该报错（只是跳过）: %v", err)
	}
	if !res.Skipped {
		t.Errorf("压完更大时必须记 Skipped（否则等于把用户的文件弄大还报成功）：%+v", res)
	}
	if b, _ := os.ReadFile(src); string(b) != "tiny" {
		t.Errorf("原文件必须一字未动，实际 %q", b)
	}
	if _, err := os.Stat(res.Dst); !os.IsNotExist(err) {
		t.Errorf("更大的产物必须被删掉，不能留在磁盘上（err=%v）", err)
	}
	// 覆盖模式下同样不许把文件弄大。
	res2, err := eng.CompressFile(context.Background(), src, Options{Quality: 95, Overwrite: true})
	if err != nil || !res2.Skipped {
		t.Errorf("覆盖模式压完更大也要跳过（err=%v res=%+v）", err, res2)
	}
	if b, _ := os.ReadFile(src); string(b) != "tiny" {
		t.Errorf("覆盖模式下也不许把原文件弄大，实际 %q", b)
	}
}

func TestCompressErrorCarriesVipsOutput(t *testing.T) {
	dir := t.TempDir()
	bin, _ := fakeVips(t, dir, "fail")
	src := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(src, []byte(strings.Repeat("A", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Engine{Bin: bin}.CompressFile(context.Background(), src, Options{})
	if err == nil {
		t.Fatal("vips 失败必须报错")
	}
	if !strings.Contains(err.Error(), "unable to load") {
		t.Errorf("错误里要带上 vips 的真实输出（不然用户不知道哪张图有问题），实际 %v", err)
	}
}

func TestCompressRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	bin, _ := fakeVips(t, dir, "ok")
	src := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := Engine{Bin: bin}
	if _, err := eng.CompressFile(context.Background(), src, Options{}); err == nil {
		t.Error("非图片必须被拒绝（覆盖模式下这等于拿任意文件冒险）")
	}
}

func TestEngineUnavailableReturnsReason(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := Engine{Reason: "没有引擎"}
	_, err := eng.CompressFile(context.Background(), src, Options{})
	if err == nil || !strings.Contains(err.Error(), "没有引擎") {
		t.Errorf("引擎不可用必须如实报原因，实际 %v", err)
	}
}

// TestCompressFileRealVips 是**真机行为**测试：装了就真压一张图，没装就跳过。
//
// 为什么必须有一条真的：假 vips 只能证明"我们传了对的参数"，不能证明
// "libvips 真的认这些参数、真的把图压小了"。参数写错时 vips 往往**不报错**，
// 只是忽略（压缩率悄悄变差）—— 只有真跑一次、比较前后体积才看得见。
//
// 沙箱化：源图与产物都在 t.TempDir()，不碰任何真实图片。
func TestCompressFileRealVips(t *testing.T) {
	eng := DetectEngine("/opt/homebrew")
	if !eng.Available() {
		t.Skip("本机没装 libvips，跳过真实压缩行为校验")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "noise.png")
	// 造一张"压缩空间很大"的图：随机噪点 + 大尺寸（PNG 无损，转 WebP 后应显著变小）。
	writeNoisePNG(t, src, 900, 700)

	before, _ := os.Stat(src)
	res, err := eng.CompressFile(context.Background(), src,
		Options{Quality: 70, Format: FormatWebP, StripMetadata: true})
	if err != nil {
		t.Fatalf("真实 vips 压缩失败：%v", err)
	}
	if res.Skipped {
		t.Fatalf("噪点 PNG 转 WebP 应当变小，却报了跳过（%d → %d）", res.Before, res.After)
	}
	if res.After >= before.Size() {
		t.Fatalf("产物没有变小：%d → %d", before.Size(), res.After)
	}
	raw, err := os.ReadFile(res.Dst)
	if err != nil {
		t.Fatal(err)
	}
	// WebP 容器头：RIFF....WEBP —— 证明产出的确实是 WebP，不是被改名的原图。
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WEBP" {
		t.Errorf("产物不是合法的 WebP（前 12 字节：%q）", raw[:min(12, len(raw))])
	}
	t.Logf("真实 vips：%d → %d 字节（省 %.0f%%）", res.Before, res.After,
		100*float64(res.Before-res.After)/float64(res.Before))
	_ = before
}

// TestThumbnailArgsAlwaysCarryWidth 是"默认值就坏"那次事故的门禁。
//
// 判据贴着 libvips 的真实约束：`vips thumbnail` 至少要 in / out / width
// 三个**位置参数**，少给宽度它会 `thumbnail: too few arguments` 直接退出 1。
// 而界面上「最长边」的默认值就是 0（不缩放）—— 这条判据一旦失守，
// **默认设置下每一张图都压不了**（2026-09-18 用户报障的原样：
// `vips 处理 xxx.heic 失败: exit status 1 / thumbnail: too few arguments`）。
func TestThumbnailArgsAlwaysCarryWidth(t *testing.T) {
	for _, edge := range []int{0, 1, 1280, maxEdgeLimit} {
		o := Options{MaxEdge: edge}
		args := o.thumbnailArgs("/in.jpg", "/out.jpg")
		if args[0] != "thumbnail" {
			t.Fatalf("MaxEdge=%d：操作名应是 thumbnail，得到 %q", edge, args[0])
		}
		// 位置参数：操作名之后，以 "--" 开头的都算选项，其余按位置算。
		pos, width := 0, ""
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "--") {
				continue
			}
			pos++
			if pos == 3 {
				width = a
			}
		}
		if pos < 3 {
			t.Fatalf("MaxEdge=%d：vips thumbnail 至少要 in/out/width 三个位置参数，"+
				"现在只有 %d 个（%v）—— 这正是 `thumbnail: too few arguments` 的成因", edge, pos, args)
		}
		if w, err := strconv.Atoi(width); err != nil || w <= 0 {
			t.Fatalf("MaxEdge=%d：width 必须是正整数，得到 %q（%v）", edge, width, args)
		}
		// `--size down`：没有它，vips thumbnail 会把**小图放大**到目标框
		//（默认 size 语义），而界面与文档都对用户承诺"只缩不放"。
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--size down") {
			t.Fatalf("MaxEdge=%d：命令里必须有 `--size down`（只缩不放），实际 %v。\n"+
				"少了它，900×700 的图配 1280 会被放大成 1280×996（实测），"+
				"MaxEdge=0 时更会放大到超过 libwebp 上限报 `webpsave: image too large`", edge, args)
		}
	}
}

// TestCompressAllOptionCombinationsRealVips：**选项矩阵**逐格真跑。
//
// 为什么必须有这条：形状门禁只能挡住"少参数"，而"某一格组合让真 vips 报错"
// 只有真跑才知道。老实现恰恰败在这里 —— 假 vips 永远说 OK，真 vips 一张都压不了
// （默认档"不缩放"就是坏的那一格）。
//
// 没有 vips 的机器上如实 skip（不是假装通过）。
func TestCompressAllOptionCombinationsRealVips(t *testing.T) {
	eng := DetectEngine("/opt/homebrew")
	if !eng.Available() {
		t.Skip("本机没装 libvips，跳过真实选项矩阵校验")
	}
	for _, edge := range []int{0, 1280} {
		for _, format := range []Format{FormatKeep, FormatWebP, FormatJPEG, FormatPNG} {
			for _, strip := range []bool{false, true} {
				dir := t.TempDir()
				src := filepath.Join(dir, "probe.png")
				writeNoisePNG(t, src, 640, 480)
				res, err := eng.CompressFile(context.Background(), src,
					Options{Quality: 80, Format: format, MaxEdge: edge, StripMetadata: strip})
				if err != nil {
					t.Fatalf("最长边=%d 格式=%s strip=%v：真实 vips 失败：%v", edge, format, strip, err)
				}
				if _, serr := os.Stat(res.Dst); serr != nil && !res.Skipped {
					t.Fatalf("最长边=%d 格式=%s strip=%v：没有产出 %s（skipped=%v）",
						edge, format, strip, res.Dst, res.Skipped)
				} else if serr == nil {
					// **绝不放大**：产物宽高都不得超过源图（640×480）。
					// 这是第二层 bug 的门禁：`--size down` 缺失时 vips 会把小图
					// 放大到目标框（900×700 + 1280 → 1280×996，实测），
					// 而界面/文档对用户承诺的是"比目标小的图保持原尺寸"。
					w, h := vipsImageDims(t, eng.Bin, res.Dst)
					if w > 640 || h > 480 {
						t.Fatalf("最长边=%d 格式=%s strip=%v：产物是 %dx%d，比源图 640x480 还大 —— "+
							"vips thumbnail 少了 `--size down` 就会放大（2026-09-18 实测）",
							edge, format, strip, w, h)
					}
				}
			}
		}
	}
}

// TestCompressHEICViaSipsFallback：iPhone 拍的 HEIC 必须能压。
//
// 这台机器上的 vips（Homebrew 8.18.6）读不了 HEIC —— 它的动态模块搜索路径被
// 编译成了构建机路径（sharp-libvips），实测 `vips -l` 里没有 heifload、直接报
// `is not a known file format`。所以引擎对 HEIC/HEIF 先走系统 sips 转 PNG。
// 这条测试就是用户 2026-09-18 报的那个场景：HEIC + **默认「不缩放」** → 必须成功。
func TestCompressHEICViaSipsFallback(t *testing.T) {
	eng := DetectEngine("/opt/homebrew")
	if !eng.Available() {
		t.Skip("本机没装 libvips，跳过 HEIC 兜底校验")
	}
	if _, err := os.Stat(SipsBin); err != nil {
		t.Skip("本机没有 sips（不是 macOS？），跳过 HEIC 兜底校验")
	}
	dir := t.TempDir()
	png := filepath.Join(dir, "src.png")
	// 用平滑渐变（像真实照片）：随机噪声转 WebP 可能**变大**，
	// 那样测的就是"压完更大要不要保留原文件"，而不是"HEIC 这条路通不通"。
	writeGradientPNG(t, png, 640, 480)
	heic := filepath.Join(dir, "photo.heic")
	if out, err := exec.Command(SipsBin, "-s", "format", "heic", png, "--out", heic).
		CombinedOutput(); err != nil {
		t.Skipf("这台机器的 sips 生成不了 HEIC（%v：%s），跳过", err, strings.TrimSpace(string(out)))
	}
	res, err := eng.CompressFile(context.Background(), heic,
		Options{Quality: 80, Format: FormatWebP, MaxEdge: 0, StripMetadata: true})
	if err != nil {
		t.Fatalf("压 HEIC 失败（正是用户 2026-09-18 报的场景）：%v", err)
	}
	if res.Skipped {
		// 压完更大 → 保留原文件、如实标记：这是**正确**行为（绝不把文件弄大）。
		// 判据是"HEIC 这条路能不能走通"，不是"每一张都必须变小"。
		t.Logf("HEIC 走得通；这一张压完更大（%d → %d），按设计保留原文件", res.Before, res.After)
		return
	}
	if !strings.HasSuffix(res.Dst, ".min.webp") {
		t.Fatalf("产物名应按**原文件名**算（.min.webp），不该变成 .png：%s", res.Dst)
	}
	w, h := vipsImageDims(t, eng.Bin, res.Dst)
	if w > 640 || h > 480 {
		t.Fatalf("产物 %dx%d 比源图 640x480 还大：HEIC 这条路也必须只缩不放", w, h)
	}
}

// TestVipsCapsComeFromRuntime：格式能力必须来自**运行体**，不是编译期配置。
//
// 依据是 2026-09-18 的实测：Homebrew 的 vips 8.18.6 瓶（sharp-libvips 构建）把
// 动态模块路径编译成了构建机路径，`vips --vips-config` 里写着
// "HEIC/AVIF load/save with libheif: true"，而 `vips -l` 里连 heifload 都没有。
// 所以面板判断"能不能压这个格式"只能问 `vips -l`。
func TestVipsCapsComeFromRuntime(t *testing.T) {
	eng := DetectEngine("/opt/homebrew")
	if !eng.Available() {
		t.Skip("本机没装 libvips，跳过格式能力校验")
	}
	if len(eng.Ops) == 0 {
		t.Fatalf("引擎可用却拿不到操作表（probeOps 返回空）：格式能力必须来自运行体")
	}
	// 核心格式是面板对用户的承诺，缺一个都算能力退化。
	for _, op := range []string{"jpegload", "jpegsave", "pngload", "pngsave", "webpload", "webpsave"} {
		if !eng.Ops[op] {
			t.Errorf("核心操作 %s 在这台机器上不可用 —— 面板连 jpg/png/webp 都压不了", op)
		}
	}
	// canSave/canLoad 必须与操作表一致：Ops 说不行就必须提前拦下来，
	// 而不是放行到 vips 那里再抛一句 is not a known file format。
	for ext, op := range saverForExt {
		if got, want := eng.canSave(ext), eng.Ops[op]; got != want {
			t.Errorf("canSave(%s) 与运行体能力不一致：canSave=%v，Ops[%s]=%v", ext, got, op, want)
		}
	}
	for ext, op := range loaderForExt {
		if got, want := eng.canLoad(ext), eng.Ops[op]; got != want {
			t.Errorf("canLoad(%s) 与运行体能力不一致：canLoad=%v，Ops[%s]=%v", ext, got, op, want)
		}
	}
}

// TestUnsupportedOutputGivesActionableError：写不了的格式要给出**能照做的动作**。
//
// 用户场景（2026-09-18）：iPhone 的 HEIC + 默认「保持原格式」→ 这台机器的 vips
// 写不了 HEIC。错误里必须有"不能输出 heic"和替代格式，而不是 vips 的原文。
func TestUnsupportedOutputGivesActionableError(t *testing.T) {
	eng := DetectEngine("/opt/homebrew")
	if !eng.Available() {
		t.Skip("本机没装 libvips，跳过")
	}
	if eng.canSave(".heic") {
		t.Skip("这台机器的 vips 能写 HEIC（模块可用），本条不适用")
	}
	if _, err := os.Stat(SipsBin); err != nil {
		t.Skip("本机没有 sips，造不出 HEIC 素材")
	}
	dir := t.TempDir()
	png := filepath.Join(dir, "src.png")
	writeGradientPNG(t, png, 320, 240)
	heic := filepath.Join(dir, "photo.heic")
	if out, err := exec.Command(SipsBin, "-s", "format", "heic", png, "--out", heic).
		CombinedOutput(); err != nil {
		t.Skipf("sips 生成不了 HEIC（%v：%s）", err, strings.TrimSpace(string(out)))
	}
	_, err := eng.CompressFile(context.Background(), heic, Options{Quality: 80, Format: FormatKeep})
	if err == nil {
		t.Fatal("vips 写不了 HEIC 时，FormatKeep 必须如实报错（不许报成功）")
	}
	for _, want := range []string{"不能输出 heic", "WebP"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应当有 %q（用户据此知道下一步做什么），实际：%v", want, err)
		}
	}
}

// vipsImageDims 用 vipsheader 读产物尺寸（`-f width` / `-f height`）。
//
// 为什么不用 Go 解码：产物可能是 png / jpeg / webp / avif 四种之一，
// 而 vipsheader 就装在被测引擎旁边，且这两条真实测试本来就要求 vips 存在。
func vipsImageDims(t *testing.T, vipsBin, path string) (int, int) {
	t.Helper()
	header := filepath.Join(filepath.Dir(vipsBin), "vipsheader")
	read := func(field string) int {
		out, err := exec.Command(header, "-f", field, path).Output()
		if err != nil {
			t.Fatalf("vipsheader -f %s %s 失败：%v", field, path, err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatalf("vipsheader 输出的 %s 不是整数：%q", field, out)
		}
		return n
	}
	return read("width"), read("height")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// writeGradientPNG 生成一张平滑渐变的 PNG。
//
// 比随机噪声更像真实照片：有损编码（WebP/JPEG/HEIC）都能把它显著压小，
// 因此适合验证"真的变小了"，而不会把测试变成在测 Skipped 分支。
func writeGradientPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(x * 255 / w),
				G: uint8(y * 255 / h),
				B: uint8((x + y) * 255 / (w + h)),
				A: 255,
			})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// writeNoisePNG 生成一张随机噪点的 PNG（压缩空间大，适合验证"真的变小了"）。
func writeNoisePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(42))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(rnd.Intn(256)), G: uint8(rnd.Intn(256)), B: uint8(rnd.Intn(256)), A: 255,
			})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

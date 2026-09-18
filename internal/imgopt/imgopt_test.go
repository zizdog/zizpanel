package imgopt

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
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
	empty := t.TempDir()
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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

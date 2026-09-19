package native

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
)

// ---------- 脚手架 ----------

// shimPath 造一个临时 PATH：目录里放同名假命令，并把该目录排在最前。
func shimPath(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho shim-$0 >&2\nexit 127\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 只留假命令目录 + 系统目录，好让其它命令照常能找到。
	return dir + ":/usr/bin:/bin"
}

func testCtx(t *testing.T) (*Ctx, *execx.Execer) {
	t.Helper()
	ex := execx.New()
	return NewCtx(ex, execx.NewProbeCache(), t.TempDir()), ex
}

func realProbe(t *testing.T, c *Ctx) Probe {
	t.Helper()
	p := c.Probe(context.Background())
	if !p.Available {
		t.Skipf("本机原生桥不可用，跳过真实调用：%s", p.Reason)
	}
	return p
}

// ---------- Probe：真实结论 ----------

func TestProbeReportsBackendOrRealReason(t *testing.T) {
	c, _ := testCtx(t)
	p := c.Probe(context.Background())
	if p.Available {
		if p.Backend != BridgeJXA && p.Backend != BridgePyObjC {
			t.Fatalf("可用时必须给出后端，实际 %q", p.Backend)
		}
		if p.Reason != "" {
			t.Fatalf("可用时不该带原因：%s", p.Reason)
		}
		t.Logf("原生桥可用：backend=%s", p.Backend)
		return
	}
	// 不可用时原因必须是人话，且点出两个后端各自的真实结论。
	for _, want := range []string{"python3", "osascript"} {
		if !strings.Contains(p.Reason, want) {
			t.Errorf("不可用原因应点出 %s 的真实情况，实际 %q", want, p.Reason)
		}
	}
	t.Logf("原生桥不可用（如实上报）：%s", p.Reason)
}

// TestProbeBothBackendsBroken：两个后端都跑不通时，必须分别给出真实原因。
func TestProbeBothBackendsBroken(t *testing.T) {
	t.Setenv("PATH", shimPath(t, "python3", "osascript"))
	c, _ := testCtx(t)
	p, err := RunProbe(context.Background(), c.Exec, nil, c.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available {
		t.Fatal("两个命令都跑不通，不该判定可用")
	}
	for _, want := range []string{"python3 后端不可用", "JXA 后端不可用"} {
		if !strings.Contains(p.Reason, want) {
			t.Errorf("原因应点出 %q，实际 %q", want, p.Reason)
		}
	}
	if p.Backend != "" {
		t.Errorf("不可用时不该有后端，实际 %q", p.Backend)
	}
	// 报错里不该回显完整临时路径之外的私密路径：这里只验证不含家目录。
	if home, _ := os.UserHomeDir(); home != "" && strings.Contains(p.Reason, home) {
		t.Errorf("原因不该回显家目录：%q", p.Reason)
	}
}

// TestProbeMissingCommands：PATH 里连名字都没有时，原因必须是"找不到命令"。
func TestProbeMissingCommands(t *testing.T) {
	dir := t.TempDir() // 空目录，什么都不放
	t.Setenv("PATH", dir)
	c, _ := testCtx(t)
	// execx.LookPath 用进程 PATH，这里临时改回空目录即可。
	p, err := RunProbe(context.Background(), c.Exec, nil, c.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available {
		t.Fatal("空 PATH 下不该判定可用")
	}
	if !strings.Contains(p.Reason, "找不到命令 python3") || !strings.Contains(p.Reason, "找不到命令 osascript") {
		t.Fatalf("应说清两个命令都找不到，实际 %q", p.Reason)
	}
}

// fakePythonNoPyObjC 造一个"存在但没有 PyObjC"的 python3。
func TestProbePythonMissingModules(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "python3")
	body := "#!/bin/sh\nprintf 'MISSING Quartz,Vision,PDFKit\\n' >&2\nexit 2\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	// 连 osascript 也挡掉，才能看到 python3 那条原因。
	shim := filepath.Join(dir, "osascript")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	c, _ := testCtx(t)
	p, err := RunProbe(context.Background(), c.Exec, nil, c.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available {
		t.Fatal("PyObjC 缺失且 JXA 失效时不该判定可用")
	}
	if !strings.Contains(p.Reason, "PyObjC 模块缺失") || !strings.Contains(p.Reason, "Quartz") {
		t.Errorf("原因应点出缺失的 PyObjC 模块，实际 %q", p.Reason)
	}
}

func TestProbeCommandLineToolsHint(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "python3")
	body := "#!/bin/sh\necho 'xcrun: error: invalid active developer path' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "osascript")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	c, _ := testCtx(t)
	p, _ := RunProbe(context.Background(), c.Exec, nil, c.TempDir)
	if p.Available {
		t.Fatal("不该判定可用")
	}
	if !strings.Contains(p.Reason, "命令行开发者工具") {
		t.Errorf("应给出安装命令行开发者工具的提示，实际 %q", p.Reason)
	}
}

func TestProbeIsCached(t *testing.T) {
	// 第一次用假命令拿到"不可用"结论；删掉假命令再探，必须还是缓存里的结论。
	realPath := os.Getenv("PATH")
	dir := t.TempDir()
	for _, n := range []string{"python3", "osascript"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	c, _ := testCtx(t)
	first, _ := RunProbe(context.Background(), c.Exec, c.Probes, c.TempDir)
	if first.Available {
		t.Fatal("假命令不该判定可用")
	}
	t.Setenv("PATH", realPath)
	second, _ := RunProbe(context.Background(), c.Exec, c.Probes, c.TempDir)
	if second.Available != first.Available || second.Reason != first.Reason {
		t.Fatalf("第二次应命中缓存：first=%+v second=%+v", first, second)
	}
	// 新缓存必须真的重跑（证明上面那次是缓存而不是巧合）。
	fresh := NewCtx(c.Exec, execx.NewProbeCache(), c.TempDir)
	if got := fresh.Probe(context.Background()); !got.Available {
		t.Fatalf("真实 PATH 下应探测到可用后端，实际 %+v", got)
	}
}

// ---------- 调用：载荷与失败分支 ----------

func TestEncodePayloadRejectsHuge(t *testing.T) {
	big := strings.Repeat("x", maxPayloadBytes+1)
	if _, err := EncodePayload(map[string]string{"a": big}); err == nil {
		t.Fatal("超大载荷应被拒绝，而不是静默截断")
	}
	s, err := EncodePayload(map[string]string{"a": "中文"})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal([]byte(s), &back); err != nil {
		t.Fatalf("载荷应是合法 JSON: %v", err)
	}
	if back["a"] != "中文" {
		t.Fatalf("中文载荷变了：%q", back["a"])
	}
}

func TestCallUnavailableBridgeReturnsReason(t *testing.T) {
	c, _ := testCtx(t)
	_, err := Call(context.Background(), c, Probe{Available: false, Reason: "缺少命令 osascript"},
		Script{Name: "x", Cmd: "osascript"}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "缺少命令 osascript") {
		t.Fatalf("不可用桥必须返回真实原因，实际 %v", err)
	}
}

func TestCallTimeoutIsReported(t *testing.T) {
	c, _ := testCtx(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-slow")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sc := Script{Name: "假慢脚本", Cmd: fake, Timeout: 300 * time.Millisecond}
	if _, err := Call(context.Background(), c, Probe{Available: true}, sc, map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "未完成") {
		t.Fatalf("超时必须如实报错，实际 %v", err)
	}
}

func TestCallFailureCarriesStderr(t *testing.T) {
	c, _ := testCtx(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-native")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'boom: 内部错误' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sc := Script{Name: "假桥", Cmd: fake, Timeout: 5 * time.Second}
	_, err := Call(context.Background(), c, Probe{Available: true}, sc, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("失败必须带上 stderr 关键行，实际 %v", err)
	}
}

func TestSanitizeMessageHidesHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	if home == "" {
		t.Skip("取不到家目录")
	}
	got := SanitizeMessage("打不开 " + home + "/Documents/a.pdf")
	if strings.Contains(got, home) {
		t.Fatalf("不该回显完整家目录：%q", got)
	}
	if !strings.Contains(got, "~/Documents/a.pdf") {
		t.Fatalf("应缩成 ~：%q", got)
	}
}

// ---------- WarpDocument：纯 Go ----------

func TestWarpDocumentCropsToQuad(t *testing.T) {
	// 造 100x100：中间 20..80 的方块是红色，其余蓝色。
	src := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if x >= 20 && x < 80 && y >= 20 && y < 80 {
				src.Set(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				src.Set(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	// 归一化（原点左下）：y 像素 20 → 0.8，像素 80 → 0.2。
	cs := Corners{
		TopLeft:     [2]float64{0.2, 0.8},
		TopRight:    [2]float64{0.8, 0.8},
		BottomLeft:  [2]float64{0.2, 0.2},
		BottomRight: [2]float64{0.8, 0.2},
	}
	out := WarpDocument(src, cs)
	if out.Bounds().Dx() < 55 || out.Bounds().Dx() > 65 {
		t.Errorf("输出宽度应接近 60，实际 %d", out.Bounds().Dx())
	}
	if out.Bounds().Dy() < 55 || out.Bounds().Dy() > 65 {
		t.Errorf("输出高度应接近 60，实际 %d", out.Bounds().Dy())
	}
	// 四角与中心都应是红色（裁对了）。
	r, g, b, _ := out.At(2, 2).RGBA()
	if r < 0xf000 || g > 0x1000 || b > 0x1000 {
		t.Errorf("左上角应裁到红色区域，实际 r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
	cr, cg, cb, _ := out.At(out.Bounds().Dx()/2, out.Bounds().Dy()/2).RGBA()
	if cr < 0xf000 || cg > 0x1000 || cb > 0x1000 {
		t.Errorf("中心应是红色，实际 r=%d g=%d b=%d", cr>>8, cg>>8, cb>>8)
	}
}

// ---------- 真实原生调用（桥不可用就打印原因并跳过） ----------

func TestRealOCRBarcodeAndDocument(t *testing.T) {
	c, _ := testCtx(t)
	p := realProbe(t, c)
	ctx := context.Background()
	dir := t.TempDir()

	qr := filepath.Join(dir, "qr.png")
	if err := writeQRPNG(ctx, c, p, "MacSaber-Hello-12345", qr); err != nil {
		t.Skipf("造二维码测试素材失败，跳过：%v", err)
	}
	bc, err := c.Barcodes(ctx, p, qr)
	if err != nil {
		t.Fatalf("条码识别失败: %v", err)
	}
	found := false
	for _, it := range bc.Items {
		if it.Payload == "MacSaber-Hello-12345" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应识别出二维码内容，实际 %+v", bc.Items)
	}

	doc, err := c.DetectDocument(ctx, p, qr)
	if err != nil {
		t.Fatalf("文档分割失败: %v", err)
	}
	if !doc.Found || doc.Corners == nil {
		t.Fatalf("纯色方块图应能被当成文档区域，实际 %+v", doc)
	}
	// 四角必须落在归一化范围内。
	for name, c2 := range map[string][2]float64{
		"tl": doc.Corners.TopLeft, "tr": doc.Corners.TopRight,
		"bl": doc.Corners.BottomLeft, "br": doc.Corners.BottomRight,
	} {
		if c2[0] < -0.01 || c2[0] > 1.01 || c2[1] < -0.01 || c2[1] > 1.01 {
			t.Errorf("%s 归一化坐标越界：%v", name, c2)
		}
	}

	// OCR：二维码图没有文字，只要求"不报错且返回结构合法"。
	ocr, err := c.OCRImage(ctx, p, qr, []string{"zh-Hans", "en-US"})
	if err != nil {
		t.Fatalf("OCR 调用失败: %v", err)
	}
	if ocr.Text != "" && len(ocr.Lines) == 0 {
		t.Fatalf("文本非空却没有明细行: %+v", ocr)
	}
	t.Logf("OCR 在无文字图上返回 %d 行（正常）", len(ocr.Lines))

	// 不存在的文件必须报错，不许返回空结果装成功。
	if _, err := c.OCRImage(ctx, p, filepath.Join(dir, "nope.png"), nil); err == nil {
		t.Fatal("读不到的图片必须报错")
	}
}

func TestRealPDFSuite(t *testing.T) {
	c, _ := testCtx(t)
	p := realProbe(t, c)
	ctx := context.Background()
	dir := t.TempDir()

	a := filepath.Join(dir, "a.pdf")
	b := filepath.Join(dir, "b.pdf")
	if err := writeTextPDF(a, "Alpha one"); err != nil {
		t.Fatal(err)
	}
	if err := writeTextPDF(b, "Beta two"); err != nil {
		t.Fatal(err)
	}

	info, err := c.PDFInfo(ctx, p, a)
	if err != nil {
		t.Fatalf("读 PDF 信息失败: %v", err)
	}
	if info.Pages != 1 || info.Encrypted || info.Locked {
		t.Fatalf("信息不符：%+v", info)
	}
	if len(info.PageSizes) != 1 || info.PageSizes[0].Width <= 0 {
		t.Fatalf("应给出页尺寸：%+v", info.PageSizes)
	}

	txt, err := c.PDFText(ctx, p, a, 0)
	if err != nil {
		t.Fatalf("提取文本失败: %v", err)
	}
	if !strings.Contains(txt.Text, "Alpha one") {
		t.Fatalf("提取文本应含 Alpha one，实际 %q", txt.Text)
	}

	merged := filepath.Join(dir, "merged.pdf")
	if err := c.MergePDF(ctx, p, []string{a, b}, merged); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	minfo, err := c.PDFInfo(ctx, p, merged)
	if err != nil {
		t.Fatalf("读合并结果失败: %v", err)
	}
	if minfo.Pages != 2 {
		t.Fatalf("合并后应有 2 页，实际 %d", minfo.Pages)
	}
	mtxt, _ := c.PDFText(ctx, p, merged, 0)
	if !strings.Contains(mtxt.Text, "Alpha one") || !strings.Contains(mtxt.Text, "Beta two") {
		t.Fatalf("合并后文本应含两份内容，实际 %q", mtxt.Text)
	}

	outDir := filepath.Join(dir, "pages")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := c.SplitPDF(ctx, p, merged, outDir)
	if err != nil {
		t.Fatalf("拆分失败: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("应拆出 2 个文件，实际 %v", files)
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || st.Size() == 0 {
			t.Fatalf("拆分产物无效：%s (%v)", f, err)
		}
	}

	enc := filepath.Join(dir, "enc.pdf")
	if err := c.EncryptPDF(ctx, p, a, enc, "open-sesame", ""); err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	ei, err := c.PDFInfo(ctx, p, enc)
	if err != nil {
		t.Fatalf("读加密结果失败: %v", err)
	}
	if !ei.Encrypted {
		t.Fatalf("产物应标记为加密：%+v", ei)
	}
	if len(ei.Notes) == 0 {
		t.Errorf("加密文档读不到页数时应如实说明原因：%+v", ei)
	}

	// 加密产物必须真的打不开（不是"看起来加密"）。
	if _, err := c.PDFText(ctx, p, enc, 0); err == nil {
		t.Fatal("加密 PDF 不该能直接提取文本")
	}

	// 不存在的输入必须报错。
	if _, err := c.PDFInfo(ctx, p, filepath.Join(dir, "nope.pdf")); err == nil {
		t.Fatal("读不到的 PDF 必须报错")
	}
}

// OCR 必须能识别中文：fast 级别会把中文变成乱码，这条测试锁住 accurate（坑 N4）。
func TestRealOCRRecognizesChinese(t *testing.T) {
	c, _ := testCtx(t)
	p := realProbe(t, c)
	ctx := context.Background()
	dir := t.TempDir()
	img := filepath.Join(dir, "cn.png")
	sc := Script{Name: "造中文图", Cmd: "osascript", PreArgs: []string{"-l", "JavaScript"},
		Body: cnImageScript, Timeout: time.Minute}
	if _, err := Call(ctx, c, p, sc, map[string]string{
		"text": "中文识别测试 12345", "output": img}); err != nil {
		t.Skipf("造中文测试图失败，跳过：%v", err)
	}
	res, err := c.OCRImage(ctx, p, img, []string{"zh-Hans", "en-US"})
	if err != nil {
		t.Fatalf("OCR 调用失败: %v", err)
	}
	if !strings.Contains(res.Text, "中文识别测试") {
		t.Fatalf("中文识别结果不对（fast 级别会乱码，必须用 accurate）：%q", res.Text)
	}
	if !strings.Contains(res.Text, "12345") {
		t.Fatalf("数字也该识别出来，实际 %q", res.Text)
	}
}

const cnImageScript = `function run(argv){
  ObjC.import('Foundation'); ObjC.import('AppKit');
  var P = JSON.parse(ObjC.unwrap($.NSString.stringWithContentsOfFileEncodingError($(argv[0]), 4, $())));
  var img = $.NSImage.alloc.initWithSize($.NSMakeSize(900, 200));
  img.lockFocus;
  $.NSColor.whiteColor.setFill;
  $.NSBezierPath.bezierPathWithRect($.NSMakeRect(0,0,900,200)).fill;
  var attrs = $.NSMutableDictionary.dictionary;
  attrs.setObjectForKey($.NSFont.fontWithNameSize('PingFang SC', 56), 'NSFont');
  attrs.setObjectForKey($.NSColor.blackColor, 'NSColor');
  $.NSString.alloc.initWithUTF8String(P.text).drawAtPointWithAttributes($.NSMakePoint(30, 70), attrs);
  img.unlockFocus;
  var rep = $.NSBitmapImageRep.imageRepWithData(img.TIFFRepresentation);
  var png = rep.representationUsingTypeProperties(4, $());
  console.log(JSON.stringify({ok: png.writeToFileAtomically($(P.output), true)}));
}`

func TestCapabilityRealAndShimmed(t *testing.T) {
	c, _ := testCtx(t)
	p := c.Probe(context.Background())
	if !p.Available {
		t.Skipf("原生桥不可用，跳过能力探测：%s", p.Reason)
	}
	ctx := context.Background()

	// VNDetectDocumentSegmentationRequest 是较新的 API：可用就说可用，不可用要给真实原因。
	got := c.Capability(ctx, p, "test.docseg", "Vision", "VNDetectDocumentSegmentationRequest")
	if !got.Available {
		t.Skipf("本机 Vision 不支持文档分割，如实标记不可用：%s", got.Reason)
	}
	// 不存在的类必须如实说不可用（而不是当成可用）。
	miss := c.Capability(ctx, p, "test.nosuch", "Vision", "VNNotARealRequest")
	if miss.Available {
		t.Fatal("不存在的类不该判定可用")
	}
	if !strings.Contains(miss.Reason, "VNNotARealRequest") {
		t.Errorf("原因应点出缺失的类名，实际 %q", miss.Reason)
	}
	// 不存在的框架同理。
	nf := c.Capability(ctx, p, "test.nofw", "NoSuchFramework", "Whatever")
	if nf.Available {
		t.Fatal("不存在的框架不该判定可用")
	}
	if !strings.Contains(nf.Reason, "NoSuchFramework") {
		t.Errorf("原因应点出缺失的框架，实际 %q", nf.Reason)
	}
}

func TestCapabilityUnavailableBridge(t *testing.T) {
	c, _ := testCtx(t)
	got := c.Capability(context.Background(), Probe{Available: false, Reason: "缺少命令 osascript"},
		"test.never", "Vision", "VNRecognizeTextRequest")
	if got.Available || !strings.Contains(got.Reason, "osascript") {
		t.Fatalf("桥不可用时能力探测必须原样给出原因，实际 %+v", got)
	}
}

// 非 ASCII 路径与内容必须原样过桥（osascript 的 argv 会把它们变成乱码，坑 N3）。
func TestRealNonASCIIPathAndContent(t *testing.T) {
	c, _ := testCtx(t)
	p := realProbe(t, c)
	ctx := context.Background()
	dir := t.TempDir()
	// 目录名带中文，文件也用中文名。
	zhDir := filepath.Join(dir, "中文目录")
	if err := os.MkdirAll(zhDir, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(zhDir, "带中文名的图.png")
	writeGoPNGTo(t, src, 8, 8)

	// 载荷里有中文 → 脚本侧必须读得一模一样。
	sc := Script{Name: "回显载荷", Cmd: "osascript", PreArgs: []string{"-l", "JavaScript"},
		Body: echoScript, Timeout: time.Minute}
	out, err := Call(ctx, c, p, sc, map[string]string{"text": "中文识别测试 12345", "path": src})
	if err != nil {
		t.Fatalf("回显调用失败: %v", err)
	}
	if !strings.Contains(out, "中文识别测试 12345") {
		t.Fatalf("中文载荷过桥后变了：%q", out)
	}
	if !strings.Contains(out, zhDir) {
		t.Fatalf("中文路径过桥后变了：%q", out)
	}

	// 真调 Vision：中文路径的图片必须能被打开（读到尺寸即可证明路径没坏）。
	if _, err := c.OCRImage(ctx, p, src, []string{"zh-Hans", "en-US"}); err != nil {
		t.Fatalf("中文路径的图片应能被读取：%v", err)
	}
}

const echoScript = `function run(argv){
  ObjC.import('Foundation');
  var raw = $.NSString.stringWithContentsOfFileEncodingError($(argv[0]), 4, $());
  var P = JSON.parse(ObjC.unwrap(raw));
  console.log(JSON.stringify({text: P.text, path: P.path}));
}`

func writeGoPNGTo(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 7, A: 255})
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

// writeTextPDF 手写一个极小 PDF（带文本内容流），不依赖任何外部工具。
func writeTextPDF(path, text string) error {
	content := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", escapePDF(text))
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var sb strings.Builder
	sb.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objs))
	for i, o := range objs {
		offsets = append(offsets, sb.Len())
		fmt.Fprintf(&sb, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := sb.Len()
	fmt.Fprintf(&sb, "xref\n0 %d\n", len(objs)+1)
	sb.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&sb, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&sb, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

func escapePDF(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)")
	return r.Replace(s)
}

// writeQRPNG 用 CoreImage 造一个真二维码当测试素材（不依赖网络与第三方库）。
func writeQRPNG(ctx context.Context, c *Ctx, p Probe, payload, out string) error {
	sc := Script{Name: "造二维码", Cmd: "osascript", PreArgs: []string{"-l", "JavaScript"}, Body: qrGenScript, Timeout: time.Minute}
	_, err := Call(ctx, c, p, sc, map[string]string{"text": payload, "output": out})
	return err
}

const qrGenScript = `function run(argv){
  ObjC.import('Foundation'); ObjC.import('CoreImage'); ObjC.import('AppKit');
  var p = JSON.parse(ObjC.unwrap($.NSString.stringWithContentsOfFileEncodingError($(argv[0]), 4, $())));
  var data = $(p.text).dataUsingEncoding(4);
  var f = $.CIFilter.filterWithName('CIQRCodeGenerator');
  f.setValueForKey(data, 'inputMessage');
  f.setValueForKey('M', 'inputCorrectionLevel');
  var ctx = $.CIContext.context;
  var cg = ctx.createCGImageFromRect(f.outputImage, f.outputImage.extent);
  var rep = $.NSBitmapImageRep.alloc.initWithCGImage(cg);
  var png = rep.representationUsingTypeProperties(4, $());
  if (!png.writeToFileAtomically($(p.output), true)) { console.log(JSON.stringify({error:'写不出二维码'})); return; }
  console.log(JSON.stringify({ok:true}));
}`

// ---------- 纯 Go 编解码 ----------

func TestEncodePNG(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.png")
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 60), uint8(y * 60), 0, 255})
		}
	}
	if err := EncodePNG(path, img); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := png.Decode(f)
	if err != nil {
		t.Fatalf("产物应是合法 PNG: %v", err)
	}
	if got.Bounds().Dx() != 4 {
		t.Fatalf("尺寸不符：%v", got.Bounds())
	}
}

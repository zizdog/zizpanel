package tools

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/native"
	"github.com/zizdog/macsaber/internal/tool"
)

// ---------- 元数据纪律 ----------

func TestOCRToolsRegisteredAndHonest(t *testing.T) {
	reg := registryForTest(t)
	for _, id := range []string{"ocr.image", "ocr.qrcode", "ocr.deskew"} {
		m := metaOf(t, reg, id)
		if m.Category != "ocr" {
			t.Errorf("%s: 分类应为 ocr，实际 %q", id, m.Category)
		}
		// 分类标题必须在契约里登记，否则列表上会显示英文 id "ocr"。
		if tool.CategoryTitles[m.Category] != "OCR 视觉" {
			t.Errorf("%s: 分类 %q 缺中文标题，实际 %q", id, m.Category, tool.CategoryTitles[m.Category])
		}
		if len([]rune(m.Summary)) > 40 {
			t.Errorf("%s: summary 超过 40 字：%q", id, m.Summary)
		}
		if m.Danger {
			t.Errorf("%s: 本轮不允许危险工具", id)
		}
	}
	if m := metaOf(t, reg, "ocr.image"); !m.Async {
		t.Error("ocr.image 要跑 Vision，应异步")
	}
	if m := metaOf(t, reg, "ocr.deskew"); !m.Async {
		t.Error("ocr.deskew 要解码大图并重采样，应异步")
	}
	if m := metaOf(t, reg, "ocr.qrcode"); m.Async {
		t.Error("ocr.qrcode 是快操作，保持同步")
	}
	// 中文优先必须是默认值。
	def := ""
	for _, p := range metaOf(t, reg, "ocr.image").Params {
		if p.Name == "langs" {
			def, _ = p.Default.(string)
		}
	}
	if !strings.Contains(def, "zh-Hans") {
		t.Fatalf("识别语言默认应中文优先，实际 %q", def)
	}
}

func TestSplitLangs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"zh-Hans,en-US", []string{"zh-Hans", "en-US"}},
		{" zh-Hant , en-US ", []string{"zh-Hant", "en-US"}},
		{"", []string{"zh-Hans", "en-US"}},
		{",,,", []string{"zh-Hans", "en-US"}},
	}
	for _, c := range cases {
		got := splitLangs(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitLangs(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// ---------- 桥不可用 ----------

func TestOCRUnavailableWhenBridgeBroken(t *testing.T) {
	// 用假命令挡掉两个后端：SlowProbe 必须给出真实原因。
	dir := t.TempDir()
	for _, n := range []string{"python3", "osascript"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	b := newBench(t)
	_, err := b.runErr("ocr.qrcode", map[string]any{"input": writeGoPNGFile(t, b.home, "x.png", 8, 8)})
	if err == nil {
		t.Fatal("桥不可用时应拒绝执行")
	}
	if !strings.Contains(err.Error(), "不可用") {
		t.Errorf("原因应说明桥不可用，实际 %q", err.Error())
	}
}

func writeGoPNGFile(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	writeGoPNG(t, p, w, h)
	return p
}

// ---------- 真实 Vision（桥不可用就打印原因跳过） ----------

func TestOCRImageReal(t *testing.T) {
	b := newBench(t)
	requireNativeBridge(t, b)

	img := writeTextImage(t, b.home)
	if img == "" {
		t.Skip("本机无法造出带文字的测试图（缺 qlmanage），跳过真实 OCR")
	}
	_, snap := b.run("ocr.image", map[string]any{"input": img,
		"langs": "zh-Hans,en-US", "output": filepath.Join(b.write, "ocr.txt")})
	data := b.resultData(t, snap)
	text, _ := data["text"].(string)
	if !strings.Contains(strings.ToLower(text), "macsaber") || !strings.Contains(text, "12345") {
		t.Fatalf("应识别出素材文字，实际 %q", text)
	}
	if data["chars"].(int) == 0 {
		t.Fatal("chars 不该为 0")
	}
	txt, _ := data["txt"].(string)
	raw, err := os.ReadFile(txt)
	if err != nil || !strings.Contains(strings.ToLower(string(raw)), "macsaber") {
		t.Fatalf("txt 产物应含识别结果，实际 %v", err)
	}

	// 不存在的图片必须报错，不许返回空结果装成功。
	if code, err := b.runErr("ocr.image", map[string]any{"input": filepath.Join(b.home, "nope.png")}); err == nil {
		t.Fatalf("读不到的图片应被拒（code=%d）", code)
	}
}

func TestOCRQRCodeReal(t *testing.T) {
	b := newBench(t)
	requireNativeBridge(t, b)
	qr := writeQRFixture(t, b)

	res, _ := b.run("ocr.qrcode", map[string]any{"input": qr})
	data := res.Data.(map[string]any)
	items, _ := data["items"].([]native.Barcode)
	found := false
	for _, it := range items {
		if it.Payload == "MacSaber-Wave2-OCR" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应识别出二维码内容，实际 %+v", items)
	}
	if data["count"].(int) != len(items) {
		t.Errorf("count 与实际条数不符：%v vs %d", data["count"], len(items))
	}
}

func TestOCRDeskewReal(t *testing.T) {
	b := newBench(t)
	requireNativeBridge(t, b)
	qr := writeQRFixture(t, b)
	// 把二维码贴在一张大画布上并转个小角度：有明确文档区域可检测。
	skewed := filepath.Join(b.home, "skewed.png")
	writeSkewedDoc(t, skewed, qr)

	out := filepath.Join(b.write, "deskewed.png")
	_, snap := b.run("ocr.deskew", map[string]any{"input": skewed, "output": out})
	data := b.resultData(t, snap)
	got, _ := data["output"].(string)
	if !strings.HasPrefix(got, b.write) {
		t.Fatalf("产物应落在写根内，实际 %s", got)
	}
	st, err := os.Stat(got)
	if err != nil || st.Size() == 0 {
		t.Fatalf("校正产物无效：%v", err)
	}
	if _, ok := data["corners"]; !ok {
		t.Error("应回报检测到的四角")
	}
	// 产物必须比原图小（裁掉了背景）。
	if st.Size() >= pathBytes(t, skewed) {
		t.Errorf("校正后应裁掉背景，产物 %d 字节 >= 原图 %d 字节", st.Size(), pathBytes(t, skewed))
	}
}

// writeQRFixture 用 CoreImage 造二维码素材（不依赖网络与第三方库）。
func writeQRFixture(t *testing.T, b *bench) string {
	t.Helper()
	probe := native.NewCtx(b.ctx.Exec, b.ctx.Probes, b.ctx.TempDir).Probe(context.Background())
	if !probe.Available {
		t.Skipf("原生桥不可用，跳过：%s", probe.Reason)
	}
	out := filepath.Join(b.home, "qr.png")
	sc := native.Script{Name: "造二维码", Cmd: "osascript", PreArgs: []string{"-l", "JavaScript"},
		Body: qrFixtureScript, Timeout: time.Minute}
	if _, err := native.Call(context.Background(), native.NewCtx(b.ctx.Exec, b.ctx.Probes, b.ctx.TempDir),
		probe, sc, map[string]string{"text": "MacSaber-Wave2-OCR", "output": out}); err != nil {
		t.Skipf("造二维码素材失败，跳过：%v", err)
	}
	return out
}

const qrFixtureScript = `function run(argv){
  ObjC.import('Foundation'); ObjC.import('CoreImage'); ObjC.import('AppKit');
  var P = JSON.parse(ObjC.unwrap($.NSString.stringWithContentsOfFileEncodingError($(argv[0]), 4, $())));
  var f = $.CIFilter.filterWithName('CIQRCodeGenerator');
  f.setValueForKey($(P.text).dataUsingEncoding(4), 'inputMessage');
  f.setValueForKey('M', 'inputCorrectionLevel');
  var ctx = $.CIContext.context;
  var cg = ctx.createCGImageFromRect(f.outputImage, f.outputImage.extent);
  var rep = $.NSBitmapImageRep.alloc.initWithCGImage(cg);
  var png = rep.representationUsingTypeProperties(4, $());
  console.log(JSON.stringify({ok: png.writeToFileAtomically($(P.output), true)}));
}`

// writeTextImage 用 qlmanage 渲染一段带文字的 HTML 当 OCR 素材。
func writeTextImage(t *testing.T, dir string) string {
	t.Helper()
	if _, ok := execx.LookPath("qlmanage"); !ok {
		return ""
	}
	html := filepath.Join(dir, "ocr-fixture.html")
	body := `<html><body style="font:64px -apple-system;background:#fff">` +
		`<h1>MacSaber OCR 12345</h1><p>Hello Vision Framework</p></body></html>`
	if err := os.WriteFile(html, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := execx.New().Run(context.Background(), time.Minute, "qlmanage", "-t", "-s", "1000", "-o", dir, html)
	if res.ExitCode != 0 {
		t.Logf("qlmanage 渲染失败：%s", res.Stderr)
		return ""
	}
	produced := filepath.Join(dir, "ocr-fixture.html.png")
	if st, err := os.Stat(produced); err != nil || st.Size() == 0 {
		return ""
	}
	return produced
}

// writeSkewedDoc 把二维码贴到大画布上并旋转一个小角度。
func writeSkewedDoc(t *testing.T, out, qrPath string) {
	t.Helper()
	f, err := os.Open(qrPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	qr, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	const size = 420
	bg := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			bg.Set(x, y, color.RGBA{40, 90, 200, 255})
		}
	}
	// 旋转 -8°，贴到画布中央。
	angle := -8 * math.Pi / 180
	qb := qr.Bounds()
	cx, cy := float64(qb.Dx())/2, float64(qb.Dy())/2
	off := float64(size)/2 - 170
	for y := 0; y < 340; y++ {
		for x := 0; x < 340; x++ {
			sx := float64(x) - 170 + cx
			sy := float64(y) - 170 + cy
			rx := (sx-cx)*math.Cos(angle) - (sy-cy)*math.Sin(angle) + cx
			ry := (sx-cx)*math.Sin(angle) + (sy-cy)*math.Cos(angle) + cy
			ix, iy := int(rx), int(ry)
			if ix < qb.Min.X || iy < qb.Min.Y || ix >= qb.Max.X || iy >= qb.Max.Y {
				continue
			}
			px, py := int(float64(x)+off), int(float64(y)+off)
			if px < 0 || py < 0 || px >= size || py >= size {
				continue
			}
			bg.Set(px, py, qr.At(ix, iy))
		}
	}
	dst := image.NewRGBA(bg.Bounds())
	draw.Draw(dst, dst.Bounds(), bg, image.Point{}, draw.Src)
	w, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := png.Encode(w, dst); err != nil {
		t.Fatal(err)
	}
}

func pathBytes(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

package tools

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/macsaber/internal/native"
	"github.com/zizdog/macsaber/internal/tool"
)

// OCR 语言只用 Vision 支持的短标签，中文优先。
var ocrLangs = []tool.Option{
	{Value: "zh-Hans,en-US", Label: "中文简体 + 英文（推荐）"},
	{Value: "zh-Hant,en-US", Label: "中文繁体 + 英文"},
	{Value: "en-US", Label: "仅英文"},
	{Value: "ja-JP,en-US", Label: "日文 + 英文"},
	{Value: "ko-KR,en-US", Label: "韩文 + 英文"},
}

func splitLangs(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = []string{"zh-Hans", "en-US"}
	}
	return out
}

// ---------- ocr.image ----------

type ocrImage struct{}

func init() { Add(ocrImage{}) }

func (ocrImage) Meta() tool.Meta {
	ok, reason := expensiveCapability()
	return tool.Meta{
		ID: "ocr.image", Name: "图片文字识别", Category: "ocr", Icon: "scan",
		Summary: "用 Vision 识别图片文字，中文优先。",
		Async:   true, TimeoutSeconds: 300, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入图片", Type: tool.TypePath, Required: true,
				Probe:       "native.frameworks",
				Placeholder: "/Users/你/Pictures/scan.png", Help: "只能读允许的读根内的文件。"},
			{Name: "langs", Label: "识别语言", Type: tool.TypeSelect, Default: "zh-Hans,en-US",
				Options: ocrLangs, Help: "语言越多越慢；中文优先用默认值。"},
			{Name: "output", Label: "另存为 txt（可选）", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空就只在页面上显示", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (ocrImage) Probe(ctx context.Context, c *tool.Ctx) (bool, string) { return pdfProbe(ctx, c) }

func (ocrImage) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	langs := splitLangs(in.Str("langs"))
	probe, nctx := nativeBridge(ctx, c)
	if !probe.Available {
		return nil, errors.New("原生 Vision 桥不可用：" + bridgeReason(probe))
	}
	c.Logf("识别：%s（%s）", filepath.Base(src), strings.Join(langs, ","))
	c.Progress(20, "调用 Vision")
	res, err := nctx.OCRImage(ctx, probe, src, langs)
	if err != nil {
		return nil, err
	}
	data := map[string]any{
		"file": src, "langs": langs, "lines": res.Lines, "chars": len([]rune(res.Text)), "text": res.Text,
	}
	msg := fmt.Sprintf("识别到 %d 行、%d 字符", len(res.Lines), len([]rune(res.Text)))
	if len(res.Lines) == 0 {
		// 空结果不是失败，但要说清是"没识别到文字"，别让人以为是坏了。
		msg = "没有识别到文字（图片可能没有文字，或对比度太低）"
		data["note"] = "Vision 返回 0 行，未做任何猜测"
	}
	var files []tool.File
	if out := in.Path("output"); out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return nil, fmt.Errorf("建输出目录失败：%v", err)
		}
		if err := os.WriteFile(out, []byte(res.Text), 0o600); err != nil {
			return nil, fmt.Errorf("写 txt 失败：%v", err)
		}
		st, _ := os.Stat(out)
		files = append(files, tool.File{Name: filepath.Base(out), Path: out, Size: st.Size()})
		data["txt"] = out
		msg += "，已存 " + filepath.Base(out)
	}
	c.Progress(100, "完成")
	return &tool.Result{OK: true, Msg: msg, Data: data, Files: files}, nil
}

// ---------- ocr.qrcode ----------

type ocrQRCode struct{}

func init() { Add(ocrQRCode{}) }

func (ocrQRCode) Meta() tool.Meta {
	ok, reason := expensiveCapability()
	return tool.Meta{
		ID: "ocr.qrcode", Name: "二维码与条码", Category: "ocr", Icon: "qr",
		Summary: "识别图片里的二维码与条码内容。",
		Async:   false, TimeoutSeconds: 90, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入图片", Type: tool.TypePath, Required: true,
				Probe:       "native.frameworks",
				Placeholder: "/Users/你/Pictures/qr.png", Help: "只能读允许的读根内的文件。"},
		},
	}
}

func (ocrQRCode) Probe(ctx context.Context, c *tool.Ctx) (bool, string) { return pdfProbe(ctx, c) }

func (ocrQRCode) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	probe, nctx := nativeBridge(ctx, c)
	if !probe.Available {
		return nil, errors.New("原生 Vision 桥不可用：" + bridgeReason(probe))
	}
	c.Progress(30, "调用 Vision")
	res, err := nctx.Barcodes(ctx, probe, src)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"file": src, "items": res.Items, "count": len(res.Items)}
	msg := fmt.Sprintf("识别到 %d 个条码/二维码", len(res.Items))
	if len(res.Items) == 0 {
		msg = "没有识别到二维码或条码"
	}
	c.Progress(100, "完成")
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// ---------- ocr.deskew ----------

type ocrDeskew struct{}

func init() { Add(ocrDeskew{}) }

func (ocrDeskew) Meta() tool.Meta {
	ok, reason := expensiveCapability()
	return tool.Meta{
		ID: "ocr.deskew", Name: "文档扫描校正", Category: "ocr", Icon: "crop",
		Summary: "自动裁出文档四角并拉直，输出校正后的图。",
		Async:   true, TimeoutSeconds: 300, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入图片", Type: tool.TypePath, Required: true,
				Probe:       "native.frameworks",
				Placeholder: "/Users/你/Pictures/receipt.jpg", Help: "只能读允许的读根内的文件。"},
			{Name: "output", Label: "输出图片", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

// Probe：Vision 文档分割在旧系统上不存在，要真探测一次（结果进缓存）。
func (ocrDeskew) Probe(ctx context.Context, c *tool.Ctx) (bool, string) {
	nctx := native.NewCtx(c.Exec, c.Probes, c.TempDir)
	p := nctx.Probe(ctx)
	if !p.Available {
		return false, p.Reason
	}
	cap := nctx.Capability(ctx, p, "native.cap.documentsegmentation", "Vision", "VNDetectDocumentSegmentationRequest")
	if !cap.Available {
		return false, cap.Reason
	}
	return true, ""
}

func (ocrDeskew) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	dst, err := resolveOutput(c, in.Path("output"), src, ".png")
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, errors.New("输出文件与输入文件相同，请换一个输出路径")
	}
	probe, nctx := nativeBridge(ctx, c)
	if !probe.Available {
		return nil, errors.New("原生 Vision 桥不可用：" + bridgeReason(probe))
	}
	c.Progress(15, "检测文档四角")
	det, err := nctx.DetectDocument(ctx, probe, src)
	if err != nil {
		return nil, err
	}
	if !det.Found || det.Corners == nil {
		return nil, errors.New("这张图里没有检测到文档区域，没有可校正的内容")
	}
	img, err := decodeImage(src)
	if err != nil {
		return nil, err
	}
	c.Progress(60, "拉直并写出")
	warped := native.WarpDocument(img, *det.Corners)
	if err := native.EncodePNG(dst, warped); err != nil {
		return nil, fmt.Errorf("写校正结果失败：%v", err)
	}
	if err := checkImageOutput(dst); err != nil {
		return nil, err
	}
	st, _ := os.Stat(dst)
	srcSt, _ := os.Stat(src)
	data := map[string]any{
		"output": dst, "size": st.Size(), "source_size": srcSt.Size(),
		"source_pixels": fmt.Sprintf("%dx%d", img.Bounds().Dx(), img.Bounds().Dy()),
		"output_pixels": fmt.Sprintf("%dx%d", warped.Bounds().Dx(), warped.Bounds().Dy()),
		"corners":       det.Corners,
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已校正并写入 %s（%s）", filepath.Base(dst), humanSize(st.Size())),
		Data: data, Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// decodeImage 解 PNG/JPEG/GIF；失败时给真实原因。
func decodeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打不开图片：%v", err)
	}
	defer func() { _ = f.Close() }()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("不是能解码的图片：%v", err)
	}
	if img.Bounds().Dx() == 0 || img.Bounds().Dy() == 0 {
		return nil, errors.New("图片尺寸为 0")
	}
	return img, nil
}

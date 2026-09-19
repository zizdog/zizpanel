package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/native"
	"github.com/zizdog/macsaber/internal/tool"
)

// pdfBridgeReason 是首屏（便宜判据）结论：只说明为什么"连桥都没有"。
const pdfBridgeReason = "需要 osascript 或 python3 才能调用 PDFKit"

// nativeBridge 跑昂贵探测并返回桥上下文（结果进 ProbeCache，连点不重跑）。
func nativeBridge(ctx context.Context, c *tool.Ctx) (native.Probe, *native.Ctx) {
	nctx := native.NewCtx(c.Exec, c.Probes, c.TempDir)
	return nctx.Probe(ctx), nctx
}

// bridgeReason 把探测结论压成一句人话（不回显家目录）。
func bridgeReason(p native.Probe) string {
	if p.Available {
		return ""
	}
	return p.Reason
}

// ---------- pdf.merge ----------

type pdfMerge struct{}

func init() { Add(pdfMerge{}) }

func (pdfMerge) Meta() tool.Meta {
	ok, reason := expensiveCapability()
	return tool.Meta{
		ID: "pdf.merge", Name: "PDF 合并", Category: "pdf", Icon: "merge",
		Summary: "把多个 PDF 按顺序合并成一个新文件。",
		Async:   true, TimeoutSeconds: 600, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "inputs", Label: "输入 PDF（一行一个）", Type: tool.TypeTextarea, Required: true, Multiline: true,
				Probe:       "native.frameworks",
				Placeholder: "/Users/你/Documents/a.pdf\n/Users/你/Documents/b.pdf",
				Help:        "每行一个能读到的 PDF，顺序即合并顺序。"},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (pdfMerge) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	inputs, err := readPathLines(c, in.Str("inputs"), fsroot.Read, 0)
	if err != nil {
		return nil, err
	}
	if len(inputs) < 2 {
		return nil, errors.New("至少要有两个 PDF 才能合并")
	}
	dst, err := resolveOutput(c, in.Path("output"), inputs[0], ".pdf")
	if err != nil {
		return nil, err
	}
	for _, src := range inputs {
		if sameFile(src, dst) {
			return nil, errors.New("输出文件与某个输入文件相同，请换一个输出路径")
		}
	}
	probe, nctx := nativeBridge(ctx, c)
	if !probe.Available {
		return nil, errors.New("原生 PDF 桥不可用：" + bridgeReason(probe))
	}
	c.Logf("合并 %d 个 PDF → %s", len(inputs), filepath.Base(dst))
	c.Progress(10, "调用 PDFKit")
	if err := nctx.MergePDF(ctx, probe, inputs, dst); err != nil {
		return nil, err
	}
	if err := checkImageOutput(dst); err != nil {
		return nil, err
	}
	st, _ := os.Stat(dst)
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已合并 %d 个 PDF → %s", len(inputs), filepath.Base(dst)),
		Data:  map[string]any{"output": dst, "inputs": inputs, "size": st.Size()},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// ---------- pdf.split ----------

type pdfSplit struct{}

func init() { Add(pdfSplit{}) }

func (pdfSplit) Meta() tool.Meta {
	ok, reason := expensiveCapability()
	return tool.Meta{
		ID: "pdf.split", Name: "PDF 拆分", Category: "pdf", Icon: "split",
		Summary: "把每一页导出成单独的 PDF，写进新目录。",
		Async:   true, TimeoutSeconds: 600, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入 PDF", Type: tool.TypePath, Required: true,
				Probe:       "native.frameworks",
				Placeholder: "/Users/你/Documents/a.pdf", Help: "只能读允许的读根内的文件。"},
			{Name: "output_dir", Label: "输出目录", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根下的新目录", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (pdfSplit) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	dir, err := splitTarget(c, in.Path("output_dir"), src)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("建输出目录失败：%v", err)
	}
	probe, nctx := nativeBridge(ctx, c)
	if !probe.Available {
		return nil, errors.New("原生 PDF 桥不可用：" + bridgeReason(probe))
	}
	c.Logf("拆分 %s → %s", filepath.Base(src), filepath.Base(dir))
	c.Progress(10, "调用 PDFKit")
	files, err := nctx.SplitPDF(ctx, probe, src, dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("拆分没有产出任何文件")
	}
	out := make([]tool.File, 0, len(files))
	var total int64
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || st.Size() == 0 {
			return nil, fmt.Errorf("产物无效：%s", filepath.Base(f))
		}
		total += st.Size()
		out = append(out, tool.File{Name: filepath.Base(f), Path: f, Size: st.Size()})
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已拆成 %d 个 PDF，写入 %s", len(out), filepath.Base(dir)),
		Data:  map[string]any{"output_dir": dir, "pages": len(out), "total_size": total},
		Files: out,
	}, nil
}

// splitTarget 决定拆分输出目录：留空则在写根下建 <名字>-pages。
func splitTarget(c *tool.Ctx, given, src string) (string, error) {
	root := c.Guard.WriteRoots()[0]
	if given == "" {
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		return uniquePath(filepath.Join(root, base+"-pages")), nil
	}
	if st, err := os.Stat(given); err == nil {
		if !st.IsDir() {
			return "", errors.New("输出目录指向的是一个文件")
		}
		return given, nil
	}
	// 还不存在：确认它落在写根内再创建（闸门已校验父路径）。
	if !pathInside(given, root) {
		return "", errors.New("输出目录不在可写根内")
	}
	return given, nil
}

// ---------- pdf.text ----------

type pdfText struct{}

func init() { Add(pdfText{}) }

func (pdfText) Meta() tool.Meta {
	ok, reason := expensiveCapability()
	return tool.Meta{
		ID: "pdf.text", Name: "提取 PDF 文本", Category: "pdf", Icon: "text",
		Summary: "读出 PDF 的文字层，可同时存成 txt。",
		Async:   true, TimeoutSeconds: 600, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入 PDF", Type: tool.TypePath, Required: true,
				Probe:       "native.frameworks",
				Placeholder: "/Users/你/Documents/a.pdf", Help: "只能读允许的读根内的文件。"},
			{Name: "max_chars", Label: "返回字符上限", Type: tool.TypeNumber, Default: 20000,
				Min: Num(100), Max: Num(2000000), Help: "只影响页面上的预览与结果。"},
			{Name: "output", Label: "另存为 txt（可选）", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空就只在页面上显示", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (pdfText) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	limit := int(in.Num("max_chars", 20000))
	if limit <= 0 {
		limit = 20000
	}
	probe, nctx := nativeBridge(ctx, c)
	if !probe.Available {
		return nil, errors.New("原生 PDF 桥不可用：" + bridgeReason(probe))
	}
	c.Progress(10, "调用 PDFKit")
	res, err := nctx.PDFText(ctx, probe, src, 0)
	if err != nil {
		return nil, err
	}
	full := res.Text
	preview := truncateRunes(full, limit)
	data := map[string]any{
		"file": src, "chars": len([]rune(full)), "pages": len(res.Pages),
		"text": preview, "truncated": len([]rune(full)) > limit,
	}
	if res.Note != "" {
		data["note"] = res.Note
	}
	msg := fmt.Sprintf("已提取 %d 页、%d 字符", len(res.Pages), len([]rune(full)))
	var files []tool.File
	if out := in.Path("output"); out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return nil, fmt.Errorf("建输出目录失败：%v", err)
		}
		if err := os.WriteFile(out, []byte(full), 0o600); err != nil {
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

// ---------- pdf.info ----------

type pdfInfoTool struct{}

func init() { Add(pdfInfoTool{}) }

func (pdfInfoTool) Meta() tool.Meta {
	ok, reason := expensiveCapability()
	return tool.Meta{
		ID: "pdf.info", Name: "PDF 信息", Category: "pdf", Icon: "info",
		Summary: "读页数、每页尺寸、是否加密与标题作者。",
		Async:   false, TimeoutSeconds: 90, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入 PDF", Type: tool.TypePath, Required: true,
				Probe:       "native.frameworks",
				Placeholder: "/Users/你/Documents/a.pdf", Help: "只能读允许的读根内的文件。"},
		},
	}
}

func (pdfInfoTool) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	probe, nctx := nativeBridge(ctx, c)
	if !probe.Available {
		return nil, errors.New("原生 PDF 桥不可用：" + bridgeReason(probe))
	}
	res, err := nctx.PDFInfo(ctx, probe, src)
	if err != nil {
		return nil, err
	}
	st, _ := os.Stat(src)
	data := map[string]any{
		"file": src, "size": st.Size(), "pages": res.Pages,
		"encrypted": res.Encrypted, "locked": res.Locked,
		"title": res.Title, "author": res.Author, "subject": res.Subject,
		"creator": res.Creator, "producer": res.Producer,
		"created": res.Created, "modified": res.Modified,
	}
	if n := len(res.PageSizes); n > 0 {
		first := res.PageSizes[0]
		data["page_size"] = fmt.Sprintf("%.0fx%.0f 点", first.Width, first.Height)
		data["page_sizes"] = res.PageSizes
		if n > 1 {
			sizes := map[string]int{}
			for _, s := range res.PageSizes {
				sizes[fmt.Sprintf("%.0fx%.0f", s.Width, s.Height)]++
			}
			data["page_size_kinds"] = sizes
		}
	}
	if len(res.Notes) > 0 {
		data["notes"] = res.Notes
	}
	msg := fmt.Sprintf("共 %d 页", res.Pages)
	switch {
	case res.Locked:
		msg = "PDF 有打开口令，读不到页数与元数据"
	case res.Encrypted:
		msg = fmt.Sprintf("共 %d 页，文档已加密", res.Pages)
	case res.Pages == 0:
		msg = "读到 0 页，可能不是有效的 PDF"
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// ---------- pdf.encrypt（危险打样：只生成新文件，不动源文件） ----------

type pdfEncrypt struct{}

func init() { Add(pdfEncrypt{}) }

// DangerFloor 是服务端强校验的确认值。
func (pdfEncrypt) DangerFloor() string { return ConfirmText }

// ConfirmOK 要求用户手输输出文件名，避免"顺手点一下"就生成加密副本。
func (pdfEncrypt) ConfirmOK(in tool.Input) error {
	name := filepath.Base(strings.TrimSpace(in.Path("output")))
	if name == "" || name == "." {
		return errors.New("请先填输出文件名")
	}
	if strings.TrimSpace(in.Str("confirm_name")) != name {
		return fmt.Errorf("请在确认框里手输输出文件名 %s", name)
	}
	return nil
}

func (pdfEncrypt) Meta() tool.Meta {
	ok, reason := expensiveCapability()
	return tool.Meta{
		ID: "pdf.encrypt", Name: "PDF 加打开口令", Category: "pdf", Icon: "lock",
		Summary: "用打开口令另存一份 PDF，源文件不动。",
		Async:   true, TimeoutSeconds: 600, Available: ok, UnavailableReason: reason,
		Danger: true, DangerFloor: ConfirmText,
		DangerNote: "只在可写根里生成新文件；口令请自己记牢，丢了打不开。",
		Params: []tool.Param{
			{Name: "input", Label: "输入 PDF", Type: tool.TypePath, Required: true,
				Probe:       "native.frameworks",
				Placeholder: "/Users/你/Documents/a.pdf", Help: "只能读允许的读根内的文件。"},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, Required: true, AllowMissing: true,
				Placeholder: "/Users/你/MacSaberFiles/a-locked.pdf", Help: "只能写到允许的可写根内。"},
			{Name: "password", Label: "打开口令", Type: tool.TypePassword, Required: true, Secret: true,
				Placeholder: "至少 4 位", Help: "打开文档时要输入的密码，不写进审计日志。"},
			{Name: "confirm_name", Label: "手输输出文件名", Type: tool.TypeText, Required: true,
				Placeholder: "a-locked.pdf", Help: "原样填上面的文件名，防止误点。"},
		},
	}
}

func (pdfEncrypt) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	dst := in.Path("output")
	password := in.Str("password")
	if len([]rune(password)) < 4 {
		return nil, errors.New("打开口令至少 4 位")
	}
	if filepath.Ext(dst) == "" {
		dst += ".pdf"
		if _, err := os.Stat(dst); err == nil {
			return nil, errors.New("同名文件已存在，请换一个输出文件名")
		}
	}
	if sameFile(src, dst) {
		return nil, errors.New("不能把加密结果写回源文件，请换一个输出路径")
	}
	probe, nctx := nativeBridge(ctx, c)
	if !probe.Available {
		return nil, errors.New("原生 PDF 桥不可用：" + bridgeReason(probe))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, fmt.Errorf("建输出目录失败：%v", err)
	}
	c.Logf("生成加密副本 → %s", filepath.Base(dst))
	c.Progress(10, "调用 PDFKit")
	if err := nctx.EncryptPDF(ctx, probe, src, dst, password, ""); err != nil {
		return nil, err
	}
	if err := checkImageOutput(dst); err != nil {
		return nil, err
	}
	// 复核：产物必须真的打不开，否则不许报成功（坑 F2）。
	info, err := nctx.PDFInfo(ctx, probe, dst)
	if err != nil {
		return nil, fmt.Errorf("复核加密结果失败（产物已写出，请自行确认）：%v", err)
	}
	if !info.Encrypted {
		_ = os.Remove(dst)
		return nil, errors.New("产物没有真的加密，已删除，未报成功")
	}
	srcSt, _ := os.Stat(src)
	st, _ := os.Stat(dst)
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已生成加密副本 %s（源文件未改动）", filepath.Base(dst)),
		Data: map[string]any{"output": dst, "size": st.Size(), "source_size": srcSt.Size(),
			"encrypted": true, "source_untouched": true},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// ---------- 共用小工具 ----------

// expensiveCapability 是首屏判据：桥命令在不在（真探测留给 SlowProbe）。
func expensiveCapability() (bool, string) {
	_, hasJXA := execx.LookPath("osascript")
	_, hasPy := execx.LookPath("python3")
	if hasJXA || hasPy {
		return true, ""
	}
	return false, "系统缺少命令：osascript、python3"
}

// SlowProbe 用真探测替换首屏的"命令存在"结论。
func (pdfMerge) Probe(ctx context.Context, c *tool.Ctx) (bool, string)    { return pdfProbe(ctx, c) }
func (pdfSplit) Probe(ctx context.Context, c *tool.Ctx) (bool, string)    { return pdfProbe(ctx, c) }
func (pdfText) Probe(ctx context.Context, c *tool.Ctx) (bool, string)     { return pdfProbe(ctx, c) }
func (pdfInfoTool) Probe(ctx context.Context, c *tool.Ctx) (bool, string) { return pdfProbe(ctx, c) }
func (pdfEncrypt) Probe(ctx context.Context, c *tool.Ctx) (bool, string)  { return pdfProbe(ctx, c) }

func pdfProbe(ctx context.Context, c *tool.Ctx) (bool, string) {
	p := native.NewCtx(c.Exec, c.Probes, c.TempDir).Probe(ctx)
	if p.Available {
		return true, ""
	}
	return false, p.Reason
}

// pathInside 判断 p 是否在 root 之下（两侧都已由闸门解析过）。
func pathInside(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// truncateRunes 按字符（不是字节）截断，避免把中文截成半个字。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…（已截断）"
}

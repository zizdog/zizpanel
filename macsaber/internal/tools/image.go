package tools

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tool"
)

// ---------- img.info（只读同步） ----------

type imgInfo struct{}

func init() { Add(imgInfo{}) }

func (imgInfo) Meta() tool.Meta {
	ok, reason := needCmds("sips", "mdls")
	return tool.Meta{
		ID: "img.info", Name: "图片信息", Category: "img", Icon: "image",
		Summary:   "读尺寸、格式、色彩空间、DPI 与 EXIF 摘要。",
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入图片", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Pictures/a.jpg", Help: "只能读允许的读根内的文件。"},
		},
	}
}

func (imgInfo) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	res := c.Exec.Run(ctx, 20*time.Second, "sips", "-g", "all", src)
	if res.TimedOut {
		return nil, errors.New("sips 读取超时被终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("sips 读取失败（退出码 %d）：%s", res.ExitCode, failureReason(res))
	}
	info := parseSipsInfo(res.Stdout)
	if len(info) == 0 {
		return nil, errors.New("sips 没有返回任何图片信息，可能不是图片文件")
	}

	problems := []string{}
	md := map[string]any{}
	// mdls 不在首屏判据里：Spotlight 未索引时它会失败，但那不影响图片信息本身。
	mdRes := c.Exec.Run(ctx, 20*time.Second, "mdls", src)
	if mdRes.TimedOut {
		problems = append(problems, "mdls 读取超时被终止")
	} else if mdRes.ExitCode != 0 {
		problems = append(problems, "mdls 失败（"+firstLine(mdRes.Output())+"）")
	} else {
		md = parseMdlsValues(mdRes.Stdout)
	}

	data := map[string]any{"file": src, "info": info}
	if w, h, ok := sipsPixels(info); ok {
		data["width"] = w
		data["height"] = h
		data["pixels"] = w * h
	}
	if v, ok := info["dpiWidth"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			data["dpi"] = f
		}
	}
	if len(md) > 0 {
		data["metadata"] = md
	}
	for _, k := range []string{"exif:GPSLatitude", "exif:GPSLongitude", "exif:DateTimeOriginal"} {
		if v, ok := md[k]; ok {
			if _, has := data["exif"]; !has {
				data["exif"] = map[string]any{}
			}
			data["exif"].(map[string]any)[strings.TrimPrefix(k, "exif:")] = v
		}
	}
	if len(problems) > 0 {
		data["unavailable"] = problems
	}

	st, _ := os.Stat(src)
	msg := fmt.Sprintf("已读出图片信息（%s）", humanSize(st.Size()))
	if len(problems) > 0 {
		msg = fmt.Sprintf("已读出基本信息，%d 项元数据不可用", len(problems))
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// parseSipsInfo 解析 `sips -g all` 的 "key: value" 行。
func parseSipsInfo(out string) map[string]string {
	info := map[string]string{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		i := strings.Index(ln, ":")
		if i <= 0 {
			continue
		}
		k, v := strings.TrimSpace(ln[:i]), strings.TrimSpace(ln[i+1:])
		if k == "" || v == "" {
			continue
		}
		info[k] = v
	}
	return info
}

func sipsPixels(info map[string]string) (int, int, bool) {
	w, err1 := strconv.Atoi(info["pixelWidth"])
	h, err2 := strconv.Atoi(info["pixelHeight"])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// parseMdls 解析 `mdls` 的 "key = value" 行（值可能是 "文本"、数字或 (…) 列表）。
func parseMdlsValues(out string) map[string]any {
	md := map[string]any{}
	for _, ln := range strings.Split(out, "\n") {
		i := strings.Index(ln, "=")
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(ln[:i])
		v := strings.TrimSpace(ln[i+1:])
		if k == "" || v == "" {
			continue
		}
		switch {
		case v == "(null)" || strings.HasPrefix(v, "("):
			continue
		case strings.HasPrefix(v, "\""):
			md[k] = strings.Trim(v, "\"")
		default:
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				md[k] = n
				continue
			}
			md[k] = v
		}
	}
	return md
}

// ---------- img.resize（异步） ----------

type imgResize struct{}

func init() { Add(imgResize{}) }

func (imgResize) Meta() tool.Meta {
	ok, reason := needCmds("sips")
	return tool.Meta{
		ID: "img.resize", Name: "图片缩放", Category: "img", Icon: "resize",
		Summary: "按最长边或指定宽高等比缩放，输出新文件。",
		Async:   true, TimeoutSeconds: 300, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入图片", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Pictures/a.jpg", Help: "只能读允许的读根内的文件。"},
			{Name: "max_edge", Label: "最长边像素", Type: tool.TypeNumber, Min: Num(1), Max: Num(20000),
				Placeholder: "如 1600", Help: "填了就按最长边等比缩放，留空则用下面的宽高。"},
			{Name: "width", Label: "宽（像素）", Type: tool.TypeNumber, Min: Num(1), Max: Num(20000),
				Placeholder: "如 800", Help: "与高一起用；填了最长边时忽略。"},
			{Name: "height", Label: "高（像素）", Type: tool.TypeNumber, Min: Num(1), Max: Num(20000),
				Placeholder: "如 600", Help: "与宽一起用；填了最长边时忽略。"},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (imgResize) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	edge := int(in.Num("max_edge", 0))
	w, h := int(in.Num("width", 0)), int(in.Num("height", 0))
	if edge <= 0 && (w <= 0 || h <= 0) {
		return nil, errors.New("请填最长边，或同时填宽与高")
	}
	ext := strings.ToLower(filepath.Ext(src))
	dst, err := resolveOutput(c, in.Path("output"), src, ext)
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, errors.New("输出文件与输入文件相同，请换一个输出路径")
	}

	args := []string{}
	mode := ""
	if edge > 0 {
		args = append(args, "-Z", strconv.Itoa(edge))
		mode = fmt.Sprintf("最长边 %d", edge)
	} else {
		args = append(args, "--resampleHeightWidth", strconv.Itoa(h), strconv.Itoa(w))
		mode = fmt.Sprintf("%dx%d", w, h)
	}
	args = append(args, src, "--out", dst)

	c.Logf("缩放：%s（%s）", filepath.Base(src), mode)
	c.Progress(10, "调用 sips")
	if err := runSips(ctx, c, args); err != nil {
		return nil, err
	}
	if err := checkImageOutput(dst); err != nil {
		return nil, err
	}
	st, _ := os.Stat(dst)
	srcSt, _ := os.Stat(src)
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已缩放并写入 %s（%s）", filepath.Base(dst), humanSize(st.Size())),
		Data: map[string]any{
			"output": dst, "mode": mode, "size": st.Size(), "source_size": srcSt.Size(),
		},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// ---------- img.optimize（异步） ----------

type imgOptimize struct{}

func init() { Add(imgOptimize{}) }

func (imgOptimize) Meta() tool.Meta {
	ok, reason := needCmds("sips")
	return tool.Meta{
		ID: "img.optimize", Name: "JPEG 压缩", Category: "img", Icon: "compress",
		Summary: "按质量压缩 JPEG，报告前后字节与压缩率。",
		Async:   true, TimeoutSeconds: 300, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入图片", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Pictures/a.jpg", Help: "只能读允许的读根内的文件。"},
			{Name: "quality", Label: "JPEG 质量", Type: tool.TypeNumber, Default: 70,
				Min: Num(1), Max: Num(100), Help: "1-100，越大越清晰也越大。"},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (imgOptimize) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	q := int(in.Num("quality", 70))
	if q < 1 || q > 100 {
		return nil, fmt.Errorf("质量必须 1-100，收到 %d", q)
	}
	dst, err := resolveOutput(c, in.Path("output"), src, ".jpg")
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, errors.New("输出文件与输入文件相同，请换一个输出路径")
	}
	args := []string{"-s", "format", "jpeg", "-s", "formatOptions", strconv.Itoa(q), src, "--out", dst}
	c.Logf("压缩：%s（质量 %d）", filepath.Base(src), q)
	if err := runSips(ctx, c, args); err != nil {
		return nil, err
	}
	if err := checkImageOutput(dst); err != nil {
		return nil, err
	}
	srcSt, _ := os.Stat(src)
	st, _ := os.Stat(dst)
	ratio := 0.0
	if srcSt.Size() > 0 {
		ratio = 100 * (1 - float64(st.Size())/float64(srcSt.Size()))
	}
	data := map[string]any{
		"output": dst, "quality": q, "size_before": srcSt.Size(), "size_after": st.Size(),
		"bytes_saved": srcSt.Size() - st.Size(), "saved_percent": roundPercent(ratio),
	}
	msg := fmt.Sprintf("已压缩：%s → %s（省 %.1f%%）", humanSize(srcSt.Size()), humanSize(st.Size()), ratio)
	if st.Size() >= srcSt.Size() {
		// sips 不会保证变小，如实说清（可能本来就是低质量图）。
		msg = fmt.Sprintf("已输出，但没变小：%s → %s", humanSize(srcSt.Size()), humanSize(st.Size()))
		data["note"] = "质量设置高于原图，产物没有变小"
	}
	c.Progress(100, "完成")
	return &tool.Result{OK: true, Msg: msg, Data: data,
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}}}, nil
}

// ---------- img.thumbnail（异步） ----------

type imgThumb struct{}

func init() { Add(imgThumb{}) }

func (imgThumb) Meta() tool.Meta {
	ok, reason := needCmds("qlmanage")
	return tool.Meta{
		ID: "img.thumbnail", Name: "生成缩略图", Category: "img", Icon: "thumb",
		Summary: "用 QuickLook 给任意文件生成 PNG 缩略图。",
		Async:   true, TimeoutSeconds: 180, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入文件", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Pictures/a.psd", Help: "只能读允许的读根内的文件。"},
			{Name: "size", Label: "缩略图边长", Type: tool.TypeNumber, Default: 512,
				Min: Num(1), Max: Num(4096), Help: "最长边像素，QuickLook 按此尺寸渲染。"},
			{Name: "output", Label: "输出目录或文件名", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (imgThumb) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	size := int(in.Num("size", 512))
	if size <= 0 {
		size = 512
	}
	outArg := in.Path("output")
	// qlmanage 只往目录里写 "<文件名>.png"，所以先定目录再算产物名。
	dir, dst, err := thumbnailTarget(c, outArg, src)
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, errors.New("输出文件与输入文件相同，请换一个输出路径")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("建输出目录失败：%v", err)
	}
	c.Logf("生成缩略图：%s（%dpx）", filepath.Base(src), size)
	c.Progress(10, "调用 qlmanage")
	res := c.Exec.Run(ctx, 90*time.Second, "qlmanage", "-t", "-s", strconv.Itoa(size), "-o", dir, src)
	if res.TimedOut {
		return nil, errors.New("qlmanage 超过 90 秒未完成，已终止")
	}
	// qlmanage 对没有缩略图预览的文件也会"退出码 0 但不产出"，只看退出码会谎报成功（坑 F1）。
	produced := filepath.Join(dir, filepath.Base(src)+".png")
	st, statErr := os.Stat(produced)
	if statErr != nil || st.Size() == 0 {
		return nil, fmt.Errorf("qlmanage 没有产出缩略图（退出码 %d）：%s", res.ExitCode, failureReason(res))
	}
	if produced != dst {
		if err := os.Rename(produced, dst); err != nil {
			return nil, fmt.Errorf("整理缩略图失败：%v", err)
		}
	}
	st, _ = os.Stat(dst)
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已生成缩略图 %s（%s）", filepath.Base(dst), humanSize(st.Size())),
		Data:  map[string]any{"output": dst, "size": st.Size(), "max_edge": size},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// thumbnailTarget 决定 qlmanage 的输出目录与最终产物名。
func thumbnailTarget(c *tool.Ctx, given, src string) (dir, dst string, err error) {
	root := c.Guard.WriteRoots()[0]
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)) + ".png"
	if given == "" {
		return root, uniquePath(filepath.Join(root, base)), nil
	}
	if st, e := os.Stat(given); e == nil && st.IsDir() {
		return given, uniquePath(filepath.Join(given, base)), nil
	}
	if filepath.Ext(given) == "" {
		// 看起来是目录但还没建：建好后直接放进去。
		return given, uniquePath(filepath.Join(given, base)), nil
	}
	return filepath.Dir(given), given, nil
}

// ---------- img.icns（异步） ----------

type imgIcns struct{}

func init() { Add(imgIcns{}) }

func (imgIcns) Meta() tool.Meta {
	ok, reason := needCmds("sips", "iconutil")
	return tool.Meta{
		ID: "img.icns", Name: "生成 icns 图标", Category: "img", Icon: "icon",
		Summary: "把一张方图按图标规范缩放并打包成 .icns。",
		Async:   true, TimeoutSeconds: 300, Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入图片（一行一个）", Type: tool.TypeTextarea, Required: true, Multiline: true,
				Placeholder: "/Users/你/Pictures/icon.png\n（只取第一张）",
				Help:        "每行一个能读到的图片路径；只用第一张做母图。"},
			{Name: "name", Label: "图标名", Type: tool.TypeText, Default: "AppIcon",
				Placeholder: "AppIcon", Help: "产物文件名，不含 .icns 后缀。"},
			{Name: "output", Label: "输出文件或目录", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (imgIcns) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	lines, err := readPathLines(c, in.Str("input"), fsroot.Read, 1)
	if err != nil {
		return nil, err
	}
	master := lines[0]
	name := strings.TrimSpace(in.Str("name"))
	if name == "" {
		name = "AppIcon"
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		return nil, errors.New("图标名不能包含 / \\ : * ? \" < > |")
	}
	dst := icnsTarget(c, in.Path("output"), name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, fmt.Errorf("建输出目录失败：%v", err)
	}

	pixels, err := imagePixels(ctx, c, master)
	if err != nil {
		return nil, err
	}
	if pixels < 512 {
		return nil, fmt.Errorf("母图最长边只有 %d 像素，至少需要 512", pixels)
	}

	work, err := os.MkdirTemp(c.TempDir, "icns-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	iconset := filepath.Join(work, name+".iconset")
	if err := os.MkdirAll(iconset, 0o700); err != nil {
		return nil, err
	}

	c.Progress(10, "生成各尺寸")
	for _, s := range icnsSizes {
		file := fmt.Sprintf("icon_%dx%d%s.png", s.px, s.px, s.suffix)
		target := filepath.Join(iconset, file)
		if err := runSips(ctx, c, []string{"-z", strconv.Itoa(s.px), strconv.Itoa(s.px), master, "--out", target}); err != nil {
			return nil, err
		}
		if err := checkImageOutput(target); err != nil {
			return nil, err
		}
	}
	c.Progress(70, "调用 iconutil")
	res := c.Exec.Run(ctx, 120*time.Second, "iconutil", "-c", "icns", iconset, "-o", dst)
	if res.TimedOut {
		return nil, errors.New("iconutil 超过 120 秒未完成，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("iconutil 失败（退出码 %d）：%s", res.ExitCode, failureReason(res))
	}
	st, statErr := os.Stat(dst)
	if statErr != nil || st.Size() == 0 {
		return nil, fmt.Errorf("iconutil 报告成功但没有产出：%s", filepath.Base(dst))
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已生成图标 %s（%s）", filepath.Base(dst), humanSize(st.Size())),
		Data: map[string]any{"output": dst, "size": st.Size(),
			"variants": len(icnsSizes), "source_pixels": pixels},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

type icnsSize struct {
	px     int
	suffix string
}

// icnsSizes 是 iconutil 要求的完整尺寸集合（缺一个就拒绝打包）。
var icnsSizes = []icnsSize{
	{16, ""}, {16, "@2x"}, {32, ""}, {32, "@2x"},
	{128, ""}, {128, "@2x"}, {256, ""}, {256, "@2x"},
	{512, ""}, {512, "@2x"},
}

func icnsTarget(c *tool.Ctx, given, name string) string {
	file := name + ".icns"
	if given == "" {
		return uniquePath(filepath.Join(c.Guard.WriteRoots()[0], file))
	}
	if st, err := os.Stat(given); err == nil && st.IsDir() {
		return uniquePath(filepath.Join(given, file))
	}
	if filepath.Ext(given) == "" {
		return uniquePath(filepath.Join(given, file))
	}
	return given
}

// ---------- 共用小工具 ----------

// needCmds 是首屏**便宜**判据：只看命令在不在。
func needCmds(names ...string) (bool, string) {
	missing := []string{}
	for _, n := range names {
		if _, ok := execx.LookPath(n); !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return true, ""
	}
	return false, "系统缺少命令：" + strings.Join(missing, "、")
}

// runSips 跑一条 sips 命令；超时与失败都带上真实原因。
func runSips(ctx context.Context, c *tool.Ctx, args []string) error {
	res := c.Exec.Run(ctx, 5*time.Minute, "sips", args...)
	if res.TimedOut {
		return errors.New("sips 超过 5 分钟未完成，已终止")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sips 失败（退出码 %d）：%s", res.ExitCode, failureReason(res))
	}
	return nil
}

// checkImageOutput 复核产物真的存在且非空（sips 会"退出码 0 但没产出"，坑 F1）。
func checkImageOutput(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("命令报告成功但没有产出文件：%s", filepath.Base(path))
	}
	if st.Size() == 0 {
		return fmt.Errorf("产物是 0 字节：%s", filepath.Base(path))
	}
	return nil
}

// imagePixels 读图片最长边像素；先试 sips，失败再退回 Go 解码。
func imagePixels(ctx context.Context, c *tool.Ctx, path string) (int, error) {
	res := c.Exec.Run(ctx, 30*time.Second, "sips", "-g", "pixelWidth", "-g", "pixelHeight", path)
	if res.ExitCode == 0 {
		info := parseSipsInfo(res.Stdout)
		if w, h, ok := sipsPixels(info); ok {
			if w > h {
				return w, nil
			}
			return h, nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("读不到图片：%v", err)
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, fmt.Errorf("不是能识别的图片：%v", err)
	}
	if cfg.Width > cfg.Height {
		return cfg.Width, nil
	}
	return cfg.Height, nil
}

// readPathLines 把 textarea 的多行拆开并**逐行过读根闸门**。
//
// 契约里的 path 参数只能单选，所以多文件必须在这里逐行调用 fsroot 校验，
// 绝不自己拼/清洗用户路径（坑 A4）。
func readPathLines(c *tool.Ctx, raw string, mode fsroot.Mode, limit int) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, ln := range strings.Split(raw, "\n") {
		p := strings.TrimSpace(ln)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		real, err := c.Guard.Resolve(mode, p, false)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行路径不可用：%w", len(out)+1, err)
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		out = append(out, real)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		return nil, errors.New("没有可用路径，请每行填一个绝对路径")
	}
	return out, nil
}

func roundPercent(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

package tools

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tool"
)

// registryForTest 建一份注册好全部工具的注册表。
func registryForTest(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	RegisterAll(reg)
	return reg
}

// imgShimPATH 造一个临时 PATH：把指定的假命令排在真实 PATH 之前。
func imgShimPATH(t *testing.T, shims map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range shims {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// writeGoPNG 用 Go 的 image/png 现生成素材（不依赖 CLT / Python）。
func writeGoPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / max1(w-1)), G: uint8(y * 255 / max1(h-1)), B: 40, A: 255})
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

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// ---------- 元数据纪律 ----------

func TestImageToolsRegisteredAndHonest(t *testing.T) {
	reg := registryForTest(t)
	// category id 只允许用契约里已登记中文名的分类（没登记会退化成英文 id）。
	known := map[string]bool{}
	for id := range tool.CategoryTitles {
		known[id] = true
	}
	for _, id := range []string{"img.info", "img.resize", "img.optimize", "img.thumbnail", "img.icns"} {
		m := metaOf(t, reg, id)
		if m.Category != "img" {
			t.Errorf("%s: 分类应为 img，实际 %q", id, m.Category)
		}
		if !known[m.Category] {
			t.Errorf("%s: 分类 %q 没有登记中文名", id, m.Category)
		}
		if m.Danger {
			t.Errorf("%s: 本轮不允许危险工具", id)
		}
		if len([]rune(m.Summary)) > 40 {
			t.Errorf("%s: summary 超过 40 字：%q", id, m.Summary)
		}
	}
	// 大动作必须异步：缩放/压缩/缩略图/打包。
	for _, id := range []string{"img.resize", "img.optimize", "img.thumbnail", "img.icns"} {
		if m := metaOf(t, reg, id); !m.Async {
			t.Errorf("%s: 耗时动作必须标 async", id)
		}
	}
	if m := metaOf(t, reg, "img.info"); m.Async {
		t.Error("img.info 是只读快操作，应为同步")
	}
}

func TestImageAvailabilityFollowsLookPath(t *testing.T) {
	reg := registryForTest(t)
	cases := []struct {
		id  string
		bin string
	}{
		{"img.info", "sips"},
		{"img.resize", "sips"},
		{"img.optimize", "sips"},
		{"img.thumbnail", "qlmanage"},
		{"img.icns", "iconutil"},
	}
	for _, c := range cases {
		m := metaOf(t, reg, c.id)
		_, found := execx.LookPath(c.bin)
		if found != m.Available {
			t.Errorf("%s: available=%v，但 %s 存在=%v", c.id, m.Available, c.bin, found)
		}
		if !m.Available && !strings.Contains(m.UnavailableReason, c.bin) {
			t.Errorf("%s: 不可用原因应点出缺哪个命令，实际 %q", c.id, m.UnavailableReason)
		}
	}
}

// 用临时 PATH shim 构造"命令不存在"，断言原因说人话。
func TestImageUnavailableWithoutCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // 空目录：什么都不存在
	reg := registryForTest(t)
	for _, id := range []string{"img.info", "img.resize", "img.optimize", "img.thumbnail", "img.icns"} {
		m := metaOf(t, reg, id)
		if m.Available {
			t.Errorf("%s: 空 PATH 下不该可用", id)
			continue
		}
		if !strings.Contains(m.UnavailableReason, "缺少命令") {
			t.Errorf("%s: 原因应是人话，实际 %q", id, m.UnavailableReason)
		}
	}
}

// 假 sips：退出码 0 但什么都不产出 —— 工具必须如实失败（坑 F1）。
func TestImgResizeReportsMissingOutput(t *testing.T) {
	t.Setenv("PATH", imgShimPATH(t, map[string]string{"sips": "#!/bin/sh\nexit 0\n"}))
	b := newBench(t)
	src := filepath.Join(b.home, "a.png")
	writeGoPNG(t, src, 8, 8)
	_, snap := b.run("img.resize", map[string]any{"input": src, "max_edge": 4})
	if snap.Status == "succeeded" {
		t.Fatalf("假 sips 没产出，不该报成功：%+v", snap)
	}
	if !strings.Contains(snap.Error, "产出") {
		t.Errorf("失败原因应说明没有产出，实际 %q", snap.Error)
	}
}

// 假 sips 报错：stderr 的关键行要带出来。
func TestImgResizeSurfacesStderr(t *testing.T) {
	t.Setenv("PATH", imgShimPATH(t, map[string]string{"sips": "#!/bin/sh\necho 'Error: 无法识别图像' >&2\nexit 13\n"}))
	b := newBench(t)
	src := filepath.Join(b.home, "a.png")
	writeGoPNG(t, src, 8, 8)
	_, snap := b.run("img.resize", map[string]any{"input": src, "max_edge": 4})
	if snap.Status == "succeeded" {
		t.Fatal("sips 报错时不该报成功")
	}
	if !strings.Contains(snap.Error, "无法识别图像") {
		t.Errorf("应带上 stderr 关键行，实际 %q", snap.Error)
	}
	if !strings.Contains(snap.Error, "13") {
		t.Errorf("应带上退出码，实际 %q", snap.Error)
	}
}

// qlmanage 对无预览的文件"退出码 0 但不产出"。
func TestImgThumbnailNoOutputIsFailure(t *testing.T) {
	t.Setenv("PATH", imgShimPATH(t, map[string]string{"qlmanage": "#!/bin/sh\nexit 0\n"}))
	b := newBench(t)
	src := filepath.Join(b.home, "a.png")
	writeGoPNG(t, src, 8, 8)
	_, snap := b.run("img.thumbnail", map[string]any{"input": src, "size": 64})
	if snap.Status == "succeeded" {
		t.Fatal("没有产出时不该报成功")
	}
	if !strings.Contains(snap.Error, "没有产出") {
		t.Errorf("原因应说明没有产出缩略图，实际 %q", snap.Error)
	}
}

// 参数非法要给出可读原因，而不是静默成功。
func TestImgResizeRequiresSize(t *testing.T) {
	b := newBench(t)
	src := filepath.Join(b.home, "a.png")
	writeGoPNG(t, src, 8, 8)
	_, snap := b.run("img.resize", map[string]any{"input": src})
	if snap.Status == "succeeded" {
		t.Fatal("既没最长边也没宽高，应报错")
	}
	if !strings.Contains(snap.Error, "最长边") {
		t.Errorf("原因应提示怎么填，实际 %q", snap.Error)
	}
}

// ---------- 真实 sips（没有就跳过并打印原因） ----------

func TestImgInfoResizeOptimizeReal(t *testing.T) {
	if _, ok := execx.LookPath("sips"); !ok {
		t.Skip("本机没有 sips，跳过真实图片处理")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "src.png")
	writeGoPNG(t, src, 64, 48)

	// img.info：尺寸必须与素材一致。
	res, _ := b.run("img.info", map[string]any{"input": src})
	data := res.Data.(map[string]any)
	if data["width"] != 64 || data["height"] != 48 {
		t.Fatalf("尺寸应读出 64x48，实际 %v x %v", data["width"], data["height"])
	}
	info, _ := data["info"].(map[string]string)
	if info["format"] != "png" {
		t.Errorf("格式应读出 png，实际 %q", info["format"])
	}

	// img.resize：产物必须真的变小且落在写根。
	out := filepath.Join(b.write, "small.png")
	_, snap := b.run("img.resize", map[string]any{"input": src, "max_edge": 16, "output": out})
	data = b.resultData(t, snap)
	got, _ := data["output"].(string)
	if !strings.HasPrefix(got, b.write) {
		t.Fatalf("产物应落在写根内，实际 %s", got)
	}
	w, h := pngSize(t, got)
	if w != 16 || h != 12 {
		t.Fatalf("最长边 16 的等比缩放应得 16x12，实际 %dx%d", w, h)
	}

	// img.optimize：jpg 产物 + 报告前后字节。
	jpgOut := filepath.Join(b.write, "small.jpg")
	_, snap2 := b.run("img.optimize", map[string]any{"input": src, "quality": 40, "output": jpgOut})
	d2 := b.resultData(t, snap2)
	if d2["size_after"].(int64) == 0 {
		t.Fatal("压缩产物是 0 字节")
	}
	if _, ok := d2["saved_percent"]; !ok {
		t.Fatal("应报告压缩率")
	}
}

// pngSize 独立读产物尺寸（不复用工具自己的结论）。
func pngSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打不开产物 %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatalf("产物不是合法 PNG: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestImgThumbnailReal(t *testing.T) {
	if _, ok := execx.LookPath("qlmanage"); !ok {
		t.Skip("本机没有 qlmanage，跳过缩略图实测")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "thumb-src.png")
	writeGoPNG(t, src, 128, 128)
	outDir := filepath.Join(b.write, "thumbs")
	_, snap := b.run("img.thumbnail", map[string]any{"input": src, "size": 64, "output": outDir})
	data := b.resultData(t, snap)
	got, _ := data["output"].(string)
	st, err := os.Stat(got)
	if err != nil || st.Size() == 0 {
		t.Fatalf("缩略图产物无效：%s (%v)", got, err)
	}
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("qlmanage 产物应是 png，实际 %s", got)
	}
}

func TestImgIcnsReal(t *testing.T) {
	if _, ok := execx.LookPath("iconutil"); !ok {
		t.Skip("本机没有 iconutil，跳过 icns 实测")
	}
	if _, ok := execx.LookPath("sips"); !ok {
		t.Skip("本机没有 sips，跳过 icns 实测")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "master.png")
	writeGoPNG(t, src, 600, 600)
	_, snap := b.run("img.icns", map[string]any{
		"input": src, "name": "Wave2Test", "output": filepath.Join(b.write, "Wave2Test.icns")})
	data := b.resultData(t, snap)
	got, _ := data["output"].(string)
	st, err := os.Stat(got)
	if err != nil || st.Size() == 0 {
		t.Fatalf("icns 产物无效：%s (%v)", got, err)
	}
	head := make([]byte, 4)
	f, err := os.Open(got)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Read(head); err != nil {
		t.Fatal(err)
	}
	if string(head) != "icns" {
		t.Fatalf("产物不是 icns（magic=%q）", string(head))
	}

	// 母图太小时必须如实拒绝，而不是产出一个坏图标。
	small := filepath.Join(b.home, "small-master.png")
	writeGoPNG(t, small, 128, 128)
	_, snap2 := b.run("img.icns", map[string]any{"input": small, "name": "TooSmall"})
	if snap2.Status == "succeeded" {
		t.Fatal("母图只有 128 像素时应拒绝")
	}
	if !strings.Contains(snap2.Error, "512") {
		t.Errorf("原因应说明最小尺寸，实际 %q", snap2.Error)
	}
}

// readPathLines 必须逐行过读根闸门（不许工具自己拼路径）。
func TestReadPathLinesGuardsEveryLine(t *testing.T) {
	b := newBench(t)
	inside := filepath.Join(b.home, "a.png")
	writeGoPNG(t, inside, 4, 4)
	outside := filepath.Join(t.TempDir(), "b.png")

	if _, err := readPathLines(b.ctx, inside+"\n"+outside, fsroot.Read, 0); err == nil {
		t.Fatal("读根外的行必须被拒")
	}
	got, err := readPathLines(b.ctx, inside+"\n\n# 注释行\n"+inside, fsroot.Read, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应去重并跳过空行，实际 %v", got)
	}
	if _, err := readPathLines(b.ctx, "   ", fsroot.Read, 0); err == nil {
		t.Fatal("全空应报错")
	}
}

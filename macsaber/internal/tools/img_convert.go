package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// imgConvert 是**异步工具**打样：sips 图片格式转换 + 可选限制最长边。
type imgConvert struct{}

func init() { Add(imgConvert{}) }

func (imgConvert) Meta() tool.Meta {
	_, ok := execx.LookPath("sips")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/sips"
	}
	return tool.Meta{
		ID: "img.convert", Name: "图片格式转换", Category: "img", Icon: "image",
		Summary: "调用系统 sips 转换格式，可限制最长边。",
		Async:   true, TimeoutSeconds: 300,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入图片", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Pictures/a.png", Help: "只能读允许的读根内的文件。"},
			{Name: "format", Label: "目标格式", Type: tool.TypeSelect, Required: true,
				Default: "jpeg", Options: formatOptions(),
				Help: "输出格式，由系统 sips 支持列表决定。"},
			{Name: "max_edge", Label: "最长边像素", Type: tool.TypeNumber, Min: Num(1), Max: Num(20000),
				Placeholder: "留空表示不缩放", Help: "按最长边等比缩放，留空保持原尺寸。"},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

func formatOptions() []tool.Option {
	out := make([]tool.Option, 0, len(sipsFormats))
	for _, f := range sipsFormats {
		out = append(out, tool.Option{Value: f, Label: strings.ToUpper(f)})
	}
	return out
}

// Timeout 让 sips 单张图的等待上限短一些。
func (imgConvert) Timeout() time.Duration { return 5 * time.Minute }

func (imgConvert) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	dst, err := resolveOutput(c, in.Path("output"), src, strings.ToLower(in.Str("format")))
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, errors.New("输出文件与输入文件相同，请换一个输出路径")
	}
	args := []string{"-s", "format", strings.ToLower(in.Str("format"))}
	if e := in.Num("max_edge", 0); e > 0 {
		args = append(args, "-Z", fmt.Sprintf("%d", int(e)))
	}
	args = append(args, src, "--out", dst)

	c.Logf("开始转换：%s → %s", filepath.Base(src), filepath.Base(dst))
	c.Progress(10, "调用 sips")
	res := c.Exec.Run(ctx, imgConvert{}.Timeout(), "sips", args...)
	if res.TimedOut {
		return nil, fmt.Errorf("sips 超过 %s 未完成，已终止", res.Duration.Round(time.Second))
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("sips 失败（退出码 %d）：%s", res.ExitCode, failureReason(res))
	}
	st, err := os.Stat(dst)
	if err != nil {
		// sips 对不支持的格式会"退出码 0 但没产出"，必须如实报错（坑 F1）。
		return nil, fmt.Errorf("sips 报告成功但没有产出文件：%s", dst)
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("sips 产出了 0 字节文件：%s", dst)
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已转换并写入 %s（%s）", filepath.Base(dst), humanSize(st.Size())),
		Data: map[string]any{
			"output": dst, "format": in.Str("format"), "size": st.Size(),
			"max_edge": in.Num("max_edge", 0),
		},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// resolveOutput 决定输出路径：留空则写到第一个写根并自动命名。
func resolveOutput(c *tool.Ctx, given, src, format string) (string, error) {
	ext := "." + format
	if format == "jpeg" {
		ext = ".jpg"
	}
	if given == "" {
		root := c.Guard.WriteRoots()[0]
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		cand := filepath.Join(root, base+ext)
		return uniquePath(cand), nil
	}
	if st, err := os.Stat(given); err == nil && st.IsDir() {
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		return uniquePath(filepath.Join(given, base+ext)), nil
	}
	if filepath.Ext(given) == "" {
		return given + ext, nil
	}
	return given, nil
}

// uniquePath 在重名时追加 -1/-2…，绝不静默覆盖用户已有文件。
func uniquePath(p string) string {
	if _, err := os.Lstat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 1; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, err := os.Lstat(cand); err != nil {
			return cand
		}
	}
	return p
}

func sameFile(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	as, ok1 := ai.Sys().(*syscall.Stat_t)
	bs, ok2 := bi.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false
	}
	return as.Dev == bs.Dev && as.Ino == bs.Ino
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "无输出"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// failureReason 汇合 stdout 与 stderr：sips 的报错经常只在 stderr，
// 只看 stdout 会得到一句没有信息量的错误（坑 F3）。
func failureReason(res *execx.Result) string {
	out := strings.TrimSpace(res.Stdout)
	errOut := strings.TrimSpace(res.Stderr)
	switch {
	case out == "" && errOut == "":
		return "无输出"
	case errOut == "":
		return firstLine(out)
	case out == "":
		return firstLine(errOut)
	}
	return firstLine(out) + " / " + firstLine(errOut)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n)
	for _, u := range units {
		f /= unit
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return fmt.Sprintf("%.1f PB", f/unit)
}

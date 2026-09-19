package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// 本文件用系统自带 textutil/iconv/plutil/shasum/diff，全程不经 shell。

// ============================================================================
//  text.doc_convert —— textutil 文档互转（异步）
// ============================================================================

type textDocConvert struct{}

func init() { Add(textDocConvert{}) }

// docFormats 是 textutil -convert 支持的常用格式。
var docFormats = []string{"txt", "rtf", "html", "docx", "odt"}

func (textDocConvert) Meta() tool.Meta {
	_, ok := execx.LookPath("textutil")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/textutil"
	}
	return tool.Meta{
		ID: "text.doc_convert", Name: "文档互转", Category: "text", Icon: "file-text",
		Summary: "textutil 互转 txt/rtf/html/docx/odt 文档。",
		Async:   true, TimeoutSeconds: 600,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入文档", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Documents/a.docx", Help: "只能读允许的读根内的文件。"},
			{Name: "format", Label: "目标格式", Type: tool.TypeSelect, Required: true, Default: "txt",
				Options: docFormatOptions()},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

func docFormatOptions() []tool.Option {
	labels := map[string]string{
		"txt": "纯文本 TXT", "rtf": "RTF", "html": "HTML",
		"docx": "Word DOCX", "odt": "OpenDocument ODT",
	}
	out := make([]tool.Option, 0, len(docFormats))
	for _, f := range docFormats {
		label := labels[f]
		if label == "" {
			label = strings.ToUpper(f)
		}
		out = append(out, tool.Option{Value: f, Label: label})
	}
	return out
}

func (textDocConvert) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	format := strings.ToLower(in.Str("format"))
	dst, err := resolveOutput(c, in.Path("output"), src, format)
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, fmt.Errorf("输出文件与输入文件相同，请换一个输出路径")
	}
	c.Logf("转换文档：%s → %s", filepath.Base(src), format)
	c.Progress(10, "调用 textutil")
	res := c.Exec.Run(ctx, 10*time.Minute, "textutil", "-convert", format, "-output", dst, src)
	if res.ExitCode != 0 {
		return nil, cmdFailure(c, "textutil 转换失败", res)
	}
	st, err := os.Stat(dst)
	if err != nil {
		return nil, fmt.Errorf("textutil 报告成功但没有产出文件：%s", filepath.Base(dst))
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("textutil 产出了 0 字节文件：%s", filepath.Base(dst))
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK:  true,
		Msg: fmt.Sprintf("已转换为 %s（%s）", strings.ToUpper(format), humanSize(st.Size())),
		Data: map[string]any{
			"input": src, "output": dst, "format": format, "size": st.Size(),
		},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// ============================================================================
//  text.encoding_fix —— iconv 编码探测与转换（异步）
// ============================================================================

type textEncodingFix struct{}

func init() { Add(textEncodingFix{}) }

// maxEncodingBytes 是内存内转换的上限：iconv 只能写 stdout，过大必须拒绝而不是悄悄截断。
const maxEncodingBytes = 32 << 20

func (textEncodingFix) Meta() tool.Meta {
	_, ok := execx.LookPath("iconv")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/iconv"
	}
	return tool.Meta{
		ID: "text.encoding_fix", Name: "编码修复", Category: "text", Icon: "type",
		Summary: "探测并修复中文乱码，iconv 转到目标编码。",
		Async:   true, TimeoutSeconds: 600,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入文件", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Documents/garbled.txt", Help: "只能读允许的读根内的文件。"},
			{Name: "from", Label: "源编码", Type: tool.TypeSelect, Required: true, Default: "auto",
				Options: []tool.Option{
					{Value: "auto", Label: "自动探测"},
					{Value: "UTF-8", Label: "UTF-8"},
					{Value: "GBK", Label: "GBK / 国标"},
					{Value: "GB18030", Label: "GB18030"},
					{Value: "BIG5", Label: "BIG5 / 繁体"},
					{Value: "SHIFT_JIS", Label: "Shift_JIS / 日文"},
					{Value: "EUC-JP", Label: "EUC-JP"},
					{Value: "LATIN1", Label: "Latin-1"},
					{Value: "UTF-16LE", Label: "UTF-16LE"},
					{Value: "UTF-16BE", Label: "UTF-16BE"},
				}},
			{Name: "to", Label: "目标编码", Type: tool.TypeSelect, Default: "UTF-8",
				Options: []tool.Option{
					{Value: "UTF-8", Label: "UTF-8"},
					{Value: "GBK", Label: "GBK / 国标"},
					{Value: "GB18030", Label: "GB18030"},
					{Value: "BIG5", Label: "BIG5 / 繁体"},
					{Value: "UTF-16LE", Label: "UTF-16LE"},
				}},
			{Name: "skip_invalid", Label: "跳过非法字节", Type: tool.TypeBool, Default: false,
				Help: "勾选后加 iconv -c，坏字节直接丢弃。"},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (textEncodingFix) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	from := strings.TrimSpace(in.Str("from"))
	to := strings.TrimSpace(in.Str("to"))
	if to == "" {
		to = "UTF-8"
	}
	st, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败：%s", redactHome(c, err.Error()))
	}
	if st.Size() > maxEncodingBytes {
		return nil, fmt.Errorf("文件 %s 超过 32MB，编码转换暂不支持（iconv 只能写标准输出）", humanSize(st.Size()))
	}
	why := "按用户指定源编码"
	if from == "" || from == "auto" {
		from, why, err = detectSourceEncoding(ctx, c, src)
		if err != nil {
			return nil, err
		}
	}
	dst, err := resolveOutput(c, in.Path("output"), src, outputExt(src))
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, fmt.Errorf("输出文件与输入文件相同，请换一个输出路径")
	}

	args := []string{"-f", from, "-t", to}
	if in.Bool("skip_invalid", false) {
		args = append(args, "-c")
	}
	args = append(args, src)
	c.Logf("编码转换：%s → %s", from, to)
	c.Progress(30, "调用 iconv")
	// iconv 无 -o：只能收回 stdout；上限按 2 倍输入 + 余量，超出即如实报错。
	ex := &execx.Execer{MaxOutputBytes: int(st.Size())*2 + 4096}
	res := ex.Run(ctx, 10*time.Minute, "iconv", args...)
	if res.ExitCode != 0 {
		return nil, cmdFailure(c, "iconv 转换失败", res)
	}
	if res.TruncOut {
		return nil, fmt.Errorf("iconv 输出超过内存上限被截断，未写出文件")
	}
	if err := os.WriteFile(dst, []byte(res.Stdout), 0o644); err != nil {
		return nil, fmt.Errorf("写输出失败：%s", redactHome(c, err.Error()))
	}
	out, err := os.Stat(dst)
	if err != nil {
		return nil, fmt.Errorf("输出文件不存在：%s", filepath.Base(dst))
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK:  true,
		Msg: fmt.Sprintf("已从 %s 转为 %s（%s）", from, to, humanSize(out.Size())),
		Data: map[string]any{
			"input": src, "output": dst, "from_requested": in.Str("from"),
			"from_used": from, "detected": why, "to": to,
			"bytes_in": st.Size(), "bytes_out": out.Size(),
		},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: out.Size()}},
	}, nil
}

func outputExt(src string) string {
	ext := strings.TrimPrefix(filepath.Ext(src), ".")
	if ext == "" {
		return "txt"
	}
	return ext
}

// detectSourceEncoding：UTF-8 自证 → file -I 可信名 → 按中文编码逐个试（坑 T1）。
func detectSourceEncoding(ctx context.Context, c *tool.Ctx, src string) (string, string, error) {
	sample, truncated, err := readSample(src, 1<<16)
	if err != nil {
		return "", "", fmt.Errorf("读取文件失败：%s", redactHome(c, err.Error()))
	}
	if len(sample) == 0 {
		return "UTF-8", "空文件按 UTF-8 处理", nil
	}
	probe := sample
	// 截断处可能切开多字节字符，去掉尾部 8 字节再判（最长序列 4 字节）。
	if truncated && len(probe) > 8 {
		probe = probe[:len(probe)-8]
	}
	if utf8.Valid(probe) {
		return "UTF-8", "内容本身是合法 UTF-8", nil
	}
	charset := ""
	if _, ok := execx.LookPath("file"); ok {
		res := c.Exec.Run(ctx, 20*time.Second, "file", "-I", src)
		if res.ExitCode == 0 {
			charset = parseFileCharset(res.Stdout)
		}
	}
	if enc, ok := confidentCharset(charset); ok {
		return enc, "file -I 判定 " + charset, nil
	}
	// file 对 GBK 常给 iso-8859-1（不可信），逐个试；不含 LATIN1，避免"永远成功"。
	for _, cand := range []string{"GB18030", "BIG5", "SHIFT_JIS", "EUC-JP"} {
		if iconvTrial(ctx, c, cand, probe) {
			why := "按 " + cand + " 试成功"
			if charset != "" {
				why = "file 只给 " + charset + "，" + why
			}
			return cand, why, nil
		}
	}
	return "", "", fmt.Errorf("无法确定源编码（file 给 %q），请手动指定源编码", charset)
}

func readSample(path string, max int) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, max)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == nil:
		return buf, true, nil
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return buf[:n], false, nil
	default:
		return nil, false, err
	}
}

// iconvTrial 用样本试一次转换，只看能否无损解出。
func iconvTrial(ctx context.Context, c *tool.Ctx, encoding string, sample []byte) bool {
	dir, err := os.MkdirTemp(c.TempDir, "ms-enc-")
	if err != nil {
		return false
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "sample.bin")
	if err := os.WriteFile(path, sample, 0o600); err != nil {
		return false
	}
	res := c.Exec.Run(ctx, 30*time.Second, "iconv", "-f", encoding, "-t", "UTF-8", path)
	return res.ExitCode == 0 && utf8.ValidString(res.Stdout)
}

func parseFileCharset(out string) string {
	idx := strings.Index(out, "charset=")
	if idx < 0 {
		return ""
	}
	s := out[idx+len("charset="):]
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), `"'`))
}

func confidentCharset(cs string) (string, bool) {
	switch cs {
	case "utf-8", "utf8":
		return "UTF-8", true
	case "utf-16le":
		return "UTF-16LE", true
	case "utf-16be":
		return "UTF-16BE", true
	case "utf-16":
		return "UTF-16LE", true
	case "gbk":
		return "GBK", true
	case "gb2312", "gb18030", "gb-18030":
		return "GB18030", true
	case "big5", "big-5", "big5-hkscs":
		return "BIG5", true
	case "shift_jis", "shift-jis", "sjis":
		return "SHIFT_JIS", true
	case "euc-jp":
		return "EUC-JP", true
	case "euc-kr":
		return "EUC-KR", true
	case "iso-2022-jp":
		return "ISO-2022-JP", true
	case "koi8-r":
		return "KOI8-R", true
	case "windows-1251":
		return "CP1251", true
	}
	return "", false
}

// ============================================================================
//  text.plist_json —— plutil 双向转换（同步）
// ============================================================================

type textPlistJSON struct{}

func init() { Add(textPlistJSON{}) }

func (textPlistJSON) Meta() tool.Meta {
	_, ok := execx.LookPath("plutil")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/plutil"
	}
	return tool.Meta{
		ID: "text.plist_json", Name: "plist 与 JSON", Category: "text", Icon: "code",
		Summary: "plist 与 JSON 双向转换，用系统 plutil。",
		Async:   false, TimeoutSeconds: 120,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入文件", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Library/Preferences/x.plist", Help: "只能读允许的读根内的文件。"},
			{Name: "direction", Label: "转换方向", Type: tool.TypeSelect, Required: true, Default: "plist2json",
				Options: []tool.Option{
					{Value: "plist2json", Label: "plist → JSON"},
					{Value: "json2plist", Label: "JSON → plist (xml1)"},
				}},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (textPlistJSON) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	dir := in.Str("direction")
	convert, ext := "json", "json"
	if dir == "json2plist" {
		convert, ext = "xml1", "plist"
	}
	dst, err := resolveOutput(c, in.Path("output"), src, ext)
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, fmt.Errorf("输出文件与输入文件相同，请换一个输出路径")
	}
	res := c.Exec.Run(ctx, 2*time.Minute, "plutil", "-convert", convert, "-o", dst, src)
	if res.ExitCode != 0 {
		return nil, cmdFailure(c, "plutil 转换失败", res)
	}
	st, err := os.Stat(dst)
	if err != nil {
		return nil, fmt.Errorf("plutil 报告成功但没有产出文件：%s", filepath.Base(dst))
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("plutil 产出了 0 字节文件：%s", filepath.Base(dst))
	}
	return &tool.Result{
		OK:  true,
		Msg: fmt.Sprintf("已转换为 %s（%s）", strings.ToUpper(ext), humanSize(st.Size())),
		Data: map[string]any{
			"input": src, "output": dst, "direction": dir, "convert": convert, "size": st.Size(),
		},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// ============================================================================
//  text.hash_file —— shasum 大文件哈希（同步，流式外部命令）
// ============================================================================

type textHashFile struct{}

func init() { Add(textHashFile{}) }

// shasumAlgos 映射算法名 → shasum -a 的数字。
var shasumAlgos = map[string]string{"sha1": "1", "sha256": "256", "sha512": "512"}

func (textHashFile) Meta() tool.Meta {
	_, ok := execx.LookPath("shasum")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/shasum"
	}
	return tool.Meta{
		ID: "text.hash_file", Name: "大文件哈希", Category: "text", Icon: "hash",
		Summary: "shasum 流式算大文件 sha1/sha256/sha512。",
		Async:   false, TimeoutSeconds: 3600,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "file", Label: "文件", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Downloads/big.dmg", Help: "只能读允许的读根内的文件。"},
			{Name: "algo", Label: "算法", Type: tool.TypeSelect, Required: true, Default: "sha256",
				Options: []tool.Option{
					{Value: "sha256", Label: "SHA-256"},
					{Value: "sha1", Label: "SHA-1"},
					{Value: "sha512", Label: "SHA-512"},
					{Value: "all", Label: "三种都算"},
				}},
		},
	}
}

func (textHashFile) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	file := in.Path("file")
	st, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败：%s", redactHome(c, err.Error()))
	}
	if st.IsDir() {
		return nil, fmt.Errorf("这是目录，不是文件")
	}
	algo := in.Str("algo")
	algos := []string{algo}
	if algo == "all" {
		algos = []string{"sha1", "sha256", "sha512"}
	}
	hashes := map[string]string{}
	for _, a := range algos {
		num, ok := shasumAlgos[a]
		if !ok {
			return nil, fmt.Errorf("不支持的算法：%s", a)
		}
		res := c.Exec.Run(ctx, 2*time.Hour, "shasum", "-a", num, "--", file)
		if res.ExitCode != 0 {
			return nil, cmdFailure(c, "shasum 计算失败", res)
		}
		fields := strings.Fields(res.Stdout)
		if len(fields) == 0 {
			return nil, fmt.Errorf("shasum 没有输出哈希值")
		}
		hashes[a] = fields[0]
		c.Logf("%s → %s", a, fields[0])
	}
	data := map[string]any{
		"path": file, "name": filepath.Base(file), "size": st.Size(), "algo": algo,
	}
	msg := ""
	if algo == "all" {
		data["hashes"] = hashes
		msg = "SHA-1 / SHA-256 / SHA-512 计算完成"
	} else {
		data["hash"] = hashes[algo]
		msg = strings.ToUpper(algo) + " 计算完成"
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// ============================================================================
//  text.compare —— diff -u 差异摘要（同步）
// ============================================================================

type textCompare struct{}

func init() { Add(textCompare{}) }

func (textCompare) Meta() tool.Meta {
	_, ok := execx.LookPath("diff")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/diff"
	}
	return tool.Meta{
		ID: "text.compare", Name: "文件差异对比", Category: "text", Icon: "git-compare",
		Summary: "对比两个文件的文本差异，只返回前 N 行。",
		Async:   false, TimeoutSeconds: 300,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "left", Label: "文件 A", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/a.txt", Help: "只能读允许的读根内的文件。"},
			{Name: "right", Label: "文件 B", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/b.txt", Help: "只能读允许的读根内的文件。"},
			{Name: "max_lines", Label: "最多显示行数", Type: tool.TypeNumber, Default: 200,
				Min: Num(1), Max: Num(5000), Help: "差异太大时只回前 N 行。"},
		},
	}
}

func (textCompare) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	left, right := in.Path("left"), in.Path("right")
	maxLines := in.Int("max_lines", 200)
	res := c.Exec.Run(ctx, 5*time.Minute, "diff", "-u", left, right)
	if res.TimedOut {
		return nil, cmdFailure(c, "diff 对比失败", res)
	}
	switch res.ExitCode {
	case 0, 1:
	default:
		return nil, cmdFailure(c, "diff 对比失败", res)
	}
	identical := res.ExitCode == 0
	lines := []string{}
	if s := strings.TrimRight(res.Stdout, "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	added, removed := 0, 0
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	shown := lines
	truncated := res.TruncOut
	if len(shown) > maxLines {
		shown = shown[:maxLines]
		truncated = true
	}
	stL, _ := os.Stat(left)
	stR, _ := os.Stat(right)
	data := map[string]any{
		"left":      map[string]any{"path": left, "name": filepath.Base(left), "size": fileSize(stL)},
		"right":     map[string]any{"path": right, "name": filepath.Base(right), "size": fileSize(stR)},
		"identical": identical, "exit_code": res.ExitCode,
		"diff_lines": len(lines), "shown_lines": len(shown),
		"added": added, "removed": removed, "truncated": truncated,
		"preview": strings.Join(shown, "\n"),
	}
	msg := "两个文件内容相同"
	if !identical {
		msg = fmt.Sprintf("发现差异：+%d / -%d 行", added, removed)
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

func fileSize(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
}

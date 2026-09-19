package tools

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tasks"
)

// gbkText 是 "中文测试\n" 的 GBK 字节（不依赖任何外部转换工具）。
var gbkText = []byte{0xD6, 0xD0, 0xCE, 0xC4, 0xB2, 0xE2, 0xCA, 0xD4, 0x0A}
var utf8Text = "中文测试\n"

// ---------- 编码探测（纯函数） ----------

func TestParseFileCharsetAndConfidence(t *testing.T) {
	if got := parseFileCharset("a.txt: text/plain; charset=iso-8859-1\n"); got != "iso-8859-1" {
		t.Errorf("charset = %q", got)
	}
	if got := parseFileCharset("a.txt: text/plain; charset=utf-8"); got != "utf-8" {
		t.Errorf("charset = %q", got)
	}
	if _, ok := confidentCharset("iso-8859-1"); ok {
		t.Error("iso-8859-1 对中文不可信，不该当成可信判据")
	}
	if enc, ok := confidentCharset("gbk"); !ok || enc != "GBK" {
		t.Errorf("gbk → %q %v", enc, ok)
	}
	if enc, ok := confidentCharset("big5"); !ok || enc != "BIG5" {
		t.Errorf("big5 → %q %v", enc, ok)
	}
}

// ---------- text.doc_convert ----------

func TestDocConvertRealRoundTrip(t *testing.T) {
	if _, ok := execx.LookPath("textutil"); !ok {
		t.Skip("本机没有 textutil，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "note.txt")
	if err := os.WriteFile(src, []byte("第一行：中文测试\n第二行：hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("text.doc_convert", map[string]any{
		"input": src, "format": "docx",
		"output": filepath.Join(b.write, "note.docx"),
	})
	d := b.resultData(t, snap)
	docx := d["output"].(string)
	if st, err := os.Stat(docx); err != nil || st.Size() == 0 {
		t.Fatalf("docx 没有产出：%v", err)
	}
	// 再转回 txt，用内容做独立复核（不看退出码）。
	_, snap2 := b.run("text.doc_convert", map[string]any{
		"input": docx, "format": "txt",
		"output": filepath.Join(b.write, "note-back.txt"),
	})
	d2 := b.resultData(t, snap2)
	back, err := os.ReadFile(d2["output"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), "中文测试") {
		t.Fatalf("往返后内容不对：%q", string(back))
	}
}

func TestDocConvertShimFailure(t *testing.T) {
	dir := shimDir(t, map[string]string{"textutil": "#!/bin/sh\necho \"假 textutil：转换失败\" >&2\nexit 1\n"})
	t.Setenv("PATH", dir)
	b := newBench(t)
	src := filepath.Join(b.home, "note.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("text.doc_convert", map[string]any{"input": src, "format": "docx"})
	if snap.Status != tasks.Failed {
		t.Fatalf("应失败，实际 %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "假 textutil") {
		t.Errorf("失败原因应带真实 stderr，实际：%s", snap.Error)
	}
}

// ---------- text.encoding_fix ----------

func TestEncodingFixAutoGBK(t *testing.T) {
	if _, ok := execx.LookPath("iconv"); !ok {
		t.Skip("本机没有 iconv，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "garbled.txt")
	if err := os.WriteFile(src, gbkText, 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("text.encoding_fix", map[string]any{
		"input": src, "from": "auto", "to": "UTF-8",
		"output": filepath.Join(b.write, "fixed.txt"),
	})
	d := b.resultData(t, snap)
	out, err := os.ReadFile(d["output"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != utf8Text {
		t.Fatalf("修复结果 = %q，期望 %q（探测说明：%v）", string(out), utf8Text, d["detected"])
	}
	if used, _ := d["from_used"].(string); used == "auto" || used == "" {
		t.Errorf("from_used 应是真实编码，实际 %q", used)
	}
	if s, _ := d["detected"].(string); s == "" {
		t.Error("应如实给出探测过程说明")
	}
}

func TestEncodingFixExplicitGBK(t *testing.T) {
	if _, ok := execx.LookPath("iconv"); !ok {
		t.Skip("本机没有 iconv，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "garbled2.txt")
	if err := os.WriteFile(src, gbkText, 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("text.encoding_fix", map[string]any{
		"input": src, "from": "GBK", "to": "UTF-8",
		"output": filepath.Join(b.write, "fixed2.txt"),
	})
	d := b.resultData(t, snap)
	out, _ := os.ReadFile(d["output"].(string))
	if string(out) != utf8Text {
		t.Fatalf("GBK 显式转换结果 = %q", string(out))
	}
}

func TestEncodingFixHonestWhenUndetectable(t *testing.T) {
	// iconv 恒失败 + 没有 file：自动探测必须如实报"无法确定"，不许瞎猜。
	dir := shimDir(t, map[string]string{"iconv": "#!/bin/sh\nexit 1\n"})
	t.Setenv("PATH", dir)
	b := newBench(t)
	src := filepath.Join(b.home, "weird.txt")
	if err := os.WriteFile(src, []byte{0xD6, 0xD0, 0xFF, 0xFE, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("text.encoding_fix", map[string]any{"input": src, "from": "auto"})
	if snap.Status != tasks.Failed {
		t.Fatalf("探测不出来时应失败，实际 %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "源编码") {
		t.Errorf("应提示手动指定源编码，实际：%s", snap.Error)
	}
}

func TestEncodingFixShimFailure(t *testing.T) {
	dir := shimDir(t, map[string]string{"iconv": "#!/bin/sh\necho \"假 iconv：非法字节序列\" >&2\nexit 1\n"})
	t.Setenv("PATH", dir)
	b := newBench(t)
	src := filepath.Join(b.home, "garbled3.txt")
	if err := os.WriteFile(src, gbkText, 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("text.encoding_fix", map[string]any{"input": src, "from": "GBK", "to": "UTF-8"})
	if snap.Status != tasks.Failed {
		t.Fatalf("应失败，实际 %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "假 iconv") {
		t.Errorf("失败原因应带真实 stderr，实际：%s", snap.Error)
	}
}

func TestEncodingFixRefusesToOverwriteSource(t *testing.T) {
	if _, ok := execx.LookPath("iconv"); !ok {
		t.Skip("本机没有 iconv，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.write, "keep.txt")
	if err := os.WriteFile(src, gbkText, 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("text.encoding_fix", map[string]any{
		"input": src, "from": "GBK", "to": "UTF-8", "output": src,
	})
	if snap.Status != tasks.Failed {
		t.Fatalf("输出等于输入应失败，实际 %s", snap.Status)
	}
	after, _ := os.ReadFile(src)
	if string(after) != string(gbkText) {
		t.Fatal("源文件被改动了")
	}
}

// ---------- text.plist_json ----------

func TestPlistJSONRealBothWays(t *testing.T) {
	if _, ok := execx.LookPath("plutil"); !ok {
		t.Skip("本机没有 plutil，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "prefs.plist")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>name</key><string>军刀</string><key>n</key><integer>3</integer></dict></plist>
`
	if err := os.WriteFile(src, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := b.run("text.plist_json", map[string]any{
		"input": src, "direction": "plist2json",
		"output": filepath.Join(b.write, "prefs.json"),
	})
	d := res.Data.(map[string]any)
	raw, err := os.ReadFile(d["output"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("产物不是合法 JSON：%v（%s）", err, raw)
	}
	if obj["name"] != "军刀" || obj["n"] != float64(3) {
		t.Fatalf("JSON 内容不对：%v", obj)
	}

	res2, _ := b.run("text.plist_json", map[string]any{
		"input": d["output"].(string), "direction": "json2plist",
		"output": filepath.Join(b.write, "back.plist"),
	})
	d2 := res2.Data.(map[string]any)
	back, err := os.ReadFile(d2["output"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), "军刀") || !strings.Contains(string(back), "<plist") {
		t.Fatalf("plist 产物不对：%s", back)
	}
}

func TestPlistJSONShimFailure(t *testing.T) {
	dir := shimDir(t, map[string]string{"plutil": "#!/bin/sh\necho \"假 plutil：格式不对\" >&2\nexit 1\n"})
	t.Setenv("PATH", dir)
	b := newBench(t)
	src := filepath.Join(b.home, "x.plist")
	if err := os.WriteFile(src, []byte("<plist version=\"1.0\"><dict/></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, err := b.runErr("text.plist_json", map[string]any{"input": src, "direction": "plist2json"})
	if err == nil || code != 500 {
		t.Fatalf("应 500，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "假 plutil") {
		t.Errorf("失败原因应带真实 stderr，实际：%v", err)
	}
}

// ---------- text.hash_file ----------

func TestHashFileRealCrossChecked(t *testing.T) {
	if _, ok := execx.LookPath("shasum"); !ok {
		t.Skip("本机没有 shasum，跳过")
	}
	b := newBench(t)
	f := filepath.Join(b.home, "abc.bin")
	if err := os.WriteFile(f, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s256 := sha256.Sum256([]byte("abc"))
	res, _ := b.run("text.hash_file", map[string]any{"file": f, "algo": "sha256"})
	d := res.Data.(map[string]any)
	if d["hash"] != hex.EncodeToString(s256[:]) {
		t.Fatalf("sha256 = %v", d["hash"])
	}
	// 多算法版一次给三种，且与标准库一致。
	res2, _ := b.run("text.hash_file", map[string]any{"file": f, "algo": "all"})
	hashes := res2.Data.(map[string]any)["hashes"].(map[string]string)
	s1 := sha1.Sum([]byte("abc"))
	s512 := sha512.Sum512([]byte("abc"))
	if hashes["sha1"] != hex.EncodeToString(s1[:]) {
		t.Errorf("sha1 = %v", hashes["sha1"])
	}
	if hashes["sha512"] != hex.EncodeToString(s512[:]) {
		t.Errorf("sha512 = %v", hashes["sha512"])
	}
	if hashes["sha256"] != hex.EncodeToString(s256[:]) {
		t.Errorf("sha256 = %v", hashes["sha256"])
	}
}

func TestHashFileShimFailure(t *testing.T) {
	dir := shimDir(t, map[string]string{"shasum": "#!/bin/sh\necho \"假 shasum：读不了\" >&2\nexit 1\n"})
	t.Setenv("PATH", dir)
	b := newBench(t)
	f := filepath.Join(b.home, "x.bin")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, err := b.runErr("text.hash_file", map[string]any{"file": f, "algo": "sha256"})
	if err == nil || code != 500 {
		t.Fatalf("应 500，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "假 shasum") {
		t.Errorf("失败原因应带真实 stderr，实际：%v", err)
	}
}

// ---------- text.compare ----------

func TestCompareReal(t *testing.T) {
	if _, ok := execx.LookPath("diff"); !ok {
		t.Skip("本机没有 diff，跳过")
	}
	b := newBench(t)
	same := filepath.Join(b.home, "same.txt")
	if err := os.WriteFile(same, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := b.run("text.compare", map[string]any{"left": same, "right": same})
	if d := res.Data.(map[string]any); d["identical"] != true {
		t.Fatalf("相同文件应判 identical：%v", d)
	}

	small := filepath.Join(b.home, "small-a.txt")
	small2 := filepath.Join(b.home, "small-b.txt")
	if err := os.WriteFile(small, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small2, []byte("a\nX\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, _ := b.run("text.compare", map[string]any{"left": small, "right": small2})
	d2 := res2.Data.(map[string]any)
	if d2["identical"] != false {
		t.Fatal("不同文件不该判相同")
	}
	if d2["added"] != 1 || d2["removed"] != 1 {
		t.Errorf("增删计数不对：+%v -%v", d2["added"], d2["removed"])
	}
	if !strings.Contains(d2["preview"].(string), "-b") || !strings.Contains(d2["preview"].(string), "+X") {
		t.Errorf("preview 应含差异行：%q", d2["preview"])
	}

	// 大差异 + max_lines=5：只回前 5 行，且计数仍覆盖全集。
	left := filepath.Join(b.home, "left.txt")
	right := filepath.Join(b.home, "right.txt")
	var l, r strings.Builder
	for i := 0; i < 50; i++ {
		l.WriteString("a\n")
		r.WriteString("b\n")
	}
	if err := os.WriteFile(left, []byte(l.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(right, []byte(r.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res3, _ := b.run("text.compare", map[string]any{"left": left, "right": right, "max_lines": 5})
	d3 := res3.Data.(map[string]any)
	if d3["added"] != 50 || d3["removed"] != 50 {
		t.Errorf("增删计数不对：+%v -%v", d3["added"], d3["removed"])
	}
	if d3["shown_lines"] != 5 || d3["truncated"] != true {
		t.Errorf("应按 max_lines 截断：shown=%v truncated=%v", d3["shown_lines"], d3["truncated"])
	}
}

func TestCompareShimFailure(t *testing.T) {
	dir := shimDir(t, map[string]string{"diff": "#!/bin/sh\necho \"假 diff：无法比较\" >&2\nexit 2\n"})
	t.Setenv("PATH", dir)
	b := newBench(t)
	a := filepath.Join(b.home, "a.txt")
	if err := os.WriteFile(a, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, err := b.runErr("text.compare", map[string]any{"left": a, "right": a})
	if err == nil || code != 500 {
		t.Fatalf("diff 退出码 2 应报错，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "假 diff") {
		t.Errorf("失败原因应带真实 stderr，实际：%v", err)
	}
}

// ---------- 文本类工具的命令缺失分支 ----------

func TestTextToolsUnavailableWithoutCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	b := newBench(t)
	for id, bin := range map[string]string{
		"text.doc_convert":  "textutil",
		"text.encoding_fix": "iconv",
		"text.plist_json":   "plutil",
		"text.hash_file":    "shasum",
		"text.compare":      "diff",
	} {
		m := metaOf(t, b.reg, id)
		if m.Available {
			t.Errorf("%s：PATH 无 %s 时不该报告可用", id, bin)
		}
		if !strings.Contains(m.UnavailableReason, bin) {
			t.Errorf("%s：不可用原因应点出 %s，实际 %q", id, bin, m.UnavailableReason)
		}
	}
}

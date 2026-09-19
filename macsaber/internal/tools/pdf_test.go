package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/macsaber/internal/native"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
)

// requireNativeBridge 在桥不可用时打印真实原因并跳过（不伪装通过）。
func requireNativeBridge(t *testing.T, b *bench) native.Probe {
	t.Helper()
	p := native.NewCtx(b.ctx.Exec, b.ctx.Probes, b.ctx.TempDir).Probe(context.Background())
	if !p.Available {
		t.Skipf("原生框架桥不可用，跳过：%s", p.Reason)
	}
	return p
}

// ---------- 元数据纪律 ----------

func TestPDFToolsRegisteredAndHonest(t *testing.T) {
	reg := registryForTest(t)
	ids := []string{"pdf.merge", "pdf.split", "pdf.text", "pdf.info", "pdf.encrypt"}
	for _, id := range ids {
		m := metaOf(t, reg, id)
		if m.Category != "pdf" {
			t.Errorf("%s: 分类应为 pdf，实际 %q", id, m.Category)
		}
		if tool.CategoryTitles[m.Category] == "" {
			t.Errorf("%s: 分类 %q 没有登记中文名", id, m.Category)
		}
		if len([]rune(m.Summary)) > 40 {
			t.Errorf("%s: summary 超过 40 字：%q", id, m.Summary)
		}
		if !m.Available && m.UnavailableReason == "" {
			t.Errorf("%s: 不可用必须给真实原因", id)
		}
	}
	for _, id := range []string{"pdf.merge", "pdf.split", "pdf.text", "pdf.encrypt"} {
		if m := metaOf(t, reg, id); !m.Async {
			t.Errorf("%s: 耗时动作必须异步", id)
		}
	}
	// 加密是危险打样：必须给确认口径，且要有更严的手输文件名校验。
	enc := metaOf(t, reg, "pdf.encrypt")
	if !enc.Danger || enc.DangerFloor == "" {
		t.Fatalf("pdf.encrypt 必须标 danger 且有确认口径：%+v", enc)
	}
	d, _ := reg.Get("pdf.encrypt")
	if _, ok := d.(tool.DangerStricter); !ok {
		t.Error("pdf.encrypt 必须实现 ConfirmOK（手输输出文件名）")
	}
	// 危险工具的参数里必须有输出路径，绝不能是源文件。
	hasOut := false
	for _, p := range enc.Params {
		if p.Name == "output" && p.Type == tool.TypeOutPath {
			hasOut = true
		}
	}
	if !hasOut {
		t.Error("pdf.encrypt 必须有 outpath 参数")
	}
}

// 首屏判据只看命令在不在；昂贵探测留给 SlowProbe。
func TestPDFToolsUseSlowProbe(t *testing.T) {
	reg := registryForTest(t)
	for _, id := range []string{"pdf.merge", "pdf.split", "pdf.text", "pdf.info", "pdf.encrypt", "ocr.image", "ocr.qrcode", "ocr.deskew"} {
		d, ok := reg.Get(id)
		if !ok {
			t.Fatalf("%s 未注册", id)
		}
		if _, ok := d.(tool.SlowProbe); !ok {
			t.Errorf("%s: 原生能力必须实现 SlowProbe（列表阶段不做昂贵探测）", id)
		}
	}
}

func TestPDFUnavailableWithoutCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	reg := registryForTest(t)
	for _, id := range []string{"pdf.merge", "pdf.split", "pdf.text", "pdf.info", "pdf.encrypt", "ocr.image", "ocr.qrcode", "ocr.deskew"} {
		m := metaOf(t, reg, id)
		if m.Available {
			t.Errorf("%s: 空 PATH 下不该可用", id)
			continue
		}
		if !strings.Contains(m.UnavailableReason, "osascript") {
			t.Errorf("%s: 原因应点出缺 osascript，实际 %q", id, m.UnavailableReason)
		}
	}
}

// ---------- 多行路径：逐行过闸门 ----------

// TestPDFMergeRejectsOutOfRootLine：多行里只要有一行越界，整个工具必须失败。
func TestPDFMergeRejectsOutOfRootLine(t *testing.T) {
	b := newBench(t)
	inside := writePDFFixture(t, b.home, "a.pdf", "Alpha one")
	outside := writePDFFixture(t, t.TempDir(), "b.pdf", "Beta two")
	_, snap := b.run("pdf.merge", map[string]any{
		"inputs": inside + "\n" + outside,
		"output": filepath.Join(b.write, "m.pdf"),
	})
	if snap.Status == tasks.Succeeded {
		t.Fatal("读根外的行必须让整个合并失败")
	}
	if !strings.Contains(snap.Error, "第 2 行") {
		t.Errorf("应指明是第几行越界，实际 %q", snap.Error)
	}
}

func TestPDFMergeRequiresTwoInputs(t *testing.T) {
	b := newBench(t)
	a := writePDFFixture(t, b.home, "a.pdf", "Alpha one")
	_, snap := b.run("pdf.merge", map[string]any{"inputs": a})
	if snap.Status == tasks.Succeeded || !strings.Contains(snap.Error, "两个") {
		t.Fatalf("单个输入应被拒，实际 %s（%s）", snap.Status, snap.Error)
	}
}

// ---------- 危险工具的确认契约 ----------

func TestPDFEncryptRequiresHandTypedName(t *testing.T) {
	b := newBench(t)
	src := writePDFFixture(t, b.home, "a.pdf", "Alpha one")
	out := filepath.Join(b.write, "locked.pdf")
	base := map[string]any{"input": src, "output": out, "password": "open-sesame"}

	// 缺确认 → 400，且不产生任何文件。
	if code, err := b.runErr("pdf.encrypt", base); err == nil || code != 400 {
		t.Fatalf("缺确认应 400，实际 code=%d err=%v", code, err)
	}
	// 只勾确认、手输名字错 → 仍然拒绝。
	bad := map[string]any{"input": src, "output": out, "password": "open-sesame",
		"confirm_name": "wrong.pdf", "_confirm": ConfirmText}
	if code, err := b.runErr("pdf.encrypt", bad); err == nil || code != 400 {
		t.Fatalf("名字不对应 400，实际 code=%d err=%v", code, err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("被拒绝的请求不该产生文件")
	}
	// 口令太短要如实拒绝（异步工具的错误在任务快照里）。
	short := map[string]any{"input": src, "output": out, "password": "ab",
		"confirm_name": "locked.pdf", "_confirm": ConfirmText}
	_, snap := b.run("pdf.encrypt", short)
	if snap.Status == tasks.Succeeded || !strings.Contains(snap.Error, "4 位") {
		t.Fatalf("口令太短应失败并说明原因，实际 %s（%s）", snap.Status, snap.Error)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("口令太短的请求不该产生文件")
	}
}

// ---------- 真实 PDFKit（桥不可用就跳过） ----------

func TestPDFSuiteReal(t *testing.T) {
	b := newBench(t)
	requireNativeBridge(t, b)

	a := writePDFFixture(t, b.home, "a.pdf", "Alpha one")
	c := writePDFFixture(t, b.home, "b.pdf", "Beta two")

	// pdf.info（同步）
	res, _ := b.run("pdf.info", map[string]any{"input": a})
	info := res.Data.(map[string]any)
	if info["pages"] != 1 {
		t.Fatalf("应读到 1 页，实际 %v", info["pages"])
	}
	if info["encrypted"] != false {
		t.Fatalf("普通 PDF 不该报加密：%v", info)
	}
	if _, ok := info["page_size"]; !ok {
		t.Error("应给出页尺寸")
	}

	// pdf.text（异步）
	_, snap := b.run("pdf.text", map[string]any{"input": a, "max_chars": 1000,
		"output": filepath.Join(b.write, "a.txt")})
	data := b.resultData(t, snap)
	if !strings.Contains(data["text"].(string), "Alpha one") {
		t.Fatalf("应提取到 Alpha one，实际 %q", data["text"])
	}
	txtPath, _ := data["txt"].(string)
	raw, err := os.ReadFile(txtPath)
	if err != nil || !strings.Contains(string(raw), "Alpha one") {
		t.Fatalf("txt 产物应含正文，实际 %v %q", err, string(raw))
	}

	// pdf.merge（异步）
	merged := filepath.Join(b.write, "merged.pdf")
	_, snap2 := b.run("pdf.merge", map[string]any{"inputs": a + "\n" + c, "output": merged})
	if s := snap2.Status; s != tasks.Succeeded {
		t.Fatalf("合并应成功，实际 %s（%s）", s, snap2.Error)
	}
	res2, _ := b.run("pdf.info", map[string]any{"input": merged})
	if got := res2.Data.(map[string]any)["pages"]; got != 2 {
		t.Fatalf("合并后应有 2 页，实际 %v", got)
	}

	// pdf.split（异步）：必须落在写根的新目录里。
	outDir := filepath.Join(b.write, "split-out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, snap3 := b.run("pdf.split", map[string]any{"input": merged, "output_dir": outDir})
	d3 := b.resultData(t, snap3)
	pages, _ := d3["pages"].(int)
	if pages != 2 {
		t.Fatalf("应拆出 2 页，实际 %v", d3["pages"])
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 2 {
		t.Fatalf("目录里应有 2 个文件，实际 %d", len(entries))
	}
}

func TestPDFEncryptReal(t *testing.T) {
	b := newBench(t)
	requireNativeBridge(t, b)

	src := writePDFFixture(t, b.home, "a.pdf", "Alpha one")
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(b.write, "locked.pdf")
	body := map[string]any{"input": src, "output": out, "password": "open-sesame",
		"confirm_name": "locked.pdf", "_confirm": ConfirmText}
	_, snap := b.run("pdf.encrypt", body)
	data := b.resultData(t, snap)
	if data["encrypted"] != true {
		t.Fatalf("应复核出加密：%v", data)
	}
	// 源文件必须一字未动。
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("源文件被改动了，违反本轮约束")
	}
	// 产物必须真的打不开：再读它的页数应当读不到，并如实说明。
	res, _ := b.run("pdf.info", map[string]any{"input": out})
	info := res.Data.(map[string]any)
	if info["encrypted"] != true {
		t.Fatalf("产物应标记加密：%v", info)
	}
	if notes, ok := info["notes"]; !ok {
		t.Errorf("读不到页数时应给原因，实际 %v", info)
	} else if len(notes.([]string)) == 0 {
		t.Error("notes 不该为空")
	}
}

// writePDFFixture 手写一个极小但合法的 PDF（带文本层），不依赖任何外部工具。
func writePDFFixture(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", strings.NewReplacer(
		"\\", "\\\\", "(", "\\(", ")", "\\)").Replace(text))
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
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("造 PDF 素材 %s 失败: %v", name, err)
	}
	return path
}

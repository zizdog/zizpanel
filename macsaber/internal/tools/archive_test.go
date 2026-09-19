package tools

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
)

// ---------- 测试脚手架 ----------

// pathWithShims 把 PATH 指向一个只含指定 shim 的目录，用来构造缺失/失败分支。
func pathWithShims(t *testing.T, shims map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range shims {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func requireCmds(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, ok := execx.LookPath(n); !ok {
			t.Skipf("本机没有 %s，跳过", n)
		}
	}
}

// printfShim 造"只输出固定行"的 shim 体：PATH 里只有 shim 目录，用不了 cat。
func printfShim(lines ...string) string {
	quoted := make([]string, 0, len(lines))
	for _, ln := range lines {
		quoted = append(quoted, "'"+strings.ReplaceAll(ln, "'", `'\''`)+"'")
	}
	return "printf '%s\\n' " + strings.Join(quoted, " ")
}

// runTask 跑一个工具并返回任务快照（异步失败不 Fatal，交给调用方断言）。
func (b *bench) runTask(id string, body map[string]any) tasks.Snapshot {
	b.t.Helper()
	_, snap := b.run(id, body)
	return snap
}

type zipItem struct {
	name    string
	body    string
	symlink bool
}

func writeZipFile(t *testing.T, path string, items []zipItem) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, it := range items {
		if it.symlink {
			fh := &zip.FileHeader{Name: it.name, Method: zip.Deflate}
			fh.SetMode(os.ModeSymlink | 0o777)
			w, err := zw.CreateHeader(fh)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(it.body)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		w, err := zw.Create(it.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(it.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

type tarItem struct {
	name     string
	body     string
	symlink  bool
	linkname string
}

func writeTarGzFile(t *testing.T, path string, items []tarItem) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, it := range items {
		h := &tar.Header{Name: it.name, Mode: 0o644, Size: int64(len(it.body)), Typeflag: tar.TypeReg}
		if it.symlink {
			h.Typeflag = tar.TypeSymlink
			h.Linkname = it.linkname
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if !it.symlink && it.body != "" {
			if _, err := tw.Write([]byte(it.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// ---------- 元数据 ----------

func TestArchiveToolsRegistered(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAll(reg)
	want := []struct {
		id     string
		cat    string
		async  bool
		reason string
	}{
		{"archive.list", "file", false, "tar/unzip"},
		{"archive.create", "file", true, "ditto/tar"},
		{"archive.extract", "file", true, "ditto/tar"},
		{"xattr.list", "file", false, "xattr"},
	}
	for _, w := range want {
		m := metaOf(t, reg, w.id)
		if m.Category != w.cat {
			t.Errorf("%s 分类应为 %s，实际 %s", w.id, w.cat, m.Category)
		}
		if m.Async != w.async {
			t.Errorf("%s async 应为 %v", w.id, w.async)
		}
		if len([]rune(m.Summary)) > 40 {
			t.Errorf("%s summary 超过 40 字：%q", w.id, m.Summary)
		}
	}
}

func TestArchiveUnavailableWhenCommandsMissing(t *testing.T) {
	pathWithShims(t, nil) // PATH 指向空目录：所有外部命令都算缺失
	reg := tool.NewRegistry()
	RegisterAll(reg)
	for _, id := range []string{"archive.create", "archive.extract", "archive.list", "xattr.list"} {
		m := metaOf(t, reg, id)
		if m.Available {
			t.Errorf("%s 在无命令环境应不可用", id)
			continue
		}
		if strings.TrimSpace(m.UnavailableReason) == "" {
			t.Errorf("%s 不可用必须给人话原因", id)
		}
	}
	if r := metaOf(t, reg, "archive.create").UnavailableReason; !strings.Contains(r, "ditto") {
		t.Errorf("原因应点出缺 ditto，实际 %q", r)
	}
	if r := metaOf(t, reg, "xattr.list").UnavailableReason; !strings.Contains(r, "xattr") {
		t.Errorf("原因应点出缺 xattr，实际 %q", r)
	}
}

// ---------- 打包含义：真造恶意归档 ----------

func TestUnsafeEntryName(t *testing.T) {
	cases := []struct {
		name string
		bad  bool
	}{
		{"a/b.txt", false},
		{"../evil.txt", true},
		{"a/../../evil.txt", true},
		{"/abs.txt", true},
		{"C:/evil.txt", true},
		{`C:\evil.txt`, true},
		{"a:b.txt", false},  // 冒号在 macOS 文件名里合法
		{"a\\..\\b", false}, // 反斜杠在 macOS 只是普通字符
		{"", true},
	}
	for _, c := range cases {
		got := unsafeEntryName(c.name) != ""
		if got != c.bad {
			t.Errorf("unsafeEntryName(%q) 拒绝=%v，期望 %v", c.name, got, c.bad)
		}
	}
}

// TestArchiveExtractRejectsZipSlip 真造含 ../evil.txt 的 zip，断言整体被拒且目标外无产物。
func TestArchiveExtractRejectsZipSlip(t *testing.T) {
	requireCmds(t, "ditto", "tar")
	b := newBench(t)
	evil := filepath.Join(b.write, "slip.zip")
	writeZipFile(t, evil, []zipItem{
		{name: "../evil.txt", body: "pwned"},
		{name: "../../evil2.txt", body: "pwned"},
		{name: "ok.txt", body: "fine"},
	})
	// 复核：zip 里真的存在越界条目名（不是测试写歪了）。
	zr, err := zip.OpenReader(evil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	_ = zr.Close()
	if strings.Join(names, ",") != "../evil.txt,../../evil2.txt,ok.txt" {
		t.Fatalf("测试归档条目不符: %v", names)
	}

	target := filepath.Join(b.write, "slip-out")
	snap := b.runTask("archive.extract", map[string]any{"input": evil, "outpath": target})
	if snap.Status != tasks.Failed {
		t.Fatalf("含 ../ 的 zip 必须被拒，实际状态 %s（%s）", snap.Status, snap.Error)
	}
	if !strings.Contains(snap.Error, "../evil.txt") {
		t.Errorf("错误里要指出是哪个条目，实际 %q", snap.Error)
	}
	for _, p := range []string{
		filepath.Join(target, "evil.txt"),
		filepath.Join(b.write, "evil.txt"),
		filepath.Join(b.home, "evil2.txt"),
	} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("拒绝后不该产生文件：%s", p)
		}
	}
	if _, err := os.Lstat(target); err == nil {
		t.Errorf("校验阶段就该拒绝，不该创建目标目录 %s", target)
	}
}

func TestArchiveExtractRejectsSymlinkAndAbsolute(t *testing.T) {
	requireCmds(t, "ditto", "tar")
	cases := []struct {
		name     string
		items    []zipItem
		wantWord string
	}{
		{"symlink", []zipItem{{name: "link", body: "/etc/passwd", symlink: true}}, "符号链接"},
		{"absolute", []zipItem{{name: "/tmp/ms-abs-evil.txt", body: "x"}}, "绝对路径"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t)
			z := filepath.Join(b.write, "bad.zip")
			writeZipFile(t, z, c.items)
			target := filepath.Join(b.write, "out-"+c.name)
			snap := b.runTask("archive.extract", map[string]any{"input": z, "outpath": target})
			if snap.Status != tasks.Failed {
				t.Fatalf("应被拒，实际 %s", snap.Status)
			}
			if !strings.Contains(snap.Error, c.wantWord) {
				t.Errorf("错误应说明原因 %q，实际 %q", c.wantWord, snap.Error)
			}
			if _, err := os.Lstat(target); err == nil {
				t.Errorf("不该创建目标目录 %s", target)
			}
		})
	}
}

func TestArchiveExtractRejectsTarTraversal(t *testing.T) {
	requireCmds(t, "ditto", "tar")
	b := newBench(t)
	bad := filepath.Join(b.write, "bad.tgz")
	writeTarGzFile(t, bad, []tarItem{
		{name: "../evil.txt", body: "pwned"},
		{name: "link", symlink: true, linkname: "/etc/passwd"},
	})
	target := filepath.Join(b.write, "tar-out")
	snap := b.runTask("archive.extract", map[string]any{"input": bad, "outpath": target})
	if snap.Status != tasks.Failed || !strings.Contains(snap.Error, "../evil.txt") {
		t.Fatalf("tgz 越界条目必须被拒并点名，实际 %s / %q", snap.Status, snap.Error)
	}
	if _, err := os.Lstat(filepath.Join(b.write, "evil.txt")); err == nil {
		t.Fatal("拒绝了却仍然产生了文件")
	}
}

// ---------- 打包/解压往返（真实 ditto 与 tar）----------

func TestArchiveRoundTrip(t *testing.T) {
	requireCmds(t, "ditto", "tar", "unzip")
	for _, format := range []string{"zip", "tgz"} {
		t.Run(format, func(t *testing.T) {
			b := newBench(t)
			src := filepath.Join(b.home, "pkg")
			if err := os.MkdirAll(filepath.Join(src, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o600); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(b.write, "pkg-"+format)
			snap := b.runTask("archive.create", map[string]any{
				"input": src, "format": format, "outpath": out})
			if snap.Status != tasks.Succeeded {
				t.Fatalf("打包失败: %s", snap.Error)
			}
			got := snap.Result.(*tool.Result).Data.(map[string]any)["output"].(string)
			wantExt := ".zip"
			if format == "tgz" {
				wantExt = ".tgz"
			}
			if !strings.HasSuffix(got, wantExt) {
				t.Fatalf("输出应自动补后缀 %s，实际 %s", wantExt, got)
			}
			if st, err := os.Stat(got); err != nil || st.Size() == 0 {
				t.Fatalf("产物缺失或 0 字节: %v", err)
			}

			res, _ := b.run("archive.list", map[string]any{"path": got, "limit": 50})
			ld := res.Data.(map[string]any)
			if ld["count"].(int) < 3 {
				t.Errorf("列表条目数偏少: %v", ld["count"])
			}
			joined := fmt.Sprint(ld["items"])
			if !strings.Contains(joined, "pkg/a.txt") {
				t.Errorf("列表应含 pkg/a.txt，实际 %s", joined)
			}

			target := filepath.Join(b.write, "unpack-"+format)
			snap2 := b.runTask("archive.extract", map[string]any{"input": got, "outpath": target})
			if snap2.Status != tasks.Succeeded {
				t.Fatalf("解压失败: %s", snap2.Error)
			}
			data := b.resultData(t, snap2)
			if data["files"].(int) < 2 {
				t.Errorf("解压文件数偏少: %v", data["files"])
			}
			for p, want := range map[string]string{
				filepath.Join(target, "pkg", "a.txt"):        "hello",
				filepath.Join(target, "pkg", "sub", "b.txt"): "world",
			} {
				blob, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("解压产物缺失 %s: %v", p, err)
				}
				if string(blob) != want {
					t.Errorf("%s 内容 %q，期望 %q", p, blob, want)
				}
			}
		})
	}
}

func TestArchiveCreateRefusesOverwriteAndBadTarget(t *testing.T) {
	requireCmds(t, "ditto", "tar")
	b := newBench(t)
	src := filepath.Join(b.home, "one.txt")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(b.write, "one.zip")
	if err := os.WriteFile(out, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := b.runTask("archive.create", map[string]any{"input": src, "format": "zip", "outpath": out})
	if snap.Status != tasks.Failed || !strings.Contains(snap.Error, "已存在") {
		t.Fatalf("默认不该覆盖已有文件，实际 %s / %q", snap.Status, snap.Error)
	}
	// 勾选覆盖后应成功。
	snap2 := b.runTask("archive.create", map[string]any{
		"input": src, "format": "zip", "outpath": out, "overwrite": true})
	if snap2.Status != tasks.Succeeded {
		t.Fatalf("勾选覆盖后应成功: %s", snap2.Error)
	}
	// 输出当成目录用 → 明确报错。
	dirOut := filepath.Join(b.write, "adir")
	if err := os.MkdirAll(dirOut, 0o700); err != nil {
		t.Fatal(err)
	}
	snap3 := b.runTask("archive.create", map[string]any{
		"input": src, "format": "zip", "outpath": dirOut, "overwrite": true})
	if snap3.Status != tasks.Failed || !strings.Contains(snap3.Error, "目录") {
		t.Fatalf("输出是目录应报错，实际 %s / %q", snap3.Status, snap3.Error)
	}
}

func TestArchiveExtractRefusesNonEmptyDir(t *testing.T) {
	requireCmds(t, "ditto", "tar")
	b := newBench(t)
	z := filepath.Join(b.write, "ok.zip")
	writeZipFile(t, z, []zipItem{{name: "a.txt", body: "hi"}})
	target := filepath.Join(b.write, "occupied")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := b.runTask("archive.extract", map[string]any{"input": z, "outpath": target})
	if snap.Status != tasks.Failed || !strings.Contains(snap.Error, "非空") {
		t.Fatalf("非空目标必须拒绝，实际 %s / %q", snap.Status, snap.Error)
	}
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Fatal("拒绝时不该动用户已有文件")
	}
}

// ---------- 命令缺失/失败分支 ----------

func TestArchiveCreateUnavailable503(t *testing.T) {
	pathWithShims(t, nil)
	b := newBench(t)
	code, err := b.runErr("archive.create", map[string]any{
		"input": b.home, "format": "zip", "outpath": filepath.Join(b.write, "x.zip")})
	if err == nil || code != 503 {
		t.Fatalf("命令缺失应 503，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "ditto") {
		t.Errorf("原因应点出缺 ditto，实际 %v", err)
	}
}

func TestArchiveListSurfacesRealStderr(t *testing.T) {
	pathWithShims(t, map[string]string{
		"unzip": "echo 'boom: bad zip' >&2\nexit 1",
		"tar":   "exit 0",
	})
	b := newBench(t)
	z := filepath.Join(b.home, "x.zip")
	writeZipFile(t, z, []zipItem{{name: "a.txt", body: "hi"}})
	code, err := b.runErr("archive.list", map[string]any{"path": z})
	if err == nil || code != 500 {
		t.Fatalf("unzip 失败应 500，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "boom: bad zip") {
		t.Errorf("应回显真实 stderr 关键行，实际 %v", err)
	}
}

func TestArchiveExtractFailureLeavesCleanTarget(t *testing.T) {
	pathWithShims(t, map[string]string{
		"ditto": "echo 'ditto: cannot extract' >&2\nexit 1",
		"tar":   "exit 0",
	})
	b := newBench(t)
	z := filepath.Join(b.write, "ok.zip")
	writeZipFile(t, z, []zipItem{{name: "a.txt", body: "hi"}})
	target := filepath.Join(b.write, "failed-out")
	snap := b.runTask("archive.extract", map[string]any{"input": z, "outpath": target})
	if snap.Status != tasks.Failed || !strings.Contains(snap.Error, "cannot extract") {
		t.Fatalf("解压失败要如实上报，实际 %s / %q", snap.Status, snap.Error)
	}
	ents, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("目标目录应存在但为空: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("失败后不该留下临时产物: %v", ents)
	}
}

// ---------- 解析函数 ----------

func TestParseUnzipList(t *testing.T) {
	fixture := `Archive:  /tmp/out.zip
  Length      Date    Time    Name
---------  ---------- -----   ----
        0  09-20-2026 01:47   src/
        6  09-20-2026 01:47   src/a.txt
      163  09-20-2026 01:47   src/._a.txt
---------                     -------
      169                     3 files
`
	rows := parseUnzipList(fixture)
	if len(rows) != 3 {
		t.Fatalf("应解析 3 条，实际 %d: %+v", len(rows), rows)
	}
	if rows[1].Name != "src/a.txt" || rows[1].Size != 6 {
		t.Errorf("第 2 条解析错: %+v", rows[1])
	}
	if !rows[0].Dir || rows[1].Dir {
		t.Errorf("目录标记错: %+v", rows)
	}
}

func TestParseXattrList(t *testing.T) {
	out := "com.apple.provenance: \ncom.apple.quarantine: 0081;0;Safari;\n"
	attrs, q := parseXattrList(out)
	if len(attrs) != 2 {
		t.Fatalf("应解析 2 条，实际 %d", len(attrs))
	}
	if q == nil || q["agent"] != "Safari" {
		t.Fatalf("隔离标记应解析出写入程序，实际 %+v", q)
	}
}

func TestXattrListReadOnly(t *testing.T) {
	requireCmds(t, "xattr")
	b := newBench(t)
	f := filepath.Join(b.home, "plain.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := b.run("xattr.list", map[string]any{"path": f})
	if res.OK != true {
		t.Fatal("xattr.list 应成功")
	}
	if _, ok := res.Data.(map[string]any)["attributes"]; !ok {
		t.Error("结果应含 attributes")
	}
}

package tools

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
)

// ---------- 脚手架 ----------

type bench struct {
	t      *testing.T
	reg    *tool.Registry
	runner *tool.Runner
	tasks  *tasks.Manager
	ctx    *tool.Ctx
	home   string
	write  string
}

func newBench(t *testing.T) *bench {
	t.Helper()
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	writeRoot := filepath.Join(home, "MacSaberFiles")
	if err := os.MkdirAll(writeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	g, err := fsroot.New([]string{home}, []string{writeRoot})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := tool.OpenAudit(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	c := &tool.Ctx{
		Ctx: context.Background(), Guard: g, Exec: execx.New(),
		Probes: execx.NewProbeCache(), TempDir: t.TempDir(),
		Log: func(string) {}, Progress: func(int, string) {},
	}
	tm := tasks.NewManager()
	reg := tool.NewRegistry()
	RegisterAll(reg)
	return &bench{t: t, reg: reg, tasks: tm, ctx: c, home: home, write: writeRoot,
		runner: &tool.Runner{Reg: reg, Ctx: c, Tasks: tm, Audit: audit}}
}

// run 走真正的 Runner（校验 + 审计 + 执行）；异步工具返回任务快照。
func (b *bench) run(id string, body map[string]any) (*tool.Result, tasks.Snapshot) {
	b.t.Helper()
	resp, code, err := b.runner.Run(context.Background(), "tester", id, body)
	if err != nil {
		b.t.Fatalf("%s 执行失败（code=%d）: %v", id, code, err)
	}
	if resp.Accepted {
		tk, ok := b.tasks.Get(resp.TaskID)
		if !ok {
			b.t.Fatalf("任务 %s 不存在", resp.TaskID)
		}
		select {
		case <-tk.Done():
		case <-time.After(60 * time.Second):
			b.t.Fatalf("任务 %s 超时", resp.TaskID)
		}
		return nil, tk.Snapshot()
	}
	return resp.Result, tasks.Snapshot{}
}

func (b *bench) runErr(id string, body map[string]any) (int, error) {
	b.t.Helper()
	_, code, err := b.runner.Run(context.Background(), "tester", id, body)
	return code, err
}

func (b *bench) resultData(t *testing.T, snap tasks.Snapshot) map[string]any {
	t.Helper()
	if snap.Status != tasks.Succeeded {
		t.Fatalf("任务应成功，实际 %s（%s）", snap.Status, snap.Error)
	}
	res, ok := snap.Result.(*tool.Result)
	if !ok || res == nil {
		t.Fatalf("任务结果类型不符: %#v", snap.Result)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("结果 data 类型不符: %#v", res.Data)
	}
	return data
}

// ---------- 元数据完整性 ----------

// TestRegisteredToolsMetaIntegrity：真实工具包的元数据必须自洽（注册本身会 panic）。
func TestRegisteredToolsMetaIntegrity(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAll(reg)
	cats := reg.List()
	if len(cats) == 0 {
		t.Fatal("分类为空")
	}
	ids := map[string]bool{}
	for _, c := range cats {
		if c.Title == "" {
			t.Errorf("分类 %s 缺标题", c.ID)
		}
		for _, m := range c.Tools {
			if ids[m.ID] {
				t.Fatalf("工具 id 重复: %s", m.ID)
			}
			ids[m.ID] = true
			if err := tool.ValidateMeta(m); err != nil {
				t.Errorf("%s: %v", m.ID, err)
			}
			if m.Available && m.UnavailableReason != "" {
				t.Errorf("%s: available=true 不该带原因", m.ID)
			}
			if !m.Available && m.UnavailableReason == "" {
				t.Errorf("%s: 不可用必须给真实原因", m.ID)
			}
			if m.Danger && m.DangerFloor == "" {
				t.Errorf("%s: 危险工具必须给确认口径", m.ID)
			}
		}
	}
	for _, want := range []string{"img.convert", "text.hash", "sys.overview", "sec.dequarantine"} {
		if !ids[want] {
			t.Errorf("示例工具 %s 未注册", want)
		}
	}
	// 三个必做示例的形态契约。
	if m := metaOf(t, reg, "img.convert"); !m.Async || m.Danger {
		t.Errorf("img.convert 应为异步且非危险: async=%v danger=%v", m.Async, m.Danger)
	}
	if m := metaOf(t, reg, "text.hash"); m.Async || m.Danger {
		t.Errorf("text.hash 应为同步且非危险: async=%v danger=%v", m.Async, m.Danger)
	}
	if m := metaOf(t, reg, "sys.overview"); m.Async || m.Danger {
		t.Errorf("sys.overview 应为同步且非危险: async=%v danger=%v", m.Async, m.Danger)
	}
	if m := metaOf(t, reg, "sec.dequarantine"); !m.Danger || m.DangerFloor == "" {
		t.Error("sec.dequarantine 必须标 danger 且有确认口径")
	}
}

// TestAvailabilityIsTruthful：available 必须与真实探测一致（禁止猜/谎报）。
func TestAvailabilityIsTruthful(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAll(reg)
	want := map[string]string{
		"img.convert":      "sips",
		"sys.overview":     "sysctl",
		"sec.dequarantine": "xattr",
	}
	for id, bin := range want {
		m := metaOf(t, reg, id)
		_, found := execx.LookPath(bin)
		if found != m.Available {
			t.Errorf("%s: 元数据 available=%v，但 %s 存在=%v", id, m.Available, bin, found)
		}
		if !m.Available && !strings.Contains(m.UnavailableReason, bin) {
			t.Errorf("%s: 不可用原因应点出缺哪个命令，实际 %q", id, m.UnavailableReason)
		}
	}
	// 纯 Go 工具必须恒为可用（不依赖外部命令）。
	if m := metaOf(t, reg, "text.hash"); !m.Available {
		t.Error("text.hash 是纯 Go 实现，不该依赖外部命令")
	}
}

func metaOf(t *testing.T, reg *tool.Registry, id string) tool.Meta {
	t.Helper()
	d, ok := reg.Get(id)
	if !ok {
		t.Fatalf("工具 %s 不存在", id)
	}
	return d.Meta()
}

// ---------- text.hash（同步、纯 Go）----------

func TestTextHashTextAndFile(t *testing.T) {
	b := newBench(t)
	// 已知向量：sha256("abc")。
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

	res, _ := b.run("text.hash", map[string]any{"algo": "sha256", "text": "abc"})
	got := res.Data.(map[string]any)["text"].(map[string]any)["hash"]
	if got != want {
		t.Fatalf("sha256(abc) = %v，期望 %s", got, want)
	}

	f := filepath.Join(b.home, "abc.txt")
	if err := os.WriteFile(f, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, _ := b.run("text.hash", map[string]any{"algo": "sha256", "file": f})
	fh := res2.Data.(map[string]any)["file"].(map[string]any)["hash"]
	if fh != want {
		t.Fatalf("文件 sha256 = %v，期望 %s", fh, want)
	}

	// 既无文本也无文件 → 必须报错（不许谎报成功）。
	if _, err := b.runErr("text.hash", map[string]any{"algo": "sha256"}); err == nil {
		t.Fatal("既无文本也无文件时应报错")
	}
	// 读根之外的文件被闸门拦住。
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := b.runErr("text.hash", map[string]any{"algo": "sha256", "file": outside}); err == nil || code != 403 {
		t.Fatalf("读根外文件应 403，实际 code=%d err=%v", code, err)
	}
}

// ---------- img.convert（异步、真实调 sips）----------

func TestImgConvertRealSips(t *testing.T) {
	if _, ok := execx.LookPath("sips"); !ok {
		t.Skip("本机没有 sips，跳过")
	}
	b := newBench(t)
	src := filepath.Join(b.home, "tiny.png")
	if err := os.WriteFile(src, tinyPNG(), 0o644); err != nil {
		t.Fatal(err)
	}

	// 异步路径：立刻 202 + task_id，结果走任务中心。
	resp, code, err := b.runner.Run(context.Background(), "tester", "img.convert", map[string]any{
		"input": src, "format": "jpeg", "max_edge": 8,
		"output": filepath.Join(b.write, "tiny.jpg"),
	})
	if err != nil || code != 202 {
		t.Fatalf("img.convert 应走异步 202，实际 code=%d err=%v", code, err)
	}
	tk, _ := b.tasks.Get(resp.TaskID)
	select {
	case <-tk.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("转换任务超时")
	}
	snap := tk.Snapshot()
	data := b.resultData(t, snap)
	out, _ := data["output"].(string)
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("产物是 0 字节")
	}
	if !strings.HasPrefix(out, b.write) {
		t.Fatalf("产物应落在写根内，实际 %s", out)
	}
	res := snap.Result.(*tool.Result)
	if len(res.Files) != 1 || !strings.Contains(res.Files[0].DownloadURL, "/api/files/download?path=") {
		t.Fatalf("产物应带下载链接，实际 %+v", res.Files)
	}

	// 输出留空 → 自动落到第一个写根且不覆盖已有文件。
	src2 := filepath.Join(b.home, "tiny2.png")
	if err := os.WriteFile(src2, tinyPNG(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap2 := b.run("img.convert", map[string]any{"input": src2, "format": "png"})
	d2 := b.resultData(t, snap2)
	if got, _ := d2["output"].(string); !strings.HasPrefix(got, b.write) {
		t.Fatalf("自动输出应在写根内，实际 %s", got)
	}

	// 读根外输入必须被拒（并且不产生任何任务）。
	if code, err := b.runErr("img.convert", map[string]any{
		"input": filepath.Join(t.TempDir(), "nope.png"), "format": "png"}); err == nil || code != 403 {
		t.Fatalf("读根外输入应 403，实际 code=%d err=%v", code, err)
	}
}

// ---------- sys.overview（同步、只读解析）----------

func TestSysOverviewReadOnly(t *testing.T) {
	b := newBench(t)
	res, _ := b.run("sys.overview", nil)
	data := res.Data.(map[string]any)
	if _, ok := data["paths"]; !ok {
		t.Error("应报告读写根")
	}
	if problems, ok := data["unavailable"]; ok {
		t.Logf("本机部分采集不可用（如实上报）: %v", problems)
	}
	if _, ok := data["hardware"]; !ok {
		t.Error("应采到硬件信息")
	}
	if _, ok := data["disks"]; !ok {
		t.Error("应采到磁盘信息")
	}
	// 内存项应解析出人类可读值。
	if mem, ok := data["memory"].(map[string]any); ok {
		if mem["free_human"] == "" {
			t.Error("内存 free_human 不该为空")
		}
	}
}

// ---------- sec.dequarantine（危险打样）----------

func TestDangerToolRequiresConfirmAndDryRun(t *testing.T) {
	b := newBench(t)
	f := filepath.Join(b.home, "quarantined.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 缺确认 → 400。
	if code, err := b.runErr("sec.dequarantine", map[string]any{"file": f, "dry_run": false}); err == nil || code != 400 {
		t.Fatalf("缺确认应 400，实际 code=%d err=%v", code, err)
	}
	// 有确认但 dry_run → 只读。
	res, _ := b.run("sec.dequarantine", map[string]any{"file": f, "dry_run": true, "_confirm": ConfirmText})
	if changed := res.Data.(map[string]any)["changed"]; changed != false {
		t.Fatalf("dry_run 不该改文件，实际 changed=%v", changed)
	}
}

func TestDangerToolRealXattr(t *testing.T) {
	if _, ok := execx.LookPath("xattr"); !ok {
		t.Skip("本机没有 xattr，跳过")
	}
	b := newBench(t)
	f := filepath.Join(b.home, "quarantined.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := execx.New().Run(context.Background(), 10*time.Second,
		"xattr", "-w", "com.apple.quarantine", "0081;0;Safari;", f)
	if out.ExitCode != 0 {
		t.Skipf("本机不允许设置扩展属性，跳过真实去隔离: %s", out.Stderr)
	}
	res, _ := b.run("sec.dequarantine", map[string]any{
		"file": f, "dry_run": false, "_confirm": ConfirmText})
	data := res.Data.(map[string]any)
	if data["present"] != false || data["changed"] != true {
		t.Fatalf("去隔离结果不符: %v", data)
	}
	// 复核：属性真的没了（独立于工具自报）。
	again := execx.New().Run(context.Background(), 10*time.Second, "xattr", "-p", "com.apple.quarantine", f)
	if again.ExitCode == 0 {
		t.Fatal("复核发现属性仍在（谎报成功）")
	}
	// 再跑一次：应如实说"没有标记，无需处理"。
	res2, _ := b.run("sec.dequarantine", map[string]any{
		"file": f, "dry_run": false, "_confirm": ConfirmText})
	if changed := res2.Data.(map[string]any)["changed"]; changed != false {
		t.Fatalf("第二次不该报 changed: %v", res2.Data)
	}
}

// tinyPNG 造一个 1x1 的真 PNG（不依赖任何外部工具）。
// 注意：zlib 流必须完整 —— 手写的"存储块 + adler32"少一个字节就会让
// sips 报 exit 13 "Cannot extract image from file"，排查成本很高（坑 F4）。
func tinyPNG() []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	chunk := func(typ string, data []byte) {
		var ln [4]byte
		binary.BigEndian.PutUint32(ln[:], uint32(len(data)))
		buf.Write(ln[:])
		body := append([]byte(typ), data...)
		buf.Write(body)
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(body))
		buf.Write(crc[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 1) // width
	binary.BigEndian.PutUint32(ihdr[4:], 1) // height
	ihdr[8] = 8                             // bit depth
	ihdr[9] = 2                             // truecolor
	chunk("IHDR", ihdr)

	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write([]byte{0x00, 0xff, 0x00, 0x00}); err != nil { // filter 0 + RGB
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return buf.Bytes()
}

package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tasks"
)

// ---------- 测试用工具 ----------

type okTool struct{ ran *int32 }

func (okTool) Meta() Meta {
	return Meta{ID: "demo.ok", Name: "示例", Category: "example", Summary: "只回显参数",
		Available: true,
		Params: []Param{
			{Name: "text", Label: "文本", Type: TypeText},
			{Name: "file", Label: "文件", Type: TypePath, Help: "只能读读根内文件。"},
			{Name: "out", Label: "输出", Type: TypeOutPath, AllowMissing: true, Help: "只能写写根内。"},
		}}
}

func (okTool) Run(ctx context.Context, c *Ctx, in Input) (*Result, error) {
	return &Result{OK: true, Msg: "ok", Data: map[string]any{
		"text": in.Str("text"), "file": in.Path("file"), "out": in.Path("out"),
	}}, nil
}

type asyncTool struct{}

func (asyncTool) Meta() Meta {
	return Meta{ID: "demo.async", Name: "异步示例", Category: "example", Summary: "后台跑",
		Async: true, Available: true,
		Params: []Param{{Name: "text", Label: "文本", Type: TypeText, Required: true}}}
}

func (asyncTool) Run(ctx context.Context, c *Ctx, in Input) (*Result, error) {
	c.Log("后台任务开始")
	if in.Str("text") == "fail" {
		return nil, errors.New("异步失败原因")
	}
	return &Result{OK: true, Msg: "异步完成", Data: map[string]any{"echo": in.Str("text")}}, nil
}

type dangerTool struct{ ran *int32 }

func (d dangerTool) Meta() Meta {
	return Meta{ID: "demo.danger", Name: "危险示例", Category: "example", Summary: "需要确认",
		Danger: true, DangerFloor: "我已知晓", Available: true,
		Params: []Param{{Name: "text", Label: "文本", Type: TypeText}}}
}

func (d dangerTool) DangerFloor() string { return "我已知晓" }

func (d dangerTool) Run(ctx context.Context, c *Ctx, in Input) (*Result, error) {
	*d.ran++
	return &Result{OK: true, Msg: "危险动作已执行（打样）"}, nil
}

// strictDangerTool 演示"更严的确认"：除了勾选，还必须手输目标名字。
type strictDangerTool struct{ ran *int32 }

func (strictDangerTool) Meta() Meta {
	return Meta{ID: "demo.danger.strict", Name: "严格危险示例", Category: "example",
		Danger: true, DangerFloor: "我已知晓", Available: true,
		Params: []Param{{Name: "name", Label: "目标名", Type: TypeText, Required: true}}}
}

func (strictDangerTool) DangerFloor() string { return "我已知晓" }

func (strictDangerTool) ConfirmOK(in Input) error {
	if in.Str("name") != "confirm-me" {
		return errors.New("请把目标名原样输入")
	}
	return nil
}

func (s strictDangerTool) Run(ctx context.Context, c *Ctx, in Input) (*Result, error) {
	*s.ran++
	return &Result{OK: true, Msg: "执行了"}, nil
}

type unavailableTool struct{}

func (unavailableTool) Meta() Meta {
	return Meta{ID: "demo.missing", Name: "缺依赖", Category: "example",
		Available: false, UnavailableReason: "缺少命令 foobar"}
}

func (unavailableTool) Run(ctx context.Context, c *Ctx, in Input) (*Result, error) {
	return &Result{OK: true, Msg: "不该被执行"}, nil
}

func (unavailableTool) Probe(ctx context.Context, c *Ctx) (bool, string) {
	return false, "缺依赖"
}

type probeTool struct {
	mu    sync.Mutex
	calls int
}

func (p *probeTool) Meta() Meta {
	return Meta{ID: "demo.probe", Name: "昂贵探测", Category: "example", Available: true,
		Params: []Param{{Name: "text", Label: "文本", Type: TypeText, Probe: "vision"}}}
}

func (p *probeTool) Probe(ctx context.Context, c *Ctx) (bool, string) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if c.Probes != nil {
		if ok, reason, hit := c.Probes.Get("demo.probe"); hit {
			return ok, reason
		}
		c.Probes.Put("demo.probe", false, "Vision 不可用", 0)
	}
	return false, "Vision 不可用"
}

func (p *probeTool) Run(ctx context.Context, c *Ctx, in Input) (*Result, error) {
	return &Result{OK: true, Msg: "不该被执行"}, nil
}

// ---------- 测试脚手架 ----------

type harness struct {
	t      *testing.T
	reg    *Registry
	runner *Runner
	tasks  *tasks.Manager
	home   string
	write  string
	audit  *Audit
}

func newHarness(t *testing.T) *harness {
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
	audit, err := OpenAudit(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	tm := tasks.NewManager()
	h := &harness{t: t, reg: NewRegistry(), tasks: tm, home: home, write: writeRoot, audit: audit}
	h.runner = &Runner{
		Reg: h.reg, Tasks: tm, Audit: audit,
		Ctx: &Ctx{Ctx: context.Background(), Guard: g, Exec: execx.New(),
			Probes: execx.NewProbeCache(), TempDir: t.TempDir(),
			Log: func(string) {}, Progress: func(int, string) {}},
	}
	return h
}

func (h *harness) waitTask(id string) tasks.Snapshot {
	h.t.Helper()
	tk, ok := h.tasks.Get(id)
	if !ok {
		h.t.Fatalf("任务 %s 不存在", id)
	}
	select {
	case <-tk.Done():
	case <-time.After(5 * time.Second):
		h.t.Fatalf("任务 %s 超时未结束", id)
	}
	return tk.Snapshot()
}

// ---------- 元数据完整性 ----------

func TestMetaIntegrity(t *testing.T) {
	reg := NewRegistry()
	// 故意用不合法的元数据，注册必须 panic（编程错误要早暴露）。
	bad := []struct {
		name string
		m    Meta
	}{
		{"id 为空", Meta{Name: "x", Category: "c", Available: true}},
		{"缺 name", Meta{ID: "a.b", Category: "c", Available: true}},
		{"缺 category", Meta{ID: "a.b", Name: "x", Available: true}},
		{"不可用但不说原因", Meta{ID: "a.b", Name: "x", Category: "c"}},
		{"可用却带原因", Meta{ID: "a.b", Name: "x", Category: "c", Available: true, UnavailableReason: "x"}},
		{"危险却没有确认口径", Meta{ID: "a.b", Name: "x", Category: "c", Available: true, Danger: true}},
		{"非危险却有确认口径", Meta{ID: "a.b", Name: "x", Category: "c", Available: true, DangerFloor: "我已知晓"}},
	}
	for _, c := range bad {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("非法元数据应 panic: %s", c.name)
				}
			}()
			reg.Register(staticTool{meta: c.m})
		}()
	}
	// 参数类型非法 / 重复 / select 无选项 / 路径无 help
	paramBad := []Meta{
		{ID: "p.a", Name: "x", Category: "c", Available: true, Params: []Param{{Name: "a", Label: "A", Type: "weird"}}},
		{ID: "p.b", Name: "x", Category: "c", Available: true, Params: []Param{
			{Name: "a", Label: "A", Type: TypeText}, {Name: "a", Label: "A2", Type: TypeText}}},
		{ID: "p.c", Name: "x", Category: "c", Available: true, Params: []Param{{Name: "a", Label: "A", Type: TypeSelect}}},
		{ID: "p.d", Name: "x", Category: "c", Available: true, Params: []Param{{Name: "a", Label: "A", Type: TypePath}}},
		{ID: "p.e", Name: "x", Category: "c", Available: true, Params: []Param{{Name: "", Label: "A", Type: TypeText}}},
	}
	for _, m := range paramBad {
		if err := ValidateMeta(m); err == nil {
			t.Errorf("非法参数元数据应被拒: %+v", m)
		}
	}
	// 合法元数据 + 重复 id
	ok := staticTool{meta: Meta{ID: "dup.id", Name: "x", Category: "c", Available: true,
		Params: []Param{{Name: "a", Label: "A", Type: TypeBool}}}}
	reg.Register(ok)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("重复 id 应 panic")
			}
		}()
		reg.Register(ok)
	}()
}

func numPtr(f float64) *float64 { return &f }

type staticTool struct{ meta Meta }

func (s staticTool) Meta() Meta { return s.meta }
func (s staticTool) Run(ctx context.Context, c *Ctx, in Input) (*Result, error) {
	return &Result{OK: true, Msg: "ok"}, nil
}

// TestRegistryAddsStatically：注册表只接受自洽元数据，列表按注册顺序展开。
func TestRegistryAddsStatically(t *testing.T) {
	reg := NewRegistry()
	reg.Register(staticTool{meta: Meta{ID: "z.b", Name: "B", Category: "text", Available: true}})
	reg.Register(staticTool{meta: Meta{ID: "a.a", Name: "A", Category: "img", Available: true}})
	cats := reg.List()
	if len(cats) != 2 {
		t.Fatalf("应有 2 个分类，实际 %d", len(cats))
	}
	// img 在 CategoryOrder 里排在 text 前面。
	if cats[0].ID != "img" || cats[1].ID != "text" {
		t.Fatalf("分类顺序不符: %s, %s", cats[0].ID, cats[1].ID)
	}
	if cats[0].Title != "图片处理" {
		t.Errorf("分类标题应取中文名，实际 %q", cats[0].Title)
	}
	if _, ok := reg.Get("a.a"); !ok {
		t.Error("应按 id 取到工具")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("不存在的 id 不该取到")
	}
}

// ---------- Run 包装 ----------

func TestSyncRunResolvesPathsAndAudits(t *testing.T) {
	h := newHarness(t)
	var ran int32
	h.reg.Register(okTool{ran: &ran})
	src := filepath.Join(h.home, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(h.write, "b.txt")
	resp, code, err := h.runner.Run(context.Background(), "tester", "demo.ok",
		map[string]any{"text": "hi", "file": src, "out": out})
	if err != nil {
		t.Fatalf("同步执行失败: %v (code=%d)", err, code)
	}
	if resp.Accepted || resp.Result == nil || !resp.Result.OK {
		t.Fatalf("同步结果不符: %+v", resp)
	}
	data := resp.Result.Data.(map[string]any)
	if data["file"] != src {
		t.Errorf("file 应解析为该路径，实际 %v", data["file"])
	}
	if _, ok := data["out"].(string); !ok || data["out"] == "" {
		t.Errorf("out 应解析为真实路径，实际 %v", data["out"])
	}
	raw, err := os.ReadFile(h.audit.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tool":"demo.ok"`) || !strings.Contains(string(raw), `"status":"ok"`) {
		t.Fatalf("审计日志内容不符: %s", raw)
	}
}

func TestPathOutsideRootRejected(t *testing.T) {
	h := newHarness(t)
	h.reg.Register(okTool{ran: new(int32)})
	outside := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, code, err := h.runner.Run(context.Background(), "u", "demo.ok", map[string]any{"file": outside})
	if err == nil {
		t.Fatal("读根之外的路径应被拒")
	}
	if code != 403 {
		t.Errorf("路径越界应 403，实际 %d", code)
	}
	_, code2, err2 := h.runner.Run(context.Background(), "u", "demo.ok",
		map[string]any{"out": filepath.Join(h.home, "not-write-root.txt")})
	if err2 == nil || code2 != 403 {
		t.Fatalf("写根之外的 outpath 应 403，实际 code=%d err=%v", code2, err2)
	}
}

func TestParamTypeValidation(t *testing.T) {
	h := newHarness(t)
	reg := NewRegistry()
	reg.Register(staticTool{meta: Meta{ID: "demo.types", Name: "类型", Category: "example", Available: true,
		Params: []Param{
			{Name: "n", Label: "数字", Type: TypeNumber, Min: numPtr(1), Max: numPtr(10)},
			{Name: "b", Label: "布尔", Type: TypeBool},
			{Name: "s", Label: "选择", Type: TypeSelect, Options: []Option{{Value: "a", Label: "A"}}},
		}}})
	h.runner.Reg = reg
	if _, code, err := h.runner.Run(context.Background(), "u", "demo.types",
		map[string]any{"n": 99}); err == nil || code != 400 {
		t.Fatalf("超上限的数字应 400，实际 code=%d err=%v", code, err)
	}
	if _, code, err := h.runner.Run(context.Background(), "u", "demo.types",
		map[string]any{"n": "abc"}); err == nil || code != 400 {
		t.Fatalf("非数字应 400，实际 code=%d err=%v", code, err)
	}
	if _, code, err := h.runner.Run(context.Background(), "u", "demo.types",
		map[string]any{"s": "z"}); err == nil || code != 400 {
		t.Fatalf("select 越权取值应 400，实际 code=%d err=%v", code, err)
	}
	if _, _, err := h.runner.Run(context.Background(), "u", "demo.types",
		map[string]any{"n": 5, "b": true, "s": "a"}); err != nil {
		t.Fatalf("合法参数应通过: %v", err)
	}
}

func TestAsyncRunReturnsTaskAndResult(t *testing.T) {
	h := newHarness(t)
	h.reg.Register(asyncTool{})
	resp, code, err := h.runner.Run(context.Background(), "u", "demo.async", map[string]any{"text": "hello"})
	if err != nil || code != 202 {
		t.Fatalf("异步应 202，实际 code=%d err=%v", code, err)
	}
	if !resp.Accepted || resp.TaskID == "" {
		t.Fatalf("异步应返回 task_id，实际 %+v", resp)
	}
	snap := h.waitTask(resp.TaskID)
	if snap.Status != tasks.Succeeded {
		t.Fatalf("任务应成功，实际 %s（%s）", snap.Status, snap.Error)
	}
	if snap.Result == nil {
		t.Fatal("任务结果应可查")
	}
}

func TestAsyncFailureRecorded(t *testing.T) {
	h := newHarness(t)
	h.reg.Register(asyncTool{})
	resp, _, err := h.runner.Run(context.Background(), "u", "demo.async", map[string]any{"text": "fail"})
	if err != nil {
		t.Fatalf("提交异步任务本身不该失败: %v", err)
	}
	snap := h.waitTask(resp.TaskID)
	if snap.Status != tasks.Failed {
		t.Fatalf("应记失败，实际 %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "异步失败原因") {
		t.Fatalf("失败原因应如实记录，实际 %q", snap.Error)
	}
}

func TestDangerRequiresConfirmAndDoesNotRun(t *testing.T) {
	h := newHarness(t)
	var ran int32
	h.reg.Register(dangerTool{ran: &ran})
	_, code, err := h.runner.Run(context.Background(), "u", "demo.danger", map[string]any{"text": "x"})
	if err == nil || code != 400 {
		t.Fatalf("缺确认的危险工具应 400，实际 code=%d err=%v", code, err)
	}
	if ran != 0 {
		t.Fatal("缺确认时**绝不能执行**")
	}
	// 确认值不对也不行。
	_, _, err = h.runner.Run(context.Background(), "u", "demo.danger",
		map[string]any{"text": "x", ConfirmField: "随便"})
	if err == nil || ran != 0 {
		t.Fatal("确认值不符时不能执行")
	}
	// 正确确认才执行。
	if _, _, err := h.runner.Run(context.Background(), "u", "demo.danger",
		map[string]any{"text": "x", ConfirmField: "我已知晓"}); err != nil {
		t.Fatalf("带正确确认应执行: %v", err)
	}
	if ran != 1 {
		t.Fatalf("确认后应执行一次，实际 %d", ran)
	}
}

// TestStricterConfirmEnforced：工具自己加的更严确认必须在执行前生效。
func TestStricterConfirmEnforced(t *testing.T) {
	h := newHarness(t)
	var ran int32
	h.reg.Register(strictDangerTool{ran: &ran})
	// 勾选层级过了，但目标名没原样输入 → 拒，且不执行。
	_, code, err := h.runner.Run(context.Background(), "u", "demo.danger.strict",
		map[string]any{"name": "随便", ConfirmField: "我已知晓"})
	if err == nil || code != 400 {
		t.Fatalf("严格确认不通过应 400，实际 code=%d err=%v", code, err)
	}
	if ran != 0 {
		t.Fatal("严格确认不通过时不能执行")
	}
	if _, _, err := h.runner.Run(context.Background(), "u", "demo.danger.strict",
		map[string]any{"name": "confirm-me", ConfirmField: "我已知晓"}); err != nil {
		t.Fatalf("严格确认通过后应执行: %v", err)
	}
	if ran != 1 {
		t.Fatalf("应执行一次，实际 %d", ran)
	}
}

func TestUnavailableToolRefused(t *testing.T) {
	h := newHarness(t)
	h.reg.Register(unavailableTool{})
	_, code, err := h.runner.Run(context.Background(), "u", "demo.missing", nil)
	if err == nil || code != 503 {
		t.Fatalf("不可用工具应 503 并给原因，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "缺少命令 foobar") {
		t.Fatalf("应带真实原因，实际 %v", err)
	}
}

func TestSlowProbeOnlyOnRun(t *testing.T) {
	h := newHarness(t)
	p := &probeTool{}
	h.reg.Register(p)
	// 列表阶段绝不触发昂贵探测。
	h.reg.List()
	if p.calls != 0 {
		t.Fatalf("列表阶段不该做昂贵探测，实际调用 %d 次", p.calls)
	}
	_, code, err := h.runner.Run(context.Background(), "u", "demo.probe", map[string]any{"text": "x"})
	if err == nil || code != 503 {
		t.Fatalf("探测失败应 503，实际 code=%d err=%v", code, err)
	}
	if p.calls != 1 {
		t.Fatalf("点执行时应探测一次，实际 %d", p.calls)
	}
}

func TestAuditDoesNotLeakPassword(t *testing.T) {
	h := newHarness(t)
	reg := NewRegistry()
	reg.Register(staticTool{meta: Meta{ID: "demo.pw", Name: "口令", Category: "example", Available: true,
		Params: []Param{{Name: "password", Label: "口令", Type: TypePassword}}}})
	h.runner.Reg = reg
	if _, _, err := h.runner.Run(context.Background(), "u", "demo.pw",
		map[string]any{"password": "super-secret-value"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(h.audit.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-value") {
		t.Fatalf("审计日志泄露了口令: %s", raw)
	}
	if !strings.Contains(string(raw), "<已提交>") {
		t.Fatalf("审计应记录口令已提交但不出值: %s", raw)
	}
}

func TestAuditArgvAndLongValueTruncated(t *testing.T) {
	h := newHarness(t)
	reg := NewRegistry()
	reg.Register(staticTool{meta: Meta{ID: "demo.long", Name: "长值", Category: "example", Available: true,
		Params: []Param{{Name: "text", Label: "文本", Type: TypeTextarea}}}})
	h.runner.Reg = reg
	long := strings.Repeat("x", 5000)
	if _, _, err := h.runner.Run(context.Background(), "u", "demo.long", map[string]any{"text": long}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(h.audit.Path())
	if len(raw) > 4000 {
		t.Fatalf("审计单条应被截断，实际长度 %d", len(raw))
	}
}

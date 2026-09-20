package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tasks"
)

// ErrConfirmRequired 表示危险工具缺少确认字段：**拒绝且绝不执行**。
var ErrConfirmRequired = errors.New("危险操作需要确认")

// ErrUnavailable 表示工具在当前机器上不可用（真实原因在 error 里）。
var ErrUnavailable = errors.New("工具不可用")

// ConfirmField 是危险操作确认字段名（前端按此提交，后端强校验）。
const ConfirmField = "_confirm"

// Runner 把注册表、路径闸门、执行器、任务中心、审计串起来。
type Runner struct {
	Reg    *Registry
	Ctx    *Ctx
	Tasks  *tasks.Manager
	Audit  *Audit
	Logger func(format string, args ...any)
}

// Response 是 POST /api/tools/{id}/run 的返回。
type Response struct {
	// Accepted=true 表示走了异步任务，结果去 GET /api/tasks/{id} 取。
	Accepted bool    `json:"accepted,omitempty"`
	TaskID   string  `json:"task_id,omitempty"`
	Result   *Result `json:"result,omitempty"`
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger(format, args...)
	}
}

// statusForError 决定校验失败该回哪个状态码：
// 路径越界/敏感目录 = 403（不是用户填错，是**不允许**），其余校验错误 400。
func statusForError(err error) int {
	var fe *fsroot.Error
	if errors.As(err, &fe) {
		return 403
	}
	return 400
}

// Run 执行工具：校验 → 审计 → 同步执行或投任务中心。
func (r *Runner) Run(ctx context.Context, user, id string, body map[string]any) (*Response, int, error) {
	if r.Ctx == nil || r.Ctx.Guard == nil {
		return nil, 500, errors.New("服务未初始化路径闸门")
	}
	d, ok := r.Reg.Get(id)
	if !ok {
		return nil, 404, fmt.Errorf("工具不存在: %s", id)
	}
	m := d.Meta()

	in, err := r.resolve(d, m, body)
	if err != nil {
		return nil, statusForError(err), err
	}
	if m.Danger {
		got, _ := body[ConfirmField].(string)
		if strings.TrimSpace(got) != m.DangerFloor {
			return nil, 400, fmt.Errorf("%w：请提交 %s=%s", ErrConfirmRequired, ConfirmField, m.DangerFloor)
		}
		// 更严的确认（可选）：危险工具自己复核语义，仍要求用户先填对了。
		if sc, ok := d.(DangerStricter); ok {
			if err := sc.ConfirmOK(in); err != nil {
				return nil, 400, fmt.Errorf("%w：%v", ErrConfirmRequired, err)
			}
		}
	}
	if !m.Available {
		reason := m.UnavailableReason
		if reason == "" {
			reason = "当前机器缺少依赖"
		}
		return nil, 503, fmt.Errorf("%w：%s", ErrUnavailable, reason)
	}
	// 昂贵探测只在用户真点这个工具时做（坑 E1）。
	if sp, ok := d.(SlowProbe); ok {
		if ok2, reason := sp.Probe(ctx, r.Ctx); !ok2 {
			return nil, 503, fmt.Errorf("%w：%s", ErrUnavailable, reason)
		}
	}

	summary := summarize(m, body)
	start := time.Now()
	r.audit(user, m.ID, summary, "start", "", 0)

	if !m.Async {
		res, err := r.runSync(ctx, d, m, in)
		dur := time.Since(start)
		if err != nil {
			r.audit(user, m.ID, summary, "error", err.Error(), dur)
			return nil, 500, err
		}
		r.audit(user, m.ID, summary, "ok", "", dur)
		fillDownloadURLs(res)
		return &Response{Result: res}, 200, nil
	}

	if r.Tasks == nil {
		err := errors.New("异步工具需要任务中心")
		r.audit(user, m.ID, summary, "error", err.Error(), time.Since(start))
		return nil, 500, err
	}
	title := m.Name
	tk := r.Tasks.Start(m.ID, title, func(tctx context.Context, log tasks.Logger) (any, error) {
		c := *r.Ctx
		c.Ctx = tctx
		c.Log = log
		c.Progress = func(percent int, text string) {
			log(fmt.Sprintf("[%d%%] %s", percent, text))
		}
		res, err := d.Run(tctx, &c, in)
		dur := time.Since(start)
		if err != nil {
			r.audit(user, m.ID, summary, "error", err.Error(), dur)
			return nil, err
		}
		if res == nil {
			res = &Result{OK: true, Msg: "完成"}
		}
		res.OK = true
		if res.Msg == "" {
			res.Msg = "完成"
		}
		fillDownloadURLs(res)
		r.audit(user, m.ID, summary, "ok", "", dur)
		return res, nil
	})
	return &Response{Accepted: true, TaskID: tk.ID()}, 202, nil
}

func (r *Runner) runSync(ctx context.Context, d Doer, m Meta, in Input) (*Result, error) {
	c := *r.Ctx
	c.Ctx = ctx
	if c.Log == nil {
		c.Log = func(string) {}
	}
	if c.Progress == nil {
		c.Progress = func(int, string) {}
	}
	res, err := d.Run(ctx, &c, in)
	if err != nil {
		return nil, err
	}
	if res == nil {
		res = &Result{OK: true, Msg: "完成"}
	}
	res.OK = true
	if res.Msg == "" {
		res.Msg = "完成"
	}
	return res, nil
}

// resolve 校验参数：类型、必填、select 取值、路径白名单。
//
// 未在 params 里声明的字段会被丢掉，只有确认字段原样透传给工具
// （危险工具自己复核，见 Dangerous 契约）。
func (r *Runner) resolve(d Doer, m Meta, body map[string]any) (Input, error) {
	raw := map[string]any{}
	resolved := map[string]string{}
	in := Input{raw: raw, resolved: resolved}
	if v, ok := body[ConfirmField]; ok {
		raw[ConfirmField] = toString(v)
	}
	for _, p := range m.Params {
		v, present := body[p.Name]
		if !present || v == nil || (isStringish(p.Type) && strings.TrimSpace(toString(v)) == "") {
			if p.Required {
				return in, fmt.Errorf("参数 %s（%s）必填", p.Label, p.Name)
			}
			if p.Default != nil {
				raw[p.Name] = p.Default
			}
			continue
		}
		switch p.Type {
		case TypeText, TypeTextarea, TypePassword:
			raw[p.Name] = toString(v)
		case TypeNumber:
			f, err := toFloat(v)
			if err != nil {
				return in, fmt.Errorf("参数 %s 必须是数字", p.Label)
			}
			if p.Min != nil && f < *p.Min {
				return in, fmt.Errorf("参数 %s 不能小于 %g", p.Label, *p.Min)
			}
			if p.Max != nil && f > *p.Max {
				return in, fmt.Errorf("参数 %s 不能大于 %g", p.Label, *p.Max)
			}
			raw[p.Name] = f
		case TypeBool:
			b, err := toBool(v)
			if err != nil {
				return in, fmt.Errorf("参数 %s 必须是布尔值", p.Label)
			}
			raw[p.Name] = b
		case TypeSelect:
			s := toString(v)
			okOpt := false
			for _, o := range p.Options {
				if o.Value == s {
					okOpt = true
				}
			}
			if !okOpt {
				return in, fmt.Errorf("参数 %s 取值非法: %s", p.Label, s)
			}
			raw[p.Name] = s
		case TypePath, TypeOutPath:
			s := toString(v)
			mode := fsroot.Read
			if p.Type == TypeOutPath {
				mode = fsroot.Write
			}
			real, err := r.Ctx.Guard.Resolve(mode, s, p.AllowMissing)
			if err != nil {
				return in, err
			}
			raw[p.Name] = s
			resolved[p.Name] = real
		}
	}
	return in, nil
}

func isStringish(t string) bool {
	switch t {
	case TypeText, TypeTextarea, TypePassword, TypePath, TypeOutPath, TypeSelect:
		return true
	}
	return false
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprintf("%v", v)
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case json.Number:
		return x.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	case int:
		return float64(x), nil
	}
	return 0, fmt.Errorf("不是数字")
}

func toBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		if s == "true" || s == "1" || s == "on" {
			return true, nil
		}
		if s == "false" || s == "0" || s == "off" || s == "" {
			return false, nil
		}
	case float64:
		return x != 0, nil
	}
	return false, fmt.Errorf("不是布尔值")
}

// summarize 生成审计用的参数摘要：口令类只记"已提交"，长值截断。
func summarize(m Meta, body map[string]any) string {
	byName := map[string]Param{}
	for _, p := range m.Params {
		byName[p.Name] = p
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		if k == ConfirmField {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := body[k]
		p, known := byName[k]
		if known && (p.Type == TypePassword || p.Secret) {
			parts = append(parts, k+"=<已提交>")
			continue
		}
		s := toString(v)
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	return truncate(strings.Join(parts, " "), 1000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fillDownloadURLs 给产物补下载链接（base64url 路径，避免中文与斜杠转义问题）。
func fillDownloadURLs(res *Result) {
	if res == nil {
		return
	}
	for i := range res.Files {
		f := &res.Files[i]
		if f.Path == "" {
			continue
		}
		// 相对路径：直连（/）与经面板子路径（/macsaber/）都对；绝对 /api 会打到面板的接口（真机实测 404）。
		f.DownloadURL = "api/files/download?path=" + base64.RawURLEncoding.EncodeToString([]byte(f.Path))
		if f.Size == 0 {
			if st, err := os.Stat(f.Path); err == nil {
				f.Size = st.Size()
			}
		}
		if f.Name == "" {
			f.Name = filepath.Base(f.Path)
		}
	}
}

// Audit 是 <data>/audit.log 的 JSONL 追加器。可并发使用。
type Audit struct {
	mu   sync.Mutex
	f    *os.File
	path string
	// Err 是最近一次写失败（非 nil 时危险工具会被拒绝执行）。
	Err error
}

// AuditEntry 是一条审计记录。
type AuditEntry struct {
	At       time.Time `json:"at"`
	User     string    `json:"user"`
	Tool     string    `json:"tool"`
	Params   string    `json:"params"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
	Duration int64     `json:"duration_ms"`
}

// OpenAudit 打开（并追加）审计日志。
func OpenAudit(path string) (*Audit, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Audit{f: f, path: path}, nil
}

// Path 返回审计文件路径。
func (a *Audit) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// Write 追加一条。写失败会记进 a.Err 并返回错误（调用方据此拒绝危险操作）。
func (a *Audit) Write(e AuditEntry) error {
	if a == nil {
		return nil
	}
	e.At = time.Now()
	// 审计日志是给人 grep 的，不要 HTML 转义（否则 <已提交> 会变成 \u003c…）。
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return err
	}
	b := []byte(strings.TrimRight(sb.String(), "\n"))
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.f.Write(append(b, '\n')); err != nil {
		a.Err = err
		return err
	}
	return nil
}

// Close 关闭审计文件。
func (a *Audit) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	return a.f.Close()
}

func (r *Runner) audit(user, id, params, status, errMsg string, dur time.Duration) {
	if r.Audit == nil {
		return
	}
	if err := r.Audit.Write(AuditEntry{
		User: user, Tool: id, Params: params, Status: status,
		Error: errMsg, Duration: dur.Milliseconds(),
	}); err != nil {
		r.logf("审计写入失败: %v", err)
	}
}

// Package tool 是工具注册表与统一 Run 包装层。
//
// 契约（后续所有工具包都按这个来）：
//   - 每个工具实现 Doer；元数据由 Meta() 下发，前端一套渲染器画完。
//   - 长任务声明 Async=true，Run 走任务中心（202 + task_id）。
//   - 危险工具声明 Danger=true，且必须实现 DangerConfirmer；后端强制校验确认字段。
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
)

// 参数类型固定这几种，前端渲染器只认它们。
const (
	TypeText     = "text"
	TypeNumber   = "number"
	TypeSelect   = "select"
	TypeBool     = "bool"
	TypePath     = "path"
	TypeOutPath  = "outpath"
	TypePassword = "password"
	TypeTextarea = "textarea"
)

// validTypes 是参数类型白名单（有测试锁死）。
var validTypes = map[string]bool{
	TypeText: true, TypeNumber: true, TypeSelect: true, TypeBool: true,
	TypePath: true, TypeOutPath: true, TypePassword: true, TypeTextarea: true,
}

// Option 是 select 的一个选项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Param 是一个参数声明。Label/Placeholder/Help 都是用户可见文案（一句话 ≤40 字）。
type Param struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Options     []Option `json:"options,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	// AllowMissing 只对 path/outpath 有意义：目标可以还不存在（新建文件）。
	AllowMissing bool `json:"allow_missing,omitempty"`
	// Probe 非空表示"这个参数依赖昂贵探测"，前端在按钮上给可见进度（坑 E1）。
	Probe string `json:"probe,omitempty"`
	// Multiline 让 textarea 参数的行数更多。
	Multiline bool `json:"multiline,omitempty"`
	// Secret 表示值不能进审计日志（口令类）。
	Secret bool `json:"secret,omitempty"`
}

// Meta 是一个工具对外的全部元数据。
type Meta struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Summary   string  `json:"summary"`
	Category  string  `json:"category"`
	Icon      string  `json:"icon,omitempty"`
	Params    []Param `json:"params"`
	Async     bool    `json:"async"`
	Danger    bool    `json:"danger"`
	Available bool    `json:"available"`
	// UnavailableReason 是**真实原因**；available=false 时必须有值。
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	// DangerFloor 是危险确认时用户必须原样输入/勾选的值（如 "我已知晓"）。
	DangerFloor string `json:"danger_floor,omitempty"`
	// TimeoutSeconds 是同步工具的服务端超时；0 表示用默认值。
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// DangerOnly 仅供打样：实现真的会执行，但动作是可逆的安全动作。
	DangerNote string `json:"danger_note,omitempty"`
}

// Doer 是工具实现。
type Doer interface {
	// Meta 返回元数据。Available 必须来自真实探测，不许猜（诚实原则）。
	Meta() Meta
	// Run 执行。in 是校验后的参数表（路径已解析为真实绝对路径）。
	Run(ctx context.Context, c *Ctx, in Input) (*Result, error)
}

// DangerConfirmer：危险工具的确认契约。
//
// 最小实现只需给一句话（用户勾选/手输它）；可选实现 ConfirmOK 做更严的校验
// （例如必须手输目标名字，而不只是勾一个框）。服务端在**执行前**强制校验：
// 缺字段或校验不通过一律拒绝，工具代码不会跑到。
type DangerConfirmer interface {
	DangerFloor() string
}

// DangerStricter 是更严的确认（可选实现）。
type DangerStricter interface {
	ConfirmOK(in Input) error
}

// Availlable 慢探测（昂贵）：只在用户真点这个工具时调用。
type SlowProbe interface {
	// Probe 返回 (是否可用, 真实原因)。结果应由实现在 ProbeCache 里缓存。
	Probe(ctx context.Context, c *Ctx) (bool, string)
}

// Input 是校验后的参数值。
type Input struct {
	raw      map[string]any
	resolved map[string]string
}

// Get 取任意值（bool/number 由 decode 还原）。
func (i Input) Get(name string) any { return i.raw[name] }

// Str 取字符串。
func (i Input) Str(name string) string {
	if v, ok := i.raw[name].(string); ok {
		return v
	}
	return ""
}

// Bool 取布尔；缺省返回 def。
func (i Input) Bool(name string, def bool) bool {
	if v, ok := i.raw[name].(bool); ok {
		return v
	}
	return def
}

// Num 取数字；缺省返回 def。
func (i Input) Num(name string, def float64) float64 {
	switch v := i.raw[name].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return def
}

// Int 取整数。
func (i Input) Int(name string, def int) int { return int(i.Num(name, float64(def))) }

// Path 取已解析为真实绝对路径的路径参数。
func (i Input) Path(name string) string { return i.resolved[name] }

// Has 判断参数是否被提交（含空串）。
func (i Input) Has(name string) bool {
	_, ok := i.raw[name]
	return ok
}

// Raw 返回原始 map 的副本（工具自己判空用）。
func (i Input) Raw() map[string]any {
	out := make(map[string]any, len(i.raw))
	for k, v := range i.raw {
		out[k] = v
	}
	return out
}

// File 是结果里可下载的文件。
type File struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

// Result 是所有工具统一的结果结构。
type Result struct {
	OK    bool   `json:"ok"`
	Msg   string `json:"msg"`
	Data  any    `json:"data,omitempty"`
	Files []File `json:"files,omitempty"`
}

// ResultFromFile 造一个"单文件产物"结果（download_url 由 web 层补）。
func ResultFromFile(msg, path string) *Result {
	r := &Result{OK: true, Msg: msg}
	if st, err := os.Stat(path); err == nil {
		r.Files = []File{{Name: filepath.Base(path), Path: path, Size: st.Size()}}
	}
	return r
}

// Ctx 是工具执行上下文：路径闸门、执行器、探测缓存、进度口。
type Ctx struct {
	// Ctx 是任务的 context（异步时是任务自己的，用户刷新不会中断）。
	Ctx context.Context
	// Guard 是路径白名单闸门。
	Guard *fsroot.Guard
	// Exec 是安全命令封装。
	Exec *execx.Execer
	// Probes 是昂贵探测缓存。
	Probes *execx.ProbeCache
	// TempDir 是本进程可写的临时目录（系统临时目录，不是用户数据目录）。
	TempDir string
	// Log 写任务进度（同步工具里是空实现）。
	Log func(text string)
	// Progress 报告百分比（无进度概念的工具可以不调）。
	Progress func(percent int, text string)
}

// Logf 便捷格式化进度。
func (c *Ctx) Logf(format string, args ...any) {
	if c != nil && c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

// Registry 是工具注册表。
type Registry struct {
	mu    sync.RWMutex
	byID  map[string]Doer
	order []string
}

// NewRegistry 建立空注册表。
func NewRegistry() *Registry {
	return &Registry{byID: map[string]Doer{}}
}

// Register 注册工具；id 重复或参数类型非法都是编程错误，直接 panic（有测试兜底）。
func (r *Registry) Register(d Doer) {
	m := d.Meta()
	if err := ValidateMeta(m); err != nil {
		panic("工具元数据非法: " + err.Error())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[m.ID]; dup {
		panic("工具 id 重复: " + m.ID)
	}
	r.byID[m.ID] = d
	r.order = append(r.order, m.ID)
}

// Get 按 id 取工具。
func (r *Registry) Get(id string) (Doer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byID[id]
	return d, ok
}

// All 返回全部工具（按注册顺序）。
func (r *Registry) All() []Doer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Doer, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// ValidateMeta 校验元数据自洽性（供测试与 Register 共用）。
func ValidateMeta(m Meta) error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("id 为空")
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%s: name 为空", m.ID)
	}
	if strings.TrimSpace(m.Category) == "" {
		return fmt.Errorf("%s: category 为空", m.ID)
	}
	if !m.Available && strings.TrimSpace(m.UnavailableReason) == "" {
		return fmt.Errorf("%s: available=false 必须给 unavailable_reason", m.ID)
	}
	if m.Available && strings.TrimSpace(m.UnavailableReason) != "" {
		return fmt.Errorf("%s: available=true 不该带 unavailable_reason", m.ID)
	}
	seen := map[string]bool{}
	for _, p := range m.Params {
		if !validTypes[p.Type] {
			return fmt.Errorf("%s.%s: 非法参数类型 %q", m.ID, p.Name, p.Type)
		}
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Label) == "" {
			return fmt.Errorf("%s: 参数缺 name/label", m.ID)
		}
		if seen[p.Name] {
			return fmt.Errorf("%s: 参数名重复 %s", m.ID, p.Name)
		}
		seen[p.Name] = true
		if p.Type == TypeSelect && len(p.Options) == 0 {
			return fmt.Errorf("%s.%s: select 必须给 options", m.ID, p.Name)
		}
		if (p.Type == TypePath || p.Type == TypeOutPath) && strings.TrimSpace(p.Help) == "" {
			return fmt.Errorf("%s.%s: 路径参数必须写 help（说明落在哪个根内）", m.ID, p.Name)
		}
	}
	if m.Danger && strings.TrimSpace(m.DangerFloor) == "" {
		return fmt.Errorf("%s: danger=true 必须有 danger_floor（后端强校验确认字段）", m.ID)
	}
	if !m.Danger && strings.TrimSpace(m.DangerFloor) != "" {
		return fmt.Errorf("%s: 非危险工具不该有 danger_floor", m.ID)
	}
	return nil
}

// Category 是 /api/tools 的返回分组。
type Category struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Icon  string `json:"icon"`
	Order int    `json:"-"`
	Tools []Meta `json:"tools"`
}

// CategoryTitles 给分类 id 一个中文标题。未登记的分类用 id 兜底。
var CategoryTitles = map[string]string{
	"img":     "图片处理",
	"text":    "文本与编码",
	"sys":     "系统与网络",
	"pdf":     "PDF",
	"ocr":     "OCR 视觉",
	"media":   "音视频",
	"file":    "文件与归档",
	"search":  "搜索",
	"sec":     "安全与签名",
	"auto":    "自动化桥接",
	"example": "示例",
}

// CategoryOrder 决定分类顺序（未登记的排在后面）。
var CategoryOrder = []string{"img", "text", "sys", "pdf", "ocr", "media", "file", "search", "sec", "auto", "example"}

// List 组装 /api/tools 的返回（首屏只信 Meta 里的便宜探测结论，不做昂贵探测）。
func (r *Registry) List() []Category {
	metas := make([]Meta, 0)
	for _, d := range r.All() {
		metas = append(metas, d.Meta())
	}
	idx := map[string]int{}
	cats := make([]Category, 0)
	for _, m := range metas {
		i, ok := idx[m.Category]
		if !ok {
			title := CategoryTitles[m.Category]
			if title == "" {
				title = m.Category
			}
			cats = append(cats, Category{ID: m.Category, Title: title, Icon: m.Icon})
			i = len(cats) - 1
			idx[m.Category] = i
		}
		if cats[i].Icon == "" {
			cats[i].Icon = m.Icon
		}
		cats[i].Tools = append(cats[i].Tools, m)
	}
	rank := func(id string) int {
		for i, c := range CategoryOrder {
			if c == id {
				return i
			}
		}
		return len(CategoryOrder)
	}
	sort.SliceStable(cats, func(a, b int) bool { return rank(cats[a].ID) < rank(cats[b].ID) })
	return cats
}

// Timeout 返回工具的服务端超时。
func Timeout(m Meta, d Doer) time.Duration {
	if t, ok := d.(interface{ Timeout() time.Duration }); ok {
		if v := t.Timeout(); v > 0 {
			return v
		}
	}
	if m.TimeoutSeconds > 0 {
		return time.Duration(m.TimeoutSeconds) * time.Second
	}
	if m.Async {
		return 30 * time.Minute
	}
	return 60 * time.Second
}

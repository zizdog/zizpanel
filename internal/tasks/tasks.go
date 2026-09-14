// Package tasks 是面板的「任务中心」：把安装/卸载这类**几分钟到十几分钟**的操作
// 从"同步 HTTP 请求"改成"后台任务 + 实时进度流"。
//
// 为什么要有它（这三条都是真实痛点，见 SPEC-任务中心.md）：
//
//  1. 同步请求只能给出"请等待"。brew install / docker compose up 的输出被
//     CombinedOutput() 攒在内存里，直到结束才一次性返回 —— 用户在整段时间里
//     看不到任何真实进展（下载了没、卡在哪一步、是不是已经死了）。
//  2. 关掉窗口就再也找不回来。前端那个「后台继续（关闭窗口）」按钮只隐藏了 DOM，
//     用户没有任何入口能重新看到后续进度。
//  3. 任务挂在 HTTP 请求的 context 上，用户一刷新/关标签页，
//     r.Context() 被取消 → 正在跑的 brew / docker 子进程被杀，
//     机器上留下**装到一半**的状态。
//
// 所以任务用 context.Background() 派生，与请求生命周期彻底解耦；
// 每个任务带一个有界的行缓冲与订阅广播，SSE 从任意 seq 续传。
//
// 有意不做的两件事：
//   - **不持久化**。面板进程重启后，"正在安装"本身就是假的（子进程已随进程组结束），
//     把上次残留的任务显示成 running 是谎报；内存版让这件事自动消失。
//   - **不做假百分比**。brew / docker 没有可信的总进度，进度 = 当前步骤 +
//     真实输出流 + 已用时（docker 的层进度本来就在输出里）。
package tasks

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Status 是任务的生命周期状态。
type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// 日志行的级别。用字符串而不是数字：SSE/JSON 里直接可读，排障时不用查表。
const (
	LevelCmd  = "cmd"  // 要执行的命令（灰）
	LevelStep = "step" // 步骤小标题（加粗）
	LevelOut  = "out"  // 命令的普通输出（等宽正文）
	LevelErr  = "err"  // 错误输出（红）
	LevelOK   = "ok"   // 成功（绿）
	LevelWarn = "warn" // 警告（黄）
)

// maxLinesPerTask 是单任务保留的日志行上限。
//
// 为什么是 4000 行：`brew install mysql` 的完整输出大约几百行，`docker compose pull`
// 在层多的时候也就上千行；4000 行足够覆盖一次安装的全过程，同时保证 30 个任务的
// 内存占用可控（一行按 200 字节算，30 × 4000 × 200B ≈ 24MB 上限）。
const maxLinesPerTask = 4000

// maxTasks 是保留的最近任务数（含已完成）。
const maxTasks = 30

// LogFunc 是投喂给任务的一条日志。level 用上面的常量。
type LogFunc func(level, text string)

// Line 是一条日志行。Seq 全局递增（从 1 开始），前端据此断点续传与去重。
type Line struct {
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

// Meta 是任务的元信息（列表页与进度窗标题都用它）。
type Meta struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Target    string    `json:"target"`
	Title     string    `json:"title"`
	Status    Status    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	// FinishedAt 未结束时是 Go 的零值时间（0001-01-01T00:00:00Z）。
	// 用零值而不是指针，是为了让前端少一层判空。
	FinishedAt time.Time `json:"finished_at"`
	ElapsedMS  int64     `json:"elapsed_ms"`
	LineCount  int64     `json:"line_count"`
	// Last 是最后一行日志，列表里直接显示，省得为"看一眼在干什么"再拉详情。
	Last string `json:"last"`
	// Error 是失败原因（status=failed 时有值）。
	Error string `json:"error"`
	// Result 是业务返回值（*services.InstallResult），只在成功后有值。
	// 用 any 是为了不反向依赖 services 包。
	Result any `json:"result,omitempty"`
}

// Task 是一次长任务。
//
// 并发说明：任务自身的字段由 mu 保护；订阅者集合也在 mu 下维护。
// Log 可能被多个 goroutine 调用（命令输出的 stdout/stderr 是两个 goroutine）。
type Task struct {
	id        string
	kind      string
	target    string
	title     string
	startedAt time.Time

	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	status     Status
	finishedAt time.Time
	errMsg     string
	result     any
	// lines 是环形缓冲的尾部：超过 maxLinesPerTask 后从头部丢。
	lines []Line
	// nextSeq 是下一条日志的序号（从 1 开始）。
	nextSeq int64
	// total 是**曾经写过**的行数（不因环形缓冲回绕而减少），进度窗用它显示规模。
	total int64
	subs  map[int]chan Line
	subID int
}

// newTask 建立任务对象（不启动）。id 由 Manager 生成。
func newTask(id, kind, target, title string) *Task {
	return &Task{
		id:        id,
		kind:      kind,
		target:    target,
		title:     title,
		startedAt: time.Now(),
		done:      make(chan struct{}),
		status:    StatusRunning,
		subs:      map[int]chan Line{},
		nextSeq:   1,
	}
}

// ID 返回任务 id。
func (t *Task) ID() string { return t.id }

// Done 在任务结束时关闭，供 SSE / 等待者使用。
func (t *Task) Done() <-chan struct{} { return t.done }

// Log 写一条日志。text 里的换行会拆成多行（命令输出经常一次给一大段）。
func (t *Task) Log(level, text string) {
	for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		t.logLine(level, ln)
	}
}

func (t *Task) logLine(level, text string) {
	t.mu.Lock()
	line := Line{Seq: t.nextSeq, At: time.Now(), Level: level, Text: text}
	t.nextSeq++
	t.total++
	t.lines = append(t.lines, line)
	if len(t.lines) > maxLinesPerTask {
		// 丢最老的。copy 到前面，避免底层数组无限增长。
		drop := len(t.lines) - maxLinesPerTask
		t.lines = append(t.lines[:0], t.lines[drop:]...)
	}
	// 广播必须在锁内做（否则与订阅者的加入有竞态会导致丢行），
	// 但绝不阻塞：满了就关掉那个订阅者，让它的 SSE 断开重连、用 Last-Event-ID 补齐。
	for id, ch := range t.subs {
		select {
		case ch <- line:
		default:
			delete(t.subs, id)
			close(ch)
		}
	}
	t.mu.Unlock()
}

// LogFunc 返回只绑定该任务的 Logger，供业务代码投喂输出。
func (t *Task) LogFunc() LogFunc { return t.Log }

// Snapshot 取 seq > after 的日志（最多 limit 条）。
//
// 返回值：日志、下一条应当请求的 seq、是否还有更多、当前缓冲里最老的 seq。
// oldest 用来如实告诉用户"更早的日志已经滚出缓冲"，而不是假装日志是完整的。
func (t *Task) Snapshot(after int64, limit int) (lines []Line, next int64, hasMore bool, oldest int64) {
	if limit <= 0 {
		limit = 800
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	next = after
	if len(t.lines) > 0 {
		oldest = t.lines[0].Seq
	}
	for _, l := range t.lines {
		if l.Seq <= after {
			continue
		}
		if len(lines) >= limit {
			return lines, next, true, oldest
		}
		lines = append(lines, l)
		next = l.Seq
	}
	return lines, next, false, oldest
}

// Subscribe 注册一个订阅者，返回 id 与接收通道。
// 通道是有缓冲的；调用方只需在退出时 Unsubscribe。
//
// 导出是因为 SSE 处理器在 web 包：订阅顺序必须是"先订阅、再取快照"，
// 否则两者之间产生的行会丢（见 handleTaskStream 的注释）。
func (t *Task) Subscribe() (int, <-chan Line) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subID++
	id := t.subID
	// 缓冲 512 行：单次 emit 不可能超过这个量，正常消费者不会被误判为"跟不上"。
	ch := make(chan Line, 512)
	t.subs[id] = ch
	return id, ch
}

// Unsubscribe 注销订阅者并关闭通道。
func (t *Task) Unsubscribe(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ch, ok := t.subs[id]; ok {
		delete(t.subs, id)
		close(ch)
	}
}

// Meta 返回当前元信息快照。
func (t *Task) Meta() Meta {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.metaLocked()
}

func (t *Task) metaLocked() Meta {
	end := t.finishedAt
	if t.status == StatusRunning {
		end = time.Now()
	}
	elapsed := end.Sub(t.startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	m := Meta{
		ID:         t.id,
		Kind:       t.kind,
		Target:     t.target,
		Title:      t.title,
		Status:     t.status,
		StartedAt:  t.startedAt,
		ElapsedMS:  elapsed.Milliseconds(),
		LineCount:  t.total,
		Error:      t.errMsg,
		Result:     t.result,
		FinishedAt: t.finishedAt,
	}
	if n := len(t.lines); n > 0 {
		m.Last = t.lines[n-1].Text
	}
	return m
}

// Status 返回当前状态。
func (t *Task) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// finish 结束任务（幂等：只有第一次生效）。
func (t *Task) finish(status Status, err error, result any) {
	t.mu.Lock()
	if t.status != StatusRunning {
		t.mu.Unlock()
		return
	}
	t.status = status
	t.finishedAt = time.Now()
	t.result = result
	if err != nil {
		t.errMsg = err.Error()
	}
	t.mu.Unlock()

	// 收尾日志走 Log，保证它也能被正在看进度的人看到。
	switch status {
	case StatusSucceeded:
		t.Log(LevelOK, "任务完成 ✅")
	case StatusCanceled:
		t.Log(LevelWarn, "任务已被中断")
	case StatusFailed:
		if err != nil {
			t.Log(LevelErr, "任务失败："+err.Error())
		}
	}
	close(t.done)
	// 任务结束后订阅者通道可能还有尾巴；SSE 处理器会在 done 后 drain 一次再退出。
	// 这里把订阅者留着，让 drain 拿得到数据。
}

// Manager 持有最近的任务。可并发使用。
type Manager struct {
	mu    sync.Mutex
	tasks []*Task // 新的在前
	seq   atomic.Int64
}

// NewManager 建立任务中心。
func NewManager() *Manager { return &Manager{} }

// Start 启动一个任务并立刻返回。
//
// fn 在独立 goroutine 里执行，ctx 由 context.Background() 派生 ——
// **与 HTTP 请求无关**，所以用户关窗口/刷新/换页面都不会打断安装。
// 中断只能通过 Cancel。
func (m *Manager) Start(kind, target, title string, fn func(ctx context.Context, log LogFunc) (any, error)) *Task {
	id := fmt.Sprintf("t-%d-%d", time.Now().UnixMilli(), m.seq.Add(1))
	t := newTask(id, kind, target, title)

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	m.mu.Lock()
	m.tasks = append([]*Task{t}, m.tasks...)
	// 只保留最近 maxTasks 个：优先淘汰最老的**已结束**任务；
	// 如果全都在跑（理论上不会），就按最老的淘汰，避免无限增长。
	for len(m.tasks) > maxTasks {
		idx := -1
		for i := len(m.tasks) - 1; i >= 0; i-- {
			if m.tasks[i].Status() != StatusRunning {
				idx = i
				break
			}
		}
		if idx < 0 {
			idx = len(m.tasks) - 1
		}
		m.tasks = append(m.tasks[:idx], m.tasks[idx+1:]...)
	}
	m.mu.Unlock()

	t.Log(LevelStep, title+"：任务已创建")

	go func() {
		// 任务结束时释放 ctx（context.CancelFunc 本身幂等，无需再包一层）
		defer cancel()
		// panic 必须就地兜住：这是普通 goroutine，一旦 panic 整个面板进程都会退出，
		// 而安装代码里全是外部命令与文件操作，出人意料的情况并不罕见。
		defer func() {
			if rec := recover(); rec != nil {
				t.finish(StatusFailed, fmt.Errorf("内部错误（panic）：%v", rec), nil)
			}
		}()

		result, err := fn(ctx, t.LogFunc())
		switch {
		case err != nil && ctx.Err() == context.Canceled:
			// 被中断时子进程通常返回 "signal: killed"，那不是真失败，
			// 如实报"已中断"，否则用户会以为是自己装错了。
			t.finish(StatusCanceled, err, result)
		case err != nil:
			t.finish(StatusFailed, err, result)
		default:
			t.finish(StatusSucceeded, nil, result)
		}
	}()

	return t
}

// List 返回最近任务的元信息（新的在前）。
func (m *Manager) List() []Meta {
	m.mu.Lock()
	snapshot := make([]*Task, len(m.tasks))
	copy(snapshot, m.tasks)
	m.mu.Unlock()

	out := make([]Meta, 0, len(snapshot))
	for _, t := range snapshot {
		out = append(out, t.Meta())
	}
	return out
}

// Get 按 id 取任务；找不到返回 nil。
func (m *Manager) Get(id string) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.id == id {
			return t
		}
	}
	return nil
}

// RunningFor 返回某个 target 上正在跑的任务（用于禁止重复安装同一个应用）。
func (m *Manager) RunningFor(target string) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.target == target && t.Status() == StatusRunning {
			return t
		}
	}
	return nil
}

// RunningCount 是运行中的任务数。
func (m *Manager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.tasks {
		if t.Status() == StatusRunning {
			n++
		}
	}
	return n
}

// Cancel 中断任务。已经结束的任务返回错误（让前端能如实提示，而不是假装成功）。
func (m *Manager) Cancel(id string) (*Task, error) {
	t := m.Get(id)
	if t == nil {
		return nil, fmt.Errorf("任务不存在（可能已被更早的任务挤出列表）")
	}
	if t.Status() != StatusRunning {
		return nil, fmt.Errorf("任务已经结束，无法中断")
	}
	t.cancel()
	return t, nil
}

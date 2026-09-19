// Package tasks 是 mac军刀 的内存任务中心。
//
// 结论：长任务必须走这里（202 + task_id，前端轮询），绝不让浏览器干等；
// 任务挂在 context.Background() 上，**与 HTTP 请求无关**，用户刷新不会杀掉任务（坑 D1）。
package tasks

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Status 是任务状态。
type Status string

const (
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	Canceled  Status = "canceled"
)

// Line 是一条进度日志。
type Line struct {
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

// Logger 是任务执行体写进度用的接口。
type Logger func(text string)

// maxLines 是单任务保留的日志行数（环形缓冲，超出丢最老的）。
const maxLines = 400

// Task 是一个任务。字段由 mu 保护，可并发读写。
type Task struct {
	id        string
	kind      string
	title     string
	startedAt time.Time

	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	status     Status
	finishedAt time.Time
	errMsg     string
	result     any
	lines      []Line
	nextSeq    int64
	total      int64
}

// ID 返回任务 id。
func (t *Task) ID() string { return t.id }

// Done 在任务结束时关闭。
func (t *Task) Done() <-chan struct{} { return t.done }

// Status 返回当前状态。
func (t *Task) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// Log 追加进度（多行会拆开）。
func (t *Task) Log(text string) {
	for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		t.mu.Lock()
		t.nextSeq++
		t.total++
		t.lines = append(t.lines, Line{Seq: t.nextSeq, At: time.Now(), Level: "info", Text: ln})
		if len(t.lines) > maxLines {
			t.lines = append(t.lines[:0], t.lines[len(t.lines)-maxLines:]...)
		}
		t.mu.Unlock()
	}
}

// Snapshot 是给前端的任务快照。
type Snapshot struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Status     Status    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	ElapsedMS  int64     `json:"elapsed_ms"`
	LineCount  int64     `json:"line_count"`
	Last       string    `json:"last"`
	Logs       []Line    `json:"logs"`
	Result     any       `json:"result,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Snapshot 返回当前快照。
func (t *Task) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	end := t.finishedAt
	if end.IsZero() {
		end = time.Now()
	}
	s := Snapshot{
		ID: t.id, Kind: t.kind, Title: t.title, Status: t.status,
		StartedAt: t.startedAt, ElapsedMS: end.Sub(t.startedAt).Milliseconds(),
		LineCount: t.total, Result: t.result, Error: t.errMsg,
		Logs: append([]Line{}, t.lines...),
	}
	if !t.finishedAt.IsZero() {
		s.FinishedAt = t.finishedAt
	}
	if n := len(t.lines); n > 0 {
		s.Last = t.lines[n-1].Text
	}
	return s
}

func (t *Task) finish(st Status, err error, result any) {
	t.mu.Lock()
	if t.status != Running {
		t.mu.Unlock()
		return
	}
	t.status = st
	t.finishedAt = time.Now()
	t.result = result
	if err != nil {
		t.errMsg = err.Error()
	}
	t.mu.Unlock()
	close(t.done)
}

// Manager 持有最近的任务（新的在前）。可并发使用。
type Manager struct {
	mu    sync.Mutex
	tasks []*Task
	seq   atomic.Int64
	keep  int
}

// NewManager 建立任务中心。keep<=0 时默认保留 200 个。
func NewManager() *Manager { return &Manager{keep: 200} }

// Start 启动任务并立刻返回。fn 里的 err 会如实记进任务（不谎报成功）。
func (m *Manager) Start(kind, title string, fn func(ctx context.Context, log Logger) (any, error)) *Task {
	id := fmt.Sprintf("t-%d-%d", time.Now().UnixMilli(), m.seq.Add(1))
	t := &Task{
		id: id, kind: kind, title: title,
		startedAt: time.Now(), done: make(chan struct{}), status: Running,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	m.mu.Lock()
	m.tasks = append([]*Task{t}, m.tasks...)
	if m.keep > 0 && len(m.tasks) > m.keep {
		m.tasks = m.tasks[:m.keep]
	}
	m.mu.Unlock()

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.finish(Failed, fmt.Errorf("内部错误（panic）：%v", rec), nil)
			}
		}()
		res, err := fn(ctx, t.Log)
		switch {
		case err == nil:
			t.finish(Succeeded, nil, res)
		case ctx.Err() == context.Canceled:
			t.finish(Canceled, err, res)
		default:
			t.finish(Failed, err, res)
		}
	}()
	return t
}

// Get 按 id 取任务。
func (m *Manager) Get(id string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.id == id {
			return t, true
		}
	}
	return nil, false
}

// List 返回快照列表（新的在前）。
func (m *Manager) List(limit int) []Snapshot {
	m.mu.Lock()
	tasks := append([]*Task{}, m.tasks...)
	m.mu.Unlock()
	if limit <= 0 || limit > len(tasks) {
		limit = len(tasks)
	}
	out := make([]Snapshot, 0, limit)
	for _, t := range tasks[:limit] {
		out = append(out, t.Snapshot())
	}
	return out
}

// Cancel 取消任务；任务不存在或已结束都返回 false。
func (m *Manager) Cancel(id string) bool {
	t, ok := m.Get(id)
	if !ok || t.Status() != Running {
		return false
	}
	t.cancel()
	return true
}

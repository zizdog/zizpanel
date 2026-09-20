// history.go —— 上一次申请的真实结果（GET 的状态显示靠它）。
//
// 为什么不探测现状：读一次受保护路径就会触发弹窗/记 denial（坑 191），
// 所以"当前授权到底有没有"只由用户上一次显式申请的结果回答；没有记录就 unknown。
package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry 是一次申请的真实结果。
type Entry struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	Result string    `json:"result"`
}

// History 是权限申请的落盘历史（一个 JSON 文件，0600）。
type History struct {
	mu   sync.Mutex
	path string
	m    map[string]Entry
}

type historyFile struct {
	Items map[string]Entry `json:"items"`
}

// historyCache 按路径共享实例：每次请求新建会让并发写互相覆盖（丢历史）。
var historyCache sync.Map

// HistoryFor 返回某个落盘路径对应的共享 History（path 为空则只驻内存）。
func HistoryFor(path string) *History {
	if path == "" {
		return NewHistory("")
	}
	if v, ok := historyCache.Load(path); ok {
		return v.(*History)
	}
	h := NewHistory(path)
	actual, _ := historyCache.LoadOrStore(path, h)
	return actual.(*History)
}

// NewHistory 读入已有历史；文件不存在/坏掉都只是"没有历史"，绝不报错阻塞页面。
func NewHistory(path string) *History {
	h := &History{path: path, m: map[string]Entry{}}
	if path == "" {
		return h
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return h
	}
	var f historyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return h
	}
	for k, v := range f.Items {
		h.m[k] = v
	}
	return h
}

// Last 返回某一项最近一次申请的结果。
func (h *History) Last(id string) (Entry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.m[id]
	return e, ok
}

// Record 记录一次结果；写盘失败只返回错误（调用方不因此谎报申请结果）。
func (h *History) Record(e Entry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	h.mu.Lock()
	h.m[e.ID] = e
	raw, err := h.snapshotLocked()
	h.mu.Unlock()
	if err != nil || h.path == "" {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return err
	}
	// 先写临时文件再原子替换：崩在写一半也不会留下坏 JSON。
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

func (h *History) snapshotLocked() ([]byte, error) {
	f := historyFile{Items: map[string]Entry{}}
	for k, v := range h.m {
		f.Items[k] = v
	}
	return json.MarshalIndent(f, "", "  ")
}

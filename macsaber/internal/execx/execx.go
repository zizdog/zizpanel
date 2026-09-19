// Package execx 是 mac军刀 唯一的命令执行入口。
//
// 结论：只用 exec.CommandContext 逐参传，绝不 sh -c 拼用户输入（坑 B1）；
// 超时用进程组 kill（macOS 上 kill(-pgid) 才能带走子进程，坑 B2）；
// 输出按上限截断并在结果里明说被截断（坑 B3）。
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Result 是一次命令执行的完整事实：不吞错、不谎报成功。
type Result struct {
	Argv     []string
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	TruncOut bool
	TruncErr bool
	Duration time.Duration
}

// Output 返回给用户看的最佳文本（有 stdout 用 stdout，否则用 stderr）。
func (r *Result) Output() string {
	if strings.TrimSpace(r.Stdout) != "" {
		return r.Stdout
	}
	return r.Stderr
}

// Execer 是执行器：可并发使用。
type Execer struct {
	// MaxOutputBytes 是 stdout / stderr 各自的截断上限。
	MaxOutputBytes int
	// DefaultTimeout 为 0 时用 defaultTimeout。
	DefaultTimeout time.Duration
}

// New 建立执行器。
func New() *Execer { return &Execer{MaxOutputBytes: 1 << 20} }

const defaultTimeout = 60 * time.Second

type limitWriter struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.limit = 1 << 20
	}
	if w.buf.Len() < w.limit {
		n := w.limit - w.buf.Len()
		if n > len(p) {
			n = len(p)
		}
		w.buf.Write(p[:n])
	}
	if w.buf.Len() >= w.limit {
		w.over = true
	}
	// 永远报告"全部收下"，否则执行体会把超限当成写失败。
	return len(p), nil
}

func (w *limitWriter) String() string { return w.buf.String() }

// Run 执行命令。ctx 取消或超时都会**杀掉整个进程组**。
// name 必须是绝对路径或 PATH 内的名字；args 一律作为独立 argv 传递。
func (e *Execer) Run(ctx context.Context, timeout time.Duration, name string, args ...string) *Result {
	if e == nil {
		e = New()
	}
	if timeout <= 0 {
		timeout = e.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, name, args...)
	// 独立进程组 + 手动 kill：CommandContext 默认只杀直接子进程，
	// 子进程再 fork 出来的（shell 脚本里的 sleep）会活下来（坑 B2）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil
	}
	cmd.WaitDelay = 3 * time.Second

	outW := &limitWriter{limit: e.MaxOutputBytes}
	errW := &limitWriter{limit: e.MaxOutputBytes}
	cmd.Stdout = outW
	cmd.Stderr = errW

	res := &Result{Argv: append([]string{name}, args...)}
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout = outW.String()
	res.Stderr = errW.String()
	res.TruncOut = outW.over
	res.TruncErr = errW.over

	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			if res.Stderr == "" {
				res.Stderr = err.Error()
			}
		}
		if cctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
			res.ExitCode = -1
			res.Stderr = strings.TrimRight(res.Stderr, "\n") +
				fmt.Sprintf("\n命令超过 %s 被强制终止", timeout)
		}
	}
	return res
}

// LookPath 是"便宜探测"：命令是否存在。首屏只允许用这一类判据。
func LookPath(name string) (string, bool) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	return p, true
}

// ProbeCache 缓存探测结果（进程内，带 TTL）。
//
// 首屏不许跑昂贵探测（Quartz / Vision 之类），所以昂贵结论只在用户真点工具时算，
// 算完缓存在这里，避免连点反复付代价。
type ProbeCache struct {
	mu      sync.Mutex
	entries map[string]probeEntry
	now     func() time.Time
}

type probeEntry struct {
	ok      bool
	reason  string
	expires time.Time
}

// NewProbeCache 建立探测缓存。
func NewProbeCache() *ProbeCache {
	return &ProbeCache{entries: map[string]probeEntry{}, now: time.Now}
}

// TTLOf 决定缓存时长：成功的结论可以放久一点。
func TTLOf(ok bool) time.Duration {
	if ok {
		return 10 * time.Minute
	}
	return 2 * time.Minute
}

// Get 取缓存结论。
func (c *ProbeCache) Get(key string) (ok bool, reason string, found bool) {
	if c == nil {
		return false, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, hit := c.entries[key]
	if !hit || c.now().After(e.expires) {
		return false, "", false
	}
	return e.ok, e.reason, true
}

// Put 写缓存结论。
func (c *ProbeCache) Put(key string, ok bool, reason string, ttl time.Duration) {
	if c == nil {
		return
	}
	if ttl <= 0 {
		ttl = TTLOf(ok)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = probeEntry{ok: ok, reason: reason, expires: c.now().Add(ttl)}
}

// Framework 探测原生框架是否可用（如 python3 -c "import Quartz"、JXA Vision）。
//
// 这是**昂贵**探测：只在用户真点这个工具时调用（坑 B4）。
func (e *Execer) Framework(ctx context.Context, cache *ProbeCache, key, name string, args ...string) (bool, string) {
	if cache != nil {
		if ok, reason, hit := cache.Get(key); hit {
			return ok, reason
		}
	}
	if _, ok := LookPath(name); !ok {
		reason := "缺少命令 " + name
		cache.Put(key, false, reason, 0)
		return false, reason
	}
	res := e.Run(ctx, 15*time.Second, name, args...)
	if res.ExitCode == 0 {
		cache.Put(key, true, "", 0)
		return true, ""
	}
	reason := "原生框架不可用：" + firstLine(res.Output())
	if reason == "原生框架不可用：" {
		reason = "原生框架不可用（未返回原因）"
	}
	cache.Put(key, false, reason, 0)
	return false, reason
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

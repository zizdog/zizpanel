package videoopt

import (
	"path/filepath"
	"sync"
	"time"
)

// ============================================================================
//  ffprobe 结论的进程级缓存
//
//  用户报障：改一次压缩配置就要重新「正在读取目录与视频信息…」等很久 —— 因为
//  每次改配置都重新请求 /video-plan，而它在服务端对每个视频重跑 ffprobe。
//
//  修法：探测结论按「绝对路径 + size + mtime」缓存，配置变更只做纯函数重算
//  （0 次 ffprobe）；「⟳ 重新扫描」显式让这个目录失效。
//
//  为什么挂在 web.Server 上经 PlanRequest 注入：web 层每次请求都新建 Manager
//  （本项目已知陷阱，同 StartHintStore / HealthCache）—— 缓存挂在 Manager 上
//  活不过一次请求。方法容忍 nil 接收者：没注入 = 每次现探，绝不报错。
// ============================================================================

// DefaultProbeTTL 是探测结论的默认存活期；超期必须重探（绝不把很久以前的
// 结论当成现在的）。文件 size/mtime 一变也是未命中，与 TTL 相互独立。
const DefaultProbeTTL = 10 * time.Minute

// DefaultProbeMaxEntries 是条目上限：面板一次只看一个目录，2000 条足够覆盖
// 来回切换的目录；超限按最近使用淘汰，防止长时间运行把内存吃掉。
const DefaultProbeMaxEntries = 2000

// ProbeCache 是 ffprobe 结论的进程级缓存（跨请求）。
type ProbeCache struct {
	mu      sync.Mutex
	entries map[string]probeEntry
	ttl     time.Duration
	max     int
}

type probeEntry struct {
	info     MediaInfo
	size     int64
	modTime  time.Time
	at       time.Time
	lastUsed time.Time
}

// NewProbeCache 造一份缓存；ttl / max 非正时取默认值。
func NewProbeCache(ttl time.Duration, max int) *ProbeCache {
	if ttl <= 0 {
		ttl = DefaultProbeTTL
	}
	if max <= 0 {
		max = DefaultProbeMaxEntries
	}
	return &ProbeCache{entries: map[string]probeEntry{}, ttl: ttl, max: max}
}

// Get 按当前的存储指纹取结论；未命中（没探过 / 过期 / size 或 mtime 变了）
// 一律返回 false —— 调用方必须重新探测，**绝不拿旧数据糊**。
func (c *ProbeCache) Get(path string, size int64, modTime time.Time) (MediaInfo, bool) {
	if c == nil {
		return MediaInfo{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[path]
	if !ok || e.size != size || !e.modTime.Equal(modTime) {
		return MediaInfo{}, false
	}
	if time.Since(e.at) > c.ttl {
		delete(c.entries, path)
		return MediaInfo{}, false
	}
	e.lastUsed = time.Now()
	c.entries[path] = e
	return e.info, true
}

// Put 记录一次探测结论（连同这次的存储指纹）。
func (c *ProbeCache) Put(path string, size int64, modTime time.Time, info MediaInfo) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.evictLocked()
	}
	now := time.Now()
	c.entries[path] = probeEntry{info: info, size: size, modTime: modTime, at: now, lastUsed: now}
}

// InvalidateDir 丢掉某个目录**这一层**的全部结论，返回丢掉的条数。
//
// 「⟳ 重新扫描」按钮走这里：用户明说"文件变了但 size/mtime 没变"时要能强制重探。
func (c *ProbeCache) InvalidateDir(dir string) int {
	if c == nil {
		return 0
	}
	clean := filepath.Clean(dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for p := range c.entries {
		// 只失效**该目录里**的文件；子目录不共享（扫目录本身非递归）。
		if filepath.Dir(p) == clean {
			delete(c.entries, p)
			n++
		}
	}
	return n
}

// Len 返回当前条目数（门禁与日志用；TTL 过期的条目也算在内，直到被取用/淘汰）。
func (c *ProbeCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// evictLocked 先清过期条目，再丢一个最久没用过的（调用方已持锁）。
func (c *ProbeCache) evictLocked() {
	now := time.Now()
	for p, e := range c.entries {
		if now.Sub(e.at) > c.ttl {
			delete(c.entries, p)
		}
	}
	if len(c.entries) < c.max {
		return
	}
	oldest, oldestAt := "", time.Time{}
	for p, e := range c.entries {
		if oldest == "" || e.lastUsed.Before(oldestAt) {
			oldest, oldestAt = p, e.lastUsed
		}
	}
	if oldest != "" {
		delete(c.entries, oldest)
	}
}

// ============================================================================
//  外部命令版本串的进程级缓存
//
//  为什么要它：/video-plan 每次请求都会 spawn 一次 `ffmpeg -version` 只为填响应里的
//  engine 字段；改配置走缓存后（0 次 ffprobe）它反而成了热路径上最大的开销。
//  版本串只在用户装/升级 ffmpeg 时变，缓存一小段时间完全够（读不到就不缓存，下次再试）。
//  nil 接收者 = 每次都问（没注入也不报错）。
// ============================================================================

// VersionCache 是"读一次外部命令版本串"的小缓存。
type VersionCache struct {
	mu  sync.Mutex
	v   string
	at  time.Time
	ttl time.Duration
}

// NewVersionCache 造一份版本串缓存；ttl 非正时取 DefaultProbeTTL。
func NewVersionCache(ttl time.Duration) *VersionCache {
	if ttl <= 0 {
		ttl = DefaultProbeTTL
	}
	return &VersionCache{ttl: ttl}
}

// Get 返回版本串：命中就直接给，否则调 fetch 并把**非空**结果缓存起来。
func (c *VersionCache) Get(fetch func() string) string {
	if c == nil {
		return fetch()
	}
	c.mu.Lock()
	if c.v != "" && time.Since(c.at) < c.ttl {
		v := c.v
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	v := fetch()
	if v == "" {
		return ""
	}
	c.mu.Lock()
	c.v, c.at = v, time.Now()
	c.mu.Unlock()
	return v
}

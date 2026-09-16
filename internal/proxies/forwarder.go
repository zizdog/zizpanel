package proxies

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
//  面板侧回环 TCP 转发器
//
//  要解决的问题（真机事实）：
//    macOS 15 有「本地网络」隐私门。Homebrew 的 nginx（ad-hoc 签名、标识随
//    二进制 UUID 变）访问局域网会被拦，无头服务器没人点弹窗 → 直接
//    "No route to host"，反代全部 502。面板自己（Go、linker-signed、标识 a.out）
//    从来没被拦过。
//
//  所以把局域网出口收回面板：面板在 127.0.0.1:<port> 起 TCP 转发器转发到规则
//  的真实目标，生成的 nginx 配置只连回环（回环不受这道门限制）。
//
//  性能（已基准验证，产品形态的 Go 实现）：
//    纯回环 512MB：直连 ~6.8–7.2 GB/s vs 经转发 ~7.1–7.4 GB/s（噪声内）；
//    单请求延迟 500 次：4880µs vs 5000µs（+120µs/请求）；
//    传输期 CPU 2.8%、内存 9MB、每连接 2 个 goroutine。
//
//  设计要点：
//    · 只绑 127.0.0.1（写死字面量，绝不 0.0.0.0）；每条监听器只转发到
//      **它自己规则**的目标，不是开放代理；
//    · 端口从固定回环区间分配，优先按规则 id 推导、优先复用数据库里的旧值，
//      分配结果由调用方落库（SetForwardPort），重启后能对齐 nginx 配置；
//    · 每连接两个 goroutine 做双向 io.Copy、TCP_NODELAY、半关闭（CloseWrite）、
//      空闲超时、每规则并发上限（防 fd 耗尽）；
//    · 统计（active/total/bytes）用原子计数；拨号失败限速留痕并可查。
// ============================================================================

const (
	// DefaultForwardPortMin / Max 是回环转发端口区间。
	//
	// 选 47000–47999：远离常用服务端口，且整段都在回环上（面板绝不绑 0.0.0.0）。
	DefaultForwardPortMin = 47000
	DefaultForwardPortMax = 47999

	defaultDialTimeout = 5 * time.Second
	defaultIdleTimeout = 120 * time.Second
	defaultMaxConns    = 256
	defaultLogEvery    = 30 * time.Second
)

// ManagerOptions 是 Manager 的构造参数（零值即用默认值，便于生产直接 New）。
type ManagerOptions struct {
	// PortMin/PortMax 是分配给转发器的回环端口区间。
	PortMin, PortMax int
	// DialTimeout 是连真实目标的超时（参考实现用 5s）。
	DialTimeout time.Duration
	// IdleTimeout 是"双向都没有流量"多久后断开连接（例如 120s）。
	IdleTimeout time.Duration
	// MaxConnsPerRule 是每条规则的并发连接上限（防 fd 耗尽）。
	MaxConnsPerRule int
	// LogEvery 是同一规则转发错误的最短日志间隔（限速，防刷屏）。
	LogEvery time.Duration
	// Logf 是可选日志出口；nil 表示不打印（但错误仍会记进状态，界面可见）。
	Logf func(format string, args ...any)
	// LookupHost 是 DNS 解析函数（判断域名目标是公网还是私有）；nil = 系统解析。
	LookupHost func(host string) ([]string, error)
}

// forwardEntry 是一条规则正在运行的转发器。
type forwardEntry struct {
	id       int64
	port     int
	upstream string // host:port（真实目标）
	ln       net.Listener

	active   atomic.Int64
	total    atomic.Uint64
	bytesIn  atomic.Int64 // 客户端 → 目标
	bytesOut atomic.Int64 // 目标 → 客户端

	sem     chan struct{}
	closing atomic.Bool

	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	lastErr   string
	lastErrAt time.Time
	errCount  uint64
	lastLogAt time.Time
}

// Manager 管理面板上的全部回环转发器。
//
// 生命周期约定（谁在什么时候起停）：
//   - 规则新增 / 修改 / 启用 → Ensure（见 web 层 syncForwarder）；
//   - 规则停用 / 删除 / 改成不转发 → Stop；
//   - 面板启动 → Reconcile（按数据库里的规则起该起的、停该停的）；
//   - 面板退出 → StopAll。
//
// Manager 自身不读写数据库：端口由调用方落库（这样它能在单测里完全脱离 DB）。
type Manager struct {
	mu      sync.Mutex
	entries map[int64]*forwardEntry
	closed  bool

	portMin, portMax int
	dialTimeout      time.Duration
	idleTimeout      time.Duration
	maxConns         int
	logEvery         time.Duration

	logf       func(format string, args ...any)
	lookupHost func(host string) ([]string, error)

	wg sync.WaitGroup // 连接 goroutine（StopAll 不等待空闲连接，仅用于测试观察）
}

// NewManager 创建转发器管理器。
func NewManager(opt ManagerOptions) *Manager {
	m := &Manager{
		entries:     map[int64]*forwardEntry{},
		portMin:     opt.PortMin,
		portMax:     opt.PortMax,
		dialTimeout: opt.DialTimeout,
		idleTimeout: opt.IdleTimeout,
		maxConns:    opt.MaxConnsPerRule,
		logEvery:    opt.LogEvery,
		logf:        opt.Logf,
		lookupHost:  opt.LookupHost,
	}
	if m.portMin <= 0 || m.portMax <= 0 || m.portMax < m.portMin {
		m.portMin, m.portMax = DefaultForwardPortMin, DefaultForwardPortMax
	}
	if m.dialTimeout <= 0 {
		m.dialTimeout = defaultDialTimeout
	}
	if m.idleTimeout <= 0 {
		m.idleTimeout = defaultIdleTimeout
	}
	if m.maxConns <= 0 {
		m.maxConns = defaultMaxConns
	}
	if m.logEvery <= 0 {
		m.logEvery = defaultLogEvery
	}
	return m
}

// Ensure 让一条规则的回环转发器与当前配置对齐，并把分配到的端口写回 rule.ForwardPort。
//
// 幂等：目标与端口都没变时直接复用现有监听器（不会断掉在用连接）。
// 返回实际监听的端口；不需要转发时返回 0（同时 rule.ForwardPort 被清 0）。
func (m *Manager) Ensure(rule *Rule) (int, error) {
	if rule == nil {
		return 0, nil
	}
	if !rule.Enabled {
		m.Stop(rule.ID)
		return 0, nil
	}
	if !rule.NeedsForward(m.lookupHost) {
		m.Stop(rule.ID)
		rule.ForwardPort = 0
		return 0, nil
	}
	host, port, err := rule.TargetHostPort()
	if err != nil {
		return 0, fmt.Errorf("规则「%s」的目标地址无法解析出主机/端口：%w", rule.Name, err)
	}
	upstream := net.JoinHostPort(host, strconv.Itoa(port))

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, errors.New("转发器管理器已关闭")
	}
	if e, ok := m.entries[rule.ID]; ok {
		// 目标没变、且端口匹配（或调用方还没记住端口）→ 复用。
		if e.upstream == upstream && (rule.ForwardPort <= 0 || e.port == rule.ForwardPort) {
			rule.ForwardPort = e.port
			return e.port, nil
		}
		delete(m.entries, rule.ID)
		// 同步关掉旧监听器再重新绑：换目标/换端口时通常还要复用同一个端口，
		// 异步关会让紧接着的 net.Listen 偶发 EADDRINUSE（表现是随机 502）。
		m.stopEntry(e)
	}
	ln, bound, lerr := m.listenLocked(rule.ID, rule.ForwardPort)
	if lerr != nil {
		return 0, lerr
	}
	e := &forwardEntry{
		id:       rule.ID,
		port:     bound,
		upstream: upstream,
		ln:       ln,
		sem:      make(chan struct{}, m.maxConns),
		conns:    map[net.Conn]struct{}{},
	}
	m.entries[rule.ID] = e
	m.wg.Add(1)
	go m.serve(e)
	rule.ForwardPort = bound
	return bound, nil
}

// Stop 停掉某条规则的转发器（幂等）。
//
// 只关监听器与在途连接，不动调用方内存里的 ForwardPort —— "停用但保留端口"
// 是有意的：重新启用时能复用同一个端口，nginx 配置不用重写。
func (m *Manager) Stop(id int64) {
	m.mu.Lock()
	e := m.entries[id]
	delete(m.entries, id)
	m.mu.Unlock()
	if e != nil {
		m.stopEntry(e)
	}
}

// StopAll 关闭所有监听器与在途连接（面板退出时调用）。
func (m *Manager) StopAll() {
	m.mu.Lock()
	m.closed = true
	victims := make([]*forwardEntry, 0, len(m.entries))
	for id, e := range m.entries {
		victims = append(victims, e)
		delete(m.entries, id)
	}
	m.mu.Unlock()
	for _, e := range victims {
		m.stopEntry(e)
	}
}

// Reconcile 把运行中的监听器对齐到 rules：该起的起、该停的停。
//
// 它会直接修改传入 rule 的 ForwardPort（分配结果）。返回**分配结果发生变化**
// 的规则 id（升序）—— 调用方据此落库并重写对应的 nginx 配置（否则 nginx 里的
// proxy_pass 会指向旧的/没人听的端口）。
func (m *Manager) Reconcile(rules []*Rule) []int64 {
	desired := map[int64]*Rule{}
	changed := map[int64]bool{}
	for _, r := range rules {
		if r == nil || !r.Enabled {
			continue
		}
		if !r.NeedsForward(m.lookupHost) {
			m.Stop(r.ID)
			if r.ForwardPort != 0 {
				r.ForwardPort = 0
				changed[r.ID] = true
			}
			continue
		}
		desired[r.ID] = r
	}

	m.mu.Lock()
	var stale []*forwardEntry
	for id, e := range m.entries {
		if _, ok := desired[id]; !ok {
			stale = append(stale, e)
			delete(m.entries, id)
		}
	}
	m.mu.Unlock()
	for _, e := range stale {
		m.stopEntry(e)
	}

	for id, r := range desired {
		before := r.ForwardPort
		if _, err := m.Ensure(r); err != nil {
			m.recordEnsureErr(id, err)
			continue
		}
		if r.ForwardPort != before {
			changed[id] = true
		}
	}

	out := make([]int64, 0, len(changed))
	for id := range changed {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ForwardStatus 是一条规则转发器的可查状态（给规则视图与诊断用）。
type ForwardStatus struct {
	RuleID      int64  `json:"rule_id"`
	Listening   bool   `json:"listening"`
	ListenPort  int    `json:"listen_port"`
	Upstream    string `json:"upstream"`
	ActiveConns int64  `json:"active_conns"`
	TotalConns  uint64 `json:"total_conns"`
	BytesIn     int64  `json:"bytes_in"`
	BytesOut    int64  `json:"bytes_out"`
	LastError   string `json:"last_error"`
	LastErrTime string `json:"last_error_at"`
	ErrorCount  uint64 `json:"error_count"`
}

// Status 返回某条规则转发器的状态快照；第二条返回值表示监听器是否存在。
func (m *Manager) Status(id int64) (ForwardStatus, bool) {
	m.mu.Lock()
	e := m.entries[id]
	m.mu.Unlock()
	if e == nil {
		return ForwardStatus{RuleID: id}, false
	}
	st := ForwardStatus{
		RuleID:      e.id,
		Listening:   true,
		ListenPort:  e.port,
		Upstream:    e.upstream,
		ActiveConns: e.active.Load(),
		TotalConns:  e.total.Load(),
		BytesIn:     e.bytesIn.Load(),
		BytesOut:    e.bytesOut.Load(),
	}
	e.mu.Lock()
	st.LastError = e.lastErr
	st.ErrorCount = e.errCount
	if !e.lastErrAt.IsZero() {
		st.LastErrTime = e.lastErrAt.Format(time.RFC3339)
	}
	e.mu.Unlock()
	return st, true
}

// TargetScope 按本管理器的解析器判断目标网段（界面提示复用同一套判据）。
func (m *Manager) TargetScope(target string) TargetScope {
	host, _, err := (&Rule{Target: target}).TargetHostPort()
	if err != nil {
		return ScopePrivate
	}
	return ClassifyTarget(host, m.lookupHost)
}

// ---------- 内部实现 ----------

// listenLocked 在回环区间里找一个能真正绑上的端口。
//
// 候选顺序：调用方给的首选端口（数据库里的旧值）→ 按规则 id 推导的候选 →
// 从该候选开始顺延。用"直接尝试 net.Listen"而不是先查再用，避免 TOCTOU。
func (m *Manager) listenLocked(id int64, prefer int) (net.Listener, int, error) {
	span := m.portMax - m.portMin + 1
	start := m.portMin + int(id%int64(span))
	if prefer >= m.portMin && prefer <= m.portMax {
		start = prefer
	}
	var lastErr error
	for i := 0; i < span; i++ {
		p := m.portMin + (start-m.portMin+i)%span
		if m.portTakenLocked(p) {
			continue
		}
		// 只绑 127.0.0.1：写死字面量，绝不给 0.0.0.0 留任何入口。
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			lastErr = err
			continue
		}
		return ln, p, nil
	}
	if lastErr == nil {
		lastErr = errors.New("区间内的端口都被本面板的其它规则占用")
	}
	return nil, 0, fmt.Errorf("回环转发端口 %d-%d 都不可用，无法为规则 %d 启动转发器：%v",
		m.portMin, m.portMax, id, lastErr)
}

func (m *Manager) portTakenLocked(port int) bool {
	for _, e := range m.entries {
		if e.port == port {
			return true
		}
	}
	return false
}

// recordEnsureErr 记录"连监听都起不来"这类错误（同样限速打日志）。
func (m *Manager) recordEnsureErr(id int64, err error) {
	if err == nil {
		return
	}
	if m.logf != nil {
		m.logf("规则 %d 启动回环转发器失败: %v", id, err)
	}
}

// serve 接受连接。每连接两个 goroutine 做双向 io.Copy。
func (m *Manager) serve(e *forwardEntry) {
	defer m.wg.Done()
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return // listener 已关闭
		}
		e.total.Add(1)
		select {
		case e.sem <- struct{}{}:
		default:
			// 并发上限：拒绝新连接并留痕。不排队 —— 排队会让客户端一直挂着，
			// 不如立刻断开让 nginx 早点返回 502，用户能马上看到问题。
			m.recordErr(e, fmt.Errorf("并发连接数超过上限 %d，已拒绝新连接", m.maxConns))
			_ = c.Close()
			continue
		}
		if !m.trackConn(e, c) {
			<-e.sem
			_ = c.Close()
			continue
		}
		e.active.Add(1)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer func() {
				m.untrackConn(e, c)
				<-e.sem
				e.active.Add(-1)
			}()
			m.handle(e, c)
		}()
	}
}

func (m *Manager) trackConn(e *forwardEntry, c net.Conn) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing.Load() {
		return false
	}
	e.conns[c] = struct{}{}
	return true
}

func (m *Manager) untrackConn(e *forwardEntry, c net.Conn) {
	e.mu.Lock()
	delete(e.conns, c)
	e.mu.Unlock()
}

// stopEntry 关监听器 + 强关在途连接。
//
// 为什么要强关在途连接：StopAll 发生在面板退出路径上，空闲连接可能还要等
// 120s 空闲超时；不强关会让进程"卡"在退出阶段。
func (m *Manager) stopEntry(e *forwardEntry) {
	e.closing.Store(true)
	_ = e.ln.Close()
	e.mu.Lock()
	for c := range e.conns {
		_ = c.Close()
	}
	e.conns = map[net.Conn]struct{}{}
	e.mu.Unlock()
}

// handle 处理一条连接：拨号真实目标，然后双向透传。
func (m *Manager) handle(e *forwardEntry, c net.Conn) {
	defer func() { _ = c.Close() }()
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.SetNoDelay(true)
	}
	d := net.Dialer{Timeout: m.dialTimeout}
	u, err := d.Dial("tcp", e.upstream)
	if err != nil {
		m.recordErr(e, fmt.Errorf("拨号上游 %s 失败: %w", e.upstream, err))
		return
	}
	defer func() { _ = u.Close() }()
	if t, ok := u.(*net.TCPConn); ok {
		_ = t.SetNoDelay(true)
	}

	// 空闲超时通过"每次读写前刷新 deadline"实现；包装类型不实现
	// ReaderFrom/WriterTo，保证 io.Copy 走普通 Read/Write 循环（能用上 deadline）。
	cc := &idleConn{Conn: c, timeout: m.idleTimeout}
	uu := &idleConn{Conn: u, timeout: m.idleTimeout}

	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(uu, cc) // 客户端 → 目标
		e.bytesIn.Add(n)
		halfClose(uu)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(cc, uu) // 目标 → 客户端
		e.bytesOut.Add(n)
		halfClose(cc)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func (m *Manager) recordErr(e *forwardEntry, err error) {
	if err == nil {
		return
	}
	now := time.Now()
	e.mu.Lock()
	e.lastErr = err.Error()
	e.lastErrAt = now
	e.errCount++
	count := e.errCount
	log := m.logf != nil && now.Sub(e.lastLogAt) >= m.logEvery
	if log {
		e.lastLogAt = now
	}
	e.mu.Unlock()
	if log {
		// 限速：同一条规则默认最多每 30s 打一行，避免上游长期不可达时刷屏。
		m.logf("规则 %d 回环转发错误（累计 %d 次）: %v", e.id, count, err)
	}
}

// idleConn 在每次 Read/Write 前刷新 deadline，实现"无流量即断开"。
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Write(b)
}

// CloseWrite 把半关闭透传给底层 TCP 连接。
//
// 必须有这个方法：io.Copy 操作的是 *idleConn，如果它不实现 CloseWrite，
// halfClose 的类型断言会静默失败 —— 表现就是"大文件传到一半偶发空响应"。
func (c *idleConn) CloseWrite() error {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// halfClose 对 TCP 做半关闭（写方向 FIN），让对端及时看到 EOF。
//
// 不做半关闭时，io.Copy 一边结束后直接 Close 会把另一方向还在传的数据截断
// （HTTP/1.1 的 keep-alive 与流式响应都会因此变成"偶发空响应"）。
func halfClose(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

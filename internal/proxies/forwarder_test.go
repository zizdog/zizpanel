package proxies

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================================
//  回环 TCP 转发器的单测
//
//  全部跑在 127.0.0.1 上：上游是测试自己 net.Listen 起来的假服务。
//  不连镜像站、不连 192.168.1.8、不碰真实 nginx —— AGENTS.md 的硬性约定。
// ============================================================================

// newTestManager 造一个端口区间与生产不同的管理器，并在测试结束关掉所有监听器。
func newTestManager(t *testing.T, opt ManagerOptions) *Manager {
	t.Helper()
	if opt.PortMin == 0 {
		opt.PortMin = 48100
	}
	if opt.PortMax == 0 {
		opt.PortMax = 48199
	}
	m := NewManager(opt)
	t.Cleanup(m.StopAll)
	return m
}

// startUpstream 起一个 127.0.0.1 上的假上游，返回它的 host:port。
func startUpstream(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	return ln.Addr().String()
}

// forwardRule 造一条"强制经面板转发"的规则（目标是回环假上游，auto 不会转，
// 所以测试统一用 on 强制）。
func forwardRule(id int64, upstream string) *Rule {
	return &Rule{ID: id, Name: fmt.Sprintf("规则%d", id), Listen: 18080,
		Target: "http://" + upstream, Enabled: true, LANForward: LANForwardOn}
}

func dialForwarder(t *testing.T, port int) *net.TCPConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatalf("连不上转发器 127.0.0.1:%d：%v", port, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*net.TCPConn)
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestForwarderBidirectionalAndHalfClose：字节双向透传 + 半关闭（CloseWrite）。
//
// 半关闭是这里最容易被忽略、也最容易出故障的一点：客户端写完就半关闭时，
// 上游必须立刻看到 EOF 才能把响应写回来；不做 CloseWrite 会偶发空响应。
func TestForwarderBidirectionalAndHalfClose(t *testing.T) {
	upstream := startUpstream(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		b, _ := io.ReadAll(c) // 客户端半关闭后才返回
		_, _ = c.Write(append([]byte("reply:"), b...))
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	})
	m := newTestManager(t, ManagerOptions{})
	rule := forwardRule(1, upstream)
	port, err := m.Ensure(rule)
	if err != nil {
		t.Fatalf("Ensure 失败：%v", err)
	}
	if port <= 0 {
		t.Fatal("Ensure 应当分配一个回环端口")
	}

	c := dialForwarder(t, port)
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("读响应失败：%v", err)
	}
	if string(got) != "reply:ping" {
		t.Fatalf("透传结果 = %q，期望 %q", got, "reply:ping")
	}

	// 统计：bytes_in 是"客户端 → 目标"，bytes_out 是"目标 → 客户端"。
	waitUntil(t, 2*time.Second, "统计落到 bytes_in=4/bytes_out>0", func() bool {
		st, ok := m.Status(rule.ID)
		return ok && st.BytesIn == 4 && st.BytesOut == int64(len("reply:ping")) && st.TotalConns >= 1
	})
	st, ok := m.Status(rule.ID)
	if !ok || !st.Listening {
		t.Fatalf("Status 应报告正在监听：%+v ok=%v", st, ok)
	}
	if st.ListenPort != port {
		t.Errorf("Status.ListenPort = %d，期望 %d", st.ListenPort, port)
	}
	if st.Upstream != upstream {
		t.Errorf("Status.Upstream = %q，期望 %q", st.Upstream, upstream)
	}
}

// TestForwarderOnlyListensOnLoopback：只绑 127.0.0.1，绝不 0.0.0.0。
//
// 这是安全边界：转发器要是绑在 0.0.0.0 上，它就变成一个人人可用的开放代理。
func TestForwarderOnlyListensOnLoopback(t *testing.T) {
	upstream := startUpstream(t, func(c net.Conn) { _ = c.Close() })
	m := newTestManager(t, ManagerOptions{})
	rule := forwardRule(2, upstream)
	if _, err := m.Ensure(rule); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	e := m.entries[rule.ID]
	m.mu.Unlock()
	if e == nil {
		t.Fatal("应该有正在运行的转发器")
	}
	addr, ok := e.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("监听地址类型异常：%T", e.ln.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Fatalf("转发器绑了非回环地址 %s —— 这是开放代理级的安全问题", addr.IP)
	}
	if addr.IP.String() != "127.0.0.1" {
		t.Fatalf("必须写死绑 127.0.0.1，实际 %s", addr.IP)
	}
}

// TestForwarderPortAllocationPrefersStableAndSkipsBusy：端口优先复用旧值，
// 被占用时顺延，且推导候选与规则 id 绑定。
func TestForwarderPortAllocationPrefersStableAndSkipsBusy(t *testing.T) {
	upstream := startUpstream(t, func(c net.Conn) { _ = c.Close() })

	// 1) 按 id 推导：区间 48200..48203，id=7 → 48200 + 7%4 = 48203
	m := newTestManager(t, ManagerOptions{PortMin: 48200, PortMax: 48203})
	r1 := forwardRule(7, upstream)
	got, err := m.Ensure(r1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 48203 {
		t.Fatalf("按 id 推导的候选端口 = %d，期望 48203", got)
	}

	// 2) 首选端口（数据库里的旧值）优先于推导候选
	r2 := forwardRule(5, upstream)
	r2.ForwardPort = 48202
	got2, err := m.Ensure(r2)
	if err != nil {
		t.Fatal(err)
	}
	if got2 != 48202 {
		t.Fatalf("应复用旧端口 48202，实际 %d", got2)
	}

	// 3) 候选端口被外部进程占用时顺延
	busy, err := net.Listen("tcp", "127.0.0.1:48200")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	r3 := forwardRule(4, upstream) // id=4 → 候选 48200（被占）
	got3, err := m.Ensure(r3)
	if err != nil {
		t.Fatal(err)
	}
	if got3 == 48200 {
		t.Fatal("候选端口被占用时必须顺延")
	}
	if got3 < 48200 || got3 > 48203 {
		t.Fatalf("顺延结果 %d 超出区间", got3)
	}
}

// TestForwarderReusesPortAfterRestart：面板重启（新管理器）后优先复用数据库里的端口，
// 否则 nginx 配置里的 proxy_pass 会指向一个没人听的端口。
func TestForwarderReusesPortAfterRestart(t *testing.T) {
	upstream := startUpstream(t, func(c net.Conn) { _ = c.Close() })
	m1 := newTestManager(t, ManagerOptions{PortMin: 48300, PortMax: 48399})
	rule := forwardRule(9, upstream)
	port, err := m1.Ensure(rule)
	if err != nil {
		t.Fatal(err)
	}
	m1.StopAll() // 面板退出

	m2 := newTestManager(t, ManagerOptions{PortMin: 48300, PortMax: 48399})
	rule2 := forwardRule(9, upstream)
	rule2.ForwardPort = port // 数据库里记住的值
	again, err := m2.Ensure(rule2)
	if err != nil {
		t.Fatal(err)
	}
	if again != port {
		t.Fatalf("重启后应复用端口 %d，实际 %d", port, again)
	}
}

// TestForwarderConcurrency：多条连接并发都被正确服务，统计对得上。
func TestForwarderConcurrency(t *testing.T) {
	upstream := startUpstream(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		_, _ = c.Write(buf)
	})
	m := newTestManager(t, ManagerOptions{MaxConnsPerRule: 64})
	rule := forwardRule(11, upstream)
	port, err := m.Ensure(rule)
	if err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Write([]byte("hello")); err != nil {
				errs <- err
				return
			}
			buf := make([]byte, 5)
			if _, err := io.ReadFull(c, buf); err != nil {
				errs <- err
				return
			}
			if string(buf) != "hello" {
				errs <- fmt.Errorf("回显 = %q", buf)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发连接失败：%v", err)
	}
	waitUntil(t, 3*time.Second, "16 条连接都记进统计且活跃数归零", func() bool {
		st, ok := m.Status(rule.ID)
		return ok && st.TotalConns == n && st.ActiveConns == 0
	})
}

// TestForwarderRejectsBeyondMaxConns：超过并发上限的新连接被立刻断开并留痕。
func TestForwarderRejectsBeyondMaxConns(t *testing.T) {
	release := make(chan struct{})
	upstream := startUpstream(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		<-release
	})
	m := newTestManager(t, ManagerOptions{MaxConnsPerRule: 1})
	rule := forwardRule(12, upstream)
	port, err := m.Ensure(rule)
	if err != nil {
		t.Fatal(err)
	}
	first := dialForwarder(t, port)
	defer func() { _ = first.Close() }()
	// 等第一条真正进入处理中（active=1），再打第二条。
	waitUntil(t, 2*time.Second, "第一条连接进入活跃状态", func() bool {
		st, _ := m.Status(rule.ID)
		return st.ActiveConns == 1
	})

	second := dialForwarder(t, port)
	defer func() { _ = second.Close() }()
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := second.Read(buf); err == nil {
		t.Fatal("超过并发上限的连接应被立刻断开")
	}
	waitUntil(t, 2*time.Second, "并发超限被记进状态", func() bool {
		st, _ := m.Status(rule.ID)
		return strings.Contains(st.LastError, "并发连接数")
	})
	close(release)
}

// TestForwarderIdleTimeout：双向都没有流量满 IdleTimeout 后连接被断开。
func TestForwarderIdleTimeout(t *testing.T) {
	upstream := startUpstream(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(io.Discard, c) // 收下但永不回应
	})
	m := newTestManager(t, ManagerOptions{IdleTimeout: 200 * time.Millisecond})
	rule := forwardRule(13, upstream)
	port, err := m.Ensure(rule)
	if err != nil {
		t.Fatal(err)
	}
	c := dialForwarder(t, port)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("空闲超时后连接应被断开")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("空闲超时没有生效（等了 %v）", elapsed)
	}
}

// TestForwarderDialFailureIsRecorded：拨号目标失败要限速留痕，且状态里查得到。
func TestForwarderDialFailureIsRecorded(t *testing.T) {
	var logs int
	var mu sync.Mutex
	m := newTestManager(t, ManagerOptions{
		Logf: func(string, ...any) { mu.Lock(); logs++; mu.Unlock() },
	})
	// 127.0.0.1:1 基本不可能有服务在听。
	rule := &Rule{ID: 14, Name: "bad", Listen: 18080, Target: "http://127.0.0.1:1",
		Enabled: true, LANForward: LANForwardOn}
	port, err := m.Ensure(rule)
	if err != nil {
		t.Fatal(err)
	}
	c := dialForwarder(t, port)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("上游连不上时应断开连接")
	}
	waitUntil(t, 2*time.Second, "拨号失败写进状态", func() bool {
		st, _ := m.Status(rule.ID)
		return strings.Contains(st.LastError, "拨号上游") && st.ErrorCount >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if logs == 0 {
		t.Error("拨号失败至少要打一行日志（否则用户看不到）")
	}
}

// TestForwarderReconcile：启动时按规则集起该起的、停该停的，并在端口变化时报告。
func TestForwarderReconcile(t *testing.T) {
	m := newTestManager(t, ManagerOptions{PortMin: 48400, PortMax: 48499})
	enabledPrivate := &Rule{ID: 1, Name: "局域网", Listen: 18080, Target: "http://192.168.1.8:8081", Enabled: true}
	enabledPublic := &Rule{ID: 2, Name: "公网", Listen: 18081, Target: "http://93.184.216.34:80", Enabled: true}
	disabledPrivate := &Rule{ID: 3, Name: "停用", Listen: 18082, Target: "http://192.168.1.8:8081", Enabled: false}
	rules := []*Rule{enabledPrivate, enabledPublic, disabledPrivate}

	changed := m.Reconcile(rules)
	if enabledPrivate.ForwardPort <= 0 {
		t.Fatalf("局域网规则应当被分配转发端口：%+v", enabledPrivate)
	}
	if enabledPublic.ForwardPort != 0 {
		t.Fatalf("公网规则不该走转发：%+v", enabledPublic)
	}
	if disabledPrivate.ForwardPort != 0 {
		t.Fatalf("停用规则不该起转发器：%+v", disabledPrivate)
	}
	if _, ok := m.Status(enabledPrivate.ID); !ok {
		t.Fatal("局域网规则应当有一个正在监听的转发器")
	}
	if _, ok := m.Status(enabledPublic.ID); ok {
		t.Fatal("公网规则不该有转发器")
	}
	if len(changed) != 1 || changed[0] != enabledPrivate.ID {
		t.Fatalf("changed = %v，期望只有规则 1", changed)
	}
	saved := enabledPrivate.ForwardPort

	// 目标改成不转发（off）→ 监听器必须停掉
	enabledPrivate.LANForward = LANForwardOff
	m.Reconcile(rules)
	if enabledPrivate.ForwardPort != 0 {
		t.Fatalf("off 之后应清掉转发端口：%+v", enabledPrivate)
	}
	if _, ok := m.Status(enabledPrivate.ID); ok {
		t.Fatal("off 之后监听器必须停掉")
	}

	// 恢复 auto → 重新使用同一个回环端口（nginx 配置不用改）
	enabledPrivate.LANForward = LANForwardAuto
	m.Reconcile(rules)
	if enabledPrivate.ForwardPort != saved {
		t.Fatalf("恢复转发后应复用语端口 %d，实际 %d", saved, enabledPrivate.ForwardPort)
	}
}

// TestForwarderStopAllClosesListeners：面板退出时所有监听器都要关掉。
func TestForwarderStopAllClosesListeners(t *testing.T) {
	upstream := startUpstream(t, func(c net.Conn) { _ = c.Close() })
	m := newTestManager(t, ManagerOptions{PortMin: 48500, PortMax: 48599})
	rule := forwardRule(21, upstream)
	port, err := m.Ensure(rule)
	if err != nil {
		t.Fatal(err)
	}
	m.StopAll()
	if _, ok := m.Status(rule.ID); ok {
		t.Fatal("StopAll 之后不应还有监听器")
	}
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond); err == nil {
		t.Fatal("StopAll 之后端口仍可连接")
	}
}

// TestForwarderTargetChangeRestarts：改目标后转发到新地址（同一条规则只留一个监听器）。
func TestForwarderTargetChangeRestarts(t *testing.T) {
	upA := startUpstream(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Write([]byte("A"))
	})
	upB := startUpstream(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Write([]byte("B"))
	})
	m := newTestManager(t, ManagerOptions{PortMin: 48600, PortMax: 48699})
	rule := forwardRule(31, upA)
	port, err := m.Ensure(rule)
	if err != nil {
		t.Fatal(err)
	}
	rule.Target = "http://" + upB
	if _, err := m.Ensure(rule); err != nil {
		t.Fatal(err)
	}
	if rule.ForwardPort != port {
		t.Fatalf("同一规则的端口应保持不变，实际 %d → %d", port, rule.ForwardPort)
	}
	c := dialForwarder(t, rule.ForwardPort)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "B" {
		t.Fatalf("改目标后应转发到新上游，实际收到 %q", buf)
	}
	m.mu.Lock()
	n := len(m.entries)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("同一条规则只应有一个监听器，实际 %d", n)
	}
}

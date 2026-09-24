package services

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
//  健康检查的整批预算
//
//  真机现象（2026-09-24 只读复测本机已装 1.9.7，10 个服务全在跑）：
//    GET /api/v1/services?health=0  496 ms
//    GET /api/v1/services?health=1  6097 ms（首次）
//    GET /api/v1/services?health=1   565 ms（紧接着第二次）
//  根因：health.go 的 httpHealth 对每条服务起一个 `/usr/bin/curl --max-time 6`，
//  List 并发跑它们，但只要有一条 6 秒没答完（服务正在启动 / 被 TCC 挡住 /
//  端口假死），整张列表就跟着等满 6 秒。第二次快只是因为那个服务恰好起来了。
//
//  修法：整批健康检查只有一个预算；预算内谁先答完算谁。没答完的**如实报
//  「未确认」**（既不是健康、也不是确定故障），那个探针继续在后台跑完，
//  结论写进跨请求的 HealthCache —— 下一次请求立刻拿到真实结果，不会永远停在
//  「未确认」。
// ---------------------------------------------------------------------------

// healthBatchBudget 是**整批**健康检查的预算：健康探针最多让列表多等这么久。
//
// 为什么是 2.5 秒：本机 10 条服务的 curl 并发实测 200~400ms 就全回来了，
// 2.5 秒有 6 倍余量；真正卡住的服务不该让用户等满原来的 6 秒。
// 抽成变量：单测调小它，避免每个用例真等 2.5 秒（同 serviceActionWaitCap）。
var healthBatchBudget = 2500 * time.Millisecond

// healthProbeTimeout 是单个探针自己的上限：后台跑也要收口，不能永远挂着。
// 原来是"请求 ctx + 8s"，现在请求返回后探针改挂后台，这个上限必须留着。
const healthProbeTimeout = 8 * time.Second

// healthResultTTL 是缓存结论的有效期。超过它就重新探，绝不把很久以前的结论
// 当成现在的（那是谎报）。命中缓存时也会顺手在后台刷新一次，所以它主要防的是
// "面板很久没人打开"之后的陈旧数据。
var healthResultTTL = 2 * time.Minute

// HealthCache 是健康检查结论的**进程级**缓存（跨请求）。
//
// 为什么必须有：web 层每次请求都新建 Manager（见 api_services.go 的 svcManager），
// 缓存挂在 Manager 上活不过一次请求 —— 那样预算内没答完的服务会**永远**停在
// 「未确认」。所以由 Server 持有一份并经 Options.HealthCache 注入；不落库：
// 面板重启后重新探一次即可（同 StartHintStore 的理由）。
//
// 方法都容忍 nil 接收者：没注入 = 每次现探，绝不因此报错。
type HealthCache struct {
	mu      sync.Mutex
	results map[string]cachedHealth
	running map[string]bool
}

type cachedHealth struct {
	h  Health
	at time.Time
}

// Lookup 返回 ttl 内的缓存结论；第二个返回值 = 有没有可用结论。
func (c *HealthCache) Lookup(name string, ttl time.Duration) (Health, bool) {
	if c == nil {
		return Health{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.results[name]
	if !ok || time.Since(e.at) > ttl {
		return Health{}, false
	}
	return e.h, true
}

// Start 认领一次探针；返回 false = 已经有一个在跑（不重复起，它跑完会写缓存）。
func (c *HealthCache) Start(name string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[name] {
		return false
	}
	if c.running == nil {
		c.running = map[string]bool{}
	}
	c.running[name] = true
	return true
}

// Finish 记录探针结论并释放认领。
func (c *HealthCache) Finish(name string, h Health) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.results == nil {
		c.results = map[string]cachedHealth{}
	}
	c.results[name] = cachedHealth{h: h, at: time.Now()}
	delete(c.running, name)
}

// probeHealth 走注入的探针（仅供测试）或驱动自己的实现。
func (m *Manager) probeHealth(ctx context.Context, drv Driver, s *Service) Health {
	if m.healthProbeOverride != nil {
		return m.healthProbeOverride(ctx, s)
	}
	if drv == nil {
		return Health{Checked: false}
	}
	return drv.Health(ctx)
}

// startHealthProbe 在后台起一次探针，返回结果通道；已有探针在跑时返回 nil。
//
// 刻意**不**用请求的 ctx：请求返回后探针继续跑完，结论进缓存供下一次请求用。
func (m *Manager) startHealthProbe(s *Service, drv Driver) <-chan Health {
	if !m.opt.HealthCache.Start(s.Name) {
		return nil
	}
	ch := make(chan Health, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
		defer cancel()
		h := m.probeHealth(ctx, drv, s)
		m.opt.HealthCache.Finish(s.Name, h)
		ch <- h
	}()
	return ch
}

// awaitHealth 在预算内拿结论：缓存命中或探针答完 → 真实结论；否则如实报「未确认」。
func (m *Manager) awaitHealth(ch <-chan Health, s *Service, drv Driver) Health {
	if h, ok := m.opt.HealthCache.Lookup(s.Name, healthResultTTL); ok {
		// 热路径：上一次探针的真实结论，顺手在后台刷新保持新鲜。
		m.startHealthProbe(s, drv)
		return h
	}
	if ch == nil {
		// 同一个服务的探针还在后台跑（并发请求/上次预算外的），它跑完会写缓存。
		return unconfirmedHealth(s)
	}
	select {
	case h := <-ch:
		return h
	case <-time.After(healthBatchBudget):
		return unconfirmedHealth(s)
	}
}

// unconfirmedHealth 是"预算内没拿到结论"的如实表示。
//
// 语义：Checked=false（前端据此**不把它算成故障**，与"没配检查地址"同一栏）、
// Unconfirmed=true 让前端能单独标「未确认（超时）」；绝不说健康、也绝不说确定故障。
func unconfirmedHealth(s *Service) Health {
	return Health{
		Checked:     false,
		Unconfirmed: true,
		URL:         s.HealthURL,
		Message:     fmt.Sprintf("未在 %.1f 秒内响应：结果未确认（不是故障），稍后刷新看真实结论", healthBatchBudget.Seconds()),
	}
}

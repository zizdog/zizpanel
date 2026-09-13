package sysinfo

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestLastIsNonBlocking 锁定「请求路径不采集」这一约定。
//
// 回归背景：Collect 必须跑 `top -l 1 -n 0`（约 0.5 秒），早期实现让每个 HTTP
// 请求都同步采集，并发时在互斥锁上排队，实测 5 并发延迟线性累加到 3.3 秒。
// 现在 Last 只读缓存，必须做到毫秒级返回且并发不退化。
func TestLastIsNonBlocking(t *testing.T) {
	c := NewCollector("/")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, 3*time.Second)

	// 预热后同样要快：即使后台正好在采集，Last 也不能等它。
	time.Sleep(100 * time.Millisecond)

	const n = 20
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s := c.Last(); s == nil {
				t.Error("Last 返回 nil")
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 若仍在请求路径同步采集，n 个并发至少要 n×0.5 秒；这里给足余量仍应远低于它。
	if elapsed > 2*time.Second {
		t.Fatalf("%d 个并发 Last 耗时 %v，说明请求路径仍在同步采集", n, elapsed)
	}
	t.Logf("%d 个并发 Last 共耗时 %v", n, elapsed)
}

// TestStartCollectsFirstFrame 保证后台启动后立刻有数据，首个请求不用等。
func TestStartCollectsFirstFrame(t *testing.T) {
	c := NewCollector("/")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, time.Hour) // 周期设很长，只有首帧会采集

	s := c.Last()
	if s.MemTotal == 0 {
		t.Error("首帧应已完成采集，MemTotal 不应为 0")
	}
	if s.Hostname == "" {
		t.Error("首帧应已填充静态信息 Hostname")
	}
}

// TestStartIsIdempotent 防止重复启动叠加出多个采样 goroutine。
func TestStartIsIdempotent(t *testing.T) {
	c := NewCollector("/")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, time.Hour)
	c.Start(ctx, time.Hour)
	c.Start(ctx, time.Hour)
	if !c.sampling.Load() {
		t.Fatal("sampling 应为 true")
	}
}

// TestRefreshAsyncDoesNotBlock 校验过期刷新是异步的：top 再慢也不拖住调用方。
func TestRefreshAsyncDoesNotBlock(t *testing.T) {
	c := NewCollector("/")
	start := time.Now()
	c.RefreshAsync(context.Background()) // 缓存为空 → 必然触发刷新
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("RefreshAsync 阻塞了 %v，应立即返回", d)
	}

	// 刷新最终应写入缓存，且 demand 已被标记。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := c.Last(); s.MemTotal > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("异步刷新未在 5 秒内产出缓存")
}

// TestLastNotBlockedByInFlightCollect 是本文件最重要的回归测试。
//
// 真实故障：Collect 早期在开头就 `c.mu.Lock()` 并 defer 解锁，于是整段采集
// （top + vmstat + netstat，日常 0.5 秒，偶发 5-7 秒）都持有数据锁。请求路径
// 虽然改成读缓存，却仍要抢这把锁，结果接口在真机上随机卡 0.6 / 5.7 / 7.0 秒。
// 修复方式是把采集串行锁 collectMu 与数据锁 mu 分离。这里确认读缓存不再被采集阻塞。
func TestLastNotBlockedByInFlightCollect(t *testing.T) {
	c := NewCollector("/")
	c.initStatic()
	// 先采集一次建立基线，让后续采集走完整路径。
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("预热采集失败: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Collect(context.Background()) // 模拟一次慢采集
	}()

	var worst time.Duration
	for i := 0; i < 500; i++ {
		s := time.Now()
		if c.Last() == nil {
			t.Error("Last 返回 nil")
		}
		if d := time.Since(s); d > worst {
			worst = d
		}
	}
	<-done

	if worst > 100*time.Millisecond {
		t.Fatalf("采集进行中 Last 最坏耗时 %v；说明读缓存仍被采集动作阻塞", worst)
	}
	t.Logf("采集进行中 500 次 Last 最坏耗时 %v", worst)
}

package services

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestListHealthHasBatchBudget 是本轮"慢服务不许拖住状态页"的门禁。
//
// 现象（2026-09-24 真机实测）：GET /api/v1/services?health=1 首次 6031ms ——
// 只要有一条服务的 httpHealth（/usr/bin/curl --max-time 6）没答完，整张列表就
// 跟着等满 6 秒。现有门禁（health_test.go）只断言单条 httpHealth 的判定语义，
// 没有任何一条约束 List 的耗时，所以这条路一路绿着进了真机。
//
// 用注入的假探针验证（不跑 curl、不碰真实端口/服务）：
//  1. 一个 10 秒才回的探针**不许**让 List 超过预算（含容差），且结果必须如实标
//     「未确认」—— 既不是健康、也不是确定故障；
//  2. 负向对照：探针都很快时必须拿到真实结论（不是未确认）—— 否则"一律返回
//     未确认"也能让第 1 条变绿，等于没测。
func TestListHealthHasBatchBudget(t *testing.T) {
	origBudget := healthBatchBudget
	healthBatchBudget = 300 * time.Millisecond
	defer func() { healthBatchBudget = origBudget }()

	// newBareService 造一条不碰真实系统的服务记录。StartCmd 让 DriverFor 走
	// commandDriver：Status 只读 WorkDir 临时目录里的 pid 文件，不碰 launchd、
	// 不碰真实进程，也不占端口。
	newBareService := func(t *testing.T) *Manager {
		t.Helper()
		repo := newTestRepo(t)
		svc := &Service{
			Name: "slow", DisplayName: "Slow", Kind: KindNative,
			StartCmd:  "true",
			HealthURL: "http://127.0.0.1:1/",
		}
		if err := repo.Create(context.Background(), svc); err != nil {
			t.Fatalf("登记服务失败: %v", err)
		}
		return NewManager(repo, Options{
			UserHome: t.TempDir(), UserName: "zizdog", WorkDir: t.TempDir(),
		})
	}

	t.Run("10秒的探针不许让List超过预算", func(t *testing.T) {
		mm := newBareService(t)
		// 刻意不恢复这个注入：预算外那个后台探针会继续跑一会儿（这正是要验证的
		// 行为），恢复它反而是在它读字段时引入数据竞争。Manager 是本次用例私有的，
		// 不恢复也不会漏给别的用例。
		mm.SetHealthProbeForTest(func(ctx context.Context, s *Service) Health {
			select {
			case <-time.After(10 * time.Second):
				return Health{Checked: true, OK: true, URL: s.HealthURL}
			case <-ctx.Done():
				return Health{Checked: false, URL: s.HealthURL}
			}
		})

		start := time.Now()
		views, err := mm.List(context.Background(), true)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("List 失败: %v", err)
		}
		// 容差 1.5s 覆盖 goroutine 调度与 SQLite，但仍远小于探针的 10 秒。
		if limit := healthBatchBudget + 1500*time.Millisecond; elapsed > limit {
			t.Fatalf("10 秒的探针把 List 拖了 %v（预算 %v + 容差 1.5s = %v）：慢服务又拖住整页了",
				elapsed, healthBatchBudget, limit)
		}
		if len(views) != 1 {
			t.Fatalf("应返回 1 个服务，实际 %d", len(views))
		}
		h := views[0].Health
		if !h.Unconfirmed {
			t.Errorf("预算内没答完必须如实标「未确认」，实际 %+v", h)
		}
		if h.Checked || h.OK {
			t.Errorf("未确认既不是健康、也不是确定故障：不该 checked/ok，实际 %+v", h)
		}
		if !strings.Contains(h.Message, "未确认") {
			t.Errorf("未确认要有一句人话说明，实际 %q", h.Message)
		}
	})

	t.Run("快探针必须给出真实结论", func(t *testing.T) {
		mm := newBareService(t)
		mm.SetHealthProbeForTest(func(ctx context.Context, s *Service) Health {
			return Health{Checked: true, OK: true, Code: 200, URL: s.HealthURL, Message: "HTTP 200"}
		})

		views, err := mm.List(context.Background(), true)
		if err != nil {
			t.Fatalf("List 失败: %v", err)
		}
		if len(views) != 1 {
			t.Fatalf("应返回 1 个服务，实际 %d", len(views))
		}
		h := views[0].Health
		if h.Unconfirmed {
			t.Fatalf("探针立刻答完了，不该报未确认（否则「一律返回未确认」也能过上面那条）: %+v", h)
		}
		if !h.Checked || !h.OK {
			t.Fatalf("快探针的真实结论必须原样返回，实际 %+v", h)
		}
	})
}

package scheduler

import (
	"context"
	"sync"
	"time"
)

// ============================================================================
//  面板内置任务的每日循环
//
//  为什么放在 scheduler 包、而不是在 web 层再起一个 goroutine：
//  这是"面板自己的定时任务"，与用户建的 shell/backup 任务同属一个领域。
//  项目里已经有 internal/scheduler（cron 表达式解析 + launchd 注册），
//  内置任务也归它管，避免出现第二套"定时"概念。
//
//  为什么不直接复用本包的 launchd 机制：
//  launchd 跑的是**独立进程**里的 shell 命令，它写不进面板的任务中心与审计表，
//  失败的续期就只能留在日志文件里 —— 而项目的铁律是"失败必须可见"。
//  所以内置任务跑在面板进程内，进度进任务中心、结果进审计。
//
//  有意不做的事：
//   - 不做假百分比、不做重试风暴：一天只触发一次，失败了明天再试。
//   - 不持久化"上次执行日期"：面板重启后若当天已过点，会补跑一次。
//     这对续期是安全的 —— 真正决定要不要签发的门槛是 NeedsRenewal，
//     而不是"今天跑没跑过"。
// ============================================================================

// DailyTask 是一个面板内置的每日任务。
//
// Run 返回 error 表示失败；失败一定会被记录（logf + 调用方审计），
// 不会被吞掉 —— "能谎报成功的功能比没做更糟"。
type DailyTask struct {
	// Name 是任务名（写进日志/审计，例如 "acme-renew"）
	Name string
	// Run 是任务体。ctx 会在超时或面板退出时被取消。
	Run func(ctx context.Context) error
}

// DailyOutcome 是一次内置任务的执行结果。
type DailyOutcome struct {
	Name string
	Err  error
}

// dailyTaskTimeout 是单个内置任务的超时。
//
// 续期要走 ACME 的 DNS/HTTP 校验，最慢的情况是等 CA 轮询（几十秒到几分钟），
// 15 分钟足够，同时保证一个卡死的任务不会把每日循环永远占住。
const dailyTaskTimeout = 15 * time.Minute

// DailyRunner 在面板进程内按"每天某个时刻"执行内置任务。
//
// 判定方式不是"睡到那个时刻"，而是**每分钟醒一次看当前时间是否已过点、
// 且今天还没跑过**。这样：
//   - Mac 睡眠/唤醒（launchd 会补跑，普通 timer 不会）后仍然会补跑；
//   - 系统时间被改、时区变化也不会把下一次触发推到 24 小时之后。
type DailyRunner struct {
	hour, minute int
	tasks        []DailyTask
	logf         func(format string, args ...any)

	// now / tick 可在单测里替换（同包测试直接改字段），生产用默认值。
	now  func() time.Time
	tick time.Duration

	mu      sync.Mutex
	lastDay string // 已触发过的日期（本地时区，2006-01-02）
	running bool   // 防止上一轮还没结束时重复触发
}

// NewDailyRunner 构造每日循环。hour/minute 是本地时间几点几分。
//
// logf 传 nil 时不打日志（单测用）。
func NewDailyRunner(hour, minute int, tasks []DailyTask, logf func(format string, args ...any)) *DailyRunner {
	if hour < 0 || hour > 23 {
		hour = 3
	}
	if minute < 0 || minute > 59 {
		minute = 0
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &DailyRunner{
		hour:   hour,
		minute: minute,
		tasks:  tasks,
		logf:   logf,
		now:    time.Now,
		tick:   time.Minute,
	}
}

// NextRun 返回严格晚于 now 的下一次触发时刻（供界面提示/测试）。
func (r *DailyRunner) NextRun(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), r.hour, r.minute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Start 启动后台循环：注册后立即检查一次（补跑），之后每分钟检查一次。
func (r *DailyRunner) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(r.tick)
		defer ticker.Stop()

		// 启动即检查：面板可能在 3 点之后才启动（升级、重启、断电恢复）。
		r.tickOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.tickOnce(ctx)
			}
		}
	}()
}

// RunTasks 立即执行全部内置任务并返回每个任务的结果（供"手动触发"与单测使用）。
//
// 注意：它**不**改 lastDay —— 手动触发不该影响"今天还跑不跑"的判断。
func (r *DailyRunner) RunTasks(ctx context.Context) []DailyOutcome {
	out := make([]DailyOutcome, 0, len(r.tasks))
	for _, t := range r.tasks {
		tctx, cancel := context.WithTimeout(ctx, dailyTaskTimeout)
		err := t.Run(tctx)
		cancel()
		if err != nil {
			// 失败必须留痕：调用方可能只关心自己的审计，这里兜一层日志。
			r.logf("内置任务 %s 失败: %v", t.Name, err)
		}
		out = append(out, DailyOutcome{Name: t.Name, Err: err})
	}
	return out
}

// tickOnce 是每分钟的检查：到点了、今天还没跑过、且没有正在跑的一轮，就执行。
func (r *DailyRunner) tickOnce(ctx context.Context) {
	now := r.now()
	if !r.due(now) {
		return
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	// 先占位再执行：执行可能持续几分钟，期间每分钟都会 tick 一次，
	// 不先占位就会重复触发同一批任务。
	r.running = true
	r.lastDay = now.Format("2006-01-02")
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	r.logf("开始执行面板内置每日任务（%d 个）", len(r.tasks))
	for _, oc := range r.RunTasks(ctx) {
		if oc.Err == nil {
			r.logf("内置任务 %s 完成", oc.Name)
		}
	}
}

// due 判断"今天到点了，而且今天还没跑过"。
func (r *DailyRunner) due(now time.Time) bool {
	if now.Hour() < r.hour || (now.Hour() == r.hour && now.Minute() < r.minute) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastDay != now.Format("2006-01-02")
}

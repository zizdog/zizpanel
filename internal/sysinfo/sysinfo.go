// Package sysinfo 通过 macOS 原生命令采集系统指标。
//
// 采集策略：
//   - 优先调用 /usr/bin/sysctl、/usr/bin/vm_stat、/usr/bin/netstat 等系统自带工具，
//     而不是引入 cgo 或第三方库 —— 这些命令在任何 macOS 上都存在，且输出稳定。
//   - CPU 使用率必须用「两次采样的差值」计算，单次快照算不出使用率。
//   - 所有采集都带超时，避免系统卡顿时把面板拖死。
package sysinfo

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Sample 是一次系统指标快照。
type Sample struct {
	Ts        int64   `json:"ts"`
	Hostname  string  `json:"hostname"`
	OS        string  `json:"os"`
	Arch      string  `json:"arch"`
	Uptime    int64   `json:"uptime"` // 秒
	CPUModel  string  `json:"cpu_model"`
	CPUCores  int     `json:"cpu_cores"`
	CPUUsed   float64 `json:"cpu_used"` // 百分比 0-100
	LoadAvg1  float64 `json:"load_1"`
	LoadAvg5  float64 `json:"load_5"`
	LoadAvg15 float64 `json:"load_15"`
	MemTotal  uint64  `json:"mem_total"` // 字节
	MemUsed   uint64  `json:"mem_used"`
	MemFree   uint64  `json:"mem_free"`
	MemCached uint64  `json:"mem_cached"` // inactive + purgeable，实为可回收
	SwapTotal uint64  `json:"swap_total"`
	SwapUsed  uint64  `json:"swap_used"`
	DiskTotal uint64  `json:"disk_total"`
	DiskUsed  uint64  `json:"disk_used"`
	DiskFree  uint64  `json:"disk_free"`
	NetRx     uint64  `json:"net_rx"` // 累计字节
	NetTx     uint64  `json:"net_tx"`
	NetRxRate float64 `json:"net_rx_rate"` // 字节/秒
	NetTxRate float64 `json:"net_tx_rate"`
	CPUTemp   float64 `json:"cpu_temp"` // 摄氏度，取不到为 0
	Procs     int     `json:"procs"`
}

// Collector 持有采样状态（上一次的 CPU 与网络累计值）。
type Collector struct {
	mu        sync.Mutex
	collectMu sync.Mutex // 串行化采集动作；与 mu 分离，慢采集不会阻塞读缓存
	lastCPU   cpuTicks
	lastNet   netCounters
	lastTime  time.Time
	diskPath  string
	static    Sample
	initOnce  sync.Once
	cache     *Sample // 上一次完整采样，请求路径只读这里

	// ---- 后台采样与按需刷新 ----
	//
	// 为什么需要这一层：采集 CPU 必须调用 `top -l 1 -n 0`，而 top 要等一个采样
	// 周期，实测约 0.5 秒。若每个 HTTP 请求都同步采集一次，多个并发请求会在互斥锁上
	// 排队，延迟线性累加（实测 5 个并发 = 0.65/1.30/1.96/2.65/3.28 秒）。
	// 因此改为：后台 goroutine 定期采样写入 cache，请求路径只读 cache，永不阻塞。
	demandMu   sync.Mutex
	lastDemand time.Time   // 最近一次「有人要看指标」的时刻
	refreshing atomic.Bool // 防止过期刷新叠加
	sampling   atomic.Bool // 后台循环是否已启动

	// 温度探针的缓存；只在 Collect 内访问，而 Collect 由 collectMu 串行化，故无需加锁。
	tempAt       time.Time
	tempVal      float64
	tempDisabled bool
}

// demandTTL 是「无人查看」多久后暂停后台采样。
// 面板没人打开时不应该持续跑 top 白耗 CPU，空闲即停。
const demandTTL = 30 * time.Second

// staleAfter 是缓存超过多久算过期、需要触发一次异步刷新。
const staleAfter = 5 * time.Second

// NewCollector 创建采集器。diskPath 是要统计的磁盘路径（如网站根目录所在卷）。
func NewCollector(diskPath string) *Collector {
	if diskPath == "" {
		diskPath = "/"
	}
	return &Collector{diskPath: diskPath}
}

type cpuTicks struct {
	user, sys, idle, nice uint64
}

type netCounters struct {
	rx, tx uint64
}

func (c *Collector) initStatic() {
	c.initOnce.Do(func() {
		s := Sample{
			Hostname: runTrim("hostname", "-s"),
			OS:       runTrim("sw_vers", "-productVersion"),
			Arch:     runtime.GOARCH,
			CPUModel: runTrim("sysctl", "-n", "machdep.cpu.brand_string"),
			CPUCores: runtime.NumCPU(),
		}
		if v, err := strconv.Atoi(runTrim("sysctl", "-n", "hw.ncpu")); err == nil && v > 0 {
			s.CPUCores = v
		}
		s.MemTotal = sysctlUint("hw.memsize")
		c.static = s
	})
}

// Collect 采集一次指标。返回的 Sample 可直接 JSON 序列化给前端。
func (c *Collector) Collect(ctx context.Context) (Sample, error) {
	c.initStatic()

	// 锁分两把，这是本文件最关键的设计：
	//   collectMu —— 串行化「采集」这个动作本身，避免并发重复调用 top/netstat。
	//   mu        —— 只保护 lastCPU/lastNet/lastTime/cache，持有时间是微秒级。
	//
	// 采集期间绝不能持有 mu：top / netstat 偶尔会阻塞数秒（实测 netstat -ib 出现过
	// 20 秒），一旦持锁，请求路径读缓存就被拖住，表现成接口随机卡 5-7 秒。
	c.collectMu.Lock()
	defer c.collectMu.Unlock()

	now := time.Now()

	// 短暂读基线后立即释放，所有慢操作都在锁外完成。
	c.mu.Lock()
	prevCPU, prevNet, prevTime := c.lastCPU, c.lastNet, c.lastTime
	c.mu.Unlock()

	out := c.static
	out.Ts = now.UnixMilli()
	out.Uptime = bootUptime()

	// ---- 主路径：top -l 1 -n 0（Apple Silicon 上唯一可靠的 CPU 来源）----
	tp, topOK := readTop(ctx)
	if topOK {
		if tp.HasCPU {
			out.CPUUsed = round2(tp.CPUUser + tp.CPUSys)
		}
		if tp.HasLoad {
			out.LoadAvg1, out.LoadAvg5, out.LoadAvg15 = tp.Load1, tp.Load5, tp.Load15
		}
		if tp.HasProcs {
			out.Procs = tp.Procs
		}
	}

	// ---- 兜底：kern.cp_time 差值（部分系统版本可用，或 top 失败时）----
	nextCPU := prevCPU
	if !topOK || !tp.HasCPU {
		cur := readCPUTicks(ctx)
		if prevCPU != (cpuTicks{}) && now.Sub(prevTime) > 200*time.Millisecond {
			du := float64(cur.user - prevCPU.user)
			ds := float64(cur.sys - prevCPU.sys)
			dn := float64(cur.nice - prevCPU.nice)
			di := float64(cur.idle - prevCPU.idle)
			if total := du + ds + dn + di; total > 0 {
				out.CPUUsed = clampPct(round2((du + ds + dn) / total * 100))
			}
		}
		nextCPU = cur
	}

	// top 不可用时，负载改用 sysctl vm.loadavg
	if !topOK || !tp.HasLoad {
		out.LoadAvg1, out.LoadAvg5, out.LoadAvg15 = loadAvg()
	}

	// top 不可用时，进程数改用 ps 统计
	if !topOK || !tp.HasProcs {
		out.Procs = procCount(ctx)
	}

	// ---- 内存：vm_stat 精确计算；top 的 PhysMem 仅作交叉校验 ----
	out.MemFree, out.MemCached, out.SwapTotal, out.SwapUsed = readMem(ctx)
	if out.MemTotal > out.MemFree {
		used := out.MemTotal - out.MemFree - out.MemCached
		out.MemUsed = used
	}
	// vm_stat 与 top 的口径可能相差"压缩内存"，取较大值更贴近"关于本机"的显示，
	// 避免用户看到面板内存占用明显低于活动监视器而产生疑虑。
	if topOK && tp.HasMem && tp.MemUsed > out.MemUsed {
		out.MemUsed = tp.MemUsed
	}

	// ---- 磁盘 ----
	out.DiskTotal, out.DiskUsed, out.DiskFree = diskUsage(c.diskPath)

	// ---- 网络 ----
	n := readNet(ctx)
	if prevNet != (netCounters{}) {
		dt := now.Sub(prevTime).Seconds()
		if dt > 0.2 {
			if n.rx >= prevNet.rx {
				out.NetRxRate = round2(float64(n.rx-prevNet.rx) / dt)
			}
			if n.tx >= prevNet.tx {
				out.NetTxRate = round2(float64(n.tx-prevNet.tx) / dt)
			}
		}
	}
	out.NetRx, out.NetTx = n.rx, n.tx

	// ---- 温度（Apple Silicon 需要 root + powermetrics，取不到就留 0）----
	out.CPUTemp = c.readTemp(ctx)

	// 结果与基线一次性提交，短暂持锁。
	c.mu.Lock()
	c.lastCPU, c.lastNet, c.lastTime = nextCPU, n, now
	cp := out
	c.cache = &cp
	c.mu.Unlock()

	return out, nil
}

// Start 启动后台采样循环，并同步采集一次让缓存立刻可用。
//
// 循环的空闲策略：只有当 demandTTL 内有人查看过指标时才继续采样，
// 否则跳过这一轮 —— 没人看面板时不做无谓的 top 调用。
func (c *Collector) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	if !c.sampling.CompareAndSwap(false, true) {
		return // 只允许启动一次
	}
	c.touchDemand()
	c.safeCollect(ctx) // 首帧同步，避免首个请求看到空数据

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !c.wanted() {
					continue // 空闲：跳过采样
				}
				c.safeCollect(ctx)
			}
		}
	}()
}

// safeCollect 采集一次；panic 不应打断后台循环。
func (c *Collector) safeCollect(ctx context.Context) {
	defer func() { _ = recover() }()
	_, _ = c.Collect(ctx)
}

func (c *Collector) touchDemand() {
	c.demandMu.Lock()
	c.lastDemand = time.Now()
	c.demandMu.Unlock()
}

// wanted 报告是否仍有人在看指标。
func (c *Collector) wanted() bool {
	c.demandMu.Lock()
	defer c.demandMu.Unlock()
	return !c.lastDemand.IsZero() && time.Since(c.lastDemand) < demandTTL
}

// RefreshAsync 在缓存过期时触发一次后台刷新，然后立即返回。
//
// 请求路径永远不等待采集：先拿到（可能略旧的）缓存值，刷新结果由下一次
// 轮询或 SSE 推送带给前端。这样即使 top 卡住也不会拖慢接口。
func (c *Collector) RefreshAsync(ctx context.Context) {
	c.touchDemand()
	c.mu.Lock()
	stale := c.cache == nil || time.Since(c.lastTime) > staleAfter
	c.mu.Unlock()
	if !stale {
		return
	}
	if !c.refreshing.CompareAndSwap(false, true) {
		return // 已有刷新在跑
	}
	go func() {
		defer c.refreshing.Store(false)
		c.safeCollect(ctx)
	}()
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// Last 返回最近一次采样的副本，并把采集器标记为「有人在看」。
// 该调用永不阻塞在采集上，可安全用于 HTTP 请求路径。
func (c *Collector) Last() *Sample {
	c.touchDemand()
	c.initStatic()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		s := c.static
		return &s
	}
	s := *c.cache
	return &s
}

// ---------- 采集实现 ----------

var (
	reLoadavg = regexp.MustCompile(`\{([\d.]+)\s+([\d.]+)\s+([\d.]+)\}`)
	rePages   = regexp.MustCompile(`page size of (\d+) bytes`)
	reVMStat  = regexp.MustCompile(`(?m)^(.+?):\s+(\d+)\.?$`)
)

// loadAvg 解析 vm.loadavg 的输出，形如 { 2.28 2.31 2.41 }。
func loadAvg() (float64, float64, float64) {
	raw := runTrim("sysctl", "-n", "vm.loadavg")
	m := reLoadavg.FindStringSubmatch(raw)
	if len(m) == 4 {
		a, _ := strconv.ParseFloat(m[1], 64)
		b, _ := strconv.ParseFloat(m[2], 64)
		d, _ := strconv.ParseFloat(m[3], 64)
		return a, b, d
	}
	return 0, 0, 0
}

// readCPUTicks 解析 `sysctl -n kern.cp_time`，格式为 "user nice sys idle"。
//
// 注意：Apple Silicon + macOS 15 上该 OID 已不存在，会返回空字符串，
// 此时返回值全 0，调用方会退回 top 方案。保留是为了兼容仍支持该 OID 的系统。
func readCPUTicks(ctx context.Context) cpuTicks {
	raw := runCmd(ctx, "sysctl", "-n", "kern.cp_time")
	f := strings.Fields(raw)
	if len(f) < 4 {
		return cpuTicks{}
	}
	vals := make([]uint64, 4)
	for i := 0; i < 4; i++ {
		vals[i], _ = strconv.ParseUint(f[i], 10, 64)
	}
	// kern.cp_time 顺序：user, nice, sys, idle
	return cpuTicks{user: vals[0], nice: vals[1], sys: vals[2], idle: vals[3]}
}

// readMem 用 vm_stat + sysctl 计算内存。解析逻辑见 macos.go 的 parseVMStat，
// 那里有关于 macOS 内存语义（free / inactive / purgeable）的说明。
func readMem(ctx context.Context) (free, cached uint64, swapTotal, swapUsed uint64) {
	out := runCmd(ctx, "vm_stat")
	free, cached, _ = parseVMStat(out)
	swapTotal, swapUsed = parseSwap(runCmd(ctx, "sysctl", "-n", "vm.swapusage"))
	return
}

var reSwap = regexp.MustCompile(`total = ([\d.]+)M\s+used = ([\d.]+)M`)

func parseSwap(s string) (uint64, uint64) {
	m := reSwap.FindStringSubmatch(s)
	if len(m) != 3 {
		return 0, 0
	}
	t, _ := strconv.ParseFloat(m[1], 64)
	u, _ := strconv.ParseFloat(m[2], 64)
	return uint64(t * 1024 * 1024), uint64(u * 1024 * 1024)
}

// readNet 解析 `netstat -ib` 的累计流量。
//
// 关键点：netstat -ib 每个接口会输出多行（IPv4/IPv6/链路层），
// 直接累加会重复计算。正确做法是按接口名去重，且只取有 Ibytes 列的那几行中
// 第一个出现的（lo0 必须排除）。
func readNet(ctx context.Context) netCounters {
	out := runCmd(ctx, "netstat", "-ib")
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return netCounters{}
	}
	header := strings.Fields(lines[0])
	iName, iBytes, oBytes := -1, -1, -1
	for i, h := range header {
		switch h {
		case "Name":
			iName = i
		case "Ibytes":
			iBytes = i
		case "Obytes":
			oBytes = i
		}
	}
	if iName < 0 || iBytes < 0 || oBytes < 0 {
		return netCounters{}
	}
	seen := map[string]bool{}
	var rx, tx uint64
	for _, ln := range lines[1:] {
		f := strings.Fields(ln)
		if len(f) <= oBytes || len(f) <= iBytes {
			continue
		}
		name := strings.TrimSuffix(f[iName], "*")
		if name == "lo0" || strings.HasPrefix(name, "gif") || strings.HasPrefix(name, "stf") {
			continue
		}
		if seen[name] {
			continue // 同一接口的 IPv6 行会重复，只取第一行
		}
		seen[name] = true
		ib, err1 := strconv.ParseUint(f[iBytes], 10, 64)
		ob, err2 := strconv.ParseUint(f[oBytes], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += ib
		tx += ob
	}
	return netCounters{rx: rx, tx: tx}
}

// readTemp 读取 CPU 温度。Apple Silicon 上普通进程读不到 SMC，
// 需要 root + powermetrics；因此这里只在明显可用时才返回数值。
// tempTTL 是温度采样的最小间隔。
//
// powermetrics 是重量级工具（每次要起进程、接管电源管理采样），而温度本身变化很慢，
// 每次采集都调用它纯属浪费；而且 Apple Silicon 上 `--samplers smc` 并不被支持
// （实测 M4 返回 unrecognized sampler），永远拿不到值。因此这里做两件事：
//   - 非 root 直接永久关闭该探针，省掉每轮一次 `id -u` 子进程；
//   - 结果缓存 tempTTL，最多每分钟真正调用一次。
func (c *Collector) readTemp(ctx context.Context) float64 {
	if c.tempDisabled {
		return 0
	}
	// powermetrics 需要 root；非 root 时这个探针永远不会成功，直接停用。
	if !isRoot() {
		c.tempDisabled = true
		return 0
	}
	if !c.tempAt.IsZero() && time.Since(c.tempAt) < tempTTL {
		return c.tempVal
	}
	c.tempAt = time.Now()

	out := runCmdTimeout(ctx, 1200*time.Millisecond, "powermetrics", "-n", "1", "-i", "500",
		"--samplers", "smc")
	if m := reTemp.FindStringSubmatch(out); len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			c.tempVal = round2(v)
		}
	}
	return c.tempVal
}

var reTemp = regexp.MustCompile(`CPU die temperature:\s*([\d.]+)`)

// tempTTL 见 readTemp 的说明。
const tempTTL = 60 * time.Second

func isRoot() bool { return runTrim("id", "-u") == "0" }

func procCount(ctx context.Context) int {
	out := runCmd(ctx, "ps", "-ax")
	n := strings.Count(out, "\n")
	if n > 0 {
		n-- // 减去表头
	}
	return n
}

// ---------- 命令执行 ----------

const cmdTimeout = 3 * time.Second

func runCmd(ctx context.Context, name string, args ...string) string {
	return runCmdTimeout(ctx, cmdTimeout, name, args...)
}

func runCmdTimeout(ctx context.Context, d time.Duration, name string, args ...string) string {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/usr/bin/"+name, args...).Output()
	if err != nil {
		// /usr/bin 下找不到时回退到 PATH
		out2, err2 := exec.CommandContext(cctx, name, args...).Output()
		if err2 != nil {
			return ""
		}
		return string(out2)
	}
	return string(out)
}

func runTrim(name string, args ...string) string {
	return strings.TrimSpace(runCmd(context.Background(), name, args...))
}

func sysctlUint(key string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(runTrim("sysctl", "-n", key)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// bootUptime 解析 kern.boottime 得到运行秒数。
func bootUptime() int64 {
	out := runTrim("sysctl", "-n", "kern.boottime")
	// 格式：{ sec = 1726000000, usec = 0 } Thu Sep 10 ...
	m := reBoot.FindStringSubmatch(out)
	if len(m) != 2 {
		return 0
	}
	sec, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	d := time.Now().Unix() - sec
	if d < 0 {
		return 0
	}
	return d
}

var reBoot = regexp.MustCompile(`sec = (\d+)`)

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// ---------- 格式化辅助（前端也用到，统一放这里） ----------

// HumanBytes 把字节数格式化为人类可读字符串。
func HumanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

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
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tempReaderPy 是读 Apple Silicon 温度传感器的脚本（IOHID + ctypes）。
// 从 stdin 喂给系统自带的 python3；理由见脚本头部注释。
//
//go:embed temp-reader.py
var tempReaderPy string

// firstLine 取错误信息的第一行：子进程的报错常常是多行堆栈，
// 塞进一句界面提示里没有意义。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

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
	// CPUTempNote 说明温度的来源，或者"为什么取不到"。
	// 以前前端写死显示"不可读取（需 root）"—— 那句话在 Apple Silicon 上**是错的**
	// （真实原因是 powermetrics 没有 smc 采样器），所以要如实回报原因。
	CPUTempNote string `json:"cpu_temp_note"`
	Procs       int    `json:"procs"`
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

	// 温度探针的缓存与说明；只在 Collect 内访问，而 Collect 由 collectMu 串行化，故无需加锁。
	tempNote string
	tempAt   time.Time
	tempVal  float64
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

	// ---- 温度（Apple Silicon 走 IOHID，见 readTemp 的说明）----
	out.CPUTemp, out.CPUTempNote = c.readTemp(ctx)

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

// readTemp 读取 CPU 温度，返回（摄氏度，来源/原因说明）。取不到时温度为 0。
//
// 平台差异（这是这段代码的全部复杂性来源）：
//   - **Apple Silicon**：powermetrics **没有 smc 采样器**（M4 实测只支持
//     tasks/battery/network/disk/interrupts/cpu_power/thermal/sfi/gpu_power/ane_power），
//     而 thermal 给的是"热压力等级"不是温度。以前这里写 `--samplers smc`，
//     在 M 系列上永远拿不到值，界面上就显示成"不可读取（需 root）" —— **那是误报**。
//     正确做法是读 IOHID 的 AppleVendor 温度传感器（见 temp-reader.py）。
//   - **Intel**：沿用 powermetrics 的 smc 采样器（老机器上它才是对的）。
//
// 采样本身很轻（HID 读取约 100ms；powermetrics 较重），而温度变化很慢，
// 所以结果缓存 tempTTL，最多每分钟真正读一次。失败**不永久停用**：
// 权限、python3 之类的条件可能随后被满足，下一轮就能自愈。
func (c *Collector) readTemp(ctx context.Context) (float64, string) {
	if !c.tempAt.IsZero() && time.Since(c.tempAt) < tempTTL {
		return c.tempVal, c.tempNote
	}
	c.tempAt = time.Now()

	if runtime.GOARCH == "arm64" {
		if v, note := readTempIOHID(ctx); v > 0 {
			c.tempVal, c.tempNote = v, note
			return c.tempVal, c.tempNote
		} else if note != "" {
			c.tempNote = note
		}
	}
	// Intel（以及 arm64 上 IOHID 失败的兜底）：powermetrics 的 smc 采样器
	if isRoot() {
		out := runCmdTimeout(ctx, 1500*time.Millisecond, "powermetrics", "-n", "1", "-i", "500",
			"--samplers", "smc")
		if m := reTemp.FindStringSubmatch(out); len(m) == 2 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > 0 {
				c.tempVal, c.tempNote = round2(v), "powermetrics（SMC）"
				return c.tempVal, c.tempNote
			}
		}
		c.tempNote = "powermetrics 没有 smc 采样器（Apple Silicon 不支持），且 IOHID 也取不到"
	} else if c.tempNote == "" {
		c.tempNote = "没有可用来源（非 root，且 IOHID 取不到）"
	}
	c.tempVal = 0
	return 0, c.tempNote
}

// readTempIOHID 走 IOHID 的 AppleVendor 温度传感器（Apple Silicon 的正确路径）。
//
// 用系统自带的 python3 + 内嵌脚本（见文件头 temp-reader.py 的说明：Go 原生要么
// 引 cgo、要么引第三方 FFI 库，两条路对"同时出 arm64/amd64 两个包"的发布流程都不划算）。
// 选哪个传感器、怎么过滤无效值都在 Go 这边做（chooseCPUTemp），所以那部分有单测。
func readTempIOHID(ctx context.Context) (float64, string) {
	python := "/usr/bin/python3"
	if _, err := os.Stat(python); err != nil {
		return 0, "没有 /usr/bin/python3（装 Command Line Tools 即可）"
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, python, "-")
	cmd.Stdin = strings.NewReader(tempReaderPy)
	out, err := cmd.Output()
	if err != nil {
		return 0, "IOHID 读取失败：" + firstLine(err.Error())
	}
	var payload struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Sensors []struct {
			Name string  `json:"name"`
			C    float64 `json:"c"`
		} `json:"sensors"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return 0, "解析温度传感器输出失败"
	}
	if !payload.OK {
		if payload.Error != "" {
			return 0, payload.Error
		}
		return 0, "没读到温度传感器"
	}
	sensors := make([]tempSensor, 0, len(payload.Sensors))
	for _, s := range payload.Sensors {
		sensors = append(sensors, tempSensor{Name: s.Name, Celsius: s.C})
	}
	return chooseCPUTemp(sensors)
}

// tempSensor 是一个温度传感器的读数。抽出来是为了让"选哪个当 CPU 温度"可单测。
type tempSensor struct {
	Name    string
	Mph     string // 未使用，占位保持结构稳定
	Celsius float64
}

// isNonCPUSensor 判断一个传感器名字是否**明确不是** CPU 温度。
//
// 依据 M4 实测的传感器名：
//   - `tcal` 是校准基准（恒定 51.8°C），拿它当 CPU 温度会凭空高出十几度；
//   - `gas gauge battery` 是电池温度；
//   - `NAND CH0 temp` 是 SSD 温度。
//
// 名字里带这些就绝对不能用 —— 宁可显示"取不到"，也不能给一个错的数。
func isNonCPUSensor(lowerName string) bool {
	for _, bad := range []string{"tcal", "battery", "gas gauge", "nand"} {
		if strings.Contains(lowerName, bad) {
			return true
		}
	}
	return false
}

// chooseCPUTemp 从传感器列表里挑出"CPU 温度"。
//
// 规则（依据 M4 实测的 46 个传感器）：
//   - 先丢掉不合常理的读数：M4 上 `PMU tdev1/PMU2 tdev1/PMU2 tdev3` 会返回 **-22°C**，
//     直接取最大值会把这些噪声当数据；所以先按 0 < t < 150 过滤。
//   - 优先 **tdie**（die = 芯片核心温度，M4 上是 PMU tdie1..tdie12 + PMU2 tdie1..12，
//     实测 33–36°C），取最大（最热的那个核心最能反映"CPU 有多热"）。
//   - 没有 tdie 时退而取 PMU 系列，再退而取全体。
//   - **明确排除 tcal**：那是校准基准（M4 上恒定 51.8°C），不是芯片温度，
//     拿它当 CPU 温度会凭空高出十几度。
func chooseCPUTemp(sensors []tempSensor) (float64, string) {
	// 先把"绝不可能是 CPU 温度"的和不合常理的**一次性滤掉**，
	// 后面三层选择都只在这个集合里挑 —— 排除规则只写一处，不会漏。
	cands := make([]tempSensor, 0, len(sensors))
	for _, s := range sensors {
		if s.Celsius <= 0 || s.Celsius >= 150 {
			continue // M4 实测有 -22°C 的无效读数
		}
		if isNonCPUSensor(strings.ToLower(s.Name)) {
			continue // tcal / battery / NAND：明确不是 CPU
		}
		cands = append(cands, s)
	}

	maxMatching := func(pick func(string) bool) (float64, int) {
		best, n := 0.0, 0
		for _, s := range cands {
			if !pick(strings.ToLower(s.Name)) {
				continue
			}
			n++
			if s.Celsius > best {
				best = s.Celsius
			}
		}
		return best, n
	}

	// 优先 tdie（芯片核心温度，M4 上 24 个，33–36°C）：取最热的核心。
	if v, n := maxMatching(func(name string) bool { return strings.Contains(name, "tdie") }); n > 0 {
		return round2(v), fmt.Sprintf("IOHID tdie×%d", n)
	}
	// 其次 PMU 系列（老机型/其它芯片的命名可能不同）
	if v, n := maxMatching(func(name string) bool { return strings.Contains(name, "pmu") }); n > 0 {
		return round2(v), fmt.Sprintf("IOHID PMU×%d", n)
	}
	// 最后退到任意"看起来是 SoC 温度"的传感器
	if v, n := maxMatching(func(string) bool { return true }); n > 0 {
		return round2(v), fmt.Sprintf("IOHID 其它×%d", n)
	}
	return 0, "没有可用的 CPU 温度传感器（读数都被过滤）"
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

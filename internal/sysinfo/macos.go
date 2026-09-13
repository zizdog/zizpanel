package sysinfo

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
//  macOS CPU / 内存 / 负载采集
//
//  踩过的坑：在 Apple Silicon（macOS 15+）上 `sysctl kern.cp_time` 已被移除，
//  返回 "unknown oid"。曾用它做两次采样的差值来算 CPU 使用率，结果恒为 0。
//
//  现在的主路径是解析 `top -l 1 -n 0`：
//    - 一次调用同时给出 CPU 使用率、Load Avg、PhysMem，进程创建开销可接受；
//    - top 的数值是内核直接统计的结果，不需要我们自己维护采样状态；
//    - `-n 0` 表示不列进程，输出很小（约 600 字节），耗时通常 < 1 秒。
//
//  `kern.cp_time` 的差值算法作为兜底保留：万一某些系统版本能取到，
//  它比 top 更精确（不受 top 采样窗口影响）。
// ---------------------------------------------------------------------------

var (
	reCPUUsage      = regexp.MustCompile(`CPU usage:\s*([\d.]+)%\s*user,\s*([\d.]+)%\s*sys,\s*([\d.]+)%\s*idle`)
	reLoadAvgCSV    = regexp.MustCompile(`Load Avg:\s*([\d.]+),\s*([\d.]+),\s*([\d.]+)`)
	rePhysMem       = regexp.MustCompile(`PhysMem:\s*([\d.]+)([KMGT])\s+used\s*\(([^)]*)\)?,\s*([\d.]+)([KMGT])\s+unused`)
	reTopWired      = regexp.MustCompile(`([\d.]+)([KMGT])\s+wired`)
	reTopCompressor = regexp.MustCompile(`([\d.]+)([KMGT])\s+compressor`)
	reTopProcesses  = regexp.MustCompile(`Processes:\s*(\d+)\s+total`)
)

// topSample 是 `top -l 1 -n 0` 解析结果。
type topSample struct {
	CPUUser   float64
	CPUSys    float64
	CPUIdle   float64
	HasCPU    bool
	Load1     float64
	Load5     float64
	Load15    float64
	HasLoad   bool
	MemUsed   uint64 // 含 compressor 的"已用"
	MemUnused uint64
	MemWired  uint64
	MemCompr  uint64
	HasMem    bool
	Procs     int
	HasProcs  bool
}

// readTop 调用 top 并解析。
//
// 超时设为 8 秒：top -l 1 的首次采样在某些负载高的机器上可能接近 2 秒，
// 留足余量避免误判失败。这里不能用 sysinfo 默认的 3 秒超时。
func readTop(ctx context.Context) (topSample, bool) {
	raw := runCmdTimeout(ctx, 8*time.Second, "top", "-l", "1", "-n", "0")
	if strings.TrimSpace(raw) == "" {
		return topSample{}, false
	}
	return parseTop(raw), true
}

// parseTop 从 top 输出中提取指标。抽取成独立函数是为了能对固定文本做单元测试。
func parseTop(raw string) topSample {
	var s topSample

	if m := reCPUUsage.FindStringSubmatch(raw); len(m) == 4 {
		s.CPUUser, _ = strconv.ParseFloat(m[1], 64)
		s.CPUSys, _ = strconv.ParseFloat(m[2], 64)
		s.CPUIdle, _ = strconv.ParseFloat(m[3], 64)
		s.HasCPU = true
	}
	if m := reLoadAvgCSV.FindStringSubmatch(raw); len(m) == 4 {
		s.Load1, _ = strconv.ParseFloat(m[1], 64)
		s.Load5, _ = strconv.ParseFloat(m[2], 64)
		s.Load15, _ = strconv.ParseFloat(m[3], 64)
		s.HasLoad = true
	}
	if m := rePhysMem.FindStringSubmatch(raw); len(m) == 6 {
		s.MemUsed = parseSizeUnit(m[1], m[2])
		s.MemUnused = parseSizeUnit(m[4], m[5])
		s.HasMem = true
		if w := reTopWired.FindStringSubmatch(m[3]); len(w) == 3 {
			s.MemWired = parseSizeUnit(w[1], w[2])
		}
		if c := reTopCompressor.FindStringSubmatch(m[3]); len(c) == 3 {
			s.MemCompr = parseSizeUnit(c[1], c[2])
		}
	}
	if m := reTopProcesses.FindStringSubmatch(raw); len(m) == 2 {
		s.Procs, _ = strconv.Atoi(m[1])
		s.HasProcs = true
	}
	return s
}

// parseSizeUnit 把 "2544M" / "15G" 这样的值转成字节。
func parseSizeUnit(v, unit string) uint64 {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	mult := float64(1)
	switch unit {
	case "K":
		mult = 1024
	case "M":
		mult = 1024 * 1024
	case "G":
		mult = 1024 * 1024 * 1024
	case "T":
		mult = 1024 * 1024 * 1024 * 1024
	}
	return uint64(f * mult)
}

// parseVMStat 解析 `vm_stat`，返回各页统计（单位：页）。
// 内存语义（macOS 与 Linux 不同，必须区分清楚）：
//
//	free    = Pages free + Pages speculative
//	cached  = Pages inactive + Pages purgeable   ← 可被系统回收，不算"已用"
//	used    = total - free - cached
func parseVMStat(raw string) (free, cached uint64, pageSize uint64) {
	pageSize = 4096
	if m := rePages.FindStringSubmatch(raw); len(m) == 2 {
		if v, err := strconv.ParseUint(m[1], 10, 64); err == nil && v > 0 {
			pageSize = v
		}
	}
	pages := map[string]uint64{}
	for _, m := range reVMStat.FindAllStringSubmatch(raw, -1) {
		key := strings.Trim(m[1], `" `)
		v, err := strconv.ParseUint(m[2], 10, 64)
		if err != nil {
			continue
		}
		pages[key] = v
	}
	get := func(k string) uint64 { return pages[k] * pageSize }
	return get("Pages free") + get("Pages speculative"),
		get("Pages inactive") + get("Pages purgeable"),
		pageSize
}

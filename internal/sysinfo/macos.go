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
//  `top -l 1 -n 0` 只用它取 CPU / Load Avg / 进程数。内存**不从 top 取**：
//  PhysMem used = total − unused，把 inactive/文件缓存也算成"已用"，虚高。
//  内存以 vm_stat 为准（活动监视器口径），见 parseVMStat。
//
//  `kern.cp_time` 的差值算法作为兜底保留：万一某些系统版本能取到，
//  它比 top 更精确（不受 top 采样窗口影响）。
// ---------------------------------------------------------------------------

var (
	reCPUUsage     = regexp.MustCompile(`CPU usage:\s*([\d.]+)%\s*user,\s*([\d.]+)%\s*sys,\s*([\d.]+)%\s*idle`)
	reLoadAvgCSV   = regexp.MustCompile(`Load Avg:\s*([\d.]+),\s*([\d.]+),\s*([\d.]+)`)
	reTopProcesses = regexp.MustCompile(`Processes:\s*(\d+)\s+total`)
	// rePhysMemUsed 只服务 vm_stat 完全拿不到时的兜底，口径偏高（见 parseTopPhysMemUsed）。
	rePhysMemUsed = regexp.MustCompile(`PhysMem:\s*([\d.]+)([KMGT])\s+used`)
)

// topSample 是 `top -l 1 -n 0` 解析结果（只含 CPU / 负载 / 进程数）。
type topSample struct {
	CPUUser  float64
	CPUSys   float64
	CPUIdle  float64
	HasCPU   bool
	Load1    float64
	Load5    float64
	Load15   float64
	HasLoad  bool
	Procs    int
	HasProcs bool
}

// readTop 调用 top 并解析，原始输出交给调用方做内存兜底。
//
// 超时设为 8 秒：top -l 1 的首次采样在某些负载高的机器上可能接近 2 秒，
// 留足余量避免误判失败。这里不能用 sysinfo 默认的 3 秒超时。
func readTop(ctx context.Context) (topSample, string, bool) {
	raw := runCmdTimeout(ctx, 8*time.Second, "top", "-l", "1", "-n", "0")
	if strings.TrimSpace(raw) == "" {
		return topSample{}, "", false
	}
	return parseTop(raw), raw, true
}

// parseTop 从 top 输出中提取 CPU / 负载 / 进程数。抽取成独立函数是为了能对固定文本做单元测试。
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

// ---------------------------------------------------------------------------
//  内存口径（活动监视器语义）
//
//  top 的 PhysMem used = total − unused，把 inactive/文件缓存也算成"已用"，
//  于是"used + free + cached > total"这种三种口径混用会吓到用户。
//  活动监视器把内存分成四块，本文件按它算：
//
//	应用 = Anonymous pages − Pages purgeable（负值夹 0）
//	有线 = Pages wired down      压缩 = Pages occupied by compressor
//	已用 = 应用 + 有线 + 压缩    缓存 = File-backed pages
//	可用 = total − 已用 − 缓存   （整数运算，保证三者之和恰为 total）
//
//  页大小缺失或必需行缺失 ⇒ Verified=false，如实标"未复核"，绝不猜默认值。
// ---------------------------------------------------------------------------

// memSample 是一次内存口径计算结果（字节）。
type memSample struct {
	Total    uint64
	Used     uint64 // App + Wired + Compr
	App      uint64
	Wired    uint64
	Compr    uint64
	Cached   uint64 // file-backed
	Free     uint64 // Total − Used − Cached
	Verified bool   // 页大小与必需行齐全（false 时不要把数值当准的）
	Note     string // 未复核原因（给用户看的一句中文）
}

// vmStatRequired 是必须出现的 vm_stat 行。缺任何一行都不猜测、直接标未复核。
var vmStatRequired = []string{
	"Pages free",
	"Pages active",
	"Pages inactive",
	"Pages speculative",
	"Pages wired down",
	"Pages purgeable",
	"File-backed pages",
	"Anonymous pages",
	"Pages occupied by compressor",
}

// parseVMStat 解析 `vm_stat` 的固定文本，按活动监视器口径算内存。
// 页大小只认首行的 "page size of N bytes"；缺失或必需行缺失 ⇒ Verified=false。
func parseVMStat(raw string, total uint64) memSample {
	s := memSample{Total: total}

	pageSize := uint64(0)
	if m := rePages.FindStringSubmatch(raw); len(m) == 2 {
		if v, err := strconv.ParseUint(m[1], 10, 64); err == nil && v > 0 {
			pageSize = v
		}
	}

	pages := map[string]uint64{}
	for _, m := range reVMStat.FindAllStringSubmatch(raw, -1) {
		key := strings.Trim(m[1], `" `)
		if v, err := strconv.ParseUint(m[2], 10, 64); err == nil {
			pages[key] = v
		}
	}

	var missing []string
	if pageSize == 0 {
		missing = append(missing, "页面大小")
	}
	for _, k := range vmStatRequired {
		if _, ok := pages[k]; !ok {
			missing = append(missing, k)
		}
	}
	if total == 0 {
		missing = append(missing, "物理内存总量")
	}
	if len(missing) > 0 {
		s.Note = "vm_stat 未复核（缺 " + strings.Join(missing, "、") + "）"
	}
	if pageSize == 0 || total == 0 {
		// 没有页大小/总量就算不出字节数；返回全 0 + Note，让上层走兜底或如实显示"未复核"。
		return s
	}
	s.Verified = len(missing) == 0

	// 应用内存 = 匿名页 − 可清除页（purgeable 可能大于 anonymous，夹到 0）。
	app := pages["Anonymous pages"]
	if purge := pages["Pages purgeable"]; purge < app {
		app -= purge
	} else {
		app = 0
	}
	s.App = app * pageSize
	s.Wired = pages["Pages wired down"] * pageSize
	s.Compr = pages["Pages occupied by compressor"] * pageSize
	s.Cached = pages["File-backed pages"] * pageSize
	s.Used = s.App + s.Wired + s.Compr

	// 整数运算保证 used + cached + free == total（页级取整也不破坏自洽）。
	if s.Used+s.Cached > total {
		if s.Cached > total {
			s.Cached = total
		}
		s.Used = total - s.Cached
	}
	s.Free = total - s.Used - s.Cached
	s.Total = s.Used + s.Cached + s.Free
	return s
}

// parseTopPhysMemUsed 是 vm_stat 完全拿不到时的兜底：取 top 的 PhysMem used。
// 口径偏高（含文件缓存），调用方必须保持 Verified=false 并说明原因。
func parseTopPhysMemUsed(raw string) (uint64, bool) {
	m := rePhysMemUsed.FindStringSubmatch(raw)
	if len(m) != 3 {
		return 0, false
	}
	v := parseSizeUnit(m[1], m[2])
	return v, v > 0
}

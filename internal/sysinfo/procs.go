package sysinfo

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 进程列表的 CPU 数值必须自己采样计算，不能用 ps 的 %CPU。
//
// 原因（实测踩到的坑）：`ps` 的 %CPU 是"自进程启动以来的平均 CPU 占用"或
// 极短窗口的瞬时值。用 `top -l 1` 那种一次性快照取进程列表时，面板进程的
// 数值会显示成 73.7% —— 因为那一刻它刚好 fork 了 top 子进程。
// 换成两次 cputime 采样求差，同一进程实测为 0.0%，与真实情况一致。
//
// 实现：cputime 是进程累计 CPU 时间（HH:MM:SS 或 MM:SS.ss），
// 两次采样之差 ÷ 时间间隔 = 该区间真实 CPU 占用率。

// Proc 是一条进程信息。
type Proc struct {
	PID     int     `json:"pid"`
	CPU     float64 `json:"cpu"` // 百分比，按采样区间计算
	Mem     float64 `json:"mem"` // 常驻内存百分比
	RSS     uint64  `json:"rss"` // KB
	Etime   string  `json:"etime"`
	Command string  `json:"command"`
	User    string  `json:"user"`
}

// ProcSampler 通过两次采样计算进程真实 CPU 占用。
type ProcSampler struct {
	mu       sync.Mutex
	last     map[int]procSample
	lastTime time.Time
	prev     []Proc
}

type procSample struct {
	cpuTime float64 // 累计 CPU 秒数
}

// NewProcSampler 创建采样器。
func NewProcSampler() *ProcSampler {
	return &ProcSampler{last: map[int]procSample{}}
}

// Sample 采集一次进程列表。
//
// 返回的 CPU 百分比基于"距上次调用"的区间；首次调用（无历史）时 CPU 为 0，
// 前端应在 1-2 秒后刷新一次以获得准确值 —— 这比给出一个错误数字要好。
func (s *ProcSampler) Sample(ctx context.Context, limit int) []Proc {
	raw := readProcesses(ctx)
	list := make([]Proc, 0, len(raw))
	cpuSeconds := make(map[int]float64, len(raw))
	for _, r := range raw {
		cpuSeconds[r.PID] = r.cpuSeconds
		list = append(list, Proc{
			PID: r.PID, User: r.User, Mem: r.Mem, RSS: r.RSS,
			Etime: r.Etime, Command: r.Command,
		})
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	dt := now.Sub(s.lastTime).Seconds()
	first := s.lastTime.IsZero()

	for i := range list {
		if first {
			continue // 无历史，CPU 留 0，等下一次采样给出真实值
		}
		if dt <= 0.2 {
			break
		}
		prev, ok := s.last[list[i].PID]
		if !ok {
			continue // 新出现的进程，没有基线
		}
		diff := cpuSeconds[list[i].PID] - prev.cpuTime
		if diff < 0 { // 进程重启或计数器回绕
			diff = 0
		}
		pct := diff / dt * 100
		if pct > 100 {
			pct = 100 // 单进程不会长期超过 100%，超过说明采样异常
		}
		list[i].CPU = round2(pct)
	}
	s.last = make(map[int]procSample, len(cpuSeconds))
	for pid, cs := range cpuSeconds {
		s.last[pid] = procSample{cpuTime: cs}
	}
	s.lastTime = now

	// 按 CPU 降序排列
	sortProcs(list)

	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	// 缓存一份，便于在采样间隔内快速响应
	s.prev = list
	return list
}

// Last 返回上次采样结果。
func (s *ProcSampler) Last() []Proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prev
}

func sortProcs(list []Proc) {
	// 插入排序：列表通常只有几百条且基本有序，比 sort.Slice 更省分配
	for i := 1; i < len(list); i++ {
		v := list[i]
		j := i - 1
		for j >= 0 && list[j].CPU < v.CPU {
			list[j+1] = list[j]
			j--
		}
		list[j+1] = v
	}
}

// readProcesses 调用 ps 读取进程快照（含累计 CPU 时间）。
func readProcesses(ctx context.Context) []procRaw {
	// cputime 在不同 macOS 版本可能是 HH:MM:SS 或 MM:SS.ss，两种都要解析
	out := runCmd(ctx, "ps", "-axo", "pid,user,pcpu,pmem,rss,etime,cputime,comm")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return nil
	}
	list := make([]procRaw, 0, len(lines))
	for _, ln := range lines[1:] {
		f := strings.Fields(ln)
		if len(f) < 8 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		pmem, _ := strconv.ParseFloat(f[3], 64)
		rss, _ := strconv.ParseUint(f[4], 10, 64)
		list = append(list, procRaw{
			PID:        pid,
			User:       f[1],
			Mem:        pmem,
			RSS:        rss,
			Etime:      f[5],
			cpuSeconds: parseCPUTime(f[6]),
			Command:    shortenPath(strings.Join(f[7:], " ")),
		})
	}
	return list
}

type procRaw struct {
	PID        int
	User       string
	Mem        float64
	RSS        uint64
	Etime      string
	cpuSeconds float64
	Command    string
}

// shortenPath 把可执行文件路径压缩成 "父目录/文件名"。
// 完整路径会把表格挤爆，但只留文件名又难以区分同名程序（如多个 python）。
func shortenPath(p string) string {
	if p == "" {
		return p
	}
	parts := strings.Split(p, "/")
	base := parts[len(parts)-1]
	if strings.HasPrefix(base, "(") { // 内核线程形如 (foo)
		return base
	}
	if len(parts) > 1 {
		return parts[len(parts)-2] + "/" + base
	}
	return base
}

// parseCPUTime 解析 ps 的 cputime 字段。
//
//	"0:01.54"     -> 1.54 秒
//	"1:02:03"     -> 3723 秒
//	"12:34.56"    -> 754.56 秒
//	"1-02:03:04"  -> 天-时:分:秒
func parseCPUTime(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var days float64
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.ParseFloat(s[:i], 64)
		if err != nil {
			return 0
		}
		days = d
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		total = total*60 + v
	}
	return days*86400 + total
}

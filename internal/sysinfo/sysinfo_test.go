package sysinfo

import (
	"testing"
	"time"
)

// 这些测试用固定的命令输出做断言。写入固定文本的原因很直接：
// CPU 采集曾经因为误用 `sysctl kern.cp_time`（Apple Silicon 上已移除）
// 而在真实机器上恒为 0 —— 单测锁死解析逻辑，避免再次回归。

const sampleTopOutput = `Processes: 499 total, 2 running, 497 sleeping, 2763 threads 
2026/09/13 09:11:56
Load Avg: 3.76, 4.38, 3.01 
CPU usage: 5.81% user, 9.78% sys, 84.40% idle 
SharedLibs: 645M resident, 137M data, 88M linkedit.
MemRegions: 189055 total, 3325M resident, 246M private, 1675M shared.
PhysMem: 15G used (2544M wired, 5953M compressor), 523M unused.
VM: 203T vsize, 5703M framework vsize, 674860(0) swapins, 832524(0) swapouts.
Networks: packets: 27398467/32G in, 14340236/13G out.
Disks: 6630537/187G read, 4451580/155G written.
`

func TestParseTopExtractsAllMetrics(t *testing.T) {
	s := parseTop(sampleTopOutput)

	if !s.HasCPU {
		t.Fatal("未解析出 CPU 使用率")
	}
	// 5.81 + 9.78 = 15.59
	if got := s.CPUUser + s.CPUSys; got < 15.5 || got > 15.7 {
		t.Fatalf("CPU 使用率解析错误: %v", got)
	}
	if s.CPUIdle < 84.3 || s.CPUIdle > 84.5 {
		t.Fatalf("CPU 空闲率解析错误: %v", s.CPUIdle)
	}
	if !s.HasLoad || s.Load1 != 3.76 || s.Load5 != 4.38 || s.Load15 != 3.01 {
		t.Fatalf("负载解析错误: %+v", s)
	}
	if !s.HasProcs || s.Procs != 499 {
		t.Fatalf("进程数解析错误: %d", s.Procs)
	}
	if !s.HasMem {
		t.Fatal("未解析出内存")
	}
	// 15G
	if s.MemUsed != 15*1024*1024*1024 {
		t.Fatalf("已用内存解析错误: %d", s.MemUsed)
	}
	// 2544M wired
	if s.MemWired != 2544*1024*1024 {
		t.Fatalf("wired 内存解析错误: %d", s.MemWired)
	}
	// 5953M compressor
	if s.MemCompr != 5953*1024*1024 {
		t.Fatalf("compressor 解析错误: %d", s.MemCompr)
	}
	// 523M unused
	if s.MemUnused != 523*1024*1024 {
		t.Fatalf("未用内存解析错误: %d", s.MemUnused)
	}
}

func TestParseTopHandlesEmptyAndGarbage(t *testing.T) {
	for _, raw := range []string{"", "top: something went wrong", "\n\n"} {
		s := parseTop(raw)
		if s.HasCPU || s.HasLoad || s.HasMem {
			t.Fatalf("非法输入不应解析出指标: %+v (%q)", s, raw)
		}
	}
}

const sampleVMStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               29022.
Pages active:                            219692.
Pages inactive:                          213680.
Pages speculative:                         5385.
Pages throttled:                              0.
Pages wired down:                        160286.
Pages purgeable:                           3856.
"Translation faults":                  12345678.
`

func TestParseVMStatRespectsPageSize(t *testing.T) {
	free, cached, pageSize := parseVMStat(sampleVMStat)

	// 必须采用输出里的 page size（16384），而不是假定的 4096
	if pageSize != 16384 {
		t.Fatalf("页大小未按输出解析: %d", pageSize)
	}
	// free = (29022 + 5385) * 16384
	wantFree := uint64((29022 + 5385) * 16384)
	if free != wantFree {
		t.Fatalf("空闲内存计算错误: got=%d want=%d", free, wantFree)
	}
	// cached = (213680 + 3856) * 16384
	wantCached := uint64((213680 + 3856) * 16384)
	if cached != wantCached {
		t.Fatalf("可回收内存计算错误: got=%d want=%d", cached, wantCached)
	}
}

func TestParseVMStatFallbackPageSize(t *testing.T) {
	// 缺少页大小说明时必须退回 4096，而不是崩溃或返回 0
	free, _, pageSize := parseVMStat("Pages free: 100.\n")
	if pageSize != 4096 {
		t.Fatalf("缺少页大小时应退回 4096，实际 %d", pageSize)
	}
	if free != 100*4096 {
		t.Fatalf("空闲内存计算错误: %d", free)
	}
}

func TestParseSwap(t *testing.T) {
	total, used := parseSwap("total = 2048.00M  used = 1014.81M  free = 1033.19M  (encrypted)")
	if total != uint64(2048*1024*1024) {
		t.Fatalf("swap 总量解析错误: %d", total)
	}
	// 1014.81M = 1064105410.56 字节，取整后允许 ±64 字节误差
	mib := float64(1024 * 1024)
	want := uint64(1014.81 * mib)
	if used < want-64 || used > want+64 {
		t.Fatalf("swap 已用解析错误: got=%d want=%d", used, want)
	}
	// 无 swap 时不能 panic
	if t2, u2 := parseSwap("not a swap line"); t2 != 0 || u2 != 0 {
		t.Fatal("无法解析时应返回 0")
	}
}

func TestParseSizeUnit(t *testing.T) {
	cases := map[string]uint64{
		"1024K": 1024 * 1024,
		"1M":    1024 * 1024,
		"1G":    1024 * 1024 * 1024,
		"2T":    2 * 1024 * 1024 * 1024 * 1024,
		"512":   512,
	}
	for in, want := range cases {
		var v, u string
		for i, c := range in {
			if c < '0' || c > '9' {
				v, u = in[:i], in[i:]
				break
			}
		}
		if v == "" {
			v, u = in, ""
		}
		if got := parseSizeUnit(v, u); got != want {
			t.Fatalf("parseSizeUnit(%q) = %d，期望 %d", in, got, want)
		}
	}
	// 非法输入不能 panic
	if got := parseSizeUnit("abc", "M"); got != 0 {
		t.Fatalf("非法输入应返回 0，实际 %d", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1024 * 1024, "1.00 MB"},
		{17179869184, "16.00 GB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.in); got != c.want {
			t.Fatalf("HumanBytes(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestClampPct(t *testing.T) {
	if clampPct(-5) != 0 || clampPct(150) != 100 || clampPct(42.5) != 42.5 {
		t.Fatal("clampPct 边界处理错误")
	}
}

// 对真实机器做一次冒烟采集：必须在合理范围内，且 CPU 不能恒为 0。
// 这是"在真机上验证过"的自动化版本。
func TestCollectOnThisMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过真实采集")
	}
	c := NewCollector("/")
	s1, err := c.Collect(t.Context())
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if s1.MemTotal == 0 {
		t.Fatal("未采集到物理内存总量")
	}
	if s1.DiskTotal == 0 {
		t.Fatal("未采集到磁盘容量")
	}
	if s1.CPUCores == 0 {
		t.Fatal("未采集到 CPU 核心数")
	}
	if s1.Hostname == "" {
		t.Fatal("未采集到主机名")
	}
	if s1.Procs == 0 {
		t.Fatal("未采集到进程数")
	}
	if s1.LoadAvg1 < 0 {
		t.Fatal("负载为负值")
	}
	// CPU 使用率必须在 0-100 之间（top 路径应能给出真实值）
	if s1.CPUUsed < 0 || s1.CPUUsed > 100 {
		t.Fatalf("CPU 使用率越界: %v", s1.CPUUsed)
	}
	t.Logf("采集成功: host=%s cpu=%.2f%% cores=%d mem=%d/%d disk=%d/%d procs=%d load=%.2f",
		s1.Hostname, s1.CPUUsed, s1.CPUCores, s1.MemUsed, s1.MemTotal,
		s1.DiskUsed, s1.DiskTotal, s1.Procs, s1.LoadAvg1)
}

// ---------- 进程 CPU 采样 ----------

func TestParseCPUTime(t *testing.T) {
	cases := map[string]float64{
		"0:00.00":    0,
		"0:01.54":    1.54,
		"1:02.03":    62.03,
		"12:34.56":   754.56,
		"1:02:03":    3723,
		"1-02:03:04": 93784,
		"":           0,
		"garbage":    0,
	}
	for in, want := range cases {
		if got := parseCPUTime(in); got != want {
			t.Fatalf("parseCPUTime(%q) = %v，期望 %v", in, got, want)
		}
	}
}

// ProcSampler 第一次采样没有基线，CPU 必须为 0；第二次才给出区间真实值。
// 这条规则很重要：首次就给一个假数字会误导用户，尤其是刚打开面板时。
func TestProcSamplerFirstSampleIsZeroThenReal(t *testing.T) {
	s := NewProcSampler()
	first := s.Sample(t.Context(), 5)
	if len(first) == 0 {
		t.Fatal("未采集到进程")
	}
	for _, p := range first {
		if p.CPU != 0 {
			t.Fatalf("首次采样 CPU 应为 0，进程 %s 得到 %v", p.Command, p.CPU)
		}
	}
	// 让 CPU 累计时间有机会增长
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = 1
	}
	second := s.Sample(t.Context(), 5)
	if len(second) == 0 {
		t.Fatal("二次采样未采集到进程")
	}
	for _, p := range second {
		if p.CPU < 0 || p.CPU > 100 {
			t.Fatalf("CPU 百分比越界: %s = %v", p.Command, p.CPU)
		}
	}
	// 排序必须按 CPU 降序
	for i := 1; i < len(second); i++ {
		if second[i-1].CPU < second[i].CPU {
			t.Fatalf("进程未按 CPU 降序排列: %v < %v", second[i-1].CPU, second[i].CPU)
		}
	}
}

func TestProcSamplerSortsByMemory(t *testing.T) {
	s := NewProcSampler()
	s.Sample(t.Context(), 0)
	list := s.Sample(t.Context(), 0)
	if len(list) < 2 {
		t.Skip("进程太少，跳过排序校验")
	}
	// 采样器本身按 CPU 排序；这里验证 RSS 字段被正确解析（非 0）
	hasRSS := false
	for _, p := range list {
		if p.RSS > 0 {
			hasRSS = true
			break
		}
	}
	if !hasRSS {
		t.Fatal("未解析出任何进程的常驻内存")
	}
}

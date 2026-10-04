package videoopt

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// EnvNotes 在任务开始时如实报告**环境**异常：有别的 ffmpeg 在抢 CPU、
// swap 吃紧、负载超过核数。只提示 —— 不改任何转码参数，也绝不动别人的进程。
//
// 为什么需要（2026-10-04 用户报障）：CPU 只有 56.6%，负载却 15.75，速度从
// 5~8x 掉到 1x。本机实测同一条 4K→480p 只用到 8 核、负载 ≈5，所以差距只能
// 来自外部争抢或 I/O 等待 —— 面板必须把它说出来，而不是让用户猜。
func EnvNotes() []string {
	used, total := swapUsedTotalMB()
	return envNotesFrom(countPeerFFmpeg(), used, total, load1(), runtime.NumCPU())
}

// envNotesFrom 是纯函数（门禁直接喂数字，不碰真机进程与 sysctl）。
func envNotesFrom(peers, swapUsedMB, swapTotalMB int, load1 float64, cores int) []string {
	var out []string
	if peers > 0 {
		out = append(out, fmt.Sprintf("检测到 %d 个其它 ffmpeg 进程，可能在抢 CPU", peers))
	}
	if swapTotalMB > 0 && swapUsedMB >= 1024 && swapUsedMB*2 >= swapTotalMB {
		out = append(out, fmt.Sprintf("内存吃紧（swap 已用 %d/%d MB），转码会变慢", swapUsedMB, swapTotalMB))
	}
	if cores > 0 && load1 > float64(cores) {
		out = append(out, fmt.Sprintf("系统负载 %.1f 超过 %d 核，转码在和别的东西抢资源", load1, cores))
	}
	return out
}

// countPeerFFmpeg 按**进程名**数 ffmpeg：用 pgrep -f / ps args 匹配整条命令行
// 会把别的 shell、面板自己的命令行也算进来（实测误报 3 个）。
func countPeerFFmpeg() int {
	out, err := exec.Command("ps", "-A", "-o", "comm=").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if filepath.Base(strings.TrimSpace(line)) == "ffmpeg" {
			n++
		}
	}
	return n
}

// swapUsedTotalMB 读 `sysctl vm.swapusage`（只读，无需 sudo；读不到按 0 处理）。
func swapUsedTotalMB() (used, total int) {
	out, err := exec.Command("sysctl", "-n", "vm.swapusage").Output()
	if err != nil {
		return 0, 0
	}
	f := strings.Fields(string(out))
	for i, tok := range f {
		if i+2 >= len(f) {
			break
		}
		switch tok {
		case "total":
			total = mbFromSysctl(f[i+2])
		case "used":
			used = mbFromSysctl(f[i+2])
		}
	}
	return used, total
}

// load1 读 1 分钟平均负载（macOS 的 loadavg 含不可中断等待，正是"CPU 不满但负载高"的来源）。
func load1() float64 {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0
	}
	f := strings.Fields(strings.Trim(string(out), "{} \n"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// mbFromSysctl 把 "3072.00M" / "1.5G" 这类值换算成 MB。
func mbFromSysctl(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1.0 / 1024
	case 'G', 'g':
		mult = 1024
	case 'T', 't':
		mult = 1024 * 1024
	}
	v, err := strconv.ParseFloat(strings.TrimRight(s, "KkMmGgTt"), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return int(v * mult)
}

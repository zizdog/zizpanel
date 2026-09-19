package main

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// 普通用户必须能正确判断 root 进程是否存活。
//
// 这条测试锁死一个真实踩到的 bug：曾经用 kill(pid, 0) 探测，
// 而普通用户向 root 进程发信号会返回 EPERM，
// 于是 `zizpanel status` 在面板明明运行时报告"已停止"。
func TestProcessAliveAsNonRoot(t *testing.T) {
	// 当前测试进程自己就是最好的样本
	if !processAlive(os.Getpid()) {
		t.Fatal("应当检测到自身进程存活")
	}
	// 不存在的 pid 必须返回 false，而不是误判为存活
	if processAlive(999999) {
		t.Fatal("不存在的 pid 不应被判为存活")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("非法 pid 必须返回 false")
	}
}

// 如果本机存在 root 进程（launchd 本身），普通用户也必须能正确判断。
func TestProcessAliveForRootProcess(t *testing.T) {
	// 样本用 **PID 1**（launchd：root、永远在；普通用户向它发信号只会拿到 EPERM ——
	// 正是老实现误报"已停止"的那个场景）。
	// 以前这里取 `ps` 输出里**第一个** root 进程：那个 PID 完全可能在 `ps` 与判定之间
	// 就退出（2026-09-19 实测踩到，pid 164），于是门禁偶发失败，报出来的却是
	// "普通用户检测不到 root 进程"——看起来像功能坏了，其实是被测对象消失了。
	// 教训：门禁的样本必须**寿命长于测试**，否则测的是调度运气。
	if !processAlive(1) {
		t.Fatalf("普通用户应能检测到 root 进程 1（launchd）存活（这正是之前的 bug）")
	}
	// 再顺带看一个别的 root 进程：有就一并验，取不到 / 刚好退出都不算失败。
	out, err := exec.Command("/bin/ps", "-axo", "pid,user").Output()
	if err != nil {
		return
	}
	for _, ln := range splitLines(string(out)) {
		f := fields(ln)
		if len(f) != 2 || f[1] != "root" {
			continue
		}
		n, err := strconv.Atoi(f[0])
		if err != nil || n <= 1 {
			continue
		}
		if !processAlive(n) {
			t.Logf("样本 pid %d 在采样后消失（竞态，不计失败）", n)
		}
		return
	}
}

func TestReadPIDMissingFile(t *testing.T) {
	if got := readPID("/nonexistent/path/panel.pid"); got != 0 {
		t.Fatalf("缺失的 pid 文件应返回 0，实际 %d", got)
	}
}

// 小工具：避免为测试引入 strings/其他依赖
func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func fields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

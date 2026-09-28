// fulldiskauth.go —— 一次性"请求完全磁盘访问授权"（安装脚本写标记、守护进程读一次）。
// 与 files.volumeauth.go 同一套纪律：有图形登录会话才读一次（这一次读就弹窗），
// 读完即删；没人在屏幕前一个字节都不读 —— 弹了没人点，系统只会记成 denial。
package permissions

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// fullDiskAuthMarkerName 是一次性授权请求标记的文件名（放在 DataDir 下）。
const fullDiskAuthMarkerName = "request-full-disk-auth.once"

// FullDiskAuthMarkerPath 返回一次性标记的完整路径。
func FullDiskAuthMarkerPath(dataDir string) string {
	return installPhasePath(dataDir, fullDiskAuthMarkerName)
}

// fullDiskProbeFn 是实际去读受保护目录的动作。变量而非常量：
// 单测必须能断言"没有登录会话时**一次都没读**"，而不去碰真实受保护目录。
var fullDiskProbeFn Probe = ReadProtected

// FullDiskAuthResult 描述一次"一次性完全磁盘访问授权请求"的处理结果。
type FullDiskAuthResult struct {
	// Skipped 为 true 表示这次**什么都没有读**（没有标记 / 没有登录会话 / 找不到目录）。
	Skipped bool
	// Reason 说明跳过或结束的原因（写进日志，用户能看懂）。
	Reason string
	// ConsoleUser 是判定时的控制台登录用户（空 = 没人在屏幕前）。
	ConsoleUser string
	// Readable / Denied 是本次读到的受保护目录数（Denied 即触发过弹窗的）。
	Readable int
	Denied   int
	// MarkerConsumed 表示一次性标记已被删除（下次启动不会再碰）。
	MarkerConsumed bool
}

// RequestFullDiskAuthorizationOnce 处理一次性完全磁盘访问授权请求（见文件头）。
//
// consoleUser 由调用方传入（守护进程用 files.ConsoleUser，与面板别处**同一判据**）；
// wait 是"等用户在弹窗上点允许"的最长时间：期间按 1 秒轮询重读；0 表示只读一次。
func RequestFullDiskAuthorizationOnce(ctx context.Context, markerPath, home, consoleUser string, wait time.Duration, logf func(string, ...any)) FullDiskAuthResult {
	log := func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	}
	res := FullDiskAuthResult{}

	if _, err := os.Stat(markerPath); err != nil {
		res.Skipped = true
		res.Reason = "没有一次性完全磁盘访问授权请求标记（不是真机安装时同意的，就不读受保护目录）"
		return res
	}

	// ① 有没有人坐在屏幕前？没有就**什么都不读**（铁律：不触发弹窗），标记保留。
	res.ConsoleUser = strings.TrimSpace(consoleUser)
	if !ConsoleOK(res.ConsoleUser) {
		res.Skipped = true
		res.Reason = fmt.Sprintf("当前没有图形登录会话（控制台用户 %q）——不触发任何系统弹窗；"+
			"标记保留，等有人在机器前时下次启动再试", res.ConsoleUser)
		log("跳过完全磁盘访问授权请求：%s", res.Reason)
		return res
	}

	targets := ProtectedDirCandidates(home)
	if len(targets) == 0 {
		res.Skipped = true
		res.Reason = "拿不到控制台用户的家目录，找不到要申请的受保护目录"
		_ = os.Remove(markerPath)
		res.MarkerConsumed = true
		log("跳过完全磁盘访问授权请求：%s（已消费一次性标记）", res.Reason)
		return res
	}

	// ② 真的去读一次 —— 这一步才会让 macOS 弹出「完全磁盘访问权限」询问。
	log("按安装时的同意，向系统申请完全磁盘访问（屏幕上可能弹出授权询问，请点「允许」）")
	deadline := time.Now().Add(wait)
	for {
		counts := fullDiskProbeFn(ctx, targets)
		res.Readable, res.Denied = 0, 0
		for _, pr := range counts {
			switch {
			case pr.Readable:
				res.Readable++
			case pr.Denied:
				res.Denied++
			}
		}
		if res.Denied == 0 || wait <= 0 || !time.Now().Before(deadline) || ctx.Err() != nil {
			break
		}
		log("受保护目录仍被拒（授权询问应已弹出），等待用户点「允许」…")
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}

	// ③ 一次性：用过就删，绝不反复打扰。
	_ = os.Remove(markerPath)
	res.MarkerConsumed = true
	return res
}

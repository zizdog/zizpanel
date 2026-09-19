package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ============================================================================
//  一次性"请求系统授权访问外接卷"
//
//  🚨 用户 2026-09-19 明确的铁律（安装脚本与面板都必须遵守）：
//    · **真机安装**才可以弹窗要权限（人就在机器前，点一下就完事）；
//    · **远程/无人在场时，绝不触发任何系统授权弹窗** —— 弹了也没人点，
//      系统只会把它记成 denial，反而让后续访问一直是"静默被拒"。
//
//  所以"什么时候去碰外接卷"不能由面板自己随时决定，而是：
//    ① 安装脚本判断这是真机安装 + 用户当面同意 → 写下一个**一次性标记**
//       （<DataDir>/request-volume-auth.once，只有 root 能写）；
//    ② 守护进程启动时看到标记，且**当前有可交互的图形登录会话**，才去读一次外接卷；
//       这一次读就是触发 macOS 弹窗的动作（身份是面板二进制本身，见下）；
//    ③ 读完（无论允许还是被拒）就把标记删掉 —— 一次性，绝不反复打扰。
//
//  为什么必须由**守护进程**去做这次读：TCC 的授权是发给"访问者二进制"的。
//  在终端里跑 `zizpanel xxx` 触发的是**终端 App** 的授权（responsible process 是终端），
//  对面板自己毫无用处；只有 launchd 守护进程发起的访问才会落到面板二进制上。
//
//  判据全部可注入（consoleUserFn / volumeMountsFn / readDirFn），
//  这样单测能断言"没有登录会话时**一个字节都没读**"，而不用真的插一块盘。
// ============================================================================

// volumeAuthMarkerName 是一次性授权请求标记的文件名（放在 DataDir 下）。
const volumeAuthMarkerName = "request-volume-auth.once"

// VolumeAuthMarkerPath 返回一次性标记的完整路径。
func VolumeAuthMarkerPath(dataDir string) string {
	return filepath.Join(filepath.Clean(dataDir), volumeAuthMarkerName)
}

// consoleUserFn 返回当前图形控制台（物理屏幕）的登录用户名；没有登录会话时返回空串。
//
// macOS 上 /dev/console 的属主就是"谁在屏幕前"。root 与 loginwindow
// 都表示"没人在"（登录窗口界面 / 无 GUI），此时**绝不允许**触发任何弹窗。
var consoleUserFn = consoleUserReal

func consoleUserReal() string {
	out, err := exec.Command("/usr/bin/stat", "-f", "%Su", "/dev/console").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// volumeMountsForAuthFn 是"要申请授权的外接卷列表"的来源（默认复用 NonSystemVolumeMounts）。
// 变量而非常量：单测要能造出"有没有外接卷"两种情形，而不依赖跑测试那台机器插没插盘。
var volumeMountsForAuthFn = NonSystemVolumeMounts

// readDirFn 是实际去读卷的动作。变量而非常量：单测必须能断言"没有登录会话时没有被调用"。
var readDirFn = os.ReadDir

// VolumeAuthResult 描述一次"一次性授权请求"的处理结果（供日志与测试断言）。
type VolumeAuthResult struct {
	// Skipped 为 true 表示这次**什么都没有读**（没有标记 / 没有登录会话 / 没有外接卷）。
	Skipped bool
	// Reason 说明跳过或结束的原因（写进日志，用户能看懂）。
	Reason string
	// ConsoleUser 是判定时的控制台登录用户（空 = 没人在屏幕前）。
	ConsoleUser string
	// Attempted 是"确实读了、但被拒"的卷（这些就是触发过系统弹窗的）。
	Attempted []string
	// Okay 是读得到的卷（已经授权过）。
	Okay []string
	// MarkerConsumed 表示一次性标记已被删除（下次启动不会再碰）。
	MarkerConsumed bool
}

// RequestVolumeAuthorizationOnce 处理一次性授权请求（见文件头说明）。
//
// wait 是"等用户在弹窗上点允许"的最长时间：期间按 1 秒轮询重读。
// 传 0 表示只读一次、不等。logf 可以为 nil。
func RequestVolumeAuthorizationOnce(ctx context.Context, markerPath string, wait time.Duration, logf func(string, ...any)) VolumeAuthResult {
	log := func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	}
	res := VolumeAuthResult{}

	if _, err := os.Stat(markerPath); err != nil {
		res.Skipped = true
		res.Reason = "没有一次性授权请求标记（不是真机安装时同意的，就不碰外接卷）"
		return res
	}

	// ① 有没有人坐在屏幕前？没有就**什么都不读**（铁律：不触发弹窗）。
	res.ConsoleUser = strings.TrimSpace(consoleUserFn())
	switch res.ConsoleUser {
	case "", "root", "loginwindow":
		res.Skipped = true
		res.Reason = fmt.Sprintf("当前没有图形登录会话（控制台用户 %q）——不触发任何系统弹窗；"+
			"标记保留，等有人在机器前时下次启动再试", res.ConsoleUser)
		log("跳过外接卷授权请求：%s", res.Reason)
		return res
	}

	// ② 有没有外接卷可读？没有就消费掉标记（没什么可授权的）。
	mounts := volumeMountsForAuthFn()
	if len(mounts) == 0 {
		res.Skipped = true
		res.Reason = "当前没有检测到任何非系统卷（外接盘）"
		_ = os.Remove(markerPath)
		res.MarkerConsumed = true
		log("跳过外接卷授权请求：%s（已消费一次性标记）", res.Reason)
		return res
	}

	// ③ 真的去读一次 —— 这一步才会让 macOS 弹出授权询问。
	log("按安装时的同意，向系统申请访问 %d 个外接卷（屏幕上可能弹出授权询问，请点「允许」）", len(mounts))
	for _, m := range mounts {
		if ctx.Err() != nil {
			break
		}
		_, err := readDirFn(m)
		if err == nil {
			res.Okay = append(res.Okay, m)
			continue
		}
		if !volumeAuthPermissionDenied(err) {
			// 不是权限问题（例如卷已卸载）→ 不算授权失败，也不该触发弹窗。
			log("外接卷 %s 读取失败（非权限原因，跳过）：%v", m, err)
			continue
		}
		res.Attempted = append(res.Attempted, m)
		log("外接卷 %s 被系统拒绝（%v）——授权询问应已弹出，等待用户点「允许」…", m, err)
		// 等用户点弹窗；点完之后同一路径就能读了。
		if wait > 0 {
			deadline := time.Now().Add(wait)
		waitLoop:
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					break waitLoop
				case <-time.After(time.Second):
				}
				if _, rerr := readDirFn(m); rerr == nil {
					res.Okay = append(res.Okay, m)
					log("外接卷 %s 已可访问（授权成功）", m)
					break waitLoop
				}
			}
		}
		if !containsStr(res.Okay, m) {
			log("外接卷 %s 仍未授权；"+
				"可在真机上打开「系统设置 → 隐私与安全性 → 完全磁盘访问权限」，把面板二进制加进去", m)
		}
	}

	// ④ 一次性：用过就删，绝不反复打扰（每次启动都弹窗是最讨人厌的行为）。
	_ = os.Remove(markerPath)
	res.MarkerConsumed = true
	return res
}

// volumeAuthPermissionDenied 判断错误链上是否有真实的权限拒绝（EPERM/EACCES）。
//
// 刻意不复用 web 包里的同名判据（那是个未导出的实现细节，且方向相反：
// web 是"把 errno 翻译成给用户的指引"，这里是"决定要不要继续等用户点弹窗"）。
func volumeAuthPermissionDenied(err error) bool {
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

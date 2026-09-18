package services

import (
	"context"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
//  「装完复核引擎」这一类动作的超时与重试
// ---------------------------------------------------------------------------
//
// 2026-09-18 真机事故（生产机）：面板刚 `brew install whisper.cpp` 成功，
// 紧接着复核 `whisper-cli --help` 却在 **60 秒**被我们自己的超时掐断，
// 任务报「引擎装好了但跑不起来」，于是：
//   · 用户以为安装失败（其实引擎好好的）；
//   · 复核在流程里排在**下载模型**和**注册网页界面服务**之前 → 这两步全没做，
//     用户点「打开」得到 502（面板无法连接 127.0.0.1:8892）。
//
// 为什么同一台开发机上同一条命令 1 秒就完成：**首次执行的额外开销是一次性的**
// （macOS 对新二进制的完整性校验、dyld 共享缓存预热、Metal 内核首次编译…）。
// 复核失败**不等于**引擎坏 —— 所以这里给足时间，并且失败后再试一次。
//
// 为什么不是"只把超时调大到 3 分钟"：真坏的时候那样要等 3 分钟才报错，
// 而且只跑一次仍然可能被一次冷启动吃掉。重试让"冷启动"和"真坏"给出不同结论：
// 冷启动第二次必过；真坏两次都失败，总时长仍可接受。

// EngineProbeTimeout 是"装完复核引擎"的**单次**超时。
const EngineProbeTimeout = 180 * time.Second

// EngineProbeRetryDelay 是两次复核之间的等待。
//
// 1.5 秒：足够让首次执行的校验/编译结果落定，又不至于让用户等得莫名其妙。
const EngineProbeRetryDelay = 1500 * time.Millisecond

// ProbeOnce 是"跑一条命令拿输出"的最小签名（与 sysRunner.RunAsUser 系对齐）。
//
// 做成参数而不是接口方法，是为了让 STT（走 RunAsUser + stderr 回调）与
// 图片压缩（直接 exec）两条完全不同的执行路径共用同一套超时/重试判据。
type ProbeOnce func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)

// RunEngineProbeWithRetry 复核一次引擎，失败后再试一次。
//
// log 可以为 nil（单测/无任务上下文）。返回的 error 里带上两次的原因，
// 让用户和我们都能判断"是冷启动还是真坏"。
func RunEngineProbeWithRetry(ctx context.Context, log func(string), run ProbeOnce, name string, args ...string) (string, error) {
	out, err := run(ctx, EngineProbeTimeout, name, args...)
	if err == nil {
		return out, nil
	}
	first := err
	// 任务被取消/整体超时：不再占用用户的时间去重试。
	if ctx.Err() != nil {
		return "", first
	}
	if log != nil {
		log(fmt.Sprintf("引擎复核第一次没通过（%v）；%s 后自动重试一次 —— "+
			"刚装好的二进制首次执行常被系统完整性校验/首次编译拖慢，第二次通常秒过",
			first, EngineProbeRetryDelay))
	}
	select {
	case <-time.After(EngineProbeRetryDelay):
	case <-ctx.Done():
		return "", first
	}
	out2, err2 := run(ctx, EngineProbeTimeout, name, args...)
	if err2 == nil {
		if log != nil {
			log("重试通过：引擎可以跑（第一次那次失败是首次执行的冷启动开销）")
		}
		return out2, nil
	}
	return "", fmt.Errorf("%w；自动重试一次仍失败：%v", first, err2)
}

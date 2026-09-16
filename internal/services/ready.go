package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

// ============================================================================
//  统一的「就绪判定」
//
//  为什么需要这个文件 —— 这是「面板报成功、其实没起来」这一族缺陷的根因：
//  仓库里原本有 8 套各自实现的就绪判定（waitPort / waitEndpoint /
//  waitEndpointLive / waitHTTP / waitHTTPBody / waitJSONBoolValue /
//  waitIOPaintReady / waitLNMPComponent），其中只有 2 套「失败即 error」。
//  其余的只写一条 Warning 就 return nil —— 任务中心于是显示「任务完成 ✅」，
//  而服务根本没起来，服务管理里也没有它。用户拿到的是一个「安装成功」的空壳。
//
//  这里只提供**一个**语义明确的入口 assertReady：
//    · 失败**默认返回 error**（诚实优先）。只有调用方把 spec.Degrade 显式设为
//      true 才降级成 Warning，而且必须同时给出 DegradeReason；理由为空时
//      assertReady 直接报错 —— 从机制上堵住「悄悄吞掉失败」。
//    · 失败信息必须说清：期望什么、实际什么、等了多久、以及**日志尾部**；
//      没有日志时明确写「没有日志」—— 这本身就是关键信息：进程可能还没开始
//      写日志就退出了，那要去查配置/目录/权限，而不是在业务逻辑里找。
//    · 失败信息还要说清：已经完成了什么、还差什么、用户可以怎么办。
//
//  刻意不做大规模重构：只在新文件里提供它，让「装完必须真的起来」的三处用上
//  （Qwen3 TTS / release 二进制 / 音色接收端），其余调用点一律不动 ——
//  统一重构会和正在改那些文件的改动互相破坏。
// ============================================================================

// readyVerdict 是一次就绪观测的结果。
type readyVerdict struct {
	// OK 表示这一步观测到「已就绪」。
	OK bool
	// Actual 描述实际观察到了什么。失败时进错误信息；成功时拼进成功日志。
	Actual string
}

// readyProbe 执行一次就绪观测。它自己负责在超时内重试；返回 OK=false
// 即代表「到超时为止都没就绪」。
type readyProbe func(ctx context.Context) readyVerdict

// readySpec 描述一次就绪判定。字段刻意都是显式的：判定什么、期望什么、
// 等了多久、失败了怎么向用户交代。
type readySpec struct {
	// What 是被判定的对象（例如「Qwen3 TTS 服务」）。
	What string
	// Expect 是期望发生的可观测事实（例如「TCP 端口 8880 开始监听」）。
	Expect string
	// Timeout 是已经等待了多久；<=0 表示这不是等待型检查（即时真实调用），
	// 错误信息里就不写「已等待」。
	Timeout time.Duration
	// Probe 是实际观测动作。
	Probe readyProbe

	// LogPath 是服务日志。非空时错误信息里会附上它的**尾部**；
	// 路径不存在、文件为空或读不到时明确写「没有日志」。
	LogPath string
	// LogTailLines 是日志尾部最多取几行；<=0 按 readyLogTailLines 处理。
	LogTailLines int

	// State 说明「现在已经完成了什么」（例如「服务已登记进服务管理」）。
	State string
	// Missing 说明「还差什么」（例如「8880 端口没有监听，网站连不上它」）。
	Missing string
	// Remedy 说明用户可以怎么办（例如「按日志尾部里的报错修好后重新部署」）。
	Remedy string

	// Result 用于写安装步骤与 Warning；可为 nil（单测直接调 assertReady 时）。
	Result *InstallResult

	// Degrade 显式选择「失败只算警告」。
	//
	// 默认 false = 失败即 error，这是刻意的默认值：谎报成功比没做更糟。
	// 只有调用方明确知道「这个降级用户能接受且看得见」时才允许打开它，
	// 并且必须写 DegradeReason。
	Degrade bool
	// DegradeReason 解释这次降级为什么可接受；Degrade=true 时不能为空。
	DegradeReason string
}

const (
	// readyLogTailLines 是错误信息里默认附上的日志行数。
	readyLogTailLines = 20
	// readyLogTailBytes 是日志尾部的字节上限（防止一条超长行把错误撑爆）。
	readyLogTailBytes = 4000
)

// assertReady 执行一次就绪判定。
//
// 成功：写一行步骤日志，返回 nil。
// 失败：默认返回 error；只有 spec.Degrade 为 true 时才写 Warning 并返回 nil。
func assertReady(ctx context.Context, spec readySpec) error {
	if spec.Probe == nil {
		// 没有探针就没有判定依据。宁可当场报错，也绝不能把「没探针」
		// 当成「通过」—— 那正是这一族缺陷的形态。
		return fmt.Errorf("%s就绪判定缺少探针（面板内部错误，请反馈）", spec.What)
	}
	if spec.Degrade && strings.TrimSpace(spec.DegradeReason) == "" {
		return fmt.Errorf("%s：显式降级必须给出 DegradeReason，"+
			"否则等于静默吞掉失败（这正是本文件要根治的那类缺陷）", spec.What)
	}

	v := spec.Probe(ctx)
	if v.OK {
		actual := strings.TrimSpace(v.Actual)
		if actual == "" {
			actual = "已就绪"
		}
		if spec.Result != nil {
			spec.Result.step(ctx, spec.What+actual)
		}
		return nil
	}

	msg := spec.failureMessage(v)
	if spec.Degrade {
		// 唯一允许「没就绪但不算失败」的路径：调用方写清了为什么可接受，
		// 并且把这条理由原样展示给用户（而不是藏在代码注释里）。
		msg += "（这是**可接受的降级**：" + strings.TrimSpace(spec.DegradeReason) + "）"
		if spec.Result != nil {
			spec.Result.Warning = msg
			spec.Result.step(ctx, "警告："+msg)
		}
		return nil
	}
	if spec.Result != nil {
		spec.Result.Warning = msg
		spec.Result.step(ctx, "错误："+msg)
	}
	return errors.New(msg)
}

// failureMessage 拼出失败的完整交代：期望 / 实际 / 等待时长 / 已完成 / 还差 /
// 怎么办 / 日志尾部。
func (s readySpec) failureMessage(v readyVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s未就绪：期望%s", s.What, s.Expect)
	if actual := strings.TrimSpace(v.Actual); actual != "" {
		fmt.Fprintf(&b, "，但实际%s", actual)
	} else {
		b.WriteString("，但实际没有任何可观测的就绪迹象")
	}
	if s.Timeout > 0 {
		fmt.Fprintf(&b, "（已等待 %s）", s.Timeout)
	}
	b.WriteString("。")
	if state := strings.TrimSpace(s.State); state != "" {
		b.WriteString(state + "；")
	}
	if missing := strings.TrimSpace(s.Missing); missing != "" {
		b.WriteString(missing + "。")
	}
	if remedy := strings.TrimSpace(s.Remedy); remedy != "" {
		b.WriteString("可以：" + remedy + "。")
	}
	b.WriteString(s.logTailClause())
	return b.String()
}

// logTailClause 生成日志尾部那一段。读不到日志时**如实写「没有日志」**，
// 不编造、也不用一句含糊的「请查看日志」糊过去。
func (s readySpec) logTailClause() string {
	path := strings.TrimSpace(s.LogPath)
	if tail, ok := readReadyLogTail(path, s.logTailLines()); ok {
		return fmt.Sprintf("服务日志 %s 尾部：\n%s", path, tail)
	}
	if path == "" {
		return "没有日志：调用方没有提供日志路径。"
	}
	return fmt.Sprintf("服务日志 %s：没有日志（文件不存在或为空）。"+
		"「没有日志」本身是信息：进程可能还没开始写日志就退出了，"+
		"请查配置/目录/权限，而不是在业务逻辑里找。", path)
}

func (s readySpec) logTailLines() int {
	if s.LogTailLines > 0 {
		return s.LogTailLines
	}
	return readyLogTailLines
}

// readReadyLogTail 读日志文件的尾部若干行。第二个返回值 false 表示
// 「没有日志」（路径为空 / 读不到 / 内容为空）—— 这是结果，不是错误。
func readReadyLogTail(path string, lines int) (string, bool) {
	if path == "" {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	tail := strings.TrimSpace(readyTailLines(string(b), lines))
	if tail == "" {
		return "", false
	}
	return tailText(tail, readyLogTailBytes), true
}

// readyTailLines 取文本的尾部若干行（保留原有顺序、用换行分隔）。
//
// 刻意不复用 homebrew_clt_mirror.go 里的 lastLines：那个把多行拼成一行
// （用 " / " 分隔）塞进任务步骤，语义不同 —— 这里的日志尾部要原样多行展示。
func readyTailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" || n <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// waitLaunchdRunning 在超时内轮询「launchd 是否真的把这个作业拉起来了」。
//
// 给**不监听任何端口**的条目用（orbien 客户端是纯出站连接，没有端口可等）。
// 对这类应用 launchd 里的进程状态才是正确判据：原来的 waitPort(0, 60s)
// 必然超时，每次安装都会误报一次「0 端口未监听」。
func waitLaunchdRunning(ctx context.Context, label string, timeout time.Duration) readyVerdict {
	deadline := time.Now().Add(timeout)
	for {
		ok, detail := readyLaunchRunning(label)
		if ok {
			return readyVerdict{OK: true, Actual: "已就绪，" + detail}
		}
		if !time.Now().Before(deadline) {
			return readyVerdict{Actual: detail}
		}
		select {
		case <-ctx.Done():
			return readyVerdict{Actual: detail + "（等待被取消）"}
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
//
//	仅测试用的注入点
//
//	真实探针要轮询 20~90 秒，单测既不可能真等、也不该去开真实端口或碰真实
//	launchd（AGENTS.md 第三节：单测不许碰真实服务）。所以把三处调用点用到的
//	底层动作做成包级变量，测试替换后即刻返回，并在测试结束后恢复。
//
//	为什么用包级变量而不是 Manager 字段：这三个调用点分属三个文件，而 Manager
//	的定义在 services.go —— 本轮不允许改它（避免和并发的改动互相破坏）。
//	生产路径永远用这里的默认值。
//
// ---------------------------------------------------------------------------
var (
	// readyWaitPort 默认就是 waitPort（轮询端口直到超时）。
	readyWaitPort = waitPort
	// readyWaitJSONBool 默认就是 waitJSONBoolValue（轮询 JSON 字段直到超时）。
	readyWaitJSONBool = waitJSONBoolValue
	// readyLaunchRunning 判断一个 launchd 作业此刻是否真的在跑。
	readyLaunchRunning = func(label string) (bool, string) {
		st, err := priv.LaunchStatus(label)
		if err != nil {
			return false, fmt.Sprintf("读取 launchd 状态失败：%v", err)
		}
		if st.Running {
			return true, fmt.Sprintf("launchd 已拉起该作业（pid %d）", st.PID)
		}
		if st.Loaded {
			return false, "launchd 已加载该作业，但进程没有在运行"
		}
		return false, "launchd 里没有这个作业"
	}
	// readyVerifyReceiverKey 默认就是 (*Manager).verifyReceiverKey。
	readyVerifyReceiverKey = func(ctx context.Context, m *Manager, key string) (bool, string) {
		return m.verifyReceiverKey(ctx, key)
	}
)

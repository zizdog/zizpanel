package services

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  安装进度管道
//
//  这一段解决的是：安装只能"请等待"。所有长命令（brew install / docker compose
//  up / pip install / hf download）过去都用 CombinedOutput()，输出被攒到进程
//  结束才一次性返回，用户在整个过程中看不到任何真实进展。
//
//  现在把输出**逐行**推给任务中心（见 SPEC-任务中心.md）：命令本身记一行，
//  stdout/stderr 边跑边发。brew 的下载百分比、docker 的层进度、pip 的收集/安装
//  都因此可见，而且这对原生与 Docker 两种安装方式是同一套代码。
// ============================================================================

// ProgressFunc 接收一行进度。与 tasks.LogFunc 是同一个类型（别名），
// 所以 web 层可以直接把 task.Log 传进来，不需要适配函数。
type ProgressFunc = tasks.LogFunc

type progressKey struct{}

// WithProgress 把一个进度接收器挂到 ctx 上。
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

// ProgressFrom 取出 ctx 上的进度接收器；没有则返回 nil。
func ProgressFrom(ctx context.Context) ProgressFunc {
	if ctx == nil {
		return nil
	}
	if fn, ok := ctx.Value(progressKey{}).(ProgressFunc); ok {
		return fn
	}
	return nil
}

// emit 给 ctx 上的接收器发一行（没有接收器时什么都不做）。
func emit(ctx context.Context, level, text string) {
	if fn := ProgressFrom(ctx); fn != nil && text != "" {
		fn(level, text)
	}
}

// step 记录一个安装步骤。
//
// 既写进 InstallResult.Steps（保持旧接口、审计内容、单测期望完全不变），
// 也实时推给任务中心 —— 这样"步骤"与"命令输出"在同一个时间线上。
func (r *InstallResult) step(ctx context.Context, msgs ...string) {
	// 变参：有些地方一次 append 多条（append(res.Steps, "a", "b")）
	r.Steps = append(r.Steps, msgs...)
	for _, msg := range msgs {
		emit(ctx, tasks.LevelStep, msg)
	}
}

// streamCmd 执行已准备好的命令，把 stdout / stderr 逐行推给进度接收器。
//
// 返回值与 CombinedOutput 的语义保持一致：累计文本 + 退出错误。
// 调用方原有的"失败时把输出摘要塞进 error"逻辑因此不用改。
//
// 为什么要两个 goroutine 分别读：如果只读 stdout，stderr 的管道缓冲区（64KB）
// 写满后子进程会**阻塞在写 stderr 上**，表现为"安装卡死"。
func streamCmd(ctx context.Context, cmd *exec.Cmd) (string, error) {
	// 命令标签**从 cmd.Args 派生**，不接受调用方传字符串。
	// 理由：手写的标签很容易和实际执行的不一致 —— 真踩过 `runColima` 把
	// `sudo -n -u <user>` 漏掉，日志里显示成没降权的直接调用，
	// 用户照着复制是另一回事。而 cmd.Args 永远是真正要执行的东西。
	emit(ctx, tasks.LevelCmd, "$ "+strings.Join(cmd.Args, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var (
		mu  sync.Mutex
		buf bytes.Buffer
		wg  sync.WaitGroup
	)
	collect := func(text string) {
		mu.Lock()
		defer mu.Unlock()
		// 只留尾部：错误信息只需要最后一段，而一次性输出可能很大。
		if buf.Len() < maxCollectedOutput {
			buf.WriteString(text)
			buf.WriteByte('\n')
		}
	}
	scan := func(r io.Reader, level string) {
		defer wg.Done()
		scanLines(r, func(line string) {
			emit(ctx, level, line)
			collect(line)
		})
	}
	wg.Add(2)
	go scan(stdout, tasks.LevelOut)
	go scan(stderr, tasks.LevelErr)

	// ctx 取消/超时时**主动关掉管道**，保证读取 goroutine 一定能看到 EOF。
	//
	// 为什么必须这么做（2026-09-18 实测）：exec.CommandContext 在超时后只 kill
	// **直接子进程**；brew 是脚本，它会 fork 出 `git` 之类的孙子进程，孙子进程
	// 继承着这两个管道。父进程被 SIGKILL 之后管道写端仍然打开，读取 goroutine
	// 永远等不到 EOF → `wg.Wait()` 死等 → 整个 HTTP 请求（市场列表）挂住。
	// 表现就是"应用页一直转圈"以及单测里 `TestMarketZombieColimaPlistNotInstalled`
	// 卡到 15 分钟超时（`go test` 的 panic dump 里能看到两个 scanLines 还活着）。
	//
	// 关掉读端不会泄漏：cmd.Wait() 之后 Go 自己也会关，重复关只是返回错误。
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = stdout.Close()
			_ = stderr.Close()
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		// 超时/取消：连同管道一起收尾，并如实返回 ctx 的错误（调用方据此判"未检查"）。
		_ = stdout.Close()
		_ = stderr.Close()
		if waitErr == nil {
			waitErr = ctx.Err()
		}
	}
	wg.Wait()
	return buf.String(), waitErr
}

// maxCollectedOutput 是保留给错误信息用的输出上限。
// 超出后不再累计（但**仍然**实时推送给任务中心），避免一条疯跑的日志把内存吃光。
const maxCollectedOutput = 256 * 1024

// scanLines 把输出**边读边切**成行，每切出一行就回调一次。
//
// 注意必须是回调式而不是"返回 []string"：后者要读到 EOF 才能返回，
// 那等于把 CombinedOutput 换了个写法，流式的意义就没了。
//
// 两个容易踩的点：
//   - 进度条用 \r 而不是 \n 刷新（pip / docker）。只按 \n 切的话，
//     整条进度会攒在缓冲区里直到结束才出现 —— 等于没做流式。
//   - 单行可能非常长（压缩后的 JSON、长 URL）。bufio.Scanner 默认 64KB 就报错，
//     这里用 ReadSlice 并在 ErrBufferFull 时继续读，把它拆成多段发出去。
//
// ANSI 颜色码复用 lnmp.go 里的 stripANSI：任务日志是纯文本渲染的，
// 转义序列只会变成乱码（终端页面另有 ansi.js，不受影响）。
func scanLines(r io.Reader, fn func(string)) {
	br := bufio.NewReaderSize(r, 32*1024)
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			// ReadSlice 会把分隔符一起带回来（且一定是本行的最后一个字符），
			// 先剪掉它，否则每行日志都拖一个换行 —— 复制日志、列表里的
			// "最后一行"都会跟着一个空行。
			text := strings.TrimSuffix(string(chunk), "\n")
			for _, part := range strings.Split(text, "\r") {
				part = stripANSI(part)
				if strings.TrimSpace(part) == "" {
					continue
				}
				fn(part)
			}
		}
		if err == bufio.ErrBufferFull {
			continue // 超长行：先把这一段已读部分发出去，继续读
		}
		if err != nil {
			return
		}
	}
}

// EmitProgress 是给 web 层用的：直接往 ctx 上的进度接收器写一行。
//
// 为什么导出：一键建站这类流程编排在 web 层（它要同时用 sites/mysql/services），
// 但"进度"这套管道在 services 里。与其在 web 层再实现一遍，
// 不如把这一行的发送暴露出来。
func EmitProgress(ctx context.Context, level, text string) { emit(ctx, level, text) }

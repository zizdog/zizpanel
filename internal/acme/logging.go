package acme

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	legolog "github.com/go-acme/lego/v4/log"
)

// 本文件解决两件事：
//
//  1. 把 lego 库内部的日志接到面板的 logf 上（进度进任务中心）。
//  2. 脱敏：任何经过本包日志出口的文本，都要把已知的密钥/token 值替换掉。
//
// 为什么要在日志层做脱敏，而不是「保证代码不打印」：
// 单靠代码纪律挡不住后续改动，也挡不住第三方库（lego 及其 190+ provider）
// 把凭据带进错误文本。这里做一道兜底，即使上游失误也不会把 token 写进任务日志。
// 注意：脱敏只处理日志，不改变返回给调用方的 error 内容（见 errors.go 的 safeError）。

var (
	// logfStore 保存当前的日志出口。lego 的全局 logger 无法按 Manager 区分，
	// 所以进程内以「最近一次 Issue/Renew 的 logf」为准；本面板同时只会跑一个证书任务。
	logfStore atomic.Value // func(string)

	secretMu sync.RWMutex
	secrets  = map[string]struct{}{}

	legoLogOnce sync.Once
)

func init() {
	logfStore.Store(func(string) {})
}

// setLogf 设置当前日志出口（由 Issue/Renew 在开始处调用）。
func setLogf(f func(string)) {
	if f == nil {
		f = func(string) {}
	}
	logfStore.Store(f)
}

// currentLogf 读取当前日志出口。
func currentLogf() func(string) {
	v := logfStore.Load()
	if v == nil {
		return func(string) {}
	}
	f, ok := v.(func(string))
	if !ok || f == nil {
		return func(string) {}
	}
	return f
}

// rememberSecret 登记一个需要脱敏的值（DNS 凭据等）。
//
// 刻意不做「用完移除」：一是并发下的引用计数不值得这个复杂度，二是这些值本来
// 就已经在进程内存里（面板配置），多留一份不增加暴露面，反而能保护后续日志。
// 太短的值不登记 —— 例如 region=cn-hangzhou 这种情况，全局替换会把正常日志也打糊。
func rememberSecret(v string) {
	if len(v) < 8 {
		return
	}
	secretMu.Lock()
	secrets[v] = struct{}{}
	secretMu.Unlock()
}

// redact 把已登记的敏感值替换成 ***。
func redact(s string) string {
	secretMu.RLock()
	defer secretMu.RUnlock()
	for v := range secrets {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

// emit 是本包唯一的日志出口：格式化 → 脱敏 → logf。
// 调用方只允许传域名、CA、路径等非敏感信息，密钥/token 永远不要作为参数传入。
func (m *Manager) emit(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if len(msg) > maxLegoLogLen {
		msg = msg[:maxLegoLogLen] + "…(截断)"
	}
	m.logf(redact(msg))
}

// legoLogger 实现 lego 的 log.StdLogger，把 lego 的输出转发到 logf。
//
// Fatal* 刻意不调用 os.Exit：lego 只在它的 CLI（cmd/ 包）里用 Fatal，
// 库路径（本包用到的 certificate / challenge / providers）从不调用。
// 一旦哪天库路径调用了 Fatal，退出整个面板是更糟的结果，所以降级成普通日志。
type legoLogger struct{}

func (legoLogger) Printf(format string, args ...any) { emitLego(fmt.Sprintf(format, args...)) }
func (legoLogger) Print(args ...any)                 { emitLego(fmt.Sprint(args...)) }
func (legoLogger) Println(args ...any)               { emitLego(fmt.Sprintln(args...)) }
func (legoLogger) Fatalf(format string, args ...any) { emitLego(fmt.Sprintf(format, args...)) }
func (legoLogger) Fatal(args ...any)                 { emitLego(fmt.Sprint(args...)) }
func (legoLogger) Fatalln(args ...any)               { emitLego(fmt.Sprintln(args...)) }

// emitLego 输出 lego 的一行日志。
func emitLego(msg string) {
	msg = strings.TrimRight(msg, "\n")
	if msg == "" {
		return
	}
	if len(msg) > maxLegoLogLen {
		msg = msg[:maxLegoLogLen] + "…(截断)"
	}
	currentLogf()(redact("[lego] " + msg))
}

// installLegoLogger 把 lego 的全局日志出口替换成 legoLogger（只做一次）。
func installLegoLogger() {
	legoLogOnce.Do(func() {
		legolog.Logger = legoLogger{}
	})
}

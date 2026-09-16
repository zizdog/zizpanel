package services

import (
	"context"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  任务内"限时询问用户"
//
//  为什么需要这一层：装 MySQL 时必须拿到 root 口令，而"装完再对齐"注定会
//  留下不一致的窗口（2026-09-16 mini：面板把 root 改成了随机口令却没记住，
//  从此每次库操作都是 1045）。正确做法是**装的时候就把它问清楚**：
//  给用户一段时间输入，超时自动生成并继续。
//
//  通道复用任务中心（tasks.Task.WaitInput），不另造一套进度/交互机制；
//  services 只依赖下面这个很窄的接口，单测里塞一个假实现即可
//  （项目铁律：单测不许连真实 MySQL、不许等真实时间）。
// ============================================================================

// InputProvider 是"向用户要一次输入"的能力。
//
// *tasks.Task 的方法签名与它逐字一致，所以 web 层不需要写适配器，
// 直接把任务对象挂到 ctx 上即可。
type InputProvider interface {
	// WaitInput 请求一次输入，最多等 timeout。
	// 返回 (值, 是否由用户提供)：provided=false 表示超时或被中断，
	// 调用方必须自己给出默认值继续（绝不能卡住）。
	WaitInput(ctx context.Context, req tasks.InputRequest, timeout time.Duration) (string, bool)
}

type inputKey struct{}

// WithInput 把输入通道挂到 ctx 上（nil 时原样返回）。
func WithInput(ctx context.Context, p InputProvider) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, inputKey{}, p)
}

// inputFrom 取出 ctx 上的输入通道；没有则返回 nil。
func inputFrom(ctx context.Context) InputProvider {
	if ctx == nil {
		return nil
	}
	if p, ok := ctx.Value(inputKey{}).(InputProvider); ok {
		return p
	}
	return nil
}

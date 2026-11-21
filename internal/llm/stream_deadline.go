package llm

import (
	"context"
	"errors"
	"time"
)

// errNoFirstToken 表示在期限内没等到模型开始回答。它不包着 context.Canceled：
// 退路（FallbackChatModel）把 context.Canceled 当成「主人打断了」而不切换。
var errNoFirstToken = errors.New("没等到模型开始回答")

// streamDeadline 是一次流式请求的两个期限：整次请求最多 total（0 = 只受调用方的 context 管）；
// 等第一个流式事件最多 firstToken（0 = 不限；收到第一个事件后就不再计这一项）。
type streamDeadline struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	release  context.CancelFunc
	watchdog *time.Timer
}

// newStreamDeadline 给 ctx 加上两个期限；请求结束时调用 close。
func newStreamDeadline(ctx context.Context, total, firstToken time.Duration) *streamDeadline {
	release := func() {}
	if total > 0 {
		ctx, release = context.WithTimeout(ctx, total)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	deadline := &streamDeadline{ctx: ctx, cancel: cancel, release: release}
	if firstToken > 0 {
		deadline.watchdog = time.AfterFunc(firstToken, func() { cancel(errNoFirstToken) })
	}
	return deadline
}

// started 在收到第一个流式事件时调用：开始回答了，后面不再限「等第一个事件」。
func (d *streamDeadline) started() {
	if d.watchdog != nil {
		d.watchdog.Stop()
		d.watchdog = nil
	}
}

// noFirstToken 是不是因为等不到第一个事件而取消的。
func (d *streamDeadline) noFirstToken() bool { return errors.Is(context.Cause(d.ctx), errNoFirstToken) }

func (d *streamDeadline) close() {
	d.started()
	d.cancel(nil)
	d.release()
}

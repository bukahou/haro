package llm

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go/circuitbreaker"

	"github.com/bukahou/haro/internal/brain"
)

// UnavailabilityBreaker 在模型服务连不上时熔断（failsafe-go 的熔断器）：
//   - 连不上一次（brain.ErrModelUnavailable）就打开：之后的请求不去连，马上交出 ErrModelUnavailable ——
//     主人马上听到「连不上」，不用每句话都等一遍超时；
//   - 过 retryDelay 之后放一个请求去试（半开），通了就恢复，没通再打开；
//   - 恢复后，下一轮的情况说明里告诉模型刚才断过多久（Brief），由它自然地交代一句。
//
// 别的错误（请求有问题、拒答、被截断、主人打断）都算服务通着：半开时拿到的试探名额也要按结果归还。
type UnavailabilityBreaker struct {
	inner   brain.ChatModel
	breaker circuitbreaker.CircuitBreaker[any]
	logger  *slog.Logger
	now     func() time.Time

	mu        sync.Mutex
	openedAt  time.Time     // 这次断开从什么时候起；零值 = 现在通着
	recovered time.Duration // 刚恢复、还没告诉模型的那次断了多久；0 = 没有
}

var (
	_ brain.ChatModel   = (*UnavailabilityBreaker)(nil)
	_ brain.Briefer     = (*UnavailabilityBreaker)(nil)
	_ brain.ModelWarmer = (*UnavailabilityBreaker)(nil)
)

// NewUnavailabilityBreaker 给 inner 加上连不上时的熔断；retryDelay 是打开之后多久再试一次。
func NewUnavailabilityBreaker(inner brain.ChatModel, retryDelay time.Duration, logger *slog.Logger) *UnavailabilityBreaker {
	b := &UnavailabilityBreaker{inner: inner, logger: logger, now: time.Now}
	b.breaker = circuitbreaker.NewBuilder[any]().
		WithFailureThreshold(1).
		WithSuccessThreshold(1).
		WithDelay(retryDelay).
		OnStateChanged(b.stateChanged).
		Build()
	return b
}

func (b *UnavailabilityBreaker) stateChanged(event circuitbreaker.StateChangedEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case event.NewState == circuitbreaker.OpenState && b.openedAt.IsZero():
		b.openedAt = b.now()
		b.logger.Warn("连不上模型服务：先不再去连，过一会儿再试", "retry_in", b.breaker.RemainingDelay().Round(time.Second))
	case event.NewState == circuitbreaker.ClosedState && !b.openedAt.IsZero():
		b.recovered = max(b.now().Sub(b.openedAt), time.Second)
		b.openedAt = time.Time{}
		b.logger.Info("模型服务恢复了", "down_for", b.recovered.Round(time.Second))
	}
}

// StreamReply 见类型说明。
func (b *UnavailabilityBreaker) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		if !b.breaker.TryAcquirePermit() {
			yield(brain.ReplyDelta{}, fmt.Errorf("%w：刚才连不上，%v 后再试", brain.ErrModelUnavailable,
				b.breaker.RemainingDelay().Round(time.Second)))
			return
		}
		var failure error
		defer func() {
			if errors.Is(failure, brain.ErrModelUnavailable) {
				b.breaker.RecordFailure()
			} else {
				b.breaker.RecordSuccess()
			}
		}()
		for delta, err := range b.inner.StreamReply(ctx, request) {
			if err != nil {
				failure = err
			}
			if !yield(delta, err) || err != nil {
				return
			}
		}
	}
}

// Brief 实现 brain.Briefer：刚恢复时告诉模型断过多久（这一轮说完、主人听完才算告诉了）。
func (b *UnavailabilityBreaker) Brief(string) brain.Briefing {
	b.mu.Lock()
	down := b.recovered
	b.mu.Unlock()
	if down == 0 {
		return brain.Briefing{}
	}
	span := "不到一分钟"
	if down >= time.Minute {
		span = fmt.Sprintf("大约 %.0f 分钟", down.Minutes())
	}
	return brain.Briefing{Lines: []string{fmt.Sprintf("刚才连不上云端的大模型（%s），刚刚恢复：这一轮用一句话顺带告诉主人刚才断过。", span)},
		Delivered: func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.recovered == down {
				b.recovered = 0
			}
		}}
}

// Warmup 转发给里面的模型（它需要预热的话）。
func (b *UnavailabilityBreaker) Warmup(ctx context.Context) error { return brain.WarmUp(ctx, b.inner) }

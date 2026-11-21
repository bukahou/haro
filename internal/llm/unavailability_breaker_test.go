package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

var quietBreakerLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// 连不上一次就打开：之后不再去连、马上交出「连不上」；过了时间放一个请求去试，通了就恢复，
// 恢复后下一轮的情况说明里说一次断过多久（主人听完才算说过）。
func TestUnavailabilityBreakerOpensAndRecovers(t *testing.T) {
	down := fmt.Errorf("%w: 请求 Claude: connection refused", brain.ErrModelUnavailable)
	inner := &scriptedModel{err: down}
	breaker := NewUnavailabilityBreaker(80*time.Millisecond, quietBreakerLogger)
	model := breaker.Guard(inner)
	request := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}}

	if _, _, _, err := collect(t, model.StreamReply(context.Background(), request)); !errors.Is(err, brain.ErrModelUnavailable) || inner.calls != 1 {
		t.Fatalf("第一次照常去连、报连不上：%v（连了 %d 次）", err, inner.calls)
	}
	if _, _, _, err := collect(t, model.StreamReply(context.Background(), request)); !errors.Is(err, brain.ErrModelUnavailable) || inner.calls != 1 {
		t.Fatalf("打开之后不再去连、马上报连不上：%v（连了 %d 次）", err, inner.calls)
	}
	if lines := breaker.Brief("").Lines; len(lines) != 0 {
		t.Errorf("还没恢复时不该说：%v", lines)
	}

	time.Sleep(120 * time.Millisecond) // 过了时间：放一个请求去试
	inner.err, inner.deltas = nil, []brain.ReplyDelta{{Text: "好了"}}
	if text, _, _, err := collect(t, model.StreamReply(context.Background(), request)); err != nil || text != "好了" || inner.calls != 2 {
		t.Fatalf("试探通了就恢复：%q %v（连了 %d 次）", text, err, inner.calls)
	}
	briefing := breaker.Brief("")
	if len(briefing.Lines) != 1 || !strings.Contains(briefing.Lines[0], "刚刚恢复") || briefing.Delivered == nil {
		t.Fatalf("恢复后应告诉模型断过：%+v", briefing)
	}
	if lines := breaker.Brief("").Lines; len(lines) != 1 {
		t.Errorf("主人听完之前下一轮照样附上：%v", lines)
	}
	briefing.Delivered()
	if lines := breaker.Brief("").Lines; len(lines) != 0 {
		t.Errorf("说过之后不再附上：%v", lines)
	}
}

// 别的错误（请求有问题、主人打断）都算服务通着：不打开。
func TestUnavailabilityBreakerIgnoresOtherErrors(t *testing.T) {
	inner := &scriptedModel{err: context.Canceled}
	model := NewUnavailabilityBreaker(time.Minute, quietBreakerLogger).Guard(inner)
	request := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}}
	for range 3 {
		collect(t, model.StreamReply(context.Background(), request))
	}
	inner.err = errors.New("请求 Claude: 400 Bad Request")
	collect(t, model.StreamReply(context.Background(), request))
	if inner.calls != 4 {
		t.Errorf("打断、请求有问题都不该打开熔断：连了 %d 次，应为 4", inner.calls)
	}
}

// 同一个熔断器套着的几个模型（前台的、接手的）一起开关：一个连不上，另一个也不再去连。
func TestUnavailabilityBreakerIsSharedByGuardedModels(t *testing.T) {
	down := fmt.Errorf("%w: 请求 Claude: connection refused", brain.ErrModelUnavailable)
	front, strong := &scriptedModel{err: down}, &scriptedModel{deltas: []brain.ReplyDelta{{Text: "好"}}}
	breaker := NewUnavailabilityBreaker(time.Minute, quietBreakerLogger)
	request := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}}
	collect(t, breaker.Guard(front).StreamReply(context.Background(), request))
	if _, _, _, err := collect(t, breaker.Guard(strong).StreamReply(context.Background(), request)); !errors.Is(err, brain.ErrModelUnavailable) || strong.calls != 0 {
		t.Errorf("前台连不上之后，接手的也应马上报连不上、不去连：%v（连了 %d 次）", err, strong.calls)
	}
}

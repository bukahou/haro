package spending

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/money"
)

// chargeLog 是只记在内存里的账本，看记了几笔、每笔是什么。
type chargeLog struct {
	mu      sync.Mutex
	charges []Charge
}

func (l *chargeLog) Record(_ context.Context, charge Charge) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.charges = append(l.charges, charge)
	return nil
}
func (l *chargeLog) Total(context.Context, time.Time, ...Purpose) (Totals, error) {
	return Totals{}, nil
}
func (l *chargeLog) FlushPending(context.Context) error       { return nil }
func (l *chargeLog) CheckWritable(context.Context) error      { return nil }
func (l *chargeLog) RaiseCap(context.Context, CapRaise) error { return nil }
func (l *chargeLog) CapRaised(context.Context, time.Time) (money.Sum, error) {
	return money.Sum{}, nil
}
func (l *chargeLog) Close() error { return nil }

func (l *chargeLog) recorded() []Charge {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Charge(nil), l.charges...)
}

// streamScript 是假的模型：按顺序吐出这些段；untilCancelled 为 true 时吐完先等到被取消，再交出 cancelled 那一段（像 Claude 被打断时那样）。
type streamScript struct {
	deltas         []brain.ReplyDelta
	err            error
	untilCancelled bool
	cancelled      brain.ReplyDelta
}

func (s streamScript) StreamReply(ctx context.Context, _ brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, delta := range s.deltas {
			if !yield(delta, nil) {
				return
			}
		}
		if s.untilCancelled {
			<-ctx.Done()
			yield(s.cancelled, ctx.Err())
			return
		}
		if s.err != nil {
			yield(brain.ReplyDelta{}, s.err)
		}
	}
}

var testPricing = Pricing{Models: map[string]TokenPrice{"m": {InputUSDPerMillion: 1, OutputUSDPerMillion: 5}}}

func metered(inner brain.ChatModel, ledger Ledger) *MeteredChatModel {
	return NewMeteredChatModel(inner, "m", PurposeChat, testPricing, ledger, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 每次请求都记一笔：正常结束记完整用量；出错时记知道的部分（标不完整）；一点用量都没有也记一行（token 不知道）。
func TestMeteredRecordsEveryRequest(t *testing.T) {
	partial := &brain.ReplyUsage{PromptTokens: 1000, Incomplete: true}
	for _, c := range []struct {
		name       string
		inner      streamScript
		want       Charge
		consumeAll bool
	}{
		{"正常结束", streamScript{deltas: []brain.ReplyDelta{{Text: "好"}, {Usage: &brain.ReplyUsage{PromptTokens: 1000, OutputTokens: 10}}}},
			Charge{InputTokens: 1000, OutputTokens: 10}, true},
		{"说到一半出错，内层没给用量", streamScript{deltas: []brain.ReplyDelta{{Text: "好"}}, err: errors.New("断了")},
			Charge{TokensUnknown: true, Incomplete: true}, true},
		{"出错，什么用量都没有", streamScript{err: errors.New("连不上")}, Charge{TokensUnknown: true, Incomplete: true}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ledger := &chargeLog{}
			for range metered(c.inner, ledger).StreamReply(context.Background(), brain.ChatRequest{}) {
			}
			got := ledger.recorded()
			if len(got) != 1 || got[0].InputTokens != c.want.InputTokens || got[0].OutputTokens != c.want.OutputTokens ||
				got[0].Incomplete != c.want.Incomplete || got[0].TokensUnknown != c.want.TokensUnknown {
				t.Errorf("应记一笔 %+v：%+v", c.want, got)
			}
		})
	}
	// 出错时内层交出了知道的输入：照实记、标不完整
	ledger := &chargeLog{}
	inner := streamScript{deltas: []brain.ReplyDelta{{Text: "好"}, {Usage: partial}}, err: errors.New("断了")}
	for range metered(inner, ledger).StreamReply(context.Background(), brain.ChatRequest{}) {
	}
	if got := ledger.recorded(); len(got) != 1 || got[0].InputTokens != 1000 || !got[0].Incomplete || got[0].TokensUnknown {
		t.Errorf("出错时应记知道的输入：%+v", got)
	}
}

// 回答被打断：调用方取消后中途不要了，接着把内层收完，记下内层交出的那部分用量（以前这次请求不进账本）。
func TestMeteredRecordsInterruptedRequest(t *testing.T) {
	ledger := &chargeLog{}
	inner := streamScript{deltas: []brain.ReplyDelta{{Text: "从前"}}, untilCancelled: true,
		cancelled: brain.ReplyDelta{Usage: &brain.ReplyUsage{PromptTokens: 2000, Incomplete: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range metered(inner, ledger).StreamReply(ctx, brain.ChatRequest{}) {
		cancel() // 主人打断：取消后不再要后面的
		break
	}
	if got := ledger.recorded(); len(got) != 1 || got[0].InputTokens != 2000 || !got[0].Incomplete {
		t.Errorf("被打断的请求应记下知道的输入、标不完整：%+v", got)
	}
}

// 记账不跟着这一轮取消（主人打断了，已经花的照样要记），但有自己的期限（账本卡住时回答的收尾不一直等）。
func TestMeteredRecordsWithItsOwnDeadline(t *testing.T) {
	ledger := &contextLog{}
	inner := streamScript{deltas: []brain.ReplyDelta{{Text: "从前"}}, untilCancelled: true,
		cancelled: brain.ReplyDelta{Usage: &brain.ReplyUsage{PromptTokens: 2000, Incomplete: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range metered(inner, ledger).StreamReply(ctx, brain.ChatRequest{}) {
		cancel() // 主人打断
		break
	}
	if len(ledger.recorded()) != 1 || ledger.err != nil {
		t.Errorf("回答被取消后应照样记一笔、记账用的 ctx 不跟着取消：%d 笔，%v", len(ledger.recorded()), ledger.err)
	}
	if !ledger.hasDeadline || ledger.remaining <= 0 || ledger.remaining > recordTimeout {
		t.Errorf("记账要有期限（不超过 %v）：有没有 %v，还剩 %v", recordTimeout, ledger.hasDeadline, ledger.remaining)
	}
}

// contextLog 记下账本收到的 ctx 是什么样（取消了没有、期限还剩多久）。
type contextLog struct {
	chargeLog
	err         error
	hasDeadline bool
	remaining   time.Duration
}

func (l *contextLog) Record(ctx context.Context, charge Charge) error {
	l.err = ctx.Err()
	deadline, ok := ctx.Deadline()
	l.hasDeadline, l.remaining = ok, time.Until(deadline)
	return l.chargeLog.Record(ctx, charge)
}

// 没取消却中途不要了：不等内层（它还会接着生成），记一行 token 不知道的。
func TestMeteredRecordsAbandonedRequestAsUnknown(t *testing.T) {
	ledger := &chargeLog{}
	inner := streamScript{deltas: []brain.ReplyDelta{{Text: "从前"}, {Text: "有座山"}, {Usage: &brain.ReplyUsage{PromptTokens: 2000, OutputTokens: 5}}}}
	for range metered(inner, ledger).StreamReply(context.Background(), brain.ChatRequest{}) {
		break
	}
	if got := ledger.recorded(); len(got) != 1 || !got[0].TokensUnknown {
		t.Errorf("拿不到用量也应记一行：%+v", got)
	}
}

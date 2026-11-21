package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/telemetry"
)

var (
	tracer = otel.Tracer("github.com/bukahou/haro/assistant")
	// timeToFirstAudio 是核心指标：用户说完（commit）→ 第一段回答音频写入播放器。
	timeToFirstAudio = telemetry.MustLatencyHistogram(otel.Meter("github.com/bukahou/haro/assistant"),
		"haro.turn.time_to_first_audio", "用户说完到第一段回答音频写入播放器的时间")
)

// Components 是终端需要的全部能力，由 cmd/haro 组装注入。
type Components struct {
	Manual     ManualToggle // 回车手动唤醒 / 休眠
	Microphone Microphone
	Speaker    AudioSink
	Brain      BrainSession
}

// Options 是终端行为的可调参数。
type Options struct {
	TurnDetection realtimeproto.TurnDetection // 服务端怎么判断说完
	EchoGuard     time.Duration               // 不能打断时：播完后再静音这么久才恢复上传，防止把自己的回音当成用户说话
	WakeWord      WakeWordOptions             // 叫唤醒词才听，对话窗口结束自动休眠
	// BargeIn 为 true 时可以用唤醒词打断回答：播放时麦克风照常上传，语音前端只听唤醒词；听到就停播、取消回答、转为听用户说。
	BargeIn bool
	// NoticeTone 是服务端发来提示（haro.notice，例如后台调查查完了）时播的一声；空 = 只写日志
	NoticeTone realtimeproto.PCMAudio
	// NoticeTones 是有专门提示音的提示种类（例如语音识别断开 / 恢复，和服务端断开 / 回来用同样的两声）
	NoticeTones map[string]realtimeproto.PCMAudio
	// Timing 是固定的几个时长；零值的字段用 DefaultTiming 的
	Timing Timing
}

// WakeWordOptions 是唤醒词模式的参数。
type WakeWordOptions struct {
	Phrase      string                 // 唤醒词（只用于提示）
	IdleTimeout time.Duration          // 对话窗口：最后一次回答播完后安静这么久就休眠
	SleepTone   realtimeproto.PCMAudio // 休眠时播一声（「我去睡觉了」），让人知道要重新叫唤醒词；空 = 不播
}

// Assistant 是 haro 的对外入口：反复执行「听 → 看 → 交给服务端 → 说」。
type Assistant struct {
	parts  Components
	opts   Options
	logger *slog.Logger
	uplink micUplink                      // 上传音频的时间轴与静音开关
	replay chan realtimeproto.ServerEvent // 打断后等取消确认期间收到的事件，留给主循环处理
	busy   atomic.Bool                    // 正在回答这一轮（请求回答 → 播完）
	// hearing：主人正在说话（服务端报了开始说话，这一轮还没答完）
	hearing atomic.Bool
	// heldNotices：主人说着话时来的提示，留到这一轮答完再响（只在主循环里读写）
	heldNotices []realtimeproto.HaroNotice
}

// eventBacklog：留给主循环之后再处理的事件最多这么多（打断后等取消确认期间收到的、留到这一轮答完再响的提示），
// 再多就丢掉并写日志。
const eventBacklog = 16

// Busy 表示主人正在说话或正在回答（从开始说话到这一轮播完）；此时换麦克风、断开重连会丢掉这句话。
func (a *Assistant) Busy() bool { return a.hearing.Load() || a.busy.Load() }

// NewAssistant 组装一个 Assistant。
func NewAssistant(parts Components, opts Options, logger *slog.Logger) *Assistant {
	opts.Timing = opts.Timing.orDefault()
	return &Assistant{parts: parts, opts: opts, logger: logger, replay: make(chan realtimeproto.ServerEvent, eventBacklog)}
}

// Run 打开服务端会话（服务端判断说完、启用唤醒门），然后按唤醒词轮流说话，直到 ctx 取消或出错。
func (a *Assistant) Run(ctx context.Context) error {
	turn := a.opts.TurnDetection
	update := realtimeproto.SessionUpdate{TurnDetection: &turn, WakeWord: true}
	if err := a.parts.Brain.Send(ctx, update); err != nil {
		return fmt.Errorf("初始化服务端会话: %w", err)
	}
	return a.runWakeWord(ctx)
}

func fail(span trace.Span, step string, err error) error {
	wrapped := fmt.Errorf("%s: %w", step, err)
	if !errors.Is(err, context.Canceled) {
		span.RecordError(wrapped)
		span.SetStatus(codes.Error, wrapped.Error())
	}
	return wrapped
}

package assistant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	Trigger    TurnTrigger        // 按键模式
	Recorder   AudioRecorder      // 按键模式
	Toggle     ConversationToggle // 免按键模式（不用唤醒词时）
	Manual     ManualToggle       // 唤醒词模式：回车手动唤醒 / 休眠
	Microphone Microphone         // 免按键模式
	Camera     FrameSource        // 可为 nil：这个终端没有摄像头（v3 H-2），不取画面，并在会话里声明
	Speaker    AudioSink
	Brain      BrainSession
}

// Options 是终端行为的可调参数。
type Options struct {
	Instructions string        // 系统提示词，启动时经 session.update 交给服务端
	MinSpeech    time.Duration // 按键模式：短于这个时长的录音视为误触，直接丢弃
	// TurnDetection 非 nil 时为免按键模式（v2）：服务端侧 VAD 判断说完；nil 为按键模式（v1.5）。
	TurnDetection *realtimeproto.TurnDetection
	EchoGuard     time.Duration // 免按键模式：播完后再静音这么久才恢复上传，防止把自己的回音当成用户说话
	// WakeWord 非 nil 时启用唤醒词（v2.2，需要 TurnDetection）：叫唤醒词才听，对话窗口结束自动休眠。
	WakeWord *WakeWordOptions
	// BargeIn 为 true 时可以用唤醒词打断回答（v3 I-1，需要 WakeWord）：播放时麦克风照常上传，
	// 语音前端只听唤醒词；听到就停播、取消回答、转为听用户说。
	BargeIn bool
	// NoticeTone 是服务端发来提示（haro.notice，例如后台调查查完了）时播的一声；空 = 只写日志
	NoticeTone realtimeproto.PCMAudio
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
	uplink micUplink                      // 免按键模式：上传音频的时间轴与静音开关
	replay chan realtimeproto.ServerEvent // 打断后等取消确认期间收到的事件，留给主循环处理
	busy   atomic.Bool                    // 正在回答这一轮（请求回答 → 播完）：看守换设备要等它结束（v4 D3）
}

// Busy 表示正在回答（从请求回答到播完）；此时不该换麦克风、断开重连。
func (a *Assistant) Busy() bool { return a.busy.Load() }

// NewAssistant 组装一个 Assistant。
func NewAssistant(parts Components, opts Options, logger *slog.Logger) *Assistant {
	return &Assistant{parts: parts, opts: opts, logger: logger, replay: make(chan realtimeproto.ServerEvent, 16)}
}

// Run 循环处理对话，直到 ctx 取消。单轮出错只记录日志，不退出。
func (a *Assistant) Run(ctx context.Context) error {
	update := realtimeproto.SessionUpdate{Instructions: a.opts.Instructions, TurnDetection: a.opts.TurnDetection,
		WakeWord: a.opts.TurnDetection != nil && a.opts.WakeWord != nil, NoCamera: a.parts.Camera == nil}
	if err := a.parts.Brain.Send(ctx, update); err != nil {
		return fmt.Errorf("初始化服务端会话: %w", err)
	}
	switch {
	case update.WakeWord:
		return a.runWakeWord(ctx)
	case a.opts.TurnDetection != nil:
		return a.runHandsFree(ctx)
	}
	return a.runPushToTalk(ctx)
}

// runPushToTalk 是 v1.5 的按键模式：每轮按一次开始、再按一次结束。
func (a *Assistant) runPushToTalk(ctx context.Context) error {
	for {
		err := a.HandleTurn(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, io.EOF) { // 触发器的输入关闭了（例如终端被关），正常退出
			return err
		}
		if err != nil {
			a.logger.Error("本轮对话失败", "err", err)
		}
	}
}

// HandleTurn 处理按键模式的一轮对话。
func (a *Assistant) HandleTurn(ctx context.Context) error {
	if err := a.parts.Trigger.WaitForStart(ctx); err != nil {
		return err
	}
	a.busy.Store(true)
	defer a.busy.Store(false)
	ctx, span := tracer.Start(ctx, "haro.turn")
	defer span.End()

	audio, err := a.record(ctx)
	if err != nil {
		return fail(span, "录音", err)
	}
	committedAt := time.Now()
	span.SetAttributes(attribute.Float64("haro.speech_seconds", audio.Duration().Seconds()))
	if audio.Duration() < a.opts.MinSpeech {
		a.logger.Info("录音太短，忽略", "duration", audio.Duration())
		return nil
	}

	brain := a.parts.Brain
	if err := brain.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: audio}); err != nil {
		return fail(span, "提交录音", err)
	}
	if err := brain.Send(ctx, realtimeproto.InputAudioBufferCommit{}); err != nil {
		return fail(span, "提交录音", err)
	}
	// 服务端已经开始识别；取帧与识别并行（常驻摄像头不用等）。没有摄像头时跳过。
	if err := a.attachFrame(ctx, span); err != nil {
		return fail(span, "提交画面", err)
	}
	if err := brain.Send(ctx, realtimeproto.ResponseCreate{}); err != nil {
		return fail(span, "请求回答", err)
	}
	return a.playResponse(ctx, span, committedAt)
}

func (a *Assistant) record(ctx context.Context) (realtimeproto.PCMAudio, error) {
	ctx, span := tracer.Start(ctx, "capture.record")
	defer span.End()
	recording, err := a.parts.Recorder.StartRecording(ctx)
	if err != nil {
		return realtimeproto.PCMAudio{}, err
	}
	if err := a.parts.Trigger.WaitForStop(ctx); err != nil {
		_, _ = recording.Stop()
		return realtimeproto.PCMAudio{}, err
	}
	return recording.Stop()
}

func fail(span trace.Span, step string, err error) error {
	wrapped := fmt.Errorf("%s: %w", step, err)
	if !errors.Is(err, context.Canceled) {
		span.RecordError(wrapped)
		span.SetStatus(codes.Error, wrapped.Error())
	}
	return wrapped
}

// attachFrame 把「说完那一刻」的画面交给服务端。没有摄像头（v3 H-2）时什么都不做；取帧失败只记日志，本轮不带画面。
// 只有交给服务端失败时返回错误。
func (a *Assistant) attachFrame(ctx context.Context, span trace.Span) error {
	if a.parts.Camera == nil {
		return nil
	}
	frame, takenAt, err := a.parts.Camera.LatestFrame(ctx)
	if err != nil {
		a.logger.Warn("取帧失败，本轮不带画面", "err", err)
		return nil
	}
	span.SetAttributes(attribute.Int64("haro.frame_age_ms", time.Since(takenAt).Milliseconds()))
	return a.parts.Brain.Send(ctx, realtimeproto.ConversationItemCreate{Item: realtimeproto.ConversationItem{Role: "user",
		Content: []realtimeproto.ContentPart{{Type: "input_image", ImageJPEG: frame}}}})
}

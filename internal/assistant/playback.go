package assistant

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"haro/internal/realtimeproto"
)

// errInterrupted 表示这次回答被用户用唤醒词打断了（v3 I-1）。
var errInterrupted = errors.New("回答被打断")

// cancelDrainTimeout：被打断后等大脑确认取消（response.done）最多这么久。
const cancelDrainTimeout = 3 * time.Second

// playResponse 消费大脑的事件，直到回答结束、声音播完：日志记文字，音频交给后台写入。
//
// 为什么音频要后台写（v3 I-1）：播放器的管道满了写入就会阻塞，而语音合成比播放快——如果在这里直接写，
// 大部分播放时间都卡在写入和「等播完」上，「唤醒（打断）」事件要排在一堆音频后面好几秒才轮得到。
func (a *Assistant) playResponse(ctx context.Context, turnSpan trace.Span, committedAt time.Time) error {
	var (
		player   *player
		answer   strings.Builder
		done     *realtimeproto.ResponseDone
		finished <-chan error // 回答结束后：后台写完并播完时有值
	)
	defer func() {
		if player != nil { // 异常退出时停止播放
			player.abort()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			if done == nil {
				_ = a.parts.Brain.Send(context.WithoutCancel(ctx), realtimeproto.ResponseCancel{})
			}
			return ctx.Err()
		case err := <-finished:
			player.span.End()
			player = nil
			return a.finishResponse(turnSpan, *done, err, answer.String(), committedAt)
		case event := <-a.parts.Brain.Events():
			switch e := event.(type) {
			case realtimeproto.HaroNotice: // 正在说话：提示留到这一轮播完再响
				select {
				case a.replay <- e:
				default:
					a.logger.Warn("事件太多，丢掉一个", "event", e.Type())
				}
			case realtimeproto.HaroWakeWordDetected:
				if a.opts.BargeIn {
					err := a.interrupt(ctx, turnSpan, player, done != nil, answer.String())
					player = nil
					return err
				}
			case realtimeproto.InputAudioTranscriptionCompleted:
				if e.SpeakerChecked { // 声纹（v3 H-3）：日志里看得到是谁说的
					a.logger.Info("你说", "text", e.Transcript, "speaker", speakerLabel(e.Speaker))
				} else {
					a.logger.Info("你说", "text", e.Transcript)
				}
			case realtimeproto.ResponseOutputAudioTranscriptDelta:
				answer.WriteString(e.Delta)
			case realtimeproto.ResponseOutputAudioDelta:
				if player == nil {
					_, span := tracer.Start(ctx, "terminal.playback")
					stream, err := a.parts.Speaker.OpenStream(ctx, e.Audio.Format)
					if err != nil {
						span.End()
						return fail(turnSpan, "打开播放流", err)
					}
					player = startPlayer(stream, span)
					firstAudio := time.Since(committedAt)
					turnSpan.AddEvent("first_audio")
					turnSpan.SetAttributes(attribute.Float64("haro.turn.time_to_first_audio", firstAudio.Seconds()))
					timeToFirstAudio.Record(ctx, firstAudio.Seconds())
					a.logger.Info("⏱ 说完 → 第一个字", "elapsed", firstAudio.Round(time.Millisecond))
				}
				player.enqueue(e.Audio.Data)
			case realtimeproto.Error:
				a.logger.Error("大脑出错", "message", e.Message)
			case realtimeproto.ResponseDone:
				done = &e
				if player == nil { // 没有声音（失败、空识别等）
					return a.finishResponse(turnSpan, e, nil, answer.String(), committedAt)
				}
				finished = player.finish()
			}
		}
	}
}

func (a *Assistant) finishResponse(turnSpan trace.Span, done realtimeproto.ResponseDone, playErr error,
	answer string, committedAt time.Time) error {
	if answer != "" {
		a.logger.Info("haro 说", "text", answer)
	}
	if playErr != nil {
		return fail(turnSpan, "播放", playErr)
	}
	turnSpan.SetAttributes(attribute.String("haro.response.status", string(done.Status)))
	switch done.Status {
	case realtimeproto.StatusCompleted:
		a.logger.Info("本轮完成", "total", time.Since(committedAt).Round(time.Millisecond))
		return nil
	case realtimeproto.StatusFailed:
		return fail(turnSpan, "大脑", errors.New(done.Reason))
	default:
		a.logger.Info("本轮未完成", "status", done.Status, "reason", done.Reason)
		return nil
	}
}

// interrupt 处理打断（v3 I-1 / I-2）：立即停播；告诉大脑用户听到了多少（conversation.item.truncate）；
// 回答还没结束时再取消，并等它确认（期间收到的其他事件留给主循环）。
func (a *Assistant) interrupt(ctx context.Context, turnSpan trace.Span, p *player, responseDone bool, answer string) error {
	var played time.Duration
	if p != nil {
		played = p.abort()
		p.span.End()
	}
	a.logger.Info("✋ 被打断", "played", played.Round(time.Millisecond), "answer_so_far", answer)
	turnSpan.SetAttributes(attribute.String("haro.response.status", "interrupted"),
		attribute.Float64("haro.turn.played_seconds", played.Seconds()))
	brain := a.parts.Brain
	if err := brain.Send(ctx, realtimeproto.ConversationItemTruncate{AudioEndMs: played.Milliseconds()}); err != nil {
		a.logger.Warn("通知大脑截断历史失败", "err", err)
	}
	if !responseDone {
		if err := brain.Send(ctx, realtimeproto.ResponseCancel{}); err != nil {
			a.logger.Warn("取消回答失败", "err", err)
		}
		a.waitCancelled(ctx)
	}
	return errInterrupted
}

// waitCancelled 等被取消的回答结束（response.done）；被取消的回答剩下的文字和声音丢掉，
// 其他事件（打断后用户接着说的话：speech_started / stopped 等）放进 replay，留给主循环处理。
func (a *Assistant) waitCancelled(ctx context.Context) {
	timeout := time.After(cancelDrainTimeout)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			a.logger.Warn("等大脑确认取消超时")
			return
		case event := <-a.parts.Brain.Events():
			switch event.(type) {
			case realtimeproto.ResponseDone:
				return
			case realtimeproto.ResponseOutputAudioDelta, realtimeproto.ResponseOutputAudioTranscriptDelta:
			default:
				select {
				case a.replay <- event:
				default:
					a.logger.Warn("事件太多，丢掉一个", "event", event.Type())
				}
			}
		}
	}
}

// player 在后台把音频写进播放流，写完再等播完（Drain）；abort 立即停止。
type player struct {
	stream AudioStream
	span   trace.Span
	queue  chan []byte
	result chan error

	mu     sync.Mutex
	closed bool
}

// playerQueueSize：排队的音频块上限（一块约 0.25 s，足够放下一整段回答）。
const playerQueueSize = 1024

func startPlayer(stream AudioStream, span trace.Span) *player {
	p := &player{stream: stream, span: span, queue: make(chan []byte, playerQueueSize), result: make(chan error, 1)}
	go func() {
		var err error
		for data := range p.queue {
			if err == nil {
				err = p.stream.Write(data)
			}
		}
		if err == nil {
			err = p.stream.Drain()
		}
		p.result <- err
	}()
	return p
}

func (p *player) enqueue(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.queue <- data
	}
}

// finish 表示不会再有新的音频：写完、播完后，返回的通道里有结果。
func (p *player) finish() <-chan error {
	p.close()
	return p.result
}

// abort 立即停止播放，返回已经播出的时长。
func (p *player) abort() time.Duration {
	played := p.stream.Played()
	_ = p.stream.Abort()
	p.close()
	return played
}

func (p *player) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.queue)
	}
}

// speakerLabel 是日志里的说话人：没认出来时写「不认识」。
func speakerLabel(name string) string {
	if name == "" {
		return "不认识"
	}
	return name
}

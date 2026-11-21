package assistant

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// errInterrupted 表示这次回答被用户用唤醒词打断了。
var errInterrupted = errors.New("回答被打断")

// playResponse 消费服务端的事件，直到回答结束、声音播完：日志记文字，音频交给播放器。
//
// 音频在这里直接写进播放流（AudioStream.Write 只排进队列、不等播出），等播完放在后台：
// 等播完的这段时间里照常处理事件，「唤醒（打断）」不用排在播放后面。
func (a *Assistant) playResponse(ctx context.Context, turnSpan trace.Span, item string, committedAt time.Time) error {
	var (
		player   *player
		answer   strings.Builder
		done     *realtimeproto.ResponseDone
		finished <-chan error // 回答结束后：播完时有值
	)
	defer func() {
		if player != nil { // 异常退出（关机、重连）时停止播放
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
			player.end()
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
					err := a.interrupt(ctx, turnSpan, player, item, done != nil, answer.String())
					player = nil
					return err
				}
			case realtimeproto.InputAudioTranscriptionCompleted:
				if e.SpeakerChecked { // 声纹：日志里看得到是谁说的
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
					a.logger.Info("⏱ 说完 → 第一个字", "item", item, "elapsed", firstAudio.Round(time.Millisecond))
				}
				player.write(e.Audio.Data)
			case realtimeproto.Error:
				a.logger.Error("服务端出错", "message", e.Message)
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
		return fail(turnSpan, "服务端", errors.New(string(done.Reason)))
	default:
		a.logger.Info("本轮未完成", "status", done.Status, "reason", done.Reason)
		return nil
	}
}

// interrupt 处理打断：立即停播；告诉服务端用户听到了多少（conversation.item.truncate）；
// 回答还没结束时再取消，并等它确认（期间收到的其他事件留给主循环）。
func (a *Assistant) interrupt(ctx context.Context, turnSpan trace.Span, p *player, item string, responseDone bool, answer string) error {
	var played time.Duration
	if p != nil {
		played = p.abort()
	}
	a.logger.Info("✋ 被打断", "item", item, "played", played.Round(time.Millisecond), "answer_so_far", answer)
	turnSpan.SetAttributes(attribute.String("haro.response.status", "interrupted"),
		attribute.Float64("haro.turn.played_seconds", played.Seconds()))
	brain := a.parts.Brain
	if err := brain.Send(ctx, realtimeproto.ConversationItemTruncate{AudioEndMs: played.Milliseconds()}); err != nil {
		a.logger.Warn("通知服务端截断历史失败", "err", err)
	}
	if !responseDone {
		if err := brain.Send(ctx, realtimeproto.ResponseCancel{}); err != nil {
			a.logger.Warn("取消回答失败", "err", err)
		}
		a.waitCancelled(ctx)
	}
	return errInterrupted
}

// waitCancelled 等被取消的回答结束（response.done，最多等 Timing.CancelConfirm）；被取消的回答剩下的文字和声音丢掉，
// 其他事件（打断后用户接着说的话：speech_started / stopped 等）放进 replay，留给主循环处理。
func (a *Assistant) waitCancelled(ctx context.Context) {
	timeout := time.After(a.opts.Timing.CancelConfirm)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			a.logger.Warn("等服务端确认取消超时")
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

// player 是一轮回答的播放（只在 playResponse 的事件循环里使用）：音频直接写进播放流，
// 回答结束后在后台等播完；abort 立即停止。terminal.playback 的 span 随播放结束：
// 播完、被打断、异常退出时停播，都只结束一次。
type player struct {
	stream   AudioStream
	span     trace.Span
	writeErr error // 第一次写失败的原因：之后的音频不再写，结束时交出
	ended    sync.Once
}

func startPlayer(stream AudioStream, span trace.Span) *player {
	return &player{stream: stream, span: span}
}

// write 把一段音频交给播放流；写失败过就不再写（设备已经断了，后面的也写不进去）。
func (p *player) write(data []byte) {
	if p.writeErr == nil {
		p.writeErr = p.stream.Write(data)
	}
}

// finish 表示不会再有新的音频：在后台等播完，结果交到返回的通道。写失败过就不等了：停掉播放流（释放设备），交出原因。
func (p *player) finish() <-chan error {
	result := make(chan error, 1)
	if p.writeErr != nil {
		_ = p.stream.Abort()
		result <- p.writeErr
		return result
	}
	go func() { result <- p.stream.Drain() }()
	return result
}

// abort 立即停止播放并结束 span，返回已经播出的时长。
func (p *player) abort() time.Duration {
	played := p.stream.Played()
	_ = p.stream.Abort()
	p.end()
	return played
}

// end 结束 terminal.playback 的 span（只结束一次）。
func (p *player) end() { p.ended.Do(func() { p.span.End() }) }

// speakerLabel 是日志里的说话人：没认出来时写「不认识」。
func speakerLabel(name string) string {
	if name == "" {
		return "不认识"
	}
	return name
}

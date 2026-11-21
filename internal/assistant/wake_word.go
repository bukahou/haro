package assistant

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// runWakeWord 是终端的对话循环：麦克风一直开着，语音前端从休眠开始。
//   - 听到唤醒词（或按回车）→ 唤醒，进入对话窗口，自动轮流
//   - 最后一次回答播完后安静 IdleTimeout（或再按回车）→ 通知语音前端休眠
//
// 休眠时语音前端只认唤醒词，不识别、不上报任何内容（K2 ②）。
func (a *Assistant) runWakeWord(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mic, err := a.parts.Microphone.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer mic.Close()
	uploadDone := make(chan error, 1)
	go func() { uploadDone <- a.uploadMicrophone(ctx, mic) }()
	keys := make(chan error, 1)
	go func() {
		for {
			err := a.parts.Manual.WaitForToggle(ctx)
			keys <- err
			if err != nil {
				return
			}
		}
	}()

	window := &conversationWindow{idle: a.opts.WakeWord.IdleTimeout}
	defer window.stop()
	a.logger.Info("💤 休眠中：叫「" + a.opts.WakeWord.Phrase + "」唤醒（或按回车）")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-uploadDone:
			return err
		case err := <-keys:
			if errors.Is(err, io.EOF) { // 没有键盘（作为 systemd 服务运行时 stdin 是空的）：只用唤醒词，照常运行
				a.logger.Info("没有键盘输入：只能用唤醒词唤醒")
				keys = nil
				continue
			}
			if err != nil {
				return err
			}
			if window.awake {
				a.sleep(ctx, window, "手动休眠")
			} else if err := a.parts.Brain.Send(ctx, realtimeproto.HaroWakeWordWake{}); err != nil {
				a.logger.Error("手动唤醒失败", "err", err)
			} else {
				window.wake()
				a.logger.Info("👂 已唤醒（手动），直接说话就行")
			}
		case <-window.expired():
			a.sleep(ctx, window, "对话窗口结束")
		case event := <-a.replay: // 打断后等取消确认期间收到的事件
			a.handleWakeEvent(ctx, window, event)
		case event := <-a.parts.Brain.Events():
			a.handleWakeEvent(ctx, window, event)
		}
	}
}

// handleWakeEvent 处理唤醒词模式下服务端发来的一个事件。
func (a *Assistant) handleWakeEvent(ctx context.Context, window *conversationWindow, event realtimeproto.ServerEvent) {
	switch e := event.(type) {
	case realtimeproto.HaroWakeWordDetected:
		window.wake()
		a.logger.Info("👂 已唤醒", "keyword", e.Keyword)
	case realtimeproto.InputAudioBufferSpeechStarted:
		if window.awake {
			a.hearing.Store(true)
			window.hold() // 用户在说话，对话窗口不倒计时
		}
	case realtimeproto.InputAudioBufferSpeechStopped:
		if window.awake {
			window.hold()
			interrupted := a.respondHandsFree(ctx, e.ItemID, a.uplink.wallClockAt(e.AudioEndMs))
			a.hearing.Store(false) // 这一轮答完才放开：说完到开始回答之间也不算空闲
			if interrupted {
				window.wake() // 被打断：已经醒了，等用户接着说
				a.logger.Info("👂 打断了，请说")
				return
			}
			window.restart() // 从回答播完开始重新计时
		}
	case realtimeproto.HaroNotice:
		if e.Kind == realtimeproto.NoticeTranscriptionLost {
			// 服务端听不见了：正在说的这句已经丢了，不会再有「说完」——不再算在说话，对话窗口照常倒计时
			a.hearing.Store(false)
			if window.awake {
				window.restart()
			}
		}
		a.playNotice(ctx, e)
	case realtimeproto.Error:
		a.logger.Error("服务端出错", "message", e.Message)
	}
}

func (a *Assistant) sleep(ctx context.Context, window *conversationWindow, reason string) {
	if err := a.parts.Brain.Send(ctx, realtimeproto.HaroWakeWordSleep{}); err != nil {
		if !errors.Is(err, context.Canceled) {
			a.logger.Error("通知休眠失败：过一个对话窗口再试", "err", err)
		}
		window.restart() // 这次的计时已经用掉了：重新计时，否则窗口永远不再到期
		return
	}
	window.sleep()
	a.hearing.Store(false) // 说到一半休眠（例如按回车）：之后不会再有「说完」
	a.logger.Info("💤 " + reason + "，休眠中：叫「" + a.opts.WakeWord.Phrase + "」唤醒（或按回车）")
	a.playSleepTone(ctx)
}

// playNotice 服务端有事告诉主人（例如后台调查查完了、语音识别断开了）：响一声。醒着、睡着都响。
// 有专门提示音的种类（NoticeTones）播那一声，其余（包括不认识的）播通用的提示音。
func (a *Assistant) playNotice(ctx context.Context, notice realtimeproto.HaroNotice) {
	a.logger.Info("🔔 服务端提示", "kind", notice.Kind)
	tone, ok := a.opts.NoticeTones[notice.Kind]
	if !ok {
		tone = a.opts.NoticeTone
	}
	a.playClip(ctx, tone, "提示音")
}

// playSleepTone 休眠时说一声：不出声的话，人不知道它已经睡了，接着说话会没反应。
// 放在通知服务端休眠之后：这时语音前端只认唤醒词，提示音不会被当成用户说话。
func (a *Assistant) playSleepTone(ctx context.Context) {
	a.playClip(ctx, a.opts.WakeWord.SleepTone, "休眠提示音")
}

// playClip 在本机音箱上播一段提示音（播完才返回）。
func (a *Assistant) playClip(ctx context.Context, tone realtimeproto.PCMAudio, what string) {
	if len(tone.Data) == 0 || a.parts.Speaker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, tone.Duration()+5*time.Second)
	defer cancel()
	stream, err := a.parts.Speaker.OpenStream(ctx, tone.Format)
	if err != nil {
		a.logger.Warn("播"+what+"失败", "err", err)
		return
	}
	if err = stream.Write(tone.Data); err == nil {
		err = stream.Drain()
	}
	if err != nil {
		_ = stream.Abort()
		if !errors.Is(err, context.Canceled) {
			a.logger.Warn("播"+what+"失败", "err", err)
		}
	}
}

// conversationWindow 是唤醒后的对话窗口计时（只在主循环里使用，不需要加锁）。
type conversationWindow struct {
	idle  time.Duration
	awake bool
	timer *time.Timer
}

func (w *conversationWindow) wake()    { w.awake = true; w.restart() }
func (w *conversationWindow) sleep()   { w.awake = false; w.stop() }
func (w *conversationWindow) hold()    { w.stop() }
func (w *conversationWindow) restart() { w.stop(); w.timer = time.NewTimer(w.idle) }

func (w *conversationWindow) stop() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// expired 返回本次倒计时的通道；没有在计时时返回 nil（select 里永远不会触发）。
func (w *conversationWindow) expired() <-chan time.Time {
	if w.timer == nil {
		return nil
	}
	return w.timer.C
}

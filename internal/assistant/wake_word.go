package assistant

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// runWakeWord 是终端的对话循环：麦克风一直开着，语音前端从休眠开始。
//   - 听到唤醒词（或按回车）→ 唤醒，进入对话窗口，自动轮流
//   - 最后一次回答播完后安静 IdleTimeout（或再按回车）→ 请语音前端休眠
//
// 休眠时语音前端只认唤醒词，不识别、不上报任何内容（K2 ②）。醒 / 睡由语音前端决定：
// 终端只发请求，照服务端转来的 haro.wake_word.detected / haro.wake_word.state 改自己记的。
func (a *Assistant) runWakeWord(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mic, err := a.deps.Microphone.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer mic.Close()
	uploadDone := make(chan error, 1)
	go func() { uploadDone <- a.uploadMicrophone(ctx, mic) }()
	keys := make(chan error, 1)
	go func() {
		for {
			err := a.deps.Manual.WaitForToggle(ctx)
			keys <- err
			if err != nil {
				return
			}
		}
	}()

	window := &conversationWindow{idle: a.opts.WakeWord.IdleTimeout}
	defer window.close()
	a.logger.Info("💤 休眠中：" + a.wakeHint() + "（或按回车）")

	for {
		a.playHeldNotices(ctx, window)
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
				a.requestWakeState(ctx, window, false, "手动休眠")
			} else {
				a.requestWakeState(ctx, window, true, "手动")
			}
		case <-window.expired():
			a.requestWakeState(ctx, window, false, "对话窗口结束")
		case <-window.replyOverdue():
			request := window.settle()
			a.logger.Warn("语音前端没有回复这次切换：当作没切成", "awake", request.awake, "reason", request.reason,
				"waited", a.opts.Timing.WakeConfirm)
			if window.awake {
				window.restart() // 还醒着：过一个对话窗口再试休眠，不能停在「醒着且永不到期」
			}
		case event := <-a.replay: // 打断后等取消确认期间收到的事件
			a.handleWakeEvent(ctx, window, event)
		case event := <-a.deps.Brain.Events():
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
	case realtimeproto.HaroWakeWordState:
		a.applyWakeState(ctx, window, e.Awake)
	case realtimeproto.SessionUpdated:
		a.learnWakePhrases(e.Session.WakePhrases)
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
		a.playNotice(ctx, window, e)
	case realtimeproto.Error:
		a.logger.Error("服务端出错", "message", e.Message)
	}
}

// requestWakeState 请语音前端醒（awake）或睡。发出去不等于切成了：终端记的醒 / 睡等回复（applyWakeState）才改，
// 等不到（Timing.WakeConfirm）由主循环当作没切成。上一次的请求还没回复时不再发。
func (a *Assistant) requestWakeState(ctx context.Context, window *conversationWindow, awake bool, reason string) {
	if window.pending != nil {
		a.logger.Info("上一次切换还在等语音前端回复：这次不发", "reason", reason)
		return
	}
	var request realtimeproto.ClientEvent = realtimeproto.HaroWakeWordSleep{}
	if awake {
		request = realtimeproto.HaroWakeWordWake{}
	}
	if err := a.deps.Brain.Send(ctx, request); err != nil {
		if !errors.Is(err, context.Canceled) {
			a.logger.Error("切换醒 / 睡的请求没发出去", "awake", awake, "reason", reason, "err", err)
		}
		if window.awake {
			window.restart() // 这次的计时已经用掉了：重新计时，过一个对话窗口再试，否则窗口永远不再到期
		}
		return
	}
	window.await(awake, reason, a.opts.Timing.WakeConfirm)
}

// applyWakeState 照语音前端的回复（haro.wake_word.state）改终端记的醒 / 睡；和在等的请求一致时这次请求就完成了。
// 睡着了说一声：不出声的话，人不知道它已经睡了，接着说话会没反应。
func (a *Assistant) applyWakeState(ctx context.Context, window *conversationWindow, awake bool) {
	reason := "语音前端"
	if request := window.pending; request != nil {
		reason = request.reason
		if request.awake == awake {
			window.settle()
		}
	}
	switch {
	case awake && !window.awake:
		window.wake()
		a.logger.Info("👂 已唤醒（" + reason + "），直接说话就行")
	case !awake && window.awake:
		window.sleep()
		a.hearing.Store(false) // 说到一半休眠（例如按回车）：之后不会再有「说完」
		a.logger.Info("💤 " + reason + "，休眠中：" + a.wakeHint() + "（或按回车）")
		a.playSleepTone(ctx)
	}
}

// learnWakePhrases 记下服务端告诉的唤醒词（haro-asr 的关键词文件里的：唤醒词只写在那一处）。只在主循环里调用。
func (a *Assistant) learnWakePhrases(phrases []string) {
	if len(phrases) == 0 || slices.Equal(phrases, a.wakePhrases) {
		return
	}
	a.wakePhrases = slices.Clone(phrases)
	a.logger.Info("👂 唤醒词：" + strings.Join(phrases, "、"))
}

// wakeHint 是日志里「怎么叫醒」的提示：知道唤醒词时写出来，还不知道（服务端还没告诉）时只说叫唤醒词。
func (a *Assistant) wakeHint() string {
	if len(a.wakePhrases) == 0 {
		return "叫唤醒词唤醒"
	}
	return "叫「" + strings.Join(a.wakePhrases, "」或「") + "」唤醒"
}

// playNotice 服务端有事告诉主人（例如后台调查查完了、语音识别断开了）：响一声。醒着、睡着都响。
// 有专门提示音的种类（NoticeTones）播那一声，其余（包括不认识的）播通用的提示音。
//
// 醒着时语音前端在听：提示音和回答一样防回声（suppressEcho）。主人正说着这一句时先不响，留到这一轮答完：
// 这时响会混进这句话；防回声又会截断它（半双工上传静音会被当成说完；可以打断时语音前端开始只听唤醒词，
// 这句就丢了、也不会再报说完）。睡着时语音前端只认唤醒词，直接播。
func (a *Assistant) playNotice(ctx context.Context, window *conversationWindow, notice realtimeproto.HaroNotice) {
	if a.hearing.Load() {
		if len(a.heldNotices) < eventBacklog {
			a.logger.Info("🔔 服务端提示：主人正在说话，这一轮答完再响", "kind", notice.Kind)
			a.heldNotices = append(a.heldNotices, notice)
		} else {
			a.logger.Warn("事件太多，丢掉一个", "event", notice.Type(), "kind", notice.Kind)
		}
		return
	}
	a.logger.Info("🔔 服务端提示", "kind", notice.Kind)
	tone, ok := a.opts.NoticeTones[notice.Kind]
	if !ok {
		tone = a.opts.NoticeTone
	}
	if window.awake && a.audible(tone) {
		resume := a.suppressEcho(ctx)
		defer resume(true)
	}
	a.playClip(ctx, tone, "提示音")
}

// playHeldNotices 主人说完、这一轮答完以后，响留着的提示（只在主循环里调用）。
func (a *Assistant) playHeldNotices(ctx context.Context, window *conversationWindow) {
	if a.hearing.Load() || len(a.heldNotices) == 0 {
		return
	}
	held := a.heldNotices
	a.heldNotices = nil
	for _, notice := range held {
		a.playNotice(ctx, window, notice)
	}
}

// playSleepTone 休眠时说一声。放在语音前端回复睡着了之后：这时它只认唤醒词，提示音不会被当成用户说话。
func (a *Assistant) playSleepTone(ctx context.Context) {
	a.playClip(ctx, a.opts.WakeWord.SleepTone, "休眠提示音")
}

// audible 判断这段提示音放不放得出来（有声音、有音箱）。
func (a *Assistant) audible(tone realtimeproto.PCMAudio) bool {
	return len(tone.Data) > 0 && a.deps.Speaker != nil
}

// playClip 在本机音箱上播一段提示音（播完才返回；最多等提示音的时长 + Timing.ToneSlack）。
func (a *Assistant) playClip(ctx context.Context, tone realtimeproto.PCMAudio, what string) {
	if !a.audible(tone) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, tone.Duration()+a.opts.Timing.ToneSlack)
	defer cancel()
	stream, err := a.deps.Speaker.OpenStream(ctx, tone.Format)
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

// conversationWindow 是终端记的醒 / 睡和唤醒后的对话窗口计时（只在主循环里使用，不需要加锁）。
// awake 只照语音前端报的改（haro.wake_word.detected / state）；终端要切换时发请求，pending 记着在等回复的那次。
type conversationWindow struct {
	idle    time.Duration
	awake   bool
	timer   *time.Timer  // 对话窗口倒计时
	pending *wakeRequest // 发出去、还没等到回复的醒 / 睡请求
}

// wakeRequest 是一次还没等到回复的醒 / 睡请求。
type wakeRequest struct {
	awake  bool        // 请求醒（true）还是睡
	reason string      // 为什么切（手动、对话窗口结束），写日志用
	timer  *time.Timer // 等回复的期限
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

// await 记下刚发出的请求、开始等回复；等回复期间对话窗口不倒计时（到期也不会再发一次）。
func (w *conversationWindow) await(awake bool, reason string, timeout time.Duration) {
	w.stop()
	w.pending = &wakeRequest{awake: awake, reason: reason, timer: time.NewTimer(timeout)}
}

// settle 结束等回复，返回那次请求（没有在等时为 nil）。
func (w *conversationWindow) settle() *wakeRequest {
	request := w.pending
	if request != nil {
		request.timer.Stop()
		w.pending = nil
	}
	return request
}

// close 停掉全部计时（主循环退出时）。
func (w *conversationWindow) close() { w.stop(); w.settle() }

// expired 返回本次倒计时的通道；没有在计时时返回 nil（select 里永远不会触发）。
func (w *conversationWindow) expired() <-chan time.Time {
	if w.timer == nil {
		return nil
	}
	return w.timer.C
}

// replyOverdue 返回等回复的期限通道；没有在等时返回 nil。
func (w *conversationWindow) replyOverdue() <-chan time.Time {
	if w.pending == nil {
		return nil
	}
	return w.pending.timer.C
}

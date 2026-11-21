package asrserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/semaphore"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// VoiceDetector 是一个会话专用的 VAD（有状态，不可跨会话共享）。
type VoiceDetector interface {
	AcceptWaveform(samples []float32) // 16kHz 单声道，[-1, 1)
	IsSpeech() bool                   // 当前是否处在语音中
	PopSegments() []SpeechSegment     // 取出已经结束的语音段（静音持续够久之后才会出现）
	Close()
}

// SpeechSegment 是一段已结束的语音在会话时间轴上的位置（单位：采样点）。
type SpeechSegment struct {
	StartSample int
	SampleCount int
}

// VoiceDetectorConfig 是创建一个会话的 VAD 用的参数（由会话的 turn_detection 和 Timing 算出，见 Timing.voiceDetectorConfig）。
type VoiceDetectorConfig struct {
	Threshold  float64       // 语音概率到这么高算在说话（0–1）
	MinSilence time.Duration // 静音这么久，一段语音才算结束
	MinSpeech  time.Duration // 短于这么长的声音不算说话
	MaxSpeech  time.Duration // 一段说到这么长时，VAD 设法在下一个短停顿处切开（软上限：一段可能略长）
}

// VoiceDetectorFactory 按一个会话的参数创建 VAD。
type VoiceDetectorFactory func(config VoiceDetectorConfig) (VoiceDetector, error)

// KeywordSpotter 是一个会话专用的唤醒词识别流（有状态）。
type KeywordSpotter interface {
	AcceptWaveform(samples []float32) string // 返回识别到的唤醒词显示名，没有则为空
	Reset()                                  // 进入休眠时丢掉已积累的状态
	Close()
}

// KeywordSpotterFactory 为每个转写会话创建一个唤醒词识别流。
type KeywordSpotterFactory func() (KeywordSpotter, error)

// TurnDetector 判断一段语音在语气上说完没有（semantic_vad 用；haro-asr 注入 Smart Turn）。可跨会话共享。
type TurnDetector interface {
	EndOfTurnProbability(samples []float32) (float64, error) // 16kHz 单声道，取最后 8 秒
}

const samplesPerMs = realtimeproto.TranscriptionSampleRate / 1000

// handleRealtimeTranscription 实现 Realtime 转写会话的子集（事件定义见 realtimeproto）。
// slots 是同时进行的转写会话的名额（Limits.MaxSessions）：满了就拒绝新的会话。
func handleRealtimeTranscription(w http.ResponseWriter, r *http.Request, deps Dependencies, slots *semaphore.Weighted) {
	if r.URL.Query().Get("intent") != realtimeproto.TranscriptionIntent {
		writeError(w, http.StatusBadRequest, "只支持 intent="+realtimeproto.TranscriptionIntent)
		return
	}
	if deps.NewVoiceDetector == nil {
		writeError(w, http.StatusServiceUnavailable, "haro-asr "+missing("VAD 模型", deps.Settings.VoiceDetector)+"，提供不了流式转写")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(realtimeproto.MaxMessageBytes)
	ctx := r.Context()

	session, err := startTranscriptionSession(ctx, conn, deps, slots)
	if err != nil { // 不接受这份设置（或者没开起来、满了）：回带码的 error 再断开，对端据码判断要不要再连
		refusal := realtimeproto.ErrorEventFor("", err)
		deps.Logger.Warn("转写会话没有开始", "code", refusal.Code, "err", err)
		_ = writeEvent(ctx, conn, refusal)
		conn.Close(websocket.StatusPolicyViolation, "bad session.update")
		return
	}
	defer session.close()
	turn := session.turnDetection
	if err := writeEvent(ctx, conn, realtimeproto.SessionUpdated{Session: session.accepted()}); err != nil {
		deps.Logger.Warn("转写会话：确认设置的 session.updated 没发出去", "err", err)
		return
	}
	deps.Logger.Info("转写会话开始", "type", turn.Type, "silence_ms", turn.SilenceDurationMs,
		"eagerness", turn.Eagerness, "prefix_ms", turn.PrefixPaddingMs, "threshold", turn.Threshold,
		"wake_word", session.wake.enabled)
	err = session.run(ctx)
	switch {
	case ctx.Err() != nil: // 会话的 context 由服务派生：取消 = 服务在关停（客户端断开时连接读取会先报错）
		deps.Logger.Info("转写会话结束", "reason", "服务关停")
		return
	case websocket.CloseStatus(err) == websocket.StatusNormalClosure || errors.Is(err, io.EOF):
		deps.Logger.Info("转写会话结束", "reason", "客户端关闭")
		return
	}
	deps.Logger.Warn("转写会话异常结束", "err", err)
}

// utterance 是一句正在进行或等待判定的话：可能由多个 VAD 语音段组成（中间的停顿被模型判为「没说完」）。
type utterance struct {
	itemID         string
	startSample    int
	endSample      int
	waiting        bool   // 模型判定没说完（或刚被唤醒还没开口），正在等用户接着说
	wake           bool   // 这句话里有唤醒词：整句原样交出、标上 Wake（唤醒词由服务端的模型自己分）
	awaitingSpeech bool   // 唤醒词说完就停了，还没开口：最多等 Timing.WakeOnlyWait，等不到就把这一句（只有唤醒词）交出去
	bargeIn        bool   // 终端播放回答时用唤醒词打断的：前面可能混着 haro 自己的回声，整句原样交出、标上 BargeIn
	wakeSpeaker    string // 唤醒时声纹认得很准的人（达到 Threshold）：这一句沿用它，不再用这句（多半很短）重新比对
	// wakeGuess：唤醒时只过了唤醒门槛的人。这一句照常比对，没认出来时报成 SpeakerTentative（像是谁），
	// 由服务端按这句话的长短决定怎么回
	wakeGuess string
}

// transcriptionSession 是一个转写会话：把收到的音频按句切开、识别、报给服务端。由各管一件事的部件组成：
// 音频时间轴（timeline）、静音检测（vad）、唤醒门（wake）、回声窗口（echo）、说完判断（turn）；
// 这里只管一句话从开始到说完（current）和收发事件。
type transcriptionSession struct {
	conn          *websocket.Conn
	deps          Dependencies
	turnDetection realtimeproto.TurnDetection
	timing        Timing

	timeline audioTimeline
	vad      voiceActivity
	wake     wakeGate
	echo     echoWindow
	turn     turnEnd

	current   *utterance
	itemCount int

	releaseSlot func() // 交还同时进行的会话的名额（close 时调用）
}

// startTranscriptionSession 读取第一条消息（必须是 session.update，最多等 Limits.SessionUpdateWait），
// 校验通过、还有名额时占一个名额，创建这个会话的 VAD 和唤醒词识别。会话结束时 close 交还名额。
func startTranscriptionSession(ctx context.Context, conn *websocket.Conn, deps Dependencies,
	slots *semaphore.Weighted) (*transcriptionSession, error) {
	limits := deps.Limits.orDefault()
	readCtx, cancel := context.WithTimeout(ctx, limits.SessionUpdateWait)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return nil, fmt.Errorf("读取 session.update: %w", err)
	}
	event, err := realtimeproto.DecodeClientEvent(data, realtimeproto.AudioFormat{})
	if err != nil {
		return nil, err
	}
	update, ok := event.(realtimeproto.SessionUpdate)
	switch {
	case !ok || update.Kind != realtimeproto.SessionTranscription:
		return nil, realtimeproto.ProtocolErrorf(realtimeproto.CodeInvalidEvent, "第一条消息必须是 type=transcription 的 session.update")
	case update.InputFormat != realtimeproto.TranscriptionFormat:
		return nil, realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedAudioFormat, "只支持 %dHz 单声道的 audio/pcm",
			realtimeproto.TranscriptionSampleRate)
	case update.TurnDetection == nil:
		return nil, realtimeproto.ProtocolErrorf(realtimeproto.CodeInvalidEvent, "必须指定 turn_detection")
	}
	turn := *update.TurnDetection
	if err := turn.Validate(); err != nil { // 写错的取值明确拒绝，不悄悄当成默认值
		return nil, realtimeproto.ProtocolErrorf(realtimeproto.CodeInvalidEvent, "%v", err)
	}
	timing := deps.Timing.orDefault()
	session := &transcriptionSession{conn: conn, deps: deps, turnDetection: turn, timing: timing,
		timeline: audioTimeline{keep: samplesIn(timing.History)}, echo: newEchoWindow(timing),
		turn: turnEnd{cutoff: timing.EndOfTurnCutoff, defaultWait: samplesIn(timing.DefaultWait)}}
	vadConfig := turn
	switch turn.Type {
	case realtimeproto.TurnServerVAD:
	case realtimeproto.TurnSemanticVAD:
		if deps.TurnDetector == nil {
			return nil, realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedFeature, "haro-asr %s，不支持 semantic_vad",
				missing("说完判断模型", deps.Settings.TurnDetector))
		}
		session.turn.semantic, session.turn.detector = true, deps.TurnDetector
		session.turn.maxWait = samplesIn(session.timing.maxWait(turn.Eagerness))
		vadConfig.SilenceDurationMs = int(session.timing.SemanticSilence.Milliseconds())
	}
	if update.WakeWord && deps.NewKeywordSpotter == nil {
		return nil, realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedFeature, "haro-asr %s，不支持唤醒词",
			missing("唤醒词模型", deps.Settings.KeywordSpotter))
	}
	if !slots.TryAcquire(1) { // 每个会话自己建 VAD（重新载入模型）和唤醒词流：数量不设上限，连接失控时会撑爆内存
		return nil, realtimeproto.ProtocolErrorf(realtimeproto.CodeTooManySessions, "haro-asr 同时进行的转写会话已经有 %d 个（上限）：稍后再试",
			limits.MaxSessions)
	}
	session.releaseSlot = func() { slots.Release(1) }
	if update.WakeWord {
		spotter, err := deps.NewKeywordSpotter()
		if err != nil {
			session.close()
			return nil, fmt.Errorf("创建唤醒词识别: %w", err)
		}
		session.wake = wakeGate{enabled: true, spotter: spotter}
	}
	detector, err := deps.NewVoiceDetector(timing.voiceDetectorConfig(vadConfig))
	if err != nil {
		session.close()
		return nil, fmt.Errorf("创建 VAD: %w", err)
	}
	session.vad.detector = detector
	return session, nil
}

// close 释放这个会话的 VAD、唤醒词识别，交还名额。
func (s *transcriptionSession) close() {
	if s.vad.detector != nil {
		s.vad.detector.Close()
	}
	s.wake.close()
	s.releaseSlot()
}

// accepted 是这个会话接受之后的设置（session.updated 里回给对端）。
func (s *transcriptionSession) accepted() realtimeproto.SessionUpdate {
	turn := s.turnDetection
	return realtimeproto.SessionUpdate{Kind: realtimeproto.SessionTranscription, TurnDetection: &turn,
		WakeWord: s.wake.enabled, InputFormat: realtimeproto.TranscriptionFormat}
}

func (s *transcriptionSession) run(ctx context.Context) error {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return err
		}
		event, err := realtimeproto.DecodeClientEvent(data, realtimeproto.TranscriptionFormat)
		if err != nil {
			_ = writeEvent(ctx, s.conn, realtimeproto.ErrorEventFor("", err))
			continue
		}
		switch event.(type) {
		case realtimeproto.InputAudioBufferAppend:
			if err := s.acceptAudio(ctx, event.(realtimeproto.InputAudioBufferAppend).Audio.Data); err != nil {
				return err
			}
		case realtimeproto.HaroWakeWordSleep, realtimeproto.HaroWakeWordWake, realtimeproto.HaroPlaybackStarted,
			realtimeproto.HaroPlaybackStopped:
			if !s.wake.enabled {
				_ = writeEvent(ctx, s.conn, realtimeproto.Error{Code: realtimeproto.CodeUnsupportedFeature,
					Message: event.Type() + ": 这个会话没有启用唤醒门"})
				continue
			}
			if err := s.applyWakeEvent(ctx, event); err != nil {
				return err
			}
		default:
			_ = writeEvent(ctx, s.conn, realtimeproto.Error{Code: realtimeproto.CodeUnsupportedEvent,
				Message: "转写会话不支持 " + event.Type()})
		}
	}
}

// acceptAudio 喂给 VAD，把语音开始 / 结束翻译成事件；一句话说完时识别它。
func (s *transcriptionSession) acceptAudio(ctx context.Context, pcm []byte) error {
	samples := s.timeline.append(pcm)
	now := s.timeline.now()
	if s.echo.playing {
		if rms, peak, ok := s.echo.level.add(samples); ok {
			s.deps.Logger.Info("播放中麦克风音量", "rms", fmt.Sprintf("%.4f", rms), "peak", fmt.Sprintf("%.3f", peak))
		}
	}
	speaking := s.vad.feed(samples)
	// 醒着时也识别唤醒词：再叫一次同样标上唤醒（文字原样交出），终端收到经服务端转来的唤醒事件后重新开始对话窗口计时
	if keyword := s.wake.spot(samples); keyword != "" {
		if err := s.wakeUp(ctx, keyword, speaking); err != nil {
			return err
		}
	}
	listening := s.wake.listening(s.echo.playing)

	if s.vad.begins(speaking, now) && listening {
		s.deps.Logger.Info("VAD：开始说话", "at_ms", now/samplesPerMs, "after_wake_ms", s.wake.sinceWakeMs(now),
			"in_item", s.current != nil)
		switch {
		case s.current == nil: // 新的一句
			if err := s.beginUtterance(ctx, now); err != nil {
				return err
			}
		case s.current.waiting: // 用户在停顿后接着说：还是同一句
			s.current.waiting = false
			s.current.awaitingSpeech = false
		}
	}
	s.vad.settle(speaking)

	for _, segment := range s.vad.segments() {
		if !listening {
			s.deps.Logger.Debug("忽略（未唤醒）") // 隐私：休眠时不记录内容
			continue
		}
		if err := s.handleSegment(ctx, segment); err != nil {
			return err
		}
	}
	// 判定「没说完」（或刚唤醒还没开口）后，等太久没有接着说，就按说完处理
	limit := s.turn.waitLimit()
	if s.current != nil && s.current.awaitingSpeech {
		limit = samplesIn(s.timing.WakeOnlyWait)
	}
	if s.current != nil && s.current.waiting && !speaking && now-s.current.endSample >= limit {
		s.deps.Logger.Info("等待接着说超时，按说完处理", "item", s.current.itemID)
		return s.finishUtterance(ctx)
	}
	return nil
}

// wakeUp 听到唤醒词：进入唤醒状态，并把唤醒词所在的这句话标记为「唤醒句」（一口气说完时接得住）。
func (s *transcriptionSession) wakeUp(ctx context.Context, keyword string, speaking bool) error {
	// 声纹：唤醒词要是登记过的人说的才醒——旁人叫不醒，播放时 haro 自己的声音也打断不了
	// 唤醒词只有 1 秒多，声纹算不稳：门槛比说话时低；没到说话门槛的，唤醒句照常比对
	wakeSpeaker, wakeGuess := "", ""
	now := s.timeline.now()
	window := pcm16ToFloat(s.timeline.slice(s.echo.wakeWindowStart(now), now))
	quality := wakeQuality{level: rootMeanSquare(window)} // 几台终端同时听到时，服务端据此选一台回答
	if s.deps.Speakers.enabled() {
		name, score, sure, err := s.deps.Speakers.identifyWake(window)
		quality.score = score
		switch {
		case err != nil:
			s.deps.Logger.Warn("唤醒词的声纹算不出来，照常唤醒", "err", err)
		case name == "":
			s.deps.Logger.Info("唤醒词不是登记的人说的，不唤醒", "score", fmt.Sprintf("%.2f", score),
				"playback", s.echo.playing)
			return nil
		case sure:
			wakeSpeaker = name
			s.deps.Logger.Info("唤醒词声纹", "speaker", name, "score", fmt.Sprintf("%.2f", score))
		default:
			wakeGuess = name
			s.deps.Logger.Info("唤醒词声纹（只过了唤醒门槛：这句话还要再比对）", "speaker", name,
				"score", fmt.Sprintf("%.2f", score))
		}
	}
	bargeIn := s.echo.playing
	if bargeIn { // 播放回答时被叫：这是打断
		s.echo.interrupt()
		s.current = nil // 播放期间不会有进行中的句子；保险起见丢掉
	}
	s.wake.woke(now)
	s.deps.Logger.Info("听到唤醒词", "keyword", keyword, "audio_ms", now/samplesPerMs,
		"vad_speaking", speaking, "speech_start_ms", s.vad.speechStart/samplesPerMs, "in_item", s.current != nil)
	if err := writeEvent(ctx, s.conn, realtimeproto.HaroWakeWordDetected{Keyword: keyword,
		AudioMs: int64(now / samplesPerMs), Score: quality.score, Level: quality.level}); err != nil {
		return err
	}
	if s.current != nil { // 醒着时这句话已经开始了：只需标上唤醒（整句原样交出）
		s.current.wake = true
		s.current.wakeSpeaker, s.current.wakeGuess = wakeSpeaker, wakeGuess
		return nil
	}
	if err := s.beginUtterance(ctx, now); err != nil {
		return err
	}
	// 往前多取一段，确保整个唤醒词都在识别范围内（整句交给服务端，唤醒词由模型自己分）；
	// 但不越过唤醒词所在这段连续语音的开头——休眠时更早的话不能被带进来。
	// 打断时这段「连续语音」其实是 haro 自己的回声，开头没有意义，只按窗口往前取。
	keywordWindow := samplesIn(s.timing.WakeKeywordWindow)
	s.current.startSample = max(now-keywordWindow, s.vad.speechStart, s.echo.until, 0)
	if bargeIn {
		s.current.startSample = max(now-keywordWindow, 0)
		s.echo.cutAt(s.current.startSample)
		s.current.bargeIn = true
		s.deps.Logger.Info("打断：播放回答时听到唤醒词", "item", s.current.itemID)
	}
	s.current.wake = true
	s.current.wakeSpeaker, s.current.wakeGuess = wakeSpeaker, wakeGuess
	s.current.waiting = !speaking // 唤醒词说完就停了：等用户开口，等不到就把只有唤醒词的这一句交出去
	s.current.awaitingSpeech = !speaking
	s.vad.settle(speaking)
	return nil
}

// startPlayback：终端开始播放回答。只听唤醒词（打断），记下播放前醒没醒，播完恢复。
func (s *transcriptionSession) startPlayback() {
	s.wake.pauseForPlayback(s.echo.start())
	s.current = nil
	s.deps.Logger.Info("播放中：只听唤醒词（打断）")
}

// stopPlayback：终端播完了，恢复播放前的状态。被打断时已经醒了（wakeUp 里已经不算在播放），这里不用动。
func (s *transcriptionSession) stopPlayback() {
	if !s.echo.stop(s.timeline.now()) {
		return
	}
	s.wake.resumeAfterPlayback()
	s.deps.Logger.Info("播放结束：恢复收听", "awake", s.wake.awake)
}

// sleep 进入休眠：丢掉进行中的句子，重置唤醒词识别。
func (s *transcriptionSession) sleep() {
	s.wake.sleep()
	s.current = nil
	s.deps.Logger.Info("进入休眠")
}

func (s *transcriptionSession) beginUtterance(ctx context.Context, atSample int) error {
	s.itemCount++
	s.current = &utterance{itemID: fmt.Sprintf("item_%d", s.itemCount), startSample: atSample, endSample: atSample}
	return writeEvent(ctx, s.conn, realtimeproto.InputAudioBufferSpeechStarted{
		AudioStartMs: int64(atSample / samplesPerMs), ItemID: s.current.itemID})
}

// handleSegment 把一个 VAD 语音段并入当前这句话，再决定是否已经说完。
func (s *transcriptionSession) handleSegment(ctx context.Context, segment SpeechSegment) error {
	end := segment.StartSample + segment.SampleCount
	trimmed, cut, ignore := s.echo.trim(segment) // 开头是 haro 自己的回声：只取回声之后的部分
	switch {
	case ignore:
		s.deps.Logger.Info("忽略：haro 自己的声音（播放时开始的语音段）", "start_ms", segment.StartSample/samplesPerMs,
			"end_ms", end/samplesPerMs, "echo_until_ms", s.echo.until/samplesPerMs)
		return nil
	case cut:
		s.deps.Logger.Info("语音段开头是 haro 的回声，剪掉", "start_ms", segment.StartSample/samplesPerMs,
			"echo_until_ms", s.echo.until/samplesPerMs)
	}
	segment = trimmed
	s.deps.Logger.Info("VAD 语音段", "start_ms", segment.StartSample/samplesPerMs, "end_ms", end/samplesPerMs,
		"length_ms", segment.SampleCount/samplesPerMs, "after_wake_ms", s.wake.sinceWakeMs(segment.StartSample),
		"in_item", s.current != nil, "wake_item", s.current != nil && s.current.wake)
	if s.current == nil { // 语音段太短，IsSpeech 没来得及变成 true
		if err := s.beginUtterance(ctx, segment.StartSample); err != nil {
			return err
		}
	}
	s.current.startSample = min(s.current.startSample, segment.StartSample)
	s.current.endSample = max(s.current.endSample, segment.StartSample+segment.SampleCount)
	if !s.turn.semantic {
		return s.finishUtterance(ctx)
	}
	// 并到上限就不再等接着说：一句话不能无限长，否则保留的音频装不下它的开头
	if length := s.current.endSample - s.current.startSample; length >= samplesIn(s.timing.MergeLimit) {
		s.deps.Logger.Info("这句话并到了上限，不再等接着说，按说完处理", "item", s.current.itemID,
			"length_ms", length/samplesPerMs, "merge_limit", s.timing.MergeLimit)
		return s.finishUtterance(ctx)
	}

	// semantic_vad：把这句话（含刚才的短暂静音）交给模型判断
	started := time.Now()
	audio := s.timeline.slice(s.echo.paddedStart(s.current.startSample, s.turnDetection.PrefixPaddingMs), s.timeline.now())
	complete, probability, err := s.turn.complete(audio)
	if err != nil { // 模型出错时退回「静音即说完」，不让对话卡住
		s.deps.Logger.Error("说完判断失败，按说完处理", "err", err)
		return s.finishUtterance(ctx)
	}
	s.deps.Logger.Info("说完判断", "item", s.current.itemID, "probability", fmt.Sprintf("%.2f", probability),
		"complete", complete, "elapsed", time.Since(started).Round(time.Millisecond))
	if complete {
		return s.finishUtterance(ctx)
	}
	s.current.waiting = true
	return nil
}

// finishUtterance 发出 speech_stopped，识别这句话并发出结果。
func (s *transcriptionSession) finishUtterance(ctx context.Context) error {
	current := s.current
	s.current = nil
	if err := writeEvent(ctx, s.conn, realtimeproto.InputAudioBufferSpeechStopped{
		AudioEndMs: int64(current.endSample / samplesPerMs), ItemID: current.itemID}); err != nil {
		return err
	}
	audio := realtimeproto.PCMAudio{
		Format: realtimeproto.TranscriptionFormat,
		Data:   s.timeline.slice(s.echo.paddedStart(current.startSample, s.turnDetection.PrefixPaddingMs), current.endSample),
	}
	if lost := s.timeline.lostFrom(current.startSample); lost > 0 { // History 装不下这句话：开头已经丢了，照样识别剩下的
		s.deps.Logger.Warn("这句话的开头已经不在保留的音频里，截掉了", "item", current.itemID, "lost_ms", lost/samplesPerMs,
			"length_ms", (current.endSample-current.startSample)/samplesPerMs, "history", s.timing.History)
	}
	started := time.Now()
	transcript, err := s.deps.Transcriber.Transcribe(ctx, audio)
	if err != nil { // 带上是哪一句：服务端等这句结果的回答据此结束（只发不带编号的 error 时那一轮会一直等）
		s.deps.Logger.Warn("识别失败", "item", current.itemID, "err", err)
		return writeEvent(ctx, s.conn, realtimeproto.InputAudioTranscriptionFailed{ItemID: current.itemID,
			Code: realtimeproto.CodeTranscriptionFailed, Message: "识别失败: " + err.Error()})
	}
	s.deps.Logger.Info("一句话的范围", "item", current.itemID, "start_ms", current.startSample/samplesPerMs,
		"end_ms", current.endSample/samplesPerMs, "after_wake_ms", s.wake.sinceWakeMs(current.startSample), "wake", current.wake)
	// 唤醒词、打断时混进的回声都不在这里去掉：整句交给服务端，标上「叫醒后的第一句」「打断」，由模型自己分
	s.deps.Logger.Info("识别完成", "item", current.itemID, "audio", audio.Duration().Round(time.Millisecond),
		"elapsed", time.Since(started).Round(time.Millisecond), "text", transcript.Text, "language", transcript.Language,
		"wake", current.wake, "barge_in", current.bargeIn)
	// 唤醒时已经认出来了：这句沿用，不再对整句另算一次声纹（也不让很短的唤醒句重新比对推翻它）
	speaker, checked, failed, tentative := current.wakeSpeaker, true, false, ""
	if current.wakeSpeaker == "" {
		speaker, checked, failed = s.identifySpeaker(current.itemID, audio.Data)
		if current.wakeGuess != "" && speaker == "" {
			// 唤醒时只过了唤醒门槛：不算认出来，告诉服务端像是谁，由它按这句话的长短决定怎么回（短句受限回答）
			tentative = current.wakeGuess
		}
	}
	return writeEvent(ctx, s.conn, realtimeproto.InputAudioTranscriptionCompleted{ItemID: current.itemID,
		Transcript: transcript.Text, Speaker: speaker, SpeakerChecked: checked, SpeakerCheckFailed: failed,
		SpeakerTentative: tentative, Language: transcript.Language, Wake: current.wake, BargeIn: current.bargeIn,
		AudioMs: audio.Duration().Milliseconds()})
}

// identifySpeaker 比对这句话的声纹。没登记声纹时不比对（checked、failed 都是 false）；
// 算不出声纹时 failed = true：和「没比对」分开报，由服务端决定怎么处理。
func (s *transcriptionSession) identifySpeaker(itemID string, pcm []byte) (name string, checked, failed bool) {
	if !s.deps.Speakers.enabled() {
		return "", false, false
	}
	started := time.Now()
	name, score, err := s.deps.Speakers.identify(pcm16ToFloat(pcm))
	if err != nil {
		s.deps.Logger.Warn("这句的声纹算不出来：报给服务端（speaker_check_failed）", "item", itemID, "err", err)
		return "", false, true
	}
	s.deps.Logger.Info("声纹", "item", itemID, "speaker", name, "score", fmt.Sprintf("%.2f", score),
		"elapsed", time.Since(started).Round(time.Millisecond))
	return name, true, false
}

// applyWakeEvent 处理唤醒门的四个控制事件（会话启用了唤醒门时才会走到这里）。
// 休眠 / 手动唤醒处理完回 haro.wake_word.state：醒 / 睡只在这里改，服务端和终端照这个回复改它们记的。
func (s *transcriptionSession) applyWakeEvent(ctx context.Context, event realtimeproto.ClientEvent) error {
	switch event.(type) {
	case realtimeproto.HaroWakeWordSleep:
		s.sleep()
	case realtimeproto.HaroWakeWordWake:
		s.wake.wakeManually()
		s.deps.Logger.Info("手动唤醒")
	case realtimeproto.HaroPlaybackStarted:
		s.startPlayback()
		return nil
	case realtimeproto.HaroPlaybackStopped:
		s.stopPlayback()
		return nil
	}
	return writeEvent(ctx, s.conn, realtimeproto.HaroWakeWordState{Awake: s.wake.awake})
}

// writeEvent 把一个服务端事件编码后发给对端。
func writeEvent(ctx context.Context, conn *websocket.Conn, event realtimeproto.ServerEvent) error {
	data, err := realtimeproto.EncodeServerEvent(event)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

package asrserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

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

// VoiceDetectorFactory 按会话的 turn_detection 配置创建 VAD（SilenceDurationMs 即 VAD 的最短静音）。
type VoiceDetectorFactory func(turnDetection realtimeproto.TurnDetection) (VoiceDetector, error)

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

const (
	samplesPerMs   = realtimeproto.TranscriptionSampleRate / 1000
	historySeconds = 30 // 只保留最近 30 秒音频，用于截取语音段（含前置缓冲）

	// semantic_vad：VAD 静音这么久就交给模型判断。200 → 300 ms（v3 D-2）：真机两次实测，句子说完 238 ms 后
	// 尾音（换气、拖音）又冒出一小段，被切成新的一句、识别成「Yeah.」；等 300 ms 让尾音并进同一句（每句慢约 0.1 s）。
	semanticSilenceMs = 300

	wakeOnlyWaitMs      = 1000 // 只叫了唤醒词：等这么久没开口就回「我在。」（2 s 太久，改成 1 s）
	wakeKeywordWindowMs = 1500 // 唤醒词识别报告时刻之前的这段一定包含整个唤醒词（「哈罗哈罗」约 1 s + 报告延迟约 0.3 s）
	endOfTurnCutoff     = 0.5  // 模型概率 ≥ 0.5 算说完（与 Smart Turn 参考实现一致）
)

// semanticMaxWait 是 semantic_vad 判断「没说完」之后最多再等多久（按 eagerness）。
// 官方 Realtime 为 low 8s / medium 4s / high 2s；面对面聊天等太久很别扭，这里整体缩短。
func semanticMaxWait(eagerness string) time.Duration {
	switch eagerness {
	case "low":
		return 3 * time.Second
	case "high":
		return time.Second
	default: // "medium" / "auto" / 空
		return 2 * time.Second
	}
}

// handleRealtimeTranscription 实现 Realtime 转写会话的子集（见 realtimeproto/transcription_wire.go）。
func handleRealtimeTranscription(w http.ResponseWriter, r *http.Request, deps Dependencies) {
	if r.URL.Query().Get("intent") != "transcription" {
		writeError(w, http.StatusBadRequest, "只支持 intent=transcription")
		return
	}
	if deps.NewVoiceDetector == nil {
		writeError(w, http.StatusServiceUnavailable, "haro-asr 启动时没有指定 -vad-model，无法提供流式转写")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	ctx := r.Context()

	session, err := startTranscriptionSession(ctx, conn, deps)
	if err != nil {
		_ = wsjson.Write(ctx, conn, realtimeproto.NewWireError("invalid_request_error", err.Error()))
		conn.Close(websocket.StatusPolicyViolation, "bad session.update")
		return
	}
	defer session.detector.Close()
	if session.spotter != nil {
		defer session.spotter.Close()
	}
	turn := session.turnDetection
	deps.Logger.Info("转写会话开始", "type", turn.Type, "silence_ms", turn.SilenceDurationMs,
		"eagerness", turn.Eagerness, "prefix_ms", turn.PrefixPaddingMs, "threshold", turn.Threshold,
		"wake_word", session.wakeEnabled)
	err = session.run(ctx)
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure || errors.Is(err, io.EOF) || ctx.Err() != nil {
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
	wake           bool   // 这句话里有唤醒词：识别后在文字上去掉它
	awaitingSpeech bool   // 唤醒词说完就停了，还没开口：最多等 wakeOnlyWaitMs，等不到就按「只叫了唤醒词」处理
	bargeIn        bool   // 终端播放回答时用唤醒词打断的（v3 I-1）：唤醒词前面混着 haro 自己的回声，去掉唤醒词时连前面一起去掉
	wakeSpeaker    string // 唤醒时声纹认得很准的人（达到 Threshold，v3 V-a）：这一句沿用它，不再用这句（多半很短）重新比对
	wakeGuess      string // 唤醒时只过了唤醒门槛的人（v3 BV）：这一句照常比对；只叫了唤醒词（去掉后没有文字）时才沿用，让 haro 回「我在」
}

type transcriptionSession struct {
	conn          *websocket.Conn
	deps          Dependencies
	detector      VoiceDetector
	turnDetection realtimeproto.TurnDetection
	semantic      bool
	maxWaitSample int

	// 唤醒门（haro 扩展，v2.2）：休眠时只跑唤醒词识别，不识别、不上报任何内容
	wakeEnabled bool
	awake       bool
	spotter     KeywordSpotter

	// 打断（haro 扩展，v3 I-1）：终端播放回答期间只听唤醒词（麦克风里主要是 haro 自己的回声）
	playback            bool
	awakeBeforePlayback bool
	playbackLevel       levelMeter // 播放期间的麦克风音量（排查打断用：设备会不会在放声音时把麦克风压低）
	// echoUntil：这个采样点之前的声音是 haro 自己的回声（v3 E-1）。播完时 = 播放结束的位置；打断时 = 打断句的开头。
	// 播放期间开始、播完后才结束的语音段，只取这之后的部分——否则回声会被当成用户说的话，
	// 用户紧接着说的话也会和回声并成一句（声纹被拉低、被当成陌生人丢掉）。
	echoUntil int

	totalSamples int    // 会话时间轴：累计收到的采样点
	history      []byte // 最近的 pcm16
	historyStart int    // history[0] 对应的采样点
	speaking     bool
	speechStart  int // 最近一段连续语音的开头（VAD 判定开始说话的位置；休眠时也记录，只用来限定唤醒句的范围）
	current      *utterance
	itemCount    int
	lastWakeAt   int // 最近一次听到唤醒词的位置（采样点；排查唤醒后多出一句「Yeah.」用）
}

// startTranscriptionSession 读取第一条消息（必须是 session.update）并创建 VAD。
func startTranscriptionSession(ctx context.Context, conn *websocket.Conn, deps Dependencies) (*transcriptionSession, error) {
	var update realtimeproto.WireTranscriptionSessionUpdate
	if err := wsjson.Read(ctx, conn, &update); err != nil {
		return nil, fmt.Errorf("读取 session.update: %w", err)
	}
	input := update.Session.Audio.Input
	switch {
	case update.Type != "session.update" || update.Session.Type != "transcription":
		return nil, errors.New("第一条消息必须是 type=transcription 的 session.update")
	case input.Format.Rate != realtimeproto.TranscriptionSampleRate:
		return nil, fmt.Errorf("只支持 %dHz 的 audio/pcm", realtimeproto.TranscriptionSampleRate)
	case input.TurnDetection == nil:
		return nil, errors.New("必须指定 turn_detection")
	}
	turn := *input.TurnDetection
	session := &transcriptionSession{conn: conn, deps: deps, turnDetection: turn}
	vadConfig := turn
	switch turn.Type {
	case "server_vad":
	case "semantic_vad":
		if deps.TurnDetector == nil {
			return nil, errors.New("haro-asr 启动时没有指定 -turn-model，不支持 semantic_vad")
		}
		session.semantic = true
		session.maxWaitSample = int(semanticMaxWait(turn.Eagerness).Milliseconds()) * samplesPerMs
		vadConfig.SilenceDurationMs = semanticSilenceMs
	default:
		return nil, fmt.Errorf("不支持的 turn_detection.type %q（支持 server_vad / semantic_vad）", turn.Type)
	}
	if haro := update.Session.Haro; haro != nil && haro.WakeWord != nil && haro.WakeWord.Enabled {
		if deps.NewKeywordSpotter == nil {
			return nil, errors.New("haro-asr 启动时没有指定 -kws-model-dir / -keywords-file，不支持唤醒词")
		}
		spotter, err := deps.NewKeywordSpotter()
		if err != nil {
			return nil, fmt.Errorf("创建唤醒词识别: %w", err)
		}
		session.wakeEnabled, session.spotter = true, spotter
	}
	detector, err := deps.NewVoiceDetector(vadConfig)
	if err != nil {
		if session.spotter != nil {
			session.spotter.Close()
		}
		return nil, fmt.Errorf("创建 VAD: %w", err)
	}
	session.detector = detector
	return session, nil
}

func (s *transcriptionSession) run(ctx context.Context) error {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return err
		}
		var head struct {
			Type  string `json:"type"`
			Audio string `json:"audio"`
		}
		if err := json.Unmarshal(data, &head); err == nil && s.wakeEnabled {
			switch head.Type {
			case "haro.wake_word.sleep":
				s.sleep()
				continue
			case "haro.wake_word.wake":
				s.awake = true
				s.deps.Logger.Info("手动唤醒")
				continue
			case "haro.playback.started":
				s.startPlayback()
				continue
			case "haro.playback.stopped":
				s.stopPlayback()
				continue
			}
		}
		if err := json.Unmarshal(data, &head); err != nil || head.Type != "input_audio_buffer.append" {
			_ = wsjson.Write(ctx, s.conn, realtimeproto.NewWireError("invalid_request_error",
				fmt.Sprintf("不支持的事件 %q（只接受 input_audio_buffer.append）", head.Type)))
			continue
		}
		pcm, err := base64.StdEncoding.DecodeString(head.Audio)
		if err != nil {
			_ = wsjson.Write(ctx, s.conn, realtimeproto.NewWireError("invalid_request_error", "audio 不是合法的 base64"))
			continue
		}
		if err := s.acceptAudio(ctx, pcm); err != nil {
			return err
		}
	}
}

// acceptAudio 喂给 VAD，把语音开始 / 结束翻译成事件；一句话说完时识别它。
func (s *transcriptionSession) acceptAudio(ctx context.Context, pcm []byte) error {
	s.appendHistory(pcm)
	samples := make([]float32, len(pcm)/2)
	for i := range samples {
		samples[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	s.totalSamples += len(samples)
	if s.playback {
		if rms, peak, ok := s.playbackLevel.add(samples); ok {
			s.deps.Logger.Info("播放中麦克风音量", "rms", fmt.Sprintf("%.4f", rms), "peak", fmt.Sprintf("%.3f", peak))
		}
	}
	s.detector.AcceptWaveform(samples)
	speaking := s.detector.IsSpeech()

	if s.wakeEnabled { // 醒着时也识别：再叫一次唤醒词同样去掉它，并让终端重新开始对话窗口计时
		if keyword := s.spotter.AcceptWaveform(samples); keyword != "" {
			if err := s.wakeUp(ctx, keyword, speaking); err != nil {
				return err
			}
		}
	}
	listening := (!s.wakeEnabled || s.awake) && !s.playback

	if speaking && !s.speaking {
		s.speechStart = s.totalSamples
	}
	if speaking && !s.speaking && listening {
		s.deps.Logger.Info("VAD：开始说话", "at_ms", s.totalSamples/samplesPerMs, "after_wake_ms", s.sinceWakeMs(s.totalSamples),
			"in_item", s.current != nil)
		switch {
		case s.current == nil: // 新的一句
			if err := s.beginUtterance(ctx, s.totalSamples); err != nil {
				return err
			}
		case s.current.waiting: // 用户在停顿后接着说：还是同一句
			s.current.waiting = false
			s.current.awaitingSpeech = false
		}
	}
	s.speaking = speaking

	for _, segment := range s.detector.PopSegments() {
		if !listening {
			s.deps.Logger.Debug("忽略（未唤醒）") // 隐私：休眠时不记录内容
			continue
		}
		if err := s.handleSegment(ctx, segment); err != nil {
			return err
		}
	}
	// 判定「没说完」（或刚唤醒还没开口）后，等太久没有接着说，就按说完处理
	limit := s.waitLimit()
	if s.current != nil && s.current.awaitingSpeech {
		limit = wakeOnlyWaitMs * samplesPerMs
	}
	if s.current != nil && s.current.waiting && !speaking && s.totalSamples-s.current.endSample >= limit {
		s.deps.Logger.Info("等待接着说超时，按说完处理", "item", s.current.itemID)
		return s.finishUtterance(ctx)
	}
	return nil
}

// wakeUp 听到唤醒词：进入唤醒状态，并把唤醒词所在的这句话标记为「唤醒句」（一口气说完时接得住）。
func (s *transcriptionSession) wakeUp(ctx context.Context, keyword string, speaking bool) error {
	// 声纹（v3 H-3a）：唤醒词要是登记过的人说的才醒——旁人叫不醒，播放时 haro 自己的声音也打断不了
	// 唤醒词只有 1 秒多，声纹算不稳：门槛比说话时低（v3 BV）；没到说话门槛的，唤醒句照常比对
	wakeSpeaker, wakeGuess := "", ""
	window := pcm16ToFloat(s.sliceHistory(s.wakeWindowStart(), s.totalSamples))
	quality := wakeQuality{level: rootMeanSquare(window)} // 几台终端同时听到时，服务端据此选一台回答（v4 D2）
	if s.deps.Speakers.enabled() {
		name, score, sure, err := s.deps.Speakers.identifyWake(window)
		quality.score = score
		switch {
		case err != nil:
			s.deps.Logger.Warn("唤醒词的声纹算不出来，照常唤醒", "err", err)
		case name == "":
			s.deps.Logger.Info("唤醒词不是登记的人说的，不唤醒", "score", fmt.Sprintf("%.2f", score),
				"playback", s.playback)
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
	bargeIn := s.playback
	if bargeIn { // 播放回答时被叫：这是打断
		s.playback = false
		s.current = nil // 播放期间不会有进行中的句子；保险起见丢掉
	}
	s.awake = true
	s.lastWakeAt = s.totalSamples
	s.deps.Logger.Info("听到唤醒词", "keyword", keyword, "audio_ms", s.totalSamples/samplesPerMs,
		"vad_speaking", speaking, "speech_start_ms", s.speechStart/samplesPerMs, "in_item", s.current != nil)
	if err := wsjson.Write(ctx, s.conn, realtimeproto.WireWakeWordDetected{
		Type: "haro.wake_word.detected", Keyword: keyword, AudioMs: int64(s.totalSamples / samplesPerMs),
		Score: quality.score, Level: quality.level}); err != nil {
		return err
	}
	if s.current != nil { // 醒着时这句话已经开始了：只需标记，识别后去掉唤醒词
		s.current.wake = true
		s.current.wakeSpeaker, s.current.wakeGuess = wakeSpeaker, wakeGuess
		return nil
	}
	if err := s.beginUtterance(ctx, s.totalSamples); err != nil {
		return err
	}
	// 往前多取一段，确保整个唤醒词都在识别范围内（识别后在文字上去掉，见 StripWakePhrase）；
	// 但不越过唤醒词所在这段连续语音的开头——休眠时更早的话不能被带进来。
	// 打断时这段「连续语音」其实是 haro 自己的回声，开头没有意义，只按窗口往前取。
	s.current.startSample = max(s.totalSamples-wakeKeywordWindowMs*samplesPerMs, s.speechStart, s.echoUntil, 0)
	if bargeIn {
		s.current.startSample = max(s.totalSamples-wakeKeywordWindowMs*samplesPerMs, 0)
		s.echoUntil = s.current.startSample
		s.current.bargeIn = true
		s.deps.Logger.Info("打断：播放回答时听到唤醒词", "item", s.current.itemID)
	}
	s.current.wake = true
	s.current.wakeSpeaker, s.current.wakeGuess = wakeSpeaker, wakeGuess
	s.current.waiting = !speaking // 唤醒词说完就停了：等用户开口，等不到就按「只叫了唤醒词」处理
	s.current.awaitingSpeech = !speaking
	s.speaking = speaking
	return nil
}

// startPlayback：终端开始播放回答。只听唤醒词（打断），记下播放前醒没醒，播完恢复。
func (s *transcriptionSession) startPlayback() {
	if !s.playback {
		s.awakeBeforePlayback = s.awake
		s.playbackLevel = levelMeter{}
	}
	s.playback = true
	s.current = nil
	if s.spotter != nil {
		s.spotter.Reset()
	}
	s.deps.Logger.Info("播放中：只听唤醒词（打断）")
}

// stopPlayback：终端播完了，恢复播放前的状态。被打断时已经醒了（wakeUp 里清掉了 playback），这里不用动。
func (s *transcriptionSession) stopPlayback() {
	if !s.playback {
		return
	}
	s.playback = false
	s.awake = s.awakeBeforePlayback
	s.echoUntil = s.totalSamples
	s.deps.Logger.Info("播放结束：恢复收听", "awake", s.awake)
}

// sleep 进入休眠：丢掉进行中的句子，重置唤醒词识别。
func (s *transcriptionSession) sleep() {
	s.awake = false
	s.current = nil
	s.spotter.Reset()
	s.deps.Logger.Info("进入休眠")
}

// waitLimit 是等用户接着说的上限：semantic_vad 按 eagerness；其他情况（刚唤醒）默认 2 秒。
func (s *transcriptionSession) waitLimit() int {
	if s.maxWaitSample > 0 {
		return s.maxWaitSample
	}
	return 2000 * samplesPerMs
}

func (s *transcriptionSession) beginUtterance(ctx context.Context, atSample int) error {
	s.itemCount++
	s.current = &utterance{itemID: fmt.Sprintf("item_%d", s.itemCount), startSample: atSample, endSample: atSample}
	return wsjson.Write(ctx, s.conn, realtimeproto.WireSpeechStarted{
		Type: "input_audio_buffer.speech_started", AudioStartMs: int64(atSample / samplesPerMs), ItemID: s.current.itemID})
}

// handleSegment 把一个 VAD 语音段并入当前这句话，再决定是否已经说完。
func (s *transcriptionSession) handleSegment(ctx context.Context, segment SpeechSegment) error {
	end := segment.StartSample + segment.SampleCount
	if segment.StartSample < s.echoUntil { // 开头是 haro 自己的回声（v3 E-1）：只取回声之后的部分
		if end-s.echoUntil < minAfterEchoMs*samplesPerMs {
			s.deps.Logger.Info("忽略：haro 自己的声音（播放时开始的语音段）", "start_ms", segment.StartSample/samplesPerMs,
				"end_ms", end/samplesPerMs, "echo_until_ms", s.echoUntil/samplesPerMs)
			return nil
		}
		s.deps.Logger.Info("语音段开头是 haro 的回声，剪掉", "start_ms", segment.StartSample/samplesPerMs,
			"echo_until_ms", s.echoUntil/samplesPerMs)
		segment.SampleCount = end - s.echoUntil
		segment.StartSample = s.echoUntil
	}
	s.deps.Logger.Info("VAD 语音段", "start_ms", segment.StartSample/samplesPerMs, "end_ms", end/samplesPerMs,
		"length_ms", segment.SampleCount/samplesPerMs, "after_wake_ms", s.sinceWakeMs(segment.StartSample),
		"in_item", s.current != nil, "wake_item", s.current != nil && s.current.wake)
	if s.current == nil { // 语音段太短，IsSpeech 没来得及变成 true
		if err := s.beginUtterance(ctx, segment.StartSample); err != nil {
			return err
		}
	}
	s.current.startSample = min(s.current.startSample, segment.StartSample)
	s.current.endSample = max(s.current.endSample, segment.StartSample+segment.SampleCount)
	if !s.semantic {
		return s.finishUtterance(ctx)
	}

	// semantic_vad：把这句话（含刚才的短暂静音）交给模型判断
	started := time.Now()
	audio := s.sliceHistory(s.paddedStart(s.current.startSample), s.totalSamples)
	probability, err := s.deps.TurnDetector.EndOfTurnProbability(pcm16ToFloat(audio))
	if err != nil { // 模型出错时退回「静音即说完」，不让对话卡住
		s.deps.Logger.Error("说完判断失败，按说完处理", "err", err)
		return s.finishUtterance(ctx)
	}
	complete := probability >= endOfTurnCutoff
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
	if err := wsjson.Write(ctx, s.conn, realtimeproto.WireSpeechStopped{
		Type: "input_audio_buffer.speech_stopped", AudioEndMs: int64(current.endSample / samplesPerMs), ItemID: current.itemID}); err != nil {
		return err
	}
	audio := realtimeproto.PCMAudio{
		Format: realtimeproto.AudioFormat{SampleRate: realtimeproto.TranscriptionSampleRate, Channels: 1},
		Data:   s.sliceHistory(s.paddedStart(current.startSample), current.endSample),
	}
	started := time.Now()
	transcript, err := s.deps.Transcriber.Transcribe(ctx, audio)
	if err != nil {
		return wsjson.Write(ctx, s.conn, realtimeproto.NewWireError("server_error", "识别失败: "+err.Error()))
	}
	s.deps.Logger.Info("一句话的范围", "item", current.itemID, "start_ms", current.startSample/samplesPerMs,
		"end_ms", current.endSample/samplesPerMs, "after_wake_ms", s.sinceWakeMs(current.startSample), "wake", current.wake)
	if current.wake {
		raw := transcript.Text
		transcript.Text = StripWakePhrase(raw)
		if current.bargeIn {
			transcript.Text = StripThroughWakePhrase(raw)
		}
		s.deps.Logger.Info("去掉唤醒词", "item", current.itemID, "raw", raw, "text", transcript.Text)
	}
	s.deps.Logger.Info("识别完成", "item", current.itemID, "audio", audio.Duration().Round(time.Millisecond),
		"elapsed", time.Since(started).Round(time.Millisecond), "text", transcript.Text)
	speaker, checked := s.identifySpeaker(current.itemID, audio.Data)
	switch {
	case current.wakeSpeaker != "": // 唤醒时已经认出来了（v3 V-a）：这句沿用，不让很短的唤醒句重新比对推翻它
		speaker, checked = current.wakeSpeaker, true
	case current.wakeGuess != "" && speaker == "" && transcript.Text == "":
		// 唤醒时只过了唤醒门槛，又只叫了唤醒词：放行，让 haro 回「我在」（不带信息、不做事；v3 BV）
		s.deps.Logger.Info("只叫了唤醒词：沿用唤醒时的声纹，回「我在」", "item", current.itemID, "speaker", current.wakeGuess)
		speaker, checked = current.wakeGuess, true
	}
	return wsjson.Write(ctx, s.conn, realtimeproto.NewWireTranscriptionCompleted(current.itemID, transcript.Text, speaker, checked))
}

// minAfterEchoMs：剪掉回声后剩下不到这么长，就当整段都是回声（v3 E-1）。
const minAfterEchoMs = 250

// minWakeWindowMs：算唤醒词声纹至少要这么长的声音（避开回声后太短就不避了）。
const minWakeWindowMs = 1000

// paddedStart 是识别一句话时音频的起点：往前多取 prefix_padding_ms，但不取进 haro 自己的回声（v3 E-1）。
func (s *transcriptionSession) paddedStart(startSample int) int {
	return max(startSample-s.turnDetection.PrefixPaddingMs*samplesPerMs, min(startSample, s.echoUntil))
}

// wakeWindowStart 是算唤醒词声纹的起点：唤醒词前 1.5 秒，刚播完时避开回声（剩下至少 1 秒）。
// 播放中被叫（打断）时回声避不开，只能整段算。
func (s *transcriptionSession) wakeWindowStart() int {
	start := s.totalSamples - wakeKeywordWindowMs*samplesPerMs
	if !s.playback && s.echoUntil > start {
		start = min(s.echoUntil, s.totalSamples-minWakeWindowMs*samplesPerMs)
	}
	return start
}

func (s *transcriptionSession) appendHistory(pcm []byte) {
	s.history = append(s.history, pcm...)
	if limit := historySeconds * realtimeproto.TranscriptionSampleRate * 2; len(s.history) > limit {
		drop := len(s.history) - limit
		drop -= drop % 2
		s.history = append([]byte(nil), s.history[drop:]...)
		s.historyStart += drop / 2
	}
}

// sliceHistory 取出 [fromSample, toSample) 的 pcm16，超出保留范围的部分自动截掉。
func (s *transcriptionSession) sliceHistory(fromSample, toSample int) []byte {
	from := max(fromSample, s.historyStart) - s.historyStart
	to := min(toSample-s.historyStart, len(s.history)/2)
	if to <= from {
		return nil
	}
	return append([]byte(nil), s.history[from*2:to*2]...)
}

func pcm16ToFloat(pcm []byte) []float32 {
	samples := make([]float32, len(pcm)/2)
	for i := range samples {
		samples[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return samples
}

// sinceWakeMs 返回 sample 距最近一次唤醒词的毫秒数（没听到过唤醒词时为 -1）。排查用日志。
func (s *transcriptionSession) sinceWakeMs(sample int) int {
	if s.lastWakeAt == 0 {
		return -1
	}
	return (sample - s.lastWakeAt) / samplesPerMs
}

// identifySpeaker 比对这句话的声纹（v3 H-3）；没登记声纹或算不出来时 checked = false（服务端不过滤）。
func (s *transcriptionSession) identifySpeaker(itemID string, pcm []byte) (string, bool) {
	if !s.deps.Speakers.enabled() {
		return "", false
	}
	started := time.Now()
	name, score, err := s.deps.Speakers.identify(pcm16ToFloat(pcm))
	if err != nil {
		s.deps.Logger.Warn("声纹算不出来，不过滤", "item", itemID, "err", err)
		return "", false
	}
	s.deps.Logger.Info("声纹", "item", itemID, "speaker", name, "score", fmt.Sprintf("%.2f", score),
		"elapsed", time.Since(started).Round(time.Millisecond))
	return name, true
}

// levelMeter 按每秒统计音量（排查用日志）：返回这一秒的均方根与峰值，满一秒时 ok = true。
type levelMeter struct {
	samples int
	sumSq   float64
	peak    float64
}

func (m *levelMeter) add(samples []float32) (rms, peak float64, ok bool) {
	for _, sample := range samples {
		value := math.Abs(float64(sample))
		m.sumSq += value * value
		m.peak = max(m.peak, value)
	}
	m.samples += len(samples)
	if m.samples < realtimeproto.TranscriptionSampleRate {
		return 0, 0, false
	}
	rms, peak = math.Sqrt(m.sumSq/float64(m.samples)), m.peak
	*m = levelMeter{}
	return rms, peak, true
}

package asrserver

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/voiceprint"
)

// energyDetector 是按幅度判断的替身 VAD：幅度 > 0.1 算语音，连续 silenceSamples 个静音采样点后结束一段。
type energyDetector struct {
	silenceSamples int
	position       int
	inSpeech       bool
	start, silent  int
	segments       []SpeechSegment
}

func (d *energyDetector) AcceptWaveform(samples []float32) {
	for _, sample := range samples {
		loud := sample > 0.1 || sample < -0.1
		switch {
		case loud && !d.inSpeech:
			d.inSpeech, d.start, d.silent = true, d.position, 0
		case loud:
			d.silent = 0
		case d.inSpeech:
			d.silent++
			if d.silent >= d.silenceSamples {
				end := d.position - d.silent + 1
				d.segments = append(d.segments, SpeechSegment{StartSample: d.start, SampleCount: end - d.start})
				d.inSpeech = false
			}
		}
		d.position++
	}
}
func (d *energyDetector) IsSpeech() bool { return d.inSpeech }
func (d *energyDetector) PopSegments() []SpeechSegment {
	segments := d.segments
	d.segments = nil
	return segments
}
func (d *energyDetector) Close() {}

// pcmChunk 生成 ms 毫秒的 16kHz pcm16；loud 为 true 时是方波，否则是静音。
func pcmChunk(ms int, loud bool) []byte {
	pcm := make([]byte, ms*16*2)
	if loud {
		for i := 0; i < len(pcm)/2; i++ {
			value := int16(8000)
			if i%20 < 10 {
				value = -8000
			}
			binary.LittleEndian.PutUint16(pcm[2*i:], uint16(value))
		}
	}
	return pcm
}

// lengthTranscriber 记录最后一次送来识别的音频时长（服务端 goroutine 写、测试读，用原子变量）。
type lengthTranscriber struct{ lastAudio atomic.Int64 }

func (l *lengthTranscriber) Transcribe(_ context.Context, audio realtimeproto.PCMAudio) (Transcript, error) {
	l.lastAudio.Store(int64(audio.Duration()))
	return Transcript{Text: "你好。"}, nil
}

func dialRealtime(t *testing.T, factory VoiceDetectorFactory, transcriber Transcriber, turnDetector TurnDetector) (*websocket.Conn, context.Context) {
	t.Helper()
	server := httptest.NewServer(NewHandler(Dependencies{Transcriber: transcriber, NewVoiceDetector: factory,
		TurnDetector: turnDetector, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, transcriptionURL(t, server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn, ctx
}

func TestRealtimeTranscriptionSession(t *testing.T) {
	factory := func(realtimeproto.TurnDetection) (VoiceDetector, error) {
		return &energyDetector{silenceSamples: 300 * 16}, nil // 300ms 静音算说完
	}
	transcriber := &lengthTranscriber{}
	conn, ctx := dialRealtime(t, factory, transcriber, nil)

	turn := realtimeproto.TurnDetection{Type: "server_vad", PrefixPaddingMs: 100, SilenceDurationMs: 300}
	if err := writeClientEvent(ctx, conn, transcriptionUpdate(turn, false)); err != nil {
		t.Fatal(err)
	}
	// 200ms 静音 → 500ms 说话 → 400ms 静音，每 20ms 一块
	for _, part := range []struct {
		ms   int
		loud bool
	}{{200, false}, {500, true}, {400, false}} {
		for sent := 0; sent < part.ms; sent += 20 {
			if err := writeClientEvent(ctx, conn, audioChunk(pcmChunk(20, part.loud))); err != nil {
				t.Fatal(err)
			}
		}
	}

	var events []realtimeproto.ServerEvent
	for len(events) < 3 {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("已收到 %v，读取出错 %v", events, err)
		}
		event, err := realtimeproto.DecodeServerEvent(data)
		if err != nil || event == nil {
			t.Fatalf("解析 %s: %v", data, err)
		}
		events = append(events, event)
	}
	started, ok1 := events[0].(realtimeproto.InputAudioBufferSpeechStarted)
	stopped, ok2 := events[1].(realtimeproto.InputAudioBufferSpeechStopped)
	completed, ok3 := events[2].(realtimeproto.InputAudioTranscriptionCompleted)
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("事件顺序 %v", events)
	}
	if started.ItemID != stopped.ItemID || stopped.ItemID != completed.ItemID || completed.Transcript != "你好。" {
		t.Errorf("事件 %+v %+v %+v", started, stopped, completed)
	}
	if stopped.AudioEndMs < 690 || stopped.AudioEndMs > 710 { // 语音在 700ms 处结束，不含等静音的 300ms
		t.Errorf("audio_end_ms = %d，应约为 700", stopped.AudioEndMs)
	}
	// 识别用的音频 = 500ms 语音 + 100ms 前置缓冲
	if got := time.Duration(transcriber.lastAudio.Load()); got < 590*time.Millisecond || got > 610*time.Millisecond {
		t.Errorf("送去识别的音频 %s，应约为 600ms", got)
	}
}

func TestRealtimeRejectsBadSessionUpdate(t *testing.T) {
	factory := func(realtimeproto.TurnDetection) (VoiceDetector, error) { return &energyDetector{}, nil }
	conn, ctx := dialRealtime(t, factory, &lengthTranscriber{}, nil)
	update := transcriptionUpdate(realtimeproto.TurnDetection{Type: "server_vad"}, false)
	update.InputFormat = realtimeproto.AudioFormat{SampleRate: 24000, Channels: 1}
	_ = writeClientEvent(ctx, conn, update)
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event, _ := realtimeproto.DecodeServerEvent(data)
	if e, ok := event.(realtimeproto.Error); !ok || e.Code != realtimeproto.CodeUnsupportedAudioFormat {
		t.Errorf("应返回采样率错误，得到 %s", data)
	}
}

func TestRealtimeWithoutVADModel(t *testing.T) {
	server := httptest.NewServer(NewHandler(Dependencies{Transcriber: &lengthTranscriber{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer server.Close()
	address, err := url.Parse(server.URL) // 普通 HTTP 请求（不升级成 WebSocket）：地址按结构拼
	if err != nil {
		t.Fatal(err)
	}
	address = address.JoinPath(realtimeproto.TranscriptionPath)
	address.RawQuery = url.Values{"intent": {realtimeproto.TranscriptionIntent}}.Encode()
	resp, err := http.Get(address.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("状态码 %d，应为 503", resp.StatusCode)
	}
}

// scriptedTurnDetector 依次返回预设的概率，并记录被调用的次数。
type scriptedTurnDetector struct {
	mu            sync.Mutex
	probabilities []float64
	calls         int
}

func (s *scriptedTurnDetector) EndOfTurnProbability([]float32) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	probability := s.probabilities[min(s.calls, len(s.probabilities)-1)]
	s.calls++
	return probability, nil
}

// semanticFactory 让替身 VAD 按 session 传来的静音时长切段（semantic_vad 时应为 300ms，v3 D-2）。
func semanticFactory(t *testing.T) VoiceDetectorFactory {
	return func(turn realtimeproto.TurnDetection) (VoiceDetector, error) {
		if turn.SilenceDurationMs != semanticSilenceMs {
			t.Errorf("semantic_vad 时 VAD 静音应为 %dms，实际 %d", semanticSilenceMs, turn.SilenceDurationMs)
		}
		return &energyDetector{silenceSamples: turn.SilenceDurationMs * 16}, nil
	}
}

func sendPattern(t *testing.T, ctx context.Context, conn *websocket.Conn, parts ...[2]int) {
	t.Helper()
	for _, part := range parts { // [毫秒, 是否说话(1/0)]
		for sent := 0; sent < part[0]; sent += 20 {
			if err := writeClientEvent(ctx, conn, audioChunk(pcmChunk(20, part[1] == 1))); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func readEvents(t *testing.T, ctx context.Context, conn *websocket.Conn, n int) []realtimeproto.ServerEvent {
	t.Helper()
	var events []realtimeproto.ServerEvent
	for len(events) < n {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("已收到 %v，读取出错 %v", events, err)
		}
		if event, _ := realtimeproto.DecodeServerEvent(data); event != nil {
			events = append(events, event)
		}
	}
	return events
}

func TestSemanticVADMergesPauseIntoOneUtterance(t *testing.T) {
	turnDetector := &scriptedTurnDetector{probabilities: []float64{0.2, 0.9}} // 第一次停顿：没说完；第二次：说完
	transcriber := &lengthTranscriber{}
	conn, ctx := dialRealtime(t, semanticFactory(t), transcriber, turnDetector)
	turn := realtimeproto.TurnDetection{Type: "semantic_vad", Eagerness: "auto"}
	_ = writeClientEvent(ctx, conn, transcriptionUpdate(turn, false))

	// 0.2s 静音 → 0.5s 说话 → 0.4s 停顿 → 0.4s 说话 → 0.4s 静音
	sendPattern(t, ctx, conn, [2]int{200, 0}, [2]int{500, 1}, [2]int{400, 0}, [2]int{400, 1}, [2]int{400, 0})
	events := readEvents(t, ctx, conn, 3)
	started, ok1 := events[0].(realtimeproto.InputAudioBufferSpeechStarted)
	stopped, ok2 := events[1].(realtimeproto.InputAudioBufferSpeechStopped)
	_, ok3 := events[2].(realtimeproto.InputAudioTranscriptionCompleted)
	if !ok1 || !ok2 || !ok3 || started.ItemID != stopped.ItemID {
		t.Fatalf("应只有一句话：%v", events)
	}
	if stopped.AudioEndMs < 1490 || stopped.AudioEndMs > 1510 {
		t.Errorf("audio_end_ms = %d，应约为 1500（第二段说话结束处）", stopped.AudioEndMs)
	}
	// 识别的是整句：从第一段开始到第二段结束（0.2s → 1.5s = 1.3s）
	if got := time.Duration(transcriber.lastAudio.Load()); got < 1290*time.Millisecond || got > 1310*time.Millisecond {
		t.Errorf("送去识别的音频 %s，应约为 1.3s（停顿前后合成一句）", got)
	}
	turnDetector.mu.Lock()
	calls := turnDetector.calls
	turnDetector.mu.Unlock()
	if calls != 2 {
		t.Errorf("说完判断应调用 2 次，实际 %d", calls)
	}
}

func TestSemanticVADTimesOutWhenUserStopsTalking(t *testing.T) {
	turnDetector := &scriptedTurnDetector{probabilities: []float64{0.1}} // 一直判断「没说完」
	conn, ctx := dialRealtime(t, semanticFactory(t), &lengthTranscriber{}, turnDetector)
	turn := realtimeproto.TurnDetection{Type: "semantic_vad", Eagerness: "high"} // 最多再等 1s
	_ = writeClientEvent(ctx, conn, transcriptionUpdate(turn, false))

	sendPattern(t, ctx, conn, [2]int{200, 0}, [2]int{500, 1}, [2]int{600, 0}) // 停顿 0.6s：还没到 1s
	sendPattern(t, ctx, conn, [2]int{600, 0})                                 // 再静 0.6s：超时
	events := readEvents(t, ctx, conn, 3)
	if _, ok := events[1].(realtimeproto.InputAudioBufferSpeechStopped); !ok {
		t.Fatalf("超时后应按说完处理：%v", events)
	}
}

func TestSemanticVADRequiresTurnModel(t *testing.T) {
	conn, ctx := dialRealtime(t, semanticFactory(t), &lengthTranscriber{}, nil)
	_ = writeClientEvent(ctx, conn, transcriptionUpdate(realtimeproto.TurnDetection{Type: "semantic_vad"}, false))
	events := readEvents(t, ctx, conn, 1)
	if e, ok := events[0].(realtimeproto.Error); !ok || e.Code != realtimeproto.CodeUnsupportedFeature {
		t.Errorf("没有 Smart Turn 模型时应拒绝 semantic_vad：%v", events)
	}
}

// fakeSpotter：连续收到 10 块（200ms）「唤醒词音频」（幅度约 ±16000）后报告唤醒词。
type fakeSpotter struct {
	run    int
	resets atomic.Int32
}

func (f *fakeSpotter) AcceptWaveform(samples []float32) string {
	if len(samples) > 0 && (samples[0] > 0.4 || samples[0] < -0.4) {
		f.run++
		if f.run == 10 {
			f.run = 0
			return "哈罗哈罗"
		}
		return ""
	}
	f.run = 0
	return ""
}
func (f *fakeSpotter) Reset() { f.run = 0; f.resets.Add(1) }
func (f *fakeSpotter) Close() {}

// keywordChunk 是 20ms 的「唤醒词音频」：幅度 ±16000，替身 VAD 也会把它当成说话。
func keywordChunk() []byte {
	pcm := make([]byte, 640)
	for i := 0; i < 320; i++ {
		value := int16(16000)
		if i%20 >= 10 {
			value = -16000
		}
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(value))
	}
	return pcm
}

func dialWakeSession(t *testing.T, transcriber Transcriber, spotter *fakeSpotter) (*websocket.Conn, context.Context) {
	t.Helper()
	return dialWakeSessionWith(t, transcriber, spotter, nil)
}

func dialWakeSessionWith(t *testing.T, transcriber Transcriber, spotter *fakeSpotter, speakers *SpeakerCheck) (*websocket.Conn, context.Context) {
	t.Helper()
	server := httptest.NewServer(NewHandler(Dependencies{Transcriber: transcriber, Speakers: speakers,
		NewVoiceDetector: func(realtimeproto.TurnDetection) (VoiceDetector, error) {
			return &energyDetector{silenceSamples: 300 * 16}, nil
		},
		NewKeywordSpotter: func() (KeywordSpotter, error) { return spotter, nil },
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil))}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, transcriptionURL(t, server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	turn := realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 300}
	if err := writeClientEvent(ctx, conn, transcriptionUpdate(turn, true)); err != nil {
		t.Fatal(err)
	}
	return conn, ctx
}

func sendKeyword(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := writeClientEvent(ctx, conn, audioChunk(keywordChunk())); err != nil {
			t.Fatal(err)
		}
	}
}

// fixedTranscriber 返回固定文字，并记录送来的音频时长。
type fixedTranscriber struct {
	text      string
	lastAudio atomic.Int64
}

func (f *fixedTranscriber) Transcribe(_ context.Context, audio realtimeproto.PCMAudio) (Transcript, error) {
	f.lastAudio.Store(int64(audio.Duration()))
	return Transcript{Text: f.text}, nil
}

func TestWakeGateIgnoresSpeechUntilKeyword(t *testing.T) {
	transcriber := &fixedTranscriber{text: "哈罗哈罗，我旁边有什么？"}
	spotter := &fakeSpotter{}
	conn, ctx := dialWakeSession(t, transcriber, spotter)

	// 休眠时说一句话：不应有任何事件
	sendPattern(t, ctx, conn, [2]int{200, 0}, [2]int{500, 1}, [2]int{400, 0})
	// 一口气说完：唤醒词（0.2s）紧接着说话（0.5s），然后静音
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})

	events := readEvents(t, ctx, conn, 4)
	wake, ok := events[0].(realtimeproto.HaroWakeWordDetected)
	if !ok || wake.Keyword != "哈罗哈罗" {
		t.Fatalf("第一个事件应是唤醒（休眠时那句话不应上报）：%v", events)
	}
	if _, ok := events[1].(realtimeproto.InputAudioBufferSpeechStarted); !ok {
		t.Fatalf("事件顺序 %v", events)
	}
	if _, ok := events[2].(realtimeproto.InputAudioBufferSpeechStopped); !ok {
		t.Fatalf("事件顺序 %v", events)
	}
	completed, ok := events[3].(realtimeproto.InputAudioTranscriptionCompleted)
	if !ok || completed.Transcript != "我旁边有什么？" {
		t.Fatalf("应整句识别后去掉唤醒词：%v", events)
	}
	// 识别的音频包含唤醒词（0.2s）和后面的话（0.5s），不包含休眠时那句话
	if got := time.Duration(transcriber.lastAudio.Load()); got < 690*time.Millisecond || got > 1600*time.Millisecond {
		t.Errorf("送去识别的音频 %s，应包含唤醒词与后面的话", got)
	}

	// 休眠后再说话：不应上报；再叫唤醒词：重新唤醒
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.wake_word.sleep"})
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{400, 0})
	events = readEvents(t, ctx, conn, 1)
	if _, ok := events[0].(realtimeproto.HaroWakeWordDetected); !ok {
		t.Fatalf("休眠后第一个事件应是再次唤醒：%v", events)
	}
	if spotter.resets.Load() == 0 {
		t.Error("进入休眠时应重置唤醒词识别")
	}
}

func TestWakeWordOnlyProducesEmptyUtterance(t *testing.T) {
	transcriber := &fixedTranscriber{text: "哈罗哈罗。"}
	conn, ctx := dialWakeSession(t, transcriber, &fakeSpotter{})
	sendPattern(t, ctx, conn, [2]int{200, 0})
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{500, 0}) // 叫完唤醒词就不说了
	events := readEvents(t, ctx, conn, 4)
	if completed, ok := events[3].(realtimeproto.InputAudioTranscriptionCompleted); !ok || completed.Transcript != "" {
		t.Fatalf("只叫了唤醒词时识别结果应为空：%v", events)
	}
}

func TestWakeWordRequiresSpotter(t *testing.T) {
	conn, ctx := dialRealtime(t, func(realtimeproto.TurnDetection) (VoiceDetector, error) { return &energyDetector{}, nil },
		&lengthTranscriber{}, nil)
	_ = writeClientEvent(ctx, conn, transcriptionUpdate(realtimeproto.TurnDetection{Type: "server_vad"}, true))
	events := readEvents(t, ctx, conn, 1)
	if e, ok := events[0].(realtimeproto.Error); !ok || e.Code != realtimeproto.CodeUnsupportedFeature {
		t.Errorf("没有唤醒词模型时应拒绝：%v", events)
	}
}

// 打断（v3 I-1）：播放回答期间只听唤醒词——回声（说话声）不上报；听到唤醒词 ⇒ 唤醒，这句话标为打断，
// 识别后去掉唤醒词连同前面的回声；播完（没被打断）⇒ 恢复播放前的状态。
func TestBargeInDuringPlayback(t *testing.T) {
	transcriber := &fixedTranscriber{text: "我觉得你应该早点睡哈罗哈罗，换个话题吧。"}
	spotter := &fakeSpotter{}
	conn, ctx := dialWakeSession(t, transcriber, spotter)

	// 先唤醒、说一句，进入对话窗口
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
	readEvents(t, ctx, conn, 4)

	// 播放回答：haro 自己的回声（说话声）不应上报
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.playback.started"})
	sendPattern(t, ctx, conn, [2]int{800, 1}, [2]int{400, 0})
	// 播放中被叫：唤醒 + 打断句
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
	events := readEvents(t, ctx, conn, 4)
	if _, ok := events[0].(realtimeproto.HaroWakeWordDetected); !ok {
		t.Fatalf("播放时的回声不应上报，第一个事件应是唤醒：%v", events)
	}
	completed, ok := events[3].(realtimeproto.InputAudioTranscriptionCompleted)
	if !ok || completed.Transcript != "换个话题吧。" {
		t.Fatalf("打断句应去掉唤醒词连同前面的回声：%v", events)
	}

	// 再播一次、正常播完：恢复收听（播放前是醒着的 ⇒ 播完说话照常上报）
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.playback.started"})
	sendPattern(t, ctx, conn, [2]int{800, 1}, [2]int{400, 0})
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.playback.stopped"})
	transcriber.text = "好的"
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
	events = readEvents(t, ctx, conn, 3)
	if _, ok := events[0].(realtimeproto.InputAudioBufferSpeechStarted); !ok {
		t.Fatalf("播完后应恢复收听：%v", events)
	}
}

// 回声（v3 E-1）：播放时开始、播完才结束的语音段是 haro 自己的声音——剩一点尾巴就整段丢掉；
// 用户紧接着开口（和回声连成一段）时，只取播完之后的部分。
func TestEchoTailAfterPlaybackIsClipped(t *testing.T) {
	transcriber := &fixedTranscriber{text: "好的"}
	conn, ctx := dialWakeSession(t, transcriber, &fakeSpotter{})
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
	readEvents(t, ctx, conn, 4)

	// ① 播完时回声还没断（尾巴 100 ms）：整段丢掉；之后用户说的话单独成句
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.playback.started"})
	sendPattern(t, ctx, conn, [2]int{800, 1})
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.playback.stopped"})
	sendPattern(t, ctx, conn, [2]int{100, 1}, [2]int{400, 0}, [2]int{500, 1}, [2]int{400, 0})
	events := readEvents(t, ctx, conn, 3)
	if _, ok := events[0].(realtimeproto.InputAudioBufferSpeechStarted); !ok {
		t.Fatalf("回声尾巴不应成句：%v", events)
	}
	if got := time.Duration(transcriber.lastAudio.Load()); got > 600*time.Millisecond {
		t.Errorf("识别的应只是用户那 500 ms，实际 %v", got)
	}

	// ② 用户在回声结束前后紧接着开口（连成一段）：只取播完之后的 600 ms
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.playback.started"})
	sendPattern(t, ctx, conn, [2]int{800, 1})
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.playback.stopped"})
	sendPattern(t, ctx, conn, [2]int{600, 1}, [2]int{400, 0})
	events = readEvents(t, ctx, conn, 3)
	if _, ok := events[2].(realtimeproto.InputAudioTranscriptionCompleted); !ok {
		t.Fatalf("紧接着说的话应成句：%v", events)
	}
	if got := time.Duration(transcriber.lastAudio.Load()); got < 500*time.Millisecond || got > 700*time.Millisecond {
		t.Errorf("应只识别播完之后的约 600 ms，实际 %v", got)
	}
}

func TestStripThroughWakePhrase(t *testing.T) {
	cases := map[string]string{
		"我觉得你应该早点睡哈罗哈罗，换个话题吧。":          "换个话题吧。",
		"今天天气不错，适合出门走走 hello hello 停一下": "停一下",
		"哈罗哈罗。":   "",
		"没有唤醒词的话": "没有唤醒词的话",
	}
	for in, want := range cases {
		if got := StripThroughWakePhrase(in); got != want {
			t.Errorf("StripThroughWakePhrase(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// switchableEmbedder 返回测试指定的「当前说话人」的向量。
type switchableEmbedder struct{ current atomic.Value }

func (e *switchableEmbedder) Embed([]float32) ([]float32, error) {
	return e.current.Load().([]float32), nil
}

// 声纹（v3 H-3）：陌生人喊唤醒词叫不醒；登记过的人叫得醒，每句话标上是谁说的；醒着时陌生人的话标为没认出来。
func TestVoiceprintGatesWakeAndLabelsSpeaker(t *testing.T) {
	owner, stranger := []float32{1, 0, 0}, []float32{0, 1, 0}
	embedder := &switchableEmbedder{}
	speakers := &SpeakerCheck{Embedder: embedder, Threshold: 0.5,
		Voiceprints: []voiceprint.Voiceprint{{Name: "主人", Embeddings: [][]float32{owner}}}}
	transcriber := &fixedTranscriber{text: "哈罗哈罗，几点了？"}
	conn, ctx := dialWakeSessionWith(t, transcriber, &fakeSpotter{}, speakers)

	embedder.current.Store(stranger) // 陌生人喊唤醒词：不醒，也不上报他接着说的话
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
	time.Sleep(300 * time.Millisecond) // 等服务端处理完陌生人这段（没有事件可等），再换成主人

	embedder.current.Store(owner) // 主人喊：醒，识别结果标「主人」
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
	events := readEvents(t, ctx, conn, 4)
	if _, ok := events[0].(realtimeproto.HaroWakeWordDetected); !ok {
		t.Fatalf("陌生人叫不醒，第一个事件应是主人的唤醒：%v", events)
	}
	completed := events[3].(realtimeproto.InputAudioTranscriptionCompleted)
	if completed.Speaker != "主人" || !completed.SpeakerChecked {
		t.Errorf("应标上主人：%+v", completed)
	}

	embedder.current.Store(stranger) // 醒着时陌生人说话：照常识别，但标为没认出来（由服务端决定不理）
	transcriber.text = "你好啊"
	sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
	events = readEvents(t, ctx, conn, 3)
	completed = events[2].(realtimeproto.InputAudioTranscriptionCompleted)
	if completed.Speaker != "" || !completed.SpeakerChecked {
		t.Errorf("陌生人应标为没认出来：%+v", completed)
	}
}

// V-a：唤醒时认出来的人，这一句沿用——不让很短的唤醒句重新比对推翻它（真机：唤醒 0.52 认出、唤醒句重比 0.49 判成不认识）。
func TestWakeUtteranceKeepsWakeSpeaker(t *testing.T) {
	owner, stranger := []float32{1, 0, 0}, []float32{0, 1, 0}
	embedder := &switchableEmbedder{}
	speakers := &SpeakerCheck{Embedder: embedder, Threshold: 0.5,
		Voiceprints: []voiceprint.Voiceprint{{Name: "主人", Embeddings: [][]float32{owner}}}}
	conn, ctx := dialWakeSessionWith(t, &fixedTranscriber{text: "哈罗哈罗"}, &fakeSpotter{}, speakers)

	embedder.current.Store(owner)
	sendKeyword(t, ctx, conn)
	if wake, ok := readEvents(t, ctx, conn, 1)[0].(realtimeproto.HaroWakeWordDetected); !ok {
		t.Fatalf("主人应能唤醒：%v", wake)
	}
	embedder.current.Store(stranger) // 唤醒句结束时再比对会得到「不认识」
	sendPattern(t, ctx, conn, [2]int{1500, 0})
	events := readEvents(t, ctx, conn, 3)
	completed, ok := events[2].(realtimeproto.InputAudioTranscriptionCompleted)
	if !ok || completed.Speaker != "主人" || !completed.SpeakerChecked {
		t.Errorf("唤醒句应沿用唤醒时的声纹：%v", events)
	}
}

// BV：唤醒词很短、声纹算不稳，唤醒用低一些的门槛；只过了唤醒门槛的，接着说的话还要再比对。
func TestWeakWakeRechecksTheSentence(t *testing.T) {
	owner, weak := []float32{1, 0, 0}, []float32{0.35, 0.94, 0} // weak 和主人的相似度约 0.35：过唤醒门槛 0.2、不到 0.5
	embedder := &switchableEmbedder{}
	embedder.current.Store(weak)
	speakers := &SpeakerCheck{Embedder: embedder, Threshold: 0.5, WakeThreshold: 0.2,
		Voiceprints: []voiceprint.Voiceprint{{Name: "主人", Embeddings: [][]float32{owner}}}}

	cases := []struct {
		name, text, wantSpeaker string
	}{
		{"一口气说完：整句再比对，不到 0.5 不认", "哈罗哈罗，帮我删掉所有记事", ""},
		{"只叫了唤醒词：沿用唤醒时的人，回「我在」", "哈罗哈罗", "主人"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, ctx := dialWakeSessionWith(t, &fixedTranscriber{text: c.text}, &fakeSpotter{}, speakers)
			sendKeyword(t, ctx, conn)
			sendPattern(t, ctx, conn, [2]int{500, 1}, [2]int{400, 0})
			events := readEvents(t, ctx, conn, 4)
			wake, ok := events[0].(realtimeproto.HaroWakeWordDetected)
			if !ok {
				t.Fatalf("过了唤醒门槛应能唤醒：%v", events)
			}
			if wake.Score < 0.3 || wake.Score > 0.4 || wake.Level <= 0 { // v4 D2：唤醒质量带给服务端做仲裁
				t.Errorf("唤醒事件应带上声纹分数和音量：%+v", wake)
			}
			completed, ok := events[3].(realtimeproto.InputAudioTranscriptionCompleted)
			if !ok || completed.Speaker != c.wantSpeaker || !completed.SpeakerChecked {
				t.Errorf("说话人应为 %q：%v", c.wantSpeaker, events)
			}
		})
	}
}

func TestWakeThresholdFallsBackToThreshold(t *testing.T) {
	for _, c := range []struct{ wake, want float64 }{{0, 0.5}, {0.2, 0.2}, {0.7, 0.5}} {
		if got := (&SpeakerCheck{Threshold: 0.5, WakeThreshold: c.wake}).wakeThreshold(); got != c.want {
			t.Errorf("WakeThreshold %.1f → %.1f，应为 %.1f", c.wake, got, c.want)
		}
	}
}

// transcriptionUpdate 是转写会话的第一条消息。
func transcriptionUpdate(turn realtimeproto.TurnDetection, wakeWord bool) realtimeproto.SessionUpdate {
	return realtimeproto.SessionUpdate{Kind: realtimeproto.SessionTranscription, TurnDetection: &turn, WakeWord: wakeWord,
		InputFormat: realtimeproto.TranscriptionFormat}
}

// audioChunk 是一块 16 kHz 的音频。
func audioChunk(pcm []byte) realtimeproto.InputAudioBufferAppend {
	return realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: realtimeproto.TranscriptionFormat, Data: pcm}}
}

// writeClientEvent 把一个客户端事件编码后发出去。
func writeClientEvent(ctx context.Context, conn *websocket.Conn, event realtimeproto.ClientEvent) error {
	data, err := realtimeproto.EncodeClientEvent(event)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// transcriptionURL 是测试用的转写服务（httptest）上转写会话的地址。
func transcriptionURL(t *testing.T, base string) string {
	t.Helper()
	address, err := realtimeproto.TranscriptionURL(base)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

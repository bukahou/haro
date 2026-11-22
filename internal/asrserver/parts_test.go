package asrserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// pcmOf 是 n 个采样点、值都是 value 的 pcm16。
func pcmOf(n int, value int16) []byte {
	pcm := make([]byte, 2*n)
	for i := range n {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(value))
	}
	return pcm
}

// countingPCM 是从第 from 个采样点起的 n 个采样点，每个的值是它在时间轴上的位置（对 30000 取余），用来核对取出来的是哪一段。
func countingPCM(from, n int) []byte {
	pcm := make([]byte, 2*n)
	for i := range n {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16((from+i)%30000)))
	}
	return pcm
}

// 时间轴：累计采样点一直往前走；至少保留最近 keep 个采样点（攒到 1.5 倍才丢一次），取出来的位置和内容对得上，
// 取超出范围的部分时自动截掉，并能说出开头丢了多少。
func TestAudioTimelineKeepsRecentHistory(t *testing.T) {
	const keep, chunk = 10000, 320
	timeline := audioTimeline{keep: keep}
	samples := timeline.append(pcmOf(chunk, 16384))
	if timeline.now() != chunk || len(samples) != chunk || samples[0] != 0.5 {
		t.Fatalf("第一块之后：now=%d，采样 %d 个、第一个 %v", timeline.now(), len(samples), samples[0])
	}
	for timeline.now() < 5*keep {
		timeline.append(countingPCM(timeline.now(), chunk))
		if kept := len(timeline.pcm) / 2; kept < min(keep, timeline.now()) || kept > keep+keep/2 {
			t.Fatalf("now=%d：保留了 %d 个采样点，应在 %d 到 %d 之间", timeline.now(), kept, keep, keep+keep/2)
		}
	}
	now := timeline.now()
	if got := timeline.slice(now-keep, now); !bytes.Equal(got, countingPCM(now-keep, keep)) {
		t.Error("最近 keep 个采样点应原样取得出来（位置和内容都对）")
	}
	if got := len(timeline.slice(0, now)) / 2; got != now-timeline.start || timeline.lostFrom(0) != timeline.start {
		t.Errorf("从头取：只取得到保留的 %d 个，开头丢了 %d 个（lostFrom %d）", got, timeline.start, timeline.lostFrom(0))
	}
	if timeline.lostFrom(now-keep) != 0 {
		t.Errorf("保留范围以内没有丢：%d", timeline.lostFrom(now-keep))
	}
	if got := len(timeline.slice(now-100, now+100)) / 2; got != 100 {
		t.Errorf("取到「现在」为止：%d 个采样点", got)
	}
	if got := timeline.slice(now, now-1); got != nil {
		t.Errorf("范围是空的时候取不到：%d 字节", len(got))
	}
}

// 回声窗口：播完时回声到播放结束；语音段开头的回声剪掉，剩下太短就整段当回声；识别、算声纹都不取进回声。
func TestEchoWindow(t *testing.T) {
	echo := newEchoWindow(DefaultTiming)
	minAfterEchoMs, minWakeWindowMs := int(DefaultTiming.MinAfterEcho.Milliseconds()), int(DefaultTiming.MinWakeWindow.Milliseconds())
	wakeKeywordWindowMs := int(DefaultTiming.WakeKeywordWindow.Milliseconds())
	if !echo.start() || echo.start() {
		t.Fatal("第一次开始播放算刚开始，之后再说开始不算")
	}
	if !echo.stop(10000) || echo.stop(20000) || echo.until != 10000 {
		t.Fatalf("播完：回声到播放结束的位置，只算一次：until=%d", echo.until)
	}
	ms := samplesPerMs
	segment := SpeechSegment{StartSample: 9000, SampleCount: 1000 + 500*ms}
	if trimmed, cut, ignore := echo.trim(segment); !cut || ignore || trimmed.StartSample != 10000 || trimmed.SampleCount != 500*ms {
		t.Errorf("开头是回声：剪到回声之后：%+v cut=%v ignore=%v", trimmed, cut, ignore)
	}
	short := SpeechSegment{StartSample: 9000, SampleCount: 1000 + (minAfterEchoMs-1)*ms}
	if _, _, ignore := echo.trim(short); !ignore {
		t.Error("剪掉回声后不到 minAfterEchoMs：整段当回声")
	}
	after := SpeechSegment{StartSample: 10000, SampleCount: 100}
	if trimmed, cut, ignore := echo.trim(after); cut || ignore || trimmed != after {
		t.Errorf("回声之后开始的不动：%+v", trimmed)
	}
	if got := echo.paddedStart(10100, 300); got != 10000 {
		t.Errorf("往前多取一段，但不取进回声：%d", got)
	}
	if got := echo.paddedStart(50000, 300); got != 50000-300*ms {
		t.Errorf("离回声远时照常往前多取：%d", got)
	}
	if now := 10000 + 1200*ms; echo.wakeWindowStart(now) != 10000 {
		t.Errorf("刚播完时算声纹避开回声：%d", echo.wakeWindowStart(now))
	}
	if now := 10000 + 500*ms; echo.wakeWindowStart(now) != now-minWakeWindowMs*ms {
		t.Errorf("避开回声后不到 minWakeWindowMs 时不避（至少留这么长）：%d", echo.wakeWindowStart(now))
	}
	if now := 10000 + 5000*ms; echo.wakeWindowStart(now) != now-wakeKeywordWindowMs*ms {
		t.Errorf("离回声远时照常取唤醒词前 wakeKeywordWindowMs：%d", echo.wakeWindowStart(now))
	}
	echo.start()
	echo.interrupt()
	if echo.playing {
		t.Error("打断后不再算在播放")
	}
	echo.cutAt(42)
	if echo.until != 42 {
		t.Errorf("打断时回声到打断句的开头：%d", echo.until)
	}
}

type countingSpotter struct{ resets int }

func (s *countingSpotter) AcceptWaveform([]float32) string { return "" }
func (s *countingSpotter) Reset()                          { s.resets++ }
func (s *countingSpotter) Close()                          {}

// 唤醒门：没启用时一直在听；启用时醒着、且终端没在播放才听。播完恢复播放前醒没醒；
// 播放中收到休眠 / 手动唤醒时，播完按收到的来（不被播放前的状态盖回去）。
func TestWakeGate(t *testing.T) {
	var off wakeGate
	if !off.listening(false) || off.listening(true) || off.spot(nil) != "" {
		t.Fatal("没启用唤醒门：不播放时一直在听，不识别唤醒词")
	}
	spotter := &countingSpotter{}
	gate := wakeGate{enabled: true, spotter: spotter}
	if gate.listening(false) {
		t.Error("启用后从休眠开始：不听")
	}
	gate.woke(16000)
	if !gate.listening(false) || gate.listening(true) || gate.sinceWakeMs(32000) != 1000 {
		t.Errorf("醒了：不播放时听，播放时不听；距唤醒 %d 毫秒", gate.sinceWakeMs(32000))
	}
	gate.pauseForPlayback(true)
	gate.pauseForPlayback(false) // 播放中又说一次开始：不改记下的状态
	gate.resumeAfterPlayback()
	if !gate.awake {
		t.Error("播完恢复播放前（醒着）")
	}
	gate.pauseForPlayback(true)
	gate.sleep() // 播放中收到休眠
	gate.resumeAfterPlayback()
	if gate.awake {
		t.Error("播放中收到休眠：播完不能又恢复成醒着")
	}
	gate.pauseForPlayback(true) // 睡着时开始播放，播放中手动唤醒
	gate.wakeManually()
	gate.resumeAfterPlayback()
	if !gate.awake {
		t.Error("播放中手动唤醒：播完保持醒着")
	}
	if spotter.resets != 5 {
		t.Errorf("每次开始播放、休眠都重置唤醒词识别：重置了 %d 次，应为 5 次", spotter.resets)
	}
}

type fixedTurnDetector struct {
	probability float64
	err         error
}

func (d fixedTurnDetector) EndOfTurnProbability([]float32) (float64, error) {
	return d.probability, d.err
}

// 说完判断：概率到 endOfTurnCutoff 算说完；等接着说的上限 semantic_vad 按 eagerness，其他情况 2 秒。
func TestTurnEnd(t *testing.T) {
	if got := (turnEnd{defaultWait: samplesIn(DefaultTiming.DefaultWait)}).waitLimit(); got != 2000*samplesPerMs {
		t.Errorf("默认等 2 秒：%d", got)
	}
	if got := (turnEnd{semantic: true, maxWait: 123}).waitLimit(); got != 123 {
		t.Errorf("semantic_vad 按 eagerness：%d", got)
	}
	cutoff := DefaultTiming.EndOfTurnCutoff
	for probability, want := range map[float64]bool{cutoff: true, cutoff - 0.01: false} {
		complete, got, err := turnEnd{semantic: true, cutoff: cutoff, detector: fixedTurnDetector{probability: probability}}.complete(pcmOf(10, 0))
		if err != nil || complete != want || got != probability {
			t.Errorf("概率 %.2f：说完应为 %v，实际 %v（%v）", probability, want, complete, err)
		}
	}
	if _, _, err := (turnEnd{semantic: true, detector: fixedTurnDetector{err: errors.New("模型出错")}}).complete(nil); err == nil {
		t.Error("模型出错要交出错误（调用方按说完处理）")
	}
}

// 时长与门槛：没注入时用 DefaultTiming；eagerness 对到三档（medium、auto、空都按 auto）。
func TestTimingDefaultsAndEagerness(t *testing.T) {
	if (Timing{}).orDefault() != DefaultTiming {
		t.Error("零值应换成 DefaultTiming")
	}
	custom := DefaultTiming
	custom.WakeOnlyWait = 500 * time.Millisecond
	if custom.orDefault() != custom {
		t.Error("注入的值照用")
	}
	for eagerness, want := range map[string]time.Duration{"low": 3 * time.Second, "high": time.Second, "medium": 2 * time.Second,
		"auto": 2 * time.Second, "": 2 * time.Second} {
		if got := DefaultTiming.maxWait(eagerness); got != want {
			t.Errorf("eagerness %q：%v，应为 %v", eagerness, got, want)
		}
	}
}

// 保留的音频要装得下可能合并出的最长句，不然长句的开头会被截掉。
func TestDefaultHistoryCoversLongestUtterance(t *testing.T) {
	if longest := DefaultTiming.longestUtterance(); DefaultTiming.History < longest {
		t.Errorf("History %v 装不下可能合并出的最长句 %v", DefaultTiming.History, longest)
	}
}

// VAD 的参数：门槛、静音时长按会话（没给时用 Timing 的默认值），最短 / 最长语音段按 Timing。
func TestVoiceDetectorConfig(t *testing.T) {
	timing := DefaultTiming
	timing.VADMinSpeech, timing.VADMaxSpeech = 400*time.Millisecond, 12*time.Second
	given := timing.voiceDetectorConfig(realtimeproto.TurnDetection{Threshold: 0.6, SilenceDurationMs: 500})
	if want := (VoiceDetectorConfig{Threshold: 0.6, MinSilence: 500 * time.Millisecond, MinSpeech: 400 * time.Millisecond,
		MaxSpeech: 12 * time.Second}); given != want {
		t.Errorf("会话给了门槛、静音时长就用会话的：%+v，应为 %+v", given, want)
	}
	fallback := timing.voiceDetectorConfig(realtimeproto.TurnDetection{})
	if fallback.Threshold != timing.VADThreshold || fallback.MinSilence != timing.VADSilence {
		t.Errorf("会话没给时用 Timing 的默认值：%+v", fallback)
	}
}

// recordingHandler 记下所有日志（测试按级别和属性名核对，不看日志的文字）。
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// attr 是 level 级别、带 key 属性的第一条日志里这个属性的值（没有时 ok = false）。
func (h *recordingHandler) attr(level slog.Level, key string) (value slog.Value, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, record := range h.records {
		if record.Level != level {
			continue
		}
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == key {
				value, ok = attr.Value, true
			}
			return !ok
		})
		if ok {
			return value, true
		}
	}
	return slog.Value{}, false
}

// 一句话比保留的音频还长（History 设得比它短）：开头丢了要记一条警告、说出丢了多少，剩下的照样识别。
func TestTruncatedUtteranceIsLogged(t *testing.T) {
	timing := DefaultTiming
	timing.History = 500 * time.Millisecond
	logs := &recordingHandler{}
	transcriber := &lengthTranscriber{}
	factory := func(VoiceDetectorConfig) (VoiceDetector, error) {
		return &energyDetector{silenceSamples: 300 * 16}, nil
	}
	conn, ctx := dialRealtimeWith(t, Dependencies{Transcriber: transcriber, Timing: timing, Logger: slog.New(logs),
		NewVoiceDetector: factory})
	startSession(t, ctx, conn, realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 300}, false)
	sendPattern(t, ctx, conn, [2]int{200, 0}, [2]int{1500, 1}, [2]int{400, 0}) // 一句 1.5 s，只保留 0.5 s（最多 0.75 s）
	if _, ok := readEvents(t, ctx, conn, 3)[2].(realtimeproto.InputAudioTranscriptionCompleted); !ok {
		t.Fatal("剩下的部分照样识别")
	}
	if lost, ok := logs.attr(slog.LevelWarn, "lost_ms"); !ok || lost.Int64() < 500 {
		t.Errorf("开头丢了应记警告并说出丢了多少：%v %v", lost, ok)
	}
	if got := time.Duration(transcriber.lastAudio.Load()); got >= 1500*time.Millisecond {
		t.Errorf("保留不下的开头送不去识别：%v", got)
	}
}

// semantic_vad 并到 MergeLimit 就不再等接着说：这一句马上交出去，不再问说完判断模型。
func TestSemanticVADStopsMergingAtLimit(t *testing.T) {
	timing := DefaultTiming
	timing.MergeLimit = time.Second
	turnDetector := &scriptedTurnDetector{probabilities: []float64{0.1}} // 一直判断「没说完」
	transcriber := &lengthTranscriber{}
	conn, ctx := dialRealtimeWith(t, Dependencies{Transcriber: transcriber, Timing: timing, TurnDetector: turnDetector,
		NewVoiceDetector: semanticFactory(t)})
	startSession(t, ctx, conn, realtimeproto.TurnDetection{Type: "semantic_vad", Eagerness: "auto"}, false)
	// 0.2s 静音 → 0.6s 说话 → 0.4s 停顿（没说完，等接着说）→ 0.6s 说话（并起来 1.6s，到了上限）→ 0.4s 静音
	sendPattern(t, ctx, conn, [2]int{200, 0}, [2]int{600, 1}, [2]int{400, 0}, [2]int{600, 1}, [2]int{400, 0})
	events := readEvents(t, ctx, conn, 3)
	if stopped, ok := events[1].(realtimeproto.InputAudioBufferSpeechStopped); !ok || stopped.AudioEndMs < 1790 || stopped.AudioEndMs > 1810 {
		t.Fatalf("并到上限应马上按说完处理（第二段结束处 1.8s）：%v", events)
	}
	if got := time.Duration(transcriber.lastAudio.Load()); got < 1590*time.Millisecond || got > 1610*time.Millisecond {
		t.Errorf("识别的是并起来的整句（0.2s → 1.8s）：%v", got)
	}
	turnDetector.mu.Lock()
	calls := turnDetector.calls
	turnDetector.mu.Unlock()
	if calls != 1 {
		t.Errorf("到了上限不再问说完判断模型：调用了 %d 次，应为 1 次", calls)
	}
}

// quietDetector 是从不报告在说话的 VAD（唤醒词响了、但 VAD 没把它当成一段话）。
type quietDetector struct{}

func (quietDetector) AcceptWaveform([]float32)     {}
func (quietDetector) IsSpeech() bool               { return false }
func (quietDetector) PopSegments() []SpeechSegment { return nil }
func (quietDetector) Close()                       {}

// 唤醒词说完就停了、一直没开口：等 Timing.WakeOnlyWait（注入的值）后按「只叫了唤醒词」交出这一句。
func TestWakeOnlyWaitComesFromTiming(t *testing.T) {
	timing := DefaultTiming
	timing.WakeOnlyWait = 200 * time.Millisecond
	server := httptest.NewServer(NewHandler(Dependencies{Transcriber: &fixedTranscriber{text: "哈罗哈罗。"}, Timing: timing,
		NewVoiceDetector:  func(VoiceDetectorConfig) (VoiceDetector, error) { return quietDetector{}, nil },
		NewKeywordSpotter: func() (KeywordSpotter, error) { return &fakeSpotter{}, nil },
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil))}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, transcriptionURL(t, server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	startSession(t, ctx, conn, realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 300}, true)
	sendKeyword(t, ctx, conn)
	sendPattern(t, ctx, conn, [2]int{300, 0}) // 比注入的 200 ms 长、比默认的 1 s 短
	events := readEvents(t, ctx, conn, 4)
	if _, ok := events[2].(realtimeproto.InputAudioBufferSpeechStopped); !ok {
		t.Fatalf("等过注入的 WakeOnlyWait 应按说完处理：%v", events)
	}
	if completed, ok := events[3].(realtimeproto.InputAudioTranscriptionCompleted); !ok || !completed.Wake {
		t.Fatalf("只叫了唤醒词也原样交出：%v", events)
	}
}

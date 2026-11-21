package asrserver

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
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

// 时间轴：累计采样点一直往前走；只保留最近 historySeconds 秒，取超出范围的部分时自动截掉。
func TestAudioTimelineKeepsRecentHistory(t *testing.T) {
	timeline := audioTimeline{keep: samplesIn(DefaultTiming.History)}
	historySeconds := int(DefaultTiming.History.Seconds())
	second := realtimeproto.TranscriptionSampleRate
	samples := timeline.append(pcmOf(second, 16384))
	if timeline.now() != second || len(samples) != second || samples[0] != 0.5 {
		t.Fatalf("一秒之后：now=%d，采样 %d 个、第一个 %v", timeline.now(), len(samples), samples[0])
	}
	for range historySeconds + 1 {
		timeline.append(pcmOf(second, 0))
	}
	now := timeline.now()
	if got := len(timeline.slice(0, now)) / 2; got != historySeconds*second {
		t.Errorf("只保留最近 %d 秒：取到 %d 个采样点", historySeconds, got)
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

// 唤醒门：没启用时一直在听；启用时醒着、且终端没在播放才听。播放前醒没醒，播完恢复。
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
	gate.sleep() // 播放中按回车休眠
	gate.pauseForPlayback(false)
	gate.resumeAfterPlayback()
	if !gate.awake || spotter.resets != 3 {
		t.Errorf("播完恢复播放前（醒着）；每次开始播放、休眠都重置唤醒词识别：awake=%v resets=%d", gate.awake, spotter.resets)
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
		NewVoiceDetector:  func(realtimeproto.TurnDetection) (VoiceDetector, error) { return quietDetector{}, nil },
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
	turn := realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 300}
	if err := writeClientEvent(ctx, conn, transcriptionUpdate(turn, true)); err != nil {
		t.Fatal(err)
	}
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

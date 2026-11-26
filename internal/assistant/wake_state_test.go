package assistant

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 终端记的醒 / 睡只照语音前端的回复改：请求发出去、还没回复时不变，等回复期间不再发第二次；
// 回复和在等的请求一致时这次请求完成；不一致（例如识别流重开后告诉终端的旧状态）时照回复改、请求接着等。
func TestWakeStateChangesOnlyOnReply(t *testing.T) {
	ctx := context.Background()
	brain := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}
	a := NewAssistant(Dependencies{Brain: brain}, Options{WakeWord: WakeWordOptions{}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	window := &conversationWindow{idle: time.Minute}
	defer window.close()

	a.requestWakeState(ctx, window, true, "手动")
	a.requestWakeState(ctx, window, true, "手动") // 还在等回复时又按了一次回车
	if window.awake || window.pending == nil {
		t.Fatal("手动唤醒还没回复：终端不应已经醒了")
	}
	if got := brain.sentTypes(); !slices.Equal(got, []string{"haro.wake_word.wake"}) {
		t.Fatalf("等回复期间不应再发请求：发了 %v", got)
	}
	a.handleWakeEvent(ctx, window, realtimeproto.HaroWakeWordState{Awake: true})
	if !window.awake || window.pending != nil || window.expired() == nil {
		t.Fatal("回复醒着：应醒了、请求完成、对话窗口开始倒计时")
	}

	a.requestWakeState(ctx, window, false, "手动休眠")
	a.handleWakeEvent(ctx, window, realtimeproto.HaroWakeWordState{Awake: true})
	if !window.awake || window.pending == nil {
		t.Fatal("回复和请求不一致：照回复（醒着）记，休眠请求接着等")
	}
	a.handleWakeEvent(ctx, window, realtimeproto.HaroWakeWordState{Awake: false})
	if window.awake || window.pending != nil || window.expired() != nil {
		t.Fatal("回复睡着了：应睡着、请求完成、不再倒计时")
	}
}

// 回答播放期间收到语音前端的回复（例如等着的休眠请求的回复）：留给主循环处理，不能丢——
// 丢了终端就等不到回复，当作没切成，和已经睡着的语音前端对不上。
func TestWakeStateDuringPlaybackIsKeptForTheMainLoop(t *testing.T) {
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	a := newBargeInAssistant(brain, sink)
	brain.events <- chunk
	brain.events <- realtimeproto.HaroWakeWordState{Awake: false}
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	close(sink.release)
	if err := a.playResponse(context.Background(), noSpan(), "item_1", time.Now()); err != nil {
		t.Fatalf("正常播完应返回 nil，得到 %v", err)
	}
	select {
	case event := <-a.replay:
		if state, ok := event.(realtimeproto.HaroWakeWordState); !ok || state.Awake {
			t.Fatalf("留给主循环的应是那条回复：%+v", event)
		}
	default:
		t.Fatal("播放期间收到的醒 / 睡回复被丢掉了")
	}
}

// 唤醒词由服务端告诉（session.updated，来自 haro-asr 的关键词文件）：收到之前提示里不写具体的词，收到之后照它提示。
func TestWakeHintUsesPhrasesFromServer(t *testing.T) {
	a := NewAssistant(Dependencies{Brain: &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 1)}},
		Options{WakeWord: WakeWordOptions{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := a.wakeHint(); got != "叫唤醒词唤醒" {
		t.Errorf("还不知道唤醒词时：%q", got)
	}
	window := &conversationWindow{idle: time.Minute}
	defer window.close()
	a.handleWakeEvent(context.Background(), window, realtimeproto.SessionUpdated{Session: realtimeproto.SessionUpdate{
		WakeWord: true, WakePhrases: []string{"哈罗哈罗", "小助手"}}})
	if got := a.wakeHint(); got != "叫「哈罗哈罗」或「小助手」唤醒" {
		t.Errorf("收到唤醒词以后：%q", got)
	}
}

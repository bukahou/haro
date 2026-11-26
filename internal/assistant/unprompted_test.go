package assistant

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 服务端自己开口（到点的提醒）：主循环照播回答的方式播（报播放开始 / 播完），播完以后请语音前端醒来，
// 主人可以不叫唤醒词直接接话；本来就醒着时只重新计时。
func TestUnpromptedResponseIsPlayedThenStaysAwake(t *testing.T) {
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	a := newBargeInAssistant(brain, sink)
	window := &conversationWindow{idle: time.Minute}
	defer window.close()
	brain.events <- realtimeproto.ResponseOutputAudioTranscriptDelta{Delta: "该喝水了。"}
	brain.events <- chunk
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	close(sink.release)
	a.handleWakeEvent(context.Background(), window, realtimeproto.ResponseCreated{Unprompted: realtimeproto.UnpromptedReminder})
	select {
	case <-sink.opened:
	default:
		t.Fatal("应播出来")
	}
	want := []string{"haro.playback.started", "haro.playback.stopped", "haro.wake_word.wake"}
	if got := brain.sentTypes(); !slices.Equal(got, want) {
		t.Fatalf("播完以后应请语音前端醒来：%v，应为 %v", got, want)
	}
	if window.pending == nil || !window.pending.awake {
		t.Error("应在等醒来的回复")
	}

	// 本来就醒着：不再请醒，只从播完开始重新计时
	brain.sent = nil
	window.settle()
	window.wake()
	sink2 := newSlowSink()
	a.deps.Speaker = sink2
	brain.events <- chunk
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	close(sink2.release)
	a.handleWakeEvent(context.Background(), window, realtimeproto.ResponseCreated{Unprompted: realtimeproto.UnpromptedReminder})
	if got := brain.sentTypes(); slices.Contains(got, "haro.wake_word.wake") || window.expired() == nil {
		t.Errorf("醒着时只重新计时：%v", got)
	}
}

// 不是服务端自己开口的 response.created（回答主人说的话时）在主循环里不管：回答由「说完」那一轮播。
func TestPromptedResponseCreatedIsIgnoredByMainLoop(t *testing.T) {
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 4)}, newSlowSink()
	a := newBargeInAssistant(brain, sink)
	window := &conversationWindow{idle: time.Minute}
	defer window.close()
	a.handleWakeEvent(context.Background(), window, realtimeproto.ResponseCreated{})
	if got := brain.sentTypes(); len(got) != 0 {
		t.Errorf("不该有动作：%v", got)
	}
}

// 模型判断主人要结束对话（response.done 带 end_conversation）：这一轮播完就请语音前端休眠，不等对话窗口。
// 被打断就不睡（主人还在说）。
func TestEndConversationSleepsAfterPlayback(t *testing.T) {
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	a := newBargeInAssistant(brain, sink)
	window := &conversationWindow{idle: time.Minute}
	defer window.close()
	window.wake()
	brain.events <- chunk
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted, EndConversation: true}
	close(sink.release)
	a.handleWakeEvent(context.Background(), window, realtimeproto.InputAudioBufferSpeechStopped{ItemID: "item_1"})
	if got := brain.sentTypes(); !slices.Contains(got, "haro.wake_word.sleep") {
		t.Fatalf("播完应请语音前端休眠：%v", got)
	}

	brain.sent = nil
	window.settle()
	window.wake()
	sink2 := newSlowSink()
	a.deps.Speaker = sink2
	brain.events <- chunk
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted, EndConversation: true}
	brain.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗"} // 还在播（没 release）的时候被打断
	a.handleWakeEvent(context.Background(), window, realtimeproto.InputAudioBufferSpeechStopped{ItemID: "item_2"})
	if !sink2.wasAborted() {
		t.Fatal("测试前提：应是播的时候被打断")
	}
	if got := brain.sentTypes(); slices.Contains(got, "haro.wake_word.sleep") {
		t.Errorf("被打断就不睡：%v", got)
	}
}

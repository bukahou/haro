package brain_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 醒 / 睡由识别流（haro-asr 的唤醒门）决定：会话把终端的请求转给它，等它回复 haro.wake_word.state 才转给终端；
// 识别流断开期间的请求、发不出去的请求，重开后告诉新的流，由它回复。

// expectQuiet 断言接下来 d 之内会话不给终端发任何事件。
func expectQuiet(t *testing.T, session *brain.Session, d time.Duration, what string) {
	t.Helper()
	select {
	case event := <-session.Events():
		t.Fatalf("%s：收到 %s（%+v）", what, event.Type(), event)
	case <-time.After(d):
	}
}

// waitAwakeCalls 等识别流收到的醒 / 睡请求变成 want。
func waitAwakeCalls(t *testing.T, stream *braintest.Stream, want []bool, what string) {
	t.Helper()
	deadline := time.Now().Add(braintest.EventTimeout)
	for !slices.Equal(stream.AwakeCalls(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%s：识别流收到 %v，应为 %v", what, stream.AwakeCalls(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func openWakeSession(t *testing.T, voice *braintest.Voice) (*brain.Session, *braintest.Stream) {
	t.Helper()
	session := newReopenSession(t, voice, brain.ReopenTiming{Delay: time.Millisecond, MaxDelay: 5 * time.Millisecond,
		LostNoticeAfter: time.Hour})
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	return session, braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn, WakeWord: true})
}

func expectState(t *testing.T, session *brain.Session, awake bool, what string) {
	t.Helper()
	if state := braintest.Expect[realtimeproto.HaroWakeWordState](t, session); state.Awake != awake {
		t.Fatalf("%s：转给终端的状态是 awake=%v，应为 %v", what, state.Awake, awake)
	}
}

func TestWakeStateFollowsRecognizerReplies(t *testing.T) {
	voice := braintest.NewVoice()
	session, stream := openWakeSession(t, voice)
	ctx := context.Background()

	for i, awake := range []bool{true, false} {
		var request realtimeproto.ClientEvent = realtimeproto.HaroWakeWordSleep{}
		if awake {
			request = realtimeproto.HaroWakeWordWake{}
		}
		if err := session.Send(ctx, request); err != nil {
			t.Fatal(err)
		}
		waitAwakeCalls(t, stream, []bool{true, false}[:i+1], "请求应转给识别流")
		expectQuiet(t, session, 50*time.Millisecond, "识别流还没回复：不应告诉终端已经切了")
		if err := stream.Emit(realtimeproto.HaroWakeWordState{Awake: awake}); err != nil {
			t.Fatal(err)
		}
		expectState(t, session, awake, "识别流的回复应转给终端")
	}
}

// 识别流断开、正在重开时终端要休眠：记下，重开后告诉新的流；新的流回复之后才转给终端。
func TestWakeRequestDuringReopenGoesToTheNewStream(t *testing.T) {
	voice := braintest.NewVoice()
	session, first := openWakeSession(t, voice)
	ctx := context.Background()
	_ = session.Send(ctx, realtimeproto.HaroWakeWordWake{})
	_ = first.Emit(realtimeproto.HaroWakeWordState{Awake: true})
	expectState(t, session, true, "醒着")

	voice.FailNextOpen(errors.New("连不上 haro-asr"))
	_ = first.Close()
	deadline := time.Now().Add(braintest.EventTimeout)
	for voice.OpenAttempts() < 2 { // 会话发现断开、开始重开
		if time.Now().After(deadline) {
			t.Fatal("会话没有重开识别流")
		}
		time.Sleep(time.Millisecond)
	}
	if err := session.Send(ctx, realtimeproto.HaroWakeWordSleep{}); err != nil {
		t.Fatalf("重开期间的休眠请求应记下，不报错：%v", err)
	}
	voice.FailNextOpen(nil)
	second := waitOpened(t, voice, 2)
	waitAwakeCalls(t, second, []bool{false}, "重开后应按终端要的（睡着）告诉新的流")
	expectQuiet(t, session, 50*time.Millisecond, "新的流还没回复：不应告诉终端已经睡了")
	_ = second.Emit(realtimeproto.HaroWakeWordState{Awake: false})
	expectState(t, session, false, "新的流的回复应转给终端")
}

// 请求发不出去（识别流坏了）：记下要的状态，关掉这条流重开，新的流收到这个状态、回复后转给终端。
func TestWakeRequestThatCannotBeSentReopensTheStream(t *testing.T) {
	voice := braintest.NewVoice()
	session, first := openWakeSession(t, voice)
	first.FailNextSetAwake(errors.New("写不进"))
	if err := session.Send(context.Background(), realtimeproto.HaroWakeWordWake{}); err != nil {
		t.Fatalf("发不出去时由会话自己重开，不报给终端：%v", err)
	}
	second := waitOpened(t, voice, 2)
	waitAwakeCalls(t, second, []bool{true}, "重开后应把要的状态（醒着）告诉新的流")
	_ = second.Emit(realtimeproto.HaroWakeWordState{Awake: true})
	expectState(t, session, true, "新的流的回复应转给终端")
}

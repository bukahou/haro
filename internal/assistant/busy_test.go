package assistant

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 主人在说话时算忙（看守这时不换麦克风，否则这句话就丢了）：从开始说话到这一轮答完。
// 说到一半服务端听不见了、或者休眠了（之后不会再有「说完」），都要放开。
func TestBusyWhileOwnerSpeaks(t *testing.T) {
	ctx := context.Background()
	brain := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}
	a := NewAssistant(Dependencies{Brain: brain}, Options{WakeWord: WakeWordOptions{Phrase: "哈罗哈罗"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	window := &conversationWindow{idle: time.Minute}
	defer window.stop()
	window.wake()

	if a.Busy() {
		t.Fatal("还没说话：不该忙")
	}
	a.handleWakeEvent(ctx, window, realtimeproto.InputAudioBufferSpeechStarted{ItemID: "item_1"})
	if !a.Busy() {
		t.Fatal("主人开始说话：应算忙")
	}
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	a.handleWakeEvent(ctx, window, realtimeproto.InputAudioBufferSpeechStopped{ItemID: "item_1"})
	if a.Busy() {
		t.Fatal("这一轮答完了：不该再忙")
	}

	a.handleWakeEvent(ctx, window, realtimeproto.InputAudioBufferSpeechStarted{ItemID: "item_2"})
	a.handleWakeEvent(ctx, window, realtimeproto.HaroNotice{Kind: realtimeproto.NoticeTranscriptionLost})
	if a.Busy() || window.expired() == nil {
		t.Fatal("服务端听不见了：这句不会再有「说完」，不该再算忙，对话窗口照常倒计时")
	}

	a.handleWakeEvent(ctx, window, realtimeproto.InputAudioBufferSpeechStarted{ItemID: "item_3"})
	a.sleep(ctx, window, "手动休眠")
	if a.Busy() {
		t.Fatal("说到一半休眠：不该再忙")
	}
}

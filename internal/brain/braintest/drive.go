package braintest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// EventTimeout 是等会话发出下一个事件的上限。
const EventTimeout = 5 * time.Second

// Open 发出 session.update 打开流式识别，返回打开的识别流。update 没带 turn_detection 时用服务端 VAD。
func Open(t testing.TB, session *brain.Session, voice *Voice, update realtimeproto.SessionUpdate) *Stream {
	t.Helper()
	if update.TurnDetection == nil {
		update.TurnDetection = &realtimeproto.TurnDetection{Type: "server_vad"}
	}
	before := voice.Opened()
	if err := session.Send(context.Background(), update); err != nil {
		t.Fatalf("session.update：%v", err)
	}
	if voice.Opened() == before {
		t.Fatal("session.update 之后会话没有打开识别流")
	}
	return voice.Stream()
}

// Utterance 是主人说的一句话（识别服务交给大脑的结果）。
type Utterance struct {
	Text           string
	Speaker        string // 声纹认出的人
	SpeakerChecked bool   // 比对过声纹；为 true 且 Speaker 为空表示不是登记的人
	Wake           bool   // 这句话之前叫了唤醒词
}

// Ask 让主人说一句话并请求回答：识别流先报「开始说 / 说完了」，终端请求回答，识别结果随后到达。
// 说话前转发给终端的事件（唤醒、开始说、说完了）由 Ask 消费掉；返回从识别结果开始、到 response.done 为止的事件。
func Ask(t testing.TB, session *brain.Session, stream *Stream, u Utterance) []realtimeproto.ServerEvent {
	t.Helper()
	Speak(t, session, stream, u)
	send(t, session, realtimeproto.ResponseCreate{})
	Recognized(t, stream, u)
	return CollectUntilDone(t, session)
}

// Speak 让识别流报出「（叫唤醒词）→ 开始说 → 说完了」，并消费会话转发给终端的这几个事件。
// 之后由测试请求回答、再用 Recognized 交出识别结果；u 的编号记在 stream 上，Recognized 沿用。
func Speak(t testing.TB, session *brain.Session, stream *Stream, u Utterance) {
	t.Helper()
	item := stream.NextItemID()
	if u.Wake {
		emit(t, stream, realtimeproto.HaroWakeWordDetected{Keyword: "唤醒词"})
		Expect[realtimeproto.HaroWakeWordDetected](t, session)
	}
	emit(t, stream, realtimeproto.InputAudioBufferSpeechStarted{ItemID: item},
		realtimeproto.InputAudioBufferSpeechStopped{ItemID: item})
	Expect[realtimeproto.InputAudioBufferSpeechStarted](t, session)
	Expect[realtimeproto.InputAudioBufferSpeechStopped](t, session)
}

// Recognized 交出最近一句话（Speak 报过的那句）的识别结果。
func Recognized(t testing.TB, stream *Stream, u Utterance) {
	t.Helper()
	emit(t, stream, realtimeproto.InputAudioTranscriptionCompleted{ItemID: stream.LastItemID(), Transcript: u.Text,
		Speaker: u.Speaker, SpeakerChecked: u.SpeakerChecked})
}

// Next 等会话发出下一个事件。
func Next(t testing.TB, session *brain.Session) realtimeproto.ServerEvent {
	t.Helper()
	select {
	case event := <-session.Events():
		return event
	case <-time.After(EventTimeout):
		t.Fatal("等会话的事件超时")
		return nil
	}
}

// Expect 等下一个事件，并要求它是 T 类型。
func Expect[T realtimeproto.ServerEvent](t testing.TB, session *brain.Session) T {
	t.Helper()
	event := Next(t, session)
	got, ok := event.(T)
	if !ok {
		var want T
		t.Fatalf("下一个事件应是 %s，收到 %s（%+v）", want.Type(), event.Type(), event)
	}
	return got
}

// CollectUntilDone 收集事件直到 response.done（含）。
func CollectUntilDone(t testing.TB, session *brain.Session) []realtimeproto.ServerEvent {
	t.Helper()
	var events []realtimeproto.ServerEvent
	deadline := time.After(EventTimeout)
	for {
		select {
		case event := <-session.Events():
			events = append(events, event)
			if _, done := event.(realtimeproto.ResponseDone); done {
				return events
			}
		case <-deadline:
			t.Fatalf("等 response.done 超时，已收到 %v", Types(events))
		}
	}
}

// Done 是事件里最后的 response.done。
func Done(t testing.TB, events []realtimeproto.ServerEvent) realtimeproto.ResponseDone {
	t.Helper()
	if len(events) > 0 {
		if done, ok := events[len(events)-1].(realtimeproto.ResponseDone); ok {
			return done
		}
	}
	t.Fatalf("事件不以 response.done 结尾：%v", Types(events))
	return realtimeproto.ResponseDone{}
}

// Spoken 拼出事件里播出的声音数据（替身合成器把文字原样放进 PCM 时，就是播出的文字）。
func Spoken(events []realtimeproto.ServerEvent) string {
	var spoken strings.Builder
	for _, event := range events {
		if audio, ok := event.(realtimeproto.ResponseOutputAudioDelta); ok {
			spoken.Write(audio.Audio.Data)
		}
	}
	return spoken.String()
}

// Transcript 拼出事件里发给终端的回答文字。
func Transcript(events []realtimeproto.ServerEvent) string {
	var text strings.Builder
	for _, event := range events {
		if delta, ok := event.(realtimeproto.ResponseOutputAudioTranscriptDelta); ok {
			text.WriteString(delta.Delta)
		}
	}
	return text.String()
}

// Types 是事件的类型名。
func Types(events []realtimeproto.ServerEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type())
	}
	return types
}

func send(t testing.TB, session *brain.Session, event realtimeproto.ClientEvent) {
	t.Helper()
	if err := session.Send(context.Background(), event); err != nil {
		t.Fatalf("发送 %s：%v", event.Type(), err)
	}
}

func emit(t testing.TB, stream *Stream, events ...realtimeproto.ServerEvent) {
	t.Helper()
	if err := stream.Emit(events...); err != nil {
		t.Fatal(err)
	}
}

package brain_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// protocolCode 是会话返回的错误码；不是带码的错误时为空。
func protocolCode(err error) realtimeproto.ErrorCode {
	if protocolErr, ok := errors.AsType[*realtimeproto.ProtocolError](err); ok {
		return protocolErr.Code
	}
	return ""
}

// session.update 声明的输入格式不是识别流要的（例如官方客户端固定的 24 kHz）：整条拒绝、识别流不打开，
// 不等到每一块音频再报错。
func TestSessionUpdateRejectsUnsupportedInputFormat(t *testing.T) {
	voice := braintest.NewVoice()
	session := newStreamSession(t, voice, &fakeModel{}, nil)
	turn := realtimeproto.TurnDetection{Type: realtimeproto.TurnServerVAD}
	err := session.Send(context.Background(), realtimeproto.SessionUpdate{TurnDetection: &turn,
		InputFormat: realtimeproto.AudioFormat{SampleRate: 24000, Channels: 1}})
	if protocolCode(err) != realtimeproto.CodeUnsupportedAudioFormat {
		t.Fatalf("24 kHz 应以 unsupported_audio_format 拒绝：%v", err)
	}
	if voice.Opened() != 0 {
		t.Error("被拒的 session.update 不应打开识别流")
	}
	if err := session.Send(context.Background(), realtimeproto.SessionUpdate{TurnDetection: &turn,
		InputFormat: realtimeproto.TranscriptionFormat}); err != nil || voice.Opened() != 1 {
		t.Errorf("16 kHz 单声道应照常打开识别流：%v（打开 %d 条）", err, voice.Opened())
	}
}

// 识别流打开以后再改说完判断、唤醒门：明确拒绝（unsupported_feature），识别流不重开、设置不变；
// 同样的设置再发一次、只改 create_response、不带 turn_detection 的都照常收下。
func TestSessionUpdateAfterStreamOpened(t *testing.T) {
	voice := braintest.NewVoice()
	session := newStreamSession(t, voice, &fakeModel{}, nil)
	ctx := context.Background()
	turn := realtimeproto.TurnDetection{Type: realtimeproto.TurnServerVAD, SilenceDurationMs: 700}
	braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn})

	semantic := realtimeproto.TurnDetection{Type: realtimeproto.TurnSemanticVAD}
	shorter := turn
	shorter.SilenceDurationMs = 300
	for name, update := range map[string]realtimeproto.SessionUpdate{
		"换说完方式":   {TurnDetection: &semantic},
		"改静音时长":   {TurnDetection: &shorter},
		"打开唤醒门":   {TurnDetection: &turn, WakeWord: true},
		"单独要求唤醒门": {WakeWord: true},
	} {
		if err := session.Send(ctx, update); protocolCode(err) != realtimeproto.CodeUnsupportedFeature {
			t.Errorf("%s：识别流已开时应以 unsupported_feature 拒绝：%v", name, err)
		}
	}
	if voice.Opened() != 1 || voice.Config().TurnDetection.SilenceDurationMs != 700 || voice.Config().WakeWord {
		t.Errorf("被拒之后识别流不重开、设置不变：打开 %d 条，%+v", voice.Opened(), voice.Config())
	}

	manual := false
	noAutoResponse := turn
	noAutoResponse.CreateResponse = &manual
	for name, update := range map[string]realtimeproto.SessionUpdate{
		"同样的设置再发一次":          {TurnDetection: &turn},
		"只改 create_response": {TurnDetection: &noAutoResponse},
		"不带 turn_detection":  {InputFormat: realtimeproto.TranscriptionFormat},
	} {
		if err := session.Send(ctx, update); err != nil {
			t.Errorf("%s 应照常收下：%v", name, err)
		}
	}
	if voice.Opened() != 1 {
		t.Errorf("收下的 session.update 也不重开识别流：打开 %d 条", voice.Opened())
	}
}

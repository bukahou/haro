package realtimeproto

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

var input16k = AudioFormat{SampleRate: 16000, Channels: 1}

func TestClientEventsRoundTrip(t *testing.T) {
	createResponse := false
	events := []ClientEvent{
		SessionUpdate{InputFormat: input16k, WakeWord: true,
			TurnDetection: &TurnDetection{Type: "semantic_vad", Eagerness: "auto", Threshold: 0.5, CreateResponse: &createResponse}},
		SessionUpdate{InputFormat: input16k}, // turn_detection 为 null：这次不改
		SessionUpdate{Kind: SessionTranscription, InputFormat: input16k, WakeWord: true,
			TurnDetection: &TurnDetection{Type: "server_vad", SilenceDurationMs: 500}}, // 服务端 → haro-asr
		InputAudioBufferAppend{Audio: PCMAudio{Format: input16k, Data: []byte{1, 2, 3, 4}}},
		ResponseCreate{}, ResponseCancel{}, HaroWakeWordSleep{}, HaroWakeWordWake{},
		HaroPlaybackStarted{}, HaroPlaybackStopped{}, ConversationItemTruncate{AudioEndMs: 2340},
		HaroTerminalDevices{Microphone: "USB 麦克风"},
		HaroMCPMessage{Message: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)},
	}
	for _, event := range events {
		data, err := EncodeClientEvent(event)
		if err != nil {
			t.Fatalf("编码 %s: %v", event.Type(), err)
		}
		decoded, err := DecodeClientEvent(data, input16k)
		if err != nil {
			t.Fatalf("解码 %s: %v\n%s", event.Type(), err, data)
		}
		if !reflect.DeepEqual(decoded, event) {
			t.Errorf("%s 往返不一致\n got %#v\nwant %#v\njson %s", event.Type(), decoded, event, data)
		}
	}
}

// 线上的字段名照 OpenAI Realtime 正式版；haro 自己的放在 "haro" 下。
func TestClientWireShape(t *testing.T) {
	data, _ := EncodeClientEvent(SessionUpdate{InputFormat: input16k, WakeWord: true,
		TurnDetection: &TurnDetection{Type: "server_vad"}})
	var wire map[string]any
	_ = json.Unmarshal(data, &wire)
	session := wire["session"].(map[string]any)
	input := session["audio"].(map[string]any)["input"].(map[string]any)
	if session["type"] != "realtime" || input["format"].(map[string]any)["rate"] != 16000.0 ||
		input["turn_detection"].(map[string]any)["type"] != "server_vad" ||
		session["haro"].(map[string]any)["wake_word"].(map[string]any)["enabled"] != true {
		t.Errorf("session.update 的形状不对：%s", data)
	}
}

func TestServerEventsRoundTrip(t *testing.T) {
	events := []ServerEvent{
		SessionUpdated{Session: SessionUpdate{Kind: SessionTranscription, InputFormat: input16k, WakeWord: true,
			TurnDetection: &TurnDetection{Type: "semantic_vad", Eagerness: "low"}}}, // haro-asr → 服务端：接受了设置
		InputAudioBufferSpeechStarted{AudioStartMs: 1200, ItemID: "item_1"},
		InputAudioBufferSpeechStopped{AudioEndMs: 3400, ItemID: "item_1"},
		InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: "讲个冷笑话"},
		InputAudioTranscriptionCompleted{ItemID: "item_2", Transcript: "你是谁", Speaker: "主人", SpeakerChecked: true},
		InputAudioTranscriptionCompleted{ItemID: "item_3", Transcript: "别理我", SpeakerChecked: true},
		InputAudioTranscriptionCompleted{ItemID: "item_3", Transcript: "嗯", SpeakerCheckFailed: true}, // 开着声纹、这句算不出来
		InputAudioTranscriptionCompleted{ItemID: "item_4", Transcript: "哈罗哈罗，在吗", SpeakerChecked: true, SpeakerTentative: "主人",
			Language: "zh", Wake: true, BargeIn: true, AudioMs: 1500},
		InputAudioTranscriptionFailed{ItemID: "item_4", Code: CodeTranscriptionFailed, Message: "识别失败"},
		HaroWakeWordDetected{Keyword: "哈罗哈罗", AudioMs: 900, Score: 0.62, Level: 0.03},
		HaroWakeWordState{Awake: true},
		HaroWakeWordState{},
		HaroNotice{Kind: NoticeResearchDone},
		ResponseOutputAudioTranscriptDelta{Delta: "为什么数学老师离婚了？"},
		ResponseOutputAudioDelta{Audio: PCMAudio{Format: AudioFormat{SampleRate: 32000, Channels: 1}, Data: []byte{9, 8, 7, 6}}},
		ResponseCreated{},
		ResponseDone{Status: StatusCompleted, Usage: &Usage{InputTokens: 1200, OutputTokens: 20}},
		ResponseDone{Status: StatusIncomplete, Reason: ReasonEmptyTranscript},
		Error{Code: CodeNoTranscriptionStream, Message: "识别流没有打开"},
		HaroMCPMessage{Message: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)},
	}
	for _, event := range events {
		data, err := EncodeServerEvent(event)
		if err != nil {
			t.Fatalf("编码 %s: %v", event.Type(), err)
		}
		decoded, err := DecodeServerEvent(data)
		if err != nil {
			t.Fatalf("解码 %s: %v\n%s", event.Type(), err, data)
		}
		if !reflect.DeepEqual(decoded, event) {
			t.Errorf("%s 往返不一致\n got %#v\nwant %#v\njson %s", event.Type(), decoded, event, data)
		}
	}
}

// 错误事件照 Realtime 的形状带 type；没给码的按 server_error。
func TestErrorWireShape(t *testing.T) {
	for _, c := range []struct {
		event         Error
		code, errType string
	}{
		{Error{Code: CodeUnsupportedEvent, Message: "x"}, "unsupported_event", "invalid_request_error"},
		{Error{Code: CodeTranscriptionFailed, Message: "x"}, "transcription_failed", "server_error"},
		{Error{Message: "x"}, "server_error", "server_error"},
	} {
		data, _ := EncodeServerEvent(c.event)
		var wire struct {
			Error struct{ Type, Code string }
		}
		_ = json.Unmarshal(data, &wire)
		if wire.Error.Code != c.code || wire.Error.Type != c.errType {
			t.Errorf("%+v 编码成 %s", c.event, data)
		}
	}
}

// 声纹的三种结果只能是一种：算不出来（speaker_check_failed）时还说比对过、认出了谁，编码和解码都拒绝。
func TestSpeakerCheckFailedIsExclusive(t *testing.T) {
	for _, event := range []InputAudioTranscriptionCompleted{
		{ItemID: "item_1", SpeakerCheckFailed: true, SpeakerChecked: true},
		{ItemID: "item_1", SpeakerCheckFailed: true, Speaker: "主人"},
	} {
		if _, err := EncodeServerEvent(event); err == nil {
			t.Errorf("矛盾的声纹结果不应编码出去：%+v", event)
		}
	}
	data := []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"item_1","transcript":"嗯",` +
		`"haro":{"speaker":"主人","speaker_checked":true,"speaker_check_failed":true}}`)
	if _, err := DecodeServerEvent(data); err == nil {
		t.Errorf("矛盾的声纹结果应解码失败：%s", data)
	}
}

// 对端自己的问题（server_error 类）过一会儿再试可能就好；请求类的码原样再发也一样被拒。没有码的按 server_error。
func TestErrorCodeServerSide(t *testing.T) {
	for code, want := range map[ErrorCode]bool{
		CodeServerError: true, CodeTranscriptionFailed: true, "": true,
		CodeUnsupportedFeature: false, CodeInvalidEvent: false, CodeUnsupportedAudioFormat: false,
	} {
		if got := code.ServerSide(); got != want {
			t.Errorf("%q.ServerSide() = %v，应为 %v", code, got, want)
		}
	}
}

// 官方实现发的 delta 没有 haro 扩展字段：按官方默认 24 kHz 单声道。
func TestAudioDeltaWithoutHaroFormat(t *testing.T) {
	event, err := DecodeServerEvent([]byte(`{"type":"response.output_audio.delta","delta":"AQI="}`))
	if err != nil {
		t.Fatal(err)
	}
	audio := event.(ResponseOutputAudioDelta).Audio
	if audio.Format != DefaultOutputFormat || len(audio.Data) != 2 {
		t.Errorf("得到 %+v", audio)
	}
}

// 对端比自己新时多出来的服务端事件：忽略，不报错。
func TestUnknownServerEventIsIgnored(t *testing.T) {
	event, err := DecodeServerEvent([]byte(`{"type":"session.created","session":{}}`))
	if event != nil || err != nil {
		t.Errorf("不认识的服务端事件应返回 nil, nil：%#v %v", event, err)
	}
}

// 客户端事件出错时带错误码：调用方按码处理，不看文字。
func TestDecodeClientEventErrorCodes(t *testing.T) {
	cases := []struct {
		name  string
		data  string
		input AudioFormat
		code  ErrorCode
	}{
		{"不认识的事件", `{"type":"conversation.item.delete"}`, input16k, CodeUnsupportedEvent},
		{"手动提交（服务端判断说完）", `{"type":"input_audio_buffer.commit"}`, input16k, CodeUnsupportedEvent},
		{"不是 JSON", `{"type":`, input16k, CodeInvalidEvent},
		{"附带画面（画面改为看的时候经 MCP 要）", `{"type":"conversation.item.create","item":{"type":"message","role":"user",` +
			`"content":[{"type":"input_image","image_url":"data:image/jpeg;base64,AQ=="}]}}`, input16k, CodeUnsupportedEvent},
		{"送音频之前没声明格式", `{"type":"input_audio_buffer.append","audio":"AQI="}`, AudioFormat{}, CodeInputFormatRequired},
		{"不是 PCM", `{"type":"session.update","session":{"type":"realtime","audio":{"input":{"format":{"type":"audio/pcmu","rate":8000},` +
			`"turn_detection":null}}}}`, input16k, CodeUnsupportedAudioFormat},
		{"不认识的会话种类", `{"type":"session.update","session":{"type":"translation","audio":{"input":{"turn_detection":null}}}}`,
			input16k, CodeInvalidEvent},
	}
	for _, c := range cases {
		_, err := DecodeClientEvent([]byte(c.data), c.input)
		protocolErr, ok := errors.AsType[*ProtocolError](err)
		if !ok || protocolErr.Code != c.code {
			t.Errorf("%s：应返回错误码 %s，得到 %v", c.name, c.code, err)
		}
	}
}

// 处理事件时出的错：带码的保留码，其他的按 server_error；说明前面加上事件名。
func TestErrorEventFor(t *testing.T) {
	coded := ErrorEventFor("input_audio_buffer.append", ProtocolErrorf(CodeNoTranscriptionStream, "识别流没有打开"))
	if coded.Code != CodeNoTranscriptionStream || coded.Message != "input_audio_buffer.append: 识别流没有打开" {
		t.Errorf("带码的错误：%+v", coded)
	}
	plain := ErrorEventFor("", errors.New("连不上"))
	if plain.Code != CodeServerError || plain.Message != "连不上" {
		t.Errorf("不带码的错误：%+v", plain)
	}
}

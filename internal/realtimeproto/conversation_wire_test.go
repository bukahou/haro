package realtimeproto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

var input16k = AudioFormat{SampleRate: 16000, Channels: 1}

func TestClientEventsRoundTrip(t *testing.T) {
	createResponse := false
	events := []ClientEvent{
		SessionUpdate{Instructions: "你是 Haro", InputFormat: input16k, WakeWord: true,
			TurnDetection: &TurnDetection{Type: "semantic_vad", Eagerness: "auto", Threshold: 0.5, CreateResponse: &createResponse}},
		SessionUpdate{InputFormat: input16k},                 // 按键模式：turn_detection 为 null
		SessionUpdate{InputFormat: input16k, NoCamera: true}, // 没有摄像头的终端
		InputAudioBufferAppend{Audio: PCMAudio{Format: input16k, Data: []byte{1, 2, 3, 4}}},
		InputAudioBufferCommit{},
		ConversationItemCreate{Item: ConversationItem{Role: "user", Content: []ContentPart{
			{Type: "input_image", ImageJPEG: []byte{0xFF, 0xD8, 0xFF}}, {Type: "input_text", Text: "看这个"}}}},
		ResponseCreate{}, ResponseCancel{}, HaroWakeWordSleep{}, HaroWakeWordWake{},
		HaroPlaybackStarted{}, HaroPlaybackStopped{}, ConversationItemTruncate{AudioEndMs: 2340},
		HaroTerminalDevices{Microphone: "USB 麦克风", Camera: "摄像头"},
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
	data, _ = EncodeClientEvent(ConversationItemCreate{Item: ConversationItem{Role: "user",
		Content: []ContentPart{{Type: "input_image", ImageJPEG: []byte{1}}}}})
	if !strings.Contains(string(data), `"image_url":"data:image/jpeg;base64,AQ=="`) {
		t.Errorf("图片应以 data URI 发送：%s", data)
	}
}

func TestServerEventsRoundTrip(t *testing.T) {
	events := []ServerEvent{
		InputAudioBufferSpeechStarted{AudioStartMs: 1200, ItemID: "item_1"},
		InputAudioBufferSpeechStopped{AudioEndMs: 3400, ItemID: "item_1"},
		InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: "讲个冷笑话"},
		InputAudioTranscriptionCompleted{ItemID: "item_2", Transcript: "你是谁", Speaker: "主人", SpeakerChecked: true},
		InputAudioTranscriptionCompleted{ItemID: "item_3", Transcript: "别理我", SpeakerChecked: true},
		HaroWakeWordDetected{Keyword: "哈罗哈罗", AudioMs: 900},
		ResponseOutputAudioTranscriptDelta{Delta: "为什么数学老师离婚了？"},
		ResponseOutputAudioDelta{Audio: PCMAudio{Format: AudioFormat{SampleRate: 32000, Channels: 1}, Data: []byte{9, 8, 7, 6}}},
		ResponseDone{Status: StatusCompleted, Usage: &Usage{InputTokens: 1200, OutputTokens: 20}},
		ResponseDone{Status: StatusIncomplete, Reason: "empty_transcript"},
		Error{Message: "语音合成: 连不上"},
	}
	for _, event := range events {
		data, err := EncodeServerEvent(event)
		if err != nil {
			t.Fatalf("编码 %s: %v", event.Type(), err)
		}
		decoded, err := DecodeServerWireEvent(data)
		if err != nil {
			t.Fatalf("解码 %s: %v\n%s", event.Type(), err, data)
		}
		if !reflect.DeepEqual(decoded, event) {
			t.Errorf("%s 往返不一致\n got %#v\nwant %#v\njson %s", event.Type(), decoded, event, data)
		}
	}
}

// 官方实现发的 delta 没有 haro 扩展字段：按官方默认 24 kHz 单声道。
func TestAudioDeltaWithoutHaroFormat(t *testing.T) {
	event, err := DecodeServerWireEvent([]byte(`{"type":"response.output_audio.delta","delta":"AQI="}`))
	if err != nil {
		t.Fatal(err)
	}
	audio := event.(ResponseOutputAudioDelta).Audio
	if audio.Format != DefaultOutputFormat || len(audio.Data) != 2 {
		t.Errorf("得到 %+v", audio)
	}
}

func TestDecodeClientEventRejectsUnknown(t *testing.T) {
	if _, err := DecodeClientEvent([]byte(`{"type":"conversation.item.delete"}`), input16k); err == nil {
		t.Error("不认识的事件应报错")
	}
	if _, err := DecodeClientEvent([]byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user",`+
		`"content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}}`), input16k); err == nil {
		t.Error("图片只收 data URI")
	}
}

package realtimeproto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// 本文件是「转写会话」（OpenAI Realtime 的 intent=transcription）在 WebSocket 上的 JSON 格式子集，
// 用于 haro 服务端 ↔ haro-asr。和官方的差异：
//   - 音频固定为 16kHz 单声道 pcm16（官方 pcm 只支持 24kHz）；在 session.update 的 format.rate 里声明。
//   - 只实现 server_vad；服务端不发 session.created / committed，只发下面列出的事件。
// ⚠️ 落地 v3 或对接官方实现前，须对照当时的官方文档核对字段。

// TranscriptionPath 是转写会话的 WebSocket 路径。
const TranscriptionPath = "/v1/realtime?intent=transcription"

// TranscriptionSampleRate 是转写会话要求的采样率。
const TranscriptionSampleRate = 16000

// WireTranscriptionSessionUpdate 是客户端建立连接后发的第一条消息。
type WireTranscriptionSessionUpdate struct {
	Type    string                   `json:"type"` // "session.update"
	Session WireTranscriptionSession `json:"session"`
}

// WireTranscriptionSession 是转写会话的配置。
type WireTranscriptionSession struct {
	Type  string                 `json:"type"` // "transcription"
	Haro  *WireHaroSessionConfig `json:"haro,omitempty"`
	Audio struct {
		Input struct {
			Format        WireAudioFormat `json:"format"`
			TurnDetection *TurnDetection  `json:"turn_detection"`
		} `json:"input"`
	} `json:"audio"`
}

// WireHaroSessionConfig 是 haro 自己的扩展（OpenAI Realtime 没有唤醒词的概念）。
type WireHaroSessionConfig struct {
	WakeWord *struct {
		Enabled bool `json:"enabled"` // 启用唤醒门：会话从休眠开始
	} `json:"wake_word,omitempty"`
	Camera *struct {
		Enabled bool `json:"enabled"` // 对话会话：终端有没有摄像头（v3 H-2）；不声明 = 有
	} `json:"camera,omitempty"`
}

// WireWakeWordDetected 是服务端事件 haro.wake_word.detected。
type WireWakeWordDetected struct {
	Type    string  `json:"type"`
	Keyword string  `json:"keyword"`
	AudioMs int64   `json:"audio_ms"`
	Score   float64 `json:"score,omitempty"` // 唤醒词声纹分数（v4 D2 仲裁用）
	Level   float64 `json:"level,omitempty"` // 唤醒词那一段的音量（RMS）
}

// WireAudioFormat 是音频格式声明。
type WireAudioFormat struct {
	Type string `json:"type"` // "audio/pcm"
	Rate int    `json:"rate"`
}

// WireAudioAppend 携带一块 base64 编码的 pcm16 音频。
type WireAudioAppend struct {
	Type  string `json:"type"` // "input_audio_buffer.append"
	Audio string `json:"audio"`
}

// WireSpeechStarted / WireSpeechStopped / WireTranscriptionCompleted / WireError 是服务端事件。
type WireSpeechStarted struct {
	Type         string `json:"type"`
	AudioStartMs int64  `json:"audio_start_ms"`
	ItemID       string `json:"item_id"`
}

type WireSpeechStopped struct {
	Type       string `json:"type"`
	AudioEndMs int64  `json:"audio_end_ms"`
	ItemID     string `json:"item_id"`
}

type WireTranscriptionCompleted struct {
	Type       string                 `json:"type"`
	ItemID     string                 `json:"item_id"`
	Transcript string                 `json:"transcript"`
	Haro       *WireTranscriptSpeaker `json:"haro,omitempty"`
}

// WireTranscriptSpeaker 是识别结果里的声纹比对结果（haro 扩展，v3 H-3）。
type WireTranscriptSpeaker struct {
	Speaker string `json:"speaker"` // 认出来的人；没认出来为空
	Checked bool   `json:"speaker_checked"`
}

// NewWireTranscriptionCompleted 构造识别结果事件；checked 为 false 时不带声纹字段。
func NewWireTranscriptionCompleted(itemID, transcript, speaker string, checked bool) WireTranscriptionCompleted {
	wire := WireTranscriptionCompleted{Type: "conversation.item.input_audio_transcription.completed",
		ItemID: itemID, Transcript: transcript}
	if checked {
		wire.Haro = &WireTranscriptSpeaker{Speaker: speaker, Checked: true}
	}
	return wire
}

type WireError struct {
	Type  string `json:"type"` // "error"
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// NewTranscriptionSessionUpdate 构造 session.update；wakeWord 为 true 时启用唤醒门（haro 扩展）。
func NewTranscriptionSessionUpdate(turnDetection TurnDetection, wakeWord bool) WireTranscriptionSessionUpdate {
	update := WireTranscriptionSessionUpdate{Type: "session.update"}
	update.Session.Type = "transcription"
	if wakeWord {
		update.Session.Haro = &WireHaroSessionConfig{}
		update.Session.Haro.WakeWord = &struct {
			Enabled bool `json:"enabled"`
		}{Enabled: true}
	}
	update.Session.Audio.Input.Format = WireAudioFormat{Type: "audio/pcm", Rate: TranscriptionSampleRate}
	update.Session.Audio.Input.TurnDetection = &turnDetection
	return update
}

// NewWireAudioAppend 把 PCM 编码成 append 消息。
func NewWireAudioAppend(pcm []byte) WireAudioAppend {
	return WireAudioAppend{Type: "input_audio_buffer.append", Audio: base64.StdEncoding.EncodeToString(pcm)}
}

// NewWireError 构造错误事件。
func NewWireError(errorType, message string) WireError {
	wire := WireError{Type: "error"}
	wire.Error.Type, wire.Error.Message = errorType, message
	return wire
}

// DecodeServerWireEvent 把服务端发来的 JSON 解成 ServerEvent；不认识的事件返回 nil, nil（向前兼容）。
func DecodeServerWireEvent(data []byte) (ServerEvent, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("解析事件: %w", err)
	}
	switch head.Type {
	case "input_audio_buffer.speech_started":
		var wire WireSpeechStarted
		err := json.Unmarshal(data, &wire)
		return InputAudioBufferSpeechStarted{AudioStartMs: wire.AudioStartMs, ItemID: wire.ItemID}, err
	case "input_audio_buffer.speech_stopped":
		var wire WireSpeechStopped
		err := json.Unmarshal(data, &wire)
		return InputAudioBufferSpeechStopped{AudioEndMs: wire.AudioEndMs, ItemID: wire.ItemID}, err
	case "conversation.item.input_audio_transcription.completed":
		var wire WireTranscriptionCompleted
		err := json.Unmarshal(data, &wire)
		completed := InputAudioTranscriptionCompleted{ItemID: wire.ItemID, Transcript: wire.Transcript}
		if wire.Haro != nil {
			completed.Speaker, completed.SpeakerChecked = wire.Haro.Speaker, wire.Haro.Checked
		}
		return completed, err
	case "haro.wake_word.detected":
		var wire WireWakeWordDetected
		err := json.Unmarshal(data, &wire)
		return HaroWakeWordDetected{Keyword: wire.Keyword, AudioMs: wire.AudioMs, Score: wire.Score, Level: wire.Level}, err
	case "error":
		var wire WireError
		err := json.Unmarshal(data, &wire)
		return Error{Message: wire.Error.Message}, err
	default:
		if event, ok, err := decodeResponseEvent(head.Type, data); ok { // 对话会话的回答事件（conversation_wire.go）
			return event, err
		}
		return nil, nil
	}
}

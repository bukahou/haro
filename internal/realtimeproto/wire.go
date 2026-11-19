package realtimeproto

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// 本文件是事件在 WebSocket 上的 JSON 结构，全协议只有这一份：编解码在 codec.go，JSON Schema 由它生成（schema.go）。
//
// 事件名与字段照 OpenAI Realtime 正式版（GA）的子集。和官方不同的地方：
//   - 音频不限 24 kHz：输入格式在 session.update 的 session.audio.input.format 里声明（haro 用 16 kHz）；
//     输出格式随每段 response.output_audio.delta 放在 haro 扩展字段里（由语音合成引擎决定）。
//     收到的 delta 没带扩展字段时，按官方默认的 24 kHz 单声道处理。
//   - haro 自己的东西一律放在 "haro" 键下，或者用 haro. 开头的事件名；官方实现会忽略。
//   - 不发 session.created 等 haro 用不到的事件；收到不认识的服务端事件一律忽略。
//
// 两种会话共用这份定义：
//   - 对话会话（session.type = realtime）：终端 ↔ haro-server，路径 ConversationPath；
//   - 转写会话（session.type = transcription）：haro-server ↔ haro-asr，路径 TranscriptionPath、带 intent=TranscriptionIntent，
//     只用其中一部分事件。

// ConversationPath 是对话会话的 WebSocket 路径（haro-server 提供）。
const ConversationPath = "/v1/realtime"

// TranscriptionPath 是转写会话的 WebSocket 路径（haro-asr 提供）；连接地址上带 intent 参数 TranscriptionIntent。
const TranscriptionPath = "/v1/realtime"

// TranscriptionIntent 是转写会话连接地址上 intent 参数的值（照 OpenAI Realtime 的转写会话）。
const TranscriptionIntent = "transcription"

// TranscriptionURL 由转写服务的根地址（http(s)://host:port）拼出转写会话的 WebSocket 地址：
// 按结构把 scheme 换成 ws(s)、接上 TranscriptionPath、带上 intent 参数。
func TranscriptionURL(base string) (string, error) {
	address, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("转写服务地址 %q: %w", base, err)
	}
	switch address.Scheme {
	case "http":
		address.Scheme = "ws"
	case "https":
		address.Scheme = "wss"
	default:
		return "", fmt.Errorf("转写服务地址要是 http(s)://，现在是 %q", base)
	}
	address = address.JoinPath(TranscriptionPath)
	address.RawQuery = url.Values{"intent": {TranscriptionIntent}}.Encode()
	return address.String(), nil
}

// TranscriptionSampleRate 是转写会话要求的采样率（识别、唤醒词、声纹的模型都按 16 kHz）。
const TranscriptionSampleRate = 16000

// TranscriptionFormat 是转写会话的音频格式：16 kHz 单声道。
var TranscriptionFormat = AudioFormat{SampleRate: TranscriptionSampleRate, Channels: 1}

// MaxMessageBytes 是一条消息的大小上限，两端读消息时都按它限。
const MaxMessageBytes = 8 << 20

// DefaultOutputFormat 是官方 Realtime 的输出音频格式；delta 没声明格式时用它。
var DefaultOutputFormat = AudioFormat{SampleRate: 24000, Channels: 1}

// 事件名（Realtime 正式版的名字；haro 扩展以 haro. 开头）。
const (
	typeSessionUpdate            = "session.update"
	typeInputAudioBufferAppend   = "input_audio_buffer.append"
	typeConversationItemTruncate = "conversation.item.truncate"
	typeResponseCreate           = "response.create"
	typeResponseCancel           = "response.cancel"
	typeHaroWakeWordSleep        = "haro.wake_word.sleep"
	typeHaroWakeWordWake         = "haro.wake_word.wake"
	typeHaroPlaybackStarted      = "haro.playback.started"
	typeHaroPlaybackStopped      = "haro.playback.stopped"
	typeHaroTerminalDevices      = "haro.terminal.devices"
	typeHaroMCPMessage           = "haro.mcp.message"

	typeSpeechStarted          = "input_audio_buffer.speech_started"
	typeSpeechStopped          = "input_audio_buffer.speech_stopped"
	typeTranscriptionCompleted = "conversation.item.input_audio_transcription.completed"
	typeHaroWakeWordDetected   = "haro.wake_word.detected"
	typeTranscriptDelta        = "response.output_audio_transcript.delta"
	typeAudioDelta             = "response.output_audio.delta"
	typeResponseCreated        = "response.created"
	typeResponseDone           = "response.done"
	typeHaroNotice             = "haro.notice"
	typeError                  = "error"
)

// ——— 客户端事件 ———

type wireSessionUpdate struct {
	Type    string      `json:"type"`
	Session wireSession `json:"session"`
}

type wireSession struct {
	Type  SessionKind `json:"type" jsonschema:"realtime = 对话会话（终端 ↔ 服务端）；transcription = 转写会话（服务端 ↔ haro-asr）"`
	Audio struct {
		Input struct {
			Format        *wireAudioFormat `json:"format,omitempty" jsonschema:"之后 input_audio_buffer.append 的音频格式；送音频之前必须声明"`
			TurnDetection *TurnDetection   `json:"turn_detection" jsonschema:"由服务端的 VAD 判断说话开始和结束；null = 这次不改"`
		} `json:"input"`
	} `json:"audio"`
	Haro *wireHaroSession `json:"haro,omitempty" jsonschema:"haro 扩展"`
}

type wireHaroSession struct {
	WakeWord *wireEnabled `json:"wake_word,omitempty" jsonschema:"启用唤醒门：会话从休眠开始，听到唤醒词才处理用户说的话"`
}

type wireEnabled struct {
	Enabled bool `json:"enabled"`
}

type wireAudioFormat struct {
	Type string `json:"type" jsonschema:"audio/pcm（16 位有符号小端、单声道）"`
	Rate int    `json:"rate" jsonschema:"采样率（Hz）"`
}

type wireAudioAppend struct {
	Type  string `json:"type"`
	Audio string `json:"audio" jsonschema:"base64 编码的 PCM"`
}

type wireTypeOnly struct {
	Type string `json:"type"`
}

type wireTerminalDevices struct {
	Type       string `json:"type"`
	Microphone string `json:"microphone" jsonschema:"麦克风的名字；空 = 没有"`
	Speaker    string `json:"speaker" jsonschema:"音箱的名字；空 = 没有"`
}

type wireMCPMessage struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message" jsonschema:"一条完整的 MCP（JSON-RPC 2.0）消息"`
}

type wireItemTruncate struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id,omitempty" jsonschema:"haro 不填：截最近一次回答"`
	ContentIndex int    `json:"content_index"`
	AudioEndMs   int64  `json:"audio_end_ms" jsonschema:"用户听到了回答的前多少毫秒"`
}

// ——— 服务端事件 ———

type wireSpeechStarted struct {
	Type         string `json:"type"`
	AudioStartMs int64  `json:"audio_start_ms" jsonschema:"在会话的音频时间轴上的位置（毫秒）"`
	ItemID       string `json:"item_id"`
}

type wireSpeechStopped struct {
	Type       string `json:"type"`
	AudioEndMs int64  `json:"audio_end_ms" jsonschema:"语音实际结束的位置（毫秒，不含等静音的时间）"`
	ItemID     string `json:"item_id"`
}

type wireTranscriptionCompleted struct {
	Type       string                 `json:"type"`
	ItemID     string                 `json:"item_id"`
	Transcript string                 `json:"transcript"`
	Haro       *wireTranscriptSpeaker `json:"haro,omitempty" jsonschema:"haro 扩展：声纹比对结果；没比对时不带"`
}

type wireTranscriptSpeaker struct {
	Speaker string `json:"speaker" jsonschema:"认出来的人；没认出来为空"`
	Checked bool   `json:"speaker_checked"`
}

type wireWakeWordDetected struct {
	Type    string  `json:"type"`
	Keyword string  `json:"keyword"`
	AudioMs int64   `json:"audio_ms" jsonschema:"唤醒词结束处在会话音频时间轴上的位置（毫秒）"`
	Score   float64 `json:"score,omitempty" jsonschema:"唤醒词的声纹分数"`
	Level   float64 `json:"level,omitempty" jsonschema:"唤醒词那一段的音量（RMS，0–1）"`
}

type wireTranscriptDelta struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

type wireAudioDelta struct {
	Type  string             `json:"type"`
	Delta string             `json:"delta" jsonschema:"base64 编码的 PCM"`
	Haro  *wireAudioDeltaExt `json:"haro,omitempty" jsonschema:"haro 扩展：这段音频的格式；不带 = 官方默认的 24 kHz 单声道"`
}

type wireAudioDeltaExt struct {
	Format wireOutputFormat `json:"format"`
}

type wireOutputFormat struct {
	Type     string `json:"type" jsonschema:"audio/pcm"`
	Rate     int    `json:"rate"`
	Channels int    `json:"channels"`
}

type wireNotice struct {
	Type string `json:"type"`
	Kind string `json:"kind" jsonschema:"提示的种类，例如 research_done；不认识的种类也照样提示"`
}

type wireResponseCreated struct {
	Type     string `json:"type"`
	Response struct {
		Status ResponseStatus `json:"status" jsonschema:"in_progress"`
	} `json:"response"`
}

type wireResponseDone struct {
	Type     string           `json:"type"`
	Response wireResponseBody `json:"response"`
}

type wireResponseBody struct {
	Status        ResponseStatus     `json:"status" jsonschema:"completed、cancelled、incomplete 或 failed"`
	StatusDetails *wireStatusDetails `json:"status_details,omitempty"`
	Usage         *wireUsage         `json:"usage,omitempty"`
}

type wireStatusDetails struct {
	Reason ResponseReason `json:"reason" jsonschema:"没有正常完成的原因，例如 unknown_speaker、empty_transcript"`
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type wireError struct {
	Type  string        `json:"type"`
	Error wireErrorBody `json:"error"`
}

type wireErrorBody struct {
	Type    string    `json:"type" jsonschema:"invalid_request_error 或 server_error"`
	Code    ErrorCode `json:"code" jsonschema:"稳定的错误码，客户端按它判断"`
	Message string    `json:"message" jsonschema:"给人看的说明；措辞会变，不要拿来判断"`
}

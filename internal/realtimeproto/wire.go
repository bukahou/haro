package realtimeproto

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/bukahou/haro/internal/endpoint"
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
//     只用其中一部分事件。haro-asr 接受第一条 session.update 后回 session.updated，之后才处理音频；
//     不接受时回带码的 error 并断开（码是请求类的 = 这份设置它做不到，原样再连没用）。

// ConversationPath 是对话会话的 WebSocket 路径（haro-server 提供）。
const ConversationPath = "/v1/realtime"

// TranscriptionPath 是转写会话的 WebSocket 路径（haro-asr 提供）；连接地址上带 intent 参数 TranscriptionIntent。
const TranscriptionPath = "/v1/realtime"

// TranscriptionIntent 是转写会话连接地址上 intent 参数的值（照 OpenAI Realtime 的转写会话）。
const TranscriptionIntent = "transcription"

// TranscriptionURL 由转写服务的根地址（http(s)://host:port）拼出转写会话的 WebSocket 地址：
// scheme 换成 ws(s)、接上 TranscriptionPath、带上 intent 参数。
func TranscriptionURL(base string) (string, error) {
	address, err := endpoint.WebSocket(base, TranscriptionPath, url.Values{"intent": {TranscriptionIntent}})
	if err != nil {
		return "", fmt.Errorf("转写服务地址：%w", err)
	}
	return address, nil
}

// TranscriptionSampleRate 是转写会话要求的采样率（识别、唤醒词、声纹的模型都按 16 kHz）。
const TranscriptionSampleRate = 16000

// TranscriptionFormat 是转写会话的音频格式：16 kHz 单声道。
var TranscriptionFormat = AudioFormat{SampleRate: TranscriptionSampleRate, Channels: 1}

// MaxMessageBytes 是一条消息的大小上限，两端读消息时都按它限。
const MaxMessageBytes = 8 << 20

// MaxSessions 是同时最多几条会话，两种会话用同一个数：haro-server 的对话会话、haro-asr 的转写会话。每条对话会话在 haro-asr
// 占一个转写会话，服务端多接下的会话那边开不了识别流，所以两边一样多，满了都在握手时拒绝（too_many_sessions）。
// 每个转写会话自己建一个 Silero VAD（重新载入模型）和唤醒词流，不设上限时连接失控（终端重连出错、局域网里谁都能连）
// 会撑爆原生内存。家里每台终端一个会话（断线重连时新旧两个会短暂重叠），远到不了这个数；定值，没有实测每个会话占多少内存
const MaxSessions = 8

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

	typeSessionUpdated         = "session.updated"
	typeSpeechStarted          = "input_audio_buffer.speech_started"
	typeSpeechStopped          = "input_audio_buffer.speech_stopped"
	typeTranscriptionCompleted = "conversation.item.input_audio_transcription.completed"
	typeTranscriptionFailed    = "conversation.item.input_audio_transcription.failed"
	typeHaroWakeWordDetected   = "haro.wake_word.detected"
	typeHaroWakeWordState      = "haro.wake_word.state"
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
	WakeWord *wireWakeWordSetting `json:"wake_word,omitempty" jsonschema:"启用唤醒门：会话从休眠开始，听到唤醒词才处理用户说的话"`
}

type wireWakeWordSetting struct {
	Enabled bool     `json:"enabled"`
	Phrases []string `json:"phrases,omitempty" jsonschema:"只在 session.updated 里：语音前端认的唤醒词（关键词文件里的显示名）；session.update 里带了就拒绝"`
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
	Type         string      `json:"type"`
	ItemID       string      `json:"item_id,omitempty" jsonschema:"不填：haro 只截最近一次回答（它不发回答的 item_id），填了就拒绝（invalid_event）"`
	ContentIndex int         `json:"content_index"`
	AudioEndMs   millisField `json:"audio_end_ms" jsonschema:"用户听到了回答的前多少毫秒（必填；缺了或为负拒绝：invalid_event）"`
}

// millisField 是必填、不能为负的毫秒数（线上就是一个整数）：解码时记下有没有这个字段，缺了（或是 null）不能当成 0 去执行。
// 生成 Schema 时按 wireTypeSchemas 里写的算。
type millisField struct {
	ms  int64
	set bool
}

func (m *millisField) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil // 和缺了一样
	}
	if err := json.Unmarshal(data, &m.ms); err != nil {
		return err
	}
	m.set = true
	return nil
}

func (m millisField) MarshalJSON() ([]byte, error) { return json.Marshal(m.ms) }

// ——— 服务端事件 ———

type wireSessionUpdated struct {
	Type    string      `json:"type"`
	Session wireSession `json:"session" jsonschema:"对端接受之后的会话设置（和 session.update 的 session 同一形状）"`
}

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
	Type       string               `json:"type"`
	ItemID     string               `json:"item_id"`
	Transcript string               `json:"transcript"`
	Haro       *wireTranscriptExtra `json:"haro,omitempty" jsonschema:"haro 扩展：声纹、语种、唤醒、长度；都没有时不带"`
}

type wireTranscriptionFailed struct {
	Type         string        `json:"type"`
	ItemID       string        `json:"item_id" jsonschema:"识别失败的是哪一句话（和 speech_stopped 的 item_id 一致）"`
	ContentIndex int           `json:"content_index"`
	Error        wireErrorBody `json:"error"`
}

type wireTranscriptExtra struct {
	Speaker     string `json:"speaker" jsonschema:"认出来的人；没认出来为空"`
	Checked     bool   `json:"speaker_checked"`
	CheckFailed bool   `json:"speaker_check_failed,omitempty" jsonschema:"开着声纹，但这句算不出声纹（这时 speaker_checked 为 false、speaker 为空）：和没开声纹不是一回事"`
	Tentative   string `json:"speaker_tentative,omitempty" jsonschema:"叫醒这句的声纹只过了唤醒的门槛、没确认时，像是谁"`
	Language    string `json:"language,omitempty" jsonschema:"识别出的语种，例如 zh、ja、en"`
	Wake        bool   `json:"wake,omitempty" jsonschema:"这句话里叫了唤醒词（叫醒后的第一句）"`
	BargeIn     bool   `json:"barge_in,omitempty" jsonschema:"这句话是在 haro 说话时打断说的（开头可能混着回声）"`
	AudioMs     int64  `json:"audio_ms,omitempty" jsonschema:"这句话的音频有多长（毫秒）"`
}

type wireWakeWordDetected struct {
	Type    string  `json:"type"`
	Keyword string  `json:"keyword"`
	AudioMs int64   `json:"audio_ms" jsonschema:"唤醒词结束处在会话音频时间轴上的位置（毫秒）"`
	Score   float64 `json:"score,omitempty" jsonschema:"唤醒词的声纹分数"`
	Level   float64 `json:"level,omitempty" jsonschema:"唤醒词那一段的音量（RMS，0–1）"`
}

type wireWakeWordState struct {
	Type  string `json:"type"`
	Awake bool   `json:"awake" jsonschema:"处理完请求之后醒着（true）还是睡着（false）"`
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
	Haro *wireResponseCreatedExt `json:"haro,omitempty" jsonschema:"haro 扩展：服务端自己开口时带上；回答主人说的话时不带"`
}

type wireResponseCreatedExt struct {
	Unprompted string `json:"unprompted" jsonschema:"服务端自己开口的缘由，例如 reminder（到点的提醒）；不认识的缘由照样播"`
}

type wireResponseDone struct {
	Type     string               `json:"type"`
	Response wireResponseBody     `json:"response"`
	Haro     *wireResponseDoneExt `json:"haro,omitempty" jsonschema:"haro 扩展：没有要带的时不带"`
}

type wireResponseDoneExt struct {
	EndConversation bool `json:"end_conversation,omitempty" jsonschema:"主人要结束这次对话：终端播完这次回答后请语音前端休眠，被打断就不睡"`
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

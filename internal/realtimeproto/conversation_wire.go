package realtimeproto

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 本文件是「对话会话」在 WebSocket 上的 JSON 格式：终端（haro）↔ 服务端（haro-server），v3 第 3 步。
// 事件名与字段照 OpenAI Realtime 正式版（GA）的子集；和官方不同的地方：
//   - 音频不限 24 kHz：输入格式在 session.update 的 session.audio.input.format 里声明（haro 用 16 kHz）；
//     输出格式随每段 response.output_audio.delta 放在 haro 扩展字段里（语音合成引擎决定，GPT-SoVITS 是 32 kHz）。
//     收到的 delta 没带扩展字段时，按官方默认的 24 kHz 单声道处理。
//   - haro 自己的东西（唤醒词）一律放在 "haro" 键下，官方实现会忽略。
//   - 服务端不发 session.created / response.created 等 haro 用不到的事件；客户端不认识的事件一律忽略。
// 转写会话（haro ↔ haro-asr）的格式在 transcription_wire.go，两者共用服务端事件的解码。

// ConversationPath 是对话会话的 WebSocket 路径（haro-server 提供）。
const ConversationPath = "/v1/realtime"

// DefaultOutputFormat 是官方 Realtime 的输出音频格式；delta 没声明格式时用它。
var DefaultOutputFormat = AudioFormat{SampleRate: 24000, Channels: 1}

// ——— 客户端事件 ———

type wireConversationSessionUpdate struct {
	Type    string `json:"type"`
	Session struct {
		Type         string `json:"type"` // "realtime"
		Instructions string `json:"instructions,omitempty"`
		Audio        struct {
			Input struct {
				Format        *WireAudioFormat `json:"format,omitempty"`
				TurnDetection *TurnDetection   `json:"turn_detection"`
			} `json:"input"`
		} `json:"audio"`
		Haro *WireHaroSessionConfig `json:"haro,omitempty"`
	} `json:"session"`
}

type wireItemCreate struct {
	Type string `json:"type"`
	Item struct {
		Type    string        `json:"type"` // "message"
		Role    string        `json:"role"`
		Content []wireContent `json:"content"`
	} `json:"item"`
}

type wireContent struct {
	Type     string `json:"type"`                // "input_text" | "input_image"
	Text     string `json:"text,omitempty"`      // input_text
	ImageURL string `json:"image_url,omitempty"` // input_image：data:image/jpeg;base64,...
}

type wireTypeOnly struct {
	Type string `json:"type"`
}

type wireTerminalDevices struct {
	Type       string `json:"type"`
	Microphone string `json:"microphone"`
	Speaker    string `json:"speaker"`
	Camera     string `json:"camera"`
}

type wireItemTruncate struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id,omitempty"` // haro 不填：截最近一次回答
	ContentIndex int    `json:"content_index"`
	AudioEndMs   int64  `json:"audio_end_ms"`
}

const jpegDataURIPrefix = "data:image/jpeg;base64,"

// EncodeClientEvent 把终端的事件编码成 JSON。
func EncodeClientEvent(event ClientEvent) ([]byte, error) {
	switch e := event.(type) {
	case SessionUpdate:
		var wire wireConversationSessionUpdate
		wire.Type = e.Type()
		wire.Session.Type = "realtime"
		wire.Session.Instructions = e.Instructions
		if e.InputFormat.SampleRate > 0 {
			if e.InputFormat.Channels > 1 {
				return nil, fmt.Errorf("输入音频只支持单声道，收到 %d 声道", e.InputFormat.Channels)
			}
			wire.Session.Audio.Input.Format = &WireAudioFormat{Type: "audio/pcm", Rate: e.InputFormat.SampleRate}
		}
		wire.Session.Audio.Input.TurnDetection = e.TurnDetection
		if e.WakeWord || e.NoCamera {
			wire.Session.Haro = &WireHaroSessionConfig{}
		}
		if e.WakeWord {
			wire.Session.Haro.WakeWord = &struct {
				Enabled bool `json:"enabled"`
			}{Enabled: true}
		}
		if e.NoCamera {
			wire.Session.Haro.Camera = &struct {
				Enabled bool `json:"enabled"`
			}{Enabled: false}
		}
		return json.Marshal(wire)
	case InputAudioBufferAppend:
		return json.Marshal(NewWireAudioAppend(e.Audio.Data))
	case ConversationItemCreate:
		var wire wireItemCreate
		wire.Type = e.Type()
		wire.Item.Type, wire.Item.Role = "message", e.Item.Role
		for _, part := range e.Item.Content {
			switch part.Type {
			case "input_image":
				wire.Item.Content = append(wire.Item.Content, wireContent{Type: part.Type,
					ImageURL: jpegDataURIPrefix + base64.StdEncoding.EncodeToString(part.ImageJPEG)})
			case "input_text":
				wire.Item.Content = append(wire.Item.Content, wireContent{Type: part.Type, Text: part.Text})
			default:
				return nil, fmt.Errorf("不支持的内容类型 %q", part.Type)
			}
		}
		return json.Marshal(wire)
	case InputAudioBufferCommit, ResponseCreate, ResponseCancel, HaroWakeWordSleep, HaroWakeWordWake,
		HaroPlaybackStarted, HaroPlaybackStopped:
		return json.Marshal(wireTypeOnly{Type: e.Type()})
	case ConversationItemTruncate:
		return json.Marshal(wireItemTruncate{Type: e.Type(), AudioEndMs: e.AudioEndMs})
	case HaroTerminalDevices:
		return json.Marshal(wireTerminalDevices{Type: e.Type(), Microphone: e.Microphone, Speaker: e.Speaker, Camera: e.Camera})
	default:
		return nil, fmt.Errorf("不支持编码的客户端事件 %s", event.Type())
	}
}

// DecodeClientEvent 把终端发来的 JSON 解成事件。input_audio_buffer.append 用 inputFormat 作为音频格式
// （由服务端记住会话里声明的格式）。不认识的事件返回错误（服务端据此回一个 error 事件）。
func DecodeClientEvent(data []byte, inputFormat AudioFormat) (ClientEvent, error) {
	var head wireTypeOnly
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("解析事件: %w", err)
	}
	switch head.Type {
	case "session.update":
		var wire wireConversationSessionUpdate
		if err := json.Unmarshal(data, &wire); err != nil {
			return nil, err
		}
		update := SessionUpdate{Instructions: wire.Session.Instructions, TurnDetection: wire.Session.Audio.Input.TurnDetection}
		if format := wire.Session.Audio.Input.Format; format != nil {
			if format.Type != "" && format.Type != "audio/pcm" {
				return nil, fmt.Errorf("只支持 audio/pcm 输入，收到 %q", format.Type)
			}
			update.InputFormat = AudioFormat{SampleRate: format.Rate, Channels: 1}
		}
		if haro := wire.Session.Haro; haro != nil {
			if haro.WakeWord != nil {
				update.WakeWord = haro.WakeWord.Enabled
			}
			if haro.Camera != nil {
				update.NoCamera = !haro.Camera.Enabled
			}
		}
		return update, nil
	case "input_audio_buffer.append":
		var wire WireAudioAppend
		if err := json.Unmarshal(data, &wire); err != nil {
			return nil, err
		}
		pcm, err := base64.StdEncoding.DecodeString(wire.Audio)
		if err != nil {
			return nil, fmt.Errorf("音频不是合法的 base64: %w", err)
		}
		return InputAudioBufferAppend{Audio: PCMAudio{Format: inputFormat, Data: pcm}}, nil
	case "conversation.item.create":
		var wire wireItemCreate
		if err := json.Unmarshal(data, &wire); err != nil {
			return nil, err
		}
		item := ConversationItem{Role: wire.Item.Role}
		for _, content := range wire.Item.Content {
			switch content.Type {
			case "input_image":
				if !strings.HasPrefix(content.ImageURL, jpegDataURIPrefix) {
					return nil, errors.New("input_image 只支持 data:image/jpeg;base64 形式")
				}
				jpeg, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(content.ImageURL, jpegDataURIPrefix))
				if err != nil {
					return nil, fmt.Errorf("图片不是合法的 base64: %w", err)
				}
				item.Content = append(item.Content, ContentPart{Type: content.Type, ImageJPEG: jpeg})
			case "input_text":
				item.Content = append(item.Content, ContentPart{Type: content.Type, Text: content.Text})
			default:
				return nil, fmt.Errorf("不支持的内容类型 %q", content.Type)
			}
		}
		return ConversationItemCreate{Item: item}, nil
	case "input_audio_buffer.commit":
		return InputAudioBufferCommit{}, nil
	case "response.create":
		return ResponseCreate{}, nil
	case "response.cancel":
		return ResponseCancel{}, nil
	case "haro.wake_word.sleep":
		return HaroWakeWordSleep{}, nil
	case "haro.wake_word.wake":
		return HaroWakeWordWake{}, nil
	case "haro.playback.started":
		return HaroPlaybackStarted{}, nil
	case "haro.playback.stopped":
		return HaroPlaybackStopped{}, nil
	case "haro.terminal.devices":
		var wire wireTerminalDevices
		err := json.Unmarshal(data, &wire)
		return HaroTerminalDevices{Microphone: wire.Microphone, Speaker: wire.Speaker, Camera: wire.Camera}, err
	case "conversation.item.truncate":
		var wire wireItemTruncate
		err := json.Unmarshal(data, &wire)
		return ConversationItemTruncate{AudioEndMs: wire.AudioEndMs}, err
	default:
		return nil, fmt.Errorf("不支持的事件 %q", head.Type)
	}
}

// ——— 服务端事件 ———

type wireTranscriptDelta struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

type wireAudioDelta struct {
	Type  string `json:"type"`
	Delta string `json:"delta"` // base64 pcm16
	Haro  *struct {
		Format wireOutputFormat `json:"format"`
	} `json:"haro,omitempty"`
}

type wireOutputFormat struct {
	Type     string `json:"type"` // "audio/pcm"
	Rate     int    `json:"rate"`
	Channels int    `json:"channels"`
}

type wireNotice struct {
	Type string `json:"type"`
	Kind string `json:"kind"`
}

type wireResponseDone struct {
	Type     string `json:"type"`
	Response struct {
		Status        ResponseStatus `json:"status"`
		StatusDetails *struct {
			Reason string `json:"reason"`
		} `json:"status_details,omitempty"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"response"`
}

// EncodeServerEvent 把服务端的事件编码成 JSON。
func EncodeServerEvent(event ServerEvent) ([]byte, error) {
	switch e := event.(type) {
	case InputAudioBufferSpeechStarted:
		return json.Marshal(WireSpeechStarted{Type: e.Type(), AudioStartMs: e.AudioStartMs, ItemID: e.ItemID})
	case InputAudioBufferSpeechStopped:
		return json.Marshal(WireSpeechStopped{Type: e.Type(), AudioEndMs: e.AudioEndMs, ItemID: e.ItemID})
	case InputAudioTranscriptionCompleted:
		return json.Marshal(NewWireTranscriptionCompleted(e.ItemID, e.Transcript, e.Speaker, e.SpeakerChecked))
	case HaroWakeWordDetected:
		return json.Marshal(WireWakeWordDetected{Type: e.Type(), Keyword: e.Keyword, AudioMs: e.AudioMs})
	case ResponseOutputAudioTranscriptDelta:
		return json.Marshal(wireTranscriptDelta{Type: e.Type(), Delta: e.Delta})
	case ResponseOutputAudioDelta:
		wire := wireAudioDelta{Type: e.Type(), Delta: base64.StdEncoding.EncodeToString(e.Audio.Data)}
		wire.Haro = &struct {
			Format wireOutputFormat `json:"format"`
		}{Format: wireOutputFormat{Type: "audio/pcm", Rate: e.Audio.Format.SampleRate, Channels: e.Audio.Format.Channels}}
		return json.Marshal(wire)
	case ResponseDone:
		var wire wireResponseDone
		wire.Type = e.Type()
		wire.Response.Status = e.Status
		if e.Reason != "" {
			wire.Response.StatusDetails = &struct {
				Reason string `json:"reason"`
			}{Reason: e.Reason}
		}
		if e.Usage != nil {
			wire.Response.Usage = &struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}{InputTokens: e.Usage.InputTokens, OutputTokens: e.Usage.OutputTokens}
		}
		return json.Marshal(wire)
	case HaroNotice:
		return json.Marshal(wireNotice{Type: e.Type(), Kind: e.Kind})
	case Error:
		return json.Marshal(NewWireError("server_error", e.Message))
	default:
		return nil, fmt.Errorf("不支持编码的服务端事件 %s", event.Type())
	}
}

// decodeResponseEvent 解码对话会话特有的服务端事件（被 DecodeServerWireEvent 调用）；不是这几种时 ok=false。
func decodeResponseEvent(eventType string, data []byte) (event ServerEvent, ok bool, err error) {
	switch eventType {
	case "response.output_audio_transcript.delta":
		var wire wireTranscriptDelta
		err := json.Unmarshal(data, &wire)
		return ResponseOutputAudioTranscriptDelta{Delta: wire.Delta}, true, err
	case "response.output_audio.delta":
		var wire wireAudioDelta
		if err := json.Unmarshal(data, &wire); err != nil {
			return nil, true, err
		}
		pcm, err := base64.StdEncoding.DecodeString(wire.Delta)
		if err != nil {
			return nil, true, fmt.Errorf("音频不是合法的 base64: %w", err)
		}
		format := DefaultOutputFormat
		if wire.Haro != nil && wire.Haro.Format.Rate > 0 {
			format = AudioFormat{SampleRate: wire.Haro.Format.Rate, Channels: max(wire.Haro.Format.Channels, 1)}
		}
		return ResponseOutputAudioDelta{Audio: PCMAudio{Format: format, Data: pcm}}, true, nil
	case "haro.notice":
		var wire wireNotice
		err := json.Unmarshal(data, &wire)
		return HaroNotice{Kind: wire.Kind}, true, err
	case "response.done":
		var wire wireResponseDone
		if err := json.Unmarshal(data, &wire); err != nil {
			return nil, true, err
		}
		done := ResponseDone{Status: wire.Response.Status}
		if wire.Response.StatusDetails != nil {
			done.Reason = wire.Response.StatusDetails.Reason
		}
		if wire.Response.Usage != nil {
			done.Usage = &Usage{InputTokens: wire.Response.Usage.InputTokens, OutputTokens: wire.Response.Usage.OutputTokens}
		}
		return done, true, nil
	default:
		return nil, false, nil
	}
}

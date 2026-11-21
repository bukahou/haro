package realtimeproto

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

// 事件的编解码，全协议只有这一处：编码由每个事件的 toWire 给出线上结构，解码查下面两张表。
// 两张表同时是 JSON Schema 的来源（schema.go），加事件时只改这里和 wire.go。

// clientCodec 是一种客户端事件的线上结构（生成 Schema 用的零值）和解码。
type clientCodec struct {
	wire   any
	decode func(data []byte, input AudioFormat) (ClientEvent, error)
}

// serverCodec 是一种服务端事件的线上结构和解码。
type serverCodec struct {
	wire   any
	decode func(data []byte) (ServerEvent, error)
}

var clientEvents = map[string]clientCodec{
	typeSessionUpdate:            {wireSessionUpdate{}, decodeSessionUpdate},
	typeInputAudioBufferAppend:   {wireAudioAppend{}, decodeAudioAppend},
	typeConversationItemTruncate: {wireItemTruncate{}, decodeItemTruncate},
	typeResponseCreate:           typeOnlyClient(ResponseCreate{}),
	typeResponseCancel:           typeOnlyClient(ResponseCancel{}),
	typeHaroWakeWordSleep:        typeOnlyClient(HaroWakeWordSleep{}),
	typeHaroWakeWordWake:         typeOnlyClient(HaroWakeWordWake{}),
	typeHaroPlaybackStarted:      typeOnlyClient(HaroPlaybackStarted{}),
	typeHaroPlaybackStopped:      typeOnlyClient(HaroPlaybackStopped{}),
	typeHaroTerminalDevices:      {wireTerminalDevices{}, decodeTerminalDevices},
	typeHaroMCPMessage:           {wireMCPMessage{}, decodeMCPMessageFromClient},
}

var serverEvents = map[string]serverCodec{
	typeSessionUpdated:         {wireSessionUpdated{}, decodeSessionUpdated},
	typeSpeechStarted:          {wireSpeechStarted{}, decodeSpeechStarted},
	typeSpeechStopped:          {wireSpeechStopped{}, decodeSpeechStopped},
	typeTranscriptionCompleted: {wireTranscriptionCompleted{}, decodeTranscriptionCompleted},
	typeTranscriptionFailed:    {wireTranscriptionFailed{}, decodeTranscriptionFailed},
	typeHaroWakeWordDetected:   {wireWakeWordDetected{}, decodeWakeWordDetected},
	typeHaroWakeWordState:      {wireWakeWordState{}, decodeWakeWordState},
	typeTranscriptDelta:        {wireTranscriptDelta{}, decodeTranscriptDelta},
	typeAudioDelta:             {wireAudioDelta{}, decodeAudioDelta},
	typeResponseCreated:        {wireResponseCreated{}, decodeResponseCreated},
	typeResponseDone:           {wireResponseDone{}, decodeResponseDone},
	typeHaroNotice:             {wireNotice{}, decodeNotice},
	typeError:                  {wireError{}, decodeError},
	typeHaroMCPMessage:         {wireMCPMessage{}, decodeMCPMessageFromServer},
}

// EncodeClientEvent 把客户端事件编码成 JSON。
func EncodeClientEvent(event ClientEvent) ([]byte, error) {
	wire, err := event.toWire()
	if err != nil {
		return nil, err
	}
	return json.Marshal(wire)
}

// EncodeServerEvent 把服务端事件编码成 JSON。
func EncodeServerEvent(event ServerEvent) ([]byte, error) {
	wire, err := event.toWire()
	if err != nil {
		return nil, err
	}
	return json.Marshal(wire)
}

// DecodeClientEvent 把客户端发来的 JSON 解成事件。input 是会话里声明的输入格式（input_audio_buffer.append 用它）。
// 出错时返回带错误码的 *ProtocolError：解不开 → invalid_event，不认识或不支持的事件 → unsupported_event。
func DecodeClientEvent(data []byte, input AudioFormat) (ClientEvent, error) {
	name, err := EventType(data)
	if err != nil {
		return nil, err
	}
	codec, ok := clientEvents[name]
	if !ok {
		return nil, ProtocolErrorf(CodeUnsupportedEvent, "不支持的事件 %q", name)
	}
	event, err := codec.decode(data, input)
	if err != nil {
		return nil, withCode(CodeInvalidEvent, name, err)
	}
	return event, nil
}

// DecodeServerEvent 把服务端发来的 JSON 解成事件。不认识的事件返回 nil, nil：
// 对端比自己新时多出来的事件忽略即可（Realtime 的约定）。
func DecodeServerEvent(data []byte) (ServerEvent, error) {
	name, err := EventType(data)
	if err != nil {
		return nil, err
	}
	codec, ok := serverEvents[name]
	if !ok {
		return nil, nil
	}
	event, err := codec.decode(data)
	if err != nil {
		return nil, withCode(CodeInvalidEvent, name, err)
	}
	return event, nil
}

// EventType 是一条消息的事件名（type 字段）；不是 JSON 对象、type 不是字符串时返回带 invalid_event 码的错误，
// 没有 type 时为空。解码和日志（解不开的事件按名字计数）共用。
//
// 只读到顶层的 type 为止（编码出来的事件 type 都在最前面，后面的音频不用扫）：整条消息只由各事件自己的解码
// 完整解析一次，不先为了取事件名整条解一遍。type 不在最前面时，前面的值跳过（只扫不解）。
func EventType(data []byte) (string, error) {
	decoder := jsontext.NewDecoder(bytes.NewReader(data), // 和 json.Unmarshal 一样宽松：重名的键、不合法的 UTF-8 都放过
		jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if start, err := decoder.ReadToken(); err != nil || start.Kind() != '{' {
		return "", ProtocolErrorf(CodeInvalidEvent, "事件要是 JSON 对象")
	}
	for decoder.PeekKind() == '"' {
		name, err := decoder.ReadToken()
		if err != nil {
			return "", ProtocolErrorf(CodeInvalidEvent, "解析事件: %v", err)
		}
		if name.String() != "type" {
			if err := decoder.SkipValue(); err != nil {
				return "", ProtocolErrorf(CodeInvalidEvent, "解析事件: %v", err)
			}
			continue
		}
		value, err := decoder.ReadToken()
		if err != nil || value.Kind() != '"' {
			return "", ProtocolErrorf(CodeInvalidEvent, "事件的 type 要是字符串")
		}
		return value.String(), nil
	}
	if _, err := decoder.ReadToken(); err != nil { // 没有 type：读到的应是这个对象的结尾
		return "", ProtocolErrorf(CodeInvalidEvent, "解析事件: %v", err)
	}
	return "", nil
}

// withCode 给解码时的错误补上错误码；已经带码的保持原样。
func withCode(code ErrorCode, eventType string, err error) error {
	if _, ok := errors.AsType[*ProtocolError](err); ok {
		return err
	}
	return ProtocolErrorf(code, "%s: %v", eventType, err)
}

// ——— 客户端事件 ———

func (e SessionUpdate) toWire() (any, error) {
	wire := wireSessionUpdate{Type: typeSessionUpdate}
	wire.Session.Type = cmp.Or(e.Kind, SessionRealtime)
	if e.InputFormat.SampleRate > 0 {
		if e.InputFormat.Channels > 1 {
			return nil, fmt.Errorf("输入音频只支持单声道，收到 %d 声道", e.InputFormat.Channels)
		}
		wire.Session.Audio.Input.Format = &wireAudioFormat{Type: "audio/pcm", Rate: e.InputFormat.SampleRate}
	}
	wire.Session.Audio.Input.TurnDetection = e.TurnDetection
	if e.WakeWord {
		wire.Session.Haro = &wireHaroSession{WakeWord: &wireEnabled{Enabled: true}}
	}
	return wire, nil
}

func decodeSessionUpdate(data []byte, _ AudioFormat) (ClientEvent, error) {
	var wire wireSessionUpdate
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	update := SessionUpdate{Kind: wire.Session.Type, TurnDetection: wire.Session.Audio.Input.TurnDetection}
	switch update.Kind {
	case SessionRealtime:
		update.Kind = "" // 对话会话是默认值
	case SessionTranscription:
	default:
		return nil, fmt.Errorf("session.type 只能是 %s 或 %s，收到 %q", SessionRealtime, SessionTranscription, wire.Session.Type)
	}
	if format := wire.Session.Audio.Input.Format; format != nil {
		if format.Type != "" && format.Type != "audio/pcm" {
			return nil, ProtocolErrorf(CodeUnsupportedAudioFormat, "只支持 audio/pcm 输入，收到 %q", format.Type)
		}
		update.InputFormat = AudioFormat{SampleRate: format.Rate, Channels: 1}
	}
	if haro := wire.Session.Haro; haro != nil && haro.WakeWord != nil {
		update.WakeWord = haro.WakeWord.Enabled
	}
	return update, nil
}

func (e InputAudioBufferAppend) toWire() (any, error) {
	return wireAudioAppend{Type: typeInputAudioBufferAppend, Audio: base64.StdEncoding.EncodeToString(e.Audio.Data)}, nil
}

func decodeAudioAppend(data []byte, input AudioFormat) (ClientEvent, error) {
	if input.SampleRate == 0 {
		return nil, ProtocolErrorf(CodeInputFormatRequired, "送音频之前要在 session.update 里声明 audio.input.format")
	}
	var wire wireAudioAppend
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	pcm, err := base64.StdEncoding.DecodeString(wire.Audio)
	if err != nil {
		return nil, fmt.Errorf("音频不是合法的 base64: %w", err)
	}
	return InputAudioBufferAppend{Audio: PCMAudio{Format: input, Data: pcm}}, nil
}

func (e ConversationItemTruncate) toWire() (any, error) {
	return wireItemTruncate{Type: typeConversationItemTruncate, AudioEndMs: e.AudioEndMs}, nil
}

func decodeItemTruncate(data []byte, _ AudioFormat) (ClientEvent, error) {
	var wire wireItemTruncate
	err := json.Unmarshal(data, &wire)
	return ConversationItemTruncate{AudioEndMs: wire.AudioEndMs}, err
}

func (e HaroTerminalDevices) toWire() (any, error) {
	return wireTerminalDevices{Type: typeHaroTerminalDevices, Microphone: e.Microphone, Speaker: e.Speaker}, nil
}

func decodeTerminalDevices(data []byte, _ AudioFormat) (ClientEvent, error) {
	var wire wireTerminalDevices
	err := json.Unmarshal(data, &wire)
	return HaroTerminalDevices{Microphone: wire.Microphone, Speaker: wire.Speaker}, err
}

func (e HaroMCPMessage) toWire() (any, error) {
	if len(e.Message) == 0 {
		return nil, errors.New("haro.mcp.message 没有内容")
	}
	return wireMCPMessage{Type: typeHaroMCPMessage, Message: e.Message}, nil
}

func decodeMCPMessage(data []byte) (HaroMCPMessage, error) {
	var wire wireMCPMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		return HaroMCPMessage{}, err
	}
	if len(wire.Message) == 0 {
		return HaroMCPMessage{}, errors.New("haro.mcp.message 没有 message")
	}
	return HaroMCPMessage{Message: wire.Message}, nil
}

func decodeMCPMessageFromClient(data []byte, _ AudioFormat) (ClientEvent, error) {
	return decodeMCPMessage(data)
}
func decodeMCPMessageFromServer(data []byte) (ServerEvent, error) { return decodeMCPMessage(data) }

// 只有事件名、没有内容的事件。
func (e ResponseCreate) toWire() (any, error)      { return wireTypeOnly{Type: e.Type()}, nil }
func (e ResponseCancel) toWire() (any, error)      { return wireTypeOnly{Type: e.Type()}, nil }
func (e HaroWakeWordSleep) toWire() (any, error)   { return wireTypeOnly{Type: e.Type()}, nil }
func (e HaroWakeWordWake) toWire() (any, error)    { return wireTypeOnly{Type: e.Type()}, nil }
func (e HaroPlaybackStarted) toWire() (any, error) { return wireTypeOnly{Type: e.Type()}, nil }
func (e HaroPlaybackStopped) toWire() (any, error) { return wireTypeOnly{Type: e.Type()}, nil }

// typeOnlyClient：没有内容的事件同样完整解析一次（EventType 只读到 type 为止），整条不是合法 JSON 的照样拒绝。
func typeOnlyClient(event ClientEvent) clientCodec {
	return clientCodec{wireTypeOnly{}, func(data []byte, _ AudioFormat) (ClientEvent, error) {
		var wire wireTypeOnly
		return event, json.Unmarshal(data, &wire)
	}}
}

// ——— 服务端事件 ———

// session.updated 的 session 和 session.update 的同一形状：编解码借用 session.update 的那一份。
func (e SessionUpdated) toWire() (any, error) {
	update, err := e.Session.toWire()
	if err != nil {
		return nil, err
	}
	return wireSessionUpdated{Type: typeSessionUpdated, Session: update.(wireSessionUpdate).Session}, nil
}

func decodeSessionUpdated(data []byte) (ServerEvent, error) {
	update, err := decodeSessionUpdate(data, AudioFormat{})
	if err != nil {
		return nil, err
	}
	return SessionUpdated{Session: update.(SessionUpdate)}, nil
}

func (e InputAudioBufferSpeechStarted) toWire() (any, error) {
	return wireSpeechStarted{Type: typeSpeechStarted, AudioStartMs: e.AudioStartMs, ItemID: e.ItemID}, nil
}

func decodeSpeechStarted(data []byte) (ServerEvent, error) {
	var wire wireSpeechStarted
	err := json.Unmarshal(data, &wire)
	return InputAudioBufferSpeechStarted{AudioStartMs: wire.AudioStartMs, ItemID: wire.ItemID}, err
}

func (e InputAudioBufferSpeechStopped) toWire() (any, error) {
	return wireSpeechStopped{Type: typeSpeechStopped, AudioEndMs: e.AudioEndMs, ItemID: e.ItemID}, nil
}

func decodeSpeechStopped(data []byte) (ServerEvent, error) {
	var wire wireSpeechStopped
	err := json.Unmarshal(data, &wire)
	return InputAudioBufferSpeechStopped{AudioEndMs: wire.AudioEndMs, ItemID: wire.ItemID}, err
}

func (e InputAudioTranscriptionCompleted) toWire() (any, error) {
	if err := checkSpeakerResult(e.SpeakerCheckFailed, e.SpeakerChecked, e.Speaker); err != nil {
		return nil, err
	}
	wire := wireTranscriptionCompleted{Type: typeTranscriptionCompleted, ItemID: e.ItemID, Transcript: e.Transcript}
	extra := wireTranscriptExtra{Speaker: e.Speaker, Checked: e.SpeakerChecked, CheckFailed: e.SpeakerCheckFailed,
		Tentative: e.SpeakerTentative, Language: e.Language, Wake: e.Wake, BargeIn: e.BargeIn, AudioMs: e.AudioMs}
	if extra != (wireTranscriptExtra{}) {
		wire.Haro = &extra
	}
	return wire, nil
}

func decodeTranscriptionCompleted(data []byte) (ServerEvent, error) {
	var wire wireTranscriptionCompleted
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	completed := InputAudioTranscriptionCompleted{ItemID: wire.ItemID, Transcript: wire.Transcript}
	if extra := wire.Haro; extra != nil {
		if err := checkSpeakerResult(extra.CheckFailed, extra.Checked, extra.Speaker); err != nil {
			return nil, err
		}
		completed.Speaker, completed.SpeakerChecked, completed.SpeakerTentative = extra.Speaker, extra.Checked, extra.Tentative
		completed.SpeakerCheckFailed = extra.CheckFailed
		completed.Language, completed.Wake, completed.BargeIn, completed.AudioMs = extra.Language, extra.Wake, extra.BargeIn, extra.AudioMs
	}
	return completed, nil
}

// checkSpeakerResult：声纹的三种结果（没比对 / 比对过 / 算不出来）只能是其中一种，算不出来时不会同时比对过、认出了谁。
func checkSpeakerResult(failed, checked bool, speaker string) error {
	if failed && (checked || speaker != "") {
		return errors.New("speaker_check_failed 时 speaker_checked 要是 false、speaker 要为空")
	}
	return nil
}

func (e InputAudioTranscriptionFailed) toWire() (any, error) {
	code := cmp.Or(e.Code, CodeTranscriptionFailed)
	return wireTranscriptionFailed{Type: typeTranscriptionFailed, ItemID: e.ItemID,
		Error: wireErrorBody{Type: code.errorType(), Code: code, Message: e.Message}}, nil
}

func decodeTranscriptionFailed(data []byte) (ServerEvent, error) {
	var wire wireTranscriptionFailed
	err := json.Unmarshal(data, &wire)
	return InputAudioTranscriptionFailed{ItemID: wire.ItemID, Code: wire.Error.Code, Message: wire.Error.Message}, err
}

func (e HaroWakeWordDetected) toWire() (any, error) {
	return wireWakeWordDetected{Type: typeHaroWakeWordDetected, Keyword: e.Keyword, AudioMs: e.AudioMs,
		Score: e.Score, Level: e.Level}, nil
}

func decodeWakeWordDetected(data []byte) (ServerEvent, error) {
	var wire wireWakeWordDetected
	err := json.Unmarshal(data, &wire)
	return HaroWakeWordDetected{Keyword: wire.Keyword, AudioMs: wire.AudioMs, Score: wire.Score, Level: wire.Level}, err
}

func (e HaroWakeWordState) toWire() (any, error) {
	return wireWakeWordState{Type: typeHaroWakeWordState, Awake: e.Awake}, nil
}

func decodeWakeWordState(data []byte) (ServerEvent, error) {
	var wire wireWakeWordState
	err := json.Unmarshal(data, &wire)
	return HaroWakeWordState{Awake: wire.Awake}, err
}

func (e ResponseOutputAudioTranscriptDelta) toWire() (any, error) {
	return wireTranscriptDelta{Type: typeTranscriptDelta, Delta: e.Delta}, nil
}

func decodeTranscriptDelta(data []byte) (ServerEvent, error) {
	var wire wireTranscriptDelta
	err := json.Unmarshal(data, &wire)
	return ResponseOutputAudioTranscriptDelta{Delta: wire.Delta}, err
}

func (e ResponseOutputAudioDelta) toWire() (any, error) {
	return wireAudioDelta{Type: typeAudioDelta, Delta: base64.StdEncoding.EncodeToString(e.Audio.Data),
		Haro: &wireAudioDeltaExt{Format: wireOutputFormat{Type: "audio/pcm", Rate: e.Audio.Format.SampleRate,
			Channels: e.Audio.Format.Channels}}}, nil
}

func decodeAudioDelta(data []byte) (ServerEvent, error) {
	var wire wireAudioDelta
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	pcm, err := base64.StdEncoding.DecodeString(wire.Delta)
	if err != nil {
		return nil, fmt.Errorf("音频不是合法的 base64: %w", err)
	}
	format := DefaultOutputFormat
	if wire.Haro != nil && wire.Haro.Format.Rate > 0 {
		format = AudioFormat{SampleRate: wire.Haro.Format.Rate, Channels: max(wire.Haro.Format.Channels, 1)}
	}
	return ResponseOutputAudioDelta{Audio: PCMAudio{Format: format, Data: pcm}}, nil
}

func (e ResponseCreated) toWire() (any, error) {
	wire := wireResponseCreated{Type: typeResponseCreated}
	wire.Response.Status = StatusInProgress
	return wire, nil
}

func decodeResponseCreated(data []byte) (ServerEvent, error) {
	var wire wireResponseCreated
	return ResponseCreated{}, json.Unmarshal(data, &wire)
}

func (e ResponseDone) toWire() (any, error) {
	wire := wireResponseDone{Type: typeResponseDone, Response: wireResponseBody{Status: e.Status}}
	if e.Reason != "" {
		wire.Response.StatusDetails = &wireStatusDetails{Reason: e.Reason}
	}
	if e.Usage != nil {
		wire.Response.Usage = &wireUsage{InputTokens: e.Usage.InputTokens, OutputTokens: e.Usage.OutputTokens}
	}
	return wire, nil
}

func decodeResponseDone(data []byte) (ServerEvent, error) {
	var wire wireResponseDone
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	done := ResponseDone{Status: wire.Response.Status}
	if wire.Response.StatusDetails != nil {
		done.Reason = wire.Response.StatusDetails.Reason
	}
	if wire.Response.Usage != nil {
		done.Usage = &Usage{InputTokens: wire.Response.Usage.InputTokens, OutputTokens: wire.Response.Usage.OutputTokens}
	}
	return done, nil
}

func (e HaroNotice) toWire() (any, error) { return wireNotice{Type: typeHaroNotice, Kind: e.Kind}, nil }

func decodeNotice(data []byte) (ServerEvent, error) {
	var wire wireNotice
	err := json.Unmarshal(data, &wire)
	return HaroNotice{Kind: wire.Kind}, err
}

func (e Error) toWire() (any, error) {
	code := cmp.Or(e.Code, CodeServerError)
	return wireError{Type: typeError, Error: wireErrorBody{Type: code.errorType(), Code: code, Message: e.Message}}, nil
}

func decodeError(data []byte) (ServerEvent, error) {
	var wire wireError
	err := json.Unmarshal(data, &wire)
	return Error{Code: wire.Error.Code, Message: wire.Error.Message}, err
}

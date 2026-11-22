package realtimeproto

import (
	"cmp"
	"errors"
	"fmt"
)

// ErrorCode 是错误事件里的错误码：客户端按它判断该怎么办，不看说明文字。
// 可以新增；改名或改含义属于不兼容的变更，要升协议版本。
type ErrorCode string

const (
	CodeInvalidEvent               ErrorCode = "invalid_event"                // 事件解不开，或字段不合法
	CodeUnsupportedEvent           ErrorCode = "unsupported_event"            // 不认识、或本协议不支持的事件
	CodeInputFormatRequired        ErrorCode = "input_format_required"        // 送音频之前没在 session.update 里声明输入格式
	CodeUnsupportedAudioFormat     ErrorCode = "unsupported_audio_format"     // 音频格式不受支持
	CodeNoTranscriptionStream      ErrorCode = "no_transcription_stream"      // 识别流没开（session.update 没带 turn_detection）或已断开
	CodeTranscriptionFailed        ErrorCode = "transcription_failed"         // 识别服务出错或断开
	CodeResponseInProgress         ErrorCode = "response_in_progress"         // 已有进行中的回答
	CodeUnsupportedFeature         ErrorCode = "unsupported_feature"          // 对端没有这项能力（例如没配唤醒词模型却要求唤醒门）
	CodeUnsupportedProtocolVersion ErrorCode = "unsupported_protocol_version" // 握手时报的协议版本不受支持
	CodeTooManySessions            ErrorCode = "too_many_sessions"            // 对端同时进行的会话到了上限：过一会儿再试
	CodeServerError                ErrorCode = "server_error"                 // 服务端自己的问题
)

// errorType 是 Realtime 错误事件里的 error.type：请求有问题，还是服务端自己的问题。
func (c ErrorCode) errorType() string {
	switch c {
	case CodeServerError, CodeTranscriptionFailed, CodeTooManySessions:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// ServerSide 表示错误出在对端自己（error.type = server_error）：同样的请求过一会儿再发可能就好了。
// 其他码是请求本身的问题，原样再发也一样被拒。没有码的按 server_error（和编码时一样）。
func (c ErrorCode) ServerSide() bool {
	return cmp.Or(c, CodeServerError).errorType() == "server_error"
}

// ProtocolError 是带错误码的 error。处理事件时返回它，连接层把码原样发给对端；其他 error 一律按 server_error。
type ProtocolError struct {
	Code    ErrorCode
	Message string
}

func (e *ProtocolError) Error() string { return e.Message }

// ProtocolErrorf 构造带错误码的 error。
func ProtocolErrorf(code ErrorCode, format string, args ...any) error {
	return &ProtocolError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorEventFor 把处理某个事件时出的错换成发给对端的错误事件：带码的保留码，其他按 server_error。
// eventType 为空时不加前缀。
func ErrorEventFor(eventType string, err error) Error {
	code := CodeServerError
	if protocolErr, ok := errors.AsType[*ProtocolError](err); ok {
		code = protocolErr.Code
	}
	message := err.Error()
	if eventType != "" {
		message = eventType + ": " + message
	}
	return Error{Code: code, Message: message}
}

// ErrIncompatibleProtocol 表示两端的协议版本不兼容（握手被拒，或者对端没有选我们的版本）：
// 重连没用，要把终端和服务端升到同一版本。
var ErrIncompatibleProtocol = errors.New("协议版本和对端不兼容")

// HTTPError 是 HTTP 接口出错时的响应体，形状同 Realtime 错误事件里的 error。
type HTTPError struct {
	Error HTTPErrorDetail `json:"error"`
}

// HTTPErrorDetail 是出错的说明。
type HTTPErrorDetail struct {
	Type    string    `json:"type"`           // invalid_request_error 或 server_error
	Code    ErrorCode `json:"code,omitempty"` // 稳定的错误码（有的话）：调用方按它判断
	Message string    `json:"message"`        // 给人看的说明
	// SupportedProtocols 是服务端支持的协议版本（握手时版本不合被拒才带）。
	SupportedProtocols []string `json:"supported_protocols,omitempty"`
}

// NewHTTPError 按 HTTP 状态码构造出错的响应体：5xx 是服务端自己的问题，其余是请求的问题。
func NewHTTPError(status int, code ErrorCode, message string) HTTPError {
	errorType := "invalid_request_error"
	if status >= 500 {
		errorType = "server_error"
	}
	return HTTPError{Error: HTTPErrorDetail{Type: errorType, Code: code, Message: message}}
}

// ResponseReason 是回答没有正常完成的原因（response.done 的 status_details.reason）。
type ResponseReason string

const (
	ReasonNoInputAudio        ResponseReason = "no_input_audio"       // 没收到这句话的音频
	ReasonUnknownSpeaker      ResponseReason = "unknown_speaker"      // 不是登记的人说的
	ReasonEmptyTranscript     ResponseReason = "empty_transcript"     // 没识别出字
	ReasonTranscriptionFailed ResponseReason = "transcription_failed" // 识别出错
	ReasonGenerationFailed    ResponseReason = "generation_failed"    // 模型生成回答出错
	ReasonMaxOutputTokens     ResponseReason = "max_output_tokens"    // 回答到了模型的输出上限，没说完（status = incomplete）
	ReasonSynthesisFailed     ResponseReason = "synthesis_failed"     // 语音合成出错
	ReasonServerDisconnected  ResponseReason = "server_disconnected"  // 终端侧补的：回答进行中和服务端断了（不在线上传输）
)

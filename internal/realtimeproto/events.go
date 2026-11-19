// Package realtimeproto 定义终端与服务端之间的事件：取 OpenAI Realtime 事件模型的子集。
//
// 终端（assistant）与服务端（brain）只认这里的 Go 类型；跨机器时由本包的 WebSocket + JSON 编解码（*_wire.go）传递，
// 两边的代码不用关心传输方式。每个事件的 Type() 返回 Realtime 正式版的事件名。
package realtimeproto

import "time"

// ClientEvent 是终端发给服务端的事件。
type ClientEvent interface {
	Type() string
	clientEvent()
}

// ServerEvent 是服务端发给终端的事件。
type ServerEvent interface {
	Type() string
	serverEvent()
}

// ——— 终端 → 服务端 ———

// SessionUpdate 更新会话配置。
type SessionUpdate struct {
	Instructions string
	// TurnDetection 非 nil 且识别流还没开时，服务端打开流式识别，由服务端侧的 VAD 判断说话开始与结束；
	// nil 表示这次不改（一直没开过识别流时，送来的音频会被拒绝）。
	TurnDetection *TurnDetection
	// WakeWord 为 true 时启用唤醒门（haro 扩展）：会话从休眠开始，听到唤醒词才处理用户说的话。
	WakeWord bool
	// InputFormat 是之后 input_audio_buffer.append 的音频格式（走 WebSocket 时声明在会话里）。
	// 进程内可以不填：每段音频本身带格式。
	InputFormat AudioFormat
	// NoCamera 为 true 表示这个终端没有摄像头（haro 扩展）：服务端不提供「看画面」工具。
	// 默认 false = 有摄像头（不声明时保持原来的行为）。
	NoCamera bool
}

// InputAudioBufferAppend 往识别流送一段 PCM（终端录音每 20ms 一块）。识别流没开时服务端报错，不暂存音频。
type InputAudioBufferAppend struct {
	Audio PCMAudio
}

// InputAudioBufferCommit 是 Realtime 的手动提交（turn_detection: null 时由客户端发出）。haro 只由服务端 VAD 判断说完，
// 服务端收到一律报错拒绝；保留这个类型是为了认得出它、明确拒绝。
type InputAudioBufferCommit struct{}

// ConversationItemCreate 往对话里加一条内容（终端用它附带当前画面）。
type ConversationItemCreate struct {
	Item ConversationItem
}

// ResponseCreate 请服务端基于当前对话生成回答。
type ResponseCreate struct{}

// ResponseCancel 取消正在生成的回答（被打断、终端退出时发出）。
type ResponseCancel struct{}

// HaroWakeWordSleep 让语音前端进入休眠：只认唤醒词（haro 扩展；终端在对话窗口结束时发出）。
type HaroWakeWordSleep struct{}

// HaroWakeWordWake 让语音前端立即进入唤醒状态（haro 扩展；手动唤醒，例如按回车）。
type HaroWakeWordWake struct{}

// HaroPlaybackStarted 表示终端开始播放回答（haro 扩展，用于打断）：语音前端只听唤醒词，
// 听到就报 haro.wake_word.detected（终端据此停播、取消回答）；其他声音（主要是 haro 自己的回声）一概不理。
type HaroPlaybackStarted struct{}

// HaroPlaybackStopped 表示终端播完了（含回声消散的等待）：语音前端恢复正常收听。
type HaroPlaybackStopped struct{}

func (HaroPlaybackStarted) Type() string { return "haro.playback.started" }
func (HaroPlaybackStopped) Type() string { return "haro.playback.stopped" }
func (HaroPlaybackStarted) clientEvent() {}
func (HaroPlaybackStopped) clientEvent() {}

// HaroTerminalDevices 是终端现在有哪些设备（haro 扩展）：连上时发一次，之后有变化再发。
// 名字为空 = 没有这个设备。没有音箱时服务端不再合成语音，只发文字（终端写进日志）。
type HaroTerminalDevices struct {
	Microphone string
	Speaker    string
	Camera     string
}

func (HaroTerminalDevices) Type() string { return "haro.terminal.devices" }
func (HaroTerminalDevices) clientEvent() {}

// ConversationItemTruncate 表示用户只听到了回答的前 AudioEndMs 毫秒（被打断时由终端发出）：
// 服务端只把听到的部分记进历史。与 Realtime 的 conversation.item.truncate 一致（haro 只截最近一次回答，不带 item_id）。
type ConversationItemTruncate struct {
	AudioEndMs int64
}

func (ConversationItemTruncate) Type() string { return "conversation.item.truncate" }
func (ConversationItemTruncate) clientEvent() {}

func (HaroWakeWordSleep) Type() string { return "haro.wake_word.sleep" }
func (HaroWakeWordWake) Type() string  { return "haro.wake_word.wake" }
func (HaroWakeWordSleep) clientEvent() {}
func (HaroWakeWordWake) clientEvent()  {}

func (SessionUpdate) Type() string          { return "session.update" }
func (InputAudioBufferAppend) Type() string { return "input_audio_buffer.append" }
func (InputAudioBufferCommit) Type() string { return "input_audio_buffer.commit" }
func (ConversationItemCreate) Type() string { return "conversation.item.create" }
func (ResponseCreate) Type() string         { return "response.create" }
func (ResponseCancel) Type() string         { return "response.cancel" }

func (SessionUpdate) clientEvent()          {}
func (InputAudioBufferAppend) clientEvent() {}
func (InputAudioBufferCommit) clientEvent() {}
func (ConversationItemCreate) clientEvent() {}
func (ResponseCreate) clientEvent()         {}
func (ResponseCancel) clientEvent()         {}

// ——— 服务端 → 终端 ———

// HaroWakeWordDetected 表示听到了唤醒词，语音前端已进入唤醒状态（haro 扩展）。
type HaroWakeWordDetected struct {
	Keyword string
	AudioMs int64 // 唤醒词结束处在会话音频时间轴上的位置
	// 唤醒质量：几台终端同时听到时，由质量最高的那台回答
	Score float64 // 唤醒词的声纹分数（没比对声纹时为 0）
	Level float64 // 唤醒词那一段的音量（RMS，0–1；离得近的那台大）
}

func (HaroWakeWordDetected) Type() string { return "haro.wake_word.detected" }
func (HaroWakeWordDetected) serverEvent() {}

// HaroNotice 是服务端给终端的一个提示（haro 扩展）：终端播一声提示音、不说话（服务端还不能主动开口）。
// 例如后台调查查完了：主人听到后叫醒 haro，它先说结论。
type HaroNotice struct {
	Kind string // NoticeResearchDone 等；终端不认识的种类也照样响一声
}

// NoticeResearchDone：后台调查结束了（查完、没查完或失败）。
const NoticeResearchDone = "research_done"

func (HaroNotice) Type() string { return "haro.notice" }
func (HaroNotice) serverEvent() {}

// InputAudioBufferSpeechStarted 表示 VAD 检测到用户开始说话（server_vad 模式）。
type InputAudioBufferSpeechStarted struct {
	AudioStartMs int64 // 从会话开始累计的音频时间轴上的位置
	ItemID       string
}

// InputAudioBufferSpeechStopped 表示 VAD 判定用户说完了（server_vad 模式）。
type InputAudioBufferSpeechStopped struct {
	AudioEndMs int64 // 语音实际结束的位置（不含等静音的时间）
	ItemID     string
}

// InputAudioTranscriptionCompleted 是用户这句话的识别结果。
type InputAudioTranscriptionCompleted struct {
	ItemID     string
	Transcript string
	// 声纹（haro 扩展）：SpeakerChecked 为 true 时比对过，Speaker 是认出来的人（没认出来为空）。
	// 没有登记声纹时不比对（SpeakerChecked = false）。
	Speaker        string
	SpeakerChecked bool
}

// ResponseOutputAudioTranscriptDelta 是接下来那段音频对应的文字（一次一整句，在这句的第一块音频之前发）。
type ResponseOutputAudioTranscriptDelta struct {
	Delta string
}

// ResponseOutputAudioDelta 是一段回答音频（PCM，边合成边发）。
type ResponseOutputAudioDelta struct {
	Audio PCMAudio
}

// ResponseDone 表示本次回答结束；每个 ResponseCreate 恰好对应一个 ResponseDone。
type ResponseDone struct {
	Status ResponseStatus
	Reason string // Status 不是 completed 时的原因，例如 "empty_transcript"
	Usage  *Usage
}

// Error 是服务端侧的错误。出错的回答之后仍会有一个 Status=failed 的 ResponseDone。
type Error struct {
	Message string
}

func (InputAudioBufferSpeechStarted) Type() string { return "input_audio_buffer.speech_started" }
func (InputAudioBufferSpeechStopped) Type() string { return "input_audio_buffer.speech_stopped" }
func (InputAudioTranscriptionCompleted) Type() string {
	return "conversation.item.input_audio_transcription.completed"
}
func (ResponseOutputAudioTranscriptDelta) Type() string {
	return "response.output_audio_transcript.delta"
}
func (ResponseOutputAudioDelta) Type() string { return "response.output_audio.delta" }
func (ResponseDone) Type() string             { return "response.done" }
func (Error) Type() string                    { return "error" }

func (InputAudioBufferSpeechStarted) serverEvent()      {}
func (InputAudioBufferSpeechStopped) serverEvent()      {}
func (InputAudioTranscriptionCompleted) serverEvent()   {}
func (ResponseOutputAudioTranscriptDelta) serverEvent() {}
func (ResponseOutputAudioDelta) serverEvent()           {}
func (ResponseDone) serverEvent()                       {}
func (Error) serverEvent()                              {}

// ——— 公共数据类型 ———

// TurnDetection 对应 Realtime 的 turn_detection。
//   - server_vad：静音持续 SilenceDurationMs 就算说完
//   - semantic_vad：短暂静音后由模型（Smart Turn）判断语气上说完没有；没说完就继续等，最长按 Eagerness 决定
type TurnDetection struct {
	Type              string  `json:"type"`                          // "server_vad" | "semantic_vad"
	Eagerness         string  `json:"eagerness,omitempty"`           // semantic_vad："low" | "medium" | "high" | "auto"
	Threshold         float64 `json:"threshold,omitempty"`           // 语音概率阈值，0–1
	PrefixPaddingMs   int     `json:"prefix_padding_ms,omitempty"`   // 识别时带上语音开始前的这段音频
	SilenceDurationMs int     `json:"silence_duration_ms,omitempty"` // 静音持续多久算说完
	CreateResponse    *bool   `json:"create_response,omitempty"`     // haro 固定为 false：由终端带上画面后再请求回答
}

// ResponseStatus 对应 Realtime response.status。
type ResponseStatus string

const (
	StatusCompleted  ResponseStatus = "completed"
	StatusCancelled  ResponseStatus = "cancelled"
	StatusIncomplete ResponseStatus = "incomplete"
	StatusFailed     ResponseStatus = "failed"
)

// Usage 是一次回答的 token 用量。
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ConversationItem 是对话中的一条内容。
type ConversationItem struct {
	Role    string // "user"
	Content []ContentPart
}

// ContentPart 是一条内容里的一部分。
type ContentPart struct {
	Type      string // "input_text" | "input_image"
	Text      string
	ImageJPEG []byte
}

// AudioFormat 描述 PCM 格式。样本一律是 16 位有符号小端（s16le）。
type AudioFormat struct {
	SampleRate int
	Channels   int
}

// BytesPerSecond 返回该格式每秒的字节数。
func (f AudioFormat) BytesPerSecond() int { return f.SampleRate * f.Channels * 2 }

// PCMAudio 是一段带格式的 PCM 数据。
type PCMAudio struct {
	Format AudioFormat
	Data   []byte
}

// Duration 返回这段音频的时长。
func (a PCMAudio) Duration() time.Duration {
	perSecond := a.Format.BytesPerSecond()
	if perSecond == 0 {
		return 0
	}
	return time.Duration(len(a.Data)) * time.Second / time.Duration(perSecond)
}

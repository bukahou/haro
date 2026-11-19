// Package brain 是 haro 的服务端：识别 → 模型流式回答 → 按句切分 → 合成。
//
// 对外只暴露 Session（经 realtimeproto 事件与终端交互）；本文件定义服务端依赖的各段能力（Go interface），
// 实现在 speech / llm 包，由 cmd/haro 组装注入。v3 服务端与终端分机部署时，本包不改，只在外面套 WebSocket。
package brain

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// ChatModel 根据完整的消息列表（系统提示词 + 历史 + 本轮输入 + 工具往来）流式给出回答。
type ChatModel interface {
	// StreamReply 逐段产出可见文字与工具调用；调用方 range 拉取，提前 break 即中止生成。
	// tools 为空表示这一轮不允许调用工具。最后一个 ReplyDelta 带 Usage。
	StreamReply(ctx context.Context, messages []ChatMessage, tools []ToolSpec) iter.Seq2[ReplyDelta, error]
	// Warmup 预先把模型加载进显存，避免第一轮对话等几十秒。
	Warmup(ctx context.Context) error
}

// FrameExpert 是看画面的幕后专家（可选）：设了它，look_at_camera 不把画面交给前台模型，
// 而是把画面和主人这一轮的原话交给专家，前台拿到的是专家的判断（文字），再用自己的口吻转述。
// 专家出错时退回把画面直接交给前台。
type FrameExpert interface {
	ExamineFrame(ctx context.Context, frame []byte, request string) (string, error)
}

// SpokenError 是可以直接念给主人的错误（例如「这个月的额度用完了」）：模型或工具返回它时，
// 服务端把 SpokenMessage 说出来，而不是只报「出错了」。用 errors.As 判断。
type SpokenError interface {
	error
	SpokenMessage() string
}

// ProfileSource 给出主人资料：全家共用的 + 说话人自己的事实与偏好，外部注入（不在公开代码里）。
// 放在系统提示词里内置规矩之后、性格之前；很少变，所以不影响提示词缓存。没有资料时返回空。
type ProfileSource interface {
	Profile(person string) string
}

// Briefer 在每轮对话前给模型补充即时情况（时间、位置、额度提醒、后台调查的任务板……）。
// 返回的几行附在这一轮主人的话前面（不放系统提示词：见 Session.userTurn）、不进历史；person 是这一轮说话人的对话名。
// 每轮都会调用：不要在这里做网络请求。
type Briefer interface {
	Brief(person string) []string
}

// SpeechSynthesizer 把一句话合成为语音（在内存里传，不落临时文件）。
// 边合成边产出：第一段声音一到就交给调用方，不等整句合成完。
// 调用方 range 拉取，提前 break 即中止合成。
type SpeechSynthesizer interface {
	Synthesize(ctx context.Context, utterance Utterance) iter.Seq2[realtimeproto.PCMAudio, error]
}

// Utterance 是要合成的一句话：文字（情绪标记已取出）+ 情绪 + 语言。
// 情绪怎么体现由各语音合成实现决定（GPT-SoVITS 选参考录音；不支持情绪的实现忽略它）。
type Utterance struct {
	Text     string
	Emotion  Emotion
	Language Language
}

// Emotion 是一句话的情绪（与语音合成引擎无关的一层）。空值 = 平常。
type Emotion string

const (
	EmotionDefault Emotion = ""        // 平常
	EmotionTease   Emotion = "tease"   // 调侃
	EmotionLaugh   Emotion = "laugh"   // 笑
	EmotionExcited Emotion = "excited" // 激动
	EmotionGentle  Emotion = "gentle"  // 温柔
	EmotionWhisper Emotion = "whisper" // 轻声
	EmotionFlirt   Emotion = "flirt"   // 俏皮暧昧的语气（给需要的性格用）
)

// Language 是一句话的语言（语音合成需要）。
type Language string

const (
	LanguageChinese  Language = "zh" // 中文（可夹英文）
	LanguageJapanese Language = "ja" // 日语（可夹英文）
)

// TextSegmenter 把流式文字切成适合逐句合成的句子。每次回答用一个新实例（有内部状态）。
type TextSegmenter interface {
	// Push 喂入新文字，返回已经可以合成的完整句子（可能为空）。
	Push(text string) []string
	// Flush 在文字结束时调用，返回剩余内容（不会返回空句）。
	Flush() []string
}

// ChatRole 是对话消息的角色。
type ChatRole string

const (
	RoleSystem    ChatRole = "system"
	RoleUser      ChatRole = "user"
	RoleAssistant ChatRole = "assistant"
	RoleTool      ChatRole = "tool"
)

// ChatMessage 是一条对话消息。Image 只在本轮输入里携带，历史里不保留图片。
// json 标签：对话存盘用；历史里不放图片。
type ChatMessage struct {
	Role       ChatRole   `json:"role"`
	Text       string     `json:"text,omitempty"`
	Image      []byte     `json:"-"`                      // JPEG；nil 表示没有图片
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant：这一轮模型要求调用的工具
	ToolName   string     `json:"tool_name,omitempty"`    // tool：这条是哪个工具的结果
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool：对应哪一次调用（ToolCall.ID）
	// Escalate：这一句交给更强的模型重答（前台模型调用了 ask_stronger_model），选模型的包装层据此换模型。只标在本轮输入上，不存盘
	Escalate bool `json:"-"`
}

// ReplyDelta 是模型流式输出的一段。
type ReplyDelta struct {
	Text      string      // 新增的可见文字（可能为空）
	ToolCalls []ToolCall  // 模型要求调用的工具（可能为空）
	Usage     *ReplyUsage // 只在最后一段上有
}

// ToolSpec 描述一个可以给模型用的工具。
type ToolSpec struct {
	Name                 string
	Description          string
	Parameters           json.RawMessage // 参数的 JSON Schema
	RequiresConfirmation bool            // 会删除或修改数据（MCP destructiveHint）：执行前要用户口头确认
}

// ToolCall 是模型发起的一次工具调用。ID 在一次会话内唯一（模型没给时由服务端补上），
// 用来把调用和结果对上；各家模型对 ID 格式的要求不同，由适配器按目标模型重写。
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// Toolbox 是服务端能用的全部工具（内置 + 各 MCP 服务）。
type Toolbox interface {
	Specs() []ToolSpec
	// Call 执行一次调用，返回给模型看的结果（一般是 JSON 文本）。
	Call(ctx context.Context, call ToolCall) (string, error)
}

// ReplyUsage 是一次回答的用量与诊断信息。
type ReplyUsage struct {
	PromptTokens int // 输入的总数（含下面两项）
	OutputTokens int
	// 提示词缓存（Claude）：命中缓存读进来的、这次写进缓存的输入 token。价格和普通输入不同（读约 0.1 倍、写约 1.25 倍），
	// 记账时分开算；不报缓存的模型都是 0
	CacheReadTokens  int
	CacheWriteTokens int
	ThinkingChars    int   // 思考内容的字数（只统计，不朗读）；用来查「看不见的输出」
	LoadMillis       int64 // 本次请求里加载模型花的时间
	Requests         int   // 这次回答请求了模型几次（调工具时不止一次；add 累加时计数）
}

// add 把一次模型请求的用量累加进来：调工具时一次回答要请求模型好几次，每次都要计费，
// 只记最后一次会让日志里的用量比账单少。
func (u *ReplyUsage) add(request *ReplyUsage) *ReplyUsage {
	if request == nil {
		return u
	}
	total := ReplyUsage{}
	if u != nil {
		total = *u
	}
	total.PromptTokens += request.PromptTokens
	total.OutputTokens += request.OutputTokens
	total.CacheReadTokens += request.CacheReadTokens
	total.CacheWriteTokens += request.CacheWriteTokens
	total.ThinkingChars += request.ThinkingChars
	total.LoadMillis += request.LoadMillis
	total.Requests++
	return &total
}

// StreamingRecognizer 打开流式转写会话（server_vad，v2 免按键）：持续送音频，收「说话开始 / 结束 / 识别结果」事件。
type StreamingRecognizer interface {
	OpenStream(ctx context.Context, config StreamConfig) (RecognizerStream, error)
}

// StreamConfig 是流式转写会话的配置。
type StreamConfig struct {
	TurnDetection realtimeproto.TurnDetection
	WakeWord      bool // 启用唤醒门（haro 扩展）：会话从休眠开始
}

// RecognizerStream 是一个进行中的转写会话。
type RecognizerStream interface {
	// SendAudio 送一块 16kHz 单声道 pcm16。
	SendAudio(ctx context.Context, pcm []byte) error
	// SetAwake 切换唤醒门（只在启用唤醒词时有意义）：false = 休眠，true = 手动唤醒。
	SetAwake(ctx context.Context, awake bool) error
	// SetPlayback 告诉语音前端终端在不在播放回答（打断用）：true = 只听唤醒词（听到即打断），false = 恢复。
	SetPlayback(ctx context.Context, playing bool) error
	// Events 发出 InputAudioBufferSpeechStarted / SpeechStopped / InputAudioTranscriptionCompleted /
	// HaroWakeWordDetected / Error；连接断开时关闭。
	Events() <-chan realtimeproto.ServerEvent
	Close() error
}

type speakerKey struct{}

// WithSpeaker 把这一轮说话人（对话名）放进 ctx：工具执行时据此区分是谁的事（后台调查按人记任务）。
func WithSpeaker(ctx context.Context, person string) context.Context {
	return context.WithValue(ctx, speakerKey{}, person)
}

// SpeakerFrom 取出这一轮说话人；没有时为 SharedPerson（全家共用）。
func SpeakerFrom(ctx context.Context) string {
	if person, ok := ctx.Value(speakerKey{}).(string); ok && person != "" {
		return person
	}
	return SharedPerson
}

// Package brain 是 haro 的服务端：识别 → 模型流式回答 → 按句切分 → 合成。
//
// 对外只暴露 Session（经 realtimeproto 事件与终端交互）；本文件定义服务端依赖的各段能力（Go interface），
// 实现在 speech / llm 等包，由 cmd/haro-server 组装注入；和终端之间的 WebSocket 在本包外面。
package brain

import (
	"context"
	"encoding/json"
	"errors"
	"iter"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// ChatModel 根据完整的消息列表（系统提示词 + 历史 + 本轮输入 + 工具往来）流式给出回答。
type ChatModel interface {
	// StreamReply 逐段产出可见文字与工具调用；调用方 range 拉取，提前 break 即中止生成。
	// 流只有两种结束：最后一段带 Usage 和 Stop（为什么停下，不是 StopUnknown），或者出错（那一段带知道的那部分用量）。
	// 连接正常关掉却没收到厂商约定的结束标志，适配层按出错交出，不当成说完。
	StreamReply(ctx context.Context, request ChatRequest) iter.Seq2[ReplyDelta, error]
}

// ChatRequest 是一次模型请求。
type ChatRequest struct {
	Messages []ChatMessage
	Tools    []ToolSpec // 这次带的工具定义；空 = 没有工具
	// ForbidToolCalls：工具定义照给（定义不变，提示词缓存的前缀就不变），但这一轮不许调用 ——
	// 例如最后一轮逼模型直接回答。各适配用厂商的「不许调用工具」选项实现；服务端不认这个选项的，这一轮不带工具。
	ForbidToolCalls bool
}

// ErrModelUnavailable 表示连不上模型服务（网络不通、等不到回答、认证失败、限流、服务端出错或过载）：
// 不是请求本身有问题，过一会儿再试可能就好了。适配层把厂商的错误标成它（errors.Is 判断）；
// 大脑据此进入最低能力：不调用模型，念「连不上」和现在的时间（第 7 段）。
var ErrModelUnavailable = errors.New("连不上模型服务")

// ModelWarmer 是需要预热的模型（可选）：按需加载模型的服务（本地 Ollama）在第一轮对话前先把模型装进显存，
// 免得第一轮等几十秒；云端不用实现。包装别的模型的那几层用 WarmUp 转发。
type ModelWarmer interface {
	Warmup(ctx context.Context) error
}

// WarmUp 预热 model（它需要预热的话）。
func WarmUp(ctx context.Context, model ChatModel) error {
	if warmer, ok := model.(ModelWarmer); ok {
		return warmer.Warmup(ctx)
	}
	return nil
}

// StopReason 是模型这次为什么停下。零值是「不知道」：没说为什么停的流不当成说完了。
type StopReason int

const (
	// StopUnknown：不知道为什么停（流结束了，最后一段却没交出原因）。大脑和 CollectReply 按失败处理（ErrStopUnknown）
	StopUnknown   StopReason = iota
	StopCompleted            // 说完了，或停下来等工具的结果
	// StopMaxTokens：输出到了上限（或上下文满了），被截断。截断时适配不交出工具调用（参数可能不完整）
	StopMaxTokens
)

// ErrStopUnknown 表示模型的流结束了，却没说为什么停（StopUnknown），或者给的原因认不出：不当成说完，
// 说了一半的回答、写了一半的摘要都不能当完整的用。
var ErrStopUnknown = errors.New("模型没说为什么停下，不当成说完")

// TerminalCamera 是终端经 MCP 提供的摄像头（每条终端连接一个，由连接层注入）。
type TerminalCamera interface {
	// Available 是终端现在能不能拍（它的工具清单里有没有「拍一帧」）。
	Available() bool
	// Capture 让终端现拍一帧（JPEG）。失败时 error 的说明可以直接交给模型。
	Capture(ctx context.Context) ([]byte, error)
}

// FrameExpert 是看画面的幕后专家（可选，代理「看画面」，以子任务方式调用）：设了它，look_at_camera 不把画面交给前台模型，
// 而是把画面和主人这一轮的原话交给专家，前台拿到的是专家的判断（文字），再用自己的口吻转述。
// 专家出错时退回把画面直接交给前台。
type FrameExpert interface {
	AgentName() string // 日志、代理链里的名字
	ExamineFrame(ctx context.Context, question FrameQuestion) (FrameReport, error)
}

// FrameQuestion 是交给专家的一次看画面。
type FrameQuestion struct {
	Frame   []byte // 摄像头当前画面（JPEG）
	Request string // 主人这一轮的原话
	// Question 是前台整理的要看清什么（主人的话接着上文时，指代已经说清楚）；空 = 只看主人的原话
	Question string
	Style    string // 说给主人听的那句用的口吻（当前性格的说明）；空 = 不限
}

// FrameReport 是专家看完画面的结论，分两段交出：Spoken 直接念给主人，Details 留在这一轮的工具结果里供主人追问。
type FrameReport struct {
	Spoken  string // 说给主人听的一两句（专家已压到很短）；空 = 专家没按要求分开交，只有 Details
	Details string // 看到的细节、判断依据、查到的资料（不念给主人）
}

// SpokenError 是可以直接念给主人的错误（例如「这个月的额度用完了」）：模型返回它时，服务端按 Phrase
// 取固定的话念出来（每种语言一份，见 Phrases），而不是只报「出错了」。用 errors.As 判断。
type SpokenError interface {
	error
	// Phrase 是要念的那句话的键（Phrase* 常量）和要填进去的值。
	Phrase() (key string, values map[string]string)
}

// ErrInvalidArguments 表示一次工具调用的参数不对（缺了、格式不对、取值之间对不上、编号指向的东西没有）：照工具说明改了参数
// 再调一次就行，不是服务出了问题。工具返回的错误满足 errors.Is(err, ErrInvalidArguments) 时，交给模型的附注叫它改参数重调，
// 而不是当成「这次没查到」。用 InvalidArguments 包装。
var ErrInvalidArguments = errors.New("参数不对")

// InvalidArguments 把一个参数问题标成 ErrInvalidArguments：说明原样保留（Error() 不变），原来的错误照样能 errors.Is / errors.As。
func InvalidArguments(err error) error {
	if err == nil {
		return nil
	}
	return &invalidArgumentsError{err: err}
}

type invalidArgumentsError struct{ err error }

func (e *invalidArgumentsError) Error() string        { return e.err.Error() }
func (e *invalidArgumentsError) Unwrap() error        { return e.err }
func (e *invalidArgumentsError) Is(target error) bool { return target == ErrInvalidArguments }

// ProfileSource 给出主人资料：全家共用的 + 说话人自己的事实与偏好，外部注入（不在公开代码里）。
// 放在系统提示词里内置规矩之后、性格之前；很少变，所以不影响提示词缓存。没有资料时返回空。
type ProfileSource interface {
	Profile(person string) string
}

// Briefer 在每轮对话前给模型补充即时情况（时间、位置、额度提醒、后台调查的任务板……）。
// 给出的几行附在这一轮主人的话前面（不放系统提示词：见 Session.userTurn）、不进历史；person 是这一轮说话人的对话名。
// 每轮都会调用：不要在这里做网络请求。
type Briefer interface {
	Brief(person string) Briefing
}

// Briefing 是一个 Briefer 这一轮附上的情况。
type Briefing struct {
	Lines []string
	// Delivered 在这一轮说完、主人也听完了（播放没被打断）之后调用：这些情况算是告诉主人了（任务板上失败的调查、
	// 额度提醒，下一轮不再附上）。被打断、出错时不调用，下一轮照样附上。nil = 不用确认（时间、位置这类每轮重新算的）
	Delivered func()
	// Interrupted 在附上这些情况的那一轮被主人打断时调用（生成时或播放时；出错不算）。Delivered 和它最多调用一个。
	// 用来限制被打断后再附上几次（BriefingCuts）。可以为 nil
	Interrupted func()
}

// Deliveries 收集「主人听完这一轮才算数」的事（见 OnDelivered）；会话里是这一轮的记录。
type Deliveries interface {
	Add(delivered func())
}

type deliveriesKey struct{}

// WithDeliveries 把收集处放进工具执行用的 ctx。
func WithDeliveries(ctx context.Context, deliveries Deliveries) context.Context {
	return context.WithValue(ctx, deliveriesKey{}, deliveries)
}

// DeliveriesFrom 取出 ctx 里的收集处；没有时为 nil（工具不在一轮回答里执行，或经别的进程转交时要另外带过去）。
func DeliveriesFrom(ctx context.Context) Deliveries {
	deliveries, _ := ctx.Value(deliveriesKey{}).(Deliveries)
	return deliveries
}

// OnDelivered 让工具登记一件「主人听完这一轮才算数」的事，例如把调查结论取出来讲了，听完才标已汇报：
// 和 Briefing.Delivered 同一个时机调用（这一轮说完、播放没被打断）；被打断、出错时不调用。
// ctx 用工具执行时收到的那个（工具在回答的那个 goroutine 里同步执行）；ctx 里没有收集处时不登记。
func OnDelivered(ctx context.Context, delivered func()) {
	if deliveries := DeliveriesFrom(ctx); deliveries != nil {
		deliveries.Add(delivered)
	}
}

// BriefLines 把只给几行情况、不用确认的函数当作 Briefer（时间、位置这类每轮重新算的）。
type BriefLines func(person string) []string

// Brief 实现 Briefer。
func (f BriefLines) Brief(person string) Briefing { return Briefing{Lines: f(person)} }

// SpeechSynthesizer 把一句话合成为语音（在内存里传，不落临时文件）。
// 边合成边产出：第一段声音一到就交给调用方，不等整句合成完。
// 调用方 range 拉取，提前 break 即中止合成。
type SpeechSynthesizer interface {
	Synthesize(ctx context.Context, utterance Utterance) iter.Seq2[realtimeproto.PCMAudio, error]
}

// Utterance 是要合成的一句话：文字（情绪标记已取出）+ 情绪。用什么语言念由语音合成自己从文字判断。
// 情绪怎么体现由各语音合成实现决定（GPT-SoVITS 选参考录音；不支持情绪的实现忽略它）。
type Utterance struct {
	Text    string
	Emotion Emotion
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
	// Synthetic：用户角色里系统合成的消息，不是主人说的话（每轮附带的「现在的情况」、历史里「当时看了画面」的替身）。
	// 怎么交给模型由各适配层决定；找「本轮主人那句话」时跳过它
	Synthetic bool `json:"synthetic,omitempty"`
	// Replay：assistant：这次回答里模型服务要求原样带回的内容（见 ProviderReplay）；不存盘
	Replay *ProviderReplay `json:"-"`
}

// ReplyDelta 是模型流式输出的一段。
type ReplyDelta struct {
	Text      string          // 新增的可见文字（可能为空）
	ToolCalls []ToolCall      // 模型要求调用的工具（可能为空）
	Usage     *ReplyUsage     // 只在最后一段上有
	Stop      StopReason      // 只在最后一段上有意义（最后一段是 StopUnknown = 没说为什么停）
	Replay    *ProviderReplay // 只在最后一段上有：要原样带回的内容（没有时为 nil）
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
	ThinkingChars    int // 思考内容的字数（只统计，不朗读）；用来查「看不见的输出」
	Requests         int // 这次回答请求了模型几次（调工具时不止一次；add 累加时计数）
	// Incomplete：请求被打断或出错，用量只知道一部分：OutputTokens 不知道（为 0，不编）；
	// 知道多少输入取决于厂商（Claude 在流的开头就报了输入）。TokensUnknown：连输入也不知道（还没开始就失败了，
	// 或服务端不报用量）。累加时有一次不知道，合计就不知道
	Incomplete    bool
	TokensUnknown bool
}

// InputKnown 是知不知道输入的 token 数（含缓存读写）；OutputKnown 是知不知道输出的 token 数（请求完整结束才知道）。
// 不知道的不往外报（回答结束的事件、观测里都不写），不编成 0。
func (u *ReplyUsage) InputKnown() bool  { return u != nil && !u.TokensUnknown }
func (u *ReplyUsage) OutputKnown() bool { return u.InputKnown() && !u.Incomplete }

// add 把一次模型请求的用量累加进来：调工具时一次回答要请求模型好几次，每次都要计费，
// 只记最后一次会让日志里的用量比账单少。哪一次不知道（不完整），合计就不知道（不完整）。
func (u *ReplyUsage) add(request *ReplyUsage) *ReplyUsage {
	if request == nil { // 这次请求没交出用量：不知道
		request = &ReplyUsage{TokensUnknown: true}
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
	total.Requests++
	total.Incomplete = total.Incomplete || request.Incomplete
	total.TokensUnknown = total.TokensUnknown || request.TokensUnknown
	return &total
}

// StreamingRecognizer 打开流式转写会话（server_vad）：持续送音频，收「说话开始 / 结束 / 识别结果」事件。
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
	// HaroWakeWordDetected / Error；连接断开时关闭。打开时转写服务确认设置的 SessionUpdated（带着它认的唤醒词）
	// 排在最前面（实现可以不发：会话就不知道唤醒词，终端的提示里不写具体是哪个词）。
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

type voiceVerifiedKey struct{}

// WithVoiceVerified 记下这一轮的话是不是声纹认出的登记过的人说的（算不出声纹、沿用上一句的人时不算）：
// 只许本人做的事（例如定提醒）据此决定做不做。
func WithVoiceVerified(ctx context.Context, verified bool) context.Context {
	return context.WithValue(ctx, voiceVerifiedKey{}, verified)
}

// VoiceVerifiedFrom 取出这一轮的话是不是声纹认出的登记过的人说的；没记过（不是在会话里调用）时为 false。
func VoiceVerifiedFrom(ctx context.Context) bool {
	verified, _ := ctx.Value(voiceVerifiedKey{}).(bool)
	return verified
}

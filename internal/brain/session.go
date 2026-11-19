package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// Components 是服务端需要的全部能力，由 cmd/haro 组装注入。
type Components struct {
	Streaming    StreamingRecognizer // 流式识别：服务端判断说完、唤醒词都靠它
	Model        ChatModel
	Synthesizer  SpeechSynthesizer
	Tools        Toolbox              // 可选：v2.5 工具（内置 + MCP）
	FrameExpert  FrameExpert          // 可选：看画面的幕后专家；nil = 画面直接交给前台模型
	NewSegmenter func() TextSegmenter // 每次回答新建一个
	// Conversations 是所有终端连接共用的对话簿；nil = 每条连接自己一份、只在内存里
	Conversations *ConversationBook
	// WakeArbiter 是多台终端同时听到唤醒词时的仲裁；nil = 不仲裁（每台都回答）
	WakeArbiter *WakeArbiter
	// Briefers 每轮补充给模型的即时情况（额度提醒、任务板）；可以为空
	Briefers []Briefer
	// Profiles 是主人资料；nil = 没有
	Profiles ProfileSource
	// Notices 是所有终端连接共用的提示中转（后台调查查完了让终端响一声）；nil = 不发提示
	Notices *NoticeHub
}

// Options 是服务端的可调参数。
type Options struct {
	MaxHistoryTurn int // 保留最近几轮（一问一答算一轮）的文字历史

	// Instructions 是默认的基础提示词（身份、能力、底线）。v3 起由服务端持有，终端不再发；
	// 终端在 session.update 里带了 instructions 时以终端的为准（与 Realtime 一致）。
	Instructions string

	// 性格档案（可选）：两个以上时提供 switch_persona 工具
	Personas       []Persona
	DefaultPersona string       // 没记过当前性格时用哪个（ID）
	PersonaState   PersonaState // 可选：记住当前性格，重启后保持

	// Escalation：有更强的模型可以接手重答（选模型的包装层配了这条路）时为 true，提供 ask_stronger_model 工具
	Escalation bool
}

// Session 是一次与终端的会话：接收 realtimeproto 客户端事件，经 Events() 发出服务端事件。
// 同一时间只允许一个进行中的回答（与 Realtime 一致）。说完由识别流判断（session.update 里的 turn_detection），不接受手动提交。
type Session struct {
	parts  Components
	opts   Options
	logger *slog.Logger

	events    chan realtimeproto.ServerEvent
	closed    chan struct{}
	closeOnce sync.Once

	mu             sync.Mutex
	instructions   string
	transcription  *transcriptionJob  // 已判定说完、等待被回答使用的识别任务
	image          []byte             // 等待被回答使用的画面
	cancelResponse context.CancelFunc // 非 nil 表示有进行中的回答

	stream      RecognizerStream             // 识别流：音频转发给它，由它判断说完；nil = 还没打开或已断开
	pendingJobs map[string]*transcriptionJob // 已判定说完、等待识别结果的语音段（按 item_id）
	wakePending bool                         // 刚听到唤醒词：接下来这句如果是空的，就回「我在。」
	playing     bool                         // 终端正在播放回答（打断自己的唤醒在仲裁里最优先，v4 D2）

	conversation       *Conversation   // 这一轮说话的人的对话（跟人走，不跟连接走）
	spoken             []spokenSegment // 最近一次回答已经发给终端的句子（打断时按「听到多少」截断历史，v3 I-2）
	truncateAtMs       int64           // 终端说用户只听到了前这么多毫秒；-1 = 没有截断
	answeredInResponse bool            // 这次回答已经记进历史（截断时只改这次的，不碰上一轮）
	noCamera           bool            // 终端声明没有摄像头（session.update）：不提供看画面工具
	cameraGone         bool            // 终端报告摄像头现在不可用（设备报告）：同上
	noSpeaker          bool            // 终端说现在没有音箱：不合成语音，只发文字
}

var (
	// errNoStream：识别流还没打开（session.update 没带 turn_detection）或已断开，音频无处可送。
	errNoStream = errors.New("识别流没有打开：session.update 要带 turn_detection；断开后要重新打开")
	// errManualCommit：说完由服务端判断，不接受 input_audio_buffer.commit。
	errManualCommit = errors.New("不接受手动提交：说完由服务端判断（session.update 里的 turn_detection）")
)

// NewSession 创建会话。
// parts.Conversations 为 nil 时这条会话自己建一个只在内存里的对话簿（和 v3 一样：断开就没了）。
func NewSession(parts Components, opts Options, logger *slog.Logger) *Session {
	if parts.Conversations == nil {
		parts.Conversations = NewConversationBook(nil, opts, logger)
	}
	conversation := parts.Conversations.Conversation(SharedPerson) // 第一句话认出是谁之前先用全家的
	if persona := conversation.currentPersona(); persona != nil {
		logger.Info("当前性格", "persona", persona.Name, "available", len(opts.Personas))
	}
	session := &Session{
		parts: parts, opts: opts, logger: logger, conversation: conversation, instructions: opts.Instructions, truncateAtMs: -1,
		events: make(chan realtimeproto.ServerEvent, 32),
		closed: make(chan struct{}),
	}
	if parts.Notices != nil {
		parts.Notices.join(session)
	}
	return session
}

// Warmup 预热模型。只在部署侧调用（不是 Realtime 事件）。
func (s *Session) Warmup(ctx context.Context) error {
	return s.parts.Model.Warmup(ctx)
}

// Events 返回服务端事件流（会话期间不关闭）。
func (s *Session) Events() <-chan realtimeproto.ServerEvent { return s.events }

// Close 结束会话并取消进行中的回答。
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.cancelResponse != nil {
			s.cancelResponse()
		}
		stream := s.stream
		s.mu.Unlock()
		close(s.closed)
		if s.parts.WakeArbiter != nil {
			s.parts.WakeArbiter.leave(s)
		}
		if s.parts.Notices != nil {
			s.parts.Notices.leave(s)
		}
		if stream != nil {
			_ = stream.Close()
		}
	})
	return nil
}

// Send 处理一个客户端事件。ctx 是这一轮对话的上下文（携带追踪 span），
// 由它派生的识别、回答都会随它取消。
func (s *Session) Send(ctx context.Context, event realtimeproto.ClientEvent) error {
	// 音频与唤醒门控制直接转发给识别流，不持锁做网络写入。
	switch e := event.(type) {
	case realtimeproto.HaroPlaybackStarted, realtimeproto.HaroPlaybackStopped:
		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()
		if stream == nil { // 识别流还没打开或已断开：没有要通知的
			return nil
		}
		_, playing := event.(realtimeproto.HaroPlaybackStarted)
		s.mu.Lock()
		s.playing = playing
		s.mu.Unlock()
		return stream.SetPlayback(ctx, playing)
	case realtimeproto.ConversationItemTruncate:
		s.truncate(e.AudioEndMs)
		return nil
	case realtimeproto.HaroTerminalDevices:
		s.mu.Lock()
		s.noSpeaker = e.Speaker == ""
		s.cameraGone = e.Camera == "" // 摄像头被拔掉 / 插回来：不给 / 再给模型看画面的工具
		s.mu.Unlock()
		s.logger.Info("终端设备", "microphone", deviceLabel(e.Microphone), "speaker", deviceLabel(e.Speaker),
			"camera", deviceLabel(e.Camera))
		return nil
	case realtimeproto.HaroWakeWordSleep, realtimeproto.HaroWakeWordWake:
		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()
		if stream == nil {
			return errors.New("没有流式转写会话，无法切换唤醒状态")
		}
		_, awake := event.(realtimeproto.HaroWakeWordWake)
		return stream.SetAwake(ctx, awake)
	}
	if audio, ok := event.(realtimeproto.InputAudioBufferAppend); ok {
		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()
		if stream == nil {
			return errNoStream
		}
		if audio.Audio.Format != streamFormat {
			return fmt.Errorf("只接受 %+v 的音频，收到 %+v", streamFormat, audio.Audio.Format)
		}
		return stream.SendAudio(ctx, audio.Audio.Data)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e := event.(type) {
	case realtimeproto.SessionUpdate:
		if e.Instructions != "" { // 不带 instructions 时保留服务端自己的默认提示词
			s.instructions = e.Instructions
		}
		s.noCamera = e.NoCamera
		if e.TurnDetection != nil && s.stream == nil {
			if err := s.openStream(ctx, StreamConfig{TurnDetection: *e.TurnDetection, WakeWord: e.WakeWord}); err != nil {
				return err
			}
		}
	case realtimeproto.InputAudioBufferCommit:
		return errManualCommit
	case realtimeproto.ConversationItemCreate:
		for _, part := range e.Item.Content {
			if part.Type == "input_image" {
				s.image = part.ImageJPEG
			}
		}
	case realtimeproto.ResponseCreate:
		if s.cancelResponse != nil {
			return errors.New("已有进行中的回答")
		}
		responseCtx, cancel := context.WithCancel(ctx)
		s.cancelResponse = cancel
		job, image := s.transcription, s.image
		s.transcription, s.image = nil, nil
		go s.runResponse(responseCtx, job, image)
	case realtimeproto.ResponseCancel:
		if s.cancelResponse != nil {
			s.cancelResponse()
		}
	default:
		return fmt.Errorf("不支持的事件 %s", event.Type())
	}
	return nil
}

// emit 发出一个服务端事件；会话关闭后丢弃。
func (s *Session) emit(event realtimeproto.ServerEvent) {
	select {
	case s.events <- event:
	case <-s.closed:
	}
}

// tryEmit 不阻塞地发一个事件（从别的 goroutine 发提示用）；会话已关闭或积压太多时返回 false。
func (s *Session) tryEmit(event realtimeproto.ServerEvent) bool {
	select {
	case <-s.closed:
		return false
	default:
	}
	select {
	case s.events <- event:
		return true
	default:
		s.logger.Warn("终端积压的事件太多：这次提示不发了", "event", event.Type())
		return false
	}
}

// historyWithInstructions 返回「系统提示词 + 当前说话人的历史」的副本。
func (s *Session) historyWithInstructions() []ChatMessage {
	messages := []ChatMessage{}
	if prompt := s.systemPrompt(); prompt != "" {
		messages = append(messages, ChatMessage{Role: RoleSystem, Text: prompt})
	}
	return append(messages, s.currentConversation().messages()...)
}

// currentConversation 返回这一轮说话人的对话。
func (s *Session) currentConversation() *Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversation
}

// useConversationOf 换成这个人的对话（每轮认出说话人之后调用；没比对声纹时用全家的）。
func (s *Session) useConversationOf(person string) {
	conversation := s.parts.Conversations.Conversation(person)
	s.mu.Lock()
	changed := s.conversation != conversation
	s.conversation = conversation
	s.mu.Unlock()
	if s.parts.Notices != nil {
		s.parts.Notices.spoke(s, conversation.Person())
	}
	if changed {
		s.logger.Info("对话换人", "person", conversation.Person())
	}
}

// remember 记下一轮：用户的话、这一轮的工具往来（调用了什么、结果是什么）、最终回答。图片不进历史。
//
// 为什么要保留工具往来：只保留最终回答时，历史里全是「记下了」「网络正常」，小模型照着学，
// 就不再调用工具、直接说办好了（2026-11-15 实测：同一段历史，只留回答 0/4 调用 save_note，
// 保留工具往来 4/4）。
func (s *Session) remember(question string, toolTrail []ChatMessage, answer string) {
	turn := make([]ChatMessage, 0, len(toolTrail)+2)
	turn = append(turn, ChatMessage{Role: RoleUser, Text: question})
	for _, msg := range toolTrail {
		if len(msg.Image) > 0 { // 图片不进历史：只留一句「当时看了画面」
			msg = ChatMessage{Role: msg.Role, Text: frameRecalledNote}
		}
		turn = append(turn, msg)
	}
	turn = append(turn, ChatMessage{Role: RoleAssistant, Text: answer})
	s.mu.Lock()
	s.answeredInResponse = true
	conversation := s.conversation
	s.mu.Unlock()
	conversation.appendTurn(turn, s.opts.MaxHistoryTurn)
}

// deviceLabel 是日志里的设备名：没有时写「没有」。
func deviceLabel(name string) string {
	if name == "" {
		return "没有"
	}
	return name
}

package brain

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// Components 是服务端需要的全部能力，由 cmd/haro-server 组装注入。
type Components struct {
	Streaming StreamingRecognizer // 流式识别：服务端判断说完、唤醒词都靠它
	// Front 是前台：主对话的代理（模型、基础规则、能用的工具、步数）
	Front Agent
	// Handoff 是接手（可选）：哪些情况把这一轮剩下的部分交给更强的代理回答
	Handoff      Handoff
	Synthesizer  SpeechSynthesizer
	Tools        Toolbox              // 可选：工具（内置 + MCP）
	FrameExpert  FrameExpert          // 可选：看画面的幕后专家；nil = 画面直接交给前台模型
	Camera       TerminalCamera       // 可选：终端经 MCP 提供的摄像头；nil 或拍不了时不给模型看画面的工具
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
	// MonthlyCap、OverBudget：月度上限到了以后，主人可以口头临时提高（见 monthly_cap.go）；
	// 都设了才有这个能力。OverBudget 的模型不受额度管（否则上限到了就没法判断），由组装处限每天的次数
	MonthlyCap MonthlyCap
	OverBudget *Agent
}

// Options 是服务端的可调参数。
type Options struct {
	MaxHistoryTurn int // 保留最近几轮（一问一答算一轮）的文字历史
	// CompactContextTokens：一轮里请求模型带的上下文（输入 token）到了这么多，就提前把较早的几轮移出窗口、压缩；0 = 不看
	CompactContextTokens int

	StreamReopen ReopenTiming // 识别流断开后重开的节奏；零值 = DefaultReopenTiming
	// ConfirmationTimeout：会破坏的操作问了主人之后，多久没确认就作废；零值 = DefaultConfirmationTimeout
	ConfirmationTimeout time.Duration
	// ToolTimeout：单次工具执行最多多久（所有工具共用，包括远程 MCP）；零值 = DefaultToolTimeout
	ToolTimeout time.Duration
	// Phrases：不经模型、直接念的固定的话（每种语言一份）；零值 = 程序里带的那份
	Phrases Phrases
	// MonthlyRaiseYen：口头提高月度上限时每次加多少（念给主人听的数，和额度那一层用同一个配置）
	MonthlyRaiseYen float64

	// 性格档案（可选）：两个以上时提供 switch_persona 工具
	Personas       []Persona
	DefaultPersona string       // 没记过当前性格时用哪个（ID）
	PersonaState   PersonaState // 可选：记住当前性格，重启后保持
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
	// lifetime 随会话结束而取消：回答都在它下面跑（判定说完时自动开始的回答没有请求的上下文可用）
	lifetime    context.Context
	endLifetime context.CancelFunc
	responses   sync.WaitGroup // 进行中的回答（Close 等它们收尾）

	mu             sync.Mutex
	transcription  *transcriptionJob  // 已判定说完、等待被回答使用的识别任务
	cancelResponse context.CancelFunc // 非 nil 表示有进行中的回答
	autoRespond    bool               // 判定说完时自己开始回答（turn_detection 的 create_response，默认是）

	stream          RecognizerStream             // 识别流：音频转发给它，由它判断说完；nil = 还没打开或断开了正在重开
	streamConfig    *StreamConfig                // 打开识别流用的设置；nil = 还没打开过（断开后按它重开）
	pendingJobs     map[string]*transcriptionJob // 已判定说完、等待识别结果的语音段（按 item_id）
	lastWakeKeyword string                       // 最近一次听到的唤醒词（配置里的原文，告诉模型开头可能带着它）
	language        string                       // 主人最近一句话识别出的语种（固定的话按它选语言）
	// awake 照识别流报的记（醒 / 睡只由识别流的唤醒门改：haro.wake_word.detected、haro.wake_word.state）；
	// 识别流断开期间记终端要的，重开后告诉新的流、由它回复确认。
	// playing 照终端报的记（播没播由终端决定）。断开重开后按这两个状态重新告诉新的流
	awake        bool
	playing      bool // 终端正在播放回答（只听唤醒词；打断自己的唤醒在仲裁里最优先）
	droppedAudio int  // 识别流断开期间收到、只能丢掉的音频块数（重开时记一次日志）
	// speech 是识别流报了「开始说话」、还没报「说完」的那句；识别流在这时断开，这句就丢了（不会再有「说完」）
	speech *realtimeproto.InputAudioBufferSpeechStarted

	conversation       *Conversation   // 这一轮说话的人的对话（跟人走，不跟连接走）
	spoken             []spokenSegment // 最近一次回答已经发给终端的句子（打断时按「听到多少」截断历史）
	answerRound        int             // 记进历史的最终回答是这次回答的第几段（之后才收到的截断只改这一段）
	truncateAtMs       int64           // 终端说用户只听到了前这么多毫秒；-1 = 没有截断
	answeredInResponse bool            // 这次回答已经记进历史（截断时只改这次的，不碰上一轮）
	responseItem       string          // 这次回答的是哪一句话（编号，打断的日志用）
	// pendingDelivered：上一轮说完了，这一轮附上的情况（任务板的结论、额度提醒）等确认主人听完才算告诉了——
	// 下一轮回答开始时还没收到截断就调用；播放中被打断就丢掉（之后再附上）。声音在终端播完要比服务端发完晚得多
	pendingDelivered []func()
	noSpeaker        bool // 终端说现在没有音箱：不合成语音，只发文字
}

// errNoStream：识别流还没打开（session.update 没带 turn_detection）或已断开，音频无处可送。
var errNoStream = realtimeproto.ProtocolErrorf(realtimeproto.CodeNoTranscriptionStream,
	"识别流没有打开：session.update 要带 turn_detection；断开后要重新打开")

// NewSession 创建会话。
// parts.Conversations 为 nil 时这条会话自己建一个只在内存里的对话簿（断开就没了）。
func NewSession(parts Components, opts Options, logger *slog.Logger) *Session {
	if opts.Phrases.Languages == nil {
		opts.Phrases = builtinPhrasesOrPanic()
	}
	if parts.Conversations == nil {
		parts.Conversations = NewConversationBook(nil, opts, logger)
	}
	conversation := parts.Conversations.Conversation(SharedPerson) // 第一句话认出是谁之前先用全家的
	if persona := conversation.currentPersona(); persona != nil {
		logger.Info("当前性格", "persona", persona.Name, "available", len(opts.Personas))
	}
	lifetime, endLifetime := context.WithCancel(context.Background())
	session := &Session{
		parts: parts, opts: opts, logger: logger, conversation: conversation, truncateAtMs: -1,
		events: make(chan realtimeproto.ServerEvent, 32),
		closed: make(chan struct{}), lifetime: lifetime, endLifetime: endLifetime,
	}
	if parts.Notices != nil {
		parts.Notices.join(session)
	}
	return session
}

// Events 返回服务端事件流（会话期间不关闭）。
func (s *Session) Events() <-chan realtimeproto.ServerEvent { return s.events }

// Close 结束会话：取消进行中的回答，等它收尾（写进历史）后才返回。
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.cancelResponse != nil {
			s.cancelResponse()
		}
		close(s.closed) // 在锁里关：重开识别流时持锁查 closed，关掉之后不会再挂上新的流
		stream := s.stream
		s.mu.Unlock()
		if s.parts.WakeArbiter != nil {
			s.parts.WakeArbiter.leave(s)
		}
		if s.parts.Notices != nil {
			s.parts.Notices.leave(s)
		}
		if stream != nil {
			_ = stream.Close()
		}
		s.endLifetime()
	})
	s.responses.Wait() // 等进行中的那一轮收尾（被取消后照 3.4 的规则写进历史）：关停时之后才关数据库
	return nil
}

// Send 处理一个客户端事件。ctx 是这一轮对话的上下文（携带追踪 span），
// 由它派生的识别、回答都会随它取消。
func (s *Session) Send(ctx context.Context, event realtimeproto.ClientEvent) error {
	// 音频与唤醒门控制直接转发给识别流，不持锁做网络写入。
	switch e := event.(type) {
	case realtimeproto.HaroPlaybackStarted, realtimeproto.HaroPlaybackStopped:
		_, playing := event.(realtimeproto.HaroPlaybackStarted)
		s.mu.Lock()
		s.playing = playing
		stream := s.stream
		s.mu.Unlock()
		if stream == nil { // 识别流还没打开或正在重开：重开后按记下的状态告诉新的流
			return nil
		}
		return stream.SetPlayback(ctx, playing)
	case realtimeproto.ConversationItemTruncate:
		s.truncate(e.AudioEndMs)
		return nil
	case realtimeproto.HaroTerminalDevices:
		s.mu.Lock()
		s.noSpeaker = e.Speaker == ""
		s.mu.Unlock()
		s.logger.Info("终端设备", "microphone", deviceLabel(e.Microphone), "speaker", deviceLabel(e.Speaker))
		return nil
	case realtimeproto.HaroWakeWordSleep, realtimeproto.HaroWakeWordWake:
		_, awake := event.(realtimeproto.HaroWakeWordWake)
		s.mu.Lock()
		stream, opened := s.stream, s.streamConfig != nil
		if opened && stream == nil {
			s.awake = awake // 识别流正在重开：记下，重开后告诉新的流（由它回复确认）
		}
		s.mu.Unlock()
		switch {
		case !opened:
			return realtimeproto.ProtocolErrorf(realtimeproto.CodeNoTranscriptionStream, "没有流式转写会话，无法切换唤醒状态")
		case stream == nil:
			return nil
		}
		s.requestAwake(ctx, stream, awake)
		return nil
	}
	if audio, ok := event.(realtimeproto.InputAudioBufferAppend); ok {
		if audio.Audio.Format != streamFormat {
			return realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedAudioFormat, "只接受 %+v 的音频，收到 %+v",
				streamFormat, audio.Audio.Format)
		}
		s.mu.Lock()
		stream, opened := s.stream, s.streamConfig != nil
		if opened && stream == nil {
			s.droppedAudio++
		}
		s.mu.Unlock()
		switch {
		case !opened:
			return errNoStream
		case stream == nil: // 识别流断开、正在重开：这段音频没处送，丢掉（不逐块回错误）
			return nil
		}
		if err := stream.SendAudio(ctx, audio.Audio.Data); err != nil && ctx.Err() == nil {
			// 流刚断：转发事件的循环马上会发现并开始重开，这一块算丢掉
			s.mu.Lock()
			s.droppedAudio++
			s.mu.Unlock()
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e := event.(type) {
	case realtimeproto.SessionUpdate:
		if e.TurnDetection != nil {
			s.autoRespond = e.TurnDetection.CreateResponse == nil || *e.TurnDetection.CreateResponse
		}
		if e.TurnDetection != nil && s.streamConfig == nil {
			if err := s.openStream(ctx, StreamConfig{TurnDetection: *e.TurnDetection, WakeWord: e.WakeWord}); err != nil {
				return err
			}
		}
	case realtimeproto.ResponseCreate:
		return s.startResponseLocked()
	case realtimeproto.ResponseCancel:
		if s.cancelResponse != nil {
			s.cancelResponse()
		}
	default:
		return realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedEvent, "不支持的事件 %s", event.Type())
	}
	return nil
}

// startResponseLocked 开始回答最近判定说完的那句话（回答会等它的识别结果）。调用方持有 s.mu。
func (s *Session) startResponseLocked() error {
	if s.cancelResponse != nil {
		return realtimeproto.ProtocolErrorf(realtimeproto.CodeResponseInProgress, "已有进行中的回答")
	}
	responseCtx, cancel := context.WithCancel(s.lifetime)
	s.cancelResponse = cancel
	job := s.transcription
	s.transcription = nil
	s.responses.Add(1)
	go func() {
		defer s.responses.Done()
		s.runResponse(responseCtx, job)
	}()
	return nil
}

// 发给终端的事件经一条有上限的队列（events）。两条路：
//   - emit：一定要送到的（回答的文字和音频、这轮结束、出错……）—— 队列满了就等：终端读得慢，回答跟着慢，不丢；
//   - tryEmit：可以丢的（别的 goroutine 发来的提示）—— 队列满了就丢、计数（haro.events.dropped），不拖住发的那一方。

// emit 发出一个一定要送到的服务端事件；会话关闭后丢弃。
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
		droppedEvents.Add(s.lifetime, 1, metric.WithAttributes(attribute.String("haro.event.type", event.Type())))
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

// rememberTurn 把整理好的一轮（closeTurn 的结果：主人的话、工具往来、最终回答）写进这个人的历史；
// contextTokens 是这一轮请求模型时带的上下文（见 Conversation.appendTurn）。
//
// 为什么要保留工具往来：只保留最终回答时，历史里全是「记下了」「网络正常」，小模型照着学，
// 就不再调用工具、直接说办好了（实测：同一段历史，只留回答时 4 次里 0 次调用 save_note，
// 保留工具往来时 4 次都调用）。
func (s *Session) rememberTurn(at time.Time, messages []ChatMessage, contextTokens int) {
	s.mu.Lock()
	s.answeredInResponse = true
	conversation := s.conversation
	s.mu.Unlock()
	conversation.appendTurn(ConversationTurn{At: at, Messages: messages}, contextTokens)
}

// deviceLabel 是日志里的设备名：没有时写「没有」。
func deviceLabel(name string) string {
	if name == "" {
		return "没有"
	}
	return name
}

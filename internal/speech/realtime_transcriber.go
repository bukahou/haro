package speech

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/heartbeat"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// RealtimeTranscriptionClient 连接 OpenAI Realtime 风格的转写会话（WebSocket；说完判断用 server_vad 还是
// semantic_vad 由 session.update 里的 turn_detection 指定，唤醒门、播放这些 haro 扩展事件也走这条连接）。
// 服务端可以是 haro-asr，也可以是其他实现了同一子集的服务（事件定义见 realtimeproto）。
// 连着的时候按 Heartbeat 定时 ping：对端卡住（进程还在、不回应）时断开连接，由会话按断开重开。
// 连接为什么断了、哪些事件解不开，记在 Logger 里（会话那边只知道流结束了）。
type RealtimeTranscriptionClient struct {
	BaseURL   string           // http(s)://host:port，连接时换成 ws(s)://
	Heartbeat heartbeat.Timing // 零值 = heartbeat.Default（和终端那条连接一样）
	Logger    *slog.Logger     // nil = slog.Default()
}

// errHeartbeatLost：转写服务在心跳的期限内没有回应（进程还在、卡住了），客户端断开了连接。
var errHeartbeatLost = errors.New("转写服务没有回应心跳")

// NewRealtimeTranscriptionClient 创建客户端。
func NewRealtimeTranscriptionClient(baseURL string) *RealtimeTranscriptionClient {
	return &RealtimeTranscriptionClient{BaseURL: baseURL}
}

// OpenStream 建立连接、发送 session.update，等到转写服务确认（session.updated）才返回。ctx 管建立连接和等确认；
// 会话持续到 Close。
//
// 转写服务不接受这份设置时（回请求类错误码的 error，例如要 semantic_vad 而它没有说完判断模型）返回的错误包着
// brain.ErrStreamRejected 和带那个码的 *realtimeproto.ProtocolError：原样再连也一样被拒，要改配置。
// 它自己的问题（server_error 类）和连接出错返回普通的错误，由会话按断开重开的办法再试。
func (c *RealtimeTranscriptionClient) OpenStream(ctx context.Context, config brain.StreamConfig) (brain.RecognizerStream, error) {
	address, err := realtimeproto.TranscriptionURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, address, nil)
	if err != nil {
		return nil, fmt.Errorf("连接转写服务 %s: %w", address, err)
	}
	conn.SetReadLimit(realtimeproto.MaxMessageBytes)
	stream := &realtimeTranscriptionStream{conn: conn, events: make(chan realtimeproto.ServerEvent, 32),
		logger: cmp.Or(c.Logger, slog.Default()).With("transcription", address)}
	turn := config.TurnDetection
	if err := stream.send(ctx, realtimeproto.SessionUpdate{Kind: realtimeproto.SessionTranscription, TurnDetection: &turn,
		WakeWord: config.WakeWord, InputFormat: realtimeproto.TranscriptionFormat}); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("发送 session.update: %w", err)
	}
	accepted, err := stream.awaitSessionUpdated(ctx)
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	stream.events <- accepted // 确认原样交给会话（带着转写服务认的唤醒词）：排在之后所有事件前面
	streamCtx, cancel := context.WithCancel(context.Background())
	stream.cancel = cancel
	go stream.readEvents(streamCtx)
	go stream.keepAlive(streamCtx, c.Heartbeat.OrDefault())
	return stream, nil
}

// awaitSessionUpdated 读到转写服务对 session.update 的答复：session.updated = 接受（交回它，带着转写服务认的唤醒词）；
// error = 不接受。不认识的事件跳过（别的实现可能先发 session.created）；确认之前发来别的事件算对端不守协议。
func (s *realtimeTranscriptionStream) awaitSessionUpdated(ctx context.Context) (realtimeproto.SessionUpdated, error) {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return realtimeproto.SessionUpdated{}, fmt.Errorf("等转写服务确认 session.update: %w", err)
		}
		event, err := realtimeproto.DecodeServerEvent(data)
		if err != nil {
			return realtimeproto.SessionUpdated{}, fmt.Errorf("转写服务对 session.update 的答复解不开: %w", err)
		}
		switch reply := event.(type) {
		case nil:
		case realtimeproto.SessionUpdated:
			return reply, nil
		case realtimeproto.Error:
			refusal := &realtimeproto.ProtocolError{Code: reply.Code, Message: "转写服务: " + reply.Message}
			if reply.Code.ServerSide() { // 它自己的问题（例如出错、忙不过来）：过一会儿再连可能就好了
				return realtimeproto.SessionUpdated{}, refusal
			}
			return realtimeproto.SessionUpdated{}, fmt.Errorf("%w: %w", brain.ErrStreamRejected, refusal)
		default:
			return realtimeproto.SessionUpdated{}, fmt.Errorf("转写服务在确认 session.update 之前发来了 %s", event.Type())
		}
	}
}

// keepAlive 定时 ping 转写服务；等不到回应就断开连接（读循环随之结束、事件通道关闭，会话当作断开去重开）。
// ping 的回应由读循环处理，所以和 readEvents 一起运行。
func (s *realtimeTranscriptionStream) keepAlive(ctx context.Context, timing heartbeat.Timing) {
	if err := heartbeat.Keep(ctx, s.conn, timing); err != nil {
		s.ended(fmt.Errorf("%w: %w", errHeartbeatLost, err))
		_ = s.conn.CloseNow()
	}
}

type realtimeTranscriptionStream struct {
	conn    *websocket.Conn
	cancel  context.CancelFunc
	events  chan realtimeproto.ServerEvent
	writeMu sync.Mutex
	logger  *slog.Logger

	endOnce   sync.Once
	endReason error // 流为什么结束（只记第一个原因：心跳断开后读循环接着报的读错误不算）
}

// ended 记下流结束的原因（只记第一个）。
func (s *realtimeTranscriptionStream) ended(reason error) {
	s.endOnce.Do(func() { s.endReason = reason })
}

// send 把一个事件编码后发给转写服务（写入串行）。
func (s *realtimeTranscriptionStream) send(ctx context.Context, event realtimeproto.ClientEvent) error {
	data, err := realtimeproto.EncodeClientEvent(event)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.Write(ctx, websocket.MessageText, data)
}

func (s *realtimeTranscriptionStream) SendAudio(ctx context.Context, pcm []byte) error {
	return s.send(ctx, realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: realtimeproto.TranscriptionFormat, Data: pcm}})
}

func (s *realtimeTranscriptionStream) SetAwake(ctx context.Context, awake bool) error {
	if awake {
		return s.send(ctx, realtimeproto.HaroWakeWordWake{})
	}
	return s.send(ctx, realtimeproto.HaroWakeWordSleep{})
}

func (s *realtimeTranscriptionStream) SetPlayback(ctx context.Context, playing bool) error {
	if playing {
		return s.send(ctx, realtimeproto.HaroPlaybackStarted{})
	}
	return s.send(ctx, realtimeproto.HaroPlaybackStopped{})
}

func (s *realtimeTranscriptionStream) Events() <-chan realtimeproto.ServerEvent { return s.events }

func (s *realtimeTranscriptionStream) Close() error {
	s.cancel()
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

// readEvents 把转写服务的事件交给会话，直到连接断开（或 Close）。
// 解不开的事件跳过，按事件名计数：每种第一次解不开时记一条警告，流结束时再报每种一共几条（不每条刷一行）；
// 不认识的事件按 Realtime 的约定忽略。连接不是由 Close 关掉的，结束时记一条警告写明原因。
func (s *realtimeTranscriptionStream) readEvents(ctx context.Context) {
	undecodable := map[string]int{}
	defer func() {
		for eventType, count := range undecodable {
			if count > 1 {
				s.logger.Warn("这条识别流里有事件解不开，都跳过了", "event", eventType, "count", count)
			}
		}
		close(s.events)
	}()
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.ended(err)
			if ctx.Err() == nil { // 不是 Close 关的：转写服务断开、心跳超时、消息超过读上限……
				s.logger.Warn("和转写服务的连接断了", "reason", s.endReason, "close_status", websocket.CloseStatus(s.endReason))
			}
			return
		}
		event, err := realtimeproto.DecodeServerEvent(data)
		if err != nil {
			eventType, _ := realtimeproto.EventType(data) // 连事件名都解不开时为空
			if undecodable[eventType]++; undecodable[eventType] == 1 {
				s.logger.Warn("转写服务发来的事件解不开，跳过", "event", eventType, "err", err)
			}
			continue
		}
		if event == nil { // 不认识的事件（对端比自己新）
			continue
		}
		select {
		case s.events <- event:
		case <-ctx.Done():
			return
		}
	}
}

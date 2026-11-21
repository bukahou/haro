package speech

import (
	"context"
	"fmt"
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
type RealtimeTranscriptionClient struct {
	BaseURL   string           // http(s)://host:port，连接时换成 ws(s)://
	Heartbeat heartbeat.Timing // 零值 = heartbeat.Default（和终端那条连接一样）
}

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
	stream := &realtimeTranscriptionStream{conn: conn, events: make(chan realtimeproto.ServerEvent, 32)}
	turn := config.TurnDetection
	if err := stream.send(ctx, realtimeproto.SessionUpdate{Kind: realtimeproto.SessionTranscription, TurnDetection: &turn,
		WakeWord: config.WakeWord, InputFormat: realtimeproto.TranscriptionFormat}); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("发送 session.update: %w", err)
	}
	if err := stream.awaitSessionUpdated(ctx); err != nil {
		conn.CloseNow()
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	stream.cancel = cancel
	go stream.readEvents(streamCtx)
	go stream.keepAlive(streamCtx, c.Heartbeat.OrDefault())
	return stream, nil
}

// awaitSessionUpdated 读到转写服务对 session.update 的答复：session.updated = 接受；error = 不接受。
// 不认识的事件跳过（别的实现可能先发 session.created）；确认之前发来别的事件算对端不守协议。
func (s *realtimeTranscriptionStream) awaitSessionUpdated(ctx context.Context) error {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("等转写服务确认 session.update: %w", err)
		}
		event, err := realtimeproto.DecodeServerEvent(data)
		if err != nil {
			return fmt.Errorf("转写服务对 session.update 的答复解不开: %w", err)
		}
		switch reply := event.(type) {
		case nil:
		case realtimeproto.SessionUpdated:
			return nil
		case realtimeproto.Error:
			refusal := &realtimeproto.ProtocolError{Code: reply.Code, Message: "转写服务: " + reply.Message}
			if reply.Code.ServerSide() { // 它自己的问题（例如出错、忙不过来）：过一会儿再连可能就好了
				return refusal
			}
			return fmt.Errorf("%w: %w", brain.ErrStreamRejected, refusal)
		default:
			return fmt.Errorf("转写服务在确认 session.update 之前发来了 %s", event.Type())
		}
	}
}

// keepAlive 定时 ping 转写服务；等不到回应就断开连接（读循环随之结束、事件通道关闭，会话当作断开去重开）。
// ping 的回应由读循环处理，所以和 readEvents 一起运行。
func (s *realtimeTranscriptionStream) keepAlive(ctx context.Context, timing heartbeat.Timing) {
	if err := heartbeat.Keep(ctx, s.conn, timing); err != nil {
		_ = s.conn.CloseNow()
	}
}

type realtimeTranscriptionStream struct {
	conn    *websocket.Conn
	cancel  context.CancelFunc
	events  chan realtimeproto.ServerEvent
	writeMu sync.Mutex
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

func (s *realtimeTranscriptionStream) readEvents(ctx context.Context) {
	defer close(s.events)
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		event, err := realtimeproto.DecodeServerEvent(data)
		if err != nil || event == nil { // 解析失败或不认识的事件：跳过
			continue
		}
		select {
		case s.events <- event:
		case <-ctx.Done():
			return
		}
	}
}

package speech

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// RealtimeTranscriptionClient 连接 OpenAI Realtime 风格的转写会话（WebSocket，server_vad）。
// 服务端可以是 haro-asr，也可以是其他实现了同一子集的服务（事件定义见 realtimeproto）。
type RealtimeTranscriptionClient struct {
	BaseURL string // http(s)://host:port，会自动换成 ws(s)://
}

// NewRealtimeTranscriptionClient 创建客户端。
func NewRealtimeTranscriptionClient(baseURL string) *RealtimeTranscriptionClient {
	return &RealtimeTranscriptionClient{BaseURL: strings.TrimRight(baseURL, "/")}
}

// OpenStream 建立连接并发送 session.update。ctx 只用于建立连接；会话持续到 Close。
func (c *RealtimeTranscriptionClient) OpenStream(ctx context.Context, config brain.StreamConfig) (brain.RecognizerStream, error) {
	url := "ws" + strings.TrimPrefix(c.BaseURL, "http") + realtimeproto.TranscriptionPath
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("连接转写服务 %s: %w", url, err)
	}
	conn.SetReadLimit(realtimeproto.MaxMessageBytes)
	streamCtx, cancel := context.WithCancel(context.Background())
	stream := &realtimeTranscriptionStream{conn: conn, cancel: cancel,
		events: make(chan realtimeproto.ServerEvent, 32)}
	turn := config.TurnDetection
	if err := stream.send(ctx, realtimeproto.SessionUpdate{Kind: realtimeproto.SessionTranscription, TurnDetection: &turn,
		WakeWord: config.WakeWord, InputFormat: realtimeproto.TranscriptionFormat}); err != nil {
		cancel()
		conn.CloseNow()
		return nil, fmt.Errorf("发送 session.update: %w", err)
	}
	go stream.readEvents(streamCtx)
	return stream, nil
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

package speech

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"haro/internal/brain"
	"haro/internal/realtimeproto"
)

// RealtimeTranscriptionClient 连接 OpenAI Realtime 风格的转写会话（WebSocket，server_vad）。
// 服务端可以是 haro-asr，也可以是其他实现了同一子集的服务（见 realtimeproto/transcription_wire.go）。
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
	conn.SetReadLimit(1 << 20)
	if err := wsjson.Write(ctx, conn, realtimeproto.NewTranscriptionSessionUpdate(config.TurnDetection, config.WakeWord)); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("发送 session.update: %w", err)
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	stream := &realtimeTranscriptionStream{conn: conn, cancel: cancel,
		events: make(chan realtimeproto.ServerEvent, 32)}
	go stream.readEvents(streamCtx)
	return stream, nil
}

type realtimeTranscriptionStream struct {
	conn    *websocket.Conn
	cancel  context.CancelFunc
	events  chan realtimeproto.ServerEvent
	writeMu sync.Mutex
}

func (s *realtimeTranscriptionStream) SendAudio(ctx context.Context, pcm []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return wsjson.Write(ctx, s.conn, realtimeproto.NewWireAudioAppend(pcm))
}

func (s *realtimeTranscriptionStream) SetAwake(ctx context.Context, awake bool) error {
	eventType := "haro.wake_word.sleep"
	if awake {
		eventType = "haro.wake_word.wake"
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return wsjson.Write(ctx, s.conn, map[string]string{"type": eventType})
}

func (s *realtimeTranscriptionStream) SetPlayback(ctx context.Context, playing bool) error {
	eventType := "haro.playback.stopped"
	if playing {
		eventType = "haro.playback.started"
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return wsjson.Write(ctx, s.conn, map[string]string{"type": eventType})
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
		event, err := realtimeproto.DecodeServerWireEvent(data)
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

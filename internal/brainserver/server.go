// Package brainserver 把服务端挂到 WebSocket 上（v3 第 3 步）：
//   - GET /v1/realtime：对话会话（Realtime 事件，格式见 realtimeproto/conversation_wire.go）
//   - GET /health
//
// 一条连接一个会话（S-3，与 OpenAI Realtime 一致）：连接断了，这次的对话历史就没了（性格记在文件里，不受影响）。
// 本包只管协议和连接，会话由外部注入（haro-server 注入 brain.Session）。
package brainserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// Session 是一次对话会话（brain.Session 实现了它）。
type Session interface {
	Send(ctx context.Context, event realtimeproto.ClientEvent) error
	Events() <-chan realtimeproto.ServerEvent // 会话期间不关闭
	Close() error
}

// SessionFactory 为每条连接新建一个会话。
type SessionFactory func() Session

// defaultInputFormat 是终端没在 session.update 里声明格式时的输入音频格式（haro 的麦克风是 16 kHz）。
var defaultInputFormat = realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}

// readLimit：一条消息最大 8 MB（一帧画面 base64 后约 100–200 KB，留足余量）。
const readLimit = 8 << 20

// NewHandler 返回挂好路由的 HTTP handler。
func NewHandler(newSession SessionFactory, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+realtimeproto.ConversationPath, func(w http.ResponseWriter, r *http.Request) {
		serveConversation(w, r, newSession, logger)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

func serveConversation(w http.ResponseWriter, r *http.Request, newSession SessionFactory, logger *slog.Logger) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logger.Warn("WebSocket 握手失败", "remote", r.RemoteAddr, "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	session := newSession()
	defer session.Close()
	logger.Info("终端已连接", "remote", r.RemoteAddr)

	var writeMu sync.Mutex
	write := func(event realtimeproto.ServerEvent) error {
		data, err := realtimeproto.EncodeServerEvent(event)
		if err != nil {
			logger.Error("编码事件失败", "event", event.Type(), "err", err)
			return nil
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.Write(ctx, websocket.MessageText, data)
	}
	go func() { // 会话的事件 → 终端
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-session.Events():
				if err := write(event); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	inputFormat := defaultInputFormat
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if status := websocket.CloseStatus(err); status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway ||
				errors.Is(err, io.EOF) || ctx.Err() != nil {
				logger.Info("终端已断开", "remote", r.RemoteAddr)
			} else {
				logger.Warn("终端连接异常断开", "remote", r.RemoteAddr, "err", err)
			}
			return
		}
		event, err := realtimeproto.DecodeClientEvent(data, inputFormat)
		if err != nil {
			_ = write(realtimeproto.Error{Message: err.Error()})
			continue
		}
		if update, ok := event.(realtimeproto.SessionUpdate); ok && update.InputFormat.SampleRate > 0 {
			inputFormat = update.InputFormat
		}
		if err := session.Send(ctx, event); err != nil {
			_ = write(realtimeproto.Error{Message: event.Type() + ": " + err.Error()})
		}
	}
}

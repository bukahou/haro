// Package brainclient 是终端一侧的大脑：经 WebSocket 连 haro-brain（v3 第 3 步），实现 assistant.BrainSession。
//
// 与进程内的 brain.Session 保持同样的约定：Events() 在会话期间不关闭。
// 连接断了时：补发一个 Error；如果正有回答在进行，再补发一个 status=failed 的 ResponseDone（终端就不会一直等）；
// 之后 Send 都返回 ErrDisconnected，Done() 关闭。终端的看守（internal/terminal）据此关掉这条连接、重新 Dial（v4 D3）。
package brainclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"haro/internal/realtimeproto"
)

// ErrDisconnected 表示和大脑的连接断了。
var ErrDisconnected = errors.New("和大脑的连接断了")

// Client 是一条到 haro-brain 的对话会话。
type Client struct {
	conn        *websocket.Conn
	inputFormat realtimeproto.AudioFormat
	events      chan realtimeproto.ServerEvent
	done        chan struct{}
	logger      *slog.Logger
	cancel      context.CancelFunc
	writeMu     sync.Mutex

	mu           sync.Mutex
	inFlight     bool // 发了 response.create、还没收到 response.done
	disconnected bool
	closing      bool
}

// Dial 连接大脑；大脑还没起来时每秒重试一次，最多等 wait（开机时终端可能比大脑先启动）。
// inputFormat 是之后发送的麦克风音频格式，会声明在 session.update 里（S-1）。
func Dial(ctx context.Context, baseURL string, inputFormat realtimeproto.AudioFormat, wait time.Duration,
	logger *slog.Logger) (*Client, error) {
	url := "ws" + strings.TrimPrefix(strings.TrimRight(baseURL, "/"), "http") + realtimeproto.ConversationPath
	deadline := time.Now().Add(wait)
	for attempt := 1; ; attempt++ {
		conn, _, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			conn.SetReadLimit(8 << 20)
			readCtx, cancel := context.WithCancel(context.Background())
			client := &Client{conn: conn, inputFormat: inputFormat, logger: logger, cancel: cancel,
				events: make(chan realtimeproto.ServerEvent, 64), done: make(chan struct{})}
			go client.readEvents(readCtx)
			logger.Info("已连接大脑", "url", url)
			return client, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, fmt.Errorf("连不上大脑 %s（等了 %s，haro-brain 起来了吗？）: %w", url, wait, err)
		}
		if attempt == 1 {
			logger.Info("等大脑启动", "url", url, "wait", wait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Send 发送一个事件。session.update 没写输入格式时补上；连接断了以后返回 ErrDisconnected。
func (c *Client) Send(ctx context.Context, event realtimeproto.ClientEvent) error {
	switch e := event.(type) {
	case realtimeproto.SessionUpdate:
		if e.InputFormat.SampleRate == 0 {
			e.InputFormat = c.inputFormat
		}
		event = e
	case realtimeproto.InputAudioBufferAppend:
		if e.Audio.Format != c.inputFormat {
			return fmt.Errorf("音频格式 %+v 与会话声明的 %+v 不一致", e.Audio.Format, c.inputFormat)
		}
	}
	data, err := realtimeproto.EncodeClientEvent(event)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.disconnected {
		c.mu.Unlock()
		return ErrDisconnected
	}
	if _, ok := event.(realtimeproto.ResponseCreate); ok {
		c.inFlight = true
	}
	c.mu.Unlock()

	c.writeMu.Lock()
	err = c.conn.Write(ctx, websocket.MessageText, data)
	c.writeMu.Unlock()
	if err != nil && ctx.Err() == nil {
		c.disconnect(err)
		return fmt.Errorf("%w: %v", ErrDisconnected, err)
	}
	return err
}

// Events 返回大脑发来的事件（会话期间不关闭）。
func (c *Client) Events() <-chan realtimeproto.ServerEvent { return c.events }

// Done 在连接断开后关闭。
func (c *Client) Done() <-chan struct{} { return c.done }

// Close 主动关闭连接（程序退出时）。
func (c *Client) Close() error {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	c.cancel()
	return c.conn.Close(websocket.StatusNormalClosure, "")
}

func (c *Client) readEvents(ctx context.Context) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			c.disconnect(err)
			return
		}
		event, err := realtimeproto.DecodeServerWireEvent(data)
		if err != nil {
			c.logger.Warn("解析大脑的事件失败", "err", err)
			continue
		}
		if event == nil { // 不认识的事件（例如以后新增的）：忽略
			continue
		}
		if _, ok := event.(realtimeproto.ResponseDone); ok {
			c.mu.Lock()
			c.inFlight = false
			c.mu.Unlock()
		}
		select {
		case c.events <- event:
		case <-ctx.Done():
			return
		}
	}
}

// disconnect 只执行一次：标记断开，补发事件，关闭 Done()。主动关闭时不补发。
func (c *Client) disconnect(cause error) {
	c.mu.Lock()
	if c.disconnected {
		c.mu.Unlock()
		return
	}
	c.disconnected = true
	inFlight, closing := c.inFlight, c.closing
	c.inFlight = false
	c.mu.Unlock()
	close(c.done)
	if closing {
		return
	}
	c.logger.Error("和大脑的连接断了", "err", cause)
	go func() { // 不在读循环里阻塞：没人读事件时也不会卡住
		c.events <- realtimeproto.Error{Message: ErrDisconnected.Error()}
		if inFlight {
			c.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusFailed, Reason: "brain_disconnected"}
		}
	}()
}

// Package brainclient 是终端一侧的服务端：经 WebSocket 连 haro-server，实现 assistant.BrainSession。
//
// 与进程内的 brain.Session 保持同样的约定：Events() 在会话期间不关闭。
// 连接断了时：补发一个 Error；如果正有回答在进行，再补发一个 status=failed 的 ResponseDone（终端就不会一直等）；
// 之后 Send 都返回 ErrDisconnected，Done() 关闭。终端的看守（internal/terminal）据此关掉这条连接、重新 Dial。
package brainclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/realtimeproto"
)

var (
	// ErrDisconnected 表示和服务端的连接断了。
	ErrDisconnected = errors.New("和服务端的连接断了")
	// ErrUnreachable 表示等满了时限也连不上服务端。
	ErrUnreachable = errors.New("连不上服务端")
	// ErrFormatMismatch 表示要发的音频和会话声明的输入格式不一致。
	ErrFormatMismatch = errors.New("音频格式与会话声明的不一致")
)

// Client 是一条到 haro-server 的对话会话。
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

// Dial 连接服务端；服务端还没起来时每秒重试一次，最多等 wait（开机时终端可能比服务端先启动）。
// inputFormat 是之后发送的麦克风音频格式，会声明在 session.update 里。
func Dial(ctx context.Context, baseURL string, inputFormat realtimeproto.AudioFormat, wait time.Duration,
	logger *slog.Logger) (*Client, error) {
	url := "ws" + strings.TrimPrefix(strings.TrimRight(baseURL, "/"), "http") + realtimeproto.ConversationPath
	deadline := time.Now().Add(wait)
	// 握手时报上终端的版本；服务端在响应里报它的版本，不一样时在日志里提醒升级
	options := &websocket.DialOptions{HTTPHeader: http.Header{
		"User-Agent":     {buildinfo.UserAgent()},
		buildinfo.Header: {buildinfo.Version},
	}}
	for attempt := 1; ; attempt++ {
		conn, response, err := websocket.Dial(ctx, url, options)
		if err == nil {
			conn.SetReadLimit(8 << 20)
			readCtx, cancel := context.WithCancel(context.Background())
			client := &Client{conn: conn, inputFormat: inputFormat, logger: logger, cancel: cancel,
				events: make(chan realtimeproto.ServerEvent, 64), done: make(chan struct{})}
			go client.readEvents(readCtx)
			if serverVersion := response.Header.Get(buildinfo.Header); buildinfo.Mismatch(buildinfo.Version, serverVersion) {
				logger.Warn("已连接服务端，但版本和终端不同：服务端更新后，重新运行安装脚本把终端升到最新", "url", url,
					"terminal", buildinfo.Version, "server", buildinfo.Reported(serverVersion))
			} else {
				logger.Info("已连接服务端", "url", url, "server_version", buildinfo.Reported(serverVersion))
			}
			return client, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, fmt.Errorf("%w %s（等了 %s，haro-server 起来了吗？）: %w", ErrUnreachable, url, wait, err)
		}
		if attempt == 1 {
			logger.Info("等服务端启动", "url", url, "wait", wait)
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
			return fmt.Errorf("%w：%+v，会话声明的是 %+v", ErrFormatMismatch, e.Audio.Format, c.inputFormat)
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

// Events 返回服务端发来的事件（会话期间不关闭）。
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
			c.logger.Warn("解析服务端的事件失败", "err", err)
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
	c.logger.Error("和服务端的连接断了", "err", cause)
	go func() { // 不在读循环里阻塞：没人读事件时也不会卡住
		c.events <- realtimeproto.Error{Message: ErrDisconnected.Error()}
		if inFlight {
			c.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusFailed, Reason: "brain_disconnected"}
		}
	}()
}

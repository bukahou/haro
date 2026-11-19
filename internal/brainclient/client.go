// Package brainclient 是终端一侧的服务端：经 WebSocket 连 haro-server，实现 assistant.BrainSession。
//
// 与进程内的 brain.Session 保持同样的约定：Events() 在会话期间不关闭。
// 连接断了时：补发一个 Error；如果正有回答在进行，再补发一个 status=failed 的 ResponseDone（终端就不会一直等）；
// 之后 Send 都返回 ErrDisconnected，Done() 关闭。终端的看守（internal/terminal）据此关掉这条连接、重新 Dial。
package brainclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/heartbeat"
	"github.com/bukahou/haro/internal/mcpbridge"
	"github.com/bukahou/haro/internal/realtimeproto"
)

var (
	// ErrDisconnected 表示和服务端的连接断了。
	ErrDisconnected = errors.New("和服务端的连接断了")
	// ErrUnreachable 表示连不上服务端（握手没成功，且不是协议版本的问题）。
	ErrUnreachable = errors.New("连不上服务端")
	// ErrFormatMismatch 表示要发的音频和会话声明的输入格式不一致。
	ErrFormatMismatch = errors.New("音频格式与会话声明的不一致")
)

// Client 是一条到 haro-server 的对话会话。
type Client struct {
	conn        *websocket.Conn
	tools       *mcpbridge.Transport // 设备能力的 MCP 通道（终端是 MCP 服务端）
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

// DialOptions 是连接服务端的设置。
type DialOptions struct {
	BaseURL     string                    // 服务端地址：http(s)://host:port
	InputFormat realtimeproto.AudioFormat // 之后发送的麦克风音频格式，会声明在 session.update 里
	Heartbeat   heartbeat.Timing          // 零值 = heartbeat.Default
}

// Dial 连接服务端一次；连不上直接返回错误（重试由终端的看守负责）。
// 协议版本和服务端不兼容时（握手被拒，或者服务端没有选 realtimeproto.ProtocolV2），
// 返回的错误包着 realtimeproto.ErrIncompatibleProtocol：重连没用，要升级其中一端。
func Dial(ctx context.Context, opts DialOptions, logger *slog.Logger) (*Client, error) {
	url := "ws" + strings.TrimPrefix(strings.TrimRight(opts.BaseURL, "/"), "http") + realtimeproto.ConversationPath
	// 握手时报上协议版本和终端的构建版本；服务端在响应里报它的构建版本，不一样时在日志里提醒升级
	options := &websocket.DialOptions{
		Subprotocols: []string{realtimeproto.ProtocolV2},
		HTTPHeader:   http.Header{"User-Agent": {buildinfo.UserAgent()}, buildinfo.Header: {buildinfo.Version}},
	}
	conn, response, err := websocket.Dial(ctx, url, options)
	if err != nil {
		if rejection, ok := protocolRejection(response); ok {
			return nil, fmt.Errorf("%w：%s（服务端支持 %s）", realtimeproto.ErrIncompatibleProtocol, rejection.Message,
				strings.Join(rejection.SupportedProtocols, ", "))
		}
		return nil, fmt.Errorf("%w %s: %w", ErrUnreachable, url, err)
	}
	if conn.Subprotocol() != realtimeproto.ProtocolV2 {
		conn.Close(websocket.StatusProtocolError, "需要 "+realtimeproto.ProtocolV2)
		return nil, fmt.Errorf("%w：服务端没有选 %s（服务端是旧版本？）", realtimeproto.ErrIncompatibleProtocol,
			realtimeproto.ProtocolV2)
	}
	conn.SetReadLimit(realtimeproto.MaxMessageBytes)
	readCtx, cancel := context.WithCancel(context.Background())
	client := &Client{conn: conn, inputFormat: opts.InputFormat, logger: logger, cancel: cancel,
		events: make(chan realtimeproto.ServerEvent, 64), done: make(chan struct{})}
	client.tools = mcpbridge.NewTransport(func(ctx context.Context, message json.RawMessage) error {
		return client.Send(ctx, realtimeproto.HaroMCPMessage{Message: message})
	})
	go client.readEvents(readCtx)
	go client.keepAlive(readCtx, opts.Heartbeat.OrDefault())
	if serverVersion := response.Header.Get(buildinfo.Header); buildinfo.Mismatch(buildinfo.Version, serverVersion) {
		logger.Warn("已连接服务端，但版本和终端不同：服务端更新后，重新运行安装脚本把终端升到最新", "url", url,
			"terminal", buildinfo.Version, "server", buildinfo.Reported(serverVersion))
	} else {
		logger.Info("已连接服务端", "url", url, "server_version", buildinfo.Reported(serverVersion))
	}
	return client, nil
}

// protocolRejection 从被拒的握手响应里取出「协议版本不兼容」的说明；不是这种拒绝时 ok = false。
// （WebSocket 库只保证能读响应体的前 1024 字节，说明都在这个范围里。）
func protocolRejection(response *http.Response) (realtimeproto.HTTPErrorDetail, bool) {
	if response == nil || response.StatusCode != http.StatusBadRequest || response.Body == nil {
		return realtimeproto.HTTPErrorDetail{}, false
	}
	var body realtimeproto.HTTPError
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&body); err != nil ||
		body.Error.Code != realtimeproto.CodeUnsupportedProtocolVersion {
		return realtimeproto.HTTPErrorDetail{}, false
	}
	return body.Error, true
}

// keepAlive 定时 ping 服务端：半开的连接不会让读循环出错，只能靠等不到回应来发现。
func (c *Client) keepAlive(ctx context.Context, timing heartbeat.Timing) {
	if err := heartbeat.Keep(ctx, c.conn, timing); err != nil {
		c.disconnect(err)
		c.conn.CloseNow()
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

// DeviceTools 返回这条连接上的 MCP 通道（终端经它把设备能力提供给服务端）。
func (c *Client) DeviceTools() mcp.Transport { return c.tools }

// deliverTools 把一条 MCP 消息交给设备能力的会话；它卡住超过 eventBacklogTimeout 时断开连接，返回 false。
func (c *Client) deliverTools(ctx context.Context, message json.RawMessage) bool {
	deliverCtx, cancel := context.WithTimeout(ctx, eventBacklogTimeout)
	defer cancel()
	err := c.tools.Deliver(deliverCtx, message)
	switch {
	case err == nil:
		return true
	case ctx.Err() != nil:
		return false
	default:
		c.disconnect(fmt.Errorf("%w：设备能力（MCP）%v", errEventBacklog, err))
		c.conn.CloseNow()
		return false
	}
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
		event, err := realtimeproto.DecodeServerEvent(data)
		if err != nil {
			c.logger.Warn("解析服务端的事件失败", "err", err)
			continue
		}
		if event == nil { // 不认识的事件（例如以后新增的）：忽略
			continue
		}
		if message, ok := event.(realtimeproto.HaroMCPMessage); ok { // 设备能力：交给 MCP 会话，不给对话
			if !c.deliverTools(ctx, message.Message) {
				return
			}
			continue
		}
		if _, ok := event.(realtimeproto.ResponseDone); ok {
			c.mu.Lock()
			c.inFlight = false
			c.mu.Unlock()
		}
		if !c.deliver(ctx, event) {
			return
		}
	}
}

// eventBacklogTimeout：事件队列满了以后最多等这么久。终端一直腾不出地方收，就当它卡住了，
// 断开连接让看守重连——不悄悄丢事件（丢了音频是破音，丢了 response.done 终端会一直等）。
const eventBacklogTimeout = 5 * time.Second

// errEventBacklog：终端处理不过来服务端发来的事件。
var errEventBacklog = errors.New("终端处理不过来服务端的事件")

// deliver 把事件交给终端；返回 false 表示读循环该结束了（连接关了，或者终端卡住、已断开）。
func (c *Client) deliver(ctx context.Context, event realtimeproto.ServerEvent) bool {
	select {
	case c.events <- event:
		return true
	default:
	}
	timer := time.NewTimer(eventBacklogTimeout)
	defer timer.Stop()
	select {
	case c.events <- event:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		c.disconnect(fmt.Errorf("%w：%s 内没腾出地方", errEventBacklog, eventBacklogTimeout))
		c.conn.CloseNow()
		return false
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
	c.tools.Close()
	if closing {
		return
	}
	c.logger.Error("和服务端的连接断了", "err", cause)
	go func() { // 不在读循环里阻塞：没人读事件时也不会卡住
		c.events <- realtimeproto.Error{Message: ErrDisconnected.Error()}
		if inFlight {
			c.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusFailed, Reason: realtimeproto.ReasonServerDisconnected}
		}
	}()
}

// Package mcpbridge 让 MCP 跑在 haro 的对话连接上：每条 MCP（JSON-RPC）消息装进一个 haro.mcp.message 事件。
// 终端是 MCP 服务端（把设备能力做成工具），haro-server 是 MCP 客户端；两边各用一个 Transport，
// 不另开端口、不另建连接。
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrClosed 表示这条连接已经断了。
var ErrClosed = errors.New("MCP 通道已关闭")

// incomingBuffer：对端发来、MCP 会话还没读走的消息最多攒这么多（会话没连上之前先到的初始化请求就攒在这里）。
const incomingBuffer = 16

// Send 把一条 MCP 消息装进 haro.mcp.message 事件发给对端。
type Send func(ctx context.Context, message json.RawMessage) error

// Transport 实现 mcp.Transport：写 = 经 Send 发出去；读 = 取连接层用 Deliver 送进来的消息。
// 一个 Transport 对应一条对话连接，只能 Connect 一次。
type Transport struct {
	send      Send
	incoming  chan json.RawMessage
	closed    chan struct{}
	closeOnce sync.Once
}

var _ mcp.Transport = (*Transport)(nil)

// NewTransport 创建一条连接上的 MCP 通道。
func NewTransport(send Send) *Transport {
	return &Transport{send: send, incoming: make(chan json.RawMessage, incomingBuffer), closed: make(chan struct{})}
}

// Deliver 把对端发来的一条 MCP 消息交给 MCP 会话（连接层收到 haro.mcp.message 时调用）。
// 会话处理不过来时一直等到 ctx 结束；通道关了返回 ErrClosed。
func (t *Transport) Deliver(ctx context.Context, message json.RawMessage) error {
	select {
	case <-t.closed:
		return ErrClosed
	default:
	}
	select {
	case t.incoming <- message:
		return nil
	case <-t.closed:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close 关掉通道（连接断了时调用）：MCP 会话的读取随之结束。
func (t *Transport) Close() {
	t.closeOnce.Do(func() { close(t.closed) })
}

// Connect 实现 mcp.Transport。
func (t *Transport) Connect(context.Context) (mcp.Connection, error) {
	return connection{t}, nil
}

type connection struct{ transport *Transport }

func (c connection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case data := <-c.transport.incoming:
		return jsonrpc.DecodeMessage(data)
	case <-c.transport.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c connection) Write(ctx context.Context, message jsonrpc.Message) error {
	select {
	case <-c.transport.closed:
		return ErrClosed
	default:
	}
	data, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return err
	}
	return c.transport.send(ctx, data)
}

func (c connection) Close() error {
	c.transport.Close()
	return nil
}

func (connection) SessionID() string { return "" }

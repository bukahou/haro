package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pair 把两个 Transport 背靠背接起来：一边发出的消息由另一边的 Deliver 收到（模拟两端经对话连接互发事件）。
func pair() (client, server *Transport) {
	var c, s *Transport
	c = NewTransport(func(ctx context.Context, message json.RawMessage) error { return s.Deliver(ctx, message) })
	s = NewTransport(func(ctx context.Context, message json.RawMessage) error { return c.Deliver(ctx, message) })
	return c, s
}

type echoInput struct {
	Text string `json:"text"`
}

type echoOutput struct {
	Echo string `json:"echo"`
}

// 两端经桥接层跑完整的 MCP：初始化、取清单、调用工具、工具清单变化的通知。
func TestMCPOverBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientSide, serverSide := pair()
	server := mcp.NewServer(&mcp.Implementation{Name: "terminal", Version: "test"}, &mcp.ServerOptions{HasTools: true})
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	changed := make(chan struct{}, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "server", Version: "test"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed <- struct{}{} }})
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 0 {
		t.Fatalf("一开始没有工具：%v %v", tools, err)
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput{Echo: in.Text}, nil
	})
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("加了工具，客户端应收到「工具清单变了」")
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: echoInput{Text: "你好"}})
	if err != nil || result.IsError {
		t.Fatalf("调用工具：%v %+v", err, result)
	}
	var out echoOutput
	data, _ := json.Marshal(result.StructuredContent)
	if json.Unmarshal(data, &out) != nil || out.Echo != "你好" {
		t.Errorf("结构化结果 = %s", data)
	}
}

// 连接断了（关掉通道）：读取结束，之后送进来的消息返回 ErrClosed。
func TestClosedTransport(t *testing.T) {
	transport := NewTransport(func(context.Context, json.RawMessage) error { return nil })
	connection, _ := transport.Connect(context.Background())
	transport.Close()
	if _, err := connection.Read(context.Background()); err == nil {
		t.Error("关掉后读取应结束")
	}
	if err := transport.Deliver(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, ErrClosed) {
		t.Errorf("关掉后送消息应返回 ErrClosed：%v", err)
	}
}

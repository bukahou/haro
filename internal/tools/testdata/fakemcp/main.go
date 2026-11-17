// fakemcp 是测试用的「第三方 MCP 服务」：测 Toolbox 的 MCP 客户端（启动子进程、读工具清单、按标注决定要不要口头确认）。
// v5 起自研工具都内置，仓库里没有真正的 MCP 服务程序了，所以用它代替。
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Text string `json:"text"`
}

type echoOutput struct {
	Echo string `json:"echo"`
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "fakemcp", Version: "0.0.1"}, nil)
	echo := func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput{Echo: in.Text}, nil
	}
	notDestructive := false
	mcp.AddTool(server, &mcp.Tool{Name: "read_echo", Description: "只读", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, echo)
	mcp.AddTool(server, &mcp.Tool{Name: "add_echo", Description: "只新增", Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive}}, echo)
	mcp.AddTool(server, &mcp.Tool{Name: "wipe_echo", Description: "没有标注：按规范视为有破坏性"}, echo)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Print(err)
	}
}

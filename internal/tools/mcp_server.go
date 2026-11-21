package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerSpec 描述一个远程的第三方 MCP 工具服务（Streamable HTTP）。
// 工具只有三种来源：进程内的（内置工具，见 builtinServer）、远程的（这里）、终端的（会话里的看画面）。
type ServerSpec struct {
	Name    string            // 用于日志
	URL     string            //
	Headers map[string]string // 每个请求都带上（例如 Authorization: Bearer <密钥>；密钥放请求头而不是网址里，免得进日志）
	Client  *http.Client      // 可选；默认 http.DefaultClient
}

// MCPServer 是连上的一个 MCP 服务。
type MCPServer struct {
	name    string
	session *mcp.ClientSession
	tools   []*mcp.Tool
}

// ConnectMCPServer 连接一个远程 MCP 服务并读出它的工具清单。
func ConnectMCPServer(ctx context.Context, spec ServerSpec, logger *slog.Logger) (*MCPServer, error) {
	if spec.URL == "" {
		return nil, fmt.Errorf("工具服务 %s 没有地址", spec.Name)
	}
	httpClient := spec.Client
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if len(spec.Headers) > 0 {
		httpClient = withHeaders(httpClient, spec.Headers)
	}
	// 不开常驻的 SSE 推送流：只用请求-响应，省一条长连接
	return connectMCP(ctx, spec.Name, &mcp.StreamableClientTransport{Endpoint: spec.URL, HTTPClient: httpClient,
		DisableStandaloneSSE: true}, logger)
}

// connectMCP 经 transport 连上一个 MCP 服务并读出它的工具清单。
func connectMCP(ctx context.Context, name string, transport mcp.Transport, logger *slog.Logger) (*MCPServer, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "haro", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("连接工具服务 %s: %w", name, err)
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("读取 %s 的工具清单: %w", name, err)
	}
	logger.Info("工具服务已连接", "server", name, "tools", len(listed.Tools))
	return &MCPServer{name: name, session: session, tools: listed.Tools}, nil
}

// Tool 返回名为 name 的工具；没有时返回 nil。
func (s *MCPServer) Tool(name string) *mcp.Tool {
	for _, tool := range s.tools {
		if tool.Name == name {
			return tool
		}
	}
	return nil
}

// ToolError 是工具服务正常回应、但说这次调用出错了（参数不对、查不到……）：连接没问题，重连也没用。
// 其他错误（连不上、会话断了）不是这个类型。
type ToolError struct {
	Tool string
	Text string // 服务给的说明
}

func (e *ToolError) Error() string { return e.Text }

// SpokenToolError 是工具报的、可以直接念给主人的错误（例如额度用完）：实现 brain.SpokenError。
type SpokenToolError struct {
	ToolError
	Message string // 念给主人的话
}

// SpokenMessage 实现 brain.SpokenError。
func (e *SpokenToolError) SpokenMessage() string { return e.Message }

// Call 调用一个工具，返回文字结果；工具报错时返回 *ToolError（可以念给主人的是 *SpokenToolError），连接出问题时返回其他错误。
func (s *MCPServer) Call(ctx context.Context, name string, arguments map[string]any) (string, error) {
	return s.callWithMeta(ctx, name, arguments, nil)
}

func (s *MCPServer) call(ctx context.Context, name string, arguments map[string]any) (string, error) {
	return s.Call(ctx, name, arguments)
}

func (s *MCPServer) callWithMeta(ctx context.Context, name string, arguments map[string]any, meta mcp.Meta) (string, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	result, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments, Meta: meta})
	if err != nil {
		return "", err
	}
	if result.IsError {
		toolErr := ToolError{Tool: name, Text: contentText(result)}
		if message, ok := result.Meta[metaSpoken].(string); ok {
			return "", &SpokenToolError{ToolError: toolErr, Message: message}
		}
		return "", &toolErr
	}
	return resultText(result), nil
}

// CallKnown 只带上工具清单里声明过的参数再调用：第三方服务改版删了某个可选参数时，不因多传而整个调用失败。
func (s *MCPServer) CallKnown(ctx context.Context, name string, arguments map[string]any) (string, error) {
	if tool := s.Tool(name); tool != nil {
		if known := declaredProperties(tool.InputSchema); known != nil {
			kept := map[string]any{}
			for key, value := range arguments {
				if known[key] {
					kept[key] = value
				}
			}
			arguments = kept
		}
	}
	return s.Call(ctx, name, arguments)
}

// declaredProperties 读出参数 schema 里的属性名；读不出时返回 nil（不过滤）。
func declaredProperties(schema any) map[string]bool {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.Properties == nil {
		return nil
	}
	known := map[string]bool{}
	for key := range parsed.Properties {
		known[key] = true
	}
	return known
}

// Close 断开（本地子进程会被结束）。
func (s *MCPServer) Close() error { return s.session.Close() }

// resultText 优先用结构化结果（JSON），没有就拼接文本内容。
func resultText(result *mcp.CallToolResult) string {
	if result.StructuredContent != nil {
		if data, err := json.Marshal(result.StructuredContent); err == nil {
			return string(data)
		}
	}
	return contentText(result)
}

// contentText 拼接结果里的文本内容（出错时的说明在这里）。
func contentText(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// withHeaders 返回一个每个请求都带上 headers 的客户端（不改原客户端）。
func withHeaders(client *http.Client, headers map[string]string) *http.Client {
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone := *client
	clone.Transport = headerTransport{base: base, headers: headers}
	return &clone
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for key, value := range t.headers {
		request.Header.Set(key, value)
	}
	return t.base.RoundTrip(request)
}

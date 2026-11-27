package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
)

func writeServersFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

// 配置文件：格式照 .mcp.json；headers、url 里的 ${名字} 换成环境变量的值；写错的地方都起不来，不静默用别的值。
func TestLoadRemoteServers(t *testing.T) {
	path := writeServersFile(t, `{"mcpServers": {"ops": {"type": "http", "url": "http://ops.example.com:${OPS_PORT}/mcp",
		"headers": {"Authorization": "Bearer ${OPS_TOKEN}"}, "tools": ["cluster_status", "restart_pod"], "confirm": ["restart_pod"]}}}`)
	servers, err := LoadRemoteServers(path, lookupFrom(map[string]string{"OPS_TOKEN": "token-1", "OPS_PORT": "8080"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].URL != "http://ops.example.com:8080/mcp" || servers[0].Headers["Authorization"] != "Bearer token-1" ||
		!slices.Equal(servers[0].Tools, []string{"cluster_status", "restart_pod"}) || !slices.Equal(servers[0].Confirm, []string{"restart_pod"}) {
		t.Errorf("读出的配置：%+v", servers)
	}

	env := lookupFrom(map[string]string{"OPS_TOKEN": "token-1", "EMPTY": ""})
	for name, content := range map[string]string{
		"认不出的字段":       `{"mcpServers": {"ops": {"type": "http", "url": "http://ops.example.com/mcp", "tools": ["a"], "tool": ["b"]}}}`,
		"没列允许名单":       `{"mcpServers": {"ops": {"type": "http", "url": "http://ops.example.com/mcp"}}}`,
		"确认的不在名单里":     `{"mcpServers": {"ops": {"type": "http", "url": "http://ops.example.com/mcp", "tools": ["a"], "confirm": ["b"]}}}`,
		"环境变量没设":       `{"mcpServers": {"ops": {"type": "http", "url": "http://ops.example.com/mcp", "headers": {"X": "${NOPE}"}, "tools": ["a"]}}}`,
		"环境变量是空的":      `{"mcpServers": {"ops": {"type": "http", "url": "http://ops.example.com/mcp", "headers": {"X": "${EMPTY}"}, "tools": ["a"]}}}`,
		"在本机拉起程序":      `{"mcpServers": {"ops": {"type": "stdio", "url": "http://ops.example.com/mcp", "tools": ["a"]}}}`,
		"地址不是 http(s)": `{"mcpServers": {"ops": {"type": "http", "url": "ops.example.com/mcp", "tools": ["a"]}}}`,
		"名字有大写":        `{"mcpServers": {"Ops": {"type": "http", "url": "http://ops.example.com/mcp", "tools": ["a"]}}}`,
		"一个服务都没有":      `{"mcpServers": {}}`,
	} {
		if _, err := LoadRemoteServers(writeServersFile(t, content), env); err == nil {
			t.Errorf("%s：应起不来", name)
		}
	}
}

// fakeOpsServer 是一个外部 MCP 服务：只认带对令牌的请求；ready 为 false 时一律回 503（模拟还没起来）。
func fakeOpsServer(t *testing.T, ready *atomic.Bool) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "ops", Version: "1.0.0"}, nil)
	notDestructive := false
	text := func(reply string) mcp.ToolHandler {
		return func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply}}}, nil
		}
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"cluster": map[string]any{"type": "string"}}}
	server.AddTool(&mcp.Tool{Name: "cluster_status", InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		text("3 个节点都正常"))
	server.AddTool(&mcp.Tool{Name: "restart_pod", InputSchema: schema}, text("重启了")) // 没标注：按规范算会破坏
	server.AddTool(&mcp.Tool{Name: "scale_down", InputSchema: schema, Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive}},
		text("缩了"))
	server.AddTool(&mcp.Tool{Name: "delete_cluster", InputSchema: schema}, text("删了"))
	protocol := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		protocol.ServeHTTP(w, r)
	}))
	t.Cleanup(remote.Close)
	return remote
}

func offeredNames(box *Toolbox) []string {
	var names []string
	for _, spec := range box.Specs() {
		names = append(names, spec.Name)
	}
	return names
}

// 只开放允许名单里的、服务那边有的工具；要不要主人确认 = 服务标了会破坏（没标注的按规范也算）或配置里列了。
func TestRemoteServerAllowlistAndConfirmation(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	remote := fakeOpsServer(t, &ready)
	box, err := NewToolbox(t.Context(), quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	err = box.AttachRemoteServers(t.Context(), []RemoteServer{{Name: "ops", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer token-1"},
		Tools: []string{"cluster_status", "restart_pod", "scale_down", "gone_tool"}, Confirm: []string{"scale_down"}}}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if got := offeredNames(box); !slices.Equal(got, []string{"cluster_status", "restart_pod", "scale_down"}) {
		t.Fatalf("开放的工具：%v（delete_cluster 不在名单里，gone_tool 服务那边没有）", got)
	}
	for _, spec := range box.Specs() {
		if want := spec.Name != "cluster_status"; spec.RequiresConfirmation != want {
			t.Errorf("%s 要不要确认：%v", spec.Name, spec.RequiresConfirmation)
		}
	}
	call := func(ctx context.Context, name string) (string, error) {
		return box.Call(ctx, brain.ToolCall{Name: name, Arguments: map[string]any{"cluster": "home"}})
	}
	if got, err := call(t.Context(), "cluster_status"); err != nil || got != "3 个节点都正常" {
		t.Errorf("只读的直接执行：%q %v", got, err)
	}
	for _, name := range []string{"restart_pod", "scale_down"} {
		if _, err := call(t.Context(), name); !errors.Is(err, brain.ErrNotConfirmed) {
			t.Errorf("%s 没有主人的确认不该执行：%v", name, err)
		}
		if got, err := call(confirmedFor(name), name); err != nil || got == "" {
			t.Errorf("%s 确认过就执行：%q %v", name, got, err)
		}
	}
	if _, err := call(t.Context(), "delete_cluster"); err == nil {
		t.Error("名单外的工具不该能调用")
	}
}

// 启动时连不上不影响启动；在后台重连，服务起来后它的工具才出现在清单里。
func TestRemoteServerAttachesWhenReachable(t *testing.T) {
	savedFirst, savedLatest := remoteReconnectFirst, remoteReconnectLatest
	remoteReconnectFirst, remoteReconnectLatest = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { remoteReconnectFirst, remoteReconnectLatest = savedFirst, savedLatest })
	var ready atomic.Bool
	remote := fakeOpsServer(t, &ready)
	box, err := NewToolbox(t.Context(), quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if err := box.AttachRemoteServers(t.Context(), []RemoteServer{{Name: "ops", URL: remote.URL,
		Headers: map[string]string{"Authorization": "Bearer token-1"}, Tools: []string{"cluster_status"}}}, &http.Client{}); err != nil {
		t.Fatalf("连不上不该让启动失败：%v", err)
	}
	if got := offeredNames(box); len(got) != 0 {
		t.Fatalf("还没连上时不该有它的工具：%v", got)
	}
	ready.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(offeredNames(box), "cluster_status") {
		if time.Now().After(deadline) {
			t.Fatal("服务起来后工具应加进清单")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, err := box.Call(t.Context(), brain.ToolCall{Name: "cluster_status", Arguments: map[string]any{}}); err != nil || !strings.Contains(got, "正常") {
		t.Errorf("连上后能调用：%q %v", got, err)
	}
}

// 外部工具和内置工具重名：启动就报错（模型看到两个同名工具，调用哪个说不清）。
func TestRemoteToolNameClashFailsAtStartup(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	remote := fakeOpsServer(t, &ready)
	clash := func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: "cluster_status", Annotations: ReadOnly}, func(context.Context, struct{}) (struct{}, error) {
			return struct{}{}, nil
		})
	}
	box, err := NewToolbox(t.Context(), quietLogger, clash)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	err = box.AttachRemoteServers(t.Context(), []RemoteServer{{Name: "ops", URL: remote.URL,
		Headers: map[string]string{"Authorization": "Bearer token-1"}, Tools: []string{"cluster_status"}}}, &http.Client{})
	if err == nil || !strings.Contains(err.Error(), "cluster_status") {
		t.Errorf("重名应报错：%v", err)
	}
}

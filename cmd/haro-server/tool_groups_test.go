package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/config"
)

func offeredToolNames(kit tooling) []string {
	if kit.toolbox == nil {
		return nil
	}
	var names []string
	for _, spec := range kit.toolbox.Specs() {
		names = append(names, spec.Name)
	}
	return names
}

// HARO_TOOLS 只列出的组才装：只开天气时没有记事、提醒、搜索、调查的工具（也不用记事库）。
func TestToolGroupsSelectBuiltins(t *testing.T) {
	all := offeredToolNames(fullTooling(t))
	weatherOnly := offeredToolNames(toolingWith(t, func(s *config.ServerSettings) {
		s.ToolGroups, s.NotesDB = []string{config.ToolGroupWeather}, ""
	}))
	if len(weatherOnly) == 0 || len(weatherOnly) >= len(all) {
		t.Fatalf("只开天气：%v（全开：%v）", weatherOnly, all)
	}
	for _, name := range weatherOnly {
		if !slices.Contains(all, name) {
			t.Errorf("只开天气时多出了 %s", name)
		}
	}
	if slices.Contains(weatherOnly, "web_search") || slices.Contains(weatherOnly, "remember") {
		t.Errorf("只开天气时不该有搜索、记事：%v", weatherOnly)
	}
	if kit := toolingWith(t, func(s *config.ServerSettings) { s.ToolGroups = []string{config.ToolGroupsNone} }); kit.toolbox != nil {
		t.Errorf("none = 一个工具都没有：%v", offeredToolNames(kit))
	}
}

// HARO_MCP_SERVERS_FILE 里列的外部服务：允许名单里的工具装进清单（和内置的同一份），凭证从环境变量来。
func TestExternalServersFromFile(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "ops", Version: "1.0.0"}, nil)
	reply := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	schema := map[string]any{"type": "object"}
	server.AddTool(&mcp.Tool{Name: "cluster_status", InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, reply)
	server.AddTool(&mcp.Tool{Name: "drain_node", InputSchema: schema}, reply)
	protocol := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		protocol.ServeHTTP(w, r)
	}))
	defer remote.Close()
	t.Setenv("TEST_OPS_TOKEN", "secret-1")
	file := filepath.Join(t.TempDir(), "mcp.json")
	content := `{"mcpServers": {"ops": {"type": "http", "url": "` + remote.URL + `",
		"headers": {"Authorization": "Bearer ${TEST_OPS_TOKEN}"}, "tools": ["cluster_status"]}}}`
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	kit := toolingWith(t, func(s *config.ServerSettings) {
		s.ToolGroups, s.MCPServersFile = []string{config.ToolGroupWeather}, file
	})
	names := offeredToolNames(kit)
	if !slices.Contains(names, "cluster_status") || slices.Contains(names, "drain_node") {
		t.Errorf("外部服务只开放允许名单里的：%v", names)
	}
}

package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"haro/internal/brain"
	"haro/internal/netdiag"
	"haro/internal/notes"
)

// buildFakeServer 把测试用的第三方 MCP 服务（testdata/fakemcp）编译到临时目录。
func buildFakeServer(t *testing.T) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "fakemcp")
	cmd := exec.Command("go", "build", "-o", output, "./testdata/fakemcp")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("编译 fakemcp: %v\n%s", err, out)
	}
	return output
}

// 第三方 MCP 服务：启动子进程、汇总工具、按 MCP 标注决定要不要口头确认、转发调用。
func TestToolboxWithThirdPartyServer(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过（要编译并启动 MCP 服务）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	box, err := NewToolbox(ctx, []ServerSpec{{Name: "fakemcp", Command: buildFakeServer(t)}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	confirmation := map[string]bool{}
	for _, spec := range box.Specs() {
		confirmation[spec.Name] = spec.RequiresConfirmation
		if !json.Valid(spec.Parameters) {
			t.Errorf("%s 的参数 schema 不是合法 JSON", spec.Name)
		}
	}
	want := map[string]bool{"get_current_time": false, "read_echo": false, "add_echo": false, "wipe_echo": true}
	for name, needs := range want {
		got, ok := confirmation[name]
		if !ok || got != needs {
			t.Errorf("工具 %s：存在=%v 需要确认=%v，应为需要确认=%v", name, ok, got, needs)
		}
	}
	if echoed, err := box.Call(ctx, brain.ToolCall{Name: "read_echo", Arguments: map[string]any{"text": "你好"}}); err != nil || echoed != `{"echo":"你好"}` {
		t.Errorf("read_echo：%s %v", echoed, err)
	}
	if _, err := box.Call(ctx, brain.ToolCall{Name: "no_such_tool"}); err == nil {
		t.Error("不存在的工具应返回错误")
	}
}

// 记事本内置在大脑里（v5）：工具名、要不要确认、结果格式与原 haro-notes 一致。
func TestNoteToolsBuiltin(t *testing.T) {
	ctx := context.Background()
	store, err := notes.Open(ctx, filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, err := NewToolbox(ctx, nil, logger, NoteTools(store, logger)...)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	confirmation := map[string]bool{}
	for _, spec := range box.Specs() {
		confirmation[spec.Name] = spec.RequiresConfirmation
		if !json.Valid(spec.Parameters) {
			t.Errorf("%s 的参数 schema 不是合法 JSON", spec.Name)
		}
	}
	want := map[string]bool{"get_current_time": false, "save_note": false, "search_notes": false,
		"list_recent_notes": false, "delete_note": true}
	for name, needs := range want {
		got, ok := confirmation[name]
		if !ok || got != needs {
			t.Errorf("工具 %s：存在=%v 需要确认=%v，应为需要确认=%v", name, ok, got, needs)
		}
	}

	saved, err := box.Call(ctx, brain.ToolCall{Name: "save_note", Arguments: map[string]any{"content": "车停在C3的7号位。"}})
	if err != nil || !strings.Contains(saved, "C3") {
		t.Fatalf("save_note：%s %v", saved, err)
	}
	found, err := box.Call(ctx, brain.ToolCall{Name: "search_notes", Arguments: map[string]any{"query": "车停在哪"}})
	if err != nil || !strings.Contains(found, "C3的7号位") {
		t.Errorf("search_notes：%s %v", found, err)
	}
	if recent, err := box.Call(ctx, brain.ToolCall{Name: "list_recent_notes"}); err != nil || !strings.Contains(recent, "C3") {
		t.Errorf("list_recent_notes：%s %v", recent, err)
	}
	// 一次删多条（v3 D-1）：找到的删掉、找不到的写进 not_found（不再整体报错）
	deleted, err := box.Call(ctx, brain.ToolCall{Name: "delete_note",
		Arguments: map[string]any{"queries": []any{"车停在C3的7号位", "天气预报"}}})
	if err != nil || !strings.Contains(deleted, `"deleted":["车停在C3的7号位。"]`) || !strings.Contains(deleted, `"not_found":["天气预报"]`) {
		t.Errorf("delete_note：%s %v", deleted, err)
	}
	if found, _ := box.Call(ctx, brain.ToolCall{Name: "search_notes", Arguments: map[string]any{"query": "车停在哪"}}); found != `{"notes":[]}` {
		t.Errorf("删掉后不应再查到：%s", found)
	}
	if _, err := box.Call(ctx, brain.ToolCall{Name: "delete_note", Arguments: map[string]any{"queries": []any{}}}); err == nil {
		t.Error("queries 为空应报错")
	}
	if _, err := box.Call(ctx, brain.ToolCall{Name: "save_note", Arguments: map[string]any{"content": 3}}); err == nil {
		t.Error("参数类型不对应报错")
	}
	if now, err := box.Call(ctx, brain.ToolCall{Name: "get_current_time"}); err != nil || !strings.Contains(now, "星期") {
		t.Errorf("get_current_time：%s %v", now, err)
	}
}

func TestNetworkDiagnosticsBuiltins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, err := NewToolbox(context.Background(), nil, logger, NetworkDiagnostics(netdiag.NewChecker("localhost", "127.0.0.1:1"))...)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	names := map[string]bool{}
	for _, spec := range box.Specs() {
		names[spec.Name] = true
		if spec.RequiresConfirmation {
			t.Errorf("%s 是只读工具，不应要求确认", spec.Name)
		}
	}
	if !names["get_current_time"] || !names["check_network"] || !names["network_interfaces"] {
		t.Fatalf("内置工具不全：%v", names)
	}
	result, err := box.Call(context.Background(), brain.ToolCall{Name: "network_interfaces"})
	if err != nil || !strings.Contains(result, "interfaces") {
		t.Errorf("network_interfaces 应在本机直接执行：%q %v", result, err)
	}
}

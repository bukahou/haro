package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/notes"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

type echoInput struct {
	Text string `json:"text"`
}

type echoOutput struct {
	Echo string `json:"echo"`
}

// echoTools 是三个标注各不相同的工具（只读、只新增、没有标注）。
func echoTools(server *mcp.Server) {
	echo := func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput{Echo: in.Text}, nil
	}
	mcp.AddTool(server, &mcp.Tool{Name: "read_echo", Description: "只读", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, echo)
	mcp.AddTool(server, &mcp.Tool{Name: "add_echo", Description: "只新增", Annotations: &mcp.ToolAnnotations{DestructiveHint: new(false)}}, echo)
	mcp.AddTool(server, &mcp.Tool{Name: "wipe_echo", Description: "没有标注：按规范视为有破坏性"}, echo)
}

func confirmations(box *Toolbox) map[string]bool {
	needs := map[string]bool{}
	for _, spec := range box.Specs() {
		needs[spec.Name] = spec.RequiresConfirmation
	}
	return needs
}

// 工具清单按 MCP 标注决定要不要口头确认（只读的不要；非只读时 destructiveHint 缺省按有破坏性），调用转给工具服务。
func TestToolboxConfirmationFollowsAnnotations(t *testing.T) {
	ctx := context.Background()
	box, err := NewToolbox(ctx, quietLogger, echoTools)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	for _, spec := range box.Specs() {
		if !json.Valid(spec.Parameters) {
			t.Errorf("%s 的参数定义不是合法 JSON", spec.Name)
		}
	}
	want := map[string]bool{"read_echo": false, "add_echo": false, "wipe_echo": true}
	if got := confirmations(box); len(got) != len(want) {
		t.Errorf("工具清单：%v", got)
	} else {
		for name, needs := range want {
			if got[name] != needs {
				t.Errorf("%s：需要确认应为 %v", name, needs)
			}
		}
	}
	if echoed, err := box.Call(ctx, brain.ToolCall{Name: "read_echo", Arguments: map[string]any{"text": "你好"}}); err != nil || echoed != `{"echo":"你好"}` {
		t.Errorf("read_echo：%s %v", echoed, err)
	}
	if _, err := box.Call(ctx, brain.ToolCall{Name: "no_such_tool"}); err == nil {
		t.Error("不存在的工具应返回错误")
	}
}

// 和工具服务握手时报的版本是程序的版本号：进程内的内置服务、连远程服务的客户端都是。
func TestToolboxReportsProgramVersion(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "v0.0.0-test"
	t.Cleanup(func() { buildinfo.Version = saved })
	clientVersions := make(chan string, 1)
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0.0.1"}, &mcp.ServerOptions{
		InitializedHandler: func(_ context.Context, request *mcp.InitializedRequest) {
			clientVersions <- request.Session.InitializeParams().ClientInfo.Version
		}})
	mcp.AddTool(server, &mcp.Tool{Name: "read_echo", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
			return nil, echoOutput{Echo: in.Text}, nil
		})
	remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer remote.Close()

	box, err := NewToolbox(context.Background(), quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if got := box.servers[0].session.InitializeResult().ServerInfo.Version; got != buildinfo.Version {
		t.Errorf("内置服务报的版本：%q", got)
	}
	client, err := ConnectMCPServer(context.Background(), ServerSpec{Name: "fake", URL: remote.URL}, quietLogger) // 联网搜索走这条路
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case got := <-clientVersions:
		if got != buildinfo.Version {
			t.Errorf("连远程服务时报的客户端版本：%q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("远程服务没收到握手")
	}
}

// 工具重名（内置之间、内置和远程之间）：启动就报错，不让模型看到两个同名工具。
func TestDuplicateToolNamesFailAtStartup(t *testing.T) {
	ctx := context.Background()
	greet := func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: "say_hi", Annotations: ReadOnly}, func(context.Context, struct{}) (struct{}, error) {
			return struct{}{}, nil
		})
	}
	if _, err := NewToolbox(ctx, quietLogger, greet, greet); err == nil || !strings.Contains(err.Error(), "say_hi") {
		t.Errorf("内置工具重名应启动失败：%v", err)
	}
}

// spoken 是能直接念给主人的错误。
type spoken struct{}

func (spoken) Error() string { return "额度用完了" }
func (spoken) Phrase() (string, map[string]string) {
	return brain.PhraseOverMonthlyBudget, map[string]string{"spent": "3100", "limit": "3000"}
}

// collected 收下工具登记的「主人听完才算数」的事。
type collected struct {
	mu   sync.Mutex
	list []func()
}

func (c *collected) Add(delivered func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, delivered)
}

// 过了 MCP 这层：说话人和「主人听完才算数的事」经元数据带到工具里；能念给主人的错误原样还原；参数按定义校验。
func TestBuiltinToolsGetSpeakerDeliveriesAndSpokenErrors(t *testing.T) {
	ctx := context.Background()
	probe := func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: "whoami", Annotations: ReadOnly}, func(ctx context.Context, in echoInput) (echoOutput, error) {
			brain.OnDelivered(ctx, func() {})
			return echoOutput{Echo: brain.SpeakerFrom(ctx) + ":" + in.Text}, nil
		})
		AddTool(server, &mcp.Tool{Name: "over_budget", Annotations: ReadOnly}, func(context.Context, struct{}) (echoOutput, error) {
			return echoOutput{}, spoken{}
		})
	}
	box, err := NewToolbox(ctx, quietLogger, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	var heard collected
	turn := brain.WithDeliveries(brain.WithSpeaker(ctx, "主人"), &heard)
	if result, err := box.Call(turn, brain.ToolCall{Name: "whoami", Arguments: map[string]any{"text": "嗨"}}); err != nil ||
		result != `{"echo":"主人:嗨"}` || len(heard.list) != 1 {
		t.Errorf("说话人、登记的事要带到工具里：%s %v（登记了 %d 件）", result, err, len(heard.list))
	}
	if _, err := box.Call(ctx, brain.ToolCall{Name: "whoami", Arguments: map[string]any{"text": 3}}); err == nil {
		t.Error("参数类型不对应在调用前就报错")
	}
	_, err = box.Call(ctx, brain.ToolCall{Name: "over_budget"})
	var spokenErr brain.SpokenError
	if !errors.As(err, &spokenErr) {
		t.Fatalf("能念给主人的错误要原样还原：%v", err)
	}
	if key, values := spokenErr.Phrase(); key != brain.PhraseOverMonthlyBudget || values["spent"] != "3100" || values["limit"] != "3000" {
		t.Errorf("那句话的键和数要原样带过来：%s %v", key, values)
	}
}

func callJSON[Out any](t *testing.T, box *Toolbox, name string, arguments map[string]any) (Out, error) {
	t.Helper()
	return callJSONWith[Out](t, context.Background(), box, name, arguments)
}

// callJSONWith 同 callJSON，调用时用 ctx（例如带着主人的确认）。
func callJSONWith[Out any](t *testing.T, ctx context.Context, box *Toolbox, name string, arguments map[string]any) (Out, error) {
	t.Helper()
	var out Out
	result, err := box.Call(ctx, brain.ToolCall{Name: name, Arguments: arguments})
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("%s 的结果不是 JSON：%s", name, result)
	}
	return out, nil
}

// confirmedFor 是带着主人对 tool 的确认的 ctx（确认闸执行时就是这样带的）。
func confirmedFor(tool string) context.Context {
	return brain.WithConfirmation(context.Background(), brain.Confirmation{ActionID: "act_1", Tool: tool, Person: "主人"})
}

// 会破坏的工具（delete_note）由工具箱自己守门：绕过会话直接调用、带着别的工具的确认、确认的人没认出来，都不执行
// （记事还在）；阳性对照：带着主人对它的确认就删掉。不会破坏的工具不用确认。
func TestDestructiveToolNeedsConfirmation(t *testing.T) {
	ctx := context.Background()
	store, err := notes.Open(ctx, filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	box, err := NewToolbox(ctx, quietLogger, NoteTools(store, 10, quietLogger))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	saved, err := callJSON[noteOutput](t, box, "save_note", map[string]any{"content": "车停在C3的7号位。"})
	if err != nil {
		t.Fatalf("不会破坏的工具不用确认：%v", err)
	}
	remove := map[string]any{"ids": []any{saved.Note.ID}}
	for label, call := range map[string]context.Context{
		"绕过会话直接调用":  ctx,
		"带着别的工具的确认": confirmedFor("raise_monthly_cap"),
		"确认的人没认出来":  brain.WithConfirmation(ctx, brain.Confirmation{ActionID: "act_1", Tool: "delete_note", Person: brain.SharedPerson}),
	} {
		if _, err := box.Call(call, brain.ToolCall{Name: "delete_note", Arguments: remove}); !errors.Is(err, brain.ErrNotConfirmed) {
			t.Errorf("%s：应拒绝执行：%v", label, err)
		}
	}
	if list, _, err := store.List(ctx, 10); err != nil || len(list) != 1 {
		t.Fatalf("没确认的删除不该执行：%v %v", list, err)
	}
	deleted, err := callJSONWith[deleteNotesOutput](t, confirmedFor("delete_note"), box, "delete_note", remove)
	if err != nil || len(deleted.Deleted) != 1 {
		t.Errorf("带着主人的确认应能删：%+v %v", deleted, err)
	}
}

// 记事本：记、列出（连编号交给模型挑，超过上限时写明一共几条）、按编号删（只有删要确认）、最近删掉的、按编号恢复。
func TestNoteTools(t *testing.T) {
	ctx := context.Background()
	store, err := notes.Open(ctx, filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	box, err := NewToolbox(ctx, quietLogger, NoteTools(store, 2, quietLogger))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	want := map[string]bool{"save_note": false, "search_notes": false, "delete_note": true,
		"list_deleted_notes": false, "restore_note": false}
	if got := confirmations(box); len(got) != len(want) {
		t.Fatalf("工具清单：%v", got)
	} else {
		for name, needs := range want {
			if got[name] != needs {
				t.Errorf("%s：需要确认应为 %v", name, needs)
			}
		}
	}

	var ids []int64
	for _, content := range []string{"车停在C3的7号位。", "钥匙在玄关的抽屉里。"} {
		saved, err := callJSON[noteOutput](t, box, "save_note", map[string]any{"content": content})
		if err != nil || saved.Note.ID == 0 || saved.Note.Content != content {
			t.Fatalf("save_note：%+v %v", saved, err)
		}
		ids = append(ids, saved.Note.ID)
	}
	listed, err := callJSON[noteListOutput](t, box, "search_notes", nil)
	if err != nil || listed.Total != 2 || len(listed.Notes) != 2 || listed.Note != "" || listed.Notes[0].ID != ids[1] {
		t.Fatalf("search_notes：全部连编号给出，新的在前：%+v %v", listed, err)
	}
	if _, err := callJSON[noteOutput](t, box, "save_note", map[string]any{"content": "第三条"}); err != nil {
		t.Fatal(err)
	}
	if listed, _ := callJSON[noteListOutput](t, box, "search_notes", nil); listed.Total != 3 || len(listed.Notes) != 2 || listed.Note == "" {
		t.Errorf("超过上限：只给最近的，并写明一共几条：%+v", listed)
	}

	confirmed := confirmedFor("delete_note") // 删除要带着主人的确认（见 TestDestructiveToolNeedsConfirmation）
	deleted, err := callJSONWith[deleteNotesOutput](t, confirmed, box, "delete_note", map[string]any{"ids": []any{ids[0], 999}})
	if err != nil || len(deleted.Deleted) != 1 || deleted.Deleted[0].Content != "车停在C3的7号位。" ||
		len(deleted.NotFound) != 1 || deleted.NotFound[0] != 999 {
		t.Errorf("delete_note：删掉有的、没有的编号写进 not_found：%+v %v", deleted, err)
	}
	if _, err := box.Call(confirmed, brain.ToolCall{Name: "delete_note", Arguments: map[string]any{"ids": []any{}}}); err == nil ||
		errors.Is(err, brain.ErrNotConfirmed) {
		t.Errorf("ids 为空应报错：%v", err)
	}
	gone, err := callJSON[noteListOutput](t, box, "list_deleted_notes", nil)
	if err != nil || len(gone.Notes) != 1 || gone.Notes[0].ID != ids[0] || gone.Notes[0].DeletedAt == "" {
		t.Errorf("list_deleted_notes：%+v %v", gone, err)
	}
	if restored, err := callJSON[noteOutput](t, box, "restore_note", map[string]any{"id": ids[0]}); err != nil || restored.Note.ID != ids[0] {
		t.Errorf("restore_note：%+v %v", restored, err)
	}
	if _, err := box.Call(ctx, brain.ToolCall{Name: "restore_note", Arguments: map[string]any{"id": ids[0]}}); err == nil {
		t.Error("恢复没删掉的应报错")
	}
	if _, err := box.Call(ctx, brain.ToolCall{Name: "save_note", Arguments: map[string]any{"content": 3}}); err == nil {
		t.Error("参数类型不对应报错")
	}
}

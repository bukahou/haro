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
	"github.com/bukahou/haro/internal/memory"
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

// openMemory 建一个只在内存里的长期记忆（不按意思找：工具的测试不用嵌入服务）。
func openMemory(t *testing.T) (*memory.Keeper, notes.Repository) {
	t.Helper()
	store, err := notes.Open(context.Background(), filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return memory.NewKeeper(store, nil, nil, 2, quietLogger), store
}

// owner 是主人（声纹认出）在说话的一轮。
func owner(ctx context.Context) context.Context {
	return brain.WithVoiceVerified(brain.WithSpeaker(ctx, "主人"), true)
}

// 会破坏的工具（forget）由工具箱自己守门：绕过会话直接调用、带着别的工具的确认、确认的人没认出来，都不执行
// （记忆还在）；阳性对照：带着主人对它的确认就忘掉。不会破坏的工具不用确认。
func TestDestructiveToolNeedsConfirmation(t *testing.T) {
	ctx := context.Background()
	keeper, store := openMemory(t)
	box, err := NewToolbox(ctx, quietLogger, MemoryTools(keeper, time.UTC, quietLogger))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	saved, err := callJSONWith[memoryOutput](t, owner(ctx), box, RememberTool, map[string]any{"content": "车停在C3的7号位。", "source": "owner"})
	if err != nil {
		t.Fatalf("不会破坏的工具不用确认：%v", err)
	}
	forget := map[string]any{"ids": []any{saved.Memory.ID}}
	for label, call := range map[string]context.Context{
		"绕过会话直接调用":  owner(ctx),
		"带着别的工具的确认": brain.WithSpeaker(confirmedFor("raise_monthly_cap"), "主人"),
		"确认的人没认出来":  brain.WithConfirmation(ctx, brain.Confirmation{ActionID: "act_1", Tool: ForgetTool, Person: brain.SharedPerson}),
	} {
		if _, err := box.Call(call, brain.ToolCall{Name: ForgetTool, Arguments: forget}); !errors.Is(err, brain.ErrNotConfirmed) {
			t.Errorf("%s：应拒绝执行：%v", label, err)
		}
	}
	if list, _, err := store.List(ctx, "主人", 10); err != nil || len(list) != 1 {
		t.Fatalf("没确认的忘掉不该执行：%v %v", list, err)
	}
	forgotten, err := callJSONWith[forgetOutput](t, brain.WithSpeaker(confirmedFor(ForgetTool), "主人"), box, ForgetTool, forget)
	if err != nil || len(forgotten.Forgotten) != 1 {
		t.Errorf("带着主人的确认应能忘掉：%+v %v", forgotten, err)
	}
}

// 长期记忆的五个工具：记住（只认声纹认出的这句，来源必须是认得的）、按意思找（没配嵌入服务时给最近的并写明）、
// 列出（超过上限时写明一共几条）、按编号忘掉（只有它要确认；没有的写进 not_found）、恢复（只认声纹认出的这句）；只看得到自己的。
func TestMemoryTools(t *testing.T) {
	ctx := context.Background()
	keeper, _ := openMemory(t)
	box, err := NewToolbox(ctx, quietLogger, MemoryTools(keeper, time.UTC, quietLogger))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	want := map[string]bool{RememberTool: false, RecallTool: false, ListMemoriesTool: false, ForgetTool: true, RestoreMemoryTool: false}
	if got := confirmations(box); len(got) != len(want) {
		t.Fatalf("工具清单：%v", got)
	} else {
		for name, needs := range want {
			if got[name] != needs {
				t.Errorf("%s：需要确认应为 %v", name, needs)
			}
		}
	}
	remember := map[string]any{"content": "不吃香菜", "source": "model", "resident": true}
	if _, err := callJSONWith[memoryOutput](t, brain.WithSpeaker(ctx, "主人"), box, RememberTool, remember); err == nil ||
		!strings.Contains(err.Error(), "没认出") {
		t.Fatalf("声纹没认出时不能记：%v", err)
	}
	if _, err := callJSONWith[memoryOutput](t, owner(ctx), box, RememberTool, map[string]any{"content": "随便", "source": "guess"}); err == nil {
		t.Error("不认得的来源应报错")
	}
	var ids []int64
	for _, args := range []map[string]any{remember, {"content": "车停在C3的7号位。", "source": "owner"}, {"content": "第三条", "source": "owner"}} {
		saved, err := callJSONWith[memoryOutput](t, owner(ctx), box, RememberTool, args)
		if err != nil || saved.Memory.ID == 0 {
			t.Fatalf("remember：%+v %v", saved, err)
		}
		ids = append(ids, saved.Memory.ID)
	}
	if listed, err := callJSONWith[memoryListOutput](t, owner(ctx), box, ListMemoriesTool, nil); err != nil || listed.Total != 3 ||
		len(listed.Memories) != 2 || listed.Note == "" || listed.Memories[0].ID != ids[2] {
		t.Errorf("超过上限：只给最近的（新的在前），并写明一共几条：%+v %v", listed, err)
	}
	other := brain.WithVoiceVerified(brain.WithSpeaker(ctx, "老大"), true)
	if listed, _ := callJSONWith[memoryListOutput](t, other, box, ListMemoriesTool, nil); listed.Total != 0 {
		t.Errorf("只看得到自己的：%+v", listed)
	}
	recalled, err := callJSONWith[recallOutput](t, owner(ctx), box, RecallTool, map[string]any{"query": "主人有什么忌口"})
	if err != nil || len(recalled.Memories) == 0 || recalled.Note == "" || len(recalled.Conversations) != 0 {
		t.Errorf("没配嵌入服务：给最近的并写明，对话查不了：%+v %v", recalled, err)
	}
	forgotten, err := callJSONWith[forgetOutput](t, brain.WithSpeaker(confirmedFor(ForgetTool), "主人"), box, ForgetTool,
		map[string]any{"ids": []any{ids[1], 999}})
	if err != nil || len(forgotten.Forgotten) != 1 || len(forgotten.NotFound) != 1 || forgotten.NotFound[0] != 999 {
		t.Errorf("forget：忘掉有的、没有的写进 not_found：%+v %v", forgotten, err)
	}
	gone, err := callJSONWith[memoryListOutput](t, owner(ctx), box, ListMemoriesTool, map[string]any{"include_forgotten": true})
	if err != nil || len(gone.Forgotten) != 1 || gone.Forgotten[0].ID != ids[1] || gone.Forgotten[0].ForgottenAt == "" {
		t.Errorf("列出最近忘掉的：%+v %v", gone, err)
	}
	restore := map[string]any{"id": ids[1]}
	if _, err := callJSONWith[memoryOutput](t, brain.WithSpeaker(ctx, "主人"), box, RestoreMemoryTool, restore); err == nil {
		t.Error("声纹没认出时不能恢复")
	}
	if restored, err := callJSONWith[memoryOutput](t, owner(ctx), box, RestoreMemoryTool, restore); err != nil || restored.Memory.ID != ids[1] {
		t.Errorf("恢复：%+v %v", restored, err)
	}
	if _, err := box.Call(owner(ctx), brain.ToolCall{Name: RestoreMemoryTool, Arguments: restore}); err == nil {
		t.Error("没忘掉的恢复不了")
	}
}

package braintest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// recorder 记下替身报的错，用来测替身自己。
type recorder struct {
	mu     sync.Mutex
	errors []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
func (r *recorder) reported() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

func drain(t *testing.T, model *Model, ctx context.Context, messages []brain.ChatMessage) (string, []brain.ToolCall, error) {
	t.Helper()
	var text strings.Builder
	var calls []brain.ToolCall
	for delta, err := range model.StreamReply(ctx, messages, nil) {
		if err != nil {
			return text.String(), calls, err
		}
		text.WriteString(delta.Text)
		calls = append(calls, delta.ToolCalls...)
	}
	return text.String(), calls, nil
}

func TestModelFollowsScriptAndRecordsRequests(t *testing.T) {
	model := NewModel(t, Reply{Chunks: []string{"你", "好"}, Usage: &brain.ReplyUsage{OutputTokens: 2}}, Call("search_notes", nil))
	input := []brain.ChatMessage{{Role: brain.RoleUser, Text: "随便说什么"}}
	if text, _, err := drain(t, model, context.Background(), input); err != nil || text != "你好" {
		t.Fatalf("第一次：%q %v", text, err)
	}
	if _, calls, err := drain(t, model, context.Background(), input); err != nil || len(calls) != 1 || calls[0].Name != "search_notes" {
		t.Fatalf("第二次应要求调用工具：%+v %v", calls, err)
	}
	if got := model.Requests(); len(got) != 2 || got[1].Input().Text != "随便说什么" {
		t.Errorf("应记下两次请求：%+v", got)
	}
	if model.Remaining() != 0 {
		t.Errorf("脚本应已用完：还剩 %d", model.Remaining())
	}
}

func TestModelReportsExtraRequests(t *testing.T) {
	report := &recorder{}
	model := NewModel(report)
	if _, _, err := drain(t, model, context.Background(), nil); !errors.Is(err, ErrScriptExhausted) {
		t.Errorf("脚本用完时应返回 ErrScriptExhausted：%v", err)
	}
	if got := report.reported(); len(got) != 1 || !strings.Contains(got[0], "脚本已经用完") {
		t.Errorf("应报告多出来的请求：%v", got)
	}
}

func TestModelStopsWhenCancelled(t *testing.T) {
	model := NewModel(t, Reply{Chunks: []string{"很", "慢"}, Delay: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := drain(t, model, ctx, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("取消后应返回 context.Canceled：%v", err)
	}
}

func TestCheckToolPairs(t *testing.T) {
	call := brain.ToolCall{ID: "call_1", Name: "search_notes"}
	result := brain.ChatMessage{Role: brain.RoleTool, ToolName: "search_notes", ToolCallID: "call_1", Text: "结果"}
	asked := brain.ChatMessage{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{call}}
	cases := []struct {
		name     string
		messages []brain.ChatMessage
		ok       bool
	}{
		{"成对", []brain.ChatMessage{{Role: brain.RoleUser}, asked, result, {Role: brain.RoleAssistant, Text: "好"}}, true},
		{"画面排在工具结果之后", []brain.ChatMessage{{Role: brain.RoleUser}, asked, result, {Role: brain.RoleUser, Image: []byte{1}}}, true},
		{"调用没有结果就接着说话", []brain.ChatMessage{{Role: brain.RoleUser}, asked, {Role: brain.RoleUser, Text: "嗯"}}, false},
		{"结果对不上调用", []brain.ChatMessage{{Role: brain.RoleUser}, result}, false},
		{"调用没有编号", []brain.ChatMessage{{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{Name: "x"}}}}, false},
		{"最后的调用没有结果", []brain.ChatMessage{{Role: brain.RoleUser}, asked}, false},
	}
	for _, c := range cases {
		if err := CheckToolPairs(c.messages); (err == nil) != c.ok {
			t.Errorf("%s：err = %v", c.name, err)
		}
	}
}

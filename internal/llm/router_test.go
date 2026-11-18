package llm

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

// namedModel 回答自己的名字，记下被问了几次。
type namedModel struct {
	name  string
	calls int
}

func (m *namedModel) StreamReply(context.Context, []brain.ChatMessage, []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.calls++
	return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{Text: m.name}, nil) }
}

func (m *namedModel) Warmup(context.Context) error { return nil }

func TestRouterChoosesModelByRule(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	haiku, sonnet := &namedModel{name: "haiku"}, &namedModel{name: "sonnet"}
	router := NewRouterChatModel(RouterOptions{Default: haiku, Vision: sonnet, Correction: sonnet,
		VisionName: "sonnet", CorrectionName: "sonnet", Logger: logger})
	system := brain.ChatMessage{Role: brain.RoleSystem, Text: "你是 haro"}
	look := brain.ChatMessage{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "1", Name: "look_at_camera"}}}
	lookResult := brain.ChatMessage{Role: brain.RoleTool, ToolName: "look_at_camera", ToolCallID: "1", Text: "见下一条"}
	frame := brain.ChatMessage{Role: brain.RoleUser, Text: "这是摄像头当前画面。", Image: []byte{0xff, 0xd8}}
	cases := []struct {
		name     string
		messages []brain.ChatMessage
		want     string
	}{
		{"平常的一句", []brain.ChatMessage{system, {Role: brain.RoleUser, Text: "今天几号"}}, "haiku"},
		{"看了画面：带画面的那次请求", []brain.ChatMessage{system, {Role: brain.RoleUser, Text: "这是什么"}, look, lookResult, frame}, "sonnet"},
		{"主人在纠正", []brain.ChatMessage{system, {Role: brain.RoleUser, Text: "那是耳机"}, {Role: brain.RoleAssistant, Text: "是充电宝"},
			{Role: brain.RoleUser, Text: "不对", Escalate: true}}, "sonnet"},
		{"纠正那一轮里调了工具：仍看本轮主人那句", []brain.ChatMessage{system, {Role: brain.RoleUser, Text: "不对", Escalate: true},
			{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "2", Name: "web_search"}}},
			{Role: brain.RoleTool, ToolName: "web_search", ToolCallID: "2", Text: "结果"}}, "sonnet"},
		{"上一轮纠正过，这一轮没有", []brain.ChatMessage{system, {Role: brain.RoleUser, Text: "不对", Escalate: true},
			{Role: brain.RoleAssistant, Text: "抱歉"}, {Role: brain.RoleUser, Text: "好了谢谢"}}, "haiku"},
	}
	for _, c := range cases {
		text, _, _, err := collect(t, router.StreamReply(context.Background(), c.messages, nil))
		if err != nil || text != c.want {
			t.Errorf("%s：交给了 %q（%v），应为 %q", c.name, text, err, c.want)
		}
	}

	onlyVision := NewRouterChatModel(RouterOptions{Default: haiku, Vision: sonnet, VisionName: "sonnet", Logger: logger})
	corrected := []brain.ChatMessage{system, {Role: brain.RoleUser, Text: "不对", Escalate: true}}
	if text, _, _, _ := collect(t, onlyVision.StreamReply(context.Background(), corrected, nil)); text != "haiku" {
		t.Errorf("没配纠正用的模型时，纠正也用默认模型：交给了 %q", text)
	}
}

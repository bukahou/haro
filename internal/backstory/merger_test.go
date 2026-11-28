package backstory

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

// scriptedModel 依次交回准备好的回答（每次一个工具调用的参数；nil = 不调用工具）。
type scriptedModel struct {
	replies [][]any
	seen    []brain.ChatRequest
}

func (m *scriptedModel) StreamReply(_ context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	m.seen = append(m.seen, request)
	reply := m.replies[0]
	m.replies = m.replies[1:]
	return func(yield func(brain.ReplyDelta, error) bool) {
		delta := brain.ReplyDelta{Stop: brain.StopCompleted}
		if reply != nil {
			items := make([]any, 0, len(reply))
			for _, text := range reply {
				items = append(items, map[string]any{"text": text})
			}
			delta.ToolCalls = []brain.ToolCall{{ID: "c1", Name: saveTool, Arguments: map[string]any{"items": items}}}
		}
		yield(delta, nil)
	}
}

// 代理用 save_backstory 交回清单；太长时再请它压一次；没调用工具算失败（不从文字里找）。
func TestAgentMerger(t *testing.T) {
	model := &scriptedModel{replies: [][]any{{"喜欢咖啡"}}}
	merger := AgentMerger{Agent: brain.Agent{Model: model, Instructions: "整理"}}
	entries, err := merger.Merge(context.Background(), []Entry{{Text: "主人改过的"}}, nil, nil)
	if err != nil || len(entries) != 1 || entries[0].Text != "喜欢咖啡" {
		t.Fatalf("交回：%+v %v", entries, err)
	}
	if !strings.Contains(model.seen[0].Messages[1].Text, "主人改过的") || len(model.seen[0].Tools) != 1 {
		t.Errorf("材料里带着主人改过的、给了工具：%+v", model.seen[0])
	}
	long := strings.Repeat("长", MaxRunes+1)
	model = &scriptedModel{replies: [][]any{{long}, {"压短了"}}}
	merger = AgentMerger{Agent: brain.Agent{Model: model}}
	if entries, err := merger.Merge(context.Background(), nil, nil, nil); err != nil || entries[0].Text != "压短了" || len(model.seen) != 2 {
		t.Errorf("太长时再压一次：%+v %v", entries, err)
	}
	model = &scriptedModel{replies: [][]any{nil}}
	if _, err := (AgentMerger{Agent: brain.Agent{Model: model}}).Merge(context.Background(), nil, nil, nil); err == nil {
		t.Error("没调用工具应算失败")
	}
}

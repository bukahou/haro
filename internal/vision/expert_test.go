package vision

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

// scriptedModel 按请求次序给出回答；replies 用完后（或没给工具时）回答 final。
type scriptedModel struct {
	mu       sync.Mutex
	replies  []brain.ReplyDelta
	final    string
	err      error
	requests [][]brain.ChatMessage
	specs    [][]brain.ToolSpec
}

func (m *scriptedModel) StreamReply(_ context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.mu.Lock()
	m.requests = append(m.requests, append([]brain.ChatMessage(nil), messages...))
	m.specs = append(m.specs, tools)
	var reply brain.ReplyDelta
	if len(tools) > 0 && len(m.replies) > 0 {
		reply, m.replies = m.replies[0], m.replies[1:]
	} else {
		reply = brain.ReplyDelta{Text: m.final}
	}
	reply.Usage = &brain.ReplyUsage{PromptTokens: 100, OutputTokens: 10}
	err := m.err
	m.mu.Unlock()
	return func(yield func(brain.ReplyDelta, error) bool) {
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		yield(reply, nil)
	}
}
func (m *scriptedModel) Warmup(context.Context) error { return nil }

func searchCall(query string) brain.ReplyDelta {
	return brain.ReplyDelta{Text: "我查一下。", ToolCalls: []brain.ToolCall{{Name: searchTool, Arguments: map[string]any{"query": query}}}}
}

type recordedSearch struct{ queries []string }

func (r *recordedSearch) search(_ context.Context, query string) (string, error) {
	r.queries = append(r.queries, query)
	return "搜到：Groovy M.2 SSD 外付けケース", nil
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestExpertAnswersFromFrame(t *testing.T) {
	model := &scriptedModel{final: "蓝色盒子，印着 M.2，看着像硬盘盒的包装。"}
	searches := &recordedSearch{}
	expert := NewExpert(model, "strong", searches.search, discard)
	report, err := expert.ExamineFrame(context.Background(), []byte("jpeg"), "这是什么？")
	if err != nil || report != "蓝色盒子，印着 M.2，看着像硬盘盒的包装。" {
		t.Fatalf("应直接给出判断：%q %v", report, err)
	}
	first := model.requests[0]
	if first[0].Role != brain.RoleSystem || !strings.Contains(first[1].Text, "这是什么？") || string(first[2].Image) != "jpeg" {
		t.Errorf("专家应拿到提示词、主人的原话和画面：%+v", first)
	}
	if len(model.specs[0]) != 1 || model.specs[0][0].Name != searchTool || len(searches.queries) != 0 {
		t.Errorf("能联网时应给搜索工具，但没要求就不搜：%+v %v", model.specs[0], searches.queries)
	}
}

func TestExpertSearchesThenAnswers(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{searchCall("蓝色 包装盒 M.2 Groovy")}, final: "是 Groovy 的 M.2 硬盘盒。"}
	searches := &recordedSearch{}
	report, err := NewExpert(model, "strong", searches.search, discard).ExamineFrame(context.Background(), []byte("jpeg"), "这是什么？")
	if err != nil || report != "是 Groovy 的 M.2 硬盘盒。" {
		t.Fatalf("搜完应给出判断：%q %v", report, err)
	}
	if len(searches.queries) != 1 || searches.queries[0] != "蓝色 包装盒 M.2 Groovy" {
		t.Errorf("应按专家给的词搜一次：%v", searches.queries)
	}
	second := model.requests[1]
	last := second[len(second)-1]
	if last.Role != brain.RoleTool || !strings.Contains(last.Text, "Groovy M.2") || string(second[2].Image) != "jpeg" {
		t.Errorf("第二次请求应带着画面和搜索结果：%+v", second)
	}
}

func TestExpertSearchLimit(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{searchCall("一"), searchCall("二"), searchCall("三")}, final: "看着像硬盘盒。"}
	searches := &recordedSearch{}
	report, err := NewExpert(model, "strong", searches.search, discard).ExamineFrame(context.Background(), []byte("jpeg"), "这是什么？")
	if err != nil || report != "看着像硬盘盒。" {
		t.Fatalf("搜到上限后应直接回答：%q %v", report, err)
	}
	if len(searches.queries) != maxSearches || len(model.specs) != maxSearches+1 || model.specs[maxSearches] != nil {
		t.Errorf("最多搜 %d 次，之后不再给工具：搜了 %v，各次工具 %v", maxSearches, searches.queries, model.specs)
	}
}

func TestExpertWithoutSearch(t *testing.T) {
	model := &scriptedModel{final: "白色的小盒子。"}
	if _, err := NewExpert(model, "strong", nil, discard).ExamineFrame(context.Background(), []byte("jpeg"), "这是什么？"); err != nil {
		t.Fatal(err)
	}
	if model.specs[0] != nil {
		t.Errorf("不能联网时不应给搜索工具：%+v", model.specs[0])
	}
}

func TestExpertErrors(t *testing.T) {
	failing := &scriptedModel{err: errors.New("503")}
	if _, err := NewExpert(failing, "strong", nil, discard).ExamineFrame(context.Background(), []byte("jpeg"), "这是什么？"); err == nil {
		t.Error("模型出错时应返回错误（由前台退回直接看画面）")
	}
	silent := &scriptedModel{final: "  "}
	if _, err := NewExpert(silent, "strong", nil, discard).ExamineFrame(context.Background(), []byte("jpeg"), "这是什么？"); err == nil {
		t.Error("专家没说出判断时应返回错误")
	}
}

// stubbornModel 不管给没给工具都要求再搜（防专家陷入循环）。
type stubbornModel struct{ requests int }

func (m *stubbornModel) StreamReply(context.Context, []brain.ChatMessage, []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.requests++
	requests := m.requests
	return func(yield func(brain.ReplyDelta, error) bool) {
		if requests > 10 {
			yield(brain.ReplyDelta{}, errors.New("请求太多次：专家没有停下来"))
			return
		}
		yield(brain.ReplyDelta{Text: "看着像硬盘盒。", ToolCalls: []brain.ToolCall{{Name: searchTool, Arguments: map[string]any{"query": "再搜"}}}}, nil)
	}
}
func (m *stubbornModel) Warmup(context.Context) error { return nil }

func TestExpertStopsWhenToolsAreGone(t *testing.T) {
	model, searches := &stubbornModel{}, &recordedSearch{}
	report, err := NewExpert(model, "strong", searches.search, discard).ExamineFrame(context.Background(), []byte("jpeg"), "这是什么？")
	if err != nil || report != "看着像硬盘盒。" || model.requests != maxSearches+1 {
		t.Fatalf("不给工具的那次请求之后应停下、用它的文字：%q %v，请求了 %d 次", report, err, model.requests)
	}
}

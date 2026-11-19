package vision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"slices"
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

var question = brain.FrameQuestion{Frame: []byte("jpeg"), Request: "这是什么？"}

func reportCall(spoken, details string) brain.ReplyDelta {
	return brain.ReplyDelta{ToolCalls: []brain.ToolCall{{Name: reportTool,
		Arguments: map[string]any{"spoken": spoken, "details": details}}}}
}

func toolNames(specs []brain.ToolSpec) []string {
	var names []string
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// 看完经 report_frame 分两段交结论：说给主人听的一句、细节。
func TestExpertReportsSpokenAndDetails(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{reportCall("看着像硬盘盒的包装。", "蓝色盒子，印着 M.2。")}}
	searches := &recordedSearch{}
	report, err := NewExpert(model, "strong", searches.search, discard).ExamineFrame(context.Background(), question)
	if err != nil || report != (brain.FrameReport{Spoken: "看着像硬盘盒的包装。", Details: "蓝色盒子，印着 M.2。"}) {
		t.Fatalf("应交出两段结论：%+v %v", report, err)
	}
	first := model.requests[0]
	if first[0].Role != brain.RoleSystem || !strings.Contains(first[1].Text, "这是什么？") || string(first[2].Image) != "jpeg" {
		t.Errorf("专家应拿到提示词、主人的原话和画面：%+v", first)
	}
	if names := toolNames(model.specs[0]); !slices.Equal(names, []string{reportTool, searchTool}) || len(searches.queries) != 0 {
		t.Errorf("应给交结论和搜索两个工具，没要求就不搜：%v %v", names, searches.queries)
	}
}

func TestExpertSearchesThenReports(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{searchCall("蓝色 包装盒 M.2 Groovy"), reportCall("是 Groovy 的 M.2 硬盘盒。", "查到了型号。")}}
	searches := &recordedSearch{}
	report, err := NewExpert(model, "strong", searches.search, discard).ExamineFrame(context.Background(), question)
	if err != nil || report.Spoken != "是 Groovy 的 M.2 硬盘盒。" {
		t.Fatalf("搜完应交出结论：%+v %v", report, err)
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

// 专家直接用文字回答、没交结论：提醒一次，它再交。
func TestExpertRemindsOnceToReport(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{{Text: "看着像硬盘盒。"}, reportCall("看着像硬盘盒。", "蓝色盒子。")}}
	report, err := NewExpert(model, "strong", nil, discard).ExamineFrame(context.Background(), question)
	if err != nil || report.Spoken != "看着像硬盘盒。" {
		t.Fatalf("提醒后应交出结论：%+v %v", report, err)
	}
	second := model.requests[1]
	if last := second[len(second)-1]; last.Role != brain.RoleUser || last.Text != reportReminder {
		t.Errorf("第二次请求应带着提醒：%+v", last)
	}
}

// 提醒后仍只用文字回答：把它说的话当细节交给前台（前台自己压短），不报错。
func TestExpertFallsBackToDetails(t *testing.T) {
	model := &scriptedModel{final: "蓝色盒子，印着 M.2，看着像硬盘盒的包装。"}
	report, err := NewExpert(model, "strong", nil, discard).ExamineFrame(context.Background(), question)
	if err != nil || report != (brain.FrameReport{Details: "蓝色盒子，印着 M.2，看着像硬盘盒的包装。"}) || len(model.requests) != 2 {
		t.Fatalf("应提醒一次后用它的话当细节：%+v %v，请求了 %d 次", report, err, len(model.requests))
	}
}

func TestExpertSearchLimit(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{searchCall("一"), searchCall("二"), searchCall("三")}, final: "看着像硬盘盒。"}
	searches := &recordedSearch{}
	report, err := NewExpert(model, "strong", searches.search, discard).ExamineFrame(context.Background(), question)
	if err != nil || report.Details != "看着像硬盘盒。" {
		t.Fatalf("搜到上限后应停下：%+v %v", report, err)
	}
	if len(searches.queries) != maxSearches || len(model.requests) != maxRounds {
		t.Errorf("最多搜 %d 次、最多请求 %d 次：搜了 %v，请求了 %d 次", maxSearches, maxRounds, searches.queries, len(model.requests))
	}
	for _, specs := range model.specs[maxSearches:] {
		if slices.Contains(toolNames(specs), searchTool) {
			t.Errorf("搜到上限后不应再给搜索工具：%v", toolNames(specs))
		}
	}
}

func TestExpertWithoutSearch(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{reportCall("白色的小盒子。", "")}}
	if _, err := NewExpert(model, "strong", nil, discard).ExamineFrame(context.Background(), question); err != nil {
		t.Fatal(err)
	}
	if names := toolNames(model.specs[0]); !slices.Equal(names, []string{reportTool}) {
		t.Errorf("不能联网时只给交结论的工具：%v", names)
	}
}

func TestExpertErrors(t *testing.T) {
	failing := &scriptedModel{err: errors.New("503")}
	if _, err := NewExpert(failing, "strong", nil, discard).ExamineFrame(context.Background(), question); err == nil {
		t.Error("模型出错时应返回错误（由前台退回直接看画面）")
	}
	silent := &scriptedModel{final: "  "}
	if _, err := NewExpert(silent, "strong", nil, discard).ExamineFrame(context.Background(), question); err == nil {
		t.Error("专家没说出判断时应返回错误")
	}
}

// 给了性格时，专家拿到性格说明（spoken 用这个口吻）；没给时不多这一条。
func TestExpertGetsStyle(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{reportCall("看着像充电头哦。", "")}}
	styled := question
	styled.Style = "你现在的性格是「温柔陪伴」。"
	if _, err := NewExpert(model, "strong", nil, discard).ExamineFrame(context.Background(), styled); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(model.requests[0], func(msg brain.ChatMessage) bool { return strings.Contains(msg.Text, styled.Style) }) {
		t.Errorf("专家应拿到性格说明：%+v", model.requests[0])
	}
	plain := &scriptedModel{replies: []brain.ReplyDelta{reportCall("看着像充电头。", "")}}
	if _, err := NewExpert(plain, "strong", nil, discard).ExamineFrame(context.Background(), question); err != nil {
		t.Fatal(err)
	}
	if len(plain.requests[0]) != len(model.requests[0])-1 {
		t.Errorf("没给性格时不应多一条：%d 条 / %d 条", len(plain.requests[0]), len(model.requests[0]))
	}
}

// spoken 超过字数上限：请它压短一次，用压短后的。
func TestExpertShortensLongSpoken(t *testing.T) {
	long := strings.Repeat("长", brain.CameraAnswerChars+1)
	model := &scriptedModel{replies: []brain.ReplyDelta{reportCall(long, "细节。"), reportCall("短的。", "细节。")}}
	report, err := NewExpert(model, "strong", nil, discard).ExamineFrame(context.Background(), question)
	if err != nil || report.Spoken != "短的。" || len(model.requests) != 2 {
		t.Fatalf("应压短一次后用短的：%+v %v，请求了 %d 次", report, err, len(model.requests))
	}
	second := model.requests[1]
	if last := second[len(second)-1]; last.Role != brain.RoleTool || last.Text != shortenRequest(brain.CameraAnswerChars+1) {
		t.Errorf("第二次请求应带着压短的要求：%+v", last)
	}
}

// 压短一次后还超：照用，不再请求、不截断；压短时没再交结论：用压短前的。
func TestExpertKeepsLongSpokenAfterOneTry(t *testing.T) {
	long := strings.Repeat("长", brain.CameraAnswerChars+1)
	longer := strings.Repeat("长", brain.CameraAnswerChars+5)
	model := &scriptedModel{replies: []brain.ReplyDelta{reportCall(long, ""), reportCall(longer, "")}}
	report, err := NewExpert(model, "strong", nil, discard).ExamineFrame(context.Background(), question)
	if err != nil || report.Spoken != longer || len(model.requests) != 2 {
		t.Fatalf("压短一次后应照用：%+v %v，请求了 %d 次", report, err, len(model.requests))
	}
	silent := &scriptedModel{replies: []brain.ReplyDelta{reportCall(long, "细节。")}, final: "好的。"}
	report, err = NewExpert(silent, "strong", nil, discard).ExamineFrame(context.Background(), question)
	if err != nil || report.Spoken != long {
		t.Fatalf("压短时没再交结论，应用压短前的：%+v %v", report, err)
	}
}

// 工具的参数定义由结构体生成；说给主人听那句的字数上限引用大脑里的同一个常量。
func TestReportToolSchema(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(reportSpec.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	limit := fmt.Sprintf("不超过 %d 个字", brain.CameraAnswerChars)
	if !strings.Contains(schema.Properties["spoken"].Description, limit) || schema.Properties["details"].Description == "" {
		t.Errorf("spoken 的说明应写明「%s」，details 应有说明：%+v", limit, schema.Properties)
	}
	if !slices.Contains(schema.Required, "spoken") || !slices.Contains(schema.Required, "details") {
		t.Errorf("两段都必填：%v", schema.Required)
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

func TestExpertStopsAfterMaxRounds(t *testing.T) {
	model, searches := &stubbornModel{}, &recordedSearch{}
	report, err := NewExpert(model, "strong", searches.search, discard).ExamineFrame(context.Background(), question)
	if err != nil || report.Details != "看着像硬盘盒。" || model.requests != maxRounds {
		t.Fatalf("到轮数上限应停下、用它的文字当细节：%+v %v，请求了 %d 次", report, err, model.requests)
	}
}

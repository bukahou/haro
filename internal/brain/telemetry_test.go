package brain_test

import (
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
)

// 全局的追踪只能换一次：这个包的测试共用一个能读出来的。
var (
	spanRecorderOnce sync.Once
	recordedSpans    = tracetest.NewSpanRecorder()
)

func endedSpansSince(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	spanRecorderOnce.Do(func() { otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recordedSpans))) })
	before := len(recordedSpans.Ended())
	return func() []sdktrace.ReadOnlySpan { return recordedSpans.Ended()[before:] }
}

func attributesOf(span sdktrace.ReadOnlySpan) map[attribute.Key]string {
	attrs := map[attribute.Key]string{}
	for _, kv := range span.Attributes() {
		attrs[kv.Key] = kv.Value.Emit()
	}
	return attrs
}

// 一轮回答记成 invoke_agent（代理名是最后回答的那个，交接记成事件）；工具调用记成 execute_tool（工具名、调用编号）。
func TestTurnAndToolSpansUseGenAIAttributes(t *testing.T) {
	ended := endedSpansSince(t)
	front := braintest.NewModel(t, searchFor("耳机"), askStronger)
	strong := braintest.NewModel(t, braintest.Say("是耳机盒。"))
	parts := brain.Components{Front: brain.Agent{Model: front}, Synthesizer: fakeSynthesizer{}, Tools: &fakeToolbox{},
		Handoff: brain.Handoff{Agent: strongAgent(strong, 8), OnCorrection: true}}
	k := newTalker(t, parts, brain.Options{MaxHistoryTurn: 6})
	k.say("不对，这是什么")

	var turn, tool sdktrace.ReadOnlySpan
	for _, span := range ended() {
		switch span.Name() {
		case "brain.llm.stream":
			turn = span
		case "brain.tool":
			tool = span
		}
	}
	if turn == nil || tool == nil {
		t.Fatalf("应有一轮回答和一次工具调用的 span")
	}
	if attrs := attributesOf(turn); attrs["gen_ai.operation.name"] != "invoke_agent" || attrs["gen_ai.agent.name"] != "强模型" {
		t.Errorf("一轮回答：%v", attrs)
	}
	handoff := false
	for _, event := range turn.Events() {
		handoff = handoff || event.Name == "handoff"
	}
	if !handoff {
		t.Error("交给接手的代理应记成事件")
	}
	if attrs := attributesOf(tool); attrs["gen_ai.operation.name"] != "execute_tool" || attrs["gen_ai.tool.name"] != "search_notes" ||
		attrs["gen_ai.tool.call.id"] == "" {
		t.Errorf("工具调用：%v", attrs)
	}
}

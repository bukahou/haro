package llm

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/bukahou/haro/internal/brain"
)

// 全局的追踪、指标只能换一次（之前拿到的 tracer 跟着第一次换上的走）：整个包的测试共用一套能读出来的。
var (
	recorderOnce sync.Once
	testSpans    = tracetest.NewSpanRecorder()
	testReader   = sdkmetric.NewManualReader()
)

// telemetryRecorders 返回能读出来的追踪、指标，以及这个测试开始前已经结束的 span 数（只看之后的）。
func telemetryRecorders(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader, int) {
	t.Helper()
	recorderOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(testSpans)))
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	})
	return testSpans, testReader, len(testSpans.Ended())
}

func spanAttributes(span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range span.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	return attrs
}

// 每次请求一个 span「chat <模型>」，按 gen_ai.* 记提供方、模型、用量（含缓存读写）、为什么停；用量和耗时也进标准指标。
func TestRequestTelemetryFollowsGenAIConventions(t *testing.T) {
	spans, reader, before := telemetryRecorders(t)
	inner := &scriptedModel{deltas: []brain.ReplyDelta{{Text: "好的"},
		{Usage: &brain.ReplyUsage{PromptTokens: 1200, OutputTokens: 30, CacheReadTokens: 1000, CacheWriteTokens: 100, Requests: 1},
			Stop: brain.StopCompleted}}}
	model := WithRequestTelemetry(inner, ProviderAnthropic, "claude-haiku-5-5")
	if _, _, _, err := collect(t, model.StreamReply(context.Background(), brain.ChatRequest{})); err != nil {
		t.Fatal(err)
	}
	ended := spans.Ended()[before:]
	if len(ended) != 1 || ended[0].Name() != "chat claude-haiku-5-5" {
		t.Fatalf("应有一个 span「chat <模型>」：%d 个", len(ended))
	}
	attrs := spanAttributes(ended[0])
	want := map[attribute.Key]string{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "anthropic",
		"gen_ai.request.model": "claude-haiku-5-5", "gen_ai.usage.input_tokens": "1200", "gen_ai.usage.output_tokens": "30",
		"gen_ai.usage.cache_read.input_tokens": "1000", "gen_ai.usage.cache_creation.input_tokens": "100",
		"gen_ai.response.finish_reasons": `["stop"]`}
	for key, value := range want {
		if got := attrs[key].Emit(); got != value {
			t.Errorf("%s = %q，应为 %q", key, got, value)
		}
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			names[m.Name] = true
		}
	}
	if !names["gen_ai.client.operation.duration"] || !names["gen_ai.client.token.usage"] {
		t.Errorf("应记标准指标：%v", names)
	}
}

// 用量只写知道的：服务端不报用量时不写 token 数；没正常结束时只写输入（输出不知道）——出错那一段带的用量照样记。
func TestRequestTelemetryRecordsOnlyKnownUsage(t *testing.T) {
	spans, _, before := telemetryRecorders(t)
	unreported := WithRequestTelemetry(&scriptedModel{deltas: []brain.ReplyDelta{{Text: "好的"},
		{Usage: &brain.ReplyUsage{TokensUnknown: true}, Stop: brain.StopCompleted}}}, ProviderOpenAICompatible, "m")
	collect(t, unreported.StreamReply(context.Background(), brain.ChatRequest{}))
	cut := WithRequestTelemetry(&cutModel{usage: &brain.ReplyUsage{PromptTokens: 900, Incomplete: true}}, ProviderAnthropic, "m")
	collect(t, cut.StreamReply(context.Background(), brain.ChatRequest{}))

	ended := spans.Ended()[before:]
	if len(ended) != 2 {
		t.Fatalf("应有 2 个 span：%d", len(ended))
	}
	if attrs := spanAttributes(ended[0]); attrs["gen_ai.usage.input_tokens"].Type() != attribute.INVALID ||
		attrs["gen_ai.usage.output_tokens"].Type() != attribute.INVALID {
		t.Errorf("服务端不报用量时不该写 token 数：%v", attrs)
	}
	if attrs := spanAttributes(ended[1]); attrs["gen_ai.usage.input_tokens"].Emit() != "900" ||
		attrs["gen_ai.usage.output_tokens"].Type() != attribute.INVALID {
		t.Errorf("没正常结束时只写输入：%v", attrs)
	}
}

// cutModel 说到一半出错，出错那一段带着知道的用量（像 Claude 流中途断开时那样）。
type cutModel struct{ usage *brain.ReplyUsage }

func (m *cutModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		if yield(brain.ReplyDelta{Text: "从前"}, nil) {
			yield(brain.ReplyDelta{Usage: m.usage}, fmt.Errorf("%w: 断了", brain.ErrModelUnavailable))
		}
	}
}

// 被截断记 length；连不上记出错（error.type = unavailable）；调用方自己取消（主人打断）不算出错；
// 最后一段没说为什么停（StopUnknown）时不记为什么停（不编成 stop）。
func TestRequestTelemetryRecordsStopsAndErrors(t *testing.T) {
	spans, _, before := telemetryRecorders(t)
	truncated := WithRequestTelemetry(&scriptedModel{deltas: []brain.ReplyDelta{{Text: "说到一半", Stop: brain.StopMaxTokens}}},
		ProviderOllama, "local")
	collect(t, truncated.StreamReply(context.Background(), brain.ChatRequest{}))
	down := WithRequestTelemetry(&scriptedModel{err: fmt.Errorf("%w: 连不上", brain.ErrModelUnavailable)}, ProviderAnthropic, "m")
	collect(t, down.StreamReply(context.Background(), brain.ChatRequest{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := WithRequestTelemetry(&scriptedModel{err: context.Canceled}, ProviderAnthropic, "m")
	collect(t, cancelled.StreamReply(ctx, brain.ChatRequest{}))
	unknown := WithRequestTelemetry(&scriptedModel{deltas: []brain.ReplyDelta{{Text: "说完了吗"}}}, ProviderOllama, "local")
	collect(t, unknown.StreamReply(context.Background(), brain.ChatRequest{}))

	ended := spans.Ended()[before:]
	if len(ended) != 4 {
		t.Fatalf("应有 4 个 span：%d", len(ended))
	}
	if got := spanAttributes(ended[0])["gen_ai.response.finish_reasons"].Emit(); got != `["length"]` {
		t.Errorf("被截断应记 length：%q", got)
	}
	if ended[1].Status().Code != codes.Error || spanAttributes(ended[1])["error.type"].Emit() != "unavailable" {
		t.Errorf("连不上应记出错：%+v %v", ended[1].Status(), spanAttributes(ended[1])["error.type"])
	}
	if ended[2].Status().Code == codes.Error {
		t.Error("主人打断（调用方取消）不算出错")
	}
	if reasons, set := spanAttributes(ended[3])["gen_ai.response.finish_reasons"]; set {
		t.Errorf("没说为什么停时不该记为什么停：%q", reasons.Emit())
	}
}

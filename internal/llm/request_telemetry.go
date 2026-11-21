package llm

import (
	"context"
	"errors"
	"iter"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/bukahou/haro/internal/brain"
)

// 每次请求模型的观测，按 OpenTelemetry 的大模型语义约定（gen_ai.*）：一个 span「chat <模型>」，记提供方、模型、
// 用量（含缓存读写）、为什么停、出错的类别；指标 gen_ai.client.operation.duration、gen_ai.client.token.usage。
// 各适配层不各写一份：组装处给每个具体模型套上（WithRequestTelemetry）。

// 提供方的名字（gen_ai.provider.name）：语义约定里有的用它的值；OpenAI 兼容接口背后是哪家不知道，不编成 openai。
var (
	ProviderAnthropic        = string(genaiconv.ProviderNameAnthropic)
	ProviderOllama           = "ollama"
	ProviderOpenAICompatible = "openai_compatible"
)

var (
	tracer = otel.Tracer("github.com/bukahou/haro/llm")
	meter  = otel.Meter("github.com/bukahou/haro/llm")

	operationDuration = mustInstrument(genaiconv.NewClientOperationDuration(meter))
	tokenUsage        = mustInstrument(genaiconv.NewClientTokenUsage(meter))
)

func mustInstrument[T any](instrument T, err error) T {
	if err != nil {
		panic(err)
	}
	return instrument
}

// 为什么停（gen_ai.response.finish_reasons）：用各家通用的写法。
const (
	finishStop      = "stop"
	finishLength    = "length"
	finishToolCalls = "tool_calls"
)

// requestTelemetry 是套上观测的模型。
type requestTelemetry struct {
	inner    brain.ChatModel
	provider genaiconv.ProviderNameAttr
	model    string
}

var (
	_ brain.ChatModel   = (*requestTelemetry)(nil)
	_ brain.ModelWarmer = (*requestTelemetry)(nil)
)

// WithRequestTelemetry 给 inner 套上每次请求的观测；provider 用上面的 Provider* 之一，model 是模型 ID。
func WithRequestTelemetry(inner brain.ChatModel, provider, model string) brain.ChatModel {
	return &requestTelemetry{inner: inner, provider: genaiconv.ProviderNameAttr(provider), model: model}
}

// StreamReply 原样转发，结束时记下这次请求。调用方取消（主人打断）不算出错。
func (t *requestTelemetry) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		ctx, span := tracer.Start(ctx, string(genaiconv.OperationNameChat)+" "+t.model, trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(semconv.GenAIOperationNameChat, semconv.GenAIProviderNameKey.String(string(t.provider)),
				semconv.GenAIRequestModel(t.model)))
		defer span.End()
		started := time.Now()
		var usage *brain.ReplyUsage
		var failure error
		stop, toolCalls := brain.StopUnknown, false
		defer func() { t.record(ctx, span, started, usage, finishReason(stop, toolCalls), failure) }()
		for delta, err := range t.inner.StreamReply(ctx, request) {
			switch {
			case err != nil:
				failure = err
			case delta.Usage != nil:
				usage = delta.Usage
			}
			toolCalls = toolCalls || len(delta.ToolCalls) > 0
			stop = delta.Stop
			if !yield(delta, err) || err != nil {
				return
			}
		}
	}
}

// finishReason 是这次请求为什么停（gen_ai.response.finish_reasons 的写法）；不知道时为空，不记。
func finishReason(stop brain.StopReason, toolCalls bool) string {
	switch {
	case stop == brain.StopMaxTokens:
		return finishLength
	case stop == brain.StopCompleted && toolCalls:
		return finishToolCalls
	case stop == brain.StopCompleted:
		return finishStop
	}
	return ""
}

// record 把这次请求记进 span 和指标。
func (t *requestTelemetry) record(ctx context.Context, span trace.Span, started time.Time, usage *brain.ReplyUsage,
	finish string, failure error) {
	attrs := []attribute.KeyValue{operationDuration.AttrRequestModel(t.model)}
	switch {
	case failure != nil && ctx.Err() == nil:
		errorType := string(genaiconv.ErrorTypeOther)
		if errors.Is(failure, brain.ErrModelUnavailable) {
			errorType = "unavailable"
		}
		span.RecordError(failure)
		span.SetStatus(codes.Error, failure.Error())
		span.SetAttributes(semconv.ErrorTypeKey.String(errorType))
		attrs = append(attrs, operationDuration.AttrErrorType(genaiconv.ErrorTypeAttr(errorType)))
	case failure == nil && finish != "":
		span.SetAttributes(semconv.GenAIResponseFinishReasons(finish))
	}
	if usage != nil {
		span.SetAttributes(semconv.GenAIUsageInputTokens(usage.PromptTokens), semconv.GenAIUsageOutputTokens(usage.OutputTokens),
			semconv.GenAIUsageCacheReadInputTokens(usage.CacheReadTokens),
			semconv.GenAIUsageCacheCreationInputTokens(usage.CacheWriteTokens))
		model := tokenUsage.AttrRequestModel(t.model)
		tokenUsage.Record(ctx, int64(usage.PromptTokens), genaiconv.OperationNameChat, t.provider, genaiconv.TokenTypeInput, model)
		tokenUsage.Record(ctx, int64(usage.OutputTokens), genaiconv.OperationNameChat, t.provider, genaiconv.TokenTypeOutput, model)
	}
	operationDuration.Record(ctx, time.Since(started).Seconds(), genaiconv.OperationNameChat, t.provider, attrs...)
}

// Warmup 转发给里面的模型（它需要预热的话）。
func (t *requestTelemetry) Warmup(ctx context.Context) error { return brain.WarmUp(ctx, t.inner) }

package brain

import (
	"go.opentelemetry.io/otel"

	"github.com/bukahou/haro/internal/telemetry"
)

var (
	tracer = otel.Tracer("github.com/bukahou/haro/brain")
	meter  = otel.Meter("github.com/bukahou/haro/brain")

	// timeToFirstToken：请求模型 → 第一个可见 token。
	timeToFirstToken = telemetry.MustLatencyHistogram(meter, "haro.llm.time_to_first_token",
		"请求模型到第一个可见 token 的时间")
	// timeToFirstSentence：请求模型 → 切出第一句（决定第一个字什么时候能合成）。
	timeToFirstSentence = telemetry.MustLatencyHistogram(meter, "haro.llm.time_to_first_sentence",
		"请求模型到切出第一句的时间")
)

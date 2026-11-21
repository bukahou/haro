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
	// droppedEvents：发给终端的事件因为积压太多被丢掉（只有可以丢的那类：从别的 goroutine 发来的提示）。
	droppedEvents = telemetry.MustCounter(meter, "haro.events.dropped", "发给终端的事件因为积压太多被丢掉的个数")
)

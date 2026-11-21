// Package telemetry 初始化 OpenTelemetry 追踪与指标。
// 配了 OTLP 地址就导出（例如接到 OTel Collector）；没配就只用日志里的耗时。
package telemetry

import (
	"context"
	"errors"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// SetupTelemetry 返回关闭函数；baseURL 为空时什么都不做。
// baseURL 是 OTLP/HTTP 的根地址（例如 http://collector:4318），与标准变量 OTEL_EXPORTER_OTLP_ENDPOINT 同义：
// 追踪发往 <baseURL>/v1/traces，指标发往 <baseURL>/v1/metrics。
func SetupTelemetry(ctx context.Context, baseURL string) (func(context.Context) error, error) {
	if baseURL == "" {
		return func(context.Context) error { return nil }, nil
	}
	baseURL = strings.TrimRight(baseURL, "/")
	serviceResource := resource.NewSchemaless(semconv.ServiceName("haro"))

	traceExporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(baseURL+"/v1/traces"))
	if err != nil {
		return nil, err
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter), sdktrace.WithResource(serviceResource))
	otel.SetTracerProvider(tracerProvider)

	metricExporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(baseURL+"/v1/metrics"))
	if err != nil {
		return nil, err
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)), sdkmetric.WithResource(serviceResource))
	otel.SetMeterProvider(meterProvider)

	return func(ctx context.Context) error {
		return errors.Join(tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
	}, nil
}

// latencyBucketsSeconds 是对话延迟类指标的分桶（秒）：覆盖 0.25–10 秒，在 1–3 秒这段最密。
var latencyBucketsSeconds = []float64{0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10}

// MustLatencyHistogram 创建一个单位为秒的延迟直方图。
// 在 SetupTelemetry 之前创建也可以：全局 MeterProvider 会在设置后自动接管。
// MustCounter 创建一个计数器（只在启动时调用：参数写错就是程序错了）。
func MustCounter(meter metric.Meter, name, description string) metric.Int64Counter {
	counter, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		panic(err)
	}
	return counter
}

func MustLatencyHistogram(meter metric.Meter, name, description string) metric.Float64Histogram {
	histogram, err := meter.Float64Histogram(name,
		metric.WithUnit("s"), metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(latencyBucketsSeconds...))
	if err != nil {
		panic(err)
	}
	return histogram
}

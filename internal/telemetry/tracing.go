// Package telemetry 初始化 OpenTelemetry 追踪与指标（OTLP/HTTP）。
//
// 往哪发：标准的环境变量 OTEL_EXPORTER_OTLP_ENDPOINT（或分信号的 OTEL_EXPORTER_OTLP_TRACES_ENDPOINT、
// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT）设了时以它为准，交给 SDK 自己读（同族的 HEADERS、TIMEOUT 等也照标准生效）；
// 都没设时用 HARO_OTLP_ENDPOINT（根地址，追踪发往 <根地址>/v1/traces、指标发往 <根地址>/v1/metrics）；都没有就不导出，只打日志。
// 报的资源：每个程序报自己的 service.name（ServiceServer、ServiceTerminal）和版本号，标准的 OTEL_SERVICE_NAME、
// OTEL_RESOURCE_ATTRIBUTES 设了时覆盖它们。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/endpoint"
)

// 各程序报的 service.name（终端和服务端的 span、指标分开；OTEL_SERVICE_NAME 设了时以它为准）。
const (
	ServiceServer   = "haro-server"
	ServiceTerminal = "haro-terminal"
)

// ShutdownTimeout 是退出时等遥测发出去最多多久：采集器连不上时不拖住退出（终端、服务端同一个值）。
const ShutdownTimeout = time.Second

// 标准的环境变量（OpenTelemetry 规范里的名字）。
const (
	envEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envProtocol        = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envTracesProtocol  = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	envMetricsProtocol = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
)

// httpProtobuf 是这里用的 OTLP 传输方式（OTEL_EXPORTER_OTLP_*PROTOCOL 只能写它或不写）。
const httpProtobuf = "http/protobuf"

// Setup 开启遥测：service 是这个程序报的 service.name（ServiceServer 等），fallbackEndpoint 是 HARO_OTLP_ENDPOINT。
// 返回的 shutdown 在 ShutdownTimeout 之内把还没发出的发出去、关掉导出器；不导出时什么都不做。
func Setup(ctx context.Context, service, fallbackEndpoint string) (shutdown func() error, err error) {
	return setup(ctx, service, fallbackEndpoint, os.Getenv, ShutdownTimeout)
}

func setup(ctx context.Context, service, fallbackEndpoint string, getenv func(string) string,
	shutdownTimeout time.Duration) (func() error, error) {
	traces, err := exportTarget(getenv, envTracesEndpoint, envTracesProtocol, "traces", fallbackEndpoint)
	if err != nil {
		return nil, err
	}
	metrics, err := exportTarget(getenv, envMetricsEndpoint, envMetricsProtocol, "metrics", fallbackEndpoint)
	if err != nil {
		return nil, err
	}
	if !traces.enabled && !metrics.enabled {
		return func() error { return nil }, nil
	}
	serviceResource, err := newResource(ctx, service)
	if err != nil {
		return nil, err
	}
	var shutdowns []func(context.Context) error
	if traces.enabled {
		var options []otlptracehttp.Option
		if traces.endpointURL != "" {
			options = append(options, otlptracehttp.WithEndpointURL(traces.endpointURL))
		}
		exporter, err := otlptracehttp.New(ctx, options...)
		if err != nil {
			return nil, fmt.Errorf("追踪的导出器：%w", err)
		}
		provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(serviceResource))
		otel.SetTracerProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	if metrics.enabled {
		var options []otlpmetrichttp.Option
		if metrics.endpointURL != "" {
			options = append(options, otlpmetrichttp.WithEndpointURL(metrics.endpointURL))
		}
		exporter, err := otlpmetrichttp.New(ctx, options...)
		if err != nil {
			return nil, fmt.Errorf("指标的导出器：%w", err)
		}
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
			sdkmetric.WithResource(serviceResource))
		otel.SetMeterProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	return func() error {
		flushCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		var problems []error
		for _, shutdown := range shutdowns {
			problems = append(problems, shutdown(flushCtx))
		}
		return errors.Join(problems...)
	}, nil
}

// target 是一种信号往哪发：enabled = 要导出；endpointURL 非空 = 用这个完整地址（来自 HARO_OTLP_ENDPOINT），
// 空 = 交给 SDK 按标准的环境变量自己读。
type target struct {
	enabled     bool
	endpointURL string
}

// exportTarget 决定一种信号（signal 是 traces / metrics）往哪发：分信号的标准变量、通用的标准变量、HARO_OTLP_ENDPOINT，
// 先设了的为准。传输方式写了 http/protobuf 以外的（例如 grpc）就报错：这里只有 HTTP 的导出器，不能悄悄换掉。
func exportTarget(getenv func(string) string, signalEndpointVar, signalProtocolVar, signal, fallback string) (target, error) {
	for _, name := range []string{signalProtocolVar, envProtocol} {
		if protocol := getenv(name); protocol != "" && protocol != httpProtobuf {
			return target{}, fmt.Errorf("%s=%q：只支持 %s（OTLP/HTTP）", name, protocol, httpProtobuf)
		}
	}
	if getenv(signalEndpointVar) != "" || getenv(envEndpoint) != "" {
		return target{enabled: true}, nil
	}
	if fallback == "" {
		return target{}, nil
	}
	endpointURL, err := endpoint.Join(fallback, "/v1/"+signal, nil)
	if err != nil {
		return target{}, fmt.Errorf("HARO_OTLP_ENDPOINT %q：%w", fallback, err)
	}
	return target{enabled: true, endpointURL: endpointURL}, nil
}

// newResource 是这个程序报的资源：service.name、service.version，再合并标准的 OTEL_SERVICE_NAME、OTEL_RESOURCE_ATTRIBUTES
// （后者覆盖前者；OTEL_RESOURCE_ATTRIBUTES 写错时报错，不悄悄丢掉）。
func newResource(ctx context.Context, service string) (*resource.Resource, error) {
	serviceResource, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceVersion(buildinfo.Version)),
		resource.WithFromEnv())
	if err != nil {
		return nil, fmt.Errorf("遥测的资源（OTEL_SERVICE_NAME、OTEL_RESOURCE_ATTRIBUTES）：%w", err)
	}
	return serviceResource, nil
}

// latencyBucketsSeconds 是对话延迟类指标的分桶（秒）：覆盖 0.25–10 秒，在 1–3 秒这段最密。
var latencyBucketsSeconds = []float64{0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10}

// MustCounter 创建一个计数器（只在启动时调用：参数写错就是程序错了）。
func MustCounter(meter metric.Meter, name, description string) metric.Int64Counter {
	counter, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		panic(err)
	}
	return counter
}

// MustGauge 创建一个记「现在是多少」的仪表（只在启动时调用）。unit 照 UCUM 写，例如 s。
func MustGauge(meter metric.Meter, name, description, unit string) metric.Float64Gauge {
	gauge, err := meter.Float64Gauge(name, metric.WithUnit(unit), metric.WithDescription(description))
	if err != nil {
		panic(err)
	}
	return gauge
}

// MustLatencyHistogram 创建一个单位为秒的延迟直方图。
// 在 Setup 之前创建也可以：全局 MeterProvider 会在设置后自动接管。
func MustLatencyHistogram(meter metric.Meter, name, description string) metric.Float64Histogram {
	histogram, err := meter.Float64Histogram(name,
		metric.WithUnit("s"), metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(latencyBucketsSeconds...))
	if err != nil {
		panic(err)
	}
	return histogram
}
